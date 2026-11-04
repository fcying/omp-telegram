package omp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureUsageReport = "```\nUsage (1s ago)\n\nAnthropic\n- Claude 5h\n  secret-account@example.test: 30.00% used (70.0% left)  ← in use by this session\n  [###-------]\n  resets in 2h\n```"
const fixtureUsageFallback = "Usage\nInput tokens: 10\nOutput tokens: 20\nCache read tokens: 0\nCache write tokens: 0\nTotal tokens: 30\nPremium requests: 0\nCost: $0.000001"

var fixtureUsagePrompts int

func fixtureUsageCommand(command map[string]any) bool {
	mode := os.Getenv("OMP_TEST_USAGE_MODE")
	if mode == "" {
		return false
	}
	emit := func(value any) { json.NewEncoder(os.Stdout).Encode(value) }
	reply := func(data any, success bool) {
		emit(map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": success, "data": data})
	}
	output := func(text string) { emit(map[string]any{"type": "command_output", "text": text}) }
	switch command["type"] {
	case "get_state":
		id := "native-id"
		if mode == "scope-change" {
			id = "other-id"
			emit(map[string]any{"type": "session_info_update", "sessionId": id})
		}
		if release := os.Getenv("OMP_TEST_USAGE_STATE_RELEASE"); release != "" {
			emit(map[string]any{"type": "usage_state_waiting"})
			for {
				_, err := os.Stat(release)
				if err == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
		}
		reply(map[string]any{"sessionId": id}, true)
		return true
	case "usage_probe":
		reply(map[string]any{"prompts": fixtureUsagePrompts}, true)
		return true
	case "usage_public_output":
		output(fixtureUsageReport)
		reply(nil, true)
		return true
	}
	if command["type"] != "prompt" || command["message"] != "/usage" {
		return false
	}
	fixtureUsagePrompts++
	if mode == "reject" {
		reply(nil, false)
		return true
	}
	if mode == "exit" {
		os.Exit(0)
	}
	if mode == "delay" {
		emit(map[string]any{"type": "metadata_waiting"})
		time.Sleep(300 * time.Millisecond)
	}
	emit(map[string]any{"type": "agent_start"})
	output("unrelated private output")
	output("Usage tips are not a quota report")
	output("```\nUsageStatistics\nnot a quota report\n```")
	text := fixtureUsageReport
	switch mode {
	case "fallback":
		text = fixtureUsageFallback
	case "malformed":
		text = "```\nUsage\nsecret-account@example.test"
	case "oversized":
		text = "```\nUsage\n" + strings.Repeat("sensitive", maxUsageOutput)
	}
	if mode != "missing" && mode != "missing-late" {
		output(text)
		if mode == "duplicate" {
			output(text)
		}
	}
	if mode == "scope-ack" {
		emit(map[string]any{"type": "session_info_update", "sessionId": "other-id"})
	}
	switch mode {
	case "unknown-ack":
		reply(map[string]any{}, true)
	case "null-ack":
		reply(map[string]any{"agentInvoked": nil}, true)
	case "malformed-ack":
		reply("sensitive", true)
	default:
		reply(map[string]any{"agentInvoked": mode == "agent"}, true)
	}
	if mode == "late" || mode == "missing-late" {
		output(fixtureUsageReport)
	}
	return true
}

func seedUsageCatalog(client *Client) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.commandCatalog = CommandCatalog{State: CatalogReady, SessionID: "native-id", commands: map[string]commandInfo{"usage": {source: commandSourceBuiltin}}}
}

func usageFixture(t *testing.T, mode string) *Client {
	t.Helper()
	t.Setenv("OMP_TEST_USAGE_MODE", mode)
	client := fixtureClient(t)
	seedUsageCatalog(client)
	return client
}

func drainUsageUnrelated(t *testing.T, client *Client) {
	t.Helper()
	if event := nextSessionEvent(t, client); event["type"] != "agent_start" {
		t.Fatal("unrelated agent event lost")
	}
	for _, want := range []string{"unrelated private output", "Usage tips are not a quota report", "```\nUsageStatistics\nnot a quota report\n```"} {
		if event := nextSessionEvent(t, client); event["text"] != want {
			t.Fatal("unrelated command output lost")
		}
	}
}

func TestUsageCapturesOnlyItsLocalReport(t *testing.T) {
	for _, mode := range []string{"valid", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			client := usageFixture(t, mode)
			want := fixtureUsageReport
			if mode == "fallback" {
				want = fixtureUsageFallback
			}
			for range 2 {
				got, err := client.Usage(context.Background())
				if err != nil || got != want {
					t.Fatalf("usage capture failed: error = %v, matching = %t", err, got == want)
				}
				drainUsageUnrelated(t, client)
				select {
				case <-client.Events():
					t.Fatal("private usage output forwarded")
				default:
				}
			}
			// A later explicit native /usage is not the private query.
			if _, err := client.Call(context.Background(), "prompt", map[string]any{"message": "/usage"}); err != nil {
				t.Fatal(err)
			}
			drainUsageUnrelated(t, client)
			if event := nextSessionEvent(t, client); event["text"] != want {
				t.Fatal("explicit user usage output was swallowed")
			}
		})
	}
}

func TestUsageOutsidePrivateQueryIsForwarded(t *testing.T) {
	client := usageFixture(t, "valid")
	if _, err := client.Call(context.Background(), "usage_public_output", nil); err != nil {
		t.Fatal(err)
	}
	if event := nextSessionEvent(t, client); event["text"] != fixtureUsageReport {
		t.Fatal("ordinary native output was swallowed")
	}
}

func TestUsageKnownLocalInvalidReportsLeaveRuntimeUsable(t *testing.T) {
	for _, mode := range []string{"malformed", "oversized", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			client := usageFixture(t, mode)
			for range 2 {
				got, err := client.Usage(context.Background())
				if err == nil || got != "" || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "secret-account") {
					t.Fatal("invalid private output accepted or exposed")
				}
				drainUsageUnrelated(t, client)
				select {
				case <-client.stop:
					t.Fatal("confirmed local invalid report retired runtime")
				case <-client.Events():
					t.Fatal("invalid private report forwarded")
				default:
				}
			}
		})
	}
}

func TestUsageUncertainACKOrMissingOutputRetiresClient(t *testing.T) {
	for _, mode := range []string{"agent", "unknown-ack", "null-ack", "malformed-ack", "missing", "missing-late", "exit"} {
		t.Run(mode, func(t *testing.T) {
			client := usageFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := client.Usage(ctx)
			if err == nil || got != "" || strings.Contains(err.Error(), "sensitive") || ctx.Err() != nil {
				t.Fatal("uncertain private command accepted, exposed, or unbounded")
			}
			select {
			case <-client.stop:
			default:
				t.Fatal("uncertain command left runtime usable")
			}
			if _, err := client.Usage(ctx); err == nil {
				t.Fatal("late report could become another query")
			}
			assertNoUsageEventsAfterClose(t, client)
		})
	}
}

func assertNoUsageEventsAfterClose(t *testing.T, client *Client) {
	t.Helper()
	_ = client.Close()
	for raw := range client.Events() {
		var event struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &event) == nil && matchingUsageOutput(event.Text) {
			t.Fatal("private or late report leaked to events")
		}
	}
}

func TestUsageLateOutputAfterACKIsPrivateAndRetiresClient(t *testing.T) {
	client := usageFixture(t, "late")
	_, _ = client.Usage(context.Background())
	select {
	case <-client.stop:
	case <-time.After(5 * time.Second):
		t.Fatal("late report left runtime usable")
	}
	if _, err := client.Usage(context.Background()); err == nil {
		t.Fatal("late output crossed into next query")
	}
	assertNoUsageEventsAfterClose(t, client)
}

func TestUsageExplicitRejectionDoesNotRetireClient(t *testing.T) {
	client := usageFixture(t, "reject")
	for range 2 {
		got, err := client.Usage(context.Background())
		if got != "" || ClassifyError(err) != "rejected" {
			t.Fatal("explicit rejection was not preserved")
		}
		select {
		case <-client.stop:
			t.Fatal("explicit rejection retired runtime")
		default:
		}
	}
}

func TestUsageCancellationTimeoutAndExclusion(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "cancel"
		if timeout {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			client := usageFixture(t, "delay")
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if timeout {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
				want = context.DeadlineExceeded
			}
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, err := client.Usage(ctx)
				finished <- err
			}()
			if event := nextSessionEvent(t, client); event["type"] != "metadata_waiting" {
				t.Fatal("query did not enter local command")
			}
			if _, err := client.Usage(context.Background()); err == nil {
				t.Fatal("overlapping private query accepted")
			}
			if _, err := client.ConfiguredThinking(context.Background()); err == nil {
				t.Fatal("overlapping thinking query accepted")
			}
			if _, err := client.SessionInfo(context.Background()); err == nil {
				t.Fatal("overlapping session query accepted")
			}
			if !timeout {
				cancel()
			}
			if err := <-finished; !errors.Is(err, want) {
				t.Fatalf("interruption = %v, want %v", err, want)
			}
			select {
			case <-client.stop:
			default:
				t.Fatal("interrupted command left runtime usable")
			}
			assertNoUsageEventsAfterClose(t, client)
		})
	}
}

func TestUsageRequiresCurrentBuiltinCatalog(t *testing.T) {
	for _, mode := range []string{"unknown", "unsupported", "extension", "missing", "unscoped", "scope-change"} {
		t.Run(mode, func(t *testing.T) {
			client := usageFixture(t, "valid")
			client.mu.Lock()
			switch mode {
			case "unknown":
				client.commandCatalog.State = CatalogUnknown
			case "unsupported":
				client.commandCatalog.State = CatalogUnsupported
			case "extension":
				client.commandCatalog.commands["usage"] = commandInfo{source: commandSourceExtension}
			case "missing":
				delete(client.commandCatalog.commands, "usage")
			case "unscoped":
				client.commandCatalog.SessionID = ""
			case "scope-change":
				client.commandCatalog.SessionID = "stale-id"
			}
			client.mu.Unlock()
			if got, err := client.Usage(context.Background()); err == nil || got != "" {
				t.Fatal("usage invoked without current builtin proof")
			}
			raw, err := client.Call(context.Background(), "usage_probe", nil)
			var probe struct {
				Prompts int `json:"prompts"`
			}
			if err != nil || json.Unmarshal(raw, &probe) != nil || probe.Prompts != 0 {
				t.Fatal("unproven builtin prompt reached native process")
			}
		})
	}
}

func TestUsageRechecksAuthorizationAfterWriteGate(t *testing.T) {
	for _, change := range []string{"extension", "session", "epoch"} {
		t.Run(change, func(t *testing.T) {
			release := filepath.Join(t.TempDir(), "release-state")
			t.Setenv("OMP_TEST_USAGE_STATE_RELEASE", release)
			client := usageFixture(t, "valid")
			t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, err := client.Usage(ctx)
				finished <- err
			}()
			if event := nextSessionEvent(t, client); event["type"] != "usage_state_waiting" {
				t.Fatal("get_state did not reach its ACK gate")
			}
			client.writeGate <- struct{}{}
			held := true
			defer func() {
				if held {
					<-client.writeGate
				}
			}()
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				client.mu.Lock()
				waiting := client.metadata != nil && client.metadata.usage != nil && !client.metadata.usage.started
				client.mu.Unlock()
				if waiting {
					break
				}
				select {
				case <-tick.C:
				case <-ctx.Done():
					t.Fatal("usage did not reach the prompt write gate")
				}
			}
			// Apply the same invalidation paths used by the reader while Send is blocked.
			if change == "extension" {
				err := client.receiveCommandCatalogUpdate([]byte(`{"commands":[{"name":"usage","source":"extension"}]}`))
				if err != nil {
					t.Fatal(err)
				}
			} else {
				client.receiveCommandCatalogSession([]byte(`{"sessionId":"other-id"}`))
				if change == "epoch" {
					client.receiveCommandCatalogSession([]byte(`{"sessionId":"native-id"}`))
					err := client.receiveCommandCatalogUpdate([]byte(`{"commands":[{"name":"usage","source":"builtin"}]}`))
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			<-client.writeGate
			held = false
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("stale authorization was accepted")
				}
			case <-ctx.Done():
				t.Fatal("unsent query did not finish")
			}
			raw, err := client.Call(ctx, "usage_probe", nil)
			var probe struct {
				Prompts int `json:"prompts"`
			}
			if err != nil || json.Unmarshal(raw, &probe) != nil || probe.Prompts != 0 || client.Stopping() {
				t.Fatalf("unsent query executed, replayed, or retired the runtime: prompts=%d, error=%v", probe.Prompts, err)
			}
		})
	}
}

func TestUsageScopeChangeAtACKRejectsReport(t *testing.T) {
	client := usageFixture(t, "scope-ack")
	if got, err := client.Usage(context.Background()); err == nil || got != "" {
		t.Fatal("usage from changed session accepted")
	}
}

func TestUsagePreCanceledContextDoesNotSubmit(t *testing.T) {
	client := usageFixture(t, "valid")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := client.Usage(ctx); got != "" || err != context.Canceled {
		t.Fatal("pre-canceled context was ignored")
	}
	if got, err := client.Usage(context.Background()); err != nil || got != fixtureUsageReport {
		t.Fatal("pre-cancellation disabled private query")
	}
}

func TestUsageNeverLogsNativeOutput(t *testing.T) {
	t.Setenv("OMP_TEST_USAGE_MODE", "valid")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Start(ctx, Config{Binary: binary}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	seedUsageCatalog(client)
	if _, err := client.Usage(ctx); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	for _, private := range []string{"secret-account", "30.00%", "unrelated private output"} {
		if strings.Contains(logs.String(), private) {
			t.Fatal("native usage output appeared in logs")
		}
	}
}
