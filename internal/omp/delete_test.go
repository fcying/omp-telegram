package omp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDeleteMockChild(t *testing.T) {
	if os.Getenv("OMP_TEST_DELETE_CHILD") != "1" {
		return
	}
	file := os.Getenv("OMP_TEST_DELETE_FILE")
	artifacts := os.Getenv("OMP_TEST_DELETE_ARTIFACTS")
	mode := os.Getenv("OMP_TEST_DELETE_MODE")
	if mode == "policy" && (os.Getenv(botTokenName) != "" || os.Getenv("OMP_TEST_DELETE_SHOULD_HIDE") != "" || os.Getenv("OMP_TEST_DELETE_KEEP") != "permitted") {
		os.Exit(2)
	}
	emit := func(frame any) {
		data, _ := json.Marshal(frame)
		fmt.Println(string(data))
	}
	reply := func(command map[string]any, success bool, data any) {
		emit(map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": success, "data": data})
	}
	emit(map[string]any{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": []int{1, 2}, "maxFrameBytes": 1048576, "maxReassembledFrameBytes": 67108864})
	input := bufio.NewScanner(os.Stdin)
	awaitEOF := func() {
		if !input.Scan() {
			_ = os.WriteFile(file, []byte("resurrected"), 0600)
			os.Exit(0)
		}
	}
	for input.Scan() {
		var command map[string]any
		if json.Unmarshal(input.Bytes(), &command) != nil {
			os.Exit(2)
		}
		switch command["type"] {
		case "negotiate_protocol":
			reply(command, true, map[string]any{"accepted": true})
		case "get_state":
			id := "native-id"
			if mode == "state_mismatch" || mode == "identity_mismatch" {
				id = "other-id"
			}
			reply(command, true, map[string]any{"sessionId": id, "sessionFile": file})
		case "prompt":
			switch command["message"] {
			case "/session info":
				if mode != "missing_info" {
					id := "native-id"
					if mode == "info_mismatch" || mode == "identity_mismatch" {
						id = "other-id"
					}
					cwd := filepath.Dir(file)
					if original := os.Getenv("OMP_TEST_DELETE_CWD"); original != "" {
						cwd = original
					}
					if mode == "cwd_mismatch" {
						cwd = os.TempDir()
					}
					emit(map[string]any{"type": "command_output", "text": "Session: " + id + "\nTitle: fixture\nCWD: " + cwd})
				}
				reply(command, true, map[string]any{"agentInvoked": false})
			case "/session delete":
				if reached := os.Getenv("OMP_TEST_DELETE_REACHED"); reached != "" {
					_ = os.WriteFile(reached, []byte("reached"), 0600)
				}
				deleted := mode == "success" || strings.HasPrefix(mode, "success_") || strings.HasPrefix(mode, "deleted_") || mode == "policy"
				if deleted {
					if err := os.Remove(file); err != nil {
						os.Exit(3)
					}
					if artifacts != "" {
						if err := os.RemoveAll(artifacts); err != nil {
							os.Exit(3)
						}
					}
					if mutated := os.Getenv("OMP_TEST_DELETE_MUTATED"); mutated != "" {
						_ = os.WriteFile(mutated, []byte("mutated"), 0600)
					}
					cwd := os.Getenv("OMP_TEST_DELETE_CWD")
					if mode == "success_nonempty" {
						if err := os.WriteFile(filepath.Join(cwd, "keep"), []byte("user data"), 0600); err != nil {
							os.Exit(3)
						}
					}
					if mode == "success_replaced" {
						if os.Rename(cwd, cwd+"-moved") != nil || os.Mkdir(cwd, 0700) != nil {
							os.Exit(3)
						}
					}
				}
				if mode == "timeout" || mode == "cancel" {
					for {
						time.Sleep(time.Second)
					}
				}
				if mode == "deleted_timeout" || mode == "deleted_cancel" || mode == "deleted_output_lost" {
					if mode == "deleted_output_lost" {
						_ = os.Stdout.Close()
					}
					awaitEOF()
					continue
				}
				if mode == "deleted_protocol" {
					fmt.Println("{invalid-frame")
					awaitEOF()
					continue
				}
				if mode == "reject" {
					reply(command, false, map[string]any{"error": "SECRET_DELETE_DIAGNOSTIC"})
					continue
				}
				if mode == "native_failure" {
					emit(map[string]any{"type": "command_output", "text": "Failed to delete session: SECRET_DELETE_DIAGNOSTIC " + file})
					reply(command, true, map[string]any{"agentInvoked": false})
					break
				}
				if mode == "unrelated_events" {
					for range 128 {
						emit(map[string]any{"type": "agent_start"})
					}
					reply(command, true, map[string]any{"agentInvoked": false})
					continue
				}
				if mode != "missing_output" && mode != "deleted_missing_output" && mode != "deleted_bad_ack" {
					text := "Session deleted: " + file + ". Use ACP to resume another session."
					if mode == "wrong_output" || mode == "deleted_wrong_output" {
						text = "Session deleted: " + file + "-different. Use ACP to resume another session."
					}
					emit(map[string]any{"type": "agent_start"})
					emit(map[string]any{"type": "command_output", "text": text})
				}
				if mode == "deleted_bad_ack" {
					reply(command, true, map[string]any{"agentInvoked": true})
				} else {
					reply(command, true, map[string]any{"agentInvoked": mode == "agent_invoked"})
				}
				if deleted {
					// Native OMP recreates history on EOF after deletion; SIGKILL must precede stdin closure.
					awaitEOF()
				}
			default:
				os.Exit(2)
			}
		default:
			os.Exit(2)
		}
	}
	os.Exit(0)
}

func deletionFixture(t *testing.T, mode string) (Config, string, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(file, []byte("native history"), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := filepath.Join(t.TempDir(), "argv")
	script := filepath.Join(t.TempDir(), "mock-omp")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	contents := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + quote(argv) + "\nexec " + quote(executable) + " -test.run='^TestDeleteMockChild$'\n"
	if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TEST_DELETE_CHILD", "1")
	t.Setenv("OMP_TEST_DELETE_FILE", file)
	t.Setenv("OMP_TEST_DELETE_MODE", mode)
	return Config{Binary: script, CWD: filepath.Dir(file)}, file, argv
}

func TestDeleteSessionNativeCommandAndHardStop(t *testing.T) {
	cfg, file, argv := deletionFixture(t, "success")
	cfg.Args = []string{"--model", "fixture-model"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := DeleteSession(ctx, cfg, "native-id", testRPCLogger()); err != nil {
		t.Fatalf("native deletion failed: %v", err)
	}
	args, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--mode", "rpc", "--cwd", cfg.CWD, "--resume", "native-id", "--model", "fixture-model"}
	if strings.TrimSpace(string(args)) != strings.Join(want, "\n") {
		t.Fatalf("unexpected RPC argv: %q", args)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted file was recreated or remained")
	}
}

func TestDeleteSessionPreservesChildEnvironmentPolicy(t *testing.T) {
	cfg, file, _ := deletionFixture(t, "policy")
	t.Setenv(botTokenName, "private-token")
	t.Setenv("OMP_TEST_DELETE_SHOULD_HIDE", "private")
	t.Setenv("OMP_TEST_DELETE_KEEP", "permitted")
	policy, err := NewEnvironment("denylist", nil, []string{"OMP_TEST_DELETE_SHOULD_HIDE", botTokenName})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Environment = policy
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := DeleteSession(ctx, cfg, "native-id", testRPCLogger()); err != nil {
		t.Fatalf("native deletion did not apply child environment policy: %v", err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("native deletion with restricted environment left the file")
	}
}

func TestDeleteSessionRejectsUncertainDeletion(t *testing.T) {
	for _, mode := range []string{"state_mismatch", "identity_mismatch", "info_mismatch", "cwd_mismatch", "missing_info", "agent_invoked", "missing_output", "wrong_output", "no_unlink", "reject", "unrelated_events"} {
		t.Run(mode, func(t *testing.T) {
			cfg, file, _ := deletionFixture(t, mode)
			reached := filepath.Join(t.TempDir(), "reached")
			t.Setenv("OMP_TEST_DELETE_REACHED", reached)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := DeleteSession(ctx, cfg, "native-id", testRPCLogger())
			mustReachDelete := mode != "state_mismatch" && mode != "identity_mismatch" && mode != "info_mismatch" && mode != "cwd_mismatch" && mode != "missing_info"
			_, reachErr := os.Stat(reached)
			if mustReachDelete && reachErr != nil || !mustReachDelete && !errors.Is(reachErr, os.ErrNotExist) {
				t.Fatalf("native deletion stage mismatch: %v", reachErr)
			}
			if err == nil || strings.Contains(err.Error(), file) || strings.Contains(err.Error(), "SECRET_DELETE_DIAGNOSTIC") {
				t.Fatalf("unverified deletion accepted or sensitive error: %v", err)
			}
			wantReason := "native_unconfirmed"
			switch mode {
			case "state_mismatch", "info_mismatch", "missing_info":
				wantReason = "identity_unconfirmed"
			case "identity_mismatch", "cwd_mismatch":
				wantReason = "identity_mismatch"
			case "missing_output":
				wantReason = "timeout"
			}
			if got := ClassifyError(err); got != wantReason {
				t.Fatalf("deletion reason = %q, want %q", got, wantReason)
			}
			if errors.Is(err, ErrSessionFileGone) {
				t.Fatal("native file remained but deletion was reported as missing")
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("bridge deleted a file without native proof: %v", err)
			}
		})
	}
}

func TestDeleteSessionRejectsInvalidRequest(t *testing.T) {
	cfg, file, argv := deletionFixture(t, "success")
	cfg.Resume = "other-id"
	if err := DeleteSession(context.Background(), cfg, "native-id", testRPCLogger()); err == nil || ClassifyError(err) != "invalid_request" {
		t.Fatalf("invalid request outcome = %v, reason = %q", err, ClassifyError(err))
	}
	if _, err := os.Stat(argv); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid request started a native process")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("invalid request altered session file")
	}
}

func TestDeleteSessionCancellationAndDeadline(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			cfg, file, _ := deletionFixture(t, mode)
			reached := filepath.Join(t.TempDir(), "reached")
			t.Setenv("OMP_TEST_DELETE_REACHED", reached)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if mode == "cancel" {
				go func() {
					for {
						if _, err := os.Stat(reached); err == nil {
							cancel()
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Millisecond):
						}
					}
				}()
			} else {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, time.Second)
				defer stop()
			}
			err := DeleteSession(ctx, cfg, "native-id", testRPCLogger())
			if _, statErr := os.Stat(reached); statErr != nil {
				t.Fatalf("native delete command was not reached: %v", statErr)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) || mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("interrupted deletion returned %v", err)
			}
			wantReason := "timeout"
			if mode == "cancel" {
				wantReason = "cancelled"
			}
			if got := ClassifyError(err); got != wantReason {
				t.Fatalf("interrupted deletion reason = %q, want %q", got, wantReason)
			}
			if errors.Is(err, ErrSessionFileGone) {
				t.Fatal("interrupted deletion reported the existing native file as missing")
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("interrupted deletion removed session: %v", err)
			}
		})
	}
}

func TestDeleteSessionNeverResurrectsAfterUncertainMutation(t *testing.T) {
	for _, mode := range []string{"deleted_timeout", "deleted_cancel", "deleted_output_lost", "deleted_protocol", "deleted_missing_output", "deleted_wrong_output", "deleted_bad_ack"} {
		t.Run(mode, func(t *testing.T) {
			cfg, file, _ := deletionFixture(t, mode)
			artifacts := filepath.Join(t.TempDir(), "artifacts")
			if err := os.Mkdir(artifacts, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(artifacts, "transcript"), []byte("private"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("OMP_TEST_DELETE_ARTIFACTS", artifacts)
			mutated := filepath.Join(t.TempDir(), "mutated")
			t.Setenv("OMP_TEST_DELETE_MUTATED", mutated)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if mode == "deleted_cancel" {
				go func() {
					for {
						if _, err := os.Stat(mutated); err == nil {
							cancel()
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Millisecond):
						}
					}
				}()
			} else if mode == "deleted_timeout" || mode == "deleted_missing_output" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, time.Second)
				defer stop()
			}
			err := DeleteSession(ctx, cfg, "native-id", testRPCLogger())
			if _, statErr := os.Stat(mutated); statErr != nil {
				t.Fatalf("child did not perform the deletion: %v", statErr)
			}
			if err == nil || strings.Contains(err.Error(), file) || strings.Contains(err.Error(), "SECRET_DELETE_DIAGNOSTIC") {
				t.Fatalf("uncertain native deletion returned %v", err)
			}
			if !errors.Is(err, ErrSessionFileGone) {
				t.Fatalf("missing native file was not reported after uncertain deletion: %v", err)
			}
			if mode == "deleted_cancel" && !errors.Is(err, context.Canceled) || mode == "deleted_timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("interrupted deletion returned %v", err)
			}
			if _, statErr := os.Stat(file); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("deleted JSONL was resurrected: %v", statErr)
			}
			if _, statErr := os.Stat(artifacts); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("deleted artifacts remain: %v", statErr)
			}
		})
	}
}

func TestDeleteSessionNativeFailureOutput(t *testing.T) {
	cfg, file, _ := deletionFixture(t, "native_failure")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	start := time.Now()
	err := DeleteSession(ctx, cfg, "native-id", testRPCLogger())
	if err == nil || strings.Contains(err.Error(), file) || strings.Contains(err.Error(), "SECRET_DELETE_DIAGNOSTIC") {
		t.Fatalf("native failure leaked diagnostics or succeeded: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("native failure output was not handled promptly: %v", elapsed)
	}
	contents, statErr := os.ReadFile(file)
	if statErr != nil || string(contents) != "native history" {
		t.Fatalf("failed native command changed session file: %v", statErr)
	}
}

func TestDeleteBoundSessionWorkspaceLifetime(t *testing.T) {
	for _, mode := range []string{"success", "native_failure", "file_mismatch", "preexisting", "success_nonempty", "success_replaced"} {
		t.Run(mode, func(t *testing.T) {
			nativeMode := mode
			if mode == "file_mismatch" || mode == "preexisting" {
				nativeMode = "success"
			}
			cfg, file, _ := deletionFixture(t, nativeMode)
			ancestor := t.TempDir()
			created := filepath.Join(ancestor, "missing")
			cfg.CWD = filepath.Join(created, "workspace")
			keep := filepath.Join(ancestor, "keep")
			if err := os.WriteFile(keep, []byte("existing data"), 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "preexisting" {
				if err := os.MkdirAll(cfg.CWD, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cfg.CWD, "keep"), []byte("user data"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("OMP_TEST_DELETE_CWD", cfg.CWD)
			reached := filepath.Join(ancestor, "reached")
			t.Setenv("OMP_TEST_DELETE_REACHED", reached)
			expected := file
			if mode == "file_mismatch" {
				expected = filepath.Join(ancestor, "other.jsonl")
				if err := os.WriteFile(expected, []byte("other history"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := DeleteBoundSession(ctx, cfg, "native-id", expected, testRPCLogger())
			partial := mode == "success_nonempty" || mode == "success_replaced"
			failed := partial || mode == "native_failure" || mode == "file_mismatch"
			if (err != nil) != failed || errors.Is(err, ErrSessionFileGone) != partial {
				t.Fatalf("unexpected deletion outcome: %v", err)
			}
			if failed {
				wantReason := "native_failed"
				if partial {
					wantReason = "cleanup_failed"
				} else if mode == "file_mismatch" {
					wantReason = "identity_mismatch"
				}
				if got := ClassifyError(err); got != wantReason {
					t.Fatalf("bound deletion reason = %q, want %q", got, wantReason)
				}
			}
			if err != nil && (strings.Contains(err.Error(), file) || strings.Contains(err.Error(), cfg.CWD) || strings.Contains(err.Error(), "SECRET_DELETE_DIAGNOSTIC")) {
				t.Fatalf("sensitive deletion error: %v", err)
			}
			_, reachErr := os.Stat(reached)
			if mode == "file_mismatch" {
				if !errors.Is(reachErr, os.ErrNotExist) {
					t.Fatal("mismatched persisted file reached destructive command")
				}
				if data, err := os.ReadFile(expected); err != nil || string(data) != "other history" {
					t.Fatalf("mismatched persisted file changed: %v", err)
				}
			} else if reachErr != nil {
				t.Fatalf("native deletion did not run in recreated workspace: %v", reachErr)
			}
			if mode == "native_failure" || mode == "file_mismatch" {
				if data, err := os.ReadFile(file); err != nil || string(data) != "native history" {
					t.Fatalf("failed deletion changed native history: %v", err)
				}
			} else if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("native history remained or was resurrected: %v", err)
			}
			if mode == "preexisting" || mode == "success_nonempty" {
				if data, err := os.ReadFile(filepath.Join(cfg.CWD, "keep")); err != nil || string(data) != "user data" {
					t.Fatalf("workspace data was removed: %v", err)
				}
			} else if mode == "success_replaced" {
				for _, path := range []string{cfg.CWD, cfg.CWD + "-moved"} {
					if info, err := os.Lstat(path); err != nil || !info.IsDir() {
						t.Fatalf("replacement or moved directory was removed: %v", err)
					}
				}
			} else if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned empty workspace was not cleaned: %v", err)
			}
			if data, err := os.ReadFile(keep); err != nil || string(data) != "existing data" {
				t.Fatalf("preexisting ancestor data changed: %v", err)
			}
		})
	}
}

func TestDeleteBoundSessionRejectsInvalidPathsBeforeStart(t *testing.T) {
	for _, mode := range []string{"relative_file", "unclean_file", "missing_file", "symlink_file", "symlink_workspace", "nondirectory_workspace"} {
		t.Run(mode, func(t *testing.T) {
			cfg, file, argv := deletionFixture(t, "success")
			ancestor := t.TempDir()
			created := filepath.Join(ancestor, "missing")
			cfg.CWD = filepath.Join(created, "workspace")
			expected := file
			switch mode {
			case "relative_file":
				expected = "session.jsonl"
			case "unclean_file":
				expected = filepath.Dir(file) + "/./session.jsonl"
			case "missing_file":
				expected = filepath.Join(ancestor, "absent.jsonl")
			case "symlink_file":
				expected = filepath.Join(ancestor, "linked.jsonl")
				if err := os.Symlink(file, expected); err != nil {
					t.Fatal(err)
				}
			case "symlink_workspace":
				if err := os.Symlink(t.TempDir(), created); err != nil {
					t.Fatal(err)
				}
			case "nondirectory_workspace":
				if err := os.WriteFile(created, []byte("user data"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := DeleteBoundSession(context.Background(), cfg, "native-id", expected, testRPCLogger())
			if err == nil || strings.Contains(err.Error(), file) || strings.Contains(err.Error(), cfg.CWD) {
				t.Fatalf("invalid path accepted or exposed: %v", err)
			}
			if errors.Is(err, ErrBoundSessionMissing) != (mode == "missing_file") {
				t.Fatalf("incorrect missing-file classification for %s: %v", mode, err)
			}
			wantReason := "workspace_unavailable"
			switch mode {
			case "relative_file", "unclean_file":
				wantReason = "invalid_request"
			case "missing_file":
				wantReason = "file_missing"
			case "symlink_file":
				wantReason = "file_unavailable"
			}
			if got := ClassifyError(err); got != wantReason {
				t.Fatalf("invalid path reason = %q, want %q", got, wantReason)
			}
			if _, err := os.Stat(argv); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid path started native child")
			}
			if _, err := os.Lstat(created); mode != "symlink_workspace" && mode != "nondirectory_workspace" && !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid expected file created workspace")
			}
			if data, err := os.ReadFile(file); err != nil || string(data) != "native history" {
				t.Fatalf("invalid request changed native history: %v", err)
			}
		})
	}
}

func TestDeleteSessionStartupFailureRetainsHistory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(file, []byte("native history"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Binary: filepath.Join(root, "missing-omp"), CWD: root}
	err := DeleteSession(context.Background(), cfg, "native-id", testRPCLogger())
	if err == nil || ClassifyError(err) != "startup_failed" {
		t.Fatalf("startup outcome = %v, reason = %q", err, ClassifyError(err))
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "native history" {
		t.Fatalf("failed startup changed history: %q, %v", data, err)
	}
}

func TestDeleteBoundSessionCancellationPrecedesMissingFile(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := Config{Binary: filepath.Join(root, "must-not-start"), CWD: filepath.Join(root, "missing-workspace")}
	err := DeleteBoundSession(ctx, cfg, "native-id", filepath.Join(root, "missing.jsonl"), testRPCLogger())
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrBoundSessionMissing) || ClassifyError(err) != "cancelled" {
		t.Fatalf("cancelled request was treated as missing-file cleanup: %v", err)
	}
	if _, err := os.Stat(cfg.CWD); !os.IsNotExist(err) {
		t.Fatalf("cancelled request recreated workspace: %v", err)
	}
}

func TestDeleteBoundSessionSearchOnlyAncestor(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		t.Skip("requires Linux directory search permissions without root bypass")
	}
	for _, existing := range []bool{true, false} {
		name := "missing-workspace"
		if existing {
			name = "existing-workspace"
		}
		t.Run(name, func(t *testing.T) {
			cfg, file, _ := deletionFixture(t, "success")
			gate := filepath.Join(t.TempDir(), "search-only")
			base := filepath.Join(gate, "owned")
			if err := os.MkdirAll(base, 0700); err != nil {
				t.Fatal(err)
			}
			cfg.CWD = filepath.Join(base, "workspace")
			if existing {
				if err := os.Mkdir(cfg.CWD, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cfg.CWD, "keep.txt"), []byte("user data"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = os.Chmod(gate, 0700) })
			if err := os.Chmod(gate, 0111); err != nil {
				t.Fatal(err)
			}
			t.Setenv("OMP_TEST_DELETE_CWD", cfg.CWD)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := DeleteBoundSession(ctx, cfg, "native-id", file, testRPCLogger()); err != nil {
				t.Fatalf("search-only ancestor prevented native deletion: %v", err)
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatalf("native history remained after deletion: %v", err)
			}
			if existing {
				if data, err := os.ReadFile(filepath.Join(cfg.CWD, "keep.txt")); err != nil || string(data) != "user data" {
					t.Fatalf("existing workspace changed: %q, %v", data, err)
				}
			} else if _, err := os.Stat(cfg.CWD); !os.IsNotExist(err) {
				t.Fatalf("temporary workspace was not cleaned: %v", err)
			}
			for _, path := range []string{gate, base} {
				if info, err := os.Stat(path); err != nil || !info.IsDir() {
					t.Fatalf("preexisting ancestor removed: %s, %v", path, err)
				}
			}
		})
	}
}
