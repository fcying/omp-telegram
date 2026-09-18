package bridge

import (
	"bytes"
	"context"
	"encoding/json"
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
	b := Bridge{cfg: config.Config{DataDir: dir}, db: db}
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
