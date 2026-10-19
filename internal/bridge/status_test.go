package bridge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/i18n"
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
	state.ConfiguredThinking = "auto"
	text := formatStatus("en", state, "/home/test/project/foo", "saved-id", "/home/test", 2)
	fields := statusFields(text)
	for key, want := range map[string]string{
		"Workspace": "~/project/foo", "Session": "Bugfix HAL", "Model": "openai/gpt-5.6",
		"Fast": "off", "Context": "64% (82k / 128k)",
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

func TestStatusUsesConfiguredThinkingInsteadOfEffectiveLevel(t *testing.T) {
	for _, scenario := range []struct{ configured, want string }{
		{"auto", "auto"}, {"low", "low"}, {"", "n/a"},
	} {
		for _, effective := range []string{"medium", "high"} {
			t.Run(scenario.want+"/"+effective, func(t *testing.T) {
				var state statusState
				if err := json.Unmarshal([]byte(`{"thinkingLevel":"`+effective+`"}`), &state); err != nil {
					t.Fatal(err)
				}
				state.ConfiguredThinking = scenario.configured
				if got := statusFields(formatStatus("en", state, "/work", "session", "", 0))["Thinking"]; got != scenario.want {
					t.Fatalf("configured %q with effective %q displayed %q", scenario.configured, effective, got)
				}
			})
		}
	}
}

func TestNativeStatusCannotSupplyConfiguredThinking(t *testing.T) {
	var state statusState
	if err := json.Unmarshal([]byte(`{
		"model":{"id":"answer-model"},"thinkingLevel":"high",
		"configuredThinking":"auto","ConfiguredThinking":"max","configured":"low"
	}`), &state); err != nil {
		t.Fatal(err)
	}
	if state.ConfiguredThinking != "" {
		t.Fatal("native JSON supplied the actor-owned thinking projection")
	}
	if got := statusFields(formatStatus("en", state, "/work", "session", "", 0))["Thinking"]; got != "n/a" {
		t.Fatalf("native state supplied configured thinking to detailed status: %q", got)
	}
	for _, text := range []string{compactProgressStatus("en", state), replyHeader("en", state, replyMetrics{})} {
		if !strings.Contains(text, "answer-model") || strings.ContainsAny(text, "⦸○◔◑◒◕◉⟳") || strings.Contains(text, "high") {
			t.Fatalf("unknown configured thinking inferred a native level: %s", text)
		}
	}
}

func TestStatusRendersNativeContextPercentage(t *testing.T) {
	var state statusState
	if err := json.Unmarshal([]byte(`{"contextUsage":{"tokens":67300,"contextWindow":272000,"percent":24.74}}`), &state); err != nil {
		t.Fatal(err)
	}
	if got := statusFields(formatStatus("en", state, "/work", "id", "", 0))["Context"]; got != "25% (67.3k / 272k)" {
		t.Fatalf("native context percentage rendered as %q, want 25%% (67.3k / 272k)", got)
	}
}

func TestStatusDistinguishesMissingMetricsFromZero(t *testing.T) {
	missing := statusFields(formatStatus("en", statusState{}, "/work", "saved-id", "", 0))
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
	fields := statusFields(formatStatus("en", zero, "/work", "saved-id", "", 0))
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
		if got := statusFields(formatStatus("en", state, "/work", "id", "", 0))["Fast"]; got != scenario.want {
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
		if got := statusFields(formatStatus("en", statusState{}, scenario.path, "id", "/home/test", 0))["Workspace"]; got != scenario.want {
			t.Errorf("workspace %q rendered as %q, want %q", scenario.path, got, scenario.want)
		}
	}
}

func TestStatusCommandPublishesDetailedReply(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_CONFIGURED_THINKING", "auto")
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	client := w.client
	command("/status")
	if w.client != client || len(w.b.slots) != 1 {
		t.Fatal("status released an already-connected runtime")
	}
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil {
		t.Fatal(err)
	}
	fields := statusFields(text)
	if fields["Model"] != "fixture/safe" || fields["Thinking"] != "auto" || fields["Queued"] != "0" {
		t.Fatalf("status command omitted configured thinking or native model state: %v", fields)
	}
	if strings.Contains(text, "SECRET") || strings.Contains(text, "PRIVATE") {
		t.Fatal("status command leaked native secrets")
	}
}

func TestClosedStatusDoesNotRestartRuntime(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	command("/close")
	before := w.binding
	command("/status")
	if w.client != nil || w.runtime != runtimeReleased || len(w.b.slots) != 0 || w.binding != before {
		t.Fatal("status restarted or changed a closed session")
	}
}

func TestChineseStatusPreservesNativeIdentityAndPaths(t *testing.T) {
	state := statusState{}
	state.Model.Provider = "Workspace"
	state.Model.ID = "on"
	state.SessionName = "Session ID: on 中文"
	text := formatStatus("zh", state, "/Workspace/on", "session-id", "", 3)
	for _, native := range []string{"/Workspace/on", "Workspace/on", "Session ID: on 中文", "session-"} {
		if !strings.Contains(text, native) {
			t.Fatalf("localized status changed native data %q: %q", native, text)
		}
	}
	if !strings.Contains(text, "工作区: ") || !strings.Contains(text, "排队: 3") || strings.Contains(text, "%!s") {
		t.Fatalf("status labels or formatted arguments were not localized: %q", text)
	}
}

func TestChineseStatusLocalizesMissingValuesWithoutTranslatingNativeSentinels(t *testing.T) {
	missing := formatRuntimeStatus("zh", statusState{}, "/work", "", "", 0, -1, false)
	if strings.Contains(missing, "n/a") || !strings.Contains(missing, "空闲: 不可用") {
		t.Fatalf("Bridge-owned missing values were not localized: %q", missing)
	}
	state := statusState{SessionName: "n/a", ConfiguredThinking: "n/a"}
	state.Model.Provider = "on"
	state.Model.ID = "off"
	text := formatStatus("zh", state, "/n/a", "n/a", "", 0)
	for _, native := range []string{"/n/a", "on/off", "会话: n/a", "思考: n/a"} {
		if !strings.Contains(text, native) {
			t.Fatalf("native value %q was translated: %q", native, text)
		}
	}
}

func TestTypedStatusValuesRenderAtTheirSource(t *testing.T) {
	active, inactive := true, false
	for _, scenario := range []struct {
		language         i18n.Language
		yes, no, missing string
	}{
		{i18n.English, "yes", "no", "n/a"},
		{i18n.Chinese, "是", "否", "不可用"},
	} {
		for _, item := range []struct {
			value *bool
			want  string
		}{
			{&active, scenario.yes}, {&inactive, scenario.no}, {nil, scenario.missing},
		} {
			if got := statusBoolText(scenario.language, item.value, i18n.Msg.Yes, i18n.Msg.No); got != item.want {
				t.Fatalf("boolean in %s = %q, want %q", scenario.language, got, item.want)
			}
		}
		state := statusState{}
		for _, got := range []string{state.contextText(scenario.language), state.speedText(scenario.language), statusIdle(scenario.language, -time.Second)} {
			if got != scenario.missing {
				t.Fatalf("missing status in %s = %q, want %q", scenario.language, got, scenario.missing)
			}
		}
		if got := statusIdle(scenario.language, 90*time.Second); got != "2m0s" {
			t.Fatalf("valid idle duration in %s = %q", scenario.language, got)
		}
	}
}
