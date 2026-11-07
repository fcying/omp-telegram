package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func finalActionFixture(t *testing.T, s *Store) *FinalActionSet {
	t.Helper()
	binding := Binding{Bot: 1, Chat: 7, Thread: 8, Workspace: "/workspace", Session: "/workspace/session.jsonl", SessionID: "session", Generation: 42, Running: true}
	requireStoreOK(t, s.Save(binding))
	requireStoreOK(t, s.Accept(10, []byte(`{}`)))
	requireStoreOK(t, s.Submit(10, 99))
	return &FinalActionSet{Binding: binding, User: 55, Config: "current", BodyParts: 2, Actions: []FinalAction{{Token: "first", Label: "First", Command: "/status"}, {Token: "second", Label: "Second", Command: "/followup later"}}}
}

func claimFixture(set *FinalActionSet, inboxID, messageID int64) FinalActionClaim {
	return FinalActionClaim{Token: "first", Config: set.Config, InboxID: inboxID, Bot: 1, Chat: 7, Thread: 8, User: set.User, MessageID: messageID, Binding: set.Binding}
}

func TestFinalActionsAttachOnlySuccessfulRealTailAcrossSendRace(t *testing.T) {
	for _, stage := range []string{"before", "during", "after"} {
		t.Run(stage, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			ctx := context.Background()
			set := finalActionFixture(t, s)
			first, err := s.AppendAssistantBodyReplies(ctx, 10, 7, 8, []string{"body first split", "body tail", "truncation notice"}, "intermediate", 2)
			requireStoreOK(t, err)
			var count int
			requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM final_actions").Scan(&count))
			if count != 0 {
				t.Fatal("buttons existed before successful settlement")
			}
			settle := func() {
				t.Helper()
				set.BodyParts = 0
				target, err := s.CompleteInboxWithFinalActions(ctx, 10, 7, 8, first, "final", nil, set)
				requireStoreOK(t, err)
				if target != first {
					t.Fatalf("header target changed: %d", target)
				}
			}
			if stage == "before" {
				settle()
			}
			for index := range 3 {
				out, err := s.ClaimNextOutput(ctx, time.Now().Unix())
				requireStoreOK(t, err)
				if (out.FinalActions != "") != (stage == "before" && index == 1) {
					t.Fatalf("unexpected markup on split %d: %+v", index, out)
				}
				if out.AssistantBody != (index < 2) {
					t.Fatalf("notice classified as body: %+v", out)
				}
				if index == 1 && stage == "during" {
					settle()
				}
				requireStoreOK(t, s.MarkOutputDelivered(out.ID, int64(100+index), out.Header, out.FinalActions))
				if index == 1 && stage == "after" {
					settle()
				}
			}
			edit, err := s.ClaimFinalActionEdit(ctx)
			if stage == "before" {
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("sent keyboard scheduled duplicate edit: %+v/%v", edit, err)
				}
			} else {
				requireStoreOK(t, err)
				if edit.ID == first || edit.Text != "body tail" || edit.MessageID != 101 || edit.HeaderEditable || edit.FinalActions == "" {
					t.Fatalf("tail edit lost separate identity: %+v", edit)
				}
				requireStoreOK(t, s.FinishFinalActionEdit(edit.ID, edit.FinalActions))
			}
			if _, err := s.NextOutput(); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("markup edit replayed body: %v", err)
			}
		})
	}
}

func TestFinalActionsClaimIdentityAndCurrentConfiguration(t *testing.T) {
	for _, name := range []string{"bot", "chat", "topic", "user", "message", "config", "session", "generation", "unknown", "submitted", "persisted binding", "closed", "starting"} {
		t.Run(name, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			set := finalActionFixture(t, s)
			_, err := s.CompleteInboxWithFinalActions(context.Background(), 10, 7, 8, 0, "", []string{"first", "last"}, set)
			requireStoreOK(t, err)
			for i := int64(1); i <= 2; i++ {
				out, err := s.ClaimNextOutput(context.Background(), time.Now().Unix())
				requireStoreOK(t, err)
				requireStoreOK(t, s.MarkOutputDelivered(out.ID, 100+i, out.Header, out.FinalActions))
			}
			requireStoreOK(t, s.Accept(20, []byte(`{}`)))
			claim := claimFixture(set, 20, 102)
			switch name {
			case "bot":
				claim.Bot++
			case "chat":
				claim.Chat++
			case "topic":
				claim.Thread++
			case "user":
				claim.User++
			case "message":
				claim.MessageID++
			case "config":
				claim.Config = "changed"
			case "session":
				claim.Binding.Session = "other"
			case "generation":
				claim.Binding.Generation++
			case "unknown":
				claim.Token = "unknown"
			case "submitted":
				requireStoreOK(t, s.Submit(20, 0))
			case "persisted binding":
				changed := set.Binding
				changed.Generation++
				requireStoreOK(t, s.Save(changed))
			case "closed":
				requireStoreOK(t, s.SetRunning(set.Binding, false))
			case "starting":
				requireStoreOK(t, s.PrepareStart(set.Binding, StartIntent{Bot: 1, Chat: 7, Thread: 8, Kind: "new", Workspace: "/workspace", Generation: 43}))
			}
			_, ok, err := s.ReserveFinalAction(context.Background(), claim)
			requireStoreOK(t, err)
			if ok {
				t.Fatal("mismatched callback consumed action")
			}
			var consumed int64
			requireStoreOK(t, s.DB.QueryRow("SELECT consumed_by FROM final_actions WHERE token='first'").Scan(&consumed))
			if consumed != 0 {
				t.Fatalf("rejected identity consumed token: %d", consumed)
			}
		})
	}
}

func TestFinalActionsDoubleClickAndRestartNeverReplay(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	set := finalActionFixture(t, s)
	set.BodyParts = 1
	_, err := s.CompleteInboxWithFinalActions(context.Background(), 10, 7, 8, 0, "", []string{"answer"}, set)
	requireStoreOK(t, err)
	out, err := s.ClaimNextOutput(context.Background(), time.Now().Unix())
	requireStoreOK(t, err)
	requireStoreOK(t, s.MarkOutputDelivered(out.ID, 100, out.Header, out.FinalActions))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	for _, id := range []int64{20, 21, 22} {
		requireStoreOK(t, s.Accept(id, []byte(`{}`)))
	}
	var successes atomic.Int32
	var group sync.WaitGroup
	for _, id := range []int64{20, 21} {
		group.Go(func() {
			_, ok, err := s.ReserveFinalAction(context.Background(), claimFixture(set, id, 100))
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				successes.Add(1)
			}
		})
	}
	group.Wait()
	if successes.Load() != 1 {
		t.Fatalf("double click claimed %d times", successes.Load())
	}
	var state string
	requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=20").Scan(&state))
	if state != "pending" {
		t.Fatalf("claim bypassed normal command lifecycle: %s", state)
	}
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	_, ok, err := s.ReserveFinalAction(context.Background(), claimFixture(set, 20, 100))
	requireStoreOK(t, err)
	if ok {
		t.Fatal("restart replayed consumed action")
	}
	claim := claimFixture(set, 22, 100)
	claim.Token = "second"
	command, ok, err := s.ReserveFinalAction(context.Background(), claim)
	requireStoreOK(t, err)
	if !ok || command != "/followup later" {
		t.Fatalf("separate button lost after restart: %q/%t", command, ok)
	}
}

func TestFinalActionsRegistrationRollsBackWithSettlement(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	set := finalActionFixture(t, s)
	_, err := s.DB.Exec(`CREATE TRIGGER reject_action BEFORE INSERT ON final_actions BEGIN SELECT RAISE(ABORT,'injected'); END`)
	requireStoreOK(t, err)
	if _, err := s.CompleteInboxWithFinalActions(context.Background(), 10, 7, 8, 0, "", []string{"first", "last"}, set); err == nil {
		t.Fatal("failed registration settled result")
	}
	var state string
	var count int
	requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state))
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&count))
	if state != "submitted" || count != 0 {
		t.Fatalf("registration failure was not atomic: %s/%d", state, count)
	}
}

func TestFinalActionsEditSnapshotAndUncertainRestart(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	set := finalActionFixture(t, s)
	first, err := s.AppendAssistantReplies(context.Background(), 10, 7, 8, []string{"answer"}, "")
	requireStoreOK(t, err)
	out, err := s.ClaimNextOutput(context.Background(), time.Now().Unix())
	requireStoreOK(t, err)
	requireStoreOK(t, s.MarkOutputDelivered(out.ID, 100, "", ""))
	set.BodyParts = 0
	_, err = s.CompleteInboxWithFinalActions(context.Background(), 10, 7, 8, first, "final", nil, set)
	requireStoreOK(t, err)
	edit, err := s.ClaimFinalActionEdit(context.Background())
	requireStoreOK(t, err)
	requireStoreOK(t, s.Accept(20, []byte(`{}`)))
	_, ok, err := s.ReserveFinalAction(context.Background(), claimFixture(set, 20, 100))
	requireStoreOK(t, err)
	if !ok {
		t.Fatal("known delivered token could not be claimed")
	}
	requireStoreOK(t, s.ConsumeFinalAction(context.Background(), 20))
	if _, err := s.ClaimFinalActionEdit(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("button consumption released an in-flight edit: %v", err)
	}
	requireStoreOK(t, s.FinishFinalActionEdit(edit.ID, edit.FinalActions))
	newer, err := s.ClaimFinalActionEdit(context.Background())
	requireStoreOK(t, err)
	var buttons []FinalAction
	requireStoreOK(t, json.Unmarshal([]byte(newer.FinalActions), &buttons))
	if len(buttons) != 1 || buttons[0].Token != "second" {
		t.Fatalf("old markup acknowledgment erased newer edit: %+v", buttons)
	}
	// Simulate process exit after reserving an edit, with no Telegram result.
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	if _, err := s.ClaimFinalActionEdit(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("uncertain edit automatically replayed: %v", err)
	}
	if _, err := s.NextOutput(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("uncertain edit replayed body: %v", err)
	}
}

func TestFinalActionConsumptionFailureKeepsReservationAndZeroInboxIsRejected(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	set := finalActionFixture(t, s)
	set.BodyParts = 1
	_, err := s.CompleteInboxWithFinalActions(context.Background(), 10, 7, 8, 0, "", []string{"answer"}, set)
	requireStoreOK(t, err)
	out, err := s.ClaimNextOutput(context.Background(), time.Now().Unix())
	requireStoreOK(t, err)
	requireStoreOK(t, s.MarkOutputDelivered(out.ID, 100, "", out.FinalActions))
	requireStoreOK(t, s.Accept(20, []byte(`{}`)))
	_, ok, err := s.ReserveFinalAction(context.Background(), claimFixture(set, 20, 100))
	requireStoreOK(t, err)
	if !ok {
		t.Fatal("could not reserve action")
	}
	_, err = s.DB.Exec(`CREATE TRIGGER reject_action_claim BEFORE UPDATE OF final_actions ON outbox BEGIN SELECT RAISE(ABORT,'injected'); END`)
	requireStoreOK(t, err)
	if err := s.ConsumeFinalAction(context.Background(), 20); err == nil {
		t.Fatal("failed markup transaction returned successful consumption")
	}
	var consumed bool
	requireStoreOK(t, s.DB.QueryRow("SELECT consumed FROM final_actions WHERE token='first'").Scan(&consumed))
	if consumed {
		t.Fatal("failed claim transaction consumed button")
	}
	_, err = s.DB.Exec("DROP TRIGGER reject_action_claim")
	requireStoreOK(t, err)
	requireStoreOK(t, s.ConsumeFinalAction(context.Background(), 20))
	if _, _, err := s.ReserveFinalAction(context.Background(), claimFixture(set, 0, 100)); err == nil {
		t.Fatal("zero inbox ID bypassed reservation ownership")
	}
}

func TestFinalActionsSchema24UpgradePreservesOutbox(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	requireStoreOK(t, s.Enqueue(7, 8, "existing body"))
	_, err := s.DB.Exec(`DROP TRIGGER delete_outbox_final_actions;
DROP TABLE final_actions;
DROP INDEX idx_outbox_final_actions;
ALTER TABLE outbox DROP COLUMN assistant_body;
ALTER TABLE outbox DROP COLUMN final_actions;
ALTER TABLE outbox DROP COLUMN actions_pending;
PRAGMA user_version=23;`)
	requireStoreOK(t, err)
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	var version int
	requireStoreOK(t, s.DB.QueryRow("PRAGMA user_version").Scan(&version))
	out, err := s.NextOutput()
	requireStoreOK(t, err)
	if version != 24 || out.Text != "existing body" || out.FinalActions != "" || out.AssistantBody {
		t.Fatalf("schema24 changed existing delivery: version=%d output=%+v", version, out)
	}
}

func TestFinalActionReleaseSettlesOriginalCallbackBeforeReuse(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	set := finalActionFixture(t, s)
	set.BodyParts = 1
	_, err := s.CompleteInboxWithFinalActions(context.Background(), 10, 7, 8, 0, "", []string{"answer"}, set)
	requireStoreOK(t, err)
	out, err := s.ClaimNextOutput(context.Background(), time.Now().Unix())
	requireStoreOK(t, err)
	requireStoreOK(t, s.MarkOutputDelivered(out.ID, 100, "", out.FinalActions))
	for _, id := range []int64{20, 21} {
		requireStoreOK(t, s.Accept(id, []byte(`{}`)))
	}
	_, ok, err := s.ReserveFinalAction(context.Background(), claimFixture(set, 20, 100))
	requireStoreOK(t, err)
	if !ok {
		t.Fatal("could not reserve fresh favorite")
	}
	requireStoreOK(t, s.ReleaseFinalAction(context.Background(), 20))
	_, ok, err = s.ReserveFinalAction(context.Background(), claimFixture(set, 20, 100))
	requireStoreOK(t, err)
	if ok {
		t.Fatal("rejected callback update became replayable")
	}
	_, ok, err = s.ReserveFinalAction(context.Background(), claimFixture(set, 21, 100))
	requireStoreOK(t, err)
	if !ok {
		t.Fatal("fresh click could not retry determinate rejection")
	}
	requireStoreOK(t, s.ConsumeFinalAction(context.Background(), 21))
	requireStoreOK(t, s.ReleaseFinalAction(context.Background(), 21))
	requireStoreOK(t, s.Accept(22, []byte(`{}`)))
	_, ok, err = s.ReserveFinalAction(context.Background(), claimFixture(set, 22, 100))
	requireStoreOK(t, err)
	if ok {
		t.Fatal("release revived a committed favorite")
	}
}

func TestFinalActionReservationRetainsSettledSourceUntilRelease(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	set := finalActionFixture(t, s)
	set.BodyParts = 1
	_, err := s.CompleteInboxWithFinalActions(ctx, 10, 7, 8, 0, "", []string{"answer"}, set)
	requireStoreOK(t, err)
	out, err := s.ClaimNextOutput(ctx, time.Now().Unix())
	requireStoreOK(t, err)
	requireStoreOK(t, s.MarkOutputDelivered(out.ID, 100, out.Header, out.FinalActions))
	if _, err := s.ClaimFinalActionEdit(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("fixture markup was not settled: %v", err)
	}
	old := time.Now().AddDate(0, 0, -120).Unix()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	requireStoreOK(t, setMessageTimes(s, old, old, []int64{10}, []int64{out.ID}))
	requireStoreOK(t, s.Accept(20, []byte(`{}`)))
	_, reserved, err := s.ReserveFinalAction(ctx, claimFixture(set, 20, 100))
	requireStoreOK(t, err)
	if !reserved {
		t.Fatal("could not reserve old delivered favorite")
	}
	result, err := s.CleanupMessages(ctx, cutoff)
	requireStoreOK(t, err)
	if result.Outbox != 0 {
		t.Fatalf("cleanup deleted source while qualification was pending: %+v", result)
	}
	requireStoreOK(t, s.CheckFinalActionReservation(ctx, 20))
	requireStoreOK(t, s.ReleaseFinalAction(ctx, 20))
	result, err = s.CleanupMessages(ctx, cutoff)
	requireStoreOK(t, err)
	if result.Outbox != 1 {
		t.Fatalf("released favorite retained its expired source: %+v", result)
	}
	if err := s.ConsumeFinalAction(ctx, 20); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing reservation was accepted for execution: %v", err)
	}
}
