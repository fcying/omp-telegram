package bridge

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"omp-telegram/internal/config"
	"omp-telegram/internal/media"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

type Bridge struct {
	cfg           config.Config
	db            *store.Store
	tg            *telegram.Client
	bot           telegram.User
	slots         chan struct{}
	wg            sync.WaitGroup
	fatal         chan error
	mediaSlots    chan struct{}
	resumeSlots   chan struct{}
	sessionMu     sync.Mutex
	sessionClaims map[string]sessionClaim
}
type incoming struct {
	id       int64
	msg      *telegram.Message
	callback *telegram.CallbackQuery
}
type target struct{ chat, thread int64 }

func supportedConversation(m *telegram.Message) bool {
	return m != nil && (m.MessageThreadID != 0 || m.Chat.Type == "private")
}

type queued struct {
	id        int64
	user      int64
	text      string
	images    []media.Image
	preparing bool
	cancel    context.CancelFunc
	directory string
}

type confirmation struct {
	action, uiID, method string
	workspace            string
	options              []string
	expires              time.Time
	generation           int64
	user                 int64
	messageID            int64
	sessions             []omp.SessionSummary
	models               []omp.ModelRole
	page                 int
}
type worker struct {
	b                   *Bridge
	key                 target
	input               chan incoming
	client              *omp.Client
	binding             store.Binding
	startIntent         *store.StartIntent
	sessionID           string
	claimedSession      string
	restoring           bool
	queue               []queued
	stream              strings.Builder
	active              int64
	owner               int64
	turn                uint64
	finishing           bool
	compacting          bool
	progress            progressState
	operations          chan operationResult
	background          sync.WaitGroup
	busy                bool
	lastTyping          time.Time
	preview             string
	lastAssistant       *terminalAssistant
	finalAssistantTexts []string
	lastPreview         string
	previewID           int64
	previewBusy         bool
	progressSuppressed  bool
	previewResult       chan previewResult
	confirms            map[string]confirmation
	keyboardCleanup     chan int64
	mediaResults        chan mediaResult
	sendResults         chan sendResult
	hostRequests        map[string]context.CancelFunc
	resumeResults       chan resumeListResult
	resumeCancel        context.CancelFunc
	resumeRequest       uint64
	ctx                 context.Context
	cancel              context.CancelFunc
}

type terminalAssistant struct {
	StopReason   string
	ErrorMessage string
}
type previewResult struct {
	id         int64
	text       string
	generation int64
	turn       uint64
	err        error
}

const (
	maxRecentTools   = 6
	maxActiveTools   = 6
	maxToolNameUnits = 64
	maxPreviewUnits  = 2200
	maxProgressUnits = 3500
)

type progressState struct {
	ActiveTools map[string]progressTool
	RecentTools []progressTool
	Retrying    bool
}

type progressTool struct {
	ID      string
	Name    string
	Running bool
	IsError bool
}

type operationResult struct {
	generation int64
	kind       string
	cancelled  bool
	err        error
}
type callbackResult bool

const (
	callbackDone      callbackResult = true
	callbackUncertain callbackResult = false
)

type rpcEvent struct {
	Type                  string                       `json:"type"`
	ID                    string                       `json:"id"`
	Method                string                       `json:"method"`
	TargetID              string                       `json:"targetId"`
	Arguments             json.RawMessage              `json:"arguments"`
	Title                 string                       `json:"title"`
	Message               json.RawMessage              `json:"message"`
	Options               []string                     `json:"options"`
	AgentInvoked          *bool                        `json:"agentInvoked"`
	IsTerminal            *bool                        `json:"isTerminal"`
	Success               bool                         `json:"success"`
	Command               string                       `json:"command"`
	ToolName              string                       `json:"toolName"`
	ToolCallID            string                       `json:"toolCallId"`
	IsError               bool                         `json:"isError"`
	AssistantMessageEvent struct{ Type, Delta string } `json:"assistantMessageEvent"`
	Messages              []message                    `json:"messages"`
}

type message struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
}

type terminalResult int

const (
	terminalDone terminalResult = iota
	terminalUncertain
	terminalCancelled
)

var botCommands = []telegram.BotCommand{
	{Command: "new", Description: "New session: /new <name or project path>"},
	{Command: "stop", Description: "Stop task and clear queue"},
	{Command: "review", Description: "Run omp review: /review [arguments]"},
	{Command: "close", Description: "Close omp, keep workspace and session"},
	{Command: "resume", Description: "Choose an omp session in this working directory"},
	{Command: "status", Description: "Show session, model, context, speed and queue"},
	{Command: "name", Description: "Name the omp session: /name <title>"},
	{Command: "model", Description: "Choose a cycle role or /model provider/model"},
	{Command: "thinking", Description: "Choose the thinking level for this session"},
	{Command: "fast", Description: "Choose fast mode: /fast [on|off|status]"},
	{Command: "compact", Description: "Compact context after confirmation"},
	{Command: "handoff", Description: "Run native handoff: /handoff [instructions]"},
	{Command: "help", Description: "Show usage help"},
}

func commandHelp() string {
	var help strings.Builder
	for i, command := range botCommands {
		if i > 0 {
			help.WriteByte('\n')
		}
		help.WriteByte('/')
		help.WriteString(command.Command)
		help.WriteString(" - ")
		help.WriteString(command.Description)
	}
	return help.String()
}

func Run(ctx context.Context, cfg config.Config, db *store.Store) error {
	tg := telegram.New(cfg.Token)
	bot, err := tg.GetMe(ctx)
	if err != nil {
		return err
	}
	if err = db.CheckBot(bot.ID); err != nil {
		return err
	}
	// Replace both previously registered lists so all clients receive English descriptions.
	for _, language := range []string{"", "zh"} {
		if err = tg.SetCommands(ctx, botCommands, language); err != nil {
			return fmt.Errorf("register Telegram commands: %w", err)
		}
	}
	log.Print("Telegram command menus registered (English)")
	b := &Bridge{cfg: cfg, db: db, tg: tg, bot: bot, slots: make(chan struct{}, cfg.MaxWorkers), fatal: make(chan error, 1), mediaSlots: make(chan struct{}, 2), resumeSlots: make(chan struct{}, 2)}
	ctx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); b.wg.Wait() }()
	b.cleanupDatabase(ctx)
	workers := map[target]*worker{}
	if err := b.restoreWorkers(ctx, workers); err != nil {
		return err
	}
	b.wg.Add(2)
	if cfg.DatabaseRetentionDays > 0 {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.runDatabaseJanitor(ctx)
		}()
	}
	go func() {
		defer b.wg.Done()
		if err := b.deliver(ctx); err != nil {
			b.fail(err)
		}
	}()
	wake := make(chan struct{}, 1)
	go func() {
		defer b.wg.Done()
		for ctx.Err() == nil {
			offset, err := db.Offset()
			if err != nil {
				b.fail(err)
				return
			}
			updates, err := tg.GetUpdates(ctx, offset)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("Telegram polling failed: %v; retrying", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				continue
			}
			for _, u := range updates {
				raw, err := json.Marshal(u)
				if err == nil {
					err = db.Accept(u.UpdateID, raw)
				}
				if err != nil {
					b.fail(err)
					return
				}
			}
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}()
	delivered := map[int64]bool{}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		inputs, err := db.Pending()
		if err != nil {
			return err
		}
		pendingIDs := make(map[int64]bool, len(inputs))
		for _, in := range inputs {
			pendingIDs[in.ID] = true
		}
		for id := range delivered {
			if !pendingIDs[id] {
				delete(delivered, id)
			}
		}
		blocked := make(map[target]bool)
		for _, in := range inputs {
			if delivered[in.ID] {
				continue
			}
			var u telegram.Update
			if json.Unmarshal(in.Raw, &u) != nil {
				if err = db.Mark(in.ID, "ignored"); err != nil {
					return err
				}
				continue
			}
			m := u.Message
			var user int64
			if m != nil && m.From != nil {
				user = m.From.ID
			}
			if u.CallbackQuery != nil {
				m = u.CallbackQuery.Message
				user = u.CallbackQuery.From.ID
			}
			if m == nil || !cfg.Authorized(user, m.Chat.ID) {
				if err = db.Mark(in.ID, "ignored"); err != nil {
					return err
				}
				continue
			}
			if !supportedConversation(m) {
				if err = db.Enqueue(m.Chat.ID, 0, "This bot supports private chats and Telegram topics. In groups, use it inside a topic."); err != nil {
					return err
				}
				if err = db.Mark(in.ID, "done"); err != nil {
					return err
				}
				continue
			}
			key := target{m.Chat.ID, m.MessageThreadID}
			if blocked[key] {
				continue
			}
			w := workers[key]
			if w == nil {
				w = b.launchWorker(ctx, key, store.Binding{}, false, nil)
				workers[key] = w
			}
			select {
			case w.input <- incoming{in.ID, m, u.CallbackQuery}:
				delivered[in.ID] = true
			default:
				blocked[key] = true
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-b.fatal:
			return err
		case <-wake:
		case <-tick.C:
		}
	}
}

const databaseJanitorInterval = 24 * time.Hour

func (b *Bridge) cleanupDatabase(ctx context.Context) {
	if b.cfg.DatabaseRetentionDays == 0 {
		return
	}
	result, err := b.db.CleanupMessages(ctx, time.Now().AddDate(0, 0, -b.cfg.DatabaseRetentionDays).Unix())
	for _, path := range result.AttachmentPaths {
		removeOutboxSnapshot(filepath.Join(b.cfg.DataDir, "attachments", "outbox"), path)
	}
	if err != nil {
		log.Printf("database cleanup failed: %v", err)
		return
	}
	b.reconcileOutboxSnapshots(ctx, time.Now().AddDate(0, 0, -b.cfg.DatabaseRetentionDays))
	if result.Inbox != 0 || result.Outbox != 0 {
		log.Printf("database cleanup completed: inbox=%d outbox=%d", result.Inbox, result.Outbox)
	}
}

func (b *Bridge) runDatabaseJanitor(ctx context.Context) {
	ticker := time.NewTicker(databaseJanitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.cleanupDatabase(ctx)
		}
	}
}

func removeOutboxSnapshot(spoolRoot, path string) {
	rel, err := filepath.Rel(spoolRoot, path)
	if err != nil || !filepath.IsLocal(rel) {
		return
	}
	root, err := os.OpenRoot(spoolRoot)
	if err != nil {
		return
	}
	defer root.Close()
	if err = root.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Print("database cleanup snapshot removal failed")
	}
}

func (b *Bridge) reconcileOutboxSnapshots(ctx context.Context, cutoff time.Time) {
	paths, err := b.db.OutboxAttachmentPaths(ctx)
	if err != nil {
		log.Print("database cleanup snapshot reconciliation failed")
		return
	}
	referenced := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		referenced[path] = struct{}{}
	}
	spoolRoot := filepath.Join(b.cfg.DataDir, "attachments", "outbox")
	root, err := os.OpenRoot(spoolRoot)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Print("database cleanup snapshot reconciliation failed")
		}
		return
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		log.Print("database cleanup snapshot reconciliation failed")
		return
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		log.Print("database cleanup snapshot reconciliation failed")
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "attachment-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if _, exists := referenced[filepath.Join(spoolRoot, entry.Name())]; exists {
			continue
		}
		if err = root.Remove(entry.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Print("database cleanup snapshot removal failed")
		}
	}
}

func (b *Bridge) deliver(ctx context.Context) error {
	for ctx.Err() == nil {
		o, e := b.db.NextOutput()
		if errors.Is(e, sql.ErrNoRows) {
			select {
			case <-ctx.Done():
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if e != nil {
			log.Print("outbox read failed")
			return e
		}
		if e = b.db.MarkOutput(o.ID, "sending"); e != nil {
			return e
		}
		switch o.Kind {
		case "text":
			_, e = b.tg.Send(ctx, o.Chat, o.Thread, o.Text, nil)
		case "photo", "document":
			_, e = b.tg.SendFile(ctx, o.Chat, o.Thread, o.Kind, o.Path, o.Name, o.Text)
		default:
			return errors.New("unsupported outbox content kind")
		}
		state := "done"
		if e != nil {
			state = "failed"
			if telegram.DeliveryUncertain(e) {
				state = "uncertain"
			}
			log.Printf("Telegram delivery %s: %v; not replaying automatically", state, e)
		}
		if e = b.db.MarkOutput(o.ID, state); e != nil {
			return e
		}
		if o.Kind != "text" {
			if state == "done" {
				_ = os.Remove(o.Path)
			} else {
				if e = b.db.Enqueue(o.Chat, o.Thread, "Attachment delivery "+state+". It will not be replayed automatically."); e != nil {
					return e
				}
			}
		}
	}
	return nil
}
func (b *Bridge) fail(err error) {
	select {
	case b.fatal <- err:
	default:
	}
}
func (w *worker) say(s string) {
	for _, part := range split(s, 3800) {
		if e := w.b.db.Enqueue(w.key.chat, w.key.thread, part); e != nil {
			log.Print("outbox write failed; stopping worker")
			w.b.fail(e)
			w.cancel()
			return
		}
	}
}
func (w *worker) mark(id int64, state string) bool {
	if id == 0 {
		return true
	}
	if e := w.b.db.Mark(id, state); e != nil {
		w.b.fail(e)
		w.cancel()
		return false
	}
	return true
}
func (w *worker) run() {
	w.initMedia()
	w.initResumePicker()
	defer func() { w.cancel(); w.background.Wait(); w.drainMediaResults() }()
	defer w.shutdown()
	if w.startIntent != nil {
		w.say("A requested " + w.startIntent.Kind + " session start was interrupted before omp identity was saved. The outcome is uncertain. Use /close to cancel it, then use /new or /resume explicitly.")
	} else if w.restoring {
		w.start(true, w.binding.Session, w.binding.Workspace, false)
		w.restoring = false
	}
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	for {
		var events <-chan json.RawMessage
		var done <-chan struct{}
		if w.client != nil {
			events = w.client.Events()
			done = w.client.Done()
		}
		if events != nil {
			select {
			case raw, ok := <-events:
				if !ok {
					w.failed()
				} else {
					w.event(raw)
				}
				continue
			default:
			}
		}
		select {
		case <-w.ctx.Done():
			return
		case in := <-w.input:
			w.handle(in)
		case raw, ok := <-events:
			if !ok {
				w.failed()
				continue
			}
			w.event(raw)
		case <-done:
			w.failed()
		case result := <-w.previewResult:
			w.previewFinished(result)
		case result := <-w.operations:
			if result.generation == w.binding.Generation && w.client != nil {
				w.compacting = false
				w.busy = false
				if result.kind == "handoff" {
					switch {
					case result.err != nil:
						w.say("Handoff failed or its outcome is uncertain. It will not be replayed automatically.")
					case result.cancelled:
						w.say("Handoff canceled without a result.")
					default:
						w.say("Handoff completed.")
					}
				} else if result.err != nil {
					w.say("Compaction failed.")
				} else {
					w.say("Compaction completed.")
				}
			}
		case result := <-w.mediaResults:
			w.preparedMedia(result)
		case result := <-w.sendResults:
			w.preparedSend(result)
		case result := <-w.resumeResults:
			w.resumeListed(result)
		case <-tick.C:
			w.typing()
			w.flushPreview()
			w.expire()
		}
		w.dispatch()
	}
}
func (w *worker) shutdown() { w.shutdownWithSlot(true) }

func (w *worker) shutdownWithSlot(releaseSlot bool) {
	if !w.restoring && w.ctx.Err() == nil {
		w.persistClosed()
	}
	w.cancelResumeList()
	for _, q := range w.queue {
		if q.cancel != nil {
			q.cancel()
		}
		removeIncoming(w.binding.Workspace, q.directory)
	}
	for id, cancel := range w.hostRequests {
		cancel()
		delete(w.hostRequests, id)
	}
	w.progress = progressState{}
	w.progressSuppressed = false
	w.turn++
	w.finishing = false
	w.compacting = false
	w.preview = ""
	w.finalAssistantTexts = nil
	w.lastAssistant = nil
	w.stream.Reset()
	w.clearConfirmations()
	if w.client != nil {
		w.client.Close()
		w.client = nil
		if releaseSlot {
			<-w.b.slots
		}
	}
	w.releaseSession()
	if w.active != 0 {
		w.mark(w.active, "uncertain")
		if w.binding.Running && w.ctx.Err() != nil {
			if err := w.b.db.SetInterrupted(w.binding, true); err != nil {
				w.b.fail(err)
			} else {
				w.binding.Interrupted = true
			}
		}
	}
}
func (w *worker) failed() {
	w.say("omp exited. The task outcome is uncertain and will not be replayed automatically. Use /resume to restore the session.")
	w.shutdown()
	w.busy = false
	w.active = 0
	w.clearQueue()
}
func (w *worker) clearQueue() {
	for _, q := range w.queue {
		if q.cancel != nil {
			q.cancel()
		}
		removeIncoming(w.binding.Workspace, q.directory)
		w.mark(q.id, "cancelled")
	}
	w.queue = nil
}
func (w *worker) call(kind string, fields map[string]any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	defer cancel()
	return w.client.Call(ctx, kind, fields)
}

func (w *worker) newWorkspace(arg string) (string, error) {
	if arg != "" {
		return resolveWorkspace(w.b.cfg.WorkspaceRoot, arg)
	}
	old, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	if errors.Is(err, sql.ErrNoRows) || err == nil && old.Workspace == "" {
		return resolveWorkspace(w.b.cfg.WorkspaceRoot, w.b.cfg.WorkspaceRoot)
	}
	if err != nil {
		return "", errors.New("Failed to read the session binding.")
	}
	if !filepath.IsAbs(old.Workspace) {
		return "", errors.New("Cannot determine the working directory. Use /new <name or project path>.")
	}
	return old.Workspace, nil
}

func (w *worker) start(resume bool, target, expectedCWD string, replace bool) {
	if w.client != nil && !replace {
		w.say("An instance is already running. Use /new for a fresh session, or /close before resuming another session.")
		return
	}
	if w.startIntent != nil && !w.restoring {
		w.say("A previous session start is still uncertain. Use /close before starting another session.")
		return
	}
	old, e := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		w.say("Failed to read the session.")
		return
	}
	var cwd, session string
	if resume {
		if w.restoring {
			info, err := os.Stat(target)
			if !filepath.IsAbs(target) || err != nil || !info.Mode().IsRegular() || !filepath.IsAbs(expectedCWD) {
				w.say("Cannot restore the saved omp session. Its session file or working directory is unavailable. No replacement session was created; use /resume to select a session.")
				return
			}
			cwd = expectedCWD
		} else if !validSessionID(target) {
			w.say("Usage: /resume, or /resume <omp session ID>.")
			return
		}
		if w.b.sessionInUse(target) {
			w.say("This session is already running in another conversation. Close that instance first, or use a full session ID.")
			return
		}
		session = target
	} else {
		cwd = target
		if !filepath.IsAbs(cwd) {
			w.say("Invalid working directory.")
			return
		}
	}
	reuseSlot := replace && w.client != nil
	reserved := false
	if !reuseSlot {
		select {
		case w.b.slots <- struct{}{}:
			reserved = true
		default:
			w.say("The active instance limit has been reached.")
			return
		}
	}
	if !resume {
		if e = os.MkdirAll(cwd, 0700); e != nil {
			if reserved {
				<-w.b.slots
			}
			w.say("Failed to create the working directory.")
			return
		}
	}
	if !w.restoring {
		intentWorkspace := cwd
		if intentWorkspace == "" {
			intentWorkspace = old.Workspace
		}
		intent := store.StartIntent{Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread, Workspace: intentWorkspace, Session: session, Generation: old.Generation + 1, Kind: "new"}
		if resume {
			intent.Kind = "resume"
		}
		if e = w.b.db.PrepareStart(old, intent); e != nil {
			if reserved {
				<-w.b.slots
			}
			w.say("Failed to save the startup intent.")
			return
		}
		w.startIntent = &intent
		w.binding = old
		w.binding.Running = false
		if replace {
			w.clearQueue()
			w.shutdownWithSlot(false)
			if reuseSlot {
				reserved = true
			}
			w.active = 0
			w.busy = false
		}
	}
	c, e := omp.Start(w.ctx, omp.Config{Binary: w.b.cfg.OMP, CWD: cwd, Resume: session, Args: w.b.cfg.OMPArgs})
	if e != nil {
		if reserved {
			<-w.b.slots
		}
		if resume {
			w.say("Failed to resume omp. The startup outcome is uncertain; use /close before retrying.")
		} else {
			w.say("Failed to start omp. The startup outcome is uncertain; use /close before retrying.")
		}
		return
	}
	w.client = c
	w.initMedia()
	ctx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
	info, e := c.SessionInfo(ctx)
	cancel()
	if e != nil {
		w.shutdown()
		w.say("Cannot obtain omp session identity and working directory. The instance has been closed.")
		return
	}
	if resume && !w.restoring && !strings.HasPrefix(strings.ToLower(info.ID), strings.ToLower(target)) {
		w.shutdown()
		w.say("The resumed session did not match the requested omp session ID.")
		return
	}
	if w.restoring && !sameSessionFile(info.File, target) {
		w.shutdown()
		w.say("omp did not restore the saved session file. The instance has been closed.")
		return
	}
	if !resume {
		expectedCWD = cwd
	}
	if expectedCWD != "" && !sameWorkspace(info.CWD, expectedCWD) {
		w.shutdown()
		w.say("omp selected a different working directory. The instance has been closed.")
		return
	}
	if !w.claimSession(info.File, info.ID) {
		w.shutdown()
		w.say("This omp session is already active in another conversation.")
		return
	}
	if _, e = w.call("set_host_tools", map[string]any{"tools": telegramSendTools}); e != nil {
		w.shutdown()
		w.say("Cannot register Telegram attachment delivery. The instance has been closed.")
		return
	}
	interrupted := w.restoring && old.Interrupted
	binding := store.Binding{Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread, Workspace: info.CWD, Session: info.File, Generation: old.Generation + 1, Running: true}
	if w.restoring {
		e = w.b.db.Save(binding)
	} else {
		e = w.b.db.CommitStart(binding)
	}
	if e != nil {
		w.shutdown()
		w.say("Failed to save the session binding. The instance has been closed.")
		return
	}
	w.binding = binding
	w.startIntent = nil
	w.preview, w.lastPreview = "", ""
	w.previewID = 0
	ready := "omp is ready.\nWorkspace: " + info.CWD + "\nSession: " + info.ID
	if interrupted {
		ready += "\n\n⚠️ Gateway restarted while the previous task was active. It was interrupted and was not resubmitted."
	}
	w.say(ready)
}
func (w *worker) handle(in incoming) {
	if in.callback != nil {
		if !w.mark(in.id, "submitted") {
			return
		}
		if w.callback(in.callback) == callbackUncertain {
			w.mark(in.id, "uncertain")
		} else {
			w.mark(in.id, "done")
		}
		return
	}
	text := strings.TrimSpace(in.msg.Text)
	hasAttachment := len(in.msg.Photo) != 0 || in.msg.Document != nil
	if hasAttachment {
		text = strings.TrimSpace(in.msg.Caption)
	}
	if text == "" && !hasAttachment {
		w.mark(in.id, "ignored")
		return
	}
	if hasAttachment {
		if w.client == nil {
			w.say("No instance is running in this conversation. Start with /new <name or project path>, or use /resume for a saved session.")
			w.mark(in.id, "done")
			return
		}
		if len(w.queue) >= w.b.cfg.QueueCapacity {
			w.say("The queue is full. This message was not submitted.")
			w.mark(in.id, "cancelled")
			return
		}
		w.queueMedia(in)
		return
	}
	if !strings.HasPrefix(text, "/") {
		w.enqueuePrompt(in, text)
		return
	}
	fields := strings.Fields(text)
	cmd := fields[0]
	if name, bot, ok := strings.Cut(cmd, "@"); ok {
		if !strings.EqualFold(bot, w.b.bot.Username) {
			w.mark(in.id, "ignored")
			return
		}
		cmd = name
	}
	arg := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	if cmd == "/review" {
		prompt := "/review"
		if arg != "" {
			prompt += " " + arg
		}
		w.enqueuePrompt(in, prompt)
		return
	}
	if !w.mark(in.id, "submitted") {
		return
	}
	defer w.mark(in.id, "done")
	switch cmd {
	case "/help", "/start":
		w.say(commandHelp())
	case "/new":
		workspace, err := w.newWorkspace(arg)
		if err != nil {
			w.say(err.Error())
			return
		}
		if w.client != nil {
			w.confirm(confirmation{action: "new", workspace: workspace, user: in.msg.From.ID}, "Start a new omp session in: "+workspace+"?\nExisting files and session history will be preserved.", []string{"Confirm", "Cancel"})
		} else {
			w.start(false, workspace, "", false)
		}
	case "/resume":
		if arg == "" {
			w.requestResumeList(in.msg.From.ID)
		} else {
			w.start(true, arg, "", false)
		}
	case "/close":
		if !w.cancelStart() || !w.persistClosed() {
			return
		}
		w.clearQueue()
		w.shutdown()
		w.active = 0
		w.busy = false
		w.say("The instance is closed. The session has been preserved.")
	case "/stop":
		w.clearQueue()
		if w.client != nil {
			if _, e := w.call("abort", nil); e != nil {
				w.say("The abort request failed.")
				return
			}
		}
		w.say("Abort requested and queued prompts cleared.")
	case "/status":
		w.status()
	case "/name":
		if arg == "" {
			w.say("Usage: /name <session title>")
			return
		}
		if w.client == nil {
			w.say("No instance is running.")
			return
		}
		if _, err := w.call("set_session_name", map[string]any{"name": arg}); err != nil {
			w.say("The session name change could not be confirmed. Use /status to check before retrying.")
			return
		}
		w.say("Session named: " + menuText(arg, 160))
	case "/model":
		if arg == "" {
			w.showModelPicker(in.msg.From.ID)
			return
		}
		provider, model, ok := strings.Cut(arg, "/")
		if !ok || provider == "" || model == "" {
			w.say("Usage: /model or /model provider/model")
			return
		}
		w.switchModel(provider, model)
	case "/thinking":
		if arg != "" {
			w.say("Usage: /thinking")
			return
		}
		w.showThinkingPicker(in.msg.From.ID)
	case "/fast":
		switch arg {
		case "":
			w.showFastPicker(in.msg.From.ID)
		case "on", "off":
			w.switchFast(arg == "on")
		case "status":
			w.showFastStatus()
		default:
			w.say("Usage: /fast [on|off|status]")
		}
	case "/handoff":
		w.handoff(arg)
	case "/compact":
		if w.client == nil || w.busy {
			w.say("An idle instance is required.")
			return
		}
		w.confirm(confirmation{action: "compact", user: in.msg.From.ID}, "Compact the current session?", []string{"Confirm", "Cancel"})
	default:
		w.say("Unsupported command. Use /help.")
	}
}
func (w *worker) handoff(instructions string) {
	if w.client == nil {
		w.say("No instance is running.")
		return
	}
	if w.busy || w.compacting || w.finishing || len(w.queue) != 0 {
		w.say("Wait for the current task and queue to finish before handoff.")
		return
	}
	var fields map[string]any
	if instructions != "" {
		fields = map[string]any{"customInstructions": instructions}
	}
	w.busy, w.compacting = true, true
	client, generation := w.client, w.binding.Generation
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		raw, err := client.Call(w.ctx, "handoff", fields)
		var result *struct {
			SavedPath string `json:"savedPath"`
		}
		if err == nil {
			err = json.Unmarshal(raw, &result)
		}
		select {
		case w.operations <- operationResult{generation: generation, kind: "handoff", cancelled: result == nil, err: err}:
		case <-w.ctx.Done():
		}
	}()
	w.say("Handoff requested.")
}

func (w *worker) status() {
	if w.client == nil {
		n, _ := w.b.db.Uncertain()
		w.say(fmt.Sprintf("No instance is running. Global uncertain records: %d. Use /resume to restore a session; tasks are not replayed automatically.", n))
		return
	}
	raw, e := w.call("get_state", nil)
	if e != nil {
		w.say("Failed to read the session state.")
		return
	}
	var s statusState
	if json.Unmarshal(raw, &s) != nil {
		w.say("Failed to read the session state.")
		return
	}
	home, _ := os.UserHomeDir()
	w.say(formatStatus(s, w.binding.Workspace, w.sessionID, home, len(w.queue)))
}
func (w *worker) dispatch() {
	if w.client == nil || w.busy || w.compacting || w.finishing || len(w.queue) == 0 {
		return
	}
	if w.queue[0].preparing {
		return
	}
	q := w.queue[0]
	w.queue = w.queue[1:]
	if !w.mark(q.id, "submitted") {
		return
	}
	w.active = q.id
	w.owner = q.user
	w.turn++
	w.busy = true
	w.progress = progressState{ActiveTools: make(map[string]progressTool)}
	w.progressSuppressed = false
	w.preview = ""
	w.stream.Reset()
	w.lastPreview = ""
	w.previewID = 0
	fields := map[string]any{"message": q.text}
	if len(q.images) > 0 {
		fields["images"] = q.images
	}
	raw, err := w.call("prompt", fields)
	if err != nil {
		w.say("Submission failed or its outcome is uncertain. It will not be replayed automatically.")
		w.mark(q.id, "uncertain")
		w.shutdown()
		w.clearQueue()
		w.active = 0
		w.busy = false
		return
	}
	var response struct {
		AgentInvoked *bool `json:"agentInvoked"`
	}
	if json.Unmarshal(raw, &response) == nil && response.AgentInvoked != nil && !*response.AgentInvoked {
		w.finishTerminal(rpcEvent{})
	}
}

func (w *worker) enqueuePrompt(in incoming, text string) {
	if w.client == nil {
		w.say("No instance is running in this conversation. Start with /new <name or project path>, or use /resume for a saved session.")
		w.mark(in.id, "done")
		return
	}
	if len(w.queue) >= w.b.cfg.QueueCapacity {
		w.say("The queue is full. This message was not submitted.")
		w.mark(in.id, "cancelled")
		return
	}
	w.queue = append(w.queue, queued{id: in.id, user: in.msg.From.ID, text: text})
}
func (w *worker) finish() {
	// Progress state is finalized only after the durable result transaction.
	if w.active != 0 {
		text := w.preview
		if err := w.b.db.CompleteInboxWithReplies(w.ctx, w.active, w.key.chat, w.key.thread, split(text, 3800)); err != nil {
			if w.ctx.Err() == nil {
				log.Print("final result commit failed; stopping worker")
				w.b.fail(err)
				w.cancel()
			}
			return
		}
		w.active = 0
		w.busy = false
		w.preview = ""
		w.stream.Reset()
		w.clearConfirmations()
	} else if w.preview != "" {
		w.say(w.preview)
	}
	w.lastAssistant = nil
	w.finalAssistantTexts = nil
	if w.previewBusy {
		w.finishing = true
		return
	}
	w.finishPreview()
}

func (w *worker) finishUncertain(notice string) {
	w.finishIncomplete("uncertain", notice)
}

func (w *worker) finishCancelled(notice string) {
	w.finishIncomplete("cancelled", notice)
}

func (w *worker) finishIncomplete(state, notice string) {
	if w.active == 0 {
		w.lastAssistant = nil
		w.finalAssistantTexts = nil
		return
	}
	text := w.preview
	if text != "" {
		text += "\n\n"
	}
	text += notice
	var err error
	switch state {
	case "uncertain":
		err = w.b.db.CompleteInboxUncertainWithReplies(w.ctx, w.active, w.key.chat, w.key.thread, split(text, 3800))
	case "cancelled":
		err = w.b.db.CompleteInboxCancelledWithReplies(w.ctx, w.active, w.key.chat, w.key.thread, split(text, 3800))
	default:
		panic("invalid terminal state")
	}
	if err != nil {
		if w.ctx.Err() == nil {
			log.Print("incomplete result commit failed; stopping worker")
			w.b.fail(err)
			w.cancel()
		}
		return
	}
	w.active = 0
	w.busy = false
	w.preview = ""
	w.lastAssistant = nil
	w.finalAssistantTexts = nil
	w.stream.Reset()
	w.clearConfirmations()
	if w.previewBusy {
		w.finishing = true
		return
	}
	w.finishPreview()
}

func lastAssistant(messages []message) (message, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			return messages[i], true
		}
	}
	return message{}, false
}

func assistantText(m message) string {
	var blocks []struct{ Type, Text string }
	if json.Unmarshal(m.Content, &blocks) != nil {
		return ""
	}
	var texts []string
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func assistantTexts(messages []message) []string {
	var texts []string
	for _, m := range messages {
		if m.Role == "assistant" {
			if text := assistantText(m); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return texts
}

func assistantTerminalState(m message) terminalResult {
	switch m.StopReason {
	case "aborted":
		return terminalCancelled
	case "error":
		return terminalUncertain
	}
	if m.ErrorMessage != "" {
		return terminalUncertain
	}
	return terminalDone
}

func (w *worker) terminalState(e rpcEvent) terminalResult {
	if m, ok := lastAssistant(e.Messages); ok {
		return assistantTerminalState(m)
	}
	if w.lastAssistant != nil {
		return assistantTerminalState(message{Role: "assistant", StopReason: w.lastAssistant.StopReason, ErrorMessage: w.lastAssistant.ErrorMessage})
	}
	return terminalDone
}

func (w *worker) finishTerminal(e rpcEvent) {
	switch w.terminalState(e) {
	case terminalUncertain:
		w.finishUncertain("omp reported that the task failed before producing a confirmed result. The task outcome is uncertain and will not be replayed automatically.")
	case terminalCancelled:
		w.finishCancelled("Task was cancelled before completion.")
	case terminalDone:
		if w.preview == "" {
			w.finishUncertain("omp ended without a confirmed text result. The task outcome is uncertain and will not be replayed automatically.")
			return
		}
		w.finish()
	}
}
func (w *worker) finishPreview() {
	w.finishing = false
	w.progress = progressState{}
	if w.previewID != 0 {
		ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
		_ = w.b.tg.Edit(ctx, w.key.chat, w.previewID, "Task ended. The complete result follows.", nil)
		cancel()
	}
}
func (w *worker) event(raw []byte) {
	var e rpcEvent
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	switch e.Type {
	case "host_tool_call":
		w.renameProgressTool(e.ToolCallID, e.ToolName)
		w.hostSend(e)
	case "host_tool_cancel":
		if cancel, ok := w.hostRequests[e.TargetID]; ok {
			cancel()
			delete(w.hostRequests, e.TargetID)
		}
	case "tool_execution_start":
		w.startProgressTool(e.ToolCallID, e.ToolName)
	case "tool_execution_end":
		w.finishProgressTool(e.ToolCallID, e.IsError)
	case "message_update":
		if e.AssistantMessageEvent.Type == "text_delta" {
			w.stream.WriteString(e.AssistantMessageEvent.Delta)
			w.preview = w.stream.String()
		}
	case "auto_compaction_start":
		w.compacting = true
	case "auto_compaction_end":
		w.compacting = false
	case "auto_retry_start":
		w.progress.Retrying = true
	case "auto_retry_end":
		w.progress.Retrying = false
	case "agent_start":
		w.busy = true
		w.progress.ActiveTools = make(map[string]progressTool)
		w.lastAssistant = nil
		w.finalAssistantTexts = nil
	case "message_end":
		var m message
		if json.Unmarshal(e.Message, &m) == nil && m.Role == "assistant" {
			w.lastAssistant = &terminalAssistant{StopReason: m.StopReason, ErrorMessage: m.ErrorMessage}
			if text := assistantText(m); text != "" {
				w.finalAssistantTexts = append(w.finalAssistantTexts, text)
			}
		}
	case "agent_end":
		if e.IsTerminal != nil && !*e.IsTerminal {
			return
		}
		texts := assistantTexts(e.Messages)
		if len(texts) > 0 {
			w.preview = strings.Join(texts, "\n\n")
		} else if len(w.finalAssistantTexts) > 0 {
			w.preview = strings.Join(w.finalAssistantTexts, "\n\n")
		}
		w.finishTerminal(e)
	case "prompt_result":
		if e.AgentInvoked != nil && !*e.AgentInvoked {
			w.finishTerminal(e)
		}
	case "response":
		if !e.Success {
			w.say("The omp request failed. Use /status to check the session state.")
			w.mark(w.active, "uncertain")
			w.shutdown()
			w.clearQueue()
			w.active = 0
			w.busy = false
		}
	case "extension_ui_request":
		w.ui(e)
	}
}
func (w *worker) typing() {
	if w.b.cfg.ProgressMode == "off" {
		return
	}
	preparing := len(w.queue) > 0 && w.queue[0].preparing
	if (!w.busy && !preparing) || time.Since(w.lastTyping) < 5*time.Second {
		return
	}
	w.lastTyping = time.Now()
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		ctx, cancel := context.WithTimeout(w.ctx, 4*time.Second)
		defer cancel()
		_ = w.b.tg.Typing(ctx, w.key.chat, w.key.thread)
	}()
}

func clipUTF16(s string, limit int) string {
	if parts := split(s, limit); len(parts) > 0 {
		return parts[0]
	}
	return ""
}

func utf16Length(s string) int {
	units := 0
	for _, r := range s {
		size := utf16.RuneLen(r)
		if size < 0 {
			size = 1
		}
		units += size
	}
	return units
}

func tailUTF16(s string, limit int) string {
	if utf16Length(s) <= limit {
		return s
	}
	runes := []rune(s)
	units, start := 0, len(runes)
	for start > 0 {
		size := utf16.RuneLen(runes[start-1])
		if size < 0 {
			size = 1
		}
		if units+size > limit {
			break
		}
		units += size
		start--
	}
	return string(runes[start:])
}

func (w *worker) addRecentTool(tool progressTool) {
	w.progress.RecentTools = append(w.progress.RecentTools, tool)
	if len(w.progress.RecentTools) > maxRecentTools {
		w.progress.RecentTools = w.progress.RecentTools[len(w.progress.RecentTools)-maxRecentTools:]
	}
}

func (w *worker) startProgressTool(id, name string) {
	if id == "" || name == "" {
		return
	}
	if w.progress.ActiveTools == nil {
		w.progress.ActiveTools = make(map[string]progressTool)
	}
	tool := progressTool{ID: id, Name: clipUTF16(name, maxToolNameUnits), Running: true}
	w.progress.ActiveTools[id] = tool
	w.addRecentTool(tool)
}

func (w *worker) renameProgressTool(id, name string) {
	if id == "" || name == "" {
		return
	}
	tool, ok := w.progress.ActiveTools[id]
	if !ok {
		return
	}
	tool.Name = clipUTF16(name, maxToolNameUnits)
	w.progress.ActiveTools[id] = tool
	for i := len(w.progress.RecentTools) - 1; i >= 0; i-- {
		if w.progress.RecentTools[i].ID == id && w.progress.RecentTools[i].Running {
			w.progress.RecentTools[i] = tool
			return
		}
	}
}

func (w *worker) finishProgressTool(id string, isError bool) {
	tool, ok := w.progress.ActiveTools[id]
	if !ok {
		return
	}
	delete(w.progress.ActiveTools, id)
	tool.Running, tool.IsError = false, isError
	for i := len(w.progress.RecentTools) - 1; i >= 0; i-- {
		if w.progress.RecentTools[i].ID == id {
			w.progress.RecentTools[i] = tool
			return
		}
	}
	w.addRecentTool(tool)
}

func sortedProgressTools(tools map[string]progressTool) []progressTool {
	out := make([]progressTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func progressToolLine(tool progressTool) string {
	state := "completed"
	if tool.Running {
		state = "running"
	} else if tool.IsError {
		state = "failed"
	}
	return "- " + tool.Name + ": " + state
}

func (w *worker) progressStatus() string {
	switch {
	case w.progress.Retrying:
		return "Retrying automatically..."
	case w.compacting:
		return "Compacting context..."
	case len(w.progress.ActiveTools) > 1:
		return "Running tools..."
	case len(w.progress.ActiveTools) == 1:
		return "Running tool..."
	case w.busy:
		return "Running"
	default:
		return ""
	}
}

func (w *worker) renderProgress() string {
	if !w.busy || w.active == 0 {
		return ""
	}
	sections := []string{"Processing..."}
	if status := w.progressStatus(); status != "" {
		sections = append(sections, "Status: "+status)
	}
	active := sortedProgressTools(w.progress.ActiveTools)
	if len(active) > 0 {
		lines := []string{"Tools:"}
		for i, tool := range active {
			if i == maxActiveTools {
				lines = append(lines, fmt.Sprintf("- %d more", len(active)-i))
				break
			}
			lines = append(lines, progressToolLine(tool))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	text := strings.Join(sections, "\n\n")
	if w.preview != "" {
		available := maxProgressUnits - utf16Length(text) - utf16Length("\n\nOutput:\n")
		if available > 0 {
			if available > maxPreviewUnits {
				available = maxPreviewUnits
			}
			text += "\n\nOutput:\n" + tailUTF16(w.preview, available)
		}
	}
	if w.b.cfg.ProgressMode != "verbose" || len(w.progress.RecentTools) == 0 {
		return text
	}
	var recent []string
	for i := len(w.progress.RecentTools) - 1; i >= 0; i-- {
		candidate := append([]string{progressToolLine(w.progress.RecentTools[i])}, recent...)
		section := "Recent tool activity:\n" + strings.Join(candidate, "\n")
		if utf16Length(text)+utf16Length("\n\n")+utf16Length(section) > maxProgressUnits {
			break
		}
		recent = candidate
	}
	if len(recent) > 0 {
		text += "\n\nRecent tool activity:\n" + strings.Join(recent, "\n")
	}
	return text
}

func (w *worker) previewFinished(result previewResult) {
	w.previewBusy = false
	if result.generation != w.binding.Generation || result.turn != w.turn {
		return
	}
	if result.err == nil {
		w.previewID = result.id
		w.lastPreview = result.text
	} else if result.id == 0 {
		w.progressSuppressed = true
	}
	if w.finishing {
		w.finishPreview()
	}
}

func (w *worker) flushPreview() {
	if w.b.cfg.ProgressMode == "off" || w.progressSuppressed {
		return
	}
	snapshot := w.renderProgress()
	if w.finishing || snapshot == "" || w.previewBusy || snapshot == w.lastPreview {
		return
	}
	text := snapshot
	id := w.previewID
	gen := w.binding.Generation
	turn := w.turn
	w.previewBusy = true
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
		defer cancel()
		var err error
		if id == 0 {
			var message telegram.Message
			message, err = w.b.tg.Send(ctx, w.key.chat, w.key.thread, text, nil)
			if err == nil {
				id = message.MessageID
			}
		} else {
			err = w.b.tg.Edit(ctx, w.key.chat, id, text, nil)
		}
		select {
		case w.previewResult <- previewResult{id: id, text: snapshot, generation: gen, turn: turn, err: err}:
		case <-w.ctx.Done():
		}
	}()
}
func (w *worker) confirm(c confirmation, title string, options []string) {
	parts := split(title, 3800)
	if len(parts) == 0 {
		title = "Confirmation required"
	} else {
		title = parts[0]
	}
	var data [12]byte
	if _, e := rand.Read(data[:]); e != nil {
		return
	}
	token := hex.EncodeToString(data[:])
	c.expires = time.Now().Add(2 * time.Minute)
	c.generation = w.binding.Generation
	if c.user == 0 {
		c.user = w.owner
	}
	k := &telegram.Keyboard{}
	for i, label := range options {
		k.InlineKeyboard = append(k.InlineKeyboard, []telegram.Button{{Text: label, CallbackData: fmt.Sprintf("%s:%d", token, i)}})
	}
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	message, e := w.b.tg.Send(ctx, w.key.chat, w.key.thread, title, k)
	if e != nil {
		w.cancelUI(c)
		w.say("Failed to send the confirmation. The operation was canceled.")
		return
	}
	c.messageID = message.MessageID
	w.confirms[token] = c
}
func (w *worker) callback(q *telegram.CallbackQuery) callbackResult {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	token, index, ok := strings.Cut(q.Data, ":")
	c, exists := w.confirms[token]
	if !ok || !exists || c.user != q.From.ID {
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
		return callbackDone
	}
	if time.Now().After(c.expires) || c.generation != w.binding.Generation {
		delete(w.confirms, token)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
		w.clearKeyboard(c.messageID)
		if c.generation == w.binding.Generation {
			w.cancelUI(c)
		}
		return callbackDone
	}
	n, err := strconv.Atoi(index)
	if err != nil || n < 0 || (c.method != "select" && n > 1) || (c.method == "select" && n > len(c.options)) {
		return callbackDone
	}
	_ = w.b.tg.AnswerCallback(ctx, q.ID, "Received")
	if c.action == "resume" {
		delete(w.confirms, token)
		messageID := c.messageID
		if messageID == 0 && q.Message != nil {
			messageID = q.Message.MessageID
		}
		w.selectResume(c, n, messageID)
		return callbackDone
	}
	delete(w.confirms, token)
	w.clearKeyboard(c.messageID)
	if c.action == "ui" {
		frame := map[string]any{"type": "extension_ui_response", "id": c.uiID}
		if c.method == "confirm" {
			frame["confirmed"] = n == 0
		} else if n >= 0 && n < len(c.options) {
			frame["value"] = c.options[n]
		} else {
			frame["cancelled"] = true
		}
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
		defer cancel()
		if w.client == nil || w.client.Send(ctx, frame) != nil {
			w.say("The confirmation could not be delivered to omp. Its state is uncertain and the instance has been closed.")
			w.shutdown()
			w.clearQueue()
			w.active = 0
			w.busy = false
			return callbackUncertain
		}
		return callbackDone
	}
	if c.action == "model" {
		w.selectModel(c, n)
		return callbackDone
	}
	if c.action == "thinking" {
		w.selectThinking(c, n)
		return callbackDone
	}
	if c.action == "fast" {
		if n < len(c.options) {
			w.switchFast(c.options[n] == "on")
		}
		return callbackDone
	}
	if n != 0 {
		return callbackDone
	}
	switch c.action {
	case "new":
		if !filepath.IsAbs(c.workspace) {
			w.say("Invalid working directory. The existing instance is still running.")
			return callbackDone
		}
		if err := os.MkdirAll(c.workspace, 0700); err != nil {
			w.say("Cannot create the working directory. The existing instance is still running.")
			return callbackDone
		}
		w.start(false, c.workspace, "", true)
	case "compact":
		if w.client == nil || w.busy {
			w.say("The instance is not idle. Compaction was canceled.")
			return callbackDone
		}
		w.busy = true
		w.compacting = true
		client := w.client
		generation := w.binding.Generation
		w.background.Add(1)
		go func() {
			defer w.background.Done()
			_, err := client.Call(w.ctx, "compact", nil)
			select {
			case w.operations <- operationResult{generation: generation, err: err}:
			case <-w.ctx.Done():
			}
		}()
	}
	return callbackDone
}

func (w *worker) clearKeyboard(messageID int64) {
	if messageID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	_ = w.b.tg.ClearKeyboard(ctx, w.key.chat, messageID)
}

func (w *worker) queueKeyboardCleanup(messageID int64) {
	if messageID == 0 || w.ctx.Err() != nil {
		return
	}
	if w.keyboardCleanup == nil {
		w.keyboardCleanup = make(chan int64, 32)
		w.background.Add(1)
		go func() {
			defer w.background.Done()
			for {
				select {
				case <-w.ctx.Done():
					return
				case id := <-w.keyboardCleanup:
					w.clearKeyboard(id)
				}
			}
		}()
	}
	// Tokens are already invalidated; UI cleanup is expendable under backpressure.
	select {
	case w.keyboardCleanup <- messageID:
	default:
	}
}

func (w *worker) cancelStart() bool {
	if w.startIntent == nil {
		return true
	}
	if err := w.b.db.CancelStart(*w.startIntent); err != nil {
		w.b.fail(err)
		return false
	}
	w.startIntent = nil
	return true
}
func (w *worker) ui(e rpcEvent) {
	switch e.Method {
	case "confirm", "select":
		var msg string
		_ = json.Unmarshal(e.Message, &msg)
		c := confirmation{action: "ui", uiID: e.ID, method: e.Method, options: e.Options}
		options := e.Options
		if e.Method == "confirm" {
			options = []string{"Confirm", "Cancel"}
		} else if len(options) == 0 || len(options) > 20 {
			w.cancelUI(c)
			w.say("The number of options is unsupported. The dialog was canceled.")
			return
		}
		if e.Method == "select" {
			options = append(append([]string(nil), options...), "Cancel")
		}
		w.confirm(c, e.Title+"\n"+msg, options)
	case "input", "editor":
		w.cancelUI(confirmation{uiID: e.ID})
		w.say("Input dialogs are not supported and have been canceled. Provide the information in a normal message.")
	case "cancel":
		for token, c := range w.confirms {
			if c.action == "ui" && c.uiID != "" && c.uiID == e.TargetID {
				w.dropConfirmation(token, c)
			}
		}
	}
}
func (w *worker) cancelUI(c confirmation) {
	if c.uiID != "" && w.client != nil {
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
		defer cancel()
		_ = w.client.Send(ctx, map[string]any{"type": "extension_ui_response", "id": c.uiID, "cancelled": true})
	}
}

// Programmatic invalidation does not send native UI replies. The caller owns
// that decision, since native cancellation and old generations must not echo.
func (w *worker) dropConfirmation(token string, c confirmation) {
	delete(w.confirms, token)
	w.queueKeyboardCleanup(c.messageID)
}

func (w *worker) clearConfirmations() {
	for token, c := range w.confirms {
		w.dropConfirmation(token, c)
	}
}

func (w *worker) expire() {
	for token, c := range w.confirms {
		if time.Now().After(c.expires) {
			w.dropConfirmation(token, c)
			if c.generation == w.binding.Generation {
				w.cancelUI(c)
			}
		}
	}
}
func split(s string, limit int) []string {
	var out []string
	start, n := 0, 0
	for i, r := range s {
		size := utf16.RuneLen(r)
		if size < 0 {
			size = 1
		}
		if n+size > limit {
			out = append(out, s[start:i])
			start = i
			n = 0
		}
		n += size
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
