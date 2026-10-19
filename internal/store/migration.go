package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrMigrationLocked = errors.New("binding has an unresolved migration")
	ErrStaleMigration  = errors.New("stale migration")
)

// Migration retains the immutable source until an independently verified target
// or an explicit recovery decision is published. It never expires automatically.
type Migration struct {
	ID                          string
	Source                      Binding
	InboxID                     int64
	Phase                       string
	SummaryEntryID, SummaryHash string
	CandidateFile, CandidateID  string
	Provider, Model, Thinking   string
	Fast                        bool
	CreatedAt                   int64
	ErrorKind                   string
}

const migrationSchema = `CREATE TABLE migrations(
 bot INTEGER NOT NULL,chat INTEGER NOT NULL,thread INTEGER NOT NULL,
 id TEXT NOT NULL UNIQUE,inbox_id INTEGER NOT NULL,
 workspace TEXT NOT NULL,session TEXT NOT NULL,session_id TEXT NOT NULL,
 generation INTEGER NOT NULL,last_used_at INTEGER NOT NULL,running INTEGER NOT NULL,interrupted INTEGER NOT NULL,
 phase TEXT NOT NULL CHECK(phase IN ('prepared','handoff','initialize','verify')),
 summary_entry_id TEXT NOT NULL,summary_hash TEXT NOT NULL,
 candidate_file TEXT NOT NULL,candidate_id TEXT NOT NULL,
 provider TEXT NOT NULL,model TEXT NOT NULL,thinking TEXT NOT NULL,fast INTEGER NOT NULL,
 created_at INTEGER NOT NULL,error_kind TEXT NOT NULL,
 PRIMARY KEY(bot,chat,thread));
 CREATE INDEX idx_migrations_source ON migrations(bot,session_id COLLATE NOCASE);
 CREATE INDEX idx_migrations_candidate ON migrations(bot,candidate_id COLLATE NOCASE);`

const migrationColumns = `id,bot,chat,thread,workspace,session,session_id,generation,last_used_at,running,interrupted,inbox_id,phase,summary_entry_id,summary_hash,candidate_file,candidate_id,provider,model,thinking,fast,created_at,error_kind`

func scanMigration(row rowScanner) (Migration, error) {
	var m Migration
	err := row.Scan(&m.ID, &m.Source.Bot, &m.Source.Chat, &m.Source.Thread, &m.Source.Workspace, &m.Source.Session, &m.Source.SessionID, &m.Source.Generation, &m.Source.LastUsedAt, &m.Source.Running, &m.Source.Interrupted, &m.InboxID, &m.Phase, &m.SummaryEntryID, &m.SummaryHash, &m.CandidateFile, &m.CandidateID, &m.Provider, &m.Model, &m.Thinking, &m.Fast, &m.CreatedAt, &m.ErrorKind)
	return m, err
}

func (s *Store) Migration(bot, chat, thread int64) (Migration, error) {
	return scanMigration(s.DB.QueryRow("SELECT "+migrationColumns+" FROM migrations WHERE bot=? AND chat=? AND thread=?", bot, chat, thread))
}

func (s *Store) PendingMigrations(bot int64) ([]Migration, error) {
	rows, err := s.DB.Query("SELECT "+migrationColumns+" FROM migrations WHERE bot=? ORDER BY chat,thread", bot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Migration
	for rows.Next() {
		m, err := scanMigration(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

func migrationGuard(tx *sql.Tx, bot, chat, thread int64) error {
	var locked bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM migrations WHERE bot=? AND chat=? AND thread=?)", bot, chat, thread).Scan(&locked); err != nil {
		return err
	}
	if locked {
		return ErrMigrationLocked
	}
	return nil
}

func migrationPhase(phase string) int {
	switch phase {
	case "prepared":
		return 1
	case "handoff":
		return 2
	case "initialize":
		return 3
	case "verify":
		return 4
	default:
		return 0
	}
}

func migrationSessionGuard(tx *sql.Tx, bot int64, file, id string) error {
	var locked bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM migrations WHERE bot=? AND ((?<>'' AND (session=? OR candidate_file=?)) OR (?<>'' AND (session_id=? COLLATE NOCASE OR candidate_id=? COLLATE NOCASE))))", bot, file, file, file, id, id, id).Scan(&locked); err != nil {
		return err
	}
	if locked {
		return ErrMigrationLocked
	}
	return nil
}

func (s *Store) PrepareMigration(m Migration) error {
	if m.ID == "" || m.Source.Generation <= 0 || m.Source.Workspace == "" || m.Source.Session == "" || m.Source.SessionID == "" || m.InboxID <= 0 || m.Phase != "prepared" || m.SummaryEntryID != "" || m.SummaryHash != "" || m.CandidateFile != "" || m.CandidateID != "" {
		return errors.New("invalid migration")
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = time.Now().Unix()
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := migrationGuard(tx, m.Source.Bot, m.Source.Chat, m.Source.Thread); err != nil {
		return err
	}
	if err := migrationSessionGuard(tx, m.Source.Bot, m.Source.Session, m.Source.SessionID); err != nil {
		return err
	}
	var source Binding
	source, err = scanBinding(tx.QueryRow("SELECT bot,chat,thread,workspace,session,session_id,generation,last_used_at,running,interrupted FROM bindings WHERE bot=? AND chat=? AND thread=?", m.Source.Bot, m.Source.Chat, m.Source.Thread))
	if errors.Is(err, sql.ErrNoRows) || err == nil && source != m.Source {
		return ErrStaleBinding
	}
	if err != nil {
		return err
	}
	var blocked bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM startup_intents WHERE bot=? AND chat=? AND thread=?)", source.Bot, source.Chat, source.Thread).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return errors.New("binding has a pending start")
	}
	var state InboxState
	if err := tx.QueryRow("SELECT state FROM inbox WHERE id=?", m.InboxID).Scan(&state); err != nil {
		return err
	}
	if state != InboxPending && state != InboxSubmitted {
		return ErrInboxStateTransition
	}
	_, err = tx.Exec("INSERT INTO migrations("+migrationColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", m.ID, source.Bot, source.Chat, source.Thread, source.Workspace, source.Session, source.SessionID, source.Generation, source.LastUsedAt, source.Running, source.Interrupted, m.InboxID, m.Phase, m.SummaryEntryID, m.SummaryHash, m.CandidateFile, m.CandidateID, m.Provider, m.Model, m.Thinking, m.Fast, m.CreatedAt, m.ErrorKind)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func sameMigrationSource(a, b Migration) bool {
	return a.ID == b.ID && a.Source == b.Source && a.InboxID == b.InboxID && a.Provider == b.Provider && a.Model == b.Model && a.Thinking == b.Thinking && a.Fast == b.Fast && (a.CreatedAt == b.CreatedAt || b.CreatedAt == 0)
}

func (s *Store) UpdateMigration(m Migration, expectedPhase string) error {
	phase, previous := migrationPhase(m.Phase), migrationPhase(expectedPhase)
	if previous == 0 || phase < previous || phase > previous+1 || (m.CandidateFile == "") != (m.CandidateID == "") || (m.SummaryEntryID == "") != (m.SummaryHash == "") {
		return errors.New("invalid migration update")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanMigration(tx.QueryRow("SELECT "+migrationColumns+" FROM migrations WHERE bot=? AND chat=? AND thread=?", m.Source.Bot, m.Source.Chat, m.Source.Thread))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStaleMigration
	}
	if err != nil {
		return err
	}
	if !sameMigrationSource(current, m) || current.Phase != expectedPhase || current.CandidateFile != "" && (current.CandidateFile != m.CandidateFile || current.CandidateID != m.CandidateID) || current.SummaryEntryID != "" && (current.SummaryEntryID != m.SummaryEntryID || current.SummaryHash != m.SummaryHash) {
		return ErrStaleMigration
	}
	if current.CandidateFile == "" && m.CandidateFile != "" {
		// Discovery must not overwrite a newer cancellation/failure reason.
		m.ErrorKind = current.ErrorKind
		if err := migrationSessionGuard(tx, m.Source.Bot, m.CandidateFile, m.CandidateID); err != nil {
			return err
		}
	}
	if err := migrationSourceCAS(tx, current); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE migrations SET phase=?,summary_entry_id=?,summary_hash=?,candidate_file=?,candidate_id=?,error_kind=? WHERE id=?", m.Phase, m.SummaryEntryID, m.SummaryHash, m.CandidateFile, m.CandidateID, m.ErrorKind, m.ID); err != nil {
		return err
	}
	if m.ErrorKind != "" {
		if _, err := tx.Exec("UPDATE inbox SET state='uncertain',updated_at=? WHERE id=? AND state='submitted'", time.Now().Unix(), current.InboxID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FailMigration settles only the operation outcome, preserving concurrently observed evidence.
func (s *Store) FailMigration(id, kind string) (Migration, error) {
	var m Migration
	if id == "" || kind == "" {
		return m, ErrStaleMigration
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return m, err
	}
	defer tx.Rollback()
	m, err = scanMigration(tx.QueryRow("SELECT "+migrationColumns+" FROM migrations WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrStaleMigration
	}
	if err != nil {
		return m, err
	}
	if err := migrationSourceCAS(tx, m); err != nil {
		return m, err
	}
	if _, err := tx.Exec("UPDATE migrations SET error_kind=? WHERE id=?", kind, id); err != nil {
		return m, err
	}
	if _, err := tx.Exec("UPDATE inbox SET state='uncertain',updated_at=? WHERE id=? AND state='submitted'", time.Now().Unix(), m.InboxID); err != nil {
		return m, err
	}
	if err := tx.Commit(); err != nil {
		return m, err
	}
	m.ErrorKind = kind
	return m, nil
}

// Interrupted is independent crash/stop bookkeeping, not migration identity.
func migrationSourceCAS(tx *sql.Tx, m Migration) error {
	current, err := scanBinding(tx.QueryRow("SELECT bot,chat,thread,workspace,session,session_id,generation,last_used_at,running,interrupted FROM bindings WHERE bot=? AND chat=? AND thread=?", m.Source.Bot, m.Source.Chat, m.Source.Thread))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStaleBinding
	}
	if err != nil {
		return err
	}
	current.Interrupted = m.Source.Interrupted
	if current != m.Source {
		return ErrStaleBinding
	}
	return nil
}

func (s *Store) CommitMigration(m Migration, target Binding, notice string) error {
	return s.publishMigration(m, target, notice, false)
}

func (s *Store) ResolveMigration(m Migration, target Binding, notice string) error {
	return s.publishMigration(m, target, notice, true)
}

func (s *Store) publishMigration(m Migration, target Binding, notice string, recovery bool) error {
	if target.Bot != m.Source.Bot || target.Chat != m.Source.Chat || target.Thread != m.Source.Thread || target.Workspace != m.Source.Workspace || target.Generation != m.Source.Generation+1 || notice == "" {
		return errors.New("invalid migration target")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanMigration(tx.QueryRow("SELECT "+migrationColumns+" FROM migrations WHERE bot=? AND chat=? AND thread=?", m.Source.Bot, m.Source.Chat, m.Source.Thread))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStaleMigration
	}
	if err != nil {
		return err
	}
	if !sameMigrationSource(current, m) || current.Phase != m.Phase || current.CandidateFile != m.CandidateFile || current.CandidateID != m.CandidateID || current.SummaryEntryID != m.SummaryEntryID || current.SummaryHash != m.SummaryHash || current.ErrorKind != m.ErrorKind {
		return ErrStaleMigration
	}
	candidate := current.CandidateFile != "" && target.Session == current.CandidateFile && target.SessionID == current.CandidateID
	source := target.Session == current.Source.Session && target.SessionID == current.Source.SessionID
	if !recovery && (current.Phase != "verify" || current.SummaryEntryID == "" || current.SummaryHash == "" || !candidate) || recovery && !candidate && !source {
		return errors.New("migration target is not recorded or verified")
	}
	if err := migrationSourceCAS(tx, current); err != nil {
		return err
	}
	var replyTo int64
	if err := tx.QueryRow("SELECT reply_to FROM inbox WHERE id=? AND state IN ('pending','submitted','uncertain')", current.InboxID).Scan(&replyTo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInboxStateTransition
		}
		return err
	}
	if _, err := tx.Exec("INSERT INTO history(bot,chat,thread,workspace,session,generation) SELECT bot,chat,thread,workspace,session,generation FROM bindings WHERE bot=? AND chat=? AND thread=?", target.Bot, target.Chat, target.Thread); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE bindings SET workspace=?,session=?,session_id=?,generation=?,last_used_at=?,running=?,interrupted=? WHERE bot=? AND chat=? AND thread=?", target.Workspace, target.Session, target.SessionID, target.Generation, target.LastUsedAt, target.Running, target.Interrupted, target.Bot, target.Chat, target.Thread); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM migrations WHERE id=?", current.ID); err != nil {
		return err
	}
	now := time.Now().Unix()
	if _, err := tx.Exec("INSERT INTO outbox(inbox_id,chat,thread,text,state,reply_to,created_at,updated_at,next_attempt_at) VALUES(?,?,?,?,'pending',?,?,?,?)", current.InboxID, target.Chat, target.Thread, notice, replyTo, now, now, now); err != nil {
		return fmt.Errorf("enqueue migration notice: %w", err)
	}
	if _, err := tx.Exec("UPDATE inbox SET state='done',updated_at=? WHERE id=?", now, current.InboxID); err != nil {
		return err
	}
	return tx.Commit()
}
