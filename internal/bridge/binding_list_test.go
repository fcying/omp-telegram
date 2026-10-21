package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

func bindingEntryButton(t *testing.T, f *fakeHTTP, thread int64) string {
	t.Helper()
	marker := fmt.Sprintf(" · #%d · ", thread)
	for _, button := range resumeButtons(t, f) {
		label, _ := button["text"].(string)
		data, _ := button["callback_data"].(string)
		if strings.Contains(label, marker) && data != "" {
			return data
		}
	}
	t.Fatalf("no selectable binding for topic %d", thread)
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
		runtime:  runtimeReleased,
		cancel:   cancel,
		confirms: make(map[string]confirmation),
	})
	recent := time.Now().Add(-7*time.Minute - 30*time.Second).Unix()
	older := time.Now().Add(-4*24*time.Hour - 10*time.Minute).Unix()
	entries := []store.BindingListEntry{
		{Binding: &store.Binding{Bot: 99, Chat: -10, Thread: 11, Workspace: "/current", SessionID: "current-session", Generation: 7, Running: true}},
		{Binding: &store.Binding{Bot: 99, Chat: -10, Thread: 55, Workspace: "/open", SessionID: "open-session", Generation: 5, Running: true}},
		{Binding: &store.Binding{Bot: 99, Chat: -10, Thread: 22, Workspace: "/closed", SessionID: "closed-session", Generation: 4, LastUsedAt: older}, SessionName: "Closed task"},
		{Binding: &store.Binding{Bot: 99, Chat: -10, Thread: 33, Workspace: "/resume", SessionID: "old-session", Generation: 8, LastUsedAt: recent}, Intent: &store.StartIntent{Bot: 99, Chat: -10, Thread: 33, Kind: "resume", Workspace: "/resume", Session: "resume-session", Generation: 9}},
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
	encoded, err := json.Marshal(message["reply_markup"])
	if err != nil {
		t.Fatal(err)
	}
	var keyboard telegram.Keyboard
	if err := json.Unmarshal(encoded, &keyboard); err != nil {
		t.Fatal(err)
	}
	if len(keyboard.InlineKeyboard) != len(entries)+1 {
		t.Fatalf("binding list did not render one row per entry: %+v", keyboard.InlineKeyboard)
	}
	titles := []string{"1. * current · #11", "2. open · #55", "3. Closed task · #22", "4. resume · #33", "5. pending · #44"}
	details := []string{"📖 · - · /current", "📖 · - · /open", "📕 · 4d · /closed", "⏳ · 7m · /resume", "⏳ · - · /pending"}
	for i := range titles {
		row := keyboard.InlineKeyboard[i]
		if len(row) != 1 || !strings.Contains(row[0].Text, titles[i]) || !strings.Contains(row[0].Text, details[i]) {
			t.Fatalf("binding %d did not combine its identity, logical state, age and workspace: %+v", i+1, row)
		}
		if i == 1 || i == 2 {
			if row[0].CallbackData == "" || row[0].Disabled != nil {
				t.Fatalf("binding %d is not selectable: %+v", i+1, row)
			}
		} else if row[0].Disabled == nil || row[0].CallbackData != "" {
			t.Fatalf("binding %d should not offer deletion: %+v", i+1, row)
		}
	}
	if row := keyboard.InlineKeyboard[len(entries)]; len(row) != 1 || row[0].Text != "Close" || row[0].CallbackData == "" {
		t.Fatalf("close button = %+v", row)
	}
}

func TestBindingListSortsCurrentPendingAndRecentAcrossPages(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Unix()
	for _, entry := range []store.Binding{
		{Bot: 99, Chat: -10, Thread: 50, Workspace: "/recent50", Generation: 1, LastUsedAt: now - 100},
		{Bot: 99, Chat: -10, Thread: 40, Workspace: "/unknown40", Generation: 1},
		{Bot: 99, Chat: -10, Thread: 11, Workspace: "/current", Generation: 1, LastUsedAt: now - 500},
		{Bot: 99, Chat: -10, Thread: 30, Workspace: "/old30", Generation: 1, LastUsedAt: now - 1000},
		{Bot: 99, Chat: -10, Thread: 60, Workspace: "/future60", Generation: 1, LastUsedAt: now + 3600},
		{Bot: 99, Chat: -10, Thread: 20, Workspace: "/recent20", Generation: 1, LastUsedAt: now - 100},
		{Bot: 99, Chat: -10, Thread: 41, Workspace: "/unknown41", Generation: 1},
		{Bot: 99, Chat: -10, Thread: 42, Workspace: "/unknown42", Generation: 1},
		{Bot: 99, Chat: -10, Thread: 43, Workspace: "/unknown43", Generation: 1},
		{Bot: 99, Chat: -10, Thread: 44, Workspace: "/unknown44", Generation: 1},
	} {
		if err := db.Save(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.PrepareStart(store.Binding{Bot: 99, Chat: -10, Thread: 7}, store.StartIntent{Bot: 99, Chat: -10, Thread: 7, Kind: "new", Workspace: "/pending", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	w, fake := newBindingTestWorker(t, db)
	w.showBindings(7, false, 0)
	text, rows := pickerView(t, fake)
	titles := []string{
		"1. * current · #11", "2. pending · #7", "3. recent20 · #20",
		"4. recent50 · #50", "5. old30 · #30", "6. unknown40 · #40",
		"7. unknown41 · #41", "8. unknown42 · #42", "9. unknown43 · #43", "10. unknown44 · #44",
	}
	if text != "Saved bindings\nPage 1/2" || len(rows) != 11 {
		t.Fatalf("first page = %q, rows = %+v", text, rows)
	}
	for i, title := range titles {
		if !strings.HasPrefix(rows[i][0]["text"].(string), title+" · ") {
			t.Fatalf("binding %d title = %+v, want prefix %q", i+1, rows[i], title)
		}
	}
	messageID := int64(fake.messageCount())
	w.callback(&telegram.CallbackQuery{ID: "next", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: bindingButton(t, fake, "Next")})
	text, rows = pickerView(t, fake)
	if text != "Saved bindings\nPage 2/2" || len(rows) != 2 || !strings.Contains(rows[0][0]["text"].(string), "future60 · #60 · 📕 · - · /future60") {
		t.Fatalf("second page = %q, rows = %+v", text, rows)
	}
	w.showBindings(7, true, 0)
	text, rows = pickerView(t, fake)
	oldTitles := []string{
		"1. old30 · #30", "2. * current · #11", "3. recent20 · #20",
		"4. recent50 · #50", "5. unknown40 · #40", "6. unknown41 · #41",
		"7. unknown42 · #42", "8. unknown43 · #43", "9. unknown44 · #44", "10. future60 · #60",
	}
	if text != "Saved bindings (oldest first)\nPage 1/2" || len(rows) != 11 {
		t.Fatalf("oldest-first page = %q, rows = %+v", text, rows)
	}
	for i, title := range oldTitles {
		if !strings.HasPrefix(rows[i][0]["text"].(string), title+" · ") {
			t.Fatalf("oldest-first binding %d title = %+v, want prefix %q", i+1, rows[i], title)
		}
	}
	messageID = int64(fake.messageCount())
	w.callback(&telegram.CallbackQuery{ID: "old-next", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: bindingButton(t, fake, "Next")})
	text, rows = pickerView(t, fake)
	if text != "Saved bindings (oldest first)\nPage 2/2" || len(rows) != 2 || !strings.Contains(rows[0][0]["text"].(string), "pending · #7 · ") {
		t.Fatalf("oldest-first second page = %q, rows = %+v", text, rows)
	}
}

func TestBindingsOldPrioritizesClosedThenOldOpen(t *testing.T) {
	db, err := store.Open(t.TempDir())
	requireStoreOK(t, err)
	defer db.Close()
	now := time.Now().Unix()
	for _, binding := range []store.Binding{
		{Thread: 11, LastUsedAt: now - 60},
		{Thread: 30, LastUsedAt: now - 100},
		{Thread: 31, LastUsedAt: now - 100},
		{Thread: 32},
		{Thread: 33, LastUsedAt: now + 3600},
		{Thread: 20, Running: true, LastUsedAt: now - 365*86400},
		{Thread: 21, Running: true, LastUsedAt: now - 365*86400},
		{Thread: 22, Running: true, LastUsedAt: now - 200},
		{Thread: 23, Running: true},
		{Thread: 24, Running: true, LastUsedAt: now + 3600},
		{Thread: 6, Running: true, LastUsedAt: now - 1},
		{Thread: 7, LastUsedAt: now - 2*365*86400},
	} {
		binding.Bot, binding.Chat = 99, -10
		binding.Workspace, binding.Generation = "/sort", 1
		requireStoreOK(t, db.Save(binding))
	}
	for _, thread := range []int64{5, 6, 7} {
		before, err := db.Binding(99, -10, thread)
		if errors.Is(err, sql.ErrNoRows) {
			before = store.Binding{Bot: 99, Chat: -10, Thread: thread}
		} else {
			requireStoreOK(t, err)
		}
		requireStoreOK(t, db.PrepareStart(before, store.StartIntent{Bot: 99, Chat: -10, Thread: thread, Kind: "new", Workspace: "/pending", Generation: before.Generation + 1}))
	}
	w, fake := newBindingTestWorker(t, db)
	w.showBindings(7, true, 0)
	want := []int64{30, 31, 11, 32, 33, 20, 21, 22, 23, 24, 5, 6, 7}
	for start := 0; start < len(want); start += bindingPageSize {
		_, rows := pickerView(t, fake)
		end := min(start+bindingPageSize, len(want))
		if len(rows) != end-start+1 {
			t.Fatalf("page at %d has wrong entry count: %+v", start, rows)
		}
		for i, thread := range want[start:end] {
			if !strings.Contains(rows[i][0]["text"].(string), fmt.Sprintf(" · #%d · ", thread)) {
				t.Fatalf("entry %d = %q, want topic %d", start+i, rows[i][0]["text"], thread)
			}
		}
		if end < len(want) {
			c := onlyBindingConfirmation(t, w)
			w.callback(&telegram.CallbackQuery{ID: "next", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: c.messageID}, Data: bindingButton(t, fake, "Next")})
		}
	}
}

func TestBindingsOldCommandSortsWithoutChangingDefault(t *testing.T) {
	fake, db, send := setupBridge(t)
	now := time.Now().Unix()
	for _, entry := range []store.Binding{
		{Bot: 99, Chat: -10, Thread: 22, Workspace: "/old", Generation: 1, LastUsedAt: now - 7200},
		{Bot: 99, Chat: -10, Thread: 33, Workspace: "/recent", Generation: 1, LastUsedAt: now - 60},
	} {
		if err := db.Save(entry); err != nil {
			t.Fatal(err)
		}
	}
	send(update(1, 11, "/bindings old"))
	waitInputDone(t, db, 1)
	text, rows := pickerView(t, fake)
	if text != "Saved bindings (oldest first)\nPage 1/1" || len(rows) != 3 || !strings.Contains(rows[0][0]["text"].(string), "old · #22 · ") || !strings.Contains(rows[1][0]["text"].(string), "recent · #33 · ") {
		t.Fatalf("oldest-first command = %q, rows = %+v", text, rows)
	}
	send(update(2, 11, "/bindings old extra"))
	waitInputDone(t, db, 2)
	waitFor(t, func() bool { return fake.has(11, "Usage: /bindings [old]") })
	send(update(3, 11, "/bindings"))
	waitInputDone(t, db, 3)
	text, rows = pickerView(t, fake)
	if text != "Saved bindings\nPage 1/1" || len(rows) != 3 || !strings.Contains(rows[0][0]["text"].(string), "recent · #33 · ") || !strings.Contains(rows[1][0]["text"].(string), "old · #22 · ") {
		t.Fatalf("default bindings command = %q, rows = %+v", text, rows)
	}
}

func TestBindingNavigationSharesFooterRow(t *testing.T) {
	w, fake := newBindingTestWorker(t, nil)
	entries := make([]store.BindingListEntry, 2*bindingPageSize+1)
	for i := range entries {
		entries[i].Binding = &store.Binding{Bot: 99, Chat: -10, Thread: int64(i + 20), Generation: 1}
	}
	w.showBindingsPage(confirmation{bindings: entries, user: 7}, 0, 0)
	messageID := int64(fake.messageCount())
	checkFooter := func(page string, rowCount int, labels ...string) {
		t.Helper()
		text, rows := pickerView(t, fake)
		if text != "Saved bindings\n"+page || len(rows) != rowCount {
			t.Fatalf("page = %q, rows = %+v", text, rows)
		}
		footer := rows[len(rows)-1]
		if len(footer) != len(labels) {
			t.Fatalf("footer = %+v, want buttons %v", footer, labels)
		}
		for i, label := range labels {
			if footer[i]["text"] != label || footer[i]["callback_data"] == nil {
				t.Fatalf("footer button %d = %+v, want %q", i, footer[i], label)
			}
		}
	}
	click := func(label string) {
		t.Helper()
		data := bindingButton(t, fake, label)
		if got := w.callback(&telegram.CallbackQuery{ID: label, From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: data}); got != callbackDone {
			t.Fatalf("%s callback result = %v", label, got)
		}
	}
	checkFooter("Page 1/3", 11, "Next", "Close")
	click("Next")
	checkFooter("Page 2/3", 11, "Previous", "Next", "Close")
	click("Next")
	checkFooter("Page 3/3", 2, "Previous", "Close")
	click("Previous")
	checkFooter("Page 2/3", 11, "Previous", "Next", "Close")
}

func TestBindingListDisablesCurrentClosedDelete(t *testing.T) {
	w, fake := newBindingTestWorker(t, nil)
	entries := []store.BindingListEntry{{Binding: &store.Binding{Bot: 99, Chat: -10, Thread: 11, Workspace: "/current", SessionID: "current-session", Generation: 7}}}
	w.showBindingsPage(confirmation{bindings: entries, user: 7}, 0, 0)
	buttons := resumeButtons(t, fake)
	if len(buttons) != 2 || !strings.Contains(buttons[0]["text"].(string), "* current · #11 · 📕 · - · /current") {
		t.Fatalf("current closed buttons = %+v", buttons)
	}
	if _, ok := buttons[0]["callback_data"]; ok {
		t.Fatalf("current closed title unexpectedly has callback data: %+v", buttons[0])
	}
	if _, ok := buttons[0]["disabled"]; !ok {
		t.Fatalf("current closed title is not disabled: %+v", buttons[0])
	}
}

func TestBindingListKeepsTopicVisibleWithLongName(t *testing.T) {
	w, fake := newBindingTestWorker(t, nil)
	entry := store.BindingListEntry{
		Binding:     &store.Binding{Bot: 99, Chat: -10, Thread: 43062, Workspace: "/project/omp-telegram", Generation: 1},
		SessionName: strings.Repeat("a", 100),
	}
	w.showBindingsPage(confirmation{bindings: []store.BindingListEntry{entry}, user: 7}, 0, 0)
	_, rows := pickerView(t, fake)
	if len(rows) != 2 || !strings.Contains(rows[0][0]["text"].(string), "... · #43062 · 📕 · - · /project/omp-telegram") || rows[0][0]["callback_data"] == nil {
		t.Fatalf("long title obscured topic or delete action: %+v", rows)
	}
}

func TestBindingNameLookupKeepsRecentOrder(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	sessions := []resumeFixtureSession{
		{ID: "abcd0000-0000-4000-8000-000000000000", CWD: workspace, Title: "Zzz session"},
		{ID: "abcd0000-0000-4000-8000-000000000001", CWD: workspace, Title: "Aaa session"},
	}
	raw, err := json.Marshal(sessions)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS", string(raw))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, session := range sessions {
		if err := db.Save(store.Binding{Bot: 99, Chat: -10, Thread: int64(22 + 11*i), Workspace: workspace, SessionID: session.ID, Generation: 1, LastUsedAt: time.Now().Unix() - int64(100*(i+1))}); err != nil {
			t.Fatal(err)
		}
	}
	w, fake := newBindingTestWorker(t, db)
	w.b.cfg.OMP = binary
	w.showBindings(7, false, 0)
	_, rows := pickerView(t, fake)
	if len(rows) != 3 || !strings.Contains(rows[0][0]["text"].(string), filepath.Base(workspace)+" · #22 · ") || !strings.Contains(rows[1][0]["text"].(string), filepath.Base(workspace)+" · #33 · ") {
		t.Fatalf("initial recent order = %+v", rows)
	}
	var result bindingNamesResult
	select {
	case result = <-w.bindingNameResults:
	case <-time.After(5 * time.Second):
		t.Fatal("session name lookup did not finish")
	}
	w.bindingNamesLoaded(result)
	w.background.Wait()
	_, rows = pickerView(t, fake)
	if len(rows) != 3 || !strings.Contains(rows[0][0]["text"].(string), "Zzz session · #22 · ") || !strings.Contains(rows[1][0]["text"].(string), "Aaa session · #33 · ") || rows[0][0]["callback_data"] == nil || rows[1][0]["callback_data"] == nil {
		t.Fatalf("native titles reordered bindings: %+v", rows)
	}
	w.showBindings(7, true, 0)
	text, rows := pickerView(t, fake)
	if text != "Saved bindings (oldest first)\nPage 1/1" || !strings.Contains(rows[0][0]["text"].(string), filepath.Base(workspace)+" · #33 · ") || !strings.Contains(rows[1][0]["text"].(string), filepath.Base(workspace)+" · #22 · ") {
		t.Fatalf("initial oldest-first order = %q, rows = %+v", text, rows)
	}
	select {
	case result = <-w.bindingNameResults:
	case <-time.After(5 * time.Second):
		t.Fatal("oldest-first session name lookup did not finish")
	}
	w.bindingNamesLoaded(result)
	w.cancel()
	w.background.Wait()
	text, rows = pickerView(t, fake)
	if text != "Saved bindings (oldest first)\nPage 1/1" || !strings.Contains(rows[0][0]["text"].(string), "Aaa session · #33 · ") || !strings.Contains(rows[1][0]["text"].(string), "Zzz session · #22 · ") {
		t.Fatalf("native titles reordered oldest-first bindings: %q, rows = %+v", text, rows)
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
		text, rows := pickerView(t, fake)
		if text != "Saved bindings\nPage 2/2" || len(rows) != 2 || !strings.Contains(rows[0][0]["text"].(string), "unknown · #30 · 📖 · - · unknown") || rows[0][0]["callback_data"] == nil {
			t.Fatalf("last page lost binding title action: text=%q rows=%+v", text, rows)
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

func TestBindingCloseRetriesFailedKeyboardRemoval(t *testing.T) {
	w, fake := newBindingTestWorker(t, nil)
	entries := []store.BindingListEntry{{Binding: &store.Binding{Bot: 99, Chat: -10, Thread: 22, Generation: 1, Running: true}}}
	w.showBindingsPage(confirmation{bindings: entries, user: 7}, 0, 0)
	messageID := int64(fake.messageCount())
	data := bindingButton(t, fake, "Close")
	click := func() {
		t.Helper()
		if result := w.callback(&telegram.CallbackQuery{ID: "close", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: data}); result != callbackDone {
			t.Fatalf("close callback result = %v", result)
		}
	}
	fake.failKeyboardClear = true
	click()
	if got := lastCallback(t, fake); got != "Could not close the bindings menu. Tap Close again." {
		t.Fatalf("failed cleanup callback = %q", got)
	}
	if c := onlyBindingConfirmation(t, w); c.messageID != messageID {
		t.Fatalf("failed cleanup lost the menu identity: %+v", c)
	}
	fake.failKeyboardClear = false
	click()
	if got := lastCallback(t, fake); got != "Closed" || len(w.confirms) != 0 {
		t.Fatalf("retry result = %q, confirmations = %d", got, len(w.confirms))
	}
	assertKeyboardClears(t, fake, int(messageID), int(messageID))
}

func TestBindingDeleteCallbackDeletesClosedBinding(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	closed := store.Binding{Bot: 99, Chat: -10, Thread: 22, Workspace: filepath.Join(t.TempDir(), "missing", "workspace"), Session: filepath.Join(t.TempDir(), "closed.jsonl"), Generation: 4}
	if err := os.WriteFile(closed.Session, []byte("native history"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = db.Save(closed); err != nil {
		t.Fatal(err)
	}
	w, fake := newBindingTestWorker(t, db)
	w.showBindingsPage(confirmation{bindings: []store.BindingListEntry{{Binding: &closed}}, user: 7}, 0, 0)
	listMessageID := int64(fake.messageCount())
	w.callback(&telegram.CallbackQuery{ID: "list-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: listMessageID}, Data: bindingEntryButton(t, fake, closed.Thread)})
	confirmation := onlyBindingConfirmation(t, w)
	if confirmation.action != "binding_delete" || confirmation.messageID == listMessageID {
		t.Fatalf("delete confirmation = %+v", confirmation)
	}
	if _, err = db.Binding(closed.Bot, closed.Chat, closed.Thread); err != nil {
		t.Fatalf("selecting the entry removed the binding before confirmation: %v", err)
	}
	_, confirmationRows := pickerView(t, fake)
	if len(confirmationRows) != 1 || len(confirmationRows[0]) != 3 || confirmationRows[0][0]["text"] != "Binding" || confirmationRows[0][0]["style"] != "danger" || confirmationRows[0][1]["text"] != "Binding+Session" || confirmationRows[0][1]["style"] != "danger" || confirmationRows[0][2]["text"] != "Cancel" || confirmationRows[0][2]["style"] != nil {
		t.Fatalf("delete confirmation buttons = %+v", confirmationRows)
	}
	deleteMessageID := confirmation.messageID
	w.callback(&telegram.CallbackQuery{ID: "confirm-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: deleteMessageID}, Data: bindingButton(t, fake, "Binding")})
	if _, err = db.Binding(closed.Bot, closed.Chat, closed.Thread); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted binding lookup error = %v", err)
	}
	if len(w.confirms) != 0 {
		t.Fatalf("delete left confirmations: %+v", w.confirms)
	}
	assertFinishedMenu(t, fake, int(deleteMessageID), "Selected: Binding")
	if data, err := os.ReadFile(closed.Session); err != nil || string(data) != "native history" {
		t.Fatalf("Binding changed native history: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Dir(closed.Workspace)); !os.IsNotExist(err) {
		t.Fatalf("Binding created a missing workspace component: %v", err)
	}
}

func TestBindingsOldKeepsOrderAfterDeletingClosedEntry(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Unix()
	for _, entry := range []store.Binding{
		{Bot: 99, Chat: -10, Thread: 22, Workspace: "/old", Generation: 1, LastUsedAt: now - 7200},
		{Bot: 99, Chat: -10, Thread: 33, Workspace: "/middle", Generation: 1, LastUsedAt: now - 3600},
		{Bot: 99, Chat: -10, Thread: 44, Workspace: "/recent", Generation: 1, LastUsedAt: now - 60},
	} {
		if err := db.Save(entry); err != nil {
			t.Fatal(err)
		}
	}
	w, fake := newBindingTestWorker(t, db)
	w.showBindings(7, true, 0)
	listID := int64(fake.messageCount())
	w.callback(&telegram.CallbackQuery{ID: "choose-old", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: listID}, Data: bindingEntryButton(t, fake, 22)})
	confirmID := onlyBindingConfirmation(t, w).messageID
	w.callback(&telegram.CallbackQuery{ID: "delete-old", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: confirmID}, Data: bindingButton(t, fake, "Binding")})
	if _, err := db.Binding(99, -10, 22); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old binding still present: %v", err)
	}
	text, rows := pickerView(t, fake)
	if text != "Saved bindings (oldest first)\nPage 1/1" || len(rows) != 3 || !strings.Contains(rows[0][0]["text"].(string), "middle · #33 · ") || !strings.Contains(rows[1][0]["text"].(string), "recent · #44 · ") {
		t.Fatalf("oldest-first list after deletion = %q, rows = %+v", text, rows)
	}
}

func TestBindingDeletionRefreshesSourcePage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		old    bool
		page   int
		target int64
		first  int64
	}{
		{name: "recent middle", page: 1, target: 30, first: 31},
		{name: "old middle", old: true, page: 1, target: 30, first: 29},
		{name: "recent last", page: 2, target: 40, first: 30},
		{name: "old last", old: true, page: 2, target: 20, first: 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			now := time.Now().Unix()
			for i := range 2*bindingPageSize + 1 {
				if err := db.Save(store.Binding{Bot: 99, Chat: -10, Thread: int64(20 + i), Workspace: "/binding", Generation: 1, LastUsedAt: now - int64(i+1)*60}); err != nil {
					t.Fatal(err)
				}
			}
			w, fake := newBindingTestWorker(t, db)
			w.showBindings(7, tc.old, 0)
			listID := int64(fake.messageCount())
			click := func(id int64, label string) {
				t.Helper()
				data := bindingButton(t, fake, label)
				if got := w.callback(&telegram.CallbackQuery{ID: label, From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: id}, Data: data}); got != callbackDone {
					t.Fatalf("%s callback result = %v", label, got)
				}
			}
			for range tc.page {
				click(listID, "Next")
			}
			data := bindingEntryButton(t, fake, tc.target)
			w.callback(&telegram.CallbackQuery{ID: "select-binding", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: listID}, Data: data})
			click(onlyBindingConfirmation(t, w).messageID, "Binding")
			text, rows := pickerView(t, fake)
			header := "Saved bindings"
			if tc.old {
				header += " (oldest first)"
			}
			if text != header+"\nPage 2/2" || len(rows) != 11 || !strings.Contains(rows[0][0]["text"].(string), fmt.Sprintf(" · #%d · ", tc.first)) {
				t.Fatalf("refreshed page = %q, rows = %+v", text, rows)
			}
			w.cancel()
			w.background.Wait()
		})
	}
}

func TestBindingDeleteTitleTargetsMatchingEntry(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first := store.Binding{Bot: 99, Chat: -10, Thread: 22, Workspace: "/first", Generation: 1}
	second := store.Binding{Bot: 99, Chat: -10, Thread: 33, Workspace: "/" + strings.Repeat("segment/", 480) + "target", Generation: 1}
	name := strings.Repeat("长😀 ", 20) + "selected"
	for _, binding := range []store.Binding{first, second} {
		if err := db.Save(binding); err != nil {
			t.Fatal(err)
		}
	}
	w, fake := newBindingTestWorker(t, db)
	w.showBindingsPage(confirmation{bindings: []store.BindingListEntry{{Binding: &first}, {Binding: &second, SessionName: "\t" + strings.ReplaceAll(name, " ", "\x00\n") + "\t"}}, user: 7}, 0, 0)
	listMessageID := int64(fake.messageCount())
	w.callback(&telegram.CallbackQuery{ID: "select-second", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: listMessageID}, Data: bindingEntryButton(t, fake, second.Thread)})
	confirm := onlyBindingConfirmation(t, w)
	if confirm.deleteThread != second.Thread {
		t.Fatalf("second title selected topic %d", confirm.deleteThread)
	}
	fake.mu.Lock()
	var details strings.Builder
	for _, message := range fake.messages[listMessageID:] {
		text, _ := message["text"].(string)
		details.WriteString(text)
	}
	fake.mu.Unlock()
	if !strings.Contains(details.String(), name) || !strings.Contains(details.String(), second.Workspace) {
		t.Fatal("confirmation lost the selected binding's full name or workspace across message chunks")
	}
	if _, err := db.Binding(second.Bot, second.Chat, second.Thread); err != nil {
		t.Fatalf("selecting the long entry deleted it before confirmation: %v", err)
	}
	w.callback(&telegram.CallbackQuery{ID: "delete-second", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: confirm.messageID}, Data: bindingButton(t, fake, "Binding")})
	if _, err := db.Binding(second.Bot, second.Chat, second.Thread); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second binding still present: %v", err)
	}
	if _, err := db.Binding(first.Bot, first.Chat, first.Thread); err != nil {
		t.Fatalf("first binding was changed: %v", err)
	}
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
	w.callback(&telegram.CallbackQuery{ID: "list-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: listMessageID}, Data: bindingEntryButton(t, fake, old.Thread)})
	confirmation := onlyBindingConfirmation(t, w)
	replacement := old
	replacement.Generation = 5
	replacement.Workspace = "/replacement"
	if err = db.Save(replacement); err != nil {
		t.Fatal(err)
	}
	w.callback(&telegram.CallbackQuery{ID: "confirm-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: confirmation.messageID}, Data: bindingButton(t, fake, "Binding")})
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
	a.callback(&telegram.CallbackQuery{ID: "a-list-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: aListMessageID}, Data: bindingEntryButton(t, fake, closed.Thread)})
	aDelete := onlyBindingConfirmation(t, a)
	aDeleteData := bindingButton(t, fake, "Binding")
	staleList := newWorker(target{chat: -10, thread: 44})
	staleList.showBindingsPage(confirmation{bindings: entries, user: 7, epoch: b.bindingsEpoch.Load()}, 0, 0)
	staleListMessageID := int64(fake.messageCount())
	staleListData := bindingEntryButton(t, fake, closed.Thread)

	c.showBindingsPage(confirmation{bindings: entries, user: 7, epoch: b.bindingsEpoch.Load()}, 0, 0)
	cListMessageID := int64(fake.messageCount())
	c.callback(&telegram.CallbackQuery{ID: "c-list-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: cListMessageID}, Data: bindingEntryButton(t, fake, closed.Thread)})
	cDelete := onlyBindingConfirmation(t, c)
	c.callback(&telegram.CallbackQuery{ID: "c-confirm-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: cDelete.messageID}, Data: bindingButton(t, fake, "Binding")})
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

func TestBindingDeleteCancelKeepsBinding(t *testing.T) {
	db, err := store.Open(t.TempDir())
	requireStoreOK(t, err)
	defer db.Close()
	closed := store.Binding{Bot: 99, Chat: -10, Thread: 22, Workspace: "/closed", Session: "/sessions/closed.jsonl", Generation: 4}
	requireStoreOK(t, db.Save(closed))
	w, fake := newBindingTestWorker(t, db)
	w.showBindingsPage(confirmation{bindings: []store.BindingListEntry{{Binding: &closed}}, user: 7}, 0, 0)
	w.callback(&telegram.CallbackQuery{ID: "list-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: int64(fake.messageCount())}, Data: bindingEntryButton(t, fake, closed.Thread)})
	c := onlyBindingConfirmation(t, w)
	approve := bindingButton(t, fake, "Binding")
	w.callback(&telegram.CallbackQuery{ID: "unauthorized-delete", From: telegram.User{ID: 8}, Message: &telegram.Message{MessageID: c.messageID}, Data: approve})
	if saved, err := db.Binding(closed.Bot, closed.Chat, closed.Thread); err != nil || !sameBindingIdentity(saved, closed) || len(w.confirms) != 1 {
		t.Fatalf("unauthorized Binding callback changed binding or consumed owner menu: %+v, %v", saved, err)
	}
	w.callback(&telegram.CallbackQuery{ID: "cancel-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: c.messageID}, Data: bindingButton(t, fake, "Cancel")})
	text := assertFinishedMenu(t, fake, int(c.messageID), "Cancel")
	if !strings.Contains(text, "Topic 22") {
		t.Fatal("cancelled binding deletion lost its conversation identity")
	}
	w.callback(&telegram.CallbackQuery{ID: "replay-delete", From: telegram.User{ID: 7}, Data: approve})
	saved, err := db.Binding(closed.Bot, closed.Chat, closed.Thread)
	requireStoreOK(t, err)
	if !sameBindingIdentity(saved, closed) || len(w.confirms) != 0 {
		t.Fatal("cancelled binding deletion changed the binding or remained actionable")
	}
}

func TestBindingAgeUnitBoundaries(t *testing.T) {
	const now = int64(2_000_000_000)
	const day = int64(24 * 60 * 60)
	for _, tc := range []struct {
		name      string
		timestamp int64
		want      string
	}{
		{name: "missing", timestamp: 0, want: "-"},
		{name: "negative", timestamp: -1, want: "-"},
		{name: "future", timestamp: now + 1, want: "-"},
		{name: "current", timestamp: now, want: "now"},
		{name: "under minute", timestamp: now - 59, want: "now"},
		{name: "minute", timestamp: now - 60, want: "1m"},
		{name: "under hour", timestamp: now - 3599, want: "59m"},
		{name: "hour", timestamp: now - 3600, want: "1h"},
		{name: "under day", timestamp: now - day + 1, want: "23h"},
		{name: "day", timestamp: now - day, want: "1d"},
		{name: "discard smaller units", timestamp: now - 3*day - 12*3600, want: "3d"},
		{name: "under week", timestamp: now - 7*day + 1, want: "6d"},
		{name: "week", timestamp: now - 7*day, want: "1w"},
		{name: "under month", timestamp: now - 30*day + 1, want: "4w"},
		{name: "month", timestamp: now - 30*day, want: "1mo"},
		{name: "under second month", timestamp: now - 60*day + 1, want: "1mo"},
		{name: "second month", timestamp: now - 60*day, want: "2mo"},
		{name: "under year", timestamp: now - 365*day + 1, want: "12mo"},
		{name: "year", timestamp: now - 365*day, want: "1y"},
		{name: "second year", timestamp: now - 730*day, want: "2y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatBindingAge(tc.timestamp, now); got != tc.want {
				t.Fatalf("age = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestQueuePreviewLocalizesOnlyBridgeFallbacks(t *testing.T) {
	if got := queueTaskText("zh", queued{preparing: true, album: true}); got != "排队中的相册" {
		t.Fatalf("album fallback was not localized: %q", got)
	}
	for _, q := range []queued{
		{displayText: "Queued task", preparing: true, album: true},
		{displayText: "Preparing attachment..."},
		{text: "Queued task"},
	} {
		want := q.displayText
		if want == "" {
			want = q.text
		}
		if got := queueTaskText("zh", q); got != want {
			t.Fatalf("user queue text was translated: got %q, want %q", got, want)
		}
	}
}
