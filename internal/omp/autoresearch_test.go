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

var fixtureAutoresearchStateCalls int

func fixtureAutoresearchCommand(command map[string]any) bool {
	payload := os.Getenv("OMP_TEST_AUTORESEARCH_ENTRIES")
	if payload == "" {
		return false
	}
	response := map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": true}
	switch command["type"] {
	case "get_state":
		fixtureAutoresearchStateCalls++
		id := "native-id"
		if fixtureAutoresearchStateCalls > 1 && os.Getenv("OMP_TEST_AUTORESEARCH_SWITCH") == "1" {
			id = "replacement-id"
		}
		response["data"] = map[string]any{"sessionId": id, "sessionFile": os.Getenv("OMP_TEST_AUTORESEARCH_FILE")}
	case "get_entries":
		cursor := os.Getenv("OMP_TEST_AUTORESEARCH_CURSOR")
		if payload == "rejected" || cursor != "" && command["since"] != cursor {
			response["success"] = false
			response["error"] = "entries cursor rejected"
		} else {
			response["data"] = json.RawMessage(payload)
		}
	default:
		return false
	}
	raw, _ := json.Marshal(response)
	fmt.Println(string(raw))
	return true
}

func TestAutoresearchModeCurrentBranchAndFailures(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		enabled bool
		invalid bool
	}{
		{"empty", `{"entries":[],"leafId":null}`, false, false},
		{"no-control", `{"entries":[{"id":"m","parentId":null,"type":"message","message":{"role":"assistant","content":"opaque"}}],"leafId":"m"}`, false, false},
		{"on", `{"entries":[{"id":"on","parentId":null,"type":"custom","customType":"autoresearch-control","data":{"mode":"on"}}],"leafId":"on"}`, true, false},
		{"latest-off", `{"entries":[{"id":"off","parentId":"on","type":"custom","customType":"autoresearch-control","data":{"mode":"off"}},{"id":"on","parentId":null,"type":"custom","customType":"autoresearch-control","data":{"mode":"on"}}],"leafId":"off"}`, false, false},
		{"other-branch-off", `{"entries":[{"id":"on","parentId":null,"type":"custom","customType":"autoresearch-control","data":{"mode":"on"}},{"id":"m","parentId":"on","type":"message"},{"id":"off","parentId":"on","type":"custom","customType":"autoresearch-control","data":{"mode":"off"}}],"leafId":"m"}`, true, false},
		{"clear", `{"entries":[{"id":"clear","parentId":null,"type":"custom","customType":"autoresearch-control","data":{"mode":"clear"}}],"leafId":"clear"}`, false, false},
		{"unrelated-data", `{"entries":[{"id":"custom","parentId":null,"type":"custom","customType":"other","data":"opaque"}],"leafId":"custom"}`, false, false},
		{"missing-entries", `{"leafId":null}`, false, true},
		{"missing-leaf", `{"entries":[]}`, false, true},
		{"missing-parent", `{"entries":[{"id":"m","parentId":"missing","type":"message"}],"leafId":"m"}`, false, true},
		{"missing-leaf-entry", `{"entries":[],"leafId":"missing"}`, false, true},
		{"cycle", `{"entries":[{"id":"a","parentId":"b"},{"id":"b","parentId":"a"}],"leafId":"a"}`, false, true},
		{"control-cycle", `{"entries":[{"id":"a","parentId":"a","type":"custom","customType":"autoresearch-control","data":{"mode":"on"}}],"leafId":"a"}`, false, true},
		{"duplicate-id", `{"entries":[{"id":"a"},{"id":"a"}],"leafId":"a"}`, false, true},
		{"unknown-mode", `{"entries":[{"id":"a","type":"custom","customType":"autoresearch-control","data":{"mode":"future"}}],"leafId":"a"}`, false, true},
		{"malformed-mode", `{"entries":[{"id":"a","type":"custom","customType":"autoresearch-control","data":{"mode":true}}],"leafId":"a"}`, false, true},
		{"unsupported", `rejected`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OMP_TEST_AUTORESEARCH_ENTRIES", tc.payload)
			t.Setenv("OMP_TEST_AUTORESEARCH_FILE", filepath.Join(t.TempDir(), "unpersisted.jsonl"))
			client := fixtureClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			enabled, err := client.AutoresearchMode(ctx)
			if (err != nil) != tc.invalid {
				t.Fatalf("mode query error = %v, want invalid=%t", err, tc.invalid)
			}
			if !tc.invalid && enabled != tc.enabled {
				t.Fatalf("mode = %t, want %t", enabled, tc.enabled)
			}
		})
	}
}

func TestAutoresearchModeLargeSavedHistoryUsesCurrentRPCBranch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	write := func(value any) {
		t.Helper()
		if err := encoder.Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	write(map[string]any{"type": "title", "title": "native title"})
	write(map[string]any{"type": "session", "version": 3, "id": "native-id"})
	parent := any(nil)
	padding := strings.Repeat("opaque history ", 20<<10)
	for i := range 256 {
		id := fmt.Sprintf("m%d", i)
		write(map[string]any{"type": "message", "id": id, "parentId": parent, "message": map[string]any{"role": "user", "content": padding}})
		parent = id
	}
	write(map[string]any{"type": "custom", "id": "on", "parentId": parent, "customType": "autoresearch-control", "data": map[string]string{"mode": "on"}})
	write(map[string]any{"type": "custom", "id": "other-off", "parentId": parent, "customType": "autoresearch-control", "data": map[string]string{"mode": "off"}})
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() <= maxLogical {
		t.Fatalf("history must exceed the RPC reassembly limit: info=%v err=%v", info, err)
	}
	for _, tc := range []struct {
		name, payload string
		enabled       bool
	}{
		{"restored-on-branch", `{"entries":[],"leafId":"on"}`, true},
		{"off-on-other-branch", `{"entries":[],"leafId":"other-off"}`, false},
		{"off-appended-after-file-read", `{"entries":[{"id":"new-off","parentId":"on","type":"custom","customType":"autoresearch-control","data":{"mode":"off"}}],"leafId":"new-off"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OMP_TEST_AUTORESEARCH_FILE", path)
			t.Setenv("OMP_TEST_AUTORESEARCH_CURSOR", "other-off")
			t.Setenv("OMP_TEST_AUTORESEARCH_ENTRIES", tc.payload)
			client := fixtureClient(t)
			// Keep the >64 MiB behavior check independent of race instrumentation speed.
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			enabled, err := client.AutoresearchMode(ctx)
			if err != nil || enabled != tc.enabled {
				t.Fatalf("saved history mode=%t err=%v, want %t", enabled, err, tc.enabled)
			}
		})
	}
}

func TestAutoresearchModeRecoversMalformedHistoryThroughRPC(t *testing.T) {
	const prefix = "{\"type\":\"session\",\"version\":3,\"id\":\"native-id\"}\n" +
		"{\"type\":\"custom\",\"id\":\"off\",\"parentId\":null,\"customType\":\"autoresearch-control\",\"data\":{\"mode\":\"off\"}}\n"
	const partialMessage = `{"type":"message","id":"unfinished"`
	for _, tc := range []struct {
		name, suffix, payload string
		enabled, invalid      bool
	}{
		{"recovered-off", partialMessage, `{"entries":[],"leafId":"off"}`, false, false},
		{"unpersisted-on", `{"type":"custom","id":"on","parentId":"off","customType":"autoresearch-control","data":{"mode":`, `{"entries":[{"id":"on","parentId":"off","type":"custom","customType":"autoresearch-control","data":{"mode":"on"}}],"leafId":"on"}`, true, false},
		{"native-suffix-after-malformed-middle", "not-json\n{\"id\":\"disk-off\",\"parentId\":\"off\",\"type\":\"custom\",\"customType\":\"autoresearch-control\",\"data\":{\"mode\":\"off\"}}\n", `{"entries":[{"id":"disk-off","parentId":"off","type":"custom","customType":"autoresearch-control","data":{"mode":"off"}},{"id":"native-on","parentId":"off","type":"custom","customType":"autoresearch-control","data":{"mode":"on"}}],"leafId":"native-on"}`, true, false},
		{"invalid-disk-control", `{"id":"invalid","parentId":"off","type":"custom","customType":"autoresearch-control","data":{"mode":true}}`, `{"entries":[],"leafId":"off"}`, false, true},
		{"rejected-cursor", partialMessage, "rejected", false, true},
		{"missing-ancestor", partialMessage, `{"entries":[{"id":"on","parentId":"missing","type":"custom","customType":"autoresearch-control","data":{"mode":"on"}}],"leafId":"on"}`, false, true},
		{"invalid-rpc-control", partialMessage, `{"entries":[{"id":"on","parentId":"off","type":"custom","customType":"autoresearch-control","data":{"mode":true}}],"leafId":"on"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.jsonl")
			history := prefix + tc.suffix
			if err := os.WriteFile(path, []byte(history), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("OMP_TEST_AUTORESEARCH_FILE", path)
			t.Setenv("OMP_TEST_AUTORESEARCH_CURSOR", "off")
			t.Setenv("OMP_TEST_AUTORESEARCH_ENTRIES", tc.payload)
			client := fixtureClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			enabled, err := client.AutoresearchMode(ctx)
			if (err != nil) != tc.invalid || !tc.invalid && enabled != tc.enabled {
				t.Fatalf("recovered mode=%t err=%v, want mode=%t invalid=%t", enabled, err, tc.enabled, tc.invalid)
			}
			saved, err := os.ReadFile(path)
			if err != nil || string(saved) != history {
				t.Fatal("mode lookup modified native history")
			}
		})
	}
}

func TestAutoresearchModeUsesNativeMigrationForLegacyHistory(t *testing.T) {
	const legacyEntries = "{\"type\":\"message\",\"message\":{\"role\":\"assistant\",\"content\":\"saved reply\"}}\n" +
		"{\"type\":\"custom\",\"customType\":\"autoresearch-control\",\"data\":{\"mode\":\"off\"}}\n"
	for _, tc := range []struct {
		name, version, payload string
		enabled, invalid       bool
	}{
		{"v1-native-on", `,"version":1`, `{"entries":[{"id":"m","parentId":null,"type":"message"},{"id":"on","parentId":"m","type":"custom","customType":"autoresearch-control","data":{"mode":"on"}}],"leafId":"on"}`, true, false},
		{"v1-native-off", `,"version":1`, `{"entries":[{"id":"m","parentId":null,"type":"message"}],"leafId":"m"}`, false, false},
		{"unversioned-native-on", "", `{"entries":[{"id":"on","parentId":null,"type":"custom","customType":"autoresearch-control","data":{"mode":"on"}},{"id":"m","parentId":"on","type":"message"}],"leafId":"m"}`, true, false},
		{"modern-missing-id", `,"version":3`, `{"entries":[],"leafId":null}`, false, true},
		{"legacy-rejected", `,"version":1`, "rejected", false, true},
		{"legacy-invalid-native-control", `,"version":1`, `{"entries":[{"id":"on","parentId":null,"type":"custom","customType":"autoresearch-control","data":{"mode":true}}],"leafId":"on"}`, false, true},
		{"legacy-broken-native-ancestry", "", `{"entries":[{"id":"m","parentId":"missing","type":"message"}],"leafId":"m"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.jsonl")
			history := "{\"type\":\"session\",\"id\":\"native-id\"" + tc.version + "}\n" + legacyEntries
			if err := os.WriteFile(path, []byte(history), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("OMP_TEST_AUTORESEARCH_FILE", path)
			t.Setenv("OMP_TEST_AUTORESEARCH_ENTRIES", tc.payload)
			client := fixtureClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			enabled, err := client.AutoresearchMode(ctx)
			if (err != nil) != tc.invalid || !tc.invalid && enabled != tc.enabled {
				t.Fatalf("legacy mode=%t err=%v, want mode=%t invalid=%t", enabled, err, tc.enabled, tc.invalid)
			}
			saved, err := os.ReadFile(path)
			if err != nil || string(saved) != history {
				t.Fatal("mode lookup modified native legacy history")
			}
		})
	}
}

func TestAutoresearchModeRejectsHistoryMismatchAndConcurrentSessionChange(t *testing.T) {
	for _, tc := range []struct {
		name, history string
		switchSession bool
	}{
		{"wrong-session", "{\"type\":\"session\",\"id\":\"other-id\"}\n", false},
		{"session-changed", "{\"type\":\"session\",\"id\":\"native-id\"}\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.jsonl")
			if err := os.WriteFile(path, []byte(tc.history), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("OMP_TEST_AUTORESEARCH_FILE", path)
			t.Setenv("OMP_TEST_AUTORESEARCH_ENTRIES", `{"entries":[],"leafId":null}`)
			if tc.switchSession {
				t.Setenv("OMP_TEST_AUTORESEARCH_SWITCH", "1")
			}
			client := fixtureClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if enabled, err := client.AutoresearchMode(ctx); err == nil || enabled {
				t.Fatalf("unconfirmed history was treated as mode=%t: err=%v", enabled, err)
			}
		})
	}
}

func TestAutoresearchCommandSourceBoundaries(t *testing.T) {
	for _, source := range []string{"extension", "builtin", "file", "future", ""} {
		t.Run(source, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"commands": []map[string]any{
				{"name": "autoresearch", "source": source, "aliases": []string{"research_alias"}},
				{"name": "other_extension", "source": "extension"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			commands, err := parseCommandCatalog(raw)
			if err != nil {
				t.Fatal(err)
			}
			catalog := CommandCatalog{State: CatalogReady, commands: commands}
			allowed := source == "extension" || source == "builtin" || source == "file"
			aliasAllowed := source == "builtin" || source == "file"
			if catalog.HasExecutableCommand("autoresearch") != allowed || catalog.HasExecutableCommand("research_alias") != aliasAllowed || catalog.HasExecutableCommand("other_extension") {
				t.Fatal("autoresearch exception relaxed another extension, alias, or unknown source")
			}
			iterated := map[string]bool{}
			for name := range catalog.ExecutableCommands {
				iterated[name] = true
			}
			if iterated["autoresearch"] != allowed || iterated["research_alias"] != aliasAllowed || iterated["other_extension"] {
				t.Fatal("menu availability disagreed with execution source restrictions")
			}
		})
	}
}

func TestAutoresearchAliasDoesNotAuthorizeAnotherExtension(t *testing.T) {
	commands, err := parseCommandCatalog([]byte(`{"commands":[{"name":"other_extension","source":"extension","aliases":["autoresearch"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	catalog := CommandCatalog{State: CatalogReady, commands: commands}
	if catalog.HasExecutableCommand("autoresearch") {
		t.Fatal("another extension gained execution permission through the autoresearch alias")
	}
	for name := range catalog.ExecutableCommands {
		if name == "autoresearch" {
			t.Fatal("another extension's alias was published as executable autoresearch")
		}
	}
}
