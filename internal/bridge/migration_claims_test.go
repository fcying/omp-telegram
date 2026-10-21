package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"omp-telegram/internal/config"
	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

const migrationClaimsSourceID = "11111111-1111-4111-8111-111111111111"
const migrationClaimsCandidateID = "22222222-2222-4222-8222-222222222222"

func prepareClaimTestMigration(t *testing.T, db *store.Store, source store.Binding, candidate bool) store.Migration {
	t.Helper()
	if err := db.Save(source); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(update(9001, source.Thread, "/migrate"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Accept(9001, raw); err != nil {
		t.Fatal(err)
	}
	migration := store.Migration{ID: "claims-test-migration", Source: source, InboxID: 9001, Phase: "prepared", Provider: "fixture", Model: "default", Thinking: "auto"}
	if err := db.PrepareMigration(migration); err != nil {
		t.Fatal(err)
	}
	migration, err = db.Migration(source.Bot, source.Chat, source.Thread)
	if err != nil {
		t.Fatal(err)
	}
	if candidate {
		migration.Phase = "handoff"
		migration.SummaryEntryID, migration.SummaryHash = "summary-entry", strings.Repeat("a", 64)
		if err := db.UpdateMigration(migration, "prepared"); err != nil {
			t.Fatal(err)
		}
		migration.Phase = "initialize"
		migration.CandidateID = migrationClaimsCandidateID
		migration.CandidateFile = filepath.Join(source.Workspace, "candidate.jsonl")
		if err := db.UpdateMigration(migration, "handoff"); err != nil {
			t.Fatal(err)
		}
	}
	return migration
}

func migrationClaimWorker(t *testing.T, running bool) *worker {
	t.Helper()
	w, _, _ := setupWorkspaceWorker(t)
	workspace := t.TempDir()
	source := store.Binding{Bot: 99, Chat: -10, Thread: 11, Workspace: workspace, Session: filepath.Join(workspace, "source.jsonl"), SessionID: migrationClaimsSourceID, Generation: 4, Running: running}
	migration := prepareClaimTestMigration(t, w.b.db, source, true)
	for _, file := range []string{source.Session, migration.CandidateFile} {
		if err := os.WriteFile(file, []byte("preserved\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	w.binding, w.migration = source, &migration
	if !w.restoreMigrationClaims() {
		t.Fatal("migration claims were not restored")
	}
	return w
}

func assertMigrationIdentitiesReserved(t *testing.T, w *worker) {
	t.Helper()
	for _, id := range []string{migrationClaimsSourceID, migrationClaimsCandidateID} {
		if !w.b.sessionInUse(id) {
			t.Fatalf("migration identity %s is not reserved", id)
		}
		busy, elsewhere := w.b.sessionDeleteStatus(w, id)
		if !busy || elsewhere {
			t.Fatalf("owner deletion status for %s = %t, %t", id, busy, elsewhere)
		}
		busy, elsewhere = w.b.sessionDeleteStatus(nil, id[:8])
		if !busy || !elsewhere {
			t.Fatalf("cross-worker prefix deletion status for %s = %t, %t", id, busy, elsewhere)
		}
	}
}

func TestMigrationClaimsRetainSourceAndCandidateBeforeCommit(t *testing.T) {
	w := migrationClaimWorker(t, true)
	assertMigrationIdentitiesReserved(t, w)
	if w.claimedSession != w.binding.Session || w.sessionID != migrationClaimsSourceID || len(w.b.sessionClaims) != 2 {
		t.Fatal("candidate reservation replaced the source identity")
	}
	if !w.reserveMigrationCandidate(w.migration.CandidateFile, w.migration.CandidateID) {
		t.Fatal("repeated candidate reservation was not idempotent")
	}
	if w.claimSession("unrelated", "33333333") {
		t.Fatal("ordinary claim replacement erased migration reservations")
	}
	other := &worker{b: w.b, key: target{chat: -10, thread: 22}}
	for _, id := range []string{migrationClaimsSourceID, migrationClaimsCandidateID} {
		if other.claimSession(filepath.Join(w.binding.Workspace, "other.jsonl"), id[:8]) ||
			w.b.reserveExport(other, id[:8]) || w.b.reserveDelete(other, id[:8]) ||
			w.b.reserveExport(w, id) || w.claimSession(w.binding.Session, id) || w.beginSessionRestore(id) {
			t.Fatalf("ordinary claim operation bypassed migration reservation for %s", id)
		}
	}
	assertMigrationIdentitiesReserved(t, w)
}

func TestMigrationClaimsSurviveCloseReleaseAndTeardown(t *testing.T) {
	for _, action := range []string{"release", "close", "teardown"} {
		t.Run(action, func(t *testing.T) {
			w := migrationClaimWorker(t, true)
			source := w.binding
			w.sessionRuntimeActive.Store(true)
			w.sessionRuntimeStarting.Store(true)
			switch action {
			case "release":
				w.releaseSession()
			case "close":
				if !w.closeLogicalSession() {
					t.Fatal("locked runtime could not close")
				}
			case "teardown":
				w.teardownWorker(true)
			}
			assertMigrationIdentitiesReserved(t, w)
			if w.sessionRuntimeActive.Load() || w.sessionRuntimeStarting.Load() {
				t.Fatal("closed migration retained live runtime flags")
			}
			current, err := w.b.db.Binding(source.Bot, source.Chat, source.Thread)
			if err != nil || current != source || w.binding != source {
				t.Fatalf("closing migration changed source: %+v, %v", current, err)
			}
			if _, err := w.b.db.Migration(source.Bot, source.Chat, source.Thread); err != nil {
				t.Fatalf("closing migration removed its durable record: %v", err)
			}
		})
	}
}

func TestMigrationCandidateReservationRejectsConflictsWithoutDroppingSource(t *testing.T) {
	for _, conflict := range []string{"prefix", "path", "export", "delete"} {
		t.Run(conflict, func(t *testing.T) {
			b := &Bridge{}
			w, other := &worker{b: b}, &worker{b: b}
			w.migration = &store.Migration{ID: "migration", Source: store.Binding{Session: "source", SessionID: migrationClaimsSourceID}}
			if !w.restoreMigrationClaims() {
				t.Fatal("source reservation failed")
			}
			switch conflict {
			case "prefix":
				b.sessionClaims["other"] = sessionClaim{owner: other, id: migrationClaimsCandidateID[:8]}
			case "path":
				b.sessionClaims["candidate"] = sessionClaim{owner: other, id: "33333333"}
			case "export":
				b.exportClaims = map[*worker]string{other: migrationClaimsCandidateID[:8]}
			case "delete":
				b.deleteClaims = map[*worker]string{other: migrationClaimsCandidateID[:8]}
			}
			if w.reserveMigrationCandidate("candidate", migrationClaimsCandidateID) {
				t.Fatal("candidate reservation bypassed an existing operation")
			}
			claim, exists := b.sessionClaims["source"]
			if !exists || claim.owner != w || !claim.migration || claim.id != migrationClaimsSourceID || w.sessionID != migrationClaimsSourceID {
				t.Fatal("failed candidate reservation changed source claim")
			}
		})
	}
}

func TestMigrationFinishClaimsKeepsOnlyDurablyResolvedTarget(t *testing.T) {
	for _, old := range []bool{false, true} {
		name := "candidate"
		if old {
			name = "source"
		}
		t.Run(name, func(t *testing.T) {
			w := migrationClaimWorker(t, true)
			if w.finishMigrationClaims("unreserved", "33333333") || len(w.b.sessionClaims) != 2 {
				t.Fatal("unreserved target discarded migration claims")
			}
			target := w.binding
			target.Generation++
			if !old {
				target.Session, target.SessionID = w.migration.CandidateFile, w.migration.CandidateID
			}
			if err := w.b.db.ResolveMigration(*w.migration, target, "迁移恢复完成"); err != nil {
				t.Fatal(err)
			}
			if !w.finishMigrationClaims(target.Session, target.SessionID) {
				t.Fatal("resolved target could not retain its claim before actor lock was cleared")
			}
			w.migration = nil
			w.binding = target
			claim := w.b.sessionClaims[target.Session]
			if len(w.b.sessionClaims) != 1 || claim.owner != w || claim.migration || claim.id != target.SessionID || w.claimedSession != target.Session || w.sessionID != target.SessionID {
				t.Fatal("migration completion did not retain exactly the selected ordinary claim")
			}
			other := &worker{b: w.b}
			if other.claimSession("other", target.SessionID) {
				t.Fatal("selected target lost exclusive ownership")
			}
			w.releaseSession()
			if w.b.sessionInUse(target.SessionID) {
				t.Fatal("resolved target still has a durable migration reservation")
			}
		})
	}
}

func TestPersistentMigrationClaimsProtectInactiveIdentitiesWithoutWorker(t *testing.T) {
	w := migrationClaimWorker(t, false)
	w.b.sessionClaims = nil
	w.migration = nil
	other := &worker{b: w.b, key: target{chat: -10, thread: 22}}
	for _, id := range []string{migrationClaimsSourceID, migrationClaimsCandidateID} {
		busy, elsewhere := w.b.sessionDeleteStatus(other, id[:8])
		if !busy || !elsewhere || !w.b.sessionInUseByOther(other, id[:8]) ||
			w.b.reserveExport(other, id) || w.b.reserveDelete(other, id) || other.claimSession("other", id) {
			t.Fatalf("durable inactive migration identity %s was not protected", id)
		}
		available, err := other.sessionDeleteAvailable(id[:8], w.binding.Workspace)
		if err != nil || available {
			t.Fatalf("durable migration deletion availability for %s = %t, %v", id, available, err)
		}
	}
	if !w.b.migrationBindingLocked(99, -10, 11) {
		t.Fatal("closed binding did not retain its durable migration lock")
	}
}

func TestMigrationLockedWorkerRefusesForgetResumeDeleteAndExport(t *testing.T) {
	for _, running := range []bool{false, true} {
		name := "closed"
		if running {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			w := migrationClaimWorker(t, running)
			source := w.binding
			requester := &worker{ctx: w.ctx, forgetResults: make(chan bindingForgetResult, 1)}
			w.forgetBinding(bindingForgetRequest{generation: source.Generation, source: requester})
			result := <-requester.forgetResults
			if result.ok || result.status != i18n.Msg.MigrationUnresolvedCannotDeleteBinding || w.canForgetBinding() {
				t.Fatalf("locked binding forget result = %+v", result)
			}
			w.touchBinding()
			w.requestResumeList(7)
			w.requestExportList(7, "session")
			w.requestDirectExport(7, "session", migrationClaimsSourceID)
			w.beginExport(omp.SessionSummary{ID: migrationClaimsSourceID, CWD: source.Workspace}, "session", source.Workspace, source.Generation)
			w.beginSessionDelete(confirmation{generation: source.Generation, sessions: []omp.SessionSummary{{ID: migrationClaimsSourceID, CWD: source.Workspace}}, workspace: source.Workspace})
			if w.client != nil || w.resumeCancel != nil || w.exportCancel != nil || w.deleteCancel != nil || len(w.b.slots) != 0 {
				t.Fatal("locked operation launched a native process or background operation")
			}
			current, err := w.b.db.Binding(source.Bot, source.Chat, source.Thread)
			if err != nil || current != source || w.binding != source {
				t.Fatalf("locked operations modified saved source: %+v, %v", current, err)
			}
			assertMigrationIdentitiesReserved(t, w)
		})
	}
}

func TestMigrationBindingDeleteCallbackRefusesClosedSource(t *testing.T) {
	w := migrationClaimWorker(t, false)
	observer := testWorker(t, &worker{b: w.b, key: target{chat: -10, thread: 22}, ctx: w.ctx, confirms: map[string]confirmation{}})
	c := confirmation{action: "binding_delete", options: []string{"Binding", "Binding+Session", "Cancel"}, messageID: 1, deleteBot: 99, deleteChat: -10, deleteThread: 11, deleteGeneration: w.binding.Generation}
	q := &telegram.CallbackQuery{ID: "migration-delete", Message: &telegram.Message{MessageID: 1}}
	for _, choice := range []string{"0", "1"} {
		if observer.bindingCallback(w.ctx, q, "token", choice, c) != callbackDone {
			t.Fatal("locked binding callback was not consumed")
		}
	}
	if observer.forgetPending || !w.b.migrationBindingLocked(99, -10, 11) {
		t.Fatal("closed migrating binding was forgotten")
	}
	if _, err := w.b.db.Binding(99, -10, 11); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationRecoveryReservesClosedAndRunningSourceWithoutLaunching(t *testing.T) {
	for _, running := range []bool{false, true} {
		name := "closed"
		if running {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			db, err := store.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			source := store.Binding{Bot: 99, Chat: -10, Thread: 11, Workspace: t.TempDir(), Session: "/missing/source.jsonl", SessionID: migrationClaimsSourceID, Generation: 2, Running: running}
			prepareClaimTestMigration(t, db, source, true)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx, cancel := context.WithCancel(context.Background())
			b := testBridge(t, &Bridge{db: db, bot: telegram.User{ID: 99}, tg: newTestTelegram(t), cfg: config.Config{AllowedChats: []int64{-10}, QueueCapacity: 4, OMP: "/must-not-start/omp"}, slots: make(chan struct{}, 1), fatal: make(chan error, 1)})
			workers := make(map[target]*worker)
			err = b.restoreWorkers(ctx, workers)
			cancel()
			b.wg.Wait()
			if err != nil {
				t.Fatal(err)
			}
			w := workers[target{chat: -10, thread: 11}]
			if w == nil || !w.migrationLocked() || w.client != nil || w.restoring || len(b.slots) != 0 {
				t.Fatal("migration recovery omitted its locked actor or started ordinary runtime restoration")
			}
			assertMigrationIdentitiesReserved(t, w)
			current, err := db.Binding(99, -10, 11)
			if err != nil || current != source {
				t.Fatalf("recovery changed source binding: %+v, %v", current, err)
			}
			var state string
			if err := db.DB.QueryRow("SELECT state FROM inbox WHERE id=9001").Scan(&state); err != nil || state != "pending" {
				t.Fatalf("recovery replayed/settled migration input: %q, %v", state, err)
			}
		})
	}
}

func TestMigrationRecoveryReservesIdentitiesBeforeOrdinaryBindingClaims(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workspace := t.TempDir()
	ordinary := store.Binding{Bot: 99, Chat: -10, Thread: 11, Workspace: workspace, Session: filepath.Join(workspace, "candidate.jsonl"), SessionID: migrationClaimsCandidateID, Generation: 1, Running: true}
	if err := db.Save(ordinary); err != nil {
		t.Fatal(err)
	}
	source := store.Binding{Bot: 99, Chat: -10, Thread: 33, Workspace: workspace, Session: filepath.Join(workspace, "source.jsonl"), SessionID: migrationClaimsSourceID, Generation: 3}
	prepareClaimTestMigration(t, db, source, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := testBridge(t, &Bridge{db: db, bot: telegram.User{ID: 99}, cfg: config.Config{AllowedChats: []int64{-10}, QueueCapacity: 4}})
	workers := make(map[target]*worker)
	if err := b.restoreWorkers(ctx, workers); err == nil {
		t.Fatal("ordinary binding was allowed to claim a migration candidate")
	}
	w := workers[target{chat: -10, thread: 33}]
	if w == nil || !w.migrationLocked() || workers[target{chat: -10, thread: 11}] != nil {
		t.Fatal("ordinary binding acquired ownership before migration reservations")
	}
	assertMigrationIdentitiesReserved(t, w)
	if _, err := db.Migration(99, -10, 33); err != nil {
		t.Fatalf("claim conflict discarded migration: %v", err)
	}
}

func unobservedMigrationClaimWorker(t *testing.T) *worker {
	t.Helper()
	w, _, _ := setupWorkspaceWorker(t)
	workspace := t.TempDir()
	source := store.Binding{Bot: 99, Chat: -10, Thread: 11, Workspace: workspace, Session: filepath.Join(workspace, "source.jsonl"), SessionID: migrationClaimsSourceID, Generation: 4, Running: true}
	migration := prepareClaimTestMigration(t, w.b.db, source, false)
	w.binding, w.migration = source, &migration
	if !w.restoreMigrationClaims() {
		t.Fatal("source reservation failed")
	}
	return w
}

func TestMigrationCandidateObservationRejectsConflictsBeforePersistence(t *testing.T) {
	for _, identity := range []string{"source", "candidate"} {
		for _, conflict := range []string{"claim", "path", "export", "delete"} {
			t.Run(identity+"/"+conflict, func(t *testing.T) {
				w := unobservedMigrationClaimWorker(t)
				file, id := filepath.Join(w.binding.Workspace, "candidate.jsonl"), migrationClaimsCandidateID
				conflictFile, conflictID := file, id
				if identity == "source" {
					conflictFile, conflictID = w.binding.Session, w.binding.SessionID
				}
				other := &worker{b: w.b, key: target{chat: -10, thread: 22}}
				switch conflict {
				case "claim":
					w.b.sessionClaims["other"] = sessionClaim{owner: other, id: conflictID[:8]}
				case "path":
					w.b.sessionClaims[conflictFile] = sessionClaim{owner: other, id: "33333333"}
				case "export":
					w.b.exportClaims = map[*worker]string{other: conflictID[:8]}
				case "delete":
					w.b.deleteClaims = map[*worker]string{other: conflictID[:8]}
				}
				before := len(w.b.sessionClaims)
				if _, err := w.b.observeMigrationCandidate(w, w.migration.ID, file, id); err == nil {
					t.Fatal("observation accepted a conflicting identity")
				}
				current, err := w.b.db.Migration(99, -10, 11)
				if err != nil || current.CandidateID != "" || current.CandidateFile != "" || len(w.b.sessionClaims) != before {
					t.Fatalf("rejected observation poisoned durable recovery: %+v, %v", current, err)
				}
			})
		}
	}
}

func TestMigrationCandidateObservationRejectsOtherPendingMigration(t *testing.T) {
	w := unobservedMigrationClaimWorker(t)
	file := filepath.Join(w.binding.Workspace, "candidate.jsonl")
	other := store.Binding{Bot: 99, Chat: -10, Thread: 22, Workspace: w.binding.Workspace, Session: file, SessionID: migrationClaimsCandidateID, Generation: 1}
	if err := w.b.db.Save(other); err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(9002, []byte(`{"update_id":9002}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.PrepareMigration(store.Migration{ID: "other-pending-migration", Source: other, InboxID: 9002, Phase: "prepared"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.b.observeMigrationCandidate(w, w.migration.ID, file, migrationClaimsCandidateID); err == nil {
		t.Fatal("observation ignored another pending migration without a live worker")
	}
	current, err := w.b.db.Migration(99, -10, 11)
	if err != nil || current.CandidateID != "" || len(w.b.sessionClaims) != 1 {
		t.Fatalf("conflicting pending migration poisoned source recovery: %+v, %v", current, err)
	}
}

func TestMigrationCandidateObservationPersistsAfterCancelAndTeardown(t *testing.T) {
	w := unobservedMigrationClaimWorker(t)
	migrationID := w.migration.ID
	current := *w.migration
	current.ErrorKind = "cancelled"
	if err := w.b.db.UpdateMigration(current, current.Phase); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(w.binding.Workspace, "candidate.jsonl")
	ready, result := make(chan struct{}), make(chan error, 1)
	go func() {
		<-ready
		observed, err := w.b.observeMigrationCandidate(w, migrationID, file, migrationClaimsCandidateID)
		if err == nil && observed.ErrorKind != "cancelled" {
			err = errors.New("candidate observation discarded current failure reason")
		}
		result <- err
	}()
	w.cancel()
	w.teardownWorker(true)
	w.migration = nil
	w.claimedSession, w.sessionID = "actor-path", "actor-id"
	close(ready)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if w.migration != nil || w.claimedSession != "actor-path" || w.sessionID != "actor-id" {
		t.Fatal("background observation mutated actor-owned migration/session state")
	}
	persisted, err := w.b.db.Migration(99, -10, 11)
	if err != nil || persisted.CandidateFile != file || persisted.CandidateID != migrationClaimsCandidateID || persisted.ErrorKind != "cancelled" {
		t.Fatalf("shutdown lost observed candidate: %+v, %v", persisted, err)
	}
	assertMigrationIdentitiesReserved(t, w)
}

func TestMigrationCandidateObservationIgnoresConcurrentActorState(t *testing.T) {
	w := unobservedMigrationClaimWorker(t)
	migrationID := w.migration.ID
	file := filepath.Join(w.binding.Workspace, "candidate.jsonl")
	done := make(chan struct{})
	actorOnly := &store.Migration{ID: "actor-only"}
	go func() {
		defer close(done)
		for range 1000 {
			w.migration = nil
			w.claimedSession, w.sessionID = "actor-path", "actor-id"
			w.migration = actorOnly
			w.claimedSession, w.sessionID = "actor-path-2", "actor-id-2"
		}
	}()
	_, err := w.b.observeMigrationCandidate(w, migrationID, file, migrationClaimsCandidateID)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	assertMigrationIdentitiesReserved(t, w)
}

func TestMigrationCandidateObservationRejectsStaleAndChangedIdentity(t *testing.T) {
	w := unobservedMigrationClaimWorker(t)
	file := filepath.Join(w.binding.Workspace, "candidate.jsonl")
	if _, err := w.b.observeMigrationCandidate(w, "stale-migration", file, migrationClaimsCandidateID); err == nil {
		t.Fatal("stale observation accepted")
	}
	first, err := w.b.observeMigrationCandidate(w, w.migration.ID, file, migrationClaimsCandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.b.observeMigrationCandidate(w, first.ID, file, "33333333"); err == nil {
		t.Fatal("observed candidate changed after persistence")
	}
	again, err := w.b.observeMigrationCandidate(w, first.ID, file, first.CandidateID)
	if err != nil || again != first || len(w.b.sessionClaims) != 2 {
		t.Fatalf("repeated observation changed migration: %+v, %v", again, err)
	}
	assertMigrationIdentitiesReserved(t, w)
}
