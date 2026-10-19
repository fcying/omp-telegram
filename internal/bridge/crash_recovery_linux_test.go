package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"omp-telegram/internal/config"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

// Keep subreaping process-local to this controller, not the ordinary test runner.
func TestCrashRecovery(t *testing.T) {
	if os.Getenv("OMP_TELEGRAM_CRASH_CONTROLLER") != "1" {
		root := t.TempDir()
		binary, err := os.Executable()
		requireStoreOK(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestCrashRecovery$", "-test.v")
		cmd.Env = append(os.Environ(), "OMP_TELEGRAM_CRASH_CONTROLLER=1", "TMPDIR="+root)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("crash recovery controller: %v\n%s", err, output)
		}
		t.Log(string(output))
		return
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Skipf("pidfd cleanup is unavailable: %v", err)
	}
	requireStoreOK(t, unix.Close(fd))
	t.Run("PromptUnacknowledged", testCrashPromptUnacknowledged)
	t.Run("DeliveryUncommitted", testCrashDeliveryUncommitted)
	t.Run("StartupUncommitted", testCrashStartupUncommitted)
}

// This entry runs the real daemon loop and store in a separate killable process.
func TestCrashRecoveryDaemonProcess(t *testing.T) {
	root := os.Getenv("OMP_TELEGRAM_CRASH_ROOT")
	if root == "" {
		return
	}
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", filepath.Join(root, "sessions"))
	endpoint, err := url.Parse(os.Getenv("OMP_TELEGRAM_CRASH_ENDPOINT"))
	if err != nil {
		t.Fatal(err)
	}
	http.DefaultTransport = &crashRecoveryTransport{endpoint: endpoint, base: http.DefaultTransport}
	environment, err := omp.NewEnvironment("allowlist", []string{
		"HOME", "TMPDIR", "PI_CODING_AGENT_DIR", "GORACE",
		"OMP_TELEGRAM_FIXTURE_SESSION_ROOT", "OMP_TELEGRAM_FIXTURE_RPC_TRACE", "OMP_TELEGRAM_FIXTURE_HOLD_RPC",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	requireStoreOK(t, err)
	cfg := config.Config{
		Token: "fake", AllowedUsers: []int64{7}, AllowedChats: []int64{-10},
		OMP: binary, OMPEnvironment: environment, OMPArgsFingerprint: strings.Repeat("b", 64),
		DataDir: filepath.Join(root, "data"), WorkspaceRoot: filepath.Join(root, "workspace"),
		MaxWorkers: 2, QueueCapacity: 4, ProgressMode: "off",
	}
	db, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := Run(ctx, cfg, db, testLogs(t), testAppVersion); err != nil {
		t.Fatal(err)
	}
}

type crashRecoveryTransport struct {
	endpoint *url.URL
	base     http.RoundTripper
}

func (p *crashRecoveryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.URL.Scheme = p.endpoint.Scheme
	request.URL.Host = p.endpoint.Host
	return p.base.RoundTrip(request)
}

type crashRecoveryDaemon struct {
	t        *testing.T
	root     string
	db       *store.Store
	fake     *fakeHTTP
	endpoint string
	cmd      *exec.Cmd
	output   bytes.Buffer
	nextID   int64
	mu       sync.Mutex
	holdText string
	accepted chan struct{}
}

func newCrashRecoveryDaemon(t *testing.T) *crashRecoveryDaemon {
	t.Helper()
	d := &crashRecoveryDaemon{t: t, root: t.TempDir(), fake: &fakeHTTP{updates: make(chan telegram.Update, 32)}}
	for _, name := range []string{"data", "workspace", "sessions", "home", "agent", "tmp"} {
		requireStoreOK(t, os.MkdirAll(filepath.Join(d.root, name), 0700))
	}
	db, err := store.Open(filepath.Join(d.root, "data"))
	requireStoreOK(t, err)
	requireStoreOK(t, db.Close())
	// Observe live durable state without running Store.Open's restart reconciliation.
	path := url.URL{Scheme: "file", Path: filepath.Join(d.root, "data", "omp-telegram.db"), RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	observer, err := sql.Open("sqlite", path.String())
	requireStoreOK(t, err)
	d.db = &store.Store{DB: observer}
	t.Cleanup(func() { requireStoreOK(t, observer.Close()) })
	server := httptest.NewServer(http.HandlerFunc(d.serveTelegram))
	d.endpoint = server.URL
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		d.stop(os.Kill)
		if t.Failed() {
			t.Log(d.output.String())
		}
	})
	return d
}

func (d *crashRecoveryDaemon) serveTelegram(w http.ResponseWriter, r *http.Request) {
	var text string
	if filepath.Base(r.URL.Path) == "sendMessage" {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var message struct{ Text string }
		if json.Unmarshal(body, &message) != nil {
			return
		}
		text = message.Text
	}
	response, err := d.fake.RoundTrip(r)
	if err != nil {
		return
	}
	defer response.Body.Close()
	d.mu.Lock()
	hold := d.holdText != "" && strings.Contains(text, d.holdText)
	if hold && d.accepted != nil {
		close(d.accepted)
		d.accepted = nil
	}
	d.mu.Unlock()
	if hold {
		// The remote side accepted the send, but no response reaches the daemon.
		<-r.Context().Done()
		return
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (d *crashRecoveryDaemon) start(holdRPC string) {
	d.t.Helper()
	d.output.Reset()
	binary, err := os.Executable()
	requireStoreOK(d.t, err)
	d.cmd = exec.Command(binary, "-test.run=^TestCrashRecoveryDaemonProcess$")
	d.cmd.Env = append(os.Environ(),
		"OMP_TELEGRAM_CRASH_ROOT="+d.root, "OMP_TELEGRAM_CRASH_ENDPOINT="+d.endpoint,
		"HOME="+filepath.Join(d.root, "home"), "TMPDIR="+filepath.Join(d.root, "tmp"),
		"PI_CODING_AGENT_DIR="+filepath.Join(d.root, "agent"),
		"OMP_TELEGRAM_FIXTURE_RPC_TRACE="+filepath.Join(d.root, "rpc-trace"),
		"OMP_TELEGRAM_FIXTURE_HOLD_RPC="+holdRPC,
	)
	d.cmd.Stdout, d.cmd.Stderr = &d.output, &d.output
	requireStoreOK(d.t, d.cmd.Start())
	d.command("/help")
}

func (d *crashRecoveryDaemon) stop(signal os.Signal) {
	d.t.Helper()
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	if err := d.cmd.Process.Signal(signal); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		d.t.Error(err)
	}
	err := d.cmd.Wait()
	d.cmd = nil
	if signal == os.Kill {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			d.t.Errorf("daemon was not killed at checkpoint: %v", err)
		}
	} else if err != nil {
		d.t.Errorf("daemon shutdown: %v\n%s", err, d.output.String())
	}
	// Only this isolated controller's adopted fixture children may be killed/reaped.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		paths, err := filepath.Glob("/proc/self/task/*/children")
		requireStoreOK(d.t, err)
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			requireStoreOK(d.t, err)
			for _, child := range strings.Fields(string(data)) {
				pid, err := strconv.Atoi(child)
				requireStoreOK(d.t, err)
				fd, err := unix.PidfdOpen(pid, 0)
				if errors.Is(err, unix.ESRCH) {
					continue
				}
				requireStoreOK(d.t, err)
				_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
				requireStoreOK(d.t, unix.Close(fd))
			}
		}
		_, err = unix.Wait4(-1, nil, unix.WNOHANG, nil)
		if errors.Is(err, unix.ECHILD) {
			return
		}
		requireStoreOK(d.t, err)
		time.Sleep(10 * time.Millisecond)
	}
	d.t.Error("test-owned fixture children were not reaped")
}

func (d *crashRecoveryDaemon) send(text string) int64 {
	d.t.Helper()
	d.nextID++
	d.fake.updates <- update(d.nextID, 11, text)
	return d.nextID
}

func (d *crashRecoveryDaemon) command(text string) {
	d.t.Helper()
	waitInputDone(d.t, d.db, d.send(text))
}

func (d *crashRecoveryDaemon) binding() store.Binding {
	d.t.Helper()
	binding, err := d.db.Binding(99, -10, 11)
	requireStoreOK(d.t, err)
	return binding
}

func (d *crashRecoveryDaemon) assertState(table string, id int64, want string) {
	d.t.Helper()
	waitFor(d.t, func() bool {
		var state string
		return d.db.DB.QueryRow("SELECT state FROM "+table+" WHERE id=?", id).Scan(&state) == nil && state == want
	})
}

func (d *crashRecoveryDaemon) probe(before store.Binding) {
	d.t.Helper()
	d.command("cwd")
	waitFor(d.t, func() bool { return d.fake.has(11, "answer: "+before.Workspace) })
	if after := d.binding(); !sameBindingIdentity(before, after) {
		d.t.Fatalf("recovery changed session identity: before=%+v after=%+v", before, after)
	}
}

func testCrashPromptUnacknowledged(t *testing.T) {
	d := newCrashRecoveryDaemon(t)
	d.start("prompt")
	d.command("/new " + filepath.Join(d.root, "workspace"))
	before := d.binding()
	active := d.send("crash-prompt")
	waitFixtureRPCTrace(t, filepath.Join(d.root, "rpc-trace"), "prompt", "crash-prompt")
	d.assertState("inbox", active, "submitted")
	queued := d.send("/followup crash-queued")
	d.assertState("inbox", queued, "pending")
	d.stop(os.Kill)
	d.assertState("inbox", active, "submitted")
	d.start("")
	d.assertState("inbox", active, "uncertain")
	d.assertState("inbox", queued, "cancelled")
	d.probe(before)
	counts := map[string]int{}
	for _, entry := range fixtureRPCTrace(filepath.Join(d.root, "rpc-trace")) {
		if entry.kind == "prompt" {
			counts[entry.payload]++
		}
	}
	if counts["crash-prompt"] != 1 || counts["crash-queued"] != 0 {
		t.Fatalf("replayed pre-crash work: %v", counts)
	}
	t.Log("SIGKILL before prompt ACK: submitted -> uncertain, queued -> cancelled, no replay, saved session usable")
}

func testCrashDeliveryUncommitted(t *testing.T) {
	d := newCrashRecoveryDaemon(t)
	d.start("")
	d.command("/new " + filepath.Join(d.root, "workspace"))
	before := d.binding()
	accepted := make(chan struct{})
	d.mu.Lock()
	d.holdText, d.accepted = "answer: crash-delivery", accepted
	d.mu.Unlock()
	input := d.send("crash-delivery")
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("Telegram did not accept the final reply")
	}
	var output int64
	requireStoreOK(t, d.db.DB.QueryRow("SELECT id FROM outbox WHERE inbox_id=? AND state='sending'", input).Scan(&output))
	d.assertState("inbox", input, "done")
	d.stop(os.Kill)
	d.assertState("outbox", output, "sending")
	d.start("")
	d.assertState("outbox", output, "uncertain")
	d.probe(before)
	d.fake.mu.Lock()
	defer d.fake.mu.Unlock()
	count := 0
	for _, message := range d.fake.messages {
		if strings.Contains(message["text"].(string), "answer: crash-delivery") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("ambiguous final reply was sent %d times", count)
	}
	t.Log("SIGKILL after Telegram acceptance: sending -> uncertain, no resend, later reply delivered")
}

func testCrashStartupUncommitted(t *testing.T) {
	d := newCrashRecoveryDaemon(t)
	d.start("")
	d.command("/new " + filepath.Join(d.root, "workspace"))
	before := d.binding()
	d.stop(os.Interrupt)
	requireStoreOK(t, os.WriteFile(filepath.Join(d.root, "rpc-trace"), nil, 0600))
	d.start("set_host_tools")
	workspace := filepath.Join(d.root, "replacement")
	after := d.fake.messageCount()
	d.command("/new " + workspace)
	waitFor(t, func() bool { return d.fake.button(after) != "" })
	d.nextID++
	d.fake.updates <- telegram.Update{UpdateID: d.nextID, CallbackQuery: &telegram.CallbackQuery{
		ID: "crash-start", From: telegram.User{ID: 7}, Message: update(0, 11, "").Message, Data: d.fake.button(after),
	}}
	waitFixtureRPCTrace(t, filepath.Join(d.root, "rpc-trace"), "set_host_tools", "")
	var intent store.StartIntent
	waitFor(t, func() bool {
		intents, err := d.db.PendingStarts(99)
		if err != nil || len(intents) != 1 || intents[0].Workspace != workspace {
			return false
		}
		intent = intents[0]
		return true
	})
	uncommitted := d.binding()
	if uncommitted.Session != before.Session || uncommitted.SessionID != before.SessionID || uncommitted.Workspace != before.Workspace || uncommitted.Generation != before.Generation || uncommitted.Running {
		t.Fatalf("startup overwrote the saved binding before commit: %+v", uncommitted)
	}
	d.stop(os.Kill)
	d.start("")
	intents, err := d.db.PendingStarts(99)
	requireStoreOK(t, err)
	if len(intents) != 1 || intents[0] != intent || !sameBindingIdentity(d.binding(), uncommitted) {
		t.Fatalf("recovery lost startup intent or replaced saved binding: intents=%+v binding=%+v", intents, d.binding())
	}
	d.command("/close")
	intents, err = d.db.PendingStarts(99)
	requireStoreOK(t, err)
	if len(intents) != 0 {
		t.Fatalf("explicit close did not cancel the startup intent: %+v", intents)
	}
	d.command("/resume " + before.SessionID)
	current := d.binding()
	if current.Session != before.Session || current.SessionID != before.SessionID || current.Workspace != before.Workspace || !current.Running {
		t.Fatalf("explicit resume could not recover the original session: %+v", current)
	}
	d.probe(current)
	t.Log("SIGKILL before CommitStart: old identity and intent preserved, explicit cancellation/resume usable")
}
