package bridge

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"omp-telegram/internal/telegram"
)

func runResearchActor(t *testing.T, w *worker) (func(int64, string), func()) {
	t.Helper()
	w.input = make(chan incoming, 16)
	stopped := make(chan struct{})
	go func() { defer close(stopped); w.run() }()
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			w.cancel()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("research actor did not stop")
			}
		})
	}
	t.Cleanup(shutdown)
	return func(id int64, text string) {
		u := update(id, 11, text)
		raw, err := json.Marshal(u)
		requireStoreOK(t, err)
		requireStoreOK(t, w.b.db.Accept(id, raw))
		w.input <- incoming{id: id, msg: u.Message}
	}, shutdown
}

func researchClearCallback(t *testing.T, w *worker, text string) *telegram.CallbackQuery {
	t.Helper()
	nativeInput(t, w, 100, text, false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	f := http.DefaultTransport.(*fakeHTTP)
	buttons := resumeButtons(t, f)
	return &telegram.CallbackQuery{ID: "research-control-clear", From: telegram.User{ID: 7},
		Message: &telegram.Message{MessageID: int64(f.messageCount()), MessageThreadID: w.key.thread, Chat: telegram.Chat{ID: w.key.chat}},
		Data:    buttons[0]["callback_data"].(string)}
}

func TestAutoresearchOffPreservesActiveOrdinaryOwnerAndQueue(t *testing.T) {
	for _, staleProof := range []bool{false, true} {
		t.Run(map[bool]string{false: "recorded-off", true: "fresh-mode-query"}[staleProof], func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeInput(t, w, 100, "wait", false)
			researchBarrier(t, w)
			client, binding, root, turn := w.client, w.binding, w.rootRequestID, w.turn
			nativeInput(t, w, 101, "/followup ordinary queued work", false)
			if staleProof {
				nativeRPC(t, w, "fixture_catalog_update", map[string]any{"addName": "unrelated_command"})
				w.commandCatalogUpdated()
				nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
			}
			nativeInput(t, w, 102, "/autoresearch off", false)
			if staleProof {
				waitFixtureRPCTrace(t, trace, "entries_waiting", "")
				if w.active != 100 || queueState(t, w, 100) != "submitted" {
					t.Fatal("mode lookup retired an ordinary owner")
				}
				nativeRPC(t, w, "fixture_autoresearch_release_entries", nil)
			}
			nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
			if w.client != client || !sameBindingIdentity(w.binding, binding) || w.rootRequestID != root || w.turn != turn || w.active != 100 ||
				queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "pending" || w.resumeFailed || w.research.disablePending {
				t.Fatal("off interrupted or changed the ordinary task, session, or deferred queue")
			}
			if researchTraceCount(trace, "prompt", "/autoresearch off") != 0 {
				t.Fatal("off was submitted as a busy native prompt or steer")
			}
			nativeRPC(t, w, "fixture_finish_root", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			if queueState(t, w, 100) != "done" || strings.Join(researchReplies(t, w, 100), "") != "root completed" || w.client != client ||
				researchTraceCount(trace, "prompt", "wait") != 1 || researchTraceCount(trace, "prompt", "ordinary queued work") != 1 {
				t.Fatal("ordinary task and queued work did not finish exactly once on their original client")
			}
		})
	}
}

func TestAutoresearchReadOnlyOffPreservesQueuedCompletionAcrossCatalogUpdate(t *testing.T) {
	for _, invalidateCatalog := range []bool{false, true} {
		t.Run(map[bool]string{false: "stable-scope", true: "reader-invalidated-scope"}[invalidateCatalog], func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeInput(t, w, 100, "wait", false)
			researchBarrier(t, w)
			client, binding := w.client, w.binding
			nativeInput(t, w, 101, "/followup preserved work", false)
			nativeRPC(t, w, "fixture_catalog_update", map[string]any{"addName": "first_update"})
			researchEventBarrier(t, w)
			nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
			nativeInput(t, w, 102, "/autoresearch off", false)
			waitFixtureRPCTrace(t, trace, "entries_waiting", "")
			if c := w.research.control; c == nil || c.intent != "off" || c.text != "" {
				t.Fatal("did not enter the read-only off query")
			}
			// Let the reader update the snapshot before the actor consumes the preceding completion.
			nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
				"type": "agent_end", "isTerminal": true,
				"messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{
					"type": "text", "text": "ordinary root final reply",
				}}}},
			}})
			if invalidateCatalog {
				nativeRPC(t, w, "fixture_catalog_update", map[string]any{"addName": "second_update"})
			}
			researchEventBarrier(t, w)
			if queueState(t, w, 100) != "done" || strings.Join(researchReplies(t, w, 100), "") != "ordinary root final reply" || w.taskActive() {
				t.Fatal("read-only off lost the ordinary root completion or final reply")
			}
			if w.client != client || !sameBindingIdentity(w.binding, binding) || queueState(t, w, 101) != "pending" {
				t.Fatal("read-only off changed the runtime, binding or preserved queue")
			}
			if invalidateCatalog {
				if queueState(t, w, 102) != "uncertain" || !w.research.disablePending || !w.resumeFailed {
					t.Fatal("invalidated read-only off did not retain the uncertain control and queue pause")
				}
				nativeRPC(t, w, "fixture_autoresearch_release_entries", nil)
				nativeUntil(t, w, func() bool { return !w.rpcOperationActive && len(w.rpcOperations) == 0 })
				if queueState(t, w, 102) != "uncertain" || !w.research.disablePending || !w.resumeFailed || queueState(t, w, 101) != "pending" {
					t.Fatal("stale mode result changed the failed control or released preserved work")
				}
			} else if queueState(t, w, 102) != "submitted" || w.research.disablePending || w.resumeFailed {
				t.Fatal("ordinary completion failed a still-valid read-only off query")
			}
		})
	}
}

func TestAutoresearchRouteScopeFailurePreservesOrdinaryCompletion(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	nativeInput(t, w, 100, "wait", false)
	researchBarrier(t, w)
	client, binding := w.client, w.binding
	nativeInput(t, w, 101, "/followup preserved work", false)
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
	nativeInput(t, w, 102, "/autoresearch next goal", false)
	waitFixtureRPCTrace(t, trace, "entries_waiting", "")
	// The reader sees the catalog update before the actor handles the preceding completion.
	nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
		"type": "agent_end", "isTerminal": true,
		"messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "text", "text": "ordinary root final reply",
		}}}},
	}})
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"addName": "updated_command"})
	researchEventBarrier(t, w)
	if queueState(t, w, 100) != "done" || strings.Join(researchReplies(t, w, 100), "") != "ordinary root final reply" || w.taskActive() {
		t.Fatal("route scope failure lost the ordinary task completion or final reply")
	}
	if w.client != client || !sameBindingIdentity(w.binding, binding) || queueState(t, w, 102) != "cancelled" ||
		queueState(t, w, 101) != "pending" || !w.resumeFailed || researchTraceCount(trace, "prompt", "/autoresearch next goal") != 0 {
		t.Fatal("failed route changed the runtime, submitted research or lost the queue pause")
	}
	nativeRPC(t, w, "fixture_autoresearch_release_entries", nil)
	nativeUntil(t, w, func() bool { return !w.rpcOperationActive && len(w.rpcOperations) == 0 })
	if queueState(t, w, 102) != "cancelled" || queueState(t, w, 101) != "pending" || !w.resumeFailed || w.client != client {
		t.Fatal("stale route result changed the canceled goal or released preserved work")
	}
}

func TestAutoresearchStartupToggleDisablesWithoutWaitingForRoot(t *testing.T) {
	for _, explicitOff := range []bool{false, true} {
		t.Run(map[bool]string{false: "toggle", true: "off-during-toggle-lookup"}[explicitOff], func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "agent_start"})
			nativeInput(t, w, 100, "/autoresearch continuous goal", false)
			nativeUntil(t, w, func() bool { return w.research.accepted && !w.rpcOperationActive })
			waitFixtureRPCTrace(t, trace, "research_control_waiting", "agent_start")
			nativeInput(t, w, 101, "/autoresearch", false)
			if queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "pending" {
				t.Fatal("startup toggle did not preserve the native root's completion boundary")
			}
			if explicitOff {
				nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
			}
			nativeRPC(t, w, "fixture_research_control_release", nil)
			nativeUntil(t, w, func() bool { return w.research.control != nil })
			lastControl := int64(101)
			if explicitOff {
				waitFixtureRPCTrace(t, trace, "entries_waiting", "")
				nativeInput(t, w, 102, "/autoresearch off", false)
				lastControl = 102
			}
			nativeUntil(t, w, func() bool { return queueState(t, w, lastControl) == "done" })
			researchBarrier(t, w)
			enabled, err := w.client.AutoresearchMode(w.ctx)
			requireStoreOK(t, err)
			toggleState := "done"
			if explicitOff {
				toggleState = "cancelled"
			}
			if enabled || queueState(t, w, 100) != "uncertain" || queueState(t, w, 101) != toggleState ||
				researchTraceCount(trace, "prompt", "/autoresearch") != 0 || researchTraceCount(trace, "prompt", "/autoresearch off") != 1 {
				t.Fatal("startup toggle waited behind or reenabled the autonomous research root")
			}
			owned, err := w.researchLeaseOwned()
			requireStoreOK(t, err)
			if owned {
				t.Fatal("confirmed off retained the research lease")
			}
			nativeInput(t, w, 103, "/followup ordinary work after startup toggle", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 103) == "done" })
			if researchTraceCount(trace, "prompt", "ordinary work after startup toggle") != 1 {
				t.Fatal("startup toggle did not admit ordinary work after confirmed off")
			}
		})
	}
}

func TestAutoresearchOffCancelsStartupTogglesAndPreservesQueue(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		toggles      int
		agentStarted bool
	}{
		{name: "before-agent-start", toggles: 1},
		{name: "before-agent-start-multiple", toggles: 2},
		{name: "during-toggle-lookup-multiple", toggles: 2, agentStarted: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "agent_start"})
			nativeInput(t, w, 100, "/autoresearch continuous goal", false)
			nativeUntil(t, w, func() bool { return w.research.accepted && !w.rpcOperationActive })
			owner, err := w.currentWorkspaceOwner()
			requireStoreOK(t, err)
			nativeInput(t, w, 101, "/followup work before startup toggles", false)
			for i := 0; i < scenario.toggles; i++ {
				text := "/autoresearch"
				if i != 0 {
					text += "@fixture_bot"
				}
				nativeInput(t, w, int64(102+i), text, false)
			}
			nativeInput(t, w, 200, "/followup work after startup toggles", false)
			if scenario.agentStarted {
				nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
				nativeRPC(t, w, "fixture_research_control_release", nil)
			}
			send, shutdown := runResearchActor(t, w)
			if scenario.agentStarted {
				waitFixtureRPCTrace(t, trace, "entries_waiting", "")
			}
			send(300, "/autoresearch off")
			waitFor(t, func() bool {
				return queueState(t, w, 300) == "done" && queueState(t, w, 101) == "done" && queueState(t, w, 200) == "done"
			})
			shutdown()
			for i := 0; i < scenario.toggles; i++ {
				if queueState(t, w, int64(102+i)) != "cancelled" {
					t.Fatal("off did not cancel every pending shutdown toggle")
				}
			}
			lastMode := ""
			var prompts []string
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "autoresearch_mode" {
					lastMode = entry.payload
				}
				if entry.kind == "prompt" && entry.payload != "/session info" {
					prompts = append(prompts, entry.payload)
				}
			}
			owned, err := w.b.db.ResearchWorkspaceOwned(owner)
			requireStoreOK(t, err)
			if queueState(t, w, 100) != "uncertain" || lastMode != "off" || owned {
				t.Fatal("an old startup toggle reenabled research or reacquired its lease after off")
			}
			if strings.Join(prompts, "\n") != "/autoresearch continuous goal\n/autoresearch off\nwork before startup toggles\nwork after startup toggles" {
				t.Fatalf("off replayed a toggle or changed preserved prompt order: %q", prompts)
			}
			t.Logf("worker.run: toggles=cancelled off=done queued_work=done mode=%s lease_owned=%t", lastMode, owned)
		})
	}
}

func TestAutoresearchOverlappingReadOnlyOffPreservesOrdinaryTask(t *testing.T) {
	for _, rootFinishes := range []bool{false, true} {
		t.Run(map[bool]string{false: "running-root", true: "completed-root"}[rootFinishes], func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeInput(t, w, 100, "wait", false)
			researchBarrier(t, w)
			client, binding, root, turn := w.client, w.binding, w.rootRequestID, w.turn
			nativeInput(t, w, 101, "/followup ordinary queued work after overlapping off", false)
			nativeRPC(t, w, "fixture_catalog_update", map[string]any{"addName": "unrelated_command"})
			w.commandCatalogUpdated()
			nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
			nativeInput(t, w, 102, "/autoresearch off", false)
			waitFixtureRPCTrace(t, trace, "entries_waiting", "")
			if rootFinishes {
				nativeRPC(t, w, "fixture_finish_root", nil)
				researchEventBarrier(t, w)
				if queueState(t, w, 100) != "done" {
					t.Fatal("read-only off lost the ordinary root's completion")
				}
			}
			nativeInput(t, w, 103, "/autoresearch off", false)
			if queueState(t, w, 102) != "uncertain" {
				t.Fatal("canceled read-only off was reported complete")
			}
			if w.client != client || !sameBindingIdentity(w.binding, binding) ||
				(!rootFinishes && (w.rootRequestID != root || w.turn != turn || queueState(t, w, 100) != "submitted")) {
				t.Fatal("overlapping read-only off retired or changed the ordinary owner")
			}
			nativeUntil(t, w, func() bool { return len(w.rpcOperations) == 0 && w.rpcOperationActive })
			waitFor(t, func() bool { return researchTraceCount(trace, "entries_waiting", "") == 2 })
			nativeRPC(t, w, "fixture_autoresearch_release_entries", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, 103) == "done" })
			if w.client != client || w.resumeFailed || w.research.disablePending || researchTraceCount(trace, "prompt", "/autoresearch off") != 0 {
				t.Fatal("overlapping off submitted a native control or retained the queue pause")
			}
			if !rootFinishes {
				nativeRPC(t, w, "fixture_finish_root", nil)
			}
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			if queueState(t, w, 100) != "done" || strings.Join(researchReplies(t, w, 100), "") != "root completed" ||
				w.client != client || researchTraceCount(trace, "prompt", "wait") != 1 ||
				researchTraceCount(trace, "prompt", "ordinary queued work after overlapping off") != 1 {
				t.Fatal("overlapping off lost the ordinary reply or replayed work")
			}
		})
	}
}

func TestAutoresearchOffReplacingRoutePreservesOrdinaryTask(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		rootFinishes, staleProof bool
	}{
		{name: "running-root-recorded-off"},
		{name: "running-root-fresh-query", staleProof: true},
		{name: "completed-root-recorded-off", rootFinishes: true},
		{name: "completed-root-fresh-query", rootFinishes: true, staleProof: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeInput(t, w, 100, "wait", false)
			researchBarrier(t, w)
			client, binding, root, turn := w.client, w.binding, w.rootRequestID, w.turn
			nativeInput(t, w, 101, "/followup preserved work after route", false)
			if tc.staleProof {
				nativeRPC(t, w, "fixture_catalog_update", map[string]any{"addName": "unrelated_command"})
				w.commandCatalogUpdated()
			}
			nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
			nativeInput(t, w, 102, "/autoresearch next goal", false)
			waitFixtureRPCTrace(t, trace, "entries_waiting", "")
			if tc.rootFinishes {
				nativeRPC(t, w, "fixture_finish_root", nil)
				researchEventBarrier(t, w)
				if queueState(t, w, 100) != "done" {
					t.Fatal("route lookup lost the ordinary root's completion")
				}
			}
			nativeInput(t, w, 103, "/autoresearch off", false)
			if queueState(t, w, 102) != "cancelled" || w.client != client || !sameBindingIdentity(w.binding, binding) ||
				(!tc.rootFinishes && (w.rootRequestID != root || w.turn != turn || queueState(t, w, 100) != "submitted")) {
				t.Fatal("off replacing a read-only route changed the ordinary owner or failed to cancel the goal")
			}
			if tc.staleProof || tc.rootFinishes {
				nativeUntil(t, w, func() bool { return len(w.rpcOperations) == 0 && w.rpcOperationActive })
				waitFor(t, func() bool { return researchTraceCount(trace, "entries_waiting", "") == 2 })
			}
			nativeRPC(t, w, "fixture_autoresearch_release_entries", nil)
			nativeUntil(t, w, func() bool {
				return queueState(t, w, 103) == "done" && !w.rpcOperationActive && len(w.rpcOperations) == 0
			})
			if w.client != client || w.resumeFailed || w.research.disablePending || queueState(t, w, 102) != "cancelled" ||
				researchTraceCount(trace, "prompt", "/autoresearch next goal") != 0 || researchTraceCount(trace, "prompt", "/autoresearch off") != 0 {
				t.Fatal("stale route result or replacing off submitted a native control or left the queue paused")
			}
			if !tc.rootFinishes {
				nativeRPC(t, w, "fixture_finish_root", nil)
			}
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			if queueState(t, w, 100) != "done" || strings.Join(researchReplies(t, w, 100), "") != "root completed" ||
				w.client != client || researchTraceCount(trace, "prompt", "wait") != 1 ||
				researchTraceCount(trace, "prompt", "preserved work after route") != 1 {
				t.Fatal("off replacing a route lost the ordinary reply or replayed preserved work")
			}
		})
	}
}

func TestAutoresearchSlowControlsRemainPreemptible(t *testing.T) {
	for _, intent := range []string{"off", "clear"} {
		for _, phase := range []string{"get_state", "idle", "get_entries", "prompt", "result", "postmode"} {
			for _, interrupt := range []string{"/stop", "/close"} {
				t.Run(intent+"/"+phase+"/"+interrupt, func(t *testing.T) {
					t.Setenv("OMP_TELEGRAM_FIXTURE_AUTORESEARCH_MODE", "on")
					w, _, trace := researchWorker(t, "autoresearch")
					var clear *telegram.CallbackQuery
					if intent == "clear" {
						clear = researchClearCallback(t, w, "/autoresearch clear --force --keep-tree")
					} else {
						// Admission acquires the exact persisted owner without starting an agent.
						nativeInput(t, w, 100, "/session pin", false)
						nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
					}
					owner, err := w.currentWorkspaceOwner()
					requireStoreOK(t, err)
					client := w.client
					waitKind, waitPhase := "research_control_waiting", phase
					switch phase {
					case "get_entries":
						nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
						waitKind, waitPhase = "entries_waiting", ""
					case "idle":
						nth := 2
						if intent == "clear" {
							nth = 3
						}
						nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "get_state", "nth": nth})
						waitPhase = "get_state"
					case "postmode":
						nth := 3
						if intent == "clear" {
							nth = 4
						}
						nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "get_state", "nth": nth})
						waitPhase = "get_state"
					default:
						nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": phase})
					}
					if intent == "clear" {
						w.callback(clear)
					} else {
						nativeInput(t, w, 101, "/autoresearch off", false)
					}
					send, shutdown := runResearchActor(t, w)
					waitFixtureRPCTrace(t, trace, waitKind, waitPhase)
					send(102, "/followup do not run after canceled control")
					send(103, interrupt)
					waitFor(t, func() bool { return queueState(t, w, 103) == "done" && queueState(t, w, 102) == "cancelled" })
					select {
					case <-client.Done():
					case <-time.After(time.Second):
						t.Fatal("Stop/close did not retire a runtime with a held research control")
					}
					owned, err := w.b.db.ResearchWorkspaceOwned(owner)
					requireStoreOK(t, err)
					if !owned {
						t.Fatal("cancellation released research ownership without completed local control and disabled mode")
					}
					if intent == "off" && queueState(t, w, 101) != "uncertain" {
						t.Fatal("canceled off was reported complete")
					}
					if researchTraceCount(trace, "prompt", "do not run after canceled control") != 0 {
						t.Fatal("canceled control admitted queued work")
					}
					shutdown()
					if researchTraceCount(trace, "prompt", "/autoresearch off") > 1 || researchTraceCount(trace, "autoresearch_clear", "clear --force --keep-tree") > 1 {
						t.Fatal("Stop/close implicitly replayed the canceled native control")
					}
				})
			}
		}
	}
}

func TestAutoresearchLocalCompletionRequiresAcceptanceAndFencesOldResults(t *testing.T) {
	w, command, trace := researchWorker(t, "autoresearch")
	nativeInput(t, w, 100, "/autoresearch", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	owner, err := w.currentWorkspaceOwner()
	requireStoreOK(t, err)
	nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "prompt"})
	nativeInput(t, w, 101, "/autoresearch off", false)
	nativeUntil(t, w, func() bool { return w.research.control != nil && w.research.control.completed })
	old := w.research.control
	owned, err := w.b.db.ResearchWorkspaceOwned(owner)
	requireStoreOK(t, err)
	if !owned || old.accepted || queueState(t, w, 101) != "submitted" {
		t.Fatal("completion before acceptance released ownership or claimed control done")
	}
	command("/close")
	callback := waitOperation(t, w)
	w.operationReturned(callback)
	nativeInput(t, w, 102, "/autoresearch off", false)
	nativeUntil(t, w, func() bool { return !w.research.disablePending && !w.resumeFailed && !w.controlInProgress() })
	command("/close")
	command("/new " + t.TempDir())
	nativeCatalogReady(t, w)
	before := nativeOutputs(t, w)
	w.operationFinished(callback)
	w.event([]byte(`{"type":"prompt_result","id":"` + old.requestID + `","status":"completed","agentInvoked":false,"sessionSettled":true}`))
	if nativeOutputs(t, w) != before || w.research.control != nil || w.research.enabled {
		t.Fatal("retired control callback/result affected the replacement scope")
	}
	if researchTraceCount(trace, "prompt", "/autoresearch off") != 2 {
		t.Fatal("control count differs from the two explicit off requests")
	}
}

func TestAutoresearchScopeUpdatesDuringLocalResultWaitStayInActor(t *testing.T) {
	for _, sessionChange := range []bool{false, true} {
		t.Run(map[bool]string{false: "catalog", true: "session"}[sessionChange], func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_AUTORESEARCH_MODE", "on")
			w, _, trace := researchWorker(t, "autoresearch")
			clear := researchClearCallback(t, w, "/autoresearch clear --force --keep-tree")
			owner, err := w.currentWorkspaceOwner()
			requireStoreOK(t, err)
			nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "result"})
			w.callback(clear)
			nativeUntil(t, w, func() bool { return w.research.control != nil && w.research.control.phase == "completion" })
			oldID := w.research.control.requestID
			fields := map[string]any{"sourceName": "autoresearch", "source": "unknown"}
			if sessionChange {
				fields = map[string]any{"newSession": true}
			}
			nativeRPC(t, w, "fixture_catalog_update", fields)
			nativeUntil(t, w, func() bool { return !w.controlInProgress() })
			owned, err := w.b.db.ResearchWorkspaceOwned(owner)
			requireStoreOK(t, err)
			if !owned || !w.resumeFailed || w.client != nil {
				t.Fatal("scope update was stolen during local result wait or released uncertain research ownership")
			}
			before := nativeOutputs(t, w)
			w.event([]byte(`{"type":"prompt_result","id":"` + oldID + `","status":"completed","agentInvoked":false,"sessionSettled":true}`))
			if nativeOutputs(t, w) != before || researchTraceCount(trace, "autoresearch_clear", "clear --force --keep-tree") != 1 {
				t.Fatal("old result released or replayed a scope-invalid clear")
			}
		})
	}
}

func TestAutoresearchResultWaitDispatchesUnrelatedEventsAndMatchesRequest(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_AUTORESEARCH_MODE", "on")
	w, _, trace := researchWorker(t, "autoresearch")
	clear := researchClearCallback(t, w, "/autoresearch clear --force --keep-tree")
	owner, err := w.currentWorkspaceOwner()
	requireStoreOK(t, err)
	nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "result"})
	w.callback(clear)
	nativeUntil(t, w, func() bool { return w.research.control != nil && w.research.control.phase == "completion" })
	id := w.research.control.requestID
	nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
		"type": "prompt_result", "id": id + "old", "status": "completed", "agentInvoked": false, "sessionSettled": true,
	}})
	nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{"type": "auto_retry_start"}})
	nativeUntil(t, w, func() bool { return w.progress.Retrying })
	owned, err := w.b.db.ResearchWorkspaceOwned(owner)
	requireStoreOK(t, err)
	if !owned || w.research.control == nil || w.research.control.completed {
		t.Fatal("an unrelated event or request completed the local control or stole actor event dispatch")
	}
	nativeRPC(t, w, "fixture_research_control_release", nil)
	nativeUntil(t, w, func() bool { return !w.controlInProgress() })
	owned, err = w.b.db.ResearchWorkspaceOwned(owner)
	requireStoreOK(t, err)
	if owned || w.resumeFailed || researchTraceCount(trace, "autoresearch_clear", "clear --force --keep-tree") != 1 {
		t.Fatal("correlated completed local control and disabled mode failed to release its lease exactly once")
	}
}

func TestAutoresearchIncompleteLocalResultsNeverReleaseLease(t *testing.T) {
	for _, fields := range []map[string]any{
		{"status": "aborted"}, {"status": "missing"}, {"agentInvoked": true},
		{"agentInvoked": "missing"}, {"sessionSettled": false}, {"sessionSettled": "missing"},
	} {
		name, err := json.Marshal(fields)
		requireStoreOK(t, err)
		t.Run(string(name), func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_AUTORESEARCH_MODE", "on")
			w, _, trace := researchWorker(t, "autoresearch")
			clear := researchClearCallback(t, w, "/autoresearch clear --force --keep-tree")
			owner, err := w.currentWorkspaceOwner()
			requireStoreOK(t, err)
			nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "result"})
			w.callback(clear)
			nativeUntil(t, w, func() bool { return w.research.control != nil && w.research.control.phase == "completion" })
			nativeRPC(t, w, "fixture_research_control_release", fields)
			nativeUntil(t, w, func() bool { return !w.controlInProgress() })
			owned, err := w.b.db.ResearchWorkspaceOwned(owner)
			requireStoreOK(t, err)
			if !owned || !w.resumeFailed || !w.research.disablePending || w.client != nil ||
				researchTraceCount(trace, "autoresearch_clear", "clear --force --keep-tree") != 1 {
				t.Fatal("incomplete/nonlocal result released ownership, claimed success, retained the runtime, or replayed destruction")
			}
		})
	}
}

func TestAutoresearchRoutingLookupCanBeStoppedBeforeConfirmationOrPrompt(t *testing.T) {
	for _, text := range []string{"/autoresearch", "/autoresearch a new goal", "/autoresearch clear --force --keep-tree"} {
		for _, phase := range []string{"get_state", "get_entries"} {
			for _, interrupt := range []string{"/stop", "/close"} {
				t.Run(text+"/"+phase+"/"+interrupt, func(t *testing.T) {
					w, _, trace := researchWorker(t, "autoresearch")
					waitKind, waitPhase := "research_control_waiting", phase
					if phase == "get_entries" {
						nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
						waitKind, waitPhase = "entries_waiting", ""
					} else {
						nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": phase})
					}
					client := w.client
					send, shutdown := runResearchActor(t, w)
					send(100, text)
					waitFixtureRPCTrace(t, trace, waitKind, waitPhase)
					send(101, interrupt)
					waitFor(t, func() bool { return queueState(t, w, 101) == "done" && queueState(t, w, 100) == "cancelled" })
					select {
					case <-client.Done():
					case <-time.After(time.Second):
						t.Fatal("routing lookup blocked Stop/close")
					}
					shutdown()
					if researchTraceCount(trace, "prompt", text) != 0 || len(w.confirms) != 0 {
						t.Fatal("canceled routing lookup submitted a prompt or produced a destructive confirmation")
					}
				})
			}
		}
	}
}

func TestAutoresearchToggleResultWaitsForAcceptance(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "prompt"})
	nativeInput(t, w, 100, "/autoresearch", false)
	nativeUntil(t, w, func() bool {
		return w.research.control != nil && w.research.control.phase == "acceptance"
	})
	owner, err := w.currentWorkspaceOwner()
	requireStoreOK(t, err)
	owned, err := w.b.db.ResearchWorkspaceOwned(owner)
	requireStoreOK(t, err)
	if !owned || queueState(t, w, 100) != "submitted" || w.research.control.accepted {
		t.Fatal("local toggle result completed the submission before acceptance")
	}
	nativeRPC(t, w, "fixture_research_control_release", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	if !w.research.known || !w.research.enabled || w.taskActive() || researchTraceCount(trace, "prompt", "/autoresearch") != 1 {
		t.Fatal("accepted local toggle lost its recorded mode or replayed the command")
	}
}

func TestAutoresearchOffModeFailurePreservesOrdinaryTask(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	nativeInput(t, w, 100, "wait", false)
	researchBarrier(t, w)
	client, root := w.client, w.rootRequestID
	nativeInput(t, w, 101, "/followup work held after failed mode lookup", false)
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"addName": "unrelated_command"})
	w.commandCatalogUpdated()
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "error"})
	nativeInput(t, w, 102, "/autoresearch off", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "uncertain" })
	if w.client != client || w.rootRequestID != root || w.active != 100 || queueState(t, w, 100) != "submitted" {
		t.Fatal("failed read-only mode lookup interrupted the ordinary task")
	}
	if !w.resumeFailed || !w.research.disablePending || researchTraceCount(trace, "prompt", "/autoresearch off") != 0 {
		t.Fatal("failed mode lookup submitted off or admitted future work")
	}
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	if strings.Join(researchReplies(t, w, 100), "") != "root completed" || queueState(t, w, 101) != "pending" ||
		researchTraceCount(trace, "prompt", "work held after failed mode lookup") != 0 {
		t.Fatal("failed off lost the ordinary result or dispatched paused work")
	}
}

func TestAutoresearchOffRetryPreservesOrdinaryTaskAndUnpausesQueue(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	nativeInput(t, w, 100, "wait", false)
	researchBarrier(t, w)
	client, binding, root, turn := w.client, w.binding, w.rootRequestID, w.turn
	nativeInput(t, w, 101, "/followup ordinary work after retry", false)
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"addName": "unrelated_command"})
	w.commandCatalogUpdated()
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "error"})
	for _, id := range []int64{102, 103} {
		nativeInput(t, w, id, "/autoresearch off", false)
		nativeUntil(t, w, func() bool { return queueState(t, w, id) == "uncertain" })
		if w.client != client || w.active != 100 || queueState(t, w, 100) != "submitted" ||
			queueState(t, w, 101) != "pending" || !w.resumeFailed || !w.research.disablePending {
			t.Fatal("failed off retry interrupted ordinary work or removed the queue pause")
		}
	}
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": ""})
	nativeInput(t, w, 104, "/autoresearch off", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 104) == "done" })
	if w.client != client || !sameBindingIdentity(w.binding, binding) || w.rootRequestID != root || w.turn != turn ||
		w.active != 100 || queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "pending" ||
		w.resumeFailed || w.research.disablePending || researchTraceCount(trace, "prompt", "/autoresearch off") != 0 {
		t.Fatal("confirmed mode-off retry changed the ordinary owner or failed to unpause its queue")
	}
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	if queueState(t, w, 100) != "done" || strings.Join(researchReplies(t, w, 100), "") != "root completed" ||
		w.client != client || researchTraceCount(trace, "prompt", "wait") != 1 ||
		researchTraceCount(trace, "prompt", "ordinary work after retry") != 1 {
		t.Fatal("off retry lost the ordinary reply or replayed submitted work")
	}
}

func TestAutoresearchOffWithoutSessionFileRequiresLiveRuntime(t *testing.T) {
	for _, connected := range []bool{true, false} {
		t.Run(map[bool]string{true: "connected", false: "cold-resume"}[connected], func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeInput(t, w, 100, "/autoresearch", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			client, binding := w.client, w.binding
			owner, err := w.currentWorkspaceOwner()
			requireStoreOK(t, err)
			// The fixture persists eagerly; live native sessions may have no file yet.
			requireStoreOK(t, os.Remove(w.binding.Session))
			if !connected {
				w.releaseRuntimeWithReason(true, "idle")
			}
			nativeInput(t, w, 101, "/autoresearch off", false)
			nativeUntil(t, w, func() bool {
				state := queueState(t, w, 101)
				return state != "pending" && state != "submitted" && !w.controlInProgress()
			})
			owned, err := w.b.db.ResearchWorkspaceOwned(owner)
			requireStoreOK(t, err)
			if !connected {
				if queueState(t, w, 101) != "uncertain" || !owned || !w.resumeFailed || !w.research.disablePending ||
					w.client != nil || researchTraceCount(trace, "prompt", "/autoresearch off") != 0 {
					t.Fatal("missing saved history admitted a cold resume or released its lease")
				}
				return
			}
			if queueState(t, w, 101) != "done" || owned || w.resumeFailed || w.research.disablePending ||
				w.client != client || !sameBindingIdentity(w.binding, binding) ||
				researchTraceCount(trace, "prompt", "/autoresearch off") != 1 {
				t.Fatal("live off required a session file or failed to confirm disable on its original owner")
			}
			nativeInput(t, w, 102, "ordinary work after local off", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
			if researchTraceCount(trace, "prompt", "ordinary work after local off") != 1 {
				t.Fatal("confirmed live off did not admit subsequent ordinary work exactly once")
			}
		})
	}
}

func TestAutoresearchLocalCompletionKeepsQueuedResearchGoal(t *testing.T) {
	for _, phase := range []string{"prompt", "result"} {
		t.Run(phase, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": phase})
			nativeInput(t, w, 100, "/autoresearch", false)
			nativeUntil(t, w, func() bool { return w.active == 100 })
			client := w.client
			nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
			nativeInput(t, w, 101, "/autoresearch next goal", false)
			waitFixtureRPCTrace(t, trace, "research_control_waiting", phase)
			if phase == "result" {
				nativeRPC(t, w, "fixture_research_control_release", nil)
			}
			// Consume the local completion before releasing mode metadata. A route
			// lookup for the next goal must not swallow the current root's result.
			researchEventBarrier(t, w)
			if phase == "prompt" {
				nativeRPC(t, w, "fixture_research_control_release", nil)
			}
			nativeRPC(t, w, "fixture_autoresearch_release_entries", nil)
			nativeUntil(t, w, func() bool { return w.active == 101 && w.research.roundOpen && !w.rpcOperationActive })
			researchBarrier(t, w)
			if queueState(t, w, 100) != "done" || queueState(t, w, 101) != "submitted" || w.client != client ||
				researchTraceCount(trace, "prompt", "/autoresearch") != 1 || researchTraceCount(trace, "prompt", "/autoresearch next goal") != 1 {
				t.Fatal("local completion lost its owner or blocked/replayed the next research goal")
			}
			researchRound(t, w, "reply from the queued research goal")
			if strings.Join(researchReplies(t, w, 101), "") != "reply from the queued research goal" {
				t.Fatal("the queued research goal did not retain its durable reply ownership")
			}
		})
	}
}
