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
	maxFrame            = 1 << 20
	maxLogical          = 64 << 20
	maxRPCBufferedBytes = 64 << 20
	chunkPayload        = 256 << 10
)

var errQueueOverflow = errors.New("omp: RPC byte budget exceeded; execution state uncertain")
var errAskDialogUnsupported = errors.New("omp: native Ask dialog unsupported")

// byteBudget shares one occupancy snapshot between the reader and event forwarder.
// Low bits hold frame/event credit until delivery; the high flag tracks bytes
// read into bufio, including incomplete physical lines, without charging credit.
const readAheadFlag int64 = 1 << 62

type byteBudget struct{ state atomic.Int64 }

func (b *byteBudget) usedBytes() int64 {
	return b.state.Load() &^ readAheadFlag
}

func (b *byteBudget) setReadAhead(buffered bool) {
	if buffered {
		b.state.Or(readAheadFlag)
	} else {
		b.state.And(^readAheadFlag)
	}
}

type readAheadReader struct {
	stdout *os.File
	budget *byteBudget
}

func (r *readAheadReader) Read(data []byte) (int, error) {
	n, err := r.stdout.Read(data)
	if n > 0 {
		// ReadSlice can keep filling an incomplete line before returning any credit.
		// Publish only after data arrives, so an idle blocking Read stays empty.
		r.budget.setReadAhead(true)
	}
	return n, err
}

// reserve returns the exact occupancy observed by a failed reservation. The
// forwarder may release credits immediately afterwards, so callers must retain it.
func (b *byteBudget) reserve(n int) (int64, bool) {
	if b == nil || n == 0 {
		return 0, true
	}
	for {
		state := b.state.Load()
		used := state &^ readAheadFlag
		if int64(n) > maxRPCBufferedBytes-used {
			return used, false
		}
		if b.state.CompareAndSwap(state, state+int64(n)) {
			return used, true
		}
	}
}

func (b *byteBudget) release(n int) {
	if b != nil && n != 0 {
		// n never exceeds the byte credit, so subtraction preserves the flag.
		b.state.Add(-int64(n))
	}
}

type byteBudgetFailure struct {
	used      int64
	requested int
}

type queueOverflowDetails struct {
	reason, stage, eventKind string
	budget                   byteBudgetFailure
	queued                   int
}

// ConsumerPhaseState is an opaque, by-value token for restoring a nested
// consumer phase, including its original start time, on the same Client.
type ConsumerPhaseState struct {
	phase   string
	started time.Time
}

var rpcDiagnosticsEpoch = time.Now()

var errProtocol = errors.New("omp: invalid RPC frame")

// ErrRequestTooLarge means the request was rejected before any bytes were written.
var ErrRequestTooLarge = errors.New("omp: RPC request exceeds frame limit")

var nextClientID atomic.Uint64

type Config struct {
	Binary, CWD, Resume string
	Args                []string
	Environment         Environment
	NoSession           bool
	// ResumeModelFallback retries only native saved-model restoration failures.
	ResumeModelFallback bool
}
type result struct {
	data json.RawMessage
	err  error
}
type request struct {
	command string
	result  chan result
	catalog *commandCatalogQuery
}

// Client owns a process group. Raw events can contain sensitive content and must not be logged.
type Client struct {
	id                        uint64
	log                       *slog.Logger
	cmd                       *exec.Cmd
	stdin, stdout             *os.File
	resumeDiagnostics         *resumeModelDiagnostics
	usedDefaultModelFallback  bool
	protocolFailed            atomic.Bool
	startupUI                 atomic.Pointer[startupUITracker]
	events                    chan json.RawMessage
	queuedEvents              chan json.RawMessage
	bytes                     byteBudget
	consumerMu                sync.Mutex
	consumerPhase             ConsumerPhaseState
	forwardBlockedAt          atomic.Int64 // Monotonic nanoseconds since rpcDiagnosticsEpoch, plus one.
	ready                     chan struct{}
	readDone, done, stop      chan struct{}
	stopOnce                  sync.Once
	shutdownMu                sync.Mutex // Serializes destructive arming, killing, and stdin closure.
	destructiveShutdown       bool
	writeGate                 chan struct{}
	next                      atomic.Uint64
	frameLimit                atomic.Int64
	closeRequested            atomic.Bool
	hardStopRequested         atomic.Bool
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
	usageFence                *usageQuery
	commandCatalog            CommandCatalog
	catalogUpdateRevision     uint64
	catalogQuery              *commandCatalogQuery
}

// BeginConsumerPhase records a bounded diagnostic phase without changing event
// consumption. EndConsumerPhase must receive the token on this same client.
func (c *Client) BeginConsumerPhase(phase string) ConsumerPhaseState {
	switch phase {
	case "idle", "input", "callback", "rpc_event", "operation", "dispatch", "tick", "result", "runtime", "store", "telegram":
	default:
		phase = "other"
	}
	c.consumerMu.Lock()
	previous := c.consumerPhase
	c.consumerPhase = ConsumerPhaseState{phase: phase, started: time.Now()}
	c.consumerMu.Unlock()
	return previous
}

// EndConsumerPhase restores an earlier phase without restarting its elapsed time.
func (c *Client) EndConsumerPhase(previous ConsumerPhaseState) {
	c.consumerMu.Lock()
	c.consumerPhase = previous
	c.consumerMu.Unlock()
}

func (c *Client) consumerPhaseSnapshot() (string, int64) {
	c.consumerMu.Lock()
	state := c.consumerPhase
	c.consumerMu.Unlock()
	if state.phase == "" {
		return "idle", 0
	}
	return state.phase, time.Since(state.started).Milliseconds()
}

// ValidateArgs protects the transport and session lifecycle owned by the bridge.
func ValidateArgs(args []string) error {
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--", "--mode", "--cwd", "--resume", "--session", "--continue", "--print", "--no-session", "--export":
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
	c, err := startOnce(ctx, cfg, rpcLogger)
	if err == nil || !cfg.ResumeModelFallback || cfg.Resume == "" || ctx.Err() != nil || ClassifyError(err) != "model_restore" {
		return c, err
	}
	cfg.Args = defaultModelArgs(cfg.Args)
	cfg.ResumeModelFallback = false
	c, err = startOnce(ctx, cfg, rpcLogger)
	if err == nil {
		c.usedDefaultModelFallback = true
		c.log.Info("rpc saved model fallback", "event", "rpc_model_restore_fallback")
	}
	return c, err
}

func startOnce(ctx context.Context, cfg Config, rpcLogger *slog.Logger) (*Client, error) {
	if rpcLogger == nil {
		return nil, errors.New("omp: RPC logger is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateArgs(cfg.Args); err != nil {
		return nil, err
	}
	if cfg.Resume != "" && cfg.NoSession {
		return nil, errors.New("omp: resume and no-session lifecycle selections conflict")
	}
	if cfg.Binary == "" {
		cfg.Binary = "omp"
	}
	clientID := nextClientID.Add(1)
	args := make([]string, 0, 6+len(cfg.Args))
	args = append(args, "--mode", "rpc-ui")
	if cfg.CWD != "" {
		args = append(args, "--cwd", cfg.CWD)
	}
	if cfg.Resume != "" {
		args = append(args, "--resume", cfg.Resume)
	}
	args = append(args, cfg.Args...)
	if cfg.NoSession {
		args = append(args, "--no-session")
	}
	cmd := exec.Command(cfg.Binary, args...)
	cmd.Env = ChildEnv(cfg.Environment)
	cmd.Dir = cfg.CWD
	// Explicit descriptors keep os/exec Wait independent of inherited stderr.
	stderr, diagnostics, err := openResumeModelDiagnostics(cfg.ResumeModelFallback && cfg.Resume != "")
	if err != nil {
		return nil, errors.New("omp: cannot open process diagnostics")
	}
	defer func() {
		stderr.Close()
		if diagnostics != nil {
			diagnostics.finish()
		}
	}()
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
	stderr.Close()
	c := &Client{id: clientID, log: rpcLogger.With("client_id", clientID), cmd: cmd, stdin: inWrite, stdout: outRead, events: make(chan json.RawMessage), queuedEvents: make(chan json.RawMessage, 128), ready: make(chan struct{}), readDone: make(chan struct{}), done: make(chan struct{}), stop: make(chan struct{}), writeGate: make(chan struct{}, 1), pending: make(map[string]request)}
	c.consumerPhase = ConsumerPhaseState{phase: "idle", started: time.Now()}
	c.resumeDiagnostics = diagnostics
	diagnostics = nil // The supervisor now owns the collector.
	c.frameLimit.Store(maxFrame)
	c.startupUI.Store(&startupUITracker{})
	go c.readLoop()
	go c.forwardEvents()
	go c.supervise(ctx, waited)
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	select {
	case <-c.ready:
	case <-c.stop:
		return nil, c.finishStartupFailure(ctx, startup, "ready", c.failure())
	case <-c.done:
		return nil, c.finishStartupFailure(ctx, startup, "ready", c.failure())
	case <-startup.Done():
		return nil, c.finishStartupFailure(ctx, startup, "ready", startup.Err())
	}
	// Opt in before protocol negotiation: OMP accepts this side channel while
	// session_start is running, whereas ordinary commands wait for startup.
	data, askErr := c.Call(startup, "set_ask_dialog", map[string]any{"enabled": true})
	if askErr == nil {
		var dialog struct {
			Enabled *bool `json:"enabled"`
		}
		if json.Unmarshal(data, &dialog) != nil || dialog.Enabled == nil || !*dialog.Enabled {
			askErr = errProtocol
		}
	}
	if askErr != nil && !errors.Is(askErr, errAskDialogUnsupported) {
		return nil, c.finishStartupFailure(ctx, startup, "set_ask_dialog", askErr)
	}
	if _, err = c.Call(startup, "negotiate_protocol", map[string]any{"protocolVersion": 2}); err != nil {
		return nil, c.finishStartupFailure(ctx, startup, "negotiate_protocol", err)
	}
	c.discardStartupUI()
	return c, nil
}

// startupFailure runs only before negotiation, after fully reaping the process group.
func (c *Client) startupFailure(ctx context.Context) error {
	_ = c.Close()
	err := c.failure()
	select {
	case <-c.ready:
		return err
	default:
	}
	if ctx.Err() == nil && !c.protocolFailed.Load() && ClassifyError(err) == "process_exit" && c.resumeDiagnostics != nil && c.resumeDiagnostics.matched {
		return &classifiedError{kind: "model_restore", err: errors.New("omp: saved session model could not be restored")}
	}
	return err
}

func (c *Client) Events() <-chan json.RawMessage { return c.events }
func (c *Client) Done() <-chan struct{}          { return c.done }
func (c *Client) ID() uint64                     { return c.id }

// UsedDefaultModelFallback reports a successful saved-model startup recovery.
func (c *Client) UsedDefaultModelFallback() bool { return c.usedDefaultModelFallback }

// Stopping reports a terminal transport signal without waiting for process reaping.
func (c *Client) Stopping() bool {
	select {
	case <-c.stop:
		return true
	default:
		return false
	}
}

// BufferedOutput atomically snapshots unread prefetched bytes, partial frames
// and queued events, including transfers between those forms of occupancy.
func (c *Client) BufferedOutput() bool {
	return c.bytes.state.Load() != 0
}

// FrameLimit includes the newline delimiting a request frame.
func (c *Client) FrameLimit() int { return int(c.frameLimit.Load()) }

func (c *Client) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return &classifiedError{kind: "client_closed", err: errors.New("omp: process closed")}
}

func (c *Client) fail(err error) {
	if tracker := c.startupUI.Load(); tracker != nil {
		tracker.finish()
	}
	c.failureStop.Store(true)
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.stopOnce.Do(func() { close(c.stop) })
}

func (c *Client) transportEnded(err error) {
	if tracker := c.startupUI.Load(); tracker != nil {
		tracker.finish()
	}
	c.mu.Lock()
	if !c.closeRequested.Load() && !c.failureStop.Load() && !c.contextCanceled.Load() {
		c.transportEndedBeforeClose.Store(true)
	}
	if c.err == nil && !c.hardStopRequested.Load() {
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

// armDestructiveShutdown prevents any subsequent supervisor exit from closing
// stdin before killing the group. It is only used for mutating native commands.
func (c *Client) armDestructiveShutdown() error {
	c.shutdownMu.Lock()
	defer c.shutdownMu.Unlock()
	select {
	case <-c.stop:
		return c.failure()
	default:
	}
	c.destructiveShutdown = true
	return nil
}

// TerminateNow kills the process group before allowing stdin to close or drain.
func (c *Client) TerminateNow() error {
	c.shutdownMu.Lock()
	select {
	case <-c.done:
		c.shutdownMu.Unlock()
		return nil
	default:
	}
	c.mu.Lock()
	c.closeRequested.Store(true)
	c.hardStopRequested.Store(true)
	c.mu.Unlock()
	killErr := syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	c.shutdownMu.Unlock()
	_ = c.Close()
	if killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
		return killErr
	}
	return nil
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
		if !c.hardStopRequested.Load() {
			if err != nil {
				c.fail(&classifiedError{kind: "process_exit", err: errors.New("omp: process exited unsuccessfully")})
			} else {
				c.fail(&classifiedError{kind: "process_exit", err: errors.New("omp: process exited")})
			}
		}
	case <-ctx.Done():
		c.contextCanceled.Store(true)
		if c.startupUI.Load() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			c.fail(context.DeadlineExceeded)
		} else {
			c.fail(&classifiedError{kind: "cancelled", err: errors.New("omp: process context canceled")})
		}
	case <-c.stop:
	}
	c.shutdownMu.Lock()
	if c.destructiveShutdown {
		// OMP can recreate deleted history on graceful EOF, even after RPC failure.
		c.hardStopRequested.Store(true)
		syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	}
	c.stdin.Close()
	c.shutdownMu.Unlock()
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
	if c.resumeDiagnostics != nil {
		c.resumeDiagnostics.finish()
	}
	close(c.done)
}

// Send writes a one-way frame, preserving the caller's correlation ID.
func (c *Client) Send(ctx context.Context, frame map[string]any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return errors.New("omp: cannot encode RPC request")
	}
	if len(data)+1 > c.FrameLimit() {
		return ErrRequestTooLarge
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
	if frame["type"] == "prompt" {
		err := c.prepareUsagePrompt(ctx, frame["id"])
		if err != nil {
			return err
		}
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

// ReserveRequestID returns a correlation ID without sending a request.
func (c *Client) ReserveRequestID() string {
	return strconv.FormatUint(c.next.Add(1), 10)
}

// Call resolves the command acknowledgment, not the completion of an agent turn.
func (c *Client) Call(ctx context.Context, typeName string, fields map[string]any) (json.RawMessage, error) {
	return c.CallWithID(ctx, c.ReserveRequestID(), typeName, fields)
}

// CallWithID registers its correlation ID before writing the RPC request.
func (c *Client) CallWithID(ctx context.Context, id, typeName string, fields map[string]any) (json.RawMessage, error) {
	if id == "" {
		return nil, errors.New("omp: empty RPC request ID")
	}
	r := request{command: typeName, result: make(chan result, 1)}
	c.mu.Lock()
	if _, found := c.pending[id]; found {
		c.mu.Unlock()
		return nil, errors.New("omp: duplicate pending RPC request ID")
	}
	c.pending[id] = r
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if current, found := c.pending[id]; found && current.result == r.result {
			delete(c.pending, id)
		}
		c.mu.Unlock()
	}()
	frame := make(map[string]any, len(fields)+2)
	for k, v := range fields {
		frame[k] = v
	}
	frame["type"] = typeName
	frame["id"] = id
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
	Error    json.RawMessage  `json:"error"`
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
	c.protocolFailed.Store(true)
	c.protocolLogOnce.Do(func() {
		c.log.LogAttrs(context.Background(), slog.LevelError, "rpc protocol error", slog.String("event", "rpc_protocol_error"))
	})
	c.fail(&classifiedError{kind: "protocol", err: err})
}

func (c *Client) queueOverflow(err error, details queueOverflowDetails) {
	c.protocolFailed.Store(true)
	c.queueOverflowLogOnce.Do(func() {
		phase, phaseMS := c.consumerPhaseSnapshot()
		var forwardWaitMS int64
		if started := c.forwardBlockedAt.Load(); started != 0 {
			forwardWaitMS = max(0, (time.Since(rpcDiagnosticsEpoch).Nanoseconds()-(started-1))/int64(time.Millisecond))
		}
		c.log.LogAttrs(context.Background(), slog.LevelError, "rpc queue overflow",
			slog.String("event", "rpc_queue_overflow"),
			slog.String("reason", details.reason),
			slog.String("stage", details.stage),
			slog.String("rpc_event", overflowEventKind(details.eventKind)),
			slog.Int("queued_events", details.queued),
			slog.Int("queue_capacity", cap(c.queuedEvents)),
			slog.Int64("buffered_bytes", c.bytes.usedBytes()),
			slog.Int("buffer_limit_bytes", maxRPCBufferedBytes),
			slog.Int("rejected_bytes", details.budget.requested),
			slog.Int64("rejected_used_bytes", details.budget.used),
			slog.Int64("forward_wait_ms", forwardWaitMS),
			slog.String("consumer_phase", phase),
			slog.Int64("consumer_phase_ms", phaseMS))
	})
	c.fail(&classifiedError{kind: "queue_overflow", err: err})
}

func overflowEventKind(kind string) string {
	switch kind {
	case "ready", "rpc_chunk", "response", "agent_start", "agent_end", "prompt_result",
		"message_start", "message_update", "message_end", "turn_start", "turn_end",
		"tool_execution_start", "tool_execution_update", "tool_execution_end", "host_tool_call",
		"auto_compaction_start", "auto_compaction_end", "auto_retry_start", "auto_retry_end",
		"session_info_update", "available_commands_update", "command_output":
		return kind
	default:
		return "other"
	}
}

func decodeObject(data []byte) (envelope, error) {
	var frame envelope
	trimmed := bytes.TrimSpace(data)
	if !utf8.Valid(data) || len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(data, &frame) != nil || frame.Type == "" {
		return frame, errProtocol
	}
	return frame, nil
}

// The reader never waits for consumers: the forwarding goroutine can block on
// Events while the reader continues until a slot or byte credit is exhausted.
func (c *Client) forwardEvents() {
	defer close(c.events)
	for {
		select {
		case <-c.stop:
			return
		case frame, ok := <-c.queuedEvents:
			if !ok {
				return
			}
			select {
			case c.events <- frame:
				c.bytes.release(cap(frame))
				continue
			case <-c.stop:
				return
			default:
			}
			c.forwardBlockedAt.Store(time.Since(rpcDiagnosticsEpoch).Nanoseconds() + 1)
			select {
			case c.events <- frame:
				c.forwardBlockedAt.Store(0)
				c.bytes.release(cap(frame))
			case <-c.stop:
				c.forwardBlockedAt.Store(0)
				return
			}
		}
	}
}

func (c *Client) readLoop() {
	defer close(c.readDone)
	defer close(c.queuedEvents)
	// After a fatal decode/queue error, keep draining until the supervisor reaps the group.
	defer io.Copy(io.Discard, c.stdout)
	reader := bufio.NewReaderSize(&readAheadReader{stdout: c.stdout, budget: &c.bytes}, 64<<10)
	defer c.bytes.setReadAhead(false)
	decoder := frameDecoder{physical: maxFrame, logical: maxLogical, budget: &c.bytes}
	ready := false
	var readFailure byteBudgetFailure
	for {
		data, err := readLineBudget(reader, decoder.physical, &c.bytes, &readFailure)
		// Keep the read/prefetch flag until readLineBudget has reserved frame credit;
		// publish new prefetch before any response/event credit can be released.
		// Both handoffs overlap in one atomic state, so occupancy cannot disappear.
		c.bytes.setReadAhead(reader.Buffered() != 0)
		if err != nil {
			c.bytes.release(cap(decoder.data))
			if decoder.id != "" && !errors.Is(err, errQueueOverflow) {
				err = errProtocol
			}
			// Preserve protocol exclusion even when process exit won the stop race.
			if errors.Is(err, errProtocol) || errors.Is(err, errQueueOverflow) {
				c.protocolFailed.Store(true)
			}
			select {
			case <-c.stop:
				return
			default:
			}
			if errors.Is(err, errQueueOverflow) {
				c.queueOverflow(err, queueOverflowDetails{reason: "byte_budget", stage: "physical_read", budget: readFailure, queued: len(c.queuedEvents)})
				return
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
			c.bytes.release(cap(data))
			c.bytes.release(cap(decoder.data))
			if errors.Is(err, errQueueOverflow) {
				c.queueOverflow(err, queueOverflowDetails{reason: "byte_budget", stage: "chunk_reassembly", eventKind: "rpc_chunk", budget: decoder.budgetFailure, queued: len(c.queuedEvents)})
			} else {
				c.protocolFailure(errProtocol)
			}
			return
		}
		if decoder.reassembled || frame == nil {
			c.bytes.release(cap(data))
		}
		if frame == nil {
			continue
		}
		env, err := decodeObject(frame)
		if err != nil {
			c.protocolFailure(errProtocol)
			c.bytes.release(cap(frame))
			return
		}
		if !ready {
			c.bytes.release(cap(frame))
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
			c.bytes.release(cap(frame))
			c.protocolFailure(errProtocol)
			return
		}
		c.logLifecycle(env, "received")
		if env.Type == "response" {
			c.bytes.release(cap(frame))
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
				if r.catalog != nil {
					err := c.receiveCommandCatalogResponse(r.catalog, env)
					if errors.Is(err, errProtocol) {
						c.protocolFailure(errProtocol)
					}
					c.finishCommandCatalogQuery(r.catalog, err)
					if errors.Is(err, errProtocol) {
						return
					}
					c.logLifecycle(env, "response_queued")
					continue
				}
				if env.Command == "get_state" && *env.Success {
					c.receiveCommandCatalogSession(env.Data)
				}
				c.receiveUsageResponse(env)
				response := result{data: env.Data}
				if !*env.Success {
					var rejection error
					if env.Command == "set_ask_dialog" {
						var reason string
						if json.Unmarshal(env.Error, &reason) == nil && reason == "Unknown command: set_ask_dialog" {
							rejection = errAskDialogUnsupported
						}
					}
					if rejection == nil {
						rejection = errors.New("omp: RPC command rejected")
					}
					response.err = &classifiedError{kind: "rejected", err: rejection}
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
		if env.Type == "extension_ui_request" {
			if tracker := c.startupUI.Load(); tracker != nil {
				tracker.observe(env.ID, frame)
			}
		}
		if env.Type == "session_info_update" {
			c.receiveCommandCatalogSession(frame)
		}
		if env.Type == "available_commands_update" {
			if err := c.receiveCommandCatalogUpdate(frame); err != nil {
				c.bytes.release(cap(frame))
				c.protocolFailure(errProtocol)
				return
			}
		}
		if env.Type == "command_output" && c.captureMetadata(frame) {
			c.bytes.release(cap(frame))
			continue
		}
		select {
		case c.queuedEvents <- json.RawMessage(frame):
			// The receiver, not this buffered send, releases byte credit.
			c.logLifecycle(env, "event_queued")
		default:
			details := queueOverflowDetails{reason: "event_slots", stage: "event_enqueue", eventKind: env.Type,
				budget: byteBudgetFailure{used: c.bytes.usedBytes(), requested: cap(frame)}, queued: len(c.queuedEvents)}
			c.bytes.release(cap(frame))
			c.logLifecycle(env, "rejected")
			c.queueOverflow(errors.New("omp: event queue overflow; execution state uncertain"), details)
			return
		}
	}
}

func readLine(reader *bufio.Reader, limit int) ([]byte, error) {
	return readLineBudget(reader, limit, nil, nil)
}

func readLineBudget(reader *bufio.Reader, limit int, budget *byteBudget, failure *byteBudgetFailure) ([]byte, error) {
	var data []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(part) > limit-len(data) {
			budget.release(cap(data))
			return nil, errProtocol
		}
		needed := len(data) + len(part)
		if needed > cap(data) {
			capacity := max(needed, 2*cap(data))
			if capacity > limit {
				capacity = limit
			}
			requested := capacity - cap(data)
			if used, ok := budget.reserve(requested); !ok {
				if failure != nil {
					*failure = byteBudgetFailure{used: used, requested: requested}
				}
				budget.release(cap(data))
				return nil, errQueueOverflow
			}
			replacement := make([]byte, len(data), capacity)
			copy(replacement, data)
			data = replacement
		}
		data = append(data, part...)
		if err == nil {
			return data[:len(data)-1], nil
		}
		if err != bufio.ErrBufferFull {
			budget.release(cap(data))
			if len(data) != 0 {
				return nil, errProtocol
			}
			return nil, err
		}
	}
}

type frameDecoder struct {
	physical, logical   int
	id                  string
	count, length, next int
	data                []byte
	budget              *byteBudget
	reassembled         bool
	budgetFailure       byteBudgetFailure
}

func (d *frameDecoder) push(data []byte) ([]byte, error) {
	d.reassembled = false
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
		if used, ok := d.budget.reserve(chunk.Length); !ok {
			d.budgetFailure = byteBudgetFailure{used: used, requested: chunk.Length}
			return nil, errQueueOverflow
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
	d.reassembled = true
	logical, err := decodeObject(result)
	if err != nil || logical.Type == "rpc_chunk" {
		d.budget.release(cap(result))
		return nil, errProtocol
	}
	return result, nil
}
