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
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 4
const messageCleanupBatchSize = 1000

type Store struct{ DB *sql.DB }
type Binding struct {
	Bot, Chat, Thread  int64
	Workspace, Session string
	Generation         int64
	Running            bool
	Interrupted        bool
}
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
	ID, Chat, Thread int64
	Text             string
	Kind, Path, Name string
}

type CleanupResult struct {
	Inbox, Outbox   int64
	AttachmentPaths []string
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
 CREATE TABLE bindings(bot INTEGER,chat INTEGER,thread INTEGER,workspace TEXT NOT NULL,session TEXT NOT NULL,generation INTEGER NOT NULL,running INTEGER NOT NULL DEFAULT 0,interrupted INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(bot,chat,thread));
 CREATE TABLE startup_intents(bot INTEGER,chat INTEGER,thread INTEGER,kind TEXT NOT NULL CHECK(kind IN ('new','resume')),workspace TEXT NOT NULL,session TEXT NOT NULL,generation INTEGER NOT NULL,PRIMARY KEY(bot,chat,thread));
 CREATE TABLE history(bot INTEGER,chat INTEGER,thread INTEGER,workspace TEXT,session TEXT,generation INTEGER);
 CREATE TABLE inbox(id INTEGER PRIMARY KEY,raw BLOB NOT NULL,state TEXT NOT NULL,created_at INTEGER NOT NULL DEFAULT 0,updated_at INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE outbox(id INTEGER PRIMARY KEY AUTOINCREMENT,chat INTEGER,thread INTEGER,text TEXT NOT NULL,state TEXT NOT NULL,kind TEXT NOT NULL DEFAULT 'text',path TEXT NOT NULL DEFAULT '',name TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL DEFAULT 0,updated_at INTEGER NOT NULL DEFAULT 0);
 CREATE INDEX idx_inbox_state ON inbox(state,id);
 CREATE INDEX idx_outbox_state ON outbox(state,id);`)
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
func (s *Store) Mark(id int64, state string) error {
	_, e := s.DB.Exec("UPDATE inbox SET state=?,updated_at=? WHERE id=?", state, time.Now().Unix(), id)
	return e
}
func (s *Store) Binding(bot, chat, thread int64) (Binding, error) {
	b := Binding{Bot: bot, Chat: chat, Thread: thread}
	e := s.DB.QueryRow("SELECT workspace,session,generation,running,interrupted FROM bindings WHERE bot=? AND chat=? AND thread=?", bot, chat, thread).Scan(&b.Workspace, &b.Session, &b.Generation, &b.Running, &b.Interrupted)
	return b, e
}
func (s *Store) Save(b Binding) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	_, e = tx.Exec("INSERT INTO history(bot,chat,thread,workspace,session,generation) SELECT bot,chat,thread,workspace,session,generation FROM bindings WHERE bot=? AND chat=? AND thread=?", b.Bot, b.Chat, b.Thread)
	if e != nil {
		return e
	}
	_, e = tx.Exec("INSERT INTO bindings(bot,chat,thread,workspace,session,generation,running,interrupted) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(bot,chat,thread) DO UPDATE SET workspace=excluded.workspace,session=excluded.session,generation=excluded.generation,running=excluded.running,interrupted=excluded.interrupted", b.Bot, b.Chat, b.Thread, b.Workspace, b.Session, b.Generation, b.Running, b.Interrupted)
	if e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Store) RunningBindings(bot int64) ([]Binding, error) {
	rows, e := s.DB.Query("SELECT bot,chat,thread,workspace,session,generation,running,interrupted FROM bindings WHERE bot=? AND running=1 ORDER BY chat,thread", bot)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var bindings []Binding
	for rows.Next() {
		var b Binding
		if e = rows.Scan(&b.Bot, &b.Chat, &b.Thread, &b.Workspace, &b.Session, &b.Generation, &b.Running, &b.Interrupted); e != nil {
			return nil, e
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

func (s *Store) SetRunning(b Binding, running bool) error {
	_, e := s.DB.Exec("UPDATE bindings SET running=? WHERE bot=? AND chat=? AND thread=? AND generation=?", running, b.Bot, b.Chat, b.Thread, b.Generation)
	return e
}

func (s *Store) SetInterrupted(b Binding, interrupted bool) error {
	_, err := s.DB.Exec("UPDATE bindings SET interrupted=? WHERE bot=? AND chat=? AND thread=? AND generation=?", interrupted, b.Bot, b.Chat, b.Thread, b.Generation)
	return err
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
	result, err := tx.ExecContext(ctx, "DELETE FROM inbox WHERE id IN (SELECT id FROM inbox WHERE state IN ('done','cancelled','ignored','failed','uncertain') AND updated_at<? ORDER BY id LIMIT ?)", cutoff, messageCleanupBatchSize)
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
	rows, err := tx.QueryContext(ctx, "SELECT path FROM outbox WHERE id IN (SELECT id FROM outbox WHERE state IN ('done','sent','failed','uncertain','cancelled') AND updated_at<? ORDER BY id LIMIT ?) AND kind IN ('photo','document')", cutoff, messageCleanupBatchSize)
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
	result, err := tx.ExecContext(ctx, "DELETE FROM outbox WHERE id IN (SELECT id FROM outbox WHERE state IN ('done','sent','failed','uncertain','cancelled') AND updated_at<? ORDER BY id LIMIT ?)", cutoff, messageCleanupBatchSize)
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
	if (intent.Kind != "new" && intent.Kind != "resume") || intent.Generation != previous.Generation+1 {
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
	_, err = tx.Exec("INSERT INTO startup_intents(bot,chat,thread,kind,workspace,session,generation) VALUES(?,?,?,?,?,?,?)", intent.Bot, intent.Chat, intent.Thread, intent.Kind, intent.Workspace, intent.Session, intent.Generation)
	if err != nil {
		return err
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
	if _, err = tx.Exec("INSERT INTO history(bot,chat,thread,workspace,session,generation) SELECT bot,chat,thread,workspace,session,generation FROM bindings WHERE bot=? AND chat=? AND thread=?", b.Bot, b.Chat, b.Thread); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO bindings(bot,chat,thread,workspace,session,generation,running,interrupted) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(bot,chat,thread) DO UPDATE SET workspace=excluded.workspace,session=excluded.session,generation=excluded.generation,running=excluded.running,interrupted=excluded.interrupted", b.Bot, b.Chat, b.Thread, b.Workspace, b.Session, b.Generation, b.Running, b.Interrupted); err != nil {
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
	return s.completeInboxWithReplies(ctx, id, chat, thread, "done", replies)
}

// CompleteInboxUncertainWithReplies commits a terminal result that cannot be confirmed.
// A failed transaction leaves the submitted input and outbox unchanged.
func (s *Store) CompleteInboxUncertainWithReplies(ctx context.Context, id, chat, thread int64, replies []string) error {
	return s.completeInboxWithReplies(ctx, id, chat, thread, "uncertain", replies)
}

// CompleteInboxCancelledWithReplies commits an aborted terminal result and its replies together.
// A failed transaction leaves the submitted input and outbox unchanged.
func (s *Store) CompleteInboxCancelledWithReplies(ctx context.Context, id, chat, thread int64, replies []string) error {
	return s.completeInboxWithReplies(ctx, id, chat, thread, "cancelled", replies)
}

func (s *Store) completeInboxWithReplies(ctx context.Context, id, chat, thread int64, state string, replies []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, "UPDATE inbox SET state=?,updated_at=? WHERE id=? AND state='submitted'", state, now, id)
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
		if _, err := tx.ExecContext(ctx, "INSERT INTO outbox(chat,thread,text,state,created_at,updated_at) VALUES(?,?,?,'pending',?,?)", chat, thread, reply, now, now); err != nil {
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
	e := s.DB.QueryRow("SELECT id,chat,thread,text,kind,path,name FROM outbox WHERE state='pending' ORDER BY id LIMIT 1").Scan(&o.ID, &o.Chat, &o.Thread, &o.Text, &o.Kind, &o.Path, &o.Name)
	return o, e
}
func (s *Store) MarkOutput(id int64, state string) error {
	_, e := s.DB.Exec("UPDATE outbox SET state=?,updated_at=? WHERE id=?", state, time.Now().Unix(), id)
	return e
}
func (s *Store) Uncertain() (int, error) {
	var n int
	e := s.DB.QueryRow("SELECT (SELECT COUNT(*) FROM inbox WHERE state='uncertain')+(SELECT COUNT(*) FROM outbox WHERE state='uncertain')").Scan(&n)
	return n, e
}
