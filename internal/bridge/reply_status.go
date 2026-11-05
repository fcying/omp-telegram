package bridge

import (
	"context"
	"encoding/json"
	"time"

	"omp-telegram/internal/omp"
)

const replyStatusTimeout = time.Second

type replyStatusState struct {
	metrics replyMetrics
	pending *replyStatusRequest
	header  string
}

type replyStatusRequest struct {
	client     *omp.Client
	generation int64
	active     int64
	turn       uint64
	epoch      uint64
	sessionID  string
	round      bool
	usage      bool
	cancel     context.CancelFunc
}

type replyStatusResult struct {
	request *replyStatusRequest
	state   statusState
	valid   bool
}

func (w *worker) resetReplyStatus() {
	if request := w.replyStatus.pending; request != nil {
		request.cancel()
	}
	w.replyStatus = replyStatusState{}
}

func (w *worker) finishReplyWithStatus(round bool) {
	if w.replyStatus.pending != nil {
		return
	}
	w.replyStatus.header = compactHeader(w.b.language(), statusState{}, &w.replyStatus.metrics)
	client, connected := w.runtimeClient()
	if !connected || !w.taskActive() || w.preview == "" || w.ctx.Err() != nil {
		w.commitReplyStatus(round)
		return
	}
	w.stopTyping()
	w.resetIdleProbe()
	w.cancelSteerProbe()
	w.cancelProgressNative()
	catalog := client.CommandCatalog()
	ctx, cancel := context.WithTimeout(w.ctx, replyStatusTimeout)
	request := &replyStatusRequest{
		client: client, generation: w.binding.Generation, active: w.active, turn: w.turn,
		epoch: catalog.Epoch, sessionID: w.binding.SessionID, round: round, cancel: cancel,
	}
	w.replyStatus.pending = request
	w.background.Add(1)
	// This read never owns the mutation lane; controls can flush the confirmed reply.
	go func() {
		defer w.background.Done()
		defer cancel()
		sample := replyStatusResult{request: request}
		raw, err := client.Call(ctx, "get_state", nil)
		sample.valid = err == nil && json.Unmarshal(raw, &sample.state) == nil
		select {
		case w.operations <- operationResult{kind: "reply_status", meta: sample}:
		case <-w.ctx.Done():
		}
	}()
}

func (w *worker) replyStatusMatches(request *replyStatusRequest) bool {
	return w.client == request.client && w.binding.Generation == request.generation && w.active == request.active && w.turn == request.turn
}

func (w *worker) replyStatusFinished(result operationResult) {
	sample, ok := result.meta.(replyStatusResult)
	if !ok || sample.request == nil || sample.request.usage || w.replyStatus.pending != sample.request {
		return
	}
	request := sample.request
	request.cancel()
	w.replyStatus.pending = nil
	if !w.replyStatusMatches(request) {
		return
	}
	catalog := request.client.CommandCatalog()
	if sample.valid && request.sessionID != "" && catalog.Epoch == request.epoch && catalog.SessionID == request.sessionID && w.binding.SessionID == request.sessionID && sample.state.SessionID == request.sessionID {
		sample.state.ConfiguredThinking = w.configuredThinking
		w.replyStatus.header = compactHeader(w.b.language(), sample.state, &w.replyStatus.metrics)
		if w.beginReplyUsage(request, sample.state) {
			return
		}
	}
	w.commitReplyStatus(request.round)
}

func (w *worker) flushReplyStatus() {
	request := w.commitPendingReplyStatus()
	if request != nil && request.usage && w.client == request.client && w.binding.Generation == request.generation && request.client.UsageNeedsRetirement() {
		w.releaseRuntimeWithReason(true, "usage_interrupted")
	}
}

// Runtime teardown commits the answer without recursively releasing its client.
func (w *worker) commitPendingReplyStatus() *replyStatusRequest {
	request := w.replyStatus.pending
	if request == nil {
		return nil
	}
	request.cancel()
	w.replyStatus.pending = nil
	if w.replyStatusMatches(request) {
		w.commitReplyStatus(request.round)
	}
	return request
}

func (w *worker) commitReplyStatus(round bool) {
	if round {
		w.commitResearchRound()
		w.probeResearchSettlement()
		return
	}
	w.finish()
	if !w.taskActive() {
		w.clearSteerFence()
	}
}

func (w *worker) replyStatusEvent(e rpcEvent) bool {
	if w.replyStatus.pending == nil {
		return false
	}
	switch e.Type {
	case "agent_end":
		// A repeated completion cannot replace the reply already awaiting its header.
		return true
	case "command_output":
		// The reader captures our usage report; unrelated receipts retain their own path.
		if w.replyStatus.pending.usage {
			return false
		}
	case "prompt_result":
		if e.Status == "completed" {
			return false
		}
	case "session_settled", "available_commands_update", "advisor_cost_changed", "thinking_level_changed":
		return false
	}
	client := w.client
	w.flushReplyStatus()
	return w.client != client
}
