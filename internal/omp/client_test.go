package omp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func testRPCLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

const rpcSensitiveCanary = "SECRET_PROMPT SECRET_OUTPUT SECRET_TOKEN https://api.telegram.org/botSECRET_TOKEN/ Authorization: SECRET_HEADER"

func recordFixtureChildEnvironment() {
	path := os.Getenv("OMP_TEST_CHILD_ENV_RESULT")
	if path == "" {
		return
	}
	tokenPresent := false
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "OMP_TELEGRAM_BOT_TOKEN" {
			tokenPresent = true
			break
		}
	}
	data := fmt.Sprintf("%t\n%s\n%s\n%s\n", tokenPresent, os.Getenv("OMP_TEST_OMP_ENV"), os.Getenv("OMP_TELEGRAM_FIXTURE_FLAG"), os.Getenv("PI_CONFIG_FILES"))
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		os.Exit(3)
	}
}

func TestMain(m *testing.M) {
	recordFixtureChildEnvironment()
	fixtureResumeModelChild()
	if len(os.Args) > 1 && os.Args[1] == "config" {
		fixtureModelConfig()
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "acp" {
		fixtureACP()
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "--mode" {
		fixtureResumeModelStart()
		fixtureDiscoveryStart()
		fixtureRPCUIStart()
		if os.Getenv("OMP_TEST_DISCOVERY_WAIT_READY") == "1" {
			fixtureDiscoveryWrite("waiting", "ready")
			_, _ = io.Copy(io.Discard, os.Stdin)
			os.Exit(0)
		}
		if os.Getenv("OMP_TEST_RPC_EOF_BEFORE_READY") == "1" {
			_ = os.Stdout.Close()
			_, _ = io.Copy(io.Discard, os.Stdin)
			os.Exit(0)
		}
		fmt.Println(`{"type":"ready","protocolVersion":1,"supportedProtocolVersions":[1,2],"maxFrameBytes":1048576,"maxReassembledFrameBytes":67108864}`)
		input := bufio.NewReader(os.Stdin)
		var deferredResponse map[string]any
		for {
			line, err := input.ReadBytes('\n')
			if err != nil {
				fixtureDiscoveryFinish()
				os.Exit(0)
			}
			var command map[string]any
			if json.Unmarshal(line, &command) != nil {
				os.Exit(1)
			}
			fixtureDiscoveryRecord(command)
			if fixtureRPCUICommand(command) {
				continue
			}
			if fixtureResumeModelCommand(command) {
				continue
			}
			if fixtureMigrationCommand(command) {
				continue
			}
			if fixtureUsageCommand(command) {
				continue
			}
			if fixtureSessionCommand(command) {
				continue
			}
			if fixtureModelCommand(command) {
				continue
			}
			if fixtureAutoresearchCommand(command) {
				continue
			}
			if fixtureCatalogCommand(command) {
				continue
			}
			switch command["type"] {
			case "deferred_correlation":
				reply := map[string]any{"type": "response", "id": command["id"], "command": "deferred_correlation", "success": true, "data": map[string]any{"value": command["value"]}}
				if deferredResponse == nil {
					deferredResponse = reply
					continue
				}
				encoded, _ := json.Marshal(reply)
				fmt.Println(string(encoded))
				encoded, _ = json.Marshal(deferredResponse)
				fmt.Println(string(encoded))
				deferredResponse = nil
			case "shutdown_probe":
				fmt.Printf("{\"type\":\"response\",\"command\":\"shutdown_probe\",\"id\":%q,\"success\":true}\n", command["id"])
				release, _ := command["release"].(string)
				marker, _ := command["marker"].(string)
				for {
					if _, err := os.Stat(release); err == nil {
						break
					}
					time.Sleep(time.Millisecond)
				}
				queued, err := input.ReadBytes('\n')
				if err == nil {
					if err := os.WriteFile(marker, queued, 0600); err != nil {
						os.Exit(3)
					}
				}
				os.Exit(0)
			case "rejected":
				fmt.Printf("{\"type\":\"response\",\"command\":\"rejected\",\"id\":%q,\"success\":false,\"error\":\"SECRET_REJECTION\"}\n", command["id"])
			case "unknown":
				fmt.Println(`{"type":"response","command":"unknown","success":false}`)
			case "malformed":
				fmt.Printf("{\"type\":\"response\",\"command\":\"malformed\",\"id\":%q,\"error\":%q}\n", command["id"], rpcSensitiveCanary)
			case "peer":
				fmt.Println(`{"type":"response","id":"peer-secret","command":"peer","success":true}`)
			case "prefetched_events":
				fmt.Fprint(os.Stdout, "{\"type\":\"agent_start\"}\n{\"type\":\"extension_ui_request\",\"id\":\"next-dialog\",\"method\":\"ask\",\"questions\":[]}\n")
			case "partial_physical_line":
				fmt.Fprint(os.Stdout, `{"type":"extension_ui_request","id":"fragment-dialog","method":"ask","questions":[`)
			case "release_partial_physical_line":
				fmt.Fprint(os.Stdout, "]}\n")
			case "partial_physical_line_eof":
				_ = os.Stdout.Close()
				_, _ = io.Copy(io.Discard, os.Stdin)
				os.Exit(0)
			case "overflow":
				for range 200 {
					fmt.Println(`{"type":"agent_start"}`)
				}
			case "byte_overflow":
				// The logical reservation fits alone; an undelivered event must
				// exhaust the shared budget before the event-slot limit is reached.
				const margin = 4 << 10
				frame := []byte(`{"type":"agent_start","padding":"` + strings.Repeat("x", 2*margin) + `"}` + "\n")
				_, _ = os.Stdout.Write(frame)
				length := maxRPCBufferedBytes - margin
				count := (length + chunkPayload - 1) / chunkPayload
				fmt.Println(string(chunkFrame("byte-budget", 0, count, length, []byte("{"))))
			default:
				reply := map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": true, "data": map[string]any{"accepted": true, "private": rpcSensitiveCanary}}
				encoded, _ := json.Marshal(reply)
				fmt.Println(string(encoded))
				if command["type"] == "prompt" {
					reply["success"] = false
					reply["error"] = rpcSensitiveCanary
					encoded, _ = json.Marshal(reply)
					fmt.Println(string(encoded))
				}
			}
		}
	}
	// Only child fixtures inherit this; the test process keeps its race exit window.
	if err := os.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0")); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func childEnvironmentCaptureScript() string {
	return "token_present=false\n" +
		"if [ \"${OMP_TELEGRAM_BOT_TOKEN+x}\" = x ]; then token_present=true; fi\n" +
		"printf '%s\\n%s\\n%s\\n%s\\n' \"$token_present\" \"$OMP_TEST_OMP_ENV\" \"$OMP_TELEGRAM_FIXTURE_FLAG\" \"$PI_CONFIG_FILES\" > \"$OMP_TEST_CHILD_ENV_RESULT\"\n"
}

func assertChildEnvironment(t *testing.T, path string, wantConfigFiles []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("child environment fixture did not record its environment")
	}
	fields := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(fields) != 4 || fields[0] != "true" {
		t.Fatal("zero-value child environment policy did not inherit the bridge token")
	}
	if fields[1] != "preserved-runtime-setting" || fields[2] != "preserved-telegram-fixture" {
		t.Fatal("child process did not preserve non-secret OMP environment")
	}
	gotConfigFiles := filepath.SplitList(fields[3])
	if len(gotConfigFiles) != len(wantConfigFiles) {
		t.Fatal("child process did not preserve PI_CONFIG_FILES overlays")
	}
	for i, want := range wantConfigFiles {
		if gotConfigFiles[i] != want {
			t.Fatal("child process did not preserve PI_CONFIG_FILES overlays")
		}
	}
}

func TestOMPChildProcessesUseZeroValueEnvironmentPolicy(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "child-environment")
	t.Setenv("OMP_TELEGRAM_BOT_TOKEN", "fixture-only-not-a-credential")
	t.Setenv("OMP_TEST_OMP_ENV", "preserved-runtime-setting")
	t.Setenv("OMP_TELEGRAM_FIXTURE_FLAG", "preserved-telegram-fixture")
	t.Setenv("OMP_TEST_CHILD_ENV_RESULT", capture)
	t.Setenv("PI_CONFIG_FILES", "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := os.Remove(capture); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	client, err := Start(ctx, Config{Binary: exe}, testRPCLogger())
	if err != nil {
		t.Fatal("runtime child failed to start")
	}
	if err := client.Close(); err != nil {
		t.Fatal("runtime child failed to close")
	}
	assertChildEnvironment(t, capture, nil)

	if err := os.Remove(capture); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	listCfg := listFixture(t, "normal")
	writeListPage(t, listCfg, 0, []map[string]any{}, "")
	if _, err := ListSessions(ctx, listCfg); err != nil {
		t.Fatal("ACP listing child failed")
	}
	assertChildEnvironment(t, capture, nil)

	root := t.TempDir()
	baseConfig := filepath.Join(root, "environment.json")
	cliConfig := filepath.Join(root, "cli.json")
	if err := os.WriteFile(baseConfig, []byte(`{"cycleOrder":["base"],"modelRoles":{"base":"fixture/base:low"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliConfig, []byte(`{"cycleOrder":["overlay"],"modelRoles":{"overlay":"fixture/overlay:high"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TEST_CONFIG_OVERLAYS", "1")
	t.Setenv("PI_CONFIG_FILES", baseConfig)
	if err := os.Remove(capture); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	roles, err := CycleRoles(ctx, Config{Binary: exe, CWD: root, Args: []string{"--config", cliConfig}})
	if err != nil || len(roles) != 1 || roles[0] != (ModelRole{Role: "overlay", Selector: "fixture/overlay:high"}) {
		t.Fatal("model config child did not apply the PI_CONFIG_FILES overlay")
	}
	assertChildEnvironment(t, capture, []string{baseConfig, cliConfig})

	if err := os.Remove(capture); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "workspace")
	session := filepath.Join(cwd, "session.jsonl")
	writeSessionHeader(t, session, "native-id", cwd)
	renderBinary := filepath.Join(root, "omp-render")
	renderScript := "#!/bin/sh\n" +
		"[ \"$1\" = render ] && [ \"$2\" = native-id ] && [ \"$3\" = -q ] && [ \"$4\" = -t ] || exit 11\n" +
		childEnvironmentCaptureScript() +
		"printf 'session  %s\\nopen  fixture\\n' \"$PWD/session.jsonl\" >&2\n"
	if err := os.WriteFile(renderBinary, []byte(renderScript), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSessionPath(ctx, Config{Binary: renderBinary, CWD: cwd}, "native-id"); err != nil {
		t.Fatal("render child failed")
	}
	assertChildEnvironment(t, capture, []string{baseConfig})

	if err := os.Remove(capture); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	input := filepath.Join(root, "export-session.jsonl")
	output := filepath.Join(root, "export.html")
	if err := os.WriteFile(input, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	exportBinary := filepath.Join(root, "omp-export")
	exportScript := "#!/bin/sh\n" +
		"[ \"$1\" = --export ] || exit 11\n" +
		childEnvironmentCaptureScript() +
		"printf '%s' '<html>fixture</html>' > \"$3\"\n"
	if err := os.WriteFile(exportBinary, []byte(exportScript), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ExportHTML(ctx, exportBinary, input, output, Environment{}); err != nil {
		t.Fatal("HTML export child failed")
	}
	assertChildEnvironment(t, capture, []string{baseConfig})
}

func TestOMPChildProcessesUseAllowlistEnvironment(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(root, "child-environment")
	t.Setenv("OMP_TEST_CHILD_ENV_RESULT", capture)
	t.Setenv("OMP_TEST_OMP_ENV", "preserved-runtime-setting")
	t.Setenv("OMP_TELEGRAM_FIXTURE_FLAG", "excluded-parent-setting")
	t.Setenv("OMP_TELEGRAM_BOT_TOKEN", "secret")
	t.Setenv("PI_CONFIG_FILES", filepath.Join(root, "excluded-parent-config.json"))
	policy, err := NewEnvironment("allowlist", []string{"OMP_TEST_CHILD_ENV_RESULT", "OMP_TEST_OMP_ENV"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	assertCaptured := func() {
		t.Helper()
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatalf("native child did not record environment: %v", err)
		}
		if got := string(data); got != "false\npreserved-runtime-setting\n\n\n" {
			t.Fatalf("native child received unlisted parent environment: %q", got)
		}
		if err := os.Remove(capture); err != nil {
			t.Fatal(err)
		}
	}
	client, err := Start(ctx, Config{Binary: exe, Environment: policy}, testRPCLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	assertCaptured()
	listCfg := listFixture(t, "normal")
	listCfg.Environment = policy
	writeListPage(t, listCfg, 0, []map[string]any{}, "")
	if _, err := ListSessions(ctx, listCfg); err != nil {
		t.Fatal(err)
	}
	assertCaptured()
	cwd := filepath.Join(root, "workspace")
	file := filepath.Join(cwd, "session.jsonl")
	writeSessionHeader(t, file, "native-id", cwd)
	renderBinary := filepath.Join(root, "omp-render")
	renderScript := "#!/bin/sh\n" + childEnvironmentCaptureScript() +
		"printf 'session  %s\\n' \"$PWD/session.jsonl\" >&2\n"
	if err := os.WriteFile(renderBinary, []byte(renderScript), 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveSessionPath(ctx, Config{Binary: renderBinary, CWD: cwd, Environment: policy}, "native-id"); err != nil || got != file {
		t.Fatalf("rendered session = %q, error = %v", got, err)
	}
	assertCaptured()
	input := filepath.Join(root, "export.jsonl")
	output := filepath.Join(root, "export.html")
	if err := os.WriteFile(input, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	exportBinary := filepath.Join(root, "omp-export")
	exportScript := "#!/bin/sh\n" + childEnvironmentCaptureScript() +
		"printf '<html>fixture</html>' > \"$3\"\n"
	if err := os.WriteFile(exportBinary, []byte(exportScript), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ExportHTML(ctx, exportBinary, input, output, policy); err != nil {
		t.Fatal(err)
	}
	assertCaptured()
}

func TestOMPChildProcessesUseDenylistEnvironment(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(root, "child-environment")
	t.Setenv("OMP_TEST_CHILD_ENV_RESULT", capture)
	t.Setenv("OMP_TEST_OMP_ENV", "preserved-runtime-setting")
	t.Setenv("OMP_TELEGRAM_FIXTURE_FLAG", "parent-secret")
	t.Setenv("PI_CONFIG_FILES", filepath.Join(root, "parent-config.json"))
	t.Setenv(botTokenName, "bot-secret")
	policy, err := NewEnvironment("denylist", nil, []string{"OMP_TELEGRAM_FIXTURE_FLAG", "PI_CONFIG_FILES"})
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	assertCaptured := func() {
		t.Helper()
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatalf("native child did not record environment: %v", err)
		}
		if got := string(data); got != "true\npreserved-runtime-setting\n\n\n" {
			t.Fatalf("denylist lost an unlisted bot token or preserved a denied variable: %q", got)
		}
		if err := os.Remove(capture); err != nil {
			t.Fatal(err)
		}
	}
	client, err := Start(ctx, Config{Binary: exe, Environment: policy}, testRPCLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	assertCaptured()
	listCfg := listFixture(t, "normal")
	listCfg.Environment = policy
	writeListPage(t, listCfg, 0, []map[string]any{}, "")
	if _, err := ListSessions(ctx, listCfg); err != nil {
		t.Fatal(err)
	}
	assertCaptured()
	if got := os.Getenv("OMP_TELEGRAM_FIXTURE_FLAG"); got != "parent-secret" {
		t.Fatal("exclusion changed the bridge parent environment")
	}
}

func TestOMPChildProcessReceivesExplicitlyAllowlistedBotToken(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "child-environment")
	t.Setenv("OMP_TEST_CHILD_ENV_RESULT", capture)
	t.Setenv(botTokenName, "fixture-only-not-a-credential")
	policy, err := NewEnvironment("allowlist", []string{"OMP_TEST_CHILD_ENV_RESULT", botTokenName}, nil)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Start(ctx, Config{Binary: exe, Environment: policy}, testRPCLogger())
	if err != nil {
		t.Fatal("runtime child failed to start")
	}
	if err := client.Close(); err != nil {
		t.Fatal("runtime child failed to close")
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal("runtime child did not record its environment")
	}
	if !strings.HasPrefix(string(data), "true\n") {
		t.Fatal("explicitly allowlisted bot token did not reach the OMP process")
	}
}

func TestReadLargePhysicalFrame(t *testing.T) {
	payload := strings.Repeat("x", 128<<10) + "\n"
	got, err := readLine(bufio.NewReaderSize(strings.NewReader(payload), 4096), maxFrame)
	if err != nil || string(got) != payload[:len(payload)-1] {
		t.Fatalf("large frame lost: %v", err)
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader(payload)), 1024); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader("{}")), maxFrame); !errors.Is(err, errProtocol) {
		t.Fatalf("unterminated frame error = %v, want protocol error", err)
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader("")), maxFrame); !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream error = %v, want EOF", err)
	}
	for _, payload := range []string{"", "{}"} {
		reader := bufio.NewReader(io.MultiReader(strings.NewReader(payload), iotest.ErrReader(os.ErrClosed)))
		_, err := readLine(reader, maxFrame)
		wantErr := os.ErrClosed
		if payload != "" {
			wantErr = errProtocol
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("closed stream with %q: error = %v, want %v", payload, err, wantErr)
		}
	}
}

func chunkFrame(id string, index, count, length int, data []byte) []byte {
	encoded, _ := json.Marshal(map[string]any{"type": "rpc_chunk", "chunkId": id, "index": index, "count": count, "byteLength": length, "data": base64.StdEncoding.EncodeToString(data)})
	return encoded
}

func TestChunkReassemblyAndOrder(t *testing.T) {
	payload := []byte(`{"type":"message_end","text":"` + strings.Repeat("a", 1100) + `"}`)
	d := frameDecoder{physical: 1024, logical: maxLogical}
	first := chunkFrame("one", 0, 2, len(payload), payload[:600])
	second := chunkFrame("one", 1, 2, len(payload), payload[600:])
	if got, err := d.push(first); err != nil || got != nil {
		t.Fatalf("first chunk: %v", err)
	}
	got, err := d.push(second)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("reassembly: %v", err)
	}
	for _, interruption := range [][]byte{first, chunkFrame("two", 1, 2, len(payload), payload[600:]), []byte(`{"type":"agent_end"}`)} {
		d = frameDecoder{physical: 1024, logical: maxLogical}
		d.push(first)
		if _, err := d.push(interruption); err == nil {
			t.Fatal("invalid chunk sequence accepted")
		}
	}
}

func TestChunksRejectInvalidEncodingAndLength(t *testing.T) {
	for _, bad := range []string{"YQ==\n", "YR==", "!!!="} {
		d := frameDecoder{physical: 1, logical: maxLogical}
		frame, _ := json.Marshal(map[string]any{"type": "rpc_chunk", "chunkId": "one", "index": 0, "count": 2, "byteLength": 2, "data": bad})
		if _, err := d.push(frame); err == nil {
			t.Fatal("invalid base64 accepted")
		}
	}
	d := frameDecoder{physical: 1, logical: maxLogical}
	d.push(chunkFrame("one", 0, 2, 2, []byte{'{'}))
	if _, err := d.push(chunkFrame("one", 1, 2, 2, []byte{0xff})); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
	d = frameDecoder{physical: 1, logical: maxLogical}
	d.push(chunkFrame("one", 0, 2, 4, []byte{'{'}))
	if _, err := d.push(chunkFrame("one", 1, 2, 4, []byte{'}'})); err == nil {
		t.Fatal("short reassembly accepted")
	}
}

func FuzzFrameDecoder(f *testing.F) {
	for _, seed := range []struct {
		data, splits []byte
	}{
		{data: []byte(`{"type":"agent_start"}`), splits: []byte{1, 4}},
		{data: []byte(`{"type":"rpc_chunk","chunkId":"x","index":0,"count":2,"byteLength":4,"data":"ew=="}`), splits: []byte{0, 1}},
		{data: []byte{0, 1, 2, 3, 255}, splits: []byte{2, 3}},
	} {
		f.Add(seed.data, seed.splits)
	}
	f.Fuzz(func(t *testing.T, data, splits []byte) {
		d := frameDecoder{physical: 1024, logical: maxLogical}
		_, _ = d.push(data)
		if len(data) > 1<<20 {
			data = data[:1<<20]
		}
		payload, err := json.Marshal(map[string]string{"type": "agent_end", "text": string(data)})
		if err != nil {
			t.Fatal(err)
		}
		positions := []int{0, len(payload)}
		for _, raw := range splits {
			if len(positions) >= 34 {
				break
			}
			positions = append(positions, 1+int(raw)%(len(payload)-1))
		}
		sort.Ints(positions)
		unique := positions[:0]
		for _, position := range positions {
			if len(unique) == 0 || unique[len(unique)-1] != position {
				unique = append(unique, position)
			}
		}
		positions = unique
		if len(positions) < 3 {
			positions = []int{0, len(payload) / 2, len(payload)}
		}
		d = frameDecoder{physical: 1, logical: maxLogical}
		count := len(positions) - 1
		for i := 0; i < count; i++ {
			got, err := d.push(chunkFrame("fuzz", i, count, len(payload), payload[positions[i]:positions[i+1]]))
			if err != nil {
				t.Fatal(err)
			}
			if i+1 == count && !bytes.Equal(got, payload) {
				t.Fatalf("reassembled payload differs: got %q want %q", got, payload)
			}
		}
	})
}

func fixtureClient(t *testing.T) *Client {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	client, err := Start(ctx, Config{Binary: binary}, testRPCLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestCallWithIDCorrelatesOutOfOrderResponsesAndRejectsPendingCollision(t *testing.T) {
	client := fixtureClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	firstID, secondID := client.ReserveRequestID(), client.ReserveRequestID()
	if firstID == secondID {
		t.Fatal("reserved request IDs collided")
	}
	client.writeGate <- struct{}{}
	gateHeld := true
	defer func() {
		if gateHeld {
			<-client.writeGate
		}
	}()
	type callResult struct {
		data json.RawMessage
		err  error
	}
	first := make(chan callResult, 1)
	go func() {
		data, err := client.CallWithID(ctx, firstID, "deferred_correlation", map[string]any{"value": "first"})
		first <- callResult{data: data, err: err}
	}()
	for {
		client.mu.Lock()
		_, registered := client.pending[firstID]
		client.mu.Unlock()
		if registered {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("first request was not registered before writing")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := client.CallWithID(ctx, firstID, "deferred_correlation", map[string]any{"value": "duplicate"}); err == nil {
		t.Fatal("pending request ID collision was accepted")
	}
	<-client.writeGate
	gateHeld = false
	secondData, err := client.CallWithID(ctx, secondID, "deferred_correlation", map[string]any{"value": "second"})
	if err != nil {
		t.Fatal(err)
	}
	firstResult := <-first
	if firstResult.err != nil {
		t.Fatal(firstResult.err)
	}
	for _, check := range []struct {
		data json.RawMessage
		want string
	}{{firstResult.data, "first"}, {secondData, "second"}} {
		var decoded struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(check.data, &decoded); err != nil || decoded.Value != check.want {
			t.Fatalf("response correlation: got %s, want %q (error %v)", check.data, check.want, err)
		}
	}
	client.mu.Lock()
	remaining := len(client.pending)
	client.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("requests left pending after responses: %d", remaining)
	}
}

func TestTerminateNowKillsBeforeQueuedStdinCanDrain(t *testing.T) {
	client, release, marker := shutdownProbeClient(t)
	result := make(chan error, 1)
	go func() { result <- client.TerminateNow() }()
	select {
	case <-client.stop:
	case <-time.After(time.Second):
		t.Fatal("immediate termination did not signal shutdown")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("hard stop waited for graceful stdin drain")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child processed queued stdin after hard stop: %v", err)
	}
	if err := client.TerminateNow(); err != nil {
		t.Fatalf("repeated hard stop: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close after hard stop: %v", err)
	}
}

func TestCloseStillDrainsQueuedStdin(t *testing.T) {
	client, release, marker := shutdownProbeClient(t)
	result := make(chan error, 1)
	go func() { result <- client.Close() }()
	select {
	case <-client.stop:
	case <-time.After(time.Second):
		t.Fatal("close did not signal shutdown")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("graceful close did not complete")
	}
	queued, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("queued request was not drained: %v", err)
	}
	var frame struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(queued, &frame); err != nil || frame.Type != "queued_after_probe" {
		t.Fatalf("unexpected queued frame: %s (%v)", queued, err)
	}
}

func shutdownProbeClient(t *testing.T) (*Client, string, string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client, err := Start(context.Background(), Config{Binary: binary}, testRPCLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	release := filepath.Join(t.TempDir(), "release")
	marker := filepath.Join(t.TempDir(), "consumed")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Call(ctx, "shutdown_probe", map[string]any{"release": release, "marker": marker}); err != nil {
		t.Fatal(err)
	}
	if err := client.Send(ctx, map[string]any{"type": "queued_after_probe", "id": client.ReserveRequestID()}); err != nil {
		t.Fatal(err)
	}
	return client, release, marker
}

func TestLatePromptResponseIsDiscarded(t *testing.T) {
	client := fixtureClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Call(ctx, "prompt", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-client.Events():
		t.Fatalf("late response leaked as event: %s", data)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestUncorrelatedAndMalformedResponsesFailClosed(t *testing.T) {
	for _, command := range []string{"unknown", "malformed"} {
		t.Run(command, func(t *testing.T) {
			client := fixtureClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := client.Call(ctx, command, nil)
			if err == nil || ctx.Err() != nil {
				t.Fatalf("response did not immediately fail closed: %v", err)
			}
			if ClassifyError(err) != "protocol" {
				t.Fatalf("protocol failure classified as %q", ClassifyError(err))
			}
		})
	}
}

func TestEventOverflowStopsProcess(t *testing.T) {
	client := fixtureClient(t)
	if err := client.Send(context.Background(), map[string]any{"type": "overflow"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.Done():
		if err := client.failure(); ClassifyError(err) != "queue_overflow" {
			t.Fatalf("wrong failure: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("overflow left process running")
	}
}

func TestEventByteBudgetOverflowAndConsumption(t *testing.T) {
	t.Run("overflow before queue slots", func(t *testing.T) {
		client := fixtureClient(t)
		if err := client.Send(context.Background(), map[string]any{"type": "byte_overflow"}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-client.Done():
			if err := client.failure(); ClassifyError(err) != "queue_overflow" || !errors.Is(err, errQueueOverflow) {
				t.Fatalf("byte overflow did not fail closed: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("byte overflow left process running")
		}
	})

	t.Run("receive restores byte credit", func(t *testing.T) {
		c := &Client{events: make(chan json.RawMessage), queuedEvents: make(chan json.RawMessage, 128), stop: make(chan struct{})}
		forwarded := make(chan struct{})
		go func() {
			c.forwardEvents()
			close(forwarded)
		}()
		defer func() {
			close(c.stop)
			<-forwarded
		}()
		frame := json.RawMessage(make([]byte, 40<<20))
		if _, ok := c.bytes.reserve(cap(frame)); !ok {
			t.Fatal("first event rejected")
		}
		c.queuedEvents <- frame
		d := frameDecoder{physical: 1024, logical: maxLogical, budget: &c.bytes}
		firstChunk := chunkFrame("budget", 0, 2, 30<<20, []byte("{"))
		if _, err := d.push(firstChunk); err != errQueueOverflow {
			t.Fatalf("decoder admitted a frame beyond the shared byte budget: %v", err)
		}
		select {
		case <-c.Events():
		case <-time.After(time.Second):
			t.Fatal("forwarder did not deliver event")
		}
		deadline := time.After(time.Second)
		for {
			result, err := d.push(firstChunk)
			if err == nil {
				if result != nil || cap(d.data) != 30<<20 {
					t.Fatal("decoder did not reserve the announced reassembly capacity")
				}
				c.bytes.release(cap(d.data))
				break
			}
			if err != errQueueOverflow {
				t.Fatalf("decoder rejected frame after consumption: %v", err)
			}
			select {
			case <-deadline:
				t.Fatal("receiving an event did not restore decoder byte credit")
			default:
				time.Sleep(time.Millisecond)
			}
		}
	})
}

func TestRejectedRPCExposesOnlySafeFailureKind(t *testing.T) {
	client := fixtureClient(t)
	_, err := client.Call(context.Background(), "rejected", nil)
	if err == nil || ClassifyError(err) != "rejected" || strings.Contains(err.Error(), "SECRET_REJECTION") {
		t.Fatalf("unsafe or incorrect rejection classification: %v", err)
	}
	if _, err := client.Call(context.Background(), "get_state", nil); err != nil {
		t.Fatalf("rejection closed the client: %v", err)
	}
}

func bufferedRPCLogger(format string) (*slog.Logger, *bytes.Buffer) {
	var output bytes.Buffer
	options := &slog.HandlerOptions{Level: slog.LevelDebug}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(&output, options)), &output
	}
	return slog.New(slog.NewTextHandler(&output, options)), &output
}

func TestStartupTransportEOFLogsWarning(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TEST_RPC_EOF_BEFORE_READY", "1")
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Start(ctx, Config{Binary: binary, Args: []string{"--review-canary", rpcSensitiveCanary}}, logger)
	if client != nil {
		t.Fatal("startup returned a client after transport EOF")
	}
	if err == nil || ClassifyError(err) != "process_exit" {
		t.Fatalf("startup EOF error = %v", err)
	}
	logs := output.String()
	if strings.Count(logs, "event=rpc_process_exit") != 1 || !strings.Contains(logs, "level=WARN") {
		t.Fatalf("startup EOF warning missing or duplicated: %s", logs)
	}
	for _, canary := range []string{"SECRET_PROMPT", "SECRET_OUTPUT", "SECRET_TOKEN", "SECRET_HEADER", "https://api.telegram.org", "Authorization"} {
		if strings.Contains(logs, canary) {
			t.Fatalf("startup EOF log leaked %q: %s", canary, logs)
		}
	}
}

func TestNormalCloseAndContextCancellationDoNotWarn(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("close", func(t *testing.T) {
		logger, output := bufferedRPCLogger("text")
		client, err := Start(context.Background(), Config{Binary: binary}, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
		logs := output.String()
		if strings.Contains(logs, "level=WARN") || strings.Contains(logs, "level=ERROR") {
			t.Fatalf("normal close logged at warning/error: %s", logs)
		}
		if !strings.Contains(logs, "reason=closed") {
			t.Fatalf("normal close reason missing: %s", logs)
		}
	})
	t.Run("context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		logger, output := bufferedRPCLogger("text")
		client, err := Start(ctx, Config{Binary: binary}, logger)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		cancel()
		select {
		case <-client.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("context cancellation did not stop RPC process")
		}
		logs := output.String()
		if strings.Contains(logs, "level=WARN") || strings.Contains(logs, "level=ERROR") {
			t.Fatalf("context cancellation logged at warning/error: %s", logs)
		}
		if !strings.Contains(logs, "reason=context_canceled") {
			t.Fatalf("context cancellation reason missing: %s", logs)
		}
	})
}

func TestFixtureLifecycleLogsExcludeFrameData(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			logger, output := bufferedRPCLogger(format)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Start(ctx, Config{Binary: binary}, logger)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Call(ctx, "prompt", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Call(ctx, "barrier", nil); err != nil {
				t.Fatal(err)
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			logs := output.String()
			if !strings.Contains(logs, "rpc_lifecycle") || !strings.Contains(logs, "response_queued") || !strings.Contains(logs, "response_ignored") {
				t.Fatalf("lifecycle correlation fields missing: %s", logs)
			}
			if !strings.Contains(logs, "request_id_valid") {
				t.Fatalf("request ID validity field missing: %s", logs)
			}
			for _, canary := range []string{"SECRET_PROMPT", "SECRET_OUTPUT", "SECRET_TOKEN", "SECRET_HEADER", "https://api.telegram.org", "Authorization"} {
				if strings.Contains(logs, canary) {
					t.Fatalf("frame data leaked into %s logs: %q", format, canary)
				}
			}
		})
	}
}

func TestRejectedFrameLogsProtocolErrorWithoutFrame(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			logger, output := bufferedRPCLogger(format)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Start(ctx, Config{Binary: binary}, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.Call(ctx, "malformed", nil); err == nil {
				t.Fatal("malformed response was accepted")
			}
			select {
			case <-client.Done():
			case <-time.After(6 * time.Second):
				t.Fatal("malformed frame did not stop client")
			}
			logs := output.String()
			if !strings.Contains(logs, "rpc_protocol_error") {
				t.Fatalf("protocol error event missing: %s", logs)
			}
			for _, canary := range []string{"SECRET_PROMPT", "SECRET_OUTPUT", "SECRET_TOKEN", "SECRET_HEADER", "https://api.telegram.org", "Authorization"} {
				if strings.Contains(logs, canary) {
					t.Fatalf("rejected frame leaked into logs: %q", canary)
				}
			}
		})
	}
}

func TestClientIDsAreProcessGlobalAndCorrelationIDsStayNumeric(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	first, err := Start(context.Background(), Config{Binary: binary}, testRPCLogger())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Start(context.Background(), Config{Binary: binary}, testRPCLogger())
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	defer first.Close()
	defer second.Close()
	if first.ID() == 0 || second.ID() == 0 || first.ID() == second.ID() {
		t.Fatalf("client IDs are not unique: %d, %d", first.ID(), second.ID())
	}
}

func TestPeerCorrelationIDIsOmittedFromLifecycleLogs(t *testing.T) {
	logger, output := bufferedRPCLogger("json")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client, err := Start(context.Background(), Config{Binary: binary}, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := client.Call(ctx, "peer", nil); err == nil {
		t.Fatal("uncorrelated peer response unexpectedly completed call")
	}
	barrier, stopBarrier := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopBarrier()
	if _, err := client.Call(barrier, "barrier", nil); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	logs := output.String()
	if !strings.Contains(logs, `"phase":"response_ignored"`) || !strings.Contains(logs, `"request_id_valid":false`) {
		t.Fatalf("correlation metadata missing: %s", logs)
	}
	if strings.Contains(logs, "peer-secret") {
		t.Fatal("peer correlation ID leaked into lifecycle logs")
	}
}

func TestConsumerPhaseRestorationAndRedaction(t *testing.T) {
	for _, test := range []struct {
		name, phase, want string
		nested            bool
	}{
		{name: "nested phase retains outer elapsed time", phase: "callback", want: "callback", nested: true},
		{name: "unknown phase is redacted", phase: rpcSensitiveCanary, want: "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			c := &Client{log: slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo})), stop: make(chan struct{})}
			c.BeginConsumerPhase(test.phase)
			if test.nested {
				time.Sleep(10 * time.Millisecond)
				previous := c.BeginConsumerPhase("telegram")
				c.EndConsumerPhase(previous)
			}
			c.queueOverflow(errQueueOverflow, queueOverflowDetails{reason: "byte_budget", stage: "physical_read"})
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["consumer_phase"] != test.want || strings.Contains(output.String(), "SECRET_") {
				t.Fatalf("unsafe or unrestored consumer phase: %s", output.String())
			}
			if test.nested {
				if elapsed, ok := record["consumer_phase_ms"].(float64); !ok || elapsed < 5 {
					t.Fatalf("restoration restarted outer phase elapsed time: %v", record["consumer_phase_ms"])
				}
			}
		})
	}

	c := &Client{}
	c.BeginConsumerPhase("operation")
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for range 1000 {
			previous := c.BeginConsumerPhase("callback")
			c.EndConsumerPhase(previous)
		}
	}()
	for {
		phase, elapsed := c.consumerPhaseSnapshot()
		if (phase != "operation" && phase != "callback") || elapsed < 0 {
			t.Fatalf("incoherent concurrent phase snapshot: %q, %d", phase, elapsed)
		}
		select {
		case <-finished:
			return
		default:
		}
	}
}

func TestPhysicalReadBudgetFailureRetainsPreReleaseSnapshot(t *testing.T) {
	var budget byteBudget
	initial := int64(maxRPCBufferedBytes - 24)
	if _, ok := budget.reserve(int(initial)); !ok {
		t.Fatal("cannot reserve initial byte credit")
	}
	var failure byteBudgetFailure
	reader := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 40)+"\n"), 16)
	if data, err := readLineBudget(reader, 1024, &budget, &failure); data != nil || err != errQueueOverflow {
		t.Fatalf("physical read did not retain overflow sentinel: data=%v err=%v", data, err)
	}
	if failure.used != initial+16 || failure.requested != 16 || budget.usedBytes() != initial {
		t.Fatalf("failure snapshot lost partial-frame credit: failure=%+v current=%d", failure, budget.usedBytes())
	}
}

func TestReadAheadFlagPreservesFrameBudget(t *testing.T) {
	for _, prefetched := range []bool{false, true} {
		t.Run(fmt.Sprintf("prefetched=%t", prefetched), func(t *testing.T) {
			c := &Client{}
			wire := "{\"type\":\"extension_ui_request\",\"id\":\"next-dialog\",\"method\":\"ask\",\"questions\":[]}\n"
			initial := maxRPCBufferedBytes - len(wire)
			if _, ok := c.bytes.reserve(initial); !ok {
				t.Fatal("cannot reserve boundary credit")
			}
			c.bytes.setReadAhead(prefetched)
			var failure byteBudgetFailure
			frame, err := readLineBudget(bufio.NewReader(strings.NewReader(wire)), maxFrame, &c.bytes, &failure)
			if err != nil {
				t.Fatalf("prefetch flag consumed available frame budget: %v (%+v)", err, failure)
			}
			var event envelope
			if json.Unmarshal(frame, &event) != nil || event.Type != "extension_ui_request" || string(frame) != strings.TrimSuffix(wire, "\n") {
				t.Fatal("budget-boundary native dialog was corrupted")
			}
			if used, ok := c.bytes.reserve(1); ok || used != maxRPCBufferedBytes {
				t.Fatalf("full budget reservation returned used=%d ok=%t", used, ok)
			}
			if c.bytes.usedBytes() != maxRPCBufferedBytes || !c.BufferedOutput() {
				t.Fatal("full frame credit was not visible to the consumer")
			}
			c.bytes.release(cap(frame))
			c.bytes.release(initial)
			if c.bytes.usedBytes() != 0 || c.BufferedOutput() != prefetched {
				t.Fatal("credit release corrupted prefetch-only occupancy")
			}
			c.bytes.setReadAhead(false)
			if c.BufferedOutput() {
				t.Fatal("clearing prefetch left permanent buffered occupancy")
			}
			if used, ok := c.bytes.reserve(maxRPCBufferedBytes); !ok || used != 0 {
				t.Fatalf("handoff corrupted reusable frame budget: used=%d ok=%t", used, ok)
			}
			c.bytes.setReadAhead(true)
			c.bytes.setReadAhead(false)
			if c.bytes.usedBytes() != maxRPCBufferedBytes || !c.BufferedOutput() {
				t.Fatal("prefetch flag update destroyed frame credit")
			}
			c.bytes.release(maxRPCBufferedBytes)
			if c.BufferedOutput() {
				t.Fatal("reused frame budget retained occupancy after release")
			}
		})
	}
}

func TestReadLoopOverflowDiagnosticsAtInfo(t *testing.T) {
	makeEvent := func(kind string, padding int) []byte {
		data, err := json.Marshal(map[string]any{"type": kind, "id": rpcSensitiveCanary, "data": rpcSensitiveCanary + strings.Repeat("x", padding)})
		if err != nil {
			t.Fatal(err)
		}
		return append(data, '\n')
	}
	known := makeEvent("message_update", 0)
	unknown := makeEvent(rpcSensitiveCanary, 0)
	large := makeEvent(rpcSensitiveCanary, 2048)
	chunk := append(chunkFrame(rpcSensitiveCanary, 0, 2, 2<<20, []byte(rpcSensitiveCanary)), '\n')
	for _, test := range []struct {
		name, reason, stage, kind string
		frames                    [][]byte
		initial, rejectedUsed     int64
		rejected, queued          int
		current                   int64
	}{
		{name: "event slots known kind", reason: "event_slots", stage: "event_enqueue", kind: "message_update", frames: [][]byte{known, known}, rejectedUsed: int64(2 * len(known)), rejected: len(known), queued: 1, current: int64(len(known))},
		{name: "event slots private kind", reason: "event_slots", stage: "event_enqueue", kind: "other", frames: [][]byte{known, unknown}, rejectedUsed: int64(len(known) + len(unknown)), rejected: len(unknown), queued: 1, current: int64(len(known))},
		{name: "physical bytes", reason: "byte_budget", stage: "physical_read", kind: "other", frames: [][]byte{large}, initial: maxRPCBufferedBytes - 1024, rejectedUsed: maxRPCBufferedBytes - 1024, rejected: len(large), current: maxRPCBufferedBytes - 1024},
		{name: "chunk bytes", reason: "byte_budget", stage: "chunk_reassembly", kind: "rpc_chunk", frames: [][]byte{chunk}, initial: maxRPCBufferedBytes - 2048, rejectedUsed: int64(maxRPCBufferedBytes - 2048 + len(chunk)), rejected: 2 << 20, current: maxRPCBufferedBytes - 2048},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo}))
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			c := &Client{log: logger, stdout: reader, queuedEvents: make(chan json.RawMessage, 1), ready: make(chan struct{}), stop: make(chan struct{}), readDone: make(chan struct{})}
			if _, ok := c.bytes.reserve(int(test.initial)); !ok {
				t.Fatal("cannot reserve initial byte credit")
			}
			previous := c.BeginConsumerPhase("callback")
			defer c.EndConsumerPhase(previous)
			go c.readLoop()
			input := []byte("{\"type\":\"ready\",\"protocolVersion\":1,\"supportedProtocolVersions\":[1,2],\"maxFrameBytes\":1048576,\"maxReassembledFrameBytes\":67108864}\n")
			for _, frame := range test.frames {
				input = append(input, frame...)
			}
			// A fatal frame leaves unread data, so diagnostics must mask the
			// packed prefetch flag while retaining the exact byte snapshot.
			input = append(input, []byte("{\"type\":\"agent_end\"}\n")...)
			if _, err := writer.Write(input); err != nil {
				t.Fatal(err)
			}
			_ = writer.Close()
			select {
			case <-c.readDone:
			case <-time.After(5 * time.Second):
				t.Fatal("overflow reader did not finish")
			}
			if c.bytes.usedBytes() != test.current || c.BufferedOutput() != (test.current != 0) {
				t.Fatal("terminal reader left stale prefetch occupancy or lost byte credit")
			}
			if err := c.failure(); ClassifyError(err) != "queue_overflow" || !c.protocolFailed.Load() || (test.reason == "byte_budget" && !errors.Is(err, errQueueOverflow)) {
				t.Fatalf("overflow classification changed: %v", err)
			}
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatalf("expected one INFO-visible error record: %v: %s", err, output.String())
			}
			for key, want := range map[string]string{"event": "rpc_queue_overflow", "level": "ERROR", "reason": test.reason, "stage": test.stage, "rpc_event": test.kind, "consumer_phase": "callback"} {
				if record[key] != want {
					t.Errorf("%s = %v, want %q", key, record[key], want)
				}
			}
			for key, want := range map[string]int64{"queued_events": int64(test.queued), "queue_capacity": 1, "buffered_bytes": test.current, "buffer_limit_bytes": maxRPCBufferedBytes, "rejected_bytes": int64(test.rejected), "rejected_used_bytes": test.rejectedUsed, "forward_wait_ms": 0} {
				if record[key] != float64(want) {
					t.Errorf("%s = %v, want %d", key, record[key], want)
				}
			}
			if elapsed, ok := record["consumer_phase_ms"].(float64); !ok || elapsed < 0 {
				t.Errorf("missing phase duration: %v", record["consumer_phase_ms"])
			}
			for _, canary := range []string{"SECRET_PROMPT", "SECRET_OUTPUT", "SECRET_TOKEN", "SECRET_HEADER", "https://api.telegram.org", "Authorization"} {
				if strings.Contains(output.String(), canary) {
					t.Fatalf("overflow leaked private frame content: %q", canary)
				}
			}
		})
	}
}

func TestForwarderBlockedDurationClearsAfterDeliveryAndLogsOverflow(t *testing.T) {
	var output bytes.Buffer
	c := &Client{log: slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo})), events: make(chan json.RawMessage), queuedEvents: make(chan json.RawMessage, 1), stop: make(chan struct{})}
	frame := json.RawMessage(`{"type":"message_update"}`)
	if _, ok := c.bytes.reserve(cap(frame)); !ok {
		t.Fatal("cannot reserve small event")
	}
	c.queuedEvents <- frame
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		c.forwardEvents()
	}()
	defer func() {
		c.stopOnce.Do(func() { close(c.stop) })
		<-finished
	}()
	deadline := time.Now().Add(time.Second)
	for c.forwardBlockedAt.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("forwarder did not start waiting")
		}
		time.Sleep(time.Millisecond)
	}
	started := c.forwardBlockedAt.Load()
	time.Sleep(5 * time.Millisecond)
	if elapsed := time.Since(rpcDiagnosticsEpoch).Nanoseconds() - (started - 1); elapsed < int64(time.Millisecond) {
		t.Fatalf("blocked duration did not advance: %d", elapsed)
	}
	select {
	case <-c.events:
	case <-time.After(time.Second):
		t.Fatal("forwarder did not deliver event")
	}
	for c.forwardBlockedAt.Load() != 0 || c.bytes.usedBytes() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("delivery did not clear wait state and release credit")
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok := c.bytes.reserve(cap(frame)); !ok {
		t.Fatal("cannot reserve second event")
	}
	c.queuedEvents <- frame
	deadline = time.Now().Add(time.Second)
	for c.forwardBlockedAt.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("forwarder did not start second wait")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(5 * time.Millisecond)
	details := queueOverflowDetails{reason: "event_slots", stage: "event_enqueue", eventKind: rpcSensitiveCanary, budget: byteBudgetFailure{used: c.bytes.usedBytes(), requested: cap(frame)}}
	c.queueOverflow(errQueueOverflow, details)
	c.queueOverflow(errQueueOverflow, details)
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("overflow should log exactly once: %v: %s", err, output.String())
	}
	if elapsed, ok := record["forward_wait_ms"].(float64); !ok || elapsed < 1 {
		t.Fatalf("blocked wait missing from error log: %v", record["forward_wait_ms"])
	}
	if record["rpc_event"] != "other" || strings.Contains(output.String(), "SECRET_") {
		t.Fatal("overflow event kind was not sanitized")
	}
}
