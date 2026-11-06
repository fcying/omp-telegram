package bridge

import (
	"context"
	"encoding/json"
	"time"

	"omp-telegram/internal/omp"
)

const (
	progressNativeInterval = 5 * time.Second
	progressNativeTimeout  = 3 * time.Second
)

type progressNativeState struct {
	results   chan progressNativeResult
	pending   *progressNativeRequest
	completed *progressNativeRequest
	next      time.Time
}

type progressNativeRequest struct {
	client     *omp.Client
	generation int64
	active     int64
	turn       uint64
	epoch      uint64
	sessionID  string
	cancel     context.CancelFunc
}

type progressNativeResult struct {
	request *progressNativeRequest
	state   statusState
	valid   bool
}

func (w *worker) cancelProgressNative() {
	if request := w.progressNative.pending; request != nil {
		request.cancel()
		w.progressNative.pending = nil
	}
	w.progressNative.next = time.Time{}
	w.progressNative.completed = nil
	w.progress.NativeStatus = ""
	w.replyStatus.liveState = statusState{}
	w.replyStatus.liveScope = nil
}

// Title and other metadata updates do not change the native sampling scope.
// The reader has already fenced actual session changes in the catalog epoch.
func (w *worker) progressNativeSessionChanged() {
	client, connected := w.runtimeClient()
	if !connected || w.binding.SessionID == "" || client.CommandCatalog().SessionID != w.binding.SessionID {
		w.cancelProgressNative()
		return
	}
	for _, request := range []*progressNativeRequest{w.progressNative.pending, w.progressNative.completed, w.replyStatus.liveScope} {
		if request != nil && !w.progressNativeMatches(request) {
			w.cancelProgressNative()
			return
		}
	}
}

func (w *worker) progressNativeMatches(request *progressNativeRequest) bool {
	if request == nil || request.client == nil || request.sessionID == "" ||
		w.client != request.client || w.binding.Generation != request.generation ||
		w.turn != request.turn || w.active != request.active || !w.taskRunning() || w.finishing ||
		w.binding.SessionID != request.sessionID {
		return false
	}
	catalog := request.client.CommandCatalog()
	return catalog.Epoch == request.epoch && catalog.SessionID == request.sessionID
}

func (w *worker) progressContextReadCompleted() bool {
	return w.progressNativeMatches(w.progressNative.completed)
}

// Empty means context has not been read in this scope, not a failed read.
func (w *worker) progressNativeStatus() string {
	if !w.progressContextReadCompleted() {
		return ""
	}
	return w.progress.NativeStatus
}

// Assistant headers share this bounded sampler even when progress UI is disabled.
func (w *worker) refreshProgressNative(now time.Time) {
	client, connected := w.runtimeClient()
	if !connected || !w.taskRunning() || w.finishing || w.replyStatus.pending != nil || now.Sub(w.progress.StartedAt) < progressInitialDelay {
		return
	}
	if w.progressNative.pending != nil || now.Before(w.progressNative.next) {
		return
	}
	if w.progressNative.results == nil {
		w.progressNative.results = make(chan progressNativeResult, 1)
	}
	catalog := client.CommandCatalog()
	ctx, cancel := context.WithTimeout(w.ctx, progressNativeTimeout)
	request := &progressNativeRequest{
		client: client, generation: w.binding.Generation, active: w.active, turn: w.turn,
		epoch: catalog.Epoch, sessionID: w.binding.SessionID, cancel: cancel,
	}
	w.progressNative.pending = request
	w.progressNative.next = now.Add(progressNativeInterval)
	results := w.progressNative.results
	w.background.Add(1)
	// Shared display sampling must not delay controls or stdout consumption.
	go func() {
		defer w.background.Done()
		defer cancel()
		result := progressNativeResult{request: request}
		raw, err := client.Call(ctx, "get_state", nil)
		result.valid = err == nil && json.Unmarshal(raw, &result.state) == nil
		select {
		case results <- result:
		case <-w.ctx.Done():
		}
	}()
}

// The actor can immediately flush a completed current-scope read, including failure.
func (w *worker) progressNativeFinished(result progressNativeResult) bool {
	request := result.request
	if request == nil || w.progressNative.pending != request {
		return false
	}
	request.cancel()
	w.progressNative.pending = nil
	if !w.progressNativeMatches(request) {
		return false
	}
	if result.valid && result.state.SessionID != request.sessionID {
		return false
	}
	w.progressNative.completed = request
	w.progress.NativeStatus = compactUnknownContext
	w.replyStatus.liveState = statusState{}
	w.replyStatus.liveScope = nil
	if !result.valid {
		return true
	}
	result.state.ConfiguredThinking = w.configuredThinking
	// Retain only the same safe display projection, never native model config
	// or formatted Markdown. Intermediate headers recheck this request's scope.
	w.replyStatus.liveState = statusState{
		Model:           omp.Model{ID: result.state.Model.ID, Provider: result.state.Model.Provider},
		FastModeEnabled: result.state.FastModeEnabled, FastModeActive: result.state.FastModeActive,
		IsStreaming: result.state.IsStreaming, IsCompacting: result.state.IsCompacting,
		ContextUsage: result.state.ContextUsage,
	}
	w.replyStatus.liveScope = request
	w.progress.NativeStatus = compactHeader(w.b.language(), result.state, nil)
	return true
}
