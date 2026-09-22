package bridge

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"omp-telegram/internal/config"
	"omp-telegram/internal/logging"
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
	log           *slog.Logger
	telegramLog   *slog.Logger
	storeLog      *slog.Logger
	mediaLog      *slog.Logger
	rpcLog        *slog.Logger
	slots         chan struct{}
	wg            sync.WaitGroup
	fatal         chan error
	mediaSlots    chan struct{}
	resumeSlots   chan struct{}
	sessionMu     sync.Mutex
	sessionClaims map[string]sessionClaim
	exportClaims  map[*worker]string
	bindingsEpoch atomic.Uint64
	ctx           context.Context
	workerExits   chan workerExit
}
type incoming struct {
	id       int64
	msg      *telegram.Message
	callback *telegram.CallbackQuery
}
type target struct{ chat, thread int64 }

type workerExit struct {
	key    target
	worker *worker
}

func supportedConversation(m *telegram.Message) bool {
	return m != nil && (m.MessageThreadID != 0 || m.Chat.Type == "private")
}

type queued struct {
	id          int64
	user        int64
	replyTo     int64
	text        string
	displayText string
	reply       *telegram.Message
	images      []media.Image
	albumKey    albumKey
	album       bool
	preparing   bool
	cancel      context.CancelFunc
	directory   string
}

type albumKey struct {
	group string
	user  int64
}

type pendingAlbum struct {
	key       albumKey
	ownerID   int64
	startedAt time.Time
	version   uint64
	messages  []telegram.Message
	caption   string
	reply     *telegram.Message
	timer     *time.Timer
}

type albumEvent struct {
	key     albumKey
	version uint64
}

type confirmation struct {
	action, uiID, method string
	workspace            string
	exportFormat         string
	options              []string
	expires              time.Time
	generation, active   int64
	epoch                uint64
	turn                 uint64
	user                 int64
	messageID            int64
	sessions             []omp.SessionSummary
	nativeSessions       []omp.SessionSummary
	pickerOptions        []resumePickerOption
	pinnedSessions       map[string]struct{}
	models               []omp.ModelRole
	bindings             []store.BindingListEntry
	deleteBot            int64
	deleteChat           int64
	deleteThread         int64
	deleteGeneration     int64
	page                 int
}
type runtimeState uint8

const (
	runtimeReleased runtimeState = iota
	runtimeStarting
	runtimeConnected
)

type worker struct {
	b                     *Bridge
	log                   *slog.Logger
	key                   target
	input                 chan incoming
	client                *omp.Client
	binding               store.Binding
	startIntent           *store.StartIntent
	sessionID             string
	claimedSession        string
	restoring             bool
	queue                 []queued
	stream                strings.Builder
	active, activeReplyTo int64
	owner                 int64
	turn                  uint64
	finishing             bool
	compacting            bool
	progress              progressState
	operations            chan operationResult
	albums                map[albumKey]*pendingAlbum
	albumEvents           chan albumEvent
	albumVersion          uint64
	albumSuppressed       map[albumKey]time.Time
	background            sync.WaitGroup
	busy                  bool
	awaitingContinuation  bool
	runtime               runtimeState
	runtimeResuming       bool
	lastActivity          time.Time
	lastLogicalActivity   time.Time

	lastTyping          time.Time
	typingCancel        context.CancelFunc
	idleProbe           idleProbeState
	preview             string
	lastAssistant       *terminalAssistant
	finalAssistantTexts []string
	lastPreview         string
	previewID           int64
	previewStopToken    string
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
	exportResults       chan exportResult
	exportCancel        context.CancelFunc
	exportRequest       uint64
	exportingSession    string
	bindingNameResults  chan bindingNamesResult
	bindingNameCancel   context.CancelFunc
	bindingNameRequest  uint64
	doctorResults       chan doctorResult
	doctorCancel        context.CancelFunc
	doctorRequest       uint64

	ctx           context.Context
	cancel        context.CancelFunc
	exitMu        sync.Mutex
	exitRequested bool
}

type terminalAssistant struct {
	StopReason                 string
	ErrorMessage               string
	ErrorClassificationMessage string
	ErrorStatus                int
}

type previewResult struct {
	id         int64
	taskID     int64
	created    bool
	text       string
	stopToken  string
	generation int64
	turn       uint64
	err        error
}

const (
	maxRecentTools       = 6
	maxActiveTools       = 6
	maxToolNameUnits     = 64
	maxPreviewUnits      = 2200
	maxProgressUnits     = 3500
	progressInitialDelay = 3 * time.Second
)

var logicalWorkerIdleTimeout = 5 * time.Minute

type progressState struct {
	StartedAt   time.Time
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
	Role                       string          `json:"role"`
	Content                    json.RawMessage `json:"content"`
	StopReason                 string          `json:"stopReason"`
	ErrorMessage               string          `json:"errorMessage"`
	ErrorClassificationMessage string          `json:"errorClassificationMessage"`
	ErrorStatus                int             `json:"errorStatus"`
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
	{Command: "queue", Description: "Show and cancel pending bridge tasks"},
	{Command: "resume", Description: "Choose or pin an omp session in this working directory"},
	{Command: "review", Description: "Run omp review: /review [arguments]"},
	{Command: "close", Description: "Close omp, keep workspace and session"},
	{Command: "export", Description: "Export an omp session: /export [html] [session-id]"},
	{Command: "bindings", Description: "List saved conversation/session bindings"},
	{Command: "status", Description: "Show session, model, context, speed and queue"},
	{Command: "doctor", Description: "Run safe diagnostics"},
	{Command: "name", Description: "Name the omp session: /name <title>"},
	{Command: "model", Description: "Choose a cycle role or /model provider/model"},
	{Command: "thinking", Description: "Choose the thinking level for this session"},
	{Command: "fast", Description: "Choose fast mode: /fast [on|off|status]"},
	{Command: "compact", Description: "Compact context after confirmation"},
	{Command: "handoff", Description: "Run native handoff: /handoff [instructions]"},
	{Command: "help", Description: "Show usage help"},
}

func Run(ctx context.Context, cfg config.Config, db *store.Store, logs *logging.Registry) error {
	if logs == nil {
		return errors.New("logging registry required")
	}
	log := logs.Logger(logging.Bridge)
	telegramLog := logs.Logger(logging.Telegram)
	storeLog := logs.Logger(logging.Store)
	mediaLog := logs.Logger(logging.Media)
	rpcLog := logs.Logger(logging.RPC)
	tg := telegram.New(cfg.Token, telegramLog)
	bot, err := tg.GetMe(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logTelegramFailure(telegramLog, slog.LevelError, "telegram_identity_failed", "telegram identity lookup failed", err)
		}
		return err
	}
	if err = db.CheckBot(bot.ID); err != nil {
		storeLog.Error("bot consistency check failed", "event", "bot_check_failed", "error_kind", "persistence")
		return err
	}
	// Command menus improve discoverability but are not required for polling.
	registered := 0
	for _, language := range []string{"", "zh"} {
		if err = tg.SetCommands(ctx, botCommands, language); err != nil {
			if ctx.Err() == nil {
				logTelegramFailure(telegramLog, slog.LevelWarn, "telegram_commands_failed", "telegram command registration failed", err, slog.String("language_code", language))
			}
			continue
		}
		registered++
	}
	if registered > 0 {
		telegramLog.Info("telegram command menu registered", "event", "command_menu_registered", "languages", registered)
	}

	ctx, cancel := context.WithCancel(ctx)
	b := &Bridge{cfg: cfg, db: db, tg: tg, bot: bot, log: log, telegramLog: telegramLog, storeLog: storeLog, mediaLog: mediaLog, rpcLog: rpcLog, slots: make(chan struct{}, cfg.MaxWorkers), fatal: make(chan error, 1), mediaSlots: make(chan struct{}, 2), resumeSlots: make(chan struct{}, 2), ctx: ctx, workerExits: make(chan workerExit)}
	defer func() { cancel(); b.wg.Wait() }()
	b.reconcileProgressCleanup(ctx)
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
	var pollBackoff time.Duration
	go func() {
		defer b.wg.Done()
		for ctx.Err() == nil {
			offset, err := db.Offset()
			if err != nil {
				b.storeLog.Error("database offset failed", "event", "inbox_read_failed", "reason", "offset", "error_kind", "persistence")
				b.fail(err)
				return
			}
			updates, err := tg.GetUpdates(ctx, offset)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				logTelegramFailure(b.telegramLog, slog.LevelWarn, "poll_failed", "telegram polling failed", err)
				pollBackoff = nextPollDelay(pollBackoff, telegram.ClassifyError(err).RetryAfter)
				if !waitPollBackoff(ctx, pollBackoff) {
					return
				}
				continue
			}
			pollBackoff = 0
			for _, u := range updates {
				raw, err := json.Marshal(u)
				if err == nil {
					err = db.Accept(u.UpdateID, raw)
				}
				if err != nil {
					b.storeLog.Error("update persistence failed", "event", "inbox_state_write_failed", "reason", "accept", "error_kind", "persistence")
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
			b.storeLog.Error("pending input read failed", "event", "inbox_read_failed", "reason", "pending", "error_kind", "persistence")
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
					b.storeLog.Error("input state persistence failed", "event", "inbox_state_write_failed")
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
					b.storeLog.Error("input state persistence failed", "event", "inbox_state_write_failed")
					return err
				}
				continue
			}
			if !supportedConversation(m) {
				if err = db.Enqueue(m.Chat.ID, 0, "This bot supports private chats and Telegram topics. In groups, use it inside a topic."); err != nil {
					b.storeLog.Error("unsupported conversation reply failed", "event", "outbox_write_failed")
					return err
				}
				if err = db.Mark(in.ID, "done"); err != nil {
					b.storeLog.Error("input state persistence failed", "event", "inbox_state_write_failed")
					return err
				}
				continue
			}
			key := target{m.Chat.ID, m.MessageThreadID}
			if blocked[key] {
				continue
			}
			w := workers[key]
			if w != nil && w.exitRequestedState() {
				blocked[key] = true
				continue
			}
			if w == nil {
				binding, bindingErr := b.bindingForWorker(key)
				if bindingErr != nil {
					b.storeLog.Error("session binding read failed", "event", "binding_read_failed", "reason", "worker", "error_kind", "persistence")
					return bindingErr
				}
				w = b.launchWorker(ctx, key, binding, false, nil)
				workers[key] = w
			}
			if !w.tryInput(incoming{in.ID, m, u.CallbackQuery}) {
				blocked[key] = true
				continue
			}
			delivered[in.ID] = true

		}
		select {
		case exit := <-b.workerExits:
			removeExitedWorker(workers, exit)
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
		if removeErr := removeOutboxSnapshot(filepath.Join(b.cfg.DataDir, "attachments", "outbox"), path); removeErr != nil {
			b.storeLog.Warn("database cleanup snapshot removal failed", "event", "snapshot_cleanup_failed")
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			b.storeLog.Warn("database cleanup failed", "event", "cleanup_failed")
		}
		return
	}
	b.reconcileOutboxSnapshots(ctx, time.Now().AddDate(0, 0, -b.cfg.DatabaseRetentionDays))
	if result.Inbox != 0 || result.Outbox != 0 {
		b.storeLog.Info("database cleanup completed", "event", "cleanup_completed", "inbox_count", result.Inbox, "outbox_count", result.Outbox)
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

func removeOutboxSnapshot(spoolRoot, path string) error {
	rel, err := filepath.Rel(spoolRoot, path)
	if err != nil || !filepath.IsLocal(rel) {
		return nil
	}
	root, err := os.OpenRoot(spoolRoot)
	if err != nil {
		return nil
	}
	defer root.Close()
	if err = root.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (b *Bridge) reconcileOutboxSnapshots(ctx context.Context, cutoff time.Time) {
	paths, err := b.db.OutboxAttachmentPaths(ctx)
	if err != nil {
		if ctx.Err() == nil {
			b.storeLog.Warn("database cleanup snapshot reconciliation failed", "event", "snapshot_cleanup_failed")
		}
		return
	}
	referenced := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		referenced[path] = struct{}{}
	}
	spoolRoot := filepath.Join(b.cfg.DataDir, "attachments", "outbox")
	root, err := os.OpenRoot(spoolRoot)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && ctx.Err() == nil {
			b.storeLog.Warn("database cleanup snapshot reconciliation failed", "event", "snapshot_cleanup_failed")
		}
		return
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		b.storeLog.Warn("database cleanup snapshot reconciliation failed", "event", "snapshot_cleanup_failed")
		return
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		b.storeLog.Warn("database cleanup snapshot reconciliation failed", "event", "snapshot_cleanup_failed")
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
			b.storeLog.Warn("database cleanup snapshot removal failed", "event", "snapshot_cleanup_failed")
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
			b.storeLog.Error("outbox read failed", "event", "outbox_read_failed")
			return e
		}
		if e = b.db.MarkOutput(o.ID, "sending"); e != nil {
			b.storeLog.Error("outbox state persistence failed", "event", "outbox_state_write_failed", "outbox_id", o.ID, "chat_id", o.Chat, "thread_id", o.Thread, "state", "sending")
			return e
		}
		switch o.Kind {
		case "text":
			_, e = b.tg.Send(ctx, o.Chat, o.Thread, o.Text, telegram.SendOptions{ReplyToMessageID: o.ReplyTo})
		case "photo", "document":
			_, e = b.tg.SendFile(ctx, o.Chat, o.Thread, o.Kind, o.Path, o.Name, o.Text, telegram.SendOptions{ReplyToMessageID: o.ReplyTo})
		default:
			b.storeLog.Error("unsupported outbox content kind", "event", "outbox_read_failed", "reason", "invalid_kind")
			return errors.New("unsupported outbox content kind")
		}
		state := store.OutboxDone
		if e != nil {
			info := telegram.ClassifyError(e)
			state = store.OutboxFailed
			if info.Uncertain {
				state = store.OutboxUncertain
			}
			if ctx.Err() == nil && info.Reason != "cancelled" {
				logTelegramFailure(b.telegramLog, slog.LevelWarn, "delivery_failed", "telegram delivery failed", e,
					slog.Int64("outbox_id", o.ID), slog.Int64("chat_id", o.Chat), slog.Int64("thread_id", o.Thread),
					slog.String("kind", o.Kind), slog.String("state", string(state)), slog.Bool("replay", false))
			}
		}
		if e = b.db.MarkOutput(o.ID, state); e != nil {
			b.storeLog.Error("outbox state persistence failed", "event", "outbox_state_write_failed", "outbox_id", o.ID, "chat_id", o.Chat, "thread_id", o.Thread, "state", state)
			return e
		}
		if state == store.OutboxDone && o.InboxID != 0 {
			b.cleanupDeliveredProgress(ctx, o.InboxID)
		}
		if o.Kind != "text" {
			if state == store.OutboxDone {
				if removeErr := os.Remove(o.Path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					b.storeLog.Warn("attachment cleanup failed", "event", "snapshot_cleanup_failed")
				}
			} else {
				if e = b.db.Enqueue(o.Chat, o.Thread, "Attachment delivery "+string(state)+". It will not be replayed automatically."); e != nil {
					b.storeLog.Error("delivery failure notice persistence failed", "event", "outbox_write_failed")
					return e
				}
			}
		}
	}
	return nil
}
func (b *Bridge) reconcileProgressCleanup(ctx context.Context) {
	messages, err := b.db.CompletedProgressMessages()
	if err != nil {
		b.storeLog.Warn("completed progress lookup failed", "event", "progress_cleanup_state_failed")
		return
	}
	for _, message := range messages {
		b.deleteProgressMessage(ctx, message)
	}
}

func (b *Bridge) cleanupDeliveredProgress(ctx context.Context, inboxID int64) {
	message, ready, err := b.db.CompletedProgressMessage(inboxID)
	if err != nil {
		b.storeLog.Warn("completed progress lookup failed", "event", "progress_cleanup_state_failed")
		return
	}
	if ready {
		b.deleteProgressMessage(ctx, message)
	}
}

func (b *Bridge) deleteProgressMessage(ctx context.Context, message store.ProgressMessage) {
	if message.MessageID == 0 || ctx.Err() != nil {
		return
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := b.tg.Delete(deleteCtx, message.Chat, message.MessageID)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			info := telegram.ClassifyError(err)
			permanent := !info.Uncertain && info.Code >= http.StatusBadRequest && info.Code < http.StatusInternalServerError && info.Code != http.StatusTooManyRequests
			event := "progress_cleanup_failed"
			if permanent {
				event = "progress_cleanup_abandoned"
			}
			logTelegramFailure(b.telegramLog, slog.LevelWarn, event, "progress message cleanup failed", err,
				slog.Int64("inbox_id", message.InboxID), slog.Int64("chat_id", message.Chat), slog.Int64("message_id", message.MessageID))
			if permanent {
				if err := b.db.ClearProgressMessage(message.InboxID, message.MessageID); err != nil {
					b.storeLog.Warn("progress cleanup state persistence failed", "event", "progress_cleanup_state_failed", "inbox_id", message.InboxID)
				}
			}
		}
		return
	}
	if err := b.db.ClearProgressMessage(message.InboxID, message.MessageID); err != nil {
		b.storeLog.Warn("progress cleanup state persistence failed", "event", "progress_cleanup_state_failed", "inbox_id", message.InboxID)
	}
}

func (b *Bridge) fail(err error) {
	select {
	case b.fatal <- err:
	default:
	}
}
func (w *worker) say(s string) {
	for _, part := range telegram.SplitForTelegram(s, telegram.MaxMessageUTF16) {
		if e := w.b.db.Enqueue(w.key.chat, w.key.thread, part); e != nil {
			w.b.storeLog.Error("outbox write failed", "event", "outbox_write_failed")
			w.b.fail(e)
			w.cancel()
			return
		}
	}
}
func (w *worker) mark(id int64, state store.InboxState) bool {
	if id == 0 {
		return true
	}
	if e := w.b.db.Mark(id, state); e != nil {
		w.b.storeLog.Error("input state persistence failed", "event", "inbox_state_write_failed")
		w.b.fail(e)
		w.cancel()
		return false
	}
	return true
}

func (w *worker) touchLogicalActivity() {
	w.lastLogicalActivity = time.Now()
}

func (w *worker) touchActivity() {
	w.touchLogicalActivity()
	w.resetIdleProbe()
	if w.runtime == runtimeConnected && w.client != nil {
		w.lastActivity = time.Now()
	}
}

func (w *worker) markExitRequested() {
	w.exitMu.Lock()
	w.exitRequested = true
	w.exitMu.Unlock()
}

func (w *worker) exitRequestedState() bool {
	w.exitMu.Lock()
	defer w.exitMu.Unlock()
	return w.exitRequested
}

func (w *worker) tryInput(in incoming) bool {
	w.exitMu.Lock()
	defer w.exitMu.Unlock()
	if w.exitRequested {
		return false
	}
	select {
	case w.input <- in:
		return true
	default:
		return false
	}
}

func (w *worker) sessionControlBusy() bool {
	return w.busy || w.compacting || w.finishing || w.exportingSession != ""
}

func (w *worker) hasRuntimeConfirmation() bool {
	for _, c := range w.confirms {
		switch c.action {
		case "model", "thinking", "fast", "compact", "ui", "new":
			return true
		}
	}
	return false
}

func (w *worker) idleEligible() bool {
	return w.runtime == runtimeConnected && w.client != nil && w.active == 0 && !w.busy && !w.compacting && !w.finishing && !w.previewBusy && len(w.queue) == 0 && len(w.hostRequests) == 0 && w.resumeCancel == nil && w.exportCancel == nil && w.startIntent == nil && !w.hasRuntimeConfirmation()
}

func (w *worker) sessionDurable() bool {
	path := w.binding.Session
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func (w *worker) releaseIdleRuntime(now time.Time) {
	if w.b.cfg.IdleTimeout <= 0 || !w.idleEligible() || w.lastActivity.IsZero() || now.Sub(w.lastActivity) < w.b.cfg.IdleTimeout || !w.sessionDurable() {
		return
	}
	w.releaseRuntimeWithReason(true, "idle")
}

func (w *worker) logicalIdleEligibleLocked() bool {
	return !w.binding.Running && w.runtime == runtimeReleased && w.client == nil && !w.restoring && !w.runtimeResuming && w.active == 0 && !w.busy && !w.compacting && !w.finishing && !w.previewBusy && !w.awaitingContinuation && len(w.input) == 0 && len(w.queue) == 0 && len(w.albums) == 0 && len(w.albumSuppressed) == 0 && len(w.hostRequests) == 0 && w.resumeCancel == nil && w.exportCancel == nil && w.exportingSession == "" && w.bindingNameCancel == nil && w.doctorCancel == nil && w.startIntent == nil && len(w.confirms) == 0 && w.idleProbe.cancel == nil && w.typingCancel == nil && len(w.operations) == 0 && len(w.previewResult) == 0 && len(w.mediaResults) == 0 && len(w.sendResults) == 0 && len(w.resumeResults) == 0 && len(w.exportResults) == 0 && len(w.bindingNameResults) == 0 && len(w.doctorResults) == 0 && len(w.idleProbe.results) == 0 && len(w.albumEvents) == 0 && len(w.keyboardCleanup) == 0
}

func (w *worker) evictIfIdle(now time.Time) bool {
	if logicalWorkerIdleTimeout <= 0 || w.lastLogicalActivity.IsZero() || now.Sub(w.lastLogicalActivity) < logicalWorkerIdleTimeout {
		return false
	}
	w.exitMu.Lock()
	defer w.exitMu.Unlock()
	if w.exitRequested || !w.logicalIdleEligibleLocked() {
		return false
	}
	w.exitRequested = true
	w.cancel()
	return true
}

func (w *worker) runtimeClient() (*omp.Client, bool) {
	return w.client, w.runtime == runtimeConnected && w.client != nil
}

func (w *worker) releaseRuntimeWithReason(releaseSlot bool, reason string) {
	w.stopTyping()
	w.resetIdleProbe()
	w.awaitingContinuation = false
	sessionID := w.sessionID
	generation := w.binding.Generation
	// Disable the old client's event and Done channels before closing it, so an
	// intentional release cannot be mistaken for an unexpected exit.
	if client := w.client; client != nil {
		clientID := client.ID()
		w.client = nil
		w.runtime = runtimeReleased
		client.Close()
		if releaseSlot {
			<-w.b.slots
		}
		attrs := []slog.Attr{
			slog.String("event", "runtime_release"),
			slog.Int64("generation", generation),
			slog.String("reason", reason),
			slog.Uint64("client_id", clientID),
		}
		if validSessionID(sessionID) {
			attrs = append(attrs, slog.String("session_id", sessionID))
		}
		w.log.LogAttrs(context.Background(), slog.LevelInfo, "runtime released", attrs...)
	}
	w.runtime = runtimeReleased
	w.previewID = 0
	w.previewStopToken = ""
}

func (w *worker) releaseRuntime(releaseSlot bool) {
	w.releaseRuntimeWithReason(releaseSlot, "explicit")
}

func (w *worker) closeRuntime() { w.releaseRuntimeWithReason(true, "explicit") }

func (w *worker) closeFailedStart() {
	if w.runtimeResuming || w.restoring {
		w.releaseRuntimeWithReason(true, "failure")
		return
	}
	w.closeLogicalSession()
}

func (w *worker) ensureRuntime() (*omp.Client, error) {
	if client, ok := w.runtimeClient(); ok {
		return client, nil
	}
	if w.runtime == runtimeStarting {
		return nil, errors.New("OMP is starting. Try again after it is ready.")
	}
	if !w.binding.Running || w.binding.Session == "" || !filepath.IsAbs(w.binding.Session) || !filepath.IsAbs(w.binding.Workspace) {
		return nil, errors.New("No instance is running in this conversation. Start with /new <name or project path>, or use /resume for a saved session.")
	}
	w.runtimeResuming, w.restoring = true, true
	w.start(true, w.binding.Session, w.binding.Workspace, false)
	w.restoring, w.runtimeResuming = false, false
	if client, ok := w.runtimeClient(); ok {
		w.logRuntimeEvent(slog.LevelInfo, "runtime_resume", "lazy", "runtime resumed", w.sessionID)
		return client, nil
	}
	if w.ctx.Err() == nil {
		w.logRuntimeEvent(slog.LevelWarn, "restore_runtime_failed", "lazy", "runtime restore failed", w.sessionID)
	}
	return nil, errors.New("Failed to resume the released OMP session. No prompt was submitted.")
}

func (w *worker) submit(q queued) bool {
	if q.id == 0 {
		return true
	}
	if err := w.b.db.Submit(q.id, q.replyTo); err != nil {
		w.b.storeLog.Error("task submission persistence failed", "event", "inbox_state_write_failed")
		w.b.fail(err)
		w.cancel()
		return false
	}
	return true
}

func (w *worker) logTaskSubmit(inboxID int64) {
	attrs := []slog.Attr{
		slog.String("event", "task_submit"),
		slog.Int64("inbox_id", inboxID),
		slog.Int64("generation", w.binding.Generation),
		slog.Uint64("turn", w.turn),
	}
	if w.client != nil {
		attrs = append(attrs, slog.Uint64("client_id", w.client.ID()))
	}
	if validSessionID(w.sessionID) {
		attrs = append(attrs, slog.String("session_id", w.sessionID))
	}
	w.log.LogAttrs(context.Background(), slog.LevelInfo, "task submitted", attrs...)
}
func (w *worker) run() {
	w.touchLogicalActivity()
	w.log.Info("worker started", "event", "worker_start")
	w.initMedia()
	w.initResumePicker()
	defer func() { w.cancel(); w.background.Wait(); w.drainMediaResults(); w.drainExportResults() }()
	defer func() {
		w.teardownWorker(true)
		w.log.Info("worker stopped", "event", "worker_stop")
	}()
	if w.startIntent != nil {
		w.say("A requested " + w.startIntent.Kind + " session start was interrupted before omp identity was saved. The outcome is uncertain. Use /close to cancel it, then use /new or /resume explicitly.")
	} else if w.restoring {
		w.start(true, w.binding.Session, w.binding.Workspace, false)
		w.restoring = false
		if _, connected := w.runtimeClient(); !connected && w.ctx.Err() == nil && w.binding.Running {
			w.logRuntimeEvent(slog.LevelWarn, "restore_runtime_failed", "resume", "runtime restore failed", w.sessionID)
		}
	}
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	for {
		var events <-chan json.RawMessage
		var done <-chan struct{}
		if client, ok := w.runtimeClient(); ok {
			events = client.Events()
			done = client.Done()
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
			if result.generation == w.binding.Generation {
				if _, ok := w.runtimeClient(); ok {
					w.touchActivity()
					w.compacting = false
					w.busy = false
					if result.kind == "handoff" {
						switch {
						case result.err != nil:
							w.say("Handoff failed or its outcome is uncertain. It will not be replayed automatically.")
						case result.cancelled:
							w.say("Handoff canceled without a result.")
						default:
							w.touchBinding()
							w.say("Handoff completed.")
						}
					} else if result.err != nil {
						w.say("Compaction failed.")
					} else {
						w.touchBinding()
						w.say("Compaction completed.")
					}
				}
			}
		case event := <-w.albumEvents:
			w.sealAlbum(event)
		case result := <-w.mediaResults:
			w.preparedMedia(result)
		case result := <-w.sendResults:
			w.preparedSend(result)
		case result := <-w.resumeResults:
			w.resumeListed(result)
		case result := <-w.exportResults:
			w.exportFinished(result)
		case result := <-w.bindingNameResults:
			w.bindingNamesLoaded(result)
		case result := <-w.doctorResults:
			w.doctorFinished(result)
		case result := <-w.idleProbe.results:
			w.idleProbeFinished(result, time.Now())
		case <-tick.C:
			now := time.Now()
			w.typing()
			w.flushPreview()
			w.expire()
			w.probeStuckTask(now)
			w.releaseIdleRuntime(now)
			if w.evictIfIdle(now) {
				return
			}
		}
		w.dispatch()
	}
}

func (w *worker) teardownWorker(releaseSlot bool) {
	completion := w.taskCompletionAttrs(w.active, "uncertain")
	w.stopTyping()
	w.resetIdleProbe()
	w.cancelResumeList()
	w.cancelBindingNameLookup()
	if w.doctorCancel != nil {
		w.doctorCancel()
		w.doctorCancel = nil
	}

	w.clearQueue()
	w.clearAlbums()
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
	w.activeReplyTo = 0
	w.finalAssistantTexts = nil
	w.lastAssistant = nil
	w.stream.Reset()
	w.clearConfirmations()
	w.releaseRuntime(releaseSlot)
	w.releaseSession()
	if activeID := w.active; activeID != 0 {
		if w.mark(activeID, "uncertain") {
			w.log.LogAttrs(context.Background(), slog.LevelInfo, "task completed", completion...)
		}
		if w.binding.Running && w.ctx.Err() != nil {
			if err := w.b.db.SetInterrupted(w.binding, true); err != nil {
				w.b.storeLog.Error("interrupted state persistence failed", "event", "binding_write_failed", "reason", "set_interrupted", "error_kind", "persistence")
				w.b.fail(err)
			} else {
				w.binding.Interrupted = true
			}
		}
	}
}

func (w *worker) closeLogicalSession() bool {
	if !w.cancelStart() || !w.persistClosed() {
		return false
	}
	sessionID := w.sessionID
	generation := w.binding.Generation
	clientID := uint64(0)
	if w.client != nil {
		clientID = w.client.ID()
	}
	w.clearQueue()
	w.teardownWorker(true)
	w.active = 0
	w.activeReplyTo = 0
	w.busy = false
	attrs := []slog.Attr{
		slog.String("event", "session_close"),
		slog.Int64("generation", generation),
		slog.String("reason", "explicit"),
	}
	if validSessionID(sessionID) {
		attrs = append(attrs, slog.String("session_id", sessionID))
	}
	if clientID != 0 {
		attrs = append(attrs, slog.Uint64("client_id", clientID))
	}
	w.log.LogAttrs(context.Background(), slog.LevelInfo, "session closed", attrs...)
	return true
}

func (w *worker) failed() {
	if w.runtime == runtimeReleased && w.client == nil {
		return
	}
	if w.client != nil {
		w.logRuntimeEvent(slog.LevelWarn, "runtime_exit", "failure", "runtime exited", w.sessionID)
	}
	w.say("omp exited. The task outcome is uncertain and will not be replayed automatically. Use /resume to restore the session.")
	w.closeLogicalSession()
}
func (w *worker) cancelQueuedTask(id int64) bool {
	for i := range w.queue {
		if w.queue[i].id != id {
			continue
		}
		q := w.queue[i]
		if q.album {
			if q.preparing && q.cancel == nil {
				count := 1
				if album, ok := w.albums[q.albumKey]; ok {
					count = len(album.messages)
				}
				w.log.Info("album preparation finished", "event", "album_prepare", "inbox_id", q.id, "count", count, "result", "cancelled")
			}
			w.suppressAlbum(q.albumKey)
			w.cancelAlbum(q.id)
		}
		if q.cancel != nil {
			q.cancel()
		}
		removeIncoming(w.binding.Workspace, q.directory, w.mediaTaskLogger(q.id))
		w.queue = append(w.queue[:i], w.queue[i+1:]...)
		if w.mark(q.id, "cancelled") {
			w.logQueuedTaskComplete(q.id, "cancelled")
		}
		return true
	}
	return false
}

func (w *worker) clearQueue() {
	for len(w.queue) > 0 {
		w.cancelQueuedTask(w.queue[0].id)
	}
}

func (w *worker) stop() {
	w.clearQueue()
	w.requestAbort("Abort requested and queued prompts cleared.")
}

func (w *worker) stopActiveTask() {
	w.requestAbort("Abort requested for the current task. Queued prompts will continue.")
}

func (w *worker) requestAbort(notice string) {
	if w.runtime == runtimeReleased {
		w.say("No active task. Queued prompts cleared.")
		return
	}
	if _, err := w.call("abort", nil); err != nil {
		w.say("The abort request failed.")
		return
	}
	w.touchBinding()
	w.say(notice)
}

func (w *worker) call(kind string, fields map[string]any) (json.RawMessage, error) {
	client, err := w.ensureRuntime()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	defer cancel()
	raw, err := client.Call(ctx, kind, fields)
	if err != nil {
		// Invalidate watchdog evidence without extending the runtime idle lifetime.
		w.resetIdleProbe()
		if w.log.Enabled(context.Background(), slog.LevelDebug) {
			attrs := []slog.Attr{
				slog.String("event", "request_failed"),
				slog.String("phase", "handled"),
				slog.Uint64("client_id", client.ID()),
				slog.Int64("generation", w.binding.Generation),
				slog.Uint64("turn", w.turn),
				slog.String("error_kind", omp.ClassifyError(err)),
			}
			if w.active != 0 {
				attrs = append(attrs, slog.Int64("inbox_id", w.active))
			}
			w.log.LogAttrs(context.Background(), slog.LevelDebug, "rpc request failed", attrs...)
		}
		return raw, err
	}
	w.touchActivity()
	return raw, nil
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
	w.startInternal(resume, target, expectedCWD, replace, 0, false)
}

func (w *worker) startFenced(resume bool, target, expectedCWD string, replace bool, generation int64) {
	w.startInternal(resume, target, expectedCWD, replace, generation, true)
}

func (w *worker) renameNewTopic(workspace string) {
	if w.key.thread == 0 || w.b.tg == nil {
		return
	}
	name := clipUTF16(menuText(filepath.Base(filepath.Clean(workspace)), 128), 128)
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	if err := w.b.tg.EditForumTopic(ctx, w.key.chat, w.key.thread, name); err != nil {
		info := telegram.ClassifyError(err)
		w.log.Warn("new topic rename failed", "event", "topic_rename_failed", "error_kind", info.Reason)
		w.say("The session started, but the Telegram topic title could not be updated.")
	}
}

func (w *worker) startInternal(resume bool, target, expectedCWD string, replace bool, expectedGeneration int64, fenced bool) {
	if _, connected := w.runtimeClient(); connected && !replace {
		w.say("An instance is already running. Use /new for a fresh session, or /close before resuming another session.")
		return
	}
	if w.runtime == runtimeStarting {
		w.say("OMP is already starting.")
		return
	}
	if w.startIntent != nil && !w.restoring {
		w.say("A previous session start is still uncertain. Use /close before starting another session.")
		return
	}
	if w.exportCancel != nil && !w.restoring {
		w.say("The omp session operation is still loading. Wait for the export to finish.")
		return
	}
	old, e := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		w.say("Failed to read the session.")
		return
	}
	oldMissing := errors.Is(e, sql.ErrNoRows)
	renameTopic := !resume && !w.restoring && w.key.thread != 0 && oldMissing
	if oldMissing && !w.restoring {
		w.cancelResumeList()
		w.clearConfirmations()
	}
	previousGeneration := old.Generation
	if fenced {
		previousGeneration = expectedGeneration
	}
	nextGeneration := previousGeneration + 1
	var cwd, session string

	if resume {
		if w.restoring {
			info, err := os.Stat(target)
			workspaceInfo, workspaceErr := os.Stat(expectedCWD)
			if !filepath.IsAbs(target) || err != nil || !info.Mode().IsRegular() || !filepath.IsAbs(expectedCWD) || workspaceErr != nil || !workspaceInfo.IsDir() {
				if !w.runtimeResuming {
					sessionID := w.sessionID
					if w.persistClosed() {
						w.releaseSession()
						w.logRuntimeEvent(slog.LevelInfo, "restore_runtime_skipped", "session_file_unavailable", "startup restore skipped", sessionID)
						w.say("The saved omp session file or working directory is unavailable, so startup restore was skipped. Use /new to start a new session.")
					}
					return
				}
				w.say("Cannot restore the saved omp session. Its session file or working directory is unavailable. No replacement session was created; use /resume to select a session.")
				return
			}
			cwd = expectedCWD
		} else if !validSessionID(target) {
			w.say("Usage: /resume, or /resume <omp session ID>.")
			return
		}
		if !w.runtimeResuming && w.b.sessionInUseByOther(w, target) {
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
	reuseSlot := replace && w.runtime == runtimeConnected && w.client != nil
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
		intent := store.StartIntent{Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread, Workspace: intentWorkspace, Session: session, Generation: nextGeneration, Kind: "new"}
		if resume {
			intent.Kind = "resume"
		}
		previous := old
		if fenced {
			previous.Generation = previousGeneration
			if oldMissing || old.Generation != previousGeneration {
				previous.Running = false
			}
		}
		if e = w.b.db.PrepareStart(previous, intent); e != nil {
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
			w.teardownWorker(false)
			if reuseSlot {
				reserved = true
			}
			w.active = 0
			w.busy = false
		}
	}
	w.runtime = runtimeStarting
	c, e := omp.Start(w.ctx, omp.Config{Binary: w.b.cfg.OMP, CWD: cwd, Resume: session, Args: w.b.cfg.OMPArgs}, w.b.rpcLog)
	if e != nil {
		if reserved {
			<-w.b.slots
		}
		w.runtime = runtimeReleased
		if w.runtimeResuming {
			w.say("Failed to resume the released OMP session. No prompt was submitted.")
		} else if resume {
			w.say("Failed to resume omp. The startup outcome is uncertain; use /close before retrying.")
		} else {
			w.say("Failed to start omp. The startup outcome is uncertain; use /close before retrying.")
		}
		return
	}
	w.client = c
	w.runtime = runtimeConnected
	w.initMedia()
	ctx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
	info, e := c.SessionInfo(ctx)
	cancel()
	if e != nil {
		w.logRuntimeEvent(slog.LevelError, "runtime_identity_invalid", "failure", "runtime session identity unavailable", "")
		w.closeFailedStart()
		w.say("Cannot obtain omp session identity and working directory. The instance has been closed.")
		return
	}
	if resume && !w.restoring && !strings.HasPrefix(strings.ToLower(info.ID), strings.ToLower(target)) {
		w.closeFailedStart()
		w.say("The resumed session did not match the requested omp session ID.")
		return
	}
	if w.restoring && !sameSessionFile(info.File, target) {
		w.closeFailedStart()
		w.say("omp did not restore the saved session file. The instance has been closed.")
		return
	}
	if !resume {
		expectedCWD = cwd
	}
	if expectedCWD != "" && !sameWorkspace(info.CWD, expectedCWD) {
		w.closeFailedStart()
		w.say("omp selected a different working directory. The instance has been closed.")
		return
	}
	if !w.claimSession(info.File, info.ID) {
		w.closeFailedStart()
		w.say("This omp session is already active in another conversation.")
		return
	}
	if _, e = w.call("set_host_tools", map[string]any{"tools": telegramSendTools}); e != nil {
		w.closeFailedStart()
		w.say("Cannot register Telegram attachment delivery. The instance has been closed.")
		return
	}
	interrupted := w.restoring && !w.runtimeResuming && old.Interrupted
	binding := store.Binding{Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread, Workspace: info.CWD, Session: info.File, SessionID: info.ID, Generation: nextGeneration, LastUsedAt: old.LastUsedAt, Running: true}

	if w.runtimeResuming {
		binding = old
	} else if w.restoring {
		e = w.b.db.Save(binding)
	} else {
		e = w.b.db.CommitStart(binding)
	}
	if e != nil {
		w.b.storeLog.Error("session binding persistence failed", "event", "binding_write_failed", "reason", "commit_start", "error_kind", "persistence")
		w.closeFailedStart()
		w.say("Failed to save the session binding. The instance has been closed.")
		return
	}
	w.binding = binding
	w.startIntent = nil
	w.preview, w.lastPreview = "", ""
	w.previewID = 0
	w.runtime = runtimeConnected
	w.touchActivity()
	if !w.runtimeResuming && !w.restoring {
		w.touchBinding()
	}
	if !w.runtimeResuming {
		if replace {
			w.logSessionEvent("session_replace", "replace", "session replaced", info.ID)
		} else if resume {
			w.logSessionEvent("session_resume", "resume", "session resumed", info.ID)
		} else {
			w.logSessionEvent("session_new", "new", "session created", info.ID)
		}
	}
	runtimeReason := "new"
	if resume {
		runtimeReason = "resume"
	}
	if w.runtimeResuming {
		runtimeReason = "lazy"
	}
	w.logRuntimeEvent(slog.LevelInfo, "runtime_connected", runtimeReason, "runtime connected", info.ID)
	if renameTopic {
		w.renameNewTopic(info.CWD)
	}
	if w.runtimeResuming {
		return
	}
	ready := "omp is ready.\nWorkspace: " + info.CWD + "\nSession: " + info.ID
	if interrupted {
		ready += "\n\n⚠️ Gateway restarted while the previous task was active. It was interrupted and was not resubmitted."
	}
	w.say(ready)
}
func (w *worker) handle(in incoming) {
	w.touchLogicalActivity()
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
		if in.msg.MediaGroupID != "" {
			w.collectAlbum(in)
			return
		}
		if _, err := w.ensureRuntime(); err != nil {
			w.say(err.Error())
			w.mark(in.id, "done")
			return
		}
		if len(w.queue) >= w.b.cfg.QueueCapacity {
			w.say("The queue is full. This message was not submitted.")
			if w.mark(in.id, "cancelled") {
				w.log.Warn("task queue rejected", "event", "queue_rejected", "inbox_id", in.id, "reason", "worker_queue_full")
			}
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
		w.enqueueReviewPrompt(in, prompt)
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
		if w.binding.Running {
			w.confirm(confirmation{action: "new", workspace: workspace, user: in.msg.From.ID}, "Start a new omp session in: "+workspace+"?\nExisting files and session history will be preserved.", []string{"Confirm", "Cancel"})
		} else {
			w.start(false, workspace, "", false)
		}
	case "/resume":
		if arg == "" {
			w.requestResumeList(in.msg.From.ID)
		} else {
			w.start(true, arg, "", true)
		}
	case "/export":
		format, sessionID, ok := parseExportArgs(arg)
		if !ok {
			w.say("Usage: /export [html] [session-id].")
			return
		}
		if sessionID == "" {
			w.requestExportList(in.msg.From.ID, format)
		} else {
			w.requestDirectExport(in.msg.From.ID, format, sessionID)
		}
	case "/bindings":
		if arg != "" {
			w.say("Usage: /bindings")
			return
		}
		w.showBindings(in.msg.From.ID)
	case "/close":
		if !w.closeLogicalSession() {
			return
		}
		w.say("The instance is closed. The session has been preserved.")
	case "/stop":
		w.stop()
	case "/queue":
		if arg != "" {
			w.say("Usage: /queue")
			return
		}
		w.showQueue(in.msg.From.ID)
	case "/status":
		w.status()
	case "/doctor":
		if arg != "" {
			w.say("Usage: /doctor")
			return
		}
		w.runDoctor()
	case "/name":
		if arg == "" {
			w.say("Usage: /name <session title>")
			return
		}
		if w.exportingSession != "" {
			w.say("Wait for the current session export to finish before changing its name.")
			return
		}
		if _, err := w.ensureRuntime(); err != nil {
			w.say(err.Error())
			return
		}
		if _, err := w.call("set_session_name", map[string]any{"name": arg}); err != nil {
			w.say("The session name change could not be confirmed. Use /status to check before retrying.")
			return
		}
		w.touchBinding()
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
		if w.sessionControlBusy() {
			w.say("An idle instance is required.")
			return
		}
		if _, err := w.ensureRuntime(); err != nil {
			w.say("An idle instance is required.")
			return
		}
		w.confirm(confirmation{action: "compact", user: in.msg.From.ID}, "Compact the current session?", []string{"Confirm", "Cancel"})
	default:
		w.say("Unsupported command. Use /help.")
	}
}
func (w *worker) handoff(instructions string) {
	if w.sessionControlBusy() || len(w.queue) != 0 {
		w.say("Wait for the current task and queue to finish before handoff.")
		return
	}
	client, err := w.ensureRuntime()
	if err != nil {
		w.say(err.Error())
		return
	}
	var fields map[string]any
	if instructions != "" {
		fields = map[string]any{"customInstructions": instructions}
	}
	w.busy, w.compacting = true, true
	w.touchActivity()
	generation := w.binding.Generation
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
	home, _ := os.UserHomeDir()
	if w.runtime == runtimeReleased && w.binding.Running {
		idle := time.Duration(-1)
		if !w.lastActivity.IsZero() {
			idle = time.Since(w.lastActivity)
		}
		w.say(formatReleasedStatus(w.binding.Workspace, w.sessionID, home, len(w.queue), idle))
		return
	}
	if _, connected := w.runtimeClient(); !connected {
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
	w.say(formatRuntimeStatus(s, w.binding.Workspace, w.sessionID, home, len(w.queue), time.Since(w.lastActivity)))
}
func (w *worker) dispatch() {
	if w.busy || w.compacting || w.finishing || w.exportingSession != "" || len(w.queue) == 0 {
		return
	}
	if _, err := w.ensureRuntime(); err != nil || w.queue[0].preparing {
		return
	}
	q := w.queue[0]
	w.queue = w.queue[1:]
	if !w.submit(q) {
		return
	}
	w.active = q.id
	w.activeReplyTo = q.replyTo
	w.owner = q.user
	w.stopTyping()
	w.resetIdleProbe()
	w.awaitingContinuation = false
	w.turn++
	w.busy = true
	w.progress = progressState{StartedAt: time.Now(), ActiveTools: make(map[string]progressTool)}
	w.logTaskSubmit(q.id)
	w.progressSuppressed = false
	w.preview = ""
	w.stream.Reset()
	w.lastPreview = ""
	w.previewID = 0
	w.previewStopToken = ""
	fields := map[string]any{"message": q.text}
	if len(q.images) > 0 {
		fields["images"] = q.images
	}
	raw, err := w.call("prompt", fields)
	if err != nil {
		w.say("Submission failed or its outcome is uncertain. It will not be replayed automatically.")
		w.closeLogicalSession()
		return
	}
	w.touchBinding()
	var response struct {
		AgentInvoked *bool `json:"agentInvoked"`
	}
	if json.Unmarshal(raw, &response) == nil && response.AgentInvoked != nil && !*response.AgentInvoked {
		w.finishTerminal(rpcEvent{})
	}
}

func (w *worker) enqueuePrompt(in incoming, text string) {
	w.enqueuePreparedPrompt(in, preparePromptText(in.msg, text), text)
}

func (w *worker) enqueueReviewPrompt(in incoming, review string) {
	w.enqueuePreparedPrompt(in, prepareReviewPrompt(in.msg, review), review)
}

func (w *worker) enqueuePreparedPrompt(in incoming, prompt, displayText string) {
	if _, err := w.ensureRuntime(); err != nil {
		w.say(err.Error())
		w.mark(in.id, "done")
		return
	}
	if len(w.queue) >= w.b.cfg.QueueCapacity {
		w.say("The queue is full. This message was not submitted.")
		if w.mark(in.id, "cancelled") {
			w.log.Warn("task queue rejected", "event", "queue_rejected", "inbox_id", in.id, "reason", "worker_queue_full")
		}
		return
	}
	w.queue = append(w.queue, queued{id: in.id, user: in.msg.From.ID, replyTo: in.msg.MessageID, text: prompt, displayText: displayText})
}
func (w *worker) logQueuedTaskComplete(inboxID int64, result string) {
	if inboxID == 0 {
		return
	}
	attrs := []slog.Attr{
		slog.String("event", "task_complete"),
		slog.Int64("inbox_id", inboxID),
		slog.String("result", result),
		slog.Int64("generation", w.binding.Generation),
	}
	if validSessionID(w.sessionID) {
		attrs = append(attrs, slog.String("session_id", w.sessionID))
	}
	w.log.LogAttrs(context.Background(), slog.LevelInfo, "task completed", attrs...)
}

func (w *worker) taskCompletionAttrs(inboxID int64, result string) []slog.Attr {
	if inboxID == 0 {
		return nil
	}
	attrs := []slog.Attr{
		slog.String("event", "task_complete"),
		slog.Int64("inbox_id", inboxID),
		slog.String("result", result),
		slog.Int64("generation", w.binding.Generation),
		slog.Uint64("turn", w.turn),
	}
	if w.client != nil {
		attrs = append(attrs, slog.Uint64("client_id", w.client.ID()))
	}
	if started := w.progress.StartedAt; !started.IsZero() {
		duration := time.Since(started).Milliseconds()
		if duration < 0 {
			duration = 0
		}
		attrs = append(attrs, slog.Int64("duration_ms", duration))
	}
	if validSessionID(w.sessionID) {
		attrs = append(attrs, slog.String("session_id", w.sessionID))
	}
	return attrs
}

func (w *worker) logTaskComplete(inboxID int64, result string) {
	if attrs := w.taskCompletionAttrs(inboxID, result); len(attrs) != 0 {
		w.log.LogAttrs(context.Background(), slog.LevelInfo, "task completed", attrs...)
	}
}

func (w *worker) lifecycleAttrs(event, reason, sessionID string) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("event", event),
		slog.Int64("generation", w.binding.Generation),
		slog.String("reason", reason),
	}
	if validSessionID(sessionID) {
		attrs = append(attrs, slog.String("session_id", sessionID))
	}
	if w.client != nil {
		attrs = append(attrs, slog.Uint64("client_id", w.client.ID()))
	}
	return attrs
}

func (w *worker) logSessionEvent(event, reason, message, sessionID string) {
	w.log.LogAttrs(context.Background(), slog.LevelInfo, message, w.lifecycleAttrs(event, reason, sessionID)...)
}

func (w *worker) logRuntimeEvent(level slog.Level, event, reason, message, sessionID string) {
	w.log.LogAttrs(context.Background(), level, message, w.lifecycleAttrs(event, reason, sessionID)...)
}

func (w *worker) finish() {
	w.stopTyping()
	w.resetIdleProbe()
	w.awaitingContinuation = false
	// Progress state is finalized only after the durable result transaction.
	if w.active != 0 {
		taskID := w.active
		text := w.preview
		if err := w.b.db.CompleteInboxWithReplies(w.ctx, taskID, w.key.chat, w.key.thread, telegram.SplitForTelegram(text, telegram.MaxMessageUTF16)); err != nil {
			if w.ctx.Err() == nil {
				w.b.storeLog.Error("final result commit failed", "event", "final_commit_failed")
				w.b.fail(err)
				w.cancel()
			}
			return
		}
		w.logTaskComplete(taskID, "done")
		w.active = 0
		w.activeReplyTo = 0
		w.busy = false
		w.preview = ""
		w.stream.Reset()
		w.clearTaskConfirmations()
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

func (w *worker) finishUncertain(notice string) bool {
	return w.finishIncomplete("uncertain", notice)
}

func (w *worker) finishCancelled(notice string) bool {
	return w.finishIncomplete("cancelled", notice)
}

func (w *worker) finishIncomplete(state, notice string) bool {
	w.stopTyping()
	w.resetIdleProbe()
	w.awaitingContinuation = false
	if w.active == 0 {
		w.lastAssistant = nil
		w.finalAssistantTexts = nil
		return false
	}
	taskID := w.active
	text := w.preview
	if text != "" {
		text += "\n\n"
	}
	text += notice
	var err error
	switch state {
	case "uncertain":
		err = w.b.db.CompleteInboxUncertainWithReplies(w.ctx, taskID, w.key.chat, w.key.thread, telegram.SplitForTelegram(text, telegram.MaxMessageUTF16))
	case "cancelled":
		err = w.b.db.CompleteInboxCancelledWithReplies(w.ctx, taskID, w.key.chat, w.key.thread, telegram.SplitForTelegram(text, telegram.MaxMessageUTF16))
	default:
		panic("invalid terminal state")
	}
	if err != nil {
		if w.ctx.Err() == nil {
			w.b.storeLog.Error("incomplete result commit failed", "event", "final_commit_failed")
			w.b.fail(err)
			w.cancel()
		}
		return false
	}
	w.logTaskComplete(taskID, state)
	w.active = 0
	w.activeReplyTo = 0
	w.busy = false
	w.preview = ""
	w.lastAssistant = nil
	w.finalAssistantTexts = nil
	w.stream.Reset()
	w.clearTaskConfirmations()
	if w.previewBusy {
		w.finishing = true
		return true
	}
	w.finishPreview()
	return true
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
	if m.ErrorMessage != "" || m.ErrorClassificationMessage != "" || m.ErrorStatus != 0 {
		return terminalUncertain
	}
	return terminalDone
}

func (w *worker) terminalState(e rpcEvent) terminalResult {
	if m, ok := lastAssistant(e.Messages); ok {
		return assistantTerminalState(m)
	}
	if w.lastAssistant != nil {
		return assistantTerminalState(message{
			Role:                       "assistant",
			StopReason:                 w.lastAssistant.StopReason,
			ErrorMessage:               w.lastAssistant.ErrorMessage,
			ErrorClassificationMessage: w.lastAssistant.ErrorClassificationMessage,
			ErrorStatus:                w.lastAssistant.ErrorStatus,
		})
	}
	return terminalDone
}

func isRateLimitedError(errorMessage string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", " ", "-", " ").Replace(errorMessage))
	if strings.Contains(normalized, "rate limit") || strings.Contains(normalized, "too many requests") {
		return true
	}
	for _, token := range strings.FieldsFunc(normalized, func(r rune) bool {
		return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'z'))
	}) {
		if token == "429" {
			return true
		}
	}
	return false
}

func (w *worker) terminalFailureNotice(e rpcEvent) string {
	m := message{}
	if assistant, ok := lastAssistant(e.Messages); ok {
		m = assistant
	} else if w.lastAssistant != nil {
		m = message{
			Role:                       "assistant",
			StopReason:                 w.lastAssistant.StopReason,
			ErrorMessage:               w.lastAssistant.ErrorMessage,
			ErrorClassificationMessage: w.lastAssistant.ErrorClassificationMessage,
			ErrorStatus:                w.lastAssistant.ErrorStatus,
		}
	}
	classification := m.ErrorClassificationMessage
	if classification == "" {
		classification = m.ErrorMessage
	}
	if m.ErrorStatus == http.StatusTooManyRequests || isRateLimitedError(classification) {
		return "omp reported that the task was rate-limited by the model provider. The task outcome is uncertain and will not be replayed automatically."
	}
	return "omp reported that the task failed before producing a confirmed result. The task outcome is uncertain and will not be replayed automatically."
}

func (w *worker) finishTerminal(e rpcEvent) {
	switch w.terminalState(e) {
	case terminalUncertain:
		w.finishUncertain(w.terminalFailureNotice(e))
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
	w.previewID = 0
	w.lastPreview = ""
	w.previewStopToken = ""
	// Delivery reconciliation clears any persisted inbox association after success.
}
func (w *worker) event(raw []byte) {
	w.touchActivity()
	var e rpcEvent
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	w.logLifecycle(e)
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
		w.awaitingContinuation = false
		w.busy = true
		w.progress.ActiveTools = make(map[string]progressTool)
		w.lastAssistant = nil
		w.finalAssistantTexts = nil
	case "message_end":
		var m message
		if json.Unmarshal(e.Message, &m) == nil && m.Role == "assistant" {
			w.lastAssistant = &terminalAssistant{
				StopReason:                 m.StopReason,
				ErrorMessage:               m.ErrorMessage,
				ErrorClassificationMessage: m.ErrorClassificationMessage,
				ErrorStatus:                m.ErrorStatus,
			}
			if text := assistantText(m); text != "" {
				w.finalAssistantTexts = append(w.finalAssistantTexts, text)
			}
		}
	case "agent_end":
		if e.IsTerminal != nil && !*e.IsTerminal {
			w.awaitingContinuation = true
			return
		}
		w.awaitingContinuation = false
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
			w.closeLogicalSession()
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
	w.stopTyping()
	w.lastTyping = time.Now()
	ctx, cancel := context.WithTimeout(w.ctx, 4*time.Second)
	w.typingCancel = cancel
	w.background.Add(1)
	go func() {
		defer w.background.Done()
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

func (w *worker) newProgressStopToken() string {
	if w.owner == 0 {
		return ""
	}
	var data [12]byte
	if _, err := rand.Read(data[:]); err != nil {
		return ""
	}
	return fmt.Sprintf("stop-%d-%s", w.owner, hex.EncodeToString(data[:]))
}

func (w *worker) progressKeyboard(token string) *telegram.Keyboard {
	if token != "" && token != w.previewStopToken {
		return &telegram.Keyboard{InlineKeyboard: [][]telegram.Button{{{Text: "Stop", CallbackData: token + ":0"}}}}
	}
	if token == "" {
		token = w.previewStopToken
	}
	c, ok := w.confirms[token]
	if !ok || c.action != "stop" || c.generation != w.binding.Generation || c.turn != w.turn || c.active != w.active {
		return nil
	}
	return &telegram.Keyboard{InlineKeyboard: [][]telegram.Button{{{Text: "Stop", CallbackData: token + ":0"}}}}
}

func progressStopTokenOwner(token string) (int64, bool) {
	raw, ok := strings.CutPrefix(token, "stop-")
	if !ok {
		return 0, false
	}
	owner, nonce, ok := strings.Cut(raw, "-")
	if !ok || len(nonce) != 24 {
		return 0, false
	}
	id, err := strconv.ParseInt(owner, 10, 64)
	return id, err == nil && id > 0
}

func (w *worker) deleteOrphanProgressMessage(messageID int64) {
	if messageID == 0 || w.b.tg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.b.tg.Delete(ctx, w.key.chat, messageID); err != nil {
		logTelegramFailure(w.b.telegramLog, slog.LevelWarn, "progress_cleanup_failed", "orphan progress message cleanup failed", err,
			slog.Int64("chat_id", w.key.chat), slog.Int64("message_id", messageID))
	}
}

func (w *worker) recordProgressMessage(result previewResult) {
	if !result.created || result.err != nil || result.id == 0 || result.taskID == 0 || w.b.db == nil {
		return
	}
	if err := w.b.db.SetProgressMessage(result.taskID, result.id); err != nil {
		w.b.storeLog.Error("progress message association failed", "event", "progress_message_write_failed", "inbox_id", result.taskID, "message_id", result.id)
		w.deleteOrphanProgressMessage(result.id)
		w.b.fail(err)
		return
	}
	if w.ctx != nil && (result.generation != w.binding.Generation || result.turn != w.turn || w.active == 0) {
		w.b.cleanupDeliveredProgress(w.ctx, result.taskID)
	}
}

func (w *worker) previewFinished(result previewResult) {
	w.recordProgressMessage(result)
	w.previewBusy = false
	if result.generation != w.binding.Generation || result.turn != w.turn {
		if result.stopToken != "" {
			if c, ok := w.confirms[result.stopToken]; ok && c.action == "stop" && c.generation == result.generation && c.turn == result.turn {
				delete(w.confirms, result.stopToken)
				if w.previewStopToken == result.stopToken {
					w.previewStopToken = ""
				}
			}
			w.queueKeyboardCleanup(result.id)
		}
		return
	}
	if result.err == nil {
		w.previewID = result.id
		w.lastPreview = result.text
	}
	if result.stopToken != "" {
		c, registered := w.confirms[result.stopToken]
		switch {
		case !registered || c.action != "stop" || c.generation != result.generation || c.turn != result.turn:
			// The token was consumed while Telegram accepted the initial send.
			w.queueKeyboardCleanup(result.id)
		case result.err != nil:
			delete(w.confirms, result.stopToken)
			if w.previewStopToken == result.stopToken {
				w.previewStopToken = ""
			}
		case w.active != 0 && w.busy && !w.finishing && c.active == w.active:
			c.messageID = result.id
			w.confirms[result.stopToken] = c
		default:
			delete(w.confirms, result.stopToken)
			if w.previewStopToken == result.stopToken {
				w.previewStopToken = ""
			}
			w.queueKeyboardCleanup(result.id)
		}
	}
	if result.err != nil && result.id == 0 {
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
	if w.previewID == 0 && time.Since(w.progress.StartedAt) < progressInitialDelay {
		return
	}
	snapshot := w.renderProgress()
	if w.finishing || snapshot == "" || w.previewBusy || snapshot == w.lastPreview {
		return
	}
	text := telegram.ClipConvertible(snapshot, telegram.MaxMessageUTF16)
	id := w.previewID
	creating := id == 0
	gen := w.binding.Generation
	turn := w.turn
	replyTo := w.activeReplyTo
	taskID := w.active
	stopToken := ""
	if id == 0 && w.active != 0 && w.busy && !w.finishing {
		stopToken = w.newProgressStopToken()
		if stopToken != "" {
			w.confirms[stopToken] = confirmation{action: "stop", generation: gen, active: w.active, turn: turn, user: w.owner}
			w.previewStopToken = stopToken
		}
	}
	keyboard := w.progressKeyboard(stopToken)
	w.previewBusy = true
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
		defer cancel()
		var err error
		if id == 0 {
			var message telegram.Message
			message, err = w.b.tg.Send(ctx, w.key.chat, w.key.thread, text, telegram.SendOptions{ReplyToMessageID: replyTo, Keyboard: keyboard})
			if err == nil {
				id = message.MessageID
			}
		} else {
			err = w.b.tg.Edit(ctx, w.key.chat, id, text, keyboard)
		}
		select {
		case w.previewResult <- previewResult{id: id, taskID: taskID, created: creating, text: snapshot, stopToken: stopToken, generation: gen, turn: turn, err: err}:
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
	if c.action == "binding_delete" {
		c.generation = w.conversationGeneration()
	}
	if c.user == 0 {
		c.user = w.owner
	}
	k := &telegram.Keyboard{}
	for i, label := range options {
		k.InlineKeyboard = append(k.InlineKeyboard, []telegram.Button{{Text: label, CallbackData: fmt.Sprintf("%s:%d", token, i)}})
	}
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	w.touchActivity()
	defer cancel()
	message, e := w.b.tg.Send(ctx, w.key.chat, w.key.thread, title, telegram.SendOptions{Keyboard: k})
	if e != nil {
		w.cancelUI(c)
		w.say("Failed to send the confirmation. The operation was canceled.")
		return
	}
	c.messageID = message.MessageID
	w.confirms[token] = c
}
func (w *worker) queueCallback(ctx context.Context, q *telegram.CallbackQuery, token, action string, c confirmation) callbackResult {
	if (!c.expires.IsZero() && time.Now().After(c.expires)) || c.generation != w.conversationGeneration() || q.Message == nil || q.Message.MessageID != c.messageID {
		delete(w.confirms, token)
		if q.Message != nil && q.Message.MessageID == c.messageID {
			w.clearKeyboard(c.messageID)
		}
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
		return callbackDone
	}
	switch {
	case action == "close":
		delete(w.confirms, token)
		w.clearKeyboard(c.messageID)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "Closed")
	case action == "previous":
		delete(w.confirms, token)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "Received")
		w.showQueuePage(c, c.page-1, c.messageID)
	case action == "next":
		delete(w.confirms, token)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "Received")
		w.showQueuePage(c, c.page+1, c.messageID)
	case strings.HasPrefix(action, "cancel:"):
		id, err := strconv.ParseInt(strings.TrimPrefix(action, "cancel:"), 10, 64)
		if err != nil || id <= 0 {
			delete(w.confirms, token)
			w.clearKeyboard(c.messageID)
			_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
			return callbackDone
		}
		delete(w.confirms, token)
		if w.cancelQueuedTask(id) {
			_ = w.b.tg.AnswerCallback(ctx, q.ID, "Cancelled queued task.")
		} else {
			_ = w.b.tg.AnswerCallback(ctx, q.ID, "Task is no longer queued.")
		}
		w.showQueuePage(c, c.page, c.messageID)
	default:
		delete(w.confirms, token)
		w.clearKeyboard(c.messageID)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
	}
	return callbackDone
}

func (w *worker) callback(q *telegram.CallbackQuery) callbackResult {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	token, index, ok := strings.Cut(q.Data, ":")
	c, exists := w.confirms[token]
	if !ok || !exists {
		if owner, isStop := progressStopTokenOwner(token); isStop && owner == q.From.ID && q.Message != nil {
			w.clearKeyboard(q.Message.MessageID)
		}
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
		return callbackDone
	}
	if c.user != q.From.ID {
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
		return callbackDone
	}
	if (c.action == "resume" || c.action == "export") && (q.Message == nil || q.Message.MessageID != c.messageID) {
		delete(w.confirms, token)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
		return callbackDone
	}
	if c.action == "queue" {
		return w.queueCallback(ctx, q, token, index, c)
	}
	if c.action == "bindings" || c.action == "binding_delete" {
		return w.bindingCallback(ctx, q, token, index, c)
	}
	messageID := c.messageID
	if c.action == "stop" && messageID == 0 && q.Message != nil {
		messageID = q.Message.MessageID
	}
	if (!c.expires.IsZero() && time.Now().After(c.expires)) || c.generation != w.binding.Generation {
		delete(w.confirms, token)
		if w.previewStopToken == token {
			w.previewStopToken = ""
		}
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
		w.clearKeyboard(messageID)
		if c.generation == w.binding.Generation {
			w.cancelUI(c)
		}
		return callbackDone
	}
	n, err := strconv.Atoi(index)
	if err != nil || n < 0 || (c.action == "stop" && n != 0) || (c.method != "select" && c.action != "stop" && n > 1) || (c.method == "select" && n > len(c.options)) {
		return callbackDone
	}
	if c.action == "stop" {
		if c.active != w.active || c.turn != w.turn || w.active == 0 || !w.busy {
			delete(w.confirms, token)
			if w.previewStopToken == token {
				w.previewStopToken = ""
			}
			_ = w.b.tg.AnswerCallback(ctx, q.ID, "This action has expired")
			w.clearKeyboard(messageID)
			return callbackDone
		}
		delete(w.confirms, token)
		if w.previewStopToken == token {
			w.previewStopToken = ""
		}
		_ = w.b.tg.AnswerCallback(ctx, q.ID, "Stopping")
		w.clearKeyboard(messageID)
		w.stopActiveTask()
		return callbackDone
	}
	_ = w.b.tg.AnswerCallback(ctx, q.ID, "Received")
	if c.action == "resume" || c.action == "export" {
		delete(w.confirms, token)
		messageID := c.messageID
		if messageID == 0 && q.Message != nil {
			messageID = q.Message.MessageID
		}
		w.selectSession(c, n, messageID)
		return callbackDone
	}
	delete(w.confirms, token)
	w.clearKeyboard(c.messageID)
	if c.action == "ui" && w.exportingSession != "" {
		w.say("Wait for the current session export to finish.")
		return callbackDone
	}
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
		client, err := w.ensureRuntime()
		if err != nil || client.Send(ctx, frame) != nil {
			w.say("The confirmation could not be delivered to omp. Its state is uncertain and the instance has been closed.")
			w.closeLogicalSession()
			return callbackUncertain
		}
		w.touchActivity()
		w.touchBinding()
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
		if w.sessionControlBusy() {
			w.say("The instance is not idle. Compaction was canceled.")
			return callbackDone
		}
		client, err := w.ensureRuntime()
		if err != nil {
			w.say("The instance is not idle. Compaction was canceled.")
			return callbackDone
		}
		w.busy = true
		w.compacting = true
		w.touchActivity()
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
	if client, connected := w.runtimeClient(); c.uiID != "" && connected {
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
		defer cancel()
		_ = client.Send(ctx, map[string]any{"type": "extension_ui_response", "id": c.uiID, "cancelled": true})
	}
}

// Programmatic invalidation removes the Telegram token first. Expiry and
// explicit runtime teardown may then cancel a current-generation native UI.
func (w *worker) dropConfirmation(token string, c confirmation) {
	delete(w.confirms, token)
	if w.previewStopToken == token {
		w.previewStopToken = ""
	}
	w.queueKeyboardCleanup(c.messageID)
}

func (w *worker) clearConfirmations() {
	for token, c := range w.confirms {
		w.dropConfirmation(token, c)
	}
}

func (w *worker) clearTaskConfirmations() {
	for token, c := range w.confirms {
		if c.action == "queue" || c.action == "resume" || c.action == "export" {
			continue
		}
		w.dropConfirmation(token, c)
	}
}

func (w *worker) clearRuntimeConfirmations() {
	for token, c := range w.confirms {
		if c.action == "resume" {
			continue
		}
		w.dropConfirmation(token, c)
		if c.action == "ui" && c.generation == w.binding.Generation {
			w.cancelUI(c)
		}
	}
}

func (w *worker) expire() {
	now := time.Now()
	for token, c := range w.confirms {
		if !c.expires.IsZero() && now.After(c.expires) {
			w.dropConfirmation(token, c)
			if c.generation == w.binding.Generation {
				w.cancelUI(c)
			}
		}
	}
	w.expireAlbums(now)
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
