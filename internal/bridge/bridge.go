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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"omp-telegram/internal/config"
	"omp-telegram/internal/i18n"
	"omp-telegram/internal/logging"
	"omp-telegram/internal/media"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

type Bridge struct {
	cfg               config.Config
	version           string
	db                *store.Store
	tg                *telegram.Client
	bot               telegram.User
	log               *slog.Logger
	telegramLog       *slog.Logger
	storeLog          *slog.Logger
	mediaLog          *slog.Logger
	rpcLog            *slog.Logger
	slots             chan struct{}
	argsMu            sync.Mutex
	argsPresets       [][]string
	argsFingerprint   string
	argsIndex         int
	wg                sync.WaitGroup
	fatal             chan error
	mediaSlots        chan struct{}
	resumeSlots       chan struct{}
	sessionMu         sync.Mutex
	sessionClaims     map[string]sessionClaim
	exportClaims      map[*worker]string
	deleteClaims      map[*worker]string
	bindingsEpoch     atomic.Uint64
	incomingMu        sync.Mutex
	incomingInUse     map[string]struct{}
	ctx               context.Context
	workerExits       chan workerExit
	forgetRequests    chan bindingForgetRequest
	commandMenus      commandMenuState
	languageMu        sync.Mutex
	interfaceLanguage atomic.Uint32
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

type bindingForgetRequest struct {
	key        target
	generation int64
	session    bool
	source     *worker
}

type bindingForgetResult struct {
	status  i18n.Message
	binding *store.Binding
	ok      bool
}

const deliveryWorkerCount = 4

const maxAssistantOutputBytes = 4 << 20
const maxFinalReplyBytes = 1 << 20
const maxFinalReplyMessages = 32

func supportedConversation(m *telegram.Message) bool {
	return m != nil && (m.MessageThreadID != 0 || m.Chat.Type == "private")
}

func updateSender(u telegram.Update) (*telegram.Message, int64) {
	if u.CallbackQuery != nil {
		return u.CallbackQuery.Message, u.CallbackQuery.From.ID
	}
	if u.Message != nil && u.Message.From != nil {
		return u.Message, u.Message.From.ID
	}
	return u.Message, 0
}

type queued struct {
	id            int64
	user          int64
	replyTo       int64
	text          string
	displayText   string
	kind          queuedInputKind
	nativeName    string
	nativeSession string
	bridgeReview  bool // Explicit /review, not raw followup or attachment text.
	reply         *telegram.Message
	images        []media.Image
	albumKey      albumKey
	album         bool
	preparing     bool
	cancel        context.CancelFunc
	directory     string
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
	action, uiID, method            string
	uiTitle                         string
	uiClientID                      uint64
	workspace                       string
	exportFormat                    string
	ompArgs                         []string
	argsPresetIndex                 int
	argsFingerprint                 string
	options                         []string
	expires                         time.Time
	generation, active              int64
	epoch                           uint64
	turn                            uint64
	user                            int64
	messageID                       int64
	sessions                        []omp.SessionSummary
	nativeSessions                  []omp.SessionSummary
	pickerOptions                   []resumePickerOption
	pinnedSessions                  map[string]struct{}
	models                          []omp.ModelRole
	bindings                        []store.BindingListEntry
	bindingsOld                     bool
	deleteBot                       int64
	deleteChat                      int64
	deleteThread                    int64
	deleteGeneration                int64
	deleteRunning                   bool
	page, argsPage, argsChoiceCount int
	uiSelectMenu                    bool
	autoresearchText                string
}
type runtimeState uint8

const (
	runtimeReleased runtimeState = iota
	runtimeStarting
	runtimeConnected
)

type taskState struct {
	active, activeReplyTo int64
	owner                 int64
	awaitingContinuation  bool
	busy                  bool
	activeInputKind       queuedInputKind
}

type controlOperation uint8

const (
	controlNone controlOperation = iota
	controlAbort
	controlStatus
	controlName
	controlModel
	controlNative
)

type sessionOperation uint8

const (
	sessionOperationNone sessionOperation = iota
	sessionOperationCompact
	sessionOperationHandoff
)

type progressTransport struct {
	finishing          bool
	previewBusy        bool
	previewID          int64
	previewStopToken   string
	progressSuppressed bool
	lastPreview        string
}

type runtimeLifecycle struct {
	runtime         runtimeState
	runtimeResuming bool
	restoring       bool
}

type worker struct {
	b      *Bridge
	log    *slog.Logger
	key    target
	input  chan incoming
	client *omp.Client
	// Cross-worker deletion checks distinguish a live runtime from a retained logical claim.
	sessionRuntimeActive   atomic.Bool
	sessionRuntimeStarting atomic.Bool
	launchArgs             []string
	configuredThinking     string
	binding                store.Binding
	startIntent            *store.StartIntent
	migration              *store.Migration
	migrationCancel        context.CancelFunc
	migrationClientID      uint64
	migrationAttempt       uint64
	migrationStartup       *migrationStartup
	migrationRuntimeCancel context.CancelFunc
	migrationRun           *migrationRun
	migrationResults       chan migrationResult
	sessionID              string
	claimedSession         string
	runtimeLifecycle
	queue                []queued
	stream               strings.Builder
	preview              string
	finalAssistantBytes  int
	finalizedStreamBytes int
	finalOutputTruncated bool
	taskState
	turn                uint64
	steers              map[string]steerRequest
	steerFence          steerFence
	activeAttachmentDir string
	rootRequestID       string
	nativeCatalog       nativeCommandCatalog
	research            autoresearchState
	nativeControl       *nativeControlRequest
	resumeFailed        bool
	progressTransport
	compacting             bool
	progress               progressState
	progressNative         progressNativeState
	replyStatus            replyStatusState
	operations             chan operationResult
	albums                 map[albumKey]*pendingAlbum
	rpcOperations          []rpcOperation
	rpcOperationActive     bool
	rpcOperationClientID   uint64
	rpcExitPendingClientID uint64
	modelMenuID            int64
	topicRenameResults     chan topicRenameResult
	albumEvents            chan albumEvent
	albumVersion           uint64
	albumSuppressed        map[albumKey]time.Time
	background             sync.WaitGroup
	controlOp              controlOperation
	statusReleaseClientID  uint64
	sessionOp              sessionOperation
	lastActivity           time.Time
	lastLogicalActivity    time.Time

	lastIntentRetentionCheck time.Time

	lastTyping          time.Time
	typingCancel        context.CancelFunc
	idleProbe           idleProbeState
	lastAssistant       *terminalAssistant
	finalAssistantTexts []string
	assistantRunStart   int
	previewResult       chan previewResult
	confirms            map[string]confirmation
	keyboardCleanup     chan int64
	stopCallbackReplies chan stopCallbackReply
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
	deleteResults       chan sessionDeleteResult
	deleteCancel        context.CancelFunc
	deleteContext       context.Context
	deleteRequest       uint64
	bindingNameResults  chan bindingNamesResult
	bindingNameCancel   context.CancelFunc
	bindingNameRequest  uint64
	doctorResults       chan doctorResult
	doctorCancel        context.CancelFunc
	doctorRequest       uint64
	forgetRequests      chan bindingForgetRequest
	forgetResults       chan bindingForgetResult
	forgetPending       bool
	forgetConfirmation  *confirmation

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
	maxActiveTools       = 6
	maxRecentTools       = 5
	maxToolNameUnits     = 64
	maxToolDetailUnits   = 100
	maxToolDetailsUnits  = 1600
	maxPreviewUnits      = 2200
	maxProgressUnits     = 3500
	progressInitialDelay = 3 * time.Second
)

var logicalWorkerIdleTimeout = 5 * time.Minute

type progressState struct {
	StartedAt    time.Time
	ActiveTools  map[string]progressTool
	RecentTools  []progressToolResult
	EarlierTools int
	Retrying     bool
	NativeStatus string
}

type progressTool struct {
	ID         string
	Name       string
	Args       string
	Detail     string
	DetailKind i18n.Message
	StartedAt  time.Time
}

type progressToolResult struct {
	Name       string
	Args       string
	Detail     string
	DetailKind i18n.Message
	Duration   time.Duration
	Failed     bool
}

type operationResult struct {
	generation int64
	active     int64
	turn       uint64
	clientID   uint64
	kind       string
	data       json.RawMessage
	message    string
	meta       any
	cancelled  bool
	err        error
}

func uncertainOperationOutcome(err error, cancelled bool) bool {
	if err != nil {
		return omp.ClassifyError(err) != "rejected"
	}
	return cancelled
}

type rpcOperation struct {
	generation int64
	active     int64
	turn       uint64
	clientID   uint64
	kind       string
	call       func(context.Context) (json.RawMessage, error)
	message    string
	meta       any
}
type topicRenameResult struct {
	generation int64
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
	Status                string                       `json:"status"`
	SessionSettled        *bool                        `json:"sessionSettled"`
	Method                string                       `json:"method"`
	TargetID              string                       `json:"targetId"`
	Arguments             json.RawMessage              `json:"arguments"`
	ThinkingLevel         string                       `json:"thinkingLevel"`
	Configured            *string                      `json:"configured"`
	Title                 string                       `json:"title"`
	Message               json.RawMessage              `json:"message"`
	Text                  string                       `json:"text"`
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
	ModelID                    string          `json:"model"`
	Provider                   string          `json:"provider"`
	Duration                   *float64        `json:"duration"`
	Usage                      *messageUsage   `json:"usage"`
	Content                    json.RawMessage `json:"content"`
	StopReason                 string          `json:"stopReason"`
	ErrorMessage               string          `json:"errorMessage"`
	ErrorClassificationMessage string          `json:"errorClassificationMessage"`
	ErrorStatus                int             `json:"errorStatus"`
}

type messageUsage struct {
	Input         *int64 `json:"input"`
	Output        *int64 `json:"output"`
	CacheRead     *int64 `json:"cacheRead"`
	CacheWrite    *int64 `json:"cacheWrite"`
	TotalTokens   *int64 `json:"totalTokens"`
	ContextTokens *int64 `json:"contextTokens"`
	Orchestration *struct {
		Input     *int64 `json:"input"`
		Output    *int64 `json:"output"`
		CacheRead *int64 `json:"cacheRead"`
	} `json:"orchestration"`
}

type terminalResult int

const (
	terminalDone terminalResult = iota
	terminalUncertain
	terminalCancelled
)

var botCommands = []struct {
	Command     string
	Description i18n.Message
}{
	{Command: "new", Description: i18n.Msg.NewSessionNewNameProjectPath},
	{Command: "stop", Description: i18n.Msg.StopTaskClearQueue},
	{Command: "followup", Description: i18n.Msg.QueuePromptAfterCurrentTask},
	{Command: "queue", Description: i18n.Msg.ShowCancelPendingBridgeTasks},
	{Command: "resume", Description: i18n.Msg.ChoosePinOmpSessionWorkingDirectory},
	{Command: "args", Description: i18n.Msg.SelectOMPArgumentPresetArgsNumber},
	{Command: "review", Description: i18n.Msg.RunOmpReviewReviewArguments},
	{Command: "close", Description: i18n.Msg.CloseOmpKeepWorkspaceSession},
	{Command: "export", Description: i18n.Msg.ExportOmpSessionExportHtmlSessionId},
	{Command: "bindings", Description: i18n.Msg.ListSavedBindingsBindingsOld},
	{Command: "status", Description: i18n.Msg.ShowSessionModelContextSpeedQueue},
	{Command: "doctor", Description: i18n.Msg.RunSafeDiagnostics},
	{Command: "name", Description: i18n.Msg.NameOmpSessionNameTitle},
	{Command: "model", Description: i18n.Msg.ChooseCycleRoleModelProviderModel},
	{Command: "thinking", Description: i18n.Msg.ChooseThinkingLevelSession},
	{Command: "fast", Description: i18n.Msg.ToggleNativeFastMode},
	{Command: "compact", Description: i18n.Msg.CompactContextWhileIdle},
	{Command: "handoff", Description: i18n.Msg.RunNativeHandoffHandoffInstructions},
	{Command: "migrate", Description: i18n.Msg.MigrateContextMigrateRecoverOldSessionIdSessionFile},
	{Command: "lang", Description: i18n.Msg.Language.CommandDescription},
	{Command: "help", Description: i18n.Msg.CommandHelp},
}

const pendingInputBatchSize = 256

func Run(ctx context.Context, cfg config.Config, db *store.Store, logs *logging.Registry, version string) (runErr error) {
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
	ctx, cancel := context.WithCancel(ctx)
	b := &Bridge{cfg: cfg, version: version, db: db, tg: tg, bot: bot, log: log, telegramLog: telegramLog, storeLog: storeLog, mediaLog: mediaLog, rpcLog: rpcLog, slots: make(chan struct{}, cfg.MaxWorkers), fatal: make(chan error, 1), mediaSlots: make(chan struct{}, 2), resumeSlots: make(chan struct{}, 2), ctx: ctx, workerExits: make(chan workerExit), forgetRequests: make(chan bindingForgetRequest, 16)}
	if err := b.initializeArgsSelection(); err != nil {
		cancel()
		storeLog.Error("OMP argument selection load failed", "event", "omp_args_selection_failed", "error_kind", "persistence")
		return err
	}
	if err := b.initializeLanguage(); err != nil {
		cancel()
		storeLog.Error("interface language read failed", "event", "language_read_failed", "error_kind", "persistence")
		return err
	}
	b.bootstrapCommandMenus(ctx)
	// Command menus improve discoverability but are not required for polling.
	registered := 0
	for _, language := range []string{"", "zh"} {
		if err = tg.SetCommands(ctx, localizedCommands(nil, b.language()), language); err != nil {
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

	defer func() {
		cancel()
		b.wg.Wait()
		if err := b.cancelPendingPrompts("shutdown"); err != nil && runErr == nil {
			runErr = err
		}
	}()
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.runCommandMenus(ctx)
	}()
	b.reconcileProgressCleanup(ctx)
	workers := map[target]*worker{}
	if err := b.restoreWorkers(ctx, workers); err != nil {
		return err
	}
	b.cleanupDatabase(ctx)
	b.wg.Add(deliveryWorkerCount + 2)
	if cfg.DatabaseRetentionDays > 0 {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.runDatabaseJanitor(ctx)
		}()
	}
	go func() {
		defer b.wg.Done()
		b.runProgressJanitor(ctx, progressCleanupInterval)
	}()
	for range deliveryWorkerCount {
		go func() {
			defer b.wg.Done()
			if err := b.deliver(ctx); err != nil {
				b.fail(err)
			}
		}()
	}
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
				var err error
				m, user := updateSender(u)
				if m == nil || !cfg.Authorized(user, m.Chat.ID) {
					err = db.AcceptIgnored(u.UpdateID)
				} else {
					var raw []byte
					raw, err = json.Marshal(u)
					if err == nil {
						err = db.Accept(u.UpdateID, raw)
					}
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
	pendingCursor := int64(-1)
	var pendingHighWater int64
	pendingCycleActive := false
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if len(delivered) > 0 {
			ids := make([]int64, 0, len(delivered))
			for id := range delivered {
				ids = append(ids, id)
			}
			pendingIDs, err := db.PendingIDs(ids)
			if err != nil {
				b.storeLog.Error("pending input state read failed", "event", "inbox_read_failed", "reason", "pending_ids", "error_kind", "persistence")
				return err
			}
			for id := range delivered {
				if _, ok := pendingIDs[id]; !ok {
					delete(delivered, id)
				}
			}
		}
		if !pendingCycleActive {
			offset, err := db.Offset()
			if err != nil {
				b.storeLog.Error("database offset failed", "event", "inbox_read_failed", "reason", "offset", "error_kind", "persistence")
				return err
			}
			pendingCursor = -1
			pendingHighWater = offset - 1
			pendingCycleActive = true
		}
		inputs, err := db.PendingAfter(pendingCursor, pendingHighWater, pendingInputBatchSize)
		if err != nil {
			b.storeLog.Error("pending input read failed", "event", "inbox_read_failed", "reason", "pending", "error_kind", "persistence")
			return err
		}
		if len(inputs) == 0 {
			pendingCursor = -1
			pendingHighWater = 0
			pendingCycleActive = false
		} else {
			pendingCursor = inputs[len(inputs)-1].ID
			if pendingCursor >= pendingHighWater {
				pendingCycleActive = false
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
			m, user := updateSender(u)
			if m == nil || !cfg.Authorized(user, m.Chat.ID) {
				if err = db.Mark(in.ID, "ignored"); err != nil {
					b.storeLog.Error("input state persistence failed", "event", "inbox_state_write_failed")
					return err
				}
				continue
			}
			if !supportedConversation(m) {
				notice := b.tr(i18n.Msg.BotSupportsPrivateChatsTelegramTopicsGroupsUseInside)
				if err = db.Enqueue(m.Chat.ID, 0, notice); err != nil {
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
		case request := <-b.forgetRequests:
			if target := workers[request.key]; target == nil || !target.tryForget(request) {
				request.reply(bindingForgetResult{status: i18n.Msg.BindingChangedBusyUseBindingsRefresh})
			}
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

const progressCleanupInterval = time.Minute

func (b *Bridge) runProgressJanitor(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.reconcileProgressCleanup(ctx)
		}
	}
}

const databaseJanitorInterval = 24 * time.Hour

func (b *Bridge) cleanupDatabase(ctx context.Context) {
	if b.cfg.DatabaseRetentionDays == 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -b.cfg.DatabaseRetentionDays)
	result, err := b.db.CleanupMessages(ctx, cutoff.Unix())
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
	b.reconcileOutboxSnapshots(ctx, cutoff)
	var incomingCount int
	if b.cfg.DataDir != "" {
		cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		incomingCount, err = b.cleanupIncomingAttachments(cleanupCtx, cutoff)
		cancel()
		if err != nil && ctx.Err() == nil {
			b.storeLog.Warn("incoming attachment cleanup failed", "event", "cleanup_failed", "error_kind", "filesystem")
		}
	}
	if result.Inbox != 0 || result.Outbox != 0 || incomingCount != 0 {
		b.storeLog.Info("database cleanup completed", "event", "cleanup_completed", "inbox_count", result.Inbox, "outbox_count", result.Outbox, "incoming_count", incomingCount)
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

const maxServerRetryDelaySeconds int64 = 5 * 60
const maxServerDeliveryAttempts int64 = 12

func retryableServerRejection(info telegram.ErrorInfo) bool {
	if info.Uncertain {
		return false
	}
	switch info.Code {
	case http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func deliveryRetryDelaySeconds(output store.Output, info telegram.ErrorInfo) int64 {
	if info.Reason == "rate_limited" {
		if info.RetryAfter > 0 {
			return int64(info.RetryAfter)
		}
		return 1
	}
	attemptCount := output.AttemptCount
	if retryableServerRejection(info) {
		attemptCount = output.ServerRetryCount + 1
	}
	delay := int64(1)
	for attempt := int64(1); attempt < attemptCount && delay < maxServerRetryDelaySeconds; attempt++ {
		delay *= 2
	}
	if delay > maxServerRetryDelaySeconds {
		return maxServerRetryDelaySeconds
	}
	return delay
}

func (b *Bridge) deliver(ctx context.Context) error {
	for ctx.Err() == nil {
		o, err := b.db.ClaimNextOutput(ctx, time.Now().Unix())
		if errors.Is(err, sql.ErrNoRows) {
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if o.ID != 0 {
				b.storeLog.Error("outbox claim state persistence failed", "event", "outbox_state_write_failed", "outbox_id", o.ID, "chat_id", o.Chat, "thread_id", o.Thread, "state", string(store.OutboxSending))
			} else {
				b.storeLog.Error("outbox claim failed", "event", "outbox_read_failed")
			}
			return err
		}
		switch o.Kind {
		case "text":
			_, err = b.tg.Send(ctx, o.Chat, o.Thread, o.Text, telegram.SendOptions{ReplyToMessageID: o.ReplyTo})
		case "photo", "document":
			_, err = b.tg.SendFile(ctx, o.Chat, o.Thread, o.Kind, o.Path, o.Name, o.Text, telegram.SendOptions{ReplyToMessageID: o.ReplyTo})
		default:
			b.storeLog.Error("unsupported outbox content kind", "event", "outbox_read_failed", "reason", "invalid_kind")
			return errors.New("unsupported outbox content kind")
		}
		state := store.OutboxDone
		retryExhausted := false
		apiCode := 0
		if err != nil {
			info := telegram.ClassifyError(err)
			apiCode = info.Code
			rateLimited := info.Reason == "rate_limited" && !info.Uncertain
			serverRejection := retryableServerRejection(info)
			if rateLimited || serverRejection {
				delay := deliveryRetryDelaySeconds(o, info)
				nextAttempt := time.Now().Unix()
				maxInt64 := int64(^uint64(0) >> 1)
				if delay > maxInt64-nextAttempt {
					nextAttempt = maxInt64
				} else {
					nextAttempt += delay
				}
				if serverRejection {
					serverRetryCount, exhausted, retryErr := b.db.RetryServerOutput(o.ID, nextAttempt, maxServerDeliveryAttempts)
					if retryErr != nil {
						b.storeLog.Error("outbox retry state persistence failed", "event", "outbox_state_write_failed", "outbox_id", o.ID, "chat_id", o.Chat, "thread_id", o.Thread)
						return retryErr
					}
					o.ServerRetryCount = serverRetryCount
					retryExhausted = exhausted
					if retryExhausted {
						state = store.OutboxFailed
					}
				} else if retryErr := b.db.RetryOutput(o.ID, nextAttempt); retryErr != nil {
					b.storeLog.Error("outbox retry state persistence failed", "event", "outbox_state_write_failed", "outbox_id", o.ID, "chat_id", o.Chat, "thread_id", o.Thread, "state", string(store.OutboxPending))
					return retryErr
				}
				if !retryExhausted {
					if ctx.Err() == nil {
						event, message := "delivery_retry_scheduled", "telegram delivery deferred for retry"
						if rateLimited {
							event, message = "delivery_rate_limited", "telegram delivery deferred by rate limit"
						}
						logTelegramFailure(b.telegramLog, slog.LevelWarn, event, message, err,
							slog.Int64("outbox_id", o.ID), slog.Int64("chat_id", o.Chat), slog.Int64("thread_id", o.Thread),
							slog.String("kind", o.Kind), slog.String("state", string(store.OutboxPending)), slog.Bool("replay", true))
					}
					continue
				}
			} else {
				state = store.OutboxFailed
				if info.Uncertain {
					state = store.OutboxUncertain
				}
				if ctx.Err() == nil && info.Reason != "cancelled" {
					logTelegramFailure(b.telegramLog, slog.LevelWarn, "delivery_failed", "telegram delivery failed", err,
						slog.Int64("outbox_id", o.ID), slog.Int64("chat_id", o.Chat), slog.Int64("thread_id", o.Thread),
						slog.String("kind", o.Kind), slog.String("state", string(state)), slog.Bool("replay", false))
				}
			}
		}
		if !retryExhausted {
			if err = b.db.MarkOutput(o.ID, state); err != nil {
				b.storeLog.Error("outbox state persistence failed", "event", "outbox_state_write_failed", "outbox_id", o.ID, "chat_id", o.Chat, "thread_id", o.Thread, "state", string(state))
				return err
			}
		}
		if retryExhausted {
			b.telegramLog.LogAttrs(context.Background(), slog.LevelWarn, "telegram delivery retries exhausted",
				slog.String("event", "delivery_retry_exhausted"),
				slog.Int64("outbox_id", o.ID), slog.Int64("chat_id", o.Chat), slog.Int64("thread_id", o.Thread),
				slog.String("kind", o.Kind), slog.Int64("attempt_count", o.AttemptCount), slog.Int64("server_retry_count", o.ServerRetryCount), slog.Int("api_code", apiCode))
		}
		if o.InboxID != 0 {
			b.cleanupDeliveredProgress(ctx, o.InboxID)
		}
		if o.Kind != "text" {
			if removeErr := removeOutboxSnapshot(filepath.Join(b.cfg.DataDir, "attachments", "outbox"), o.Path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				b.storeLog.Warn("attachment cleanup failed", "event", "snapshot_cleanup_failed")
			}
			if state != store.OutboxDone {
				source := i18n.Msg.AttachmentDeliveryFailedWillNotReplayedAutomatically
				if state == store.OutboxUncertain {
					source = i18n.Msg.AttachmentDeliveryUncertainWillNotReplayedAutomatically
				}
				notice := b.tr(source)
				if err = b.db.Enqueue(o.Chat, o.Thread, notice); err != nil {
					b.storeLog.Error("delivery failure notice persistence failed", "event", "outbox_write_failed")
					return err
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
	} else {
		for _, message := range messages {
			b.deleteProgressMessage(ctx, message)
		}
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

func (w *worker) tryForget(request bindingForgetRequest) bool {
	w.exitMu.Lock()
	defer w.exitMu.Unlock()
	if w.exitRequested {
		return false
	}
	select {
	case w.forgetRequests <- request:
		return true
	default:
		return false
	}
}

func (w *worker) taskActive() bool { return w.active != 0 }

func (w *worker) taskRunning() bool { return w.taskActive() && w.busy }

func (w *worker) beginTask(q queued) {
	w.cancelProgressNative()
	w.resetReplyStatus()
	w.taskState = taskState{active: q.id, activeReplyTo: q.replyTo, owner: q.user, busy: true, activeInputKind: q.kind}
	w.activeAttachmentDir = q.directory
	w.rootRequestID = ""
	w.turn++
	w.beginResearchTask(q)
}

func (w *worker) clearTask() {
	w.cancelProgressNative()
	w.resetReplyStatus()
	w.finishActiveIncoming()
	w.active = 0
	w.activeReplyTo = 0
	w.awaitingContinuation = false
	w.busy = false
	w.rootRequestID = ""
	w.activeInputKind = queuedPrompt
	w.research.root, w.research.command, w.research.roundOpen = false, false, false
}

func (w *worker) awaitTaskContinuation() { w.awaitingContinuation = true }

func (w *worker) clearAwaitingContinuation() { w.awaitingContinuation = false }

func (w *worker) controlInProgress() bool {
	return w.controlOp != controlNone || w.research.control != nil
}

func (w *worker) beginControlOperation(kind controlOperation) { w.controlOp = kind }

func (w *worker) endControlOperation() {
	w.controlOp = controlNone
	w.modelMenuID = 0
}

func (w *worker) sessionOperationActive() bool { return w.sessionOp != sessionOperationNone }

func (w *worker) beginSessionOperation(kind sessionOperation) { w.sessionOp = kind }

func (w *worker) endSessionOperation() { w.sessionOp = sessionOperationNone }

func (w *worker) completeSessionOperation() {
	w.endSessionOperation()
	w.compacting = false
}

func (w *worker) taskOrSessionBusy() bool { return w.busy || w.sessionOperationActive() }

func (w *worker) clearFailedHostOperation() {
	w.busy = false
	w.compacting = false
	w.endSessionOperation()
}

func (w *worker) sessionControlBusy() bool {
	return w.migrationLocked() || w.taskOrSessionBusy() || w.controlInProgress() || w.compacting || w.finishing || w.exportingSession != ""
}

func (w *worker) canDispatch() bool {
	return !w.resumeFailed && !w.steerFence.active && !w.sessionControlBusy() && len(w.queue) != 0
}

func (w *worker) canReleaseRuntime() bool {
	return !w.migrationLocked() && w.runtime == runtimeConnected && w.client != nil &&
		!w.taskActive() && !w.taskOrSessionBusy() && !w.controlInProgress() &&
		!w.compacting && !w.finishing && !w.previewBusy &&
		len(w.queue) == 0 && len(w.hostRequests) == 0 &&
		w.resumeCancel == nil && w.exportCancel == nil && w.deleteCancel == nil && w.startIntent == nil &&
		!w.hasRuntimeConfirmation()
}

func (w *worker) lifecycleIdle() bool {
	return !w.migrationLocked() && !w.binding.Running && w.runtime == runtimeReleased && w.client == nil &&
		!w.restoring && !w.runtimeResuming && !w.taskActive() &&
		!w.taskOrSessionBusy() && !w.controlInProgress() &&
		!w.compacting && !w.finishing && !w.previewBusy &&
		!w.awaitingContinuation && w.exportingSession == ""
}

func (w *worker) canEvict() bool {
	if !w.lifecycleIdle() {
		return false
	}
	return len(w.input) == 0 && len(w.queue) == 0 && len(w.albums) == 0 && len(w.albumSuppressed) == 0 &&
		len(w.hostRequests) == 0 && w.resumeCancel == nil && w.exportCancel == nil && w.deleteCancel == nil &&
		w.bindingNameCancel == nil && w.doctorCancel == nil && w.startIntent == nil &&
		len(w.confirms) == 0 && w.idleProbe.cancel == nil && w.typingCancel == nil &&
		len(w.operations) == 0 && len(w.previewResult) == 0 && len(w.mediaResults) == 0 &&
		len(w.sendResults) == 0 && len(w.resumeResults) == 0 && len(w.exportResults) == 0 && len(w.deleteResults) == 0 &&
		len(w.bindingNameResults) == 0 && len(w.doctorResults) == 0 && len(w.idleProbe.results) == 0 &&
		len(w.stopCallbackReplies) == 0 &&
		len(w.albumEvents) == 0 && len(w.keyboardCleanup) == 0
}

func (w *worker) hasRuntimeConfirmation() bool {
	for _, c := range w.confirms {
		switch c.action {
		case "model", "thinking", "ui", "new", "autoresearch_clear":
			return true
		}
	}
	return false
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
	if w.statusReleaseClientID != 0 || w.b.cfg.IdleTimeout <= 0 || !w.canReleaseRuntime() || w.lastActivity.IsZero() || now.Sub(w.lastActivity) < w.b.cfg.IdleTimeout || !w.sessionDurable() {
		return
	}
	w.releaseRuntimeWithReason(true, "idle")
}

func (w *worker) evictIfIdle(now time.Time) bool {
	if logicalWorkerIdleTimeout <= 0 || w.lastLogicalActivity.IsZero() || now.Sub(w.lastLogicalActivity) < logicalWorkerIdleTimeout {
		return false
	}
	w.exitMu.Lock()
	defer w.exitMu.Unlock()
	if w.exitRequested || !w.canEvict() {
		return false
	}
	w.exitRequested = true
	w.cancel()
	return true
}

func (w *worker) runtimeClient() (*omp.Client, bool) {
	return w.client, w.runtime == runtimeConnected && w.client != nil
}

func (w *worker) beginRuntimeStart() {
	w.sessionRuntimeActive.Store(false)
	w.sessionRuntimeStarting.Store(true)
	w.runtime = runtimeStarting
}

func (w *worker) runtimeStarted(client *omp.Client, args []string) {
	w.b.registerCommandMenuWorker(w)
	w.client = client
	w.launchArgs = args
	w.configuredThinking = ""
	w.runtime = runtimeConnected
	w.resumeFailed = false
	w.b.sessionMu.Lock()
	w.sessionRuntimeActive.Store(true)
	w.sessionRuntimeStarting.Store(false)
	w.b.sessionMu.Unlock()
}

func (w *worker) runtimeStopped() {
	w.cancelNativeControl()
	w.flushReplyStatus()
	w.cancelProgressNative()
	w.configuredThinking = ""
	w.statusReleaseClientID = 0
	w.research.known, w.research.enabled = false, false
	w.clearCommandCatalog()
	for token, c := range w.confirms {
		if c.action == "ui" {
			w.dropConfirmation(token, c)
		}
	}
	w.client = nil
	w.launchArgs = nil
	w.runtime = runtimeReleased
}

func (w *worker) beginRuntimeResume() {
	w.restoring, w.runtimeResuming = true, true
}

func (w *worker) endRuntimeResume() {
	w.restoring, w.runtimeResuming = false, false
}

func (w *worker) releaseRuntimeWithReason(releaseSlot bool, reason string) {
	w.interruptModelMenu()
	w.invalidateResearchAdmission()
	w.cancelResearchControl()
	w.stopTyping()
	w.resetIdleProbe()
	w.clearAwaitingContinuation()
	// Drop old-client calls, but keep the active lane fenced until its result returns.
	w.rpcOperations = nil
	sessionID := w.sessionID
	generation := w.binding.Generation
	// Disable the old client's event and Done channels before closing it, so an
	// intentional release cannot be mistaken for an unexpected exit.
	client := w.client
	w.runtimeStopped()
	if client != nil {
		clientID := client.ID()
		if w.steerFence.active || w.research.root || w.research.disablePending || w.controlOp == controlAbort {
			if err := client.TerminateNow(); err != nil {
				w.b.fail(err)
				w.cancel()
			}
		} else {
			client.Close()
		}
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
	if w.migrationRuntimeCancel != nil {
		w.migrationRuntimeCancel()
		w.migrationRuntimeCancel = nil
	}
	if w.steerFence.active {
		w.settleOutstandingSteers()
	}
	w.previewID = 0
	w.previewStopToken = ""
	w.cancelSteerProbe()
	w.sessionRuntimeStarting.Store(false)
	w.sessionRuntimeActive.Store(false)
}

func (w *worker) releaseRuntime(releaseSlot bool) {
	w.releaseRuntimeWithReason(releaseSlot, "explicit")
}

func (w *worker) closeRuntime() { w.releaseRuntimeWithReason(true, "explicit") }

func (w *worker) closeFailedStart() {
	if w.runtimeResuming || w.restoring {
		w.releaseRuntimeWithReason(true, "failure")
		w.releaseSession()
		return
	}
	w.closeLogicalSession()
}

func (w *worker) ensureRuntime() (*omp.Client, error) {
	if w.migrationLocked() {
		return nil, newMessageError(i18n.Msg.SessionMigrationUnresolvedUseMigrateRecoverRecoverExplicitly)
	}
	if client, ok := w.runtimeClient(); ok {
		return client, nil
	}
	if w.runtime == runtimeStarting {
		return nil, newMessageError(i18n.Msg.OMPStartingTryAgainAfterReady)
	}
	if !w.binding.Running || w.binding.Session == "" || !filepath.IsAbs(w.binding.Session) || !filepath.IsAbs(w.binding.Workspace) {
		return nil, newMessageError(i18n.Msg.NoInstanceRunningConversationStartNewNameProjectPath)
	}
	w.beginRuntimeResume()
	failure := w.start(true, w.binding.Session, w.binding.Workspace, false)
	w.endRuntimeResume()
	if client, ok := w.runtimeClient(); ok {
		w.logRuntimeEvent(slog.LevelInfo, "runtime_resume", "lazy", "runtime resumed", w.sessionID)
		return client, nil
	}
	if w.ctx.Err() == nil {
		w.logRuntimeEvent(slog.LevelWarn, "restore_runtime_failed", "lazy", "runtime restore failed", w.sessionID)
	}
	if failure != "" {
		return nil, errors.New(failure)
	}
	return nil, newMessageError(i18n.Msg.FailedResumeReleasedOMPSessionNoPromptSubmitted)
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
	defer func() {
		w.cancel()
		w.background.Wait()
		w.drainMigrationResults()
		w.drainMediaResults()
		w.drainExportResults()
		w.drainDeleteResults()
	}()
	defer func() {
		w.teardownWorker(true)
		w.log.Info("worker stopped", "event", "worker_stop")
	}()
	if w.migration != nil {
		w.say(w.migrationStatus())
	}
	if w.startIntent != nil {
		kind := w.startIntent.Kind
		switch kind {
		case "new":
			kind = w.tr(i18n.Msg.New)
		case "resume":
			kind = w.tr(i18n.Msg.Resume)
		}
		w.say(w.tr(i18n.Msg.RequestedSessionStartInterruptedBeforeOmpIdentitySavedOutcome, kind))
	} else if w.restoring {
		w.restoreBinding()
	}
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	onTick := func() bool {
		now := time.Now()
		w.expireStartIntent(now)
		w.typing()
		w.flushPreview()
		w.expire(now)
		w.probeStuckTask(now)
		w.steeredTick(now)
		w.researchControlTick(now)
		w.releaseIdleRuntime(now)
		return w.evictIfIdle(now)
	}
	for {
		w.finishPendingStatusRelease()
		var events <-chan json.RawMessage
		var done <-chan struct{}
		if client, ok := w.runtimeClient(); ok && w.rpcExitPendingClientID != client.ID() {
			events = client.Events()
			done = client.Done()
		}
		// Prefer actor inputs and operation results over a continuously ready RPC event queue.
		select {
		case <-w.ctx.Done():
			return
		case request := <-w.forgetRequests:
			w.forgetBinding(request)
			continue
		case result := <-w.forgetResults:
			w.forgetFinished(result)
			continue
		case in := <-w.input:
			w.handle(in)
			w.dispatch()
			continue
		case <-done:
			w.failed()
			w.dispatch()
			continue
		case result := <-w.previewResult:
			w.previewFinished(result)
			w.dispatch()
			continue
		case result := <-w.nativeCatalog.results:
			w.commandCatalogFinished(result)
			w.dispatch()
			continue
		case result := <-w.migrationResults:
			w.migrationFinished(result)
			continue
		case result := <-w.operations:
			w.operationReturned(result)
			w.dispatch()
			continue
		case result := <-w.progressNative.results:
			w.progressNativeFinished(result)
			continue
		case result := <-w.topicRenameResults:
			w.topicRenameFinished(result)
			w.dispatch()
			continue
		case event := <-w.albumEvents:
			w.sealAlbum(event)
			w.dispatch()
			continue
		case result := <-w.mediaResults:
			w.preparedMedia(result)
			w.dispatch()
			continue
		case result := <-w.sendResults:
			w.preparedSend(result)
			w.dispatch()
			continue
		case result := <-w.resumeResults:
			w.resumeListed(result)
			w.dispatch()
			continue
		case result := <-w.exportResults:
			w.exportFinished(result)
			w.dispatch()
			continue
		case result := <-w.deleteResults:
			w.sessionDeleteFinished(result)
			w.dispatch()
			continue
		case result := <-w.bindingNameResults:
			w.bindingNamesLoaded(result)
			w.dispatch()
			continue
		case result := <-w.doctorResults:
			w.doctorFinished(result)
			w.dispatch()
			continue
		case result := <-w.idleProbe.results:
			w.idleProbeFinished(result, time.Now())
			w.dispatch()
			continue
		case <-tick.C:
			if onTick() {
				return
			}
			w.dispatch()
			continue
		default:
		}
		if events != nil {
			select {
			case raw, ok := <-events:
				if !ok {
					w.failed()
				} else {
					w.event(raw)
				}
				w.dispatch()
				continue
			default:
			}
		}
		select {
		case <-w.ctx.Done():
			return
		case request := <-w.forgetRequests:
			w.forgetBinding(request)
		case result := <-w.forgetResults:
			w.forgetFinished(result)
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
		case result := <-w.nativeCatalog.results:
			w.commandCatalogFinished(result)
		case result := <-w.operations:
			w.operationReturned(result)
		case result := <-w.topicRenameResults:
			w.topicRenameFinished(result)
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
		case result := <-w.deleteResults:
			w.sessionDeleteFinished(result)
		case result := <-w.migrationResults:
			w.migrationFinished(result)
		case result := <-w.bindingNameResults:
			w.bindingNamesLoaded(result)
		case result := <-w.doctorResults:
			w.doctorFinished(result)
		case result := <-w.idleProbe.results:
			w.idleProbeFinished(result, time.Now())
		case result := <-w.steerFence.probeResults:
			w.steeredQuiescenceFinished(result)
		case result := <-w.progressNative.results:
			w.progressNativeFinished(result)
		case <-tick.C:
			if onTick() {
				return
			}
		}
		w.dispatch()
	}
}

func (w *worker) operationFinished(result operationResult) {
	if result.kind == "native_control" {
		w.nativeControlFinished(result)
		return
	}
	if result.kind == "research_control" {
		w.researchControlFinished(result)
		if w.research.control == nil && w.client != nil {
			w.resolveSlashInputs()
		}
		return
	}
	if result.kind == "research_admission" {
		w.researchAdmissionFinished(result)
		return
	}
	if result.kind == "research_settlement" {
		w.researchSettlementFinished(result)
		return
	}
	if result.generation != w.binding.Generation || result.active != 0 && result.active != w.active || result.turn != 0 && result.turn != w.turn {
		return
	}
	if result.clientID != 0 && (w.client == nil || w.client.ID() != result.clientID) {
		return
	}
	if result.err != nil && w.log.Enabled(context.Background(), slog.LevelDebug) {
		attrs := []slog.Attr{
			slog.String("event", "request_failed"),
			slog.String("phase", "handled"),
			slog.Uint64("client_id", result.clientID),
			slog.Int64("generation", result.generation),
			slog.Uint64("turn", result.turn),
			slog.String("error_kind", omp.ClassifyError(result.err)),
		}
		if result.active != 0 {
			attrs = append(attrs, slog.Int64("inbox_id", result.active))
		}
		w.log.LogAttrs(context.Background(), slog.LevelDebug, "rpc request failed", attrs...)
	}
	if result.kind == "steer" {
		w.steerOperationFinished(result)
		w.probeResearchSettlement()
		return
	}
	if (result.kind == "prompt" || result.kind == "native_prompt") && w.research.root && result.err == nil {
		w.research.accepted = true
		if c := w.research.control; c != nil && c.intent == "local" {
			c.accepted = true
			w.researchControlMode(c, "postmode")
			w.touchBinding()
			return
		}
		// Only the canonical research handler requires correlated local completion.
		if w.research.command {
			w.touchBinding()
			return
		}
	}
	if result.kind == "prompt" || result.kind == "native_prompt" {
		if w.steerFence.active && result.err != nil {
			w.retireTaskRuntime(w.tr(i18n.Msg.Task.SubmissionUncertain))
			return
		}
		if errors.Is(result.err, omp.ErrRequestTooLarge) {
			w.finishCancelled(w.tr(i18n.Msg.PromptExceedsOmpRPCFrameLimitNoTaskSubmitted))
			return
		}
		if result.err != nil {
			notice := w.tr(i18n.Msg.SubmissionFailedOutcomeUncertainTaskWillNotReplayedAutomatically)
			w.clearQueue()
			w.retireTaskRuntime(notice)
			return
		}
		w.touchBinding()
		var response struct {
			AgentInvoked *bool `json:"agentInvoked"`
		}
		if json.Unmarshal(result.data, &response) == nil && response.AgentInvoked != nil && !*response.AgentInvoked {
			if result.kind == "native_prompt" {
				w.finishLocalCommandSubmission()
				return
			}
			if w.steerFence.active {
				w.deferSteeredTerminal(rpcEvent{})
			} else {
				w.finishTerminal(rpcEvent{})
			}
		}
		if response.AgentInvoked != nil && *response.AgentInvoked {
			w.activeInputKind = queuedPrompt
		}
		return
	}
	if strings.HasPrefix(result.kind, "model_") {
		w.modelOperationFinished(result)
		return
	}
	switch result.kind {
	case "handoff":
		w.completeSessionOperation()
		cancelled := result.cancelled
		uncertain := uncertainOperationOutcome(result.err, cancelled)
		if result.err == nil && !cancelled {
			var response *struct {
				SavedPath string `json:"savedPath"`
			}
			if err := json.Unmarshal(result.data, &response); err != nil {
				uncertain = true
			} else {
				cancelled = response == nil
				uncertain = cancelled
			}
		}
		if uncertain {
			w.releaseRuntimeWithReason(true, "failure")
		}
		if result.err != nil {
			if uncertain {
				w.say(w.tr(i18n.Msg.HandoffFailedOutcomeUncertainInstanceClosedWillResumeOn))
			} else {
				w.say(w.tr(i18n.Msg.OmpRejectedHandoff))
			}
		} else if cancelled {
			w.say(w.tr(i18n.Msg.HandoffCanceledWithoutResultInstanceClosedWillResumeOn))
		} else if uncertain {
			w.say(w.tr(i18n.Msg.HandoffResultCouldNotConfirmedInstanceClosedWillResume))
		} else {
			w.touchBinding()
			w.say(w.tr(i18n.Msg.HandoffCompleted))
		}
	case "compact", "":
		w.completeSessionOperation()
		uncertain := uncertainOperationOutcome(result.err, result.cancelled)
		if uncertain {
			w.releaseRuntimeWithReason(true, "failure")
		}
		var message string
		if result.err != nil {
			if uncertain {
				message = w.tr(i18n.Msg.CompactionFailedOutcomeUncertainInstanceClosedWillResumeOn)
			} else {
				message = w.tr(i18n.Msg.OmpRejectedCompaction)
			}
		} else if result.cancelled {
			message = w.tr(i18n.Msg.CompactionCanceledInstanceClosedWillResumeOnNextRequest)
		} else {
			w.touchBinding()
			var state statusState
			if json.Unmarshal(result.data, &state) != nil {
				state = statusState{}
			}
			if state.SessionID != w.sessionID {
				if state.SessionID != "" {
					// Resume the saved binding before another request can use this runtime.
					w.releaseRuntimeWithReason(true, "failure")
				}
				state = statusState{}
			}
			message = w.tr(i18n.Msg.CompactionCompletedContext, state.contextText(w.b.language()))
		}
		messageID, _ := result.meta.(int64)
		if !w.finishMenu(messageID, message) {
			w.say(message)
		}
	case "abort":
		if result.err != nil || result.cancelled {
			if uncertainOperationOutcome(result.err, result.cancelled) {
				w.releaseRuntimeWithReason(true, "failure")
				if w.active != 0 {
					w.finishUncertain(w.tr(i18n.Msg.AbortOutcomeUncertainActiveTaskWillNotReplayedAutomatically))
				}
			}
			w.endControlOperation()
			w.say(w.tr(i18n.Msg.AbortRequestFailed))
		} else {
			w.completeAbort(true, result.message)
		}
	case "abort_state":
		idle, settled, pending := rpcIdleState(result.data)
		for _, c := range w.confirms {
			if c.action == "ui" {
				pending = true
				break
			}
		}
		pending = pending || len(w.hostRequests) != 0 || w.client.BufferedOutput()
		if result.err == nil && !result.cancelled && idle && settled && !pending && !w.taskActive() && !w.taskOrSessionBusy() && !w.compacting {
			w.completeAbort(false, w.tr(i18n.Msg.NoActiveTaskQueuedPromptsCleared))
		} else {
			client := w.client
			w.startOperation("abort", client, func(ctx context.Context) (json.RawMessage, error) {
				return client.Call(ctx, "abort", nil)
			}, 0, result.message)
		}
	case "status":
		w.endControlOperation()
		var state statusState
		valid := result.err == nil && !result.cancelled && json.Unmarshal(result.data, &state) == nil
		if mode, ok := result.meta.(*statusModeResult); ok && mode != nil && mode.sessionID != "" {
			catalog := w.client.CommandCatalog()
			if mode.epoch == catalog.Epoch && mode.sessionID == catalog.SessionID {
				w.research.known = mode.known
				if mode.known {
					w.research.enabled = mode.enabled
				}
			}
		}
		if !valid {
			w.releaseStatusRuntime(result, false)
			w.say(w.tr(i18n.Msg.FailedReadSessionState))
			return
		}
		if state.SessionID == w.binding.SessionID {
			state.ConfiguredThinking = w.configuredThinking
		}
		research := w.researchStatus()
		w.releaseStatusRuntime(result, true)
		home, _ := os.UserHomeDir()
		w.say(formatRuntimeStatus(w.b.language(), state, w.binding.Workspace, w.sessionID, home, len(w.queue), time.Since(w.lastActivity), w.runtime == runtimeConnected) + research)
	case "name":
		w.endControlOperation()
		if result.err != nil {
			w.say(w.tr(i18n.Msg.SessionNameChangeCouldNotConfirmedUseStatusCheck))
			return
		}
		w.touchBinding()
		w.say(result.message)
	default:
		w.endControlOperation()
	}
}

// teardownWorker releases runtime/session resources and settles unconfirmed work.
// Callers reusing the worker must clear its task fields separately.
func (w *worker) teardownWorker(releaseSlot bool) {
	if w.migrationRun != nil {
		w.cancelMigration("shutdown", false)
	}
	w.flushReplyStatus()
	w.interruptModelMenu()
	w.b.removeCommandMenu(w)
	completion := w.taskCompletionAttrs(w.active, "uncertain")
	w.stopTyping()
	w.resetIdleProbe()
	w.cancelResumeList()
	w.forgetConfirmation = nil
	if w.deleteCancel != nil {
		w.deleteCancel()
	}
	w.cancelBindingNameLookup()
	if w.doctorCancel != nil {
		w.doctorCancel()
		w.doctorCancel = nil
	}

	w.clearQueue()
	w.clearAlbums()
	w.cancelHostRequests()
	w.progress = progressState{}
	w.progressSuppressed = false
	w.turn++
	w.finishing = false
	w.compacting = false
	w.endSessionOperation()
	w.endControlOperation()
	w.preview = ""
	w.finalizedStreamBytes = 0
	w.activeReplyTo = 0
	w.stream.Reset()
	w.resetAssistantResult()
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
	w.finishActiveIncoming()
	w.clearSteerFence()
	w.research = autoresearchState{}
}

func (w *worker) closeLogicalSession() bool {
	if w.migrationLocked() {
		w.cancelMigration("closed", false)
		return true
	}
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
	w.clearTask()
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
	if w.migrationLocked() {
		w.cancelMigration("runtime_exit", true)
		return
	}
	w.flushReplyStatus()
	if w.ctx.Err() != nil {
		return
	}
	if w.runtime == runtimeReleased && w.client == nil {
		return
	}
	clientID := uint64(0)
	if w.client != nil {
		clientID = w.client.ID()
	}
	if clientID != 0 && w.rpcOperationActive && w.rpcOperationClientID == clientID {
		if w.rpcExitPendingClientID != clientID {
			w.rpcExitPendingClientID = clientID
			w.logRuntimeEvent(slog.LevelWarn, "runtime_exit", "failure", "runtime exited", w.sessionID)
		}
		return
	}
	if clientID != 0 && w.rpcExitPendingClientID != clientID {
		w.logRuntimeEvent(slog.LevelWarn, "runtime_exit", "failure", "runtime exited", w.sessionID)
	}
	w.rpcExitPendingClientID = 0
	if w.research.root {
		w.retireResearch(w.tr(i18n.Msg.OmpExitedDuringAutoresearchTaskOutcomeUncertainWillNot))
		return
	}
	if w.steerFence.active {
		w.retireTaskRuntime(w.tr(i18n.Msg.OmpExitedWhileSteeringTaskOutcomeUncertainWillNot))
		return
	}
	w.clearQueue()
	w.retireTaskRuntime(w.tr(i18n.Msg.OmpExitedTaskOutcomeUncertainWillNotReplayedAutomatically))
}
func (w *worker) cancelQueuedTask(id int64) bool {
	for i := range w.queue {
		if w.queue[i].id != id {
			continue
		}
		q := w.queue[i]
		if probe := w.research.admission; probe != nil && probe.inbox == id {
			w.invalidateResearchAdmission()
		}
		if c := w.research.control; c != nil && c.intent == "route" && c.queued && c.inbox == id {
			w.endResearchControl(c)
		}
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
		w.b.discardIncoming(q.directory, w.mediaTaskLogger(q.id))
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
	if w.migrationLocked() {
		w.cancelMigration("cancelled", true)
		return
	}
	if w.nativeControl != nil {
		w.clearQueue()
		w.stopNativeControl(w.tr(i18n.Msg.AbortRequestedQueuedPromptsCleared))
		return
	}
	if w.research.control != nil {
		w.handleResearchStop()
		w.clearQueue()
		return
	}
	// A pending lookup cannot prove mode is off. Stop it on a fresh runtime
	// rather than starting another blocking lookup behind the canceled one.
	pendingMode := w.research.admission != nil && !w.research.admission.ready && w.researchAvailable(w.research.admission.client)
	w.clearQueue()
	if pendingMode {
		w.retireResearch(w.tr(i18n.Msg.AutoresearchAdmissionInterruptedTaskOutcomeUncertainWillNotReplayed))
		w.disableResearch(0)
		return
	}
	if w.handleResearchStop() {
		return
	}
	if w.steerFence.active {
		w.stopSteeredRoot()
		return
	}
	w.requestAbort(w.tr(i18n.Msg.AbortRequestedQueuedPromptsCleared))
}

func (w *worker) stopActiveTask() {
	if w.nativeControl != nil {
		w.stopNativeControl(w.tr(i18n.Msg.AbortRequestedCurrentTaskQueuedPromptsWillContinue))
		return
	}
	if w.handleResearchStop() {
		return
	}
	if w.steerFence.active {
		w.stopSteeredRoot()
		return
	}
	w.requestAbort(w.tr(i18n.Msg.AbortRequestedCurrentTaskQueuedPromptsWillContinue))
}

func (w *worker) requestAbort(notice string) {
	if w.runtime == runtimeReleased {
		w.say(w.tr(i18n.Msg.NoActiveTaskQueuedPromptsCleared))
		return
	}
	if w.controlInProgress() {
		w.say(w.tr(i18n.Msg.SessionOperationStillLoading))
		return
	}
	client, connected := w.runtimeClient()
	if !connected {
		w.say(w.tr(i18n.Msg.AbortRequestFailed))
		return
	}
	w.beginControlOperation(controlAbort)
	if !w.taskActive() {
		w.startOperation("abort_state", client, func(ctx context.Context) (json.RawMessage, error) {
			probeCtx, cancel := context.WithTimeout(ctx, idleProbeTimeout)
			defer cancel()
			return client.Call(probeCtx, "get_state", nil)
		}, 0, notice)
		return
	}
	w.startOperation("abort", client, func(ctx context.Context) (json.RawMessage, error) {
		return client.Call(ctx, "abort", nil)
	}, 0, notice)
}

func (w *worker) completeAbort(retire bool, notice string) {
	if retire {
		// Abort acknowledgements do not fence native asynchronous continuations.
		w.releaseRuntimeWithReason(true, "explicit")
		w.completeSessionOperation()
	}
	w.endControlOperation()
	if retire {
		if w.taskActive() {
			if !w.finishCancelled(w.tr(i18n.Msg.TaskCancelledBeforeCompletion)) {
				return
			}
		} else {
			w.cancelHostRequests()
			w.clearTaskConfirmations()
		}
	}
	w.touchBinding()
	w.say(notice)
}

func (w *worker) startOperation(kind string, client *omp.Client, call func(context.Context) (json.RawMessage, error), active int64, message string, metadata ...any) {
	w.statusReleaseClientID = 0
	if kind == "steer" {
		w.invalidateResearchSettlement()
	}
	request := rpcOperation{
		generation: w.binding.Generation,
		active:     active,
		turn:       w.turn,
		clientID:   client.ID(),
		kind:       kind,
		call:       call,
		message:    message,
	}
	if len(metadata) > 0 {
		request.meta = metadata[0]
	}
	w.touchActivity()
	w.rpcOperations = append(w.rpcOperations, request)
	w.dispatchRPCOperation()
}

func (w *worker) dispatchRPCOperation() {
	if w.rpcOperationActive || w.rpcExitPendingClientID != 0 || len(w.rpcOperations) == 0 || w.ctx.Err() != nil {
		return
	}
	request := w.rpcOperations[0]
	w.rpcOperations[0] = rpcOperation{}
	w.rpcOperations = w.rpcOperations[1:]
	if len(w.rpcOperations) == 0 {
		w.rpcOperations = nil
	}
	w.rpcOperationActive = true
	w.rpcOperationClientID = request.clientID
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		timeout := 30 * time.Second
		if request.kind == "compact" || request.kind == "handoff" {
			timeout = 180 * time.Second
		}
		ctx, cancel := context.WithTimeout(w.ctx, timeout)
		defer cancel()
		data, err := request.call(ctx)
		result := operationResult{
			generation: request.generation,
			active:     request.active,
			turn:       request.turn,
			clientID:   request.clientID,
			kind:       request.kind,
			data:       data,
			message:    request.message,
			meta:       request.meta,
			err:        err,
		}
		select {
		case w.operations <- result:
		case <-w.ctx.Done():
		}
	}()
}

func (w *worker) operationReturned(result operationResult) {
	if result.kind == "reply_status" {
		w.replyStatusFinished(result)
		return
	}
	w.rpcOperationActive = false
	w.rpcOperationClientID = 0
	pendingExit := result.clientID != 0 && w.rpcExitPendingClientID == result.clientID
	w.operationFinished(result)
	if pendingExit {
		if client, connected := w.runtimeClient(); connected && client.ID() == result.clientID {
			w.failed()
		} else {
			w.rpcExitPendingClientID = 0
		}
	}
	w.dispatchRPCOperation()
}

func (w *worker) beginControl(kind string, call func(*omp.Client, context.Context) (json.RawMessage, error), message string) bool {
	if w.controlInProgress() {
		w.say(w.tr(i18n.Msg.SessionOperationStillLoading))
		return false
	}
	client, err := w.ensureRuntime()
	if err != nil {
		w.say(w.errorText(err))
		return false
	}
	switch kind {
	case "status":
		w.beginControlOperation(controlStatus)
	case "name":
		w.beginControlOperation(controlName)
	}
	w.startOperation(kind, client, func(ctx context.Context) (json.RawMessage, error) {
		return call(client, ctx)
	}, 0, message)
	return true
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
		return "", newMessageError(i18n.Msg.FailedReadSessionBinding)
	}
	if !filepath.IsAbs(old.Workspace) {
		return "", newMessageError(i18n.Msg.CannotDetermineWorkingDirectoryUseNewNameProjectPath)
	}
	return old.Workspace, nil
}

func (w *worker) start(resume bool, target, expectedCWD string, replace bool) string {
	return w.reportStartFailure(w.startInternal(resume, target, expectedCWD, replace, 0, false, 0))
}

func (w *worker) startFencedWithArgs(resume bool, target, expectedCWD string, replace bool, generation int64, epoch uint64, args []string) {
	w.reportStartFailure(w.startInternalWithArgs(resume, target, expectedCWD, replace, generation, true, epoch, &args))
}

func (w *worker) reportStartFailure(message string) string {
	if message != "" && !w.runtimeResuming {
		w.say(message)
	}
	return message
}

func (w *worker) renameNewTopic(workspace string) {
	if w.key.thread == 0 || w.b.tg == nil {
		return
	}
	name := clipUTF16(menuText(filepath.Base(filepath.Clean(workspace)), 128), 128)
	if name == "" {
		return
	}
	if w.topicRenameResults == nil {
		w.topicRenameResults = make(chan topicRenameResult, 1)
	}
	generation, chat, thread := w.binding.Generation, w.key.chat, w.key.thread
	tg, results, workerCtx := w.b.tg, w.topicRenameResults, w.ctx
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		ctx, cancel := context.WithTimeout(workerCtx, 10*time.Second)
		defer cancel()
		err := tg.EditForumTopic(ctx, chat, thread, name)
		select {
		case results <- topicRenameResult{generation: generation, err: err}:
		case <-workerCtx.Done():
		}
	}()
}

func (w *worker) topicRenameFinished(result topicRenameResult) {
	if result.generation != w.binding.Generation || result.err == nil {
		return
	}
	info := telegram.ClassifyError(result.err)
	w.log.Warn("new topic rename failed", "event", "topic_rename_failed", "error_kind", info.Reason)
	w.say(w.tr(i18n.Msg.SessionStartedButTelegramTopicTitleCouldNotUpdated))
}

func (w *worker) startInternal(resume bool, target, expectedCWD string, replace bool, expectedGeneration int64, fenced bool, expectedEpoch uint64) string {
	return w.startInternalWithArgs(resume, target, expectedCWD, replace, expectedGeneration, fenced, expectedEpoch, nil)
}

func (w *worker) startInternalWithArgs(resume bool, target, expectedCWD string, replace bool, expectedGeneration int64, fenced bool, expectedEpoch uint64, argsOverride *[]string) string {
	if w.migrationLocked() {
		return w.tr(i18n.Msg.SessionMigrationUnresolvedCannotCreateSwitchBindings)
	}
	if _, connected := w.runtimeClient(); connected && !replace {
		return w.tr(i18n.Msg.InstanceAlreadyRunningUseNewFreshSessionCloseBefore)
	}
	if w.runtime == runtimeStarting {
		return w.tr(i18n.Msg.OMPAlreadyStarting)
	}
	if w.startIntent != nil && !w.restoring {
		return w.tr(i18n.Msg.PreviousSessionStartStillUncertainUseCloseBeforeStarting)
	}
	if (w.exportCancel != nil || w.deleteCancel != nil) && !w.restoring {
		return w.tr(i18n.Msg.OmpSessionOperationStillLoadingWaitFinish)
	}
	old, e := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return w.tr(i18n.Msg.FailedReadSession)
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
			if !savedSessionAvailable(target, expectedCWD) {
				return w.tr(i18n.Msg.Session.RestoreFailed)
			}
			cwd = expectedCWD
		} else if !validSessionID(target) {
			return w.tr(i18n.Msg.UsageResumeResumeOmpSessionID)
		}
		if !w.restoring {
			cwd = expectedCWD
		}
		if !w.runtimeResuming && w.b.sessionInUseByOther(w, target) {
			return w.tr(i18n.Msg.SessionAlreadyRunningAnotherConversationCloseInstanceFirstUse)
		}
		session = target
	} else {
		cwd = target
		if !filepath.IsAbs(cwd) {
			return w.tr(i18n.Msg.InvalidWorkingDirectory)
		}
	}
	reuseSlot := replace && w.runtime == runtimeConnected && w.client != nil
	reserved := false
	if !reuseSlot {
		select {
		case w.b.slots <- struct{}{}:
			reserved = true
		default:
			return w.tr(i18n.Msg.InstanceLimitReached)
		}
	}
	if !resume {
		if e = os.MkdirAll(cwd, 0700); e != nil {
			if reserved {
				<-w.b.slots
			}
			return w.tr(i18n.Msg.FailedCreateWorkingDirectory)
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
		w.b.sessionMu.Lock()
		if resume && fenced && expectedEpoch != w.b.bindingsEpoch.Load() {
			w.b.sessionMu.Unlock()
			if reserved {
				<-w.b.slots
			}
			return w.tr(i18n.Msg.SessionListChangedUseResumeAgain)
		}
		if resume && w.b.deleteMatchesLocked(session) {
			w.b.sessionMu.Unlock()
			if reserved {
				<-w.b.slots
			}
			return w.tr(i18n.Msg.SessionDeletedTryAgainAfterOperationFinishes)
		}
		e = w.b.db.PrepareStart(previous, intent)
		w.b.sessionMu.Unlock()
		if e != nil {
			if reserved {
				<-w.b.slots
			}
			return w.tr(i18n.Msg.FailedSaveStartupIntent, e.Error())
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
			w.clearTask()
		}
	}
	if resume && (w.runtimeResuming || w.restoring) && !w.beginSessionRestore(w.binding.SessionID) {
		if reserved {
			<-w.b.slots
		}
		return w.tr(i18n.Msg.SavedSessionDeletedUsedElsewhereNoOMPProcessStarted)
	}
	w.beginRuntimeStart()
	var launchArgs []string
	if argsOverride == nil {
		launchArgs = w.b.selectedArgsSnapshot()
	} else {
		launchArgs = *argsOverride
	}
	c, e := omp.Start(w.ctx, omp.Config{Binary: w.b.cfg.OMP, CWD: cwd, Resume: session, Args: launchArgs, Environment: w.b.cfg.OMPEnvironment}, w.b.rpcLog)
	if e != nil {
		if reserved {
			<-w.b.slots
		}
		w.runtimeStopped()
		if w.runtimeResuming || w.restoring {
			w.releaseSession()
		} else {
			w.sessionRuntimeStarting.Store(false)
			w.sessionRuntimeActive.Store(false)
		}
		if w.runtimeResuming {
			return w.tr(i18n.Msg.FailedResumeReleasedOMPSessionNoPromptSubmitted)
		}
		if resume {
			return w.tr(i18n.Msg.FailedResumeOmpStartupOutcomeUncertainUseCloseBefore)
		}
		return w.tr(i18n.Msg.FailedStartOmpStartupOutcomeUncertainUseCloseBefore)
	}
	w.runtimeStarted(c, launchArgs)
	w.initMedia()
	ctx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
	info, e := c.SessionInfo(ctx)
	cancel()
	if e != nil {
		w.logRuntimeEvent(slog.LevelError, "runtime_identity_invalid", "failure", "runtime session identity unavailable", "")
		w.closeFailedStart()
		return w.tr(i18n.Msg.CannotObtainOmpSessionIdentityWorkingDirectoryInstanceClosed)
	}
	if resume && !w.restoring && !strings.HasPrefix(strings.ToLower(info.ID), strings.ToLower(target)) {
		w.closeFailedStart()
		return w.tr(i18n.Msg.ResumedSessionDidNotMatchRequestedOmpSessionID)
	}
	if w.restoring && !sameSessionFile(info.File, target) {
		w.closeFailedStart()
		return w.tr(i18n.Msg.OmpDidNotRestoreSavedSessionFileInstanceClosed)
	}
	if w.runtimeResuming && !strings.EqualFold(info.ID, old.SessionID) {
		w.closeFailedStart()
		return w.tr(i18n.Msg.OmpDidNotRestoreSavedSessionIdentityInstanceClosed)
	}
	if !resume {
		expectedCWD = cwd
	}
	if expectedCWD != "" && !sameWorkspace(info.CWD, expectedCWD) {
		w.closeFailedStart()
		return w.tr(i18n.Msg.OmpSelectedDifferentWorkingDirectoryInstanceClosed)
	}
	if !w.claimSession(info.File, info.ID) {
		w.closeFailedStart()
		return w.tr(i18n.Msg.OmpSessionUnavailableAlreadyUseAnotherConversation)
	}
	ctx, cancel = context.WithTimeout(w.ctx, 3*time.Second)
	configuredThinking, thinkingErr := c.ConfiguredThinking(ctx)
	cancel()
	if thinkingErr != nil {
		if c.Stopping() {
			w.closeFailedStart()
			return w.tr(i18n.Msg.CannotQueryOmpThinkingConfigurationInstanceClosed)
		}
		configuredThinking = ""
	}
	ctx, cancel = context.WithTimeout(w.ctx, 30*time.Second)
	_, e = c.Call(ctx, "set_host_tools", map[string]any{"tools": telegramSendTools})
	cancel()
	if e != nil {
		w.closeFailedStart()
		return w.tr(i18n.Msg.CannotRegisterTelegramAttachmentDeliveryInstanceClosed)
	}
	binding := store.Binding{Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread, Workspace: info.CWD, Session: info.File, SessionID: info.ID, Generation: nextGeneration, LastUsedAt: old.LastUsedAt, Running: true}

	w.b.sessionMu.Lock()
	if w.runtimeResuming {
		binding = old
	} else {
		e = w.b.db.CommitStart(binding)
	}
	w.b.sessionMu.Unlock()
	if e != nil {
		w.b.storeLog.Error("session binding persistence failed", "event", "binding_write_failed", "reason", "commit_start", "error_kind", "persistence")
		w.closeFailedStart()
		return w.tr(i18n.Msg.FailedSaveSessionBindingInstanceClosed)
	}
	w.binding = binding
	w.configuredThinking = configuredThinking
	w.startIntent = nil
	w.preview, w.lastPreview = "", ""
	w.previewID = 0
	w.touchActivity()
	if !w.runtimeResuming {
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
	w.refreshCommandCatalog(c)
	if renameTopic {
		w.renameNewTopic(info.CWD)
	}
	if !w.runtimeResuming {
		w.say(w.tr(i18n.Msg.OmpReadyWorkspaceSession, info.CWD, info.ID))
	}
	return ""
}
func (w *worker) handle(in incoming) {
	w.flushReplyStatus()
	w.statusReleaseClientID = 0
	w.touchLogicalActivity()
	if w.handleMigrationIncoming(in) {
		return
	}
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
			if w.ctx.Err() != nil {
				return
			}
			w.say(w.errorText(err))
			w.mark(in.id, "done")
			return
		}
		if w.pendingInputFull() {
			w.say(w.tr(i18n.Msg.Queue.Full))
			if w.mark(in.id, "cancelled") {
				w.log.Warn("task queue rejected", "event", "queue_rejected", "inbox_id", in.id, "reason", "worker_queue_full")
			}
			return
		}
		w.queueMedia(in)
		return
	}
	if !strings.HasPrefix(text, "/") {
		if w.taskActive() {
			if _, err := w.ensureRuntime(); err != nil {
				if w.ctx.Err() == nil {
					w.say(w.errorText(err))
					w.mark(in.id, store.InboxCancelled)
				}
				return
			}
		}
		w.routePrompt(in, text)
		return
	}
	cmd, tail, foreign := parseSlashInvocation(text, w.b.bot.Username)
	if foreign {
		w.mark(in.id, "ignored")
		return
	}
	if !knownBridgeCommand(cmd) {
		w.routeSlashInput(in, text)
		return
	}
	arg := slashCommandArguments(tail)
	if cmd == "/fast" {
		text := "/fast"
		if arg != "" {
			text += " " + arg
		}
		w.beginNativeControl(in, text)
		return
	}
	if cmd == "/review" {
		prompt := "/review"
		if arg != "" {
			prompt += " " + arg
		}
		w.enqueueReviewPrompt(in, prompt)
		return
	}
	if cmd == "/followup" {
		if arg == "" {
			w.say(w.tr(i18n.Msg.UsageFollowupMessage))
			w.mark(in.id, "done")
			return
		}
		w.enqueuePrompt(in, arg)
		return
	}
	if !w.mark(in.id, "submitted") {
		return
	}
	defer w.mark(in.id, "done")
	switch cmd {
	case "/help", "/start":
		w.say(w.commandHelp())
	case "/lang":
		w.handleLanguageCommand(arg)
	case "/new":
		workspace, err := w.newWorkspace(arg)
		if err != nil {
			w.say(w.errorText(err))
			return
		}
		if w.binding.Running {
			w.confirm(confirmation{action: "new", workspace: workspace, user: in.msg.From.ID}, w.tr(i18n.Msg.StartNewOmpSessionExistingFilesSessionHistoryWill, workspace), []string{w.tr(i18n.Msg.Confirm), w.tr(i18n.Msg.Cancel)})
		} else {
			w.start(false, workspace, "", false)
		}
	case "/resume":
		if arg == "" {
			w.requestResumeList(in.msg.From.ID)
		} else {
			w.start(true, arg, "", true)
		}
	case "/args":
		w.handleArgsCommand(arg, in.msg.From.ID)
	case "/export":
		format, sessionID, ok := parseExportArgs(arg)
		if !ok {
			w.say(w.tr(i18n.Msg.UsageExportHtmlSessionId))
			return
		}
		if sessionID == "" {
			w.requestExportList(in.msg.From.ID, format)
		} else {
			w.requestDirectExport(in.msg.From.ID, format, sessionID)
		}
	case "/bindings":
		if arg != "" && arg != "old" {
			w.say(w.tr(i18n.Msg.UsageBindingsOld))
			return
		}
		w.showBindings(in.msg.From.ID, arg == "old", 0)
	case "/close":
		if !w.closeLogicalSession() {
			return
		}
		w.say(w.tr(i18n.Msg.InstanceClosedSessionPreserved))
	case "/stop":
		w.stop()
	case "/queue":
		if arg != "" {
			w.say(w.tr(i18n.Msg.UsageQueue))
			return
		}
		w.showQueue(in.msg.From.ID)
	case "/status":
		w.status()
	case "/doctor":
		if arg != "" {
			w.say(w.tr(i18n.Msg.UsageDoctor))
			return
		}
		w.runDoctor()
	case "/name":
		if arg == "" {
			w.say(w.tr(i18n.Msg.UsageNameSessionTitle))
			return
		}
		if w.exportingSession != "" {
			w.say(w.tr(i18n.Msg.WaitCurrentSessionExportFinishBeforeChangingName))
			return
		}
		name := menuText(arg, 160)
		w.beginControl("name", func(client *omp.Client, ctx context.Context) (json.RawMessage, error) {
			return client.Call(ctx, "set_session_name", map[string]any{"name": arg})
		}, w.tr(i18n.Msg.SessionNamed, name))
	case "/model":
		if arg == "" {
			w.showModelPicker(in.msg.From.ID)
			return
		}
		provider, model, ok := strings.Cut(arg, "/")
		if !ok || provider == "" || model == "" {
			w.say(w.tr(i18n.Msg.UsageModelModelProviderModel))
			return
		}
		w.switchModel(provider, model)
	case "/thinking":
		if arg != "" {
			w.say(w.tr(i18n.Msg.UsageThinking))
			return
		}
		w.showThinkingPicker(in.msg.From.ID)
	case "/handoff":
		w.handoff(arg)
	case "/compact":
		if arg != "" {
			w.say(w.tr(i18n.Msg.UsageCompact))
			return
		}
		if w.sessionControlBusy() {
			w.say(w.tr(i18n.Msg.IdleInstanceRequired))
			return
		}
		client, err := w.ensureRuntime()
		if err != nil {
			w.say(w.errorText(err))
			return
		}
		w.beginSessionOperation(sessionOperationCompact)
		w.touchActivity()
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
		var messageID int64
		if message, err := w.b.tg.Send(ctx, w.key.chat, w.key.thread, w.tr(i18n.Msg.Compacting), telegram.SendOptions{}); err == nil {
			messageID = message.MessageID
		}
		cancel()
		w.startOperation("compact", client, func(ctx context.Context) (json.RawMessage, error) {
			if _, err := client.Call(ctx, "compact", nil); err != nil {
				return nil, err
			}
			stateCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			// Usage lookup cannot turn confirmed compaction into a failure.
			state, err := client.Call(stateCtx, "get_state", nil)
			if err != nil {
				return nil, nil
			}
			return state, nil
		}, 0, "", messageID)
	}
}
func (w *worker) handoff(instructions string) {
	if w.sessionControlBusy() || len(w.queue) != 0 {
		w.say(w.tr(i18n.Msg.WaitCurrentTaskQueueFinishBeforeHandoff))
		return
	}
	client, err := w.ensureRuntime()
	if err != nil {
		w.say(w.errorText(err))
		return
	}
	var fields map[string]any
	if instructions != "" {
		fields = map[string]any{"customInstructions": instructions}
	}
	w.beginSessionOperation(sessionOperationHandoff)
	w.touchActivity()
	w.startOperation("handoff", client, func(ctx context.Context) (json.RawMessage, error) {
		return client.Call(ctx, "handoff", fields)
	}, 0, "")
	w.say(w.tr(i18n.Msg.HandoffRequested))
}
func (w *worker) status() {
	if w.controlInProgress() {
		w.say(w.tr(i18n.Msg.SessionOperationStillLoading))
		return
	}
	w.b.sessionMu.Lock()
	occupied := w.binding.SessionID != "" && w.b.sessionMatchesLocked(w, w.binding.SessionID)
	w.b.sessionMu.Unlock()
	if occupied {
		w.say(w.tr(i18n.Msg.SavedSessionUseAnotherConversationRunStatusThereClose))
		return
	}
	temporary := w.runtime == runtimeReleased && w.binding.Running
	client, connected := w.runtimeClient()
	if !connected {
		if !temporary {
			n, _ := w.b.db.Uncertain()
			w.say(w.tr(i18n.Msg.NoInstanceRunningGlobalUncertainRecordsUseResumeRestore, n))
			return
		}
		var err error
		client, err = w.ensureRuntime()
		if err != nil {
			w.say(w.errorText(err))
			return
		}
	}
	var mode *statusModeResult
	queryResearch := w.researchAvailable(client)
	if queryResearch || temporary {
		mode = &statusModeResult{temporary: temporary}
	}
	w.beginControlOperation(controlStatus)
	w.startOperation("status", client, func(ctx context.Context) (json.RawMessage, error) {
		if queryResearch {
			catalog := client.CommandCatalog()
			mode.epoch, mode.sessionID = catalog.Epoch, catalog.SessionID
			modeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			enabled, err := client.AutoresearchMode(modeCtx)
			cancel()
			mode.known, mode.enabled = err == nil, enabled
		}
		return client.Call(ctx, "get_state", nil)
	}, 0, "", mode)
	if temporary {
		// Arm before the actor can consume events; the result must not rearm it.
		w.statusReleaseClientID = client.ID()
	}
}

// promptInlineCount reserves the longest possible uint64 request ID before dispatch.
func promptInlineCount(message string, images []media.Image, limit int) (int, bool) {
	base, _ := json.Marshal(struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Message string `json:"message"`
	}{Type: "prompt", ID: "18446744073709551615", Message: message})
	used := len(base) + 1 // Newline framing.
	if used > limit {
		return 0, false
	}
	used += len(`,"images":[]`)
	for i, image := range images {
		withoutData := image
		withoutData.Data = ""
		encoded, _ := json.Marshal(withoutData)
		additional := len(encoded) + len(image.Data) // Base64 needs no JSON escaping.
		if i > 0 {
			additional++
		}
		if used+additional > limit {
			return i, true
		}
		used += additional
	}
	return len(images), true
}

func (w *worker) dispatch() {
	if !w.canDispatch() {
		return
	}
	client, err := w.ensureRuntime()
	if err != nil {
		w.resumeFailed = true
		w.say(w.errorText(err))
		return
	}
	if w.queue[0].preparing {
		return
	}
	if !w.nativeCommandDispatchReady(client) {
		return
	}
	if !w.researchDispatchReady(client) {
		return
	}
	if !w.queuedCommandDispatchReady(client) {
		return
	}
	w.invalidateResearchAdmission()
	q := w.queue[0]
	inline, fits := promptInlineCount(q.text, q.images, client.FrameLimit())
	w.queue[0] = queued{}
	w.queue = w.queue[1:]
	if !fits {
		w.b.discardIncoming(q.directory, w.mediaTaskLogger(q.id))
		if w.mark(q.id, "failed") {
			w.logQueuedTaskComplete(q.id, "failed")
			w.say(w.tr(i18n.Msg.PromptExceedsOmpRPCFrameLimitNoTaskSubmitted))
		}
		return
	}
	if inline < len(q.images) {
		clear(q.images[inline:])
		q.images = q.images[:inline]
	}
	if !w.submit(q) {
		w.b.discardIncoming(q.directory, w.mediaTaskLogger(q.id))
		return
	}
	w.beginTask(q)
	w.stopTyping()
	w.resetIdleProbe()
	w.clearAwaitingContinuation()
	w.progress = progressState{StartedAt: time.Now(), ActiveTools: make(map[string]progressTool)}
	w.logTaskSubmit(q.id)
	w.progressSuppressed = false
	w.preview = ""
	w.stream.Reset()
	w.finalAssistantBytes = 0
	w.finalizedStreamBytes = 0
	w.finalOutputTruncated = false
	w.lastPreview = ""
	w.previewID = 0
	w.previewStopToken = ""
	fields := map[string]any{"message": q.text}
	if len(q.images) > 0 {
		fields["images"] = q.images
	}
	requestID := client.ReserveRequestID()
	w.rootRequestID = requestID
	kind := "prompt"
	if q.kind == queuedNativeCommand {
		kind = "native_prompt"
	}
	w.startOperation(kind, client, func(ctx context.Context) (json.RawMessage, error) {
		return client.CallWithID(ctx, requestID, "prompt", fields)
	}, q.id, "")
}

func (w *worker) enqueuePrompt(in incoming, text string) {
	w.enqueuePreparedPrompt(in, preparePromptText(in.msg, text), text)
}

func (w *worker) enqueueReviewPrompt(in incoming, review string) {
	w.enqueuePreparedPrompt(in, prepareReviewPrompt(in.msg, review), review)
	if last := len(w.queue) - 1; last >= 0 && w.queue[last].id == in.id {
		w.queue[last].bridgeReview = true
	}
}

func (w *worker) enqueuePreparedPrompt(in incoming, prompt, displayText string) {
	if _, err := w.ensureRuntime(); err != nil {
		if w.ctx.Err() != nil {
			return
		}
		w.say(w.errorText(err))
		w.mark(in.id, "done")
		return
	}
	if w.pendingInputFull() {
		w.say(w.tr(i18n.Msg.Queue.Full))
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

func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// Split only as much input as needed to fill the allowed Telegram pages.
func splitTaskText(text string, maxParts int) ([]string, bool) {
	size := min(len(text), 64<<10)
	for {
		parts := telegram.SplitForTelegram(truncateUTF8(text, size), telegram.MaxMessageUTF16)
		if len(parts) > maxParts {
			return parts[:maxParts+1], true
		}
		if size == len(text) {
			return parts, false
		}
		size = min(len(text), size*2)
	}
}

func fitTaskReplyBudget(language i18n.Language, parts []string, finalNotice string, truncated bool) []string {
	truncationNotice := i18n.Text(language, i18n.Msg.ReplyTruncated)
	totalBytes := len(finalNotice)
	for _, part := range parts {
		totalBytes += len(part)
	}
	if totalBytes > maxFinalReplyBytes {
		truncated = true
	}
	maxTextParts := maxFinalReplyMessages
	if finalNotice != "" {
		maxTextParts--
	}
	if truncated {
		maxTextParts--
		totalBytes += len(truncationNotice)
	}
	for len(parts) > maxTextParts || totalBytes > maxFinalReplyBytes {
		last := len(parts) - 1
		totalBytes -= len(parts[last])
		parts = parts[:last]
	}
	if truncated {
		parts = append(parts, truncationNotice)
	}
	if finalNotice != "" {
		parts = append(parts, finalNotice)
	}
	return parts
}

func taskReplies(language i18n.Language, text, finalNotice string, truncated bool) []string {
	if finalNotice != "" && !truncated && len(text)+len(finalNotice)+2 <= maxFinalReplyBytes {
		combined := finalNotice
		if text != "" {
			combined = text + "\n\n" + finalNotice
		}
		parts, overflow := splitTaskText(combined, maxFinalReplyMessages)
		if !overflow {
			var totalBytes int
			for _, part := range parts {
				totalBytes += len(part)
			}
			if totalBytes <= maxFinalReplyBytes {
				return parts
			}
		}
	}
	budget := maxFinalReplyBytes - len(i18n.Text(language, i18n.Msg.ReplyTruncated)) - len(finalNotice)
	if len(text) > maxFinalReplyBytes-len(finalNotice) || truncated && len(text) > budget {
		text = truncateUTF8(text, budget)
		truncated = true
	}
	maxTextParts := maxFinalReplyMessages
	if finalNotice != "" {
		maxTextParts--
	}
	if truncated {
		maxTextParts--
	}
	parts, overflow := splitTaskText(text, maxTextParts)
	if overflow && !truncated {
		truncated = true
		maxTextParts--
	}
	if len(parts) > maxTextParts {
		parts = parts[:maxTextParts]
	}
	return fitTaskReplyBudget(language, parts, finalNotice, truncated)
}

func (w *worker) assistantBudgetExceeded() {
	if w.research.root {
		w.retireResearch(w.tr(i18n.Msg.AutoresearchOutputExceededBridgeResourceBudgetOutcomeUncertainWill))
		return
	}
	w.logRuntimeEvent(slog.LevelWarn, "assistant_output_overflow", "resource_limit", "assistant output exceeded bridge budget", w.sessionID)
	if w.steerFence.active {
		w.retireTaskRuntime(w.tr(i18n.Msg.AssistantOutputBudgetExceeded))
		return
	}
	w.clearQueue()
	w.retireTaskRuntime(w.tr(i18n.Msg.AssistantOutputBudgetExceededSessionKept))
}

func (w *worker) resetAssistantResult() {
	w.lastAssistant = nil
	w.finalAssistantTexts, w.assistantRunStart = nil, 0
	w.finalAssistantBytes = 0
	w.finalOutputTruncated = false
}

func (w *worker) finish() {
	w.stopTyping()
	w.resetIdleProbe()
	w.clearAwaitingContinuation()
	// Progress state is finalized only after the durable result transaction.
	if w.active != 0 {
		taskID := w.active
		if err := w.b.db.CompleteInboxWithReplies(w.ctx, taskID, w.key.chat, w.key.thread, replyTaskReplies(w.b.language(), w.preview, w.replyStatus.header, w.finalOutputTruncated)); err != nil {
			if w.ctx.Err() == nil {
				w.b.storeLog.Error("final result commit failed", "event", "final_commit_failed")
				w.b.fail(err)
				w.cancel()
			}
			return
		}
		w.cancelHostRequests()
		w.logTaskComplete(taskID, "done")
		w.clearTask()
		w.preview = ""
		w.stream.Reset()
		w.finalizedStreamBytes = 0
		w.clearTaskConfirmations()
	} else if w.preview != "" {
		for _, part := range taskReplies(w.b.language(), w.preview, "", w.finalOutputTruncated) {
			w.say(part)
		}
	}
	w.resetAssistantResult()
	w.finishProgressWhenReady()
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
	w.clearAwaitingContinuation()
	if w.active == 0 {
		w.lastAssistant = nil
		w.finalAssistantTexts, w.assistantRunStart = nil, 0
		return false
	}
	taskID := w.active
	var err error
	switch state {
	case "uncertain":
		err = w.b.db.CompleteInboxUncertainWithReplies(w.ctx, taskID, w.key.chat, w.key.thread, taskReplies(w.b.language(), w.preview, notice, w.finalOutputTruncated))
	case "cancelled":
		err = w.b.db.CompleteInboxCancelledWithReplies(w.ctx, taskID, w.key.chat, w.key.thread, taskReplies(w.b.language(), w.preview, notice, w.finalOutputTruncated))
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
	w.cancelHostRequests()
	w.logTaskComplete(taskID, state)
	w.clearTask()
	w.preview = ""
	w.resetAssistantResult()
	w.stream.Reset()
	w.finalizedStreamBytes = 0
	w.clearTaskConfirmations()
	w.finishProgressWhenReady()
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

func boundedAssistantTexts(messages []message, priorTexts, currentTexts []string, limit int) (string, bool, bool) {
	var preview strings.Builder
	found := false
	appendText := func(text string) bool {
		if text == "" {
			return true
		}
		found = true
		separator := ""
		if preview.Len() != 0 {
			separator = "\n\n"
		}
		remaining := limit - preview.Len()
		if len(separator)+len(text) > remaining {
			if remaining > len(separator) {
				preview.WriteString(separator)
				preview.WriteString(truncateUTF8(text, remaining-len(separator)))
			}
			return false
		}
		preview.WriteString(separator)
		preview.WriteString(text)
		return true
	}
	for _, text := range priorTexts {
		if !appendText(text) {
			return preview.String(), found, true
		}
	}
	terminalFound := false
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		text := assistantText(m)
		if text == "" {
			continue
		}
		terminalFound = true
		if !appendText(text) {
			return preview.String(), found, true
		}
	}
	if !terminalFound {
		for _, text := range currentTexts {
			if !appendText(text) {
				return preview.String(), found, true
			}
		}
	}
	return preview.String(), found, false
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

const terminalFailureCredentialKeyPattern = `(?:authorization|api[_ -]?(?:key|token)|access[_ -]?token|refresh[_ -]?token|token|password|secret|client[_ -]?secret)`

var (
	terminalFailureDoubleQuotedCredentialPattern = regexp.MustCompile(`(?i)(["']?` + terminalFailureCredentialKeyPattern + `["']?\s*[:=]\s*")((?:\\.|[^"\\])*)(")`)
	terminalFailureSingleQuotedCredentialPattern = regexp.MustCompile(`(?i)(["']?` + terminalFailureCredentialKeyPattern + `["']?\s*[:=]\s*')((?:\\.|[^'\\])*)(')`)
	terminalFailureCredentialPattern             = regexp.MustCompile(`(?i)(["']?` + terminalFailureCredentialKeyPattern + `["']?\s*[:=]\s*)((?:Bearer|Basic|Token|ApiKey)\s+)?([^\s,;}\]"']+)`)
	terminalFailureIDPattern                     = regexp.MustCompile(`(?i)\b((?:request|trace|correlation)[_-]?id)\b\s*[:=]\s*[^\s,;]+`)
	terminalFailureURLPattern                    = regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s]+`)
	terminalFailureUnixPathPattern               = regexp.MustCompile(`(^|[\s(:])/(?:[^\s/,;]+/)+[^\s/,;]+`)
	terminalFailureWindowsPathPattern            = regexp.MustCompile(`(?i)\b[A-Z]:\\(?:[^\s\\]+\\)*[^\s,;]+`)
	terminalFailureSecretMarkerPattern           = regexp.MustCompile(`(?i)\bsecret\b`)
)

func containsUnkeyedSecretMarker(detail string) bool {
	for _, match := range terminalFailureSecretMarkerPattern.FindAllStringIndex(detail, -1) {
		suffix := strings.TrimLeft(detail[match[1]:], "\"' \t\r\n")
		if len(suffix) > 0 && (suffix[0] == ':' || suffix[0] == '=') {
			continue
		}
		return true
	}
	return false
}

func terminalFailureDetail(raw string) string {
	detail := menuText(raw, 512)
	if detail == "" {
		return ""
	}
	detail = terminalFailureDoubleQuotedCredentialPattern.ReplaceAllString(detail, "$1[redacted]$3")
	detail = terminalFailureSingleQuotedCredentialPattern.ReplaceAllString(detail, "$1[redacted]$3")
	detail = terminalFailureCredentialPattern.ReplaceAllString(detail, "$1$2[redacted]")
	if containsUnkeyedSecretMarker(detail) {
		return ""
	}
	detail = terminalFailureIDPattern.ReplaceAllString(detail, "$1=[redacted]")
	detail = terminalFailureURLPattern.ReplaceAllString(detail, "[redacted URL]")
	detail = terminalFailureUnixPathPattern.ReplaceAllString(detail, "$1[redacted path]")
	detail = terminalFailureWindowsPathPattern.ReplaceAllString(detail, "[redacted path]")
	return menuText(detail, 512)
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
		return w.tr(i18n.Msg.OmpReportedTaskRateLimitedModelProviderTaskOutcome)
	}
	if detail := terminalFailureDetail(m.ErrorMessage); detail != "" {
		return w.tr(i18n.Msg.OmpReportedTaskFailedBeforeProducingConfirmedResultReason, detail)
	}
	return w.tr(i18n.Msg.OmpReportedTaskFailedBeforeProducingConfirmedResultTask)
}

func (w *worker) finishTerminal(e rpcEvent) {
	switch w.terminalState(e) {
	case terminalUncertain:
		w.finishUncertain(w.terminalFailureNotice(e))
	case terminalCancelled:
		w.finishCancelled(w.tr(i18n.Msg.TaskCancelledBeforeCompletion))
	case terminalDone:
		if w.preview == "" {
			w.finishUncertain(w.tr(i18n.Msg.OmpEndedWithoutConfirmedTextResultTaskOutcomeUncertain))
			return
		}
		w.finishReplyWithStatus(false)
	}
}
func (w *worker) finishProgressWhenReady() {
	if w.previewBusy {
		w.finishing = true
		return
	}
	w.finishPreview()
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
		w.statusReleaseClientID = 0
		return
	}
	if w.migrationLocked() {
		if e.Type == "agent_start" || e.Type == "tool_execution_start" || e.Type == "host_tool_call" {
			w.cancelMigration("unexpected_activity", true)
		}
		return
	}
	if w.replyStatusEvent(e) {
		return
	}
	if w.statusReleaseClientID != 0 {
		switch e.Type {
		case "available_commands_update", "session_info_update", "advisor_cost_changed":
		case "extension_ui_request":
			// Startup widget updates do not wait for a native UI response.
			if e.Method != "setWidget" {
				w.statusReleaseClientID = 0
			}
		default:
			w.statusReleaseClientID = 0
		}
		if w.statusReleaseClientID != 0 && (w.client == nil || w.client.CommandCatalog().SessionID != w.binding.SessionID) {
			w.statusReleaseClientID = 0
		}
	}
	w.logLifecycle(e)
	if w.researchControlEvent(e) {
		return
	}
	switch e.Type {
	case "command_output":
		w.nativeCommandOutput(e.Text)
	case "thinking_level_changed":
		if e.Configured != nil {
			w.configuredThinking = *e.Configured
		} else {
			w.configuredThinking = e.ThinkingLevel
		}
	case "available_commands_update":
		w.commandCatalogUpdated()
	case "session_info_update":
		w.cancelProgressNative()
		// The reader invalidates the catalog before publishing session changes.
		w.syncCommandMenu()
		if client, ok := w.runtimeClient(); ok && client.CommandCatalog().State == omp.CatalogUnknown {
			w.refreshCommandCatalog(client)
		}
	case "host_tool_call":
		w.renameProgressTool(e.ToolCallID, e.ToolName)
		w.recordProgressToolPayload(e.ToolCallID, e.Type, raw)
		w.hostSend(e)
	case "host_tool_cancel":
		if cancel, ok := w.hostRequests[e.TargetID]; ok {
			cancel()
			delete(w.hostRequests, e.TargetID)
		}
	case "tool_execution_start":
		w.startProgressTool(e.ToolCallID, e.ToolName)
		w.recordProgressToolPayload(e.ToolCallID, e.Type, raw)
	case "tool_execution_update":
		w.recordProgressToolPayload(e.ToolCallID, e.Type, raw)
	case "tool_execution_end":
		w.recordProgressToolPayload(e.ToolCallID, e.Type, raw)
		w.endProgressTool(e.ToolCallID, e.IsError)
	case "message_update":
		if w.research.root && !w.research.roundOpen {
			return
		}
		if e.AssistantMessageEvent.Type == "text_delta" {
			delta := e.AssistantMessageEvent.Delta
			pendingBytes := w.stream.Len() - w.finalizedStreamBytes
			if len(delta) > maxAssistantOutputBytes-w.stream.Len() || len(delta) > maxAssistantOutputBytes-w.finalAssistantBytes-pendingBytes {
				w.assistantBudgetExceeded()
				return
			}
			w.stream.WriteString(delta)
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
		if w.research.root {
			w.invalidateResearchSettlement()
			w.research.roundOpen = true
			w.research.notices = 0
		}
		w.newSteeredRun()
		if !w.taskActive() {
			return
		}
		w.activeInputKind = queuedPrompt
		w.clearAwaitingContinuation()
		w.busy = true
		w.progress.ActiveTools = make(map[string]progressTool)
		w.lastAssistant = nil
		if w.research.root {
			w.finalAssistantTexts, w.assistantRunStart = nil, 0
			w.finalAssistantBytes = 0
		} else {
			w.assistantRunStart = len(w.finalAssistantTexts)
		}
		w.finalizedStreamBytes = w.stream.Len()
		w.routeDeferredResearchToggle()
	case "message_end":
		if w.research.root && !w.research.roundOpen {
			return
		}
		var m message
		if json.Unmarshal(e.Message, &m) == nil && m.Role == "assistant" {
			if w.taskActive() {
				w.replyStatus.metrics.observe(m)
			}
			w.lastAssistant = &terminalAssistant{
				StopReason:                 m.StopReason,
				ErrorMessage:               m.ErrorMessage,
				ErrorClassificationMessage: m.ErrorClassificationMessage,
				ErrorStatus:                m.ErrorStatus,
			}
			if text := assistantText(m); text != "" {
				if len(text) > maxAssistantOutputBytes-w.finalAssistantBytes {
					w.assistantBudgetExceeded()
					return
				}
				w.finalAssistantTexts = append(w.finalAssistantTexts, text)
				w.finalAssistantBytes += len(text)
			}
			w.finalizedStreamBytes = w.stream.Len()
		}
	case "agent_end":
		if w.research.root {
			terminal := w.terminalState(e)
			w.flushResearchRound(e)
			if terminal != terminalDone {
				w.retireResearch(w.tr(i18n.Msg.AutoresearchEndedInterruptedFailedRoundWillNotReplayedAutomatically))
			}
			return
		}
		if e.IsTerminal != nil && !*e.IsTerminal {
			text, found, truncated := boundedAssistantTexts(e.Messages, nil, nil, maxAssistantOutputBytes)
			if found {
				priorBytes := w.finalAssistantBytes
				for _, cached := range w.finalAssistantTexts[w.assistantRunStart:] {
					priorBytes -= len(cached)
				}
				if truncated || len(text) > maxAssistantOutputBytes-priorBytes {
					w.assistantBudgetExceeded()
					return
				}
				clear(w.finalAssistantTexts[w.assistantRunStart:])
				w.finalAssistantTexts = append(w.finalAssistantTexts[:w.assistantRunStart], text)
				w.finalAssistantBytes = priorBytes + len(text)
			}
			w.awaitTaskContinuation()
			return
		}
		w.clearAwaitingContinuation()
		text, found, truncated := boundedAssistantTexts(e.Messages, w.finalAssistantTexts[:w.assistantRunStart], w.finalAssistantTexts[w.assistantRunStart:], maxFinalReplyBytes)
		if found {
			w.preview = text
			w.finalOutputTruncated = truncated
		}
		if w.steerFence.active {
			w.deferSteeredTerminal(e)
			return
		}
		w.finishTerminal(e)
	case "prompt_result":
		if w.steerResult(e) {
			w.probeResearchSettlement()
			return
		}
		if e.ID != "" && e.ID != w.rootRequestID {
			return
		}
		if w.researchLocalResult(e) {
			return
		}
		if w.research.root {
			if e.Status == "aborted" || e.Status == "error" {
				w.retireResearch(w.tr(i18n.Msg.AutoresearchInterruptedFailedOutcomeUncertainWillNotReplayedAutomatically))
			}
			return
		}
		if w.steerFence.active {
			if e.AgentInvoked != nil && !*e.AgentInvoked {
				w.deferSteeredTerminal(e)
			}
			return
		}
		if e.AgentInvoked != nil && !*e.AgentInvoked {
			if w.activeInputKind == queuedNativeCommand {
				switch e.Status {
				case "completed":
					w.finishLocalCommandSubmission()
				case "aborted":
					w.finishCancelled(w.tr(i18n.Msg.TaskCancelledBeforeCompletion))
				case "error":
					w.finishUncertain(w.tr(i18n.Msg.NativeCommandFailedBeforeProducingConfirmedResultTaskOutcome))
				default:
					w.finishUncertain(w.tr(i18n.Msg.NativeCommandCompletionCouldNotConfirmedTaskOutcomeUncertain))
				}
				return
			}
			w.finishTerminal(e)
		}
	case "session_settled":
		w.invalidateResearchSettlement()
		w.probeResearchSettlement()
		w.probeSteeredQuiescence()
	case "response":
		if !e.Success {
			w.clearQueue()
			w.retireTaskRuntime(w.tr(i18n.Msg.OmpRequestFailedTaskOutcomeUncertainWillNotReplayed))
		}
	case "extension_ui_request":
		if w.researchNotify(e) {
			return
		}
		w.ui(e)
	}
}
func (w *worker) typing() {
	if w.b.cfg.ProgressMode == "off" || w.replyStatus.pending != nil {
		return
	}
	preparing := len(w.queue) > 0 && w.queue[0].preparing
	if (!w.taskOrSessionBusy() && !preparing) || time.Since(w.lastTyping) < 5*time.Second {
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

func (w *worker) startProgressTool(id, name string) {
	if id == "" || name == "" {
		return
	}
	if w.progress.ActiveTools == nil {
		w.progress.ActiveTools = make(map[string]progressTool)
	}
	w.progress.ActiveTools[id] = progressTool{ID: id, Name: clipUTF16(name, maxToolNameUnits), StartedAt: time.Now()}
}

func progressToolPayload(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	units, cut := 0, 0
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		units += utf16.RuneLen(r)
		if units > maxToolDetailUnits {
			return string(raw[:cut]) + "…"
		}
		i += size
		if units < maxToolDetailUnits {
			cut = i
		}
	}
	return string(raw)
}

type progressPayloadField string

func (p *progressPayloadField) UnmarshalJSON(data []byte) error {
	*p = progressPayloadField(progressToolPayload(data))
	return nil
}

func (w *worker) recordProgressToolPayload(id, eventType string, raw []byte) {
	if w.b == nil || w.b.cfg.ProgressMode != "verbose" {
		return
	}
	tool, ok := w.progress.ActiveTools[id]
	if !ok {
		return
	}
	var payload struct {
		Args          progressPayloadField `json:"args"`
		Arguments     progressPayloadField `json:"arguments"`
		PartialResult progressPayloadField `json:"partialResult"`
		Result        progressPayloadField `json:"result"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return
	}
	switch eventType {
	case "tool_execution_start", "host_tool_call":
		args := payload.Args
		if args == "" {
			args = payload.Arguments
		}
		if args != "" {
			tool.Args = string(args)
		}
	case "tool_execution_update":
		if payload.PartialResult != "" {
			tool.Detail = string(payload.PartialResult)
			tool.DetailKind = i18n.Msg.Update
		}
	case "tool_execution_end":
		if payload.Result != "" {
			tool.Detail = string(payload.Result)
			tool.DetailKind = i18n.Msg.Result
		}
	}
	w.progress.ActiveTools[id] = tool
}

func (w *worker) renameProgressTool(id, name string) bool {
	if id == "" || name == "" {
		return false
	}
	tool, ok := w.progress.ActiveTools[id]
	if !ok {
		return false
	}
	tool.Name = clipUTF16(name, maxToolNameUnits)
	w.progress.ActiveTools[id] = tool
	return true
}

func (w *worker) endProgressTool(id string, failed bool) {
	tool, ok := w.progress.ActiveTools[id]
	if !ok {
		return
	}
	delete(w.progress.ActiveTools, id)
	if w.b == nil || w.b.cfg.ProgressMode != "verbose" {
		return
	}
	result := progressToolResult{Name: tool.Name, Args: tool.Args, Detail: tool.Detail, DetailKind: tool.DetailKind, Duration: time.Since(tool.StartedAt).Round(time.Second), Failed: failed}
	if len(w.progress.RecentTools) < maxRecentTools {
		w.progress.RecentTools = append(w.progress.RecentTools, result)
		return
	}
	w.progress.RecentTools[w.progress.EarlierTools%maxRecentTools] = result
	w.progress.EarlierTools++
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

func progressToolLine(language i18n.Language, tool progressTool) string {
	return i18n.Text(language, i18n.Msg.RunningTool, tool.Name)
}

func (w *worker) progressStatus() string {
	switch {
	case w.progress.Retrying:
		return w.tr(i18n.Msg.RetryingAutomatically)
	case w.compacting:
		return w.tr(i18n.Msg.CompactingContext)
	case len(w.progress.ActiveTools) > 1:
		return w.tr(i18n.Msg.RunningTools)
	case len(w.progress.ActiveTools) == 1:
		return w.tr(i18n.Msg.ToolRunningPhase)
	case w.busy:
		return w.tr(i18n.Msg.Running)
	default:
		return ""
	}
}

func progressToolLiteral(detail string) string {
	marker := "`"
	if strings.ContainsRune(detail, '\n') {
		marker = "```"
	}
	for strings.Contains(detail, marker) {
		marker += "`"
	}
	if strings.ContainsRune(detail, '\n') {
		return "\n" + marker + "\n" + detail + "\n" + marker
	}
	return marker + detail + marker
}

func appendProgressToolDetail(lines []string, label, detail string, budget *int) []string {
	if detail == "" || *budget == 0 {
		return lines
	}
	limit := min(*budget, maxToolDetailUnits)
	visible := clipUTF16(detail, limit)
	line := "  " + label + ": " + progressToolLiteral(visible)
	cost := utf16Length(line) + 1
	if cost > *budget {
		return lines
	}
	*budget -= cost
	return append(lines, line)
}

func (w *worker) renderProgress() string {
	if !w.taskRunning() {
		return ""
	}
	var sections []string
	if w.progress.NativeStatus != "" {
		sections = append(sections, w.progress.NativeStatus)
	}
	sections = append(sections, w.tr(i18n.Msg.Processing))
	if status := w.progressStatus(); status != "" {
		sections = append(sections, w.tr(i18n.Msg.StatusValue, status))
	}
	verbose := w.b != nil && w.b.cfg.ProgressMode == "verbose"
	detailBudget := max(0, maxToolDetailsUnits-utf16Length(w.progress.NativeStatus))
	active := sortedProgressTools(w.progress.ActiveTools)
	if len(active) > 0 {
		lines := []string{w.tr(i18n.Msg.Tools)}
		for i, tool := range active {
			if i == maxActiveTools {
				lines = append(lines, w.tr(i18n.Msg.More, len(active)-i))
				break
			}
			lines = append(lines, progressToolLine(w.b.language(), tool))
			if verbose {
				lines = appendProgressToolDetail(lines, w.tr(i18n.Msg.Args), tool.Args, &detailBudget)
				lines = appendProgressToolDetail(lines, w.tr(tool.DetailKind), tool.Detail, &detailBudget)
			}
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	if verbose && len(w.progress.RecentTools) > 0 {
		lines := []string{w.tr(i18n.Msg.RecentTools)}
		if w.progress.EarlierTools > 0 {
			lines = append(lines, w.tr(i18n.Msg.EarlierOmitted, w.progress.EarlierTools))
		}
		for i := range w.progress.RecentTools {
			tool := w.progress.RecentTools[(w.progress.EarlierTools+i)%len(w.progress.RecentTools)]
			status := w.tr(i18n.Msg.Completed)
			if tool.Failed {
				status = w.tr(i18n.Msg.Failed)
			}
			lines = append(lines, w.tr(i18n.Msg.RecentToolOutcome, tool.Name, status, tool.Duration))
			lines = appendProgressToolDetail(lines, w.tr(i18n.Msg.Args), tool.Args, &detailBudget)
			lines = appendProgressToolDetail(lines, w.tr(tool.DetailKind), tool.Detail, &detailBudget)
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	text := strings.Join(sections, "\n\n")
	if w.preview != "" {
		available := maxProgressUnits - utf16Length(text) - utf16Length(w.tr(i18n.Msg.Output))
		if available > 0 {
			if available > maxPreviewUnits {
				available = maxPreviewUnits
			}
			text += w.tr(i18n.Msg.Output) + tailUTF16(w.preview, available)
		}
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
		return &telegram.Keyboard{InlineKeyboard: [][]telegram.Button{{{Text: w.tr(i18n.Msg.Stop), CallbackData: token + ":0"}}}}
	}
	if token == "" {
		token = w.previewStopToken
	}
	c, ok := w.confirms[token]
	if !ok || c.action != "stop" || c.generation != w.binding.Generation || c.turn != w.turn || c.active != w.active {
		return nil
	}
	return &telegram.Keyboard{InlineKeyboard: [][]telegram.Button{{{Text: w.tr(i18n.Msg.Stop), CallbackData: token + ":0"}}}}
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
		case w.taskRunning() && !w.finishing && c.active == w.active:
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
	if w.b.cfg.ProgressMode == "off" || w.progressSuppressed || w.replyStatus.pending != nil {
		return
	}
	if w.previewID == 0 && time.Since(w.progress.StartedAt) < progressInitialDelay {
		return
	}
	w.refreshProgressNative(time.Now())
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
	if id == 0 && w.taskRunning() && !w.finishing {
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
		title = w.tr(i18n.Msg.ConfirmationRequired)
	} else if c.action == "binding_delete" {
		title = parts[len(parts)-1]
	} else {
		title = parts[0]
	}
	c.uiTitle = title
	var data [12]byte
	if _, e := rand.Read(data[:]); e != nil {
		return
	}
	token := hex.EncodeToString(data[:])
	if c.action != "ui" {
		c.expires = time.Now().Add(2 * time.Minute)
	}
	c.generation = w.binding.Generation
	if c.action == "binding_delete" {
		c.generation = w.conversationGeneration()
	}
	if c.user == 0 {
		c.user = w.owner
	}
	k := &telegram.Keyboard{}
	var deleteButtons []telegram.Button
	for i, label := range options {
		button := telegram.Button{Text: label, CallbackData: fmt.Sprintf("%s:%d", token, i)}
		if c.action == "binding_delete" || c.action == "session_delete" {
			if i == 0 || c.action == "binding_delete" && i == 1 {
				button.Style = "danger"
			}
			deleteButtons = append(deleteButtons, button)
		} else {
			k.InlineKeyboard = append(k.InlineKeyboard, []telegram.Button{button})
		}
	}
	if len(deleteButtons) != 0 {
		k.InlineKeyboard = append(k.InlineKeyboard, deleteButtons)
	}
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	w.touchActivity()
	defer cancel()
	if c.action == "binding_delete" && len(parts) > 1 {
		for _, part := range parts[:len(parts)-1] {
			if _, err := w.b.tg.Send(ctx, w.key.chat, w.key.thread, part, telegram.SendOptions{}); err != nil {
				w.say(w.tr(i18n.Msg.FailedSendConfirmationOperationCanceled))
				return
			}
		}
	}
	message, e := w.b.tg.Send(ctx, w.key.chat, w.key.thread, title, telegram.SendOptions{Keyboard: k})
	if e != nil {
		w.cancelUI(c)
		w.say(w.tr(i18n.Msg.FailedSendConfirmationOperationCanceled))
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
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
		return callbackDone
	}
	switch {
	case action == "close":
		if err := w.b.tg.ClearKeyboard(ctx, w.key.chat, c.messageID); err != nil {
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.CouldNotCloseQueueMenuTapCloseAgain))
			return callbackDone
		}
		delete(w.confirms, token)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Closed))
	case action == "previous":
		delete(w.confirms, token)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Received))
		w.showQueuePage(c, c.page-1, c.messageID)
	case action == "next":
		delete(w.confirms, token)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Received))
		w.showQueuePage(c, c.page+1, c.messageID)
	case strings.HasPrefix(action, "cancel:"):
		id, err := strconv.ParseInt(strings.TrimPrefix(action, "cancel:"), 10, 64)
		if err != nil || id <= 0 {
			delete(w.confirms, token)
			w.clearKeyboard(c.messageID)
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
			return callbackDone
		}
		delete(w.confirms, token)
		if w.cancelQueuedTask(id) {
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.CancelledQueuedTask))
		} else {
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.TaskNoLongerQueued))
		}
		w.showQueuePage(c, c.page, c.messageID)
	default:
		delete(w.confirms, token)
		w.clearKeyboard(c.messageID)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
	}
	return callbackDone
}

func (w *worker) callback(q *telegram.CallbackQuery) callbackResult {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	token, index, ok := strings.Cut(q.Data, ":")
	c, exists := w.confirms[token]
	if !ok || !exists {
		if owner, isStop := progressStopTokenOwner(token); isStop {
			var messageID int64
			if owner == q.From.ID && q.Message != nil {
				messageID = q.Message.MessageID
			}
			w.queueStopCallbackReply(q.ID, i18n.Msg.ActionExpired, messageID)
			return callbackDone
		}
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
		return callbackDone
	}
	if c.user != q.From.ID {
		if c.action == "stop" {
			w.queueStopCallbackReply(q.ID, i18n.Msg.ActionExpired, 0)
			return callbackDone
		}
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
		return callbackDone
	}
	if c.uiSelectMenu && (q.Message == nil || q.Message.MessageID != c.messageID) {
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
		return callbackDone
	}
	if c.action == "ui" && (w.client == nil || c.uiClientID != w.client.ID()) {
		delete(w.confirms, token)
		w.queueKeyboardCleanup(c.messageID)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
		return callbackDone
	}
	if c.action == "autoresearch_clear" && !researchCallbackMessage(q, c, w.key) {
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
		return callbackDone
	}
	if (c.action == "resume" || c.action == "export" || c.action == "session_delete") && (q.Message == nil || q.Message.MessageID != c.messageID) {
		delete(w.confirms, token)
		_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
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
		if c.action == "stop" {
			w.queueStopCallbackReply(q.ID, i18n.Msg.ActionExpired, messageID)
		} else {
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
			w.clearKeyboard(messageID)
		}
		if c.generation == w.binding.Generation {
			w.cancelUI(c)
		}
		return callbackDone
	}
	if c.uiSelectMenu && (index == "previous" || index == "next") {
		page := c.page - 1
		if index == "next" {
			page = c.page + 1
		}
		pages := (len(c.options) + uiSelectPageSize - 1) / uiSelectPageSize
		if page < 0 || page >= pages {
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
			return callbackDone
		}
		if w.showUISelectPage(c, page, c.messageID) {
			delete(w.confirms, token)
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Received))
		} else {
			delete(w.confirms, token)
			w.clearKeyboard(c.messageID)
		}
		return callbackDone
	}
	n, err := strconv.Atoi(index)
	if err != nil || n < 0 || (c.action == "stop" && n != 0) || (c.method != "select" && c.action != "stop" && n > 1) || (c.method == "select" && n > len(c.options)) {
		return callbackDone
	}
	if c.uiSelectMenu && n < len(c.options) {
		start := c.page * uiSelectPageSize
		if n < start || n >= min(start+uiSelectPageSize, len(c.options)) {
			_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.ActionExpired))
			return callbackDone
		}
	}
	if c.action == "stop" {
		if c.active != w.active || c.turn != w.turn || !w.taskRunning() {
			delete(w.confirms, token)
			if w.previewStopToken == token {
				w.previewStopToken = ""
			}
			w.queueStopCallbackReply(q.ID, i18n.Msg.ActionExpired, messageID)
			return callbackDone
		}
		delete(w.confirms, token)
		if w.previewStopToken == token {
			w.previewStopToken = ""
		}
		w.queueStopCallbackReply(q.ID, i18n.Msg.Stopping, messageID)
		w.stopActiveTask()
		return callbackDone
	}
	_ = w.b.tg.AnswerCallback(ctx, q.ID, w.tr(i18n.Msg.Received))
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
	if c.action == "args" {
		w.selectArgsFromMenu(c, n)
		return callbackDone
	}
	if c.action == "ui" && w.exportingSession != "" {
		text := w.tr(i18n.Msg.WaitCurrentSessionExportFinish)
		w.finishMenu(c.messageID, text)
		w.say(text)
		return callbackDone
	}
	w.finishConfirmation(c, n)
	if c.action == "session_delete" {
		if n == 0 {
			w.beginSessionDelete(c)
		}
		return callbackDone
	}
	if c.action == "autoresearch_clear" {
		if n == 0 {
			w.confirmResearchClear(c)
		}
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
		client, connected := w.runtimeClient()
		if !connected || client.Send(ctx, frame) != nil {
			w.clearQueue()
			w.retireTaskRuntime(w.tr(i18n.Msg.ConfirmationCouldNotDeliveredOmpStateUncertainWillNot))
			return callbackUncertain
		}
		w.touchActivity()
		w.touchBinding()
		return callbackDone
	}
	if c.method == "select" && n == len(c.options) {
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
	if n != 0 {
		return callbackDone
	}
	switch c.action {
	case "new":
		if !filepath.IsAbs(c.workspace) {
			w.say(w.tr(i18n.Msg.InvalidWorkingDirectoryExistingInstanceStillRunning))
			return callbackDone
		}
		if err := os.MkdirAll(c.workspace, 0700); err != nil {
			w.say(w.tr(i18n.Msg.CannotCreateWorkingDirectoryExistingInstanceStillRunning))
			return callbackDone
		}
		w.start(false, c.workspace, "", true)
	}
	return callbackDone
}

// Menu completion records the user's choice, not the operation's success.
func (w *worker) finishConfirmation(c confirmation, index int) {
	cancelled := index != 0
	if c.method == "select" {
		cancelled = index == len(c.options)
	}
	text := w.tr(i18n.Msg.Cancel)
	if cancelled {
		if c.uiTitle != "" {
			text += "\n" + c.uiTitle
		}
	} else {
		if c.method == "select" {
			text = w.tr(i18n.Msg.Selected, c.options[index])
			if c.action == "model" || c.action == "thinking" {
				text += w.tr(i18n.Msg.Applying)
			}
		} else {
			text = w.tr(i18n.Msg.Confirmed)
			if c.uiTitle != "" {
				text += "\n" + c.uiTitle
			}
		}
	}
	w.finishMenu(c.messageID, text)
}

func (w *worker) finishMenu(messageID int64, text string) bool {
	if messageID == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	keyboard := &telegram.Keyboard{InlineKeyboard: [][]telegram.Button{}}
	if w.b.tg.Edit(ctx, w.key.chat, messageID, truncateUTF8(text, 3700), keyboard) != nil {
		w.queueKeyboardCleanup(messageID)
		return false
	}
	return true
}

type stopCallbackReply struct {
	id   string
	text string
}

func (w *worker) queueStopCallbackReply(id string, message i18n.Message, messageID int64) {
	w.queueKeyboardCleanup(messageID)
	if w.ctx.Err() != nil {
		return
	}
	if w.stopCallbackReplies == nil {
		replies := make(chan stopCallbackReply, 32)
		w.stopCallbackReplies = replies
		parent, tg := w.ctx, w.b.tg
		w.background.Add(1)
		go func() {
			defer w.background.Done()
			for {
				select {
				case <-parent.Done():
					return
				case reply := <-replies:
					ctx, cancel := context.WithTimeout(parent, 5*time.Second)
					_ = tg.AnswerCallback(ctx, reply.id, reply.text)
					cancel()
				}
			}
		}()
	}
	// Stop tokens are already checked; UI receipts are expendable under backpressure.
	select {
	case w.stopCallbackReplies <- stopCallbackReply{id: id, text: w.tr(message)}:
	default:
	}
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

func (w *worker) expireStartIntent(now time.Time) {
	if w.startIntent == nil || w.b.cfg.DatabaseRetentionDays == 0 || w.client != nil || w.runtime != runtimeReleased || w.restoring || now.Sub(w.lastIntentRetentionCheck) < databaseJanitorInterval {
		return
	}
	w.lastIntentRetentionCheck = now
	cutoff := now.AddDate(0, 0, -w.b.cfg.DatabaseRetentionDays).Unix()
	expired, err := w.b.db.ExpireStart(w.ctx, *w.startIntent, cutoff)
	if err != nil {
		if w.ctx.Err() == nil {
			w.b.storeLog.Error("startup intent cleanup failed", "event", "cleanup_failed", "error_kind", "persistence")
			w.b.fail(err)
		}
		return
	}
	if expired {
		w.startIntent = nil
		w.b.bindingsEpoch.Add(1)
		w.log.Info("expired startup intent removed", "event", "cleanup_completed")
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

const uiSelectPageSize = 10

func (w *worker) showUISelectPage(c confirmation, page int, messageID int64) bool {
	pages := (len(c.options) + uiSelectPageSize - 1) / uiSelectPageSize
	if page < 0 || page >= pages {
		return false
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		w.cancelUI(c)
		w.say(w.tr(i18n.Msg.CannotCreateNativeSelectionMenuDialogCanceled))
		return false
	}
	token := hex.EncodeToString(random[:])
	text := c.uiTitle
	if pages > 1 {
		text += w.tr(i18n.Msg.Page, page+1, pages)
	}
	keyboard := &telegram.Keyboard{}
	start, end := page*uiSelectPageSize, min((page+1)*uiSelectPageSize, len(c.options))
	for i := start; i < end; i++ {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegram.Button{{Text: c.options[i], CallbackData: fmt.Sprintf("%s:%d", token, i)}})
	}
	var navigation []telegram.Button
	if page > 0 {
		navigation = append(navigation, telegram.Button{Text: w.tr(i18n.Msg.Previous), CallbackData: token + ":previous"})
	}
	if page+1 < pages {
		navigation = append(navigation, telegram.Button{Text: w.tr(i18n.Msg.Next), CallbackData: token + ":next"})
	}
	navigation = append(navigation, telegram.Button{Text: w.tr(i18n.Msg.Cancel), CallbackData: fmt.Sprintf("%s:%d", token, len(c.options))})
	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, navigation)
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	var err error
	if messageID != 0 {
		err = w.b.tg.Edit(ctx, w.key.chat, messageID, text, keyboard)
	} else {
		var message telegram.Message
		message, err = w.b.tg.Send(ctx, w.key.chat, w.key.thread, text, telegram.SendOptions{Keyboard: keyboard})
		messageID = message.MessageID
	}
	if err != nil {
		w.cancelUI(c)
		w.say(w.tr(i18n.Msg.FailedDisplayNativeSelectionMenuDialogCanceled))
		return false
	}
	c.page, c.messageID = page, messageID
	w.confirms[token] = c
	w.touchActivity()
	return true
}

func (w *worker) ui(e rpcEvent) {
	client, connected := w.runtimeClient()
	if !connected {
		return
	}
	switch e.Method {
	case "confirm", "select":
		var msg string
		_ = json.Unmarshal(e.Message, &msg)
		c := confirmation{action: "ui", uiID: e.ID, uiClientID: client.ID(), method: e.Method, options: e.Options}
		if e.Method == "confirm" {
			w.confirm(c, e.Title+"\n"+msg, []string{w.tr(i18n.Msg.Confirm), w.tr(i18n.Msg.Cancel)})
			return
		}
		if len(e.Options) == 0 {
			w.cancelUI(c)
			w.say(w.tr(i18n.Msg.NumberOptionsUnsupportedDialogCanceled))
			return
		}
		c.user, c.generation, c.uiSelectMenu = w.owner, w.binding.Generation, true
		parts := split(e.Title+"\n"+msg, 3700)
		c.uiTitle = w.tr(i18n.Msg.ChooseOption)
		if len(parts) != 0 {
			c.uiTitle = parts[0]
		}
		w.showUISelectPage(c, 0, 0)
	case "input", "editor":
		w.cancelUI(confirmation{uiID: e.ID, uiClientID: client.ID()})
		w.say(w.tr(i18n.Msg.InputDialogsNotSupportedCanceledProvideInformationNormalMessage))
	case "cancel":
		for token, c := range w.confirms {
			if c.action == "ui" && c.uiID != "" && c.uiID == e.TargetID {
				w.dropConfirmation(token, c)
			}
		}
	}
}
func (w *worker) cancelUI(c confirmation) {
	if client, connected := w.runtimeClient(); c.uiID != "" && connected && c.uiClientID == client.ID() {
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
		defer cancel()
		_ = client.Send(ctx, map[string]any{"type": "extension_ui_response", "id": c.uiID, "cancelled": true})
	}
}

// Programmatic invalidation removes the Telegram token first. Explicit runtime
// teardown may then cancel a current-generation native UI.
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
		if c.action == "queue" || c.action == "resume" || c.action == "export" || c.action == "session_delete" {
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

func (w *worker) expire(now time.Time) {
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
