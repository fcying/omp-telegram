package bridge

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"omp-telegram/internal/omp"
)

// Decode only fields that are safe to display, never raw model configuration.
type statusState struct {
	Model           omp.Model `json:"model"`
	SessionID       string    `json:"sessionId"`
	SessionName     string    `json:"sessionName"`
	ThinkingLevel   string    `json:"thinkingLevel"`
	FastModeEnabled *bool     `json:"fastModeEnabled"`
	FastModeActive  *bool     `json:"fastModeActive"`
	IsStreaming     *bool     `json:"isStreaming"`
	IsCompacting    *bool     `json:"isCompacting"`
	TokensPerSecond *float64  `json:"tokensPerSecond"`
	ContextUsage    *struct {
		Tokens        *int64   `json:"tokens"`
		ContextWindow *int64   `json:"contextWindow"`
		Percent       *float64 `json:"percent"`
	} `json:"contextUsage"`
}

func formatStatus(s statusState, workspace, sessionID, home string, queued int) string {
	if s.SessionID != "" {
		sessionID = s.SessionID
	}
	session := menuText(sessionID, 128)
	if session == "" {
		session = "n/a"
	}
	if name := menuText(s.SessionName, 160); name != "" {
		shortID := "n/a"
		if sessionID != "" {
			shortID = menuText(sessionID, 8)
		}
		session = name + "\nSession ID: " + shortID
	}
	model := "n/a"
	if s.Model.Provider != "" && s.Model.ID != "" {
		model = menuText(s.Model.Provider+"/"+s.Model.ID, 256)
	}
	thinking := menuText(s.ThinkingLevel, 32)
	if thinking == "" {
		thinking = "n/a"
	}
	fast := statusBool(s.FastModeActive, "on", "off")
	if s.FastModeEnabled != nil && (s.FastModeActive == nil || *s.FastModeActive != *s.FastModeEnabled) {
		fast += " (setting: " + statusBool(s.FastModeEnabled, "on", "off") + ")"
	}
	context := "n/a"
	if usage := s.ContextUsage; usage != nil && usage.Tokens != nil && usage.ContextWindow != nil && *usage.Tokens >= 0 && *usage.ContextWindow > 0 {
		percent := float64(*usage.Tokens) / float64(*usage.ContextWindow) * 100
		if usage.Percent != nil && validStatusNumber(*usage.Percent) {
			percent = *usage.Percent
		}
		context = fmt.Sprintf("%.0f%% (%s / %s)", percent, statusTokens(*usage.Tokens), statusTokens(*usage.ContextWindow))
	}
	speed := "n/a"
	if s.TokensPerSecond != nil && validStatusNumber(*s.TokensPerSecond) {
		speed = statusDecimal(*s.TokensPerSecond) + " tok/s"
	}
	return fmt.Sprintf("Workspace: %s\nSession: %s\nModel: %s\nThinking: %s\nFast: %s\nContext: %s\nRunning: %s\nCompacting: %s\nQueued: %d\nSpeed: %s",
		clipUTF16(statusWorkspace(workspace, home), 1024), session, model, thinking, fast, context,
		statusBool(s.IsStreaming, "yes", "no"), statusBool(s.IsCompacting, "yes", "no"), queued, speed)
}

func formatRuntimeStatus(s statusState, workspace, sessionID, home string, queued int, idle time.Duration) string {
	return formatStatus(s, workspace, sessionID, home, queued) + "\nOMP: connected\nIdle: " + statusIdle(idle)
}

func formatReleasedStatus(workspace, sessionID, home string, queued int, idle time.Duration) string {
	session := menuText(sessionID, 128)
	if session == "" {
		session = "n/a"
	}
	return fmt.Sprintf("Workspace: %s\nSession: %s\nOMP: released\nModel: unavailable while released\nContext: unavailable while released\nQueued: %d\nIdle: %s", clipUTF16(statusWorkspace(workspace, home), 1024), session, queued, statusIdle(idle))
}

func statusIdle(idle time.Duration) string {
	if idle < 0 {
		return "n/a"
	}
	if idle < time.Minute {
		return idle.Round(time.Second).String()
	}
	return idle.Round(time.Minute).String()
}

func statusWorkspace(workspace, home string) string {
	if filepath.IsAbs(workspace) && filepath.IsAbs(home) {
		if rel, err := filepath.Rel(home, workspace); err == nil {
			if rel == "." {
				return "~"
			}
			if filepath.IsLocal(rel) {
				return "~" + string(filepath.Separator) + rel
			}
		}
	}
	return workspace
}

func statusBool(value *bool, yes, no string) string {
	if value == nil {
		return "n/a"
	}
	if *value {
		return yes
	}
	return no
}

func validStatusNumber(n float64) bool {
	return n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func statusDecimal(n float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(n, 'f', 1, 64), ".0")
}

func statusTokens(n int64) string {
	if n >= 1000000 {
		return statusDecimal(float64(n)/1000000) + "M"
	}
	if n >= 1000 {
		return statusDecimal(float64(n)/1000) + "k"
	}
	return strconv.FormatInt(n, 10)
}
