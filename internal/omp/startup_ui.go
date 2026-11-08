package omp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
)

// Observation never changes protocol acceptance. Excess IDs are ignored, and
// hashing keeps both retained memory and the lifetime of raw UI IDs bounded.
const maxStartupPendingUI = 128

type startupUITracker struct {
	mu       sync.Mutex
	closed   bool
	pending  map[[sha256.Size]byte]string
	snapshot startupUISnapshot
}

type startupUISnapshot struct {
	count   int
	methods []string
	confirm bool
}

func (t *startupUITracker) observe(id string, frame []byte) {
	var ui struct {
		Method   string `json:"method"`
		TargetID string `json:"targetId"`
	}
	if json.Unmarshal(frame, &ui) != nil {
		return
	}
	switch ui.Method {
	case "cancel":
		if ui.TargetID == "" {
			return
		}
	case "ask", "confirm", "select", "input", "editor":
		if id == "" {
			return
		}
	default:
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if ui.Method == "cancel" {
		delete(t.pending, sha256.Sum256([]byte(ui.TargetID)))
		return
	}
	key := sha256.Sum256([]byte(id))
	if _, exists := t.pending[key]; !exists && len(t.pending) >= maxStartupPendingUI {
		return
	}
	if t.pending == nil {
		t.pending = make(map[[sha256.Size]byte]string)
	}
	t.pending[key] = ui.Method
}

// finish freezes evidence before shutdown can emit cancellation events. Readers
// that loaded the old pointer cannot add requests after this snapshot or cleanup.
func (t *startupUITracker) finish() startupUISnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return t.snapshot
	}
	t.closed = true
	snapshot := startupUISnapshot{count: len(t.pending)}
	for _, method := range []string{"ask", "confirm", "select", "input", "editor"} {
		for _, pending := range t.pending {
			if pending == method {
				snapshot.methods = append(snapshot.methods, method)
				snapshot.confirm = snapshot.confirm || method == "confirm"
				break
			}
		}
	}
	t.pending = nil
	t.snapshot = snapshot
	return snapshot
}

func (c *Client) releaseStartupUI() startupUISnapshot {
	if tracker := c.startupUI.Swap(nil); tracker != nil {
		return tracker.finish()
	}
	return startupUISnapshot{}
}

func (c *Client) discardStartupUI() {
	if tracker := c.startupUI.Swap(nil); tracker != nil {
		tracker.mu.Lock()
		tracker.closed = true
		tracker.pending = nil
		tracker.snapshot = startupUISnapshot{}
		tracker.mu.Unlock()
	}
}

func (c *Client) finishStartupFailure(parent, startup context.Context, stage string, cause error) error {
	snapshot := c.releaseStartupUI()
	// A terminal protocol/transport error takes precedence over a racing timeout.
	c.mu.Lock()
	terminal := c.err
	c.mu.Unlock()
	parentErr := parent.Err()
	if terminal != nil {
		// Parent-deadline teardown must not replace an already determined failure.
		teardown := ClassifyError(terminal) == "cancelled" || errors.Is(terminal, context.DeadlineExceeded)
		deadlineCleanup := teardown && (errors.Is(cause, context.DeadlineExceeded) || errors.Is(parentErr, context.DeadlineExceeded))
		if !deadlineCleanup {
			cause = terminal
		}
	}
	if errors.Is(parentErr, context.Canceled) {
		cause = context.Canceled
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		if c.protocolFailed.Load() {
			cause = errProtocol
		} else if snapshot.count > 0 {
			kind := "startup_timeout_pending_ui"
			if snapshot.confirm {
				kind = "startup_timeout_pending_confirm"
			}
			cause = &classifiedError{kind: kind, err: errors.New("omp: startup timed out with an unanswered UI request detected")}
		}
	}
	if terminal == nil {
		c.fail(cause)
	}
	causeKind := ClassifyError(cause)
	err := cause
	if stage == "ready" && causeKind == "process_exit" {
		// Only a pre-ready process exit can be reclassified from reaped stderr.
		err = c.startupFailure(startup)
	} else {
		_ = c.Close()
	}
	// Cancellation is normal daemon shutdown, not an actionable startup failure.
	if causeKind == "cancelled" {
		return context.Canceled
	}
	methods := snapshot.methods
	if methods == nil {
		methods = []string{}
	}
	c.log.Warn("rpc startup failed", "event", "rpc_startup_failed", "stage", stage,
		"reason", ClassifyError(err), "pending_ui_count", snapshot.count, "pending_ui_methods", methods)
	return err
}
