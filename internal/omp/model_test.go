package omp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixtureModelConfig() {
	if len(os.Args) != 5 || os.Args[2] != "get" || os.Args[4] != "--json" {
		os.Exit(2)
	}
	var value any
	switch os.Args[3] {
	case "cycleOrder":
		value = []string{"slow", "unconfigured", "default", "smol"}
	case "modelRoles":
		value = map[string]string{"smol": "fixture/quick:low", "slow": "fixture/deep:high"}
	default:
		os.Exit(2)
	}
	if os.Getenv("OMP_TEST_CONFIG_OVERLAYS") == "1" {
		for _, file := range filepath.SplitList(os.Getenv("PI_CONFIG_FILES")) {
			data, err := os.ReadFile(file)
			var overlay map[string]json.RawMessage
			if err != nil || json.Unmarshal(data, &overlay) != nil {
				os.Exit(2)
			}
			if raw, exists := overlay[os.Args[3]]; exists {
				if os.Args[3] == "modelRoles" {
					var values map[string]string
					if json.Unmarshal(raw, &values) != nil {
						os.Exit(2)
					}
					for role, selector := range values {
						value.(map[string]string)[role] = selector
					}
				} else if json.Unmarshal(raw, &value) != nil {
					os.Exit(2)
				}
			}
		}
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"key": os.Args[3], "value": value})
}

func fixtureModelCommand(command map[string]any) bool {
	mode := os.Getenv("OMP_TEST_MODEL_MODE")
	if mode == "" {
		return false
	}
	emit := func(value any) { json.NewEncoder(os.Stdout).Encode(value) }
	reply := func(data any) {
		emit(map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": true, "data": data})
	}
	if command["type"] == "get_state" {
		id := "family/deep"
		if mode == "mismatch" {
			id = "wrong"
		}
		reply(map[string]any{"model": Model{Provider: "fixture", ID: id}})
		return true
	}
	if command["type"] != "prompt" || command["message"] != "/model @slow" {
		return false
	}
	if mode == "delay" {
		time.Sleep(time.Second)
	}
	emit(map[string]any{"type": "command_output", "text": "unrelated private output"})
	if mode != "missing" {
		emit(map[string]any{"type": "command_output", "text": "Model set to fixture/family/deep."})
	}
	reply(map[string]any{"agentInvoked": mode == "agent"})
	return true
}

func TestCycleRolesPreservesNativeOrderAndDefaultFallback(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	roles, err := CycleRoles(context.Background(), Config{Binary: exe, CWD: t.TempDir()})
	want := []ModelRole{{Role: "slow", Selector: "fixture/deep:high"}, {Role: "default"}, {Role: "smol", Selector: "fixture/quick:low"}}
	if err != nil || !reflect.DeepEqual(roles, want) {
		t.Fatalf("roles = %#v, error = %v", roles, err)
	}
}

func TestCycleRolesRejectsUnqueryableOverrides(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"--profile=work", "--smol=quick", "--slow=deep", "--plan=planner"} {
		roles, err := CycleRoles(context.Background(), Config{Binary: exe, CWD: t.TempDir(), Args: []string{arg}})
		if err == nil || roles != nil {
			t.Fatalf("unsupported runtime override returned a misleading model list: %v, %v", roles, err)
		}
	}
}

func TestCycleRolesAppliesOrderedConfigOverlays(t *testing.T) {
	t.Setenv("OMP_TEST_CONFIG_OVERLAYS", "1")
	root := t.TempDir()
	t.Setenv("HOME", root)
	writeConfig := func(name, data string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	envFile := writeConfig("environment.json", `{"cycleOrder":["env"],"modelRoles":{"env":"fixture/env:low","shared":"fixture/env:low"}}`)
	first := writeConfig("first config.json", `{"cycleOrder":["first","shared"],"modelRoles":{"first":"fixture/first:low","shared":"fixture/first:medium"}}`)
	second := writeConfig("second:config.json", `{"cycleOrder":["shared","env","first"],"modelRoles":{"shared":"fixture/second:high"}}`)
	t.Setenv("PI_CONFIG_FILES", envFile)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	roles, err := CycleRoles(context.Background(), Config{Binary: exe, CWD: root, Args: []string{"--model", "fixture/initial", "--config", "first config.json", "--config=~/second:config.json"}})
	want := []ModelRole{{Role: "shared", Selector: "fixture/second:high"}, {Role: "env", Selector: "fixture/env:low"}, {Role: "first", Selector: "fixture/first:low"}}
	if err != nil || !reflect.DeepEqual(roles, want) {
		t.Fatalf("ordered overlays = %#v, want %#v, err=%v", roles, want, err)
	}
	roles, err = CycleRoles(context.Background(), Config{Binary: exe, CWD: root, Args: []string{"--config=" + second, "--config", first}})
	want = []ModelRole{{Role: "first", Selector: "fixture/first:low"}, {Role: "shared", Selector: "fixture/first:medium"}}
	if err != nil || !reflect.DeepEqual(roles, want) {
		t.Fatalf("reversed overlays = %#v, want %#v, err=%v", roles, want, err)
	}
	if got := os.Getenv("PI_CONFIG_FILES"); got != envFile {
		t.Fatal("model query changed the parent configuration environment")
	}
}

func TestCycleRolesRejectsInvalidConfigWithoutFallback(t *testing.T) {
	t.Setenv("OMP_TEST_CONFIG_OVERLAYS", "1")
	t.Setenv("PI_CONFIG_FILES", "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	invalid := filepath.Join(root, "private-invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"cycleOrder":`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--config"}, {"--config="}, {"--config", "private-missing.json"}, {"--config", invalid}} {
		roles, err := CycleRoles(context.Background(), Config{Binary: exe, CWD: root, Args: args})
		if err == nil || roles != nil || strings.Contains(err.Error(), "private-") {
			t.Fatalf("invalid overlay did not fail safely: roles=%v, err=%v", roles, err)
		}
	}
}

func TestSetModelRoleVerifiesStateAndPreservesUnrelatedOutput(t *testing.T) {
	t.Setenv("OMP_TEST_MODEL_MODE", "success")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client, err := Start(context.Background(), Config{Binary: exe}, testRPCLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	model, err := client.SetModelRole(context.Background(), "slow")
	if err != nil || model != (Model{Provider: "fixture", ID: "family/deep"}) {
		t.Fatalf("model = %#v, error = %v", model, err)
	}
	event := nextSessionEvent(t, client)
	if event["text"] != "unrelated private output" {
		t.Fatalf("unrelated event lost: %v", event)
	}
	select {
	case event := <-client.Events():
		t.Fatalf("native success leaked: %s", event)
	default:
	}
}

func TestSetModelRoleStopsUncertainClient(t *testing.T) {
	for _, mode := range []string{"mismatch", "agent", "missing", "delay"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OMP_TEST_MODEL_MODE", mode)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			client, err := Start(context.Background(), Config{Binary: exe}, testRPCLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if _, err := client.SetModelRole(ctx, "slow"); err == nil {
				t.Fatal("uncertain model selection succeeded")
			}
			select {
			case <-client.stop:
			default:
				t.Fatal("uncertain client remained usable")
			}
			if !client.metadataUnavailable {
				t.Fatal("interrupted metadata correlation remained usable")
			}
		})
	}
}

func TestModelRoleRejectsPromptInjection(t *testing.T) {
	client := &Client{}
	for _, role := range []string{"slow\nrun this", "slow extra", "@slow", "slow:high", ""} {
		if _, err := client.SetModelRole(context.Background(), role); err == nil {
			t.Fatalf("unsafe role accepted: %q", role)
		}
	}
}
