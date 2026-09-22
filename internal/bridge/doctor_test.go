package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func TestDoctorChecksReportSafeState(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	session := filepath.Join(workspace, "session.jsonl")
	if err := os.WriteFile(session, []byte("session"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	binding := doctorBindingSnapshot{
		exists:     true,
		generation: 3,
		workspace:  workspace,
		session:    session,
		sessionID:  "12345678-1234-4234-8234-123456789abc",
	}
	oldGetMe, oldRunOMP, oldStatfs := doctorGetMe, doctorRunOMP, doctorStatfs
	t.Cleanup(func() {
		doctorGetMe, doctorRunOMP, doctorStatfs = oldGetMe, oldRunOMP, oldStatfs
	})
	doctorGetMe = func(context.Context, *telegram.Client) error { return nil }
	doctorRunOMP = func(context.Context, string) error { return nil }
	doctorStatfs = func(string) (uint64, error) { return 2 << 30, nil }

	checks := runDoctorChecks(context.Background(), config.Config{
		OMP:           "/usr/bin/omp",
		DataDir:       dataDir,
		MaxWorkers:    2,
		QueueCapacity: 4,
		ProgressMode:  "summary",
	}, newTestTelegram(t), db, binding, runtimeReleased)
	for _, check := range checks {
		if check.Level != doctorOK {
			t.Fatalf("check %s = %#v, want OK", check.Name, check)
		}
	}
	report := formatDoctorReport(checks)
	if !strings.Contains(report, "omp-telegram diagnostics") || !strings.Contains(report, "Result: OK") {
		t.Fatalf("unexpected report: %q", report)
	}
	if strings.Contains(report, dataDir) || strings.Contains(report, workspace) || strings.Contains(report, session) {
		t.Fatalf("report leaked a path: %q", report)
	}
	if !strings.Contains(report, "[12345678]") {
		t.Fatalf("report omitted the safe session identifier: %q", report)
	}
}

func TestDoctorReportsUncertainRecords(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Accept(1, []byte(`{"update_id":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.Mark(1, "submitted"); err != nil {
		t.Fatal(err)
	}
	if err := db.Mark(1, "uncertain"); err != nil {
		t.Fatal(err)
	}
	if err := db.Enqueue(1, 2, "uncertain output"); err != nil {
		t.Fatal(err)
	}
	output, err := db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkOutput(output.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkOutput(output.ID, "uncertain"); err != nil {
		t.Fatal(err)
	}
	inbox, outbox := checkDoctorUncertain(context.Background(), db)
	if inbox.Level != doctorWarn || inbox.Message != "1 uncertain" {
		t.Fatalf("inbox check = %#v", inbox)
	}
	if outbox.Level != doctorWarn || outbox.Message != "1 uncertain" {
		t.Fatalf("outbox check = %#v", outbox)
	}
}

func TestDoctorRunsAsyncRejectsDuplicateAndFencesRuntime(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dataDir := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	oldGetMe, oldRunOMP, oldStatfs := doctorGetMe, doctorRunOMP, doctorStatfs
	t.Cleanup(func() {
		doctorGetMe, doctorRunOMP, doctorStatfs = oldGetMe, oldRunOMP, oldStatfs
	})
	doctorGetMe = func(ctx context.Context, _ *telegram.Client) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	doctorRunOMP = func(context.Context, string) error { return nil }
	doctorStatfs = func(string) (uint64, error) { return 2 << 30, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := testWorker(t, &worker{
		b: testBridge(t, &Bridge{
			cfg: config.Config{OMP: "fixture", DataDir: dataDir, MaxWorkers: 1, QueueCapacity: 1, ProgressMode: "summary"},
			db:  db,
			tg:  newTestTelegram(t),
			bot: telegram.User{ID: 99},
		}),
		key:     target{chat: 1, thread: 2},
		ctx:     ctx,
		cancel:  cancel,
		runtime: runtimeReleased,
	})
	w.runDoctor()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("doctor did not start asynchronously")
	}
	w.runDoctor()
	duplicate, err := db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Text != "Diagnostics are already running." {
		t.Fatalf("duplicate response = %q", duplicate.Text)
	}
	if err := db.MarkOutput(duplicate.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkOutput(duplicate.ID, "done"); err != nil {
		t.Fatal(err)
	}
	close(release)
	var result doctorResult
	select {
	case result = <-w.doctorResults:
	case <-time.After(2 * time.Second):
		t.Fatal("doctor did not finish")
	}
	w.runtime = runtimeConnected
	w.doctorFinished(result)
	stale, err := db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if stale.Text != "State changed during diagnostics. Run /doctor again." {
		t.Fatalf("stale response = %q", stale.Text)
	}
	w.teardownWorker(true)
	w.background.Wait()
}

func TestDoctorFreshConnectedSessionIsWarning(t *testing.T) {
	binding := doctorBindingSnapshot{
		exists:    true,
		session:   filepath.Join(t.TempDir(), "not-persisted.jsonl"),
		sessionID: "12345678-1234-4234-8234-123456789abc",
		running:   true,
	}
	connected := checkDoctorSession(binding, runtimeConnected)
	if connected.Level != doctorWarn || connected.Message != "session history not persisted yet" {
		t.Fatalf("connected fresh session = %#v", connected)
	}
	released := checkDoctorSession(binding, runtimeReleased)
	if released.Level != doctorFail || released.Message != "released session file unavailable" {
		t.Fatalf("released fresh session = %#v", released)
	}
	binding.running = false
	closed := checkDoctorSession(binding, runtimeReleased)
	if closed.Level != doctorWarn || closed.Message != "saved session file is unavailable" {
		t.Fatalf("closed missing session = %#v", closed)
	}
}

func TestDoctorSnapshotsBindingBeforeAsyncWork(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workspace := t.TempDir()
	first := store.Binding{
		Bot: 99, Chat: 1, Thread: 2, Workspace: workspace,
		Session: filepath.Join(workspace, "first.jsonl"), SessionID: "11111111-1111-4111-8111-111111111111", Generation: 1,
	}
	if err := db.Save(first); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	oldGetMe, oldRunOMP, oldStatfs := doctorGetMe, doctorRunOMP, doctorStatfs
	t.Cleanup(func() {
		doctorGetMe, doctorRunOMP, doctorStatfs = oldGetMe, oldRunOMP, oldStatfs
	})
	doctorGetMe = func(ctx context.Context, _ *telegram.Client) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	doctorRunOMP = func(context.Context, string) error { return nil }
	doctorStatfs = func(string) (uint64, error) { return 2 << 30, nil }

	ctx, cancel := context.WithCancel(context.Background())
	w := testWorker(t, &worker{
		b: testBridge(t, &Bridge{
			cfg: config.Config{OMP: "fixture", DataDir: t.TempDir(), MaxWorkers: 1, QueueCapacity: 1, ProgressMode: "summary"},
			db:  db,
			tg:  newTestTelegram(t),
			bot: telegram.User{ID: 99},
		}),
		key: target{chat: 1, thread: 2}, ctx: ctx, cancel: cancel,
	})
	t.Cleanup(func() {
		w.teardownWorker(true)
		cancel()
		w.background.Wait()
	})
	w.runDoctor()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("doctor did not reach asynchronous checks")
	}
	second := first
	second.Generation = 2
	second.SessionID = "22222222-2222-4222-8222-222222222222"
	if err := db.Save(second); err != nil {
		t.Fatal(err)
	}
	close(release)
	var result doctorResult
	select {
	case result = <-w.doctorResults:
	case <-time.After(2 * time.Second):
		t.Fatal("doctor did not finish")
	}
	w.doctorFinished(result)
	output, err := db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if output.Text != "State changed during diagnostics. Run /doctor again." {
		t.Fatalf("binding replacement response = %q", output.Text)
	}
}

func TestDoctorTimeoutCanRetry(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	oldGetMe, oldRunOMP, oldStatfs, oldTimeout := doctorGetMe, doctorRunOMP, doctorStatfs, doctorTimeout
	t.Cleanup(func() {
		doctorGetMe, doctorRunOMP, doctorStatfs, doctorTimeout = oldGetMe, oldRunOMP, oldStatfs, oldTimeout
	})
	doctorTimeout = 25 * time.Millisecond
	doctorGetMe = func(ctx context.Context, _ *telegram.Client) error {
		<-ctx.Done()
		return ctx.Err()
	}
	doctorRunOMP = func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	doctorStatfs = func(string) (uint64, error) { return 2 << 30, nil }

	ctx, cancel := context.WithCancel(context.Background())
	w := testWorker(t, &worker{
		b: testBridge(t, &Bridge{
			cfg: config.Config{OMP: "fixture", DataDir: t.TempDir(), MaxWorkers: 1, QueueCapacity: 1, ProgressMode: "summary"},
			db:  db,
			tg:  newTestTelegram(t),
			bot: telegram.User{ID: 99},
		}),
		key: target{chat: 1, thread: 2}, ctx: ctx, cancel: cancel,
	})
	t.Cleanup(func() {
		w.teardownWorker(true)
		cancel()
		w.background.Wait()
	})
	w.runDoctor()
	var result doctorResult
	select {
	case result = <-w.doctorResults:
	case <-time.After(time.Second):
		t.Fatal("timed-out doctor did not return a result")
	}
	w.doctorFinished(result)
	if w.doctorCancel != nil {
		t.Fatal("timed-out doctor kept its cancellation handle")
	}
	timeoutOutput, err := db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if timeoutOutput.Text != "Diagnostics timed out. Run /doctor again." {
		t.Fatalf("timeout response = %q", timeoutOutput.Text)
	}
	if err := db.MarkOutput(timeoutOutput.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkOutput(timeoutOutput.ID, "done"); err != nil {
		t.Fatal(err)
	}

	doctorTimeout = time.Second
	doctorGetMe = func(context.Context, *telegram.Client) error { return nil }
	doctorRunOMP = func(context.Context, string) error { return nil }
	w.runDoctor()
	select {
	case result = <-w.doctorResults:
	case <-time.After(time.Second):
		t.Fatal("doctor could not be retried after timeout")
	}
	if result.err != nil {
		t.Fatalf("retry result error = %v", result.err)
	}
	w.doctorFinished(result)
	retryOutput, err := db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(retryOutput.Text, "omp-telegram diagnostics") {
		t.Fatalf("retry response = %q", retryOutput.Text)
	}
}

func TestStoreQuickCheckHonorsCanceledContext(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.QuickCheck(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("QuickCheck canceled error = %v", err)
	}
}

func TestDoctorStopsChecksAfterCancellation(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	oldGetMe, oldRunOMP, oldStatfs := doctorGetMe, doctorRunOMP, doctorStatfs
	t.Cleanup(func() {
		doctorGetMe, doctorRunOMP, doctorStatfs = oldGetMe, oldRunOMP, oldStatfs
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doctorGetMe = func(context.Context, *telegram.Client) error {
		cancel()
		return nil
	}
	doctorRunOMP = func(context.Context, string) error {
		t.Fatal("doctor continued after cancellation")
		return nil
	}
	doctorStatfs = func(string) (uint64, error) {
		t.Fatal("doctor reached filesystem checks after cancellation")
		return 0, nil
	}
	checks := runDoctorChecks(ctx, config.Config{OMP: "fixture", DataDir: t.TempDir(), MaxWorkers: 1, QueueCapacity: 1, ProgressMode: "summary"}, newTestTelegram(t), db, doctorBindingSnapshot{}, runtimeReleased)
	if len(checks) != 1 || checks[0].Name != "Config" {
		t.Fatalf("checks after cancellation = %#v", checks)
	}
}
