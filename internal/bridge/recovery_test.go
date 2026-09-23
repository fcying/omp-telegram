package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

// recoveryDaemon keeps one transport installed across complete daemon lifetimes.
// Each restart closes and reopens SQLite, exercising crash-state reconciliation.
type recoveryDaemon struct {
	t      *testing.T
	root   string
	cfg    config.Config
	db     *store.Store
	fake   *fakeHTTP
	cancel context.CancelFunc
	done   chan error
	nextID int64
	chat   int64
}

func newRecoveryDaemon(t *testing.T, maxWorkers int) *recoveryDaemon {
	t.Helper()
	return newRecoveryDaemonForChat(t, maxWorkers, -10)
}

func newRecoveryDaemonForChat(t *testing.T, maxWorkers int, chat int64) *recoveryDaemon {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d := &recoveryDaemon{
		t: t, root: t.TempDir(), chat: chat,
		fake: &fakeHTTP{updates: make(chan telegram.Update, 32)},
		cfg:  config.Config{Token: "fake", AllowedUsers: []int64{7}, AllowedChats: []int64{chat}, WorkspaceRoot: t.TempDir(), OMP: exe, DataDir: t.TempDir(), MaxWorkers: maxWorkers, QueueCapacity: 4},
	}
	old := http.DefaultTransport
	http.DefaultTransport = d.fake
	t.Cleanup(func() {
		defer func() { http.DefaultTransport = old }()
		d.stop()
	})
	d.start()
	return d
}

func (d *recoveryDaemon) start() {
	d.t.Helper()
	var err error
	d.db, err = store.Open(d.root)
	if err != nil {
		d.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.done = make(chan error, 1)
	cfg, db, done := d.cfg, d.db, d.done
	logs := testLogs(d.t)
	go func() { done <- Run(ctx, cfg, db, logs) }()
}

func (d *recoveryDaemon) stop() {
	d.t.Helper()
	if d.cancel == nil {
		return
	}
	d.cancel()
	select {
	case err := <-d.done:
		if err != nil {
			d.t.Error(err)
		}
	case <-time.After(10 * time.Second):
		d.t.Fatal("daemon shutdown timed out")
	}
	d.cancel = nil
	if err := d.db.Close(); err != nil {
		d.t.Fatal(err)
	}
}

func (d *recoveryDaemon) send(thread int64, text string) int64 {
	d.t.Helper()
	d.nextID++
	u := update(d.nextID, thread, text)
	u.Message.Chat.ID = d.chat
	if d.chat > 0 {
		u.Message.Chat.Type = "private"
	}
	d.fake.updates <- u
	return d.nextID
}

func (d *recoveryDaemon) command(thread int64, text string) {
	d.t.Helper()
	waitInputDone(d.t, d.db, d.send(thread, text))
}

func (d *recoveryDaemon) binding(thread int64) store.Binding {
	d.t.Helper()
	b, err := d.db.Binding(99, d.chat, thread)
	if err != nil {
		d.t.Fatal(err)
	}
	return b
}

func (d *recoveryDaemon) state(id int64) string {
	d.t.Helper()
	var state string
	if err := d.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", id).Scan(&state); err != nil {
		d.t.Fatal(err)
	}
	return state
}

func (d *recoveryDaemon) restored(before store.Binding) store.Binding {
	d.t.Helper()
	waitFor(d.t, func() bool {
		b, err := d.db.Binding(before.Bot, before.Chat, before.Thread)
		return err == nil && b.Generation > before.Generation
	})
	after := d.binding(before.Thread)
	if after.Session != before.Session || after.Workspace != before.Workspace || !after.Running {
		d.t.Fatalf("restore changed session identity or lost running intent: before=%+v after=%+v", before, after)
	}
	return after
}

func TestForeignBotCommandsRemainIgnoredAfterRestart(t *testing.T) {
	d := newRecoveryDaemonForChat(t, 1, 7)
	ids := []int64{
		d.send(0, "/status@OtherBot"),
		d.send(0, "/review@OtherBot foo"),
	}
	for _, id := range ids {
		waitFor(t, func() bool {
			var state string
			return d.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", id).Scan(&state) == nil && state == "ignored"
		})
	}
	d.stop()
	d.start()
	// A command to this bot is a polling barrier and must still be accepted.
	d.command(0, "/help@FIXTURE_BOT")
	for _, id := range ids {
		if got := d.state(id); got != "ignored" {
			t.Fatalf("foreign command %d changed after restart: %s", id, got)
		}
	}
	pending, err := d.db.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatalf("foreign commands remain eligible for replay: %v, %v", pending, err)
	}
	var replies int
	if err := d.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&replies); err != nil || replies != 1 {
		t.Fatalf("foreign commands generated replies: count=%d, err=%v", replies, err)
	}
}

func TestDaemonRecoveryPreservesLiveSessionsWithoutReplayingTasks(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
	d := newRecoveryDaemon(t, 3)
	for _, thread := range []int64{11, 22, 33} {
		d.command(thread, "/new "+t.TempDir())
	}
	live, stopped := d.binding(11), d.binding(22)
	if !filepath.IsAbs(live.Session) || !live.Running || !stopped.Running {
		t.Fatal("new sessions did not persist absolute identity and running intent")
	}
	d.command(33, "/close")
	closed := d.binding(33)
	if closed.Running {
		t.Fatal("explicit close retained running intent")
	}
	stopTask := d.send(22, "wait")
	waitFor(t, func() bool {
		var state string
		return d.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", stopTask).Scan(&state) == nil && state == "submitted"
	})
	d.command(22, "/stop")
	if !d.binding(22).Running {
		t.Fatal("stop cleared running intent")
	}
	uncertain := d.send(11, "wait")
	waitFor(t, func() bool {
		var state string
		return d.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", uncertain).Scan(&state) == nil && state == "submitted"
	})
	queued := d.send(11, "/review queued-before-restart")
	waitFor(t, func() bool {
		var state string
		return d.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", queued).Scan(&state) == nil && state == "pending"
	})
	lastUsedBeforeRestart := map[int64]int64{11: d.binding(11).LastUsedAt, 22: d.binding(22).LastUsedAt}
	d.stop()
	// A changed discovery root makes ID-based resume fail; only the saved
	// absolute native file path can recover these sessions.
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
	d.start()
	restoredLive := d.restored(live)
	restoredStopped := d.restored(stopped)
	if restoredLive.LastUsedAt != lastUsedBeforeRestart[11] || restoredStopped.LastUsedAt != lastUsedBeforeRestart[22] {
		t.Fatalf("startup restore changed last-used timestamps: before=%v live=%d stopped=%d", lastUsedBeforeRestart, restoredLive.LastUsedAt, restoredStopped.LastUsedAt)
	}
	waitFor(t, func() bool { return d.fake.has(11, "Gateway restarted while the previous task was active") })
	if d.fake.has(22, "Gateway restarted while the previous task was active") {
		t.Fatal("idle session restoration published an interruption warning")
	}
	if d.binding(11).Interrupted {
		t.Fatal("restored binding retained its interruption marker")
	}
	if state := d.state(uncertain); state != "uncertain" {
		t.Fatalf("interrupted submission became %q", state)
	}
	if state := d.state(queued); state != "cancelled" {
		t.Fatalf("previously queued prompt became %q", state)
	}
	// A replayed wait prompt would hold the fixture busy and prevent this reply.
	for _, before := range []store.Binding{live, stopped} {
		id := d.send(before.Thread, "cwd")
		waitFor(t, func() bool { return d.fake.has(before.Thread, "answer: "+before.Workspace) })
		waitInputDone(t, d.db, id)
	}
	d.command(33, "closed-topic-probe")
	if got := d.binding(33); got != closed || d.fake.has(33, "answer: closed-topic-probe") {
		t.Fatalf("closed topic was revived: %+v", got)
	}
	if state := d.state(uncertain); state != "uncertain" {
		t.Fatalf("uncertain task was replayed: %q", state)
	}
}

func TestDaemonRecoveryRetainsUncommittedStartupIntent(t *testing.T) {
	t.Run("group-topic", func(t *testing.T) { testRecoveryStartupIntent(t, -10, 11) })
	t.Run("private-chat", func(t *testing.T) { testRecoveryStartupIntent(t, 7, 0) })
}

func testRecoveryStartupIntent(t *testing.T, chat, thread int64) {
	t.Helper()
	d := newRecoveryDaemonForChat(t, 1, chat)
	d.command(thread, "/help")
	d.stop()
	db, err := store.Open(d.root)
	if err != nil {
		t.Fatal(err)
	}
	intent := store.StartIntent{Bot: 99, Chat: chat, Thread: thread, Kind: "new", Workspace: t.TempDir(), Generation: 1}
	if err = db.PrepareStart(store.Binding{Bot: 99, Chat: chat, Thread: thread}, intent); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	d.start()
	waitFor(t, func() bool { return d.fake.has(thread, "start was interrupted") })
	intents, err := d.db.PendingStarts(99)
	if err != nil || len(intents) != 1 || intents[0] != intent {
		t.Fatalf("recovered startup intents = %+v, error %v", intents, err)
	}
	d.command(thread, "/new "+t.TempDir())
	intents, err = d.db.PendingStarts(99)
	if err != nil || len(intents) != 1 || intents[0] != intent {
		t.Fatalf("new command replaced pending startup intent: %+v, error %v", intents, err)
	}
	d.command(thread, "/close")
	intents, err = d.db.PendingStarts(99)
	if err != nil || len(intents) != 0 {
		t.Fatalf("close did not cancel startup intent: %+v, error %v", intents, err)
	}
}

func TestDaemonRecoveryPreservesPrivateChatWithoutReplayingTasks(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
	d := newRecoveryDaemonForChat(t, 1, 7)
	d.command(0, "/new "+t.TempDir())
	before := d.binding(0)
	if !filepath.IsAbs(before.Session) || !before.Running {
		t.Fatal("private session did not persist absolute identity and running intent")
	}
	uncertain := d.send(0, "wait")
	waitFor(t, func() bool {
		var state string
		return d.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", uncertain).Scan(&state) == nil && state == "submitted"
	})
	queued := d.send(0, "/review queued-before-restart")
	waitFor(t, func() bool {
		var state string
		return d.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", queued).Scan(&state) == nil && state == "pending"
	})
	d.stop()
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
	d.start()
	d.restored(before)
	waitFor(t, func() bool { return d.fake.has(0, "Gateway restarted while the previous task was active") })
	if state := d.state(uncertain); state != "uncertain" {
		t.Fatalf("interrupted private submission became %q", state)
	}
	if state := d.state(queued); state != "cancelled" {
		t.Fatalf("queued private prompt became %q", state)
	}
	id := d.send(0, "cwd")
	waitFor(t, func() bool { return d.fake.has(0, "answer: "+before.Workspace) })
	waitInputDone(t, d.db, id)
}

func TestRecoverySkipsNonTopicGroupsAndUnauthorizedPrivateChats(t *testing.T) {
	for _, chat := range []int64{-10, 0, 7} {
		for _, startup := range []bool{false, true} {
			db, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			binding := store.Binding{Bot: 99, Chat: chat, Workspace: t.TempDir(), Session: "/missing/session.jsonl", SessionID: "deadbeef", Generation: 1, Running: true}
			if startup {
				intent := store.StartIntent{Bot: 99, Chat: chat, Kind: "new", Workspace: binding.Workspace, Generation: 1}
				err = db.PrepareStart(store.Binding{Bot: 99, Chat: chat}, intent)
			} else {
				err = db.Save(binding)
			}
			if err != nil {
				t.Fatal(err)
			}
			b := testBridge(t, &Bridge{cfg: config.Config{AllowedChats: []int64{-10, 0}, QueueCapacity: 4},
				db: db, bot: telegram.User{ID: 99}, tg: newTestTelegram(t),
				slots: make(chan struct{}, 1), fatal: make(chan error, 1)})
			ctx, cancel := context.WithCancel(context.Background())
			workers := make(map[target]*worker)
			err = b.restoreWorkers(ctx, workers)
			cancel()
			b.wg.Wait()
			if closeErr := db.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(workers) != 0 {
				t.Fatalf("recovered unsupported or unauthorized chat %d (startup=%v)", chat, startup)
			}
		}
	}
}

func TestDaemonRecoverySkipsRemovedChatAuthorization(t *testing.T) {
	d := newRecoveryDaemon(t, 1)
	d.command(11, "/new "+t.TempDir())
	before := d.binding(11)
	d.stop()
	d.cfg.AllowedChats = []int64{-20}
	d.start()
	// Processing an authorized command provides a startup barrier without
	// requesting or allocating an instance in the removed chat.
	d.nextID++
	u := update(d.nextID, 22, "/help")
	u.Message.Chat.ID = -20
	d.fake.updates <- u
	waitInputDone(t, d.db, d.nextID)
	if got := d.binding(11); got != before {
		t.Fatalf("unauthorized saved topic was changed: before=%+v after=%+v", before, got)
	}
}

func TestDaemonRecoverySkipsMissingSessionWithoutReplacement(t *testing.T) {
	d := newRecoveryDaemon(t, 1)
	d.command(11, "/new "+t.TempDir())
	before := d.binding(11)
	d.stop()
	if err := os.Remove(before.Session); err != nil {
		t.Fatal(err)
	}
	d.start()
	waitFor(t, func() bool { return !d.binding(11).Running })
	waitFor(t, func() bool { return d.fake.has(11, "startup restore was skipped") })
	after := d.binding(11)
	if after.Session != before.Session || after.Workspace != before.Workspace || after.Generation != before.Generation || after.Running {
		t.Fatalf("skipped restoration changed the saved binding: before=%+v after=%+v", before, after)
	}
	d.command(11, "missing-session-probe")
	if d.fake.has(11, "answer: missing-session-probe") {
		t.Fatal("missing session silently started a replacement")
	}
	if _, err := os.Stat(before.Session); !os.IsNotExist(err) {
		t.Fatalf("missing native session was recreated: %v", err)
	}
	// Closing the unavailable runtime must release the only process slot.
	d.command(22, "/new "+t.TempDir())
	if !d.binding(22).Running {
		t.Fatal("skipped restoration leaked the only process slot")
	}
	bindings, err := d.db.RunningBindings(99)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].Thread != 22 {
		t.Fatalf("skipped restoration left an ineligible binding running: %+v", bindings)
	}
}

func TestDaemonRecoveryHonorsReducedWorkerLimit(t *testing.T) {
	d := newRecoveryDaemon(t, 2)
	d.command(11, "/new "+t.TempDir())
	d.command(22, "/new "+t.TempDir())
	before := []store.Binding{d.binding(11), d.binding(22)}
	d.stop()
	d.cfg.MaxWorkers = 1
	d.start()
	waitFor(t, func() bool {
		restored := 0
		blocked := 0
		for _, b := range before {
			after := d.binding(b.Thread)
			switch {
			case after.Generation > b.Generation:
				restored++
			case after.Generation == b.Generation:
				blocked++
			}
		}
		return restored == 1 && blocked == 1
	})
	var blocked store.Binding
	for _, b := range before {
		after := d.binding(b.Thread)
		if after.Generation == b.Generation {
			blocked = b
			break
		}
	}
	if blocked.SessionID == "" {
		t.Fatal("could not identify the binding blocked by worker capacity")
	}
	d.command(blocked.Thread, "/status")
	waitFor(t, func() bool { return d.fake.has(blocked.Thread, "OMP: released") })
	if !d.fake.has(blocked.Thread, "Idle: n/a") {
		t.Fatal("capacity-blocked released status omitted unknown idle state")
	}
	// Each probe runs after its topic's automatic restoration attempt.
	for _, b := range before {
		d.command(b.Thread, "capacity-probe")
	}
	waitFor(t, func() bool {
		return d.fake.has(11, "answer: capacity-probe") || d.fake.has(22, "answer: capacity-probe")
	})
	restored, answered := 0, 0
	for _, b := range before {
		after := d.binding(b.Thread)
		if after.Session != b.Session || after.Workspace != b.Workspace || !after.Running {
			t.Fatalf("capacity pressure discarded saved identity or intent: %+v", after)
		}
		if after.Generation > b.Generation {
			restored++
		}
		if d.fake.has(b.Thread, "answer: capacity-probe") {
			answered++
		}
	}
	if restored != 1 || answered != 1 {
		t.Fatalf("worker cap: restored=%d responding=%d, want one each", restored, answered)
	}
	d.command(33, "/resume "+blocked.SessionID)
	if _, err := d.db.Binding(99, d.chat, 33); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("resume of a capacity-blocked logical session created a binding: %v", err)
	}
}

func TestDaemonShutdownCancelsPendingQueue(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
	orderPath := filepath.Join(t.TempDir(), "rpc-order")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_ORDER", orderPath)
	d := newRecoveryDaemon(t, 1)
	gate := make(chan struct{})
	photoData, documentData := []byte("fake image"), []byte("document")
	d.fake.mu.Lock()
	d.fake.files = map[string][]byte{"shutdown-photo": photoData, "shutdown-document": documentData}
	d.fake.downloadGate = gate
	d.fake.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
		d.fake.mu.Lock()
		d.fake.downloadGate = nil
		d.fake.mu.Unlock()
	})
	d.command(11, "/new "+t.TempDir())
	active := d.send(11, "wait")
	waitFor(t, func() bool {
		var state string
		return d.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", active).Scan(&state) == nil && state == "submitted"
	})
	queuedText := d.send(11, "queued text before shutdown")
	queuedReview := d.send(11, "/review queued-before-shutdown")
	sendAttachment := func(fileID string, photo bool) int64 {
		d.nextID++
		u := update(d.nextID, 11, "")
		if photo {
			u.Message.Photo = []telegram.PhotoSize{{FileID: fileID, Width: 1, Height: 1, FileSize: int64(len(photoData))}}
		} else {
			u.Message.Document = &telegram.Document{FileID: fileID, FileName: "queued.txt", MimeType: "text/plain", FileSize: int64(len(documentData))}
		}
		d.fake.updates <- u
		return d.nextID
	}
	queuedPhoto := sendAttachment("shutdown-photo", true)
	queuedDocument := sendAttachment("shutdown-document", false)
	queued := []int64{queuedText, queuedReview, queuedPhoto, queuedDocument}
	waitFor(t, func() bool {
		for _, id := range queued {
			var state string
			if err := d.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", id).Scan(&state); err != nil || state != "pending" {
				return false
			}
		}
		d.fake.mu.Lock()
		defer d.fake.mu.Unlock()
		return d.fake.fileRequests == 2 && d.fake.downloadRequests == 2
	})
	d.stop()
	close(gate)
	d.fake.mu.Lock()
	d.fake.downloadGate = nil
	d.fake.mu.Unlock()
	d.start()
	if state := d.state(active); state != "uncertain" {
		t.Fatalf("active input after restart = %q, want uncertain", state)
	}
	for _, id := range queued {
		if state := d.state(id); state != "cancelled" {
			t.Errorf("queued input %d after restart = %q, want cancelled", id, state)
		}
	}
	d.command(11, "/status")
	rpcOrder, err := os.ReadFile(orderPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(rpcOrder) != "prompt\n" {
		t.Fatalf("prompt RPCs after shutdown and restart = %q, want only the original active prompt", rpcOrder)
	}
	d.fake.mu.Lock()
	fileRequests, downloadRequests := d.fake.fileRequests, d.fake.downloadRequests
	d.fake.mu.Unlock()
	if fileRequests != 2 || downloadRequests != 2 {
		t.Fatalf("attachment downloads after restart = metadata:%d download:%d, want no replay after the two original downloads", fileRequests, downloadRequests)
	}
}

func TestPendingAlbumIsCancelledOnRecovery(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := update(1, 11, "")
	owner.Message.MediaGroupID = "album-recovery"
	owner.Message.Photo = []telegram.PhotoSize{{FileID: "photo-a", Width: 8, Height: 8}}
	member := update(2, 11, "")
	member.Message.MediaGroupID = "album-recovery"
	member.Message.Photo = []telegram.PhotoSize{{FileID: "photo-b", Width: 8, Height: 8}}
	for _, u := range []telegram.Update{owner, member} {
		raw, marshalErr := json.Marshal(u)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if err := db.Accept(u.UpdateID, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Mark(2, "done"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := testBridge(t, &Bridge{db: db, bot: telegram.User{ID: 99}, cfg: config.Config{AllowedChats: []int64{-10}}})
	workers := make(map[target]*worker)
	if err := b.restoreWorkers(ctx, workers); err != nil {
		t.Fatal(err)
	}
	var ownerState, memberState string
	if err := db.DB.QueryRow("SELECT state FROM inbox WHERE id=1").Scan(&ownerState); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&memberState); err != nil {
		t.Fatal(err)
	}
	if ownerState != "cancelled" || memberState != "done" {
		t.Fatalf("recovered album states = owner:%q member:%q", ownerState, memberState)
	}
}
