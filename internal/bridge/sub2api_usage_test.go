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
	t.Cleanup(func() { http.DefaultTransport = fakeTransport })
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

func awaitSub2apiUsage(t *testing.T, w *worker) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for w.sub2api.pending != nil {
		select {
		case result := <-w.sub2api.results:
			w.sub2apiUsageFinished(result)
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
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("quota read not started")
	}
	endUsageReply(w)
	awaitReplyStatus(t, w)
	if queueState(t, w, 2) != "done" {
		t.Fatal("HTTP quota read blocked task settlement")
	}
	release.Do(func() { close(gate) })
	awaitSub2apiUsage(t, w)
	header := finalReplyStatus(t, w, 2, "answer")
	if !strings.Contains(header, "Plus 99% 84%") {
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
	awaitSub2apiUsage(t, w)
	command("/queue")
	if got := w.providerUsageText("openai-codex", true); got != "Plus 99% 84%" {
		t.Fatalf("read-only queue input discarded the active task account: %q", got)
	}
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest2)
	awaitSub2apiUsage(t, w)
	text := w.providerUsageText("openai-codex", true)
	if text != "Plus 99% 84% · Prolite 7d 0%" {
		t.Fatalf("multi-account quota = %q", text)
	}
	endUsageReply(w)
	awaitReplyStatus(t, w)
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	if got := w.providerUsageText("openai-codex", true); got != "" {
		t.Fatalf("new task inherited an unconfirmed account: %q", got)
	}
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest3)
	awaitSub2apiUsage(t, w)
	if got := w.providerUsageText("openai-codex", true); got != "Plus 99% 84%" {
		t.Fatalf("new request attributed to previous task account: %q", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("account quota cache did not share confirmed accounts: %d reads", calls.Load())
	}
}

func TestSub2apiLateResultCannotAffectReplacement(t *testing.T) {
	w, _, _, _ := sub2apiTestWorker(t, nil, nil)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	var result sub2apiUsageResult
	select {
	case result = <-w.sub2api.results:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP result missing")
	}
	endUsageReply(w)
	awaitReplyStatus(t, w)
	before := finalReplyStatus(t, w, 2, "answer")
	w.binding.Generation++
	w.sub2apiUsageFinished(result)
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
	for _, late := range []bool{false, true} {
		name := "ready"
		if late {
			name = "late"
		}
		t.Run(name, func(t *testing.T) {
			w, _, _, _ := sub2apiTestWorker(t, nil, nil)
			w.research.root, w.research.enabled, w.research.roundOpen = true, true, true
			sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
			awaitSub2apiUsage(t, w)
			w.event([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","model":"safe","provider":"openai-codex","content":[{"type":"text","text":"round A"}]}}`))
			w.prepareReplyStatus(true, compactStatusFields{}, true)
			if !w.commitResearchRound() {
				t.Fatal("first research body not committed")
			}
			target := w.replyStatus.roundStatusOutboxID
			w.event([]byte(`{"type":"agent_start"}`))
			sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest2)
			var result sub2apiUsageResult
			if late {
				select {
				case result = <-w.sub2api.results:
				case <-time.After(3 * time.Second):
					t.Fatal("second research quota result missing")
				}
			} else {
				awaitSub2apiUsage(t, w)
			}
			if !w.commitResearchRound() {
				t.Fatal("empty research round not committed")
			}
			w.prepareReplyStatus(false, compactStatusFields{}, true)
			w.finishResult(false)
			if late {
				w.sub2apiUsageFinished(result)
			}
			var header, body string
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT header,text FROM outbox WHERE id=?", target).Scan(&header, &body))
			if body != "round A" || !strings.Contains(header, "Plus 99% 84%") || strings.Contains(header, "Prolite") {
				t.Fatalf("second round quota replaced the first round body account: header=%q body=%q", header, body)
			}
		})
	}
}
