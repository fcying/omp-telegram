package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func TestRealOMPWorkspaceMoveUpdatesTopicBinding(t *testing.T) {
	binary := os.Getenv("OMP_TELEGRAM_TEST_OMP")
	if binary == "" {
		t.Skip("set OMP_TELEGRAM_TEST_OMP to an actual omp executable")
	}
	binary, err := filepath.Abs(binary)
	requireStoreOK(t, err)
	info, err := os.Stat(binary)
	requireStoreOK(t, err)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatal("explicit OMP binary is not an executable regular file")
	}
	root, err := filepath.Abs(t.TempDir())
	requireStoreOK(t, err)
	dirs := map[string]string{
		"home": filepath.Join(root, "home"), "agent": filepath.Join(root, "agent"),
		"workspace": filepath.Join(root, "workspace"), "sessions": filepath.Join(root, "sessions"),
		"data": filepath.Join(root, "data"), "config": filepath.Join(root, "config"),
		"tmp": filepath.Join(root, "tmp"),
	}
	for _, dir := range dirs {
		requireStoreOK(t, os.MkdirAll(dir, 0700))
	}
	for name, dir := range map[string]string{
		"HOME": dirs["home"], "PI_CODING_AGENT_DIR": dirs["agent"], "TMPDIR": dirs["tmp"],
		"XDG_CONFIG_HOME": dirs["config"], "XDG_DATA_HOME": dirs["data"], "PATH": "/usr/local/bin:/usr/bin:/bin",
	} {
		t.Setenv(name, dir)
	}
	environment, err := omp.NewEnvironment("allowlist", []string{
		"HOME", "PI_CODING_AGENT_DIR", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "PATH",
	}, nil)
	requireStoreOK(t, err)
	args := []string{
		"--session-dir", dirs["sessions"], "--model", "openai/gpt-4o", "--no-title",
		"--no-extensions", "--no-skills", "--no-rules", "--no-lsp",
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	db, err := store.Open(dirs["data"])
	requireStoreOK(t, err)
	w := testWorker(t, &worker{
		b: testBridge(t, &Bridge{ctx: ctx, cfg: config.Config{
			OMP: binary, OMPArgs: [][]string{args}, OMPEnvironment: environment,
			DataDir: dirs["data"], WorkspaceRoot: dirs["workspace"], QueueCapacity: 4, ProgressMode: "off",
		}, db: db, tg: newTestTelegram(t), bot: telegram.User{ID: 99}, slots: make(chan struct{}, 1)}),
		key: target{chat: -10, thread: 11}, ctx: ctx, cancel: cancel, confirms: make(map[string]confirmation),
	})
	t.Cleanup(func() {
		w.teardownWorker(true)
		cancel()
		w.background.Wait()
		requireStoreOK(t, db.Close())
	})

	oldWorkspace := filepath.Join(dirs["workspace"], "old")
	newWorkspace := filepath.Join(dirs["workspace"], "renamed with spaces")
	requireStoreOK(t, os.MkdirAll(oldWorkspace, 0700))
	nativeInput(t, w, 1, "/new "+oldWorkspace, false)
	if w.client == nil {
		t.Fatal("real OMP session failed to start")
	}
	nativeCatalogReady(t, w)
	before := w.binding
	requireStoreOK(t, os.Rename(oldWorkspace, newWorkspace))

	nativeInput(t, w, 2, "/move "+newWorkspace, false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 2) == "done" && !w.controlInProgress() })
	saved, err := db.Binding(before.Bot, before.Chat, before.Thread)
	requireStoreOK(t, err)
	nativeInfo, err := w.client.SessionInfo(ctx)
	if err != nil || !sameBindingIdentityExceptGeneration(w.binding, saved) ||
		!strings.EqualFold(saved.SessionID, before.SessionID) || saved.Generation != before.Generation+1 ||
		saved.Workspace != newWorkspace || nativeInfo.ID != before.SessionID || nativeInfo.CWD != newWorkspace || nativeInfo.File != saved.Session {
		t.Fatalf("real OMP move did not reconcile the topic binding: saved=%+v runtime=%+v err=%v", saved, nativeInfo, err)
	}
}

func sameBindingIdentityExceptGeneration(left, right store.Binding) bool {
	left.Generation = right.Generation
	left.LastUsedAt = right.LastUsedAt
	return left == right
}
