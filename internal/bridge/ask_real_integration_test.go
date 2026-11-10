package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	for _, scenario := range []string{"fields", "guidance", "stop"} {
		t.Run(scenario, func(t *testing.T) {
			root, err := filepath.Abs(t.TempDir())
			requireStoreOK(t, err)
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
			provider := &realOMPAskProvider{harness: harness, guidance: scenario == "guidance", ctx: ctx, continuePostTool: make(chan struct{})}
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
			nativeInput(t, w, 100, "native ask smoke", false)
			until(func() bool { return w.rootRequestID != "" && w.busy })
			client, turn, rootID := w.client, w.turn, w.rootRequestID
			assertRoot := func() {
				if w.client != client || w.turn != turn || w.rootRequestID != rootID || rootID == "" || w.active != 100 || !w.busy || harness.count() != 1 {
					t.Fatal("native Ask escaped the original root or invoked extra model work while collecting fields")
				}
			}
			selectField := func(title, previousID string) confirmation {
				var current confirmation
				until(func() bool {
					for _, c := range w.confirms {
						if c.action == "ui" && c.method == "select" && c.uiID != previousID && strings.Contains(c.uiTitle, title) {
							current = c
							return true
						}
					}
					return false
				})
				assertRoot()
				return current
			}
			inputField := func(method, title string) *telegram.Message {
				until(func() bool {
					return w.ask != nil && w.ask.method == method && w.ask.cancel == nil && w.ask.inputID != 0 && strings.Contains(w.ask.questions[0].Question, title)
				})
				assertRoot()
				return askTestPrompt(t, fake)
			}
			// Capture the actual delivered callback envelope before its token expires.
			button := func(label string) *telegram.CallbackQuery {
				data := askTestButton(t, fake, label)
				token, _, _ := strings.Cut(data, ":")
				c, ok := w.confirms[token]
				if !ok {
					t.Fatal("Telegram field button has no live interaction")
				}
				return &telegram.CallbackQuery{ID: "real-ask-" + token, From: telegram.User{ID: 7}, Data: data,
					Message: &telegram.Message{MessageID: c.messageID, Chat: telegram.Chat{ID: w.key.chat}, MessageThreadID: w.key.thread}}
			}
			reply := func(id int64, text string, prompt *telegram.Message) {
				u := update(id, 11, text)
				u.Message.ReplyToMessage = prompt
				raw, err := json.Marshal(u)
				requireStoreOK(t, err)
				requireStoreOK(t, db.Accept(id, raw))
				w.handle(incoming{id: id, msg: u.Message})
				w.dispatch()
			}
			topic := selectField("Which topic?", "")
			w.callback(button("Programming"))
			features := selectField("Which features?", topic.uiID)
			if scenario == "guidance" {
				stale := button("Auth")
				token, _, _ := strings.Cut(stale.Data, ":")
				nativeInput(t, w, 210, "ordinary unquoted guidance: include guidance-confirmed in the final answer", false)
				assertRoot()
				if w.ask != nil || !reflect.DeepEqual(w.confirms[token], features) || queueState(t, w, 210) != "pending" ||
					len(w.deferredSteers) != 1 || len(w.steers) != 0 || len(w.queue) != 0 {
					t.Fatal("ordinary guidance changed the current field or escaped the original native Ask root")
				}
			}
			if scenario == "stop" {
				stale := button("Auth")
				w.callback(button("Other (type your own)"))
				prompt := inputField("input", "Which features?")
				w.callback(button("Cancel"))
				selectField("Which features?", features.uiID)
				current := button("Search")
				nativeInput(t, w, 300, "/followup must not replay", false)
				if queueState(t, w, 300) != "pending" || len(w.queue) != 1 {
					t.Fatal("Stop scenario did not queue its followup behind the waiting Ask")
				}
				reply(301, "/stop", prompt)
				until(func() bool { return w.client == nil && w.ask == nil && w.askSending == 0 && !w.controlInProgress() })
				w.callback(stale)
				w.callback(current)
				reply(302, "late field answer must not replay", prompt)
				if queueState(t, w, 100) != "cancelled" || queueState(t, w, 300) != "cancelled" ||
					queueState(t, w, 301) != "done" || queueState(t, w, 302) != "cancelled" || harness.count() != 1 ||
					w.client != nil || w.ask != nil || len(w.queue) != 0 || len(w.deferredSteers) != 0 {
					t.Fatal("Stop or late field interactions replayed work or left the native Ask root/followup unconfirmed")
				}
				t.Log("Stop replying to an expired field prompt retired the waiting runtime; stale/current callbacks and a late reply could not replay its root or queued followup")
				return
			}
			// Checkbox rows are separate native select calls, not a bridge-owned form.
			w.callback(button("Search"))
			features = selectField("Which features?", features.uiID)
			w.callback(button("Auth"))
			features = selectField("Which features?", features.uiID)
			w.callback(button("Done selecting"))
			selectField("Which language?", features.uiID)
			w.callback(button("Other (type your own)"))
			reply(200, "  Elixir  ", inputField("input", "Which language?"))
			rationale := inputField("input", "Why this choice?")
			text, _ := pickerView(t, fake)
			if !strings.Contains(text+rationale.Text, "Explain briefly") {
				t.Fatal("native input placeholder was not shown to the user")
			}
			reply(201, "  Keep the original root  ", rationale)
			reply(202, "  First line\nSecond line  ", inputField("editor", "Implementation notes?"))
			inputField("input", "Optional note?")
			w.callback(button("Submit empty"))
			saved := inputField("editor", "Saved draft?")
			text, _ = pickerView(t, fake)
			if !strings.Contains(text+saved.Text, "Saved draft\nSecond line") {
				t.Fatal("native editor prefill was not shown to the user")
			}
			w.callback(button("Use default"))
			inputField("editor", "Empty notes?")
			w.callback(button("Submit empty"))
			inputField("editor", "How many?")
			oldQuantity, oldDefault := w.ask.id, button("Use default")
			w.callback(button("Submit empty"))
			inputField("editor", "How many?")
			if w.ask.id == oldQuantity {
				t.Fatal("required empty value did not reach native validation and re-open the field")
			}
			quantity := w.ask
			w.callback(oldDefault)
			if w.ask != quantity {
				t.Fatal("a stale default button answered the replacement field")
			}
			reply(203, "10", askTestPrompt(t, fake))
			inputField("editor", "How many?")
			if w.ask.id == quantity.id {
				t.Fatal("out-of-range value did not reach native validation")
			}
			reply(204, " 4 ", askTestPrompt(t, fake))
			code := inputField("input", "Reference code?")
			codeID := w.ask.id
			reply(205, " OK ", code)
			inputField("input", "Reference code?")
			if w.ask.id == codeID {
				t.Fatal("pattern validation did not see the exact untrimmed answer")
			}
			reply(206, "OK", askTestPrompt(t, fake))
			if scenario == "guidance" {
				until(func() bool {
					return queueState(t, w, 100) == "done" || len(w.deferredSteers) == 0 && len(w.steers) != 0 && !w.rpcOperationActive
				})
				close(provider.continuePostTool)
			}
			until(func() bool { return queueState(t, w, 100) == "done" && w.ask == nil && w.askSending == 0 })
			calls := harness.count()
			if w.client != client || w.turn != turn || queueState(t, w, 200) != "done" || queueState(t, w, 201) != "done" || queueState(t, w, 202) != "done" || queueState(t, w, 203) != "done" || queueState(t, w, 204) != "done" || queueState(t, w, 205) != "done" || queueState(t, w, 206) != "done" {
				t.Fatal("native Ask field answers replaced the runtime/turn or were not consumed")
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
				t.Fatal("native Ask field answers invoked an extra prompt")
			}
			if body := researchBody(t, w, 100); body != want {
				t.Fatalf("original native turn final reply = %q, want %q", body, want)
			}
			t.Log("native HTML ask consumed named one-of/many-of/custom/input/editor fields as JSON and continued the original root with any deferred guidance")
		})
	}
}

type realOMPAskProvider struct {
	harness          *realOMPProvider
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
		Tools []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != "gpt-5" || !request.Stream {
		p.harness.reject(w, "unexpected Ask provider request")
		return
	}
	users, hasAsk, toolResult, toolResults := 0, false, "", 0
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
			toolResults++
			toolResult = text
		}
	}
	for _, tool := range request.Tools {
		if tool.Function.Name != "ask" {
			continue
		}
		var schema struct {
			Properties map[string]struct{ Type string } `json:"properties"`
			Required   []string                         `json:"required"`
		}
		schemaErr := json.Unmarshal(tool.Function.Parameters, &schema)
		requiresHTML := slices.Contains(schema.Required, "html")
		_, legacyQuestions := schema.Properties["questions"]
		if schemaErr != nil || schema.Properties["html"].Type != "string" || !requiresHTML || legacyQuestions {
			p.harness.reject(w, "native ask schema must require an HTML form instead of questions[]")
			return
		}
		hasAsk = true
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
		stage == 1 && (users != 1 || toolResults != 0) || stage > 1 && toolResults != 1 {
		p.harness.reject(w, "native ask tool/history/result missing or model work duplicated")
		return
	}
	if stage > 1 {
		// The native tool owns HTML parsing and serialization. Verify exact
		// named values, including option values distinct from their labels.
		payloadStart := strings.IndexByte(toolResult, '{')
		if payloadStart < 0 {
			p.harness.reject(w, "native HTML ask result did not contain JSON answers")
			return
		}
		var answers map[string]any
		want := map[string]any{
			"topic": "topic-programming", "features": []any{"auth-enabled", "full-text"},
			"language": "Elixir", "rationale": "  Keep the original root  ", "notes": "  First line\nSecond line  ",
			"optional": "", "saved": "  Saved draft\nSecond line  ", "emptyNotes": "", "quantity": float64(4), "code": "OK",
		}
		answerErr := json.Unmarshal([]byte(toolResult[payloadStart:]), &answers)
		if answerErr != nil || !reflect.DeepEqual(answers, want) {
			p.harness.reject(w, "native HTML ask result must contain exactly the submitted named field values as JSON")
			return
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
		html := `<form>
			<fieldset><legend>Which topic?</legend>
				<label><input type="radio" name="topic" value="topic-programming">Programming</label>
				<label><input type="radio" name="topic" value="topic-investing">Investing</label>
				<label><input type="radio" name="topic" value="topic-languages">Languages</label>
			</fieldset>
			<fieldset><legend>Which features?</legend>
				<label><input type="checkbox" name="features" value="auth-enabled">Auth</label>
				<label><input type="checkbox" name="features" value="billing-enabled">Billing</label>
				<label><input type="checkbox" name="features" value="full-text">Search</label>
			</fieldset>
			<fieldset><legend>Which language?</legend>
				<label><input type="radio" name="language" value="go">Go</label>
				<label><input type="radio" name="language" value="rust">Rust</label>
			</fieldset>
			<label>Why this choice?<input name="rationale" placeholder="Explain briefly" required></label>
			<label>Implementation notes?<textarea name="notes" required>Replace this draft</textarea></label>
			<label>Optional note?<input name="optional" placeholder="This can be blank"></label>
			<label>Saved draft?<textarea name="saved">  Saved draft
Second line  </textarea></label>
			<label>Empty notes?<textarea name="emptyNotes"></textarea></label>
			<label>How many?<input type="number" name="quantity" value="3" min="1" max="9" required></label>
			<label>Reference code?<input name="code" pattern="[A-Z]{2}" required></label>
		</form>`
		arguments, _ := json.Marshal(map[string]any{"i": "Collecting smoke-test choices", "html": html})
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
