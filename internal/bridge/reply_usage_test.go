package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureReplyUsageReport = "```\nUsage (0s ago)\n\nOpenai Codex\n- 5 hours\n  PRIVATE_USAGE_ACCOUNT (pro): 30.00% used (70.0% left)  ← in use by this session\n- 7 days\n  PRIVATE_USAGE_ACCOUNT (pro): 65.00% used (35.0% left)  ← in use by this session\n```"

func fixtureReplyUsage(command, response map[string]any, emit func(any), mode, account string) bool {
	if mode == "" || command["message"] != "/usage" {
		return false
	}
	recordFixtureRPCTrace("prompt", "/usage")
	response["data"] = map[string]any{"agentInvoked": false}
	report := strings.ReplaceAll(fixtureReplyUsageReport, "PRIVATE_USAGE_ACCOUNT", account)
	switch mode {
	case "hold", "hold-report":
		recordFixtureRPCTrace("usage_waiting", "")
		if gate := os.Getenv("OMP_TELEGRAM_FIXTURE_USAGE_RELEASE"); gate != "" {
			waitForFixtureFile(gate)
		}
		if mode == "hold" {
			return true
		}
		emit(map[string]any{"type": "command_output", "text": report})
	case "reject":
		response["success"], response["error"] = false, "PRIVATE_USAGE_REJECTION"
	case "missing":
	case "fallback":
		emit(map[string]any{"type": "command_output", "text": "Usage\nInput tokens: 123\nCost: $0.01"})
	case "new-value":
		emit(map[string]any{"type": "command_output", "text": strings.ReplaceAll(report, "30.00%", "55.00%")})
	default:
		emit(map[string]any{"type": "command_output", "text": report})
		if mode == "unknown-ack" {
			response["data"] = map[string]any{}
		}
	}
	emit(response)
	return true
}

func replyUsageWorker(t *testing.T, mode string) (*worker, func(string), string) {
	t.Helper()
	t.Setenv("OMP_TELEGRAM_FIXTURE_USAGE_MODE", mode)
	w, _, command, trace := replyStatusWorker(t)
	setProviderUsageFields(w, true)
	nativeRPC(t, w, "set_model", map[string]any{"provider": "openai-codex", "modelId": "safe"})
	return w, command, trace
}

func setProviderUsageFields(w *worker, enabled bool) {
	for _, kind := range []string{"progress", "intermediate", "final"} {
		fields := w.b.cfg.StatusBar.Fields(kind)
		selected := make([]string, 0, len(fields)+1)
		for _, field := range fields {
			if field != "quota" {
				selected = append(selected, field)
			}
		}
		if enabled {
			selected = append(selected, "quota")
		}
		switch kind {
		case "progress":
			w.b.cfg.StatusBar.Progress = selected
		case "intermediate":
			w.b.cfg.StatusBar.Intermediate = selected
		case "final":
			w.b.cfg.StatusBar.Final = selected
		}
	}
}

func endUsageReply(w *worker) {
	w.event([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","model":"safe","provider":"openai-codex","duration":1000,"usage":{"output":100,"totalTokens":1100},"content":[{"type":"text","text":"answer"}]}}`))
	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","stopReason":"stop","model":"safe","provider":"openai-codex","duration":1000,"usage":{"output":100,"totalTokens":1100},"content":[{"type":"text","text":"answer"}]}]}`))
}

func awaitProviderUsage(t *testing.T, w *worker) {
	t.Helper()
	awaitReplyStatus(t, w)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for w.providerUsage.pending != nil {
		select {
		case result := <-w.operations:
			w.operationReturned(result)
		case result := <-w.nativeCatalog.results:
			w.commandCatalogFinished(result)
		case <-deadline.C:
			t.Fatal("idle quota refresh did not finish")
		}
	}
}

func seedProviderUsage(t *testing.T, w *worker) {
	t.Helper()
	account, err := w.client.ProviderAccountScope(w.ctx, "openai-codex")
	requireStoreOK(t, err)
	w.setProviderUsageScope("openai-codex", account)
	w.b.providerUsage.claim(w.providerUsage.key, time.Now())
	w.b.providerUsage.save(w.providerUsage.key, quotaSample{windows: [2]quotaWindow{
		{present: true, used: 30}, {present: true, used: 65},
	}})
}

func assertUsagePrivate(t *testing.T, w *worker) {
	t.Helper()
	var leaked int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE text LIKE '%PRIVATE_USAGE%' OR text LIKE '%in use by this session%' OR header LIKE '%PRIVATE_USAGE%' OR header LIKE '%in use by this session%'").Scan(&leaked))
	if leaked != 0 {
		t.Fatal("account identities or raw quota reports escaped to outbox")
	}
}

func TestFinalReplyProviderUsage(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			w, _, trace := replyUsageWorker(t, "normal")
			setProviderUsageFields(w, enabled)
			endUsageReply(w)
			awaitReplyStatus(t, w)
			if queueState(t, w, 2) != "done" {
				t.Fatal("quota refresh blocked task settlement")
			}
			awaitProviderUsage(t, w)
			metadata := finalReplyStatus(t, w, 2, "answer")
			if !strings.Contains(metadata, "100 t/s") || strings.Contains(metadata, "70% 35%") != enabled {
				t.Fatalf("quota opt-in changed the final reply: %q", metadata)
			}
			wantQueries := 0
			if enabled {
				wantQueries = 1
			}
			if researchTraceCount(trace, "prompt", "/usage") != wantQueries {
				t.Fatal("quota opt-in or single-flight refresh violated")
			}
			assertUsagePrivate(t, w)
		})
	}
}

func TestSnapshotFinalReplyProviderUsage(t *testing.T) {
	const intermediate = `{"role":"assistant","stopReason":"stop","model":"safe","provider":"openai-codex","duration":1000,"usage":{"output":100,"totalTokens":1100},"content":[{"type":"text","text":"intermediate"}]}`
	const answer = `{"role":"assistant","stopReason":"stop","model":"safe","provider":"openai-codex","duration":1000,"usage":{"output":100,"totalTokens":1100},"content":[{"type":"text","text":"answer"}]}`
	for _, tc := range []struct {
		name  string
		mixed bool
	}{
		{name: "terminal-only"},
		{name: "mixed", mixed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := filepath.Join(t.TempDir(), "release-usage")
			t.Setenv("OMP_TELEGRAM_FIXTURE_USAGE_RELEASE", gate)
			t.Setenv("OMP_TELEGRAM_FIXTURE_USAGE_MODE", "hold-report")
			w, f, _, trace := replyStatusWorker(t)
			t.Cleanup(func() { requireStoreOK(t, os.WriteFile(gate, nil, 0600)) })
			setProviderUsageFields(w, true)
			nativeRPC(t, w, "set_model", map[string]any{"provider": "openai-codex", "modelId": "safe"})
			bodies := []string{"answer"}
			snapshot := answer
			var intermediateHeader string
			if tc.mixed {
				w.event([]byte(`{"type":"message_end","message":` + intermediate + `}`))
				deliverReplies(t, w)
				intermediateHeader, _ = attachedReplyBody(t, researchReplies(t, w, 2)[0])
				bodies = []string{"intermediate", "answer"}
				snapshot = intermediate + "," + answer
			}
			w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[` + snapshot + `]}`))
			awaitReplyStatus(t, w)
			waitFixtureRPCTrace(t, trace, "usage_waiting", "")
			if queueState(t, w, 2) != "done" || w.providerUsage.pending == nil {
				t.Fatal("task did not settle independently of the held quota refresh")
			}
			finalReplyStatus(t, w, 2, bodies...)
			deliverReplies(t, w)
			f.mu.Lock()
			before := append([]map[string]any(nil), f.messages...)
			f.mu.Unlock()
			if len(before) != len(bodies) {
				t.Fatalf("final answer was not delivered before quota refresh: %+v", before)
			}
			for i, request := range before {
				text, _ := request["text"].(string)
				_, body := attachedReplyBody(t, text)
				if request["message_id"] != nil || body != bodies[i] {
					t.Fatalf("first delivery edited or changed body %d: %+v", i, request)
				}
			}
			requireStoreOK(t, os.WriteFile(gate, nil, 0600))
			awaitProviderUsage(t, w)
			header := finalReplyStatus(t, w, 2, bodies...)
			if !strings.Contains(header, "70% 35%") {
				t.Fatalf("successful quota refresh missed the actual final answer: %q", header)
			}
			if tc.mixed {
				header, _ := attachedReplyBody(t, researchReplies(t, w, 2)[0])
				if header != intermediateHeader {
					t.Fatalf("final quota refresh changed the intermediate header: %q", header)
				}
			}
			deliverReplies(t, w)
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.messages) != len(bodies)+1 {
				t.Fatalf("quota refresh did not produce exactly one header edit: %+v", f.messages)
			}
			edit := f.messages[len(bodies)]
			text, _ := edit["text"].(string)
			header, body := attachedReplyBody(t, text)
			if edit["message_id"] != float64(len(bodies)) || body != "answer" || !strings.Contains(header, "70% 35%") {
				t.Fatalf("quota edit missed the final answer or changed its body: %+v", edit)
			}
		})
	}
}

func TestProviderUsageCacheVisibleDuringQueuedWork(t *testing.T) {
	w, _, trace := replyUsageWorker(t, "normal")
	seedProviderUsage(t, w)
	w.b.cfg.ProgressMode = "summary"
	for name, header := range map[string]string{"progress": w.renderProgress(), "intermediate": w.intermediateAssistantHeader()} {
		if strings.Contains(header, "70%") || strings.Contains(header, "quota ?") {
			t.Fatalf("%s exposed quota before the current scope was sampled: %q", name, header)
		}
	}
	w.progress.StartedAt = time.Now().Add(-progressInitialDelay)
	w.refreshProgressNative(time.Now())
	select {
	case sample := <-w.progressNative.results:
		w.progressNativeFinished(sample)
	case <-time.After(5 * time.Second):
		t.Fatal("live account sample did not finish")
	}
	for name, header := range map[string]string{"progress": w.renderProgress(), "intermediate": w.intermediateAssistantHeader()} {
		if !strings.Contains(header, "70% 35%") {
			t.Fatalf("%s lost cached usage: %q", name, header)
		}
	}
	nativeInput(t, w, 100, "/followup queued next task", false)
	endUsageReply(w)
	awaitReplyStatus(t, w)
	if !strings.Contains(finalReplyStatus(t, w, 2, "answer"), "70% 35%") || researchTraceCount(trace, "prompt", "/usage") != 0 {
		t.Fatal("queued work hid cached usage or triggered a blocking refresh")
	}
	w.dispatch()
	drainWatchdogEvents(t, w)
	if queueState(t, w, 100) != "done" || researchTraceCount(trace, "prompt", "queued next task") != 1 {
		t.Fatal("cached usage interfered with queued work")
	}
	assertUsagePrivate(t, w)
}

func TestProviderUsageCacheSurvivesSessionReplacement(t *testing.T) {
	w, command, trace := replyUsageWorker(t, "normal")
	endUsageReply(w)
	awaitProviderUsage(t, w)
	before := w.binding.SessionID
	command("/close")
	command("/new " + w.binding.Workspace)
	if w.binding.SessionID == before {
		t.Fatal("fixture did not replace the session")
	}
	nativeRPC(t, w, "set_model", map[string]any{"provider": "openai-codex", "modelId": "safe"})
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	active := w.active
	endUsageReply(w)
	awaitReplyStatus(t, w)
	if !strings.Contains(finalReplyStatus(t, w, active, "answer"), "70% 35%") || researchTraceCount(trace, "prompt", "/usage") != 1 {
		t.Fatal("same account did not reuse its cache across native sessions")
	}
}

func TestProviderUsageDoesNotBorrowAnotherAccount(t *testing.T) {
	for _, account := range []string{"PRIVATE_USAGE_OTHER", ""} {
		t.Run(account, func(t *testing.T) {
			w, _, trace := replyUsageWorker(t, "normal")
			seedProviderUsage(t, w)
			nativeRPC(t, w, "fixture_usage_account", map[string]any{"account": account})
			nativeInput(t, w, 100, "/followup prevent idle refresh", false)
			endUsageReply(w)
			awaitReplyStatus(t, w)
			header := finalReplyStatus(t, w, 2, "answer")
			if strings.Contains(header, "70%") || researchTraceCount(trace, "prompt", "/usage") != 0 {
				t.Fatalf("unverified account borrowed another account's cache: %q", header)
			}
		})
	}
}

func TestFinalReplyUsageFailuresPreserveCachedAnswer(t *testing.T) {
	for _, mode := range []string{"unavailable", "fallback", "reject", "missing", "unknown-ack", "hold"} {
		t.Run(mode, func(t *testing.T) {
			w, _, trace := replyUsageWorker(t, mode)
			seedProviderUsage(t, w)
			endUsageReply(w)
			awaitReplyStatus(t, w)
			w.refreshProviderUsage(time.Now().Add(providerUsageInterval))
			awaitProviderUsage(t, w)
			header := finalReplyStatus(t, w, 2, "answer")
			if queueState(t, w, 2) != "done" || !strings.Contains(header, "70% 35%") {
				t.Fatalf("refresh failure lost cached data: %q", header)
			}
			wantQueries := 1
			if mode == "unavailable" {
				wantQueries = 0
			}
			if researchTraceCount(trace, "prompt", "/usage") != wantQueries {
				t.Fatal("failed refresh retried or bypassed builtin availability")
			}
			assertUsagePrivate(t, w)
		})
	}
}

func TestFinalReplyUsageUnsentCancellationKeepsRuntime(t *testing.T) {
	w, _, trace := replyUsageWorker(t, "hold")
	client, binding := w.client, w.binding
	endUsageReply(w)
	sample := receiveReplyStatus(t, w)
	nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "get_state"})
	w.operationReturned(sample)
	waitFixtureRPCTrace(t, trace, "research_control_waiting", "get_state")
	if queueState(t, w, 2) != "done" {
		t.Fatal("idle refresh still owns the completed task")
	}
	nativeInput(t, w, 100, "next after unsent usage", false)
	result := waitOperation(t, w)
	if result.kind != "reply_usage" || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("expected canceled unsent query: %s, %v", result.kind, result.err)
	}
	w.operationReturned(result)
	if w.client != client || client.Stopping() || len(w.b.slots) != 1 || !sameBindingIdentity(binding, w.binding) {
		t.Fatal("unsent cancellation replaced the runtime or changed session ownership")
	}
	nativeRPC(t, w, "fixture_research_control_release", nil)
	w.dispatch()
	drainWatchdogEvents(t, w)
	if queueState(t, w, 100) != "done" || researchTraceCount(trace, "prompt", "/usage") != 0 || researchTraceCount(trace, "prompt", "next after unsent usage") != 1 {
		t.Fatal("unsent usage blocked or replayed the next prompt")
	}
}

func TestFinalReplyUsageInterruptionFencesLateResult(t *testing.T) {
	for _, input := range []string{"/stop", "/close", "missing-terminal"} {
		t.Run(input, func(t *testing.T) {
			w, command, trace := replyUsageWorker(t, "hold")
			endUsageReply(w)
			w.operationReturned(receiveReplyStatus(t, w))
			waitFixtureRPCTrace(t, trace, "usage_waiting", "")
			if queueState(t, w, 2) != "done" || w.replyStatus.pending != nil {
				t.Fatal("quota refresh blocked the final answer")
			}
			header := finalReplyStatus(t, w, 2, "answer")
			if strings.Contains(header, "70%") || strings.Contains(header, "35%") {
				t.Fatal("first reply invented quota before any successful reading")
			}
			command(input)
			late := waitOperation(t, w)
			active := w.active
			w.operationReturned(late)
			if w.active != active || strings.Contains(finalReplyStatus(t, w, 2, "answer"), "70%") {
				t.Fatal("late quota result changed a replacement or the old final header")
			}
		})
	}
}

func TestFinalReplyUsageRuntimeReleaseOwnsOneSlot(t *testing.T) {
	w, _, trace := replyUsageWorker(t, "hold")
	endUsageReply(w)
	w.operationReturned(receiveReplyStatus(t, w))
	waitFixtureRPCTrace(t, trace, "usage_waiting", "")
	w.b.slots = make(chan struct{}, 2)
	w.b.slots <- struct{}{}
	w.b.slots <- struct{}{}
	w.releaseRuntimeWithReason(true, "failure")
	w.operationReturned(waitOperation(t, w))
	if w.client != nil || len(w.b.slots) != 1 || queueState(t, w, 2) != "done" {
		t.Fatal("quota teardown consumed a peer's slot or changed the completed task")
	}
	finalReplyStatus(t, w, 2, "answer")
}

func TestFinalReplyUsageCompactionEventFollowsClientLifetime(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsent", true: "sent"}[sent], func(t *testing.T) {
			w, _, trace := replyUsageWorker(t, "hold")
			client, binding := w.client, w.binding
			endUsageReply(w)
			sample := receiveReplyStatus(t, w)
			if !sent {
				nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "get_state"})
			}
			w.operationReturned(sample)
			if sent {
				waitFixtureRPCTrace(t, trace, "usage_waiting", "")
			} else {
				waitFixtureRPCTrace(t, trace, "research_control_waiting", "get_state")
			}
			w.event([]byte(`{"type":"auto_compaction_start"}`))
			if sent {
				if w.client != nil || w.compacting || len(w.b.slots) != 0 {
					t.Fatal("retired client's activity changed the worker")
				}
			} else if w.client != client || client.Stopping() || !w.compacting {
				t.Fatal("unsent optional query lost live native activity")
			}
			w.operationReturned(waitOperation(t, w))
			if !sent {
				nativeRPC(t, w, "fixture_research_control_release", nil)
			}
			nativeInput(t, w, 100, "after native activity", false)
			if !sent {
				if queueState(t, w, 100) != "pending" {
					t.Fatal("queued work bypassed live native compaction")
				}
				w.event([]byte(`{"type":"auto_compaction_end"}`))
				w.dispatch()
			}
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			if !sameBindingIdentity(binding, w.binding) || researchTraceCount(trace, "prompt", "after native activity") != 1 || researchBody(t, w, 2) != "answer" {
				t.Fatal("native activity lost session identity, the old answer, or prompt ownership")
			}
		})
	}
}

func TestFinalReplyUsagePreservesOtherNativeOutput(t *testing.T) {
	w, _, trace := replyUsageWorker(t, "hold")
	endUsageReply(w)
	w.operationReturned(receiveReplyStatus(t, w))
	waitFixtureRPCTrace(t, trace, "usage_waiting", "")
	w.event([]byte(`{"type":"command_output","text":"other command receipt"}`))
	if w.providerUsage.pending == nil || w.client == nil {
		t.Fatal("unrelated native receipt interrupted quota refresh")
	}
	var receipts int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE text=? AND inbox_id=0", "other command receipt").Scan(&receipts))
	if receipts != 1 {
		t.Fatal("unrelated receipt was lost or assigned to the completed answer")
	}
	w.flushProviderUsage()
	w.operationReturned(waitOperation(t, w))
}

func TestFinalReplyUsageStoppingBeforeDonePreservesQueue(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "release-usage")
	t.Setenv("OMP_TELEGRAM_FIXTURE_USAGE_RELEASE", gate)
	w, _, trace := replyUsageWorker(t, "hold")
	t.Cleanup(func() { requireStoreOK(t, os.WriteFile(gate, nil, 0600)) })
	client, binding := w.client, w.binding
	endUsageReply(w)
	w.operationReturned(receiveReplyStatus(t, w))
	waitFixtureRPCTrace(t, trace, "usage_waiting", "")
	w.providerUsage.pending.cancel()
	late := waitOperation(t, w)
	if late.err == nil || !client.Stopping() {
		t.Fatal("submitted private query did not fence the failed transport")
	}
	nativeInput(t, w, 100, "first queued task", false)
	nativeInput(t, w, 101, "/followup second queued task", false)
	replacement := w.client
	w.operationReturned(late)
	if replacement == nil || replacement == client || w.client != replacement || len(w.b.slots) != 1 || !sameBindingIdentity(binding, w.binding) || queueState(t, w, 2) != "done" || queueState(t, w, 101) != "pending" {
		t.Fatal("late quota failure retained the failed runtime or changed replacement ownership")
	}
	w.dispatch()
	accepted := waitOperation(t, w)
	requireStoreOK(t, accepted.err)
	w.operationReturned(accepted)
	if researchTraceCount(trace, "prompt", "first queued task") != 1 || researchTraceCount(trace, "prompt", "missing-terminal") != 1 {
		t.Fatal("quota retirement replayed prior work or lost the next prompt")
	}
}

func TestProviderUsageRejectsAccountChangeDuringRefresh(t *testing.T) {
	w, command, _ := replyUsageWorker(t, "new-value")
	seedProviderUsage(t, w)
	nativeRPC(t, w, "fixture_usage_account", map[string]any{"account": "PRIVATE_USAGE_ACCOUNT", "flipAfter": 3})
	endUsageReply(w)
	awaitReplyStatus(t, w)
	w.refreshProviderUsage(time.Now().Add(providerUsageInterval))
	awaitProviderUsage(t, w)
	nativeRPC(t, w, "fixture_usage_account", map[string]any{"account": "PRIVATE_USAGE_ACCOUNT"})
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	active := w.active
	endUsageReply(w)
	awaitReplyStatus(t, w)
	header := finalReplyStatus(t, w, active, "answer")
	if !strings.Contains(header, "70% 35%") || strings.Contains(header, "45%") {
		t.Fatalf("account change overwrote the previous account's last-good sample: %q", header)
	}
}

func TestProviderUsageRefreshDoesNotPreventIdleRelease(t *testing.T) {
	w, _, _ := replyUsageWorker(t, "normal")
	w.b.cfg.IdleTimeout = 10 * time.Minute
	endUsageReply(w)
	sample := receiveReplyStatus(t, w)
	w.lastActivity = time.Now().Add(-2 * w.b.cfg.IdleTimeout)
	before := w.binding
	w.operationReturned(sample)
	awaitProviderUsage(t, w)
	w.releaseIdleRuntime(time.Now())
	if w.client != nil || len(w.b.slots) != 0 || !sameBindingIdentity(before, w.binding) {
		t.Fatal("optional quota refresh kept an idle runtime alive or changed its session")
	}
}

func TestProviderUsageRefreshCannotChangeReplyProvider(t *testing.T) {
	w, _, _ := replyUsageWorker(t, "normal")
	w.event([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","model":"safe","provider":"anthropic","content":[{"type":"text","text":"answer"}]}}`))
	w.event([]byte(`{"type":"agent_end","messages":[]}`))
	awaitProviderUsage(t, w)
	header := finalReplyStatus(t, w, 2, "answer")
	if strings.Contains(header, "70%") {
		t.Fatalf("Codex quota was written into another provider's answer: %q", header)
	}
	nativeInput(t, w, 100, "missing-terminal", false)
	endUsageReply(w)
	awaitReplyStatus(t, w)
	if !strings.Contains(finalReplyStatus(t, w, 100, "answer"), "70% 35%") {
		t.Fatal("provider mismatch discarded the correctly scoped Codex cache")
	}
}
