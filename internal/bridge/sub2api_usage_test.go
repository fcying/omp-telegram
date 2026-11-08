package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"omp-telegram/internal/config"
)

const sub2apiTestRequest1 = "11111111-1111-4111-8111-111111111111"
const sub2apiTestRequest2 = "22222222-2222-4222-8222-222222222222"
const sub2apiTestRequest3 = "33333333-3333-4333-8333-333333333333"

func sub2apiTestWorker(t *testing.T, gate <-chan struct{}, started chan<- struct{}) (*worker, func(string), string, *atomic.Int32) {
	t.Helper()
	var quotaCalls atomic.Int32
	var signaled sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Header.Get("x-api-key") != "PRIVATE_SUB2API_KEY" {
			rw.WriteHeader(http.StatusForbidden)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		var data any
		switch req.URL.Path {
		case "/api/v1/admin/usage":
			id := req.URL.Query().Get("request_id")
			account := int64(2)
			if id == "client:"+sub2apiTestRequest2 {
				account = 3
			}
			if id != "client:"+sub2apiTestRequest1 && id != "client:"+sub2apiTestRequest2 && id != "client:"+sub2apiTestRequest3 {
				data = map[string]any{"total": 0, "items": []any{}, "page": 1}
			} else {
				data = map[string]any{"total": 1, "items": []any{map[string]any{"request_id": id, "account_id": account}}, "page": 1}
			}
		case "/api/v1/admin/openai/accounts/2/quota", "/api/v1/admin/openai/accounts/3/quota":
			quotaCalls.Add(1)
			if started != nil {
				signaled.Do(func() { started <- struct{}{} })
			}
			if gate != nil {
				select {
				case <-gate:
				case <-req.Context().Done():
					return
				}
			}
			if strings.Contains(req.URL.Path, "/3/") {
				data = map[string]any{"plan_type": "prolite", "rate_limit": map[string]any{"primary_window": map[string]any{"limit_window_seconds": 604800, "used_percent": 100}}}
			} else {
				data = map[string]any{"plan_type": "plus", "rate_limit": map[string]any{"primary_window": map[string]any{"limit_window_seconds": 18000, "used_percent": 1}, "secondary_window": map[string]any{"limit_window_seconds": 604800, "used_percent": 16}}}
			}
		default:
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"code": 0, "data": data})
	}))
	t.Cleanup(server.Close)
	realTransport := http.DefaultTransport
	w, command, trace := replyUsageWorker(t, "normal")
	fakeTransport := http.DefaultTransport
	http.DefaultTransport = logTestTransport(func(req *http.Request) (*http.Response, error) {
		if strings.HasPrefix(req.URL.String(), server.URL+"/") {
			return realTransport.RoundTrip(req)
		}
		return fakeTransport.RoundTrip(req)
	})
	t.Cleanup(func() {
		w.cancel()
		w.background.Wait()
		http.DefaultTransport = fakeTransport
	})
	w.b.cfg.Sub2API = config.Sub2API{BaseURL: server.URL, APIKey: "PRIVATE_SUB2API_KEY", Providers: []string{"openai-codex"}}
	return w, command, trace, &quotaCalls
}

func sub2apiTestNotify(t *testing.T, w *worker, provider, session, id string) {
	t.Helper()
	metadata, err := json.Marshal(map[string]string{"sessionId": session, "provider": provider, "clientRequestId": id})
	requireStoreOK(t, err)
	raw, err := json.Marshal(map[string]any{"type": "extension_ui_request", "method": "notify", "message": "omp-telegram.sub2api.v1 " + string(metadata)})
	requireStoreOK(t, err)
	w.event(raw)
}

func finishSub2apiTask(t *testing.T, w *worker) {
	t.Helper()
	endUsageReply(w)
	awaitReplyStatus(t, w)
	if w.taskActive() {
		t.Fatal("terminal reply did not settle the task")
	}
	w.refreshProviderUsage(time.Now())
}

func awaitSub2apiUsage(t *testing.T, w *worker) {
	t.Helper()
	awaitSub2apiUsageAt(t, w, time.Now())
}

func awaitSub2apiUsageAt(t *testing.T, w *worker, now time.Time) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for w.sub2api.pending != nil {
		select {
		case result := <-w.sub2api.results:
			w.sub2apiUsageFinished(result, now)
		case result := <-w.operations:
			w.operationReturned(result)
		case <-timer.C:
			t.Fatal("bounded sub2api read did not finish")
		}
	}
}

func TestSub2apiFinalHeaderBackfillDoesNotBlockTask(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(gate) }) })
	w, _, trace, _ := sub2apiTestWorker(t, gate, started)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	finishSub2apiTask(t, w)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("quota read not started")
	}
	if queueState(t, w, 2) != "done" {
		t.Fatal("HTTP quota read blocked task settlement")
	}
	// A healthy upstream response may take longer than the native RPC budget.
	time.Sleep(3500 * time.Millisecond)
	release.Do(func() { close(gate) })
	awaitSub2apiUsage(t, w)
	header := finalReplyStatus(t, w, 2, "answer")
	if !strings.Contains(header, "Plus 1% 16%") {
		t.Fatalf("actual account quota missing from final header: %q", header)
	}
	if researchTraceCount(trace, "prompt", "/usage") != 0 {
		t.Fatal("mapped provider fell back to native OAuth usage")
	}
	var leaked int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE text LIKE '%PRIVATE_SUB2API%' OR text LIKE '%omp-telegram.sub2api%' OR text LIKE ? OR header LIKE '%PRIVATE_SUB2API%'", "%"+sub2apiTestRequest1+"%").Scan(&leaked))
	if leaked != 0 {
		t.Fatal("private correlation metadata escaped to Telegram persistence")
	}
}

func TestSub2apiMultipleActualAccountsAndNextTaskIsolation(t *testing.T) {
	w, command, _, calls := sub2apiTestWorker(t, nil, nil)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	command("/queue")
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest2)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest3)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	if calls.Load() != 0 || w.sub2api.pending != nil {
		t.Fatal("capturing task request IDs started HTTP before settlement")
	}
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	if got := w.providerUsageText("openai-codex", true); got != "Plus 1% 16% · Prolite 7d 100%" {
		t.Fatalf("multi-account quota = %q", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("same-account request IDs were not deduplicated: %d quota reads", calls.Load())
	}
	command("/queue")
	if got := w.providerUsageText("openai-codex", true); got != "Plus 1% 16% · Prolite 7d 100%" {
		t.Fatalf("read-only queue input discarded confirmed accounts: %q", got)
	}
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	if got := w.providerUsageText("openai-codex", true); got != "" {
		t.Fatalf("new task inherited an unconfirmed account: %q", got)
	}
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest3)
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	if got := w.providerUsageText("openai-codex", true); got != "Plus 1% 16%" {
		t.Fatalf("new request attributed to previous task account: %q", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("account quota cache did not share confirmed accounts: %d reads", calls.Load())
	}
}

func TestSub2apiLateResultCannotAffectReplacement(t *testing.T) {
	w, _, _, _ := sub2apiTestWorker(t, nil, nil)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	finishSub2apiTask(t, w)
	var result sub2apiUsageResult
	select {
	case result = <-w.sub2api.results:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP result missing")
	}
	before := finalReplyStatus(t, w, 2, "answer")
	w.binding.Generation++
	w.sub2apiUsageFinished(result, time.Now())
	if after := finalReplyStatus(t, w, 2, "answer"); after != before {
		t.Fatal("old generation result changed the finalized header")
	}
}

func TestSub2apiUnmatchedMetadataIsPrivateAndDoesNotQuery(t *testing.T) {
	w, _, _, calls := sub2apiTestWorker(t, nil, nil)
	sub2apiTestNotify(t, w, "other-provider", w.binding.SessionID, sub2apiTestRequest1)
	sub2apiTestNotify(t, w, "openai-codex", "other-session", sub2apiTestRequest1)
	setProviderUsageFields(w, false)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	if w.sub2api.pending != nil || calls.Load() != 0 {
		t.Fatal("unmatched or disabled metadata started a quota query")
	}
	var leaked int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE text LIKE '%omp-telegram.sub2api%' OR text LIKE ?", "%"+sub2apiTestRequest1+"%").Scan(&leaked))
	if leaked != 0 {
		t.Fatal("ignored private metadata escaped to conversation")
	}
}

func TestSub2apiResearchFinalHeaderKeepsBodyAccount(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		name := "ready"
		if delayed {
			name = "delayed-ledger"
		}
		t.Run(name, func(t *testing.T) {
			w, _, _, calls := sub2apiTestWorker(t, nil, nil)
			w.b.cfg.Sub2API.Providers = []string{"openai-codex", "other-provider"}
			var lookups atomic.Int32
			var published atomic.Bool
			published.Store(!delayed)
			interceptSub2api(t, w, func(req *http.Request) *http.Response {
				if req.URL.Path == "/api/v1/admin/usage" {
					lookups.Add(1)
					id := req.URL.Query().Get("request_id")
					if id != "client:"+sub2apiTestRequest1 && id != "client:"+sub2apiTestRequest3 {
						t.Error("research queried an ID from a later empty round")
					}
					if !published.Load() {
						return sub2apiTestResponse(sub2apiEmptyLedger)
					}
				}
				return nil
			})
			w.research.root, w.research.enabled, w.research.roundOpen = true, true, true
			sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
			sub2apiTestNotify(t, w, "other-provider", w.binding.SessionID, sub2apiTestRequest3)
			w.event([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","model":"safe","provider":"other-provider","content":[{"type":"text","text":"round A"}]}}`))
			w.prepareReplyStatus(true, compactStatusFields{}, true)
			if !w.commitResearchRound() {
				t.Fatal("first research body not committed")
			}
			target := w.replyStatus.roundStatusOutboxID
			w.event([]byte(`{"type":"agent_start"}`))
			sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest2)
			w.refreshProviderUsage(time.Now().Add(time.Hour))
			if !w.commitResearchRound() {
				t.Fatal("empty research round not committed")
			}
			if lookups.Load() != 0 || calls.Load() != 0 || w.sub2api.pending != nil {
				t.Fatal("research sampled quota before final task settlement")
			}
			w.research.enabled = false
			w.prepareReplyStatus(false, compactStatusFields{}, true)
			w.finishResult(false)
			if w.taskActive() || queueState(t, w, 2) != "done" {
				t.Fatal("research task did not finish before quota sampling")
			}
			w.refreshProviderUsage(time.Now())
			awaitSub2apiUsage(t, w)
			if delayed {
				published.Store(true)
				w.refreshProviderUsage(w.sub2api.retryNext)
				awaitSub2apiUsage(t, w)
			}
			wantLookups := int32(2)
			if delayed {
				wantLookups = 4
			}
			if lookups.Load() != wantLookups {
				t.Fatalf("research did not retain all body-round request IDs: lookups=%d want=%d", lookups.Load(), wantLookups)
			}
			var header, body string
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT header,text FROM outbox WHERE id=?", target).Scan(&header, &body))
			if body != "round A" || !strings.Contains(header, "Plus 1% 16%") || strings.Contains(header, "Prolite") || calls.Load() != 1 {
				t.Fatalf("empty round replaced body account: header=%q body=%q quota_calls=%d", header, body, calls.Load())
			}
		})
	}
}

func TestSub2apiTaskCollectsConfiguredProviders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reverse  bool
		provider string
	}{
		{"first-provider-body", false, "openai-codex"},
		{"reverse-notifications", true, "openai-codex"},
		{"second-provider-body", false, "other-provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, _, calls := sub2apiTestWorker(t, nil, nil)
			w.b.cfg.Sub2API.Providers = []string{"openai-codex", "other-provider"}
			var lookups atomic.Int32
			interceptSub2api(t, w, func(req *http.Request) *http.Response {
				if req.URL.Path == "/api/v1/admin/usage" {
					lookups.Add(1)
				}
				return nil
			})
			if tc.reverse {
				sub2apiTestNotify(t, w, "other-provider", w.binding.SessionID, sub2apiTestRequest2)
				sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
			} else {
				sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
				sub2apiTestNotify(t, w, "other-provider", w.binding.SessionID, sub2apiTestRequest2)
			}
			sub2apiTestNotify(t, w, "other-provider", w.binding.SessionID, sub2apiTestRequest3)
			raw, err := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{
				"role": "assistant", "stopReason": "stop", "model": "safe", "provider": tc.provider,
				"content": []any{map[string]any{"type": "text", "text": "answer"}},
			}})
			requireStoreOK(t, err)
			w.event(raw)
			w.event([]byte(`{"type":"agent_end","messages":[]}`))
			awaitReplyStatus(t, w)
			w.refreshProviderUsage(time.Now())
			awaitSub2apiUsage(t, w)
			header := finalReplyStatus(t, w, 2, "answer")
			if !strings.Contains(header, "Plus 1% 16% · Prolite 7d 100%") || lookups.Load() != 3 || calls.Load() != 2 {
				t.Fatalf("task lost provider requests or repeated an account read: lookups=%d calls=%d header=%q", lookups.Load(), calls.Load(), header)
			}
			if w.providerUsage.targetProvider != tc.provider {
				t.Fatal("quota changed the actual body provider")
			}
		})
	}
}

func TestSub2apiStatusProjectionPreservesActualResponse(t *testing.T) {
	w, _, _, calls := sub2apiTestWorker(t, nil, nil)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	nativeRPC(t, w, "set_model", map[string]any{"provider": "display-provider", "modelId": "display-model"})
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	header := finalReplyStatus(t, w, 2, "answer")
	if !strings.Contains(header, "Plus 1% 16%") || calls.Load() != 1 || w.sub2api.requests[sub2apiTestRequest1] != 2 {
		t.Fatalf("status projection discarded the actual response: calls=%d header=%q", calls.Load(), header)
	}
	if w.providerUsage.targetProvider != "openai-codex" || queueState(t, w, 2) != "done" {
		t.Fatal("status projection changed the reply owner or prevented completion")
	}
}
