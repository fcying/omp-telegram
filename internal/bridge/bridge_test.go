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
	"omp-telegram/internal/logging"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func requireStoreOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func sameBindingIdentity(left, right store.Binding) bool {
	left.LastUsedAt = 0
	right.LastUsedAt = 0
	return left == right
}

func testLogs(t *testing.T) *logging.Registry {
	t.Helper()
	logs, err := logging.New(io.Discard, logging.Options{Level: "info", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

func testBridge(t *testing.T, b *Bridge) *Bridge {
	t.Helper()
	logs := testLogs(t)
	b.log = logs.Logger(logging.Bridge)
	b.telegramLog = logs.Logger(logging.Telegram)
	b.storeLog = logs.Logger(logging.Store)
	b.mediaLog = logs.Logger(logging.Media)
	b.rpcLog = logs.Logger(logging.RPC)
	return b
}

func testWorker(t *testing.T, w *worker) *worker {
	t.Helper()
	if w.b == nil {
		w.b = testBridge(t, &Bridge{})
	} else if w.b.log == nil {
		w.b = testBridge(t, w.b)
	}
	if w.log == nil {
		w.log = w.b.log.With("chat_id", w.key.chat, "thread_id", w.key.thread)
	}
	return w
}

func newTestTelegram(t *testing.T) *telegram.Client {
	return telegram.New("fake", testLogs(t).Logger(logging.Telegram))
}

// This subprocess speaks RPC to exercise the real pipe and actor boundaries.
func TestMain(m *testing.M) {
	if len(os.Args) == 5 && os.Args[1] == "config" && os.Args[2] == "get" && os.Args[4] == "--json" {
		var value any
		switch os.Args[3] {
		case "cycleOrder":
			value = []string{"smol", "default", "slow", "free"}
		case "modelRoles":
			value = map[string]string{"smol": "fixture/quick:low", "default": "fixture/safe:medium", "slow": "fixture/deep:high", "free": "fixture/free:off"}
		default:
			os.Exit(2)
		}
		if json.NewEncoder(os.Stdout).Encode(map[string]any{"key": os.Args[3], "value": value}) != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "acp" {
		runResumeListFixture()
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "render" {
		runResumeRenderFixture()
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
		sessionID := fmt.Sprintf("%08x-0000-4000-8000-%012x", os.Getpid(), os.Getpid())
		session := filepath.Join(root, sessionID+".jsonl")
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
			data, _ := json.Marshal(map[string]string{"type": "session", "id": sessionID, "cwd": cwd})
			if os.WriteFile(session, data, 0600) != nil {
				os.Exit(2)
			}
		}
		sessionID = strings.TrimSuffix(filepath.Base(session), ".jsonl")
		emit(map[string]any{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": []int{1, 2}, "maxFrameBytes": 1048576, "maxReassembledFrameBytes": 67108864})
		streaming := false
		modelProvider, modelID := "fixture", "safe"
		thinkingLevel := "medium"
		fastEnabled, fastActive := false, false
		sessionName := ""
		rootPrompts := 0
		uiReplies := 0
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
				uiReplies++
				continue
			case "host_tool_result":
				text := "attachment accepted"
				if failed, _ := cmd["isError"].(bool); failed {
					text = "attachment rejected"
				}
				streaming = false
				emit(map[string]any{"type": "agent_end", "messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}}})
				continue
			case "get_state":
				resp["data"] = map[string]any{"sessionId": sessionID, "sessionFile": session, "sessionName": sessionName, "model": map[string]any{"provider": modelProvider, "id": modelID, "headers": map[string]string{"Authorization": "SECRET"}}, "thinkingLevel": thinkingLevel, "fastModeEnabled": fastEnabled, "fastModeActive": fastActive, "systemPrompt": "PRIVATE", "fixtureRootPrompts": rootPrompts, "fixtureUIReplies": uiReplies}
				resp["data"].(map[string]any)["isStreaming"] = streaming
				resp["data"].(map[string]any)["isCompacting"] = false
			case "set_session_name":
				name, _ := cmd["name"].(string)
				if strings.TrimSpace(name) == "" || modelID == "reject-name" {
					resp["success"] = false
				} else {
					sessionName = strings.TrimSpace(name)
				}
			case "handoff":
				if streaming {
					resp["success"] = false
				} else if cmd["customInstructions"] == "hold" {
					continue
				} else if cmd["customInstructions"] == "cancel" {
					resp["data"] = nil
				} else if cmd["customInstructions"] == "fail" {
					resp["success"] = false
				} else {
					resp["data"] = map[string]any{}
				}
			case "set_fast_mode":
				enabled, ok := cmd["enabled"].(bool)
				if !ok || (enabled && modelID == "no-fast") {
					resp["success"] = false
				} else {
					fastEnabled = enabled
					fastActive = enabled && modelID != "fast-fallback"
					resp["data"] = map[string]bool{"enabled": fastEnabled, "active": fastActive}
				}
			case "set_model":
				modelProvider, _ = cmd["provider"].(string)
				modelID, _ = cmd["modelId"].(string)
				resp["data"] = map[string]string{"provider": modelProvider, "id": modelID}
			case "set_thinking_level":
				level, _ := cmd["level"].(string)
				if modelID == "reject-thinking" {
					resp["success"] = false
				} else if level == "max" {
					thinkingLevel = "high"
				} else {
					thinkingLevel = level
				}
			case "cycle_model":
				os.Exit(2)
			case "prompt":
				text, _ := cmd["message"].(string)
				if strings.HasPrefix(text, "/model @") {
					id, ok := map[string]string{"smol": "quick", "default": "safe", "slow": "deep", "free": "free"}[strings.TrimPrefix(text, "/model @")]
					if !ok {
						os.Exit(2)
					}
					modelProvider, modelID = "fixture", id
					emit(map[string]any{"type": "command_output", "text": "Model set to " + modelProvider + "/" + modelID + "."})
					resp["data"] = map[string]any{"agentInvoked": false}
					emit(resp)
					continue
				}
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
				if (text == "/review" || strings.HasPrefix(text, "/review ")) && strings.Contains(text, replyContextStart) {
					text = "native review: " + text
				}
				rootPrompts++
				if text == "failed-prompt-ack" {
					emit(map[string]any{"type": "agent_start"})
					resp["success"] = false
					emit(resp)
					continue
				}
				if _, ok := cmd["streamingBehavior"]; ok || streaming {
					os.Exit(2)
				}
				emit(resp)
				streaming = true
				emit(map[string]any{"type": "agent_start"})
				if text == "missing-terminal" || text == "nonterminal-only" {
					if text == "nonterminal-only" {
						emit(map[string]any{"type": "agent_end", "isTerminal": false})
					}
					streaming = false
					continue
				}
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
					if len(images) > 1 {
						text = fmt.Sprintf("images=%d; %s", len(images), text)
					}
				}
				if text == "cwd" {
					text, _ = os.Getwd()
				}
				emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": text}})
				emit(map[string]any{"type": "agent_end", "isTerminal": false})
				streaming = false
				emit(map[string]any{"type": "agent_end", "messages": []any{map[string]any{"role": "user", "content": text}, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "text": "PRIVATE"}, map[string]any{"type": "text", "text": "answer: " + text}}}}})
				continue
			case "abort":
				if _, ok := cmd["clearQueuedMessages"]; ok {
					os.Exit(2)
				}
				streaming = false
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
	mu                   sync.Mutex
	messages             []map[string]any
	callbacks            []string
	updates              chan telegram.Update
	rejectCommands       bool
	rejectLanguage       string
	files                map[string][]byte
	failProgress         bool
	progressCalls        int
	downloadGate         <-chan struct{}
	fileRequests         int
	uploads              []mediaUpload
	keyboardClears       []map[string]any
	deletedMessages      []map[string]any
	progressDeleteStatus int
	failKeyboardClear    bool
	keyboardClearGate    <-chan struct{}
	typingRequests       chan context.Context
}

func (f *fakeHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	if response, handled, err := f.mediaRequest(r); handled {
		return response, err
	}
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	if f.failProgress && (filepath.Base(r.URL.Path) == "sendMessage" || filepath.Base(r.URL.Path) == "editMessageText") {
		f.mu.Lock()
		f.progressCalls++
		f.mu.Unlock()
		return nil, errors.New("progress transport failed")
	}
	var result any = true
	switch filepath.Base(r.URL.Path) {
	case "sendChatAction":
		if f.typingRequests != nil {
			select {
			case f.typingRequests <- r.Context():
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
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
	case "deleteMessage":
		f.mu.Lock()
		f.deletedMessages = append(f.deletedMessages, req)
		status := f.progressDeleteStatus
		f.mu.Unlock()
		if status != 0 {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"ok":false,"error_code":%d,"description":"progress deletion rejected"}`, status))), Header: make(http.Header), Request: r}, nil
		}
	case "editMessageReplyMarkup":
		f.mu.Lock()
		f.keyboardClears = append(f.keyboardClears, req)
		fail := f.failKeyboardClear
		gate := f.keyboardClearGate
		f.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		if fail {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":400,"description":"keyboard cleanup failed"}`)), Header: make(http.Header), Request: r}, nil
		}
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
		gotThread, _ := m["message_thread_id"].(float64)
		if gotThread == float64(thread) && strings.Contains(m["text"].(string), text) {
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
	logs := testLogs(t)
	go func() { done <- Run(ctx, cfg, db, logs) }()
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
			err = Run(ctx, config.Config{Token: "fake", MaxWorkers: 1, QueueCapacity: 1}, db, testLogs(t))
			var apiErr *telegram.APIError
			if !errors.As(err, &apiErr) || apiErr.Code != 400 {
				t.Fatalf("registration failure did not stop startup: %v", err)
			}
		})
	}
}

func TestStartIsHiddenHelpAlias(t *testing.T) {
	for _, command := range botCommands {
		if command.Command == "start" {
			t.Fatal("start is registered in the visible command menu")
		}
	}
	if strings.Contains(commandHelp(), "/start") {
		t.Fatal("start is included in visible help")
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Accept(1, []byte(`{"update_id":1}`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{db: db, fatal: make(chan error, 1)}), key: target{chat: -10, thread: 11}, ctx: ctx, cancel: cancel})
	w.handle(incoming{id: 1, msg: &telegram.Message{Text: "/start"}})
	output, err := db.NextOutput()
	if err != nil || output.Text != commandHelp() {
		t.Fatalf("hidden start reply = %#v, err = %v", output, err)
	}
}

func TestQueueCommandIsVisible(t *testing.T) {
	for _, command := range botCommands {
		if command.Command == "queue" {
			if !strings.Contains(command.Description, "pending") {
				t.Fatalf("queue command description = %q", command.Description)
			}
			if !strings.Contains(commandHelp(), "/queue - "+command.Description) {
				t.Fatal("queue command missing from help")
			}
			return
		}
	}
	t.Fatal("queue command missing from Telegram menu")
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
	send(update(13, 11, "/review"))
	waitFor(t, func() bool {
		var state string
		return db.DB.QueryRow("SELECT state FROM inbox WHERE id=13").Scan(&state) == nil && state == "pending"
	})
	send(update(6, 11, "/stop"))
	waitFor(t, func() bool {
		var state string
		_ = db.DB.QueryRow("SELECT state FROM inbox WHERE id=4").Scan(&state)
		return state == "cancelled"
	})
	waitFor(t, func() bool {
		var state string
		return db.DB.QueryRow("SELECT state FROM inbox WHERE id=13").Scan(&state) == nil && state == "cancelled"
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
func TestTailUTF16(t *testing.T) {
	if got := tailUTF16("discard😀keep", 6); got != "😀keep" {
		t.Fatalf("UTF-16 tail = %q", got)
	}
}
func TestProgressOffKeepsInternalTextWithoutLiveDelivery(t *testing.T) {
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{cfg: config.Config{ProgressMode: "off"}}), busy: true})
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"checking bridge"}}`))
	w.event([]byte(`{"type":"tool_execution_start","toolCallId":"read-1","toolName":"read"}`))
	w.flushPreview()
	w.typing()
	if w.preview != "checking bridge" || len(w.progress.ActiveTools) != 1 {
		t.Fatal("off mode stopped internal progress tracking")
	}
	if w.previewBusy || w.previewID != 0 || w.lastPreview != "" {
		t.Fatal("off mode started live progress delivery")
	}
	if !w.lastTyping.IsZero() {
		t.Fatal("off mode sent a typing action")
	}
}

func TestProgressRenderingTracksConcurrentToolsAndLifecycle(t *testing.T) {
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{cfg: config.Config{ProgressMode: "summary"}}), active: 10, busy: true})
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"PRIVATE"}}`))
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"checking bridge"}}`))
	w.event([]byte(`{"type":"tool_execution_start","toolCallId":"read-1","toolName":"read","arguments":{"path":"SECRET path"},"result":"SECRET result"}`))
	w.event([]byte(`{"type":"tool_execution_start","toolCallId":"bash-1","toolName":"bash"}`))
	if got := w.progressStatus(); got != "Running tools..." {
		t.Fatalf("concurrent tool status = %q", got)
	}
	w.event([]byte(`{"type":"tool_execution_end","toolCallId":"read-1"}`))
	if len(w.progress.ActiveTools) != 1 || w.progress.ActiveTools["bash-1"].Name != "bash" {
		t.Fatal("ending one tool removed a concurrent tool")
	}
	progress := w.renderProgress()
	for _, want := range []string{"checking bridge", "bash: running", "Status: Running tool..."} {
		if !strings.Contains(progress, want) {
			t.Fatalf("summary progress missing %q: %q", want, progress)
		}
	}
	for _, hidden := range []string{"PRIVATE", "SECRET path", "SECRET result", "read: completed", "toolCallId"} {
		if strings.Contains(progress, hidden) {
			t.Fatalf("summary progress leaked %q: %q", hidden, progress)
		}
	}
	w.b.cfg.ProgressMode = "verbose"
	progress = w.renderProgress()
	if !strings.Contains(progress, "Recent tool activity:") || !strings.Contains(progress, "read: completed") {
		t.Fatalf("verbose progress omitted recent completion: %q", progress)
	}
}

func TestManualCompactionDoesNotReuseTaskProgress(t *testing.T) {
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{cfg: config.Config{ProgressMode: "summary"}}), busy: true, compacting: true, previewID: 42})
	if got := w.renderProgress(); got != "" {
		t.Fatalf("manual compaction rendered task progress: %q", got)
	}
}

func TestAgentStartPreservesRetryState(t *testing.T) {
	w := testWorker(t, &worker{active: 10, busy: true})
	w.event([]byte(`{"type":"auto_retry_start"}`))
	if !w.progress.Retrying {
		t.Fatal("auto_retry_start did not enable retry state")
	}
	w.event([]byte(`{"type":"agent_start"}`))
	if !w.progress.Retrying {
		t.Fatal("agent_start cleared active retry state")
	}
	w.event([]byte(`{"type":"auto_retry_end"}`))
	if w.progress.Retrying {
		t.Fatal("auto_retry_end did not clear retry state")
	}
}

func TestHostToolCompletionWaitsForToolExecutionEnd(t *testing.T) {
	w := testWorker(t, &worker{active: 0, busy: true})
	w.event([]byte(`{"type":"tool_execution_start","toolCallId":"tool-1","toolName":"telegram_send"}`))
	w.event([]byte(`{"type":"host_tool_call","id":"host-1","toolCallId":"tool-1","toolName":"telegram_send"}`))
	w.event([]byte(`{"type":"host_tool_cancel","targetId":"host-1"}`))
	if _, ok := w.progress.ActiveTools["tool-1"]; !ok {
		t.Fatal("host tool event ended progress before tool_execution_end")
	}
	w.event([]byte(`{"type":"tool_execution_end","toolCallId":"tool-1"}`))
	if len(w.progress.ActiveTools) != 0 || len(w.progress.RecentTools) != 1 || w.progress.RecentTools[0].Running {
		t.Fatal("tool_execution_end did not exclusively complete host tool progress")
	}
}

func TestProgressStatusPriorityAndBounds(t *testing.T) {
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{cfg: config.Config{ProgressMode: "verbose"}}), active: 10, busy: true, preview: strings.Repeat("😀", maxProgressUnits)})
	w.startProgressTool("tool-1", "bash")
	w.compacting = true
	if !strings.Contains(w.renderProgress(), "Status: Compacting context...") {
		t.Fatal("compaction status was not rendered")
	}
	w.progress.Retrying = true
	progress := w.renderProgress()
	if !strings.Contains(progress, "Status: Retrying automatically...") || len(utf16.Encode([]rune(progress))) > maxProgressUnits {
		t.Fatalf("retry priority or progress bound failed: %d UTF-16 units", len(utf16.Encode([]rune(progress))))
	}
}

func TestProgressCompletionFences(t *testing.T) {
	for _, result := range []previewResult{
		{generation: 6, turn: 8, id: 99, text: "stale generation"},
		{generation: 7, turn: 9, id: 99, text: "stale turn"},
	} {
		w := testWorker(t, &worker{binding: store.Binding{Generation: 7}, turn: 8, previewBusy: true, previewID: 42, lastPreview: "current", finishing: true})
		w.previewFinished(result)
		if w.previewID != 42 || w.lastPreview != "current" || !w.finishing {
			t.Fatalf("stale progress result changed current turn: %#v", w)
		}
	}
	w := testWorker(t, &worker{binding: store.Binding{Generation: 7}, turn: 8, previewBusy: true, previewID: 42, lastPreview: "current"})
	w.previewFinished(previewResult{generation: 7, turn: 8, id: 42, text: "failed", err: errors.New("edit failed")})
	if w.progressSuppressed || w.lastPreview != "current" || w.previewID != 42 {
		t.Fatal("edit failure was not retained for a later retry")
	}
}

func TestProgressEditCannotRestoreClearedAssociation(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	requireStoreOK(t, db.Accept(10, []byte(`{"update_id":10}`)))
	requireStoreOK(t, db.Mark(10, "submitted"))
	requireStoreOK(t, db.CompleteInboxWithReplies(context.Background(), 10, 7, 8, []string{"reply"}))
	requireStoreOK(t, db.SetProgressMessage(10, 77))
	requireStoreOK(t, db.ClearProgressMessage(10, 77))
	fake := &fakeHTTP{}
	previous := http.DefaultTransport
	http.DefaultTransport = fake
	defer func() { http.DefaultTransport = previous }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := testWorker(t, &worker{
		b:             testBridge(t, &Bridge{cfg: config.Config{ProgressMode: "summary"}, db: db, tg: newTestTelegram(t), fatal: make(chan error, 1)}),
		binding:       store.Binding{Generation: 7},
		turn:          8,
		ctx:           ctx,
		cancel:        cancel,
		active:        10,
		busy:          true,
		preview:       "late edit",
		previewID:     77,
		lastPreview:   "current",
		previewResult: make(chan previewResult, 1),
		confirms:      make(map[string]confirmation),
	})
	w.flushPreview()
	result := <-w.previewResult
	if result.created {
		t.Fatal("Edit response was marked as a newly created progress message")
	}
	w.previewFinished(result)
	var progressID int64
	requireStoreOK(t, db.DB.QueryRow("SELECT progress_message_id FROM inbox WHERE id=10").Scan(&progressID))
	if progressID != 0 {
		t.Fatalf("late edit restored progress association: %d", progressID)
	}
	select {
	case err := <-w.b.fatal:
		t.Fatalf("late edit made bridge fatal: %v", err)
	default:
	}
}

func TestProgressAssociationFailureDeletesSentMessage(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	requireStoreOK(t, db.Accept(10, []byte(`{"update_id":10}`)))
	requireStoreOK(t, db.Mark(10, "submitted"))
	_, err = db.DB.Exec(`CREATE TRIGGER reject_progress_association BEFORE UPDATE OF progress_message_id ON inbox BEGIN SELECT RAISE(FAIL,'injected progress association failure'); END`)
	requireStoreOK(t, err)
	fake := &fakeHTTP{}
	previous := http.DefaultTransport
	http.DefaultTransport = fake
	defer func() { http.DefaultTransport = previous }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := testWorker(t, &worker{
		b:             testBridge(t, &Bridge{cfg: config.Config{ProgressMode: "summary"}, db: db, tg: newTestTelegram(t), fatal: make(chan error, 1)}),
		binding:       store.Binding{Generation: 1},
		ctx:           ctx,
		cancel:        cancel,
		active:        10,
		owner:         7,
		turn:          1,
		busy:          true,
		preview:       "working",
		previewResult: make(chan previewResult, 1),
		confirms:      make(map[string]confirmation),
	})
	w.flushPreview()
	result := <-w.previewResult
	if result.err != nil || result.id == 0 {
		t.Fatalf("initial progress send failed: %+v", result)
	}
	w.previewFinished(result)
	waitFor(t, func() bool { return len(fake.deletedMessageIDs()) == 1 })
	if got := fake.deletedMessageIDs(); len(got) != 1 || got[0] != result.id {
		t.Fatalf("orphan progress deletion = %v, want [%d]", got, result.id)
	}
	select {
	case err := <-w.b.fatal:
		if err == nil {
			t.Fatal("association failure reported a nil fatal error")
		}
	default:
		t.Fatal("association failure did not remain fatal")
	}
}

func TestProgressModeDoesNotChangeDurableCompletion(t *testing.T) {
	for _, mode := range []string{"off", "summary", "verbose"} {
		t.Run(mode, func(t *testing.T) {
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
			w := testWorker(t, &worker{b: testBridge(t, &Bridge{cfg: config.Config{ProgressMode: mode}, db: db}), ctx: ctx, cancel: cancel, active: 10, busy: true, preview: "answer", confirms: map[string]confirmation{}})
			w.finish()
			output, err := db.NextOutput()
			if err != nil || output.Text != "answer" {
				t.Fatalf("durable output = %#v, err = %v", output, err)
			}
			var state string
			if err = db.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state); err != nil || state != "done" {
				t.Fatalf("inbox state = %q, err = %v", state, err)
			}
		})
	}
}

func TestDeliveryCleansDoneProgressAfterAllReplies(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	requireStoreOK(t, db.Accept(10, []byte(`{}`)))
	requireStoreOK(t, db.Mark(10, "submitted"))
	requireStoreOK(t, db.CompleteInboxWithReplies(context.Background(), 10, 7, 8, []string{"first", "second"}))
	requireStoreOK(t, db.SetProgressMessage(10, 77))
	fake := &fakeHTTP{}
	previous := http.DefaultTransport
	http.DefaultTransport = fake
	defer func() { http.DefaultTransport = previous }()
	b := testBridge(t, &Bridge{db: db, tg: newTestTelegram(t)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.deliver(ctx) }()
	waitFor(t, func() bool {
		ids := fake.deletedMessageIDs()
		return len(ids) == 1 && ids[0] == 77
	})
	var state string
	requireStoreOK(t, db.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state))
	if state != "done" {
		t.Fatalf("inbox state = %q, want done", state)
	}
	var progressID int64
	requireStoreOK(t, db.DB.QueryRow("SELECT progress_message_id FROM inbox WHERE id=10").Scan(&progressID))
	if progressID != 0 {
		t.Fatalf("progress association remained after delivery: %d", progressID)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("delivery loop returned error: %v", err)
	}
}
func TestProgressDeletionTransientFailureRetainsAssociation(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	requireStoreOK(t, db.Accept(10, []byte(`{}`)))
	requireStoreOK(t, db.Mark(10, "submitted"))
	requireStoreOK(t, db.CompleteInboxWithReplies(context.Background(), 10, 7, 8, []string{"reply"}))
	requireStoreOK(t, db.SetProgressMessage(10, 77))
	out, err := db.NextOutput()
	requireStoreOK(t, err)
	requireStoreOK(t, db.MarkOutput(out.ID, "done"))
	fake := &fakeHTTP{progressDeleteStatus: http.StatusInternalServerError}
	previous := http.DefaultTransport
	http.DefaultTransport = fake
	defer func() { http.DefaultTransport = previous }()
	b := testBridge(t, &Bridge{db: db, tg: newTestTelegram(t)})
	b.cleanupDeliveredProgress(context.Background(), 10)
	var progressID int64
	requireStoreOK(t, db.DB.QueryRow("SELECT progress_message_id FROM inbox WHERE id=10").Scan(&progressID))
	if progressID != 77 {
		t.Fatalf("failed deletion cleared progress association: %d", progressID)
	}
	fake.progressDeleteStatus = 0
	b.reconcileProgressCleanup(context.Background())
	requireStoreOK(t, db.DB.QueryRow("SELECT progress_message_id FROM inbox WHERE id=10").Scan(&progressID))
	if progressID != 0 {
		t.Fatalf("successful retry left progress association: %d", progressID)
	}
}

func TestProgressDeletionPermanentFailureClearsAssociation(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	requireStoreOK(t, db.Accept(10, []byte(`{"update_id":10}`)))
	requireStoreOK(t, db.Mark(10, "submitted"))
	requireStoreOK(t, db.CompleteInboxWithReplies(context.Background(), 10, 7, 8, []string{"reply"}))
	requireStoreOK(t, db.SetProgressMessage(10, 77))
	out, err := db.NextOutput()
	requireStoreOK(t, err)
	requireStoreOK(t, db.MarkOutput(out.ID, "done"))
	fake := &fakeHTTP{progressDeleteStatus: http.StatusBadRequest}
	previous := http.DefaultTransport
	http.DefaultTransport = fake
	defer func() { http.DefaultTransport = previous }()
	b := testBridge(t, &Bridge{db: db, tg: newTestTelegram(t)})
	b.cleanupDeliveredProgress(context.Background(), 10)
	var progressID int64
	requireStoreOK(t, db.DB.QueryRow("SELECT progress_message_id FROM inbox WHERE id=10").Scan(&progressID))
	if progressID != 0 {
		t.Fatalf("permanent deletion failure retained progress association: %d", progressID)
	}
}

func TestProgressDeliveryFailureDoesNotAffectWorker(t *testing.T) {
	fake := &fakeHTTP{failProgress: true}
	old := http.DefaultTransport
	http.DefaultTransport = fake
	defer func() { http.DefaultTransport = old }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{cfg: config.Config{ProgressMode: "summary"}, tg: newTestTelegram(t), fatal: make(chan error, 1)}),
		binding:       store.Binding{Generation: 1},
		ctx:           ctx,
		cancel:        cancel,
		active:        10,
		owner:         7,
		turn:          1,
		busy:          true,
		preview:       "working",
		previewResult: make(chan previewResult, 1),
		confirms:      make(map[string]confirmation)})
	w.flushPreview()
	result := <-w.previewResult
	if result.err == nil {
		t.Fatal("failed Telegram progress delivery was reported as successful")
	}
	w.previewFinished(result)
	w.flushPreview()
	if !w.progressSuppressed || !w.busy || w.previewBusy || w.lastPreview != "" || w.previewID != 0 || len(w.confirms) != 0 || w.previewStopToken != "" {
		t.Fatal("initial progress failure changed worker task state incorrectly")
	}
	fake.mu.Lock()
	calls := fake.progressCalls
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("initial progress send retried %d times", calls)
	}
	select {
	case err := <-w.b.fatal:
		t.Fatalf("progress failure made daemon fatal: %v", err)
	default:
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
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{db: db}), ctx: ctx, cancel: cancel, active: 10, previewBusy: true, preview: "answer", busy: true, confirms: map[string]confirmation{}})
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
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{db: db}), ctx: ctx, cancel: cancel, active: 10, busy: true, confirms: map[string]confirmation{}})
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

func TestNormalTerminalCompletionIsDone(t *testing.T) {
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
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{db: db}), ctx: ctx, cancel: cancel, active: 10, busy: true, confirms: map[string]confirmation{}})
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","content":[{"type":"text","text":"final answer"}],"stopReason":"stop"}]}`))
	var state string
	if err = db.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state); err != nil || state != "done" {
		t.Fatalf("normal terminal state = %q, error %v", state, err)
	}
	o, err := db.NextOutput()
	if err != nil || o.Text != "final answer" {
		t.Fatalf("normal terminal output = %q, error %v", o.Text, err)
	}
}

func TestCompactedTerminalEventsUseMessageEndMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, preview, message, state, want, hidden string
	}{
		{
			name:    "aborted",
			preview: "partial answer",
			message: `{"role":"assistant","content":[{"type":"text","text":"partial answer"}],"stopReason":"aborted","errorMessage":"Request was aborted"}`,
			state:   "cancelled",
			want:    "Task was cancelled before completion.",
			hidden:  "Request was aborted",
		},
		{
			name:    "error",
			preview: "partial answer",
			message: `{"role":"assistant","stopReason":"error","errorStatus":500,"errorMessage":"SECRET diagnostics"}`,
			state:   "uncertain",
			want:    "omp reported that the task failed",
			hidden:  "SECRET diagnostics",
		},
		{
			name:    "structured rate limit status",
			preview: "partial answer",
			message: `{"role":"assistant","stopReason":"error","errorStatus":429,"errorMessage":"Resource exhausted"}`,
			state:   "uncertain",
			want:    "rate-limited by the model provider",
			hidden:  "Resource exhausted",
		},
		{
			name:    "structured rate limit classification",
			preview: "partial answer",
			message: `{"role":"assistant","stopReason":"error","errorClassificationMessage":"rate_limit_error","errorMessage":"Resource exhausted"}`,
			state:   "uncertain",
			want:    "rate-limited by the model provider",
			hidden:  "rate_limit_error",
		},
		{
			name:    "rate limited",
			preview: "partial answer",
			message: `{"role":"assistant","stopReason":"error","errorMessage":"429 Too Many Requests"}`,
			state:   "uncertain",
			want:    "rate-limited by the model provider",
			hidden:  "429 Too Many Requests",
		},
		{
			name:    "embedded status code is not rate limit",
			preview: "partial answer",
			message: `{"role":"assistant","stopReason":"error","errorMessage":"request-id=abc429xyz"}`,
			state:   "uncertain",
			want:    "omp reported that the task failed",
			hidden:  "request-id=abc429xyz",
		},
		{
			name:    "rewritten normal",
			preview: "old streamed answer",
			message: `{"role":"assistant","content":[{"type":"text","text":"rewritten final answer"}],"stopReason":"stop"}`,
			state:   "done",
			want:    "rewritten final answer",
			hidden:  "old streamed answer",
		},
		{
			name:    "normal without delta",
			message: `{"role":"assistant","content":[{"type":"text","text":"final answer"}],"stopReason":"stop"}`,
			state:   "done",
			want:    "final answer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			w := testWorker(t, &worker{b: testBridge(t, &Bridge{db: db}), ctx: ctx, cancel: cancel, active: 10, busy: true, confirms: map[string]confirmation{}})
			w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"` + tc.preview + `"}}`))
			w.event([]byte(`{"type":"message_end","message":` + tc.message + `}`))
			w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[],"messageCount":1}`))
			var state string
			if err = db.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state); err != nil || state != tc.state {
				t.Fatalf("compacted terminal state = %q, error %v; want %q", state, err, tc.state)
			}
			o, err := db.NextOutput()
			if err != nil || !strings.Contains(o.Text, tc.want) || tc.hidden != "" && strings.Contains(o.Text, tc.hidden) {
				t.Fatalf("compacted terminal output = %q, error %v", o.Text, err)
			}
		})
	}
}

func TestAgentStartClearsTerminalMetadata(t *testing.T) {
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
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{db: db}), ctx: ctx, cancel: cancel, active: 10, busy: true, confirms: map[string]confirmation{}})
	w.event([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"aborted","errorMessage":"Request was aborted"}}`))
	w.event([]byte(`{"type":"agent_start"}`))
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"final answer"}}`))
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[]}`))
	var state string
	if err = db.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state); err != nil || state != "done" {
		t.Fatalf("post-agent-start state = %q, error %v", state, err)
	}
}

func TestTerminalAbortIsCancelled(t *testing.T) {
	for _, tc := range []struct {
		name, event, want, hidden string
	}{
		{
			name:   "partial output",
			event:  `{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","content":[{"type":"text","text":"partial answer"}],"stopReason":"aborted","errorMessage":"Request was aborted"}]}`,
			want:   "partial answer",
			hidden: "Request was aborted",
		},
		{
			name:   "empty output",
			event:  `{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","stopReason":"aborted","errorMessage":"Stopped by user"}]}`,
			want:   "Task was cancelled before completion.",
			hidden: "Stopped by user",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			w := testWorker(t, &worker{b: testBridge(t, &Bridge{db: db}), ctx: ctx, cancel: cancel, active: 10, busy: true, confirms: map[string]confirmation{}})
			w.event([]byte(tc.event))
			var state string
			if err = db.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state); err != nil || state != "cancelled" {
				t.Fatalf("terminal abort state = %q, error %v", state, err)
			}
			o, err := db.NextOutput()
			if err != nil || !strings.Contains(o.Text, tc.want) || !strings.Contains(o.Text, "Task was cancelled before completion.") || strings.Contains(o.Text, tc.hidden) {
				t.Fatalf("terminal abort output = %q, error %v", o.Text, err)
			}
		})
	}
}

func TestNonterminalAgentEndDoesNotCompleteInput(t *testing.T) {
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
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{db: db}), ctx: ctx, cancel: cancel, active: 10, busy: true, confirms: map[string]confirmation{}})
	w.event([]byte(`{"type":"agent_end","isTerminal":false,"messages":[{"role":"assistant","stopReason":"error"}]}`))
	var state string
	if err = db.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state); err != nil || state != "submitted" || w.active != 10 {
		t.Fatalf("nonterminal agent end state = %q, active %d, error %v", state, w.active, err)
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
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{db: db}), ctx: ctx, cancel: cancel, active: 10, busy: true, confirms: map[string]confirmation{}})
	w.event([]byte(`{"type":"agent_end"}`))
	var state string
	if err = db.DB.QueryRow("SELECT state FROM inbox WHERE id=10").Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("empty terminal state = %q, error %v", state, err)
	}
}

func TestDispatchWaitsForPreviewCleanup(t *testing.T) {
	w := testWorker(t, &worker{client: &omp.Client{}, finishing: true, queue: []queued{{id: 1}}})
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
	w.teardownWorker(false)
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

func (f *fakeHTTP) deletedMessageIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int64, 0, len(f.deletedMessages))
	for _, message := range f.deletedMessages {
		if id, ok := message["message_id"].(float64); ok {
			ids = append(ids, int64(id))
		}
	}
	return ids
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
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{cfg: config.Config{OMP: binary, WorkspaceRoot: t.TempDir()}, db: db, tg: newTestTelegram(t), bot: telegram.User{ID: 99}, slots: make(chan struct{}, 1)}), key: target{chat: -10, thread: 11}, ctx: ctx, cancel: cancel, confirms: make(map[string]confirmation)})
	w.resumeResults = make(chan resumeListResult, 4)
	t.Cleanup(func() {
		w.teardownWorker(true)
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

func TestIdleReviewWithArgumentsCompletes(t *testing.T) {
	f, db, send := setupBridge(t)
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)
	send(update(2, 11, "/review staged changes"))
	waitFor(t, func() bool { return f.has(11, "answer: /review staged changes") })
	waitInputDone(t, db, 2)
	send(update(3, 11, "after review"))
	waitFor(t, func() bool { return f.has(11, "answer: after review") })
	waitInputDone(t, db, 3)
}

func TestUnsupportedFollowupDoesNotStartTask(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new " + t.TempDir())
	command("/followup must not run")
	w.dispatch()
	if w.busy || w.active != 0 || len(w.queue) != 0 {
		t.Fatal("unsupported followup started or queued a task")
	}
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&state); err != nil || state != "done" {
		t.Fatalf("unsupported command state=%q, err=%v", state, err)
	}
}

func TestFailedPromptAckClosesClientBeforeDispatch(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	if w.client == nil {
		t.Fatal("fixture did not start")
	}
	client := w.client
	command("failed-prompt-ack")
	command("/review must not run")
	w.dispatch()
	for id, want := range map[int64]string{2: "uncertain", 3: "cancelled"} {
		var state string
		if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", id).Scan(&state); err != nil || state != want {
			t.Fatalf("input %d: state=%q, want=%q, err=%v", id, state, want, err)
		}
	}
	if w.client != nil || len(w.queue) != 0 {
		t.Fatal("failed prompt retained a client or queued work")
	}
	select {
	case <-client.Done():
	default:
		t.Fatal("uncertain process still running")
	}
	binding, err := w.b.db.Binding(99, -10, 11)
	if err != nil || binding.Running {
		t.Fatalf("uncertain process remains recoverable: %+v, %v", binding, err)
	}
}

func TestUnboundNewUsesWorkspaceRoot(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	root := filepath.Join(w.b.cfg.WorkspaceRoot, "default-workspace")
	w.b.cfg.WorkspaceRoot = root
	command("/new")
	if w.client == nil {
		t.Fatal("unbound new did not start a native session")
	}
	binding, err := w.b.db.Binding(99, -10, 11)
	if err != nil || binding.Workspace != root || !binding.Running {
		t.Fatalf("default workspace binding = %+v, err=%v", binding, err)
	}
	info, err := w.client.SessionInfo(w.ctx)
	if err != nil || info.CWD != root {
		t.Fatalf("native default directory = %q, err=%v", info.CWD, err)
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

func TestWorkerExitIdentityDoesNotDeleteReplacement(t *testing.T) {
	key := target{chat: -10, thread: 11}
	oldWorker := &worker{}
	newWorker := &worker{}
	workers := map[target]*worker{key: newWorker}
	removeExitedWorker(workers, workerExit{key: key, worker: oldWorker})
	if workers[key] != newWorker {
		t.Fatal("stale worker exit removed replacement worker")
	}
	removeExitedWorker(workers, workerExit{key: key, worker: newWorker})
	if _, ok := workers[key]; ok {
		t.Fatal("matching worker exit did not remove worker")
	}
}

func TestIdleWorkerRequestsExit(t *testing.T) {
	oldTimeout := logicalWorkerIdleTimeout
	logicalWorkerIdleTimeout = 10 * time.Millisecond
	t.Cleanup(func() { logicalWorkerIdleTimeout = oldTimeout })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := testBridge(t, &Bridge{cfg: config.Config{QueueCapacity: 1}, ctx: ctx, workerExits: make(chan workerExit, 1)})
	w := b.newWorker(ctx, target{chat: -10, thread: 11}, store.Binding{}, false, nil)
	b.runWorker(w)
	var exit workerExit
	select {
	case exit = <-b.workerExits:
	case <-time.After(3 * time.Second):
		t.Fatal("idle worker did not request exit")
	}
	if exit.key != w.key || exit.worker != w || !w.exitRequestedState() {
		t.Fatalf("worker exit = %+v, worker=%p, requested=%t", exit, w, w.exitRequestedState())
	}
	b.wg.Wait()
}

func TestRateLimitTextFallbackRequiresRequestsPhrase(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{text: "Too Many Requests", want: true},
		{text: "rate_limit_error", want: true},
		{text: "too many request IDs", want: false},
	} {
		if got := isRateLimitedError(tc.text); got != tc.want {
			t.Fatalf("isRateLimitedError(%q) = %t, want %t", tc.text, got, tc.want)
		}
	}
}
