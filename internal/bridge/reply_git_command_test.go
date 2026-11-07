package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/telegram"
)

func replyGitCommandScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git status script")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadReplyGitFieldEmptyCommandKeepsSafeProbe(t *testing.T) {
	repo := newReplyGitRepository(t, true)
	hook, marker := replyGitSideEffectHook(t)
	for _, command := range [][]string{nil, {}} {
		if got := readReplyGitField(context.Background(), repo.path, repo.env, command, 0); got != "main" {
			t.Fatalf("empty command lost built-in clean state: %q", got)
		}
	}
	repo.git(t, "config", "filter.sideeffect.clean", hook)
	replyGitSetFilterAttribute(t, repo)
	repo.write(t, "tracked", "evil\n")
	for _, command := range [][]string{nil, {}} {
		if got := readReplyGitField(context.Background(), repo.path, repo.env, command, 0); got != "" {
			t.Fatalf("empty command bypassed built-in filter safety: %q", got)
		}
	}
	replyGitExpectAbsent(t, marker)
}

func TestReadReplyGitFieldCustomGitLog(t *testing.T) {
	repo := newReplyGitRepository(t, true)
	want := repo.git(t, "rev-parse", "--short", "HEAD")
	command := []string{"git", "log", "-1", "--format=%h"}
	got := readReplyGitField(context.Background(), repo.path, repo.env, command, 0)
	if got != want {
		t.Fatalf("custom git log returned %q, want %q", got, want)
	}
}

func TestReadReplyGitFieldLiteralArgvWorkspaceAndEnvironment(t *testing.T) {
	workspace := t.TempDir()
	script := replyGitCommandScript(t, "[ \"$PWD\" = \"$EXPECTED_WORKSPACE\" ] || exit 1\n[ \"$#\" -eq 2 ] && [ -z \"$2\" ] || exit 2\n[ \"$GIT_DIR\" = custom ] && [ \"$GIT_TRACE2\" = user-owned ] || exit 3\nprintf '%s\\n' \"$1\"")
	environment := append(os.Environ(), "EXPECTED_WORKSPACE="+workspace, "GIT_DIR=custom", "GIT_TRACE2=user-owned")
	label := "  $HOME; echo **dev**`@a1b2c3d*  "
	got := readReplyGitField(context.Background(), workspace, environment, []string{script, label, ""}, 0)
	if got != label {
		t.Fatalf("custom command changed literal argv, workspace, environment or spaces: %q", got)
	}
}

func TestReadReplyGitFieldCustomOutputBoundaries(t *testing.T) {
	for _, scenario := range []struct {
		name string
		body string
		want string
	}{
		{"trailing-newlines", "printf 'dev@a1b2c3d*\\r\\n\\n'", "dev@a1b2c3d*"},
		{"empty", "printf ''", ""},
		{"newline-only", "printf '\\n'", ""},
		{"failed", "printf 'plausible*'; exit 1", ""},
		{"stderr-only", "printf 'private diagnostic' >&2", ""},
		{"multiline", "printf 'main\\nother\\n'", ""},
		{"unicode-line-separator", "printf 'main\u2028other'", ""},
		{"unicode-paragraph-separator", "printf 'main\u2029other'", ""},
		{"control", "printf 'main\\033[31m'", ""},
		{"nul", "printf 'main\\000hidden'", ""},
		{"invalid-utf8", "printf '\\377'", ""},
		{"oversized", "printf '%070000d' 0", ""},
		{"unicode-limit", "printf '" + strings.Repeat("界", 30) + "'", strings.Repeat("界", replyGitBranchSize/3)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			repo := newReplyGitRepository(t, true)
			script := replyGitCommandScript(t, scenario.body)
			got := readReplyGitField(context.Background(), repo.path, repo.env, []string{script}, 0)
			if got != scenario.want {
				t.Fatalf("custom output = %q, want %q; must not fall back to built-in Git", got, scenario.want)
			}
		})
	}
	repo := newReplyGitRepository(t, true)
	missing := filepath.Join(t.TempDir(), "missing")
	if got := readReplyGitField(context.Background(), repo.path, repo.env, []string{missing}, 0); got != "" {
		t.Fatalf("missing executable fell back to built-in Git: %q", got)
	}
}

func TestReadReplyGitFieldCustomCommandTimeout(t *testing.T) {
	script := replyGitCommandScript(t, "printf 'incomplete'; exec sleep 5")
	for _, parentTimeout := range []time.Duration{25 * time.Millisecond, 5 * time.Second} {
		t.Run(parentTimeout.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), parentTimeout)
			defer cancel()
			start := time.Now()
			got := readReplyGitField(ctx, t.TempDir(), os.Environ(), []string{script}, 0)
			elapsed := time.Since(start)
			if got != "" || elapsed > 2*time.Second {
				t.Fatalf("timed-out command returned %q after %s", got, elapsed)
			}
		})
	}
}

func TestCustomGitCommandFinalAndLateQuotaKeepSnapshot(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "usage-release")
	t.Setenv("OMP_TELEGRAM_FIXTURE_USAGE_RELEASE", gate)
	w, _, _ := replyUsageWorker(t, "hold-report")
	t.Cleanup(func() { requireStoreOK(t, os.WriteFile(gate, nil, 0600)) })
	script := replyGitCommandScript(t, "printf 'run\\n' >> custom-git-runs\ncat custom-git-label")
	w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{"git", "quota"}, GitCommand: []string{script}}
	labelPath := filepath.Join(w.binding.Workspace, "custom-git-label")
	label := "dev/``@a1b2c3d*"
	requireStoreOK(t, os.WriteFile(labelPath, []byte(label+"\n"), 0600))
	endStatusBarBody(t, w, "**answer** with `code`", "openai-codex")
	awaitReplyStatus(t, w)
	var id int64
	var header, body string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT id,header,text FROM outbox WHERE inbox_id=2").Scan(&id, &header, &body))
	if telegram.ConvertMarkdown(header) != "<code>"+label+"</code>" || body != "**answer** with `code`" {
		t.Fatalf("custom Git display corrupted literal label or answer: %q/%q", header, body)
	}
	deliverReplies(t, w)
	requireStoreOK(t, os.WriteFile(labelPath, []byte("changed"), 0600))
	requireStoreOK(t, os.WriteFile(gate, nil, 0600))
	awaitProviderUsage(t, w)
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT header,text FROM outbox WHERE id=?", id).Scan(&header, &body))
	if telegram.ConvertMarkdown(header) != "<code>"+label+"</code> | 5h30% 7d65%" || body != "**answer** with `code`" {
		t.Fatalf("late quota changed the captured custom label or answer: %q/%q", header, body)
	}
	runs, err := os.ReadFile(filepath.Join(w.binding.Workspace, "custom-git-runs"))
	requireStoreOK(t, err)
	if string(runs) != "run\n" {
		t.Fatalf("late quota reran the custom command: %q", runs)
	}
	deliverReplies(t, w)
}

func TestCustomGitCommandOnlyRunsAtResearchRoot(t *testing.T) {
	w, _, _ := researchWorker(t, "autoresearch")
	script := replyGitCommandScript(t, "printf 'run\\n' >> custom-git-runs\ncat custom-git-label")
	w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{"git"}, GitCommand: []string{script}}
	researchRoot(t, w, 100, "/autoresearch custom status")
	workspace := w.binding.Workspace
	requireStoreOK(t, os.WriteFile(filepath.Join(workspace, "custom-git-label"), []byte("startup"), 0600))
	researchRound(t, w, "middle")
	awaitReplyStatus(t, w)
	replyGitExpectAbsent(t, filepath.Join(workspace, "custom-git-runs"))
	requireStoreOK(t, os.WriteFile(filepath.Join(workspace, "custom-git-label"), []byte("dev@a1b2c3d*\n"), 0600))
	nativeRPC(t, w, "fixture_autoresearch_continue", nil)
	nativeUntil(t, w, func() bool { return w.research.roundOpen })
	researchFinishNaturally(t, w, "final")
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	rows := researchReplies(t, w, 100)
	if len(rows) != 2 || rows[0] != "middle" {
		t.Fatalf("custom Git command changed research replies: %q", rows)
	}
	header, body := attachedReplyBody(t, rows[1])
	if telegram.ConvertMarkdown(header) != "<code>dev@a1b2c3d*</code>" || body != "final" {
		t.Fatalf("research root lost completion-time custom output: %q", rows[1])
	}
}

func TestUnselectedGitDoesNotRunCustomCommand(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	script := replyGitCommandScript(t, "printf ran > custom-git-ran\nprintf 'dev*'")
	w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{}, GitCommand: []string{script}}
	endStatusBarBody(t, w, "answer", "safe-provider")
	awaitReplyStatus(t, w)
	replyGitExpectAbsent(t, filepath.Join(w.binding.Workspace, "custom-git-ran"))
	if rows := researchReplies(t, w, 2); len(rows) != 1 || rows[0] != "answer" || queueState(t, w, 2) != "done" {
		t.Fatalf("unselected custom Git command changed completion: %q", rows)
	}
}
