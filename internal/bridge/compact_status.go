package bridge

import (
	"fmt"
	"strings"

	"omp-telegram/internal/i18n"
)

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

func compactContextStatus(s statusState) string {
	usage := s.ContextUsage
	if usage == nil || usage.Tokens == nil || usage.ContextWindow == nil || *usage.Tokens < 0 || *usage.ContextWindow <= 0 {
		return ""
	}
	percent := float64(*usage.Tokens) / float64(*usage.ContextWindow) * 100
	if usage.Percent != nil && validStatusNumber(*usage.Percent) {
		percent = *usage.Percent
	}
	return fmt.Sprintf("◫ %.0f%% (%s)", percent, statusTokens(*usage.ContextWindow))
}

func compactProgressStatus(language i18n.Language, s statusState) string {
	identity := ""
	if model := menuText(s.Model.ID, 128); model != "" {
		identity = compactModelStatus(model, s.ConfiguredThinking)
	}
	if fast := compactFastStatus(language, s); fast != "" {
		if identity != "" {
			identity += " "
		}
		identity += fast
	}
	var parts []string
	if identity != "" {
		parts = append(parts, identity)
	}
	if context := compactContextStatus(s); context != "" {
		parts = append(parts, context)
	}
	if s.TokensPerSecond != nil && validStatusNumber(*s.TokensPerSecond) {
		parts = append(parts, s.speedText(language))
	}
	return strings.Join(parts, " | ")
}
