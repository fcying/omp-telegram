package bridge

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/i18n"
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

func TestCompactHeaderKeepsLiveAndReplyMetricSources(t *testing.T) {
	state := replyMetricsState(t, `{"model":{"id":"answer-model","provider":"private-provider"},"fastModeActive":true,"fastModeEnabled":true,"isStreaming":false,"isCompacting":false,"contextUsage":{"tokens":175000,"contextWindow":500000,"percent":35},"tokensPerSecond":25.8}`)
	state.ConfiguredThinking = "auto"
	var metrics replyMetrics
	msg := replyMetricsMessage(10, 1000)
	contextTokens := int64(210000)
	msg.Usage.ContextTokens = &contextTokens
	metrics.observe(msg)
	if got := compactHeader("en", state, nil); got != "⟳ `answer-model` ⚡ | 35% (500k) | 25.8 t/s" {
		t.Fatalf("live header changed sampled values or compact format: %s", got)
	}
	if got := compactHeader("en", state, &metrics); got != "⟳ `answer-model` ⚡ | 42% (500k) | 10 t/s" {
		t.Fatalf("reply header reused live metrics or changed compact format: %s", got)
	}
}

func TestContextPlaceholderUpdatesProgressButNotDeliveredReplies(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.previewResult = make(chan previewResult, 1)
	allowInitialProgress(w)
	preview := func() previewResult {
		t.Helper()
		w.flushPreview()
		select {
		case result := <-w.previewResult:
			requireStoreOK(t, result.err)
			w.previewFinished(result)
			return result
		case <-time.After(5 * time.Second):
			t.Fatal("context transition did not reach progress")
			return previewResult{}
		}
	}
	initial := preview()
	if strings.Contains(initial.text, "ctx ?") || strings.Contains(initial.text, "% (") {
		t.Fatalf("pending first read displayed context: %q", initial.text)
	}
	w.progressNativeFinished(receiveProgressNative(t, w))
	message := func(text string) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{
			"role": "assistant", "stopReason": "toolUse", "model": "safe", "provider": "fixture", "duration": 1000,
			"usage":   map[string]any{"output": 100, "totalTokens": 1100},
			"content": []any{map[string]any{"type": "text", "text": text}},
		}})
		requireStoreOK(t, err)
		w.event(raw)
		deliverReplies(t, w)
	}
	message("Earlier question?")
	before := researchReplies(t, w, 2)
	if len(before) != 1 || !strings.Contains(before[0], "ctx ?") {
		t.Fatalf("early reply omitted unknown context: %q", before)
	}
	nativeRPC(t, w, "fixture_get_state_update", map[string]any{
		"contextUsage": map[string]any{"tokens": 1100, "contextWindow": 10000, "percent": 11},
	})
	w.refreshProgressNative(w.progressNative.next)
	w.progressNativeFinished(receiveProgressNative(t, w))
	known := preview()
	knownHeader, _, _ := strings.Cut(known.text, "\n")
	if !known.created || known.id <= initial.id || !strings.HasPrefix(knownHeader, w.tr(i18n.Msg.Processing)+compactHeaderSeparator) || !strings.Contains(knownHeader, "11% (10k)") || strings.Contains(known.text, "ctx ?") {
		t.Fatalf("known context did not replace its progress placeholder: %+v", known)
	}
	message("Later answer.")
	w.refreshProgressNative(w.progressNative.next)
	failed := receiveProgressNative(t, w)
	failed.valid = false
	w.progressNativeFinished(failed)
	unknown := preview()
	unknownHeader, _, _ := strings.Cut(unknown.text, "\n")
	if !unknown.created || unknown.id <= known.id || !strings.HasPrefix(unknownHeader, w.tr(i18n.Msg.Processing)+compactHeaderSeparator) || !strings.Contains(unknownHeader, "ctx ?") || strings.Contains(unknown.text, "11% (10k)") {
		t.Fatalf("failed sample retained stale context: %+v", unknown)
	}
	after := researchReplies(t, w, 2)
	if len(after) != 2 || after[0] != before[0] || !strings.Contains(after[1], "11% (10k)") || strings.Contains(after[1], "ctx ?") {
		t.Fatalf("sampling rewrote delivered context or failed to update subsequent replies: %q", after)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	earlier := 0
	for _, request := range f.messages {
		text, _ := request["text"].(string)
		if strings.Contains(text, "Earlier question?") {
			earlier++
			if request["message_id"] != nil || !strings.Contains(text, "ctx ?") {
				t.Fatalf("sampling edited an existing assistant reply: %+v", request)
			}
		}
	}
	if earlier != 1 {
		t.Fatalf("earlier question was sent %d times", earlier)
	}
}

func TestProgressNativeStatusDistinguishesFastSettingAndUnknownMetrics(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 1
	w.previewResult = make(chan previewResult, 1)
	command("/new " + t.TempDir())
	deliverReplies(t, w)
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

func TestProgressNativeRefreshUsesLatestGlobalLanguage(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 1
	w.previewResult = make(chan previewResult, 1)
	command("/new " + t.TempDir())
	deliverReplies(t, w)
	nativeRPC(t, w, "set_model", map[string]any{"provider": "**fixture**", "modelId": "fast-fallback"})
	nativeRPC(t, w, "set_fast_mode", map[string]any{"enabled": true})
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	allowInitialProgress(w)
	w.refreshProgressNative(time.Now())
	w.progressNativeFinished(receiveProgressNative(t, w))
	flush := func(language, previous i18n.Language) previewResult {
		t.Helper()
		w.flushPreview()
		select {
		case result := <-w.previewResult:
			if result.err != nil {
				t.Fatal(result.err)
			}
			w.previewFinished(result)
			f.mu.Lock()
			text, _ := f.messages[len(f.messages)-1]["text"].(string)
			f.mu.Unlock()
			want := i18n.Text(language, i18n.Msg.Fast, "")
			if !strings.Contains(w.progress.NativeStatus, want) || !strings.Contains(text, want) {
				t.Fatalf("refreshed native status did not reach progress in %s: cache=%q, delivered=%q", language, w.progress.NativeStatus, text)
			}
			if previous != "" && strings.Contains(text, i18n.Text(previous, i18n.Msg.Fast, "")) {
				t.Fatalf("refreshed progress retained the previous native status language: %q", text)
			}
			return result
		case <-time.After(5 * time.Second):
			t.Fatal("native status refresh did not reach Telegram progress")
			return previewResult{}
		}
	}
	initial := flush(i18n.English, "")
	previous := i18n.English
	for _, language := range []i18n.Language{i18n.Chinese, i18n.English} {
		w.refreshProgressNative(w.progressNative.next)
		result := receiveProgressNative(t, w)
		// Switch after sampling starts; completion must read the current preference.
		command("/lang " + string(language))
		w.progressNativeFinished(result)
		edit := flush(language, previous)
		if edit.created || edit.id != initial.id {
			t.Fatalf("language refresh replaced the existing progress message: initial=%d, edit=%+v", initial.id, edit)
		}
		previous = language
	}
}

func TestProgressNativeUsesConfiguredThinkingAcrossEffectiveLevels(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		configured string
		symbol     string
	}{
		{"auto", "auto", "⟳"},
		{"manual", "max", "◉"},
		{"unknown", "", ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, _, command := setupWorkspaceWorker(t)
			w.b.cfg.ProgressMode = "summary"
			command("/new " + t.TempDir())
			w.configuredThinking = scenario.configured
			w.taskState = taskState{active: 100, busy: true}
			allowInitialProgress(w)
			now := time.Now()
			for _, effective := range []string{"low", "high"} {
				nativeRPC(t, w, "set_thinking_level", map[string]any{"level": effective})
				w.refreshProgressNative(now)
				result := receiveProgressNative(t, w)
				w.progressNativeFinished(result)
				text := w.progress.NativeStatus
				if !strings.Contains(text, "safe") || strings.ContainsAny(text, "◔◒") {
					t.Fatalf("effective thinking replaced configured progress state: %s", text)
				}
				if scenario.symbol == "" {
					if strings.ContainsAny(text, "⦸○◔◑◒◕◉⟳") {
						t.Fatalf("unknown configured thinking displayed a symbol: %s", text)
					}
				} else if !strings.Contains(text, scenario.symbol) {
					t.Fatalf("configured thinking %q lost its symbol: %s", scenario.configured, text)
				}
				now = now.Add(progressNativeInterval)
			}
		})
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
		{"cancelled_request", func(w *worker, _ *progressNativeResult) { w.cancelProgressNative() }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, _, command := setupWorkspaceWorker(t)
			w.b.cfg.ProgressMode = "summary"
			command("/new " + t.TempDir())
			client := w.client
			defer func() { w.client = client }()
			w.taskState = taskState{active: 100, busy: true}
			w.configuredThinking = "auto"
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

func TestProgressNativeSkipsInactiveAndDelayedSampling(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*worker)
	}{
		{"idle", func(w *worker) { w.clearTask() }},
		{"initial_delay", func(w *worker) { w.progress.StartedAt = time.Now() }},
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
				t.Fatal("inactive or delayed sampling started a native query")
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

func TestProgressNativeKeepsLatestStatusThroughMetadataAndActivity(t *testing.T) {
	for _, mode := range []string{"summary", "verbose", "off"} {
		t.Run(mode, func(t *testing.T) {
			w, f, _, _ := replyStatusWorker(t)
			w.b.cfg.ProgressMode = mode
			w.previewResult = make(chan previewResult, 1)
			allowInitialProgress(w)
			metadata, err := json.Marshal(map[string]any{
				"type": "session_info_update", "sessionId": w.binding.SessionID, "title": "Updated title",
			})
			requireStoreOK(t, err)
			var previous previewResult
			for i, sample := range []struct {
				model, thinking string
				context         int64
				speed           float64
				fast            bool
				want            string
			}{
				{"safe", "auto", 1100, 12.5, false, "⟳ `safe` | 11% (10k) | 12.5 t/s"},
				{"answer-model", "max", 2500, 25.8, true, "◉ `answer-model` ⚡ | 25% (10k) | 25.8 t/s"},
			} {
				nativeRPC(t, w, "set_model", map[string]any{"provider": "fixture", "modelId": sample.model})
				nativeRPC(t, w, "set_fast_mode", map[string]any{"enabled": sample.fast})
				nativeRPC(t, w, "fixture_get_state_update", map[string]any{
					"contextUsage": map[string]any{"tokens": sample.context, "contextWindow": 10000},
				})
				now := time.Now()
				if w.progressNative.next.After(now) {
					now = w.progressNative.next
				}
				w.refreshProgressNative(now)
				result := receiveProgressNative(t, w)
				// The fixture has no speed field; inject only that native response
				// field to exercise its projection into the consumer-visible preview.
				result.state.TokensPerSecond = &sample.speed
				next := w.progressNative.next
				w.event(metadata)
				w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Working"}}`))
				w.event([]byte(`{"type":"tool_execution_start","toolCallId":"live-tool","toolName":"read"}`))
				w.event([]byte(`{"type":"tool_execution_update","toolCallId":"live-tool"}`))
				w.event([]byte(`{"type":"tool_execution_end","toolCallId":"live-tool"}`))
				thinking, err := json.Marshal(map[string]any{
					"type": "thinking_level_changed", "thinkingLevel": "low", "configured": sample.thinking,
				})
				requireStoreOK(t, err)
				w.event(thinking)
				if w.progressNative.pending != result.request || !w.progressNative.next.Equal(next) {
					t.Fatal("same-session metadata or ordinary activity cancelled or rescheduled the sample")
				}
				if !w.progressNativeFinished(result) || w.progress.NativeStatus != sample.want {
					t.Fatalf("latest native status was not accepted: %q", w.progress.NativeStatus)
				}
				w.event(metadata)
				if w.progress.NativeStatus != sample.want || w.replyStatus.liveScope != result.request {
					t.Fatal("same-session metadata erased accepted native status")
				}
				w.flushPreview()
				if mode == "off" {
					if f.messageCount() != 0 || w.previewBusy || w.progressNative.pending != nil {
						t.Fatal("disabled progress sent a preview or started an extra status query")
					}
					continue
				}
				select {
				case preview := <-w.previewResult:
					requireStoreOK(t, preview.err)
					w.previewFinished(preview)
					header, _, _ := strings.Cut(preview.text, "\n")
					if !strings.Contains(header, sample.want) {
						t.Fatalf("latest status did not reach the live preview: %q", preview.text)
					}
					if i > 0 && (preview.created || preview.id != previous.id) {
						t.Fatal("ordinary activity replaced rather than updated the live preview")
					}
					previous = preview
				case <-time.After(5 * time.Second):
					t.Fatal("latest status did not reach Telegram")
				}
			}
		})
	}
}

func TestProgressNativeSessionMetadataClearsChangedScopes(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*worker)
	}{
		{"generation", func(w *worker) { w.binding.Generation++ }},
		{"turn", func(w *worker) { w.turn++ }},
		{"root", func(w *worker) { w.active++ }},
		{"client", func(w *worker) { w.client = &omp.Client{} }},
		{"binding_session", func(w *worker) { w.binding.SessionID = "other-session" }},
		{"epoch_unknown_session", func(w *worker) { w.client.InvalidateCommandCatalog() }},
		{"native_session", func(w *worker) {
			nativeRPC(t, w, "fixture_research_control_event", map[string]any{"event": map[string]any{
				"type": "session_info_update", "sessionId": "other-session",
			}})
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, _, _, _ := replyStatusWorker(t)
			client := w.client
			defer func() { w.client = client }()
			allowInitialProgress(w)
			w.refreshProgressNative(time.Now())
			if !w.progressNativeFinished(receiveProgressNative(t, w)) {
				t.Fatal("fixture did not establish live native status")
			}
			w.refreshProgressNative(w.progressNative.next)
			late := receiveProgressNative(t, w)
			scenario.change(w)
			w.progressNativeSessionChanged()
			if w.progressNative.pending != nil || w.progressNative.completed != nil || w.replyStatus.liveScope != nil || w.progress.NativeStatus != "" || !w.progressNative.next.IsZero() {
				t.Fatal("session metadata retained native status from a changed or unknown scope")
			}
			if w.progressNativeFinished(late) || w.progress.NativeStatus != "" {
				t.Fatal("late native status revived a cleared scope")
			}
		})
	}
}

func TestContextVisibilityFollowsCompletedScopedReads(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		usage any
		valid bool
		want  string
	}{
		{"known", map[string]any{"tokens": 1100, "contextWindow": 10000}, true, "11% (10k)"},
		{"missing", nil, true, compactUnknownContext},
		{"invalid", map[string]any{"tokens": 1100, "contextWindow": 0}, true, compactUnknownContext},
		{"query_failure", nil, false, compactUnknownContext},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, _, _, _ := replyStatusWorker(t)
			allowInitialProgress(w)
			nativeRPC(t, w, "fixture_get_state_update", map[string]any{"contextUsage": scenario.usage})
			messageHeader := func(text string) string {
				t.Helper()
				raw, err := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{
					"role": "assistant", "stopReason": "toolUse", "model": "safe", "provider": "fixture", "duration": 1000,
					"usage":   map[string]any{"output": 100, "totalTokens": 1100},
					"content": []any{map[string]any{"type": "text", "text": text}},
				}})
				requireStoreOK(t, err)
				w.event(raw)
				rows := researchReplies(t, w, w.active)
				header, body := attachedReplyBody(t, rows[len(rows)-1])
				if body != text {
					t.Fatalf("context state changed assistant body: %q", body)
				}
				return header
			}
			assertUnread := func(header string) {
				t.Helper()
				if w.progressContextReadCompleted() || w.progressNativeStatus() != "" || strings.Contains(w.renderProgress(), "ctx ?") || strings.Contains(w.renderProgress(), "% (") ||
					strings.Contains(header, "ctx ?") || strings.Contains(header, "% (") || strings.Contains(w.unconfirmedAssistantHeader(), "ctx ?") {
					t.Fatalf("unread context appeared in progress or assistant header: %q", header)
				}
			}
			assertUnread(messageHeader("Before read"))
			w.refreshProgressNative(time.Now())
			assertUnread(messageHeader("While read is pending"))
			result := receiveProgressNative(t, w)
			result.valid = scenario.valid
			if !w.progressNativeFinished(result) || !w.progressContextReadCompleted() {
				t.Fatal("current-scope completed read was not recorded")
			}
			if !strings.Contains(w.renderProgress(), scenario.want) || !strings.Contains(messageHeader("After read"), scenario.want) {
				t.Fatalf("completed context did not reach progress and formal message: progress=%q", w.renderProgress())
			}
			if !strings.Contains(w.unconfirmedAssistantHeader(), compactUnknownContext) {
				t.Fatal("completed read was lost for an unconfirmed streamed message")
			}
			rows := researchReplies(t, w, w.active)
			for _, row := range rows[:2] {
				header, _ := attachedReplyBody(t, row)
				if strings.Contains(header, "ctx ?") || strings.Contains(header, "% (") {
					t.Fatal("completed read rewrote an already queued preread header")
				}
			}
			w.binding.Generation++
			assertUnread(w.intermediateAssistantHeader())
			w.cancelProgressNative()
			if w.progressNative.completed != nil {
				t.Fatal("scope reset retained completed-read evidence")
			}
			assertUnread(messageHeader("After scope reset"))
		})
	}
}
