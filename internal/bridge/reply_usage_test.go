package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureReplyUsageReport = "```\nUsage (0s ago)\n\nOpenai Codex\n- 5 hours\n  PRIVATE_USAGE_ACCOUNT (pro): 30.00% used (70.0% left)  ← in use by this session\n- 7 days\n  PRIVATE_USAGE_ACCOUNT (pro): 65.00% used (35.0% left)  ← in use by this session\n```"

func fixtureReplyUsage(command, response map[string]any, emit func(any)) bool {
	mode := os.Getenv("OMP_TELEGRAM_FIXTURE_USAGE_MODE")
	if mode == "" || command["message"] != "/usage" {
		return false
	}
	recordFixtureRPCTrace("prompt", "/usage")
	response["data"] = map[string]any{"agentInvoked": false}
	switch mode {
	case "hold", "hold-report":
		recordFixtureRPCTrace("usage_waiting", "")
		if gate := os.Getenv("OMP_TELEGRAM_FIXTURE_USAGE_RELEASE"); gate != "" {
			waitForFixtureFile(gate)
		}
		if mode == "hold" {
			return true
		}
		emit(map[string]any{"type": "command_output", "text": fixtureReplyUsageReport})
	case "reject":
		response["success"], response["error"] = false, "PRIVATE_USAGE_REJECTION"
	case "missing":
	case "fallback":
		emit(map[string]any{"type": "command_output", "text": "Usage\nInput tokens: 123\nCost: $0.01"})
	default:
		emit(map[string]any{"type": "command_output", "text": fixtureReplyUsageReport})
		if mode == "unknown-ack" {
			response["data"] = map[string]any{}
		}
	}
	emit(response)
	return true
}

func replyUsageWorker(t *testing.T, mode string) (*worker, func(string), string) {
	t.Helper()
	t.Setenv("OMP_TELEGRAM_FIXTURE_USAGE_MODE", mode)
	w, _, command, trace := replyStatusWorker(t)
	w.b.cfg.ProviderUsage = true
	nativeRPC(t, w, "set_model", map[string]any{"provider": "openai-codex", "modelId": "safe"})
	return w, command, trace
}

func endUsageReply(w *worker) {
	w.event([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","model":"safe","provider":"openai-codex","duration":1000,"usage":{"output":100,"totalTokens":1100},"content":[{"type":"text","text":"answer"}]}}`))
	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","stopReason":"stop","model":"safe","provider":"openai-codex","duration":1000,"usage":{"output":100,"totalTokens":1100},"content":[{"type":"text","text":"answer"}]}]}`))
}

func TestFinalReplyProviderUsage(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			w, _, trace := replyUsageWorker(t, "normal")
			w.b.cfg.ProviderUsage = enabled
			w.refreshProgressNative(time.Now())
			if researchTraceCount(trace, "prompt", "/usage") != 0 {
				t.Fatal("quota queried before final reply")
			}
			endUsageReply(w)
			awaitReplyStatus(t, w)
			metadata := finalReplyStatus(t, w, 2, "answer")
			if queueState(t, w, 2) != "done" || !strings.Contains(metadata, "100 t/s") {
				t.Fatalf("quota changed the confirmed answer metadata: %q", metadata)
			}
			if strings.Contains(metadata, "5h30% 7d65%") != enabled {
				t.Fatalf("final quota opt-in mismatch: %q", metadata)
			}
			wantQueries := 0
			if enabled {
				wantQueries = 1
			}
			if researchTraceCount(trace, "prompt", "/usage") != wantQueries {
				t.Fatal("quota did not execute exactly once at final reply")
			}
			var leaked int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE text LIKE '%PRIVATE_USAGE%' OR text LIKE '%in use by this session%' OR header LIKE '%PRIVATE_USAGE%' OR header LIKE '%in use by this session%'").Scan(&leaked))
			if leaked != 0 {
				t.Fatal("private quota report escaped to outbox")
			}
		})
	}
}

func TestFinalReplyUsageSkipsQueuedPrompt(t *testing.T) {
	w, _, trace := replyUsageWorker(t, "hold")
	client := w.client
	nativeInput(t, w, 100, "/followup queued next task", false)
	endUsageReply(w)
	awaitReplyStatus(t, w)
	metadata := finalReplyStatus(t, w, 2, "answer")
	if queueState(t, w, 2) != "done" || strings.Contains(metadata, "5h") {
		t.Fatalf("queued work changed the confirmed answer metadata: %q", metadata)
	}
	if researchTraceCount(trace, "prompt", "/usage") != 0 || w.client != client || client.Stopping() || queueState(t, w, 100) != "pending" {
		t.Fatal("queued prompt triggered optional usage or runtime retirement")
	}
	w.dispatch()
	drainWatchdogEvents(t, w)
	if queueState(t, w, 100) != "done" || w.client != client || researchTraceCount(trace, "prompt", "queued next task") != 1 {
		t.Fatal("queued prompt did not complete once on the existing runtime")
	}
}

func TestFinalReplyUsageUnsentCancellationKeepsRuntime(t *testing.T) {
	w, _, trace := replyUsageWorker(t, "hold")
	client, binding := w.client, w.binding
	endUsageReply(w)
	sample := receiveReplyStatus(t, w)
	// Hold only the private query's prerequisite, after the final snapshot arrived.
	nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "get_state"})
	w.operationReturned(sample)
	waitFixtureRPCTrace(t, trace, "research_control_waiting", "get_state")
	nativeInput(t, w, 100, "next after unsent usage", false)
	if queueState(t, w, 2) != "done" || queueState(t, w, 100) != "submitted" || w.client != client || client.Stopping() {
		t.Fatal("new input retired the unsent query's runtime or lost the next task")
	}
	result := waitOperation(t, w)
	if result.kind != "reply_usage" || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("expected an unsent canceled usage query: kind=%s, error=%v", result.kind, result.err)
	}
	w.operationReturned(result)
	if w.client != client || client.Stopping() || len(w.b.slots) != 1 || !sameBindingIdentity(binding, w.binding) {
		t.Fatal("unsent cancellation changed runtime, process slot, or session identity")
	}
	nativeRPC(t, w, "fixture_research_control_release", nil)
	w.dispatch()
	drainWatchdogEvents(t, w)
	if queueState(t, w, 100) != "done" || w.client != client ||
		researchTraceCount(trace, "prompt", "/usage") != 0 || researchTraceCount(trace, "prompt", "next after unsent usage") != 1 {
		t.Fatal("unsent usage reached native or subsequent input did not reuse the runtime")
	}
}

func TestFinalReplyUsageFailuresPreserveConfirmedAnswer(t *testing.T) {
	for _, mode := range []string{"unavailable", "fallback", "reject", "missing", "unknown-ack", "hold"} {
		t.Run(mode, func(t *testing.T) {
			w, _, trace := replyUsageWorker(t, mode)
			endUsageReply(w)
			awaitReplyStatus(t, w)
			metadata := finalReplyStatus(t, w, 2, "answer")
			if queueState(t, w, 2) != "done" || !strings.Contains(metadata, "100 t/s") || strings.Contains(metadata, "5h") {
				t.Fatalf("optional quota failure changed confirmed reply: %q", metadata)
			}
			wantQueries := 1
			if mode == "unavailable" {
				wantQueries = 0
			}
			if researchTraceCount(trace, "prompt", "/usage") != wantQueries {
				t.Fatal("quota failure retried or bypassed builtin proof")
			}
		})
	}
}

func TestFinalReplyUsageStoppingBeforeDonePreservesQueue(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "release-usage")
	t.Setenv("OMP_TELEGRAM_FIXTURE_USAGE_RELEASE", gate)
	w, _, trace := replyUsageWorker(t, "hold")
	t.Cleanup(func() { requireStoreOK(t, os.WriteFile(gate, nil, 0600)) })
	client, binding, claim := w.client, w.binding, w.claimedSession
	endUsageReply(w)
	w.operationReturned(receiveReplyStatus(t, w))
	query := waitFixtureRPCTrace(t, trace, "usage_waiting", "")
	// Cancel only the optional query while the fixture holds process cleanup open.
	w.replyStatus.pending.cancel()
	result := waitOperation(t, w)
	if result.kind != "reply_usage" || result.err == nil || !client.Stopping() {
		t.Fatal("optional usage did not reach its terminal transport failure")
	}
	select {
	case <-client.Done():
		t.Fatal("fixture failed to hold the result-before-Done interleaving")
	default:
	}
	nativeInput(t, w, 100, "/followup first queued task", false)
	nativeInput(t, w, 101, "/followup second queued task", false)
	replacement := w.client
	w.operationReturned(result)
	if replacement == nil || replacement == client || w.client != replacement || len(w.b.slots) != 1 || w.rpcExitPendingClientID != 0 {
		t.Fatal("late usage failure retained its stopped runtime or retired the replacement")
	}
	if !sameBindingIdentity(w.binding, binding) || w.claimedSession != claim {
		t.Fatal("optional query retirement changed the session binding or claim")
	}
	finalReplyStatus(t, w, 2, "answer")
	if queueState(t, w, 2) != "done" || queueState(t, w, 101) != "pending" {
		t.Fatal("optional query failure lost the answer or queued work")
	}
	w.dispatch()
	accepted := waitOperation(t, w)
	requireStoreOK(t, accepted.err)
	w.operationReturned(accepted)
	prompt := waitFixtureRPCTrace(t, trace, "prompt", "first queued task")
	if prompt.pid == query.pid || researchTraceCount(trace, "prompt", "first queued task") != 1 ||
		researchTraceCount(trace, "prompt", "missing-terminal") != 1 || queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "pending" {
		t.Fatal("queued task reused the failed runtime, replayed work, or canceled its followup")
	}
}

func TestFinalReplyUsageInterruptionFencesLateResult(t *testing.T) {
	for _, input := range []string{"/stop", "/close", "missing-terminal"} {
		t.Run(input, func(t *testing.T) {
			w, command, trace := replyUsageWorker(t, "hold")
			endUsageReply(w)
			w.operationReturned(receiveReplyStatus(t, w))
			waitFixtureRPCTrace(t, trace, "usage_waiting", "")
			command(input)
			if queueState(t, w, 2) != "done" {
				t.Fatal("control discarded confirmed reply awaiting quota")
			}
			late := waitOperation(t, w)
			if late.kind != "reply_usage" {
				t.Fatalf("expected interrupted quota result, got %s", late.kind)
			}
			before := w.active
			w.operationReturned(late)
			if w.active != before || w.replyStatus.header != "" {
				t.Fatal("late quota result changed replacement task")
			}
			metadata := finalReplyStatus(t, w, 2, "answer")
			if strings.Contains(metadata, "5h ") {
				t.Fatalf("interrupted quota contaminated old answer: %q", metadata)
			}
		})
	}
}

func TestFinalReplyUsageCompactionEventFollowsClientLifetime(t *testing.T) {
	for _, scenario := range []struct {
		name string
		sent bool
	}{
		{"retired", true},
		{"retained", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, _, trace := replyUsageWorker(t, "hold")
			client, binding := w.client, w.binding
			endUsageReply(w)
			sample := receiveReplyStatus(t, w)
			if !scenario.sent {
				nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "get_state"})
			}
			w.operationReturned(sample)
			if scenario.sent {
				waitFixtureRPCTrace(t, trace, "usage_waiting", "")
			} else {
				waitFixtureRPCTrace(t, trace, "research_control_waiting", "get_state")
			}
			w.event([]byte(`{"type":"auto_compaction_start"}`))
			if scenario.sent {
				if w.client != nil || w.compacting || len(w.b.slots) != 0 {
					t.Fatal("retired client's compaction event changed worker state")
				}
			} else if w.client != client || client.Stopping() || !w.compacting {
				t.Fatal("retained client's compaction event was lost")
			}
			result := waitOperation(t, w)
			if result.kind != "reply_usage" {
				t.Fatalf("expected interrupted usage result, got %s", result.kind)
			}
			w.operationReturned(result)
			if !scenario.sent {
				nativeRPC(t, w, "fixture_research_control_release", nil)
			}
			nativeInput(t, w, 100, "after native activity", false)
			if !scenario.sent {
				if queueState(t, w, 100) != "pending" || researchTraceCount(trace, "prompt", "after native activity") != 0 {
					t.Fatal("queued prompt bypassed live native compaction")
				}
				w.event([]byte(`{"type":"auto_compaction_end"}`))
				w.dispatch()
			}
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			if (w.client == client) == scenario.sent || !sameBindingIdentity(binding, w.binding) ||
				researchTraceCount(trace, "prompt", "after native activity") != 1 {
				t.Fatal("native activity changed session identity, replayed work, or blocked dispatch")
			}
			for id, want := range map[int64]string{2: "answer", 100: "answer: after native activity"} {
				if queueState(t, w, id) != "done" || researchBody(t, w, id) != want {
					t.Fatalf("native activity lost or contaminated the answer for task %d", id)
				}
			}
		})
	}
}

func TestFinalReplyUsagePreservesOtherNativeOutput(t *testing.T) {
	w, _, trace := replyUsageWorker(t, "hold")
	endUsageReply(w)
	w.operationReturned(receiveReplyStatus(t, w))
	waitFixtureRPCTrace(t, trace, "usage_waiting", "")
	w.event([]byte(`{"type":"command_output","text":"other command receipt"}`))
	if w.replyStatus.pending == nil || w.client == nil {
		t.Fatal("unrelated native output interrupted private quota query")
	}
	var receipts int
	err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE text=? AND inbox_id=0", "other command receipt").Scan(&receipts)
	requireStoreOK(t, err)
	if receipts != 1 {
		t.Fatalf("unrelated native output was lost or assigned to the answer: %d receipts", receipts)
	}
	w.flushReplyStatus()
	w.operationReturned(waitOperation(t, w))
}

func TestFinalReplyUsageRuntimeReleaseOwnsOneSlot(t *testing.T) {
	w, _, trace := replyUsageWorker(t, "hold")
	endUsageReply(w)
	w.operationReturned(receiveReplyStatus(t, w))
	waitFixtureRPCTrace(t, trace, "usage_waiting", "")
	// Keep a peer's occupied slot so a double release fails without hanging the suite.
	w.b.slots = make(chan struct{}, 2)
	w.b.slots <- struct{}{}
	w.b.slots <- struct{}{}
	w.releaseRuntimeWithReason(true, "failure")
	if w.client != nil || len(w.b.slots) != 1 {
		t.Fatal("usage teardown released another runtime's slot")
	}
	w.operationReturned(waitOperation(t, w))
	finalReplyStatus(t, w, 2, "answer")
	if queueState(t, w, 2) != "done" {
		t.Fatal("runtime teardown lost the confirmed answer")
	}
}

func TestFinalReplyUsageIgnoresCanceledSteerProbe(t *testing.T) {
	w, _, trace := replyUsageWorker(t, "hold")
	nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "get_state"})
	w.steerFence.active = true
	endUsageReply(w)
	waitFixtureRPCTrace(t, trace, "research_control_waiting", "get_state")
	w.lastActivity = time.Now().Add(-stuckTaskQuietPeriod - time.Second)
	w.probeStuckTask(time.Now())
	select {
	case result := <-w.idleProbe.results:
		w.idleProbeFinished(result, time.Now())
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not confirm the settled root")
	}
	var canceled steerProbeResult
	select {
	case canceled = <-w.steerFence.probeResults:
	case <-time.After(2 * time.Second):
		t.Fatal("final reply did not cancel the other settlement probe")
	}
	if !errors.Is(canceled.err, context.Canceled) {
		t.Fatalf("expected canceled steer probe, got %v", canceled.err)
	}
	w.operationReturned(receiveReplyStatus(t, w))
	waitFixtureRPCTrace(t, trace, "usage_waiting", "")
	client, pending := w.client, w.replyStatus.pending
	w.steeredQuiescenceFinished(canceled)
	if pending == nil || !pending.usage || w.replyStatus.pending != pending || w.client != client || len(w.b.slots) != 1 || queueState(t, w, 2) != "submitted" {
		t.Fatal("late settlement result retired the runtime or committed the reply awaiting usage")
	}
	w.flushReplyStatus()
	w.operationReturned(waitOperation(t, w))
	if queueState(t, w, 2) != "done" || len(w.b.slots) != 0 {
		t.Fatal("interrupted final usage did not commit and release exactly once")
	}
}
