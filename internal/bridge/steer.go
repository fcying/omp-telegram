package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
)

type steerRequest struct {
	inboxID, rootID, generation int64
	turn, clientID, admission   uint64
}

type steerFence struct {
	active, settled, terminalSeen bool
	revision, admission           uint64
	terminal                      terminalResult
	notice                        string
	probeCancel                   context.CancelFunc
	probeResults                  chan steerProbeResult
	nextProbe                     time.Time
}

type steerProbeResult struct {
	client             *omp.Client
	generation, rootID int64
	turn, revision     uint64
	settled            bool
	err                error
}

func (w *worker) pendingInputFull() bool {
	return len(w.queue)+len(w.steers) >= w.b.cfg.QueueCapacity
}

func (w *worker) routePrompt(in incoming, text string) {
	if w.taskActive() && w.activeInputKind != queuedNativeCommand && w.runtime == runtimeConnected && w.client != nil {
		w.steer(in, text)
		return
	}
	w.enqueuePrompt(in, text)
}

func (w *worker) steer(in incoming, text string) {
	client := w.client
	if w.rejectUnsupportedSlash(client.CommandCatalog(), text, in.id) {
		return
	}
	if w.pendingInputFull() {
		w.say("The queue is full. This message was not submitted.")
		w.mark(in.id, "cancelled")
		return
	}
	prompt := preparePromptText(in.msg, text)
	// Leave room for streamingBehavior on the already conservative prompt frame budget.
	if _, fits := promptInlineCount(prompt, nil, client.FrameLimit()-len(`,"streamingBehavior":"steer"`)); !fits {
		w.say("Prompt exceeds the omp RPC frame limit. No task was submitted.")
		w.mark(in.id, "failed")
		return
	}
	id := client.ReserveRequestID()
	if err := w.b.db.Submit(in.id, in.msg.MessageID); err != nil {
		w.b.fail(err)
		w.cancel()
		return
	}
	w.steerFence.active = true
	w.steerFence.settled = false
	w.steerFence.revision++
	w.steerFence.admission++
	w.cancelSteerProbe()
	if w.steers == nil {
		w.steers = make(map[string]steerRequest)
	}
	request := steerRequest{inboxID: in.id, rootID: w.active, generation: w.binding.Generation, turn: w.turn, clientID: client.ID(), admission: w.steerFence.admission}
	w.steers[id] = request
	w.log.LogAttrs(context.Background(), slog.LevelInfo, "steer submitted", slog.String("event", "steer_submit"), slog.Int64("inbox_id", in.id), slog.Int64("root_inbox_id", w.active), slog.Bool("request_id_valid", true), slog.Int64("generation", w.binding.Generation), slog.Uint64("turn", w.turn), slog.Uint64("client_id", client.ID()))
	w.startOperation("steer", client, func(ctx context.Context) (json.RawMessage, error) {
		return client.CallWithID(ctx, id, "prompt", map[string]any{"message": prompt, "streamingBehavior": "steer"})
	}, w.active, "", id)
}

func (w *worker) currentSteer(r steerRequest) bool {
	return w.client != nil && r.rootID == w.active && r.generation == w.binding.Generation && r.turn == w.turn && r.clientID == w.client.ID()
}

func (w *worker) finishSteer(id string, state store.InboxState) bool {
	r, ok := w.steers[id]
	if !ok || !w.currentSteer(r) {
		return false
	}
	if err := w.b.db.FinishSubmitted(r.inboxID, state); err != nil {
		w.b.fail(err)
		w.cancel()
		return false
	}
	delete(w.steers, id)
	w.log.LogAttrs(context.Background(), slog.LevelInfo, "steer completed", slog.String("event", "steer_complete"), slog.String("result", string(state)), slog.Int64("inbox_id", r.inboxID), slog.Int64("root_inbox_id", r.rootID), slog.Int64("generation", r.generation), slog.Uint64("turn", r.turn), slog.Uint64("client_id", r.clientID))
	return true
}

func (w *worker) steerOperationFinished(result operationResult) {
	id, _ := result.meta.(string)
	r, ok := w.steers[id]
	if !ok || !w.currentSteer(r) || result.clientID != r.clientID {
		return
	}
	if result.err != nil {
		state := store.InboxUncertain
		if omp.ClassifyError(result.err) == "rejected" {
			state = store.InboxFailed
		}
		if !w.finishSteer(id, state) {
			return
		}
		if state == store.InboxUncertain {
			w.retireSteeredRoot("Steering outcome is uncertain and will not be replayed automatically.")
		} else {
			w.say("Steering failed. The current task may not include that instruction.")
			w.maybeFinishSteeredRoot()
		}
		return
	}
	var response struct {
		AgentInvoked *bool `json:"agentInvoked"`
	}
	if json.Unmarshal(result.data, &response) == nil && response.AgentInvoked != nil && !*response.AgentInvoked {
		if w.finishSteer(id, store.InboxDone) {
			w.probeSteeredQuiescence()
			w.maybeFinishSteeredRoot()
		}
	}
}

func (w *worker) steerResult(e rpcEvent) bool {
	r, ok := w.steers[e.ID]
	if !ok {
		return false
	}
	if !w.currentSteer(r) {
		return true
	}
	state := store.InboxUncertain
	switch e.Status {
	case "completed":
		state = store.InboxDone
	case "aborted":
		state = store.InboxCancelled
	case "error":
		state = store.InboxFailed
	}
	if !w.finishSteer(e.ID, state) {
		return true
	}
	// A later false is current session evidence, regardless of admission order.
	if r.admission == w.steerFence.admission || e.SessionSettled != nil && !*e.SessionSettled {
		w.steerFence.settled = e.SessionSettled != nil && *e.SessionSettled
	}
	if state == store.InboxFailed {
		w.say("Steering failed. The current task may not include that instruction.")
	} else if state == store.InboxUncertain {
		w.retireSteeredRoot("Steering outcome is uncertain and will not be replayed automatically.")
		return true
	}
	w.maybeFinishSteeredRoot()
	return true
}

func (w *worker) newSteeredRun() {
	if !w.steerFence.active {
		return
	}
	w.steerFence.revision++
	w.steerFence.settled = false
	w.steerFence.terminalSeen = false
	w.steerFence.notice = ""
	w.cancelSteerProbe()
}

func (w *worker) deferSteeredTerminal(e rpcEvent) {
	w.steerFence.terminalSeen = true
	w.steerFence.terminal = w.terminalState(e)
	w.steerFence.notice = ""
	if w.steerFence.terminal == terminalUncertain {
		w.steerFence.notice = w.terminalFailureNotice(e)
	}
	w.maybeFinishSteeredRoot()
}

func (w *worker) maybeFinishSteeredRoot() {
	if w.research.root {
		return
	}
	fence := &w.steerFence
	if !fence.active || len(w.steers) != 0 || !fence.terminalSeen {
		return
	}
	if !fence.settled {
		w.probeSteeredQuiescence()
		return
	}
	switch fence.terminal {
	case terminalDone:
		if w.preview == "" {
			w.finishUncertain("omp ended without a confirmed text result. The task outcome is uncertain and will not be replayed automatically.")
		} else {
			w.finish()
		}
	case terminalCancelled:
		w.finishCancelled("Task was cancelled before completion.")
	case terminalUncertain:
		w.finishUncertain(fence.notice)
	}
	if w.active == 0 {
		w.clearSteerFence()
	}
}

func (w *worker) cancelSteerProbe() {
	if w.steerFence.probeCancel != nil {
		w.steerFence.probeCancel()
		w.steerFence.probeCancel = nil
	}
}

func (w *worker) clearSteerFence() {
	w.cancelSteerProbe()
	w.steerFence = steerFence{}
}

func (w *worker) probeSteeredQuiescence() {
	if w.research.root {
		return
	}
	fence := &w.steerFence
	if !fence.active || w.client == nil || fence.settled || fence.probeCancel != nil || len(w.steers) != 0 || !fence.terminalSeen {
		return
	}
	if time.Now().Before(fence.nextProbe) {
		return
	}
	if fence.probeResults == nil {
		fence.probeResults = make(chan steerProbeResult, 1)
	}
	ctx, cancel := context.WithTimeout(w.ctx, idleProbeTimeout)
	fence.probeCancel = cancel
	result := steerProbeResult{client: w.client, generation: w.binding.Generation, rootID: w.active, turn: w.turn, revision: fence.revision}
	results := fence.probeResults
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer cancel()
		raw, err := result.client.Call(ctx, "get_state", nil)
		result.err = err
		if err == nil {
			var state struct {
				Settled *bool `json:"isSettled"`
			}
			result.settled = json.Unmarshal(raw, &state) == nil && state.Settled != nil && *state.Settled
		}
		select {
		case results <- result:
		case <-w.ctx.Done():
		}
	}()
}

func (w *worker) steeredQuiescenceFinished(result steerProbeResult) {
	fence := &w.steerFence
	if !fence.active || w.client != result.client || w.binding.Generation != result.generation || w.turn != result.turn || w.active != result.rootID || fence.revision != result.revision {
		return
	}
	select {
	case raw, ok := <-result.client.Events():
		w.cancelSteerProbe()
		if !ok {
			w.failed()
		} else {
			w.event(raw)
		}
		return
	default:
	}
	if result.client.BufferedOutput() {
		w.cancelSteerProbe()
		return
	}
	w.cancelSteerProbe()
	if result.err != nil {
		w.retireSteeredRoot("Steering state could not be confirmed. The task outcome is uncertain and will not be replayed automatically.")
		return
	}
	if result.settled {
		fence.settled = true
		w.maybeFinishSteeredRoot()
	} else {
		fence.nextProbe = time.Now().Add(stuckTaskQuietPeriod)
	}
}

func (w *worker) settleOutstandingSteers() bool {
	for id, r := range w.steers {
		if err := w.b.db.FinishSubmitted(r.inboxID, store.InboxUncertain); err != nil {
			w.b.fail(err)
			w.cancel()
			return false
		}
		delete(w.steers, id)
		w.log.LogAttrs(context.Background(), slog.LevelInfo, "steer completed", slog.String("event", "steer_complete"), slog.String("result", "uncertain"), slog.Int64("inbox_id", r.inboxID), slog.Int64("root_inbox_id", r.rootID), slog.Int64("generation", r.generation), slog.Uint64("turn", r.turn), slog.Uint64("client_id", r.clientID))
	}
	return true
}

func (w *worker) stopSteeredRoot() {
	w.retireSteeredRoot("Steering was interrupted. The task outcome is uncertain and will not be replayed automatically.")
}

func (w *worker) retireSteeredRoot(notice string) {
	if w.research.root {
		w.retireResearch(notice)
		return
	}
	w.releaseRuntimeWithReason(true, "failure")
	w.endControlOperation()
	w.completeSessionOperation()
	if w.ctx.Err() != nil || !w.settleOutstandingSteers() {
		return
	}
	if w.active != 0 && !w.finishUncertain(notice) {
		return
	}
	w.clearSteerFence()
}

func (w *worker) steeredTick(now time.Time) {
	fence := &w.steerFence
	if !fence.active || w.client == nil || fence.settled || fence.probeCancel != nil {
		return
	}
	if !fence.terminalSeen || len(w.steers) != 0 {
		return
	}
	if !w.lastActivity.IsZero() && now.Sub(w.lastActivity) >= stuckTaskQuietPeriod {
		w.probeSteeredQuiescence()
	}
}
