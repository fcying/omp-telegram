package bridge

import (
	"encoding/json"
	"strings"
	"time"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

// This is operation ownership, not a copy of the native experiment state. Mode
// is always read from the current native branch before admitting another root.
type autoresearchState struct {
	root           bool
	command        bool
	accepted       bool
	roundOpen      bool
	known          bool
	enabled        bool
	disablePending bool
	notices        int
	settlement     researchSettlement
	admission      *researchAdmission
	control        *researchControl
	modeClient     *omp.Client
	modeEpoch      uint64
	modeRevision   uint64
	modeSession    string
	modeGeneration int64
}

func autoresearchArguments(text, username string) (string, bool) {
	end := strings.IndexByte(text, ' ')
	if end < 0 {
		end = len(text)
	}
	command, foreign := parseSlashCommandToken(text[:end], username)
	return strings.TrimSpace(text[end:]), !foreign && command == "/autoresearch"
}

func autoresearchClear(args string) bool {
	return args == "clear" || strings.HasPrefix(args, "clear ")
}

func (w *worker) researchAvailable(client *omp.Client) bool {
	catalog := client.CommandCatalog()
	return catalog.State != omp.CatalogUnknown && catalog.HasExtensionCommand("autoresearch") && catalog.HasExecutableCommand("autoresearch")
}

func (w *worker) researchModeFailure() {
	w.resumeFailed = true
	w.say("Autoresearch mode could not be confirmed. No waiting input was submitted. Use /autoresearch off to safely disable it, or /close.")
}

// pendingIndex is -1 for new input; the result reports an in-place queue conversion.
func (w *worker) routeResearchInput(in incoming, text string, client *omp.Client, pendingIndex int) bool {
	args, _ := autoresearchArguments(text, w.b.bot.Username)
	if args == "off" {
		if pendingIndex >= 0 {
			w.queue = append(w.queue[:pendingIndex], w.queue[pendingIndex+1:]...)
		}
		if w.mark(in.id, store.InboxSubmitted) {
			w.disableResearch(in.id)
		}
		return false
	}
	queuedControls := 0
	if pendingIndex >= 0 {
		queuedControls = 1
	}
	if autoresearchClear(args) && (w.sessionControlBusy() || w.taskActive() || len(w.queue) > queuedControls || len(w.steers) != 0) {
		if pendingIndex >= 0 {
			w.queue = append(w.queue[:pendingIndex], w.queue[pendingIndex+1:]...)
		}
		w.mark(in.id, store.InboxCancelled)
		w.say("Autoresearch clear requires an idle instance and an empty queue. Stop research first.")
		return false
	}
	if w.research.control != nil {
		if pendingIndex >= 0 {
			w.queue = append(w.queue[:pendingIndex], w.queue[pendingIndex+1:]...)
		}
		w.mark(in.id, store.InboxCancelled)
		w.say("The autoresearch control is still loading. No command was submitted.")
		return false
	}
	// Convert pending discovery in place; the control fences dispatch until lookup finishes.
	if pendingIndex >= 0 {
		q := &w.queue[pendingIndex]
		q.kind, q.nativeName, q.text = queuedNativeCommand, "autoresearch", text
		q.nativeSession = client.CommandCatalog().SessionID
	} else {
		if w.pendingInputFull() {
			w.mark(in.id, store.InboxCancelled)
			w.say("The queue is full. This message was not submitted.")
			return false
		}
		w.enqueueNativeCommand(in, text, "autoresearch", client.CommandCatalog().SessionID)
	}
	// A pending native root owns its local completion control. The queued
	// command will read fresh mode evidence after that root has settled.
	if w.taskActive() && w.activeInputKind == queuedNativeCommand {
		return pendingIndex >= 0
	}
	c := w.newResearchControl(client, "route", in.id)
	c.input, c.text, c.queued = in, text, true
	w.researchControlMode(c, "mode")
	return pendingIndex >= 0
}

// Agent execution resolves the native root's local-completion boundary. Empty
// toggles must now use the control path, not wait behind an autonomous root.
func (w *worker) routeDeferredResearchToggle() {
	if !w.taskActive() || !w.research.root || w.activeInputKind == queuedNativeCommand || w.research.control != nil {
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
	for i := range w.queue {
		q := &w.queue[i]
		if q.kind != queuedNativeCommand || q.nativeName != "autoresearch" {
			continue
		}
		args, command := autoresearchArguments(q.text, w.b.bot.Username)
		if !command || args != "" {
			continue
		}
		if q.nativeSession != catalog.SessionID || !w.researchAvailable(client) {
			w.cancelQueuedTask(q.id)
			w.say("The queued native command is no longer available. It was canceled and was not sent as a model prompt.")
			return
		}
		w.routeResearchInput(incoming{id: q.id, msg: q.reply}, q.text, client, i)
		return
	}
}

// Native mode is independent of command discovery. A confirmed disabled mode
// admits ordinary roots while discovery is pending; enabled mode waits for the
// canonical extension before taking research ownership.
func (w *worker) researchDispatchReady(client *omp.Client) bool {
	if w.research.control != nil {
		return false
	}
	if w.research.disablePending {
		w.resumeFailed = true
		return false
	}
	catalog := client.CommandCatalog()
	if catalog.State == omp.CatalogUnknown {
		w.refreshCommandCatalog(client)
	} else if !w.researchAvailable(client) {
		w.invalidateResearchAdmission()
		w.research.known, w.research.enabled = false, false
		return true
	}
	if !w.researchAdmissionReady(client) {
		return false
	}
	if catalog.State == omp.CatalogUnknown && w.research.enabled {
		return false
	}
	if len(w.queue) != 0 {
		q := w.queue[0]
		if args, command := autoresearchArguments(q.text, w.b.bot.Username); command && args == "" && w.research.enabled {
			w.queue[0] = queued{}
			w.queue = w.queue[1:]
			if w.mark(q.id, store.InboxSubmitted) {
				w.disableResearch(q.id)
			}
			return false
		}
	}
	if len(w.queue) != 0 {
		q := w.queue[0]
		_, command := autoresearchArguments(q.text, w.b.bot.Username)
		if w.research.enabled || command && q.kind == queuedNativeCommand && q.nativeName == "autoresearch" {
			if err := w.ensureResearchWorkspace(); err != nil {
				w.resumeFailed = true
				w.say(err.Error())
				return false
			}
		}
	}
	return true
}

func (w *worker) beginResearchTask(q queued) {
	w.invalidateResearchSettlement()
	_, command := autoresearchArguments(q.text, w.b.bot.Username)
	w.research.command = command && q.kind == queuedNativeCommand && q.nativeName == "autoresearch"
	w.research.accepted = false
	w.research.root = w.research.enabled || w.research.command
	w.research.roundOpen = false
	w.research.notices = 0
}

func (w *worker) flushResearchRound(e rpcEvent) {
	if !w.research.root || !w.taskActive() || !w.research.roundOpen {
		return
	}
	text, found, truncated := boundedAssistantTexts(e.Messages, maxFinalReplyBytes)
	if found {
		w.preview, w.finalOutputTruncated = text, truncated
	} else if len(w.finalAssistantTexts) != 0 {
		w.preview = strings.Join(w.finalAssistantTexts, "\n\n")
	}
	if w.preview != "" {
		if err := w.b.db.AppendInboxReplies(w.ctx, w.active, w.key.chat, w.key.thread, taskReplies(w.preview, "", w.finalOutputTruncated)); err != nil {
			w.b.fail(err)
			w.cancel()
			return
		}
	}
	w.research.roundOpen = false
	w.preview = ""
	w.stream.Reset()
	w.finalAssistantTexts = nil
	w.finalAssistantBytes, w.finalizedStreamBytes = 0, 0
	w.finalOutputTruncated = false
	w.lastAssistant = nil
	w.awaitTaskContinuation()
}

func (w *worker) researchLocalResult(e rpcEvent) bool {
	if !w.research.root || e.ID == "" || e.ID != w.rootRequestID || e.AgentInvoked == nil || *e.AgentInvoked {
		return false
	}
	if e.Status != "completed" || e.SessionSettled == nil || !*e.SessionSettled {
		w.retireResearch("Autoresearch command completion could not be confirmed. It will not be replayed automatically.")
		return true
	}
	if w.research.control == nil {
		c := w.newResearchControl(w.client, "local", w.active)
		c.requestID = w.rootRequestID
		c.completed, c.accepted = true, w.research.accepted
		c.phase, c.deadline = "acceptance", time.Now().Add(15*time.Second)
		if c.accepted {
			w.researchControlMode(c, "postmode")
		}
	}
	return true
}

func (w *worker) researchNotify(e rpcEvent) bool {
	if e.Method != "notify" || !(w.research.root || w.research.command || w.research.disablePending) {
		return false
	}
	if w.research.notices >= 8 {
		return true
	}
	var text string
	if json.Unmarshal(e.Message, &text) != nil {
		return true
	}
	// Notifications are display only: never derive completion or mode from them.
	if trimmed := strings.TrimSpace(text); strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		text = "Native diagnostic notification omitted."
	}
	text = terminalFailureDetail(text)
	if text != "" {
		w.research.notices++
		w.say("Autoresearch: " + text)
	}
	return true
}

func (w *worker) retireResearch(notice string) {
	w.invalidateResearchSettlement()
	w.research.disablePending = true
	w.resumeFailed = true
	w.releaseRuntimeWithReason(true, "failure")
	w.endControlOperation()
	w.completeSessionOperation()
	if w.ctx.Err() != nil || !w.settleOutstandingSteers() {
		return
	}
	if w.taskActive() {
		w.finishUncertain(notice)
	}
	w.clearSteerFence()
}

// routeResearchOff bypasses ordinary admission only for an exact persisted
// lease owner. Unknown identities still use the normal command routing path.
func (w *worker) routeResearchOff(in incoming, text string) bool {
	args, command := autoresearchArguments(text, w.b.bot.Username)
	if !command || args != "off" {
		return false
	}
	owner, err := w.currentWorkspaceOwner()
	if err != nil {
		return false
	}
	owned, err := w.b.db.ResearchWorkspaceOwned(owner)
	if err != nil {
		w.b.fail(err)
		w.cancel()
		return true
	}
	if !owned {
		return false
	}
	if client, connected := w.runtimeClient(); connected && client.CommandCatalog().State == omp.CatalogUnknown {
		w.enqueueSlashInput(in, text, client)
		return true
	}
	if w.mark(in.id, store.InboxSubmitted) {
		w.disableResearch(in.id)
	}
	return true
}

func researchCallbackMessage(q *telegram.CallbackQuery, c confirmation, key target) bool {
	return q.Message != nil && q.Message.MessageID == c.messageID && q.Message.Chat.ID == key.chat && q.Message.MessageThreadID == key.thread
}

func (w *worker) handleResearchStop() bool {
	if w.research.control != nil {
		w.cancelResearchControl()
		w.retireResearch("Autoresearch control was canceled. Its outcome is uncertain; no control will be replayed automatically.")
		return true
	}
	owned, err := w.researchLeaseOwned()
	if err != nil {
		w.researchModeFailure()
		return true
	}
	if owned || w.research.root || w.research.enabled || w.research.disablePending {
		w.disableResearch(0)
		return true
	}
	client, connected := w.runtimeClient()
	if !connected || !w.researchAvailable(client) || w.researchOffProof(client) {
		return false
	}
	c := w.newResearchControl(client, "stop", 0)
	w.researchControlMode(c, "mode")
	return true
}

func (w *worker) researchStatus() string {
	switch {
	case w.research.disablePending:
		return "\nAutoresearch: disable not confirmed; queued work paused."
	case w.research.root:
		return "\nAutoresearch: active research root (managed until off or Stop; a round ending is not research completion)."
	case w.research.known && w.research.enabled:
		return "\nAutoresearch: mode enabled; no active research root."
	case w.research.known:
		return "\nAutoresearch: mode disabled."
	default:
		return ""
	}
}
