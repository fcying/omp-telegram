package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"omp-telegram/internal/store"
)

// canonicalWorkspaceRoot uses the physical worktree root, not the common Git
// directory: linked worktrees have independent files and independent leases.
// Resolve missing descendants through their existing ancestor for /new's
// pre-mkdir fence, then repeat after creation and after OMP startup metadata.
func canonicalWorkspaceRoot(ctx context.Context, dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", errors.New("workspace must be absolute")
	}
	resolved, err := resolveWorkspace(string(filepath.Separator), dir)
	if err != nil {
		return "", err
	}
	ancestor := filepath.Clean(resolved)
	var missing []string
	for {
		_, err := os.Stat(ancestor)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", err
		}
		missing = append(missing, filepath.Base(ancestor))
		ancestor = parent
	}
	physical, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(physical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("workspace is not a directory")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "git", "-C", physical, "rev-parse", "--show-toplevel")
	// Git diagnostics are parsed below. LANGUAGE takes precedence over LC_ALL
	// for gettext, so pin both and ignore process-global Git overrides.
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(key, "GIT_") && key != "LC_ALL" && key != "LANGUAGE" {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "LC_ALL=C", "LANGUAGE=C")
	out, err := cmd.Output()
	if err == nil {
		root := strings.TrimSuffix(string(out), "\n")
		return filepath.EvalSymlinks(root)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !strings.Contains(string(exit.Stderr), "not a git repository") {
		return "", fmt.Errorf("cannot resolve physical Git worktree: %w", err)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		physical = filepath.Join(physical, missing[i])
	}
	return physical, nil
}

func (w *worker) workspaceOwner(session string) store.WorkspaceOwner {
	return store.WorkspaceOwner{Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread, Session: session}
}

func (w *worker) currentWorkspaceOwner() (store.WorkspaceOwner, error) {
	id := w.sessionID
	if id == "" {
		id = w.binding.SessionID
	}
	if !validSessionID(id) {
		return store.WorkspaceOwner{}, errors.New("workspace session identity unavailable")
	}
	return w.workspaceOwner(id), nil
}

func (w *worker) ensureResearchWorkspace() error {
	owner, err := w.currentWorkspaceOwner()
	if err != nil {
		return err
	}
	w.b.sessionMu.Lock()
	defer w.b.sessionMu.Unlock()
	root, err := w.b.refreshWorkspaceRootLocked(w.ctx, w.binding.Workspace)
	if err != nil {
		return err
	}
	if w.b.deleteMatchesLocked(owner.Session) {
		return errors.New("session is being deleted")
	}
	return w.b.db.ClaimResearchWorkspace(root, owner)
}

func (w *worker) releaseResearchWorkspace() error {
	owner, err := w.currentWorkspaceOwner()
	if err != nil {
		return err
	}
	w.b.sessionMu.Lock()
	defer w.b.sessionMu.Unlock()
	return w.b.db.ReleaseResearchWorkspace(owner)
}

func (w *worker) admitCurrentWorkspace() error {
	owner, err := w.currentWorkspaceOwner()
	if err != nil {
		return err
	}
	w.b.sessionMu.Lock()
	defer w.b.sessionMu.Unlock()
	root, err := w.b.refreshWorkspaceRootLocked(w.ctx, w.binding.Workspace)
	if err != nil {
		return err
	}
	if err = w.b.db.CheckWorkspaceSession(owner); err != nil {
		return err
	}
	return w.b.db.AdmitWorkspace(root, owner)
}

func startupWorkspaceSession(resume bool, target string, generation int64) string {
	if resume {
		return target
	}
	return "start:" + strconv.FormatInt(generation, 10)
}

func (b *Bridge) restoreWorkspaceUsers(ctx context.Context, bindings []store.Binding, intents []store.StartIntent) error {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	return b.refreshWorkspaceUsersLocked(ctx, bindings, intents)
}

// refreshWorkspaceRootLocked must run at every admission boundary, not just
// when a worker's path changes: ordinary user work can create or remove Git
// repositories and merge previously independent physical workspace identities.
func (b *Bridge) refreshWorkspaceRootLocked(ctx context.Context, workspace string) (string, error) {
	if err := b.refreshWorkspaceUsersLocked(ctx, nil, nil); err != nil {
		return "", err
	}
	if workspace == "" {
		return "", nil
	}
	return canonicalWorkspaceRoot(ctx, workspace)
}

func (b *Bridge) refreshWorkspaceUsersLocked(ctx context.Context, bindings []store.Binding, intents []store.StartIntent) error {
	users, leases, err := b.db.WorkspaceRegistrations()
	if err != nil {
		return err
	}
	bots := map[int64]bool{b.bot.ID: true}
	for _, registration := range users {
		bots[registration.Owner.Bot] = true
	}
	for _, registration := range leases {
		bots[registration.Owner.Bot] = true
	}
	for bot := range bots {
		running, err := b.db.RunningBindings(bot)
		if err != nil {
			return err
		}
		pending, err := b.db.PendingStarts(bot)
		if err != nil {
			return err
		}
		bindings = append(bindings, running...)
		intents = append(intents, pending...)
	}
	// Durable paths preserve the original source when a canonical root changes.
	// Registrations without a binding or intent keep their registered path as
	// the source; never silently discard an ordinary logical user.
	sources := make(map[store.WorkspaceOwner][]string)
	ownerKey := func(owner store.WorkspaceOwner) store.WorkspaceOwner {
		owner.Session = strings.ToLower(owner.Session)
		return owner
	}
	for _, binding := range bindings {
		if !validSessionID(binding.SessionID) {
			return errors.New("saved workspace session identity unavailable")
		}
		owner := ownerKey(store.WorkspaceOwner{Bot: binding.Bot, Chat: binding.Chat, Thread: binding.Thread, Session: binding.SessionID})
		sources[owner] = append(sources[owner], binding.Workspace)
	}
	for _, intent := range intents {
		if intent.Workspace == "" {
			continue
		}
		owner := ownerKey(store.WorkspaceOwner{Bot: intent.Bot, Chat: intent.Chat, Thread: intent.Thread, Session: startupWorkspaceSession(intent.Kind == "resume", intent.Session, intent.Generation)})
		sources[owner] = append(sources[owner], intent.Workspace)
	}
	fallback := make(map[store.WorkspaceOwner][]string)
	for _, registration := range users {
		owner := ownerKey(registration.Owner)
		if _, durable := sources[owner]; !durable {
			fallback[owner] = append(fallback[owner], registration.Root)
		}
	}
	for owner, paths := range fallback {
		sources[owner] = paths
	}
	var refreshedUsers, refreshedLeases []store.WorkspaceRegistration
	resolved := make(map[string]string)
	resolve := func(path string) (string, error) {
		if root, exists := resolved[path]; exists {
			return root, nil
		}
		root, err := canonicalWorkspaceRoot(ctx, path)
		if err == nil {
			resolved[path] = root
		}
		return root, err
	}
	for owner, paths := range sources {
		for _, path := range paths {
			root, err := resolve(path)
			if err != nil {
				// Keep a conservative registration for an unavailable path, but
				// let unrelated roots refresh and perform their own admission.
				root = filepath.Clean(path)
				for _, registration := range users {
					if ownerKey(registration.Owner) == owner {
						refreshedUsers = append(refreshedUsers, registration)
					}
				}
			}
			refreshedUsers = append(refreshedUsers, store.WorkspaceRegistration{Root: root, Owner: owner})
		}
	}
	leaseSources := make(map[store.WorkspaceOwner][]string)
	for _, registration := range leases {
		owner := ownerKey(registration.Owner)
		leaseSources[owner] = append(leaseSources[owner], registration.Root)
	}
	for owner, paths := range leaseSources {
		// An old root alone cannot reveal a newly nested repository. Follow
		// the exact native owner's original workspace as well, including a
		// closed binding whose uncertain/on lease must remain exclusive.
		binding, err := b.db.Binding(owner.Bot, owner.Chat, owner.Thread)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && strings.EqualFold(binding.SessionID, owner.Session) {
			paths = append(paths, binding.Workspace)
		}
		paths = append(paths, sources[owner]...)
		for _, path := range paths {
			root, err := resolve(path)
			if err != nil {
				// Historical roots stay in SQLite even when their files disappear.
				// Preserve the original path too; only confirmed off releases it.
				root = filepath.Clean(path)
			}
			refreshedLeases = append(refreshedLeases, store.WorkspaceRegistration{Root: root, Owner: owner})
		}
	}
	return b.db.RefreshWorkspaceRegistrations(refreshedUsers, refreshedLeases)
}
