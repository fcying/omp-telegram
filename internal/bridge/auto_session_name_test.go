package bridge

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/i18n"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func autoNameWorker(t *testing.T, enabled bool) (*worker, func(string), string) {
	t.Helper()
	t.Setenv("OMP_TELEGRAM_FIXTURE_NATIVE_COMMANDS", "auto-name")
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
	trace := filepath.Join(t.TempDir(), "rpc.trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.AutoSessionName = enabled
	w.b.cfg.StatusBar = config.StatusBar{Progress: [][]string{}, Final: [][]string{}}
	w.b.cfg.QueueCapacity = 16
	command("/new " + t.TempDir())
	nativeCatalogReady(t, w)
	return w, command, trace
}

func autoNameStateCheck(t *testing.T, w *worker) operationResult {
	t.Helper()
	nativeUntil(t, w, func() bool { return w.autoSessionName != nil })
	result := waitOperation(t, w)
	if result.kind != "auto_session_name_state" {
		t.Fatalf("expected automatic naming state check, got %s", result.kind)
	}
	return result
}

func TestAutoSessionNameOptInAndSingleAttempt(t *testing.T) {
	for _, scenario := range []string{"disabled", "enabled", "named", "rejected", "resume"} {
		t.Run(scenario, func(t *testing.T) {
			w, command, trace := autoNameWorker(t, scenario != "disabled")
			if scenario == "named" {
				command("/name Manual session")
			}
			if scenario == "rejected" {
				nativeRPC(t, w, "fixture_auto_name", map[string]any{"renameMode": "reject"})
			}
			if scenario == "resume" {
				id := w.sessionID
				command("/close")
				command("/resume " + id)
				nativeCatalogReady(t, w)
			}
			turn := w.turn
			nativeInput(t, w, 100, "Fix parser boundaries", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			researchBarrier(t, w)
			nativeInput(t, w, 101, "Add parser examples", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			researchBarrier(t, w)
			if scenario == "enabled" {
				// Confirmed admission must not impose metadata requirements on later /name.
				nativeRPC(t, w, "fixture_catalog_update", map[string]any{"sourceName": "session", "source": "file"})
				command("/name Manual session")
			}
			want := 0
			if scenario == "enabled" || scenario == "rejected" {
				want = 1
			}
			if got := researchTraceCount(trace, "prompt", "/rename"); got != want {
				t.Fatalf("automatic rename submissions=%d, want %d", got, want)
			}
			if w.client == nil || w.taskActive() || w.controlInProgress() || w.turn != turn+2 {
				t.Fatal("automatic naming changed runtime, task ownership, or turn count")
			}
			if body := researchBody(t, w, 100); body != "answer: Fix parser boundaries" {
				t.Fatalf("automatic naming changed the durable task body: %q", body)
			}
			if scenario == "named" || scenario == "enabled" {
				raw, err := w.call("get_state", nil)
				if err != nil || !strings.Contains(string(raw), "Manual session") {
					t.Fatal("automatic naming replaced the explicit session name")
				}
			}
		})
	}
}

func TestAutoSessionNameStartsAfterDurableSuccess(t *testing.T) {
	w, _, trace := autoNameWorker(t, true)
	nativeInput(t, w, 100, "/native_local", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	researchBarrier(t, w)
	if researchTraceCount(trace, "prompt", "/rename") != 0 {
		t.Fatal("local command triggered automatic naming")
	}
	nativeInput(t, w, 101, "wait", false)
	researchBarrier(t, w)
	if researchTraceCount(trace, "prompt", "/rename") != 0 {
		t.Fatal("unfinished task triggered automatic naming")
	}
	nativeRPC(t, w, "fixture_finish_root", nil)
	check := autoNameStateCheck(t, w)
	if queueState(t, w, 101) != "done" || researchBody(t, w, 101) != "root completed" {
		t.Fatal("automatic naming preceded durable task completion")
	}
	var delivered int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=101 AND state!='pending'").Scan(&delivered))
	if delivered != 0 || w.taskActive() || w.controlInProgress() || w.turn != 2 || len(w.queue) != 0 {
		t.Fatal("automatic naming required Telegram delivery or acquired task/control ownership")
	}
	w.operationReturned(check)
	researchBarrier(t, w)
	if researchTraceCount(trace, "prompt", "/rename") != 1 {
		t.Fatal("durably completed ordinary task did not trigger naming")
	}
}

func TestAutoSessionNameRechecksFullIdentityBeforeConsumption(t *testing.T) {
	for _, scenario := range []string{"client", "generation", "session", "epoch", "persisted", "state-id", "state-name", "builtin"} {
		t.Run(scenario, func(t *testing.T) {
			w, _, trace := autoNameWorker(t, true)
			if scenario == "state-id" || scenario == "state-name" {
				mode := "wrong-session"
				if scenario == "state-name" {
					mode = "malformed-name"
				}
				nativeRPC(t, w, "fixture_auto_name", map[string]any{"stateMode": mode})
			}
			nativeInput(t, w, 100, "Fix parser boundaries", false)
			check := autoNameStateCheck(t, w)
			client, binding, session := w.client, w.binding, w.sessionID
			switch scenario {
			case "client":
				w.client = nil
			case "generation":
				w.binding.Generation++
			case "session":
				w.sessionID = "replacement-session"
			case "epoch":
				w.client.InvalidateCommandCatalog()
			case "persisted":
				_, err := w.b.db.DB.Exec("UPDATE bindings SET session_id='replacement-session'")
				requireStoreOK(t, err)
			case "builtin":
				nativeRPC(t, w, "fixture_catalog_update", map[string]any{"sourceName": "rename", "source": "file"})
			}
			w.operationReturned(check)
			w.client, w.binding, w.sessionID = client, binding, session
			researchBarrier(t, w)
			stored, err := w.b.db.Binding(binding.Bot, binding.Chat, binding.Thread)
			requireStoreOK(t, err)
			if !stored.AutoSessionNamePending || researchTraceCount(trace, "prompt", "/rename") != 0 {
				t.Fatal("stale or malformed evidence consumed eligibility or submitted automatic naming")
			}
		})
	}
}

func TestAutoSessionNameStateFailureWaitsForNextSuccessfulTask(t *testing.T) {
	w, _, trace := autoNameWorker(t, true)
	nativeRPC(t, w, "fixture_auto_name", map[string]any{"stateMode": "reject"})
	nativeInput(t, w, 100, "Fix parser boundaries", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	researchBarrier(t, w)
	if !w.binding.AutoSessionNamePending || w.autoSessionNameReady || researchTraceCount(trace, "prompt", "/rename") != 0 {
		t.Fatal("state failure consumed eligibility or scheduled an immediate retry")
	}
	nativeRPC(t, w, "fixture_auto_name", map[string]any{"stateMode": ""})
	researchBarrier(t, w)
	if researchTraceCount(trace, "prompt", "/rename") != 0 {
		t.Fatal("clearing the state failure retried without another successful task")
	}
	nativeInput(t, w, 101, "Add parser examples", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	researchBarrier(t, w)
	if researchTraceCount(trace, "prompt", "/rename") != 1 {
		t.Fatal("next successful task could not use retained eligibility")
	}
}

func TestAutoSessionNameConsumeFailureKeepsRuntimeAndRetriesAfterSuccess(t *testing.T) {
	w, command, trace := autoNameWorker(t, true)
	w.b.fatal = make(chan error, 1)
	client, turn := w.client, w.turn
	_, err := w.b.db.DB.Exec(`CREATE TRIGGER reject_auto_session_name BEFORE UPDATE OF auto_session_name_pending ON bindings WHEN OLD.auto_session_name_pending=1 AND NEW.auto_session_name_pending=0 BEGIN SELECT RAISE(ABORT,'injected eligibility failure'); END`)
	requireStoreOK(t, err)
	nativeInput(t, w, 100, "Fix parser boundaries", false)
	w.operationReturned(autoNameStateCheck(t, w))
	select {
	case err := <-w.b.fatal:
		t.Fatalf("optional naming failure made the bridge fatal: %v", err)
	default:
	}
	if w.ctx.Err() != nil || w.client != client {
		t.Fatal("optional naming failure stopped the worker or replaced its runtime")
	}
	researchBarrier(t, w)
	if queueState(t, w, 100) != "done" || researchBody(t, w, 100) != "answer: Fix parser boundaries" {
		t.Fatal("optional naming failure changed the committed task result")
	}
	binding, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	requireStoreOK(t, err)
	if !binding.AutoSessionNamePending || !w.binding.AutoSessionNamePending || researchTraceCount(trace, "prompt", "/rename") != 0 {
		t.Fatal("failed eligibility consumption submitted naming or lost eligibility")
	}
	notices := nativeOutputs(t, w)
	if !strings.Contains(notices, w.tr(i18n.Msg.Session.AutoNameFailed)) || strings.Contains(notices, "injected eligibility failure") {
		t.Fatal("naming failure notice was missing or exposed the database error")
	}
	_, err = w.b.db.DB.Exec("DROP TRIGGER reject_auto_session_name")
	requireStoreOK(t, err)
	command("/status")
	researchBarrier(t, w)
	if researchTraceCount(trace, "prompt", "/rename") != 0 {
		t.Fatal("database recovery retried naming before another successful task")
	}
	nativeInput(t, w, 101, "Add parser examples", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	researchBarrier(t, w)
	if researchBody(t, w, 101) != "answer: Add parser examples" || w.client != client || w.turn != turn+2 {
		t.Fatal("the original runtime could not complete the next task normally")
	}
	binding, err = w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	requireStoreOK(t, err)
	if binding.AutoSessionNamePending || researchTraceCount(trace, "prompt", "/rename") != 1 {
		t.Fatal("the next successful task did not consume the retained eligibility exactly once")
	}
}

func TestAutoSessionNameYieldsToUserWorkAndIgnoresLateAck(t *testing.T) {
	w, _, trace := autoNameWorker(t, true)
	nativeRPC(t, w, "fixture_auto_name", map[string]any{"renameMode": "hold"})
	nativeInput(t, w, 100, "Fix parser boundaries", false)
	check := autoNameStateCheck(t, w)
	w.operationReturned(check)
	waitFixtureRPCTrace(t, trace, "auto_name_waiting", "")
	if w.controlInProgress() || w.taskActive() {
		t.Fatal("pending automatic naming occupied task/control state")
	}
	client := w.client
	nativeInput(t, w, 101, "wait", false)
	nativeUntil(t, w, func() bool { return w.active == 101 && !w.rpcOperationActive })
	nativeRPC(t, w, "fixture_auto_name", map[string]any{"release": true})
	researchBarrier(t, w)
	if w.client != client || w.active != 101 || w.turn != 2 || queueState(t, w, 100) != "done" {
		t.Fatal("late automatic naming acknowledgment changed the new task or runtime")
	}
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	researchBarrier(t, w)
	if researchTraceCount(trace, "prompt", "/rename") != 1 {
		t.Fatal("consumed automatic naming was replayed")
	}
}

func TestAutoSessionNamePreAdmissionManualPriority(t *testing.T) {
	for _, scenario := range []string{"cancel", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			w, _, trace := autoNameWorker(t, true)
			nativeRPC(t, w, "fixture_auto_name", map[string]any{"renameMode": "hold-admission"})
			nativeInput(t, w, 100, "Fix parser boundaries", false)
			w.operationReturned(autoNameStateCheck(t, w))
			waitFixtureRPCTrace(t, trace, "auto_name_waiting", "")
			client, turn := w.client, w.turn
			if scenario == "timeout" {
				w.operationReturned(waitOperation(t, w))
			}
			nativeInput(t, w, 101, "/name Operator chosen name", false)
			if scenario == "cancel" {
				w.operationReturned(waitOperation(t, w))
			}
			waitFixtureRPCTrace(t, trace, "auto_name_barrier_waiting", "")
			var state struct{ SessionName string }
			raw, err := w.call("get_state", nil)
			if err != nil || json.Unmarshal(raw, &state) != nil || state.SessionName != "" {
				t.Fatal("manual name was written before the old rename passed admission")
			}
			nativeRPC(t, w, "fixture_auto_name", map[string]any{"release": true})
			researchBarrier(t, w)
			raw, err = w.call("get_state", nil)
			if err != nil || json.Unmarshal(raw, &state) != nil || state.SessionName != "Operator chosen name" {
				t.Fatal("delayed automatic naming overwrote the manual name")
			}
			if w.client != client || w.turn != turn || queueState(t, w, 100) != "done" || researchTraceCount(trace, "prompt", "/rename") != 1 {
				t.Fatal("manual naming retired the runtime, changed task ownership, or replayed automatic naming")
			}
		})
	}
}

func TestAutoSessionNameUnconfirmedBarrierRejectsManualWrite(t *testing.T) {
	for _, scenario := range []string{"source-revoked", "query-rejected"} {
		t.Run(scenario, func(t *testing.T) {
			w, command, trace := autoNameWorker(t, true)
			nativeRPC(t, w, "fixture_auto_name", map[string]any{"renameMode": "hold-admission"})
			nativeInput(t, w, 100, "Fix parser boundaries", false)
			w.operationReturned(autoNameStateCheck(t, w))
			waitFixtureRPCTrace(t, trace, "auto_name_waiting", "")
			if scenario == "source-revoked" {
				nativeRPC(t, w, "fixture_catalog_update", map[string]any{"sourceName": "session", "source": "file"})
			} else {
				nativeRPC(t, w, "fixture_auto_name", map[string]any{"barrierMode": "reject"})
			}
			client, session := w.client, w.sessionID
			command("/name Operator chosen name")
			researchBarrier(t, w)
			var state struct{ SessionName string }
			raw, err := w.call("get_state", nil)
			if err != nil || json.Unmarshal(raw, &state) != nil || state.SessionName != "" {
				t.Fatal("unconfirmed admission barrier allowed a manual name write")
			}
			if w.client != client || !strings.Contains(nativeOutputs(t, w), w.tr(i18n.Msg.SessionNameChangeCouldNotConfirmedUseStatusCheck)) {
				t.Fatal("failed naming barrier retired the runtime or claimed name success")
			}
			nativeRPC(t, w, "fixture_auto_name", map[string]any{"release": true})
			command("/close")
			command("/resume " + session)
			command("/name Operator chosen name")
			raw, err = w.call("get_state", nil)
			if err != nil || json.Unmarshal(raw, &state) != nil || state.SessionName != "Operator chosen name" {
				t.Fatal("explicit runtime replacement retained an old naming fence")
			}
		})
	}
}

func TestAutoSessionNameCheckYieldsWithoutConsuming(t *testing.T) {
	w, _, trace := autoNameWorker(t, true)
	nativeInput(t, w, 100, "Fix parser boundaries", false)
	check := autoNameStateCheck(t, w)
	// A control arriving between state retrieval and submission takes precedence.
	nativeInput(t, w, 101, "/name Manual session", false)
	w.operationReturned(check)
	researchBarrier(t, w)
	if researchTraceCount(trace, "prompt", "/rename") != 0 {
		t.Fatal("automatic naming overtook a manual control")
	}
	var state struct{ SessionName string }
	raw, err := w.call("get_state", nil)
	if err != nil || json.Unmarshal(raw, &state) != nil || state.SessionName != "Manual session" {
		t.Fatal("manual name did not survive the cancelled automatic check")
	}
}

func TestAutoSessionNameRequiresSuccessfulResultCommit(t *testing.T) {
	for _, scenario := range []string{"cancelled", "failed", "commit-rejected"} {
		t.Run(scenario, func(t *testing.T) {
			w, _, trace := autoNameWorker(t, true)
			fastRoot(t, w)
			reason := "aborted"
			if scenario == "failed" {
				reason = "error"
			}
			if scenario == "commit-rejected" {
				reason = "stop"
				_, err := w.b.db.DB.Exec(`CREATE TRIGGER reject_task_completion BEFORE UPDATE OF state ON inbox WHEN NEW.id=100 AND NEW.state='done' BEGIN SELECT RAISE(ABORT,'injected completion failure'); END`)
				requireStoreOK(t, err)
			}
			event, err := json.Marshal(map[string]any{"type": "agent_end", "isTerminal": true, "messages": []any{map[string]any{"role": "assistant", "stopReason": reason, "content": []any{map[string]any{"type": "text", "text": "root result"}}}}})
			requireStoreOK(t, err)
			w.event(event)
			w.dispatch()
			if w.autoSessionNameReady || w.autoSessionName != nil || researchTraceCount(trace, "prompt", "/rename") != 0 {
				t.Fatal("unsuccessful task/result transaction triggered automatic naming")
			}
			if queueState(t, w, 100) == "done" {
				t.Fatal("unsuccessful task/result transaction was reported as done")
			}
		})
	}
}

func TestAutoSessionNameYieldsToIdleControls(t *testing.T) {
	for _, phase := range []string{"state", "ack"} {
		for _, control := range []string{"args", "args-menu", "migrate"} {
			t.Run(phase+"/"+control, func(t *testing.T) {
				if control == "migrate" {
					t.Setenv("OMP_TELEGRAM_FIXTURE_MIGRATION", "success")
				}
				w, _, trace := autoNameWorker(t, true)
				var query *telegram.CallbackQuery
				if control != "migrate" {
					configureTestArgPresets(w.b, [][]string{nil, {"--config", testConfigOverlay(t)}}, strings.Repeat("a", 64), 1)
				}
				if phase == "ack" {
					nativeRPC(t, w, "fixture_auto_name", map[string]any{"renameMode": "hold"})
				}
				nativeInput(t, w, 90, "Fix parser boundaries", false)
				check := autoNameStateCheck(t, w)
				if control == "args-menu" {
					// Completion expires older menus; race a current menu's callback.
					w.showArgsMenuPage(confirmation{action: "args", user: 7}, 0)
					fake := http.DefaultTransport.(*fakeHTTP)
					_, rows := pickerView(t, fake)
					query = &telegram.CallbackQuery{ID: "auto-name-args", From: telegram.User{ID: 7}, Data: rows[1][0]["callback_data"].(string), Message: &telegram.Message{MessageID: int64(fake.messageCount()), MessageThreadID: w.key.thread, Chat: telegram.Chat{ID: w.key.chat}}}
				}
				if phase == "ack" {
					w.operationReturned(check)
					waitFixtureRPCTrace(t, trace, "auto_name_waiting", "")
				}
				client, source := w.client, w.binding
				w.b.cfg.IdleTimeout = time.Second
				w.releaseIdleRuntime(w.lastActivity.Add(1500 * time.Millisecond))
				if w.client != client {
					t.Fatal("idle timeout retired the automatic naming operation before control preemption")
				}
				if query != nil {
					u := update(100, w.key.thread, "")
					u.Message, u.CallbackQuery = nil, query
					raw, err := json.Marshal(u)
					requireStoreOK(t, err)
					requireStoreOK(t, w.b.db.Accept(100, raw))
					w.handle(incoming{id: 100, callback: query})
				} else {
					text := "/args 2"
					if control == "migrate" {
						text = "/migrate"
					}
					nativeInput(t, w, 100, text, false)
				}
				if queueState(t, w, 100) != "pending" || !w.rpcOperationActive || w.controlInProgress() {
					t.Fatal("control ran before the cancelled automatic operation released its lane")
				}
				w.releaseIdleRuntime(w.lastActivity.Add(1500 * time.Millisecond))
				if w.client != client {
					t.Fatal("idle timeout retired the cancelled operation before its result was handled")
				}
				if phase == "state" {
					w.operationReturned(check)
				}
				if control == "migrate" {
					nativeUntil(t, w, func() bool { return w.migrationRun != nil })
					for _, stage := range []string{"prepare", "handoff", "candidate"} {
						migrationWorkflowStep(t, w, stage)
					}
					m, err := w.b.db.Migration(source.Bot, source.Chat, source.Thread)
					requireStoreOK(t, err)
					migrationWorkflowStep(t, w, "verify")
					migrationWorkflowCommitted(t, w, source, m.CandidateID, m.CandidateFile)
				} else {
					nativeUntil(t, w, func() bool { return !w.rpcOperationActive && !w.controlInProgress() && w.client != client })
					requireArgsSelection(t, w.b, 2)
					requireArgsBinding(t, w, source)
				}
				if strings.Contains(nativeOutputs(t, w), w.tr(i18n.Msg.MigrationOnlyAllowedWhenTasksQueueSessionControlsAll)) || strings.Contains(nativeOutputs(t, w), w.tr(i18n.Msg.ConversationBusyUseAgainAfterTaskQueueFinish, "/args")) {
					t.Fatal("cancelled automatic naming rejected the user control as busy")
				}
			})
		}
	}
}

func TestAutoSessionNameDeferredInputIsCancelledOnShutdown(t *testing.T) {
	w, _, trace := autoNameWorker(t, true)
	nativeInput(t, w, 100, "Fix parser boundaries", false)
	check := autoNameStateCheck(t, w)
	nativeInput(t, w, 101, "Do not submit after shutdown", false)
	w.cancel()
	w.teardownWorker(true)
	w.operationReturned(check)
	w.background.Wait()
	if queueState(t, w, 101) != string(store.InboxCancelled) || researchTraceCount(trace, "prompt", "Do not submit after shutdown") != 0 {
		t.Fatal("shutdown lost or submitted the deferred input")
	}
}

func TestAutoSessionNameFinishedAllowsIdleRelease(t *testing.T) {
	w, _, _ := autoNameWorker(t, true)
	nativeInput(t, w, 100, "Fix parser boundaries", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	researchBarrier(t, w)
	before := w.binding
	w.b.cfg.IdleTimeout = time.Second
	w.releaseIdleRuntime(w.lastActivity.Add(1500 * time.Millisecond))
	if w.client != nil || w.runtime != runtimeReleased || !sameBindingIdentity(w.binding, before) {
		t.Fatal("completed automatic naming prevented idle release or changed the saved session")
	}
}
