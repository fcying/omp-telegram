package bridge

import (
	"encoding/json"
	"strings"
	"testing"

	"omp-telegram/internal/telegram"
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
	if len(w.confirms) != 1 {
		t.Fatal("unauthorized selection consumed the thinking menu")
	}
	assertThinkingLevel(t, w, "medium")
	f.failKeyboardClear = true
	clickKeyboard(w, 7, data)
	completed := settingsMenuResult(t, f, messageID)
	if !strings.Contains(completed, "Thinking level: high") || !strings.Contains(completed, "adjusted the requested max") {
		t.Fatalf("thinking menu reported the requested rather than verified level: %q", completed)
	}
	assertThinkingLevel(t, w, "high")
	var actualReply bool
	if err := w.b.db.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM outbox WHERE text LIKE 'Thinking level: high%')").Scan(&actualReply); err != nil || !actualReply {
		t.Fatalf("adjusted thinking level was not reported: %v", err)
	}
	if !sameBindingIdentity(w.binding, before) {
		t.Fatal("thinking change replaced the session")
	}
	if _, err := w.call("set_thinking_level", map[string]any{"level": "low"}); err != nil {
		t.Fatal(err)
	}
	clickKeyboard(w, 7, data)
	assertThinkingLevel(t, w, "low")
	if got := settingsMenuResult(t, f, messageID); got != completed {
		t.Fatal("replayed thinking callback changed the completed menu")
	}
}

func TestThinkingPickerRejectsUnavailableActions(t *testing.T) {
	for _, scenario := range []string{"cancel", "busy", "compacting", "queue", "generation", "rpc-failure"} {
		t.Run(scenario, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			command("/thinking")
			buttons := resumeButtons(t, f)
			messageID := f.messageCount()
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
			if scenario == "generation" {
				if len(w.confirms) != 0 {
					t.Fatal("expired thinking menu remained actionable")
				}
				return
			}
			completed := settingsMenuResult(t, f, messageID)
			if scenario == "cancel" {
				if completed != "Cancel" {
					t.Fatalf("cancelled thinking menu = %q", completed)
				}
			} else if strings.Contains(completed, "Thinking level:") {
				t.Fatalf("rejected thinking selection appeared successful: %q", completed)
			}
			if scenario == "rpc-failure" {
				command("/close")
				if got := settingsMenuResult(t, f, messageID); got != completed {
					t.Fatal("closing after a native thinking rejection rewrote its failure")
				}
				assertSettingsMenuNeverInterrupted(t, f, messageID)
			}
		})
	}
}

func TestThinkingMenuStaysPendingUntilVerified(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	command("/thinking")
	messageID := f.messageCount()
	data := resumeButtons(t, f)[6]["callback_data"].(string)
	w.callback(&telegram.CallbackQuery{ID: "verify-thinking-menu", From: telegram.User{ID: 7}, Data: data})
	for _, kind := range []string{"model_state", "model_set_thinking", "model_verify_thinking"} {
		pending, rows := pickerView(t, f)
		if !strings.Contains(pending, "Applying") || strings.Contains(pending, "Thinking level:") || len(rows) != 0 {
			t.Fatalf("thinking menu showed success before %s verified the result: %q", kind, pending)
		}
		result := waitOperation(t, w)
		if result.kind != kind {
			t.Fatalf("thinking operation = %q, want %q", result.kind, kind)
		}
		w.operationReturned(result)
	}
	completed := settingsMenuResult(t, f, messageID)
	if !strings.Contains(completed, "Thinking level: high") || !strings.Contains(completed, "adjusted the requested max") {
		t.Fatalf("verified thinking menu = %q", completed)
	}
	assertThinkingLevel(t, w, "high")
	command("/close")
	if got := settingsMenuResult(t, f, messageID); got != completed {
		t.Fatal("closing after verified thinking success rewrote its result")
	}
	assertSettingsMenuNeverInterrupted(t, f, messageID)
}

func TestConsumedThinkingMenuInterruptedAtUncertainEdges(t *testing.T) {
	for _, scenario := range []struct {
		name, heldKind, actualLevel string
		replace                     bool
	}{
		{name: "close before mutation", heldKind: "model_state", actualLevel: "medium"},
		{name: "replace before verification receipt", heldKind: "model_verify_thinking", actualLevel: "high", replace: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			before, client := w.binding, w.client
			command("/thinking")
			messageID := f.messageCount()
			data := resumeButtons(t, f)[6]["callback_data"].(string)
			var replacement string
			if scenario.replace {
				command("/new " + t.TempDir())
				replacement = resumeButtons(t, f)[0]["callback_data"].(string)
			}
			w.callback(&telegram.CallbackQuery{ID: "interrupt-thinking", From: telegram.User{ID: 7}, Data: data})
			var stale operationResult
			for _, kind := range []string{"model_state", "model_set_thinking", "model_verify_thinking"} {
				result := waitOperation(t, w)
				if result.kind != kind || result.err != nil || result.cancelled {
					t.Fatalf("thinking operation at %s did not succeed: %+v", kind, result)
				}
				if kind == scenario.heldKind {
					stale = result
					break
				}
				w.operationReturned(result)
			}
			assertThinkingLevel(t, w, scenario.actualLevel)
			pending, rows := pickerView(t, f)
			if !strings.Contains(pending, "Applying") || strings.Contains(pending, "Thinking level:") || len(rows) != 0 || !w.controlInProgress() {
				t.Fatalf("thinking menu was not awaiting %s: %q", scenario.heldKind, pending)
			}
			if scenario.replace {
				w.callback(&telegram.CallbackQuery{ID: "replace-thinking-runtime", From: telegram.User{ID: 7}, Data: replacement})
				if w.client == nil || w.client == client || w.binding.Generation <= before.Generation || !w.binding.Running || len(w.b.slots) != 1 {
					t.Fatal("replacement did not install a new native session")
				}
				assertThinkingLevel(t, w, "medium")
			} else {
				command("/close")
				if w.client != nil || w.binding.Running || len(w.b.slots) != 0 {
					t.Fatal("close retained the thinking runtime or capacity")
				}
			}
			if w.controlInProgress() || len(w.confirms) != 0 {
				t.Fatal("thinking interruption retained control or actionable menus")
			}
			interrupted := settingsMenuResult(t, f, messageID)
			if !strings.Contains(strings.ToLower(interrupted), "interrupted") || !strings.Contains(interrupted, "could not be confirmed") || strings.Contains(interrupted, "Thinking level:") {
				t.Fatalf("uncertain thinking change appeared successful: %q", interrupted)
			}
			w.operationReturned(stale)
			clickKeyboard(w, 7, data)
			if got := settingsMenuResult(t, f, messageID); got != interrupted {
				t.Fatal("stale thinking result or callback replay rewrote the interruption")
			}
			if !scenario.replace {
				if w.client != nil || w.binding.Running || w.controlInProgress() {
					t.Fatal("pre-mutation result or replay revived the closed runtime")
				}
				return
			}
			command("/thinking")
			current, currentClient := w.binding, w.client
			text, keyboard := pickerView(t, f)
			button := resumeButtons(t, f)[0]["callback_data"].(string)
			w.operationReturned(stale)
			clickKeyboard(w, 7, data)
			after, afterKeyboard := pickerView(t, f)
			if after != text || len(afterKeyboard) != len(keyboard) || resumeButtons(t, f)[0]["callback_data"].(string) != button || len(w.confirms) != 1 || w.client != currentClient || !sameBindingIdentity(w.binding, current) {
				t.Fatal("old thinking verification changed the current session or menu")
			}
			assertThinkingLevel(t, w, "medium")
			if got := settingsMenuResult(t, f, messageID); got != interrupted {
				t.Fatal("late verification rewrote the original interruption after a new menu opened")
			}
		})
	}
}
