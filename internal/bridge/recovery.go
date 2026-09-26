package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func (b *Bridge) newWorker(ctx context.Context, key target, binding store.Binding, restoring bool, intent *store.StartIntent) *worker {
	ctx, cancel := context.WithCancel(ctx)
	workerLog := b.log.With("chat_id", key.chat, "thread_id", key.thread)
	return &worker{
		b: b, log: workerLog, key: key, binding: binding, runtimeLifecycle: runtimeLifecycle{restoring: restoring}, startIntent: intent,
		input:    make(chan incoming, b.cfg.QueueCapacity+16),
		confirms: map[string]confirmation{}, previewResult: make(chan previewResult, 1),
		forgetRequests: make(chan bindingForgetRequest, 1), forgetResults: make(chan bindingForgetResult, 1),
		topicRenameResults: make(chan topicRenameResult, 1), operations: make(chan operationResult, 1), ctx: ctx, cancel: cancel,
	}
}

func (b *Bridge) runWorker(w *worker) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		w.run()
		w.markExitRequested()
		select {
		case request := <-w.forgetRequests:
			request.reply(bindingForgetResult{status: "Binding changed. Use /bindings to refresh."})
		default:
		}
		if b.workerExits == nil || b.ctx == nil {
			return
		}
		select {
		case b.workerExits <- workerExit{key: w.key, worker: w}:
		case <-b.ctx.Done():
		}
	}()
}

func (b *Bridge) launchWorker(ctx context.Context, key target, binding store.Binding, restoring bool, intent *store.StartIntent) *worker {
	w := b.newWorker(ctx, key, binding, restoring, intent)
	b.runWorker(w)
	return w
}

func removeExitedWorker(workers map[target]*worker, exit workerExit) {
	if workers[exit.key] == exit.worker {
		delete(workers, exit.key)
	}
}

func (b *Bridge) bindingForWorker(key target) (store.Binding, error) {
	binding, err := b.db.Binding(b.bot.ID, key.chat, key.thread)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Binding{Bot: b.bot.ID, Chat: key.chat, Thread: key.thread}, nil
	}
	return binding, err
}

func savedSessionAvailable(session, workspace string) bool {
	if !filepath.IsAbs(session) || !filepath.IsAbs(workspace) {
		return false
	}
	file, err := os.Stat(session)
	if err != nil || !file.Mode().IsRegular() {
		return false
	}
	dir, err := os.Stat(workspace)
	return err == nil && dir.IsDir()
}

func (w *worker) restoreBinding() {
	w.restoring = false
	if !savedSessionAvailable(w.binding.Session, w.binding.Workspace) {
		sessionID := w.sessionID
		if w.persistClosed() {
			w.releaseSession()
			w.logRuntimeEvent(slog.LevelInfo, "restore_runtime_skipped", "session_file_unavailable", "startup restore skipped", sessionID)
			w.say("The saved omp session file or working directory is unavailable, so startup restore was skipped. Use /new to start a new session.")
		}
		return
	}
	if !w.binding.Interrupted {
		return
	}
	if err := w.b.db.SetInterrupted(w.binding, false); err != nil {
		w.b.storeLog.Error("interrupted state persistence failed", "event", "binding_write_failed", "reason", "clear_interrupted", "error_kind", "persistence")
		w.b.fail(err)
		w.cancel()
		return
	}
	w.binding.Interrupted = false
	w.say("⚠️ Gateway restarted while the previous task was active. It was interrupted and was not resubmitted.")
}

func (b *Bridge) restoreWorkers(ctx context.Context, workers map[target]*worker) error {
	intents, err := b.db.PendingStarts(b.bot.ID)
	if err != nil {
		b.storeLog.Error("startup intent read failed", "event", "binding_read_failed", "reason", "pending_starts", "error_kind", "persistence")
		return err
	}
	bindings, err := b.db.RunningBindings(b.bot.ID)
	if err != nil {
		b.storeLog.Error("session binding read failed", "event", "binding_read_failed", "reason", "running_bindings", "error_kind", "persistence")
		return err
	}
	// Snapshot before polling starts: saved prompts must not restart with their bindings.
	if err := b.cancelPendingPrompts("restore"); err != nil {
		return err
	}
	restored := make([]*worker, 0, len(bindings))
	for _, binding := range bindings {
		if ctx.Err() != nil {
			break
		}
		if (binding.Thread == 0 && binding.Chat <= 0) || !slices.Contains(b.cfg.AllowedChats, binding.Chat) {
			continue
		}
		key := target{binding.Chat, binding.Thread}
		w := b.newWorker(ctx, key, binding, true, nil)
		if !w.claimPersistedSession() {
			w.log.Error("saved session claim failed", "event", "restore_claim", "generation", binding.Generation, "result", "failed", "reason", "identity_conflict")
			return errors.New("saved session identity is unavailable or claimed by another conversation")
		}
		w.log.Debug("saved session claim restored", "event", "restore_claim", "generation", binding.Generation, "session_id", binding.SessionID, "result", "done")
		workers[key] = w
		restored = append(restored, w)
	}
	for _, w := range restored {
		b.runWorker(w)
	}
	for i := range intents {
		intent := &intents[i]
		if ctx.Err() != nil {
			break
		}
		if (intent.Thread == 0 && intent.Chat <= 0) || !slices.Contains(b.cfg.AllowedChats, intent.Chat) {
			continue
		}
		key := target{intent.Chat, intent.Thread}
		if workers[key] != nil {
			continue
		}
		binding, err := b.db.Binding(intent.Bot, intent.Chat, intent.Thread)
		if errors.Is(err, sql.ErrNoRows) {
			binding = store.Binding{Bot: intent.Bot, Chat: intent.Chat, Thread: intent.Thread}
		} else if err != nil {
			b.storeLog.Error("session binding read failed", "event", "binding_read_failed", "reason", "restore_intent", "error_kind", "persistence")
			return err
		}
		workers[key] = b.launchWorker(ctx, key, binding, false, intent)
	}
	return nil
}

func (b *Bridge) cancelPendingPrompts(reason string) error {
	offset, err := b.db.Offset()
	if err != nil {
		b.storeLog.Error("pending input offset read failed", "event", "inbox_read_failed", "reason", reason, "error_kind", "persistence")
		return err
	}
	cursor := int64(-1)
	throughID := offset - 1
	for {
		pending, err := b.db.PendingAfter(cursor, throughID, pendingInputBatchSize)
		if err != nil {
			b.storeLog.Error("pending input read failed", "event", "inbox_read_failed", "reason", reason, "error_kind", "persistence")
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		cursor = pending[len(pending)-1].ID
		for _, in := range pending {
			var update telegram.Update
			if json.Unmarshal(in.Raw, &update) != nil || update.Message == nil || !deferredPrompt(update.Message, b.bot.Username) {
				continue
			}
			if err := b.db.Mark(in.ID, "cancelled"); err != nil {
				b.storeLog.Error("input cancellation persistence failed", "event", "inbox_state_write_failed", "reason", reason, "inbox_id", in.ID, "error_kind", "persistence")
				return err
			}
		}
	}
}

func deferredPrompt(message *telegram.Message, botUsername string) bool {
	if len(message.Photo) != 0 || message.Document != nil {
		return true
	}
	text := strings.TrimSpace(message.Text)
	if !strings.HasPrefix(text, "/") {
		return true
	}
	command, foreign := parseSlashCommandToken(strings.Fields(text)[0], botUsername)
	if foreign {
		return false
	}
	return command == "/review" || !knownBridgeCommand(command)
}

func sameSessionFile(left, right string) bool {
	a, err := os.Stat(left)
	if err != nil {
		return false
	}
	b, err := os.Stat(right)
	return err == nil && os.SameFile(a, b)
}
func (w *worker) persistClosed() bool {
	if !w.binding.Running {
		return true
	}
	if err := w.b.db.SetRunning(w.binding, false); err != nil {
		w.b.storeLog.Error("session close persistence failed", "event", "binding_write_failed", "reason", "set_running", "error_kind", "persistence")
		w.b.fail(err)
		return false
	}
	w.binding.Running = false
	return true
}
