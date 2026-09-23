package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"omp-telegram/internal/media"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func finishExport(t *testing.T, w *worker) exportResult {
	t.Helper()
	select {
	case result := <-w.exportResults:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("session export did not finish")
		return exportResult{}
	}
}

func TestExportSessionQueuesPrivateSnapshotAfterClose(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	before := w.binding
	raw := []byte("{\"native\":true}\n")
	if err := os.WriteFile(before.Session, raw, 0600); err != nil {
		t.Fatal(err)
	}
	command("/close")
	command("/export " + before.SessionID)
	if len(w.confirms) != 0 {
		t.Fatal("direct export opened a picker")
	}
	w.exportFinished(finishExport(t, w))
	var kind, path, name string
	var outboxChat, outboxThread, replyTo int64
	if err := w.b.db.DB.QueryRow("SELECT kind,path,name,chat,thread,reply_to FROM outbox WHERE kind='document' ORDER BY id DESC LIMIT 1").Scan(&kind, &path, &name, &outboxChat, &outboxThread, &replyTo); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(raw) {
		t.Fatalf("queued session export = %q, error = %v", data, err)
	}
	if path == before.Session || kind != "document" || name != filepath.Base(before.Session) || outboxChat != w.key.chat || outboxThread != w.key.thread || replyTo != 0 {
		t.Fatalf("queued session export metadata = path %q, kind %q, name %q, chat %d, thread %d, reply_to %d", path, kind, name, outboxChat, outboxThread, replyTo)
	}
	if data, err := os.ReadFile(before.Session); err != nil || string(data) != string(raw) {
		t.Fatalf("native session file was modified or removed: %q, %v", data, err)
	}
	if after, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread); err != nil || after.LastUsedAt != before.LastUsedAt {
		t.Fatalf("export changed binding last-used metadata: before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestExportCommandStaysControlFlow(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new test")
	setResumeFixtures(t, w.binding.Workspace, 1)
	command("/export")
	w.resumeListed(finishResumeList(t, w))
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox ORDER BY id DESC LIMIT 1").Scan(&state); err != nil || state != "done" {
		t.Fatalf("export command state=%q, error=%v", state, err)
	}
	if w.active != 0 || w.busy || len(w.queue) != 0 {
		t.Fatal("export command started or queued a prompt")
	}
}

func TestParseExportArgs(t *testing.T) {
	for _, tc := range []struct {
		arg, format, sessionID string
		ok                     bool
	}{
		{arg: "", format: "session", ok: true},
		{arg: "html", format: "html", ok: true},
		{arg: "abcd1234", format: "session", sessionID: "abcd1234", ok: true},
		{arg: "html abcd1234", format: "html", sessionID: "abcd1234", ok: true},
		{arg: "raw", ok: false},
		{arg: "session abcd1234", ok: false},
	} {
		format, sessionID, ok := parseExportArgs(tc.arg)
		if format != tc.format || sessionID != tc.sessionID || ok != tc.ok {
			t.Errorf("parseExportArgs(%q)=(%q,%q,%t), want (%q,%q,%t)", tc.arg, format, sessionID, ok, tc.format, tc.sessionID, tc.ok)
		}
	}
}

func TestSnapshotSessionBoundsAndProtectsSource(t *testing.T) {
	root, spool := t.TempDir(), t.TempDir()
	source := filepath.Join(root, "2026-09-20T12-00-00-000Z_1234abcd.jsonl")
	raw := []byte("{\"native\":true}\n")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotSession(context.Background(), spool, source)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(snapshot.Path)
	if snapshot.Name != filepath.Base(source) || snapshot.Kind != "document" {
		t.Fatalf("snapshot metadata = %#v", snapshot)
	}
	if data, readErr := os.ReadFile(snapshot.Path); readErr != nil || string(data) != string(raw) {
		t.Fatalf("snapshot bytes = %q, error = %v", data, readErr)
	}
	if info, statErr := os.Stat(snapshot.Path); statErr != nil || info.Mode().Perm() != 0400 {
		t.Fatalf("snapshot mode = %v, error = %v", info.Mode().Perm(), statErr)
	}
	updated := []byte("{\"native\":false}\n")
	if err := os.WriteFile(source, updated, 0600); err != nil {
		t.Fatal(err)
	}
	if data, readErr := os.ReadFile(snapshot.Path); readErr != nil || string(data) != string(raw) {
		t.Fatalf("snapshot changed after source mutation = %q, error = %v", data, readErr)
	}
	if data, readErr := os.ReadFile(source); readErr != nil || string(data) != string(updated) {
		t.Fatalf("source mutation = %q, error = %v", data, readErr)
	}
	large := filepath.Join(root, "large.jsonl")
	file, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(media.MaxDocumentBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = SnapshotSession(context.Background(), spool, large); !errors.Is(err, errSessionExportTooLarge) {
		t.Fatalf("oversized snapshot error = %v", err)
	}
}

func TestSnapshotSessionRequiresSpoolDirectorySync(t *testing.T) {
	root, spool := t.TempDir(), t.TempDir()
	source := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(source, []byte("native\n"), 0600); err != nil {
		t.Fatal(err)
	}
	syncErr := errors.New("directory sync failed")
	oldSync := exportDirectorySync
	t.Cleanup(func() { exportDirectorySync = oldSync })
	calls := 0
	exportDirectorySync = func(path string) error {
		calls++
		if path != spool {
			t.Errorf("directory sync path = %q, want %q", path, spool)
		}
		return syncErr
	}
	if _, err := SnapshotSession(context.Background(), spool, source); !errors.Is(err, syncErr) {
		t.Fatalf("snapshot sync failure = %v, want %v", err, syncErr)
	}
	if calls != 1 {
		t.Fatalf("directory sync calls = %d, want 1", calls)
	}
	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed snapshot left unowned spool entries: %v", entries)
	}
}

func TestSnapshotSessionRejectsNonRegularFiles(t *testing.T) {
	root, spool := t.TempDir(), t.TempDir()
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	paths := []string{directory, fifo}
	if _, err := os.Stat("/dev/null"); err == nil {
		paths = append(paths, "/dev/null")
	}
	for _, path := range paths {
		if _, err := SnapshotSession(context.Background(), spool, path); err == nil {
			t.Fatalf("snapshot accepted non-regular file %q", path)
		}
	}
}

func TestSnapshotSessionRejectsSymlinkedSource(t *testing.T) {
	root, spool := t.TempDir(), t.TempDir()
	target := filepath.Join(root, "session.jsonl")
	link := filepath.Join(root, "link.jsonl")
	if err := os.WriteFile(target, []byte("native"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotSession(context.Background(), spool, link); err == nil {
		t.Fatal("snapshot accepted a symlinked session source")
	}
}

func TestExportReportsOversizedSession(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new test")
	file, err := os.OpenFile(w.binding.Session, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(media.MaxDocumentBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	w.beginExport(omp.SessionSummary{ID: w.sessionID, CWD: w.binding.Workspace}, "session", w.binding.Workspace, w.binding.Generation)
	result := finishExport(t, w)
	if !errors.Is(result.err, errSessionExportTooLarge) {
		t.Fatalf("oversized export result = %v", result.err)
	}
	w.exportFinished(result)
	var text string
	if err = w.b.db.DB.QueryRow("SELECT text FROM outbox WHERE kind='text' ORDER BY id DESC LIMIT 1").Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "Session export is too large to send through Telegram." {
		t.Fatalf("oversized export reply = %q", text)
	}
}

func TestExportUsesCommittedPathAfterIdleRelease(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	before := w.binding
	raw := []byte("{\"released\":true}\n")
	if err := os.WriteFile(before.Session, raw, 0600); err != nil {
		t.Fatal(err)
	}
	w.releaseRuntime(true)
	command("/export " + before.SessionID)
	w.b.cfg.OMP = filepath.Join(t.TempDir(), "resolver-must-not-run")
	w.exportFinished(finishExport(t, w))
	var path string
	if err := w.b.db.DB.QueryRow("SELECT path FROM outbox WHERE kind='document' ORDER BY id DESC LIMIT 1").Scan(&path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(raw) {
		t.Fatalf("released-session export = %q, error=%v", data, err)
	}
}

func TestExportLoadsClosedBindingForFreshWorker(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	before := w.binding
	command("/close")
	setResumeFixtures(t, before.Workspace, 1)

	ctx, cancel := context.WithCancel(w.ctx)
	fresh := w.b.newWorker(ctx, w.key, store.Binding{}, false, nil)
	fresh.initResumePicker()
	t.Cleanup(func() {
		fresh.teardownWorker(true)
		cancel()
		fresh.background.Wait()
	})
	fresh.requestSessionList(7, "export", "session")
	fresh.resumeListed(finishResumeList(t, fresh))
	if fresh.client != nil || fresh.binding.Generation != before.Generation || fresh.binding.Session != before.Session || len(fresh.confirms) != 1 {
		t.Fatalf("fresh export worker state = client=%v binding=%+v confirmations=%d", fresh.client != nil, fresh.binding, len(fresh.confirms))
	}
	fresh.cancelResumeList()
	fresh.b.cfg.OMP = filepath.Join(t.TempDir(), "resolver-must-not-run")
	fresh.requestDirectExport(7, "session", before.SessionID)
	fresh.exportFinished(finishExport(t, fresh))
	var path string
	if err := w.b.db.DB.QueryRow("SELECT path FROM outbox WHERE kind='document' ORDER BY id DESC LIMIT 1").Scan(&path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatalf("fresh-worker export snapshot = %q, error=%v", data, err)
	}
}

func TestExportPickerShowsFormatAndCurrentWorkspace(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	before := w.binding
	sessions := setResumeFixtures(t, before.Workspace, 1)
	command("/export html")
	w.resumeListed(finishResumeList(t, w))
	if len(w.confirms) != 1 {
		t.Fatal("export command did not publish one picker")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var found bool
	for _, message := range f.messages {
		text, _ := message["text"].(string)
		if len(text) > 0 && containsAll(text, "Export omp session", "Format: HTML", sessions[0].ID, before.Workspace) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("export picker omitted format, workspace, or native session identity")
	}
}

func TestExportPickerSurvivesTaskCompletionCleanup(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	setResumeFixtures(t, w.binding.Workspace, 1)
	w.busy = true
	command("/export")
	w.resumeListed(finishResumeList(t, w))
	w.busy = false
	w.clearTaskConfirmations()
	buttons := resumeButtons(t, f)
	if len(buttons) == 0 {
		t.Fatal("export picker disappeared when the task completed")
	}
	clickResume(w, 7, buttons[0]["callback_data"].(string))
	w.exportFinished(finishExport(t, w))
	var documents int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE kind='document'").Scan(&documents); err != nil || documents != 1 {
		t.Fatalf("surviving export picker enqueued %d documents, error=%v", documents, err)
	}
}

func TestExportRequiresCommittedWorkspace(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/export")
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil || text != "No working directory is selected. Use /new <name or path>, or /resume first." {
		t.Fatalf("unbound export reply = %q, error=%v", text, err)
	}
	if w.client != nil {
		t.Fatal("unbound export started a runtime")
	}
}

func TestExportRejectsUnusableWorkspace(t *testing.T) {
	for _, scenario := range []string{"relative", "missing", "file"} {
		t.Run(scenario, func(t *testing.T) {
			w, _, command := setupWorkspaceWorker(t)
			command("/new test")
			workspace := w.binding.Workspace
			if scenario == "relative" {
				if _, err := w.b.db.DB.Exec("UPDATE bindings SET workspace=? WHERE bot=? AND chat=? AND thread=?", "relative", w.b.bot.ID, w.key.chat, w.key.thread); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "missing" {
				if err := os.RemoveAll(workspace); err != nil {
					t.Fatal(err)
				}
			} else {
				file := filepath.Join(t.TempDir(), "workspace-file")
				if err := os.WriteFile(file, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := w.b.db.DB.Exec("UPDATE bindings SET workspace=? WHERE bot=? AND chat=? AND thread=?", file, w.b.bot.ID, w.key.chat, w.key.thread); err != nil {
					t.Fatal(err)
				}
			}
			command("/export")
			var text string
			if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil || text != "The working directory is unavailable. No sessions were loaded." {
				t.Fatalf("%s workspace reply = %q, error=%v", scenario, text, err)
			}
		})
	}
}

func TestExportRejectsConcurrentJob(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	exporter := filepath.Join(t.TempDir(), "omp-export")
	script := "#!/bin/sh\n" +
		"sleep 1\n" +
		"printf '%s' '<html>native</html>' > \"$3\"\n"
	if err := os.WriteFile(exporter, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	w.b.cfg.OMP = exporter
	command("/export html " + w.sessionID)
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox ORDER BY id DESC LIMIT 1").Scan(&state); err != nil || state != "done" {
		t.Fatalf("async export command state=%q, error=%v", state, err)
	}
	if w.exportCancel == nil {
		t.Fatal("first export did not reserve the worker")
	}
	w.beginExport(omp.SessionSummary{ID: w.sessionID, CWD: w.binding.Workspace}, "html", w.binding.Workspace, w.binding.Generation)
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil || text != "The omp session operation is still loading." {
		t.Fatalf("concurrent export reply = %q, error=%v", text, err)
	}
	w.cancelExport()
	w.exportFinished(finishExport(t, w))
}

func TestExportTeardownCancelsAndCleansSnapshot(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	exporter := filepath.Join(t.TempDir(), "omp-export")
	if err := os.WriteFile(exporter, []byte("#!/bin/sh\nsleep 1\nprintf '%s' '<html>native</html>' > \"$3\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	w.b.cfg.OMP = exporter
	w.beginExport(omp.SessionSummary{ID: w.sessionID, CWD: w.binding.Workspace}, "html", w.binding.Workspace, w.binding.Generation)
	if w.exportCancel == nil {
		t.Fatal("export did not start")
	}
	w.teardownWorker(true)
	w.background.Wait()
	w.drainExportResults()
	w.b.sessionMu.Lock()
	_, reserved := w.b.exportClaims[w]
	w.b.sessionMu.Unlock()
	if reserved {
		t.Fatal("teardown left an export reservation")
	}
	spool := filepath.Join(w.b.cfg.DataDir, "attachments", "outbox")
	entries, err := os.ReadDir(spool)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("teardown left export snapshots: %v", entries)
	}
}

func TestStaleExportResultCleansSnapshot(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new test")
	path := filepath.Join(t.TempDir(), "snapshot.jsonl")
	if err := os.WriteFile(path, []byte("snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	_, cancel := context.WithCancel(context.Background())
	w.exportCancel = cancel
	w.exportRequest = 7
	generation := w.binding.Generation
	w.binding.Generation++
	w.exportFinished(exportResult{request: 7, generation: generation, file: media.File{Path: path, Name: "snapshot.jsonl", Kind: "document"}})
	w.cancelExport()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale export snapshot still exists, error=%v", err)
	}
	var documents int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE kind='document'").Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if documents != 0 {
		t.Fatalf("stale export enqueued %d documents", documents)
	}
}

func TestExportResultRequiresPersistedBindingGeneration(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	w.beginExport(omp.SessionSummary{ID: w.sessionID, CWD: w.binding.Workspace}, "session", w.binding.Workspace, w.binding.Generation)
	result := finishExport(t, w)
	if _, err := w.b.db.DB.Exec("DELETE FROM bindings WHERE bot=? AND chat=? AND thread=?", w.b.bot.ID, w.key.chat, w.key.thread); err != nil {
		t.Fatal(err)
	}
	replacement := w.binding
	replacement.Workspace = t.TempDir()
	replacement.Session = filepath.Join(replacement.Workspace, "replacement.jsonl")
	replacement.SessionID = "ffff0000-0000-4000-8000-000000000000"
	if err := w.b.db.Save(replacement); err != nil {
		t.Fatal(err)
	}
	w.exportFinished(result)
	if result.file.Path != "" {
		if _, err := os.Stat(result.file.Path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale export snapshot survived binding deletion: %v", err)
		}
	}
	var documents int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE kind='document'").Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if documents != 0 || w.exportCancel != nil {
		t.Fatalf("stale export state: documents=%d export_cancel=%v", documents, w.exportCancel != nil)
	}
}

func TestExportHTMLQueuesNativeOutput(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	exporter := filepath.Join(t.TempDir(), "omp-export")
	script := "#!/bin/sh\n" +
		"[ \"$#\" -eq 3 ] || exit 11\n" +
		"[ \"$1\" = --export ] || exit 12\n" +
		"printf '%s' '<html>native</html>' > \"$3\"\n"
	if err := os.WriteFile(exporter, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	w.b.cfg.OMP = exporter
	w.beginExport(omp.SessionSummary{ID: w.sessionID, CWD: w.binding.Workspace}, "html", w.binding.Workspace, w.binding.Generation)
	w.exportFinished(finishExport(t, w))
	var path, name string
	if err := w.b.db.DB.QueryRow("SELECT path,name FROM outbox WHERE kind='document' ORDER BY id DESC LIMIT 1").Scan(&path, &name); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "<html>native</html>" || name != "omp-session-"+shortSessionID(w.sessionID)+".html" {
		t.Fatalf("queued HTML export = %q, name=%q, error=%v", data, name, err)
	}
	var notices int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE kind='text' AND text LIKE 'Session export queued%'").Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if notices != 0 {
		t.Fatalf("export emitted %d unconfirmed success notices", notices)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0400 {
		t.Fatalf("HTML snapshot mode = %v", info.Mode().Perm())
	}
}

func TestHTMLSnapshotTimeoutRemovesTarget(t *testing.T) {
	root, spool := t.TempDir(), t.TempDir()
	input := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(input, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "omp-export")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := snapshotHTML(ctx, binary, input, spool, "omp-session-test.html"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed out HTML snapshot error = %v", err)
	}
	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("HTML failure left spool entries: %v", entries)
	}
}

func TestHTMLSnapshotRequiresFinalSpoolDirectorySync(t *testing.T) {
	root, spool := t.TempDir(), t.TempDir()
	source := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(source, []byte("native\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "omp-export")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s' '<html>native</html>' > \"$3\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	syncErr := errors.New("final directory sync failed")
	oldSync := exportDirectorySync
	t.Cleanup(func() { exportDirectorySync = oldSync })
	calls := 0
	exportDirectorySync = func(path string) error {
		calls++
		if path != spool {
			t.Errorf("directory sync path = %q, want %q", path, spool)
		}
		if calls == 1 {
			return oldSync(path)
		}
		return syncErr
	}
	if _, err := snapshotHTML(context.Background(), binary, source, spool, "omp-session-test.html"); !errors.Is(err, syncErr) {
		t.Fatalf("HTML snapshot sync failure = %v, want %v", err, syncErr)
	}
	if calls != 2 {
		t.Fatalf("directory sync calls = %d, want source and final snapshots", calls)
	}
	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed HTML snapshot left unowned spool entries: %v", entries)
	}
}

func TestHTMLSnapshotUsesStableSourceSnapshot(t *testing.T) {
	root, spool := t.TempDir(), t.TempDir()
	source := filepath.Join(root, "session.jsonl")
	original := []byte("{\"native\":\"original\"}\n")
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "omp-export")
	script := "#!/bin/sh\nprintf '%s' '{\"native\":\"changed\"}\\n' > \"$HTML_SOURCE\"\ncat \"$2\" > \"$3\"\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTML_SOURCE", source)
	file, err := snapshotHTML(context.Background(), binary, source, spool, "omp-session-test.html")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Path)
	data, err := os.ReadFile(file.Path)
	if err != nil || string(data) != string(original) {
		t.Fatalf("HTML exporter did not use stable source snapshot: data=%q, error=%v", data, err)
	}
	if data, err := os.ReadFile(source); err != nil || string(data) == string(original) {
		t.Fatalf("source mutation did not occur in fixture: data=%q, error=%v", data, err)
	}
}

func TestHTMLSnapshotRejectsSymlinkedSource(t *testing.T) {
	root, spool := t.TempDir(), t.TempDir()
	target := filepath.Join(root, "session.jsonl")
	link := filepath.Join(root, "link.jsonl")
	if err := os.WriteFile(target, []byte("native\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "executed")
	binary := filepath.Join(root, "omp-export")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf executed > \"$HTML_EXPORT_MARKER\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTML_EXPORT_MARKER", marker)
	if _, err := snapshotHTML(context.Background(), binary, link, spool, "omp-session-test.html"); err == nil {
		t.Fatal("HTML snapshot accepted a symlinked source")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTML exporter ran for a symlinked source, stat error=%v", err)
	}
	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink rejection left spool entries: %v", entries)
	}
}

func TestHTMLSnapshotStopsExporterWhenOutputGrowsTooLarge(t *testing.T) {
	root, spool := t.TempDir(), t.TempDir()
	source := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(source, []byte("native\n"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "completed")
	binary := filepath.Join(root, "omp-export")
	script := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ \"$i\" -lt 20 ]; do\n" +
		"  printf '0123456789abcdef' >> \"$3\"\n" +
		"  i=$((i + 1))\n" +
		"  sleep 0.01\n" +
		"done\n" +
		"printf completed > \"$HTML_EXPORT_COMPLETED\"\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTML_EXPORT_COMPLETED", marker)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := snapshotHTMLWithLimit(ctx, binary, source, spool, "omp-session-test.html", 32); !errors.Is(err, errSessionExportTooLarge) {
		t.Fatalf("growing HTML export error = %v, want %v", err, errSessionExportTooLarge)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized exporter completed after cancellation, stat error=%v", err)
	}
	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("oversized HTML export left spool entries: %v", entries)
	}
}

func TestExportPickerAllowsBusyButRejectsCurrentSession(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	before := w.binding
	listing, err := json.Marshal([]resumeFixtureSession{{ID: w.sessionID, CWD: before.Workspace, Title: "current", UpdatedAt: "2026-09-20T12:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS", string(listing))
	w.busy = true
	command("/export")
	w.resumeListed(finishResumeList(t, w))
	if len(w.confirms) != 1 {
		t.Fatal("busy export did not publish a picker")
	}
	clickResume(w, 7, resumeButtons(t, f)[0]["callback_data"].(string))
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil || !strings.Contains(text, "Wait for the current task and queue to finish before exporting.") {
		t.Fatalf("busy current-session export reply = %q, error=%v", text, err)
	}
}

func TestExportAllowsOtherSessionWhileCurrentTaskBusy(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	before := w.binding
	sessions := setResumeFixtures(t, before.Workspace, 1)
	w.busy = true
	command("/export")
	w.resumeListed(finishResumeList(t, w))
	if len(w.confirms) != 1 {
		t.Fatal("busy export did not publish a picker")
	}
	clickResume(w, 7, resumeButtons(t, f)[0]["callback_data"].(string))
	w.exportFinished(finishExport(t, w))
	var path string
	if err := w.b.db.DB.QueryRow("SELECT path FROM outbox WHERE kind='document' ORDER BY id DESC LIMIT 1").Scan(&path); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), sessions[0].ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(source) {
		t.Fatalf("other-session export = %q, want %q, error=%v", data, source, err)
	}
}

func TestExportOtherSessionPreservesCurrentUIConfirmation(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	w.owner = 7
	w.event([]byte(`{"type":"extension_ui_request","id":"native-confirm","method":"confirm","title":"Approve"}`))
	var uiToken string
	for token, c := range w.confirms {
		if c.action == "ui" {
			uiToken = token
			break
		}
	}
	if uiToken == "" {
		t.Fatal("native UI confirmation was not registered")
	}
	setResumeFixtures(t, w.binding.Workspace, 1)
	command("/export")
	w.resumeListed(finishResumeList(t, w))
	buttons := resumeButtons(t, f)
	message := update(0, 11, "").Message
	message.MessageID = int64(f.messageCount())
	w.callback(&telegram.CallbackQuery{ID: "picker-callback", From: telegram.User{ID: 7}, Message: message, Data: buttons[0]["callback_data"].(string)})
	result := finishExport(t, w)
	if _, ok := w.confirms[uiToken]; !ok {
		t.Fatal("exporting another session cleared the current UI confirmation")
	}
	var state struct {
		Replies int `json:"fixtureUIReplies"`
	}
	raw, err := w.call("get_state", nil)
	if err != nil || json.Unmarshal(raw, &state) != nil || state.Replies != 0 {
		t.Fatalf("other-session export canceled current native UI: replies=%d, error=%v", state.Replies, err)
	}
	w.exportFinished(result)
}

func TestExportRejectsSessionClaimedByOtherConversation(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new test")
	session := omp.SessionSummary{ID: "abcd0000-0000-4000-8000-000000000000", CWD: w.binding.Workspace}
	other := &worker{}
	w.b.sessionClaims = map[string]sessionClaim{"other-session": {owner: other, id: session.ID}}
	w.beginExport(session, "session", w.binding.Workspace, w.binding.Generation)
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil || text != "This session is currently active in another conversation. Close that instance before exporting it." {
		t.Fatalf("claimed session export reply = %q, error=%v", text, err)
	}
}

func TestDirectCurrentPrefixRejectsWhileBusy(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new test")
	prefix := w.sessionID[:8]
	marker := filepath.Join(t.TempDir(), "rendered")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RENDER_MARKER", marker)
	w.busy = true
	command("/export " + prefix)
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil || !strings.Contains(text, "Wait for the current task and queue to finish before exporting.") {
		t.Fatalf("busy current-prefix export reply = %q, error=%v", text, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("busy current-prefix export reached native render, stat error=%v", err)
	}
	if w.exportCancel != nil {
		t.Fatal("busy current-prefix export started an export")
	}
}

func TestDirectCurrentPrefixUsesNativeRenderWhenIdle(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	prefix := w.sessionID[:8]
	marker := filepath.Join(t.TempDir(), "rendered")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RENDER_MARKER", marker)
	command("/export " + prefix)
	result := finishExport(t, w)
	if result.err != nil {
		t.Fatalf("idle current-prefix export failed: %v", result.err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("idle current-prefix export did not use native render: %v", err)
	}
	w.exportFinished(result)
	var path string
	if err := w.b.db.DB.QueryRow("SELECT path FROM outbox WHERE kind='document' ORDER BY id DESC LIMIT 1").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
		t.Fatalf("idle current-prefix export snapshot = %q, error=%v", data, err)
	}
}

func TestInactiveExportRejectsCustomSessionDirectory(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new test")
	w.b.cfg.OMPArgs = []string{"--session-dir", filepath.Join(t.TempDir(), "native-store")}
	command("/export deadbeef-0000-4000-8000-000000000000")
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil || text != "Cannot export an inactive session when omp uses a custom session directory." {
		t.Fatalf("custom session-directory export reply = %q, error=%v", text, err)
	}
	if w.exportCancel != nil {
		t.Fatal("custom session-directory export started an export")
	}
}

func TestExportPickerListsMultipleSessions(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	sessions := setResumeFixtures(t, w.binding.Workspace, 10)
	command("/export")
	w.resumeListed(finishResumeList(t, w))
	buttons := resumeButtons(t, f)
	if len(buttons) != 10 {
		t.Fatalf("export first page has %d buttons, want eight choices, next, cancel", len(buttons))
	}
	for i := range 8 {
		if !strings.Contains(buttons[i]["text"].(string), sessions[i].Title) {
			t.Fatalf("export choice %d lost native ordering or title: %v", i, buttons[i])
		}
	}
	clickResume(w, 7, buttons[8]["callback_data"].(string))
	buttons = resumeButtons(t, f)
	if len(buttons) != 4 || !strings.Contains(buttons[0]["text"].(string), sessions[8].Title) || !strings.Contains(buttons[1]["text"].(string), sessions[9].Title) {
		t.Fatalf("export second page does not contain final native choices: %v", buttons)
	}
}

func TestDirectHTMLExportDoesNotOpenPicker(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	exporter := filepath.Join(t.TempDir(), "omp-export")
	if err := os.WriteFile(exporter, []byte("#!/bin/sh\nprintf '%s' '<html>direct</html>' > \"$3\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	w.b.cfg.OMP = exporter
	command("/export html " + w.sessionID)
	if len(w.confirms) != 0 {
		t.Fatal("direct HTML export opened a picker")
	}
	w.exportFinished(finishExport(t, w))
	var path, name string
	if err := w.b.db.DB.QueryRow("SELECT path,name FROM outbox WHERE kind='document' ORDER BY id DESC LIMIT 1").Scan(&path, &name); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "<html>direct</html>" || name != "omp-session-"+shortSessionID(w.sessionID)+".html" {
		t.Fatalf("direct HTML export = %q, name=%q, error=%v", data, name, err)
	}
}

func TestExportRejectsInvalidCommandArguments(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/export html first second")
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil || text != "Usage: /export [html] [session-id]." {
		t.Fatalf("invalid export args reply = %q, error=%v", text, err)
	}
	if len(w.confirms) != 0 || w.exportCancel != nil {
		t.Fatal("invalid export args opened or started an export")
	}
}

func TestExportPickerRejectsMissingSessionFile(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	sessions := setResumeFixtures(t, w.binding.Workspace, 1)
	command("/export")
	w.resumeListed(finishResumeList(t, w))
	path := filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), sessions[0].ID+".jsonl")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	clickResume(w, 7, resumeButtons(t, f)[0]["callback_data"].(string))
	w.exportFinished(finishExport(t, w))
	var documents int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE kind='document'").Scan(&documents); err != nil || documents != 0 {
		t.Fatalf("disappeared session enqueued %d documents, error=%v", documents, err)
	}
}

func TestExportPickerRejectsSessionWorkspaceMismatch(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	other := t.TempDir()
	session := resumeFixtureSession{ID: "abcd0000-0000-4000-8000-000000000000", CWD: w.binding.Workspace, Title: "mismatched-session", UpdatedAt: "2026-09-20T12:00:00Z"}
	path := filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), session.ID+".jsonl")
	data, err := json.Marshal(map[string]string{"type": "session", "id": session.ID, "cwd": other})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	listing, err := json.Marshal([]resumeFixtureSession{session})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS", string(listing))
	command("/export")
	w.resumeListed(finishResumeList(t, w))
	clickResume(w, 7, resumeButtons(t, f)[0]["callback_data"].(string))
	w.exportFinished(finishExport(t, w))
	var documents int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE kind='document'").Scan(&documents); err != nil || documents != 0 {
		t.Fatalf("mismatched session enqueued %d documents, error=%v", documents, err)
	}
}

func TestExportPickerRejectsWrongUserAndExpiredToken(t *testing.T) {
	for _, scenario := range []string{"wrong-user", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new test")
			setResumeFixtures(t, w.binding.Workspace, 1)
			command("/export")
			w.resumeListed(finishResumeList(t, w))
			button := resumeButtons(t, f)[0]
			user := int64(7)
			if scenario == "wrong-user" {
				user = 8
			} else {
				for token, confirmation := range w.confirms {
					confirmation.expires = time.Now().Add(-time.Second)
					w.confirms[token] = confirmation
				}
			}
			clickResume(w, user, button["callback_data"].(string))
			if w.exportCancel != nil {
				t.Fatal("invalid export callback started an export")
			}
			var documents int
			if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE kind='document'").Scan(&documents); err != nil || documents != 0 {
				t.Fatalf("%s callback enqueued %d documents, error=%v", scenario, documents, err)
			}
		})
	}
}

func TestExportRejectsCurrentSessionWhileCompactingOrQueued(t *testing.T) {
	for _, state := range []string{"compacting", "queued"} {
		t.Run(state, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new test")
			listing, err := json.Marshal([]resumeFixtureSession{{ID: w.sessionID, CWD: w.binding.Workspace, Title: "current", UpdatedAt: "2026-09-20T12:00:00Z"}})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS", string(listing))
			if state == "compacting" {
				w.compacting = true
			} else {
				w.queue = []queued{{}}
			}
			command("/export")
			w.resumeListed(finishResumeList(t, w))
			clickResume(w, 7, resumeButtons(t, f)[0]["callback_data"].(string))
			var text string
			if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil || !strings.Contains(text, "Wait for the current task and queue to finish before exporting.") {
				t.Fatalf("%s current-session export reply = %q, error=%v", state, text, err)
			}
		})
	}
}

type exportDeliveryFailureTransport struct{}

func (exportDeliveryFailureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error_code":400,"description":"export delivery rejected"}`)),
		Header:     make(http.Header),
		Request:    r,
	}, nil
}

func TestExportSnapshotDeliveryRemovesSnapshotAfterSuccess(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := filepath.Join(root, "session.jsonl")
	raw := []byte("{\"native\":true}\n")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotSession(context.Background(), filepath.Join(root, "spool"), source)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnqueueAttachment(-10, 11, "document", snapshot.Path, snapshot.Name, ""); err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	http.DefaultTransport = &fakeHTTP{}
	defer func() { http.DefaultTransport = previous }()
	b := testBridge(t, &Bridge{db: db, tg: newTestTelegram(t)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.deliver(ctx) }()
	waitFor(t, func() bool {
		var state string
		return db.DB.QueryRow("SELECT state FROM outbox WHERE path=?", snapshot.Path).Scan(&state) == nil && state == "done"
	})
	if _, err := os.Stat(snapshot.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed export snapshot still exists, error=%v", err)
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != string(raw) {
		t.Fatalf("confirmed delivery changed source, data=%q, error=%v", data, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("delivery loop returned error: %v", err)
	}
}

func TestExportSnapshotDeliveryFailureRetainsSnapshotAndSource(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := filepath.Join(root, "session.jsonl")
	raw := []byte("{\"native\":true}\n")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotSession(context.Background(), filepath.Join(root, "spool"), source)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnqueueAttachment(-10, 11, "document", snapshot.Path, snapshot.Name, ""); err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	http.DefaultTransport = exportDeliveryFailureTransport{}
	defer func() { http.DefaultTransport = previous }()
	b := testBridge(t, &Bridge{db: db, tg: newTestTelegram(t)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.deliver(ctx) }()
	waitFor(t, func() bool {
		var state string
		return db.DB.QueryRow("SELECT state FROM outbox WHERE path=?", snapshot.Path).Scan(&state) == nil && state == "failed"
	})
	if _, err := os.Stat(snapshot.Path); err != nil {
		t.Fatalf("failed export snapshot was removed: %v", err)
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != string(raw) {
		t.Fatalf("failed delivery changed source, data=%q, error=%v", data, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("delivery loop returned error: %v", err)
	}
}

func TestExportEnqueueFailureCleansSnapshot(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	if _, err := w.b.db.DB.Exec("CREATE TRIGGER reject_export_attachment BEFORE INSERT ON outbox WHEN NEW.kind='document' BEGIN SELECT RAISE(FAIL, 'injected export failure'); END"); err != nil {
		t.Fatal(err)
	}
	w.beginExport(omp.SessionSummary{ID: w.sessionID, CWD: w.binding.Workspace}, "session", w.binding.Workspace, w.binding.Generation)
	w.exportFinished(finishExport(t, w))
	entries, err := os.ReadDir(filepath.Join(w.b.cfg.DataDir, "attachments", "outbox"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("enqueue failure left export snapshots: %v", entries)
	}
}

func TestHTMLSnapshotRejectsInvalidOutputAndCleansTarget(t *testing.T) {
	cases := []struct {
		name   string
		script string
		large  bool
	}{
		{name: "exporter-failure", script: "#!/bin/sh\nexit 7\n"},
		{name: "empty", script: "#!/bin/sh\n: > \"$3\"\n"},
		{name: "oversized", script: fmt.Sprintf("#!/bin/sh\ntruncate -s %d \"$3\"\n", media.MaxDocumentBytes+1), large: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, spool := t.TempDir(), t.TempDir()
			input := filepath.Join(root, "session.jsonl")
			if err := os.WriteFile(input, []byte("{\"native\":true}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(root, "omp-export")
			if err := os.WriteFile(binary, []byte(tc.script), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := snapshotHTML(context.Background(), binary, input, spool, "omp-session-test.html"); err == nil || tc.large && !errors.Is(err, errSessionExportTooLarge) {
				t.Fatalf("invalid HTML output error = %v", err)
			}
			entries, err := os.ReadDir(spool)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("invalid HTML output left spool entries: %v", entries)
			}
		})
	}
}

func TestExportHTMLLeavesOriginalSessionUnchanged(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	raw := []byte("{\"native\":true,\"stable\":true}\n")
	if err := os.WriteFile(w.binding.Session, raw, 0600); err != nil {
		t.Fatal(err)
	}
	exporter := filepath.Join(t.TempDir(), "omp-export")
	if err := os.WriteFile(exporter, []byte("#!/bin/sh\nprintf '%s' '<html>stable</html>' > \"$3\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	w.b.cfg.OMP = exporter
	w.beginExport(omp.SessionSummary{ID: w.sessionID, CWD: w.binding.Workspace}, "html", w.binding.Workspace, w.binding.Generation)
	w.exportFinished(finishExport(t, w))
	if data, err := os.ReadFile(w.binding.Session); err != nil || string(data) != string(raw) {
		t.Fatalf("HTML export changed original session, data=%q, error=%v", data, err)
	}
}

func TestExportPreservesBindingRuntimeAndClaimState(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	before := w.binding
	client := w.client
	claimed := w.claimedSession
	slots := len(w.b.slots)
	startIntent := w.startIntent
	w.b.sessionMu.Lock()
	claims := make(map[string]sessionClaim, len(w.b.sessionClaims))
	for path, claim := range w.b.sessionClaims {
		claims[path] = claim
	}
	w.b.sessionMu.Unlock()
	var intentsBefore int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM startup_intents WHERE bot=? AND chat=? AND thread=?", w.b.bot.ID, w.key.chat, w.key.thread).Scan(&intentsBefore); err != nil {
		t.Fatal(err)
	}
	w.b.cfg.OMP = filepath.Join(t.TempDir(), "resolver-must-not-run")
	command("/export " + w.sessionID)
	w.exportFinished(finishExport(t, w))
	after, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastUsedAt != before.LastUsedAt || after.Generation != before.Generation || after.Running != before.Running {
		t.Fatalf("export changed binding state: before=%+v after=%+v", before, after)
	}
	if w.client != client || w.claimedSession != claimed || len(w.b.slots) != slots || w.startIntent != startIntent {
		t.Fatalf("export changed runtime state: client=%p/%p claim=%q/%q slots=%d/%d intent=%p/%p", w.client, client, w.claimedSession, claimed, len(w.b.slots), slots, w.startIntent, startIntent)
	}
	w.b.sessionMu.Lock()
	defer w.b.sessionMu.Unlock()
	if len(w.b.sessionClaims) != len(claims) {
		t.Fatalf("export changed claim count: before=%d after=%d", len(claims), len(w.b.sessionClaims))
	}
	for path, claim := range claims {
		afterClaim, ok := w.b.sessionClaims[path]
		if !ok || afterClaim.owner != claim.owner || afterClaim.id != claim.id {
			t.Fatalf("export changed session claim %q: before=%+v after=%+v", path, claim, afterClaim)
		}
	}
	var intentsAfter int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM startup_intents WHERE bot=? AND chat=? AND thread=?", w.b.bot.ID, w.key.chat, w.key.thread).Scan(&intentsAfter); err != nil {
		t.Fatal(err)
	}
	if intentsAfter != intentsBefore {
		t.Fatalf("export changed startup intents: before=%d after=%d", intentsBefore, intentsAfter)
	}
}

func TestDirectExportResolvesOtherSessionPrefix(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.DataDir = t.TempDir()
	command("/new test")
	sessions := setResumeFixtures(t, w.binding.Workspace, 1)
	command("/export " + sessions[0].ID[:8])
	if len(w.confirms) != 0 {
		t.Fatal("direct prefix export opened a picker")
	}
	w.exportFinished(finishExport(t, w))
	var path, name string
	if err := w.b.db.DB.QueryRow("SELECT path,name FROM outbox WHERE kind='document' ORDER BY id DESC LIMIT 1").Scan(&path, &name); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), sessions[0].ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(source) || name != filepath.Base(sessions[0].ID+".jsonl") {
		t.Fatalf("prefix export = %q, name=%q, want %q, error=%v", data, name, source, err)
	}
}

func containsAll(text string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(text, part) {
			return false
		}
	}
	return true
}
