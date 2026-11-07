package omp

import (
	"encoding/json"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const sub2apiNotificationMarker = "omp-telegram.sub2api.v1 "
const maxSub2apiNotificationBytes = 2048
const maxSub2apiIdentityBytes = 128

// Sub2apiRequest identifies one live provider response, never a historical account.
type Sub2apiRequest struct {
	SessionID       string `json:"sessionId"`
	Provider        string `json:"provider"`
	ClientRequestID string `json:"clientRequestId"`
}

// ParseSub2apiNotification recognizes private metadata even when it is malformed.
// A recognized but invalid notification returns a zero request for safe suppression.
func ParseSub2apiNotification(method, message string) (Sub2apiRequest, bool) {
	if !strings.HasPrefix(message, sub2apiNotificationMarker) {
		return Sub2apiRequest{}, false
	}
	if method != "notify" || len(message) > maxSub2apiNotificationBytes || !utf8.ValidString(message) {
		return Sub2apiRequest{}, true
	}
	decoder := json.NewDecoder(strings.NewReader(message[len(sub2apiNotificationMarker):]))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return Sub2apiRequest{}, true
	}
	var request Sub2apiRequest
	var seen uint8
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return Sub2apiRequest{}, true
		}
		var target *string
		var bit uint8
		switch key {
		case "sessionId":
			target, bit = &request.SessionID, 1
		case "provider":
			target, bit = &request.Provider, 2
		case "clientRequestId":
			target, bit = &request.ClientRequestID, 4
		default:
			return Sub2apiRequest{}, true
		}
		if seen&bit != 0 || decoder.Decode(target) != nil {
			return Sub2apiRequest{}, true
		}
		seen |= bit
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || seen != 7 {
		return Sub2apiRequest{}, true
	}
	if _, err := decoder.Token(); err != io.EOF || !validSub2apiIdentity(request.SessionID) ||
		!validSub2apiIdentity(request.Provider) || !validSub2apiUUID(request.ClientRequestID) {
		return Sub2apiRequest{}, true
	}
	return request, true
}

func validSub2apiIdentity(value string) bool {
	if len(value) == 0 || len(value) > maxSub2apiIdentityBytes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}

func validSub2apiUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if value[i] != '-' {
				return false
			}
			continue
		}
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return value[14] >= '1' && value[14] <= '8' && strings.ContainsRune("89abAB", rune(value[19]))
}
