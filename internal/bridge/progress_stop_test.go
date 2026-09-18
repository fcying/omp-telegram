package bridge

import (
	"strings"
	"testing"

	"omp-telegram/internal/telegram"
)

func TestProgressStopButtonAbortsOnlyItsActiveRootTask(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 4
	w.previewResult = make(chan previewResult, 1)
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	if err != nil {
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
	replyTo := f.messages[0]["reply_to_message_id"]
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
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=3").Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("queued root state = %q, error %v", state, err)
	}
	if len(w.queue) != 0 || len(w.confirms) != 0 {
		t.Fatal("valid progress stop did not consume its UI state")
	}
}

func TestTerminalCompletionClearsProgressStopButton(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 4
	w.previewResult = make(chan previewResult, 1)
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	command("question")
	w.dispatch()
	w.flushPreview()
	result := <-w.previewResult
	if result.err != nil {
		t.Fatal(result.err)
	}
	w.previewFinished(result)
	w.preview = "final"
	w.finish()
	waitKeyboardClears(t, f, int(result.id))
	if len(w.confirms) != 0 || w.previewStopToken != "" {
		t.Fatal("terminal completion retained progress stop state")
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
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	command("wait")
	w.dispatch()
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
	progressReplyTo := f.messages[0]["reply_to_message_id"]
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
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	command("wait")
	w.dispatch()
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
