package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/media"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

type mediaUpload struct {
	Kind, Thread, Caption, Name string
	Data                        []byte
}

func mediaResponse(r *http.Request, result any) *http.Response {
	raw, _ := json.Marshal(map[string]any{"ok": true, "result": result})
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw)), Request: r}
}
func (f *fakeHTTP) mediaRequest(r *http.Request) (*http.Response, bool, error) {
	if strings.Contains(r.URL.Path, "/file/bot") {
		id := filepath.Base(r.URL.Path)
		f.mu.Lock()
		data := append([]byte(nil), f.files[id]...)
		gate := f.downloadGate
		f.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return nil, true, r.Context().Err()
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: r}, true, nil
	}
	switch filepath.Base(r.URL.Path) {
	case "getFile":
		var req struct {
			ID string `json:"file_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, true, err
		}
		f.mu.Lock()
		size := len(f.files[req.ID])
		f.fileRequests++
		f.mu.Unlock()
		return mediaResponse(r, map[string]any{"file_path": "files/" + req.ID, "file_size": size}), true, nil
	case "sendDocument", "sendPhoto":
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			return nil, true, err
		}
		defer r.MultipartForm.RemoveAll()
		kind := "document"
		if filepath.Base(r.URL.Path) == "sendPhoto" {
			kind = "photo"
		}
		file, header, err := r.FormFile(kind)
		if err != nil {
			return nil, true, err
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			return nil, true, err
		}
		f.mu.Lock()
		f.uploads = append(f.uploads, mediaUpload{Kind: kind, Thread: r.FormValue("message_thread_id"), Caption: r.FormValue("caption"), Name: header.Filename, Data: data})
		f.mu.Unlock()
		return mediaResponse(r, map[string]any{"message_id": 1000}), true, nil
	}
	return nil, false, nil
}
func testPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestNativePhotoCaptionReachesImagePrompt(t *testing.T) {
	f, db, send := setupBridge(t)
	data := testPNG(t)
	f.mu.Lock()
	f.files = map[string][]byte{"photo": data}
	f.mu.Unlock()
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)
	u := update(2, 11, "")
	u.Message.Caption = "/stop"
	u.Message.Photo = []telegram.PhotoSize{{FileID: "photo", Width: 8, Height: 8, FileSize: int64(len(data))}}
	send(u)
	waitFor(t, func() bool { return f.has(11, "image=8x8; Telegram attachment:\n/stop") })
	binding, _ := db.Binding(99, -10, 11)
	files, err := filepath.Glob(filepath.Join(binding.Workspace, ".telegram", "incoming", "*", "photo.jpg"))
	if err != nil || len(files) != 1 {
		t.Fatalf("original image missing: %v", err)
	}
	original, err := os.ReadFile(files[0])
	if err != nil || !bytes.Equal(original, data) {
		t.Fatal("original image was altered")
	}
}

func TestTwoPhotoAlbumAggregatesIntoOneTask(t *testing.T) {
	f, db, send := setupBridge(t)
	data := testPNG(t)
	f.mu.Lock()
	f.files = map[string][]byte{"photo-a": data, "photo-b": data}
	f.mu.Unlock()
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)
	first := update(2, 11, "")
	first.Message.MediaGroupID = "two-photo-album"
	first.Message.Photo = []telegram.PhotoSize{{FileID: "photo-a", Width: 8, Height: 8, FileSize: int64(len(data))}}
	second := update(3, 11, "")
	second.Message.MediaGroupID = "two-photo-album"
	second.Message.Photo = []telegram.PhotoSize{{FileID: "photo-b", Width: 8, Height: 8, FileSize: int64(len(data))}}
	send(first)
	send(second)
	waitFor(t, func() bool { return f.has(11, "answer: images=2; image=8x8; Telegram album:") })
	waitInputDone(t, db, 2)
	waitInputDone(t, db, 3)
}

func TestPhotoAlbumAggregatesIntoOneTask(t *testing.T) {
	f, db, send := setupBridge(t)
	data := testPNG(t)
	f.mu.Lock()
	f.files = map[string][]byte{"photo-a": data, "photo-b": data, "photo-c": data}
	f.mu.Unlock()
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)
	first := update(2, 11, "")
	first.Message.MediaGroupID = "album-1"
	first.Message.Caption = "compare these three"
	first.Message.Photo = []telegram.PhotoSize{{FileID: "photo-a", Width: 8, Height: 8, FileSize: int64(len(data))}}
	ordinary := update(3, 11, "ordinary after album")
	second := update(4, 11, "")
	second.Message.MediaGroupID = "album-1"
	second.Message.ReplyToMessage = &telegram.Message{Text: "quoted album"}
	second.Message.Quote = &telegram.TextQuote{Text: "selected album quote"}
	second.Message.Photo = []telegram.PhotoSize{{FileID: "photo-b", Width: 8, Height: 8, FileSize: int64(len(data))}}
	third := update(5, 11, "")
	third.Message.MediaGroupID = "album-1"
	third.Message.Photo = []telegram.PhotoSize{{FileID: "photo-c", Width: 8, Height: 8, FileSize: int64(len(data))}}
	send(first)
	send(ordinary)
	send(second)
	send(third)

	waitFor(t, func() bool {
		return f.has(11, "answer: images=3; image=8x8; [Telegram reply context - quoted content]\nFrom: user\nQuote:\nselected album quote\n[/Telegram reply context]\n\n[Current user message]\nTelegram album:\ncompare these three\n\nAttachments:\n1.") && f.has(11, "2. ") && f.has(11, "3. ")
	})
	for _, id := range []int64{2, 3, 4, 5} {
		waitInputDone(t, db, id)
	}
	waitFor(t, func() bool { return f.has(11, "answer: ordinary after album") })

	f.mu.Lock()
	var answers []string
	for _, message := range f.messages {
		if text, ok := message["text"].(string); ok && strings.HasPrefix(text, "answer: ") {
			answers = append(answers, text)
		}
	}
	f.mu.Unlock()
	if len(answers) != 2 || !strings.Contains(answers[0], "Telegram album:\ncompare these three") || strings.Count(answers[0], replyContextStart) != 1 || answers[1] != "answer: ordinary after album" {
		t.Fatalf("album task ordering or count = %#v", answers)
	}
	var replyTo int64
	if err := db.DB.QueryRow("SELECT reply_to FROM outbox WHERE inbox_id=? LIMIT 1", 2).Scan(&replyTo); err != nil || replyTo != 2 {
		t.Fatalf("album final reply target = %d, error %v", replyTo, err)
	}
}

func TestAlbumReplySkipsEmptyContext(t *testing.T) {
	empty := telegram.Message{Quote: &telegram.TextQuote{Text: " "}}
	valid := telegram.Message{ReplyToMessage: &telegram.Message{Text: "quoted text"}}
	got := albumReply([]telegram.Message{empty, valid})
	if got == nil || got.ReplyToMessage == nil || got.ReplyToMessage.Text != "quoted text" {
		t.Fatalf("album reply selected %+v, want valid reply", got)
	}
	if got := albumReply([]telegram.Message{empty}); got != nil {
		t.Fatalf("empty album reply context selected %+v", got)
	}
}

func TestAlbumUsesOneQueueSlotAndCompletesMembers(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	queueReady(t, w, command)
	data := testPNG(t)
	accept := func(id int64, message *telegram.Message) {
		t.Helper()
		raw, err := json.Marshal(telegram.Update{UpdateID: id, Message: message})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.b.db.Accept(id, raw); err != nil {
			t.Fatal(err)
		}
		w.handle(incoming{id: id, msg: message})
	}
	first := update(2, 11, "")
	first.Message.MediaGroupID = "album-queue"
	first.Message.Photo = []telegram.PhotoSize{{FileID: "photo-a", Width: 8, Height: 8, FileSize: int64(len(data))}}
	accept(2, first.Message)
	if len(w.queue) != 1 || len(w.albums) != 1 || queueTaskText(w.queue[0]) != "Queued album" {
		t.Fatalf("album placeholder = queue:%+v albums:%d", w.queue, len(w.albums))
	}
	second := update(3, 11, "")
	second.Message.MediaGroupID = "album-queue"
	second.Message.Caption = "later caption"
	second.Message.Photo = []telegram.PhotoSize{{FileID: "photo-b", Width: 8, Height: 8, FileSize: int64(len(data))}}
	accept(3, second.Message)
	album, ok := w.albums[albumKey{group: "album-queue", user: 7}]
	if len(w.queue) != 1 || !ok || len(album.messages) != 2 || queueTaskText(w.queue[0]) != "later caption" {
		t.Fatalf("album member consumed an extra queue slot or preview lost caption: queue=%+v albums=%+v", w.queue, w.albums)
	}
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=3").Scan(&state); err != nil || state != "done" {
		t.Fatalf("secondary album state = %q, error %v", state, err)
	}
	ordinary := update(4, 11, "blocked by album")
	accept(4, ordinary.Message)
	if len(w.queue) != 1 {
		t.Fatalf("ordinary task bypassed full album slot: %+v", w.queue)
	}
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=4").Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("ordinary task state = %q, error %v", state, err)
	}
}

func TestCancelledAlbumSuppressesLateMembers(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	queueReady(t, w, command)
	accept := func(id int64, message *telegram.Message) {
		t.Helper()
		raw, err := json.Marshal(telegram.Update{UpdateID: id, Message: message})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.b.db.Accept(id, raw); err != nil {
			t.Fatal(err)
		}
		w.handle(incoming{id: id, msg: message})
	}
	first := update(2, 11, "")
	first.Message.MediaGroupID = "album-cancel"
	first.Message.Caption = "cancel this album"
	first.Message.Photo = []telegram.PhotoSize{{FileID: "photo-a", FileSize: 1}}
	accept(2, first.Message)
	if len(w.queue) != 1 || queueTaskText(w.queue[0]) != "cancel this album" {
		t.Fatalf("album caption was not used for queue preview: %+v", w.queue)
	}
	if !w.cancelQueuedTask(2) {
		t.Fatal("cancelQueuedTask did not cancel album owner")
	}
	late := update(3, 11, "")
	late.Message.MediaGroupID = "album-cancel"
	late.Message.Photo = []telegram.PhotoSize{{FileID: "photo-b", FileSize: 1}}
	accept(3, late.Message)
	if len(w.queue) != 0 || len(w.albums) != 0 {
		t.Fatalf("late album member recreated task: queue=%+v albums=%+v", w.queue, w.albums)
	}
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", 3).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("late album member state = %q, error %v", state, err)
	}
}

func TestTeardownPreservesAlbumSuppression(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	queueReady(t, w, command)
	accept := func(id int64, message *telegram.Message) {
		t.Helper()
		raw, err := json.Marshal(telegram.Update{UpdateID: id, Message: message})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.b.db.Accept(id, raw); err != nil {
			t.Fatal(err)
		}
		w.handle(incoming{id: id, msg: message})
	}
	first := update(2, 11, "")
	first.Message.MediaGroupID = "album-teardown"
	first.Message.Photo = []telegram.PhotoSize{{FileID: "photo-a", FileSize: 1}}
	accept(2, first.Message)
	key := albumKey{group: "album-teardown", user: 7}
	if _, ok := w.albums[key]; !ok {
		t.Fatal("album was not collected")
	}
	w.teardownWorker(false)
	if !w.albumIsSuppressed(key) {
		t.Fatal("teardown dropped album suppression")
	}
	late := update(3, 11, "")
	late.Message.MediaGroupID = "album-teardown"
	late.Message.Photo = []telegram.PhotoSize{{FileID: "photo-b", FileSize: 1}}
	accept(3, late.Message)
	if len(w.queue) != 0 || len(w.albums) != 0 {
		t.Fatalf("late teardown album member recreated task: queue=%+v albums=%+v", w.queue, w.albums)
	}
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", 3).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("late teardown album member state = %q, error %v", state, err)
	}
}

func TestSealedAlbumSuppressesLateMembers(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	queueReady(t, w, command)
	accept := func(id int64, message *telegram.Message) {
		t.Helper()
		raw, err := json.Marshal(telegram.Update{UpdateID: id, Message: message})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.b.db.Accept(id, raw); err != nil {
			t.Fatal(err)
		}
		w.handle(incoming{id: id, msg: message})
	}
	first := update(2, 11, "")
	first.Message.MediaGroupID = "album-sealed"
	first.Message.Photo = []telegram.PhotoSize{{FileID: "photo", FileSize: 1}}
	accept(2, first.Message)
	key := albumKey{group: "album-sealed", user: 7}
	album := w.albums[key]
	if album == nil {
		t.Fatal("album was not collected")
	}
	w.sealAlbum(albumEvent{key: key, version: album.version})
	if len(w.albums) != 0 || !w.albumIsSuppressed(key) {
		t.Fatalf("sealed album was not fenced: albums=%+v suppressed=%t", w.albums, w.albumIsSuppressed(key))
	}
	late := update(3, 11, "")
	late.Message.MediaGroupID = "album-sealed"
	late.Message.Photo = []telegram.PhotoSize{{FileID: "photo", FileSize: 1}}
	accept(3, late.Message)
	if len(w.queue) != 1 || len(w.albums) != 0 {
		t.Fatalf("late sealed member recreated task: queue=%+v albums=%+v", w.queue, w.albums)
	}
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=3").Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("late sealed member state = %q, error %v", state, err)
	}
}

func TestDuplicateAlbumMemberIsIgnored(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	queueReady(t, w, command)
	accept := func(id int64, message *telegram.Message) {
		t.Helper()
		raw, err := json.Marshal(telegram.Update{UpdateID: id, Message: message})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.b.db.Accept(id, raw); err != nil {
			t.Fatal(err)
		}
		w.handle(incoming{id: id, msg: message})
	}
	first := update(2, 11, "")
	first.Message.MediaGroupID = "album-duplicate"
	first.Message.Photo = []telegram.PhotoSize{{FileID: "photo", FileSize: 1}}
	accept(2, first.Message)
	duplicate := update(3, 11, "")
	duplicate.Message.MediaGroupID = "album-duplicate"
	duplicate.Message.MessageID = first.Message.MessageID
	duplicate.Message.Photo = []telegram.PhotoSize{{FileID: "photo", FileSize: 1}}
	accept(3, duplicate.Message)
	if len(w.queue) != 1 || len(w.albums[albumKey{group: "album-duplicate", user: 7}].messages) != 1 {
		t.Fatalf("duplicate album member changed collection: queue=%+v albums=%+v", w.queue, w.albums)
	}
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=3").Scan(&state); err != nil || state != "done" {
		t.Fatalf("duplicate album member state = %q, error %v", state, err)
	}
}

func TestAlbumQueueFullRejectsOnce(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	queueReady(t, w, command)
	w.queue = append(w.queue, queued{id: 99, preparing: false})
	accept := func(id int64) {
		t.Helper()
		member := update(id, 11, "")
		member.Message.MediaGroupID = "album-full"
		member.Message.Photo = []telegram.PhotoSize{{FileID: "photo", FileSize: 1}}
		raw, err := json.Marshal(telegram.Update{UpdateID: id, Message: member.Message})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.b.db.Accept(id, raw); err != nil {
			t.Fatal(err)
		}
		w.handle(incoming{id: id, msg: member.Message})
	}
	accept(2)
	accept(3)
	if len(w.queue) != 1 || w.queue[0].id != 99 {
		t.Fatalf("full queue changed after rejected album: %+v", w.queue)
	}
	out, err := w.b.db.NextOutput()
	if err != nil || out.Text != "The queue is full. This album was not submitted." {
		t.Fatalf("album queue-full notice = %+v, error %v", out, err)
	}
	if err := w.b.db.MarkOutput(out.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.MarkOutput(out.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if extra, err := w.b.db.NextOutput(); err == nil {
		t.Fatalf("late album fragment produced a second queue-full notice: %+v", extra)
	}
}

func TestAlbumKeysIsolateGroupsUsersAndWorkers(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	queueReady(t, w, command)
	accept := func(id int64, group string, user int64, thread int64) {
		t.Helper()
		member := update(id, thread, "")
		member.Message.From = &telegram.User{ID: user}
		member.Message.MediaGroupID = group
		member.Message.Photo = []telegram.PhotoSize{{FileID: "photo", FileSize: 1}}
		raw, err := json.Marshal(telegram.Update{UpdateID: id, Message: member.Message})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.b.db.Accept(id, raw); err != nil {
			t.Fatal(err)
		}
		w.handle(incoming{id: id, msg: member.Message})
	}
	accept(2, "group-a", 7, 11)
	accept(3, "group-b", 7, 11)
	accept(4, "group-a", 8, 11)
	if len(w.albums) != 3 || len(w.queue) != 3 {
		t.Fatalf("album group or user keys merged: albums=%+v queue=%+v", w.albums, w.queue)
	}
	other := &worker{
		b:       w.b,
		log:     w.log,
		key:     target{chat: -10, thread: 22},
		binding: w.binding,
		client:  w.client,
		runtime: runtimeConnected,
		ctx:     w.ctx,
		cancel:  w.cancel,
	}
	other.initAlbums()
	otherMember := update(5, 22, "")
	otherMember.Message.MediaGroupID = "group-a"
	otherMember.Message.Photo = []telegram.PhotoSize{{FileID: "photo", FileSize: 1}}
	raw, err := json.Marshal(telegram.Update{UpdateID: 5, Message: otherMember.Message})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(5, raw); err != nil {
		t.Fatal(err)
	}
	other.handle(incoming{id: 5, msg: otherMember.Message})
	if len(other.albums) != 1 || len(other.queue) != 1 {
		t.Fatalf("other worker did not collect its album: albums=%+v queue=%+v", other.albums, other.queue)
	}
	if len(w.albums) != 3 || len(w.queue) != 3 {
		t.Fatalf("album state crossed worker boundary: albums=%+v queue=%+v", w.albums, w.queue)
	}
}

func TestSuppressedAlbumsExpireOnWorkerTick(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	queueReady(t, w, command)
	w.initAlbums()
	key := albumKey{group: "expired", user: 7}
	w.albumSuppressed[key] = time.Now().Add(-time.Second)
	w.expire()
	if _, ok := w.albumSuppressed[key]; ok {
		t.Fatal("expired album suppression remained after worker tick")
	}
}

func TestAlbumRejectsTooManyMembers(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	queueReady(t, w, command)
	accept := func(id int64, message *telegram.Message) {
		t.Helper()
		raw, err := json.Marshal(telegram.Update{UpdateID: id, Message: message})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.b.db.Accept(id, raw); err != nil {
			t.Fatal(err)
		}
		w.handle(incoming{id: id, msg: message})
	}
	for id := int64(2); id <= 11; id++ {
		member := update(id, 11, "")
		member.Message.MediaGroupID = "album-limit"
		member.Message.Photo = []telegram.PhotoSize{{FileID: "photo", FileSize: 1}}
		accept(id, member.Message)
	}
	if len(w.queue) != 1 || len(w.albums) != 1 || len(w.albums[albumKey{group: "album-limit", user: 7}].messages) != maxAlbumItems {
		t.Fatalf("album reached wrong size before rejection: queue=%+v albums=%+v", w.queue, w.albums)
	}
	tooMany := update(12, 11, "")
	tooMany.Message.MediaGroupID = "album-limit"
	tooMany.Message.Photo = []telegram.PhotoSize{{FileID: "photo", FileSize: 1}}
	accept(12, tooMany.Message)
	if len(w.queue) != 0 || len(w.albums) != 0 {
		t.Fatalf("oversized album was not removed: queue=%+v albums=%+v", w.queue, w.albums)
	}
	var ownerState, extraState string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&ownerState); err != nil || ownerState != "failed" {
		t.Fatalf("oversized album owner state = %q, error %v", ownerState, err)
	}
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=12").Scan(&extraState); err != nil || extraState != "cancelled" {
		t.Fatalf("oversized album extra state = %q, error %v", extraState, err)
	}
	out, err := w.b.db.NextOutput()
	if err != nil || out.Text != "Album contains too many items." {
		t.Fatalf("oversized album notice = %+v, error %v", out, err)
	}
}

func TestAlbumPreparationFailureIsAtomic(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	queueReady(t, w, command)
	member := update(2, 11, "")
	member.Message.MediaGroupID = "album-failure"
	member.Message.Photo = []telegram.PhotoSize{{FileID: "photo", FileSize: 1}}
	raw, err := json.Marshal(telegram.Update{UpdateID: 2, Message: member.Message})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(2, raw); err != nil {
		t.Fatal(err)
	}
	w.handle(incoming{id: 2, msg: member.Message})
	key := albumKey{group: "album-failure", user: 7}
	delete(w.albums, key)
	directory := filepath.Join(w.binding.Workspace, ".telegram", "incoming", "failed")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "partial.bin"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	w.preparedMedia(mediaResult{
		id:         2,
		generation: w.binding.Generation,
		workspace:  w.binding.Workspace,
		input:      media.Input{Directory: directory},
		err:        errors.New("download failed"),
		logger:     w.mediaTaskLogger(2),
		album:      true,
		count:      3,
	})
	if len(w.queue) != 0 {
		t.Fatalf("failed album placeholder remained: %+v", w.queue)
	}
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&state); err != nil || state != "failed" {
		t.Fatalf("failed album owner state = %q, error %v", state, err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("failed album directory still exists, error %v", err)
	}
	out, err := w.b.db.NextOutput()
	if err != nil || out.Text != "Album could not be prepared: download failed" {
		t.Fatalf("album failure notice = %+v, error %v", out, err)
	}
}

func TestAlbumMembersUseMessageIDOrder(t *testing.T) {
	f, db, send := setupBridge(t)
	f.mu.Lock()
	f.files = map[string][]byte{"high": []byte("high"), "low": []byte("low"), "middle": []byte("middle")}
	f.mu.Unlock()
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)
	high := update(2, 11, "")
	high.Message.MessageID = 103
	high.Message.MediaGroupID = "ordered-album"
	high.Message.Document = &telegram.Document{FileID: "high", FileName: "high.txt"}
	low := update(3, 11, "")
	low.Message.MessageID = 101
	low.Message.MediaGroupID = "ordered-album"
	low.Message.Document = &telegram.Document{FileID: "low", FileName: "low.txt"}
	middle := update(4, 11, "")
	middle.Message.MessageID = 102
	middle.Message.MediaGroupID = "ordered-album"
	middle.Message.Document = &telegram.Document{FileID: "middle", FileName: "middle.txt"}
	send(high)
	send(low)
	send(middle)
	waitFor(t, func() bool { return f.has(11, "Telegram album:") })
	waitInputDone(t, db, 2)
	waitInputDone(t, db, 3)
	waitInputDone(t, db, 4)
	binding, _ := db.Binding(99, -10, 11)
	ordered := []struct {
		name, content string
	}{{"001-low.txt", "low"}, {"002-middle.txt", "middle"}, {"003-high.txt", "high"}}
	for _, want := range ordered {
		files, err := filepath.Glob(filepath.Join(binding.Workspace, ".telegram", "incoming", "*", want.name))
		if err != nil || len(files) != 1 {
			t.Fatalf("ordered album file %s = %v, error %v", want.name, files, err)
		}
		data, err := os.ReadFile(files[0])
		if err != nil || string(data) != want.content {
			t.Fatalf("ordered album content %s = %q, error %v", want.name, data, err)
		}
	}
}

func TestDocumentAlbumAggregatesWithoutCaption(t *testing.T) {
	f, db, send := setupBridge(t)
	f.mu.Lock()
	f.files = map[string][]byte{"document-a": []byte("first document"), "document-b": []byte("second document")}
	f.mu.Unlock()
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)

	first := update(2, 11, "")
	first.Message.MediaGroupID = "document-album"
	first.Message.Document = &telegram.Document{FileID: "document-a", FileName: "report.pdf"}
	second := update(3, 11, "")
	second.Message.MediaGroupID = "document-album"
	second.Message.Document = &telegram.Document{FileID: "document-b", FileName: "report.pdf"}
	send(first)
	send(second)

	waitFor(t, func() bool {
		return f.has(11, "answer: Telegram album:\nPlease inspect the attached files.\n\nAttachments:\n1.") && f.has(11, "2. ")
	})
	waitInputDone(t, db, 2)
	waitInputDone(t, db, 3)
	binding, _ := db.Binding(99, -10, 11)
	firstFiles, err := filepath.Glob(filepath.Join(binding.Workspace, ".telegram", "incoming", "*", "001-report.pdf"))
	if err != nil || len(firstFiles) != 1 {
		t.Fatalf("first album filename = %v, error %v", firstFiles, err)
	}
	secondFiles, err := filepath.Glob(filepath.Join(binding.Workspace, ".telegram", "incoming", "*", "002-report.pdf"))
	if err != nil || len(secondFiles) != 1 {
		t.Fatalf("second album filename = %v, error %v", secondFiles, err)
	}
	firstData, err := os.ReadFile(firstFiles[0])
	if err != nil || string(firstData) != "first document" {
		t.Fatalf("first album file content = %q, error %v", firstData, err)
	}
	secondData, err := os.ReadFile(secondFiles[0])
	if err != nil || string(secondData) != "second document" {
		t.Fatalf("second album file content = %q, error %v", secondData, err)
	}
}

func TestAlbumTimerHonorsHardMaxWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{cfg: config.Config{QueueCapacity: 1}}), ctx: ctx, cancel: cancel})
	defer cancel()
	w.initAlbums()
	key := albumKey{group: "album-hard-max", user: 7}
	album := &pendingAlbum{key: key, ownerID: 1, startedAt: time.Now().Add(-albumMaxWait + 50*time.Millisecond)}
	w.albums[key] = album
	w.scheduleAlbum(album)
	select {
	case event := <-w.albumEvents:
		if event.key != key || event.version != album.version {
			t.Fatalf("hard max event = %+v, album version = %d", event, album.version)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("album did not seal at hard max wait")
	}
	w.clearAlbums()
}

func TestAlbumTimerVersionFencesStaleReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{cfg: config.Config{QueueCapacity: 1}}), ctx: ctx, cancel: cancel})
	w.initAlbums()
	key := albumKey{group: "album-timer", user: 7}
	album := &pendingAlbum{key: key, ownerID: 1, startedAt: time.Now()}
	w.albums[key] = album
	w.scheduleAlbum(album)
	stale := albumEvent{key: key, version: album.version}
	w.scheduleAlbum(album)
	w.sealAlbum(stale)
	if _, ok := w.albums[key]; !ok {
		t.Fatal("stale album timer sealed the current album")
	}
	w.clearAlbums()

	replacement := &pendingAlbum{key: key, ownerID: 2, startedAt: time.Now()}
	w.albums[key] = replacement
	w.scheduleAlbum(replacement)
	w.sealAlbum(stale)
	if _, ok := w.albums[key]; !ok {
		t.Fatal("stale timer from a previous album sealed its replacement")
	}
	w.clearAlbums()
}

func TestDocumentWithoutCaptionIsDownloadedAndDispatched(t *testing.T) {
	f, db, send := setupBridge(t)
	f.mu.Lock()
	f.files = map[string][]byte{"doc": []byte("document contents")}
	f.mu.Unlock()
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)
	u := update(2, 11, "")
	u.Message.Document = &telegram.Document{FileID: "doc", FileName: "../report.txt"}
	send(u)
	waitFor(t, func() bool { return f.has(11, "Attachment saved at") })
	binding, _ := db.Binding(99, -10, 11)
	files, err := filepath.Glob(filepath.Join(binding.Workspace, ".telegram", "incoming", "*", "report.txt"))
	if err != nil || len(files) != 1 {
		t.Fatal("document not retained inside attachment directory")
	}
	data, err := os.ReadFile(files[0])
	if err != nil || string(data) != "document contents" {
		t.Fatal("downloaded document changed")
	}
}

func TestUnauthorizedMediaIsNeverDownloaded(t *testing.T) {
	f, db, send := setupBridge(t)
	u := update(1, 11, "")
	u.Message.From.ID = 666
	u.Message.Document = &telegram.Document{FileID: "private", FileName: "secret.txt"}
	send(u)
	waitFor(t, func() bool {
		var state string
		_ = db.DB.QueryRow("SELECT state FROM inbox WHERE id=1").Scan(&state)
		return state == "ignored"
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fileRequests != 0 {
		t.Fatal("unauthorized file metadata requested")
	}
}

func TestStopCancelsPendingDownloadWithoutBlockingTopic(t *testing.T) {
	f, db, send := setupBridge(t)
	f.mu.Lock()
	f.files = map[string][]byte{"doc": []byte("data")}
	f.downloadGate = make(chan struct{})
	f.mu.Unlock()
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)
	u := update(2, 11, "")
	u.Message.Document = &telegram.Document{FileID: "doc", FileName: "file.txt"}
	send(u)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.fileRequests == 1 })
	send(update(3, 11, "/stop"))
	waitInputDone(t, db, 3)
	var state string
	if err := db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&state); err != nil || state != "cancelled" {
		t.Fatal("pending media was not cancelled")
	}
	send(update(4, 11, "after cancelled download"))
	waitFor(t, func() bool { return f.has(11, "answer: after cancelled download") })
}

func TestStopCancelsPendingAlbumDownload(t *testing.T) {
	f, db, send := setupBridge(t)
	data := testPNG(t)
	f.mu.Lock()
	f.files = map[string][]byte{"photo": data}
	f.downloadGate = make(chan struct{})
	f.mu.Unlock()
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)
	member := update(2, 11, "")
	member.Message.MediaGroupID = "stop-album"
	member.Message.Photo = []telegram.PhotoSize{{FileID: "photo", Width: 8, Height: 8, FileSize: int64(len(data))}}
	send(member)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.fileRequests == 1 })
	send(update(3, 11, "/stop"))
	waitInputDone(t, db, 3)
	var state string
	if err := db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("pending album state = %q, error %v", state, err)
	}
	late := update(4, 11, "")
	late.Message.MediaGroupID = "stop-album"
	late.Message.Photo = member.Message.Photo
	send(late)
	waitFor(t, func() bool {
		var lateState string
		return db.DB.QueryRow("SELECT state FROM inbox WHERE id=4").Scan(&lateState) == nil && lateState == "cancelled"
	})
	f.mu.Lock()
	requests := f.fileRequests
	f.mu.Unlock()
	if requests != 1 {
		t.Fatalf("suppressed album member triggered download: %d", requests)
	}
}

func TestStopCancelsActiveAndQueuedAlbums(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	queueReady(t, w, command)
	command("active task")
	w.dispatch()
	active := w.active
	if active == 0 || !w.busy {
		t.Fatal("active task did not start")
	}
	accept := func(id int64, message *telegram.Message) {
		t.Helper()
		raw, err := json.Marshal(telegram.Update{UpdateID: id, Message: message})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.b.db.Accept(id, raw); err != nil {
			t.Fatal(err)
		}
		w.handle(incoming{id: id, msg: message})
	}
	member := update(3, 11, "")
	member.Message.MediaGroupID = "stop-queued-album"
	member.Message.Photo = []telegram.PhotoSize{{FileID: "photo", FileSize: 1}}
	accept(3, member.Message)
	accept(4, update(4, 11, "queued after album").Message)
	if len(w.queue) != 2 || len(w.albums) != 1 {
		t.Fatalf("queued stop fixture = queue:%+v albums:%+v", w.queue, w.albums)
	}
	w.stop()
	if active != w.active || !w.busy || len(w.queue) != 0 || len(w.albums) != 0 {
		t.Fatalf("stop changed active task or retained queued album: active=%d/%d busy=%t queue=%+v albums=%+v", w.active, active, w.busy, w.queue, w.albums)
	}
	for _, id := range []int64{3, 4} {
		var state string
		if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", id).Scan(&state); err != nil || state != "cancelled" {
			t.Fatalf("stopped queued input %d state = %q, error %v", id, state, err)
		}
	}
}

func TestHostToolQueuesImageAndDocumentWithoutCrossingTopics(t *testing.T) {
	for _, kind := range []string{"photo", "document"} {
		t.Run(kind, func(t *testing.T) {
			f, db, send := setupBridge(t)
			send(update(1, 11, "/new "+t.TempDir()))
			waitBinding(t, db, 11)
			binding, _ := db.Binding(99, -10, 11)
			name := "report.txt"
			content := []byte("report data")
			if kind == "photo" {
				name = "image.png"
				content = testPNG(t)
			}
			if err := os.WriteFile(filepath.Join(binding.Workspace, name), content, 0600); err != nil {
				t.Fatal(err)
			}
			send(update(2, 11, "send-attachment "+kind+" "+name))
			waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.uploads) == 1 })
			f.mu.Lock()
			upload := f.uploads[0]
			f.mu.Unlock()
			if upload.Kind != kind || upload.Thread != "11" || upload.Name != name || !bytes.Equal(upload.Data, content) {
				t.Fatal("attachment delivery changed file or destination")
			}
			waitFor(t, func() bool { return f.has(11, "attachment accepted") })
			var pending int
			waitFor(t, func() bool {
				_ = db.DB.QueryRow("SELECT count(*) FROM outbox WHERE kind=? AND state='done'", kind).Scan(&pending)
				return pending == 1
			})
		})
	}
}

func TestHostToolRejectsFilesOutsideWorkspace(t *testing.T) {
	f, db, send := setupBridge(t)
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	send(update(2, 11, "send-attachment document "+outside))
	waitFor(t, func() bool { return f.has(11, "attachment rejected") })
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.uploads) != 0 {
		t.Fatal("outside file was uploaded")
	}
}

func TestObsoleteHostAttachmentCannotReachNewGeneration(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	if w.client == nil {
		t.Fatal("worker did not start")
	}
	staged := filepath.Join(t.TempDir(), "snapshot")
	if err := os.WriteFile(staged, []byte("old bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	oldGeneration := w.binding.Generation
	w.binding.Generation++
	cancelled := false
	w.hostRequests["reused-id"] = func() { cancelled = true }
	w.preparedSend(sendResult{id: "reused-id", generation: oldGeneration, client: w.client, file: media.File{Path: staged, Name: "old.txt", Kind: "document"}})
	var count int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE kind != 'text'").Scan(&count); err != nil || count != 0 {
		t.Fatal("obsolete attachment was queued")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatal("obsolete snapshot was retained")
	}
	if cancelled {
		t.Fatal("old result cancelled a new request with the same ID")
	}
}

func TestDatabaseCleanupRemovesOnlyOwnedSnapshots(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	spool := filepath.Join(dir, "attachments", "outbox")
	if err = os.MkdirAll(spool, 0700); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(spool, "attachment-retained")
	missing := filepath.Join(spool, "attachment-missing")
	outside := filepath.Join(t.TempDir(), "outside")
	for _, path := range []string{snapshot, missing, outside} {
		if path != missing {
			if err = os.WriteFile(path, []byte("attachment"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err = db.EnqueueAttachment(1, 2, "document", path, "file", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{1, 2, 3} {
		if err = db.MarkOutput(id, "sending"); err != nil {
			t.Fatal(err)
		}
		if err = db.MarkOutput(id, "done"); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().AddDate(0, 0, -91).Unix()
	if _, err = db.DB.Exec("UPDATE outbox SET created_at=?,updated_at=?", old, old); err != nil {
		t.Fatal(err)
	}
	result, err := db.CleanupMessages(context.Background(), time.Now().AddDate(0, 0, -90).Unix())
	if err != nil || result.Outbox != 3 {
		t.Fatalf("cleanup result = %+v, error %v", result, err)
	}
	for _, path := range result.AttachmentPaths {
		removeOutboxSnapshot(spool, path)
	}
	if _, err = os.Stat(snapshot); !os.IsNotExist(err) {
		t.Fatalf("owned snapshot remained: %v", err)
	}
	if _, err = os.Stat(outside); err != nil {
		t.Fatalf("outside path was removed: %v", err)
	}
}

func TestDatabaseCleanupReconcilesOrphanedSnapshots(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	spool := filepath.Join(dir, "attachments", "outbox")
	if err = os.MkdirAll(spool, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(spool, "attachment-orphan")
	referenced := filepath.Join(spool, "attachment-referenced")
	recent := filepath.Join(spool, "attachment-recent")
	unrelated := filepath.Join(spool, "keep")
	for _, path := range []string{orphan, referenced, recent, unrelated} {
		if err = os.WriteFile(path, []byte("attachment"), 0400); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().AddDate(0, 0, -91)
	for _, path := range []string{orphan, referenced, unrelated} {
		if err = os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.EnqueueAttachment(1, 2, "document", referenced, "file", ""); err != nil {
		t.Fatal(err)
	}
	b := testBridge(t, &Bridge{cfg: config.Config{DataDir: dir}, db: db})
	b.reconcileOutboxSnapshots(context.Background(), time.Now().AddDate(0, 0, -90))
	if _, err = os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan snapshot remained: %v", err)
	}
	for _, path := range []string{referenced, recent, unrelated} {
		if _, err = os.Stat(path); err != nil {
			t.Fatalf("retained snapshot %q: %v", path, err)
		}
	}
}

func TestFullQueueRejectsAttachmentBeforePreparation(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	command("/new " + t.TempDir())
	command("accepted task")
	acceptedID := w.queue[0].id
	u := update(3, 11, "")
	u.Message.Caption = "rejected attachment"
	u.Message.Document = &telegram.Document{FileID: "rejected", FileName: "rejected.txt", MimeType: "text/plain", FileSize: 4}
	f.files = map[string][]byte{"rejected": []byte("data")}
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(u.UpdateID, raw); err != nil {
		t.Fatal(err)
	}
	w.handle(incoming{id: u.UpdateID, msg: u.Message})
	if len(w.queue) != 1 || w.queue[0].id != acceptedID {
		t.Fatal("rejected attachment was admitted beyond queue capacity")
	}
	w.background.Wait()
	f.mu.Lock()
	requests := f.fileRequests
	f.mu.Unlock()
	if requests != 0 {
		t.Fatal("rejected attachment triggered Telegram file access")
	}
	w.dispatch()
	drainWatchdogEvents(t, w)
	w.dispatch()
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", u.UpdateID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("rejected attachment state=%q err=%v", state, err)
	}
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", acceptedID).Scan(&state); err != nil || state != "done" {
		t.Fatalf("accepted task did not complete: state=%q err=%v", state, err)
	}
}
