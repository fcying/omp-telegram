package bridge

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/store"
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

func TestIdleReleasePreservesLogicalSessionAndStatusDoesNotWake(t *testing.T) {
	w, _, command, before := releasedIdleWorker(t)
	command("/status")
	if w.client != nil || w.runtime != runtimeReleased || !sameBindingIdentity(w.binding, before) {
		t.Fatal("released status woke or changed the logical session")
	}
	out, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "OMP: released") || !strings.Contains(out.Text, "Model: unavailable while released") || !strings.Contains(out.Text, "Context: unavailable while released") {
		t.Fatalf("released status = %q", out.Text)
	}
	for _, forbidden := range []string{"Running:", "Compacting:", "Speed:", "Thinking:", "Fast:"} {
		if strings.Contains(out.Text, forbidden) {
			t.Fatalf("released status leaked live metric %q: %q", forbidden, out.Text)
		}
	}
}

func TestReleasedStatusShowsRunningBindingWithoutActivity(t *testing.T) {
	w, _, command, before := releasedIdleWorker(t)
	w.lastActivity = time.Time{}
	command("/status")
	if w.client != nil || w.runtime != runtimeReleased || !sameBindingIdentity(w.binding, before) {
		t.Fatal("released status woke or changed the logical session")
	}
	out, err := w.b.db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "OMP: released") || !strings.Contains(out.Text, "Idle: n/a") {
		t.Fatalf("released status without activity = %q", out.Text)
	}
	if strings.Contains(out.Text, "No instance is running") {
		t.Fatalf("released running binding was reported as absent: %q", out.Text)
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

func TestFailedStartupRestorePreservesLogicalSession(t *testing.T) {
	w, _, _, before := releasedIdleWorker(t)
	if _, err := w.ensureRuntime(); err != nil {
		t.Fatal("could not restore fixture runtime")
	}
	w.restoring = true
	w.closeFailedStart()
	w.restoring = false
	if w.client != nil || w.runtime != runtimeReleased || !sameBindingIdentity(w.binding, before) || !w.b.sessionInUse(w.sessionID) {
		t.Fatalf("failed startup restoration closed logical session: binding=%+v runtime=%d client=%t", w.binding, w.runtime, w.client != nil)
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

func TestIdleReleaseBlockedByCompactConfirmation(t *testing.T) {
	assertIdleReleaseBlockedByConfirmation(t, "compact")
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
	w.expire()
	w.releaseIdleRuntime(time.Now())
	if w.client != nil || w.runtime != runtimeReleased {
		t.Fatal("expired runtime confirmation still prevented idle release")
	}
	if len(w.confirms) != 0 {
		t.Fatalf("expired confirmation remained: %#v", w.confirms)
	}
	waitKeyboardClears(t, f, 41)
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
