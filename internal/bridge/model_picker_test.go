package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

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
	assertKeyboardClears(t, f)
	assertPickerModel(t, w, "fixture", "safe")
	clickKeyboard(w, 7, data)
	assertKeyboardClears(t, f, messageID)
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
	assertKeyboardClears(t, f, messageID)
}

func TestModelPickerRejectsUnavailableSelection(t *testing.T) {
	for _, scenario := range []string{"generation", "busy", "compacting", "finishing", "queued"} {
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
			}
			clickKeyboard(w, 7, data)
			assertPickerModel(t, w, "fixture", "safe")
			assertKeyboardClears(t, f, messageID)
			w.busy, w.compacting, w.finishing = false, false, false
			w.queue = nil
			clickKeyboard(w, 7, data)
			assertPickerModel(t, w, "fixture", "safe")
			assertKeyboardClears(t, f, messageID)
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
				f.mu.Unlock()
				index, want = 0, "quick"
			}
			clickKeyboard(w, 7, buttons[index]["callback_data"].(string))
			assertKeyboardClears(t, f, messageID)
			assertPickerModel(t, w, "fixture", want)
			if w.client == nil {
				t.Fatal("keyboard cleanup failure closed the session")
			}
		})
	}
}

func TestManualModelSelectionReportsActualIdentity(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	command("/model custom-provider/custom/model")
	assertPickerModel(t, w, "custom-provider", "custom/model")
	var reply bool
	if err := w.b.db.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM outbox WHERE text LIKE '%custom-provider/custom/model%')").Scan(&reply); err != nil || !reply {
		t.Fatal("manual selection omitted actual model identity")
	}
	assertKeyboardClears(t, f)
}
