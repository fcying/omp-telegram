package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/telegram"
)

func sentReplyTarget(fields map[string]any) any {
	parameters, ok := fields["reply_parameters"].(map[string]any)
	if !ok {
		return nil
	}
	return parameters["message_id"]
}

func allowInitialProgress(w *worker) {
	w.progress.StartedAt = time.Now().Add(-progressInitialDelay)
}

func TestInitialProgressWaitsForDelayAndIsDeletedAfterDelivery(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 1
	w.previewResult = make(chan previewResult, 1)
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	w.flushPreview()
	select {
	case <-w.previewResult:
		t.Fatal("initial progress was sent before the delay")
	default:
	}
	w.progress.StartedAt = time.Now().Add(-progressInitialDelay)
	w.flushPreview()
	var result previewResult
	select {
	case result = <-w.previewResult:
		if result.err != nil || result.id == 0 {
			t.Fatalf("progress send failed: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial progress remained delayed")
	}
	w.previewFinished(result)
	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"short answer"}]}]}`))
	awaitReplyStatus(t, w)
	output, err := w.b.db.NextOutput()
	if err != nil || replyBody(output.Text) != "short answer" {
		t.Fatalf("final reply = %+v, err=%v", output, err)
	}
	if got := f.deletedMessageIDs(); len(got) != 0 {
		t.Fatalf("progress was deleted before final delivery: %v", got)
	}
	if err := w.b.db.MarkOutput(output.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.MarkOutput(output.ID, "done"); err != nil {
		t.Fatal(err)
	}
	w.b.cleanupDeliveredProgress(context.Background(), output.InboxID)
	waitFor(t, func() bool { return len(f.deletedMessageIDs()) == 1 })
	if got := f.deletedMessageIDs(); len(got) != 1 || got[0] != result.id {
		t.Fatalf("deleted progress messages = %v, want [%d]", got, result.id)
	}
}

func TestShortTaskCompletesWithoutProgressMessage(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.previewResult = make(chan previewResult, 1)
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	command("short task")
	w.dispatch()
	w.progress.StartedAt = time.Now()
	w.flushPreview()
	select {
	case result := <-w.previewResult:
		t.Fatalf("short task sent progress: %+v", result)
	default:
	}
	w.preview = "short answer"
	w.finish()
	f.mu.Lock()
	messages := len(f.messages)
	f.mu.Unlock()
	if messages != 0 {
		t.Fatalf("short task sent %d Telegram messages before final delivery", messages)
	}
}

func TestInitialProgressSurvivesNativeActivity(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 1
	w.previewResult = make(chan previewResult, 1)
	command("/new " + t.TempDir())
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	w.event([]byte(`{"type":"agent_start"}`))
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"working"}}`))
	allowInitialProgress(w)
	w.flushPreview()
	select {
	case result := <-w.previewResult:
		if result.err != nil || result.id == 0 {
			t.Fatalf("progress send failed: %+v", result)
		}
		w.previewFinished(result)
	case <-time.After(time.Second):
		t.Fatal("native activity postponed initial progress")
	}
	if f.button(0) == "" {
		t.Fatal("long task progress omitted Stop button")
	}
}

func TestToolHistoryEditsProgressWithSameStopButton(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "verbose"
	w.b.cfg.QueueCapacity = 1
	w.previewResult = make(chan previewResult, 1)
	command("/new " + t.TempDir())
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	w.event([]byte(`{"type":"agent_start"}`))
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Checking source"}}`))
	w.event([]byte(`{"type":"tool_execution_start","toolCallId":"read-1","toolName":"read","args":{"path":"SECRET path"}}`))
	before := f.messageCount()
	allowInitialProgress(w)
	w.flushPreview()
	var initial previewResult
	select {
	case initial = <-w.previewResult:
		if initial.err != nil || initial.id == 0 {
			t.Fatalf("progress send failed: %+v", initial)
		}
		w.previewFinished(initial)
	case <-time.After(5 * time.Second):
		t.Fatal("initial progress not sent")
	}
	stop := f.button(before)
	if !strings.HasPrefix(stop, "stop-7-") {
		t.Fatalf("initial Stop callback = %q", stop)
	}
	f.mu.Lock()
	initialText, _ := f.messages[before]["text"].(string)
	f.mu.Unlock()
	if !strings.Contains(initialText, `Args: <code>{"path":"SECRET path"}</code>`) {
		t.Fatalf("initial progress omitted tool arguments: %q", initialText)
	}
	w.event([]byte(`{"type":"tool_execution_update","toolCallId":"read-1","partialResult":{"content":[{"type":"text","text":"SECRET partial"}]}}`))
	w.flushPreview()
	select {
	case result := <-w.previewResult:
		if result.err != nil || result.id != initial.id || result.created {
			t.Fatalf("partial progress edit failed: %+v", result)
		}
		w.previewFinished(result)
	case <-time.After(5 * time.Second):
		t.Fatal("partial tool update did not edit progress")
	}
	f.mu.Lock()
	partialText, _ := f.messages[before+1]["text"].(string)
	f.mu.Unlock()
	if !strings.Contains(partialText, "Update: <code>") || !strings.Contains(partialText, "SECRET partial") || strings.Contains(partialText, "SECRET result") || f.button(before+1) != stop {
		t.Fatalf("partial progress lost tool update or Stop callback: %q", partialText)
	}
	w.event([]byte(`{"type":"tool_execution_end","toolCallId":"read-1","toolName":"read","result":{"content":[{"type":"text","text":"SECRET result"}]}}`))
	w.flushPreview()
	select {
	case result := <-w.previewResult:
		if result.err != nil || result.id != initial.id || result.created {
			t.Fatalf("progress edit failed: %+v", result)
		}
		w.previewFinished(result)
	case <-time.After(5 * time.Second):
		t.Fatal("tool completion did not edit progress")
	}
	f.mu.Lock()
	if len(f.messages) != before+3 {
		f.mu.Unlock()
		t.Fatalf("tool activity created another message: %d new requests", f.messageCount()-before)
	}
	edit := f.messages[before+2]
	text, _ := edit["text"].(string)
	messageID := edit["message_id"]
	f.mu.Unlock()
	if messageID != float64(initial.id) || !strings.Contains(text, "Status: Running") || !strings.Contains(text, "Recent tools:\n- read: completed (") || !strings.Contains(text, `Args: <code>{"path":"SECRET path"}</code>`) || !strings.Contains(text, "Result: <code>") || !strings.Contains(text, "SECRET result") || strings.Contains(text, "SECRET partial") || !strings.Contains(text, "Output:\nChecking source") {
		t.Fatalf("edited verbose progress is missing tool results: id=%v, text=%q", messageID, text)
	}
	if got := f.button(before + 2); got != stop {
		t.Fatalf("progress edit replaced Stop callback: %q", got)
	}
}

func TestProgressStopContinuesQueuedTasks(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 4
	w.previewResult = make(chan previewResult, 1)
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	command("wait")
	w.dispatch()
	if w.active == 0 || !w.busy {
		t.Fatal("root task did not become active")
	}
	command("/followup queued")
	if len(w.queue) != 1 {
		t.Fatal("queued root task missing before stop")
	}
	allowInitialProgress(w)
	w.flushPreview()
	result := <-w.previewResult
	if result.err != nil || result.id == 0 {
		t.Fatalf("progress send failed: %+v", result)
	}
	w.previewFinished(result)
	data := f.button(0)
	if !strings.HasPrefix(data, "stop-7-") {
		t.Fatalf("progress stop callback = %q", data)
	}
	f.mu.Lock()
	replyTo := sentReplyTarget(f.messages[0])
	f.mu.Unlock()
	if replyTo != float64(2) {
		t.Fatalf("progress reply target = %v, want original root message 2", replyTo)
	}

	clickKeyboard(w, 8, data)
	assertKeyboardClears(t, f)
	if len(w.queue) != 1 || len(w.confirms) != 1 {
		t.Fatal("non-owner consumed the progress stop button")
	}

	clickKeyboard(w, 7, data)
	waitKeyboardClears(t, f, int(result.id))
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=3").Scan(&state); err != nil || state != "pending" {
		t.Fatalf("queued root state = %q, error %v; want pending", state, err)
	}
	if got := f.deletedMessageIDs(); len(got) != 0 {
		t.Fatalf("cancelled task deleted progress: %v", got)
	}
	if len(w.queue) != 1 || len(w.confirms) != 0 {
		t.Fatal("valid progress stop cleared queued work or retained UI state")
	}

	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("stopped root state = %q, error %v; want cancelled", state, err)
	}
	w.dispatch()
	nativeUntil(t, w, func() bool { return queueState(t, w, 3) == "done" })
	var replies int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=3").Scan(&replies))
	var finalText string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox WHERE inbox_id=3").Scan(&finalText))
	if replies != 1 || replyBody(finalText) != "answer: queued" {
		t.Fatal("queued task did not produce its own confirmed reply after Stop")
	}
}

func TestTerminalCompletionDeletesProgressMessage(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 4
	w.previewResult = make(chan previewResult, 1)
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	command("question")
	w.dispatch()
	allowInitialProgress(w)
	w.flushPreview()
	result := <-w.previewResult
	if result.err != nil {
		t.Fatal(result.err)
	}
	w.previewFinished(result)
	w.preview = "final"
	w.finish()
	out, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if got := f.deletedMessageIDs(); len(got) != 0 {
		t.Fatalf("progress was deleted before final delivery: %v", got)
	}
	if err := w.b.db.MarkOutput(out.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.MarkOutput(out.ID, "done"); err != nil {
		t.Fatal(err)
	}
	w.b.cleanupDeliveredProgress(context.Background(), out.InboxID)
	waitKeyboardClears(t, f, int(result.id))
	if len(w.confirms) != 0 || w.previewStopToken != "" {
		t.Fatal("terminal completion retained progress stop state")
	}
	waitFor(t, func() bool { return len(f.deletedMessageIDs()) == 1 })
	if got := f.deletedMessageIDs(); len(got) != 1 || got[0] != result.id {
		t.Fatalf("deleted progress messages = %v, want [%d]", got, result.id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, message := range f.messages {
		if message["text"] == "Task ended. The complete result follows." {
			t.Fatal("terminal progress placeholder was sent")
		}
	}
}

func TestStaleProgressStopClearsOnlyOwnerKeyboard(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	token := "stop-7-0123456789abcdef01234567"
	w.confirms[token] = confirmation{action: "stop", generation: w.binding.Generation, active: 99, turn: w.turn, user: 7, messageID: 42}
	w.active, w.busy = 0, false
	data := token + ":0"
	clickKeyboard(w, 8, data)
	assertKeyboardClears(t, f)
	clickKeyboard(w, 7, data)
	waitKeyboardClears(t, f, 42)
	if _, exists := w.confirms[token]; exists {
		t.Fatal("stale stop token remained actionable")
	}

	// Once task settlement removed the token, the encoded owner still lets the
	// owner remove its own stale keyboard without allowing another user to do so.
	settledToken := "stop-7-fedcba987654321001234567"
	settledData := settledToken + ":0"
	w.callback(&telegram.CallbackQuery{ID: "stale-stop-wrong-owner", From: telegram.User{ID: 8}, Message: &telegram.Message{MessageID: 43}, Data: settledData})
	assertKeyboardClears(t, f, 42)
	w.callback(&telegram.CallbackQuery{ID: "stale-stop-owner", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: 43}, Data: settledData})
	waitKeyboardClears(t, f, 42, 43)
}

func TestRootCompletionPersistsReplyToOrigin(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	w.b.cfg.QueueCapacity = 4
	command("question")
	ready, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	w.dispatch()
	w.preview = "final"
	w.finish()
	out, err := w.b.db.NextOutput()
	if err != nil || out.Text != "final" || out.ReplyTo != 2 {
		t.Fatalf("root final output = %+v, error %v", out, err)
	}
}

func TestQueuedRootRepliesRetainTheirOwnOrigins(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	command("first")
	command("second")
	w.dispatch()
	w.preview = "first answer"
	w.finish()
	w.dispatch()
	w.preview = "second answer"
	w.finish()
	for _, want := range []struct {
		text    string
		replyTo int64
	}{{"first answer", 2}, {"second answer", 3}} {
		out, err := w.b.db.NextOutput()
		if err != nil || out.Text != want.text || out.ReplyTo != want.replyTo {
			t.Fatalf("queued root output = %+v, error %v", out, err)
		}
		if err = w.b.db.MarkOutput(out.ID, "sending"); err != nil {
			t.Fatal(err)
		}
		if err = w.b.db.MarkOutput(out.ID, "done"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAttachmentQueueRetainsOriginReplyTarget(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	in := incoming{id: 2, msg: &telegram.Message{MessageID: 42, From: &telegram.User{ID: 7}, Chat: telegram.Chat{ID: -10}, Document: &telegram.Document{FileID: "attachment"}}}
	w.queueMedia(in)
	if len(w.queue) != 1 || w.queue[0].replyTo != 42 {
		t.Fatalf("attachment root lost its reply target: %+v", w.queue)
	}
}

func activeProgressStop(t *testing.T) (*worker, *fakeHTTP, string, int) {
	t.Helper()
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 4
	w.previewResult = make(chan previewResult, 1)
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	command("wait")
	w.dispatch()
	allowInitialProgress(w)
	w.flushPreview()
	result := <-w.previewResult
	if result.err != nil || result.id == 0 {
		t.Fatalf("progress send failed: %+v", result)
	}
	w.previewFinished(result)
	return w, f, f.button(0), int(result.id)
}

func stopConfirmation(t *testing.T, w *worker, data string) (string, confirmation) {
	t.Helper()
	token, _, ok := strings.Cut(data, ":")
	if !ok {
		t.Fatalf("invalid stop callback data %q", data)
	}
	c, ok := w.confirms[token]
	if !ok || c.action != "stop" {
		t.Fatalf("missing stop confirmation for %q: %+v", token, c)
	}
	return token, c
}

func TestProgressStopRejectsStaleGenerationAndTurn(t *testing.T) {
	for _, scenario := range []string{"generation", "turn", "expiry"} {
		t.Run(scenario, func(t *testing.T) {
			w, f, data, messageID := activeProgressStop(t)
			token, c := stopConfirmation(t, w, data)
			if scenario == "generation" {
				c.generation++
			} else if scenario == "expiry" {
				c.expires = time.Now().Add(-time.Second)
			} else {
				c.turn++
			}
			w.confirms[token] = c
			active, turn := w.active, w.turn
			clickKeyboard(w, 7, data)
			waitKeyboardClears(t, f, messageID)
			if w.active != active || w.turn != turn || !w.busy {
				t.Fatalf("stale %s Stop changed active task: active=%d turn=%d busy=%t", scenario, w.active, w.turn, w.busy)
			}
		})
	}
}

func TestProgressStopReplayAndOldTaskCannotAbortNewerTask(t *testing.T) {
	t.Run("replay", func(t *testing.T) {
		w, f, data, messageID := activeProgressStop(t)
		clickKeyboard(w, 7, data)
		waitKeyboardClears(t, f, messageID)
		if queueState(t, w, 2) != "cancelled" {
			t.Fatal("first Stop did not cancel the root")
		}
		clickKeyboard(w, 7, data)
		if queueState(t, w, 2) != "cancelled" {
			t.Fatal("replayed Stop rewrote the cancelled root")
		}
	})
	t.Run("old-task", func(t *testing.T) {
		w, f, data, messageID := activeProgressStop(t)
		_, c := stopConfirmation(t, w, data)
		w.active = c.active + 1
		clickKeyboard(w, 7, data)
		waitKeyboardClears(t, f, messageID)
		if w.active != c.active+1 || !w.busy {
			t.Fatal("old Stop affected the newer active task")
		}
	})
}

func TestProgressStopCleanupFailureDoesNotBlockAbort(t *testing.T) {
	w, f, data, messageID := activeProgressStop(t)
	f.failKeyboardClear = true
	clickKeyboard(w, 7, data)
	waitKeyboardClears(t, f, messageID)
	if queueState(t, w, 2) != "cancelled" || len(w.confirms) != 0 {
		t.Fatal("keyboard cleanup failure prevented Stop")
	}
}

type stopCallbackHTTP struct {
	base    *fakeHTTP
	method  string
	gate    <-chan struct{}
	started chan context.Context
}

func (f *stopCallbackHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/"+f.method) {
		f.started <- r.Context()
		if f.gate == nil {
			return nil, errors.New("Telegram unavailable")
		}
		select {
		case <-f.gate:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return f.base.RoundTrip(r)
}

func TestProgressStopAdvancesWithoutTelegram(t *testing.T) {
	for _, method := range []string{"answerCallbackQuery", "editMessageReplyMarkup"} {
		for _, outcome := range []string{"blocked", "failure"} {
			t.Run(method+"/"+outcome, func(t *testing.T) {
				w, f, data, messageID := activeProgressStop(t)
				nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
				client := w.client
				transport := &stopCallbackHTTP{base: f, method: method, started: make(chan context.Context, 8)}
				if outcome == "blocked" {
					gate := make(chan struct{})
					transport.gate = gate
					defer close(gate)
				}
				http.DefaultTransport = transport
				w.input = make(chan incoming, 2)
				followup := update(1000, 11, "/followup queued")
				stop := telegram.Update{UpdateID: 1001, CallbackQuery: &telegram.CallbackQuery{
					ID: "network-stop", From: telegram.User{ID: 7},
					Message: &telegram.Message{MessageID: int64(messageID)}, Data: data,
				}}
				for _, u := range []telegram.Update{followup, stop} {
					raw, err := json.Marshal(u)
					requireStoreOK(t, err)
					requireStoreOK(t, w.b.db.Accept(u.UpdateID, raw))
					w.input <- incoming{id: u.UpdateID, msg: u.Message, callback: u.CallbackQuery}
				}
				done := make(chan struct{})
				go func() {
					w.run()
					close(done)
				}()
				t.Cleanup(func() {
					w.cancel()
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Error("worker did not cancel Stop UI requests")
					}
				})
				defer func() {
					if t.Failed() {
						t.Logf("inbox states: root=%s callback=%s followup=%s", queueState(t, w, 2), queueState(t, w, 1001), queueState(t, w, 1000))
					}
				}()
				var uiContext context.Context
				select {
				case uiContext = <-transport.started:
				case <-time.After(3 * time.Second):
					t.Fatal("Stop UI request did not start")
				}
				// The gate stays closed until the old process exits and the actor
				// settles the callback and completes the preserved followup.
				waitForTimeout(t, 3*time.Second, func() bool {
					select {
					case <-client.Done():
						return queueState(t, w, 1001) == "done" && queueState(t, w, 1000) == "done"
					default:
						return false
					}
				})
				if outcome == "blocked" && uiContext.Err() != nil {
					t.Fatal("Stop waited for the Telegram request to expire")
				}
				if got := replyBody(strings.Join(researchReplies(t, w, 1000), "")); got != "answer: queued" {
					t.Fatalf("preserved followup reply = %q", got)
				}
			})
		}
	}
}

func TestIncompleteTerminalStatesClearProgressStopButton(t *testing.T) {
	for _, scenario := range []string{"cancelled", "uncertain"} {
		t.Run(scenario, func(t *testing.T) {
			w, f, _, messageID := activeProgressStop(t)
			if scenario == "cancelled" {
				w.finishCancelled("Task was cancelled before completion.")
			} else {
				w.finishUncertain("Task outcome is uncertain.")
			}
			waitKeyboardClears(t, f, messageID)
			if len(w.confirms) != 0 || w.previewStopToken != "" {
				t.Fatalf("%s terminal state retained progress Stop", scenario)
			}
		})
	}
}

func TestReviewFinalReplyRetainsCommandOrigin(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	command("/review staged changes")
	w.dispatch()
	w.preview = "review complete"
	w.finish()
	out, err := w.b.db.NextOutput()
	if err != nil || out.Text != "review complete" || out.ReplyTo != 2 {
		t.Fatalf("review final output = %+v, error %v", out, err)
	}
}

func TestAttachmentFinalReplyRetainsAttachmentOrigin(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.Accept(2, []byte(`{"update_id":2}`)); err != nil {
		t.Fatal(err)
	}
	w.queue = append(w.queue, queued{id: 2, user: 7, replyTo: 42, text: "attachment prompt"})
	w.dispatch()
	w.preview = "attachment complete"
	w.finish()
	out, err := w.b.db.NextOutput()
	if err != nil || out.Text != "attachment complete" || out.ReplyTo != 42 {
		t.Fatalf("attachment final output = %+v, error %v", out, err)
	}
}

func TestProgressAndFinalReplyShareRootOrigin(t *testing.T) {
	w, f, _, messageID := activeProgressStop(t)
	f.mu.Lock()
	progressReplyTo := sentReplyTarget(f.messages[0])
	f.mu.Unlock()
	if progressReplyTo != float64(2) {
		t.Fatalf("progress reply target = %v, want 2", progressReplyTo)
	}
	w.preview = "final"
	w.finish()
	out, err := w.b.db.NextOutput()
	if err != nil || out.Text != "final" || out.ReplyTo != 2 {
		t.Fatalf("final reply target = %+v, error %v", out, err)
	}
	waitKeyboardClears(t, f, messageID)
}

func TestProgressStopAcceptsCallbackBeforeInitialResult(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 4
	w.previewResult = make(chan previewResult, 1)
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	command("wait")
	w.dispatch()
	allowInitialProgress(w)
	w.flushPreview()
	result := <-w.previewResult
	if result.err != nil || result.id == 0 {
		t.Fatalf("initial progress send failed: %+v", result)
	}
	data := f.button(0)
	token, c := stopConfirmation(t, w, data)
	if c.messageID != 0 {
		t.Fatalf("pending Stop recorded message ID %d before actor processed send result", c.messageID)
	}
	w.callback(&telegram.CallbackQuery{ID: "pending-stop", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: result.id}, Data: data})
	drainControlOperations(t, w)
	waitKeyboardClears(t, f, int(result.id))
	if queueState(t, w, 2) != "cancelled" {
		t.Fatal("pending Stop did not abort the active task")
	}
	if _, exists := w.confirms[token]; exists || w.previewStopToken != "" {
		t.Fatal("pending Stop remained registered after callback consumption")
	}
	w.previewFinished(result)
	if _, exists := w.confirms[token]; exists || w.previewStopToken != "" {
		t.Fatal("initial send result re-registered a consumed Stop token")
	}
}
