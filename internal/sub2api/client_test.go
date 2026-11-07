package sub2api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testRequestID = "12345678-1234-4234-8234-123456789abc"

func TestResolveRequiresUniqueExactAttribution(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want int64
	}{
		{"exact", `{"items":[{"request_id":"client:` + testRequestID + `","account_id":42}],"total":1,"page":1}`, 42},
		{"other request", `{"items":[{"request_id":"client:other","account_id":42}],"total":1}`, 0},
		{"different case", `{"items":[{"request_id":"client:` + strings.ToUpper(testRequestID) + `","account_id":42}],"total":1}`, 0},
		{"raw ID", `{"items":[{"request_id":"` + testRequestID + `","account_id":42}],"total":1}`, 0},
		{"substring", `{"items":[{"request_id":"client:` + testRequestID + `-suffix","account_id":42}],"total":1}`, 0},
		{"ambiguous total", `{"items":[{"request_id":"client:` + testRequestID + `","account_id":42}],"total":2}`, 0},
		{"ambiguous rows", `{"items":[{"request_id":"client:` + testRequestID + `","account_id":42},{"request_id":"client:` + testRequestID + `","account_id":43}],"total":1}`, 0},
		{"missing", `{"items":[],"total":0}`, 0},
		{"missing total", `{"items":[{"request_id":"client:` + testRequestID + `","account_id":42}]}`, 0},
		{"null row", `{"items":[null],"total":1}`, 0},
		{"zero account", `{"items":[{"request_id":"client:` + testRequestID + `","account_id":0}],"total":1}`, 0},
		{"negative account", `{"items":[{"request_id":"client:` + testRequestID + `","account_id":-42}],"total":1}`, 0},
		{"malformed account", `{"items":[{"request_id":"client:` + testRequestID + `","account_id":"42"}],"total":1}`, 0},
		{"wrong page", `{"items":[{"request_id":"client:` + testRequestID + `","account_id":42}],"total":1,"page":2}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/proxy/api/v1/admin/usage" ||
					r.URL.Query().Get("request_id") != "client:"+testRequestID ||
					r.URL.Query().Get("page") != "1" || r.URL.Query().Get("page_size") != "2" ||
					len(r.URL.Query()) != 3 || r.Header.Get("x-api-key") != "test-key" || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected request protocol: %s %s", r.Method, r.URL.Path)
				}
				fmt.Fprint(w, `{"code":0,"data":`+tc.data+`}`)
			}))
			defer server.Close()
			got, err := New(server.URL+"/proxy/", "test-key").Resolve(context.Background(), testRequestID)
			if got != tc.want || (err != nil) != (tc.want == 0) {
				t.Fatalf("Resolve = %d, %v; want account %d", got, err, tc.want)
			}
			if requests.Load() != 1 {
				t.Fatalf("requests = %d, want one without retries", requests.Load())
			}
		})
	}
}

func TestResolveNotReadyRequiresExplicitEmptySuccessfulPage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"empty first page", http.StatusOK, `{"code":0,"data":{"total":0,"items":[],"page":1}}`, true},
		{"empty without page", http.StatusOK, `{"code":0,"data":{"total":0,"items":[]}}`, true},
		{"null rows", http.StatusOK, `{"code":0,"data":{"total":0,"items":null}}`, false},
		{"missing rows", http.StatusOK, `{"code":0,"data":{"total":0}}`, false},
		{"missing total", http.StatusOK, `{"code":0,"data":{"items":[]}}`, false},
		{"wrong page", http.StatusOK, `{"code":0,"data":{"total":0,"items":[],"page":2}}`, false},
		{"inconsistent total", http.StatusOK, `{"code":0,"data":{"total":1,"items":[]}}`, false},
		{"malformed total", http.StatusOK, `{"code":0,"data":{"total":"0","items":[]}}`, false},
		{"forbidden", http.StatusForbidden, `{"code":0,"data":{"total":0,"items":[]}}`, false},
		{"API failure", http.StatusOK, `{"code":403,"data":{"total":0,"items":[]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			_, err := New(server.URL, "private-key").Resolve(context.Background(), testRequestID)
			if err == nil || errors.Is(err, ErrNotReady) != tc.want {
				t.Fatalf("Resolve error = %v; want not ready %t", err, tc.want)
			}
		})
	}
}

func TestQuotaUsesWindowDurationAndPreservesMissingValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  string
		plan  string
		five  *float64
		seven *float64
	}{
		{"reversed", `{"plan_type":"plus","rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":37.5},"secondary_window":{"limit_window_seconds":18000,"used_percent":100}}}`, "plus", new(float64(100)), new(37.5)},
		{"prolite seven only", `{"plan_type":"prolite","rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":4.25}}}`, "prolite", nil, new(4.25)},
		{"zero is present", `{"plan_type":"free","rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":0}}}`, "free", new(float64(0)), nil},
		{"absent", `{"plan_type":"pro","rate_limit":{}}`, "pro", nil, nil},
		{"unsupported duration", `{"rate_limit":{"primary_window":{"limit_window_seconds":3600,"used_percent":9}}}`, "", nil, nil},
		{"private plan omitted", `{"plan_type":"private-user@example.com secret","rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":12}}}`, "", new(float64(12)), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/prefix/api/v1/admin/openai/accounts/42/quota" || r.URL.RawQuery != "" {
					t.Errorf("unexpected quota request: %s %s", r.Method, r.URL.Path)
				}
				fmt.Fprint(w, `{"code":0,"data":`+tc.data+`}`)
			}))
			defer server.Close()
			got, err := New(server.URL+"/prefix", "test-key").Quota(context.Background(), 42)
			if err != nil || got.AccountID != 42 || got.Plan != tc.plan || !sameFloat(got.FiveHour, tc.five) || !sameFloat(got.SevenDay, tc.seven) {
				t.Fatalf("Quota = %+v, %v; want plan %q, five %v, seven %v", got, err, tc.plan, tc.five, tc.seven)
			}
		})
	}
}

func TestQuotaResetObservations(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fetched     string
		five        string
		seven       string
		swapped     bool
		fiveAt      int64
		sevenAt     int64
		fiveApprox  bool
		sevenApprox bool
	}{
		{
			name: "absolute seconds preferred and elapsed preserved", fetched: `1700000000`,
			five:   `,"reset_at":1700003600,"reset_after_seconds":7200`,
			seven:  `,"reset_at":1,"reset_after_seconds":7200`,
			fiveAt: 1700003600, sevenAt: 1,
		},
		{
			name: "relative uses fetched seconds", fetched: `1700000000`,
			five: `,"reset_after_seconds":3600`, seven: `,"reset_after_seconds":7200`,
			fiveAt: 1700003600, sevenAt: 1700007200, fiveApprox: true, sevenApprox: true,
		},
		{
			name: "swapped windows retain their resets", fetched: `1700000000`, swapped: true,
			five: `,"reset_at":1700003600`, seven: `,"reset_after_seconds":7200`,
			fiveAt: 1700003600, sevenAt: 1700007200, sevenApprox: true,
		},
		{
			name: "invalid absolute permits relative fallback", fetched: `1700000000`,
			five:   `,"reset_at":"invalid","reset_after_seconds":3600`,
			seven:  `,"reset_at":null,"reset_after_seconds":7200`,
			fiveAt: 1700003600, sevenAt: 1700007200, fiveApprox: true, sevenApprox: true,
		},
		{name: "missing reset does not use window duration"},
		{
			name: "malformed optional fields preserve percentages", fetched: `{"invalid":true}`,
			five:  `,"reset_at":"1700003600","reset_after_seconds":{}`,
			seven: `,"reset_at":[],"reset_after_seconds":true`,
		},
		{
			name: "zero and negative are absent", fetched: `1700000000`,
			five:  `,"reset_at":0,"reset_after_seconds":0`,
			seven: `,"reset_at":-1,"reset_after_seconds":-1`,
		},
		{
			name: "fractional seconds are ignored", fetched: `1700000000`,
			five:  `,"reset_at":1700003600.5,"reset_after_seconds":0.5`,
			seven: `,"reset_at":null,"reset_after_seconds":null`,
		},
		{
			name: "integer and time storage overflow are ignored", fetched: `1700000000`,
			five:  `,"reset_at":9223372036854775808,"reset_after_seconds":1e999`,
			seven: `,"reset_at":9223372036854775807,"reset_after_seconds":9223372036854775807`,
		},
		{
			name: "relative addition cannot overflow", fetched: `9223371974719179006`,
			five: `,"reset_after_seconds":2`, seven: `,"reset_after_seconds":9223371974719179007`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := `{"limit_window_seconds":18000,"used_percent":20` + tc.five + `}`
			secondary := `{"limit_window_seconds":604800,"used_percent":60` + tc.seven + `}`
			if tc.swapped {
				primary, secondary = secondary, primary
			}
			fetched := ""
			if tc.fetched != "" {
				fetched = `"fetched_at":` + tc.fetched + `,`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"code":0,"data":{`+fetched+`"rate_limit":{"primary_window":`+primary+`,"secondary_window":`+secondary+`}}}`)
			}))
			defer server.Close()
			got, err := New(server.URL, "test-key").Quota(context.Background(), 42)
			if err != nil || got.AccountID != 42 || !sameFloat(got.FiveHour, new(float64(20))) || !sameFloat(got.SevenDay, new(float64(60))) {
				t.Fatalf("quota percentages lost: %+v, %v", got, err)
			}
			for _, window := range []struct {
				name       string
				at         time.Time
				approx     bool
				wantAt     int64
				wantApprox bool
			}{
				{"five hour", got.FiveHourResetAt, got.FiveHourResetApproximate, tc.fiveAt, tc.fiveApprox},
				{"seven day", got.SevenDayResetAt, got.SevenDayResetApproximate, tc.sevenAt, tc.sevenApprox},
			} {
				wantAt := time.Time{}
				if window.wantAt > 0 {
					wantAt = time.Unix(window.wantAt, 0)
				}
				if !window.at.Equal(wantAt) || window.approx != window.wantApprox {
					t.Errorf("%s reset = %v, approximate %t; want %v, approximate %t", window.name, window.at, window.approx, wantAt, window.wantApprox)
				}
			}
		})
	}
}

func TestQuotaRelativeResetFallsBackToOneReceiptTime(t *testing.T) {
	for _, fetched := range []string{"", `,"fetched_at":null`, `,"fetched_at":"invalid"`, `,"fetched_at":0`, `,"fetched_at":-1`, `,"fetched_at":9223372036854775808`} {
		t.Run(fetched, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"code":0,"data":{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":20,"reset_after_seconds":3600},"secondary_window":{"limit_window_seconds":604800,"used_percent":60,"reset_after_seconds":7200}}`+fetched+`}}`)
			}))
			defer server.Close()
			start := time.Now()
			got, err := New(server.URL, "test-key").Quota(context.Background(), 42)
			end := time.Now()
			if err != nil || !got.FiveHourResetApproximate || !got.SevenDayResetApproximate {
				t.Fatalf("relative reset = %+v, %v", got, err)
			}
			anchor := got.FiveHourResetAt.Add(-time.Hour)
			if anchor.Before(start) || anchor.After(end) || !got.SevenDayResetAt.Add(-2*time.Hour).Equal(anchor) {
				t.Fatalf("reset anchors must share receipt in [%v, %v]: %v, %v", start, end, got.FiveHourResetAt, got.SevenDayResetAt)
			}
		})
	}
}

func TestQuotaRejectsMalformedAndUnavailableData(t *testing.T) {
	for _, data := range []string{
		`null`,
		`{}`,
		`{"error_type":"unauthorized","rate_limit":{}}`,
		`{"error_type":"missingquota","balance":1000,"rate_limit":{}}`,
		`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":10},"secondary_window":{"limit_window_seconds":18000,"used_percent":20}}}`,
		`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":10},"secondary_window":{"limit_window_seconds":604800,"used_percent":20}}}`,
		`{"rate_limit":{"primary_window":{"limit_window_seconds":3600,"used_percent":10},"secondary_window":{"limit_window_seconds":3600,"used_percent":20}}}`,
		`{"rate_limit":{"primary_window":{"used_percent":10}}}`,
		`{"rate_limit":{"primary_window":{"limit_window_seconds":18000}}}`,
		`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":null}}}`,
		`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":-0.01}}}`,
		`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":100.01}}}`,
		`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":"12"}}}`,
		`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":1e999}}}`,
		`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":NaN}}}`,
	} {
		t.Run(data, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"code":0,"data":`+data+`}`)
			}))
			defer server.Close()
			got, err := New(server.URL, "secret-key").Quota(context.Background(), 42)
			if err == nil || got != (Quota{}) {
				t.Fatalf("invalid quota accepted: %+v, %v", got, err)
			}
		})
	}
}

func TestClientRefusesRedirectsWithoutForwardingSecrets(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		if r.Header.Get("x-api-key") != "" {
			t.Error("redirect forwarded API key")
		}
	}))
	defer target.Close()
	for _, crossOrigin := range []bool{false, true} {
		t.Run(fmt.Sprint(crossOrigin), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirect-target" {
					forwarded.Add(1)
					return
				}
				location := "/redirect-target"
				if crossOrigin {
					location = target.URL
				}
				http.Redirect(w, r, location, http.StatusFound)
			}))
			defer server.Close()
			if _, err := New(server.URL, "private-key").Resolve(context.Background(), testRequestID); err == nil {
				t.Fatal("redirect accepted")
			}
		})
	}
	if forwarded.Load() != 0 {
		t.Fatalf("redirect targets received %d requests", forwarded.Load())
	}
}

func TestClientHonorsInFlightCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := New(server.URL, "private-key").Resolve(ctx, testRequestID)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request ignored cancellation")
	}
}

func TestClientErrorsRemainPrivateAndResponsesAreBounded(t *testing.T) {
	const secret = "PRIVATE-UPSTREAM-CREDENTIAL-AND-IDENTITY"
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, secret},
		{"API error", http.StatusOK, `{"code":401,"message":"` + secret + `","data":null}`},
		{"malformed", http.StatusOK, secret},
		{"no code", http.StatusOK, `{"message":"` + secret + `","data":{}}`},
		{"trailing JSON", http.StatusOK, `{"code":0,"data":{}} {"secret":"` + secret + `"}`},
		{"oversize", http.StatusOK, `{"code":0,"data":{"private":"` + strings.Repeat("x", maxResponseBytes) + secret + `"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			_, err := New(server.URL, secret).Resolve(context.Background(), testRequestID)
			assertPrivateError(t, err, secret, server.URL, testRequestID)
		})
	}
	client := New("https://upstream.invalid/private-path", secret)
	client.http.Transport = failingTransport{err: errors.New("https://" + secret + "@upstream.invalid/private-path " + testRequestID)}
	_, err := client.Resolve(context.Background(), testRequestID)
	assertPrivateError(t, err, secret, "upstream.invalid", testRequestID)
}

func TestClientRejectsInvalidInputsBeforeNetworkAccess(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	for _, base := range []string{
		"", "ftp://example.com", "http://", "http://example.com:99999", "http://example.com:",
		"http://user:password@" + strings.TrimPrefix(server.URL, "http://"),
		server.URL + "?private=1", server.URL + "?", server.URL + "#private", server.URL + "#",
		server.URL + "/prefix/../escape", server.URL + "/prefix/%2e%2e/escape", server.URL + "\n",
	} {
		_, err := New(base, "private-key").Resolve(context.Background(), testRequestID)
		if err == nil {
			t.Errorf("accepted invalid base URL %q", base)
		}
	}
	client := New(server.URL, "private-key")
	for _, id := range []string{"", "client:" + testRequestID, testRequestID + "suffix", "12345678-1234-0234-8234-123456789abc", "12345678-1234-4234-0234-123456789abc"} {
		if _, err := client.Resolve(context.Background(), id); err == nil {
			t.Errorf("accepted invalid request ID %q", id)
		}
	}
	for _, account := range []int64{0, -1} {
		if _, err := client.Quota(context.Background(), account); err == nil {
			t.Errorf("accepted invalid account %d", account)
		}
	}
	if _, err := New(server.URL, "").Resolve(context.Background(), testRequestID); err == nil {
		t.Error("accepted missing API key")
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid inputs caused %d requests", requests.Load())
	}
}

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

func assertPrivateError(t *testing.T, err error, private ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected private error")
	}
	for _, text := range private {
		if strings.Contains(err.Error(), text) {
			t.Fatalf("error contains private input: %v", err)
		}
	}
}

func sameFloat(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
