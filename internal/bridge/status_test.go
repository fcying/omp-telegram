package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

func statusFields(text string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		if key, value, ok := strings.Cut(line, ": "); ok {
			fields[key] = value
		}
	}
	return fields
}

func TestStatusDisplaysWhitelistedNativeMetrics(t *testing.T) {
	var state statusState
	if err := json.Unmarshal([]byte(`{
		"sessionId":"abcdef12-3456-7890", "sessionName":"Bugfix HAL",
		"model":{"provider":"openai","id":"gpt-5.6","headers":{"Authorization":"SECRET"}},
		"systemPrompt":"PRIVATE", "thinkingLevel":"high",
		"fastModeEnabled":false,"fastModeActive":false,
		"contextUsage":{"tokens":82000,"contextWindow":128000,"percent":64.0625},
		"isStreaming":true,"isCompacting":false,"queuedMessageCount":99,"tokensPerSecond":87
	}`), &state); err != nil {
		t.Fatal(err)
	}
	text := formatStatus(state, "/home/test/project/foo", "saved-id", "/home/test", 2)
	fields := statusFields(text)
	for key, want := range map[string]string{
		"Workspace": "~/project/foo", "Session": "Bugfix HAL", "Model": "openai/gpt-5.6",
		"Thinking": "high", "Fast": "off", "Context": "64% (82k / 128k)",
		"Running": "yes", "Compacting": "no", "Queued": "2", "Speed": "87 tok/s",
	} {
		if fields[key] != want {
			t.Errorf("%s=%q, want %q", key, fields[key], want)
		}
	}
	if !strings.HasPrefix(fields["Session ID"], "abcdef12") || strings.Contains(text, "saved-id") {
		t.Fatal("named session did not retain the actual session identity")
	}
	if strings.Contains(text, "SECRET") || strings.Contains(text, "PRIVATE") || strings.Contains(text, "Authorization") {
		t.Fatal("status leaked non-whitelisted native state")
	}
}

func TestStatusRendersNativeContextPercentage(t *testing.T) {
	var state statusState
	if err := json.Unmarshal([]byte(`{"contextUsage":{"tokens":67300,"contextWindow":272000,"percent":24.74}}`), &state); err != nil {
		t.Fatal(err)
	}
	if got := statusFields(formatStatus(state, "/work", "id", "", 0))["Context"]; got != "25% (67.3k / 272k)" {
		t.Fatalf("native context percentage rendered as %q, want 25%% (67.3k / 272k)", got)
	}
}

func TestStatusDistinguishesMissingMetricsFromZero(t *testing.T) {
	missing := statusFields(formatStatus(statusState{}, "/work", "saved-id", "", 0))
	for _, key := range []string{"Thinking", "Fast", "Context", "Speed", "Running", "Compacting"} {
		if missing[key] != "n/a" {
			t.Errorf("absent %s was reported as %q", key, missing[key])
		}
	}
	if missing["Session"] != "saved-id" {
		t.Fatal("unnamed session lost fallback identity")
	}
	var zero statusState
	if err := json.Unmarshal([]byte(`{"fastModeActive":false,"isStreaming":false,"isCompacting":false,"tokensPerSecond":0,"contextUsage":{"tokens":0,"contextWindow":128000}}`), &zero); err != nil {
		t.Fatal(err)
	}
	fields := statusFields(formatStatus(zero, "/work", "saved-id", "", 0))
	if fields["Context"] != "0% (0 / 128k)" || fields["Speed"] != "0 tok/s" || fields["Fast"] != "off" || fields["Running"] != "no" {
		t.Fatalf("valid zero metrics became missing: %v", fields)
	}
}

func TestStatusKeepsFastSettingSeparateFromActualState(t *testing.T) {
	for _, scenario := range []struct{ raw, want string }{
		{`{"fastModeEnabled":true,"fastModeActive":false}`, "off (setting: on)"},
		{`{"fastModeEnabled":false,"fastModeActive":true}`, "on (setting: off)"},
		{`{"fastModeEnabled":true}`, "n/a (setting: on)"},
	} {
		var state statusState
		if err := json.Unmarshal([]byte(scenario.raw), &state); err != nil {
			t.Fatal(err)
		}
		if got := statusFields(formatStatus(state, "/work", "id", "", 0))["Fast"]; got != scenario.want {
			t.Fatalf("fast state=%q, want=%q", got, scenario.want)
		}
	}
}

func TestStatusHomeAbbreviationRespectsDirectoryBoundaries(t *testing.T) {
	for _, scenario := range []struct{ path, want string }{
		{"/home/test", "~"},
		{"/home/test/project  files", "~/project  files"},
		{"/home/test-other/project", "/home/test-other/project"},
		{"/var/project", "/var/project"},
	} {
		if got := statusFields(formatStatus(statusState{}, scenario.path, "id", "/home/test", 0))["Workspace"]; got != scenario.want {
			t.Errorf("workspace %q rendered as %q, want %q", scenario.path, got, scenario.want)
		}
	}
}

func TestStatusCommandPublishesDetailedReply(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	command("/status")
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil {
		t.Fatal(err)
	}
	fields := statusFields(text)
	if fields["Model"] != "fixture/safe" || fields["Thinking"] != "medium" || fields["Queued"] != "0" {
		t.Fatalf("status command omitted native model state: %v", fields)
	}
	if strings.Contains(text, "SECRET") || strings.Contains(text, "PRIVATE") {
		t.Fatal("status command leaked native secrets")
	}
}
