package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/telegram"
)

const resumePageSize = 8

type resumeListResult struct {
	request          uint64
	generation, user int64
	cwd              string
	sessions         []omp.SessionSummary
	err              error
}

func sameWorkspace(left, right string) bool {
	a, err := filepath.EvalSymlinks(left)
	if err != nil {
		return false
	}
	b, err := filepath.EvalSymlinks(right)
	return err == nil && filepath.Clean(a) == filepath.Clean(b)
}

func (w *worker) initResumePicker() {
	if w.resumeResults == nil {
		w.resumeResults = make(chan resumeListResult, 1)
	}
	if w.b.resumeSlots == nil {
		w.b.resumeSlots = make(chan struct{}, 2)
	}
}
func (w *worker) cancelResumeList() {
	w.resumeRequest++
	if w.resumeCancel != nil {
		w.resumeCancel()
		w.resumeCancel = nil
	}
	for token, c := range w.confirms {
		if c.action == "resume" {
			w.dropConfirmation(token, c)
		}
	}
}
func (w *worker) resumeBusy() bool { return w.busy || w.compacting || len(w.queue) != 0 }

func (w *worker) requestResumeList(user int64) {
	if w.resumeBusy() {
		w.say("Wait for the current task and queue to finish before selecting a session.")
		return
	}
	w.initResumePicker()
	if w.resumeCancel != nil {
		w.say("The omp session list is still loading.")
		return
	}
	binding, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	if err != nil || !filepath.IsAbs(binding.Workspace) {
		w.say("No working directory is selected. Use /new <name or path>, or /resume <omp session ID>.")
		return
	}
	cwd, err := filepath.EvalSymlinks(binding.Workspace)
	if err != nil {
		w.say("The working directory is unavailable. No sessions were loaded.")
		return
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		w.say("The working directory is unavailable. No sessions were loaded.")
		return
	}
	w.cancelResumeList()
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	w.resumeCancel = cancel
	request, generation := w.resumeRequest, w.binding.Generation
	cfg := omp.Config{Binary: w.b.cfg.OMP, CWD: cwd, Args: w.b.cfg.OMPArgs}
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		var sessions []omp.SessionSummary
		var err error
		select {
		case w.b.resumeSlots <- struct{}{}:
			sessions, err = omp.ListSessions(ctx, cfg)
			<-w.b.resumeSlots
		case <-ctx.Done():
			err = ctx.Err()
		}
		result := resumeListResult{request: request, generation: generation, user: user, cwd: cwd, sessions: sessions, err: err}
		select {
		case w.resumeResults <- result:
		case <-w.ctx.Done():
		}
	}()
}

func (w *worker) resumeListed(result resumeListResult) {
	if result.request != w.resumeRequest || w.resumeCancel == nil {
		return
	}
	w.resumeCancel()
	w.resumeCancel = nil
	if result.generation != w.binding.Generation {
		return
	}
	if result.err != nil {
		if !errors.Is(result.err, context.Canceled) {
			w.say("Native omp session listing failed: " + result.err.Error())
		}
		return
	}
	if w.resumeBusy() {
		w.say("The conversation became busy. Use /resume again when it is idle.")
		return
	}
	if len(result.sessions) == 0 {
		w.say("omp has no saved sessions in this working directory.")
		return
	}
	w.showResumePage(confirmation{action: "resume", method: "select", workspace: result.cwd, user: result.user, generation: result.generation, sessions: result.sessions}, 0, 0)
}

func menuText(text string, limit int) string {
	text = strings.Join(strings.FieldsFunc(text, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	units := 0
	for i, r := range text {
		n := utf16.RuneLen(r)
		if n < 0 {
			n = 1
		}
		if units+n > limit {
			return text[:i] + "..."
		}
		units += n
	}
	return text
}

func (w *worker) showResumePage(c confirmation, page int, messageID int64) {
	pages := (len(c.sessions) + resumePageSize - 1) / resumePageSize
	if page < 0 || page >= pages {
		return
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		w.say("Cannot create the session selection menu.")
		return
	}
	token := hex.EncodeToString(random[:])
	c.action, c.method, c.page = "resume", "select", page
	c.expires = time.Now().Add(2 * time.Minute)
	c.options = nil
	start, end := page*resumePageSize, min((page+1)*resumePageSize, len(c.sessions))
	var text strings.Builder
	fmt.Fprintf(&text, "Saved omp sessions\nWorkspace: %s\nPage %d/%d\n", menuText(c.workspace, 512), page+1, pages)
	keyboard := &telegram.Keyboard{}
	add := func(label string) {
		index := len(c.options)
		c.options = append(c.options, label)
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegram.Button{{Text: label, CallbackData: fmt.Sprintf("%s:%d", token, index)}})
	}
	for i, session := range c.sessions[start:end] {
		title := menuText(session.Title, 40)
		if title == "" {
			title = "Untitled"
		}
		id := session.ID
		if len(id) > 8 {
			id = id[len(id)-8:]
		}
		marker := ""
		if w.client != nil && strings.EqualFold(session.ID, w.sessionID) {
			marker = " [current]"
		}
		fmt.Fprintf(&text, "\n%d. %s%s\nID: %s\n", i+1, title, marker, session.ID)
		if updated, err := time.Parse(time.RFC3339Nano, session.UpdatedAt); err == nil {
			fmt.Fprintf(&text, "Updated: %s\n", updated.UTC().Format("2006-01-02 15:04 UTC"))
		}
		add(fmt.Sprintf("%d. %s [%s]", i+1, menuText(title, 28), id))
	}
	if page > 0 {
		add("Previous")
	}
	if page+1 < pages {
		add("Next")
	}
	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegram.Button{{Text: "Cancel", CallbackData: fmt.Sprintf("%s:%d", token, len(c.options))}})
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	var err error
	if messageID != 0 {
		err = w.b.tg.Edit(ctx, w.key.chat, messageID, text.String(), keyboard)
	} else {
		var message telegram.Message
		message, err = w.b.tg.Send(ctx, w.key.chat, w.key.thread, text.String(), keyboard)
		messageID = message.MessageID
	}
	if err != nil {
		w.say("Failed to display the session selection menu. Use /resume to try again.")
		return
	}
	c.messageID = messageID
	w.confirms[token] = c
}

func (w *worker) selectResume(c confirmation, index int, messageID int64) {
	if w.resumeBusy() {
		w.clearKeyboard(messageID)
		w.say("The conversation is busy. Use /resume again after the task and queue finish.")
		return
	}
	start, end := c.page*resumePageSize, min((c.page+1)*resumePageSize, len(c.sessions))
	if start < 0 || start >= len(c.sessions) || index < 0 {
		w.clearKeyboard(messageID)
		return
	}
	rows := end - start
	if index < rows {
		w.clearKeyboard(messageID)
		selected := c.sessions[start+index]
		if !sameWorkspace(selected.CWD, c.workspace) {
			w.say("The selected session is not in this working directory.")
			return
		}
		if w.client != nil && strings.EqualFold(selected.ID, w.sessionID) {
			w.say("This omp session is already active in this conversation.")
			return
		}
		if w.b.sessionInUse(selected.ID) {
			w.say("This session is active in another conversation. Close that instance first.")
			return
		}
		if !validSessionID(selected.ID) {
			w.say("omp returned an unsupported session ID.")
			return
		}
		w.start(true, selected.ID, c.workspace, true)
		return
	}
	if c.page > 0 {
		if index == rows {
			w.showResumePage(c, c.page-1, messageID)
			return
		}
		rows++
	}
	if end < len(c.sessions) {
		if index == rows {
			w.showResumePage(c, c.page+1, messageID)
			return
		}
		rows++
	}
	if index == rows {
		w.clearKeyboard(messageID)
	}
}
