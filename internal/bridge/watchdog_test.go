package bridge

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIdleProbeRequiresExplicitFalseState(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"isStreaming":false}`, `{"isStreaming":null,"isCompacting":false}`, `{"isStreaming":false,"isCompacting":null}`, `{"isStreaming":true,"isCompacting":false}`, `{"isStreaming":false,"isCompacting":true}`, `{"isStreaming":"false","isCompacting":false}`} {
		idle, _, _ := rpcIdleState([]byte(raw))
		if idle {
			t.Fatalf("unconfirmed state accepted as idle: %s", raw)
		}
	}
	idle, _, _ := rpcIdleState([]byte(`{"isStreaming":false,"isCompacting":false}`))
	if !idle {
		t.Fatal("explicit idle state rejected")
	}
}

func probeResultFor(w *worker) idleProbeResult {
	return idleProbeResult{client: w.client, generation: w.binding.Generation, turn: w.turn, active: w.active, revision: w.idleProbe.revision, idle: true}
}

func TestRejectedNativeCallInvalidatesIdleEvidence(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	command("/new " + t.TempDir())
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	now := time.Now()
	w.idleProbeFinished(probeResultFor(w), now)
	stale := probeResultFor(w)
	ctx, cancel := context.WithCancel(w.ctx)
	w.idleProbe.cancel = cancel
	if _, err := w.call("set_session_name", map[string]any{"name": ""}); err == nil {
		t.Fatal("native request unexpectedly accepted")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("failed RPC left in-flight probe active")
	}
	w.idleProbeFinished(stale, now.Add(time.Minute))
	w.idleProbeFinished(probeResultFor(w), now.Add(time.Minute))
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", w.active).Scan(&state); err != nil || state != "submitted" || !w.busy {
		t.Fatalf("failed RPC retained previous idle evidence: state=%q err=%v", state, err)
	}
}

func drainWatchdogEvents(t *testing.T, w *worker) {
	t.Helper()
	// The later response guarantees earlier events have entered the client queue.
	w.operationReturned(waitOperation(t, w))
	client := w.client
	if _, err := client.Call(w.ctx, "get_state", nil); err != nil {
		t.Fatal(err)
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for client.BufferedOutput() {
		select {
		case raw, ok := <-client.Events():
			if !ok {
				w.failed()
				return
			}
			w.event(raw)
			if w.client != client {
				return
			}
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("buffered RPC event was not forwarded")
		}
	}
	awaitReplyStatus(t, w)
}

func TestMissingTerminalRecoveryRetiresClientAndPreservesQueue(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	active, oldClient, binding, claim := w.active, w.client, w.binding, w.claimedSession
	command("/followup next task")
	queuedID := w.queue[0].id
	now := time.Now()
	w.lastActivity = now.Add(-stuckTaskQuietPeriod)
	w.probeStuckTask(now)
	select {
	case result := <-w.idleProbe.results:
		w.idleProbeFinished(result, now)
	case <-w.ctx.Done():
		t.Fatal("idle probe did not return")
	}
	if w.active != active || !w.busy || w.idleProbe.confirmed.IsZero() {
		t.Fatal("first idle observation completed task or failed to confirm")
	}
	stale := probeResultFor(w)
	now = now.Add(stuckTaskQuietPeriod)
	w.probeStuckTask(now)
	select {
	case result := <-w.idleProbe.results:
		w.idleProbeFinished(result, now)
	case <-w.ctx.Done():
		t.Fatal("second idle probe did not return")
	}
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", active).Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("missing completion state=%q err=%v", state, err)
	}
	if w.client != nil || w.busy || !sameBindingIdentity(w.binding, binding) || w.claimedSession != claim || len(w.queue) != 1 {
		t.Fatal("recovery lost logical session/queue or retained old runtime")
	}
	select {
	case <-oldClient.Done():
	default:
		t.Fatal("old runtime survived recovery")
	}
	w.dispatch()
	nativeUntil(t, w, func() bool { return w.active == queuedID })
	if w.client == oldClient {
		t.Fatal("queued task did not resume on a new client")
	}
	w.idleProbeFinished(stale, now.Add(time.Minute))
	if w.active != queuedID {
		t.Fatal("old probe completed following task")
	}
	drainWatchdogEvents(t, w)
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", queuedID).Scan(&state); err != nil || state != "done" {
		t.Fatalf("queued completion state=%q err=%v", state, err)
	}
}

func TestActiveAskBlocksWatchdogUntilNativeCancellation(t *testing.T) {
	for _, method := range []string{"ask", "input", "editor"} {
		t.Run(method, func(t *testing.T) {
			w, _, command := setupWorkspaceWorker(t)
			w.b.cfg.QueueCapacity = 4
			command("/new " + t.TempDir())
			command("missing-terminal")
			w.dispatch()
			drainWatchdogEvents(t, w)
			active, client, binding, turn, root := w.active, w.client, w.binding, w.turn, w.rootRequestID
			raw, err := client.Call(w.ctx, "get_state", nil)
			idle, _, _ := rpcIdleState(raw)
			if err != nil || !idle || w.awaitingContinuation || len(w.progress.ActiveTools) != 0 {
				t.Fatalf("fixture did not enter idle without native work: idle=%v err=%v", idle, err)
			}
			w.event([]byte(`{"type":"extension_ui_request","id":"watchdog-ask","method":"` + method + `","title":"Choose a topic","questions":[{"id":"topic","question":"Choose a topic","options":[{"label":"Go"}]}]}`))
			a := w.ask
			if a == nil || a.cancel == nil {
				t.Fatal("Ask did not begin asynchronous display")
			}
			if _, exists := w.confirms[a.token]; exists {
				t.Fatal("Ask confirmation installed before display completion")
			}
			now := time.Now().Add(5 * time.Minute)
			for _, phase := range []string{"rendering", "displayed"} {
				if phase == "displayed" {
					for a.cancel != nil {
						w.operationReturned(waitOperation(t, w))
					}
					if w.confirms[a.token].action != "ask" {
						t.Fatal("completed Ask display did not install confirmation")
					}
					command("watchdog guidance")
					if len(w.deferredSteers) != 1 || queueState(t, w, 3) != "pending" {
						t.Fatal("guidance did not defer behind active Ask")
					}
				}
				w.lastActivity = now.Add(-stuckTaskQuietPeriod)
				w.probeStuckTask(now)
				w.idleProbeFinished(probeResultFor(w), now)
				w.idleProbeFinished(probeResultFor(w), now.Add(stuckTaskQuietPeriod))
				if w.client != client || w.active != active || w.ask != a || !w.busy ||
					!sameBindingIdentity(w.binding, binding) || w.turn != turn || w.rootRequestID != root ||
					queueState(t, w, active) != "submitted" || w.idleProbe.cancel != nil || !w.idleProbe.confirmed.IsZero() {
					t.Fatalf("%s Ask wait was probed, revoked, or lost its original root", phase)
				}
				select {
				case <-client.Done():
					t.Fatalf("%s Ask wait retired its client", phase)
				default:
				}
				now = now.Add(2 * stuckTaskQuietPeriod)
			}
			w.event([]byte(`{"type":"extension_ui_request","method":"cancel","targetId":"watchdog-ask"}`))
			if w.ask != nil || len(w.deferredSteers) != 1 || !w.stuckTaskEligible() {
				t.Fatal("deferred guidance alone blocked recovery after Ask cancellation")
			}
			for probe := range 2 {
				w.probeStuckTask(now)
				select {
				case result := <-w.idleProbe.results:
					w.idleProbeFinished(result, now)
				case <-w.ctx.Done():
					t.Fatal("idle probe did not return after Ask cancellation")
				}
				if probe == 0 && (w.active != active || w.idleProbe.confirmed.IsZero()) {
					t.Fatal("first post-Ask idle observation did not retain and confirm root")
				}
				now = now.Add(stuckTaskQuietPeriod)
			}
			if queueState(t, w, active) != "uncertain" || w.client != nil || w.busy ||
				len(w.deferredSteers) != 0 || queueState(t, w, 3) != "cancelled" {
				t.Fatal("stuck root did not recover and revoke deferred guidance after Ask ended")
			}
			select {
			case <-client.Done():
			default:
				t.Fatal("stuck runtime survived post-Ask recovery")
			}
		})
	}
}

func TestNonTerminalAgentEndBlocksMissingCompletionWatchdog(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 2
	command("/new " + t.TempDir())
	command("nonterminal-only")
	w.dispatch()
	drainWatchdogEvents(t, w)
	active, client := w.active, w.client
	raw, err := client.Call(w.ctx, "get_state", nil)
	idle, _, _ := rpcIdleState(raw)
	if err != nil || !idle {
		t.Fatalf("fixture did not enter native async idle: %v", err)
	}
	now := time.Now().Add(5 * time.Minute)
	w.probeStuckTask(now)
	w.idleProbeFinished(probeResultFor(w), now)
	w.idleProbeFinished(probeResultFor(w), now.Add(time.Minute))
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", active).Scan(&state); err != nil || state != "submitted" {
		t.Fatalf("async wait was settled: state=%q err=%v", state, err)
	}
	if w.client != client || w.active != active || !w.busy || w.idleProbe.cancel != nil {
		t.Fatal("async continuation was probed or retired")
	}
	w.event([]byte(`{"type":"agent_start"}`))
	w.idleProbeFinished(probeResultFor(w), now)
	w.idleProbeFinished(probeResultFor(w), now.Add(time.Minute))
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", active).Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("resumed turn missing completion did not recover: state=%q err=%v", state, err)
	}
}

func TestIdleConfirmationResetAndFences(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 2
	command("/new " + t.TempDir())
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	now := time.Now()
	for _, activity := range []string{`{"type":"agent_end","isTerminal":false}`, `{"type":"unrecognized_activity"}`, `{invalid`} {
		first := probeResultFor(w)
		w.idleProbeFinished(first, now)
		w.event([]byte(activity))
		w.idleProbeFinished(first, now.Add(time.Minute))
		if !w.idleProbe.confirmed.IsZero() || w.active == 0 {
			t.Fatal("activity failed to invalidate idle observation")
		}
		w.event([]byte(`{"type":"agent_start"}`))
	}
	for _, result := range []idleProbeResult{{idle: false}, {idle: true, err: errors.New("probe failed")}} {
		w.idleProbeFinished(probeResultFor(w), now)
		current := probeResultFor(w)
		current.idle, current.err = result.idle, result.err
		w.idleProbeFinished(current, now.Add(time.Minute))
		if !w.idleProbe.confirmed.IsZero() || w.active == 0 {
			t.Fatal("failed/unknown probe failed to reset confirmation")
		}
	}
	for _, block := range []func(){
		func() { w.compacting = true },
		func() { w.progress.Retrying = true },
		func() { w.progress.ActiveTools = map[string]progressTool{"tool": {Name: "read"}} },
		func() { w.hostRequests = map[string]context.CancelFunc{"host": func() {}} },
		func() { w.confirms["ui"] = confirmation{action: "ui"} },
	} {
		block()
		if w.stuckTaskEligible() {
			t.Fatal("native work or user wait considered recoverable")
		}
		w.compacting = false
		w.progress = progressState{}
		w.hostRequests = nil
		delete(w.confirms, "ui")
	}
}

func TestTerminalCancelsPendingTyping(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	command("/new " + t.TempDir())
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	w.b.cfg.ProgressMode = "summary"
	f.typingRequests = make(chan context.Context, 1)
	w.typing()
	var ctx context.Context
	select {
	case ctx = <-f.typingRequests:
	case <-time.After(time.Second):
		t.Fatal("typing request did not reach transport")
	}
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","content":[{"type":"text","text":"done"}]}]}`))
	select {
	case <-ctx.Done():
	default:
		t.Fatal("terminal left typing request active")
	}
}

func TestLateAgentStartDoesNotWedgeIdleQueue(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 2
	w.b.cfg.ProgressMode = "summary"
	command("/new " + t.TempDir())
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","content":[{"type":"text","text":"done"}]}]}`))
	awaitReplyStatus(t, w)
	if w.active != 0 || w.busy {
		t.Fatal("terminal event did not release the task")
	}
	command("next task")
	w.event([]byte(`{"type":"agent_start"}`))
	if w.taskOrSessionBusy() {
		t.Fatal("late agent start made the idle worker busy")
	}
	w.typing()
	if !w.lastTyping.IsZero() {
		t.Fatal("late agent start resumed typing")
	}
	w.dispatch()
	if w.active == 0 || len(w.queue) != 0 {
		t.Fatal("late agent start prevented queued prompt dispatch")
	}
}

func TestIdleProbeRejectsOldOwnershipAndQueuedActivity(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	command("/new " + t.TempDir())
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	now := time.Now()
	w.idleProbeFinished(probeResultFor(w), now)
	for _, change := range []func(*idleProbeResult){
		func(r *idleProbeResult) { r.client = nil },
		func(r *idleProbeResult) { r.generation-- },
		func(r *idleProbeResult) { r.turn-- },
		func(r *idleProbeResult) { r.active-- },
		func(r *idleProbeResult) { r.revision-- },
	} {
		result := probeResultFor(w)
		change(&result)
		w.idleProbeFinished(result, now.Add(time.Minute))
		if w.active == 0 || w.idleProbe.confirmed != now {
			t.Fatal("stale ownership result changed active task")
		}
	}
	// A real terminal event queued before the probe must win, even if the actor
	// has not yet received it and therefore has not incremented its revision.
	result := probeResultFor(w)
	if _, err := w.client.Call(w.ctx, "abort", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.client.Call(w.ctx, "get_state", nil); err != nil {
		t.Fatal(err)
	}
	w.idleProbeFinished(result, now.Add(time.Minute))
	if w.client != result.client {
		t.Fatal("probe retired runtime despite queued RPC output")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for w.active != 0 {
		select {
		case raw, ok := <-result.client.Events():
			if !ok {
				t.Fatal("runtime closed before queued terminal event")
			}
			w.event(raw)
		case <-deadline.C:
			t.Fatal("queued terminal event did not complete the task")
		}
	}
	if w.client != result.client {
		t.Fatal("terminal event retired runtime")
	}
}
