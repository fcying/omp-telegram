package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/telegram"
)

func thinkingQueryFailureReceipt(t *testing.T, w *worker, stopped bool) string {
	t.Helper()
	var text string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox WHERE text LIKE 'Thinking level: %' ORDER BY id DESC LIMIT 1").Scan(&text))
	if !strings.Contains(text, "Thinking level: high") || !strings.Contains(text, "adjusted the requested max") || !strings.Contains(text, "Thinking: n/a") || !strings.Contains(text, "change succeeded") || !strings.Contains(text, "follow-up thinking query failed") {
		t.Fatalf("optional query failure lost the verified, adjusted thinking outcome: %q", text)
	}
	if strings.Contains(text, "runtime was closed") != stopped {
		t.Fatalf("receipt misreported runtime retirement: %q", text)
	}
	var receipts, failures int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE text LIKE 'Thinking level: %'").Scan(&receipts))
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE text LIKE '%could not be confirmed%' OR text LIKE '%PRIVATE_%' OR text LIKE '%task outcome is uncertain%'").Scan(&failures))
	if receipts != 1 || failures != 0 {
		t.Fatalf("thinking operation emitted duplicate or misleading receipts: successes=%d failures=%d", receipts, failures)
	}
	return text
}

func TestThinkingChangeOptionalConfiguredQueryFailure(t *testing.T) {
	for _, mode := range []string{"reject", "missing", "malformed", "unknown-ack", "hold"} {
		t.Run(mode, func(t *testing.T) {
			_, trace := fixtureRPCControlFiles(t, "")
			t.Setenv("OMP_TELEGRAM_FIXTURE_EFFORT_MODE", mode)
			t.Setenv("OMP_TELEGRAM_FIXTURE_EFFORT_AFTER", "2")
			t.Setenv("OMP_TELEGRAM_FIXTURE_CONFIGURED_THINKING", "low")
			w, f, command := setupWorkspaceWorker(t)
			w.b.cfg.QueueCapacity = 4
			command("/new " + t.TempDir())
			before, client, claim := w.binding, w.client, w.claimedSession
			if w.configuredThinking != "low" {
				t.Fatal("startup did not prime the thinking cache before the optional query failure")
			}
			command("/thinking")
			messageID := f.messageCount()
			data := resumeButtons(t, f)[6]["callback_data"].(string)
			w.callback(&telegram.CallbackQuery{ID: "thinking-query-failure", From: telegram.User{ID: 7}, Data: data})
			for _, kind := range []string{"model_state", "model_set_thinking"} {
				result := waitOperation(t, w)
				if result.kind != kind || result.err != nil {
					t.Fatalf("thinking prerequisite = %s, want %s, error=%v", result.kind, kind, result.err)
				}
				w.operationReturned(result)
			}
			failedQuery := waitFixtureRPCTrace(t, trace, "effort_failure", mode)
			acceptWorkerInput(t, w, 100, "queued after optional configured thinking failure")
			probeStarted := make(chan struct{})
			w.startOperation("thinking_queued_probe", client, func(ctx context.Context) (json.RawMessage, error) {
				close(probeStarted)
				return client.Call(ctx, "get_state", nil)
			}, 0, "")
			result := waitOperation(t, w)
			if result.kind != "model_verify_thinking" || result.err != nil {
				t.Fatalf("verified thinking change became an overall failure: kind=%s error=%v", result.kind, result.err)
			}
			stopped := mode == "unknown-ack" || mode == "hold"
			if client.Stopping() != stopped {
				t.Fatalf("fixture mode %s stopped=%t, want %t", mode, client.Stopping(), stopped)
			}
			w.operationReturned(result)
			receipt := thinkingQueryFailureReceipt(t, w, stopped)
			if menu := settingsMenuResult(t, f, messageID); menu != receipt {
				t.Fatalf("thinking menu lost the confirmed outcome: %q", menu)
			}
			assertSettingsMenuNeverInterrupted(t, f, messageID)
			if w.configuredThinking != "" {
				t.Fatal("optional query failure retained the previous configured thinking cache")
			}
			if !sameBindingIdentity(w.binding, before) || w.claimedSession != claim || !w.b.sessionInUse(w.sessionID) {
				t.Fatal("optional query failure changed the logical session binding or claim")
			}
			saved, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
			if err != nil || !saved.Running || !sameBindingIdentity(saved, before) {
				t.Fatalf("optional query failure changed the saved binding: %+v, error=%v", saved, err)
			}
			if stopped {
				assertRetiredOperationClient(t, w, client, before)
				if len(w.b.slots) != 0 || w.rpcExitPendingClientID != 0 || len(w.rpcOperations) != 0 {
					t.Fatal("stopped thinking client retained its runtime slot or RPC lane")
				}
				select {
				case <-probeStarted:
					t.Fatal("queued RPC ran on the unsafe process")
				default:
				}
			} else {
				probe := waitOperation(t, w)
				requireStoreOK(t, probe.err)
				w.operationReturned(probe)
				if w.client != client || w.runtime != runtimeConnected || len(w.b.slots) != 1 || w.controlInProgress() {
					t.Fatal("safe optional query failure retired the usable runtime")
				}
				assertThinkingLevel(t, w, "high")
				command("/status")
				var status string
				requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&status))
				if fields := statusFields(status); fields["Thinking"] != "n/a" {
					t.Fatalf("status inferred configured thinking from the verified effective level: %q", status)
				}
			}
			w.callback(&telegram.CallbackQuery{ID: "thinking-query-replay", From: telegram.User{ID: 7}, Data: data})
			if w.controlInProgress() || researchTraceCount(trace, "set_thinking_level", "") != 1 || settingsMenuResult(t, f, messageID) != receipt || w.client != nil && w.client != client {
				t.Fatal("completed thinking button replay restarted or changed the operation")
			}
			if len(w.queue) != 1 {
				t.Fatal("optional query result lost the queued user request")
			}
			w.dispatch()
			promptResult := waitOperation(t, w)
			requireStoreOK(t, promptResult.err)
			w.operationReturned(promptResult)
			prompt := waitFixtureRPCTrace(t, trace, "prompt", "queued after optional configured thinking failure")
			if (prompt.pid != failedQuery.pid) != stopped {
				t.Fatalf("queued request used the wrong process after %s: query=%s prompt=%s", mode, failedQuery.pid, prompt.pid)
			}
			if count := researchTraceCount(trace, "prompt", "queued after optional configured thinking failure"); count != 1 {
				t.Fatalf("queued request was replayed %d times", count)
			}
		})
	}
}

func TestThinkingQueryFailureDoneBeforeOperationReturn(t *testing.T) {
	_, trace := fixtureRPCControlFiles(t, "")
	t.Setenv("OMP_TELEGRAM_FIXTURE_EFFORT_MODE", "unknown-ack")
	t.Setenv("OMP_TELEGRAM_FIXTURE_EFFORT_AFTER", "2")
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	before, client, claim := w.binding, w.client, w.claimedSession
	command("/thinking")
	messageID := f.messageCount()
	data := resumeButtons(t, f)[6]["callback_data"].(string)
	w.callback(&telegram.CallbackQuery{ID: "thinking-early-done", From: telegram.User{ID: 7}, Data: data})
	for _, kind := range []string{"model_state", "model_set_thinking"} {
		result := waitOperation(t, w)
		if result.kind != kind || result.err != nil {
			t.Fatalf("thinking prerequisite = %s, want %s, error=%v", result.kind, kind, result.err)
		}
		w.operationReturned(result)
	}
	failedQuery := waitFixtureRPCTrace(t, trace, "effort_failure", "unknown-ack")
	select {
	case <-client.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("unsafe thinking query did not stop the process")
	}
	w.failed()
	if w.rpcExitPendingClientID != client.ID() || w.client != client || !w.controlInProgress() {
		t.Fatal("Done bypassed the active thinking operation's result fence")
	}
	acceptWorkerInput(t, w, 100, "queued after early thinking Done")
	probeStarted := make(chan struct{})
	w.startOperation("thinking_queued_probe", client, func(context.Context) (json.RawMessage, error) {
		close(probeStarted)
		return nil, nil
	}, 0, "")
	result := waitOperation(t, w)
	if result.kind != "model_verify_thinking" || result.err != nil {
		t.Fatalf("early Done lost the verified thinking outcome: kind=%s error=%v", result.kind, result.err)
	}
	w.operationReturned(result)
	assertRetiredOperationClient(t, w, client, before)
	receipt := thinkingQueryFailureReceipt(t, w, true)
	if settingsMenuResult(t, f, messageID) != receipt {
		t.Fatal("early Done lost the confirmed thinking menu outcome")
	}
	assertSettingsMenuNeverInterrupted(t, f, messageID)
	if w.rpcExitPendingClientID != 0 || len(w.b.slots) != 0 || len(w.rpcOperations) != 0 || w.claimedSession != claim || !w.b.sessionInUse(w.sessionID) || len(w.queue) != 1 {
		t.Fatal("early Done lost the session claim, runtime slot, queued request, or exit fence")
	}
	select {
	case <-probeStarted:
		t.Fatal("early Done allowed a queued RPC to run on the unsafe client")
	default:
	}
	w.dispatch()
	promptResult := waitOperation(t, w)
	requireStoreOK(t, promptResult.err)
	w.operationReturned(promptResult)
	prompt := waitFixtureRPCTrace(t, trace, "prompt", "queued after early thinking Done")
	if prompt.pid == failedQuery.pid {
		t.Fatal("early Done reused the unsafe process for the next user request")
	}
}
