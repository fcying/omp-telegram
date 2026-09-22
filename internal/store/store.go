package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 9
const messageCleanupBatchSize = 1000

// InboxState identifies the durable lifecycle state of an incoming update.
type InboxState string

const (
	InboxPending   InboxState = "pending"
	InboxSubmitted InboxState = "submitted"
	InboxDone      InboxState = "done"
	InboxCancelled InboxState = "cancelled"
	InboxFailed    InboxState = "failed"
	InboxIgnored   InboxState = "ignored"
	InboxUncertain InboxState = "uncertain"
)

// OutboxState identifies the durable lifecycle state of a Telegram delivery.
type OutboxState string

const (
	OutboxPending   OutboxState = "pending"
	OutboxSending   OutboxState = "sending"
	OutboxDone      OutboxState = "done"
	OutboxFailed    OutboxState = "failed"
	OutboxUncertain OutboxState = "uncertain"
	OutboxCancelled OutboxState = "cancelled"
	OutboxSent      OutboxState = "sent"
)

var (
	ErrInboxStateTransition  = errors.New("inbox state transition rejected")
	ErrOutboxStateTransition = errors.New("outbox state transition rejected")
	ErrStaleBinding          = errors.New("stale binding")
)

type Store struct{ DB *sql.DB }
type Binding struct {
	Bot, Chat, Thread             int64
	Workspace, Session, SessionID string
	Generation                    int64
	LastUsedAt                    int64
	Running                       bool
	Interrupted                   bool
}
type BindingListEntry struct {
	Binding     *Binding
	Intent      *StartIntent
	SessionName string // Resolved native metadata; never persisted by the bridge.
}
type bindingKey struct{ bot, chat, thread int64 }
type StartIntent struct {
	Bot, Chat, Thread  int64
	Kind               string
	Workspace, Session string
	Generation         int64
}
type Input struct {
	ID  int64
	Raw json.RawMessage
}
type Output struct {
	ID, InboxID, Chat, Thread, ReplyTo int64
	Text                               string
	Kind, Path, Name                   string
}

type ProgressMessage struct {
	InboxID, Chat, MessageID int64
}

type CleanupResult struct {
	Inbox, Outbox   int64
	AttachmentPaths []string
}
type UncertainCounts struct {
	Inbox  int
	Outbox int
}

func (s *Store) QuickCheck(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := s.DB.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return quickCheckError(err)
	}
	defer rows.Close()
	var result string
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return quickCheckError(err)
		}
		return errors.New("sqlite quick check unavailable")
	}
	if err := rows.Scan(&result); err != nil {
		return quickCheckError(err)
	}
	if result != "ok" {
		return errors.New("sqlite quick check failed")
	}
	if rows.Next() {
		return errors.New("sqlite quick check unavailable")
	}
	if err := rows.Err(); err != nil {
		return quickCheckError(err)
	}
	return nil
}

func quickCheckError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New("sqlite quick check unavailable")
}

func (s *Store) UncertainCounts(ctx context.Context) (UncertainCounts, error) {
	var counts UncertainCounts
	if err := ctx.Err(); err != nil {
		return counts, err
	}
	err := s.DB.QueryRowContext(ctx, `
SELECT
 (SELECT COUNT(*) FROM inbox WHERE state='uncertain'),
 (SELECT COUNT(*) FROM outbox WHERE state='uncertain')`).Scan(&counts.Inbox, &counts.Outbox)
	return counts, err
}

func Open(dir string) (*Store, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	// Existing directories may be the user's project root; preserve their permissions.
	path := filepath.Join(dir, "omp-telegram.db")
	if info, e := os.Lstat(path); e == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("database must be a regular file")
	} else if e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return nil, e
	}
	if e = f.Close(); e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if e != nil {
		return nil, e
	}
	// Keep one connection for a simple, ordered SQLite lifecycle. Context-aware diagnostic queries may still cause a bounded stall here; a second connection would not remove SQLite file-lock contention without measurements.
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA busy_timeout=5000;`)
	if e == nil {
		e = initialize(db)
	}
	if e != nil {
		db.Close()
		return nil, e
	}
	return &Store{db}, nil
}

func initialize(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 0 || version > schemaVersion {
		return fmt.Errorf("unsupported database schema version %d; this binary supports version %d", version, schemaVersion)
	}
	if version == 0 {
		var populated bool
		if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%')").Scan(&populated); err != nil {
			return err
		}
		if populated {
			return fmt.Errorf("unversioned database is unsupported; back it up and use a fresh data directory")
		}
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return err
	}
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if version == 0 {
		_, e = tx.Exec(`
 CREATE TABLE meta (key TEXT PRIMARY KEY,value INTEGER NOT NULL);
 CREATE TABLE bindings(bot INTEGER,chat INTEGER,thread INTEGER,workspace TEXT NOT NULL,session TEXT NOT NULL,session_id TEXT NOT NULL DEFAULT '',generation INTEGER NOT NULL,last_used_at INTEGER NOT NULL DEFAULT 0,running INTEGER NOT NULL DEFAULT 0,interrupted INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(bot,chat,thread));
 CREATE TABLE startup_intents(bot INTEGER,chat INTEGER,thread INTEGER,kind TEXT NOT NULL CHECK(kind IN ('new','resume')),workspace TEXT NOT NULL,session TEXT NOT NULL,generation INTEGER NOT NULL,PRIMARY KEY(bot,chat,thread));
 CREATE TABLE history(bot INTEGER,chat INTEGER,thread INTEGER,workspace TEXT,session TEXT,generation INTEGER);
 CREATE TABLE session_favorites(bot INTEGER,chat INTEGER,thread INTEGER,workspace TEXT NOT NULL,session_id TEXT NOT NULL,PRIMARY KEY(bot,chat,thread,workspace,session_id));
 CREATE TABLE inbox(id INTEGER PRIMARY KEY,raw BLOB NOT NULL,state TEXT NOT NULL,reply_to INTEGER NOT NULL DEFAULT 0,progress_message_id INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL DEFAULT 0,updated_at INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE outbox(id INTEGER PRIMARY KEY AUTOINCREMENT,inbox_id INTEGER NOT NULL DEFAULT 0,chat INTEGER,thread INTEGER,text TEXT NOT NULL,state TEXT NOT NULL,reply_to INTEGER NOT NULL DEFAULT 0,kind TEXT NOT NULL DEFAULT 'text',path TEXT NOT NULL DEFAULT '',name TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL DEFAULT 0,updated_at INTEGER NOT NULL DEFAULT 0);
 CREATE INDEX idx_inbox_state ON inbox(state,id);
 CREATE INDEX idx_outbox_state ON outbox(state,id);
 CREATE INDEX idx_outbox_inbox_state ON outbox(inbox_id,state);`)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); e != nil {
			return e
		}
	}
	if version == 1 {
		_, e = tx.Exec("CREATE TABLE startup_intents(bot INTEGER,chat INTEGER,thread INTEGER,kind TEXT NOT NULL CHECK(kind IN ('new','resume')),workspace TEXT NOT NULL,session TEXT NOT NULL,generation INTEGER NOT NULL,PRIMARY KEY(bot,chat,thread))")
		if e != nil {
			return e
		}
		version = 2
	}
	if version == 2 {
		_, e = tx.Exec("ALTER TABLE bindings ADD COLUMN interrupted INTEGER NOT NULL DEFAULT 0")
		if e != nil {
			return e
		}
		version = 3
	}
	if version == 3 {
		for _, query := range []string{
			"ALTER TABLE inbox ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE inbox ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE outbox ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE outbox ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0",
		} {
			if _, e = tx.Exec(query); e != nil {
				return e
			}
		}
		now := time.Now().Unix()
		if _, e = tx.Exec("UPDATE inbox SET created_at=?,updated_at=? WHERE created_at=0 OR updated_at=0", now, now); e != nil {
			return e
		}
		if _, e = tx.Exec("UPDATE outbox SET created_at=?,updated_at=? WHERE created_at=0 OR updated_at=0", now, now); e != nil {
			return e
		}
		version = 4
	}
	if version == 4 {
		for _, query := range []string{
			"ALTER TABLE inbox ADD COLUMN reply_to INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE outbox ADD COLUMN reply_to INTEGER NOT NULL DEFAULT 0",
		} {
			if _, e = tx.Exec(query); e != nil {
				return e
			}
		}
		version = 5
	}
	if version == 5 {
		if _, e = tx.Exec("ALTER TABLE bindings ADD COLUMN session_id TEXT NOT NULL DEFAULT ''"); e != nil {
			return e
		}
		version = 6
	}
	if version == 6 {
		for _, query := range []string{
			"ALTER TABLE inbox ADD COLUMN progress_message_id INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE outbox ADD COLUMN inbox_id INTEGER NOT NULL DEFAULT 0",
			"CREATE INDEX idx_outbox_inbox_state ON outbox(inbox_id,state)",
		} {
			if _, e = tx.Exec(query); e != nil {
				return e
			}
		}
		version = 7
	}
	if version == 7 {
		if _, e = tx.Exec("ALTER TABLE bindings ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0"); e != nil {
			return e
		}
		version = 8
	}
	if version == 8 {
		_, e = tx.Exec("CREATE TABLE session_favorites(bot INTEGER,chat INTEGER,thread INTEGER,workspace TEXT NOT NULL,session_id TEXT NOT NULL,PRIMARY KEY(bot,chat,thread,workspace,session_id))")
		if e != nil {
			return e
		}
		version = 9
	}
	if e = backfillSessionIDs(tx); e != nil {
		return e
	}
	if version != 0 {
		if _, e = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); e != nil {
			return e
		}
	}
	now := time.Now().Unix()
	if _, e = tx.Exec(`UPDATE inbox SET state='uncertain',updated_at=? WHERE state='submitted';
 UPDATE outbox SET state='uncertain',updated_at=? WHERE state='sending';`, now, now); e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) Offset() (int64, error) {
	var n int64
	e := s.DB.QueryRow("SELECT value FROM meta WHERE key='offset'").Scan(&n)
	if e == sql.ErrNoRows {
		return 0, nil
	}
	return n, e
}
func (s *Store) CheckBot(id int64) error {
	if _, e := s.DB.Exec("INSERT INTO meta(key,value) VALUES('bot',?) ON CONFLICT(key) DO NOTHING", id); e != nil {
		return e
	}
	var old int64
	e := s.DB.QueryRow("SELECT value FROM meta WHERE key='bot'").Scan(&old)
	if e == nil && old != id {
		return fmt.Errorf("data directory belongs to another bot")
	}
	return e
}
func (s *Store) Accept(id int64, raw []byte) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, e = tx.Exec("INSERT INTO inbox(id,raw,state,created_at,updated_at) VALUES(?,?, 'pending',?,?) ON CONFLICT(id) DO NOTHING", id, raw, now, now); e != nil {
		return e
	}
	if _, e = tx.Exec("INSERT INTO meta VALUES('offset',?) ON CONFLICT(key) DO UPDATE SET value=MAX(value,excluded.value)", id+1); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Pending() ([]Input, error) {
	rows, e := s.DB.Query("SELECT id,raw FROM inbox WHERE state='pending' ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Input
	for rows.Next() {
		var v Input
		if e = rows.Scan(&v.ID, &v.Raw); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) Mark(id int64, state InboxState) error {
	var expected string
	switch state {
	case InboxSubmitted:
		expected = "'pending'"
	case InboxDone:
		expected = "'pending','submitted'"
	case InboxCancelled:
		expected = "'pending','submitted'"
	case InboxFailed:
		expected = "'pending'"
	case InboxIgnored:
		expected = "'pending'"
	case InboxUncertain:
		expected = "'submitted'"
	default:
		return fmt.Errorf("%w: %s", ErrInboxStateTransition, state)
	}
	result, err := s.DB.Exec("UPDATE inbox SET state=?,updated_at=? WHERE id=? AND state IN ("+expected+")", string(state), time.Now().Unix(), id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: %s", ErrInboxStateTransition, state)
	}
	return nil
}

// Submit records a root task's originating Telegram message before OMP accepts it.
func (s *Store) Submit(id, replyTo int64) error {
	if replyTo < 0 {
		return errors.New("invalid reply target")
	}
	result, err := s.DB.Exec("UPDATE inbox SET state='submitted',reply_to=?,updated_at=? WHERE id=? AND state='pending'", replyTo, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrInboxStateTransition
	}
	return nil
}
func (s *Store) Binding(bot, chat, thread int64) (Binding, error) {
	b := Binding{Bot: bot, Chat: chat, Thread: thread}
	err := s.DB.QueryRow("SELECT workspace,session,session_id,generation,last_used_at,running,interrupted FROM bindings WHERE bot=? AND chat=? AND thread=?", bot, chat, thread).Scan(&b.Workspace, &b.Session, &b.SessionID, &b.Generation, &b.LastUsedAt, &b.Running, &b.Interrupted)
	return b, err
}

func scanBinding(scanner interface{ Scan(...any) error }) (Binding, error) {
	var b Binding
	err := scanner.Scan(&b.Bot, &b.Chat, &b.Thread, &b.Workspace, &b.Session, &b.SessionID, &b.Generation, &b.LastUsedAt, &b.Running, &b.Interrupted)
	return b, err
}

func (s *Store) PinnedSessions(bot, chat, thread int64, workspace string) (map[string]struct{}, error) {
	rows, err := s.DB.Query("SELECT session_id FROM session_favorites WHERE bot=? AND chat=? AND thread=? AND workspace=?", bot, chat, thread, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pinned := make(map[string]struct{})
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return nil, err
		}
		pinned[strings.ToLower(sessionID)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return pinned, nil
}

func (s *Store) SetPinnedSession(bot, chat, thread int64, workspace, sessionID string, pinned bool) error {
	if workspace == "" || sessionID == "" {
		return errors.New("invalid pinned session")
	}
	if pinned {
		_, err := s.DB.Exec("INSERT INTO session_favorites(bot,chat,thread,workspace,session_id) VALUES(?,?,?,?,?) ON CONFLICT(bot,chat,thread,workspace,session_id) DO NOTHING", bot, chat, thread, workspace, strings.ToLower(sessionID))
		return err
	}
	_, err := s.DB.Exec("DELETE FROM session_favorites WHERE bot=? AND chat=? AND thread=? AND workspace=? AND session_id=?", bot, chat, thread, workspace, strings.ToLower(sessionID))
	return err
}

// SetPinnedSessionIfGeneration changes a favorite only while the conversation
// still has the picker generation that produced the action.
func (s *Store) SetPinnedSessionIfGeneration(bot, chat, thread, generation int64, workspace, sessionID string, pinned bool) (bool, error) {
	if workspace == "" || sessionID == "" {
		return false, errors.New("invalid pinned session")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var current int64
	if err = tx.QueryRow("SELECT generation FROM bindings WHERE bot=? AND chat=? AND thread=?", bot, chat, thread).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if current != generation {
		return false, nil
	}
	if pinned {
		_, err = tx.Exec("INSERT INTO session_favorites(bot,chat,thread,workspace,session_id) VALUES(?,?,?,?,?) ON CONFLICT(bot,chat,thread,workspace,session_id) DO NOTHING", bot, chat, thread, workspace, strings.ToLower(sessionID))
	} else {
		_, err = tx.Exec("DELETE FROM session_favorites WHERE bot=? AND chat=? AND thread=? AND workspace=? AND session_id=?", bot, chat, thread, workspace, strings.ToLower(sessionID))
	}
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) Save(b Binding) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO history(bot,chat,thread,workspace,session,generation) SELECT bot,chat,thread,workspace,session,generation FROM bindings WHERE bot=? AND chat=? AND thread=?", b.Bot, b.Chat, b.Thread); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO bindings(bot,chat,thread,workspace,session,session_id,generation,last_used_at,running,interrupted) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(bot,chat,thread) DO UPDATE SET workspace=excluded.workspace,session=excluded.session,session_id=excluded.session_id,generation=excluded.generation,last_used_at=excluded.last_used_at,running=excluded.running,interrupted=excluded.interrupted", b.Bot, b.Chat, b.Thread, b.Workspace, b.Session, b.SessionID, b.Generation, b.LastUsedAt, b.Running, b.Interrupted); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BindingsForChat(bot, chat int64) ([]BindingListEntry, error) {
	rows, err := s.DB.Query("SELECT bot,chat,thread,workspace,session,session_id,generation,last_used_at,running,interrupted FROM bindings WHERE bot=? AND chat=? ORDER BY thread", bot, chat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make(map[bindingKey]BindingListEntry)
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		copy := b
		entries[bindingKey{bot: b.Bot, chat: b.Chat, thread: b.Thread}] = BindingListEntry{Binding: &copy}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	intentRows, err := s.DB.Query("SELECT bot,chat,thread,kind,workspace,session,generation FROM startup_intents WHERE bot=? AND chat=? ORDER BY thread", bot, chat)
	if err != nil {
		return nil, err
	}
	defer intentRows.Close()
	for intentRows.Next() {
		var intent StartIntent
		if err = intentRows.Scan(&intent.Bot, &intent.Chat, &intent.Thread, &intent.Kind, &intent.Workspace, &intent.Session, &intent.Generation); err != nil {
			return nil, err
		}
		copy := intent
		key := bindingKey{bot: intent.Bot, chat: intent.Chat, thread: intent.Thread}
		entry := entries[key]
		entry.Intent = &copy
		entries[key] = entry
	}
	if err = intentRows.Err(); err != nil {
		return nil, err
	}
	keys := make([]bindingKey, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(left, right bindingKey) int {
		if left.thread < right.thread {
			return -1
		}
		if left.thread > right.thread {
			return 1
		}
		return 0
	})
	result := make([]BindingListEntry, 0, len(keys))
	for _, key := range keys {
		result = append(result, entries[key])
	}
	return result, nil
}

func (s *Store) TouchBinding(bot, chat, thread, generation int64) (bool, error) {
	result, err := s.DB.Exec("UPDATE bindings SET last_used_at=? WHERE bot=? AND chat=? AND thread=? AND generation=?", time.Now().Unix(), bot, chat, thread, generation)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (s *Store) DeleteClosedBinding(bot, chat, thread, generation int64) (bool, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec("DELETE FROM bindings WHERE bot=? AND chat=? AND thread=? AND generation=? AND running=0 AND NOT EXISTS (SELECT 1 FROM startup_intents WHERE bot=? AND chat=? AND thread=?)", bot, chat, thread, generation, bot, chat, thread)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed != 1 {
		return false, nil
	}
	if _, err = tx.Exec("DELETE FROM history WHERE bot=? AND chat=? AND thread=?", bot, chat, thread); err != nil {
		return false, err
	}
	if _, err = tx.Exec("DELETE FROM session_favorites WHERE bot=? AND chat=? AND thread=?", bot, chat, thread); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) RunningBindings(bot int64) ([]Binding, error) {
	rows, err := s.DB.Query("SELECT bot,chat,thread,workspace,session,session_id,generation,last_used_at,running,interrupted FROM bindings WHERE bot=? AND running=1 ORDER BY chat,thread", bot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bindings []Binding
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

func backfillSessionIDs(tx *sql.Tx) error {
	rows, err := tx.Query("SELECT bot,chat,thread,session FROM bindings WHERE session_id='' ")
	if err != nil {
		return err
	}
	type bindingKey struct{ bot, chat, thread int64 }
	ids := make(map[bindingKey]string)
	for rows.Next() {
		var key bindingKey
		var session string
		if err = rows.Scan(&key.bot, &key.chat, &key.thread, &session); err != nil {
			rows.Close()
			return err
		}
		if id := sessionIDFromPath(session); id != "" {
			ids[key] = id
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for key, id := range ids {
		if _, err = tx.Exec("UPDATE bindings SET session_id=? WHERE bot=? AND chat=? AND thread=?", id, key.bot, key.chat, key.thread); err != nil {
			return err
		}
	}
	return nil
}

func sessionIDFromPath(session string) string {
	base := strings.TrimSuffix(filepath.Base(session), ".jsonl")
	if id := validSessionID(base); id != "" {
		return id
	}
	if separator := strings.LastIndexByte(base, '_'); separator >= 0 {
		return validSessionID(base[separator+1:])
	}
	return ""
}

func validSessionID(id string) string {
	if len(id) < 8 || len(id) > 64 || strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") {
		return ""
	}
	for _, r := range id {
		if r != '-' && !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return ""
		}
	}
	return id
}

func (s *Store) SetRunning(b Binding, running bool) error {
	result, err := s.DB.Exec("UPDATE bindings SET running=? WHERE bot=? AND chat=? AND thread=? AND generation=?", running, b.Bot, b.Chat, b.Thread, b.Generation)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrStaleBinding
	}
	return nil
}

func (s *Store) SetInterrupted(b Binding, interrupted bool) error {
	result, err := s.DB.Exec("UPDATE bindings SET interrupted=? WHERE bot=? AND chat=? AND thread=? AND generation=?", interrupted, b.Bot, b.Chat, b.Thread, b.Generation)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrStaleBinding
	}
	return nil
}

func (s *Store) CleanupMessages(ctx context.Context, cutoff int64) (CleanupResult, error) {
	var result CleanupResult
	for {
		count, err := s.cleanupInboxBatch(ctx, cutoff)
		if err != nil {
			return result, err
		}
		result.Inbox += count
		if count < messageCleanupBatchSize {
			break
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
	}
	for {
		count, paths, err := s.cleanupOutboxBatch(ctx, cutoff)
		if err != nil {
			return result, err
		}
		result.Outbox += count
		result.AttachmentPaths = append(result.AttachmentPaths, paths...)
		if count < messageCleanupBatchSize {
			return result, nil
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
	}
}

func (s *Store) OutboxAttachmentPaths(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT path FROM outbox WHERE kind IN ('photo','document') AND path != ''")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err = rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

func (s *Store) cleanupInboxBatch(ctx context.Context, cutoff int64) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE inbox AS candidate
SET progress_message_id=0
WHERE candidate.state IN ('done','cancelled','ignored','failed','uncertain')
  AND candidate.progress_message_id>0
  AND candidate.updated_at<?
  AND NOT EXISTS (
		SELECT 1
		FROM outbox AS active
		WHERE active.inbox_id=candidate.id
		  AND active.state IN ('pending','sending')
	)`, cutoff); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM inbox WHERE id IN (SELECT id FROM inbox WHERE state IN ('done','cancelled','ignored','failed','uncertain') AND progress_message_id=0 AND updated_at<? ORDER BY id LIMIT ?)", cutoff, messageCleanupBatchSize)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return count, tx.Commit()
}

func (s *Store) cleanupOutboxBatch(ctx context.Context, cutoff int64) (int64, []string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT path FROM outbox WHERE id IN (SELECT candidate.id FROM outbox AS candidate WHERE candidate.state IN ('done','sent','failed','uncertain','cancelled') AND candidate.updated_at<? AND NOT EXISTS (SELECT 1 FROM inbox AS progress WHERE progress.id=candidate.inbox_id AND progress.progress_message_id>0) ORDER BY candidate.id LIMIT ?) AND kind IN ('photo','document')", cutoff, messageCleanupBatchSize)
	if err != nil {
		return 0, nil, err
	}
	var paths []string
	for rows.Next() {
		var path string
		if err = rows.Scan(&path); err != nil {
			rows.Close()
			return 0, nil, err
		}
		paths = append(paths, path)
	}
	if err = rows.Close(); err != nil {
		return 0, nil, err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM outbox WHERE id IN (SELECT candidate.id FROM outbox AS candidate WHERE candidate.state IN ('done','sent','failed','uncertain','cancelled') AND candidate.updated_at<? AND NOT EXISTS (SELECT 1 FROM inbox AS progress WHERE progress.id=candidate.inbox_id AND progress.progress_message_id>0) ORDER BY candidate.id LIMIT ?)", cutoff, messageCleanupBatchSize)
	if err != nil {
		return 0, nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, nil, err
	}
	if err = tx.Commit(); err != nil {
		return 0, nil, err
	}
	return count, paths, nil
}
func (s *Store) PrepareStart(previous Binding, intent StartIntent) error {
	if previous.Generation < 0 || (intent.Kind != "new" && intent.Kind != "resume") || intent.Generation != previous.Generation+1 || intent.Bot != previous.Bot || intent.Chat != previous.Chat || intent.Thread != previous.Thread {
		return errors.New("invalid startup intent")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if previous.Running {
		result, err := tx.Exec("UPDATE bindings SET running=0 WHERE bot=? AND chat=? AND thread=? AND generation=? AND running=1", previous.Bot, previous.Chat, previous.Thread, previous.Generation)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return errors.New("startup binding changed")
		}
	}
	where := "NOT EXISTS (SELECT 1 FROM bindings WHERE bot=? AND chat=? AND thread=?)"
	args := []any{intent.Bot, intent.Chat, intent.Thread, intent.Kind, intent.Workspace, intent.Session, intent.Generation, previous.Bot, previous.Chat, previous.Thread}
	if previous.Generation > 0 {
		where = "EXISTS (SELECT 1 FROM bindings WHERE bot=? AND chat=? AND thread=? AND generation=?)"
		args = append(args, previous.Generation)
	}
	query := "INSERT INTO startup_intents(bot,chat,thread,kind,workspace,session,generation) SELECT ?,?,?,?,?,?,? WHERE " + where
	result, err := tx.Exec(query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("startup binding changed")
	}
	return tx.Commit()
}
func (s *Store) CommitStart(b Binding) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var generation int64
	if err = tx.QueryRow("SELECT generation FROM startup_intents WHERE bot=? AND chat=? AND thread=?", b.Bot, b.Chat, b.Thread).Scan(&generation); err != nil {
		return err
	}
	if generation != b.Generation {
		return errors.New("startup intent changed")
	}
	lastUsedAt := b.LastUsedAt
	if err = tx.QueryRow("SELECT last_used_at FROM bindings WHERE bot=? AND chat=? AND thread=?", b.Bot, b.Chat, b.Thread).Scan(&lastUsedAt); err != nil && err != sql.ErrNoRows {
		return err
	}
	if _, err = tx.Exec("INSERT INTO history(bot,chat,thread,workspace,session,generation) SELECT bot,chat,thread,workspace,session,generation FROM bindings WHERE bot=? AND chat=? AND thread=?", b.Bot, b.Chat, b.Thread); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO bindings(bot,chat,thread,workspace,session,session_id,generation,last_used_at,running,interrupted) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(bot,chat,thread) DO UPDATE SET workspace=excluded.workspace,session=excluded.session,session_id=excluded.session_id,generation=excluded.generation,last_used_at=excluded.last_used_at,running=excluded.running,interrupted=excluded.interrupted", b.Bot, b.Chat, b.Thread, b.Workspace, b.Session, b.SessionID, b.Generation, lastUsedAt, b.Running, b.Interrupted); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM startup_intents WHERE bot=? AND chat=? AND thread=? AND generation=?", b.Bot, b.Chat, b.Thread, b.Generation); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CancelStart(intent StartIntent) error {
	_, err := s.DB.Exec("DELETE FROM startup_intents WHERE bot=? AND chat=? AND thread=? AND generation=?", intent.Bot, intent.Chat, intent.Thread, intent.Generation)
	return err
}

func (s *Store) PendingStarts(bot int64) ([]StartIntent, error) {
	rows, err := s.DB.Query("SELECT bot,chat,thread,kind,workspace,session,generation FROM startup_intents WHERE bot=? ORDER BY chat,thread", bot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var intents []StartIntent
	for rows.Next() {
		var intent StartIntent
		if err = rows.Scan(&intent.Bot, &intent.Chat, &intent.Thread, &intent.Kind, &intent.Workspace, &intent.Session, &intent.Generation); err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

func (s *Store) Enqueue(chat, thread int64, text string) error {
	now := time.Now().Unix()
	_, e := s.DB.Exec("INSERT INTO outbox(chat,thread,text,state,created_at,updated_at) VALUES(?,?,?,'pending',?,?)", chat, thread, text, now, now)
	return e
}

// CompleteInboxWithReplies commits the complete final result and input completion together.
// A failed transaction leaves the submitted input and outbox unchanged.
func (s *Store) CompleteInboxWithReplies(ctx context.Context, id, chat, thread int64, replies []string) error {
	return s.completeInboxWithReplies(ctx, id, chat, thread, InboxDone, replies)
}

// CompleteInboxUncertainWithReplies commits a terminal result that cannot be confirmed.
// A failed transaction leaves the submitted input and outbox unchanged.
func (s *Store) CompleteInboxUncertainWithReplies(ctx context.Context, id, chat, thread int64, replies []string) error {
	return s.completeInboxWithReplies(ctx, id, chat, thread, InboxUncertain, replies)
}

// CompleteInboxCancelledWithReplies commits an aborted terminal result and its replies together.
// A failed transaction leaves the submitted input and outbox unchanged.
func (s *Store) CompleteInboxCancelledWithReplies(ctx context.Context, id, chat, thread int64, replies []string) error {
	return s.completeInboxWithReplies(ctx, id, chat, thread, InboxCancelled, replies)
}

func (s *Store) completeInboxWithReplies(ctx context.Context, id, chat, thread int64, state InboxState, replies []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var replyTo int64
	if err := tx.QueryRowContext(ctx, "SELECT reply_to FROM inbox WHERE id=? AND state='submitted'", id).Scan(&replyTo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("input is not submitted")
		}
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE inbox SET state=?,updated_at=? WHERE id=? AND state='submitted'", string(state), now, id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("input is not submitted")
	}
	for _, reply := range replies {
		if _, err := tx.ExecContext(ctx, "INSERT INTO outbox(inbox_id,chat,thread,text,state,reply_to,created_at,updated_at) VALUES(?,?,?,?,'pending',?,?,?)", id, chat, thread, reply, replyTo, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) EnqueueAttachment(chat, thread int64, kind, path, name, caption string) error {
	if kind != "photo" && kind != "document" {
		return fmt.Errorf("unsupported attachment kind %q", kind)
	}
	now := time.Now().Unix()
	_, e := s.DB.Exec("INSERT INTO outbox(chat,thread,text,state,kind,path,name,created_at,updated_at) VALUES(?,?,?,'pending',?,?,?,?,?)", chat, thread, caption, kind, path, name, now, now)
	return e
}

func (s *Store) NextOutput() (Output, error) {
	var o Output
	err := s.DB.QueryRow("SELECT id,inbox_id,chat,thread,text,reply_to,kind,path,name FROM outbox WHERE state='pending' ORDER BY id LIMIT 1").Scan(&o.ID, &o.InboxID, &o.Chat, &o.Thread, &o.Text, &o.ReplyTo, &o.Kind, &o.Path, &o.Name)
	return o, err
}

func (s *Store) MarkOutput(id int64, state OutboxState) error {
	var expected string
	switch state {
	case OutboxSending:
		expected = "'pending'"
	case OutboxDone, OutboxFailed, OutboxUncertain, OutboxCancelled, OutboxSent:
		expected = "'sending'"
	default:
		return fmt.Errorf("%w: %s", ErrOutboxStateTransition, state)
	}
	result, err := s.DB.Exec("UPDATE outbox SET state=?,updated_at=? WHERE id=? AND state IN ("+expected+")", string(state), time.Now().Unix(), id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: %s", ErrOutboxStateTransition, state)
	}
	return nil
}

func (s *Store) SetProgressMessage(inboxID, messageID int64) error {
	if inboxID <= 0 || messageID <= 0 {
		return errors.New("invalid progress message identity")
	}
	result, err := s.DB.Exec("UPDATE inbox SET progress_message_id=? WHERE id=?", messageID, inboxID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) CompletedProgressMessage(inboxID int64) (ProgressMessage, bool, error) {
	var message ProgressMessage
	err := s.DB.QueryRow(`
SELECT i.id,MIN(o.chat),i.progress_message_id
FROM inbox i
JOIN outbox o ON o.inbox_id=i.id
WHERE i.id=? AND i.state IN ('done','cancelled','failed','ignored','uncertain') AND i.progress_message_id>0
  AND NOT EXISTS (SELECT 1 FROM outbox pending WHERE pending.inbox_id=i.id AND pending.state<>'done')
GROUP BY i.id,i.progress_message_id`, inboxID).Scan(&message.InboxID, &message.Chat, &message.MessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return ProgressMessage{}, false, nil
	}
	return message, err == nil, err
}

func (s *Store) CompletedProgressMessages() ([]ProgressMessage, error) {
	rows, err := s.DB.Query(`
SELECT i.id,MIN(o.chat),i.progress_message_id
FROM inbox i
JOIN outbox o ON o.inbox_id=i.id
WHERE i.state IN ('done','cancelled','failed','ignored','uncertain') AND i.progress_message_id>0
  AND NOT EXISTS (SELECT 1 FROM outbox pending WHERE pending.inbox_id=i.id AND pending.state<>'done')
GROUP BY i.id,i.progress_message_id ORDER BY i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []ProgressMessage
	for rows.Next() {
		var message ProgressMessage
		if err := rows.Scan(&message.InboxID, &message.Chat, &message.MessageID); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) ClearProgressMessage(inboxID, messageID int64) error {
	_, err := s.DB.Exec("UPDATE inbox SET progress_message_id=0 WHERE id=? AND progress_message_id=?", inboxID, messageID)
	return err
}

func (s *Store) Uncertain() (int, error) {
	var n int
	err := s.DB.QueryRow("SELECT (SELECT COUNT(*) FROM inbox WHERE state='uncertain')+(SELECT COUNT(*) FROM outbox WHERE state='uncertain')").Scan(&n)
	return n, err
}
