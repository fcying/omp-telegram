package bridge

import (
	"fmt"
	"strings"

	"omp-telegram/internal/i18n"
)

const compactHeaderSeparator = " | "

type compactStatusFields struct {
	status, model, thinking, fast, context, speed, quota, git string
}

func compactThinkingStatus(thinking string) string {
	switch thinking {
	case "off":
		return "⦸"
	case "minimal":
		return "○"
	case "low":
		return "◔"
	case "medium":
		return "◑"
	case "high":
		return "◒"
	case "xhigh":
		return "◕"
	case "max":
		return "◉"
	case "auto":
		return "⟳"
	default:
		if level := menuText(thinking, 32); level != "" && level != "n/a" {
			return progressToolLiteral(level)
		}
		return ""
	}
}

func compactFastStatus(language i18n.Language, s statusState) string {
	if s.FastModeActive == nil {
		return ""
	}
	if s.FastModeEnabled != nil && *s.FastModeActive != *s.FastModeEnabled {
		return i18n.Text(language, i18n.Msg.Fast, s.fastText(language))
	}
	if *s.FastModeActive {
		return "⚡"
	}
	return ""
}

func compactSpeedStatus(speed float64) string {
	if !validStatusNumber(speed) {
		return ""
	}
	return statusDecimal(speed) + " t/s"
}

// Nil metrics selects live native values from a completed status read. Reply
// metrics fence sampled settings and provide output-inclusive context and speed.
// Completed messages confirm context usage even while the root is streaming;
// capacity and identity must still match the sample.
func compactFields(language i18n.Language, s statusState, metrics *replyMetrics, completed, contextRead bool) compactStatusFields {
	modelID := s.Model.ID
	matchesState := true
	if metrics != nil && metrics.modelID != "" {
		modelID = metrics.modelID
		matchesState = modelID == s.Model.ID && (metrics.provider == "" || metrics.provider == s.Model.Provider)
	}
	var values compactStatusFields
	if model := menuText(modelID, 256); model != "" {
		values.model = progressToolLiteral(model)
		if matchesState && s.Model.ID != "" {
			values.thinking = compactThinkingStatus(s.ConfiguredThinking)
			values.fast = compactFastStatus(language, s)
		}
	}
	percent, window := 0.0, int64(0)
	usage := s.ContextUsage
	if contextRead && metrics == nil {
		if usage != nil && usage.Tokens != nil && usage.ContextWindow != nil && *usage.Tokens >= 0 && *usage.ContextWindow > 0 {
			window = *usage.ContextWindow
			percent = float64(*usage.Tokens) / float64(window) * 100
			if usage.Percent != nil && validStatusNumber(*usage.Percent) {
				percent = *usage.Percent
			}
		}
	} else if contextRead && matchesState && metrics.modelID != "" && metrics.provider != "" && metrics.lastContextKnown &&
		(completed || s.IsStreaming != nil && !*s.IsStreaming && s.IsCompacting != nil && !*s.IsCompacting) &&
		usage != nil && usage.ContextWindow != nil && *usage.ContextWindow > 0 {
		window = *usage.ContextWindow
		percent = float64(metrics.lastContext) / float64(window) * 100
	}
	if window > 0 {
		values.context = fmt.Sprintf("%.0f%% (%s)", percent, statusTokens(window))
	}
	if metrics != nil {
		values.speed = metrics.speedText()
	} else if s.TokensPerSecond != nil {
		values.speed = compactSpeedStatus(*s.TokensPerSecond)
	}
	return values
}

func renderCompactFields(fields [][]string, values compactStatusFields) string {
	return renderCompactFieldsLimit(fields, values, 0)
}

func renderCompactFieldsLimit(fields [][]string, values compactStatusFields, limit int) string {
	var result strings.Builder
	remaining := limit
	for _, group := range fields {
		shown := false
		for _, field := range group {
			value := ""
			switch field {
			case "status":
				value = values.status
			case "model":
				value = values.model
			case "thinking":
				value = values.thinking
			case "fast":
				value = values.fast
			case "context":
				value = values.context
			case "speed":
				value = values.speed
			case "quota":
				value = values.quota
			case "git":
				value = values.git
			}
			if value == "" {
				continue
			}
			// Keep leading inline-code delimiters from becoming block fences.
			if result.Len() == 0 && strings.HasPrefix(value, "```") {
				value = "**" + value + "**"
			}
			separator := ""
			if result.Len() > 0 {
				separator = compactHeaderSeparator
				if shown {
					separator = " "
				}
			}
			if limit > 0 {
				units := utf16Length(value) + len(separator)
				if units > remaining {
					continue
				}
				remaining -= units
			}
			result.WriteString(separator)
			result.WriteString(value)
			shown = true
		}
	}
	return result.String()
}
