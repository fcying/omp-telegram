package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// FollowupRequest retains correlation metadata only, including after cancellation.
// Prompt contents and model/user text are deliberately not duplicated here.
type FollowupRequest struct {
	Token, Instance           string
	Bot, Chat, Thread, User   int64
	PromptID, TaskID, InboxID int64
	Workspace, Session        string
	Generation, ExpiresAt     int64
	State                     string
}

func createFollowupSchema(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS followup_requests(
 token TEXT PRIMARY KEY,instance TEXT NOT NULL,bot INTEGER NOT NULL,chat INTEGER NOT NULL,thread INTEGER NOT NULL,user INTEGER NOT NULL,
 prompt_id INTEGER NOT NULL DEFAULT 0,task_id INTEGER NOT NULL,inbox_id INTEGER NOT NULL DEFAULT 0,
 workspace TEXT NOT NULL,session TEXT NOT NULL,generation INTEGER NOT NULL,expires_at INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','delivered','cancelled','consumed','uncertain')));
CREATE INDEX IF NOT EXISTS idx_followup_owner ON followup_requests(bot,chat,thread,user,state);
CREATE UNIQUE INDEX IF NOT EXISTS idx_followup_prompt ON followup_requests(bot,chat,prompt_id) WHERE prompt_id>0;
CREATE UNIQUE INDEX IF NOT EXISTS idx_followup_inbox ON followup_requests(inbox_id) WHERE inbox_id>0;
CREATE INDEX IF NOT EXISTS idx_followup_expiry ON followup_requests(expires_at);`)
	return err
}

// CreateFollowupRequest replaces the user's previous request before any send.
func (s *Store) CreateFollowupRequest(r FollowupRequest) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE followup_requests SET state='cancelled' WHERE bot=? AND chat=? AND thread=? AND user=? AND state IN ('pending','delivered')`, r.Bot, r.Chat, r.Thread, r.User); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO followup_requests(token,instance,bot,chat,thread,user,task_id,workspace,session,generation,expires_at,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,'pending')`, r.Token, r.Instance, r.Bot, r.Chat, r.Thread, r.User, r.TaskID, r.Workspace, r.Session, r.Generation, r.ExpiresAt); err != nil {
		return err
	}
	return tx.Commit()
}

const followupColumns = `token,instance,bot,chat,thread,user,prompt_id,task_id,inbox_id,workspace,session,generation,expires_at,state`

func scanFollowup(row *sql.Row) (FollowupRequest, error) {
	var r FollowupRequest
	err := row.Scan(&r.Token, &r.Instance, &r.Bot, &r.Chat, &r.Thread, &r.User, &r.PromptID, &r.TaskID, &r.InboxID, &r.Workspace, &r.Session, &r.Generation, &r.ExpiresAt, &r.State)
	return r, err
}

func (s *Store) FollowupRequest(token string) (FollowupRequest, error) {
	return scanFollowup(s.DB.QueryRow(`SELECT `+followupColumns+` FROM followup_requests WHERE token=?`, token))
}

// FollowupPrompt recognizes old replies even if Telegram omits the prompt text.
func (s *Store) FollowupPrompt(bot, chat, messageID int64) (FollowupRequest, error) {
	return scanFollowup(s.DB.QueryRow(`SELECT `+followupColumns+` FROM followup_requests WHERE bot=? AND chat=? AND prompt_id=? AND prompt_id>0`, bot, chat, messageID))
}

// FinishFollowupPrompt never promotes a cancelled/replaced request. A send with
// unknown delivery has no usable prompt ID and is never retried automatically.
func (s *Store) FinishFollowupPrompt(token, instance string, messageID int64, delivered bool) error {
	state := "uncertain"
	if delivered && messageID > 0 {
		state = "delivered"
	} else {
		messageID = 0
	}
	_, err := s.DB.Exec(`UPDATE followup_requests SET state=CASE WHEN state='pending' THEN ? ELSE state END,prompt_id=CASE WHEN ?>0 THEN ? ELSE prompt_id END WHERE token=? AND instance=? AND state IN ('pending','cancelled')`, state, messageID, messageID, token, instance)
	return err
}

func (s *Store) CancelFollowupRequests(bot, chat, thread int64) error {
	_, err := s.DB.Exec(`UPDATE followup_requests SET state='cancelled' WHERE bot=? AND chat=? AND thread=? AND state IN ('pending','delivered')`, bot, chat, thread)
	return err
}

// CancelFollowupRequest revokes only this pending or delivered input, never queued work.
func (s *Store) CancelFollowupRequest(r FollowupRequest) (bool, error) {
	result, err := s.DB.Exec(`UPDATE followup_requests SET state='cancelled'
WHERE token=? AND instance=? AND prompt_id=? AND user=? AND bot=? AND chat=? AND thread=?
AND generation=? AND workspace=? AND session=? AND state IN ('pending','delivered') AND expires_at>?
AND EXISTS(SELECT 1 FROM bindings WHERE bot=? AND chat=? AND thread=? AND generation=? AND workspace=? AND session=? AND running=1)`,
		r.Token, r.Instance, r.PromptID, r.User, r.Bot, r.Chat, r.Thread, r.Generation, r.Workspace, r.Session, time.Now().Unix(),
		r.Bot, r.Chat, r.Thread, r.Generation, r.Workspace, r.Session)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// ConsumeFollowupRequest claims exactly one pending inbox admission. Leaving its
// inbox pending lets the existing worker Submit and crash cancellation own the
// normal queue lifecycle; the durable consumed token prevents any replay.
func (s *Store) ConsumeFollowupRequest(r FollowupRequest, inboxID int64) (bool, error) {
	if inboxID <= 0 {
		return false, nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE followup_requests SET state='consumed',inbox_id=?
WHERE token=? AND instance=? AND state='delivered' AND prompt_id=? AND user=? AND bot=? AND chat=? AND thread=? AND generation=? AND workspace=? AND session=? AND expires_at>?
AND EXISTS(SELECT 1 FROM inbox WHERE id=? AND state='pending')
AND EXISTS(SELECT 1 FROM bindings WHERE bot=? AND chat=? AND thread=? AND generation=? AND workspace=? AND session=? AND running=1)`,
		inboxID, r.Token, r.Instance, r.PromptID, r.User, r.Bot, r.Chat, r.Thread, r.Generation, r.Workspace, r.Session, time.Now().Unix(), inboxID, r.Bot, r.Chat, r.Thread, r.Generation, r.Workspace, r.Session)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	if n != 1 {
		return false, errors.New("invalid follow-up claim count")
	}
	return true, tx.Commit()
}

// CancelPendingFollowupInbox settles a consumed admission interrupted before
// append without changing an already submitted or terminal inbox on duplicate delivery.
func (s *Store) CancelPendingFollowupInbox(inboxID int64) error {
	_, err := s.DB.Exec(`UPDATE inbox SET state='cancelled',updated_at=? WHERE id=? AND state='pending'`, time.Now().Unix(), inboxID)
	return err
}

// CleanupFollowupRequests removes one bounded batch using the message retention
// cutoff. Expired unread prompts need no separate tombstone: their bot marker
// still rejects late replies after deletion. Active consumed admissions retain
// their correlation until inbox recovery or submission has settled.
func (s *Store) CleanupFollowupRequests(ctx context.Context, cutoff int64) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM followup_requests WHERE token IN (
SELECT token FROM followup_requests
WHERE expires_at<? AND expires_at<=?
AND NOT EXISTS (SELECT 1 FROM inbox WHERE id=followup_requests.inbox_id AND state IN ('pending','submitted'))
ORDER BY expires_at,token LIMIT ?)`, cutoff, time.Now().Unix(), messageCleanupBatchSize)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return count, tx.Commit()
}
