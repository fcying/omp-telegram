package bridge

import (
	"context"
	"encoding/json"
	"time"

	"omp-telegram/internal/omp"
)

const replyStatusTimeout = time.Second

type replyStatusState struct {
	metrics             replyMetrics
	pending             *replyStatusRequest
	header              string
	roundStatusOutboxID int64
	roundStatusHeader   string
	liveState           statusState
	liveScope           *progressNativeRequest
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
	account [32]byte
}

func (w *worker) resetReplyStatus() {
	if request := w.replyStatus.pending; request != nil {
		request.cancel()
	}
	w.replyStatus = replyStatusState{}
}

// Streamed text without a completed message has no confirmed reply metrics.
func (w *worker) unconfirmedAssistantHeader() string {
	readCompleted := w.progressContextReadCompleted()
	header := ""
	if readCompleted {
		header = compactUnknownContext
	}
	provider := w.replyStatus.metrics.provider
	if provider == "" {
		provider = w.replyStatus.liveState.Model.Provider
	}
	return w.withProviderUsage(header, provider, readCompleted)
}

// Reuse only a validated, same-run status projection. No formatted status is
// parsed, and intermediate delivery never starts another native/quota query.
func (w *worker) intermediateAssistantHeader() string {
	metrics := &w.replyStatus.metrics
	if metrics.modelID == "" {
		return w.unconfirmedAssistantHeader()
	}
	state := statusState{}
	if w.progressNativeMatches(w.replyStatus.liveScope) {
		state = w.replyStatus.liveState
	}
	if state.Model.ID == "" {
		state = statusState{Model: omp.Model{ID: metrics.modelID, Provider: metrics.provider}}
	}
	state.ConfiguredThinking = w.configuredThinking
	readCompleted := w.progressContextReadCompleted()
	header := compactMessageHeader(w.b.language(), state, metrics, readCompleted)
	return w.withProviderUsage(header, metrics.provider, readCompleted)
}

func (w *worker) finishReplyWithStatus(round bool) {
	if w.replyStatus.pending != nil {
		return
	}
	readCompleted := w.progressContextReadCompleted()
	w.replyStatus.header = formatCompactHeader(w.b.language(), statusState{}, &w.replyStatus.metrics, false, readCompleted)
	w.providerUsage.baseHeader = w.replyStatus.header
	w.replyStatus.header = w.withProviderUsage(w.replyStatus.header, w.replyStatus.metrics.provider, readCompleted)
	client, connected := w.runtimeClient()
	if !connected || !w.taskActive() || !w.hasAssistantBody() || w.ctx.Err() != nil {
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
		if sample.valid && sample.state.SessionID == request.sessionID {
			sample.account = w.readProviderUsageScope(ctx, client, sample.state)
		}
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
	// A stale response is not evidence that this scope's context was read.
	w.replyStatus.header = formatCompactHeader(w.b.language(), statusState{}, &w.replyStatus.metrics, false, false)
	readCompleted := false
	if request.sessionID != "" && catalog.Epoch == request.epoch && catalog.SessionID == request.sessionID && w.binding.SessionID == request.sessionID {
		if !sample.valid {
			readCompleted = true
			w.setProviderUsageScope("", [32]byte{})
			w.replyStatus.header = compactHeader(w.b.language(), statusState{}, &w.replyStatus.metrics)
		} else if sample.state.SessionID == request.sessionID {
			readCompleted = true
			sample.state.ConfiguredThinking = w.configuredThinking
			w.setProviderUsageScope(sample.state.Model.Provider, sample.account)
			w.replyStatus.header = compactHeader(w.b.language(), sample.state, &w.replyStatus.metrics)
		}
	}
	w.providerUsage.baseHeader = w.replyStatus.header
	w.replyStatus.header = w.withProviderUsage(w.replyStatus.header, w.replyStatus.metrics.provider, readCompleted)
	w.commitReplyStatus(request.round)
	w.refreshProviderUsage(time.Now())
}

func (w *worker) flushReplyStatus() {
	w.commitPendingReplyStatus()
	w.flushProviderUsage()
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
		if w.commitResearchRound() {
			w.probeResearchSettlement()
		}
		return
	}
	w.finish()
	if !w.taskActive() {
		w.clearSteerFence()
	}
}

func (w *worker) replyStatusEvent(e rpcEvent) bool {
	if w.providerUsage.pending != nil && (assistantContinuationEvent(e) || e.Type == "auto_compaction_start") {
		client := w.client
		w.flushProviderUsage()
		if w.client != client {
			return true
		}
	}
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
	case "extension_ui_request":
		if !assistantContinuationEvent(e) {
			return false
		}
	case "session_settled", "available_commands_update", "advisor_cost_changed", "thinking_level_changed":
		return false
	}
	client := w.client
	w.flushReplyStatus()
	return w.client != client
}
