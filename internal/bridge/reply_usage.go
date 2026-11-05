package bridge

import (
	"context"
	"encoding/json"
	"time"
)

const replyUsageTimeout = 3 * time.Second

func (w *worker) beginReplyUsage(request *replyStatusRequest, state statusState) bool {
	metrics := w.replyStatus.metrics
	if !w.b.cfg.ProviderUsage || request.round || w.research.root || w.research.enabled ||
		w.controlInProgress() || w.sessionOperationActive() || w.research.control != nil ||
		w.rpcOperationActive || len(w.rpcOperations) != 0 || len(w.queue) != 0 {
		return false
	}
	if metrics.provider != "anthropic" && metrics.provider != "openai-codex" {
		return false
	}
	if metrics.modelID == "" || metrics.modelID != state.Model.ID || metrics.provider != state.Model.Provider ||
		state.IsStreaming == nil || *state.IsStreaming || state.IsCompacting == nil || *state.IsCompacting ||
		!request.client.CommandCatalog().HasBuiltinCommand("usage") {
		return false
	}
	ctx, cancel := context.WithTimeout(w.ctx, replyUsageTimeout)
	request.usage, request.cancel = true, cancel
	w.replyStatus.pending = request
	w.startOperation("reply_usage", request.client, func(context.Context) (json.RawMessage, error) {
		defer cancel()
		text, err := request.client.Usage(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(replyUsageHeader(text, metrics.provider))
	}, request.active, "", request)
	return true
}

func (w *worker) replyUsageFinished(result operationResult) {
	request, ok := result.meta.(*replyStatusRequest)
	if !ok || request == nil || !request.usage || w.replyStatus.pending != request {
		return
	}
	request.cancel()
	w.replyStatus.pending = nil
	if !w.replyStatusMatches(request) {
		return
	}
	catalog := request.client.CommandCatalog()
	if result.err == nil && catalog.Epoch == request.epoch && catalog.SessionID == request.sessionID && w.binding.SessionID == request.sessionID {
		var suffix string
		err := json.Unmarshal(result.data, &suffix)
		if err == nil && suffix != "" {
			if w.replyStatus.header != "" {
				w.replyStatus.header += compactHeaderSeparator
			}
			w.replyStatus.header += suffix
		}
	}
	w.commitReplyStatus(request.round)
}
