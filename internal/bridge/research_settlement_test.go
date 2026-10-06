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
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"pending": false})
	nativeRPC(t, w, "fixture_autoresearch_settled", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
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
			if !steer {
				researchAdmitNewRound(t, w)
			} else {
				// Steering admission must fence independently of agent_start.
				nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"mode": "on", "streaming": true})
			}
			if steer {
				nativeInput(t, w, 101, "instruction for the new round", false)
				if queueState(t, w, 101) != "submitted" || len(w.steers) != 1 {
					t.Fatal("steer was not durably admitted before the old result")
				}
			}
			w.operationReturned(result)
			if queueState(t, w, 100) != "submitted" || w.active != 100 {
				t.Fatal("old off snapshot settled newly admitted research work")
			}
			if steer {
				nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" && !w.rpcOperationActive })
				nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"streaming": false})
				researchAdmitNewRound(t, w)
			}
			researchFinishNaturally(t, w, "new round final result")
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			if len(w.steers) != 0 || w.steerFence.active {
				t.Fatal("natural completion lost steer ownership or retained its fence")
			}
		})
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
		case <-timer.C:
			t.Fatal("settlement was not queued behind the root callback")
		}
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
