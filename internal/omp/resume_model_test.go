package omp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type resumeModelLaunch struct {
	PID           int
	Args          []string
	CWD           string
	Preserved     string
	Denied        bool
	PreviousAlive bool
}

func fixtureResumeModelChild() {
	if len(os.Args) < 2 || os.Args[1] != "restore-stderr-holder" {
		return
	}
	// Escape the original group while retaining the selected descriptors.
	if err := os.WriteFile(os.Getenv("OMP_TEST_RESTORE_HOLDER"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(3)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func fixtureResumeModelStart() {
	mode := os.Getenv("OMP_TEST_RESTORE_MODE")
	if mode == "" {
		return
	}
	cwd, _ := os.Getwd()
	_, denied := os.LookupEnv("OMP_TEST_RESTORE_DENIED")
	launch := resumeModelLaunch{PID: os.Getpid(), Args: os.Args[1:], CWD: cwd, Preserved: os.Getenv("OMP_TEST_RESTORE_PRESERVE"), Denied: denied}
	if previous, err := os.ReadFile(os.Getenv("OMP_TEST_RESTORE_LAUNCHES")); err == nil {
		var first resumeModelLaunch
		line, _, _ := bytes.Cut(previous, []byte{'\n'})
		if json.Unmarshal(line, &first) == nil {
			launch.PreviousAlive = syscall.Kill(first.PID, 0) != syscall.ESRCH
		}
	}
	data, _ := json.Marshal(launch)
	file, err := os.OpenFile(os.Getenv("OMP_TEST_RESTORE_LAUNCHES"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(3)
	}
	_, _ = file.Write(append(data, '\n'))
	_ = file.Close()
	fallback := false
	for i, arg := range os.Args {
		if arg == "--model" && i+1 < len(os.Args) && os.Args[i+1] == "@default" {
			fallback = true
			break
		}
	}
	if fallback && mode != "fallback-fails" {
		return
	}
	const fatal = "SessionModelRestoreError: Could not restore model "
	switch mode {
	case "valid", "negotiation", "post-ready":
		return
	case "generic":
		fmt.Fprintln(os.Stderr, rpcSensitiveCanary)
	case "near-match":
		fmt.Fprintln(os.Stderr, "source snippet: "+fatal+rpcSensitiveCanary)
		fmt.Fprintln(os.Stderr, " "+fatal+rpcSensitiveCanary)
		fmt.Fprintln(os.Stderr, "\x1b[31m"+fatal+rpcSensitiveCanary)
	case "huge":
		_, _ = io.WriteString(os.Stderr, strings.Repeat(rpcSensitiveCanary, 40000)+"\n")
		_, _ = io.WriteString(os.Stderr, fatal+strings.Repeat(rpcSensitiveCanary, 40000)+"\n")
	case "inherited", "inherited-ready", "inherited-stdout", "inherited-frame", "inherited-chunks":
		child := exec.Command(os.Args[0], "restore-stderr-holder")
		child.Stderr = os.Stderr
		if mode != "inherited" && mode != "inherited-ready" {
			child.Stdout = os.Stdout
		}
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if child.Start() != nil {
			os.Exit(3)
		}
		_ = child.Process.Release()
		for {
			if _, err := os.Stat(os.Getenv("OMP_TEST_RESTORE_HOLDER")); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if mode == "inherited-ready" {
			return
		}
		fmt.Fprintln(os.Stderr, fatal+rpcSensitiveCanary)
		if mode == "inherited-frame" || mode == "inherited-chunks" {
			fixtureResumeModelTruncatedOutput(mode)
		}
	case "protocol":
		fmt.Fprintln(os.Stderr, fatal+rpcSensitiveCanary)
		fmt.Println(`{"type":"invalid_ready"}`)
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "truncated-frame", "truncated-chunks":
		fmt.Fprintln(os.Stderr, fatal+rpcSensitiveCanary)
		fixtureResumeModelTruncatedOutput(mode)
	case "cancelled", "timeout":
		fmt.Fprintln(os.Stderr, fatal+rpcSensitiveCanary)
		_ = os.WriteFile(os.Getenv("OMP_TEST_RESTORE_LAUNCHES")+".fatal", nil, 0600)
		_, _ = io.Copy(io.Discard, os.Stdin)
	default:
		fmt.Fprintln(os.Stderr, fatal+rpcSensitiveCanary)
	}
	os.Exit(1)
}

func fixtureResumeModelTruncatedOutput(mode string) {
	if strings.HasSuffix(mode, "frame") {
		fmt.Fprint(os.Stdout, `{"type":"ready"`)
		return
	}
	prefix := bytes.Repeat([]byte{'x'}, chunkPayload)
	copy(prefix, `{"type":"ready","padding":"`)
	fmt.Printf("%s\n", chunkFrame("restore", 0, 4, maxFrame, prefix))
}

func fixtureResumeModelCommand(command map[string]any) bool {
	mode := os.Getenv("OMP_TEST_RESTORE_MODE")
	if mode == "negotiation" && command["type"] == "negotiate_protocol" {
		fmt.Fprintln(os.Stderr, "SessionModelRestoreError: Could not restore model "+rpcSensitiveCanary)
		fmt.Printf("{\"type\":\"response\",\"id\":%q,\"command\":\"negotiate_protocol\",\"success\":false}\n", command["id"])
		return true
	}
	if mode == "post-ready" && command["type"] == "restore_failure" {
		fmt.Fprintln(os.Stderr, "SessionModelRestoreError: Could not restore model "+rpcSensitiveCanary)
		os.Exit(1)
	}
	return false
}

func readResumeModelLaunches(t *testing.T, path string) []resumeModelLaunch {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var launches []resumeModelLaunch
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var launch resumeModelLaunch
		if err := json.Unmarshal(line, &launch); err != nil {
			t.Fatal(err)
		}
		launches = append(launches, launch)
	}
	return launches
}

func TestResumeModelFallback(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		resume   bool
		enabled  bool
		launches int
		success  bool
		used     bool
	}{
		{"valid", true, true, 1, true, false},
		{"missing", true, true, 2, true, true},
		{"huge", true, true, 2, true, true},
		{"inherited", true, true, 2, true, true},
		{"inherited-ready", true, true, 1, true, false},
		{"inherited-stdout", true, true, 2, true, true},
		{"inherited-frame", true, true, 1, false, false},
		{"inherited-chunks", true, true, 1, false, false},
		{"generic", true, true, 1, false, false},
		{"near-match", true, true, 1, false, false},
		{"protocol", true, true, 1, false, false},
		{"truncated-frame", true, true, 1, false, false},
		{"truncated-chunks", true, true, 1, false, false},
		{"negotiation", true, true, 1, false, false},
		{"post-ready", true, true, 1, true, false},
		{"fallback-fails", true, true, 2, false, false},
		{"missing", true, false, 1, false, false},
		{"missing", false, true, 1, false, false},
		{"cancelled", true, true, 1, false, false},
		{"timeout", true, true, 1, false, false},
	} {
		t.Run(fmt.Sprintf("%s/resume=%t/enabled=%t", tc.mode, tc.resume, tc.enabled), func(t *testing.T) {
			root := t.TempDir()
			launchPath := filepath.Join(root, "launches")
			holderPath := filepath.Join(root, "holder")
			t.Setenv("OMP_TEST_RESTORE_MODE", tc.mode)
			t.Setenv("OMP_TEST_RESTORE_LAUNCHES", launchPath)
			t.Setenv("OMP_TEST_RESTORE_HOLDER", holderPath)
			t.Setenv("OMP_TEST_RESTORE_PRESERVE", "preserved-value")
			t.Setenv("OMP_TEST_RESTORE_DENIED", "must-not-inherit")
			t.Cleanup(func() {
				if data, err := os.ReadFile(holderPath); err == nil {
					pid, _ := strconv.Atoi(string(data))
					if pid > 0 {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			})
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			environment, err := NewEnvironment("denylist", nil, []string{"OMP_TEST_RESTORE_DENIED"})
			if err != nil {
				t.Fatal(err)
			}
			backing := []string{"--thinking", "low", "unchanged-capacity"}
			if tc.mode == "missing" {
				backing = []string{"--model", "fixture/saved", "--thinking", "low", "unchanged-capacity"}
			}
			original := append([]string(nil), backing...)
			cfg := Config{Binary: binary, CWD: root, Args: backing[:len(backing)-1], Environment: environment, ResumeModelFallback: tc.enabled}
			if tc.resume {
				cfg.Resume = filepath.Join(root, "saved-session.jsonl")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if tc.mode == "timeout" {
				var timeoutCancel context.CancelFunc
				ctx, timeoutCancel = context.WithTimeout(ctx, time.Second)
				defer timeoutCancel()
			}
			if tc.mode == "cancelled" {
				go func() {
					for {
						if _, err := os.Stat(launchPath + ".fatal"); err == nil {
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
			}
			logger, output := bufferedRPCLogger("json")
			type startedClient struct {
				client *Client
				err    error
			}
			started := make(chan startedClient, 1)
			go func() {
				client, err := Start(ctx, cfg, logger)
				started <- startedClient{client, err}
			}()
			var client *Client
			var startErr error
			select {
			case result := <-started:
				client, startErr = result.client, result.err
			case <-time.After(4 * time.Second):
				t.Fatal("startup or diagnostic cleanup stalled")
			}
			if (startErr == nil) != tc.success {
				t.Fatalf("startup success = %t, want %t (kind %s)", startErr == nil, tc.success, ClassifyError(startErr))
			}
			if client != nil {
				if client.UsedDefaultModelFallback() != tc.used {
					t.Fatal("incorrect fallback status")
				}
				if tc.mode == "post-ready" {
					if _, err := client.Call(ctx, "restore_failure", nil); err == nil {
						t.Fatal("post-ready failure unexpectedly succeeded")
					}
				}
				closed := make(chan struct{})
				go func() { _ = client.Close(); close(closed) }()
				select {
				case <-closed:
				case <-time.After(4 * time.Second):
					t.Fatal("normal-close diagnostic cleanup stalled")
				}
			}
			if !reflect.DeepEqual(backing, original) {
				t.Fatal("fallback mutated caller arguments")
			}
			launches := readResumeModelLaunches(t, launchPath)
			if len(launches) != tc.launches {
				t.Fatalf("launch count = %d, want %d", len(launches), tc.launches)
			}
			wantArgs := []string{"--mode", "rpc", "--cwd", root}
			if tc.resume {
				wantArgs = append(wantArgs, "--resume", cfg.Resume)
			}
			for i, launch := range launches {
				wantLaunchArgs := append([]string(nil), wantArgs...)
				if i == 0 {
					wantLaunchArgs = append(wantLaunchArgs, cfg.Args...)
				} else {
					wantLaunchArgs = append(wantLaunchArgs, "--model", "@default", "--thinking", "low")
				}
				if !reflect.DeepEqual(launch.Args, wantLaunchArgs) || launch.CWD != root || launch.Preserved != "preserved-value" || launch.Denied || launch.PreviousAlive {
					t.Fatal("launch changed session identity, arguments, working directory, or environment policy")
				}
				if err := syscall.Kill(launch.PID, 0); err != syscall.ESRCH {
					t.Fatal("original process was not fully reaped")
				}
			}
			if strings.Contains(output.String(), "SECRET_") || strings.Contains(output.String(), "SessionModelRestoreError") || (startErr != nil && strings.Contains(startErr.Error(), "SECRET_")) {
				t.Fatal("native diagnostics leaked through errors or logs")
			}
			wantFallbackEvents := 0
			if tc.used {
				wantFallbackEvents = 1
			}
			if got := strings.Count(output.String(), `"event":"rpc_model_restore_fallback"`); got != wantFallbackEvents {
				t.Fatalf("fallback success event count = %d", got)
			}
		})
	}
}

func TestDefaultModelArgsReplacesSelectors(t *testing.T) {
	for _, args := range [][]string{
		{"--model", "fixture/old", "--thinking", "low", "--models", "fixture/*"},
		{"--thinking", "low", "--model=fixture/old", "--models", "fixture/*"},
		{"--model=@default", "--thinking", "low", "--model", "fixture/old", "--models", "fixture/*", "--model", "@default"},
		{"--thinking", "low", "--model", "@default", "--models", "fixture/*"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got := defaultModelArgs(args)
			want := []string{"--model", "@default", "--thinking", "low", "--models", "fixture/*"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("fallback arguments = %q, want %q", got, want)
			}
		})
	}
}

func TestDefaultModelArgsPreservesOptionValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"prompt", []string{"--append-system-prompt", "--model", "--thinking", "low"}, []string{"--model", "@default", "--append-system-prompt", "--model", "--thinking", "low"}},
		{"prompt-equals-value", []string{"--system-prompt", "--model=literal", "--model", "fixture/old"}, []string{"--model", "@default", "--system-prompt", "--model=literal"}},
		{"config-path", []string{"--config", "--model", "--model=fixture/old"}, []string{"--model", "@default", "--config", "--model"}},
		{"inline-config", []string{"--config=--model", "--model", "fixture/old"}, []string{"--model", "@default", "--config=--model"}},
		{"short-extension", []string{"-e", "--model", "--model=fixture/old"}, []string{"--model", "@default", "-e", "--model"}},
		{"boolean", []string{"--no-tools", "--model", "fixture/old"}, []string{"--model", "@default", "--no-tools"}},
		{"extension-flag", []string{"--custom", "value", "--model", "fixture/old"}, []string{"--model", "@default", "--custom", "value"}},
		{"missing-system-prompt", []string{"--model=fixture/old", "--system-prompt"}, []string{"--model", "@default", "--system-prompt"}},
		{"missing-append-system-prompt", []string{"--append-system-prompt"}, []string{"--model", "@default", "--append-system-prompt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := defaultModelArgs(tc.args)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("fallback arguments = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResumeModelChunkBudgetFailureAfterProcessExitDoesNotFallback(t *testing.T) {
	assertResumeModelNoFallbackAfterProcessExit(t, []byte("{\"type\":\"rpc_chunk\",\"chunkId\":\"restore\",\"index\":0,\"count\":256,\"byteLength\":67108864,\"data\":\"eA==\"}\n"))
}

func TestResumeModelTruncationAfterProcessExitDoesNotFallback(t *testing.T) {
	prefix := bytes.Repeat([]byte{'x'}, chunkPayload)
	copy(prefix, `{"type":"ready","padding":"`)
	for _, tc := range []struct {
		name string
		wire []byte
	}{
		{"physical", []byte(`{"type":"ready"`)},
		{"chunked", append(chunkFrame("restore", 0, 4, maxFrame, prefix), '\n')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertResumeModelNoFallbackAfterProcessExit(t, tc.wire)
		})
	}
}

func assertResumeModelNoFallbackAfterProcessExit(t *testing.T, wire []byte) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	logger, _ := bufferedRPCLogger("json")
	c := &Client{
		log: logger, stdout: reader, ready: make(chan struct{}),
		readDone: make(chan struct{}), done: make(chan struct{}), stop: make(chan struct{}),
		queuedEvents: make(chan json.RawMessage), resumeDiagnostics: &resumeModelDiagnostics{matched: true},
	}
	// Let process exit win before the reader discovers the protocol failure.
	c.fail(&classifiedError{kind: "process_exit", err: fmt.Errorf("fixture process exit")})
	go c.readLoop()
	_, err = writer.Write(wire)
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	select {
	case <-c.readDone:
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not settle after protocol failure")
	}
	close(c.done)
	if kind := ClassifyError(c.startupFailure(context.Background())); kind != "process_exit" {
		t.Fatalf("protocol failure permitted model fallback: %s", kind)
	}
}
