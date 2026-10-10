package bridge

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
)

type queuedInputKind uint8

const (
	queuedPrompt queuedInputKind = iota
	queuedSlashPending
	queuedNativeCommand
)

type commandCatalogResult struct {
	client  *omp.Client
	request uint64
	epoch   uint64
	err     error
}

type nativeCommandCatalog struct {
	results        chan commandCatalogResult
	cancel         context.CancelFunc
	request        uint64
	waitEpoch      uint64
	pausedClientID uint64
	pausedEpoch    uint64
	menuClientID   uint64
	menuRevision   uint64
}

// The client owns authoritative reader-ordered snapshots; events only wake classification.
func (w *worker) refreshCommandCatalog(client *omp.Client) {
	if w.nativeCatalog.cancel != nil {
		return
	}
	epoch := client.CommandCatalog().Epoch
	if w.nativeCatalog.pausedClientID == client.ID() && w.nativeCatalog.pausedEpoch == epoch {
		return
	}
	if w.nativeCatalog.results == nil {
		w.nativeCatalog.results = make(chan commandCatalogResult, 1)
	}
	w.nativeCatalog.request++
	request := w.nativeCatalog.request
	ctx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
	w.nativeCatalog.cancel = cancel
	w.nativeCatalog.waitEpoch = epoch
	w.background.Add(1)
	results := w.nativeCatalog.results
	go func() {
		defer w.background.Done()
		epoch, err := client.RefreshCommandCatalog(ctx)
		select {
		case results <- commandCatalogResult{client: client, request: request, epoch: epoch, err: err}:
		case <-w.ctx.Done():
		}
	}()
}

func (w *worker) clearCommandCatalog() {
	if w.nativeCatalog.cancel != nil {
		w.nativeCatalog.cancel()
		w.nativeCatalog.cancel = nil
	}
	w.nativeCatalog.request++
	w.nativeCatalog.pausedClientID = 0
	w.nativeCatalog.pausedEpoch = 0
}

func (w *worker) commandCatalogFinished(result commandCatalogResult) {
	if result.request != w.nativeCatalog.request || w.client != result.client {
		return
	}
	if w.nativeCatalog.cancel != nil {
		w.nativeCatalog.cancel()
		w.nativeCatalog.cancel = nil
	}
	catalog := result.client.CommandCatalog()
	select {
	case <-result.client.Done():
		w.failed()
		return
	default:
	}
	if result.err != nil {
		waitInterrupted := errors.Is(result.err, context.DeadlineExceeded) || errors.Is(result.err, context.Canceled)
		rejected := omp.ClassifyError(result.err) == "rejected"
		if !waitInterrupted && !rejected {
			if w.ctx.Err() == nil {
				w.logRuntimeEvent(slog.LevelWarn, "command_catalog_failed", omp.ClassifyError(result.err), "native command discovery failed", w.sessionID)
				w.failed()
			}
			return
		}
		// Reader-published snapshots remain authoritative even after a waiter expires or is rejected.
		if catalog.State != omp.CatalogUnknown {
			w.resolveSlashInputs()
			return
		}
		if result.epoch != catalog.Epoch && (rejected || w.nativeCatalog.waitEpoch != catalog.Epoch) {
			// A new scope may refresh once; a timed-out waiter still joins the old in-flight query.
			w.refreshCommandCatalog(result.client)
			return
		}
		// Pause discovery for this scope instead of repeatedly rejoining an unanswered query.
		// Scope invalidation or a reader-published update restores command classification.
		w.nativeCatalog.pausedClientID = result.client.ID()
		w.nativeCatalog.pausedEpoch = catalog.Epoch
		if w.ctx.Err() == nil {
			w.logRuntimeEvent(slog.LevelWarn, "command_catalog_unavailable", omp.ClassifyError(result.err), "native command discovery unavailable", w.sessionID)
			w.say("Native command discovery is unavailable. Waiting slash commands were not submitted; the running task and runtime were preserved. Use /queue and Cancel on the waiting slash item to let later tasks continue.")
		}
		return
	}
	if catalog.State == omp.CatalogUnknown {
		w.refreshCommandCatalog(result.client)
	} else {
		w.resolveSlashInputs()
	}
}

func (w *worker) commandCatalogUpdated() {
	w.resolveSlashInputs()
	if client, ok := w.runtimeClient(); ok && client.CommandCatalog().State != omp.CatalogUnknown {
		w.nativeCatalog.pausedClientID = 0
		w.nativeCatalog.pausedEpoch = 0
	}
}

func telegramMenuCommandName(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if c != '_' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func (w *worker) syncCommandMenu() {
	client, ok := w.runtimeClient()
	if !ok {
		return
	}
	catalog := client.CommandCatalog()
	if w.nativeCatalog.menuClientID == client.ID() && w.nativeCatalog.menuRevision == catalog.Revision {
		return
	}
	var names []string
	for name := range catalog.ExecutableCommands {
		if !telegramMenuCommandName(name) {
			continue
		}
		invocation := "/" + name
		if knownBridgeCommand(invocation) || unsupportedNativeCommand(invocation, w.b.bot.Username) != "" {
			continue
		}
		names = append(names, name)
	}
	w.b.updateCommandMenu(w, names)
	w.nativeCatalog.menuClientID = client.ID()
	w.nativeCatalog.menuRevision = catalog.Revision
}

// Normalize only an advertised command, never unknown slash-prefixed user text.
func advertisedCommand(catalog omp.CommandCatalog, text, botUsername string) (string, string, bool) {
	// Extension/custom/file names can contain colons and split only at a literal space.
	end := strings.IndexByte(text, ' ')
	if end < 0 {
		end = len(text)
	}
	token := text[:end]
	command, foreign := parseSlashCommandToken(token, botUsername)
	if foreign {
		return "", "", false
	}
	name := strings.TrimPrefix(command, "/")
	if catalog.HasCommand(name) {
		if command == token {
			return name, text, true
		}
		return name, command + text[end:], true
	}
	// Builtins support these separators; refused advertised sources must not fall back to prompts.
	command, tail, foreign := parseSlashInvocation(text, botUsername)
	if foreign {
		return "", "", false
	}
	name = strings.TrimPrefix(command, "/")
	if catalog.HasBuiltinCommand(name) || catalog.HasCommand(name) && !catalog.HasExecutableCommand(name) {
		if len(command)+len(tail) == len(text) {
			return name, text, true
		}
		return name, command + tail, true
	}
	return "", "", false
}

// Reserve lifecycle commands even when discovery is unavailable. Falling back
// to a raw prompt would still let OMP execute these builtins.
func unsupportedNativeCommand(text, botUsername string) string {
	command, tail, foreign := parseSlashInvocation(text, botUsername)
	if foreign {
		return ""
	}
	switch command {
	case "/move", "/wt", "/worktree":
		return command
	case "/session":
		if strings.EqualFold(slashCommandArguments(tail), "delete") {
			return "/session delete"
		}
	}
	return ""
}

func (w *worker) unsupportedNativeCommandNotice(command string) {
	if command == "/session delete" {
		w.say("/session delete is not supported by the bridge because it deletes the active session. Use /close, then the /resume picker Delete control instead.")
		return
	}
	w.say(command + " is not supported by the bridge because it can move the session or workspace. Use /new or /resume instead.")
}

func (w *worker) unsupportedCommandSourceNotice(catalog omp.CommandCatalog, name string) {
	if catalog.HasExtensionCommand(name) {
		w.say("Extension commands are not supported by the bridge because they can change the native session without updating its binding. No command was submitted.")
		return
	}
	w.say("Commands with an unknown or unsupported source are not supported by the bridge. No command was submitted.")
}

// Unsupported discovery cannot prove that slash text is safe model input.
func (w *worker) rejectUnsupportedSlash(catalog omp.CommandCatalog, text string, inbox int64) bool {
	if catalog.State != omp.CatalogUnsupported || !strings.HasPrefix(text, "/") {
		return false
	}
	if !w.cancelQueuedTask(inbox) && !w.mark(inbox, store.InboxCancelled) {
		return true
	}
	w.say("OMP does not support native command discovery. Slash input was cancelled because its command source cannot be verified. Use an OMP version with command discovery, or send ordinary text without a leading slash.")
	return true
}

func (w *worker) routeSlashInput(in incoming, text string) {
	if command := unsupportedNativeCommand(text, w.b.bot.Username); command != "" {
		if w.mark(in.id, store.InboxCancelled) {
			w.unsupportedNativeCommandNotice(command)
		}
		return
	}
	if w.routeResearchOff(in, text) {
		return
	}
	client, err := w.ensureRuntime()
	if err != nil {
		if w.ctx.Err() != nil {
			return
		}
		w.say(err.Error())
		w.mark(in.id, store.InboxDone)
		return
	}
	catalog := client.CommandCatalog()
	if w.rejectUnsupportedSlash(catalog, text, in.id) {
		return
	}
	if catalog.State != omp.CatalogUnknown {
		if _, research := autoresearchArguments(text, w.b.bot.Username); research {
			if !w.researchAvailable(client) {
				w.mark(in.id, store.InboxCancelled)
				w.say("/autoresearch is not available from the current native extension catalog. No command was submitted.")
				return
			}
			_, normalized, _ := advertisedCommand(catalog, text, w.b.bot.Username)
			w.routeResearchInput(in, normalized, client, -1)
			return
		}
		if name, normalized, native := advertisedCommand(catalog, text, w.b.bot.Username); native {
			if !catalog.HasExecutableCommand(name) {
				if w.mark(in.id, store.InboxCancelled) {
					w.unsupportedCommandSourceNotice(catalog, name)
				}
				return
			}
			w.enqueueNativeCommand(in, normalized, name, catalog.SessionID)
		} else {
			w.routePrompt(in, text)
		}
		return
	}
	w.enqueueSlashInput(in, text, client)
}

func (w *worker) enqueueSlashInput(in incoming, text string, client *omp.Client) {
	if w.pendingInputFull() {
		w.say("The queue is full. This message was not submitted.")
		w.mark(in.id, store.InboxCancelled)
		return
	}
	w.queue = append(w.queue, queued{id: in.id, user: in.msg.From.ID, replyTo: in.msg.MessageID, text: text, displayText: text, reply: in.msg, kind: queuedSlashPending})
	w.refreshCommandCatalog(client)
}

func (w *worker) enqueueNativeCommand(in incoming, text, name, sessionID string) {
	if w.pendingInputFull() {
		w.say("The queue is full. This message was not submitted.")
		w.mark(in.id, store.InboxCancelled)
		return
	}
	w.queue = append(w.queue, queued{id: in.id, user: in.msg.From.ID, replyTo: in.msg.MessageID, text: text, displayText: text, kind: queuedNativeCommand, nativeName: name, nativeSession: sessionID})
}

func (w *worker) resolveSlashInputs() {
	w.syncCommandMenu()
	if w.research.control != nil {
		return
	}
	for i := 0; i < len(w.queue); {
		q := &w.queue[i]
		if q.kind != queuedSlashPending {
			i++
			continue
		}
		client, ok := w.runtimeClient()
		if !ok {
			return
		}
		// Reload the reader snapshot before each classification, including after asynchronous research lookup.
		catalog := client.CommandCatalog()
		if catalog.State == omp.CatalogUnknown {
			return
		}
		if w.rejectUnsupportedSlash(catalog, q.text, q.id) {
			continue
		}
		if _, research := autoresearchArguments(q.text, w.b.bot.Username); research {
			in := incoming{id: q.id, msg: q.reply}
			if !w.researchAvailable(client) {
				w.cancelQueuedTask(q.id)
				w.say("/autoresearch is not available from the current native extension catalog. No command was submitted.")
			} else {
				_, normalized, _ := advertisedCommand(catalog, q.text, w.b.bot.Username)
				if w.routeResearchInput(in, normalized, client, i) {
					i++
				}
			}
			if client != w.client || w.research.control != nil {
				return
			}
			continue
		}
		if name, normalized, native := advertisedCommand(catalog, q.text, w.b.bot.Username); native {
			if !catalog.HasExecutableCommand(name) {
				if w.cancelQueuedTask(q.id) {
					w.unsupportedCommandSourceNotice(catalog, name)
				}
				continue
			}
			q.kind, q.nativeName, q.text, q.reply = queuedNativeCommand, name, normalized, nil
			q.nativeSession = catalog.SessionID
			i++
			continue
		}
		if w.taskActive() && w.activeInputKind != queuedNativeCommand {
			in := incoming{id: q.id, msg: q.reply}
			text := q.text
			w.queue = append(w.queue[:i], w.queue[i+1:]...)
			w.steer(in, text)
			continue
		}
		q.kind = queuedPrompt
		q.text = preparePromptText(q.reply, q.text)
		q.reply = nil
		i++
	}
	w.routeDeferredResearchToggle()
}

func (w *worker) nativeCommandDispatchReady(client *omp.Client) bool {
	select {
	case <-client.Done():
		w.failed()
		return false
	default:
	}
	w.resolveSlashInputs()
	return w.queuedCommandDispatchReady(client)
}

// Revalidate without classifying pending inputs or issuing a blocking query.
func (w *worker) queuedCommandDispatchReady(client *omp.Client) bool {
	if client != w.client || len(w.queue) == 0 || w.queue[0].preparing {
		return false
	}
	q := w.queue[0]
	if _, research := autoresearchArguments(q.text, w.b.bot.Username); research {
		if q.kind == queuedSlashPending || q.kind == queuedNativeCommand && client.CommandCatalog().State == omp.CatalogUnknown {
			w.refreshCommandCatalog(client)
			return false
		}
		if q.kind != queuedNativeCommand || q.nativeName != "autoresearch" || !w.researchAvailable(client) {
			if w.cancelQueuedTask(q.id) {
				w.say("Send /autoresearch directly using the current native extension command, not through a followup or attachment. No command was submitted.")
			}
			return false
		}
		args, _ := autoresearchArguments(q.text, w.b.bot.Username)
		if autoresearchClear(args) || args == "off" {
			w.cancelQueuedTask(q.id)
			w.say("Autoresearch controls must be sent directly; clear also requires confirmation. No command was submitted.")
			return false
		}
	}
	if command := unsupportedNativeCommand(q.text, w.b.bot.Username); command != "" {
		if w.cancelQueuedTask(q.id) {
			w.unsupportedNativeCommandNotice(command)
		}
		return false
	}
	// Prompt-only paths, including followups and attachments, can still execute raw slash commands.
	if q.kind == queuedPrompt && !strings.HasPrefix(q.text, "/") {
		return true
	}
	catalog := client.CommandCatalog()
	if catalog.State == omp.CatalogUnknown {
		w.refreshCommandCatalog(client)
		return false
	}
	if q.kind == queuedPrompt && !q.bridgeReview && w.rejectUnsupportedSlash(catalog, q.text, q.id) {
		return false
	}
	name, _, native := advertisedCommand(catalog, q.text, w.b.bot.Username)
	if native && !catalog.HasExecutableCommand(name) {
		if w.cancelQueuedTask(q.id) {
			w.unsupportedCommandSourceNotice(catalog, name)
		}
		return false
	}
	if q.kind == queuedPrompt {
		return true
	}
	if q.kind == queuedNativeCommand && (q.nativeSession != catalog.SessionID || !native || name != q.nativeName) {
		w.cancelQueuedTask(q.id)
		w.say("The queued native command is no longer available. It was canceled and was not sent as a model prompt.")
		return false
	}
	return q.kind == queuedNativeCommand
}

// Local acknowledgment settles submission, not any later background output.
func (w *worker) finishLocalCommandSubmission() {
	w.preview = ""
	w.finalOutputTruncated = false
	w.finish()
}
