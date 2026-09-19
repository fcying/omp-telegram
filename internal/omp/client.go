package omp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	maxFrame     = 1 << 20
	maxLogical   = 64 << 20
	chunkPayload = 256 << 10
)

var errProtocol = errors.New("omp: invalid RPC frame")

var nextClientID atomic.Uint64

type Config struct {
	Binary, CWD, Resume string
	Args                []string
}
type result struct {
	data json.RawMessage
	err  error
}
type request struct {
	command string
	result  chan result
}

// Client owns a process group. Raw events can contain sensitive content and must not be logged.
type Client struct {
	id                        uint64
	log                       *slog.Logger
	cmd                       *exec.Cmd
	stdin, stdout             *os.File
	events                    chan json.RawMessage
	ready                     chan struct{}
	readDone, done, stop      chan struct{}
	stopOnce                  sync.Once
	writeGate                 chan struct{}
	next                      atomic.Uint64
	frameLimit                atomic.Int64
	closeRequested            atomic.Bool
	transportEndedBeforeClose atomic.Bool
	failureStop               atomic.Bool
	contextCanceled           atomic.Bool
	protocolLogOnce           sync.Once
	queueOverflowLogOnce      sync.Once
	exitLogOnce               sync.Once
	mu                        sync.Mutex
	pending                   map[string]request
	err                       error
	metadataBusy              bool
	metadataUnavailable       bool
	metadata                  *metadataQuery
}

// ValidateArgs protects the transport and session lifecycle owned by the bridge.
func ValidateArgs(args []string) error {
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--", "--mode", "--cwd", "--resume", "--session", "--continue", "--print", "--no-session":
			return errors.New("omp.args cannot override RPC mode, working directory, or session lifecycle")
		}
		if strings.HasPrefix(arg, "-r") || strings.HasPrefix(arg, "-c") || strings.HasPrefix(arg, "-p") {
			return errors.New("omp.args cannot override RPC mode, working directory, or session lifecycle")
		}
		if strings.ContainsRune(arg, 0) {
			return errors.New("omp.args cannot contain NUL bytes")
		}
	}
	return nil
}

func Start(ctx context.Context, cfg Config, rpcLogger *slog.Logger) (*Client, error) {
	if rpcLogger == nil {
		return nil, errors.New("omp: RPC logger is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateArgs(cfg.Args); err != nil {
		return nil, err
	}
	if cfg.Binary == "" {
		cfg.Binary = "omp"
	}
	clientID := nextClientID.Add(1)
	args := make([]string, 0, 6+len(cfg.Args))
	args = append(args, "--mode", "rpc")
	if cfg.CWD != "" {
		args = append(args, "--cwd", cfg.CWD)
	}
	if cfg.Resume != "" {
		args = append(args, "--resume", cfg.Resume)
	}
	args = append(args, cfg.Args...)
	cmd := exec.Command(cfg.Binary, args...)
	cmd.Dir = cfg.CWD
	// A descriptor avoids os/exec copy goroutines waiting on inherited stderr.
	stderr, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, errors.New("omp: cannot discard process diagnostics")
	}
	defer stderr.Close()
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.New("omp: cannot create input pipe")
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		inRead.Close()
		inWrite.Close()
		return nil, errors.New("omp: cannot create output pipe")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inRead, outWrite, stderr
	waited, err := startProcess(cmd)
	if err != nil {
		inRead.Close()
		inWrite.Close()
		outRead.Close()
		outWrite.Close()
		return nil, errors.New("omp: cannot start process; check binary and working directory")
	}
	inRead.Close()
	outWrite.Close()
	c := &Client{id: clientID, log: rpcLogger.With("client_id", clientID), cmd: cmd, stdin: inWrite, stdout: outRead, events: make(chan json.RawMessage, 128), ready: make(chan struct{}), readDone: make(chan struct{}), done: make(chan struct{}), stop: make(chan struct{}), writeGate: make(chan struct{}, 1), pending: make(map[string]request)}
	c.frameLimit.Store(maxFrame)
	go c.readLoop()
	go c.supervise(ctx, waited)
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	select {
	case <-c.ready:
	case <-c.stop:
		c.Close()
		return nil, c.failure()
	case <-c.done:
		return nil, c.failure()
	case <-startup.Done():
		c.fail(errors.New("omp: ready timeout or startup cancellation"))
		c.Close()
		return nil, c.failure()
	}
	if _, err = c.Call(startup, "negotiate_protocol", map[string]any{"protocolVersion": 2}); err != nil {
		c.fail(errors.New("omp: protocol v2 negotiation failed"))
		c.Close()
		return nil, c.failure()
	}
	return c, nil
}

func (c *Client) Events() <-chan json.RawMessage { return c.events }
func (c *Client) Done() <-chan struct{}          { return c.done }
func (c *Client) ID() uint64                     { return c.id }

func (c *Client) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return &classifiedError{kind: "client_closed", err: errors.New("omp: process closed")}
}

func (c *Client) fail(err error) {
	c.failureStop.Store(true)
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.stopOnce.Do(func() { close(c.stop) })
}

func (c *Client) transportEnded(err error) {
	c.mu.Lock()
	if !c.closeRequested.Load() && !c.failureStop.Load() && !c.contextCanceled.Load() {
		c.transportEndedBeforeClose.Store(true)
	}
	if c.err == nil {
		c.err = &classifiedError{kind: "process_exit", err: err}
	}
	c.mu.Unlock()
	c.stopOnce.Do(func() { close(c.stop) })
}

// Close drains stdout while allowing a short graceful shutdown, then kills the process group.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closeRequested.Store(true)
	c.mu.Unlock()
	c.stopOnce.Do(func() { close(c.stop) })
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Client) logProcessExit(level slog.Level, reason string) {
	c.exitLogOnce.Do(func() {
		c.log.LogAttrs(context.Background(), level, "rpc process exit", slog.String("event", "rpc_process_exit"), slog.String("reason", reason))
	})
}

func (c *Client) processExited(ctx context.Context) {
	if ctx.Err() != nil {
		c.contextCanceled.Store(true)
	}
	switch {
	case c.contextCanceled.Load():
		c.logProcessExit(slog.LevelDebug, "context_canceled")
	case c.transportEndedBeforeClose.Load() && !c.failureStop.Load():
		c.logProcessExit(slog.LevelWarn, "unexpected")
	case c.closeRequested.Load() && !c.failureStop.Load():
		c.logProcessExit(slog.LevelDebug, "closed")
	case c.failureStop.Load():
		// The owning failure boundary already recorded the durable error.
		c.logProcessExit(slog.LevelDebug, "failure")
	default:
		c.logProcessExit(slog.LevelWarn, "unexpected")
	}
}

func (c *Client) supervise(ctx context.Context, waited <-chan error) {
	exited := false
	select {
	case err := <-waited:
		exited = true
		c.processExited(ctx)
		if err != nil {
			c.fail(&classifiedError{kind: "process_exit", err: errors.New("omp: process exited unsuccessfully")})
		} else {
			c.fail(&classifiedError{kind: "process_exit", err: errors.New("omp: process exited")})
		}
	case <-ctx.Done():
		c.contextCanceled.Store(true)
		c.fail(&classifiedError{kind: "cancelled", err: errors.New("omp: process context canceled")})
	case <-c.stop:
	}
	c.stdin.Close()
	if !exited {
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-waited:
			exited = true
			c.processExited(ctx)
		case <-timer.C:
		}
		timer.Stop()
	}
	if !exited {
		syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
		timer := time.NewTimer(time.Second)
		select {
		case <-waited:
			exited = true
			c.processExited(ctx)
		case <-timer.C:
		}
		timer.Stop()
	}
	// Also remove descendants left alive after the group leader exits.
	syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	if !exited {
		<-waited
		c.processExited(ctx)
	}
	timer := time.NewTimer(time.Second)
	select {
	case <-c.readDone:
	case <-timer.C:
		c.stdout.Close()
		<-c.readDone
	}
	timer.Stop()
	c.stdout.Close()
	close(c.done)
}

// Send writes a one-way frame, preserving the caller's correlation ID.
func (c *Client) Send(ctx context.Context, frame map[string]any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return errors.New("omp: cannot encode RPC request")
	}
	if len(data)+1 > int(c.frameLimit.Load()) {
		return errors.New("omp: RPC request exceeds frame limit")
	}
	select {
	case c.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stop:
		return c.failure()
	}
	defer func() { <-c.writeGate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.stop:
		return c.failure()
	default:
	}
	// Pipe deadlines interrupt a blocked write without leaking a writer goroutine.
	c.stdin.SetWriteDeadline(time.Time{})
	canceled := make(chan struct{})
	cancelWatch := context.AfterFunc(ctx, func() { c.stdin.SetWriteDeadline(time.Now()); close(canceled) })
	data = append(data, '\n')
	for len(data) > 0 {
		n, writeErr := c.stdin.Write(data)
		data = data[n:]
		if writeErr != nil {
			if !cancelWatch() {
				<-canceled
			}
			c.fail(errors.New("omp: RPC write failed; execution state uncertain"))
			return c.failure()
		}
	}
	if !cancelWatch() {
		<-canceled
	}
	return nil
}

// Call resolves the command acknowledgment, not the completion of an agent turn.
func (c *Client) Call(ctx context.Context, typeName string, fields map[string]any) (json.RawMessage, error) {
	frame := make(map[string]any, len(fields)+2)
	for k, v := range fields {
		frame[k] = v
	}
	frame["type"] = typeName
	id := strconv.FormatUint(c.next.Add(1), 10)
	frame["id"] = id
	r := request{command: typeName, result: make(chan result, 1)}
	c.mu.Lock()
	c.pending[id] = r
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.Send(ctx, frame); err != nil {
		return nil, err
	}
	select {
	case got := <-r.result:
		return got.data, got.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.stop:
		return nil, c.failure()
	case <-c.done:
		return nil, c.failure()
	}
}

type envelope struct {
	Type     string           `json:"type"`
	ID       string           `json:"id"`
	Command  string           `json:"command"`
	Success  *bool            `json:"success"`
	Data     json.RawMessage  `json:"data"`
	Terminal terminalMetadata `json:"isTerminal"`
}

// Invalid optional metadata must not change protocol acceptance.
type terminalMetadata uint8

func (t *terminalMetadata) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case "true":
		*t = 1
	case "false":
		*t = 2
	case "null":
		*t = 0
	default:
		*t = 3
	}
	return nil
}

func lifecycleEventAllowed(event string) bool {
	switch event {
	case "agent_start", "agent_end", "prompt_result", "auto_compaction_start", "auto_compaction_end", "response":
		return true
	default:
		return false
	}
}

func lifecyclePhaseAllowed(phase string) bool {
	switch phase {
	case "received", "event_queued", "response_queued", "response_ignored", "rejected":
		return true
	default:
		return false
	}
}

func (c *Client) logLifecycle(env envelope, phase string) {
	if !lifecycleEventAllowed(env.Type) || !lifecyclePhaseAllowed(phase) || !c.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	terminal := "absent"
	switch env.Terminal {
	case 1:
		terminal = "true"
	case 2:
		terminal = "false"
	case 3:
		terminal = "invalid"
	}
	attrs := []slog.Attr{
		slog.String("event", "rpc_lifecycle"),
		slog.String("rpc_event", env.Type),
		slog.String("phase", phase),
		slog.String("terminal", terminal),
	}
	// Request IDs are local decimal counters; never log arbitrary peer strings.
	requestID, err := strconv.ParseUint(env.ID, 10, 64)
	requestIDValid := env.ID != "" && err == nil
	attrs = append(attrs, slog.Bool("request_id_valid", requestIDValid))
	if requestIDValid {
		attrs = append(attrs, slog.Uint64("request_id", requestID))
	}
	c.log.LogAttrs(context.Background(), slog.LevelDebug, "rpc lifecycle", attrs...)
}

func (c *Client) protocolFailure(err error) {
	c.protocolLogOnce.Do(func() {
		c.log.LogAttrs(context.Background(), slog.LevelError, "rpc protocol error", slog.String("event", "rpc_protocol_error"))
	})
	c.fail(&classifiedError{kind: "protocol", err: err})
}

func (c *Client) queueOverflow(err error) {
	c.queueOverflowLogOnce.Do(func() {
		c.log.LogAttrs(context.Background(), slog.LevelError, "rpc queue overflow", slog.String("event", "rpc_queue_overflow"))
	})
	c.fail(&classifiedError{kind: "queue_overflow", err: err})
}

func decodeObject(data []byte) (envelope, error) {
	var frame envelope
	trimmed := bytes.TrimSpace(data)
	if !utf8.Valid(data) || len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(data, &frame) != nil || frame.Type == "" {
		return frame, errProtocol
	}
	return frame, nil
}

func (c *Client) readLoop() {
	defer close(c.readDone)
	defer close(c.events)
	// After a fatal decode/queue error, keep draining until the supervisor reaps the group.
	defer io.Copy(io.Discard, c.stdout)
	reader := bufio.NewReaderSize(c.stdout, 64<<10)
	decoder := frameDecoder{physical: maxFrame, logical: maxLogical}
	ready := false
	for {
		data, err := readLine(reader, decoder.physical)
		if err != nil {
			select {
			case <-c.stop:
				return
			default:
			}
			if errors.Is(err, errProtocol) {
				c.protocolFailure(errProtocol)
			} else {
				c.transportEnded(errors.New("omp: RPC output ended or exceeded its frame limit"))
			}
			return
		}
		frame, err := decoder.push(data)
		if err != nil {
			c.protocolFailure(errProtocol)
			return
		}
		if frame == nil {
			continue
		}
		env, err := decodeObject(frame)
		if err != nil {
			c.protocolFailure(errProtocol)
			return
		}
		if !ready {
			var announcement struct {
				ProtocolVersion int   `json:"protocolVersion"`
				Versions        []int `json:"supportedProtocolVersions"`
				Frame           int   `json:"maxFrameBytes"`
				Logical         int   `json:"maxReassembledFrameBytes"`
			}
			if env.Type != "ready" || json.Unmarshal(frame, &announcement) != nil || announcement.ProtocolVersion != 1 || announcement.Frame < 1024 || announcement.Frame > maxFrame || announcement.Logical < announcement.Frame || announcement.Logical > maxLogical {
				c.protocolFailure(errors.New("omp: invalid ready transport limits"))
				return
			}
			v2 := false
			for _, v := range announcement.Versions {
				if v == 2 {
					v2 = true
				}
			}
			if !v2 {
				c.protocolFailure(errors.New("omp: protocol v2 is required"))
				return
			}
			decoder.physical, decoder.logical = announcement.Frame, announcement.Logical
			c.frameLimit.Store(int64(announcement.Frame))
			ready = true
			close(c.ready)
			continue
		}
		if env.Type == "ready" {
			c.protocolFailure(errProtocol)
			return
		}
		c.logLifecycle(env, "received")
		if env.Type == "response" {
			if env.Success == nil || env.Command == "" {
				c.logLifecycle(env, "rejected")
				c.protocolFailure(errors.New("omp: malformed RPC response"))
				return
			}
			c.mu.Lock()
			r, found := c.pending[env.ID]
			if found {
				delete(c.pending, env.ID)
			}
			c.mu.Unlock()
			if found {
				if r.command != env.Command {
					c.logLifecycle(env, "rejected")
					c.protocolFailure(errors.New("omp: RPC response command mismatch"))
					return
				}
				response := result{data: env.Data}
				if !*env.Success {
					response.err = &classifiedError{kind: "rejected", err: errors.New("omp: RPC command rejected")}
				}
				r.result <- response
				c.logLifecycle(env, "response_queued")
				continue
			}
			if env.ID == "" {
				c.logLifecycle(env, "rejected")
				// The server omits IDs for unknown commands and parse errors. Fail closed
				// instead of stranding every pending call or guessing their correlation.
				c.protocolFailure(errors.New("omp: uncorrelated RPC response"))
				return
			}
			// A response with an ID is never an asynchronous event. If its caller
			// already left, it cannot safely affect a later bridge operation.
			c.logLifecycle(env, "response_ignored")
			continue
		}
		if env.Type == "command_output" && c.captureMetadata(frame) {
			continue
		}
		select {
		case c.events <- json.RawMessage(frame):
			// A buffered send confirms queueing, not consumption by the bridge.
			c.logLifecycle(env, "event_queued")
		default:
			c.logLifecycle(env, "rejected")
			c.queueOverflow(errors.New("omp: event queue overflow; execution state uncertain"))
			return
		}
	}
}

func readLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var data []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(data)+len(part) > limit {
			return nil, errProtocol
		}
		data = append(data, part...)
		if err == nil {
			return data[:len(data)-1], nil
		}
		if err != bufio.ErrBufferFull {
			return nil, err
		}
	}
}

type frameDecoder struct {
	physical, logical   int
	id                  string
	count, length, next int
	data                []byte
}

func (d *frameDecoder) push(data []byte) ([]byte, error) {
	env, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	if env.Type != "rpc_chunk" {
		if d.id != "" {
			return nil, errProtocol
		}
		return data, nil
	}
	var chunk struct {
		ID     string `json:"chunkId"`
		Index  *int   `json:"index"`
		Count  int    `json:"count"`
		Length int    `json:"byteLength"`
		Data   string `json:"data"`
	}
	if json.Unmarshal(data, &chunk) != nil || chunk.ID == "" || len(chunk.ID) > 128 || chunk.Index == nil || *chunk.Index < 0 || chunk.Count < 2 || chunk.Count > (d.logical+chunkPayload-1)/chunkPayload || *chunk.Index >= chunk.Count || chunk.Length < d.physical || chunk.Length > d.logical || chunk.Data == "" {
		return nil, errProtocol
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(chunk.Data)
	if err != nil || len(decoded) == 0 || len(decoded) > chunkPayload || base64.StdEncoding.EncodeToString(decoded) != chunk.Data {
		return nil, errProtocol
	}
	if d.id == "" {
		if *chunk.Index != 0 {
			return nil, errProtocol
		}
		d.id, d.count, d.length, d.next = chunk.ID, chunk.Count, chunk.Length, 0
		d.data = make([]byte, 0, chunk.Length)
	}
	if d.id != chunk.ID || d.count != chunk.Count || d.length != chunk.Length || d.next != *chunk.Index || len(d.data)+len(decoded) > d.length {
		return nil, errProtocol
	}
	d.data = append(d.data, decoded...)
	d.next++
	if d.next < d.count {
		return nil, nil
	}
	if len(d.data) != d.length {
		return nil, errProtocol
	}
	result := d.data
	d.id = ""
	d.data = nil
	logical, err := decodeObject(result)
	if err != nil || logical.Type == "rpc_chunk" {
		return nil, errProtocol
	}
	return result, nil
}
