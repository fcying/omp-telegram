package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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

const (
	albumQuietPeriod = 500 * time.Millisecond
	albumMaxWait     = 2 * time.Second
	maxAlbumItems    = 10
)

type mediaResult struct {
	id, generation int64
	workspace      string
	input          media.Input
	err            error
	logger         *slog.Logger
	album          bool
	count          int
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

func albumPreparationResult(err error) string {
	if err == nil {
		return "done"
	}
	if mediaCancellation(err) {
		return "cancelled"
	}
	return "failed"
}

func mediaErrorKind(err error) string {
	if telegram.ClassifyError(err).Reason == "timeout" {
		return "timeout"
	}
	return "unknown"
}
func (w *worker) initAlbums() {
	if w.albums == nil {
		w.albums = make(map[albumKey]*pendingAlbum)
	}
	if w.albumSuppressed == nil {
		w.albumSuppressed = make(map[albumKey]time.Time)
	}
	if w.albumEvents == nil {
		w.albumEvents = make(chan albumEvent, w.b.cfg.QueueCapacity+1)
	}
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
	w.initAlbums()
	if w.b.mediaSlots == nil {
		w.b.mediaSlots = make(chan struct{}, 2)
	}
}
func (w *worker) queueMedia(in incoming) {
	w.initMedia()
	message := *in.msg
	displayText := strings.TrimSpace(message.Caption)
	if displayText == "" {
		displayText = "Queued attachment"
	}
	user := int64(0)
	if message.From != nil {
		user = message.From.ID
	}
	w.queue = append(w.queue, queued{id: in.id, user: user, replyTo: message.MessageID, displayText: displayText, reply: &message, preparing: true})
	w.startMediaPreparation(in.id, []telegram.Message{message}, false)
}

func (w *worker) collectAlbum(in incoming) {
	w.initAlbums()
	message := *in.msg
	user := int64(0)
	if message.From != nil {
		user = message.From.ID
	}
	key := albumKey{group: message.MediaGroupID, user: user}
	if w.albumIsSuppressed(key) {
		w.mark(in.id, "cancelled")
		return
	}
	if album, ok := w.albums[key]; ok {
		for _, existing := range album.messages {
			if existing.MessageID == message.MessageID {
				w.mark(in.id, "done")
				return
			}
		}
		if len(album.messages) >= maxAlbumItems {
			w.rejectAlbum(album)
			w.mark(in.id, "cancelled")
			return
		}
		if !w.mark(in.id, "done") {
			return
		}
		album.messages = append(album.messages, message)
		caption := albumCaption(album.messages)
		if strings.TrimSpace(caption) != "" {
			album.caption = caption
			for i := range w.queue {
				if w.queue[i].id == album.ownerID && w.queue[i].album {
					w.queue[i].displayText = caption
					break
				}
			}
		}
		w.log.Info("album member collected", "event", "album_collect", "inbox_id", album.ownerID, "count", len(album.messages))
		w.scheduleAlbum(album)
		return
	}
	if _, err := w.ensureRuntime(); err != nil {
		w.suppressAlbum(key)
		w.say(err.Error())
		w.mark(in.id, "done")
		return
	}
	if len(w.queue) >= w.b.cfg.QueueCapacity {
		w.suppressAlbum(key)
		w.say("The queue is full. This album was not submitted.")
		if w.mark(in.id, "cancelled") {
			w.log.Warn("task queue rejected", "event", "queue_rejected", "inbox_id", in.id, "reason", "worker_queue_full")
		}
		return
	}
	displayText := strings.TrimSpace(message.Caption)
	if displayText == "" {
		displayText = "Queued album"
	}
	album := &pendingAlbum{
		key:       key,
		ownerID:   in.id,
		startedAt: time.Now(),
		messages:  []telegram.Message{message},
		caption:   message.Caption,
	}
	w.albums[key] = album
	w.queue = append(w.queue, queued{id: in.id, user: user, replyTo: message.MessageID, displayText: displayText, albumKey: key, album: true, preparing: true})
	w.log.Info("album member collected", "event", "album_collect", "inbox_id", in.id, "count", 1)
	w.scheduleAlbum(album)
}

func (w *worker) suppressAlbum(key albumKey) {
	w.initAlbums()
	now := time.Now()
	w.expireAlbums(now)
	w.albumSuppressed[key] = now.Add(albumMaxWait + time.Second)
}

func (w *worker) expireAlbums(now time.Time) {
	for key, until := range w.albumSuppressed {
		if !now.Before(until) {
			delete(w.albumSuppressed, key)
		}
	}
}

func (w *worker) albumIsSuppressed(key albumKey) bool {
	until, ok := w.albumSuppressed[key]
	if !ok {
		return false
	}
	if !time.Now().Before(until) {
		delete(w.albumSuppressed, key)
		return false
	}
	return true
}

func (w *worker) rejectAlbum(album *pendingAlbum) {
	if album.timer != nil {
		album.timer.Stop()
		album.timer = nil
	}
	delete(w.albums, album.key)
	w.suppressAlbum(album.key)
	for i, q := range w.queue {
		if q.id != album.ownerID || !q.album {
			continue
		}
		if q.cancel != nil {
			q.cancel()
		}
		removeIncoming(w.binding.Workspace, q.directory, w.mediaTaskLogger(q.id))
		w.queue = append(w.queue[:i], w.queue[i+1:]...)
		break
	}
	w.mark(album.ownerID, "failed")
	w.say("Album contains too many items.")
	w.log.Warn("album rejected", "event", "album_rejected", "inbox_id", album.ownerID, "count", len(album.messages)+1, "reason", "too_many_items")
}

func (w *worker) scheduleAlbum(album *pendingAlbum) {
	if album.timer != nil {
		album.timer.Stop()
	}
	w.albumVersion++
	album.version = w.albumVersion
	version := album.version
	key := album.key
	ready := w.albumEvents
	ctx := w.ctx
	deadline := time.Now().Add(albumQuietPeriod)
	if maxDeadline := album.startedAt.Add(albumMaxWait); maxDeadline.Before(deadline) {
		deadline = maxDeadline
	}
	delay := time.Until(deadline)
	if delay < 0 {
		delay = 0
	}
	album.timer = time.AfterFunc(delay, func() {
		select {
		case ready <- albumEvent{key: key, version: version}:
		case <-ctx.Done():
		}
	})
}

func albumCaption(messages []telegram.Message) string {
	selected := -1
	for i := range messages {
		if strings.TrimSpace(messages[i].Caption) == "" {
			continue
		}
		if selected < 0 || messages[i].MessageID < messages[selected].MessageID {
			selected = i
		}
	}
	if selected < 0 {
		return ""
	}
	return messages[selected].Caption
}

func albumReply(messages []telegram.Message) *telegram.Message {
	for i := range messages {
		if formatReplyContext(extractReplyContext(&messages[i])) != "" {
			return &messages[i]
		}
	}
	return nil
}

func (w *worker) sealAlbum(result albumEvent) {
	album, ok := w.albums[result.key]
	if !ok || album.version != result.version {
		return
	}
	if album.timer != nil {
		album.timer.Stop()
		album.timer = nil
	}
	delete(w.albums, result.key)
	w.suppressAlbum(result.key)
	messages := append([]telegram.Message(nil), album.messages...)
	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].MessageID < messages[j].MessageID
	})
	if len(messages) == 0 {
		w.suppressAlbum(result.key)
		w.mark(album.ownerID, "cancelled")
		return
	}
	caption := albumCaption(messages)
	reply := albumReply(messages)
	index := -1
	for i, queued := range w.queue {
		if queued.id == album.ownerID && queued.album && queued.preparing {
			index = i
			break
		}
	}
	if index < 0 {
		w.suppressAlbum(result.key)
		w.mark(album.ownerID, "cancelled")
		return
	}
	album.messages = messages
	album.caption = caption
	album.reply = reply
	q := &w.queue[index]
	q.replyTo = messages[0].MessageID
	q.reply = reply
	q.displayText = strings.TrimSpace(caption)
	if q.displayText == "" {
		q.displayText = "Queued album"
	}
	w.startMediaPreparation(album.ownerID, messages, true)
}

func (w *worker) cancelAlbum(ownerID int64) {
	for key, album := range w.albums {
		if album.ownerID != ownerID {
			continue
		}
		if album.timer != nil {
			album.timer.Stop()
		}
		delete(w.albums, key)
		return
	}
}

func (w *worker) clearAlbums() {
	for key, album := range w.albums {
		w.suppressAlbum(key)
		if album.timer != nil {
			album.timer.Stop()
		}
		delete(w.albums, key)
	}
}

func (w *worker) startMediaPreparation(id int64, messages []telegram.Message, album bool) {
	w.initMedia()
	ctx, cancel := context.WithCancel(w.ctx)
	for i := range w.queue {
		if w.queue[i].id == id {
			w.queue[i].cancel = cancel
			break
		}
	}
	members := append([]telegram.Message(nil), messages...)
	workspace, generation := w.binding.Workspace, w.binding.Generation
	mediaLogger := w.mediaTaskLogger(id)
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer cancel()
		var prepared media.Input
		var err error
		select {
		case w.b.mediaSlots <- struct{}{}:
			if album {
				prepared, err = media.PrepareAlbum(ctx, w.b.tg, workspace, members)
			} else {
				prepared, err = media.Prepare(ctx, w.b.tg, workspace, members[0])
			}
			<-w.b.mediaSlots
		case <-ctx.Done():
			err = ctx.Err()
		}
		result := mediaResult{id: id, generation: generation, workspace: workspace, input: prepared, err: err, logger: mediaLogger, album: album, count: len(members)}
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
		if result.album {
			result.logger.Info("album preparation finished", "event", "album_prepare", "inbox_id", result.id, "count", result.count, "result", albumPreparationResult(result.err))
		}
		removeIncoming(result.workspace, result.input.Directory, result.logger)
		return
	}
	if result.err != nil {
		q := w.queue[index]
		if q.album {
			w.suppressAlbum(q.albumKey)
		}
		if result.album {
			result.logger.Info("album preparation finished", "event", "album_prepare", "inbox_id", result.id, "count", result.count, "result", albumPreparationResult(result.err))
		}
		removeIncoming(result.workspace, result.input.Directory, result.logger)
		w.queue = append(w.queue[:index], w.queue[index+1:]...)
		w.mark(result.id, "failed")
		if !mediaCancellation(result.err) {
			result.logger.Warn("attachment preparation failed", "event", "prepare_failed", "error_kind", mediaErrorKind(result.err))
			message := "Attachment could not be prepared: " + result.err.Error()
			if q.album {
				message = "Album could not be prepared: " + result.err.Error()
			}
			w.say(message)
		}
		return
	}
	q := &w.queue[index]
	q.preparing = false
	q.text = preparePromptText(q.reply, result.input.Text)
	q.reply = nil
	q.images = result.input.Images
	q.directory = result.input.Directory
	q.cancel = nil
	if result.album {
		result.logger.Info("album preparation finished", "event", "album_prepare", "inbox_id", result.id, "count", result.count, "result", "done")
	}
}

func (w *worker) hostResult(client *omp.Client, id, text string, failed bool) error {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	return client.Send(ctx, map[string]any{"type": "host_tool_result", "id": id, "isError": failed, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}})
}

func (w *worker) sendHostResult(client *omp.Client, id, text string, failed bool) {
	if err := w.hostResult(client, id, text, failed); err != nil {
		w.hostResultFailed(client, err)
	}
}

func (w *worker) hostResultFailed(client *omp.Client, err error) {
	if client != w.client {
		return
	}
	attrs := []slog.Attr{
		slog.String("event", "host_tool_result_failed"),
		slog.String("error_kind", omp.ClassifyError(err)),
		slog.Uint64("client_id", client.ID()),
		slog.Int64("generation", w.binding.Generation),
	}
	if w.active != 0 {
		attrs = append(attrs, slog.Int64("inbox_id", w.active))
	}
	w.log.LogAttrs(context.Background(), slog.LevelWarn, "host tool result delivery failed", attrs...)
	for id, cancel := range w.hostRequests {
		cancel()
		delete(w.hostRequests, id)
	}
	if w.active != 0 {
		w.finishUncertain("A host tool result could not be delivered to omp. The instance was closed; the task outcome is uncertain and will not be replayed automatically.")
	} else {
		w.say("A host tool result could not be delivered to omp. The instance was closed.")
	}
	w.busy = false
	w.compacting = false
	w.releaseRuntimeWithReason(true, "failure")
}
func (w *worker) hostSend(event rpcEvent) {
	client, connected := w.runtimeClient()
	if !connected {
		return
	}
	if event.ToolName != "telegram_send" {
		w.sendHostResult(client, event.ID, "Unsupported host tool.", true)
		return
	}
	if !w.busy || w.active == 0 {
		w.sendHostResult(client, event.ID, "Attachments can only be sent during an active Telegram request.", true)
		return
	}
	w.initMedia()
	if _, exists := w.hostRequests[event.ID]; exists {
		return
	}
	if len(w.hostRequests) >= 16 {
		w.sendHostResult(client, event.ID, "Too many pending attachment requests.", true)
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
		w.sendHostResult(client, event.ID, "Invalid attachment request.", true)
		return
	}
	if request.Kind == "" {
		request.Kind = "document"
	}
	if request.Kind != "document" && request.Kind != "photo" {
		w.sendHostResult(client, event.ID, "Attachment kind must be photo or document.", true)
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
		w.sendHostResult(result.client, result.id, result.err.Error(), true)
		return
	}
	file := result.file
	if err := w.b.db.EnqueueAttachment(w.key.chat, w.key.thread, file.Kind, file.Path, file.Name, file.Caption); err != nil {
		result.logger.Error("attachment persistence failed", "event", "attachment_persist_failed", "error_kind", "persistence")
		removeMediaSnapshot(file.Path, result.logger)
		w.sendHostResult(result.client, result.id, "Cannot persist attachment delivery.", true)
		w.b.fail(err)
		return
	}
	w.sendHostResult(result.client, result.id, "Attachment queued for this Telegram conversation. Delivery is not yet confirmed.", false)
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
