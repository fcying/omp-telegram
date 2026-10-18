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
	results chan progressNativeResult
	pending *progressNativeRequest
	next    time.Time
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
	w.progress.NativeStatus = ""
}

func (w *worker) refreshProgressNative(now time.Time) {
	client, connected := w.runtimeClient()
	if !connected || !w.taskRunning() || w.finishing || w.replyStatus.pending != nil || w.b.cfg.ProgressMode == "off" || w.progressSuppressed || now.Sub(w.progress.StartedAt) < progressInitialDelay {
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
	// UI sampling has its own lane; it must not delay controls or stdout consumption.
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

func (w *worker) progressNativeFinished(result progressNativeResult) {
	request := result.request
	if request == nil || w.progressNative.pending != request {
		return
	}
	request.cancel()
	w.progressNative.pending = nil
	if w.client != request.client || w.binding.Generation != request.generation || w.turn != request.turn || w.active != request.active || !w.taskRunning() || w.finishing {
		return
	}
	w.progress.NativeStatus = ""
	catalog := request.client.CommandCatalog()
	if !result.valid || catalog.Epoch != request.epoch || w.binding.SessionID != request.sessionID || catalog.SessionID != request.sessionID || result.state.SessionID != request.sessionID {
		return
	}
	result.state.ConfiguredThinking = w.configuredThinking
	w.progress.NativeStatus = compactProgressStatus(result.state)
}
