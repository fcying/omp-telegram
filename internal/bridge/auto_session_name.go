package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
)

// Automatic naming owns one submission, never a task or a control lock.
type autoSessionNameRequest struct {
	client  *omp.Client
	binding store.Binding
	epoch   uint64
	ctx     context.Context
	cancel  context.CancelFunc
}

type autoSessionNameCheck struct {
	request *autoSessionNameRequest
	allow   chan bool
}

type sessionNameOperation struct {
	fence            *autoSessionNameRequest
	barrierConfirmed bool
}

func (w *worker) beginSessionName(arg string) {
	if arg == "" {
		w.say(w.tr(i18n.Msg.UsageNameSessionTitle))
		return
	}
	if w.exportingSession != "" {
		w.say(w.tr(i18n.Msg.WaitCurrentSessionExportFinishBeforeChangingName))
		return
	}
	name := menuText(arg, 160)
	var operation *sessionNameOperation
	var catalog omp.CommandCatalog
	if fence := w.autoSessionNameFence; fence != nil {
		operation = &sessionNameOperation{fence: fence}
		catalog = fence.client.CommandCatalog()
	}
	w.beginControl("name", func(client *omp.Client, ctx context.Context) (json.RawMessage, error) {
		if operation != nil {
			fence := operation.fence
			binding := fence.binding
			validCatalog := func() bool {
				current := client.CommandCatalog()
				return current.Epoch == catalog.Epoch && current.SessionID == catalog.SessionID &&
					current.SessionID == binding.SessionID && current.HasBuiltinCommand("session")
			}
			if client != fence.client || !validCatalog() {
				return nil, errors.New("automatic session naming barrier unavailable")
			}
			// This native prompt shares inputGate with /rename, unlike get_state.
			info, err := client.SessionInfo(ctx)
			if err != nil {
				return nil, err
			}
			if !validCatalog() || !strings.EqualFold(info.ID, binding.SessionID) ||
				!(info.File == binding.Session || sameSessionFile(info.File, binding.Session)) ||
				!sameWorkspace(info.CWD, binding.Workspace) {
				return nil, errors.New("automatic session naming barrier identity changed")
			}
			operation.barrierConfirmed = true
		}
		return client.Call(ctx, "set_session_name", map[string]any{"name": arg})
	}, w.tr(i18n.Msg.SessionNamed, name), operation)
}

func (w *worker) deferAutoSessionNameInput(in incoming) bool {
	if w.autoSessionName == nil || !w.rpcOperationActive {
		return false
	}
	w.autoSessionName.cancel()
	w.autoSessionNameReady = false
	w.autoSessionNameInput = in
	w.autoSessionNameInputPending = true
	return true
}

func (w *worker) resumeAutoSessionNameInput() {
	if !w.autoSessionNameInputPending {
		return
	}
	in := w.autoSessionNameInput
	w.autoSessionNameInput = incoming{}
	w.autoSessionNameInputPending = false
	if w.ctx.Err() != nil {
		w.mark(in.id, store.InboxCancelled)
		return
	}
	w.handle(in)
}

func (w *worker) cancelAutoSessionName() {
	if request := w.autoSessionName; request != nil {
		request.cancel()
		w.autoSessionName = nil
	}
	w.autoSessionNameReady = false
}

func (w *worker) autoSessionNameIdle() bool {
	return !w.taskActive() && !w.sessionControlBusy() && !w.steerFence.active &&
		w.ask == nil && w.askSending == 0 && len(w.hostRequests) == 0 &&
		len(w.queue) == 0 && len(w.input) == 0 && len(w.rpcOperations) == 0 &&
		w.startIntent == nil && w.resumeCancel == nil && w.exportCancel == nil && w.deleteCancel == nil
}

func (w *worker) autoSessionNameScope(request *autoSessionNameRequest) bool {
	binding := request.binding
	if w.autoSessionName != request || request.ctx.Err() != nil || !w.b.cfg.AutoSessionName ||
		w.client != request.client || !w.binding.Running || !w.binding.AutoSessionNamePending ||
		w.binding.Generation != binding.Generation || w.binding.SessionID != binding.SessionID ||
		w.binding.Session != binding.Session || w.binding.Workspace != binding.Workspace || w.sessionID != binding.SessionID {
		return false
	}
	catalog := request.client.CommandCatalog()
	return catalog.Epoch == request.epoch && catalog.SessionID == binding.SessionID && catalog.HasBuiltinCommand("rename")
}

func (w *worker) dispatchAutoSessionName() {
	if !w.autoSessionNameReady || w.autoSessionName != nil || w.rpcOperationActive ||
		!w.b.cfg.AutoSessionName || !w.binding.AutoSessionNamePending || !w.autoSessionNameIdle() || w.ctx.Err() != nil {
		return
	}
	client, connected := w.runtimeClient()
	if !connected {
		return
	}
	catalog := client.CommandCatalog()
	if catalog.State == omp.CatalogUnknown {
		return
	}
	// Retry a failed state read only after another ordinary task commits.
	w.autoSessionNameReady = false
	if catalog.SessionID != w.binding.SessionID || !catalog.HasBuiltinCommand("rename") {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
	request := &autoSessionNameRequest{client: client, binding: w.binding, epoch: catalog.Epoch, ctx: ctx, cancel: cancel}
	w.autoSessionName = request
	// Internal maintenance must not consume a final action, touch activity, or own a turn.
	w.rpcOperations = append(w.rpcOperations, rpcOperation{
		kind: "auto_session_name", generation: request.binding.Generation, clientID: client.ID(), meta: request,
		call: func(context.Context) (json.RawMessage, error) {
			defer cancel()
			state, err := client.Call(ctx, "get_state", nil)
			if err != nil {
				return nil, err
			}
			check := autoSessionNameCheck{request: request, allow: make(chan bool, 1)}
			select {
			case w.operations <- operationResult{kind: "auto_session_name_state", data: state, meta: check}:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			select {
			case allowed := <-check.allow:
				if !allowed {
					return nil, nil
				}
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			// Reader-published scope changes can arrive after the actor grants submission.
			current := client.CommandCatalog()
			if current.Epoch != request.epoch || current.SessionID != request.binding.SessionID || !current.HasBuiltinCommand("rename") {
				return nil, nil
			}
			return client.Call(ctx, "prompt", map[string]any{"message": "/rename"})
		},
	})
	w.dispatchRPCOperation()
}

// The actor validates and consumes while the same RPC operation retains the lane.
func (w *worker) autoSessionNameChecked(result operationResult) {
	check, ok := result.meta.(autoSessionNameCheck)
	if !ok {
		return
	}
	allowed := false
	defer func() { check.allow <- allowed }()
	request := check.request
	if !w.autoSessionNameScope(request) || !w.autoSessionNameIdle() {
		return
	}
	var state struct {
		SessionID   string  `json:"sessionId"`
		SessionName *string `json:"sessionName"`
	}
	if json.Unmarshal(result.data, &state) != nil || state.SessionID != request.binding.SessionID {
		return
	}
	consumed, err := w.b.db.ConsumeAutoSessionName(request.binding)
	if err != nil {
		w.b.storeLog.Error("automatic session name eligibility persistence failed", "event", "auto_session_name_write_failed")
		noticeErr := w.b.db.Enqueue(w.key.chat, w.key.thread, w.tr(i18n.Msg.Session.AutoNameFailed))
		if noticeErr != nil {
			w.b.storeLog.Error("automatic session name failure notice persistence failed", "event", "outbox_write_failed")
		}
		return
	}
	if !consumed {
		return
	}
	w.binding.AutoSessionNamePending = false
	allowed = state.SessionName == nil || *state.SessionName == ""
	if allowed {
		// Cancellation cannot retract a prompt awaiting native input-hook admission.
		w.autoSessionNameFence = request
	}
}

func (w *worker) autoSessionNameFinished(result operationResult) {
	request, ok := result.meta.(*autoSessionNameRequest)
	if ok && result.err == nil && w.autoSessionNameFence == request {
		var ack struct {
			AgentInvoked *bool `json:"agentInvoked"`
		}
		if json.Unmarshal(result.data, &ack) == nil && ack.AgentInvoked != nil && !*ack.AgentInvoked {
			w.autoSessionNameFence = nil
		}
	}
	if ok && w.autoSessionName == request {
		request.cancel()
		w.autoSessionName = nil
	}
	// An ACK is not title completion. No failure here retires or replays user work.
}
