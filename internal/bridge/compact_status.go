package bridge

import (
	"fmt"
	"strings"

	"omp-telegram/internal/i18n"
)

const compactHeaderSeparator = " | "

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
	return statusDecimal(speed) + "t/s"
}

// Nil metrics selects live native values; reply metrics fence sampled settings
// and provide output-inclusive context and whole-task average speed.
func compactHeader(language i18n.Language, s statusState, metrics *replyMetrics) string {
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
	if metrics == nil {
		if usage != nil && usage.Tokens != nil && usage.ContextWindow != nil && *usage.Tokens >= 0 && *usage.ContextWindow > 0 {
			window = *usage.ContextWindow
			percent = float64(*usage.Tokens) / float64(window) * 100
			if usage.Percent != nil && validStatusNumber(*usage.Percent) {
				percent = *usage.Percent
			}
		}
	} else if matchesState && metrics.modelID != "" && metrics.provider != "" && metrics.lastContextKnown &&
		s.IsStreaming != nil && !*s.IsStreaming && s.IsCompacting != nil && !*s.IsCompacting &&
		usage != nil && usage.ContextWindow != nil && *usage.ContextWindow > 0 {
		window = *usage.ContextWindow
		percent = float64(metrics.lastContext) / float64(window) * 100
	}
	if window > 0 {
		parts = append(parts, fmt.Sprintf("%.0f%%/%s", percent, statusTokens(window)))
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
