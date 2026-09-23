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
	w.controlBusy = true
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
	if w.client != nil || w.runtime != runtimeReleased || w.controlBusy || !w.binding.Running || !sameBindingIdentity(w.binding, binding) {
		t.Fatalf("uncertain operation did not retire only its runtime: client=%v runtime=%v controlBusy=%t binding=%+v", w.client, w.runtime, w.controlBusy, w.binding)
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

	acceptWorkerInput(t, w, 3, "queued after abort")
	if len(w.queue) != 1 {
		t.Fatal("queued prompt was not retained while abort was pending")
	}
	w.controlBusy = true
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
	queuedPrompt := waitFixtureRPCTrace(t, trace, "prompt", "queued after abort")
	queuedResult := waitOperation(t, w)
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

func TestRejectedAbortKeepsRuntime(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_HOLD_RPC", "")
	t.Setenv("OMP_TELEGRAM_FIXTURE_REJECT_RPC", "abort")
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	client := w.client
	command("/stop")
	if w.client != client || w.runtime != runtimeConnected || w.controlBusy {
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
	if w.client != client || w.runtime != runtimeConnected || w.controlBusy {
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
	w.controlBusy = true
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
	queued := waitFixtureRPCTrace(t, trace, "prompt", "queued after model switch")
	promptResult := waitOperation(t, w)
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
	w.controlBusy = true
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
	w.controlBusy = true
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
			w.controlBusy = true
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
