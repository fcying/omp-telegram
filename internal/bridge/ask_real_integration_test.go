package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

// An actual native ask call must resume its original provider turn, not submit
// the Telegram answer as another user message or a steering instruction.
func TestRealOMPAskCompatibility(t *testing.T) {
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
	for _, scenario := range []string{"combined", "select-editor", "guidance", "stop"} {
		t.Run(scenario, func(t *testing.T) {
			fallback := scenario == "select-editor"
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
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			t.Cleanup(cancel)
			harness := &realOMPProvider{changed: make(chan struct{}, 1), errors: make(chan string, 1)}
			provider := &realOMPAskProvider{harness: harness, fallback: fallback, guidance: scenario == "guidance", ctx: ctx, continuePostTool: make(chan struct{})}
			server := httptest.NewServer(http.HandlerFunc(provider.serve))
			t.Cleanup(server.Close)
			models := fmt.Sprintf(`{"providers":{"openai":{"baseUrl":%q,"apiKey":"local-smoke-placeholder","api":"openai-completions","models":[{"id":"gpt-5","name":"Local Ask smoke model","reasoning":true,"input":["text"],"contextWindow":128000,"maxTokens":4096,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"compat":{"supportsReasoningEffort":true}}]}}}`, server.URL+"/v1")
			requireStoreOK(t, os.WriteFile(filepath.Join(root, "agent", "models.yml"), []byte(models), 0600))
			nativeConfig := filepath.Join(root, "native.yml")
			requireStoreOK(t, os.WriteFile(nativeConfig, []byte(`{"modelRoles":{"default":"openai/gpt-5"},"defaultThinkingLevel":"off","prewalk":{"enabled":false},"disabledProviders":["ollama","llama.cpp","lm-studio","apple"]}`), 0600))
			args := []string{"--config", nativeConfig, "--session-dir", filepath.Join(root, "sessions"),
				"--no-title", "--tools", "ask", "--no-lsp", "--no-extensions", "--no-skills", "--no-rules", "--no-ui"}
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
			until := func(ready func() bool) { realOMPUntil(t, w, harness, ready) }
			nativeInput(t, w, 1, "/new "+filepath.Join(root, "workspace"), false)
			until(func() bool { return w.nativeCatalog.cancel == nil })
			if w.client == nil {
				t.Fatal("native Ask runtime failed to start")
			}
			if fallback {
				nativeRPC(t, w, "set_ask_dialog", map[string]any{"enabled": false})
			}
			nativeInput(t, w, 100, "native ask smoke", false)
			until(func() bool {
				if fallback {
					for _, c := range w.confirms {
						if c.action == "ui" && c.method == "select" {
							return true
						}
					}
					return false
				}
				return w.ask != nil && w.ask.cancel == nil
			})
			client, turn, rootID := w.client, w.turn, w.rootRequestID
			if w.active != 100 || !w.busy || rootID == "" {
				t.Fatal("native Ask did not belong to the original root")
			}
			if scenario == "stop" {
				askTestCallback(w, 7, askTestButton(t, fake, "Custom answer"))
				until(func() bool { return w.ask != nil && w.ask.cancel == nil && w.ask.inputID != 0 })
				prompt := askTestPrompt(t, fake)
				askTestCallback(w, 7, askTestButton(t, fake, "Back"))
				until(func() bool { return w.ask != nil && w.ask.cancel == nil })
				stale := askTestButton(t, fake, "Programming")
				nativeInput(t, w, 200, "/followup must not replay", false)
				u := update(201, 11, "/stop")
				u.Message.ReplyToMessage = prompt
				raw, err := json.Marshal(u)
				requireStoreOK(t, err)
				requireStoreOK(t, db.Accept(201, raw))
				w.handle(incoming{id: 201, msg: u.Message})
				w.dispatch()
				until(func() bool { return w.client == nil && w.ask == nil && w.askSending == 0 && !w.controlInProgress() })
				askTestCallback(w, 7, stale)
				if queueState(t, w, 100) != "cancelled" || queueState(t, w, 200) != "cancelled" || harness.count() != 1 {
					t.Fatal("Stop replayed work or left the native Ask root unconfirmed")
				}
				t.Log("Stop replying to an expired Ask prompt retired the waiting runtime and cancelled its queued followup without another provider request")
				return
			}
			click := func(label string) {
				askTestCallback(w, 7, askTestButton(t, fake, label))
				until(func() bool { return w.ask == nil || w.ask.cancel == nil })
			}
			if fallback {
				click("Other (type your own)")
				until(func() bool { return w.ask != nil && w.ask.cancel == nil && w.ask.inputID != 0 })
			} else {
				click("Programming")
				if scenario == "guidance" {
					a, epoch, token, messageID := w.ask, w.ask.epoch, w.ask.token, w.ask.messageID
					nativeInput(t, w, 210, "ordinary unquoted guidance: include guidance-confirmed in the final answer", false)
					if w.ask != a || a.question != 1 || a.epoch != epoch || a.token != token || a.messageID != messageID ||
						len(a.answers) != 1 || strings.Join(a.answers[0].SelectedOptions, ",") != "Programming" || len(a.selected) != 0 ||
						w.client != client || w.turn != turn || w.rootRequestID != rootID || w.active != 100 || !w.busy ||
						queueState(t, w, 210) != "pending" || len(w.deferredSteers) != 1 || len(w.steers) != 0 || len(w.queue) != 0 || harness.count() != 1 {
						t.Fatal("ordinary guidance cancelled native Ask, changed its UI/partial answer, or escaped the original root")
					}
				}
				click("Search")
				click("Auth")
				click("Confirm selection")
				click("Custom answer")
				until(func() bool { return w.ask != nil && w.ask.cancel == nil && w.ask.inputID != 0 })
			}
			prompt := askTestPrompt(t, fake)
			u := update(200, 11, "  Elixir  ")
			u.Message.ReplyToMessage = prompt
			raw, err := json.Marshal(u)
			requireStoreOK(t, err)
			requireStoreOK(t, db.Accept(200, raw))
			w.handle(incoming{id: 200, msg: u.Message})
			w.dispatch()
			if scenario == "guidance" {
				until(func() bool {
					return queueState(t, w, 100) == "done" || len(w.deferredSteers) == 0 && len(w.steers) != 0 && !w.rpcOperationActive
				})
				close(provider.continuePostTool)
			}
			until(func() bool { return queueState(t, w, 100) == "done" && w.ask == nil && w.askSending == 0 })
			calls := harness.count()
			if w.client != client || w.turn != turn || queueState(t, w, 200) != "done" {
				t.Fatal("native Ask answer replaced the runtime/turn or was not consumed")
			}
			want := "ask-complete"
			if scenario == "guidance" {
				want = "guidance-confirmed"
				if calls != 2 && calls != 3 || queueState(t, w, 210) != "done" || len(w.queue) != 0 || len(w.deferredSteers) != 0 {
					t.Fatal("guidance did not complete exactly once in the original native Ask root")
				}
				if calls == 3 {
					want = "post-tool-intermediate\n\n" + want
				}
			} else if calls != 2 {
				t.Fatal("native Ask answer invoked an extra prompt")
			}
			if body := researchBody(t, w, 100); body != want {
				t.Fatalf("original native turn final reply = %q, want %q", body, want)
			}
			t.Log("native ask tool consumed the selected labels/custom reply and continued the original root with any deferred guidance")
		})
	}
}

type realOMPAskProvider struct {
	harness          *realOMPProvider
	fallback         bool
	guidance         bool
	ctx              context.Context
	continuePostTool chan struct{}
}

func (p *realOMPAskProvider) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-5","object":"model","created":1,"owned_by":"smoke"}]}`)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		p.harness.reject(w, "unexpected Ask provider endpoint")
		return
	}
	var request struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct{ Function struct{ Name string } } `json:"tools"`
	}
	if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != "gpt-5" || !request.Stream {
		p.harness.reject(w, "unexpected Ask provider request")
		return
	}
	users, hasAsk, toolResult := 0, false, ""
	for _, message := range request.Messages {
		text := realOMPContentText(message.Content)
		if message.Role == "user" {
			users++
			valid := users == 1 && strings.Contains(text, "native ask smoke")
			if p.guidance && users == 2 && toolResult != "" && strings.Contains(text, "ordinary unquoted guidance: include guidance-confirmed in the final answer") {
				valid = true
			}
			if !valid {
				p.harness.reject(w, "Telegram Ask answer became a prompt/steer or guidance arrived before the Ask tool result")
				return
			}
		}
		if message.Role == "tool" && message.ToolCallID == "call-native-ask" {
			toolResult = text
		}
	}
	for _, tool := range request.Tools {
		hasAsk = hasAsk || tool.Function.Name == "ask"
	}
	p.harness.mu.Lock()
	p.harness.calls++
	stage := p.harness.calls
	p.harness.mu.Unlock()
	maxCalls := 2
	if p.guidance {
		maxCalls = 3
	}
	if !hasAsk || users < 1 || users > 1 && !p.guidance || stage > maxCalls ||
		stage == 1 && (users != 1 || toolResult != "") ||
		stage > 1 && (!strings.Contains(toolResult, "Elixir") || strings.Contains(strings.ToLower(toolResult), "cancel") || strings.Contains(strings.ToLower(toolResult), "timed out")) {
		p.harness.reject(w, "native ask tool/history/result missing, cancelled, or model work duplicated")
		return
	}
	if stage > 1 && !p.fallback {
		for _, label := range []string{"Programming", "Auth", "Search"} {
			if !strings.Contains(toolResult, label) {
				p.harness.reject(w, "native combined ask result lost a selected label")
				return
			}
		}
	}
	if p.guidance && stage > 1 && users == 1 {
		if stage != 2 {
			p.harness.reject(w, "native post-tool request repeated without deferred guidance")
			return
		}
		// Native steering can wait for the next model-turn boundary, rather
		// than cancelling an already running provider request.
		select {
		case <-p.continuePostTool:
		case <-r.Context().Done():
			return
		case <-p.ctx.Done():
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(delta map[string]any, finish any) {
		chunk := map[string]any{"id": "ask-smoke-" + strconv.Itoa(stage), "object": "chat.completion.chunk", "created": 1, "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
	}
	select {
	case p.harness.changed <- struct{}{}:
	default:
	}
	if stage == 1 {
		questions := []map[string]any{askTestQuestion("language", "Which language?", false, "Go", "Rust")}
		if !p.fallback {
			questions = []map[string]any{
				askTestQuestion("topic", "Which topic?", false, "Programming", "Investing", "Languages"),
				askTestQuestion("features", "Which features?", true, "Auth", "Billing", "Search"),
				askTestQuestion("language", "Which language?", false, "Go", "Rust"),
			}
		}
		arguments, _ := json.Marshal(map[string]any{"questions": questions})
		emit(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-native-ask", "type": "function",
			"function": map[string]any{"name": "ask", "arguments": string(arguments)}}}}, nil)
		emit(map[string]any{}, "tool_calls")
	} else {
		text := "ask-complete"
		if p.guidance {
			if users == 1 {
				text = "post-tool-intermediate"
			} else {
				text = "guidance-confirmed"
			}
		}
		emit(map[string]any{"content": text}, nil)
		emit(map[string]any{}, "stop")
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}
