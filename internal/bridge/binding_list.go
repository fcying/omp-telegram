package bridge

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

type bindingNamesResult struct {
	request    uint64
	generation int64
	names      map[string]string
}

const (
	bindingPageSize = 10
	queuePageSize   = 10
)

func (w *worker) conversationGeneration() int64 {
	if w.startIntent != nil {
		return w.startIntent.Generation
	}
	return w.binding.Generation
}

func (w *worker) touchBinding() {
	if w.migrationLocked() || w.b == nil || w.b.db == nil || w.binding.Generation == 0 {
		return
	}
	usedAt := time.Now().Unix()
	updated, err := w.b.db.TouchBinding(w.b.bot.ID, w.key.chat, w.key.thread, w.binding.Generation, usedAt)
	if err != nil {
		w.b.storeLog.Error("binding last-used timestamp failed", "event", "binding_write_failed", "reason", "touch_last_used", "error_kind", "persistence")
		return
	}
	if updated {
		w.binding.LastUsedAt = usedAt
	}
}

func (w *worker) invalidateBindingMenus() {
	for token, c := range w.confirms {
		if c.action == "bindings" || c.action == "binding_delete" {
			w.dropConfirmation(token, c)
		}
	}
}

func (w *worker) showBindings(user int64, oldestFirst bool, page int) {
	w.invalidateBindingMenus()
	epoch := w.b.bindingsEpoch.Load()
	entries, err := w.b.db.BindingsForChat(w.b.bot.ID, w.key.chat)
	if err != nil {
		w.b.storeLog.Error("binding list read failed", "event", "binding_read_failed", "reason", "bindings", "error_kind", "persistence")
		w.say(w.tr(i18n.Msg.FailedReadSavedBindings))
		return
	}
	if len(entries) == 0 {
		w.say(w.tr(i18n.Msg.NoSavedConversationBindings))
		return
	}
	w.sortBindings(entries, oldestFirst)
	generation := w.conversationGeneration()
	page = max(0, min(page, (len(entries)-1)/bindingPageSize))
	w.showBindingsPage(confirmation{action: "bindings", method: "list", user: user, bindings: entries, generation: generation, epoch: epoch, bindingsOld: oldestFirst}, page, 0)
	w.lookupBindingNames(entries, generation)
}

func (w *worker) sortBindings(entries []store.BindingListEntry, oldestFirst bool) {
	now := time.Now().Unix()
	slices.SortFunc(entries, func(left, right store.BindingListEntry) int {
		leftRank, rightRank := 2, 2
		if oldestFirst {
			if left.Intent != nil {
				leftRank = 3
			} else if left.Binding != nil && !left.Binding.Running {
				leftRank = 1
			}
			if right.Intent != nil {
				rightRank = 3
			} else if right.Binding != nil && !right.Binding.Running {
				rightRank = 1
			}
		} else {
			if left.Intent != nil {
				leftRank = 1
			}
			if right.Intent != nil {
				rightRank = 1
			}
			if bindingThread(left) == w.key.thread {
				leftRank = 0
			}
			if bindingThread(right) == w.key.thread {
				rightRank = 0
			}
		}
		if leftRank != rightRank {
			return cmp.Compare(leftRank, rightRank)
		}
		if leftRank == 2 || (oldestFirst && leftRank == 1) {
			leftUsed, rightUsed := bindingLastUsedAt(left), bindingLastUsedAt(right)
			if leftUsed <= 0 || leftUsed > now {
				leftUsed = 0
			}
			if rightUsed <= 0 || rightUsed > now {
				rightUsed = 0
			}
			if leftUsed != rightUsed {
				if oldestFirst && leftUsed != 0 && rightUsed != 0 {
					return cmp.Compare(leftUsed, rightUsed)
				}
				return cmp.Compare(rightUsed, leftUsed)
			}
		}
		return cmp.Compare(bindingThread(left), bindingThread(right))
	})
}

func bindingSessionID(entry store.BindingListEntry) string {
	if entry.Intent != nil {
		if entry.Intent.Session != "" {
			return entry.Intent.Session
		}
		if entry.Intent.Kind == "new" {
			return ""
		}
	}
	if entry.Binding != nil {
		return entry.Binding.SessionID
	}
	return ""
}

func (w *worker) cancelBindingNameLookup() {
	if w.bindingNameCancel != nil {
		w.bindingNameCancel()
		w.bindingNameCancel = nil
	}
	w.bindingNameRequest++
}

func (w *worker) lookupBindingNames(entries []store.BindingListEntry, generation int64) {
	w.cancelBindingNameLookup()
	if w.bindingNameResults == nil {
		w.bindingNameResults = make(chan bindingNamesResult, 1)
	}
	workspaces := make(map[string]struct{})
	for _, entry := range entries {
		if bindingSessionID(entry) == "" {
			continue
		}
		workspace := bindingWorkspace(entry)
		if filepath.IsAbs(workspace) {
			workspaces[workspace] = struct{}{}
		}
	}
	if len(workspaces) == 0 {
		return
	}
	if w.b.resumeSlots == nil {
		w.b.resumeSlots = make(chan struct{}, 2)
	}
	request := w.bindingNameRequest
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	w.bindingNameCancel = cancel
	slots := w.b.resumeSlots
	binary, args, environment := w.b.cfg.OMP, w.b.selectedArgsSnapshot(), w.b.cfg.OMPEnvironment
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer cancel()
		names := make(map[string]string)
		for workspace := range workspaces {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			sessions, err := omp.ListSessions(ctx, omp.Config{Binary: binary, CWD: workspace, Args: args, Environment: environment})
			<-slots
			if err != nil {
				continue
			}
			for _, session := range sessions {
				if title := menuText(session.Title, 160); title != "" {
					names[session.ID] = title
				}
			}
		}
		result := bindingNamesResult{request: request, generation: generation, names: names}
		select {
		case w.bindingNameResults <- result:
		case <-w.ctx.Done():
		}
	}()
}

func (w *worker) bindingNamesLoaded(result bindingNamesResult) {
	if result.request != w.bindingNameRequest || result.generation != w.conversationGeneration() {
		return
	}
	w.bindingNameCancel = nil
	if len(result.names) == 0 {
		return
	}
	for token, c := range w.confirms {
		if c.action != "bindings" || c.generation != result.generation {
			continue
		}
		updated := append([]store.BindingListEntry(nil), c.bindings...)
		changed := false
		for i := range updated {
			if name := result.names[bindingSessionID(updated[i])]; name != "" && updated[i].SessionName != name {
				updated[i].SessionName = name
				changed = true
			}
		}
		if !changed {
			return
		}
		c.bindings = updated
		delete(w.confirms, token)
		w.showBindingsPage(c, c.page, c.messageID)
		return
	}
}

func bindingSessionName(language i18n.Language, entry store.BindingListEntry, limit int) string {
	if name := menuText(entry.SessionName, limit); name != "" {
		return name
	}
	if bindingWorkspace(entry) == "" {
		return i18n.Text(language, i18n.Msg.Unknown)
	}
	return menuText(filepath.Base(bindingWorkspace(entry)), limit)
}

func (w *worker) showBindingsPage(c confirmation, page int, messageID int64) {
	pages := (len(c.bindings) + bindingPageSize - 1) / bindingPageSize
	if page < 0 || page >= pages {
		return
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		w.say(w.tr(i18n.Msg.CannotCreateBindingListMenu))
		return
	}
	token := hex.EncodeToString(random[:])
	c.action, c.method, c.page = "bindings", "list", page
	c.expires = time.Now().Add(2 * time.Minute)
	c.options = nil
	start, end := page*bindingPageSize, min((page+1)*bindingPageSize, len(c.bindings))
	var text strings.Builder
	now := time.Now().Unix()
	home, _ := os.UserHomeDir()
	if c.bindingsOld {
		text.WriteString(w.tr(i18n.Msg.SavedBindingsOldestFirstPage, page+1, pages))
	} else {
		text.WriteString(w.tr(i18n.Msg.SavedBindingsPage, page+1, pages))
	}
	keyboard := &telegram.Keyboard{}
	button := func(label, action string) telegram.Button {
		index := len(c.options)
		c.options = append(c.options, action)
		return telegram.Button{Text: label, CallbackData: fmt.Sprintf("%s:%d", token, index)}
	}
	for i, entry := range c.bindings[start:end] {
		index := start + i
		current := entry.Binding != nil && entry.Binding.Chat == w.key.chat && entry.Binding.Thread == w.key.thread
		if entry.Intent != nil && entry.Binding == nil {
			current = entry.Intent.Chat == w.key.chat && entry.Intent.Thread == w.key.thread
		}
		marker := ""
		if current {
			marker = "* "
		}
		topic := w.tr(i18n.Msg.MainChat)
		if thread := bindingThread(entry); thread != 0 {
			topic = fmt.Sprintf("#%d", thread)
		}
		age := formatBindingAge(bindingLastUsedAt(entry), now)
		var status string
		if entry.Intent != nil {
			status = "⏳"
		} else if entry.Binding != nil && entry.Binding.Running {
			status = "📖"
		} else {
			status = "📕"
		}
		workspace := w.tr(i18n.Msg.Unknown)
		if path := bindingWorkspace(entry); path != "" {
			workspace = statusWorkspace(path, home)
		}
		label := menuText(fmt.Sprintf("%d. %s%s · %s · %s · %s · %s", index+1, marker, bindingSessionName(w.b.language(), entry, 32), topic, status, age, workspace), 256)
		entryButton := telegram.Button{Text: label}
		if bindingCanDelete(entry, current) {
			optionIndex := len(c.options)
			c.options = append(c.options, "delete:"+strconv.Itoa(index))
			entryButton.CallbackData = fmt.Sprintf("%s:%d", token, optionIndex)
		} else {
			entryButton.Disabled = &telegram.DisabledButton{}
		}
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegram.Button{entryButton})
	}
	navigation := make([]telegram.Button, 0, 3)
	if page > 0 {
		navigation = append(navigation, button(w.tr(i18n.Msg.Previous), "previous"))
	}
	if page+1 < pages {
		navigation = append(navigation, button(w.tr(i18n.Msg.Next), "next"))
	}
	navigation = append(navigation, button(w.tr(i18n.Msg.Close), "close"))
	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, navigation)
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	var err error
	if messageID != 0 {
		err = w.b.tg.Edit(ctx, w.key.chat, messageID, text.String(), keyboard)
	} else {
		var message telegram.Message
		message, err = w.b.tg.Send(ctx, w.key.chat, w.key.thread, text.String(), telegram.SendOptions{Keyboard: keyboard})
		messageID = message.MessageID
	}
	if err != nil {
		w.say(w.tr(i18n.Msg.FailedDisplaySavedBindingsUseBindingsTryAgain))
		return
	}
	c.messageID = messageID
	if w.confirms == nil {
		w.confirms = make(map[string]confirmation)
	}
	w.confirms[token] = c
}

func (w *worker) showQueue(user int64) {
	w.showQueuePage(confirmation{action: "queue", user: user}, 0, 0)
}

func queueTaskText(language i18n.Language, q queued) string {
	if q.preparing {
		if q.album {
			if text := menuText(q.displayText, 80); text != "" {
				return text
			}
			return i18n.Text(language, i18n.Msg.QueuedAlbum)
		}
		return i18n.Text(language, i18n.Msg.PreparingAttachment)
	}
	if text := menuText(q.displayText, 80); text != "" {
		return text
	}
	if q.album {
		return i18n.Text(language, i18n.Msg.QueuedAlbum)
	}
	if q.directory != "" {
		return i18n.Text(language, i18n.Msg.QueuedAttachment)
	}
	if text := menuText(q.text, 80); text != "" {
		return text
	}
	return i18n.Text(language, i18n.Msg.QueuedTask)
}

func (w *worker) showQueuePage(c confirmation, page int, messageID int64) {
	pages := (len(w.queue) + queuePageSize - 1) / queuePageSize
	if pages == 0 {
		pages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	var token string
	if len(w.queue) != 0 {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			w.say(w.tr(i18n.Msg.CannotCreateQueueMenu))
			return
		}
		token = hex.EncodeToString(random[:])
		c.action = "queue"
		c.page = page
		c.expires = time.Now().Add(2 * time.Minute)
		c.generation = w.conversationGeneration()
		c.options = nil
	}
	start, end := page*queuePageSize, min((page+1)*queuePageSize, len(w.queue))
	running := w.tr(i18n.Msg.No)
	if w.taskRunning() {
		running = w.tr(i18n.Msg.Yes)
	}
	var text strings.Builder
	text.WriteString(w.tr(i18n.Msg.QueueRunningPending, running, len(w.queue)))
	var keyboard *telegram.Keyboard
	if len(w.queue) != 0 {
		text.WriteString(w.tr(i18n.Msg.Page, page+1, pages))
		keyboard = &telegram.Keyboard{}
		for i, q := range w.queue[start:end] {
			if i == 0 {
				text.WriteString("\n")
			}
			label := menuText(fmt.Sprintf("%d. %s", start+i+1, queueTaskText(w.b.language(), q)), 128)
			keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegram.Button{
				{Text: label, Disabled: &telegram.DisabledButton{}},
				{Text: w.tr(i18n.Msg.Cancel), CallbackData: fmt.Sprintf("%s:cancel:%d", token, q.id), Style: "danger"},
			})
		}
		navigation := []telegram.Button{}
		if page > 0 {
			navigation = append(navigation, telegram.Button{Text: w.tr(i18n.Msg.Previous), CallbackData: token + ":previous"})
		}
		if page+1 < pages {
			navigation = append(navigation, telegram.Button{Text: w.tr(i18n.Msg.Next), CallbackData: token + ":next"})
		}
		navigation = append(navigation, telegram.Button{Text: w.tr(i18n.Msg.Close), CallbackData: token + ":close"})
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, navigation)
	}
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	var err error
	if messageID != 0 {
		if keyboard == nil {
			keyboard = &telegram.Keyboard{InlineKeyboard: [][]telegram.Button{}}
		}
		err = w.b.tg.Edit(ctx, w.key.chat, messageID, text.String(), keyboard)
	} else {
		var message telegram.Message
		message, err = w.b.tg.Send(ctx, w.key.chat, w.key.thread, text.String(), telegram.SendOptions{Keyboard: keyboard})
		messageID = message.MessageID
	}
	if err != nil {
		w.say(w.tr(i18n.Msg.FailedDisplayQueueUseQueueTryAgain))
		return
	}
	if len(w.queue) == 0 {
		return
	}
	c.messageID = messageID
	if w.confirms == nil {
		w.confirms = make(map[string]confirmation)
	}
	w.confirms[token] = c
}

func bindingThread(entry store.BindingListEntry) int64 {
	if entry.Binding != nil {
		return entry.Binding.Thread
	}
	if entry.Intent != nil {
		return entry.Intent.Thread
	}
	return 0
}

func bindingConversationTitle(language i18n.Language, entry store.BindingListEntry) string {
	if thread := bindingThread(entry); thread != 0 {
		return i18n.Text(language, i18n.Msg.Topic, thread)
	}
	return i18n.Text(language, i18n.Msg.MainChat)
}

func bindingEntryStatus(language i18n.Language, entry store.BindingListEntry) string {
	if entry.Binding != nil && entry.Binding.Running {
		return i18n.Text(language, i18n.Msg.Open)
	}
	return i18n.Text(language, i18n.Msg.Closed)
}

func bindingLastUsedAt(entry store.BindingListEntry) int64 {
	if entry.Binding == nil {
		return 0
	}
	return entry.Binding.LastUsedAt
}

func bindingWorkspace(entry store.BindingListEntry) string {
	if entry.Intent != nil && entry.Intent.Workspace != "" {
		return entry.Intent.Workspace
	}
	if entry.Binding != nil && entry.Binding.Workspace != "" {
		return entry.Binding.Workspace
	}
	return ""
}

func formatBindingAge(timestamp, now int64) string {
	if timestamp <= 0 || timestamp > now {
		return "-"
	}
	const (
		minute = int64(60)
		hour   = 60 * minute
		day    = 24 * hour
		week   = 7 * day
		month  = 30 * day
		year   = 365 * day
	)
	age := now - timestamp
	switch {
	case age < minute:
		return "now"
	case age < hour:
		return strconv.FormatInt(age/minute, 10) + "m"
	case age < day:
		return strconv.FormatInt(age/hour, 10) + "h"
	case age < week:
		return strconv.FormatInt(age/day, 10) + "d"
	case age < month:
		return strconv.FormatInt(age/week, 10) + "w"
	case age < year:
		return strconv.FormatInt(age/month, 10) + "mo"
	default:
		return strconv.FormatInt(age/year, 10) + "y"
	}
}

func bindingCanDelete(entry store.BindingListEntry, current bool) bool {
	return entry.Intent == nil && entry.Binding != nil && !current
}

func (request bindingForgetRequest) reply(result bindingForgetResult) {
	select {
	case request.source.forgetResults <- result:
	case <-request.source.ctx.Done():
	}
}

func (w *worker) canForgetBinding() bool {
	return !w.migrationLocked() && (w.runtime == runtimeReleased && w.client == nil || w.canReleaseRuntime()) &&
		!w.taskActive() && !w.sessionControlBusy() && !w.restoring && !w.runtimeResuming &&
		!w.awaitingContinuation && !w.previewBusy && w.exportingSession == "" &&
		len(w.input) == 0 && len(w.queue) == 0 && len(w.albums) == 0 &&
		len(w.hostRequests) == 0 && len(w.confirms) == 0 && len(w.operations) == 0 &&
		len(w.rpcOperations) == 0 && !w.rpcOperationActive &&
		len(w.previewResult) == 0 && len(w.mediaResults) == 0 && len(w.sendResults) == 0 &&
		w.resumeCancel == nil && w.exportCancel == nil && w.deleteCancel == nil && w.bindingNameCancel == nil && w.doctorCancel == nil
}

func (w *worker) forgetBinding(request bindingForgetRequest) {
	if w.migrationLocked() {
		request.reply(bindingForgetResult{status: i18n.Msg.MigrationUnresolvedCannotDeleteBinding})
		return
	}
	if w.ctx.Err() != nil {
		request.reply(bindingForgetResult{status: i18n.Msg.BindingChangedUseBindingsRefresh})
		return
	}
	if w.binding.Generation != request.generation || !w.binding.Running || w.startIntent != nil {
		request.reply(bindingForgetResult{status: i18n.Msg.BindingChangedUseBindingsRefresh})
		return
	}
	if !w.canForgetBinding() {
		request.reply(bindingForgetResult{status: i18n.Msg.BindingBusyTryAgainWhenTaskQueueIdle})
		return
	}
	w.exitMu.Lock()
	if w.exitRequested || len(w.input) != 0 {
		w.exitMu.Unlock()
		request.reply(bindingForgetResult{status: i18n.Msg.BindingBusyTryAgainLater})
		return
	}
	w.exitRequested = true
	w.exitMu.Unlock()
	defer func() {
		if w.ctx.Err() == nil {
			w.exitMu.Lock()
			w.exitRequested = false
			w.exitMu.Unlock()
		}
	}()
	if !w.closeLogicalSession() {
		request.reply(bindingForgetResult{status: i18n.Msg.FailedCloseSavedBinding})
		return
	}
	if request.session {
		binding := w.binding
		request.reply(bindingForgetResult{ok: true, binding: &binding})
		w.cancel()
		return
	}
	ok, err := w.b.db.DeleteClosedBinding(w.b.bot.ID, w.key.chat, w.key.thread, request.generation)
	if err != nil {
		w.b.storeLog.Error("binding deletion failed", "event", "binding_delete_failed", "reason", "transaction", "error_kind", "persistence")
		request.reply(bindingForgetResult{status: i18n.Msg.FailedDeleteSavedBinding})
		return
	}
	if !ok {
		request.reply(bindingForgetResult{status: i18n.Msg.BindingChangedUseBindingsRefresh})
		return
	}
	w.b.bindingsEpoch.Add(1)
	request.reply(bindingForgetResult{ok: true})
	w.cancel()
}

func (w *worker) forgetFinished(result bindingForgetResult) {
	w.forgetPending = false
	c := w.forgetConfirmation
	w.forgetConfirmation = nil
	if c == nil || c.generation != w.conversationGeneration() || w.ctx.Err() != nil {
		return
	}
	if result.ok {
		if result.binding != nil {
			w.beginBindingSessionDelete(*c, *result.binding)
			return
		}
		w.say(w.tr(i18n.Msg.SavedBindingDeletedWorkspaceNativeOMPSessionHistoryPreserved))
		w.showBindings(c.user, c.bindingsOld, c.page)
		return
	}
	w.say(w.tr(result.status))
}

func (w *worker) bindingCallback(ctx context.Context, q *telegram.CallbackQuery, token, index string, c confirmation) callbackResult {
	if !c.expires.IsZero() && time.Now().After(c.expires) {
		delete(w.confirms, token)
		if q.Message != nil && q.Message.MessageID == c.messageID {
			w.clearKeyboard(c.messageID)
		}
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
		return callbackDone
	}
	if c.epoch != w.b.bindingsEpoch.Load() {
		delete(w.confirms, token)
		if q.Message != nil && q.Message.MessageID == c.messageID {
			w.clearKeyboard(c.messageID)
		}
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
		return callbackDone
	}
	if q.Message == nil || q.Message.MessageID != c.messageID || c.generation != w.conversationGeneration() {
		delete(w.confirms, token)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
		return callbackDone
	}
	n, err := strconv.Atoi(index)
	if err != nil || n < 0 || n >= len(c.options) {
		delete(w.confirms, token)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
		return callbackDone
	}
	if c.action == "binding_delete" {
		delete(w.confirms, token)
		if n == len(c.options)-1 {
			w.finishConfirmation(c, n)
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Canceled))
			return callbackDone
		}
		w.finishMenu(c.messageID, w.tr(i18n.Msg.Selected, c.options[n])+"\n"+c.uiTitle)
		if w.b.migrationBindingLocked(c.deleteBot, c.deleteChat, c.deleteThread) {
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.MigrationUnresolved))
			w.say(w.tr(i18n.Msg.MigrationUnresolvedCannotDeleteOtherBinding))
			return callbackDone
		}
		if c.deleteRunning {
			if w.forgetPending || w.deleteCancel != nil || w.b.forgetRequests == nil {
				_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.DeleteUnavailable))
				w.say(w.tr(i18n.Msg.AnotherBindingDeletionPendingTryAgainLater))
				return callbackDone
			}
			w.forgetPending = true
			w.forgetConfirmation = &c
			select {
			case w.b.forgetRequests <- bindingForgetRequest{key: target{chat: c.deleteChat, thread: c.deleteThread}, generation: c.deleteGeneration, session: n == 1, source: w}:
				_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Deleting))
			default:
				w.forgetPending = false
				w.forgetConfirmation = nil
				_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.DeleteUnavailable))
				w.say(w.tr(i18n.Msg.BindingDeletionUnavailableTryAgainLater))
			}
			return callbackDone
		}
		if n == 1 {
			binding, err := w.b.db.Binding(c.deleteBot, c.deleteChat, c.deleteThread)
			if err != nil || binding.Generation != c.deleteGeneration || binding.Running {
				_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.BindingChanged))
				w.say(w.tr(i18n.Msg.SavedBindingChangedUseBindingsRefresh))
				return callbackDone
			}
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Deleting))
			w.beginBindingSessionDelete(c, binding)
			return callbackDone
		}
		ok, err := w.b.db.DeleteClosedBinding(c.deleteBot, c.deleteChat, c.deleteThread, c.deleteGeneration)
		if err != nil {
			w.b.storeLog.Error("binding deletion failed", "event", "binding_delete_failed", "reason", "transaction", "error_kind", "persistence")
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.DeleteFailed))
			w.say(w.tr(i18n.Msg.FailedDeleteSavedBinding))
			return callbackDone
		}
		if !ok {
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.BindingChanged))
			w.say(w.tr(i18n.Msg.SavedBindingChangedUseBindingsRefresh))
			return callbackDone
		}
		w.b.bindingsEpoch.Add(1)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Deleted))
		if c.deleteChat == w.key.chat && c.deleteThread == w.key.thread {
			w.binding = store.Binding{Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread}
			w.sessionID = ""
			w.claimedSession = ""
		}
		w.say(w.tr(i18n.Msg.SavedBindingDeletedWorkspaceNativeOMPSessionHistoryPreserved))
		w.showBindings(c.user, c.bindingsOld, c.page)
		return callbackDone
	}
	action := c.options[n]
	if action == "close" {
		if err := w.b.tg.ClearKeyboard(ctx, w.key.chat, c.messageID); err != nil {
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.CouldNotCloseBindingsMenuTapCloseAgain))
			return callbackDone
		}
		delete(w.confirms, token)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Closed))
		return callbackDone
	}
	delete(w.confirms, token)
	_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Received))
	switch {
	case action == "previous":
		w.showBindingsPage(c, c.page-1, c.messageID)
	case action == "next":
		w.showBindingsPage(c, c.page+1, c.messageID)
	case strings.HasPrefix(action, "delete:"):
		entryIndex, err := strconv.Atoi(strings.TrimPrefix(action, "delete:"))
		if err != nil || entryIndex < 0 || entryIndex >= len(c.bindings) {
			w.clearKeyboard(c.messageID)
			return callbackDone
		}
		entry := c.bindings[entryIndex]
		current := (entry.Binding != nil && entry.Binding.Chat == w.key.chat && entry.Binding.Thread == w.key.thread) || (entry.Binding == nil && entry.Intent != nil && entry.Intent.Chat == w.key.chat && entry.Intent.Thread == w.key.thread)
		if !bindingCanDelete(entry, current) {
			w.clearKeyboard(c.messageID)
			w.say(w.tr(i18n.Msg.OnlyBindingAnotherConversationWithoutPendingStartCanDeleted))
			return callbackDone
		}
		if w.b.migrationBindingLocked(entry.Binding.Bot, entry.Binding.Chat, entry.Binding.Thread) {
			w.clearKeyboard(c.messageID)
			w.say(w.tr(i18n.Msg.MigrationUnresolvedCannotDeleteOtherBinding))
			return callbackDone
		}
		w.clearKeyboard(c.messageID)
		prompt := w.tr(i18n.Msg.ChooseBindingDeletion, bindingConversationTitle(w.b.language(), entry))
		if entry.Binding.Running {
			prompt = w.tr(i18n.Msg.CloseChooseBindingDeletion, bindingConversationTitle(w.b.language(), entry))
		}
		prompt += fmt.Sprintf("\n\n%s · %s\n%s: %s", bindingSessionName(w.b.language(), entry, 0), bindingEntryStatus(w.b.language(), entry), w.tr(i18n.Msg.Workspace), bindingWorkspace(entry))
		if id := bindingSessionID(entry); id != "" {
			prompt += "\n" + w.tr(i18n.Msg.ID, id)
		}
		w.confirm(confirmation{action: "binding_delete", method: "confirm", user: c.user, epoch: c.epoch, bindingsOld: c.bindingsOld, page: c.page, ompArgs: w.b.selectedArgsSnapshot(), options: []string{"Binding", "Binding+Session", "Cancel"}, deleteBot: entry.Binding.Bot, deleteChat: entry.Binding.Chat, deleteThread: entry.Binding.Thread, deleteGeneration: entry.Binding.Generation, deleteRunning: entry.Binding.Running}, prompt, []string{w.tr(i18n.Msg.BindingOnly), w.tr(i18n.Msg.BindingAndSession), w.tr(i18n.Msg.Cancel)})
	}
	return callbackDone
}
