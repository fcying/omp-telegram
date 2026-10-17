package bridge

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/telegram"
)

func receiveProgressNative(t *testing.T, w *worker) progressNativeResult {
	t.Helper()
	select {
	case result := <-w.progressNative.results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("native progress query did not finish")
		return progressNativeResult{}
	}
}

func TestProgressNativeStatusDistinguishesFastSettingAndUnknownMetrics(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 1
	w.previewResult = make(chan previewResult, 1)
	command("/new " + t.TempDir())
	nativeRPC(t, w, "set_model", map[string]any{"provider": "**fixture**", "modelId": "fast-fallback"})
	nativeRPC(t, w, "set_fast_mode", map[string]any{"enabled": true})
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	allowInitialProgress(w)
	w.flushPreview()
	w.progressNativeFinished(receiveProgressNative(t, w))
	select {
	case result := <-w.previewResult:
		w.previewFinished(result)
	case <-time.After(5 * time.Second):
		t.Fatal("initial progress was not sent")
	}
	w.flushPreview()
	select {
	case result := <-w.previewResult:
		if result.created || result.err != nil {
			t.Fatalf("native status did not edit existing progress: %+v", result)
		}
		w.previewFinished(result)
	case <-time.After(5 * time.Second):
		t.Fatal("native status did not reach Telegram progress")
	}
	f.mu.Lock()
	text, _ := f.messages[len(f.messages)-1]["text"].(string)
	f.mu.Unlock()
	for _, want := range []string{"<code>fast-fallback</code>", "fast off (setting: on)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("progress lost effective or unknown state %q: %s", want, text)
		}
	}
	for _, secret := range []string{"SECRET", "PRIVATE", "Authorization", "systemPrompt", "fixture", "n/a", "◫", "tok/s"} {
		if strings.Contains(text, secret) {
			t.Fatalf("progress exposed raw configuration or interpreted model markup: %s", text)
		}
	}
}

func TestProgressNativeRejectsStaleAndUnconfirmedSnapshots(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*worker, *progressNativeResult)
	}{
		{"generation", func(w *worker, _ *progressNativeResult) { w.binding.Generation++ }},
		{"turn", func(w *worker, _ *progressNativeResult) { w.turn++ }},
		{"task", func(w *worker, _ *progressNativeResult) { w.active++ }},
		{"client", func(w *worker, _ *progressNativeResult) { w.client = &omp.Client{} }},
		{"binding_session", func(w *worker, _ *progressNativeResult) { w.binding.SessionID = "other-session" }},
		{"native_session", func(_ *worker, r *progressNativeResult) { r.state.SessionID = "other-session" }},
		{"epoch", func(_ *worker, r *progressNativeResult) { r.request.epoch++ }},
		{"query_failure", func(_ *worker, r *progressNativeResult) { r.valid = false }},
		{"cancelled_request", func(w *worker, _ *progressNativeResult) { w.cancelProgressNative() }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, _, command := setupWorkspaceWorker(t)
			w.b.cfg.ProgressMode = "summary"
			command("/new " + t.TempDir())
			client := w.client
			defer func() { w.client = client }()
			w.taskState = taskState{active: 100, busy: true}
			allowInitialProgress(w)
			w.refreshProgressNative(time.Now())
			result := receiveProgressNative(t, w)
			if !result.valid {
				t.Fatal("fixture did not supply a valid native snapshot")
			}
			scenario.change(w, &result)
			w.progressNativeFinished(result)
			if w.progress.NativeStatus != "" {
				t.Fatal("stale or unconfirmed native snapshot appeared in progress")
			}
		})
	}
}

func TestProgressNativeSkipsInactiveAndDisabledUI(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*worker)
	}{
		{"idle", func(w *worker) { w.clearTask() }},
		{"off", func(w *worker) { w.b.cfg.ProgressMode = "off" }},
		{"initial_delay", func(w *worker) { w.progress.StartedAt = time.Now() }},
		{"suppressed", func(w *worker) { w.progressSuppressed = true }},
		{"finishing", func(w *worker) { w.finishing = true }},
		{"released", func(w *worker) { w.runtime = runtimeReleased }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, _, command := setupWorkspaceWorker(t)
			w.b.cfg.ProgressMode = "summary"
			command("/new " + t.TempDir())
			defer func() { w.runtime = runtimeConnected }()
			w.taskState = taskState{active: 100, busy: true}
			allowInitialProgress(w)
			scenario.change(w)
			w.refreshProgressNative(time.Now())
			if w.progressNative.pending != nil {
				t.Fatal("inactive or disabled progress started a native query")
			}
		})
	}
}

func TestProgressNativeSamplingIsSingleFlightAndRateLimited(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	command("/new " + t.TempDir())
	w.taskState = taskState{active: 100, busy: true}
	allowInitialProgress(w)
	now := time.Now()
	w.refreshProgressNative(now)
	pending := w.progressNative.pending
	w.refreshProgressNative(now.Add(progressNativeInterval))
	if w.progressNative.pending != pending {
		t.Fatal("a pending query was replaced by another sample")
	}
	w.progressNativeFinished(receiveProgressNative(t, w))
	w.refreshProgressNative(now)
	if w.progressNative.pending != nil {
		t.Fatal("native sampling ignored the refresh interval")
	}
	w.refreshProgressNative(now.Add(progressNativeInterval))
	if w.progressNative.pending == nil {
		t.Fatal("native sampling did not refresh after the interval")
	}
	w.progressNativeFinished(receiveProgressNative(t, w))
}

func TestProgressNativePendingQueryDoesNotBlockStop(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "rpc.trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 1
	command("/new " + t.TempDir())
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "get_state"})
	allowInitialProgress(w)
	w.refreshProgressNative(time.Now())
	waitFixtureRPCTrace(t, trace, "research_control_waiting", "get_state")
	command("/stop")
	if w.client != nil || w.taskActive() {
		t.Fatal("progress query kept Stop from retiring the active runtime")
	}
	w.progressNativeFinished(receiveProgressNative(t, w))
	if got := telegram.ConvertMarkdown(w.renderProgress()); got != "" {
		t.Fatalf("late status query revived stopped progress: %s", got)
	}
}
