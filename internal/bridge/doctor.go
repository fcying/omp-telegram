package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

type doctorLevel uint8

const (
	doctorOK doctorLevel = iota
	doctorWarn
	doctorFail
)

type doctorCheck struct {
	Name        i18n.Message
	Level       doctorLevel
	Message     i18n.Message
	MessageArgs []any
	Opaque      string
	Delivery    *doctorDelivery
}

type doctorDelivery struct {
	stats store.DeliveryDiagnostics
	now   int64
}

type doctorBindingSnapshot struct {
	exists     bool
	generation int64
	workspace  string
	session    string
	sessionID  string
	running    bool
}

type doctorFence struct {
	binding  doctorBindingSnapshot
	runtime  runtimeState
	clientID uint64
}

type doctorResult struct {
	request uint64
	fence   doctorFence
	checks  []doctorCheck
	err     error
}

var doctorTimeout = 10 * time.Second

const (
	doctorTelegramTimeout        = 5 * time.Second
	doctorOMPTimeout             = 5 * time.Second
	doctorDiskWarn        uint64 = 1 << 30
	doctorDiskFail        uint64 = 128 << 20
)

var (
	doctorGetMe  = defaultDoctorGetMe
	doctorRunOMP = checkDoctorOMP
	doctorStatfs = platformDoctorStatfs
)

func (w *worker) runDoctor() {
	if w.doctorCancel != nil {
		w.say(w.tr(i18n.Msg.DiagnosticsAlreadyRunning))
		return
	}
	b := w.b
	key := w.key
	binding, err := readDoctorBinding(b.db, b.bot.ID, key)
	if err != nil {
		w.say(w.tr(i18n.Msg.DiagnosticsUnavailableRunDoctorAgain))
		return
	}
	fence := doctorFence{binding: binding, runtime: w.runtime}
	if w.client != nil {
		fence.clientID = w.client.ID()
	}
	if w.doctorResults == nil {
		w.doctorResults = make(chan doctorResult, 1)
	}
	workerCtx := w.ctx
	ctx, cancel := context.WithTimeout(workerCtx, doctorTimeout)
	w.doctorCancel = cancel
	w.doctorRequest++
	request := w.doctorRequest
	results := w.doctorResults
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		result := doctorResult{request: request, fence: fence}
		result.checks, result.err = runDoctorChecks(ctx, b.cfg, b.tg, b.db, fence.binding, fence.runtime, key)
		select {
		case results <- result:
		case <-workerCtx.Done():
		}
		cancel()
	}()
}

func readDoctorBinding(db *store.Store, bot int64, key target) (doctorBindingSnapshot, error) {
	binding, err := db.Binding(bot, key.chat, key.thread)
	if errors.Is(err, sql.ErrNoRows) {
		return doctorBindingSnapshot{}, nil
	}
	if err != nil {
		return doctorBindingSnapshot{}, err
	}
	return doctorBindingSnapshot{
		exists:     true,
		generation: binding.Generation,
		workspace:  binding.Workspace,
		session:    binding.Session,
		sessionID:  binding.SessionID,
		running:    binding.Running,
	}, nil
}

func sameDoctorBinding(left, right doctorBindingSnapshot) bool {
	return left == right
}

func runDoctorChecks(ctx context.Context, cfg config.Config, tg *telegram.Client, db *store.Store, binding doctorBindingSnapshot, runtime runtimeState, key target) ([]doctorCheck, error) {
	checks := make([]doctorCheck, 0, 11)
	appendChecks := func(items ...doctorCheck) bool {
		if ctx.Err() != nil {
			return false
		}
		checks = append(checks, items...)
		return ctx.Err() == nil
	}
	if !appendChecks(checkDoctorConfig(cfg)) {
		return checks, ctx.Err()
	}
	if !appendChecks(checkDoctorTelegram(ctx, tg)) {
		return checks, ctx.Err()
	}
	if !appendChecks(checkDoctorDatabase(ctx, db)) {
		return checks, ctx.Err()
	}
	if !appendChecks(checkDoctorStorage(cfg.DataDir)) {
		return checks, ctx.Err()
	}
	if !appendChecks(checkDoctorOMPWithTimeout(ctx, cfg.OMP, cfg.OMPEnvironment)) {
		return checks, ctx.Err()
	}
	workspace, session := checkDoctorWorkspaceSession(binding, runtime)
	if !appendChecks(workspace, session) {
		return checks, ctx.Err()
	}
	if !appendChecks(checkDoctorRuntime(runtime, binding)) {
		return checks, ctx.Err()
	}
	if !appendChecks(checkDoctorInbox(ctx, db)) {
		return checks, ctx.Err()
	}
	if !appendChecks(checkDoctorDisk(cfg.DataDir)) {
		return checks, ctx.Err()
	}
	// A delivery query failure, including its deadline, degrades only this check.
	outbox := checkDoctorDelivery(ctx, db, key, time.Now().Unix())
	return append(checks, outbox), nil
}

func checkDoctorConfig(cfg config.Config) doctorCheck {
	if cfg.OMP == "" || cfg.DataDir == "" || cfg.MaxWorkers < 1 || cfg.MaxWorkers > config.MaxWorkersLimit || cfg.QueueCapacity < 1 || cfg.QueueCapacity > config.MaxQueueCapacityLimit {
		return doctorCheck{Name: i18n.Msg.Config, Level: doctorFail, Message: i18n.Msg.InvalidRuntimeConfiguration}
	}
	switch cfg.ProgressMode {
	case "off", "summary", "verbose":
		return doctorCheck{Name: i18n.Msg.Config, Level: doctorOK}
	default:
		return doctorCheck{Name: i18n.Msg.Config, Level: doctorFail, Message: i18n.Msg.InvalidRuntimeConfiguration}
	}
}

func defaultDoctorGetMe(ctx context.Context, tg *telegram.Client) error {
	if tg == nil {
		return errors.New("telegram client unavailable")
	}
	_, err := tg.GetMe(ctx)
	return err
}

func checkDoctorTelegram(parent context.Context, tg *telegram.Client) doctorCheck {
	ctx, cancel := context.WithTimeout(parent, doctorTelegramTimeout)
	defer cancel()
	if err := doctorGetMe(ctx, tg); err != nil {
		return doctorCheck{Name: i18n.Msg.Telegram, Level: doctorFail, Message: i18n.Msg.BotAPIUnavailable}
	}
	return doctorCheck{Name: i18n.Msg.Telegram, Level: doctorOK}
}

func checkDoctorDatabase(ctx context.Context, db *store.Store) doctorCheck {
	if db == nil {
		return doctorCheck{Name: i18n.Msg.Database, Level: doctorFail, Message: i18n.Msg.SQLiteCheckUnavailable}
	}
	if err := db.QuickCheck(ctx); err != nil {
		if errors.Is(err, store.ErrQuickCheckFailed) {
			return doctorCheck{Name: i18n.Msg.Database, Level: doctorFail, Message: i18n.Msg.SQLiteQuickCheckFailed}
		}
		return doctorCheck{Name: i18n.Msg.Database, Level: doctorFail, Message: i18n.Msg.SQLiteCheckUnavailable}
	}
	return doctorCheck{Name: i18n.Msg.Database, Level: doctorOK}
}

func checkDoctorStorage(dataDir string) doctorCheck {
	fail := func() doctorCheck {
		return doctorCheck{Name: i18n.Msg.Storage, Level: doctorFail, Message: i18n.Msg.DataDirectoryNotWritable}
	}
	if !filepath.IsAbs(dataDir) {
		return fail()
	}
	info, err := os.Stat(dataDir)
	if err != nil || !info.IsDir() {
		return fail()
	}
	file, err := os.CreateTemp(dataDir, ".doctor-*")
	if err != nil {
		return fail()
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err = file.WriteString("doctor"); err != nil {
		_ = file.Close()
		return fail()
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return fail()
	}
	if err = file.Close(); err != nil {
		return fail()
	}
	if err = os.Remove(path); err != nil {
		return fail()
	}
	return doctorCheck{Name: i18n.Msg.Storage, Level: doctorOK}
}

func checkDoctorOMPWithTimeout(parent context.Context, binary string, environment omp.Environment) doctorCheck {
	ctx, cancel := context.WithTimeout(parent, doctorOMPTimeout)
	defer cancel()
	if err := doctorRunOMP(ctx, binary, environment); err != nil {
		return doctorCheck{Name: i18n.Msg.OMPBinary, Level: doctorFail, Message: i18n.Msg.ExecutableCheckFailed}
	}
	return doctorCheck{Name: i18n.Msg.OMPBinary, Level: doctorOK}
}

func checkDoctorOMP(ctx context.Context, binary string, environment omp.Environment) error {
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("omp binary is unavailable")
	}
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = omp.ChildEnv(environment)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func checkDoctorWorkspaceSession(binding doctorBindingSnapshot, runtime runtimeState) (doctorCheck, doctorCheck) {
	if !binding.exists {
		return doctorCheck{Name: i18n.Msg.Workspace, Level: doctorWarn, Message: i18n.Msg.NotSelected}, doctorCheck{Name: i18n.Msg.Session.Label, Level: doctorWarn, Message: i18n.Msg.NotSelected}
	}
	workspace := doctorCheck{Name: i18n.Msg.Workspace, Level: doctorFail, Message: i18n.Msg.DirectoryUnavailable}
	if filepath.IsAbs(binding.workspace) {
		if info, err := os.Stat(binding.workspace); err == nil && info.IsDir() {
			workspace.Level = doctorOK
			workspace.Message = i18n.Message{}
		}
	}
	session := checkDoctorSession(binding, runtime)
	return workspace, session
}

func checkDoctorSession(binding doctorBindingSnapshot, runtime runtimeState) doctorCheck {
	if binding.session == "" {
		return doctorCheck{Name: i18n.Msg.Session.Label, Level: doctorWarn, Message: i18n.Msg.NotSelected}
	}
	available := filepath.IsAbs(binding.session)
	if available {
		info, err := os.Stat(binding.session)
		available = err == nil && info.Mode().IsRegular()
	}
	if !available {
		if binding.running && runtime != runtimeReleased {
			return doctorCheck{Name: i18n.Msg.Session.Label, Level: doctorWarn, Message: i18n.Msg.SessionHistoryNotPersistedYet}
		}
		if binding.running {
			return doctorCheck{Name: i18n.Msg.Session.Label, Level: doctorFail, Message: i18n.Msg.ReleasedSessionFileUnavailable}
		}
		return doctorCheck{Name: i18n.Msg.Session.Label, Level: doctorWarn, Message: i18n.Msg.SavedSessionFileUnavailable}
	}
	message := ""
	if validSessionID(binding.sessionID) {
		message = "[" + shortSessionID(binding.sessionID) + "]"
	}
	return doctorCheck{Name: i18n.Msg.Session.Label, Level: doctorOK, Opaque: message}
}

func checkDoctorRuntime(runtime runtimeState, binding doctorBindingSnapshot) doctorCheck {
	status := i18n.Msg.Closed
	switch runtime {
	case runtimeConnected:
		status = i18n.Msg.Connected
	case runtimeStarting:
		status = i18n.Msg.Starting
	case runtimeReleased:
		if binding.exists && binding.running {
			status = i18n.Msg.Released
		}
	default:
		return doctorCheck{Name: i18n.Msg.Runtime, Level: doctorFail, Message: i18n.Msg.InconsistentInternalRuntimeState}
	}
	return doctorCheck{Name: i18n.Msg.Runtime, Level: doctorOK, Message: status}
}

func checkDoctorInbox(ctx context.Context, db *store.Store) doctorCheck {
	inbox := doctorCheck{Name: i18n.Msg.Inbox, Level: doctorWarn, Message: i18n.Msg.CountUnavailable}
	if db == nil {
		return inbox
	}
	count, err := db.InboxUncertainCount(ctx)
	if err != nil {
		return inbox
	}
	inbox.Level, inbox.Message = doctorOK, i18n.Message{}
	if count > 0 {
		inbox.Level = doctorWarn
		inbox.Message = i18n.Msg.UncertainCount
		inbox.MessageArgs = []any{count}
	}
	return inbox
}

func checkDoctorDelivery(ctx context.Context, db *store.Store, key target, now int64) doctorCheck {
	outbox := doctorCheck{Name: i18n.Msg.Outbox, Level: doctorWarn, Message: i18n.Msg.CountUnavailable}
	if db == nil {
		return outbox
	}
	stats, err := db.DeliveryDiagnostics(ctx, key.chat, key.thread)
	if err != nil {
		return outbox
	}
	outbox.Level, outbox.Message = doctorOK, i18n.Message{}
	outbox.Delivery = &doctorDelivery{stats: stats, now: now}
	if stats.Topic.Uncertain > 0 || stats.Global.Uncertain > 0 {
		outbox.Level = doctorWarn
	}
	return outbox
}

func (d *doctorDelivery) head() (state string, wait, retry int64) {
	switch d.stats.HeadState {
	case store.OutboxPending:
		return "pending", doctorElapsed(d.now, d.stats.HeadCreatedAt), max(int64(0), d.stats.HeadNextAttemptAt-d.now)
	case store.OutboxSending:
		return "sending", doctorElapsed(d.now, d.stats.HeadUpdatedAt), 0
	default:
		return "idle", 0, 0
	}
}

func doctorElapsed(now, at int64) int64 {
	if at <= 0 {
		return 0
	}
	return max(int64(0), now-at)
}

// Integer formatting also handles retry deadlines beyond time.Duration's range.
func doctorSeconds(seconds int64) string {
	if seconds >= 3600 {
		return fmt.Sprintf("%dh%dm%ds", seconds/3600, seconds/60%60, seconds%60)
	}
	if seconds >= 60 {
		return fmt.Sprintf("%dm%ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%ds", seconds)
}

func formatDoctorDelivery(language i18n.Language, d *doctorDelivery) string {
	stats := d.stats
	state, wait, retry := d.head()
	head := i18n.Text(language, i18n.Msg.Delivery.Idle)
	switch state {
	case "pending":
		head = i18n.Text(language, i18n.Msg.Delivery.Ready)
		if retry > 0 {
			head = i18n.Text(language, i18n.Msg.Delivery.RetryIn, doctorSeconds(retry))
		}
	case "sending":
		head = i18n.Text(language, i18n.Msg.Delivery.Sending, doctorSeconds(wait))
	}
	var report strings.Builder
	report.WriteString(i18n.Text(language, i18n.Msg.Delivery.Counts, stats.Topic.Pending, stats.Topic.Sending))
	if stats.Topic.Pending > 0 {
		age := i18n.Text(language, i18n.Msg.Unavailable)
		if stats.OldestPendingAt > 0 {
			age = doctorSeconds(doctorElapsed(d.now, stats.OldestPendingAt))
		}
		report.WriteString("\n  " + i18n.Text(language, i18n.Msg.Delivery.OldestPending, age))
	}
	report.WriteString("\n  " + i18n.Text(language, i18n.Msg.Delivery.Head, head))
	report.WriteString("\n  " + i18n.Text(language, i18n.Msg.Delivery.Uncertain, stats.Topic.Uncertain))
	report.WriteString("\n  " + i18n.Text(language, i18n.Msg.Delivery.Global, stats.Global.Pending, stats.Global.Sending, stats.Global.Uncertain))
	return report.String()
}

func checkDoctorDisk(dataDir string) doctorCheck {
	free, err := doctorStatfs(dataDir)
	if err != nil {
		return doctorCheck{Name: i18n.Msg.Disk, Level: doctorWarn, Message: i18n.Msg.FreeSpaceUnavailable}
	}
	level := doctorOK
	if free < doctorDiskFail {
		level = doctorFail
	} else if free < doctorDiskWarn {
		level = doctorWarn
	}
	if free >= 1<<30 {
		return doctorCheck{Name: i18n.Msg.Disk, Level: level, Message: i18n.Msg.DiskFreeGiB, MessageArgs: []any{float64(free) / (1 << 30)}}
	}
	return doctorCheck{Name: i18n.Msg.Disk, Level: level, Message: i18n.Msg.DiskFreeMiB, MessageArgs: []any{free / (1 << 20)}}
}

func doctorOverall(checks []doctorCheck) doctorLevel {
	overall := doctorOK
	for _, check := range checks {
		if check.Level == doctorFail {
			return doctorFail
		}
		if check.Level == doctorWarn {
			overall = doctorWarn
		}
	}
	return overall
}

func doctorLevelName(level doctorLevel) i18n.Message {
	switch level {
	case doctorWarn:
		return i18n.Msg.WARN
	case doctorFail:
		return i18n.Msg.FAIL
	default:
		return i18n.Msg.OK
	}
}

func doctorResultName(level doctorLevel) string {
	switch level {
	case doctorWarn:
		return "warn"
	case doctorFail:
		return "fail"
	default:
		return "ok"
	}
}

func formatDoctorReport(language i18n.Language, checks []doctorCheck) string {
	var report strings.Builder
	report.WriteString(i18n.Text(language, i18n.Msg.OmpTelegramDiagnostics))
	for _, check := range checks {
		report.WriteByte('\n')
		report.WriteString(fmt.Sprintf("%-12s", i18n.Text(language, check.Name)))
		if check.Name == i18n.Msg.Runtime && check.Level == doctorOK {
			report.WriteByte(' ')
			report.WriteString(i18n.Text(language, check.Message))
			continue
		}
		report.WriteByte(' ')
		report.WriteString(i18n.Text(language, doctorLevelName(check.Level)))
		if check.Delivery != nil {
			report.WriteString(" - ")
			report.WriteString(formatDoctorDelivery(language, check.Delivery))
			continue
		}
		if check.Opaque != "" {
			report.WriteString(" - ")
			report.WriteString(check.Opaque)
		} else if check.Message != (i18n.Message{}) {
			report.WriteString(" - ")
			report.WriteString(i18n.Text(language, check.Message, check.MessageArgs...))
		}
	}
	report.WriteString(i18n.Text(language, i18n.Msg.ResultSummary, i18n.Text(language, doctorLevelName(doctorOverall(checks)))))
	return report.String()
}

func (w *worker) doctorFinished(result doctorResult) {
	if result.request != w.doctorRequest {
		return
	}
	if w.doctorCancel != nil {
		w.doctorCancel()
		w.doctorCancel = nil
	}
	if result.err != nil {
		if w.ctx.Err() != nil {
			return
		}
		message := i18n.Msg.DiagnosticsUnavailableRunDoctorAgain
		if errors.Is(result.err, context.DeadlineExceeded) {
			message = i18n.Msg.DiagnosticsTimedOutRunDoctorAgain
		}
		w.log.Warn("doctor diagnostics failed", "event", "doctor", "result", "fail")
		w.say(w.tr(message))
		return
	}
	current, err := readDoctorBinding(w.b.db, w.b.bot.ID, w.key)
	if err != nil {
		if w.ctx.Err() == nil {
			w.log.Warn("doctor binding snapshot failed", "event", "doctor", "result", "fail")
			w.say(w.tr(i18n.Msg.DiagnosticsUnavailableRunDoctorAgain))
		}
		return
	}
	clientID := uint64(0)
	if w.client != nil {
		clientID = w.client.ID()
	}
	if result.fence.runtime != w.runtime || result.fence.clientID != clientID || !sameDoctorBinding(result.fence.binding, current) {
		w.log.Info("doctor diagnostics discarded", "event", "doctor", "result", "stale")
		w.say(w.tr(i18n.Msg.StateChangedDuringDiagnosticsRunDoctorAgain))
		return
	}
	level := doctorOverall(result.checks)
	attrs := []slog.Attr{slog.String("event", "doctor"), slog.String("result", doctorResultName(level))}
	for _, check := range result.checks {
		if check.Name != i18n.Msg.Outbox {
			continue
		}
		if check.Delivery == nil {
			attrs = append(attrs, slog.String("head_state", "unavailable"))
			break
		}
		d := check.Delivery
		state, wait, retry := d.head()
		attrs = append(attrs,
			slog.Int64("pending_count", d.stats.Topic.Pending),
			slog.Int64("sending_count", d.stats.Topic.Sending),
			slog.Int64("uncertain_count", d.stats.Topic.Uncertain),
			slog.String("head_state", state),
			slog.Int64("head_wait_seconds", wait),
			slog.Int64("retry_in_seconds", retry))
		break
	}
	w.log.LogAttrs(context.Background(), slog.LevelInfo, "doctor diagnostics completed", attrs...)
	w.say(formatDoctorReport(w.b.language(), result.checks))
}
