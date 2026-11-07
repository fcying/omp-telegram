package bridge

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestFormalDeliveryFailureRetainsPreview(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		name := "rejected"
		if uncertain {
			name = "uncertain"
		}
		t.Run(name, func(t *testing.T) {
			w, f := realtimePreviewWorker(t)
			const body = "Keep this visible until its formal delivery is confirmed."
			w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Keep this visible until its formal delivery is confirmed."}}`))
			preview := sendRealtimePreview(t, w)
			assistantMessage(t, w, body)
			outputID := w.assistantDelivery.statusOutboxID
			f.mu.Lock()
			if uncertain {
				f.failProgress = true
			} else {
				f.sendResponses = map[int64][]fakeHTTPResponse{
					w.key.chat: {{status: http.StatusBadRequest, body: `{"ok":false,"error_code":400,"description":"formal delivery rejected"}`}},
				}
			}
			f.mu.Unlock()
			deliverReplies(t, w)
			w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[]}`))
			awaitReplyStatus(t, w)
			// Terminal UI cleanup lanes remain alive until the worker is cancelled.
			w.cancel()
			w.background.Wait()
			w.b.reconcileProgressCleanup(context.Background())
			var state, stored string
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT state,text FROM outbox WHERE id=?", outputID).Scan(&state, &stored))
			want := "failed"
			if uncertain {
				want = "uncertain"
			}
			previews, err := w.b.db.ProgressMessages(preview.taskID)
			requireStoreOK(t, err)
			if state != want || stored != body || len(previews) != 1 || previews[0].MessageID != preview.id || len(f.deletedMessageIDs()) != 0 {
				t.Fatalf("formal %s lost the visible body: state=%s body=%q previews=%v deleted=%v", want, state, stored, previews, f.deletedMessageIDs())
			}
		})
	}
}

func TestRunningProgressCleanupRetriesWithoutDeletingCurrentOrFormalMessages(t *testing.T) {
	w, f := realtimePreviewWorker(t)
	root := w.active
	old := sendRealtimePreview(t, w)
	f.mu.Lock()
	f.progressDeleteStatus = http.StatusInternalServerError
	f.mu.Unlock()
	assistantMessage(t, w, "A persistent completed answer")
	deliveryCtx, stopDelivery := context.WithCancel(w.ctx)
	deliveryDone := make(chan error, 1)
	go func() { deliveryDone <- w.b.deliver(deliveryCtx) }()
	defer func() {
		stopDelivery()
		requireStoreOK(t, <-deliveryDone)
	}()
	waitFor(t, func() bool {
		var state string
		err := w.b.db.DB.QueryRow("SELECT state FROM outbox WHERE id=?", w.assistantDelivery.statusOutboxID).Scan(&state)
		return err == nil && state == "done" && len(f.deletedMessageIDs()) != 0
	})
	w.background.Wait()
	if len(f.deletedMessageIDs()) == 0 {
		t.Fatal("diagnostic deletion failure did not occur")
	}
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Current live work"}}`))
	current := sendRealtimePreview(t, w)
	f.mu.Lock()
	f.progressDeleteStatus = 0
	f.mu.Unlock()
	ctx, cancel := context.WithCancel(w.ctx)
	done := make(chan struct{})
	go func() {
		w.b.runProgressJanitor(ctx, time.Millisecond)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	awaitProgressDeletion(t, w, root, old.id)
	previews, err := w.b.db.ProgressMessages(root)
	requireStoreOK(t, err)
	if queueState(t, w, root) != "submitted" || len(previews) != 1 || previews[0].MessageID != current.id {
		t.Fatalf("running janitor removed the current preview: %+v", previews)
	}
	for _, id := range f.deletedMessageIDs() {
		if id != old.id {
			t.Fatalf("janitor removed a current or formal message: %d", id)
		}
	}
}

func TestSteerCannotDiscardPreviewAwaitingFormalDelivery(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_PENDING_STEER", "executing")
	w, f := realtimePreviewWorker(t)
	old := sendRealtimePreview(t, w)
	assistantMessage(t, w, "Completed text awaiting delivery")
	nativeInput(t, w, 101, "Continue after the completed message", false)
	w.operationReturned(waitOperation(t, w))
	w.background.Wait()
	if deleted := f.deletedMessageIDs(); len(deleted) != 0 {
		t.Fatalf("steer bypassed the formal delivery boundary: %v", deleted)
	}
	deliverReplies(t, w)
	awaitProgressDeletion(t, w, old.taskID, old.id)
}

func TestLocalCommandProgressRetiresWithoutAssistantReplies(t *testing.T) {
	for _, scenario := range []string{"ack", "completion", "late-preview", "delete-retry"} {
		t.Run(scenario, func(t *testing.T) {
			mode := "enabled"
			if scenario == "completion" {
				mode = "delayed-local"
			}
			w, _, trace := nativeWorker(t, mode)
			f := http.DefaultTransport.(*fakeHTTP)
			nativeCatalogReady(t, w)
			deliverReplies(t, w)
			w.b.cfg.ProgressMode = "summary"
			w.previewResult = make(chan previewResult, 4)
			nativeInput(t, w, 100, "/native_local", false)
			if scenario == "completion" {
				nativeUntil(t, w, func() bool { return !w.rpcOperationActive })
			}
			var preview previewResult
			var transport *acceptedPreviewHTTP
			release := func() {}
			if scenario == "late-preview" {
				transport = &acceptedPreviewHTTP{base: f, started: make(chan struct{}), gate: make(chan struct{})}
				release = sync.OnceFunc(func() { close(transport.gate) })
				defer release()
				http.DefaultTransport = transport
				defer func() { http.DefaultTransport = f }()
				allowInitialProgress(w)
				w.flushPreview()
				select {
				case <-transport.started:
				case <-time.After(5 * time.Second):
					t.Fatal("local command preview was not accepted")
				}
			} else {
				preview = sendRealtimePreview(t, w)
			}
			if scenario == "delete-retry" {
				f.mu.Lock()
				f.progressDeleteStatus = http.StatusInternalServerError
				f.mu.Unlock()
			}
			if scenario == "completion" {
				nativeRPC(t, w, "fixture_finish_local", nil)
			}
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			if scenario == "late-preview" {
				if transport.requestCtx.Err() != nil {
					t.Fatal("local completion cancelled the accepted preview before its identity arrived")
				}
				release()
				select {
				case preview = <-w.previewResult:
					requireStoreOK(t, preview.err)
					w.previewFinished(preview)
				case <-time.After(5 * time.Second):
					t.Fatal("late local preview acknowledgment did not arrive")
				}
			}
			if scenario == "delete-retry" {
				waitFor(t, func() bool { return len(f.deletedMessageIDs()) != 0 })
				// Join cleanup only after its first attempt, without waiting for the worker deadline.
				w.cancel()
				w.background.Wait()
				previews, err := w.b.db.ProgressMessages(100)
				requireStoreOK(t, err)
				if len(previews) != 1 || len(f.deletedMessageIDs()) == 0 {
					t.Fatal("transient deletion failure lost its cleanup identity")
				}
				f.mu.Lock()
				f.progressDeleteStatus = 0
				f.mu.Unlock()
				w.b.reconcileProgressCleanup(context.Background())
			}
			awaitProgressDeletion(t, w, 100, preview.id)
			var replies int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE inbox_id=100").Scan(&replies))
			if replies != 0 || w.taskActive() {
				t.Fatalf("local cleanup invented an assistant reply or retained ownership: replies=%d", replies)
			}
			for _, id := range f.deletedMessageIDs() {
				if id != preview.id {
					t.Fatalf("local cleanup deleted another message: %d", id)
				}
			}
			waitFixtureRPCTrace(t, trace, "prompt", "/native_local")
		})
	}
}

func TestLocalCommandCommitFailureKeepsProgress(t *testing.T) {
	w, _, _ := nativeWorker(t, "enabled")
	f := http.DefaultTransport.(*fakeHTTP)
	nativeCatalogReady(t, w)
	deliverReplies(t, w)
	w.b.cfg.ProgressMode = "summary"
	w.previewResult = make(chan previewResult, 4)
	nativeInput(t, w, 100, "/native_local", false)
	preview := sendRealtimePreview(t, w)
	_, err := w.b.db.DB.Exec(`CREATE TRIGGER reject_local_completion BEFORE UPDATE OF state ON inbox
WHEN OLD.id=100 AND NEW.state='done' BEGIN SELECT RAISE(ABORT,'completion rejected'); END`)
	requireStoreOK(t, err)
	w.operationReturned(waitOperation(t, w))
	w.background.Wait()
	previews, err := w.b.db.ProgressMessages(100)
	requireStoreOK(t, err)
	if queueState(t, w, 100) != "submitted" || len(previews) != 1 || previews[0].MessageID != preview.id || len(f.deletedMessageIDs()) != 0 {
		t.Fatalf("failed result commit discarded local progress: previews=%v deleted=%v", previews, f.deletedMessageIDs())
	}
}
