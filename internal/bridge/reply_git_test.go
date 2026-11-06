package bridge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"omp-telegram/internal/config"
)

type replyGitRepository struct {
	path string
	env  []string
}

func newReplyGitRepository(t *testing.T, committed bool) replyGitRepository {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	repo := replyGitRepository{path: filepath.Join(root, "workspace")}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, "GIT_") && name != "HOME" && name != "XDG_CONFIG_HOME" {
			repo.env = append(repo.env, entry)
		}
	}
	// Neither repository setup nor the helper reads or writes user configuration.
	repo.env = append(repo.env, "HOME="+root, "XDG_CONFIG_HOME="+root,
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if err := os.Mkdir(repo.path, 0700); err != nil {
		t.Fatal(err)
	}
	repo.git(t, "init", "-b", "main")
	repo.git(t, "config", "user.name", "Reply Git Test")
	repo.git(t, "config", "user.email", "reply-git@example.invalid")
	repo.git(t, "config", "commit.gpgsign", "false")
	repo.git(t, "config", "core.autocrlf", "false")
	if committed {
		repo.write(t, "tracked", "base\n")
		repo.git(t, "add", "tracked")
		repo.git(t, "commit", "-m", "base")
	}
	return repo
}

func (repo replyGitRepository) command(args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", repo.path}, args...)...)
	cmd.Env = repo.env
	return cmd
}

func (repo replyGitRepository) git(t *testing.T, args ...string) string {
	t.Helper()
	output, err := repo.command(args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (repo replyGitRepository) write(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo.path, name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func (repo replyGitRepository) expect(t *testing.T, want string) {
	t.Helper()
	if got := readReplyGit(context.Background(), repo.path, repo.env, 0); got != want {
		t.Fatalf("Git label = %q, want %q", got, want)
	}
}

func TestReadReplyGitBranchAndDirtyStates(t *testing.T) {
	for _, state := range []string{"clean", "modified", "staged", "deleted", "untracked", "ignored", "renamed", "unborn", "unborn-staged", "unborn-untracked", "detached", "detached-dirty", "literal-branch", "ahead", "behind"} {
		t.Run(state, func(t *testing.T) {
			repo := newReplyGitRepository(t, !strings.HasPrefix(state, "unborn"))
			want := "main"
			switch state {
			case "modified", "staged":
				repo.write(t, "tracked", "changed\n")
				if state == "staged" {
					repo.git(t, "add", "tracked")
				}
				want += "*"
			case "deleted":
				if err := os.Remove(filepath.Join(repo.path, "tracked")); err != nil {
					t.Fatal(err)
				}
				want += "*"
			case "untracked", "unborn-untracked":
				repo.write(t, "odd\n? filename", "new\n")
				want += "*"
			case "ignored":
				if err := os.WriteFile(filepath.Join(repo.path, ".git", "info", "exclude"), []byte("ignored\n"), 0600); err != nil {
					t.Fatal(err)
				}
				repo.write(t, "ignored", "not dirty\n")
			case "renamed":
				repo.git(t, "mv", "tracked", "? renamed\nfile")
				want += "*"
			case "unborn-staged":
				repo.write(t, "new", "new\n")
				repo.git(t, "add", "new")
				want += "*"
			case "detached", "detached-dirty":
				repo.git(t, "checkout", "--detach", "HEAD")
				want = "HEAD"
				if state == "detached-dirty" {
					repo.write(t, "tracked", "changed\n")
					want += "*"
				}
			case "literal-branch":
				repo.git(t, "checkout", "-b", "feature/literal_name")
				repo.write(t, "tracked", "changed\n")
				want = "feature/literal_name*"
			case "ahead", "behind":
				repo.git(t, "branch", "upstream")
				repo.git(t, "branch", "--set-upstream-to=upstream", "main")
				repo.write(t, "tracked", "second\n")
				repo.git(t, "commit", "-am", "second")
				if state == "behind" {
					repo.git(t, "branch", "-f", "upstream", "HEAD")
					repo.git(t, "reset", "--hard", "HEAD~")
				}
			}
			repo.expect(t, want)
		})
	}
}

func TestReadReplyGitConflict(t *testing.T) {
	repo := newReplyGitRepository(t, true)
	repo.git(t, "checkout", "-b", "other")
	repo.write(t, "tracked", "other\n")
	repo.git(t, "commit", "-am", "other")
	repo.git(t, "checkout", "main")
	repo.write(t, "tracked", "main\n")
	repo.git(t, "commit", "-am", "main")
	if output, err := repo.command("merge", "other").CombinedOutput(); err == nil {
		t.Fatalf("expected merge conflict, got %s", output)
	}
	repo.expect(t, "main*")
}

func TestReadReplyGitLinkedWorktree(t *testing.T) {
	repo := newReplyGitRepository(t, true)
	linked := filepath.Join(t.TempDir(), "linked")
	repo.git(t, "worktree", "add", "-b", "linked-branch", linked)
	repo.path = linked
	repo.expect(t, "linked-branch")
	repo.write(t, "untracked", "new\n")
	repo.expect(t, "linked-branch*")
}

func TestReadReplyGitSubmoduleDirtyDespiteIgnoreConfiguration(t *testing.T) {
	repo := newReplyGitRepository(t, true)
	source := newReplyGitRepository(t, true)
	repo.git(t, "-c", "protocol.file.allow=always", "submodule", "add", source.path, "module")
	repo.git(t, "commit", "-am", "submodule")
	repo.git(t, "config", "submodule.module.ignore", "all")
	repo.expect(t, "main")
	if err := os.WriteFile(filepath.Join(repo.path, "module", "tracked"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.expect(t, "main*")
}

func TestReadReplyGitSanitizesRoutingEnvironment(t *testing.T) {
	repo := newReplyGitRepository(t, true)
	other := newReplyGitRepository(t, true)
	other.git(t, "checkout", "-b", "wrong-repository")
	other.write(t, "dirty", "other\n")
	repo.env = append(repo.env,
		"GIT_DIR="+filepath.Join(other.path, ".git"), "GIT_COMMON_DIR="+filepath.Join(other.path, ".git"),
		"GIT_WORK_TREE="+other.path, "GIT_INDEX_FILE="+filepath.Join(other.path, ".git", "index"),
		"GIT_OBJECT_DIRECTORY="+filepath.Join(other.path, ".git", "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/does/not/exist", "GIT_CEILING_DIRECTORIES="+repo.path,
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.worktree", "GIT_CONFIG_VALUE_0="+other.path,
		"GIT_CONFIG_PARAMETERS='core.worktree'='"+other.path+"'")
	repo.expect(t, "main")
	input := []string{"HOME=/test-home", "PATH=/test-path", "LANG=en_US.UTF-8", "GIT_DIR=/wrong", "GIT_CONFIG_KEY_0=core.worktree", "ORDINARY=value"}
	want := []string{"HOME=/test-home", "PATH=/test-path", "LANG=en_US.UTF-8", "ORDINARY=value",
		"GIT_TRACE2=0", "GIT_TRACE2_EVENT=0", "GIT_TRACE2_PERF=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1"}
	if got := replyGitEnvironment(input); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("filtered environment = %q, want %q", got, want)
	}
	if input[3] != "GIT_DIR=/wrong" {
		t.Fatal("environment filtering mutated the input snapshot")
	}
}

func TestReadReplyGitDoesNotRefreshIndexOrRunFsmonitor(t *testing.T) {
	repo := newReplyGitRepository(t, true)
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hook := filepath.Join(t.TempDir(), "fsmonitor")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf ran > '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	repo.git(t, "config", "core.fsmonitor", hook)
	trace := filepath.Join(t.TempDir(), "git-trace")
	trace2 := filepath.Join(t.TempDir(), "git-trace2")
	repo.env = append(repo.env, "GIT_TRACE="+trace, "GIT_TRACE2_EVENT="+trace2)
	index := filepath.Join(repo.path, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(946684800, 0)
	if err := os.Chtimes(index, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	// Force the index's cached stat information to need refreshing, without
	// changing the tracked contents.
	if err := os.Chtimes(filepath.Join(repo.path, "tracked"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	repo.expect(t, "main")
	repo.write(t, "tracked", "modified\n")
	repo.expect(t, "main*")
	repo.write(t, "tracked", "base\n")
	repo.expect(t, "main")
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("status changed index bytes or modification time")
	}
	for _, path := range []string{marker, trace, trace2, index + ".lock"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unexpected status side effect at %q: %v", path, err)
		}
	}
}

func TestReadReplyGitUnknownAndUnsafeRepositories(t *testing.T) {
	repo := newReplyGitRepository(t, true)
	for _, workspace := range []string{"", ".", "relative", t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		if got := readReplyGit(context.Background(), workspace, repo.env, 0); got != "" {
			t.Fatalf("invalid workspace returned %q", got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := readReplyGit(ctx, repo.path, repo.env, 0); got != "" {
		t.Fatalf("cancelled read returned %q", got)
	}
	t.Run("safe-directory", func(t *testing.T) {
		unsafe := newReplyGitRepository(t, true)
		unsafe.env = append(unsafe.env, "GIT_TEST_ASSUME_DIFFERENT_OWNER=1")
		unsafe.expect(t, "")
	})
	t.Run("corrupt-index", func(t *testing.T) {
		corrupt := newReplyGitRepository(t, true)
		if err := os.WriteFile(filepath.Join(corrupt.path, ".git", "index"), []byte("truncated"), 0600); err != nil {
			t.Fatal(err)
		}
		corrupt.expect(t, "")
	})
}

func TestReadReplyGitBoundsOutputAndClipsUnicodeBeforeDirtyStar(t *testing.T) {
	t.Run("output-cap", func(t *testing.T) {
		repo := newReplyGitRepository(t, true)
		for i := range 800 {
			repo.write(t, fmt.Sprintf("%04d-%s", i, strings.Repeat("x", 100)), "new\n")
		}
		repo.expect(t, "")
	})
	t.Run("unicode-label", func(t *testing.T) {
		repo := newReplyGitRepository(t, true)
		branch := strings.Repeat("界", 30) + "_literal"
		repo.git(t, "checkout", "-b", branch)
		repo.write(t, "dirty", "new\n")
		got := readReplyGit(context.Background(), repo.path, repo.env, 0)
		want := strings.Repeat("界", replyGitBranchSize/3) + "*"
		if got != want || !utf8.ValidString(got) || strings.Contains(got, "\\*") {
			t.Fatalf("clipped raw label = %q, want %q", got, want)
		}
	})
}

func TestReadReplyGitBoundsCommandTime(t *testing.T) {
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo is unavailable")
	}
	for _, parentTimeout := range []time.Duration{30 * time.Millisecond, 2 * time.Second} {
		t.Run(parentTimeout.String(), func(t *testing.T) {
			repo := newReplyGitRepository(t, true)
			head := filepath.Join(repo.path, ".git", "HEAD")
			if err := os.Remove(head); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command(mkfifo, head).CombinedOutput(); err != nil {
				t.Fatalf("create blocking HEAD: %v, %s", err, output)
			}
			ctx, cancel := context.WithTimeout(context.Background(), parentTimeout)
			defer cancel()
			start := time.Now()
			got := readReplyGit(ctx, repo.path, repo.env, 0)
			elapsed := time.Since(start)
			if got != "" || elapsed > time.Second {
				t.Fatalf("blocked Git returned %q after %s", got, elapsed)
			}
			if elapsed < min(parentTimeout, config.DefaultGitTimeout)/2 {
				t.Fatalf("Git did not exercise the blocking read: %s", elapsed)
			}
		})
	}
}

func TestParseReplyGitStatusRejectsIncompleteRecords(t *testing.T) {
	oid := strings.Repeat("a", 40)
	header := "# branch.oid " + oid + "\x00# branch.head main\x00"
	ordinary := "1 .M N... 100644 100644 100644 " + oid + " " + oid + " tracked\x00"
	rename := "2 R. N... 100644 100644 100644 " + oid + " " + oid + " R100 destination\x00source\x00"
	for name, output := range map[string]string{
		"empty": "", "missing-head": "# branch.oid " + oid + "\x00",
		"missing-oid": "# branch.head main\x00", "unterminated-header": strings.TrimSuffix(header, "\x00"),
		"unterminated-file":          header + strings.TrimSuffix(ordinary, "\x00"),
		"unterminated-rename-source": header + strings.TrimSuffix(rename, "\x00"),
		"missing-rename-source":      header + strings.Split(rename, "\x00")[0] + "\x00",
		"empty-record":               header + "\x00", "unknown-record": header + "garbage\x00",
		"short-file-record": header + "1 .M\x00", "empty-path": header + "? \x00",
		"bad-mode":                 header + strings.Replace(ordinary, "100644", "999999", 1),
		"bad-object-id":            header + strings.Replace(ordinary, oid, "not-an-oid", 1),
		"bad-status":               header + strings.Replace(ordinary, ".M", "zz", 1),
		"bad-submodule":            header + strings.Replace(ordinary, "N...", "Nbad", 1),
		"duplicate-head":           header + "# branch.head other\x00",
		"duplicate-oid":            header + "# branch.oid " + oid + "\x00",
		"invalid-utf8-head":        "# branch.oid " + oid + "\x00# branch.head bad\xff\x00",
		"control-head":             "# branch.oid " + oid + "\x00# branch.head bad\nbranch\x00",
		"invalid-ahead-behind":     header + "# branch.ab +1 broken\x00",
		"malformed-unknown-ahead":  header + "# branch.ab +?1 -?\x00",
		"malformed-unknown-behind": header + "# branch.ab +? -??\x00",
	} {
		t.Run(name, func(t *testing.T) {
			if got := parseReplyGitStatus(output); got != "" {
				t.Fatalf("invalid output returned %q", got)
			}
		})
	}
	for _, output := range []string{header + ordinary, header + rename, header + "? odd\nname\x00"} {
		if got := parseReplyGitStatus(output); got != "main*" {
			t.Fatalf("valid dirty records returned %q", got)
		}
	}
	for _, counts := range []string{"+3 -2", "+? -?"} {
		branchHeader := header + "# branch.ab " + counts + "\x00"
		if got := parseReplyGitStatus(branchHeader + "! ignored\x00"); got != "main" {
			t.Fatalf("ahead/behind %q or ignored record marked dirty: %q", counts, got)
		}
		if got := parseReplyGitStatus(branchHeader + ordinary); got != "main*" {
			t.Fatalf("ahead/behind %q hid dirty record: %q", counts, got)
		}
	}
}

func TestReplyGitOutputExactLimit(t *testing.T) {
	var output replyGitOutput
	data := bytes.Repeat([]byte{'x'}, replyGitMaxOutput)
	if n, err := output.Write(data); n != len(data) || err != nil || output.overflow {
		t.Fatalf("exact output limit: n=%d err=%v overflow=%t", n, err, output.overflow)
	}
	if _, err := output.Write([]byte{'x'}); err == nil || !output.overflow || len(output.data) != replyGitMaxOutput {
		t.Fatal("overflow was not rejected without growing the buffer")
	}
}
