package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

func followupStoreFixture(t *testing.T, s *Store, token string) FollowupRequest {
	t.Helper()
	r := FollowupRequest{Token: token, Instance: "bridge-a", Bot: 99, Chat: -10, Thread: 11, User: 7, TaskID: 3, Workspace: "/workspace", Session: "/session.jsonl", Generation: 1, ExpiresAt: time.Now().Add(10 * time.Minute).Unix()}
	requireStoreOK(t, s.Save(Binding{Bot: r.Bot, Chat: r.Chat, Thread: r.Thread, Workspace: r.Workspace, Session: r.Session, Generation: r.Generation, Running: true}))
	requireStoreOK(t, s.CreateFollowupRequest(r))
	return r
}

func TestFollowupReplacementRetainsCancelledPromptIdentity(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	old := followupStoreFixture(t, s, "old")
	next := old
	next.Token = "next"
	requireStoreOK(t, s.CreateFollowupRequest(next))
	// An old asynchronous send can finish after the replacement was persisted.
	requireStoreOK(t, s.FinishFollowupPrompt(old.Token, old.Instance, 40, true))
	got, err := s.FollowupPrompt(old.Bot, old.Chat, 40)
	requireStoreOK(t, err)
	if got.State != "cancelled" || got.Token != old.Token {
		t.Fatalf("replaced prompt revived: %+v", got)
	}
	requireStoreOK(t, s.FinishFollowupPrompt(next.Token, next.Instance, 41, true))
	requireStoreOK(t, s.CancelFollowupRequests(next.Bot, next.Chat, next.Thread))
	got, err = s.FollowupPrompt(next.Bot, next.Chat, 41)
	requireStoreOK(t, err)
	if got.State != "cancelled" {
		t.Fatalf("cancelled prompt = %+v", got)
	}
}

func TestFollowupUnknownDeliveryNeverBecomesAcceptable(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	r := followupStoreFixture(t, s, "uncertain")
	requireStoreOK(t, s.FinishFollowupPrompt(r.Token, r.Instance, 40, false))
	requireStoreOK(t, s.FinishFollowupPrompt(r.Token, r.Instance, 41, true))
	got, err := s.FollowupRequest(r.Token)
	requireStoreOK(t, err)
	if got.State != "uncertain" || got.PromptID != 0 {
		t.Fatalf("unknown delivery promoted: %+v", got)
	}
	if _, err := s.FollowupPrompt(r.Bot, r.Chat, 41); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("uncertain prompt correlated by guessed ID: %v", err)
	}
}

func TestFollowupConsumeCASAndInboxRecovery(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	requireStoreOK(t, err)
	r := followupStoreFixture(t, s, "claim")
	requireStoreOK(t, s.FinishFollowupPrompt(r.Token, r.Instance, 40, true))
	r, err = s.FollowupRequest(r.Token)
	requireStoreOK(t, err)
	for _, id := range []int64{10, 11} {
		requireStoreOK(t, s.Accept(id, []byte(`{}`)))
	}
	var wg sync.WaitGroup
	results := make(chan int64, 2)
	errors := make(chan error, 2)
	for _, id := range []int64{10, 11} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			accepted, err := s.ConsumeFollowupRequest(r, id)
			if err != nil {
				errors <- err
			} else if accepted {
				results <- id
			}
		}(id)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if len(results) != 1 {
		t.Fatalf("accepted %d replies, want one", len(results))
	}
	acceptedID := <-results
	requireStoreOK(t, s.Close())
	s, err = Open(dir)
	requireStoreOK(t, err)
	t.Cleanup(func() { s.Close() })
	got, err := s.FollowupPrompt(r.Bot, r.Chat, 40)
	requireStoreOK(t, err)
	if got.State != "consumed" || got.InboxID != acceptedID {
		t.Fatalf("consumed correlation lost after restart: %+v", got)
	}
	accepted, err := s.ConsumeFollowupRequest(r, acceptedID)
	requireStoreOK(t, err)
	if accepted {
		t.Fatal("consumed admission replayed after reopen")
	}
	requireStoreOK(t, s.CancelPendingFollowupInbox(acceptedID))
	var state string
	requireStoreOK(t, s.DB.QueryRow(`SELECT state FROM inbox WHERE id=?`, acceptedID).Scan(&state))
	if state != "cancelled" {
		t.Fatalf("interrupted admission state = %s", state)
	}
}

func TestFollowupClaimRequiresCurrentBindingPendingInboxAndExpiry(t *testing.T) {
	for _, reason := range []string{"expired", "binding", "inbox", "owner", "instance", "prompt"} {
		t.Run(reason, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			r := followupStoreFixture(t, s, reason)
			requireStoreOK(t, s.FinishFollowupPrompt(r.Token, r.Instance, 40, true))
			var err error
			r, err = s.FollowupRequest(r.Token)
			requireStoreOK(t, err)
			requireStoreOK(t, s.Accept(10, []byte(`{}`)))
			switch reason {
			case "expired":
				_, err = s.DB.Exec(`UPDATE followup_requests SET expires_at=0 WHERE token=?`, r.Token)
			case "binding":
				_, err = s.DB.Exec(`UPDATE bindings SET generation=generation+1`)
			case "inbox":
				err = s.Mark(10, InboxCancelled)
			case "owner":
				r.User++
			case "instance":
				r.Instance = "bridge-b"
			case "prompt":
				r.PromptID++
			}
			requireStoreOK(t, err)
			accepted, err := s.ConsumeFollowupRequest(r, 10)
			requireStoreOK(t, err)
			if accepted {
				t.Fatal("invalid follow-up was consumed")
			}
			got, err := s.FollowupRequest(r.Token)
			requireStoreOK(t, err)
			if got.State != "delivered" {
				t.Fatalf("rejected claim consumed prompt: %+v", got)
			}
		})
	}
}

func TestCleanupFollowupRequestsRetainsActiveAdmissionsAndValidPrompts(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	base := followupStoreFixture(t, s, "valid-pending")
	now := time.Now().Unix()
	cutoff := now - 3600
	cases := []struct {
		token, state, inbox string
		expires             int64
		removed             bool
	}{
		{"expired-pending", "pending", "", cutoff - 1, true},
		{"expired-delivered", "delivered", "", cutoff - 1, true},
		{"cancelled", "cancelled", "", cutoff - 1, true},
		{"uncertain", "uncertain", "", cutoff - 1, true},
		{"consumed-done", "consumed", "done", cutoff - 1, true},
		{"consumed-cancelled", "consumed", "cancelled", cutoff - 1, true},
		{"consumed-missing", "consumed", "", cutoff - 1, true},
		{"consumed-pending", "consumed", "pending", cutoff - 1, false},
		{"consumed-submitted", "consumed", "submitted", cutoff - 1, false},
		{"recent-expired", "delivered", "", now - 1, false},
		{"cutoff-boundary", "delivered", "", cutoff, false},
		{"valid-delivered", "delivered", "", now + 3600, false},
	}
	for i, tc := range cases {
		r := base
		r.Token, r.State, r.ExpiresAt, r.User = tc.token, tc.state, tc.expires, int64(i+100)
		requireStoreOK(t, s.CreateFollowupRequest(r))
		inboxID := int64(0)
		if tc.state == "consumed" {
			inboxID = int64(i + 10)
		}
		if tc.inbox != "" {
			requireStoreOK(t, s.Accept(inboxID, []byte(`{}`)))
			if tc.inbox != "pending" {
				requireStoreOK(t, s.Mark(inboxID, InboxState(tc.inbox)))
			}
		}
		_, err := s.DB.Exec(`UPDATE followup_requests SET state=?,inbox_id=? WHERE token=?`, tc.state, inboxID, tc.token)
		requireStoreOK(t, err)
	}
	count, err := s.CleanupFollowupRequests(context.Background(), cutoff)
	requireStoreOK(t, err)
	if count != 7 {
		t.Fatalf("removed %d requests, want 7 old settled or expired requests", count)
	}
	for _, tc := range cases {
		_, err := s.FollowupRequest(tc.token)
		if tc.removed {
			if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("old request %s survived cleanup: %v", tc.token, err)
			}
		} else {
			requireStoreOK(t, err)
		}
	}
	// Even a future retention cutoff cannot erase a still-valid request.
	count, err = s.CleanupFollowupRequests(context.Background(), now+7200)
	requireStoreOK(t, err)
	if count != 2 {
		t.Fatalf("future cutoff removed %d requests, want only the two expired retained requests", count)
	}
	for _, token := range []string{base.Token, "valid-delivered", "consumed-pending", "consumed-submitted"} {
		_, err := s.FollowupRequest(token)
		requireStoreOK(t, err)
	}
	for _, token := range []string{"consumed-pending", "consumed-submitted"} {
		r, err := s.FollowupRequest(token)
		requireStoreOK(t, err)
		requireStoreOK(t, s.Mark(r.InboxID, InboxCancelled))
	}
	count, err = s.CleanupFollowupRequests(context.Background(), now+7200)
	requireStoreOK(t, err)
	if count != 2 {
		t.Fatalf("settled admissions not released for cleanup: %d", count)
	}
	binding, err := s.Binding(base.Bot, base.Chat, base.Thread)
	requireStoreOK(t, err)
	if !binding.Running || binding.Workspace != base.Workspace || binding.Session != base.Session {
		t.Fatalf("request cleanup changed session binding: %+v", binding)
	}
}

func TestCleanupFollowupRequestsUsesBoundedBatches(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	base := followupStoreFixture(t, s, "live")
	cutoff := time.Now().Unix() - 3600
	tx, err := s.DB.Begin()
	requireStoreOK(t, err)
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO followup_requests(token,instance,bot,chat,thread,user,task_id,workspace,session,generation,expires_at,state)
SELECT ?,instance,bot,chat,thread,user,task_id,workspace,session,generation,?,'pending' FROM followup_requests WHERE token=?`)
	requireStoreOK(t, err)
	for i := range 1001 {
		_, err = stmt.Exec("expired-"+strconv.Itoa(i), cutoff-1, base.Token)
		requireStoreOK(t, err)
	}
	requireStoreOK(t, stmt.Close())
	requireStoreOK(t, tx.Commit())
	for _, want := range []int64{1000, 1, 0} {
		count, err := s.CleanupFollowupRequests(context.Background(), cutoff)
		requireStoreOK(t, err)
		if count != want {
			t.Fatalf("batch removed %d requests, want %d", count, want)
		}
	}
	_, err = s.FollowupRequest(base.Token)
	requireStoreOK(t, err)
}
