package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
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
				if !strings.Contains(text, before.Workspace) {
					t.Fatal("cancelled session replacement lost its workspace context")
				}
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

func TestNativeUIDialogSurvivesBridgeMenuExpiry(t *testing.T) {
	for _, method := range []string{"confirm", "select"} {
		t.Run(method, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			w.owner = 7
			before, client := w.binding, w.client
			options := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K"}
			w.ui(rpcEvent{ID: "waiting-native-dialog", Method: method, Title: "Permission required", Options: options})
			data := pickerButtons(t, f)[0]["callback_data"].(string)
			var next string
			if method == "select" {
				next = askTestButton(t, f, "Next")
			}
			token, _, _ := strings.Cut(data, ":")
			messageID := int64(f.messageCount())
			command("/new")
			bridgeData := resumeButtons(t, f)[0]["callback_data"].(string)
			bridgeToken, _, _ := strings.Cut(bridgeData, ":")
			now := time.Now().Add(3 * time.Minute)
			w.expire(now)
			if _, ok := w.confirms[token]; !ok {
				t.Fatal("bridge menu expiry removed a pending native dialog")
			}
			if _, ok := w.confirms[bridgeToken]; ok {
				t.Fatal("bridge-owned confirmation no longer expires")
			}
			readReplies := func() (int, string) {
				t.Helper()
				raw, err := w.call("get_state", nil)
				var state struct {
					Replies int    `json:"fixtureUIReplies"`
					Value   string `json:"fixtureUIValue"`
				}
				if err != nil || json.Unmarshal(raw, &state) != nil {
					t.Fatalf("native UI state unavailable: %v", err)
				}
				return state.Replies, state.Value
			}
			if replies, _ := readReplies(); replies != 0 {
				t.Fatal("bridge answered a native dialog without a user decision")
			}
			clickKeyboard(w, 7, bridgeData)
			if w.client != client || !sameBindingIdentity(w.binding, before) {
				t.Fatal("expired bridge confirmation replaced the session")
			}
			expectedValue := ""
			if method == "select" {
				w.callback(&telegram.CallbackQuery{ID: "next-native-page", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: next})
				data = askTestButton(t, f, options[10])
				token, _, _ = strings.Cut(data, ":")
				w.expire(now.Add(3 * time.Minute))
				if _, ok := w.confirms[token]; !ok {
					t.Fatal("pagination added a bridge deadline to the native dialog")
				}
				expectedValue = options[10]
			}
			w.callback(&telegram.CallbackQuery{ID: "delayed-native-choice", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: data})
			if replies, value := readReplies(); replies != 1 || value != expectedValue {
				t.Fatalf("delayed native choice: replies=%d value=%q", replies, value)
			}
		})
	}
}

func TestNativeUISelectionShowsSinglePageWithoutNavigation(t *testing.T) {
	for _, count := range []int{3, 10} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			w.owner = 7
			options := make([]string, count)
			for i := range options {
				options[i] = fmt.Sprintf("commit %02d", i)
			}
			raw, err := json.Marshal(map[string]any{"type": "extension_ui_request", "id": "review-commits", "method": "select", "title": "Choose commit", "options": options})
			if err != nil {
				t.Fatal(err)
			}
			w.event(raw)
			text, rows := pickerView(t, f)
			numericRows := (count + 4) / 5
			if strings.Contains(text, "Page ") || len(rows) != numericRows+1 {
				t.Fatalf("single-page commit choices = %q; %v", text, rows)
			}
			for row := range numericRows {
				if len(rows[row]) != min(5, count-row*5) {
					t.Fatalf("numeric row must contain up to five choices without controls: %v", rows)
				}
				for column, button := range rows[row] {
					index := row*5 + column
					if button["text"] != fmt.Sprint(index+1) || !strings.Contains(text, fmt.Sprintf("%d. %s", index+1, options[index])) {
						t.Fatalf("single-page choice lost its body label or numeric button: %q; %v", text, rows)
					}
				}
			}
			requirePickerFooter(t, rows, "Cancel")
			data := askTestButton(t, f, options[len(options)-1])
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
		})
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
	if !strings.Contains(text, "Page 1/3") || len(rows) != 3 || len(rows[0]) != 5 || len(rows[1]) != 5 {
		t.Fatalf("first commit page = %q; %v", text, rows)
	}
	requirePickerFooter(t, rows, "Next", "Cancel")
	first := askTestButton(t, f, options[0])
	next := askTestButton(t, f, "Next")
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
	if !strings.Contains(text, "Page 2/3") || len(rows) != 3 || len(rows[0]) != 5 || len(rows[1]) != 5 {
		t.Fatalf("middle commit page = %q; %v", text, rows)
	}
	requirePickerFooter(t, rows, "Previous", "Next", "Cancel")
	for i := 10; i < 20; i++ {
		if !strings.Contains(text, fmt.Sprintf("%d. %s", i+1, options[i])) {
			t.Fatalf("middle page lost a label: %q; %v", text, rows)
		}
		if data := askTestButton(t, f, options[i]); !strings.HasSuffix(data, fmt.Sprintf(":%d", i)) {
			t.Fatalf("middle page lost a global option index: %q", data)
		}
	}
	click(7, first)
	if _, exists := w.confirms[strings.SplitN(first, ":", 2)[0]]; exists {
		t.Fatal("old commit page retained an actionable token")
	}
	click(7, askTestButton(t, f, "Previous"))
	text, rows = pickerView(t, f)
	if !strings.Contains(text, "Page 1/3") {
		t.Fatalf("previous did not return to first commit page: %q", text)
	}
	click(7, askTestButton(t, f, "Next"))
	text, rows = pickerView(t, f)
	if !strings.Contains(text, "Page 2/3") {
		t.Fatalf("next did not reopen middle commit page: %q", text)
	}
	click(7, askTestButton(t, f, "Next"))
	text, rows = pickerView(t, f)
	if !strings.Contains(text, "Page 3/3") || len(rows) != 2 || len(rows[0]) != 2 || rows[0][0]["text"] != "21" || rows[0][1]["text"] != "22" {
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
	click(7, askTestButton(t, f, options[20]))
	result, err = w.call("get_state", nil)
	if err != nil || json.Unmarshal(result, &state) != nil || state.Replies != 1 || state.Value != options[20] {
		t.Fatalf("last-page choice = %+v, error %v", state, err)
	}
	raw, err = json.Marshal(map[string]any{"type": "extension_ui_request", "id": "review-cancel", "method": "select", "title": "Choose commit", "options": options[:11]})
	if err != nil {
		t.Fatal(err)
	}
	w.event(raw)
	messageID = int64(f.messageCount())
	click(7, askTestButton(t, f, "Next"))
	text, rows = pickerView(t, f)
	if !strings.Contains(text, "Page 2/2") || len(rows) != 2 {
		t.Fatalf("cancel page = %q; %v", text, rows)
	}
	requirePickerFooter(t, rows, "Previous", "Cancel")
	click(7, askTestButton(t, f, "Cancel"))
	result, err = w.call("get_state", nil)
	if err != nil || json.Unmarshal(result, &state) != nil || state.Replies != 2 || state.Value != "" {
		t.Fatalf("cancel response = %+v, error %v", state, err)
	}
}

func TestNativeUISelectionPaginatesByContentAndKeepsRawValues(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	w.owner = 7
	options := make([]string, 4)
	details := make([]askOption, len(options))
	for i := range options {
		options[i] = fmt.Sprintf("Choice %d\n  %s\nlabel tail", i+1, strings.Repeat("label ", 20))
		details[i] = askOption{
			Description: fmt.Sprintf("Description %d\n\n  %s\ndescription tail", i+1, strings.Repeat("description ", 30)),
			Preview:     "Preview\n  " + strings.Repeat("preview ", 10) + "\npreview tail",
		}
	}
	w.ui(rpcEvent{ID: "content-paged-review", Method: "select", Title: "Choose a review action", Options: options, OptionDetails: details})
	messageID := int64(f.messageCount())
	click := func(data string) {
		w.callback(&telegram.CallbackQuery{ID: "content-page", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: data})
	}
	assertPage := func(first int, indicator string) [][]map[string]any {
		t.Helper()
		text, rows := pickerView(t, f)
		if !strings.Contains(text, "Choose a review action") || !strings.Contains(text, indicator) || len(rows) != 2 || len(rows[0]) != 2 {
			t.Fatalf("content-sized review page = %q; %v", text, rows)
		}
		for offset := range 2 {
			index := first + offset
			if rows[0][offset]["text"] != fmt.Sprint(index+1) {
				t.Fatalf("content pagination lost global number: %v", rows)
			}
			for _, want := range []string{options[index], details[index].Description, details[index].Preview} {
				if !strings.Contains(html.UnescapeString(text), want) {
					t.Fatalf("content page clipped %q: %q", want, text)
				}
			}
		}
		return rows
	}
	rows := assertPage(0, "Page 1/2")
	requirePickerFooter(t, rows, "Next", "Cancel")
	click(askTestButton(t, f, "Next"))
	rows = assertPage(2, "Page 2/2")
	requirePickerFooter(t, rows, "Previous", "Cancel")
	click(askTestButton(t, f, "Previous"))
	rows = assertPage(0, "Page 1/2")
	click(askTestButton(t, f, "Next"))
	rows = assertPage(2, "Page 2/2")
	result, err := w.call("get_state", nil)
	var state struct {
		Replies int    `json:"fixtureUIReplies"`
		Value   string `json:"fixtureUIValue"`
	}
	if err != nil || json.Unmarshal(result, &state) != nil || state.Replies != 0 {
		t.Fatalf("content page navigation submitted an answer: %+v, %v", state, err)
	}
	click(askTestButton(t, f, "Choice 3"))
	result, err = w.call("get_state", nil)
	if err != nil || json.Unmarshal(result, &state) != nil || state.Replies != 1 || state.Value != options[2] {
		t.Fatalf("content page selection changed the original global value: %+v, %v", state, err)
	}
}

func TestCompactCompletionUsesOneMessageAndFreshUsage(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		missing     bool
		timeout     bool
		newSession  bool
		editFailure bool
		sendFailure bool
	}{
		{name: "post-compaction usage"},
		{name: "usage unavailable", missing: true},
		{name: "usage query timeout", timeout: true},
		{name: "different native session", newSession: true},
		{name: "menu edit failure", editFailure: true},
		{name: "status send failure", sendFailure: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			before, client := w.binding, w.client
			w.operations = make(chan operationResult, 1)
			var outboxBefore int
			if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&outboxBefore); err != nil {
				t.Fatal(err)
			}
			w.progress.NativeStatus = "Context: 90% (9k / 10k)"
			if !scenario.missing {
				nativeRPC(t, w, "fixture_get_state_update", map[string]any{
					"contextUsage": map[string]any{"tokens": 2500, "contextWindow": 10000},
					"newSession":   scenario.newSession,
				})
			}
			if scenario.timeout {
				nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "get_state"})
			}
			messagesBefore := f.messageCount()
			f.mu.Lock()
			f.failProgress = scenario.sendFailure
			f.mu.Unlock()
			command("/compact")
			messageID := f.messageCount()
			if w.sessionOp != sessionOperationCompact || w.taskActive() || !sameBindingIdentity(w.binding, before) || len(w.confirms) != 0 || f.button(messagesBefore) != "" {
				t.Fatal("compact did not start directly without confirmation in the current session")
			}
			result := waitOperation(t, w)
			if result.err != nil {
				t.Fatalf("compaction failed: %v", result.err)
			}
			if scenario.editFailure {
				http.DefaultTransport = &failedMenuHTTP{
					base: f, clearStarted: make(chan struct{}, 4), clearFinished: make(chan struct{}, 4),
				}
			}
			w.operationReturned(result)
			if w.sessionOperationActive() || !sameBindingIdentity(w.binding, before) {
				t.Fatal("completed compaction left the session busy or changed its binding")
			}
			if scenario.newSession {
				if w.client != nil || w.runtime != runtimeReleased || len(w.b.slots) != 0 {
					t.Fatal("native session mismatch retained the wrong runtime")
				}
				select {
				case <-client.Done():
				default:
					t.Fatal("native session mismatch did not close the old process")
				}
			} else if w.client != client {
				t.Fatal("completed compaction unnecessarily retired its runtime")
			}
			var outboxAfter int
			if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&outboxAfter); err != nil {
				t.Fatal(err)
			}
			var text string
			if scenario.editFailure || scenario.sendFailure {
				if outboxAfter != outboxBefore+1 {
					t.Fatal("failed status send or edit lost the compaction result")
				}
				if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil {
					t.Fatal(err)
				}
			} else {
				if outboxAfter != outboxBefore {
					t.Fatal("successful compaction queued a duplicate result message")
				}
				text = assertFinishedMenu(t, f, messageID, "Context:")
			}
			wantUsage := "25% (2.5k / 10k)"
			if scenario.missing || scenario.timeout || scenario.newSession {
				wantUsage = "n/a"
			}
			if !strings.Contains(text, "Compaction completed.") || !strings.Contains(text, "Context: "+wantUsage) {
				t.Fatalf("compaction result did not show confirmed usage %q: %s", wantUsage, text)
			}
			if strings.Contains(text, "PRIVATE") || strings.Contains(text, "SECRET") || strings.Contains(text, "90%") {
				t.Fatalf("compaction result exposed raw state or stale usage: %s", text)
			}
			if scenario.newSession {
				command("/name restored original session")
				if w.client == nil || w.client == client || !sameBindingIdentity(w.binding, before) {
					t.Fatal("next request did not resume the preserved session")
				}
				data, err := w.client.Call(w.ctx, "get_state", nil)
				if err != nil {
					t.Fatal(err)
				}
				var state statusState
				if json.Unmarshal(data, &state) != nil || state.SessionID != before.SessionID {
					t.Fatal("next request reached a different native session")
				}
			}
		})
	}
}

func TestResumeKeyboardNavigationAndSelection(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	before := w.binding
	sessions := setResumeFixtures(t, before.Workspace, 12)
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	first := resumeButtons(t, f)
	messageID := f.messageCount()
	clickResume(w, 7, askTestButton(t, f, "Next"))
	second := resumeButtons(t, f)
	assertKeyboardClears(t, f)
	// A callback from the replaced page cannot clear the new page or select.
	clickResume(w, 7, first[0]["callback_data"].(string))
	assertKeyboardClears(t, f)
	if !sameBindingIdentity(w.binding, before) {
		t.Fatal("stale first-page selection changed the session")
	}
	clickResume(w, 7, askTestButton(t, f, "Previous"))
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
	w.ui(rpcEvent{ID: "first-review", Method: "select", Title: "Choose commit", Options: []string{"first-review"}})
	_, rows := pickerView(t, f)
	choices = append(choices, rows[0][0]["callback_data"].(string))
	messageIDs = append(messageIDs, int64(f.messageCount()))
	secondShown := make(chan struct{})
	f.sendResponses = map[int64][]fakeHTTPResponse{-10: {{status: 200, body: `{"ok":true,"result":{"message_id":500}}`, onClose: func() { close(secondShown) }}}}
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
	_, err := client.Call(w.ctx, "fixture_research_control_event", map[string]any{"event": map[string]any{
		"type": "extension_ui_request", "id": "second-review", "method": "select", "title": "Choose commit", "options": []string{"second-review"},
	}})
	requireStoreOK(t, err)
	wait(secondShown, "blocked keyboard cleanup prevented the next native menu")
	_, rows = pickerView(t, f)
	choices = append(choices, rows[0][0]["callback_data"].(string))
	messageIDs = append(messageIDs, 500)
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
	for _, method := range []string{"confirm", "select"} {
		t.Run(method, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			w.owner = 7
			w.ui(rpcEvent{ID: "native-dialog", Method: method, Title: "Choose", Options: []string{"First", "Second"}})
			data := resumeButtons(t, f)[0]["callback_data"].(string)
			messageID := f.messageCount()
			w.expire(time.Now().Add(3 * time.Minute))
			command("/model")
			modelData := resumeButtons(t, f)[0]["callback_data"].(string)
			modelToken, _, _ := strings.Cut(modelData, ":")
			w.event([]byte(`{"type":"extension_ui_request","method":"cancel","targetId":"native-dialog"}`))
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
		})
	}
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
					w.stream.WriteString("Finished")
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
			click(askTestButton(t, f, "Next"))
			data := askTestButton(t, f, options[uiSelectPageSize])
			if cancel {
				data = askTestButton(t, f, "Cancel")
			}
			click(data)
			text, rows := pickerView(t, f)
			if cancel {
				if !strings.Contains(text, "review") || !strings.Contains(text, options[uiSelectPageSize]) || strings.Contains(text, "Your choice") {
					t.Fatalf("cancelled review lost its operation context: %q", text)
				}
			} else if !strings.Contains(text, "Select commit to review") || !strings.Contains(text, "Options") || !strings.Contains(text, options[uiSelectPageSize]) || !strings.Contains(text, "Your choice") || strings.LastIndex(text, options[uiSelectPageSize]) < strings.Index(text, "Your choice") {
				t.Fatalf("completed review lost its question, options, or chosen answer: %q", text)
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

func TestNativeUICompletedSelectionRetainsDisplayableOptions(t *testing.T) {
	for _, count := range []int{25, 100, 400} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			w.owner = 7
			options := make([]string, count)
			for index := range options {
				options[index] = fmt.Sprintf("opt-%03d", index)
			}
			w.ui(rpcEvent{ID: "all-options", Method: "select", Title: "Review all options", Options: options})
			messageID := int64(f.messageCount())
			click := func(data string) {
				w.callback(&telegram.CallbackQuery{ID: "all-options", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: data})
			}
			selected := count - 4
			for page := 0; page < selected/uiSelectPageSize; page++ {
				click(askTestButton(t, f, "Next"))
			}
			click(askTestButton(t, f, options[selected]))
			text := assertFinishedMenu(t, f, int(messageID), "Your choice")
			body, answer, _ := strings.Cut(text, "<b>Your choice</b>")
			if !strings.Contains(answer, askOptionLabel(selected, options[selected])) {
				t.Fatalf("completed menu lost its selected value: %q", text)
			}
			if count == 400 {
				if !strings.Contains(body, "...") {
					t.Fatal("oversized options did not indicate display truncation")
				}
				return
			}
			for index, option := range options {
				if !strings.Contains(body, askOptionLabel(index, option)) {
					t.Fatalf("completed menu omitted displayable option %d: %q", index+1, body)
				}
			}
		})
	}
}

func TestNativeUISequentialSelectionsShareReceipt(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancel), func(t *testing.T) {
			w, f, _, _ := replyStatusWorker(t)
			first := "3. Review a specific commit"
			w.ui(rpcEvent{ID: "review-mode", Method: "select", Title: "Choose review mode", Options: []string{first}})
			messageID := int64(f.messageCount())
			click := func(data string) {
				w.callback(&telegram.CallbackQuery{ID: "review-choice", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: data})
			}
			click(askTestButton(t, f, first))
			options := make([]string, uiSelectPageSize+1)
			for i := range options {
				options[i] = fmt.Sprintf("%07x fix: commit %d", i+1, i)
			}
			w.ui(rpcEvent{ID: "review-commit", Method: "select", Title: "Choose commit", Options: options})
			f.mu.Lock()
			reusedID := f.messages[len(f.messages)-1]["message_id"]
			f.mu.Unlock()
			if reusedID != float64(messageID) {
				t.Fatal("second native selection created another bubble")
			}
			stale := askTestButton(t, f, options[0])
			click(askTestButton(t, f, "Next"))
			choice := askTestButton(t, f, options[uiSelectPageSize])
			want := options[uiSelectPageSize]
			if cancel {
				choice = askTestButton(t, f, "Cancel")
				want = "Cancel"
			}
			click(choice)
			text, rows := pickerView(t, f)
			if !strings.Contains(text, first) || !strings.Contains(text, want) || len(rows) != 0 || len(w.confirms) != 0 {
				t.Fatalf("combined native receipt lost a choice or retained buttons: %q", text)
			}
			click(stale)
			click(choice)
			raw, err := w.call("get_state", nil)
			var state struct {
				Replies int `json:"fixtureUIReplies"`
			}
			if err != nil || json.Unmarshal(raw, &state) != nil || state.Replies != 2 {
				t.Fatalf("navigation/stale buttons submitted extra native choices: %s, %v", raw, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, request := range f.messages[int(messageID):] {
				if request["message_id"] != float64(messageID) {
					t.Fatalf("multi-step choice sent another receipt: %+v", request)
				}
			}
		})
	}
}

func TestNativeUISequentialSelectionInvalidationPreservesReceipt(t *testing.T) {
	for _, invalidation := range []string{"native-cancel", "task-cleanup"} {
		for _, paginate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/paginate=%t", invalidation, paginate), func(t *testing.T) {
				w, f, _, _ := replyStatusWorker(t)
				first := "Review a specific commit"
				w.ui(rpcEvent{ID: "review-mode", Method: "select", Title: "Choose review mode", Options: []string{first}})
				messageID := int64(f.messageCount())
				click := func(data string) {
					w.callback(&telegram.CallbackQuery{ID: "review-choice", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: data})
				}
				firstChoice := askTestButton(t, f, first)
				click(firstChoice)
				assertFinishedMenu(t, f, int(messageID), "Your choice")
				options := make([]string, uiSelectPageSize+1)
				for i := range options {
					options[i] = fmt.Sprintf("commit %02d", i)
				}
				raw, err := json.Marshal(map[string]any{"type": "extension_ui_request", "id": "review-commit", "method": "select", "title": "Choose commit", "options": options})
				if err != nil {
					t.Fatal(err)
				}
				w.event(raw)
				text, _ := pickerView(t, f)
				if !strings.Contains(text, first) || !strings.Contains(text, "Choose review mode") || !strings.Contains(text, "Choose commit") {
					t.Errorf("pending selection hid the completed choice: %q", text)
				}
				stale := askTestButton(t, f, options[0])
				pending := stale
				if paginate {
					click(askTestButton(t, f, "Next"))
					text, _ = pickerView(t, f)
					if !strings.Contains(text, first) || !strings.Contains(text, "Choose review mode") || !strings.Contains(text, "Page 2/2") {
						t.Errorf("pagination hid the completed choice: %q", text)
					}
					if _, exists := w.confirms[strings.SplitN(stale, ":", 2)[0]]; exists {
						t.Fatal("pagination retained the old selection token")
					}
					pending = askTestButton(t, f, options[uiSelectPageSize])
				}
				if invalidation == "native-cancel" {
					w.event([]byte(`{"type":"extension_ui_request","id":"cancel-review-commit","method":"cancel","targetId":"review-commit"}`))
				} else {
					w.clearTaskConfirmations()
				}
				if len(w.confirms) != 0 {
					t.Fatal("invalidated native selection retained actionable tokens")
				}
				waitKeyboardClears(t, f, int(messageID))
				text, _ = pickerView(t, f)
				if !strings.Contains(text, first) || !strings.Contains(text, "Choose review mode") {
					t.Errorf("invalidating the second selection erased the completed choice: %q", text)
				}
				click(firstChoice)
				click(stale)
				click(pending)
				if after, _ := pickerView(t, f); after != text {
					t.Fatal("invalidated callbacks changed the visible receipt")
				}
				raw, err = w.call("get_state", nil)
				var state struct {
					Replies int    `json:"fixtureUIReplies"`
					Value   string `json:"fixtureUIValue"`
				}
				if err != nil || json.Unmarshal(raw, &state) != nil || state.Replies != 1 || state.Value != first {
					t.Fatalf("invalidation/navigation submitted an unchosen native answer: %s, %v", raw, err)
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				markup, err := json.Marshal(f.keyboardClears[0]["reply_markup"])
				var keyboard telegram.Keyboard
				if err != nil || json.Unmarshal(markup, &keyboard) != nil || len(keyboard.InlineKeyboard) != 0 {
					t.Fatalf("invalidated selection retained its keyboard: %s, %v", markup, err)
				}
				for _, request := range f.messages[int(messageID):] {
					if request["message_id"] != float64(messageID) {
						t.Fatalf("second selection created a duplicate menu or receipt: %+v", request)
					}
				}
			})
		}
	}
}

type nativeSelectEditHTTP struct {
	base     *fakeHTTP
	response fakeHTTPResponse
	err      error
}

func (f *nativeSelectEditHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/editMessageText") {
		if f.err != nil {
			return nil, f.err
		}
		return &http.Response{
			StatusCode: f.response.status,
			Body:       io.NopCloser(strings.NewReader(f.response.body)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	}
	return f.base.RoundTrip(r)
}

func TestNativeUISelectionMissingReceiptCreatesUsableMenu(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	first := "Review a specific commit"
	w.ui(rpcEvent{ID: "review-mode", Method: "select", Title: "Choose review mode", Options: []string{first}})
	oldMessageID := int64(f.messageCount())
	oldChoice := askTestButton(t, f, first)
	click := func(user, messageID int64, data string) {
		w.callback(&telegram.CallbackQuery{ID: "review-choice", From: telegram.User{ID: user}, Message: &telegram.Message{MessageID: messageID}, Data: data})
	}
	click(7, oldMessageID, oldChoice)
	http.DefaultTransport = &nativeSelectEditHTTP{base: f, response: fakeHTTPResponse{
		status: http.StatusBadRequest,
		body:   `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`,
	}}
	options := make([]string, uiSelectPageSize+1)
	for i := range options {
		options[i] = fmt.Sprintf("commit %02d", i)
	}
	w.ui(rpcEvent{ID: "review-commit", Method: "select", Title: "Choose commit", Options: options})
	http.DefaultTransport = f
	messageID := int64(f.messageCount())
	text, rows := pickerView(t, f)
	if messageID <= oldMessageID || !strings.Contains(text, "Choose commit") || len(rows) != 3 || len(w.confirms) != 1 {
		t.Fatalf("missing receipt did not produce a usable new menu: %q; %v", text, rows)
	}
	f.mu.Lock()
	editedID := f.messages[len(f.messages)-1]["message_id"]
	f.mu.Unlock()
	if editedID != nil {
		t.Fatal("missing receipt fallback did not send a new message")
	}
	assertReplies := func(want int, value string) {
		t.Helper()
		raw, err := w.call("get_state", nil)
		var state struct {
			Replies int    `json:"fixtureUIReplies"`
			Value   string `json:"fixtureUIValue"`
		}
		if err != nil || json.Unmarshal(raw, &state) != nil || state.Replies != want || state.Value != value {
			t.Fatalf("native choices = %+v, want %d replies and %q, error %v", state, want, value, err)
		}
	}
	assertReplies(1, first)
	next := askTestButton(t, f, "Next")
	click(8, messageID, next)
	click(7, oldMessageID, next)
	click(7, oldMessageID, oldChoice)
	assertReplies(1, first)
	click(7, messageID, next)
	choice := askTestButton(t, f, options[uiSelectPageSize])
	click(7, messageID, choice)
	text = assertFinishedMenu(t, f, int(messageID), options[uiSelectPageSize])
	if !strings.Contains(text, first) {
		t.Fatalf("replacement menu lost its earlier choice: %q", text)
	}
	click(7, messageID, next)
	click(7, messageID, choice)
	assertReplies(2, options[uiSelectPageSize])
}

func TestNativeUISelectionEditFailureDoesNotResendMenu(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		pagination bool
		response   fakeHTTPResponse
		err        error
	}{
		{name: "reuse-other-400", response: fakeHTTPResponse{status: 400, body: `{"ok":false,"error_code":400,"description":"Bad Request: message cannot be edited"}`}},
		{name: "reuse-server-error", response: fakeHTTPResponse{status: 500, body: `{"ok":false,"error_code":500,"description":"message to edit not found"}`}},
		{name: "reuse-transport", err: errors.New("message to edit not found")},
		{name: "reuse-timeout", err: context.DeadlineExceeded},
		{name: "reuse-uncertain-400", response: fakeHTTPResponse{status: 400, body: `{"error_code":400,"description":"message to edit not found"}`}},
		{name: "pagination-missing-message", pagination: true, response: fakeHTTPResponse{status: 400, body: `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, f, _, _ := replyStatusWorker(t)
			options := make([]string, uiSelectPageSize+1)
			for i := range options {
				options[i] = fmt.Sprintf("commit %02d", i)
			}
			w.ui(rpcEvent{ID: "first-menu", Method: "select", Title: "First menu", Options: options})
			messageID := int64(f.messageCount())
			next := askTestButton(t, f, "Next")
			if !scenario.pagination {
				w.callback(&telegram.CallbackQuery{ID: "first-choice", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: askTestButton(t, f, options[0])})
			}
			before := f.messageCount()
			http.DefaultTransport = &nativeSelectEditHTTP{base: f, response: scenario.response, err: scenario.err}
			if scenario.pagination {
				w.callback(&telegram.CallbackQuery{ID: "next-page", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: next})
			} else {
				w.ui(rpcEvent{ID: "second-menu", Method: "select", Title: "Second menu", Options: options})
			}
			http.DefaultTransport = f
			f.mu.Lock()
			for _, message := range f.messages[before:] {
				if message["reply_markup"] != nil {
					f.mu.Unlock()
					t.Fatalf("failed edit blindly resent a selection menu: %+v", message)
				}
			}
			f.mu.Unlock()
			if len(w.confirms) != 0 {
				t.Fatal("failed edit retained an actionable selection menu")
			}
			raw, err := w.call("get_state", nil)
			var state struct {
				Replies int    `json:"fixtureUIReplies"`
				Value   string `json:"fixtureUIValue"`
			}
			want := 2
			if scenario.pagination {
				want = 1
			}
			if err != nil || json.Unmarshal(raw, &state) != nil || state.Replies != want || state.Value != "" {
				t.Fatalf("failed menu cancellation = %+v, error %v", state, err)
			}
		})
	}
}

func TestNativeUIIdleSelectionCannotBecomeNewRootReceipt(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "off"
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	w.owner = 7
	w.ui(rpcEvent{ID: "idle-choice", Method: "select", Title: "Choose while idle", Options: []string{"Idle choice"}})
	messageID := int64(f.messageCount())
	_, rows := pickerView(t, f)
	choice := rows[0][0]["callback_data"].(string)
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	if !w.taskActive() || w.turn == 0 {
		t.Fatal("new root did not start")
	}
	w.callback(&telegram.CallbackQuery{ID: "idle-choice", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: choice})
	text := assertFinishedMenu(t, f, int(messageID), "Idle choice")
	raw, err := w.call("get_state", nil)
	var state struct {
		Replies int    `json:"fixtureUIReplies"`
		Value   string `json:"fixtureUIValue"`
	}
	if err != nil || json.Unmarshal(raw, &state) != nil || state.Replies != 1 || state.Value != "Idle choice" {
		t.Fatalf("old idle menu did not answer OMP normally: %+v, error %v", state, err)
	}
	w.ui(rpcEvent{ID: "new-root-choice", Method: "select", Title: "Choose for new root", Options: []string{"New root choice"}})
	f.mu.Lock()
	newMessage := f.messages[len(f.messages)-1]
	f.mu.Unlock()
	if newMessage["message_id"] != nil {
		t.Fatal("new root selection overwrote the old idle receipt")
	}
	if after := assertFinishedMenu(t, f, int(messageID), "Idle choice"); after != text {
		t.Fatal("new root menu changed the old idle receipt")
	}
}

func TestNativeUIPendingSelectionKeepsOriginalScope(t *testing.T) {
	for _, boundary := range []string{"active", "turn", "zero-turn"} {
		t.Run(boundary, func(t *testing.T) {
			w, f, _, _ := replyStatusWorker(t)
			turn := w.turn
			if boundary == "zero-turn" {
				w.turn = 0
			}
			w.ui(rpcEvent{ID: "old-scope-choice", Method: "select", Title: "Choose in original scope", Options: []string{"Original choice"}})
			messageID := int64(f.messageCount())
			_, rows := pickerView(t, f)
			switch boundary {
			case "active":
				w.active++
			case "turn":
				w.turn++
			case "zero-turn":
				w.turn = turn
			}
			w.callback(&telegram.CallbackQuery{ID: "old-scope-choice", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: rows[0][0]["callback_data"].(string)})
			assertFinishedMenu(t, f, int(messageID), "Original choice")
			w.ui(rpcEvent{ID: "current-scope-choice", Method: "select", Title: "Choose in current scope", Options: []string{"Current choice"}})
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.messages[len(f.messages)-1]["message_id"] != nil {
				t.Fatal("current selection overwrote a receipt borrowed from another scope")
			}
		})
	}
}

func TestNativeUISelectionReceiptDoesNotCrossScope(t *testing.T) {
	for _, boundary := range []string{"turn", "generation", "agent"} {
		t.Run(boundary, func(t *testing.T) {
			w, f, _, _ := replyStatusWorker(t)
			w.ui(rpcEvent{ID: "first-choice", Method: "select", Title: "Choose first", Options: []string{"First"}})
			messageID := int64(f.messageCount())
			_, rows := pickerView(t, f)
			w.callback(&telegram.CallbackQuery{ID: "first-choice", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: rows[0][0]["callback_data"].(string)})
			switch boundary {
			case "turn":
				w.turn++
			case "generation":
				w.binding.Generation++
			case "agent":
				w.event([]byte(`{"type":"agent_start"}`))
			}
			w.ui(rpcEvent{ID: "unrelated-choice", Method: "select", Title: "Choose unrelated", Options: []string{"Second"}})
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.messages[len(f.messages)-1]["message_id"] != nil {
				t.Fatal("unrelated selection rewrote an earlier receipt")
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
	f.mu.Lock()
	f.failProgress, f.failKeyboardClear = true, true
	f.keyboardClearGate = make(chan struct{})
	f.mu.Unlock()
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
			if cancel && !strings.Contains(text, "native operation") {
				t.Fatal("cancelled native confirmation lost its operation context")
			}
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

func TestNativeUIRepliesCannotReachReplacementRuntime(t *testing.T) {
	for _, method := range []string{"confirm", "select"} {
		t.Run(method, func(t *testing.T) {
			w, _, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			w.owner = 7
			w.ui(rpcEvent{ID: "old-native-dialog", Method: method, Title: "Choose", Options: []string{"Old choice"}})
			var token string
			var old confirmation
			for candidate, c := range w.confirms {
				if c.action == "ui" {
					token, old = candidate, c
					break
				}
			}
			if token == "" {
				t.Fatal("native dialog was not created")
			}
			before := w.binding
			w.releaseRuntimeWithReason(true, "failure")
			command("/name replacement runtime")
			client := w.client
			if client == nil || client.ID() == old.uiClientID || !sameBindingIdentity(w.binding, before) {
				t.Fatal("runtime replacement did not preserve the session")
			}
			w.cancelUI(old)
			w.callback(&telegram.CallbackQuery{ID: "old-click", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: old.messageID, Chat: telegram.Chat{ID: w.key.chat}, MessageThreadID: w.key.thread}, Data: token + ":0"})
			raw, err := w.call("get_state", nil)
			var state struct {
				Replies int `json:"fixtureUIReplies"`
			}
			if err != nil || json.Unmarshal(raw, &state) != nil || state.Replies != 0 || w.client != client || !sameBindingIdentity(w.binding, before) {
				t.Fatalf("old native UI reached or replaced the new runtime: replies=%d error=%v", state.Replies, err)
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
				var outboxBefore int
				requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&outboxBefore))
				click := func() {
					w.callback(&telegram.CallbackQuery{ID: "export-blocked-dialog", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: int64(messageID)}, Data: data})
				}
				click()
				reason := "Wait for the current session export to finish."
				text := assertFinishedMenu(t, f, messageID, reason)
				if strings.Contains(text, "Selected") || strings.Contains(text, "Confirmed") || strings.Contains(text, "Cancel") {
					t.Fatalf("rejected native dialog falsely displays a submitted decision: %q", text)
				}
				var outboxAfter int
				requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&outboxAfter))
				if outboxAfter != outboxBefore {
					t.Fatal("native export rejection queued a duplicate menu result")
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

func TestNativeUISelectionDetailsAndRawValue(t *testing.T) {
	value := "[x] " + strings.Repeat("完整选项", 45) + "\n\n  label continuation"
	description := "Option explanation\n\n  " + strings.Repeat("preserved description ", 20) + "\nlast description line"
	preview := "Preview\n  code: `literal`\n\nlast preview line"
	for _, scenario := range []struct {
		name    string
		details []askOption
		shown   bool
	}{
		{name: "missing"},
		{name: "aligned", details: []askOption{{Description: description, Preview: preview}, {Description: "Done explanation"}}, shown: true},
		{name: "partial", details: []askOption{{Description: description, Preview: preview}}, shown: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			w.owner = 7
			raw, err := json.Marshal(map[string]any{
				"type": "extension_ui_request", "id": "native-details", "method": "select",
				"title": "Choose an action", "options": []string{value, "Done"}, "optionDetails": scenario.details,
			})
			if err != nil {
				t.Fatal(err)
			}
			w.event(raw)
			text, rows := pickerView(t, f)
			if !strings.Contains(text, "Choose an action") || !strings.Contains(html.UnescapeString(text), "1. "+value) || len(rows[0]) != 2 || rows[0][0]["text"] != "1" || rows[0][1]["text"] != "2" {
				t.Fatalf("pending menu lost a complete label or numeric button: %q; %v", text, rows)
			}
			for _, detail := range []string{description, preview} {
				if strings.Contains(html.UnescapeString(text), detail) != scenario.shown {
					t.Fatalf("native detail was clipped or associated with another value: %q", text)
				}
			}
			messageID := int64(f.messageCount())
			w.callback(&telegram.CallbackQuery{ID: "native-details", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: pickerButtons(t, f)[0]["callback_data"].(string)})
			text = assertFinishedMenu(t, f, int(messageID), "Your choice")
			body, answer, ok := strings.Cut(text, "Your choice")
			if !ok || !strings.Contains(answer, "1. "+value) || strings.Contains(answer, "Done") || !strings.Contains(text, "Choose an action") || !strings.Contains(text, "Done") {
				t.Fatalf("native receipt inferred a checkbox set or lost the actual choice: %q", text)
			}
			if scenario.shown && (!strings.Contains(html.UnescapeString(body), value) || !strings.Contains(html.UnescapeString(body), description) || !strings.Contains(html.UnescapeString(body), preview)) {
				t.Fatalf("native completion clipped options even though the full receipt fits: %q", text)
			}
			result, err := w.call("get_state", nil)
			var state struct {
				Replies int    `json:"fixtureUIReplies"`
				Value   string `json:"fixtureUIValue"`
			}
			if err != nil || json.Unmarshal(result, &state) != nil || state.Replies != 1 || state.Value != value {
				t.Fatalf("numeric native selection changed the submitted value: %+v, %v", state, err)
			}
		})
	}
}

func TestNativeUISelectionKeepsHistoryAndFooterWithinBudget(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	w.ui(rpcEvent{ID: "first-choice", Method: "select", Title: "Prior question", Options: []string{"Previous choice"}})
	messageID := int64(f.messageCount())
	click := func(data string) {
		w.callback(&telegram.CallbackQuery{ID: "budget-choice", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: data})
	}
	_, rows := pickerView(t, f)
	click(rows[0][0]["callback_data"].(string))
	options := make([]string, uiSelectPageSize)
	details := make([]askOption, len(options))
	options[0] = "Actual selected value"
	for i := range options {
		if i != 0 {
			options[i] = fmt.Sprintf("Option %d %s", i+1, strings.Repeat("long label ", 200))
		}
		details[i] = askOption{Description: strings.Repeat("```literal```\n", 100)}
	}
	w.ui(rpcEvent{ID: "next-choice", Method: "select", Title: "Current question " + strings.Repeat("context ", 1000), Options: options, OptionDetails: details})
	pending, rows := pickerView(t, f)
	for _, want := range []string{"Prior question", "Previous choice", "Current question", "Page 1/"} {
		if !strings.Contains(pending, want) {
			t.Fatalf("pending selection lost its question or chosen history %q: %q", want, pending)
		}
	}
	click(rows[0][0]["callback_data"].(string))
	text := assertFinishedMenu(t, f, int(messageID), "Your choice")
	_, answer, ok := strings.Cut(text, "Your choice")
	if !ok || !strings.Contains(answer, options[0]) || strings.Contains(answer, "Done") || !strings.Contains(text, "Prior question") || !strings.Contains(text, "Previous choice") || !strings.Contains(text, "Current question") || !strings.Contains(text, "Option 10") || !strings.Contains(text, "\n...") {
		t.Fatalf("large native receipt dropped history, options, answer, or its omission marker: %q", text)
	}
	if strings.Count(text, "<pre>") != strings.Count(text, "</pre>") || strings.Count(text, "<code>") != strings.Count(text, "</code>") {
		t.Fatalf("large native receipt cut a literal span: %q", text)
	}
}
