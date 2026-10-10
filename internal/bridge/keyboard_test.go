package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"omp-telegram/internal/telegram"
)

func assertKeyboardClears(t *testing.T, f *fakeHTTP, messageIDs ...int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.keyboardClears) != len(messageIDs) {
		t.Fatalf("keyboard clears = %v, want message IDs %v", f.keyboardClears, messageIDs)
	}
	for i, id := range messageIDs {
		request := f.keyboardClears[i]
		if request["chat_id"] != float64(-10) || request["message_id"] != float64(id) {
			t.Fatalf("keyboard clear %d targeted %v, want chat -10 message %d", i, request, id)
		}
	}
}

func assertFinishedMenu(t *testing.T, f *fakeHTTP, messageID int, expected string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.messages) - 1; i >= 0; i-- {
		message := f.messages[i]
		if message["chat_id"] != float64(-10) || message["message_id"] != float64(messageID) {
			continue
		}
		text, _ := message["text"].(string)
		if !strings.Contains(text, expected) {
			t.Fatalf("menu %d does not show %q: %q", messageID, expected, text)
		}
		raw, err := json.Marshal(message["reply_markup"])
		var keyboard telegram.Keyboard
		if err != nil || json.Unmarshal(raw, &keyboard) != nil || len(keyboard.InlineKeyboard) != 0 {
			t.Fatalf("completed menu %d retains choices: %v", messageID, message["reply_markup"])
		}
		return text
	}
	t.Fatalf("original menu %d was not updated", messageID)
	return ""
}

// Omit the callback message to exercise the identity saved when the menu was sent.
func clickKeyboard(w *worker, user int64, data string) {
	w.callback(&telegram.CallbackQuery{ID: "keyboard-callback", From: telegram.User{ID: user}, Data: data})
	for w.controlInProgress() {
		researchControl := w.research.control != nil
		var events <-chan json.RawMessage
		if researchControl && w.client != nil {
			events = w.client.Events()
		}
		select {
		case result := <-w.operations:
			w.operationReturned(result)
		case raw, ok := <-events:
			if ok {
				w.event(raw)
			} else {
				w.failed()
			}
		case result := <-w.nativeCatalog.results:
			w.commandCatalogFinished(result)
		case <-time.After(5 * time.Second):
			panic("control operation did not complete")
		}
		if researchControl {
			w.dispatch()
		}
	}
}

func TestNewConfirmationDisplaysDecision(t *testing.T) {
	for _, scenario := range []string{"approve", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			before, client := w.binding, w.client
			command("/new")
			buttons := resumeButtons(t, f)
			messageID := f.messageCount()
			index := 0
			if scenario == "cancel" {
				index = 1
			}
			clickKeyboard(w, 7, buttons[index]["callback_data"].(string))
			expected := "Confirmed"
			if scenario == "cancel" {
				expected = "Cancel"
			}
			text := assertFinishedMenu(t, f, messageID, expected)
			if scenario == "cancel" {
				if !sameBindingIdentity(w.binding, before) || w.client != client {
					t.Fatal("cancel replaced the running session")
				}
			} else if w.binding.Session == before.Session || w.binding.Generation <= before.Generation || w.binding.Workspace != before.Workspace || w.client == nil {
				t.Fatal("approval did not create a fresh session in the existing workspace")
			}
			after := w.binding
			// Both approval replay and approval after cancellation must be inert.
			clickKeyboard(w, 7, buttons[0]["callback_data"].(string))
			if after := assertFinishedMenu(t, f, messageID, expected); after != text {
				t.Fatal("a consumed confirmation changed the displayed decision")
			}
			if !sameBindingIdentity(w.binding, after) {
				t.Fatal("consumed confirmation changed the session again")
			}
		})
	}
}

func TestConfirmationOwnerAndExpiryKeyboardCleanup(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before := w.binding
	command("/new")
	data := resumeButtons(t, f)[0]["callback_data"].(string)
	messageID := f.messageCount()
	clickKeyboard(w, 8, data)
	_, rows := pickerView(t, f)
	if len(rows) == 0 {
		t.Fatal("unauthorized callback removed the owner's choices")
	}
	if !sameBindingIdentity(w.binding, before) {
		t.Fatal("another user replaced the session")
	}
	// The owner can still use the same menu after an unauthorized click.
	clickKeyboard(w, 7, data)
	assertFinishedMenu(t, f, messageID, "Confirmed")
	if w.binding.Session == before.Session {
		t.Fatal("unauthorized click consumed the owner's confirmation")
	}

	before = w.binding
	command("/new")
	data = resumeButtons(t, f)[0]["callback_data"].(string)
	expiredMessageID := f.messageCount()
	token, _, _ := strings.Cut(data, ":")
	c := w.confirms[token]
	c.expires = time.Now().Add(-time.Second)
	w.confirms[token] = c
	// Expiry must not allow a different user to clear the owner's keyboard.
	clickKeyboard(w, 8, data)
	_, rows = pickerView(t, f)
	if len(rows) == 0 {
		t.Fatal("unauthorized callback removed expired choices")
	}
	clickKeyboard(w, 7, data)
	assertKeyboardClears(t, f, expiredMessageID)
	clickKeyboard(w, 7, data)
	assertKeyboardClears(t, f, expiredMessageID)
	if !sameBindingIdentity(w.binding, before) {
		t.Fatal("expired confirmation replaced the session")
	}
}

func TestExpiredKeyboardCleanupDoesNotBlockWorker(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	w.b.cfg.ProgressMode = "off"
	command("/new " + t.TempDir())
	command("wait")
	w.dispatch()
	active := w.active
	for range 40 {
		command("/new")
	}
	for token, c := range w.confirms {
		c.expires = time.Now().Add(-time.Second)
		w.confirms[token] = c
	}
	f.keyboardClearGate = make(chan struct{})
	w.input = make(chan incoming, 1)
	done := make(chan struct{})
	go func() {
		w.run()
		close(done)
	}()
	t.Cleanup(func() {
		w.cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("worker did not cancel blocked keyboard cleanup during shutdown")
		}
	})
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.keyboardClears) != 0
	})
	u := update(1000, 11, "/stop")
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(u.UpdateID, raw); err != nil {
		t.Fatal(err)
	}
	w.input <- incoming{id: u.UpdateID, msg: u.Message}
	// Both controls and terminal events must advance while cleanup is stuck.
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var stopped, settled string
		_ = w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", u.UpdateID).Scan(&stopped)
		_ = w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", active).Scan(&settled)
		if stopped == "done" && settled != "submitted" && settled != "pending" && settled != "" {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("expired keyboard cleanup blocked /stop or terminal event processing")
		case <-tick.C:
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.keyboardClears) != 1 {
		t.Fatal("expired keyboard cleanup launched concurrent requests")
	}
}

func TestNativeUISelectionShowsSinglePageWithoutNavigation(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	w.owner = 7
	options := make([]string, uiSelectPageSize)
	for i := range options {
		options[i] = fmt.Sprintf("commit %02d", i)
	}
	raw, err := json.Marshal(map[string]any{"type": "extension_ui_request", "id": "review-commits", "method": "select", "title": "Choose commit", "options": options})
	if err != nil {
		t.Fatal(err)
	}
	w.event(raw)
	text, rows := pickerView(t, f)
	if strings.Contains(text, "Page ") || len(rows) != len(options)+1 {
		t.Fatalf("single-page commit choices = %q; %v", text, rows)
	}
	requirePickerFooter(t, rows, "Cancel")
	data := rows[len(options)-1][0]["callback_data"].(string)
	messageID := int64(f.messageCount())
	w.callback(&telegram.CallbackQuery{ID: "select-commit", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: data})
	result, err := w.call("get_state", nil)
	var state struct {
		Replies int    `json:"fixtureUIReplies"`
		Value   string `json:"fixtureUIValue"`
	}
	if err != nil || json.Unmarshal(result, &state) != nil || state.Replies != 1 || state.Value != options[len(options)-1] {
		t.Fatalf("selected native commit = %+v, error %v", state, err)
	}
}

func TestNativeUISelectionPaginatesWithoutSubmittingNavigation(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	w.owner = 7
	options := make([]string, 22)
	for i := range options {
		options[i] = fmt.Sprintf("commit %02d", i)
	}
	raw, err := json.Marshal(map[string]any{"type": "extension_ui_request", "id": "review-commits", "method": "select", "title": "Choose commit", "options": options})
	if err != nil {
		t.Fatal(err)
	}
	w.event(raw)
	text, rows := pickerView(t, f)
	if !strings.Contains(text, "Page 1/3") || len(rows) != uiSelectPageSize+1 {
		t.Fatalf("first commit page = %q; %v", text, rows)
	}
	requirePickerFooter(t, rows, "Next", "Cancel")
	first := rows[0][0]["callback_data"].(string)
	next := rows[uiSelectPageSize][0]["callback_data"].(string)
	messageID := int64(f.messageCount())
	click := func(user int64, data string) {
		w.callback(&telegram.CallbackQuery{ID: "review-callback", From: telegram.User{ID: user}, Message: &telegram.Message{MessageID: messageID}, Data: data})
	}
	click(8, next)
	text, _ = pickerView(t, f)
	if !strings.Contains(text, "Page 1/3") {
		t.Fatal("another user paged the native commit menu")
	}
	click(7, next)
	text, rows = pickerView(t, f)
	if !strings.Contains(text, "Page 2/3") || len(rows) != uiSelectPageSize+1 {
		t.Fatalf("middle commit page = %q; %v", text, rows)
	}
	requirePickerFooter(t, rows, "Previous", "Next", "Cancel")
	click(7, first)
	if _, exists := w.confirms[strings.SplitN(first, ":", 2)[0]]; exists {
		t.Fatal("old commit page retained an actionable token")
	}
	click(7, rows[uiSelectPageSize][0]["callback_data"].(string))
	text, rows = pickerView(t, f)
	if !strings.Contains(text, "Page 1/3") {
		t.Fatalf("previous did not return to first commit page: %q", text)
	}
	click(7, rows[uiSelectPageSize][0]["callback_data"].(string))
	text, rows = pickerView(t, f)
	if !strings.Contains(text, "Page 2/3") {
		t.Fatalf("next did not reopen middle commit page: %q", text)
	}
	click(7, rows[uiSelectPageSize][1]["callback_data"].(string))
	text, rows = pickerView(t, f)
	if !strings.Contains(text, "Page 3/3") || len(rows) != 7 || rows[4][0]["text"] != options[20] {
		t.Fatalf("last commit page = %q; %v", text, rows)
	}
	requirePickerFooter(t, rows, "Previous", "Cancel")
	result, err := w.call("get_state", nil)
	var state struct {
		Replies int    `json:"fixtureUIReplies"`
		Value   string `json:"fixtureUIValue"`
	}
	if err != nil || json.Unmarshal(result, &state) != nil || state.Replies != 0 {
		t.Fatalf("navigation submitted a native choice: %+v, error %v", state, err)
	}
	click(7, rows[4][0]["callback_data"].(string))
	result, err = w.call("get_state", nil)
	if err != nil || json.Unmarshal(result, &state) != nil || state.Replies != 1 || state.Value != options[20] {
		t.Fatalf("last-page choice = %+v, error %v", state, err)
	}
	raw, err = json.Marshal(map[string]any{"type": "extension_ui_request", "id": "review-cancel", "method": "select", "title": "Choose commit", "options": options[:9]})
	if err != nil {
		t.Fatal(err)
	}
	w.event(raw)
	_, rows = pickerView(t, f)
	messageID = int64(f.messageCount())
	click(7, rows[uiSelectPageSize][0]["callback_data"].(string))
	text, rows = pickerView(t, f)
	if !strings.Contains(text, "Page 2/2") || len(rows) != 2 {
		t.Fatalf("cancel page = %q; %v", text, rows)
	}
	requirePickerFooter(t, rows, "Previous", "Cancel")
	click(7, rows[1][1]["callback_data"].(string))
	result, err = w.call("get_state", nil)
	if err != nil || json.Unmarshal(result, &state) != nil || state.Replies != 2 || state.Value != "" {
		t.Fatalf("cancel response = %+v, error %v", state, err)
	}
}

func TestCompactConfirmationDisplaysReceiptBeforeCompletion(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before := w.binding
	w.operations = make(chan operationResult, 1)
	command("/compact")
	data := resumeButtons(t, f)[0]["callback_data"].(string)
	messageID := f.messageCount()
	clickKeyboard(w, 7, data)
	text := assertFinishedMenu(t, f, messageID, "Confirmed")
	if w.sessionOp != sessionOperationCompact || w.taskActive() || !sameBindingIdentity(w.binding, before) {
		t.Fatal("compaction confirmation did not start compaction in the current session")
	}
	select {
	case result := <-w.operations:
		if result.err != nil {
			t.Fatalf("compaction failed: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not finish")
	}
	clickKeyboard(w, 7, data)
	if after := assertFinishedMenu(t, f, messageID, "Confirmed"); after != text {
		t.Fatal("a consumed compact confirmation changed its receipt")
	}
}

func TestResumeKeyboardNavigationAndSelection(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	before := w.binding
	sessions := setResumeFixtures(t, before.Workspace, 10)
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	first := resumeButtons(t, f)
	messageID := f.messageCount()
	clickResume(w, 7, first[8]["callback_data"].(string))
	second := resumeButtons(t, f)
	assertKeyboardClears(t, f)
	// A callback from the replaced page cannot clear the new page or select.
	clickResume(w, 7, first[0]["callback_data"].(string))
	assertKeyboardClears(t, f)
	if !sameBindingIdentity(w.binding, before) {
		t.Fatal("stale first-page selection changed the session")
	}
	clickResume(w, 7, second[2]["callback_data"].(string))
	current := resumeButtons(t, f)
	assertKeyboardClears(t, f)
	if !sameBindingIdentity(w.binding, before) {
		t.Fatal("navigation changed the session")
	}
	clickResume(w, 7, second[0]["callback_data"].(string))
	assertKeyboardClears(t, f)
	if !sameBindingIdentity(w.binding, before) {
		t.Fatal("stale second-page selection changed the session")
	}
	data := current[1]["callback_data"].(string)
	clickResume(w, 7, data)
	// Navigation edits the existing Telegram message, not its replacement ID.
	text := assertFinishedMenu(t, f, messageID, sessions[1].ID)
	if w.sessionID != sessions[1].ID || w.binding.Generation <= before.Generation || w.binding.Workspace != before.Workspace {
		t.Fatal("current-page selection did not restore the chosen session")
	}
	after := w.binding
	clickResume(w, 7, data)
	if after := assertFinishedMenu(t, f, messageID, sessions[1].ID); after != text {
		t.Fatal("a consumed resume selection changed its receipt")
	}
	if !sameBindingIdentity(w.binding, after) {
		t.Fatal("replayed picker selection restarted the session")
	}
}

func waitKeyboardClears(t *testing.T, f *fakeHTTP, messageIDs ...int) {
	t.Helper()
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.keyboardClears) >= len(messageIDs)
	})
	assertKeyboardClears(t, f, messageIDs...)
}

// Observe cleanup lifetime and callback receipts without polling actor state.
type failedMenuHTTP struct {
	base          *fakeHTTP
	clearStarted  chan struct{}
	clearFinished chan struct{}
	callbacks     chan string
	mu            sync.Mutex
	active        int
	concurrent    bool
}

func (f *failedMenuHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/editMessageText") {
		return nil, errors.New("menu text edit failed")
	}
	if strings.HasSuffix(r.URL.Path, "/editMessageReplyMarkup") {
		f.mu.Lock()
		f.active++
		f.concurrent = f.concurrent || f.active > 1
		f.mu.Unlock()
		f.clearStarted <- struct{}{}
		defer func() {
			f.mu.Lock()
			f.active--
			f.mu.Unlock()
			f.clearFinished <- struct{}{}
		}()
	}
	response, err := f.base.RoundTrip(r)
	if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") && err == nil {
		f.base.mu.Lock()
		text := f.base.callbacks[len(f.base.callbacks)-1]
		f.base.mu.Unlock()
		f.callbacks <- text
	}
	return response, err
}

func TestFailedMenuCleanupDoesNotBlockWorker(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	w.owner = 7
	client := w.client
	var choices []string
	var messageIDs []int64
	for _, id := range []string{"first-review", "second-review"} {
		w.ui(rpcEvent{ID: id, Method: "select", Title: "Choose commit", Options: []string{id}})
		_, rows := pickerView(t, f)
		choices = append(choices, rows[0][0]["callback_data"].(string))
		messageIDs = append(messageIDs, int64(f.messageCount()))
	}
	gate := make(chan struct{})
	f.keyboardClearGate = gate
	transport := &failedMenuHTTP{
		base: f, clearStarted: make(chan struct{}, 8), clearFinished: make(chan struct{}, 8), callbacks: make(chan string, 8),
	}
	http.DefaultTransport = transport
	w.input = make(chan incoming, 8)
	done := make(chan struct{})
	go func() {
		w.run()
		close(done)
	}()
	t.Cleanup(func() {
		w.cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("worker did not cancel failed-menu keyboard cleanup")
		}
	})
	// A single watchdog bounds all barriers; no elapsed-time assertion is needed.
	watchdog := time.NewTimer(3 * time.Second)
	defer watchdog.Stop()
	wait := func(ch <-chan struct{}, failure string) {
		t.Helper()
		select {
		case <-ch:
		case <-watchdog.C:
			t.Fatal(failure)
		}
	}
	receipt := func(expected string) {
		t.Helper()
		select {
		case text := <-transport.callbacks:
			if text != expected {
				t.Fatalf("callback receipt = %q, want %q", text, expected)
			}
		case <-watchdog.C:
			t.Fatal("blocked keyboard cleanup prevented another control input")
		}
	}
	var updateID int64 = 1000
	click := func(index int) {
		t.Helper()
		updateID++
		q := &telegram.CallbackQuery{ID: fmt.Sprint(updateID), From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageIDs[index]}, Data: choices[index]}
		u := telegram.Update{UpdateID: updateID, CallbackQuery: q}
		raw, err := json.Marshal(u)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.b.db.Accept(updateID, raw); err != nil {
			t.Fatal(err)
		}
		w.input <- incoming{id: updateID, callback: q}
	}
	click(0)
	receipt("Received")
	wait(transport.clearStarted, "failed menu did not queue keyboard cleanup")
	// The first clear stays gated while a different menu is submitted and both
	// consumed menus are replayed. Expiry receipts are visible to the user.
	click(1)
	click(0)
	click(1)
	receipt("Received")
	receipt("This action has expired")
	receipt("This action has expired")
	raw, err := client.Call(w.ctx, "get_state", nil)
	var state struct {
		Replies int    `json:"fixtureUIReplies"`
		Value   string `json:"fixtureUIValue"`
	}
	if err != nil || json.Unmarshal(raw, &state) != nil || state.Replies != 2 || state.Value != "second-review" {
		t.Fatalf("native decisions while cleanup was blocked = %+v, error %v", state, err)
	}
	close(gate)
	wait(transport.clearFinished, "first keyboard cleanup did not finish")
	wait(transport.clearStarted, "second keyboard cleanup did not start")
	wait(transport.clearFinished, "second keyboard cleanup did not finish")
	transport.mu.Lock()
	concurrent := transport.concurrent
	transport.mu.Unlock()
	if concurrent {
		t.Fatal("failed-menu keyboard cleanup ran concurrently")
	}
}

func TestResumePickerCloseClearsKeyboard(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	setResumeFixtures(t, w.binding.Workspace, 1)
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	data := resumeButtons(t, f)[0]["callback_data"].(string)
	messageID := f.messageCount()
	command("/close")
	waitKeyboardClears(t, f, messageID)
	before := w.binding
	clickKeyboard(w, 7, data)
	if w.client != nil || !sameBindingIdentity(w.binding, before) || len(w.confirms) != 0 {
		t.Fatal("closed session picker remained actionable")
	}
	assertKeyboardClears(t, f, messageID)
}

func TestNativeUICancelClearsOnlyItsKeyboardWithoutEcho(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	w.owner = 7
	w.event([]byte(`{"type":"extension_ui_request","id":"native-select","method":"select","title":"Choose","options":["First","Second"]}`))
	data := resumeButtons(t, f)[0]["callback_data"].(string)
	messageID := f.messageCount()
	command("/model")
	modelData := resumeButtons(t, f)[0]["callback_data"].(string)
	modelToken, _, _ := strings.Cut(modelData, ":")
	w.event([]byte(`{"type":"extension_ui_request","method":"cancel","targetId":"native-select"}`))
	waitKeyboardClears(t, f, messageID)
	clickKeyboard(w, 7, data)
	w.event([]byte(`{"type":"extension_ui_request","method":"cancel","targetId":""}`))
	if _, exists := w.confirms[modelToken]; !exists {
		t.Fatal("native UI cancellation invalidated an unrelated model picker")
	}
	raw, err := w.call("get_state", nil)
	var state struct {
		Replies int `json:"fixtureUIReplies"`
	}
	if err != nil || json.Unmarshal(raw, &state) != nil || state.Replies != 0 {
		t.Fatalf("native cancellation was echoed back as a UI response: %+v, %v", state, err)
	}
	assertKeyboardClears(t, f, messageID)
}

func TestModelPickerInvalidationClearsKeyboard(t *testing.T) {
	for _, reason := range []string{"shutdown", "replacement", "completed", "cancelled"} {
		t.Run(reason, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			command("/model")
			data := resumeButtons(t, f)[0]["callback_data"].(string)
			messageID := f.messageCount()
			cleared := []int{messageID}
			switch reason {
			case "shutdown":
				w.teardownWorker(true)
			case "replacement":
				command("/new")
				confirmationID := f.messageCount()
				clickKeyboard(w, 7, resumeButtons(t, f)[0]["callback_data"].(string))
				assertFinishedMenu(t, f, confirmationID, "Confirmed")
			default:
				w.b.cfg.QueueCapacity = 1
				command("wait")
				w.dispatch()
				if reason == "completed" {
					w.preview = "Finished"
					w.finish()
				} else {
					w.finishCancelled("Cancelled")
				}
			}
			waitKeyboardClears(t, f, cleared...)
			before := w.binding
			clickKeyboard(w, 7, data)
			if !sameBindingIdentity(w.binding, before) || len(w.confirms) != 0 {
				t.Fatal("invalidated model picker remained actionable")
			}
			assertKeyboardClears(t, f, cleared...)
		})
	}
}

func TestNativeUISelectionDisplaysResult(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		name := "commit"
		if cancel {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			w.owner = 7
			options := make([]string, uiSelectPageSize+1)
			for i := range options {
				options[i] = fmt.Sprintf("%07x fix: commit %d", i+1, i)
			}
			w.ui(rpcEvent{ID: "review-result", Method: "select", Title: "Select commit to review", Options: options})
			messageID := int64(f.messageCount())
			click := func(data string) {
				w.callback(&telegram.CallbackQuery{ID: "review-result", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: data})
			}
			_, rows := pickerView(t, f)
			click(rows[uiSelectPageSize][0]["callback_data"].(string))
			_, rows = pickerView(t, f)
			data := rows[0][0]["callback_data"].(string)
			if cancel {
				data = rows[1][1]["callback_data"].(string)
			}
			click(data)
			text, rows := pickerView(t, f)
			if cancel {
				if text != "Cancel" {
					t.Fatalf("cancelled review still shows a selection prompt: %q", text)
				}
			} else if !strings.Contains(text, options[uiSelectPageSize]) || strings.Contains(text, "Select commit to review") || strings.Contains(text, "Page ") {
				t.Fatalf("completed review does not identify the selected commit: %q", text)
			}
			if len(rows) != 0 || len(w.confirms) != 0 {
				t.Fatal("completed review retained actionable choices")
			}
			f.mu.Lock()
			editedID := f.messages[len(f.messages)-1]["message_id"]
			f.mu.Unlock()
			if editedID != float64(messageID) {
				t.Fatal("review result did not replace the original menu")
			}
			click(data)
			after, _ := pickerView(t, f)
			if after != text {
				t.Fatal("a consumed callback changed the displayed review result")
			}
		})
	}
}

func TestNativeUISelectionEditFailurePreservesSession(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	w.owner = 7
	before, client := w.binding, w.client
	w.ui(rpcEvent{ID: "review-edit-failure", Method: "select", Title: "Select commit to review", Options: []string{"abc1234 fix: preview"}})
	_, rows := pickerView(t, f)
	data := rows[0][0]["callback_data"].(string)
	messageID := int64(f.messageCount())
	f.failProgress, f.failKeyboardClear = true, true
	f.keyboardClearGate = make(chan struct{})
	transport := &failedMenuHTTP{
		base: f, clearStarted: make(chan struct{}, 1), clearFinished: make(chan struct{}, 1), callbacks: make(chan string, 1),
	}
	http.DefaultTransport = transport
	result := w.callback(&telegram.CallbackQuery{ID: "review-edit-failure", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: data})
	if result != callbackDone || w.client != client || !sameBindingIdentity(w.binding, before) || len(w.confirms) != 0 {
		t.Fatal("best-effort menu edit failure interrupted or retained the native selection")
	}
	select {
	case <-transport.clearStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("failed menu did not queue background keyboard cleanup")
	}
	w.cancel()
	cleaned := make(chan struct{})
	go func() {
		w.background.Wait()
		close(cleaned)
	}()
	select {
	case <-cleaned:
	case <-time.After(3 * time.Second):
		t.Fatal("failed-menu cleanup did not cancel while its HTTP request was blocked")
	}
}

func TestNativeConfirmationDisplaysDecision(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancel), func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			before, client := w.binding, w.client
			w.owner = 7
			w.ui(rpcEvent{ID: "native-confirm", Method: "confirm", Title: "Proceed with the native operation?"})
			messageID := f.messageCount()
			buttons := resumeButtons(t, f)
			index, expected := 0, "Confirmed"
			if cancel {
				index, expected = 1, "Cancel"
			}
			data := buttons[index]["callback_data"].(string)
			clickKeyboard(w, 7, data)
			text := assertFinishedMenu(t, f, messageID, expected)
			if w.client != client || !sameBindingIdentity(w.binding, before) || len(w.confirms) != 0 {
				t.Fatal("native confirmation changed the session or retained an actionable token")
			}
			clickKeyboard(w, 7, data)
			if after := assertFinishedMenu(t, f, messageID, expected); after != text {
				t.Fatal("consumed native confirmation changed its displayed decision")
			}
		})
	}
}

func TestNativeUIExportRejectionTerminatesOriginalMenu(t *testing.T) {
	for _, method := range []string{"select", "confirm"} {
		for _, cancel := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%t", method, cancel), func(t *testing.T) {
				w, f, command := setupWorkspaceWorker(t)
				command("/new " + t.TempDir())
				w.owner = 7
				before, client, runtime := w.binding, w.client, w.runtime
				w.ui(rpcEvent{ID: "export-blocked-dialog", Method: method, Title: "Proceed with native work?", Options: []string{"abc1234 fix: preview"}})
				messageID := f.messageCount()
				buttons := resumeButtons(t, f)
				index := 0
				if cancel {
					index = 1
				}
				data := buttons[index]["callback_data"].(string)
				w.exportingSession = w.sessionID
				click := func() {
					w.callback(&telegram.CallbackQuery{ID: "export-blocked-dialog", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: int64(messageID)}, Data: data})
				}
				click()
				reason := "Wait for the current session export to finish."
				text := assertFinishedMenu(t, f, messageID, reason)
				if strings.Contains(text, "Selected") || strings.Contains(text, "Confirmed") || strings.Contains(text, "Cancel") {
					t.Fatalf("rejected native dialog falsely displays a submitted decision: %q", text)
				}
				if !strings.Contains(nativeOutputs(t, w), reason) {
					t.Fatal("native export rejection lost its existing separate notice")
				}
				if w.client != client || w.runtime != runtime || !sameBindingIdentity(w.binding, before) || w.exportingSession != before.SessionID || len(w.confirms) != 0 {
					t.Fatal("rejected native dialog changed runtime, binding, export, or retained choices")
				}
				assertUnchanged := func() {
					t.Helper()
					if after := assertFinishedMenu(t, f, messageID, reason); after != text {
						t.Fatal("replayed rejection changed the original menu")
					}
					if w.client != client || w.runtime != runtime || !sameBindingIdentity(w.binding, before) {
						t.Fatal("replayed rejection changed runtime or binding")
					}
				}
				assertUnchanged()
				click()
				assertUnchanged()
				// Finishing export must not make a rejected token actionable again.
				w.exportingSession = ""
				click()
				assertUnchanged()
				f.mu.Lock()
				receipt := f.callbacks[len(f.callbacks)-1]
				f.mu.Unlock()
				if receipt != "This action has expired" {
					t.Fatalf("replayed rejection receipt = %q", receipt)
				}
			})
		}
	}
}
