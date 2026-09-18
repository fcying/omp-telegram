package bridge

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func TestPrivateChatRoutingIsolationAndResume(t *testing.T) {
	d := newRecoveryDaemon(t, 4)
	d.command(11, "/help")
	d.stop()
	d.cfg.AllowedChats = []int64{7, 8, -10, -20}
	d.cfg.AllowedUsers = []int64{7, 8}
	d.cfg.ProgressMode = "summary"
	d.start()

	state := func(id int64) string {
		var value string
		_ = d.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", id).Scan(&value)
		return value
	}
	send := func(chat, thread int64, kind, text string) int64 {
		d.nextID++
		u := update(d.nextID, thread, text)
		u.Message.Chat = telegram.Chat{ID: chat, Type: kind}
		if chat > 0 {
			u.Message.From.ID = chat
		}
		d.fake.updates <- u
		return d.nextID
	}
	command := func(chat, thread int64, kind, text string) {
		waitInputDone(t, d.db, send(chat, thread, kind, text))
	}
	has := func(chat, thread int64, text string) bool {
		d.fake.mu.Lock()
		defer d.fake.mu.Unlock()
		for _, m := range d.fake.messages {
			gotThread, _ := m["message_thread_id"].(float64)
			if m["chat_id"] == float64(chat) && gotThread == float64(thread) && strings.Contains(m["text"].(string), text) {
				return true
			}
		}
		return false
	}
	binding := func(chat, thread int64) store.Binding {
		t.Helper()
		b, err := d.db.Binding(99, chat, thread)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	command(7, 0, "private", "/help")
	waitFor(t, func() bool { return has(7, 0, "/new") })
	command(7, 0, "private", "/new private-project")
	private := binding(7, 0)
	if private.Workspace != filepath.Join(d.cfg.WorkspaceRoot, "private-project") {
		t.Fatalf("private named workspace = %q", private.Workspace)
	}

	// Same-chat topics and another private chat must not share session identity.
	for _, conversation := range []struct {
		chat, thread int64
		kind         string
	}{{7, 11, "private"}, {-10, 11, "supergroup"}, {8, 0, "private"}} {
		command(conversation.chat, conversation.thread, conversation.kind, "/new "+t.TempDir())
		if b := binding(conversation.chat, conversation.thread); b.Session == private.Session {
			t.Fatal("different conversations share a native session")
		}
		command(conversation.chat, conversation.thread, conversation.kind, "isolated-reply")
		waitFor(t, func() bool { return has(conversation.chat, conversation.thread, "answer: isolated-reply") })
	}
	if has(7, 0, "answer: isolated-reply") {
		t.Fatal("topic or other private chat output crossed into the private main session")
	}

	for _, kind := range []string{"group", "supergroup"} {
		command(-20, 0, kind, "/new "+t.TempDir())
	}
	if _, err := d.db.Binding(99, -20, 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ordinary group allocated a binding: %v", err)
	}
	waitFor(t, func() bool { return has(-20, 0, "In groups, use it inside a topic.") })

	// The private exception must not bypass either authorization dimension.
	for _, unauthorized := range []struct{ chat, user int64 }{{9, 7}, {7, 99}} {
		d.nextID++
		u := update(d.nextID, 0, "/close")
		u.Message.Chat = telegram.Chat{ID: unauthorized.chat, Type: "private"}
		u.Message.From.ID = unauthorized.user
		d.fake.updates <- u
		id := d.nextID
		waitFor(t, func() bool { return state(id) == "ignored" })
	}
	if !binding(7, 0).Running {
		t.Fatal("unauthorized message closed the private session")
	}

	active := send(7, 0, "private", "wait")
	waitFor(t, func() bool { return state(active) == "submitted" })
	waitFor(t, func() bool { return has(7, 0, "Processing...") })
	queued := send(7, 0, "private", "must-not-run")
	command(7, 0, "private", "/stop")
	waitFor(t, func() bool { return state(queued) == "cancelled" })
	command(7, 0, "private", "/review private changes")
	waitFor(t, func() bool { return has(7, 0, "answer: /review private changes") })
	command(7, 0, "private", "/status")
	waitFor(t, func() bool { return has(7, 0, "fixture/safe") })

	// Use the real callback routing path for a private-session picker.
	command(7, 0, "private", "/close")
	sessions := setResumeFixtures(t, private.Workspace, 1)
	command(7, 0, "private", "/resume")
	waitFor(t, func() bool {
		d.fake.mu.Lock()
		defer d.fake.mu.Unlock()
		for _, m := range d.fake.messages {
			if m["chat_id"] == float64(7) && strings.Contains(m["text"].(string), "Saved omp sessions") && m["reply_markup"] != nil {
				return true
			}
		}
		return false
	})
	buttons := resumeButtons(t, d.fake)
	token := buttons[0]["callback_data"].(string)
	callback := func(user int64) int64 {
		d.nextID++
		m := &telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: 7, Type: "private"}}
		d.fake.updates <- telegram.Update{UpdateID: d.nextID, CallbackQuery: &telegram.CallbackQuery{ID: "private-picker", From: telegram.User{ID: user}, Message: m, Data: token}}
		return d.nextID
	}
	denied := callback(99)
	waitFor(t, func() bool { return state(denied) == "ignored" })
	if binding(7, 0).Running {
		t.Fatal("unauthorized callback resumed private session")
	}
	waitInputDone(t, d.db, callback(7))
	after := binding(7, 0)
	if filepath.Base(after.Session) != sessions[0].ID+".jsonl" || after.Workspace != private.Workspace || !after.Running {
		t.Fatalf("private picker restored wrong binding: %+v", after)
	}
	command(7, 0, "private", "private-resumed")
	waitFor(t, func() bool { return has(7, 0, "answer: private-resumed") })

	// A running private instance must survive /new until its button is confirmed.
	command(7, 0, "private", "/new")
	waitFor(t, func() bool { return has(7, 0, "Start a new omp session in: "+after.Workspace) })
	if got := binding(7, 0); got != after {
		t.Fatalf("private /new replaced the instance before confirmation: %+v", got)
	}
	buttons = resumeButtons(t, d.fake)
	token = buttons[0]["callback_data"].(string)
	denied = callback(99)
	waitFor(t, func() bool { return state(denied) == "ignored" })
	if got := binding(7, 0); got != after {
		t.Fatal("unauthorized confirmation replaced the private instance")
	}
	waitInputDone(t, d.db, callback(7))
	replaced := binding(7, 0)
	if replaced.Session == after.Session || replaced.Generation <= after.Generation || replaced.Workspace != after.Workspace || !replaced.Running {
		t.Fatalf("private confirmation did not create a fresh session in the same workspace: %+v", replaced)
	}
	command(7, 0, "private", "private-replaced")
	waitFor(t, func() bool { return has(7, 0, "answer: private-replaced") })
	waitInputDone(t, d.db, callback(7))
	if got := binding(7, 0); got != replaced {
		t.Fatal("replayed private confirmation replaced the session again")
	}
	if !binding(7, 11).Running {
		t.Fatal("private main close/resume affected its topic")
	}
	d.fake.mu.Lock()
	defer d.fake.mu.Unlock()
	for _, m := range d.fake.messages {
		if m["chat_id"] == float64(8) || (m["chat_id"] == float64(7) && m["message_thread_id"] != float64(11)) {
			if _, present := m["message_thread_id"]; present {
				t.Fatal("ordinary private reply included message_thread_id")
			}
		}
	}
}
