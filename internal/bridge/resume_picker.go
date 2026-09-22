package bridge

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"omp-telegram/internal/media"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/telegram"
)

const resumePageSize = 8

type resumeListResult struct {
	request                   uint64
	generation, user          int64
	epoch                     uint64
	cwd, action, exportFormat string
	sessions                  []omp.SessionSummary
	err                       error
}

type resumePickerOption struct {
	session int
	action  string
}

type exportResult struct {
	request                          uint64
	generation                       int64
	epoch                            uint64
	format                           string
	sessionID                        string
	workspace                        string
	bindingSession, bindingSessionID string
	file                             media.File
	err                              error
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
	if w.exportResults == nil {
		w.exportResults = make(chan exportResult, 1)
	}
	if w.b.resumeSlots == nil {
		w.b.resumeSlots = make(chan struct{}, 2)
	}
}

func (w *worker) cancelExport() {
	w.exportRequest++
	if w.exportCancel != nil {
		w.exportCancel()
	}
}

func (w *worker) cancelResumeList() {
	w.resumeRequest++
	if w.resumeCancel != nil {
		w.resumeCancel()
		w.resumeCancel = nil
	}
	w.cancelExport()
	for token, c := range w.confirms {
		if c.action == "resume" || c.action == "export" {
			w.dropConfirmation(token, c)
		}
	}
}

func (w *worker) persistedBindingGenerationMatches(generation int64) bool {
	binding, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	return err == nil && binding.Generation == generation
}

func (w *worker) persistedExportBindingMatches(result exportResult) bool {
	binding, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	return err == nil && binding.Generation == result.generation && binding.Workspace == result.workspace && binding.Session == result.bindingSession && binding.SessionID == result.bindingSessionID
}

func (w *worker) resumeBusy() bool {
	return w.busy || w.compacting || len(w.queue) != 0 || w.exportCancel != nil
}

func (w *worker) requestResumeList(user int64) {
	w.requestSessionList(user, "resume", "")
}

func (w *worker) requestExportList(user int64, format string) {
	w.requestSessionList(user, "export", format)
}

func (w *worker) requestDirectExport(user int64, format, sessionID string) {
	if !validSessionID(sessionID) {
		w.say("Usage: /export [html] [session-id].")
		return
	}
	w.initResumePicker()
	if w.resumeCancel != nil || w.exportCancel != nil {
		w.say("The omp session operation is still loading.")
		return
	}
	binding, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	if errors.Is(err, sql.ErrNoRows) || err == nil && binding.Workspace == "" {
		w.say("No working directory is selected. Use /new <name or path>, or /resume first.")
		return
	}
	if err != nil {
		w.say("Failed to read the session.")
		return
	}
	w.binding = binding
	cwd, err := exportWorkspace(binding.Workspace)
	if err != nil {
		w.say(err.Error())
		return
	}
	w.cancelResumeList()
	w.beginExport(omp.SessionSummary{ID: sessionID, CWD: cwd}, format, cwd, binding.Generation)
}

func parseExportArgs(arg string) (format, sessionID string, ok bool) {
	fields := strings.Fields(arg)
	switch len(fields) {
	case 0:
		return "session", "", true
	case 1:
		if fields[0] == "html" {
			return "html", "", true
		}
		if fields[0] == "raw" {
			return "", "", false
		}
		return "session", fields[0], true
	case 2:
		if fields[0] != "html" {
			return "", "", false
		}
		return "html", fields[1], true
	default:
		return "", "", false
	}
}

func exportWorkspace(workspace string) (string, error) {
	if !filepath.IsAbs(workspace) {
		return "", errors.New("The working directory is unavailable. No sessions were loaded.")
	}
	cwd, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", errors.New("The working directory is unavailable. No sessions were loaded.")
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return "", errors.New("The working directory is unavailable. No sessions were loaded.")
	}
	return cwd, nil
}
func (w *worker) requestSessionList(user int64, action, format string) {
	if action != "export" && w.resumeBusy() {
		w.say("Wait for the current task and queue to finish before selecting a session.")
		return
	}
	w.initResumePicker()
	if w.resumeCancel != nil || w.exportCancel != nil {
		w.say("The omp session operation is still loading.")
		return
	}
	binding, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	if errors.Is(err, sql.ErrNoRows) || err == nil && binding.Workspace == "" {
		w.say("No working directory is selected. Use /new <name or path>, or /resume first.")
		return
	}
	if err != nil {
		w.say("Failed to read the session.")
		return
	}
	w.binding = binding
	cwd, err := exportWorkspace(binding.Workspace)
	if err != nil {
		w.say(err.Error())
		return
	}
	w.cancelResumeList()
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	w.resumeCancel = cancel
	request, generation := w.resumeRequest, binding.Generation
	epoch := w.b.bindingsEpoch.Load()
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
		result := resumeListResult{request: request, generation: generation, epoch: epoch, user: user, cwd: cwd, action: action, exportFormat: format, sessions: sessions, err: err}
		select {
		case w.resumeResults <- result:
		case <-w.ctx.Done():
		}
	}()
}

func (w *worker) acceptSessionList(result resumeListResult) bool {
	if result.request != w.resumeRequest || w.resumeCancel == nil {
		return false
	}
	w.resumeCancel()
	w.resumeCancel = nil
	if result.epoch != w.b.bindingsEpoch.Load() {
		w.say("The saved binding changed. Use /resume again.")
		return false
	}
	if result.generation != w.binding.Generation {
		return false
	}
	if !w.persistedBindingGenerationMatches(result.generation) {
		w.say("The saved binding changed. Use /resume again.")
		return false
	}
	if result.err != nil {
		if !errors.Is(result.err, context.Canceled) {
			w.say("Native omp session listing failed: " + result.err.Error())
		}
		return false
	}
	if result.action != "export" && w.resumeBusy() {
		w.say("The conversation became busy. Use /resume again when it is idle.")
		return false
	}
	return true
}
func pinnedSessionKey(sessionID string) string {
	return strings.ToLower(sessionID)
}

func sortPinnedSessions(sessions []omp.SessionSummary, pinned map[string]struct{}) {
	sort.SliceStable(sessions, func(i, j int) bool {
		_, leftPinned := pinned[pinnedSessionKey(sessions[i].ID)]
		_, rightPinned := pinned[pinnedSessionKey(sessions[j].ID)]
		return leftPinned && !rightPinned
	})
}

func sessionIsPinned(session omp.SessionSummary, pinned map[string]struct{}) bool {
	_, ok := pinned[pinnedSessionKey(session.ID)]
	return ok
}

func (w *worker) resumeListed(result resumeListResult) {
	if !w.acceptSessionList(result) {
		return
	}
	if result.action == "export" {
		w.exportListed(result)
		return
	}
	if len(result.sessions) == 0 {
		w.say("omp has no saved sessions in this working directory.")
		return
	}
	pinned, err := w.b.db.PinnedSessions(w.b.bot.ID, w.key.chat, w.key.thread, result.cwd)
	if err != nil {
		w.say("Failed to read pinned sessions.")
		return
	}
	nativeSessions := append([]omp.SessionSummary(nil), result.sessions...)
	sessions := append([]omp.SessionSummary(nil), nativeSessions...)
	sortPinnedSessions(sessions, pinned)
	w.showResumePage(confirmation{action: "resume", method: "select", workspace: result.cwd, user: result.user, generation: result.generation, epoch: result.epoch, sessions: sessions, nativeSessions: nativeSessions, pinnedSessions: pinned}, 0, 0)
}

func (w *worker) exportListed(result resumeListResult) {
	if len(result.sessions) == 0 {
		w.say("omp has no saved sessions in this working directory.")
		return
	}
	w.showResumePage(confirmation{action: "export", method: "select", workspace: result.cwd, exportFormat: result.exportFormat, user: result.user, generation: result.generation, epoch: result.epoch, sessions: result.sessions}, 0, 0)
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

func sessionIDMatches(actual, requested string) bool {
	return strings.EqualFold(actual, requested) || strings.HasPrefix(strings.ToLower(actual), strings.ToLower(requested))
}

func (w *worker) bindingSessionSelected(sessionID string) bool {
	return sessionIDMatches(w.binding.SessionID, sessionID)
}

func (w *worker) exportCurrentBusy(sessionID string) bool {
	return w.bindingSessionSelected(sessionID) && (w.active != 0 || w.busy || w.compacting || w.finishing || len(w.queue) != 0)
}

func (w *worker) beginExport(session omp.SessionSummary, format, workspace string, generation int64) {
	if format != "session" && format != "html" {
		w.say("Usage: /export [html] [session-id].")
		return
	}
	w.initResumePicker()
	if w.exportCancel != nil || w.exportingSession != "" {
		w.say("The omp session operation is still loading.")
		return
	}
	committed, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	if err != nil || committed.Generation != generation {
		w.say("The saved binding changed. Use /export again.")
		return
	}
	currentCandidate := sessionIDMatches(committed.SessionID, session.ID)
	currentExact := strings.EqualFold(committed.SessionID, session.ID)
	if currentCandidate && w.exportCurrentBusy(session.ID) {
		w.say("Wait for the current task and queue to finish before exporting.")
		return
	}
	if !currentExact && omp.HasCustomSessionDir(w.b.cfg.OMPArgs) {
		w.say("Cannot export an inactive session when omp uses a custom session directory.")
		return
	}
	if w.b.sessionInUseByOther(w, session.ID) {
		w.say("This session is currently active in another conversation. Close that instance before exporting it.")
		return
	}
	if !w.b.reserveExport(w, session.ID) {
		w.say("This session is currently being exported. Try again after that export finishes.")
		return
	}
	if currentCandidate {
		w.clearRuntimeConfirmations()
	}
	ctx, cancel := context.WithTimeout(w.ctx, 2*time.Minute)
	w.exportCancel = cancel
	w.exportRequest++
	request := w.exportRequest
	epoch := w.b.bindingsEpoch.Load()
	if currentCandidate {
		w.exportingSession = session.ID
	}
	bindingSession := committed.Session
	bindingSessionID := committed.SessionID
	cfg := omp.Config{Binary: w.b.cfg.OMP, CWD: workspace, Args: w.b.cfg.OMPArgs}
	spool := filepath.Join(w.b.cfg.DataDir, "attachments", "outbox")
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		var file media.File
		var err error
		export := func() {
			source := ""
			if strings.EqualFold(bindingSessionID, session.ID) {
				source = bindingSession
			} else {
				var resolveErr error
				source, resolveErr = omp.ResolveSessionPath(ctx, cfg, session.ID)
				if resolveErr != nil {
					err = resolveErr
					return
				}
			}
			if format == "html" {
				htmlCtx, htmlCancel := context.WithTimeout(ctx, 30*time.Second)
				defer htmlCancel()
				run := func() {
					file, err = snapshotHTML(htmlCtx, w.b.cfg.OMP, source, spool, "omp-session-"+shortSessionID(session.ID)+".html")
				}
				if w.b.mediaSlots == nil {
					run()
				} else {
					select {
					case w.b.mediaSlots <- struct{}{}:
						run()
						<-w.b.mediaSlots
					case <-htmlCtx.Done():
						err = htmlCtx.Err()
					}
				}
				if errors.Is(htmlCtx.Err(), context.DeadlineExceeded) {
					err = errSessionHTMLTimeout
				}
				return
			}
			run := func() {
				file, err = SnapshotSession(ctx, spool, source)
			}
			if w.b.mediaSlots == nil {
				run()
				return
			}
			select {
			case w.b.mediaSlots <- struct{}{}:
				run()
				<-w.b.mediaSlots
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		select {
		case w.b.resumeSlots <- struct{}{}:
			export()
			<-w.b.resumeSlots
		case <-ctx.Done():
			err = ctx.Err()
		}
		result := exportResult{request: request, generation: generation, epoch: epoch, format: exportFormatName(format), sessionID: session.ID, workspace: committed.Workspace, bindingSession: bindingSession, bindingSessionID: bindingSessionID, file: file, err: err}
		select {
		case w.exportResults <- result:
		case <-w.ctx.Done():
			w.cleanupExportResult(result)
			w.b.releaseExport(w)
		}
	}()
}

func (w *worker) cleanupExportResult(result exportResult) {
	if result.file.Path != "" {
		removeMediaSnapshot(result.file.Path, w.log)
	}
}

func (w *worker) exportFinished(result exportResult) {
	defer w.b.releaseExport(w)
	if result.request != w.exportRequest || result.generation != w.binding.Generation || result.epoch != w.b.bindingsEpoch.Load() || w.exportCancel == nil {
		w.cleanupExportResult(result)
		if w.exportCancel != nil {
			w.exportCancel()
			w.exportCancel = nil
			w.exportingSession = ""
		}
		return
	}
	w.exportingSession = ""
	w.exportCancel()
	w.exportCancel = nil
	if result.err != nil {
		w.cleanupExportResult(result)
		w.log.Warn("session export failed", "event", "session_export", "format", result.format, "session_id", result.sessionID, "result", "failed", "reason", sessionExportReason(result.err))
		if !errors.Is(result.err, context.Canceled) {
			w.say(sessionExportMessage(result))
		}
		return
	}
	if !w.persistedExportBindingMatches(result) {
		w.cleanupExportResult(result)
		w.log.Warn("session export discarded", "event", "session_export", "format", result.format, "session_id", result.sessionID, "result", "stale_binding")
		return
	}
	if err := w.b.db.EnqueueAttachment(w.key.chat, w.key.thread, "document", result.file.Path, result.file.Name, ""); err != nil {
		w.cleanupExportResult(result)
		w.b.storeLog.Error("session export persistence failed", "event", "session_export", "format", result.format, "session_id", result.sessionID, "result", "failed", "reason", "persistence")
		w.b.fail(err)
		return
	}
	w.log.Info("session export queued", "event", "session_export", "format", result.format, "session_id", result.sessionID, "result", "done")
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
	if c.action != "export" {
		c.action = "resume"
	}
	c.method, c.page = "select", page
	c.expires = time.Now().Add(2 * time.Minute)
	c.options = nil
	c.pickerOptions = nil
	start, end := page*resumePageSize, min((page+1)*resumePageSize, len(c.sessions))
	var text strings.Builder
	heading := "Saved omp sessions"
	if c.action == "export" {
		heading = "Export omp session"
	}
	fmt.Fprintf(&text, "%s\nWorkspace: %s\n", heading, menuText(c.workspace, 512))
	if c.action == "export" {
		fmt.Fprintf(&text, "Format: %s\n", strings.ToUpper(c.exportFormat))
	}
	fmt.Fprintf(&text, "Page %d/%d\n", page+1, pages)
	keyboard := &telegram.Keyboard{}
	add := func(label string, option resumePickerOption) {
		index := len(c.options)
		c.options = append(c.options, label)
		c.pickerOptions = append(c.pickerOptions, option)
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
		if w.bindingSessionSelected(session.ID) {
			marker = " [current]"
		}
		if c.action == "resume" && sessionIsPinned(session, c.pinnedSessions) {
			marker += " [pinned]"
		}
		sessionIndex := start + i
		fmt.Fprintf(&text, "\n%d. %s%s\nID: %s\n", sessionIndex+1, title, marker, session.ID)
		if updated, err := time.Parse(time.RFC3339Nano, session.UpdatedAt); err == nil {
			fmt.Fprintf(&text, "Updated: %s\n", updated.UTC().Format("2006-01-02 15:04 UTC"))
		}
		add(fmt.Sprintf("%d. %s [%s]", sessionIndex+1, menuText(title, 28), id), resumePickerOption{session: sessionIndex, action: "select"})
		if c.action == "resume" {
			pinLabel := "Pin"
			if sessionIsPinned(session, c.pinnedSessions) {
				pinLabel = "Unpin"
			}
			add(pinLabel, resumePickerOption{session: sessionIndex, action: "toggle_pin"})
		}
	}
	if page > 0 {
		add("Previous", resumePickerOption{action: "previous"})
	}
	if page+1 < pages {
		add("Next", resumePickerOption{action: "next"})
	}
	add("Cancel", resumePickerOption{action: "cancel"})
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
		command := "/resume"
		if c.action == "export" {
			command = "/export"
		}
		w.say("Failed to display the session selection menu. Use " + command + " to try again.")
		return
	}
	c.messageID = messageID
	w.confirms[token] = c
}

func (w *worker) selectResume(c confirmation, index int, messageID int64) {
	w.selectSession(c, index, messageID)
}

func (w *worker) selectSession(c confirmation, index int, messageID int64) {
	command := "/resume"
	if c.action == "export" {
		command = "/export"
	}
	if index < 0 || index >= len(c.pickerOptions) {
		w.clearKeyboard(messageID)
		return
	}
	option := c.pickerOptions[index]
	if c.epoch != w.b.bindingsEpoch.Load() {
		w.clearKeyboard(messageID)
		w.say("The saved binding changed. Use " + command + " again.")
		return
	}
	switch option.action {
	case "previous":
		w.showResumePage(c, c.page-1, messageID)
		return
	case "next":
		w.showResumePage(c, c.page+1, messageID)
		return
	case "cancel":
		w.clearKeyboard(messageID)
		return
	case "toggle_pin":
		w.togglePinnedSession(c, option.session, messageID)
		return
	case "select":
	default:
		w.clearKeyboard(messageID)
		return
	}
	if c.action != "export" && w.resumeBusy() {
		w.clearKeyboard(messageID)
		w.say("The conversation is busy. Use " + command + " again after the task and queue finish.")
		return
	}
	if !w.persistedBindingGenerationMatches(c.generation) {
		w.clearKeyboard(messageID)
		return
	}
	selected := c.sessions[option.session]
	w.clearKeyboard(messageID)
	if !sameWorkspace(selected.CWD, c.workspace) {
		w.say("The selected session is not in this working directory.")
		return
	}
	if !validSessionID(selected.ID) {
		w.say("omp returned an unsupported session ID.")
		return
	}
	if c.action == "export" {
		w.beginExport(selected, c.exportFormat, c.workspace, c.generation)
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
	w.startFenced(true, selected.ID, c.workspace, true, c.generation)
}

func (w *worker) togglePinnedSession(c confirmation, sessionIndex int, messageID int64) {
	if c.epoch != w.b.bindingsEpoch.Load() || !w.persistedBindingGenerationMatches(c.generation) {
		w.clearKeyboard(messageID)
		w.say("The saved binding changed. Use /resume again.")
		return
	}
	if sessionIndex < 0 || sessionIndex >= len(c.sessions) {
		w.clearKeyboard(messageID)
		return
	}
	selected := c.sessions[sessionIndex]
	if !sameWorkspace(selected.CWD, c.workspace) {
		w.clearKeyboard(messageID)
		w.say("The selected session is not in this working directory.")
		return
	}
	if !validSessionID(selected.ID) {
		w.clearKeyboard(messageID)
		w.say("omp returned an unsupported session ID.")
		return
	}
	pinned := sessionIsPinned(selected, c.pinnedSessions)
	changed, err := w.b.db.SetPinnedSessionIfGeneration(w.b.bot.ID, w.key.chat, w.key.thread, c.generation, c.workspace, selected.ID, !pinned)
	if err != nil {
		w.clearKeyboard(messageID)
		w.say("Failed to update pinned sessions.")
		return
	}
	if !changed {
		w.clearKeyboard(messageID)
		w.say("The saved binding changed. Use /resume again.")
		return
	}
	if c.pinnedSessions == nil {
		c.pinnedSessions = make(map[string]struct{})
	}
	if pinned {
		delete(c.pinnedSessions, pinnedSessionKey(selected.ID))
	} else {
		c.pinnedSessions[pinnedSessionKey(selected.ID)] = struct{}{}
	}
	base := c.nativeSessions
	if len(base) == 0 {
		base = c.sessions
	}
	c.sessions = append([]omp.SessionSummary(nil), base...)
	sortPinnedSessions(c.sessions, c.pinnedSessions)
	w.showResumePage(c, 0, messageID)
}

func (w *worker) drainExportResults() {
	for {
		select {
		case result := <-w.exportResults:
			w.cleanupExportResult(result)
			w.b.releaseExport(w)
		default:
			return
		}
	}
}
