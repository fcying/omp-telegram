package bridge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/i18n"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

const finalActionPrefix = "fa:"

// finalActionDispatch is owned by the actor and follows only pre-execution work.
// The database reservation is durable; losing this receipt cannot make it replayable.
type finalActionDispatch struct {
	inboxID  int64
	held     bool
	finished bool
	accepted bool
}

func (w *worker) withFinalAction(action *finalActionDispatch) func() {
	previous := w.finalAction
	w.finalAction = action
	if action != nil {
		action.held = false
	}
	return func() {
		if action != nil && !action.held {
			for _, c := range w.confirms {
				if c.finalAction == action {
					action.held = true
					break
				}
			}
			if !action.held {
				w.releaseFinalAction(action)
			}
		}
		w.finalAction = previous
	}
}

func (w *worker) holdFinalAction() {
	if w.finalAction != nil {
		w.finalAction.held = true
	}
}

func (w *worker) consumeFinalAction() bool {
	action := w.finalAction
	if action == nil {
		return true
	}
	if action.finished {
		return action.accepted
	}
	if err := w.b.db.ConsumeFinalAction(w.ctx, action.inboxID); err != nil {
		// Never release after an uncertain persistence outcome.
		action.finished = true
		if errors.Is(err, sql.ErrNoRows) {
			w.say(w.tr(i18n.Msg.ActionUnavailable))
			return false
		}
		w.b.fail(err)
		w.cancel()
		return false
	}
	action.finished = true
	action.accepted = true
	return true
}

func (w *worker) releaseFinalAction(action *finalActionDispatch) {
	if action == nil || action.finished {
		return
	}
	if err := w.b.db.ReleaseFinalAction(w.ctx, action.inboxID); err != nil {
		w.b.fail(err)
		w.cancel()
	}
	action.finished = true
}

func finalActionsConfigKey(actions []config.FinalAction) string {
	data, _ := json.Marshal(actions)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func finalActionKeyboard(encoded string) *telegram.Keyboard {
	if encoded == "" {
		return nil
	}
	var actions []store.FinalAction
	if json.Unmarshal([]byte(encoded), &actions) != nil || len(actions) == 0 {
		return nil
	}
	rows := make([][]telegram.Button, 0, len(actions))
	row := make([]telegram.Button, 0, min(3, len(actions)))
	rowUnits := 0
	for i, action := range actions {
		units := utf16Length(action.Label)
		if len(row) != 0 && (len(row) == 3 || rowUnits+units > 24) {
			rows = append(rows, row)
			row = make([]telegram.Button, 0, min(3, len(actions)-i))
			rowUnits = 0
		}
		row = append(row, telegram.Button{Text: action.Label, CallbackData: finalActionPrefix + action.Token})
		rowUnits += units
	}
	rows = append(rows, row)
	return &telegram.Keyboard{InlineKeyboard: rows}
}

// completeAssistantResult keeps header and body-tail identities independent.
func (w *worker) completeAssistantResult(taskID, headerTarget int64, header string, parts []string, truncated, hasBody bool) (int64, error) {
	defer w.beginConsumerPhase("store", "complete_assistant_result").end()
	bodyParts := 0
	if hasBody {
		bodyParts = len(parts)
		if truncated {
			bodyParts--
		}
	}
	set := &store.FinalActionSet{BodyParts: bodyParts}
	if len(w.b.cfg.FinalActions) != 0 && w.activeInputKind != queuedNativeCommand && w.owner > 0 && w.binding.Session != "" {
		set.Binding, set.User = w.binding, w.owner
		set.Config = finalActionsConfigKey(w.b.cfg.FinalActions)
		set.Actions = make([]store.FinalAction, 0, len(w.b.cfg.FinalActions))
		for _, action := range w.b.cfg.FinalActions {
			set.Actions = append(set.Actions, store.FinalAction{Token: rand.Text(), Label: action.Label, Command: action.Command})
		}
	}
	return w.b.db.CompleteInboxWithFinalActions(w.ctx, taskID, w.key.chat, w.key.thread, headerTarget, header, parts, set)
}

// handleFinalAction consumes only its own namespace, including unknown/stale tokens.
func (w *worker) handleFinalAction(in incoming) bool {
	q := in.callback
	if q == nil || !strings.HasPrefix(q.Data, finalActionPrefix) {
		return false
	}
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	reject := func() {
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionUnavailable))
		if err := w.b.db.SettleRejectedFinalAction(ctx, in.id); err != nil {
			w.b.fail(err)
			w.cancel()
		}
	}
	if q.Message == nil || q.Message.From == nil || q.Message.From.ID != w.b.bot.ID || q.From.ID <= 0 || q.Message.Chat.ID != w.key.chat || q.Message.MessageThreadID != w.key.thread || len(w.b.cfg.FinalActions) == 0 {
		reject()
		return true
	}
	command, claimed, err := w.b.db.ReserveFinalAction(ctx, store.FinalActionClaim{
		Token: strings.TrimPrefix(q.Data, finalActionPrefix), Config: finalActionsConfigKey(w.b.cfg.FinalActions),
		InboxID: in.id, Bot: w.b.bot.ID, Chat: q.Message.Chat.ID, Thread: q.Message.MessageThreadID,
		User: q.From.ID, MessageID: q.Message.MessageID, Binding: w.binding,
	})
	if err != nil {
		w.b.fail(err)
		w.cancel()
		return true
	}
	if !claimed {
		reject()
		return true
	}
	_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Received))
	defer w.withFinalAction(&finalActionDispatch{inboxID: in.id})()
	// Shared dispatch derives the sender from this real callback and keeps its inbox ID.
	w.dispatchShortcut(in, command)
	return true
}

// deliverFinalActions attempts markup only; neither failure nor uncertainty resends a body.
func (b *Bridge) deliverFinalActions(ctx context.Context) error {
	output, err := b.db.ClaimFinalActionEdit(ctx)
	if errors.Is(err, sql.ErrNoRows) || err != nil && ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return b.db.ReleaseAssistantEdit(output.ID)
	}
	err = b.tg.EditKeyboard(ctx, output.Chat, output.MessageID, finalActionKeyboard(output.FinalActions))
	if err != nil && ctx.Err() == nil {
		logTelegramFailure(b.telegramLog, slog.LevelWarn, "final_actions_update_failed", "assistant actions update failed", err,
			slog.Int64("outbox_id", output.ID), slog.Int64("chat_id", output.Chat))
	}
	err = b.db.FinishFinalActionEdit(output.ID, output.FinalActions)
	if ctx.Err() != nil {
		return nil
	}
	return err
}
