package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
)

const (
	replyUsageTimeout     = 3 * time.Second
	providerUsageInterval = 5 * time.Minute
)

type providerUsageKey struct {
	provider string
	account  [32]byte
	args     [32]byte
}

type providerUsageEntry struct {
	text string
	next time.Time
}

type providerUsageCache struct {
	sync.Mutex
	entries map[providerUsageKey]providerUsageEntry
}

func (c *providerUsageCache) load(key providerUsageKey) providerUsageEntry {
	c.Lock()
	defer c.Unlock()
	return c.entries[key]
}

func (c *providerUsageCache) claim(key providerUsageKey, now time.Time) bool {
	c.Lock()
	defer c.Unlock()
	entry := c.entries[key]
	if now.Before(entry.next) {
		return false
	}
	if c.entries == nil {
		c.entries = make(map[providerUsageKey]providerUsageEntry)
	}
	entry.next = now.Add(providerUsageInterval)
	c.entries[key] = entry
	return true
}

func (c *providerUsageCache) save(key providerUsageKey, text string) {
	c.Lock()
	defer c.Unlock()
	entry := c.entries[key]
	entry.text = text
	c.entries[key] = entry
}

type providerUsageState struct {
	key            providerUsageKey
	client         *omp.Client
	args           [32]byte
	generation     int64
	epoch          uint64
	sessionID      string
	pending        *providerUsageRequest
	target         int64
	targetProvider string
	baseHeader     string
}

type providerUsageRequest struct {
	client         *omp.Client
	generation     int64
	epoch          uint64
	sessionID      string
	key            providerUsageKey
	target         int64
	targetProvider string
	baseHeader     string
	cancel         context.CancelFunc
	// The operation result channel publishes these fields to the actor.
	text         string
	scopeChanged bool
}

func supportedProviderUsage(provider string) bool {
	return provider == "anthropic" || provider == "openai-codex"
}

func (w *worker) readProviderUsageScope(ctx context.Context, client *omp.Client, state statusState) [32]byte {
	if !w.b.cfg.ProviderUsage || !supportedProviderUsage(state.Model.Provider) {
		return [32]byte{}
	}
	scope, _ := client.ProviderAccountScope(ctx, state.Model.Provider)
	return scope
}

func (w *worker) resetProviderUsage(args []string) {
	w.cancelProviderUsage()
	encoded, _ := json.Marshal(args)
	w.providerUsage = providerUsageState{args: sha256.Sum256(encoded)}
}

func (w *worker) setProviderUsageScope(provider string, account [32]byte) {
	u := &w.providerUsage
	u.key = providerUsageKey{}
	if !w.b.cfg.ProviderUsage || !supportedProviderUsage(provider) || account == ([32]byte{}) || w.client == nil {
		return
	}
	catalog := w.client.CommandCatalog()
	u.key = providerUsageKey{provider: provider, account: account, args: u.args}
	u.client, u.generation, u.epoch, u.sessionID = w.client, w.binding.Generation, catalog.Epoch, w.binding.SessionID
}

func (w *worker) providerUsageMatches(client *omp.Client, generation int64, epoch uint64, sessionID string) bool {
	if client == nil || w.client != client || w.binding.Generation != generation || sessionID == "" || w.binding.SessionID != sessionID {
		return false
	}
	catalog := client.CommandCatalog()
	return catalog.Epoch == epoch && catalog.SessionID == sessionID
}

func (w *worker) withProviderUsage(header, provider string, readCompleted bool) string {
	if !w.b.cfg.ProviderUsage || !readCompleted {
		return header
	}
	suffix := w.tr(i18n.Msg.ProviderUsageUnavailable)
	u := &w.providerUsage
	if u.key.provider == provider && w.providerUsageMatches(u.client, u.generation, u.epoch, u.sessionID) {
		entry := w.b.providerUsage.load(u.key)
		if entry.text != "" {
			suffix = entry.text
		}
	}
	if header != "" {
		header += compactHeaderSeparator
	}
	return header + suffix
}

// Refresh only an already identified idle runtime. Rendering never waits for it.
func (w *worker) refreshProviderUsage(now time.Time) {
	u := &w.providerUsage
	if !w.b.cfg.ProviderUsage || u.pending != nil || u.key.provider == "" ||
		!w.providerUsageMatches(u.client, u.generation, u.epoch, u.sessionID) ||
		w.taskActive() || w.sessionControlBusy() || w.awaitingContinuation || w.steerFence.active ||
		w.research.enabled || w.research.disablePending || w.restoring || w.runtimeResuming ||
		w.rpcOperationActive || len(w.rpcOperations) != 0 || len(w.queue) != 0 || len(w.input) != 0 ||
		!u.client.CommandCatalog().HasBuiltinCommand("usage") {
		return
	}
	claimed := w.b.providerUsage.claim(u.key, now)
	if !claimed {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, replyUsageTimeout)
	request := &providerUsageRequest{client: u.client, generation: u.generation, epoch: u.epoch,
		sessionID: u.sessionID, key: u.key, target: u.target, targetProvider: u.targetProvider,
		baseHeader: u.baseHeader, cancel: cancel}
	u.pending = request
	w.startOperation("reply_usage", request.client, func(context.Context) (json.RawMessage, error) {
		defer cancel()
		raw, err := request.client.Call(ctx, "get_state", nil)
		if err != nil {
			return nil, err
		}
		var state statusState
		if json.Unmarshal(raw, &state) != nil || state.SessionID != request.sessionID || state.Model.Provider != request.key.provider ||
			state.IsStreaming == nil || *state.IsStreaming || state.IsCompacting == nil || *state.IsCompacting {
			return nil, errors.New("provider usage runtime unavailable")
		}
		account, err := request.client.ProviderAccountScope(ctx, request.key.provider)
		if err != nil || account != request.key.account {
			request.scopeChanged = true
			return nil, errors.New("provider usage account unavailable")
		}
		text, err := request.client.Usage(ctx)
		if err != nil {
			return nil, err
		}
		account, err = request.client.ProviderAccountScope(ctx, request.key.provider)
		if err != nil || account != request.key.account {
			request.scopeChanged = true
			return nil, errors.New("provider usage account changed")
		}
		request.text = replyUsageHeader(text, request.key.provider)
		return nil, nil
	}, 0, "", request)
}

func (w *worker) replyUsageFinished(result operationResult) {
	request, ok := result.meta.(*providerUsageRequest)
	if !ok || request == nil || w.providerUsage.pending != request {
		return
	}
	request.cancel()
	w.providerUsage.pending = nil
	if !w.providerUsageMatches(request.client, request.generation, request.epoch, request.sessionID) {
		return
	}
	if request.scopeChanged {
		w.providerUsage.key = providerUsageKey{}
		return
	}
	if result.err != nil || request.text == "" || request.client.Stopping() {
		return
	}
	w.b.providerUsage.save(request.key, request.text)
	if request.target != 0 && request.targetProvider == request.key.provider && w.providerUsage.target == request.target && w.providerUsage.key == request.key {
		header := w.withProviderUsage(request.baseHeader, request.key.provider, true)
		err := w.b.db.SetAssistantHeader(w.ctx, request.target, w.assistantHeader(header, true))
		if err != nil && w.ctx.Err() == nil {
			w.b.storeLog.Error("provider usage header update failed", "event", "assistant_header_update_failed", "error_kind", "persistence")
		}
	}
}

func (w *worker) cancelProviderUsage() {
	if request := w.providerUsage.pending; request != nil {
		request.cancel()
		w.providerUsage.pending = nil
	}
}

func (w *worker) flushProviderUsage() {
	request := w.providerUsage.pending
	if request == nil {
		return
	}
	w.cancelProviderUsage()
	if w.client == request.client && w.binding.Generation == request.generation && request.client.UsageNeedsRetirement() {
		w.releaseRuntimeWithReason(true, "usage_interrupted")
	}
}
