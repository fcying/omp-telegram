package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

type bindingNamesResult struct {
	request    uint64
	generation int64
	names      map[string]string
}

const bindingPageSize = 6

func (w *worker) conversationGeneration() int64 {
	if w.startIntent != nil {
		return w.startIntent.Generation
	}
	return w.binding.Generation
}

func (w *worker) touchBinding() {
	if w.b == nil || w.b.db == nil || w.binding.Generation == 0 {
		return
	}
	updated, err := w.b.db.TouchBinding(w.b.bot.ID, w.key.chat, w.key.thread, w.binding.Generation)
	if err != nil {
		w.b.storeLog.Error("binding last-used timestamp failed", "event", "binding_write_failed", "reason", "touch_last_used", "error_kind", "persistence")
		return
	}
	if updated {
		w.binding.LastUsedAt = time.Now().Unix()
	}
}

func (w *worker) invalidateBindingMenus() {
	for token, c := range w.confirms {
		if c.action == "bindings" || c.action == "binding_delete" {
			w.dropConfirmation(token, c)
		}
	}
}

func (w *worker) showBindings(user int64) {
	w.invalidateBindingMenus()
	epoch := w.b.bindingsEpoch.Load()
	entries, err := w.b.db.BindingsForChat(w.b.bot.ID, w.key.chat)
	if err != nil {
		w.b.storeLog.Error("binding list read failed", "event", "binding_read_failed", "reason", "bindings", "error_kind", "persistence")
		w.say("Failed to read saved bindings.")
		return
	}
	if len(entries) == 0 {
		w.say("No saved conversation bindings.")
		return
	}
	generation := w.conversationGeneration()
	w.showBindingsPage(confirmation{action: "bindings", method: "list", user: user, bindings: entries, generation: generation, epoch: epoch}, 0, 0)
	w.lookupBindingNames(entries, generation)
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
			sessions, err := omp.ListSessions(ctx, omp.Config{Binary: w.b.cfg.OMP, CWD: workspace, Args: w.b.cfg.OMPArgs})
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

func bindingSessionName(entry store.BindingListEntry) string {
	if name := menuText(entry.SessionName, 160); name != "" {
		return name
	}
	if entry.Intent != nil && entry.Intent.Kind == "new" {
		return "pending"
	}
	return "unknown"
}
func (w *worker) showBindingsPage(c confirmation, page int, messageID int64) {
	pages := (len(c.bindings) + bindingPageSize - 1) / bindingPageSize
	if page < 0 || page >= pages {
		return
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		w.say("Cannot create the binding list menu.")
		return
	}
	token := hex.EncodeToString(random[:])
	c.action, c.method, c.page = "bindings", "list", page
	c.expires = time.Now().Add(2 * time.Minute)
	c.options = nil
	start, end := page*bindingPageSize, min((page+1)*bindingPageSize, len(c.bindings))
	var text strings.Builder
	home, _ := os.UserHomeDir()
	fmt.Fprintf(&text, "Saved bindings\nPage %d/%d", page+1, pages)
	keyboard := &telegram.Keyboard{}
	addInfo := func(label string) {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegram.Button{{Text: menuText(label, 512), Disabled: &telegram.DisabledButton{}}})
	}
	add := func(label, action string, style string) {
		index := len(c.options)
		c.options = append(c.options, action)
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegram.Button{{Text: label, CallbackData: fmt.Sprintf("%s:%d", token, index), Style: style}})
	}
	for i, entry := range c.bindings[start:end] {
		index := start + i
		current := entry.Binding != nil && entry.Binding.Chat == w.key.chat && entry.Binding.Thread == w.key.thread
		if entry.Intent != nil && entry.Binding == nil {
			current = entry.Intent.Chat == w.key.chat && entry.Intent.Thread == w.key.thread
		}
		marker := ""
		if current {
			marker = " [current]"
		}
		title := menuText(fmt.Sprintf("%d. %s%s", index+1, bindingConversationTitle(entry), marker), 128)
		row := []telegram.Button{{Text: title, Disabled: &telegram.DisabledButton{}}}
		if bindingCanDelete(entry, current) {
			optionIndex := len(c.options)
			c.options = append(c.options, "delete:"+strconv.Itoa(index))
			row = append(row, telegram.Button{Text: "Del", CallbackData: fmt.Sprintf("%s:%d", token, optionIndex), Style: "danger"})
		} else {
			row = append(row, telegram.Button{Text: "Del", Disabled: &telegram.DisabledButton{}})
		}
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, row)
		addInfo(fmt.Sprintf("%s · Last used: %s", bindingEntryStatus(entry), formatLastUsed(bindingLastUsedAt(entry))))
		addInfo(statusWorkspace(bindingWorkspace(entry), home))
		addInfo(fmt.Sprintf("Name: %s · %s", bindingSessionName(entry), bindingSession(entry)))
	}
	if page > 0 {
		add("Previous", "previous", "")
	}
	if page+1 < pages {
		add("Next", "next", "")
	}
	add("Close", "close", "")
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
		w.say("Failed to display saved bindings. Use /bindings to try again.")
		return
	}
	c.messageID = messageID
	if w.confirms == nil {
		w.confirms = make(map[string]confirmation)
	}
	w.confirms[token] = c
}

func bindingConversationTitle(entry store.BindingListEntry) string {
	thread := int64(0)
	if entry.Binding != nil {
		thread = entry.Binding.Thread
	} else if entry.Intent != nil {
		thread = entry.Intent.Thread
	}
	if thread == 0 {
		return "Main chat"
	}
	return fmt.Sprintf("Topic %d", thread)
}

func bindingEntryStatus(entry store.BindingListEntry) string {
	if entry.Intent != nil {
		return "Pending " + entry.Intent.Kind
	}
	if entry.Binding != nil && entry.Binding.Running {
		return "Open"
	}
	return "Closed"
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
	return "unknown"
}

func bindingSession(entry store.BindingListEntry) string {
	if entry.Intent != nil {
		if entry.Intent.Kind == "new" {
			return "pending"
		}
		if entry.Intent.Session != "" {
			return shortBindingSession(entry.Intent.Session)
		}
	}
	if entry.Binding != nil && entry.Binding.SessionID != "" {
		return shortBindingSession(entry.Binding.SessionID)
	}
	return "unknown"
}

func shortBindingSession(id string) string {
	if len(id) <= 8 {
		return id
	}
	return "..." + id[len(id)-8:]
}

func formatLastUsed(timestamp int64) string {
	if timestamp <= 0 {
		return "unknown"
	}
	age := time.Since(time.Unix(timestamp, 0))
	if age < 0 {
		return "unknown"
	}
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age/time.Minute))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age/time.Hour))
	default:
		return fmt.Sprintf("%dd %dh ago", int(age/(24*time.Hour)), int(age/time.Hour)%24)
	}
}

func bindingCanDelete(entry store.BindingListEntry, current bool) bool {
	return entry.Intent == nil && entry.Binding != nil && !entry.Binding.Running && !current
}

func (w *worker) bindingCallback(ctx context.Context, q *telegram.CallbackQuery, token, index string, c confirmation) callbackResult {
	if !c.expires.IsZero() && time.Now().After(c.expires) {
		delete(w.confirms, token)
		if q.Message != nil && q.Message.MessageID == c.messageID {
			w.clearKeyboard(c.messageID)
		}
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
		return callbackDone
	}
	if c.epoch != w.b.bindingsEpoch.Load() {
		delete(w.confirms, token)
		if q.Message != nil && q.Message.MessageID == c.messageID {
			w.clearKeyboard(c.messageID)
		}
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
		return callbackDone
	}
	if q.Message == nil || q.Message.MessageID != c.messageID || c.generation != w.conversationGeneration() {
		delete(w.confirms, token)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
		return callbackDone
	}
	n, err := strconv.Atoi(index)
	if err != nil || n < 0 || n >= len(c.options) {
		delete(w.confirms, token)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
		return callbackDone
	}
	if c.action == "binding_delete" {
		delete(w.confirms, token)
		w.clearKeyboard(c.messageID)
		if n != 0 {
			_ = w.b.tg.AnswerCallback(ctx, q.ID, "Canceled")
			return callbackDone
		}
		ok, err := w.b.db.DeleteClosedBinding(c.deleteBot, c.deleteChat, c.deleteThread, c.deleteGeneration)
		if err != nil {
			w.b.storeLog.Error("binding deletion failed", "event", "binding_delete_failed", "reason", "transaction", "error_kind", "persistence")
			_ = w.b.tg.AnswerCallback(ctx, q.ID, "Delete failed")
			w.say("Failed to delete the saved binding.")
			return callbackDone
		}
		if !ok {
			_ = w.b.tg.AnswerCallback(ctx, q.ID, "Binding changed")
			w.say("The saved binding changed. Use /bindings to refresh.")
			return callbackDone
		}
		w.b.bindingsEpoch.Add(1)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "Deleted")
		if c.deleteChat == w.key.chat && c.deleteThread == w.key.thread {
			w.binding = store.Binding{Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread}
			w.sessionID = ""
			w.claimedSession = ""
		}
		w.say("Saved binding deleted. Workspace and native OMP session history were preserved.")
		return callbackDone
	}
	action := c.options[n]
	delete(w.confirms, token)
	_ = w.b.tg.AnswerCallback(ctx, q.ID, "Received")
	switch {
	case action == "close":
		w.clearKeyboard(c.messageID)
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
			w.say("Only a closed binding in another conversation can be deleted.")
			return callbackDone
		}
		w.clearKeyboard(c.messageID)
		w.confirm(confirmation{action: "binding_delete", method: "confirm", user: c.user, epoch: c.epoch, options: []string{"Delete", "Cancel"}, deleteBot: entry.Binding.Bot, deleteChat: entry.Binding.Chat, deleteThread: entry.Binding.Thread, deleteGeneration: entry.Binding.Generation}, fmt.Sprintf("Forget saved binding for %s?\nWorkspace and native OMP session history will NOT be deleted.", bindingConversationTitle(entry)), []string{"Delete", "Cancel"})
	}
	return callbackDone
}
