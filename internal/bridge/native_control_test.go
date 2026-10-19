package bridge

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func assertFastState(t *testing.T, w *worker, enabled, active bool, runs int) {
	t.Helper()
	raw, err := w.call("get_state", nil)
	var state struct {
		Enabled bool `json:"fastModeEnabled"`
		Active  bool `json:"fastModeActive"`
		Runs    int  `json:"fixtureRootPrompts"`
	}
	if err != nil || json.Unmarshal(raw, &state) != nil || state.Enabled != enabled || state.Active != active || state.Runs != runs {
		t.Fatalf("fast state=%+v, want enabled=%t active=%t runs=%d, err=%v", state, enabled, active, runs, err)
	}
}

func fastRoot(t *testing.T, w *worker) {
	t.Helper()
	nativeInput(t, w, 100, "wait", false)
	researchBarrier(t, w)
	if w.active != 100 || !w.busy || w.rootRequestID == "" || queueState(t, w, 100) != "submitted" {
		t.Fatal("fixture did not retain an active root")
	}
}

func TestFastNativeIdleCommandsNeverRunModelOrOpenPicker(t *testing.T) {
	w, _, trace := nativeWorker(t, "fast")
	nativeCatalogReady(t, w)
	binding, client := w.binding, w.client
	for i, tc := range []struct {
		command, output string
		enabled         bool
	}{
		{command: "/fast", output: "Fast mode: on", enabled: true},
		{command: "/fast", output: "Fast mode: off"},
		{command: "/fast on", output: "Fast mode: on", enabled: true},
		{command: "/fast status", output: "Fast mode: on", enabled: true},
		{command: "/fast off", output: "Fast mode: off"},
		{command: "/fast ultra", output: "Fast mode: on (ultra)", enabled: true},
		{command: "/fast off", output: "Fast mode: off"},
	} {
		id := int64(100 + i)
		nativeInput(t, w, id, tc.command, false)
		nativeUntil(t, w, func() bool { return queueState(t, w, id) == "done" && !w.controlInProgress() })
		researchBarrier(t, w)
		assertFastState(t, w, tc.enabled, tc.enabled, 0)
		if w.client != client || !sameBindingIdentity(w.binding, binding) || w.taskActive() || len(w.queue) != 0 || len(w.confirms) != 0 {
			t.Fatalf("%s changed the session, started a task, queued work, or opened a picker", tc.command)
		}
		if !strings.Contains(nativeOutputs(t, w), tc.output) {
			t.Fatalf("%s lost native output %q", tc.command, tc.output)
		}
	}
	if researchTraceCount(trace, "prompt", "/fast") != 2 || researchTraceCount(trace, "set_fast_mode", "") != 0 {
		t.Fatal("fast did not use the native prompt toggle exclusively")
	}
	var notices int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=0 AND text LIKE 'Fast mode:%'").Scan(&notices))
	if notices != 7 {
		t.Fatalf("native fast produced %d standalone outputs, want 7", notices)
	}
}

func TestFastNativeDuringRootPreservesOwnershipQueueAndFinalReply(t *testing.T) {
	w, _, trace := nativeWorker(t, "fast")
	nativeCatalogReady(t, w)
	fastRoot(t, w)
	nativeInput(t, w, 101, "/followup queued after root", false)
	client, binding, root, turn, owner := w.client, w.binding, w.rootRequestID, w.turn, w.owner
	if len(w.queue) != 1 || queueState(t, w, 101) != "pending" {
		t.Fatal("fixture did not queue the followup")
	}
	for i, text := range []string{"/fast", "/fast status", "/fast off", "/fast@fixture_bot ultra"} {
		id := int64(102 + i)
		nativeInput(t, w, id, text, true)
		nativeUntil(t, w, func() bool { return queueState(t, w, id) == "done" && !w.controlInProgress() })
		researchBarrier(t, w)
		if w.client != client || !sameBindingIdentity(w.binding, binding) || w.rootRequestID != root || w.turn != turn || w.owner != owner || w.active != 100 || !w.busy || len(w.queue) != 1 || len(w.steers) != 0 || queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "pending" {
			t.Fatalf("%s changed root ownership, turn, or deferred followup", text)
		}
	}
	assertFastState(t, w, true, true, 1)
	if researchTraceCount(trace, "prompt", "/fast ultra") != 1 || researchTraceCount(trace, "prompt", "queued after root") != 0 {
		t.Fatal("fast included reply context or dispatched the queued followup")
	}
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" && queueState(t, w, 101) == "done" })
	rootReply := strings.Join(researchReplies(t, w, 100), "\n")
	if replyBody(rootReply) != "root completed" || strings.Contains(rootReply, "Fast mode:") || strings.Contains(rootReply, "late native output") {
		t.Fatalf("native output polluted root final reply: %q", rootReply)
	}
	if researchTraceCount(trace, "prompt", "wait") != 1 || researchTraceCount(trace, "prompt", "queued after root") != 1 {
		t.Fatal("independent control replayed or lost ordinary work")
	}
}

func TestFastNativeArgumentsAndUnsupportedModelUseNativeOutput(t *testing.T) {
	w, _, trace := nativeWorker(t, "fast")
	nativeCatalogReady(t, w)
	nativeRPC(t, w, "set_model", map[string]any{"provider": "fixture", "modelId": "fast-fallback"})
	for i, text := range []string{"/fast on", "/fast invalid", "/fast on extra", "/fast status"} {
		id := int64(100 + i)
		nativeInput(t, w, id, text, false)
		nativeUntil(t, w, func() bool { return queueState(t, w, id) == "done" && !w.controlInProgress() })
		researchBarrier(t, w)
		assertFastState(t, w, true, false, 0)
		if researchTraceCount(trace, "prompt", text) != 1 {
			t.Fatalf("%q was not interpreted by the native command", text)
		}
	}
	var usage int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=0 AND text=?", "Usage: /fast [on|off|status|ultra]").Scan(&usage))
	if usage != 2 || w.taskActive() || len(w.confirms) != 0 {
		t.Fatal("invalid native arguments started model work, opened a menu, or lost usage output")
	}
}

func TestFastNativeRequiresBuiltinCatalogProof(t *testing.T) {
	for _, mode := range []string{"fast-file", "fast-extension", "fast-missing", "unsupported", "fail"} {
		t.Run(mode, func(t *testing.T) {
			w, _, trace := nativeWorker(t, mode)
			nativeUntil(t, w, func() bool { return w.nativeCatalog.cancel == nil })
			nativeInput(t, w, 100, "/fast on", false)
			nativeUntil(t, w, func() bool {
				return queueState(t, w, 100) != "pending" && queueState(t, w, 100) != "submitted" && !w.controlInProgress()
			})
			if researchTraceCount(trace, "prompt", "/fast on") != 0 || w.taskActive() || len(w.queue) != 0 {
				t.Fatal("untrusted or unavailable native command was sent to the model")
			}
			assertFastState(t, w, false, false, 0)
			if !strings.Contains(strings.ToLower(nativeOutputs(t, w)), "unavailable") {
				t.Fatal("missing builtin proof was not reported to the operator")
			}
		})
	}
}

func TestFastNativeExplicitRejectionKeepsActiveRoot(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_FAST_MODE", "reject")
	w, _, trace := nativeWorker(t, "fast")
	nativeCatalogReady(t, w)
	fastRoot(t, w)
	nativeInput(t, w, 101, "/followup after rejected control", false)
	client, root, turn, owner := w.client, w.rootRequestID, w.turn, w.owner
	nativeInput(t, w, 102, "/fast on", false)
	nativeUntil(t, w, func() bool {
		return queueState(t, w, 102) != "submitted" && queueState(t, w, 102) != "pending" && !w.controlInProgress()
	})
	if w.client != client || w.rootRequestID != root || w.turn != turn || w.owner != owner || w.active != 100 || !w.busy || queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "pending" {
		t.Fatal("explicit fast rejection retired the unrelated active root or dispatched its followup")
	}
	assertFastState(t, w, false, false, 1)
	if strings.Contains(nativeOutputs(t, w), "PRIVATE_FAST_REJECTION") || researchTraceCount(trace, "prompt", "/fast on") != 1 {
		t.Fatal("native rejection leaked private details or retried the control")
	}
	nativeRPC(t, w, "fixture_fast_mode", map[string]any{"mode": ""})
	nativeInput(t, w, 103, "/fast", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 103) == "done" && !w.controlInProgress() })
	assertFastState(t, w, true, true, 1)
	if w.client != client || w.rootRequestID != root || w.active != 100 || queueState(t, w, 101) != "pending" {
		t.Fatal("retry after explicit rejection lost the original root or retained a failed control fence")
	}
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	if queueState(t, w, 100) != "done" {
		t.Fatal("rejected control prevented normal root completion")
	}
}

func TestFastNativeUnconfirmedAcknowledgmentNeverReplays(t *testing.T) {
	for _, mode := range []string{"missing-agent-invoked", "agent-invoked", "malformed-agent-invoked"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_FAST_MODE", mode)
			w, command, trace := nativeWorker(t, "fast")
			nativeCatalogReady(t, w)
			fastRoot(t, w)
			nativeInput(t, w, 101, "/fast", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "uncertain" && !w.controlInProgress() })
			if researchTraceCount(trace, "prompt", "/fast") != 1 || queueState(t, w, 100) == "done" {
				t.Fatal("unconfirmed control was retried or falsely completed the active root")
			}
			if !strings.Contains(strings.ToLower(nativeOutputs(t, w)), "could not be confirmed") {
				t.Fatal("unconfirmed fast acknowledgment was reported as successful")
			}
			t.Setenv("OMP_TELEGRAM_FIXTURE_FAST_MODE", "")
			command("/close")
			command("/new " + t.TempDir())
			nativeCatalogReady(t, w)
			nativeInput(t, w, 102, "new root after uncertain fast", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
			if researchTraceCount(trace, "prompt", "/fast") != 1 || researchTraceCount(trace, "prompt", "wait") != 1 {
				t.Fatal("new runtime replayed an uncertain control or its interrupted root")
			}
		})
	}
}

func TestFastNativeRootFinishesBeforeControlAcknowledgment(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_FAST_MODE", "hold")
	w, _, trace := nativeWorker(t, "fast")
	nativeCatalogReady(t, w)
	fastRoot(t, w)
	nativeInput(t, w, 101, "/followup wait", false)
	nativeInput(t, w, 102, "/fast on", false)
	waitFixtureRPCTrace(t, trace, "fast_waiting", "/fast on")
	nativeRPC(t, w, "fixture_finish_root", nil)
	researchEventBarrier(t, w)
	if queueState(t, w, 102) != "submitted" || queueState(t, w, 101) != "pending" || len(w.queue) != 1 || researchTraceCount(trace, "prompt", "wait") != 1 || queueState(t, w, 100) == "uncertain" {
		t.Fatal("root completion settled the fast control or dispatched the next root before its acknowledgment")
	}
	nativeRPC(t, w, "fixture_fast_release", nil)
	nativeUntil(t, w, func() bool {
		return queueState(t, w, 100) == "done" && queueState(t, w, 102) == "done" && w.active == 101 && !w.rpcOperationActive
	})
	if queueState(t, w, 101) != "submitted" || researchTraceCount(trace, "prompt", "wait") != 2 {
		t.Fatal("control acknowledgment consumed or lost the queued root")
	}
	if replyBody(strings.Join(researchReplies(t, w, 100), "\n")) != "root completed" {
		t.Fatal("fast acknowledgment replaced the completed root's final answer")
	}
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
}

func TestFastNativeLateAcknowledgmentCannotAffectReplacement(t *testing.T) {
	for _, action := range []string{"/stop", "/close", "runtime-replacement"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_FAST_MODE", "hold")
			w, command, trace := nativeWorker(t, "fast")
			nativeCatalogReady(t, w)
			fastRoot(t, w)
			oldClient, oldBinding := w.client, w.binding
			nativeInput(t, w, 101, "/fast", false)
			waitFixtureRPCTrace(t, trace, "fast_waiting", "/fast")
			researchEventBarrier(t, w)
			nativeRPC(t, w, "fixture_fast_release", nil)
			late := waitOperation(t, w)
			if late.kind != "native_control" || late.err != nil {
				t.Fatalf("fixture did not capture a successful native control callback: %+v", late)
			}
			if action == "runtime-replacement" {
				w.retireTaskRuntime("fixture runtime replacement")
			} else {
				nativeInput(t, w, 102, action, false)
			}
			if w.client != nil || queueState(t, w, 101) != "uncertain" {
				t.Fatal("interruption retained the old runtime or claimed the pending fast result")
			}
			// Release the old RPC lane before starting the replacement task.
			w.operationReturned(late)
			t.Setenv("OMP_TELEGRAM_FIXTURE_FAST_MODE", "")
			if action == "runtime-replacement" {
				_, err := w.ensureRuntime()
				requireStoreOK(t, err)
			} else {
				if action == "/stop" {
					command("/close")
				}
				command("/new " + t.TempDir())
			}
			nativeCatalogReady(t, w)
			fastRootID := int64(103)
			nativeInput(t, w, fastRootID, "wait", false)
			researchBarrier(t, w)
			client, root, turn, owner, binding := w.client, w.rootRequestID, w.turn, w.owner, w.binding
			if client == oldClient {
				t.Fatal("fixture did not replace the native client")
			}
			if action == "runtime-replacement" {
				if !sameBindingIdentity(binding, oldBinding) {
					t.Fatal("runtime replacement changed the logical session")
				}
			} else if binding.Generation <= oldBinding.Generation {
				t.Fatal("new session did not replace the binding generation")
			}
			before := nativeOutputs(t, w)
			w.operationReturned(late)
			w.operationReturned(late)
			if w.client != client || !sameBindingIdentity(w.binding, binding) || w.rootRequestID != root || w.turn != turn || w.owner != owner || w.active != fastRootID || queueState(t, w, fastRootID) != "submitted" || queueState(t, w, 101) != "uncertain" || nativeOutputs(t, w) != before {
				t.Fatal("late or duplicate old fast acknowledgment changed the replacement root")
			}
			assertFastState(t, w, false, false, 1)
			nativeRPC(t, w, "fixture_finish_root", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, fastRootID) == "done" })
		})
	}
}

func TestFastNativePendingControlBlocksIdleRelease(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_FAST_MODE", "hold")
	w, _, trace := nativeWorker(t, "fast")
	w.b.cfg.IdleTimeout = time.Minute
	nativeCatalogReady(t, w)
	nativeInput(t, w, 100, "/fast", false)
	waitFixtureRPCTrace(t, trace, "fast_waiting", "/fast")
	researchEventBarrier(t, w)
	client := w.client
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	w.releaseIdleRuntime(time.Now())
	if w.client != client || queueState(t, w, 100) != "submitted" {
		t.Fatal("idle timeout discarded an unacknowledged native toggle")
	}
	nativeRPC(t, w, "fixture_fast_release", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" && !w.controlInProgress() })
	assertFastState(t, w, true, true, 0)
}

func TestFastNativeCatalogLookupDoesNotQueueOrPromptWithoutBuiltin(t *testing.T) {
	w, _, trace := nativeWorker(t, "hold")
	waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
	nativeInput(t, w, 100, "/fast on", false)
	if len(w.queue) != 0 || w.taskActive() || researchTraceCount(trace, "prompt", "/fast on") != 0 {
		t.Fatal("fast waiting for catalog proof acquired model ownership or entered the prompt queue")
	}
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"removeName": "fast"})
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "cancelled" && !w.controlInProgress() })
	if researchTraceCount(trace, "prompt", "/fast on") != 0 || w.taskActive() || len(w.queue) != 0 {
		t.Fatal("catalog lookup sent a missing builtin to the model")
	}
}

func TestFastNativeCatalogUpdateEnablesControlBeforeDiscoveryReply(t *testing.T) {
	w, _, trace := nativeWorker(t, "hold")
	waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
	nativeInput(t, w, 100, "/fast", false)
	nativeRPC(t, w, "fixture_catalog_update", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" && !w.controlInProgress() })
	assertFastState(t, w, true, true, 0)
	if w.taskActive() || len(w.queue) != 0 || researchTraceCount(trace, "prompt", "/fast") != 1 || researchTraceCount(trace, "catalog_query", "") != 1 {
		t.Fatal("published builtin proof duplicated discovery, replayed fast, or started model work")
	}
}

func TestFastNativeAcknowledgmentRequiresOriginalCatalogEpoch(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_FAST_MODE", "hold")
	w, _, trace := nativeWorker(t, "fast")
	nativeCatalogReady(t, w)
	fastRoot(t, w)
	nativeInput(t, w, 101, "/fast", false)
	waitFixtureRPCTrace(t, trace, "fast_waiting", "/fast")
	researchEventBarrier(t, w)
	w.client.InvalidateCommandCatalog()
	nativeRPC(t, w, "fixture_fast_release", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "uncertain" && !w.controlInProgress() })
	if researchTraceCount(trace, "prompt", "/fast") != 1 || queueState(t, w, 100) == "done" {
		t.Fatal("out-of-epoch acknowledgment replayed fast or confirmed unrelated root completion")
	}
}

func TestFastNativePendingControlKeepsResearchStopSemantics(t *testing.T) {
	for _, action := range []string{"stop", "task-stop"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_FAST_MODE", "hold")
			w, _, trace := researchWorker(t, "autoresearch")
			researchRoot(t, w, 100, "/autoresearch goal with fast control")
			nativeInput(t, w, 101, "/followup work after research stop", false)
			nativeInput(t, w, 102, "/fast on", false)
			waitFixtureRPCTrace(t, trace, "fast_waiting", "/fast on")
			if action == "stop" {
				nativeInput(t, w, 103, "/stop", false)
			} else {
				w.stopActiveTask()
				w.dispatch()
			}
			if queueState(t, w, 100) != "uncertain" {
				t.Fatalf("pending fast bypassed autonomous stop: root=%s", queueState(t, w, 100))
			}
			nativeUntil(t, w, func() bool {
				return !w.controlInProgress() && !w.research.disablePending && !w.resumeFailed && (queueState(t, w, 101) == "done" || queueState(t, w, 101) == "cancelled")
			})
			if queueState(t, w, 102) != "uncertain" || researchTraceCount(trace, "autoresearch_mode", "off") != 1 || researchTraceCount(trace, "prompt", "/autoresearch goal with fast control") != 1 {
				t.Fatal("fast interruption lost mode-off confirmation or replayed autonomous work")
			}
			want := "done"
			if action == "stop" {
				want = "cancelled"
			}
			if queueState(t, w, 101) != want {
				t.Fatalf("queued followup=%s, want %s", queueState(t, w, 101), want)
			}
		})
	}
}

func TestFastNativePendingControlKeepsSteeredStopSemantics(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_FAST_MODE", "hold")
	trace := filepath.Join(t.TempDir(), "rpc.trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	w := pendingNativeSteerWorker(t, "executing")
	nativeInput(t, w, 100, "/fast", false)
	waitFixtureRPCTrace(t, trace, "fast_waiting", "/fast")
	w.stopActiveTask()
	if w.client != nil || w.taskActive() || w.steerFence.active || len(w.queue) != 1 {
		t.Fatal("pending fast bypassed the steered owner's shutdown boundary")
	}
	for id, want := range map[int64]string{2: "uncertain", 3: "pending", 4: "uncertain", 100: "uncertain"} {
		if got := queueState(t, w, id); got != want {
			t.Fatalf("inbox %d after task Stop = %s, want %s", id, got, want)
		}
	}
}
