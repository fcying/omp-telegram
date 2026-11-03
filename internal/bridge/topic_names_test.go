package bridge

import (
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func finishTopicRename(t *testing.T, w *worker) topicRenameResult {
	t.Helper()
	select {
	case result := <-w.topicRenameResults:
		w.topicRenameFinished(result)
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("topic rename did not finish")
		return topicRenameResult{}
	}
}

func recordedTopicName(t *testing.T, db *store.Store, chat, thread int64) (string, int64) {
	t.Helper()
	var name string
	var revision int64
	if err := db.DB.QueryRow("SELECT name,revision FROM topic_names WHERE bot=99 AND chat=? AND thread=?", chat, thread).Scan(&name, &revision); err != nil {
		t.Fatal(err)
	}
	return name, revision
}

func TestNewTopicNameCollisionOnlyRenamesNewTopic(t *testing.T) {
	w, fake, command := setupWorkspaceWorker(t)
	w.b.slots = make(chan struct{}, 3)
	workspace := filepath.Join(t.TempDir(), "project")
	command("/new " + workspace)
	if result := finishTopicRename(t, w); result.err != nil {
		t.Fatal(result.err)
	}
	peer := newArgsTestPeer(t, w, 22)
	if failure := peer.start(false, workspace, "", false); failure != "" {
		t.Fatal(failure)
	}
	if result := finishTopicRename(t, peer); result.err != nil {
		t.Fatal(result.err)
	}
	otherChat := newArgsTestPeer(t, w, 33)
	otherChat.key.chat = -20
	if failure := otherChat.start(false, workspace, "", false); failure != "" {
		t.Fatal(failure)
	}
	if result := finishTopicRename(t, otherChat); result.err != nil {
		t.Fatal(result.err)
	}

	fake.mu.Lock()
	edits := append([]map[string]any(nil), fake.forumTopicEdits...)
	fake.mu.Unlock()
	if len(edits) != 3 {
		t.Fatalf("topic edits = %+v", edits)
	}
	for i, expected := range []struct {
		chat, thread int64
		name         string
	}{{-10, 11, "project"}, {-10, 22, "project [22]"}, {-20, 33, "project"}} {
		if edits[i]["chat_id"] != float64(expected.chat) || edits[i]["message_thread_id"] != float64(expected.thread) || edits[i]["name"] != expected.name {
			t.Fatalf("edit %d = %+v, want %+v", i, edits[i], expected)
		}
	}
	if name, _ := recordedTopicName(t, w.b.db, -10, 11); name != "project" {
		t.Fatalf("old topic name changed to %q", name)
	}
	command("/close")
	command("/new " + filepath.Join(t.TempDir(), "replacement"))
	command("/new")
	fake.mu.Lock()
	count := len(fake.forumTopicEdits)
	fake.mu.Unlock()
	if count != 3 {
		t.Fatalf("replacement or repeated /new renamed an existing topic: %d edits", count)
	}
}

func TestNewTopicCollisionPreservesUTF16Suffix(t *testing.T) {
	w, fake, command := setupWorkspaceWorker(t)
	base := strings.Repeat("a", 123) + "😀xyz"
	requireStoreOK(t, w.b.db.ObserveTopicName(99, -10, 22, base))
	command("/new " + filepath.Join(t.TempDir(), base))
	if result := finishTopicRename(t, w); result.err != nil {
		t.Fatal(result.err)
	}
	fake.mu.Lock()
	edits := append([]map[string]any(nil), fake.forumTopicEdits...)
	fake.mu.Unlock()
	want := strings.Repeat("a", 123) + " [11]"
	if len(edits) != 1 || edits[0]["name"] != want || len(utf16.Encode([]rune(want))) != 128 {
		t.Fatalf("topic edits = %+v, want complete suffix in %q", edits, want)
	}
}

func TestAuthorizedTopicServiceMessagesObserveWithoutPrompt(t *testing.T) {
	fake, db, send := setupBridge(t)
	service := func(id int64, created bool, name, text string) telegram.Update {
		u := update(id, 11, text)
		if created {
			u.Message.ForumTopicCreated = &telegram.ForumTopicCreated{Name: name}
		} else {
			u.Message.ForumTopicEdited = &telegram.ForumTopicEdited{Name: name}
		}
		return u
	}
	waitIgnored := func(id int64) {
		t.Helper()
		waitFor(t, func() bool {
			var state string
			return db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", id).Scan(&state) == nil && state == "ignored"
		})
	}
	send(service(1, true, "created title", "must not become a prompt"))
	waitIgnored(1)
	if name, _ := recordedTopicName(t, db, -10, 11); name != "created title" {
		t.Fatalf("created name = %q", name)
	}
	if _, err := db.Binding(99, -10, 11); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("service message started a session: %v", err)
	}
	send(update(2, 11, "/new "+filepath.Join(t.TempDir(), "project")))
	waitBinding(t, db, 11)
	send(service(3, false, "manual title", "must not become a prompt"))
	waitIgnored(3)
	name, revision := recordedTopicName(t, db, -10, 11)
	if name != "manual title" {
		t.Fatalf("edited name = %q", name)
	}
	send(service(4, false, "", ""))
	waitIgnored(4)
	if got, rev := recordedTopicName(t, db, -10, 11); got != name || rev != revision {
		t.Fatalf("icon-only edit changed name/revision: %q/%d, want %q/%d", got, rev, name, revision)
	}
	unauthorized := service(5, false, "unauthorized title", "")
	unauthorized.Message.From.ID = 999
	send(unauthorized)
	waitIgnored(5)
	if got, rev := recordedTopicName(t, db, -10, 11); got != name || rev != revision {
		t.Fatalf("unauthorized event changed name/revision: %q/%d", got, rev)
	}
	historical := service(6, false, "historical title", "")
	send(telegram.Update{UpdateID: 6, CallbackQuery: &telegram.CallbackQuery{ID: "history", From: telegram.User{ID: 7}, Message: historical.Message, Data: "expired:0"}})
	waitInputDone(t, db, 6)
	if got, rev := recordedTopicName(t, db, -10, 11); got != name || rev != revision {
		t.Fatalf("callback's historical event changed name/revision: %q/%d", got, rev)
	}
	if fake.has(11, "answer: must not become a prompt") {
		t.Fatal("topic service message became a model prompt")
	}
}

func TestObservedTitleKeepsInFlightRenameReserved(t *testing.T) {
	fake, db, send := setupBridge(t)
	gate := make(chan struct{})
	started := make(chan struct{}, 2)
	fake.mu.Lock()
	fake.forumTopicEditGate, fake.forumTopicEditStarted = gate, started
	fake.mu.Unlock()
	defer close(gate)
	workspace := filepath.Join(t.TempDir(), "project")
	send(update(1, 11, "/new "+workspace))
	waitBinding(t, db, 11)
	waitTopicRenameStarted(t, started)
	manual := update(2, 11, "")
	manual.Message.ForumTopicEdited = &telegram.ForumTopicEdited{Name: "manual"}
	send(manual)
	waitFor(t, func() bool {
		var state string
		return db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&state) == nil && state == "ignored"
	})
	if name, _ := recordedTopicName(t, db, -10, 11); name != "manual" {
		t.Fatalf("observed title = %q, want manual", name)
	}
	send(update(3, 22, "/new "+workspace))
	waitBinding(t, db, 22)
	waitTopicRenameStarted(t, started)
	fake.mu.Lock()
	edits := append([]map[string]any(nil), fake.forumTopicEdits...)
	fake.mu.Unlock()
	if len(edits) != 2 || edits[0]["name"] != "project" || edits[1]["name"] != "project [22]" {
		t.Fatalf("in-flight topic edits = %+v", edits)
	}
}

func waitTopicRenameStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("topic rename did not reach Telegram")
	}
}

func TestTopicRenameFailureReservationSettlement(t *testing.T) {
	for _, tc := range []struct {
		name      string
		response  *fakeHTTPResponse
		err       error
		stale     bool
		observed  string
		want      string
		uncertain bool
		wantCalls int
	}{
		{name: "rejected", response: &fakeHTTPResponse{status: 400, body: `{"ok":false,"error_code":400,"description":"SECRET_REJECTION"}`}, want: "previous title", wantCalls: 1},
		{name: "uncertain", err: errors.New("SECRET_TRANSPORT"), want: "previous title", uncertain: true, wantCalls: 3},
		{name: "malformed", response: &fakeHTTPResponse{status: 200, body: `{`}, want: "previous title", uncertain: true, wantCalls: 1},
		{name: "stale rejected", response: &fakeHTTPResponse{status: 400, body: `{"ok":false,"error_code":400,"description":"SECRET_REJECTION"}`}, stale: true, want: "previous title", wantCalls: 1},
		{name: "stale uncertain", err: errors.New("SECRET_TRANSPORT"), stale: true, want: "previous title", uncertain: true, wantCalls: 3},
		{name: "manual edit", response: &fakeHTTPResponse{status: 400, body: `{"ok":false,"error_code":400,"description":"SECRET_REJECTION"}`}, observed: "manual title", want: "manual title", wantCalls: 1},
		{name: "same name observed", response: &fakeHTTPResponse{status: 400, body: `{"ok":false,"error_code":400,"description":"SECRET_REJECTION"}`}, observed: "project", want: "project", wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &logCapture{}
			w, command := setupLoggingWorker(t, capture, "json")
			fake := http.DefaultTransport.(*fakeHTTP)
			requireStoreOK(t, w.b.db.ObserveTopicName(99, -10, 11, "previous title"))
			gate := make(chan struct{})
			started := make(chan struct{}, 1)
			fake.mu.Lock()
			fake.forumTopicEditResponse, fake.forumTopicEditError = tc.response, tc.err
			fake.forumTopicEditGate, fake.forumTopicEditStarted = gate, started
			fake.mu.Unlock()
			command("/new " + filepath.Join(t.TempDir(), "project"))
			before := w.binding
			client := w.client
			waitTopicRenameStarted(t, started)
			if tc.observed != "" {
				w.handle(incoming{msg: &telegram.Message{ForumTopicEdited: &telegram.ForumTopicEdited{Name: tc.observed}}})
			}
			if tc.stale {
				w.binding.Generation++
			}
			close(gate)
			var result topicRenameResult
			select {
			case result = <-w.topicRenameResults:
			case <-time.After(5 * time.Second):
				t.Fatal("topic rename did not finish")
			}
			if result.err == nil || telegram.ClassifyError(result.err).Uncertain != tc.uncertain {
				t.Fatalf("rename result = %+v, want uncertain=%t", result, tc.uncertain)
			}
			if result.persistenceFailed {
				t.Fatal("rename settlement failed")
			}
			if name, _ := recordedTopicName(t, w.b.db, -10, 11); name != tc.want {
				t.Fatalf("settled name = %q, want %q", name, tc.want)
			}
			w.topicRenameFinished(result)
			probe, err := w.b.db.ReserveTopicName(99, -10, 22, "project")
			requireStoreOK(t, err)
			wantProbe := "project"
			if tc.uncertain || tc.observed == "project" {
				wantProbe += " [22]"
			}
			if probe.Name != wantProbe {
				t.Fatalf("old title reservation = %q, want %q", probe.Name, wantProbe)
			}
			if got, err := w.b.db.Binding(99, -10, 11); err != nil || !sameBindingIdentity(got, before) || !got.Running || w.client != client || client == nil {
				t.Fatalf("rename failure changed started session: %+v, error=%v", got, err)
			}
			var notices int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE text LIKE '%topic title%'").Scan(&notices))
			wantNotices := 1
			if tc.stale {
				wantNotices = 0
			}
			if notices != wantNotices {
				t.Fatalf("failure notices = %d, want %d", notices, wantNotices)
			}
			fake.mu.Lock()
			calls := len(fake.forumTopicEdits)
			fake.mu.Unlock()
			if calls != tc.wantCalls {
				t.Fatalf("HTTP calls = %d, want existing retry behavior %d", calls, tc.wantCalls)
			}
			for _, line := range strings.Split(capture.String(), "\n") {
				if strings.Contains(line, "topic_rename_failed") || strings.Contains(line, "topic_name_write_failed") {
					if strings.Contains(line, "SECRET_") || strings.Contains(line, "project") || strings.Contains(line, "previous title") || strings.Contains(line, "manual title") {
						t.Fatalf("topic failure log exposed title or raw error: %s", line)
					}
				}
			}
		})
	}
}

func TestSuccessfulTopicRenameSettlesBeforeNotification(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	requireStoreOK(t, w.b.db.ObserveTopicName(99, -10, 11, "previous title"))
	command("/new " + filepath.Join(t.TempDir(), "project"))
	var result topicRenameResult
	select {
	case result = <-w.topicRenameResults:
	case <-time.After(5 * time.Second):
		t.Fatal("topic rename did not finish")
	}
	if result.err != nil || result.persistenceFailed {
		t.Fatalf("rename result = %+v", result)
	}
	if name, _ := recordedTopicName(t, w.b.db, -10, 11); name != "project" {
		t.Fatalf("producer did not confirm rename: %q", name)
	}
	w.binding.Generation++
	w.topicRenameFinished(result)
	probe, err := w.b.db.ReserveTopicName(99, -10, 22, "previous title")
	requireStoreOK(t, err)
	if probe.Name != "previous title" {
		t.Fatalf("confirmed rename retained previous title: %q", probe.Name)
	}
}

func TestTopicRenameCancellationSettlesWithoutShutdownNotice(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response fakeHTTPResponse
		wantName string
		claimed  bool
	}{
		{name: "success", response: fakeHTTPResponse{status: 200, body: `{"ok":true,"result":true}`}, wantName: "project", claimed: true},
		{name: "rejected", response: fakeHTTPResponse{status: 400, body: `{"ok":false,"error_code":400,"description":"SECRET_REJECTION"}`}, wantName: "previous title"},
		{name: "uncertain", response: fakeHTTPResponse{status: 200, body: `{`}, wantName: "previous title", claimed: true},
	} {
		for _, blocked := range []bool{false, true} {
			name := "queued notification"
			if blocked {
				name = "unconsumed notification"
			}
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				w, fake, command := setupWorkspaceWorker(t)
				requireStoreOK(t, w.b.db.ObserveTopicName(99, -10, 11, "previous title"))
				gate := make(chan struct{})
				started := make(chan struct{}, 1)
				fake.mu.Lock()
				fake.forumTopicEditGate, fake.forumTopicEditStarted = gate, started
				fake.forumTopicEditResponse = &tc.response
				if blocked {
					// Close runs after the response has been decoded, before the producer gets its outcome.
					fake.forumTopicEditClosed = w.cancel
					w.topicRenameResults <- topicRenameResult{}
				}
				fake.mu.Unlock()
				command("/new " + filepath.Join(t.TempDir(), "project"))
				waitTopicRenameStarted(t, started)
				close(gate)
				if !blocked {
					select {
					case result := <-w.topicRenameResults:
						w.cancel()
						w.topicRenameFinished(result)
					case <-time.After(5 * time.Second):
						t.Fatal("topic rename notification did not arrive")
					}
				}
				finished := make(chan struct{})
				go func() {
					w.background.Wait()
					close(finished)
				}()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Fatal("rename producer blocked worker shutdown")
				}
				if w.ctx.Err() == nil {
					t.Fatal("worker was not cancelled")
				}
				if got, _ := recordedTopicName(t, w.b.db, -10, 11); got != tc.wantName {
					t.Fatalf("settled name = %q, want %q", got, tc.wantName)
				}
				probe, err := w.b.db.ReserveTopicName(99, -10, 22, "project")
				requireStoreOK(t, err)
				wantProbe := "project"
				if tc.claimed {
					wantProbe += " [22]"
				}
				if probe.Name != wantProbe {
					t.Fatalf("post-shutdown reservation = %q, want %q", probe.Name, wantProbe)
				}
				var notices int
				requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE text LIKE '%topic title%'").Scan(&notices))
				if notices != 0 {
					t.Fatalf("shutdown rename notices = %d, want 0", notices)
				}
				fake.mu.Lock()
				calls := len(fake.forumTopicEdits)
				fake.mu.Unlock()
				if calls != 1 {
					t.Fatalf("HTTP calls = %d, want no replay", calls)
				}
			})
		}
	}
}

func TestTopicNamePersistenceFailureKeepsStartedSession(t *testing.T) {
	for _, phase := range []string{"reserve", "confirm", "release", "retain"} {
		t.Run(phase, func(t *testing.T) {
			capture := &logCapture{}
			w, command := setupLoggingWorker(t, capture, "json")
			fake := http.DefaultTransport.(*fakeHTTP)
			gate := make(chan struct{})
			started := make(chan struct{}, 1)
			if phase != "reserve" {
				fake.mu.Lock()
				fake.forumTopicEditGate, fake.forumTopicEditStarted = gate, started
				switch phase {
				case "release":
					fake.forumTopicEditResponse = &fakeHTTPResponse{status: 400, body: `{"ok":false,"error_code":400,"description":"SECRET_REJECTION"}`}
				case "retain":
					fake.forumTopicEditResponse = &fakeHTTPResponse{status: 200, body: `{`}
				}
				fake.mu.Unlock()
			}
			breakPersistence := func() {
				t.Helper()
				table := "topic_names"
				if phase == "release" || phase == "retain" {
					table = "topic_name_reservations"
				}
				_, err := w.b.db.DB.Exec("DROP TABLE " + table)
				requireStoreOK(t, err)
			}
			if phase == "reserve" {
				breakPersistence()
			}
			command("/new " + filepath.Join(t.TempDir(), "SECRET_TOPIC_NAME"))
			if phase != "reserve" {
				waitTopicRenameStarted(t, started)
				breakPersistence()
				close(gate)
				select {
				case result := <-w.topicRenameResults:
					if !result.persistenceFailed || (phase == "confirm") != (result.err == nil) {
						t.Fatalf("rename settlement result = %+v", result)
					}
					w.topicRenameFinished(result)
				case <-time.After(5 * time.Second):
					t.Fatal("topic rename did not finish")
				}
			}
			if binding, err := w.b.db.Binding(99, -10, 11); err != nil || !binding.Running || w.client == nil {
				t.Fatalf("persistence failure closed started session: %+v, error=%v", binding, err)
			}
			var notices int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE text LIKE '%topic title%'").Scan(&notices))
			if notices != 1 {
				t.Fatalf("failure notices = %d, want 1", notices)
			}
			fake.mu.Lock()
			calls := len(fake.forumTopicEdits)
			fake.mu.Unlock()
			wantCalls := 0
			if phase != "reserve" {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("HTTP calls = %d, want %d", calls, wantCalls)
			}
			for _, line := range strings.Split(capture.String(), "\n") {
				if strings.Contains(line, "topic_name_write_failed") && (strings.Contains(line, "SECRET_TOPIC_NAME") || strings.Contains(line, "no such table")) {
					t.Fatalf("persistence failure log exposed title or raw error: %s", line)
				}
			}
		})
	}
}

func TestStoppedTopicRenameLogsSettlementFailure(t *testing.T) {
	capture := &logCapture{}
	w, command := setupLoggingWorker(t, capture, "json")
	fake := http.DefaultTransport.(*fakeHTTP)
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	fake.mu.Lock()
	fake.forumTopicEditGate, fake.forumTopicEditStarted = gate, started
	fake.forumTopicEditResponse = &fakeHTTPResponse{status: 200, body: `{"ok":true,"result":true}`}
	fake.forumTopicEditClosed = w.cancel
	fake.mu.Unlock()
	w.topicRenameResults <- topicRenameResult{}
	command("/new " + filepath.Join(t.TempDir(), "SECRET_TOPIC_NAME"))
	waitTopicRenameStarted(t, started)
	_, err := w.b.db.DB.Exec("DROP TABLE topic_names")
	requireStoreOK(t, err)
	close(gate)
	finished := make(chan struct{})
	go func() {
		w.background.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("failed settlement blocked worker shutdown")
	}
	if !strings.Contains(capture.String(), "topic_name_write_failed") {
		t.Fatal("stopped producer did not log persistence failure")
	}
	for _, line := range strings.Split(capture.String(), "\n") {
		if strings.Contains(line, "topic_name_write_failed") && (strings.Contains(line, "SECRET_TOPIC_NAME") || strings.Contains(line, "no such table")) {
			t.Fatalf("stopped producer exposed title or raw error: %s", line)
		}
	}
	var notices int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE text LIKE '%topic title%'").Scan(&notices))
	if notices != 0 {
		t.Fatalf("stopped producer notices = %d, want 0", notices)
	}
}

func TestNewTopicDoesNotInferExistingTitleFromBinding(t *testing.T) {
	w, fake, command := setupWorkspaceWorker(t)
	workspace := filepath.Join(t.TempDir(), "project")
	old := store.Binding{Bot: 99, Chat: -10, Thread: 22, Workspace: workspace, Generation: 1}
	requireStoreOK(t, w.b.db.Save(old))
	command("/new " + workspace)
	if result := finishTopicRename(t, w); result.err != nil {
		t.Fatal(result.err)
	}
	fake.mu.Lock()
	edits := append([]map[string]any(nil), fake.forumTopicEdits...)
	fake.mu.Unlock()
	if len(edits) != 1 || edits[0]["name"] != "project" || edits[0]["message_thread_id"] != float64(11) {
		t.Fatalf("unobserved binding title caused a conflict: %+v", edits)
	}
	var count int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM topic_names WHERE bot=99 AND chat=-10 AND thread=22").Scan(&count))
	if count != 0 {
		t.Fatal("existing binding was backfilled with an assumed topic title")
	}
}
