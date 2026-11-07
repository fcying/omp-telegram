package store

import (
	"context"
	"testing"
)

func TestProgressReplacementWaitsForEveryPartAndKeepsItsBoundary(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	ctx := context.Background()
	requireStoreOK(t, s.Accept(10, []byte(`{}`)))
	requireStoreOK(t, s.Submit(10, 42))
	requireStoreOK(t, s.SetProgressMessage(10, 7, 77))
	first, err := s.AppendAssistantReplies(ctx, 10, 7, 8, []string{"first part", "second part"}, "")
	requireStoreOK(t, err)
	requireStoreOK(t, s.RetireProgressMessages(10, 0, first, 2))
	for part := 0; part < 2; part++ {
		out, err := s.NextOutput()
		requireStoreOK(t, err)
		requireStoreOK(t, s.MarkOutput(out.ID, OutboxSending))
		candidates, err := s.CompletedProgressMessages(10)
		requireStoreOK(t, err)
		if len(candidates) != 0 {
			t.Fatalf("in-flight part %d allowed deletion: %+v", part, candidates)
		}
		requireStoreOK(t, s.MarkOutput(out.ID, OutboxDone))
	}
	requireStoreOK(t, s.SetProgressMessage(10, 7, 78))
	candidates, err := s.CompletedProgressMessages(10)
	requireStoreOK(t, err)
	if len(candidates) != 1 || candidates[0].MessageID != 77 {
		t.Fatalf("running cleanup must exclude the current preview: %+v", candidates)
	}
	later, err := s.AppendAssistantReplies(ctx, 10, 7, 8, []string{"later body"}, "")
	requireStoreOK(t, err)
	requireStoreOK(t, s.RetireProgressMessages(10, 0, later, 1))
	requireStoreOK(t, s.MarkOutput(later, OutboxSending))
	requireStoreOK(t, s.MarkOutput(later, OutboxFailed))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	candidates, err = s.CompletedProgressMessages(0)
	requireStoreOK(t, err)
	if len(candidates) != 1 || candidates[0].MessageID != 77 {
		t.Fatalf("restart or later failure changed the earlier replacement: %+v", candidates)
	}
}

func TestUndeliveredReplacementRetainsVisibleProgressAcrossRestart(t *testing.T) {
	for _, state := range []OutboxState{OutboxFailed, OutboxUncertain, OutboxCancelled} {
		for _, retired := range []bool{false, true} {
			name := string(state) + "/legacy"
			if retired {
				name = string(state) + "/retired"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				s := openTestStore(t, dir)
				requireStoreOK(t, s.Accept(10, []byte(`{}`)))
				requireStoreOK(t, s.Submit(10, 42))
				requireStoreOK(t, s.SetProgressMessage(10, 7, 77))
				first, err := s.AppendAssistantReplies(context.Background(), 10, 7, 8, []string{"delivered prefix", "visible preview tail"}, "")
				requireStoreOK(t, err)
				if retired {
					requireStoreOK(t, s.RetireProgressMessages(10, 0, first, 2))
				}
				for _, result := range []OutboxState{OutboxDone, state} {
					out, err := s.NextOutput()
					requireStoreOK(t, err)
					requireStoreOK(t, s.MarkOutput(out.ID, OutboxSending))
					requireStoreOK(t, s.MarkOutput(out.ID, result))
				}
				requireStoreOK(t, s.FinishSubmitted(10, InboxDone))
				if !retired {
					restoreProgressSchema24(t, s)
					_, err = s.DB.Exec("PRAGMA user_version=24")
					requireStoreOK(t, err)
				}
				requireStoreOK(t, s.Close())
				s = openTestStore(t, dir)
				candidates, err := s.CompletedProgressMessages(0)
				requireStoreOK(t, err)
				previews, err := s.ProgressMessages(10)
				requireStoreOK(t, err)
				if len(candidates) != 0 || len(previews) != 1 || previews[0].MessageID != 77 {
					t.Fatalf("undelivered body lost its preview: candidates=%v retained=%v", candidates, previews)
				}
			})
		}
	}
}
