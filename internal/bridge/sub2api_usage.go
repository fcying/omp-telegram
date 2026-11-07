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

const maxSub2apiRequests = 64

var errSub2apiCachePending = errors.New("sub2api quota not ready")

type sub2apiBridge struct {
	once   sync.Once
	client *sub2api.Client
	scope  [32]byte
}

type sub2apiQuotaSnapshot struct {
	samples []quotaSample
	next    time.Time
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
	epoch      uint64
	client     *omp.Client
	generation int64
	catalog    uint64
	sessionID  string
	turn       uint64
	inbox      int64
	target     int64
	provider   string
	requests   map[string]int64
	invalid    bool
	sub2apiQuotaSnapshot
	attempts int
	pending  *sub2apiUsageRequest
	results  chan sub2apiUsageResult
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
	sub2apiQuotaSnapshot
	err error
}

func (w *worker) resetSub2apiUsage() {
	s := &w.sub2api
	if s.pending != nil {
		s.pending.cancel()
	}
	w.sub2api = sub2apiUsageState{epoch: s.epoch + 1, results: s.results}
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
	if s.provider == "" {
		s.client, s.generation = w.client, w.binding.Generation
		s.catalog, s.sessionID = w.client.CommandCatalog().Epoch, trace.SessionID
		s.turn, s.inbox, s.provider = w.turn, w.active, trace.Provider
		s.requests = make(map[string]int64)
	}
	if !w.sub2apiUsageMatches(s.client, s.generation, s.catalog, s.sessionID, s.turn) {
		return true
	}
	if s.provider != trace.Provider {
		s.invalid, s.samples = true, nil
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
	s.samples, s.next, s.attempts = nil, time.Time{}, 0
	w.refreshSub2apiUsage(time.Now())
	return true
}

func (w *worker) sub2apiUsageMatches(client *omp.Client, generation int64, epoch uint64, sessionID string, turn uint64) bool {
	return client != nil && !client.Stopping() && w.turn == turn && w.providerUsageMatches(client, generation, epoch, sessionID)
}

func (w *worker) sub2apiUsageText(provider string) string {
	s := &w.sub2api
	if s.invalid || s.provider != provider || !w.sub2apiUsageMatches(s.client, s.generation, s.catalog, s.sessionID, s.turn) {
		return ""
	}
	return s.sub2apiQuotaSnapshot.text(time.Now())
}

func (w *worker) refreshSub2apiUsage(now time.Time) {
	s := &w.sub2api
	if !w.providerUsageEnabled() || s.invalid || s.provider == "" || s.pending != nil || now.Before(s.next) ||
		!w.b.cfg.Sub2API.HasProvider(s.provider) || !w.sub2apiUsageMatches(s.client, s.generation, s.catalog, s.sessionID, s.turn) {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, replyUsageTimeout)
	r := &sub2apiUsageRequest{epoch: s.epoch, client: s.client, generation: s.generation,
		catalog: s.catalog, sessionID: s.sessionID, turn: s.turn, provider: s.provider, cancel: cancel}
	seen := make(map[int64]bool)
	for id, account := range s.requests {
		if account == 0 {
			r.unresolved = append(r.unresolved, id)
		} else if !seen[account] {
			r.accounts = append(r.accounts, account)
			seen[account] = true
		}
	}
	if s.results == nil {
		s.results = make(chan sub2apiUsageResult, 1)
	}
	s.pending, s.samples = r, nil
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
			result.err = err
			return result
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
	// Stable account order keeps a multi-account header from changing between reads.
	slices.Sort(accounts)
	result.samples = make([]quotaSample, 0, len(accounts))
	for _, account := range accounts {
		var identity [40]byte
		copy(identity[:32], scope[:])
		binary.BigEndian.PutUint64(identity[32:], uint64(account))
		key := providerUsageKey{provider: "sub2api", account: sha256.Sum256(identity[:])}
		now := time.Now()
		entry, claimed := b.claimSub2apiQuota(key, now)
		if claimed {
			quota, err := client.Quota(ctx, account)
			if err != nil {
				result.err = err
				return result
			}
			b.providerUsage.save(key, sub2apiQuotaSample(quota))
			entry = b.providerUsage.load(key)
		}
		if !entry.sample.available() {
			result.err = errSub2apiCachePending
			return result
		}
		if result.next.IsZero() || entry.next.Before(result.next) {
			result.next = entry.next
		}
		result.samples = append(result.samples, entry.sample)
	}
	return result
}

// Clear expired data atomically with the refresh claim, including failed reads.
func (b *Bridge) claimSub2apiQuota(key providerUsageKey, now time.Time) (providerUsageEntry, bool) {
	c := &b.providerUsage
	c.Lock()
	defer c.Unlock()
	entry := c.entries[key]
	if now.Before(entry.next) {
		return entry, false
	}
	if c.entries == nil {
		c.entries = make(map[providerUsageKey]providerUsageEntry)
	}
	entry = providerUsageEntry{next: now.Add(providerUsageInterval)}
	c.entries[key] = entry
	return entry, true
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

func (w *worker) sub2apiUsageFinished(result sub2apiUsageResult) {
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
		}
	}
	if result.err != nil {
		s.attempts++
		s.next = time.Now().Add(providerUsageInterval)
		if s.attempts < 3 && (errors.Is(result.err, sub2api.ErrNotReady) || errors.Is(result.err, errSub2apiCachePending)) {
			s.next = time.Now().Add(1500 * time.Millisecond)
		}
		return
	}
	for _, account := range s.requests {
		if account == 0 {
			w.refreshSub2apiUsage(time.Now())
			return
		}
	}
	s.sub2apiQuotaSnapshot = result.sub2apiQuotaSnapshot
	if w.research.root && s.target != 0 && s.target == w.replyStatus.roundStatusOutboxID {
		w.replyStatus.roundSub2apiQuota = s.sub2apiQuotaSnapshot
	}
	if w.b.cfg.StatusBar.Has("final", "quota") && s.target != 0 && w.providerUsage.target == s.target && w.providerUsage.targetProvider == s.provider {
		fields := w.providerUsage.baseFields
		fields.quota = s.sub2apiQuotaSnapshot.text(time.Now())
		header := w.renderStatusBar("final", fields)
		if err := w.b.db.SetAssistantHeader(w.ctx, s.target, header); err != nil && w.ctx.Err() == nil {
			w.b.storeLog.Error("provider usage header update failed", "event", "assistant_header_update_failed", "error_kind", "persistence")
		}
	}
	w.progress.NativeStatus = ""
}
