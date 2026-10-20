package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func TestRealOMPBridgeCompatibility(t *testing.T) {
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
	root := t.TempDir()
	for _, name := range []string{"home", "agent", "workspace", "sessions", "data", "tmp"} {
		requireStoreOK(t, os.MkdirAll(filepath.Join(root, name), 0700))
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(root, "agent"))
	t.Setenv("TMPDIR", filepath.Join(root, "tmp"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	environment, err := omp.NewEnvironment("allowlist", []string{"HOME", "PI_CODING_AGENT_DIR", "TMPDIR", "PATH"}, nil)
	requireStoreOK(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	version := exec.CommandContext(ctx, binary, "--session-dir", filepath.Join(root, "sessions"), "--version")
	version.Dir, version.Env = filepath.Join(root, "workspace"), omp.ChildEnv(environment)
	output, err := version.CombinedOutput()
	requireStoreOK(t, err)
	actualVersion := regexp.MustCompile(`\b[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?\b`).FindString(string(output))
	if actualVersion == "" {
		t.Fatal("OMP did not report a version")
	}
	t.Logf("native OMP version: %s", actualVersion)

	provider := &realOMPProvider{changed: make(chan struct{}, 1), errors: make(chan string, 1), root: make(chan struct{}), tool: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(provider.serve))
	t.Cleanup(server.Close)
	models := fmt.Sprintf(`{"providers":{"openai":{"baseUrl":%q,"apiKey":"local-smoke-placeholder","api":"openai-completions","models":[{"id":"gpt-5","name":"Local smoke model","reasoning":true,"input":["text"],"contextWindow":128000,"maxTokens":4096,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"compat":{"supportsReasoningEffort":true}}]}}}`, server.URL+"/v1")
	requireStoreOK(t, os.WriteFile(filepath.Join(root, "agent", "models.yml"), []byte(models), 0600))
	nativeConfig := filepath.Join(root, "native.yml")
	requireStoreOK(t, os.WriteFile(nativeConfig, []byte(`{"modelRoles":{"default":"openai/gpt-5"},"defaultThinkingLevel":"off","prewalk":{"enabled":false},"disabledProviders":["ollama","llama.cpp","lm-studio","apple"]}`), 0600))
	args := []string{"--config", nativeConfig, "--session-dir", filepath.Join(root, "sessions"),
		"--no-title", "--no-tools", "--no-lsp", "--no-extensions", "--no-skills", "--no-rules", "--no-ui"}
	db, err := store.Open(filepath.Join(root, "data"))
	requireStoreOK(t, err)
	oldTransport := http.DefaultTransport
	fake := &fakeHTTP{}
	http.DefaultTransport = fake
	w := testWorker(t, &worker{
		b: testBridge(t, &Bridge{ctx: ctx, cfg: config.Config{
			OMP: binary, OMPArgs: [][]string{args}, OMPEnvironment: environment,
			DataDir: filepath.Join(root, "data"), WorkspaceRoot: filepath.Join(root, "workspace"), QueueCapacity: 4, ProgressMode: "off",
		}, db: db, tg: newTestTelegram(t), bot: telegram.User{ID: 99}, slots: make(chan struct{}, 1)}),
		key: target{chat: -10, thread: 11}, ctx: ctx, cancel: cancel, confirms: make(map[string]confirmation),
	})
	t.Cleanup(func() {
		w.teardownWorker(true)
		cancel()
		w.background.Wait()
		w.drainMediaResults()
		http.DefaultTransport = oldTransport
		requireStoreOK(t, db.Close())
	})
	input := func(id int64, text string) { nativeInput(t, w, id, text, false) }
	until := func(ready func() bool) { realOMPUntil(t, w, provider, ready) }
	input(1, "/new "+filepath.Join(root, "workspace"))
	until(func() bool { return w.nativeCatalog.cancel == nil })
	if w.client == nil || !w.client.CommandCatalog().HasBuiltinCommand("fast") {
		t.Fatal("native startup/catalog did not expose builtin fast")
	}
	binding, client := w.binding, w.client

	input(100, "smoke root")
	until(func() bool { return provider.count() == 1 && !w.rpcOperationActive })
	if w.active != 100 || !w.busy || queueState(t, w, 100) != "submitted" {
		t.Fatal("prompt acceptance was confused with task completion")
	}
	rootID, turn, owner := w.rootRequestID, w.turn, w.owner
	input(101, "/followup smoke followup")
	for i, command := range []string{"/fast", "/fast status", "/fast off", "/fast on"} {
		id := int64(110 + i)
		input(id, command)
		until(func() bool { return queueState(t, w, id) == "done" && !w.controlInProgress() })
		if w.client != client || w.rootRequestID != rootID || w.turn != turn || w.owner != owner || w.active != 100 || !w.busy {
			t.Fatal("native fast changed the active root's ownership")
		}
		if len(w.queue) != 1 || queueState(t, w, 101) != "pending" || provider.count() != 1 {
			t.Fatal("native fast invoked the provider or dispatched the queued followup")
		}
	}
	close(provider.root)
	until(func() bool { return queueState(t, w, 101) == "done" })
	for id, want := range map[int64]string{100: "root-complete", 101: "followup-complete"} {
		if queueState(t, w, id) != "done" || replyBody(strings.Join(researchReplies(t, w, id), "")) != want {
			t.Fatal("native control polluted or misattributed a final reply")
		}
	}
	if provider.count() != 2 || provider.followupTier() != "priority" {
		t.Fatal("native fast was not reflected in the next real provider request")
	}
	t.Log("streaming fast: root/queue preserved, two provider requests, followup service_tier=priority")

	artifact := filepath.Join(root, "workspace", "artifact.txt")
	requireStoreOK(t, os.WriteFile(artifact, []byte("smoke artifact\n"), 0600))
	input(200, "smoke attachment")
	until(func() bool { return provider.count() == 4 })
	var snapshot, state string
	var chat, thread int64
	requireStoreOK(t, db.DB.QueryRow("SELECT path,state,chat,thread FROM outbox WHERE kind='document'").Scan(&snapshot, &state, &chat, &thread))
	if state != "pending" || chat != -10 || thread != 11 || queueState(t, w, 200) != "submitted" {
		t.Fatal("host tool did not queue the attachment independently of task completion/delivery")
	}
	requireStoreOK(t, os.WriteFile(artifact, []byte("changed after enqueue\n"), 0600))
	contents, err := os.ReadFile(snapshot)
	requireStoreOK(t, err)
	if string(contents) != "smoke artifact\n" {
		t.Fatal("queued attachment was not an immutable workspace snapshot")
	}
	close(provider.tool)
	until(func() bool { return queueState(t, w, 200) == "done" })
	if replyBody(strings.Join(researchReplies(t, w, 200), "")) != "attachment-complete" {
		t.Fatal("native host tool result did not complete the original task")
	}
	t.Log("host tool: real OMP call/result round trip, immutable attachment pending for original topic, no delivery claim")

	input(300, "smoke stop")
	until(func() bool { return provider.count() == 5 && !w.rpcOperationActive })
	stoppedClient := w.client
	input(301, "/followup smoke must not replay")
	input(302, "/stop")
	until(func() bool { return w.client == nil && !w.taskActive() && !w.rpcOperationActive })
	if queueState(t, w, 300) != "cancelled" || queueState(t, w, 301) != "cancelled" {
		t.Fatal("stop did not cancel active and queued work")
	}
	input(303, "smoke resumed")
	until(func() bool { return queueState(t, w, 303) == "done" })
	if w.client == stoppedClient || !sameBindingIdentity(binding, w.binding) || provider.count() != 6 {
		t.Fatal("stop/resume changed session identity or replayed old work")
	}
	if replyBody(strings.Join(researchReplies(t, w, 303), "")) != "resumed-complete" {
		t.Fatal("resumed session did not produce its own final reply")
	}
	t.Log("stop/resume: old runtime retired, queued work cancelled, saved native context resumed without replay")
}

// Only the real provider's selected contract metadata is retained, never raw requests.
type realOMPProvider struct {
	mu      sync.Mutex
	calls   int
	tier    string
	changed chan struct{}
	errors  chan string
	root    chan struct{}
	tool    chan struct{}
}

func (p *realOMPProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *realOMPProvider) followupTier() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tier
}

func (p *realOMPProvider) reject(w http.ResponseWriter, reason string) {
	select {
	case p.errors <- reason:
	default:
	}
	http.Error(w, "local smoke contract rejected", http.StatusBadRequest)
}

func (p *realOMPProvider) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-5","object":"model","created":1,"owned_by":"smoke"}]}`)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		p.reject(w, fmt.Sprintf("unexpected local provider endpoint: %s %s", r.Method, r.URL.Path))
		return
	}
	var request struct {
		Model       string `json:"model"`
		Stream      bool   `json:"stream"`
		ServiceTier string `json:"service_tier"`
		Messages    []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct{ Function struct{ Name string } } `json:"tools"`
	}
	if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != "gpt-5" || !request.Stream {
		p.reject(w, "unexpected provider request schema/model/stream")
		return
	}
	var user string
	hasRoot, hasRootAnswer, hasToolResult, hasHostTool := false, false, false, false
	for _, message := range request.Messages {
		text := realOMPContentText(message.Content)
		if message.Role == "user" {
			user = text
			hasRoot = hasRoot || strings.Contains(text, "smoke root")
		}
		if message.Role == "assistant" && text == "root-complete" {
			hasRootAnswer = true
		}
		if message.Role == "tool" && message.ToolCallID == "call-smoke" {
			hasToolResult = true
		}
	}
	for _, tool := range request.Tools {
		hasHostTool = hasHostTool || tool.Function.Name == "telegram_send"
	}
	p.mu.Lock()
	p.calls++
	stage := p.calls
	if stage == 2 {
		p.tier = request.ServiceTier
	}
	p.mu.Unlock()
	expected := map[int]string{1: "smoke root", 2: "smoke followup", 3: "smoke attachment", 4: "smoke attachment", 5: "smoke stop", 6: "smoke resumed"}
	if expected[stage] == "" || !strings.Contains(user, expected[stage]) {
		p.reject(w, "provider work was duplicated, lost, or reordered")
		return
	}
	if !hasHostTool || stage == 4 && !hasToolResult || stage == 6 && (!hasRoot || !hasRootAnswer) {
		p.reject(w, "host tool contract or resumed native history is missing")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(delta map[string]any, finish any) {
		chunk := map[string]any{"id": "smoke-" + strconv.Itoa(stage), "object": "chat.completion.chunk", "created": 1, "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
	}
	select {
	case p.changed <- struct{}{}:
	default:
	}
	switch stage {
	case 1:
		emit(map[string]any{"content": "root-"}, nil)
		select {
		case <-p.root:
		case <-r.Context().Done():
			return
		}
		emit(map[string]any{"content": "complete"}, nil)
	case 2:
		emit(map[string]any{"content": "followup-complete"}, nil)
	case 3:
		emit(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-smoke", "type": "function",
			"function": map[string]any{"name": "telegram_send", "arguments": `{"path":"artifact.txt","kind":"document","caption":"smoke artifact"}`}}}}, nil)
		emit(map[string]any{}, "tool_calls")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		return
	case 4:
		emit(map[string]any{"content": "attachment-"}, nil)
		select {
		case <-p.tool:
		case <-r.Context().Done():
			return
		}
		emit(map[string]any{"content": "complete"}, nil)
	case 5:
		emit(map[string]any{"content": "unfinished-"}, nil)
		<-r.Context().Done()
		return
	case 6:
		emit(map[string]any{"content": "resumed-complete"}, nil)
	}
	emit(map[string]any{}, "stop")
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func realOMPContentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct{ Text string }
	_ = json.Unmarshal(raw, &parts)
	var result strings.Builder
	for _, part := range parts {
		result.WriteString(part.Text)
	}
	return result.String()
}

func realOMPUntil(t *testing.T, w *worker, provider *realOMPProvider, ready func() bool) {
	t.Helper()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for !ready() || w.replyStatus.pending != nil {
		var events <-chan json.RawMessage
		if w.client != nil {
			events = w.client.Events()
		}
		select {
		case reason := <-provider.errors:
			t.Fatal(reason)
		case <-provider.changed:
		case result := <-w.nativeCatalog.results:
			w.commandCatalogFinished(result)
		case result := <-w.operations:
			w.operationReturned(result)
		case result := <-w.sendResults:
			w.preparedSend(result)
		case event, ok := <-events:
			if !ok {
				t.Fatal("native RPC stream closed before contract completion")
			}
			w.event(event)
		case <-w.ctx.Done():
			t.Fatal("real OMP smoke context ended")
		case <-timer.C:
			t.Fatalf("real OMP contract timed out: provider_calls=%d active=%d rpc_active=%t", provider.count(), w.active, w.rpcOperationActive)
		}
		w.dispatch()
	}
}
