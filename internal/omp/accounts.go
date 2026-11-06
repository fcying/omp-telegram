package omp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxAccountScopeLabel  = 512
	maxAccountScopeDetail = 2048
)

// ProviderAccountScope privately identifies the unique active OAuth account.
// It is a read-only snapshot, not an atomic binding between an account and usage.
func (c *Client) ProviderAccountScope(ctx context.Context, provider string) ([32]byte, error) {
	var unavailable [32]byte
	if err := ctx.Err(); err != nil {
		return unavailable, err
	}
	if provider != "anthropic" && provider != "openai-codex" {
		return unavailable, errors.New("omp: provider account scope unavailable")
	}
	raw, err := c.Call(ctx, "get_logout_accounts", map[string]any{"providerId": provider})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return unavailable, ctxErr
	}
	if err != nil {
		// Do not expose native diagnostics or account fields through this API.
		if errors.Is(err, context.Canceled) {
			return unavailable, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return unavailable, context.DeadlineExceeded
		}
		return unavailable, errors.New("omp: provider account scope unavailable")
	}
	var response struct {
		Accounts []struct {
			CredentialID int64  `json:"credentialId"`
			Provider     string `json:"provider"`
			Label        string `json:"label"`
			Detail       string `json:"detail"`
			Type         string `json:"type"`
			Active       bool   `json:"active"`
		} `json:"accounts"`
	}
	if !utf8.Valid(raw) || json.Unmarshal(raw, &response) != nil {
		return unavailable, errors.New("omp: provider account scope unavailable")
	}
	active := -1
	for i, account := range response.Accounts {
		if !account.Active {
			continue
		}
		if active != -1 {
			return unavailable, errors.New("omp: provider account scope unavailable")
		}
		active = i
	}
	if active == -1 {
		return unavailable, errors.New("omp: provider account scope unavailable")
	}
	account := response.Accounts[active]
	if account.Provider != provider || account.Type != "oauth" || account.CredentialID <= 0 ||
		!reliableAccountScopeLabel(account.Label) || !validAccountScopeText(account.Detail, maxAccountScopeDetail) {
		return unavailable, errors.New("omp: provider account scope unavailable")
	}

	// Fixed-width lengths make field boundaries unambiguous without retaining PII.
	hash := sha256.New()
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(len(provider)))
	hash.Write(encoded[:])
	hash.Write([]byte(provider))
	binary.BigEndian.PutUint64(encoded[:], uint64(account.CredentialID))
	hash.Write(encoded[:])
	for _, field := range [...]string{account.Label, account.Detail} {
		binary.BigEndian.PutUint64(encoded[:], uint64(len(field)))
		hash.Write(encoded[:])
		hash.Write([]byte(field))
	}
	var scope [32]byte
	hash.Sum(scope[:0])
	if err := ctx.Err(); err != nil {
		return unavailable, err
	}
	return scope, nil
}

func validAccountScopeText(text string, maxBytes int) bool {
	if len(text) > maxBytes || !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return false
		}
	}
	return true
}

func reliableAccountScopeLabel(label string) bool {
	if label == "" || strings.TrimSpace(label) != label || !validAccountScopeText(label, maxAccountScopeLabel) {
		return false
	}
	lower := strings.ToLower(label)
	for _, placeholder := range [...]string{"oauth", "oauth credential", "oauth account", "credential", "account", "unknown", "unknown account", "default"} {
		if lower == placeholder || strings.HasPrefix(lower, placeholder+" #") {
			return false
		}
	}
	return true
}
