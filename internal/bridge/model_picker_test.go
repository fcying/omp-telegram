package bridge

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"testing"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
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
	_, rows := pickerView(t, f)
	if len(rows) != 2 || len(rows[0]) != 4 || len(rows[1]) != 1 || rows[1][0]["text"] != "Cancel" {
		t.Fatalf("model picker rows = %v, want four cycle roles and separate cancel", rows)
	}
	buttons := pickerButtons(t, f)
	for i, role := range []string{"smol", "default", "slow", "free"} {
		if buttons[i]["text"] != fmt.Sprint(i+1) || !f.has(11, role) {
			t.Fatalf("choice %d lost its body role %q or numeric button: %v", i, role, buttons[i])
		}
	}
	if !f.has(11, "* default - fixture/safe:medium") {
		t.Fatal("current model role was not shown in the body")
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
			data := pickerButtons(t, f)[0]["callback_data"].(string)
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
			buttons := pickerButtons(t, f)
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
			if scenario == "cancel" && !strings.Contains(settingsMenuResult(t, f, messageID), "cycle role") {
				t.Fatal("cancelled model picker lost its operation context")
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
		{name: "initial state", command: "/thinking", kind: "model_state", data: json.RawMessage(`{`)},
		{name: "cancelled request", command: "/thinking", kind: "model_set_thinking", cancelled: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			command(scenario.command)
			messageID := f.messageCount()
			data := pickerButtons(t, f)[0]["callback_data"].(string)
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
	clickKeyboard(w, 7, pickerButtons(t, f)[0]["callback_data"].(string))
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

func TestConsumedModelMenusFinishOnRuntimeInterruption(t *testing.T) {
	for _, scenario := range []struct {
		name, menu, kind string
		replace          bool
	}{
		{name: "model close after native selection", menu: "/model", kind: "model_set_role"},
		{name: "model replacement after native mutation", menu: "/model", kind: "model_set_role", replace: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			before, client := w.binding, w.client
			command(scenario.menu)
			messageID := f.messageCount()
			data := pickerButtons(t, f)[0]["callback_data"].(string)
			var replacement string
			if scenario.replace {
				// Arm the normal /new confirmation before holding the settings result.
				command("/new " + t.TempDir())
				replacement = pickerButtons(t, f)[0]["callback_data"].(string)
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
			assertPickerModel(t, w, "fixture", "quick")
			pending, rows := pickerView(t, f)
			if !strings.Contains(pending, "Applying") || len(rows) != 0 || !w.controlInProgress() {
				t.Fatalf("consumed menu was not awaiting the native result: %q", pending)
			}
			if scenario.replace {
				w.callback(&telegram.CallbackQuery{ID: "replace-setting-runtime", From: telegram.User{ID: 7}, Data: replacement})
				if w.client == nil || w.client == client || w.binding.Generation <= before.Generation || !w.binding.Running || len(w.b.slots) != 1 {
					t.Fatal("confirmed replacement did not install a new running native session")
				}
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
			button := pickerButtons(t, f)[0]["callback_data"].(string)
			w.operationReturned(stale)
			clickKeyboard(w, 7, data)
			after, afterKeyboard := pickerView(t, f)
			if after != text || len(afterKeyboard) != len(keyboard) || pickerButtons(t, f)[0]["callback_data"].(string) != button || len(w.confirms) != 1 || w.client != currentClient || !sameBindingIdentity(w.binding, current) {
				t.Fatal("old settings result changed the replacement runtime or current menu")
			}
			assertPickerModel(t, w, "fixture", "safe")
			if got := settingsMenuResult(t, f, messageID); got != interrupted {
				t.Fatal("replayed result rewrote the old menu after a new menu opened")
			}
		})
	}
}

func TestChineseModelPickerPreservesNativeRolesAndIdentity(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	w.b.setLanguage(i18n.Chinese)
	command("/model")
	buttons := pickerButtons(t, f)
	if len(buttons) != 5 || buttons[1]["text"] != "2" || !f.has(11, "* default - fixture/safe:medium") {
		t.Fatalf("translated picker changed native role data: %v", buttons)
	}
	if buttons[4]["text"] != i18n.Text(i18n.Chinese, i18n.Msg.Cancel) || !f.has(11, i18n.Text(i18n.Chinese, i18n.Msg.ChooseOMPCycleRoleCurrentModel, "fixture/safe")) {
		t.Fatal("model picker did not translate its title and cancel control")
	}
	messageID := f.messageCount()
	clickKeyboard(w, 7, buttons[2]["callback_data"].(string))
	completed := settingsMenuResult(t, f, messageID)
	if !strings.Contains(completed, i18n.Text(i18n.Chinese, i18n.Msg.ModelSwitched, "fixture/deep")) || !strings.Contains(completed, i18n.Text(i18n.Chinese, i18n.Msg.Role, "slow")) {
		t.Fatalf("translated selection lost the actual model or role: %q", completed)
	}
	assertPickerModel(t, w, "fixture", "deep")
}

func TestChineseThinkingPickerKeepsProtocolOptions(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	w.b.setLanguage(i18n.Chinese)
	command("/thinking")
	_, rows := pickerView(t, f)
	if len(rows) != len(thinkingLevels)+1 {
		t.Fatalf("thinking picker rows = %v, want one short label per row", rows)
	}
	for _, row := range rows {
		if len(row) != 1 {
			t.Fatalf("thinking picker grouped short labels: %v", rows)
		}
	}
	buttons := pickerButtons(t, f)
	if len(buttons) != len(thinkingLevels)+1 || buttons[4]["text"] != i18n.Text(i18n.Chinese, i18n.Msg.High) {
		t.Fatalf("thinking picker did not translate its labels: %v", buttons)
	}
	for _, c := range w.confirms {
		if c.action == "thinking" && strings.Join(c.options, ",") != strings.Join(thinkingLevels[:], ",") {
			t.Fatalf("translated thinking picker changed protocol options: %v", c.options)
		}
	}
	messageID := f.messageCount()
	clickKeyboard(w, 7, buttons[4]["callback_data"].(string))
	var state modelSettings
	raw, err := w.call("get_state", nil)
	if err != nil || json.Unmarshal(raw, &state) != nil || state.ThinkingLevel != "high" {
		t.Fatalf("translated thinking choice did not send the native level: %s, %v", raw, err)
	}
	if got := settingsMenuResult(t, f, messageID); got != i18n.Text(i18n.Chinese, i18n.Msg.ThinkingLevel, "high") {
		t.Fatalf("thinking selection changed native state text: %q", got)
	}
}

func TestThinkingLevelLabelsPreserveProtocolValues(t *testing.T) {
	w, _, _ := setupWorkspaceWorker(t)
	labels := []struct {
		level   string
		message i18n.Message
	}{
		{"off", i18n.Msg.Off},
		{"minimal", i18n.Msg.Minimal},
		{"low", i18n.Msg.Low},
		{"medium", i18n.Msg.Medium},
		{"high", i18n.Msg.High},
		{"xhigh", i18n.Msg.Xhigh},
		{"max", i18n.Msg.Max},
	}
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		w.b.setLanguage(language)
		for i, label := range labels {
			if thinkingLevels[i] != label.level {
				t.Fatalf("thinking protocol level %d changed: %q", i, thinkingLevels[i])
			}
			if got, want := w.thinkingLevelLabel(label.level), i18n.Text(language, label.message); got != want {
				t.Fatalf("thinking label %q: got %q, want %q", label.level, got, want)
			}
		}
		for _, native := range []string{"Cancel", "custom", "high "} {
			if got := w.thinkingLevelLabel(native); got != native {
				t.Fatalf("native thinking value changed: got %q, want %q", got, native)
			}
		}
	}
}

func TestSettingsMenusPublishOneCompletion(t *testing.T) {
	for _, selection := range []struct {
		command string
		index   int
	}{
		{"/model", 0},
		{"/thinking", 4},
		{"/args", 0},
	} {
		for _, editFails := range []bool{false, true} {
			name := selection.command + "/edit"
			if editFails {
				name = selection.command + "/fallback"
			}
			t.Run(name, func(t *testing.T) {
				w, f, command := setupWorkspaceWorker(t)
				if selection.command == "/args" {
					configureTestArgPresets(w.b, [][]string{nil}, strings.Repeat("1", 64), 1)
				}
				command("/new " + t.TempDir())
				deliverReplies(t, w)
				command(selection.command)
				buttons := pickerButtons(t, f)
				messageID := f.messageCount()
				var before int
				requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&before))
				f.mu.Lock()
				offset := len(f.messages)
				f.failProgress = editFails
				f.mu.Unlock()
				clickKeyboard(w, 7, buttons[selection.index]["callback_data"].(string))
				f.mu.Lock()
				f.failProgress = false
				f.mu.Unlock()
				var after int
				requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&after))
				wantNew := 0
				if editFails {
					wantNew = 1
				} else {
					settingsMenuResult(t, f, messageID)
				}
				if after-before != wantNew {
					t.Fatalf("selection queued %d extra replies, want %d", after-before, wantNew)
				}
				deliverReplies(t, w)
				f.mu.Lock()
				newMessages := 0
				for _, request := range f.messages[offset:] {
					if request["message_id"] == nil {
						newMessages++
					}
				}
				f.mu.Unlock()
				if newMessages != wantNew {
					t.Fatalf("selection sent %d new Telegram bubbles, want %d", newMessages, wantNew)
				}
				switch selection.command {
				case "/model":
					assertPickerModel(t, w, "fixture", "quick")
				case "/thinking":
					raw, err := w.call("get_state", nil)
					requireStoreOK(t, err)
					var state modelSettings
					requireStoreOK(t, json.Unmarshal(raw, &state))
					if state.ThinkingLevel != "high" {
						t.Fatal("receipt routing changed the requested thinking level")
					}
				case "/args":
					requireArgsSelection(t, w.b, 1)
				}
			})
		}
	}
}

func TestModelMenuContentPagesKeepOriginalRole(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before := w.binding
	first := "fixture/" + strings.Repeat("long **selector** ", 50)
	second := "fixture/" + strings.Repeat("other <selector> ", 50)
	w.showModelPickerReady(7, omp.Model{}, []omp.ModelRole{{Role: "smol", Selector: first}, {Role: "slow", Selector: second}})
	body, rows := pickerView(t, f)
	if !strings.Contains(html.UnescapeString(body), first) || rows[0][0]["text"] != "1" || !strings.Contains(body, "Page 1/2") {
		t.Fatalf("first model page lost its full role: %q; %v", body, rows)
	}
	stale := rows[0][0]["callback_data"].(string)
	clickKeyboard(w, 7, askTestButton(t, f, "Next"))
	body, rows = pickerView(t, f)
	if !strings.Contains(html.UnescapeString(body), second) || strings.Contains(body, "<selector>") || rows[0][0]["text"] != "2" {
		t.Fatalf("second model page lost its full literal role: %q; %v", body, rows)
	}
	assertPickerModel(t, w, "fixture", "safe")
	clickKeyboard(w, 7, stale)
	assertPickerModel(t, w, "fixture", "safe")
	clickKeyboard(w, 7, rows[0][0]["callback_data"].(string))
	assertPickerModel(t, w, "fixture", "deep")
	if !sameBindingIdentity(before, w.binding) {
		t.Fatal("paged model selection replaced the session")
	}
}
