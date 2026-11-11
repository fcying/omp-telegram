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
const followupCancelPrefix = "followup_cancel:"

type followupPromptResult struct {
	token, instance string
	messageID, user int64
	inline          bool
}

type followupInput struct {
	user, messageID, expiresAt, generation int64
	inline                                 bool
}

func (w *worker) rememberFollowupInput(r store.FollowupRequest) {
	for token, input := range w.followupInputs {
		if token != r.Token && input.user == r.User {
			delete(w.followupInputs, token)
			w.finishFollowupPrompt(input.messageID, input.user, token, input.inline, "")
		}
	}
	if w.followupInputs == nil {
		w.followupInputs = make(map[string]followupInput)
	}
	w.followupInputs[r.Token] = followupInput{user: r.User, messageID: r.PromptID, expiresAt: r.ExpiresAt,
		generation: r.Generation, inline: w.inlineFollowup(r)}
}

func (w *worker) expireFollowupInputs(now time.Time) {
	for token, input := range w.followupInputs {
		if now.Unix() >= input.expiresAt || !w.binding.Running || input.generation != w.binding.Generation {
			delete(w.followupInputs, token)
			w.finishFollowupPrompt(input.messageID, input.user, token, input.inline, "")
		}
	}
}

func (w *worker) followupInputValid(r store.FollowupRequest, now time.Time) bool {
	return r.State == "delivered" && r.PromptID > 0 &&
		r.Instance == w.b.quickActionInstance() && now.Unix() < r.ExpiresAt &&
		w.binding.Running && r.Generation == w.binding.Generation &&
		r.Session == w.binding.Session && r.Workspace == w.binding.Workspace
}

func (b *Bridge) quickActionInstance() string {
	b.quickActionsOnce.Do(func() { b.quickActionsInstance = rand.Text() })
	return b.quickActionsInstance
}

func followupMarker(token string) string { return followupMarkerPrefix + token + "]" }

func (w *worker) inlineFollowup(r store.FollowupRequest) bool {
	return r.Instance == w.b.quickActionInstance() && w.inlineForceReplyUser(r.User)
}

func (w *worker) followupPromptText(user int64, token, notice string) string {
	text := notice
	if text == "" {
		text = "[" + w.tr(i18n.Msg.FollowupPrompt) + "](tg://user?id=" + strconv.FormatInt(user, 10) + ")"
	}
	return text + "\n" + followupMarker(token)
}

func (w *worker) finishFollowupPrompt(messageID, user int64, token string, inline bool, notice string) {
	if inline {
		text := ""
		if notice != "" {
			text = w.followupPromptText(user, token, notice)
		}
		w.queueInlineInputReceipt(messageID, text)
		return
	}
	if notice != "" {
		w.queueForceReplyReceipt(messageID, user, notice, true)
		return
	}
	w.queueForceReplyCleanup(messageID, user)
}

func (w *worker) handleFollowupCancel(ctx context.Context, q *telegram.CallbackQuery) bool {
	token, handled := strings.CutPrefix(q.Data, followupCancelPrefix)
	if !handled {
		return false
	}
	r, err := w.b.db.FollowupRequest(token)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		w.followupPersistenceFailed(err)
		return true
	}
	m := q.Message
	valid := err == nil && m != nil && m.From != nil && m.From.ID == w.b.bot.ID && m.MessageID > 0 &&
		m.Chat.ID == w.key.chat && m.MessageThreadID == w.key.thread && q.From.ID == r.User &&
		r.Bot == w.b.bot.ID && r.Chat == w.key.chat && r.Thread == w.key.thread && w.inlineFollowup(r) &&
		w.binding.Running && r.Generation == w.binding.Generation && r.Session == w.binding.Session &&
		r.Workspace == w.binding.Workspace && time.Now().Unix() < r.ExpiresAt
	if valid {
		if r.PromptID > 0 {
			valid = r.PromptID == m.MessageID
		} else {
			marker, marked := followupReplyToken(m.Text)
			valid = marked && marker == r.Token
		}
	}
	if !valid {
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.FollowupExpired))
		return true
	}
	cancelled, err := w.b.db.CancelFollowupRequest(r)
	if err != nil {
		w.followupPersistenceFailed(err)
		return true
	}
	if !cancelled {
		w.queueInlineInputReceipt(m.MessageID, "")
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.FollowupExpired))
		return true
	}
	delete(w.followupInputs, r.Token)
	w.finishFollowupPrompt(m.MessageID, r.User, r.Token, true, w.tr(i18n.Msg.Canceled))
	_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Canceled))
	return true
}

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
	w.rememberFollowupInput(r)
	// The root reply may target a bot message when a favorite started the task.
	// Mention the user explicitly so selective ForceReply also survives reply fallback.
	prompt := w.followupPromptText(r.User, r.Token, "")
	inline := w.inlineFollowup(r)
	options := telegram.SendOptions{ReplyToMessageID: w.activeReplyTo}
	if inline {
		options.Keyboard = &telegram.Keyboard{ForceReply: true, InlineKeyboard: [][]telegram.Button{{{
			Text: w.tr(i18n.Msg.Cancel), CallbackData: followupCancelPrefix + r.Token,
		}}}}
	} else {
		options.ForceReply = &telegram.ForceReply{ForceReply: true, Selective: true}
	}
	workerCtx, results := w.ctx, w.operations
	previous, next := w.orderForceReply()
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer close(next)
		if previous != nil {
			<-previous
		}
		sendCtx, cancel := context.WithTimeout(workerCtx, 10*time.Second)
		defer cancel()
		message, err := w.sendForceReply(sendCtx, prompt, options, r.User)
		result := operationResult{kind: "followup_prompt", err: err, meta: followupPromptResult{
			token: r.Token, instance: r.Instance, messageID: message.MessageID, user: r.User, inline: inline}}
		select {
		case results <- result:
		case <-workerCtx.Done():
		}
		if workerCtx.Err() != nil {
			w.finishFollowupPrompt(message.MessageID, r.User, r.Token, inline, "")
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
	err := w.b.db.FinishFollowupPrompt(r.token, r.instance, r.messageID, result.err == nil)
	if err != nil {
		delete(w.followupInputs, r.token)
		w.finishFollowupPrompt(r.messageID, r.user, r.token, r.inline, "")
		w.followupPersistenceFailed(err)
		return true
	}
	request, err := w.b.db.FollowupRequest(r.token)
	valid := err == nil && w.followupInputValid(request, time.Now())
	if !valid {
		delete(w.followupInputs, r.token)
		w.finishFollowupPrompt(r.messageID, r.user, r.token, r.inline, "")
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			w.followupPersistenceFailed(err)
		}
		return true
	}
	w.rememberFollowupInput(request)
	return true
}

func (w *worker) cancelFollowupInputs() {
	if err := w.b.db.CancelFollowupRequests(w.b.bot.ID, w.key.chat, w.key.thread); err != nil {
		w.followupPersistenceFailed(err)
	}
	for token, input := range w.followupInputs {
		delete(w.followupInputs, token)
		w.finishFollowupPrompt(input.messageID, input.user, token, input.inline, "")
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
		delete(w.followupInputs, r.Token)
		w.finishFollowupPrompt(r.PromptID, r.User, r.Token, w.inlineFollowup(r), "")
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
	valid := w.followupInputValid(r, time.Now())
	if !valid || reply.MessageID != r.PromptID {
		if !valid {
			delete(w.followupInputs, r.Token)
			w.finishFollowupPrompt(r.PromptID, r.User, r.Token, w.inlineFollowup(r), "")
		}
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
		delete(w.followupInputs, r.Token)
		w.finishFollowupPrompt(r.PromptID, r.User, r.Token, w.inlineFollowup(r), "")
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
	delete(w.followupInputs, r.Token)
	w.finishFollowupPrompt(r.PromptID, r.User, r.Token, w.inlineFollowup(r), w.tr(i18n.Msg.FollowupQueued))
	stripFollowupQuote(in)
	w.queue = append(w.queue, queued{id: in.id, user: message.From.ID, replyTo: message.MessageID, text: text, displayText: text})
	return true
}
