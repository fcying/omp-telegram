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
	w.replyStatus.header = replyHeader(statusState{}, w.replyStatus.metrics)
	client, connected := w.runtimeClient()
	if !connected || !w.taskActive() || w.preview == "" || w.ctx.Err() != nil {
		w.commitReplyStatus(round)
		return
	}
	w.stopTyping()
	w.resetIdleProbe()
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
	if !ok || sample.request == nil || w.replyStatus.pending != sample.request {
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
		w.replyStatus.header = replyHeader(sample.state, w.replyStatus.metrics)
	}
	w.commitReplyStatus(request.round)
}

func (w *worker) flushReplyStatus() {
	request := w.replyStatus.pending
	if request == nil {
		return
	}
	request.cancel()
	w.replyStatus.pending = nil
	if w.replyStatusMatches(request) {
		w.commitReplyStatus(request.round)
	}
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
	case "prompt_result":
		if e.Status == "completed" {
			return false
		}
	case "session_settled", "available_commands_update", "advisor_cost_changed":
		return false
	}
	w.flushReplyStatus()
	return false
}
