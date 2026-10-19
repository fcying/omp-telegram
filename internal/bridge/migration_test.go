package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func TestMigrationRecoveryStartupDeadlineCancelsPendingRuntime(t *testing.T) {
	for _, stage := range []string{"pre-ready", "handshake"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				startupCtx, startupCancel := context.WithCancel(ctx)
				defer startupCancel()
				startup := &migrationStartup{attempt: 1, ctx: startupCtx, cancel: startupCancel}
				w := testWorker(t, &worker{ctx: ctx, migrationAttempt: 1, migrationStartup: startup, migrationRun: &migrationRun{id: "deadline", ctx: ctx}})
				started := make(chan context.Context, 1)
				began := time.Now()
				w.startMigrationJob("recover-start", 45*time.Second, func(phaseCtx context.Context) migrationResult {
					return startup.run(phaseCtx, func(processCtx context.Context) migrationResult {
						started <- processCtx
						if stage == "pre-ready" {
							<-processCtx.Done()
							return migrationResult{err: processCtx.Err()}
						}
						<-phaseCtx.Done()
						// Even a final handshake response racing the deadline cannot win.
						return migrationResult{}
					})
				})
				processCtx := <-started
				time.Sleep(44 * time.Second)
				synctest.Wait()
				if processCtx.Err() != nil {
					t.Fatal("pending process was canceled before the recovery deadline")
				}
				select {
				case result := <-w.migrationResults:
					t.Fatalf("pending startup returned before its deadline: %+v", result)
				default:
				}
				time.Sleep(time.Second)
				synctest.Wait()
				w.background.Wait()
				select {
				case result := <-w.migrationResults:
					if !errors.Is(result.err, context.DeadlineExceeded) || processCtx.Err() == nil || time.Since(began) != 45*time.Second {
						t.Fatalf("deadline did not cancel the process context: result=%+v process=%v elapsed=%v", result, processCtx.Err(), time.Since(began))
					}
					if w.migrationStartup != startup {
						t.Fatal("startup ownership was released before its result was reaped")
					}
					w.discardMigrationClient(result)
					if w.migrationStartup != nil {
						t.Fatal("reaping the timed-out result did not release startup ownership")
					}
				default:
					t.Fatal("recovery deadline did not return the pending startup result")
				}
			})
		})
	}
}

func TestMigrationRecoveryStartupSuccessOutlivesPhaseContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startupCtx, startupCancel := context.WithCancel(ctx)
		defer startupCancel()
		startup := &migrationStartup{attempt: 1, ctx: startupCtx, cancel: startupCancel}
		w := testWorker(t, &worker{ctx: ctx, migrationAttempt: 1, migrationStartup: startup, migrationRun: &migrationRun{id: "success", ctx: ctx}})
		var phaseCtx, processCtx context.Context
		w.startMigrationJob("recover-start", 45*time.Second, func(ctx context.Context) migrationResult {
			phaseCtx = ctx
			return startup.run(ctx, func(ctx context.Context) migrationResult {
				processCtx = ctx
				return migrationResult{}
			})
		})
		w.background.Wait()
		result := <-w.migrationResults
		if result.err != nil || phaseCtx.Err() != context.Canceled || processCtx.Err() != nil {
			t.Fatalf("successful startup inherited deferred phase cancellation: result=%+v phase=%v process=%v", result, phaseCtx.Err(), processCtx.Err())
		}
		time.Sleep(46 * time.Second)
		synctest.Wait()
		if processCtx.Err() != nil {
			t.Fatalf("successful runtime inherited the startup deadline: %v", processCtx.Err())
		}
		startup.cancel()
		if processCtx.Err() != context.Canceled {
			t.Fatal("runtime lifetime cancellation was detached from its owner")
		}
	})
}

func migrationWorkflowWorker(t *testing.T, mode string) (*worker, *fakeHTTP, string) {
	t.Helper()
	t.Setenv("OMP_TELEGRAM_FIXTURE_MIGRATION", mode)
	t.Setenv("OMP_TELEGRAM_FIXTURE_NATIVE_COMMANDS", "")
	t.Setenv("OMP_TELEGRAM_FIXTURE_CONFIGURED_THINKING", "auto")
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
	trace := filepath.Join(t.TempDir(), "migration.trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	w, fake, command := setupWorkspaceWorker(t)
	// The real verification deadline is 30 seconds; do not let the generic
	// twenty-second workspace fixture deadline turn it into process failure.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	w.ctx, w.cancel = ctx, cancel
	t.Cleanup(cancel)
	w.b.cfg.QueueCapacity = 8
	w.b.cfg.IdleTimeout = time.Minute
	command("/new " + t.TempDir())
	nativeCatalogReady(t, w)
	return w, fake, trace
}

// Drive real asynchronous job results and reader events on the actor's owner
// goroutine. Tests deliberately pause between results to observe durable phases.
func migrationWorkflowResult(t *testing.T, w *worker, stage string, timeout time.Duration) migrationResult {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		var events <-chan json.RawMessage
		if w.client != nil {
			events = w.client.Events()
		}
		select {
		case result := <-w.migrationResults:
			if result.attempt != w.migrationAttempt {
				w.migrationFinished(result)
				continue
			}
			if result.stage != stage {
				if stage == "recover-start" && result.stage == "recover-resolve" {
					w.migrationFinished(result)
					continue
				}
				if run := w.migrationRun; run == nil || result.id != run.id || result.clientID != w.migrationClientID || run.ctx.Err() != nil {
					w.migrationFinished(result)
					continue
				}
				t.Fatalf("migration result stage = %q, want %q", result.stage, stage)
			}
			return result
		case result := <-w.nativeCatalog.results:
			w.commandCatalogFinished(result)
		case result := <-w.operations:
			w.operationReturned(result)
		case raw, ok := <-events:
			if !ok {
				t.Fatal("migration fixture runtime exited unexpectedly")
			}
			w.event(raw)
		case <-timer.C:
			t.Fatalf("migration stage %s did not return", stage)
		}
	}
}

func migrationWorkflowStep(t *testing.T, w *worker, stage string) {
	t.Helper()
	result := migrationWorkflowResult(t, w, stage, 5*time.Second)
	if result.err != nil {
		t.Fatalf("migration stage %s failed: %v", stage, result.err)
	}
	w.migrationFinished(result)
}

func migrationWorkflowPhase(t *testing.T, w *worker, source store.Binding, phase string) store.Migration {
	t.Helper()
	m, err := w.b.db.Migration(source.Bot, source.Chat, source.Thread)
	requireStoreOK(t, err)
	if w.migration == nil || w.migration.ID != m.ID || m.Phase != phase || m.Source != source || !sameBindingIdentity(w.binding, source) {
		t.Fatalf("phase %s changed source or lost durable state: migration=%+v binding=%+v", phase, m, w.binding)
	}
	persisted, err := w.b.db.Binding(source.Bot, source.Chat, source.Thread)
	requireStoreOK(t, err)
	if !sameBindingIdentity(persisted, source) {
		t.Fatalf("phase %s prematurely published a target: %+v", phase, persisted)
	}
	return m
}

func migrationWorkflowInbox(t *testing.T, w *worker, id int64, want store.InboxState) {
	t.Helper()
	var state store.InboxState
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", id).Scan(&state))
	if state != want {
		t.Fatalf("inbox %d = %s, want %s", id, state, want)
	}
}

func migrationWorkflowClaims(t *testing.T, w *worker, m store.Migration) {
	t.Helper()
	for _, identity := range []struct{ file, id string }{{m.Source.Session, m.Source.SessionID}, {m.CandidateFile, m.CandidateID}} {
		if identity.id == "" {
			continue
		}
		if !w.b.sessionInUse(identity.id) {
			t.Fatalf("migration identity %s was released", identity.id)
		}
		w.b.sessionMu.Lock()
		claim, exists := w.b.sessionClaims[identity.file]
		w.b.sessionMu.Unlock()
		if !exists || claim.owner != w || claim.id != identity.id || !claim.migration {
			t.Fatalf("migration reservation missing for %s: %+v", identity.id, claim)
		}
		busy, elsewhere := w.b.sessionDeleteStatus(nil, identity.id[:8])
		if !busy || !elsewhere {
			t.Fatalf("migration prefix %s is not protected from another worker", identity.id[:8])
		}
	}
}

func migrationWorkflowTraceCount(trace, kind string) int {
	count := 0
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == kind {
			count++
		}
	}
	return count
}

func migrationWorkflowToVerify(t *testing.T, w *worker) (store.Binding, store.Migration) {
	t.Helper()
	source := w.binding
	acceptWorkerInput(t, w, 100, "/migrate")
	if w.migrationRun == nil || !w.migrationLocked() || w.migration != nil {
		t.Fatal("public migrate input did not start asynchronous preflight")
	}
	migrationWorkflowInbox(t, w, 100, store.InboxSubmitted)
	migrationWorkflowStep(t, w, "prepare")
	migrationWorkflowPhase(t, w, source, "handoff")
	migrationWorkflowStep(t, w, "handoff")
	m := migrationWorkflowPhase(t, w, source, "initialize")
	if m.SummaryEntryID == "" || len(m.SummaryHash) != 64 || m.Provider != "fixture" || m.Model != "safe" || m.Thinking != "auto" {
		t.Fatalf("handoff evidence or configured settings were not frozen: %+v", m)
	}
	migrationWorkflowStep(t, w, "candidate")
	m = migrationWorkflowPhase(t, w, source, "verify")
	if m.CandidateID == "" || m.CandidateID == source.SessionID || m.CandidateFile == source.Session || !filepath.IsAbs(m.CandidateFile) {
		t.Fatalf("candidate was not durably recorded before verification: %+v", m)
	}
	migrationWorkflowClaims(t, w, m)
	return source, m
}

func migrationWorkflowCommitted(t *testing.T, w *worker, source store.Binding, targetID, targetFile string) {
	t.Helper()
	if w.migrationLocked() || w.migration != nil || w.migrationRun != nil || w.binding.Generation != source.Generation+1 || w.binding.SessionID != targetID || w.binding.Session != targetFile || !w.binding.Running {
		t.Fatalf("migration did not publish the verified target: %+v", w.binding)
	}
	persisted, err := w.b.db.Binding(source.Bot, source.Chat, source.Thread)
	requireStoreOK(t, err)
	if !sameBindingIdentity(persisted, w.binding) {
		t.Fatalf("runtime binding differs from committed binding: %+v", persisted)
	}
	if _, err := w.b.db.Migration(source.Bot, source.Chat, source.Thread); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("committed migration remains locked: %v", err)
	}
	migrationWorkflowInbox(t, w, 100, store.InboxDone)
	var text, state string
	var count int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE inbox_id=100").Scan(&count))
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text,state FROM outbox WHERE inbox_id=100").Scan(&text, &state))
	if count != 1 || state != string(store.OutboxPending) || !strings.Contains(text, targetID) {
		t.Fatalf("durable final notification = count:%d state:%s text:%q", count, state, text)
	}
	w.b.sessionMu.Lock()
	claim, exists := w.b.sessionClaims[targetFile]
	owned := 0
	for _, c := range w.b.sessionClaims {
		if c.owner == w {
			owned++
		}
	}
	w.b.sessionMu.Unlock()
	if !exists || claim.owner != w || claim.id != targetID || claim.migration || owned != 1 {
		t.Fatalf("committed target did not replace dual migration claims: %+v, owned=%d", claim, owned)
	}
	if w.client == nil {
		t.Fatal("migration commit discarded its recovered runtime")
	}
	select {
	case <-w.client.Done():
		t.Fatal("migration commit canceled the recovered runtime lifetime")
	case <-time.After(25 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(w.ctx, time.Second)
	defer cancel()
	raw, err := w.client.Call(ctx, "get_state", nil)
	requireStoreOK(t, err)
	var live struct {
		ID string `json:"sessionId"`
	}
	if json.Unmarshal(raw, &live) != nil || live.ID != targetID {
		t.Fatalf("committed runtime is not usable on the selected target: %s", raw)
	}
}

func TestMigrationWorkflowCommitsOnlyAfterIndependentVerification(t *testing.T) {
	w, _, trace := migrationWorkflowWorker(t, "success")
	source, m := migrationWorkflowToVerify(t, w)
	migrationWorkflowStep(t, w, "verify")
	migrationWorkflowCommitted(t, w, source, m.CandidateID, m.CandidateFile)
	if _, err := os.Stat(source.Session); err != nil {
		t.Fatalf("migration removed the original journal: %v", err)
	}
	if _, err := os.Stat(m.CandidateFile); err != nil {
		t.Fatalf("migration did not create a fresh persistent journal: %v", err)
	}
	var history int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM history WHERE bot=? AND chat=? AND thread=? AND session=?", source.Bot, source.Chat, source.Thread, source.Session).Scan(&history))
	if history != 1 {
		t.Fatalf("source archive count = %d", history)
	}
	for _, kind := range []string{"handoff", "migration_command", "new_session"} {
		if got := migrationWorkflowTraceCount(trace, kind); got != 1 {
			t.Fatalf("%s side effect occurred %d times", kind, got)
		}
	}
	for _, kind := range []string{"get_entries", "get_messages"} {
		if migrationWorkflowTraceCount(trace, kind) == 0 {
			t.Fatalf("independent %s verification was skipped", kind)
		}
	}
}

func TestMigrationWorkflowFailureRetainsSourceCandidateLockAndInbox(t *testing.T) {
	for _, mode := range []string{"setup-fail", "hash-mismatch", "model-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			w, _, trace := migrationWorkflowWorker(t, mode)
			source, m := migrationWorkflowToVerify(t, w)
			result := migrationWorkflowResult(t, w, "verify", 5*time.Second)
			if result.err == nil {
				t.Fatal("invalid candidate passed independent verification")
			}
			w.migrationFinished(result)
			m = migrationWorkflowPhase(t, w, source, "verify")
			if m.ErrorKind == "" || !w.migrationLocked() || w.migrationRun != nil || w.client != nil || m.CandidateID == "" {
				t.Fatalf("failed migration lost its lock or candidate: %+v", m)
			}
			migrationWorkflowClaims(t, w, m)
			migrationWorkflowInbox(t, w, 100, store.InboxUncertain)
			if migrationWorkflowTraceCount(trace, "new_session") != 1 || migrationWorkflowTraceCount(trace, "handoff") != 1 {
				t.Fatal("failed migration retried a session-creating side effect")
			}
			var finals int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE inbox_id=100").Scan(&finals))
			if finals != 0 {
				t.Fatal("failed migration enqueued a successful completion")
			}
		})
	}
}

func TestMigrationWorkflowBudgetRestoreFailureCannotCreateOrCommitCandidate(t *testing.T) {
	w, _, trace := migrationWorkflowWorker(t, "budget-restore-fail")
	source := w.binding
	acceptWorkerInput(t, w, 100, "/migrate")
	migrationWorkflowStep(t, w, "prepare")
	result := migrationWorkflowResult(t, w, "handoff", 5*time.Second)
	if result.err == nil {
		t.Fatal("unconfirmed budget restoration allowed migration to continue")
	}
	w.migrationFinished(result)
	m := migrationWorkflowPhase(t, w, source, "handoff")
	if !w.migrationLocked() || w.client != nil || w.migrationRun != nil || m.CandidateID != "" {
		t.Fatalf("unrestored runtime was reused or a candidate was published: %+v", m)
	}
	migrationWorkflowInbox(t, w, 100, store.InboxUncertain)
	if migrationWorkflowTraceCount(trace, "new_session") != 0 || migrationWorkflowTraceCount(trace, "migration_command") != 0 {
		t.Fatal("unconfirmed restoration reached fresh-session initialization")
	}
}

func TestMigrationWorkflowMarkerTimeoutKeepsKnownCandidate(t *testing.T) {
	w, _, trace := migrationWorkflowWorker(t, "no-marker")
	source, _ := migrationWorkflowToVerify(t, w)
	result := migrationWorkflowResult(t, w, "verify", 40*time.Second)
	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("missing marker error = %v, want bounded verification deadline", result.err)
	}
	w.migrationFinished(result)
	m := migrationWorkflowPhase(t, w, source, "verify")
	if m.CandidateID == "" || m.ErrorKind == "" || w.client != nil || !w.migrationLocked() {
		t.Fatalf("verification timeout lost its known candidate or lock: %+v", m)
	}
	migrationWorkflowClaims(t, w, m)
	migrationWorkflowInbox(t, w, 100, store.InboxUncertain)
	if migrationWorkflowTraceCount(trace, "new_session") != 1 {
		t.Fatal("verification polling recreated the candidate")
	}
}

func TestMigrationWorkflowExplicitRecoveryNeverRepeatsHandoffOrCreation(t *testing.T) {
	for _, recoverOld := range []bool{true, false} {
		name := "candidate"
		if recoverOld {
			name = "old"
		}
		t.Run(name, func(t *testing.T) {
			w, _, trace := migrationWorkflowWorker(t, "held")
			source, m := migrationWorkflowToVerify(t, w)
			waitFixtureRPCTrace(t, trace, "migration_waiting", m.CandidateID)
			acceptWorkerInput(t, w, 101, "/stop")
			migrationWorkflowPhase(t, w, source, "verify")
			migrationWorkflowClaims(t, w, m)
			migrationWorkflowInbox(t, w, 100, store.InboxUncertain)
			migrationWorkflowInbox(t, w, 101, store.InboxDone)
			if w.client != nil || w.migrationRun != nil || !w.migrationLocked() {
				t.Fatal("stop did not cancel only the runtime while retaining the migration lock")
			}
			target, file := m.CandidateID, m.CandidateFile
			argument := target
			if recoverOld {
				argument, target, file = "old", source.SessionID, source.Session
			}
			acceptWorkerInput(t, w, 102, "/migrate recover "+argument)
			migrationWorkflowStep(t, w, "recover-start")
			migrationWorkflowStep(t, w, "recover-verify")
			migrationWorkflowCommitted(t, w, source, target, file)
			if w.migrationStartup != nil || w.migrationRuntimeCancel == nil || len(w.b.slots) != 1 {
				t.Fatal("successful recovery did not transfer exactly one live runtime and slot")
			}
			migrationWorkflowInbox(t, w, 102, store.InboxDone)
			if migrationWorkflowTraceCount(trace, "handoff") != 1 || migrationWorkflowTraceCount(trace, "migration_command") != 1 || migrationWorkflowTraceCount(trace, "new_session") != 1 {
				t.Fatal("explicit recovery repeated handoff or fresh-session initialization")
			}
		})
	}
}

func TestMigrationWorkflowPendingLockBlocksPublicInputsAndIdleRelease(t *testing.T) {
	w, fake, trace := migrationWorkflowWorker(t, "held")
	source, m := migrationWorkflowToVerify(t, w)
	waitFixtureRPCTrace(t, trace, "migration_waiting", m.CandidateID)
	before := len(fixtureRPCTrace(trace))
	for i, text := range []string{"ordinary input", "/followup next", "/new " + t.TempDir(), "/resume " + source.SessionID, "/model fixture/safe", "/args"} {
		id := int64(200 + i)
		acceptWorkerInput(t, w, id, text)
		migrationWorkflowInbox(t, w, id, store.InboxCancelled)
	}
	for i, msg := range []*telegram.Message{
		{Caption: "image input", Photo: []telegram.PhotoSize{{FileID: "migration-photo", Width: 1, Height: 1}}},
		{Caption: "document input", Document: &telegram.Document{FileID: "migration-document", FileName: "input.txt", MimeType: "text/plain"}},
	} {
		id := int64(210 + i)
		requireStoreOK(t, w.b.db.Accept(id, []byte(`{"update_id":210}`)))
		w.handle(incoming{id: id, msg: msg})
		migrationWorkflowInbox(t, w, id, store.InboxCancelled)
	}
	requireStoreOK(t, w.b.db.Accept(220, []byte(`{"update_id":220}`)))
	w.handle(incoming{id: 220, callback: &telegram.CallbackQuery{ID: "migration-blocked-callback", From: telegram.User{ID: 7}, Data: "new"}})
	migrationWorkflowInbox(t, w, 220, store.InboxCancelled)
	client := w.client
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	if w.canReleaseRuntime() {
		t.Fatal("migration runtime became eligible for idle release")
	}
	w.releaseIdleRuntime(time.Now())
	w.dispatch()
	if w.client != client || client == nil || w.previewBusy || len(w.queue) != 0 || len(w.albums) != 0 || w.resumeCancel != nil || w.startIntent != nil {
		t.Fatal("locked public input reached media preparation, queuing, session control, or idle release")
	}
	fake.mu.Lock()
	fileRequests, downloads := fake.fileRequests, fake.downloadRequests
	callbackAnswered := len(fake.callbacks) == 1 && fake.callbacks[0] != ""
	fake.mu.Unlock()
	if !callbackAnswered {
		t.Fatal("locked callback was not explicitly acknowledged to Telegram")
	}
	if fileRequests != 0 || downloads != 0 {
		t.Fatalf("locked media reached Telegram file APIs: metadata=%d downloads=%d", fileRequests, downloads)
	}
	if got := len(fixtureRPCTrace(trace)); got != before {
		t.Fatalf("blocked public input produced RPC side effects: trace grew from %d to %d", before, got)
	}
	migrationWorkflowPhase(t, w, source, "verify")
	migrationWorkflowClaims(t, w, m)
	acceptWorkerInput(t, w, 229, "/migrate")
	migrationWorkflowInbox(t, w, 229, store.InboxDone)
	if migrationWorkflowTraceCount(trace, "new_session") != 1 || migrationWorkflowTraceCount(trace, "handoff") != 1 {
		t.Fatal("repeated migrate input restarted a locked migration")
	}
	acceptWorkerInput(t, w, 230, "/status")
	migrationWorkflowInbox(t, w, 230, store.InboxDone)
	acceptWorkerInput(t, w, 231, "/close")
	migrationWorkflowInbox(t, w, 231, store.InboxDone)
	migrationWorkflowInbox(t, w, 100, store.InboxUncertain)
	migrationWorkflowPhase(t, w, source, "verify")
	migrationWorkflowClaims(t, w, m)
}

func TestMigrationWorkflowLateCandidateAfterStopRemainsReservedWithoutCommit(t *testing.T) {
	w, _, trace := migrationWorkflowWorker(t, "success")
	source := w.binding
	acceptWorkerInput(t, w, 100, "/migrate")
	migrationWorkflowStep(t, w, "prepare")
	migrationWorkflowStep(t, w, "handoff")
	migrationWorkflowPhase(t, w, source, "initialize")
	// The subprocess has already created a real candidate, but its observation
	// has not yet been delivered to the actor when the public stop arrives.
	result := migrationWorkflowResult(t, w, "candidate", 5*time.Second)
	if result.err != nil || result.info.ID == "" {
		t.Fatalf("candidate observation failed: %+v", result)
	}
	acceptWorkerInput(t, w, 101, "/stop")
	migrationWorkflowInbox(t, w, 100, store.InboxUncertain)
	w.migrationFinished(result)
	m := migrationWorkflowPhase(t, w, source, "initialize")
	if m.CandidateID != result.info.ID || m.CandidateFile != result.info.File || m.ErrorKind == "" || w.client != nil || w.migrationRun != nil || !w.migrationLocked() {
		t.Fatalf("late candidate was lost or committed after cancellation: %+v", m)
	}
	migrationWorkflowClaims(t, w, m)
	migrationWorkflowInbox(t, w, 100, store.InboxUncertain)
	if migrationWorkflowTraceCount(trace, "new_session") != 1 || migrationWorkflowTraceCount(trace, "handoff") != 1 || migrationWorkflowTraceCount(trace, "get_entries") != 0 {
		t.Fatal("late candidate delivery restarted migration work after stop")
	}
	// Repeated delivery is also not authority to unlock or publish the target.
	w.migrationFinished(result)
	migrationWorkflowPhase(t, w, source, "initialize")
	migrationWorkflowClaims(t, w, m)
}

func TestMigrationWorkflowStoppedStartupBlocksRecoveryUntilResultReaped(t *testing.T) {
	w, _, trace := migrationWorkflowWorker(t, "held")
	source, m := migrationWorkflowToVerify(t, w)
	waitFixtureRPCTrace(t, trace, "migration_waiting", m.CandidateID)
	acceptWorkerInput(t, w, 101, "/stop")
	acceptWorkerInput(t, w, 102, "/migrate recover "+m.CandidateID)
	stale := migrationWorkflowResult(t, w, "recover-start", 5*time.Second)
	if stale.err != nil || stale.client == nil || stale.info.ID != m.CandidateID {
		t.Fatalf("first recovery did not produce a real candidate process: %+v", stale)
	}
	if w.migrationStartup == nil || len(w.b.slots) != 1 {
		t.Fatal("completed startup lost ownership before its result was reaped")
	}
	t.Cleanup(func() {
		if stale.client != nil {
			w.migrationFinished(stale)
		}
	})
	acceptWorkerInput(t, w, 103, "/stop")
	starts := migrationWorkflowTraceCount(trace, "migration_runtime_start")
	acceptWorkerInput(t, w, 104, "/migrate recover old")
	migrationWorkflowInbox(t, w, 104, store.InboxDone)
	if w.migrationRun != nil || w.client != nil || migrationWorkflowTraceCount(trace, "migration_runtime_start") != starts {
		t.Fatal("new recovery started before the stopped startup result was reaped")
	}
	oldClient := stale.client
	w.migrationFinished(stale)
	stale.client = nil
	waitFor(t, func() bool {
		select {
		case <-oldClient.Done():
			return true
		default:
			return false
		}
	})
	if len(w.b.slots) != 0 || w.client != nil || w.migrationRun != nil || w.migrationStartup != nil {
		t.Fatal("reaping the canceled startup did not release its process and slot")
	}
	migrationWorkflowPhase(t, w, source, "verify")
	migrationWorkflowClaims(t, w, m)
	acceptWorkerInput(t, w, 105, "/migrate recover old")
	migrationWorkflowStep(t, w, "recover-start")
	migrationWorkflowStep(t, w, "recover-verify")
	migrationWorkflowCommitted(t, w, source, source.SessionID, source.Session)
	if migrationWorkflowTraceCount(trace, "handoff") != 1 || migrationWorkflowTraceCount(trace, "new_session") != 1 {
		t.Fatal("retry after reaping repeated handoff or fresh-session creation")
	}
}

func migrationWorkflowWriteCandidate(t *testing.T, file, id string, request omp.MigrationRequest) {
	t.Helper()
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	requireStoreOK(t, err)
	encoder := json.NewEncoder(f)
	for _, entry := range []any{
		map[string]any{"type": "session", "id": id, "cwd": request.Workspace},
		map[string]any{"type": "model_change", "id": "model", "parentId": nil, "model": "fixture/safe"},
		map[string]any{"type": "message", "id": "seed", "parentId": "model", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": request.Summary}}}},
		map[string]any{"type": "custom", "id": "marker", "parentId": "seed", "customType": omp.MigrationMarkerType, "data": map[string]any{"version": 1, "migrationId": request.MigrationID, "sourceSessionId": request.SourceSessionID, "sourceGeneration": request.SourceGeneration, "targetSessionId": id, "workspace": request.Workspace, "summaryEntryId": "seed", "summarySha256": request.SummarySHA256, "reloadCompleted": true}},
	} {
		if err := encoder.Encode(entry); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	requireStoreOK(t, f.Close())
}

func TestMigrationWorkflowRecoveryConflictNeverRecordsOrOpensUnknownCandidate(t *testing.T) {
	w, _, trace := migrationWorkflowWorker(t, "success")
	source := w.binding
	acceptWorkerInput(t, w, 100, "/migrate")
	migrationWorkflowStep(t, w, "prepare")
	handoff := migrationWorkflowResult(t, w, "handoff", 5*time.Second)
	if handoff.err != nil {
		t.Fatalf("source handoff failed: %v", handoff.err)
	}
	// Stop before initialization, leaving a migration whose candidate identity
	// is genuinely unknown. Recovery must inspect the supplied journal offline.
	acceptWorkerInput(t, w, 101, "/stop")
	m := *w.migration
	m.Phase, m.SummaryEntryID, m.SummaryHash = "initialize", handoff.info.ID, handoff.request.SummarySHA256
	requireStoreOK(t, w.b.db.UpdateMigration(m, "handoff"))
	w.migration = &m
	id := "22222222-2222-4222-8222-222222222222"
	file := filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), id+".jsonl")
	migrationWorkflowWriteCandidate(t, file, id, handoff.request)
	other := &worker{b: w.b, key: target{chat: w.key.chat, thread: w.key.thread + 1}}
	if !other.claimSession(file, id) {
		t.Fatal("competing worker could not claim the unrecorded candidate")
	}
	t.Cleanup(other.releaseSession)
	starts := migrationWorkflowTraceCount(trace, "migration_runtime_start")
	acceptWorkerInput(t, w, 102, "/migrate recover "+file)
	resolved := migrationWorkflowResult(t, w, "recover-resolve", 5*time.Second)
	w.migrationFinished(resolved)
	m = migrationWorkflowPhase(t, w, source, "initialize")
	if m.CandidateID != "" || m.CandidateFile != "" || m.ErrorKind == "" || w.client != nil || w.migrationRun != nil || !w.migrationLocked() {
		t.Fatalf("conflicting candidate was recorded or adopted: %+v", m)
	}
	if got := migrationWorkflowTraceCount(trace, "migration_runtime_start"); got != starts {
		t.Fatalf("conflicting recovery started OMP before reserving the candidate: starts=%d, want %d", got, starts)
	}
	w.b.sessionMu.Lock()
	claim := w.b.sessionClaims[file]
	w.b.sessionMu.Unlock()
	if claim.owner != other || claim.migration || claim.id != id {
		t.Fatalf("conflicting recovery stole the existing claim: %+v", claim)
	}
	migrationWorkflowClaims(t, w, m)
	migrationWorkflowInbox(t, w, 100, store.InboxUncertain)
	if migrationWorkflowTraceCount(trace, "new_session") != 0 {
		t.Fatal("explicit candidate recovery created a new session")
	}
}

func TestMigrationWorkflowStaleResolveCannotReplaceNewRecoverOld(t *testing.T) {
	w, _, trace := migrationWorkflowWorker(t, "held")
	source, m := migrationWorkflowToVerify(t, w)
	waitFixtureRPCTrace(t, trace, "migration_waiting", m.CandidateID)
	acceptWorkerInput(t, w, 101, "/stop")
	acceptWorkerInput(t, w, 102, "/migrate recover "+m.CandidateID)
	stale := migrationWorkflowResult(t, w, "recover-resolve", 5*time.Second)
	if stale.err != nil || stale.client != nil || stale.info.ID != m.CandidateID {
		t.Fatalf("offline candidate resolution failed: %+v", stale)
	}
	acceptWorkerInput(t, w, 103, "/stop")
	acceptWorkerInput(t, w, 104, "/migrate recover old")
	if w.migrationRun == nil || !w.migrationRun.old || stale.attempt == w.migrationAttempt {
		t.Fatal("new recover old did not create an independent attempt")
	}
	starts := migrationWorkflowTraceCount(trace, "migration_runtime_start")
	w.migrationFinished(stale)
	if w.client != nil || w.migrationRun == nil || !w.migrationRun.old || migrationWorkflowTraceCount(trace, "migration_runtime_start") != starts {
		t.Fatal("stale offline candidate result started or replaced the new recover old")
	}
	migrationWorkflowPhase(t, w, source, "verify")
	migrationWorkflowClaims(t, w, m)
	migrationWorkflowStep(t, w, "recover-start")
	migrationWorkflowStep(t, w, "recover-verify")
	migrationWorkflowCommitted(t, w, source, source.SessionID, source.Session)
}

func TestMigrationWorkflowStopCancelsStartupBeforeReadyAndRequiresReaping(t *testing.T) {
	w, _, trace := migrationWorkflowWorker(t, "held")
	source, m := migrationWorkflowToVerify(t, w)
	waitFixtureRPCTrace(t, trace, "migration_waiting", m.CandidateID)
	acceptWorkerInput(t, w, 101, "/stop")
	release := filepath.Join(t.TempDir(), "release-startup")
	t.Setenv("OMP_TELEGRAM_FIXTURE_MIGRATION_START_HOLD", release)
	acceptWorkerInput(t, w, 102, "/migrate recover old")
	migrationWorkflowStep(t, w, "recover-resolve")
	waitFixtureRPCTrace(t, trace, "migration_start_waiting", source.SessionID)
	acceptWorkerInput(t, w, 103, "/stop")
	starts := migrationWorkflowTraceCount(trace, "migration_runtime_start")
	acceptWorkerInput(t, w, 104, "/migrate recover old")
	migrationWorkflowInbox(t, w, 104, store.InboxDone)
	if w.migrationRun != nil || w.client != nil || migrationWorkflowTraceCount(trace, "migration_runtime_start") != starts {
		t.Fatal("recover old bypassed an unreaped startup canceled before ready")
	}
	result := migrationWorkflowResult(t, w, "recover-start", 5*time.Second)
	if result.err == nil {
		t.Fatal("canceled pre-ready subprocess reported successful startup")
	}
	if w.migrationStartup == nil {
		t.Fatal("pre-ready startup ownership was released before reaping its failure")
	}
	w.migrationFinished(result)
	if len(w.b.slots) != 0 || w.client != nil || w.migrationRun != nil || w.migrationStartup != nil {
		t.Fatal("canceled pre-ready startup leaked a runtime or process slot")
	}
	migrationWorkflowPhase(t, w, source, "verify")
	migrationWorkflowClaims(t, w, m)
	migrationWorkflowInbox(t, w, 100, store.InboxUncertain)
	t.Setenv("OMP_TELEGRAM_FIXTURE_MIGRATION_START_HOLD", "")
	acceptWorkerInput(t, w, 105, "/migrate recover old")
	migrationWorkflowStep(t, w, "recover-start")
	migrationWorkflowStep(t, w, "recover-verify")
	migrationWorkflowCommitted(t, w, source, source.SessionID, source.Session)
}

func TestMigrationGateAllowsLanguageChangesWithoutUnlocking(t *testing.T) {
	db, err := store.Open(t.TempDir())
	requireStoreOK(t, err)
	defer db.Close()
	w, _ := newBindingTestWorker(t, db)
	w.migration = &store.Migration{ID: "language-gate", Phase: "verify", Source: w.binding}
	locked := w.migration
	for index, language := range []string{"zh", "en"} {
		id := int64(800 + index)
		requireStoreOK(t, db.Accept(id, []byte(`{"update_id":800}`)))
		if !w.handleMigrationIncoming(incoming{id: id, msg: &telegram.Message{Text: "/lang " + language}}) {
			t.Fatal("migration gate did not handle the language command")
		}
		saved, err := db.Language()
		requireStoreOK(t, err)
		if string(saved) != language || string(w.b.language()) != language {
			t.Fatalf("language was not saved under migration lock: saved=%q runtime=%q", saved, w.b.language())
		}
		migrationWorkflowInbox(t, w, id, store.InboxDone)
		if !w.migrationLocked() || w.migration != locked || len(w.queue) != 0 {
			t.Fatal("language change disturbed the migration lock or queue")
		}
	}
	if !strings.Contains(latestLanguageOutput(t, w), "Global language set to English") {
		t.Fatal("language acknowledgement did not use the selected language")
	}
}

func TestMigrationNoticeUsesSelectedLanguageAndPreservesIdentity(t *testing.T) {
	w, _, _ := migrationWorkflowWorker(t, "success")
	source, migration := migrationWorkflowToVerify(t, w)
	requireStoreOK(t, w.b.db.Accept(801, []byte(`{"update_id":801}`)))
	if !w.handleMigrationIncoming(incoming{id: 801, msg: &telegram.Message{Text: "/lang zh"}}) {
		t.Fatal("migration gate did not accept the language change")
	}
	migrationWorkflowStep(t, w, "verify")
	migrationWorkflowCommitted(t, w, source, migration.CandidateID, migration.CandidateFile)
	var notice string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox WHERE inbox_id=100").Scan(&notice))
	if !strings.HasPrefix(notice, "迁移完成.") || !strings.Contains(notice, source.Workspace) || !strings.Contains(notice, migration.CandidateID) {
		t.Fatalf("durable migration notice missed the selected language or changed native identity: %q", notice)
	}
}
