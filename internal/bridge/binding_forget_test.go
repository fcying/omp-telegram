package bridge

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func waitBindingAction(t *testing.T, fake *fakeHTTP, after int, text, label string) (int64, string) {
	t.Helper()
	var messageID int64
	var data string
	waitFor(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for i := after; i < len(fake.messages); i++ {
			message := fake.messages[i]
			body, _ := message["text"].(string)
			if message["message_thread_id"] != float64(11) || !strings.Contains(body, text) {
				continue
			}
			markup, ok := message["reply_markup"].(map[string]any)
			if !ok {
				continue
			}
			for _, row := range markup["inline_keyboard"].([]any) {
				for _, value := range row.([]any) {
					button := value.(map[string]any)
					labelText, _ := button["text"].(string)
					matches := labelText == label
					if label == "" {
						matches = labelText != "Previous" && labelText != "Next" && labelText != "Close"
					}
					if matches && button["callback_data"] != nil {
						messageID, data = int64(i+1), button["callback_data"].(string)
						return true
					}
				}
			}
		}
		return false
	})
	return messageID, data
}

func sendBindingCallback(t *testing.T, db *store.Store, send func(telegram.Update), id, messageID int64, data string) {
	t.Helper()
	message := update(0, 11, "").Message
	message.MessageID = messageID
	send(telegram.Update{UpdateID: id, CallbackQuery: &telegram.CallbackQuery{ID: strconv.FormatInt(id, 10), From: telegram.User{ID: 7}, Message: message, Data: data}})
	waitInputDone(t, db, id)
}

func TestBindingDeleteOpenFromOtherTopic(t *testing.T) {
	fake, db, send := setupBridge(t)
	workspace := t.TempDir()
	send(update(1, 22, "/new "+workspace))
	waitBinding(t, db, 22)
	waitFor(t, func() bool { return fake.has(22, "omp is ready.") })
	bound, err := db.Binding(99, -10, 22)
	if err != nil || !bound.Running {
		t.Fatalf("target binding = %+v, %v", bound, err)
	}
	closed := store.Binding{Bot: 99, Chat: -10, Thread: 33, Workspace: t.TempDir(), Generation: 1}
	if err := db.Save(closed); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPinnedSession(99, -10, 22, workspace, bound.SessionID, true); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, "preserved.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	before := fake.messageCount()
	send(update(2, 11, "/bindings old"))
	listID, data := waitBindingAction(t, fake, before, "Saved bindings (oldest first)", "")
	oldListID, oldData := listID, data
	sendBindingCallback(t, db, send, 3, listID, data)
	confirmID, data := waitBindingAction(t, fake, int(listID), "Close and forget idle binding for Topic 22?", "Delete")
	sendBindingCallback(t, db, send, 4, confirmID, data)
	waitFor(t, func() bool { _, err := db.Binding(99, -10, 22); return errors.Is(err, sql.ErrNoRows) })
	waitFor(t, func() bool { return fake.has(11, "Saved binding deleted.") })
	if fake.has(22, "Saved binding deleted.") {
		t.Fatal("deletion reply was sent to the deleted topic")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatalf("workspace changed: %q, %v", data, err)
	}
	if _, err := os.Stat(bound.Session); err != nil {
		t.Fatalf("native session file was removed: %v", err)
	}
	pinned, err := db.PinnedSessions(99, -10, 22, workspace)
	if err != nil || len(pinned) != 0 {
		t.Fatalf("deleted binding retained favorites: %+v, %v", pinned, err)
	}
	listID, data = waitBindingAction(t, fake, int(confirmID), "Saved bindings (oldest first)", "")
	messageCount := fake.messageCount()
	sendBindingCallback(t, db, send, 5, oldListID, oldData)
	if fake.messageCount() != messageCount {
		t.Fatal("stale binding menu opened another confirmation")
	}
	if _, err := db.Binding(99, -10, 33); err != nil {
		t.Fatalf("stale menu changed the remaining binding: %v", err)
	}
	sendBindingCallback(t, db, send, 6, listID, data)
	confirmID, data = waitBindingAction(t, fake, int(listID), "Forget saved binding for Topic 33?", "Delete")
	sendBindingCallback(t, db, send, 7, confirmID, data)
	waitFor(t, func() bool { _, err := db.Binding(99, -10, 33); return errors.Is(err, sql.ErrNoRows) })
	waitFor(t, func() bool { return fake.has(11, "No saved conversation bindings.") })
}

func TestBindingDeleteOpenClampsLastPage(t *testing.T) {
	fake, db, send := setupBridge(t)
	workspace := t.TempDir()
	send(update(1, 22, "/new "+workspace))
	waitBinding(t, db, 22)
	waitFor(t, func() bool { return fake.has(22, "omp is ready.") })
	now := time.Now().Unix()
	for i := range bindingPageSize {
		if err := db.Save(store.Binding{Bot: 99, Chat: -10, Thread: int64(30 + i), Workspace: "/older", Generation: 1, LastUsedAt: now - int64(i+1)*3600}); err != nil {
			t.Fatal(err)
		}
	}
	before := fake.messageCount()
	send(update(2, 11, "/bindings old"))
	listID, data := waitBindingAction(t, fake, before, "Saved bindings (oldest first)\nPage 1/2", "Next")
	sendBindingCallback(t, db, send, 3, listID, data)
	text, rows := pickerView(t, fake)
	if text != "Saved bindings (oldest first)\nPage 2/2" || len(rows) != 3 || !strings.Contains(rows[0][0]["text"].(string), " · #22") {
		t.Fatalf("open binding last page = %q, rows = %+v", text, rows)
	}
	sendBindingCallback(t, db, send, 4, listID, rows[0][0]["callback_data"].(string))
	confirmID, data := waitBindingAction(t, fake, int(listID), "Close and forget idle binding for Topic 22?", "Delete")
	sendBindingCallback(t, db, send, 5, confirmID, data)
	waitFor(t, func() bool { _, err := db.Binding(99, -10, 22); return errors.Is(err, sql.ErrNoRows) })
	waitFor(t, func() bool { return fake.has(11, "Saved bindings (oldest first)\nPage 1/1") })
	text, rows = pickerView(t, fake)
	if text != "Saved bindings (oldest first)\nPage 1/1" || len(rows) != 13 || rows[0][0]["text"] != "1. older · #35" {
		t.Fatalf("open binding refreshed page = %q, rows = %+v", text, rows)
	}
}

func TestBindingDeleteRestoredOpenWithoutRuntime(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
	d := newRecoveryDaemon(t, 1)
	workspace := t.TempDir()
	d.command(22, "/new "+workspace)
	waitFor(t, func() bool { return d.fake.has(22, "omp is ready.") })
	previous := d.binding(22)
	if !previous.Running {
		t.Fatal("target binding was not open before restart")
	}
	if _, err := os.Stat(previous.Session); err != nil {
		t.Fatalf("native session file is unavailable: %v", err)
	}
	targetMessages := d.fake.messageCount()
	d.stop()
	d.start()
	if restored := d.binding(22); restored != previous {
		t.Fatalf("restart changed the target binding: before=%+v after=%+v", previous, restored)
	}
	d.command(11, "/new "+t.TempDir())
	waitFor(t, func() bool { return d.fake.has(11, "omp is ready.") })
	d.command(11, "/close")
	d.command(11, "/resume "+previous.SessionID)
	waitFor(t, func() bool { return d.fake.has(11, "This session is already running in another conversation.") })
	before := d.fake.messageCount()
	d.command(11, "/bindings")
	listID, data := waitBindingAction(t, d.fake, before, "Saved bindings", "")
	click := func(messageID int64, data string) {
		t.Helper()
		d.nextID++
		sendBindingCallback(t, d.db, func(update telegram.Update) { d.fake.updates <- update }, d.nextID, messageID, data)
	}
	click(listID, data)
	confirmID, data := waitBindingAction(t, d.fake, int(listID), "Close and forget idle binding for Topic 22?", "Delete")
	click(confirmID, data)
	waitFor(t, func() bool { _, err := d.db.Binding(99, d.chat, 22); return errors.Is(err, sql.ErrNoRows) })
	waitFor(t, func() bool { return d.fake.has(11, "Saved binding deleted.") })
	if _, err := os.Stat(previous.Session); err != nil {
		t.Fatalf("forget removed the native session: %v", err)
	}
	d.fake.mu.Lock()
	for _, message := range d.fake.messages[targetMessages:] {
		if message["message_thread_id"] == float64(22) {
			d.fake.mu.Unlock()
			t.Fatal("restore or forget sent a message to the unavailable topic")
		}
	}
	d.fake.mu.Unlock()
	d.command(11, "/resume "+previous.SessionID)
	waitFor(t, func() bool {
		binding, err := d.db.Binding(99, d.chat, 11)
		return err == nil && binding.Running && binding.SessionID == previous.SessionID && binding.Workspace == workspace
	})
}

func TestBindingDeleteOpenRejectsActiveTask(t *testing.T) {
	fake, db, send := setupBridge(t)
	send(update(1, 22, "/new "+t.TempDir()))
	waitBinding(t, db, 22)
	waitFor(t, func() bool { return fake.has(22, "omp is ready.") })
	send(update(2, 22, "wait"))
	waitFor(t, func() bool {
		var state string
		return db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&state) == nil && state == "submitted"
	})
	before := fake.messageCount()
	send(update(3, 11, "/bindings"))
	listID, data := waitBindingAction(t, fake, before, "Saved bindings", "")
	sendBindingCallback(t, db, send, 4, listID, data)
	confirmID, data := waitBindingAction(t, fake, int(listID), "Close and forget idle binding for Topic 22?", "Delete")
	sendBindingCallback(t, db, send, 5, confirmID, data)
	waitFor(t, func() bool { return fake.has(11, "Binding is busy.") })
	bound, err := db.Binding(99, -10, 22)
	if err != nil || !bound.Running {
		t.Fatalf("active binding was deleted or closed: %+v, %v", bound, err)
	}
}

func TestBindingForgetRejectsStaleOpenGeneration(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	current := store.Binding{Bot: 99, Chat: -10, Thread: 22, Generation: 5, Running: true}
	if err := db.Save(current); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := testBridge(t, &Bridge{db: db, bot: telegram.User{ID: 99}})
	source := b.newWorker(ctx, target{chat: -10, thread: 11}, store.Binding{}, false, nil)
	target := b.newWorker(ctx, target{chat: -10, thread: 22}, current, false, nil)
	target.forgetBinding(bindingForgetRequest{key: target.key, generation: 4, source: source})
	result := <-source.forgetResults
	if result.ok || target.exitRequestedState() {
		t.Fatalf("stale request changed current worker: %+v", result)
	}
	bound, err := db.Binding(99, -10, 22)
	if err != nil || !bound.Running || bound.Generation != current.Generation {
		t.Fatalf("stale request changed binding: %+v, %v", bound, err)
	}
}

func TestBindingForgetRequestReturnsWhenTargetExits(t *testing.T) {
	sourceCtx, stopSource := context.WithCancel(context.Background())
	defer stopSource()
	targetCtx, stopTarget := context.WithCancel(context.Background())
	b := testBridge(t, &Bridge{ctx: sourceCtx, workerExits: make(chan workerExit, 1)})
	source := b.newWorker(sourceCtx, target{chat: -10, thread: 11}, store.Binding{}, false, nil)
	target := b.newWorker(targetCtx, target{chat: -10, thread: 22}, store.Binding{}, false, nil)
	if !target.tryForget(bindingForgetRequest{key: target.key, generation: 1, source: source}) {
		t.Fatal("could not enqueue request before target exit")
	}
	stopTarget()
	b.runWorker(target)
	select {
	case result := <-source.forgetResults:
		if result.ok || result.status == "" {
			t.Fatalf("exited target reported success: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exited target lost the pending deletion request")
	}
	b.wg.Wait()
}
