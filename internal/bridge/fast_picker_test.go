package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

func assertFastState(t *testing.T, w *worker, enabled, active bool) {
	t.Helper()
	raw, err := w.call("get_state", nil)
	var state struct {
		Enabled bool `json:"fastModeEnabled"`
		Active  bool `json:"fastModeActive"`
		Runs    int  `json:"fixtureRootPrompts"`
	}
	if err != nil || json.Unmarshal(raw, &state) != nil || state.Enabled != enabled || state.Active != active || state.Runs != 0 {
		t.Fatalf("fast state=%+v, want enabled=%t active=%t with no agent runs, err=%v", state, enabled, active, err)
	}
}

func TestFastPickerOwnershipCancelReplayAndBusy(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before := w.binding
	command("/fast")
	buttons := resumeButtons(t, f)
	messageID := f.messageCount()
	data := buttons[0]["callback_data"].(string)
	assertFastState(t, w, false, false)
	clickKeyboard(w, 8, data)
	if len(w.confirms) != 1 {
		t.Fatal("unauthorized selection consumed the fast mode menu")
	}
	assertFastState(t, w, false, false)
	f.failKeyboardClear = true
	clickKeyboard(w, 7, data)
	completed := settingsMenuResult(t, f, messageID)
	if completed != "Fast setting: on\nFast active: on" {
		t.Fatalf("completed fast menu = %q", completed)
	}
	assertFastState(t, w, true, true)
	command("/fast off")
	clickKeyboard(w, 7, data)
	assertFastState(t, w, false, false)
	if got := settingsMenuResult(t, f, messageID); got != completed {
		t.Fatal("replayed fast callback changed the completed menu")
	}
	command("/fast")
	buttons = resumeButtons(t, f)
	messageID = f.messageCount()
	clickKeyboard(w, 7, buttons[len(buttons)-1]["callback_data"].(string))
	assertFastState(t, w, false, false)
	if got := settingsMenuResult(t, f, messageID); got != "Cancel" {
		t.Fatalf("cancelled fast menu = %q", got)
	}
	command("/fast")
	buttons = resumeButtons(t, f)
	messageID = f.messageCount()
	w.busy = true
	clickKeyboard(w, 7, buttons[0]["callback_data"].(string))
	if got := settingsMenuResult(t, f, messageID); strings.Contains(got, "Fast setting:") {
		t.Fatalf("busy fast selection appeared successful: %q", got)
	}
	command("/fast:on")
	assertFastState(t, w, false, false)
	if !sameBindingIdentity(w.binding, before) {
		t.Fatal("fast mode command changed session identity")
	}
}

func TestFastReportsActualStateAndUnsupportedModel(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	command("/model:fixture/fast-fallback")
	command("/fast")
	messageID := f.messageCount()
	clickKeyboard(w, 7, resumeButtons(t, f)[0]["callback_data"].(string))
	assertFastState(t, w, true, false)
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil {
		t.Fatal(err)
	}
	fields := statusFields(text)
	if fields["Fast setting"] != "on" || fields["Fast active"] != "off" {
		t.Fatalf("requested setting was confused with active mode: %v", fields)
	}
	if got := settingsMenuResult(t, f, messageID); got != text {
		t.Fatalf("fast fallback menu = %q, durable reply = %q", got, text)
	}
	command("/fast off")
	command("/model fixture/no-fast")
	command("/fast")
	messageID = f.messageCount()
	clickKeyboard(w, 7, resumeButtons(t, f)[0]["callback_data"].(string))
	if got := settingsMenuResult(t, f, messageID); !strings.Contains(got, "could not be confirmed") || strings.Contains(got, "Fast setting:") {
		t.Fatalf("unsupported fast mode appeared successful: %q", got)
	}
	assertFastState(t, w, false, false)
	command("/fast status")
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil || statusFields(text)["Fast active"] != "off" {
		t.Fatal("unsupported model was reported as fast")
	}
}
