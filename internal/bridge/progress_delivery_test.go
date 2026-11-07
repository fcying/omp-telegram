package bridge

import (
	"context"
	"net/http"
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
