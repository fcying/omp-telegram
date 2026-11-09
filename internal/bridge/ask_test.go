package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/telegram"
)

type askTestAnswer struct {
	ID       string   `json:"id"`
	Selected []string `json:"selectedOptions"`
	Custom   string   `json:"customInput"`
}

type askTestResponse struct {
	ID        string          `json:"id"`
	Value     string          `json:"value"`
	Cancelled bool            `json:"cancelled"`
	Confirmed *bool           `json:"confirmed"`
	Answers   []askTestAnswer `json:"answers"`
}

func askTestWorker(t *testing.T) (*worker, *fakeHTTP, string) {
	t.Helper()
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	f := http.DefaultTransport.(*fakeHTTP)
	w.b.cfg.ProgressMode = "off"
	nativeInput(t, w, 100, "wait", false)
	researchBarrier(t, w)
	if w.active != 100 || !w.busy {
		t.Fatal("Ask fixture has no active root")
	}
	return w, f, trace
}

func askTestEvent(t *testing.T, w *worker, event map[string]any) {
	t.Helper()
	event["type"] = "extension_ui_request"
	nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": event})
	researchBarrier(t, w)
	askTestSettle(t, w)
}

func askTestQuestion(id, question string, multi bool, labels ...string) map[string]any {
	options := make([]map[string]string, 0, len(labels))
	for _, label := range labels {
		options = append(options, map[string]string{"label": label, "description": "Description for " + label})
	}
	return map[string]any{"id": id, "question": question, "multi": multi, "options": options}
}

func askTestOpen(t *testing.T, w *worker, id string, questions ...map[string]any) {
	t.Helper()
	askTestEvent(t, w, map[string]any{"id": id, "method": "ask", "questions": questions})
	if w.ask == nil {
		t.Fatal("native Ask was not presented")
	}
}

func askTestState(t *testing.T, w *worker) []askTestResponse {
	t.Helper()
	askTestSettle(t, w)
	raw, err := w.call("get_state", nil)
	requireStoreOK(t, err)
	var state struct {
		Responses []askTestResponse `json:"fixtureUIResponses"`
	}
	requireStoreOK(t, json.Unmarshal(raw, &state))
	return state.Responses
}

func askTestSettle(t *testing.T, w *worker) {
	t.Helper()
	nativeUntil(t, w, func() bool { return w.askSending == 0 && (w.ask == nil || w.ask.cancel == nil) })
}

func askTestCallback(w *worker, user int64, data string) {
	token, _, _ := strings.Cut(data, ":")
	c := w.confirms[token]
	w.callback(&telegram.CallbackQuery{ID: "ask-test", From: telegram.User{ID: user}, Data: data,
		Message: &telegram.Message{MessageID: c.messageID, Chat: telegram.Chat{ID: w.key.chat}, MessageThreadID: w.key.thread}})
}

func askTestButton(t *testing.T, f *fakeHTTP, label string) string {
	t.Helper()
	_, rows := pickerView(t, f)
	for _, row := range rows {
		for _, button := range row {
			if text, _ := button["text"].(string); strings.Contains(strings.ToLower(text), strings.ToLower(label)) {
				return button["callback_data"].(string)
			}
		}
	}
	t.Fatalf("Ask button containing %q missing: %v", label, rows)
	return ""
}

func askTestClick(t *testing.T, w *worker, f *fakeHTTP, label string) {
	t.Helper()
	if w.ask != nil && label == "Other" {
		label = "Custom answer"
	}
	if w.ask != nil && label == "Done" {
		label = "Confirm selection"
	}
	askTestCallback(w, 7, askTestButton(t, f, label))
	researchBarrier(t, w)
	askTestSettle(t, w)
}

func askTestPrompt(t *testing.T, f *fakeHTTP) *telegram.Message {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.messages) - 1; i >= 0; i-- {
		text, _ := f.messages[i]["text"].(string)
		if strings.Contains(text, "[ask:") {
			return &telegram.Message{MessageID: int64(i + 1), Text: text, From: &telegram.User{ID: 99, IsBot: true}, Chat: telegram.Chat{ID: -10}, MessageThreadID: 11}
		}
	}
	t.Fatal("question-specific Ask reply prompt missing")
	return nil
}

func askTestReply(t *testing.T, w *worker, id, user, thread int64, text string, prompt *telegram.Message) {
	t.Helper()
	u := update(id, thread, text)
	u.Message.From.ID = user
	u.Message.ReplyToMessage = prompt
	raw, err := json.Marshal(u)
	requireStoreOK(t, err)
	requireStoreOK(t, w.b.db.Accept(id, raw))
	w.handle(incoming{id: id, msg: u.Message})
	w.dispatch()
	if w.client != nil {
		researchBarrier(t, w)
	}
	askTestSettle(t, w)
}

func TestAskCombinedAnswersStayInOriginalTurn(t *testing.T) {
	w, f, trace := askTestWorker(t)
	root, turn, client := w.rootRequestID, w.turn, w.client
	askTestOpen(t, w, "combined",
		askTestQuestion("topic", "Which topic?", false, "Programming", "Investing"),
		askTestQuestion("features", "Which features?", true, "Auth", "Billing", "Search"),
		askTestQuestion("language", "Which language?", false, "Go", "Rust"))
	askTestClick(t, w, f, "Programming")
	if responses := askTestState(t, w); len(responses) != 0 {
		t.Fatalf("partial Ask submitted: %+v", responses)
	}
	askTestClick(t, w, f, "Search")
	askTestClick(t, w, f, "Auth")
	askTestClick(t, w, f, "Done")
	askTestClick(t, w, f, "Other")
	prompt := askTestPrompt(t, f)
	if !strings.Contains(prompt.Text, "Which language?") {
		t.Fatalf("reply prompt lost its question: %q", prompt.Text)
	}
	askTestReply(t, w, 200, 7, 11, "  Elixir  ", prompt)
	responses := askTestState(t, w)
	if len(responses) != 1 || responses[0].ID != "combined" || len(responses[0].Answers) != 3 {
		t.Fatalf("combined Ask response = %+v", responses)
	}
	answers := responses[0].Answers
	if answers[0].ID != "topic" || strings.Join(answers[0].Selected, ",") != "Programming" || answers[0].Custom != "" ||
		answers[1].ID != "features" || strings.Join(answers[1].Selected, ",") != "Auth,Search" ||
		answers[2].ID != "language" || len(answers[2].Selected) != 0 || answers[2].Custom != "Elixir" {
		t.Fatalf("ordered labels/custom answer = %+v", answers)
	}
	if w.ask != nil || w.rootRequestID != root || w.turn != turn || w.client != client || w.active != 100 || !w.busy {
		t.Fatal("Ask changed native turn ownership")
	}
	if researchTraceCount(trace, "prompt", "wait") != 1 || researchTraceCount(trace, "prompt", "Elixir") != 0 || researchTraceCount(trace, "steer", "Elixir") != 0 {
		t.Fatal("Ask text became a new model input")
	}
	for _, entry := range fixtureRPCTrace(trace) {
		if (entry.kind == "prompt" || entry.kind == "steer") && strings.Contains(entry.payload, "Elixir") {
			t.Fatal("custom reply leaked into prompt/steer")
		}
	}
}

func TestAskAllowsEmptyMultiAndMultiCustom(t *testing.T) {
	w, f, _ := askTestWorker(t)
	askTestOpen(t, w, "multi", askTestQuestion("empty", "Optional features?", true, "Auth"), askTestQuestion("mixed", "Other features?", true, "Search", "Billing"))
	askTestClick(t, w, f, "Done")
	askTestClick(t, w, f, "Search")
	askTestClick(t, w, f, "Other")
	askTestReply(t, w, 200, 7, 11, "Analytics", askTestPrompt(t, f))
	askTestClick(t, w, f, "Done")
	responses := askTestState(t, w)
	if len(responses) != 1 || len(responses[0].Answers) != 2 {
		t.Fatalf("multi response = %+v", responses)
	}
	if a := responses[0].Answers[0]; a.ID != "empty" || a.Selected == nil || len(a.Selected) != 0 || a.Custom != "" {
		t.Fatalf("empty multi = %+v", a)
	}
	if a := responses[0].Answers[1]; a.ID != "mixed" || strings.Join(a.Selected, ",") != "Search" || a.Custom != "Analytics" {
		t.Fatalf("mixed multi = %+v", a)
	}
}

func TestAskTextRequiresCurrentBotPromptAndOwner(t *testing.T) {
	w, f, trace := askTestWorker(t)
	askTestOpen(t, w, "text", askTestQuestion("topic", "What topic?", false, "Go"))
	staleButton := askTestButton(t, f, "Go")
	askTestClick(t, w, f, "Other")
	prompt := askTestPrompt(t, f)
	for i, invalid := range []struct {
		user, thread int64
		text         string
		prompt       *telegram.Message
	}{
		{7, 11, " \n\t ", prompt},
		{8, 11, "wrong-owner-canary", prompt},
		{7, 12, "wrong-topic-canary", prompt},
		{7, 11, "wrong-id-canary", &telegram.Message{MessageID: prompt.MessageID + 1000, Text: prompt.Text, From: prompt.From}},
		{7, 11, "forged-bot-canary", &telegram.Message{MessageID: prompt.MessageID, Text: prompt.Text, From: &telegram.User{ID: 8}}},
	} {
		askTestReply(t, w, int64(200+i), invalid.user, invalid.thread, invalid.text, invalid.prompt)
		if w.ask == nil || len(askTestState(t, w)) != 0 {
			t.Fatalf("invalid text %d resolved Ask", i)
		}
	}
	askTestCallback(w, 7, staleButton)
	researchBarrier(t, w)
	if len(askTestState(t, w)) != 0 {
		t.Fatal("old keyboard answered the custom text question")
	}
	askTestReply(t, w, 220, 7, 11, "accepted-canary", prompt)
	responses := askTestState(t, w)
	if len(responses) != 1 || responses[0].Answers[0].Custom != "accepted-canary" {
		t.Fatalf("valid answer = %+v", responses)
	}
	askTestReply(t, w, 221, 7, 11, "stale-text-canary", prompt)
	if len(askTestState(t, w)) != 1 {
		t.Fatal("old text prompt answered twice")
	}
	for _, entry := range fixtureRPCTrace(trace) {
		if (entry.kind == "prompt" || entry.kind == "steer") && strings.Contains(entry.payload, "canary") {
			t.Fatalf("Ask reply escaped as model input: %+v", entry)
		}
	}
}

func TestAskCallbacksEnforceOwnerTopicAndEpoch(t *testing.T) {
	w, f, _ := askTestWorker(t)
	askTestOpen(t, w, "two", askTestQuestion("first", "First?", false, "Alpha"), askTestQuestion("second", "Second?", false, "Beta"))
	first := askTestButton(t, f, "Alpha")
	token, _, _ := strings.Cut(first, ":")
	messageID := w.confirms[token].messageID
	for _, callback := range []*telegram.CallbackQuery{
		{ID: "wrong-user", From: telegram.User{ID: 8}, Message: &telegram.Message{MessageID: messageID, Chat: telegram.Chat{ID: -10}, MessageThreadID: 11}, Data: first},
		{ID: "wrong-topic", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID, Chat: telegram.Chat{ID: -10}, MessageThreadID: 12}, Data: first},
		{ID: "wrong-chat", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID, Chat: telegram.Chat{ID: -20}, MessageThreadID: 11}, Data: first},
	} {
		w.callback(callback)
		researchBarrier(t, w)
		if len(askTestState(t, w)) != 0 {
			t.Fatal("unauthorized callback answered Ask")
		}
	}
	askTestClick(t, w, f, "Alpha")
	askTestCallback(w, 7, first)
	researchBarrier(t, w)
	if len(askTestState(t, w)) != 0 {
		t.Fatal("old question token answered new question")
	}
	askTestClick(t, w, f, "Beta")
	if responses := askTestState(t, w); len(responses) != 1 || len(responses[0].Answers) != 2 || strings.Join(responses[0].Answers[1].Selected, ",") != "Beta" {
		t.Fatalf("current callback = %+v", responses)
	}
}

func TestAskFollowupAndDuplicatePreserveInteraction(t *testing.T) {
	w, f, _ := askTestWorker(t)
	question := askTestQuestion("topic", "Which topic?", false, "Go")
	askTestOpen(t, w, "first", question)
	choice := askTestButton(t, f, "Go")
	before := f.messageCount()
	askTestEvent(t, w, map[string]any{"id": "first", "method": "ask", "questions": []any{question}})
	if f.messageCount() != before || len(askTestState(t, w)) != 0 {
		t.Fatal("duplicate request redisplayed or answered Ask")
	}
	nativeInput(t, w, 200, "/followup do this later", false)
	researchBarrier(t, w)
	if w.ask == nil || queueState(t, w, 200) != "pending" {
		t.Fatal("ordinary followup cancelled Ask")
	}
	askTestEvent(t, w, map[string]any{"id": "concurrent", "method": "ask", "questions": []any{question}})
	responses := askTestState(t, w)
	if len(responses) != 1 || responses[0].ID != "concurrent" || !responses[0].Cancelled || w.ask == nil {
		t.Fatalf("concurrent request replaced active Ask: %+v", responses)
	}
	askTestCallback(w, 7, choice)
	researchBarrier(t, w)
	responses = askTestState(t, w)
	if len(responses) != 2 || responses[1].ID != "first" || responses[1].Cancelled {
		t.Fatalf("original Ask lost its answer: %+v", responses)
	}
	askTestEvent(t, w, map[string]any{"id": "first", "method": "ask", "questions": []any{question}})
	if w.ask != nil || len(askTestState(t, w)) != 2 {
		t.Fatal("completed duplicate reopened Ask")
	}
}

func TestAskCancellationAndNativeTimeoutInvalidateReplies(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_PENDING_STEER", "hold")
	for _, cancellation := range []string{"user", "native", "timeout"} {
		t.Run(cancellation, func(t *testing.T) {
			w, f, trace := askTestWorker(t)
			askTestToolEvent(t, w, "tool_execution_start", "cancelled-ask-tool")
			askTestOpen(t, w, "cancel-me", askTestQuestion("topic", "Topic?", false, "Go"))
			stale := askTestButton(t, f, "Go")
			askTestClick(t, w, f, "Other")
			prompt := askTestPrompt(t, f)
			nativeInput(t, w, 210, "cancelled-dialog guidance", false)
			askTestNoGuidance(t, w, trace, 210)
			if cancellation == "user" {
				askTestClick(t, w, f, "Cancel")
			} else {
				askTestEvent(t, w, map[string]any{"method": "cancel", "targetId": "cancel-me", "timedOut": cancellation == "timeout"})
			}
			if w.ask != nil {
				t.Fatal("cancelled native Ask remained active")
			}
			askTestCallback(w, 7, stale)
			askTestReply(t, w, 200, 7, 11, "late-answer", prompt)
			responses := askTestState(t, w)
			if cancellation == "user" {
				if len(responses) != 1 || !responses[0].Cancelled || responses[0].ID != "cancel-me" {
					t.Fatalf("user cancellation = %+v", responses)
				}
			} else if len(responses) != 0 {
				t.Fatalf("native cancellation answered already-resolved request: %+v", responses)
			}
			askTestNoGuidance(t, w, trace, 210)
			askTestToolEvent(t, w, "tool_execution_end", "cancelled-ask-tool")
			nativeUntil(t, w, func() bool {
				return len(w.deferredSteers) == 0 && !w.rpcOperationActive && len(w.rpcOperations) == 0
			})
			if researchTraceCount(trace, "prompt", "cancelled-dialog guidance") != 1 || w.active != 100 || queueState(t, w, 210) != "submitted" {
				t.Fatal("native cancellation/timeout released guidance before tool end or lost its original root")
			}
		})
	}
}

func TestAskFallbackEditorDoesNotAutoacceptApproval(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_REJECT_RPC", "set_ask_dialog")
	w, f, trace := askTestWorker(t)
	askTestEvent(t, w, map[string]any{"id": "fallback-choice", "method": "select", "title": "Which language?", "options": []string{"Go", "Other (type your own)"}})
	askTestClick(t, w, f, "Other")
	askTestEvent(t, w, map[string]any{"id": "fallback-editor", "method": "editor", "title": "Which language?", "promptStyle": true})
	prompt := askTestPrompt(t, f)
	askTestReply(t, w, 200, 7, 11, "  Elixir  ", prompt)
	responses := askTestState(t, w)
	if len(responses) != 2 || responses[0].Value != "Other (type your own)" || responses[1].ID != "fallback-editor" || responses[1].Value != "Elixir" {
		t.Fatalf("fallback responses = %+v", responses)
	}
	askTestEvent(t, w, map[string]any{"id": "approval", "method": "confirm", "title": "Approve dangerous operation?"})
	if len(askTestState(t, w)) != 2 {
		t.Fatal("approval was autoaccepted")
	}
	askTestClick(t, w, f, "Cancel")
	responses = askTestState(t, w)
	if len(responses) != 3 || responses[2].Confirmed == nil || *responses[2].Confirmed {
		t.Fatalf("explicit denial = %+v", responses)
	}
	for _, entry := range fixtureRPCTrace(trace) {
		if (entry.kind == "prompt" || entry.kind == "steer") && strings.Contains(entry.payload, "Elixir") {
			t.Fatal("fallback editor submitted a new model input")
		}
	}
}

func TestAskLifecycleRetiresButtonsAndText(t *testing.T) {
	for _, boundary := range []string{"stop", "root", "session", "runtime"} {
		t.Run(boundary, func(t *testing.T) {
			w, f, trace := askTestWorker(t)
			askTestToolEvent(t, w, "tool_execution_start", "old-lifecycle-tool")
			askTestOpen(t, w, "old", askTestQuestion("topic", "Old topic?", false, "Go"))
			stale := askTestButton(t, f, "Go")
			askTestClick(t, w, f, "Other")
			prompt := askTestPrompt(t, f)
			nativeInput(t, w, 210, "retired-root guidance", false)
			askTestNoGuidance(t, w, trace, 210)
			switch boundary {
			case "stop":
				nativeInput(t, w, 200, "/stop", false)
				nativeUntil(t, w, func() bool { return w.client == nil && !w.controlInProgress() })
				nativeInput(t, w, 201, "wait", false)
				nativeCatalogReady(t, w)
				researchBarrier(t, w)
			case "root":
				nativeRPC(t, w, "fixture_finish_root", nil)
				nativeUntil(t, w, func() bool { return !w.taskActive() })
				nativeInput(t, w, 201, "wait", false)
				researchBarrier(t, w)
			case "session", "runtime":
				if boundary == "runtime" {
					w.client.Close()
					w.failed()
				} else {
					askTestEvent(t, w, map[string]any{"method": "cancel", "targetId": "old"})
				}
				nativeInput(t, w, 200, "/stop", false)
				nativeUntil(t, w, func() bool { return w.client == nil && !w.controlInProgress() })
				before := w.binding.Generation
				nativeInput(t, w, 201, "/new "+t.TempDir(), false)
				drainControlOperations(t, w)
				buttons := resumeButtons(t, f)
				clickKeyboard(w, 7, buttons[0]["callback_data"].(string))
				if w.binding.Generation <= before {
					t.Fatal("session replacement did not advance generation")
				}
				nativeCatalogReady(t, w)
				nativeInput(t, w, 202, "wait", false)
				researchBarrier(t, w)
			}
			nativeUntil(t, w, func() bool { return w.askSending == 0 })
			if queueState(t, w, 210) != "cancelled" || len(w.deferredSteers) != 0 || researchTraceCount(trace, "prompt", "retired-root guidance") != 0 {
				t.Fatal("lifecycle boundary retained or dispatched old-root guidance")
			}
			askTestOpen(t, w, "fresh", askTestQuestion("topic", "Fresh topic?", false, "Rust"))
			askTestToolEvent(t, w, "tool_execution_end", "old-lifecycle-tool")
			askTestEvent(t, w, map[string]any{"method": "cancel", "targetId": "old", "timedOut": true})
			askTestCallback(w, 7, stale)
			askTestReply(t, w, 220, 7, 11, "late-lifecycle-canary", prompt)
			if w.ask == nil {
				t.Fatal("old interaction resolved a new Ask")
			}
			askTestClick(t, w, f, "Rust")
			responses := askTestState(t, w)
			last := responses[len(responses)-1]
			if last.ID != "fresh" || len(last.Answers) != 1 || strings.Join(last.Answers[0].Selected, ",") != "Rust" {
				t.Fatalf("replacement Ask = %+v", responses)
			}
			if queueState(t, w, 210) != "cancelled" || len(w.deferredSteers) != 0 || researchTraceCount(trace, "prompt", "retired-root guidance") != 0 {
				t.Fatal("late old-task events attached guidance to the replacement root")
			}
			for _, entry := range fixtureRPCTrace(trace) {
				if (entry.kind == "prompt" || entry.kind == "steer") && strings.Contains(entry.payload, "late-lifecycle-canary") {
					t.Fatal("retired text prompt became a new model input")
				}
			}
		})
	}
}

func TestAskEditorRejectsSecretInput(t *testing.T) {
	w, _, _ := askTestWorker(t)
	askTestEvent(t, w, map[string]any{"id": "secret", "method": "input", "title": "Password", "secret": true})
	responses := askTestState(t, w)
	if w.ask != nil || len(responses) != 1 || responses[0].ID != "secret" || !responses[0].Cancelled {
		t.Fatalf("secret input did not fail closed: %+v", responses)
	}
}

func TestAskLateDisplayResultCannotRetireNewInteraction(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			entered, release := make(chan struct{}), make(chan struct{})
			released := false
			t.Cleanup(func() {
				if !released {
					close(release)
				}
			})
			response := fakeHTTPResponse{status: 200, body: `{"ok":true,"result":{"message_id":500}}`, onClose: func() { close(entered); <-release }}
			if failed {
				response.status, response.body = 400, `{"ok":false,"error_code":400,"description":"late Ask display failure"}`
			}
			f.mu.Lock()
			f.sendResponses = map[int64][]fakeHTTPResponse{-10: {response}}
			f.mu.Unlock()
			nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
				"type": "extension_ui_request", "id": "old-display", "method": "ask", "questions": []any{askTestQuestion("old", "Old question?", false, "Go")},
			}})
			researchEventBarrier(t, w)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("Ask display did not reach delayed response")
			}
			askTestEvent(t, w, map[string]any{"method": "cancel", "targetId": "old-display"})
			askTestOpen(t, w, "new-display", askTestQuestion("new", "New question?", false, "Rust"))
			client := w.client
			close(release)
			released = true
			w.operationReturned(waitOperation(t, w))
			if w.client != client || w.ask == nil {
				t.Fatal("late old display result retired a current interaction")
			}
			askTestClick(t, w, f, "Rust")
			responses := askTestState(t, w)
			if len(responses) != 1 || responses[0].ID != "new-display" {
				t.Fatalf("late display produced another answer: %+v", responses)
			}
		})
	}
}

func TestAskResponseSendFailureRetiresRuntimeWithoutRetry(t *testing.T) {
	w, f, trace := askTestWorker(t)
	askTestOpen(t, w, "send-failure", askTestQuestion("topic", "Topic?", false, "Go"))
	choice := askTestButton(t, f, "Go")
	w.client.Close()
	askTestCallback(w, 7, choice)
	nativeUntil(t, w, func() bool { return w.client == nil && w.askSending == 0 })
	if w.ask != nil || queueState(t, w, 100) != "uncertain" {
		t.Fatal("unconfirmed Ask response retained its runtime or falsely completed the root")
	}
	askTestCallback(w, 7, choice)
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "extension_ui_response" && strings.Contains(entry.payload, "send-failure") {
			t.Fatal("closed native runtime received or replayed an Ask response")
		}
	}
}

func askTestToolEvent(t *testing.T, w *worker, kind, id string) {
	t.Helper()
	nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
		"type": kind, "toolCallId": id, "toolName": "ask",
	}})
	researchEventBarrier(t, w)
	w.dispatch()
}

func askTestNoGuidance(t *testing.T, w *worker, trace string, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if queueState(t, w, id) != "pending" {
			t.Fatalf("held guidance %d is not pending", id)
		}
	}
	if len(w.queue) != 0 || len(w.steers) != 0 {
		t.Fatal("held guidance became a followup or native steer")
	}
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && strings.Contains(entry.payload, "guidance") {
			t.Fatalf("native received guidance before Ask completed: %q", entry.payload)
		}
	}
}

func TestAskBufferedNativeEventsHoldInitialGuidance(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_PENDING_STEER", "hold")
	w, f, trace := askTestWorker(t)
	client, root, turn := w.client, w.rootRequestID, w.turn
	generation, session, epoch := w.binding.Generation, w.sessionID, client.CommandCatalog().Epoch
	nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
		"type": "tool_execution_start", "toolCallId": "buffered-ask-tool", "toolName": "ask",
	}})
	nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
		"type": "extension_ui_request", "id": "buffered-ask", "method": "ask",
		"questions": []any{askTestQuestion("topic", "Topic?", false, "Go")},
	}})
	if w.askBlocksSteering() || !client.BufferedOutput() || w.rpcOperationActive || len(w.rpcOperations) != 0 {
		t.Fatal("fixture did not leave native Ask events ahead of worker state")
	}
	nativeInput(t, w, 200, "reader-ahead guidance", false)
	askTestNoGuidance(t, w, trace, 200)
	if len(w.deferredSteers) != 1 || w.client != client || w.rootRequestID != root || w.turn != turn || w.active != 100 {
		t.Fatal("initial guidance bypassed buffered Ask or changed its root")
	}
	held := w.deferredSteers[0]
	if held.owner.rootID != 100 || held.owner.generation != generation || held.owner.turn != turn || held.owner.clientID != client.ID() ||
		held.session != session || held.epoch != epoch {
		t.Fatal("reader-ahead guidance did not capture its original immutable scope")
	}
	nativeUntil(t, w, func() bool { return w.ask != nil && w.ask.cancel == nil })
	askTestNoGuidance(t, w, trace, 200)
	askTestClick(t, w, f, "Go")
	responses := askTestState(t, w)
	if len(responses) != 1 || responses[0].ID != "buffered-ask" || responses[0].Cancelled ||
		len(responses[0].Answers) != 1 || strings.Join(responses[0].Answers[0].Selected, ",") != "Go" {
		t.Fatalf("buffered Ask lost its explicit answer: %+v", responses)
	}
	askTestNoGuidance(t, w, trace, 200)
	askTestToolEvent(t, w, "tool_execution_end", "buffered-ask-tool")
	// Wait for the actual prompt RPC acknowledgment, not its durable admission.
	nativeUntil(t, w, func() bool {
		return len(w.deferredSteers) == 0 && !w.rpcOperationActive && len(w.rpcOperations) == 0
	})
	if researchTraceCount(trace, "prompt", "reader-ahead guidance") != 1 || queueState(t, w, 200) != "submitted" ||
		len(w.steers) != 1 || len(w.queue) != 0 || w.client != client || w.rootRequestID != root || w.turn != turn ||
		w.active != 100 || w.binding.Generation != generation || w.sessionID != session {
		t.Fatal("buffered guidance did not dispatch exactly once on its original root after Ask ended")
	}
	for _, steer := range w.steers {
		if steer.inboxID != 200 || steer.rootID != held.owner.rootID || steer.generation != held.owner.generation ||
			steer.turn != held.owner.turn || steer.clientID != held.owner.clientID {
			t.Fatal("released reader-ahead guidance changed ownership")
		}
	}
}

func TestAskBufferedTerminalCancelsInitialGuidance(t *testing.T) {
	w, _, trace := askTestWorker(t)
	client, root, turn := w.client, w.rootRequestID, w.turn
	nativeRPC(t, w, "fixture_finish_root", nil)
	if !client.BufferedOutput() || w.askBlocksSteering() || w.rpcOperationActive || len(w.rpcOperations) != 0 {
		t.Fatal("fixture did not leave a terminal event ahead of worker state")
	}
	nativeInput(t, w, 200, "terminal-buffered guidance", false)
	askTestNoGuidance(t, w, trace, 200)
	if len(w.deferredSteers) != 1 || w.client != client || w.rootRequestID != root || w.turn != turn || w.active != 100 {
		t.Fatal("initial guidance bypassed buffered terminal or became a new root")
	}
	nativeUntil(t, w, func() bool { return !w.taskActive() && !w.rpcOperationActive && len(w.rpcOperations) == 0 })
	if queueState(t, w, 100) != "done" || queueState(t, w, 200) != "cancelled" || w.active != 0 ||
		len(w.deferredSteers) != 0 || len(w.steers) != 0 || len(w.queue) != 0 ||
		researchTraceCount(trace, "prompt", "terminal-buffered guidance") != 0 || researchTraceCount(trace, "prompt", "wait") != 1 {
		t.Fatal("buffered terminal submitted, replayed, or transferred guidance to another root")
	}
}

func TestAskOrdinaryInputWaitsForResponseAndNativeToolEnd(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_PENDING_STEER", "hold")
	for _, first := range []string{"response", "tool-end"} {
		t.Run(first, func(t *testing.T) {
			w, f, trace := askTestWorker(t)
			askTestToolEvent(t, w, "tool_execution_start", "held-ask")
			askTestOpen(t, w, "preserved",
				askTestQuestion("topic", "Topic?", false, "Programming"),
				askTestQuestion("features", "Features?", true, "Auth", "Search"),
				askTestQuestion("language", "Language?", false, "Rust"))
			askTestClick(t, w, f, "Programming")
			askTestClick(t, w, f, "Auth")
			a, epoch, token, message := w.ask, w.ask.epoch, w.ask.token, w.ask.messageID
			client, turn, root := w.client, w.turn, w.rootRequestID
			nativeInput(t, w, 200, "ordinary guidance", false)
			researchBarrier(t, w)
			askTestNoGuidance(t, w, trace, 200)
			if w.ask != a || a.question != 1 || a.epoch != epoch || a.token != token || a.messageID != message ||
				len(a.answers) != 1 || strings.Join(a.answers[0].SelectedOptions, ",") != "Programming" || !a.selected[0] || len(a.selected) != 1 ||
				w.client != client || w.turn != turn || w.rootRequestID != root || w.active != 100 || !w.busy || len(askTestState(t, w)) != 0 {
				t.Fatal("ordinary guidance changed Ask UI, partial answers, or original ownership")
			}
			askTestClick(t, w, f, "Confirm selection")
			askTestCallback(w, 7, askTestButton(t, f, "Rust"))
			result := waitOperation(t, w)
			if result.kind != "ask_response" || result.err != nil || w.ask != nil || w.askSending != 1 {
				t.Fatalf("expected unapplied Ask send result, got %s: %v", result.kind, result.err)
			}
			if first == "response" {
				w.operationReturned(result)
				w.dispatch()
				researchBarrier(t, w)
				askTestNoGuidance(t, w, trace, 200)
				askTestToolEvent(t, w, "tool_execution_end", "held-ask")
			} else {
				askTestToolEvent(t, w, "tool_execution_end", "held-ask")
				askTestNoGuidance(t, w, trace, 200)
				w.operationReturned(result)
				w.dispatch()
			}
			researchBarrier(t, w)
			nativeUntil(t, w, func() bool {
				return len(w.deferredSteers) == 0 && !w.rpcOperationActive && len(w.rpcOperations) == 0
			})
			responses := askTestState(t, w)
			if len(responses) != 1 || responses[0].ID != "preserved" || responses[0].Cancelled || len(responses[0].Answers) != 3 ||
				strings.Join(responses[0].Answers[1].Selected, ",") != "Auth" || strings.Join(responses[0].Answers[2].Selected, ",") != "Rust" {
				t.Fatalf("preserved Ask answers = %+v", responses)
			}
			if researchTraceCount(trace, "prompt", "ordinary guidance") != 1 || queueState(t, w, 200) != "submitted" ||
				len(w.steers) != 1 || len(w.deferredSteers) != 0 || len(w.queue) != 0 ||
				w.client != client || w.turn != turn || w.rootRequestID != root || w.active != 100 {
				t.Fatal("guidance did not dispatch exactly once within its original root")
			}
			for _, steer := range w.steers {
				if steer.rootID != 100 || steer.turn != turn || steer.clientID != client.ID() || steer.generation != w.binding.Generation {
					t.Fatal("released guidance acquired different ownership")
				}
			}
		})
	}
}

func TestAskDeferredGuidanceFIFOAndSharedCapacity(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_PENDING_STEER", "hold")
	w, f, trace := askTestWorker(t)
	w.b.cfg.QueueCapacity = 3
	askTestToolEvent(t, w, "tool_execution_start", "fifo-ask")
	askTestOpen(t, w, "fifo", askTestQuestion("topic", "Topic?", false, "Go"))
	nativeInput(t, w, 200, "first guidance", false)
	nativeInput(t, w, 201, "second guidance", false)
	nativeInput(t, w, 202, "/followup later work", false)
	nativeInput(t, w, 203, "overflow guidance", false)
	nativeInput(t, w, 204, "/followup overflow work", false)
	researchBarrier(t, w)
	if len(w.deferredSteers) != 2 || len(w.queue) != 1 || len(w.steers) != 0 ||
		queueState(t, w, 200) != "pending" || queueState(t, w, 201) != "pending" || queueState(t, w, 202) != "pending" ||
		queueState(t, w, 203) != "cancelled" || queueState(t, w, 204) != "cancelled" {
		t.Fatal("deferred guidance and followups did not share queue capacity")
	}
	askTestClick(t, w, f, "Go")
	askTestToolEvent(t, w, "tool_execution_end", "fifo-ask")
	nativeUntil(t, w, func() bool {
		return len(w.deferredSteers) == 0 && !w.rpcOperationActive && len(w.rpcOperations) == 0
	})
	var guidance []string
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && strings.Contains(entry.payload, "guidance") {
			guidance = append(guidance, entry.payload)
		}
	}
	if strings.Join(guidance, "|") != "first guidance|second guidance" || len(w.deferredSteers) != 0 || len(w.steers) != 2 ||
		w.active != 100 || queueState(t, w, 202) != "pending" || researchTraceCount(trace, "prompt", "later work") != 0 {
		t.Fatalf("guidance lost FIFO/original-root ownership or replayed followup: %v", guidance)
	}
	nativeInput(t, w, 205, "inflight overflow guidance", false)
	if queueState(t, w, 205) != "cancelled" {
		t.Fatal("inflight guidance stopped counting toward shared queue capacity")
	}
}

func TestAskDeferredGuidanceCancelledBeforeFinalMetadata(t *testing.T) {
	w, _, _, trace := replyStatusWorker(t)
	askTestToolEvent(t, w, "tool_execution_start", "metadata-ask")
	askTestOpen(t, w, "metadata", askTestQuestion("topic", "Topic?", false, "Go"))
	nativeInput(t, w, 210, "finished-root guidance", false)
	askTestNoGuidance(t, w, trace, 210)
	nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "get_state"})
	replyStatusMessage(t, w, 100, 1000, 1100)
	endReplyStatus(w)
	waitFixtureRPCTrace(t, trace, "research_control_waiting", "get_state")
	if w.replyStatus.pending == nil || queueState(t, w, 2) != "submitted" ||
		queueState(t, w, 210) != "cancelled" || len(w.deferredSteers) != 0 {
		t.Fatal("finished-root guidance survived into asynchronous final metadata sampling")
	}
	nativeRPC(t, w, "fixture_research_control_release", nil)
	nativeUntil(t, w, func() bool { return !w.taskActive() })
	if researchBody(t, w, 2) != "answer" || researchTraceCount(trace, "prompt", "finished-root guidance") != 0 {
		t.Fatal("final sampling changed the answer or submitted finished-root guidance")
	}
}

func TestAskDeferredGuidanceRejectsReplacedScope(t *testing.T) {
	for _, boundary := range []string{"generation", "turn", "native-session", "catalog-epoch"} {
		t.Run(boundary, func(t *testing.T) {
			w, _, trace := askTestWorker(t)
			askTestToolEvent(t, w, "tool_execution_start", "old-scope")
			askTestOpen(t, w, "old-scope", askTestQuestion("topic", "Topic?", false, "Go"))
			nativeInput(t, w, 200, "obsolete guidance", false)
			askTestNoGuidance(t, w, trace, 200)
			switch boundary {
			case "generation":
				w.binding.Generation++
			case "turn":
				w.turn++
			case "native-session":
				nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
					"type": "session_info_update", "sessionId": "replacement-native-session",
				}})
				researchBarrier(t, w)
			case "catalog-epoch":
				w.client.InvalidateCommandCatalog()
			}
			w.dispatch()
			if queueState(t, w, 200) != "cancelled" || len(w.deferredSteers) != 0 || len(w.steers) != 0 || len(w.queue) != 0 {
				t.Fatal("stale guidance survived its immutable owner scope")
			}
			askTestToolEvent(t, w, "tool_execution_end", "old-scope")
			if researchTraceCount(trace, "prompt", "obsolete guidance") != 0 {
				t.Fatal("late native completion replayed stale guidance")
			}
		})
	}
}

func TestAskGuidanceCannotAdoptReaderAheadSession(t *testing.T) {
	w, _, trace := askTestWorker(t)
	askTestToolEvent(t, w, "tool_execution_start", "reader-ahead-tool")
	askTestOpen(t, w, "reader-ahead", askTestQuestion("topic", "Topic?", false, "Go"))
	client, root, turn, session := w.client, w.rootRequestID, w.turn, w.sessionID
	nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
		"type": "session_info_update", "sessionId": "reader-ahead-replacement",
	}})
	if client.CommandCatalog().SessionID != "reader-ahead-replacement" || w.sessionID != session {
		t.Fatal("fixture did not establish reader-ahead session identity")
	}
	nativeInput(t, w, 200, "reader-ahead guidance", false)
	researchBarrier(t, w)
	if queueState(t, w, 200) != "cancelled" || len(w.deferredSteers) != 0 || len(w.steers) != 0 || len(w.queue) != 0 ||
		w.client != client || w.rootRequestID != root || w.turn != turn || w.active != 100 || researchTraceCount(trace, "prompt", "reader-ahead guidance") != 0 {
		t.Fatal("guidance admitted against an old root adopted the reader's replacement session")
	}
	askTestToolEvent(t, w, "tool_execution_end", "reader-ahead-tool")
	if researchTraceCount(trace, "prompt", "reader-ahead guidance") != 0 {
		t.Fatal("late tool completion released reader-ahead guidance")
	}
}

func TestAskFallbackEditorCancellationResolvesOnce(t *testing.T) {
	w, f, trace := askTestWorker(t)
	askTestEvent(t, w, map[string]any{"id": "editor-cancel", "method": "editor", "title": "Explain your choice", "promptStyle": true})
	prompt := askTestPrompt(t, f)
	askTestClick(t, w, f, "Cancel")
	askTestReply(t, w, 200, 7, 11, "cancelled-editor-canary", prompt)
	responses := askTestState(t, w)
	if len(responses) != 1 || responses[0].ID != "editor-cancel" || !responses[0].Cancelled {
		t.Fatalf("editor cancellation = %+v", responses)
	}
	for _, entry := range fixtureRPCTrace(trace) {
		if (entry.kind == "prompt" || entry.kind == "steer") && strings.Contains(entry.payload, "cancelled-editor-canary") {
			t.Fatal("cancelled editor reply became a model input")
		}
	}
}

func TestAskFastTextReplyWaitsForExactTelegramPromptID(t *testing.T) {
	w, f, _ := askTestWorker(t)
	askTestOpen(t, w, "fast-reply", askTestQuestion("topic", "Which topic?", false, "Go"))
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	f.mu.Lock()
	f.sendResponses = map[int64][]fakeHTTPResponse{-10: {{status: 200, body: `{"ok":true,"result":{"message_id":700}}`, onClose: func() { close(entered); <-release }}}}
	f.mu.Unlock()
	askTestCallback(w, 7, askTestButton(t, f, "Custom answer"))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("text prompt did not reach delayed display completion")
	}
	prompt := askTestPrompt(t, f)
	prompt.MessageID = 700
	u := update(200, 11, "fast-answer")
	u.Message.ReplyToMessage = prompt
	raw, err := json.Marshal(u)
	requireStoreOK(t, err)
	requireStoreOK(t, w.b.db.Accept(200, raw))
	w.handle(incoming{id: 200, msg: u.Message})
	w.dispatch()
	state, err := w.call("get_state", nil)
	requireStoreOK(t, err)
	var before struct {
		Responses []askTestResponse `json:"fixtureUIResponses"`
	}
	requireStoreOK(t, json.Unmarshal(state, &before))
	if len(before.Responses) != 0 {
		t.Fatal("answer was accepted before confirming Telegram prompt identity")
	}
	close(release)
	released = true
	askTestSettle(t, w)
	responses := askTestState(t, w)
	if len(responses) != 1 || len(responses[0].Answers) != 1 || responses[0].Answers[0].Custom != "fast-answer" || queueState(t, w, 200) != "done" {
		t.Fatalf("fast valid text reply was dropped or misrouted: %+v", responses)
	}
}

func TestAskBackRevokesCustomPrompt(t *testing.T) {
	w, f, _ := askTestWorker(t)
	askTestOpen(t, w, "back", askTestQuestion("first", "First topic?", false, "Go"), askTestQuestion("second", "Second topic?", false, "Rust"))
	askTestClick(t, w, f, "Other")
	prompt := askTestPrompt(t, f)
	askTestClick(t, w, f, "Back")
	askTestReply(t, w, 200, 7, 11, "old-custom-answer", prompt)
	if w.ask == nil || len(askTestState(t, w)) != 0 {
		t.Fatal("revoked prompt advanced the current question")
	}
	askTestClick(t, w, f, "Go")
	askTestReply(t, w, 201, 7, 11, "old-custom-answer", prompt)
	if len(askTestState(t, w)) != 0 {
		t.Fatal("old custom prompt answered a later question")
	}
	askTestClick(t, w, f, "Rust")
	responses := askTestState(t, w)
	if len(responses) != 1 || len(responses[0].Answers) != 2 || responses[0].Answers[0].Custom != "" || responses[0].Answers[1].Custom != "" {
		t.Fatalf("revoked text leaked into answers: %+v", responses)
	}
}

func TestAskCurrentDisplayFailureCancelsNativeRequest(t *testing.T) {
	w, f, _ := askTestWorker(t)
	client := w.client
	f.mu.Lock()
	f.failProgress = true
	f.mu.Unlock()
	askTestEvent(t, w, map[string]any{"id": "display-failed", "method": "ask", "questions": []any{askTestQuestion("topic", "Topic?", false, "Go")}})
	f.mu.Lock()
	f.failProgress = false
	f.mu.Unlock()
	responses := askTestState(t, w)
	if w.ask != nil || w.client != client || len(responses) != 1 || responses[0].ID != "display-failed" || !responses[0].Cancelled {
		t.Fatalf("failed Telegram display left native Ask waiting: %+v", responses)
	}
}

func TestAskCannotStealExistingNativeSelectionOrApproval(t *testing.T) {
	for _, method := range []string{"select", "confirm"} {
		t.Run(method, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			askTestEvent(t, w, map[string]any{"id": "existing", "method": method, "title": "Explicit decision", "options": []string{"Allow", "Deny"}})
			label := "Allow"
			if method == "confirm" {
				label = "Confirm"
			}
			original := askTestButton(t, f, label)
			askTestEvent(t, w, map[string]any{"id": "intruder", "method": "ask", "questions": []any{askTestQuestion("topic", "Topic?", false, "Go")}})
			responses := askTestState(t, w)
			if w.ask != nil || len(responses) != 1 || responses[0].ID != "intruder" || !responses[0].Cancelled {
				t.Fatalf("Ask stole an existing native decision: %+v", responses)
			}
			askTestCallback(w, 7, original)
			responses = askTestState(t, w)
			if len(responses) != 2 || responses[1].ID != "existing" || responses[1].Cancelled {
				t.Fatalf("original native decision stopped working: %+v", responses)
			}
			if method == "confirm" && (responses[1].Confirmed == nil || !*responses[1].Confirmed) {
				t.Fatal("explicit approval did not require its own callback")
			}
		})
	}
}

func TestAskLateResponseFailureCannotRetireReplacementRuntime(t *testing.T) {
	w, f, _ := askTestWorker(t)
	askTestOpen(t, w, "late-response", askTestQuestion("topic", "Topic?", false, "Go"))
	oldClient := w.client
	choice := askTestButton(t, f, "Go")
	oldClient.Close()
	askTestCallback(w, 7, choice)
	response := waitOperation(t, w)
	if response.kind != "ask_response" || response.err == nil {
		t.Fatalf("closed native send did not produce a response failure: %s, %v", response.kind, response.err)
	}
	nativeInput(t, w, 200, "/stop", false)
	drainControlOperations(t, w)
	nativeInput(t, w, 201, "/new "+t.TempDir(), false)
	drainControlOperations(t, w)
	buttons := resumeButtons(t, f)
	clickKeyboard(w, 7, buttons[0]["callback_data"].(string))
	nativeCatalogReady(t, w)
	replacement := w.client
	if replacement == nil || replacement == oldClient {
		t.Fatal("fixture did not replace the native runtime")
	}
	w.operationReturned(response)
	if w.client != replacement {
		t.Fatal("late failed send to old client retired the replacement runtime")
	}
	nativeInput(t, w, 202, "wait", false)
	researchBarrier(t, w)
	askTestOpen(t, w, "replacement-ask", askTestQuestion("topic", "New topic?", false, "Rust"))
	askTestClick(t, w, f, "Rust")
	responses := askTestState(t, w)
	if len(responses) != 1 || responses[0].ID != "replacement-ask" {
		t.Fatalf("replacement runtime replayed the old answer: %+v", responses)
	}
}

func TestAskRootlessStopRetiresPendingDialog(t *testing.T) {
	w, _, _ := askTestWorker(t)
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return !w.taskActive() })
	askTestEvent(t, w, map[string]any{"id": "rootless-editor", "method": "editor", "title": "Idle question"})
	if w.ask == nil || w.ask.inputID == 0 {
		t.Fatal("connected rootless text dialog was rejected")
	}
	nativeInput(t, w, 200, "/stop", false)
	nativeUntil(t, w, func() bool { return !w.controlInProgress() })
	if w.ask != nil || w.client != nil {
		t.Fatal("Stop left a rootless dialog blocking input and idle release")
	}
}

func TestAskQueuedEventCannotAdoptReaderSession(t *testing.T) {
	w, _, _ := askTestWorker(t)
	client, turn := w.client, w.turn
	nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
		"type": "extension_ui_request", "id": "old-session-ask", "method": "ask",
		"questions": []any{askTestQuestion("topic", "Old session?", false, "Go")},
	}})
	nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
		"type": "session_info_update", "sessionId": "next-native-session",
	}})
	if client.CommandCatalog().SessionID != "next-native-session" {
		t.Fatal("reader did not publish the replacement session before the actor")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case raw := <-client.Events():
			var event rpcEvent
			requireStoreOK(t, json.Unmarshal(raw, &event))
			w.event(raw)
			if event.ID != "old-session-ask" {
				continue
			}
			if w.ask != nil || w.client != client || w.turn != turn || w.active != 100 {
				t.Fatal("queued Ask adopted the reader's replacement session or changed its task")
			}
			return
		case <-deadline.C:
			t.Fatal("queued old-session Ask was not delivered")
		}
	}
}

func TestAskRejectedStopCannotStrandInput(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_REJECT_RPC", "abort")
	for _, rootless := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "rootless"}[rootless], func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			if rootless {
				nativeRPC(t, w, "fixture_finish_root", nil)
				nativeUntil(t, w, func() bool { return !w.taskActive() })
			}
			askTestOpen(t, w, "rejected-stop", askTestQuestion("topic", "Topic?", false, "Go"))
			askTestClick(t, w, f, "Other")
			prompt := askTestPrompt(t, f)
			nativeInput(t, w, 200, "/stop", false)
			nativeUntil(t, w, func() bool { return !w.controlInProgress() })
			if w.ask != nil || w.client != nil {
				t.Fatal("rejected Stop left revoked input blocking the native dialog")
			}
			askTestReply(t, w, 201, 7, 11, "late rejected-stop answer", prompt)
			if queueState(t, w, 201) != "cancelled" {
				t.Fatal("revoked text reply was submitted after failed Stop")
			}
			if !rootless && queueState(t, w, 100) != "uncertain" {
				t.Fatal("failed Stop claimed a confirmed root outcome")
			}
		})
	}
}

func TestAskStopBypassesExpiredReplyPrompt(t *testing.T) {
	w, f, _ := askTestWorker(t)
	askTestOpen(t, w, "expired-stop", askTestQuestion("topic", "Topic?", false, "Go"))
	askTestClick(t, w, f, "Other")
	prompt := askTestPrompt(t, f)
	askTestClick(t, w, f, "Back")
	u := update(200, 11, "/stop")
	u.Message.ReplyToMessage = prompt
	raw, err := json.Marshal(u)
	requireStoreOK(t, err)
	requireStoreOK(t, w.b.db.Accept(200, raw))
	w.handle(incoming{id: 200, msg: u.Message})
	w.dispatch()
	nativeUntil(t, w, func() bool { return w.client == nil && !w.controlInProgress() })
	if w.ask != nil || w.taskActive() || queueState(t, w, 200) != "done" {
		t.Fatal("quoted Stop was consumed as an expired Ask answer")
	}
}

func TestAskResearchOffBypassesReplyPrompt(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "expired"}[expired], func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			f := http.DefaultTransport.(*fakeHTTP)
			w.b.cfg.ProgressMode = "off"
			researchRoot(t, w, 100, "/autoresearch improve throughput")
			askTestOpen(t, w, "research-off", askTestQuestion("topic", "Topic?", false, "Go"))
			askTestClick(t, w, f, "Other")
			prompt := askTestPrompt(t, f)
			if expired {
				askTestClick(t, w, f, "Back")
			}
			askTestReply(t, w, 200, 7, 11, "/autoresearch off", prompt)
			nativeUntil(t, w, func() bool { return queueState(t, w, 200) == "done" })
			if w.ask != nil || w.taskActive() || w.research.root || w.research.enabled || w.research.disablePending ||
				queueState(t, w, 100) != "uncertain" || researchTraceCount(trace, "prompt", "/autoresearch off") != 1 {
				t.Fatal("quoted research off did not interrupt research and confirm mode-off")
			}
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind != "extension_ui_response" {
					continue
				}
				var response askTestResponse
				requireStoreOK(t, json.Unmarshal([]byte(entry.payload), &response))
				if !response.Cancelled {
					t.Fatalf("research off was submitted as a native answer: %+v", response)
				}
			}
		})
	}
}

func TestAskForeignCommandDoesNotAnswer(t *testing.T) {
	w, f, _ := askTestWorker(t)
	askTestOpen(t, w, "foreign", askTestQuestion("topic", "Topic?", false, "Go"))
	askTestClick(t, w, f, "Other")
	a, epoch := w.ask, w.ask.epoch
	askTestReply(t, w, 200, 7, 11, "/status@OtherBot", askTestPrompt(t, f))
	if w.ask != a || a.epoch != epoch || !a.input || a.custom != "" || queueState(t, w, 200) != "ignored" || len(askTestState(t, w)) != 0 {
		t.Fatal("foreign-bot command changed or answered the current Ask")
	}
}

func TestAskCommandsDoNotWaitForPromptDelivery(t *testing.T) {
	for _, command := range []string{"/stop", "/status@OtherBot"} {
		t.Run(command, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			askTestOpen(t, w, "pending-command", askTestQuestion("topic", "Topic?", false, "Go"))
			entered, release := make(chan struct{}), make(chan struct{})
			released := false
			t.Cleanup(func() {
				if !released {
					close(release)
				}
			})
			f.mu.Lock()
			f.sendResponses = map[int64][]fakeHTTPResponse{-10: {{status: 200, body: `{"ok":true,"result":{"message_id":700}}`, onClose: func() { close(entered); <-release }}}}
			f.mu.Unlock()
			askTestCallback(w, 7, askTestButton(t, f, "Custom answer"))
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("text prompt did not reach delayed delivery completion")
			}
			a := w.ask
			prompt := askTestPrompt(t, f)
			prompt.MessageID = 700
			u := update(200, 11, command)
			u.Message.ReplyToMessage = prompt
			raw, err := json.Marshal(u)
			requireStoreOK(t, err)
			requireStoreOK(t, w.b.db.Accept(200, raw))
			w.handle(incoming{id: 200, msg: u.Message})
			w.dispatch()
			if a.pendingInput != nil {
				t.Fatal("command was buffered as an unconfirmed Ask answer")
			}
			if command == "/stop" {
				if w.client != nil && !w.controlInProgress() {
					t.Fatal("Stop waited for Telegram delivery")
				}
				w.ui(rpcEvent{Method: "cancel", TargetID: a.id})
			} else if w.ask != a || !a.input || a.custom != "" || queueState(t, w, 200) != "ignored" {
				t.Fatal("foreign command was not ignored during prompt delivery")
			}
			close(release)
			released = true
			askTestSettle(t, w)
			if command == "/stop" {
				nativeUntil(t, w, func() bool { return w.client == nil && !w.controlInProgress() })
				if w.taskActive() || queueState(t, w, 200) != "done" {
					t.Fatalf("native cancellation discarded the explicit Stop: active=%d, command=%s", w.active, queueState(t, w, 200))
				}
			} else if len(askTestState(t, w)) != 0 || w.ask != a {
				t.Fatal("foreign command produced a native answer after delivery")
			}
		})
	}
}

func TestAskLiteralQuestionPreservesForceReplyOwner(t *testing.T) {
	for name, question := range map[string]string{
		"wide-table":                 "|" + strings.Repeat("A", 100) + "|B|\n|---|---|\n" + strings.Repeat("|x|y|\n", 40),
		"single-line-fence-boundary": strings.Repeat("`", 1000),
		"fences-and-mentions":        "```\n[Another user](tg://user?id=8)\n```\n" + strings.Repeat("`", 32),
	} {
		t.Run(name, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			askTestOpen(t, w, "literal-prompt", askTestQuestion("topic", question, false, "Go"))
			askTestClick(t, w, f, "Other")
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, message := range f.messages {
				text, _ := message["text"].(string)
				if !strings.Contains(text, "[ask:") {
					continue
				}
				markup, ok := message["reply_markup"].(map[string]any)
				if !ok || markup["force_reply"] != true || markup["selective"] != true {
					continue
				}
				if message["parse_mode"] != "HTML" || !strings.Contains(text, `<a href="tg://user?id=7">`) || strings.Count(text, `<a href="tg://user?id=`) != 1 {
					t.Fatal("question Markdown removed or changed the selective ForceReply target")
				}
				_, body, found := strings.Cut(text, "<pre>")
				body, _, closed := strings.Cut(body, "</pre>")
				if !found || !closed || html.UnescapeString(body) != clipUTF16(question, 1000)+"\n" {
					t.Fatal("question text escaped its literal block or was reformatted")
				}
				return
			}
			t.Fatal("selective ForceReply prompt missing")
		})
	}
}

func TestNativeUIDialogsShareSingleRequestAdmission(t *testing.T) {
	for _, first := range []string{"select", "confirm"} {
		for _, second := range []string{"select", "confirm"} {
			t.Run(first+"/"+second, func(t *testing.T) {
				w, f, _ := askTestWorker(t)
				client, root, turn := w.client, w.active, w.turn
				original := map[string]any{"id": "first", "method": first, "title": "Original decision", "options": []string{"Allow", "Deny"}}
				askTestEvent(t, w, original)
				label := "Allow"
				if first == "confirm" {
					label = "Confirm"
				}
				choice := askTestButton(t, f, label)
				token, _, _ := strings.Cut(choice, ":")
				menu := w.confirms[token]
				before := f.messageCount()
				askTestEvent(t, w, original)
				if f.messageCount() != before || len(askTestState(t, w)) != 0 || w.confirms[token].messageID != menu.messageID {
					t.Fatal("duplicate request changed its pending menu or sent a response")
				}
				intruder := map[string]any{"id": "second", "method": second, "title": "Concurrent decision", "options": []string{"Replace", "Cancel"}}
				askTestEvent(t, w, intruder)
				askTestEvent(t, w, intruder)
				responses := askTestState(t, w)
				if len(responses) != 1 || responses[0].ID != "second" || !responses[0].Cancelled || f.messageCount() != before ||
					w.confirms[token].uiID != "first" || w.confirms[token].messageID != menu.messageID ||
					w.client != client || w.active != root || w.turn != turn {
					t.Fatalf("concurrent request displaced the original menu or was not cancelled once: %+v", responses)
				}
				askTestCallback(w, 7, choice)
				responses = askTestState(t, w)
				if len(responses) != 2 || responses[1].ID != "first" || responses[1].Cancelled {
					t.Fatalf("original menu no longer answered its own request: %+v", responses)
				}
				if first == "select" && responses[1].Value != "Allow" || first == "confirm" && (responses[1].Confirmed == nil || !*responses[1].Confirmed) {
					t.Fatalf("original decision changed: %+v", responses[1])
				}
				completed := f.messageCount()
				askTestEvent(t, w, original)
				if len(askTestState(t, w)) != 2 || f.messageCount() != completed {
					t.Fatal("completed request reopened or replayed its response")
				}
			})
		}
	}
}

func TestNativeUIWaitsForAskResponseSettlement(t *testing.T) {
	for _, method := range []string{"select", "confirm", "ask"} {
		t.Run(method, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			askTestOpen(t, w, "answered", askTestQuestion("topic", "Topic?", false, "Go"))
			askTestCallback(w, 7, askTestButton(t, f, "Go"))
			response := waitOperation(t, w)
			if response.kind != "ask_response" || response.err != nil {
				t.Fatal("first answer was not successfully written")
			}
			next := rpcEvent{ID: "next", Method: method, Title: "Next decision", Options: []string{"Allow", "Deny"},
				Questions: []askQuestion{{ID: "next-question", Question: "Continue?", Options: []askOption{{Label: "Allow"}}}}}
			before := f.messageCount()
			w.ui(next)
			w.ui(next)
			if w.ask == nil || w.askSending != 1 || f.messageCount() != before {
				t.Fatal("next question was cancelled, duplicated or shown before write settlement")
			}
			w.operationReturned(response)
			askTestSettle(t, w)
			label := "Allow"
			if method == "confirm" {
				label = "Confirm"
			}
			askTestClick(t, w, f, label)
			responses := askTestState(t, w)
			if len(responses) != 2 || responses[1].ID != "next" || responses[1].Cancelled {
				t.Fatalf("sequential question did not receive its user's answer: %+v", responses)
			}
			switch method {
			case "select":
				if responses[1].Value != "Allow" {
					t.Fatalf("sequential selection = %+v", responses[1])
				}
			case "confirm":
				if responses[1].Confirmed == nil || !*responses[1].Confirmed {
					t.Fatalf("sequential confirmation = %+v", responses[1])
				}
			case "ask":
				if len(responses[1].Answers) != 1 || strings.Join(responses[1].Answers[0].Selected, ",") != "Allow" {
					t.Fatalf("sequential Ask = %+v", responses[1])
				}
			}
		})
	}
}

func TestNativeUIDeferredQuestionIsInvalidated(t *testing.T) {
	for _, boundary := range []string{"write-failure", "native-cancel", "generation", "runtime"} {
		t.Run(boundary, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			askTestOpen(t, w, "answered", askTestQuestion("topic", "Topic?", false, "Go"))
			askTestCallback(w, 7, askTestButton(t, f, "Go"))
			response := waitOperation(t, w)
			if response.kind != "ask_response" || response.err != nil {
				t.Fatal("fixture did not retain the successful response result")
			}
			w.ui(rpcEvent{ID: "next", Method: "select", Title: "Next decision", Options: []string{"Allow"}})
			before := f.messageCount()
			switch boundary {
			case "write-failure":
				// Inject an uncertain write result after the next request has arrived.
				response.err = context.DeadlineExceeded
			case "native-cancel":
				w.ui(rpcEvent{Method: "cancel", TargetID: "next"})
			case "generation":
				w.binding.Generation++
			case "runtime":
				w.releaseRuntime(true)
			}
			w.operationReturned(response)
			if w.ask != nil || f.messageCount() != before {
				t.Fatal("invalidated deferred question was displayed by the late result")
			}
			if boundary == "write-failure" && (w.client != nil || queueState(t, w, 100) != "uncertain") {
				t.Fatal("failed write did not retire its original runtime conservatively")
			}
		})
	}
}

func TestNativeUIStopRevokesDeferredQuestion(t *testing.T) {
	w, f, _ := askTestWorker(t)
	askTestOpen(t, w, "answered", askTestQuestion("topic", "Topic?", false, "Go"))
	askTestCallback(w, 7, askTestButton(t, f, "Go"))
	response := waitOperation(t, w)
	if response.kind != "ask_response" || response.err != nil {
		t.Fatal("fixture did not retain the successful response result")
	}
	w.ui(rpcEvent{ID: "next", Method: "select", Title: "Next decision", Options: []string{"Allow"}})
	before := f.messageCount()
	nativeInput(t, w, 200, "/stop", false)
	w.operationReturned(response)
	if f.messageCount() != before {
		t.Fatal("completed write reopened a question after Stop revoked it")
	}
	drainControlOperations(t, w)
	if w.ask != nil || w.client != nil {
		t.Fatal("Stop retained the deferred question or its runtime")
	}
}

func TestNativeUIStopBeforeNextQuestion(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_HOLD_RPC", "abort")
	for _, stop := range []string{"command", "task-stop"} {
		for _, write := range []string{"before-ui", "after-ui"} {
			t.Run(stop+"/"+write, func(t *testing.T) {
				w, f, _ := askTestWorker(t)
				askTestOpen(t, w, "answered", askTestQuestion("topic", "Topic?", false, "Go"))
				askTestCallback(w, 7, askTestButton(t, f, "Go"))
				response := waitOperation(t, w)
				if response.kind != "ask_response" || response.err != nil {
					t.Fatal("fixture did not retain the successful response result")
				}
				if stop == "command" {
					nativeInput(t, w, 200, "/stop", false)
				} else {
					w.stopActiveTask()
				}
				before := f.messageCount()
				if write == "before-ui" {
					w.operationReturned(response)
				}
				w.ui(rpcEvent{ID: "next", Method: "select", Title: "Next decision", Options: []string{"Allow"}})
				if write == "after-ui" {
					w.operationReturned(response)
				}
				responses := askTestState(t, w)
				if w.controlOp != controlAbort || w.active != 100 || w.ask != nil || f.messageCount() != before {
					t.Fatal("Stop reopened a question or lost its pending abort/root")
				}
				if len(responses) != 2 || responses[0].ID != "answered" || responses[0].Cancelled ||
					responses[1].ID != "next" || !responses[1].Cancelled {
					t.Fatalf("Stop did not cancel the next question after the original answer: %+v", responses)
				}
			})
		}
	}
}

func TestNativeUIAfterReusableStop(t *testing.T) {
	for _, stop := range []string{"rejected", "idle"} {
		t.Run(stop, func(t *testing.T) {
			if stop == "rejected" {
				t.Setenv("OMP_TELEGRAM_FIXTURE_REJECT_RPC", "abort")
			}
			w, f, _ := askTestWorker(t)
			client := w.client
			if stop == "idle" {
				nativeRPC(t, w, "fixture_finish_root", nil)
				nativeUntil(t, w, func() bool { return !w.taskActive() })
			}
			nativeInput(t, w, 200, "/stop", false)
			drainControlOperations(t, w)
			if w.client != client {
				t.Fatal("Stop retired a reusable runtime without a pending dialog")
			}
			if stop == "idle" && w.taskActive() {
				t.Fatal("idle Stop unexpectedly started a task")
			}
			askTestOpen(t, w, "fresh", askTestQuestion("topic", "Topic?", false, "Go"))
			askTestClick(t, w, f, "Go")
			responses := askTestState(t, w)
			if len(responses) != 1 || responses[0].ID != "fresh" || responses[0].Cancelled ||
				len(responses[0].Answers) != 1 || len(responses[0].Answers[0].Selected) != 1 || responses[0].Answers[0].Selected[0] != "Go" {
				t.Fatalf("reusable runtime did not accept a fresh UI answer: %+v", responses)
			}
		})
	}
}

func TestNativeUIAfterRootlessResearchStop(t *testing.T) {
	w, _, _ := researchWorker(t, "autoresearch")
	f := http.DefaultTransport.(*fakeHTTP)
	w.b.cfg.ProgressMode = "off"
	nativeInput(t, w, 100, "/autoresearch", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	researchBarrier(t, w)
	if w.taskActive() || !w.research.enabled {
		t.Fatal("fixture did not leave enabled research in a rootless runtime")
	}
	client := w.client
	nativeInput(t, w, 200, "/stop", false)
	nativeUntil(t, w, func() bool {
		return w.research.control != nil && w.research.control.phase == "postmode"
	})
	confirmation := waitOperation(t, w)
	if confirmation.err != nil {
		t.Fatalf("fixture did not retain a successful mode confirmation: %v", confirmation.err)
	}
	w.ui(rpcEvent{ID: "before-off-confirmation", Method: "input", Title: "Continue?"})
	responses := askTestState(t, w)
	if w.ask != nil || len(responses) != 1 || responses[0].ID != "before-off-confirmation" || !responses[0].Cancelled {
		t.Fatalf("pending research Stop admitted a native dialog: %+v", responses)
	}
	w.operationReturned(confirmation)
	if w.client != client || w.taskActive() || w.controlInProgress() || w.research.control != nil || w.research.enabled || w.research.disablePending {
		t.Fatal("research Stop did not confirm mode-off in the same rootless runtime")
	}
	askTestOpen(t, w, "after-off-confirmation", askTestQuestion("decision", "Continue?", false, "Allow"))
	askTestClick(t, w, f, "Allow")
	responses = askTestState(t, w)
	if len(responses) != 2 || responses[1].ID != "after-off-confirmation" || responses[1].Cancelled ||
		len(responses[1].Answers) != 1 || len(responses[1].Answers[0].Selected) != 1 || responses[1].Answers[0].Selected[0] != "Allow" {
		t.Fatalf("confirmed research Stop did not allow a fresh rootless answer: %+v", responses)
	}
}

func TestCurrentExportRejectsNativeUIWithoutResponses(t *testing.T) {
	for _, displayFailure := range []bool{false, true} {
		name := "normal"
		if displayFailure {
			name = "display-failure"
		}
		t.Run(name, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			nativeRPC(t, w, "fixture_finish_root", nil)
			nativeUntil(t, w, func() bool { return !w.taskActive() })
			source, err := os.ReadFile(w.binding.Session)
			requireStoreOK(t, err)
			w.initResumePicker()
			w.b.resumeSlots = make(chan struct{}, 1)
			w.b.resumeSlots <- struct{}{}
			w.beginExport(omp.SessionSummary{ID: w.sessionID, CWD: w.binding.Workspace}, "session", w.binding.Workspace, w.binding.Generation)
			if w.exportCancel == nil || w.exportingSession != w.sessionID {
				t.Fatal("fixture did not enter the current-session export window")
			}
			defer func() {
				if w.exportCancel != nil {
					w.cancelExport()
					w.exportFinished(finishExport(t, w))
				}
			}()
			events := []map[string]any{
				{"id": "ask", "method": "ask", "questions": []any{askTestQuestion("decision", "Continue?", false, "Allow")}},
				{"id": "input", "method": "input", "title": "Continue?"},
				{"id": "editor", "method": "editor", "title": "Continue?"},
				{"id": "confirm", "method": "confirm", "title": "Continue?"},
				{"id": "select", "method": "select", "options": []string{"Allow"}},
				{"id": "empty", "method": "ask"},
				{"id": "secret", "method": "input", "secret": true},
			}
			before := f.messageCount()
			f.mu.Lock()
			f.failProgress = displayFailure
			f.mu.Unlock()
			for _, event := range events {
				askTestEvent(t, w, event)
			}
			f.mu.Lock()
			f.failProgress = false
			f.mu.Unlock()
			responses := askTestState(t, w)
			if len(responses) != 0 || w.hasRuntimeConfirmation() || f.messageCount() != before || w.exportingSession != w.sessionID {
				t.Fatalf("native UI escaped export admission: responses=%+v ask=%t", responses, w.ask != nil)
			}
			<-w.b.resumeSlots
			result := finishExport(t, w)
			requireStoreOK(t, result.err)
			data, err := os.ReadFile(result.file.Path)
			requireStoreOK(t, err)
			if !bytes.Equal(data, source) {
				t.Fatal("export changed the session snapshot")
			}
			w.exportFinished(result)
			for _, event := range events {
				askTestEvent(t, w, event)
			}
			responses = askTestState(t, w)
			if len(responses) != 0 || f.messageCount() != before {
				t.Fatal("export completion replayed rejected native requests")
			}
			askTestOpen(t, w, "fresh", askTestQuestion("decision", "Continue?", false, "Allow"))
			askTestClick(t, w, f, "Allow")
			responses = askTestState(t, w)
			if len(responses) != 1 || responses[0].ID != "fresh" || responses[0].Cancelled ||
				len(responses[0].Answers) != 1 || len(responses[0].Answers[0].Selected) != 1 || responses[0].Answers[0].Selected[0] != "Allow" {
				t.Fatalf("fresh native interaction failed after export: %+v", responses)
			}
		})
	}
}

func TestCurrentExportWaitsForNativeInteraction(t *testing.T) {
	for _, phase := range []string{"display", "dialog", "response", "select"} {
		t.Run(phase, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			nativeRPC(t, w, "fixture_finish_root", nil)
			nativeUntil(t, w, func() bool { return !w.taskActive() })
			var held operationResult
			if phase == "display" {
				w.ui(rpcEvent{ID: "pending", Method: "ask", Questions: []askQuestion{
					{ID: "decision", Question: "Continue?", Options: []askOption{{Label: "Allow"}}},
				}})
				held = waitOperation(t, w)
				if held.kind != "ask_display" || held.err != nil {
					t.Fatal("fixture did not retain a successful display result")
				}
			} else if phase == "select" {
				askTestEvent(t, w, map[string]any{"id": "pending", "method": "select", "options": []string{"Allow"}})
			} else {
				askTestOpen(t, w, "pending", askTestQuestion("decision", "Continue?", false, "Allow"))
				if phase == "response" {
					askTestCallback(w, 7, askTestButton(t, f, "Allow"))
					held = waitOperation(t, w)
					if held.kind != "ask_response" || held.err != nil {
						t.Fatal("fixture did not retain a successful answer write")
					}
				}
			}
			w.beginExport(omp.SessionSummary{ID: w.sessionID, CWD: w.binding.Workspace}, "session", w.binding.Workspace, w.binding.Generation)
			if w.exportCancel != nil || w.exportingSession != "" {
				t.Fatal("export started across an unresolved native interaction")
			}
			if held.kind != "" {
				w.operationReturned(held)
			}
			if phase != "response" {
				askTestClick(t, w, f, "Allow")
			}
			responses := askTestState(t, w)
			if len(responses) != 1 || responses[0].ID != "pending" || responses[0].Cancelled {
				t.Fatalf("rejected export cancelled or replaced the native decision: %+v", responses)
			}
		})
	}
}
