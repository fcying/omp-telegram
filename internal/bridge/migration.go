package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
)

const migrationPollInterval = 100 * time.Millisecond

var errMigrationLease = errors.New("migration runtime identity changed")

// A run owns only transient RPC work. The durable migration remains locked after it ends.
type migrationRun struct {
	id       string
	ctx      context.Context
	inboxID  int64
	request  omp.MigrationRequest
	recovery bool
	old      bool
	args     []string
}

// Pending starts must be reaped before another recovery may open the same journal.
type migrationStartup struct {
	attempt uint64
	ctx     context.Context
	cancel  context.CancelFunc
}

// Only pending startup work inherits its phase deadline; an adopted runtime does not.
func (s *migrationStartup) run(ctx context.Context, start func(context.Context) migrationResult) migrationResult {
	stop := context.AfterFunc(ctx, s.cancel)
	defer stop()
	result := start(s.ctx)
	if !stop() || ctx.Err() != nil {
		// Cancellation may race with the final handshake. Never adopt that process.
		s.cancel()
		result.err = ctx.Err()
	}
	return result
}

type migrationResult struct {
	id       string
	attempt  uint64
	clientID uint64
	stage    string
	info     omp.SessionInfo
	settings omp.MigrationSettings
	request  omp.MigrationRequest
	client   *omp.Client
	err      error
}

func (w *worker) migrationLocked() bool { return w.migration != nil || w.migrationRun != nil }

func (w *worker) migrationStatus() string {
	if w.migration == nil {
		return "正在检查迁移前置条件."
	}
	m := w.migration
	text := fmt.Sprintf("迁移尚未解决.\n操作: %s\n阶段: %s\n源 Session: %s", m.ID, m.Phase, m.Source.SessionID)
	if m.CandidateID != "" {
		text += "\n候选 Session: " + m.CandidateID
	}
	if m.ErrorKind != "" {
		text += "\n状态: " + m.ErrorKind
	}
	return text + "\n普通输入已锁定. 使用 /migrate recover old 恢复源会话, 或 /migrate recover <session-id 或绝对文件路径> 核验候选. 不会自动重新创建 Session."
}

// This gate precedes media preparation, slash routing and callback execution.
func (w *worker) handleMigrationIncoming(in incoming) bool {
	if in.callback != nil {
		if !w.migrationLocked() {
			return false
		}
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
		defer cancel()
		_ = w.b.tg.AnswerCallback(ctx, in.callback.ID, "会话迁移尚未解决.")
		w.mark(in.id, store.InboxCancelled)
		return true
	}
	if in.msg == nil {
		return false
	}
	attached := len(in.msg.Photo) != 0 || in.msg.Document != nil
	text := strings.TrimSpace(in.msg.Text)
	command, tail, foreign := parseSlashInvocation(text, w.b.bot.Username)
	if foreign {
		return false
	}
	if !attached && command == "/migrate" {
		if !w.mark(in.id, store.InboxSubmitted) {
			return true
		}
		arg := slashCommandArguments(tail)
		action, target, _ := strings.Cut(arg, " ")
		if arg == "" {
			w.beginMigration(in.id)
		} else if action == "recover" && strings.TrimSpace(target) != "" {
			w.recoverMigration(in.id, strings.TrimSpace(target))
		} else {
			w.say("用法: /migrate, /migrate recover old, 或 /migrate recover <session-id 或绝对文件路径>.")
			w.mark(in.id, store.InboxDone)
		}
		return true
	}
	if !w.migrationLocked() {
		return false
	}
	if !attached {
		switch command {
		case "/status":
			w.say(w.migrationStatus())
			w.mark(in.id, store.InboxDone)
			return true
		case "/help", "/start":
			w.say(commandHelp(w.b.version))
			w.mark(in.id, store.InboxDone)
			return true
		case "/stop", "/close":
			w.cancelMigration("cancelled", true)
			if command == "/close" && w.migration == nil {
				w.closeLogicalSession()
			}
			w.mark(in.id, store.InboxDone)
			return true
		}
	}
	w.mark(in.id, store.InboxCancelled)
	w.say("迁移尚未解决, 本条输入未排队也未提交. 使用 /status 查看显式恢复方式.")
	return true
}

func newMigrationID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func (w *worker) beginMigration(inboxID int64) {
	if w.migrationLocked() {
		w.say(w.migrationStatus())
		w.mark(inboxID, store.InboxDone)
		return
	}
	if w.sessionControlBusy() || w.taskActive() || len(w.queue) != 0 || len(w.albums) != 0 ||
		w.previewBusy || len(w.hostRequests) != 0 || w.startIntent != nil || w.resumeCancel != nil ||
		w.exportCancel != nil || w.deleteCancel != nil || w.hasRuntimeConfirmation() || w.rpcOperationActive || len(w.rpcOperations) != 0 {
		w.say("仅允许在任务、队列及会话控制全部空闲时迁移.")
		w.mark(inboxID, store.InboxDone)
		return
	}
	client, err := w.ensureRuntime()
	if err != nil {
		w.say("无法恢复源 Session, 未开始迁移.")
		w.finishMigrationInput(inboxID, store.InboxFailed)
		return
	}
	id, err := newMigrationID()
	if err != nil {
		w.say("无法创建迁移操作 ID.")
		w.finishMigrationInput(inboxID, store.InboxFailed)
		return
	}
	ctx, cancel := context.WithCancel(w.ctx)
	w.migrationCancel = cancel
	w.migrationClientID = client.ID()
	w.migrationAttempt++
	w.migrationRun = &migrationRun{id: id, ctx: ctx, inboxID: inboxID, args: cloneArgv(w.launchArgs)}
	source := w.binding
	w.startMigrationJob("prepare", 30*time.Second, func(ctx context.Context) migrationResult {
		result := migrationResult{}
		_, err := client.RefreshCommandCatalog(ctx)
		if err != nil || !client.CommandCatalog().HasExtensionCommand(omp.MigrationCommand) ||
			!client.CommandCatalog().HasExtensionCommand(omp.MigrationHandoffCommand) {
			result.err = errors.New("migration extension unavailable")
			return result
		}
		result.info, result.err = client.SessionInfo(ctx)
		if result.err != nil {
			return result
		}
		if !strings.EqualFold(result.info.ID, source.SessionID) || !sameSessionFile(result.info.File, source.Session) || !sameWorkspace(result.info.CWD, source.Workspace) {
			result.err = errMigrationLease
			return result
		}
		result.settings, result.err = client.MigrationSettings(ctx)
		return result
	})
}

func (w *worker) startMigrationJob(stage string, timeout time.Duration, work func(context.Context) migrationResult) {
	run := w.migrationRun
	if run == nil {
		return
	}
	if w.migrationResults == nil {
		w.migrationResults = make(chan migrationResult, 4)
	}
	results := w.migrationResults
	clientID := w.migrationClientID
	attempt := w.migrationAttempt
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		ctx, cancel := context.WithTimeout(run.ctx, timeout)
		defer cancel()
		result := work(ctx)
		result.id, result.clientID, result.stage, result.attempt = run.id, clientID, stage, attempt
		if stage == "candidate" && result.info.ID != "" {
			_, err := w.b.observeMigrationCandidate(w, run.id, result.info.File, result.info.ID)
			if err != nil {
				result.err = err
			}
		}
		done := run.ctx.Done()
		if stage == "recover-start" {
			// The actor must receive and reap a canceled startup before permitting another.
			done = w.ctx.Done()
		}
		select {
		case results <- result:
		case <-done:
			if result.client != nil {
				result.client.Close()
				<-w.b.slots
			}
		}
	}()
}

func (w *worker) saveMigration(next store.Migration) bool {
	if w.migration == nil {
		return false
	}
	if err := w.b.db.UpdateMigration(next, w.migration.Phase); err != nil {
		w.b.fail(err)
		w.cancelMigration("persistence", true)
		return false
	}
	w.migration = &next
	return true
}

func (w *worker) migrationFinished(result migrationResult) {
	run := w.migrationRun
	if run == nil || result.id != run.id || result.attempt != w.migrationAttempt || result.clientID != w.migrationClientID || run.ctx.Err() != nil {
		if result.stage == "candidate" && w.migration != nil && result.id == w.migration.ID {
			if current, err := w.b.db.Migration(w.b.bot.ID, w.key.chat, w.key.thread); err == nil && current.ID == result.id {
				w.migration = &current
			}
		}
		w.discardMigrationClient(result)
		return
	}
	if result.stage == "recover-resolve" {
		if result.err != nil {
			w.cancelMigration("recovery_failed", true)
			return
		}
		if !run.old && !w.recordMigrationCandidate(result.info) {
			return
		}
		w.startMigrationRecoveryRuntime(result.info)
		return
	}
	if result.stage == "recover-start" {
		w.migrationRecoveryStarted(result)
		return
	}
	client, connected := w.runtimeClient()
	if !connected || client.ID() != result.clientID || client.Stopping() {
		w.cancelMigration("runtime_changed", true)
		return
	}
	if result.err != nil {
		w.cancelMigration("unconfirmed", true)
		return
	}
	switch result.stage {
	case "prepare":
		settings := result.settings
		m := store.Migration{ID: run.id, Source: w.binding, InboxID: run.inboxID, Phase: "prepared", Provider: settings.Provider, Model: settings.Model, Thinking: settings.Thinking, Fast: settings.Fast, CreatedAt: time.Now().Unix()}
		if err := w.b.db.PrepareMigration(m); err != nil {
			w.cancelMigration("prepare_failed", true)
			return
		}
		prepared := m
		w.migration = &prepared
		if !w.restoreMigrationClaims() {
			w.cancelMigration("claim_conflict", true)
			return
		}
		m.Phase = "handoff"
		if !w.saveMigration(m) {
			return
		}
		w.clearConfirmations()
		w.say("开始迁移. Handoff 最多等待 180s, 完成验证前不会切换绑定.")
		source := m.Source
		w.startMigrationJob("handoff", 180*time.Second, func(ctx context.Context) migrationResult {
			cursor, err := omp.CaptureHandoff(source.Session, source.SessionID, source.Workspace)
			if err != nil {
				return migrationResult{err: err}
			}
			raw, err := client.MigrationHandoff(ctx, m.ID, omp.SessionInfo{ID: source.SessionID, CWD: source.Workspace})
			if err != nil {
				return migrationResult{err: err}
			}
			var response *struct{}
			if json.Unmarshal(raw, &response) != nil || response == nil {
				return migrationResult{err: errors.New("handoff did not confirm completion")}
			}
			summary, entryID, hash, err := omp.ReadHandoff(cursor)
			return migrationResult{request: omp.MigrationRequest{Version: 1, MigrationID: m.ID, SourceSessionID: source.SessionID, SourceGeneration: source.Generation, Workspace: source.Workspace, Summary: summary, SummarySHA256: hash}, info: omp.SessionInfo{ID: entryID}, err: err}
		})
	case "handoff":
		prompt, err := omp.MigrationPrompt(result.request)
		if err != nil {
			w.cancelMigration("summary_invalid", true)
			return
		}
		m := *w.migration
		m.SummaryEntryID, m.SummaryHash, m.Phase = result.info.ID, result.request.SummarySHA256, "initialize"
		if !w.saveMigration(m) {
			return
		}
		run.request = result.request
		w.startMigrationJob("candidate", 30*time.Second, func(ctx context.Context) migrationResult {
			_, submissionErr := client.Call(ctx, "prompt", map[string]any{"message": prompt})
			return awaitMigrationCandidate(ctx, client, m.Source, submissionErr)
		})
	case "candidate":
		if !w.recordMigrationCandidate(result.info) {
			return
		}
		m := *w.migration
		m.Phase = "verify"
		if !w.saveMigration(m) {
			return
		}
		w.verifyMigrationCandidate()
	case "verify":
		w.commitMigration(result.info)
	case "recover-verify":
		w.commitMigration(result.info)
	}
}

func awaitMigrationCandidate(ctx context.Context, client *omp.Client, source store.Binding, submissionErr error) migrationResult {
	timer := time.NewTicker(migrationPollInterval)
	defer timer.Stop()
	for {
		raw, err := client.Call(ctx, "get_state", nil)
		if err != nil {
			return migrationResult{err: err}
		}
		var state struct {
			ID   string `json:"sessionId"`
			File string `json:"sessionFile"`
		}
		if json.Unmarshal(raw, &state) != nil || !validSessionID(state.ID) {
			return migrationResult{err: errMigrationLease}
		}
		if !strings.EqualFold(state.ID, source.SessionID) {
			if !filepath.IsAbs(state.File) || filepath.Clean(state.File) == filepath.Clean(source.Session) {
				return migrationResult{err: errMigrationLease}
			}
			return migrationResult{info: omp.SessionInfo{ID: state.ID, File: state.File, CWD: source.Workspace}}
		}
		if submissionErr != nil {
			return migrationResult{err: submissionErr}
		}
		select {
		case <-ctx.Done():
			return migrationResult{err: ctx.Err()}
		case <-timer.C:
		}
	}
}

func migrationSettings(m store.Migration) omp.MigrationSettings {
	return omp.MigrationSettings{Provider: m.Provider, Model: m.Model, Thinking: m.Thinking, Fast: m.Fast}
}

func migrationRequest(m store.Migration) omp.MigrationRequest {
	return omp.MigrationRequest{Version: 1, MigrationID: m.ID, SourceSessionID: m.Source.SessionID, SourceGeneration: m.Source.Generation, Workspace: m.Source.Workspace, SummarySHA256: m.SummaryHash}
}

func (w *worker) verifyMigrationCandidate() {
	m := *w.migration
	request := w.migrationRun.request
	if request.MigrationID == "" {
		request = migrationRequest(m)
	}
	client := w.client
	stage := "verify"
	if w.migrationRun.recovery {
		stage = "recover-verify"
	}
	w.startMigrationJob(stage, 30*time.Second, func(ctx context.Context) migrationResult {
		timer := time.NewTicker(migrationPollInterval)
		defer timer.Stop()
		target := omp.SessionInfo{ID: m.CandidateID, File: m.CandidateFile, CWD: m.Source.Workspace}
		for {
			err := client.VerifyMigration(ctx, request, target, migrationSettings(m))
			if err == nil {
				info, err := client.SessionInfo(ctx)
				if err == nil && (!strings.EqualFold(info.ID, target.ID) || !sameSessionFile(info.File, target.File) || !sameWorkspace(info.CWD, target.CWD)) {
					err = errMigrationLease
				}
				if err == nil {
					_, err = client.Call(ctx, "set_host_tools", map[string]any{"tools": telegramSendTools})
				}
				return migrationResult{info: info, err: err}
			}
			if !errors.Is(err, omp.ErrMigrationMarkerNotReady) && !errors.Is(err, omp.ErrMigrationNotSettled) {
				return migrationResult{err: err}
			}
			select {
			case <-ctx.Done():
				return migrationResult{err: ctx.Err()}
			case <-timer.C:
			}
		}
	})
}

func (w *worker) commitMigration(info omp.SessionInfo) {
	m := *w.migration
	run := w.migrationRun
	if run == nil || run.ctx.Err() != nil || w.client == nil || w.client.ID() != w.migrationClientID || w.client.Stopping() {
		w.cancelMigration("runtime_changed", true)
		return
	}
	target := m.Source
	target.Session, target.SessionID, target.Workspace = info.File, info.ID, info.CWD
	target.Generation++
	target.Running, target.Interrupted, target.LastUsedAt = true, false, time.Now().Unix()
	notice := "迁移完成.\nWorkspace: " + info.CWD + "\nSession: " + info.ID + "\n旧 Session 已保留, 可通过 /resume 恢复."
	var err error
	if run.recovery {
		notice = "迁移已显式恢复.\nWorkspace: " + info.CWD + "\nSession: " + info.ID
		err = w.b.db.ResolveMigration(m, target, notice)
	} else {
		err = w.b.db.CommitMigration(m, target, notice)
	}
	if err != nil {
		w.cancelMigration("commit_failed", true)
		return
	}
	if !w.finishMigrationClaims(info.File, info.ID) {
		w.b.fail(errors.New("committed migration claim unavailable"))
		w.cancel()
		return
	}
	w.binding = target
	w.configuredThinking = m.Thinking
	w.migration, w.migrationRun = nil, nil
	if w.migrationCancel != nil {
		w.migrationCancel()
		w.migrationCancel = nil
	}
	w.migrationClientID = 0
	if run.recovery && run.inboxID != m.InboxID {
		w.mark(run.inboxID, store.InboxDone)
	}
	w.touchActivity()
	w.refreshCommandCatalog(w.client)
}

func (w *worker) cancelMigration(kind string, notify bool) {
	run := w.migrationRun
	if w.migrationCancel != nil {
		w.migrationCancel()
		w.migrationCancel = nil
	}
	if w.migrationStartup != nil {
		w.migrationStartup.cancel()
	}
	w.migrationRun = nil
	w.migrationClientID = 0
	if w.migration != nil {
		w.b.sessionMu.Lock()
		m, err := w.b.db.FailMigration(w.migration.ID, kind)
		w.b.sessionMu.Unlock()
		if err != nil {
			w.b.fail(err)
		} else {
			w.migration = &m
		}
	} else if run != nil {
		w.finishMigrationInput(run.inboxID, store.InboxFailed)
	}
	if run != nil && run.recovery && w.migration != nil && run.inboxID != w.migration.InboxID {
		w.finishMigrationInput(run.inboxID, store.InboxUncertain)
	}
	w.clearQueue()
	w.releaseRuntimeWithReason(true, "migration")
	if notify && w.ctx.Err() == nil {
		if w.migration != nil {
			w.say("迁移未确认, Runtime 已停止, 绑定和迁移锁已保留. 不会自动重试.\n" + w.migrationStatus())
		} else {
			w.say("未开始迁移. 请确认已通过 omp.args 显式加载 omp_telegram_migrate_v1 Extension, 且源 Session 可恢复并已完全空闲.")
		}
	}
}

func (w *worker) finishMigrationInput(id int64, state store.InboxState) {
	if id != 0 {
		if err := w.b.db.FinishSubmitted(id, state); err != nil {
			w.b.fail(err)
			w.cancel()
		}
	}
}

// Late identity observations remain useful evidence, but can never authorize a commit.
func (w *worker) recordMigrationCandidate(info omp.SessionInfo) bool {
	if w.migration == nil || !validSessionID(info.ID) || !filepath.IsAbs(info.File) || strings.EqualFold(info.ID, w.migration.Source.SessionID) {
		return false
	}
	m, err := w.b.observeMigrationCandidate(w, w.migration.ID, info.File, info.ID)
	if err != nil {
		w.cancelMigration("candidate_conflict", true)
		return false
	}
	w.migration = &m
	return true
}

func (w *worker) recoverMigration(inboxID int64, target string) {
	if w.migration == nil {
		w.say("没有待解决的迁移.")
		w.mark(inboxID, store.InboxDone)
		return
	}
	if w.migrationRun != nil {
		w.say("迁移操作仍在执行. 先使用 /stop 停止 Runtime, 再显式恢复.")
		w.mark(inboxID, store.InboxDone)
		return
	}
	if w.migrationStartup != nil {
		w.say("上一次恢复进程仍在退出. 等待回收后再显式恢复, 不会并发启动同一 Session.")
		w.mark(inboxID, store.InboxDone)
		return
	}
	if target != "old" && !validSessionID(target) && !filepath.IsAbs(target) {
		w.say("请提供 old, 完整 Session ID, 或新 JSONL 的绝对路径.")
		w.mark(inboxID, store.InboxDone)
		return
	}
	if strings.EqualFold(target, w.migration.Source.SessionID) {
		w.say("恢复源 Session 请使用 /migrate recover old.")
		w.mark(inboxID, store.InboxDone)
		return
	}
	if w.client != nil {
		w.releaseRuntimeWithReason(true, "migration")
	}
	m := *w.migration
	ctx, cancel := context.WithCancel(w.ctx)
	w.migrationCancel = cancel
	w.migrationClientID = 0
	w.migrationAttempt++
	args := w.b.selectedArgsSnapshot()
	w.migrationRun = &migrationRun{id: m.ID, ctx: ctx, inboxID: inboxID, recovery: true, old: target == "old", args: args}
	cfg := omp.Config{Binary: w.b.cfg.OMP, CWD: m.Source.Workspace, Args: args, Environment: w.b.cfg.OMPEnvironment}
	w.startMigrationJob("recover-resolve", 30*time.Second, func(ctx context.Context) migrationResult {
		if target == "old" {
			stat, err := os.Lstat(m.Source.Session)
			if err != nil || !stat.Mode().IsRegular() {
				return migrationResult{err: errors.New("source session unavailable")}
			}
			return migrationResult{info: omp.SessionInfo{ID: m.Source.SessionID, File: m.Source.Session, CWD: m.Source.Workspace}}
		}
		path := m.CandidateFile
		expectedID := m.CandidateID
		if filepath.IsAbs(target) {
			path = filepath.Clean(target)
			if m.CandidateFile != "" && path != m.CandidateFile {
				return migrationResult{err: errMigrationLease}
			}
		} else if expectedID != "" {
			if !strings.EqualFold(target, expectedID) {
				return migrationResult{err: errMigrationLease}
			}
		} else {
			var err error
			path, err = omp.ResolveSessionPath(ctx, cfg, target)
			if err != nil {
				return migrationResult{err: err}
			}
			expectedID = target
		}
		info, err := omp.InspectMigrationCandidate(migrationRequest(m), path)
		if err == nil && expectedID != "" && !strings.EqualFold(info.ID, expectedID) {
			err = errMigrationLease
		}
		return migrationResult{info: info, err: err}
	})
}

func (w *worker) startMigrationRecoveryRuntime(expected omp.SessionInfo) {
	select {
	case w.b.slots <- struct{}{}:
	default:
		w.cancelMigration("capacity", true)
		return
	}
	run := w.migrationRun
	startupCtx, startupCancel := context.WithCancel(w.ctx)
	startup := &migrationStartup{attempt: w.migrationAttempt, ctx: startupCtx, cancel: startupCancel}
	w.migrationStartup = startup
	cfg := omp.Config{Binary: w.b.cfg.OMP, CWD: expected.CWD, Resume: expected.File, Args: run.args, Environment: w.b.cfg.OMPEnvironment}
	rpcLog := w.b.rpcLog
	w.startMigrationJob("recover-start", 45*time.Second, func(ctx context.Context) migrationResult {
		return startup.run(ctx, func(startupCtx context.Context) migrationResult {
			client, err := omp.Start(startupCtx, cfg, rpcLog)
			if err != nil {
				<-w.b.slots
				return migrationResult{err: err}
			}
			if ctx.Err() != nil {
				client.Close()
				<-w.b.slots
				return migrationResult{err: ctx.Err()}
			}
			info, err := client.SessionInfo(ctx)
			if err == nil && (!strings.EqualFold(info.ID, expected.ID) || !sameSessionFile(info.File, expected.File) || !sameWorkspace(info.CWD, expected.CWD)) {
				err = errMigrationLease
			}
			return migrationResult{client: client, info: info, err: err}
		})
	})
}

func (w *worker) migrationRecoveryStarted(result migrationResult) {
	if result.client == nil {
		w.discardMigrationClient(result)
		w.cancelMigration("recovery_failed", true)
		return
	}
	if result.err != nil {
		w.discardMigrationClient(result)
		w.cancelMigration("recovery_failed", true)
		return
	}
	run := w.migrationRun
	if w.migrationStartup == nil || w.migrationStartup.attempt != result.attempt {
		w.discardMigrationClient(result)
		w.cancelMigration("startup_changed", true)
		return
	}
	w.migrationRuntimeCancel = w.migrationStartup.cancel
	w.migrationStartup = nil
	w.runtimeStarted(result.client, run.args)
	w.initMedia()
	w.migrationClientID = result.client.ID()
	if !run.old {
		if result.info.ID != w.migration.CandidateID || result.info.File != w.migration.CandidateFile {
			w.cancelMigration("runtime_changed", true)
			return
		}
		w.verifyMigrationCandidate()
		return
	}
	client, info := result.client, result.info
	w.startMigrationJob("recover-verify", 30*time.Second, func(ctx context.Context) migrationResult {
		_, err := client.Call(ctx, "set_host_tools", map[string]any{"tools": telegramSendTools})
		return migrationResult{info: info, err: err}
	})
}

func (w *worker) discardMigrationClient(result migrationResult) {
	if result.client != nil {
		result.client.Close()
		<-w.b.slots
	}
	if result.stage == "recover-start" && w.migrationStartup != nil && w.migrationStartup.attempt == result.attempt {
		w.migrationStartup.cancel()
		w.migrationStartup = nil
	}
}

func (w *worker) drainMigrationResults() {
	for {
		select {
		case result := <-w.migrationResults:
			w.discardMigrationClient(result)
		default:
			return
		}
	}
}
