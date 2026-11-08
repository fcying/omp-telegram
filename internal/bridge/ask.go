package bridge

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"
	"strings"
	"time"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

// Request identity is immutable; interaction epochs fence edits and replies.
type askRequest struct {
	client             *omp.Client
	generation         int64
	session            string
	active             int64
	turn               uint64
	catalogEpoch       uint64
	user               int64
	id, method         string
	questions          []askQuestion
	answers            []askAnswer
	question           int
	epoch              uint64
	messageID, inputID int64
	token, inputToken  string
	input              bool
	selected           map[int]bool
	custom             string
	page               int
	cancel             context.CancelFunc
	pendingInput       *incoming
	deferredUI         *rpcEvent
}

type askQuestion struct {
	ID       string      `json:"id"`
	Question string      `json:"question"`
	Header   string      `json:"header"`
	Options  []askOption `json:"options"`
	Multi    bool        `json:"multi"`
}

type askOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview"`
}

type askAnswer struct {
	ID              string   `json:"id"`
	SelectedOptions []string `json:"selectedOptions"`
	CustomInput     string   `json:"customInput,omitempty"`
}

type askDisplay struct {
	request            *askRequest
	epoch              uint64
	messageID, inputID int64
	token              string
}

// Per-runtime receipts prevent duplicate requests from reopening consumed UI.
const maxUIReceipts = 4096
const askMarkerPrefix = "[ask:"

func (w *worker) askScope(a *askRequest) bool {
	return w.askRuntimeScope(a) && w.active == a.active && w.turn == a.turn
}

func (w *worker) askRuntimeScope(a *askRequest) bool {
	if a == nil || w.client != a.client || w.binding.Generation != a.generation || w.sessionID != a.session {
		return false
	}
	catalog := a.client.CommandCatalog()
	return catalog.Epoch == a.catalogEpoch && catalog.SessionID == a.session
}

func (w *worker) admitUI(e rpcEvent, client *omp.Client) bool {
	if e.ID == "" {
		return false
	}
	catalog := client.CommandCatalog()
	if catalog.SessionID != w.sessionID {
		return false
	}
	if _, seen := w.uiSeen[e.ID]; seen {
		return false
	}
	if len(w.uiSeen) >= maxUIReceipts {
		w.retireTaskRuntime(w.tr(i18n.Msg.AskUnconfirmed))
		return false
	}
	if w.uiSeen == nil {
		w.uiSeen = make(map[string]struct{})
	}
	w.uiSeen[e.ID] = struct{}{}
	// Even cancellation can resume native work during the session snapshot.
	if w.exportingSession != "" {
		return false
	}
	busy := w.uiStopped || w.ask != nil
	for _, c := range w.confirms {
		if c.action == "ui" {
			busy = true
			break
		}
	}
	if busy || w.askSending != 0 {
		a := &askRequest{client: client, generation: w.binding.Generation, session: w.sessionID,
			active: w.active, turn: w.turn, user: w.owner, id: e.ID, method: e.Method, catalogEpoch: catalog.Epoch}
		if busy {
			w.sendAskResponse(a, map[string]any{"cancelled": true})
		} else {
			// The next native question may precede our response-write notification.
			deferred := e
			a.deferredUI = &deferred
			w.ask = a
		}
		return false
	}
	return true
}

func (w *worker) beginAsk(e rpcEvent) {
	client, connected := w.runtimeClient()
	if !connected || e.ID == "" {
		return
	}
	catalog := client.CommandCatalog()
	if catalog.SessionID != w.sessionID {
		return
	}
	a := &askRequest{client: client, generation: w.binding.Generation, session: w.sessionID,
		active: w.active, turn: w.turn, user: w.owner, id: e.ID, method: e.Method,
		questions: e.Questions, catalogEpoch: catalog.Epoch}
	if a.user == 0 || e.Secret {
		w.sendAskResponse(a, map[string]any{"cancelled": true})
		return
	}
	if e.Method != "ask" {
		a.questions = []askQuestion{{ID: e.ID, Question: e.Title}}
	}
	if len(a.questions) == 0 {
		w.sendAskResponse(a, map[string]any{"cancelled": true})
		return
	}
	if e.Method == "ask" {
		ids := make(map[string]bool, len(a.questions))
		for _, question := range a.questions {
			if question.ID == "" || ids[question.ID] {
				w.sendAskResponse(a, map[string]any{"cancelled": true})
				return
			}
			ids[question.ID] = true
		}
		a.selected = make(map[int]bool)
		a.answers = make([]askAnswer, 0, len(a.questions))
	}
	w.ask = a
	if e.Method == "editor" || e.Method == "input" {
		a.input = true
	}
	w.renderAsk(a)
}

// Local consumption precedes the only send attempt. No business ACK exists.
func (w *worker) sendAskResponse(a *askRequest, fields map[string]any) {
	fields["type"], fields["id"] = "extension_ui_response", a.id
	w.askSending++
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
		defer cancel()
		err := a.client.Send(ctx, fields)
		select {
		case w.operations <- operationResult{kind: "ask_response", err: err, meta: a}:
		case <-w.ctx.Done():
		}
	}()
}

func (w *worker) invalidateAskInteraction(a *askRequest) {
	a.deferredUI = nil
	if a.pendingInput != nil {
		w.mark(a.pendingInput.id, store.InboxCancelled)
		a.pendingInput = nil
	}
	a.epoch++
	delete(w.confirms, a.token)
	a.token = ""
	a.inputID, a.inputToken = 0, ""
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
}

func (w *worker) endAsk(sendCancel bool) {
	a := w.ask
	if a == nil {
		return
	}
	w.ask = nil
	w.invalidateAskInteraction(a)
	w.queueKeyboardCleanup(a.messageID)
	if sendCancel && w.client == a.client {
		w.sendAskResponse(a, map[string]any{"cancelled": true})
	}
}

// Stop revokes input immediately; its existing owner-specific path stops OMP.
func (w *worker) pauseAsk() {
	if w.controlInProgress() && w.nativeControl == nil && w.research.control == nil {
		return
	}
	w.uiStopped = true
	if w.ask == nil {
		return
	}
	w.invalidateAskInteraction(w.ask)
	w.queueKeyboardCleanup(w.ask.messageID)
}

func (w *worker) renderAsk(a *askRequest) {
	w.invalidateAskInteraction(a)
	if !w.askScope(a) || w.ask != a {
		w.endAsk(true)
		return
	}
	a.token = rand.Text()
	question := a.questions[a.question]
	text := fmt.Sprintf("Ask · %d/%d\n%s", a.question+1, len(a.questions), question.Question)
	if question.Header != "" {
		text += "\n" + question.Header
	}
	keyboard := &telegram.Keyboard{}
	add := func(label, action string) {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegram.Button{{Text: label, CallbackData: a.token + ":" + action}})
	}
	if a.input {
		if a.method == "ask" {
			add(w.tr(i18n.Msg.AskBack), "back")
		}
	} else {
		start := a.page * uiSelectPageSize
		end := min(start+uiSelectPageSize, len(question.Options))
		for index := start; index < end; index++ {
			option := question.Options[index]
			label := option.Label
			if question.Multi {
				if a.selected[index] {
					label = "☑ " + label
				} else {
					label = "☐ " + label
				}
			}
			add(menuText(label, 100), strconv.Itoa(index))
			if option.Description != "" {
				text += "\n" + option.Label + ": " + option.Description
			}
			if option.Preview != "" {
				text += "\n" + option.Preview
			}
		}
		if start > 0 {
			add(w.tr(i18n.Msg.Previous), "previous")
		}
		if end < len(question.Options) {
			add(w.tr(i18n.Msg.Next), "next")
		}
		if a.custom != "" {
			text += "\n" + w.tr(i18n.Msg.AskCustom) + ": " + a.custom
		}
		add(w.tr(i18n.Msg.AskCustom), "custom")
		if question.Multi {
			add(w.tr(i18n.Msg.AskConfirm), "confirm")
		}
	}
	add(w.tr(i18n.Msg.Cancel), "cancel")
	var prompt string
	if a.input {
		a.inputToken = rand.Text()
		// Force a fenced literal block so question Markdown cannot expand or add mentions.
		prompt = "[" + w.tr(i18n.Msg.AskInput) + "](tg://user?id=" + strconv.FormatInt(a.user, 10) + ")" +
			progressToolLiteral(clipUTF16(question.Question, 1000)+"\n") + "\n" + askMarkerPrefix + a.inputToken + "]"
	}
	result := askDisplay{request: a, epoch: a.epoch, messageID: a.messageID, token: a.token}
	text = clipUTF16(text, 3700)
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	a.cancel = cancel
	tg, key, results, workerCtx := w.b.tg, w.key, w.operations, w.ctx
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer cancel()
		var err error
		if result.messageID == 0 {
			var message telegram.Message
			message, err = tg.Send(ctx, key.chat, key.thread, text, telegram.SendOptions{Keyboard: keyboard})
			result.messageID = message.MessageID
		} else {
			err = tg.Edit(ctx, key.chat, result.messageID, text, keyboard)
		}
		if err == nil && prompt != "" {
			var message telegram.Message
			message, err = tg.Send(ctx, key.chat, key.thread, prompt, telegram.SendOptions{
				ForceReply: &telegram.ForceReply{ForceReply: true, Selective: true}, ReplyToMessageID: result.messageID})
			result.inputID = message.MessageID
		}
		select {
		case results <- operationResult{kind: "ask_display", err: err, meta: result}:
		case <-workerCtx.Done():
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()
			_ = tg.ClearKeyboard(cleanupCtx, key.chat, result.messageID)
		}
	}()
}

func (w *worker) askOperationFinished(result operationResult) bool {
	switch result.kind {
	case "ask_response":
		w.askSending--
		a := result.meta.(*askRequest)
		if result.err != nil && w.askRuntimeScope(a) {
			w.retireTaskRuntime(w.tr(i18n.Msg.AskUnconfirmed))
			return true
		}
		pending := w.ask
		if w.askSending == 0 && pending != nil && pending.deferredUI != nil {
			client, connected := w.runtimeClient()
			if w.uiStopped || !connected || client != pending.client || !w.askScope(pending) {
				w.endAsk(false)
				return true
			}
			event := *pending.deferredUI
			pending.deferredUI = nil
			w.ask = nil
			w.applyUI(event, client)
		}
		return true
	case "ask_display":
		display := result.meta.(askDisplay)
		a := display.request
		if w.ask != a || !w.askScope(a) || a.epoch != display.epoch {
			w.queueKeyboardCleanup(display.messageID)
			return true
		}
		a.cancel = nil
		a.messageID, a.inputID = display.messageID, display.inputID
		if result.err != nil {
			w.endAsk(true)
			w.say(w.tr(i18n.Msg.AskDisplayFailed))
			return true
		}
		w.confirms[display.token] = confirmation{action: "ask", user: a.user, generation: a.generation,
			epoch: a.epoch, messageID: a.messageID, uiClientID: a.client.ID()}
		w.touchActivity()
		if a.pendingInput != nil {
			in := *a.pendingInput
			a.pendingInput = nil
			w.handle(in)
		}
		return true
	}
	return false
}

func (w *worker) askCallback(ctx context.Context, q *telegram.CallbackQuery, token, action string, c confirmation) callbackResult {
	a := w.ask
	if !w.askScope(a) || a.token != token || a.epoch != c.epoch || q.Message == nil ||
		q.Message.MessageID != a.messageID || q.Message.Chat.ID != w.key.chat || q.Message.MessageThreadID != w.key.thread {
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
		return callbackDone
	}
	_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Received))
	question := a.questions[a.question]
	switch action {
	case "cancel":
		w.endAsk(true)
		return callbackDone
	case "back":
		if a.input && a.method == "ask" {
			a.input = false
			w.renderAsk(a)
		}
		return callbackDone
	case "custom":
		if !a.input {
			a.input = true
			w.renderAsk(a)
		}
		return callbackDone
	}
	if a.input {
		return callbackDone
	}
	switch action {
	case "previous", "next":
		page := a.page - 1
		if action == "next" {
			page = a.page + 1
		}
		if page >= 0 && page*uiSelectPageSize < len(question.Options) {
			a.page = page
			w.renderAsk(a)
		}
	case "confirm":
		if question.Multi {
			w.advanceAsk(a)
		}
	default:
		index, err := strconv.Atoi(action)
		if err != nil || index < a.page*uiSelectPageSize || index >= min((a.page+1)*uiSelectPageSize, len(question.Options)) {
			return callbackDone
		}
		if question.Multi {
			a.selected[index] = !a.selected[index]
			w.renderAsk(a)
		} else {
			a.selected[index] = true
			w.advanceAsk(a)
		}
	}
	return callbackDone
}

func (w *worker) advanceAsk(a *askRequest) {
	if a.method != "ask" {
		w.endAsk(false)
		w.sendAskResponse(a, map[string]any{"value": a.custom})
		return
	}
	question := a.questions[a.question]
	answer := askAnswer{ID: question.ID, SelectedOptions: []string{}, CustomInput: a.custom}
	for index, option := range question.Options {
		if a.selected[index] {
			answer.SelectedOptions = append(answer.SelectedOptions, option.Label)
		}
	}
	a.answers = append(a.answers, answer)
	a.question++
	if a.question < len(a.questions) {
		clear(a.selected)
		a.custom, a.page, a.input = "", 0, false
		w.renderAsk(a)
		return
	}
	w.endAsk(false)
	w.sendAskResponse(a, map[string]any{"answers": a.answers})
}

func (w *worker) routeAskInput(in *incoming) bool {
	if in.msg == nil || in.msg.ReplyToMessage == nil {
		return false
	}
	message, reply := in.msg, in.msg.ReplyToMessage
	line := reply.Text[strings.LastIndexByte(reply.Text, '\n')+1:]
	marker, marked := strings.CutPrefix(line, askMarkerPrefix)
	marked = marked && strings.HasSuffix(marker, "]") && reply.From != nil && reply.From.ID == w.b.bot.ID
	a := w.ask
	currentID := a != nil && a.inputID != 0 && reply.MessageID == a.inputID
	if !marked && !currentID {
		return false
	}
	text := strings.TrimSpace(message.Text)
	if len(message.Photo) == 0 && message.Document == nil && message.From != nil &&
		message.Chat.ID == w.key.chat && message.MessageThreadID == w.key.thread &&
		reply.From != nil && reply.From.ID == w.b.bot.ID && reply.Chat.ID == w.key.chat {
		command, _, foreign := parseSlashInvocation(text, w.b.bot.Username)
		researchOff := false
		if command == "/autoresearch" {
			args, research := autoresearchArguments(text, w.b.bot.Username)
			researchOff = research && args == "off"
		}
		// Commands retain normal routing even when the Ask prompt is stale or still sending.
		if foreign || knownBridgeCommand(command) || researchOff {
			stripFollowupQuote(in)
			return false
		}
	}
	// A reply can reach the actor before its send result. Wait for the exact
	// delivered message ID instead of interpreting it as a prompt or answer.
	if w.askScope(a) && a.input && a.inputID == 0 && a.cancel != nil && a.pendingInput == nil &&
		marked && marker == a.inputToken+"]" && message.From != nil && message.From.ID == a.user &&
		message.Chat.ID == w.key.chat && message.MessageThreadID == w.key.thread && reply.Chat.ID == w.key.chat {
		pending := *in
		a.pendingInput = &pending
		return true
	}
	if !w.askScope(a) || !a.input || !currentID || marker != a.inputToken+"]" ||
		message.From == nil || message.From.ID != a.user || reply.From == nil || reply.From.ID != w.b.bot.ID ||
		message.Chat.ID != w.key.chat || message.MessageThreadID != w.key.thread || reply.Chat.ID != w.key.chat {
		w.say(w.tr(i18n.Msg.AskExpired))
		w.mark(in.id, store.InboxCancelled)
		return true
	}
	if text == "" || len(message.Photo) != 0 || message.Document != nil {
		w.say(w.tr(i18n.Msg.AskTextRequired))
		w.mark(in.id, store.InboxCancelled)
		return true
	}
	if !w.mark(in.id, store.InboxSubmitted) {
		return true
	}
	a.custom = text
	a.input = false
	if a.questions[a.question].Multi {
		w.renderAsk(a)
	} else {
		w.advanceAsk(a)
	}
	w.mark(in.id, store.InboxDone)
	return true
}
