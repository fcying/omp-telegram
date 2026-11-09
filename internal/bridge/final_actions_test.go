package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/i18n"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func configureFinalActionFixture(w *worker) {
	w.b.cfg.FinalActions = []config.FinalAction{{Label: "Continue", Command: "/followup next task"}, {Label: "Queue", Command: "/queue"}}
	w.b.cfg.AllowedUsers = []int64{7}
	w.b.cfg.AllowedChats = []int64{-10}
	w.b.cfg.StatusBar = config.StatusBar{Progress: [][]string{}, Intermediate: [][]string{}, Final: [][]string{}}
}

func readFinalActionFixture(t *testing.T, w *worker) (int64, []store.FinalAction) {
	t.Helper()
	var messageID int64
	var encoded string
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT message_id,final_actions FROM outbox WHERE final_actions<>'' ORDER BY id DESC LIMIT 1").Scan(&messageID, &encoded))
	var actions []store.FinalAction
	requireStoreOK(t, json.Unmarshal([]byte(encoded), &actions))
	return messageID, actions
}

func finalActionIncoming(t *testing.T, w *worker, id, messageID int64, token string) incoming {
	t.Helper()
	q := &telegram.CallbackQuery{ID: "callback-" + strconv.FormatInt(id, 10), From: telegram.User{ID: 7}, Data: finalActionPrefix + token, Message: &telegram.Message{MessageID: messageID, MessageThreadID: w.key.thread, From: &w.b.bot, Chat: telegram.Chat{ID: w.key.chat}}}
	raw, err := json.Marshal(telegram.Update{UpdateID: id, CallbackQuery: q})
	requireStoreOK(t, err)
	requireStoreOK(t, w.b.db.Accept(id, raw))
	return incoming{id: id, callback: q}
}

func TestFinalActionsSuccessUsesLastHeaderlessBodySplit(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	configureFinalActionFixture(w)
	text := strings.Repeat("x", telegram.MaxMessageUTF16) + " tail"
	if !w.queueAssistantText(text, false) {
		t.Fatal("could not queue assistant body")
	}
	deliverReplies(t, w)
	f.mu.Lock()
	before := len(f.messages)
	hasEarlyMarkup := false
	for _, message := range f.messages {
		if message["reply_markup"] != nil {
			hasEarlyMarkup = true
		}
	}
	f.mu.Unlock()
	if hasEarlyMarkup {
		t.Fatal("favorites appeared before root success")
	}
	if before != 2 {
		t.Fatalf("fixture did not split real body: %d", before)
	}
	w.finish()
	requireStoreOK(t, w.b.deliverFinalActions(context.Background()))
	messageID, actions := readFinalActionFixture(t, w)
	if messageID != 2 || len(actions) != 2 {
		t.Fatalf("tail metadata = %d/%+v", messageID, actions)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) != before || len(f.keyboardClears) != 1 || f.keyboardClears[0]["message_id"] != float64(2) {
		t.Fatalf("success resent body or edited header target: messages=%+v edits=%+v", f.messages, f.keyboardClears)
	}
	keyboard := f.keyboardClears[0]["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	if len(keyboard) != 1 || len(keyboard[0].([]any)) != 2 {
		t.Fatalf("configured action row changed: %+v", keyboard)
	}
}

func TestFinalActionsNeverTargetTruncationNotice(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	configureFinalActionFixture(w)
	w.assistantDelivery.queuedParts = maxFinalReplyMessages - 3
	w.stream.WriteString(strings.Repeat("x", telegram.MaxMessageUTF16*2))
	w.finish()
	rows, err := w.b.db.DB.Query("SELECT text,assistant_body,final_actions FROM outbox WHERE inbox_id=2 ORDER BY id")
	requireStoreOK(t, err)
	defer rows.Close()
	var bodies, notices, buttons int
	for rows.Next() {
		var text, encoded string
		var body bool
		requireStoreOK(t, rows.Scan(&text, &body, &encoded))
		if body {
			bodies++
		}
		if text == w.tr(i18n.Msg.ReplyTruncated) {
			notices++
			if body || encoded != "" {
				t.Fatal("truncation notice owns favorites")
			}
		}
		if encoded != "" {
			buttons++
			if !body {
				t.Fatal("favorites not on real body")
			}
		}
	}
	requireStoreOK(t, rows.Err())
	if bodies != 1 || notices != 1 || buttons != 1 {
		t.Fatalf("bounded body/notice/action counts = %d/%d/%d", bodies, notices, buttons)
	}
}

func TestFinalActionsAbsentOnIncompleteAndNativeResults(t *testing.T) {
	for _, terminal := range []string{"uncertain", "cancelled", "native", "empty"} {
		t.Run(terminal, func(t *testing.T) {
			w, _, _, _ := replyStatusWorker(t)
			configureFinalActionFixture(w)
			if terminal != "empty" {
				w.stream.WriteString("body")
			}
			switch terminal {
			case "uncertain":
				w.finishUncertain("error notice")
			case "cancelled":
				w.finishCancelled("cancel notice")
			case "native":
				w.activeInputKind = queuedNativeCommand
				w.finish()
			case "empty":
				w.finish()
			}
			var count int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM final_actions").Scan(&count))
			if count != 0 {
				t.Fatalf("%s result issued favorites", terminal)
			}
		})
	}
}

func TestFinalActionClickKeepsRealInboxUserAndOneShotQueue(t *testing.T) {
	w, _, _, _ := replyStatusWorker(t)
	configureFinalActionFixture(w)
	w.stream.WriteString("done")
	w.finish()
	deliverReplies(t, w)
	messageID, actions := readFinalActionFixture(t, w)
	in := finalActionIncoming(t, w, 40, messageID, actions[0].Token)
	if !w.handleFinalAction(in) {
		t.Fatal("favorite callback fell through")
	}
	if len(w.queue) != 1 || w.queue[0].id != 40 || w.queue[0].user != 7 || w.queue[0].text != "next task" || w.queue[0].kind != queuedPrompt {
		t.Fatalf("shortcut fabricated identity or bypassed queue: %+v", w.queue)
	}
	if queueState(t, w, 40) != "pending" {
		t.Fatal("shortcut submitted queued prompt early")
	}
	// Duplicate delivery of the original update must not invalidate its queued command.
	w.handleFinalAction(in)
	if len(w.queue) != 1 || queueState(t, w, 40) != "pending" {
		t.Fatal("duplicate update replayed or cancelled accepted queue entry")
	}
	w.handleFinalAction(finalActionIncoming(t, w, 41, messageID, actions[0].Token))
	if len(w.queue) != 1 || queueState(t, w, 41) != "done" {
		t.Fatal("double click replayed action")
	}
	w.handleFinalAction(finalActionIncoming(t, w, 42, messageID, actions[1].Token))
	if queueState(t, w, 42) != "done" {
		t.Fatal("independent button was consumed with first button")
	}
}

func TestFinalActionRejectsChangedIdentityAndConfiguration(t *testing.T) {
	for _, mismatch := range []string{"sender", "bot", "chat", "topic", "message", "config", "generation", "unknown"} {
		t.Run(mismatch, func(t *testing.T) {
			w, _, _, _ := replyStatusWorker(t)
			configureFinalActionFixture(w)
			w.stream.WriteString("done")
			w.finish()
			deliverReplies(t, w)
			messageID, actions := readFinalActionFixture(t, w)
			in := finalActionIncoming(t, w, 40, messageID, actions[0].Token)
			switch mismatch {
			case "sender":
				in.callback.From.ID++
			case "bot":
				bot := *in.callback.Message.From
				bot.ID++
				in.callback.Message.From = &bot
			case "chat":
				in.callback.Message.Chat.ID++
			case "topic":
				in.callback.Message.MessageThreadID++
			case "message":
				in.callback.Message.MessageID++
			case "config":
				w.b.cfg.FinalActions[0].Command = "/followup changed"
			case "generation":
				w.binding.Generation++
			case "unknown":
				in.callback.Data = finalActionPrefix + "unknown"
			}
			if !w.handleFinalAction(in) || len(w.queue) != 0 || queueState(t, w, 40) != "done" {
				t.Fatal("stale or forged action escaped rejection")
			}
		})
	}
}

func TestFinalActionsMarkupFailureNeverResendsBody(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejection", true: "uncertain"}[uncertain], func(t *testing.T) {
			w, f, _, _ := replyStatusWorker(t)
			configureFinalActionFixture(w)
			if !w.queueAssistantText("answer", false) {
				t.Fatal("could not queue body")
			}
			deliverReplies(t, w)
			w.finish()
			f.mu.Lock()
			before := len(f.messages)
			f.failKeyboardClear = !uncertain
			f.mu.Unlock()
			if uncertain {
				http.DefaultTransport = logTestTransport(func(request *http.Request) (*http.Response, error) {
					if filepath.Base(request.URL.Path) == "editMessageReplyMarkup" {
						return nil, errors.New("response lost")
					}
					return f.RoundTrip(request)
				})
			}
			requireStoreOK(t, w.b.deliverFinalActions(context.Background()))
			requireStoreOK(t, w.b.deliverFinalActions(context.Background()))
			if _, err := w.b.db.NextOutput(); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("markup error scheduled body replay: %v", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.messages) != before {
				t.Fatal("markup failure resent assistant body")
			}
		})
	}
}

func TestLateAssistantHeaderEditPreservesFavorites(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	configureFinalActionFixture(w)
	w.stream.WriteString("answer")
	w.finish()
	deliverReplies(t, w)
	var target int64
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT id FROM outbox WHERE inbox_id=2 AND header_editable=1").Scan(&target))
	requireStoreOK(t, w.b.db.SetAssistantHeader(context.Background(), target, "late quota"))
	requireStoreOK(t, w.b.deliverAssistantHeader(context.Background()))
	f.mu.Lock()
	defer f.mu.Unlock()
	last := f.messages[len(f.messages)-1]
	if last["message_id"] == nil || last["reply_markup"] == nil || !strings.Contains(last["text"].(string), "answer") {
		t.Fatalf("late quota edit erased keyboard or body: %+v", last)
	}
}

func TestAssistantMetadataEditsSerializeUntilResponse(t *testing.T) {
	for _, first := range []string{"header", "markup"} {
		t.Run(first, func(t *testing.T) {
			w, f, _, _ := replyStatusWorker(t)
			configureFinalActionFixture(w)
			w.stream.WriteString("answer")
			w.finish()
			deliverReplies(t, w)
			messageID, actions := readFinalActionFixture(t, w)
			var target int64
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT id FROM outbox WHERE inbox_id=2 AND header_editable=1").Scan(&target))
			requireStoreOK(t, w.b.db.SetAssistantHeader(context.Background(), target, "late quota"))
			consume := func() {
				in := finalActionIncoming(t, w, 40, messageID, actions[0].Token)
				_, claimed, err := w.b.db.ReserveFinalAction(context.Background(), store.FinalActionClaim{
					Token: actions[0].Token, Config: finalActionsConfigKey(w.b.cfg.FinalActions), InboxID: in.id,
					Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread, User: 7, MessageID: messageID, Binding: w.binding,
				})
				requireStoreOK(t, err)
				if !claimed {
					t.Fatal("fixture could not consume first button")
				}
				requireStoreOK(t, w.b.db.ConsumeFinalAction(context.Background(), in.id))
			}
			method := "editMessageText"
			if first == "markup" {
				consume()
				method = "editMessageReplyMarkup"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			applied := make(chan struct{}, 1)
			release := make(chan struct{}, 1)
			defer close(release)
			http.DefaultTransport = logTestTransport(func(request *http.Request) (*http.Response, error) {
				response, err := f.RoundTrip(request)
				if filepath.Base(request.URL.Path) == method {
					select {
					case applied <- struct{}{}:
					default:
					}
					select {
					case <-release:
					case <-request.Context().Done():
						return nil, request.Context().Err()
					}
				}
				return response, err
			})
			done := make(chan error, 1)
			go func() {
				if first == "header" {
					done <- w.b.deliverAssistantHeader(ctx)
				} else {
					done <- w.b.deliverFinalActions(ctx)
				}
			}()
			select {
			case <-applied:
			case <-ctx.Done():
				t.Fatal("first metadata edit was not applied")
			}
			if first == "header" {
				consume()
				requireStoreOK(t, w.b.deliverFinalActions(ctx))
			} else {
				requireStoreOK(t, w.b.deliverAssistantHeader(ctx))
			}
			f.mu.Lock()
			headerCalls, markupCalls := len(f.messages)-1, len(f.keyboardClears)
			f.mu.Unlock()
			if headerCalls+markupCalls != 1 {
				t.Fatalf("another edit reached Telegram before the first response: header=%d markup=%d", headerCalls, markupCalls)
			}
			release <- struct{}{}
			requireStoreOK(t, <-done)
			if first == "header" {
				requireStoreOK(t, w.b.deliverFinalActions(ctx))
			} else {
				requireStoreOK(t, w.b.deliverAssistantHeader(ctx))
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			var last map[string]any
			if first == "header" {
				last = f.keyboardClears[len(f.keyboardClears)-1]
			} else {
				last = f.messages[len(f.messages)-1]
			}
			row := last["reply_markup"].(map[string]any)["inline_keyboard"].([]any)[0].([]any)
			if len(row) != 1 || row[0].(map[string]any)["callback_data"] != finalActionPrefix+actions[1].Token {
				t.Fatalf("last remote edit restored consumed button: %+v", last)
			}
		})
	}
}

func TestFinalActionsWrapRowsWithoutChangingActionOrder(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		labels []string
		widths []int
	}{
		{"six short labels", []string{"One", "Two", "Three", "Four", "五", "六"}, []int{3, 3}},
		{"long label", []string{"Short", strings.Repeat("界", 25), "Tail"}, []int{1, 1, 1}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actions := make([]store.FinalAction, 0, len(scenario.labels))
			for i, label := range scenario.labels {
				actions = append(actions, store.FinalAction{Token: string(rune('a' + i)), Label: label, Command: "/queue"})
			}
			encoded, err := json.Marshal(actions)
			requireStoreOK(t, err)
			keyboard := finalActionKeyboard(string(encoded))
			if len(keyboard.InlineKeyboard) != len(scenario.widths) {
				t.Fatalf("row count = %d, want %v", len(keyboard.InlineKeyboard), scenario.widths)
			}
			position := 0
			for i, row := range keyboard.InlineKeyboard {
				if len(row) != scenario.widths[i] {
					t.Fatalf("row %d has %d buttons, want %d", i, len(row), scenario.widths[i])
				}
				for _, button := range row {
					if button.Text != actions[position].Label || button.CallbackData != finalActionPrefix+actions[position].Token {
						t.Fatalf("wrapping changed action %d: %+v", position, button)
					}
					position++
				}
			}
		})
	}
}

func TestFinalActionsCancelledDeliveryKeepsPendingMarkupForNextConsumer(t *testing.T) {
	w, f, _, _ := replyStatusWorker(t)
	configureFinalActionFixture(w)
	if !w.queueAssistantText("answer", false) {
		t.Fatal("could not queue body")
	}
	deliverReplies(t, w)
	w.finish()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	requireStoreOK(t, w.b.deliverFinalActions(ctx))
	requireStoreOK(t, w.b.deliverFinalActions(context.Background()))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) != 1 || len(f.keyboardClears) != 1 {
		t.Fatalf("cancelled consumer lost the markup or resent body: sends=%d edits=%d", len(f.messages), len(f.keyboardClears))
	}
	markup := f.keyboardClears[0]["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	if len(markup) != 1 || len(markup[0].([]any)) != 2 {
		t.Fatalf("next consumer did not publish the pending action row: %+v", markup)
	}
}

func TestFinalActionCommandsKeepConfirmationAndBusyRestrictions(t *testing.T) {
	for _, scenario := range []string{"new", "compact-idle", "compact-busy"} {
		t.Run(scenario, func(t *testing.T) {
			w, _, command, _ := replyStatusWorker(t)
			configureFinalActionFixture(w)
			invocation := "/compact"
			if scenario == "new" {
				invocation = "/new"
			}
			w.b.cfg.FinalActions = []config.FinalAction{{Label: "Action", Command: invocation}}
			if !w.queueAssistantText("answer", false) {
				t.Fatal("could not queue answer")
			}
			w.finish()
			deliverReplies(t, w)
			messageID, actions := readFinalActionFixture(t, w)
			before, client := w.binding, w.client
			if scenario == "compact-busy" {
				command("missing-terminal")
				w.dispatch()
			}
			active := w.active
			w.handle(finalActionIncoming(t, w, 40, messageID, actions[0].Token))
			if !sameBindingIdentity(w.binding, before) || w.client != client || queueState(t, w, 40) != "done" {
				t.Fatal("shortcut changed session identity or callback lifecycle")
			}
			switch scenario {
			case "new":
				confirmed := false
				for _, pending := range w.confirms {
					if pending.action == "new" && pending.user == 7 && pending.messageID > 0 {
						confirmed = true
					}
				}
				if !confirmed || w.sessionOp != sessionOperationNone {
					t.Fatal("New shortcut bypassed the existing confirmation")
				}
			case "compact-busy":
				if active == 0 || w.active != active || !w.taskRunning() || w.sessionOp != sessionOperationNone {
					t.Fatal("Compact shortcut bypassed the active-task restriction")
				}
			case "compact-idle":
				if w.sessionOp != sessionOperationCompact {
					t.Fatal("idle Compact shortcut did not invoke the existing operation")
				}
				result := waitOperation(t, w)
				requireStoreOK(t, result.err)
				if result.kind != "compact" {
					t.Fatalf("shortcut invoked another operation: %s", result.kind)
				}
				w.operationReturned(result)
				if w.sessionOp != sessionOperationNone || w.client != client || !sameBindingIdentity(w.binding, before) {
					t.Fatal("completed Compact shortcut changed the session")
				}
			}
		})
	}
}

func favoriteCommandFixture(t *testing.T, invocation string) (*worker, int64, string) {
	t.Helper()
	w, _, _, _ := replyStatusWorker(t)
	configureFinalActionFixture(w)
	w.b.cfg.FinalActions = []config.FinalAction{{Label: "Action", Command: invocation}}
	w.stream.WriteString("answer")
	w.finish()
	deliverReplies(t, w)
	messageID, actions := readFinalActionFixture(t, w)
	return w, messageID, actions[0].Token
}

func TestFinalActionBusyCompactCanRetryButAcceptedCompactCannot(t *testing.T) {
	w, messageID, token := favoriteCommandFixture(t, "/compact")
	w.busy = true
	w.handleFinalAction(finalActionIncoming(t, w, 40, messageID, token))
	if w.sessionOp != sessionOperationNone {
		t.Fatal("busy Compact started an operation")
	}
	w.busy = false
	w.handleFinalAction(finalActionIncoming(t, w, 41, messageID, token))
	if w.sessionOp != sessionOperationCompact {
		t.Fatal("determinate busy rejection burned the favorite")
	}
	result := waitOperation(t, w)
	if result.kind != "compact" {
		t.Fatalf("retry dispatched %q instead of Compact", result.kind)
	}
	w.operationReturned(result)
	w.handleFinalAction(finalActionIncoming(t, w, 42, messageID, token))
	if w.sessionOp != sessionOperationNone {
		t.Fatal("accepted Compact could be repeated")
	}
}

func TestFinalActionRejectedArgumentsAndConfigurationRemainRetryable(t *testing.T) {
	for _, command := range []string{"/compact unexpected", "/model invalid", "/followup", "/export html first second"} {
		t.Run(command, func(t *testing.T) {
			w, messageID, token := favoriteCommandFixture(t, command)
			w.handleFinalAction(finalActionIncoming(t, w, 40, messageID, token))
			_, reserved, err := w.b.db.ReserveFinalAction(context.Background(), store.FinalActionClaim{
				Token: token, Config: finalActionsConfigKey(w.b.cfg.FinalActions), InboxID: finalActionIncoming(t, w, 41, messageID, token).id,
				Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread, User: 7, MessageID: messageID, Binding: w.binding,
			})
			requireStoreOK(t, err)
			if !reserved {
				t.Fatal("argument rejection consumed the favorite")
			}
		})
	}
	w, messageID, token := favoriteCommandFixture(t, "/followup retry after config restore")
	original := w.b.cfg.FinalActions[0]
	w.b.cfg.FinalActions[0].Command = "/queue"
	w.handleFinalAction(finalActionIncoming(t, w, 40, messageID, token))
	w.b.cfg.FinalActions[0] = original
	w.handleFinalAction(finalActionIncoming(t, w, 41, messageID, token))
	if len(w.queue) != 1 || w.queue[0].id != 41 {
		t.Fatal("configuration rejection burned a still-valid original favorite")
	}
}

func TestFinalActionNewCancellationReleasesOnlyForNewClick(t *testing.T) {
	w, messageID, token := favoriteCommandFixture(t, "/new")
	in := finalActionIncoming(t, w, 40, messageID, token)
	w.handleFinalAction(in)
	w.handleFinalAction(finalActionIncoming(t, w, 41, messageID, token))
	var confirmationToken string
	var pending confirmation
	for key, c := range w.confirms {
		if c.action == "new" {
			if confirmationToken != "" {
				t.Fatal("double click created duplicate execution confirmations")
			}
			confirmationToken, pending = key, c
		}
	}
	if confirmationToken == "" {
		t.Fatal("favorite did not open New confirmation")
	}
	w.callback(&telegram.CallbackQuery{ID: "cancel-new", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: pending.messageID}, Data: confirmationToken + ":1"})
	w.handleFinalAction(in)
	if len(w.confirms) != 0 {
		t.Fatal("same update replayed a cancelled confirmation")
	}
	w.handleFinalAction(finalActionIncoming(t, w, 43, messageID, token))
	found := false
	for _, c := range w.confirms {
		found = found || c.action == "new"
	}
	if !found {
		t.Fatal("cancelled New could not be clicked again")
	}
}

func TestFinalActionAsyncUnknownNativeCanRetryAfterCatalogChange(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_NATIVE_COMMANDS", "hold")
	w, messageID, token := favoriteCommandFixture(t, "/notadvertised")
	w.handleFinalAction(finalActionIncoming(t, w, 40, messageID, token))
	if len(w.queue) != 1 || w.queue[0].kind != queuedSlashPending {
		t.Fatal("favorite skipped asynchronous native qualification")
	}
	w.handleFinalAction(finalActionIncoming(t, w, 41, messageID, token))
	if len(w.queue) != 1 {
		t.Fatal("reservation allowed concurrent discovery clicks")
	}
	nativeRPC(t, w, "fixture_catalog_update", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 40) == "cancelled" })
	nativeRPC(t, w, "fixture_catalog_update", map[string]any{"addName": "notadvertised", "sourceName": "notadvertised", "source": "file"})
	w.commandCatalogUpdated()
	w.handleFinalAction(finalActionIncoming(t, w, 42, messageID, token))
	if len(w.queue) != 1 || w.queue[0].id != 42 || w.queue[0].kind != queuedNativeCommand {
		t.Fatal("determinate unknown-command rejection burned the favorite")
	}
	w.handleFinalAction(finalActionIncoming(t, w, 43, messageID, token))
	if len(w.queue) != 1 {
		t.Fatal("accepted native command was repeatable")
	}
}

func TestFinalActionFailedConfirmationAndAuthorizationCanRetry(t *testing.T) {
	for _, failure := range []string{"confirmation-send", "authorization"} {
		t.Run(failure, func(t *testing.T) {
			w, messageID, token := favoriteCommandFixture(t, "/new")
			fake := http.DefaultTransport.(*fakeHTTP)
			if failure == "confirmation-send" {
				fake.mu.Lock()
				fake.failProgress = true
				fake.mu.Unlock()
			} else {
				w.b.cfg.AllowedUsers = []int64{8}
			}
			w.handleFinalAction(finalActionIncoming(t, w, 40, messageID, token))
			if len(w.confirms) != 0 {
				t.Fatal("rejected click retained an executable confirmation")
			}
			fake.mu.Lock()
			fake.failProgress = false
			fake.mu.Unlock()
			w.b.cfg.AllowedUsers = []int64{7}
			w.handleFinalAction(finalActionIncoming(t, w, 41, messageID, token))
			found := false
			for _, c := range w.confirms {
				found = found || c.action == "new"
			}
			if !found {
				t.Fatal("pre-execution rejection burned the favorite")
			}
		})
	}
}

func TestFinalActionMissingSourceCannotExecuteConfirmation(t *testing.T) {
	w, messageID, token := favoriteCommandFixture(t, "/new")
	w.handleFinalAction(finalActionIncoming(t, w, 40, messageID, token))
	var key string
	var pending confirmation
	for candidate, c := range w.confirms {
		if c.action == "new" {
			key, pending = candidate, c
		}
	}
	if key == "" {
		t.Fatal("favorite did not open execution confirmation")
	}
	before, client := w.binding, w.client
	_, err := w.b.db.DB.Exec("DELETE FROM outbox WHERE id=(SELECT outbox_id FROM final_actions WHERE token=?)", token)
	requireStoreOK(t, err)
	w.callback(&telegram.CallbackQuery{ID: "confirm-deleted-source", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: pending.messageID}, Data: key + ":0"})
	if !sameBindingIdentity(w.binding, before) || w.client != client || w.startIntent != nil || len(w.confirms) != 0 {
		t.Fatal("confirmation with a missing reservation changed or restarted the session")
	}
	var starts int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM startup_intents").Scan(&starts))
	if starts != 0 {
		t.Fatal("missing reservation left a startup intent")
	}
}

func TestFinalActionMissingReservationDoesNotWedgeSessionControls(t *testing.T) {
	for _, command := range []string{"/compact", "/handoff"} {
		t.Run(command, func(t *testing.T) {
			w, messageID, token := favoriteCommandFixture(t, command)
			in := finalActionIncoming(t, w, 40, messageID, token)
			_, reserved, err := w.b.db.ReserveFinalAction(context.Background(), store.FinalActionClaim{
				Token: token, Config: finalActionsConfigKey(w.b.cfg.FinalActions), InboxID: in.id,
				Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread, User: 7, MessageID: messageID, Binding: w.binding,
			})
			requireStoreOK(t, err)
			if !reserved {
				t.Fatal("could not reserve favorite")
			}
			_, err = w.b.db.DB.Exec("DELETE FROM outbox WHERE id=(SELECT outbox_id FROM final_actions WHERE token=?)", token)
			requireStoreOK(t, err)
			func() {
				defer w.withFinalAction(&finalActionDispatch{inboxID: in.id})()
				w.dispatchShortcut(in, command)
			}()
			if w.sessionOp != sessionOperationNone || w.controlInProgress() || w.rpcOperationActive {
				t.Fatal("missing reservation left an unsubmitted control operation active")
			}
		})
	}
}

func TestFinalActionArgsPreflightRejectionAndCancellationCanRetry(t *testing.T) {
	for _, failure := range []string{"pending", "query-failed", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			w, messageID, token := favoriteCommandFixture(t, "/args 2")
			configureTestArgPresets(w.b, [][]string{nil, nil}, strings.Repeat("a", 64), 1)
			persistTestSelection(t, w.b, 1)
			client, before := w.client, w.binding
			w.handleFinalAction(finalActionIncoming(t, w, 40, messageID, token))
			if w.argsApply == nil {
				t.Fatal("favorite did not enter native argument qualification")
			}
			result := waitOperation(t, w)
			if result.kind != "args_state" {
				t.Fatalf("favorite qualified through %q instead of args_state", result.kind)
			}
			switch failure {
			case "pending":
				var state map[string]any
				requireStoreOK(t, json.Unmarshal(result.data, &state))
				state["hasPendingAsyncWork"], state["isSettled"] = true, false
				result.data, _ = json.Marshal(state)
			case "query-failed":
				result.err = context.DeadlineExceeded
			case "cancelled":
				w.cancelArgsApply(false)
			}
			w.operationReturned(result)
			requireArgsSelection(t, w.b, 1)
			if w.client != client || !sameBindingIdentity(w.binding, before) || w.controlInProgress() {
				t.Fatal("rejected argument qualification changed or blocked the session")
			}
			w.handleFinalAction(finalActionIncoming(t, w, 41, messageID, token))
			if w.argsApply == nil {
				t.Fatal("preflight rejection burned the argument favorite")
			}
			drainControlOperations(t, w)
			requireArgsSelection(t, w.b, 2)
			w.handleFinalAction(finalActionIncoming(t, w, 42, messageID, token))
			if w.argsApply != nil || w.controlInProgress() {
				t.Fatal("committed argument preset favorite was repeatable")
			}
		})
	}
}

func TestFinalActionDroppedQueuedPreflightReleasesReservationAndRetention(t *testing.T) {
	w, messageID, token := favoriteCommandFixture(t, "/model fixture/retry-safe")
	unblock := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	})
	w.startOperation("reply_usage", w.client, func(ctx context.Context) (json.RawMessage, error) {
		select {
		case <-unblock:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, 0, "")
	w.handleFinalAction(finalActionIncoming(t, w, 40, messageID, token))
	if len(w.rpcOperations) != 1 || w.rpcOperations[0].kind != "model_state" {
		t.Fatal("favorite preflight was not queued behind the active usage lane")
	}
	w.releaseRuntimeWithReason(true, "test-drop-before-execution")
	close(unblock)
	w.operationReturned(waitOperation(t, w))
	if len(w.rpcOperations) != 0 || w.controlInProgress() {
		t.Fatal("dropped qualification retained command ownership")
	}
	in := finalActionIncoming(t, w, 41, messageID, token)
	_, reserved, err := w.b.db.ReserveFinalAction(context.Background(), store.FinalActionClaim{
		Token: token, Config: finalActionsConfigKey(w.b.cfg.FinalActions), InboxID: in.id,
		Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread, User: 7, MessageID: messageID, Binding: w.binding,
	})
	requireStoreOK(t, err)
	if !reserved {
		t.Fatal("dropped unexecuted qualification permanently reserved the favorite")
	}
	requireStoreOK(t, w.b.db.ReleaseFinalAction(context.Background(), in.id))
	requireStoreOK(t, w.b.deliverFinalActions(context.Background()))
	old := time.Now().AddDate(0, 0, -120).Unix()
	_, err = w.b.db.DB.Exec("UPDATE outbox SET created_at=?,updated_at=? WHERE id=(SELECT outbox_id FROM final_actions WHERE token=?)", old, old, token)
	requireStoreOK(t, err)
	cleaned, err := w.b.db.CleanupMessages(context.Background(), time.Now().AddDate(0, 0, -90).Unix())
	requireStoreOK(t, err)
	if cleaned.Outbox != 1 {
		t.Fatalf("dropped qualification permanently retained its source: %+v", cleaned)
	}
}
