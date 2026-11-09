package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func topicNameState(t *testing.T, s *Store, bot, chat, thread int64) string {
	t.Helper()
	var name string
	requireStoreOK(t, s.DB.QueryRow("SELECT name FROM topic_names WHERE bot=? AND chat=? AND thread=?", bot, chat, thread).Scan(&name))
	return name
}

func requireTopicNameAvailability(t *testing.T, s *Store, base string, available bool) {
	t.Helper()
	r, err := s.ReserveTopicName(1, 2, 90, base)
	requireStoreOK(t, err)
	if (r.Name == base) != available {
		t.Fatalf("reservation for %q = %q, want availability %t", base, r.Name, available)
	}
	requireStoreOK(t, s.ReleaseTopicName(1, 2, 90, r))
}

func TestReserveTopicNameExactMatchAndScope(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	requireStoreOK(t, s.ObserveTopicName(1, 2, 3, "Project"))
	for _, tc := range []struct {
		bot, chat, thread int64
		base, want        string
	}{
		{1, 2, 3, "Project", "Project"},
		{1, 2, 3, "Project", "Project"},
		{1, 2, 4, "project", "project"},
		{1, 3, 3, "Project", "Project"},
		{2, 2, 3, "Project", "Project"},
		{1, 2, 5, "Project", "Project [5]"},
	} {
		r, err := s.ReserveTopicName(tc.bot, tc.chat, tc.thread, tc.base)
		requireStoreOK(t, err)
		if r.Name != tc.want {
			t.Fatalf("reservation (%d,%d,%d) = %q, want %q", tc.bot, tc.chat, tc.thread, r.Name, tc.want)
		}
	}
	requireStoreOK(t, s.ObserveTopicName(1, 2, 6, "Project [7]"))
	requireStoreOK(t, s.ObserveTopicName(1, 2, 8, "Project [7-2]"))
	r, err := s.ReserveTopicName(1, 2, 7, "Project")
	requireStoreOK(t, err)
	if r.Name != "Project [7-3]" {
		t.Fatalf("occupied suffix reservation = %q", r.Name)
	}
	if name := topicNameState(t, s, 1, 2, 3); name != "Project" {
		t.Fatalf("existing topic was renamed to %q", name)
	}
}

func TestReserveTopicNameConcurrentThreads(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	const count = 16
	type result struct {
		thread int64
		r      TopicNameReservation
		err    error
	}
	results := make(chan result, count)
	start := make(chan struct{})
	for thread := int64(1); thread <= count; thread++ {
		go func(thread int64) {
			<-start
			r, err := s.ReserveTopicName(1, 2, thread, "same")
			results <- result{thread, r, err}
		}(thread)
	}
	close(start)
	names := make(map[string]bool)
	plain := 0
	for range count {
		got := <-results
		requireStoreOK(t, got.err)
		if names[got.r.Name] {
			t.Fatalf("duplicate concurrent reservation %q", got.r.Name)
		}
		names[got.r.Name] = true
		if got.r.Name == "same" {
			plain++
		} else if want := fmt.Sprintf("same [%d]", got.thread); got.r.Name != want {
			t.Fatalf("reservation = %q, want %q", got.r.Name, want)
		}
	}
	if plain != 1 {
		t.Fatalf("plain reservations = %d, want 1", plain)
	}
}

func TestTopicNamesRetainedUntilObservation(t *testing.T) {
	for _, outcome := range []string{"uncertain", "restart"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			s := openTestStore(t, dir)
			requireStoreOK(t, s.ObserveTopicName(1, 2, 3, "previous"))
			r, err := s.ReserveTopicName(1, 2, 3, "reserved")
			requireStoreOK(t, err)
			if outcome == "uncertain" {
				requireStoreOK(t, s.RetainTopicName(1, 2, 3, r))
			} else {
				requireStoreOK(t, s.Close())
				s = openTestStore(t, dir)
			}
			// Settled and recovered claims cannot be changed by late HTTP results.
			requireStoreOK(t, s.ReleaseTopicName(1, 2, 3, r))
			requireStoreOK(t, s.ConfirmTopicName(1, 2, 3, r))
			if name := topicNameState(t, s, 1, 2, 3); name != "previous" {
				t.Fatalf("late result changed known title to %q", name)
			}
			requireTopicNameAvailability(t, s, "reserved", false)
			requireTopicNameAvailability(t, s, "previous", false)
			requireStoreOK(t, s.ObserveTopicName(1, 2, 3, "manual"))
			requireTopicNameAvailability(t, s, "reserved", true)
			requireTopicNameAvailability(t, s, "previous", true)
			requireTopicNameAvailability(t, s, "manual", false)
		})
	}
}

func TestReserveTopicNameLongEmojiKeepsSuffix(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	base := strings.Repeat("😀", 64)
	first, err := s.ReserveTopicName(1, 2, 3, base)
	requireStoreOK(t, err)
	if first.Name != base {
		t.Fatal("nonconflicting 128-unit title changed")
	}
	requireStoreOK(t, s.ObserveTopicName(1, 2, 5, strings.Repeat("😀", 62)+" [4]"))
	r, err := s.ReserveTopicName(1, 2, 4, base)
	requireStoreOK(t, err)
	want := strings.Repeat("😀", 61) + " [4-2]"
	if r.Name != want || !utf8.ValidString(r.Name) || len(utf16.Encode([]rune(r.Name))) != 128 {
		t.Fatalf("long title = %q, want intact 128-unit title %q", r.Name, want)
	}
}

func TestReleaseTopicNameMatchingClaimAndTombstone(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	r, err := s.ReserveTopicName(1, 2, 3, "title")
	requireStoreOK(t, err)
	requireStoreOK(t, s.ReleaseTopicName(1, 2, 3, r))
	requireTopicNameAvailability(t, s, "title", true)
	next, err := s.ReserveTopicName(1, 2, 3, "title")
	requireStoreOK(t, err)
	for _, stale := range []TopicNameReservation{
		r,
		{Name: "other", Revision: next.Revision},
		{Name: next.Name, Revision: next.Revision + 1},
	} {
		requireStoreOK(t, s.ReleaseTopicName(1, 2, 3, stale))
		requireStoreOK(t, s.ConfirmTopicName(1, 2, 3, stale))
		requireStoreOK(t, s.RetainTopicName(1, 2, 3, stale))
	}
	requireTopicNameAvailability(t, s, "title", false)
	requireStoreOK(t, s.ObserveTopicName(1, 2, 3, "manual"))
	requireStoreOK(t, s.ObserveTopicName(1, 2, 3, ""))
	requireStoreOK(t, s.ReleaseTopicName(1, 2, 3, next))
	if name := topicNameState(t, s, 1, 2, 3); name != "manual" {
		t.Fatalf("late rejection overwrote observation with %q", name)
	}
	requireTopicNameAvailability(t, s, "title", true)
	// Service events may report manually duplicated titles.
	requireStoreOK(t, s.ObserveTopicName(1, 2, 4, "manual"))
	requireTopicNameAvailability(t, s, "manual", false)
}

func TestTopicNameReservationWriteFailurePreservesPreviousState(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	requireStoreOK(t, s.ObserveTopicName(1, 2, 3, "previous"))
	requireStoreOK(t, s.ObserveTopicName(1, 2, 4, "older"))
	r, err := s.ReserveTopicName(1, 2, 4, "reserved")
	requireStoreOK(t, err)
	_, err = s.DB.Exec(`CREATE TRIGGER reject_topic_reservation BEFORE INSERT ON topic_name_reservations
BEGIN SELECT RAISE(ABORT, 'blocked'); END;`)
	requireStoreOK(t, err)
	if _, err := s.ReserveTopicName(1, 2, 3, "new"); err == nil {
		t.Fatal("reservation succeeded despite failed write")
	}
	if name := topicNameState(t, s, 1, 2, 3); name != "previous" {
		t.Fatalf("failed reservation changed previous title to %q", name)
	}
	_, err = s.DB.Exec(`DROP TRIGGER reject_topic_reservation;
CREATE TRIGGER reject_topic_settlement BEFORE DELETE ON topic_name_reservations
BEGIN SELECT RAISE(ABORT, 'blocked'); END;`)
	requireStoreOK(t, err)
	if err := s.ReleaseTopicName(1, 2, 4, r); err == nil {
		t.Fatal("release succeeded despite failed write")
	}
	if err := s.ConfirmTopicName(1, 2, 4, r); err == nil {
		t.Fatal("confirmation succeeded despite failed write")
	}
	if name := topicNameState(t, s, 1, 2, 4); name != "older" {
		t.Fatalf("failed confirmation changed known title to %q", name)
	}
	_, err = s.DB.Exec("DROP TRIGGER reject_topic_settlement")
	requireStoreOK(t, err)
	requireTopicNameAvailability(t, s, "new", true)
	requireTopicNameAvailability(t, s, "reserved", false)
	requireTopicNameAvailability(t, s, "older", false)
	// A failed settlement leaves the original request active and retryable locally.
	requireStoreOK(t, s.ReleaseTopicName(1, 2, 4, r))
	requireTopicNameAvailability(t, s, "reserved", true)
}

func TestTopicNamesSchema20UpgradePreservesBindingWithoutGuessingTitle(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	before := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/workspace/project", Session: "/session", Generation: 7}
	requireStoreOK(t, s.Save(before))
	restoreLegacyProgressSchema(t, s)
	_, err := s.DB.Exec("DROP TABLE topic_name_reservations; DROP TABLE topic_names; PRAGMA user_version=20")
	requireStoreOK(t, err)
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	after, err := s.Binding(1, 2, 3)
	requireStoreOK(t, err)
	var version int
	requireStoreOK(t, s.DB.QueryRow("PRAGMA user_version").Scan(&version))
	if after != before || version != schemaVersion {
		t.Fatalf("v20 upgrade = binding %+v, version %d", after, version)
	}
	r, err := s.ReserveTopicName(1, 2, 4, "project")
	requireStoreOK(t, err)
	if r.Name != "project" {
		t.Fatalf("upgrade guessed a title from old binding: %+v", r)
	}
}

func TestTopicNameObservationDuringRequest(t *testing.T) {
	for _, outcome := range []string{"confirm", "matching-confirm", "release"} {
		t.Run(outcome, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			requireStoreOK(t, s.ObserveTopicName(1, 2, 3, "old"))
			r, err := s.ReserveTopicName(1, 2, 3, "new")
			requireStoreOK(t, err)
			requireTopicNameAvailability(t, s, "old", false)
			requireTopicNameAvailability(t, s, "new", false)
			observed := "manual"
			if outcome == "matching-confirm" {
				observed = "new"
			}
			requireStoreOK(t, s.ObserveTopicName(1, 2, 3, observed))
			// A service event cannot settle a request that is still in HTTP flight.
			requireTopicNameAvailability(t, s, "new", false)
			if outcome == "release" {
				requireStoreOK(t, s.ReleaseTopicName(1, 2, 3, r))
			} else {
				requireStoreOK(t, s.ConfirmTopicName(1, 2, 3, r))
			}
			requireStoreOK(t, s.ReleaseTopicName(1, 2, 3, r))
			requireStoreOK(t, s.ConfirmTopicName(1, 2, 3, r))
			requireStoreOK(t, s.RetainTopicName(1, 2, 3, r))
			if name := topicNameState(t, s, 1, 2, 3); name != observed {
				t.Fatalf("late result changed observed title to %q", name)
			}
			requireTopicNameAvailability(t, s, "new", outcome == "release")
			requireStoreOK(t, s.ObserveTopicName(1, 2, 3, "settled"))
			requireTopicNameAvailability(t, s, "new", true)
			requireTopicNameAvailability(t, s, "old", true)
		})
	}
}

func TestTopicNameMultipleRequestsSettleIndependently(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	requireStoreOK(t, s.ObserveTopicName(1, 2, 3, "old"))
	first, err := s.ReserveTopicName(1, 2, 3, "first")
	requireStoreOK(t, err)
	second, err := s.ReserveTopicName(1, 2, 3, "second")
	requireStoreOK(t, err)
	for _, name := range []string{"old", "first", "second"} {
		requireTopicNameAvailability(t, s, name, false)
	}
	requireStoreOK(t, s.ReleaseTopicName(1, 2, 3, first))
	requireStoreOK(t, s.ConfirmTopicName(1, 2, 3, first))
	requireStoreOK(t, s.RetainTopicName(1, 2, 3, first))
	requireTopicNameAvailability(t, s, "first", true)
	requireTopicNameAvailability(t, s, "second", false)
	if name := topicNameState(t, s, 1, 2, 3); name != "old" {
		t.Fatalf("reservation or rejection changed known title to %q", name)
	}
	requireStoreOK(t, s.ConfirmTopicName(1, 2, 3, second))
	if name := topicNameState(t, s, 1, 2, 3); name != "second" {
		t.Fatalf("matching confirmation did not commit title: %q", name)
	}
	requireTopicNameAvailability(t, s, "old", true)
	third, err := s.ReserveTopicName(1, 2, 3, "third")
	requireStoreOK(t, err)
	fourth, err := s.ReserveTopicName(1, 2, 3, "fourth")
	requireStoreOK(t, err)
	requireStoreOK(t, s.ConfirmTopicName(1, 2, 3, fourth))
	requireStoreOK(t, s.ConfirmTopicName(1, 2, 3, third))
	requireStoreOK(t, s.ReleaseTopicName(1, 2, 3, third))
	if name := topicNameState(t, s, 1, 2, 3); name != "fourth" {
		t.Fatalf("out-of-order confirmation replaced newer title with %q", name)
	}
	requireTopicNameAvailability(t, s, "third", false)
	requireStoreOK(t, s.ObserveTopicName(1, 2, 3, "fourth"))
	requireTopicNameAvailability(t, s, "third", true)
}

func TestExplicitTopicNameSurvivesSchemaUpgradeAndReopen(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	requireStoreOK(t, s.ObserveTopicName(1, 2, 3, "existing title"))
	_, err := s.DB.Exec("ALTER TABLE bindings DROP COLUMN auto_session_name_pending; ALTER TABLE topic_names DROP COLUMN explicit; PRAGMA user_version=25")
	requireStoreOK(t, err)
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	if title := topicNameState(t, s, 1, 2, 3); title != "existing title" {
		t.Fatalf("schema upgrade changed the title: %q", title)
	}
	r, err := s.ReserveTopicName(1, 2, 3, "workspace")
	requireStoreOK(t, err)
	requireStoreOK(t, s.ReleaseTopicName(1, 2, 3, r))
	r, err = s.ReserveExactTopicName(1, 2, 3, "Trading research")
	requireStoreOK(t, err)
	requireStoreOK(t, s.ConfirmTopicName(1, 2, 3, r))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	_, err = s.ReserveTopicName(1, 2, 3, "workspace")
	if !errors.Is(err, ErrTopicNameExplicit) {
		t.Fatalf("reopened database allowed automatic overwrite: %v", err)
	}
	if title := topicNameState(t, s, 1, 2, 3); title != "Trading research" {
		t.Fatalf("reopen changed the explicit title: %q", title)
	}
	_, err = s.ReserveTopicName(1, 2, 4, "workspace")
	requireStoreOK(t, err)
}
