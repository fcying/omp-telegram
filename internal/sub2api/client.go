package sub2api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const maxResponseBytes = 256 << 10

var errRequest = errors.New("sub2api request failed")

// ErrNotReady means an exact usage query returned an explicitly empty first page.
var ErrNotReady = errors.New("sub2api request attribution not ready")

// Client reads request attribution and quota from one configured sub2api origin.
type Client struct {
	base   *url.URL
	apiKey string
	http   *http.Client
}

type Quota struct {
	AccountID                int64
	Plan                     string
	FiveHour                 *float64
	SevenDay                 *float64
	FiveHourResetAt          time.Time
	SevenDayResetAt          time.Time
	FiveHourResetApproximate bool
	SevenDayResetApproximate bool
}

func New(baseURL, apiKey string) *Client {
	c := &Client{
		apiKey: apiKey,
		http: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	u, err := url.Parse(baseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(baseURL, "#") ||
		strings.ContainsFunc(baseURL, unicode.IsControl) || strings.TrimSpace(baseURL) != baseURL ||
		strings.HasSuffix(u.Host, ":") || apiKey == "" || strings.ContainsFunc(apiKey, unicode.IsControl) {
		return c
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return c
		}
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return c
		}
	}
	c.base = u
	return c
}

// Resolve accepts only the uniquely attributed, exact client request ID.
func (c *Client) Resolve(ctx context.Context, requestID string) (int64, error) {
	if !validUUID(requestID) {
		return 0, errRequest
	}
	var data struct {
		Items []struct {
			RequestID string `json:"request_id"`
			AccountID int64  `json:"account_id"`
		} `json:"items"`
		Total *int `json:"total"`
		Page  *int `json:"page"`
	}
	query := url.Values{
		"request_id": {"client:" + requestID},
		"page":       {"1"},
		"page_size":  {"2"},
	}
	if err := c.get(ctx, "api/v1/admin/usage", query, &data); err != nil {
		return 0, err
	}
	if data.Total != nil && *data.Total == 0 && data.Items != nil && len(data.Items) == 0 &&
		(data.Page == nil || *data.Page == 1) {
		return 0, ErrNotReady
	}
	if data.Total == nil || *data.Total != 1 || len(data.Items) != 1 ||
		(data.Page != nil && *data.Page != 1) ||
		data.Items[0].RequestID != "client:"+requestID || data.Items[0].AccountID <= 0 {
		return 0, errRequest
	}
	return data.Items[0].AccountID, nil
}

func (c *Client) Quota(ctx context.Context, accountID int64) (Quota, error) {
	if accountID <= 0 {
		return Quota{}, errRequest
	}
	type window struct {
		Seconds    *int            `json:"limit_window_seconds"`
		Used       *float64        `json:"used_percent"`
		ResetAt    json.RawMessage `json:"reset_at"`
		ResetAfter json.RawMessage `json:"reset_after_seconds"`
	}
	var data struct {
		Plan      string          `json:"plan_type"`
		ErrorType string          `json:"error_type"`
		FetchedAt json.RawMessage `json:"fetched_at"`
		RateLimit *struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	path := "api/v1/admin/openai/accounts/" + strconv.FormatInt(accountID, 10) + "/quota"
	if err := c.get(ctx, path, nil, &data); err != nil {
		return Quota{}, err
	}
	if data.ErrorType != "" || data.RateLimit == nil {
		return Quota{}, errRequest
	}
	quota := Quota{AccountID: accountID, Plan: safePlan(data.Plan)}
	var resetAnchor time.Time
	if seconds := quotaTimestampSeconds(data.FetchedAt); seconds > 0 {
		resetAnchor = time.Unix(seconds, 0)
	}
	var seenWindow int
	for _, w := range []*window{data.RateLimit.Primary, data.RateLimit.Secondary} {
		if w == nil {
			continue
		}
		if w.Seconds == nil || w.Used == nil || math.IsNaN(*w.Used) || math.IsInf(*w.Used, 0) || *w.Used < 0 || *w.Used > 100 {
			return Quota{}, errRequest
		}
		if seenWindow == *w.Seconds {
			return Quota{}, errRequest
		}
		seenWindow = *w.Seconds
		var target **float64
		var resetAt *time.Time
		var approximate *bool
		switch *w.Seconds {
		case 18000:
			target = &quota.FiveHour
			resetAt = &quota.FiveHourResetAt
			approximate = &quota.FiveHourResetApproximate
		case 604800:
			target = &quota.SevenDay
			resetAt = &quota.SevenDayResetAt
			approximate = &quota.SevenDayResetApproximate
		default:
			continue
		}
		*target = w.Used
		if seconds := quotaTimestampSeconds(w.ResetAt); seconds > 0 {
			*resetAt = time.Unix(seconds, 0)
		} else if seconds := quotaTimestampSeconds(w.ResetAfter); seconds > 0 {
			if resetAnchor.IsZero() {
				resetAnchor = time.Now()
			}
			if seconds <= maxQuotaUnixSeconds-resetAnchor.Unix() {
				*resetAt = time.Unix(resetAnchor.Unix()+seconds, int64(resetAnchor.Nanosecond()))
				*approximate = true
			}
		}
	}
	return quota, nil
}

// time.Time stores signed seconds from year 1, before the Unix epoch.
const maxQuotaUnixSeconds = int64(math.MaxInt64) - 62135596800

func quotaTimestampSeconds(raw json.RawMessage) int64 {
	var seconds int64
	if json.Unmarshal(raw, &seconds) != nil || seconds <= 0 || seconds > maxQuotaUnixSeconds {
		return 0
	}
	return seconds
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	if c == nil || c.base == nil {
		return errRequest
	}
	u := c.base.JoinPath(path)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errRequest
	}
	req.Header.Set("x-api-key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errRequest
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errRequest
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errRequest
	}
	if len(body) > maxResponseBytes {
		return errRequest
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Code == nil || *envelope.Code != 0 ||
		len(envelope.Data) == 0 || string(envelope.Data) == "null" || json.Unmarshal(envelope.Data, out) != nil {
		return errRequest
	}
	return nil
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return false
			}
			continue
		}
		if !((s[i] >= '0' && s[i] <= '9') || (s[i] >= 'a' && s[i] <= 'f') || (s[i] >= 'A' && s[i] <= 'F')) {
			return false
		}
	}
	return s[14] >= '1' && s[14] <= '8' && strings.ContainsRune("89aAbB", rune(s[19]))
}

func safePlan(plan string) string {
	// Only known plan labels may leave the upstream; never forward account identity.
	switch plan {
	case "free", "go", "plus", "pro", "prolite", "team", "business", "enterprise", "edu":
		return plan
	default:
		return ""
	}
}
