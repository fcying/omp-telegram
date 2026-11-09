package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func awaitReplyStatus(t *testing.T, w *worker) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for w.replyStatus.pending != nil {
		select {
		case result := <-w.operations:
			w.operationReturned(result)
		case result := <-w.nativeCatalog.results:
			w.commandCatalogFinished(result)
		case <-timer.C:
			t.Fatal("actor did not finish the bounded final status query")
		}
	}
}

func replyStatusWorker(t *testing.T) (*worker, *fakeHTTP, func(string), string) {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "rpc.trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "off"
	w.b.cfg.QueueCapacity = 4
	w.b.cfg.StatusBar.Intermediate = w.b.cfg.StatusBar.Fields("final")
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	requireStoreOK(t, err)
	requireStoreOK(t, w.b.db.MarkOutput(ready.ID, "sending"))
	requireStoreOK(t, w.b.db.MarkOutput(ready.ID, "done"))
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	return w, f, command, trace
}

func replyStatusMessage(t *testing.T, w *worker, output int64, duration float64, total int64) {
	t.Helper()
	m := map[string]any{
		"role": "assistant", "stopReason": "stop", "model": "safe", "provider": "fixture", "duration": duration,
		"usage":   map[string]any{"output": output, "totalTokens": total},
		"content": []any{map[string]any{"type": "text", "text": "answer"}},
	}
	raw, err := json.Marshal(map[string]any{"type": "message_end", "message": m})
	requireStoreOK(t, err)
	w.event(raw)
}

func receiveReplyStatus(t *testing.T, w *worker) operationResult {
	t.Helper()
	select {
	case result := <-w.operations:
		if result.kind != "reply_status" {
			t.Fatalf("unexpected operation kind: %s", result.kind)
		}
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("final status query did not reach its deadline")
		return operationResult{}
	}
}

func endReplyStatus(w *worker) {
	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","stopReason":"stop","model":"safe","provider":"fixture","duration":3000,"usage":{"output":100,"totalTokens":5500},"content":[{"type":"text","text":"answer"}]}]}`))
}

func TestFinalReplyUsesWholeTaskAverageAndPostOutputContext(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	w.progress.NativeStatus = "Context: 10% (1k / 10k) | Speed: 999 tok/s"
	replyStatusMessage(t, w, 100, 1000, 1000)
	replyStatusMessage(t, w, 100, 3000, 5500)
	nativeRPC(t, w, "fixture_get_state_update", map[string]any{
		"contextUsage": map[string]any{"tokens": 5400, "contextWindow": 10000, "percent": 54},
	})
	endReplyStatus(w)
	w.operationReturned(receiveReplyStatus(t, w))
	metadata := finalReplyStatus(t, w, 2, "answer", "answer")
	if queueState(t, w, 2) != "done" {
		t.Fatalf("confirmed answer was not committed intact: %q", metadata)
	}
	for _, want := range []string{"`safe`", "50 t/s", "55% (10k)"} {
		if !strings.Contains(metadata, want) {
			t.Fatalf("final reply missing confirmed metric %q: %s", want, metadata)
		}
	}
	for _, forbidden := range []string{"fixture", "SECRET", "PRIVATE", "54%", "999 tok/s"} {
		if strings.Contains(metadata, forbidden) {
			t.Fatalf("final reply exposed provider/configuration or reused pre-output state: %s", metadata)
		}
	}
	if w.replyStatus.metrics.speedText() != "" || f.messageCount() != 0 {
		t.Fatal("completed reply retained metrics or enabled progress while off")
	}
}

func TestFinalReplyUsesConfiguredThinkingAcrossEffectiveLevels(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		configured string
		symbol     string
	}{
		{"auto", "auto", "⟳"},
		{"manual", "max", "◉"},
		{"unknown", "", ""},
	} {
		for _, effective := range []string{"low", "high"} {
			t.Run(scenario.name+"/"+effective, func(t *testing.T) {
				w, _, _, _ := replyStatusWorker(t)
				w.configuredThinking = scenario.configured
				nativeRPC(t, w, "set_thinking_level", map[string]any{"level": effective})
				replyStatusMessage(t, w, 100, 1000, 1100)
				endReplyStatus(w)
				result := receiveReplyStatus(t, w)
				w.operationReturned(result)
				metadata := finalReplyStatus(t, w, 2, "answer")
				if !strings.Contains(metadata, "safe") || strings.ContainsAny(metadata, "◔◒") {
					t.Fatalf("effective thinking replaced configured reply state: %s", metadata)
				}
				if scenario.symbol == "" {
					if strings.ContainsAny(metadata, "⦸○◔◑◒◕◉⟳") {
						t.Fatalf("unknown configured thinking displayed a symbol: %s", metadata)
					}
				} else if !strings.Contains(metadata, scenario.symbol) {
					t.Fatalf("configured thinking %q lost its symbol: %s", scenario.configured, metadata)
				}
			})
		}
	}
}

func TestFinalReplyRejectsUnconfirmedThinkingSnapshots(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*worker, *replyStatusResult)
	}{
		{"binding_session", func(w *worker, _ *replyStatusResult) { w.binding.SessionID = "other-session" }},
		{"native_session", func(_ *worker, r *replyStatusResult) { r.state.SessionID = "other-session" }},
		{"epoch", func(_ *worker, r *replyStatusResult) { r.request.epoch++ }},
		{"query_failure", func(_ *worker, r *replyStatusResult) { r.valid = false }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, _, _, _ := replyStatusWorker(t)
			w.configuredThinking = "auto"
			replyStatusMessage(t, w, 100, 1000, 1100)
			endReplyStatus(w)
			result := receiveReplyStatus(t, w)
			sample := result.meta.(replyStatusResult)
			if !sample.valid {
				t.Fatal("fixture did not supply a valid native snapshot")
			}
			scenario.change(w, &sample)
			result.meta = sample
			w.operationReturned(result)
			metadata := finalReplyStatus(t, w, 2, "answer")
			if !strings.Contains(metadata, "100 t/s") || strings.ContainsAny(metadata, "⦸○◔◑◒◕◉⟳") {
				t.Fatalf("unconfirmed snapshot changed reply or projected configured thinking: %s", metadata)
			}
		})
	}
}

func TestFinalReplyMetadataPreservesPendingStatusAndUsage(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "usage.release")
	t.Setenv("OMP_TELEGRAM_FIXTURE_USAGE_RELEASE", gate)
	w, _, trace := replyUsageWorker(t, "hold-report")
	t.Cleanup(func() { requireStoreOK(t, os.WriteFile(gate, nil, 0600)) })
	nativeRPC(t, w, "fixture_get_state_update", map[string]any{
		"contextUsage": map[string]any{"tokens": 9000, "contextWindow": 10000, "percent": 90},
	})
	endUsageReply(w)
	sample := receiveReplyStatus(t, w)
	client := w.client
	metadata := func() {
		t.Helper()
		pending, usage := w.replyStatus.pending, w.providerUsage.pending
		state := "submitted"
		if usage != nil {
			state = "done"
		}
		if pending == nil && usage == nil {
			t.Fatal("neither status sampling nor idle quota refresh was pending")
		}
		for _, method := range []string{"setStatus", "setWidget", "notify"} {
			raw, err := json.Marshal(map[string]any{"type": "extension_ui_request", "method": method, "message": "metadata"})
			requireStoreOK(t, err)
			w.event(raw)
			rows := researchReplies(t, w, 2)
			if w.replyStatus.pending != pending || w.providerUsage.pending != usage || w.client != client || client.Stopping() || queueState(t, w, 2) != state || len(rows) != 1 || replyBody(t, w, 2, 0) != "answer" {
				t.Fatalf("%s interrupted status or quota sampling or changed the committed body", method)
			}
		}
	}
	metadata()
	w.operationReturned(sample)
	waitFixtureRPCTrace(t, trace, "usage_waiting", "")
	if w.providerUsage.pending == nil || w.replyStatus.pending != nil || queueState(t, w, 2) != "done" {
		t.Fatal("idle quota refresh blocked final settlement")
	}
	if !client.UsageNeedsRetirement() {
		t.Fatal("private usage was not submitted and awaiting its report")
	}
	metadata()
	requireStoreOK(t, os.WriteFile(gate, nil, 0600))
	awaitProviderUsage(t, w)
	header := finalReplyStatus(t, w, 2, "answer")
	for _, want := range []string{"11% (10k)", "100 t/s", "30% 65%"} {
		if !strings.Contains(header, want) {
			t.Fatalf("metadata prevented confirmed final metric %q: %s", want, header)
		}
	}
}

func TestFinalReplyMetadataTimeoutDoesNotDiscardConfirmedAnswer(t *testing.T) {
	w, _, _, trace := replyStatusWorker(t)
	replyStatusMessage(t, w, 100, 1000, 1100)
	nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "get_state"})
	endReplyStatus(w)
	waitFixtureRPCTrace(t, trace, "research_control_waiting", "get_state")
	// Repeated summaries must neither replace the body nor count tokens twice.
	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"duplicate"}]}]}`))
	w.operationReturned(receiveReplyStatus(t, w))
	metadata := finalReplyStatus(t, w, 2, "answer")
	if queueState(t, w, 2) != "done" || !strings.Contains(metadata, "100 t/s") || strings.Contains(metadata, "ctx ?") {
		t.Fatalf("read timeout changed confirmed answer or displayed a context placeholder: %q", metadata)
	}
	if strings.Contains(metadata, "% (") || strings.Contains(metadata, "duplicate") || w.client == nil {
		t.Fatal("read timeout fabricated final context or retired a healthy runtime")
	}
}

func TestFinalReplyLateSnapshotCannotChangeNewInputOrRPCOwnership(t *testing.T) {
	for _, control := range []string{"/stop", "/close", "missing-terminal"} {
		t.Run(control, func(t *testing.T) {
			w, _, command, _ := replyStatusWorker(t)
			w.configuredThinking = "auto"
			replyStatusMessage(t, w, 100, 1000, 1100)
			endReplyStatus(w)
			late := receiveReplyStatus(t, w)
			// The snapshot has arrived, but the actor receives input first.
			command(control)
			if queueState(t, w, 2) != "done" {
				t.Fatal("input reclassified a confirmed completed task")
			}
			metadata := finalReplyStatus(t, w, 2, "answer")
			if strings.Contains(metadata, "% (") || strings.Contains(metadata, "ctx ?") || strings.Contains(metadata, "⟳") {
				t.Fatalf("input delayed answer delivery or used an unaccepted snapshot: %q", metadata)
			}
			w.dispatch()
			active, lane := w.active, w.rpcOperationActive
			w.operationReturned(late)
			if w.active != active || w.rpcOperationActive != lane || w.replyStatus.header != "" {
				t.Fatal("old status result changed new task or released its RPC lane")
			}
			if control == "missing-terminal" {
				if active != 3 || !lane {
					t.Fatal("new prompt did not start while old metadata was pending")
				}
				drainWatchdogEvents(t, w)
			}
		})
	}
}

func TestLanguageChangeDuringTaskPreservesTaskAndOldOutput(t *testing.T) {
	w, _, command, _ := replyStatusWorker(t)
	taskID := w.active
	command("/lang")
	var noticeID int64
	var original string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT id,text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&noticeID, &original))
	if !strings.Contains(original, "Global language: English") {
		t.Fatalf("task did not start with the English interface: %q", original)
	}
	command("/lang zh")
	if w.active != taskID {
		t.Fatal("language change replaced the active task")
	}
	replyStatusMessage(t, w, 100, 1000, 1000)
	endReplyStatus(w)
	w.operationReturned(receiveReplyStatus(t, w))
	finalReplyStatus(t, w, taskID, "answer")
	var preserved string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox WHERE id=?", noticeID).Scan(&preserved))
	if preserved != original {
		t.Fatalf("language change rewrote old outbox text: %q", preserved)
	}
}

func sampleRunningReplyCapacity(t *testing.T, w *worker) {
	t.Helper()
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"streaming": true})
	nativeRPC(t, w, "fixture_get_state_update", map[string]any{
		"contextUsage": map[string]any{"tokens": 9000, "contextWindow": 10000, "percent": 90},
	})
	allowInitialProgress(w)
	w.flushPreview()
	result := receiveProgressNative(t, w)
	if !result.valid || result.state.IsStreaming == nil || !*result.state.IsStreaming {
		t.Fatal("fixture did not supply a valid running capacity sample")
	}
	w.progressNativeFinished(result)
	if w.replyStatus.liveScope == nil {
		t.Fatal("running capacity sample was not accepted")
	}
}

func TestIntermediateReplyUsesRunningCapacityWithoutProgressUI(t *testing.T) {
	for _, mode := range []string{"off", "suppressed"} {
		t.Run(mode, func(t *testing.T) {
			w, f, _, _ := replyStatusWorker(t)
			if mode == "suppressed" {
				w.b.cfg.ProgressMode = "summary"
				w.progressSuppressed = true
			}
			w.configuredThinking = "auto"
			sampleRunningReplyCapacity(t, w)
			next := w.progressNative.next
			replyStatusMessage(t, w, 100, 1000, 1100)
			w.event([]byte(`{"type":"message_start"}`))
			rows := researchReplies(t, w, w.active)
			if len(rows) != 1 {
				t.Fatalf("completed middle body count = %d, want 1", len(rows))
			}
			header, body := attachedReplyBody(t, rows[0])
			if !strings.Contains(header, "11% (10k)") || !strings.Contains(header, "100 t/s") || body != "answer" {
				t.Fatalf("completed middle body lost confirmed usage or used live input usage: %q", rows)
			}
			if !*w.replyStatus.liveState.IsStreaming || compactFields("en", w.replyStatus.liveState, &w.replyStatus.metrics, false, true).context != "" {
				t.Fatal("intermediate formatting forged idle state or relaxed final context eligibility")
			}
			if f.messageCount() != 0 || w.progressNative.pending != nil || !w.progressNative.next.Equal(next) {
				t.Fatal("header sampling sent disabled progress or queried again for a message")
			}
			deliverReplies(t, w)
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.messages) != 1 || queueState(t, w, w.active) != "submitted" {
				t.Fatalf("middle header did not reach Telegram independently of progress UI: %+v", f.messages)
			}
			text, _ := f.messages[0]["text"].(string)
			sentHeader, sentBody := attachedReplyBody(t, text)
			if !strings.Contains(sentHeader, "11% (10k)") || !strings.Contains(sentHeader, "<code>safe</code>") || sentBody != "answer" {
				t.Fatalf("middle Send lost confirmed status or body: %q", text)
			}
		})
	}
}

func TestIntermediateReplyRejectsUnconfirmedCapacityAtDelivery(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	sampleRunningReplyCapacity(t, w)
	state, scope := w.replyStatus.liveState, *w.replyStatus.liveScope
	for _, scenario := range []struct {
		name   string
		change func(*worker)
	}{
		{"model", func(w *worker) { w.replyStatus.liveState.Model.ID = "other-model" }},
		{"provider", func(w *worker) { w.replyStatus.liveState.Model.Provider = "other-provider" }},
		{"missing_model", func(w *worker) { w.replyStatus.liveState.Model.ID = "" }},
		{"missing_provider", func(w *worker) { w.replyStatus.liveState.Model.Provider = "" }},
		{"client", func(w *worker) { w.replyStatus.liveScope.client = nil }},
		{"generation", func(w *worker) { w.replyStatus.liveScope.generation++ }},
		{"root", func(w *worker) { w.replyStatus.liveScope.active++ }},
		{"turn", func(w *worker) { w.replyStatus.liveScope.turn++ }},
		{"session", func(w *worker) { w.replyStatus.liveScope.sessionID = "other-session" }},
		{"missing_session", func(w *worker) { w.replyStatus.liveScope.sessionID = "" }},
		{"epoch", func(w *worker) { w.replyStatus.liveScope.epoch++ }},
		{"unavailable_sample", func(w *worker) { w.replyStatus.liveScope = nil }},
		{"missing_usage", func(w *worker) { w.replyStatus.metrics.lastContextKnown = false }},
		{"missing_capacity", func(w *worker) { w.replyStatus.liveState.ContextUsage = nil }},
		{"unknown_window", func(w *worker) { w.replyStatus.liveState.ContextUsage.ContextWindow = nil }},
		{"zero_window", func(w *worker) { n := int64(0); w.replyStatus.liveState.ContextUsage.ContextWindow = &n }},
		{"negative_window", func(w *worker) { n := int64(-1); w.replyStatus.liveState.ContextUsage.ContextWindow = &n }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			currentScope, usage := scope, *state.ContextUsage
			w.replyStatus.liveState = state
			w.replyStatus.liveState.ContextUsage = &usage
			w.replyStatus.liveScope = &currentScope
			scenario.change(w)
			if scenario.name == "missing_usage" {
				w.event([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","model":"safe","provider":"fixture","duration":1000,"usage":{"output":100},"content":[{"type":"text","text":"answer"}]}}`))
			} else {
				replyStatusMessage(t, w, 100, 1000, 1100)
			}
			rows := researchReplies(t, w, w.active)
			header, body := attachedReplyBody(t, rows[len(rows)-1])
			if strings.Contains(header, "% (") || !strings.Contains(header, "100 t/s") || body != "answer" {
				t.Fatalf("unconfirmed capacity leaked into the immutable middle body: %q", rows[len(rows)-1])
			}
		})
	}
}

func TestFinalContextIsAbsentUntilCurrentScopeReadCompletes(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		usage  any
		change func(*replyStatusResult)
		want   string
	}{
		{"known", map[string]any{"tokens": 900, "contextWindow": 10000}, nil, "11% (10k)"},
		{"missing", nil, nil, ""},
		{"invalid", map[string]any{"tokens": 1100, "contextWindow": 0}, nil, ""},
		{"query_failure", nil, func(r *replyStatusResult) { r.valid = false }, ""},
		{"stale_session", nil, func(r *replyStatusResult) { r.state.SessionID = "other-session" }, ""},
		{"stale_epoch", nil, func(r *replyStatusResult) { r.request.epoch++ }, ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, _, _, _ := replyStatusWorker(t)
			setProviderUsageFields(w, true)
			nativeRPC(t, w, "fixture_get_state_update", map[string]any{"contextUsage": scenario.usage})
			replyStatusMessage(t, w, 100, 1000, 1100)
			endReplyStatus(w)
			assertPending := func() {
				t.Helper()
				if w.replyStatus.pending == nil || strings.Contains(w.replyStatus.header, "ctx ?") || strings.Contains(w.replyStatus.header, "% (") || strings.Contains(w.replyStatus.header, "quota ?") {
					t.Fatalf("pending final read displayed context: %q", w.replyStatus.header)
				}
				rows := researchReplies(t, w, w.active)
				if len(rows) != 1 {
					t.Fatalf("pending final metadata changed formal body count: %q", rows)
				}
				header, body := attachedReplyBody(t, rows[0])
				if body != "answer" || strings.Contains(header, "ctx ?") || strings.Contains(header, "% (") || strings.Contains(header, "quota ?") {
					t.Fatalf("initial formal header displayed unread context or changed body: %q", rows[0])
				}
			}
			assertPending()
			result := receiveReplyStatus(t, w)
			assertPending()
			if scenario.change != nil {
				sample := result.meta.(replyStatusResult)
				scenario.change(&sample)
				result.meta = sample
			}
			w.operationReturned(result)
			header := finalReplyStatus(t, w, 2, "answer")
			if scenario.want == "" {
				if strings.Contains(header, "ctx ?") || strings.Contains(header, "% (") || strings.Contains(header, "quota ?") {
					t.Fatalf("stale final read marked current context as read: %q", header)
				}
			} else if !strings.Contains(header, scenario.want) || strings.Contains(header, "quota ?") {
				t.Fatalf("completed final read omitted context %q or displayed a quota placeholder: %q", scenario.want, header)
			}
		})
	}
}

func TestInterruptedFirstFinalReadDoesNotDisplayUnknownContext(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	setProviderUsageFields(w, true)
	replyStatusMessage(t, w, 100, 1000, 1100)
	endReplyStatus(w)
	late := receiveReplyStatus(t, w)
	w.flushReplyStatus()
	header := finalReplyStatus(t, w, 2, "answer")
	if strings.Contains(header, "ctx ?") || strings.Contains(header, "% (") || strings.Contains(header, "quota ?") {
		t.Fatalf("interrupted unconsumed first read displayed context: %q", header)
	}
	w.operationReturned(late)
	if got := finalReplyStatus(t, w, 2, "answer"); got != header {
		t.Fatalf("late final read rewrote the interrupted result: before=%q after=%q", header, got)
	}
}
