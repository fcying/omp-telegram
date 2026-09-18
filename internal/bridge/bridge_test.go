package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"omp-telegram/internal/config"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

// This subprocess speaks RPC to exercise the real pipe and actor boundaries.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "acp" {
		runResumeListFixture()
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "--mode" {
		out := json.NewEncoder(os.Stdout)
		emit := func(v any) {
			if out.Encode(v) != nil {
				os.Exit(2)
			}
		}
		root := os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT")
		session := filepath.Join(root, fmt.Sprintf("%08x-0000-4000-8000-%012x.jsonl", os.Getpid(), os.Getpid()))
		var resume string
		for i, arg := range os.Args {
			if arg == "--resume" && i+1 < len(os.Args) {
				resume = os.Args[i+1]
			}
		}
		if resume != "" {
			if filepath.IsAbs(resume) {
				session = resume
			} else {
				entries, err := os.ReadDir(root)
				if err != nil {
					os.Exit(2)
				}
				session = ""
				for _, entry := range entries {
					if strings.HasPrefix(strings.ToLower(entry.Name()), strings.ToLower(resume)) {
						session = filepath.Join(root, entry.Name())
						break
					}
				}
				if session == "" {
					os.Exit(2)
				}
			}
			data, err := os.ReadFile(session)
			if err != nil {
				os.Exit(2)
			}
			var saved struct {
				CWD string `json:"cwd"`
			}
			if json.Unmarshal(data, &saved) != nil || os.Chdir(saved.CWD) != nil {
				os.Exit(2)
			}
		} else {
			cwd, err := os.Getwd()
			if err != nil {
				os.Exit(2)
			}
			data, _ := json.Marshal(map[string]string{"cwd": cwd})
			if os.WriteFile(session, data, 0600) != nil {
				os.Exit(2)
			}
		}
		sessionID := strings.TrimSuffix(filepath.Base(session), ".jsonl")
		emit(map[string]any{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": []int{1, 2}, "maxFrameBytes": 1048576, "maxReassembledFrameBytes": 67108864})
		scan := bufio.NewScanner(os.Stdin)
		for scan.Scan() {
			var cmd map[string]any
			if json.Unmarshal(scan.Bytes(), &cmd) != nil {
				continue
			}
			typ, _ := cmd["type"].(string)
			resp := map[string]any{"type": "response", "id": cmd["id"], "command": typ, "success": true}
			switch typ {
			case "extension_ui_response":
				continue
			case "host_tool_result":
				text := "attachment accepted"
				if failed, _ := cmd["isError"].(bool); failed {
					text = "attachment rejected"
				}
				emit(map[string]any{"type": "agent_end", "messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}}})
				continue
			case "get_state":
				resp["data"] = map[string]any{"sessionId": sessionID, "sessionFile": session, "model": map[string]any{"provider": "fixture", "id": "safe", "headers": map[string]string{"Authorization": "SECRET"}}, "systemPrompt": "PRIVATE"}
			case "prompt":
				text, _ := cmd["message"].(string)
				if text == "/session info" {
					cwd, err := os.Getwd()
					if err != nil {
						os.Exit(2)
					}
					emit(map[string]any{"type": "command_output", "text": fmt.Sprintf("Session: %s\nTitle: fixture\nCWD: %s", sessionID, cwd)})
					resp["data"] = map[string]any{"agentInvoked": false}
					emit(resp)
					continue
				}
				emit(resp)
				emit(map[string]any{"type": "agent_start"})
				if text == "wait" {
					continue
				}
				if strings.HasPrefix(text, "send-attachment ") {
					parts := strings.SplitN(text, " ", 3)
					if len(parts) != 3 {
						os.Exit(2)
					}
					emit(map[string]any{"type": "host_tool_call", "id": "attachment-call", "toolName": "telegram_send", "arguments": map[string]any{"path": parts[2], "kind": parts[1], "caption": "artifact"}})
					continue
				}
				if images, ok := cmd["images"].([]any); ok && len(images) > 0 {
					item := images[0].(map[string]any)
					data, err := base64.StdEncoding.DecodeString(item["data"].(string))
					if err != nil {
						os.Exit(2)
					}
					cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
					if err != nil {
						os.Exit(2)
					}
					text = fmt.Sprintf("image=%dx%d; %s", cfg.Width, cfg.Height, text)
				}
				if text == "cwd" {
					text, _ = os.Getwd()
				}
				emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": text}})
				emit(map[string]any{"type": "agent_end", "isTerminal": false})
				emit(map[string]any{"type": "agent_end", "messages": []any{map[string]any{"role": "user", "content": text}, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "text": "PRIVATE"}, map[string]any{"type": "text", "text": "answer: " + text}}}}})
				continue
			case "abort":
				emit(resp)
				emit(map[string]any{"type": "agent_end", "messages": []any{}})
				continue
			}
			emit(resp)
		}
		os.Exit(0)
	}
	sessions, err := os.MkdirTemp("", "omp-telegram-fixture-sessions-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", sessions); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(sessions)
	os.Exit(code)
}

type fakeHTTP struct {
	mu             sync.Mutex
	messages       []map[string]any
	callbacks      []string
	updates        chan telegram.Update
	rejectCommands bool
	rejectLanguage string
	files          map[string][]byte
	downloadGate   <-chan struct{}
	fileRequests   int
	uploads        []mediaUpload
}

func (f *fakeHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	if response, handled, err := f.mediaRequest(r); handled {
		return response, err
	}
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	var result any = true
	switch filepath.Base(r.URL.Path) {
	case "getMe":
		result = telegram.User{ID: 99, Username: "fixture_bot", IsBot: true}
	case "setMyCommands":
		if f.rejectCommands && req["language_code"] == f.rejectLanguage {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":400,"description":"command registration rejected"}`)), Header: make(http.Header), Request: r}, nil
		}
	case "getUpdates":
		select {
		case u := <-f.updates:
			result = []telegram.Update{u}
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(20 * time.Millisecond):
			result = []telegram.Update{}
		}
	case "sendMessage", "editMessageText":
		f.mu.Lock()
		f.messages = append(f.messages, req)
		id := len(f.messages)
		f.mu.Unlock()
		result = map[string]any{"message_id": id}
	case "answerCallbackQuery":
		f.mu.Lock()
		f.callbacks = append(f.callbacks, req["text"].(string))
		f.mu.Unlock()
	}
	raw, _ := json.Marshal(map[string]any{"ok": true, "result": result})
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw))), Header: make(http.Header), Request: r}, nil
}
func (f *fakeHTTP) has(thread int64, text string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.messages {
		if m["message_thread_id"] == float64(thread) && strings.Contains(m["text"].(string), text) {
			return true
		}
	}
	return false
}
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("observable condition not reached")
}
func setupBridge(t *testing.T) (*fakeHTTP, *store.Store, func(telegram.Update)) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeHTTP{updates: make(chan telegram.Update, 32)}
	old := http.DefaultTransport
	http.DefaultTransport = fake
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	exe, _ := os.Executable()
	cfg := config.Config{Token: "fake", AllowedUsers: []int64{7, 8}, AllowedChats: []int64{-10}, WorkspaceRoot: t.TempDir(), OMP: exe, DataDir: t.TempDir(), MaxWorkers: 2, QueueCapacity: 4}
	go func() { done <- Run(ctx, cfg, db) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon shutdown timed out")
		}
		http.DefaultTransport = old
		db.Close()
	})
	return fake, db, func(u telegram.Update) { fake.updates <- u }
}

func TestCommandRegistrationFailureStopsStartup(t *testing.T) {
	for _, language := range []string{"", "zh"} {
		t.Run("language="+language, func(t *testing.T) {
			db, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			previous := http.DefaultTransport
			http.DefaultTransport = &fakeHTTP{rejectCommands: true, rejectLanguage: language}
			defer func() { http.DefaultTransport = previous }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = Run(ctx, config.Config{Token: "fake", MaxWorkers: 1, QueueCapacity: 1}, db)
			var apiErr *telegram.APIError
			if !errors.As(err, &apiErr) || apiErr.Code != 400 {
				t.Fatalf("registration failure did not stop startup: %v", err)
			}
		})
	}
}
func update(id, thread int64, text string) telegram.Update {
	return telegram.Update{UpdateID: id, Message: &telegram.Message{MessageID: id, MessageThreadID: thread, From: &telegram.User{ID: 7}, Chat: telegram.Chat{ID: -10}, Text: text}}
}
func TestTopicsQueueStopAndResume(t *testing.T) {
	f, db, send := setupBridge(t)
	source := t.TempDir()
	send(update(1, 11, "/new "+source))
	send(update(2, 22, "/new "+source))
	waitBinding(t, db, 11)
	waitBinding(t, db, 22)
	first, err := db.Binding(99, -10, 11)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.Binding(99, -10, 22)
	if err != nil {
		t.Fatal(err)
	}
	if first.Workspace != source || second.Workspace != source {
		t.Fatal("topics did not select the source working directory")
	}
	if first.Session == second.Session {
		t.Fatal("topics share a native session")
	}
	marker := filepath.Join(first.Workspace, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(second.Workspace, "keep.txt")); err != nil || string(data) != "keep" {
		t.Fatal("topics selecting the same directory do not share files")
	}
	send(update(3, 11, "wait"))
	send(update(4, 11, "must not run"))
	send(update(5, 22, "independent"))
	waitFor(t, func() bool { return f.has(22, "answer: independent") })
	if f.has(11, "independent") {
		t.Fatal("cross-topic output")
	}
	send(update(6, 11, "/stop"))
	waitFor(t, func() bool {
		var state string
		_ = db.DB.QueryRow("SELECT state FROM inbox WHERE id=4").Scan(&state)
		return state == "cancelled"
	})
	send(update(7, 11, "after stop"))
	waitFor(t, func() bool { return f.has(11, "answer: after stop") })
	if f.has(11, "must not run") {
		t.Fatal("cancelled prompt executed")
	}
	send(update(8, 11, "/status"))
	waitFor(t, func() bool { return f.has(11, "fixture/safe") })
	if f.has(11, "SECRET") || f.has(11, "PRIVATE") {
		t.Fatal("sensitive state exposed")
	}
	send(update(9, 11, "/close"))
	waitInputDone(t, db, 9)
	send(update(10, 11, "/resume "+strings.TrimSuffix(filepath.Base(first.Session), ".jsonl")))
	waitFor(t, func() bool { b, e := db.Binding(99, -10, 11); return e == nil && b.Generation > first.Generation })
	restored, _ := db.Binding(99, -10, 11)
	if restored.Session != first.Session || restored.Workspace != first.Workspace {
		t.Fatal("resume selected another session or working directory")
	}
	send(update(11, 11, "cwd"))
	waitFor(t, func() bool { return f.has(11, "answer: "+first.Workspace) })
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatal("close/resume lost workspace files")
	}
}
func TestUnauthorizedUpdateCannotSpawn(t *testing.T) {
	_, db, send := setupBridge(t)
	u := update(1, 11, "/new "+t.TempDir())
	u.Message.From.ID = 666
	send(u)
	waitFor(t, func() bool {
		var state string
		_ = db.DB.QueryRow("SELECT state FROM inbox WHERE id=1").Scan(&state)
		return state == "ignored"
	})
	if _, e := db.Binding(99, -10, 11); e == nil {
		t.Fatal("unauthorized binding")
	}
}
func TestSplitPreservesUnicodeAndLength(t *testing.T) {
	text := strings.Repeat("字😀<&>\n", 1800)
	parts := split(text, 3800)
	if strings.Join(parts, "") != text {
		t.Fatal("content changed")
	}
	for _, p := range parts {
		if len(utf16.Encode([]rune(p))) > 3800 {
			t.Fatal("Telegram length exceeded")
		}
	}
}
func TestFinalPersistsWhilePreviewIsInFlight(t *testing.T) {
	db, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = db.Accept(10, []byte(`{"update_id":10}`)); e != nil {
		t.Fatal(e)
	}
	if e = db.Mark(10, "submitted"); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &worker{b: &Bridge{db: db}, ctx: ctx, cancel: cancel, active: 10, previewBusy: true, preview: "answer", busy: true, confirms: map[string]confirmation{}}
	w.finish()
	o, e := db.NextOutput()
	if e != nil || o.Text != "answer" {
		t.Fatalf("final was not persisted before preview completion: %v", e)
	}
	if !w.finishing {
		t.Fatal("preview finalization was not deferred")
	}
}

func TestTerminalRPCFailureIsUncertainAndSanitized(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Accept(10, []byte(`{"update_id":10}`)); err != nil {
		t.Fatal(err)
	}
	if err = db.Mark(10, "submitted"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &worker{b: &Bridge{db: db}, ctx: ctx, cancel: cancel, active: 10, busy: true, confirms: map[string]confirmation{}}
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"partial answer"}}`))
	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"partial answer"}],"stopReason":"error","errorMessage":"SECRET upstream diagnostics"}]}`))
	var state string
	if err = db.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("terminal failure state = %q, error %v", state, err)
	}
	o, err := db.NextOutput()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(o.Text, "omp reported that the task failed") || !strings.Contains(o.Text, "partial answer") || strings.Contains(o.Text, "SECRET") {
		t.Fatalf("terminal failure output = %q", o.Text)
	}
}

func TestTerminalWithoutTextIsUncertain(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Accept(10, []byte(`{"update_id":10}`)); err != nil {
		t.Fatal(err)
	}
	if err = db.Mark(10, "submitted"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &worker{b: &Bridge{db: db}, ctx: ctx, cancel: cancel, active: 10, busy: true, confirms: map[string]confirmation{}}
	w.event([]byte(`{"type":"agent_end"}`))
	var state string
	if err = db.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("empty terminal state = %q, error %v", state, err)
	}
}

func TestDispatchWaitsForPreviewCleanup(t *testing.T) {
	w := &worker{client: &omp.Client{}, finishing: true, queue: []queued{{id: 1}}}
	w.dispatch()
	if len(w.queue) != 1 {
		t.Fatal("next prompt dispatched before prior preview cleanup")
	}
}

func TestReplacementShutdownRetainsCapacitySlot(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	if len(w.b.slots) != 1 {
		t.Fatal("running worker did not hold capacity slot")
	}
	w.shutdownWithSlot(false)
	if len(w.b.slots) != 1 {
		t.Fatal("replacement shutdown released its capacity slot")
	}
	<-w.b.slots
}

func TestStartFailureRetainsStartupIntent(t *testing.T) {
	w, _, _ := setupWorkspaceWorker(t)
	w.b.cfg.OMP = filepath.Join(t.TempDir(), "missing-omp")
	w.start(false, t.TempDir(), "", false)
	if w.startIntent == nil {
		t.Fatal("failed start discarded startup intent")
	}
	intents, err := w.b.db.PendingStarts(w.b.bot.ID)
	if err != nil || len(intents) != 1 || intents[0] != *w.startIntent {
		t.Fatalf("failed start durable intent = %+v, error %v", intents, err)
	}
}

func TestFailedUICallbackMarksUpdateUncertain(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	w.confirms["ui"] = confirmation{action: "ui", uiID: "ui-request", method: "confirm", generation: w.binding.Generation, user: 7, expires: time.Now().Add(time.Minute)}
	_ = w.client.Close()
	if err := w.b.db.Accept(99, []byte(`{"update_id":99}`)); err != nil {
		t.Fatal(err)
	}
	w.handle(incoming{id: 99, callback: &telegram.CallbackQuery{ID: "callback", From: telegram.User{ID: 7}, Data: "ui:0"}})
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=99").Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("failed UI callback state = %q, error %v", state, err)
	}
	if w.client != nil {
		t.Fatal("failed UI callback left omp running")
	}
}

func TestFinalReplyWriteFailureDoesNotCompleteInput(t *testing.T) {
	w, _, _ := setupWorkspaceWorker(t)
	if err := w.b.db.Accept(10, []byte(`{"update_id":10}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Mark(10, "submitted"); err != nil {
		t.Fatal(err)
	}
	_, err := w.b.db.DB.Exec(`CREATE TRIGGER reject_final_part BEFORE INSERT ON outbox WHEN NEW.text='second' BEGIN SELECT RAISE(FAIL,'injected final failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	w.active, w.busy = 10, true
	w.preview = strings.Repeat("x", 3800) + "second"
	w.finish()
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "submitted" {
		t.Fatalf("failed final reply marked input %q", state)
	}
	var outputs int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&outputs); err != nil {
		t.Fatal(err)
	}
	if outputs != 0 {
		t.Fatal("failed final reply left partial output")
	}
}

func (f *fakeHTTP) button(after int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.messages) - 1; i >= after; i-- {
		m := f.messages[i]
		markup, ok := m["reply_markup"].(map[string]any)
		if !ok {
			continue
		}
		rows := markup["inline_keyboard"].([]any)
		return rows[0].([]any)[0].(map[string]any)["callback_data"].(string)
	}
	return ""
}

func (f *fakeHTTP) messageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}

func waitBinding(t *testing.T, db *store.Store, thread int64) {
	t.Helper()
	waitFor(t, func() bool { b, err := db.Binding(99, -10, thread); return err == nil && b.Session != "" })
}

func waitInputDone(t *testing.T, db *store.Store, id int64) {
	t.Helper()
	waitFor(t, func() bool {
		var state string
		return db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", id).Scan(&state) == nil && state == "done"
	})
}
func TestConfirmationOwnerReplayAndCompactCompletion(t *testing.T) {
	f, db, send := setupBridge(t)
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)
	old, _ := db.Binding(99, -10, 11)
	marker := filepath.Join(old.Workspace, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	after := f.messageCount()
	send(update(2, 11, "/new"))
	waitFor(t, func() bool { return f.button(after) != "" })
	token := f.button(after)
	callback := func(id, user int64, data string) {
		send(telegram.Update{UpdateID: id, CallbackQuery: &telegram.CallbackQuery{ID: fmt.Sprint(id), From: telegram.User{ID: user}, Message: update(0, 11, "").Message, Data: data}})
	}
	callback(3, 8, token)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.callbacks) > 0 })
	current, _ := db.Binding(99, -10, 11)
	if current.Generation != old.Generation {
		t.Fatal("another allowed user approved replacement")
	}
	callback(4, 7, token)
	waitFor(t, func() bool { b, e := db.Binding(99, -10, 11); return e == nil && b.Generation > old.Generation })
	replaced, _ := db.Binding(99, -10, 11)
	if replaced.Session == old.Session {
		t.Fatal("new reused previous session")
	}
	if replaced.Workspace != old.Workspace {
		t.Fatal("replacement changed the working directory")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatal("new removed old workspace files")
	}
	callback(5, 7, token)
	waitFor(t, func() bool {
		var state string
		_ = db.DB.QueryRow("SELECT state FROM inbox WHERE id=5").Scan(&state)
		return state == "done"
	})
	current, _ = db.Binding(99, -10, 11)
	if current.Generation != replaced.Generation {
		t.Fatal("consumed callback replaced session again")
	}
	after = f.messageCount()
	send(update(6, 11, "/compact"))
	waitFor(t, func() bool { return f.button(after) != "" })
	callback(7, 7, f.button(after))
	send(update(8, 11, "after compact"))
	waitFor(t, func() bool { return f.has(11, "answer: after compact") })
}

func TestResumeDoesNotRecreateMissingWorkspace(t *testing.T) {
	_, db, send := setupBridge(t)
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)
	send(update(2, 11, "/close"))
	waitInputDone(t, db, 2)
	before, err := db.Binding(99, -10, 11)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(before.Workspace); err != nil {
		t.Fatal(err)
	}
	send(update(3, 11, "/resume"))
	waitInputDone(t, db, 3)
	after, err := db.Binding(99, -10, 11)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("failed resume replaced durable binding")
	}
	if _, err := os.Stat(before.Workspace); !os.IsNotExist(err) {
		t.Fatal("missing workspace silently recreated")
	}
}

func TestNewUsesSelectedDirectory(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	source := t.TempDir()
	marker := filepath.Join(source, "source.txt")
	if err := os.WriteFile(marker, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	workspace, err := resolveWorkspace(w.b.cfg.WorkspaceRoot, source)
	if err != nil {
		t.Fatal(err)
	}
	command("/new " + source)
	if w.client == nil {
		t.Fatal("new worker failed to start")
	}
	if w.binding.Workspace != workspace || workspace != source {
		t.Fatal("new did not use the selected directory directly")
	}
	info, err := w.client.SessionInfo(w.ctx)
	if err != nil || info.CWD != source {
		t.Fatalf("omp cwd does not match selected directory: %+v, %v", info, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "untouched" {
		t.Fatal("source content changed")
	}
}

func TestNewNameCreatesAndReusesDirectory(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	workspace := filepath.Join(w.b.cfg.WorkspaceRoot, "test")
	command("/new test")
	if w.client == nil || w.binding.Workspace != workspace {
		t.Fatal("named workspace was not created directly under the configured root")
	}
	first := w.binding
	marker := filepath.Join(first.Workspace, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	command("/close")
	command("/new")
	if w.client == nil || w.binding.Workspace != workspace || w.binding.Session == first.Session {
		t.Fatal("new did not retain the named directory with a fresh session")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatal("previous workspace contents changed")
	}
}

func TestNewCreatesMissingAbsoluteDirectory(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	workspace := filepath.Join(t.TempDir(), "missing", "project")
	command("/new " + workspace)
	if w.client == nil || w.binding.Workspace != workspace {
		t.Fatal("new did not create and select the missing absolute directory")
	}
	info, err := w.client.SessionInfo(w.ctx)
	if err != nil || info.CWD != workspace {
		t.Fatalf("omp did not start in the created absolute directory: %+v, %v", info, err)
	}
}

func TestNewRejectsAbsoluteFile(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	command("/new " + file)
	if w.client != nil || len(w.b.slots) != 0 {
		t.Fatal("new started a worker for a regular file")
	}
	if _, err := w.b.db.Binding(99, -10, 11); err == nil {
		t.Fatalf("new bound a regular file: %v", err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "keep" {
		t.Fatal("new modified the selected regular file")
	}
}

// Drive commands synchronously so configuration changes and expiry do not race the actor.
func setupWorkspaceWorker(t *testing.T) (*worker, *fakeHTTP, func(string)) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeHTTP{}
	old := http.DefaultTransport
	http.DefaultTransport = f
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	w := &worker{b: &Bridge{cfg: config.Config{OMP: binary, WorkspaceRoot: t.TempDir()}, db: db, tg: telegram.New("fake"), bot: telegram.User{ID: 99}, slots: make(chan struct{}, 1)}, key: target{chat: -10, thread: 11}, ctx: ctx, cancel: cancel, confirms: make(map[string]confirmation)}
	w.resumeResults = make(chan resumeListResult, 4)
	t.Cleanup(func() {
		w.shutdown()
		cancel()
		w.background.Wait()
		http.DefaultTransport = old
		db.Close()
	})
	var id int64
	return w, f, func(text string) {
		id++
		u := update(id, 11, text)
		raw, err := json.Marshal(u)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Accept(id, raw); err != nil {
			t.Fatal(err)
		}
		w.handle(incoming{id: id, msg: u.Message})
	}
}

func TestUnboundNewDoesNotAllocate(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new")
	if w.client != nil || len(w.b.slots) != 0 {
		t.Fatal("unbound new allocated a worker")
	}
	if _, err := w.b.db.Binding(99, -10, 11); err == nil {
		t.Fatal("unbound new persisted a binding")
	}
	entries, err := os.ReadDir(w.b.cfg.WorkspaceRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unbound new changed workspace root: %v, %v", entries, err)
	}
}

func TestClosedNewReusesPersistedDirectoryAfterRootChange(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before, err := w.b.db.Binding(99, -10, 11)
	if err != nil {
		t.Fatal(err)
	}
	command("/close")
	w.b.cfg.WorkspaceRoot = t.TempDir()
	command("/new")
	after, err := w.b.db.Binding(99, -10, 11)
	if err != nil {
		t.Fatal(err)
	}
	if after.Workspace != before.Workspace || after.Session == before.Session {
		t.Fatal("closed new did not create a fresh session in the exact persisted directory")
	}
	entries, err := os.ReadDir(w.b.cfg.WorkspaceRoot)
	if err != nil || len(entries) != 0 {
		t.Fatal("no-argument new used changed configured root")
	}
}

func TestExplicitNewFreezesDirectoryAndRejectsInvalidConfirmations(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before, err := w.b.db.Binding(99, -10, 11)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	workspace, err := resolveWorkspace(w.b.cfg.WorkspaceRoot, source)
	if err != nil {
		t.Fatal(err)
	}
	menuAfter := f.messageCount()
	command("/new " + source)
	data := f.button(menuAfter)
	if data == "" {
		t.Fatal("replacement did not request confirmation")
	}
	assertUnchanged := func() {
		t.Helper()
		got, err := w.b.db.Binding(99, -10, 11)
		if err != nil || got != before {
			t.Fatal("unapproved replacement changed binding")
		}
	}
	assertUnchanged()
	click := func(user int64, token string) {
		w.callback(&telegram.CallbackQuery{ID: "confirm", From: telegram.User{ID: user}, Message: update(0, 11, "").Message, Data: token})
	}
	click(8, data)
	assertUnchanged()
	token, _, _ := strings.Cut(data, ":")
	c := w.confirms[token]
	c.expires = time.Now().Add(-time.Second)
	w.confirms[token] = c
	click(7, data)
	assertUnchanged()
	menuAfter = f.messageCount()
	command("/new " + source)
	data = f.button(menuAfter)
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	w.b.cfg.WorkspaceRoot = t.TempDir()
	click(7, data)
	after, err := w.b.db.Binding(99, -10, 11)
	if err != nil || after.Workspace != workspace || after.Session == before.Session {
		t.Fatalf("replacement did not use frozen directory: %+v, %v", after, err)
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		t.Fatalf("replacement did not recreate selected directory: %v", err)
	}
	click(7, data)
	replayed, err := w.b.db.Binding(99, -10, 11)
	if err != nil || replayed != after {
		t.Fatal("replayed confirmation changed binding")
	}
}

func TestInvalidNewPreservesRunningSession(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	before, err := w.b.db.Binding(99, -10, 11)
	if err != nil {
		t.Fatal(err)
	}
	client := w.client
	assertUsable := func() {
		t.Helper()
		got, err := w.b.db.Binding(99, -10, 11)
		if err != nil || got != before || w.client != client {
			t.Fatal("invalid new replaced running session")
		}
		if _, err := w.call("get_state", nil); err != nil {
			t.Fatalf("original session no longer usable: %v", err)
		}
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"relative/path", file} {
		command("/new " + input)
		assertUsable()
		if len(w.confirms) != 0 {
			t.Fatal("invalid source requested confirmation")
		}
	}
	source := t.TempDir()
	workspace, err := resolveWorkspace(w.b.cfg.WorkspaceRoot, source)
	if err != nil {
		t.Fatal(err)
	}
	after := f.messageCount()
	command("/new " + source)
	data := f.button(after)
	if data == "" {
		t.Fatal("replacement did not request confirmation")
	}
	if err := os.Remove(workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspace, []byte("obstruction"), 0600); err != nil {
		t.Fatal(err)
	}
	w.callback(&telegram.CallbackQuery{ID: "conflict", From: telegram.User{ID: 7}, Message: update(0, 11, "").Message, Data: data})
	assertUsable()
	if data, err := os.ReadFile(workspace); err != nil || string(data) != "obstruction" {
		t.Fatal("new modified conflicting directory file")
	}
}
