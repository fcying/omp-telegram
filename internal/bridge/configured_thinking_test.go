package bridge

import (
	"strings"
	"testing"
	"time"
)

func configuredThinkingProgress(t *testing.T, w *worker) string {
	t.Helper()
	allowInitialProgress(w)
	now := time.Now()
	if w.progressNative.next.After(now) {
		now = w.progressNative.next
	}
	w.refreshProgressNative(now)
	w.progressNativeFinished(receiveProgressNative(t, w))
	return w.renderProgress()
}

func TestConfiguredThinkingStartupAndLazyResume(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		name := "new"
		if resumed {
			name = "resumed"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_CONFIGURED_THINKING", "auto")
			w, _, command := setupWorkspaceWorker(t)
			w.b.cfg.ProgressMode = "summary"
			w.b.cfg.QueueCapacity = 1
			command("/new " + t.TempDir())
			before, client := w.binding, w.client
			want := "⟳"
			if resumed {
				w.releaseRuntime(true)
				if w.configuredThinking != "" {
					t.Fatal("released runtime retained configured thinking")
				}
				t.Setenv("OMP_TELEGRAM_FIXTURE_CONFIGURED_THINKING", "low")
				command("/name lazy restored")
				if w.client == nil || w.client == client || !sameBindingIdentity(w.binding, before) {
					t.Fatal("lazy resume did not preserve the session on a new runtime")
				}
				want = "◔"
			}
			command("missing-terminal")
			w.dispatch()
			drainWatchdogEvents(t, w)
			if text := configuredThinkingProgress(t, w); !strings.Contains(text, want+" ") || strings.Contains(text, "◑ ") {
				t.Fatalf("progress used effective medium instead of configured thinking: %q", text)
			}
		})
	}
}

func TestConfiguredThinkingEventsOverrideEffectiveSnapshots(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_CONFIGURED_THINKING", "low")
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 1
	command("/new " + t.TempDir())
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	for _, change := range []struct {
		event              map[string]any
		configured, symbol string
	}{
		{map[string]any{"type": "thinking_level_changed", "thinkingLevel": "high", "configured": "auto"}, "auto", "⟳"},
		{map[string]any{"type": "thinking_level_changed", "thinkingLevel": "xhigh"}, "xhigh", "◕"},
	} {
		nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": change.event})
		nativeUntil(t, w, func() bool { return w.configuredThinking == change.configured })
		if text := configuredThinkingProgress(t, w); !strings.Contains(text, change.symbol+" ") || strings.Contains(text, "◑ ") {
			t.Fatalf("progress lost configured event precedence: %q", text)
		}
	}
}

func TestConfiguredThinkingResyncsModelRolesAndThinkingSelection(t *testing.T) {
	for _, scenario := range []struct{ name, initial, command, choice, symbol string }{
		{"role auto", "low", "/model", "slow", "⟳"},
		{"role manual", "auto", "/model", "smol", "◔"},
		{"thinking manual", "auto", "/thinking", "max", "◒"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_CONFIGURED_THINKING", scenario.initial)
			w, f, command := setupWorkspaceWorker(t)
			w.b.cfg.ProgressMode = "summary"
			w.b.cfg.QueueCapacity = 1
			command("/new " + t.TempDir())
			command(scenario.command)
			var selected string
			for _, button := range resumeButtons(t, f) {
				label := button["text"].(string)
				if label == scenario.choice || strings.HasPrefix(label, scenario.choice+" - ") {
					selected = button["callback_data"].(string)
					break
				}
			}
			if selected == "" {
				t.Fatal("configured choice was not available")
			}
			clickKeyboard(w, 7, selected)
			command("missing-terminal")
			w.dispatch()
			drainWatchdogEvents(t, w)
			if text := configuredThinkingProgress(t, w); !strings.Contains(text, scenario.symbol+" ") {
				t.Fatalf("new configuration was not synchronized before the next task: %q", text)
			}
		})
	}
}
