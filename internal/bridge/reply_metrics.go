package bridge

import "math"

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
	return compactSpeedStatus(speed)
}
