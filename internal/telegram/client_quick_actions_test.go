package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestForceReplySendAndFallback(t *testing.T) {
	for _, fallback := range []string{"none", "reply target", "HTML"} {
		t.Run(fallback, func(t *testing.T) {
			var attempts atomic.Int32
			client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
				attempt := attempts.Add(1)
				var fields map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
					t.Error(err)
					return
				}
				if r.URL.Path != "/bot123:secret-token/sendMessage" || string(fields["chat_id"]) != "-10" || string(fields["message_thread_id"]) != "8" {
					t.Errorf("wrong prompt endpoint or destination: %s %s", r.URL.Path, fields)
				}
				var markup struct {
					ForceReply  bool            `json:"force_reply"`
					Selective   bool            `json:"selective"`
					Placeholder string          `json:"input_field_placeholder"`
					Inline      json.RawMessage `json:"inline_keyboard"`
				}
				if err := json.Unmarshal(fields["reply_markup"], &markup); err != nil || !markup.ForceReply || !markup.Selective || markup.Placeholder != "Next task" || markup.Inline != nil {
					t.Errorf("invalid ForceReply markup: %s err=%v", fields["reply_markup"], err)
				}
				_, hasReply := fields["reply_parameters"]
				if hasReply != !(fallback == "reply target" && attempt == 2) {
					t.Errorf("unexpected reply fallback fields: %s", fields)
				}
				if hasReply {
					var reply struct {
						MessageID int64 `json:"message_id"`
					}
					if err := json.Unmarshal(fields["reply_parameters"], &reply); err != nil || reply.MessageID != 41 {
						t.Error("prompt lost its intended reply target")
					}
				}
				if attempt == 1 && fallback != "none" {
					description := "Bad Request: reply message not found"
					if fallback == "HTML" {
						description = "Bad Request: can't parse entities"
					}
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprintf(w, `{"ok":false,"error_code":400,"description":%q}`, description)
					return
				}
				if fallback == "HTML" && attempt == 2 {
					if _, ok := fields["parse_mode"]; ok {
						t.Error("HTML rejection must retry plain text")
					}
				}
				fmt.Fprint(w, `{"ok":true,"result":{"message_id":42}}`)
			})
			message, err := client.Send(context.Background(), -10, 8, "Reply with **your task**", SendOptions{
				ReplyToMessageID: 41,
				ForceReply:       &ForceReply{ForceReply: true, Selective: true, InputFieldPlaceholder: "Next task"},
			})
			wantAttempts := int32(1)
			if fallback != "none" {
				wantAttempts = 2
			}
			if err != nil || message.MessageID != 42 || attempts.Load() != wantAttempts {
				t.Fatalf("ForceReply send=%+v err=%v attempts=%d", message, err, attempts.Load())
			}
		})
	}
}

func TestSendRejectsConflictingReplyMarkupBeforeHTTP(t *testing.T) {
	var attempts atomic.Int32
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":42}}`)
	})
	keyboard := &Keyboard{InlineKeyboard: [][]Button{{{Text: "Action", CallbackData: "opaque"}}}}
	force := &ForceReply{ForceReply: true}
	remove := &ReplyKeyboardRemove{RemoveKeyboard: true}
	for _, options := range []SendOptions{
		{Keyboard: keyboard, ForceReply: force},
		{Keyboard: keyboard, RemoveKeyboard: remove},
		{ForceReply: force, RemoveKeyboard: remove},
	} {
		message, err := client.Send(context.Background(), -10, 8, "prompt", options)
		if err == nil || DeliveryUncertain(err) || message.MessageID != 0 || attempts.Load() != 0 {
			t.Fatalf("conflicting markup send=%+v err=%v requests=%d", message, err, attempts.Load())
		}
	}
}

func TestEditKeyboardOnlyChangesMarkup(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprint(remove), func(t *testing.T) {
			var attempts atomic.Int32
			client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				var fields map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
					t.Error(err)
					return
				}
				if r.URL.Path != "/bot123:secret-token/editMessageReplyMarkup" || string(fields["chat_id"]) != "-10" || string(fields["message_id"]) != "42" || len(fields) != 3 {
					t.Errorf("markup edit must not resend body or change routing: %s %s", r.URL.Path, fields)
				}
				var keyboard Keyboard
				if err := json.Unmarshal(fields["reply_markup"], &keyboard); err != nil {
					t.Error(err)
				} else if remove {
					if keyboard.InlineKeyboard == nil || len(keyboard.InlineKeyboard) != 0 {
						t.Error("nil keyboard must explicitly clear markup")
					}
				} else if len(keyboard.InlineKeyboard) != 1 || len(keyboard.InlineKeyboard[0]) != 1 || keyboard.InlineKeyboard[0][0].CallbackData != "opaque" {
					t.Error("button payload not applied")
				}
				fmt.Fprint(w, `{"ok":true,"result":{"message_id":42}}`)
			})
			var keyboard *Keyboard
			if !remove {
				keyboard = &Keyboard{InlineKeyboard: [][]Button{{{Text: "Action", CallbackData: "opaque"}}}}
			}
			if err := client.EditKeyboard(context.Background(), -10, 42, keyboard); err != nil || attempts.Load() != 1 {
				t.Fatalf("markup edit err=%v requests=%d", err, attempts.Load())
			}
		})
	}
}

func TestEditKeyboardErrorPolicy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        int
		description string
		wantSuccess bool
	}{
		{"not modified", 400, "Bad Request: message is not modified", true},
		{"missing message", 400, "Bad Request: message to edit not found", false},
		{"rate limit", 429, "Too Many Requests", false},
		{"server failure", 500, "Internal Server Error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(tc.code)
				fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":%q,"parameters":{"retry_after":1}}`, tc.code, tc.description)
			})
			err := client.EditKeyboard(context.Background(), -10, 42, nil)
			if attempts.Load() != 1 || (err == nil) != tc.wantSuccess {
				t.Fatalf("markup edit err=%v requests=%d", err, attempts.Load())
			}
			if !tc.wantSuccess {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Code != tc.code || DeliveryUncertain(err) {
					t.Fatalf("markup error classification lost: %v", err)
				}
			}
		})
	}
}
