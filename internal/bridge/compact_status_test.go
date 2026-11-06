package bridge

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"omp-telegram/internal/config"
	"omp-telegram/internal/i18n"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func TestRenderCompactFieldsPreservesSelectedOrderAndSubset(t *testing.T) {
	values := compactStatusFields{
		status: "running", model: "`model`", thinking: "◒", fast: "⚡",
		context: "25% (128k)", speed: "10 t/s", quota: "quota 80%", git: "`main`",
	}
	selected := []string{"git", "quota", "speed", "context", "fast", "model", "thinking", "status"}
	header := renderCompactFields(selected, values)
	previous := -1
	for _, value := range []string{values.git, values.quota, values.speed, values.context, values.fast, values.model, values.thinking, values.status} {
		position := strings.Index(header, value)
		if position <= previous || strings.Count(header, value) != 1 {
			t.Fatalf("selected field %q was missing, repeated, or out of order: %q", value, header)
		}
		previous = position
	}
	for _, scenario := range []struct {
		field string
		want  string
	}{
		{"status", values.status}, {"model", values.model}, {"thinking", values.thinking}, {"fast", values.fast},
		{"context", values.context}, {"speed", values.speed}, {"quota", values.quota}, {"git", values.git},
	} {
		t.Run(scenario.field, func(t *testing.T) {
			if got := renderCompactFields([]string{scenario.field}, values); got != scenario.want {
				t.Fatalf("individual field included unselected values: %q", got)
			}
		})
	}
	if got := renderCompactFields([]string{}, values); got != "" {
		t.Fatalf("disabled header retained fields: %q", got)
	}
}

func TestRenderCompactFieldsSkipsUnavailableValuesWithoutSeparators(t *testing.T) {
	values := compactStatusFields{model: "`model`"}
	if got := renderCompactFields([]string{"quota", "unknown", "thinking", "model", "context", "git"}, values); got != values.model {
		t.Fatalf("missing values left labels or separators: %q", got)
	}
	if got := renderCompactFields((config.StatusBar{}).Fields("final"), compactStatusFields{}); got != "" {
		t.Fatalf("unavailable fields fabricated a header: %q", got)
	}
}

func TestCompactFieldsOmitsUnknownValuesInEveryLanguage(t *testing.T) {
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		for _, raw := range []string{
			`{}`,
			`{"model":{"id":"answer-model"},"fastModeEnabled":true}`,
			`{"model":{"id":"answer-model"},"contextUsage":{"tokens":1100,"contextWindow":0}}`,
		} {
			state := replyMetricsState(t, raw)
			state.ConfiguredThinking = "n/a"
			values := compactFields(language, state, nil, false, true)
			if values.thinking != "" || values.fast != "" || values.context != "" || values.speed != "" || values.quota != "" || values.git != "" {
				t.Fatalf("unknown native values became fields in %s: %+v", language, values)
			}
			header := renderCompactFields((config.StatusBar{}).Fields("final"), values)
			for _, placeholder := range []string{"ctx ?", "quota ?", "n/a", "不可用"} {
				if strings.Contains(header, placeholder) {
					t.Fatalf("unknown field emitted %q in %s: %q", placeholder, language, header)
				}
			}
		}
	}
}

func TestCompactFieldsSeparatesIdentitySettings(t *testing.T) {
	state := replyMetricsState(t, `{"model":{"id":"answer-model","provider":"private-provider"},"fastModeActive":true,"fastModeEnabled":true}`)
	state.ConfiguredThinking = "high"
	values := compactFields("en", state, nil, false, false)
	for _, scenario := range []struct {
		field string
		want  string
	}{
		{"model", "`answer-model`"}, {"thinking", "◒"}, {"fast", "⚡"},
	} {
		if got := renderCompactFields([]string{scenario.field}, values); got != scenario.want {
			t.Fatalf("field %s was not independent: %q", scenario.field, got)
		}
	}
	var metrics replyMetrics
	msg := replyMetricsMessage(10, 1000)
	msg.ModelID = "different-model"
	metrics.observe(msg)
	values = compactFields("en", state, &metrics, true, true)
	if values.model != "`different-model`" || values.thinking != "" || values.fast != "" {
		t.Fatalf("sampled settings crossed reply identity: %+v", values)
	}
}

func TestCompactFieldsCompletedContextRequiresReadAndMatchingCapacity(t *testing.T) {
	state := replyMetricsState(t, `{"model":{"id":"answer-model","provider":"private-provider"},"isStreaming":true,"isCompacting":false,"contextUsage":{"tokens":9000,"contextWindow":10000}}`)
	var metrics replyMetrics
	msg := replyMetricsMessage(233, 1000)
	total := int64(9233)
	msg.Usage.TotalTokens = &total
	metrics.observe(msg)
	if got := compactFields("en", state, &metrics, false, true).context; got != "" {
		t.Fatalf("live streaming context was presented as confirmed: %q", got)
	}
	if got := compactFields("en", state, &metrics, true, true).context; got != "92% (10k)" {
		t.Fatalf("completed message lost output-inclusive context: %q", got)
	}
	if got := compactFields("en", state, &metrics, true, false).context; got != "" {
		t.Fatalf("completed context bypassed the read fence: %q", got)
	}
	state.Model.Provider = "other-provider"
	if got := compactFields("en", state, &metrics, true, true).context; got != "" {
		t.Fatalf("completed context crossed provider identity: %q", got)
	}
	state.Model.Provider = msg.Provider
	*state.ContextUsage.ContextWindow = 0
	if got := compactFields("en", state, &metrics, true, true).context; got != "" {
		t.Fatalf("completed context invented capacity: %q", got)
	}
}

func TestCompactFieldsPreservesNativeNumericValidation(t *testing.T) {
	for _, number := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		state := replyMetricsState(t, `{"contextUsage":{"tokens":2500,"contextWindow":10000}}`)
		state.TokensPerSecond = &number
		state.ContextUsage.Percent = &number
		values := compactFields("en", state, nil, false, true)
		if values.speed != "" || values.context != "25% (10k)" {
			t.Fatalf("invalid number bypassed validation or discarded valid token context: %+v", values)
		}
	}
	for _, raw := range []string{
		`{"contextUsage":{"tokens":-1,"contextWindow":10000}}`,
		`{"contextUsage":{"tokens":2500,"contextWindow":-1}}`,
		`{"contextUsage":{"contextWindow":10000}}`,
		`{"contextUsage":{"tokens":2500}}`,
	} {
		if got := compactFields("en", replyMetricsState(t, raw), nil, false, true).context; got != "" {
			t.Fatalf("invalid native context was shown: %q", got)
		}
	}
	state := replyMetricsState(t, `{"tokensPerSecond":0,"contextUsage":{"tokens":0,"contextWindow":10000}}`)
	values := compactFields("en", state, nil, false, true)
	if values.speed != "0 t/s" || values.context != "0% (10k)" {
		t.Fatalf("valid zero metrics were hidden: %+v", values)
	}
	if got := compactFields("en", state, nil, false, false).context; got != "" {
		t.Fatalf("native context appeared before a completed read: %q", got)
	}
}

func TestCompactHeaderBudgetNeverCutsLiteralFields(t *testing.T) {
	for _, width := range []int{498, 500, 504, 510, 512, 600} {
		values := compactStatusFields{model: progressToolLiteral(strings.Repeat("x", width)), git: "`main*`", status: assistantCompletedIcon}
		header := renderCompactFieldsLimit([]string{"model", "git", "status"}, values, assistantHeaderUnits)
		if utf16Length(header) > assistantHeaderUnits {
			t.Fatalf("bounded header exceeded the reserved space: %d", utf16Length(header))
		}
		if strings.Contains(header, "main") && !strings.Contains(header, values.git) {
			t.Fatalf("header clipping presented a dirty branch without its marker/fence: %q", header)
		}
		if strings.Count(header, "`")%2 != 0 || strings.HasSuffix(header, compactHeaderSeparator) {
			t.Fatalf("header clipping broke a literal or left an empty field: %q", header)
		}
	}
	values := compactStatusFields{git: "`main*`", status: assistantCompletedIcon, model: "`overflow`"}
	limit := utf16Length(values.git + compactHeaderSeparator + values.status)
	if got := renderCompactFieldsLimit([]string{"git", "status", "model"}, values, limit); got != values.git+compactHeaderSeparator+values.status {
		t.Fatalf("exact budget lost a separator or accepted an overflowing field: %q", got)
	}
}

func TestCompactLeadingLiteralPreservesTelegramBodyAndFieldOrder(t *testing.T) {
	const body = "**bold** answer with `body code`"
	for _, first := range []string{"git", "model"} {
		for _, width := range []int{1, 2, 3} {
			t.Run(fmt.Sprintf("%s/%d", first, width), func(t *testing.T) {
				ticks := strings.Repeat("`", width)
				branch := "feature/" + ticks + "name*"
				model := "model/" + ticks + "name*"
				state := replyMetricsState(t, `{"model":{"id":"model"}}`)
				state.Model.ID = model
				state.ConfiguredThinking = "high"
				values := compactFields("en", state, nil, false, false)
				values.git = progressToolLiteral(branch)
				values.status = "unselected status"
				selected := []string{"unknown", "quota", first}
				wantHeader := "<code>" + branch + "</code> | <code>" + model + "</code> | ◒"
				if first == "git" {
					selected = append(selected, "model", "thinking")
				} else {
					selected = append(selected, "git", "thinking")
					wantHeader = "<code>" + model + "</code> | <code>" + branch + "</code> | ◒"
				}
				header := renderCompactFieldsLimit(selected, values, assistantHeaderUnits)
				output := store.Output{Header: header, Text: body}
				want := wantHeader + "\n\n<b>bold</b> answer with <code>body code</code>"
				if got := telegram.ConvertMarkdown(assistantOutputText(output)); got != want {
					t.Fatalf("leading literal changed field visibility, order, or body entities:\ngot:  %s\nwant: %s", got, want)
				}
				if got := renderCompactFieldsLimit([]string{}, values, assistantHeaderUnits); got != "" {
					t.Fatalf("disabled fields retained a literal safety prefix: %q", got)
				}
			})
		}
	}
}

func TestCompactLeadingLiteralIncludesEncodingInHeaderBudget(t *testing.T) {
	const body = "**bold** answer with `body code`"
	for _, first := range []string{"git", "model"} {
		for _, width := range []int{1, 2, 3} {
			for _, units := range []int{assistantHeaderUnits - 1, assistantHeaderUnits, assistantHeaderUnits + 1} {
				t.Run(fmt.Sprintf("%s/%d/%d", first, width, units), func(t *testing.T) {
					overhead := 0
					if width >= 2 {
						overhead = 4
					}
					base := "name/" + strings.Repeat("`", width) + "😀"
					padding := units - overhead - utf16Length(progressToolLiteral(base+"*"))
					literal := base + strings.Repeat("x", padding) + "*"
					values := compactStatusFields{status: assistantCompletedIcon}
					if first == "git" {
						values.git = progressToolLiteral(literal)
					} else {
						values.model = progressToolLiteral(literal)
					}
					header := renderCompactFieldsLimit([]string{"unknown", first, "status"}, values, assistantHeaderUnits)
					wantHeader := "<code>" + literal + "</code>"
					if units > assistantHeaderUnits {
						if header != assistantCompletedIcon {
							t.Fatalf("overflowing encoded literal was not omitted intact: %q", header)
						}
						wantHeader = assistantCompletedIcon
					} else if got := utf16Length(header); got != units {
						t.Fatalf("encoded header used %d UTF-16 units, want %d", got, units)
					}
					output := store.Output{Header: header, Text: body}
					want := wantHeader + "\n\n<b>bold</b> answer with <code>body code</code>"
					if got := telegram.ConvertMarkdown(assistantOutputText(output)); got != want {
						t.Fatalf("budgeted literal changed visible text or body entities:\ngot:  %s\nwant: %s", got, want)
					}
				})
			}
		}
	}
}
