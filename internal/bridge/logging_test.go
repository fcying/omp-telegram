package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/logging"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

type logCapture struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.Write(p)
}
func (c *logCapture) String() string { c.mu.Lock(); defer c.mu.Unlock(); return c.data.String() }

func setupLoggingWorker(t *testing.T, writer io.Writer, format string) (*worker, func(string)) {
	t.Helper()
	w, _, command := setupWorkspaceWorker(t)
	logs, err := logging.New(writer, logging.Options{Level: "debug", Format: format})
	if err != nil {
		t.Fatal(err)
	}
	w.b.log = logs.Logger(logging.Bridge)
	w.b.telegramLog = logs.Logger(logging.Telegram)
	w.b.storeLog = logs.Logger(logging.Store)
	w.b.mediaLog = logs.Logger(logging.Media)
	w.b.rpcLog = logs.Logger(logging.RPC)
	w.log = w.b.log.With("chat_id", w.key.chat, "thread_id", w.key.thread)
	w.b.tg = telegram.New("fake", w.b.telegramLog)
	w.b.cfg.ProgressMode = "off"
	w.b.cfg.QueueCapacity = 4
	return w, command
}

func TestControlCommandsDoNotLogRootTaskCompletion(t *testing.T) {
	capture := &logCapture{}
	w, command := setupLoggingWorker(t, capture, "json")
	command("/new " + t.TempDir())
	command("/help")
	command("/status")
	command("/status@OtherBot")
	if capturedEvent(t, capture, "task_submit") != nil || capturedEvent(t, capture, "task_complete") != nil {
		t.Fatal("control or ignored command logged a root task")
	}
	w.b.cfg.QueueCapacity = 1
	command("pending task")
	command("rejected task")
	if capturedEvent(t, capture, "queue_rejected") == nil || capturedEvent(t, capture, "task_complete") != nil {
		t.Fatal("unaccepted input was logged as completed instead of rejected")
	}
	command("/stop")
	complete := capturedEvent(t, capture, "task_complete")
	if complete["result"] != "cancelled" {
		t.Fatalf("pending cancellation log = %+v", complete)
	}
	for _, field := range []string{"turn", "client_id", "duration_ms"} {
		if _, exists := complete[field]; exists {
			t.Errorf("pending task cancellation borrowed %s", field)
		}
	}
}

func capturedEvent(t *testing.T, capture *logCapture, event string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(capture.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid JSON log: %v", err)
		}
		if record["event"] == event {
			found = record
		}
	}
	return found
}

func TestRuntimeAndTaskLogsExposeOnlyLifecycleMetadata(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			capture := &logCapture{}
			w, command := setupLoggingWorker(t, capture, format)
			workspace := t.TempDir()
			command("/new " + workspace)
			w.b.cfg.IdleTimeout = time.Minute
			w.lastActivity = time.Now().Add(-2 * time.Minute)
			w.releaseIdleRuntime(time.Now())
			if format == "json" {
				record := capturedEvent(t, capture, "runtime_release")
				if record["component"] != "bridge" || record["level"] != "INFO" || record["reason"] != "idle" {
					t.Fatalf("idle release metadata=%+v", record)
				}
			}
			command("SECRET_PROMPT SECRET_OUTPUT")
			w.dispatch()
			root := w.active
			drainWatchdogEvents(t, w)
			if format == "json" {
				resume := capturedEvent(t, capture, "runtime_resume")
				if resume["level"] != "INFO" || resume["reason"] != "lazy" {
					t.Fatalf("lazy resume metadata=%+v", resume)
				}
				complete := capturedEvent(t, capture, "task_complete")
				if complete["component"] != "bridge" || complete["level"] != "INFO" || complete["result"] != "done" || complete["inbox_id"] != float64(root) {
					t.Fatalf("task completion metadata=%+v", complete)
				}
				for _, key := range []string{"generation", "turn", "client_id", "duration_ms"} {
					if _, ok := complete[key]; !ok {
						t.Errorf("completion omitted %s", key)
					}
				}
			}
			for _, secret := range []string{"SECRET_PROMPT", "SECRET_OUTPUT", "Authorization", "PRIVATE", workspace} {
				if strings.Contains(capture.String(), secret) {
					t.Errorf("logs exposed %q", secret)
				}
			}
		})
	}
}

func TestMissingTerminalRecoveryLogsWarn(t *testing.T) {
	capture := &logCapture{}
	w, command := setupLoggingWorker(t, capture, "json")
	command("/new " + t.TempDir())
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	root := w.active
	now := time.Now()
	w.lastActivity = now.Add(-stuckTaskQuietPeriod)
	for _, probeTime := range []time.Time{now, now.Add(time.Minute)} {
		w.probeStuckTask(probeTime)
		started := capturedEvent(t, capture, "watchdog_probe")
		if started["phase"] != "start" || started["generation"] != float64(w.binding.Generation) {
			t.Fatalf("probe start metadata=%+v", started)
		}
		select {
		case result := <-w.idleProbe.results:
			w.idleProbeFinished(result, probeTime)
		case <-time.After(6 * time.Second):
			t.Fatal("watchdog probe did not finish")
		}
		if confirmed := capturedEvent(t, capture, "watchdog_probe"); confirmed["phase"] != "confirmed" {
			t.Fatalf("probe confirmation metadata=%+v", confirmed)
		}
	}
	record := capturedEvent(t, capture, "watchdog_recover")
	if record["component"] != "bridge" || record["level"] != "WARN" || record["inbox_id"] != float64(root) || record["result"] != "uncertain" || record["replay"] != false {
		t.Fatalf("watchdog metadata=%+v", record)
	}
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", root).Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("watchdog log without durable uncertainty: %s %v", state, err)
	}
}

type failedLogWriter struct{}

func (failedLogWriter) Write([]byte) (int, error) { return 0, errors.New("log sink unavailable") }

func TestLogWriteFailureDoesNotChangeTaskCompletion(t *testing.T) {
	w, command := setupLoggingWorker(t, failedLogWriter{}, "json")
	command("/new " + t.TempDir())
	command("answer normally")
	w.dispatch()
	root := w.active
	drainWatchdogEvents(t, w)
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", root).Scan(&state); err != nil || state != "done" {
		t.Fatalf("log failure changed task result: state=%q err=%v", state, err)
	}
}

func TestFailedFinalCommitDoesNotLogSuccessfulCompletion(t *testing.T) {
	capture := &logCapture{}
	w, command := setupLoggingWorker(t, capture, "json")
	command("/new " + t.TempDir())
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	if _, err := w.b.db.DB.Exec("CREATE TRIGGER reject_final BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT, 'SECRET_DATABASE_DIAGNOSTIC'); END"); err != nil {
		t.Fatal(err)
	}
	w.preview = "SECRET_OUTPUT"
	w.finish()
	record := capturedEvent(t, capture, "final_commit_failed")
	if record["component"] != "store" || record["level"] != slog.LevelError.String() {
		t.Fatalf("commit failure metadata=%+v", record)
	}
	if record := capturedEvent(t, capture, "task_complete"); record != nil {
		t.Fatalf("failed commit logged completion: %+v", record)
	}
	if strings.Contains(capture.String(), "SECRET") {
		t.Fatal("commit failure logs exposed diagnostics or content")
	}
}

type startupLogTransport struct {
	target      string
	reject      bool
	entered     chan struct{}
	enteredOnce sync.Once
	fallback    fakeHTTP
}

func (f *startupLogTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if filepath.Base(r.URL.Path) != f.target {
		return f.fallback.RoundTrip(r)
	}
	f.enteredOnce.Do(func() { close(f.entered) })
	if f.reject {
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Request: r, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":403,"description":"SECRET_STARTUP_DIAGNOSTIC"}`))}, nil
	}
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestStartupCancellationDoesNotLogFailure(t *testing.T) {
	for _, endpoint := range []string{"getMe", "setMyCommands"} {
		for _, reject := range []bool{false, true} {
			name := endpoint + "/cancel"
			if reject {
				name = endpoint + "/reject"
			}
			t.Run(name, func(t *testing.T) {
				db, err := store.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				capture := &logCapture{}
				logs, err := logging.New(capture, logging.Options{Level: "debug", Format: "json"})
				if err != nil {
					t.Fatal(err)
				}
				transport := &startupLogTransport{target: endpoint, reject: reject, entered: make(chan struct{})}
				previous := http.DefaultTransport
				http.DefaultTransport = transport
				defer func() { http.DefaultTransport = previous }()
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() {
					defer close(done)
					done <- Run(ctx, config.Config{Token: "fake"}, db, logs)
				}()
				defer func() {
					cancel()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("startup did not stop")
					}
				}()
				select {
				case <-transport.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("startup did not reach Telegram request")
				}
				event := "telegram_identity_failed"
				if endpoint == "setMyCommands" {
					event = "telegram_commands_failed"
				}
				if reject && endpoint == "setMyCommands" {
					deadline := time.NewTimer(5 * time.Second)
					ticker := time.NewTicker(time.Millisecond)
					for capturedEvent(t, capture, event) == nil {
						select {
						case <-deadline.C:
							t.Fatal("startup rejection was not logged")
						case <-ticker.C:
						}
					}
					deadline.Stop()
					ticker.Stop()
				}
				if !reject || endpoint == "setMyCommands" {
					cancel()
				}
				select {
				case err := <-done:
					if endpoint == "getMe" {
						if err == nil || (!reject && !errors.Is(err, context.Canceled)) {
							t.Fatalf("startup error contract changed: %v", err)
						}
					} else if err != nil {
						t.Fatalf("command menu failure stopped startup: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("startup did not return")
				}
				record := capturedEvent(t, capture, event)
				if reject {
					wantLevel := "ERROR"
					if endpoint == "setMyCommands" {
						wantLevel = "WARN"
					}
					if record["level"] != wantLevel || record["api_code"] != float64(403) {
						t.Fatalf("genuine startup failure was not reported: %+v", record)
					}
				} else if record != nil {
					t.Fatalf("normal cancellation logged a startup failure: %+v", record)
				}
				if !reject {
					decoder := json.NewDecoder(strings.NewReader(capture.String()))
					for {
						var record map[string]any
						err := decoder.Decode(&record)
						if errors.Is(err, io.EOF) {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
						if record["level"] == "WARN" || record["level"] == "ERROR" {
							t.Fatalf("normal cancellation emitted an alert: %+v", record)
						}
					}
				}
				if strings.Contains(capture.String(), "SECRET_STARTUP_DIAGNOSTIC") {
					t.Fatal("startup error log exposed API description")
				}
			})
		}
	}
}

type logTestTransport func(*http.Request) (*http.Response, error)

func (f logTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeliveryFailureLogsOutputIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, reason, state string
		chat, thread        int64
		uncertain           bool
	}{
		{"deleted topic", "message_thread_not_found", "failed", -10042, 81, false},
		{"private transport failure", "transport_failed", "uncertain", 7, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &logCapture{}
			w, _ := setupLoggingWorker(t, capture, "json")
			if err := w.b.db.Enqueue(tc.chat, tc.thread, "SECRET_FINAL_TEXT"); err != nil {
				t.Fatal(err)
			}
			output, err := w.b.db.NextOutput()
			if err != nil {
				t.Fatal(err)
			}
			previous := http.DefaultTransport
			attempts := 0
			http.DefaultTransport = logTestTransport(func(r *http.Request) (*http.Response, error) {
				attempts++
				if tc.uncertain {
					return nil, errors.New("SECRET_TRANSPORT https://api.telegram.org/botSECRET_TOKEN/")
				}
				return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Request: r, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":400,"description":"Bad Request: message thread not found","unexpected":"SECRET_RESPONSE_BODY"}`))}, nil
			})
			defer func() { http.DefaultTransport = previous }()
			ctx, cancel := context.WithCancel(w.ctx)
			done := make(chan error, 1)
			go func() { defer close(done); done <- w.b.deliver(ctx) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("delivery did not stop")
				}
			}()
			waitFor(t, func() bool {
				var state string
				return w.b.db.DB.QueryRow("SELECT state FROM outbox WHERE id=?", output.ID).Scan(&state) == nil && state == tc.state
			})
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("delivery did not stop after failure")
			}
			record := capturedEvent(t, capture, "delivery_failed")
			if record["component"] != "telegram" || record["level"] != "WARN" || record["reason"] != tc.reason || record["uncertain"] != tc.uncertain || record["replay"] != false {
				t.Fatalf("delivery failure metadata=%+v", record)
			}
			if record["outbox_id"] != float64(output.ID) || record["chat_id"] != float64(tc.chat) || record["thread_id"] != float64(tc.thread) || record["kind"] != "text" || record["state"] != tc.state {
				t.Fatalf("delivery failure lost output identity: %+v", record)
			}
			if !tc.uncertain && record["api_code"] != float64(400) {
				t.Fatalf("missing API code: %+v", record)
			}
			if attempts != 1 {
				t.Fatalf("failed delivery was replayed: %d attempts", attempts)
			}
			if strings.Contains(capture.String(), "SECRET") || strings.Contains(capture.String(), "api.telegram.org") {
				t.Fatal("delivery log exposed content or diagnostics")
			}
		})
	}
}

func TestOutboxStateWriteFailuresLogCorrectEvent(t *testing.T) {
	for _, target := range []string{"sending", "done"} {
		t.Run(target, func(t *testing.T) {
			capture := &logCapture{}
			w, _ := setupLoggingWorker(t, capture, "json")
			transport := http.DefaultTransport.(*fakeHTTP)
			if err := w.b.db.Enqueue(-10042, 81, "SECRET_FINAL_TEXT"); err != nil {
				t.Fatal(err)
			}
			output, err := w.b.db.NextOutput()
			if err != nil {
				t.Fatal(err)
			}
			trigger := fmt.Sprintf("CREATE TRIGGER reject_outbox_state BEFORE UPDATE OF state ON outbox WHEN NEW.state='%s' BEGIN SELECT RAISE(ABORT, 'SECRET_STORE_DIAGNOSTIC'); END", target)
			if _, err := w.b.db.DB.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
			defer cancel()
			if err := w.b.deliver(ctx); err == nil {
				t.Fatal("outbox state failure was ignored")
			}
			record := capturedEvent(t, capture, "outbox_state_write_failed")
			if record["component"] != "store" || record["level"] != "ERROR" || record["outbox_id"] != float64(output.ID) || record["chat_id"] != float64(output.Chat) || record["thread_id"] != float64(output.Thread) || record["state"] != target {
				t.Fatalf("outbox persistence metadata=%+v", record)
			}
			if capturedEvent(t, capture, "inbox_state_write_failed") != nil {
				t.Fatal("outbox failure mislabeled as inbox failure")
			}
			transport.mu.Lock()
			sends := len(transport.messages)
			transport.mu.Unlock()
			wantState, wantSends := "pending", 0
			if target == "done" {
				wantState, wantSends = "sending", 1
			}
			var state string
			if err := w.b.db.DB.QueryRow("SELECT state FROM outbox WHERE id=?", output.ID).Scan(&state); err != nil || state != wantState || sends != wantSends {
				t.Fatalf("persistence failure changed delivery semantics: state=%q sends=%d err=%v", state, sends, err)
			}
			if strings.Contains(capture.String(), "SECRET") {
				t.Fatal("persistence log exposed content or diagnostics")
			}
		})
	}
}

func TestConversationLogsOmitBotID(t *testing.T) {
	capture := &logCapture{}
	fixture, _ := setupLoggingWorker(t, capture, "json")
	w := fixture.b.newWorker(fixture.ctx, target{chat: 7, thread: 0}, store.Binding{}, false, nil)
	t.Cleanup(func() {
		w.teardownWorker(true)
		w.cancel()
		w.background.Wait()
	})
	w.start(false, t.TempDir(), "", false)
	if w.client == nil {
		t.Fatal("fixture runtime did not start")
	}
	u := update(100, 0, "")
	u.Message.Chat.ID = 7
	u.Message.Chat.Type = "private"
	u.Message.Document = &telegram.Document{FileName: "missing-id.txt"}
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(u.UpdateID, raw); err != nil {
		t.Fatal(err)
	}
	w.handle(incoming{id: u.UpdateID, msg: u.Message})
	select {
	case result := <-w.mediaResults:
		w.preparedMedia(result)
	case <-time.After(5 * time.Second):
		t.Fatal("media preparation did not return")
	}
	for _, event := range []string{"runtime_connected", "prepare_failed"} {
		record := capturedEvent(t, capture, event)
		if record["chat_id"] != float64(7) || record["thread_id"] != float64(0) {
			t.Fatalf("%s lost conversation identity: %+v", event, record)
		}
		if _, exists := record["bot_id"]; exists {
			t.Fatalf("%s still includes bot identity: %+v", event, record)
		}
	}
}

type mediaDownloadTimeout struct{}

func (mediaDownloadTimeout) Error() string {
	return "SECRET_TIMEOUT https://api.telegram.org/botSECRET_TOKEN/"
}
func (mediaDownloadTimeout) Timeout() bool { return true }

func TestMediaPreparationLogsWrappedTelegramTimeout(t *testing.T) {
	capture := &logCapture{}
	w, command := setupLoggingWorker(t, capture, "json")
	fake, ok := http.DefaultTransport.(*fakeHTTP)
	if !ok {
		t.Fatal("workspace fixture did not install fake HTTP transport")
	}
	fake.mu.Lock()
	fake.downloadErr = mediaDownloadTimeout{}
	fake.mu.Unlock()
	command("/new " + t.TempDir())
	defer w.background.Wait()
	u := update(100, 11, "")
	u.Message.Document = &telegram.Document{FileID: "download-timeout", FileName: "input.txt", MimeType: "text/plain", FileSize: 4}
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(u.UpdateID, raw); err != nil {
		t.Fatal(err)
	}
	w.handle(incoming{id: u.UpdateID, msg: u.Message})
	select {
	case result := <-w.mediaResults:
		if result.err == nil || errors.Is(result.err, context.DeadlineExceeded) {
			t.Fatalf("fixture did not exercise a wrapped transport timeout: %v", result.err)
		}
		w.preparedMedia(result)
	case <-time.After(5 * time.Second):
		t.Fatal("media preparation did not return")
	}
	record := capturedEvent(t, capture, "prepare_failed")
	if record["component"] != "media" || record["level"] != "WARN" || record["error_kind"] != "timeout" {
		t.Fatalf("wrapped timeout log=%+v", record)
	}
	if strings.Contains(capture.String(), "SECRET") || strings.Contains(capture.String(), "api.telegram.org") {
		t.Fatal("media timeout log exposed transport diagnostics")
	}
}
