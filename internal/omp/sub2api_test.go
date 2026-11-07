package omp

import (
	"encoding/json"
	"strings"
	"testing"
)

func sub2apiTestNotification(t *testing.T, request Sub2apiRequest) string {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal("cannot encode test metadata")
	}
	return sub2apiNotificationMarker + string(data)
}

func TestParseSub2apiNotificationValid(t *testing.T) {
	for _, request := range []Sub2apiRequest{
		{SessionID: "session-current", Provider: "relay-codex", ClientRequestID: "6ba7b810-9dad-41d1-80b4-00c04fd430c8"},
		{SessionID: "session-next", Provider: "openai-codex", ClientRequestID: "6BA7B810-9DAD-81D1-B0B4-00C04FD430C8"},
		{SessionID: strings.Repeat("s", 128), Provider: strings.Repeat("p", 128), ClientRequestID: "6ba7b810-9dad-11d1-90b4-00c04fd430c8"},
		{SessionID: strings.Repeat("会", 42) + "ss", Provider: strings.Repeat("中", 42) + "pp", ClientRequestID: "6ba7b810-9dad-71d1-a0b4-00c04fd430c8"},
	} {
		got, recognized := ParseSub2apiNotification("notify", sub2apiTestNotification(t, request))
		if !recognized || got != request {
			t.Fatal("valid response metadata was not preserved")
		}
	}
}

func TestParseSub2apiNotificationRecognition(t *testing.T) {
	for _, message := range []string{
		"ordinary notification",
		"prefix " + sub2apiNotificationMarker + "{}",
		"omp-telegram.sub2api.v2 {}",
		"omp-telegram.sub2api.v1",
	} {
		got, recognized := ParseSub2apiNotification("notify", message)
		if recognized || got != (Sub2apiRequest{}) {
			t.Fatal("ordinary notification was consumed")
		}
	}
	for _, method := range []string{"", "setStatus", "confirm", "Notify"} {
		got, recognized := ParseSub2apiNotification(method, sub2apiNotificationMarker+`{"private":"redacted"}`)
		if !recognized || got != (Sub2apiRequest{}) {
			t.Fatal("private marker was not suppressed for an invalid method")
		}
	}
}

func TestParseSub2apiNotificationInvalidPayload(t *testing.T) {
	const valid = `"sessionId":"current-session","provider":"relay","clientRequestId":"6ba7b810-9dad-41d1-80b4-00c04fd430c8"`
	for name, payload := range map[string]string{
		"empty":            "",
		"null":             "null",
		"array":            "[]",
		"string":           `"private payload"`,
		"empty object":     "{}",
		"missing session":  `{"provider":"relay","clientRequestId":"6ba7b810-9dad-41d1-80b4-00c04fd430c8"}`,
		"wrong field case": `{"SessionID":"current-session","provider":"relay","clientRequestId":"6ba7b810-9dad-41d1-80b4-00c04fd430c8"}`,
		"null field":       `{"sessionId":null,"provider":"relay","clientRequestId":"6ba7b810-9dad-41d1-80b4-00c04fd430c8"}`,
		"object field":     `{"sessionId":{},"provider":"relay","clientRequestId":"6ba7b810-9dad-41d1-80b4-00c04fd430c8"}`,
		"number field":     `{"sessionId":42,"provider":"relay","clientRequestId":"6ba7b810-9dad-41d1-80b4-00c04fd430c8"}`,
		"duplicate field":  `{` + valid + `,"provider":"other"}`,
		"private headers":  `{` + valid + `,"headers":{"authorization":"private credential"}}`,
		"private key":      `{` + valid + `,"apiKey":"private credential"}`,
		"unknown field":    `{` + valid + `,"future":true}`,
		"trailing object":  `{` + valid + `}{}`,
		"trailing secret":  `{` + valid + `}private credential`,
		"truncated":        `{` + valid,
		"invalid utf8":     `{` + valid + ",\"\xff\":null}",
		"oversized":        strings.Repeat("x", maxSub2apiNotificationBytes),
	} {
		t.Run(name, func(t *testing.T) {
			got, recognized := ParseSub2apiNotification("notify", sub2apiNotificationMarker+payload)
			if !recognized || got != (Sub2apiRequest{}) {
				t.Fatal("invalid private metadata was not fully suppressed")
			}
		})
	}
}

func TestParseSub2apiNotificationInvalidIdentifiers(t *testing.T) {
	valid := Sub2apiRequest{
		SessionID: "current-session", Provider: "relay",
		ClientRequestID: "6ba7b810-9dad-41d1-80b4-00c04fd430c8",
	}
	for name, value := range map[string]string{
		"empty": "", "space": " ", "leading space": " relay", "inner space": "some relay",
		"newline": "relay\n", "nul": "relay\x00", "delete": "relay\x7f",
		"unicode control": "relay\u0085", "unicode format": "relay\u200b", "unicode separator": "relay\u2028",
		"too long": strings.Repeat("x", 129), "too many bytes": strings.Repeat("会", 43),
	} {
		for _, field := range []string{"session", "provider"} {
			t.Run(field+"/"+name, func(t *testing.T) {
				request := valid
				if field == "session" {
					request.SessionID = value
				} else {
					request.Provider = value
				}
				got, recognized := ParseSub2apiNotification("notify", sub2apiTestNotification(t, request))
				if !recognized || got != (Sub2apiRequest{}) {
					t.Fatal("invalid identity was not fully suppressed")
				}
			})
		}
	}
	for name, value := range map[string]string{
		"empty": "", "short": "6ba7b810", "prefix": "client:" + valid.ClientRequestID,
		"space": valid.ClientRequestID + " ", "newline": valid.ClientRequestID + "\n",
		"non hex":      "6ba7b81z-9dad-41d1-80b4-00c04fd430c8",
		"compact":      "6ba7b8109dad41d180b400c04fd430c8",
		"nil":          "00000000-0000-0000-0000-000000000000",
		"version zero": "6ba7b810-9dad-01d1-80b4-00c04fd430c8",
		"version nine": "6ba7b810-9dad-91d1-80b4-00c04fd430c8",
		"variant":      "6ba7b810-9dad-41d1-70b4-00c04fd430c8",
	} {
		t.Run("uuid/"+name, func(t *testing.T) {
			request := valid
			request.ClientRequestID = value
			got, recognized := ParseSub2apiNotification("notify", sub2apiTestNotification(t, request))
			if !recognized || got != (Sub2apiRequest{}) {
				t.Fatal("invalid request UUID was not fully suppressed")
			}
		})
	}
}

func TestParseSub2apiNotificationSizeBoundary(t *testing.T) {
	request := Sub2apiRequest{
		SessionID: "current-session", Provider: "relay",
		ClientRequestID: "6ba7b810-9dad-41d1-80b4-00c04fd430c8",
	}
	message := sub2apiTestNotification(t, request)
	message += strings.Repeat(" ", maxSub2apiNotificationBytes-len(message))
	got, recognized := ParseSub2apiNotification("notify", message)
	if !recognized || got != request {
		t.Fatal("notification at byte limit was rejected")
	}
	got, recognized = ParseSub2apiNotification("notify", message+" ")
	if !recognized || got != (Sub2apiRequest{}) {
		t.Fatal("oversized private metadata was not fully suppressed")
	}
}
