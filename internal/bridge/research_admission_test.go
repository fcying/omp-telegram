package bridge

import (
	"encoding/json"
	"testing"
	"time"
)

func TestResearchAdmissionControlsCancelBeforeQueryReturns(t *testing.T) {
	for _, scenario := range []struct{ control, mode string }{{"/stop", "off"}, {"/stop", "on"}, {"/close", "on"}} {
		t.Run(scenario.control+"/"+scenario.mode, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_AUTORESEARCH_MODE", scenario.mode)
			w, _, trace := researchWorker(t, "autoresearch")
			nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
			client := w.client
			w.input = make(chan incoming, 16)
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				w.run()
			}()
			t.Cleanup(func() {
				w.cancel()
				select {
				case <-stopped:
				case <-time.After(5 * time.Second):
					t.Fatal("worker did not stop")
				}
			})
			send := func(id int64, text string) {
				u := update(id, 11, text)
				raw, err := json.Marshal(u)
				requireStoreOK(t, err)
				requireStoreOK(t, w.b.db.Accept(id, raw))
				w.input <- incoming{id: id, msg: u.Message}
			}
			send(100, "work canceled before admission")
			waitFor(t, func() bool { return researchTraceCount(trace, "entries_waiting", "") == 1 })
			send(101, scenario.control)
			waitFor(t, func() bool {
				return queueState(t, w, 101) == "done" && queueState(t, w, 100) == "cancelled"
			})
			if researchTraceCount(trace, "prompt", "work canceled before admission") != 0 {
				t.Fatal("control allowed the pending prompt to execute before canceling it")
			}
			select {
			case <-client.Done():
			default:
				t.Fatal("control retained the runtime with unconfirmed mode")
			}
			if scenario.control == "/stop" {
				waitFor(t, func() bool {
					return researchTraceCount(trace, "prompt", "/autoresearch off") == 1 && researchTraceCount(trace, "autoresearch_mode", "off") == 1
				})
				send(102, "work after canceled admission")
				waitFor(t, func() bool { return queueState(t, w, 102) == "done" })
				if researchTraceCount(trace, "prompt", "work after canceled admission") != 1 || researchTraceCount(trace, "prompt", "work canceled before admission") != 0 {
					t.Fatal("late metadata response replayed canceled work or lost the next prompt")
				}
			}
		})
	}
}

func TestResearchAdmissionCanceledHeadDoesNotPoisonReplacement(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
	nativeInput(t, w, 100, "removed queue head", false)
	waitFor(t, func() bool { return researchTraceCount(trace, "entries_waiting", "") == 1 })
	if !w.cancelQueuedTask(100) {
		t.Fatal("pending head could not be canceled")
	}
	nativeInput(t, w, 101, "replacement queue head", false)
	nativeRPC(t, w, "fixture_autoresearch_release_entries", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" && !w.rpcOperationActive })
	if queueState(t, w, 100) != "cancelled" || researchTraceCount(trace, "prompt", "removed queue head") != 0 || researchTraceCount(trace, "prompt", "replacement queue head") != 1 || w.resumeFailed {
		t.Fatal("canceled admission result poisoned or replayed a different queue head")
	}
}

func TestResearchAdmissionSessionChangeCancelsOriginalQueueHead(t *testing.T) {
	for _, nth := range []int{1, 2} {
		t.Run(map[int]string{1: "before-mode-snapshot", 2: "after-mode-snapshot"}[nth], func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeRPC(t, w, "fixture_get_state_update", map[string]any{"nth": nth, "newSession": true})
			nativeInput(t, w, 100, "work for original native session", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "cancelled" && !w.rpcOperationActive })
			if researchTraceCount(trace, "prompt", "work for original native session") != 0 || !w.resumeFailed {
				t.Fatal("session change admitted pending work using a different native identity")
			}
		})
	}
}

func TestResearchAdmissionCatalogUpdateRequiresFreshMode(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
	nativeInput(t, w, 100, "root after mode changed", false)
	waitFor(t, func() bool { return researchTraceCount(trace, "entries_waiting", "") == 1 })
	// The held snapshot says off. Native mode and the catalog change before it returns.
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"mode": "on"})
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"addName": "new_file_command"})
	nativeRPC(t, w, "fixture_autoresearch_release_entries", nil)
	nativeUntil(t, w, func() bool { return w.research.roundOpen && !w.rpcOperationActive })
	owner, err := w.currentWorkspaceOwner()
	requireStoreOK(t, err)
	owned, err := w.b.db.ResearchWorkspaceOwned(owner)
	requireStoreOK(t, err)
	if queueState(t, w, 100) != "submitted" || w.active != 100 || !w.research.root || !owned || researchTraceCount(trace, "prompt", "root after mode changed") != 1 {
		t.Fatal("stale disabled snapshot lost research root ownership or workspace exclusivity")
	}
}

func TestResearchAdmissionReadyModeCannotSurviveOff(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_AUTORESEARCH_MODE", "on")
	w, _, trace := researchWorker(t, "autoresearch")
	nativeInput(t, w, 100, "ordinary work after explicit off", false)
	// Finish lookup without dispatching the queue head, then change native mode.
	select {
	case result := <-w.operations:
		w.operationReturned(result)
	case <-time.After(5 * time.Second):
		t.Fatal("mode admission did not finish")
	}
	nativeInput(t, w, 101, "/autoresearch off", false)
	nativeUntil(t, w, func() bool { return w.active == 100 })
	owner, err := w.currentWorkspaceOwner()
	requireStoreOK(t, err)
	owned, err := w.b.db.ResearchWorkspaceOwned(owner)
	requireStoreOK(t, err)
	if queueState(t, w, 101) != "done" || w.research.root || w.research.enabled || owned {
		t.Fatal("cached on mode re-established research ownership after confirmed off")
	}
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" && !w.rpcOperationActive })
	if researchTraceCount(trace, "prompt", "/autoresearch off") != 1 || researchTraceCount(trace, "prompt", "ordinary work after explicit off") != 1 {
		t.Fatal("mode change replayed control or lost the queued ordinary prompt")
	}
}
