package bridge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func researchFinishNaturally(t *testing.T, w *worker, text string) {
	t.Helper()
	nativeRPC(t, w, "fixture_autoresearch_round", map[string]any{"text": text, "mode": "off"})
}

func TestAutoresearchNaturalOffCompletesRootAndReleasesQueuedWork(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	researchRoot(t, w, 100, "/autoresearch finite goal")
	nativeInput(t, w, 101, "/followup work after natural off", false)
	researchRound(t, w, "intermediate result")
	roundHeader, _ := attachedReplyBody(t, researchReplies(t, w, 100)[0])
	if !strings.HasPrefix(roundHeader, "💬") {
		t.Fatal("an intermediate research round was marked as task completion")
	}
	if queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "pending" {
		t.Fatal("mode-on settlement ended research or released its queue")
	}
	nativeRPC(t, w, "fixture_autoresearch_continue", nil)
	nativeUntil(t, w, func() bool { return w.research.roundOpen })
	researchFinishNaturally(t, w, "final finite result")
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	if queueState(t, w, 100) != "done" || w.taskActive() || w.research.enabled || w.resumeFailed {
		t.Fatal("natural off did not complete the research root normally")
	}
	replies := strings.Join(researchReplies(t, w, 100), "\n")
	for _, text := range []string{"intermediate result", "final finite result"} {
		if strings.Count(replies, text) != 1 {
			t.Fatalf("natural completion lost or duplicated round output: %q", replies)
		}
	}
	rows := researchReplies(t, w, 100)
	firstHeader, _ := attachedReplyBody(t, rows[0])
	lastHeader, _ := attachedReplyBody(t, rows[1])
	if !strings.HasPrefix(firstHeader, "💬") || !strings.HasPrefix(lastHeader, "✅") {
		t.Fatal("natural research completion did not mark only the last formal reply")
	}
	if researchTraceCount(trace, "prompt", "/autoresearch finite goal") != 1 || researchTraceCount(trace, "prompt", "work after natural off") != 1 {
		t.Fatal("natural completion replayed research or omitted queued work")
	}
}

func TestAutoresearchSettlementModeErrorPausesWithoutReplay(t *testing.T) {
	for _, mode := range []string{"error", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			researchRoot(t, w, 100, "/autoresearch uncertain settlement")
			nativeInput(t, w, 101, "/followup waiting work", false)
			nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": mode})
			researchFinishNaturally(t, w, "durable result before uncertainty")
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "uncertain" })
			if !w.resumeFailed || !w.research.disablePending || w.client != nil || queueState(t, w, 101) != "pending" {
				t.Fatal("unconfirmed mode did not retire research and pause its queue")
			}
			if got := strings.Join(researchReplies(t, w, 100), "\n"); strings.Count(got, "durable result before uncertainty") != 1 {
				t.Fatal("uncertain settlement lost or duplicated already durable output")
			}
			if researchTraceCount(trace, "prompt", "/autoresearch uncertain settlement") != 1 || researchTraceCount(trace, "prompt", "waiting work") != 0 || strings.Contains(nativeOutputs(t, w), "PRIVATE_AUTORESEARCH") {
				t.Fatal("uncertain settlement replayed input or leaked RPC diagnostics")
			}
		})
	}
}

func TestAutoresearchOffWithPendingWorkRetainsRootUntilSettled(t *testing.T) {
	w, _, _ := researchWorker(t, "autoresearch")
	researchRoot(t, w, 100, "/autoresearch pending native work")
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"pending": true})
	researchFinishNaturally(t, w, "pending result")
	researchBarrier(t, w)
	if queueState(t, w, 100) != "submitted" || !w.taskActive() || w.resumeFailed {
		t.Fatal("off mode alone completed a root with pending native work")
	}
	header, _ := attachedReplyBody(t, researchReplies(t, w, 100)[0])
	if !strings.HasPrefix(header, "💬") {
		t.Fatal("pending native work was marked successful")
	}
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"pending": false})
	nativeRPC(t, w, "fixture_autoresearch_settled", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	header, _ = attachedReplyBody(t, researchReplies(t, w, 100)[0])
	if !strings.HasPrefix(header, "✅") {
		t.Fatal("confirmed research settlement did not update its last reply")
	}
}

func TestAutoresearchIncompleteSettlementStateIsUncertain(t *testing.T) {
	w, _, _ := researchWorker(t, "autoresearch")
	researchRoot(t, w, 100, "/autoresearch incomplete native state")
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"incomplete": true})
	researchFinishNaturally(t, w, "confirmed round only")
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "uncertain" })
	if !w.resumeFailed || !w.research.disablePending {
		t.Fatal("missing pending-work evidence was treated as confirmed completion")
	}
}

// Capture real off/idle evidence without dispatching it through the actor yet.
func researchHeldOffResult(t *testing.T, w *worker, trace string) operationResult {
	t.Helper()
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
	researchFinishNaturally(t, w, "completed old round")
	nativeUntil(t, w, func() bool { return w.research.settlement.probe != nil && w.rpcOperationActive })
	waitFixtureRPCTrace(t, trace, "entries_waiting", "")
	nativeRPC(t, w, "fixture_autoresearch_release_entries", nil)
	result := waitOperation(t, w)
	var state researchSettlementState
	if result.kind != "research_settlement" || result.err != nil || json.Unmarshal(result.data, &state) != nil || state.Enabled || !state.Idle || !state.Settled || state.Pending {
		t.Fatal("fixture did not capture confirmed old off/idle evidence")
	}
	return result
}

func researchAdmitNewRound(t *testing.T, w *worker) {
	t.Helper()
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"mode": "on"})
	nativeRPC(t, w, "fixture_autoresearch_continue", nil)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for !w.research.roundOpen {
		select {
		case raw := <-w.client.Events():
			w.event(raw)
		case <-timer.C:
			t.Fatal("new research round was not admitted")
		}
	}
}

func TestAutoresearchOldOffCannotFinishNewRoundOrSteer(t *testing.T) {
	for _, steer := range []bool{false, true} {
		name := "new-round"
		if steer {
			name = "steer-admission"
		}
		t.Run(name, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			researchRoot(t, w, 100, "/autoresearch fenced rounds")
			result := researchHeldOffResult(t, w, trace)
			client, rootRequest, turn, generation := w.client, w.rootRequestID, w.turn, w.binding.Generation
			epoch, admission := client.CommandCatalog().Epoch, w.steerFence.admission
			if !steer {
				researchAdmitNewRound(t, w)
			} else {
				// Pending guidance must fence independently of agent_start or submission.
				nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"mode": "on", "streaming": true})
			}
			if steer {
				nativeInput(t, w, 101, "instruction for the new round", false)
				if queueState(t, w, 101) != "pending" || len(w.deferredSteers) != 1 || len(w.steers) != 0 ||
					!w.rpcOperationActive || w.steerFence.admission != admission || researchTraceCount(trace, "prompt", "instruction for the new round") != 0 {
					t.Fatal("guidance bypassed the held settlement RPC lane or was treated as submitted")
				}
				pending := w.deferredSteers[0]
				if pending.owner.rootID != 100 || pending.owner.clientID != client.ID() || pending.owner.turn != turn ||
					pending.owner.generation != generation || pending.epoch != epoch || pending.session != w.sessionID {
					t.Fatal("pending guidance lost its original research owner")
				}
			}
			w.operationReturned(result)
			if queueState(t, w, 100) != "submitted" || w.active != 100 {
				t.Fatal("old off snapshot settled newly admitted research work")
			}
			if steer {
				if queueState(t, w, 101) != "pending" || len(w.deferredSteers) != 1 || w.rpcOperationActive || w.research.settlement.probe != nil {
					t.Fatal("old settlement result discarded pending guidance or retained its RPC lane")
				}
				w.probeResearchSettlement()
				if w.research.settlement.probe != nil {
					t.Fatal("pending guidance allowed another settlement probe before dispatch")
				}
				w.dispatch()
				nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" && !w.rpcOperationActive })
				if queueState(t, w, 100) != "submitted" || w.active != 100 || w.client != client || w.rootRequestID != rootRequest ||
					w.turn != turn || w.binding.Generation != generation || client.CommandCatalog().Epoch != epoch ||
					len(w.deferredSteers) != 0 || len(w.steers) != 0 || w.steerFence.admission != admission+1 ||
					researchTraceCount(trace, "prompt", "instruction for the new round") != 1 || w.resumeFailed {
					t.Fatal("guidance did not finish exactly once through native ACK/result in its original research root")
				}
				nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"streaming": false})
				researchAdmitNewRound(t, w)
			}
			researchFinishNaturally(t, w, "new round final result")
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			if len(w.steers) != 0 || w.steerFence.active {
				t.Fatal("natural completion lost steer ownership or retained its fence")
			}
			if steer && (researchTraceCount(trace, "prompt", "/autoresearch fenced rounds") != 1 || researchTraceCount(trace, "prompt", "instruction for the new round") != 1) {
				t.Fatal("natural settlement replayed the research root or its guidance")
			}
		})
	}
}

func TestAutoresearchStaleDeferredGuidanceRetiresUnconfirmedRoot(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	researchRoot(t, w, 100, "/autoresearch obsolete native scope")
	result := researchHeldOffResult(t, w, trace)
	client, session, epoch := w.client, w.sessionID, w.client.CommandCatalog().Epoch
	nativeInput(t, w, 101, "obsolete research guidance", false)
	if queueState(t, w, 101) != "pending" || len(w.deferredSteers) != 1 || len(w.steers) != 0 {
		t.Fatal("guidance did not remain pending behind the held settlement result")
	}
	// Change the reader's scope without delivering the session event to the
	// actor: pending guidance still fences the already completed old probe.
	nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
		"type": "session_info_update", "sessionId": "replacement-research-session",
	}})
	if client.CommandCatalog().SessionID != "replacement-research-session" || client.CommandCatalog().Epoch == epoch || w.sessionID != session {
		t.Fatal("fixture did not establish a reader-ahead replacement session")
	}
	w.operationReturned(result)
	if queueState(t, w, 100) != "submitted" || w.research.settlement.probe != nil || w.rpcOperationActive {
		t.Fatal("old settlement evidence completed the root or retained its RPC lane")
	}
	// Deliver the queued session event and let ordinary dispatch cancel the
	// stale guidance. Its missing native ACK/result must not strand the root.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for sessionUpdated := false; !sessionUpdated; {
		select {
		case raw, open := <-client.Events():
			if !open {
				t.Fatal("native events closed before the replacement session")
			}
			var event struct{ Type string }
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			w.event(raw)
			sessionUpdated = event.Type == "session_info_update"
		case <-timer.C:
			t.Fatal("replacement session event was not delivered")
		}
	}
	w.dispatch()
	// A catalog refresh started by the replacement event may finish after
	// retirement; delivering it must not revive settlement in the new session.
	select {
	case catalog := <-w.nativeCatalog.results:
		w.commandCatalogFinished(catalog)
	case <-timer.C:
		t.Fatal("replacement catalog lookup did not return")
	}
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) != "submitted" && w.nativeCatalog.cancel == nil })
	if queueState(t, w, 100) != "uncertain" || queueState(t, w, 101) != "cancelled" ||
		!w.resumeFailed || !w.research.disablePending || w.taskActive() || w.client != nil ||
		len(w.deferredSteers) != 0 || len(w.steers) != 0 || w.research.settlement.probe != nil {
		t.Fatal("lost native research scope was falsely completed or left without a terminal outcome")
	}
	w.operationFinished(result)
	w.dispatch()
	if queueState(t, w, 100) != "uncertain" || researchTraceCount(trace, "prompt", "/autoresearch obsolete native scope") != 1 ||
		researchTraceCount(trace, "prompt", "obsolete research guidance") != 0 ||
		strings.Count(strings.Join(researchReplies(t, w, 100), "\n"), "completed old round") != 1 {
		t.Fatal("late settlement replayed stale guidance, changed the retired root, or lost its durable round")
	}
}

func TestAutoresearchQueuedRootCallbackCannotFinishFollowingWork(t *testing.T) {
	w, _, _ := researchWorker(t, "autoresearch")
	nativeInput(t, w, 100, "/autoresearch queued root callback", false)
	nativeUntil(t, w, func() bool { return w.active == 100 })
	rootCallback := waitOperation(t, w)
	// Deliver agent_start and the natural end while the submission callback is
	// still pending. The settlement RPC must wait in the existing RPC lane.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for !w.research.roundOpen {
		select {
		case raw := <-w.client.Events():
			w.event(raw)
		case <-timer.C:
			t.Fatal("research root did not start before its callback")
		}
	}
	nativeInput(t, w, 101, "/followup missing-terminal", false)
	researchFinishNaturally(t, w, "delayed callback round")
	for w.research.settlement.probe == nil {
		select {
		case raw := <-w.client.Events():
			w.event(raw)
		case result := <-w.operations:
			if result.kind != "reply_status" {
				t.Fatalf("unexpected operation before held root callback: %s", result.kind)
			}
			w.operationReturned(result)
			if !w.rpcOperationActive || queueState(t, w, 100) != "submitted" || w.active != 100 || queueState(t, w, 101) != "pending" {
				t.Fatal("round metadata released the held mutation lane or its research owner")
			}
		case <-timer.C:
			t.Fatal("settlement was not queued behind the root callback")
		}
	}
	if !w.rpcOperationActive || len(w.rpcOperations) != 1 {
		t.Fatal("settlement bypassed the held root callback's mutation lane")
	}
	w.operationReturned(rootCallback)
	nativeUntil(t, w, func() bool { return w.active == 101 && !w.rpcOperationActive })
	w.operationFinished(rootCallback)
	if queueState(t, w, 100) != "done" || queueState(t, w, 101) != "submitted" || w.active != 101 || w.resumeFailed {
		t.Fatal("stale root callback changed naturally completed ownership")
	}
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
}

func TestAutoresearchSettlementProbeDoesNotBlockStopOrClose(t *testing.T) {
	for _, control := range []string{"stop", "close"} {
		t.Run(control, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			researchRoot(t, w, 100, "/autoresearch interruptible settlement")
			oldClient := w.client
			nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
			researchFinishNaturally(t, w, "round before stop")
			nativeUntil(t, w, func() bool { return w.research.settlement.probe != nil && w.rpcOperationActive })
			waitFixtureRPCTrace(t, trace, "entries_waiting", "")
			nativeInput(t, w, 101, "/"+control, false)
			select {
			case <-oldClient.Done():
			case <-time.After(time.Second):
				t.Fatal("control waited for an asynchronous settlement probe")
			}
			if queueState(t, w, 100) != "uncertain" {
				t.Fatal("interrupted settlement retained its old submitted owner")
			}
			w.operationReturned(waitOperation(t, w))
			if queueState(t, w, 100) != "uncertain" {
				t.Fatal("stale settlement result rewrote an interrupted root")
			}
		})
	}
}

func TestAutoresearchNewSettlementSupersedesOldOnSnapshot(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	researchRoot(t, w, 100, "/autoresearch late native off")
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
	nativeRPC(t, w, "fixture_autoresearch_round", map[string]any{"text": "last completed round"})
	nativeUntil(t, w, func() bool { return w.research.settlement.probe != nil && w.rpcOperationActive })
	waitFixtureRPCTrace(t, trace, "entries_waiting", "")
	nativeRPC(t, w, "fixture_autoresearch_release_entries", nil)
	oldOn := waitOperation(t, w)
	var state researchSettlementState
	if oldOn.err != nil || json.Unmarshal(oldOn.data, &state) != nil || !state.Enabled {
		t.Fatal("fixture did not capture the old enabled snapshot")
	}
	revision := w.research.settlement.revision
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"mode": "off"})
	nativeRPC(t, w, "fixture_autoresearch_settled", nil)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for w.research.settlement.revision == revision {
		select {
		case raw := <-w.client.Events():
			w.event(raw)
		case <-timer.C:
			t.Fatal("newer settlement notification was not admitted")
		}
	}
	w.operationReturned(oldOn)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	if got := strings.Join(researchReplies(t, w, 100), "\n"); strings.Count(got, "last completed round") != 1 {
		t.Fatal("superseded on lookup lost or duplicated the round output")
	}
}
