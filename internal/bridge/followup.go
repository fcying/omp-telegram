package bridge

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

const followupMarkerPrefix = "[followup:"
const followupLifetime = 10 * time.Minute

type followupPromptResult struct {
	token, instance string
	messageID       int64
}

func (b *Bridge) quickActionInstance() string {
	b.quickActionsOnce.Do(func() { b.quickActionsInstance = rand.Text() })
	return b.quickActionsInstance
}

func followupMarker(token string) string { return followupMarkerPrefix + token + "]" }

func followupReplyToken(text string) (string, bool) {
	// The bridge appends exactly one marker line after the prompt body.
	lineStart := strings.LastIndexByte(text, '\n')
	if lineStart <= 0 || strings.TrimSpace(text[:lineStart]) == "" {
		return "", false
	}
	line := text[lineStart+1:]
	rest, found := strings.CutPrefix(line, followupMarkerPrefix)
	if !found {
		return "", false
	}
	token, closed := strings.CutSuffix(rest, "]")
	if !closed || token == "" {
		return "", false
	}
	for _, c := range token {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return "", false
		}
	}
	return token, true
}

func (w *worker) followupPersistenceFailed(err error) {
	w.b.storeLog.Error("follow-up persistence failed", "event", "followup_write_failed")
	w.b.fail(err)
	w.cancel()
}

// handleProgressFollowup shares the Stop token without consuming it. Creating
// the persisted request precedes the network send, so uncertain sends are safe.
func (w *worker) handleProgressFollowup(ctx context.Context, q *telegram.CallbackQuery, token, index string, c confirmation) bool {
	if index != "followup" {
		return false
	}
	if c.action != "stop" || c.user != q.From.ID || q.Message == nil || q.Message.MessageID <= 0 ||
		q.Message.Chat.ID != w.key.chat || q.Message.MessageThreadID != w.key.thread ||
		(c.messageID != 0 && c.messageID != q.Message.MessageID) ||
		c.generation != w.binding.Generation || c.active != w.active || c.turn != w.turn || !w.taskRunning() ||
		(!c.expires.IsZero() && !time.Now().Before(c.expires)) || !w.binding.Running || w.binding.Session == "" || w.migrationLocked() {
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.FollowupExpired))
		return true
	}
	r := store.FollowupRequest{
		Token: rand.Text(), Instance: w.b.quickActionInstance(), Bot: w.b.bot.ID,
		Chat: w.key.chat, Thread: w.key.thread, User: q.From.ID, TaskID: w.active,
		Workspace: w.binding.Workspace, Session: w.binding.Session, Generation: w.binding.Generation,
		ExpiresAt: time.Now().Add(followupLifetime).Unix(),
	}
	if err := w.b.db.CreateFollowupRequest(r); err != nil {
		w.followupPersistenceFailed(err)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionUnavailable))
		return true
	}
	// The root reply may target a bot message when a favorite started the task.
	// Mention the user explicitly so selective ForceReply also survives reply fallback.
	prompt := "[" + w.tr(i18n.Msg.FollowupPrompt) + "](tg://user?id=" + strconv.FormatInt(q.From.ID, 10) + ")\n" + followupMarker(r.Token)
	options := telegram.SendOptions{ReplyToMessageID: w.activeReplyTo, ForceReply: &telegram.ForceReply{ForceReply: true, Selective: true}}
	tg, key, workerCtx, results := w.b.tg, w.key, w.ctx, w.operations
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		sendCtx, cancel := context.WithTimeout(workerCtx, 10*time.Second)
		defer cancel()
		message, err := tg.Send(sendCtx, key.chat, key.thread, prompt, options)
		result := operationResult{kind: "followup_prompt", err: err, meta: followupPromptResult{token: r.Token, instance: r.Instance, messageID: message.MessageID}}
		select {
		case results <- result:
		case <-workerCtx.Done():
		}
	}()
	_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Received))
	return true
}

// Completion deliberately has no active-task/turn fence: natural settlement
// does not invalidate a request for the same logical session binding.
func (w *worker) followupPromptFinished(result operationResult) bool {
	if result.kind != "followup_prompt" {
		return false
	}
	r, ok := result.meta.(followupPromptResult)
	if !ok {
		return true
	}
	if err := w.b.db.FinishFollowupPrompt(r.token, r.instance, r.messageID, result.err == nil); err != nil {
		w.followupPersistenceFailed(err)
	}
	return true
}

func (w *worker) cancelFollowupInputs() {
	if err := w.b.db.CancelFollowupRequests(w.b.bot.ID, w.key.chat, w.key.thread); err != nil {
		w.followupPersistenceFailed(err)
	}
}

func stripFollowupQuote(in *incoming) {
	message := *in.msg
	message.ReplyToMessage = nil
	message.Quote = nil
	in.msg = &message
}

// routeFollowupInput runs before all ordinary text and attachment routing. Both
// a durable prompt ID and the visible marker recognize stale/uncertain replies.
func (w *worker) routeFollowupInput(in *incoming) bool {
	if in.msg == nil || in.msg.ReplyToMessage == nil {
		return false
	}
	message, reply := in.msg, in.msg.ReplyToMessage
	token, marked := followupReplyToken(reply.Text)
	marked = marked && reply.From != nil && reply.From.ID == w.b.bot.ID
	r, err := w.b.db.FollowupPrompt(w.b.bot.ID, message.Chat.ID, reply.MessageID)
	if errors.Is(err, sql.ErrNoRows) && marked {
		r, err = w.b.db.FollowupRequest(token)
	}
	if errors.Is(err, sql.ErrNoRows) {
		if marked {
			w.say(w.tr(i18n.Msg.FollowupExpired))
			w.mark(in.id, store.InboxCancelled)
			return true
		}
		return false
	}
	if err != nil {
		w.followupPersistenceFailed(err)
		return true
	}
	authorized := message.From != nil && message.From.ID == r.User &&
		r.Bot == w.b.bot.ID && r.Chat == w.key.chat && r.Thread == w.key.thread &&
		message.Chat.ID == r.Chat && message.MessageThreadID == r.Thread &&
		reply.From != nil && reply.From.ID == r.Bot && reply.Chat.ID == r.Chat
	if !authorized {
		w.say(w.tr(i18n.Msg.FollowupExpired))
		w.mark(in.id, store.InboxCancelled)
		return true
	}
	if r.State == "consumed" && r.InboxID == in.id {
		for _, queued := range w.queue {
			if queued.id == in.id {
				return true
			}
		}
		if err := w.b.db.CancelPendingFollowupInbox(in.id); err != nil {
			w.followupPersistenceFailed(err)
		}
		return true
	}
	if r.State != "delivered" || r.PromptID <= 0 || reply.MessageID != r.PromptID ||
		r.Instance != w.b.quickActionInstance() || time.Now().Unix() >= r.ExpiresAt ||
		!w.binding.Running || r.Generation != w.binding.Generation || r.Session != w.binding.Session || r.Workspace != w.binding.Workspace {
		w.say(w.tr(i18n.Msg.FollowupExpired))
		w.mark(in.id, store.InboxCancelled)
		return true
	}
	text := strings.TrimSpace(message.Text)
	command, _, foreign := parseSlashInvocation(text, w.b.bot.Username)
	// Explicit Bridge commands retain their lifecycle and authorization paths;
	// only the bridge-generated quote is removed, never arbitrary user context.
	if len(message.Photo) == 0 && message.Document == nil && strings.HasPrefix(text, "/") && !foreign && knownBridgeCommand(command) {
		stripFollowupQuote(in)
		return false
	}
	if text == "" || len(message.Photo) != 0 || message.Document != nil {
		w.say(w.tr(i18n.Msg.FollowupTextRequired))
		w.mark(in.id, store.InboxCancelled)
		return true
	}
	if w.handleMigrationIncoming(*in) {
		return true
	}
	if _, err := w.ensureRuntime(); err != nil {
		if w.ctx.Err() == nil {
			w.say(w.errorText(err))
			w.mark(in.id, store.InboxCancelled)
		}
		return true
	}
	if w.pendingInputFull() {
		w.say(w.tr(i18n.Msg.Queue.Full))
		w.mark(in.id, store.InboxCancelled)
		return true
	}
	// Runtime restoration may change the binding. Recheck before durable claim.
	if r.Generation != w.binding.Generation || r.Session != w.binding.Session || r.Workspace != w.binding.Workspace {
		w.say(w.tr(i18n.Msg.FollowupExpired))
		w.mark(in.id, store.InboxCancelled)
		return true
	}
	accepted, err := w.b.db.ConsumeFollowupRequest(r, in.id)
	if err != nil {
		w.followupPersistenceFailed(err)
		return true
	}
	if !accepted {
		w.say(w.tr(i18n.Msg.FollowupExpired))
		w.mark(in.id, store.InboxCancelled)
		return true
	}
	stripFollowupQuote(in)
	w.queue = append(w.queue, queued{id: in.id, user: message.From.ID, replyTo: message.MessageID, text: text, displayText: text})
	w.say(w.tr(i18n.Msg.FollowupQueued))
	return true
}
