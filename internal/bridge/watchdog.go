package bridge

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"omp-telegram/internal/omp"
)

const stuckTaskQuietPeriod = 30 * time.Second
const idleProbeTimeout = 5 * time.Second

type idleProbeState struct {
	revision  uint64
	cancel    context.CancelFunc
	results   chan idleProbeResult
	confirmed time.Time
	next      time.Time
}

type idleProbeResult struct {
	client     *omp.Client
	generation int64
	turn       uint64
	active     int64
	revision   uint64
	idle       bool
	err        error
}

func (w *worker) stopTyping() {
	if w.typingCancel != nil {
		w.typingCancel()
		w.typingCancel = nil
	}
	w.lastTyping = time.Time{}
}

func (w *worker) resetIdleProbe() {
	if w.idleProbe.cancel != nil {
		w.idleProbe.cancel()
		w.idleProbe.cancel = nil
	}
	w.idleProbe.revision++
	w.idleProbe.confirmed = time.Time{}
	w.idleProbe.next = time.Time{}
}

func (w *worker) stuckTaskEligible() bool {
	if w.runtime != runtimeConnected || w.client == nil || w.active == 0 || !w.busy || w.awaitingContinuation || w.compacting || w.finishing || w.progress.Retrying || len(w.progress.ActiveTools) != 0 || len(w.hostRequests) != 0 {
		return false
	}
	for _, c := range w.confirms {
		if c.action == "ui" {
			return false
		}
	}
	return true
}

func explicitlyIdle(raw json.RawMessage) bool {
	var state struct {
		Streaming  *bool `json:"isStreaming"`
		Compacting *bool `json:"isCompacting"`
	}
	return json.Unmarshal(raw, &state) == nil && state.Streaming != nil && !*state.Streaming && state.Compacting != nil && !*state.Compacting
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
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer cancel()
		raw, err := result.client.Call(ctx, "get_state", nil)
		result.err = err
		result.idle = err == nil && explicitlyIdle(raw)
		select {
		case results <- result:
		case <-w.ctx.Done():
		}
	}()
}

func (w *worker) idleProbeFinished(result idleProbeResult, now time.Time) {
	if result.client != w.client || result.generation != w.binding.Generation || result.turn != w.turn || result.active != w.active || result.revision != w.idleProbe.revision {
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
	if w.idleProbe.cancel != nil {
		w.idleProbe.cancel()
		w.idleProbe.cancel = nil
	}
	if !w.stuckTaskEligible() || result.err != nil || !result.idle {
		w.resetIdleProbe()
		w.idleProbe.next = now.Add(stuckTaskQuietPeriod)
		return
	}
	if w.idleProbe.confirmed.IsZero() {
		w.idleProbe.confirmed = now
		w.idleProbe.next = now.Add(stuckTaskQuietPeriod)
		return
	}
	if now.Sub(w.idleProbe.confirmed) < stuckTaskQuietPeriod {
		return
	}
	log.Printf("bridge lifecycle client=%p event=idle_completion_missing active=%d busy=%t turn=%d generation=%d", w.client, w.active, w.busy, w.turn, w.binding.Generation)
	w.finishUncertain("omp is idle but no confirmed terminal result was received. The task outcome is uncertain and will not be replayed automatically.")
	if w.active != 0 {
		return // The durable transaction failed; never dispatch another task.
	}
	// Retire before dispatch: late terminal events can never belong to the next task.
	// Keep the logical session claim and queue so normal dispatch lazily resumes.
	w.closeRuntime()
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

func (w *worker) logLifecycle(e rpcEvent) {
	switch e.Type {
	case "agent_start", "agent_end", "prompt_result", "auto_compaction_start", "auto_compaction_end":
	case "response":
		if e.Success {
			return
		}
	default:
		return
	}
	log.Printf("bridge lifecycle client=%p phase=received event=%s terminal=%s agent_invoked=%s active=%d busy=%t turn=%d generation=%d", w.client, e.Type, terminalFlag(e.IsTerminal), terminalFlag(e.AgentInvoked), w.active, w.busy, w.turn, w.binding.Generation)
}
