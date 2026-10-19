package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func migrationFixture(t *testing.T, s *Store) Migration {
	t.Helper()
	source := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: t.TempDir(), SessionID: "source-session", Generation: 7, LastUsedAt: 41, Running: true}
	source.Session = filepath.Join(source.Workspace, "source.jsonl")
	requireStoreOK(t, os.WriteFile(source.Session, []byte("immutable source journal\n"), 0600))
	requireStoreOK(t, s.Save(source))
	requireStoreOK(t, s.Accept(100, []byte(`{"update_id":100}`)))
	requireStoreOK(t, s.Submit(100, 23))
	return Migration{ID: "migration-one", Source: source, InboxID: 100, Phase: "prepared", Provider: "provider", Model: "model", Thinking: "auto", Fast: true, CreatedAt: 1}
}

func prepareVerifiedMigration(t *testing.T, s *Store, m Migration) Migration {
	t.Helper()
	requireStoreOK(t, s.PrepareMigration(m))
	m.Phase = "handoff"
	requireStoreOK(t, s.UpdateMigration(m, "prepared"))
	m.Phase = "initialize"
	m.SummaryEntryID, m.SummaryHash = "summary-entry", "summary-sha256"
	requireStoreOK(t, s.UpdateMigration(m, "handoff"))
	m.CandidateFile, m.CandidateID = filepath.Join(m.Source.Workspace, "candidate.jsonl"), "candidate-session"
	requireStoreOK(t, s.UpdateMigration(m, "initialize"))
	m.Phase = "verify"
	requireStoreOK(t, s.UpdateMigration(m, "initialize"))
	return m
}

func migrationTarget(m Migration) Binding {
	target := m.Source
	target.Session, target.SessionID = m.CandidateFile, m.CandidateID
	target.Generation++
	target.Interrupted = false
	return target
}

func assertMigrationUnchanged(t *testing.T, s *Store, m Migration) {
	t.Helper()
	got, err := s.Migration(m.Source.Bot, m.Source.Chat, m.Source.Thread)
	requireStoreOK(t, err)
	if got != m {
		t.Fatalf("migration = %#v, want %#v", got, m)
	}
	binding, err := s.Binding(m.Source.Bot, m.Source.Chat, m.Source.Thread)
	requireStoreOK(t, err)
	if binding != m.Source {
		t.Fatalf("binding = %#v, want unchanged source %#v", binding, m.Source)
	}
	var history, outbox int
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM history").Scan(&history))
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&outbox))
	if history != 0 || outbox != 0 {
		t.Fatalf("history/outbox = %d/%d, want 0/0", history, outbox)
	}
}

func TestMigrationSchema18Upgrade(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	m := migrationFixture(t, s)
	// Remove later additions to construct a populated schema-18 database.
	_, err := s.DB.Exec("DROP TABLE interface_language; DROP TABLE migrations; PRAGMA user_version=18")
	requireStoreOK(t, err)
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	var version int
	requireStoreOK(t, s.DB.QueryRow("PRAGMA user_version").Scan(&version))
	if version != schemaVersion {
		t.Fatalf("upgraded version = %d, want %d", version, schemaVersion)
	}
	binding, err := s.Binding(1, 2, 3)
	requireStoreOK(t, err)
	if binding != m.Source {
		t.Fatalf("upgrade changed binding: %#v", binding)
	}
	m.InboxID = 101
	requireStoreOK(t, s.Accept(m.InboxID, []byte(`{"update_id":101}`)))
	requireStoreOK(t, s.PrepareMigration(m))
	assertMigrationUnchanged(t, s, m)
}

func TestPrepareMigrationRejectsStaleCompleteSource(t *testing.T) {
	changes := map[string]func(*Binding){
		"workspace":   func(b *Binding) { b.Workspace += "/other" },
		"session":     func(b *Binding) { b.Session += ".other" },
		"session id":  func(b *Binding) { b.SessionID += "-other" },
		"generation":  func(b *Binding) { b.Generation++ },
		"last used":   func(b *Binding) { b.LastUsedAt++ },
		"running":     func(b *Binding) { b.Running = false },
		"interrupted": func(b *Binding) { b.Interrupted = true },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			m := migrationFixture(t, s)
			change(&m.Source)
			if err := s.PrepareMigration(m); !errors.Is(err, ErrStaleBinding) {
				t.Fatalf("stale prepare error = %v", err)
			}
			if _, err := s.Migration(1, 2, 3); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("rejected migration exists: %v", err)
			}
		})
	}
}

func TestMigrationPrepareAndMutationGuards(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(map[bool]string{true: "running", false: "closed"}[running], func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			m := migrationFixture(t, s)
			m.Source.Running = running
			requireStoreOK(t, s.SetRunning(m.Source, running))
			requireStoreOK(t, s.PrepareMigration(m))
			if err := s.PrepareMigration(m); !errors.Is(err, ErrMigrationLocked) {
				t.Fatalf("duplicate prepare error = %v", err)
			}
			if err := s.Save(m.Source); !errors.Is(err, ErrMigrationLocked) {
				t.Fatalf("save error = %v", err)
			}
			if err := s.SetRunning(m.Source, !running); err == nil {
				t.Fatal("running changed during migration")
			}
			intent := StartIntent{Bot: 1, Chat: 2, Thread: 3, Kind: "new", Workspace: m.Source.Workspace, Generation: m.Source.Generation + 1}
			if err := s.PrepareStart(m.Source, intent); !errors.Is(err, ErrMigrationLocked) {
				t.Fatalf("prepare start error = %v", err)
			}
			if removed, err := s.DeleteClosedBinding(1, 2, 3, m.Source.Generation); err != nil || removed {
				t.Fatalf("delete closed = %v, %v", removed, err)
			}
			if _, err := s.DeleteSessionReferences(1, m.Source.SessionID); !errors.Is(err, ErrMigrationLocked) {
				t.Fatalf("delete source error = %v", err)
			}
			if touched, err := s.TouchBinding(1, 2, 3, m.Source.Generation, 99); err != nil || touched {
				t.Fatalf("touch source = %v, %v", touched, err)
			}
			assertMigrationUnchanged(t, s, m)
		})
	}
}

func TestMigrationRejectsPendingStart(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	m := migrationFixture(t, s)
	intent := StartIntent{Bot: 1, Chat: 2, Thread: 3, Kind: "new", Workspace: m.Source.Workspace, Generation: m.Source.Generation + 1}
	requireStoreOK(t, s.PrepareStart(m.Source, intent))
	m.Source.Running = false
	if err := s.PrepareMigration(m); err == nil {
		t.Fatal("prepared migration over pending start")
	}
}

func TestMigrationImmutableRecordAndCandidateProtection(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	m := prepareVerifiedMigration(t, s, migrationFixture(t, s))
	changes := map[string]func(*Migration){
		"source":            func(m *Migration) { m.Source.Session += ".other" },
		"settings":          func(m *Migration) { m.Model += "-other" },
		"inbox":             func(m *Migration) { m.InboxID++ },
		"id":                func(m *Migration) { m.ID += "-other" },
		"candidate file":    func(m *Migration) { m.CandidateFile += ".other" },
		"candidate id":      func(m *Migration) { m.CandidateID += "-other" },
		"candidate cleared": func(m *Migration) { m.CandidateFile, m.CandidateID = "", "" },
		"summary":           func(m *Migration) { m.SummaryHash += "-other" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			changed := m
			change(&changed)
			if err := s.UpdateMigration(changed, "verify"); err == nil {
				t.Fatal("immutable migration record changed")
			}
		})
	}
	if err := s.UpdateMigration(m, "initialize"); !errors.Is(err, ErrStaleMigration) {
		t.Fatalf("stale phase error = %v", err)
	}
	if _, err := s.DeleteSessionReferences(1, m.CandidateID); !errors.Is(err, ErrMigrationLocked) {
		t.Fatalf("delete candidate error = %v", err)
	}
	other := migrationTarget(m)
	other.Thread++
	if err := s.Save(other); !errors.Is(err, ErrMigrationLocked) {
		t.Fatalf("candidate stealing error = %v", err)
	}
	m.ErrorKind = "verification_failed"
	requireStoreOK(t, s.UpdateMigration(m, "verify"))
	assertMigrationUnchanged(t, s, m)
}

func TestMigrationAtomicCommitAndDuplicate(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	m := prepareVerifiedMigration(t, s, migrationFixture(t, s))
	target := migrationTarget(m)
	requireStoreOK(t, s.CommitMigration(m, target, "迁移完成"))
	binding, err := s.Binding(1, 2, 3)
	requireStoreOK(t, err)
	if binding != target {
		t.Fatalf("published binding = %#v, want %#v", binding, target)
	}
	if _, err := s.Migration(1, 2, 3); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("committed migration remains: %v", err)
	}
	var history Binding
	requireStoreOK(t, s.DB.QueryRow("SELECT bot,chat,thread,workspace,session,generation FROM history").Scan(&history.Bot, &history.Chat, &history.Thread, &history.Workspace, &history.Session, &history.Generation))
	if history.Bot != 1 || history.Chat != 2 || history.Thread != 3 || history.Workspace != m.Source.Workspace || history.Session != m.Source.Session || history.Generation != m.Source.Generation {
		t.Fatalf("archived source = %#v", history)
	}
	output, err := s.NextOutput()
	requireStoreOK(t, err)
	if output.InboxID != 100 || output.Chat != 2 || output.Thread != 3 || output.ReplyTo != 23 || output.Text != "迁移完成" {
		t.Fatalf("notification = %#v", output)
	}
	var state InboxState
	requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=100").Scan(&state))
	if state != InboxDone {
		t.Fatalf("inbox state = %s", state)
	}
	if err := s.CommitMigration(m, target, "重复"); !errors.Is(err, ErrStaleMigration) {
		t.Fatalf("duplicate commit error = %v", err)
	}
	var historyCount, outputCount int
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM history").Scan(&historyCount))
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&outputCount))
	if historyCount != 1 || outputCount != 1 {
		t.Fatalf("duplicate commit added history/output: %d/%d", historyCount, outputCount)
	}
	journal, err := os.ReadFile(m.Source.Session)
	requireStoreOK(t, err)
	if string(journal) != "immutable source journal\n" {
		t.Fatalf("source journal changed: %q", journal)
	}
}

func TestMigrationNotificationFailureRollsBack(t *testing.T) {
	for _, resolve := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "resolve"}[resolve], func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			m := prepareVerifiedMigration(t, s, migrationFixture(t, s))
			_, err := s.DB.Exec("CREATE TRIGGER reject_notice BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT,'notification failure'); END")
			requireStoreOK(t, err)
			publish := s.CommitMigration
			if resolve {
				publish = s.ResolveMigration
			}
			if err := publish(m, migrationTarget(m), "完成"); err == nil {
				t.Fatal("notification failure was ignored")
			}
			assertMigrationUnchanged(t, s, m)
			var state InboxState
			requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=100").Scan(&state))
			if state != InboxSubmitted {
				t.Fatalf("failed transaction settled inbox: %s", state)
			}
			_, err = s.DB.Exec("DROP TRIGGER reject_notice")
			requireStoreOK(t, err)
			requireStoreOK(t, publish(m, migrationTarget(m), "完成"))
		})
	}
}

func TestMigrationRejectsStaleBindingAtCommit(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	m := prepareVerifiedMigration(t, s, migrationFixture(t, s))
	_, err := s.DB.Exec("UPDATE bindings SET generation=generation+1 WHERE bot=1 AND chat=2 AND thread=3")
	requireStoreOK(t, err)
	if err := s.CommitMigration(m, migrationTarget(m), "完成"); !errors.Is(err, ErrStaleBinding) {
		t.Fatalf("stale binding commit error = %v", err)
	}
	var count int
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&count))
	if count != 0 {
		t.Fatal("stale commit enqueued a notice")
	}
}

func TestMigrationRequiresKnownTargetAndVerifyPhase(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	m := migrationFixture(t, s)
	requireStoreOK(t, s.PrepareMigration(m))
	target := m.Source
	target.Generation++
	target.Session, target.SessionID = "unknown.jsonl", "unknown"
	if err := s.CommitMigration(m, target, "完成"); err == nil {
		t.Fatal("unverified commit accepted")
	}
	if err := s.ResolveMigration(m, target, "恢复"); err == nil {
		t.Fatal("unknown recovery target accepted")
	}
	assertMigrationUnchanged(t, s, m)
	m.CandidateFile, m.CandidateID = target.Session, target.SessionID
	requireStoreOK(t, s.UpdateMigration(m, "prepared"))
	if err := s.CommitMigration(m, target, "完成"); err == nil {
		t.Fatal("recorded but unverified candidate committed")
	}
	requireStoreOK(t, s.ResolveMigration(m, target, "恢复"))
}

func TestMigrationRecoverySourceAndRetentionAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	m := migrationFixture(t, s)
	requireStoreOK(t, s.PrepareMigration(m))
	m.ErrorKind = "interrupted"
	requireStoreOK(t, s.UpdateMigration(m, "prepared"))
	requireStoreOK(t, s.SetInterrupted(m.Source, true))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	_, err := s.DB.Exec("UPDATE inbox SET updated_at=1 WHERE id=100")
	requireStoreOK(t, err)
	_, err = s.CleanupMessages(context.Background(), time.Now().Add(time.Hour).Unix())
	requireStoreOK(t, err)
	_, err = s.CleanupExpiredStarts(context.Background(), time.Now().Add(time.Hour).Unix())
	requireStoreOK(t, err)
	pending, err := s.PendingMigrations(1)
	requireStoreOK(t, err)
	if len(pending) != 1 || pending[0] != m {
		t.Fatalf("retention/restart lost lock: %#v", pending)
	}
	otherBot, err := s.PendingMigrations(99)
	requireStoreOK(t, err)
	if len(otherBot) != 0 {
		t.Fatalf("migrations leaked between bots: %#v", otherBot)
	}
	var state InboxState
	requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=100").Scan(&state))
	if state != InboxUncertain {
		t.Fatalf("restart/retention inbox state = %s", state)
	}
	target := m.Source
	target.Generation++
	target.Running, target.Interrupted = false, false
	requireStoreOK(t, s.ResolveMigration(m, target, "恢复原会话"))
	binding, err := s.Binding(1, 2, 3)
	requireStoreOK(t, err)
	if binding != target {
		t.Fatalf("source recovery published %#v, want %#v", binding, target)
	}
}

func TestMigrationRejectsChangedCommitTarget(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	m := prepareVerifiedMigration(t, s, migrationFixture(t, s))
	changes := map[string]func(*Binding){
		"generation":   func(b *Binding) { b.Generation++ },
		"workspace":    func(b *Binding) { b.Workspace += "/other" },
		"file":         func(b *Binding) { b.Session += ".other" },
		"id":           func(b *Binding) { b.SessionID += "-other" },
		"conversation": func(b *Binding) { b.Thread++ },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			target := migrationTarget(m)
			change(&target)
			if err := s.CommitMigration(m, target, "完成"); err == nil {
				t.Fatal("changed target committed")
			}
			assertMigrationUnchanged(t, s, m)
		})
	}
	stale := m
	stale.ID += "-stale"
	if err := s.CommitMigration(stale, migrationTarget(m), "完成"); !errors.Is(err, ErrStaleMigration) {
		t.Fatalf("stale migration commit error = %v", err)
	}
	assertMigrationUnchanged(t, s, m)
}

func TestMigrationInboxCompletionFailureRollsBack(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	m := prepareVerifiedMigration(t, s, migrationFixture(t, s))
	_, err := s.DB.Exec("CREATE TRIGGER reject_migration_completion BEFORE UPDATE OF state ON inbox WHEN NEW.state='done' BEGIN SELECT RAISE(ABORT,'completion failure'); END")
	requireStoreOK(t, err)
	if err := s.CommitMigration(m, migrationTarget(m), "完成"); err == nil {
		t.Fatal("inbox completion failure was ignored")
	}
	assertMigrationUnchanged(t, s, m)
	var state InboxState
	requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=100").Scan(&state))
	if state != InboxSubmitted {
		t.Fatalf("failed completion changed inbox: %s", state)
	}
}
