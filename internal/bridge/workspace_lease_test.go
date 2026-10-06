package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func leaseTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestCanonicalWorkspaceLeasePhysicalWorktrees(t *testing.T) {
	repo := t.TempDir()
	leaseTestGit(t, repo, "init")
	leaseTestGit(t, repo, "-c", "user.name=lease-test", "-c", "user.email=lease@example.invalid", "commit", "--allow-empty", "-m", "initial")
	subdir := filepath.Join(repo, "sub", "nested")
	if err := os.MkdirAll(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	dedicated := filepath.Join(t.TempDir(), "worktree")
	leaseTestGit(t, repo, "worktree", "add", "--detach", dedicated, "HEAD")
	root, err := canonicalWorkspaceRoot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{subdir, link, filepath.Join(link, "sub"), filepath.Join(repo, "missing", "child")} {
		got, err := canonicalWorkspaceRoot(context.Background(), dir)
		if err != nil || got != root {
			t.Fatalf("root(%s)=%q,%v, want %q", dir, got, err, root)
		}
	}
	other, err := canonicalWorkspaceRoot(context.Background(), dedicated)
	if err != nil || other == root {
		t.Fatalf("linked worktree shared root: %q %v", other, err)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	requireStoreOK(t, db.ClaimResearchWorkspace(root, store.WorkspaceOwner{Bot: 99, Chat: -10, Thread: 11, Session: "aaaaaaaa-1111"}))
	requireStoreOK(t, db.ClaimResearchWorkspace(other, store.WorkspaceOwner{Bot: 99, Chat: -10, Thread: 12, Session: "bbbbbbbb-2222"}))
	// Git environment overrides must not redirect the physical identity.
	t.Setenv("GIT_WORK_TREE", dedicated)
	t.Setenv("GIT_DIR", filepath.Join(dedicated, "missing"))
	got, err := canonicalWorkspaceRoot(context.Background(), subdir)
	if err != nil || got != root {
		t.Fatalf("Git environment bypassed root: %q %v", got, err)
	}
	base, _, _ := setupWorkspaceWorker(t)
	a := leaseLogicalWorker(t, base.b, subdir, 21, "aaaaaaaa-1111")
	b := leaseLogicalWorker(t, base.b, dedicated, 22, "bbbbbbbb-2222")
	requireStoreOK(t, a.ensureResearchWorkspace())
	requireStoreOK(t, b.ensureResearchWorkspace())
}

func TestCanonicalWorkspaceLeaseNonRepositoryAliases(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	a, err := canonicalWorkspaceRoot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := canonicalWorkspaceRoot(context.Background(), link)
	if err != nil || a != b {
		t.Fatalf("nonrepo aliases differ: %q/%q %v", a, b, err)
	}
}

func leaseLogicalWorker(t *testing.T, b *Bridge, root string, thread int64, id string) *worker {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	binding := store.Binding{Bot: b.bot.ID, Chat: -10, Thread: thread, Workspace: root, Session: filepath.Join(t.TempDir(), id+".jsonl"), SessionID: id, Generation: 1, Running: true}
	if err := os.WriteFile(binding.Session, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	requireStoreOK(t, b.db.Save(binding))
	return testWorker(t, &worker{b: b, key: target{chat: -10, thread: thread}, binding: binding, sessionID: id, ctx: ctx, cancel: cancel, confirms: make(map[string]confirmation)})
}

func TestResearchLeaseRejectsNewResumeAndLazyRestoreBeforeUserWork(t *testing.T) {
	base, _, _ := setupWorkspaceWorker(t)
	root := t.TempDir()
	owner := leaseLogicalWorker(t, base.b, root, 21, "aaaaaaaa-1111")
	if err := owner.ensureResearchWorkspace(); err != nil {
		t.Fatal(err)
	}
	other := leaseLogicalWorker(t, base.b, root, 22, "bbbbbbbb-2222")
	// A missing binary makes an accidental launch observable. Every rejection
	// must happen at physical-workspace admission, before any OMP user work.
	base.b.cfg.OMP = filepath.Join(t.TempDir(), "missing-omp")
	for _, resume := range []bool{false, true} {
		target := root
		if resume {
			target = other.binding.SessionID
		}
		failure := other.startInternal(resume, target, root, false, 0, false, 0)
		if !strings.Contains(failure, "workspace is occupied") || other.client != nil || other.startIntent != nil {
			t.Fatalf("start resume=%v crossed admission: %q", resume, failure)
		}
	}
	if _, err := other.ensureRuntime(); err == nil || !strings.Contains(err.Error(), "workspace is occupied") {
		t.Fatalf("lazy restore crossed admission: %v", err)
	}
	if other.client != nil || other.startIntent != nil {
		t.Fatal("blocked lazy restore created runtime or intent")
	}
	if !owner.closeLogicalSession() {
		t.Fatal("owner close failed")
	}
	if err := other.admitCurrentWorkspace(); !errors.Is(err, store.ErrWorkspaceOccupied) {
		t.Fatalf("close discarded unknown/on lease: %v", err)
	}
	if failure := owner.startInternal(false, t.TempDir(), "", false, 0, false, 0); !strings.Contains(failure, "workspace is occupied") {
		t.Fatalf("fresh same-topic session bypassed old lease: %q", failure)
	}
}

func TestResearchLeaseIdleReleaseRetainsExclusivityAndNormalUser(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	if err := w.ensureResearchWorkspace(); err != nil {
		t.Fatal(err)
	}
	w.b.cfg.IdleTimeout = time.Minute
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	w.releaseIdleRuntime(time.Now())
	if w.client != nil {
		t.Fatal("fixture runtime did not release")
	}
	other := leaseLogicalWorker(t, w.b, w.binding.Workspace, 22, "bbbbbbbb-2222")
	if err := other.admitCurrentWorkspace(); !errors.Is(err, store.ErrWorkspaceOccupied) {
		t.Fatalf("idle discarded lease: %v", err)
	}
	if err := w.releaseResearchWorkspace(); err != nil {
		t.Fatal(err)
	}
	if err := other.admitCurrentWorkspace(); err != nil {
		t.Fatal(err)
	}
	if err := other.ensureResearchWorkspace(); !errors.Is(err, store.ErrWorkspaceOccupied) {
		t.Fatalf("idle/off discarded normal owner: %v", err)
	}
}

func TestResearchLeaseRestoresColdBindingOccupancyBeforeRuntime(t *testing.T) {
	base, _, _ := setupWorkspaceWorker(t)
	root := t.TempDir()
	a := leaseLogicalWorker(t, base.b, root, 21, "aaaaaaaa-1111")
	b := leaseLogicalWorker(t, base.b, root, 22, "bbbbbbbb-2222")
	if err := base.b.restoreWorkspaceUsers(context.Background(), []store.Binding{a.binding, b.binding}, nil); err != nil {
		t.Fatal(err)
	}
	if a.client != nil || b.client != nil {
		t.Fatal("restoration unexpectedly started runtimes")
	}
	if err := a.ensureResearchWorkspace(); !errors.Is(err, store.ErrWorkspaceOccupied) {
		t.Fatalf("cold restored topic was invisible: %v", err)
	}
}

func TestSharedWorkspaceRejectsResearchEnableAndClearBeforeNativeMutation(t *testing.T) {
	for _, command := range []string{"/autoresearch on", "/autoresearch clear"} {
		t.Run(command, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			root, err := canonicalWorkspaceRoot(w.ctx, w.binding.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			other := w.workspaceOwner("bbbbbbbb-2222")
			other.Thread++
			requireStoreOK(t, w.b.db.AdmitWorkspace(root, other))
			if command == "/autoresearch clear" {
				w.confirmResearchClear(confirmation{epoch: w.client.CommandCatalog().Epoch, autoresearchText: command})
			} else {
				nativeInput(t, w, 100, command, false)
				nativeUntil(t, w, func() bool { return !w.rpcOperationActive })
				if !w.resumeFailed {
					t.Fatal("shared research enable did not fail closed")
				}
			}
			if researchTraceCount(trace, "prompt", command) != 0 {
				t.Fatal("shared workspace received a native destructive command")
			}
			var count int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM workspace_leases").Scan(&count))
			if count != 0 {
				t.Fatal("shared workspace acquired exclusivity")
			}
		})
	}
}

func TestResearchLeaseProtectsClosedSessionHistoryFromDeletion(t *testing.T) {
	base, _, _ := setupWorkspaceWorker(t)
	owner := leaseLogicalWorker(t, base.b, t.TempDir(), 21, "aaaaaaaa-1111")
	if err := owner.ensureResearchWorkspace(); err != nil {
		t.Fatal(err)
	}
	if !owner.closeLogicalSession() {
		t.Fatal("close failed")
	}
	for _, id := range []string{"AAAAAAAA", "aaaaaaaa-1111"} {
		available, err := base.sessionDeleteAvailable(id, owner.binding.Workspace)
		if err != nil || available {
			t.Fatalf("closed lease owner history deletable: %v %v", available, err)
		}
		if base.b.reserveDelete(base, id) {
			t.Fatal("confirmation recheck admitted leased history deletion")
		}
	}
	owner.sessionID = owner.binding.SessionID
	if err := owner.releaseResearchWorkspace(); err != nil {
		t.Fatal(err)
	}
	available, err := base.sessionDeleteAvailable(owner.sessionID, owner.binding.Workspace)
	if err != nil || !available {
		t.Fatalf("confirmed off did not unlock history deletion: %v %v", available, err)
	}
}

func TestResearchLeaseUnknownNativeResumeChecksActualMetadataBeforeWork(t *testing.T) {
	w, command, trace := nativeWorker(t, "autoresearch")
	saved := w.binding
	command("/close")
	owner := leaseLogicalWorker(t, w.b, saved.Workspace, 21, "aaaaaaaa-1111")
	if err := owner.ensureResearchWorkspace(); err != nil {
		t.Fatal(err)
	}
	failure := w.startInternal(true, saved.SessionID, "", false, 0, false, 0)
	if !strings.Contains(failure, "physical workspace is unavailable") || w.client != nil || w.binding.Running {
		t.Fatalf("native resume did not fence actual workspace metadata: %q client=%v binding=%+v", failure, w.client != nil, w.binding)
	}
	if researchTraceCount(trace, "prompt", "") != 0 {
		t.Fatal("unknown workspace resume submitted user work")
	}
	root, err := canonicalWorkspaceRoot(w.ctx, saved.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.AdmitWorkspace(root, w.workspaceOwner(saved.SessionID)); !errors.Is(err, store.ErrWorkspaceOccupied) {
		t.Fatalf("failed resume disturbed the owning lease: %v", err)
	}
}

func TestWorkspaceGitInitRefreshesBothOrdinaryOwners(t *testing.T) {
	base, _, _ := setupWorkspaceWorker(t)
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	requireStoreOK(t, os.Mkdir(sub, 0700))
	a := leaseLogicalWorker(t, base.b, sub, 21, "aaaaaaaa-1111")
	b := leaseLogicalWorker(t, base.b, root, 22, "bbbbbbbb-2222")
	requireStoreOK(t, a.admitCurrentWorkspace())
	requireStoreOK(t, b.admitCurrentWorkspace())
	leaseTestGit(t, root, "init")
	for _, w := range []*worker{a, b} {
		if err := w.ensureResearchWorkspace(); !errors.Is(err, store.ErrWorkspaceOccupied) {
			t.Fatalf("merged ordinary workspace admitted research for thread %d: %v", w.key.thread, err)
		}
	}
	var leases, users int
	requireStoreOK(t, base.b.db.DB.QueryRow("SELECT COUNT(*) FROM workspace_leases").Scan(&leases))
	requireStoreOK(t, base.b.db.DB.QueryRow("SELECT COUNT(*) FROM workspace_users WHERE root=?", root).Scan(&users))
	if leases != 0 || users != 2 {
		t.Fatalf("merged registration mismatch: leases=%d users=%d", leases, users)
	}
	// Removing the repository must use each binding's original path, not keep
	// the obsolete shared normal root as an additional permanent occupant.
	requireStoreOK(t, os.RemoveAll(filepath.Join(root, ".git")))
	requireStoreOK(t, a.ensureResearchWorkspace())
	requireStoreOK(t, b.ensureResearchWorkspace())
}

func TestWorkspaceRootChangeRetainsLeaseAcrossStartAndLazyAdmission(t *testing.T) {
	base, _, _ := setupWorkspaceWorker(t)
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	requireStoreOK(t, os.Mkdir(sub, 0700))
	owner := leaseLogicalWorker(t, base.b, sub, 21, "aaaaaaaa-1111")
	requireStoreOK(t, owner.ensureResearchWorkspace())
	leaseTestGit(t, root, "init")
	other := leaseLogicalWorker(t, base.b, root, 22, "bbbbbbbb-2222")
	base.b.cfg.OMP = filepath.Join(t.TempDir(), "missing-omp")
	for _, resume := range []bool{false, true} {
		target := root
		if resume {
			target = other.binding.SessionID
		}
		failure := other.startInternal(resume, target, root, false, 0, false, 0)
		if !strings.Contains(failure, "workspace is occupied") || other.client != nil || other.startIntent != nil {
			t.Fatalf("changed root crossed startup admission: %q", failure)
		}
	}
	if _, err := other.ensureRuntime(); err == nil || !strings.Contains(err.Error(), "workspace is occupied") {
		t.Fatalf("changed root crossed lazy admission: %v", err)
	}
	requireStoreOK(t, owner.admitCurrentWorkspace())
	for _, path := range []string{sub, root} {
		if err := base.b.db.AdmitWorkspace(path, other.workspaceOwner(other.sessionID)); !errors.Is(err, store.ErrWorkspaceOccupied) {
			t.Fatalf("historical/current lease root %s was lost: %v", path, err)
		}
	}
	// Neither an ID prefix nor another same-topic native identity may release
	// any aliases. The exact owner can release without resolving Git again.
	wrong := owner.workspaceOwner("aaaaaaaa")
	requireStoreOK(t, base.b.db.ReleaseResearchWorkspace(wrong))
	if err := other.admitCurrentWorkspace(); !errors.Is(err, store.ErrWorkspaceOccupied) {
		t.Fatalf("prefix release discarded aliases: %v", err)
	}
	requireStoreOK(t, owner.releaseResearchWorkspace())
	var leases int
	requireStoreOK(t, base.b.db.DB.QueryRow("SELECT COUNT(*) FROM workspace_leases").Scan(&leases))
	if leases != 0 {
		t.Fatalf("confirmed off orphaned %d historical leases", leases)
	}
	requireStoreOK(t, other.admitCurrentWorkspace())
}

func TestWorkspaceNormalRefreshFailureRollsBackAndFailsClosed(t *testing.T) {
	base, _, _ := setupWorkspaceWorker(t)
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	requireStoreOK(t, os.Mkdir(sub, 0700))
	a := leaseLogicalWorker(t, base.b, sub, 21, "aaaaaaaa-1111")
	b := leaseLogicalWorker(t, base.b, root, 22, "bbbbbbbb-2222")
	requireStoreOK(t, a.admitCurrentWorkspace())
	requireStoreOK(t, b.admitCurrentWorkspace())
	leaseTestGit(t, root, "init")
	_, err := base.b.db.DB.Exec("CREATE TRIGGER reject_refresh BEFORE INSERT ON workspace_users WHEN NEW.thread=22 BEGIN SELECT RAISE(FAIL,'injected normal refresh failure'); END")
	requireStoreOK(t, err)
	if err := a.ensureResearchWorkspace(); err == nil || !strings.Contains(err.Error(), "injected normal refresh failure") {
		t.Fatalf("failed refresh admitted research: %v", err)
	}
	if err := b.admitCurrentWorkspace(); err == nil {
		t.Fatal("failed refresh admitted ordinary user work")
	}
	if b.claimSessionWorkspace(b.binding.Session, b.sessionID, b.binding.Workspace) {
		t.Fatal("failed refresh admitted session metadata")
	}
	var oldUsers, allUsers, leases int
	requireStoreOK(t, base.b.db.DB.QueryRow("SELECT COUNT(*) FROM workspace_users WHERE (root=? AND thread=21) OR (root=? AND thread=22)", sub, root).Scan(&oldUsers))
	requireStoreOK(t, base.b.db.DB.QueryRow("SELECT COUNT(*) FROM workspace_users").Scan(&allUsers))
	requireStoreOK(t, base.b.db.DB.QueryRow("SELECT COUNT(*) FROM workspace_leases").Scan(&leases))
	if oldUsers != 2 || allUsers != 2 || leases != 0 {
		t.Fatalf("failed refresh changed occupancy: old=%d all=%d leases=%d", oldUsers, allUsers, leases)
	}
	_, err = base.b.db.DB.Exec("DROP TRIGGER reject_refresh")
	requireStoreOK(t, err)
	if err := a.ensureResearchWorkspace(); !errors.Is(err, store.ErrWorkspaceOccupied) {
		t.Fatalf("recovered refresh forgot the other owner: %v", err)
	}
}

func TestCanonicalWorkspaceLocaleDoesNotChangeNonGitAdmission(t *testing.T) {
	// A deterministic Git stand-in emulates gettext's LANGUAGE precedence.
	// This exercises the physical-root consumer without requiring installed
	// system locales or translated Git catalogs.
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$LANGUAGE\" = C ] && [ \"$LC_ALL\" = C ]; then\n  echo 'fatal: not a git repository (or any of the parent directories): .git' >&2\nelse\n  echo 'fatal: pas un depot git' >&2\nfi\nexit 128\n"
	requireStoreOK(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700))
	t.Setenv("PATH", bin)
	t.Setenv("LANGUAGE", "fr:de")
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	base, _, _ := setupWorkspaceWorker(t)
	dir := t.TempDir()
	w := leaseLogicalWorker(t, base.b, dir, 21, "aaaaaaaa-1111")
	requireStoreOK(t, w.admitCurrentWorkspace())
	requireStoreOK(t, w.ensureResearchWorkspace())
	other := leaseLogicalWorker(t, base.b, dir, 22, "bbbbbbbb-2222")
	if err := other.admitCurrentWorkspace(); !errors.Is(err, store.ErrWorkspaceOccupied) {
		t.Fatalf("localized non-Git workspace bypassed exclusivity: %v", err)
	}
}

func TestClosedWorkspaceLeaseFollowsExactOwnerIntoNestedRepository(t *testing.T) {
	base, _, _ := setupWorkspaceWorker(t)
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	requireStoreOK(t, os.Mkdir(sub, 0700))
	leaseTestGit(t, root, "init")
	owner := leaseLogicalWorker(t, base.b, sub, 21, "aaaaaaaa-1111")
	requireStoreOK(t, owner.ensureResearchWorkspace())
	if !owner.closeLogicalSession() {
		t.Fatal("close failed")
	}
	deleted, err := base.b.db.DeleteClosedBinding(owner.binding.Bot, owner.binding.Chat, owner.binding.Thread, owner.binding.Generation)
	if deleted || !errors.Is(err, store.ErrWorkspaceOccupied) {
		t.Fatalf("forget removed the closed lease owner's workspace source: deleted=%v err=%v", deleted, err)
	}
	saved, err := base.b.db.Binding(owner.binding.Bot, owner.binding.Chat, owner.binding.Thread)
	requireStoreOK(t, err)
	if saved.Workspace != sub {
		t.Fatalf("forget changed the closed owner's workspace: %q", saved.Workspace)
	}
	leaseTestGit(t, sub, "init")
	other := leaseLogicalWorker(t, base.b, sub, 22, "bbbbbbbb-2222")
	if err := other.admitCurrentWorkspace(); !errors.Is(err, store.ErrWorkspaceOccupied) {
		t.Fatalf("nested identity bypassed closed owner's lease: %v", err)
	}
	if !owner.claimSessionWorkspace(owner.binding.Session, owner.binding.SessionID, sub) {
		t.Fatal("exact owner could not recover the changed physical identity")
	}
	for _, path := range []string{root, sub} {
		if err := base.b.db.AdmitWorkspace(path, other.workspaceOwner(other.sessionID)); !errors.Is(err, store.ErrWorkspaceOccupied) {
			t.Fatalf("nested identity lost lease root %s: %v", path, err)
		}
	}
	requireStoreOK(t, owner.releaseResearchWorkspace())
	deleted, err = base.b.db.DeleteClosedBinding(owner.binding.Bot, owner.binding.Chat, owner.binding.Thread, owner.binding.Generation)
	requireStoreOK(t, err)
	if !deleted {
		t.Fatal("confirmed release did not permit forgetting the binding")
	}
	requireStoreOK(t, other.admitCurrentWorkspace())
}

func TestWorkspaceAdmissionCancellationPreservesShutdownSettlement(t *testing.T) {
	for _, kind := range []string{"followup", "native", "photo", "album"} {
		t.Run(kind, func(t *testing.T) {
			w, _, _ := researchWorker(t, "autoresearch")
			input := update(100, w.key.thread, "")
			switch kind {
			case "followup":
				input.Message.Text = "/followup pending at shutdown"
			case "native":
				input.Message.Text = "/foo pending at shutdown"
			default:
				input.Message.Photo = []telegram.PhotoSize{{FileID: "never-downloaded", Width: 8, Height: 8}}
				if kind == "album" {
					input.Message.MediaGroupID = "cancelled-admission"
				}
			}
			raw, err := json.Marshal(input)
			requireStoreOK(t, err)
			requireStoreOK(t, w.b.db.Accept(input.UpdateID, raw))
			// Polling can persist an input just before shutdown cancels Git admission.
			w.cancel()
			w.handle(incoming{id: input.UpdateID, msg: input.Message})
			if state := queueState(t, w, input.UpdateID); state != "pending" {
				t.Fatalf("cancelled admission settled unsubmitted input as %q", state)
			}
			requireStoreOK(t, w.b.cancelPendingPrompts("shutdown"))
			if state := queueState(t, w, input.UpdateID); state != "cancelled" {
				t.Fatalf("shutdown did not cancel unsubmitted input: %q", state)
			}
		})
	}
}

func TestWorkspaceShutdownClientExitPreservesSessionBinding(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	nativeInput(t, w, 100, "wait", false)
	researchBarrier(t, w)
	nativeInput(t, w, 101, "/followup cancelled at shutdown", false)
	before := w.binding
	// Both cancellation and client exit may be ready in the actor's select.
	w.cancel()
	w.failed()
	w.teardownWorker(true)
	after, err := w.b.db.Binding(before.Bot, before.Chat, before.Thread)
	requireStoreOK(t, err)
	if !after.Running || !after.Interrupted || after.Session != before.Session || after.SessionID != before.SessionID {
		t.Fatal("shutdown treated client exit as an explicit close and lost the saved session")
	}
	if queueState(t, w, 100) != "uncertain" || queueState(t, w, 101) != "cancelled" || researchTraceCount(trace, "prompt", "cancelled at shutdown") != 0 {
		t.Fatal("shutdown did not preserve uncertain-root and cancelled-queue recovery semantics")
	}
}

func TestMergedWorkspaceResearchControlRejectsUnknownOwnerAndNonOffInputs(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	requireStoreOK(t, w.ensureResearchWorkspace())
	for _, text := range []string{"ordinary input", "/autoresearch", "/autoresearch on", "/autoresearch clear", "/autoresearch off extra", "/autoresearch@OtherBot off"} {
		if w.routeResearchOff(incoming{}, text) {
			t.Fatalf("disable-only control accepted %q", text)
		}
	}
	fullID := w.sessionID
	w.sessionID = fullID[:8]
	if w.routeResearchOff(incoming{}, "/autoresearch off") {
		t.Fatal("session prefix authorized disable-only recovery")
	}
	w.sessionID = "aaaaaaaa-1111"
	if w.routeResearchOff(incoming{}, "/autoresearch off") {
		t.Fatal("unknown native identity authorized disable-only recovery")
	}
	w.sessionID = fullID
	if researchTraceCount(trace, "prompt", "/autoresearch off") != 0 {
		t.Fatal("rejected control boundary sent a native off command")
	}
}

func TestMergedLeaseWorkspaceRejectsActiveRootSteering(t *testing.T) {
	w, command, trace := researchWorker(t, "autoresearch")
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	command("/close")
	command("/new " + sub)
	nativeCatalogReady(t, w)
	other := leaseLogicalWorker(t, w.b, root, 22, "bbbbbbbb-2222")
	requireStoreOK(t, other.ensureResearchWorkspace())
	nativeInput(t, w, 100, "wait", false)
	researchBarrier(t, w)
	leaseTestGit(t, root, "init")
	nativeInput(t, w, 101, "must not steer merged leased workspace", false)
	if queueState(t, w, 101) != "cancelled" || researchTraceCount(trace, "prompt", "must not steer merged leased workspace") != 0 {
		t.Fatal("active ordinary root steered into a newly conflicting workspace")
	}
	owned, err := w.b.db.ResearchWorkspaceOwned(other.workspaceOwner(other.sessionID))
	requireStoreOK(t, err)
	if !owned {
		t.Fatal("active admission failure released the conflicting owner's lease")
	}
}

func TestWorkspaceConflictDoesNotBlockOrdinaryAbort(t *testing.T) {
	for _, control := range []string{"command", "progress"} {
		t.Run(control, func(t *testing.T) {
			w, command, trace := researchWorker(t, "autoresearch")
			root := t.TempDir()
			sub := filepath.Join(root, "sub")
			requireStoreOK(t, os.MkdirAll(sub, 0700))
			leaseTestGit(t, root, "init")
			leaseTestGit(t, sub, "init")
			command("/close")
			command("/new " + sub)
			nativeCatalogReady(t, w)
			other := leaseLogicalWorker(t, w.b, root, 22, "bbbbbbbb-2222")
			requireStoreOK(t, other.ensureResearchWorkspace())
			nativeInput(t, w, 100, "wait", false)
			researchBarrier(t, w)
			nativeInput(t, w, 101, "/followup queued before conflict", false)
			if !w.taskActive() || w.research.root || w.steerFence.active {
				t.Fatal("ordinary root did not remain active before the workspace conflict")
			}
			client, binding := w.client, w.binding
			// Removing the nested repository merges its physical root with the lease owner.
			requireStoreOK(t, os.RemoveAll(filepath.Join(sub, ".git")))
			if !errors.Is(w.admitCurrentWorkspace(), store.ErrWorkspaceOccupied) {
				t.Fatal("merged workspace did not reject ordinary admission")
			}
			if control == "command" {
				command("/stop")
			} else {
				w.stopActiveTask()
			}
			nativeUntil(t, w, func() bool {
				return !w.taskActive() && !w.rpcOperationActive && !w.controlInProgress()
			})
			if researchTraceCount(trace, "abort", "") != 1 {
				t.Fatal("workspace conflict prevented the native abort")
			}
			wantQueueState := "cancelled"
			if control == "progress" {
				wantQueueState = "pending"
			}
			if queueState(t, w, 101) != wantQueueState || researchTraceCount(trace, "prompt", "queued before conflict") != 0 {
				t.Fatal("stop lost its queue policy or submitted queued work into the conflict")
			}
			if w.client != client || !sameBindingIdentity(binding, w.binding) {
				t.Fatal("abort replaced the existing runtime or logical session")
			}
			owned, err := w.b.db.ResearchWorkspaceOwned(other.workspaceOwner(other.sessionID))
			requireStoreOK(t, err)
			if !owned || !errors.Is(w.admitCurrentWorkspace(), store.ErrWorkspaceOccupied) {
				t.Fatal("abort released another owner's lease or admitted conflicting work")
			}
		})
	}
}

func TestMergedLeaseOwnerOffWaitsForDiscoveryWithoutOrdinaryAdmission(t *testing.T) {
	w, command, trace := nativeWorker(t, "autoresearch-hold")
	root := t.TempDir()
	command("/close")
	command("/new " + filepath.Join(root, "sub"))
	nativeRPC(t, w, "fixture_catalog_update", nil)
	w.commandCatalogUpdated()
	nativeInput(t, w, 100, "/autoresearch", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	researchBarrier(t, w)
	other := leaseLogicalWorker(t, w.b, root, 22, "bbbbbbbb-2222")
	requireStoreOK(t, other.ensureResearchWorkspace())
	leaseTestGit(t, root, "init")
	w.client.InvalidateCommandCatalog()
	nativeInput(t, w, 101, "/autoresearch off", false)
	if queueState(t, w, 101) != "pending" || researchTraceCount(trace, "prompt", "/autoresearch off") != 0 {
		t.Fatal("conflicting owner bypassed pending command discovery")
	}
	nativeRPC(t, w, "fixture_catalog_update", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	owned, err := w.b.db.ResearchWorkspaceOwned(w.workspaceOwner(w.sessionID))
	requireStoreOK(t, err)
	if owned || researchTraceCount(trace, "prompt", "/autoresearch off") != 1 {
		t.Fatal("command discovery did not permit exact-owner disable across the conflict")
	}
	owned, err = w.b.db.ResearchWorkspaceOwned(other.workspaceOwner(other.sessionID))
	requireStoreOK(t, err)
	if !owned || !errors.Is(w.admitCurrentWorkspace(), store.ErrWorkspaceOccupied) {
		t.Fatal("confirmed off released another owner's lease or admitted ordinary work")
	}
}
