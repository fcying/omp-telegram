package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
)

type workspaceMoveRequest struct {
	client       *omp.Client
	finalAction  *finalActionDispatch
	generation   int64
	sessionID    string
	binding      store.Binding
	inbox        int64
	target       string
	phase        string
	lookupCancel context.CancelFunc
	submitted    bool
}
type workspaceMoveResult struct {
	AgentInvoked bool            `json:"agentInvoked"`
	Info         omp.SessionInfo `json:"info"`
}

type workspaceMoveVerificationError struct {
	cause error
}

func (e *workspaceMoveVerificationError) Error() string { return "workspace move verification failed" }

func (e *workspaceMoveVerificationError) Unwrap() error { return e.cause }

var (
	errWorkspaceMoveUsage = errors.New("workspace move path required")
	errWorkspaceMovePath  = errors.New("invalid workspace move path")
	errWorkspaceMoveDir   = errors.New("workspace move target unavailable")
)

func parseWorkspaceMovePath(argument string) (string, error) {
	path := strings.TrimSpace(argument)
	if path == "" {
		return "", errWorkspaceMoveUsage
	}
	if path[0] == '"' {
		unquoted, err := strconv.Unquote(path)
		if err != nil {
			return "", errWorkspaceMoveUsage
		}
		path = unquoted
	} else if path[0] == '\'' {
		if len(path) < 2 || path[len(path)-1] != '\'' {
			return "", errWorkspaceMoveUsage
		}
		path = path[1 : len(path)-1]
	}
	if path == "" || !filepath.IsAbs(path) || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return "", errWorkspaceMovePath
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", errWorkspaceMoveDir
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", errWorkspaceMoveDir
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errWorkspaceMoveDir
	}
	return resolved, nil
}

func (w *worker) workspaceMoveBusy() bool {
	return w.sessionControlBusy() || len(w.queue) != 0 || w.rpcOperationActive || len(w.rpcOperations) != 0 ||
		w.replyStatus.pending != nil || w.autoSessionName != nil || w.autoSessionNameFence != nil ||
		w.nativeControl != nil || w.ask != nil || w.askSending != 0 || w.argsApply != nil ||
		w.resumeCancel != nil || w.exportCancel != nil || w.deleteCancel != nil || w.startIntent != nil ||
		w.hasRuntimeConfirmation()
}

func (w *worker) beginWorkspaceMove(in incoming, argument string) {
	target, err := parseWorkspaceMovePath(argument)
	if err != nil {
		switch {
		case errors.Is(err, errWorkspaceMoveUsage):
			w.say(w.tr(i18n.Msg.UsageMoveWorkspace))
		case errors.Is(err, errWorkspaceMovePath):
			w.say(w.tr(i18n.Msg.InvalidWorkingDirectory))
		default:
			w.say(w.tr(i18n.Msg.ProjectPathMustAccessibleDirectory))
		}
		w.mark(in.id, store.InboxCancelled)
		return
	}
	if w.workspaceMoveBusy() {
		w.say(w.tr(i18n.Msg.WorkspaceMoveRequiresIdleTopic))
		w.mark(in.id, store.InboxCancelled)
		return
	}
	client, err := w.ensureRuntime()
	if err != nil {
		if w.ctx.Err() == nil {
			w.say(w.errorText(err))
		}
		w.mark(in.id, store.InboxCancelled)
		return
	}
	if !w.binding.Running || !validSessionID(w.binding.SessionID) || w.sessionID != w.binding.SessionID {
		w.say(w.tr(i18n.Msg.NativeControlUnavailable))
		w.mark(in.id, store.InboxCancelled)
		return
	}
	catalog := client.CommandCatalog()
	if catalog.State != omp.CatalogUnknown &&
		(catalog.State != omp.CatalogReady || !strings.EqualFold(catalog.SessionID, w.sessionID) || !catalog.HasBuiltinCommand("move")) {
		w.say(w.tr(i18n.Msg.NativeControlUnavailable))
		w.mark(in.id, store.InboxCancelled)
		return
	}
	if sameWorkspace(target, w.binding.Workspace) {
		if w.mark(in.id, store.InboxDone) {
			w.say(w.tr(i18n.Msg.WorkspaceMoveCompleted, w.binding.Workspace, w.sessionID))
		}
		return
	}
	previous := w.binding
	w.b.sessionMu.Lock()
	current, readErr := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	shared, sharedErr := w.b.db.HasOtherSessionBinding(w.b.bot.ID, w.key.chat, w.key.thread, w.sessionID)
	w.b.sessionMu.Unlock()
	if readErr != nil || sharedErr != nil {
		w.b.storeLog.Error("workspace move binding check failed", "event", "binding_read_failed", "reason", "workspace_move_preflight", "error_kind", "persistence")
		w.b.fail(errors.New("workspace move preflight persistence failed"))
		w.mark(in.id, store.InboxCancelled)
		return
	}
	if current != previous {
		w.say(w.tr(i18n.Msg.SavedBindingChangedUseResumeAgain))
		w.mark(in.id, store.InboxCancelled)
		return
	}
	if shared {
		w.say(w.tr(i18n.Msg.WorkspaceMoveSharedSession))
		w.mark(in.id, store.InboxCancelled)
		return
	}
	request := &workspaceMoveRequest{
		finalAction: w.finalAction, client: client, generation: previous.Generation, sessionID: w.sessionID,
		binding: previous, inbox: in.id, target: target,
	}
	w.workspaceMove = request
	w.beginControlOperation(controlNative)
	if catalog.State == omp.CatalogUnknown {
		request.phase = "catalog"
		lookupCtx, lookupCancel := context.WithTimeout(w.ctx, 15*time.Second)
		request.lookupCancel = lookupCancel
		w.startOperation("workspace_move", client, func(context.Context) (json.RawMessage, error) {
			defer lookupCancel()
			_, err := client.RefreshCommandCatalog(lookupCtx)
			return nil, err
		}, 0, "", request)
		return
	}
	w.submitWorkspaceMove(request)
}

func (w *worker) submitWorkspaceMove(request *workspaceMoveRequest) {
	catalog := request.client.CommandCatalog()
	if !w.workspaceMoveScope(request) || catalog.State != omp.CatalogReady ||
		!strings.EqualFold(catalog.SessionID, request.sessionID) || !catalog.HasBuiltinCommand("move") {
		w.endWorkspaceMove(request)
		w.releaseFinalAction(request.finalAction)
		w.mark(request.inbox, store.InboxCancelled)
		w.say(w.tr(i18n.Msg.NativeControlUnavailable))
		return
	}
	if !w.consumeFinalAction() {
		w.endWorkspaceMove(request)
		w.mark(request.inbox, store.InboxCancelled)
		return
	}
	if !w.mark(request.inbox, store.InboxSubmitted) {
		w.endWorkspaceMove(request)
		return
	}
	request.phase = "prompt"
	request.submitted = true
	message := "/move " + request.target
	w.startOperation("workspace_move", request.client, func(ctx context.Context) (json.RawMessage, error) {
		prompt, err := request.client.Call(ctx, "prompt", map[string]any{"message": message})
		if err != nil {
			return prompt, err
		}
		var ack struct {
			AgentInvoked *bool `json:"agentInvoked"`
		}
		if json.Unmarshal(prompt, &ack) != nil || ack.AgentInvoked == nil || *ack.AgentInvoked {
			return prompt, nil
		}
		info, err := request.client.SessionInfo(ctx)
		if err != nil {
			return nil, &workspaceMoveVerificationError{cause: err}
		}
		return json.Marshal(workspaceMoveResult{AgentInvoked: false, Info: info})
	}, 0, "", request)
}

func (w *worker) workspaceMoveScope(request *workspaceMoveRequest) bool {
	return w.workspaceMove == request && w.client == request.client &&
		w.binding.Generation == request.generation && w.sessionID == request.sessionID &&
		strings.EqualFold(w.binding.SessionID, request.sessionID)
}

func (w *worker) endWorkspaceMove(request *workspaceMoveRequest) {
	if w.workspaceMove != request {
		return
	}
	w.workspaceMove = nil
	if w.controlOp == controlNative {
		w.endControlOperation()
	}
	if request.lookupCancel != nil {
		request.lookupCancel()
		request.lookupCancel = nil
	}
}

func (w *worker) cancelWorkspaceMove() {
	request := w.workspaceMove
	if request == nil {
		return
	}
	w.endWorkspaceMove(request)
	if request.submitted {
		w.clearQueue()
		w.mark(request.inbox, store.InboxUncertain)
	} else {
		w.releaseFinalAction(request.finalAction)
		w.mark(request.inbox, store.InboxCancelled)
	}
}

func (w *worker) workspaceMoveFinished(result operationResult) {
	request, ok := result.meta.(*workspaceMoveRequest)
	if !ok || w.workspaceMove != request {
		return
	}
	if !w.workspaceMoveScope(request) {
		w.endWorkspaceMove(request)
		w.clearQueue()
		w.mark(request.inbox, store.InboxUncertain)
		if w.client == request.client {
			w.retireTaskRuntime(w.tr(i18n.Msg.WorkspaceMoveUnconfirmed))
		}
		return
	}
	if request.phase == "catalog" {
		catalog := request.client.CommandCatalog()
		if !w.workspaceMoveScope(request) || catalog.State != omp.CatalogReady ||
			!strings.EqualFold(catalog.SessionID, request.sessionID) || !catalog.HasBuiltinCommand("move") {
			w.endWorkspaceMove(request)
			w.releaseFinalAction(result.finalAction)
			w.mark(request.inbox, store.InboxCancelled)
			w.say(w.tr(i18n.Msg.NativeControlUnavailable))
			return
		}
		w.submitWorkspaceMove(request)
		return
	}
	var verificationErr *workspaceMoveVerificationError
	if errors.As(result.err, &verificationErr) {
		w.endWorkspaceMove(request)
		w.clearQueue()
		w.mark(request.inbox, store.InboxUncertain)
		w.retireTaskRuntime(w.tr(i18n.Msg.WorkspaceMoveUnconfirmed))
		return
	}

	if result.err != nil {
		uncertain := request.submitted || uncertainOperationOutcome(result.err, result.cancelled)
		w.endWorkspaceMove(request)
		if uncertain {
			w.clearQueue()
			w.mark(request.inbox, store.InboxUncertain)
			w.retireTaskRuntime(w.tr(i18n.Msg.WorkspaceMoveUnconfirmed))
		} else {
			w.mark(request.inbox, store.InboxCancelled)
			w.say(w.tr(i18n.Msg.WorkspaceMoveRejected))
		}
		return
	}
	var moved workspaceMoveResult
	if json.Unmarshal(result.data, &moved) != nil || moved.AgentInvoked || !strings.EqualFold(moved.Info.ID, request.sessionID) ||
		!filepath.IsAbs(moved.Info.File) || !sameWorkspace(moved.Info.CWD, request.target) {
		if moved.Info.ID != "" && strings.EqualFold(moved.Info.ID, request.sessionID) &&
			sameWorkspace(moved.Info.CWD, request.binding.Workspace) && sameSessionFile(moved.Info.File, request.binding.Session) {
			w.endWorkspaceMove(request)
			w.mark(request.inbox, store.InboxDone)
			w.say(w.tr(i18n.Msg.WorkspaceMoveNoChange, request.binding.Workspace))
			return
		}
		w.endWorkspaceMove(request)
		w.clearQueue()
		w.mark(request.inbox, store.InboxUncertain)
		w.retireTaskRuntime(w.tr(i18n.Msg.WorkspaceMoveUnconfirmed))
		return
	}
	if !w.claimSession(moved.Info.File, moved.Info.ID) {
		w.endWorkspaceMove(request)
		w.clearQueue()
		w.mark(request.inbox, store.InboxUncertain)
		w.retireTaskRuntime(w.tr(i18n.Msg.WorkspaceMoveUnconfirmed))
		return
	}
	next := request.binding
	next.Workspace, next.Session, next.Generation = moved.Info.CWD, moved.Info.File, request.generation+1
	next.LastUsedAt, next.Running, next.Interrupted = time.Now().Unix(), true, false
	w.b.sessionMu.Lock()
	err := w.b.db.MoveBinding(request.binding, next)
	w.b.sessionMu.Unlock()
	if err != nil {
		w.endWorkspaceMove(request)
		w.clearQueue()
		w.mark(request.inbox, store.InboxUncertain)
		if !errors.Is(err, store.ErrSessionShared) && !errors.Is(err, store.ErrStaleBinding) {
			w.b.storeLog.Error("workspace move binding commit failed", "event", "binding_write_failed", "reason", "workspace_move", "error_kind", "persistence")
			w.b.fail(err)
		}
		w.retireTaskRuntime(w.tr(i18n.Msg.WorkspaceMoveUnconfirmed))
		return
	}
	w.binding = next
	w.b.bindingsEpoch.Add(1)
	w.endWorkspaceMove(request)
	w.mark(request.inbox, store.InboxDone)
	w.clearConfirmations()
	w.touchActivity()
	w.touchBinding()
	w.refreshCommandCatalog(request.client)
	w.say(w.tr(i18n.Msg.WorkspaceMoveCompleted, next.Workspace, next.SessionID))
}
