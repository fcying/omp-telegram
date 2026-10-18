package bridge

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func releasedIdleWorker(t *testing.T) (*worker, *fakeHTTP, func(string), store.Binding) {
	t.Helper()
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.IdleTimeout = time.Minute
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = w.b.db.MarkOutput(ready.ID, "done"); err != nil {
		t.Fatal(err)
	}
	before := w.binding
	if w.client == nil || !before.Running || w.runtime != runtimeConnected {
		t.Fatalf("initial runtime = client:%t binding:%+v runtime:%d", w.client != nil, before, w.runtime)
	}
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	w.releaseIdleRuntime(time.Now())
	if w.client != nil || w.runtime != runtimeReleased || !sameBindingIdentity(w.binding, before) {
		t.Fatalf("idle release changed logical binding or retained runtime: binding=%+v runtime=%d client=%t", w.binding, w.runtime, w.client != nil)
	}
	if !w.b.sessionInUse(w.sessionID) {
		t.Fatal("idle release surrendered the logical session claim")
	}
	return w, f, command, before
}
func TestCompactResumesReleasedSession(t *testing.T) {
	w, f, command, before := releasedIdleWorker(t)
	w.operations = make(chan operationResult, 1)
	command("/compact")
	messageID := f.messageCount()
	if w.client == nil || w.runtime != runtimeConnected || !sameBindingIdentity(w.binding, before) {
		t.Fatal("compact did not restore the released session")
	}
	w.operationReturned(waitOperation(t, w))
	if w.sessionOperationActive() || !sameBindingIdentity(w.binding, before) {
		t.Fatal("compaction changed the restored session or left it busy")
	}
	assertFinishedMenu(t, f, messageID, "Compaction completed.")
}

func TestCompactReportsResumeFailureInsteadOfBusy(t *testing.T) {
	w, _, command, before := releasedIdleWorker(t)
	w.b.cfg.OMP = t.TempDir() + "/missing-omp"
	command("/compact")
	if w.client != nil || w.sessionOperationActive() || !sameBindingIdentity(w.binding, before) {
		t.Fatal("failed resume changed the saved session or began compaction")
	}
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Failed to resume") || strings.Contains(text, "idle") {
		t.Fatalf("runtime startup failure was misreported as busy: %q", text)
	}
}

func TestDisabledIdleTimeoutNeverReleasesRuntime(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	w.lastActivity = time.Now().Add(-24 * time.Hour)
	w.releaseIdleRuntime(time.Now())
	if w.client == nil || w.runtime != runtimeConnected {
		t.Fatal("disabled idle timeout released the connected runtime")
	}
}

func TestIdleReleaseRequiresDurableSessionFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *worker)
	}{
		{name: "missing", setup: func(t *testing.T, w *worker) {
			if err := os.Remove(w.binding.Session); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory", setup: func(t *testing.T, w *worker) {
			w.binding.Session = w.binding.Workspace
		}},
		{name: "relative", setup: func(t *testing.T, w *worker) {
			w.binding.Session = "session.jsonl"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, command := setupWorkspaceWorker(t)
			w.b.cfg.IdleTimeout = time.Minute
			command("/new " + t.TempDir())
			if w.client == nil || w.runtime != runtimeConnected {
				t.Fatal("fixture did not start a connected runtime")
			}
			tc.setup(t, w)
			w.lastActivity = time.Now().Add(-2 * time.Minute)
			w.releaseIdleRuntime(time.Now())
			if w.client == nil || w.runtime != runtimeConnected || !w.binding.Running {
				t.Fatalf("idle release discarded non-durable session: runtime=%d client=%t running=%t", w.runtime, w.client != nil, w.binding.Running)
			}
		})
	}
}

func TestIdleReleaseAfterSessionFileAppears(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.IdleTimeout = time.Minute
	command("/new " + t.TempDir())
	if w.client == nil || !w.binding.Running {
		t.Fatal("fixture did not start a running runtime")
	}
	path := w.binding.Session
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	w.releaseIdleRuntime(time.Now())
	if w.client == nil || w.runtime != runtimeConnected || !w.binding.Running {
		t.Fatal("missing session file released the runtime")
	}
	if err := os.WriteFile(path, []byte("session history"), 0600); err != nil {
		t.Fatal(err)
	}
	w.releaseIdleRuntime(time.Now())
	if w.client != nil || w.runtime != runtimeReleased || !w.binding.Running {
		t.Fatalf("durable session was not released: runtime=%d client=%t running=%t", w.runtime, w.client != nil, w.binding.Running)
	}
}

func drainStatusOutput(t *testing.T, w *worker) {
	t.Helper()
	nativeRPC(t, w, "get_state", nil)
	client := w.client
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for client.BufferedOutput() {
		select {
		case raw, open := <-client.Events():
			if !open {
				t.Fatal("native event stream closed while preparing an idle status query")
			}
			w.event(raw)
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("native status output did not drain")
		}
	}
}

func TestReleasedStatusQueriesSavedSessionAndReleasesRuntime(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_CONFIGURED_THINKING", "auto")
	w, _, _, before := releasedIdleWorker(t)
	w.status()
	client := w.client
	if client == nil || !w.controlInProgress() {
		t.Fatal("released status did not start a temporary runtime query")
	}
	w.finishPendingStatusRelease()
	result := waitOperation(t, w)
	requireStoreOK(t, result.err)
	drainStatusOutput(t, w)
	w.operationReturned(result)
	if w.client != nil || w.runtime != runtimeReleased || len(w.b.slots) != 0 || !sameBindingIdentity(w.binding, before) {
		t.Fatal("status retained its temporary process or changed the logical session")
	}
	select {
	case <-client.Done():
	default:
		t.Fatal("temporary status process remained alive")
	}
	out, err := w.b.db.NextOutput()
	requireStoreOK(t, err)
	fields := statusFields(out.Text)
	if fields["Model"] != "fixture/safe" || fields["Thinking"] != "auto" || fields["OMP"] != "released" {
		t.Fatalf("status lost configured thinking while releasing its temporary runtime: %v", fields)
	}
	var native struct {
		Prompts int `json:"fixtureRootPrompts"`
	}
	if json.Unmarshal(result.data, &native) != nil || native.Prompts != 0 {
		t.Fatal("status submitted or replayed model work")
	}
	if strings.Contains(out.Text, "SECRET") || strings.Contains(out.Text, "PRIVATE") {
		t.Fatal("temporary status leaked non-whitelisted state")
	}
}

func TestReleasedStatusPreservesRuntimeForIncomingPrompt(t *testing.T) {
	w, _, _, before := releasedIdleWorker(t)
	w.status()
	client := w.client
	if client == nil {
		t.Fatal("status did not restore the saved session")
	}
	result := waitOperation(t, w)
	requireStoreOK(t, result.err)
	nativeInput(t, w, 100, "work received during status", false)
	w.operationReturned(result)
	if w.client != client || len(w.queue) != 1 || !sameBindingIdentity(w.binding, before) {
		t.Fatal("status discarded incoming work or its runtime")
	}
	w.dispatch()
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	if !strings.Contains(nativeOutputs(t, w), "answer: work received during status") || w.client != client {
		t.Fatal("incoming prompt did not complete on the retained runtime")
	}
}

func TestReleasedStatusRetainsUnsettledOrUnknownRuntime(t *testing.T) {
	for _, state := range []string{"pending", "unknown"} {
		t.Run(state, func(t *testing.T) {
			w, _, command, before := releasedIdleWorker(t)
			t.Setenv("OMP_TELEGRAM_FIXTURE_ABORT_STATE", state)
			command("/status")
			if w.client == nil || w.runtime != runtimeConnected || !sameBindingIdentity(w.binding, before) {
				t.Fatal("status discarded native work or unconfirmed settlement")
			}
		})
	}
}

func TestReleasedStatusQueryFailureReleasesTemporaryRuntime(t *testing.T) {
	w, _, _, before := releasedIdleWorker(t)
	w.status()
	client := w.client
	if client == nil {
		t.Fatal("status did not restore the saved session")
	}
	result := waitOperation(t, w)
	drainStatusOutput(t, w)
	result.err = context.DeadlineExceeded
	w.operationReturned(result)
	if w.client != nil || len(w.b.slots) != 0 || !sameBindingIdentity(w.binding, before) {
		t.Fatal("failed status query leaked a process slot or changed session identity")
	}
	select {
	case <-client.Done():
	default:
		t.Fatal("failed temporary status process remained alive")
	}
}

func TestReleasedStatusPreservesQueuedNativeDialog(t *testing.T) {
	for _, readError := range []bool{false, true} {
		name := "success"
		if readError {
			name = "read-error"
		}
		t.Run(name, func(t *testing.T) {
			w, f, _, before := releasedIdleWorker(t)
			w.owner = 7
			w.status()
			client := w.client
			if client == nil {
				t.Fatal("status did not restore the saved session")
			}
			result := waitOperation(t, w)
			requireStoreOK(t, result.err)
			// The response is a barrier after the native dialog entered the reader queue.
			nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
				"type": "extension_ui_request", "id": "status-startup-dialog", "method": "select",
				"title": "Startup permission", "options": []string{"Allow", "Deny"},
			}})
			if readError {
				result.err = context.DeadlineExceeded
			}
			w.operationReturned(result)
			if w.client != client || len(w.b.slots) != 1 || !sameBindingIdentity(w.binding, before) {
				t.Fatal("status retired the runtime with a queued native dialog")
			}
			nativeUntil(t, w, func() bool { return len(w.confirms) == 1 })
			_, rows := pickerView(t, f)
			w.callback(&telegram.CallbackQuery{
				ID: "status-startup-dialog", From: telegram.User{ID: 7},
				Message: &telegram.Message{MessageID: int64(f.messageCount())}, Data: rows[0][0]["callback_data"].(string),
			})
			raw, err := client.Call(w.ctx, "get_state", nil)
			requireStoreOK(t, err)
			var state struct {
				Replies int    `json:"fixtureUIReplies"`
				Value   string `json:"fixtureUIValue"`
			}
			requireStoreOK(t, json.Unmarshal(raw, &state))
			if state.Replies != 1 || state.Value != "Allow" || len(w.confirms) != 0 {
				t.Fatal("the preserved dialog did not deliver the authorized choice")
			}
			w.lastActivity = time.Now().Add(-2 * time.Minute)
			w.releaseIdleRuntime(time.Now())
			if w.client != nil || len(w.b.slots) != 0 || !sameBindingIdentity(w.binding, before) {
				t.Fatal("the completed dialog prevented later idle release")
			}
		})
	}
}

func TestReleasedStatusDefersReleaseUntilMetadataDrains(t *testing.T) {
	for _, newPrompt := range []bool{false, true} {
		name := "metadata-only"
		if newPrompt {
			name = "new-prompt"
		}
		t.Run(name, func(t *testing.T) {
			w, _, _, before := releasedIdleWorker(t)
			w.status()
			client := w.client
			if client == nil {
				t.Fatal("status did not restore the saved session")
			}
			result := waitOperation(t, w)
			requireStoreOK(t, result.err)
			for i := range 33 {
				event := map[string]any{"type": "session_info_update", "sessionId": before.SessionID, "sessionFile": before.Session}
				switch i % 3 {
				case 1:
					event = map[string]any{"type": "advisor_cost_changed"}
				case 2:
					event = map[string]any{"type": "extension_ui_request", "method": "setWidget"}
				}
				nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": event})
			}
			w.operationReturned(result)
			if w.client != client || w.statusReleaseClientID != client.ID() {
				t.Fatal("status did not defer cleanup while native metadata was buffered")
			}
			w.lastActivity = time.Now().Add(-2 * time.Minute)
			w.releaseIdleRuntime(time.Now())
			if w.client != client {
				t.Fatal("idle cleanup bypassed the pending status output guard")
			}
			if newPrompt {
				nativeInput(t, w, 100, "work after a deferred status release", false)
				nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
				w.finishPendingStatusRelease()
				if w.client != client || !strings.Contains(nativeOutputs(t, w), "answer: work after a deferred status release") {
					t.Fatal("deferred status cleanup retired the runtime used by new work")
				}
			} else {
				drainStatusOutput(t, w)
				w.finishPendingStatusRelease()
				if w.client != nil || len(w.b.slots) != 0 {
					t.Fatal("drained metadata retained the temporary status runtime")
				}
			}
			if !sameBindingIdentity(w.binding, before) {
				t.Fatal("deferred status cleanup changed the session identity")
			}
		})
	}
}

func TestReleasedStatusCannotRetireNativeWorkStartedBeforeResult(t *testing.T) {
	for _, readError := range []bool{false, true} {
		name := "success"
		if readError {
			name = "read-error"
		}
		t.Run(name, func(t *testing.T) {
			w, _, _, before := releasedIdleWorker(t)
			w.status()
			client := w.client
			if client == nil {
				t.Fatal("status did not restore the saved session")
			}
			w.finishPendingStatusRelease()
			result := waitOperation(t, w)
			requireStoreOK(t, result.err)
			idle, settled, pending := rpcIdleState(result.data)
			if !idle || !settled || pending {
				t.Fatal("the original snapshot was not idle")
			}
			// Native-owned work has no bridge root. Consume its start before the old result.
			nativeRPC(t, w, "prompt", map[string]any{"message": "wait"})
			drainStatusOutput(t, w)
			if readError {
				result.err = context.DeadlineExceeded
			}
			w.operationReturned(result)
			w.finishPendingStatusRelease()
			if w.client != client || len(w.b.slots) != 1 || !sameBindingIdentity(w.binding, before) {
				t.Fatal("the old status result retired new native work")
			}
			raw, err := client.Call(w.ctx, "get_state", nil)
			requireStoreOK(t, err)
			var current struct {
				Streaming bool `json:"isStreaming"`
			}
			requireStoreOK(t, json.Unmarshal(raw, &current))
			if !current.Streaming {
				t.Fatal("status interrupted the native-owned turn")
			}
		})
	}
}

func TestReleasedStatusCanReleaseAfterConsumedStartupMetadata(t *testing.T) {
	w, _, _, before := releasedIdleWorker(t)
	w.status()
	client := w.client
	if client == nil {
		t.Fatal("status did not restore the saved session")
	}
	w.finishPendingStatusRelease()
	result := waitOperation(t, w)
	requireStoreOK(t, result.err)
	for _, event := range []map[string]any{
		{"type": "session_info_update", "sessionId": before.SessionID, "sessionFile": before.Session},
		{"type": "advisor_cost_changed"},
		{"type": "extension_ui_request", "method": "setWidget"},
	} {
		nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": event})
	}
	drainStatusOutput(t, w)
	w.operationReturned(result)
	if w.client != nil || len(w.b.slots) != 0 || !sameBindingIdentity(w.binding, before) {
		t.Fatal("consumed startup metadata prevented status cleanup")
	}
}

func TestReleasedRuntimeIgnoresStaleExitSignal(t *testing.T) {
	w, _, _, before := releasedIdleWorker(t)
	w.failed()
	if w.client != nil || w.runtime != runtimeReleased || !sameBindingIdentity(w.binding, before) {
		t.Fatalf("stale exit changed released logical session: %+v", w.binding)
	}
}

func TestSuccessfulNativeCallRefreshesIdleActivity(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	if _, err := w.call("get_state", nil); err != nil {
		t.Fatal(err)
	}
	if time.Since(w.lastActivity) > time.Second {
		t.Fatalf("successful native call did not refresh activity: %v", w.lastActivity)
	}
}

func TestRejectedNativeCallDoesNotDelayIdleRelease(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	w.b.cfg.IdleTimeout = time.Minute
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	for range 2 {
		if _, err := w.call("set_session_name", map[string]any{"name": ""}); err == nil {
			t.Fatal("native request unexpectedly accepted")
		}
	}
	w.releaseIdleRuntime(time.Now())
	if w.client != nil || w.runtime != runtimeReleased || !w.binding.Running {
		t.Fatal("rejected RPC delayed idle release or closed logical session")
	}
}

func TestCloseReleasedRuntimeDoesNotResume(t *testing.T) {
	w, _, command, _ := releasedIdleWorker(t)
	command("/close")
	if w.client != nil || w.runtime != runtimeReleased || w.binding.Running || w.b.sessionInUse(w.sessionID) {
		t.Fatalf("close after release woke or retained the session: binding=%+v runtime=%d client=%t", w.binding, w.runtime, w.client != nil)
	}
}

func TestStopReleasedRuntimeDoesNotResume(t *testing.T) {
	w, _, command, _ := releasedIdleWorker(t)
	w.queue = []queued{{}}
	command("/stop")
	if w.client != nil || w.runtime != runtimeReleased || len(w.queue) != 0 {
		t.Fatal("stop after release woke the runtime or retained queued work")
	}
	out, err := w.b.db.NextOutput()
	if err != nil || out.Text != "No active task. Queued prompts cleared." {
		t.Fatalf("released stop reply = %+v, error %v", out, err)
	}
}
func TestReleasedRuntimeResumesBeforeSubmittingPrompt(t *testing.T) {
	w, _, command, before := releasedIdleWorker(t)
	command("after idle")
	if w.client == nil || w.runtime != runtimeConnected || !sameBindingIdentity(w.binding, before) {
		out, _ := w.b.db.NextOutput()
		t.Fatalf("lazy resume changed logical binding or did not connect: binding=%+v runtime=%d client=%t session=%q output=%+v", w.binding, w.runtime, w.client != nil, w.sessionID, out)
	}
	w.dispatch()
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&state); err != nil || state != "submitted" {
		t.Fatalf("lazy-resumed prompt state = %q, error %v", state, err)
	}
}

func TestNewReplacesReleasedLogicalSessionAfterConfirmation(t *testing.T) {
	w, f, command, before := releasedIdleWorker(t)
	workspace := t.TempDir()
	command("/new " + workspace)
	if w.client != nil || w.runtime != runtimeReleased || len(w.confirms) != 1 {
		t.Fatal("new on released runtime did not wait for confirmation")
	}
	clickKeyboard(w, 7, resumeButtons(t, f)[0]["callback_data"].(string))
	if w.client == nil || w.runtime != runtimeConnected || w.binding.Workspace != workspace || w.binding.Generation <= before.Generation {
		t.Fatalf("new did not replace released session: %+v", w.binding)
	}
}

func TestExplicitResumeReplacesReleasedLogicalSession(t *testing.T) {
	w, _, command, before := releasedIdleWorker(t)
	command("/resume " + w.sessionID)
	if w.client == nil || w.runtime != runtimeConnected || w.binding.Generation <= before.Generation || w.binding.Session != before.Session {
		t.Fatalf("explicit resume did not replace released session: %+v", w.binding)
	}
}

func TestReleasedRuntimeResumesForReviewAndNativeControls(t *testing.T) {
	for _, commandText := range []string{"/review inspect", "/model fixture/other", "/thinking", "/name resumed"} {
		t.Run(commandText, func(t *testing.T) {
			w, _, command, before := releasedIdleWorker(t)
			command(commandText)
			if w.client == nil || w.runtime != runtimeConnected || !sameBindingIdentity(w.binding, before) {
				t.Fatalf("%s did not resume the saved session: %+v", commandText, w.binding)
			}
		})
	}
}
func TestReleasedRuntimeFailureDoesNotSubmitPrompt(t *testing.T) {
	w, _, command, before := releasedIdleWorker(t)
	w.b.cfg.OMP = t.TempDir() + "/missing-omp"
	command("cannot resume")
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&state); err != nil || state == "submitted" {
		t.Fatalf("failed lazy resume prompt state = %q, error %v", state, err)
	}
	if w.client != nil || w.runtime != runtimeReleased || !sameBindingIdentity(w.binding, before) || w.startIntent != nil {
		t.Fatalf("failed lazy resume changed logical state: binding=%+v runtime=%d client=%t intent=%+v", w.binding, w.runtime, w.client != nil, w.startIntent)
	}
}

func TestIdleReleaseRequiresQuiescentRuntime(t *testing.T) {
	w, _, _, _ := releasedIdleWorker(t)
	for _, tc := range []struct {
		name  string
		apply func()
	}{
		{"busy", func() { w.busy = true }},
		{"compacting", func() { w.compacting = true }},
		{"finishing", func() { w.finishing = true }},
		{"queued", func() { w.queue = []queued{{}} }},
		{"media", func() { w.queue = []queued{{preparing: true}} }},
		{"host request", func() { w.hostRequests = map[string]context.CancelFunc{"tool": func() {}} }},
		{"resume list", func() { w.resumeCancel = func() {} }},
		{"startup intent", func() { w.startIntent = &store.StartIntent{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The first released worker cannot be used to test non-release eligibility.
			// A connected worker is constructed by lazy resuming the preserved session.
			if _, err := w.ensureRuntime(); err != nil {
				t.Fatal("could not restore fixture runtime")
			}
			w.lastActivity = time.Now().Add(-2 * time.Minute)
			tc.apply()
			w.releaseIdleRuntime(time.Now())
			if w.client == nil || w.runtime != runtimeConnected {
				t.Fatalf("idle release ignored %s", tc.name)
			}
			w.busy, w.compacting, w.finishing = false, false, false
			w.queue = nil
			w.hostRequests = nil
			w.resumeCancel = nil
			w.confirms = make(map[string]confirmation)
			w.startIntent = nil
		})
	}
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	w.releaseIdleRuntime(time.Now())
	if w.client != nil {
		t.Fatal("idle release did not run after all blockers cleared")
	}
}

func assertIdleReleaseBlockedByConfirmation(t *testing.T, action string) {
	t.Helper()
	w, _, _, _ := releasedIdleWorker(t)
	if _, err := w.ensureRuntime(); err != nil {
		t.Fatal("could not restore fixture runtime")
	}
	token := "runtime-" + action
	c := confirmation{action: action, generation: w.binding.Generation, user: 7, messageID: 41}
	if action == "ui" {
		c.uiID = "idle-ui"
	}
	if action == "new" {
		c.workspace = t.TempDir()
	}
	w.confirms[token] = c
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	w.releaseIdleRuntime(time.Now())
	if w.client == nil || w.runtime != runtimeConnected {
		t.Fatalf("idle release ignored %s confirmation", action)
	}
	if _, ok := w.confirms[token]; !ok {
		t.Fatalf("idle release removed %s confirmation", action)
	}
}

func TestIdleReleaseBlockedByModelConfirmation(t *testing.T) {
	assertIdleReleaseBlockedByConfirmation(t, "model")
}

func TestIdleReleaseBlockedByThinkingConfirmation(t *testing.T) {
	assertIdleReleaseBlockedByConfirmation(t, "thinking")
}

func TestIdleReleaseBlockedByFastConfirmation(t *testing.T) {
	assertIdleReleaseBlockedByConfirmation(t, "fast")
}

func TestIdleReleaseBlockedByNativeUIConfirmation(t *testing.T) {
	assertIdleReleaseBlockedByConfirmation(t, "ui")
}

func TestIdleReleaseBlockedByNewConfirmation(t *testing.T) {
	assertIdleReleaseBlockedByConfirmation(t, "new")
}

func TestIdleReleaseAllowedWithResumePicker(t *testing.T) {
	w, _, _, _ := releasedIdleWorker(t)
	if _, err := w.ensureRuntime(); err != nil {
		t.Fatal("could not restore fixture runtime")
	}
	token := "resume-picker"
	w.confirms[token] = confirmation{action: "resume", generation: w.binding.Generation, user: 7, messageID: 41}
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	w.releaseIdleRuntime(time.Now())
	if w.client != nil || w.runtime != runtimeReleased {
		t.Fatal("resume picker prevented idle release")
	}
	if _, ok := w.confirms[token]; !ok {
		t.Fatal("idle release removed the resume picker")
	}
}

func TestExpiredRuntimeConfirmationAllowsImmediateRelease(t *testing.T) {
	w, f, _, _ := releasedIdleWorker(t)
	if _, err := w.ensureRuntime(); err != nil {
		t.Fatal("could not restore fixture runtime")
	}
	w.confirms["expired-model"] = confirmation{action: "model", messageID: 41, expires: time.Now().Add(-time.Second)}
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	w.expire(time.Now())
	w.releaseIdleRuntime(time.Now())
	if w.client != nil || w.runtime != runtimeReleased {
		t.Fatal("expired runtime confirmation still prevented idle release")
	}
	if len(w.confirms) != 0 {
		t.Fatalf("expired confirmation remained: %#v", w.confirms)
	}
	waitKeyboardClears(t, f, 41)
}

func TestNativeUIDialogCleanupOnRuntimeExit(t *testing.T) {
	for _, control := range []string{"release", "close"} {
		for _, method := range []string{"confirm", "select"} {
			t.Run(control+"/"+method, func(t *testing.T) {
				w, f, command, before := releasedIdleWorker(t)
				if _, err := w.ensureRuntime(); err != nil {
					t.Fatal(err)
				}
				w.owner = 7
				w.ui(rpcEvent{ID: "waiting-permission", Method: method, Title: "Permission required", Options: []string{"Allow"}})
				data := resumeButtons(t, f)[0]["callback_data"].(string)
				messageID := f.messageCount()
				client := w.client
				if control == "close" {
					command("/close")
					if w.binding.Running {
						t.Fatal("close retained session restoration eligibility")
					}
				} else {
					w.confirms["resume-independent"] = confirmation{action: "resume", generation: w.binding.Generation, user: 7}
					w.releaseRuntimeWithReason(true, "explicit")
					if _, exists := w.confirms["resume-independent"]; !exists {
						t.Fatal("runtime release removed an independent resume picker")
					}
					if !sameBindingIdentity(w.binding, before) {
						t.Fatal("runtime release changed the logical session")
					}
				}
				if w.client != nil || w.runtime != runtimeReleased || len(w.b.slots) != 0 {
					t.Fatal("runtime exit retained the client or occupied process slot")
				}
				select {
				case <-client.Done():
				default:
					t.Fatal("native process remained alive after runtime exit")
				}
				if control == "release" {
					if _, err := w.ensureRuntime(); err != nil {
						t.Fatal(err)
					}
					w.lastActivity = time.Now().Add(-2 * time.Minute)
					w.releaseIdleRuntime(time.Now())
					if w.client != nil || len(w.b.slots) != 0 {
						t.Fatal("old native dialog prevented the replacement runtime from becoming idle")
					}
				}
				waitKeyboardClears(t, f, messageID)
				if control == "close" {
					command("/new " + before.Workspace)
				} else if _, err := w.ensureRuntime(); err != nil {
					t.Fatal(err)
				}
				clickKeyboard(w, 7, data)
				raw, err := w.call("get_state", nil)
				var state struct {
					Replies int `json:"fixtureUIReplies"`
				}
				if err != nil || json.Unmarshal(raw, &state) != nil || state.Replies != 0 {
					t.Fatal("an old permission button reached a replacement runtime")
				}
			})
		}
	}
}

func TestExplicitTeardownInvalidatesRuntimeConfirmation(t *testing.T) {
	w, f, _, _ := releasedIdleWorker(t)
	if _, err := w.ensureRuntime(); err != nil {
		t.Fatal("could not restore fixture runtime")
	}
	w.confirm(confirmation{action: "new", workspace: t.TempDir(), user: 7}, "Start replacement?", []string{"Confirm", "Cancel"})
	data := resumeButtons(t, f)[0]["callback_data"].(string)
	messageID := f.messageCount()
	w.teardownWorker(true)
	if len(w.confirms) != 0 {
		t.Fatalf("explicit teardown kept runtime confirmation: %#v", w.confirms)
	}
	waitKeyboardClears(t, f, messageID)
	clickKeyboard(w, 7, data)
	if len(w.confirms) != 0 {
		t.Fatal("invalidated confirmation became actionable after teardown")
	}
}

func TestLogicalEvictionKeepsRunningLogicalSession(t *testing.T) {
	w, _, _, _ := releasedIdleWorker(t)
	oldTimeout := logicalWorkerIdleTimeout
	logicalWorkerIdleTimeout = time.Millisecond
	t.Cleanup(func() { logicalWorkerIdleTimeout = oldTimeout })
	w.lastLogicalActivity = time.Now().Add(-time.Minute)
	if w.evictIfIdle(time.Now()) {
		t.Fatal("logical eviction released a running logical session")
	}
	if !w.binding.Running {
		t.Fatal("running binding changed during logical eviction")
	}
	if w.exitRequestedState() {
		t.Fatal("running logical session requested worker exit")
	}
	if !w.b.sessionInUse(w.sessionID) {
		t.Fatal("running logical session claim was lost")
	}
}
