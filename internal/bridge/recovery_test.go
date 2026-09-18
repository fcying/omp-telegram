package bridge

import (
	"context"
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
}

func newRecoveryDaemon(t *testing.T, maxWorkers int) *recoveryDaemon {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d := &recoveryDaemon{
		t: t, root: t.TempDir(),
		fake: &fakeHTTP{updates: make(chan telegram.Update, 32)},
		cfg:  config.Config{Token: "fake", AllowedUsers: []int64{7}, AllowedChats: []int64{-10}, WorkspaceRoot: t.TempDir(), OMP: exe, DataDir: t.TempDir(), MaxWorkers: maxWorkers, QueueCapacity: 4},
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
	go func() { done <- Run(ctx, cfg, db) }()
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
	d.fake.updates <- update(d.nextID, thread, text)
	return d.nextID
}

func (d *recoveryDaemon) command(thread int64, text string) {
	d.t.Helper()
	waitInputDone(d.t, d.db, d.send(thread, text))
}

func (d *recoveryDaemon) binding(thread int64) store.Binding {
	d.t.Helper()
	b, err := d.db.Binding(99, -10, thread)
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
	queued := d.send(11, "queued-before-restart")
	waitFor(t, func() bool {
		var state string
		return d.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", queued).Scan(&state) == nil && state == "pending"
	})
	d.stop()
	// A changed discovery root makes ID-based resume fail; only the saved
	// absolute native file path can recover these sessions.
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
	d.start()
	d.restored(live)
	d.restored(stopped)
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
	d := newRecoveryDaemon(t, 1)
	d.command(11, "/help")
	d.stop()
	db, err := store.Open(d.root)
	if err != nil {
		t.Fatal(err)
	}
	intent := store.StartIntent{Bot: 99, Chat: -10, Thread: 11, Kind: "new", Workspace: t.TempDir(), Generation: 1}
	if err = db.PrepareStart(store.Binding{Bot: 99, Chat: -10, Thread: 11}, intent); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	d.start()
	waitFor(t, func() bool { return d.fake.has(11, "start was interrupted") })
	intents, err := d.db.PendingStarts(99)
	if err != nil || len(intents) != 1 || intents[0] != intent {
		t.Fatalf("recovered startup intents = %+v, error %v", intents, err)
	}
	d.command(11, "/new "+t.TempDir())
	intents, err = d.db.PendingStarts(99)
	if err != nil || len(intents) != 1 || intents[0] != intent {
		t.Fatalf("new command replaced pending startup intent: %+v, error %v", intents, err)
	}
	d.command(11, "/close")
	intents, err = d.db.PendingStarts(99)
	if err != nil || len(intents) != 0 {
		t.Fatalf("close did not cancel startup intent: %+v, error %v", intents, err)
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

func TestDaemonRecoveryMissingSessionPreservesIntent(t *testing.T) {
	d := newRecoveryDaemon(t, 1)
	d.command(11, "/new "+t.TempDir())
	before := d.binding(11)
	d.stop()
	if err := os.Remove(before.Session); err != nil {
		t.Fatal(err)
	}
	d.start()
	d.command(11, "missing-session-probe")
	if got := d.binding(11); got != before {
		t.Fatalf("failed restoration changed durable intent: before=%+v after=%+v", before, got)
	}
	if _, err := os.Stat(before.Session); !os.IsNotExist(err) {
		t.Fatalf("missing native session was recreated: %v", err)
	}
	if d.fake.has(11, "answer: missing-session-probe") {
		t.Fatal("failed restoration silently started a replacement")
	}
	// The failed attempt must release its process slot.
	d.command(22, "/new "+t.TempDir())
	if !d.binding(22).Running {
		t.Fatal("failed restoration leaked the only process slot")
	}
	bindings, err := d.db.RunningBindings(99)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 {
		t.Fatalf("failed restoration erased intent: %+v", bindings)
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
}
