package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"omp-telegram/internal/media"
	"omp-telegram/internal/omp"
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
}
type sendResult struct {
	id         string
	generation int64
	file       media.File
	err        error
	client     *omp.Client
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
	w.queue = append(w.queue, queued{id: in.id, user: in.msg.From.ID, preparing: true, cancel: cancel})
	workspace, generation := w.binding.Workspace, w.binding.Generation
	message := *in.msg
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
		result := mediaResult{id: in.id, generation: generation, workspace: workspace, input: prepared, err: err}
		select {
		case w.mediaResults <- result:
		case <-w.ctx.Done():
			removeIncoming(workspace, prepared.Directory)
		}
	}()
}

func removeIncoming(workspace, directory string) {
	if directory == "" {
		return
	}
	rel, err := filepath.Rel(workspace, directory)
	if err != nil || !filepath.IsLocal(rel) || !strings.HasPrefix(rel, filepath.Join(".telegram", "incoming")+string(filepath.Separator)) {
		return
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return
	}
	defer root.Close()
	_ = root.RemoveAll(rel)
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
		removeIncoming(result.workspace, result.input.Directory)
		return
	}
	if result.err != nil {
		removeIncoming(result.workspace, result.input.Directory)
		w.queue = append(w.queue[:index], w.queue[index+1:]...)
		w.mark(result.id, "failed")
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
	if w.client == nil {
		return
	}
	if event.ToolName != "telegram_send" {
		w.hostResult(w.client, event.ID, "Unsupported host tool.", true)
		return
	}
	if !w.busy || w.active == 0 {
		w.hostResult(w.client, event.ID, "Attachments can only be sent during an active Telegram request.", true)
		return
	}
	w.initMedia()
	if _, exists := w.hostRequests[event.ID]; exists {
		return
	}
	if len(w.hostRequests) >= 16 {
		w.hostResult(w.client, event.ID, "Too many pending attachment requests.", true)
		return
	}
	var request struct {
		Path    string `json:"path"`
		Kind    string `json:"kind"`
		Caption string `json:"caption"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(event.Arguments)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || request.Path == "" {
		w.hostResult(w.client, event.ID, "Invalid attachment request.", true)
		return
	}
	if request.Kind == "" {
		request.Kind = "document"
	}
	if request.Kind != "document" && request.Kind != "photo" {
		w.hostResult(w.client, event.ID, "Attachment kind must be photo or document.", true)
		return
	}
	ctx, cancel := context.WithCancel(w.ctx)
	w.hostRequests[event.ID] = cancel
	workspace, generation, client := w.binding.Workspace, w.binding.Generation, w.client
	spool := filepath.Join(w.b.cfg.DataDir, "attachments", "outbox")
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
		result := sendResult{id: event.ID, generation: generation, client: client, file: file, err: err}
		select {
		case w.sendResults <- result:
		case <-w.ctx.Done():
			if file.Path != "" {
				_ = os.Remove(file.Path)
			}
		}
	}()
}

func (w *worker) preparedSend(result sendResult) {
	cancel, pending := w.hostRequests[result.id]
	if !pending || w.client != result.client || result.generation != w.binding.Generation {
		if result.file.Path != "" {
			_ = os.Remove(result.file.Path)
		}
		return
	}
	cancel()
	delete(w.hostRequests, result.id)
	if result.err != nil {
		w.hostResult(result.client, result.id, result.err.Error(), true)
		return
	}
	file := result.file
	if err := w.b.db.EnqueueAttachment(w.key.chat, w.key.thread, file.Kind, file.Path, file.Name, file.Caption); err != nil {
		_ = os.Remove(file.Path)
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
			removeIncoming(result.workspace, result.input.Directory)
		default:
			goto outgoing
		}
	}
outgoing:
	for {
		select {
		case result := <-w.sendResults:
			if result.file.Path != "" {
				_ = os.Remove(result.file.Path)
			}
		default:
			return
		}
	}
}
