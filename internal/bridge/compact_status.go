package bridge

import (
	"fmt"
	"strings"

	"omp-telegram/internal/i18n"
)

const compactHeaderSeparator = " | "
const compactUnknownContext = "ctx ?"

func compactModelStatus(model, thinking string) string {
	label := progressToolLiteral(model)
	symbol := ""
	switch thinking {
	case "off":
		symbol = "⦸"
	case "minimal":
		symbol = "○"
	case "low":
		symbol = "◔"
	case "medium":
		symbol = "◑"
	case "high":
		symbol = "◒"
	case "xhigh":
		symbol = "◕"
	case "max":
		symbol = "◉"
	case "auto":
		symbol = "⟳"
	default:
		if level := menuText(thinking, 32); level != "" && level != "n/a" {
			return label + " (" + progressToolLiteral(level) + ")"
		}
	}
	if symbol != "" {
		return symbol + " " + label
	}
	return label
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

// Nil metrics selects live native values from a completed status read; reply
// metrics fence sampled settings and provide output-inclusive context and speed.
func compactHeader(language i18n.Language, s statusState, metrics *replyMetrics) string {
	return formatCompactHeader(language, s, metrics, false, true)
}

// Completed messages already confirm output-inclusive usage, even while their
// root is still streaming. Capacity and identity must still match the sample.
func compactMessageHeader(language i18n.Language, s statusState, metrics *replyMetrics, contextRead bool) string {
	return formatCompactHeader(language, s, metrics, true, contextRead)
}

func formatCompactHeader(language i18n.Language, s statusState, metrics *replyMetrics, completed, contextRead bool) string {
	modelID := s.Model.ID
	matchesState := true
	if metrics != nil && metrics.modelID != "" {
		modelID = metrics.modelID
		matchesState = modelID == s.Model.ID && (metrics.provider == "" || metrics.provider == s.Model.Provider)
	}
	var parts []string
	if model := menuText(modelID, 256); model != "" {
		thinking := ""
		if matchesState && s.Model.ID != "" {
			thinking = s.ConfiguredThinking
		}
		identity := compactModelStatus(model, thinking)
		if matchesState && s.Model.ID != "" {
			if fast := compactFastStatus(language, s); fast != "" {
				identity += " " + fast
			}
		}
		parts = append(parts, identity)
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
		parts = append(parts, fmt.Sprintf("%.0f%% (%s)", percent, statusTokens(window)))
	} else if contextRead {
		parts = append(parts, compactUnknownContext)
	}
	speed := ""
	if metrics != nil {
		speed = metrics.speedText()
	} else if s.TokensPerSecond != nil {
		speed = compactSpeedStatus(*s.TokensPerSecond)
	}
	if speed != "" {
		parts = append(parts, speed)
	}
	return strings.Join(parts, compactHeaderSeparator)
}
