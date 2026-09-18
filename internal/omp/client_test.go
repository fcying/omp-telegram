package omp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

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
			case "unknown":
				fmt.Println(`{"type":"response","command":"unknown","success":false}`)
			case "malformed":
				fmt.Printf("{\"type\":\"response\",\"command\":\"malformed\",\"id\":%q}\n", command["id"])
			case "overflow":
				for range 200 {
					fmt.Println(`{"type":"agent_start"}`)
				}
			default:
				reply := map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": true, "data": map[string]any{"accepted": true}}
				encoded, _ := json.Marshal(reply)
				fmt.Println(string(encoded))
				if command["type"] == "prompt" {
					reply["success"] = false
					reply["error"] = "sensitive server diagnostics"
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

func fixtureClient(t *testing.T) *Client {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	client, err := Start(ctx, Config{Binary: binary})
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
		if err := client.failure(); !strings.Contains(err.Error(), "overflow") {
			t.Fatalf("wrong failure: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("overflow left process running")
	}
}
