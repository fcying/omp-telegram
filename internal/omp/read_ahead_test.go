package omp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type readAheadPauseWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *readAheadPauseWriter) Write(data []byte) (int, error) {
	text := string(data)
	if strings.Contains(text, `"rpc_event":"agent_start"`) && strings.Contains(text, `"phase":"event_queued"`) {
		w.once.Do(func() { close(w.entered) })
		<-w.release
	}
	return len(data), nil
}

func TestBufferedOutputIncludesUnframedReadAhead(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pause := &readAheadPauseWriter{entered: make(chan struct{}), release: make(chan struct{})}
	logger := slog.New(slog.NewJSONHandler(pause, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client, err := Start(ctx, Config{Binary: exe, CWD: t.TempDir(), NoSession: true}, logger)
	if err != nil {
		t.Fatal(err)
	}
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(pause.release) })
		client.Close()
	})
	if err := client.Send(ctx, map[string]any{"type": "prefetched_events"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pause.entered:
	case <-ctx.Done():
		t.Fatal("reader did not pause after publishing the first event")
	}
	select {
	case raw := <-client.Events():
		var event envelope
		if json.Unmarshal(raw, &event) != nil || event.Type != "agent_start" {
			t.Fatal("first native event was not preserved")
		}
	case <-ctx.Done():
		t.Fatal("first event was not delivered")
	}
	// Release credit from the consumed event, leaving only unread bytes in bufio.
	for client.bytes.usedBytes() != 0 {
		if ctx.Err() != nil {
			t.Fatal("consumed event retained byte credit")
		}
		runtime.Gosched()
	}
	if !client.BufferedOutput() {
		t.Fatal("unframed next native dialog was invisible after the preceding event was consumed")
	}
	release.Do(func() { close(pause.release) })
	select {
	case raw := <-client.Events():
		var event envelope
		if json.Unmarshal(raw, &event) != nil || event.Type != "extension_ui_request" {
			t.Fatal("prefetched native dialog was lost")
		}
	case <-ctx.Done():
		t.Fatal("prefetched dialog did not reach the consumer")
	}
	for client.BufferedOutput() {
		if ctx.Err() != nil {
			t.Fatal("fully consumed native output remained buffered")
		}
		runtime.Gosched()
	}
}

func startPartialPhysicalLine(t *testing.T) (*Client, context.Context) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	client, err := Start(ctx, Config{Binary: exe, CWD: t.TempDir(), NoSession: true}, testRPCLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	if client.BufferedOutput() || client.bytes.usedBytes() != 0 {
		t.Fatal("idle reader reported buffered output before the fragment")
	}
	if err := client.Send(ctx, map[string]any{"type": "partial_physical_line"}); err != nil {
		t.Fatal(err)
	}
	// The fixture withholds the newline and the rest of this sub-64KiB line
	// until another command, so ReadSlice cannot return frame credit yet.
	for !client.BufferedOutput() {
		if ctx.Err() != nil {
			t.Fatal("incomplete physical line remained invisible to the consumer")
		}
		runtime.Gosched()
	}
	if used := client.bytes.usedBytes(); used != 0 {
		t.Fatalf("incomplete physical line charged %d frame bytes before ReadSlice returned", used)
	}
	select {
	case raw := <-client.Events():
		t.Fatalf("incomplete physical line produced a public event: %s", raw)
	default:
	}
	return client, ctx
}

func TestBufferedOutputIncludesPartialPhysicalLine(t *testing.T) {
	client, ctx := startPartialPhysicalLine(t)
	if err := client.Send(ctx, map[string]any{"type": "release_partial_physical_line"}); err != nil {
		t.Fatal(err)
	}
	select {
	case raw, ok := <-client.Events():
		const want = `{"type":"extension_ui_request","id":"fragment-dialog","method":"ask","questions":[]}`
		if !ok || string(raw) != want {
			t.Fatalf("fragmented Ask event was lost or corrupted: %s", raw)
		}
	case <-ctx.Done():
		t.Fatal("completed physical line did not reach the consumer")
	}
	for client.BufferedOutput() {
		if ctx.Err() != nil {
			t.Fatal("consumed fragmented event left buffered occupancy")
		}
		runtime.Gosched()
	}
	if used := client.bytes.usedBytes(); used != 0 {
		t.Fatalf("consumed fragmented event retained %d frame bytes", used)
	}
}

func TestBufferedOutputPartialPhysicalLineEOF(t *testing.T) {
	client, ctx := startPartialPhysicalLine(t)
	if err := client.Send(ctx, map[string]any{"type": "partial_physical_line_eof"}); err != nil {
		t.Fatal(err)
	}
	select {
	case raw, ok := <-client.Events():
		if ok {
			t.Fatalf("unterminated physical line produced a public event: %s", raw)
		}
	case <-ctx.Done():
		t.Fatal("fragment EOF did not close the public event stream")
	}
	select {
	case <-client.done:
	case <-ctx.Done():
		t.Fatal("fragment EOF did not finish client shutdown")
	}
	if err := client.failure(); ClassifyError(err) != "protocol" {
		t.Fatalf("fragment EOF was not a protocol failure: %v", err)
	}
	if client.BufferedOutput() || client.bytes.usedBytes() != 0 {
		t.Fatal("fragment EOF left buffered occupancy or frame credit")
	}
}

type occupancyPauseWriter struct {
	ctx     context.Context
	entered chan string
	resume  chan struct{}
}

func (w *occupancyPauseWriter) Write(data []byte) (int, error) {
	text := string(data)
	phase := ""
	if strings.Contains(text, `"rpc_event":"agent_start"`) && strings.Contains(text, `"phase":"event_queued"`) {
		phase = "event"
	} else if strings.Contains(text, `"phase":"response_ignored"`) {
		phase = "prefetch"
	}
	if phase != "" {
		select {
		case w.entered <- phase:
		case <-w.ctx.Done():
			return len(data), nil
		}
		select {
		case <-w.resume:
		case <-w.ctx.Done():
		}
	}
	return len(data), nil
}

func TestBufferedOutputConcurrentOccupancyHandoffs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pause := &occupancyPauseWriter{ctx: ctx, entered: make(chan string), resume: make(chan struct{})}
	client := &Client{
		log:    slog.New(slog.NewJSONHandler(pause, &slog.HandlerOptions{Level: slog.LevelDebug})),
		stdout: reader, events: make(chan json.RawMessage), queuedEvents: make(chan json.RawMessage, 1),
		ready: make(chan struct{}), stop: make(chan struct{}), readDone: make(chan struct{}),
	}
	forwardDone := make(chan struct{})
	go client.readLoop()
	go func() {
		defer close(forwardDone)
		client.forwardEvents()
	}()
	t.Cleanup(func() {
		cancel()
		client.stopOnce.Do(func() { close(client.stop) })
		_ = writer.Close()
		_ = reader.Close()
		<-client.readDone
		<-forwardDone
	})
	waitPhase := func(want string) {
		t.Helper()
		select {
		case phase := <-pause.entered:
			if phase != want {
				t.Fatalf("reader paused at %q, want %q", phase, want)
			}
		case <-ctx.Done():
			t.Fatalf("reader did not reach %s handoff", want)
		}
	}
	resume := func() {
		t.Helper()
		select {
		case pause.resume <- struct{}{}:
		case <-ctx.Done():
			t.Fatal("reader did not resume")
		}
	}
	consume := func(want int) {
		t.Helper()
		select {
		case raw := <-client.Events():
			var event struct {
				Type string `json:"type"`
				Seq  int    `json:"seq"`
			}
			if json.Unmarshal(raw, &event) != nil || event.Type != "agent_start" || event.Seq != want {
				t.Fatalf("consumer received reordered or corrupted event %s, want %d", raw, want)
			}
		case <-ctx.Done():
			t.Fatal("event did not reach consumer")
		}
		for client.bytes.usedBytes() != 0 {
			if ctx.Err() != nil {
				t.Fatal("consumed event retained credit")
			}
			runtime.Gosched()
		}
	}
	if _, err := fmt.Fprint(writer, "{\"type\":\"ready\",\"protocolVersion\":1,\"supportedProtocolVersions\":[1,2],\"maxFrameBytes\":1048576,\"maxReassembledFrameBytes\":67108864}\n{\"type\":\"agent_start\",\"seq\":0}\n"); err != nil {
		t.Fatal(err)
	}
	waitPhase("event")
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	monitorDone := make(chan bool, 1)
	go func() {
		for monitorCtx.Err() == nil {
			if !client.BufferedOutput() {
				monitorDone <- false
				return
			}
			runtime.Gosched()
		}
		monitorDone <- true
	}()
	defer stopMonitor()
	const handoffs = 1000
	for i := 1; i <= handoffs; i++ {
		// The old event owns credit, with no prefetch. A single pipe write puts
		// the next event behind a response, without involving the public consumer.
		if _, err := fmt.Fprintf(writer, "{\"type\":\"response\",\"id\":\"ignored\",\"command\":\"barrier\",\"success\":true}\n{\"type\":\"agent_start\",\"seq\":%d}\n", i); err != nil {
			t.Fatal(err)
		}
		resume()
		waitPhase("prefetch")
		consume(i - 1)
		if !client.BufferedOutput() {
			t.Fatal("frame-to-prefetch handoff lost unread output")
		}
		resume()
		waitPhase("event")
		if !client.BufferedOutput() || client.bytes.usedBytes() == 0 {
			t.Fatal("prefetch-to-frame handoff lost the next event")
		}
	}
	stopMonitor()
	if !<-monitorDone {
		t.Fatal("concurrent consumer observed empty output during an occupied handoff")
	}
	consume(handoffs)
	if client.BufferedOutput() {
		t.Fatal("last delivered event left permanent buffered occupancy")
	}
	resume()
	_ = writer.Close()
	select {
	case <-client.readDone:
	case <-ctx.Done():
		t.Fatal("reader did not finish after EOF")
	}
	if client.BufferedOutput() {
		t.Fatal("EOF left a stale prefetch flag")
	}
}
