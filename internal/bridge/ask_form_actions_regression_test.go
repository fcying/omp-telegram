package bridge

import (
	"strings"
	"testing"
)

const askFormRegressionRadio = `<fieldset><legend>Choice</legend>
	<label><input type="radio" name="choice" value="first" checked>First choice</label>
	<label><input type="radio" name="choice" value="second">Second choice</label>
</fieldset>`

func TestAskFormSingleChoiceReplacesDefault(t *testing.T) {
	for _, tc := range []struct {
		name, field string
	}{
		{"select", `<label>Choice<select name="choice">
			<option value="first">First choice</option>
			<option value="second">Second choice</option>
		</select></label>`},
		{"radio", askFormRegressionRadio},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			askTestEvent(t, w, map[string]any{"id": "replace-default", "method": "form", "html": "<form>" + tc.field + "</form>"})
			askTestButton(t, f, "Use default")
			messageID := w.ask.messageID
			askTestClick(t, w, f, "Second choice")
			askFormRegressionSubmitted(t, w, f, messageID, "replace-default", "choice", "second", "<code>2. Second choice</code>")
		})
	}
}

func TestAskFormSingleChoiceOtherReplacesDefaultOnlyOnSubmit(t *testing.T) {
	for _, tc := range []struct {
		name, wantValue, wantFooter string
	}{
		{"submit", "Custom choice", "<code>Custom answer: Custom choice</code>"},
		{"back", "first", "<code>1. First choice</code>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			askTestEvent(t, w, map[string]any{"id": "other-default", "method": "form", "html": "<form>" + askFormRegressionRadio + "</form>"})
			askTestButton(t, f, "Use default")
			messageID := w.ask.messageID
			askTestClick(t, w, f, "Custom answer")
			prompt := askTestPrompt(t, f)
			if tc.name == "back" {
				askTestClick(t, w, f, "Back to options")
				if got := askTestState(t, w); len(got) != 0 {
					t.Fatalf("returning to options submitted a response: %+v", got)
				}
				askTestClick(t, w, f, "Use default")
			} else {
				askTestReply(t, w, 200, 7, 11, "Custom choice", prompt)
			}
			askFormRegressionSubmitted(t, w, f, messageID, "other-default", "choice", tc.wantValue, tc.wantFooter)
		})
	}
}

func TestAskFormTextEmptyReceiptDoesNotShowDefault(t *testing.T) {
	for _, tc := range []struct {
		name, button, wantValue, wantFooter string
	}{
		{"empty", "Submit empty", "", "Empty string (0 characters)"},
		{"default", "Use default", "Original draft", "<code>Original draft</code>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			askTestEvent(t, w, map[string]any{
				"id": "notes", "method": "form",
				"html": `<form><label>Notes<textarea name="notes">Original draft</textarea></label></form>`,
			})
			messageID := w.ask.messageID
			askTestClick(t, w, f, tc.button)
			askFormRegressionSubmitted(t, w, f, messageID, "notes", "notes", tc.wantValue, tc.wantFooter)
		})
	}
}

func askFormRegressionSubmitted(t *testing.T, w *worker, f *fakeHTTP, messageID int64, id, field, value, footer string) {
	t.Helper()
	got := askTestState(t, w)
	if len(got) != 1 || got[0].ID != id || got[0].Cancelled || len(got[0].Data) != 1 || got[0].Data[field] != value {
		t.Fatalf("native responses = %+v, want one submission of %s=%q for %q", got, field, value, id)
	}
	receipt := askTestReceipt(t, w, f, messageID)
	_, submitted, ok := strings.Cut(receipt, "<b>Submitted</b>")
	if !ok || strings.TrimSpace(submitted) != footer {
		t.Fatalf("receipt = %q, want Submitted footer %q", receipt, footer)
	}
}
