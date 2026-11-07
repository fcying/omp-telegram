package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/i18n"
	"omp-telegram/internal/telegram"
)

func TestAssistantStatusBarTransitionsPreserveDeliveredBody(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		for _, intermediate := range []bool{false, true} {
			for _, final := range []bool{false, true} {
				t.Run(fmt.Sprintf("delivered=%t/intermediate=%t/final=%t", delivered, intermediate, final), func(t *testing.T) {
					w, f, _, _ := replyStatusWorker(t)
					w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{}}
					if intermediate {
						w.b.cfg.StatusBar.Intermediate = []string{"model", "status"}
					}
					if final {
						w.b.cfg.StatusBar.Final = []string{"speed", "status"}
					}
					replyStatusMessage(t, w, 100, 1000, 1100)
					var id int64
					var initialHeader, body string
					requireStoreOK(t, w.b.db.DB.QueryRow("SELECT id,header,text FROM outbox WHERE inbox_id=2").Scan(&id, &initialHeader, &body))
					if body != "answer" || (initialHeader != "") != intermediate {
						t.Fatalf("initial assistant delivery changed body or header visibility: %q/%q", initialHeader, body)
					}
					if delivered {
						deliverReplies(t, w)
					}
					endReplyStatus(w)
					awaitReplyStatus(t, w)
					var finalID int64
					var header string
					requireStoreOK(t, w.b.db.DB.QueryRow("SELECT id,header,text FROM outbox WHERE inbox_id=2").Scan(&finalID, &header, &body))
					if finalID != id || body != "answer" || queueState(t, w, 2) != "done" {
						t.Fatalf("completion replaced/replayed the immutable answer: %d/%q", finalID, body)
					}
					if final {
						if !strings.HasPrefix(header, "100 t/s") || !strings.HasSuffix(header, assistantCompletedIcon) || strings.Contains(header, "safe") {
							t.Fatalf("final field selection/order was not applied: %q", header)
						}
					} else if header != "" {
						t.Fatalf("disabled final header retained intermediate metadata: %q", header)
					}
					deliverReplies(t, w)
					f.mu.Lock()
					defer f.mu.Unlock()
					wantRequests := 1
					if delivered && (intermediate || final) {
						wantRequests++
					}
					if len(f.messages) != wantRequests {
						t.Fatalf("header transition duplicated/dropped a delivery: %+v", f.messages)
					}
					last := f.messages[len(f.messages)-1]
					text, _ := last["text"].(string)
					if !final && text != "answer" {
						t.Fatalf("disabled final header left whitespace/icon on Telegram: %q", text)
					}
					if wantRequests == 2 && last["message_id"] != float64(1) {
						t.Fatalf("final metadata was sent as a new message instead of editing: %+v", last)
					}
				})
			}
		}
	}
}

func TestHeaderlessIntermediateReservesSpaceForFinalHeader(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	w.b.cfg.StatusBar.Intermediate = []string{}
	w.b.cfg.StatusBar.Final = []string{"status", "model"}
	text := strings.Repeat("x", telegram.MaxMessageUTF16)
	w.stream.WriteString(text)
	if !w.queueAssistantText(text, false) {
		t.Fatal("body was not queued")
	}
	deliverReplies(t, w)
	w.finishReplyWithStatus(false)
	awaitReplyStatus(t, w)
	deliverReplies(t, w)
	f.mu.Lock()
	defer f.mu.Unlock()
	var sent strings.Builder
	var edited bool
	for _, message := range f.messages {
		body, _ := message["text"].(string)
		if utf16Length(body) > telegram.MaxMessageUTF16 {
			t.Fatalf("late header exceeded Telegram limit: %d", utf16Length(body))
		}
		if message["message_id"] == nil {
			sent.WriteString(body)
		} else {
			edited = true
			if !strings.HasPrefix(body, assistantCompletedIcon) {
				t.Fatalf("late final header was not attached: %q", body)
			}
		}
	}
	if sent.String() != text || !edited {
		t.Fatal("header reservation changed/suppressed the body or missed its late header")
	}
}

func TestDisabledStatusBarsDoNotSampleOrHideProgressControls(t *testing.T) {
	w, _, _, trace := replyStatusWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{}}
	allowInitialProgress(w)
	before := researchTraceCount(trace, "get_state", "")
	w.refreshProgressNative(time.Now())
	if w.progressNative.pending != nil {
		t.Fatal("disabled display started native sampling")
	}
	w.startProgressTool("one", "bash")
	text := w.renderProgress()
	if !strings.Contains(text, "bash") || strings.Contains(text, "⚙") || strings.HasPrefix(text, "\n") {
		t.Fatalf("disabled header hid tool body or retained a blank header: %q", text)
	}
	if w.progressKeyboard("stop-test") == nil {
		t.Fatal("disabled progress header removed its Stop control")
	}
	replyStatusMessage(t, w, 100, 1000, 1100)
	endReplyStatus(w)
	if w.replyStatus.pending != nil || queueState(t, w, 2) != "done" || researchTraceCount(trace, "get_state", "") != before || researchTraceCount(trace, "prompt", "/usage") != 0 {
		t.Fatal("disabled display delayed settlement or started display-only RPC work")
	}
	if got := researchReplies(t, w, 2); len(got) != 1 || got[0] != "answer" {
		t.Fatalf("disabled status bars changed the result: %q", got)
	}
}

func statusBarGit(t *testing.T, workspace string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", workspace}, args...)...)
	command.Env = replyGitEnvironment(os.Environ())
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("temporary Git fixture failed: %v: %s", err, output)
	}
}

func TestFinalGitAndLateQuotaKeepSameAnswer(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "usage-release")
	t.Setenv("OMP_TELEGRAM_FIXTURE_USAGE_RELEASE", gate)
	w, _, _ := replyUsageWorker(t, "hold-report")
	t.Cleanup(func() { requireStoreOK(t, os.WriteFile(gate, nil, 0600)) })
	w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{"git", "quota", "status"}}
	workspace := w.binding.Workspace
	statusBarGit(t, workspace, "init", "-b", "before")
	w.event([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","model":"safe","provider":"openai-codex","duration":1000,"usage":{"output":100,"totalTokens":1100},"content":[{"type":"text","text":"answer"}]}}`))
	// The final repository snapshot must not come from task startup.
	statusBarGit(t, workspace, "symbolic-ref", "HEAD", "refs/heads/feature/ui")
	requireStoreOK(t, os.WriteFile(filepath.Join(workspace, "untracked"), []byte("changed"), 0600))
	w.event([]byte(`{"type":"agent_end","messages":[]}`))
	awaitReplyStatus(t, w)
	var id int64
	var header, body string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT id,header,text FROM outbox WHERE inbox_id=2").Scan(&id, &header, &body))
	if !strings.HasPrefix(header, "`feature/ui*`") || strings.Contains(header, "quota ?") || body != "answer" {
		t.Fatalf("fresh final Git snapshot was missing or changed the answer: %q/%q", header, body)
	}
	if !strings.Contains(telegram.ConvertMarkdown(header), "<code>feature/ui*</code>") {
		t.Fatalf("Telegram swallowed the dirty marker: %q", header)
	}
	deliverReplies(t, w)
	requireStoreOK(t, os.WriteFile(gate, nil, 0600))
	awaitProviderUsage(t, w)
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT header,text FROM outbox WHERE id=?", id).Scan(&header, &body))
	if !strings.HasPrefix(header, "`feature/ui*`") || !strings.Contains(header, "5h30% 7d65%") || body != "answer" {
		t.Fatalf("late quota edit dropped Git state or changed immutable body: %q/%q", header, body)
	}
	deliverReplies(t, w)
}

func TestTerminalOnlyQuotaHeaderWaitsForRealValue(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "usage-release")
	t.Setenv("OMP_TELEGRAM_FIXTURE_USAGE_RELEASE", gate)
	w, _, _ := replyUsageWorker(t, "hold-report")
	t.Cleanup(func() { requireStoreOK(t, os.WriteFile(gate, nil, 0600)) })
	w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{"quota"}}
	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","stopReason":"stop","model":"safe","provider":"openai-codex","content":[{"type":"text","text":"answer"}]}]}`))
	awaitReplyStatus(t, w)
	var id int64
	var header, body string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT id,header,text FROM outbox WHERE inbox_id=2").Scan(&id, &header, &body))
	if header != "" || body != "answer" || queueState(t, w, 2) != "done" {
		t.Fatalf("unknown quota delayed the result or produced a placeholder: %q/%q", header, body)
	}
	deliverReplies(t, w)
	requireStoreOK(t, os.WriteFile(gate, nil, 0600))
	awaitProviderUsage(t, w)
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT header,text FROM outbox WHERE id=?", id).Scan(&header, &body))
	if header != "5h30% 7d65%" || body != "answer" {
		t.Fatalf("real quota failed to attach to the initially headerless answer: %q/%q", header, body)
	}
	deliverReplies(t, w)
}

func TestResearchStatusBarsApplyGitOnlyAtRootSettlement(t *testing.T) {
	w, _, _ := researchWorker(t, "autoresearch")
	w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{"git", "status"}}
	researchRoot(t, w, 100, "/autoresearch status bar")
	workspace := w.binding.Workspace
	statusBarGit(t, workspace, "init", "-b", "main")
	researchRound(t, w, "middle")
	awaitReplyStatus(t, w)
	if rows := researchReplies(t, w, 100); len(rows) != 1 || rows[0] != "middle" || queueState(t, w, 100) != "submitted" {
		t.Fatalf("round boundary applied final metadata or completed the owner: %q", rows)
	}
	statusBarGit(t, workspace, "symbolic-ref", "HEAD", "refs/heads/final-branch")
	requireStoreOK(t, os.WriteFile(filepath.Join(workspace, "changed"), []byte("dirty"), 0600))
	nativeRPC(t, w, "fixture_autoresearch_continue", nil)
	nativeUntil(t, w, func() bool { return w.research.roundOpen })
	researchFinishNaturally(t, w, "final")
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	rows := researchReplies(t, w, 100)
	if len(rows) != 2 || rows[0] != "middle" {
		t.Fatalf("research completion changed or duplicated round bodies: %q", rows)
	}
	header, body := attachedReplyBody(t, rows[1])
	if header != "`final-branch*` | "+assistantCompletedIcon || body != "final" {
		t.Fatalf("research final metadata did not use the settlement workspace: %q", rows[1])
	}
}

func TestIntermediateOnlyQuotaWarmsCacheAfterShortTask(t *testing.T) {
	w, _, trace := replyUsageWorker(t, "normal")
	w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{"quota"}, Final: []string{}}
	endUsageReply(w)
	if queueState(t, w, 2) != "done" || w.replyStatus.pending != nil {
		t.Fatal("intermediate-only quota delayed a headerless completion")
	}
	awaitProviderUsage(t, w)
	if researchTraceCount(trace, "prompt", "/usage") != 1 {
		t.Fatal("short task failed to warm quota selected outside the final header")
	}
	if got := researchReplies(t, w, 2); len(got) != 1 || got[0] != "answer" {
		t.Fatalf("idle cache refresh added a disabled final header: %q", got)
	}
	nativeInput(t, w, 100, "missing-terminal", false)
	allowInitialProgress(w)
	w.refreshProgressNative(time.Now())
	if !w.progressNativeFinished(receiveProgressNative(t, w)) {
		t.Fatal("same-account live read was not accepted")
	}
	w.event([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"toolUse","model":"safe","provider":"openai-codex","content":[{"type":"text","text":"middle"}]}}`))
	header, body := attachedReplyBody(t, researchReplies(t, w, 100)[0])
	if header != "5h30% 7d65%" || body != "middle" || researchTraceCount(trace, "prompt", "/usage") != 1 {
		t.Fatalf("intermediate header missed the idle cache or triggered another query: %q/%q", header, body)
	}
}

func TestQuotaInDisabledProgressDoesNotStartQueries(t *testing.T) {
	w, _, trace := replyUsageWorker(t, "normal")
	w.b.cfg.StatusBar = config.StatusBar{Progress: []string{"quota"}, Intermediate: []string{}, Final: []string{}}
	before := researchTraceCount(trace, "get_state", "")
	endUsageReply(w)
	w.refreshProviderUsage(time.Now())
	if w.providerUsage.pending != nil || researchTraceCount(trace, "get_state", "") != before || researchTraceCount(trace, "prompt", "/usage") != 0 {
		t.Fatal("disabled Progress still enabled quota sampling")
	}
}

func endStatusBarBody(t *testing.T, w *worker, text, provider string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "agent_end", "isTerminal": true, "messageCount": w.assistantDelivery.messages + 1,
		"messages": []any{map[string]any{
			"role": "assistant", "stopReason": "stop", "model": "safe", "provider": provider,
			"content": []any{map[string]any{"type": "text", "text": text}},
		}},
	})
	requireStoreOK(t, err)
	w.event(raw)
}

func TestResearchFinalThinkingPreservesActorSelection(t *testing.T) {
	for _, scenario := range []struct{ configured, symbol string }{
		{"auto", "⟳"},
		{"medium", "◑"},
	} {
		t.Run(scenario.configured, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_CONFIGURED_THINKING", scenario.configured)
			w, _, _ := researchWorker(t, "autoresearch")
			w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{"thinking"}, Final: []string{"thinking"}}
			researchRoot(t, w, 100, "/autoresearch thinking selection")
			researchRound(t, w, "middle")
			awaitReplyStatus(t, w)
			rows := researchReplies(t, w, 100)
			if len(rows) != 1 || rows[0] != scenario.symbol+"\n\nmiddle" || queueState(t, w, 100) != "submitted" {
				t.Fatalf("round lost actor thinking or completed its root: %q", rows)
			}
			nativeRPC(t, w, "fixture_autoresearch_continue", nil)
			nativeUntil(t, w, func() bool { return w.research.roundOpen })
			researchFinishNaturally(t, w, "final")
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			rows = researchReplies(t, w, 100)
			if len(rows) != 2 || rows[0] != scenario.symbol+"\n\nmiddle" || rows[1] != scenario.symbol+"\n\nfinal" {
				t.Fatalf("root settlement lost actor thinking or duplicated a body: %q", rows)
			}
		})
	}
}

func TestRealTruncationNoticeBodyRemainsFinalHeaderTarget(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		for _, earlier := range []bool{false, true} {
			t.Run(fmt.Sprintf("delivered=%t/earlier=%t", delivered, earlier), func(t *testing.T) {
				w, f, _, _ := replyStatusWorker(t)
				w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{"status"}}
				if earlier {
					assistantMessage(t, w, "earlier")
				}
				notice := w.tr(i18n.Msg.ReplyTruncated)
				stoppedAssistantMessage(t, w, notice)
				var target int64
				var header, body string
				var editable bool
				requireStoreOK(t, w.b.db.DB.QueryRow("SELECT id,header,text,header_editable FROM outbox WHERE inbox_id=2 ORDER BY id DESC LIMIT 1").Scan(&target, &header, &body, &editable))
				if header != "" || body != notice || !editable || w.assistantDelivery.statusOutboxID != target {
					t.Fatalf("real body was classified as synthetic notice: %q/%q/%t", header, body, editable)
				}
				if delivered {
					deliverReplies(t, w)
				}
				w.event([]byte(`{"type":"agent_end","messages":[]}`))
				awaitReplyStatus(t, w)
				var finalID int64
				requireStoreOK(t, w.b.db.DB.QueryRow("SELECT id,header,text FROM outbox WHERE inbox_id=2 ORDER BY id DESC LIMIT 1").Scan(&finalID, &header, &body))
				rows := researchReplies(t, w, 2)
				wantBodies := 1
				if earlier {
					wantBodies++
				}
				if finalID != target || header != assistantCompletedIcon || body != notice || len(rows) != wantBodies || queueState(t, w, 2) != "done" {
					t.Fatalf("final header missed immutable latest body or added an orphan: %q", rows)
				}
				if earlier && rows[0] != "earlier" {
					t.Fatalf("final header changed earlier reply: %q", rows)
				}
				deliverReplies(t, w)
				f.mu.Lock()
				defer f.mu.Unlock()
				wantRequests := wantBodies
				if delivered {
					wantRequests++
				}
				last := f.messages[len(f.messages)-1]
				if len(f.messages) != wantRequests || last["text"] != telegram.ConvertMarkdown(assistantCompletedIcon+"\n\n"+notice) {
					t.Fatalf("Telegram final header lost or duplicated body: %+v", f.messages)
				}
				if delivered && last["message_id"] != float64(wantBodies) {
					t.Fatalf("final header edited the wrong body: %+v", last)
				}
			})
		}
	}
}

func TestTerminalTruncationNoticeBodyReceivesLateQuota(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "usage-release")
	t.Setenv("OMP_TELEGRAM_FIXTURE_USAGE_RELEASE", gate)
	w, _, trace := replyUsageWorker(t, "hold-report")
	t.Cleanup(func() { requireStoreOK(t, os.WriteFile(gate, nil, 0600)) })
	w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{"quota"}}
	assistantMessage(t, w, "earlier")
	deliverReplies(t, w)
	notice := w.tr(i18n.Msg.ReplyTruncated)
	endStatusBarBody(t, w, notice, "openai-codex")
	awaitReplyStatus(t, w)
	waitFixtureRPCTrace(t, trace, "usage_waiting", "")
	var target int64
	var header, body string
	var editable bool
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT id,header,text,header_editable FROM outbox WHERE inbox_id=2 ORDER BY id DESC LIMIT 1").Scan(&target, &header, &body, &editable))
	if header != "" || body != notice || !editable || queueState(t, w, 2) != "done" {
		t.Fatalf("terminal real body was not retained as an editable target: %q/%q/%t", header, body, editable)
	}
	deliverReplies(t, w)
	requireStoreOK(t, os.WriteFile(gate, nil, 0600))
	awaitProviderUsage(t, w)
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT header,text FROM outbox WHERE id=?", target).Scan(&header, &body))
	rows := researchReplies(t, w, 2)
	if header != "5h30% 7d65%" || body != notice || len(rows) != 2 || rows[0] != "earlier" {
		t.Fatalf("late quota edited an earlier reply or duplicated the final body: %q", rows)
	}
	deliverReplies(t, w)
}

func TestBudgetExhaustedNoticeKeepsEarlierBodyTarget(t *testing.T) {
	for _, terminalOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminalOnly=%t", terminalOnly), func(t *testing.T) {
			w, _, _, _ := replyStatusWorker(t)
			w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{"status"}}
			for range maxFinalReplyMessages - 2 {
				stoppedAssistantMessage(t, w, "body")
			}
			target := w.assistantDelivery.statusOutboxID
			if terminalOnly {
				endStatusBarBody(t, w, "over budget", "fixture")
			} else {
				stoppedAssistantMessage(t, w, "over budget")
				if w.assistantDelivery.statusOutboxID != target {
					t.Fatal("synthetic notice replaced the earlier actual body target")
				}
				w.event([]byte(`{"type":"agent_end","messages":[]}`))
			}
			awaitReplyStatus(t, w)
			var header, body string
			var editable bool
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT header,text,header_editable FROM outbox WHERE inbox_id=2 ORDER BY id DESC LIMIT 1").Scan(&header, &body, &editable))
			if header != "" || body != w.tr(i18n.Msg.ReplyTruncated) || editable {
				t.Fatalf("synthetic notice became the final header target: %q/%q/%t", header, body, editable)
			}
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT header,text FROM outbox WHERE id=?", target).Scan(&header, &body))
			rows := researchReplies(t, w, 2)
			if header != assistantCompletedIcon || body != "body" || len(rows) != maxFinalReplyMessages-1 || queueState(t, w, 2) != "done" {
				t.Fatalf("budget exhaustion lost earlier target, duplicated body or orphaned header: %q", rows)
			}
		})
	}
}
