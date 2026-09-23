package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func openTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func requireStoreOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSchemaVersionSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	var version int
	requireStoreOK(t, s.DB.QueryRow("PRAGMA user_version").Scan(&version))
	if version != schemaVersion {
		t.Fatalf("new database version = %d, want %d", version, schemaVersion)
	}
	var indexCount int
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name='idx_outbox_inbox_state'").Scan(&indexCount))
	if indexCount != 1 {
		t.Fatalf("new database progress association index count = %d, want 1", indexCount)
	}
	var favoriteTableCount int
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name='session_favorites'").Scan(&favoriteTableCount))
	if favoriteTableCount != 1 {
		t.Fatalf("session favorites table count = %d, want 1", favoriteTableCount)
	}
	requireStoreOK(t, s.Accept(10, []byte(`{"update_id":10}`)))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	pending, err := s.Pending()
	requireStoreOK(t, err)
	if len(pending) != 1 || pending[0].ID != 10 {
		t.Fatal("reopening the current schema lost existing input")
	}
}

func TestOpenEscapesQuestionMarkInDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state?one")
	s := openTestStore(t, dir)
	if err := s.Accept(10, []byte(`{"update_id":10}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "omp-telegram.db"))
	if err != nil || info.Size() == 0 {
		t.Fatalf("database in question-mark directory was not opened: info=%v error=%v", info, err)
	}
}

func TestUnsupportedSchemaLeavesDataUntouched(t *testing.T) {
	for name, version := range map[string]int{"unversioned": 0, "future": schemaVersion + 1} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := sql.Open("sqlite", filepath.Join(dir, "omp-telegram.db"))
			requireStoreOK(t, err)
			defer db.Close()
			_, err = db.Exec("CREATE TABLE saved(value TEXT); INSERT INTO saved VALUES('keep'); PRAGMA user_version=" + strconv.Itoa(version))
			requireStoreOK(t, err)
			if s, err := Open(dir); err == nil {
				s.Close()
				t.Fatal("unsupported database was accepted")
			}
			var value string
			var actual, objects int
			requireStoreOK(t, db.QueryRow("SELECT value FROM saved").Scan(&value))
			requireStoreOK(t, db.QueryRow("PRAGMA user_version").Scan(&actual))
			requireStoreOK(t, db.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'").Scan(&objects))
			if value != "keep" || actual != version || objects != 1 {
				t.Fatalf("rejected database was modified: value=%q version=%d objects=%d", value, actual, objects)
			}
		})
	}
}

func TestVersionOneMigratesStartupIntents(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "omp-telegram.db"))
	requireStoreOK(t, err)
	_, err = db.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY,value INTEGER NOT NULL);
CREATE TABLE bindings(bot INTEGER,chat INTEGER,thread INTEGER,workspace TEXT NOT NULL,session TEXT NOT NULL,generation INTEGER NOT NULL,running INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(bot,chat,thread));
CREATE TABLE history(bot INTEGER,chat INTEGER,thread INTEGER,workspace TEXT,session TEXT,generation INTEGER);
CREATE TABLE inbox(id INTEGER PRIMARY KEY,raw BLOB NOT NULL,state TEXT NOT NULL);
CREATE TABLE outbox(id INTEGER PRIMARY KEY AUTOINCREMENT,chat INTEGER,thread INTEGER,text TEXT NOT NULL,state TEXT NOT NULL,kind TEXT NOT NULL DEFAULT 'text',path TEXT NOT NULL DEFAULT '',name TEXT NOT NULL DEFAULT '');
CREATE INDEX idx_inbox_state ON inbox(state,id);
CREATE INDEX idx_outbox_state ON outbox(state,id);
PRAGMA user_version=1;`)
	requireStoreOK(t, err)
	requireStoreOK(t, db.Close())
	s := openTestStore(t, dir)
	var version int
	requireStoreOK(t, s.DB.QueryRow("PRAGMA user_version").Scan(&version))
	if version != schemaVersion {
		t.Fatalf("migrated version = %d, want %d", version, schemaVersion)
	}
	if _, err := s.PendingStarts(1); err != nil {
		t.Fatalf("startup intent migration missing table: %v", err)
	}
}

func TestCompletionRollsBackEveryReplyAndInputState(t *testing.T) {
	for name, trigger := range map[string]string{
		"reply-write":  `CREATE TRIGGER reject_reply BEFORE INSERT ON outbox WHEN NEW.text='second' BEGIN SELECT RAISE(FAIL,'injected reply failure'); END`,
		"input-update": `CREATE TRIGGER reject_done BEFORE UPDATE OF state ON inbox WHEN NEW.state='done' BEGIN SELECT RAISE(FAIL,'injected completion failure'); END`,
	} {
		t.Run(name, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			requireStoreOK(t, s.Accept(10, []byte(`{"update_id":10}`)))
			requireStoreOK(t, s.Mark(10, "submitted"))
			_, err := s.DB.Exec(trigger)
			requireStoreOK(t, err)
			if err := s.CompleteInboxWithReplies(context.Background(), 10, 1, 2, []string{"first", "second"}); err == nil {
				t.Fatal("completion ignored a persistence failure")
			}
			var state string
			requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state))
			if state != "submitted" {
				t.Fatalf("failed completion changed input to %q", state)
			}
			if _, err := s.NextOutput(); err != sql.ErrNoRows {
				t.Fatalf("failed completion left a deliverable reply: %v", err)
			}
		})
	}
}

func TestCompletionSurvivesRestartWithOrderedReplies(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	requireStoreOK(t, s.Accept(10, []byte(`{"update_id":10}`)))
	requireStoreOK(t, s.Mark(10, "submitted"))
	requireStoreOK(t, s.CompleteInboxWithReplies(context.Background(), 10, 1, 2, []string{"first", "second"}))
	if err := s.CompleteInboxWithReplies(context.Background(), 10, 1, 2, []string{"duplicate"}); err == nil {
		t.Fatal("already completed input accepted a second result")
	}
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	var state string
	requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state))
	if state != "done" {
		t.Fatalf("committed completion became %q after restart", state)
	}
	for _, expected := range []string{"first", "second"} {
		out, err := s.NextOutput()
		requireStoreOK(t, err)
		if out.Text != expected {
			t.Fatalf("next reply = %q, want %q", out.Text, expected)
		}
		requireStoreOK(t, s.MarkOutput(out.ID, "sending"))
		requireStoreOK(t, s.MarkOutput(out.ID, "done"))
	}
	if _, err := s.NextOutput(); err != sql.ErrNoRows {
		t.Fatalf("duplicate reply became deliverable: %v", err)
	}
}

func TestRootReplyTargetSurvivesCompletionAndRestart(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	requireStoreOK(t, s.Accept(10, []byte(`{"update_id":10}`)))
	requireStoreOK(t, s.Submit(10, 42))
	requireStoreOK(t, s.CompleteInboxWithReplies(context.Background(), 10, 1, 2, []string{"first", "second"}))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	for _, want := range []string{"first", "second"} {
		out, err := s.NextOutput()
		requireStoreOK(t, err)
		if out.Text != want || out.ReplyTo != 42 {
			t.Fatalf("durable reply target = %+v, want text %q replying to 42", out, want)
		}
		requireStoreOK(t, s.MarkOutput(out.ID, "sending"))
		requireStoreOK(t, s.MarkOutput(out.ID, "done"))
	}
}

func TestMarkOutputRequiresSendingState(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	requireStoreOK(t, s.Enqueue(1, 2, "reply"))
	out, err := s.NextOutput()
	requireStoreOK(t, err)
	if err = s.MarkOutput(out.ID, OutboxDone); !errors.Is(err, ErrOutboxStateTransition) {
		t.Fatalf("pending to done error = %v, want %v", err, ErrOutboxStateTransition)
	}
	requireStoreOK(t, s.MarkOutput(out.ID, OutboxSending))
	requireStoreOK(t, s.MarkOutput(out.ID, OutboxDone))
}

func TestProgressMessageWaitsForAllFinalReplies(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	requireStoreOK(t, s.Accept(10, []byte(`{"update_id":10}`)))
	requireStoreOK(t, s.Submit(10, 42))
	requireStoreOK(t, s.CompleteInboxWithReplies(context.Background(), 10, 7, 8, []string{"first", "second"}))
	requireStoreOK(t, s.SetProgressMessage(10, 99))
	if target, ready, err := s.CompletedProgressMessage(10); err != nil || ready {
		t.Fatalf("pending progress cleanup = %+v, ready=%t, err=%v", target, ready, err)
	}
	first, err := s.NextOutput()
	requireStoreOK(t, err)
	if first.InboxID != 10 {
		t.Fatalf("first output inbox = %d, want 10", first.InboxID)
	}
	requireStoreOK(t, s.MarkOutput(first.ID, "sending"))
	requireStoreOK(t, s.MarkOutput(first.ID, "done"))
	if _, ready, err := s.CompletedProgressMessage(10); err != nil || ready {
		t.Fatalf("partial progress cleanup became ready=%t, err=%v", ready, err)
	}
	second, err := s.NextOutput()
	requireStoreOK(t, err)
	requireStoreOK(t, s.MarkOutput(second.ID, "sending"))
	requireStoreOK(t, s.MarkOutput(second.ID, "done"))
	target, ready, err := s.CompletedProgressMessage(10)
	requireStoreOK(t, err)
	if !ready || target.InboxID != 10 || target.Chat != 7 || target.MessageID != 99 {
		t.Fatalf("completed progress target = %+v, ready=%t", target, ready)
	}
	targets, err := s.CompletedProgressMessages()
	requireStoreOK(t, err)
	if len(targets) != 1 || targets[0] != target {
		t.Fatalf("startup progress targets = %+v, want [%+v]", targets, target)
	}
	requireStoreOK(t, s.ClearProgressMessage(target.InboxID, target.MessageID))
	if _, ready, err := s.CompletedProgressMessage(10); err != nil || ready {
		t.Fatalf("cleared progress cleanup remained ready=%t, err=%v", ready, err)
	}
}
func TestTerminalProgressWaitsForFinalReplyDelivery(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	for _, tc := range []struct {
		id    int64
		state string
	}{
		{id: 20, state: "cancelled"},
		{id: 21, state: "uncertain"},
	} {
		requireStoreOK(t, s.Accept(tc.id, []byte(`{}`)))
		requireStoreOK(t, s.Mark(tc.id, "submitted"))
		var err error
		if tc.state == "cancelled" {
			err = s.CompleteInboxCancelledWithReplies(context.Background(), tc.id, 7, 8, []string{"terminal"})
		} else {
			err = s.CompleteInboxUncertainWithReplies(context.Background(), tc.id, 7, 8, []string{"terminal"})
		}
		requireStoreOK(t, err)
		requireStoreOK(t, s.SetProgressMessage(tc.id, tc.id+100))
		if _, ready, err := s.CompletedProgressMessage(tc.id); err != nil || ready {
			t.Fatalf("%s progress became ready before delivery: ready=%t, err=%v", tc.state, ready, err)
		}
		out, err := s.NextOutput()
		requireStoreOK(t, err)
		requireStoreOK(t, s.MarkOutput(out.ID, OutboxSending))
		requireStoreOK(t, s.MarkOutput(out.ID, OutboxDone))
		target, ready, err := s.CompletedProgressMessage(tc.id)
		requireStoreOK(t, err)
		if !ready || target.InboxID != tc.id || target.MessageID != tc.id+100 {
			t.Fatalf("%s progress after delivery = %+v, ready=%t", tc.state, target, ready)
		}
	}
}

func TestRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	requireStoreOK(t, s.Accept(10, []byte(`{"update_id":10}`)))
	requireStoreOK(t, s.Accept(11, []byte(`{"update_id":11}`)))
	requireStoreOK(t, s.Mark(11, "submitted"))
	requireStoreOK(t, s.Enqueue(1, 2, "possibly delivered"))
	first, err := s.NextOutput()
	requireStoreOK(t, err)
	requireStoreOK(t, s.MarkOutput(first.ID, "sending"))
	requireStoreOK(t, s.Enqueue(1, 2, "not sent"))
	requireStoreOK(t, s.Close())

	s = openTestStore(t, dir)
	pending, err := s.Pending()
	requireStoreOK(t, err)
	if len(pending) != 1 || pending[0].ID != 10 || string(pending[0].Raw) != `{"update_id":10}` {
		t.Fatalf("pending after restart: %+v", pending)
	}
	n, err := s.Uncertain()
	requireStoreOK(t, err)
	if n != 2 {
		t.Fatalf("uncertain = %d, want submitted input and sending output", n)
	}
	out, err := s.NextOutput()
	requireStoreOK(t, err)
	if out.Text != "not sent" || out.Chat != 1 || out.Thread != 2 {
		t.Fatalf("replayed uncertain output: %+v", out)
	}
	requireStoreOK(t, s.MarkOutput(out.ID, "sending"))
	requireStoreOK(t, s.MarkOutput(out.ID, "sent"))
	if _, err = s.NextOutput(); err != sql.ErrNoRows {
		t.Fatalf("uncertain output became deliverable: %v", err)
	}
	requireStoreOK(t, s.Accept(11, []byte(`{"duplicate":true}`)))
	pending, err = s.Pending()
	requireStoreOK(t, err)
	if len(pending) != 1 || pending[0].ID != 10 {
		t.Fatalf("duplicate revived uncertain input: %+v", pending)
	}
}

func TestClaimNextOutputDoesNotBlockOtherConversations(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	requireStoreOK(t, s.Enqueue(-10, 11, "rate limited"))
	requireStoreOK(t, s.Enqueue(-10, 11, "same conversation"))
	requireStoreOK(t, s.Enqueue(-10, 22, "independent conversation"))
	now := time.Now().Unix()
	first, err := s.ClaimNextOutput(context.Background(), now)
	requireStoreOK(t, err)
	if first.Chat != -10 || first.Thread != 11 || first.AttemptCount != 1 {
		t.Fatalf("first claim = %+v", first)
	}
	requireStoreOK(t, s.RetryOutput(first.ID, now+60))
	other, err := s.ClaimNextOutput(context.Background(), now)
	requireStoreOK(t, err)
	if other.Chat != -10 || other.Thread != 22 || other.Text != "independent conversation" {
		t.Fatalf("independent claim = %+v", other)
	}
	requireStoreOK(t, s.MarkOutput(other.ID, OutboxDone))
	if _, err = s.ClaimNextOutput(context.Background(), now); err != sql.ErrNoRows {
		t.Fatalf("same conversation bypassed retry deadline: %v", err)
	}
	if _, err = s.ClaimNextOutput(context.Background(), now+60); err != nil {
		t.Fatalf("rate-limited output did not become claimable: %v", err)
	}
}

func TestAttachmentRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	requireStoreOK(t, s.EnqueueAttachment(1, 2, "photo", "/spool/photo", "picture.png", "possibly delivered"))
	first, err := s.NextOutput()
	requireStoreOK(t, err)
	requireStoreOK(t, s.MarkOutput(first.ID, "sending"))
	want := Output{ID: first.ID + 1, Chat: 3, Thread: 4, Kind: "document", Path: "/spool/document", Name: "report.pdf", Text: "report caption"}
	requireStoreOK(t, s.EnqueueAttachment(want.Chat, want.Thread, want.Kind, want.Path, want.Name, want.Text))
	requireStoreOK(t, s.Close())

	s = openTestStore(t, dir)
	out, err := s.NextOutput()
	requireStoreOK(t, err)
	if out.ID != want.ID || out.Chat != want.Chat || out.Thread != want.Thread || out.Kind != want.Kind || out.Path != want.Path || out.Name != want.Name || out.Text != want.Text || out.NextAttemptAt <= 0 || out.AttemptCount != 0 {
		t.Fatalf("staged attachment after restart = %+v, want durable fields %+v", out, want)
	}
	n, err := s.Uncertain()
	requireStoreOK(t, err)
	if n != 1 {
		t.Fatalf("uncertain attachments = %d, want 1", n)
	}
	var state string
	requireStoreOK(t, s.DB.QueryRow("SELECT state FROM outbox WHERE id=?", first.ID).Scan(&state))
	if state != "uncertain" {
		t.Fatalf("in-flight attachment state = %q", state)
	}
	requireStoreOK(t, s.MarkOutput(out.ID, "sending"))
	requireStoreOK(t, s.MarkOutput(out.ID, "sent"))
	if _, err = s.NextOutput(); err != sql.ErrNoRows {
		t.Fatalf("ambiguous attachment was replayed: %v", err)
	}
}

func TestRejectUnsupportedAttachmentKind(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	for _, kind := range []string{"", "text", "voice", "PHOTO"} {
		if err := s.EnqueueAttachment(1, 2, kind, "/spool/file", "file", "caption"); err == nil {
			t.Fatalf("accepted attachment kind %q", kind)
		}
	}
	if out, err := s.NextOutput(); err != sql.ErrNoRows {
		t.Fatalf("invalid attachment entered outbox: %+v, %v", out, err)
	}
}

func TestAcceptAtomicDedupAndOffset(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	requireStoreOK(t, s.Accept(20, []byte(`{"original":true}`)))
	requireStoreOK(t, s.Accept(20, []byte(`{"replacement":true}`)))
	requireStoreOK(t, s.Accept(19, []byte(`{}`)))
	pending, err := s.Pending()
	requireStoreOK(t, err)
	if len(pending) != 2 || pending[1].ID != 20 || string(pending[1].Raw) != `{"original":true}` {
		t.Fatalf("dedup changed original input: %+v", pending)
	}
	offset, err := s.Offset()
	requireStoreOK(t, err)
	if offset != 21 {
		t.Fatalf("offset regressed: %d", offset)
	}
	if err = s.Accept(30, nil); err == nil {
		t.Fatal("accepted input without durable payload")
	}
	offset, err = s.Offset()
	requireStoreOK(t, err)
	if offset != 21 {
		t.Fatalf("failed payload advanced offset: %d", offset)
	}
	_, err = s.DB.Exec(`CREATE TRIGGER reject_offset BEFORE UPDATE ON meta WHEN NEW.key='offset' BEGIN SELECT RAISE(ABORT,'offset failure'); END`)
	requireStoreOK(t, err)
	if err = s.Accept(40, []byte(`{}`)); err == nil {
		t.Fatal("accepted input despite offset write failure")
	}
	pending, err = s.Pending()
	requireStoreOK(t, err)
	if len(pending) != 2 {
		t.Fatalf("offset failure left partially committed input: %+v", pending)
	}
}

func TestBotIdentitySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	requireStoreOK(t, s.CheckBot(42))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	if err := s.CheckBot(43); err == nil {
		t.Fatal("accepted a different bot's data directory")
	}
	requireStoreOK(t, s.CheckBot(42))
}

func TestLatestBindingAndHistorySurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	first := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/workspaces/first", Session: "/sessions/first.jsonl", SessionID: "11111111", Generation: 1}
	second := first
	second.Workspace, second.Session, second.SessionID, second.Generation = "/workspaces/second", "/sessions/second.jsonl", "22222222", 2
	third := second
	third.Session, third.SessionID, third.Generation = "/sessions/third.jsonl", "33333333", 3
	other := Binding{Bot: 1, Chat: 2, Thread: 4, Workspace: "/workspaces/other", Session: "/sessions/other.jsonl", SessionID: "44444444", Generation: 1}
	for _, b := range []Binding{first, other, second, third} {
		requireStoreOK(t, s.Save(b))
	}
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	for _, want := range []Binding{third, other} {
		got, err := s.Binding(want.Bot, want.Chat, want.Thread)
		requireStoreOK(t, err)
		if got != want {
			t.Fatalf("restored %+v, want %+v", got, want)
		}
	}
	rows, err := s.DB.Query("SELECT bot,chat,thread,workspace,session,generation FROM history ORDER BY generation")
	requireStoreOK(t, err)
	defer rows.Close()
	for _, persisted := range []Binding{first, second} {
		if !rows.Next() {
			t.Fatalf("lost historical binding %+v: %v", persisted, rows.Err())
		}
		var got Binding
		requireStoreOK(t, rows.Scan(&got.Bot, &got.Chat, &got.Thread, &got.Workspace, &got.Session, &got.Generation))
		persisted.SessionID = ""
		if got != persisted {
			t.Fatalf("history = %+v, want %+v", got, persisted)
		}
	}
	if rows.Next() {
		t.Fatal("unexpected historical binding")
	}
	requireStoreOK(t, rows.Err())
}

func TestPrivateStorePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	requireStoreOK(t, os.Mkdir(dir, 0755))
	requireStoreOK(t, os.Chmod(dir, 0755))
	path := filepath.Join(dir, "omp-telegram.db")
	requireStoreOK(t, os.WriteFile(path, nil, 0644))
	s := openTestStore(t, dir)
	requireStoreOK(t, s.Accept(1, []byte(`{"sensitive":"prompt"}`)))
	for path, want := range map[string]os.FileMode{dir: 0755, path: 0600} {
		info, err := os.Stat(path)
		requireStoreOK(t, err)
		if info.Mode().Perm() != want {
			t.Fatalf("%s permissions = %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}

func TestRejectDatabaseSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	requireStoreOK(t, os.WriteFile(target, []byte("unrelated"), 0600))
	requireStoreOK(t, os.Symlink(target, filepath.Join(dir, "omp-telegram.db")))
	if s, err := Open(dir); err == nil {
		s.Close()
		t.Fatal("opened symlink as database")
	}
	contents, err := os.ReadFile(target)
	requireStoreOK(t, err)
	if string(contents) != "unrelated" {
		t.Fatalf("database open modified symlink target: %q", contents)
	}
}

func TestBindingReplacementRollsBackHistory(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	old := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/workspaces/project", Session: "/sessions/original.jsonl", Generation: 1}
	requireStoreOK(t, s.Save(old))
	_, err := s.DB.Exec(`CREATE TRIGGER reject_binding BEFORE UPDATE ON bindings BEGIN SELECT RAISE(ABORT,'binding failure'); END`)
	requireStoreOK(t, err)
	next := old
	next.Session, next.Generation, next.Running = "/sessions/new.jsonl", 2, true
	if err = s.Save(next); err == nil {
		t.Fatal("binding replacement unexpectedly succeeded")
	}
	got, err := s.Binding(old.Bot, old.Chat, old.Thread)
	requireStoreOK(t, err)
	if got != old {
		t.Fatalf("failed save replaced binding: %+v", got)
	}
	var count int
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM history").Scan(&count))
	if count != 0 {
		t.Fatalf("failed save created %d historical bindings", count)
	}
}

func TestStartupIntentCommitsReplacementAtomically(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	old := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/workspaces/old", Session: "/sessions/old.jsonl", Generation: 1, LastUsedAt: 123, Running: true}
	requireStoreOK(t, s.Save(old))
	intent := StartIntent{Bot: old.Bot, Chat: old.Chat, Thread: old.Thread, Kind: "new", Workspace: "/workspaces/new", Generation: 2}
	requireStoreOK(t, s.PrepareStart(old, intent))
	stored, err := s.Binding(old.Bot, old.Chat, old.Thread)
	requireStoreOK(t, err)
	if stored.Running {
		t.Fatal("prepare retained replacement eligibility")
	}
	intents, err := s.PendingStarts(old.Bot)
	requireStoreOK(t, err)
	if len(intents) != 1 || intents[0] != intent {
		t.Fatalf("prepared intents = %+v, want %+v", intents, []StartIntent{intent})
	}
	conflict := intent
	conflict.Workspace = "/workspaces/other"
	if err = s.PrepareStart(stored, conflict); err == nil {
		t.Fatal("prepared startup intent was overwritten")
	}
	intents, err = s.PendingStarts(old.Bot)
	requireStoreOK(t, err)
	if len(intents) != 1 || intents[0] != intent {
		t.Fatalf("conflicting startup intent changed durable state: %+v", intents)
	}
	next := Binding{Bot: old.Bot, Chat: old.Chat, Thread: old.Thread, Workspace: intent.Workspace, Session: "/sessions/new.jsonl", Generation: intent.Generation, Running: true}
	requireStoreOK(t, s.CommitStart(next))
	stored, err = s.Binding(old.Bot, old.Chat, old.Thread)
	expected := next
	expected.LastUsedAt = old.LastUsedAt
	if stored != expected {
		t.Fatalf("committed binding = %+v, want %+v", stored, expected)
	}
	intents, err = s.PendingStarts(old.Bot)
	requireStoreOK(t, err)
	if len(intents) != 0 {
		t.Fatalf("committed startup intent remained: %+v", intents)
	}
}

func TestRunningBindingsSurviveRestartAndStayScoped(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	first := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/workspaces/project", Session: "/sessions/first.jsonl", Generation: 1, Running: true}
	second := first
	second.Thread, second.Session = 4, "/sessions/second.jsonl"
	third := first
	third.Chat, third.Thread, third.Session = 3, 1, "/sessions/third.jsonl"
	closed := first
	closed.Thread, closed.Running = 1, false
	otherBot := first
	otherBot.Bot = 2
	for _, b := range []Binding{third, closed, second, otherBot, first} {
		requireStoreOK(t, s.Save(b))
	}
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	got, err := s.RunningBindings(first.Bot)
	requireStoreOK(t, err)
	want := []Binding{first, second, third}
	if len(got) != len(want) {
		t.Fatalf("running bindings = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("running binding %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if err := s.SetRunning(first, false); err != nil {
		t.Fatalf("current binding close failed: %v", err)
	}
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	got, err = s.RunningBindings(first.Bot)
	requireStoreOK(t, err)
	if len(got) != 2 || got[0] != second || got[1] != third {
		t.Fatalf("closed binding eligible after restart: %+v", got)
	}
	got, err = s.RunningBindings(otherBot.Bot)
	requireStoreOK(t, err)
	if len(got) != 1 || got[0] != otherBot {
		t.Fatalf("other bot changed by close: %+v", got)
	}
}

func TestSetRunningCannotDisableReplacementGeneration(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	old := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/workspace", Session: "/sessions/old.jsonl", Generation: 1, Running: true}
	requireStoreOK(t, s.Save(old))
	next := old
	next.Session, next.Generation = "/sessions/new.jsonl", 2
	requireStoreOK(t, s.Save(next))
	if err := s.SetRunning(old, false); err != ErrStaleBinding {
		t.Fatalf("stale binding error = %v, want %v", err, ErrStaleBinding)
	}
	got, err := s.RunningBindings(1)
	requireStoreOK(t, err)
	if len(got) != 1 || got[0] != next {
		t.Fatalf("stale exit disabled replacement: %+v", got)
	}
	requireStoreOK(t, s.SetRunning(next, false))
	got, err = s.RunningBindings(1)
	requireStoreOK(t, err)
	if len(got) != 0 {
		t.Fatalf("current exit left running binding: %+v", got)
	}
	if err := s.SetRunning(old, true); err != ErrStaleBinding {
		t.Fatalf("stale binding error = %v, want %v", err, ErrStaleBinding)
	}
	got, err = s.RunningBindings(1)
	requireStoreOK(t, err)
	if len(got) != 0 {
		t.Fatalf("stale generation revived closed replacement: %+v", got)
	}
	requireStoreOK(t, s.SetRunning(next, true))
	got, err = s.RunningBindings(1)
	requireStoreOK(t, err)
	if len(got) != 1 || got[0] != next {
		t.Fatalf("current generation could not resume: %+v", got)
	}
}

func TestVersionThreeMigratesMessageTimestamps(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "omp-telegram.db"))
	requireStoreOK(t, err)
	_, err = db.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY,value INTEGER NOT NULL);
CREATE TABLE bindings(bot INTEGER,chat INTEGER,thread INTEGER,workspace TEXT NOT NULL,session TEXT NOT NULL,generation INTEGER NOT NULL,running INTEGER NOT NULL DEFAULT 0,interrupted INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(bot,chat,thread));
CREATE TABLE startup_intents(bot INTEGER,chat INTEGER,thread INTEGER,kind TEXT NOT NULL CHECK(kind IN ('new','resume')),workspace TEXT NOT NULL,session TEXT NOT NULL,generation INTEGER NOT NULL,PRIMARY KEY(bot,chat,thread));
CREATE TABLE history(bot INTEGER,chat INTEGER,thread INTEGER,workspace TEXT,session TEXT,generation INTEGER);
CREATE TABLE inbox(id INTEGER PRIMARY KEY,raw BLOB NOT NULL,state TEXT NOT NULL);
CREATE TABLE outbox(id INTEGER PRIMARY KEY AUTOINCREMENT,chat INTEGER,thread INTEGER,text TEXT NOT NULL,state TEXT NOT NULL,kind TEXT NOT NULL DEFAULT 'text',path TEXT NOT NULL DEFAULT '',name TEXT NOT NULL DEFAULT '');
CREATE INDEX idx_inbox_state ON inbox(state,id);
CREATE INDEX idx_outbox_state ON outbox(state,id);
INSERT INTO bindings(bot,chat,thread,workspace,session,generation,running) VALUES(1,2,3,'/workspace','/sessions/2026-09-17T14-39-46-235Z_01a0afcf-303b-775f-adda-fe30af90a116.jsonl',1,1);
INSERT INTO inbox VALUES(1,'input','done');
INSERT INTO outbox(chat,thread,text,state) VALUES(2,3,'output','sent');
PRAGMA user_version=3;`)
	requireStoreOK(t, err)
	requireStoreOK(t, db.Close())
	s := openTestStore(t, dir)
	var version int
	var inboxCreated, inboxUpdated, outboxCreated, outboxUpdated, inboxReplyTo, outboxReplyTo int64
	var sessionID string
	var progressMessageID, outboxInboxID int64
	requireStoreOK(t, s.DB.QueryRow("PRAGMA user_version").Scan(&version))
	requireStoreOK(t, s.DB.QueryRow("SELECT created_at,updated_at FROM inbox WHERE id=1").Scan(&inboxCreated, &inboxUpdated))
	requireStoreOK(t, s.DB.QueryRow("SELECT created_at,updated_at FROM outbox WHERE id=1").Scan(&outboxCreated, &outboxUpdated))
	requireStoreOK(t, s.DB.QueryRow("SELECT reply_to FROM inbox WHERE id=1").Scan(&inboxReplyTo))
	requireStoreOK(t, s.DB.QueryRow("SELECT reply_to FROM outbox WHERE id=1").Scan(&outboxReplyTo))
	requireStoreOK(t, s.DB.QueryRow("SELECT session_id FROM bindings WHERE bot=1 AND chat=2 AND thread=3").Scan(&sessionID))
	requireStoreOK(t, s.DB.QueryRow("SELECT progress_message_id FROM inbox WHERE id=1").Scan(&progressMessageID))
	requireStoreOK(t, s.DB.QueryRow("SELECT inbox_id FROM outbox WHERE id=1").Scan(&outboxInboxID))
	if version != schemaVersion || inboxCreated <= 0 || inboxUpdated <= 0 || outboxCreated <= 0 || outboxUpdated <= 0 || inboxReplyTo != 0 || outboxReplyTo != 0 || progressMessageID != 0 || outboxInboxID != 0 || sessionID != "01a0afcf-303b-775f-adda-fe30af90a116" {
		t.Fatalf("version/message migration = version=%d times=%d/%d/%d/%d replies=%d/%d progress=%d/%d session=%q", version, inboxCreated, inboxUpdated, outboxCreated, outboxUpdated, inboxReplyTo, outboxReplyTo, progressMessageID, outboxInboxID, sessionID)
	}
	var indexCount int
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name='idx_outbox_inbox_state'").Scan(&indexCount))
	if indexCount != 1 {
		t.Fatalf("migrated progress association index count = %d, want 1", indexCount)
	}
	result, err := s.CleanupMessages(context.Background(), time.Now().AddDate(0, 0, -90).Unix())
	requireStoreOK(t, err)
	if result.Inbox != 0 || result.Outbox != 0 {
		t.Fatalf("migration-time records were immediately pruned: %+v", result)
	}
}

func TestAlreadyMigratedBindingBackfillsSessionID(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	path := "/sessions/2026-09-17T14-39-46-235Z_01a0afcf-303b-775f-adda-fe30af90a116.jsonl"
	requireStoreOK(t, s.Save(Binding{Bot: 1, Chat: 2, Thread: 3, Session: path, Generation: 1, Running: true}))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	got, err := s.Binding(1, 2, 3)
	requireStoreOK(t, err)
	if got.SessionID != "01a0afcf-303b-775f-adda-fe30af90a116" {
		t.Fatalf("backfilled session ID = %q", got.SessionID)
	}
}

func TestCleanupMessagesProtectsNonterminalAndRecentRows(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	binding := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/workspace", Session: "/session", Generation: 1}
	requireStoreOK(t, s.Save(binding))
	for _, id := range []int64{1, 2, 3, 4, 5, 6, 7, 8} {
		requireStoreOK(t, s.Accept(id, []byte(`{}`)))
	}
	for _, id := range []int64{2, 3} {
		requireStoreOK(t, s.Mark(id, InboxSubmitted))
	}
	for id, state := range map[int64]InboxState{1: InboxDone, 2: InboxUncertain, 3: InboxUncertain, 4: InboxIgnored, 5: InboxCancelled, 7: InboxSubmitted, 8: InboxDone} {
		requireStoreOK(t, s.Mark(id, state))
	}
	for range 7 {
		requireStoreOK(t, s.Enqueue(2, 3, "message"))
	}
	for id, state := range map[int64]OutboxState{1: OutboxDone, 2: OutboxFailed, 3: OutboxUncertain, 4: OutboxCancelled, 6: OutboxSending, 7: OutboxDone} {
		if state != OutboxSending {
			requireStoreOK(t, s.MarkOutput(id, OutboxSending))
		}
		requireStoreOK(t, s.MarkOutput(id, state))
	}
	old := time.Now().AddDate(0, 0, -91).Unix()
	requireStoreOK(t, setMessageTimes(s, old, old, []int64{1, 2, 3, 4, 5, 6, 7}, []int64{1, 2, 3, 4, 5, 6}))
	result, err := s.CleanupMessages(context.Background(), time.Now().AddDate(0, 0, -90).Unix())
	requireStoreOK(t, err)
	if result.Inbox != 5 || result.Outbox != 4 {
		t.Fatalf("cleanup counts = %+v, want inbox=5 outbox=4", result)
	}
	for _, id := range []int64{6, 7, 8} {
		var state string
		requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=?", id).Scan(&state))
	}
	for _, id := range []int64{5, 6, 7} {
		var state string
		requireStoreOK(t, s.DB.QueryRow("SELECT state FROM outbox WHERE id=?", id).Scan(&state))
	}
	if _, err = s.Binding(binding.Bot, binding.Chat, binding.Thread); err != nil {
		t.Fatalf("message cleanup removed binding: %v", err)
	}
}

func TestCleanupMessagesRetainsProgressAssociationWithActiveOutbox(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	requireStoreOK(t, s.Accept(1, []byte(`{}`)))
	requireStoreOK(t, s.Mark(1, "submitted"))
	requireStoreOK(t, s.CompleteInboxWithReplies(context.Background(), 1, 2, 3, []string{"reply"}))
	requireStoreOK(t, s.SetProgressMessage(1, 77))
	out, err := s.NextOutput()
	requireStoreOK(t, err)
	old := time.Now().AddDate(0, 0, -91).Unix()
	requireStoreOK(t, setMessageTimes(s, old, old, []int64{1}, []int64{out.ID}))
	result, err := s.CleanupMessages(context.Background(), time.Now().AddDate(0, 0, -90).Unix())
	requireStoreOK(t, err)
	if result.Inbox != 0 || result.Outbox != 0 {
		t.Fatalf("cleanup removed active progress association: %+v", result)
	}
	requireStoreOK(t, s.MarkOutput(out.ID, OutboxSending))
	requireStoreOK(t, s.MarkOutput(out.ID, "uncertain"))
	requireStoreOK(t, setMessageTimes(s, old, old, nil, []int64{out.ID}))
	result, err = s.CleanupMessages(context.Background(), time.Now().AddDate(0, 0, -90).Unix())
	requireStoreOK(t, err)
	if result.Inbox != 1 || result.Outbox != 1 {
		t.Fatalf("cleanup after outbox became terminal = %+v, want inbox=1 outbox=1", result)
	}
}

func TestCleanupMessagesReleasesExpiredTerminalProgressAssociations(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	cases := []struct {
		id          int64
		state       string
		outboxState OutboxState
	}{
		{id: 1, state: "cancelled", outboxState: OutboxCancelled},
		{id: 2, state: "uncertain", outboxState: OutboxUncertain},
		{id: 3, state: "done", outboxState: OutboxFailed},
		{id: 4, state: "done", outboxState: OutboxUncertain},
	}
	old := time.Now().AddDate(0, 0, -91).Unix()
	for _, tc := range cases {
		requireStoreOK(t, s.Accept(tc.id, []byte(`{}`)))
		requireStoreOK(t, s.Mark(tc.id, "submitted"))
		var err error
		switch tc.state {
		case "cancelled":
			err = s.CompleteInboxCancelledWithReplies(context.Background(), tc.id, 7, 8, []string{"terminal"})
		case "uncertain":
			err = s.CompleteInboxUncertainWithReplies(context.Background(), tc.id, 7, 8, []string{"terminal"})
		default:
			err = s.CompleteInboxWithReplies(context.Background(), tc.id, 7, 8, []string{"terminal"})
		}
		requireStoreOK(t, err)
		out, err := s.NextOutput()
		requireStoreOK(t, err)
		requireStoreOK(t, s.MarkOutput(out.ID, OutboxSending))
		requireStoreOK(t, s.MarkOutput(out.ID, tc.outboxState))
		requireStoreOK(t, s.SetProgressMessage(tc.id, tc.id+100))
		requireStoreOK(t, setMessageTimes(s, old, old, []int64{tc.id}, []int64{out.ID}))
	}
	result, err := s.CleanupMessages(context.Background(), time.Now().AddDate(0, 0, -90).Unix())
	requireStoreOK(t, err)
	if result.Inbox != int64(len(cases)) || result.Outbox != int64(len(cases)) {
		t.Fatalf("expired terminal progress cleanup = %+v, want inbox=%d outbox=%d", result, len(cases), len(cases))
	}
	var remaining int
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM inbox WHERE progress_message_id>0").Scan(&remaining))
	if remaining != 0 {
		t.Fatalf("expired progress associations remained: %d", remaining)
	}
}

func TestCleanupMessagesUsesLatestStateTransitionAndBatches(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	for id := int64(1); id <= 2_502; id++ {
		requireStoreOK(t, s.Accept(id, []byte(`{}`)))
		requireStoreOK(t, s.Mark(id, "done"))
	}
	old := time.Now().AddDate(0, 0, -120).Unix()
	requireStoreOK(t, setMessageTimes(s, old, old, nil, nil))
	updated := time.Now().Unix()
	_, err := s.DB.Exec("UPDATE inbox SET updated_at=? WHERE id=2502", updated)
	requireStoreOK(t, err)
	result, err := s.CleanupMessages(context.Background(), time.Now().AddDate(0, 0, -90).Unix())
	requireStoreOK(t, err)
	if result.Inbox != 2_501 || result.Outbox != 0 {
		t.Fatalf("batched cleanup result = %+v", result)
	}
	var state string
	requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=2502").Scan(&state))
}

func TestVersionSevenMigratesLastUsedAtWithoutChangingHistory(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "omp-telegram.db"))
	requireStoreOK(t, err)
	_, err = db.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY,value INTEGER NOT NULL);
CREATE TABLE bindings(bot INTEGER,chat INTEGER,thread INTEGER,workspace TEXT NOT NULL,session TEXT NOT NULL,session_id TEXT NOT NULL DEFAULT '',generation INTEGER NOT NULL,running INTEGER NOT NULL DEFAULT 0,interrupted INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(bot,chat,thread));
CREATE TABLE startup_intents(bot INTEGER,chat INTEGER,thread INTEGER,kind TEXT NOT NULL CHECK(kind IN ('new','resume')),workspace TEXT NOT NULL,session TEXT NOT NULL,generation INTEGER NOT NULL,PRIMARY KEY(bot,chat,thread));
CREATE TABLE history(bot INTEGER,chat INTEGER,thread INTEGER,workspace TEXT,session TEXT,generation INTEGER);
CREATE TABLE inbox(id INTEGER PRIMARY KEY,raw BLOB NOT NULL,state TEXT NOT NULL,reply_to INTEGER NOT NULL DEFAULT 0,progress_message_id INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL DEFAULT 0,updated_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE outbox(id INTEGER PRIMARY KEY AUTOINCREMENT,inbox_id INTEGER NOT NULL DEFAULT 0,chat INTEGER,thread INTEGER,text TEXT NOT NULL,state TEXT NOT NULL,reply_to INTEGER NOT NULL DEFAULT 0,kind TEXT NOT NULL DEFAULT 'text',path TEXT NOT NULL DEFAULT '',name TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL DEFAULT 0,updated_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX idx_inbox_state ON inbox(state,id);
CREATE INDEX idx_outbox_state ON outbox(state,id);
CREATE INDEX idx_outbox_inbox_state ON outbox(inbox_id,state);
INSERT INTO bindings(bot,chat,thread,workspace,session,session_id,generation,running,interrupted) VALUES(1,2,3,'/workspace','/sessions/old.jsonl','old-session',7,0,0);
INSERT INTO history(bot,chat,thread,workspace,session,generation) VALUES(1,2,3,'/previous','/sessions/previous.jsonl',6);
PRAGMA user_version=7;`)
	requireStoreOK(t, err)
	requireStoreOK(t, db.Close())
	s := openTestStore(t, dir)
	var version int
	var lastUsed int64
	requireStoreOK(t, s.DB.QueryRow("PRAGMA user_version").Scan(&version))
	requireStoreOK(t, s.DB.QueryRow("SELECT last_used_at FROM bindings WHERE bot=1 AND chat=2 AND thread=3").Scan(&lastUsed))
	if version != schemaVersion || lastUsed != 0 {
		t.Fatalf("v7 migration = version=%d last_used_at=%d, want version=%d and zero timestamp", version, lastUsed, schemaVersion)
	}
	var favoriteTableCount int
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name='session_favorites'").Scan(&favoriteTableCount))
	if favoriteTableCount != 1 {
		t.Fatalf("v7 migration did not create session favorites table")
	}
	binding, err := s.Binding(1, 2, 3)
	requireStoreOK(t, err)
	if binding.Workspace != "/workspace" || binding.Session != "/sessions/old.jsonl" || binding.Generation != 7 {
		t.Fatalf("migration changed binding: %+v", binding)
	}
	var history int
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM history WHERE bot=1 AND chat=2 AND thread=3").Scan(&history))
	if history != 1 {
		t.Fatalf("migration changed history row count: %d", history)
	}
}

func TestBindingsForChatMergesPendingIntents(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	old := Binding{Bot: 1, Chat: 2, Thread: 10, Workspace: "/old", Session: "/sessions/old.jsonl", SessionID: "old-session", Generation: 3, LastUsedAt: time.Now().Add(-12 * 24 * time.Hour).Unix(), Running: true}
	requireStoreOK(t, s.Save(old))
	requireStoreOK(t, s.PrepareStart(old, StartIntent{Bot: 1, Chat: 2, Thread: 10, Kind: "resume", Workspace: "/old", Session: "old-session", Generation: 4}))
	requireStoreOK(t, s.PrepareStart(Binding{Bot: 1, Chat: 2, Thread: 20}, StartIntent{Bot: 1, Chat: 2, Thread: 20, Kind: "new", Workspace: "/new", Generation: 1}))
	requireStoreOK(t, s.Save(Binding{Bot: 1, Chat: 99, Thread: 30, Workspace: "/other", Generation: 1}))
	entries, err := s.BindingsForChat(1, 2)
	requireStoreOK(t, err)
	if len(entries) != 2 || entries[0].Binding == nil || entries[0].Intent == nil || entries[1].Binding != nil || entries[1].Intent == nil {
		t.Fatalf("merged binding entries = %+v", entries)
	}
	if entries[0].Binding.Thread != 10 || entries[0].Intent.Kind != "resume" || entries[0].Binding.LastUsedAt != old.LastUsedAt {
		t.Fatalf("pending resume entry = %+v", entries[0])
	}
	if entries[1].Intent.Thread != 20 || entries[1].Intent.Kind != "new" || entries[1].Intent.Workspace != "/new" {
		t.Fatalf("pending new entry = %+v", entries[1])
	}
}

func TestTouchAndDeleteClosedBindingFenceGeneration(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	first := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/one", Session: "/sessions/one.jsonl", Generation: 1}
	requireStoreOK(t, s.Save(first))
	second := first
	second.Generation = 2
	second.Session = "/sessions/two.jsonl"
	requireStoreOK(t, s.SetPinnedSession(1, 2, 3, "/one", "pinned-session", true))
	requireStoreOK(t, s.Save(second))
	changed, err := s.TouchBinding(1, 2, 3, first.Generation)
	requireStoreOK(t, err)
	if changed {
		t.Fatal("stale generation was touched")
	}
	changed, err = s.TouchBinding(1, 2, 3, second.Generation)
	requireStoreOK(t, err)
	if !changed {
		t.Fatal("current generation was not touched")
	}
	current, err := s.Binding(1, 2, 3)
	requireStoreOK(t, err)
	if current.LastUsedAt <= 0 {
		t.Fatal("touch did not persist last-used timestamp")
	}
	deleted, err := s.DeleteClosedBinding(1, 2, 3, first.Generation)
	requireStoreOK(t, err)
	if deleted {
		t.Fatal("stale generation deleted current binding")
	}
	deleted, err = s.DeleteClosedBinding(1, 2, 3, second.Generation)
	requireStoreOK(t, err)
	if !deleted {
		t.Fatal("closed binding was not deleted")
	}
	if _, err = s.Binding(1, 2, 3); err != sql.ErrNoRows {
		t.Fatalf("deleted binding lookup = %v", err)
	}
	var history int
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM history WHERE bot=1 AND chat=2 AND thread=3").Scan(&history))
	if history != 0 {
		t.Fatalf("binding history survived deletion: %d", history)
	}
	pinned, err := s.PinnedSessions(1, 2, 3, "/one")
	requireStoreOK(t, err)
	if len(pinned) != 0 {
		t.Fatalf("binding deletion left pinned sessions: %+v", pinned)
	}
	third := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/three", Session: "/sessions/three.jsonl", Generation: 3}
	requireStoreOK(t, s.Save(third))
	requireStoreOK(t, s.PrepareStart(third, StartIntent{Bot: 1, Chat: 2, Thread: 3, Kind: "new", Workspace: "/four", Generation: 4}))
	deleted, err = s.DeleteClosedBinding(1, 2, 3, third.Generation)
	requireStoreOK(t, err)
	if deleted {
		t.Fatal("pending transition binding was deleted")
	}
	if _, err = s.Binding(1, 2, 3); err != nil {
		t.Fatalf("pending binding disappeared: %v", err)
	}
}

func TestPinnedSessionsAreScopedAndIdempotent(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	requireStoreOK(t, s.SetPinnedSession(1, 2, 3, "/workspace", "ABCD-SESSION", true))
	requireStoreOK(t, s.SetPinnedSession(1, 2, 3, "/workspace", "abcd-session", true))
	pinned, err := s.PinnedSessions(1, 2, 3, "/workspace")
	requireStoreOK(t, err)
	if len(pinned) != 1 {
		t.Fatalf("duplicate pinned session rows = %d, want 1", len(pinned))
	}
	if _, ok := pinned["abcd-session"]; !ok {
		t.Fatalf("pinned session identity = %+v", pinned)
	}
	for _, scope := range []struct {
		bot, chat, thread int64
		workspace         string
	}{{1, 2, 4, "/workspace"}, {1, 2, 3, "/other"}, {2, 2, 3, "/workspace"}} {
		scoped, err := s.PinnedSessions(scope.bot, scope.chat, scope.thread, scope.workspace)
		requireStoreOK(t, err)
		if len(scoped) != 0 {
			t.Fatalf("pinned session leaked into scope %+v: %+v", scope, scoped)
		}
	}
	requireStoreOK(t, s.SetPinnedSession(1, 2, 3, "/workspace", "ABCD-SESSION", false))
	pinned, err = s.PinnedSessions(1, 2, 3, "/workspace")
	requireStoreOK(t, err)
	if len(pinned) != 0 {
		t.Fatalf("unpinned session remained: %+v", pinned)
	}
}

func TestPinnedSessionGenerationFence(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	first := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/one", Generation: 1}
	requireStoreOK(t, s.Save(first))
	changed, err := s.SetPinnedSessionIfGeneration(1, 2, 3, 1, "/one", "abcd-session", true)
	requireStoreOK(t, err)
	if !changed {
		t.Fatal("current generation did not update pinned session")
	}
	second := first
	second.Generation = 2
	second.Workspace = "/two"
	requireStoreOK(t, s.Save(second))
	changed, err = s.SetPinnedSessionIfGeneration(1, 2, 3, 1, "/one", "abcd-session", false)
	requireStoreOK(t, err)
	if changed {
		t.Fatal("stale generation changed pinned session")
	}
	pinned, err := s.PinnedSessions(1, 2, 3, "/one")
	requireStoreOK(t, err)
	if _, ok := pinned["abcd-session"]; !ok {
		t.Fatalf("stale generation removed pinned session: %+v", pinned)
	}
}

func TestPrepareStartRejectsDeletedClosedBinding(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	previous := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/one", Session: "/sessions/one.jsonl", Generation: 1}
	requireStoreOK(t, s.Save(previous))
	deleted, err := s.DeleteClosedBinding(previous.Bot, previous.Chat, previous.Thread, previous.Generation)
	requireStoreOK(t, err)
	if !deleted {
		t.Fatal("closed binding was not deleted")
	}
	intent := StartIntent{Bot: previous.Bot, Chat: previous.Chat, Thread: previous.Thread, Kind: "resume", Workspace: previous.Workspace, Session: "old-session", Generation: 2}
	if err = s.PrepareStart(previous, intent); err == nil {
		t.Fatal("startup intent recreated a deleted binding")
	}
	intents, err := s.PendingStarts(previous.Bot)
	requireStoreOK(t, err)
	if len(intents) != 0 {
		t.Fatalf("failed stale start left pending intents: %+v", intents)
	}

	first := Binding{Bot: 1, Chat: 2, Thread: 4}
	newIntent := StartIntent{Bot: first.Bot, Chat: first.Chat, Thread: first.Thread, Kind: "new", Workspace: "/new", Generation: 1}
	requireStoreOK(t, s.PrepareStart(first, newIntent))

	existing := Binding{Bot: 1, Chat: 2, Thread: 5, Generation: 1}
	requireStoreOK(t, s.Save(existing))
	if err = s.PrepareStart(Binding{Bot: existing.Bot, Chat: existing.Chat, Thread: existing.Thread}, StartIntent{Bot: existing.Bot, Chat: existing.Chat, Thread: existing.Thread, Kind: "new", Workspace: "/other", Generation: 1}); err == nil {
		t.Fatal("generation-zero startup ignored an existing binding")
	}
}

func setMessageTimes(s *Store, created, updated int64, inbox, outbox []int64) error {
	if inbox == nil {
		_, err := s.DB.Exec("UPDATE inbox SET created_at=?,updated_at=?", created, updated)
		if err != nil {
			return err
		}
	} else {
		for _, id := range inbox {
			if _, err := s.DB.Exec("UPDATE inbox SET created_at=?,updated_at=? WHERE id=?", created, updated, id); err != nil {
				return err
			}
		}
	}
	if outbox == nil {
		return nil
	}
	for _, id := range outbox {
		if _, err := s.DB.Exec("UPDATE outbox SET created_at=?,updated_at=? WHERE id=?", created, updated, id); err != nil {
			return err
		}
	}
	return nil
}
