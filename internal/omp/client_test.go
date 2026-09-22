package omp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func testRPCLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

const rpcSensitiveCanary = "SECRET_PROMPT SECRET_OUTPUT SECRET_TOKEN https://api.telegram.org/botSECRET_TOKEN/ Authorization: SECRET_HEADER"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "config" {
		fixtureModelConfig()
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "acp" {
		fixtureACP()
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "--mode" {
		if os.Getenv("OMP_TEST_RPC_EOF_BEFORE_READY") == "1" {
			_ = os.Stdout.Close()
			_, _ = io.Copy(io.Discard, os.Stdin)
			os.Exit(0)
		}
		fmt.Println(`{"type":"ready","protocolVersion":1,"supportedProtocolVersions":[1,2],"maxFrameBytes":1048576,"maxReassembledFrameBytes":67108864}`)
		input := bufio.NewReader(os.Stdin)
		for {
			line, err := input.ReadBytes('\n')
			if err != nil {
				os.Exit(0)
			}
			var command map[string]any
			if json.Unmarshal(line, &command) != nil {
				os.Exit(1)
			}
			if fixtureSessionCommand(command) {
				continue
			}
			if fixtureModelCommand(command) {
				continue
			}
			switch command["type"] {
			case "rejected":
				fmt.Printf("{\"type\":\"response\",\"command\":\"rejected\",\"id\":%q,\"success\":false,\"error\":\"SECRET_REJECTION\"}\n", command["id"])
			case "unknown":
				fmt.Println(`{"type":"response","command":"unknown","success":false}`)
			case "malformed":
				fmt.Printf("{\"type\":\"response\",\"command\":\"malformed\",\"id\":%q,\"error\":%q}\n", command["id"], rpcSensitiveCanary)
			case "peer":
				fmt.Println(`{"type":"response","id":"peer-secret","command":"peer","success":true}`)
			case "overflow":
				for range 200 {
					fmt.Println(`{"type":"agent_start"}`)
				}
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
	os.Exit(m.Run())
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	client, err := Start(ctx, Config{Binary: binary}, testRPCLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
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
