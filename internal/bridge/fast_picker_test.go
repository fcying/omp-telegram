package bridge

import (
	"encoding/json"
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
	assertKeyboardClears(t, f)
	assertFastState(t, w, false, false)
	f.failKeyboardClear = true
	clickKeyboard(w, 7, data)
	assertKeyboardClears(t, f, messageID)
	assertFastState(t, w, true, true)
	command("/fast off")
	clickKeyboard(w, 7, data)
	assertFastState(t, w, false, false)
	command("/fast")
	buttons = resumeButtons(t, f)
	clickKeyboard(w, 7, buttons[len(buttons)-1]["callback_data"].(string))
	assertFastState(t, w, false, false)
	command("/fast")
	buttons = resumeButtons(t, f)
	w.busy = true
	clickKeyboard(w, 7, buttons[0]["callback_data"].(string))
	command("/fast on")
	assertFastState(t, w, false, false)
	if !sameBindingIdentity(w.binding, before) {
		t.Fatal("fast mode command changed session identity")
	}
}

func TestFastReportsActualStateAndUnsupportedModel(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	command("/model fixture/fast-fallback")
	command("/fast on")
	assertFastState(t, w, true, false)
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil {
		t.Fatal(err)
	}
	fields := statusFields(text)
	if fields["Fast setting"] != "on" || fields["Fast active"] != "off" {
		t.Fatalf("requested setting was confused with active mode: %v", fields)
	}
	command("/fast off")
	command("/model fixture/no-fast")
	command("/fast on")
	assertFastState(t, w, false, false)
	command("/fast status")
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil || statusFields(text)["Fast active"] != "off" {
		t.Fatal("unsupported model was reported as fast")
	}
}
