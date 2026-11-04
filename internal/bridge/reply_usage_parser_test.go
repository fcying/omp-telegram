package bridge

import (
	"strings"
	"testing"
)

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
	return "- " + label + "\n  " + account + ": " + amount + marker + "\n  [███████░░░░░░░░░░░░░░░░░] 30%\n  resets in 3h"
}

func TestReplyUsageHeaderNativeProviderLayouts(t *testing.T) {
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
		{"anthropic", "5h30% 7d65%"},
		{"openai-codex", "5h12.5% 7d43.1%"},
		{"openai", ""},
		{"claude", ""},
		{"Anthropic", ""},
		{"", ""},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			if got := replyUsageHeader(text, tc.provider); got != tc.want {
				t.Fatalf("header = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplyUsageHeaderIgnoresStyledBarsNotStyledFields(t *testing.T) {
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
				if got := replyUsageHeader(styled, provider.id); got != "5h30% 7d65%" {
					t.Fatalf("decorative bar changed account percentages: %q", got)
				}
			}
			for _, field := range []string{provider.heading, "private@example.com", "30.00% used", "← in use by this session"} {
				styled := strings.ReplaceAll(text, field, "\x1b[32m"+field+"\x1b[39m")
				if got := replyUsageHeader(styled, provider.id); got != "" {
					t.Fatalf("styled semantic field produced quota: %q", got)
				}
			}
			intervening := strings.Replace(text, "- "+provider.short+"\n", "- "+provider.short+"\n  [\x1b[39m] 99%\n", 1)
			if got := replyUsageHeader(intervening, provider.id); got != "" {
				t.Fatalf("bar substituted for the required adjacent account row: %q", got)
			}
		})
	}
}

func TestReplyUsageHeaderRequiresExplicitUnambiguousAccount(t *testing.T) {
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
		{"active second account", row("Claude 5 Hour", "other@example.com", false) + "\n" + row("Claude 5 Hour", "private@example.com", true), "5h30%"},
		{"only marked window", row("Claude 5 Hour", "private@example.com", true) + "\n" + row("Claude 7 Day", "private@example.com", false), "5h30%"},
		{"only weekly", row("Claude 7 Day", "private@example.com", true), "7d30%"},
		{"different accounts", row("Claude 5 Hour", "first@example.com", true) + "\n" + row("Claude 7 Day", "second@example.com", true), ""},
		{"same email different organizations", row("Claude 5 Hour", "private@example.com (First Team)", true) + "\n" + row("Claude 7 Day", "private@example.com (Other Team)", true), ""},
		{"two marked accounts same window", row("Claude 5 Hour", "first@example.com", true) + "\n" + row("Claude 5 Hour", "second@example.com", true), ""},
		{"duplicate active window", row("Claude 5 Hour", "private@example.com", true) + "\n" + row("Claude 5 Hour", "private@example.com", true), ""},
		{"duplicate omits only affected window", row("Claude 5 Hour", "private@example.com", true) + "\n" + row("Claude 5 Hour", "private@example.com", true) + "\n" + row("Claude 7 Day", "private@example.com", true), "7d30%"},
		{"conflicting duplicate omits only affected window", row("Claude 5 Hour", "private@example.com", true) + "\n" + replyUsageRow("Claude 5 Hour", "private@example.com", "50.00% used", true) + "\n" + row("Claude 7 Day", "private@example.com", true), "7d30%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replyUsageHeader(replyUsageReport("Anthropic\n"+tc.body), "anthropic"); got != tc.want {
				t.Fatalf("header = %q, want %q", got, tc.want)
			}
		})
	}
	codex := replyUsageReport("Openai Codex\n" +
		row("5 hours", "private@example.com (org-first) · plan: team", true) + "\n" +
		row("7 days", "private@example.com (org-second) · plan: team", true))
	if got := replyUsageHeader(codex, "openai-codex"); got != "" {
		t.Fatalf("different Codex workspaces produced header %q", got)
	}
}

func TestReplyUsageHeaderExactWindowsExcludeModelTiers(t *testing.T) {
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
			if got := replyUsageHeader(text, tc.provider); got != "" {
				t.Fatalf("non-general or unknown window produced header %q", got)
			}
		})
	}
	text := replyUsageReport("Anthropic\n" + replyUsageRow("Claude 5 Hour", "private@example.com", "30.00% used", true) + "\n" +
		replyUsageRow("Claude 7 Day (Sonnet)", "private@example.com", "99.00% used", true))
	if got := replyUsageHeader(text, "anthropic"); got != "5h30%" {
		t.Fatalf("scoped tier substituted for missing general week: %q", got)
	}
}

func TestReplyUsageHeaderValidatesPercentages(t *testing.T) {
	for _, tc := range []struct {
		amount string
		want   string
	}{
		{"0.00% used (100.0% left)", "5h0%"},
		{"100.00% used (0.0% left)", "5h100%"},
		{"30.25% used (69.8% left)", "5h30.25%"},
		{"30.10% used", "5h30.1%"},
		{"0.01% used", "5h0.01%"},
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
			if got := replyUsageHeader(text, "anthropic"); got != tc.want {
				t.Fatalf("header = %q, want %q", got, tc.want)
			}
		})
	}
	text := replyUsageReport("Anthropic\n" + replyUsageRow("Claude 5 Hour", "private@example.com", "NaN% used", true) + "\n" +
		replyUsageRow("Claude 7 Day", "private@example.com", "65.00% used", true))
	if got := replyUsageHeader(text, "anthropic"); got != "7d65%" {
		t.Fatalf("invalid short window hid valid weekly window: %q", got)
	}
}

func TestReplyUsageHeaderRejectsMalformedReportsAndBounds(t *testing.T) {
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
			if got := replyUsageHeader(tc.text, "anthropic"); got != "" {
				t.Fatalf("malformed report produced header %q", got)
			}
		})
	}
	for _, text := range []string{
		strings.ReplaceAll(valid, "\n", "\r\n"),
		strings.Replace(valid, "Usage (2s ago)", "Usage", 1),
		strings.Replace(valid, "\n```", "\n\n```", 1),
	} {
		if got := replyUsageHeader(text, "anthropic"); got != "5h30%" {
			t.Fatalf("valid native framing produced header %q", got)
		}
	}
	otherProvider := strings.TrimSuffix(valid, "\n```") + "\n\nOther Provider\n" +
		replyUsageRow("Claude 5 Hour", "other@example.com", "99.00% used", true) + "\n```"
	if got := replyUsageHeader(otherProvider, "anthropic"); got != "5h30%" {
		t.Fatalf("another provider contaminated current provider: %q", got)
	}
}
