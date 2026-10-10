package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/media"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func nativeWorker(t *testing.T, mode string) (*worker, func(string), string) {
	t.Helper()
	t.Setenv("OMP_TELEGRAM_FIXTURE_NATIVE_COMMANDS", mode)
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
	trace := filepath.Join(t.TempDir(), "rpc.trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 16
	w.b.bot.Username = "fixture_bot"
	command("/new " + t.TempDir())
	return w, command, trace
}

// Drive the same asynchronous inputs as the worker actor, including reader events.
func nativeUntil(t *testing.T, w *worker, ready func() bool) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for !ready() {
		var events <-chan json.RawMessage
		if w.client != nil {
			events = w.client.Events()
		}
		select {
		case result := <-w.nativeCatalog.results:
			w.commandCatalogFinished(result)
		case result := <-w.operations:
			w.operationReturned(result)
		case raw, ok := <-events:
			if !ok {
				w.failed()
			} else {
				w.event(raw)
			}
		case <-timer.C:
			t.Fatal("native command actor did not reach expected state")
		}
		w.dispatch()
	}
}

func nativeCatalogReady(t *testing.T, w *worker) {
	t.Helper()
	nativeUntil(t, w, func() bool { return w.nativeCatalog.cancel == nil })
	if w.client == nil || w.client.CommandCatalog().State != omp.CatalogReady {
		t.Fatal("native command catalog is not ready")
	}
}

func nativeInput(t *testing.T, w *worker, id int64, text string, reply bool) {
	t.Helper()
	u := update(id, 11, text)
	if reply {
		u.Message.ReplyToMessage = &telegram.Message{Text: "quoted context"}
	}
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	requireStoreOK(t, w.b.db.Accept(id, raw))
	w.handle(incoming{id: id, msg: u.Message})
	w.dispatch()
}

func nativeRPC(t *testing.T, w *worker, command string, fields map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
	defer cancel()
	if _, err := w.client.Call(ctx, command, fields); err != nil {
		t.Fatal(err)
	}
}

func nativeOutputs(t *testing.T, w *worker) string {
	t.Helper()
	rows, err := w.b.db.DB.Query("SELECT text FROM outbox ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var output strings.Builder
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		output.WriteString(text)
		output.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func TestNativeCommandLocalSubmissionAndUncorrelatedOutput(t *testing.T) {
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	nativeInput(t, w, 100, "/native_local@fixture_bot arg  two", true)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	waitFixtureRPCTrace(t, trace, "prompt", "/native_local arg  two")
	if w.taskActive() || strings.Contains(nativeOutputs(t, w), "PRIVATE_NATIVE_OUTPUT") {
		t.Fatal("local submission kept active ownership or delivered uncorrelated output")
	}
	var replies int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=100").Scan(&replies))
	if replies != 0 {
		t.Fatalf("local acknowledgment produced %d replies, want zero", replies)
	}
	nativeInput(t, w, 101, "wait", false)
	nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	output := nativeOutputs(t, w)
	if !strings.Contains(output, "root completed") || strings.Contains(output, "PRIVATE_LATE_NATIVE_OUTPUT") {
		t.Fatalf("late native output changed the next root's reply: %q", output)
	}
}

func TestNativeCommandDelayedLocalCompletionKeepsFollowingTextQueued(t *testing.T) {
	w, _, trace := nativeWorker(t, "delayed-local")
	nativeCatalogReady(t, w)
	nativeInput(t, w, 100, "/native_local", false)
	nativeUntil(t, w, func() bool { return !w.rpcOperationActive })
	nativeInput(t, w, 101, "after local command", false)
	if queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "pending" || len(w.steers) != 0 {
		t.Fatal("unconfirmed agent invocation converted following text into steer")
	}
	nativeRPC(t, w, "fixture_finish_local", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	var replies int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=100").Scan(&replies))
	if queueState(t, w, 100) != "done" || replies != 0 || !strings.Contains(nativeOutputs(t, w), "answer: after local command") {
		t.Fatalf("delayed local completion lost its terminal state or following root: replies=%d", replies)
	}
	waitFixtureRPCTrace(t, trace, "prompt", "after local command")
}

func TestNativeCommandDelayedLocalFailureSettlesAndContinuesQueue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		state  string
	}{
		{name: "error", status: "error", state: "uncertain"},
		{name: "aborted", status: "aborted", state: "cancelled"},
		{name: "missing", status: "", state: "uncertain"},
		{name: "unknown", status: "unknown", state: "uncertain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, trace := nativeWorker(t, "delayed-local")
			nativeCatalogReady(t, w)
			client := w.client
			nativeInput(t, w, 100, "/native_local", false)
			nativeUntil(t, w, func() bool { return !w.rpcOperationActive })
			nativeInput(t, w, 101, "after local command", false)
			if queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "pending" || len(w.steers) != 0 {
				t.Fatal("local completion did not retain its submission and queue ownership")
			}
			nativeRPC(t, w, "fixture_finish_local", map[string]any{"status": tc.status})
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			if queueState(t, w, 100) != tc.state || w.client != client || w.taskActive() {
				t.Fatalf("local result settled as %s, want %s on the same idle runtime", queueState(t, w, 100), tc.state)
			}
			var notice string
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox WHERE inbox_id=100").Scan(&notice))
			output := nativeOutputs(t, w)
			if strings.Contains(output, "PRIVATE_NATIVE_ERROR") || !strings.Contains(output, "answer: after local command") {
				t.Fatal("local completion leaked diagnostics or lost the following root reply")
			}
			var localSubmissions int
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "prompt" && entry.payload == "/native_local" {
					localSubmissions++
				}
			}
			if localSubmissions != 1 {
				t.Fatalf("local command was submitted %d times, want once without replay", localSubmissions)
			}
		})
	}
}

func TestNativeCommandNamespacedReplyCompletion(t *testing.T) {
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	nativeInput(t, w, 100, "/plugin:command", true)
	if w.activeInputKind != queuedNativeCommand {
		t.Fatal("namespaced command was submitted as ordinary text")
	}
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	waitFixtureRPCTrace(t, trace, "prompt", "/plugin:command")
	var replies int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=100").Scan(&replies))
	if replies != 0 || w.taskActive() {
		t.Fatal("namespaced local completion emitted a model reply or retained ownership")
	}
}

func TestNativeCommandNamespacedInputsStayQueuedWithFullName(t *testing.T) {
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	nativeInput(t, w, 100, "wait", false)
	nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
	inputs := []string{"/plugin:command", "/plugin:command arg  two"}
	for i, text := range inputs {
		id := int64(101 + i)
		nativeInput(t, w, id, text, false)
		if len(w.queue) != i+1 || len(w.steers) != 0 {
			t.Fatalf("namespaced input %q did not stay in the pending queue", text)
		}
		q := w.queue[i]
		if queueState(t, w, id) != "pending" || q.kind != queuedNativeCommand || q.nativeName != "plugin:command" || q.text != text || len(w.steers) != 0 {
			t.Fatalf("namespaced input %q lost its full name or became steer", text)
		}
	}
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
	var prompts []string
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && entry.payload != "/session info" {
			prompts = append(prompts, entry.payload)
		}
	}
	if strings.Join(prompts, "|") != "wait|/plugin:command|/plugin:command arg  two" {
		t.Fatalf("namespaced commands bypassed queue order or changed arguments: %q", prompts)
	}
}

func TestNativeCommandNamespacedRevalidationDoesNotUsePrefix(t *testing.T) {
	for _, removed := range []string{"foo", "foo:bar"} {
		t.Run(removed, func(t *testing.T) {
			w, _, trace := nativeWorker(t, "enabled")
			nativeCatalogReady(t, w)
			nativeInput(t, w, 100, "wait", false)
			nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
			nativeInput(t, w, 101, "/foo:bar", false)
			if len(w.queue) != 1 || w.queue[0].nativeName != "foo:bar" {
				t.Fatal("prefix won over the full advertised command name")
			}
			nativeRPC(t, w, "fixture_catalog_update", map[string]any{"removeName": removed})
			nativeRPC(t, w, "fixture_finish_root", nil)
			want := "done"
			if removed == "foo:bar" {
				want = "cancelled"
			}
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == want })
			if removed == "foo" {
				waitFixtureRPCTrace(t, trace, "prompt", "/foo:bar")
			} else {
				for _, entry := range fixtureRPCTrace(trace) {
					if entry.kind == "prompt" && entry.payload == "/foo:bar" {
						t.Fatal("removed namespaced command fell back to the still-advertised prefix")
					}
				}
			}
		})
	}
}

func TestNativeCommandNewFullNameCancelsQueuedPrefix(t *testing.T) {
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	nativeInput(t, w, 100, "wait", false)
	nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
	nativeInput(t, w, 101, "/session:pin", false)
	if len(w.queue) != 1 || w.queue[0].nativeName != "session" {
		t.Fatal("builtin colon input was not queued under its original name")
	}
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"addName": "session:pin"})
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "cancelled" })
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && entry.payload == "/session:pin" {
			t.Fatal("a new full name changed the queued command's execution target")
		}
	}
}

func TestNonBuiltinCommandNonSpaceSeparatorsKeepReplyContextAndSteer(t *testing.T) {
	for _, source := range []string{"file", "custom"} {
		for _, separator := range []string{":", "\t", "\u00a0", "\ufeff"} {
			t.Run(source+"/"+separator, func(t *testing.T) {
				w, _, trace := nativeWorker(t, "enabled")
				nativeCatalogReady(t, w)
				nativeRPC(t, w, "fixture_catalog_update", map[string]any{"sourceName": "native_agent", "source": source})
				text := "/native_agent" + separator + "arg"
				nativeInput(t, w, 100, text, true)
				nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
				if !strings.Contains(nativeOutputs(t, w), "quoted context") || !strings.Contains(nativeOutputs(t, w), text) {
					t.Fatal("non-builtin invocation lost its reply context or text")
				}
				nativeInput(t, w, 101, "wait", false)
				nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
				nativeInput(t, w, 102, text, false)
				nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" && queueState(t, w, 102) == "done" })
				if len(w.queue) != 0 || !strings.Contains(nativeOutputs(t, w), "answer: "+text) {
					t.Fatal("non-builtin invocation queued a separate root instead of steering")
				}
				waitFixtureRPCTrace(t, trace, "prompt", text)
			})
		}
	}
}

func TestBuiltinNativeSeparatorsAndBotSuffix(t *testing.T) {
	for _, separator := range []string{":", "\t", "\u00a0", "\ufeff"} {
		t.Run(separator, func(t *testing.T) {
			w, _, trace := nativeWorker(t, "enabled")
			nativeCatalogReady(t, w)
			text := "/session@FIXTURE_BOT" + separator + "info"
			nativeInput(t, w, 100, text, false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			waitFixtureRPCTrace(t, trace, "prompt", "/session"+separator+"info")
		})
	}
}

func TestColonCompactRetainsBridgeControls(t *testing.T) {
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	nativeInput(t, w, 100, "/compact@fixture_bot:foo", true)
	if queueState(t, w, 100) != "done" || len(w.confirms) != 0 || w.taskActive() || w.sessionOp != sessionOperationNone || len(w.queue) != 0 {
		t.Fatal("unsupported compact arguments bypassed bridge refusal")
	}
	nativeInput(t, w, 101, "/compact:", false)
	if queueState(t, w, 101) != "done" || len(w.confirms) != 1 || w.taskActive() || w.sessionOp != sessionOperationNone {
		t.Fatal("colon compact bypassed confirmation")
	}
	w.busy = true
	nativeInput(t, w, 102, "/compact:", false)
	if queueState(t, w, 102) != "done" || len(w.confirms) != 1 || w.sessionOp != sessionOperationNone || len(w.queue) != 0 {
		t.Fatal("colon compact bypassed the idle check")
	}
	w.busy = false
	nativeRPC(t, w, "get_state", nil)
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && entry.payload != "/session info" {
			t.Fatalf("compact fell back to an RPC prompt: %q", entry.payload)
		}
	}
}

func TestUnknownSeparatorsAndAbsolutePathRemainOrdinaryPrompts(t *testing.T) {
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	for i, text := range []string{"/unknown:arg", "/unknown\targ", "/unknown@fixture_bot: arg  two", "/opt/user@host/file"} {
		id := int64(100 + i)
		nativeInput(t, w, id, text, false)
		nativeUntil(t, w, func() bool { return queueState(t, w, id) == "done" })
		waitFixtureRPCTrace(t, trace, "prompt", text)
		if !strings.Contains(nativeOutputs(t, w), "answer: "+text) {
			t.Fatalf("ordinary input %q lost its final reply", text)
		}
	}
}

func TestNativeCommandQueuesBehindFollowupWithoutSteering(t *testing.T) {
	w, command, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	nativeInput(t, w, 100, "wait", false)
	nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
	command("/followup queued first")
	nativeInput(t, w, 101, "/native_local", false)
	if queueState(t, w, 101) != "pending" || len(w.steers) != 0 {
		t.Fatal("native command was admitted as steer")
	}
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	var prompts []string
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && entry.payload != "/session info" {
			prompts = append(prompts, entry.payload)
		}
	}
	if strings.Join(prompts, "|") != "wait|queued first|/native_local" {
		t.Fatalf("native command submission order = %q", prompts)
	}
}

func TestNativeCommandDispatchRevalidatesCatalogAndSession(t *testing.T) {
	for _, change := range []string{"remove", "newSession"} {
		t.Run(change, func(t *testing.T) {
			w, _, trace := nativeWorker(t, "enabled")
			nativeCatalogReady(t, w)
			nativeInput(t, w, 100, "wait", false)
			nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
			nativeInput(t, w, 101, "/native_local", false)
			nativeRPC(t, w, "fixture_catalog_update", map[string]any{change: true})
			nativeRPC(t, w, "fixture_finish_root", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "cancelled" })
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "prompt" && entry.payload == "/native_local" {
					t.Fatal("obsolete native command was submitted")
				}
			}
			if !strings.Contains(nativeOutputs(t, w), "no longer available") {
				t.Fatal("obsolete command cancellation was silent")
			}
		})
	}
}

func TestNativeCommandUnknownTextAndBridgePrecedence(t *testing.T) {
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	nativeInput(t, w, 100, "/notadvertised@fixture_bot  original", true)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	ordinary := nativeOutputs(t, w)
	if !strings.Contains(ordinary, replyContextStart) || !strings.Contains(ordinary, "/notadvertised@fixture_bot  original") {
		t.Fatalf("unknown addressed input lost original text/reply context: %q", ordinary)
	}
	nativeInput(t, w, 101, "/native_local@other_bot", false)
	if queueState(t, w, 101) != "ignored" {
		t.Fatal("foreign addressed command was submitted")
	}
	nativeInput(t, w, 102, "/status", false)
	drainControlOperations(t, w)
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && entry.payload == "/status" {
			t.Fatal("advertised native status overrode bridge status")
		}
	}
}

func TestNativeCommandPendingDiscoveryDoesNotBlockStopOrClose(t *testing.T) {
	for _, control := range []string{"/stop", "/close"} {
		t.Run(control, func(t *testing.T) {
			w, command, trace := nativeWorker(t, "hold")
			nativeInput(t, w, 100, "/native_local", false)
			if queueState(t, w, 100) != "pending" {
				t.Fatal("unknown catalog was treated as command absence")
			}
			command(control)
			if queueState(t, w, 100) != "cancelled" {
				t.Fatal("control command did not cancel waiting classification")
			}
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "prompt" && entry.payload == "/native_local" {
					t.Fatal("classification wait fell back to ordinary prompt")
				}
			}
		})
	}
}

func TestNativeCommandReceivedUpdateClassifiesBeforeQueryFinishes(t *testing.T) {
	w, _, trace := nativeWorker(t, "hold")
	nativeInput(t, w, 100, "/native_local@fixture_bot", true)
	nativeRPC(t, w, "fixture_catalog_update", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	waitFixtureRPCTrace(t, trace, "prompt", "/native_local")
}

func TestDeferredAutoresearchGoalKeepsOwnershipBeforeFollowup(t *testing.T) {
	w, _, trace := nativeWorker(t, "autoresearch-hold")
	waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
	nativeInput(t, w, 100, "/autoresearch goal", false)
	nativeInput(t, w, 101, "/followup ordinary", false)
	if queueState(t, w, 100) != "pending" || queueState(t, w, 101) != "pending" || w.taskActive() {
		t.Fatal("discovery wait submitted research or its following input")
	}
	nativeRPC(t, w, "fixture_catalog_update", nil)
	nativeUntil(t, w, func() bool { return w.research.roundOpen && !w.rpcOperationActive })
	researchBarrier(t, w)
	if w.active != 100 || !w.research.root || queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "pending" {
		t.Fatal("deferred research lost FIFO ownership to its followup")
	}
	var prompts []string
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && entry.payload != "/session info" {
			prompts = append(prompts, entry.payload)
		}
	}
	if !slices.Equal(prompts, []string{"/autoresearch goal"}) {
		t.Fatalf("discovery reordered research execution or submitted its followup: %q", prompts)
	}
}

func TestDeferredAutoresearchControlsDisableBeforePreservedFollowup(t *testing.T) {
	for _, control := range []string{"/autoresearch", "/autoresearch off"} {
		t.Run(control, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			researchRoot(t, w, 100, "/autoresearch original goal")
			oldClient := w.client
			nativeInput(t, w, 101, "/followup preserved work", false)
			w.client.InvalidateCommandCatalog()
			nativeInput(t, w, 102, control, false)
			if queueState(t, w, 102) != "pending" {
				t.Fatal("control did not wait for command classification")
			}
			nativeRPC(t, w, "fixture_catalog_update", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
			if queueState(t, w, 100) != "uncertain" || w.client == oldClient || w.research.root || w.research.enabled {
				t.Fatal("deferred off or enabled empty toggle did not immediately retire the old research root")
			}
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			nativeRPC(t, w, "get_state", nil)
			var prompts []string
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "prompt" && entry.payload != "/session info" {
					prompts = append(prompts, entry.payload)
				}
			}
			if !slices.Equal(prompts, []string{"/autoresearch original goal", "/autoresearch off", "preserved work"}) {
				t.Fatalf("deferred control lost priority or replayed/discarded queued work: %q", prompts)
			}
		})
	}
}

func TestDeferredAutoresearchControlPreservesNextGoalAcrossRuntimeReplacement(t *testing.T) {
	for _, control := range []string{"/autoresearch off", "/autoresearch"} {
		t.Run(control, func(t *testing.T) {
			w, _, trace := nativeWorker(t, "autoresearch-hold")
			waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
			nativeRPC(t, w, "fixture_catalog_update", nil)
			w.commandCatalogUpdated()
			researchRoot(t, w, 100, "/autoresearch initial goal")
			oldClient := w.client
			w.client.InvalidateCommandCatalog()
			nativeInput(t, w, 101, control, false)
			nativeInput(t, w, 102, "/autoresearch next goal", false)
			nativeInput(t, w, 103, "/followup preserved work", false)
			nativeInput(t, w, 104, "/autoresearch", false)
			nativeInput(t, w, 105, "/autoresearch@fixture_bot", false)
			if queueState(t, w, 101) != "pending" || queueState(t, w, 102) != "pending" {
				t.Fatal("control and next goal did not wait for the same discovery")
			}
			// Keep the old discovery held while allowing the replacement to discover normally.
			t.Setenv("OMP_TELEGRAM_FIXTURE_NATIVE_COMMANDS", "autoresearch")
			nativeRPC(t, w, "fixture_catalog_update", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			nativeUntil(t, w, func() bool { return w.research.roundOpen && !w.rpcOperationActive })
			researchBarrier(t, w)
			if w.client == oldClient || queueState(t, w, 100) != "uncertain" || queueState(t, w, 102) != "submitted" || w.active != 102 || queueState(t, w, 103) != "pending" {
				t.Fatal("runtime replacement lost the next research goal or its following work")
			}
			if queueState(t, w, 104) != "cancelled" || queueState(t, w, 105) != "cancelled" {
				t.Fatal("pending discovery toggles survived off and could disable or reenable the next goal")
			}
			var prompts []string
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "prompt" && entry.payload != "/session info" {
					prompts = append(prompts, entry.payload)
				}
			}
			if !slices.Equal(prompts, []string{"/autoresearch initial goal", "/autoresearch off", "/autoresearch next goal"}) {
				t.Fatalf("runtime replacement replayed research or changed queued execution order: %q", prompts)
			}
		})
	}
}

func TestDeferredAutoresearchClearExcludesOnlyItsOwnQueueSlot(t *testing.T) {
	for _, followup := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "queued-followup"}[followup], func(t *testing.T) {
			w, _, trace := nativeWorker(t, "autoresearch-hold")
			waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
			text := "/autoresearch clear --force --keep-tree"
			nativeInput(t, w, 100, text, false)
			if followup {
				nativeInput(t, w, 101, "/followup preserved work", false)
			}
			nativeRPC(t, w, "fixture_catalog_update", nil)
			w.resolveSlashInputs()
			if followup {
				if queueState(t, w, 100) != "cancelled" || len(w.confirms) != 0 || queueState(t, w, 101) != "pending" {
					t.Fatal("deferred clear ignored other queued work or discarded its followup")
				}
				nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			} else {
				nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
				if len(w.confirms) != 1 || w.taskActive() || len(w.queue) != 0 {
					t.Fatal("deferred clear counted its own queue slot as work or bypassed confirmation")
				}
			}
			nativeRPC(t, w, "get_state", nil)
			if researchTraceCount(trace, "prompt", text) != 0 || researchTraceCount(trace, "autoresearch_clear", "clear --force --keep-tree") != 0 {
				t.Fatal("deferred clear executed native destruction before an idle confirmation")
			}
		})
	}
}

func TestNativeCommandLazyRestoreKeepsNativeSemantics(t *testing.T) {
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	old := w.client.ID()
	w.releaseRuntimeWithReason(true, "idle")
	nativeInput(t, w, 100, "/native_local@fixture_bot", true)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	if w.client == nil || w.client.ID() == old {
		t.Fatal("scenario did not restore a released runtime")
	}
	waitFixtureRPCTrace(t, trace, "prompt", "/native_local")
}

func TestNativeCommandDiscoveryFailureIsNotUnsupported(t *testing.T) {
	for _, mode := range []string{"unsupported", "fail"} {
		t.Run(mode, func(t *testing.T) {
			w, _, trace := nativeWorker(t, mode)
			nativeInput(t, w, 100, "/native_local@fixture_bot", false)
			if mode == "unsupported" {
				nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "cancelled" })
				if w.client == nil || w.client.CommandCatalog().State != omp.CatalogUnsupported {
					t.Fatal("explicit unsupported discovery did not preserve runtime")
				}
				if researchTraceCount(trace, "prompt", "/native_local@fixture_bot") != 0 {
					t.Fatal("unsupported discovery forwarded unverifiable slash input")
				}
			} else {
				nativeUntil(t, w, func() bool { return w.nativeCatalog.cancel == nil })
				if w.client == nil || queueState(t, w, 100) != "pending" || w.client.CommandCatalog().State != omp.CatalogUnknown {
					t.Fatal("rejected auxiliary discovery retired the runtime or guessed command permission")
				}
				request := w.nativeCatalog.request
				w.dispatch()
				if w.nativeCatalog.request != request || w.nativeCatalog.cancel != nil {
					t.Fatal("rejected discovery automatically retried in the same scope")
				}
				for _, entry := range fixtureRPCTrace(trace) {
					if entry.kind == "prompt" && strings.Contains(entry.payload, "native_local") {
						t.Fatal("failed discovery was treated as an empty catalog")
					}
				}
				nativeRPC(t, w, "fixture_catalog_update", nil)
				nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
				waitFixtureRPCTrace(t, trace, "prompt", "/native_local")
			}
		})
	}
}

func TestNativeCommandAgentInvocationKeepsRootLifecycle(t *testing.T) {
	for _, mode := range []string{"enabled", "missing-agent-invoked"} {
		t.Run(mode, func(t *testing.T) {
			w, _, _ := nativeWorker(t, mode)
			nativeCatalogReady(t, w)
			command := "/native_agent"
			if mode == "missing-agent-invoked" {
				command = "/native_local"
			}
			nativeInput(t, w, 100, command, false)
			nativeUntil(t, w, func() bool { return !w.rpcOperationActive && w.activeInputKind == queuedPrompt })
			if queueState(t, w, 100) != "submitted" || !w.taskActive() {
				t.Fatal("agent invocation was treated as local completion")
			}
			nativeInput(t, w, 101, "steered after native agent start", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" && queueState(t, w, 101) == "done" })
			var steerReplies int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=101").Scan(&steerReplies))
			if steerReplies != 0 || !strings.Contains(nativeOutputs(t, w), "answer: steered after native agent start") {
				t.Fatal("agent-start evidence did not allow steering under the original final-reply owner")
			}
		})
	}
}

func TestNativeCommandOldSessionDiscoveryRejectionDoesNotCloseNewSession(t *testing.T) {
	w, _, trace := nativeWorker(t, "hold-reject")
	nativeInput(t, w, 100, "/native_local@fixture_bot", true)
	waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"newSession": true})
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" && w.nativeCatalog.cancel == nil })
	if w.client == nil || w.client.CommandCatalog().SessionID != "11111111-2222-4333-8444-555555555555" {
		t.Fatal("old query rejection closed the new session")
	}
	waitFixtureRPCTrace(t, trace, "prompt", "/native_local")
}

func TestNativeCommandOldSessionWaitErrorKeepsNewRoot(t *testing.T) {
	for _, kind := range []string{"timeout", "cancelled", "transport"} {
		t.Run(kind, func(t *testing.T) {
			w, _, trace := nativeWorker(t, "hold")
			client := w.client
			oldEpoch := client.CommandCatalog().Epoch
			waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
			nativeInput(t, w, 100, "/native_local", false)
			nativeRPC(t, w, "fixture_catalog_update", map[string]any{"newSession": true})
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			if client.CommandCatalog().Epoch == oldEpoch || client.CommandCatalog().State != omp.CatalogReady {
				t.Fatal("new session did not publish its own ready catalog")
			}
			nativeInput(t, w, 101, "wait", false)
			nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
			switch kind {
			case "cancelled":
				w.nativeCatalog.cancel()
			case "transport":
				ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
				defer cancel()
				if _, err := client.Call(ctx, "fixture_catalog_exit", nil); err == nil {
					t.Fatal("exiting RPC fixture did not fail the transport")
				}
			}
			var result commandCatalogResult
			select {
			case result = <-w.nativeCatalog.results:
			case <-time.After(17 * time.Second):
				t.Fatal("catalog wait did not return")
			}
			if result.epoch != oldEpoch || result.err == nil {
				t.Fatal("failed wait lost its old query scope")
			}
			if kind == "timeout" && !errors.Is(result.err, context.DeadlineExceeded) || kind == "cancelled" && !errors.Is(result.err, context.Canceled) {
				t.Fatalf("unexpected wait error: %v", result.err)
			}
			w.commandCatalogFinished(result)
			if kind == "transport" {
				if w.client != nil || queueState(t, w, 101) != "uncertain" {
					t.Fatal("old epoch hid a real transport failure")
				}
				return
			}
			if w.client != client || w.active != 101 || queueState(t, w, 101) != "submitted" {
				t.Fatal("old-session wait error interrupted the current root")
			}
			nativeRPC(t, w, "get_state", nil)
			nativeRPC(t, w, "fixture_finish_root", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			if !strings.Contains(nativeOutputs(t, w), "root completed") || strings.Contains(nativeOutputs(t, w), "Native command discovery failed") {
				t.Fatal("current root lost its final reply or received a stale failure notice")
			}
		})
	}
}

func TestNativeCommandCurrentSessionDiscoveryFailurePreservesRootAndQueue(t *testing.T) {
	for _, kind := range []string{"timeout", "cancelled", "rejected"} {
		t.Run(kind, func(t *testing.T) {
			mode := "hold"
			if kind == "rejected" {
				mode = "hold-reject"
			}
			w, _, trace := nativeWorker(t, mode)
			waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
			client, before := w.client, w.binding
			claimed, sessionID := w.claimedSession, w.sessionID
			nativeInput(t, w, 100, "wait", false)
			nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
			turn, requestID, owner, replyTo := w.turn, w.rootRequestID, w.owner, w.activeReplyTo
			nativeInput(t, w, 101, "/extension_alias private", false)
			nativeInput(t, w, 102, "/followup after discovery", false)
			switch kind {
			case "cancelled":
				w.nativeCatalog.cancel()
			case "rejected":
				nativeRPC(t, w, "fixture_catalog_update", map[string]any{"skipUpdate": true})
			}
			var result commandCatalogResult
			select {
			case result = <-w.nativeCatalog.results:
			case <-time.After(17 * time.Second):
				t.Fatal("current-session discovery did not return")
			}
			if result.epoch != client.CommandCatalog().Epoch || omp.ClassifyError(result.err) != kind {
				t.Fatalf("discovery returned the wrong scope or error: %v", result.err)
			}
			w.commandCatalogFinished(result)
			assertNativeSessionPreserved(t, w, before, client, claimed, sessionID)
			if w.active != 100 || !w.busy || w.turn != turn || w.rootRequestID != requestID || w.owner != owner || w.activeReplyTo != replyTo || queueState(t, w, 100) != "submitted" || len(w.steers) != 0 {
				t.Fatal("auxiliary discovery failure changed the ordinary root's ownership")
			}
			if len(w.queue) != 2 || w.queue[0].id != 101 || w.queue[1].id != 102 || queueState(t, w, 101) != "pending" || queueState(t, w, 102) != "pending" {
				t.Fatal("auxiliary discovery failure changed the original pending queue")
			}
			nativeRPC(t, w, "fixture_finish_root", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			if queueState(t, w, 101) != "pending" || queueState(t, w, 102) != "pending" || w.taskActive() || w.nativeCatalog.cancel != nil {
				t.Fatal("root completion submitted a catalog-dependent slash or retried discovery")
			}
			var finalReply string
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox WHERE inbox_id=100 AND text LIKE '%root completed%' LIMIT 1").Scan(&finalReply))
			request := w.nativeCatalog.request
			for range 3 {
				w.dispatch()
			}
			if w.nativeCatalog.request != request || w.nativeCatalog.cancel != nil {
				t.Fatal("paused discovery created an automatic refresh loop")
			}
			assertNativeSessionPreserved(t, w, before, client, claimed, sessionID)
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "extension_session_mutation" || entry.kind == "prompt" && entry.payload != "/session info" && entry.payload != "wait" {
					t.Fatal("discovery failure fell back to a raw command or submitted the blocked queue")
				}
			}
			nativeRPC(t, w, "fixture_catalog_update", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
			assertNativeSessionPreserved(t, w, before, client, claimed, sessionID)
			output := nativeOutputs(t, w)
			if queueState(t, w, 101) != "cancelled" || !strings.Contains(output, "answer: after discovery") || strings.Contains(output, "runtime has been closed") || strings.Contains(output, "outcome is uncertain") {
				t.Fatal("reader catalog recovery lost the queue, failed to enforce source permissions, or reported a false runtime failure")
			}
		})
	}
}

func TestNativeCommandCurrentSessionDiscoveryTransportFailureRetiresRoot(t *testing.T) {
	w, _, trace := nativeWorker(t, "hold")
	waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
	client := w.client
	nativeInput(t, w, 100, "wait", false)
	nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
	nativeInput(t, w, 101, "/native_local", false)
	ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
	defer cancel()
	if _, err := client.Call(ctx, "fixture_catalog_exit", nil); err == nil {
		t.Fatal("fixture exit did not fail the transport")
	}
	var result commandCatalogResult
	select {
	case result = <-w.nativeCatalog.results:
	case <-time.After(3 * time.Second):
		t.Fatal("transport failure did not return discovery waiter")
	}
	w.commandCatalogFinished(result)
	if w.client != nil || queueState(t, w, 100) != "uncertain" || queueState(t, w, 101) != "cancelled" {
		t.Fatal("real transport failure preserved unsafe runtime ownership")
	}
}

func TestNativeCommandReadyUpdateWinsCurrentEpochWaitError(t *testing.T) {
	for _, kind := range []string{"timeout", "cancelled", "rejected"} {
		t.Run(kind, func(t *testing.T) {
			mode := "hold"
			if kind == "rejected" {
				mode = "hold-reject"
			}
			w, _, trace := nativeWorker(t, mode)
			waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
			client, epoch := w.client, w.client.CommandCatalog().Epoch
			nativeInput(t, w, 100, "wait", false)
			nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
			nativeInput(t, w, 101, "/extension_alias private", false)
			nativeRPC(t, w, "fixture_catalog_update", nil)
			catalog := client.CommandCatalog()
			if catalog.State != omp.CatalogReady || catalog.Epoch != epoch {
				t.Fatal("reader did not publish a current-scope ready catalog")
			}
			if kind != "rejected" {
				w.nativeCatalog.cancel()
			}
			var result commandCatalogResult
			select {
			case result = <-w.nativeCatalog.results:
			case <-time.After(3 * time.Second):
				t.Fatal("old catalog waiter did not return")
			}
			if kind == "timeout" {
				// Preserve the real waiter's request and epoch while simulating its deadline edge.
				result.err = context.DeadlineExceeded
			}
			if result.epoch != epoch || omp.ClassifyError(result.err) != kind {
				t.Fatalf("wait error lost its current query scope: %v", result.err)
			}
			w.commandCatalogFinished(result)
			if w.client != client || w.active != 100 || queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "cancelled" || client.CommandCatalog().Revision != catalog.Revision || w.nativeCatalog.cancel != nil {
				t.Fatal("wait error overrode reader discovery, changed root ownership, or bypassed extension refusal")
			}
			nativeRPC(t, w, "fixture_finish_root", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			output := nativeOutputs(t, w)
			if !strings.Contains(output, "root completed") || strings.Contains(output, "Native command discovery is unavailable") || strings.Contains(output, "outcome is uncertain") {
				t.Fatal("obsolete waiter failure changed the final reply or produced a stale failure notice")
			}
		})
	}
}

func TestNativeCommandPausedDiscoveryAllowsNewScope(t *testing.T) {
	w, _, trace := nativeWorker(t, "hold")
	waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
	client := w.client
	nativeInput(t, w, 100, "/native_local", false)
	w.nativeCatalog.cancel()
	var result commandCatalogResult
	select {
	case result = <-w.nativeCatalog.results:
	case <-time.After(3 * time.Second):
		t.Fatal("catalog waiter did not cancel")
	}
	w.commandCatalogFinished(result)
	if w.client != client || queueState(t, w, 100) != "pending" || w.nativeCatalog.cancel != nil {
		t.Fatal("discovery cancellation did not safely retain the pending command")
	}
	request := w.nativeCatalog.request
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"newSession": true, "skipUpdate": true, "releaseQuery": true})
	nativeUntil(t, w, func() bool { return w.nativeCatalog.request > request && w.nativeCatalog.cancel != nil })
	// Wait for the new query itself, not just its actor-side waiter, before publishing recovery.
	waitFor(t, func() bool {
		queries := 0
		for _, entry := range fixtureRPCTrace(trace) {
			if entry.kind == "catalog_query" {
				queries++
			}
		}
		return queries == 2
	})
	if queueState(t, w, 100) != "pending" || w.taskActive() {
		t.Fatal("scope invalidation submitted a command without new discovery")
	}
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"releaseQuery": true})
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" && w.nativeCatalog.cancel == nil })
	if w.client != client || client.CommandCatalog().State != omp.CatalogReady || client.CommandCatalog().SessionID != "11111111-2222-4333-8444-555555555555" {
		t.Fatal("new-scope discovery did not restore native commands on the original runtime")
	}
	waitFixtureRPCTrace(t, trace, "prompt", "/native_local")
}

func TestNativeCommandPausedDiscoveryKeepsQueuedNativeUnsubmitted(t *testing.T) {
	w, _, trace := nativeWorker(t, "hold")
	waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
	client := w.client
	cancelWait := func() {
		w.nativeCatalog.cancel()
		select {
		case result := <-w.nativeCatalog.results:
			w.commandCatalogFinished(result)
		case <-time.After(3 * time.Second):
			t.Fatal("catalog waiter did not cancel")
		}
	}
	nativeRPC(t, w, "fixture_catalog_update", nil)
	cancelWait()
	nativeInput(t, w, 100, "wait", false)
	nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
	nativeInput(t, w, 101, "/native_local", false)
	if len(w.queue) != 1 || w.queue[0].kind != queuedNativeCommand {
		t.Fatal("scenario did not queue an advertised native command")
	}
	client.InvalidateCommandCatalog()
	w.refreshCommandCatalog(client)
	cancelWait()
	if w.client != client || w.active != 100 || queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "pending" || w.nativeCatalog.cancel != nil {
		t.Fatal("current-scope waiter cancellation retired the root or lost native input")
	}
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
	if queueState(t, w, 101) != "pending" || w.taskActive() || w.nativeCatalog.cancel != nil {
		t.Fatal("root completion replayed discovery or submitted an unvalidated native command")
	}
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"removeName": "native_local", "releaseQuery": true})
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "cancelled" })
	nativeRPC(t, w, "get_state", nil)
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && entry.payload == "/native_local" {
			t.Fatal("queued native input bypassed unavailable or revoked catalog permissions")
		}
	}
	if w.client != client || !strings.Contains(nativeOutputs(t, w), "root completed") {
		t.Fatal("native discovery pause lost the original runtime or ordinary final reply")
	}
}

func TestNativeCommandRejectsSessionRelocation(t *testing.T) {
	for _, mode := range []string{"enabled", "hold", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			w, _, trace := nativeWorker(t, mode)
			if mode != "hold" {
				nativeUntil(t, w, func() bool { return w.nativeCatalog.cancel == nil })
			}
			before, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
			requireStoreOK(t, err)
			clientID := w.client.ID()
			inputs := []string{
				"/move /new/path", "/wt@fixture_bot branch", "/worktree:branch",
				"/session delete", "/session@fixture_bot delete", "/session:delete",
				"/session@FIXTURE_BOT: DELETE ", "/session\u3000delete", "/session:\uFEFFdelete\uFEFF",
			}
			for i, text := range inputs {
				id := int64(100 + i)
				nativeInput(t, w, id, text, true)
				if queueState(t, w, id) != "cancelled" {
					t.Fatalf("relocation input %q was not rejected", text)
				}
			}
			if !w.sessionDurable() {
				t.Fatal("rejected lifecycle input deleted the native session file")
			}
			// A pipe barrier ensures the trace includes every earlier RPC submission.
			nativeRPC(t, w, "get_state", nil)
			after, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
			requireStoreOK(t, err)
			if !sameBindingIdentity(before, after) || !sameBindingIdentity(before, w.binding) || w.client.ID() != clientID || w.taskActive() || len(w.queue) != 0 {
				t.Fatal("rejected relocation changed binding or runtime ownership")
			}
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "prompt" && entry.payload != "/session info" {
					t.Fatalf("relocation reached RPC prompt: %q", entry.payload)
				}
			}
			if !strings.Contains(nativeOutputs(t, w), "not supported") {
				t.Fatal("relocation refusal did not notify the user")
			}
		})
	}
}

func TestNativeSessionNonDestructiveSubcommandsRemainAvailable(t *testing.T) {
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	before := w.binding
	for i, invocation := range []struct{ text, expected string }{
		{"/session@fixture_bot:info", "/session:info"},
		{"/session pin", "/session pin"},
	} {
		id := int64(100 + i)
		nativeInput(t, w, id, invocation.text, true)
		nativeUntil(t, w, func() bool { return queueState(t, w, id) == "done" })
		waitFixtureRPCTrace(t, trace, "prompt", invocation.expected)
		var replies int
		requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=?", id).Scan(&replies))
		if replies != 0 {
			t.Fatalf("local session command emitted %d final replies", replies)
		}
	}
	if !sameBindingIdentity(before, w.binding) || !w.sessionDurable() || w.taskActive() {
		t.Fatal("non-destructive session commands changed session ownership")
	}
}

func TestNativeCommandFollowupCannotBypassLifecycleRefusal(t *testing.T) {
	for _, text := range []string{"/move:/new/path", "/session@fixture_bot:delete"} {
		t.Run(text, func(t *testing.T) {
			w, command, trace := nativeWorker(t, "enabled")
			nativeCatalogReady(t, w)
			nativeInput(t, w, 100, "wait", false)
			nativeUntil(t, w, func() bool { return !w.rpcOperationActive })
			command("/followup:" + text)
			id := w.queue[0].id
			nativeInput(t, w, 101, "/native_local", false)
			nativeRPC(t, w, "fixture_finish_root", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, id) == "cancelled" })
			w.dispatch()
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "prompt" && entry.payload != "/session info" && entry.payload != "wait" && entry.payload != "/native_local" {
					t.Fatalf("queued lifecycle input reached RPC prompt: %q", entry.payload)
				}
			}
		})
	}
}

type nativeUnknownSourceCase struct {
	name    string
	source  any
	missing bool
}

func nativeUnknownSources() []nativeUnknownSourceCase {
	return []nativeUnknownSourceCase{
		{name: "missing", missing: true},
		{name: "null"},
		{name: "boolean", source: true},
		{name: "number", source: 42},
		{name: "object", source: map[string]any{"private_source_metadata": "secret"}},
		{name: "array", source: []any{"private_source_metadata"}},
		{name: "future", source: "future_private_source"},
		{name: "empty", source: ""},
	}
}

func (tc nativeUnknownSourceCase) fields(name string, skipUpdate bool) map[string]any {
	fields := map[string]any{"sourceName": name, "skipUpdate": skipUpdate}
	if !tc.missing {
		fields["source"] = tc.source
	}
	return fields
}

func assertNativeSessionPreserved(t *testing.T, w *worker, before store.Binding, client *omp.Client, claimed, sessionID string) {
	t.Helper()
	// A pipe barrier includes every earlier RPC submission in the trace.
	nativeRPC(t, w, "get_state", nil)
	after, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	requireStoreOK(t, err)
	if w.client != client || !sameBindingIdentity(before, after) || !sameBindingIdentity(before, w.binding) || w.claimedSession != claimed || w.sessionID != sessionID || !w.b.sessionInUse(sessionID) || !w.sessionDurable() {
		t.Fatal("source refusal changed saved/native binding, session claim, or runtime ownership")
	}
}

func assertUnknownSourceRefusal(t *testing.T, w *worker, trace string, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if queueState(t, w, id) != "cancelled" {
			t.Fatal("unknown source command or alias was not cancelled")
		}
	}
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "extension_session_mutation" || entry.kind == "prompt" && entry.payload != "/session info" && entry.payload != "wait" && entry.payload != "/native_local" {
			t.Fatalf("unknown source refusal reached RPC prompt/steer: %s %q", entry.kind, entry.payload)
		}
	}
	output := nativeOutputs(t, w)
	if !strings.Contains(output, "unknown or unsupported source") || strings.Contains(output, "Extension commands are not supported") {
		t.Fatal("unknown source refusal did not provide a distinct generic notice")
	}
	for _, private := range []string{"extension_switch", "extension_alias", "extension:session", "private arguments", "secret", "private_source_metadata", "future_private_source"} {
		if strings.Contains(output, private) {
			t.Fatal("source refusal echoed private command, arguments, or source metadata")
		}
	}
}

func nativeQueuedRawInput(t *testing.T, w *worker, id int64, text, kind string) {
	t.Helper()
	if kind == "followup" {
		nativeInput(t, w, id, "/followup "+text, false)
		return
	}
	// Exercise the prepared prompt/attachment queue without network/media setup.
	u := update(id, 11, "")
	raw, err := json.Marshal(u)
	requireStoreOK(t, err)
	requireStoreOK(t, w.b.db.Accept(id, raw))
	w.enqueuePreparedPrompt(incoming{id: id, msg: u.Message}, text, text)
	if kind == "attachment" {
		w.queue[len(w.queue)-1].images = []media.Image{{MimeType: "image/png", Data: "AA=="}}
	}
}

func TestUnknownSourceCommandRefusalPreservesBindingAndClaims(t *testing.T) {
	for _, tc := range nativeUnknownSources() {
		for _, mode := range []string{"enabled", "hold"} {
			for _, busy := range []bool{false, true} {
				t.Run(tc.name+"/"+mode+"/busy="+map[bool]string{false: "false", true: "true"}[busy], func(t *testing.T) {
					w, _, trace := nativeWorker(t, mode)
					if mode == "enabled" {
						nativeCatalogReady(t, w)
					}
					before := w.binding
					client, claimed, sessionID := w.client, w.claimedSession, w.sessionID
					nativeRPC(t, w, "fixture_catalog_update", tc.fields("extension_switch", mode == "hold"))
					if busy {
						nativeInput(t, w, 99, "wait", false)
						nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
					}
					inputs := []string{"/extension_switch private arguments", "/extension_alias@FIXTURE_BOT secret", "/extension:session private arguments"}
					for i, text := range inputs {
						nativeInput(t, w, int64(100+i), text, true)
					}
					if mode == "hold" {
						for i := range inputs {
							if queueState(t, w, int64(100+i)) != "pending" {
								t.Fatal("unknown catalog did not retain inputs for discovery")
							}
						}
						nativeRPC(t, w, "fixture_catalog_update", nil)
						nativeUntil(t, w, func() bool { return len(w.queue) == 0 })
					}
					assertNativeSessionPreserved(t, w, before, client, claimed, sessionID)
					assertUnknownSourceRefusal(t, w, trace, 100, 101, 102)
					if len(w.queue) != 0 || len(w.steers) != 0 || busy && (w.active != 99 || !w.taskActive()) || !busy && w.taskActive() {
						t.Fatal("unknown source input retained work or interrupted/steered the busy root")
					}
					if busy {
						nativeRPC(t, w, "fixture_finish_root", nil)
						nativeUntil(t, w, func() bool { return queueState(t, w, 99) == "done" })
					}
					nativeInput(t, w, 103, "/native_local", false)
					nativeUntil(t, w, func() bool { return queueState(t, w, 103) == "done" })
					waitFixtureRPCTrace(t, trace, "prompt", "/native_local")
					assertNativeSessionPreserved(t, w, before, client, claimed, sessionID)
					assertUnknownSourceRefusal(t, w, trace, 100, 101, 102)
				})
			}
		}
	}
}

func TestUnknownSourceBuiltinSeparatorsCannotBypassRefusal(t *testing.T) {
	for _, tc := range []nativeUnknownSourceCase{{name: "null"}, {name: "future", source: "future_private_source"}} {
		for _, mode := range []string{"enabled", "hold"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				w, _, trace := nativeWorker(t, mode)
				if mode == "enabled" {
					nativeCatalogReady(t, w)
				}
				before := w.binding
				client, claimed, sessionID := w.client, w.claimedSession, w.sessionID
				nativeRPC(t, w, "fixture_catalog_update", tc.fields("session", mode == "hold"))
				for i, text := range []string{"/session:info", "/session\tinfo"} {
					nativeInput(t, w, int64(100+i), text, true)
				}
				if mode == "hold" {
					if queueState(t, w, 100) != "pending" || queueState(t, w, 101) != "pending" {
						t.Fatal("unknown catalog did not retain builtin separator inputs for discovery")
					}
					nativeRPC(t, w, "fixture_catalog_update", nil)
					nativeUntil(t, w, func() bool { return len(w.queue) == 0 })
				}
				assertNativeSessionPreserved(t, w, before, client, claimed, sessionID)
				assertUnknownSourceRefusal(t, w, trace, 100, 101)
				if w.taskActive() || len(w.queue) != 0 || len(w.steers) != 0 {
					t.Fatal("refused builtin separator inputs retained task ownership")
				}
			})
		}
	}
}

func TestQueuedBuiltinSeparatorsCannotBypassSourceRevocation(t *testing.T) {
	for _, tc := range []nativeUnknownSourceCase{{name: "null"}, {name: "future", source: "future_private_source"}} {
		for _, kind := range []string{"native", "followup"} {
			for _, separator := range []string{":", "\t"} {
				t.Run(tc.name+"/"+kind+"/"+separator, func(t *testing.T) {
					w, _, trace := nativeWorker(t, "enabled")
					nativeCatalogReady(t, w)
					before := w.binding
					client, claimed, sessionID := w.client, w.claimedSession, w.sessionID
					nativeInput(t, w, 100, "wait", false)
					nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
					text := "/session" + separator + "info"
					if kind == "native" {
						nativeInput(t, w, 101, text, false)
					} else {
						nativeQueuedRawInput(t, w, 101, text, kind)
					}
					if len(w.queue) != 1 || queueState(t, w, 101) != "pending" || len(w.steers) != 0 {
						t.Fatal("separator invocation did not stay queued behind the active root")
					}
					nativeInput(t, w, 102, "/native_local", false)
					nativeRPC(t, w, "fixture_catalog_update", tc.fields("session", false))
					nativeRPC(t, w, "fixture_finish_root", nil)
					nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "cancelled" })
					w.dispatch()
					nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
					waitFixtureRPCTrace(t, trace, "prompt", "/native_local")
					assertNativeSessionPreserved(t, w, before, client, claimed, sessionID)
					assertUnknownSourceRefusal(t, w, trace, 101)
					if w.taskActive() || len(w.queue) != 0 || len(w.steers) != 0 {
						t.Fatal("source revocation retained task ownership or lost following work")
					}
				})
			}
		}
	}
}

func TestUnknownSourceQueuedPromptWaitsForCatalogAndCannotExecute(t *testing.T) {
	for _, mode := range []string{"enabled", "hold"} {
		for _, kind := range []string{"prompt", "followup", "attachment"} {
			for _, tc := range []nativeUnknownSourceCase{{name: "null"}, {name: "future", source: "future_private_source"}} {
				t.Run(mode+"/"+kind+"/"+tc.name, func(t *testing.T) {
					w, _, trace := nativeWorker(t, mode)
					if mode == "enabled" {
						nativeCatalogReady(t, w)
					}
					before := w.binding
					client, claimed, sessionID := w.client, w.claimedSession, w.sessionID
					nativeRPC(t, w, "fixture_catalog_update", tc.fields("extension_switch", mode == "hold"))
					nativeInput(t, w, 100, "wait", false)
					nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
					nativeQueuedRawInput(t, w, 101, "/extension_alias private arguments", kind)
					if len(w.queue) != 1 || w.queue[0].kind != queuedPrompt {
						t.Fatal("scenario did not exercise the queued raw prompt path")
					}
					nativeInput(t, w, 102, "/native_local", false)
					nativeRPC(t, w, "fixture_finish_root", nil)
					if mode == "hold" {
						nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
						if queueState(t, w, 101) != "pending" || w.taskActive() || w.rpcOperationActive || w.client.CommandCatalog().State != omp.CatalogUnknown {
							t.Fatal("raw slash prompt executed before catalog discovery")
						}
						nativeRPC(t, w, "fixture_catalog_update", nil)
					}
					nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "cancelled" })
					w.dispatch()
					nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
					if w.taskActive() || len(w.queue) != 0 || len(w.steers) != 0 {
						t.Fatal("raw unknown source prompt retained task ownership or lost following work")
					}
					waitFixtureRPCTrace(t, trace, "prompt", "/native_local")
					assertNativeSessionPreserved(t, w, before, client, claimed, sessionID)
					assertUnknownSourceRefusal(t, w, trace, 101)
				})
			}
		}
	}
}

func TestNativeCommandSourceBecomesUnknownBeforeDispatch(t *testing.T) {
	for _, source := range []string{"builtin", "file", "custom"} {
		for _, alias := range []bool{false, true} {
			for _, kind := range []string{"native", "followup"} {
				name := "extension_switch"
				target := "name"
				tc := nativeUnknownSourceCase{name: "future", source: "future_private_source"}
				if alias {
					name = "extension_alias"
					target = "alias"
					tc = nativeUnknownSourceCase{name: "missing", missing: true}
				}
				t.Run(source+"/"+target+"/"+kind, func(t *testing.T) {
					w, _, trace := nativeWorker(t, "enabled")
					nativeCatalogReady(t, w)
					before := w.binding
					client, claimed, sessionID := w.client, w.claimedSession, w.sessionID
					nativeRPC(t, w, "fixture_catalog_update", map[string]any{"sourceName": "extension_switch", "source": source})
					nativeInput(t, w, 100, "wait", false)
					nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
					text := "/" + name + " private arguments"
					if kind == "native" {
						nativeInput(t, w, 101, text, false)
						if len(w.queue) != 1 || w.queue[0].kind != queuedNativeCommand || w.queue[0].nativeName != name {
							t.Fatal("safe command or alias was not classified as native before source change")
						}
					} else {
						nativeQueuedRawInput(t, w, 101, text, kind)
					}
					nativeInput(t, w, 102, "/native_local", false)
					nativeRPC(t, w, "fixture_catalog_update", tc.fields("extension_switch", false))
					nativeRPC(t, w, "fixture_finish_root", nil)
					nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "cancelled" })
					w.dispatch()
					nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
					waitFixtureRPCTrace(t, trace, "prompt", "/native_local")
					assertNativeSessionPreserved(t, w, before, client, claimed, sessionID)
					assertUnknownSourceRefusal(t, w, trace, 101)
					if w.taskActive() || len(w.queue) != 0 || len(w.steers) != 0 {
						t.Fatal("source change retained task ownership or blocked following supported work")
					}
				})
			}
		}
	}
}

func TestExecutableFullNameWinsOverUnknownSourcePrefix(t *testing.T) {
	for _, source := range []string{"file", "custom"} {
		t.Run(source, func(t *testing.T) {
			w, _, trace := nativeWorker(t, "enabled")
			nativeCatalogReady(t, w)
			nativeRPC(t, w, "fixture_catalog_update", nativeUnknownSourceCase{}.fields("session", false))
			nativeRPC(t, w, "fixture_catalog_update", map[string]any{"addName": "session:pin", "sourceName": "session:pin", "source": source})
			nativeInput(t, w, 100, "/session:pin", true)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			waitFixtureRPCTrace(t, trace, "prompt", "/session:pin")
			var replies int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=100").Scan(&replies))
			if replies != 0 || w.taskActive() || strings.Contains(nativeOutputs(t, w), "unknown or unsupported source") {
				t.Fatal("unknown prefix source overrode the executable full name")
			}
		})
	}
}

func TestNativeCommandKnownSourcesRemainExecutable(t *testing.T) {
	for _, source := range []string{"builtin", "skill", "custom", "mcp_prompt", "file"} {
		t.Run(source, func(t *testing.T) {
			w, _, trace := nativeWorker(t, "enabled")
			nativeCatalogReady(t, w)
			before := w.binding
			client, claimed, sessionID := w.client, w.claimedSession, w.sessionID
			nativeRPC(t, w, "fixture_catalog_update", map[string]any{"sourceName": "plugin:command", "source": source})
			nativeInput(t, w, 100, "/plugin:command arg  two", true)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			waitFixtureRPCTrace(t, trace, "prompt", "/plugin:command arg  two")
			var replies int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=100").Scan(&replies))
			if replies != 0 {
				t.Fatal("supported local command produced a model fallback reply")
			}
			assertNativeSessionPreserved(t, w, before, client, claimed, sessionID)
			if w.taskActive() || len(w.queue) != 0 || len(w.steers) != 0 {
				t.Fatal("known source command retained task ownership")
			}
		})
	}
}

func TestUnknownSourcePreservesForeignAndBridgeControlPrecedence(t *testing.T) {
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	before := w.binding
	client, claimed, sessionID := w.client, w.claimedSession, w.sessionID
	nativeRPC(t, w, "fixture_catalog_update", nativeUnknownSourceCase{missing: true}.fields("extension_switch", false))
	nativeRPC(t, w, "fixture_catalog_update", nativeUnknownSourceCase{}.fields("status", false))
	nativeInput(t, w, 100, "/extension_alias@other_bot private arguments", false)
	if queueState(t, w, 100) != "ignored" {
		t.Fatal("unknown source refusal overrode foreign-bot ignoring")
	}
	nativeInput(t, w, 101, "/status", false)
	drainControlOperations(t, w)
	if queueState(t, w, 101) != "done" || strings.Contains(nativeOutputs(t, w), "unknown or unsupported source") {
		t.Fatal("unknown advertised source overrode bridge control routing")
	}
	assertNativeSessionPreserved(t, w, before, client, claimed, sessionID)
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "extension_session_mutation" || entry.kind == "prompt" && entry.payload != "/session info" {
			t.Fatal("foreign or bridge-control input reached native prompt execution")
		}
	}
}

func TestExtensionCommandRefusalPreservesBindingAndClaims(t *testing.T) {
	for _, mode := range []string{"enabled", "hold"} {
		t.Run(mode, func(t *testing.T) {
			w, _, trace := nativeWorker(t, mode)
			if mode == "enabled" {
				nativeCatalogReady(t, w)
			}
			before := w.binding
			client, claimed, sessionID := w.client, w.claimedSession, w.sessionID
			inputs := []string{"/extension_switch private arguments", "/extension_alias@FIXTURE_BOT secret", "/extension:session arg  two"}
			for i, text := range inputs {
				nativeInput(t, w, int64(100+i), text, true)
			}
			if mode == "hold" {
				for i := range inputs {
					if queueState(t, w, int64(100+i)) != "pending" {
						t.Fatal("unknown catalog did not retain extension inputs for discovery")
					}
				}
				nativeRPC(t, w, "fixture_catalog_update", nil)
				nativeUntil(t, w, func() bool { return len(w.queue) == 0 })
			}
			for i := range inputs {
				if queueState(t, w, int64(100+i)) != "cancelled" {
					t.Fatal("extension command or alias was not cancelled")
				}
			}
			nativeRPC(t, w, "get_state", nil)
			after, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
			requireStoreOK(t, err)
			if w.client != client || !sameBindingIdentity(before, after) || !sameBindingIdentity(before, w.binding) || w.claimedSession != claimed || w.sessionID != sessionID || !w.b.sessionInUse(sessionID) || !w.sessionDurable() || w.taskActive() || len(w.steers) != 0 {
				t.Fatal("extension refusal changed binding, session claim, or runtime ownership")
			}
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "extension_session_mutation" || entry.kind == "prompt" && entry.payload != "/session info" {
					t.Fatalf("extension refusal reached RPC: %s %q", entry.kind, entry.payload)
				}
			}
			output := nativeOutputs(t, w)
			if !strings.Contains(output, "Extension commands are not supported") || strings.Contains(output, "extension_switch") || strings.Contains(output, "extension_alias") || strings.Contains(output, "extension:session") || strings.Contains(output, "private arguments") || strings.Contains(output, "secret") {
				t.Fatal("extension refusal was missing or echoed private catalog/input data")
			}
		})
	}
}

func TestExtensionCommandBusyRootNeverSteers(t *testing.T) {
	for _, mode := range []string{"enabled", "hold"} {
		t.Run(mode, func(t *testing.T) {
			w, _, trace := nativeWorker(t, mode)
			if mode == "enabled" {
				nativeCatalogReady(t, w)
			}
			nativeInput(t, w, 100, "wait", false)
			nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
			nativeInput(t, w, 101, "/extension_alias", false)
			if mode == "hold" {
				nativeRPC(t, w, "fixture_catalog_update", nil)
				nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "cancelled" })
			}
			if queueState(t, w, 101) != "cancelled" || w.active != 100 || !w.taskActive() || len(w.steers) != 0 || len(w.queue) != 0 {
				t.Fatal("extension input steered or interrupted the busy root")
			}
			nativeRPC(t, w, "fixture_finish_root", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "extension_session_mutation" || entry.kind == "prompt" && entry.payload != "/session info" && entry.payload != "wait" {
					t.Fatal("busy extension refusal submitted a raw prompt or steer")
				}
			}
		})
	}
}

func TestExtensionCommandQueuedPromptCannotBypassRefusal(t *testing.T) {
	for _, kind := range []string{"followup", "attachment"} {
		t.Run(kind, func(t *testing.T) {
			w, _, trace := nativeWorker(t, "enabled")
			nativeCatalogReady(t, w)
			nativeInput(t, w, 100, "wait", false)
			nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
			text := "/extension:session private"
			if kind == "followup" {
				nativeInput(t, w, 101, "/followup "+text, false)
			} else {
				// Exercise the prepared attachment queue representation without network/media setup.
				u := update(101, 11, "")
				raw, err := json.Marshal(u)
				requireStoreOK(t, err)
				requireStoreOK(t, w.b.db.Accept(101, raw))
				w.enqueuePreparedPrompt(incoming{id: 101, msg: u.Message}, text, text)
				w.queue[0].images = []media.Image{{MimeType: "image/png", Data: "AA=="}}
			}
			if len(w.queue) != 1 || w.queue[0].kind != queuedPrompt {
				t.Fatal("scenario did not exercise the raw prompt dispatch path")
			}
			nativeRPC(t, w, "fixture_finish_root", nil)
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "cancelled" })
			if w.taskActive() || len(w.queue) != 0 || len(w.steers) != 0 {
				t.Fatal("raw extension prompt retained task ownership")
			}
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "extension_session_mutation" || entry.kind == "prompt" && entry.payload == text {
					t.Fatal("raw queued extension invocation bypassed refusal")
				}
			}
		})
	}
}

func TestExtensionCommandChangedSourceIsRejectedAtDispatch(t *testing.T) {
	for _, source := range []string{"file", "custom"} {
		for _, followup := range []bool{false, true} {
			name := source + "/native"
			if followup {
				name = source + "/followup"
			}
			t.Run(name, func(t *testing.T) {
				w, _, trace := nativeWorker(t, "enabled")
				nativeCatalogReady(t, w)
				nativeRPC(t, w, "fixture_catalog_update", map[string]any{"sourceName": "foo:bar", "source": source})
				nativeInput(t, w, 100, "wait", false)
				nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
				text := "/foo:bar private"
				if followup {
					nativeInput(t, w, 101, "/followup "+text, false)
				} else {
					nativeInput(t, w, 101, text, false)
				}
				nativeRPC(t, w, "fixture_catalog_update", map[string]any{"sourceName": "foo:bar", "source": "extension"})
				nativeRPC(t, w, "fixture_finish_root", nil)
				nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "cancelled" })
				if !strings.Contains(nativeOutputs(t, w), "Extension commands are not supported") {
					t.Fatal("source change did not use extension refusal")
				}
				for _, entry := range fixtureRPCTrace(trace) {
					if entry.kind == "extension_session_mutation" || entry.kind == "prompt" && entry.payload == text {
						t.Fatal("changed extension source reached RPC")
					}
				}
			})
		}
	}
}

func TestExtensionCommandNonSpaceSyntaxAndFullNameRefusal(t *testing.T) {
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	for i, separator := range []string{":", "\t", "\u00a0", "\ufeff"} {
		id := int64(100 + i)
		nativeInput(t, w, id, "/extension_switch"+separator+"arg", false)
		if queueState(t, w, id) != "cancelled" {
			t.Fatal("unsupported extension separator syntax fell back to a prompt")
		}
	}
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"addName": "extension", "sourceName": "extension", "source": "builtin"})
	nativeInput(t, w, 104, "/extension:session arg", false)
	if queueState(t, w, 104) != "cancelled" {
		t.Fatal("builtin prefix won over the advertised full extension name")
	}
	nativeInput(t, w, 105, "/extension_alias@other_bot", false)
	if queueState(t, w, 105) != "ignored" {
		t.Fatal("extension refusal overrode foreign-bot ignoring")
	}
	nativeRPC(t, w, "get_state", nil)
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "extension_session_mutation" || entry.kind == "prompt" && entry.payload != "/session info" {
			t.Fatalf("extension syntax refusal reached RPC: %s %q", entry.kind, entry.payload)
		}
	}
	if !strings.Contains(nativeOutputs(t, w), "Extension commands are not supported") || w.taskActive() || len(w.queue) != 0 || len(w.steers) != 0 {
		t.Fatal("extension syntax refusal lost its notice or retained task ownership")
	}
}

func TestNativeCommandOldWaitErrorKeepsUnknownNewEpoch(t *testing.T) {
	for _, kind := range []string{"timeout", "cancelled", "transport"} {
		for _, followup := range []bool{false, true} {
			name := kind + "/slash"
			if followup {
				name = kind + "/followup"
			}
			t.Run(name, func(t *testing.T) {
				w, _, trace := nativeWorker(t, "hold")
				client := w.client
				oldEpoch := client.CommandCatalog().Epoch
				waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
				nativeInput(t, w, 100, "wait", false)
				nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
				nativeRPC(t, w, "fixture_catalog_update", map[string]any{"newSession": true, "skipUpdate": true})
				catalog := client.CommandCatalog()
				if catalog.Epoch == oldEpoch || catalog.State != omp.CatalogUnknown {
					t.Fatal("new session did not invalidate the catalog to Unknown")
				}
				text := "/extension_switch private"
				if followup {
					text = "/followup " + text
				}
				nativeInput(t, w, 101, text, false)
				if kind == "transport" {
					ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
					defer cancel()
					if _, err := client.Call(ctx, "fixture_catalog_exit", nil); err == nil {
						t.Fatal("fixture exit did not fail the transport")
					}
				} else {
					w.nativeCatalog.cancel()
				}
				var result commandCatalogResult
				select {
				case result = <-w.nativeCatalog.results:
				case <-time.After(3 * time.Second):
					t.Fatal("scoped discovery wait did not return")
				}
				if result.epoch != oldEpoch || result.err == nil {
					t.Fatal("old discovery waiter lost its query scope")
				}
				if kind == "timeout" {
					// Inject only the returned context error; the actual waiter and query epoch are retained.
					result.err = context.DeadlineExceeded
				} else if kind == "cancelled" && !errors.Is(result.err, context.Canceled) {
					t.Fatalf("unexpected cancellation result: %v", result.err)
				}
				w.commandCatalogFinished(result)
				if kind == "transport" {
					if w.client != nil || queueState(t, w, 100) != "uncertain" || queueState(t, w, 101) != "cancelled" {
						t.Fatal("Unknown new epoch concealed a stale transport failure")
					}
					return
				}
				if w.client != client || w.runtime != runtimeConnected || w.active != 100 || queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "pending" || w.nativeCatalog.cancel == nil || client.CommandCatalog().State != omp.CatalogUnknown {
					t.Fatal("stale context error interrupted the current root or discarded discovery")
				}
				nativeRPC(t, w, "get_state", nil)
				queries := 0
				for _, entry := range fixtureRPCTrace(trace) {
					if entry.kind == "catalog_query" {
						queries++
					}
				}
				if queries != 1 {
					t.Fatal("Unknown refresh overlapped the still in-flight discovery query")
				}
				nativeRPC(t, w, "fixture_finish_root", nil)
				nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
				if queueState(t, w, 101) != "pending" || w.taskActive() || len(w.steers) != 0 {
					t.Fatal("slash-headed queued prompt bypassed Unknown catalog discovery")
				}
				nativeRPC(t, w, "fixture_catalog_update", map[string]any{"releaseQuery": true})
				nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "cancelled" && w.nativeCatalog.cancel == nil })
				nativeInput(t, w, 102, "after discovery", false)
				nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
				output := nativeOutputs(t, w)
				if !strings.Contains(output, "root completed") || !strings.Contains(output, "answer: after discovery") || strings.Contains(output, "Native command discovery failed") {
					t.Fatal("stale expiry lost the root/following prompt or emitted a discovery failure")
				}
				for _, entry := range fixtureRPCTrace(trace) {
					if entry.kind == "extension_session_mutation" || entry.kind == "prompt" && strings.HasPrefix(entry.payload, "/extension_switch") {
						t.Fatal("queued extension command reached RPC before or after discovery")
					}
				}
			})
		}
	}
}

func TestNativeCommandUnknownNewEpochDoesNotBlockOrdinaryFollowup(t *testing.T) {
	w, _, trace := nativeWorker(t, "hold")
	waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
	nativeInput(t, w, 100, "wait", false)
	nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"newSession": true, "skipUpdate": true})
	nativeInput(t, w, 101, "/followup ordinary followup", false)
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	if w.client == nil || w.client.CommandCatalog().State != omp.CatalogUnknown || !strings.Contains(nativeOutputs(t, w), "answer: ordinary followup") {
		t.Fatal("Unknown new catalog blocked a non-slash queued prompt")
	}
	waitFixtureRPCTrace(t, trace, "prompt", "ordinary followup")
}

func awaitNativeMenu(t *testing.T, requests <-chan map[string]any, chat int64, native ...string) []telegram.BotCommand {
	t.Helper()
	want := make([]string, 0, len(botCommands)+len(native))
	for _, command := range botCommands {
		want = append(want, command.Command)
	}
	want = append(want, native...)
	slices.Sort(want)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var last []string
	for {
		select {
		case request := <-requests:
			scope, _ := request["scope"].(map[string]any)
			if scope["type"] != "chat" || scope["chat_id"] != float64(chat) || request["language_code"] != "" {
				continue
			}
			raw, err := json.Marshal(request["commands"])
			if err != nil {
				t.Fatal(err)
			}
			var commands []telegram.BotCommand
			if err := json.Unmarshal(raw, &commands); err != nil {
				t.Fatal(err)
			}
			last = last[:0]
			for _, command := range commands {
				last = append(last, command.Command)
			}
			slices.Sort(last)
			if slices.Equal(last, want) {
				return commands
			}
		case <-timer.C:
			t.Fatalf("chat menu did not converge: got %v, want %v", last, want)
		}
	}
}

func TestNativeMenuDiscoverySourceChangesAndClose(t *testing.T) {
	w, command, _ := nativeWorker(t, "enabled")
	requests := make(chan map[string]any, 64)
	http.DefaultTransport.(*fakeHTTP).commandRequests = requests
	w.b.cfg.AllowedChats = []int64{w.key.chat}
	w.b.registerCommandMenuWorker(w)
	ctx, cancel := context.WithCancel(w.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.b.runCommandMenus(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	nativeCatalogReady(t, w)
	awaitNativeMenu(t, requests, w.key.chat, "foo", "native_agent", "native_local", "session")

	// Aliases become usable only when their advertised source becomes executable.
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"sourceName": "extension_switch", "source": "file"})
	w.commandCatalogUpdated()
	awaitNativeMenu(t, requests, w.key.chat, "extension_alias", "extension_switch", "foo", "native_agent", "native_local", "session")

	// A missing source revokes menu availability instead of inheriting old permission.
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"sourceName": "native_local"})
	w.commandCatalogUpdated()
	awaitNativeMenu(t, requests, w.key.chat, "extension_alias", "extension_switch", "foo", "native_agent", "session")
	command("/close")
	awaitNativeMenu(t, requests, w.key.chat)
	command("/new")
	nativeCatalogReady(t, w)
	awaitNativeMenu(t, requests, w.key.chat, "foo", "native_agent", "native_local", "session")
}

func TestNativeCommandReclassificationKeepsAttachmentPreparing(t *testing.T) {
	for _, kind := range []string{"document", "photo"} {
		t.Run(kind, func(t *testing.T) {
			w, _, trace := nativeWorker(t, "hold")
			nativeInput(t, w, 100, "/extension_switch", false)
			gate := make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-gate:
				default:
					close(gate)
				}
			})
			f := http.DefaultTransport.(*fakeHTTP)
			data := []byte("downloaded attachment contents")
			if kind == "photo" {
				data = testPNG(t)
			}
			f.mu.Lock()
			f.files = map[string][]byte{"attachment": data}
			f.downloadGate = gate
			f.mu.Unlock()
			u := update(101, 11, "")
			u.Message.Caption = "prepared attachment caption"
			if kind == "photo" {
				u.Message.Photo = []telegram.PhotoSize{{FileID: "attachment", Width: 8, Height: 8, FileSize: int64(len(data))}}
			} else {
				u.Message.Document = &telegram.Document{FileID: "attachment", FileName: "note.txt", FileSize: int64(len(data))}
			}
			raw, err := json.Marshal(u)
			requireStoreOK(t, err)
			requireStoreOK(t, w.b.db.Accept(101, raw))
			w.handle(incoming{id: 101, msg: u.Message})
			waitFor(t, func() bool {
				f.mu.Lock()
				defer f.mu.Unlock()
				return f.downloadRequests == 1
			})
			// Publish to the reader without consuming the actor's catalog wakeup.
			nativeRPC(t, w, "fixture_catalog_update", nil)
			w.dispatch()
			if queueState(t, w, 100) != "cancelled" || queueState(t, w, 101) != "pending" || len(w.queue) != 1 || w.queue[0].id != 101 || !w.queue[0].preparing || w.taskActive() || w.rpcOperationActive {
				t.Fatal("reclassification submitted or lost the still-preparing attachment")
			}
			close(gate)
			var prepared mediaResult
			select {
			case prepared = <-w.mediaResults:
			case <-time.After(5 * time.Second):
				t.Fatal("controlled attachment preparation did not finish")
			}
			if prepared.err != nil || !strings.Contains(prepared.input.Text, u.Message.Caption) {
				t.Fatalf("attachment preparation lost its content: %v", prepared.err)
			}
			if kind == "document" {
				contents, err := os.ReadFile(filepath.Join(prepared.input.Directory, "note.txt"))
				if err != nil || string(contents) != string(data) {
					t.Fatalf("downloaded document contents changed: %v", err)
				}
			}
			w.preparedMedia(prepared)
			w.dispatch()
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
			nativeRPC(t, w, "get_state", nil)
			var prompts int
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "prompt" && entry.payload != "/session info" {
					prompts++
				}
			}
			if prompts != 1 || !strings.Contains(nativeOutputs(t, w), u.Message.Caption) || len(w.queue) != 0 || w.taskActive() {
				t.Fatalf("prepared attachment did not complete as one native prompt: prompts=%d", prompts)
			}
			if kind == "photo" && !strings.Contains(nativeOutputs(t, w), "image=8x8;") {
				t.Fatal("native consumer did not receive the downloaded image")
			}
		})
	}
}

func TestNativeCommandResearchLookupRefreshesPendingCatalog(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "updated", true: "unknown"}[unknown], func(t *testing.T) {
			w, _, trace := nativeWorker(t, "autoresearch-hold")
			nativeInput(t, w, 99, "wait", false)
			nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
			before := w.binding
			client, claimed, sessionID := w.client, w.claimedSession, w.sessionID
			nativeInput(t, w, 100, "/autoresearch goal", false)
			nativeInput(t, w, 101, "/extension_switch", false)
			nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
			nativeRPC(t, w, "fixture_catalog_update", map[string]any{"removeName": "extension_switch"})
			w.resolveSlashInputs()
			waitFixtureRPCTrace(t, trace, "entries_waiting", "")
			fields := map[string]any{"addName": "extension_switch", "sourceName": "extension_switch", "source": "extension"}
			if unknown {
				fields["newSession"], fields["skipUpdate"] = true, true
			}
			nativeRPC(t, w, "fixture_catalog_update", fields)
			nativeRPC(t, w, "fixture_autoresearch_release_entries", nil)
			nativeUntil(t, w, func() bool { return !w.rpcOperationActive && w.research.control == nil })
			if unknown {
				if client.CommandCatalog().State != omp.CatalogUnknown || queueState(t, w, 101) != "pending" || len(w.queue) != 1 || w.queue[0].id != 101 || w.queue[0].kind != queuedSlashPending || len(w.steers) != 0 || w.rpcOperationActive {
					t.Fatal("catalog invalidation did not preserve the remaining pending slash input")
				}
				nativeRPC(t, w, "fixture_catalog_update", nil)
				w.resolveSlashInputs()
			} else {
				assertNativeSessionPreserved(t, w, before, client, claimed, sessionID)
			}
			nativeRPC(t, w, "get_state", nil)
			if queueState(t, w, 101) != "cancelled" || len(w.steers) != 0 || w.active != 99 || !w.taskActive() || w.rpcOperationActive {
				t.Fatal("newly advertised extension escaped refusal or changed the active root")
			}
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "extension_session_mutation" || entry.kind == "prompt" && entry.payload != "/session info" && entry.payload != "wait" {
					t.Fatalf("forbidden extension reached the native consumer: %s %q", entry.kind, entry.payload)
				}
			}
		})
	}
}

func TestNativeCommandDispatchRevalidatesAfterResearchLookup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		text      string
		update    map[string]any
		wantState string
	}{
		{"extension", "/native_local", map[string]any{"sourceName": "native_local", "source": "extension"}, "cancelled"},
		{"unknown", "/native_local", map[string]any{"sourceName": "native_local", "source": "future_private_source"}, "cancelled"},
		{"followup_extension", "/followup /native_local", map[string]any{"sourceName": "native_local", "source": "extension"}, "cancelled"},
		{"followup_unknown", "/followup /native_local", map[string]any{"sourceName": "native_local", "source": "future_private_source"}, "cancelled"},
		{"removed", "/native_local", map[string]any{"removeName": "native_local"}, "cancelled"},
		{"longer_name", "/native_local:argument", map[string]any{"addName": "native_local:argument"}, "cancelled"},
		{"still_available", "/native_local", map[string]any{"addName": "unrelated_command"}, "done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			before := w.binding
			client, claimed, sessionID := w.client, w.claimedSession, w.sessionID
			nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
			nativeInput(t, w, 100, tc.text, false)
			waitFixtureRPCTrace(t, trace, "entries_waiting", "")
			nativeRPC(t, w, "fixture_catalog_update", tc.update)
			nativeRPC(t, w, "fixture_autoresearch_release_entries", nil)
			nativeUntil(t, w, func() bool {
				state := queueState(t, w, 100)
				return state != "pending" && state != "submitted" && !w.rpcOperationActive && w.research.control == nil
			})
			nativeRPC(t, w, "get_state", nil)
			if state := queueState(t, w, 100); state != tc.wantState {
				t.Fatalf("input state=%s, want %s after catalog update during mode lookup", state, tc.wantState)
			}
			assertNativeSessionPreserved(t, w, before, client, claimed, sessionID)
			text := strings.TrimPrefix(tc.text, "/followup ")
			wantPrompts := 0
			if tc.wantState == "done" {
				wantPrompts = 1
			}
			if count := researchTraceCount(trace, "prompt", text); count != wantPrompts {
				t.Fatalf("native consumer received %d prompts, want %d", count, wantPrompts)
			}
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "extension_session_mutation" {
					t.Fatal("updated extension source changed the native session")
				}
			}
		})
	}
}

func TestUnsupportedDiscoveryCannotExecuteSlashInput(t *testing.T) {
	for _, path := range []string{"direct-idle", "direct-running", "pending-idle", "pending-running", "followup", "prepared-attachment", "steer"} {
		t.Run(path, func(t *testing.T) {
			pending := strings.HasPrefix(path, "pending-")
			busy := path != "direct-idle" && path != "pending-idle"
			mode := "unsupported"
			if pending {
				mode = "hold"
			}
			w, _, trace := nativeWorker(t, mode)
			if pending {
				waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
			} else {
				nativeUntil(t, w, func() bool { return w.nativeCatalog.cancel == nil })
			}
			before, client := w.binding, w.client
			if busy {
				nativeInput(t, w, 100, "wait", false)
				nativeUntil(t, w, func() bool { return w.busy && !w.rpcOperationActive })
			}
			text := "/extension_switch"
			var directory string
			switch path {
			case "followup":
				nativeInput(t, w, 101, "/followup "+text, false)
			case "prepared-attachment", "steer":
				u := update(101, 11, text)
				raw, err := json.Marshal(u)
				requireStoreOK(t, err)
				requireStoreOK(t, w.b.db.Accept(101, raw))
				in := incoming{id: 101, msg: u.Message}
				if path == "steer" {
					// Revalidate the active-task path independently of slash routing.
					w.routePrompt(in, text)
				} else {
					// Current attachment preparation adds a safe prefix; defend raw queued representations too.
					w.enqueuePreparedPrompt(in, text, text)
					directory = filepath.Join(w.b.cfg.DataDir, "attachments", "inbox", "incoming-unsupported")
					requireStoreOK(t, os.MkdirAll(directory, 0700))
					w.queue[0].images = []media.Image{{MimeType: "image/png", Data: "AA=="}}
					w.queue[0].directory = directory
				}
			default:
				nativeInput(t, w, 101, text, false)
			}
			nativeInput(t, w, 102, "/followup ordinary after refusal", false)
			if pending {
				if w.client.CommandCatalog().State != omp.CatalogUnknown || queueState(t, w, 101) != "pending" || queueState(t, w, 102) != "pending" {
					t.Fatal("Unknown discovery did not preserve slash classification and FIFO")
				}
				if researchTraceCount(trace, "prompt", text) != 0 || researchTraceCount(trace, "prompt", "ordinary after refusal") != 0 {
					t.Fatal("Unknown discovery submitted the waiting slash or bypassed the FIFO head")
				}
				nativeRPC(t, w, "fixture_catalog_unsupported", nil)
			}
			if path != "followup" && path != "prepared-attachment" {
				nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "cancelled" })
				if busy && (w.active != 100 || !w.busy || len(w.steers) != 0) {
					t.Fatal("slash refusal changed the running root or created a steer")
				}
			}
			if busy {
				nativeRPC(t, w, "fixture_finish_root", nil)
			}
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "cancelled" })
			w.dispatch()
			nativeUntil(t, w, func() bool { return queueState(t, w, 102) == "done" })
			if queueState(t, w, 101) != "cancelled" || !strings.Contains(nativeOutputs(t, w), "answer: ordinary after refusal") {
				t.Fatal("slash input was not cancelled or subsequent ordinary work lost its final reply")
			}
			if w.client != client || client.CommandCatalog().State != omp.CatalogUnsupported || !sameBindingIdentity(before, w.binding) {
				t.Fatal("Unsupported refusal replaced the runtime, catalog state, or binding")
			}
			info, err := client.SessionInfo(w.ctx)
			if err != nil || info.ID != before.SessionID || info.File != before.Session {
				t.Fatalf("native session diverged from the binding: info=%+v err=%v", info, err)
			}
			mutations := 0
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "extension_session_mutation" {
					mutations++
				}
			}
			if mutations != 0 || researchTraceCount(trace, "prompt", text) != 0 {
				t.Fatalf("Unsupported discovery allowed extension execution: mutations=%d", mutations)
			}
			if directory != "" {
				if _, err := os.Stat(directory); !os.IsNotExist(err) {
					t.Fatalf("cancelled attachment directory remains: %v", err)
				}
			}
		})
	}
}

func TestUnsupportedDiscoveryPreservesBridgeCommandsWithoutRawReviewBypass(t *testing.T) {
	w, _, trace := nativeWorker(t, "unsupported")
	nativeUntil(t, w, func() bool { return w.nativeCatalog.cancel == nil })
	nativeInput(t, w, 100, "/followup /review unsafe raw input", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "cancelled" })
	nativeInput(t, w, 101, "/review inspect", false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" })
	if !strings.Contains(nativeOutputs(t, w), "answer: /review inspect") || researchTraceCount(trace, "prompt", "/review unsafe raw input") != 0 {
		t.Fatal("explicit bridge review was blocked or raw followup impersonated bridge ownership")
	}
	nativeInput(t, w, 102, "/status", false)
	nativeUntil(t, w, func() bool { return !w.rpcOperationActive })
	if queueState(t, w, 102) != "done" || !strings.Contains(nativeOutputs(t, w), "Session:") {
		t.Fatal("Unsupported discovery blocked bridge status")
	}
}
