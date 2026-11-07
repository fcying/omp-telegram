package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// FinalAction retains the opaque callback and its exact configured invocation.
type FinalAction struct {
	Token   string `json:"token"`
	Label   string `json:"label"`
	Command string `json:"command"`
}

type FinalActionSet struct {
	Binding   Binding
	User      int64
	Config    string
	BodyParts int
	Actions   []FinalAction
}

type FinalActionClaim struct {
	Token, Config                               string
	InboxID, Bot, Chat, Thread, User, MessageID int64
	Binding                                     Binding
}

func createFinalActionsSchema(tx *sql.Tx) error {
	for _, column := range []struct{ name, definition string }{
		{"assistant_body", "INTEGER NOT NULL DEFAULT 0 CHECK(assistant_body IN (0,1))"},
		{"final_actions", "TEXT NOT NULL DEFAULT ''"},
		{"actions_pending", "INTEGER NOT NULL DEFAULT 0 CHECK(actions_pending IN (0,1,2))"},
	} {
		var exists bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM pragma_table_info('outbox') WHERE name=?)", column.name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if _, err := tx.Exec("ALTER TABLE outbox ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS final_actions(
 token TEXT PRIMARY KEY,outbox_id INTEGER NOT NULL,bot INTEGER NOT NULL,chat INTEGER NOT NULL,thread INTEGER NOT NULL,
 user INTEGER NOT NULL,workspace TEXT NOT NULL,session TEXT NOT NULL,session_id TEXT NOT NULL,generation INTEGER NOT NULL,
 config TEXT NOT NULL,command TEXT NOT NULL,consumed INTEGER NOT NULL DEFAULT 0 CHECK(consumed IN (0,1)),consumed_by INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS idx_final_actions_outbox ON final_actions(outbox_id);
CREATE INDEX IF NOT EXISTS idx_final_actions_claim ON final_actions(consumed_by);
DROP INDEX IF EXISTS idx_final_actions_consumed;
CREATE INDEX IF NOT EXISTS idx_outbox_final_actions ON outbox(id) WHERE state='done' AND actions_pending=1 AND message_id>0;
CREATE TRIGGER IF NOT EXISTS delete_outbox_final_actions AFTER DELETE ON outbox BEGIN DELETE FROM final_actions WHERE outbox_id=OLD.id; END;`)
	return err
}

// CompleteInboxWithFinalActions registers buttons in the same transaction as root success.
func (s *Store) CompleteInboxWithFinalActions(ctx context.Context, id, chat, thread, targetID int64, header string, replies []string, actions *FinalActionSet) (int64, error) {
	return s.completeInboxWithReplies(ctx, id, chat, thread, InboxDone, replies, targetID, header, true, actions)
}

func registerFinalActions(ctx context.Context, tx *sql.Tx, inboxID, chat, thread int64, set *FinalActionSet) error {
	b := set.Binding
	if b.Bot == 0 || b.Chat != chat || b.Thread != thread || set.User <= 0 || b.Session == "" || set.Config == "" {
		return ErrStaleBinding
	}
	var bound bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM bindings WHERE bot=? AND chat=? AND thread=? AND workspace=? AND session=? AND session_id=? AND generation=?)`, b.Bot, chat, thread, b.Workspace, b.Session, b.SessionID, b.Generation).Scan(&bound); err != nil {
		return err
	}
	if !bound {
		return ErrStaleBinding
	}
	var target int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM outbox WHERE inbox_id=? AND chat=? AND thread=? AND kind='text' AND assistant_body=1 ORDER BY id DESC LIMIT 1", inboxID, chat, thread).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(set.Actions)
	if err != nil {
		return err
	}
	for _, action := range set.Actions {
		if action.Token == "" || action.Command == "" || action.Label == "" {
			return errors.New("invalid final action")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO final_actions(token,outbox_id,bot,chat,thread,user,workspace,session,session_id,generation,config,command) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, action.Token, target, b.Bot, chat, thread, set.User, b.Workspace, b.Session, b.SessionID, b.Generation, set.Config, action.Command); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE outbox SET final_actions=?,actions_pending=CASE WHEN actions_pending=2 THEN 2 ELSE 1 END WHERE id=?", string(encoded), target)
	return err
}

// ReserveFinalAction excludes concurrent clicks without committing command execution.
// A reservation surviving a crash is consumed during initialization, never replayed.
func (s *Store) ReserveFinalAction(ctx context.Context, claim FinalActionClaim) (string, bool, error) {
	if claim.InboxID <= 0 {
		return "", false, errors.New("final action requires a persisted callback")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	b := claim.Binding
	var command string
	err = tx.QueryRowContext(ctx, `SELECT a.command FROM final_actions a
JOIN outbox o ON o.id=a.outbox_id JOIN bindings b ON b.bot=a.bot AND b.chat=a.chat AND b.thread=a.thread
WHERE a.token=? AND a.consumed=0 AND a.consumed_by=0 AND a.config=? AND a.bot=? AND a.chat=? AND a.thread=? AND a.user=?
 AND o.state='done' AND o.message_id=? AND o.message_id>0
 AND a.workspace=? AND a.session=? AND a.session_id=? AND a.generation=?
 AND b.workspace=a.workspace AND b.session=a.session AND b.session_id=a.session_id AND b.generation=a.generation AND b.running=1
 AND NOT EXISTS(SELECT 1 FROM startup_intents pending WHERE pending.bot=a.bot AND pending.chat=a.chat AND pending.thread=a.thread)
 AND EXISTS(SELECT 1 FROM inbox WHERE id=? AND state='pending')
 AND NOT EXISTS(SELECT 1 FROM final_actions used WHERE used.consumed_by=?)`, claim.Token, claim.Config, claim.Bot, claim.Chat, claim.Thread, claim.User, claim.MessageID, b.Workspace, b.Session, b.SessionID, b.Generation, claim.InboxID, claim.InboxID).Scan(&command)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE final_actions SET consumed_by=? WHERE token=? AND consumed=0 AND consumed_by=0", claim.InboxID, claim.Token)
	if err != nil {
		return "", false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return "", false, err
	}
	if changed != 1 {
		return "", false, nil
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return command, true, nil
}

// CheckFinalActionReservation fences deferred qualification before it changes command state.
func (s *Store) CheckFinalActionReservation(ctx context.Context, inboxID int64) error {
	var reserved int
	return s.DB.QueryRowContext(ctx, `SELECT 1 FROM final_actions a JOIN outbox o ON o.id=a.outbox_id WHERE a.consumed=0 AND a.consumed_by=?`, inboxID).Scan(&reserved)
}

// ConsumeFinalAction commits acceptance before any command side effect.
func (s *Store) ConsumeFinalAction(ctx context.Context, inboxID int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := consumeFinalAction(ctx, tx, inboxID); err != nil {
		return err
	}
	return tx.Commit()
}

func consumeFinalAction(ctx context.Context, tx *sql.Tx, inboxID int64) error {
	var token, encoded string
	var outboxID int64
	err := tx.QueryRowContext(ctx, `SELECT a.token,a.outbox_id,o.final_actions FROM final_actions a JOIN outbox o ON o.id=a.outbox_id WHERE a.consumed=0 AND a.consumed_by=?`, inboxID).Scan(&token, &outboxID, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	var actions []FinalAction
	if err := json.Unmarshal([]byte(encoded), &actions); err != nil {
		return err
	}
	remaining := actions[:0]
	for _, action := range actions {
		if action.Token != token {
			remaining = append(remaining, action)
		}
	}
	encoded = ""
	if len(remaining) != 0 {
		data, err := json.Marshal(remaining)
		if err != nil {
			return err
		}
		encoded = string(data)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE final_actions SET consumed=1 WHERE token=? AND consumed=0", token); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE outbox SET final_actions=?,actions_pending=CASE WHEN actions_pending=2 THEN 2 ELSE 1 END WHERE id=?", encoded, outboxID)
	return err
}

// ReleaseFinalAction settles the rejected callback atomically with making its button reusable.
func (s *Store) ReleaseFinalAction(ctx context.Context, inboxID int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE inbox SET state='done',updated_at=? WHERE id=? AND state IN ('pending','submitted') AND EXISTS(SELECT 1 FROM final_actions WHERE consumed=0 AND consumed_by=inbox.id)", time.Now().Unix(), inboxID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE final_actions SET consumed_by=0 WHERE consumed=0 AND consumed_by=?", inboxID); err != nil {
		return err
	}
	return tx.Commit()
}

func recoverFinalActionReservations(tx *sql.Tx) error {
	rows, err := tx.Query("SELECT DISTINCT consumed_by FROM final_actions WHERE consumed=0 AND consumed_by<>0")
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := consumeFinalAction(context.Background(), tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE inbox SET state='cancelled' WHERE id=? AND state='pending'", id); err != nil {
			return err
		}
	}
	return nil
}

// ClaimFinalActionEdit shares the per-message claim with header edits.
func (s *Store) ClaimFinalActionEdit(ctx context.Context) (Output, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Output{}, err
	}
	defer tx.Rollback()
	out, err := scanOutput(tx.QueryRowContext(ctx, "SELECT "+outputColumns+" FROM outbox WHERE state='done' AND actions_pending=1 AND message_id>0 ORDER BY id LIMIT 1"))
	if err != nil {
		return Output{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE outbox SET actions_pending=2,updated_at=? WHERE id=? AND actions_pending=1", time.Now().Unix(), out.ID); err != nil {
		return Output{}, err
	}
	if err := tx.Commit(); err != nil {
		return Output{}, err
	}
	return out, nil
}

// ReleaseAssistantEdit returns an unattempted metadata claim to pending.
func (s *Store) ReleaseAssistantEdit(id int64) error {
	_, err := s.DB.Exec("UPDATE outbox SET actions_pending=1 WHERE id=? AND state='done' AND actions_pending=2", id)
	return err
}

func (s *Store) FinishFinalActionEdit(id int64, snapshot string) error {
	_, err := s.DB.Exec("UPDATE outbox SET actions_pending=CASE WHEN final_actions=? THEN 0 ELSE 1 END WHERE id=? AND state='done' AND actions_pending=2", snapshot, id)
	return err
}

// SettleRejectedFinalAction leaves an already accepted queued action alone on duplicate delivery.
func (s *Store) SettleRejectedFinalAction(ctx context.Context, inboxID int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE inbox SET state='done',updated_at=? WHERE id=? AND state='pending'
AND NOT EXISTS(SELECT 1 FROM final_actions WHERE consumed_by=inbox.id)`, time.Now().Unix(), inboxID)
	return err
}
