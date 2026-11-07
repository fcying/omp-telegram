package bridge

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

// Reserve one Telegram-sized terminal status before any immutable body is sent.
const assistantStatusReserveBytes = 4 * telegram.MaxMessageUTF16
const assistantHeaderUnits = 512
const assistantCompletedIcon = "✅"

// assistantDelivery retains positions and bounds, not completed body history.
// Only terminal-only snapshots may wait in stream for their status sample.
// messages counts every role in the current native agent run.
type assistantDelivery struct {
	messages       int
	bytes          int
	body           bool
	queuedBytes    int
	queuedParts    int
	truncated      bool
	staged         bool
	statusOutboxID int64
}

func (w *worker) hasAssistantBody() bool {
	return w.assistantDelivery.body || w.stream.Len() != 0
}

// Interrupted results retain the uncommitted body and a truthful trailing notice.
func (w *worker) remainingAssistantReplies(notice string) []string {
	parts, _ := w.boundedAssistantReplies(w.stream.String())
	if notice != "" {
		parts = append(parts, notice)
	}
	return parts
}

// Limits apply to the whole root (or research round), not each message. Capacity
// for the terminal status and one truncation notice cannot be reclaimed later.
func (w *worker) boundedAssistantReplies(text string) ([]string, bool) {
	return w.boundedAssistantRepliesWithHeader(text, "")
}

func (w *worker) boundedAssistantRepliesWithHeader(text, header string) ([]string, bool) {
	state := &w.assistantDelivery
	if text == "" || state.truncated {
		return nil, state.truncated
	}
	notice := w.tr(i18n.Msg.ReplyTruncated)
	byteBudget := max(0, maxFinalReplyBytes-assistantStatusReserveBytes-len(notice)-state.queuedBytes)
	if header != "" {
		byteBudget = max(0, byteBudget-len(header)-2)
	}
	partBudget := max(0, maxFinalReplyMessages-2-state.queuedParts)
	truncated := len(text) > byteBudget
	if truncated {
		text = truncateUTF8(text, byteBudget)
	}
	// Reserve header space while splitting the body, rather than splitting a
	// combined string: a fitting code block must not push the header out alone.
	limit := telegram.MaxMessageUTF16
	if header != "" || len(w.b.cfg.StatusBar.Fields("intermediate")) != 0 || len(w.b.cfg.StatusBar.Fields("final")) != 0 {
		limit -= assistantHeaderUnits + 2
	}
	parts, overflow := splitTaskTextLimit(text, partBudget, max(1, limit))
	truncated = truncated || overflow
	if len(parts) > partBudget {
		parts = parts[:partBudget]
	}
	var size int
	for _, part := range parts {
		size += len(part)
	}
	for size > byteBudget {
		last := len(parts) - 1
		size -= len(parts[last])
		parts = parts[:last]
		truncated = true
	}
	if len(parts) != 0 && header != "" {
		parts[0] = header + "\n\n" + parts[0]
	}
	if truncated {
		parts = append(parts, notice)
	}
	return parts, truncated
}

func assistantReplyHasBody(parts []string, truncated bool) bool {
	// Truncation appends one synthetic notice after any real body parts.
	return len(parts) != 0 && (!truncated || len(parts) > 1)
}

func (w *worker) acceptAssistantMessage(m message, enforceInputBudget, terminalOnly bool) bool {
	if !w.taskActive() {
		return true
	}
	if !w.flushStagedAssistant() {
		return false
	}
	if m.Role != "assistant" {
		return true
	}
	w.replyStatus.metrics.observe(m)
	w.recordAssistantTerminal(m)
	text := assistantText(m)
	if text == "" && (m.StopReason == "stop" || assistantTerminalState(m) != terminalDone) {
		// A terminal metadata-only snapshot must not erase its streamed text.
		text = w.stream.String()
	}
	if terminalOnly && m.StopReason == "stop" && assistantTerminalState(m) == terminalDone {
		return w.stageAssistantText(text, enforceInputBudget)
	}
	return w.queueAssistantText(text, enforceInputBudget)
}

func (w *worker) recordAssistantTerminal(m message) {
	w.lastAssistant = &terminalAssistant{
		StopReason:                 m.StopReason,
		ErrorMessage:               m.ErrorMessage,
		ErrorClassificationMessage: m.ErrorClassificationMessage,
		ErrorStatus:                m.ErrorStatus,
	}
}

// Only terminal snapshots lacking message_end may await their final status.
// Normal completed messages are already immutable in the durable outbox.
func (w *worker) stageAssistantText(text string, enforceInputBudget bool) bool {
	if enforceInputBudget && len(text) > maxAssistantOutputBytes-w.assistantDelivery.bytes {
		w.assistantBudgetExceeded()
		return false
	}
	if text != w.stream.String() {
		w.stream.Reset()
		w.stream.WriteString(text)
	}
	w.assistantDelivery.staged = text != ""
	w.assistantDelivery.body = w.assistantDelivery.body || text != ""
	return true
}

func (w *worker) flushStagedAssistant() bool {
	if !w.assistantDelivery.staged {
		return true
	}
	return w.queueAssistantText(w.stream.String(), false)
}

func assistantContinuationEvent(e rpcEvent) bool {
	switch e.Type {
	case "agent_start", "message_start", "message_update", "message_end",
		"tool_execution_start", "tool_execution_update", "tool_execution_end",
		"host_tool_call":
		return true
	case "extension_ui_request":
		switch e.Method {
		case "confirm", "select", "input", "editor":
			return true
		}
	}
	return false
}

func (w *worker) queueAssistantText(text string, enforceInputBudget bool) bool {
	return w.queueAssistantTextWithHeader(text, enforceInputBudget, w.intermediateAssistantHeader())
}

func (w *worker) queueAssistantTextWithHeader(text string, enforceInputBudget bool, header string) bool {
	defer w.beginConsumerPhase("store", "queue_assistant_text").end()
	if enforceInputBudget && len(text) > maxAssistantOutputBytes-w.assistantDelivery.bytes {
		w.assistantBudgetExceeded()
		return false
	}
	if text != "" {
		parts, truncated := w.boundedAssistantRepliesWithHeader(text, header)
		queuedBytes := 0
		for _, part := range parts {
			queuedBytes += len(part)
		}
		if len(parts) != 0 {
			var err error
			var firstID int64
			if assistantReplyHasBody(parts, truncated) {
				if header != "" {
					parts[0] = parts[0][len(header)+2:]
				}
				bodyParts := len(parts)
				if truncated {
					bodyParts--
				}
				w.assistantDelivery.statusOutboxID, err = w.b.db.AppendAssistantBodyReplies(w.ctx, w.active, w.key.chat, w.key.thread, parts, header, bodyParts)
				firstID = w.assistantDelivery.statusOutboxID
			} else {
				err = w.b.db.AppendInboxReplies(w.ctx, w.active, w.key.chat, w.key.thread, parts)
			}
			if err != nil {
				w.b.fail(err)
				w.cancel()
				return false
			}
			if firstID != 0 {
				if !w.retirePreview(w.previewReplyTo, firstID, len(parts)) {
					return false
				}
			} else {
				w.rotatePreview(w.previewReplyTo)
			}
		}
		w.assistantDelivery.queuedBytes += queuedBytes
		w.assistantDelivery.queuedParts += len(parts)
		w.assistantDelivery.truncated = truncated
		w.assistantDelivery.bytes += min(len(text), maxAssistantOutputBytes-w.assistantDelivery.bytes)
		w.assistantDelivery.body = true
	}
	// A completed snapshot or native yield owns the entire current message.
	w.stream.Reset()
	w.assistantDelivery.staged = false
	return true
}

func (w *worker) queueYieldedAssistantTail(e rpcEvent) bool {
	if w.stream.Len() == 0 || !w.taskActive() {
		return true
	}
	staged := w.assistantDelivery.staged
	header := w.unconfirmedAssistantHeader()
	if staged {
		header = w.intermediateAssistantHeader()
	}
	if !w.queueAssistantTextWithHeader(w.stream.String(), true, header) {
		return false
	}
	// With no snapshot, the streamed assistant is the next unseen position.
	// Compacted frames may still provide its exact all-role upper bound.
	if !staged {
		w.assistantDelivery.messages++
	}
	if e.MessageCount != nil && *e.MessageCount > w.assistantDelivery.messages {
		w.assistantDelivery.messages = *e.MessageCount
	}
	return true
}

func (w *worker) reconcileAssistantMessages(e rpcEvent) bool {
	if !w.taskActive() || w.research.root && !w.research.roundOpen {
		return true
	}
	// Native rpc-frame.ts removes only an unchanged leading message_end prefix.
	// messageCount is the original array length, including user and tool roles.
	// Thus this offset is in all-role positions, not assistant-message positions.
	offset := 0
	if e.MessageCount != nil && *e.MessageCount >= len(e.Messages) {
		offset = *e.MessageCount - len(e.Messages)
	}
	// A confirmed terminal snapshot is bounded for delivery, not reclassified as
	// a streaming resource failure. Nonterminal continuations retain the live cap.
	terminal := e.IsTerminal == nil || *e.IsTerminal
	enforceInputBudget := !w.research.root && !terminal
	for i, m := range e.Messages {
		position := offset + i
		if position < w.assistantDelivery.messages {
			// Only the current unsent stop can be corrected. Its position and
			// metrics were already counted; committed earlier bodies stay immutable.
			if position == w.assistantDelivery.messages-1 && m.Role == "assistant" {
				w.recordAssistantTerminal(m)
			}
			if w.assistantDelivery.staged && position == w.assistantDelivery.messages-1 && m.Role == "assistant" {
				text := assistantText(m)
				if text != "" {
					corrected := w.stageAssistantText(text, enforceInputBudget)
					if !corrected {
						return false
					}
				}
			}
			continue
		}
		// A final-only snapshot can identify its last unqueued body even when
		// the provider omitted a stop reason. Unknown message_end stays early.
		finalBody := i == len(e.Messages)-1 && m.Role == "assistant" &&
			terminal && assistantTerminalState(m) == terminalDone
		if finalBody && m.StopReason != "stop" {
			m.StopReason = "stop"
		}
		if !w.acceptAssistantMessage(m, enforceInputBudget, finalBody) {
			return false
		}
		w.assistantDelivery.messages = position + 1
	}
	return true
}

func assistantOutputText(output store.Output) string {
	if output.Header == "" {
		return output.Text
	}
	return output.Header + "\n\n" + output.Text
}

func (b *Bridge) deliverAssistantHeader(ctx context.Context) error {
	output, err := b.db.NextAssistantHeader()
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	err = b.tg.Edit(ctx, output.Chat, output.MessageID, assistantOutputText(output), finalActionKeyboard(output.FinalActions))
	if ctx.Err() != nil {
		return b.db.FinishFinalActionEdit(output.ID, output.FinalActions)
	}
	if err != nil {
		logTelegramFailure(b.telegramLog, slog.LevelWarn, "assistant_header_update_failed", "assistant status update failed", err,
			slog.Int64("outbox_id", output.ID), slog.Int64("chat_id", output.Chat))
	}
	return b.db.MarkAssistantHeaderDelivered(output.ID, output.Header, output.FinalActions)
}
