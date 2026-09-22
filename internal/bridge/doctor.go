package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"omp-telegram/internal/config"
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
	Name    string
	Level   doctorLevel
	Message string
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
		w.say("Diagnostics are already running.")
		return
	}
	b := w.b
	key := w.key
	binding, err := readDoctorBinding(b.db, b.bot.ID, key)
	if err != nil {
		w.say("Diagnostics unavailable. Run /doctor again.")
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
		result.checks = runDoctorChecks(ctx, b.cfg, b.tg, b.db, fence.binding, fence.runtime)
		if ctx.Err() != nil {
			result.err = ctx.Err()
		}
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

func runDoctorChecks(ctx context.Context, cfg config.Config, tg *telegram.Client, db *store.Store, binding doctorBindingSnapshot, runtime runtimeState) []doctorCheck {
	checks := make([]doctorCheck, 0, 11)
	appendChecks := func(items ...doctorCheck) bool {
		if ctx.Err() != nil {
			return false
		}
		checks = append(checks, items...)
		return ctx.Err() == nil
	}
	if !appendChecks(checkDoctorConfig(cfg)) {
		return checks
	}
	if !appendChecks(checkDoctorTelegram(ctx, tg)) {
		return checks
	}
	if !appendChecks(checkDoctorDatabase(ctx, db)) {
		return checks
	}
	if !appendChecks(checkDoctorStorage(cfg.DataDir)) {
		return checks
	}
	if !appendChecks(checkDoctorOMPWithTimeout(ctx, cfg.OMP)) {
		return checks
	}
	workspace, session := checkDoctorWorkspaceSession(binding, runtime)
	if !appendChecks(workspace, session) {
		return checks
	}
	if !appendChecks(checkDoctorRuntime(runtime, binding)) {
		return checks
	}
	inbox, outbox := checkDoctorUncertain(ctx, db)
	if !appendChecks(inbox, outbox) {
		return checks
	}
	appendChecks(checkDoctorDisk(cfg.DataDir))
	return checks
}

func checkDoctorConfig(cfg config.Config) doctorCheck {
	if cfg.OMP == "" || cfg.DataDir == "" || cfg.MaxWorkers < 1 || cfg.MaxWorkers > config.MaxWorkersLimit || cfg.QueueCapacity < 1 || cfg.QueueCapacity > config.MaxQueueCapacityLimit {
		return doctorCheck{Name: "Config", Level: doctorFail, Message: "invalid runtime configuration"}
	}
	switch cfg.ProgressMode {
	case "off", "summary", "verbose":
		return doctorCheck{Name: "Config", Level: doctorOK}
	default:
		return doctorCheck{Name: "Config", Level: doctorFail, Message: "invalid runtime configuration"}
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
		return doctorCheck{Name: "Telegram", Level: doctorFail, Message: "Bot API unavailable"}
	}
	return doctorCheck{Name: "Telegram", Level: doctorOK}
}

func checkDoctorDatabase(ctx context.Context, db *store.Store) doctorCheck {
	if db == nil {
		return doctorCheck{Name: "Database", Level: doctorFail, Message: "SQLite check unavailable"}
	}
	if err := db.QuickCheck(ctx); err != nil {
		if err.Error() == "sqlite quick check failed" {
			return doctorCheck{Name: "Database", Level: doctorFail, Message: "SQLite quick check failed"}
		}
		return doctorCheck{Name: "Database", Level: doctorFail, Message: "SQLite check unavailable"}
	}
	return doctorCheck{Name: "Database", Level: doctorOK}
}

func checkDoctorStorage(dataDir string) doctorCheck {
	fail := func() doctorCheck {
		return doctorCheck{Name: "Storage", Level: doctorFail, Message: "data directory is not writable"}
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
	return doctorCheck{Name: "Storage", Level: doctorOK}
}

func checkDoctorOMPWithTimeout(parent context.Context, binary string) doctorCheck {
	ctx, cancel := context.WithTimeout(parent, doctorOMPTimeout)
	defer cancel()
	if err := doctorRunOMP(ctx, binary); err != nil {
		return doctorCheck{Name: "OMP binary", Level: doctorFail, Message: "executable check failed"}
	}
	return doctorCheck{Name: "OMP binary", Level: doctorOK}
}

func checkDoctorOMP(ctx context.Context, binary string) error {
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("omp binary is unavailable")
	}
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func checkDoctorWorkspaceSession(binding doctorBindingSnapshot, runtime runtimeState) (doctorCheck, doctorCheck) {
	if !binding.exists {
		return doctorCheck{Name: "Workspace", Level: doctorWarn, Message: "not selected"}, doctorCheck{Name: "Session", Level: doctorWarn, Message: "not selected"}
	}
	workspace := doctorCheck{Name: "Workspace", Level: doctorFail, Message: "directory unavailable"}
	if filepath.IsAbs(binding.workspace) {
		if info, err := os.Stat(binding.workspace); err == nil && info.IsDir() {
			workspace.Level = doctorOK
			workspace.Message = ""
		}
	}
	session := checkDoctorSession(binding, runtime)
	return workspace, session
}

func checkDoctorSession(binding doctorBindingSnapshot, runtime runtimeState) doctorCheck {
	if binding.session == "" {
		return doctorCheck{Name: "Session", Level: doctorWarn, Message: "not selected"}
	}
	available := filepath.IsAbs(binding.session)
	if available {
		info, err := os.Stat(binding.session)
		available = err == nil && info.Mode().IsRegular()
	}
	if !available {
		if binding.running && runtime != runtimeReleased {
			return doctorCheck{Name: "Session", Level: doctorWarn, Message: "session history not persisted yet"}
		}
		if binding.running {
			return doctorCheck{Name: "Session", Level: doctorFail, Message: "released session file unavailable"}
		}
		return doctorCheck{Name: "Session", Level: doctorWarn, Message: "saved session file is unavailable"}
	}
	message := ""
	if validSessionID(binding.sessionID) {
		message = "[" + shortSessionID(binding.sessionID) + "]"
	}
	return doctorCheck{Name: "Session", Level: doctorOK, Message: message}
}

func checkDoctorRuntime(runtime runtimeState, binding doctorBindingSnapshot) doctorCheck {
	status := "Closed"
	switch runtime {
	case runtimeConnected:
		status = "Connected"
	case runtimeStarting:
		status = "Starting"
	case runtimeReleased:
		if binding.exists && binding.running {
			status = "Released"
		}
	default:
		return doctorCheck{Name: "Runtime", Level: doctorFail, Message: "inconsistent internal runtime state"}
	}
	return doctorCheck{Name: "Runtime", Level: doctorOK, Message: status}
}

func checkDoctorUncertain(ctx context.Context, db *store.Store) (doctorCheck, doctorCheck) {
	if db == nil {
		return doctorCheck{Name: "Inbox", Level: doctorWarn, Message: "count unavailable"}, doctorCheck{Name: "Outbox", Level: doctorWarn, Message: "count unavailable"}
	}
	counts, err := db.UncertainCounts(ctx)
	if err != nil {
		return doctorCheck{Name: "Inbox", Level: doctorWarn, Message: "count unavailable"}, doctorCheck{Name: "Outbox", Level: doctorWarn, Message: "count unavailable"}
	}
	inbox := doctorCheck{Name: "Inbox", Level: doctorOK}
	if counts.Inbox > 0 {
		inbox.Level = doctorWarn
		inbox.Message = fmt.Sprintf("%d uncertain", counts.Inbox)
	}
	outbox := doctorCheck{Name: "Outbox", Level: doctorOK}
	if counts.Outbox > 0 {
		outbox.Level = doctorWarn
		outbox.Message = fmt.Sprintf("%d uncertain", counts.Outbox)
	}
	return inbox, outbox
}

func checkDoctorDisk(dataDir string) doctorCheck {
	free, err := doctorStatfs(dataDir)
	if err != nil {
		return doctorCheck{Name: "Disk", Level: doctorWarn, Message: "free space unavailable"}
	}
	level := doctorOK
	if free < doctorDiskFail {
		level = doctorFail
	} else if free < doctorDiskWarn {
		level = doctorWarn
	}
	return doctorCheck{Name: "Disk", Level: level, Message: formatDoctorFreeSpace(free)}
}

func formatDoctorFreeSpace(free uint64) string {
	if free >= 1<<30 {
		return fmt.Sprintf("%.1f GiB free", float64(free)/(1<<30))
	}
	return fmt.Sprintf("%d MiB free", free/(1<<20))
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

func doctorLevelName(level doctorLevel) string {
	switch level {
	case doctorWarn:
		return "WARN"
	case doctorFail:
		return "FAIL"
	default:
		return "OK"
	}
}

func doctorResultName(level doctorLevel) string {
	return strings.ToLower(doctorLevelName(level))
}

func formatDoctorReport(checks []doctorCheck) string {
	var report strings.Builder
	report.WriteString("omp-telegram diagnostics")
	for _, check := range checks {
		report.WriteByte('\n')
		report.WriteString(fmt.Sprintf("%-12s", check.Name))
		if check.Name == "Runtime" && check.Level == doctorOK {
			report.WriteByte(' ')
			report.WriteString(check.Message)
			continue
		}
		report.WriteByte(' ')
		report.WriteString(doctorLevelName(check.Level))
		if check.Message != "" {
			report.WriteString(" - ")
			report.WriteString(check.Message)
		}
	}
	report.WriteString("\n\nResult: ")
	report.WriteString(doctorLevelName(doctorOverall(checks)))
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
		message := "Diagnostics unavailable. Run /doctor again."
		if errors.Is(result.err, context.DeadlineExceeded) {
			message = "Diagnostics timed out. Run /doctor again."
		}
		w.log.Warn("doctor diagnostics failed", "event", "doctor", "result", "fail")
		w.say(message)
		return
	}
	current, err := readDoctorBinding(w.b.db, w.b.bot.ID, w.key)
	if err != nil {
		if w.ctx.Err() == nil {
			w.log.Warn("doctor binding snapshot failed", "event", "doctor", "result", "fail")
			w.say("Diagnostics unavailable. Run /doctor again.")
		}
		return
	}
	clientID := uint64(0)
	if w.client != nil {
		clientID = w.client.ID()
	}
	if result.fence.runtime != w.runtime || result.fence.clientID != clientID || !sameDoctorBinding(result.fence.binding, current) {
		w.log.Info("doctor diagnostics discarded", "event", "doctor", "result", "stale")
		w.say("State changed during diagnostics. Run /doctor again.")
		return
	}
	level := doctorOverall(result.checks)
	w.log.Info("doctor diagnostics completed", "event", "doctor", "result", doctorResultName(level))
	w.say(formatDoctorReport(result.checks))
}
