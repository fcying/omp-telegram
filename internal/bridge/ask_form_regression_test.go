package bridge

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNativeHTMLFormChoiceResponseUsesOriginalOptionCount(t *testing.T) {
	w, f, _ := askTestWorker(t)
	askTestEvent(t, w, map[string]any{
		"id": "filtered-choices", "method": "form",
		"html": `<form>
			<label><input type="checkbox" name="features" value="hidden" checked disabled>Disabled feature</label>
			<label><input type="checkbox" name="features" value="usable">Visible feature</label>
			<select name="integrations" multiple>
				<option value="disabled" selected disabled>Disabled integration</option>
				<optgroup label="Disabled group" disabled>
					<option value="grouped" selected>Disabled group member</option>
				</optgroup>
				<option value="selected">Visible integration</option>
			</select>
			<select name="skipped" multiple>
				<option value="disabled" selected disabled>Disabled tool</option>
				<option value="tool">Visible tool</option>
			</select>
			<label><input type="checkbox" name="single" value="solo">Standalone feature</label>
		</form>`,
	})

	for _, step := range []struct {
		field string
		label string
		value string
		pick  bool
	}{
		{"features", "Visible feature", "usable", true},
		{"integrations", "Visible integration", "selected", true},
		{"skipped", "Visible tool", "tool", false},
		{"single", "Standalone feature", "solo", true},
	} {
		if w.ask == nil || w.ask.form == nil || w.ask.form.fields[w.ask.question].name != step.field {
			t.Fatalf("form field %q was not presented", step.field)
		}
		options := w.ask.questions[w.ask.question].Options
		if len(options) != 1 || options[0].Label != step.label || options[0].Value != step.value || hasAskSelection(w.ask.selected) {
			t.Fatalf("%s options/default selection = %#v / %#v; want only unselected %q", step.field, options, w.ask.selected, step.label)
		}
		if step.pick {
			askTestClick(t, w, f, step.label)
		}
		askTestClick(t, w, f, "Confirm selection")
	}

	responses := askTestState(t, w)
	if len(responses) != 1 || responses[0].ID != "filtered-choices" || responses[0].Cancelled {
		t.Fatalf("form response count/status = %+v", responses)
	}
	want := map[string]any{
		"features":     []any{"usable"},
		"integrations": []any{"selected"},
		"skipped":      []any{},
		"single":       "solo",
	}
	if !reflect.DeepEqual(responses[0].Data, want) {
		t.Fatalf("form data = %#v, want %#v", responses[0].Data, want)
	}
}

func TestNativeHTMLFormRepliesEnforceFullPatternAndOMPEmail(t *testing.T) {
	w, f, _ := askTestWorker(t)
	askTestEvent(t, w, map[string]any{
		"id": "validate-text", "method": "form",
		"html": `<form>
			<label>Code <input name="code" pattern="alpha|beta" required></label>
			<label>Contact <input name="contact" type="email" required></label>
			<label>Backup <input name="backup" type="email"></label>
		</form>`,
	})

	id := int64(210)
	for _, step := range []struct {
		field   string
		invalid []string
		valid   string
	}{
		{"code", []string{"alphabet", "prebeta"}, "beta"},
		{
			field:   "contact",
			invalid: []string{"@host", "name@", "name@@host", "name\u00a0@host", "name@host\v", "name@host\uFEFF", "\uFEFF"},
			valid:   "ops+tag@intranet",
		},
		{"backup", nil, "\uFEFF"},
	} {
		if w.ask == nil || w.ask.form == nil || w.ask.form.fields[w.ask.question].name != step.field {
			t.Fatalf("form field %q was not presented", step.field)
		}
		prompt := askTestPrompt(t, f)
		for _, invalid := range step.invalid {
			askTestReply(t, w, id, 7, 11, invalid, prompt)
			id++
			if w.ask == nil || w.ask.form == nil || !w.ask.input || w.ask.form.fields[w.ask.question].name != step.field {
				t.Fatalf("invalid %s reply %q did not reopen the same field", step.field, invalid)
			}
			retry := askTestPrompt(t, f)
			if retry.MessageID == prompt.MessageID || retry.MessageID != w.ask.inputID {
				t.Fatalf("invalid %s reply %q did not create a new live prompt", step.field, invalid)
			}
			prompt = retry
		}
		askTestReply(t, w, id, 7, 11, step.valid, prompt)
		id++
	}

	responses := askTestState(t, w)
	if len(responses) != 1 || responses[0].ID != "validate-text" || responses[0].Cancelled {
		t.Fatalf("form response count/status = %+v", responses)
	}
	want := map[string]any{"code": "beta", "contact": "ops+tag@intranet", "backup": "\uFEFF"}
	if !reflect.DeepEqual(responses[0].Data, want) {
		t.Fatalf("form data = %#v, want %#v", responses[0].Data, want)
	}
}

func TestNativeHTMLFormOtherUsesOriginalOptions(t *testing.T) {
	for _, test := range []struct {
		name, kind, label string
		custom            bool
	}{
		{"disabled-other-radio", "radio", "Other", false},
		{"disabled-other-checkbox", "checkbox", "Other (specify)", false},
		{"prefix-is-not-word", "radio", "Otherwise", true},
		{"underscore-is-word", "checkbox", "Other_value", true},
		{"punctuation-is-boundary", "radio", "OTHER-extra", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			form := fmt.Sprintf(`<form><label><input type="%s" name="choice" value="a">A</label><label><input type="%s" name="choice" value="b">B</label><label><input type="%s" name="choice" value="c" disabled>%s</label></form>`, test.kind, test.kind, test.kind, test.label)
			askTestEvent(t, w, map[string]any{"id": "other-choice", "method": "form", "html": form})
			body, rows := pickerView(t, f)
			if strings.Contains(body, test.label) {
				t.Fatalf("disabled option displayed: %q", body)
			}
			found := false
			for _, row := range rows {
				for _, button := range row {
					if button["text"] == "Custom answer" {
						found = true
					}
				}
			}
			if found != test.custom {
				t.Fatalf("Custom answer visible = %v, want %v", found, test.custom)
			}
		})
	}
}

func TestNativeHTMLFormUnsupportedPatternCancelsBeforeQuestions(t *testing.T) {
	w, _, _ := askTestWorker(t)
	client, root, turn := w.client, w.rootRequestID, w.turn
	askTestEvent(t, w, map[string]any{
		"id": "unsupported-pattern", "method": "form",
		"html": `<form><label>First <input name="first"></label><label>Code <input name="code" pattern="(?=.*[A-Z])[A-Za-z0-9]{4,}" required></label></form>`,
	})
	responses := askTestState(t, w)
	if len(responses) != 1 || responses[0].ID != "unsupported-pattern" || !responses[0].Cancelled || responses[0].Data != nil {
		t.Fatalf("unsupported form response = %+v", responses)
	}
	if w.ask != nil || w.client != client || w.rootRequestID != root || w.turn != turn || !w.busy {
		t.Fatal("unsupported form opened a question or changed root ownership")
	}
	var count int
	err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE text LIKE ?", "%validation pattern the bridge cannot support%").Scan(&count)
	if err != nil || count != 1 {
		t.Fatalf("unsupported pattern cancellation notice count = %d, error = %v", count, err)
	}
}

func TestNativeHTMLFormDisabledSelectedPlaceholderRequiresChoice(t *testing.T) {
	w, f, _ := askTestWorker(t)
	askTestEvent(t, w, map[string]any{
		"id": "placeholder", "method": "form",
		"html": `<form><select name="language" required><option value="" disabled selected>Choose a language</option><option value="go">Go</option><option value="rust">Rust</option></select></form>`,
	})
	if w.ask == nil || hasAskSelection(w.ask.selected) {
		t.Fatal("disabled selected placeholder became a valid default")
	}
	body, rows := pickerView(t, f)
	if strings.Contains(body, "Choose a language") {
		t.Fatal("disabled placeholder appeared as an option")
	}
	for _, row := range rows {
		for _, button := range row {
			if button["text"] == "Use default" {
				t.Fatal("disabled selected placeholder exposed a default submission")
			}
		}
	}
	askTestClick(t, w, f, "Rust")
	responses := askTestState(t, w)
	if len(responses) != 1 || responses[0].Cancelled || !reflect.DeepEqual(responses[0].Data, map[string]any{"language": "rust"}) {
		t.Fatalf("selected response = %+v", responses)
	}
}

func TestNativeHTMLFormURLsAndWhitespaceNumber(t *testing.T) {
	w, f, _ := askTestWorker(t)
	askTestEvent(t, w, map[string]any{
		"id": "url-number", "method": "form",
		"html": `<form><label>Mail <input type="url" name="mail" required></label><label>URN <input type="url" name="urn" required></label><label>Count <input type="number" name="count" min="1" max="9" required></label></form>`,
	})
	for i, value := range []string{"mailto:dev@example.com", "urn:example:animal:ferret:nose", " 4 "} {
		askTestReply(t, w, int64(230+i), 7, 11, value, askTestPrompt(t, f))
	}
	responses := askTestState(t, w)
	want := map[string]any{"mail": "mailto:dev@example.com", "urn": "urn:example:animal:ferret:nose", "count": float64(4)}
	if len(responses) != 1 || responses[0].Cancelled || !reflect.DeepEqual(responses[0].Data, want) {
		t.Fatalf("URL and number response = %+v, want %#v", responses, want)
	}
}
