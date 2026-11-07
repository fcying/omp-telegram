package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
	"time"

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
	sample quotaSample
	next   time.Time
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

func (c *providerUsageCache) save(key providerUsageKey, sample quotaSample) {
	c.Lock()
	defer c.Unlock()
	entry := c.entries[key]
	entry.sample = sample
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
	nextScope      time.Time
	target         int64
	targetProvider string
	baseFields     compactStatusFields
}

type providerUsageRequest struct {
	client         *omp.Client
	generation     int64
	epoch          uint64
	sessionID      string
	key            providerUsageKey
	target         int64
	targetProvider string
	baseFields     compactStatusFields
	cancel         context.CancelFunc
	scopeOnly      bool
	// The operation result channel publishes these fields to the actor.
	sample       quotaSample
	scopeChanged bool
}

func supportedProviderUsage(provider string) bool {
	return provider == "anthropic" || provider == "openai-codex"
}

func (w *worker) providerUsageEnabled() bool {
	return w.b.cfg.StatusBar.Has("intermediate", "quota") || w.b.cfg.StatusBar.Has("final", "quota") ||
		w.b.cfg.ProgressMode != "off" && w.b.cfg.StatusBar.Has("progress", "quota")
}

func (w *worker) readProviderUsageScope(ctx context.Context, client *omp.Client, state statusState) [32]byte {
	if !w.providerUsageEnabled() || w.b.cfg.Sub2API.HasProvider(state.Model.Provider) || !supportedProviderUsage(state.Model.Provider) {
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
	if provider != "" && w.sub2api.provider != "" && w.sub2api.provider != provider {
		w.resetSub2apiUsage()
	}
	u := &w.providerUsage
	u.key = providerUsageKey{}
	if w.providerUsageEnabled() && w.b.cfg.Sub2API.HasProvider(provider) {
		u.key.provider = provider
		return
	}
	if !w.providerUsageEnabled() || !supportedProviderUsage(provider) || account == ([32]byte{}) || w.client == nil {
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

func (w *worker) providerUsageText(provider string, readCompleted bool) string {
	if !w.providerUsageEnabled() || !readCompleted {
		return ""
	}
	if w.b.cfg.Sub2API.HasProvider(provider) {
		return w.sub2apiUsageText(provider)
	}
	u := &w.providerUsage
	if u.key.provider == provider && w.providerUsageMatches(u.client, u.generation, u.epoch, u.sessionID) {
		return w.b.providerUsage.load(u.key).sample.text(time.Now())
	}
	return ""
}

// Refresh only an already connected idle runtime. Rendering never waits for it.
func (w *worker) refreshProviderUsage(now time.Time) {
	w.refreshSub2apiUsage(now)
	if w.b.cfg.Sub2API.HasProvider(w.sub2api.provider) || w.b.cfg.Sub2API.HasProvider(w.providerUsage.key.provider) {
		return
	}
	u := &w.providerUsage
	if !w.providerUsageEnabled() || u.pending != nil ||
		w.taskActive() || w.sessionControlBusy() || w.awaitingContinuation || w.steerFence.active ||
		w.research.enabled || w.research.disablePending || w.restoring || w.runtimeResuming ||
		w.rpcOperationActive || len(w.rpcOperations) != 0 || len(w.queue) != 0 || len(w.input) != 0 {
		return
	}
	client, connected := w.runtimeClient()
	if !connected || !client.CommandCatalog().HasBuiltinCommand("usage") {
		return
	}
	if u.key.provider == "" || !w.providerUsageMatches(u.client, u.generation, u.epoch, u.sessionID) {
		w.discoverProviderUsageScope(client, now)
		return
	}
	claimed := w.b.providerUsage.claim(u.key, now)
	if !claimed {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, replyUsageTimeout)
	request := &providerUsageRequest{client: u.client, generation: u.generation, epoch: u.epoch,
		sessionID: u.sessionID, key: u.key, target: u.target, targetProvider: u.targetProvider,
		baseFields: u.baseFields, cancel: cancel}
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
		sampledAt := time.Now()
		account, err = request.client.ProviderAccountScope(ctx, request.key.provider)
		if err != nil || account != request.key.account {
			request.scopeChanged = true
			return nil, errors.New("provider usage account changed")
		}
		request.sample = replyUsageSample(text, request.key.provider, sampledAt)
		return nil, nil
	}, 0, "", request)
}

// Headers may omit quota at completion. Discover their shared cache scope only
// after settlement, so short tasks with quota selected elsewhere can still warm it.
func (w *worker) discoverProviderUsageScope(client *omp.Client, now time.Time) {
	u := &w.providerUsage
	catalog := client.CommandCatalog()
	if now.Before(u.nextScope) || catalog.SessionID == "" || catalog.SessionID != w.binding.SessionID {
		return
	}
	u.nextScope = now.Add(progressNativeInterval)
	ctx, cancel := context.WithTimeout(w.ctx, replyStatusTimeout)
	request := &providerUsageRequest{client: client, generation: w.binding.Generation,
		epoch: catalog.Epoch, sessionID: w.binding.SessionID, scopeOnly: true, cancel: cancel}
	u.pending = request
	w.startOperation("reply_usage", client, func(context.Context) (json.RawMessage, error) {
		defer cancel()
		raw, err := client.Call(ctx, "get_state", nil)
		if err != nil {
			return nil, err
		}
		var state statusState
		if json.Unmarshal(raw, &state) != nil || state.SessionID != request.sessionID ||
			state.IsStreaming == nil || *state.IsStreaming || state.IsCompacting == nil || *state.IsCompacting {
			return nil, errors.New("provider usage runtime unavailable")
		}
		account := w.readProviderUsageScope(ctx, client, state)
		if account != ([32]byte{}) {
			request.key.provider, request.key.account = state.Model.Provider, account
		}
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
	if request.scopeOnly {
		if result.err == nil && request.key.account != ([32]byte{}) {
			w.setProviderUsageScope(request.key.provider, request.key.account)
			w.refreshProviderUsage(time.Now())
		}
		return
	}
	if request.scopeChanged {
		w.providerUsage.key = providerUsageKey{}
		return
	}
	if result.err != nil || !request.sample.available() || request.client.Stopping() {
		return
	}
	w.b.providerUsage.save(request.key, request.sample)
	if w.b.cfg.StatusBar.Has("final", "quota") && request.target != 0 && request.targetProvider == request.key.provider && w.providerUsage.target == request.target && w.providerUsage.key == request.key {
		fields := request.baseFields
		fields.quota = w.providerUsageText(request.key.provider, true)
		header := w.renderStatusBar("final", fields)
		span := w.beginConsumerPhase("store", "set_assistant_header")
		err := w.b.db.SetAssistantHeader(w.ctx, request.target, header)
		span.end()
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
