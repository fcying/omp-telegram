package omp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureSessionCommand(command map[string]any) bool {
	if fixtureThinkingCommand(command) {
		return true
	}
	file := os.Getenv("OMP_TEST_SESSION_FILE")
	if file == "" {
		return false
	}
	emit := func(value any) {
		data, _ := json.Marshal(value)
		fmt.Println(string(data))
	}
	reply := func(data any) {
		emit(map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": true, "data": data})
	}
	if command["type"] == "get_state" {
		reply(map[string]any{"sessionId": "native-id", "sessionFile": file, "systemPrompt": "sensitive state"})
		return true
	}
	if command["type"] != "prompt" || command["message"] != "/session info" {
		return false
	}
	mode := os.Getenv("OMP_TEST_SESSION_MODE")
	if mode == "exit" {
		os.Exit(0)
	}
	if mode == "delay" {
		emit(map[string]any{"type": "metadata_waiting"})
		time.Sleep(150 * time.Millisecond)
	}
	emit(map[string]any{"type": "agent_start"})
	emit(map[string]any{"type": "command_output", "text": "unrelated command"})
	text := "Session: native-id\nTitle: fixture\nCWD: " + filepath.Dir(file)
	if mode == "malformed" {
		text = "Session: native-id\nTitle: sensitive\nextra line\nCWD: " + filepath.Dir(file)
	}
	if mode == "mismatch" {
		text = strings.Replace(text, "native-id", "other-id", 1)
	}
	if mode != "missing" {
		emit(map[string]any{"type": "command_output", "text": text})
	}
	reply(map[string]any{"agentInvoked": mode == "agent"})
	return true
}

func sessionFixture(t *testing.T, mode string) (*Client, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TEST_SESSION_FILE", file)
	t.Setenv("OMP_TEST_SESSION_MODE", mode)
	return fixtureClient(t), file
}

func nextSessionEvent(t *testing.T, client *Client) map[string]any {
	t.Helper()
	select {
	case raw, ok := <-client.Events():
		var event map[string]any
		if !ok || json.Unmarshal(raw, &event) != nil {
			t.Fatal("missing or invalid event")
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("event lost")
		return nil
	}
}

func TestSessionInfoPreservesUnrelatedEvents(t *testing.T) {
	client, file := sessionFixture(t, "")
	info, err := client.SessionInfo(context.Background())
	if err != nil || info.ID != "native-id" || info.File != file || info.CWD != filepath.Dir(file) {
		t.Fatalf("session metadata unavailable: %#v, %v", info, err)
	}
	if event := nextSessionEvent(t, client); event["type"] != "agent_start" {
		t.Fatal("agent event lost")
	}
	if event := nextSessionEvent(t, client); event["text"] != "unrelated command" {
		t.Fatal("unrelated command output lost")
	}
	if _, err := client.SessionInfo(context.Background()); err != nil {
		t.Fatalf("completed query prevented another query: %v", err)
	}
}

func TestSessionInfoDoesNotCreateUnpersistedHistory(t *testing.T) {
	client, file := sessionFixture(t, "")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	info, err := client.SessionInfo(context.Background())
	if err != nil || info.File != file {
		t.Fatalf("new native session metadata unavailable: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("metadata query created native history")
	}
}

func TestSessionInfoRejectsUncertainMetadata(t *testing.T) {
	for _, mode := range []string{"malformed", "missing", "mismatch", "agent", "exit"} {
		t.Run(mode, func(t *testing.T) {
			client, _ := sessionFixture(t, mode)
			// Race-instrumented subprocesses delay exit by one second.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			info, err := client.SessionInfo(ctx)
			if err == nil || info != (SessionInfo{}) || strings.Contains(err.Error(), "sensitive") || ctx.Err() != nil {
				t.Fatalf("unsafe metadata accepted or failure not bounded: %#v, %v", info, err)
			}
			if mode == "mismatch" {
				nextSessionEvent(t, client)
				nextSessionEvent(t, client)
				if event := nextSessionEvent(t, client); !strings.Contains(event["text"].(string), "other-id") {
					t.Fatal("different session output consumed")
				}
			}
		})
	}
}

func TestSessionInfoCancellationPreservesLateOutput(t *testing.T) {
	client, _ := sessionFixture(t, "delay")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := client.SessionInfo(ctx)
		finished <- err
	}()
	if event := nextSessionEvent(t, client); event["type"] != "metadata_waiting" {
		t.Fatal("query did not reach local command")
	}
	if _, err := client.SessionInfo(context.Background()); err == nil {
		t.Fatal("concurrent metadata query accepted")
	}
	cancel()
	if err := <-finished; err != context.Canceled {
		t.Fatalf("query ignored cancellation: %v", err)
	}
	if _, err := client.SessionInfo(context.Background()); err == nil {
		t.Fatal("query could consume an interrupted query's late output")
	}
	nextSessionEvent(t, client)
	nextSessionEvent(t, client)
	if event := nextSessionEvent(t, client); !strings.HasPrefix(event["text"].(string), "Session: native-id\n") {
		t.Fatal("late metadata output lost")
	}
}

func TestSessionInfoClosedClient(t *testing.T) {
	client, _ := sessionFixture(t, "")
	client.Close()
	if _, err := client.SessionInfo(context.Background()); err == nil {
		t.Fatal("closed process supplied metadata")
	}
}

func TestValidateArgsReservesSessionAlias(t *testing.T) {
	for _, args := range [][]string{{"--session", "native-id"}, {"--session=native-id"}} {
		if ValidateArgs(args) == nil {
			t.Fatal("native session lifecycle override accepted")
		}
	}
}
