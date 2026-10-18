package bridge

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/telegram"
)

func modelThinkingFailureReceipt(t *testing.T, w *worker, model string, stopped bool) string {
	t.Helper()
	var text string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox WHERE text LIKE 'Model switched to %' ORDER BY id DESC LIMIT 1").Scan(&text))
	if !strings.Contains(text, "Model switched to "+model+".") || !strings.Contains(text, "Thinking: n/a") || !strings.Contains(text, "model switch succeeded, but the follow-up thinking query failed") {
		t.Fatalf("optional thinking failure lost the confirmed model outcome: %q", text)
	}
	if strings.Contains(text, "runtime was closed") != stopped {
		t.Fatalf("receipt misreported runtime retirement: %q", text)
	}
	var receipts, failures int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE text LIKE 'Model switched to %'").Scan(&receipts))
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE text LIKE '%switch could not be confirmed%' OR text LIKE '%PRIVATE_EFFORT%' OR text LIKE '%task outcome is uncertain%'").Scan(&failures))
	if receipts != 1 || failures != 0 {
		t.Fatalf("model operation emitted duplicate or misleading receipts: successes=%d failures=%d", receipts, failures)
	}
	return text
}

func TestModelSwitchOptionalThinkingFailure(t *testing.T) {
	for _, selection := range []string{"direct", "role"} {
		for _, mode := range []string{"reject", "missing", "malformed", "unknown-ack", "hold"} {
			t.Run(selection+"/"+mode, func(t *testing.T) {
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
				var messageID int
				wantModel := "manual"
				if selection == "role" {
					command("/model")
					buttons := resumeButtons(t, f)
					messageID = f.messageCount()
					w.callback(&telegram.CallbackQuery{ID: "thinking-failure-role", From: telegram.User{ID: 7}, Data: buttons[2]["callback_data"].(string)})
					wantModel = "deep"
				} else {
					acceptWorkerInput(t, w, 100, "/model fixture/manual")
				}
				state := waitOperation(t, w)
				if state.kind != "model_state" || state.err != nil {
					t.Fatalf("model state prerequisite = %s, error=%v", state.kind, state.err)
				}
				w.operationReturned(state)
				failedQuery := waitFixtureRPCTrace(t, trace, "effort_failure", mode)
				acceptWorkerInput(t, w, 101, "queued after optional thinking failure")
				probeStarted := make(chan struct{})
				w.startOperation("model_queued_probe", client, func(ctx context.Context) (json.RawMessage, error) {
					close(probeStarted)
					return client.Call(ctx, "get_state", nil)
				}, 0, "")
				result := waitOperation(t, w)
				if result.err != nil {
					t.Fatalf("confirmed model change became an overall failure: %v", result.err)
				}
				stopped := mode == "unknown-ack" || mode == "hold"
				if client.Stopping() != stopped {
					t.Fatalf("fixture mode %s stopped=%t, want %t", mode, client.Stopping(), stopped)
				}
				w.operationReturned(result)
				receipt := modelThinkingFailureReceipt(t, w, "fixture/"+wantModel, stopped)
				if selection == "role" {
					menu := settingsMenuResult(t, f, messageID)
					if menu != receipt+"\nRole: slow" {
						t.Fatalf("role menu lost its confirmed outcome or selected role: %q", menu)
					}
					assertSettingsMenuNeverInterrupted(t, f, messageID)
				}
				if w.configuredThinking != "" {
					t.Fatal("optional thinking failure retained the previous model's thinking cache")
				}
				if !sameBindingIdentity(w.binding, before) || w.claimedSession != claim || !w.b.sessionInUse(w.sessionID) {
					t.Fatal("optional thinking failure changed the logical session binding or claim")
				}
				saved, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
				if err != nil || !saved.Running || !sameBindingIdentity(saved, before) {
					t.Fatalf("optional query failure changed the saved binding: %+v, error=%v", saved, err)
				}
				if stopped {
					assertRetiredOperationClient(t, w, client, before)
					if len(w.b.slots) != 0 || w.rpcExitPendingClientID != 0 || len(w.rpcOperations) != 0 {
						t.Fatal("stopped model client retained its runtime slot or RPC lane")
					}
					select {
					case <-probeStarted:
						t.Fatal("the next queued RPC ran on the unsafe process")
					default:
					}
				} else {
					probe := waitOperation(t, w)
					requireStoreOK(t, probe.err)
					w.operationReturned(probe)
					if w.client != client || w.runtime != runtimeConnected || len(w.b.slots) != 1 || w.controlInProgress() {
						t.Fatal("safe optional query failure retired the usable runtime")
					}
					assertPickerModel(t, w, "fixture", wantModel)
					command("/status")
					var status string
					requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&status))
					if fields := statusFields(status); fields["Thinking"] != "n/a" || fields["Model"] != "fixture/"+wantModel {
						t.Fatalf("status lost the changed native model or inferred unavailable thinking: %q", status)
					}
				}
				if len(w.queue) != 1 {
					t.Fatal("optional query result lost the queued user request")
				}
				w.dispatch()
				promptResult := waitOperation(t, w)
				requireStoreOK(t, promptResult.err)
				w.operationReturned(promptResult)
				prompt := waitFixtureRPCTrace(t, trace, "prompt", "queued after optional thinking failure")
				if (prompt.pid != failedQuery.pid) != stopped {
					t.Fatalf("queued request used the wrong process after %s: query=%s prompt=%s", mode, failedQuery.pid, prompt.pid)
				}
				if count := researchTraceCount(trace, "prompt", "queued after optional thinking failure"); count != 1 {
					t.Fatalf("queued request was replayed %d times", count)
				}
			})
		}
	}
}

func TestModelThinkingFailureDoneBeforeOperationReturn(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	t.Setenv("OMP_TELEGRAM_FIXTURE_EFFORT_MODE", "unknown-ack")
	t.Setenv("OMP_TELEGRAM_FIXTURE_EFFORT_AFTER", "2")
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	before, client, claim := w.binding, w.client, w.claimedSession
	releaseResult := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseResult) }) }
	t.Cleanup(release)
	w.beginControlOperation(controlModel)
	w.startOperation("model_set_role", client, func(ctx context.Context) (json.RawMessage, error) {
		model, err := client.SetModelRole(ctx, "slow")
		if err != nil {
			return nil, err
		}
		data, err := modelSelectionWithThinking(ctx, client, model)
		<-client.Done()
		select {
		case <-releaseResult:
		case <-ctx.Done():
		}
		return data, err
	}, 0, "", modelOperationRequest{action: "model_select", role: "slow"})
	failedQuery := waitFixtureRPCTrace(t, trace, "effort_failure", "unknown-ack")
	select {
	case <-client.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("unsafe thinking query did not stop the process")
	}
	w.failed()
	if w.rpcExitPendingClientID != client.ID() || w.client != client || !w.controlInProgress() {
		t.Fatal("Done bypassed the active model operation's result fence")
	}
	acceptWorkerInput(t, w, 100, "queued after early model Done")
	probeStarted := make(chan struct{})
	w.startOperation("model_queued_probe", client, func(context.Context) (json.RawMessage, error) {
		close(probeStarted)
		return nil, nil
	}, 0, "")
	release()
	result := waitOperation(t, w)
	requireStoreOK(t, result.err)
	w.operationReturned(result)
	assertRetiredOperationClient(t, w, client, before)
	modelThinkingFailureReceipt(t, w, "fixture/deep", true)
	if w.rpcExitPendingClientID != 0 || len(w.b.slots) != 0 || w.claimedSession != claim || !w.b.sessionInUse(w.sessionID) || len(w.queue) != 1 {
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
	prompt := waitFixtureRPCTrace(t, trace, "prompt", "queued after early model Done")
	if prompt.pid == failedQuery.pid {
		t.Fatal("early Done reused the unsafe process for the next user request")
	}
}

func TestModelThinkingOptionalQueryUsesParentDeadline(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_EFFORT_MODE", "hold")
	t.Setenv("OMP_TELEGRAM_FIXTURE_EFFORT_AFTER", "2")
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before, client := w.binding, w.client
	w.beginControlOperation(controlModel)
	w.startOperation("model_set_model", client, func(ctx context.Context) (json.RawMessage, error) {
		raw, err := client.Call(ctx, "set_model", map[string]any{"provider": "fixture", "modelId": "deadline"})
		if err != nil {
			return nil, err
		}
		var model omp.Model
		if err := json.Unmarshal(raw, &model); err != nil {
			return nil, err
		}
		queryCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		return modelSelectionWithThinking(queryCtx, client, model)
	}, 0, "", modelOperationRequest{action: "model_switch"})
	var result operationResult
	select {
	case result = <-w.operations:
	case <-time.After(2 * time.Second):
		t.Fatal("optional thinking query ignored the earlier parent deadline")
	}
	requireStoreOK(t, result.err)
	w.operationReturned(result)
	assertRetiredOperationClient(t, w, client, before)
	modelThinkingFailureReceipt(t, w, "fixture/deadline", true)
}
