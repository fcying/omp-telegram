package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"omp-telegram/internal/media"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/telegram"
)

var telegramSendTools = []map[string]any{{
	"name": "telegram_send", "label": "Send Telegram attachment", "loadMode": "essential",
	"description": "Queue an image or file from the current workspace for delivery to the current Telegram conversation when the user asks for it. Use kind photo for inline JPEG/PNG images, or document for original files. Only regular files inside this workspace are allowed. Never send secrets. Success means queued, not confirmed delivery.",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{
		"path":    map[string]any{"type": "string", "description": "Path to an existing file inside the current workspace."},
		"kind":    map[string]any{"type": "string", "enum": []string{"photo", "document"}, "description": "Defaults to document; photo requires JPEG/PNG within Telegram photo limits."},
		"caption": map[string]any{"type": "string", "description": "Optional caption, at most 1024 UTF-16 code units."},
	}, "required": []string{"path"}, "additionalProperties": false},
}}

type mediaResult struct {
	id, generation int64
	workspace      string
	input          media.Input
	err            error
	logger         *slog.Logger
}
type sendResult struct {
	id         string
	generation int64
	file       media.File
	err        error
	client     *omp.Client
	logger     *slog.Logger
}

func (w *worker) mediaTaskLogger(taskID int64) *slog.Logger {
	logger := w.b.mediaLog.With("chat_id", w.key.chat, "thread_id", w.key.thread, "generation", w.binding.Generation)
	if taskID != 0 {
		logger = logger.With("inbox_id", taskID)
		if taskID == w.active {
			logger = logger.With("turn", w.turn)
		}
	}
	if w.client != nil {
		logger = logger.With("client_id", w.client.ID())
	}
	return logger
}

func mediaCancellation(err error) bool {
	return errors.Is(err, context.Canceled)
}

func mediaErrorKind(err error) string {
	if telegram.ClassifyError(err).Reason == "timeout" {
		return "timeout"
	}
	return "unknown"
}

func (w *worker) initMedia() {
	if w.mediaResults == nil {
		w.mediaResults = make(chan mediaResult, w.b.cfg.QueueCapacity+1)
	}
	if w.sendResults == nil {
		w.sendResults = make(chan sendResult, 16)
	}
	if w.hostRequests == nil {
		w.hostRequests = make(map[string]context.CancelFunc)
	}
	if w.b.mediaSlots == nil {
		w.b.mediaSlots = make(chan struct{}, 2)
	}
}

func (w *worker) queueMedia(in incoming) {
	w.initMedia()
	ctx, cancel := context.WithCancel(w.ctx)
	w.queue = append(w.queue, queued{id: in.id, user: in.msg.From.ID, replyTo: in.msg.MessageID, preparing: true, cancel: cancel})
	workspace, generation := w.binding.Workspace, w.binding.Generation
	message := *in.msg
	mediaLogger := w.mediaTaskLogger(in.id)
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer cancel()
		var prepared media.Input
		var err error
		select {
		case w.b.mediaSlots <- struct{}{}:
			prepared, err = media.Prepare(ctx, w.b.tg, workspace, message)
			<-w.b.mediaSlots
		case <-ctx.Done():
			err = ctx.Err()
		}
		result := mediaResult{id: in.id, generation: generation, workspace: workspace, input: prepared, err: err, logger: mediaLogger}
		select {
		case w.mediaResults <- result:
		case <-w.ctx.Done():
			removeIncoming(workspace, prepared.Directory, mediaLogger)
		}
	}()
}

func removeIncoming(workspace, directory string, logger *slog.Logger) {
	if directory == "" {
		return
	}
	rel, err := filepath.Rel(workspace, directory)
	if err != nil || !filepath.IsLocal(rel) || !strings.HasPrefix(rel, filepath.Join(".telegram", "incoming")+string(filepath.Separator)) {
		return
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("attachment cleanup failed", "event", "cleanup_failed", "error_kind", "filesystem")
		}
		return
	}
	defer root.Close()
	if err := root.RemoveAll(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warn("attachment cleanup failed", "event", "cleanup_failed", "error_kind", "filesystem")
	}
}

func removeMediaSnapshot(path string, logger *slog.Logger) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warn("attachment cleanup failed", "event", "cleanup_failed", "error_kind", "filesystem")
	}
}

func (w *worker) preparedMedia(result mediaResult) {
	index := -1
	if w.client != nil && result.generation == w.binding.Generation {
		for i, q := range w.queue {
			if q.id == result.id && q.preparing {
				index = i
				break
			}
		}
	}
	if index < 0 {
		removeIncoming(result.workspace, result.input.Directory, result.logger)
		return
	}
	if result.err != nil {
		removeIncoming(result.workspace, result.input.Directory, result.logger)
		w.queue = append(w.queue[:index], w.queue[index+1:]...)
		w.mark(result.id, "failed")
		if !mediaCancellation(result.err) {
			result.logger.Warn("attachment preparation failed", "event", "prepare_failed", "error_kind", mediaErrorKind(result.err))
		}
		if !errors.Is(result.err, context.Canceled) {
			w.say("Attachment could not be prepared: " + result.err.Error())
		}
		return
	}
	q := &w.queue[index]
	q.preparing = false
	q.text = result.input.Text
	q.images = result.input.Images
	q.directory = result.input.Directory
	q.cancel = nil
}

func (w *worker) hostResult(client *omp.Client, id, text string, failed bool) {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	_ = client.Send(ctx, map[string]any{"type": "host_tool_result", "id": id, "isError": failed, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}})
}

func (w *worker) hostSend(event rpcEvent) {
	client, connected := w.runtimeClient()
	if !connected {
		return
	}
	if event.ToolName != "telegram_send" {
		w.hostResult(client, event.ID, "Unsupported host tool.", true)
		return
	}
	if !w.busy || w.active == 0 {
		w.hostResult(client, event.ID, "Attachments can only be sent during an active Telegram request.", true)
		return
	}
	w.initMedia()
	if _, exists := w.hostRequests[event.ID]; exists {
		return
	}
	if len(w.hostRequests) >= 16 {
		w.hostResult(client, event.ID, "Too many pending attachment requests.", true)
		return
	}
	var request struct {
		Path    string
		Kind    string
		Caption string
	}
	decoder := json.NewDecoder(strings.NewReader(string(event.Arguments)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || request.Path == "" {
		w.hostResult(client, event.ID, "Invalid attachment request.", true)
		return
	}
	if request.Kind == "" {
		request.Kind = "document"
	}
	if request.Kind != "document" && request.Kind != "photo" {
		w.hostResult(client, event.ID, "Attachment kind must be photo or document.", true)
		return
	}
	ctx, cancel := context.WithCancel(w.ctx)
	w.hostRequests[event.ID] = cancel
	workspace, generation := w.binding.Workspace, w.binding.Generation
	spool := filepath.Join(w.b.cfg.DataDir, "attachments", "outbox")
	mediaLogger := w.mediaTaskLogger(w.active)
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer cancel()
		var file media.File
		var err error
		select {
		case w.b.mediaSlots <- struct{}{}:
			file, err = media.Snapshot(ctx, workspace, spool, request.Path, request.Kind, request.Caption)
			<-w.b.mediaSlots
		case <-ctx.Done():
			err = ctx.Err()
		}
		result := sendResult{id: event.ID, generation: generation, client: client, file: file, err: err, logger: mediaLogger}
		select {
		case w.sendResults <- result:
		case <-w.ctx.Done():
			removeMediaSnapshot(file.Path, mediaLogger)
		}
	}()
}

func (w *worker) preparedSend(result sendResult) {
	cancel, pending := w.hostRequests[result.id]
	if !pending || w.client != result.client || result.generation != w.binding.Generation {
		removeMediaSnapshot(result.file.Path, result.logger)
		return
	}
	cancel()
	delete(w.hostRequests, result.id)
	if result.err != nil {
		if !mediaCancellation(result.err) {
			result.logger.Warn("attachment snapshot failed", "event", "snapshot_failed", "error_kind", mediaErrorKind(result.err))
		}
		w.hostResult(result.client, result.id, result.err.Error(), true)
		return
	}
	file := result.file
	if err := w.b.db.EnqueueAttachment(w.key.chat, w.key.thread, file.Kind, file.Path, file.Name, file.Caption); err != nil {
		result.logger.Error("attachment persistence failed", "event", "attachment_persist_failed", "error_kind", "persistence")
		removeMediaSnapshot(file.Path, result.logger)
		w.hostResult(result.client, result.id, "Cannot persist attachment delivery.", true)
		w.b.fail(err)
		return
	}
	w.hostResult(result.client, result.id, "Attachment queued for this Telegram conversation. Delivery is not yet confirmed.", false)
}

func (w *worker) drainMediaResults() {
	for {
		select {
		case result := <-w.mediaResults:
			removeIncoming(result.workspace, result.input.Directory, result.logger)
		default:
			goto outgoing
		}
	}
outgoing:
	for {
		select {
		case result := <-w.sendResults:
			removeMediaSnapshot(result.file.Path, result.logger)
		default:
			return
		}
	}
}
