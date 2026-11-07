package bridge

import (
	"testing"
	"time"
)

func TestQuotaSampleUsedAndResetDisplay(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		sample quotaSample
		want   string
	}{
		{"both windows", quotaSample{windows: [2]quotaWindow{
			{present: true, used: 20, resetAt: now.Add(2 * time.Hour)},
			{present: true, used: 60, resetAt: now.Add((5*24 + 12) * time.Hour)},
		}}, "20%(2h) 60%(5d12h)"},
		{"weekly only", quotaSample{windows: [2]quotaWindow{{},
			{present: true, used: 60, resetAt: now.Add((5*24 + 12) * time.Hour)},
		}}, "7d 60%(5d12h)"},
		{"missing reset", quotaSample{windows: [2]quotaWindow{
			{present: true, used: 30.25},
		}}, "5h 30.25%"},
		{"zero usage and short reset", quotaSample{windows: [2]quotaWindow{
			{present: true, resetAt: now.Add(30 * time.Minute)},
		}}, "5h 0%(<1h)"},
		{"approximate native day", quotaSample{windows: [2]quotaWindow{{},
			{present: true, used: 100, resetAt: now.Add(6 * 24 * time.Hour), approximate: true, resetPrecision: 24 * time.Hour},
		}}, "7d 100%(~6d)"},
		{"relative reset", quotaSample{label: "Plus", windows: [2]quotaWindow{
			{present: true, used: 20, resetAt: now.Add(2 * time.Hour), approximate: true},
		}}, "Plus 5h 20%(~2h)"},
		{"unavailable is not unused", quotaSample{label: "Plus"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sample.text(now); got != tc.want {
				t.Fatalf("quota = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestQuotaCacheCountdownAgesWithoutInventingResetUsage(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add((5*24 + 12) * time.Hour)
	var cache providerUsageCache
	key := providerUsageKey{provider: "anthropic"}
	cache.claim(key, now)
	cache.save(key, quotaSample{windows: [2]quotaWindow{{}, {present: true, used: 60, resetAt: resetAt}}})
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{now, "7d 60%(5d12h)"},
		{now.Add(time.Hour), "7d 60%(5d11h)"},
		{resetAt, "7d 60%"},
		{resetAt.Add(time.Hour), "7d 60%"},
		{resetAt.Add(365 * 24 * time.Hour), "7d 60%"},
	} {
		if got := cache.load(key).sample.text(tc.at); got != tc.want {
			t.Fatalf("cached quota at %v = %q, want %q", tc.at, got, tc.want)
		}
	}
}

func TestQuotaCacheEmptyRefreshKeepsLastGood(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var cache providerUsageCache
	key := providerUsageKey{provider: "openai-codex"}
	cache.claim(key, now)
	cache.save(key, quotaSample{label: "Plus", windows: [2]quotaWindow{{present: true, used: 30}}})
	cache.save(key, quotaSample{label: "Prolite"})
	if got := cache.load(key).sample.text(now.Add(365 * 24 * time.Hour)); got != "Plus 5h 30%" {
		t.Fatalf("empty refresh replaced unlimited last-good: %q", got)
	}
}
