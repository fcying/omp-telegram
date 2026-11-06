package omp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

var fixtureAccountScopeQueries int

func fixtureAccountScopeCommand(command map[string]any) bool {
	mode := os.Getenv("OMP_TEST_ACCOUNT_SCOPE_MODE")
	if mode == "" || command["type"] != "get_logout_accounts" {
		return false
	}
	fixtureAccountScopeQueries++
	emit := func(value any) { json.NewEncoder(os.Stdout).Encode(value) }
	reply := func(data any) {
		emit(map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": true, "data": data})
	}
	if mode == "waiting" {
		emit(map[string]any{"type": "account_scope_waiting"})
		return true
	}
	if mode == "rejected" {
		emit(map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": false, "error": "SECRET_ACCOUNT_REJECTION secret-account@example.test 918273645"})
		return true
	}
	if mode == "malformed" {
		reply(map[string]any{"accounts": "secret-account@example.test"})
		return true
	}
	account := map[string]any{
		"credentialId": 918273645,
		"provider":     command["providerId"],
		"label":        "secret-account@example.test",
		"detail":       "sensitive-plan",
		"type":         "oauth",
		"active":       true,
	}
	switch mode {
	case "rotate-id":
		if fixtureAccountScopeQueries > 1 {
			account["credentialId"] = 918273646
		}
	case "rotate-label":
		if fixtureAccountScopeQueries > 1 {
			account["label"] = "other-account@example.test"
		}
	case "rotate-detail":
		if fixtureAccountScopeQueries > 1 {
			account["detail"] = "other-plan"
		}
	case "boundaries":
		account["label"], account["detail"] = "private-label", "-detail"
		if fixtureAccountScopeQueries > 1 {
			account["label"], account["detail"] = "private-label-", "detail"
		}
	case "inactive":
		account["active"] = false
	case "provider-mismatch":
		account["provider"] = "unrelated-provider"
	case "placeholder":
		account["label"] = "OAuth credential #918273645"
	case "generic-label":
		account["label"] = "Unknown account"
	case "blank-label":
		account["label"] = " \t "
	case "empty-label":
		account["label"] = ""
	case "long-label":
		account["label"] = strings.Repeat("x", maxAccountScopeLabel+1)
	case "control-label":
		account["label"] = "secret-account@example.test\nprivate"
	case "control-detail":
		account["detail"] = "sensitive-plan\u0085private"
	case "long-detail":
		account["detail"] = strings.Repeat("x", maxAccountScopeDetail+1)
	case "non-oauth":
		account["type"] = "api_key"
	case "zero-id":
		account["credentialId"] = 0
	case "negative-id":
		account["credentialId"] = -918273645
	case "fractional-id":
		account["credentialId"] = 1.5
	}
	accounts := []map[string]any{account}
	if mode == "multiple" {
		accounts = append(accounts, map[string]any{
			"credentialId": 918273646, "provider": command["providerId"], "label": "other-account@example.test", "type": "oauth", "active": true,
		})
	}
	if mode == "inactive-extra" {
		accounts = append(accounts, map[string]any{
			"credentialId": 918273646, "provider": command["providerId"], "label": "OAuth credential #918273646", "type": "oauth", "active": false,
		})
	}
	if mode == "empty" {
		accounts = nil
	}
	reply(map[string]any{"accounts": accounts})
	return true
}

func accountScopeFixture(t *testing.T, mode string) (*Client, *bytes.Buffer) {
	t.Helper()
	t.Setenv("OMP_TEST_ACCOUNT_SCOPE_MODE", mode)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logger, logs := bufferedRPCLogger("json")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	client, err := Start(ctx, Config{Binary: binary}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, logs
}

func TestProviderAccountScopeStabilityAndIdentityChanges(t *testing.T) {
	for _, mode := range []string{"stable", "inactive-extra", "rotate-id", "rotate-label", "rotate-detail", "boundaries"} {
		t.Run(mode, func(t *testing.T) {
			client, _ := accountScopeFixture(t, mode)
			first, err := client.ProviderAccountScope(context.Background(), "anthropic")
			if err != nil || first == [32]byte{} {
				t.Fatalf("initial account scope unavailable: %v", err)
			}
			second, err := client.ProviderAccountScope(context.Background(), "anthropic")
			if err != nil {
				t.Fatalf("second account scope unavailable: %v", err)
			}
			stable := mode == "stable" || mode == "inactive-extra"
			if (first == second) != stable {
				t.Fatal("scope did not reflect account identity stability")
			}
		})
	}
}

func TestProviderAccountScopeSeparatesProviders(t *testing.T) {
	client, _ := accountScopeFixture(t, "stable")
	anthropic, err := client.ProviderAccountScope(context.Background(), "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	codex, err := client.ProviderAccountScope(context.Background(), "openai-codex")
	if err != nil || codex == anthropic || codex == [32]byte{} {
		t.Fatal("supported providers did not receive distinct account scopes")
	}
}

func TestProviderAccountScopeRejectsUnreliableIdentityPrivately(t *testing.T) {
	for _, mode := range []string{
		"empty", "inactive", "multiple", "provider-mismatch", "placeholder", "generic-label", "blank-label", "empty-label",
		"long-label", "control-label", "control-detail", "long-detail", "non-oauth", "zero-id", "negative-id", "fractional-id", "malformed", "rejected",
	} {
		t.Run(mode, func(t *testing.T) {
			client, logs := accountScopeFixture(t, mode)
			scope, err := client.ProviderAccountScope(context.Background(), "anthropic")
			if err == nil || scope != [32]byte{} {
				t.Fatal("unreliable account identity accepted")
			}
			_ = client.Close()
			privateOutput := err.Error() + logs.String()
			for event := range client.Events() {
				privateOutput += string(event)
			}
			for _, secret := range []string{"secret-account@example.test", "sensitive-plan", "918273645", "SECRET_ACCOUNT_REJECTION"} {
				if strings.Contains(privateOutput, secret) {
					t.Fatal("private account data escaped through an error, log, or event")
				}
			}
		})
	}
	for _, provider := range []string{"", "openai", "private-provider"} {
		if scope, err := new(Client).ProviderAccountScope(context.Background(), provider); err == nil || scope != [32]byte{} || strings.Contains(err.Error(), "private-provider") {
			t.Fatal("unsupported provider was queried or exposed")
		}
	}
}

func TestProviderAccountScopePreservesCancellation(t *testing.T) {
	client, _ := accountScopeFixture(t, "waiting")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := client.ProviderAccountScope(ctx, "anthropic")
		finished <- err
	}()
	if event := nextSessionEvent(t, client); event["type"] != "account_scope_waiting" {
		t.Fatal("account scope query did not reach the native RPC")
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query returned %v", err)
	}
	if client.Stopping() {
		t.Fatal("canceling a read-only account query retired the client")
	}
	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if scope, err := client.ProviderAccountScope(deadline, "anthropic"); !errors.Is(err, context.DeadlineExceeded) || scope != [32]byte{} {
		t.Fatal("expired account scope context did not preserve its deadline error")
	}
}
