package bridge

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
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
	placeholder        string
	prefill            *string
	questions          []askQuestion
	answers            []askAnswer
	form               *askForm
	formData           map[string]any
	question           int
	epoch              uint64
	messageID, inputID int64
	token, inputToken  string
	input              bool
	inlineInput        bool
	inputInline        bool
	selected           map[int]bool
	custom             string
	page               int
	pages              []selectionPage
	cancel             context.CancelFunc
	pendingInput       *incoming
	deferredUI         *rpcEvent
	cancelledText      string
	cancelledEpoch     uint64
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
	Value       string `json:"value,omitempty"`
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
	inputInline        bool
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
	w.askCancelReceipt = false
	a := &askRequest{client: client, generation: w.binding.Generation, session: w.sessionID,
		active: w.active, turn: w.turn, user: w.owner, id: e.ID, method: e.Method,
		questions: e.Questions, catalogEpoch: catalog.Epoch, placeholder: e.Placeholder, prefill: e.Prefill}
	if a.user == 0 || e.Secret {
		w.sendAskResponse(a, map[string]any{"cancelled": true})
		return
	}
	if e.Method == "form" {
		form, err := parseAskForm(e.HTML)
		if err != nil {
			w.sendAskResponse(a, map[string]any{"cancelled": true})
			if err == errAskFormPattern {
				w.say(w.tr(i18n.Msg.AskFormPatternUnsupported))
			} else {
				w.say(w.tr(i18n.Msg.AskDisplayFailed))
			}
			return
		}
		a.form, a.formData = form, form.data
		a.questions = make([]askQuestion, len(form.fields))
		for index := range form.fields {
			a.questions[index] = form.fields[index].question
		}
		if len(a.questions) == 0 {
			w.sendAskResponse(a, map[string]any{"data": a.formData})
			return
		}
		w.prepareAskFormField(a)
	} else {
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
		if e.Method == "editor" || e.Method == "input" {
			a.input = true
		}
	}
	a.inlineInput = e.Method != "ask" && e.Method != "form" && w.inlineForceReplyUser(a.user)
	w.ask = a
	w.renderAsk(a)
}

func (w *worker) prepareAskFormField(a *askRequest) {
	field := a.form.fields[a.question]
	a.selected = make(map[int]bool)
	a.custom, a.page, a.input = "", 0, false
	a.prefill, a.placeholder = nil, field.placeholder
	if field.inputType != "radio" && field.inputType != "checkbox" && field.inputType != "select" {
		a.input = true
		if field.value != "" {
			value := field.value
			a.prefill = &value
		}
		return
	}
	for index, selected := range field.selected {
		if selected {
			a.selected[index] = true
		}
	}
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

func (w *worker) closeAskInput(a *askRequest, messageID int64, inline bool) {
	if inline {
		w.queueInlineInputReceipt(messageID, "")
		return
	}
	w.queueForceReplyCleanup(messageID, a.user)
}

func (w *worker) deleteAskInput(messageID, user int64) {
	if messageID == 0 {
		return
	}
	previous, next := w.orderForceReply()
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer close(next)
		if previous != nil {
			<-previous
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.b.tg.Delete(ctx, w.key.chat, messageID); err != nil {
			logTelegramFailure(w.b.telegramLog, slog.LevelWarn, "ask_input_delete_failed", "Ask input deletion failed", err,
				slog.Int64("chat_id", w.key.chat), slog.Int64("thread_id", w.key.thread), slog.Int64("message_id", messageID))
		}
		if w.forceReplyPrompts[user] == messageID {
			w.forceReplyPrompts[user] = 0
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
	w.closeAskInput(a, a.inputID, a.inlineInput || a.inputInline)
	a.inputID, a.inputToken, a.inputInline = 0, "", false
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
	w.cancelAskInteraction(a)
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
	w.cancelAskInteraction(w.ask)
}

func (w *worker) cancelAskInteraction(a *askRequest) {
	receipt := ""
	if (a.method == "ask" || a.form != nil) && a.cancelledText == "" && a.question < len(a.questions) && (a.messageID != 0 || a.cancel != nil) {
		question := a.questions[a.question]
		receipt = w.askQuestionText(question, 0, len(question.Options), "**"+w.tr(i18n.Msg.Canceled)+"**")
		a.cancelledText = receipt
		if a.cancel != nil {
			// The in-flight display must return before its final cancellation edit.
			a.cancelledEpoch = a.epoch
		}
	}
	w.invalidateAskInteraction(a)
	if !a.inlineInput {
		w.queueKeyboardCleanup(a.messageID)
	}
	if receipt != "" && a.cancelledEpoch == 0 {
		w.queueAskReceipt(a.messageID, receipt)
	}
}

func (w *worker) renderAsk(a *askRequest) {
	w.invalidateAskInteraction(a)
	if !w.askScope(a) || w.ask != a {
		w.endAsk(true)
		return
	}
	a.token = rand.Text()
	question := a.questions[a.question]
	question.Header = fmt.Sprintf("Ask · %d/%d\n%s", a.question+1, len(a.questions), question.Header)
	start, end := 0, 0
	if !a.input {
		if len(a.pages) == 0 {
			a.pages = selectionPages(question.Options)
		}
		page := a.pages[a.page]
		start, end = page.start, page.end
	}
	footer := ""
	if a.custom != "" {
		budget := 1400
		if !a.input {
			budget = 500
		}
		footer = "**" + w.tr(i18n.Msg.AskCustom) + "**\n" + askLiteral(a.custom, budget)
	}
	text := ""
	if !a.inlineInput {
		if a.input {
			text = w.askQuestionTextBudget(question, 0, 0, footer, 3700, false)
		} else {
			text = w.selectionPageText(question, a.pages[a.page], a.page, len(a.pages), "", footer)
		}
	}
	keyboard := &telegram.Keyboard{}
	add := func(label, action string) {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegram.Button{{Text: label, CallbackData: a.token + ":" + action}})
	}
	if a.input {
		if a.form != nil {
			field := a.form.fields[a.question]
			if field.inputType == "radio" || field.inputType == "checkbox" || field.inputType == "select" {
				add(w.tr(i18n.Msg.AskBack), "back")
				if !field.required {
					add(w.tr(i18n.Msg.AskEmpty), "empty")
				}
			} else {
				if !field.required {
					add(w.tr(i18n.Msg.AskEmpty), "empty")
				}
				if a.prefill != nil {
					add(w.tr(i18n.Msg.AskDefault), "default")
				}
			}
		} else if a.method == "ask" {
			add(w.tr(i18n.Msg.AskBack), "back")
		} else {
			add(w.tr(i18n.Msg.AskEmpty), "empty")
			if a.method == "editor" && a.prefill != nil {
				add(w.tr(i18n.Msg.AskDefault), "default")
			}
		}
	} else {
		for index := start; index < end; index++ {
			label := strconv.Itoa(index + 1)
			if question.Multi {
				if a.selected[index] {
					label = "☑ " + label
				} else {
					label = "☐ " + label
				}
			}
			appendSelectionButton(keyboard, index-start, telegram.Button{Text: label, CallbackData: a.token + ":" + strconv.Itoa(index)})
		}
		if a.page > 0 {
			add(w.tr(i18n.Msg.Previous), "previous")
		}
		if a.page+1 < len(a.pages) {
			add(w.tr(i18n.Msg.Next), "next")
		}
		if a.form == nil || a.form.fields[a.question].other {
			add(w.tr(i18n.Msg.AskCustom), "custom")
		}
		if question.Multi {
			add(w.tr(i18n.Msg.AskConfirm), "confirm")
		} else if a.form != nil {
			field := a.form.fields[a.question]
			if hasAskSelection(a.selected) {
				add(w.tr(i18n.Msg.AskDefault), "default")
			} else if !field.required {
				add(w.tr(i18n.Msg.AskEmpty), "empty")
			}
		}
	}
	add(w.tr(i18n.Msg.Cancel), "cancel")
	inputInline := !a.inlineInput && a.input && w.inlineForceReplyUser(a.user) && (a.method == "ask" || a.form != nil && len(question.Options) > 0)
	var inputKeyboard *telegram.Keyboard
	if inputInline {
		rows := keyboard.InlineKeyboard
		inputKeyboard = &telegram.Keyboard{ForceReply: true, InlineKeyboard: [][]telegram.Button{{rows[0][0], rows[len(rows)-1][0]}}}
		inputKeyboard.InlineKeyboard = append(inputKeyboard.InlineKeyboard, rows[1:len(rows)-1]...)
		keyboard = &telegram.Keyboard{InlineKeyboard: [][]telegram.Button{}}
	}
	var prompt string
	if a.input {
		a.inputToken = rand.Text()
		if a.inlineInput {
			if a.placeholder != "" {
				footer = "**" + w.tr(i18n.Msg.AskPlaceholder) + "**\n" + progressToolLiteral(clipUTF16(a.placeholder, 500)+"\n")
			}
			if a.method == "editor" && a.prefill != nil {
				if footer != "" {
					footer += "\n\n"
				}
				footer += "**" + w.tr(i18n.Msg.AskPrefill) + "**\n" + progressToolLiteral(clipUTF16(*a.prefill, 500)+"\n")
			}
			text = w.askQuestionTextBudget(question, start, end, footer, 3700, false) + "\n" + askMarkerPrefix + a.inputToken + "]"
			keyboard.ForceReply = true
		} else {
			questionLimit := 1000
			if a.method != "ask" {
				questionLimit = 500
			}
			detail := clipUTF16(question.Question, questionLimit)
			if a.method != "ask" {
				if a.placeholder != "" {
					detail += "\n\n" + w.tr(i18n.Msg.AskPlaceholder) + "\n" + clipUTF16(a.placeholder, 500)
				}
				if (a.method == "editor" || a.form != nil) && a.prefill != nil {
					detail += "\n\n" + w.tr(i18n.Msg.AskPrefill) + "\n" + clipUTF16(*a.prefill, 500)
				}
			}
			// Keep question Markdown from introducing extra selective mentions.
			prompt = "[" + w.tr(i18n.Msg.AskInput) + "](tg://user?id=" + strconv.FormatInt(a.user, 10) + ")" +
				progressToolLiteral(detail+"\n") + "\n" + askMarkerPrefix + a.inputToken + "]"
		}
	}
	result := askDisplay{request: a, epoch: a.epoch, messageID: a.messageID, token: a.token, inputInline: inputInline}
	interactionCtx, cancel := context.WithCancel(w.ctx)
	a.cancel = cancel
	tg, key, results, workerCtx := w.b.tg, w.key, w.operations, w.ctx
	var previous <-chan struct{}
	var next chan struct{}
	if a.input {
		previous, next = w.orderForceReply()
	}
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer cancel()
		if next != nil {
			defer close(next)
			if previous != nil {
				<-previous
			}
		}
		ctx, displayCancel := context.WithTimeout(interactionCtx, 10*time.Second)
		defer displayCancel()
		var err error
		if result.messageID == 0 {
			var message telegram.Message
			if a.inlineInput {
				message, err = w.sendForceReply(ctx, text, telegram.SendOptions{Keyboard: keyboard}, a.user)
				result.inputID = message.MessageID
			} else {
				message, err = tg.Send(ctx, key.chat, key.thread, text, telegram.SendOptions{Keyboard: keyboard})
			}
			result.messageID = message.MessageID
		} else {
			err = tg.Edit(ctx, key.chat, result.messageID, text, keyboard)
		}
		if err == nil && prompt != "" {
			var message telegram.Message
			options := telegram.SendOptions{ForceReply: &telegram.ForceReply{ForceReply: true, Selective: true}, ReplyToMessageID: result.messageID}
			if result.inputInline {
				options.ForceReply = nil
				options.Keyboard = inputKeyboard
			}
			message, err = w.sendForceReply(ctx, prompt, options, a.user)
			result.inputID = message.MessageID
		}
		select {
		case results <- operationResult{kind: "ask_display", err: err, meta: result}:
		case <-workerCtx.Done():
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()
			_ = tg.ClearKeyboard(cleanupCtx, key.chat, result.messageID)
		}
		if workerCtx.Err() != nil {
			w.closeAskInput(a, result.inputID, a.inlineInput || result.inputInline)
		}
	}()
}

func (w *worker) askOperationFinished(result operationResult) bool {
	switch result.kind {
	case "ask_receipt":
		if result.err != nil {
			w.queueKeyboardCleanup(result.meta.(int64))
		}
		return true
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
			w.closeAskInput(a, display.inputID, a.inlineInput || display.inputInline)
			if a.cancelledEpoch != 0 && a.cancelledEpoch == display.epoch {
				a.cancelledEpoch = 0
				w.queueAskReceipt(display.messageID, a.cancelledText)
			} else {
				if !a.inlineInput {
					w.queueKeyboardCleanup(display.messageID)
				}
			}
			return true
		}
		a.cancel = nil
		a.messageID, a.inputID, a.inputInline = display.messageID, display.inputID, display.inputInline
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
		(q.Message.MessageID != a.messageID && (!a.inputInline || q.Message.MessageID != a.inputID)) || q.Message.Chat.ID != w.key.chat || q.Message.MessageThreadID != w.key.thread {
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
		return callbackDone
	}
	_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Received))
	question := a.questions[a.question]
	switch action {
	case "cancel":
		w.askCancelReceipt = a.messageID != 0 && (a.method == "ask" || a.form != nil)
		if a.input && a.inputInline {
			w.deleteAskInput(a.inputID, a.user)
			a.inputID = 0
		}
		w.endAsk(true)
		return callbackDone
	case "empty", "default":
		if a.form != nil {
			field := a.form.fields[a.question]
			if action == "default" {
				if a.input {
					if a.prefill == nil {
						return callbackDone
					}
					a.custom = *a.prefill
				} else {
					clear(a.selected)
					for index, selected := range field.selected {
						if selected {
							a.selected[index] = true
						}
					}
					a.custom = ""
				}
			} else {
				a.custom = ""
				if !a.input {
					clear(a.selected)
				}
			}
			w.advanceAsk(a)
		} else if a.input && (a.method == "input" || a.method == "editor") {
			if action == "default" {
				if a.method != "editor" || a.prefill == nil {
					return callbackDone
				}
				a.custom = *a.prefill
			} else {
				a.custom = ""
			}
			w.advanceAsk(a)
		}
		return callbackDone
	case "back":
		if a.input && a.inputInline {
			w.deleteAskInput(a.inputID, a.user)
			a.inputID = 0
		}
		if a.form != nil && a.input {
			a.input = false
			a.custom = ""
			w.renderAsk(a)
		} else if a.input && a.method == "ask" {
			a.input = false
			w.renderAsk(a)
		}
		return callbackDone
	case "custom":
		if !a.input && (a.form == nil || a.form.fields[a.question].other) {
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
		if page >= 0 && page < len(a.pages) {
			a.page = page
			w.renderAsk(a)
		}
	case "confirm":
		if question.Multi || a.form != nil {
			w.advanceAsk(a)
		}
	default:
		index, err := strconv.Atoi(action)
		page := a.pages[a.page]
		if err != nil || index < page.start || index >= page.end {
			return callbackDone
		}
		if question.Multi {
			a.selected[index] = !a.selected[index]
			w.renderAsk(a)
		} else {
			clear(a.selected)
			a.selected[index] = true
			w.advanceAsk(a)
		}
	}
	return callbackDone
}

func (w *worker) advanceAsk(a *askRequest) {
	if a.form != nil {
		w.advanceAskForm(a)
		return
	}

	// Freeze this question before the mutable request advances to another message.
	question := a.questions[a.question]
	text := ""
	if !a.inlineInput {
		text = w.askQuestionText(question, 0, len(question.Options), w.askAnswerFooter(a))
	}
	messageID := a.messageID
	w.invalidateAskInteraction(a)
	a.messageID = 0
	if !a.inlineInput {
		w.queueAskReceipt(messageID, text)
	}
	if a.method != "ask" {
		w.endAsk(false)
		w.sendAskResponse(a, map[string]any{"value": a.custom})
		return
	}
	answer := askAnswer{ID: question.ID, SelectedOptions: []string{}, CustomInput: a.custom}
	for index, option := range question.Options {
		if a.selected[index] {
			answer.SelectedOptions = append(answer.SelectedOptions, option.Label)
		}
	}
	a.answers = append(a.answers, answer)
	a.question++
	a.pages = nil
	if a.question < len(a.questions) {
		clear(a.selected)
		a.custom, a.page, a.input = "", 0, false
		w.renderAsk(a)
		return
	}
	w.endAsk(false)
	w.sendAskResponse(a, map[string]any{"answers": a.answers})
}

func (w *worker) advanceAskForm(a *askRequest) {
	field := a.form.fields[a.question]
	question := a.questions[a.question]
	var value any
	include := true
	if field.inputType == "radio" || field.inputType == "checkbox" || field.inputType == "select" {
		selectedValues := make([]string, 0, len(a.selected)+1)
		for index, option := range question.Options {
			if a.selected[index] {
				selectedValues = append(selectedValues, option.Value)
			}
		}
		other := strings.TrimSpace(a.custom)
		if other != "" {
			if !field.other {
				w.say(w.tr(i18n.Msg.AskFormInvalid))
				w.renderAsk(a)
				return
			}
			selectedValues = append(selectedValues, other)
		}
		if question.Multi && field.optionCount > 1 {
			value = selectedValues
			if field.required && len(selectedValues) == 0 {
				w.say(w.tr(i18n.Msg.AskFormInvalid))
				w.renderAsk(a)
				return
			}
		} else if len(selectedValues) > 0 {
			value = selectedValues[0]
		} else {
			include = false
			if field.required {
				w.say(w.tr(i18n.Msg.AskFormInvalid))
				w.renderAsk(a)
				return
			}
		}
	} else {
		if reason := askFormValidateText(field, a.custom); reason != "" {
			w.say(w.tr(i18n.Msg.AskFormInvalid))
			a.input = true
			a.custom = ""
			w.renderAsk(a)
			return
		}
		value = askFormTextValue(field, a.custom)
	}
	if include {
		a.formData[field.name] = value
	}
	text := ""
	if !a.inlineInput {
		text = w.askQuestionText(question, 0, len(question.Options), w.askAnswerFooter(a))
	}
	messageID := a.messageID
	w.invalidateAskInteraction(a)
	a.messageID = 0
	if !a.inlineInput {
		w.queueAskReceipt(messageID, text)
	}
	a.question++
	a.pages = nil
	if a.question < len(a.questions) {
		w.prepareAskFormField(a)
		w.renderAsk(a)
		return
	}
	w.endAsk(false)
	w.sendAskResponse(a, map[string]any{"data": a.formData})
}

// Telegram receipts are display-only and cannot settle or replay native work.
func (w *worker) queueAskReceipt(messageID int64, text string) {
	if messageID == 0 {
		return
	}
	tg, key, results, workerCtx := w.b.tg, w.key, w.operations, w.ctx
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		ctx, cancel := context.WithTimeout(workerCtx, 5*time.Second)
		defer cancel()
		keyboard := &telegram.Keyboard{InlineKeyboard: [][]telegram.Button{}}
		err := tg.Edit(ctx, key.chat, messageID, text, keyboard)
		select {
		case results <- operationResult{kind: "ask_receipt", err: err, meta: messageID}:
		case <-workerCtx.Done():
		}
	}()
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
	text := message.Text
	if len(message.Photo) == 0 && message.Document == nil && message.From != nil &&
		message.Chat.ID == w.key.chat && message.MessageThreadID == w.key.thread &&
		reply.From != nil && reply.From.ID == w.b.bot.ID && reply.Chat.ID == w.key.chat {
		command, _, foreign := parseSlashInvocation(strings.TrimSpace(text), w.b.bot.Username)
		researchOff := false
		if command == "/autoresearch" {
			args, research := autoresearchArguments(strings.TrimSpace(text), w.b.bot.Username)
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
	if a.method == "ask" {
		text = strings.TrimSpace(text)
	}
	if text == "" || len(message.Photo) != 0 || message.Document != nil {
		w.say(w.tr(i18n.Msg.AskTextRequired))
		w.mark(in.id, store.InboxCancelled)
		return true
	}
	if !w.mark(in.id, store.InboxSubmitted) {
		return true
	}
	if !a.inlineInput && (a.method == "ask" || a.form != nil && len(a.questions[a.question].Options) > 0) {
		if a.inputInline {
			w.queueInlineInputReceipt(a.inputID, w.tr(i18n.Msg.AskCustom)+"\n"+askMarkerPrefix+a.inputToken+"]")
			w.forgetForceReply(a.inputID, a.user)
		} else {
			w.queueForceReplyReceipt(a.inputID, a.user, w.tr(i18n.Msg.AskReplyReceived), false)
		}
		a.inputID = 0
	}
	a.custom = text
	a.input = false
	if a.questions[a.question].Multi {
		w.renderAsk(a)
	} else {
		if a.form != nil && len(a.selected) != 0 && strings.TrimSpace(text) != "" {
			// Only a submitted Other answer replaces the single-choice default.
			clear(a.selected)
		}
		w.advanceAsk(a)
	}
	w.mark(in.id, store.InboxDone)
	return true
}
