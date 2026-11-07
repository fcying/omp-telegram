package bridge

import (
	"strconv"
	"strings"
	"time"
)

const (
	replyUsageMaxBytes = 256 << 10
	replyUsageMaxLines = 2048
	replyUsageMaxLine  = 4096
	replyUsageMarker   = "  ← in use by this session"
)

// replyUsageSample reads only native, fenced /usage reports, never token/cost tallies.
// Native labels and account markers come from coding-agent's usage-report.ts,
// ai/src/usage/{claude,openai-codex}.ts, and tui/src/chrome/format.ts:
// https://github.com/can1357/oh-my-pi/tree/main/packages
func replyUsageSample(text, provider string, observedAt time.Time) quotaSample {
	var heading string
	var labels [2]string
	switch provider {
	case "anthropic":
		heading, labels = "Anthropic", [2]string{"- Claude 5 Hour", "- Claude 7 Day"}
	case "openai-codex":
		heading, labels = "Openai Codex", [2]string{"- 5 hours", "- 7 days"}
	default:
		return quotaSample{}
	}
	if len(text) > replyUsageMaxBytes {
		return quotaSample{}
	}
	text = strings.Trim(text, "\r\n")
	var windows [2]struct {
		seen           bool
		invalid        bool
		used           float64
		resetSeen      bool
		resetInvalid   bool
		resetAt        time.Time
		resetPrecision time.Duration
	}
	account := ""
	selected, closed := false, false
	pending, eligible, sections, count := -1, -1, 0, 0
	for line := range strings.Lines(text) {
		count++
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if count > replyUsageMaxLines || len(line) > replyUsageMaxLine || closed {
			return quotaSample{}
		}
		// Ignore native decorative bars without interpreting their terminal styling.
		if count > 2 && pending < 0 && replyUsageBar(line) {
			continue
		}
		for _, c := range line {
			if c < ' ' || c == '\x7f' {
				return quotaSample{}
			}
		}
		switch count {
		case 1:
			if line != "```" {
				return quotaSample{}
			}
			continue
		case 2:
			if line != "Usage" && !(strings.HasPrefix(line, "Usage (") && strings.HasSuffix(line, " ago)")) {
				return quotaSample{}
			}
			continue
		}
		if line == "```" {
			closed = true
			eligible = -1
			continue
		}
		if pending >= 0 {
			index := pending
			window := &windows[index]
			pending = -1
			eligible = -1
			if strings.HasPrefix(line, "  ") && strings.HasSuffix(line, replyUsageMarker) {
				if window.seen {
					window.invalid = true
				}
				window.seen = true
				row := strings.TrimSuffix(strings.TrimPrefix(line, "  "), replyUsageMarker)
				separator := strings.LastIndex(row, ": ")
				if separator <= 0 {
					window.invalid = true
					continue
				}
				identity := row[:separator]
				if strings.TrimSpace(identity) != identity {
					window.invalid = true
					continue
				}
				// Preserve organization and Codex plan qualifiers when comparing accounts.
				if account != "" && account != identity {
					return quotaSample{}
				}
				account = identity
				amount := row[separator+2:]
				used, rest, ok := strings.Cut(amount, "% used")
				percent, valid := replyUsagePercent(used)
				if !ok || !valid {
					window.invalid = true
					continue
				}
				if rest != "" {
					left := strings.TrimSuffix(strings.TrimPrefix(rest, " ("), "% left)")
					if _, valid := replyUsagePercent(left); !valid || rest != " ("+left+"% left)" {
						window.invalid = true
						continue
					}
				}
				window.used = percent
				eligible = index
				continue
			}
		}
		if eligible >= 0 && strings.HasPrefix(line, "  resets") {
			window := &windows[eligible]
			duration, precision, valid := replyUsageReset(line)
			if window.resetSeen || !valid {
				window.resetInvalid = true
			}
			window.resetSeen = true
			if valid && !window.resetInvalid {
				window.resetAt = observedAt.Add(duration)
				window.resetPrecision = precision
			}
			continue
		}
		eligible = -1
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "- ") {
			selected = line == heading
			if selected {
				sections++
				if sections > 1 {
					return quotaSample{}
				}
			}
		}
		if selected {
			for i, label := range labels {
				// Exact labels exclude scoped weekly tiers and unknown-duration windows.
				if line == label {
					pending = i
					break
				}
			}
		}
	}
	if !closed {
		return quotaSample{}
	}
	var sample quotaSample
	for i, window := range windows {
		if !window.seen || window.invalid {
			continue
		}
		sample.windows[i] = quotaWindow{present: true, used: window.used}
		if window.resetSeen && !window.resetInvalid {
			sample.windows[i].resetAt = window.resetAt
			sample.windows[i].approximate = true
			sample.windows[i].resetPrecision = window.resetPrecision
		}
	}
	return sample
}

func replyUsageBar(line string) bool {
	if !strings.HasPrefix(line, "  [") {
		return false
	}
	end := strings.LastIndexByte(line, ']')
	if end < 3 {
		return false
	}
	suffix := line[end+1:]
	if suffix == "" {
		return true
	}
	amount, ok := strings.CutPrefix(suffix, " ")
	if !ok || !strings.HasSuffix(amount, "%") {
		return false
	}
	_, valid := replyUsagePercent(strings.TrimSuffix(amount, "%"))
	return valid
}

func replyUsagePercent(text string) (float64, bool) {
	// Native usage amounts have at most two decimal places; no exponents or signs.
	whole, fraction, decimal := strings.Cut(text, ".")
	if len(whole) == 0 || len(whole) > 3 || decimal && (len(fraction) == 0 || len(fraction) > 2) {
		return 0, false
	}
	for _, digit := range text {
		if digit != '.' && (digit < '0' || digit > '9') {
			return 0, false
		}
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || !validStatusNumber(value) || value > 100 {
		return 0, false
	}
	return value, true
}

func replyUsageReset(line string) (time.Duration, time.Duration, bool) {
	amount, ok := strings.CutPrefix(line, "  resets in ")
	if !ok || len(amount) < 2 {
		return 0, 0, false
	}
	var precision time.Duration
	switch amount[len(amount)-1] {
	case 'd':
		precision = 24 * time.Hour
	case 'h':
		precision = time.Hour
	case 'm':
		precision = time.Minute
	case 's':
		precision = time.Second
	default:
		return 0, 0, false
	}
	amount = amount[:len(amount)-1]
	for _, digit := range amount {
		if digit < '0' || digit > '9' {
			return 0, 0, false
		}
	}
	value, err := strconv.ParseUint(amount, 10, 64)
	if err != nil || value == 0 || value > uint64((1<<63-1)/precision) {
		return 0, 0, false
	}
	return time.Duration(value) * precision, precision, true
}
