package bridge

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/telegram"
)

func sendRealtimePreview(t *testing.T, w *worker) previewResult {
	t.Helper()
	allowInitialProgress(w)
	w.flushPreview()
	select {
	case result := <-w.previewResult:
		requireStoreOK(t, result.err)
		w.previewFinished(result)
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("live preview did not reach Telegram")
		return previewResult{}
	}
}
func awaitProgressDeletion(t *testing.T, w *worker, taskID, messageID int64) {
	t.Helper()
	waitFor(t, func() bool {
		var pending int
		err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM progress_messages WHERE inbox_id=? AND message_id=?", taskID, messageID).Scan(&pending)
		return err == nil && pending == 0
	})
}

func realtimePreviewWorker(t *testing.T) (*worker, *fakeHTTP) {
	t.Helper()
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	w.b.cfg.ProgressMode = "summary"
	w.previewResult = make(chan previewResult, 4)
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	requireStoreOK(t, err)
	requireStoreOK(t, w.b.db.MarkOutput(ready.ID, "sending"))
	requireStoreOK(t, w.b.db.MarkOutput(ready.ID, "done"))
	command("wait")
	w.dispatch()
	waitSteerRootStart(t, w)
	w.operationReturned(waitOperation(t, w))
	return w, f
}

func TestRealtimePreviewKeepsBoundedTailWithoutChangingBody(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	body := "```text\n" + strings.Repeat("😀", 2500) + "\nrecent tail\n```"
	w.stream.Reset()
	w.stream.WriteString(body)
	text := w.renderProgress()
	_, tail, found := strings.Cut(text, w.tr(i18n.Msg.Output))
	visible := html.UnescapeString(replyMetricsHTMLTags.ReplaceAllString(telegram.ConvertMarkdown(tail), ""))
	if !found || !strings.Contains(visible, "recent tail") || utf16Length(visible) > maxPreviewUnits {
		t.Fatalf("live preview lost its recent bounded Unicode tail: %q", visible)
	}
	if w.stream.String() != body {
		t.Fatal("preview formatting changed the pending formal body")
	}
}

func TestSteerRotatesPreviewAndCompletionDeletesOnlyProgress(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_PENDING_STEER", "executing")
	w, f := realtimePreviewWorker(t)
	root, client, turn := w.active, w.client, w.turn
	stoppedAssistantMessage(t, w, "Completed before steering.")
	deliverReplies(t, w)
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Preview before steering."}}`))
	first := sendRealtimePreview(t, w)
	progress := []int64{first.id}
	f.mu.Lock()
	boundary := len(f.messages)
	f.mu.Unlock()
	for i, id := range []int64{101, 102} {
		nativeInput(t, w, id, "Steer this running task", false)
		w.operationReturned(waitOperation(t, w))
		awaitProgressDeletion(t, w, root, progress[i])
		w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"New preview after steering."}}`))
		result := sendRealtimePreview(t, w)
		if !result.created || result.id == progress[len(progress)-1] {
			t.Fatal("steer continued editing the earlier preview")
		}
		progress = append(progress, result.id)
		f.mu.Lock()
		request := f.messages[len(f.messages)-1]
		if sentReplyTarget(request) != float64(id) || request["message_id"] != nil {
			f.mu.Unlock()
			t.Fatalf("new preview did not follow the steer message: %+v", request)
		}
		for _, request := range f.messages[boundary:] {
			if request["message_id"] == float64(progress[i]) {
				f.mu.Unlock()
				t.Fatal("frozen preview was edited after a steer")
			}
		}
		boundary = len(f.messages)
		f.mu.Unlock()
		previews, err := w.b.db.ProgressMessages(root)
		requireStoreOK(t, err)
		if len(previews) != 1 || previews[0].MessageID != result.id || queueState(t, w, root) != "submitted" {
			t.Fatalf("steer did not retain only the latest preview of the running task: %+v", previews)
		}
		stoppedAssistantMessage(t, w, []string{"Question after first steer.", "Final body."}[i])
		deliverReplies(t, w)
		awaitProgressDeletion(t, w, root, result.id)
	}
	if w.active != root || w.client != client || w.turn != turn {
		t.Fatal("preview replacement changed the running task or runtime")
	}
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[]}`))
	for id := range w.steers {
		raw, err := json.Marshal(map[string]any{"type": "prompt_result", "id": id, "status": "completed", "sessionSettled": true})
		requireStoreOK(t, err)
		w.event(raw)
	}
	awaitReplyStatus(t, w)
	deliverReplies(t, w)
	deleted := make(map[int64]bool)
	for _, id := range f.deletedMessageIDs() {
		deleted[id] = true
	}
	if len(deleted) != len(progress) || queueState(t, w, root) != "done" {
		t.Fatalf("completion deleted messages other than the task's previews: %v", deleted)
	}
	for _, id := range progress {
		if !deleted[id] {
			t.Fatalf("progress %d was not cleaned: %v", id, deleted)
		}
	}
	finalReplyStatus(t, w, root, "Completed before steering.", "Question after first steer.", "Final body.")
}

func TestSteerProgressDeletionFailurePreservesRuntimeAndCleanupIdentity(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_PENDING_STEER", "executing")
	w, f := realtimePreviewWorker(t)
	root, client, turn := w.active, w.client, w.turn
	old := sendRealtimePreview(t, w)
	f.mu.Lock()
	f.progressDeleteStatus = http.StatusInternalServerError
	f.mu.Unlock()
	nativeInput(t, w, 101, "Continue despite a Telegram deletion failure", false)
	w.operationReturned(waitOperation(t, w))
	w.background.Wait()
	previews, err := w.b.db.ProgressMessages(root)
	requireStoreOK(t, err)
	if len(previews) != 1 || previews[0].MessageID != old.id || len(f.deletedMessageIDs()) == 0 {
		t.Fatal("failed deletion lost the preview's durable cleanup identity")
	}
	current := sendRealtimePreview(t, w)
	f.mu.Lock()
	f.progressDeleteStatus = 0
	f.mu.Unlock()
	nativeInput(t, w, 102, "Replace both retained previews", false)
	w.operationReturned(waitOperation(t, w))
	awaitProgressDeletion(t, w, root, old.id)
	awaitProgressDeletion(t, w, root, current.id)
	latest := sendRealtimePreview(t, w)
	previews, err = w.b.db.ProgressMessages(root)
	requireStoreOK(t, err)
	if len(previews) != 1 || previews[0].MessageID != latest.id || w.active != root || w.client != client || w.turn != turn || queueState(t, w, root) != "submitted" {
		t.Fatal("preview cleanup changed execution or removed the newest preview")
	}
}

func TestSteerCancelsPendingPreviewEditWithoutRebindingItsResult(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_PENDING_STEER", "executing")
	w, f := realtimePreviewWorker(t)
	initial := sendRealtimePreview(t, w)
	transport := &pendingProgressEditHTTP{fallback: f, started: make(chan struct{}), gate: make(chan struct{})}
	http.DefaultTransport = transport
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Blocked old preview update."}}`))
	w.flushPreview()
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("old preview edit did not start")
	}
	nativeInput(t, w, 101, "Use a new preview", false)
	w.flushPreview()
	var current previewResult
	for i := 0; i < 2; i++ {
		select {
		case result := <-w.previewResult:
			w.previewFinished(result)
			if result.created {
				requireStoreOK(t, result.err)
				current = result
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled preview or its replacement did not settle")
		}
	}
	if current.id == 0 || current.id == initial.id || w.previewID != current.id || w.previewBusy {
		t.Fatal("late preview response rebound or blocked the new preview")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.messages {
		if request["message_id"] == float64(initial.id) {
			t.Fatal("cancelled old preview edit reached Telegram after steering")
		}
	}
}

type acceptedPreviewHTTP struct {
	base       *fakeHTTP
	started    chan struct{}
	gate       chan struct{}
	held       atomic.Bool
	requestCtx context.Context
}

func (f *acceptedPreviewHTTP) RoundTrip(request *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(request.URL.Path, "/sendMessage") {
		return f.base.RoundTrip(request)
	}
	hold := f.held.CompareAndSwap(false, true)
	if !hold {
		return f.base.RoundTrip(request)
	}
	response, err := f.base.RoundTrip(request)
	if err != nil {
		return response, err
	}
	f.requestCtx = request.Context()
	close(f.started)
	select {
	case <-f.gate:
		return response, nil
	case <-request.Context().Done():
		response.Body.Close()
		return nil, request.Context().Err()
	}
}

func TestSteerPreservesAcceptedPreviewSendIdentity(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_PENDING_STEER", "executing")
	w, f := realtimePreviewWorker(t)
	transport := &acceptedPreviewHTTP{base: f, started: make(chan struct{}), gate: make(chan struct{})}
	release := sync.OnceFunc(func() { close(transport.gate) })
	defer release()
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = f }()
	allowInitialProgress(w)
	w.flushPreview()
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("initial preview did not reach Telegram")
	}
	nativeInput(t, w, 101, "Steer before the preview response arrives", false)
	if transport.requestCtx.Err() != nil {
		t.Fatal("steer cancelled an accepted Send before its identity arrived")
	}
	w.operationReturned(waitOperation(t, w))
	current := sendRealtimePreview(t, w)
	release()
	var late previewResult
	select {
	case late = <-w.previewResult:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted preview identity did not arrive")
	}
	requireStoreOK(t, late.err)
	w.previewFinished(late)
	if late.id == 0 || late.id == current.id || w.previewID != current.id {
		t.Fatal("old Send identity was lost or rebound the current preview")
	}
	awaitProgressDeletion(t, w, late.taskID, late.id)
	previews, err := w.b.db.ProgressMessages(w.active)
	requireStoreOK(t, err)
	if len(previews) != 1 || previews[0].MessageID != current.id || !w.taskActive() {
		t.Fatal("late Send cleanup removed the new preview or stopped the task")
	}
	assistantMessage(t, w, "Final body")
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[]}`))
	for id := range w.steers {
		raw, err := json.Marshal(map[string]any{"type": "prompt_result", "id": id, "status": "completed", "sessionSettled": true})
		requireStoreOK(t, err)
		w.event(raw)
	}
	awaitReplyStatus(t, w)
	deliverReplies(t, w)
	waitFor(t, func() bool {
		var pending int
		err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM progress_messages WHERE inbox_id=?", late.taskID).Scan(&pending)
		return err == nil && pending == 0
	})
	removed := make(map[int64]bool)
	for _, id := range f.deletedMessageIDs() {
		if id != late.id && id != current.id {
			t.Fatalf("cleanup deleted a formal reply instead of a preview: %d", id)
		}
		removed[id] = true
	}
	if !removed[late.id] || !removed[current.id] {
		t.Fatalf("completion left an accepted preview visible: %v", f.deletedMessageIDs())
	}
}

func TestFormalBodyDeliveryPrecedesNewPreview(t *testing.T) {
	w, f := realtimePreviewWorker(t)
	root, client, turn := w.active, w.client, w.turn
	initial := sendRealtimePreview(t, w)
	transport := &acceptedPreviewHTTP{base: f, started: make(chan struct{}), gate: make(chan struct{})}
	release := sync.OnceFunc(func() { close(transport.gate) })
	defer release()
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = f }()
	assistantMessage(t, w, "First confirmed body")
	outputID := w.assistantDelivery.statusOutboxID
	ctx, cancel := context.WithCancel(w.ctx)
	done := make(chan error, 1)
	go func() { done <- w.b.deliver(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			requireStoreOK(t, err)
		case <-time.After(5 * time.Second):
			t.Error("delivery did not stop")
		}
	}()
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("formal body Send did not start")
	}
	awaitProgressDeletion(t, w, root, initial.id)
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Work after the formal body"}}`))
	w.flushPreview()
	if w.previewID != 0 || w.previewBusy || f.messageCount() != 2 {
		t.Fatal("new preview overtook an in-flight formal body or edited the old preview")
	}
	release()
	waitFor(t, func() bool {
		var state string
		err := w.b.db.DB.QueryRow("SELECT state FROM outbox WHERE id=?", outputID).Scan(&state)
		return err == nil && state == "done"
	})
	next := sendRealtimePreview(t, w)
	if next.id <= initial.id || !next.created || !strings.Contains(next.text, "Work after the formal body") {
		t.Fatal("new live output did not create a fresh preview")
	}
	assistantMessage(t, w, "Second confirmed body")
	outputID = w.assistantDelivery.statusOutboxID
	waitFor(t, func() bool {
		var state string
		err := w.b.db.DB.QueryRow("SELECT state FROM outbox WHERE id=?", outputID).Scan(&state)
		return err == nil && state == "done"
	})
	awaitProgressDeletion(t, w, root, next.id)
	if w.active != root || w.client != client || w.turn != turn || queueState(t, w, root) != "submitted" {
		t.Fatal("completed message cleanup changed the running task or runtime")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var first, second, preview int
	for i, request := range f.messages {
		text, _ := request["text"].(string)
		switch {
		case strings.Contains(text, "First confirmed body"):
			first = i
			if sentReplyTarget(request) != float64(w.active) {
				t.Fatal("first formal body lost the original request quote")
			}
		case strings.Contains(text, "Second confirmed body"):
			second = i
			if sentReplyTarget(request) != nil {
				t.Fatal("continuation formal body repeated the original request quote")
			}
		case strings.Contains(text, "Work after the formal body"):
			preview = i
		}
		if request["message_id"] == float64(initial.id) || request["message_id"] == float64(next.id) {
			t.Fatal("a frozen preview was edited after its formal message")
		}
	}
	if first == 0 || first >= preview || preview >= second {
		t.Fatalf("visible message order is not body, preview, body: %d, %d, %d", first, preview, second)
	}
}

func TestCompletedMessageDeletesLateAcceptedPreview(t *testing.T) {
	w, f := realtimePreviewWorker(t)
	root, client, turn := w.active, w.client, w.turn
	transport := &acceptedPreviewHTTP{base: f, started: make(chan struct{}), gate: make(chan struct{})}
	release := sync.OnceFunc(func() { close(transport.gate) })
	defer release()
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = f }()
	allowInitialProgress(w)
	w.flushPreview()
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("initial preview was not accepted")
	}
	assistantMessage(t, w, "Confirmed body while the preview response is pending")
	if transport.requestCtx.Err() != nil {
		t.Fatal("message completion cancelled an accepted Send before its identity arrived")
	}
	release()
	select {
	case late := <-w.previewResult:
		requireStoreOK(t, late.err)
		w.previewFinished(late)
		awaitProgressDeletion(t, w, root, late.id)
	case <-time.After(5 * time.Second):
		t.Fatal("accepted preview identity did not arrive")
	}
	deliverReplies(t, w)
	latest := sendRealtimePreview(t, w)
	previews, err := w.b.db.ProgressMessages(root)
	requireStoreOK(t, err)
	if len(previews) != 1 || previews[0].MessageID != latest.id || w.active != root || w.client != client || w.turn != turn || queueState(t, w, root) != "submitted" {
		t.Fatal("late preview cleanup changed execution or removed the newest preview")
	}
	rows := researchReplies(t, w, root)
	if len(rows) != 1 || replyBody(t, rows[0]) != "Confirmed body while the preview response is pending" {
		t.Fatal("preview cleanup lost or duplicated the formal reply")
	}
}
