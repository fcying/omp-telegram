package bridge

import (
	"context"
	"errors"
	"time"

	"omp-telegram/internal/omp"
)

func (w *worker) sessionDeleteAvailable(sessionID, workspace string) (bool, error) {
	leased, err := w.b.db.ResearchSessionLeased(w.b.bot.ID, sessionID)
	if err != nil || leased {
		return false, err
	}
	bindings, err := w.b.db.RunningBindings(w.b.bot.ID)
	if err != nil {
		return false, err
	}
	for _, binding := range bindings {
		if binding.SessionID == "" {
			if sameWorkspace(binding.Workspace, workspace) {
				return false, nil
			}
		} else if sessionIDsMatch(binding.SessionID, sessionID) {
			return false, nil
		}
	}
	intents, err := w.b.db.PendingStarts(w.b.bot.ID)
	if err != nil {
		return false, err
	}
	for _, intent := range intents {
		if intent.Kind == "resume" && (sessionIDsMatch(intent.Session, sessionID) || !validSessionID(intent.Session) && sameWorkspace(intent.Workspace, workspace)) {
			return false, nil
		}
	}
	return true, nil
}

func (w *worker) confirmSessionDelete(c confirmation, sessionIndex int, messageID int64) {
	w.clearKeyboard(messageID)
	if c.epoch != w.b.bindingsEpoch.Load() || !w.persistedBindingGenerationMatches(c.generation) {
		w.say("The saved binding changed. Use /resume again.")
		return
	}
	if w.resumeBusy() {
		w.say("Wait for the current task and queue to finish before deleting a session.")
		return
	}
	if sessionIndex < 0 || sessionIndex >= len(c.sessions) {
		return
	}
	selected := c.sessions[sessionIndex]
	if !sameWorkspace(selected.CWD, c.workspace) {
		w.say("The selected session is not in this working directory.")
		return
	}
	if !validSessionID(selected.ID) {
		w.say("omp returned an unsupported session ID.")
		return
	}
	if w.b.sessionInUse(selected.ID) {
		w.say("This session is currently active. Close it before deleting.")
		return
	}
	available, err := w.sessionDeleteAvailable(selected.ID, c.workspace)
	if err != nil {
		w.say("Failed to check the session state. No session was deleted.")
		return
	}
	if !available {
		w.say("This session is currently active or has a retained autoresearch workspace lease. Close active sessions; for a retained lease, resume its owner and confirm autoresearch off before deleting.")
		return
	}
	title := "Delete omp session?\n\nTitle: " + menuText(selected.Title, 80) + "\nID: " + selected.ID + "\n\nThis deletes the saved OMP session and its artifacts.\nThe workspace files will not be deleted."
	w.confirm(confirmation{action: "session_delete", workspace: c.workspace, epoch: c.epoch, user: c.user, sessions: []omp.SessionSummary{selected}}, title, []string{"Delete", "Cancel"})
}

func (w *worker) beginSessionDelete(c confirmation) {
	if c.epoch != w.b.bindingsEpoch.Load() || !w.persistedBindingGenerationMatches(c.generation) {
		w.say("The saved binding changed. Use /resume again.")
		return
	}
	if w.resumeBusy() || len(c.sessions) != 1 {
		w.say("Wait for the current task and queue to finish before deleting a session.")
		return
	}
	selected := c.sessions[0]
	if !validSessionID(selected.ID) || !sameWorkspace(selected.CWD, c.workspace) {
		w.say("The selected session is not in this working directory.")
		return
	}
	if !w.b.reserveDelete(w, selected.ID) {
		w.say("This session is currently active or being processed, or has a retained autoresearch workspace lease. Resume a retained lease's owner and confirm autoresearch off before deleting.")
		return
	}
	available, err := w.sessionDeleteAvailable(selected.ID, c.workspace)
	if err != nil || !available {
		w.b.releaseDelete(w)
		if err != nil {
			w.say("Failed to check the session state. No session was deleted.")
		} else {
			w.say("This session is currently active or has a retained autoresearch workspace lease. Close active sessions; for a retained lease, resume its owner and confirm autoresearch off before deleting.")
		}
		return
	}
	if w.deleteResults == nil {
		w.deleteResults = make(chan sessionDeleteResult, 1)
	}
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	w.deleteCancel = cancel
	w.deleteRequest++
	request := w.deleteRequest
	cfg := omp.Config{Binary: w.b.cfg.OMP, CWD: c.workspace, Args: w.b.cfg.OMPArgs, Environment: w.b.cfg.OMPEnvironment}
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		err := omp.DeleteSession(ctx, cfg, selected.ID, w.b.rpcLog)
		result := sessionDeleteResult{request: request, generation: c.generation, epoch: c.epoch, user: c.user, sessionID: selected.ID, err: err}
		select {
		case w.deleteResults <- result:
		case <-w.ctx.Done():
			w.b.releaseDelete(w)
		}
	}()
}

func (w *worker) sessionDeleteFinished(result sessionDeleteResult) {
	defer w.b.releaseDelete(w)
	if result.request != w.deleteRequest || w.deleteCancel == nil {
		return
	}
	w.deleteCancel()
	w.deleteCancel = nil
	if result.err != nil {
		if errors.Is(result.err, omp.ErrSessionFileGone) {
			w.b.sessionMu.Lock()
			w.b.bindingsEpoch.Add(1)
			w.b.sessionMu.Unlock()
		}
		w.log.Warn("native session deletion failed", "event", "session_delete", "session_id", result.sessionID, "result", "failed")
		w.say("Failed to delete the omp session. Use /resume to check its status before trying again.")
		return
	}
	w.b.sessionMu.Lock()
	cleanupErr := w.b.db.DeletePinnedSession(w.b.bot.ID, result.sessionID)
	refresh := result.epoch == w.b.bindingsEpoch.Load()
	epoch := w.b.bindingsEpoch.Add(1)
	w.b.sessionMu.Unlock()
	if cleanupErr != nil {
		w.b.storeLog.Error("session pin cleanup failed", "event", "session_delete", "session_id", result.sessionID, "error_kind", "persistence")
		w.say("Session deleted, but failed to clear pinned sessions.")
	} else {
		w.log.Info("native session deleted", "event", "session_delete", "session_id", result.sessionID, "result", "done")
		w.say("Session deleted.")
	}
	if refresh && epoch == w.b.bindingsEpoch.Load() && result.generation == w.binding.Generation && w.persistedBindingGenerationMatches(result.generation) {
		w.requestSessionList(result.user, "delete_refresh", "")
	}
}

func (w *worker) drainDeleteResults() {
	for {
		select {
		case <-w.deleteResults:
			w.b.releaseDelete(w)
		default:
			return
		}
	}
}
