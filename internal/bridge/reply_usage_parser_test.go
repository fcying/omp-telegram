package bridge

import (
	"strings"
	"testing"
	"time"
)

var replyUsageObservedAt = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// Layouts follow upstream usage-report.ts, claude.ts, openai-codex.ts, and
// active-oauth-account.ts, including the native formatter's provider headings.
func replyUsageReport(body string) string {
	return "```\nUsage (2s ago)\n\n" + body + "\n```"
}

func replyUsageRow(label, account, amount string, active bool) string {
	marker := ""
	if active {
		marker = "  ← in use by this session"
	}
	return "- " + label + "\n  " + account + ": " + amount + marker + "\n  [███████░░░░░░░░░░░░░░░░░] 30%"
}

func TestReplyUsageSampleNativeProviderLayouts(t *testing.T) {
	text := replyUsageReport(`Anthropic
  Models with usage data
    anthropic/claude-sonnet-4-6
- private@example.com (Private Team): 2 saved rate-limit resets — available — /usage reset to spend
  soonest expires in 2d (2026-11-06)
` + replyUsageRow("Claude 5 Hour", "other@example.com (Other Team)", "99.00% used (1.0% left)", false) + "\n" +
		replyUsageRow("Claude 7 Day", "other@example.com (Other Team)", "98.00% used (2.0% left)", false) + "\n" +
		replyUsageRow("Claude 5 Hour", "private@example.com (Private Team)", "30.00% used (70.0% left)", true) + "\n" +
		replyUsageRow("Claude 7 Day", "private@example.com (Private Team)", "65.00% used (35.0% left)", true) + "\n" +
		replyUsageRow("Claude 7 Day (Opus)", "private@example.com (Private Team)", "100.00% used (0.0% left)", true) + "\n\nOpenai Codex\n" +
		replyUsageRow("5 hours", "private@example.com (org-123) · plan: team", "12.50% used (87.5% left)", true) + "\n" +
		replyUsageRow("7 days", "private@example.com (org-123) · plan: team", "43.10% used (56.9% left)", true))
	for _, tc := range []struct {
		provider string
		want     string
	}{
		{"anthropic", "30% 65%"},
		{"openai-codex", "12.5% 43.1%"},
		{"openai", ""},
		{"claude", ""},
		{"Anthropic", ""},
		{"", ""},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			if got := replyUsageSample(text, tc.provider, replyUsageObservedAt).text(replyUsageObservedAt); got != tc.want {
				t.Fatalf("quota = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplyUsageSampleIgnoresStyledBarsNotStyledFields(t *testing.T) {
	for _, provider := range []struct {
		id, heading, short, week string
	}{
		{"anthropic", "Anthropic", "Claude 5 Hour", "Claude 7 Day"},
		{"openai-codex", "Openai Codex", "5 hours", "7 days"},
	} {
		t.Run(provider.id, func(t *testing.T) {
			text := replyUsageReport(provider.heading + "\n" +
				replyUsageRow(provider.short, "private@example.com", "30.00% used", true) + "\n" +
				replyUsageRow(provider.week, "private@example.com", "65.00% used", true))
			for _, bar := range []string{
				"  [███████░░░░░░░░░░░░░░░░░\x1b[39m] 99%",
				"  [\x1b[38;2;120;150;180m███████░░░░░░░░░░░░░░░░░\x1b[39m] 99%",
				"  [\x1b[38;2;120;150;180m························\x1b[39m]",
			} {
				styled := strings.ReplaceAll(text, "  [███████░░░░░░░░░░░░░░░░░] 30%", bar)
				if got := replyUsageSample(styled, provider.id, replyUsageObservedAt).text(replyUsageObservedAt); got != "30% 65%" {
					t.Fatalf("decorative bar changed account percentages: %q", got)
				}
			}
			for _, field := range []string{provider.heading, "private@example.com", "30.00% used", "← in use by this session"} {
				styled := strings.ReplaceAll(text, field, "\x1b[32m"+field+"\x1b[39m")
				if got := replyUsageSample(styled, provider.id, replyUsageObservedAt).text(replyUsageObservedAt); got != "" {
					t.Fatalf("styled semantic field produced quota: %q", got)
				}
			}
			intervening := strings.Replace(text, "- "+provider.short+"\n", "- "+provider.short+"\n  [\x1b[39m] 99%\n", 1)
			if got := replyUsageSample(intervening, provider.id, replyUsageObservedAt).text(replyUsageObservedAt); got != "" {
				t.Fatalf("bar substituted for the required adjacent account row: %q", got)
			}
		})
	}
}

func TestReplyUsageSampleRequiresExplicitUnambiguousAccount(t *testing.T) {
	row := func(label, account string, active bool) string {
		return replyUsageRow(label, account, "30.00% used (70.0% left)", active)
	}
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"single unmarked account", row("Claude 5 Hour", "private@example.com", false), ""},
		{"unmarked windows", row("Claude 5 Hour", "private@example.com", false) + "\n" + row("Claude 7 Day", "private@example.com", false), ""},
		{"active second account", row("Claude 5 Hour", "other@example.com", false) + "\n" + row("Claude 5 Hour", "private@example.com", true), "5h 30%"},
		{"only marked window", row("Claude 5 Hour", "private@example.com", true) + "\n" + row("Claude 7 Day", "private@example.com", false), "5h 30%"},
		{"only weekly", row("Claude 7 Day", "private@example.com", true), "7d 30%"},
		{"different accounts", row("Claude 5 Hour", "first@example.com", true) + "\n" + row("Claude 7 Day", "second@example.com", true), ""},
		{"same email different organizations", row("Claude 5 Hour", "private@example.com (First Team)", true) + "\n" + row("Claude 7 Day", "private@example.com (Other Team)", true), ""},
		{"two marked accounts same window", row("Claude 5 Hour", "first@example.com", true) + "\n" + row("Claude 5 Hour", "second@example.com", true), ""},
		{"duplicate active window", row("Claude 5 Hour", "private@example.com", true) + "\n" + row("Claude 5 Hour", "private@example.com", true), ""},
		{"duplicate omits only affected window", row("Claude 5 Hour", "private@example.com", true) + "\n" + row("Claude 5 Hour", "private@example.com", true) + "\n" + row("Claude 7 Day", "private@example.com", true), "7d 30%"},
		{"conflicting duplicate omits only affected window", row("Claude 5 Hour", "private@example.com", true) + "\n" + replyUsageRow("Claude 5 Hour", "private@example.com", "50.00% used", true) + "\n" + row("Claude 7 Day", "private@example.com", true), "7d 30%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replyUsageSample(replyUsageReport("Anthropic\n"+tc.body), "anthropic", replyUsageObservedAt).text(replyUsageObservedAt); got != tc.want {
				t.Fatalf("quota = %q, want %q", got, tc.want)
			}
		})
	}
	codex := replyUsageReport("Openai Codex\n" +
		row("5 hours", "private@example.com (org-first) · plan: team", true) + "\n" +
		row("7 days", "private@example.com (org-second) · plan: team", true))
	if got := replyUsageSample(codex, "openai-codex", replyUsageObservedAt).text(replyUsageObservedAt); got != "" {
		t.Fatalf("different Codex workspaces produced quota %q", got)
	}
}

func TestReplyUsageSampleExactWindowsExcludeModelTiers(t *testing.T) {
	for _, tc := range []struct {
		provider string
		heading  string
		label    string
	}{
		{"anthropic", "Anthropic", "Claude 7 Day (Opus)"},
		{"anthropic", "Anthropic", "Claude 7 Day (Sonnet)"},
		{"anthropic", "Anthropic", "Claude 7 Day (Fable)"},
		{"anthropic", "Anthropic", "Claude 7 Day (Mythos)"},
		{"anthropic", "Anthropic", "Claude 7 Day (Unknown Model) (unknown-model)"},
		{"anthropic", "Anthropic", "Claude Extra Usage — extra"},
		{"anthropic", "Anthropic", "7 days"},
		{"openai-codex", "Openai Codex", "5 hours (Spark)"},
		{"openai-codex", "Openai Codex", "7 days (Spark)"},
		{"openai-codex", "Openai Codex", "Primary window"},
		{"openai-codex", "Openai Codex", "Secondary window"},
		{"openai-codex", "Openai Codex", "1 hour"},
		{"openai-codex", "Openai Codex", "Spend cap"},
		{"openai-codex", "Openai Codex", "Claude 7 Day"},
		{"openai-codex", "OpenAI Codex", "5 hours"},
	} {
		t.Run(tc.provider+"/"+tc.label, func(t *testing.T) {
			text := replyUsageReport(tc.heading + "\n" + replyUsageRow(tc.label, "private@example.com", "99.00% used (1.0% left)", true))
			if got := replyUsageSample(text, tc.provider, replyUsageObservedAt).text(replyUsageObservedAt); got != "" {
				t.Fatalf("non-general or unknown window produced quota %q", got)
			}
		})
	}
	text := replyUsageReport("Anthropic\n" + replyUsageRow("Claude 5 Hour", "private@example.com", "30.00% used", true) + "\n" +
		replyUsageRow("Claude 7 Day (Sonnet)", "private@example.com", "99.00% used", true))
	if got := replyUsageSample(text, "anthropic", replyUsageObservedAt).text(replyUsageObservedAt); got != "5h 30%" {
		t.Fatalf("scoped tier substituted for missing general week: %q", got)
	}
}

func TestReplyUsageSampleValidatesPercentages(t *testing.T) {
	for _, tc := range []struct {
		amount string
		want   string
	}{
		{"0.00% used (100.0% left)", "5h 0%"},
		{"100.00% used (0.0% left)", "5h 100%"},
		{"30.25% used (69.8% left)", "5h 30.25%"},
		{"30.10% used", "5h 30.1%"},
		{"0.01% used", "5h 0.01%"},
		{"unknown used", ""},
		{"NaN% used", ""},
		{"Inf% used", ""},
		{"+Inf% used", ""},
		{"-0.01% used", ""},
		{"100.01% used", ""},
		{"1e1% used", ""},
		{"1.234567890123456789% used", ""},
		{"% used", ""},
		{"30.% used", ""},
		{".30% used", ""},
		{"30..0% used", ""},
		{"30.00% used trailing text", ""},
		{"30.00% used (NaN% left)", ""},
		{"30.00% used (101.0% left)", ""},
		{"30.00% used (-1.0% left)", ""},
		{"30.00% used (70.0% left", ""},
	} {
		t.Run(tc.amount, func(t *testing.T) {
			text := replyUsageReport("Anthropic\n" + replyUsageRow("Claude 5 Hour", "private@example.com", tc.amount, true))
			if got := replyUsageSample(text, "anthropic", replyUsageObservedAt).text(replyUsageObservedAt); got != tc.want {
				t.Fatalf("quota = %q, want %q", got, tc.want)
			}
		})
	}
	text := replyUsageReport("Anthropic\n" + replyUsageRow("Claude 5 Hour", "private@example.com", "NaN% used", true) + "\n" +
		replyUsageRow("Claude 7 Day", "private@example.com", "65.00% used", true))
	if got := replyUsageSample(text, "anthropic", replyUsageObservedAt).text(replyUsageObservedAt); got != "7d 65%" {
		t.Fatalf("invalid short window hid valid weekly window: %q", got)
	}
}

func TestReplyUsageSampleRejectsMalformedReportsAndBounds(t *testing.T) {
	valid := replyUsageReport("Anthropic\n" + replyUsageRow("Claude 5 Hour", "private@example.com", "30.00% used", true))
	for _, tc := range []struct {
		name string
		text string
	}{
		{"token and cost fallback", "Usage\nInput tokens: 100\nOutput tokens: 10\nCost: $0.123456"},
		{"unfenced provider report", strings.Trim(valid, "`\n")},
		{"missing closing fence", strings.TrimSuffix(valid, "\n```")},
		{"markdown language fence", strings.Replace(valid, "```", "```text", 1)},
		{"wrong provider heading", strings.Replace(valid, "Anthropic", "Claude", 1)},
		{"wrong report title", strings.Replace(valid, "Usage (2s ago)", "Usage statistics", 1)},
		{"missing active marker", strings.ReplaceAll(valid, "  ← in use by this session", "")},
		{"different active marker", strings.ReplaceAll(valid, "← in use by this session", "(active)")},
		{"marker not at row end", strings.ReplaceAll(valid, "← in use by this session", "← in use by this session extra")},
		{"marker on separate line", strings.ReplaceAll(valid, "  ← in use by this session", "\n  ← in use by this session")},
		{"empty identity", strings.ReplaceAll(valid, "private@example.com", "")},
		{"malformed identity", strings.ReplaceAll(valid, "private@example.com: ", "private@example.com ")},
		{"missing account indentation", strings.ReplaceAll(valid, "  private@example.com", "private@example.com")},
		{"intervening line", strings.ReplaceAll(valid, "- Claude 5 Hour\n", "- Claude 5 Hour\n  notes\n")},
		{"ANSI report", strings.ReplaceAll(valid, "Anthropic", "\x1b[32mAnthropic\x1b[0m")},
		{"tab indentation", strings.ReplaceAll(valid, "  private@example.com", "\tprivate@example.com")},
		{"duplicate provider section", strings.TrimSuffix(valid, "\n```") + "\nAnthropic\n```"},
		{"multiple reports", valid + "\n" + valid},
		{"trailing prose", valid + "\nadditional text"},
		{"byte bound", strings.Repeat("x", replyUsageMaxBytes+1)},
		{"line count bound", strings.Replace(valid, "\nAnthropic", strings.Repeat("\n", replyUsageMaxLines)+"Anthropic", 1)},
		{"line length bound", strings.Replace(valid, "Anthropic", strings.Repeat("x", replyUsageMaxLine+1)+"\nAnthropic", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replyUsageSample(tc.text, "anthropic", replyUsageObservedAt).text(replyUsageObservedAt); got != "" {
				t.Fatalf("malformed report produced quota %q", got)
			}
		})
	}
	for _, text := range []string{
		strings.ReplaceAll(valid, "\n", "\r\n"),
		strings.Replace(valid, "Usage (2s ago)", "Usage", 1),
		strings.Replace(valid, "\n```", "\n\n```", 1),
	} {
		if got := replyUsageSample(text, "anthropic", replyUsageObservedAt).text(replyUsageObservedAt); got != "5h 30%" {
			t.Fatalf("valid native framing produced quota %q", got)
		}
	}
	otherProvider := strings.TrimSuffix(valid, "\n```") + "\n\nOther Provider\n" +
		replyUsageRow("Claude 5 Hour", "other@example.com", "99.00% used", true) + "\n```"
	if got := replyUsageSample(otherProvider, "anthropic", replyUsageObservedAt).text(replyUsageObservedAt); got != "5h 30%" {
		t.Fatalf("another provider contaminated current provider: %q", got)
	}
}

func TestReplyUsageSampleAssociatesResetWithActiveAccountAndWindow(t *testing.T) {
	for _, provider := range []struct {
		id, heading, short, week string
	}{
		{"anthropic", "Anthropic", "Claude 5 Hour", "Claude 7 Day"},
		{"openai-codex", "Openai Codex", "5 hours", "7 days"},
	} {
		t.Run(provider.id, func(t *testing.T) {
			text := replyUsageReport(provider.heading + "\n" +
				replyUsageRow(provider.short, "other@example.com", "99% used", false) + "\n  resets in 9h\n" +
				replyUsageRow(provider.week, "private@example.com", "60% used", true) + "\n  resets in 5d\n" +
				replyUsageRow(provider.short, "private@example.com", "20% used", true) + "\n  resets in 2h\n" +
				"  other@example.com: 99% used\n  [████] 99%\n  resets in 9h")
			sample := replyUsageSample(text, provider.id, replyUsageObservedAt)
			want := quotaSample{windows: [2]quotaWindow{
				{present: true, used: 20, resetAt: replyUsageObservedAt.Add(2 * time.Hour), approximate: true, resetPrecision: time.Hour},
				{present: true, used: 60, resetAt: replyUsageObservedAt.Add(5 * 24 * time.Hour), approximate: true, resetPrecision: 24 * time.Hour},
			}}
			if sample != want {
				t.Fatalf("sample = %#v, want %#v", sample, want)
			}
			if got := sample.text(replyUsageObservedAt); got != "20%(~2h) 60%(~5d)" {
				t.Fatalf("quota = %q", got)
			}
		})
	}
}

func TestReplyUsageSampleMissingOrInvalidResetPreservesPercentage(t *testing.T) {
	for _, reset := range []string{
		"",
		"  resets in ",
		"  resets in 0h",
		"  resets in -1h",
		"  resets in +1h",
		"  resets in 1.5h",
		"  resets in 1h30m",
		"  resets in 1w",
		"  resets in 1H",
		"  resets in 1h ",
		"  resets in 18446744073709551616s",
		"  resets in 9223372037s",
		"  resets in 106752d",
		"  resets in 2h\n  resets in 2h",
		"  resets in invalid\n  resets in 2h",
		"  resets in 2h\n  resets in invalid",
	} {
		t.Run(reset, func(t *testing.T) {
			text := replyUsageReport("Anthropic\n" + replyUsageRow("Claude 5 Hour", "private@example.com", "20% used", true) + "\n" + reset)
			sample := replyUsageSample(text, "anthropic", replyUsageObservedAt)
			want := quotaSample{windows: [2]quotaWindow{{present: true, used: 20}}}
			if sample != want {
				t.Fatalf("sample = %#v, want percentage without reset %#v", sample, want)
			}
			if got := sample.text(replyUsageObservedAt); got != "5h 20%" {
				t.Fatalf("quota = %q", got)
			}
		})
	}
}

func TestReplyUsageSampleResetDoesNotCrossReportBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, intervening string
	}{
		{"account", "  other@example.com: 99% used\n  [████] 99%"},
		{"window", replyUsageRow("Claude 7 Day", "other@example.com", "99% used", false)},
		{"model tier", replyUsageRow("Claude 7 Day (Opus)", "private@example.com", "99% used", true)},
		{"section", "Openai Codex"},
		{"saved resets", "- private@example.com: 2 saved rate-limit resets — available — /usage reset to spend\n  soonest expires in 2d (2026-11-09)"},
		{"blank line", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := replyUsageReport("Anthropic\n" + replyUsageRow("Claude 5 Hour", "private@example.com", "20% used", true) +
				"\n" + tc.intervening + "\n  resets in 9h")
			sample := replyUsageSample(text, "anthropic", replyUsageObservedAt)
			want := quotaSample{windows: [2]quotaWindow{{present: true, used: 20}}}
			if sample != want {
				t.Fatalf("unrelated reset crossed %s: %#v", tc.name, sample)
			}
		})
	}
}

func TestReplyUsageSampleApproximateResetAgesWithoutRequery(t *testing.T) {
	for _, tc := range []struct {
		reset     string
		precision time.Duration
		duration  time.Duration
		aged      string
	}{
		{"2d", 24 * time.Hour, 48 * time.Hour, "7d 60%(~1d)"},
		{"2h", time.Hour, 2 * time.Hour, "7d 60%(~1h)"},
		{"2m", time.Minute, 2 * time.Minute, "7d 60%(~1m)"},
		{"2s", time.Second, 2 * time.Second, "7d 60%(~1s)"},
	} {
		t.Run(tc.reset, func(t *testing.T) {
			text := replyUsageReport("Anthropic\n- Claude 7 Day\n  private@example.com: 60% used" + replyUsageMarker + "\n  resets in " + tc.reset)
			sample := replyUsageSample(text, "anthropic", replyUsageObservedAt)
			window := sample.windows[1]
			if !window.present || window.used != 60 || !window.approximate || window.resetPrecision != tc.precision || !window.resetAt.Equal(replyUsageObservedAt.Add(tc.duration)) {
				t.Fatalf("native reset lost its unit or observation time: %#v", window)
			}
			if got := sample.text(replyUsageObservedAt); got != "7d 60%(~"+tc.reset+")" {
				t.Fatalf("initial quota = %q", got)
			}
			if got := sample.text(replyUsageObservedAt.Add(tc.precision)); got != tc.aged {
				t.Fatalf("aged quota = %q, want %q", got, tc.aged)
			}
			if got := sample.text(replyUsageObservedAt.Add(tc.duration)); got != "7d 60%" {
				t.Fatalf("expired countdown changed used quota: %q", got)
			}
		})
	}
}
