package bridge

import (
	"encoding/json"
	"strings"
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

// Omit the callback message to exercise the identity saved when the menu was sent.
func clickKeyboard(w *worker, user int64, data string) {
	w.callback(&telegram.CallbackQuery{ID: "keyboard-callback", From: telegram.User{ID: user}, Data: data})
	for w.controlBusy {
		select {
		case result := <-w.operations:
			w.operationReturned(result)
		case <-time.After(5 * time.Second):
			panic("control operation did not complete")
		}
	}
}

func TestNewConfirmationClearsKeyboardOnce(t *testing.T) {
	for _, scenario := range []string{"approve", "cancel", "clear-failure"} {
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
			f.failKeyboardClear = scenario == "clear-failure"
			clickKeyboard(w, 7, buttons[index]["callback_data"].(string))
			assertKeyboardClears(t, f, messageID)
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
			assertKeyboardClears(t, f, messageID)
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
	assertKeyboardClears(t, f)
	if !sameBindingIdentity(w.binding, before) {
		t.Fatal("another user replaced the session")
	}
	// The owner can still use the same menu after an unauthorized click.
	clickKeyboard(w, 7, data)
	assertKeyboardClears(t, f, messageID)
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
	assertKeyboardClears(t, f, messageID)
	clickKeyboard(w, 7, data)
	assertKeyboardClears(t, f, messageID, expiredMessageID)
	clickKeyboard(w, 7, data)
	assertKeyboardClears(t, f, messageID, expiredMessageID)
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

func TestNativeSelectionCleanupFailureDoesNotCloseSession(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before := w.binding
	w.confirm(confirmation{action: "ui", uiID: "native-choice", method: "select", user: 7, options: []string{"First", "Second"}}, "Choose an option", []string{"First", "Second", "Cancel"})
	data := resumeButtons(t, f)[1]["callback_data"].(string)
	messageID := f.messageCount()
	f.failKeyboardClear = true
	result := w.callback(&telegram.CallbackQuery{ID: "native-selection", From: telegram.User{ID: 7}, Data: data})
	assertKeyboardClears(t, f, messageID)
	if result != callbackDone || w.client == nil || !sameBindingIdentity(w.binding, before) || len(w.confirms) != 0 {
		t.Fatal("keyboard cleanup failure prevented native selection delivery")
	}
	f.failKeyboardClear = false
	command("/new")
	data = resumeButtons(t, f)[0]["callback_data"].(string)
	expiredMessageID := f.messageCount()
	token, _, _ := strings.Cut(data, ":")
	c := w.confirms[token]
	c.expires = time.Now().Add(-time.Second)
	w.confirms[token] = c
	w.expire()
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.keyboardClears) == 2
	})
	assertKeyboardClears(t, f, messageID, expiredMessageID)
	clickKeyboard(w, 7, data)
	assertKeyboardClears(t, f, messageID, expiredMessageID)
	if !sameBindingIdentity(w.binding, before) {
		t.Fatal("scheduled expiry allowed a stale confirmation to replace the session")
	}
}

func TestCompactConfirmationClearsBeforeCompletion(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before := w.binding
	w.operations = make(chan operationResult, 1)
	command("/compact")
	data := resumeButtons(t, f)[0]["callback_data"].(string)
	messageID := f.messageCount()
	clickKeyboard(w, 7, data)
	assertKeyboardClears(t, f, messageID)
	if !w.busy || !w.compacting || !sameBindingIdentity(w.binding, before) {
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
	assertKeyboardClears(t, f, messageID)
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
	assertKeyboardClears(t, f, messageID)
	if w.sessionID != sessions[1].ID || w.binding.Generation <= before.Generation || w.binding.Workspace != before.Workspace {
		t.Fatal("current-page selection did not restore the chosen session")
	}
	after := w.binding
	clickResume(w, 7, data)
	assertKeyboardClears(t, f, messageID)
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
				cleared = []int{confirmationID, messageID}
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
