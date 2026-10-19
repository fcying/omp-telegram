package bridge

import (
	"context"
	"encoding/json"
	"time"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
)

// Native controls own only their submission, never the running task's lifecycle.
type nativeControlRequest struct {
	client       *omp.Client
	generation   int64
	epoch        uint64
	sessionID    string
	inbox        int64
	text         string
	phase        string
	ctx          context.Context
	cancel       context.CancelFunc
	lookupCancel context.CancelFunc
}

func (w *worker) beginNativeControl(in incoming, text string) {
	if w.controlInProgress() || w.sessionOperationActive() {
		w.say(w.tr(i18n.Msg.SessionOperationStillLoading))
		w.mark(in.id, store.InboxCancelled)
		return
	}
	client, err := w.ensureRuntime()
	if err != nil {
		w.say(w.errorText(err))
		w.mark(in.id, store.InboxCancelled)
		return
	}
	catalog := client.CommandCatalog()
	ctx, cancel := context.WithCancel(w.ctx)
	c := &nativeControlRequest{client: client, generation: w.binding.Generation,
		epoch: catalog.Epoch, sessionID: w.sessionID, inbox: in.id, text: text, ctx: ctx, cancel: cancel}
	w.nativeControl = c
	w.beginControlOperation(controlNative)
	if catalog.State == omp.CatalogUnknown {
		c.phase = "catalog"
		lookupCtx, lookupCancel := context.WithTimeout(c.ctx, 15*time.Second)
		c.lookupCancel = lookupCancel
		w.startOperation("native_control", client, func(context.Context) (json.RawMessage, error) {
			defer lookupCancel()
			_, err := client.RefreshCommandCatalog(lookupCtx)
			return nil, err
		}, 0, "", c)
		return
	}
	w.submitNativeControl(c)
}

func (w *worker) nativeControlScope(c *nativeControlRequest) bool {
	if w.nativeControl != c || w.client != c.client || w.binding.Generation != c.generation || w.sessionID != c.sessionID {
		return false
	}
	catalog := c.client.CommandCatalog()
	return catalog.Epoch == c.epoch && catalog.SessionID == c.sessionID
}

func (w *worker) endNativeControl(c *nativeControlRequest) {
	if w.nativeControl != c {
		return
	}
	c.cancel()
	w.nativeControl = nil
	if w.controlOp == controlNative {
		w.endControlOperation()
	}
}

func (w *worker) cancelNativeControl() {
	c := w.nativeControl
	if c == nil {
		return
	}
	w.endNativeControl(c)
	state := store.InboxCancelled
	if c.phase == "prompt" {
		state = store.InboxUncertain
	}
	w.mark(c.inbox, state)
}

func (w *worker) submitNativeControl(c *nativeControlRequest) {
	if !w.nativeControlScope(c) || !c.client.CommandCatalog().HasBuiltinCommand("fast") {
		w.endNativeControl(c)
		w.mark(c.inbox, store.InboxCancelled)
		w.say(w.tr(i18n.Msg.NativeControlUnavailable))
		return
	}
	if !w.mark(c.inbox, store.InboxSubmitted) {
		w.endNativeControl(c)
		return
	}
	c.phase = "prompt"
	w.startOperation("native_control", c.client, func(context.Context) (json.RawMessage, error) {
		ctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
		defer cancel()
		return c.client.Call(ctx, "prompt", map[string]any{"message": c.text})
	}, 0, "", c)
}

func (w *worker) nativeControlFinished(result operationResult) {
	c, ok := result.meta.(*nativeControlRequest)
	if !ok || w.nativeControl != c {
		return
	}
	if c.phase == "catalog" {
		if c.client.CommandCatalog().State == omp.CatalogUnknown {
			w.endNativeControl(c)
			w.mark(c.inbox, store.InboxCancelled)
			w.say(w.tr(i18n.Msg.NativeControlUnavailable))
			return
		}
		w.submitNativeControl(c)
		return
	}
	var acknowledgment struct {
		AgentInvoked *bool `json:"agentInvoked"`
	}
	local := json.Unmarshal(result.data, &acknowledgment) == nil && acknowledgment.AgentInvoked != nil && !*acknowledgment.AgentInvoked
	if w.nativeControlScope(c) && result.err == nil && !result.cancelled && local {
		w.endNativeControl(c)
		w.mark(c.inbox, store.InboxDone)
		w.touchBinding()
		return
	}
	current := w.client == c.client && w.binding.Generation == c.generation
	uncertain := result.err == nil || uncertainOperationOutcome(result.err, result.cancelled)
	w.endNativeControl(c)
	if uncertain {
		w.mark(c.inbox, store.InboxUncertain)
	} else {
		w.mark(c.inbox, store.InboxCancelled)
	}
	if current && uncertain {
		w.retireTaskRuntime(w.tr(i18n.Msg.NativeControlUnconfirmed))
		return
	}
	if current {
		w.say(w.tr(i18n.Msg.NativeControlUnconfirmed))
	}
}

func (w *worker) stopNativeControl(notice string) {
	// Preserve autonomous and steered owners' existing shutdown boundaries.
	if w.research.control != nil || w.research.root || w.research.enabled || w.research.disablePending {
		w.handleResearchStop()
		return
	}
	if w.steerFence.active {
		w.stopSteeredRoot()
		return
	}
	w.completeAbort(true, notice)
}
