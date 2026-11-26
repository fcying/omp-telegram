package bridge

import (
	"strings"
	"testing"

	"omp-telegram/internal/telegram"
)

const askFormRegressionRadio = `<fieldset><legend>Choice</legend>
	<label><input type="radio" name="choice" value="first" checked>First choice</label>
	<label><input type="radio" name="choice" value="second">Second choice</label>
</fieldset>`

func TestAskFormSingleChoiceReplacesDefault(t *testing.T) {
	for _, tc := range []struct {
		name, field string
	}{
		{"select", `<label>Choice<select name="choice">
			<option value="first">First choice</option>
			<option value="second">Second choice</option>
		</select></label>`},
		{"radio", askFormRegressionRadio},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			askTestEvent(t, w, map[string]any{"id": "replace-default", "method": "form", "html": "<form>" + tc.field + "</form>"})
			askTestButton(t, f, "Use default")
			messageID := w.ask.messageID
			askTestClick(t, w, f, "Second choice")
			askFormRegressionSubmitted(t, w, f, messageID, "replace-default", "choice", "second", "<code>2. Second choice</code>")
		})
	}
}

func TestAskFormSingleChoiceOtherReplacesDefaultOnlyOnSubmit(t *testing.T) {
	for _, tc := range []struct {
		name, wantValue, wantFooter string
	}{
		{"submit", "Custom choice", "<code>Custom answer: Custom choice</code>"},
		{"back", "first", "<code>1. First choice</code>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			askTestEvent(t, w, map[string]any{"id": "other-default", "method": "form", "html": "<form>" + askFormRegressionRadio + "</form>"})
			askTestButton(t, f, "Use default")
			messageID := w.ask.messageID
			askTestClick(t, w, f, "Custom answer")
			prompt := askTestPrompt(t, f)
			if tc.name == "back" {
				askTestClick(t, w, f, "Back to options")
				if got := askTestState(t, w); len(got) != 0 {
					t.Fatalf("returning to options submitted a response: %+v", got)
				}
				askTestClick(t, w, f, "Use default")
			} else {
				askTestReply(t, w, 200, 7, 11, "Custom choice", prompt)
			}
			askFormRegressionSubmitted(t, w, f, messageID, "other-default", "choice", tc.wantValue, tc.wantFooter)
		})
	}
}

func TestAskFormCustomReplyDoesNotSendClosingMessage(t *testing.T) {
	w, f, _ := privateAskTestWorker(t)
	askTestEvent(t, w, map[string]any{"id": "custom-receipt", "method": "form", "html": "<form>" + askFormRegressionRadio + "</form>"})
	messageID := w.ask.messageID
	askTestClick(t, w, f, "Custom answer")
	prompt := askTestPrompt(t, f)
	if !w.ask.inputInline {
		t.Fatal("private custom prompt did not use editable inline input")
	}
	f.mu.Lock()
	markup, _ := f.messages[len(f.messages)-1]["reply_markup"].(map[string]any)
	force := markup["force_reply"] == true
	f.mu.Unlock()
	if !force {
		t.Fatal("private custom prompt did not enable automatic Reply")
	}
	askTestReply(t, w, 200, 7, 11, "Custom choice", prompt)
	askFormRegressionSubmitted(t, w, f, messageID, "custom-receipt", "choice", "Custom choice", "<code>Custom answer: Custom choice</code>")
	w.background.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	compact := false
	for _, message := range f.messages {
		if sentReplyTarget(message) == float64(prompt.MessageID) {
			t.Fatalf("successful custom input sent an extra reply: %+v", message)
		}
		if message["message_id"] == float64(prompt.MessageID) {
			text, _ := message["text"].(string)
			if strings.HasPrefix(text, "Custom answer\n[ask:") && !strings.Contains(text, "Reply to this message") {
				compact = true
			}
		}
	}
	if !compact {
		t.Fatal("custom input prompt did not become a compact reply target")
	}
}

func TestPrivateAskCustomPromptBackButton(t *testing.T) {
	w, f, _ := privateAskTestWorker(t)
	askTestEvent(t, w, map[string]any{"id": "custom-back", "method": "form", "html": "<form>" + askFormRegressionRadio + "</form>"})
	messageID := w.ask.messageID
	askTestClick(t, w, f, "Custom answer")
	promptID := w.ask.inputID
	f.mu.Lock()
	promptMarkup, _ := f.messages[len(f.messages)-1]["reply_markup"].(map[string]any)
	rows, _ := promptMarkup["inline_keyboard"].([]any)
	if len(rows) == 0 {
		f.mu.Unlock()
		t.Fatal("custom prompt has no buttons")
	}
	buttons, _ := rows[0].([]any)
	if len(buttons) < 2 || len(buttons) > 3 || buttons[0].(map[string]any)["text"] != "Back to options" || buttons[1].(map[string]any)["text"] != "Cancel" {
		f.mu.Unlock()
		t.Fatalf("custom prompt buttons = %v", buttons)
	}
	for i := len(f.messages) - 2; i >= 0; i-- {
		message := f.messages[i]
		if message["message_id"] == float64(messageID) {
			markup, _ := message["reply_markup"].(map[string]any)
			if options, _ := markup["inline_keyboard"].([]any); len(options) != 0 {
				f.mu.Unlock()
				t.Fatalf("question retained buttons: %v", options)
			}
			break
		}
	}
	f.mu.Unlock()
	messageCount := f.messageCount()
	data := askTestButton(t, f, "Back to options")
	w.callback(&telegram.CallbackQuery{ID: "prompt-back", From: telegram.User{ID: 7}, Data: data,
		Message: &telegram.Message{MessageID: promptID, Chat: telegram.Chat{ID: w.key.chat}, MessageThreadID: w.key.thread}})
	askTestSettle(t, w)
	if w.ask == nil || w.ask.input || len(askTestState(t, w)) != 0 {
		t.Fatal("custom prompt Back did not return to options without submitting")
	}
	w.background.Wait()
	deleted := f.deletedMessageIDs()
	if len(deleted) != 1 || deleted[0] != promptID {
		t.Fatalf("Back deleted messages %v, want only custom prompt %d", deleted, promptID)
	}
	f.mu.Lock()
	for _, message := range f.messages[messageCount:] {
		if message["message_id"] != float64(messageID) {
			f.mu.Unlock()
			t.Fatal("Back sent an extra message")
		}
	}
	f.mu.Unlock()
	askTestClick(t, w, f, "Custom answer")
	prompt := askTestPrompt(t, f)
	if prompt.MessageID == promptID {
		t.Fatal("re-entering custom input reused the old prompt")
	}
	askTestReply(t, w, 200, 7, 11, "Custom choice", prompt)
	askFormRegressionSubmitted(t, w, f, messageID, "custom-back", "choice", "Custom choice", "<code>Custom answer: Custom choice</code>")
}

func TestPrivateAskCustomPromptCancelButton(t *testing.T) {
	w, f, _ := privateAskTestWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.previewResult = make(chan previewResult, 4)
	preview := sendRealtimePreview(t, w)
	askTestEvent(t, w, map[string]any{"id": "custom-cancel", "method": "form", "html": "<form>" + askFormRegressionRadio + "</form>"})
	askTestClick(t, w, f, "Custom answer")
	messageID := w.ask.messageID
	promptID := w.ask.inputID
	messageCount := f.messageCount()
	data := askTestButton(t, f, "Cancel")
	w.callback(&telegram.CallbackQuery{ID: "prompt-cancel", From: telegram.User{ID: 7}, Data: data,
		Message: &telegram.Message{MessageID: promptID, Chat: telegram.Chat{ID: w.key.chat}, MessageThreadID: w.key.thread}})
	responses := askTestState(t, w)
	if w.ask != nil || len(responses) != 1 || !responses[0].Cancelled {
		t.Fatalf("custom prompt Cancel: ask=%v responses=%+v", w.ask, responses)
	}
	waitFor(t, func() bool { return len(f.deletedMessageIDs()) != 0 })
	deleted := f.deletedMessageIDs()
	if len(deleted) != 1 || deleted[0] != promptID {
		t.Fatalf("Cancel deleted messages %v, want only custom prompt %d", deleted, promptID)
	}
	f.mu.Lock()
	for _, message := range f.messages[messageCount:] {
		if message["message_id"] != float64(messageID) {
			f.mu.Unlock()
			t.Fatal("Cancel sent an extra message")
		}
	}
	f.mu.Unlock()
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := len(f.messages) - 1; i >= 0; i-- {
			message := f.messages[i]
			if message["message_id"] != float64(messageID) {
				continue
			}
			text, _ := message["text"].(string)
			markup, _ := message["reply_markup"].(map[string]any)
			rows, ok := markup["inline_keyboard"].([]any)
			return strings.Contains(text, "Canceled") && ok && len(rows) == 0
		}
		return false
	})
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","stopReason":"aborted"}]}`))
	if state := queueState(t, w, 100); state != "cancelled" {
		select {
		case err := <-w.b.fatal:
			t.Fatalf("cancelled task state = %q, fatal = %v", state, err)
		default:
			t.Fatalf("cancelled task state = %q, active = %d, context = %v", state, w.active, w.ctx.Err())
		}
	}
	awaitProgressDeletion(t, w, 100, preview.id)
	deleted = f.deletedMessageIDs()
	if len(deleted) != 2 {
		t.Fatalf("cancel did not delete both the input prompt and progress message: %v", deleted)
	}
	for _, reply := range researchReplies(t, w, 100) {
		if strings.Contains(reply, "Task was cancelled before completion.") {
			t.Fatalf("cancelled question produced redundant task notice: %q", reply)
		}
	}
}

func TestAskCancellationDoesNotHideLaterTaskCancellation(t *testing.T) {
	w, f, _ := privateAskTestWorker(t)
	askTestEvent(t, w, map[string]any{"id": "cancel-then-continue", "method": "form", "html": "<form>" + askFormRegressionRadio + "</form>"})
	askTestClick(t, w, f, "Custom answer")
	data := askTestButton(t, f, "Cancel")
	w.callback(&telegram.CallbackQuery{ID: "prompt-cancel", From: telegram.User{ID: 7}, Data: data,
		Message: &telegram.Message{MessageID: w.ask.inputID, Chat: telegram.Chat{ID: w.key.chat}, MessageThreadID: w.key.thread}})
	askTestSettle(t, w)
	w.event([]byte(`{"type":"tool_execution_start","toolCallId":"next-tool","toolName":"test"}`))
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","stopReason":"aborted"}]}`))
	if replies := researchReplies(t, w, 100); len(replies) == 0 || !strings.Contains(strings.Join(replies, "\n"), "Task was cancelled before completion.") {
		t.Fatalf("later task cancellation notice missing: %q", replies)
	}
}

func TestAskCancellationNoticeAfterNonStreamingAssistantText(t *testing.T) {
	for _, source := range []string{"message_end", "agent_end"} {
		t.Run(source, func(t *testing.T) {
			w, f, _ := privateAskTestWorker(t)
			askTestEvent(t, w, map[string]any{"id": "cancel-before-text", "method": "form", "html": "<form>" + askFormRegressionRadio + "</form>"})
			askTestClick(t, w, f, "Custom answer")
			data := askTestButton(t, f, "Cancel")
			w.callback(&telegram.CallbackQuery{ID: "cancel", From: telegram.User{ID: 7}, Data: data,
				Message: &telegram.Message{MessageID: w.ask.inputID, Chat: telegram.Chat{ID: w.key.chat}, MessageThreadID: w.key.thread}})
			askTestSettle(t, w)
			if source == "message_end" {
				w.event([]byte(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"new response"}]}}`))
				w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","stopReason":"aborted"}]}`))
			} else {
				w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","stopReason":"aborted","content":[{"type":"text","text":"new response"}]}]}`))
			}
			replies := strings.Join(researchReplies(t, w, 100), "\n")
			if !strings.Contains(replies, "new response") || !strings.Contains(replies, "Task was cancelled before completion.") {
				t.Fatalf("missing assistant body or cancellation notice: %q", replies)
			}
		})
	}
}

func TestGroupAskCustomReplyClearsForceReply(t *testing.T) {
	w, f, _ := askTestWorker(t)
	askTestEvent(t, w, map[string]any{"id": "group-custom", "method": "form", "html": "<form>" + askFormRegressionRadio + "</form>"})
	askTestClick(t, w, f, "Custom answer")
	prompt := askTestPrompt(t, f)
	askTestReply(t, w, 200, 7, 11, "Custom choice", prompt)
	w.background.Wait()
	if !f.forceReplyClosed(prompt.MessageID) {
		t.Fatal("group custom reply did not dismiss its selective ForceReply")
	}
	responses := askTestState(t, w)
	if len(responses) != 1 || responses[0].Data["choice"] != "Custom choice" {
		t.Fatalf("group custom answer = %+v", responses)
	}
}

func TestPrivateFormTextUsesQuestionAsReplyPrompt(t *testing.T) {
	w, f, _ := privateAskTestWorker(t)
	askTestEvent(t, w, map[string]any{"id": "choice-and-text", "method": "form", "html": "<form>" + askFormRegressionRadio + `<label>Notes <input name="notes" required></label></form>`})
	askTestClick(t, w, f, "Second choice")
	prompt := askTestPrompt(t, f)
	if !w.ask.inlineInput || prompt.MessageID != w.ask.messageID || prompt.MessageID != w.ask.inputID {
		t.Fatalf("text question is not the reply target: prompt=%d question=%d input=%d", prompt.MessageID, w.ask.messageID, w.ask.inputID)
	}
	f.mu.Lock()
	markup, _ := f.messages[prompt.MessageID-1]["reply_markup"].(map[string]any)
	forceReply := markup["force_reply"] == true
	before := len(f.messages)
	f.mu.Unlock()
	if !forceReply {
		t.Fatal("text question did not request Reply")
	}
	askTestReply(t, w, 200, 7, 11, "  notes  ", prompt)
	responses := askTestState(t, w)
	if len(responses) != 1 || responses[0].Cancelled || responses[0].Data["choice"] != "second" || responses[0].Data["notes"] != "  notes  " {
		t.Fatalf("form response = %+v", responses)
	}
	waitFor(t, func() bool { return f.inlineInputClosed(prompt.MessageID) })
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, message := range f.messages[before:] {
		if _, isEdit := message["message_id"]; !isEdit {
			t.Fatalf("text reply sent an extra message: %+v", message)
		}
	}
}

func TestPrivateFormTextInvalidReplyUsesNewQuestion(t *testing.T) {
	w, f, _ := privateAskTestWorker(t)
	askTestEvent(t, w, map[string]any{"id": "text-retry", "method": "form", "html": `<form><label>Code <input name="code" pattern="ok" required></label></form>`})
	old := askTestPrompt(t, f)
	askTestReply(t, w, 200, 7, 11, "bad", old)
	retry := askTestPrompt(t, f)
	if retry.MessageID == old.MessageID || retry.MessageID != w.ask.messageID || retry.MessageID != w.ask.inputID {
		t.Fatalf("invalid text did not create a new reply target: old=%d retry=%d", old.MessageID, retry.MessageID)
	}
	askTestReply(t, w, 201, 7, 11, "ok", retry)
	responses := askTestState(t, w)
	if len(responses) != 1 || responses[0].Data["code"] != "ok" {
		t.Fatalf("validated response = %+v", responses)
	}
}

func TestPrivateFormTextCancelDoesNotSendClosingMessage(t *testing.T) {
	w, f, _ := privateAskTestWorker(t)
	askTestEvent(t, w, map[string]any{"id": "text-cancel", "method": "form", "html": `<form><label>Notes <input name="notes" required></label></form>`})
	prompt := askTestPrompt(t, f)
	before := f.messageCount()
	askTestClick(t, w, f, "Cancel")
	responses := askTestState(t, w)
	if len(responses) != 1 || !responses[0].Cancelled {
		t.Fatalf("cancelled response = %+v", responses)
	}
	waitFor(t, func() bool { return f.inlineInputClosed(prompt.MessageID) })
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, message := range f.messages[before:] {
		if _, isEdit := message["message_id"]; !isEdit {
			t.Fatalf("text cancellation sent an extra message: %+v", message)
		}
	}
}

func TestPrivateFormCancelledTextReplyIsExpired(t *testing.T) {
	w, f, trace := privateAskTestWorker(t)
	askTestEvent(t, w, map[string]any{"id": "text-cancel-stale", "method": "form", "html": `<form><label>Notes <input name="notes" required></label></form>`})
	prompt := askTestPrompt(t, f)
	askTestClick(t, w, f, "Cancel")
	var receipt string
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := len(f.messages) - 1; i >= 0; i-- {
			message := f.messages[i]
			if message["message_id"] == float64(prompt.MessageID) {
				receipt, _ = message["text"].(string)
				return strings.Contains(receipt, "Canceled")
			}
		}
		return false
	})
	prompt.Text = receipt
	askTestReply(t, w, 200, 7, 11, "stale answer", prompt)
	if state := queueState(t, w, 200); state != "cancelled" {
		t.Fatalf("stale reply state = %q", state)
	}
	if len(w.queue) != 0 || len(w.deferredSteers) != 0 || researchTraceCount(trace, "prompt", "stale answer") != 0 || researchTraceCount(trace, "steer", "stale answer") != 0 {
		t.Fatal("stale reply became task or guidance")
	}
	var notices int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE chat=? AND thread=? AND text LIKE ?", w.key.chat, w.key.thread, "%This Ask input is unavailable or expired.%").Scan(&notices))
	if notices != 1 {
		t.Fatalf("expired input notices = %d, want 1", notices)
	}
}

func TestAskFormTextEmptyReceiptDoesNotShowDefault(t *testing.T) {
	for _, tc := range []struct {
		name, button, wantValue, wantFooter string
	}{
		{"empty", "Submit empty", "", "Empty string (0 characters)"},
		{"default", "Use default", "Original draft", "<code>Original draft</code>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			askTestEvent(t, w, map[string]any{
				"id": "notes", "method": "form",
				"html": `<form><label>Notes<textarea name="notes">Original draft</textarea></label></form>`,
			})
			messageID := w.ask.messageID
			askTestClick(t, w, f, tc.button)
			askFormRegressionSubmitted(t, w, f, messageID, "notes", "notes", tc.wantValue, tc.wantFooter)
		})
	}
}

func askFormRegressionSubmitted(t *testing.T, w *worker, f *fakeHTTP, messageID int64, id, field, value, footer string) {
	t.Helper()
	got := askTestState(t, w)
	if len(got) != 1 || got[0].ID != id || got[0].Cancelled || len(got[0].Data) != 1 || got[0].Data[field] != value {
		t.Fatalf("native responses = %+v, want one submission of %s=%q for %q", got, field, value, id)
	}
	receipt := askTestReceipt(t, w, f, messageID)
	_, submitted, ok := strings.Cut(receipt, "<b>Submitted</b>")
	if !ok || strings.TrimSpace(submitted) != footer {
		t.Fatalf("receipt = %q, want Submitted footer %q", receipt, footer)
	}
}
