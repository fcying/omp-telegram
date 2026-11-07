package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestAssistantCompletionAddsAndRemovesHeaderAroundSend(t *testing.T) {
	for _, transition := range []struct {
		name, initial, final string
	}{
		{"add", "", "final"},
		{"remove", "initial", ""},
	} {
		for _, stage := range []string{"before", "during", "after"} {
			t.Run(transition.name+"/"+stage, func(t *testing.T) {
				s := openTestStore(t, t.TempDir())
				ctx := context.Background()
				requireStoreOK(t, s.Accept(10, []byte(`{}`)))
				requireStoreOK(t, s.Submit(10, 42))
				id, err := s.AppendAssistantReplies(ctx, 10, 7, 8, []string{"immutable body"}, transition.initial)
				requireStoreOK(t, err)
				complete := func() {
					t.Helper()
					target, err := s.CompleteInboxWithAssistantHeader(ctx, 10, 7, 8, id, transition.final, nil)
					requireStoreOK(t, err)
					if target != id {
						t.Fatalf("completion lost first body target: %d, want %d", target, id)
					}
				}
				if stage == "before" {
					complete()
				}
				out, err := s.ClaimNextOutput(ctx, time.Now().Unix())
				requireStoreOK(t, err)
				want := transition.initial
				if stage == "before" {
					want = transition.final
				}
				if out.ID != id || out.Header != want || !out.HeaderEditable || out.Text != "immutable body" {
					t.Fatalf("send snapshot = %+v", out)
				}
				if stage == "during" {
					complete()
				}
				requireStoreOK(t, s.MarkOutputDelivered(id, 100, out.Header, ""))
				if stage == "after" {
					complete()
				}
				edit, err := s.NextAssistantHeader()
				if stage == "before" {
					if !errors.Is(err, sql.ErrNoRows) {
						t.Fatalf("latest sent header remained pending: %+v, %v", edit, err)
					}
				} else {
					requireStoreOK(t, err)
					if edit.ID != id || edit.Header != transition.final || edit.Text != "immutable body" || !edit.HeaderEditable || edit.MessageID != 100 {
						t.Fatalf("final header-only edit = %+v", edit)
					}
					requireStoreOK(t, s.MarkAssistantHeaderDelivered(id, edit.Header, ""))
				}
				var count int
				var state string
				requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&count))
				requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state))
				if count != 1 || state != "done" {
					t.Fatalf("completion created output or failed settlement: %d/%q", count, state)
				}
				if _, err := s.NextOutput(); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("completion replayed body: %v", err)
				}
			})
		}
	}
}

func TestHeaderlessFinalAssistantRetainsFirstNewTarget(t *testing.T) {
	for _, prior := range []bool{false, true} {
		t.Run(strconv.FormatBool(prior), func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			ctx := context.Background()
			requireStoreOK(t, s.Accept(10, []byte(`{}`)))
			requireStoreOK(t, s.Submit(10, 42))
			var priorID int64
			if prior {
				var err error
				priorID, err = s.AppendAssistantReplies(ctx, 10, 7, 8, []string{"intermediate"}, "")
				requireStoreOK(t, err)
				out, err := s.ClaimNextOutput(ctx, time.Now().Unix())
				requireStoreOK(t, err)
				requireStoreOK(t, s.MarkOutputDelivered(out.ID, 100, out.Header, ""))
			}
			target, err := s.CompleteInboxWithAssistantHeader(ctx, 10, 7, 8, 0, "", []string{"final body", "final continuation"})
			requireStoreOK(t, err)
			if target == 0 || target == priorID {
				t.Fatalf("headerless final target = %d, prior = %d", target, priorID)
			}
			for i, body := range []string{"final body", "final continuation"} {
				out, err := s.ClaimNextOutput(ctx, time.Now().Unix())
				requireStoreOK(t, err)
				var replyTo int64
				if i == 0 && !prior {
					replyTo = 42
				}
				if out.Text != body || out.Header != "" || out.HeaderEditable != (i == 0) || out.ReplyTo != replyTo || (i == 0 && out.ID != target) {
					t.Fatalf("headerless final part %d = %+v", i, out)
				}
				requireStoreOK(t, s.MarkOutputDelivered(out.ID, out.ID+100, out.Header, ""))
				if i != 0 {
					if err := s.SetAssistantHeader(ctx, out.ID, "not first"); !errors.Is(err, sql.ErrNoRows) {
						t.Fatalf("final continuation accepted header: %v", err)
					}
				}
			}
			requireStoreOK(t, s.SetAssistantHeader(ctx, target, "late final"))
			edit, err := s.NextAssistantHeader()
			requireStoreOK(t, err)
			if edit.ID != target || edit.Text != "final body" || edit.Header != "late final" {
				t.Fatalf("late final header lost first target: %+v", edit)
			}
		})
	}
}

func TestPlainReplyAPIsKeepHeadersNoneditable(t *testing.T) {
	for _, kind := range []string{"append", "done", "uncertain", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			ctx := context.Background()
			requireStoreOK(t, s.Accept(10, []byte(`{}`)))
			requireStoreOK(t, s.Submit(10, 42))
			replies := []string{"ordinary first", "ordinary second"}
			var err error
			switch kind {
			case "append":
				err = s.AppendInboxReplies(ctx, 10, 7, 8, replies)
			case "done":
				err = s.CompleteInboxWithReplies(ctx, 10, 7, 8, replies)
			case "uncertain":
				err = s.CompleteInboxUncertainWithReplies(ctx, 10, 7, 8, replies)
			case "cancelled":
				err = s.CompleteInboxCancelledWithReplies(ctx, 10, 7, 8, replies)
			}
			requireStoreOK(t, err)
			for _, body := range replies {
				out, err := s.ClaimNextOutput(ctx, time.Now().Unix())
				requireStoreOK(t, err)
				if out.Text != body || out.Header != "" || out.HeaderEditable {
					t.Fatalf("plain reply acquired editable header: %+v", out)
				}
				for _, header := range []string{"injected", ""} {
					if err := s.SetAssistantHeader(ctx, out.ID, header); !errors.Is(err, sql.ErrNoRows) {
						t.Fatalf("plain reply accepted header %q: %v", header, err)
					}
				}
				if kind == "append" {
					target, err := s.CompleteInboxWithAssistantHeader(ctx, 10, 7, 8, out.ID, "injected", []string{"must roll back"})
					if !errors.Is(err, sql.ErrNoRows) || target != 0 {
						t.Fatalf("completion accepted ordinary owned target: %d/%v", target, err)
					}
					var state string
					var count int
					requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state))
					requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&count))
					if state != "submitted" || count != 2 {
						t.Fatalf("rejected ordinary target did not roll back: %q/%d", state, count)
					}
				}
				requireStoreOK(t, s.MarkOutputDelivered(out.ID, out.ID+100, "", ""))
			}
			if _, err := s.NextAssistantHeader(); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("plain reply acquired pending header: %v", err)
			}
		})
	}
}

func TestEmptyAssistantHeaderSnapshotRecoveryAndRetention(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	ctx := context.Background()
	requireStoreOK(t, s.Accept(10, []byte(`{}`)))
	requireStoreOK(t, s.Submit(10, 42))
	id, err := s.AppendAssistantReplies(ctx, 10, 7, 8, []string{"immutable body"}, "initial")
	requireStoreOK(t, err)
	out, err := s.ClaimNextOutput(ctx, time.Now().Unix())
	requireStoreOK(t, err)
	requireStoreOK(t, s.MarkOutputDelivered(id, 100, out.Header, ""))
	requireStoreOK(t, s.SetAssistantHeader(ctx, id, ""))
	removal, err := s.NextAssistantHeader()
	requireStoreOK(t, err)
	if removal.Header != "" {
		t.Fatalf("removal snapshot = %+v", removal)
	}
	requireStoreOK(t, s.SetAssistantHeader(ctx, id, "added again"))
	requireStoreOK(t, s.MarkAssistantHeaderDelivered(id, removal.Header, ""))
	addition, err := s.NextAssistantHeader()
	requireStoreOK(t, err)
	if addition.Header != "added again" {
		t.Fatalf("empty snapshot cleared new addition: %+v", addition)
	}
	requireStoreOK(t, s.SetAssistantHeader(ctx, id, ""))
	requireStoreOK(t, s.MarkAssistantHeaderDelivered(id, addition.Header, ""))
	requireStoreOK(t, s.FinishSubmitted(10, InboxDone))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	removal, err = s.NextAssistantHeader()
	requireStoreOK(t, err)
	if removal.ID != id || removal.Header != "" || !removal.HeaderEditable || removal.MessageID != 100 || removal.Text != "immutable body" {
		t.Fatalf("recovery lost pending removal: %+v", removal)
	}
	old := time.Now().AddDate(0, 0, -91).Unix()
	requireStoreOK(t, setMessageTimes(s, old, old, []int64{10}, []int64{id}))
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	result, err := s.CleanupMessages(ctx, cutoff)
	requireStoreOK(t, err)
	if result.Outbox != 0 {
		t.Fatalf("retention discarded empty header edit: %+v", result)
	}
	requireStoreOK(t, s.MarkAssistantHeaderDelivered(id, removal.Header, ""))
	if _, err := s.NextOutput(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("recovered header removal replayed body: %v", err)
	}
	result, err = s.CleanupMessages(ctx, cutoff)
	requireStoreOK(t, err)
	if result.Outbox != 1 {
		t.Fatalf("settled removal did not release retained body: %+v", result)
	}
}

func TestHeaderEditabilityMigrationFromVersion22(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	ctx := context.Background()
	requireStoreOK(t, s.Accept(10, []byte(`{}`)))
	requireStoreOK(t, s.Submit(10, 42))
	id, err := s.AppendAssistantReplies(ctx, 10, 7, 8, []string{"legacy assistant", "legacy continuation"}, "legacy header")
	requireStoreOK(t, err)
	out, err := s.ClaimNextOutput(ctx, time.Now().Unix())
	requireStoreOK(t, err)
	requireStoreOK(t, s.MarkOutputDelivered(id, 100, out.Header, ""))
	requireStoreOK(t, s.SetAssistantHeader(ctx, id, "pending legacy header"))
	requireStoreOK(t, s.Enqueue(7, 8, "ordinary legacy text"))
	_, err = s.DB.Exec(`DROP INDEX idx_outbox_assistant_header;
ALTER TABLE outbox DROP COLUMN header_editable;
CREATE INDEX idx_outbox_assistant_header ON outbox(id) WHERE state='done' AND header_pending=1 AND message_id>0 AND header<>'';
PRAGMA user_version=22;`)
	requireStoreOK(t, err)
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	var version, editableCount int
	requireStoreOK(t, s.DB.QueryRow("PRAGMA user_version").Scan(&version))
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE header_editable=1").Scan(&editableCount))
	if version != schemaVersion || editableCount != 1 {
		t.Fatalf("v22 upgrade version/editable rows = %d/%d", version, editableCount)
	}
	edit, err := s.NextAssistantHeader()
	requireStoreOK(t, err)
	if edit.ID != id || !edit.HeaderEditable || edit.Header != "pending legacy header" || edit.MessageID != 100 {
		t.Fatalf("migration lost legacy header target: %+v", edit)
	}
	requireStoreOK(t, s.SetAssistantHeader(ctx, id, ""))
	requireStoreOK(t, s.MarkAssistantHeaderDelivered(id, edit.Header, ""))
	edit, err = s.NextAssistantHeader()
	requireStoreOK(t, err)
	if edit.ID != id || edit.Header != "" || edit.Text != "legacy assistant" {
		t.Fatalf("migrated target cannot remove header: %+v", edit)
	}
	for _, body := range []string{"legacy continuation", "ordinary legacy text"} {
		out, err := s.ClaimNextOutput(ctx, time.Now().Unix())
		requireStoreOK(t, err)
		if out.Text != body || out.HeaderEditable {
			t.Fatalf("migration made ordinary row editable: %+v", out)
		}
		if err := s.SetAssistantHeader(ctx, out.ID, "not assistant"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("migrated ordinary row accepted header: %v", err)
		}
		requireStoreOK(t, s.MarkOutputDelivered(out.ID, out.ID+100, "", ""))
	}
}

func TestEmptyAssistantHeaderTransactionsRollBack(t *testing.T) {
	for _, transition := range []struct {
		name, initial, final string
	}{
		{"add", "", "final"},
		{"remove", "initial", ""},
	} {
		t.Run(transition.name, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			ctx := context.Background()
			requireStoreOK(t, s.Accept(10, []byte(`{}`)))
			requireStoreOK(t, s.Submit(10, 42))
			_, err := s.DB.Exec(`CREATE TRIGGER reject_empty_assistant_part BEFORE INSERT ON outbox WHEN NEW.text='rejected' BEGIN SELECT RAISE(FAIL,'injected body failure'); END`)
			requireStoreOK(t, err)
			if id, err := s.AppendAssistantReplies(ctx, 10, 7, 8, []string{"first", "rejected"}, ""); err == nil || id != 0 {
				t.Fatalf("failed headerless append returned success/ID: %d/%v", id, err)
			}
			if id, err := s.CompleteInboxWithAssistantHeader(ctx, 10, 7, 8, 0, "", []string{"first", "rejected"}); err == nil || id != 0 {
				t.Fatalf("failed headerless final returned success/ID: %d/%v", id, err)
			}
			var count int
			requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&count))
			if count != 0 {
				t.Fatalf("failed headerless transactions left %d rows", count)
			}
			id, err := s.AppendAssistantReplies(ctx, 10, 7, 8, []string{"immutable body"}, transition.initial)
			requireStoreOK(t, err)
			out, err := s.ClaimNextOutput(ctx, time.Now().Unix())
			requireStoreOK(t, err)
			requireStoreOK(t, s.MarkOutputDelivered(id, 100, out.Header, ""))
			_, err = s.DB.Exec(`CREATE TRIGGER reject_empty_assistant_header BEFORE UPDATE OF header ON outbox BEGIN SELECT RAISE(FAIL,'injected header failure'); END`)
			requireStoreOK(t, err)
			if target, err := s.CompleteInboxWithAssistantHeader(ctx, 10, 7, 8, id, transition.final, []string{"must roll back"}); err == nil || target != 0 {
				t.Fatalf("failed header mutation returned success/ID: %d/%v", target, err)
			}
			var state, header, body string
			var editable bool
			var pending int
			requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&count))
			requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state))
			requireStoreOK(t, s.DB.QueryRow("SELECT text,header,header_editable,header_pending FROM outbox WHERE id=?", id).Scan(&body, &header, &editable, &pending))
			if count != 1 || state != "submitted" || body != "immutable body" || header != transition.initial || !editable || pending != 0 {
				t.Fatalf("failed transaction changed durable target: %d/%q/%q/%q/%t/%d", count, state, body, header, editable, pending)
			}
			_, err = s.DB.Exec("DROP TRIGGER reject_empty_assistant_header")
			requireStoreOK(t, err)
			target, err := s.CompleteInboxWithAssistantHeader(ctx, 10, 7, 8, id, transition.final, nil)
			requireStoreOK(t, err)
			if target != id {
				t.Fatalf("successful retry lost target: %d, want %d", target, id)
			}
			edit, err := s.NextAssistantHeader()
			requireStoreOK(t, err)
			if edit.ID != id || edit.Header != transition.final || edit.Text != "immutable body" {
				t.Fatalf("successful retry lost header mutation: %+v", edit)
			}
		})
	}
}

func TestHeaderEditabilityMigrationRollsBackOnFailure(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	requireStoreOK(t, s.Accept(10, []byte(`{}`)))
	requireStoreOK(t, s.Submit(10, 42))
	id, err := s.AppendAssistantReplies(ctx, 10, 7, 8, []string{"legacy body"}, "legacy header")
	requireStoreOK(t, err)
	_, err = s.DB.Exec(`DROP INDEX idx_outbox_assistant_header;
ALTER TABLE outbox DROP COLUMN header_editable;
PRAGMA user_version=22;`)
	requireStoreOK(t, err)
	if err := initialize(s.DB); err == nil {
		t.Fatal("v22 migration accepted missing legacy index")
	}
	var version, columns int
	var header, state string
	requireStoreOK(t, s.DB.QueryRow("PRAGMA user_version").Scan(&version))
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM pragma_table_info('outbox') WHERE name='header_editable'").Scan(&columns))
	requireStoreOK(t, s.DB.QueryRow("SELECT header FROM outbox WHERE id=?", id).Scan(&header))
	requireStoreOK(t, s.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state))
	if version != 22 || columns != 0 || header != "legacy header" || state != "submitted" {
		t.Fatalf("failed migration changed schema/data: %d/%d/%q/%q", version, columns, header, state)
	}
	_, err = s.DB.Exec(`CREATE INDEX idx_outbox_assistant_header ON outbox(id) WHERE state='done' AND header_pending=1 AND message_id>0 AND header<>''`)
	requireStoreOK(t, err)
	requireStoreOK(t, initialize(s.DB))
	var editable bool
	requireStoreOK(t, s.DB.QueryRow("SELECT header_editable FROM outbox WHERE id=?", id).Scan(&editable))
	if !editable {
		t.Fatal("repaired migration lost legacy editable target")
	}
}

func TestUnchangedAssistantHeaderPreservesPendingWithoutNewEdit(t *testing.T) {
	for _, header := range []string{"", "same header"} {
		for _, pending := range []bool{false, true} {
			for _, stage := range []string{"before", "during", "after"} {
				for _, operation := range []string{"status", "completion"} {
					t.Run(header+"/"+strconv.FormatBool(pending)+"/"+stage+"/"+operation, func(t *testing.T) {
						s := openTestStore(t, t.TempDir())
						ctx := context.Background()
						requireStoreOK(t, s.Accept(10, []byte(`{}`)))
						requireStoreOK(t, s.Submit(10, 42))
						id, err := s.AppendAssistantReplies(ctx, 10, 7, 8, []string{"immutable body"}, header)
						requireStoreOK(t, err)
						if stage != "before" {
							_, err := s.ClaimNextOutput(ctx, time.Now().Unix())
							requireStoreOK(t, err)
						}
						if stage == "after" {
							requireStoreOK(t, s.MarkOutputDelivered(id, 100, header, ""))
						}
						if pending {
							requireStoreOK(t, s.SetAssistantHeader(ctx, id, "different header"))
							requireStoreOK(t, s.SetAssistantHeader(ctx, id, header))
						}
						if operation == "status" {
							requireStoreOK(t, s.SetAssistantHeader(ctx, id, header))
						} else {
							target, err := s.CompleteInboxWithAssistantHeader(ctx, 10, 7, 8, id, header, nil)
							requireStoreOK(t, err)
							if target != id {
								t.Fatalf("unchanged completion lost valid target: %d, want %d", target, id)
							}
						}
						var storedPending bool
						requireStoreOK(t, s.DB.QueryRow("SELECT header_pending FROM outbox WHERE id=?", id).Scan(&storedPending))
						if storedPending != pending {
							t.Fatalf("unchanged header changed pending: %t, want %t", storedPending, pending)
						}
						if stage == "before" {
							out, err := s.ClaimNextOutput(ctx, time.Now().Unix())
							requireStoreOK(t, err)
							if out.Header != header || out.Text != "immutable body" || !out.HeaderEditable {
								t.Fatalf("unchanged header altered send snapshot: %+v", out)
							}
						}
						if stage != "after" {
							requireStoreOK(t, s.MarkOutputDelivered(id, 100, header, ""))
						}
						edit, err := s.NextAssistantHeader()
						if stage == "after" && pending {
							requireStoreOK(t, err)
							if edit.ID != id || edit.Header != header || edit.Text != "immutable body" {
								t.Fatalf("unchanged update lost existing pending edit: %+v", edit)
							}
							requireStoreOK(t, s.MarkAssistantHeaderDelivered(id, header, ""))
						} else if !errors.Is(err, sql.ErrNoRows) {
							t.Fatalf("unchanged header queued redundant edit: %+v/%v", edit, err)
						}
						var count int
						requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&count))
						if count != 1 {
							t.Fatalf("unchanged header created %d outputs", count)
						}
						if _, err := s.NextOutput(); !errors.Is(err, sql.ErrNoRows) {
							t.Fatalf("unchanged header replayed body: %v", err)
						}
					})
				}
			}
		}
	}
}
