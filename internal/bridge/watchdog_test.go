package bridge

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIdleProbeRequiresExplicitFalseState(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"isStreaming":false}`, `{"isStreaming":null,"isCompacting":false}`, `{"isStreaming":false,"isCompacting":null}`, `{"isStreaming":true,"isCompacting":false}`, `{"isStreaming":false,"isCompacting":true}`, `{"isStreaming":"false","isCompacting":false}`} {
		if explicitlyIdle([]byte(raw)) {
			t.Fatalf("unconfirmed state accepted as idle: %s", raw)
		}
	}
	if !explicitlyIdle([]byte(`{"isStreaming":false,"isCompacting":false}`)) {
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
	// Consume the prompt acknowledgment before using a later RPC as the event barrier.
	w.operationReturned(waitOperation(t, w))
	if _, err := w.client.Call(w.ctx, "get_state", nil); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case raw := <-w.client.Events():
			w.event(raw)
		default:
			return
		}
	}
}

func TestMissingTerminalRecoveryRetiresClientAndPreservesQueue(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	active, oldClient, binding, claim := w.active, w.client, w.binding, w.claimedSession
	command("next task")
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
	if w.client == oldClient || w.active != queuedID {
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

func TestNonTerminalAgentEndBlocksMissingCompletionWatchdog(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 2
	command("/new " + t.TempDir())
	command("nonterminal-only")
	w.dispatch()
	drainWatchdogEvents(t, w)
	active, client := w.active, w.client
	raw, err := client.Call(w.ctx, "get_state", nil)
	if err != nil || !explicitlyIdle(raw) {
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
		func() { w.progress.ActiveTools = map[string]progressTool{"tool": {Running: true}} },
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
	if w.client != result.client || w.active != 0 {
		t.Fatal("probe retired runtime instead of honoring queued terminal event")
	}
}
