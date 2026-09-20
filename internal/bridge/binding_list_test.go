package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func newBindingTestWorker(t *testing.T, db *store.Store) (*worker, *fakeHTTP) {
	t.Helper()
	fake := &fakeHTTP{}
	previous := http.DefaultTransport
	http.DefaultTransport = fake
	t.Cleanup(func() { http.DefaultTransport = previous })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := testWorker(t, &worker{
		b:        testBridge(t, &Bridge{db: db, tg: newTestTelegram(t), bot: telegram.User{ID: 99}}),
		key:      target{chat: -10, thread: 11},
		ctx:      ctx,
		cancel:   cancel,
		confirms: make(map[string]confirmation),
	})
	return w, fake
}

func bindingButton(t *testing.T, f *fakeHTTP, label string) string {
	t.Helper()
	for _, button := range resumeButtons(t, f) {
		if button["text"] != label {
			continue
		}
		data, ok := button["callback_data"].(string)
		if !ok || data == "" {
			continue
		}
		return data
	}
	t.Fatalf("binding button %q not found", label)
	return ""
}

func onlyBindingConfirmation(t *testing.T, w *worker) confirmation {
	t.Helper()
	if len(w.confirms) != 1 {
		t.Fatalf("binding confirmations = %d, want 1", len(w.confirms))
	}
	for _, c := range w.confirms {
		return c
	}
	panic("unreachable")
}

func TestBindingListRendersStatesAndDeletionButtons(t *testing.T) {
	fake := &fakeHTTP{}
	previous := http.DefaultTransport
	http.DefaultTransport = fake
	defer func() { http.DefaultTransport = previous }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := testWorker(t, &worker{
		b:        testBridge(t, &Bridge{tg: newTestTelegram(t), bot: telegram.User{ID: 99}}),
		key:      target{chat: -10, thread: 11},
		ctx:      ctx,
		cancel:   cancel,
		confirms: make(map[string]confirmation),
	})
	entries := []store.BindingListEntry{
		{Binding: &store.Binding{Bot: 99, Chat: -10, Thread: 11, Workspace: "/current", SessionID: "current-session", Generation: 7, Running: true}},
		{Binding: &store.Binding{Bot: 99, Chat: -10, Thread: 55, Workspace: "/open", SessionID: "open-session", Generation: 5, Running: true}},
		{Binding: &store.Binding{Bot: 99, Chat: -10, Thread: 22, Workspace: "/closed", SessionID: "closed-session", Generation: 4, LastUsedAt: 1}, SessionName: "Closed task"},
		{Binding: &store.Binding{Bot: 99, Chat: -10, Thread: 33, Workspace: "/resume", SessionID: "old-session", Generation: 8, LastUsedAt: 1}, Intent: &store.StartIntent{Bot: 99, Chat: -10, Thread: 33, Kind: "resume", Workspace: "/resume", Session: "resume-session", Generation: 9}},
		{Intent: &store.StartIntent{Bot: 99, Chat: -10, Thread: 44, Kind: "new", Workspace: "/pending", Generation: 1}},
	}
	w.showBindingsPage(confirmation{bindings: entries, generation: 7, user: 7}, 0, 0)
	fake.mu.Lock()
	if len(fake.messages) != 1 {
		fake.mu.Unlock()
		t.Fatalf("binding list message count = %d, want 1", len(fake.messages))
	}
	message := fake.messages[0]
	fake.mu.Unlock()
	text, _ := message["text"].(string)
	if text != "Saved bindings\nPage 1/1" {
		t.Fatalf("binding list text = %q, want header only", text)
	}
	encoded, err := json.Marshal(message["reply_markup"])
	if err != nil {
		t.Fatal(err)
	}
	var keyboard telegram.Keyboard
	if err := json.Unmarshal(encoded, &keyboard); err != nil {
		t.Fatal(err)
	}
	if len(keyboard.InlineKeyboard) != 21 {
		t.Fatalf("binding list keyboard rows = %d, want 21", len(keyboard.InlineKeyboard))
	}
	assertInfo := func(row int, want string) {
		t.Helper()
		buttons := keyboard.InlineKeyboard[row]
		if len(buttons) == 0 || buttons[0].Text != want || buttons[0].Disabled == nil || buttons[0].CallbackData != "" {
			t.Fatalf("info row %d = %+v, want disabled %q", row, buttons, want)
		}
	}
	assertDisabledDelete := func(row int) {
		t.Helper()
		buttons := keyboard.InlineKeyboard[row]
		if len(buttons) != 2 || buttons[1].Text != "Del" || buttons[1].Disabled == nil || buttons[1].CallbackData != "" {
			t.Fatalf("disabled delete row %d = %+v", row, buttons)
		}
	}
	for _, row := range []int{0, 4, 12, 16} {
		assertDisabledDelete(row)
	}
	assertInfo(0, "1. Topic 11 [current]")
	assertInfo(1, "Open · Last used: unknown")
	assertInfo(2, "/current")
	assertInfo(3, "Name: unknown · "+bindingSession(entries[0]))
	assertInfo(4, "2. Topic 55")
	assertInfo(5, "Open · Last used: unknown")
	assertInfo(6, "/open")
	assertInfo(7, "Name: unknown · "+bindingSession(entries[1]))
	if len(keyboard.InlineKeyboard[8]) != 2 || keyboard.InlineKeyboard[8][0].Text != "3. Topic 22" || keyboard.InlineKeyboard[8][0].Disabled == nil || keyboard.InlineKeyboard[8][1].Text != "Del" || keyboard.InlineKeyboard[8][1].Style != "danger" || keyboard.InlineKeyboard[8][1].CallbackData == "" {
		t.Fatalf("closed binding title row = %+v", keyboard.InlineKeyboard[8])
	}
	assertInfo(9, "Closed · Last used: "+formatLastUsed(entries[2].Binding.LastUsedAt))
	assertInfo(10, "/closed")
	assertInfo(11, "Name: Closed task · "+bindingSession(entries[2]))
	assertInfo(12, "4. Topic 33")
	assertInfo(13, "Pending resume · Last used: "+formatLastUsed(entries[3].Binding.LastUsedAt))
	assertInfo(14, "/resume")
	assertInfo(15, "Name: unknown · "+bindingSession(entries[3]))
	assertInfo(16, "5. Topic 44")
	assertInfo(17, "Pending new · Last used: unknown")
	assertInfo(18, "/pending")
	assertInfo(19, "Name: pending · "+bindingSession(entries[4]))
	if len(keyboard.InlineKeyboard[20]) != 1 || keyboard.InlineKeyboard[20][0].Text != "Close" || keyboard.InlineKeyboard[20][0].CallbackData == "" {
		t.Fatalf("close button = %+v", keyboard.InlineKeyboard[20])
	}
}

func TestBindingListDisablesCurrentClosedDelete(t *testing.T) {
	w, fake := newBindingTestWorker(t, nil)
	entries := []store.BindingListEntry{{Binding: &store.Binding{Bot: 99, Chat: -10, Thread: 11, Workspace: "/current", SessionID: "current-session", Generation: 7}}}
	w.showBindingsPage(confirmation{bindings: entries, user: 7}, 0, 0)
	buttons := resumeButtons(t, fake)
	if len(buttons) < 2 || buttons[1]["text"] != "Del" {
		t.Fatalf("current closed buttons = %+v", buttons)
	}
	if _, ok := buttons[1]["callback_data"]; ok {
		t.Fatalf("current closed delete unexpectedly has callback data: %+v", buttons[1])
	}
	if _, ok := buttons[1]["disabled"]; !ok {
		t.Fatalf("current closed delete is not disabled: %+v", buttons[1])
	}
}

func TestBindingNameLookupDisplaysNativeTitle(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	session := resumeFixtureSession{ID: "abcd0000-0000-4000-8000-000000000000", CWD: workspace, Title: "Named session"}
	raw, err := json.Marshal([]resumeFixtureSession{session})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS", string(raw))
	w, fake := newBindingTestWorker(t, nil)
	w.b.cfg.OMP = binary
	entries := []store.BindingListEntry{{Binding: &store.Binding{Bot: 99, Chat: -10, Thread: 22, Workspace: workspace, SessionID: session.ID, Generation: 1}}}
	w.showBindingsPage(confirmation{bindings: entries, user: 7}, 0, 0)
	w.lookupBindingNames(entries, 0)
	var result bindingNamesResult
	select {
	case result = <-w.bindingNameResults:
	case <-time.After(5 * time.Second):
		t.Fatal("session name lookup did not finish")
	}
	w.bindingNamesLoaded(result)
	w.background.Wait()
	fake.mu.Lock()
	message := fake.messages[len(fake.messages)-1]
	fake.mu.Unlock()
	encoded, err := json.Marshal(message["reply_markup"])
	if err != nil {
		t.Fatal(err)
	}
	var keyboard telegram.Keyboard
	if err := json.Unmarshal(encoded, &keyboard); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			if button.Text == "Name: Named session · "+bindingSession(entries[0]) {
				found = button.Disabled != nil && button.CallbackData == ""
			}
		}
	}
	if !found {
		t.Fatalf("binding keyboard missing disabled native session name: %+v", keyboard.InlineKeyboard)
	}
}

func TestBindingCallbacksUseSentMessageID(t *testing.T) {
	t.Run("next", func(t *testing.T) {
		w, fake := newBindingTestWorker(t, nil)
		entries := make([]store.BindingListEntry, bindingPageSize+1)
		for i := range entries {
			entries[i].Binding = &store.Binding{Bot: 99, Chat: -10, Thread: int64(20 + i), Generation: int64(i + 1), Running: true}
		}
		w.showBindingsPage(confirmation{bindings: entries, user: 7}, 0, 0)
		messageID := int64(fake.messageCount())
		data := bindingButton(t, fake, "Next")
		if result := w.callback(&telegram.CallbackQuery{ID: "next", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: data}); result != callbackDone {
			t.Fatalf("next callback result = %v", result)
		}
		c := onlyBindingConfirmation(t, w)
		if c.page != 1 || c.messageID != messageID {
			t.Fatalf("next confirmation = %+v, want page 1 and message %d", c, messageID)
		}
	})

	t.Run("close", func(t *testing.T) {
		w, fake := newBindingTestWorker(t, nil)
		entries := []store.BindingListEntry{{Binding: &store.Binding{Bot: 99, Chat: -10, Thread: 22, Generation: 1, Running: true}}}
		w.showBindingsPage(confirmation{bindings: entries, user: 7}, 0, 0)
		messageID := int64(fake.messageCount())
		data := bindingButton(t, fake, "Close")
		if result := w.callback(&telegram.CallbackQuery{ID: "close", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: data}); result != callbackDone {
			t.Fatalf("close callback result = %v", result)
		}
		if len(w.confirms) != 0 {
			t.Fatalf("close left confirmations: %+v", w.confirms)
		}
		assertKeyboardClears(t, fake, int(messageID))
	})
}

func TestBindingDeleteCallbackDeletesClosedBinding(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	closed := store.Binding{Bot: 99, Chat: -10, Thread: 22, Workspace: "/closed", Session: "/sessions/closed.jsonl", Generation: 4}
	if err = db.Save(closed); err != nil {
		t.Fatal(err)
	}
	w, fake := newBindingTestWorker(t, db)
	w.showBindingsPage(confirmation{bindings: []store.BindingListEntry{{Binding: &closed}}, user: 7}, 0, 0)
	listMessageID := int64(fake.messageCount())
	w.callback(&telegram.CallbackQuery{ID: "list-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: listMessageID}, Data: bindingButton(t, fake, "Del")})
	confirmation := onlyBindingConfirmation(t, w)
	if confirmation.action != "binding_delete" || confirmation.messageID == listMessageID {
		t.Fatalf("delete confirmation = %+v", confirmation)
	}
	deleteMessageID := confirmation.messageID
	w.callback(&telegram.CallbackQuery{ID: "confirm-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: deleteMessageID}, Data: bindingButton(t, fake, "Delete")})
	if _, err = db.Binding(closed.Bot, closed.Chat, closed.Thread); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted binding lookup error = %v", err)
	}
	if len(w.confirms) != 0 {
		t.Fatalf("delete left confirmations: %+v", w.confirms)
	}
	assertKeyboardClears(t, fake, int(listMessageID), int(deleteMessageID))
}

func TestBindingDeleteCallbackRejectsStaleGeneration(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := store.Binding{Bot: 99, Chat: -10, Thread: 22, Workspace: "/old", Session: "/sessions/old.jsonl", Generation: 4}
	if err = db.Save(old); err != nil {
		t.Fatal(err)
	}
	w, fake := newBindingTestWorker(t, db)
	w.showBindingsPage(confirmation{bindings: []store.BindingListEntry{{Binding: &old}}, user: 7}, 0, 0)
	listMessageID := int64(fake.messageCount())
	w.callback(&telegram.CallbackQuery{ID: "list-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: listMessageID}, Data: bindingButton(t, fake, "Del")})
	confirmation := onlyBindingConfirmation(t, w)
	replacement := old
	replacement.Generation = 5
	replacement.Workspace = "/replacement"
	if err = db.Save(replacement); err != nil {
		t.Fatal(err)
	}
	w.callback(&telegram.CallbackQuery{ID: "confirm-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: confirmation.messageID}, Data: bindingButton(t, fake, "Delete")})
	got, err := db.Binding(old.Bot, old.Chat, old.Thread)
	if err != nil || got.Generation != replacement.Generation || got.Workspace != replacement.Workspace {
		t.Fatalf("stale delete changed binding: %+v, %v", got, err)
	}
}

func TestBindingDeleteRejectsStaleMutationEpoch(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fake := &fakeHTTP{}
	previous := http.DefaultTransport
	http.DefaultTransport = fake
	defer func() { http.DefaultTransport = previous }()
	b := testBridge(t, &Bridge{db: db, tg: newTestTelegram(t), bot: telegram.User{ID: 99}})
	newWorker := func(key target) *worker {
		ctx, cancel := context.WithCancel(context.Background())
		w := testWorker(t, &worker{b: b, key: key, ctx: ctx, cancel: cancel, confirms: make(map[string]confirmation)})
		t.Cleanup(func() {
			cancel()
			w.background.Wait()
		})
		return w
	}
	a := newWorker(target{chat: -10, thread: 11})
	c := newWorker(target{chat: -10, thread: 33})
	closed := store.Binding{Bot: 99, Chat: -10, Thread: 22, Workspace: "/old", Session: "/sessions/old.jsonl", Generation: 1}
	if err = db.Save(closed); err != nil {
		t.Fatal(err)
	}
	entries := []store.BindingListEntry{{Binding: &closed}}
	a.showBindingsPage(confirmation{bindings: entries, user: 7, epoch: b.bindingsEpoch.Load()}, 0, 0)
	aListMessageID := int64(fake.messageCount())
	a.callback(&telegram.CallbackQuery{ID: "a-list-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: aListMessageID}, Data: bindingButton(t, fake, "Del")})
	aDelete := onlyBindingConfirmation(t, a)
	aDeleteData := bindingButton(t, fake, "Delete")
	staleList := newWorker(target{chat: -10, thread: 44})
	staleList.showBindingsPage(confirmation{bindings: entries, user: 7, epoch: b.bindingsEpoch.Load()}, 0, 0)
	staleListMessageID := int64(fake.messageCount())
	staleListData := bindingButton(t, fake, "Del")

	c.showBindingsPage(confirmation{bindings: entries, user: 7, epoch: b.bindingsEpoch.Load()}, 0, 0)
	cListMessageID := int64(fake.messageCount())
	c.callback(&telegram.CallbackQuery{ID: "c-list-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: cListMessageID}, Data: bindingButton(t, fake, "Del")})
	cDelete := onlyBindingConfirmation(t, c)
	c.callback(&telegram.CallbackQuery{ID: "c-confirm-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: cDelete.messageID}, Data: bindingButton(t, fake, "Delete")})
	if got := b.bindingsEpoch.Load(); got != 1 {
		t.Fatalf("binding mutation epoch = %d, want 1", got)
	}
	staleList.callback(&telegram.CallbackQuery{ID: "stale-list-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: staleListMessageID}, Data: staleListData})
	if len(staleList.confirms) != 0 {
		t.Fatal("stale binding list opened a delete confirmation")
	}

	recreated := closed
	recreated.Workspace = "/new"
	recreated.Session = "/sessions/new.jsonl"
	if err = db.Save(recreated); err != nil {
		t.Fatal(err)
	}
	a.callback(&telegram.CallbackQuery{ID: "a-confirm-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: aDelete.messageID}, Data: aDeleteData})
	got, err := db.Binding(recreated.Bot, recreated.Chat, recreated.Thread)
	if err != nil || got.Workspace != recreated.Workspace || got.Generation != recreated.Generation {
		t.Fatalf("stale cross-worker delete changed recreated binding: %+v, %v", got, err)
	}
}

func TestBindingCallbackRejectsWrongMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := testWorker(t, &worker{ctx: ctx, cancel: cancel, key: target{chat: -10, thread: 11}, confirms: make(map[string]confirmation)})
	w.b.tg = newTestTelegram(t)
	w.confirms["token"] = confirmation{action: "bindings", generation: 0, messageID: 42, user: 7, options: []string{"close"}}
	previous := http.DefaultTransport
	http.DefaultTransport = &fakeHTTP{}
	defer func() { http.DefaultTransport = previous }()
	w.bindingCallback(ctx, &telegram.CallbackQuery{ID: "callback", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: 43}, Data: "token:0"}, "token", "0", w.confirms["token"])
	if _, exists := w.confirms["token"]; exists {
		t.Fatal("wrong-message callback kept binding token")
	}
}

func TestFormatLastUsedHandlesUnknownAndFuture(t *testing.T) {
	if got := formatLastUsed(0); got != "unknown" {
		t.Fatalf("zero timestamp = %q", got)
	}
	if got := formatLastUsed(time.Now().Add(time.Minute).Unix()); got != "unknown" {
		t.Fatalf("future timestamp = %q", got)
	}
}
