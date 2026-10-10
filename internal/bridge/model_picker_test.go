package bridge

import (
	"encoding/json"
	"strings"
	"testing"

	"omp-telegram/internal/telegram"
)

func settingsMenuResult(t *testing.T, f *fakeHTTP, messageID int) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.messages) - 1; i >= 0; i-- {
		message := f.messages[i]
		if message["message_id"] != float64(messageID) {
			continue
		}
		markup, ok := message["reply_markup"].(map[string]any)
		if !ok {
			t.Fatal("completed settings menu omitted its empty keyboard")
		}
		rows, ok := markup["inline_keyboard"].([]any)
		if !ok || len(rows) != 0 {
			t.Fatal("completed settings menu retained actionable buttons")
		}
		text, _ := message["text"].(string)
		if text == "" || strings.Contains(text, "Applying") {
			t.Fatalf("settings menu did not finish: %q", text)
		}
		return text
	}
	t.Fatalf("original settings menu %d was not updated", messageID)
	return ""
}

func assertSettingsMenuNeverInterrupted(t *testing.T, f *fakeHTTP, messageID int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, message := range f.messages {
		if message["message_id"] == float64(messageID) {
			text, _ := message["text"].(string)
			if strings.Contains(strings.ToLower(text), "interrupted") {
				t.Fatalf("completed settings menu received a spurious interruption: %q", text)
			}
		}
	}
}

func assertPickerModel(t *testing.T, w *worker, provider, id string) {
	t.Helper()
	data, err := w.call("get_state", nil)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Model struct {
			Provider string `json:"provider"`
			ID       string `json:"id"`
		} `json:"model"`
		RootPrompts int `json:"fixtureRootPrompts"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.Model.Provider != provider || state.Model.ID != id {
		t.Fatalf("actual model = %s/%s, want %s/%s", state.Model.Provider, state.Model.ID, provider, id)
	}
	if state.RootPrompts != 0 {
		t.Fatalf("model operation invoked %d root prompts", state.RootPrompts)
	}
}

func TestModelPickerUsesCycleRolesAndOwner(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	original := w.binding
	command("/model")
	buttons := resumeButtons(t, f)
	if len(buttons) != 5 {
		t.Fatalf("model picker choices = %v, want four cycle roles and cancel", buttons)
	}
	for i, role := range []string{"smol", "default", "slow", "free"} {
		if !strings.Contains(buttons[i]["text"].(string), role) {
			t.Fatalf("choice %d does not show cycle role %q: %v", i, role, buttons[i])
		}
	}
	if !f.has(11, "fixture/safe") {
		t.Fatal("picker omitted the actual current model")
	}
	assertPickerModel(t, w, "fixture", "safe")
	if !sameBindingIdentity(w.binding, original) || w.busy || len(w.queue) != 0 {
		t.Fatal("opening picker changed session or work state")
	}
	messageID := f.messageCount()
	data := buttons[2]["callback_data"].(string)
	clickKeyboard(w, 8, data)
	if len(w.confirms) != 1 {
		t.Fatal("unauthorized selection consumed the model menu")
	}
	assertPickerModel(t, w, "fixture", "safe")
	clickKeyboard(w, 7, data)
	completed := settingsMenuResult(t, f, messageID)
	if !strings.Contains(completed, "fixture/deep") || !strings.Contains(completed, "Role: slow") {
		t.Fatalf("completed model menu omitted actual identity or chosen role: %q", completed)
	}
	assertPickerModel(t, w, "fixture", "deep")
	var reply bool
	if err := w.b.db.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM outbox WHERE text LIKE '%fixture/deep%')").Scan(&reply); err != nil || !reply {
		t.Fatal("successful selection did not display resolved RPC model identity")
	}
	if !sameBindingIdentity(w.binding, original) {
		t.Fatal("model selection replaced the session")
	}
	// A consumed button cannot switch back after another model is selected.
	command("/model fixture/manual")
	assertPickerModel(t, w, "fixture", "manual")
	clickKeyboard(w, 7, data)
	assertPickerModel(t, w, "fixture", "manual")
	if got := settingsMenuResult(t, f, messageID); got != completed {
		t.Fatal("replayed model callback changed the completed menu")
	}
	command("/close")
	if got := settingsMenuResult(t, f, messageID); got != completed {
		t.Fatal("closing after a successful model change rewrote its result")
	}
	assertSettingsMenuNeverInterrupted(t, f, messageID)
}

func TestModelPickerRejectsUnavailableSelection(t *testing.T) {
	for _, scenario := range []string{"generation", "busy", "compacting", "finishing", "queued", "exporting"} {
		t.Run(scenario, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			command("/model")
			data := resumeButtons(t, f)[0]["callback_data"].(string)
			messageID := f.messageCount()
			switch scenario {
			case "generation":
				w.binding.Generation++
			case "busy":
				w.busy = true
			case "compacting":
				w.compacting = true
			case "finishing":
				w.finishing = true
			case "queued":
				w.queue = []queued{{text: "pending"}}
			case "exporting":
				w.exportingSession = w.sessionID
			}
			clickKeyboard(w, 7, data)
			assertPickerModel(t, w, "fixture", "safe")
			var completed string
			if scenario == "generation" {
				if len(w.confirms) != 0 {
					t.Fatal("expired model menu remained actionable")
				}
			} else {
				completed = settingsMenuResult(t, f, messageID)
				if strings.Contains(completed, "Model switched") {
					t.Fatalf("rejected model selection appeared successful: %q", completed)
				}
			}
			w.busy, w.compacting, w.finishing = false, false, false
			w.exportingSession = ""
			w.queue = nil
			clickKeyboard(w, 7, data)
			assertPickerModel(t, w, "fixture", "safe")
			if completed != "" && settingsMenuResult(t, f, messageID) != completed {
				t.Fatal("replayed rejected selection changed its menu")
			}
		})
	}
}

func TestModelPickerCancelAndCleanupFailure(t *testing.T) {
	for _, scenario := range []string{"cancel", "cleanup-failure"} {
		t.Run(scenario, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			command("/model")
			buttons := resumeButtons(t, f)
			messageID := f.messageCount()
			index, want := len(buttons)-1, "safe"
			if scenario == "cleanup-failure" {
				f.mu.Lock()
				f.failKeyboardClear = true
				f.failProgress = true
				f.mu.Unlock()
				index, want = 0, "quick"
			}
			clickKeyboard(w, 7, buttons[index]["callback_data"].(string))
			if scenario == "cancel" && settingsMenuResult(t, f, messageID) != "Cancel" {
				t.Fatal("cancelled model picker retained its selection prompt")
			}
			assertPickerModel(t, w, "fixture", want)
			if w.client == nil {
				t.Fatal("keyboard cleanup failure closed the session")
			}
		})
	}
}

func TestManualModelSelectionReportsActualIdentity(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	command("/model custom-provider/custom/model")
	assertPickerModel(t, w, "custom-provider", "custom/model")
	var reply bool
	if err := w.b.db.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM outbox WHERE text LIKE '%custom-provider/custom/model%')").Scan(&reply); err != nil || !reply {
		t.Fatal("manual selection omitted actual model identity")
	}
	if len(w.confirms) != 0 {
		t.Fatal("typed model selection created a menu")
	}
}

func TestSettingsMenusReportUnconfirmedResults(t *testing.T) {
	for _, scenario := range []struct {
		name, command, kind string
		data                json.RawMessage
		cancelled           bool
	}{
		{name: "model identity", command: "/model", kind: "model_set_role", data: json.RawMessage(`{"provider":"fixture"}`)},
		{name: "thinking level", command: "/thinking", kind: "model_verify_thinking", data: json.RawMessage(`{}`)},
		{name: "fast active", command: "/fast", kind: "model_set_fast", data: json.RawMessage(`{"enabled":true}`)},
		{name: "initial state", command: "/thinking", kind: "model_state", data: json.RawMessage(`{`)},
		{name: "cancelled request", command: "/fast", kind: "model_set_fast", cancelled: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			command(scenario.command)
			messageID := f.messageCount()
			data := resumeButtons(t, f)[0]["callback_data"].(string)
			w.callback(&telegram.CallbackQuery{ID: "unconfirmed-setting", From: telegram.User{ID: 7}, Data: data})
			result := waitOperation(t, w)
			result.kind, result.data, result.cancelled = scenario.kind, scenario.data, scenario.cancelled
			w.operationReturned(result)
			text := settingsMenuResult(t, f, messageID)
			if !strings.Contains(text, "could not be confirmed") || strings.Contains(text, "Model switched") || strings.Contains(text, "Fast setting:") {
				t.Fatalf("unconfirmed settings result appeared successful: %q", text)
			}
			command("/close")
			if got := settingsMenuResult(t, f, messageID); got != text {
				t.Fatal("closing after a failed settings change rewrote its failure")
			}
			assertSettingsMenuNeverInterrupted(t, f, messageID)
		})
	}
}

func TestStaleSettingsResultCannotEditNewMenu(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	command("/model")
	oldID := int64(f.messageCount())
	oldGeneration, oldClient := w.binding.Generation, w.client.ID()
	command("/new " + t.TempDir())
	clickKeyboard(w, 7, resumeButtons(t, f)[0]["callback_data"].(string))
	command("/thinking")
	before, rows := pickerView(t, f)
	w.operationReturned(operationResult{
		kind: "model_set_role", generation: oldGeneration, clientID: oldClient,
		data: json.RawMessage(`{"provider":"fixture","id":"stale"}`),
		meta: modelOperationRequest{action: "model_select", role: "slow", messageID: oldID},
	})
	after, afterRows := pickerView(t, f)
	if after != before || len(afterRows) != len(rows) || len(w.confirms) != 1 {
		t.Fatal("stale model result edited or consumed the new settings menu")
	}
}

func TestConsumedModelAndFastMenusFinishOnRuntimeInterruption(t *testing.T) {
	for _, scenario := range []struct {
		name, menu, kind string
		replace          bool
	}{
		{name: "model close after native selection", menu: "/model", kind: "model_set_role"},
		{name: "fast replacement after native mutation", menu: "/fast", kind: "model_set_fast", replace: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			before, client := w.binding, w.client
			command(scenario.menu)
			messageID := f.messageCount()
			data := resumeButtons(t, f)[0]["callback_data"].(string)
			var replacement string
			if scenario.replace {
				// Arm the normal /new confirmation before holding the settings result.
				command("/new " + t.TempDir())
				replacement = resumeButtons(t, f)[0]["callback_data"].(string)
			}
			w.callback(&telegram.CallbackQuery{ID: "interrupt-setting", From: telegram.User{ID: 7}, Data: data})
			state := waitOperation(t, w)
			if state.kind != "model_state" {
				t.Fatalf("initial settings operation = %q", state.kind)
			}
			w.operationReturned(state)
			stale := waitOperation(t, w)
			if stale.kind != scenario.kind || stale.err != nil || stale.cancelled {
				t.Fatalf("native settings operation did not succeed: %+v", stale)
			}
			if scenario.replace {
				assertFastState(t, w, true, true)
			} else {
				assertPickerModel(t, w, "fixture", "quick")
			}
			pending, rows := pickerView(t, f)
			if !strings.Contains(pending, "Applying") || len(rows) != 0 || !w.controlInProgress() {
				t.Fatalf("consumed menu was not awaiting the native result: %q", pending)
			}
			if scenario.replace {
				w.callback(&telegram.CallbackQuery{ID: "replace-setting-runtime", From: telegram.User{ID: 7}, Data: replacement})
				if w.client == nil || w.client == client || w.binding.Generation <= before.Generation || !w.binding.Running || len(w.b.slots) != 1 {
					t.Fatal("confirmed replacement did not install a new running native session")
				}
				assertFastState(t, w, false, false)
			} else {
				command("/close")
				if w.client != nil || w.binding.Running || len(w.b.slots) != 0 {
					t.Fatal("close did not release the native session and capacity")
				}
			}
			if w.controlInProgress() || len(w.confirms) != 0 {
				t.Fatal("interruption retained settings control or actionable confirmations")
			}
			interrupted := settingsMenuResult(t, f, messageID)
			if !strings.Contains(strings.ToLower(interrupted), "interrupted") || !strings.Contains(interrupted, "could not be confirmed") || strings.Contains(interrupted, "Model switched") || strings.Contains(interrupted, "Fast setting:") {
				t.Fatalf("interrupted menu claimed a confirmed result: %q", interrupted)
			}
			w.operationReturned(stale)
			clickKeyboard(w, 7, data)
			if got := settingsMenuResult(t, f, messageID); got != interrupted {
				t.Fatal("late native result or replay overwrote the original interruption")
			}
			if !scenario.replace {
				if w.client != nil || w.binding.Running || w.controlInProgress() {
					t.Fatal("late result or replay reopened the closed runtime")
				}
				return
			}
			command("/model")
			current, currentClient := w.binding, w.client
			text, keyboard := pickerView(t, f)
			button := resumeButtons(t, f)[0]["callback_data"].(string)
			w.operationReturned(stale)
			clickKeyboard(w, 7, data)
			after, afterKeyboard := pickerView(t, f)
			if after != text || len(afterKeyboard) != len(keyboard) || resumeButtons(t, f)[0]["callback_data"].(string) != button || len(w.confirms) != 1 || w.client != currentClient || !sameBindingIdentity(w.binding, current) {
				t.Fatal("old settings result changed the replacement runtime or current menu")
			}
			assertFastState(t, w, false, false)
			assertPickerModel(t, w, "fixture", "safe")
			if got := settingsMenuResult(t, f, messageID); got != interrupted {
				t.Fatal("replayed result rewrote the old menu after a new menu opened")
			}
		})
	}
}
