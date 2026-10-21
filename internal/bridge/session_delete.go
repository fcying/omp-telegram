package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
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
			if binding.Workspace == workspace || sameWorkspace(binding.Workspace, workspace) {
				return false, nil
			}
		}
	}
	intents, err := w.b.db.PendingStarts(w.b.bot.ID)
	if err != nil {
		return false, err
	}
	for _, intent := range intents {
		if intent.Kind == "resume" && (sessionIDsMatch(intent.Session, sessionID) || !validSessionID(intent.Session) && (intent.Workspace == workspace || sameWorkspace(intent.Workspace, workspace))) {
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
	w.deleteContext = ctx
	w.deleteRequest++
	request := w.deleteRequest
	cfg := omp.Config{Binary: w.b.cfg.OMP, CWD: c.workspace, Args: c.ompArgs, Environment: w.b.cfg.OMPEnvironment}
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		err := omp.DeleteSession(ctx, cfg, selected.ID, w.b.rpcLog)
		result := sessionDeleteResult{request: request, generation: c.generation, epoch: c.epoch, user: c.user, sessionID: selected.ID, err: err}
		w.deleteResults <- result
	}()
}

func (w *worker) beginBindingSessionDelete(c confirmation, binding store.Binding) {
	if w.forgetPending || w.deleteCancel != nil {
		w.say(w.tr(i18n.Msg.AnotherBindingDeletionPendingTryAgainLater))
		return
	}
	if !validSessionID(binding.SessionID) || !filepath.IsAbs(binding.Session) || !filepath.IsAbs(binding.Workspace) {
		w.say(w.tr(i18n.Msg.BindingSessionIdentityUnavailable))
		return
	}
	if !w.b.reserveDelete(w, binding.SessionID) {
		w.say(w.tr(i18n.Msg.SessionCurrentlyActiveProcessedAnotherConversationOperationNoDeletion))
		return
	}
	current, err := w.b.db.Binding(binding.Bot, binding.Chat, binding.Thread)
	if err != nil || current.Generation != binding.Generation || current.Running || current.Session != binding.Session || current.SessionID != binding.SessionID || current.Workspace != binding.Workspace {
		w.b.releaseDelete(w)
		w.say(w.tr(i18n.Msg.SavedBindingChangedUseBindingsRefresh))
		return
	}
	available, err := w.sessionDeleteAvailable(binding.SessionID, binding.Workspace)
	if err != nil || !available {
		w.b.releaseDelete(w)
		w.say(w.tr(i18n.Msg.FailedCheckSessionStateNoSessionDeleted))
		return
	}
	if w.deleteResults == nil {
		w.deleteResults = make(chan sessionDeleteResult, 1)
	}
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	w.deleteCancel = cancel
	w.deleteContext = ctx
	w.deleteRequest++
	result := sessionDeleteResult{request: w.deleteRequest, generation: c.generation, epoch: w.b.bindingsEpoch.Load(), user: c.user, sessionID: binding.SessionID, sessionFile: binding.Session, bindingDelete: &c}
	cfg := omp.Config{Binary: w.b.cfg.OMP, CWD: binding.Workspace, Args: c.ompArgs, Environment: w.b.cfg.OMPEnvironment}
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		result.err = omp.DeleteBoundSession(ctx, cfg, binding.SessionID, binding.Session, w.b.rpcLog)
		w.deleteResults <- result
	}()
}

func (w *worker) sessionDeleteFinished(result sessionDeleteResult) {
	if result.request != w.deleteRequest || w.deleteCancel == nil {
		return
	}
	defer w.b.releaseDelete(w)
	defer w.deleteCancel()
	w.deleteCancel = nil
	deleteContext := w.deleteContext
	w.deleteContext = nil
	missing := result.bindingDelete != nil && errors.Is(result.err, omp.ErrBoundSessionMissing)
	if result.err != nil && !missing {
		if errors.Is(result.err, omp.ErrSessionFileGone) {
			w.b.sessionMu.Lock()
			w.b.bindingsEpoch.Add(1)
			w.b.sessionMu.Unlock()
		}
		w.log.Warn("native session deletion failed", "event", "session_delete", "session_id", result.sessionID, "result", "failed", "reason", omp.ClassifyError(result.err))
		if w.ctx.Err() != nil {
			return
		}
		if result.bindingDelete != nil {
			w.say(w.tr(i18n.Msg.BindingSessionDeleteFailed))
		} else {
			w.say(w.tr(i18n.Msg.FailedDeleteOmpSessionUseResumeCheckStatusBefore))
		}
		return
	}
	w.b.sessionMu.Lock()
	var deletedBindings int64
	var cleanupErr error
	missingUnconfirmed := false
	if missing {
		cleanupErr = deleteContext.Err()
		if cleanupErr == nil {
			_, err := os.Lstat(result.sessionFile)
			if !errors.Is(err, os.ErrNotExist) {
				missingUnconfirmed = true
			} else {
				deletedBindings, cleanupErr = w.b.db.DeleteMissingSessionReferences(deleteContext, w.b.bot.ID, result.sessionID, result.sessionFile)
			}
		}
	} else {
		deletedBindings, cleanupErr = w.b.db.DeleteSessionReferences(w.b.bot.ID, result.sessionID)
	}
	if missing && cleanupErr != nil && deleteContext.Err() != nil {
		w.b.sessionMu.Unlock()
		w.log.Info("missing session reference cleanup cancelled", "event", "session_delete", "session_id", result.sessionID, "result", "cancelled", "reason", omp.ClassifyError(deleteContext.Err()))
		return
	}
	if cleanupErr == nil && !missingUnconfirmed {
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
	if cleanupErr != nil && !errors.Is(cleanupErr, store.ErrSessionPathConflict) {
		w.b.storeLog.Error("session reference cleanup failed", "event", "session_delete", "session_id", result.sessionID, "error_kind", "persistence", "reason", "reference_cleanup_failed")
	} else if cleanupErr == nil && !missingUnconfirmed {
		if missing {
			w.log.Info("missing session references cleared", "event", "session_delete", "session_id", result.sessionID, "bindings_removed", deletedBindings, "result", "missing", "reason", "file_missing")
		} else {
			w.log.Info("native session deleted", "event", "session_delete", "session_id", result.sessionID, "bindings_removed", deletedBindings, "result", "done")
		}
	}
	if w.ctx.Err() != nil {
		return
	}
	if missingUnconfirmed {
		w.say(w.tr(i18n.Msg.BindingSessionMissingUnconfirmed))
	} else if errors.Is(cleanupErr, store.ErrSessionPathConflict) {
		w.say(w.tr(i18n.Msg.BindingSessionMissingPathConflict))
	} else if cleanupErr != nil {
		if missing {
			w.say(w.tr(i18n.Msg.BindingSessionMissingCleanupFailed))
		} else {
			w.say(w.tr(i18n.Msg.SessionDeletedButFailedClearSavedBindingsPins))
		}
	} else {
		if missing {
			w.say(w.tr(i18n.Msg.BindingSessionMissingReferencesCleared))
		} else if result.bindingDelete != nil {
			w.say(w.tr(i18n.Msg.SavedBindingAndSessionDeleted))
		} else {
			w.say(w.tr(i18n.Msg.SessionDeleted))
		}
	}
	if c := result.bindingDelete; c != nil {
		if result.generation == w.conversationGeneration() {
			w.showBindings(c.user, c.bindingsOld, c.page)
		}
		return
	}
	if refresh && epoch == w.b.bindingsEpoch.Load() && result.generation == w.binding.Generation && w.persistedBindingGenerationMatches(result.generation) {
		w.requestSessionList(result.user, "delete_refresh", "")
	}
}

func (w *worker) drainDeleteResults() {
	// One pending deletion owns the buffered result slot; run joins producers first.
	for {
		select {
		case result := <-w.deleteResults:
			w.sessionDeleteFinished(result)
		default:
			return
		}
	}
}
