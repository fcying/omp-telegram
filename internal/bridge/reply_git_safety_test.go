package bridge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"omp-telegram/internal/config"
)

func replyGitSideEffectHook(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	marker := filepath.Join(root, "ran")
	hook := filepath.Join(root, "filter")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf ran > '"+marker+"'\ncat\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return hook, marker
}

func replyGitExpectAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unexpected Git side effect at %q: %v", path, err)
		}
	}
}

func replyGitSetFilterAttribute(t *testing.T, repo replyGitRepository) {
	t.Helper()
	gitDir := repo.git(t, "rev-parse", "--absolute-git-dir")
	if err := os.WriteFile(filepath.Join(gitDir, "info", "attributes"), []byte("tracked filter=sideeffect\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReadReplyGitDoesNotExecuteActiveFilters(t *testing.T) {
	for _, kind := range []string{"clean", "process"} {
		t.Run(kind, func(t *testing.T) {
			repo := newReplyGitRepository(t, true)
			hook, marker := replyGitSideEffectHook(t)
			repo.git(t, "config", "filter.sideeffect."+kind, hook)
			repo.git(t, "config", "filter.sideeffect.required", "true")
			replyGitSetFilterAttribute(t, repo)
			// Same size as the index entry: status must not hash the contents
			// through a clean or long-running process filter to decide dirtiness.
			repo.write(t, "tracked", "evil\n")
			stamp := time.Unix(946684800, 0)
			if err := os.Chtimes(filepath.Join(repo.path, "tracked"), stamp, stamp); err != nil {
				t.Fatal(err)
			}
			index := filepath.Join(repo.path, ".git", "index")
			before, err := os.ReadFile(index)
			if err != nil {
				t.Fatal(err)
			}
			beforeInfo, err := os.Stat(index)
			if err != nil {
				t.Fatal(err)
			}
			repo.expect(t, "")
			after, err := os.ReadFile(index)
			if err != nil {
				t.Fatal(err)
			}
			afterInfo, err := os.Stat(index)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
				t.Fatal("filter preflight changed index bytes or modification time")
			}
			replyGitExpectAbsent(t, marker, index+".lock")
		})
	}
}

func TestReadReplyGitUnusedGlobalFiltersRemainVisible(t *testing.T) {
	repo := newReplyGitRepository(t, true)
	hook, marker := replyGitSideEffectHook(t)
	config := filepath.Join(t.TempDir(), "global-config")
	contents := fmt.Sprintf("[filter \"sideeffect\"]\n clean = %s\n process = %s\n required = true\n", hook, hook)
	if err := os.WriteFile(config, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	repo.env = append(repo.env, "GIT_CONFIG_GLOBAL="+config)
	repo.expect(t, "main")
	if err := os.WriteFile(filepath.Join(repo.path, ".git", "info", "attributes"), []byte("tracked -filter\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo.expect(t, "main")
	repo.write(t, "tracked", "evil\n")
	repo.expect(t, "main*")
	replyGitExpectAbsent(t, marker)
}

func TestReadReplyGitDisablesConfiguredTrace2Targets(t *testing.T) {
	for _, scope := range []string{"GLOBAL", "SYSTEM"} {
		for _, zeros := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/inherited-zeros=%t", scope, zeros), func(t *testing.T) {
				repo := newReplyGitRepository(t, true)
				root := t.TempDir()
				config := filepath.Join(root, "config")
				normal := filepath.Join(root, "normal")
				event := filepath.Join(root, "event")
				perf := filepath.Join(root, "perf")
				contents := fmt.Sprintf("[trace2]\n normalTarget = %s\n eventTarget = %s\n perfTarget = %s\n", normal, event, perf)
				if err := os.WriteFile(config, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
				repo.env = append(repo.env, "GIT_CONFIG_"+scope+"="+config)
				if zeros {
					repo.env = append(repo.env, "GIT_TRACE2=0", "GIT_TRACE2_EVENT=0", "GIT_TRACE2_PERF=0")
				}
				repo.expect(t, "main")
				replyGitExpectAbsent(t, normal, event, perf)
			})
		}
	}
}

func TestReadReplyGitPreflightsSubmoduleFiltersAndFsmonitor(t *testing.T) {
	for _, kind := range []string{"clean", "process"} {
		t.Run(kind, func(t *testing.T) {
			repo := newReplyGitRepository(t, true)
			source := newReplyGitRepository(t, true)
			repo.git(t, "-c", "protocol.file.allow=always", "submodule", "add", source.path, "module")
			repo.git(t, "commit", "-am", "submodule")
			module := replyGitRepository{path: filepath.Join(repo.path, "module"), env: repo.env}
			hook, filterMarker := replyGitSideEffectHook(t)
			monitor, monitorMarker := replyGitSideEffectHook(t)
			module.git(t, "config", "filter.sideeffect."+kind, hook)
			replyGitSetFilterAttribute(t, module)
			module.git(t, "config", "core.fsmonitor", monitor)
			module.write(t, "tracked", "evil\n")
			repo.expect(t, "")
			replyGitExpectAbsent(t, filterMarker, monitorMarker)
		})
	}
}

func TestReadReplyGitIncludesSubmoduleWorktreeAndUntrackedChanges(t *testing.T) {
	for _, state := range []string{"untracked", "tracked", "commit"} {
		t.Run(state, func(t *testing.T) {
			repo := newReplyGitRepository(t, true)
			source := newReplyGitRepository(t, true)
			repo.git(t, "-c", "protocol.file.allow=always", "submodule", "add", source.path, "module")
			repo.git(t, "commit", "-am", "submodule")
			module := replyGitRepository{path: filepath.Join(repo.path, "module"), env: repo.env}
			module.git(t, "config", "user.name", "Reply Git Test")
			module.git(t, "config", "user.email", "reply-git@example.invalid")
			module.git(t, "config", "commit.gpgsign", "false")
			repo.expect(t, "main")
			if state == "untracked" {
				module.write(t, "new", "new\n")
			} else {
				module.write(t, "tracked", "evil\n")
				if state == "commit" {
					module.git(t, "commit", "-am", "changed")
				}
			}
			monitor, marker := replyGitSideEffectHook(t)
			module.git(t, "config", "core.fsmonitor", monitor)
			repo.expect(t, "main*")
			replyGitExpectAbsent(t, marker)
		})
	}
}

func TestReplyGitGuardsRejectNewAttributesForKnownDrivers(t *testing.T) {
	for _, kind := range []string{"clean", "process"} {
		t.Run(kind, func(t *testing.T) {
			repo := newReplyGitRepository(t, true)
			hook, marker := replyGitSideEffectHook(t)
			repo.git(t, "config", "filter.sideeffect."+kind, hook)
			ctx, cancel := context.WithTimeout(context.Background(), config.DefaultGitTimeout)
			defer cancel()
			probe := replyGitProbe{ctx: ctx, environment: replyGitEnvironment(repo.env), remaining: replyGitMaxOutput}
			if !probe.preflight(repo.path) {
				t.Fatal("unused filter failed preflight")
			}
			// Introduce the attribute only after the safety check. The real Git
			// command must fail instead of treating disabled filters as identity.
			replyGitSetFilterAttribute(t, repo)
			repo.write(t, "tracked", "evil\n")
			stamp := time.Unix(946684800, 0)
			if err := os.Chtimes(filepath.Join(repo.path, "tracked"), stamp, stamp); err != nil {
				t.Fatal(err)
			}
			if _, ok := probe.run(repo.path, nil, "status", "--porcelain=v2", "--branch",
				"--untracked-files=normal", "--ignore-submodules=dirty", "-z"); ok {
				t.Fatal("status accepted an unavailable required filter")
			}
			replyGitExpectAbsent(t, marker)
		})
	}
}
