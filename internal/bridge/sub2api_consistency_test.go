package bridge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sub2apiTestResponse(data string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(data)), Header: make(http.Header)}
}

func interceptSub2api(t *testing.T, w *worker, handler func(*http.Request) *http.Response) {
	t.Helper()
	transport := http.DefaultTransport
	http.DefaultTransport = logTestTransport(func(req *http.Request) (*http.Response, error) {
		if strings.HasPrefix(req.URL.String(), w.b.cfg.Sub2API.BaseURL+"/") {
			if response := handler(req); response != nil {
				return response, nil
			}
		}
		return transport.RoundTrip(req)
	})
	t.Cleanup(func() {
		w.cancel()
		w.background.Wait()
		http.DefaultTransport = transport
	})
}

const sub2apiEmptyLedger = `{"code":0,"data":{"page":1,"total":0,"items":[]}}`

func TestSub2apiAttributionSurvivesLongStreamAndDelayedLedger(t *testing.T) {
	w, _, _, _ := sub2apiTestWorker(t, nil, nil)
	var published atomic.Bool
	var lookups, sends atomic.Int32
	interceptSub2api(t, w, func(req *http.Request) *http.Response {
		if req.URL.Path == "/api/v1/admin/usage" {
			lookups.Add(1)
			if !published.Load() {
				return sub2apiTestResponse(sub2apiEmptyLedger)
			}
		}
		return nil
	})
	transport := http.DefaultTransport
	http.DefaultTransport = logTestTransport(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/sendMessage") {
			sends.Add(1)
		}
		return transport.RoundTrip(req)
	})
	t.Cleanup(func() {
		w.cancel()
		w.background.Wait()
		http.DefaultTransport = transport
	})
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	awaitSub2apiUsage(t, w)
	for range 4 {
		w.refreshSub2apiUsage(time.Now().Add(40 * time.Second))
		awaitSub2apiUsage(t, w)
	}
	if got := lookups.Load(); got != 0 {
		t.Fatalf("streaming request performed attribution HTTP before settlement: %d lookups", got)
	}
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	if queueState(t, w, 2) != "done" {
		t.Fatal("unresolved attribution blocked task completion")
	}
	deliverReplies(t, w)
	var target, message int64
	var body string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT id,message_id,text FROM outbox WHERE inbox_id=2 AND kind='text' AND assistant_body=1 ORDER BY id DESC LIMIT 1").Scan(&target, &message, &body))
	beforeSends := sends.Load()
	published.Store(true)
	w.refreshSub2apiUsage(w.sub2api.retryNext)
	awaitSub2apiUsage(t, w)
	if got := finalReplyStatus(t, w, 2, "answer"); !strings.Contains(got, "Plus 1% 16%") {
		t.Fatalf("published ledger did not backfill the completed reply: %q", got)
	}
	deliverReplies(t, w)
	var afterMessage int64
	var afterBody string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT message_id,text FROM outbox WHERE id=?", target).Scan(&afterMessage, &afterBody))
	if message == 0 || afterMessage != message || afterBody != body || sends.Load() != beforeSends {
		t.Fatalf("quota backfill replaced or resent the body: message=%d/%d body=%q/%q sends=%d/%d", message, afterMessage, body, afterBody, beforeSends, sends.Load())
	}
}

func TestSub2apiAttributionRetryWindowIsBounded(t *testing.T) {
	w, _, _, _ := sub2apiTestWorker(t, nil, nil)
	var lookups atomic.Int32
	interceptSub2api(t, w, func(req *http.Request) *http.Response {
		if req.URL.Path == "/api/v1/admin/usage" {
			lookups.Add(1)
			return sub2apiTestResponse(sub2apiEmptyLedger)
		}
		return nil
	})
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	finishSub2apiTask(t, w)
	now := time.Now()
	w.refreshSub2apiUsage(now)
	awaitSub2apiUsageAt(t, w, now)
	w.refreshSub2apiUsage(now.Add(time.Second - time.Nanosecond))
	if w.sub2api.pending != nil || lookups.Load() != 1 {
		t.Fatal("attribution retried before its one-second deadline")
	}
	w.refreshSub2apiUsage(now.Add(time.Second))
	awaitSub2apiUsageAt(t, w, now.Add(time.Second))
	if lookups.Load() != 2 {
		t.Fatal("attribution did not retry after one second")
	}
	lastRetry := now.Add(10*time.Second - time.Nanosecond)
	w.refreshSub2apiUsage(lastRetry)
	awaitSub2apiUsageAt(t, w, lastRetry)
	if lookups.Load() != 3 {
		t.Fatal("attribution stopped before its retry window expired")
	}
	deadline := now.Add(10 * time.Second)
	w.refreshSub2apiUsage(deadline)
	awaitSub2apiUsageAt(t, w, deadline)
	if lookups.Load() != 3 || !w.sub2api.retryExhausted || !w.sub2api.next.IsZero() {
		t.Fatalf("ledger retry escaped its 10-second window or entered quota cooldown: lookups=%d next=%v", lookups.Load(), w.sub2api.next)
	}
}

func TestSub2apiRefreshFailureKeepsConfirmedLastGood(t *testing.T) {
	for _, mode := range []string{"failure", "empty"} {
		t.Run(mode, func(t *testing.T) {
			w, _, _, _ := sub2apiTestWorker(t, nil, nil)
			sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
			finishSub2apiTask(t, w)
			awaitSub2apiUsage(t, w)
			interceptSub2api(t, w, func(req *http.Request) *http.Response {
				if strings.HasSuffix(req.URL.Path, "/quota") {
					if mode == "empty" {
						return sub2apiTestResponse(`{"code":0,"data":{"plan_type":"plus"}}`)
					}
					response := sub2apiTestResponse("unavailable")
					response.StatusCode = http.StatusServiceUnavailable
					return response
				}
				return nil
			})
			for _, elapsed := range []time.Duration{providerUsageInterval, 24 * time.Hour, 365 * 24 * time.Hour} {
				w.b.providerUsage.Lock()
				for key, entry := range w.b.providerUsage.entries {
					entry.next = time.Now().Add(-time.Second)
					w.b.providerUsage.entries[key] = entry
				}
				w.b.providerUsage.Unlock()
				at := time.Now().Add(elapsed)
				w.refreshProviderUsage(at)
				if got := w.providerUsageText("openai-codex", true); got != "Plus 1% 16%" {
					t.Fatalf("starting a refresh erased confirmed quota: %q", got)
				}
				awaitSub2apiUsageAt(t, w, at)
				if got := finalReplyStatus(t, w, 2, "answer"); !strings.Contains(got, "Plus 1% 16%") {
					t.Fatalf("unsuccessful refresh removed last-good after %v: %q", elapsed, got)
				}
				if got := w.sub2api.sub2apiQuotaSnapshot.text(at); got != "Plus 1% 16%" {
					t.Fatalf("last-good disappeared with age %v: %q", elapsed, got)
				}
			}
		})
	}
}

func TestSub2apiConfirmedAccountsShareFiveMinuteRefreshClaim(t *testing.T) {
	w, _, _, calls := sub2apiTestWorker(t, nil, nil)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest2)
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	now := time.Now()
	w.b.providerUsage.Lock()
	for key, entry := range w.b.providerUsage.entries {
		entry.next = now.Add(providerUsageInterval)
		w.b.providerUsage.entries[key] = entry
	}
	w.b.providerUsage.Unlock()
	for _, elapsed := range []time.Duration{0, time.Minute, providerUsageInterval - time.Nanosecond} {
		at := now.Add(elapsed)
		w.refreshProviderUsage(at)
		awaitSub2apiUsageAt(t, w, at)
		if got := w.providerUsageText("openai-codex", true); got != "Plus 1% 16% · Prolite 7d 100%" {
			t.Fatalf("shared refresh claim hid confirmed accounts: %q", got)
		}
		if calls.Load() != 2 {
			t.Fatalf("five-minute account claim allowed duplicate sampling: %d reads", calls.Load())
		}
	}
	w.b.providerUsage.Lock()
	for key, entry := range w.b.providerUsage.entries {
		entry.next = time.Now().Add(-time.Second)
		w.b.providerUsage.entries[key] = entry
	}
	w.b.providerUsage.Unlock()
	w.refreshProviderUsage(now.Add(providerUsageInterval))
	awaitSub2apiUsageAt(t, w, now.Add(providerUsageInterval))
	if calls.Load() != 4 {
		t.Fatalf("accounts did not refresh after five-minute claim: %d reads", calls.Load())
	}
}

func TestSub2apiPartialAttributionIsRetainedWhileLaterIDWaits(t *testing.T) {
	w, _, _, _ := sub2apiTestWorker(t, nil, nil)
	var firstLookups, laterLookups atomic.Int32
	var laterReady atomic.Bool
	interceptSub2api(t, w, func(req *http.Request) *http.Response {
		if req.URL.Path == "/api/v1/admin/usage" {
			if req.URL.Query().Get("request_id") == "client:"+sub2apiTestRequest2 {
				laterLookups.Add(1)
				if !laterReady.Load() {
					return sub2apiTestResponse(sub2apiEmptyLedger)
				}
			} else {
				firstLookups.Add(1)
			}
		}
		return nil
	})
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest2)
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	if got := w.providerUsageText("openai-codex", true); got != "Plus 1% 16%" {
		t.Fatalf("unready later request erased a confirmed account: %q", got)
	}
	w.refreshProviderUsage(w.sub2api.retryNext)
	awaitSub2apiUsage(t, w)
	laterReady.Store(true)
	w.refreshProviderUsage(w.sub2api.retryNext)
	awaitSub2apiUsage(t, w)
	if firstLookups.Load() != 1 || laterLookups.Load() != 3 {
		t.Fatalf("partial attribution repeated confirmed IDs or lost pending IDs: first=%d later=%d", firstLookups.Load(), laterLookups.Load())
	}
	if got := w.providerUsageText("openai-codex", true); got != "Plus 1% 16% · Prolite 7d 100%" {
		t.Fatalf("published later account did not join the confirmed account: %q", got)
	}
}

func TestSub2apiLookupFailureDoesNotBlockAnotherIDsSettlement(t *testing.T) {
	w, _, _, _ := sub2apiTestWorker(t, nil, nil)
	var phase, healthyLookups atomic.Int32
	interceptSub2api(t, w, func(req *http.Request) *http.Response {
		if req.URL.Path != "/api/v1/admin/usage" {
			return nil
		}
		id := req.URL.Query().Get("request_id")
		stage := phase.Load()
		if id == "client:"+sub2apiTestRequest2 {
			healthyLookups.Add(1)
			if stage == 2 {
				return nil
			}
		} else if stage != 0 {
			response := sub2apiTestResponse("unavailable")
			response.StatusCode = http.StatusServiceUnavailable
			return response
		}
		return sub2apiTestResponse(sub2apiEmptyLedger)
	})
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest2)
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	deadline := w.sub2api.retryUntil
	firstMiss := deadline.Add(-sub2apiRetryWindow)
	phase.Store(1)
	w.refreshProviderUsage(firstMiss.Add(time.Second))
	awaitSub2apiUsageAt(t, w, firstMiss.Add(time.Second))
	phase.Store(2)
	w.refreshProviderUsage(firstMiss.Add(2 * time.Second))
	awaitSub2apiUsageAt(t, w, firstMiss.Add(2*time.Second))
	header := finalReplyStatus(t, w, 2, "answer")
	if healthyLookups.Load() != 3 || w.sub2api.requests[sub2apiTestRequest2] != 3 || !strings.Contains(header, "Prolite 7d 100%") {
		t.Fatalf("failed lookup blocked a ready independent ID: lookups=%d account=%d header=%q", healthyLookups.Load(), w.sub2api.requests[sub2apiTestRequest2], header)
	}
	if w.sub2api.requests[sub2apiTestRequest1] != 0 || strings.Contains(header, "Plus") || w.sub2api.retryUntil != deadline {
		t.Fatal("failed lookup acquired an account or extended the settlement window")
	}
}

func TestSub2apiNewTaskDiscardsLateAttribution(t *testing.T) {
	w, command, _, _ := sub2apiTestWorker(t, nil, nil)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	finishSub2apiTask(t, w)
	var result sub2apiUsageResult
	select {
	case result = <-w.sub2api.results:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP result missing")
	}
	before := finalReplyStatus(t, w, 2, "answer")
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	w.sub2apiUsageFinished(result, time.Now())
	if got := w.providerUsageText("openai-codex", true); got != "" {
		t.Fatalf("new task borrowed an unconfirmed account: %q", got)
	}
	if got := finalReplyStatus(t, w, 2, "answer"); got != before {
		t.Fatal("late attribution changed the previous task header after replacement")
	}
}

func TestSub2apiHistoricalSnapshotDoesNotAdoptNextTaskAccount(t *testing.T) {
	w, command, _, _ := sub2apiTestWorker(t, nil, nil)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	snapshot := w.sub2api.sub2apiQuotaSnapshot
	before := finalReplyStatus(t, w, 2, "answer")
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest2)
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	if got := w.providerUsageText("openai-codex", true); got != "Prolite 7d 100%" {
		t.Fatalf("next task did not use its own account: %q", got)
	}
	if got := snapshot.text(time.Now().Add(365 * 24 * time.Hour)); got != "Plus 1% 16%" {
		t.Fatalf("historical last-good disappeared or adopted another account: %q", got)
	}
	if got := finalReplyStatus(t, w, 2, "answer"); got != before {
		t.Fatalf("next task changed historical final header: %q / %q", got, before)
	}
}

func TestSub2apiMissingQuotaCannotBorrowAnotherAccount(t *testing.T) {
	w, command, _, _ := sub2apiTestWorker(t, nil, nil)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	command("missing-terminal")
	w.dispatch()
	drainWatchdogEvents(t, w)
	interceptSub2api(t, w, func(req *http.Request) *http.Response {
		if strings.HasSuffix(req.URL.Path, "/3/quota") {
			return sub2apiTestResponse(`{"code":0,"data":{"plan_type":"prolite"}}`)
		}
		return nil
	})
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest2)
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	if w.sub2api.requests[sub2apiTestRequest2] != 3 {
		t.Fatal("new account attribution was not retained for an empty quota response")
	}
	if got := w.providerUsageText("openai-codex", true); got != "" {
		t.Fatalf("an account without a successful sample borrowed another account's quota: %q", got)
	}
}

func TestSub2apiConcurrentColdCacheCommitBackfillsConsumer(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	w, _, _, calls := sub2apiTestWorker(t, gate, started)
	client, scope := w.b.sub2apiHTTP()
	other := make(chan sub2apiUsageResult, 1)
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		other <- w.b.querySub2apiUsage(w.ctx, client, scope, &sub2apiUsageRequest{accounts: []int64{2}})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("shared cold-cache quota read did not start")
	}
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	if got := w.providerUsageText("openai-codex", true); got != "" {
		t.Fatalf("a claimed cold cache fabricated a sample: %q", got)
	}
	if queueState(t, w, 2) != "done" {
		t.Fatal("another worker's quota refresh blocked task completion")
	}
	close(gate)
	select {
	case result := <-other:
		if result.err != nil {
			t.Fatalf("shared quota sampling failed: %v", result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shared quota sample was not committed")
	}
	// The actor's normal refresh tick must observe the commit during cooldown.
	w.refreshProviderUsage(time.Now())
	if got := finalReplyStatus(t, w, 2, "answer"); !strings.Contains(got, "Plus 1% 16%") {
		t.Fatalf("shared successful sampling did not promptly backfill its consumer: %q", got)
	}
	if calls.Load() != 1 || w.sub2api.pending != nil {
		t.Fatalf("observing a shared commit repeated HTTP quota sampling: calls=%d pending=%t", calls.Load(), w.sub2api.pending != nil)
	}
}

func TestSub2apiQuotaFailureAfterAttributionDoesNotExhaustRefresh(t *testing.T) {
	w, _, _, _ := sub2apiTestWorker(t, nil, nil)
	var published, quotaReady atomic.Bool
	interceptSub2api(t, w, func(req *http.Request) *http.Response {
		if req.URL.Path == "/api/v1/admin/usage" && !published.Load() {
			return sub2apiTestResponse(sub2apiEmptyLedger)
		}
		if strings.HasSuffix(req.URL.Path, "/quota") && !quotaReady.Load() {
			response := sub2apiTestResponse("unavailable")
			response.StatusCode = http.StatusServiceUnavailable
			return response
		}
		return nil
	})
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	finishSub2apiTask(t, w)
	now := time.Now()
	w.refreshSub2apiUsage(now)
	awaitSub2apiUsageAt(t, w, now)
	published.Store(true)
	now = w.sub2api.retryNext
	w.refreshSub2apiUsage(now)
	awaitSub2apiUsageAt(t, w, now)
	if w.sub2api.requests[sub2apiTestRequest1] != 2 {
		t.Fatal("ledger publication did not confirm the account")
	}
	quotaReady.Store(true)
	w.b.providerUsage.Lock()
	for key, entry := range w.b.providerUsage.entries {
		entry.next = time.Now().Add(-time.Second)
		w.b.providerUsage.entries[key] = entry
	}
	w.b.providerUsage.Unlock()
	w.refreshSub2apiUsage(now.Add(providerUsageInterval))
	awaitSub2apiUsageAt(t, w, now.Add(providerUsageInterval))
	if got := w.providerUsageText("openai-codex", true); got != "Plus 1% 16%" {
		t.Fatalf("finished attribution left quota refresh stuck in its retry window: %q", got)
	}
}

func TestSub2apiOrdinaryContinuationPreservesTaskAccounts(t *testing.T) {
	for _, mode := range []string{"first-account", "second-account", "delayed-ledger"} {
		t.Run(mode, func(t *testing.T) {
			w, _, _, calls := sub2apiTestWorker(t, nil, nil)
			var published atomic.Bool
			published.Store(mode != "delayed-ledger")
			interceptSub2api(t, w, func(req *http.Request) *http.Response {
				if req.URL.Path == "/api/v1/admin/usage" && !published.Load() {
					return sub2apiTestResponse(sub2apiEmptyLedger)
				}
				return nil
			})
			sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
			active, turn := w.active, w.turn
			w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"first round"}}`))
			w.event([]byte(`{"type":"agent_end","isTerminal":false,"messages":[]}`))
			w.event([]byte(`{"type":"agent_start"}`))
			if w.active != active || w.turn != turn {
				t.Fatal("ordinary continuation replaced its task")
			}
			if mode == "second-account" {
				sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest2)
			}
			w.refreshProviderUsage(time.Now().Add(time.Hour))
			if w.sub2api.pending != nil || calls.Load() != 0 {
				t.Fatal("ordinary continuation started HTTP before terminal settlement")
			}
			finishSub2apiTask(t, w)
			awaitSub2apiUsage(t, w)
			if mode == "delayed-ledger" {
				published.Store(true)
				w.refreshProviderUsage(w.sub2api.retryNext)
				awaitSub2apiUsage(t, w)
			}
			header := finalReplyStatus(t, w, 2, "first round", "answer")
			want := "Plus 1% 16%"
			if mode == "second-account" {
				want += " · Prolite 7d 100%"
			}
			if !strings.Contains(header, want) || queueState(t, w, 2) != "done" {
				t.Fatalf("continuation lost confirmed or pending task accounts: header=%q want=%q", header, want)
			}
		})
	}
}

func TestSub2apiExhaustedAttributionKeepsConfirmedAccountRefreshing(t *testing.T) {
	w, _, _, _ := sub2apiTestWorker(t, nil, nil)
	var quotaReady atomic.Bool
	var missingLookups, quotaCalls atomic.Int32
	interceptSub2api(t, w, func(req *http.Request) *http.Response {
		if req.URL.Path == "/api/v1/admin/usage" && req.URL.Query().Get("request_id") == "client:"+sub2apiTestRequest2 {
			missingLookups.Add(1)
			return sub2apiTestResponse(sub2apiEmptyLedger)
		}
		if strings.HasSuffix(req.URL.Path, "/2/quota") {
			quotaCalls.Add(1)
			if !quotaReady.Load() {
				response := sub2apiTestResponse("unavailable")
				response.StatusCode = http.StatusServiceUnavailable
				return response
			}
		}
		return nil
	})
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest2)
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	if w.sub2api.retryUntil.IsZero() {
		t.Fatal("unresolved ledger did not get a post-settlement retry window")
	}
	now := w.sub2api.retryUntil.Add(-sub2apiRetryWindow)
	for elapsed := sub2apiRetryInterval; elapsed <= sub2apiRetryWindow; elapsed += sub2apiRetryInterval {
		w.refreshProviderUsage(now.Add(elapsed))
		awaitSub2apiUsageAt(t, w, now.Add(elapsed))
	}
	beforeLookups := missingLookups.Load()
	if !w.sub2api.retryExhausted || quotaCalls.Load() != 1 {
		t.Fatal("driver did not exhaust attribution while quota was cooling down")
	}
	quotaReady.Store(true)
	for i := 1; i <= 2; i++ {
		w.b.providerUsage.Lock()
		for key, entry := range w.b.providerUsage.entries {
			entry.next = time.Now().Add(-time.Second)
			w.b.providerUsage.entries[key] = entry
		}
		w.b.providerUsage.Unlock()
		at := time.Now().Add(time.Duration(i) * providerUsageInterval)
		w.refreshProviderUsage(at)
		awaitSub2apiUsageAt(t, w, at)
		if got := finalReplyStatus(t, w, 2, "answer"); !strings.Contains(got, "Plus 1% 16%") {
			t.Fatalf("exhausted ledger blocked a confirmed account refresh: %q", got)
		}
		if quotaCalls.Load() != int32(i+1) || missingLookups.Load() != beforeLookups || !w.sub2api.retryExhausted {
			t.Fatalf("quota refresh repeated exhausted attribution: quota=%d lookups=%d/%d exhausted=%t", quotaCalls.Load(), missingLookups.Load(), beforeLookups, w.sub2api.retryExhausted)
		}
	}
}

func TestSub2apiNewInputCancelsRefreshWithoutLosingConfirmedAccount(t *testing.T) {
	w, command, _, calls := sub2apiTestWorker(t, nil, nil)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	interceptSub2api(t, w, func(req *http.Request) *http.Response {
		if strings.HasSuffix(req.URL.Path, "/2/quota") {
			started <- struct{}{}
			<-req.Context().Done()
			cancelled <- struct{}{}
			return sub2apiTestResponse("cancelled")
		}
		return nil
	})
	w.b.providerUsage.Lock()
	for key, entry := range w.b.providerUsage.entries {
		entry.next = time.Now().Add(-time.Second)
		w.b.providerUsage.entries[key] = entry
	}
	w.b.providerUsage.Unlock()
	w.sub2api.next = time.Time{}
	w.refreshProviderUsage(time.Now())
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("quota refresh did not start")
	}
	command("/queue")
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("new input did not cancel in-flight HTTP")
	}
	awaitSub2apiUsage(t, w)
	if got := w.providerUsageText("openai-codex", true); got != "Plus 1% 16%" {
		t.Fatalf("read-only input discarded confirmed account or last-good: %q", got)
	}
	w.refreshProviderUsage(time.Now())
	awaitSub2apiUsage(t, w)
	if calls.Load() != 1 {
		t.Fatal("cancelled refresh rolled back an already-started quota claim")
	}
}

func TestSub2apiPeriodicRefreshRequiresSharedIdleGate(t *testing.T) {
	w, _, _, calls := sub2apiTestWorker(t, nil, nil)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	finishSub2apiTask(t, w)
	awaitSub2apiUsage(t, w)
	for _, tc := range []struct {
		name string
		busy *bool
	}{
		{"continuation", &w.awaitingContinuation},
		{"steer", &w.steerFence.active},
		{"research", &w.research.enabled},
		{"research-disable", &w.research.disablePending},
		{"restore", &w.restoring},
		{"resume", &w.runtimeResuming},
		{"rpc", &w.rpcOperationActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w.b.providerUsage.Lock()
			for key, entry := range w.b.providerUsage.entries {
				entry.next = time.Now().Add(-time.Second)
				w.b.providerUsage.entries[key] = entry
			}
			w.b.providerUsage.Unlock()
			w.sub2api.next = time.Time{}
			before := calls.Load()
			*tc.busy = true
			w.refreshProviderUsage(time.Now())
			if w.sub2api.pending != nil || calls.Load() != before {
				t.Fatal("periodic quota HTTP bypassed the shared idle gate")
			}
			if got := w.providerUsageText("openai-codex", true); got != "Plus 1% 16%" {
				t.Fatalf("busy gate hid already-confirmed quota: %q", got)
			}
			*tc.busy = false
			w.refreshProviderUsage(time.Now())
			awaitSub2apiUsage(t, w)
			if calls.Load() != before+1 {
				t.Fatal("returning to idle did not resume quota refresh")
			}
		})
	}
}

func TestSub2apiCancelledBatchKeepsUnqueriedAccountRefreshable(t *testing.T) {
	for _, mode := range []string{"cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			w, _, _, calls := sub2apiTestWorker(t, nil, nil)
			ctx, cancel := context.WithCancel(w.ctx)
			wantErr := context.Canceled
			if mode == "expired" {
				cancel()
				ctx, cancel = context.WithDeadline(w.ctx, time.Now().Add(-time.Second))
				wantErr = context.DeadlineExceeded
			}
			t.Cleanup(cancel)
			interceptSub2api(t, w, func(req *http.Request) *http.Response {
				if strings.HasSuffix(req.URL.Path, "/2/quota") {
					cancel()
				}
				return nil
			})
			client, scope := w.b.sub2apiHTTP()
			result := w.b.querySub2apiUsage(ctx, client, scope, &sub2apiUsageRequest{accounts: []int64{2, 3}})
			if !errors.Is(result.err, wantErr) {
				t.Fatalf("cancelled batch returned %v, want %v", result.err, wantErr)
			}
			sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest2)
			awaitSub2apiUsage(t, w)
			finishSub2apiTask(t, w)
			awaitSub2apiUsage(t, w)
			header := finalReplyStatus(t, w, 2, "answer")
			if !strings.Contains(header, "Prolite 7d 100%") || calls.Load() != 1 {
				t.Fatalf("cancelled batch blocked a fresh account query: header=%q quota_calls=%d", header, calls.Load())
			}
		})
	}
}

func TestSub2apiBatchTimeoutKeepsUnqueriedAccountRefreshable(t *testing.T) {
	gate := make(chan struct{})
	w, _, _, calls := sub2apiTestWorker(t, gate, nil)
	var blocked atomic.Int64
	interceptSub2api(t, w, func(req *http.Request) *http.Response {
		if strings.HasSuffix(req.URL.Path, "/2/quota") {
			blocked.CompareAndSwap(0, 2)
		} else if strings.HasSuffix(req.URL.Path, "/3/quota") {
			blocked.CompareAndSwap(0, 3)
		}
		return nil
	})
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest1)
	sub2apiTestNotify(t, w, "openai-codex", w.binding.SessionID, sub2apiTestRequest2)
	finishSub2apiTask(t, w)
	var result sub2apiUsageResult
	select {
	case result = <-w.sub2api.results:
	case <-time.After(sub2apiUsageTimeout + 3*time.Second):
		t.Fatal("batch timeout did not return")
	}
	close(gate)
	if !errors.Is(result.err, context.DeadlineExceeded) || len(result.resolved) != 2 || calls.Load() != 1 {
		t.Fatalf("first quota read did not exhaust the batch: resolved=%d calls=%d err=%v", len(result.resolved), calls.Load(), result.err)
	}
	now := time.Now()
	w.sub2apiUsageFinished(result, now)
	want := "Plus 1% 16%"
	if blocked.Load() == 2 {
		want = "Prolite 7d 100%"
	} else if blocked.Load() != 3 {
		t.Fatal("no first quota read was observed")
	}
	w.refreshProviderUsage(now.Add(1500 * time.Millisecond))
	awaitSub2apiUsage(t, w)
	header := finalReplyStatus(t, w, 2, "answer")
	if calls.Load() != 2 || !strings.Contains(header, want) {
		t.Fatalf("batch timeout blocked an unqueried account: calls=%d header=%q", calls.Load(), header)
	}
}
