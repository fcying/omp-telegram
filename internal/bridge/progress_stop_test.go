package bridge

import (
	"context"
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
	output, err := w.b.db.NextOutput()
	if err != nil || output.Text != "short answer" {
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
	command("queued")
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
	assertKeyboardClears(t, f, int(result.id))
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
	if callbackCount(f, "Stopping") != 1 {
		t.Fatal("progress Stop did not request one abort")
	}

	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","stopReason":"aborted","errorMessage":"Request was aborted"}]}`))
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("stopped root state = %q, error %v; want cancelled", state, err)
	}
	w.dispatch()
	if w.active != 3 || !w.busy || len(w.queue) != 0 {
		t.Fatalf("queued task did not continue after Stop: active=%d busy=%t queue=%d", w.active, w.busy, len(w.queue))
	}
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=3").Scan(&state); err != nil || state != "submitted" {
		t.Fatalf("continued root state = %q, error %v; want submitted", state, err)
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
	assertKeyboardClears(t, f, 42)
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
	assertKeyboardClears(t, f, 42, 43)
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

func callbackCount(f *fakeHTTP, text string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, got := range f.callbacks {
		if got == text {
			count++
		}
	}
	return count
}

func TestProgressStopRejectsStaleGenerationAndTurn(t *testing.T) {
	for _, scenario := range []string{"generation", "turn"} {
		t.Run(scenario, func(t *testing.T) {
			w, f, data, messageID := activeProgressStop(t)
			token, c := stopConfirmation(t, w, data)
			if scenario == "generation" {
				c.generation++
			} else {
				c.turn++
			}
			w.confirms[token] = c
			active, turn := w.active, w.turn
			clickKeyboard(w, 7, data)
			assertKeyboardClears(t, f, messageID)
			if w.active != active || w.turn != turn || !w.busy {
				t.Fatalf("stale %s Stop changed active task: active=%d turn=%d busy=%t", scenario, w.active, w.turn, w.busy)
			}
			if callbackCount(f, "Stopping") != 0 {
				t.Fatalf("stale %s Stop issued abort", scenario)
			}
		})
	}
}

func TestProgressStopReplayAndOldTaskCannotAbortNewerTask(t *testing.T) {
	w, f, data, messageID := activeProgressStop(t)
	clickKeyboard(w, 7, data)
	assertKeyboardClears(t, f, messageID)
	if callbackCount(f, "Stopping") != 1 {
		t.Fatal("first Stop was not accepted")
	}
	clickKeyboard(w, 7, data)
	if callbackCount(f, "Stopping") != 1 {
		t.Fatal("replayed Stop issued another abort")
	}

	w, f, data, messageID = activeProgressStop(t)
	_, c := stopConfirmation(t, w, data)
	w.active = c.active + 1
	clickKeyboard(w, 7, data)
	assertKeyboardClears(t, f, messageID)
	if w.active != c.active+1 || !w.busy || callbackCount(f, "Stopping") != 0 {
		t.Fatal("old Stop affected the newer active task")
	}
}

func TestProgressStopCleanupFailureDoesNotBlockAbort(t *testing.T) {
	w, f, data, messageID := activeProgressStop(t)
	f.failKeyboardClear = true
	clickKeyboard(w, 7, data)
	assertKeyboardClears(t, f, messageID)
	if callbackCount(f, "Stopping") != 1 || len(w.confirms) != 0 {
		t.Fatal("keyboard cleanup failure prevented Stop")
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
	assertKeyboardClears(t, f, int(result.id))
	if callbackCount(f, "Stopping") != 1 {
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
