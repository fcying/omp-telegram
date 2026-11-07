package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func assistantMessage(t *testing.T, w *worker, text string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
	requireStoreOK(t, err)
	w.event(raw)
}

// Final metadata is projected above the last immutable body, never in an orphan row.
func finalReplyStatus(t *testing.T, w *worker, id int64, bodies ...string) string {
	t.Helper()
	rows := researchReplies(t, w, id)
	if len(bodies) == 0 || len(rows) != len(bodies) {
		t.Fatalf("task %d reply rows = %q, want %d bodies with attached final status", id, rows, len(bodies))
	}
	for i, body := range bodies[:len(bodies)-1] {
		if replyBody(t, rows[i]) != body {
			t.Fatalf("task %d intermediate body %d = %q, want %q", id, i, rows[i], body)
		}
	}
	header, body := attachedReplyBody(t, rows[len(rows)-1])
	if body != bodies[len(bodies)-1] {
		t.Fatalf("task %d final body = %q, want %q", id, body, bodies[len(bodies)-1])
	}
	return header
}

func attachedReplyBody(t *testing.T, text string) (string, string) {
	t.Helper()
	header, body, ok := strings.Cut(text, "\n\n")
	if !ok || header == "" || strings.Contains(header, "\n") || body == "" {
		t.Fatalf("missing compact header above an intact final body: %q", text)
	}
	return header, body
}

func replyBody(t *testing.T, text string) string {
	t.Helper()
	_, body := attachedReplyBody(t, text)
	return body
}

func researchBody(t *testing.T, w *worker, id int64) string {
	t.Helper()
	rows := researchReplies(t, w, id)
	if len(rows) == 0 {
		t.Fatalf("task %d missing final reply", id)
	}
	for i := range rows {
		rows[i] = replyBody(t, rows[i])
	}
	return strings.Join(rows, "\n\n")
}

func stoppedAssistantMessage(t *testing.T, w *worker, text string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{
		"role": "assistant", "stopReason": "stop", "model": "safe", "provider": "fixture", "duration": 1000,
		"usage":   map[string]any{"output": 100, "totalTokens": 1100},
		"content": []any{map[string]any{"type": "text", "text": text}},
	}})
	requireStoreOK(t, err)
	w.event(raw)
}

func deliverReplies(t *testing.T, w *worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.b.deliver(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			requireStoreOK(t, err)
		case <-time.After(5 * time.Second):
			t.Error("outbox delivery did not stop")
		}
	}()
	waitFor(t, func() bool {
		var pending int
		err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE state IN ('pending','sending') OR header_pending=1 OR (state='done' AND message_id>0 AND actions_pending<>0)").Scan(&pending)
		if err != nil || pending != 0 {
			return false
		}
		// Do not cancel delivery between its acknowledgement and progress cleanup.
		messages, err := w.b.db.CompletedProgressMessages(0)
		return err == nil && len(messages) == 0
	})
}

func TestCompletedQuestionDeliveredBeforeTaskEndsWithProgressOff(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	client, binding, task := w.client, w.binding, w.active
	assistantMessage(t, w, "May I proceed?")
	rows := researchReplies(t, w, task)
	if len(rows) != 1 || replyBody(t, rows[0]) != "May I proceed?" || queueState(t, w, task) != "submitted" || !w.taskActive() {
		t.Fatalf("completed question not durably queued during active task: %q", rows)
	}
	deliverReplies(t, w)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) != 1 {
		t.Fatalf("question was not sent exactly once: %+v", f.messages)
	}
	text, _ := f.messages[0]["text"].(string)
	header, body, _ := strings.Cut(text, "\n")
	if strings.Contains(header, "ctx ?") || strings.TrimLeft(body, "\n") != "May I proceed?" || f.messages[0]["message_id"] != nil || sentReplyTarget(f.messages[0]) != float64(2) {
		t.Fatalf("question was not sent as its own reply: %+v", f.messages)
	}
	if queueState(t, w, task) != "submitted" || w.client != client || !sameBindingIdentity(w.binding, binding) || w.owner != 7 {
		t.Fatal("early body delivery settled the task or changed its owner/session")
	}
}

func TestIdenticalAssistantMessagesRemainDistinctAtFinalSnapshot(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	assistantMessage(t, w, "Please confirm.")
	assistantMessage(t, w, "Please confirm.")
	w.event([]byte(`{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"Please confirm."}]},{"role":"assistant","content":[{"type":"text","text":"Please confirm."}]},{"role":"assistant","content":[{"type":"text","text":"Proceeding."}]}]}`))
	awaitReplyStatus(t, w)
	finalReplyStatus(t, w, 2, "Please confirm.", "Please confirm.", "Proceeding.")
	if queueState(t, w, 2) != "done" {
		t.Fatal("final snapshot did not settle the original inbox")
	}
}

func TestCompactedSnapshotCountsAllMessageRoles(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	w.event([]byte(`{"type":"message_end","message":{"role":"user","content":"PRIVATE user"}}`))
	assistantMessage(t, w, "Which option?")
	w.event([]byte(`{"type":"message_end","message":{"role":"toolResult","content":[{"type":"text","text":"PRIVATE tool"}]}}`))
	w.event([]byte(`{"type":"agent_end","isTerminal":false,"messageCount":4,"messages":[{"role":"toolResult","content":[{"type":"text","text":"PRIVATE tool"}]},{"role":"assistant","content":[{"type":"text","text":"Using option B."}]}]}`))
	w.event([]byte(`{"type":"agent_end","isTerminal":false,"messageCount":4,"messages":[{"role":"assistant","content":[{"type":"text","text":"Using option B."}]}]}`))
	if queueState(t, w, 2) != "submitted" {
		t.Fatal("compacted continuation settled the root")
	}
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messageCount":5,"messages":[{"role":"toolResult","content":[{"type":"text","text":"PRIVATE tool"}]},{"role":"assistant","content":[{"type":"text","text":"Using option B."}]},{"role":"assistant","content":[{"type":"text","text":"Result."}]}]}`))
	awaitReplyStatus(t, w)
	finalReplyStatus(t, w, 2, "Which option?", "Using option B.", "Result.")
}

type pendingProgressEditHTTP struct {
	fallback *fakeHTTP
	started  chan struct{}
	gate     chan struct{}
}

func (f *pendingProgressEditHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/editMessageText") {
		close(f.started)
		select {
		case <-f.gate:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return f.fallback.RoundTrip(r)
}

func TestAcceptedReplyWhileProgressEditPendingPreservesWholeMarkdown(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.b.cfg.QueueCapacity = 4
	command("/new " + t.TempDir())
	ready, err := w.b.db.NextOutput()
	requireStoreOK(t, err)
	requireStoreOK(t, w.b.db.MarkOutput(ready.ID, "sending"))
	requireStoreOK(t, w.b.db.MarkOutput(ready.ID, "done"))
	command("wait")
	w.dispatch()
	waitSteerRootStart(t, w)
	waitSteerWorker(t, w, func() bool { return !w.rpcOperationActive && w.taskActive() })
	w.previewResult = make(chan previewResult, 1)
	allowInitialProgress(w)
	w.flushPreview()
	initial := <-w.previewResult
	requireStoreOK(t, initial.err)
	w.previewFinished(initial)
	assistantMessage(t, w, "**May I proceed?**")
	deliverReplies(t, w)
	sendRealtimePreview(t, w)
	transport := &pendingProgressEditHTTP{fallback: f, started: make(chan struct{}), gate: make(chan struct{})}
	defer func() {
		select {
		case <-transport.gate:
		default:
			close(transport.gate)
		}
	}()
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = f }()
	w.event([]byte(`{"type":"tool_execution_start","toolCallId":"read-1","toolName":"read"}`))
	w.flushPreview()
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("progress edit was not submitted")
	}
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"**Using "}}`))
	command("Yes, proceed.")
	if queueState(t, w, 3) != "submitted" || queueState(t, w, 2) != "submitted" || w.active != 2 || w.owner != 7 || !w.steerFence.active {
		t.Fatal("accepted steer changed root ownership or settled root")
	}
	close(transport.gate)
	select {
	case result := <-w.previewResult:
		if result.err != nil && !errors.Is(result.err, context.Canceled) {
			t.Fatalf("frozen progress edit failed unexpectedly: %v", result.err)
		}
		w.previewFinished(result)
	case <-time.After(5 * time.Second):
		t.Fatal("pending progress edit did not finish")
	}
	rows := researchReplies(t, w, 2)
	if len(rows) != 1 || replyBody(t, rows[0]) != "**May I proceed?**" {
		t.Fatalf("accepted reply froze a partial prefix or rewrote question: %q", rows)
	}
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"option B.**"}}`))
	assistantMessage(t, w, "**Using option B.**")
	deliverReplies(t, w)
	f.mu.Lock()
	defer f.mu.Unlock()
	questions, answers := 0, 0
	for _, req := range f.messages {
		text, _ := req["text"].(string)
		if strings.Contains(text, "May I proceed?") {
			questions++
			header, body, _ := strings.Cut(text, "\n")
			if strings.Contains(header, "ctx ?") || strings.TrimLeft(body, "\n") != "<b>May I proceed?</b>" || req["message_id"] != nil {
				t.Fatalf("question was edited or lost formatting: %+v", req)
			}
		}
		if strings.Contains(text, "Using ") {
			answers++
			header, body, _ := strings.Cut(text, "\n")
			if strings.Contains(header, "ctx ?") || strings.TrimLeft(body, "\n") != "<b>Using option B.</b>" || req["message_id"] != nil {
				t.Fatalf("unfinished Markdown was split or edited: %+v", req)
			}
		}
	}
	if questions != 1 || answers != 1 {
		t.Fatalf("question/answer sends = %d/%d: %+v", questions, answers, f.messages)
	}
}

func TestCompletedBodyPreservesCodeBeyondFormerProgressTail(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	intro := strings.Repeat("intro\n", 30)
	payload := strings.TrimSuffix(strings.Repeat("value := 1\n", 230), "\n")
	body := intro + "```go\n" + payload + "\n```\nFinal paragraph."
	assistantMessage(t, w, body)
	deliverReplies(t, w)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) != 1 {
		t.Fatalf("fitting assistant body was duplicated or split: %d sends", len(f.messages))
	}
	req := f.messages[0]
	text, _ := req["text"].(string)
	before, rest, opened := strings.Cut(text, "<pre>")
	code, after, closed := strings.Cut(rest, "</pre>")
	if req["message_id"] != nil || req["parse_mode"] != "HTML" || !opened || !closed {
		t.Fatalf("completed code body was edited or lost its code formatting: %+v", req)
	}
	header, introBody, _ := strings.Cut(before, "\n")
	if strings.Contains(header, "ctx ?") || strings.TrimSpace(introBody) != strings.TrimSpace(intro) || strings.TrimSuffix(code, "\n") != payload || strings.TrimSpace(after) != "Final paragraph." {
		t.Fatalf("completed body was clipped to a tail or changed its content: %q", text)
	}
}

func TestNativeYieldWithoutMessageEndKeepsBodiesSeparate(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"First complete native round."}}`))
	w.event([]byte(`{"type":"agent_end","isTerminal":false,"messages":[]}`))
	rows := researchReplies(t, w, 2)
	if len(rows) != 1 || replyBody(t, rows[0]) != "First complete native round." || queueState(t, w, 2) != "submitted" {
		t.Fatalf("native yield dropped its unended body or settled root: %q", rows)
	}
	w.event([]byte(`{"type":"agent_start"}`))
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Second round."}}`))
	stoppedAssistantMessage(t, w, "Second round.")
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","content":[{"type":"text","text":"Second round."}]}]}`))
	awaitReplyStatus(t, w)
	finalReplyStatus(t, w, 2, "First complete native round.", "Second round.")
}

func TestTerminalWithoutMessageEndPreservesWholePendingBody(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	assistantMessage(t, w, "Earlier question?")
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"**Unfinished "}}`))
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"but truthful body**"}}`))
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[]}`))
	awaitReplyStatus(t, w)
	finalReplyStatus(t, w, 2, "Earlier question?", "**Unfinished but truthful body**")
	if queueState(t, w, 2) != "done" {
		t.Fatal("truthful terminal boundary did not settle root")
	}
}

func TestEmptyTerminalCommitCleansProgressAfterAlreadyDeliveredBody(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	w.b.cfg.ProgressMode = "summary"
	w.previewResult = make(chan previewResult, 1)
	allowInitialProgress(w)
	w.flushPreview()
	initial := <-w.previewResult
	requireStoreOK(t, initial.err)
	w.previewFinished(initial)
	assistantMessage(t, w, "Already delivered question?")
	deliverReplies(t, w)
	current := sendRealtimePreview(t, w)
	transport := &pendingProgressEditHTTP{fallback: f, started: make(chan struct{}), gate: make(chan struct{})}
	defer func() {
		select {
		case <-transport.gate:
		default:
			close(transport.gate)
		}
	}()
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = f }()
	w.event([]byte(`{"type":"tool_execution_start","toolCallId":"read-1","toolName":"read"}`))
	w.flushPreview()
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("progress edit did not start")
	}
	w.finish()
	if queueState(t, w, 2) != "done" {
		t.Fatal("empty final transaction did not mark inbox done")
	}
	close(transport.gate)
	select {
	case result := <-w.previewResult:
		requireStoreOK(t, result.err)
		w.previewFinished(result)
	case <-time.After(5 * time.Second):
		t.Fatal("late progress edit did not finish")
	}
	waitFor(t, func() bool {
		var pending int
		err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM progress_messages WHERE inbox_id=2").Scan(&pending)
		return err == nil && pending == 0
	})
	deleted := f.deletedMessageIDs()
	if len(deleted) < 2 {
		t.Fatalf("empty completion left a frozen or current preview: %v", deleted)
	}
	for _, id := range deleted {
		if id != initial.id && id != current.id {
			t.Fatalf("empty completion deleted a formal body: %d", id)
		}
	}
	rows := researchReplies(t, w, 2)
	if len(rows) != 1 || replyBody(t, rows[0]) != "Already delivered question?" {
		t.Fatalf("empty completion invented a body or lost progress cleanup: %q", rows)
	}
}

func TestAssistantAppendFailureKeepsUncommittedWholeBody(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	assistantMessage(t, w, "Committed question?")
	client, binding, active, owner := w.client, w.binding, w.active, w.owner
	_, err := w.b.db.DB.Exec("CREATE TRIGGER reject_assistant_body BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT, 'body rejected'); END")
	requireStoreOK(t, err)
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"**Uncommitted whole body**"}}`))
	assistantMessage(t, w, "**Uncommitted whole body**")
	rows := researchReplies(t, w, active)
	if len(rows) != 1 || replyBody(t, rows[0]) != "Committed question?" || queueState(t, w, active) != "submitted" {
		t.Fatalf("failed append lost earlier body, leaked a partial write, or settled task: %q", rows)
	}
	if w.ctx.Err() == nil || w.stream.String() != "**Uncommitted whole body**" || w.client != client || !sameBindingIdentity(w.binding, binding) || w.active != active || w.owner != owner {
		t.Fatal("failed append cleared uncommitted text or changed logical task/session ownership")
	}
}

func TestTerminalSnapshotCorrectsOnlyUnshownCurrentBody(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	assistantMessage(t, w, "Original question?")
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"obsolete unshown draft"}}`))
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messageCount":2,"messages":[{"role":"assistant","content":[{"type":"text","text":"Correct final body."}]}]}`))
	awaitReplyStatus(t, w)
	finalReplyStatus(t, w, 2, "Original question?", "Correct final body.")
	if queueState(t, w, 2) != "done" {
		t.Fatal("authoritative final body did not settle the original inbox")
	}
}

func TestTerminalSnapshotCannotRewriteCommittedMessageEnds(t *testing.T) {
	for _, compacted := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "compacted-prefix"}[compacted], func(t *testing.T) {
			w, _, _, _ := replyStatusWorker(t)
			count := 1
			bodies := []string{"Old body."}
			messages := []any{}
			if compacted {
				w.event([]byte(`{"type":"message_end","message":{"role":"user","content":"PRIVATE"}}`))
				stoppedAssistantMessage(t, w, "Immutable earlier body.")
				count = 3
				bodies = append([]string{"Immutable earlier body."}, bodies...)
				messages = append(messages, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Must not replace committed text."}}})
			}
			stoppedAssistantMessage(t, w, "Old body.")
			deliverReplies(t, w)
			delivery, metrics := w.assistantDelivery, w.replyStatus.metrics
			corrected := map[string]any{
				"role": "assistant", "stopReason": "stop", "model": "safe", "provider": "fixture", "duration": 1000,
				"usage":   map[string]any{"output": 100, "totalTokens": 1100},
				"content": []any{map[string]any{"type": "text", "text": "Correct body."}},
			}
			messages = append(messages, corrected)
			raw, err := json.Marshal(map[string]any{"type": "agent_end", "isTerminal": true, "messageCount": count, "messages": messages})
			requireStoreOK(t, err)
			w.event(raw)
			if w.stream.Len() != 0 || w.assistantDelivery != delivery || w.replyStatus.metrics != metrics {
				t.Fatal("same-position snapshot rewrote committed text or recounted its position, bytes, or metrics")
			}
			// The same boundary with a further-compacted prefix remains idempotent.
			raw, err = json.Marshal(map[string]any{"type": "agent_end", "isTerminal": true, "messageCount": count, "messages": []any{corrected}})
			requireStoreOK(t, err)
			w.event(raw)
			if w.assistantDelivery != delivery || w.replyStatus.metrics != metrics {
				t.Fatal("repeated equivalent terminal boundary recounted the committed body")
			}
			awaitReplyStatus(t, w)
			header := finalReplyStatus(t, w, 2, bodies...)
			if !strings.Contains(header, "`safe`") || !strings.Contains(header, "100 t/s") || queueState(t, w, 2) != "done" {
				t.Fatalf("immutable body lost final status or settlement: %q", header)
			}
		})
	}
}

func TestNonterminalSnapshotCannotRewriteCommittedMessageEnds(t *testing.T) {
	for _, compacted := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "compacted-prefix"}[compacted], func(t *testing.T) {
			w, _, _, _ := replyStatusWorker(t)
			count := 1
			bodies := []string{"Old body."}
			if compacted {
				w.event([]byte(`{"type":"message_end","message":{"role":"user","content":"Original prompt"}}`))
				assistantMessage(t, w, "Immutable earlier question?")
				count = 3
				bodies = append([]string{"Immutable earlier question?"}, bodies...)
			}
			stoppedAssistantMessage(t, w, "Old body.")
			deliverReplies(t, w)
			metrics := w.replyStatus.metrics
			client, binding, turn, owner := w.client, w.binding, w.turn, w.owner
			raw, err := json.Marshal(map[string]any{"type": "agent_end", "isTerminal": false, "messageCount": count, "messages": []any{
				map[string]any{"role": "assistant", "stopReason": "stop", "content": []any{map[string]any{"type": "text", "text": "Correct body."}}},
			}})
			requireStoreOK(t, err)
			w.event(raw)
			finalReplyStatus(t, w, 2, bodies...)
			if queueState(t, w, 2) != "submitted" || !w.awaitingContinuation || w.client != client || !sameBindingIdentity(w.binding, binding) || w.turn != turn || w.owner != owner || w.replyStatus.metrics != metrics {
				t.Fatal("nonterminal snapshot completed its root, changed ownership, or recounted metrics")
			}
			w.event([]byte(`{"type":"agent_start"}`))
			stoppedAssistantMessage(t, w, "Continuation final.")
			w.event([]byte(`{"type":"agent_end","isTerminal":true,"messageCount":1,"messages":[]}`))
			awaitReplyStatus(t, w)
			finalReplyStatus(t, w, 2, append(bodies, "Continuation final.")...)
			if queueState(t, w, 2) != "done" {
				t.Fatal("continuation did not settle its original root")
			}
		})
	}
}

func TestNonterminalSnapshotRetainsRemainingInputBudget(t *testing.T) {
	w, _, _, trace := replyStatusWorker(t)
	assistantMessage(t, w, strings.Repeat("x", maxAssistantOutputBytes-32))
	stoppedAssistantMessage(t, w, "Old body.")
	binding := w.binding
	raw, err := json.Marshal(map[string]any{"type": "agent_end", "isTerminal": false, "messageCount": 3, "messages": []any{
		map[string]any{"role": "assistant", "stopReason": "stop", "content": []any{map[string]any{"type": "text", "text": strings.Repeat("Y", 33)}}},
	}})
	requireStoreOK(t, err)
	w.event(raw)
	if queueState(t, w, 2) != "uncertain" || w.client != nil || !sameBindingIdentity(w.binding, binding) {
		t.Fatal("unseen nonterminal body bypassed the remaining budget or lost its session")
	}
	if researchTraceCount(trace, "prompt", "missing-terminal") != 1 {
		t.Fatal("resource retirement replayed the original task")
	}
}

func TestMetadataOnlyTerminalPreservesCommittedBody(t *testing.T) {
	for _, reason := range []string{"stop", "error", "aborted"} {
		t.Run(reason, func(t *testing.T) {
			w, _, _, _ := replyStatusWorker(t)
			stoppedAssistantMessage(t, w, "Valid committed body.")
			terminal := map[string]any{"role": "assistant", "stopReason": reason}
			if reason == "error" {
				terminal["errorMessage"] = "Provider ended the request"
			}
			raw, err := json.Marshal(map[string]any{"type": "agent_end", "isTerminal": true, "messageCount": 1, "messages": []any{terminal}})
			requireStoreOK(t, err)
			w.event(raw)
			if reason == "stop" {
				awaitReplyStatus(t, w)
				finalReplyStatus(t, w, 2, "Valid committed body.")
				if queueState(t, w, 2) != "done" {
					t.Fatal("metadata-only success did not settle the committed body")
				}
				return
			}
			rows := researchReplies(t, w, 2)
			wantState := "uncertain"
			if reason == "aborted" {
				wantState = "cancelled"
			}
			if len(rows) != 2 || replyBody(t, rows[0]) != "Valid committed body." || rows[1] == "" || w.replyStatus.pending != nil || queueState(t, w, 2) != wantState {
				t.Fatalf("terminal metadata lost immutable text or reported false success: %q", rows)
			}
		})
	}
}

func TestTerminalOnlyBodyUsesDisplayBudget(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	corrected := strings.Repeat("corrected ", maxAssistantOutputBytes/10+1)
	raw, err := json.Marshal(map[string]any{"type": "agent_end", "isTerminal": true, "messageCount": 1, "messages": []any{map[string]any{
		"role": "assistant", "stopReason": "stop", "content": []any{map[string]any{"type": "text", "text": corrected}},
	}}})
	requireStoreOK(t, err)
	w.event(raw)
	if w.stream.String() != corrected || w.assistantDelivery.bytes != 0 || w.assistantDelivery.messages != 1 {
		t.Fatal("terminal-only body was treated as extra streamed input or consumed a committed body budget")
	}
	awaitReplyStatus(t, w)
	rows := researchReplies(t, w, 2)
	if len(rows) < 2 || len(rows) > maxFinalReplyMessages || rows[len(rows)-1] != replyTruncatedNotice || queueState(t, w, 2) != "done" {
		t.Fatalf("terminal-only body lost bounded successful delivery: %d rows", len(rows))
	}
	_, first := attachedReplyBody(t, rows[0])
	if !strings.HasPrefix(first, "corrected ") {
		t.Fatal("terminal-only snapshot lost its authoritative body")
	}
	var size int
	for _, row := range rows {
		size += len(row)
	}
	if size > maxFinalReplyBytes {
		t.Fatalf("terminal-only body exceeded display byte budget: %d", size)
	}
}

func TestTerminalOnlyBodyWaitsForFinalHeaderWithoutPlaceholderSend(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Unshown draft"}}`))
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messageCount":1,"messages":[{"role":"assistant","stopReason":"stop","model":"safe","provider":"fixture","duration":1000,"usage":{"output":100,"totalTokens":1100},"content":[{"type":"text","text":"Authoritative terminal body."}]}]}`))
	sample := receiveReplyStatus(t, w)
	if rows := researchReplies(t, w, 2); len(rows) != 0 || f.messageCount() != 0 || queueState(t, w, 2) != "submitted" {
		t.Fatalf("terminal-only body escaped before its first final header or sent a placeholder: %q", rows)
	}
	w.operationReturned(sample)
	awaitReplyStatus(t, w)
	header := finalReplyStatus(t, w, 2, "Authoritative terminal body.")
	if !strings.Contains(header, "100 t/s") || queueState(t, w, 2) != "done" {
		t.Fatalf("terminal-only first delivery lost confirmed metadata or settlement: %q", header)
	}
	deliverReplies(t, w)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) != 1 || f.messages[0]["message_id"] != nil {
		t.Fatalf("terminal-only completion did not send one combined first reply: %+v", f.messages)
	}
	text, _ := f.messages[0]["text"].(string)
	_, body := attachedReplyBody(t, text)
	if body != "Authoritative terminal body." {
		t.Fatalf("terminal-only snapshot leaked its unfinished draft: %q", body)
	}
}

func TestMessageEndDeliversBeforeFinalMetadataAndOnlyHeaderChanges(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	client, binding, active := w.client, w.binding, w.active
	question := "**May I proceed?**"
	answer := "**Using option B.**\n\n[Result](https://example.com/result?q=a&b=c)"
	stoppedAssistantMessage(t, w, question)
	deliverReplies(t, w)
	nativeRPC(t, w, "fixture_get_state_update", map[string]any{
		"contextUsage": map[string]any{"tokens": 1100, "contextWindow": 10000, "percent": 11},
	})
	stoppedAssistantMessage(t, w, answer)
	rows := researchReplies(t, w, active)
	if len(rows) != 2 || replyBody(t, rows[0]) != question || replyBody(t, rows[1]) != answer || w.replyStatus.pending != nil {
		t.Fatalf("message_end waited for final metadata instead of durably appending both bodies: %q", rows)
	}
	for _, row := range rows {
		header, _ := attachedReplyBody(t, row)
		if !strings.HasPrefix(header, "💬") {
			t.Fatalf("message_end marked an unfinished task complete: %q", header)
		}
	}
	var outputID int64
	var originalBody, originalHeader string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT id,text,header FROM outbox WHERE inbox_id=? ORDER BY id DESC LIMIT 1", active).Scan(&outputID, &originalBody, &originalHeader))
	if originalBody != answer || originalHeader == "" {
		t.Fatalf("body and status were not stored independently: body=%q header=%q", originalBody, originalHeader)
	}
	deliverReplies(t, w)
	if f.messageCount() != 2 || queueState(t, w, active) != "submitted" || !w.taskActive() || w.owner != 7 || w.client != client || !sameBindingIdentity(w.binding, binding) {
		t.Fatal("completed bodies were not sent exactly once before the root ended")
	}
	f.mu.Lock()
	initialText, _ := f.messages[1]["text"].(string)
	f.mu.Unlock()
	_, renderedBody := attachedReplyBody(t, initialText)
	if !strings.Contains(renderedBody, "<b>Using option B.</b>") || !strings.Contains(renderedBody, "<a href=\"https://example.com/result?q=a&amp;b=c\">Result</a>") {
		t.Fatalf("early answer lost Markdown/link semantics: %q", initialText)
	}
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messageCount":2,"messages":[]}`))
	sample := receiveReplyStatus(t, w)
	var body, header string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text,header FROM outbox WHERE id=?", outputID).Scan(&body, &header))
	if body != originalBody || header != originalHeader || f.messageCount() != 2 || queueState(t, w, active) != "submitted" {
		t.Fatal("pending final metadata rewrote or withheld an already sent body")
	}
	w.operationReturned(sample)
	awaitReplyStatus(t, w)
	header = finalReplyStatus(t, w, active, question, answer)
	if !strings.HasPrefix(header, "✅") {
		t.Fatalf("successful task did not mark its last formal reply: %q", header)
	}
	firstHeader, _ := attachedReplyBody(t, researchReplies(t, w, active)[0])
	if !strings.HasPrefix(firstHeader, "💬") {
		t.Fatal("task completion changed an earlier formal reply")
	}
	for _, metric := range []string{"`safe`", "100 t/s", "11% (10k)"} {
		if !strings.Contains(header, metric) {
			t.Fatalf("actual final metric %q missing from %q", metric, header)
		}
	}
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text,header FROM outbox WHERE id=?", outputID).Scan(&body, &header))
	if body != originalBody || header == originalHeader {
		t.Fatal("final sampling did not update only the separately persisted header")
	}
	deliverReplies(t, w)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) != 3 {
		t.Fatalf("expected two body Sends and one final header edit: %+v", f.messages)
	}
	for i, request := range f.messages[:2] {
		var replyTo any
		if i == 0 {
			replyTo = float64(active)
		}
		if request["message_id"] != nil || request["parse_mode"] != "HTML" || sentReplyTarget(request) != replyTo {
			t.Fatalf("formal Send changed identity or repeated the root quote: %+v", request)
		}
	}
	edit := f.messages[2]
	updatedText, _ := edit["text"].(string)
	updatedHeader, updatedBody := attachedReplyBody(t, updatedText)
	if edit["message_id"] != float64(2) || edit["parse_mode"] != "HTML" || updatedBody != renderedBody || !strings.HasPrefix(updatedHeader, "✅") || !strings.Contains(updatedHeader, "11% (10k)") {
		t.Fatalf("final status edit changed the immutable rendered body or targeted another message: %+v", edit)
	}
	if queueState(t, w, active) != "done" || w.client != client || !sameBindingIdentity(w.binding, binding) {
		t.Fatal("final metadata changed root ownership or session identity")
	}
}

func TestIncompleteTaskNeverMarksDeliveredReplySuccessful(t *testing.T) {
	for _, outcome := range []struct {
		name, reason, state string
	}{
		{"cancelled", "aborted", "cancelled"},
		{"failed", "error", "uncertain"},
		{"uncertain", "", "uncertain"},
	} {
		t.Run(outcome.name, func(t *testing.T) {
			w, f, _, _ := replyStatusWorker(t)
			root := w.active
			stoppedAssistantMessage(t, w, "Already delivered body")
			deliverReplies(t, w)
			if outcome.reason == "" {
				w.finishUncertain("No confirmed terminal result")
			} else {
				raw, err := json.Marshal(map[string]any{"type": "agent_end", "isTerminal": true, "messageCount": 1, "messages": []any{map[string]any{
					"role": "assistant", "stopReason": outcome.reason, "content": []any{map[string]any{"type": "text", "text": "Already delivered body"}},
				}}})
				requireStoreOK(t, err)
				w.event(raw)
			}
			deliverReplies(t, w)
			if queueState(t, w, root) != outcome.state {
				t.Fatalf("terminal outcome was changed: %s", queueState(t, w, root))
			}
			rows := researchReplies(t, w, root)
			header, body := attachedReplyBody(t, rows[0])
			if !strings.HasPrefix(header, "💬") || body != "Already delivered body" {
				t.Fatal("incomplete task changed a confirmed reply or marked success")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, request := range f.messages {
				text, _ := request["text"].(string)
				if strings.HasPrefix(text, "✅") {
					t.Fatal("incomplete task sent a success marker")
				}
			}
		})
	}
}

func TestCommittedBodyIgnoresNoninteractiveUIMetadata(t *testing.T) {
	w, _, trace := replyUsageWorker(t, "normal")
	nativeRPC(t, w, "fixture_get_state_update", map[string]any{
		"contextUsage": map[string]any{"tokens": 230000, "contextWindow": 500000, "percent": 46},
	})
	w.event([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","model":"safe","provider":"openai-codex","duration":1000,"usage":{"output":100,"totalTokens":230000},"content":[{"type":"text","text":"Only final body."}]}}`))
	delivery, metrics := w.assistantDelivery, w.replyStatus.metrics
	for _, method := range []string{"setStatus", "setWidget", "notify", "cancel", "futureMetadata"} {
		raw, err := json.Marshal(map[string]any{"type": "extension_ui_request", "id": "metadata-" + method, "method": method, "message": "Native metadata"})
		requireStoreOK(t, err)
		w.event(raw)
		if rows := researchReplies(t, w, 2); len(rows) != 1 || replyBody(t, rows[0]) != "Only final body." || w.stream.Len() != 0 || w.assistantDelivery != delivery || w.replyStatus.metrics != metrics || w.replyStatus.pending != nil {
			t.Fatalf("noninteractive %s changed the committed body or started final sampling: %q", method, rows)
		}
	}
	if researchTraceCount(trace, "prompt", "/usage") != 0 {
		t.Fatal("UI metadata started a premature quota sample")
	}
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messageCount":1,"messages":[]}`))
	awaitReplyStatus(t, w)
	if queueState(t, w, 2) != "done" {
		t.Fatal("quota refresh blocked settlement after harmless UI metadata")
	}
	awaitProviderUsage(t, w)
	header := finalReplyStatus(t, w, 2, "Only final body.")
	for _, metric := range []string{"`safe`", "100 t/s", "46% (500k)", "70% 35%"} {
		if !strings.Contains(header, metric) {
			t.Fatalf("final header lost actual sampled metric %q after UI metadata: %q", metric, header)
		}
	}
	if queueState(t, w, 2) != "done" || researchTraceCount(trace, "prompt", "/usage") != 1 {
		t.Fatal("UI metadata changed final settlement or private quota sampling count")
	}
}

func TestCommittedBodySurvivesCachedCompactedTerminalPrefixes(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	w.event([]byte(`{"type":"message_end","message":{"role":"user","content":"PRIVATE"}}`))
	assistantMessage(t, w, "Earlier question?")
	stoppedAssistantMessage(t, w, "Final from cached prefix.")
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messageCount":3,"messages":[]}`))
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messageCount":3,"messages":[{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"Final from cached prefix."}]}]}`))
	awaitReplyStatus(t, w)
	finalReplyStatus(t, w, 2, "Earlier question?", "Final from cached prefix.")
	if queueState(t, w, 2) != "done" || w.stream.Len() != 0 {
		t.Fatal("cached terminal prefix lost settlement or retained completed body")
	}
}

func TestNewSteerPreservesAlreadyCommittedBody(t *testing.T) {
	w, _, command, _ := replyStatusWorker(t)
	stoppedAssistantMessage(t, w, "Awaiting native continuation?")
	command("Yes, continue.")
	if rows := researchReplies(t, w, 2); len(rows) != 1 || replyBody(t, rows[0]) != "Awaiting native continuation?" || queueState(t, w, 2) != "submitted" || queueState(t, w, 3) != "submitted" || w.owner != 7 {
		t.Fatalf("incoming steer changed the immutable body or settled its root: %q", rows)
	}
	w.event([]byte(`{"type":"agent_start"}`))
	if rows := researchReplies(t, w, 2); len(rows) != 1 || replyBody(t, rows[0]) != "Awaiting native continuation?" {
		t.Fatalf("native continuation changed or duplicated the committed body: %q", rows)
	}
}

func TestStopAndFailurePreserveCommittedBodyAndTruthfulNotice(t *testing.T) {
	for _, event := range []string{"stop", "failure", "teardown", "cancelled-teardown"} {
		t.Run(event, func(t *testing.T) {
			w, _, command, _ := replyStatusWorker(t)
			stoppedAssistantMessage(t, w, "Whole committed body.")
			switch event {
			case "stop":
				command("/stop")
			case "failure":
				w.event([]byte(`{"type":"agent_end","isTerminal":true,"messageCount":2,"messages":[{"role":"assistant","stopReason":"error","errorMessage":"Provider request failed"}]}`))
			case "teardown":
				w.teardownWorker(true)
			case "cancelled-teardown":
				w.cancel()
				w.teardownWorker(true)
			}
			rows := researchReplies(t, w, 2)
			if len(rows) != 2 {
				t.Fatalf("interruption lost committed body or truthful notice: %q", rows)
			}
			if replyBody(t, rows[0]) != "Whole committed body." || rows[1] == "" {
				t.Fatalf("interruption lost immutable body, notice, or truthful status: %q", rows)
			}
			wantState := "uncertain"
			if event == "stop" {
				wantState = "cancelled"
			}
			if w.stream.Len() != 0 || w.replyStatus.header != "" || queueState(t, w, 2) != wantState {
				t.Fatal("interrupted scope retained pending text/header or settled with the wrong outcome")
			}
			if (event == "teardown" || event == "cancelled-teardown") && w.client != nil {
				t.Fatal("worker teardown retained its runtime")
			}
		})
	}
}

func TestEveryMessageEndSendsConfirmedBodyWithoutFinalQuery(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	reasons := []string{"", "toolUse", "stop", "error", "aborted"}
	for _, reason := range reasons {
		raw, err := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{
			"role": "assistant", "stopReason": reason, "model": "safe", "provider": "fixture", "duration": 1000,
			"usage":   map[string]any{"output": 100, "totalTokens": 1100},
			"content": []any{map[string]any{"type": "text", "text": "Immediate question " + reason + "?"}},
		}})
		requireStoreOK(t, err)
		w.event(raw)
		deliverReplies(t, w)
	}
	rows := researchReplies(t, w, 2)
	if len(rows) != len(reasons) || queueState(t, w, 2) != "submitted" || w.replyStatus.pending != nil {
		t.Fatalf("intermediate status delayed delivery or changed root: %q", rows)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) != len(reasons) {
		t.Fatalf("intermediate status created extra Sends: %+v", f.messages)
	}
	for i, reason := range reasons {
		header, body := attachedReplyBody(t, rows[i])
		if !strings.Contains(header, "`safe`") || !strings.Contains(header, "100 t/s") || body != "Immediate question "+reason+"?" {
			t.Fatalf("intermediate lost confirmed status or body: %q", rows[i])
		}
		request := f.messages[i]
		text, _ := request["text"].(string)
		sentHeader, sentBody, _ := strings.Cut(text, "\n")
		var replyTo any
		if i == 0 {
			replyTo = float64(2)
		}
		if !strings.Contains(sentHeader, "<code>safe</code>") || strings.TrimLeft(sentBody, "\n") != body || request["message_id"] != nil || sentReplyTarget(request) != replyTo {
			t.Fatalf("formal status/body lost immutable Send or first-only quote: %+v", request)
		}
	}
}
