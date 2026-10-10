package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"omp-telegram/internal/omp"
)

const stuckTaskQuietPeriod = 30 * time.Second
const idleProbeTimeout = 5 * time.Second
const nativeSteerIdleLimit = 5 * time.Minute

type idleProbeState struct {
	revision     uint64
	cancel       context.CancelFunc
	results      chan idleProbeResult
	confirmed    time.Time
	next         time.Time
	pendingSince time.Time
}

type idleProbeResult struct {
	client        *omp.Client
	generation    int64
	turn          uint64
	active        int64
	revision      uint64
	idle          bool
	settled       bool
	pendingNative bool
	err           error
}

func (w *worker) stopTyping() {
	if w.typingCancel != nil {
		w.typingCancel()
		w.typingCancel = nil
	}
	w.lastTyping = time.Time{}
}

func (w *worker) resetIdleProbe() {
	w.resetIdleProbeReason("activity")
}

func (w *worker) resetIdleProbeReason(reason string) {
	evidence := w.idleProbe.cancel != nil || !w.idleProbe.confirmed.IsZero() || !w.idleProbe.next.IsZero()
	generation, turn, inboxID := w.binding.Generation, w.turn, w.active
	var clientID uint64
	if w.client != nil {
		clientID = w.client.ID()
	}
	if w.idleProbe.cancel != nil {
		w.idleProbe.cancel()
		w.idleProbe.cancel = nil
	}
	w.idleProbe.revision++
	w.idleProbe.confirmed = time.Time{}
	w.idleProbe.next = time.Time{}
	w.idleProbe.pendingSince = time.Time{}
	if evidence && w.log.Enabled(context.Background(), slog.LevelDebug) {
		attrs := []slog.Attr{
			slog.String("event", "watchdog_probe_reset"),
			slog.String("reason", reason),
			slog.Int64("generation", generation),
			slog.Uint64("turn", turn),
			slog.Int64("inbox_id", inboxID),
		}
		if clientID != 0 {
			attrs = append(attrs, slog.Uint64("client_id", clientID))
		}
		w.log.LogAttrs(context.Background(), slog.LevelDebug, "watchdog reset", attrs...)
	}
}
func (w *worker) stuckTaskEligible() bool {
	if w.research.root {
		return false
	}
	if w.runtime != runtimeConnected || w.client == nil || !w.taskRunning() || w.awaitingContinuation && !w.steerFence.active || w.compacting || w.finishing || w.progress.Retrying || len(w.progress.ActiveTools) != 0 || len(w.hostRequests) != 0 {
		return false
	}
	for _, c := range w.confirms {
		if c.action == "ui" {
			return false
		}
	}
	return true
}

func rpcIdleState(raw json.RawMessage) (idle, settled, pendingNative bool) {
	var state struct {
		Streaming    *bool `json:"isStreaming"`
		Compacting   *bool `json:"isCompacting"`
		Settled      *bool `json:"isSettled"`
		PendingAsync *bool `json:"hasPendingAsyncWork"`
		Queued       *int  `json:"queuedMessageCount"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return false, false, false
	}
	idle = state.Streaming != nil && !*state.Streaming && state.Compacting != nil && !*state.Compacting
	settled = state.Settled != nil && *state.Settled
	pendingNative = state.PendingAsync != nil && *state.PendingAsync || state.Queued != nil && *state.Queued > 0
	return idle, settled, pendingNative
}

func (w *worker) probeStuckTask(now time.Time) {
	if !w.stuckTaskEligible() {
		w.resetIdleProbe()
		return
	}
	if w.idleProbe.cancel != nil || w.lastActivity.IsZero() || now.Sub(w.lastActivity) < stuckTaskQuietPeriod || now.Before(w.idleProbe.next) {
		return
	}
	if w.idleProbe.results == nil {
		w.idleProbe.results = make(chan idleProbeResult, 1)
	}
	ctx, cancel := context.WithTimeout(w.ctx, idleProbeTimeout)
	w.idleProbe.cancel = cancel
	result := idleProbeResult{client: w.client, generation: w.binding.Generation, turn: w.turn, active: w.active, revision: w.idleProbe.revision}
	results := w.idleProbe.results
	if w.log.Enabled(context.Background(), slog.LevelDebug) {
		w.log.LogAttrs(context.Background(), slog.LevelDebug, "watchdog probe", slog.String("event", "watchdog_probe"), slog.String("phase", "start"), slog.Int64("generation", result.generation), slog.Uint64("turn", result.turn), slog.Int64("inbox_id", result.active), slog.Uint64("client_id", result.client.ID()))
	}
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer cancel()
		raw, err := result.client.Call(ctx, "get_state", nil)
		result.err = err
		if err == nil {
			result.idle, result.settled, result.pendingNative = rpcIdleState(raw)
		}
		select {
		case results <- result:
		case <-w.ctx.Done():
		}
	}()
}

func (w *worker) idleProbeFinished(result idleProbeResult, now time.Time) {
	if result.client != w.client || result.generation != w.binding.Generation || result.turn != w.turn || result.active != w.active || result.revision != w.idleProbe.revision {
		if w.log.Enabled(context.Background(), slog.LevelDebug) {
			attrs := []slog.Attr{
				slog.String("event", "watchdog_probe"),
				slog.String("phase", "ignored"),
				slog.String("reason", "stale_result"),
				slog.Int64("generation", result.generation),
				slog.Uint64("turn", result.turn),
				slog.Int64("inbox_id", result.active),
			}
			if result.client != nil {
				attrs = append(attrs, slog.Uint64("client_id", result.client.ID()))
			}
			w.log.LogAttrs(context.Background(), slog.LevelDebug, "watchdog stale result", attrs...)
		}
		return
	}
	// A queued lifecycle event takes precedence over an independently resolved RPC.
	select {
	case raw, ok := <-result.client.Events():
		if !ok {
			w.failed()
		} else {
			w.event(raw)
		}
		return
	default:
	}
	if result.client.BufferedOutput() {
		w.resetIdleProbeReason("queued_rpc_output")
		return
	}
	if w.steerFence.active && result.err != nil {
		w.retireSteeredRoot("Steering state could not be confirmed. The task outcome is uncertain and will not be replayed automatically.")
		return
	}
	if !w.stuckTaskEligible() || result.err != nil || !result.idle {
		w.resetIdleProbeReason("probe_not_idle")
		w.idleProbe.next = now.Add(stuckTaskQuietPeriod)
		return
	}
	if w.steerFence.active && result.pendingNative {
		// Pending native work gets a longer grace period, not an unlimited reset.
		if w.idleProbe.cancel != nil {
			w.idleProbe.cancel()
			w.idleProbe.cancel = nil
		}
		w.idleProbe.confirmed = time.Time{}
		w.idleProbe.next = now.Add(stuckTaskQuietPeriod)
		if w.idleProbe.pendingSince.IsZero() {
			w.idleProbe.pendingSince = now
		} else if now.Sub(w.idleProbe.pendingSince) >= nativeSteerIdleLimit {
			w.retireSteeredRoot("omp's native steering queue remained idle too long. The task outcome is uncertain and will not be replayed automatically.")
		}
		return
	}
	w.idleProbe.pendingSince = time.Time{}
	if w.steerFence.active && result.settled && len(w.steers) == 0 && w.steerFence.terminalSeen {
		w.steerFence.settled = true
		w.maybeFinishSteeredRoot()
		return
	}
	if w.idleProbe.cancel != nil {
		w.idleProbe.cancel()
		w.idleProbe.cancel = nil
	}
	if w.log.Enabled(context.Background(), slog.LevelDebug) {
		w.log.LogAttrs(context.Background(), slog.LevelDebug, "watchdog idle confirmed", slog.String("event", "watchdog_probe"), slog.String("phase", "confirmed"), slog.Int64("generation", result.generation), slog.Uint64("turn", result.turn), slog.Int64("inbox_id", result.active), slog.Uint64("client_id", result.client.ID()))
	}
	if w.idleProbe.confirmed.IsZero() {
		w.idleProbe.confirmed = now
		w.idleProbe.next = now.Add(stuckTaskQuietPeriod)
		return
	}
	if now.Sub(w.idleProbe.confirmed) < stuckTaskQuietPeriod {
		return
	}
	if w.steerFence.active {
		w.retireSteeredRoot("omp is idle but steering completion could not be confirmed. The task outcome is uncertain and will not be replayed automatically.")
		return
	}
	inboxID, generation, turn := w.active, w.binding.Generation, w.turn
	clientID := uint64(0)
	if w.client != nil {
		clientID = w.client.ID()
	}
	if w.finishUncertain("omp is idle but no confirmed terminal result was received. The task outcome is uncertain and will not be replayed automatically.") {
		attrs := []slog.Attr{
			slog.String("event", "watchdog_recover"),
			slog.String("reason", "idle_completion_missing"),
			slog.String("result", "uncertain"),
			slog.Bool("replay", false),
			slog.Int64("generation", generation),
			slog.Uint64("turn", turn),
			slog.Int64("inbox_id", inboxID),
		}
		if clientID != 0 {
			attrs = append(attrs, slog.Uint64("client_id", clientID))
		}
		w.log.LogAttrs(context.Background(), slog.LevelWarn, "watchdog recovered uncertain task", attrs...)
	} else {
		return // The durable transaction failed; never dispatch another task.
	}
	// Retire before dispatch: late terminal events can never belong to the next task.
	// Keep the logical session claim and queue so normal dispatch lazily resumes.
	w.releaseRuntimeWithReason(true, "failure")
}
func terminalFlag(flag *bool) string {
	if flag == nil {
		return "absent"
	}
	if *flag {
		return "true"
	}
	return "false"
}

func numericRequestID(id string) (uint64, bool) {
	if id == "" {
		return 0, false
	}
	requestID, err := strconv.ParseUint(id, 10, 64)
	return requestID, err == nil
}

func (w *worker) logLifecycle(e rpcEvent) {
	if !w.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	switch e.Type {
	case "agent_start", "agent_end", "prompt_result", "auto_compaction_start", "auto_compaction_end":
	case "response":
		if e.Success {
			return
		}
	default:
		return
	}
	attrs := []slog.Attr{
		slog.String("event", "rpc_lifecycle"),
		slog.String("rpc_event", e.Type),
		slog.String("phase", "handled"),
		slog.String("terminal", terminalFlag(e.IsTerminal)),
		slog.String("agent_invoked", terminalFlag(e.AgentInvoked)),
		slog.Int64("generation", w.binding.Generation),
		slog.Uint64("turn", w.turn),
		slog.Int64("inbox_id", w.active),
	}
	if w.client != nil {
		attrs = append(attrs, slog.Uint64("client_id", w.client.ID()))
	}
	requestID, valid := numericRequestID(e.ID)
	attrs = append(attrs, slog.Bool("request_id_valid", valid))
	if valid {
		attrs = append(attrs, slog.Uint64("request_id", requestID))
	}
	w.log.LogAttrs(context.Background(), slog.LevelDebug, "rpc lifecycle handled", attrs...)
	if e.Type == "agent_end" && e.IsTerminal != nil && !*e.IsTerminal {
		asyncAttrs := []slog.Attr{
			slog.String("event", "watchdog_async_wait"),
			slog.Int64("generation", w.binding.Generation),
			slog.Uint64("turn", w.turn),
			slog.Int64("inbox_id", w.active),
		}
		if w.client != nil {
			asyncAttrs = append(asyncAttrs, slog.Uint64("client_id", w.client.ID()))
		}
		w.log.LogAttrs(context.Background(), slog.LevelDebug, "watchdog async wait", asyncAttrs...)
	}
}
