package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"omp-telegram/internal/media"
	"omp-telegram/internal/telegram"
)

type queueMenuSnapshot struct {
	messageID int64
	text      string
	rows      [][]map[string]any
}

func queueReady(t *testing.T, w *worker, command func(string)) {
	t.Helper()
	command("/new " + t.TempDir())
	output, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.MarkOutput(output.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.MarkOutput(output.ID, "done"); err != nil {
		t.Fatal(err)
	}
}

func latestQueueMenu(t *testing.T, f *fakeHTTP) queueMenuSnapshot {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.messages) - 1; i >= 0; i-- {
		message := f.messages[i]
		markup, ok := message["reply_markup"].(map[string]any)
		if !ok {
			continue
		}
		rawRows, ok := markup["inline_keyboard"].([]any)
		if !ok {
			continue
		}
		rows := make([][]map[string]any, 0, len(rawRows))
		for _, rawRow := range rawRows {
			rawButtons := rawRow.([]any)
			row := make([]map[string]any, 0, len(rawButtons))
			for _, rawButton := range rawButtons {
				row = append(row, rawButton.(map[string]any))
			}
			rows = append(rows, row)
		}
		messageID := int64(i + 1)
		if rawID, ok := message["message_id"].(float64); ok {
			messageID = int64(rawID)
		}
		return queueMenuSnapshot{messageID: messageID, text: message["text"].(string), rows: rows}
	}
	t.Fatal("queue keyboard missing")
	return queueMenuSnapshot{}
}

func queueButtonData(t *testing.T, menu queueMenuSnapshot, label string) string {
	t.Helper()
	for _, row := range menu.rows {
		for _, button := range row {
			if button["text"] != label {
				continue
			}
			data, ok := button["callback_data"].(string)
			if ok {
				return data
			}
		}
	}
	t.Fatalf("queue button %q missing", label)
	return ""
}

func lastCallback(t *testing.T, f *fakeHTTP) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.callbacks) == 0 {
		t.Fatal("callback answer missing")
	}
	return f.callbacks[len(f.callbacks)-1]
}

func queueCancelData(t *testing.T, menu queueMenuSnapshot, preview string) string {
	t.Helper()
	for _, row := range menu.rows {
		if len(row) != 2 || !strings.Contains(row[0]["text"].(string), preview) || row[1]["text"] != "Cancel" {
			continue
		}
		return row[1]["callback_data"].(string)
	}
	t.Fatalf("queue cancel button for %q missing", preview)
	return ""
}

func clickQueue(w *worker, user, messageID int64, data string) {
	w.callback(&telegram.CallbackQuery{
		ID:   "queue-callback",
		From: telegram.User{ID: user},
		Message: &telegram.Message{
			MessageID: messageID,
		},
		Data: data,
	})
}

func queueState(t *testing.T, w *worker, id int64) string {
	t.Helper()
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestQueueViewerCancelsOnlySelectedPendingTask(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	queueReady(t, w, command)
	command("active task")
	w.dispatch()
	active := w.active
	if active == 0 || !w.busy {
		t.Fatal("active task did not start")
	}
	command("first queued task")
	first := w.queue[0].id
	command("second queued task")
	second := w.queue[1].id

	command("/queue")
	menu := latestQueueMenu(t, f)
	if !strings.Contains(menu.text, "Running: yes") || !strings.Contains(menu.text, "Pending: 2") {
		t.Fatalf("queue summary = %q", menu.text)
	}
	for _, row := range menu.rows {
		for _, button := range row {
			if button["text"] == "Stop" {
				t.Fatal("queue menu exposed an active Stop button")
			}
		}
	}
	data := queueCancelData(t, menu, "first queued task")
	clickQueue(w, 8, menu.messageID, data)
	if got := lastCallback(t, f); got != "This action has expired" || len(w.queue) != 2 {
		t.Fatalf("unauthorized queue cancel answer=%q queue=%+v", got, w.queue)
	}
	clickQueue(w, 7, menu.messageID, data)

	if w.active != active || !w.busy || len(w.queue) != 1 || w.queue[0].id != second {
		t.Fatalf("queue cancel changed active or other pending work: active=%d busy=%t queue=%+v", w.active, w.busy, w.queue)
	}
	if got := queueState(t, w, first); got != "cancelled" {
		t.Fatalf("cancelled queue task state = %q", got)
	}
	if got := queueState(t, w, second); got != "pending" {
		t.Fatalf("untouched queue task state = %q", got)
	}
	if got := lastCallback(t, f); got != "Cancelled queued task." {
		t.Fatalf("queue cancel callback answer = %q", got)
	}
	var aborts int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE text LIKE 'Abort requested%'").Scan(&aborts); err != nil {
		t.Fatal(err)
	}
	if aborts != 0 {
		t.Fatal("queue cancellation aborted the active task")
	}
}

func TestQueueCancelReportsTaskAlreadyDispatched(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 2
	queueReady(t, w, command)
	command("active task")
	w.dispatch()
	command("queued task")
	queuedID := w.queue[0].id
	command("/queue")
	menu := latestQueueMenu(t, f)
	data := queueCancelData(t, menu, "queued task")

	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"active done"}]}]}`))
	w.dispatch()
	if w.active != queuedID || len(w.queue) != 0 || !w.busy {
		t.Fatalf("queued task did not become active: active=%d queue=%+v busy=%t", w.active, w.queue, w.busy)
	}
	clickQueue(w, 7, menu.messageID, data)
	if got := lastCallback(t, f); got != "Task is no longer queued." {
		t.Fatalf("stale queue cancel answer = %q", got)
	}
	if w.active != queuedID || !w.busy {
		t.Fatal("stale queue cancellation aborted the active task")
	}
	if got := queueState(t, w, queuedID); got != "submitted" {
		t.Fatalf("active task state after stale cancel = %q", got)
	}
}

func TestQueueViewerPaginatesPendingTasks(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 16
	queueReady(t, w, command)
	for i := range 7 {
		command("queued task " + string(rune('1'+i)))
	}
	w.queue[1].preparing = true
	w.queue[1].text = ""
	command("/queue")
	menu := latestQueueMenu(t, f)
	if !strings.Contains(menu.text, "Running: no") || !strings.Contains(menu.text, "Pending: 7") || !strings.Contains(menu.text, "Page 1/2") {
		t.Fatalf("first queue page summary = %q", menu.text)
	}
	preparing := false
	for _, row := range menu.rows {
		if len(row) == 2 && strings.Contains(row[0]["text"].(string), "Preparing attachment...") {
			preparing = true
		}
	}
	if !preparing || len(menu.rows) != 7 {
		t.Fatalf("first queue page rows = %+v", menu.rows)
	}
	next := queueButtonData(t, menu, "Next")
	clickQueue(w, 7, menu.messageID, next)
	menu = latestQueueMenu(t, f)
	if !strings.Contains(menu.text, "Page 2/2") || len(menu.rows) != 2 {
		t.Fatalf("second queue page = text %q rows %+v", menu.text, menu.rows)
	}
	previous := queueButtonData(t, menu, "Previous")
	clickQueue(w, 7, menu.messageID, previous)
	menu = latestQueueMenu(t, f)
	if !strings.Contains(menu.text, "Page 1/2") {
		t.Fatalf("previous queue page = %q", menu.text)
	}
}

func TestQueueCancelCleansPreparedAttachment(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	queueReady(t, w, command)
	command("attachment task")
	id := w.queue[0].id
	directory := filepath.Join(w.binding.Workspace, ".telegram", "incoming", "queued")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "file.txt"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	cancelled := false
	w.queue[0].preparing = true
	w.queue[0].cancel = func() { cancelled = true }
	w.queue[0].directory = directory
	command("/queue")
	menu := latestQueueMenu(t, f)
	clickQueue(w, 7, menu.messageID, queueCancelData(t, menu, "Preparing attachment..."))
	if !cancelled || len(w.queue) != 0 {
		t.Fatalf("preparing task cancellation = cancelled:%t queue:%+v", cancelled, w.queue)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("prepared attachment directory remains: %v", err)
	}
	if got := queueState(t, w, id); got != "cancelled" {
		t.Fatalf("prepared attachment state = %q", got)
	}
}

func TestQueueViewerShowsEmptyQueue(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 2
	queueReady(t, w, command)
	command("/queue")
	menu := latestQueueMenu(t, f)
	if !strings.Contains(menu.text, "Running: no") || !strings.Contains(menu.text, "Pending: 0") {
		t.Fatalf("empty queue summary = %q", menu.text)
	}
	if len(menu.rows) != 1 || len(menu.rows[0]) != 1 || menu.rows[0][0]["text"] != "Close" {
		t.Fatalf("empty queue buttons = %+v", menu.rows)
	}
}

func TestQueueCancelPreservesOrderAndReportsEachTask(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	queueReady(t, w, command)
	for _, text := range []string{"A", "B", "C"} {
		command(text)
	}
	ids := []int64{w.queue[0].id, w.queue[1].id, w.queue[2].id}
	command("/queue")
	cancel := func(preview string) {
		t.Helper()
		menu := latestQueueMenu(t, f)
		clickQueue(w, 7, menu.messageID, queueCancelData(t, menu, preview))
		if got := lastCallback(t, f); got != "Cancelled queued task." {
			t.Fatalf("cancel %s callback answer = %q", preview, got)
		}
	}
	cancel("B")
	if len(w.queue) != 2 || w.queue[0].id != ids[0] || w.queue[1].id != ids[2] {
		t.Fatalf("after middle cancellation queue = %+v", w.queue)
	}
	if got := queueState(t, w, ids[1]); got != "cancelled" {
		t.Fatalf("middle task state = %q", got)
	}
	cancel("A")
	if len(w.queue) != 1 || w.queue[0].id != ids[2] {
		t.Fatalf("after first cancellation queue = %+v", w.queue)
	}
	if got := queueState(t, w, ids[0]); got != "cancelled" {
		t.Fatalf("first task state = %q", got)
	}
	cancel("C")
	if len(w.queue) != 0 {
		t.Fatalf("after last cancellation queue = %+v", w.queue)
	}
	if got := queueState(t, w, ids[2]); got != "cancelled" {
		t.Fatalf("last task state = %q", got)
	}
	menu := latestQueueMenu(t, f)
	if !strings.Contains(menu.text, "Pending: 0") || !strings.Contains(menu.text, "Page 1/1") {
		t.Fatalf("refreshed empty queue = %q", menu.text)
	}
}

func TestQueueCancelLastItemReturnsToPreviousPage(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 16
	queueReady(t, w, command)
	for i := range 7 {
		command(string(rune('A' + i)))
	}
	command("/queue")
	menu := latestQueueMenu(t, f)
	clickQueue(w, 7, menu.messageID, queueButtonData(t, menu, "Next"))
	menu = latestQueueMenu(t, f)
	clickQueue(w, 7, menu.messageID, queueCancelData(t, menu, "G"))
	if got := lastCallback(t, f); got != "Cancelled queued task." {
		t.Fatalf("last page cancellation answer = %q", got)
	}
	menu = latestQueueMenu(t, f)
	if !strings.Contains(menu.text, "Pending: 6") || !strings.Contains(menu.text, "Page 1/1") {
		t.Fatalf("page after last item cancellation = %q", menu.text)
	}
	if len(w.queue) != 6 || w.queue[0].text != "A" || w.queue[5].text != "F" {
		t.Fatalf("remaining queue after pagination cancellation = %+v", w.queue)
	}
}

func TestQueueCallbackRejectsStaleMessageAndGeneration(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 2
	queueReady(t, w, command)
	command("pending")
	command("/queue")
	menu := latestQueueMenu(t, f)
	data := queueCancelData(t, menu, "pending")
	clickQueue(w, 7, menu.messageID+1, data)
	if got := lastCallback(t, f); got != "This action has expired" || len(w.queue) != 1 {
		t.Fatalf("wrong message callback answer=%q queue=%+v", got, w.queue)
	}
	command("/queue")
	menu = latestQueueMenu(t, f)
	w.binding.Generation++
	clickQueue(w, 7, menu.messageID, queueCancelData(t, menu, "pending"))
	if got := lastCallback(t, f); got != "This action has expired" || len(w.queue) != 1 {
		t.Fatalf("stale generation callback answer=%q queue=%+v", got, w.queue)
	}
}

func TestQueueStaleMediaResultCleansCancelledTask(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	queueReady(t, w, command)
	command("attachment task")
	id := w.queue[0].id
	directory := filepath.Join(w.binding.Workspace, ".telegram", "incoming", "stale")
	w.queue[0].preparing = true
	w.queue[0].directory = directory
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "file.txt"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if !w.cancelQueuedTask(id) {
		t.Fatal("queued attachment was not cancelled")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "late.txt"), []byte("late"), 0600); err != nil {
		t.Fatal(err)
	}
	w.preparedMedia(mediaResult{id: id, generation: w.binding.Generation, workspace: w.binding.Workspace, input: media.Input{Directory: directory}, logger: w.mediaTaskLogger(id)})
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("stale media directory remains: %v", err)
	}
	if len(w.queue) != 0 {
		t.Fatalf("stale media result recreated queue: %+v", w.queue)
	}
	if got := queueState(t, w, id); got != "cancelled" {
		t.Fatalf("stale media changed state to %q", got)
	}
}

func TestQueueCancelReleasesCapacity(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 2
	queueReady(t, w, command)
	command("A")
	command("B")
	second := w.queue[1].id
	command("/queue")
	menu := latestQueueMenu(t, f)
	clickQueue(w, 7, menu.messageID, queueCancelData(t, menu, "B"))
	if got := lastCallback(t, f); got != "Cancelled queued task." {
		t.Fatalf("capacity cancellation answer = %q", got)
	}
	command("C")
	if len(w.queue) != 2 || w.queue[0].text != "A" || w.queue[1].text != "C" {
		t.Fatalf("queue did not reuse capacity: %+v", w.queue)
	}
	if got := queueState(t, w, second); got != "cancelled" {
		t.Fatalf("released capacity task state = %q", got)
	}
}
