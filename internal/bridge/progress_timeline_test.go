package bridge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/config"
)

func awaitTimelineResult(t *testing.T, w *worker) timelineResult {
	t.Helper()
	select {
	case result := <-w.timeline.results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("activity send did not complete")
		return timelineResult{}
	}
}

func TestVerboseTimelineGroupsToolActivityAndInterimText(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "verbose"
	w.b.cfg.QueueCapacity = 2
	queueReady(t, w, command)
	command("missing-terminal")
	w.dispatch()
	if !w.taskRunning() {
		t.Fatal("fixture task was not dispatched")
	}
	w.event([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"PRIVATE"}}`))
	w.event([]byte(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Inspecting the source"}]}}`))
	w.event([]byte(`{"type":"tool_execution_start","toolCallId":"tool-1","toolName":"read","arguments":{"path":"SECRET path"}}`))
	w.event([]byte(`{"type":"tool_execution_end","toolCallId":"tool-1"}`))
	allowInitialProgress(w)
	w.flushTimeline(time.Now())
	waitFor(t, func() bool { return f.messageCount() == 1 })
	f.mu.Lock()
	message := f.messages[0]
	f.mu.Unlock()
	text, _ := message["text"].(string)
	for _, want := range []string{"Activity:", "Update: Inspecting the source", "read: started", "read: completed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("verbose timeline omitted %q: %q", want, text)
		}
	}
	for _, private := range []string{"PRIVATE", "SECRET path", "tool-1"} {
		if strings.Contains(text, private) {
			t.Fatalf("verbose timeline exposed %q: %q", private, text)
		}
	}
	if sentReplyTarget(message) != float64(2) {
		t.Fatalf("timeline reply target = %v", sentReplyTarget(message))
	}
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","content":[{"type":"text","text":"Done"}]}]}`))
	output, err := w.b.db.NextOutput()
	if err != nil || output.Text != "Done" {
		t.Fatalf("final reply = %+v, error = %v", output, err)
	}
}

func TestVerboseTimelineHostToolRenameKeepsHistoryConsistent(t *testing.T) {
	for _, tc := range []struct {
		name           string
		afterFirstSend bool
		renameAfterEnd bool
	}{
		{name: "pending start"},
		{name: "sent start", afterFirstSend: true},
		{name: "late callback", renameAfterEnd: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeHTTP{}
			oldTransport := http.DefaultTransport
			http.DefaultTransport = f
			ctx, cancel := context.WithCancel(context.Background())
			w := testWorker(t, &worker{b: testBridge(t, &Bridge{cfg: config.Config{ProgressMode: "verbose"}, tg: newTestTelegram(t)}), key: target{chat: -10, thread: 11}, ctx: ctx, cancel: cancel})
			t.Cleanup(func() {
				w.stopTimeline()
				cancel()
				w.background.Wait()
				http.DefaultTransport = oldTransport
			})
			w.beginTask(queued{id: 10, replyTo: 2})
			w.event([]byte(`{"type":"tool_execution_start","toolCallId":"tool-1","toolName":"some_generic_tool"}`))
			allowInitialProgress(w)
			var first timelineResult
			if tc.afterFirstSend {
				w.flushTimeline(time.Now())
				waitFor(t, func() bool { return f.messageCount() == 1 })
				first = awaitTimelineResult(t, w)
				w.timelineFinished(first)
			}
			if tc.renameAfterEnd {
				w.event([]byte(`{"type":"tool_execution_end","toolCallId":"tool-1"}`))
			}
			w.event([]byte(`{"type":"host_tool_call","id":"host-1","toolCallId":"tool-1","toolName":"telegram_send"}`))
			if !tc.renameAfterEnd {
				if progress := w.renderProgress(); !strings.Contains(progress, "telegram_send: running") {
					t.Fatalf("active tool name was not corrected: %q", progress)
				}
				w.event([]byte(`{"type":"tool_execution_end","toolCallId":"tool-1"}`))
			}
			now := time.Now()
			if tc.afterFirstSend {
				now = first.completedAt.Add(timelineInterval)
			}
			w.flushTimeline(now)
			wantCount := 1
			if tc.afterFirstSend {
				wantCount = 2
			}
			wantName := "telegram_send"
			if tc.afterFirstSend || tc.renameAfterEnd {
				wantName = "some_generic_tool"
			}
			waitFor(t, func() bool { return f.messageCount() == wantCount })
			w.timelineFinished(awaitTimelineResult(t, w))
			f.mu.Lock()
			var history strings.Builder
			for _, message := range f.messages {
				history.WriteString(message["text"].(string))
			}
			f.mu.Unlock()
			if !strings.Contains(history.String(), "- "+wantName+": started") || !strings.Contains(history.String(), "- "+wantName+": completed") {
				t.Fatalf("renamed tool history diverged: %q", history.String())
			}
			otherName := "some_generic_tool"
			if tc.afterFirstSend || tc.renameAfterEnd {
				otherName = "telegram_send"
			}
			if strings.Contains(history.String(), "- "+otherName+": ") {
				t.Fatalf("renamed tool history mixed names: %q", history.String())
			}
		})
	}
}

func TestVerboseTimelineMergesWhileSendingAndCoolsAfterCompletion(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "verbose"
	w.b.cfg.QueueCapacity = 2
	gate := make(chan struct{})
	f.activityGate = gate
	f.activityStarted = make(chan string, 2)
	queueReady(t, w, command)
	command("missing-terminal")
	w.dispatch()
	w.event([]byte(`{"type":"tool_execution_start","toolCallId":"read-1","toolName":"read"}`))
	allowInitialProgress(w)
	w.flushTimeline(time.Now())
	select {
	case <-f.activityStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first activity request did not start")
	}
	w.event([]byte(`{"type":"tool_execution_end","toolCallId":"read-1"}`))
	w.event([]byte(`{"type":"tool_execution_start","toolCallId":"grep-1","toolName":"grep"}`))
	w.timelineFinished(timelineResult{generation: w.binding.Generation - 1, turn: w.turn, active: w.active, completedAt: time.Now().Add(-time.Hour)})
	w.flushTimeline(time.Now().Add(time.Hour))
	gate <- struct{}{}
	first := awaitTimelineResult(t, w)
	w.timelineFinished(first)
	w.flushTimeline(first.completedAt.Add(timelineInterval - time.Nanosecond))
	select {
	case text := <-f.activityStarted:
		t.Fatalf("activity sent before completion cooldown: %q", text)
	case <-time.After(50 * time.Millisecond):
	}
	w.flushTimeline(first.completedAt.Add(timelineInterval))
	var second string
	select {
	case second = <-f.activityStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("merged activity did not start after cooldown")
	}
	if !strings.Contains(second, "read: completed") || !strings.Contains(second, "grep: started") || strings.Contains(second, "read: started") {
		t.Fatalf("second activity was stale: %q", second)
	}
	gate <- struct{}{}
	w.timelineFinished(awaitTimelineResult(t, w))
	if got := f.messageCount(); got != 2 {
		t.Fatalf("activity requests = %d, want 2", got)
	}
}

func TestVerboseTimelineSkipsFinalAndShortTaskUpdates(t *testing.T) {
	for _, short := range []bool{false, true} {
		name := "final only"
		if short {
			name = "short tool call"
		}
		t.Run(name, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			w.b.cfg.ProgressMode = "verbose"
			w.b.cfg.QueueCapacity = 2
			queueReady(t, w, command)
			command("missing-terminal")
			w.dispatch()
			w.event([]byte(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Final result"}]}}`))
			if short {
				w.event([]byte(`{"type":"tool_execution_start","toolCallId":"tool-1","toolName":"read"}`))
			} else {
				allowInitialProgress(w)
			}
			w.flushTimeline(time.Now())
			w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","content":[{"type":"text","text":"Final result"}]}]}`))
			if got := f.messageCount(); got != 0 {
				t.Fatalf("terminal/short task sent %d activity messages", got)
			}
			output, err := w.b.db.NextOutput()
			if err != nil || output.Text != "Final result" {
				t.Fatalf("final reply = %+v, error = %v", output, err)
			}
		})
	}
}

func TestVerboseTimelineLimitsMessagesAndDropsTerminalText(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "verbose"
	w.b.cfg.QueueCapacity = 2
	queueReady(t, w, command)
	command("missing-terminal")
	w.dispatch()
	if !w.taskRunning() {
		t.Fatal("fixture task was not dispatched")
	}
	allowInitialProgress(w)
	now := time.Now()
	for i := range maxTimelineAttempts + 2 {
		w.event([]byte(fmt.Sprintf(`{"type":"tool_execution_start","toolCallId":"tool-%d","toolName":"read"}`, i)))
		w.flushTimeline(now)
		if i < maxTimelineAttempts {
			count := i + 1
			waitFor(t, func() bool { return f.messageCount() == count })
			result := awaitTimelineResult(t, w)
			w.timelineFinished(result)
			now = result.completedAt.Add(timelineInterval)
		}
	}
	if got := f.messageCount(); got != maxTimelineAttempts {
		t.Fatalf("verbose timeline sent %d messages, limit %d", got, maxTimelineAttempts)
	}
	w.event([]byte(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Final result"}]}}`))
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","content":[{"type":"text","text":"Final result"}]}]}`))
	w.flushTimeline(time.Now().Add(12 * timelineInterval))
	if got := f.messageCount(); got != maxTimelineAttempts {
		t.Fatalf("terminal text produced extra timeline messages: %d", got)
	}
	output, err := w.b.db.NextOutput()
	if err != nil || output.Text != "Final result" {
		t.Fatalf("final reply changed: %+v, error = %v", output, err)
	}
}

func TestVerboseTimelineFailureDoesNotBlockFinalReply(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.cfg.ProgressMode = "verbose"
	w.b.cfg.QueueCapacity = 2
	f.failProgress = true
	queueReady(t, w, command)
	command("missing-terminal")
	w.dispatch()
	if !w.taskRunning() {
		t.Fatal("fixture task was not dispatched")
	}
	w.event([]byte(`{"type":"tool_execution_start","toolCallId":"tool-1","toolName":"read"}`))
	allowInitialProgress(w)
	w.flushTimeline(time.Now())
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.progressCalls > 0
	})
	first := awaitTimelineResult(t, w)
	w.timelineFinished(first)
	w.event([]byte(`{"type":"tool_execution_start","toolCallId":"tool-2","toolName":"grep"}`))
	f.mu.Lock()
	initialCalls := f.progressCalls
	f.mu.Unlock()
	w.flushTimeline(first.completedAt.Add(timelineInterval - time.Nanosecond))
	f.mu.Lock()
	earlyCalls := f.progressCalls
	f.mu.Unlock()
	if earlyCalls != initialCalls {
		t.Fatalf("failed send bypassed cooldown: %d -> %d", initialCalls, earlyCalls)
	}
	w.flushTimeline(first.completedAt.Add(timelineInterval))
	w.timelineFinished(awaitTimelineResult(t, w))
	f.mu.Lock()
	finalCalls := f.progressCalls
	f.mu.Unlock()
	if finalCalls != initialCalls+1 {
		t.Fatalf("failed send dropped later activity: %d -> %d", initialCalls, finalCalls)
	}
	w.event([]byte(`{"type":"agent_end","isTerminal":true,"messages":[{"role":"assistant","content":[{"type":"text","text":"Delivered"}]}]}`))
	output, err := w.b.db.NextOutput()
	if err != nil || output.Text != "Delivered" {
		t.Fatalf("timeline failure changed final reply: %+v, error = %v", output, err)
	}
}
