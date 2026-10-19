package bridge

import (
	"encoding/json"
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

func replyBody(text string) string {
	header, body, found := strings.Cut(text, "\n")
	if !found || (!strings.Contains(header, "`") && !strings.Contains(header, "<code>")) {
		return text
	}
	return strings.TrimPrefix(body, "\n")
}

func replyStatusWorker(t *testing.T) (*worker, *fakeHTTP, func(string), string) {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "rpc.trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "off"
	w.b.cfg.QueueCapacity = 4
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
		"role": "assistant", "model": "safe", "provider": "fixture", "duration": duration,
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
	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","model":"safe","provider":"fixture","duration":3000,"usage":{"output":100,"totalTokens":5500},"content":[{"type":"text","text":"answer"}]}]}`))
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
	output, err := w.b.db.NextOutput()
	requireStoreOK(t, err)
	if queueState(t, w, 2) != "done" || !strings.HasSuffix(output.Text, "\n\nanswer") {
		t.Fatalf("confirmed answer was not committed intact: %q", output.Text)
	}
	for _, want := range []string{"`safe`", "avg 50 tok/s", "◫ 55% (10k)"} {
		if !strings.Contains(output.Text, want) {
			t.Fatalf("final reply missing confirmed metric %q: %s", want, output.Text)
		}
	}
	for _, forbidden := range []string{"fixture", "SECRET", "PRIVATE", "54%", "999 tok/s"} {
		if strings.Contains(output.Text, forbidden) {
			t.Fatalf("final reply exposed provider/configuration or reused pre-output state: %s", output.Text)
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
				output, err := w.b.db.NextOutput()
				requireStoreOK(t, err)
				if !strings.HasSuffix(output.Text, "\n\nanswer") || !strings.Contains(output.Text, "safe") || strings.ContainsAny(output.Text, "◔◒") {
					t.Fatalf("effective thinking replaced configured reply state: %s", output.Text)
				}
				if scenario.symbol == "" {
					if strings.ContainsAny(output.Text, "⦸○◔◑◒◕◉⟳") {
						t.Fatalf("unknown configured thinking displayed a symbol: %s", output.Text)
					}
				} else if !strings.Contains(output.Text, scenario.symbol) {
					t.Fatalf("configured thinking %q lost its symbol: %s", scenario.configured, output.Text)
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
			output, err := w.b.db.NextOutput()
			requireStoreOK(t, err)
			if !strings.HasSuffix(output.Text, "\n\nanswer") || !strings.Contains(output.Text, "100 tok/s") || strings.ContainsAny(output.Text, "⦸○◔◑◒◕◉⟳") {
				t.Fatalf("unconfirmed snapshot changed reply or projected configured thinking: %s", output.Text)
			}
		})
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
	output, err := w.b.db.NextOutput()
	requireStoreOK(t, err)
	if queueState(t, w, 2) != "done" || !strings.HasSuffix(output.Text, "\n\nanswer") || !strings.Contains(output.Text, "avg 100 tok/s") {
		t.Fatalf("read timeout changed confirmed answer or metrics: %q", output.Text)
	}
	if strings.Contains(output.Text, "◫") || strings.Contains(output.Text, "duplicate") || w.client == nil {
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
			output, err := w.b.db.NextOutput()
			requireStoreOK(t, err)
			if !strings.HasSuffix(output.Text, "\n\nanswer") || strings.Contains(output.Text, "◫") || strings.Contains(output.Text, "⟳") {
				t.Fatalf("input delayed answer delivery or used an unaccepted snapshot: %q", output.Text)
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

func TestLanguageChangeDuringTaskPreservesOldOutputAndUsesLatestReplyLanguage(t *testing.T) {
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
	var final string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox WHERE inbox_id=? ORDER BY id DESC LIMIT 1", taskID).Scan(&final))
	if !strings.Contains(final, "平均 ") || strings.Contains(final, "avg ") || replyBody(final) != "answer" {
		t.Fatalf("final Bridge header did not use Chinese or model body changed: %q", final)
	}
	var preserved string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox WHERE id=?", noticeID).Scan(&preserved))
	if preserved != original {
		t.Fatalf("language change rewrote old outbox text: %q", preserved)
	}
}
