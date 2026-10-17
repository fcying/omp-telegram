package bridge

import (
	"encoding/json"
	"html"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"omp-telegram/internal/telegram"
)

func replyMetricsMessage(output int64, duration float64) message {
	return message{
		Role:     "assistant",
		ModelID:  "answer-model",
		Provider: "private-provider",
		Duration: &duration,
		Usage:    &messageUsage{Output: &output},
	}
}

func replyMetricsState(t *testing.T, raw string) statusState {
	t.Helper()
	var state statusState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestReplyMetricsWeightsCallsAndResetsBetweenReplies(t *testing.T) {
	var metrics replyMetrics
	metrics.observe(replyMetricsMessage(100, 1000))
	metrics.observe(replyMetricsMessage(100, 3000))
	if got := metrics.speedText(); got != "50 tok/s" {
		t.Fatalf("weighted speed = %q, want 50 tok/s", got)
	}
	tool := replyMetricsMessage(math.MaxInt64, math.NaN())
	tool.Role, tool.ModelID = "toolResult", "tool-model"
	metrics.observe(tool)
	metrics.observe(message{Role: "user"})
	if got := replyHeader(statusState{}, metrics); !strings.Contains(got, "answer-model") || strings.Contains(got, "tool-model") || !strings.Contains(got, "50 tok/s") {
		t.Fatalf("non-assistant changed reply metadata: %s", got)
	}
	metrics = replyMetrics{}
	if got := replyHeader(statusState{}, metrics); got != "" {
		t.Fatalf("new reply inherited metadata: %q", got)
	}
	metrics.observe(replyMetricsMessage(30, 2000))
	if got := metrics.speedText(); got != "15 tok/s" {
		t.Fatalf("new reply speed = %q, want 15 tok/s", got)
	}
}

func TestReplyMetricsRejectsIncompleteAndInvalidCalls(t *testing.T) {
	cases := []struct {
		name   string
		change func(*message)
	}{
		{"missing usage", func(m *message) { m.Usage = nil }},
		{"missing output", func(m *message) { m.Usage.Output = nil }},
		{"missing duration", func(m *message) { m.Duration = nil }},
		{"negative output", func(m *message) { *m.Usage.Output = -1 }},
		{"negative duration", func(m *message) { *m.Duration = -100 }},
		{"zero duration", func(m *message) { *m.Duration = 0 }},
		{"nan duration", func(m *message) { *m.Duration = math.NaN() }},
		{"positive infinity", func(m *message) { *m.Duration = math.Inf(1) }},
		{"negative infinity", func(m *message) { *m.Duration = math.Inf(-1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var metrics replyMetrics
			metrics.observe(replyMetricsMessage(100, 1000))
			bad := replyMetricsMessage(20, 1000)
			bad.ModelID = "last-model"
			tc.change(&bad)
			metrics.observe(bad)
			metrics.observe(replyMetricsMessage(100, 1000))
			if got := metrics.speedText(); got != "" {
				t.Fatalf("incomplete aggregate invented speed: %q", got)
			}
			metrics.observe(bad)
			header := replyHeader(statusState{}, metrics)
			if !strings.Contains(header, "last-model") || strings.Contains(header, "tok/s") || strings.Contains(header, "◫") {
				t.Fatalf("invalid metrics lost model fallback or invented fields: %s", header)
			}
		})
	}
}

func TestReplyMetricsThresholdZeroOutputAndOverflow(t *testing.T) {
	var metrics replyMetrics
	metrics.observe(replyMetricsMessage(0, 1000))
	if got := metrics.speedText(); got != "" {
		t.Fatalf("zero-output call displayed speed: %q", got)
	}
	metrics.observe(replyMetricsMessage(200, 1000))
	if got := metrics.speedText(); got != "100 tok/s" {
		t.Fatalf("valid zero-output duration was excluded: %q", got)
	}
	metrics = replyMetrics{}
	metrics.observe(replyMetricsMessage(50, 50))
	if got := metrics.speedText(); got != "" {
		t.Fatalf("sub-100ms aggregate displayed speed: %q", got)
	}
	metrics.observe(replyMetricsMessage(50, 50))
	if got := metrics.speedText(); got != "1000 tok/s" {
		t.Fatalf("100ms aggregate speed = %q", got)
	}
	metrics = replyMetrics{}
	metrics.observe(replyMetricsMessage(math.MaxInt64, 1000))
	metrics.observe(replyMetricsMessage(1, 1000))
	if got := metrics.speedText(); got != "" {
		t.Fatalf("token overflow displayed speed: %q", got)
	}
	metrics = replyMetrics{}
	metrics.observe(replyMetricsMessage(1, math.MaxFloat64))
	metrics.observe(replyMetricsMessage(1, math.MaxFloat64))
	if got := metrics.speedText(); got != "" {
		t.Fatalf("duration overflow displayed speed: %q", got)
	}
}

func TestReplyHeaderShowsLiteralModelAndEffectiveSettings(t *testing.T) {
	state := replyMetricsState(t, `{
		"model":{"id":"**answer**`+"`"+`<model>","provider":"private-provider"},
		"thinkingLevel":"**high**`+"`"+`<level>",
		"fastModeActive":false,"fastModeEnabled":true,
		"isStreaming":false,"isCompacting":false,
		"contextUsage":{"tokens":64000,"contextWindow":128000},
		"tokensPerSecond":99999
	}`)
	var metrics replyMetrics
	msg := replyMetricsMessage(120, 1000)
	msg.ModelID = state.Model.ID
	contextTokens := int64(64000)
	msg.Usage.ContextTokens = &contextTokens
	metrics.observe(msg)
	header := replyHeader(state, metrics)
	rendered := telegram.ConvertMarkdown(header)
	for _, want := range []string{
		"<code>" + html.EscapeString(state.Model.ID) + "</code>",
		"<code>" + html.EscapeString(state.ThinkingLevel) + "</code>",
		"fast off (setting: on)", "◫ 50%", "avg 120 tok/s",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("header missing %q: %s", want, rendered)
		}
	}
	if strings.Contains(header, "private-provider") || strings.Contains(header, "99999") || strings.Contains(header, "n/a") {
		t.Fatalf("header leaked provider, sampled speed, or placeholders: %s", header)
	}
}

func TestReplyHeaderDoesNotAttachOtherModelSettings(t *testing.T) {
	for _, raw := range []string{
		`{"model":{"id":"other-model","provider":"private-provider"},"thinkingLevel":"high","fastModeActive":true}`,
		`{"model":{"id":"answer-model","provider":"other-provider"},"thinkingLevel":"high","fastModeActive":true}`,
		`{"thinkingLevel":"high","fastModeActive":true}`,
	} {
		var metrics replyMetrics
		metrics.observe(replyMetricsMessage(10, 1000))
		header := replyHeader(replyMetricsState(t, raw), metrics)
		if !strings.Contains(header, "answer-model") || !strings.Contains(header, "10 tok/s") || strings.Contains(header, "◒") || strings.Contains(header, "⚡") || strings.Contains(header, "other-") {
			t.Fatalf("queried model settings crossed model identity: %s", header)
		}
	}
	state := replyMetricsState(t, `{"model":{"id":"answer-model"},"fastModeEnabled":true}`)
	if header := replyHeader(state, replyMetrics{}); strings.Contains(header, "⚡") || strings.Contains(header, "fast ") || strings.Contains(header, "n/a") || !strings.Contains(header, "answer-model") {
		t.Fatalf("configured-only fast state became actual activity: %s", header)
	}
	var metrics replyMetrics
	metrics.observe(replyMetricsMessage(10, 1000))
	metrics.observe(message{Role: "assistant"})
	if header := replyHeader(statusState{}, metrics); header != "" {
		t.Fatalf("missing latest identity reused earlier assistant: %s", header)
	}
}

func TestReplyHeaderUsesLastAfterOutputContextAndQueriedCapacity(t *testing.T) {
	for _, stateTokens := range []string{"9000", "9233"} {
		state := replyMetricsState(t, `{"model":{"id":"answer-model","provider":"private-provider"},"isStreaming":false,"isCompacting":false,"contextUsage":{"tokens":`+stateTokens+`,"contextWindow":10000,"percent":90}}`)
		msg := replyMetricsMessage(233, 1000)
		total := int64(9233)
		msg.Usage.TotalTokens = &total
		var metrics replyMetrics
		metrics.observe(msg)
		if got := replyHeader(state, metrics); !strings.Contains(got, "◫ 92%") {
			t.Fatalf("after-output usage was omitted or counted twice (state tokens %s): %s", stateTokens, got)
		}
	}
}

func TestReplyHeaderAccountsForContextUsageAndOrchestration(t *testing.T) {
	state := replyMetricsState(t, `{"model":{"id":"answer-model","provider":"private-provider"},"isStreaming":false,"isCompacting":false,"contextUsage":{"tokens":9000,"contextWindow":10000,"percent":90}}`)
	cases := []struct {
		name  string
		usage string
		want  string
	}{
		{"explicit context first", `{"contextTokens":9233,"totalTokens":9000,"output":233,"orchestration":{"input":100}}`, "◫ 92%"},
		{"explicit zero", `{"contextTokens":0,"totalTokens":9233}`, "◫ 0%"},
		{"total without components", `{"totalTokens":9233}`, "◫ 92%"},
		{"complete components", `{"input":8000,"output":233,"cacheRead":500,"cacheWrite":500}`, "◫ 92%"},
		{"zero total uses components", `{"totalTokens":0,"input":8000,"output":233,"cacheRead":500,"cacheWrite":500}`, "◫ 92%"},
		{"empty orchestration", `{"totalTokens":9233,"orchestration":{}}`, "◫ 92%"},
		{"orchestration partial defaults", `{"totalTokens":9233,"orchestration":{"output":233}}`, "◫ 90%"},
		{"orchestration all components", `{"totalTokens":9233,"orchestration":{"input":100,"output":20,"cacheRead":30}}`, "◫ 91%"},
		{"orchestration clamps at zero", `{"totalTokens":9233,"orchestration":{"input":10000}}`, "◫ 0%"},
		{"all components zero", `{"input":0,"output":0,"cacheRead":0,"cacheWrite":0}`, "◫ 0%"},
		{"missing usage", `null`, ""},
		{"missing input", `{"output":233,"cacheRead":0,"cacheWrite":0}`, ""},
		{"missing output", `{"input":9000,"cacheRead":0,"cacheWrite":0}`, ""},
		{"missing cache read", `{"input":9000,"output":233,"cacheWrite":0}`, ""},
		{"missing cache write", `{"input":9000,"output":233,"cacheRead":0}`, ""},
		{"negative explicit context", `{"contextTokens":-1,"totalTokens":9233}`, ""},
		{"negative total", `{"totalTokens":-1,"input":9000,"output":233,"cacheRead":0,"cacheWrite":0}`, ""},
		{"negative component", `{"input":9000,"output":233,"cacheRead":-1,"cacheWrite":0}`, ""},
		{"component overflow", `{"input":9223372036854775807,"output":1,"cacheRead":0,"cacheWrite":0}`, ""},
		{"negative orchestration", `{"totalTokens":9233,"orchestration":{"output":-1}}`, ""},
		{"orchestration overflow", `{"totalTokens":9233,"orchestration":{"input":9223372036854775807,"output":1}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := replyMetricsMessage(233, 1000)
			msg.Usage = nil
			if err := json.Unmarshal([]byte(tc.usage), &msg.Usage); err != nil {
				t.Fatal(err)
			}
			var metrics replyMetrics
			metrics.observe(msg)
			header := replyHeader(state, metrics)
			if tc.want == "" {
				if strings.Contains(header, "◫") {
					t.Fatalf("incomplete context fabricated: %s", header)
				}
			} else if !strings.Contains(header, tc.want) {
				t.Fatalf("context accounting missing %q: %s", tc.want, header)
			}
		})
	}
}

func TestReplyHeaderRequiresConfirmedIdleAndMatchingContextIdentity(t *testing.T) {
	msg := replyMetricsMessage(233, 1000)
	total := int64(9233)
	msg.Usage.TotalTokens = &total
	var metrics replyMetrics
	metrics.observe(msg)
	for _, flags := range []string{
		``, `"isStreaming":false,`, `"isCompacting":false,`,
		`"isStreaming":true,"isCompacting":false,`,
		`"isStreaming":false,"isCompacting":true,`,
	} {
		state := replyMetricsState(t, `{`+flags+`"model":{"id":"answer-model","provider":"private-provider"},"contextUsage":{"tokens":9000,"contextWindow":10000}}`)
		if got := replyHeader(state, metrics); strings.Contains(got, "◫") {
			t.Fatalf("unconfirmed final context displayed: %s", got)
		}
	}
	for _, model := range []string{`{}`, `{"id":"other-model","provider":"private-provider"}`, `{"id":"answer-model","provider":"other-provider"}`, `{"id":"answer-model"}`} {
		state := replyMetricsState(t, `{"model":`+model+`,"isStreaming":false,"isCompacting":false,"contextUsage":{"tokens":9000,"contextWindow":10000}}`)
		if got := replyHeader(state, metrics); strings.Contains(got, "◫") {
			t.Fatalf("context crossed model identity: %s", got)
		}
	}
	for _, usage := range []string{`null`, `{}`, `{"tokens":9233}`, `{"contextWindow":0}`, `{"contextWindow":-1}`} {
		state := replyMetricsState(t, `{"model":{"id":"answer-model","provider":"private-provider"},"isStreaming":false,"isCompacting":false,"contextUsage":`+usage+`}`)
		if got := replyHeader(state, metrics); strings.Contains(got, "◫") {
			t.Fatalf("missing context capacity fabricated: %s", got)
		}
	}
	if got := replyHeader(statusState{}, metrics); strings.Contains(got, "◫") || !strings.Contains(got, "233 tok/s") || !strings.Contains(got, "answer-model") {
		t.Fatalf("failed state query lost confirmed metrics or fabricated context: %s", got)
	}
}

func TestReplyHeaderContextIsIndependentOfSpeedAndDoesNotCarryForward(t *testing.T) {
	state := replyMetricsState(t, `{"model":{"id":"answer-model","provider":"private-provider"},"isStreaming":false,"isCompacting":false,"contextUsage":{"contextWindow":10000}}`)
	msg := replyMetricsMessage(233, 1000)
	total := int64(9233)
	msg.Usage.TotalTokens = &total
	msg.Duration = nil
	var metrics replyMetrics
	metrics.observe(msg)
	if got := replyHeader(state, metrics); !strings.Contains(got, "◫ 92%") || strings.Contains(got, "tok/s") {
		t.Fatalf("valid context was coupled to unknown speed: %s", got)
	}
	metrics.observe(message{Role: "toolResult"})
	if got := replyHeader(state, metrics); !strings.Contains(got, "◫") {
		t.Fatalf("non-assistant discarded last assistant context: %s", got)
	}
	metrics.observe(replyMetricsMessage(233, 1000))
	if got := replyHeader(state, metrics); strings.Contains(got, "◫") {
		t.Fatalf("missing latest usage inherited earlier context: %s", got)
	}
	for _, missingIdentity := range []message{
		{Role: "assistant", Usage: msg.Usage},
		{Role: "assistant", ModelID: msg.ModelID, Usage: msg.Usage},
		{Role: "assistant", Provider: msg.Provider, Usage: msg.Usage},
	} {
		metrics.observe(missingIdentity)
		if got := replyHeader(state, metrics); strings.Contains(got, "◫") {
			t.Fatalf("missing assistant identity fabricated matching context: %s", got)
		}
	}
	metrics = replyMetrics{}
	if got := replyHeader(state, metrics); strings.Contains(got, "◫") {
		t.Fatalf("new reply reused state prompt usage: %s", got)
	}
}

var replyMetricsHTMLTags = regexp.MustCompile(`<[^>]*>`)

func replyMetricsPlain(text string) string {
	return html.UnescapeString(replyMetricsHTMLTags.ReplaceAllString(telegram.ConvertMarkdown(text), ""))
}

func assertReplyMetricsBudget(t *testing.T, parts []string, header string) {
	t.Helper()
	if len(parts) == 0 || len(parts) > maxFinalReplyMessages {
		t.Fatalf("reply message count = %d", len(parts))
	}
	bytes := 0
	for i, part := range parts {
		bytes += len(part)
		if !utf8.ValidString(part) || utf16Length(replyMetricsPlain(part)) > telegram.MaxMessageUTF16 {
			t.Fatalf("invalid Unicode or oversized Telegram page %d", i)
		}
		if i > 0 && strings.Contains(part, header) {
			t.Fatalf("header repeated on page %d", i)
		}
	}
	if !strings.HasPrefix(parts[0], header) || bytes > maxFinalReplyBytes {
		t.Fatalf("missing first-page header or byte overflow: bytes=%d", bytes)
	}
}

func TestReplyTaskRepliesPreservesMarkdownAndUnicode(t *testing.T) {
	header := "◒ `answer-model` | avg 50 tok/s"
	body := "## 中文 😀\n\n**bold** and `literal <&>`\n\n```go\nfmt.Println(\"😀\")\n```\n\n| A | B |\n|---|---|\n| 一 | 二 |"
	parts := replyTaskReplies(body, header, false)
	assertReplyMetricsBudget(t, parts, header)
	if len(parts) != 1 || telegram.ConvertMarkdown(parts[0]) != telegram.ConvertMarkdown(header+"\n\n"+body) {
		t.Fatalf("small Markdown reply changed: %q", parts)
	}
	body = strings.Repeat("😀", 5000)
	parts = replyTaskReplies(body, header, false)
	assertReplyMetricsBudget(t, parts, header)
	visible := strings.Join(parts, "")
	if strings.Count(visible, "😀") != 5000 || strings.Contains(visible, replyTruncatedNotice) {
		t.Fatal("Unicode pagination lost or duplicated body text")
	}
	if got := replyTaskReplies("", header, false); !reflect.DeepEqual(got, []string{header}) {
		t.Fatalf("empty body reply = %q", got)
	}
}

func TestReplyTaskRepliesReservesFirstPageForBody(t *testing.T) {
	header := "◒ `answer-model` | avg 50 tok/s"
	body := strings.Repeat("body line\n", 600)
	parts := replyTaskReplies(body, header, false)
	assertReplyMetricsBudget(t, parts, header)
	if !strings.Contains(parts[0], "body line") {
		t.Fatal("long reply sent its metadata as a separate empty-body message")
	}
	if strings.Count(strings.Join(parts, "\n"), "body line") != 600 {
		t.Fatal("reserving header space lost or duplicated body lines")
	}
}

func TestReplyTaskRepliesAccountsForHeaderInPageBudget(t *testing.T) {
	header := "◒ `answer-model` | avg 50 tok/s"
	body := strings.Repeat("x", telegram.MaxMessageUTF16*maxFinalReplyMessages)
	parts := replyTaskReplies(body, header, false)
	assertReplyMetricsBudget(t, parts, header)
	if parts[len(parts)-1] != replyTruncatedNotice || strings.Count(strings.Join(parts, "\n"), replyTruncatedNotice) != 1 {
		t.Fatal("header-induced overflow lost or duplicated truncation notice")
	}
	if strings.Count(strings.Join(parts, ""), "x") >= len(body) {
		t.Fatal("header did not consume the existing message budget")
	}
}

func TestReplyTaskRepliesPreservesExplicitTruncation(t *testing.T) {
	header := "◒ `answer-model`"
	for _, body := range []string{"answer 😀", strings.Repeat("answer 😀 ", maxFinalReplyBytes/8)} {
		parts := replyTaskReplies(body, header, true)
		assertReplyMetricsBudget(t, parts, header)
		if parts[len(parts)-1] != replyTruncatedNotice || strings.Count(strings.Join(parts, "\n"), replyTruncatedNotice) != 1 {
			t.Fatal("explicit truncation notice lost or repeated")
		}
	}
	parts := replyTaskReplies("answer 😀", header, true)
	if !strings.Contains(parts[0], "answer 😀") {
		t.Fatal("small explicitly truncated body disappeared")
	}
}

func TestReplyTaskRepliesCountsMarkdownBytes(t *testing.T) {
	header := "◒ `answer-model`"
	// Long code delimiters consume bytes without consuming rendered UTF-16.
	// This reaches the byte budget before the Telegram message count budget.
	marker := strings.Repeat("`", maxFinalReplyBytes/4)
	body := marker + "\nbody\n" + marker + "\n\n" + marker + "\ntail\n" + marker
	parts := replyTaskReplies(body, header, false)
	assertReplyMetricsBudget(t, parts, header)
	if parts[len(parts)-1] != replyTruncatedNotice || !strings.Contains(strings.Join(parts, ""), "body") {
		t.Fatal("Markdown byte overflow silently lost body or truncation notice")
	}
}

func TestReplyTaskRepliesKeepsHeaderWhenFenceRepairExceedsByteBudget(t *testing.T) {
	header := "◒ `answer-model`"
	marker := strings.Repeat("`", maxFinalReplyBytes/2+1)
	body := marker + "\n" + strings.Repeat("x", telegram.MaxMessageUTF16+1)
	parts := replyTaskReplies(body, header, false)
	assertReplyMetricsBudget(t, parts, header)
	if parts[len(parts)-1] != replyTruncatedNotice {
		t.Fatal("oversized repaired fence lost its explicit truncation notice")
	}
}
