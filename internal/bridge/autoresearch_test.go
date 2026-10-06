package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/telegram"
)

func researchWorker(t *testing.T, mode string) (*worker, func(string), string) {
	t.Helper()
	w, command, trace := nativeWorker(t, mode)
	nativeCatalogReady(t, w)
	return w, command, trace
}

// Observe the ordered fixture marker before applying RPC results: settlement
// results can drain queued events themselves. Then finish the RPC lane so the
// next test action cannot race a probe started by those preceding events.
func researchBarrier(t *testing.T, w *worker) {
	t.Helper()
	nativeUntil(t, w, func() bool { return !w.rpcOperationActive && len(w.rpcOperations) == 0 })
	researchEventBarrier(t, w)
	nativeUntil(t, w, func() bool { return !w.rpcOperationActive && len(w.rpcOperations) == 0 })
}

// Drain preceding native events even when an RPC response is deliberately held.
func researchEventBarrier(t *testing.T, w *worker) {
	t.Helper()
	marker := "autoresearch-test-barrier:" + w.client.ReserveRequestID()
	nativeRPC(t, w, "fixture_autoresearch_notify", map[string]any{"message": marker})
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case result := <-w.nativeCatalog.results:
			w.commandCatalogFinished(result)
		case raw, ok := <-w.client.Events():
			if !ok {
				t.Fatal("native event stream closed before research barrier")
			}
			var event struct{ Type, Method, Message string }
			if json.Unmarshal(raw, &event) == nil && event.Type == "extension_ui_request" && event.Method == "notify" && event.Message == marker {
				// This test-only marker must not consume the root's notice budget.
				return
			}
			w.event(raw)
		case <-timer.C:
			t.Fatal("native research event barrier was not reached")
		}
		w.dispatch()
	}
}

func researchRoot(t *testing.T, w *worker, id int64, text string) {
	t.Helper()
	nativeInput(t, w, id, text, false)
	researchBarrier(t, w)
	nativeUntil(t, w, func() bool { return w.research.roundOpen && !w.rpcOperationActive })
	if w.active != id || !w.taskActive() || queueState(t, w, id) != "submitted" {
		t.Fatal("research did not retain the original submitted root")
	}
}

func researchRound(t *testing.T, w *worker, text string) {
	t.Helper()
	nativeRPC(t, w, "fixture_autoresearch_round", map[string]any{"text": text})
	researchBarrier(t, w)
}

func researchReplies(t *testing.T, w *worker, id int64) []string {
	t.Helper()
	rows, err := w.b.db.DB.Query("SELECT text FROM outbox WHERE inbox_id=? ORDER BY id", id)
	requireStoreOK(t, err)
	defer rows.Close()
	var replies []string
	for rows.Next() {
		var text string
		requireStoreOK(t, rows.Scan(&text))
		replies = append(replies, text)
	}
	requireStoreOK(t, rows.Err())
	return replies
}

func researchTraceCount(trace, kind, payload string) int {
	count := 0
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == kind && entry.payload == payload {
			count++
		}
	}
	return count
}

func TestAutoresearchMenuOnlyAllowsExactKnownExtension(t *testing.T) {
	w, command, trace := nativeWorker(t, "autoresearch")
	requests := make(chan map[string]any, 64)
	http.DefaultTransport.(*fakeHTTP).commandRequests = requests
	w.b.cfg.AllowedChats = []int64{w.key.chat}
	w.b.registerCommandMenuWorker(w)
	ctx, cancel := context.WithCancel(w.ctx)
	done := make(chan struct{})
	go func() { defer close(done); w.b.runCommandMenus(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	nativeCatalogReady(t, w)
	awaitNativeMenu(t, requests, w.key.chat, "autoresearch", "foo", "native_agent", "native_local", "session")
	for i, text := range []string{"/research_alias secret", "/extension_switch secret", "/extension_alias secret"} {
		id := int64(100 + i)
		nativeInput(t, w, id, text, false)
		if queueState(t, w, id) != "cancelled" || researchTraceCount(trace, "prompt", text) != 0 {
			t.Fatal("autoresearch exception allowed another extension or alias")
		}
	}
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"sourceName": "autoresearch", "source": "unknown"})
	w.commandCatalogUpdated()
	awaitNativeMenu(t, requests, w.key.chat, "foo", "native_agent", "native_local", "session")
	nativeInput(t, w, 110, "/autoresearch secret", false)
	if queueState(t, w, 110) != "cancelled" || researchTraceCount(trace, "prompt", "/autoresearch secret") != 0 {
		t.Fatal("unknown-source autoresearch reached the native handler")
	}
	command("/close")
	awaitNativeMenu(t, requests, w.key.chat)
}

func TestAutoresearchRoundsRetainRootAndDurablyFlushWithoutDuplicates(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	researchRoot(t, w, 100, "/autoresearch improve throughput")
	client, turn, generation := w.client, w.turn, w.binding.Generation
	nativeInput(t, w, 101, "/followup deferred ordinary task", false)
	for i, text := range []string{"research round alpha", "research round beta"} {
		if i != 0 {
			nativeRPC(t, w, "fixture_autoresearch_continue", nil)
		}
		researchRound(t, w, text)
		if queueState(t, w, 100) != "submitted" || w.active != 100 || !w.taskActive() || w.client != client || w.turn != turn || w.binding.Generation != generation {
			t.Fatal("round yield closed or replaced the explicitly managed root")
		}
		if queueState(t, w, 101) != "pending" || researchTraceCount(trace, "prompt", "deferred ordinary task") != 0 {
			t.Fatal("round yield dispatched the queued followup")
		}
		if w.preview != "" || w.stream.Len() != 0 || len(w.finalAssistantTexts) != 0 {
			t.Fatal("round text remained in growing in-memory reply buffers")
		}
	}
	replies := strings.Join(researchReplies(t, w, 100), "\n")
	for _, text := range []string{"research round alpha", "research round beta"} {
		if strings.Count(replies, text) != 1 {
			t.Fatalf("round text was lost or duplicated: %q", replies)
		}
	}
	if researchTraceCount(trace, "prompt", "/autoresearch improve throughput") != 1 {
		t.Fatal("bridge replayed the research prompt between autonomous rounds")
	}
}

func TestAutoresearchEmptyToggleNextPromptAndCurrentLeafRecovery(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	nativeInput(t, w, 100, "/autoresearch", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	if w.taskActive() || !strings.Contains(nativeOutputs(t, w), "Research fixture enabled") {
		t.Fatal("mode-only toggle hung or lost its native notification")
	}
	before, oldClient := w.binding, w.client
	w.b.cfg.IdleTimeout = time.Minute
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	w.releaseIdleRuntime(time.Now())
	if w.client != nil || w.runtime != runtimeReleased {
		t.Fatal("idle mode-only runtime did not release")
	}
	researchRoot(t, w, 101, "ordinary prompt after lazy resume")
	if w.client == oldClient || !sameBindingIdentity(w.binding, before) {
		t.Fatal("lazy resume did not preserve native session identity on a fresh runtime")
	}
	researchRound(t, w, "restored mode round")
	if queueState(t, w, 101) != "submitted" || researchTraceCount(trace, "get_entries", "") == 0 {
		t.Fatal("resumed dispatch ignored native mode on the current leaf")
	}
}

func TestAutoresearchEnabledModeAllowsNativeLocalCompletion(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	nativeInput(t, w, 100, "/autoresearch", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	nativeInput(t, w, 101, "/session pin", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	if w.taskActive() {
		t.Fatal("local native command retained the research owner")
	}
	researchRoot(t, w, 102, "research after the local command")
	if researchTraceCount(trace, "prompt", "/session pin") != 1 || researchTraceCount(trace, "prompt", "research after the local command") != 1 {
		t.Fatal("local completion lost or replayed the following research prompt")
	}
}

func TestAutoresearchLocalCompletionPreservesImmediateOrdinaryInput(t *testing.T) {
	for _, control := range []struct {
		name        string
		initialMode string
		text        string
	}{
		{"enable-toggle", "off", "/autoresearch"},
		{"enabled-native-local", "on", "/session pin"},
	} {
		t.Run(control.name, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_AUTORESEARCH_MODE", control.initialMode)
			w, _, trace := researchWorker(t, "autoresearch")
			nativeInput(t, w, 100, control.text, false)
			// Admit both inputs before consuming any local completion or agent events.
			nativeInput(t, w, 101, "ordinary input after local command", false)
			nativeInput(t, w, 102, "/followup ordinary work after off", false)
			if queueState(t, w, 101) != "pending" || queueState(t, w, 102) != "pending" || w.steerFence.active {
				t.Fatal("ordinary input bypassed the native command's completion boundary")
			}
			nativeUntil(t, w, func() bool { return w.active == 101 && w.research.roundOpen && !w.rpcOperationActive })
			if queueState(t, w, 100) != "done" || queueState(t, w, 101) != "submitted" || queueState(t, w, 102) != "pending" {
				t.Fatal("local completion lost the next ordinary owner or dispatched its followup")
			}
			researchRound(t, w, "queued ordinary research reply")
			if got := strings.Join(researchReplies(t, w, 101), "\n"); got != "queued ordinary research reply" {
				t.Fatalf("queued ordinary input lost its durable reply: %q", got)
			}
			nativeInput(t, w, 103, "/autoresearch off", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
			if w.steerFence.active || len(w.steers) != 0 || researchTraceCount(trace, "prompt", "ordinary work after off") != 1 {
				t.Fatal("local completion left a steer fence or lost subsequent queued work")
			}
		})
	}
}

func TestAutoresearchPendingStatusDoesNotBlockStopOrClose(t *testing.T) {
	for _, control := range []string{"stop", "close", "progress"} {
		t.Run(control, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			researchRoot(t, w, 100, "/autoresearch responsive stop")
			oldClient := w.client
			if control == "progress" {
				w.b.cfg.ProgressMode = "summary"
				w.previewResult = make(chan previewResult, 1)
				allowInitialProgress(w)
				w.flushPreview()
				select {
				case result := <-w.previewResult:
					if result.err != nil || result.id == 0 {
						t.Fatal("research progress could not be created")
					}
					w.previewFinished(result)
				case <-time.After(5 * time.Second):
					t.Fatal("research progress was not published")
				}
			}
			nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				w.status()
			}()
			waitFixtureRPCTrace(t, trace, "entries_waiting", "")
			select {
			case <-returned:
			case <-time.After(time.Second):
				w.cancel()
				<-returned
				t.Fatal("status blocked control handling behind native metadata")
			}
			if control == "progress" {
				f := http.DefaultTransport.(*fakeHTTP)
				w.callback(&telegram.CallbackQuery{ID: "research-progress-stop", From: telegram.User{ID: 7}, Data: f.button(f.messageCount() - 1)})
			} else {
				nativeInput(t, w, 101, "/"+control, false)
			}
			select {
			case <-oldClient.Done():
			case <-time.After(time.Second):
				t.Fatal("control did not terminate research while metadata was pending")
			}
			if queueState(t, w, 100) != "uncertain" {
				t.Fatal("interrupted research retained its submitted owner")
			}
			before := nativeOutputs(t, w)
			w.operationReturned(waitOperation(t, w))
			if nativeOutputs(t, w) != before {
				t.Fatal("retired status query published a stale reply")
			}
		})
	}
}

func TestAutoresearchProcessExitWithPendingStatusAllowsRecoveredRPC(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	researchRoot(t, w, 100, "/autoresearch interrupted goal")
	oldClient := w.client
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
	w.status()
	waitFixtureRPCTrace(t, trace, "entries_waiting", "")
	requireStoreOK(t, oldClient.TerminateNow())
	// Observe process exit before consuming the outstanding status result.
	w.failed()
	if w.client != oldClient || queueState(t, w, 100) != "submitted" {
		t.Fatal("process exit did not defer retirement until the outstanding RPC returned")
	}
	w.operationReturned(waitOperation(t, w))
	if w.client != nil || queueState(t, w, 100) != "uncertain" {
		t.Fatal("process exit did not retire the interrupted research owner")
	}
	nativeInput(t, w, 101, "/autoresearch off", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	nativeInput(t, w, 102, "/native_local", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
	nativeRPC(t, w, "get_state", nil)
	if w.client == oldClient || researchTraceCount(trace, "prompt", "/native_local") != 1 || researchTraceCount(trace, "prompt", "/autoresearch interrupted goal") != 1 {
		t.Fatal("recovered runtime lost the next RPC or replayed interrupted research")
	}
}

func TestAutoresearchOldStatusCannotRestoreEnabledMode(t *testing.T) {
	w, _, _ := researchWorker(t, "autoresearch")
	nativeInput(t, w, 100, "/autoresearch", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	w.status()
	result := waitOperation(t, w)
	oldClient := w.client
	nativeInput(t, w, 101, "/autoresearch off", false)
	if w.client == oldClient || queueState(t, w, 101) != "submitted" {
		t.Fatal("off did not retire the old runtime before starting asynchronous control")
	}
	before := nativeOutputs(t, w)
	w.operationReturned(result)
	if w.research.enabled || nativeOutputs(t, w) != before {
		t.Fatal("old status result changed the disabled replacement runtime")
	}
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
}

func TestAutoresearchEnabledModeWaitsForDiscoveryBeforeOrdinaryRoot(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_AUTORESEARCH_MODE", "on")
	w, _, trace := nativeWorker(t, "autoresearch-hold")
	waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
	nativeInput(t, w, 100, "restored research prompt", false)
	if queueState(t, w, 100) != "pending" || w.taskActive() || researchTraceCount(trace, "prompt", "restored research prompt") != 0 {
		t.Fatal("enabled research mode dispatched an ordinary root before discovery")
	}
	nativeRPC(t, w, "fixture_catalog_update", nil)
	nativeUntil(t, w, func() bool { return w.research.roundOpen && !w.rpcOperationActive })
	researchRound(t, w, "restored research round")
	if queueState(t, w, 100) != "submitted" || strings.Join(researchReplies(t, w, 100), "") != "restored research round" {
		t.Fatal("discovered research lost its owner or round reply")
	}
}

func TestAutoresearchOrdinaryTextSteersAndNativeQueueStaysDeferred(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	researchRoot(t, w, 100, "/autoresearch improve allocation")
	nativeInput(t, w, 101, "concentrate on parsing", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	nativeInput(t, w, 102, "/native_local", false)
	if w.active != 100 || queueState(t, w, 100) != "submitted" || queueState(t, w, 102) != "pending" {
		t.Fatal("ordinary steer stole root ownership or dispatched native queued work")
	}
	researchRound(t, w, "steered round")
	if w.active != 100 || !w.taskActive() || researchTraceCount(trace, "prompt", "concentrate on parsing") != 1 || researchTraceCount(trace, "prompt", "/native_local") != 0 {
		t.Fatal("steer completion or round yield settled the research root")
	}
}

func TestAutoresearchWatchdogAndIdleReleaseDoNotSettleBetweenRounds(t *testing.T) {
	w, _, _ := researchWorker(t, "autoresearch")
	researchRoot(t, w, 100, "/autoresearch keep researching")
	researchRound(t, w, "first idle interval")
	client := w.client
	w.b.cfg.IdleTimeout = time.Minute
	now := time.Now().Add(10 * time.Minute)
	w.lastActivity = now.Add(-5 * time.Minute)
	w.probeStuckTask(now)
	w.idleProbeFinished(probeResultFor(w), now)
	w.idleProbeFinished(probeResultFor(w), now.Add(time.Minute))
	w.releaseIdleRuntime(now)
	if w.client != client || w.active != 100 || queueState(t, w, 100) != "submitted" || !w.taskActive() {
		t.Fatal("idle evidence settled or released research between native continuation hooks")
	}
	nativeRPC(t, w, "fixture_autoresearch_continue", nil)
	researchRound(t, w, "second independent turn")
	if w.active != 100 || queueState(t, w, 100) != "submitted" {
		t.Fatal("a later autonomous turn lost the root")
	}
}

func TestAutoresearchNativeNotificationsAreBoundedDurableAndScoped(t *testing.T) {
	w, _, _ := researchWorker(t, "autoresearch")
	nativeRPC(t, w, "fixture_autoresearch_notify", map[string]any{"message": "unowned research notice"})
	researchBarrier(t, w)
	if strings.Contains(nativeOutputs(t, w), "unowned research notice") {
		t.Fatal("unowned native notification escaped into Telegram")
	}
	researchRoot(t, w, 100, "/autoresearch notices")
	before := nativeOutputs(t, w)
	notice := "distinct research notification " + strings.Repeat("x", 20000)
	nativeRPC(t, w, "fixture_autoresearch_notify", map[string]any{"message": notice})
	researchBarrier(t, w)
	delivered := strings.TrimPrefix(nativeOutputs(t, w), before)
	found := strings.Contains(delivered, "distinct research notification")
	if len(delivered) >= len(notice) {
		t.Fatal("native notification was not bounded before durable delivery")
	}
	if !found || queueState(t, w, 100) != "submitted" {
		t.Fatal("notification was not durably delivered for active research, or was treated as completion")
	}
	for range 12 {
		nativeRPC(t, w, "fixture_autoresearch_notify", map[string]any{"message": "research-budget-notice"})
	}
	researchBarrier(t, w)
	count := strings.Count(nativeOutputs(t, w), "research-budget-notice")
	if count == 0 || count > 8 {
		t.Fatal("research notification delivery did not enforce a bounded per-root budget")
	}
}

func TestAutoresearchStartupRefusalDoesNotLeaveFakeRoot(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch-refuse")
	nativeInput(t, w, 100, "/autoresearch refused goal", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) != "submitted" && queueState(t, w, 100) != "pending" })
	if w.taskActive() || w.active != 0 || !strings.Contains(nativeOutputs(t, w), "Research startup refused") {
		t.Fatal("native startup refusal hung or discarded its correlated display notice")
	}
	nativeInput(t, w, 101, "after refused research", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	if researchTraceCount(trace, "prompt", "/autoresearch refused goal") != 1 {
		t.Fatal("startup refusal replayed the research goal")
	}
}

func TestAutoresearchMetadataErrorsFailClosed(t *testing.T) {
	for _, mode := range []string{"error", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": mode})
			nativeInput(t, w, 100, "/autoresearch unknown mode", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) != "pending" && queueState(t, w, 100) != "submitted" })
			if w.taskActive() || researchTraceCount(trace, "prompt", "/autoresearch unknown mode") != 0 || strings.Contains(nativeOutputs(t, w), "PRIVATE_AUTORESEARCH_ENTRIES_ERROR") {
				t.Fatal("unknown mode invoked research, leaked provider details, or left fake active ownership")
			}
		})
	}
}

func TestAutoresearchExplicitOffRecoversFromOldMetadataErrors(t *testing.T) {
	for _, mode := range []string{"error", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			researchRoot(t, w, 100, "/autoresearch interrupted goal")
			oldPrompt := waitFixtureRPCTrace(t, trace, "prompt", "/autoresearch interrupted goal")
			nativeInput(t, w, 101, "/followup preserved work", false)
			nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": mode})
			nativeInput(t, w, 102, "/autoresearch off", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			off := waitFixtureRPCTrace(t, trace, "prompt", "/autoresearch off")
			followup := waitFixtureRPCTrace(t, trace, "prompt", "preserved work")
			if queueState(t, w, 100) != "uncertain" || queueState(t, w, 102) != "done" || w.research.disablePending || w.resumeFailed {
				t.Fatal("old metadata failure prevented confirmed off or changed the interrupted root outcome")
			}
			if off.pid == oldPrompt.pid || followup.pid != off.pid || researchTraceCount(trace, "autoresearch_mode", "off") != 1 || researchTraceCount(trace, "prompt", "preserved work") != 1 || researchTraceCount(trace, "prompt", "/autoresearch interrupted goal") != 1 {
				t.Fatal("off did not fence the old runtime before executing the preserved followup exactly once")
			}
		})
	}
}

func TestAutoresearchPendingExplicitOffRemovesOnlyItsOwnQueueSlot(t *testing.T) {
	w, _, trace := nativeWorker(t, "autoresearch-hold")
	waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
	nativeRPC(t, w, "fixture_catalog_update", nil)
	w.commandCatalogUpdated()
	researchRoot(t, w, 100, "/autoresearch original goal")
	oldClient := w.client
	w.client.InvalidateCommandCatalog()
	nativeInput(t, w, 101, "/followup work before off", false)
	nativeInput(t, w, 102, "/autoresearch off", false)
	nativeInput(t, w, 103, "/followup work after off", false)
	if len(w.queue) != 3 || queueState(t, w, 102) != "pending" {
		t.Fatal("off did not wait for discovery between the preserved queue entries")
	}
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "error"})
	// The old discovery stays held; the replacement runtime can discover normally.
	t.Setenv("OMP_TELEGRAM_FIXTURE_NATIVE_COMMANDS", "autoresearch")
	nativeRPC(t, w, "fixture_catalog_update", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
	nativeUntil(t, w, func() bool { return queueState(t, w, 103) == "done" })
	if w.client == oldClient || queueState(t, w, 100) != "uncertain" || queueState(t, w, 101) != "done" || len(w.queue) != 0 {
		t.Fatal("pending off lost another queue entry or retained itself as followup work")
	}
	var prompts []string
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && entry.payload != "/session info" {
			prompts = append(prompts, entry.payload)
		}
	}
	if strings.Join(prompts, "\n") != "/autoresearch original goal\n/autoresearch off\nwork before off\nwork after off" {
		t.Fatalf("pending off replayed research/control or changed the preserved queue order: %q", prompts)
	}
}

func TestAutoresearchIdleExplicitOffAdmitsOrdinaryWork(t *testing.T) {
	for _, mode := range []string{"on", "off"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_AUTORESEARCH_MODE", mode)
			w, _, trace := researchWorker(t, "autoresearch")
			nativeInput(t, w, 100, "/autoresearch off", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			if w.taskActive() || !w.research.known || w.research.enabled || researchTraceCount(trace, "autoresearch_mode", "off") != 1 {
				t.Fatal("idle explicit off failed to confirm the disabled native mode")
			}
			nativeInput(t, w, 101, "ordinary work after idle off", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			if researchTraceCount(trace, "prompt", "ordinary work after idle off") != 1 {
				t.Fatal("confirmed idle off failed to admit ordinary work exactly once")
			}
		})
	}
}

func TestAutoresearchPostOffModeFailurePausesPreservedWork(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	researchRoot(t, w, 100, "/autoresearch unconfirmed shutdown")
	nativeInput(t, w, 101, "/followup must stay paused", false)
	// The fresh runtime executes off, but its native confirmation query fails.
	t.Setenv("OMP_TELEGRAM_FIXTURE_NATIVE_COMMANDS", "autoresearch-entries-error")
	nativeInput(t, w, 102, "/autoresearch off", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "uncertain" && !w.controlInProgress() })
	if queueState(t, w, 100) != "uncertain" || queueState(t, w, 102) != "uncertain" || !w.resumeFailed || !w.research.disablePending || w.client != nil {
		t.Fatal("post-off mode lookup failure did not leave shutdown uncertain and the runtime closed")
	}
	if queueState(t, w, 101) != "pending" || researchTraceCount(trace, "prompt", "must stay paused") != 0 || researchTraceCount(trace, "prompt", "/autoresearch off") != 1 || researchTraceCount(trace, "autoresearch_mode", "off") != 1 {
		t.Fatal("post-off confirmation failure dispatched preserved work or failed before native off executed")
	}
}

func TestAutoresearchStopAndOffRetireBeforeQueuedWork(t *testing.T) {
	for _, control := range []string{"stop", "off", "progress"} {
		t.Run(control, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			researchRoot(t, w, 100, "/autoresearch continuous goal")
			oldClient := w.client
			oldPrompt := waitFixtureRPCTrace(t, trace, "prompt", "/autoresearch continuous goal")
			nativeInput(t, w, 101, "/followup preserved work", false)
			switch control {
			case "stop":
				nativeInput(t, w, 102, "/stop", false)
			case "off":
				nativeInput(t, w, 102, "/autoresearch off", false)
			case "progress":
				w.b.cfg.ProgressMode = "summary"
				w.previewResult = make(chan previewResult, 1)
				allowInitialProgress(w)
				w.flushPreview()
				select {
				case result := <-w.previewResult:
					if result.err != nil || result.id == 0 {
						t.Fatalf("progress creation failed: %+v", result)
					}
					w.previewFinished(result)
				case <-time.After(5 * time.Second):
					t.Fatal("research progress omitted Stop")
				}
				f := http.DefaultTransport.(*fakeHTTP)
				clickKeyboard(w, 7, f.button(f.messageCount()-1))
				w.dispatch()
			}
			if control == "stop" {
				nativeUntil(t, w, func() bool { return !w.controlInProgress() && !w.taskActive() })
				if queueState(t, w, 101) != "cancelled" || researchTraceCount(trace, "prompt", "preserved work") != 0 {
					t.Fatal("/stop dispatched rather than cancelled pending work")
				}
			} else {
				nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
				queued := waitFixtureRPCTrace(t, trace, "prompt", "preserved work")
				off := waitFixtureRPCTrace(t, trace, "prompt", "/autoresearch off")
				if queued.pid == oldPrompt.pid || off.pid == oldPrompt.pid || off.pid != queued.pid {
					t.Fatal("preserved work ran before mode-off on a freshly resumed runtime")
				}
				seenOff := false
				for _, entry := range fixtureRPCTrace(trace) {
					if entry.kind == "autoresearch_mode" && entry.payload == "off" && entry.pid == queued.pid {
						seenOff = true
					}
					if entry.kind == "prompt" && entry.payload == "preserved work" && !seenOff {
						t.Fatal("preserved work reached RPC before native mode was disabled")
					}
				}
			}
			select {
			case <-oldClient.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("old autonomous process survived Stop/off")
			}
			if queueState(t, w, 100) != "uncertain" || researchTraceCount(trace, "prompt", "/autoresearch continuous goal") != 1 {
				t.Fatal("forced-interrupted research was claimed complete or automatically replayed")
			}
		})
	}
}

func TestAutoresearchOffFailureNeverAutomaticallyDispatchesPreservedQueue(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch-off-error")
	researchRoot(t, w, 100, "/autoresearch unsafe off")
	oldClient := w.client
	nativeInput(t, w, 101, "/followup must remain deferred", false)
	nativeInput(t, w, 102, "/autoresearch off", false)
	nativeUntil(t, w, func() bool { return !w.controlInProgress() && queueState(t, w, 100) != "submitted" })
	for range 3 {
		w.dispatch()
	}
	if queueState(t, w, 101) != "pending" || researchTraceCount(trace, "prompt", "must remain deferred") != 0 || strings.Contains(nativeOutputs(t, w), "PRIVATE_AUTORESEARCH_OFF_ERROR") {
		t.Fatal("unconfirmed off automatically dispatched preserved work or leaked diagnostics")
	}
	select {
	case <-oldClient.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("failed off retained the old research process")
	}
}

func TestAutoresearchClearRequiresIdleAuthorizedFencedConfirmation(t *testing.T) {
	for _, scenario := range []string{"approve", "wrong-user", "wrong-message", "wrong-topic", "stale-generation", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeInput(t, w, 100, "/autoresearch clear --force --keep-tree", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			f := http.DefaultTransport.(*fakeHTTP)
			buttons := resumeButtons(t, f)
			if len(buttons) < 2 || researchTraceCount(trace, "autoresearch_clear", "clear --force --keep-tree") != 0 {
				t.Fatal("destructive clear executed before confirmation")
			}
			user, messageID := int64(7), int64(f.messageCount())
			data := buttons[0]["callback_data"].(string)
			message := &telegram.Message{MessageID: messageID, MessageThreadID: w.key.thread, Chat: telegram.Chat{ID: w.key.chat}}
			switch scenario {
			case "wrong-user":
				user = 8
			case "wrong-message":
				message.MessageID++
			case "wrong-topic":
				message.MessageThreadID++
			case "stale-generation":
				w.binding.Generation++
			case "cancel":
				data = buttons[1]["callback_data"].(string)
			}
			w.callback(&telegram.CallbackQuery{ID: "research-clear", From: telegram.User{ID: user}, Message: message, Data: data})
			if scenario == "approve" {
				nativeUntil(t, w, func() bool {
					return researchTraceCount(trace, "autoresearch_clear", "clear --force --keep-tree") == 1 && !w.controlInProgress() && !w.taskActive()
				})
			} else {
				nativeRPC(t, w, "get_state", nil)
				if researchTraceCount(trace, "autoresearch_clear", "clear --force --keep-tree") != 0 {
					t.Fatal("unauthorized, stale, or cancelled confirmation executed destructive clear")
				}
			}
		})
	}
}

func TestAutoresearchClearRejectsScopeChangeDuringFinalIdleCheck(t *testing.T) {
	for _, change := range []struct {
		name   string
		fields map[string]any
	}{
		{"source-revoked", map[string]any{"nth": 3, "sourceName": "autoresearch", "source": "unknown"}},
		{"session-changed", map[string]any{"nth": 3, "newSession": true}},
	} {
		t.Run(change.name, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_AUTORESEARCH_MODE", "on")
			w, _, trace := researchWorker(t, "autoresearch")
			const text = "/autoresearch clear --force --keep-tree"
			nativeInput(t, w, 100, text, false)
			researchBarrier(t, w)
			f := http.DefaultTransport.(*fakeHTTP)
			buttons := resumeButtons(t, f)
			if len(buttons) < 2 || len(w.confirms) != 1 {
				t.Fatal("clear did not create its authorized confirmation")
			}
			owner, err := w.currentWorkspaceOwner()
			requireStoreOK(t, err)
			owned, err := w.b.db.ResearchWorkspaceOwned(owner)
			requireStoreOK(t, err)
			if !owned {
				t.Fatal("enabled research had no lease before clear confirmation")
			}
			// The mode lookup reads state twice before the final idle query.
			nativeRPC(t, w, "fixture_get_state_update", change.fields)
			w.callback(&telegram.CallbackQuery{
				ID:   "research-clear-final-state",
				From: telegram.User{ID: 7},
				Message: &telegram.Message{
					MessageID:       int64(f.messageCount()),
					MessageThreadID: w.key.thread,
					Chat:            telegram.Chat{ID: w.key.chat},
				},
				Data: buttons[0]["callback_data"].(string),
			})
			nativeUntil(t, w, func() bool { return !w.controlInProgress() })
			if researchTraceCount(trace, "get_state_update", "") != 1 {
				t.Fatal("clear did not reach the configured final-state scope change")
			}
			if researchTraceCount(trace, "prompt", text) != 0 || researchTraceCount(trace, "autoresearch_clear", "clear --force --keep-tree") != 0 || researchTraceCount(trace, "autoresearch_mode", "off") != 0 {
				t.Fatal("stale clear confirmation sent or replayed destructive native control")
			}
			if len(w.confirms) != 0 || w.client != nil || !w.resumeFailed || !w.research.disablePending {
				t.Fatal("scope-invalid clear confirmation was not consumed and canceled")
			}
			owned, err = w.b.db.ResearchWorkspaceOwned(owner)
			requireStoreOK(t, err)
			if !owned {
				t.Fatal("unconfirmed clear released the original research lease")
			}
		})
	}
}

func TestAutoresearchOffRejectsScopeChangeDuringFinalIdleCheck(t *testing.T) {
	for _, change := range []struct {
		name   string
		fields map[string]any
	}{
		{"source-revoked", map[string]any{"nth": 2, "sourceName": "autoresearch", "source": "unknown"}},
		{"session-changed", map[string]any{"nth": 2, "newSession": true}},
	} {
		t.Run(change.name, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeInput(t, w, 100, "/autoresearch", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			researchBarrier(t, w)
			owner, err := w.currentWorkspaceOwner()
			requireStoreOK(t, err)
			owned, err := w.b.db.ResearchWorkspaceOwned(owner)
			requireStoreOK(t, err)
			if !owned {
				t.Fatal("enabled idle research had no lease before off")
			}
			// Exact-owner runtime restoration checks state once before the final
			// idle check. The fixture must not invalidate that earlier check.
			nativeRPC(t, w, "fixture_get_state_update", change.fields)
			nativeInput(t, w, 101, "/autoresearch off", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "uncertain" && !w.controlInProgress() })
			if researchTraceCount(trace, "get_state_update", "") != 1 {
				t.Fatal("off did not reach the configured final-state scope change")
			}
			if researchTraceCount(trace, "prompt", "/autoresearch off") != 0 || researchTraceCount(trace, "autoresearch_mode", "off") != 0 {
				t.Fatal("scope-invalid off sent or automatically replayed native control")
			}
			if queueState(t, w, 101) != "uncertain" || !w.research.disablePending || !w.resumeFailed {
				t.Fatal("rejected off bypassed the existing unconfirmed-disable failure policy")
			}
			owned, err = w.b.db.ResearchWorkspaceOwned(owner)
			requireStoreOK(t, err)
			if !owned {
				t.Fatal("unconfirmed off released the original research lease")
			}
		})
	}
}

func TestAutoresearchClearCannotBypassConfirmationThroughRawQueue(t *testing.T) {
	for _, kind := range []string{"followup", "attachment"} {
		t.Run(kind, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeQueuedRawInput(t, w, 100, "/autoresearch clear --force", kind)
			w.dispatch()
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) != "pending" })
			if researchTraceCount(trace, "autoresearch_clear", "clear --force") != 0 || researchTraceCount(trace, "prompt", "/autoresearch clear --force") != 0 || w.taskActive() {
				t.Fatal("raw followup/attachment bypassed destructive clear confirmation")
			}
		})
	}
}

func TestAutoresearchBusyClearCannotInterruptOrQueueDestruction(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	researchRoot(t, w, 100, "/autoresearch active goal")
	nativeInput(t, w, 101, "/autoresearch clear --force", false)
	if w.active != 100 || queueState(t, w, 100) != "submitted" || queueState(t, w, 101) == "pending" || queueState(t, w, 101) == "submitted" || len(w.confirms) != 0 || researchTraceCount(trace, "autoresearch_clear", "clear --force") != 0 {
		t.Fatal("busy clear interrupted active research or retained deferred destructive work")
	}
}

func TestAutoresearchCloseAndStaleOperationCannotAffectReplacement(t *testing.T) {
	w, command, trace := researchWorker(t, "autoresearch")
	researchRoot(t, w, 100, "/autoresearch old root")
	old := operationResult{kind: "prompt", clientID: w.client.ID(), generation: w.binding.Generation, turn: w.turn, active: w.active, data: json.RawMessage(`{"agentInvoked":true}`)}
	oldClient := w.client
	// Closing unknown/on research deliberately retains its durable lease. A
	// replacement session requires confirmed off, not just runtime shutdown.
	nativeInput(t, w, 102, "/autoresearch off", false)
	nativeUntil(t, w, func() bool { return !w.taskActive() && !w.controlInProgress() })
	command("/close")
	command("/new " + t.TempDir())
	nativeCatalogReady(t, w)
	nativeInput(t, w, 101, "new non-research question", false)
	w.operationReturned(old)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	if w.taskActive() || w.client == oldClient || researchTraceCount(trace, "prompt", "/autoresearch old root") != 1 || strings.Contains(strings.Join(researchReplies(t, w, 101), "\n"), "Research fixture") {
		t.Fatal("old research state or operation contaminated replacement generation")
	}
}

func TestAutoresearchEnabledEmptyToggleAndIdleOffCompleteLocally(t *testing.T) {
	for _, text := range []string{"/autoresearch", "/autoresearch off"} {
		t.Run(text, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_AUTORESEARCH_MODE", "on")
			w, _, trace := researchWorker(t, "autoresearch")
			nativeInput(t, w, 100, text, false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			if w.taskActive() || researchTraceCount(trace, "autoresearch_mode", "off") != 1 {
				t.Fatal("idle off or enabled empty toggle retained a model root")
			}
			nativeInput(t, w, 101, "ordinary prompt after native off", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			if w.taskActive() {
				t.Fatal("disabled mode turned the following ordinary prompt into research")
			}
		})
	}
}

func TestAutoresearchSourceIsRecheckedForQueuedNativeAndRawInputs(t *testing.T) {
	for _, kind := range []string{"native", "followup", "attachment"} {
		t.Run(kind, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			nativeInput(t, w, 100, "wait", false)
			researchBarrier(t, w)
			if kind == "native" {
				nativeInput(t, w, 101, "/autoresearch queued goal", false)
			} else {
				nativeQueuedRawInput(t, w, 101, "/autoresearch queued goal", kind)
			}
			nativeRPC(t, w, "fixture_catalog_update", map[string]any{"sourceName": "autoresearch", "source": "unknown"})
			nativeRPC(t, w, "fixture_finish_root", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "cancelled" && !w.taskActive() })
			if researchTraceCount(trace, "prompt", "/autoresearch queued goal") != 0 || w.taskActive() {
				t.Fatal("queued autoresearch ignored its changed catalog source")
			}
		})
	}
}

func TestAutoresearchCorrelatedStartupErrorRetiresWithoutFakeRoot(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch-start-error")
	client := w.client
	nativeInput(t, w, 100, "/autoresearch erroring goal", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) != "pending" && queueState(t, w, 100) != "submitted" })
	if w.taskActive() || w.active != 0 || strings.Contains(nativeOutputs(t, w), "PRIVATE_AUTORESEARCH_START_ERROR") || researchTraceCount(trace, "prompt", "/autoresearch erroring goal") != 1 {
		t.Fatal("startup error hung an active root, leaked diagnostics, or replayed the goal")
	}
	select {
	case <-client.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("uncertain startup error did not retire its process")
	}
}

func TestAutoresearchRoundRepliesStayBoundedAcrossLongRuns(t *testing.T) {
	w, _, _ := researchWorker(t, "autoresearch")
	researchRoot(t, w, 100, "/autoresearch long run")
	for round := range 4 {
		if round != 0 {
			nativeRPC(t, w, "fixture_autoresearch_continue", nil)
		}
		researchRound(t, w, strings.Repeat("bounded research output ", 8192))
		if w.preview != "" || w.stream.Len() != 0 || w.finalAssistantBytes != 0 || len(w.finalAssistantTexts) != 0 || queueState(t, w, 100) != "submitted" {
			t.Fatal("long research accumulated previous round buffers or finalized the root")
		}
	}
	var largest int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT coalesce(max(length(text)),0) FROM outbox WHERE inbox_id=100").Scan(&largest))
	if largest == 0 || largest > maxFinalReplyBytes {
		t.Fatal("round output was not delivered with bounded durable chunks")
	}
}

func TestAutoresearchStartupRecoveryNeverReplaysActiveOrPendingResearch(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_NATIVE_COMMANDS", "autoresearch")
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
	trace := filepath.Join(t.TempDir(), "rpc.trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	d := newRecoveryDaemon(t, 1)
	d.command(11, "/new "+t.TempDir())
	root := d.send(11, "/autoresearch interrupted research goal")
	waitFixtureRPCTrace(t, trace, "prompt", "/autoresearch interrupted research goal")
	waitFixtureRPCTrace(t, trace, "autoresearch_mode", "on")
	queued := d.send(11, "/followup cancelled research followup")
	waitFor(t, func() bool {
		var state string
		return d.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", queued).Scan(&state) == nil && state == "pending"
	})
	d.stop()
	d.start()
	d.command(11, "/status")
	if d.state(root) != "uncertain" || d.state(queued) != "cancelled" || researchTraceCount(trace, "prompt", "/autoresearch interrupted research goal") != 1 || researchTraceCount(trace, "prompt", "cancelled research followup") != 0 {
		t.Fatal("startup recovery replayed uncertain research or pending followup work")
	}
	// This is the first input to the released runtime: native discovery must
	// classify the control before it can be rejected or executed.
	off := d.send(11, "/autoresearch off")
	var offState string
	waitFor(t, func() bool {
		return d.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", off).Scan(&offState) == nil && offState != "pending" && offState != "submitted"
	})
	if offState != "done" || researchTraceCount(trace, "prompt", "/autoresearch off") != 1 || researchTraceCount(trace, "autoresearch_mode", "off") != 1 {
		t.Fatalf("recovered cold runtime did not confirm native off: inbox state=%q", offState)
	}
	d.command(11, "new work after recovered off")
	if researchTraceCount(trace, "prompt", "new work after recovered off") != 1 || researchTraceCount(trace, "prompt", "/autoresearch interrupted research goal") != 1 {
		t.Fatal("post-recovery off failed to disable mode before fresh work or replayed the old root")
	}
}
