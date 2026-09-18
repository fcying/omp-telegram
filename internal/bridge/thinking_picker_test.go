package bridge

import (
	"encoding/json"
	"testing"
)

func assertThinkingLevel(t *testing.T, w *worker, want string) {
	t.Helper()
	raw, err := w.call("get_state", nil)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Level string `json:"thinkingLevel"`
		Runs  int    `json:"fixtureRootPrompts"`
	}
	if err := json.Unmarshal(raw, &state); err != nil || state.Level != want || state.Runs != 0 {
		t.Fatalf("thinking state = %+v, want level %q and no model run, err=%v", state, want, err)
	}
}

func TestThinkingPickerOwnerAdjustmentAndReplay(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before := w.binding
	command("/thinking")
	buttons := resumeButtons(t, f)
	assertThinkingLevel(t, w, "medium")
	messageID := f.messageCount()
	data := buttons[6]["callback_data"].(string)
	clickKeyboard(w, 8, data)
	assertKeyboardClears(t, f)
	assertThinkingLevel(t, w, "medium")
	f.failKeyboardClear = true
	clickKeyboard(w, 7, data)
	assertKeyboardClears(t, f, messageID)
	assertThinkingLevel(t, w, "high")
	var actualReply bool
	if err := w.b.db.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM outbox WHERE text LIKE 'Thinking level: high%')").Scan(&actualReply); err != nil || !actualReply {
		t.Fatalf("adjusted thinking level was not reported: %v", err)
	}
	if w.binding != before {
		t.Fatal("thinking change replaced the session")
	}
	if _, err := w.call("set_thinking_level", map[string]any{"level": "low"}); err != nil {
		t.Fatal(err)
	}
	clickKeyboard(w, 7, data)
	assertThinkingLevel(t, w, "low")
	assertKeyboardClears(t, f, messageID)
}

func TestThinkingPickerRejectsUnavailableActions(t *testing.T) {
	for _, scenario := range []string{"cancel", "busy", "compacting", "queue", "generation", "rpc-failure"} {
		t.Run(scenario, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			command("/thinking")
			buttons := resumeButtons(t, f)
			index := 2
			switch scenario {
			case "cancel":
				index = len(buttons) - 1
			case "busy":
				w.busy = true
			case "compacting":
				w.compacting = true
			case "queue":
				w.b.cfg.QueueCapacity = 1
				command("deferred")
			case "generation":
				w.binding.Generation++
			case "rpc-failure":
				command("/model fixture/reject-thinking")
			}
			clickKeyboard(w, 7, buttons[index]["callback_data"].(string))
			assertThinkingLevel(t, w, "medium")
			assertKeyboardClears(t, f, f.messageCount())
		})
	}
}
