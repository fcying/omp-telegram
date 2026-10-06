package store

import (
	"database/sql"
	"errors"
	"strings"
)

const workspaceLeaseSchema = `
CREATE TABLE IF NOT EXISTS workspace_users(root TEXT NOT NULL,bot INTEGER NOT NULL,chat INTEGER NOT NULL,thread INTEGER NOT NULL,session TEXT NOT NULL,PRIMARY KEY(root,bot,chat,thread,session));
CREATE TABLE IF NOT EXISTS workspace_leases(root TEXT NOT NULL,bot INTEGER NOT NULL,chat INTEGER NOT NULL,thread INTEGER NOT NULL,session TEXT NOT NULL,PRIMARY KEY(root,bot,chat,thread,session));`

var ErrWorkspaceOccupied = errors.New("workspace is occupied by another logical session; close the other conversation or confirm autoresearch off in its owning session")

// WorkspaceOwner identifies a durable logical session, never a runtime or PID.
type WorkspaceOwner struct {
	Bot, Chat, Thread int64
	Session           string
}

// WorkspaceRegistration records a physical root and its durable logical owner.
type WorkspaceRegistration struct {
	Root  string
	Owner WorkspaceOwner
}

// WorkspaceRegistrations supplies the persisted sources for a physical identity
// refresh. Callers serialize the snapshot, resolution and refresh with admission.
func (s *Store) WorkspaceRegistrations() (users, leases []WorkspaceRegistration, err error) {
	for _, table := range []struct {
		name string
		out  *[]WorkspaceRegistration
	}{{"workspace_users", &users}, {"workspace_leases", &leases}} {
		rows, queryErr := s.DB.Query("SELECT root,bot,chat,thread,session FROM " + table.name)
		if queryErr != nil {
			return nil, nil, queryErr
		}
		for rows.Next() {
			var registration WorkspaceRegistration
			if err = rows.Scan(&registration.Root, &registration.Owner.Bot, &registration.Owner.Chat, &registration.Owner.Thread, &registration.Owner.Session); err != nil {
				rows.Close()
				return nil, nil, err
			}
			*table.out = append(*table.out, registration)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	return users, leases, nil
}

// RefreshWorkspaceRegistrations replaces normal registrations atomically and
// extends leases to their current physical identities. Historical lease roots
// are retained until the exact native owner explicitly confirms off or clear.
// A topology change can merge existing owners. Persist all of them; admission
// checks every lease and must fail closed until exact owners confirm disable.
func (s *Store) RefreshWorkspaceRegistrations(users, leases []WorkspaceRegistration) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, registration := range leases {
		if _, err = tx.Exec("INSERT OR IGNORE INTO workspace_leases(root,bot,chat,thread,session) VALUES(?,?,?,?,?)", registration.Root, registration.Owner.Bot, registration.Owner.Chat, registration.Owner.Thread, registration.Owner.Session); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM workspace_users"); err != nil {
		return err
	}
	for _, registration := range users {
		if _, err = tx.Exec("INSERT OR IGNORE INTO workspace_users(root,bot,chat,thread,session) VALUES(?,?,?,?,?)", registration.Root, registration.Owner.Bot, registration.Owner.Chat, registration.Owner.Thread, registration.Owner.Session); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (o WorkspaceOwner) sameTopic(other WorkspaceOwner) bool {
	return o.Bot == other.Bot && o.Chat == other.Chat && o.Thread == other.Thread
}

func (o WorkspaceOwner) matches(other WorkspaceOwner) bool {
	return o.sameTopic(other) && strings.EqualFold(o.Session, other.Session)
}

func checkWorkspaceLeases(tx *sql.Tx, root string, owner WorkspaceOwner, allowPrefix bool) error {
	rows, err := tx.Query("SELECT bot,chat,thread,session FROM workspace_leases WHERE root=?", root)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var lease WorkspaceOwner
		if err = rows.Scan(&lease.Bot, &lease.Chat, &lease.Thread, &lease.Session); err != nil {
			return err
		}
		if lease.matches(owner) {
			continue
		}
		// Prefixes only reserve an explicit resume; native metadata must still
		// establish the exact identity before user work or lease release.
		if !allowPrefix || !lease.sameTopic(owner) || len(owner.Session) < 8 || !strings.HasPrefix(strings.ToLower(lease.Session), strings.ToLower(owner.Session)) {
			return ErrWorkspaceOccupied
		}
	}
	return rows.Err()
}

func admitWorkspace(tx *sql.Tx, root string, owner WorkspaceOwner) error {
	if err := checkWorkspace(tx, root, owner); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT OR IGNORE INTO workspace_users(root,bot,chat,thread,session) VALUES(?,?,?,?,?)", root, owner.Bot, owner.Chat, owner.Thread, owner.Session)
	return err
}

func checkWorkspace(tx *sql.Tx, root string, owner WorkspaceOwner) error {
	if root == "" || owner.Session == "" {
		return errors.New("workspace identity is unavailable")
	}
	return checkWorkspaceLeases(tx, root, owner, true)
}

func (s *Store) CheckWorkspace(root string, owner WorkspaceOwner) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkWorkspace(tx, root, owner); err != nil {
		return err
	}
	return tx.Commit()
}

// AdmitWorkspace atomically checks exclusive ownership and registers a normal
// logical user. Idle runtime release must not remove this registration.
func (s *Store) AdmitWorkspace(root string, owner WorkspaceOwner) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = admitWorkspace(tx, root, owner); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClaimResearchWorkspace(root string, owner WorkspaceOwner) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = admitWorkspace(tx, root, owner); err != nil {
		return err
	}
	if err = checkWorkspaceLeases(tx, root, owner, false); err != nil {
		return err
	}
	var occupied bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM workspace_users WHERE root=? AND NOT(bot=? AND chat=? AND thread=?))", root, owner.Bot, owner.Chat, owner.Thread).Scan(&occupied); err != nil {
		return err
	}
	if occupied {
		return ErrWorkspaceOccupied
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO workspace_leases(root,bot,chat,thread,session) VALUES(?,?,?,?,?)", root, owner.Bot, owner.Chat, owner.Thread, owner.Session); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseResearchWorkspace only releases the exact session's leases, including
// historical roots. Callers must have confirmed off and no outstanding
// destructive research operations.
func (s *Store) ReleaseResearchWorkspace(owner WorkspaceOwner) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM workspace_leases WHERE bot=? AND chat=? AND thread=? AND session=? COLLATE NOCASE", owner.Bot, owner.Chat, owner.Thread, owner.Session); err != nil {
		return err
	}
	return tx.Commit()
}

// CloseWorkspaceUsers does not touch exclusive leases, including an old
// session's lease after /close or an uncertain disable.
func (s *Store) CloseWorkspaceUsers(owner WorkspaceOwner) error {
	_, err := s.DB.Exec("DELETE FROM workspace_users WHERE bot=? AND chat=? AND thread=?", owner.Bot, owner.Chat, owner.Thread)
	return err
}

// CheckWorkspaceSession prevents a topic replacing a leased session even when
// the new session selects a different workspace. Explicit resume is the only
// recovery path for the old lease; a fresh session must never release it.
func (s *Store) CheckWorkspaceSession(owner WorkspaceOwner) error {
	rows, err := s.DB.Query("SELECT session FROM workspace_leases WHERE bot=? AND chat=? AND thread=?", owner.Bot, owner.Chat, owner.Thread)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var session string
		if err = rows.Scan(&session); err != nil {
			return err
		}
		if len(owner.Session) < 8 || !strings.HasPrefix(strings.ToLower(session), strings.ToLower(owner.Session)) {
			return ErrWorkspaceOccupied
		}
	}
	return rows.Err()
}

// ResearchTopicLeased includes reservations retained after the conversation closes.
func (s *Store) ResearchTopicLeased(bot, chat, thread int64) (bool, error) {
	var leased bool
	err := s.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM workspace_leases WHERE bot=? AND chat=? AND thread=?)", bot, chat, thread).Scan(&leased)
	return leased, err
}

// ResearchWorkspaceOwned requires the full native identity, never a prefix.
// It authorizes disable-only recovery, not ordinary workspace admission.
func (s *Store) ResearchWorkspaceOwned(owner WorkspaceOwner) (bool, error) {
	var owned bool
	err := s.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM workspace_leases WHERE bot=? AND chat=? AND thread=? AND session=? COLLATE NOCASE)", owner.Bot, owner.Chat, owner.Thread, owner.Session).Scan(&owned)
	return owned, err
}

func (s *Store) ResearchSessionLeased(bot int64, id string) (bool, error) {
	rows, err := s.DB.Query("SELECT session FROM workspace_leases WHERE bot=?", bot)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	id = strings.ToLower(id)
	for rows.Next() {
		var session string
		if err = rows.Scan(&session); err != nil {
			return false, err
		}
		session = strings.ToLower(session)
		if session == id || strings.HasPrefix(session, id) || strings.HasPrefix(id, session) {
			return true, nil
		}
	}
	return false, rows.Err()
}
