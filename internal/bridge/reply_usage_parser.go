package bridge

import (
	"strconv"
	"strings"
)

const (
	replyUsageMaxBytes = 256 << 10
	replyUsageMaxLines = 2048
	replyUsageMaxLine  = 4096
	replyUsageMarker   = "  ← in use by this session"
)

// replyUsageHeader reads only native, fenced /usage reports, never token/cost tallies.
// Native labels and account markers come from coding-agent's usage-report.ts,
// ai/src/usage/{claude,openai-codex}.ts, and tui/src/chrome/format.ts:
// https://github.com/can1357/oh-my-pi/tree/main/packages
func replyUsageHeader(text, provider string) string {
	var heading string
	var labels [2]string
	switch provider {
	case "anthropic":
		heading, labels = "Anthropic", [2]string{"- Claude 5 Hour", "- Claude 7 Day"}
	case "openai-codex":
		heading, labels = "Openai Codex", [2]string{"- 5 hours", "- 7 days"}
	default:
		return ""
	}
	if len(text) > replyUsageMaxBytes {
		return ""
	}
	text = strings.Trim(text, "\r\n")
	var windows [2]struct {
		seen    bool
		invalid bool
		percent string
	}
	account := ""
	selected, closed := false, false
	pending, sections, count := -1, 0, 0
	for line := range strings.Lines(text) {
		count++
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if count > replyUsageMaxLines || len(line) > replyUsageMaxLine || closed {
			return ""
		}
		// Ignore native decorative bars without interpreting their terminal styling.
		if count > 2 && pending < 0 && replyUsageBar(line) {
			continue
		}
		for _, c := range line {
			if c < ' ' || c == '\x7f' {
				return ""
			}
		}
		switch count {
		case 1:
			if line != "```" {
				return ""
			}
			continue
		case 2:
			if line != "Usage" && !(strings.HasPrefix(line, "Usage (") && strings.HasSuffix(line, " ago)")) {
				return ""
			}
			continue
		}
		if line == "```" {
			closed = true
			continue
		}
		if pending >= 0 {
			window := &windows[pending]
			pending = -1
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
					return ""
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
				window.percent = percent
				continue
			}
		}
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "- ") {
			selected = line == heading
			if selected {
				sections++
				if sections > 1 {
					return ""
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
		return ""
	}
	var parts [2]string
	partCount := 0
	for i, label := range [...]string{"5h", "7d"} {
		if window := windows[i]; window.seen && !window.invalid && window.percent != "" {
			parts[partCount] = label + window.percent + "%"
			partCount++
		}
	}
	return strings.Join(parts[:partCount], " ")
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

func replyUsagePercent(text string) (string, bool) {
	// Native usage amounts have at most two decimal places; no exponents or signs.
	whole, fraction, decimal := strings.Cut(text, ".")
	if len(whole) == 0 || len(whole) > 3 || decimal && (len(fraction) == 0 || len(fraction) > 2) {
		return "", false
	}
	for _, digit := range text {
		if digit != '.' && (digit < '0' || digit > '9') {
			return "", false
		}
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || !validStatusNumber(value) || value > 100 {
		return "", false
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(value, 'f', 2, 64), "0"), "."), true
}
