package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
)

// Only the actor advances phases. RPC closures return immutable snapshots through
// the existing operation lane; all native events continue through worker.event.
type researchControl struct {
	client                         *omp.Client
	generation                     int64
	turn, epoch, revision          uint64
	sessionID                      string
	intent, phase, text, requestID string
	inbox                          int64
	input                          incoming
	queued, accepted, completed    bool
	cancel                         context.CancelFunc
	ctx                            context.Context
	deadline                       time.Time
}

func (w *worker) rememberResearchMode(client *omp.Client, enabled bool) {
	catalog := client.CommandCatalog()
	w.research.known, w.research.enabled = true, enabled
	w.research.modeClient, w.research.modeEpoch = client, catalog.Epoch
	w.research.modeRevision, w.research.modeSession = catalog.Revision, catalog.SessionID
	w.research.modeGeneration = w.binding.Generation
}

func (w *worker) researchOffProof(client *omp.Client) bool {
	catalog := client.CommandCatalog()
	return w.research.known && !w.research.enabled && !w.research.disablePending &&
		w.research.modeClient == client && w.research.modeGeneration == w.binding.Generation && w.research.modeEpoch == catalog.Epoch &&
		w.research.modeRevision == catalog.Revision && w.research.modeSession == catalog.SessionID
}

func (w *worker) newResearchControl(client *omp.Client, intent string, inbox int64) *researchControl {
	catalog := client.CommandCatalog()
	ctx, cancel := context.WithCancel(w.ctx)
	c := &researchControl{client: client, generation: w.binding.Generation, turn: w.turn,
		epoch: catalog.Epoch, revision: catalog.Revision, sessionID: catalog.SessionID,
		intent: intent, inbox: inbox, ctx: ctx, cancel: cancel}
	w.research.control = c
	return c
}

func (w *worker) researchControlScope(c *researchControl) bool {
	if w.research.control != c || w.client != c.client || w.binding.Generation != c.generation || w.turn != c.turn {
		return false
	}
	catalog := c.client.CommandCatalog()
	return catalog.Epoch == c.epoch && (c.phase == "catalog" || catalog.Revision == c.revision) &&
		(c.sessionID == "" || catalog.SessionID == c.sessionID)
}

func (w *worker) endResearchControl(c *researchControl) {
	if w.research.control != c {
		return
	}
	c.cancel()
	w.research.control = nil
}

func (w *worker) cancelResearchControl() {
	c := w.research.control
	if c == nil {
		return
	}
	w.endResearchControl(c)
	if c.intent == "route" {
		if c.queued {
			w.cancelQueuedTask(c.inbox)
		} else {
			w.mark(c.inbox, store.InboxCancelled)
		}
	} else if c.intent == "off" {
		w.mark(c.inbox, store.InboxUncertain)
	}
	// Cancellation never proves that native autoresearch mode is off.
	if c.intent != "route" || w.research.root {
		w.research.disablePending, w.resumeFailed = true, true
	}
}

// Pending empty toggles for a research owner are shutdown controls, not future
// enabling tasks. Keep ordinary prompts and nonempty native commands queued.
func (w *worker) cancelQueuedResearchToggles() {
	for i := 0; i < len(w.queue); {
		q := &w.queue[i]
		if q.kind != queuedSlashPending && (q.kind != queuedNativeCommand || q.nativeName != "autoresearch") {
			i++
			continue
		}
		args, command := autoresearchArguments(q.text, w.b.bot.Username)
		if !command || args != "" {
			i++
			continue
		}
		if !w.cancelQueuedTask(q.id) {
			return
		}
	}
}

func (w *worker) researchControlCall(c *researchControl, phase string, call func(context.Context) (json.RawMessage, error)) {
	c.phase = phase
	w.startOperation("research_control", c.client, func(context.Context) (json.RawMessage, error) {
		ctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
		defer cancel()
		return call(ctx)
	}, 0, "", c)
}

func (w *worker) researchControlMode(c *researchControl, phase string) {
	client := c.client
	w.researchControlCall(c, phase, func(ctx context.Context) (json.RawMessage, error) {
		enabled, err := client.AutoresearchMode(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(enabled)
	})
}

// Abort cannot fence an asynchronous research hook. Retire research before off,
// but a proven ordinary mode-off owner must retain its runtime and inbox.
func (w *worker) disableResearch(inbox int64) {
	researchActive := w.research.root || w.research.enabled
	if researchActive || w.research.disablePending {
		w.cancelQueuedResearchToggles()
	}
	replacedReadOnly := false
	if c := w.research.control; c != nil {
		// Read-only mode lookups have not submitted a native control. Replacing
		// them must not retire the ordinary owner, even if its task just completed.
		readOnly := c.intent == "route" || c.intent == "off" && c.text == ""
		replacedReadOnly = readOnly && !researchActive
		w.cancelResearchControl()
		if !replacedReadOnly {
			w.retireResearch(w.tr(i18n.Msg.AutoresearchControlInterruptedWillNotReplayedAutomatically))
		}
	}
	w.invalidateResearchAdmission()
	client, connected := w.runtimeClient()
	if connected && w.taskActive() && !researchActive && w.researchOffProof(client) {
		w.mark(inbox, store.InboxDone)
		w.say(w.tr(i18n.Msg.AutoresearchModeAlreadyDisabledCurrentOrdinaryTaskLeftRunning))
		return
	}
	if connected && (w.taskActive() || replacedReadOnly) && !researchActive {
		c := w.newResearchControl(client, "off", inbox)
		w.researchControlMode(c, "mode")
		return
	}
	w.beginResearchDisable(inbox)
}

func (w *worker) beginResearchDisable(inbox int64) {
	if w.taskActive() || w.research.root || w.steerFence.active || w.rpcOperationActive {
		w.retireResearch(w.tr(i18n.Msg.AutoresearchForciblyInterruptedTaskOutcomeUncertainWillNotReplayed))
	}
	w.research.disablePending, w.resumeFailed = true, true
	client, err := w.prepareResearchControlRuntime()
	if err != nil {
		w.mark(inbox, store.InboxUncertain)
		w.say(w.tr(i18n.Msg.AutoresearchCouldNotConfirmedDisabledQueuedWorkPaused, w.errorText(err)))
		return
	}
	c := w.newResearchControl(client, "off", inbox)
	c.text = "/autoresearch off"
	w.researchControlCall(c, "identity", func(ctx context.Context) (json.RawMessage, error) {
		info, err := client.SessionInfo(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(info)
	})
}

// Process startup keeps the existing lifecycle, but identity/catalog network
// waits belong to the cancellable operation lane, not the actor.
func (w *worker) prepareResearchControlRuntime() (*omp.Client, error) {
	sessionID := w.sessionID
	if sessionID == "" {
		sessionID = w.binding.SessionID
	}
	if sessionID == "" || !strings.EqualFold(sessionID, w.binding.SessionID) {
		return nil, newMessageError(i18n.Msg.SavedAutoresearchSessionUnavailableRestoreSessionFileWorkingDirectory)
	}
	if w.b.sessionInUseByOther(w, sessionID) {
		return nil, newMessageError(i18n.Msg.AutoresearchSessionActiveAnotherConversation)
	}
	if client, connected := w.runtimeClient(); connected {
		return client, nil
	}
	if !savedSessionAvailable(w.binding.Session, w.binding.Workspace) {
		return nil, newMessageError(i18n.Msg.SavedAutoresearchOwnerUnavailableRestoreSessionFileWorkingDirectory)
	}
	if w.runtime == runtimeStarting {
		return nil, newMessageError(i18n.Msg.OMPStarting)
	}
	select {
	case w.b.slots <- struct{}{}:
	default:
		return nil, newMessageError(i18n.Msg.ResearchInstanceLimitReached)
	}
	if !w.beginSessionRestore(sessionID) {
		<-w.b.slots
		return nil, newMessageError(i18n.Msg.AutoresearchSessionDeletedUsedElsewhere)
	}
	w.beginRuntimeStart()
	launchArgs := w.b.selectedArgsSnapshot()
	client, err := omp.Start(w.ctx, omp.Config{Binary: w.b.cfg.OMP, CWD: w.binding.Workspace,
		Resume: w.binding.Session, Args: launchArgs, Environment: w.b.cfg.OMPEnvironment}, w.b.rpcLog)
	if err != nil {
		<-w.b.slots
		w.runtimeStopped()
		w.releaseSession()
		return nil, err
	}
	w.runtimeStarted(client, launchArgs)
	return client, nil
}

func (w *worker) researchControlIdle(c *researchControl) {
	client := c.client
	w.researchControlCall(c, "idle", func(ctx context.Context) (json.RawMessage, error) {
		return client.Call(ctx, "get_state", nil)
	})
}

func (w *worker) confirmResearchClear(confirmation confirmation) {
	client, ok := w.runtimeClient()
	if !ok || !w.researchAvailable(client) || client.CommandCatalog().Epoch != confirmation.epoch ||
		w.sessionControlBusy() || w.taskActive() || len(w.queue) != 0 || len(w.steers) != 0 {
		w.say(w.tr(i18n.Msg.AutoresearchClearCanceledBecauseInstanceNoLongerIdleCommand))
		return
	}
	c := w.newResearchControl(client, "clear", 0)
	c.text = confirmation.autoresearchText
	w.researchControlMode(c, "mode")
}

func (w *worker) researchControlFailed(c *researchControl) {
	if w.research.control != c {
		return
	}
	w.endResearchControl(c)
	w.research.known = false
	if c.intent == "route" {
		if c.queued {
			w.cancelQueuedTask(c.inbox)
		} else {
			w.mark(c.inbox, store.InboxCancelled)
		}
		if w.research.root {
			w.retireResearch(w.tr(i18n.Msg.AutoresearchModeCouldNotConfirmedOldTaskInterruptedWill))
		}
		w.researchModeFailure()
		return
	}
	if c.intent == "off" && c.text == "" {
		// A failed read-only probe does not establish a research hook to retire.
		w.research.disablePending, w.resumeFailed = true, true
		w.mark(c.inbox, store.InboxUncertain)
		w.say(w.tr(i18n.Msg.AutoresearchModeCouldNotConfirmedCurrentOrdinaryTaskLeft))
		return
	}
	if c.intent == "local" || w.taskActive() {
		w.retireResearch(w.tr(i18n.Msg.AutoresearchControlCompletionCouldNotConfirmedTaskOutcomeUncertain))
	} else {
		w.research.disablePending, w.resumeFailed = true, true
		w.releaseRuntimeWithReason(true, "failure")
	}
	if c.intent == "off" {
		w.mark(c.inbox, store.InboxUncertain)
	}
	if c.intent == "clear" {
		w.say(w.tr(i18n.Msg.AutoresearchClearCouldNotConfirmedRuntimeClosedQueuedWork))
	} else {
		w.say(w.tr(i18n.Msg.AutoresearchCouldNotConfirmedDisabledRuntimeClosedQueuedWork))
	}
}

func (w *worker) researchControlFinished(result operationResult) {
	c, ok := result.meta.(*researchControl)
	if !ok || w.research.control != c {
		return
	}
	if !w.researchControlScope(c) || result.err != nil {
		w.researchControlFailed(c)
		return
	}
	client := c.client
	switch c.phase {
	case "identity":
		var info omp.SessionInfo
		if json.Unmarshal(result.data, &info) != nil {
			w.researchControlFailed(c)
			return
		}
		// A live native session can have an identity before its file is persisted.
		sameFile := info.File == w.binding.Session || sameSessionFile(info.File, w.binding.Session)
		if !strings.EqualFold(info.ID, w.binding.SessionID) || !sameFile ||
			!sameWorkspace(info.CWD, w.binding.Workspace) || !w.claimSession(info.File, info.ID) {
			w.researchControlFailed(c)
			return
		}
		catalog := client.CommandCatalog()
		c.sessionID = catalog.SessionID
		if catalog.State == omp.CatalogUnknown {
			w.researchControlCall(c, "catalog", func(ctx context.Context) (json.RawMessage, error) {
				_, err := client.RefreshCommandCatalog(ctx)
				return nil, err
			})
			return
		}
		w.researchControlIdle(c)
	case "catalog":
		c.revision = client.CommandCatalog().Revision
		w.researchControlIdle(c)
	case "mode", "postmode":
		var enabled bool
		if json.Unmarshal(result.data, &enabled) != nil {
			w.researchControlFailed(c)
			return
		}
		w.rememberResearchMode(client, enabled)
		if c.phase == "postmode" {
			if c.intent != "local" && enabled {
				w.researchControlFailed(c)
				return
			}
			w.finishResearchControl(c, enabled)
			return
		}
		switch c.intent {
		case "route":
			w.finishResearchRoute(c, enabled)
		case "stop":
			w.endResearchControl(c)
			if enabled {
				w.disableResearch(0)
			} else {
				w.requestAbort(w.tr(i18n.Msg.AbortRequestedQueuedPromptsCleared))
			}
		case "off":
			if !enabled {
				w.finishResearchControl(c, false)
			} else {
				w.endResearchControl(c)
				w.beginResearchDisable(c.inbox)
			}
		case "clear":
			w.researchControlIdle(c)
		}
	case "idle":
		idle, settled, pending := rpcIdleState(result.data)
		if !idle || !settled || pending || !w.researchAvailable(client) ||
			client.CommandCatalog().SessionID != w.sessionID ||
			(c.intent == "clear" && (w.taskActive() || len(w.queue) != 0 || len(w.steers) != 0)) {
			w.researchControlFailed(c)
			return
		}
		c.requestID = client.ReserveRequestID()
		w.research.command, w.research.notices = true, 0
		id, text := c.requestID, c.text
		w.researchControlCall(c, "prompt", func(ctx context.Context) (json.RawMessage, error) {
			return client.CallWithID(ctx, id, "prompt", map[string]any{"message": text})
		})
	case "prompt":
		var ack struct {
			AgentInvoked *bool `json:"agentInvoked"`
		}
		if len(result.data) != 0 && json.Unmarshal(result.data, &ack) != nil || ack.AgentInvoked != nil && *ack.AgentInvoked {
			w.researchControlFailed(c)
			return
		}
		c.accepted = true
		c.phase, c.deadline = "completion", time.Now().Add(15*time.Second)
		if c.completed {
			w.researchControlMode(c, "postmode")
		}
	}
}

func (w *worker) finishResearchRoute(c *researchControl, enabled bool) {
	args, _ := autoresearchArguments(c.text, w.b.bot.Username)
	w.endResearchControl(c)
	if args == "" && enabled || autoresearchClear(args) {
		if c.queued {
			for i, q := range w.queue {
				if q.id == c.inbox {
					w.queue = append(w.queue[:i], w.queue[i+1:]...)
					break
				}
			}
		}
		if args == "" {
			if w.mark(c.inbox, store.InboxSubmitted) {
				w.disableResearch(c.inbox)
			}
			return
		}
		if w.sessionControlBusy() || w.taskActive() || len(w.queue) != 0 || len(w.steers) != 0 {
			w.mark(c.inbox, store.InboxCancelled)
			w.say(w.tr(i18n.Msg.AutoresearchClearRequiresIdleInstanceEmptyQueueStopResearch))
			return
		}
		w.mark(c.inbox, store.InboxDone)
		w.confirm(confirmation{action: "autoresearch_clear", user: c.input.msg.From.ID,
			autoresearchText: c.text, epoch: c.epoch}, w.tr(i18n.Msg.ClearAutoresearchNativeClearCanHardResetTrackedFiles), []string{w.tr(i18n.Msg.ClearAutoresearch), w.tr(i18n.Msg.Cancel)})
		return
	}
	if c.queued {
		for i := range w.queue {
			if w.queue[i].id == c.inbox {
				w.queue[i].reply = nil
				return
			}
		}
		return
	}
	w.enqueueNativeCommand(c.input, c.text, "autoresearch", c.sessionID)
}

func (w *worker) finishResearchControl(c *researchControl, enabled bool) {
	w.endResearchControl(c)
	w.research.command = false
	if c.intent == "local" {
		w.research.root = false
		w.finishLocalCommandSubmission()
		return
	}
	w.research.disablePending, w.resumeFailed = false, false
	w.research.root = false
	w.touchBinding()
	if c.intent == "off" {
		w.mark(c.inbox, store.InboxDone)
		w.say(w.tr(i18n.Msg.AutoresearchModeDisabledOldResearchTaskNotReplayed))
	} else {
		w.say(w.tr(i18n.Msg.AutoresearchClearCompleted))
	}
}

func (w *worker) researchControlEvent(e rpcEvent) bool {
	c := w.research.control
	if c == nil {
		return false
	}
	if !w.researchControlScope(c) {
		w.researchControlFailed(c)
		// A preserved runtime still owns its events after the control fails.
		if w.client == c.client {
			return false
		}
		return e.Type != "available_commands_update" && e.Type != "session_info_update"
	}
	if c.requestID == "" {
		return false
	}
	if e.Type == "agent_start" {
		w.researchControlFailed(c)
		return true
	}
	if e.Type != "prompt_result" || e.ID != c.requestID {
		return false
	}
	if e.Status != "completed" || e.AgentInvoked == nil || *e.AgentInvoked || e.SessionSettled == nil || !*e.SessionSettled {
		w.researchControlFailed(c)
		return true
	}
	c.completed = true
	if c.accepted && c.phase == "completion" {
		w.researchControlMode(c, "postmode")
	}
	return true
}

func (w *worker) researchControlTick(now time.Time) {
	if c := w.research.control; c != nil && (c.phase == "acceptance" || c.phase == "completion") && !now.Before(c.deadline) {
		w.researchControlFailed(c)
	}
}
