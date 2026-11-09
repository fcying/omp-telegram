package omp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fixtureRPCUIStart() {
	if os.Getenv("OMP_TEST_RPC_UI_TRACE") == "" {
		return
	}
	fixtureRPCUIRecord(map[string]any{"type": "launch", "pid": os.Getpid(), "mode": os.Args[2]})
	if os.Getenv("OMP_TEST_RPC_UI_MODE") == "ready-timeout" {
		decoder := json.NewDecoder(os.Stdin)
		for {
			var command map[string]any
			if decoder.Decode(&command) != nil {
				os.Exit(0)
			}
			fixtureRPCUIRecord(command)
		}
	}
}

func fixtureRPCUIRecord(frame map[string]any) {
	path := os.Getenv("OMP_TEST_RPC_UI_TRACE")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(3)
	}
	if json.NewEncoder(file).Encode(frame) != nil || file.Close() != nil {
		os.Exit(3)
	}
}

var fixtureRPCUIPending any

func fixtureRPCUICommand(command map[string]any) bool {
	fixtureRPCUIRecord(command)
	mode := os.Getenv("OMP_TEST_RPC_UI_MODE")
	if fixtureStartupUICommand(command, mode) {
		return true
	}
	emit := func(frame any) { _ = json.NewEncoder(os.Stdout).Encode(frame) }
	if command["type"] == "set_ask_dialog" {
		if command["enabled"] != true {
			os.Exit(3)
		}
		reply := map[string]any{"type": "response", "id": command["id"], "command": "set_ask_dialog", "success": true, "data": map[string]any{"enabled": true}}
		switch mode {
		case "unsupported", "unsupported-protocol", "rejected", "wrong-unsupported", "wrong-command", "object-error", "uncorrelated":
			reply["success"] = false
			reply["error"] = "Unknown command: set_ask_dialog"
			if mode == "rejected" {
				reply["error"] = rpcSensitiveCanary
			}
			if mode == "wrong-unsupported" {
				reply["error"] = "Unknown command: another_command"
			}
			if mode == "wrong-command" {
				reply["command"] = "another_command"
			}
			if mode == "object-error" {
				reply["error"] = map[string]any{"message": "Unknown command: set_ask_dialog"}
			}
			if mode == "uncorrelated" {
				delete(reply, "id")
			}
		case "false":
			reply["data"] = map[string]any{"enabled": false}
		case "missing":
			reply["data"] = map[string]any{"accepted": true}
		case "null":
			reply["data"] = nil
		case "null-enabled":
			reply["data"] = map[string]any{"enabled": nil}
		case "string":
			reply["data"] = map[string]any{"enabled": "true"}
		case "array":
			reply["data"] = []any{true}
		case "timeout":
			return true
		case "transport":
			os.Exit(0)
		case "events":
			for i := range 16 {
				emit(map[string]any{"type": "extension_ui_request", "method": "notify", "message": fmt.Sprint(i)})
			}
		}
		emit(reply)
		return true
	}
	if command["type"] == "negotiate_protocol" && mode == "startup-dialog" {
		emit(map[string]any{"type": "extension_ui_request", "id": "startup-dialog", "method": "select", "title": "Startup permission", "options": []string{"Allow", "Deny"}})
		return true
	}
	if command["type"] == "negotiate_protocol" && mode == "unsupported-protocol" {
		emit(map[string]any{"type": "response", "id": command["id"], "command": "negotiate_protocol", "success": false, "error": "Unknown command: negotiate_protocol"})
		return true
	}
	if command["type"] == "native_ui_probe" {
		fixtureRPCUIPending = command["id"]
		method := "ask"
		if mode == "editor" || mode == "unsupported" {
			method = "editor"
		}
		emit(map[string]any{"type": "extension_ui_request", "id": "native-dialog", "method": method, "title": "Native question", "questions": []any{map[string]any{"id": "q1", "question": "Choose", "options": []any{map[string]any{"label": "First"}}, "multi": false}}})
		return true
	}
	if command["type"] == "extension_ui_response" && fixtureRPCUIPending != nil {
		emit(map[string]any{"type": "response", "id": fixtureRPCUIPending, "command": "native_ui_probe", "success": true, "data": command})
		fixtureRPCUIPending = nil
		return true
	}
	return false
}

func fixtureStartupUICommand(command map[string]any, mode string) bool {
	parts := strings.Split(mode, ":")
	if len(parts) != 4 || parts[0] != "diagnostic" || command["type"] != parts[1] {
		return false
	}
	emit := func(frame any) { _ = json.NewEncoder(os.Stdout).Encode(frame) }
	id := "SECRET_UI_ID"
	emitUI := func(method string) {
		emit(map[string]any{"type": "extension_ui_request", "id": id, "method": method,
			"title": rpcSensitiveCanary, "message": rpcSensitiveCanary, "questions": []any{rpcSensitiveCanary},
			"options": []string{"SECRET_UI_OPTION"}, "default": "SECRET_UI_DEFAULT", "value": "SECRET_UI_VALUE"})
	}
	method, outcome := parts[2], parts[3]
	switch method {
	case "cancelled":
		emitUI("confirm")
		emit(map[string]any{"type": "extension_ui_request", "method": "cancel", "targetId": id})
	case "unrelated-cancel":
		emitUI("confirm")
		emit(map[string]any{"type": "extension_ui_request", "method": "cancel", "targetId": "SECRET_OTHER_ID"})
	case "mixed":
		emitUI("confirm")
		id = "SECRET_SECOND_ID"
		emitUI("select")
	case "missing-id":
		id = ""
		emitUI("confirm")
	default:
		emitUI(method)
	}
	fixtureRPCUIRecord(map[string]any{"type": "startup_ui_emitted"})
	if err := os.WriteFile(os.Getenv("OMP_TEST_RPC_UI_TRACE")+".ui", nil, 0600); err != nil {
		os.Exit(3)
	}
	switch outcome {
	case "rejected":
		emit(map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": false, "error": rpcSensitiveCanary})
	case "transport":
		os.Exit(0)
	case "protocol":
		emit(map[string]any{"type": "response", "id": command["id"], "command": "SECRET_WRONG_COMMAND", "success": true})
	case "success":
		return false
	case "cleanup-cancel":
		decoder := json.NewDecoder(os.Stdin)
		for {
			var extra map[string]any
			if decoder.Decode(&extra) != nil {
				break
			}
			fixtureRPCUIRecord(extra)
		}
		emit(map[string]any{"type": "extension_ui_request", "method": "cancel", "targetId": id})
		os.Exit(0)
	}
	return true
}

func rpcUIFixture(t *testing.T, mode string) (Config, string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	trace := filepath.Join(root, "rpc-ui.jsonl")
	t.Setenv("OMP_TEST_RPC_UI_TRACE", trace)
	t.Setenv("OMP_TEST_RPC_UI_MODE", mode)
	return Config{Binary: binary, CWD: root}, trace
}

func rpcUITrace(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var frames []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	return frames
}

func TestRPCUIStartupEventsDoNotBlockNegotiation(t *testing.T) {
	cfg, _ := rpcUIFixture(t, "events")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Start(ctx, cfg, testRPCLogger())
	if err != nil {
		t.Fatalf("RPC UI startup failed: %v", err)
	}
	defer client.Close()
	for range 16 {
		select {
		case frame := <-client.Events():
			if !strings.Contains(string(frame), `"method":"notify"`) {
				t.Fatal("startup event was replaced or consumed")
			}
		case <-ctx.Done():
			t.Fatal("startup events blocked the reader or were lost")
		}
	}
}

func TestRPCUIStartupFailsClosed(t *testing.T) {
	for _, mode := range []string{"unsupported-protocol", "rejected", "wrong-unsupported", "wrong-command", "object-error", "uncorrelated", "false", "missing", "null", "null-enabled", "string", "array", "transport", "timeout", "startup-dialog"} {
		t.Run(mode, func(t *testing.T) {
			cfg, trace := rpcUIFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			client, err := Start(ctx, cfg, testRPCLogger())
			if client != nil || err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("ambiguous Ask negotiation did not fail closed or leaked rejection data")
			}
			frames := rpcUITrace(t, trace)
			pid := int(frames[0]["pid"].(float64))
			if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
				t.Fatal("startup failure returned before reaping its process")
			}
			for _, frame := range frames[1:] {
				if frame["type"] == "set_ask_dialog" {
					continue
				}
				if (mode == "startup-dialog" || mode == "unsupported-protocol") && frame["type"] == "negotiate_protocol" {
					continue
				}
				t.Fatal("failed negotiation downgraded, sent a prompt, or automatically answered startup UI")
			}
		})
	}
}

func TestRPCUINativeDialogInputDoesNotBlockReader(t *testing.T) {
	for _, mode := range []string{"enabled", "editor", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			cfg, trace := rpcUIFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Start(ctx, cfg, testRPCLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			launches := 0
			for _, frame := range rpcUITrace(t, trace) {
				if frame["type"] != "launch" {
					continue
				}
				launches++
				if frame["mode"] != "rpc-ui" {
					t.Fatal("Ask compatibility silently downgraded RPC mode")
				}
			}
			if launches != 1 {
				t.Fatal("Ask compatibility restarted the native runtime")
			}
			expectedMethod := "ask"
			if mode != "enabled" {
				expectedMethod = "editor"
			}
			got := make(chan result, 1)
			go func() {
				data, err := client.Call(ctx, "native_ui_probe", nil)
				got <- result{data: data, err: err}
			}()
			select {
			case raw := <-client.Events():
				var event struct{ Type, ID, Method string }
				if json.Unmarshal(raw, &event) != nil || event.Type != "extension_ui_request" || event.ID != "native-dialog" || event.Method != expectedMethod {
					t.Fatal("native dialog did not reach the consumer")
				}
			case <-ctx.Done():
				t.Fatal("native dialog blocked behind its pending command")
			}
			if _, err := client.Call(ctx, "get_state", nil); err != nil {
				t.Fatal("native dialog blocked independent RPC work")
			}
			response := map[string]any{"type": "extension_ui_response", "id": "native-dialog"}
			if mode == "enabled" {
				response["answers"] = []any{map[string]any{"id": "q1", "selectedOptions": []string{"First"}, "customInput": "note"}}
			} else {
				response["value"] = "answer"
			}
			if err := client.Send(ctx, response); err != nil {
				t.Fatal(err)
			}
			select {
			case reply := <-got:
				var answer struct{ Type, ID string }
				if reply.err != nil || json.Unmarshal(reply.data, &answer) != nil || answer.Type != "extension_ui_response" || answer.ID != "native-dialog" {
					t.Fatal("native dialog answer did not resolve its pending command")
				}
			case <-ctx.Done():
				t.Fatal("reader did not deliver command acknowledgement after native input")
			}
		})
	}
}

func TestValidateArgsPreservesRPCUIOwnership(t *testing.T) {
	for _, args := range [][]string{{"--mode", "rpc"}, {"--mode", "rpc-ui"}, {"--mode=rpc-ui"}, {"--cwd", "/other"}, {"--resume", "other"}, {"--session=other"}, {"--continue"}, {"--no-session"}, {"--print"}, {"--export", "other"}, {"--"}, {"-r"}, {"-c"}, {"-p"}} {
		if ValidateArgs(args) == nil {
			t.Fatalf("owned startup arguments accepted: %q", args)
		}
	}
	if err := ValidateArgs([]string{"--model", "fixture/model", "--session-dir", "/isolated/sessions", "--no-ui"}); err != nil {
		t.Fatal("unrelated runtime options were rejected")
	}
}

func TestRPCUIStartupDiagnostics(t *testing.T) {
	for _, stage := range []string{"set_ask_dialog", "negotiate_protocol"} {
		for _, test := range []struct {
			method, outcome, kind string
			count                 int
			methods               string
		}{
			{"confirm", "timeout", "startup_timeout_pending_confirm", 1, "confirm"},
			{"select", "timeout", "startup_timeout_pending_ui", 1, "select"},
			{"input", "timeout", "startup_timeout_pending_ui", 1, "input"},
			{"editor", "timeout", "startup_timeout_pending_ui", 1, "editor"},
			{"ask", "timeout", "startup_timeout_pending_ui", 1, "ask"},
			{"mixed", "timeout", "startup_timeout_pending_confirm", 2, "confirm,select"},
			{"unrelated-cancel", "timeout", "startup_timeout_pending_confirm", 1, "confirm"},
			{"confirm", "cleanup-cancel", "startup_timeout_pending_confirm", 1, "confirm"},
			{"cancelled", "timeout", "timeout", 0, ""},
			{"setWidget", "timeout", "timeout", 0, ""},
			{"notify", "timeout", "timeout", 0, ""},
			{"SECRET_UI_METHOD", "timeout", "timeout", 0, ""},
			{"missing-id", "timeout", "timeout", 0, ""},
			{"confirm", "rejected", "rejected", 1, "confirm"},
			{"confirm", "transport", "process_exit", 1, "confirm"},
			{"confirm", "protocol", "protocol", 1, "confirm"},
			{"confirm", "cancelled", "cancelled", 1, "confirm"},
		} {
			t.Run(stage+"/"+test.method+"/"+test.outcome, func(t *testing.T) {
				cfg, trace := rpcUIFixture(t, "diagnostic:"+stage+":"+test.method+":"+test.outcome)
				logger, logs := bufferedRPCLogger("json")
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				var client *Client
				var err error
				if test.outcome == "cancelled" {
					finished := make(chan struct{})
					go func() {
						client, err = Start(ctx, cfg, logger)
						close(finished)
					}()
					for {
						if _, markerErr := os.Stat(trace + ".ui"); markerErr == nil {
							// Allow observation of the already-written event before cancellation.
							time.Sleep(20 * time.Millisecond)
							cancel()
							break
						}
						select {
						case <-ctx.Done():
							t.Fatal("fixture did not emit startup UI before cancellation")
						case <-time.After(time.Millisecond):
						}
					}
					<-finished
				} else {
					client, err = Start(ctx, cfg, logger)
				}
				if client != nil || err == nil || ClassifyError(err) != test.kind {
					t.Fatalf("startup classification = %q, want %q; err=%v", ClassifyError(err), test.kind, err)
				}
				if strings.Contains(err.Error(), "SECRET") || strings.Contains(logs.String(), "SECRET") {
					t.Fatal("startup error or logger leaked UI content, options, IDs, or defaults")
				}
				assertStartupUITrace(t, trace, stage)
				var failures []map[string]any
				for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
					var record map[string]any
					if json.Unmarshal([]byte(line), &record) != nil {
						t.Fatal("startup logger emitted invalid JSON")
					}
					if record["event"] == "rpc_startup_failed" {
						failures = append(failures, record)
					}
				}
				if test.outcome == "cancelled" {
					if len(failures) != 0 {
						t.Fatal("normal daemon cancellation logged a startup failure")
					}
					return
				}
				if len(failures) != 1 {
					t.Fatalf("startup emitted %d failure records, want one", len(failures))
				}
				failure := failures[0]
				if failure["stage"] != stage || failure["reason"] != test.kind || failure["pending_ui_count"] != float64(test.count) || failure["client_id"] == nil {
					t.Fatalf("startup diagnostics do not describe the failure: %v", failure)
				}
				var methods []string
				for _, method := range failure["pending_ui_methods"].([]any) {
					methods = append(methods, method.(string))
				}
				if strings.Join(methods, ",") != test.methods {
					t.Fatalf("pending methods = %v, want %s", methods, test.methods)
				}
			})
		}
	}
}

func assertStartupUITrace(t *testing.T, trace, stage string) {
	t.Helper()
	frames := rpcUITrace(t, trace)
	pid := int(frames[0]["pid"].(float64))
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatal("startup failure returned before reaping its process")
	}
	counts := make(map[string]int)
	for _, frame := range frames {
		kind := frame["type"].(string)
		counts[kind]++
		switch kind {
		case "launch", "startup_ui_emitted", "set_ask_dialog":
		case "negotiate_protocol":
			if stage != "negotiate_protocol" {
				t.Fatal("startup continued after a failed Ask handshake")
			}
		default:
			t.Fatalf("startup auto-answered UI or sent an unrelated command: %s", kind)
		}
	}
	if counts["launch"] != 1 || frames[0]["mode"] != "rpc-ui" || counts["set_ask_dialog"] != 1 || counts["negotiate_protocol"] > 1 {
		t.Fatal("startup retried, replayed negotiation, or downgraded transport")
	}
}

func TestRPCUIStartupSuccessReleasesDiagnostics(t *testing.T) {
	cfg, _ := rpcUIFixture(t, "diagnostic:negotiate_protocol:confirm:success")
	logger, logs := bufferedRPCLogger("json")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Start(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.startupUI.Load() != nil {
		t.Fatal("successful startup retained its UI tracker")
	}
	select {
	case frame := <-client.Events():
		if !strings.Contains(string(frame), `"method":"confirm"`) || !strings.Contains(string(frame), "SECRET_UI_ID") {
			t.Fatal("startup observation consumed or replaced the original UI event")
		}
	case <-ctx.Done():
		t.Fatal("startup observation dropped the UI event")
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer probeCancel()
	finished := make(chan error, 1)
	go func() {
		_, err := client.Call(probeCtx, "native_ui_probe", nil)
		finished <- err
	}()
	select {
	case frame := <-client.Events():
		if !strings.Contains(string(frame), `"method":"ask"`) {
			t.Fatal("post-startup UI did not reach the consumer")
		}
	case <-ctx.Done():
		t.Fatal("post-startup UI blocked the reader")
	}
	if err := <-finished; ClassifyError(err) != "timeout" {
		t.Fatalf("post-startup UI timeout inherited startup diagnosis: %v", err)
	}
	if client.startupUI.Load() != nil {
		t.Fatal("post-startup UI recreated the startup tracker")
	}
	_ = client.Close()
	if strings.Contains(logs.String(), "rpc_startup_failed") || strings.Contains(logs.String(), "SECRET") {
		t.Fatal("successful startup logged a failure or leaked UI secrets")
	}
}

func TestRPCUIStartupTrackerBoundsAndFreeze(t *testing.T) {
	tracker := &startupUITracker{}
	observe := func(id, method, target string) {
		t.Helper()
		frame, err := json.Marshal(map[string]any{"method": method, "targetId": target, "title": rpcSensitiveCanary})
		if err != nil {
			t.Fatal(err)
		}
		tracker.observe(id, frame)
	}
	for i := range maxStartupPendingUI {
		observe(fmt.Sprint(i), "select", "")
	}
	observe("overflow", "confirm", "")
	observe("", "cancel", "0")
	observe("replacement", "input", "")
	snapshot := tracker.finish()
	if snapshot.count != maxStartupPendingUI || snapshot.confirm || strings.Join(snapshot.methods, ",") != "select,input" {
		t.Fatal("tracker exceeded its bound or cancellation failed to release capacity")
	}
	observe("", "cancel", "replacement")
	observe("late", "confirm", "")
	frozen := tracker.finish()
	if frozen.count != snapshot.count || frozen.confirm || strings.Join(frozen.methods, ",") != "select,input" || tracker.pending != nil {
		t.Fatal("shutdown UI changed frozen evidence or retained request IDs")
	}
}

func TestRPCUIStartupReadyFailureDiagnostics(t *testing.T) {
	for _, kind := range []string{"timeout", "process_exit"} {
		t.Run(kind, func(t *testing.T) {
			cfg, trace := rpcUIFixture(t, "ready-timeout")
			if kind == "process_exit" {
				t.Setenv("OMP_TEST_RPC_UI_MODE", "enabled")
				t.Setenv("OMP_TEST_RPC_EOF_BEFORE_READY", "1")
			}
			logger, logs := bufferedRPCLogger("json")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			client, err := Start(ctx, cfg, logger)
			if client != nil || ClassifyError(err) != kind {
				t.Fatalf("pre-ready failure = %v, want %s", err, kind)
			}
			frames := rpcUITrace(t, trace)
			if len(frames) != 1 || syscall.Kill(int(frames[0]["pid"].(float64)), 0) != syscall.ESRCH {
				t.Fatal("pre-ready failure sent a command, retried, or failed to reap")
			}
			failures := 0
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var record map[string]any
				if json.Unmarshal([]byte(line), &record) != nil {
					t.Fatal("startup logger emitted invalid JSON")
				}
				if record["event"] == "rpc_startup_failed" {
					failures++
					if record["stage"] != "ready" || record["reason"] != kind || record["pending_ui_count"] != float64(0) {
						t.Fatalf("pre-ready diagnostics = %v", record)
					}
				}
			}
			if failures != 1 {
				t.Fatal("pre-ready startup failure was not logged exactly once")
			}
		})
	}
}

func TestRPCUIStartupReadyDiagnosisSurvivesShutdownRace(t *testing.T) {
	for _, kind := range []string{"timeout", "startup_timeout_pending_confirm", "startup_timeout_pending_ui", "protocol"} {
		for _, cleanup := range []string{"cancelled", "timeout"} {
			t.Run(kind+"/"+cleanup, func(t *testing.T) {
				logger, logs := bufferedRPCLogger("json")
				c := &Client{log: logger, stop: make(chan struct{}), done: make(chan struct{}), ready: make(chan struct{})}
				close(c.done)
				tracker := &startupUITracker{}
				if kind == "startup_timeout_pending_confirm" {
					tracker.observe("pending", []byte(`{"method":"confirm"}`))
					close(c.ready)
				} else if kind == "startup_timeout_pending_ui" {
					tracker.observe("pending", []byte(`{"method":"input"}`))
					close(c.ready)
				}
				tracker.mu.Lock()
				c.startupUI.Store(tracker)
				ctx, cancel := context.WithDeadline(context.Background(), time.Now())
				defer cancel()
				finished := make(chan error, 1)
				go func() { finished <- c.finishStartupFailure(ctx, ctx, "ready", context.DeadlineExceeded) }()
				// Hold snapshot resolution until cleanup publishes its older terminal error.
				deadline := time.NewTimer(5 * time.Second)
				defer deadline.Stop()
				for c.startupUI.Load() != nil {
					select {
					case <-deadline.C:
						tracker.mu.Unlock()
						<-finished
						t.Fatal("startup did not reach its snapshot boundary")
					case <-time.After(time.Millisecond):
					}
				}
				if cleanup == "timeout" {
					c.fail(context.DeadlineExceeded)
				} else {
					c.contextCanceled.Store(true)
					c.fail(&classifiedError{kind: "cancelled", err: errors.New("omp: process context canceled")})
				}
				if kind == "protocol" {
					c.protocolFailure(errProtocol)
				}
				tracker.mu.Unlock()
				err := <-finished
				if ClassifyError(err) != kind || !strings.Contains(logs.String(), `"reason":"`+kind+`"`) || strings.Count(logs.String(), `"event":"rpc_startup_failed"`) != 1 {
					t.Fatalf("shutdown replaced the resolved ready diagnosis: want=%s err=%v log=%s", kind, err, logs.String())
				}
			})
		}
	}
}

func TestRPCUIStartupFailurePreservesConcurrentTerminal(t *testing.T) {
	for _, stage := range []string{"ready", "set_ask_dialog", "negotiate_protocol"} {
		for _, kind := range []string{"protocol", "process_exit"} {
			t.Run(stage+"/"+kind, func(t *testing.T) {
				logger, logs := bufferedRPCLogger("json")
				c := &Client{log: logger, stop: make(chan struct{}), done: make(chan struct{})}
				close(c.done)
				tracker := &startupUITracker{}
				tracker.observe("pending", []byte(`{"method":"confirm"}`))
				tracker.mu.Lock()
				c.startupUI.Store(tracker)
				ctx, cancel := context.WithDeadline(context.Background(), time.Now())
				defer cancel()
				finished := make(chan error, 1)
				go func() { finished <- c.finishStartupFailure(ctx, ctx, stage, context.DeadlineExceeded) }()
				// Publish the terminal error while the UI snapshot is still blocked.
				deadline := time.NewTimer(5 * time.Second)
				defer deadline.Stop()
				for c.startupUI.Load() != nil {
					select {
					case <-deadline.C:
						tracker.mu.Unlock()
						<-finished
						t.Fatal("startup did not reach its snapshot boundary")
					case <-time.After(time.Millisecond):
					}
				}
				if kind == "protocol" {
					c.protocolFailure(errProtocol)
				} else {
					c.transportEnded(fmt.Errorf("native process exited"))
				}
				tracker.mu.Unlock()
				err := <-finished
				if ClassifyError(err) != kind {
					t.Fatalf("startup classification = %s, terminal = %s; want %s", ClassifyError(err), ClassifyError(c.failure()), kind)
				}
				failures := 0
				for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
					var record map[string]any
					decodeErr := json.Unmarshal([]byte(line), &record)
					if decodeErr != nil {
						t.Fatal(decodeErr)
					}
					if record["event"] == "rpc_startup_failed" {
						failures++
						if record["reason"] != kind {
							t.Fatalf("startup log lost the terminal error: %v", record)
						}
					}
				}
				if failures != 1 {
					t.Fatalf("startup failure log count = %d, want 1", failures)
				}
			})
		}
	}
}

func TestRPCUIStartupCancellationBeforeChildPropagation(t *testing.T) {
	for _, stage := range []string{"ready", "set_ask_dialog", "negotiate_protocol"} {
		t.Run(stage, func(t *testing.T) {
			logger, logs := bufferedRPCLogger("json")
			c := &Client{log: logger, stop: make(chan struct{}), done: make(chan struct{})}
			close(c.done)
			// The supervisor can observe parent cancellation before its startup child.
			c.contextCanceled.Store(true)
			c.fail(&classifiedError{kind: "cancelled", err: errors.New("omp: process context canceled")})
			err := c.finishStartupFailure(context.Background(), context.Background(), stage, c.failure())
			if !errors.Is(err, context.Canceled) || ClassifyError(err) != "cancelled" {
				t.Fatalf("normal startup cancellation lost its sentinel: %v", err)
			}
			if strings.Contains(logs.String(), `"event":"rpc_startup_failed"`) {
				t.Fatalf("normal startup cancellation emitted a failure: %s", logs.String())
			}
		})
	}
}

func TestRPCUIHandshakeFailureSurvivesDeadlineCleanup(t *testing.T) {
	for _, kind := range []string{"rejected", "protocol"} {
		for _, phase := range []string{"deadline-before-freeze", "deadline-during-freeze", "cancel-before-child"} {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				logger, logs := bufferedRPCLogger("json")
				c := &Client{log: logger, stop: make(chan struct{}), done: make(chan struct{})}
				close(c.done)
				tracker := &startupUITracker{}
				tracker.observe("pending", []byte(`{"method":"confirm"}`))
				c.startupUI.Store(tracker)
				var parent context.Context
				var cancel context.CancelFunc
				if phase == "cancel-before-child" {
					parent, cancel = context.WithCancel(context.Background())
					cancel()
				} else {
					parent, cancel = context.WithDeadline(context.Background(), time.Now())
				}
				defer cancel()
				if phase == "deadline-before-freeze" {
					c.fail(context.DeadlineExceeded)
				}
				tracker.mu.Lock()
				var cause error = &classifiedError{kind: "rejected", err: errors.New("explicit handshake rejection")}
				if kind == "protocol" {
					cause = errProtocol
				}
				finished := make(chan error, 1)
				// Model the child context before its parent's cancellation propagates.
				go func() { finished <- c.finishStartupFailure(parent, context.Background(), "set_ask_dialog", cause) }()
				deadline := time.NewTimer(5 * time.Second)
				defer deadline.Stop()
				for c.startupUI.Load() != nil {
					select {
					case <-deadline.C:
						tracker.mu.Unlock()
						<-finished
						t.Fatal("startup did not reach its snapshot boundary")
					case <-time.After(time.Millisecond):
					}
				}
				if phase != "deadline-before-freeze" {
					c.contextCanceled.Store(true)
					c.fail(&classifiedError{kind: "cancelled", err: errors.New("omp: process context canceled")})
				}
				tracker.mu.Unlock()
				err := <-finished
				failures := strings.Count(logs.String(), `"event":"rpc_startup_failed"`)
				if phase == "cancel-before-child" {
					if !errors.Is(err, context.Canceled) || failures != 0 {
						t.Fatalf("parent cancellation was not normal shutdown: err=%v log=%s", err, logs.String())
					}
				} else if !errors.Is(err, cause) || ClassifyError(err) != kind || failures != 1 || !strings.Contains(logs.String(), `"reason":"`+kind+`"`) {
					t.Fatalf("deadline cleanup replaced the handshake failure: want=%s err=%v log=%s", kind, err, logs.String())
				}
			})
		}
	}
}
