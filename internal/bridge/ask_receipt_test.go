package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"omp-telegram/internal/telegram"
)

type askReceiptTransport struct {
	base        *fakeHTTP
	messageID   int64
	entered     chan struct{}
	release     <-chan struct{}
	fail        bool
	pendingEdit bool
}

func (f *askReceiptTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/editMessageText") {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var message struct {
			ID       int64             `json:"message_id"`
			Text     string            `json:"text"`
			Keyboard telegram.Keyboard `json:"reply_markup"`
		}
		err = json.Unmarshal(body, &message)
		if err != nil {
			return nil, err
		}
		matches := message.ID == f.messageID && strings.Contains(message.Text, "<b>Your choice</b>")
		if f.pendingEdit {
			matches = message.ID == f.messageID && len(message.Keyboard.InlineKeyboard) != 0
		}
		if matches {
			close(f.entered)
			if f.pendingEdit {
				// A remote edit may finish after its local context was cancelled.
				<-f.release
			} else {
				select {
				case <-f.release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			if f.fail {
				return nil, errors.New("receipt edit failed")
			}
		}
	}
	return f.base.RoundTrip(r)
}

func TestAskReceiptCannotBlockOrOverwriteNextQuestion(t *testing.T) {
	for _, fail := range []bool{false, true} {
		for _, stop := range []bool{false, true} {
			name := "answer"
			if stop {
				name = "stop"
			}
			if fail {
				name += "-failed-edit"
			}
			t.Run(name, func(t *testing.T) {
				w, f, _ := askTestWorker(t)
				askTestOpen(t, w, "two", askTestQuestion("first", "First question?", false, "Go"), askTestQuestion("second", "Second question?", false, "Rust"))
				askTestClick(t, w, f, "Other")
				oldPrompt := askTestPrompt(t, f)
				a := w.ask
				oldDisplay := askDisplay{request: a, epoch: a.epoch, messageID: a.messageID, inputID: a.inputID, token: a.token}
				release := make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				t.Cleanup(unblock)
				transport := &askReceiptTransport{base: f, messageID: a.messageID, entered: make(chan struct{}), release: release, fail: fail}
				http.DefaultTransport = transport
				reply := func(id int64, text string, prompt *telegram.Message) {
					u := update(id, 11, text)
					u.Message.ReplyToMessage = prompt
					raw, err := json.Marshal(u)
					requireStoreOK(t, err)
					requireStoreOK(t, w.b.db.Accept(id, raw))
					w.handle(incoming{id: id, msg: u.Message})
				}
				reply(200, "first custom answer", oldPrompt)
				select {
				case <-transport.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("confirmed question receipt was not attempted")
				}
				askTestSettle(t, w)
				if w.ask != a || a.question != 1 || a.messageID == oldDisplay.messageID {
					t.Fatal("receipt edit blocked the next independent question")
				}
				askTestClick(t, w, f, "Other")
				messageID, inputID, token, epoch := a.messageID, a.inputID, a.token, a.epoch
				w.askOperationFinished(operationResult{kind: "ask_display", meta: oldDisplay, err: errors.New("late old display")})
				reply(201, "stale answer", oldPrompt)
				if w.ask != a || a.messageID != messageID || a.inputID != inputID || a.token != token || a.epoch != epoch || len(a.answers) != 1 {
					t.Fatal("late old display or input changed the next question")
				}
				if stop {
					w.stopActiveTask()
					nativeUntil(t, w, func() bool { return w.client == nil && !w.controlInProgress() })
				} else {
					reply(202, "second custom answer", askTestPrompt(t, f))
					responses := askTestState(t, w)
					if len(responses) != 1 || len(responses[0].Answers) != 2 || responses[0].Answers[1].Custom != "second custom answer" {
						t.Fatalf("receipt edit became a business ACK: %+v", responses)
					}
					askTestReceipt(t, w, f, messageID)
				}
				unblock()
				// A delayed display-only result cannot reopen a stopped or completed UI.
				for {
					result := waitOperation(t, w)
					w.operationReturned(result)
					if result.kind == "ask_receipt" && result.meta == oldDisplay.messageID {
						break
					}
				}
				if !stop {
					responses := askTestState(t, w)
					if len(responses) != 1 {
						t.Fatal("late receipt replayed the native answer")
					}
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				for _, message := range f.messages {
					if message["message_id"] == float64(messageID) && strings.Contains(message["text"].(string), "first custom answer") {
						t.Fatal("old receipt overwrote the next question")
					}
					if stop && message["message_id"] == float64(messageID) && strings.Contains(message["text"].(string), "<b>Your choice</b>") {
						t.Fatal("Stop marked an unanswered question as answered")
					}
				}
			})
		}
	}
}

func TestAskCancelledQuestionRetainsOptions(t *testing.T) {
	for _, reason := range []string{"cancel", "native", "stop", "task-stop"} {
		t.Run(reason, func(t *testing.T) {
			w, f, trace := askTestWorker(t)
			askTestOpen(t, w, "cancel-history", askTestQuestion("first", "Previous question?", false, "Previous answer"),
				map[string]any{"id": "current", "question": "Pick a destination", "multi": true,
					"options": []map[string]string{{"label": "A"}, {"label": "B"}, {"label": "C"}}})
			previousID := w.ask.messageID
			askTestClick(t, w, f, "Previous answer")
			previous := askTestReceipt(t, w, f, previousID)
			messageID := w.ask.messageID
			askTestClick(t, w, f, "2. B")
			switch reason {
			case "cancel":
				askTestClick(t, w, f, "Cancel")
			case "native":
				askTestEvent(t, w, map[string]any{"method": "cancel", "targetId": "cancel-history"})
			case "stop":
				nativeInput(t, w, 200, "/stop", false)
			case "task-stop":
				w.stopActiveTask()
			}
			text := askTestReceipt(t, w, f, messageID)
			for _, want := range []string{"Pick a destination", "1. A", "2. B", "3. C", "Canceled"} {
				if !strings.Contains(text, want) {
					t.Fatalf("cancelled question lost %q: %q", want, text)
				}
			}
			if strings.Contains(text, "Your choice") || strings.Contains(text, "Submitted") {
				t.Fatalf("unconfirmed multi-selection was recorded as an answer: %q", text)
			}
			if after := askTestReceipt(t, w, f, previousID); after != previous {
				t.Fatal("cancelling the current question changed a previous confirmed answer")
			}
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind != "extension_ui_response" {
					continue
				}
				var response askTestResponse
				requireStoreOK(t, json.Unmarshal([]byte(entry.payload), &response))
				if response.ID == "cancel-history" && (!response.Cancelled || len(response.Answers) != 0) {
					t.Fatalf("cancelled dialog submitted its partial answers: %+v", response)
				}
			}
		})
	}
}

func TestAskCancelledLateEditCannotEraseOptionsOrChangeReplacement(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			w, f, _ := askTestWorker(t)
			askTestOpen(t, w, "old-edit", askTestQuestion("old", "Old question?", false, "A", "B", "C"))
			messageID := w.ask.messageID
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			transport := &askReceiptTransport{base: f, messageID: messageID, entered: make(chan struct{}), release: release, fail: fail, pendingEdit: true}
			http.DefaultTransport = transport
			askTestCallback(w, 7, askTestButton(t, f, "Custom answer"))
			select {
			case <-transport.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("pending display edit did not reach the gate")
			}
			askTestEvent(t, w, map[string]any{"method": "cancel", "targetId": "old-edit"})
			askTestOpen(t, w, "replacement", askTestQuestion("new", "New question?", false, "Rust"))
			a := w.ask
			newID, token, epoch := a.messageID, a.token, a.epoch
			unblock()
			text := askTestReceipt(t, w, f, messageID)
			for _, want := range []string{"Old question?", "1. A", "2. B", "3. C", "Canceled"} {
				if !strings.Contains(text, want) {
					t.Fatalf("late display erased cancelled question history: %q", text)
				}
			}
			if strings.Contains(text, "Rust") || strings.Contains(text, "Your choice") || w.ask != a || a.messageID != newID || a.token != token || a.epoch != epoch {
				t.Fatal("old cancelled display adopted or changed its replacement")
			}
			askTestClick(t, w, f, "Rust")
			responses := askTestState(t, w)
			if len(responses) != 1 || responses[0].ID != "replacement" || len(responses[0].Answers) != 1 || strings.Join(responses[0].Answers[0].Selected, ",") != "Rust" {
				t.Fatalf("late cancelled display replayed a native response: %+v", responses)
			}
		})
	}
}
