package bridge

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"omp-telegram/internal/store"
)

func steerInboxState(t *testing.T, w *worker, id int64) string {
	t.Helper()
	var state string
	if err := w.b.db.DB.QueryRow("SELECT state FROM inbox WHERE id=?", id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func waitSteerWorker(t *testing.T, w *worker, finished func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !finished() {
		select {
		case result := <-w.operations:
			w.operationReturned(result)
		case event, ok := <-w.client.Events():
			if !ok {
				t.Fatal("fixture RPC exited before steer completed")
			}
			w.event(event)
		case <-deadline:
			t.Fatal("steer result did not arrive")
		}
	}
}

func waitSteerRootStart(t *testing.T, w *worker) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-w.client.Events():
			if !ok {
				t.Fatal("fixture RPC exited before root start")
			}
			var header struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(event, &header); err != nil {
				t.Fatal(err)
			}
			w.event(event)
			if header.Type == "agent_start" {
				return
			}
		case <-deadline:
			t.Fatal("root agent_start was not observed")
		}
	}
}

func TestRunningTextSteersRootAndFollowupRunsSeparately(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	command("wait")
	w.dispatch()
	waitSteerRootStart(t, w)
	waitSteerWorker(t, w, func() bool { return w.rpcOperationActive == false && w.taskActive() })
	command("/followup later")
	if len(w.queue) != 1 || w.queue[0].text != "later" {
		t.Fatalf("explicit follow-up not queued: %+v", w.queue)
	}
	command("correct direction")
	if len(w.queue) != 1 || steerInboxState(t, w, 4) != "submitted" || !w.steerFence.active {
		t.Fatalf("running text was not submitted as steer: queue=%+v active=%t", w.queue, w.steerFence.active)
	}
	w.dispatch()
	if w.active != 2 {
		t.Fatal("follow-up displaced fenced root")
	}
	waitSteerWorker(t, w, func() bool { return !w.taskActive() })
	if steerInboxState(t, w, 4) != "done" || steerInboxState(t, w, 2) != "done" {
		t.Fatal("root and steer did not reach independent durable terminal states")
	}
	var finalText string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox WHERE inbox_id=2 AND reply_to=2").Scan(&finalText); err != nil || finalText != "answer: correct direction" {
		t.Fatalf("steered root final reply = %q, err=%v", finalText, err)
	}
	var steerReplies int
	if err := w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=4").Scan(&steerReplies); err != nil || steerReplies != 0 {
		t.Fatalf("steer produced a separate final reply: count=%d, err=%v", steerReplies, err)
	}
	if len(w.queue) != 1 {
		t.Fatal("steer consumed the explicit follow-up")
	}
	w.dispatch()
	if w.active != 3 {
		t.Fatal("follow-up did not start as the next root")
	}
	waitSteerWorker(t, w, func() bool { return !w.taskActive() })
	if steerInboxState(t, w, 3) != "done" {
		t.Fatal("follow-up did not finish as an independent root")
	}
}

func TestSteerTerminalFenceWaitsForLatestSettlement(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	command("wait")
	w.dispatch()
	waitSteerRootStart(t, w)
	waitSteerWorker(t, w, func() bool { return !w.rpcOperationActive && w.taskActive() })
	command("/followup later")
	message := update(4, 11, "correction")
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(4, raw); err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Submit(4, message.Message.MessageID); err != nil {
		t.Fatal(err)
	}
	second := update(5, 11, "second correction")
	secondRaw, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(5, secondRaw); err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Submit(5, second.Message.MessageID); err != nil {
		t.Fatal(err)
	}
	w.steerFence = steerFence{active: true, revision: 1, admission: 2, probeCancel: func() {}}
	w.steers = map[string]steerRequest{
		"901": {inboxID: 4, rootID: 2, generation: w.binding.Generation, turn: w.turn, clientID: w.client.ID(), admission: 1},
		"902": {inboxID: 5, rootID: 2, generation: w.binding.Generation, turn: w.turn, clientID: w.client.ID(), admission: 2},
	}
	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"answer: revised"}]}]}`))
	if w.active != 2 || len(w.queue) != 1 {
		t.Fatal("terminal agent_end crossed pending steer fence")
	}
	w.event([]byte(`{"type":"prompt_result","id":"902","status":"completed","sessionSettled":true}`))
	if w.active != 2 || !w.steerFence.settled || steerInboxState(t, w, 5) != "done" {
		t.Fatal("latest settlement did not wait for the earlier steer")
	}
	w.event([]byte(`{"type":"prompt_result","id":"901","status":"completed","sessionSettled":false}`))
	if steerInboxState(t, w, 4) != "done" || w.active != 2 || w.canDispatch() {
		t.Fatal("unsettled session released root fence")
	}
	w.event([]byte(`{"type":"session_settled"}`))
	if w.active != 2 {
		t.Fatal("unkeyed session_settled released root fence")
	}
	w.steeredQuiescenceFinished(steerProbeResult{client: w.client, generation: w.binding.Generation, rootID: w.active, turn: w.turn, revision: w.steerFence.revision - 1, settled: true})
	if w.active != 2 || w.canDispatch() {
		t.Fatal("stale settlement probe released a newer steer fence")
	}
	w.steeredQuiescenceFinished(steerProbeResult{client: w.client, generation: w.binding.Generation, rootID: w.active, turn: w.turn, revision: w.steerFence.revision, settled: true})
	if w.active != 0 || steerInboxState(t, w, 2) != "done" || !w.canDispatch() {
		t.Fatal("confirmed current settlement did not release root")
	}
}

func TestStopWithUnresolvedSteerHardStopsAndPreservesFollowup(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	command("wait")
	w.dispatch()
	waitSteerRootStart(t, w)
	waitSteerWorker(t, w, func() bool { return !w.rpcOperationActive && w.taskActive() })
	command("/followup later")
	message := update(4, 11, "correction")
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(4, raw); err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Submit(4, message.Message.MessageID); err != nil {
		t.Fatal(err)
	}
	client := w.client
	w.steerFence = steerFence{active: true, revision: 1}
	w.steers = map[string]steerRequest{"901": {inboxID: 4, rootID: 2, generation: w.binding.Generation, turn: w.turn, clientID: client.ID()}}
	w.stopActiveTask()
	if w.client != nil || len(w.queue) != 1 || w.active != 0 || w.steerFence.active {
		t.Fatal("Progress Stop left old root or steer available to subsequent tasks")
	}
	select {
	case <-client.Done():
	default:
		t.Fatal("old RPC process was not terminated before releasing follow-up")
	}
	if steerInboxState(t, w, 4) != string(store.InboxUncertain) || steerInboxState(t, w, 2) != string(store.InboxUncertain) {
		t.Fatal("Stop claimed uncertain root or steer was completed")
	}
}

func TestStopWithSteerClearsFollowup(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	command("wait")
	w.dispatch()
	waitSteerRootStart(t, w)
	waitSteerWorker(t, w, func() bool { return !w.rpcOperationActive && w.taskActive() })
	command("/followup later")
	message := update(4, 11, "correction")
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(4, raw); err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Submit(4, message.Message.MessageID); err != nil {
		t.Fatal(err)
	}
	w.steerFence = steerFence{active: true, revision: 1}
	w.steers = map[string]steerRequest{"901": {inboxID: 4, rootID: 2, generation: w.binding.Generation, turn: w.turn, clientID: w.client.ID()}}
	w.stop()
	if len(w.queue) != 0 || w.active != 0 || w.client != nil {
		t.Fatal("Stop kept the bridge queue or old runtime")
	}
	for id, want := range map[int64]string{2: "uncertain", 3: "cancelled", 4: "uncertain"} {
		if got := steerInboxState(t, w, id); got != want {
			t.Fatalf("inbox %d after Stop = %s, want %s", id, got, want)
		}
	}
}

func TestSteeredStopReportsFailedFollowupResume(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	command("wait")
	w.dispatch()
	waitSteerRootStart(t, w)
	waitSteerWorker(t, w, func() bool { return !w.rpcOperationActive && w.taskActive() })
	command("/followup later")
	message := update(4, 11, "correction")
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(4, raw); err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Submit(4, message.Message.MessageID); err != nil {
		t.Fatal(err)
	}
	w.steerFence = steerFence{active: true, revision: 1}
	w.steers = map[string]steerRequest{"901": {inboxID: 4, rootID: 2, generation: w.binding.Generation, turn: w.turn, clientID: w.client.ID()}}
	w.stopActiveTask()
	w.b.cfg.OMP = t.TempDir() + "/unavailable-omp"
	w.dispatch()
	if steerInboxState(t, w, 3) != "pending" || !w.resumeFailed {
		t.Fatal("failed resume claimed or discarded pending follow-up")
	}
	var notices int
	if err := w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE text LIKE '%No prompt was submitted%'").Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("missing single resume-failure notice: count=%d, err=%v", notices, err)
	}
	w.dispatch()
	if err := w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE text LIKE '%No prompt was submitted%'").Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("failed resume retried without user action: count=%d, err=%v", notices, err)
	}
}

func TestPendingSteerSharesBridgeQueueCapacity(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 1
	command("/new " + t.TempDir())
	command("wait")
	w.dispatch()
	waitSteerRootStart(t, w)
	waitSteerWorker(t, w, func() bool { return !w.rpcOperationActive && w.taskActive() })
	command("correction")
	if steerInboxState(t, w, 3) != "submitted" {
		t.Fatal("steer was not admitted while root ran")
	}
	command("/followup later")
	if steerInboxState(t, w, 4) != "cancelled" || len(w.queue) != 0 {
		t.Fatal("steer bypassed the pending input capacity")
	}
	waitSteerWorker(t, w, func() bool { return !w.taskActive() })
}

func TestSteerResultBeforeAcknowledgementStillSettlesRoot(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 2
	var index int
	var name, path string
	if err := w.b.db.DB.QueryRow("PRAGMA database_list").Scan(&index, &name, &path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TELEGRAM_FIXTURE_STEER_DB", path)
	t.Setenv("OMP_TELEGRAM_FIXTURE_STEER_INBOX", "3")
	command("/new " + t.TempDir())
	command("wait")
	w.dispatch()
	waitSteerRootStart(t, w)
	w.operationReturned(waitOperation(t, w))
	command("correction")
	deadline := time.After(5 * time.Second)
	for w.taskActive() {
		select {
		case raw, ok := <-w.client.Events():
			if !ok {
				t.Fatal("fixture exited before steer terminal")
			}
			w.event(raw)
		case <-deadline:
			t.Fatal("steer result blocked behind its acknowledgement")
		}
	}
	if got := steerInboxState(t, w, 3); got != "done" {
		t.Fatalf("early steer result settled as %s", got)
	}
	w.operationReturned(waitOperation(t, w))
	if w.active != 0 || w.steerFence.active || steerInboxState(t, w, 2) != "done" {
		t.Fatal("late ACK corrupted a settled root")
	}
}

func TestIdleWatchdogRetiresUnresolvedSteerWithoutReplayingFollowup(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	command("wait")
	w.dispatch()
	waitSteerRootStart(t, w)
	w.operationReturned(waitOperation(t, w))
	command("/followup later")
	message := update(4, 11, "correction")
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(4, raw); err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Submit(4, message.Message.MessageID); err != nil {
		t.Fatal(err)
	}
	w.steerFence = steerFence{active: true, revision: 1}
	w.steers = map[string]steerRequest{"901": {inboxID: 4, rootID: 2, generation: w.binding.Generation, turn: w.turn, clientID: w.client.ID()}}
	now := time.Now()
	w.lastActivity = now.Add(-2 * stuckTaskQuietPeriod)
	w.idleProbe.confirmed = now.Add(-stuckTaskQuietPeriod)
	idle, settled, pendingNative := rpcIdleState([]byte(`{"isStreaming":false,"isCompacting":false,"isSettled":false,"hasPendingAsyncWork":true,"queuedMessageCount":1}`))
	if !idle || settled || !pendingNative {
		t.Fatal("get_state pending native work was not recognized")
	}
	w.idleProbeFinished(idleProbeResult{client: w.client, generation: w.binding.Generation, turn: w.turn, active: w.active, revision: w.idleProbe.revision, idle: idle, settled: settled, pendingNative: pendingNative}, now)
	if w.client == nil || w.active != 2 || steerInboxState(t, w, 4) != "submitted" {
		t.Fatal("pending native work was treated as missing steer completion")
	}
	w.idleProbe.confirmed = now.Add(-stuckTaskQuietPeriod)
	w.idleProbeFinished(idleProbeResult{client: w.client, generation: w.binding.Generation, turn: w.turn, active: w.active, revision: w.idleProbe.revision, idle: true}, now)
	if w.client != nil || w.active != 0 || w.steerFence.active || len(w.queue) != 1 {
		t.Fatal("confirmed idle left the root or steer fence stuck")
	}
	if steerInboxState(t, w, 2) != "uncertain" || steerInboxState(t, w, 4) != "uncertain" || steerInboxState(t, w, 3) != "pending" {
		t.Fatal("watchdog replayed or misclassified an uncertain steer")
	}
}

func TestSteerStoreFailureDoesNotWriteRPC(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "rpc-trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 2
	w.b.fatal = make(chan error, 1)
	command("/new " + t.TempDir())
	command("wait")
	w.dispatch()
	waitSteerRootStart(t, w)
	w.operationReturned(waitOperation(t, w))
	if _, err := w.b.db.DB.Exec(`CREATE TRIGGER reject_steer BEFORE UPDATE ON inbox WHEN NEW.id=3 AND NEW.state='submitted' BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	command("correction")
	if w.ctx.Err() == nil || steerInboxState(t, w, 3) != "pending" {
		t.Fatal("failed durable submit still admitted steer")
	}
	select {
	case <-w.b.fatal:
	default:
		t.Fatal("persistence failure did not fail the worker")
	}
	_ = w.client.Close()
	for _, request := range fixtureRPCTrace(trace) {
		if request.kind == "prompt" && request.payload == "correction" {
			t.Fatal("RPC steer was written before durable submission")
		}
	}
}

func pendingNativeSteerWorker(t *testing.T, mode string) *worker {
	t.Helper()
	t.Setenv("OMP_TELEGRAM_FIXTURE_PENDING_STEER", mode)
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	command("wait")
	w.dispatch()
	waitSteerRootStart(t, w)
	w.operationReturned(waitOperation(t, w))
	command("/followup later")
	command("correction")
	drainWatchdogEvents(t, w)
	if len(w.steers) != 1 || steerInboxState(t, w, 4) != "submitted" || w.awaitingContinuation != (mode == "queued") {
		t.Fatal("fixture did not enter the requested native steer state")
	}
	return w
}

func TestPendingNativeSteerHasFiniteIdleRecovery(t *testing.T) {
	w := pendingNativeSteerWorker(t, "queued")
	client := w.client
	now := time.Now()
	for elapsed := time.Duration(0); elapsed <= 5*time.Minute; elapsed += stuckTaskQuietPeriod {
		result := probeResultFor(w)
		result.pendingNative = true
		w.idleProbeFinished(result, now.Add(elapsed))
		if elapsed < 5*time.Minute && (w.client != client || w.active != 2) {
			t.Fatal("native pending work retired before its grace period")
		}
	}
	if w.client != nil || w.active != 0 || w.steerFence.active || len(w.queue) != 1 {
		t.Fatal("permanent idle native queue left the conversation fenced")
	}
	select {
	case <-client.Done():
	default:
		t.Fatal("recovery released the fence before killing the old process")
	}
	for id, want := range map[int64]string{2: "uncertain", 3: "pending", 4: "uncertain"} {
		if got := steerInboxState(t, w, id); got != want {
			t.Fatalf("inbox %d = %s, want %s", id, got, want)
		}
	}
}

func TestNativeSteerStopStates(t *testing.T) {
	for _, mode := range []string{"queued", "executing"} {
		for _, stop := range []string{"progress", "command"} {
			t.Run(mode+"/"+stop, func(t *testing.T) {
				w := pendingNativeSteerWorker(t, mode)
				client := w.client
				pending, state := 1, "pending"
				if stop == "command" {
					w.stop()
					pending, state = 0, "cancelled"
				} else {
					w.stopActiveTask()
				}
				if w.client != nil || w.active != 0 || w.steerFence.active || len(w.queue) != pending {
					t.Fatal("Stop retained the old steer or mishandled follow-up")
				}
				select {
				case <-client.Done():
				default:
					t.Fatal("Stop did not terminate the old process")
				}
				for id, want := range map[int64]string{2: "uncertain", 3: state, 4: "uncertain"} {
					if got := steerInboxState(t, w, id); got != want {
						t.Fatalf("inbox %d = %s, want %s", id, got, want)
					}
				}
			})
		}
	}
}

func TestPendingNativeSteerIdleRecoveryRestartsAfterActivity(t *testing.T) {
	for _, activity := range []string{"agent_start", "streaming"} {
		t.Run(activity, func(t *testing.T) {
			w := pendingNativeSteerWorker(t, "queued")
			now := time.Now()
			probe := func(at time.Time, idle bool) {
				result := probeResultFor(w)
				result.idle = idle
				result.pendingNative = true
				w.idleProbeFinished(result, at)
			}
			probe(now, true)
			if activity == "agent_start" {
				w.event([]byte(`{"type":"agent_start"}`))
			} else {
				probe(now.Add(time.Minute), false)
			}
			probe(now.Add(4*time.Minute), true)
			probe(now.Add(5*time.Minute), true)
			if w.client == nil || w.active != 2 || steerInboxState(t, w, 4) != "submitted" {
				t.Fatal("activity failed to restart the pending idle grace period")
			}
			probe(now.Add(9*time.Minute), true)
			if w.client != nil || w.active != 0 || steerInboxState(t, w, 4) != "uncertain" {
				t.Fatal("pending idle grace period did not expire after activity")
			}
		})
	}
}

func TestSteeredStopDuringCompactionAllowsNextPrompt(t *testing.T) {
	for _, stop := range []string{"progress", "command"} {
		t.Run(stop, func(t *testing.T) {
			w := pendingNativeSteerWorker(t, "executing")
			w.event([]byte(`{"type":"auto_compaction_start"}`))
			next := int64(3)
			if stop == "command" {
				w.stop()
				if state := steerInboxState(t, w, 3); state != "cancelled" {
					t.Fatalf("cleared follow-up state = %s", state)
				}
				next = 5
				acceptWorkerInput(t, w, next, "after Stop")
			} else {
				w.stopActiveTask()
			}
			w.dispatch()
			if w.active != next {
				t.Fatalf("old compaction blocked next prompt: active=%d compacting=%t", w.active, w.compacting)
			}
			waitSteerWorker(t, w, func() bool { return !w.taskActive() })
			if state := steerInboxState(t, w, next); state != "done" {
				t.Fatalf("next prompt state = %s", state)
			}
		})
	}
}
