package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func fixtureRPCControlFiles(t *testing.T, held string) (string, string) {
	t.Helper()
	root := t.TempDir()
	order := filepath.Join(root, "order")
	trace := filepath.Join(root, "trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_HOLD_RPC", held)
	t.Setenv("OMP_TELEGRAM_FIXTURE_REJECT_RPC", "")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_ORDER", order)
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	return order, trace
}

func startTimedFixtureOperation(t *testing.T, w *worker, kind, method string, fields map[string]any, meta any) (*omp.Client, fixtureRPCTraceEntry, operationResult) {
	t.Helper()
	client := w.client
	if client == nil {
		t.Fatal("fixture runtime is not connected")
	}
	w.beginControlOperation(controlModel)
	w.startOperation(kind, client, func(ctx context.Context) (json.RawMessage, error) {
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return client.Call(callCtx, method, fields)
	}, 0, "", meta)
	order := os.Getenv("OMP_TELEGRAM_FIXTURE_RPC_ORDER")
	trace := os.Getenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE")
	waitFixtureRPCOrder(t, order, method)
	entry := waitFixtureRPCTrace(t, trace, method, "")
	result := waitOperation(t, w)
	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("held %s error = %v, want deadline exceeded", method, result.err)
	}
	return client, entry, result
}

func acceptWorkerInput(t *testing.T, w *worker, id int64, text string) {
	t.Helper()
	u := update(id, w.key.thread, text)
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(id, raw); err != nil {
		t.Fatal(err)
	}
	w.handle(incoming{id: id, msg: u.Message})
}

func assertRetiredOperationClient(t *testing.T, w *worker, client *omp.Client, binding store.Binding) {
	t.Helper()
	if w.client != nil || w.runtime != runtimeReleased || w.controlInProgress() || !w.binding.Running || !sameBindingIdentity(w.binding, binding) {
		t.Fatalf("uncertain operation did not retire only its runtime: client=%v runtime=%v controlInProgress=%t binding=%+v", w.client, w.runtime, w.controlInProgress(), w.binding)
	}
	waitFor(t, func() bool {
		select {
		case <-client.Done():
			return true
		default:
			return false
		}
	})
}

func TestRootSubmissionTimeoutPreservesSessionWithoutReplay(t *testing.T) {
	for _, kind := range []string{"prompt", "native_prompt"} {
		t.Run(kind, func(t *testing.T) {
			_, trace := fixtureRPCControlFiles(t, "prompt")
			w, _, command := setupWorkspaceWorker(t)
			w.b.cfg.QueueCapacity = 4
			command("/new " + t.TempDir())
			before, client := w.binding, w.client
			requireStoreOK(t, w.b.db.Accept(100, []byte(`{"update_id":100}`)))
			q := queued{id: 100, user: 7, text: "/review"}
			if !w.submit(q) {
				t.Fatal("review submission was not persisted")
			}
			w.beginTask(q)
			requestID := client.ReserveRequestID()
			w.rootRequestID = requestID
			w.startOperation(kind, client, func(ctx context.Context) (json.RawMessage, error) {
				ctx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				return client.CallWithID(ctx, requestID, "prompt", map[string]any{"message": "/review"})
			}, 100, "")
			waitFixtureRPCTrace(t, trace, "prompt", "/review")
			acceptWorkerInput(t, w, 101, "/followup cancelled after unconfirmed review")
			result := waitOperation(t, w)
			if !errors.Is(result.err, context.DeadlineExceeded) {
				t.Fatalf("held review error=%v, want deadline exceeded", result.err)
			}
			w.operationReturned(result)
			assertRetiredOperationClient(t, w, client, before)
			saved, err := w.b.db.Binding(99, w.key.chat, w.key.thread)
			if err != nil || !saved.Running || !sameBindingIdentity(saved, before) {
				t.Fatal("submission timeout closed or replaced the saved session")
			}
			if queueState(t, w, 100) != "uncertain" || queueState(t, w, 101) != "cancelled" || len(w.queue) != 0 || w.taskActive() {
				t.Fatal("submission timeout lost the uncertain root or retained canceled work")
			}
			if info, err := os.Stat(before.Session); err != nil || !info.Mode().IsRegular() {
				t.Fatal("submission timeout removed native history")
			}
			t.Setenv("OMP_TELEGRAM_FIXTURE_HOLD_RPC", "")
			acceptWorkerInput(t, w, 102, "/review fresh after timeout")
			w.dispatch()
			nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
			if w.client == nil || w.client == client || !sameBindingIdentity(w.binding, before) {
				t.Fatal("explicit review did not lazily resume the preserved session")
			}
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "prompt" && entry.payload == "cancelled after unconfirmed review" {
					t.Fatal("submission timeout replayed canceled queued work")
				}
			}
			if researchTraceCount(trace, "prompt", "/review") != 1 || researchTraceCount(trace, "prompt", "/review fresh after timeout") != 1 || queueState(t, w, 100) != "uncertain" {
				t.Fatal("explicit recovery replayed or rewrote the uncertain review")
			}
		})
	}
}

func TestRuntimeFailuresPreserveSessionWithoutReplay(t *testing.T) {
	for _, failure := range []string{"idle-exit", "active-exit", "stream-overflow", "final-overflow", "failed-response", "ui-confirm", "ui-select"} {
		t.Run(failure, func(t *testing.T) {
			_, trace := fixtureRPCControlFiles(t, "")
			w, _, command := setupWorkspaceWorker(t)
			w.b.cfg.QueueCapacity = 4
			command("/new " + t.TempDir())
			before, client := w.binding, w.client
			active := failure != "idle-exit"
			if active {
				requireStoreOK(t, w.b.db.Accept(100, []byte(`{"update_id":100}`)))
				q := queued{id: 100, user: 7, text: "wait"}
				if !w.submit(q) {
					t.Fatal("root submission was not persisted")
				}
				w.beginTask(q)
				requestID := client.ReserveRequestID()
				w.rootRequestID = requestID
				w.startOperation("prompt", client, func(ctx context.Context) (json.RawMessage, error) {
					return client.CallWithID(ctx, requestID, "prompt", map[string]any{"message": "wait"})
				}, 100, "")
				result := waitOperation(t, w)
				requireStoreOK(t, result.err)
				w.operationReturned(result)
				acceptWorkerInput(t, w, 101, "/followup cancelled after runtime failure")
			}
			switch failure {
			case "idle-exit", "active-exit":
				requireStoreOK(t, client.TerminateNow())
				w.failed()
			case "stream-overflow":
				w.stream.WriteString(strings.Repeat("x", maxAssistantOutputBytes))
				w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"overflow"}}`))
			case "final-overflow":
				raw, err := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", maxAssistantOutputBytes+1)}}}})
				requireStoreOK(t, err)
				w.event(raw)
			case "failed-response":
				w.event([]byte(`{"type":"response","success":false}`))
			case "ui-confirm", "ui-select":
				w.owner = 7
				method := strings.TrimPrefix(failure, "ui-")
				w.ui(rpcEvent{ID: "native-dialog", Method: method, Title: "Choose", Options: []string{strings.Repeat("x", client.FrameLimit()+1)}})
				var token string
				var c confirmation
				for candidate, dialog := range w.confirms {
					if dialog.action == "ui" {
						token, c = candidate, dialog
						break
					}
				}
				if token == "" {
					t.Fatal("native dialog was not created")
				}
				if method == "confirm" {
					requireStoreOK(t, client.TerminateNow())
				}
				requireStoreOK(t, w.b.db.Accept(103, []byte(`{"update_id":103}`)))
				w.handle(incoming{id: 103, callback: &telegram.CallbackQuery{ID: "native-click", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: c.messageID, Chat: telegram.Chat{ID: w.key.chat}, MessageThreadID: w.key.thread}, Data: token + ":0"}})
				if queueState(t, w, 103) != "uncertain" {
					t.Fatal("failed native reply lost callback uncertainty")
				}
			}
			assertRetiredOperationClient(t, w, client, before)
			saved, err := w.b.db.Binding(99, w.key.chat, w.key.thread)
			if err != nil || !saved.Running || !sameBindingIdentity(saved, before) {
				t.Fatal("runtime failure closed or replaced the saved session")
			}
			if info, err := os.Stat(before.Session); err != nil || !info.Mode().IsRegular() {
				t.Fatal("runtime failure removed native history")
			}
			if active && (queueState(t, w, 100) != "uncertain" || queueState(t, w, 101) != "cancelled" || len(w.queue) != 0 || w.taskActive()) {
				t.Fatal("runtime failure lost uncertainty or retained canceled work")
			}
			acceptWorkerInput(t, w, 102, "fresh after runtime failure")
			w.dispatch()
			nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
			if w.client == nil || w.client == client || !sameBindingIdentity(w.binding, before) {
				t.Fatal("fresh input did not resume the preserved session")
			}
			wantRoots := 0
			if active {
				wantRoots = 1
			}
			if researchTraceCount(trace, "prompt", "wait") != wantRoots || researchTraceCount(trace, "prompt", "fresh after runtime failure") != 1 || researchTraceCount(trace, "prompt", "cancelled after runtime failure") != 0 {
				t.Fatal("runtime recovery replayed uncertain or canceled work")
			}
		})
	}
}

func TestAbortTimeoutRetiresRuntimeAndContinuesQueueOnFreshClient(t *testing.T) {
	order, trace := fixtureRPCControlFiles(t, "abort")
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	before := w.binding
	oldClient := w.client

	command("wait")
	w.dispatch()
	promptResult := waitOperation(t, w)
	if promptResult.err != nil {
		t.Fatal(promptResult.err)
	}
	w.operationReturned(promptResult)
	activePrompt := waitFixtureRPCTrace(t, trace, "prompt", "wait")
	if w.active != 2 || !w.busy {
		t.Fatal("fixture prompt did not become the active task")
	}

	acceptWorkerInput(t, w, 3, "/followup queued after abort")
	if len(w.queue) != 1 {
		t.Fatal("queued prompt was not retained while abort was pending")
	}
	w.beginControlOperation(controlAbort)
	w.startOperation("abort", oldClient, func(ctx context.Context) (json.RawMessage, error) {
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return oldClient.Call(callCtx, "abort", nil)
	}, 0, "Abort requested.")
	waitFixtureRPCOrder(t, order, "abort")
	abortTrace := waitFixtureRPCTrace(t, trace, "abort", "")
	result := waitOperation(t, w)
	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("held abort error = %v, want deadline exceeded", result.err)
	}
	w.operationReturned(result)
	assertRetiredOperationClient(t, w, oldClient, before)
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("aborted active task state = %q, error %v; want uncertain", state, err)
	}
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=3").Scan(&state); err != nil || state != "pending" {
		t.Fatalf("queued task state = %q, error %v; want pending", state, err)
	}

	w.dispatch()
	queuedResult := waitOperation(t, w)
	queuedPrompt := waitFixtureRPCTrace(t, trace, "prompt", "queued after abort")
	if queuedResult.err != nil {
		t.Fatal(queuedResult.err)
	}
	w.operationReturned(queuedResult)
	if w.client == nil || w.client.ID() == oldClient.ID() || activePrompt.pid != abortTrace.pid || abortTrace.pid == queuedPrompt.pid {
		t.Fatal("queued prompt reused the retired omp process or replayed the active prompt")
	}
	activeRuns := 0
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && entry.payload == "wait" {
			activeRuns++
		}
	}
	if activeRuns != 1 {
		t.Fatalf("uncertain active prompt was sent %d times, want once", activeRuns)
	}

	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"queued answer"}]}]}`))
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=3").Scan(&state); err != nil || state != "done" {
		t.Fatalf("continued queued task state = %q, error %v; want done", state, err)
	}
}

func TestSuccessfulAbortRetiresRuntimeBeforeFollowingPrompts(t *testing.T) {
	for _, control := range []string{"command", "progress"} {
		for _, timing := range []string{"before-completion", "after-completion"} {
			t.Run(control+"/"+timing, func(t *testing.T) {
				_, trace := fixtureRPCControlFiles(t, "")
				t.Setenv("OMP_TELEGRAM_FIXTURE_ABORT_CONTINUES", "1")
				w, _, command := setupWorkspaceWorker(t)
				w.b.cfg.QueueCapacity = 4
				command("/new " + t.TempDir())
				before, client := w.binding, w.client
				command("wait")
				w.dispatch()
				root := waitOperation(t, w)
				requireStoreOK(t, root.err)
				w.operationReturned(root)
				command("/followup queued before stop")
				if control == "command" {
					w.stop()
				} else {
					w.stopActiveTask()
				}
				if timing == "after-completion" {
					nativeUntil(t, w, func() bool { return queueState(t, w, 2) != "submitted" && !w.controlInProgress() })
				}
				acceptWorkerInput(t, w, 100, "fresh message after stop")
				w.dispatch()
				nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" || queueState(t, w, 100) == "uncertain" })
				if queueState(t, w, 100) != "done" {
					t.Fatalf("message after Stop state = %s, want done", queueState(t, w, 100))
				}
				var replies int
				requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=100 AND text=?", "answer: fresh message after stop").Scan(&replies))
				if replies != 1 || queueState(t, w, 2) != "cancelled" {
					t.Fatal("Stop consumed the fresh message as steering or failed to cancel the old root")
				}
				if w.client == nil || w.client == client || !sameBindingIdentity(w.binding, before) {
					t.Fatal("message after Stop did not resume the same session on a fresh runtime")
				}
				select {
				case <-client.Done():
				default:
					t.Fatal("old runtime was still alive after the new task completed")
				}
				wantQueued := "cancelled"
				if control == "progress" {
					wantQueued = "done"
				}
				if queueState(t, w, 3) != wantQueued {
					t.Fatal("Stop changed its existing bridge queue policy")
				}
				if researchTraceCount(trace, "prompt", "wait") != 1 || researchTraceCount(trace, "prompt", "fresh message after stop") != 1 {
					t.Fatal("Stop replayed work or did not submit the fresh message exactly once")
				}
			})
		}
	}
}

func TestIdleAbortPreservesUnsavedSession(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before, client := w.binding, w.client
	requireStoreOK(t, os.Remove(before.Session))
	command("/stop")
	if w.client != client || !sameBindingIdentity(w.binding, before) || w.controlInProgress() {
		t.Fatal("idle Stop discarded the only copy of an unsaved native session")
	}
}

func TestIdleAbortUsesNativeWorkStateInsteadOfSavedHistory(t *testing.T) {
	for _, state := range []string{"idle", "pending", "unknown"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_ABORT_STATE", state)
			w, _, command := setupWorkspaceWorker(t)
			w.b.cfg.QueueCapacity = 1
			command("/new " + t.TempDir())
			client, before := w.client, w.binding
			if !w.sessionDurable() {
				t.Fatal("fixture did not create saved native history")
			}
			command("/stop")
			if !w.binding.Running || !sameBindingIdentity(w.binding, before) {
				t.Fatal("idle Stop changed the saved session identity")
			}
			if state == "idle" {
				if w.client != client {
					t.Fatal("idle Stop restarted a quiescent saved session")
				}
			} else {
				assertRetiredOperationClient(t, w, client, before)
			}
			t.Setenv("OMP_TELEGRAM_FIXTURE_ABORT_STATE", "idle")
			command("after idle stop")
			w.dispatch()
			nativeUntil(t, w, func() bool { return queueState(t, w, 3) == "done" })
			if !sameBindingIdentity(w.binding, before) {
				t.Fatal("next prompt did not retain the idle session")
			}
		})
	}
}

func TestIdleAbortRetiresPendingNativeDialog(t *testing.T) {
	for _, method := range []string{"confirm", "select"} {
		for _, delivery := range []string{"rendered", "buffered"} {
			t.Run(method+"/"+delivery, func(t *testing.T) {
				w, _, command := setupWorkspaceWorker(t)
				command("/new " + t.TempDir())
				before, client := w.binding, w.client
				nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
					"type": "extension_ui_request", "id": "idle-stop-dialog", "method": method,
					"title": "Startup permission", "message": "Allow the startup action?", "options": []string{"Allow", "Deny"},
				}})
				if delivery == "rendered" {
					nativeUntil(t, w, func() bool { return len(w.confirms) == 1 })
				}
				if w.taskActive() {
					t.Fatal("the startup dialog unexpectedly created a bridge task")
				}
				command("/stop")
				assertRetiredOperationClient(t, w, client, before)
				if len(w.confirms) != 0 || len(w.b.slots) != 0 {
					t.Fatal("Stop retained a native dialog or its process slot")
				}
				command("/name after stopped dialog")
				if w.client == nil || w.client == client || !sameBindingIdentity(w.binding, before) {
					t.Fatal("Stop did not leave the original session available on a replacement runtime")
				}
			})
		}
	}
}

func TestAbortRetiresUnsavedRootAfterEarlyTerminal(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_ABORT_CONTINUES", "1")
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	command("/new " + t.TempDir())
	before, client := w.binding, w.client
	command("wait")
	w.dispatch()
	root := waitOperation(t, w)
	requireStoreOK(t, root.err)
	w.operationReturned(root)
	requireStoreOK(t, os.Remove(before.Session))
	w.stopActiveTask()
	abort := waitOperation(t, w)
	requireStoreOK(t, abort.err)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for w.taskActive() {
		select {
		case raw := <-client.Events():
			w.event(raw)
		case <-timer.C:
			t.Fatal("native abort did not settle the root before its queued callback")
		}
	}
	w.operationReturned(abort)
	if w.client != nil || !w.binding.Running || !sameBindingIdentity(w.binding, before) {
		t.Fatal("early terminal bypassed retirement of an active unsaved session")
	}
	select {
	case <-client.Done():
	default:
		t.Fatal("stopped runtime remained alive after the early terminal")
	}
}

func TestRejectedAbortKeepsRuntime(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_HOLD_RPC", "")
	t.Setenv("OMP_TELEGRAM_FIXTURE_REJECT_RPC", "abort")
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	command("/new " + t.TempDir())
	client := w.client
	command("wait")
	w.dispatch()
	root := waitOperation(t, w)
	requireStoreOK(t, root.err)
	w.operationReturned(root)
	command("/stop")
	if w.client != client || w.runtime != runtimeConnected || w.controlInProgress() {
		t.Fatal("explicit abort rejection invalidated the reusable runtime")
	}
}

func TestUncertainModelMutationsRetireRuntime(t *testing.T) {
	for _, scenario := range []struct {
		name, kind, method string
		fields             map[string]any
		request            modelOperationRequest
	}{
		{name: "switch", kind: "model_set_model", method: "set_model", fields: map[string]any{"provider": "fixture", "modelId": "changed"}, request: modelOperationRequest{action: "model_switch"}},
		{name: "thinking", kind: "model_set_thinking", method: "set_thinking_level", fields: map[string]any{"level": "high"}, request: modelOperationRequest{action: "thinking_select", requested: "high"}},
		{name: "fast", kind: "model_set_fast", method: "set_fast_mode", fields: map[string]any{"enabled": true}, request: modelOperationRequest{action: "fast_select", enabled: true}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, trace := fixtureRPCControlFiles(t, scenario.method)
			w, _, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			before := w.binding
			client, sideEffect, result := startTimedFixtureOperation(t, w, scenario.kind, scenario.method, scenario.fields, scenario.request)
			w.operationReturned(result)
			assertRetiredOperationClient(t, w, client, before)
			if sideEffect.pid == "" || len(fixtureRPCTrace(trace)) == 0 {
				t.Fatal("fixture did not record the applied mutation before withholding its response")
			}

			command("/name lazy resumed")
			if w.client == nil || w.client.ID() == client.ID() || w.runtime != runtimeConnected || !sameBindingIdentity(w.binding, before) {
				t.Fatal("next control command did not resume the preserved session with a fresh runtime")
			}
		})
	}
}

func TestRejectedModelMutationKeepsRuntime(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_HOLD_RPC", "")
	t.Setenv("OMP_TELEGRAM_FIXTURE_REJECT_RPC", "set_model")
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	client := w.client
	command("/model fixture/rejected")
	if w.client != client || w.runtime != runtimeConnected || w.controlInProgress() {
		t.Fatal("explicit model rejection invalidated the reusable runtime")
	}
	assertPickerModel(t, w, "fixture", "safe")
}

func TestQueuedPromptAfterUncertainModelSwitchUsesFreshClient(t *testing.T) {
	order, trace := fixtureRPCControlFiles(t, "set_model")
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	before := w.binding
	oldClient := w.client
	request := modelOperationRequest{action: "model_switch"}
	w.beginControlOperation(controlModel)
	w.startOperation("model_set_model", oldClient, func(ctx context.Context) (json.RawMessage, error) {
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return oldClient.Call(callCtx, "set_model", map[string]any{"provider": "fixture", "modelId": "uncertain"})
	}, 0, "", request)
	waitFixtureRPCOrder(t, order, "set_model")
	setter := waitFixtureRPCTrace(t, trace, "set_model", "")
	acceptWorkerInput(t, w, 2, "queued after model switch")
	result := waitOperation(t, w)
	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("held model switch error = %v, want deadline exceeded", result.err)
	}
	w.operationReturned(result)
	assertRetiredOperationClient(t, w, oldClient, before)
	if len(w.queue) != 1 {
		t.Fatal("prompt queued during model switch was lost")
	}

	w.dispatch()
	promptResult := waitOperation(t, w)
	queued := waitFixtureRPCTrace(t, trace, "prompt", "queued after model switch")
	if promptResult.err != nil {
		t.Fatal(promptResult.err)
	}
	w.operationReturned(promptResult)
	if setter.pid == queued.pid || w.client == nil || w.client.ID() == oldClient.ID() {
		t.Fatal("queued prompt ran on the client with an uncertain model setting")
	}
	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"queued answer"}]}]}`))
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&state); err != nil || state != "done" {
		t.Fatalf("queued task state = %q, error %v; want done", state, err)
	}
}

func TestModelRoleFailureRetiresRuntimeWithoutClosingBinding(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_HOLD_RPC", "")
	t.Setenv("OMP_TELEGRAM_FIXTURE_REJECT_RPC", "model_role")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before := w.binding
	client := w.client
	w.beginControlOperation(controlModel)
	w.startOperation("model_set_role", client, func(ctx context.Context) (json.RawMessage, error) {
		model, err := client.SetModelRole(ctx, "slow")
		if err != nil {
			return nil, err
		}
		return json.Marshal(model)
	}, 0, "", modelOperationRequest{action: "model_select", role: "slow"})
	waitFixtureRPCTrace(t, trace, "prompt", "/model @slow")
	result := waitOperation(t, w)
	if result.err == nil || omp.ClassifyError(result.err) != "rejected" {
		t.Fatalf("native model role failure = %v, want explicit RPC rejection", result.err)
	}
	w.operationReturned(result)
	assertRetiredOperationClient(t, w, client, before)
	command("/name lazy resumed")
	if w.client == nil || w.client.ID() == client.ID() || !sameBindingIdentity(w.binding, before) {
		t.Fatal("native role failure did not lazily restore the existing binding")
	}
}

type runtimeLogCapture struct {
	mu         sync.Mutex
	lines      []string
	exitSeen   chan struct{}
	exitSignal sync.Once
}

func (w *runtimeLogCapture) Write(p []byte) (int, error) {
	line := string(p)
	w.mu.Lock()
	w.lines = append(w.lines, line)
	w.mu.Unlock()
	if bytes.Contains(p, []byte("event=runtime_exit")) {
		w.exitSignal.Do(func() { close(w.exitSeen) })
	}
	return len(p), nil
}

func (w *runtimeLogCapture) clientID(event string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range w.lines {
		if !strings.Contains(line, "event="+event) {
			continue
		}
		for _, field := range strings.Fields(line) {
			key, value, ok := strings.Cut(field, "=")
			if ok && key == "client_id" {
				return value
			}
		}
	}
	return ""
}

func TestModelRoleFailureDoesNotRaceClientDone(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_REJECT_RPC", "model_role")
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before := w.binding
	client := w.client
	if client == nil {
		t.Fatal("fixture runtime is not connected")
	}

	logs := &runtimeLogCapture{exitSeen: make(chan struct{})}
	w.log = slog.New(slog.NewTextHandler(logs, nil)).With("chat_id", w.key.chat, "thread_id", w.key.thread)
	w.b.cfg.QueueCapacity = 4
	w.beginControlOperation(controlModel)
	acceptWorkerInput(t, w, 2, "queued after role failure")
	if len(w.queue) != 1 {
		t.Fatal("prompt did not queue behind the model role operation")
	}

	releaseResult := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseResult) }) }
	w.startOperation("model_set_role", client, func(ctx context.Context) (json.RawMessage, error) {
		model, err := client.SetModelRole(ctx, "slow")
		if err != nil {
			<-client.Done()
			<-releaseResult
			return nil, err
		}
		return json.Marshal(model)
	}, 0, "", modelOperationRequest{action: "model_select", role: "slow"})
	waitFixtureRPCTrace(t, trace, "prompt", "/model @slow")

	w.input = make(chan incoming, 1)
	runDone := make(chan struct{})
	go func() {
		w.run()
		close(runDone)
	}()
	t.Cleanup(func() {
		release()
		w.cancel()
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Error("worker loop did not stop")
		}
	})

	select {
	case <-logs.exitSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("worker loop did not observe the failed runtime")
	}
	release()

	var taskState string
	waitFor(t, func() bool {
		err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&taskState)
		return err == nil && (taskState == "done" || taskState == "cancelled" || taskState == "uncertain")
	})
	if taskState != "done" {
		t.Fatalf("queued task state = %q, want done", taskState)
	}
	binding, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	if err != nil || !binding.Running || !sameBindingIdentity(binding, before) {
		t.Fatalf("role failure changed logical binding: binding=%+v, error=%v", binding, err)
	}
	oldClientID, newClientID := logs.clientID("runtime_exit"), logs.clientID("runtime_resume")
	if oldClientID == "" || newClientID == "" || oldClientID == newClientID {
		t.Fatalf("queued prompt did not resume on a fresh runtime: old=%q new=%q", oldClientID, newClientID)
	}
}

func TestMalformedModelMutationResultsRetireRuntime(t *testing.T) {
	for _, scenario := range []struct {
		name, kind, action string
		data               json.RawMessage
	}{
		{name: "model identity", kind: "model_set_model", action: "model_switch", data: json.RawMessage(`{"provider":"fixture"}`)},
		{name: "thinking level", kind: "model_verify_thinking", action: "thinking_select", data: json.RawMessage(`{}`)},
		{name: "fast state", kind: "model_set_fast", action: "fast_select", data: json.RawMessage(`{"enabled":true}`)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, _, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			client := w.client
			before := w.binding
			w.beginControlOperation(controlModel)
			w.operationFinished(operationResult{
				generation: w.binding.Generation,
				clientID:   client.ID(),
				kind:       scenario.kind,
				data:       scenario.data,
				meta:       modelOperationRequest{action: scenario.action},
			})
			assertRetiredOperationClient(t, w, client, before)
		})
	}
}
