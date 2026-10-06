package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestResearchRoundRepliesSurviveRestartWithUncertainOwner(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	requireStoreOK(t, s.Accept(10, []byte(`{"update_id":10}`)))
	requireStoreOK(t, s.Submit(10, 42))
	requireStoreOK(t, s.AppendInboxReplies(context.Background(), 10, 7, 8, []string{"round one"}))
	requireStoreOK(t, s.AppendInboxReplies(context.Background(), 10, 7, 8, []string{"round two"}))
	var state string
	requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state))
	if state != "submitted" {
		t.Fatalf("round output completed owner: %s", state)
	}
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state))
	if state != "uncertain" {
		t.Fatalf("restart did not retire in-flight research: %s", state)
	}
	for _, want := range []string{"round one", "round two"} {
		output, err := s.NextOutput()
		requireStoreOK(t, err)
		if output.Text != want || output.InboxID != 10 || output.ReplyTo != 42 || output.Chat != 7 || output.Thread != 8 {
			t.Fatalf("durable research output = %+v, want %q associated with original input", output, want)
		}
		requireStoreOK(t, s.MarkOutput(output.ID, "sending"))
		requireStoreOK(t, s.MarkOutput(output.ID, "done"))
	}
	if _, err := s.NextOutput(); err != sql.ErrNoRows {
		t.Fatalf("research output duplicated after terminal result: %v", err)
	}
	if err := s.AppendInboxReplies(context.Background(), 10, 7, 8, []string{"late round"}); err == nil {
		t.Fatal("terminal owner accepted a late research round")
	}
}

func TestResearchRoundReplyFailureIsAtomic(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	requireStoreOK(t, s.Accept(10, []byte(`{"update_id":10}`)))
	requireStoreOK(t, s.Submit(10, 42))
	_, err := s.DB.Exec(`CREATE TRIGGER reject_research_reply BEFORE INSERT ON outbox WHEN NEW.text='second' BEGIN SELECT RAISE(FAIL,'injected reply failure'); END`)
	requireStoreOK(t, err)
	if err := s.AppendInboxReplies(context.Background(), 10, 7, 8, []string{"first", "second"}); err == nil {
		t.Fatal("research round ignored a failed reply write")
	}
	var state string
	requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state))
	if state != "submitted" {
		t.Fatalf("failed research round changed owner to %s", state)
	}
	if _, err := s.NextOutput(); err != sql.ErrNoRows {
		t.Fatalf("failed research round left partial deliverable output: %v", err)
	}
}
