package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

// Probe native RPC behavior and the production bridge's pre-admission naming race.
func TestRealOMPAutoSessionNameCompatibility(t *testing.T) {
	binary := os.Getenv("OMP_TELEGRAM_TEST_OMP")
	if binary == "" {
		t.Skip("set OMP_TELEGRAM_TEST_OMP to an actual omp executable")
	}
	binary, err := filepath.Abs(binary)
	requireStoreOK(t, err)
	info, err := os.Stat(binary)
	requireStoreOK(t, err)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatal("explicit OMP binary is not an executable regular file")
	}
	root, err := filepath.Abs(t.TempDir())
	requireStoreOK(t, err)
	// ACP lists the native cwd-derived bucket, not an arbitrary --session-dir.
	sessionDir := filepath.Join(root, "agent", "sessions", "-workspace")
	for _, name := range []string{filepath.Join("home", "workspace"), filepath.Join("agent", "sessions", "-workspace"), "tmp"} {
		requireStoreOK(t, os.MkdirAll(filepath.Join(root, name), 0700))
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(root, "agent"))
	t.Setenv("TMPDIR", filepath.Join(root, "tmp"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	environment, err := omp.NewEnvironment("allowlist", []string{"HOME", "PI_CODING_AGENT_DIR", "TMPDIR", "PATH"}, nil)
	requireStoreOK(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	provider := &realOMPAutoNameProvider{ctx: ctx, plans: make(chan *realOMPAutoNamePlan, 8), errors: make(chan string, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", provider.serve)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	model := func(id string, reasoning bool) string {
		return fmt.Sprintf(`{"id":%q,"name":%q,"reasoning":%t,"input":["text"],"contextWindow":128000,"maxTokens":4096,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0}}`, id, id, reasoning)
	}
	models := fmt.Sprintf(`{"providers":{"openai":{"baseUrl":%q,"apiKey":"local-smoke-placeholder","api":"openai-completions","models":[%s,%s]}}}`, server.URL+"/v1", model("gpt-5", true), model("smoke-title", false))
	requireStoreOK(t, os.WriteFile(filepath.Join(root, "agent", "models.yml"), []byte(models), 0600))
	nativeConfig := filepath.Join(root, "native.yml")
	requireStoreOK(t, os.WriteFile(nativeConfig, []byte(`{"modelRoles":{"default":"openai/gpt-5","tiny":"openai/smoke-title"},"title":{"icons":"boring","generator":"tiny","refreshOnReplan":false,"refreshAfterPrompts":0},"defaultThinkingLevel":"off","prewalk":{"enabled":false},"disabledProviders":["ollama","llama.cpp","lm-studio","apple"]}`), 0600))
	cfg := omp.Config{Binary: binary, CWD: filepath.Join(root, "home", "workspace"), Environment: environment,
		Args: []string{"--config", nativeConfig, "--session-dir", sessionDir,
			"--no-title", "--no-tools", "--no-lsp", "--no-extensions", "--no-skills", "--no-rules"}}
	var current *realOMPAutoNameClient
	// Reap every native process before the provider and temporary directories close.
	t.Cleanup(func() {
		if current != nil {
			current.close(t)
		}
		cancel()
	})
	start := func(resume string) {
		t.Helper()
		if current != nil {
			current.close(t)
			current = nil
		}
		startConfig := cfg
		startConfig.Resume = resume
		// --no-ui is RPC-only; passing it to ACP exits before initialization.
		startConfig.Args = append(append([]string(nil), cfg.Args...), "--no-ui")
		client, err := omp.Start(ctx, startConfig, slog.New(slog.NewTextHandler(io.Discard, nil)))
		requireStoreOK(t, err)
		current = &realOMPAutoNameClient{client: client, ctx: ctx, provider: provider, events: make(chan json.RawMessage, 256), drained: make(chan struct{}), sessionDir: sessionDir}
		go func(c *realOMPAutoNameClient) {
			defer close(c.drained)
			defer close(c.events)
			for event := range c.client.Events() {
				select {
				case c.events <- event:
				case <-ctx.Done():
					// Continue draining until Close reaps the native runtime.
				}
			}
		}(current)
	}
	newSession := func(marker string) realOMPAutoNameState {
		t.Helper()
		current.call(t, "new_session", nil)
		current.turn(t, marker)
		state := current.state(t)
		if state.Name != "" {
			t.Fatalf("RPC session titled itself before explicit rename: %q", state.Name)
		}
		return state
	}
	rename := func(marker, title string, held bool) *realOMPAutoNamePlan {
		t.Helper()
		plan := &realOMPAutoNamePlan{marker: marker, title: title, started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
		provider.plans <- plan
		if !held {
			close(plan.release)
		}
		ack := current.call(t, "prompt", map[string]any{"message": "/rename"})
		var result struct {
			AgentInvoked *bool `json:"agentInvoked"`
		}
		if json.Unmarshal(ack, &result) != nil || result.AgentInvoked == nil || *result.AgentInvoked {
			t.Fatal("native rename was not acknowledged as a local background command")
		}
		current.await(t, plan.started, "title provider admission")
		return plan
	}

	start("")
	current.turn(t, "autoname-alpha")
	alpha := current.state(t)
	if alpha.Name != "" {
		t.Fatal("native RPC unexpectedly enabled automatic title generation")
	}
	rename("autoname-alpha", "Alpha cache repair", false)
	current.named(t, alpha.ID, "Alpha cache repair")
	alpha = current.state(t)

	manual := newSession("autoname-manual")
	lateManual := rename("autoname-manual", "Unwanted late title", true)
	// Both this RPC and get_state must complete while the provider is still held.
	current.call(t, "set_session_name", map[string]any{"name": "Operator chosen name"})
	current.expectState(t, manual.ID, "Operator chosen name")
	close(lateManual.release)
	current.await(t, lateManual.finished, "late manual-race response")
	current.unchanged(t, manual.ID, "Operator chosen name")
	manual = current.state(t)

	stale := newSession("autoname-stale")
	lateSwitch := rename("autoname-stale", "Wrong session title", true)
	delta := newSession("autoname-delta")
	rename("autoname-delta", "Delta parser repair", false)
	current.named(t, delta.ID, "Delta parser repair")
	close(lateSwitch.release)
	current.await(t, lateSwitch.finished, "old-session title response")
	current.unchanged(t, delta.ID, "Delta parser repair")
	delta = current.state(t)

	current.call(t, "switch_session", map[string]any{"sessionPath": stale.File})
	current.expectState(t, stale.ID, "")
	lateAbort := rename("autoname-stale", "Interrupted title", true)
	current.call(t, "abort", nil)
	close(lateAbort.release)
	current.await(t, lateAbort.finished, "interrupted title response")
	current.unchanged(t, stale.ID, "")
	current.close(t)
	current = nil

	// Query OMP's native ACP catalog, not bridge-owned or copied title metadata.
	listed, err := omp.ListSessions(ctx, cfg)
	requireStoreOK(t, err)
	for _, want := range []realOMPAutoNameState{alpha, manual, delta} {
		found := false
		for _, session := range listed {
			if session.ID == want.ID {
				found = true
				if session.Title != want.Name || session.CWD != cfg.CWD {
					t.Fatalf("native session catalog mismatch for %s: title=%q cwd=%q", want.ID, session.Title, session.CWD)
				}
			}
		}
		if !found {
			t.Fatalf("native catalog omitted persisted session %s", want.ID)
		}
	}
	for _, want := range []realOMPAutoNameState{alpha, delta} {
		start(want.File)
		current.expectState(t, want.ID, want.Name)
		// The provider rejects a restored title attached to another session's history.
		marker := "autoname-alpha"
		if want.ID == delta.ID {
			marker = "autoname-delta"
		}
		current.turn(t, "verify-history "+marker)
		current.expectState(t, want.ID, want.Name)
	}
	current.checkProvider(t)
	t.Log("native rename: background ACK, manual/switch/abort isolation, persisted catalog titles and restored conversation identities")

	t.Run("cancelled-before-input-hook-admission", func(t *testing.T) {
		current.close(t)
		current = nil
		entered, release := make(chan struct{}), make(chan struct{})
		wire := make(chan string, 4)
		mux.HandleFunc("/admission", func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
			case <-ctx.Done():
			}
		})
		mux.HandleFunc("/wire", func(w http.ResponseWriter, r *http.Request) {
			select {
			case wire <- r.URL.Query().Get("command"):
			case <-r.Context().Done():
			}
		})
		extension := filepath.Join(root, "admission-gate.ts")
		// The input hook and continue result are available in OMP v18.18.1.
		source := fmt.Sprintf(`export default function (pi) {
	pi.on("input", async (event) => {
		if (event.text === "/rename") {
			const response = await fetch(%q);
			if (!response.ok) throw new Error("admission gate failed");
		}
		return { action: "continue" };
	});
}
`, server.URL+"/admission")
		requireStoreOK(t, os.WriteFile(extension, []byte(source), 0600))
		testBinary, err := os.Executable()
		requireStoreOK(t, err)
		t.Setenv("OMP_TELEGRAM_ADMISSION_BINARY", binary)
		t.Setenv("OMP_TELEGRAM_ADMISSION_WIRE", server.URL+"/wire")
		cfg.Environment, err = omp.NewEnvironment("allowlist", []string{"HOME", "PI_CODING_AGENT_DIR", "TMPDIR", "PATH", "OMP_TELEGRAM_ADMISSION_WIRE", "OMP_TELEGRAM_ADMISSION_BINARY"}, nil)
		requireStoreOK(t, err)
		cfg.Binary = testBinary
		cfg.Args = append(cfg.Args, "-e", extension)
		start("")
		current.turn(t, "autoname-manual")
		state := current.state(t)
		_, err = current.client.RefreshCommandCatalog(ctx)
		requireStoreOK(t, err)
		db, err := store.Open(filepath.Join(root, "bridge-data"))
		requireStoreOK(t, err)
		t.Cleanup(func() { requireStoreOK(t, db.Close()) })
		binding := store.Binding{Bot: 99, Chat: -10, Thread: 11, Workspace: cfg.CWD, Session: state.File, SessionID: state.ID, Generation: 1, Running: true, AutoSessionNamePending: true}
		requireStoreOK(t, db.Save(binding))
		oldTransport := http.DefaultTransport
		http.DefaultTransport = &fakeHTTP{}
		t.Cleanup(func() { http.DefaultTransport = oldTransport })
		w := testWorker(t, &worker{
			b:   testBridge(t, &Bridge{ctx: ctx, cfg: config.Config{AutoSessionName: true, DataDir: filepath.Join(root, "bridge-data")}, db: db, tg: newTestTelegram(t), bot: telegram.User{ID: 99}}),
			key: target{chat: -10, thread: 11}, ctx: ctx, cancel: cancel,
			client: current.client, runtime: runtimeConnected, binding: binding, sessionID: state.ID,
		})
		plan := &realOMPAutoNamePlan{marker: "autoname-manual", title: "Late admission must not win", started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
		t.Cleanup(func() {
			// Release gates and reap OMP before the HTTP server or directories close.
			select {
			case <-release:
			default:
				close(release)
			}
			select {
			case <-plan.release:
			default:
				close(plan.release)
			}
			current.close(t)
			current = nil
			w.background.Wait()
		})
		provider.plans <- plan
		conversations, titles := provider.conversations.Load(), provider.titles.Load()
		w.autoSessionNameReady = true
		w.dispatchAutoSessionName()
		check := waitOperation(t, w)
		if check.kind != "auto_session_name_state" {
			t.Fatalf("expected production automatic state check, got %s", check.kind)
		}
		w.operationReturned(check)
		current.await(t, entered, "extension input hook before rename admission")
		w.cancelAutoSessionName()
		cancelled := waitOperation(t, w)
		if cancelled.kind != "auto_session_name" || cancelled.err == nil {
			t.Fatal("automatic RPC did not cancel locally while native admission was held")
		}
		w.operationReturned(cancelled)
		w.beginSessionName("Operator survives delayed admission")
		var first string
		select {
		case first = <-wire:
		case <-ctx.Done():
			t.Fatal("manual naming did not submit a native request")
		}
		if first == "set_session_name" {
			// Preserve the old implementation's failing schedule without timing guesses:
			// its unguarded write finishes before the delayed rename is admitted.
			w.operationReturned(waitOperation(t, w))
		} else if first != "/session info" {
			t.Fatalf("unexpected first manual naming request: %q", first)
		}
		close(release)
		current.await(t, plan.started, "title inference after delayed admission")
		if first == "/session info" {
			result := waitOperation(t, w)
			if result.kind != "name" || result.err != nil {
				t.Fatalf("production manual naming failed after admission barrier: %s %v", result.kind, result.err)
			}
			w.operationReturned(result)
		}
		current.expectState(t, state.ID, "Operator survives delayed admission")
		close(plan.release)
		current.await(t, plan.finished, "delayed title response")
		current.unchanged(t, state.ID, "Operator survives delayed admission")
		if w.client != current.client || w.taskActive() || w.turn != 0 || provider.conversations.Load() != conversations || provider.titles.Load() != titles+1 {
			t.Fatal("cancelled automatic naming retired the runtime, replayed a turn, or made extra model calls")
		}
	})
}

// This subprocess only observes wire submissions; all admission and naming behavior
// is provided by the actual native OMP process and the production worker methods.
func runRealOMPAutoNameAdmissionProxy(binary string) {
	endpoint := os.Getenv("OMP_TELEGRAM_ADMISSION_WIRE")
	if endpoint == "" {
		os.Exit(2)
	}
	command := exec.Command(binary, os.Args[1:]...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	stdin, err := command.StdinPipe()
	if err != nil || command.Start() != nil {
		os.Exit(2)
	}
	go func() {
		defer stdin.Close()
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 4096), 16<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			var frame struct{ Type, Message string }
			if json.Unmarshal(line, &frame) == nil && (frame.Type == "set_session_name" || frame.Type == "prompt" && frame.Message == "/session info") {
				name := frame.Type
				if frame.Type == "prompt" {
					name = frame.Message
				}
				response, err := http.Post(endpoint+"?command="+strings.ReplaceAll(name, " ", "%20"), "text/plain", nil)
				if err != nil {
					return
				}
				response.Body.Close()
			}
			if _, err := fmt.Fprintf(stdin, "%s\n", line); err != nil {
				return
			}
		}
	}()
	if command.Wait() != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

type realOMPAutoNameState struct {
	ID   string `json:"sessionId"`
	File string `json:"sessionFile"`
	Name string `json:"sessionName"`
}

type realOMPAutoNameClient struct {
	client     *omp.Client
	ctx        context.Context
	provider   *realOMPAutoNameProvider
	events     chan json.RawMessage
	drained    chan struct{}
	sessionDir string
}

func (c *realOMPAutoNameClient) close(t *testing.T) {
	t.Helper()
	err := c.client.Close()
	<-c.drained
	if err != nil {
		t.Error(err)
	}
}

func (c *realOMPAutoNameClient) checkProvider(t *testing.T) {
	t.Helper()
	select {
	case reason := <-c.provider.errors:
		t.Fatal(reason)
	default:
	}
}

func (c *realOMPAutoNameClient) call(t *testing.T, command string, fields map[string]any) json.RawMessage {
	t.Helper()
	c.checkProvider(t)
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	raw, err := c.client.Call(ctx, command, fields)
	c.checkProvider(t)
	if err != nil {
		t.Fatalf("native %s RPC: %v", command, err)
	}
	return raw
}

func (c *realOMPAutoNameClient) await(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
		c.checkProvider(t)
	case reason := <-c.provider.errors:
		t.Fatal(reason)
	case <-c.client.Done():
		t.Fatalf("native process ended while waiting for %s", what)
	case <-c.ctx.Done():
		t.Fatalf("waiting for %s: %v", what, c.ctx.Err())
	}
}

func (c *realOMPAutoNameClient) event(t *testing.T, ready func(json.RawMessage) bool) {
	t.Helper()
	for {
		select {
		case raw, ok := <-c.events:
			if !ok {
				t.Fatal("native event stream ended before the expected result")
			}
			if ready(raw) {
				c.checkProvider(t)
				return
			}
		case reason := <-c.provider.errors:
			t.Fatal(reason)
		case <-c.ctx.Done():
			t.Fatalf("waiting for native event: %v", c.ctx.Err())
		}
	}
}

func (c *realOMPAutoNameClient) turn(t *testing.T, message string) {
	t.Helper()
	id := c.client.ReserveRequestID()
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	_, err := c.client.CallWithID(ctx, id, "prompt", map[string]any{"message": message})
	requireStoreOK(t, err)
	c.event(t, func(raw json.RawMessage) bool {
		var event struct {
			Type, ID, Status string
			AgentInvoked     bool `json:"agentInvoked"`
		}
		if json.Unmarshal(raw, &event) != nil || event.Type != "prompt_result" || event.ID != id {
			return false
		}
		if event.Status != "completed" || !event.AgentInvoked {
			t.Fatalf("native conversation did not complete: status=%s invoked=%t", event.Status, event.AgentInvoked)
		}
		return true
	})
}

func (c *realOMPAutoNameClient) state(t *testing.T) realOMPAutoNameState {
	t.Helper()
	var state realOMPAutoNameState
	requireStoreOK(t, json.Unmarshal(c.call(t, "get_state", nil), &state))
	if state.ID == "" || !filepath.IsAbs(state.File) || !strings.HasPrefix(state.File, c.sessionDir+string(os.PathSeparator)) {
		t.Fatal("native session identity escaped the isolated session directory")
	}
	return state
}

func (c *realOMPAutoNameClient) expectState(t *testing.T, id, title string) {
	t.Helper()
	state := c.state(t)
	if state.ID != id || state.Name != title {
		t.Fatalf("native state changed session/title: got %s %q, want %s %q", state.ID, state.Name, id, title)
	}
}

func (c *realOMPAutoNameClient) named(t *testing.T, id, title string) {
	t.Helper()
	c.event(t, func(raw json.RawMessage) bool {
		var event struct {
			Type, Title string
			SessionID   string `json:"sessionId"`
		}
		if json.Unmarshal(raw, &event) != nil || event.Type != "session_info_update" || event.SessionID != id {
			return false
		}
		if event.Title != title {
			t.Fatalf("native rename announced wrong session/title: %s %q", event.SessionID, event.Title)
		}
		return true
	})
	c.expectState(t, id, title)
}

// A completed HTTP response is not a title-persistence barrier. Observe a bounded
// quiet period through RPC before checking the same data after process restart.
func (c *realOMPAutoNameClient) unchanged(t *testing.T, id, title string) {
	t.Helper()
	quiet := time.NewTimer(300 * time.Millisecond)
	defer quiet.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		c.expectState(t, id, title)
		select {
		case <-quiet.C:
			return
		case <-tick.C:
		case <-c.ctx.Done():
			t.Fatal(c.ctx.Err())
		}
	}
}

type realOMPAutoNamePlan struct {
	marker, title              string
	started, release, finished chan struct{}
}

type realOMPAutoNameProvider struct {
	ctx           context.Context
	conversations atomic.Int64
	titles        atomic.Int64
	plans         chan *realOMPAutoNamePlan
	errors        chan string
}

func (p *realOMPAutoNameProvider) reject(w http.ResponseWriter, reason string) {
	select {
	case p.errors <- reason:
	default:
	}
	http.Error(w, "local auto-name contract rejected", http.StatusBadRequest)
}

func (p *realOMPAutoNameProvider) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-5","object":"model","created":1,"owned_by":"smoke"},{"id":"smoke-title","object":"model","created":1,"owned_by":"smoke"}]}`)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		p.reject(w, "unexpected auto-name provider endpoint")
		return
	}
	var request struct {
		Model    string
		Stream   bool
		Messages []struct {
			Role    string
			Content json.RawMessage
		}
	}
	if json.NewDecoder(r.Body).Decode(&request) != nil || !request.Stream {
		p.reject(w, "unexpected auto-name provider request schema/stream")
		return
	}
	var user, history strings.Builder
	for _, message := range request.Messages {
		text := realOMPContentText(message.Content)
		if message.Role == "user" {
			user.Reset()
			user.WriteString(text)
		}
		if message.Role == "user" || message.Role == "assistant" {
			history.WriteString(text)
			history.WriteByte('\n')
		}
	}
	answer := ""
	switch request.Model {
	case "gpt-5":
		p.conversations.Add(1)
		marker := ""
		for _, candidate := range []string{"autoname-alpha", "autoname-manual", "autoname-stale", "autoname-delta"} {
			if strings.Contains(user.String(), candidate) {
				marker = candidate
			}
		}
		if marker == "" {
			p.reject(w, "unexpected native conversation input")
			return
		}
		if strings.Contains(user.String(), "verify-history ") {
			if !strings.Contains(history.String(), "reply-for-"+marker) {
				p.reject(w, "resumed conversation lost its own assistant history")
				return
			}
			for _, other := range []string{"autoname-alpha", "autoname-manual", "autoname-stale", "autoname-delta"} {
				if other != marker && strings.Contains(history.String(), other) {
					p.reject(w, "resumed conversation loaded another session's history")
					return
				}
			}
		}
		answer = "reply-for-" + marker
	case "smoke-title":
		p.titles.Add(1)
		var plan *realOMPAutoNamePlan
		select {
		case plan = <-p.plans:
		default:
			p.reject(w, "unexpected or duplicate native title inference")
			return
		}
		defer close(plan.finished)
		if !strings.Contains(user.String(), plan.marker) || !strings.Contains(user.String(), "reply-for-"+plan.marker) {
			p.reject(w, "native rename omitted the real conversation or used another session")
			return
		}
		close(plan.started)
		select {
		case <-plan.release:
		case <-r.Context().Done():
			return
		case <-p.ctx.Done():
			return
		}
		answer = "<title>" + plan.title + "</title>"
	default:
		p.reject(w, "native auto-name requested an unconfigured model")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, choice := range []map[string]any{
		{"index": 0, "delta": map[string]any{"role": "assistant", "content": answer}, "finish_reason": nil},
		{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
	} {
		chunk, _ := json.Marshal(map[string]any{"id": "autoname-local", "object": "chat.completion.chunk", "created": 1, "model": request.Model, "choices": []any{choice}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		w.(http.Flusher).Flush()
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}
