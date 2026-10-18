package bridge

import (
	"fmt"
	"math"
	"strings"

	"omp-telegram/internal/telegram"
)

// replyMetrics belongs to one complete reply, including its tool continuations.
// Assign replyMetrics{} before the next reply (or autoresearch round).
type replyMetrics struct {
	modelID          string
	provider         string
	output           int64
	duration         float64
	invalid          bool
	lastContext      int64
	lastContextKnown bool
}

func (m *replyMetrics) observe(msg message) {
	if msg.Role != "assistant" {
		return
	}
	// Keep identity even when this call cannot contribute a trustworthy speed.
	// A missing identity must not inherit the preceding assistant's model.
	m.modelID, m.provider = msg.ModelID, msg.Provider
	m.lastContext, m.lastContextKnown = replyContextTokens(msg.Usage)
	if msg.Usage == nil || msg.Usage.Output == nil || msg.Duration == nil {
		m.invalid = true
		return
	}
	output, duration := *msg.Usage.Output, *msg.Duration
	if output < 0 || !validStatusNumber(duration) || duration == 0 || output > math.MaxInt64-m.output {
		m.invalid = true
		return
	}
	totalDuration := m.duration + duration
	if !validStatusNumber(totalDuration) {
		m.invalid = true
		return
	}
	m.output += output
	m.duration = totalDuration
}

// Match native compaction accounting, not get_state's input-only prompt usage.
func replyContextTokens(usage *messageUsage) (int64, bool) {
	if usage == nil {
		return 0, false
	}
	if usage.ContextTokens != nil {
		return *usage.ContextTokens, *usage.ContextTokens >= 0
	}
	var raw int64
	if usage.TotalTokens != nil && *usage.TotalTokens < 0 {
		return 0, false
	}
	if usage.TotalTokens != nil && *usage.TotalTokens > 0 {
		raw = *usage.TotalTokens
	} else {
		for _, component := range [...]*int64{usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite} {
			if component == nil || *component < 0 || *component > math.MaxInt64-raw {
				return 0, false
			}
			raw += *component
		}
	}
	var orchestration int64
	if usage.Orchestration != nil {
		for _, component := range [...]*int64{usage.Orchestration.Input, usage.Orchestration.Output, usage.Orchestration.CacheRead} {
			if component == nil {
				continue
			}
			if *component < 0 || *component > math.MaxInt64-orchestration {
				return 0, false
			}
			orchestration += *component
		}
	}
	return max(0, raw-orchestration), true
}

func (m replyMetrics) speedText() string {
	// Match the native TUI's positive-output and minimum-duration rules, but
	// weight every call by its duration instead of averaging per-call speeds.
	if m.invalid || m.output <= 0 || m.duration < 100 {
		return ""
	}
	speed := float64(m.output) * 1000 / m.duration
	if !validStatusNumber(speed) {
		return ""
	}
	return statusDecimal(speed) + " tok/s"
}

func replyHeader(s statusState, m replyMetrics) string {
	modelID := m.modelID
	matchesState := true
	if modelID == "" {
		modelID = s.Model.ID
	} else {
		matchesState = modelID == s.Model.ID && (m.provider == "" || m.provider == s.Model.Provider)
	}
	var parts []string
	if model := menuText(modelID, 256); model != "" {
		thinking := ""
		if matchesState && s.Model.ID != "" {
			thinking = s.ConfiguredThinking
		}
		identity := compactModelStatus(model, thinking)
		if matchesState && s.Model.ID != "" {
			if fast := compactFastStatus(s); fast != "" {
				identity += " " + fast
			}
		}
		parts = append(parts, identity)
	}
	if matchesState && m.modelID != "" && m.provider != "" && m.lastContextKnown &&
		s.IsStreaming != nil && !*s.IsStreaming && s.IsCompacting != nil && !*s.IsCompacting &&
		s.ContextUsage != nil && s.ContextUsage.ContextWindow != nil && *s.ContextUsage.ContextWindow > 0 {
		window := *s.ContextUsage.ContextWindow
		percent := float64(m.lastContext) / float64(window) * 100
		parts = append(parts, fmt.Sprintf("◫ %.0f%% (%s)", percent, statusTokens(window)))
	}
	if speed := m.speedText(); speed != "" {
		parts = append(parts, "avg "+speed)
	}
	return strings.Join(parts, " | ")
}

func replyTaskReplies(text, header string, truncated bool) []string {
	if header == "" {
		return taskReplies(text, "", truncated)
	}
	// Bound and split the body first: prepending the small header must never
	// copy the entire, potentially 4 MiB, assistant output.
	budget := maxFinalReplyBytes - len(header) - 2 - len(replyTruncatedNotice)
	if len(text) > maxFinalReplyBytes-len(header)-2 || truncated && len(text) > budget {
		text = truncateUTF8(text, budget)
		truncated = true
	}
	maxParts := maxFinalReplyMessages
	if truncated {
		maxParts--
	}
	parts, overflow := splitTaskText(text, maxParts)
	if len(parts) == 0 {
		parts, overflow = splitTaskText(header, maxParts)
	} else {
		firstLimit := telegram.MaxMessageUTF16 - utf16Length(header) - 2
		first := telegram.SplitForTelegram(parts[0], firstLimit)
		// Raw fence repair can exceed the byte budget despite fitting on screen.
		if len(first) == 0 {
			first = []string{header}
		} else if len(first[0]) > maxFinalReplyBytes-len(header)-2-len(replyTruncatedNotice) {
			first = append([]string{header}, first...)
		} else {
			first[0] = header + "\n\n" + first[0]
		}
		if len(first) == 1 {
			parts[0] = first[0]
		} else {
			withHeader := make([]string, 0, len(first)+len(parts)-1)
			withHeader = append(withHeader, first...)
			parts = append(withHeader, parts[1:]...)
		}
	}
	truncated = truncated || overflow || len(parts) > maxFinalReplyMessages
	return fitTaskReplyBudget(parts, "", truncated)
}
