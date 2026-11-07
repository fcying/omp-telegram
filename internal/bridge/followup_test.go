package bridge

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func persistTestFollowup(t *testing.T, w *worker, token string) store.FollowupRequest {
	t.Helper()
	if w.binding.Session == "" {
		w.binding = store.Binding{Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread, Workspace: t.TempDir(), Session: filepath.Join(t.TempDir(), "session.jsonl"), Generation: 1, Running: true}
		requireStoreOK(t, w.b.db.Save(w.binding))
	}
	r := store.FollowupRequest{Token: token, Instance: w.b.quickActionInstance(), Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread, User: 7, TaskID: 2, Workspace: w.binding.Workspace, Session: w.binding.Session, Generation: w.binding.Generation, ExpiresAt: time.Now().Add(followupLifetime).Unix()}
	requireStoreOK(t, w.b.db.CreateFollowupRequest(r))
	requireStoreOK(t, w.b.db.FinishFollowupPrompt(r.Token, r.Instance, 900, true))
	r, err := w.b.db.FollowupRequest(r.Token)
	requireStoreOK(t, err)
	return r
}

func followupTestIncoming(t *testing.T, w *worker, r store.FollowupRequest, id int64, text string) incoming {
	t.Helper()
	msg := &telegram.Message{MessageID: id, MessageThreadID: w.key.thread, Chat: telegram.Chat{ID: w.key.chat}, From: &telegram.User{ID: r.User}, Text: text,
		ReplyToMessage: &telegram.Message{MessageID: r.PromptID, Chat: telegram.Chat{ID: r.Chat}, From: &telegram.User{ID: r.Bot, IsBot: true}, Text: "Bridge input prompt\n" + followupMarker(r.Token)}, Quote: &telegram.TextQuote{Text: "Bridge input prompt"}}
	raw, err := json.Marshal(telegram.Update{UpdateID: id, Message: msg})
	requireStoreOK(t, err)
	requireStoreOK(t, w.b.db.Accept(id, raw))
	return incoming{id: id, msg: msg}
}

func TestFollowupQueuesWithoutSteeringAndSurvivesNaturalSettlement(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	queueReady(t, w, command)
	r := persistTestFollowup(t, w, "natural-completion")
	// The originating root/turn is deliberately no longer current.
	w.turn += 2
	in := followupTestIncoming(t, w, r, 100, "  independent next task  ")
	replay := in
	if !w.routeFollowupInput(&in) || len(w.queue) != 1 || len(w.steers) != 0 {
		t.Fatalf("reply not independently queued: queue=%+v steers=%+v", w.queue, w.steers)
	}
	q := w.queue[0]
	if q.text != "independent next task" || q.replyTo != 100 || q.user != 7 || in.msg.ReplyToMessage != nil || in.msg.Quote != nil {
		t.Fatalf("prompt quote leaked into queue: %+v", q)
	}
	if !w.routeFollowupInput(&replay) || len(w.queue) != 1 {
		t.Fatal("duplicate inbox admission appended twice")
	}
	var state string
	requireStoreOK(t, w.b.db.DB.QueryRow(`SELECT state FROM inbox WHERE id=100`).Scan(&state))
	if state != "pending" {
		t.Fatalf("queued duplicate changed inbox state: %s", state)
	}
	got, err := w.b.db.FollowupRequest(r.Token)
	requireStoreOK(t, err)
	if got.State != "consumed" || got.InboxID != in.id {
		t.Fatalf("accepted inbox not durably claimed: %+v", got)
	}
}

func TestFollowupActiveTaskReplyIsIndependentQueue(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	queueReady(t, w, command)
	command("missing-terminal")
	w.dispatch()
	r := persistTestFollowup(t, w, "active-root")
	root, turn := w.active, w.turn
	in := followupTestIncoming(t, w, r, 100, "after this task")
	w.handle(in)
	if len(w.queue) != 1 || len(w.steers) != 0 || w.active != root || w.turn != turn || !w.taskRunning() {
		t.Fatalf("follow-up interrupted or steered root: queue=%+v steers=%+v", w.queue, w.steers)
	}
}

func TestFollowupEmptyAttachmentAndFullQueueDoNotConsume(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 2
	queueReady(t, w, command)
	r := persistTestFollowup(t, w, "retry-input")
	for n, reason := range []string{"empty", "attachment", "full"} {
		in := followupTestIncoming(t, w, r, int64(100+n), "next task")
		switch reason {
		case "empty":
			in.msg.Text = "  "
		case "attachment":
			in.msg.Document = &telegram.Document{FileID: "file"}
		case "full":
			w.b.cfg.QueueCapacity = 0
		}
		if !w.routeFollowupInput(&in) || len(w.queue) != 0 || len(w.steers) != 0 {
			t.Fatalf("%s fell through or queued", reason)
		}
		got, err := w.b.db.FollowupRequest(r.Token)
		requireStoreOK(t, err)
		if got.State != "delivered" {
			t.Fatalf("%s consumed request: %+v", reason, got)
		}
	}
	w.b.cfg.QueueCapacity = 2
	in := followupTestIncoming(t, w, r, 105, "valid retry")
	if !w.routeFollowupInput(&in) || len(w.queue) != 1 {
		t.Fatal("valid retry was not accepted")
	}
}

func TestFollowupInvalidRepliesNeverReachOrdinaryRouting(t *testing.T) {
	for _, reason := range []string{"expired", "owner", "chat", "topic", "bot", "pending", "uncertain", "cancelled", "restart", "binding", "message-id", "marker-omitted"} {
		t.Run(reason, func(t *testing.T) {
			w, _, _ := setupWorkspaceWorker(t)
			r := persistTestFollowup(t, w, reason)
			in := followupTestIncoming(t, w, r, 100, "must never become ordinary input")
			var err error
			switch reason {
			case "expired":
				_, err = w.b.db.DB.Exec(`UPDATE followup_requests SET expires_at=0`)
			case "owner":
				in.msg.From.ID++
			case "chat":
				in.msg.Chat.ID--
			case "topic":
				in.msg.MessageThreadID++
			case "bot":
				in.msg.ReplyToMessage.From.ID++
			case "pending", "uncertain":
				_, err = w.b.db.DB.Exec(`UPDATE followup_requests SET state=?,prompt_id=0`, reason)
			case "cancelled":
				err = w.b.db.CancelFollowupRequests(r.Bot, r.Chat, r.Thread)
			case "restart":
				w.b.quickActionsInstance = "new-bridge-instance"
			case "binding":
				w.binding.Generation++
			case "message-id":
				in.msg.ReplyToMessage.MessageID++
			case "marker-omitted":
				in.msg.ReplyToMessage.Text = ""
				err = w.b.db.CancelFollowupRequests(r.Bot, r.Chat, r.Thread)
			}
			requireStoreOK(t, err)
			w.handle(in)
			if len(w.queue) != 0 || len(w.steers) != 0 || w.client != nil {
				t.Fatal("invalid recognized reply reached ordinary input routing")
			}
			var state string
			requireStoreOK(t, w.b.db.DB.QueryRow(`SELECT state FROM inbox WHERE id=100`).Scan(&state))
			if state != "cancelled" {
				t.Fatalf("invalid reply state = %s", state)
			}
		})
	}
}

func TestFollowupKnownCommandsKeepPrecedenceAndStripOnlyBridgeQuote(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 2
	queueReady(t, w, command)
	r := persistTestFollowup(t, w, "commands")
	in := followupTestIncoming(t, w, r, 100, "/followup explicit next task")
	w.handle(in)
	if len(w.queue) != 1 || w.queue[0].text != "explicit next task" || strings.Contains(w.queue[0].text, "Bridge input prompt") {
		t.Fatalf("known command was not ordinarily dispatched: %+v", w.queue)
	}
	stop := followupTestIncoming(t, w, r, 101, "/stop")
	w.handle(stop)
	got, err := w.b.db.FollowupRequest(r.Token)
	requireStoreOK(t, err)
	if got.State != "cancelled" || len(w.queue) != 0 {
		t.Fatalf("explicit stop did not cancel pending request and queue: %+v", got)
	}
	ordinary := followupTestIncoming(t, w, r, 102, "/status")
	ordinary.msg.ReplyToMessage.MessageID = 901
	ordinary.msg.ReplyToMessage.Text = "a real user quotation"
	ordinary.msg.ReplyToMessage.From = &telegram.User{ID: 8}
	original := ordinary.msg
	if w.routeFollowupInput(&ordinary) || ordinary.msg != original || ordinary.msg.Quote == nil {
		t.Fatal("ordinary user reply context was stripped")
	}
}

type followupBlockingTransport struct {
	base    http.RoundTripper
	started chan struct{}
	release chan struct{}
}

func (f *followupBlockingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if filepath.Base(r.URL.Path) == "sendMessage" {
		select {
		case f.started <- struct{}{}:
		default:
		}
		select {
		case <-f.release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return f.base.RoundTrip(r)
}

func TestFollowupPromptIsPersistedAsyncForceReplyAndStopTokenSurvives(t *testing.T) {
	w, fake, _ := setupWorkspaceWorker(t)
	persistTestFollowup(t, w, "old")
	w.active, w.activeReplyTo, w.owner, w.busy, w.turn = 2, 50, 7, true, 1
	c := confirmation{action: "stop", generation: w.binding.Generation, active: w.active, turn: w.turn, user: 7, messageID: 90}
	token := w.newProgressStopToken()
	w.confirms[token] = c
	transport := &followupBlockingTransport{base: fake, started: make(chan struct{}, 1), release: make(chan struct{})}
	http.DefaultTransport = transport
	t.Cleanup(func() { w.busy = false; w.active = 0 })
	q := &telegram.CallbackQuery{ID: "click", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: 90, Chat: telegram.Chat{ID: w.key.chat}, MessageThreadID: w.key.thread}}
	if !w.handleProgressFollowup(w.ctx, q, token, "followup", c) {
		t.Fatal("follow-up callback not handled")
	}
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("async send did not start")
	}
	var pendingToken string
	requireStoreOK(t, w.b.db.DB.QueryRow(`SELECT token FROM followup_requests WHERE state='pending'`).Scan(&pendingToken))
	if _, ok := w.confirms[token]; !ok || len(w.queue) != 0 {
		t.Fatal("click consumed Stop token or queued an empty prompt")
	}
	close(transport.release)
	result := waitOperation(t, w)
	if result.kind != "followup_prompt" {
		t.Fatalf("unexpected async lane: %s", result.kind)
	}
	// Settlement before the send result must not discard the prompt.
	w.busy, w.active = false, 0
	w.turn++
	w.operationReturned(result)
	r, err := w.b.db.FollowupRequest(pendingToken)
	requireStoreOK(t, err)
	if r.State != "delivered" || r.PromptID <= 0 {
		t.Fatalf("natural finish discarded prompt: %+v", r)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.messages) != 1 {
		t.Fatalf("prompt send count = %d", len(fake.messages))
	}
	fields := fake.messages[0]
	markup, _ := fields["reply_markup"].(map[string]any)
	sentToken, marked := followupReplyToken(fields["text"].(string))
	if markup["force_reply"] != true || markup["selective"] != true || fields["message_thread_id"] != float64(w.key.thread) || sentReplyTarget(fields) != float64(50) || !marked || sentToken != pendingToken {
		t.Fatalf("ForceReply correlation missing: %+v", fields)
	}
	if !strings.Contains(fields["text"].(string), `<a href="tg://user?id=7">`) {
		t.Fatalf("selective input did not target the clicking user: %+v", fields)
	}
}

func TestFollowupSendUncertaintyAndCancelledCompletionAreRejected(t *testing.T) {
	w, _, _ := setupWorkspaceWorker(t)
	r := persistTestFollowup(t, w, "uncertain-result")
	_, err := w.b.db.DB.Exec(`UPDATE followup_requests SET state='pending',prompt_id=0`)
	requireStoreOK(t, err)
	w.operationReturned(operationResult{kind: "followup_prompt", err: errors.New("transport lost"), meta: followupPromptResult{token: r.Token, instance: r.Instance, messageID: 901}})
	in := followupTestIncoming(t, w, r, 100, "do not guess delivery")
	if !w.routeFollowupInput(&in) || len(w.queue) != 0 {
		t.Fatal("send-uncertain prompt reply fell through")
	}
	_, err = w.b.db.DB.Exec(`UPDATE followup_requests SET state='cancelled'`)
	requireStoreOK(t, err)
	w.operationReturned(operationResult{kind: "followup_prompt", meta: followupPromptResult{token: r.Token, instance: r.Instance, messageID: 902}})
	got, err := w.b.db.FollowupPrompt(r.Bot, r.Chat, 902)
	requireStoreOK(t, err)
	if got.State != "cancelled" {
		t.Fatalf("late send completion revived cancelled request: %+v", got)
	}
}

func TestFollowupConsumedInboxInterruptedBeforeAppendIsCancelled(t *testing.T) {
	w, _, _ := setupWorkspaceWorker(t)
	r := persistTestFollowup(t, w, "interrupted-admission")
	in := followupTestIncoming(t, w, r, 100, "never replay")
	accepted, err := w.b.db.ConsumeFollowupRequest(r, in.id)
	requireStoreOK(t, err)
	if !accepted {
		t.Fatal("fixture claim failed")
	}
	if !w.routeFollowupInput(&in) || len(w.queue) != 0 {
		t.Fatal("interrupted claim replayed")
	}
	var state string
	requireStoreOK(t, w.b.db.DB.QueryRow(`SELECT state FROM inbox WHERE id=100`).Scan(&state))
	if state != "cancelled" {
		t.Fatalf("interrupted inbox not cancelled: %s", state)
	}
}

func TestFollowupMigrationGateDoesNotConsumeValidRequest(t *testing.T) {
	w, _, _ := setupWorkspaceWorker(t)
	r := persistTestFollowup(t, w, "migration-gate")
	w.migration = &store.Migration{Source: w.binding}
	t.Cleanup(func() { w.migration = nil })
	in := followupTestIncoming(t, w, r, 100, "blocked by migration")
	w.handle(in)
	got, err := w.b.db.FollowupRequest(r.Token)
	requireStoreOK(t, err)
	if got.State != "delivered" || len(w.queue) != 0 || w.client != nil {
		t.Fatalf("migration gate bypassed: %+v", got)
	}
}

func TestFollowupStaleCommandsCannotChangeCurrentRoot(t *testing.T) {
	for _, tc := range []struct{ reason, text string }{
		{"expired", "/close"},
		{"cancelled", "/stop"},
		{"restart", "/close"},
		{"generation", "/stop"},
		{"session", "/close"},
		{"workspace", "/stop"},
		{"prompt-id", "/close"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			w, _, command := setupWorkspaceWorker(t)
			w.b.cfg.QueueCapacity = 4
			queueReady(t, w, command)
			r := persistTestFollowup(t, w, "stale-control")
			command("missing-terminal")
			w.dispatch()
			in := followupTestIncoming(t, w, r, 100, tc.text)
			var err error
			switch tc.reason {
			case "expired":
				_, err = w.b.db.DB.Exec(`UPDATE followup_requests SET expires_at=0`)
			case "cancelled":
				err = w.b.db.CancelFollowupRequests(r.Bot, r.Chat, r.Thread)
			case "restart":
				w.b.quickActionsInstance = "restarted-bridge"
			case "generation":
				_, err = w.b.db.DB.Exec(`UPDATE followup_requests SET generation=generation+1`)
			case "session":
				_, err = w.b.db.DB.Exec(`UPDATE followup_requests SET session='old-session'`)
			case "workspace":
				_, err = w.b.db.DB.Exec(`UPDATE followup_requests SET workspace='old-workspace'`)
			case "prompt-id":
				in.msg.ReplyToMessage.MessageID++
			}
			requireStoreOK(t, err)
			binding, root, turn, client := w.binding, w.active, w.turn, w.client
			w.handle(in)
			if w.binding != binding || w.active != root || w.turn != turn || w.client != client || !w.taskRunning() || len(w.queue) != 0 || len(w.steers) != 0 {
				t.Fatalf("stale %s changed current root or session", tc.text)
			}
			if queueState(t, w, in.id) != "cancelled" {
				t.Fatal("stale command inbox not cancelled")
			}
		})
	}
}

func TestFollowupCallbackChecksOwnerTaskAndConversation(t *testing.T) {
	w, fake, _ := setupWorkspaceWorker(t)
	persistTestFollowup(t, w, "callback-authorization")
	w.active, w.owner, w.busy, w.turn = 2, 7, true, 1
	t.Cleanup(func() { w.active = 0; w.busy = false })
	base := confirmation{action: "stop", generation: w.binding.Generation, active: 2, turn: 1, user: 7, messageID: 90}
	for _, reason := range []string{"owner", "task", "turn", "generation", "chat", "topic", "message"} {
		c := base
		q := &telegram.CallbackQuery{ID: reason, From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: 90, Chat: telegram.Chat{ID: w.key.chat}, MessageThreadID: w.key.thread}}
		switch reason {
		case "owner":
			q.From.ID++
		case "task":
			c.active++
		case "turn":
			c.turn++
		case "generation":
			c.generation++
		case "chat":
			q.Message.Chat.ID--
		case "topic":
			q.Message.MessageThreadID++
		case "message":
			q.Message.MessageID++
		}
		if !w.handleProgressFollowup(w.ctx, q, "stop-token", "followup", c) {
			t.Fatalf("%s callback fell through", reason)
		}
	}
	w.background.Wait()
	if fake.messageCount() != 0 || len(w.queue) != 0 {
		t.Fatal("unauthorized callback created a prompt or queued input")
	}
}

func TestFollowupRepeatedClickReplacesPriorRequestWithoutConsumingStop(t *testing.T) {
	w, _, _ := setupWorkspaceWorker(t)
	persistTestFollowup(t, w, "replaced-click")
	w.active, w.owner, w.busy, w.turn = 2, 7, true, 1
	t.Cleanup(func() { w.active = 0; w.busy = false })
	c := confirmation{action: "stop", generation: w.binding.Generation, active: 2, turn: 1, user: 7, messageID: 90}
	token := w.newProgressStopToken()
	w.confirms[token] = c
	q := &telegram.CallbackQuery{ID: "repeat", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: 90, Chat: telegram.Chat{ID: w.key.chat}, MessageThreadID: w.key.thread}}
	var previous store.FollowupRequest
	for n := range 2 {
		if !w.handleProgressFollowup(w.ctx, q, token, "followup", c) {
			t.Fatal("follow-up callback fell through")
		}
		w.operationReturned(waitOperation(t, w))
		var currentToken string
		requireStoreOK(t, w.b.db.DB.QueryRow(`SELECT token FROM followup_requests WHERE state='delivered'`).Scan(&currentToken))
		current, err := w.b.db.FollowupRequest(currentToken)
		requireStoreOK(t, err)
		if n == 1 {
			old, err := w.b.db.FollowupRequest(previous.Token)
			requireStoreOK(t, err)
			if old.State != "cancelled" || current.Token == old.Token {
				t.Fatalf("repeat click did not replace prior prompt: old=%+v next=%+v", old, current)
			}
		}
		previous = current
	}
	if _, ok := w.confirms[token]; !ok || len(w.queue) != 0 {
		t.Fatal("input clicks consumed Stop or submitted a task")
	}
}

func TestFollowupSlashTextUsesIndependentQueue(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	queueReady(t, w, command)
	command("missing-terminal")
	w.dispatch()
	r := persistTestFollowup(t, w, "slash-followup")
	in := followupTestIncoming(t, w, r, 100, "/not_a_bridge_command literal")
	w.handle(in)
	if len(w.queue) != 1 || w.queue[0].text != "/not_a_bridge_command literal" || len(w.steers) != 0 {
		t.Fatalf("literal slash follow-up changed queue semantics: %+v", w.queue)
	}
	got, err := w.b.db.FollowupRequest(r.Token)
	requireStoreOK(t, err)
	if got.State != "consumed" || got.InboxID != in.id {
		t.Fatalf("literal slash follow-up was not accepted once: %+v", got)
	}
}

func TestFollowupOrdinaryMarkerTextPreservesReplyContext(t *testing.T) {
	for _, kind := range []string{"other-bot", "inline", "code-block"} {
		t.Run(kind, func(t *testing.T) {
			w, _, trace := nativeWorker(t, "enabled")
			nativeCatalogReady(t, w)
			r := persistTestFollowup(t, w, "real-followup")
			in := followupTestIncoming(t, w, r, 100, "ordinary quoted request")
			in.msg.ReplyToMessage.MessageID = 901
			marker := followupMarker("unrelated")
			in.msg.ReplyToMessage.Text = "ordinary text " + marker
			if kind == "other-bot" {
				in.msg.ReplyToMessage.From.ID++
				in.msg.ReplyToMessage.Text = "Ordinary body\n" + marker
			} else if kind == "code-block" {
				in.msg.ReplyToMessage.Text = "Example\n```\n" + marker + "\n```"
			}
			in.msg.Quote = nil
			w.handle(in)
			w.dispatch()
			nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" })
			traceData, err := os.ReadFile(trace)
			requireStoreOK(t, err)
			if !strings.Contains(string(traceData), "ordinary quoted request") || !strings.Contains(string(traceData), marker) {
				t.Fatalf("ordinary reply or its quoted marker did not reach OMP: %s", traceData)
			}
		})
	}
}

func TestFollowupStopDuringMigrationPreflightInvalidatesRequest(t *testing.T) {
	w, _, _ := migrationWorkflowWorker(t, "success")
	r := persistTestFollowup(t, w, "migration-stop")
	before := w.binding
	acceptWorkerInput(t, w, 700, "/migrate")
	if w.migrationRun == nil || w.migration != nil || !w.migrationLocked() {
		t.Fatal("preflight did not start")
	}
	acceptWorkerInput(t, w, 701, "/stop")
	if w.migrationLocked() || !sameBindingIdentity(w.binding, before) {
		t.Fatal("preflight Stop did not preserve and unlock original binding")
	}
	afterStop, err := w.b.db.FollowupRequest(r.Token)
	requireStoreOK(t, err)
	in := followupTestIncoming(t, w, r, 702, "must not execute after Stop")
	w.handle(in)
	if afterStop.State != "cancelled" || len(w.queue) != 0 || queueState(t, w, in.id) != "cancelled" || w.client != nil {
		t.Fatalf("Stop left Follow-up executable: state=%s queue=%+v runtime=%v", afterStop.State, w.queue, w.client != nil)
	}
}

func TestFollowupDeletedPromptMarkerRejectsTextAndCommands(t *testing.T) {
	for _, text := range []string{"never submit this", "/close", "/stop"} {
		t.Run(text, func(t *testing.T) {
			w, _, _ := setupWorkspaceWorker(t)
			r := persistTestFollowup(t, w, "deleted-prompt")
			_, err := w.b.db.DB.Exec(`UPDATE followup_requests SET expires_at=0`)
			requireStoreOK(t, err)
			count, err := w.b.db.CleanupFollowupRequests(w.ctx, time.Now().Unix())
			requireStoreOK(t, err)
			if count != 1 {
				t.Fatalf("prompt was not cleaned up: %d", count)
			}
			in := followupTestIncoming(t, w, r, 100, text)
			binding := w.binding
			w.handle(in)
			if queueState(t, w, in.id) != "cancelled" || w.binding != binding || w.client != nil || len(w.queue) != 0 || len(w.steers) != 0 {
				t.Fatal("deleted prompt fell through to ordinary input or command routing")
			}
		})
	}
}

func TestFollowupUnrecognizedMarkersPreserveReplyContext(t *testing.T) {
	for _, tc := range []struct {
		name, marker string
		sender       telegram.User
	}{
		{name: "user", marker: "Prompt\n" + followupMarker("unknown"), sender: telegram.User{ID: 7}},
		{name: "other-bot", marker: "Prompt\n" + followupMarker("unknown"), sender: telegram.User{ID: 100, IsBot: true}},
		{name: "empty", marker: "Prompt\n[followup:]", sender: telegram.User{ID: 99, IsBot: true}},
		{name: "unclosed", marker: "Prompt\n[followup:unknown", sender: telegram.User{ID: 99, IsBot: true}},
		{name: "whitespace", marker: "Prompt\n[followup:not a token]", sender: telegram.User{ID: 99, IsBot: true}},
		{name: "inline", marker: "Example: " + followupMarker("unknown"), sender: telegram.User{ID: 99, IsBot: true}},
		{name: "inline-final-line", marker: "Example\nUse " + followupMarker("unknown"), sender: telegram.User{ID: 99, IsBot: true}},
		{name: "suffix", marker: "Example\n" + followupMarker("unknown") + " in text", sender: telegram.User{ID: 99, IsBot: true}},
		{name: "code-block", marker: "Example\n```\n" + followupMarker("unknown") + "\n```", sender: telegram.User{ID: 99, IsBot: true}},
		{name: "middle-line", marker: "Example\n" + followupMarker("unknown") + "\nMore text", sender: telegram.User{ID: 99, IsBot: true}},
		{name: "empty-body", marker: "\n" + followupMarker("unknown"), sender: telegram.User{ID: 99, IsBot: true}},
		{name: "marker-only", marker: followupMarker("unknown"), sender: telegram.User{ID: 99, IsBot: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, _ := setupWorkspaceWorker(t)
			in := incoming{msg: &telegram.Message{
				Chat:           telegram.Chat{ID: w.key.chat},
				ReplyToMessage: &telegram.Message{MessageID: 901, From: &tc.sender, Text: tc.marker},
				Quote:          &telegram.TextQuote{Text: tc.marker},
			}}
			original := in.msg
			if w.routeFollowupInput(&in) || in.msg != original || in.msg.Quote == nil {
				t.Fatal("unrecognized marker swallowed input or removed reply context")
			}
		})
	}
}
