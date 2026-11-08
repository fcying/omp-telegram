package bridge

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"omp-telegram/internal/omp"
)

const replyStatusTimeout = time.Second

type replyStatusState struct {
	metrics             replyMetrics
	roundMetrics        replyMetrics
	pending             *replyStatusRequest
	header              string
	prepared            bool
	provider            string
	roundStatusOutboxID int64
	roundSub2apiUsage   sub2apiUsageState
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
	workspace  string
	native     bool
	cancel     context.CancelFunc
}

type replyStatusResult struct {
	request *replyStatusRequest
	state   statusState
	valid   bool
	account [32]byte
	git     string
}

func (w *worker) resetReplyStatus() {
	if request := w.replyStatus.pending; request != nil {
		request.cancel()
	}
	w.replyStatus = replyStatusState{}
}

// Streamed text without a completed message has no confirmed reply metrics.
func (w *worker) unconfirmedAssistantHeader() string {
	fields := compactStatusFields{}
	provider := w.replyStatus.metrics.provider
	if provider == "" {
		provider = w.replyStatus.liveState.Model.Provider
	}
	fields.quota = w.providerUsageText(provider, w.progressContextReadCompleted())
	return w.renderStatusBar("intermediate", fields)
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
	fields := compactFields(w.b.language(), state, metrics, true, readCompleted)
	fields.quota = w.providerUsageText(metrics.provider, readCompleted)
	return w.renderStatusBar("intermediate", fields)
}

func (w *worker) finalReplyMetrics(round bool) *replyMetrics {
	if !round && w.research.root {
		return &w.replyStatus.roundMetrics
	}
	return &w.replyStatus.metrics
}

func (w *worker) prepareReplyStatus(round bool, fields compactStatusFields, readCompleted bool) {
	kind := "final"
	if round {
		kind = "intermediate"
	}
	provider := w.finalReplyMetrics(round).provider
	w.replyStatus.provider = provider
	w.replyStatus.prepared = true
	w.providerUsage.baseFields = fields
	fields.quota = w.providerUsageText(provider, readCompleted)
	if !round && w.research.root && w.b.cfg.Sub2API.HasProvider(provider) {
		fields.quota = ""
		if readCompleted {
			fields.quota = w.replyStatus.roundSub2apiUsage.sub2apiQuotaSnapshot.text(time.Now())
		}
	}
	w.replyStatus.header = w.renderStatusBar(kind, fields)
}

func (w *worker) finishReplyWithStatus(round bool) {
	if w.replyStatus.pending != nil {
		return
	}
	fields := compactFields(w.b.language(), statusState{}, w.finalReplyMetrics(round), false, false)
	w.prepareReplyStatus(round, fields, w.progressContextReadCompleted())
	client, connected := w.runtimeClient()
	kind := "final"
	if round {
		kind = "intermediate"
	}
	native := connected && w.statusNeedsNative(kind)
	git := !round && w.b.cfg.StatusBar.Has("final", "git")
	hasBody := w.hasAssistantBody() || w.research.root && w.replyStatus.roundStatusOutboxID != 0
	if !w.taskActive() || !hasBody || w.ctx.Err() != nil || !native && !git {
		w.commitReplyStatus(round)
		w.refreshProviderUsage(time.Now())
		return
	}
	w.stopTyping()
	w.resetIdleProbe()
	w.cancelSteerProbe()
	w.cancelProgressNative()
	var epoch uint64
	if connected {
		epoch = client.CommandCatalog().Epoch
	}
	ctx, cancel := context.WithCancel(w.ctx)
	request := &replyStatusRequest{
		client: client, generation: w.binding.Generation, active: w.active, turn: w.turn,
		epoch: epoch, sessionID: w.binding.SessionID, round: round, cancel: cancel,
		workspace: w.binding.Workspace, native: native,
	}
	w.replyStatus.pending = request
	var environment []string
	if git {
		environment = omp.ChildEnv(w.b.cfg.OMPEnvironment)
	}
	quota := w.b.cfg.StatusBar.Has(kind, "quota")
	w.background.Add(1)
	// This read never owns the mutation lane; controls can flush the confirmed reply.
	go func() {
		defer w.background.Done()
		defer cancel()
		sample := replyStatusResult{request: request}
		var probes sync.WaitGroup
		if git {
			probes.Add(1)
			go func() {
				defer probes.Done()
				sample.git = readReplyGitField(ctx, request.workspace, environment, w.b.cfg.StatusBar.GitCommand, w.b.cfg.StatusBar.GitTimeout)
			}()
		}
		if native {
			nativeCtx, nativeCancel := context.WithTimeout(ctx, replyStatusTimeout)
			defer nativeCancel()
			raw, err := client.Call(nativeCtx, "get_state", nil)
			sample.valid = err == nil && json.Unmarshal(raw, &sample.state) == nil
			if quota && sample.valid && sample.state.SessionID == request.sessionID {
				sample.account = w.readProviderUsageScope(nativeCtx, client, sample.state)
			}
		}
		probes.Wait()
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
	fields := compactFields(w.b.language(), statusState{}, w.finalReplyMetrics(request.round), false, false)
	readCompleted := false
	current := w.binding.SessionID == request.sessionID && w.binding.Workspace == request.workspace
	if request.client != nil {
		catalog := request.client.CommandCatalog()
		current = current && catalog.Epoch == request.epoch && catalog.SessionID == request.sessionID && request.sessionID != ""
	}
	if current {
		if request.native && !sample.valid {
			w.setProviderUsageScope("", [32]byte{})
		} else if sample.valid && sample.state.SessionID == request.sessionID {
			readCompleted = true
			sample.state.ConfiguredThinking = w.configuredThinking
			w.setProviderUsageScope(sample.state.Model.Provider, sample.account)
			fields = compactFields(w.b.language(), sample.state, w.finalReplyMetrics(request.round), false, true)
		}
		if sample.git != "" {
			fields.git = progressToolLiteral(sample.git)
		}
	}
	w.prepareReplyStatus(request.round, fields, readCompleted)
	w.commitReplyStatus(request.round)
	w.refreshProviderUsage(time.Now())
}

func (w *worker) flushReplyStatus() {
	w.commitPendingReplyStatus()
	w.cancelSub2apiUsage()
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
	research := w.research.root
	w.finish()
	if !w.taskActive() {
		if research {
			w.invalidateResearchSettlement()
		}
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
