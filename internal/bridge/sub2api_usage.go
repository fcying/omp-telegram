package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/sub2api"
)

const (
	maxSub2apiRequests   = 64
	sub2apiUsageTimeout  = 10 * time.Second
	sub2apiRetryInterval = time.Second
	sub2apiRetryWindow   = 10 * time.Second
)

type sub2apiBridge struct {
	once   sync.Once
	client *sub2api.Client
	scope  [32]byte
}

type sub2apiQuotaSnapshot struct {
	samples []quotaSample
}

func (s sub2apiQuotaSnapshot) text(now time.Time) string {
	var result strings.Builder
	for i := range s.samples {
		if !s.samples[i].available() {
			continue
		}
		if result.Len() > 0 {
			result.WriteString(" · ")
		}
		s.samples[i].appendTo(&result, now)
	}
	return result.String()
}

func (b *Bridge) sub2apiHTTP() (*sub2api.Client, [32]byte) {
	b.sub2api.once.Do(func() {
		c := b.cfg.Sub2API
		b.sub2api.client = sub2api.New(c.BaseURL, c.APIKey)
		b.sub2api.scope = sha256.Sum256([]byte(c.BaseURL + "\x00" + c.APIKey))
	})
	return b.sub2api.client, b.sub2api.scope
}

type sub2apiUsageState struct {
	epoch             uint64
	client            *omp.Client
	generation        int64
	catalog           uint64
	sessionID         string
	turn              uint64
	inbox             int64
	target            int64
	provider          string // Actual body provider, bound when its header is committed.
	requests          map[string]int64
	accounts          []int64
	invalid           bool
	lastGoodProvider  string
	lastGoodSessionID string
	lastGoodSnapshot  sub2apiQuotaSnapshot
	sub2apiQuotaSnapshot
	next           time.Time
	retryNext      time.Time
	retryUntil     time.Time
	retryExhausted bool
	pending        *sub2apiUsageRequest
	results        chan sub2apiUsageResult
}

type sub2apiUsageRequest struct {
	epoch      uint64
	client     *omp.Client
	generation int64
	catalog    uint64
	sessionID  string
	turn       uint64
	provider   string
	cancel     context.CancelFunc
	unresolved []string
	accounts   []int64
}

type sub2apiUsageResult struct {
	request  *sub2apiUsageRequest
	resolved map[string]int64
	next     time.Time
	err      error
}

func (w *worker) resetSub2apiUsage() {
	s := &w.sub2api
	lastGoodProvider := s.lastGoodProvider
	lastGoodSessionID := s.lastGoodSessionID
	lastGoodSnapshot := sub2apiQuotaSnapshot{samples: slices.Clone(s.lastGoodSnapshot.samples)}
	if current := s.sub2apiQuotaSnapshot; s.provider != "" && s.sessionID != "" && current.text(time.Now()) != "" {
		lastGoodProvider, lastGoodSessionID = s.provider, s.sessionID
		lastGoodSnapshot = sub2apiQuotaSnapshot{samples: slices.Clone(current.samples)}
	}
	w.cancelSub2apiUsage()
	w.sub2api = sub2apiUsageState{
		epoch: s.epoch + 1, results: s.results,
		lastGoodProvider: lastGoodProvider, lastGoodSessionID: lastGoodSessionID, lastGoodSnapshot: lastGoodSnapshot,
	}
}

func (w *worker) cancelSub2apiUsage() {
	s := &w.sub2api
	if s.pending != nil {
		s.pending.cancel()
		s.pending = nil
	}
}

func (w *worker) restoreResearchSub2apiUsage() {
	state := w.replyStatus.roundSub2apiUsage
	w.resetSub2apiUsage()
	state.epoch = w.sub2api.epoch
	state.results = w.sub2api.results
	w.sub2api = state
}

// The notification is private metadata, even when disabled or malformed.
func (w *worker) captureSub2apiRequest(e rpcEvent) bool {
	if e.Type != "extension_ui_request" {
		return false
	}
	var text string
	if json.Unmarshal(e.Message, &text) != nil {
		return false
	}
	trace, recognized := omp.ParseSub2apiNotification(e.Method, text)
	if !recognized {
		return false
	}
	if !w.providerUsageEnabled() || !w.b.cfg.Sub2API.Enabled() || w.migrationLocked() {
		return true
	}
	s := &w.sub2api
	if trace.ClientRequestID == "" {
		s.invalid, s.samples = true, nil
		return true
	}
	if !w.b.cfg.Sub2API.HasProvider(trace.Provider) || !w.taskActive() || w.client == nil ||
		trace.SessionID != w.binding.SessionID || w.client.CommandCatalog().SessionID != trace.SessionID {
		return true
	}
	if s.requests == nil {
		s.client, s.generation = w.client, w.binding.Generation
		s.catalog, s.sessionID = w.client.CommandCatalog().Epoch, trace.SessionID
		s.turn, s.inbox = w.turn, w.active
		s.requests = make(map[string]int64)
	}
	if !w.sub2apiUsageMatches(s.client, s.generation, s.catalog, s.sessionID, s.turn) {
		return true
	}
	if _, exists := s.requests[trace.ClientRequestID]; exists {
		return true
	}
	if len(s.requests) == maxSub2apiRequests {
		s.invalid, s.samples = true, nil
		return true
	}
	s.requests[trace.ClientRequestID] = 0
	s.next = time.Time{}
	s.retryExhausted = false
	s.retryNext, s.retryUntil = time.Time{}, time.Time{}
	return true
}

func (w *worker) sub2apiUsageMatches(client *omp.Client, generation int64, epoch uint64, sessionID string, turn uint64) bool {
	return client != nil && !client.Stopping() && w.turn == turn && w.providerUsageMatches(client, generation, epoch, sessionID)
}

func (w *worker) sub2apiUsageText(provider string) string {
	s := &w.sub2api
	now := time.Now()
	if !s.invalid && s.provider == provider && w.sub2apiUsageMatches(s.client, s.generation, s.catalog, s.sessionID, s.turn) {
		if text := s.sub2apiQuotaSnapshot.text(now); text != "" {
			return text
		}
	}
	if s.lastGoodProvider != provider || s.lastGoodSessionID == "" || s.lastGoodSessionID != w.binding.SessionID {
		return ""
	}
	return s.lastGoodSnapshot.text(now)
}

func (w *worker) refreshSub2apiUsage(now time.Time) {
	s := &w.sub2api
	if !w.providerUsageEnabled() || s.invalid || s.provider == "" ||
		!w.b.cfg.Sub2API.HasProvider(s.provider) || !w.sub2apiUsageMatches(s.client, s.generation, s.catalog, s.sessionID, s.turn) {
		return
	}
	if w.syncSub2apiQuota() {
		w.publishSub2apiQuota(now)
	}
	if s.pending != nil || !w.providerUsageRefreshReady() {
		return
	}
	unresolved := false
	for _, account := range s.requests {
		unresolved = unresolved || account == 0
	}
	resolve := unresolved && !s.retryExhausted
	if resolve {
		if !s.retryUntil.IsZero() && !now.Before(s.retryUntil) {
			s.retryExhausted = true
			resolve = false
		} else if now.Before(s.retryNext) {
			resolve = false
		}
	}
	if !resolve && (len(s.accounts) == 0 || now.Before(s.next)) {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, sub2apiUsageTimeout)
	r := &sub2apiUsageRequest{epoch: s.epoch, client: s.client, generation: s.generation,
		catalog: s.catalog, sessionID: s.sessionID, turn: s.turn, provider: s.provider, cancel: cancel,
		accounts: slices.Clone(s.accounts)}
	if resolve {
		for id, account := range s.requests {
			if account == 0 {
				r.unresolved = append(r.unresolved, id)
			}
		}
	}
	if s.results == nil {
		s.results = make(chan sub2apiUsageResult, 1)
	}
	s.pending = r
	results := s.results
	client, scope := w.b.sub2apiHTTP()
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer cancel()
		result := w.b.querySub2apiUsage(ctx, client, scope, r)
		select {
		case results <- result:
		case <-w.ctx.Done():
		}
	}()
}

func (b *Bridge) querySub2apiUsage(ctx context.Context, client *sub2api.Client, scope [32]byte, r *sub2apiUsageRequest) sub2apiUsageResult {
	result := sub2apiUsageResult{request: r, resolved: make(map[string]int64)}
	accounts := r.accounts
	for _, id := range r.unresolved {
		account, err := client.Resolve(ctx, id)
		if err != nil {
			if result.err == nil || errors.Is(err, sub2api.ErrNotReady) {
				result.err = err
			}
			if ctx.Err() != nil {
				break
			}
			continue
		}
		result.resolved[id] = account
		found := false
		for _, existing := range accounts {
			if existing == account {
				found = true
				break
			}
		}
		if !found {
			accounts = append(accounts, account)
		}
	}
	// Include unvisited accounts when computing the next refresh deadline.
	for i, account := range accounts {
		key := sub2apiAccountKey(scope, account)
		err := ctx.Err()
		if err != nil {
			if result.err == nil {
				result.err = err
			}
		} else {
			now := time.Now()
			if b.providerUsage.claim(key, now) {
				quota, err := client.Quota(ctx, account)
				if err != nil {
					if result.err == nil {
						result.err = err
					}
				} else {
					b.providerUsage.save(key, sub2apiQuotaSample(quota))
				}
			}
		}
		entry := b.providerUsage.load(key)
		if i == 0 || entry.next.Before(result.next) {
			result.next = entry.next
		}
	}
	return result
}

func sub2apiAccountKey(scope [32]byte, account int64) providerUsageKey {
	var identity [40]byte
	copy(identity[:32], scope[:])
	binary.BigEndian.PutUint64(identity[32:], uint64(account))
	return providerUsageKey{provider: "sub2api", account: sha256.Sum256(identity[:])}
}

// Observe shared commits even during cooldown or another worker's refresh.
func (w *worker) syncSub2apiQuota() bool {
	s := &w.sub2api
	if len(s.accounts) == 0 {
		return false
	}
	_, scope := w.b.sub2apiHTTP()
	var samples [maxSub2apiRequests]quotaSample
	n := 0
	for _, account := range s.accounts {
		entry := w.b.providerUsage.load(sub2apiAccountKey(scope, account))
		if entry.sample.available() {
			samples[n] = entry.sample
			n++
		}
	}
	unchanged := n == len(s.samples)
	for i := 0; unchanged && i < n; i++ {
		unchanged = samples[i] == s.samples[i]
	}
	if unchanged {
		return false
	}
	s.samples = append([]quotaSample(nil), samples[:n]...)
	return true
}

func sub2apiQuotaSample(q sub2api.Quota) quotaSample {
	var sample quotaSample
	if q.Plan != "" {
		sample.label = strings.ToUpper(q.Plan[:1]) + q.Plan[1:]
	} else {
		sample.label = "#" + strconv.FormatInt(q.AccountID, 10)
	}
	if q.FiveHour != nil {
		sample.windows[0] = quotaWindow{present: true, used: *q.FiveHour,
			resetAt: q.FiveHourResetAt, approximate: q.FiveHourResetApproximate}
	}
	if q.SevenDay != nil {
		sample.windows[1] = quotaWindow{present: true, used: *q.SevenDay,
			resetAt: q.SevenDayResetAt, approximate: q.SevenDayResetApproximate}
	}
	return sample
}

func (w *worker) sub2apiUsageFinished(result sub2apiUsageResult, now time.Time) {
	s := &w.sub2api
	r := result.request
	if r == nil || s.pending != r {
		return
	}
	r.cancel()
	s.pending = nil
	if s.invalid || s.epoch != r.epoch || s.provider != r.provider ||
		!w.sub2apiUsageMatches(r.client, r.generation, r.catalog, r.sessionID, r.turn) {
		return
	}
	for id, account := range result.resolved {
		if _, exists := s.requests[id]; exists {
			s.requests[id] = account
			if !slices.Contains(s.accounts, account) {
				s.accounts = append(s.accounts, account)
			}
		}
	}
	// Stable account order keeps a multi-account header from changing between reads.
	slices.Sort(s.accounts)
	w.syncSub2apiQuota()
	s.next = result.next
	if len(r.unresolved) != 0 && result.err != nil {
		if errors.Is(result.err, sub2api.ErrNotReady) {
			if s.retryUntil.IsZero() {
				s.retryUntil = now.Add(sub2apiRetryWindow)
			}
			s.retryNext = now.Add(sub2apiRetryInterval)
		} else {
			s.retryNext = now.Add(providerUsageInterval)
		}
	}
	unresolved := false
	for _, account := range s.requests {
		unresolved = unresolved || account == 0
	}
	if !unresolved {
		s.retryExhausted = false
		s.retryNext, s.retryUntil = time.Time{}, time.Time{}
	}
	w.publishSub2apiQuota(now)
}

func (w *worker) publishSub2apiQuota(now time.Time) {
	s := &w.sub2api
	if w.b.cfg.StatusBar.Has("final", "quota") && s.target != 0 && w.providerUsage.target == s.target && w.providerUsage.targetProvider == s.provider {
		fields := w.providerUsage.baseFields
		fields.quota = w.sub2apiUsageText(s.provider)
		header := w.renderStatusBar("final", fields)
		if err := w.b.db.SetAssistantHeader(w.ctx, s.target, header); err != nil && w.ctx.Err() == nil {
			w.b.storeLog.Error("provider usage header update failed", "event", "assistant_header_update_failed", "error_kind", "persistence")
		}
	}
	w.progress.NativeStatus = ""
}
