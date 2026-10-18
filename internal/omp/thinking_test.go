package omp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureThinkingCommand(command map[string]any) bool {
	mode := os.Getenv("OMP_TEST_THINKING_MODE")
	if mode == "missing-late" && command["type"] == "prompt" && command["message"] == "/model @slow" {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "command_output", "text": "Thinking: low\nAvailable: low, high, auto"})
		return false
	}
	if mode == "" || command["type"] != "prompt" || command["message"] != "/effort" {
		return false
	}
	emit := func(value any) { json.NewEncoder(os.Stdout).Encode(value) }
	reply := func(data any, success bool) {
		emit(map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": success, "data": data})
	}
	if mode == "reject" {
		reply(nil, false)
		return true
	}
	if mode == "exit" {
		os.Exit(0)
	}
	if mode == "delay" {
		emit(map[string]any{"type": "metadata_waiting"})
		time.Sleep(150 * time.Millisecond)
	}
	emit(map[string]any{"type": "agent_start"})
	emit(map[string]any{"type": "command_output", "text": "unrelated private output"})
	text := "Thinking: auto\nAvailable: off, minimal, low, medium, high, xhigh, max, auto"
	switch mode {
	case "default":
		text = "Thinking: model default\nAvailable: low, medium, high, auto"
	case "nonreasoning":
		text = "fixture/family/plain has no adjustable thinking level."
	case "no-model":
		text = "The current model has no adjustable thinking level."
	case "malformed":
		text = "Thinking: sensitive\nAvailable: low, high, auto"
	case "oversized":
		text = "Thinking: " + strings.Repeat("sensitive", maxThinkingOutput)
	}
	if mode != "missing" && mode != "missing-late" {
		emit(map[string]any{"type": "command_output", "text": text})
		if mode == "duplicate" {
			emit(map[string]any{"type": "command_output", "text": text})
		}
	}
	switch mode {
	case "unknown-ack":
		reply(map[string]any{}, true)
	case "malformed-ack":
		reply("sensitive", true)
	default:
		reply(map[string]any{"agentInvoked": mode == "agent"}, true)
	}
	return true
}

func TestConfiguredThinkingPreservesAutoAndUnrelatedEvents(t *testing.T) {
	for _, mode := range []string{"auto", "duplicate", "default", "nonreasoning", "no-model"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OMP_TEST_THINKING_MODE", mode)
			client := fixtureClient(t)
			want := ""
			if mode == "auto" || mode == "duplicate" {
				want = "auto"
			}
			got, err := client.ConfiguredThinking(context.Background())
			if err != nil || got != want {
				t.Fatalf("selector = %q, error = %v", got, err)
			}
			if event := nextSessionEvent(t, client); event["type"] != "agent_start" {
				t.Fatal("unrelated agent event lost")
			}
			if event := nextSessionEvent(t, client); event["text"] != "unrelated private output" {
				t.Fatal("unrelated command output lost")
			}
			select {
			case raw := <-client.Events():
				t.Fatalf("private query output forwarded: %s", raw)
			default:
			}
			if _, err := client.ConfiguredThinking(context.Background()); err != nil {
				t.Fatalf("completed query prevented another query: %v", err)
			}
		})
	}
}

func TestConfiguredThinkingRejectsInvalidLocalOutputWithoutDisablingMetadata(t *testing.T) {
	for _, mode := range []string{"malformed", "missing", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OMP_TEST_THINKING_MODE", mode)
			client, file := sessionFixture(t, "")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for range 2 {
				got, err := client.ConfiguredThinking(ctx)
				if err == nil || got != "" || strings.Contains(err.Error(), "sensitive") || ctx.Err() != nil {
					t.Fatalf("invalid output accepted or query not bounded: %q, %v", got, err)
				}
				select {
				case <-client.stop:
					t.Fatal("known-local query stopped runtime")
				default:
				}
				if client.metadataBusy || client.metadataUnavailable || client.metadata != nil {
					t.Fatal("known-local output failure left metadata unavailable")
				}
				if event := nextSessionEvent(t, client); event["type"] != "agent_start" {
					t.Fatal("unrelated agent event lost")
				}
				if event := nextSessionEvent(t, client); event["text"] != "unrelated private output" {
					t.Fatal("unrelated command output lost")
				}
				select {
				case raw := <-client.Events():
					t.Fatalf("private malformed query output forwarded: %s", raw)
				default:
				}
				info, err := client.SessionInfo(ctx)
				if err != nil || info.ID != "native-id" || info.File != file || info.CWD != filepath.Dir(file) {
					t.Fatalf("known-local effort failure prevented session metadata: %#v, %v", info, err)
				}
				if event := nextSessionEvent(t, client); event["type"] != "agent_start" {
					t.Fatal("session agent event lost")
				}
				if event := nextSessionEvent(t, client); event["text"] != "unrelated command" {
					t.Fatal("unrelated session output lost")
				}
			}
		})
	}
}

func TestConfiguredThinkingRepeatedInvalidOutputLeavesModelRoleUsable(t *testing.T) {
	for _, mode := range []string{"malformed", "missing", "oversized", "missing-late"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OMP_TEST_THINKING_MODE", mode)
			t.Setenv("OMP_TEST_MODEL_MODE", "valid")
			client := fixtureClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for range 2 {
				if got, err := client.ConfiguredThinking(ctx); err == nil || got != "" || strings.Contains(err.Error(), "sensitive") {
					t.Fatalf("bad effort output accepted: %q, %v", got, err)
				}
				if client.metadataBusy || client.metadataUnavailable || client.metadata != nil {
					t.Fatal("known-local effort failure disabled metadata")
				}
				if event := nextSessionEvent(t, client); event["type"] != "agent_start" {
					t.Fatal("unrelated agent event lost")
				}
				if event := nextSessionEvent(t, client); event["text"] != "unrelated private output" {
					t.Fatal("unrelated command output lost")
				}
			}
			model, err := client.SetModelRole(ctx, "slow")
			if err != nil || model != (Model{Provider: "fixture", ID: "family/deep"}) {
				t.Fatalf("repeated effort output failures prevented model role switch: %#v, %v", model, err)
			}
			if mode == "missing-late" {
				if event := nextSessionEvent(t, client); event["text"] != "Thinking: low\nAvailable: low, high, auto" {
					t.Fatal("late effort output consumed as model metadata")
				}
			}
			if event := nextSessionEvent(t, client); event["text"] != "unrelated private output" {
				t.Fatal("unrelated model command output lost")
			}
			select {
			case raw := <-client.Events():
				t.Fatalf("private model query output forwarded: %s", raw)
			default:
			}
		})
	}
}

func TestConfiguredThinkingStopsUncertainSubmission(t *testing.T) {
	for _, mode := range []string{"agent", "unknown-ack", "malformed-ack", "exit"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OMP_TEST_THINKING_MODE", mode)
			client := fixtureClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := client.ConfiguredThinking(ctx)
			if err == nil || got != "" || strings.Contains(err.Error(), "sensitive") || ctx.Err() != nil {
				t.Fatalf("uncertain query accepted: %q, %v", got, err)
			}
			select {
			case <-client.stop:
			default:
				t.Fatal("uncertain execution left client usable")
			}
			if !client.metadataUnavailable {
				t.Fatal("uncertain submission left metadata correlation usable")
			}
		})
	}
}

func TestConfiguredThinkingExplicitRejectionLeavesClientUsable(t *testing.T) {
	t.Setenv("OMP_TEST_THINKING_MODE", "reject")
	client := fixtureClient(t)
	for range 2 {
		got, err := client.ConfiguredThinking(context.Background())
		if got != "" || ClassifyError(err) != "rejected" {
			t.Fatalf("explicit rejection = %q, %v", got, err)
		}
		select {
		case <-client.stop:
			t.Fatal("explicit rejection stopped runtime")
		default:
		}
		if client.metadataBusy || client.metadataUnavailable || client.metadata != nil {
			t.Fatal("explicit rejection left metadata query state behind")
		}
	}
}

func TestConfiguredThinkingCancellationAndMetadataExclusion(t *testing.T) {
	t.Setenv("OMP_TEST_THINKING_MODE", "delay")
	client := fixtureClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := client.ConfiguredThinking(ctx)
		finished <- err
	}()
	if event := nextSessionEvent(t, client); event["type"] != "metadata_waiting" {
		t.Fatal("query did not reach local command")
	}
	if _, err := client.ConfiguredThinking(context.Background()); err == nil {
		t.Fatal("concurrent thinking query accepted")
	}
	if _, err := client.SessionInfo(context.Background()); err == nil {
		t.Fatal("concurrent session query accepted")
	}
	if _, err := client.SetModelRole(context.Background(), "slow"); err == nil {
		t.Fatal("concurrent model mutation accepted")
	}
	cancel()
	if err := <-finished; err != context.Canceled {
		t.Fatalf("query ignored cancellation: %v", err)
	}
	select {
	case <-client.stop:
	default:
		t.Fatal("canceled submission left client usable")
	}
	if _, err := client.ConfiguredThinking(context.Background()); err == nil {
		t.Fatal("canceled query could consume late metadata")
	}
}

func TestConfiguredThinkingPreCanceledContextLeavesMetadataUsable(t *testing.T) {
	t.Setenv("OMP_TEST_THINKING_MODE", "auto")
	client := fixtureClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := client.ConfiguredThinking(ctx); got != "" || err != context.Canceled {
		t.Fatalf("pre-canceled query = %q, %v", got, err)
	}
	if got, err := client.ConfiguredThinking(context.Background()); got != "auto" || err != nil {
		t.Fatalf("pre-canceled query poisoned client: %q, %v", got, err)
	}
}

func TestParseThinkingOutput(t *testing.T) {
	for _, selector := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max", "auto"} {
		got, ok := parseThinkingOutput("Thinking: " + selector + "\nAvailable: off, low, high, auto")
		if !ok || got != selector {
			t.Fatalf("safe selector %q parsed as %q, %t", selector, got, ok)
		}
	}
	for _, text := range []string{
		"", "Thinking: auto", "Thinking: auto\nAvailable: ",
		"Thinking: AUTO\nAvailable: auto", "Thinking: auto \nAvailable: auto",
		"Thinking: unknown\nAvailable: auto", "Thinking: auto\nAvailable: unknown",
		"Thinking: auto\nAvailable: auto, auto", "Thinking: auto\nAvailable: auto\nextra",
		"Thinking: auto\r\nAvailable: auto", "Thinking: auto\nAvailable: auto\x00",
		"Thinking: auto\nAvailable: " + strings.Repeat("auto, ", 1000),
		"unrelated has no adjustable thinking level.",
	} {
		if got, ok := parseThinkingOutput(text); ok || got != "" {
			t.Fatalf("malformed output accepted: %q", text)
		}
	}
}
