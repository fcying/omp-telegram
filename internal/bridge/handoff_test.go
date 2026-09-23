package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHandoffResultPreservesSessionAndReleasesQueue(t *testing.T) {
	for _, scenario := range []struct{ instructions, result string }{
		{"preserve decisions", "Handoff completed."},
		{"cancel", "Handoff canceled without a result."},
		{"fail", "omp rejected the handoff."},
	} {
		t.Run(scenario.instructions, func(t *testing.T) {
			f, db, send := setupBridge(t)
			send(update(1, 11, "/new "+t.TempDir()))
			waitBinding(t, db, 11)
			before, err := db.Binding(99, -10, 11)
			if err != nil {
				t.Fatal(err)
			}
			send(update(2, 11, "/handoff "+scenario.instructions))
			waitFor(t, func() bool { return f.has(11, scenario.result) })
			waitInputDone(t, db, 2)
			send(update(3, 11, "after handoff"))
			waitFor(t, func() bool { return f.has(11, "answer: after handoff") })
			waitInputDone(t, db, 3)
			after, err := db.Binding(99, -10, 11)
			lastUsed := after.LastUsedAt
			after.LastUsedAt = before.LastUsedAt
			if err != nil || after != before {
				t.Fatalf("handoff changed session binding: %+v, %v", after, err)
			}
			if lastUsed < before.LastUsedAt {
				t.Fatalf("handoff moved last-used timestamp backwards: before=%d after=%d", before.LastUsedAt, lastUsed)
			}
		})
	}
}

func TestPendingHandoffAllowsHelpCloseAndNewSession(t *testing.T) {
	f, db, send := setupBridge(t)
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)
	before, err := db.Binding(99, -10, 11)
	if err != nil {
		t.Fatal(err)
	}
	send(update(2, 11, "/handoff hold"))
	waitInputDone(t, db, 2)
	send(update(3, 11, "/help"))
	waitInputDone(t, db, 3)
	send(update(4, 11, "/close"))
	waitInputDone(t, db, 4)
	send(update(5, 11, "/new"))
	waitInputDone(t, db, 5)
	send(update(6, 11, "after closed handoff"))
	waitFor(t, func() bool { return f.has(11, "answer: after closed handoff") })
	after, err := db.Binding(99, -10, 11)
	if err != nil || after.Generation <= before.Generation || after.Session == before.Session {
		t.Fatalf("could not replace session after pending handoff: %+v, %v", after, err)
	}
	if f.has(11, "Handoff completed.") || f.has(11, "Handoff failed or its outcome is uncertain.") || f.has(11, "omp rejected the handoff.") {
		t.Fatal("old-generation handoff published a result after close")
	}
}

func TestHandoffRequiresIdleInstanceAndEmptyQueue(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/handoff")
	if w.client != nil {
		t.Fatal("handoff created an instance")
	}
	command("/new " + t.TempDir())
	w.b.cfg.QueueCapacity = 1
	command("queued work")
	command("/handoff preserve queued work")
	if w.busy || w.compacting || len(w.queue) != 1 {
		t.Fatal("handoff overtook the prompt queue")
	}
	w.clearQueue()
	command("wait")
	w.dispatch()
	active := w.active
	command("/handoff")
	if w.compacting || !w.busy || w.active != active {
		t.Fatal("handoff interrupted the active prompt")
	}
}

func TestUncertainHandoffAndCompactReleaseRuntime(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		kind       string
		err        error
		data       json.RawMessage
		cancelled  bool
		pendingRPC bool
	}{
		{name: "handoff timeout", kind: "handoff", pendingRPC: true},
		{name: "handoff cancellation", kind: "handoff", err: context.Canceled},
		{name: "handoff malformed result", kind: "handoff", data: json.RawMessage("invalid")},
		{name: "compact timeout", kind: "compact", err: context.DeadlineExceeded},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var rpcOrder string
			if scenario.pendingRPC {
				rpcOrder = filepath.Join(t.TempDir(), "rpc-order")
				t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_ORDER", rpcOrder)
			}
			w, _, command := setupWorkspaceWorker(t)
			command("/new test")
			before := w.binding
			client := w.client
			w.busy, w.compacting = true, true

			if scenario.pendingRPC {
				w.startOperation("handoff", client, func(ctx context.Context) (json.RawMessage, error) {
					// Keep the real pending-RPC timeout path without waiting 30 seconds.
					callCtx, cancel := context.WithTimeout(ctx, time.Second)
					defer cancel()
					return client.Call(callCtx, "handoff", map[string]any{"customInstructions": "hold"})
				}, 0, "")
				waitFor(t, func() bool {
					data, err := os.ReadFile(rpcOrder)
					return err == nil && string(data) == "handoff\n"
				})
				result := waitOperation(t, w)
				if result.err != context.DeadlineExceeded {
					t.Fatalf("held handoff error = %v, want deadline exceeded", result.err)
				}
				w.operationReturned(result)
				select {
				case <-client.Done():
				default:
					t.Fatal("timed-out handoff did not close the native client")
				}
			} else {
				w.operationFinished(operationResult{
					generation: w.binding.Generation,
					clientID:   client.ID(),
					kind:       scenario.kind,
					data:       scenario.data,
					cancelled:  scenario.cancelled,
					err:        scenario.err,
				})
			}
			if w.client != nil || w.runtime != runtimeReleased || !w.binding.Running || w.busy || w.compacting {
				t.Fatal("uncertain native operation retained or wedged its runtime")
			}

			command("/name lazy resumed")
			if w.client == nil || w.client.ID() == client.ID() || w.runtime != runtimeConnected || !sameBindingIdentity(w.binding, before) {
				t.Fatal("next operation did not lazily resume the preserved session")
			}
		})
	}
}

func TestRejectedHandoffKeepsRuntime(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new test")
	client := w.client
	w.handoff("fail")
	result := waitOperation(t, w)
	if result.err == nil {
		t.Fatal("fixture did not reject the handoff")
	}
	w.operationReturned(result)
	if w.client != client || w.runtime != runtimeConnected || w.busy || w.compacting {
		t.Fatal("explicit native rejection invalidated the reusable runtime")
	}
}
