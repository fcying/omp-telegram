package bridge

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
)

// Decode only fields that are safe to display, never raw model configuration.
type statusState struct {
	// ConfiguredThinking is an actor-owned projection, not native effective state.
	ConfiguredThinking string `json:"-"`

	Model           omp.Model `json:"model"`
	SessionID       string    `json:"sessionId"`
	SessionName     string    `json:"sessionName"`
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

type statusModeResult struct {
	epoch     uint64
	sessionID string
	known     bool
	enabled   bool
	temporary bool
}

func formatStatus(language i18n.Language, s statusState, workspace, sessionID, home string, queued int) string {
	if s.SessionID != "" {
		sessionID = s.SessionID
	}
	session := menuText(sessionID, 128)
	if session == "" {
		session = i18n.Text(language, i18n.Msg.Unavailable)
	}
	if name := menuText(s.SessionName, 160); name != "" {
		session = i18n.Text(language, i18n.Msg.NamedSessionIdentity, name, session)
	}
	return i18n.Text(language, i18n.Msg.DetailedStatus,
		clipUTF16(statusWorkspace(workspace, home), 1024), session, s.modelText(language), s.thinkingText(language), s.fastText(language), s.contextText(language),
		statusBoolText(language, s.IsStreaming, i18n.Msg.Yes, i18n.Msg.No), statusBoolText(language, s.IsCompacting, i18n.Msg.Yes, i18n.Msg.No), queued, s.speedText(language))
}

func (s statusState) modelText(language i18n.Language) string {
	if s.Model.Provider != "" && s.Model.ID != "" {
		return menuText(s.Model.Provider+"/"+s.Model.ID, 256)
	}
	return i18n.Text(language, i18n.Msg.Unavailable)
}

func (s statusState) thinkingText(language i18n.Language) string {
	if thinking := menuText(s.ConfiguredThinking, 32); thinking != "" {
		return thinking
	}
	return i18n.Text(language, i18n.Msg.Unavailable)
}

func (s statusState) fastText(language i18n.Language) string {
	fast := statusBoolText(language, s.FastModeActive, i18n.Msg.On, i18n.Msg.Off)
	if s.FastModeEnabled != nil && (s.FastModeActive == nil || *s.FastModeActive != *s.FastModeEnabled) {
		fast = i18n.Text(language, i18n.Msg.Setting, fast, statusBoolText(language, s.FastModeEnabled, i18n.Msg.On, i18n.Msg.Off))
	}
	return fast
}

func (s statusState) contextText(language i18n.Language) string {
	if usage := s.ContextUsage; usage != nil && usage.Tokens != nil && usage.ContextWindow != nil && *usage.Tokens >= 0 && *usage.ContextWindow > 0 {
		percent := float64(*usage.Tokens) / float64(*usage.ContextWindow) * 100
		if usage.Percent != nil && validStatusNumber(*usage.Percent) {
			percent = *usage.Percent
		}
		return fmt.Sprintf("%.0f%% (%s / %s)", percent, statusTokens(*usage.Tokens), statusTokens(*usage.ContextWindow))
	}
	return i18n.Text(language, i18n.Msg.Unavailable)
}

func (s statusState) speedText(language i18n.Language) string {
	if s.TokensPerSecond != nil && validStatusNumber(*s.TokensPerSecond) {
		return statusDecimal(*s.TokensPerSecond) + " tok/s"
	}
	return i18n.Text(language, i18n.Msg.Unavailable)
}

func formatRuntimeStatus(language i18n.Language, s statusState, workspace, sessionID, home string, queued int, idle time.Duration, connected bool) string {
	runtime := i18n.Msg.RuntimeReleased
	if connected {
		runtime = i18n.Msg.RuntimeConnected
	}
	return formatStatus(language, s, workspace, sessionID, home, queued) + i18n.Text(language, i18n.Msg.RuntimeStatus, i18n.Text(language, runtime), statusIdle(language, idle))
}

func (w *worker) releaseStatusRuntime(result operationResult, valid bool) {
	mode, ok := result.meta.(*statusModeResult)
	if !ok || mode == nil || !mode.temporary || w.statusReleaseClientID == 0 {
		return
	}
	if !w.canReleaseRuntime() || w.statusReleaseClientID != w.client.ID() {
		w.statusReleaseClientID = 0
		return
	}
	if valid {
		idle, settled, pending := rpcIdleState(result.data)
		if !idle || !settled || pending {
			w.statusReleaseClientID = 0
			return
		}
	}
	w.finishPendingStatusRelease()
}

func (w *worker) finishPendingStatusRelease() {
	if w.statusReleaseClientID == 0 {
		return
	}
	if w.controlOp == controlStatus {
		return
	}
	if !w.canReleaseRuntime() || w.client.ID() != w.statusReleaseClientID || len(w.input) != 0 || !w.sessionDurable() {
		w.statusReleaseClientID = 0
		return
	}
	client := w.client
	// Bound event handling so a busy extension cannot starve control commands.
	for range 32 {
		select {
		case raw, open := <-client.Events():
			if !open {
				w.failed()
				return
			}
			w.event(raw)
			if w.client != client || w.statusReleaseClientID != client.ID() || !w.canReleaseRuntime() {
				w.statusReleaseClientID = 0
				return
			}
		default:
			goto drained
		}
	}
	return

drained:
	// Byte credit also covers frames not yet handed to the actor.
	if client.BufferedOutput() {
		return
	}
	w.statusReleaseClientID = 0
	w.releaseRuntimeWithReason(true, "explicit")
}

func statusIdle(language i18n.Language, idle time.Duration) string {
	if idle < 0 {
		return i18n.Text(language, i18n.Msg.Unavailable)
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

func statusBoolText(language i18n.Language, value *bool, yes, no i18n.Message) string {
	if value == nil {
		return i18n.Text(language, i18n.Msg.Unavailable)
	}
	if *value {
		return i18n.Text(language, yes)
	}
	return i18n.Text(language, no)
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
