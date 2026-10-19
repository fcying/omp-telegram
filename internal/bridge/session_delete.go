package bridge

import (
	"context"
	"errors"
	"time"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
)

func (w *worker) sessionDeleteAvailable(sessionID, workspace string) (bool, error) {
	migrations, err := w.b.db.PendingMigrations(w.b.bot.ID)
	if err != nil {
		return false, err
	}
	for _, migration := range migrations {
		if sessionIDsMatch(migration.Source.SessionID, sessionID) ||
			migration.CandidateID != "" && sessionIDsMatch(migration.CandidateID, sessionID) {
			return false, nil
		}
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
		w.say(w.tr(i18n.Msg.SavedBindingChangedUseResumeAgain))
		return
	}
	if w.resumeBusy() {
		w.say(w.tr(i18n.Msg.WaitCurrentTaskQueueFinishBeforeDeletingSession))
		return
	}
	if sessionIndex < 0 || sessionIndex >= len(c.sessions) {
		return
	}
	selected := c.sessions[sessionIndex]
	if !sameWorkspace(selected.CWD, c.workspace) {
		w.say(w.tr(i18n.Msg.SelectedSessionNotWorkingDirectory))
		return
	}
	if !validSessionID(selected.ID) {
		w.say(w.tr(i18n.Msg.OmpReturnedUnsupportedSessionID))
		return
	}
	busy, elsewhere := w.b.sessionDeleteStatus(w, selected.ID)
	if busy {
		if elsewhere {
			w.say(w.tr(i18n.Msg.SessionCurrentlyActiveProcessedAnotherConversationNoDeletionStarted))
		} else {
			w.say(w.tr(i18n.Msg.SessionCurrentlyActiveProcessedConversationNoDeletionStartedStop))
		}
		return
	}
	available, err := w.sessionDeleteAvailable(selected.ID, c.workspace)
	if err != nil {
		w.say(w.tr(i18n.Msg.FailedCheckSessionStateNoSessionDeleted))
		return
	}
	if !available {
		w.say(w.tr(i18n.Msg.OMPSessionStillStartingRestoredWorkspaceWaitOperationFinish))
		return
	}
	title := w.tr(i18n.Msg.DeleteOmpSessionTitleIDDeletesSavedOMPSession, menuText(selected.Title, 80), selected.ID)
	w.confirm(confirmation{action: "session_delete", workspace: c.workspace, epoch: c.epoch, user: c.user, sessions: []omp.SessionSummary{selected}, ompArgs: c.ompArgs}, title, []string{w.tr(i18n.Msg.Delete), w.tr(i18n.Msg.Cancel)})
}

func (w *worker) beginSessionDelete(c confirmation) {
	if c.epoch != w.b.bindingsEpoch.Load() || !w.persistedBindingGenerationMatches(c.generation) {
		w.say(w.tr(i18n.Msg.SavedBindingChangedUseResumeAgain))
		return
	}
	if w.resumeBusy() || len(c.sessions) != 1 {
		w.say(w.tr(i18n.Msg.WaitCurrentTaskQueueFinishBeforeDeletingSession))
		return
	}
	selected := c.sessions[0]
	if !validSessionID(selected.ID) || !sameWorkspace(selected.CWD, c.workspace) {
		w.say(w.tr(i18n.Msg.SelectedSessionNotWorkingDirectory))
		return
	}
	if !w.b.reserveDelete(w, selected.ID) {
		w.say(w.tr(i18n.Msg.SessionCurrentlyActiveProcessedAnotherConversationOperationNoDeletion))
		return
	}
	available, err := w.sessionDeleteAvailable(selected.ID, c.workspace)
	if err != nil || !available {
		w.b.releaseDelete(w)
		if err != nil {
			w.say(w.tr(i18n.Msg.FailedCheckSessionStateNoSessionDeleted))
		} else {
			w.say(w.tr(i18n.Msg.SessionActiveIdentityStillRestoredWaitWorkFinishBefore))
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
	cfg := omp.Config{Binary: w.b.cfg.OMP, CWD: c.workspace, Args: c.ompArgs, Environment: w.b.cfg.OMPEnvironment}
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
		w.say(w.tr(i18n.Msg.FailedDeleteOmpSessionUseResumeCheckStatusBefore))
		return
	}
	w.b.sessionMu.Lock()
	deletedBindings, cleanupErr := w.b.db.DeleteSessionReferences(w.b.bot.ID, result.sessionID)
	if cleanupErr == nil {
		for path, claim := range w.b.sessionClaims {
			if !sessionIDsMatch(claim.id, result.sessionID) {
				continue
			}
			if claim.migration || claim.owner != nil && (claim.owner.sessionRuntimeStarting.Load() || claim.owner.sessionRuntimeActive.Load()) {
				continue
			}
			delete(w.b.sessionClaims, path)
		}
	}
	refresh := result.epoch == w.b.bindingsEpoch.Load()
	epoch := w.b.bindingsEpoch.Add(1)
	w.b.sessionMu.Unlock()
	if cleanupErr != nil {
		w.b.storeLog.Error("session reference cleanup failed", "event", "session_delete", "session_id", result.sessionID, "error_kind", "persistence")
		w.say(w.tr(i18n.Msg.SessionDeletedButFailedClearSavedBindingsPins))
	} else {
		w.log.Info("native session deleted", "event", "session_delete", "session_id", result.sessionID, "bindings_removed", deletedBindings, "result", "done")
		w.say(w.tr(i18n.Msg.SessionDeleted))
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
