package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/logging"
	"omp-telegram/internal/telegram"
)

type commandMenuRequest struct {
	Commands []telegram.BotCommand `json:"commands"`
	Scope    struct {
		Type   string `json:"type"`
		ChatID int64  `json:"chat_id"`
	} `json:"scope"`
	Language string `json:"language_code"`
}

type commandMenuTransport struct {
	mu            sync.Mutex
	calls         []commandMenuRequest
	menus         map[int64]map[string][]telegram.BotCommand
	failures      map[string]int
	lostResponses map[string]int
	gate          <-chan struct{}
	started       chan commandMenuRequest
	completed     chan commandMenuRequest
}

func (f *commandMenuTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if filepath.Base(r.URL.Path) != "setMyCommands" {
		return nil, fmt.Errorf("unexpected Telegram method")
	}
	var request commandMenuRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return nil, err
	}
	f.mu.Lock()
	first := len(f.calls) == 0
	f.calls = append(f.calls, request)
	lostResponse := f.lostResponses[request.Language] > 0
	if lostResponse {
		f.lostResponses[request.Language]--
	}
	fail := !lostResponse && f.failures[request.Language] > 0
	if fail {
		f.failures[request.Language]--
	}
	f.mu.Unlock()
	f.started <- request
	if first && f.gate != nil {
		select {
		case <-f.gate:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	status := http.StatusOK
	body := `{"ok":true,"result":true}`
	if fail {
		status = http.StatusBadRequest
		body = `{"ok":false,"error_code":400,"description":"SECRET_NATIVE_NAME SECRET_TOKEN https://private.invalid/workspace"}`
	} else {
		f.mu.Lock()
		if f.menus[request.Scope.ChatID] == nil {
			f.menus[request.Scope.ChatID] = make(map[string][]telegram.BotCommand)
		}
		f.menus[request.Scope.ChatID][request.Language] = request.Commands
		f.mu.Unlock()
	}
	f.completed <- request
	if lostResponse {
		return nil, io.ErrUnexpectedEOF
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
}

func newCommandMenuFixture(t *testing.T, chats ...int64) (*Bridge, *commandMenuTransport) {
	t.Helper()
	transport := &commandMenuTransport{
		menus:         make(map[int64]map[string][]telegram.BotCommand),
		failures:      make(map[string]int),
		lostResponses: make(map[string]int),
		started:       make(chan commandMenuRequest, 256),
		completed:     make(chan commandMenuRequest, 256),
	}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	b := testBridge(t, &Bridge{cfg: config.Config{AllowedChats: chats}, tg: newTestTelegram(t)})
	return b, transport
}

func startCommandMenuPublisher(t *testing.T, b *Bridge) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.runCommandMenus(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("command publisher did not stop after cancellation")
		}
	})
	return cancel, done
}

func receiveCommandMenu(t *testing.T, requests <-chan commandMenuRequest) commandMenuRequest {
	t.Helper()
	select {
	case request := <-requests:
		if request.Scope.Type != "chat" {
			t.Fatalf("command scope = %q, want chat", request.Scope.Type)
		}
		if request.Language != "" && request.Language != "zh" {
			t.Fatalf("unexpected language %q", request.Language)
		}
		return request
	case <-time.After(3 * time.Second):
		t.Fatal("command publication did not arrive")
		return commandMenuRequest{}
	}
}

func requireCommandMenu(t *testing.T, commands []telegram.BotCommand, native ...string) {
	t.Helper()
	want := slices.Clone(botCommands)
	for _, name := range native {
		want = append(want, telegram.BotCommand{Command: name, Description: nativeCommandDescription})
	}
	if !slices.Equal(commands, want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
}

func requirePublishedMenus(t *testing.T, f *commandMenuTransport, chat int64, native ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, language := range []string{"", "zh"} {
		commands, ok := f.menus[chat][language]
		if !ok {
			t.Fatalf("no menu for chat %d language %q", chat, language)
		}
		requireCommandMenu(t, commands, native...)
	}
}

func TestCommandMenuTopicUnionAndChatIsolation(t *testing.T) {
	b, f := newCommandMenuFixture(t, -10, -20)
	first := &worker{key: target{chat: -10, thread: 1}}
	second := &worker{key: target{chat: -10, thread: 2}}
	other := &worker{key: target{chat: -20, thread: 1}}
	for _, w := range []*worker{first, second, other} {
		b.registerCommandMenuWorker(w)
	}
	b.updateCommandMenu(first, []string{"zebra", "shared"})
	b.updateCommandMenu(second, []string{"alpha", "shared"})
	b.updateCommandMenu(other, []string{"private_command"})
	startCommandMenuPublisher(t, b)
	for range 4 {
		receiveCommandMenu(t, f.completed)
	}
	requirePublishedMenus(t, f, -10, "alpha", "shared", "zebra")
	requirePublishedMenus(t, f, -20, "private_command")

	b.updateCommandMenu(first, nil)
	for range 2 {
		requireCommandMenu(t, receiveCommandMenu(t, f.completed).Commands, "alpha", "shared")
	}
	b.removeCommandMenu(second)
	for range 2 {
		requireCommandMenu(t, receiveCommandMenu(t, f.completed).Commands)
	}
	requirePublishedMenus(t, f, -10)
	requirePublishedMenus(t, f, -20, "private_command")
}

func TestCommandMenuNamesCapAndBridgePrecedence(t *testing.T) {
	b, f := newCommandMenuFixture(t, -10)
	w := &worker{key: target{chat: -10, thread: 1}}
	b.registerCommandMenuWorker(w)
	names := []string{"", "Upper", "with-dash", "with.dot", "with space", "a\nb", "/slash", "中文", strings.Repeat("a", 33), "start", "move", "wt", "worktree", "_", "0", strings.Repeat("a", 32), "_"}
	for _, command := range botCommands {
		names = append(names, command.Command)
	}
	for i := 119; i >= 0; i-- {
		names = append(names, fmt.Sprintf("native_%03d", i))
	}
	b.updateCommandMenu(w, names)
	// Mutating the caller's slice must not alter the owned contribution.
	for i := range names {
		names[i] = "caller_mutated"
	}
	startCommandMenuPublisher(t, b)
	want := []string{"0", "_", strings.Repeat("a", 32)}
	for i := 0; len(want) < 100-len(botCommands); i++ {
		want = append(want, fmt.Sprintf("native_%03d", i))
	}
	for range 2 {
		request := receiveCommandMenu(t, f.completed)
		if len(request.Commands) != 100 {
			t.Fatalf("menu has %d commands, want 100", len(request.Commands))
		}
		requireCommandMenu(t, request.Commands, want...)
	}
}

func TestCommandMenuRemovalAndStaleOwners(t *testing.T) {
	b, f := newCommandMenuFixture(t, -10)
	old := &worker{key: target{chat: -10, thread: 1}}
	current := &worker{key: old.key}
	b.registerCommandMenuWorker(old)
	b.updateCommandMenu(old, []string{"obsolete"})
	b.registerCommandMenuWorker(current)
	b.updateCommandMenu(old, []string{"stale"})
	b.removeCommandMenu(old)
	startCommandMenuPublisher(t, b)
	for range 2 {
		requireCommandMenu(t, receiveCommandMenu(t, f.completed).Commands)
	}

	// A retiring actor's removal races with a replacement's fresh catalog.
	staleDone := make(chan struct{})
	go func() {
		b.removeCommandMenu(old)
		b.updateCommandMenu(old, []string{"stale"})
		close(staleDone)
	}()
	b.updateCommandMenu(current, []string{"current"})
	<-staleDone
	for range 2 {
		requireCommandMenu(t, receiveCommandMenu(t, f.completed).Commands, "current")
	}
	b.removeCommandMenu(current)
	b.updateCommandMenu(current, []string{"revived"})
	for range 2 {
		requireCommandMenu(t, receiveCommandMenu(t, f.completed).Commands)
	}
	requirePublishedMenus(t, f, -10)

	b.registerCommandMenuWorker(current)
	b.updateCommandMenu(current, []string{"rebound"})
	for range 2 {
		requireCommandMenu(t, receiveCommandMenu(t, f.completed).Commands, "rebound")
	}
}

func TestCommandMenuCoalescesAndPublishesLatestInFlight(t *testing.T) {
	b, f := newCommandMenuFixture(t, -10)
	gate := make(chan struct{})
	f.gate = gate
	w := &worker{key: target{chat: -10, thread: 1}}
	b.registerCommandMenuWorker(w)
	b.updateCommandMenu(w, []string{"first"})
	b.updateCommandMenu(w, []string{"before_start_latest"})
	startCommandMenuPublisher(t, b)
	first := receiveCommandMenu(t, f.started)
	requireCommandMenu(t, first.Commands, "before_start_latest")

	updated := make(chan struct{})
	go func() {
		b.updateCommandMenu(w, []string{"intermediate"})
		b.updateCommandMenu(w, []string{"latest"})
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(3 * time.Second):
		t.Fatal("actor catalog updates blocked behind Telegram HTTP")
	}
	close(gate)
	for range 4 {
		receiveCommandMenu(t, f.completed)
	}
	requirePublishedMenus(t, f, -10, "latest")
	f.mu.Lock()
	for _, request := range f.calls {
		for _, command := range request.Commands {
			if command.Command == "first" || command.Command == "intermediate" {
				f.mu.Unlock()
				t.Fatalf("superseded catalog published: %q", command.Command)
			}
		}
	}
	f.mu.Unlock()

	// A later chat is a publisher barrier: identical updates queued ahead of
	// it must have been processed without issuing additional HTTP requests.
	for range 10 {
		b.updateCommandMenu(w, []string{"latest"})
	}
	barrier := &worker{key: target{chat: -20, thread: 1}}
	b.registerCommandMenuWorker(barrier)
	for range 2 {
		request := receiveCommandMenu(t, f.completed)
		if request.Scope.ChatID != -20 {
			t.Fatalf("identical catalog caused another request for %d", request.Scope.ChatID)
		}
	}
	f.mu.Lock()
	calls := len(f.calls)
	f.mu.Unlock()
	if calls != 6 {
		t.Fatalf("got %d requests, want two initial, two latest, and two barrier", calls)
	}
}

func TestCommandMenuStartupResetsStaleScopesAndPreservesEarlyCatalog(t *testing.T) {
	b, f := newCommandMenuFixture(t, -10, -20)
	for _, chat := range []int64{-10, -20} {
		f.menus[chat] = map[string][]telegram.BotCommand{
			"":   {{Command: "stale_server_menu", Description: "stale"}},
			"zh": {{Command: "stale_server_menu", Description: "stale"}},
		}
	}
	w := &worker{key: target{chat: -10, thread: 1}}
	b.registerCommandMenuWorker(w)
	b.updateCommandMenu(w, []string{"early_catalog"})
	startCommandMenuPublisher(t, b)
	for range 4 {
		receiveCommandMenu(t, f.completed)
	}
	requirePublishedMenus(t, f, -10, "early_catalog")
	requirePublishedMenus(t, f, -20)
}

func TestCommandMenuFailuresAreNotCachedAndLogsAreSanitized(t *testing.T) {
	b, f := newCommandMenuFixture(t, -10)
	capture := &logCapture{}
	logs, err := logging.New(capture, logging.Options{Level: "info", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	b.telegramLog = logs.Logger(logging.Telegram)
	f.failures[""] = 1
	w := &worker{key: target{chat: -10, thread: 1}}
	b.registerCommandMenuWorker(w)
	b.updateCommandMenu(w, []string{"safe"})
	startCommandMenuPublisher(t, b)
	for range 2 {
		receiveCommandMenu(t, f.completed)
	}
	b.updateCommandMenu(w, []string{"safe"})
	request := receiveCommandMenu(t, f.completed)
	if request.Language != "" {
		t.Fatalf("retried successful language %q instead of failed default language", request.Language)
	}
	requirePublishedMenus(t, f, -10, "safe")
	text := capture.String()
	if !strings.Contains(text, "telegram_chat_commands_failed") {
		t.Fatalf("failure log missing: %s", text)
	}
	for _, secret := range []string{"SECRET_NATIVE_NAME", "SECRET_TOKEN", "private.invalid", "workspace", "command_menu_registered"} {
		if strings.Contains(text, secret) {
			t.Fatalf("unsafe or false-success log contains %q", secret)
		}
	}
}

func TestCommandMenuCancellationInterruptsBlockedRequest(t *testing.T) {
	b, f := newCommandMenuFixture(t, -10)
	f.gate = make(chan struct{})
	cancel, done := startCommandMenuPublisher(t, b)
	receiveCommandMenu(t, f.started)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("publisher did not cancel blocked HTTP request")
	}
}

func TestCommandMenuRestoresPreviousMenuAfterPublicationFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		lostResponses int
		rejections    int
		uncertain     bool
	}{
		{name: "transport retries exhausted", lostResponses: 3, uncertain: true},
		{name: "lost response then definite rejection", lostResponses: 1, rejections: 1, uncertain: true},
		{name: "definite rejection preserves successful cache", rejections: 1},
	} {
		for _, language := range []string{"", "zh"} {
			t.Run(fmt.Sprintf("%s/language=%q", tc.name, language), func(t *testing.T) {
				b, f := newCommandMenuFixture(t, -10)
				w := &worker{key: target{chat: -10, thread: 1}}
				barrier := &worker{key: target{chat: -20, thread: 1}}
				b.registerCommandMenuWorker(w)
				b.registerCommandMenuWorker(barrier)
				b.updateCommandMenu(w, []string{"menu_a"})
				b.updateCommandMenu(barrier, []string{"initial_barrier"})
				startCommandMenuPublisher(t, b)
				for range 4 {
					receiveCommandMenu(t, f.completed)
				}
				requirePublishedMenus(t, f, -10, "menu_a")

				f.mu.Lock()
				f.lostResponses[language] = tc.lostResponses
				f.failures[language] = tc.rejections
				f.mu.Unlock()
				b.updateCommandMenu(w, []string{"menu_b"})
				b.updateCommandMenu(barrier, []string{"failure_barrier"})
				// Each lost response applies B before failing. The client retries
				// internally; a later explicit rejection must not erase uncertainty.
				for range tc.lostResponses + tc.rejections + 1 {
					request := receiveCommandMenu(t, f.completed)
					if request.Scope.ChatID != -10 {
						t.Fatalf("failure attempt chat = %d, want -10", request.Scope.ChatID)
					}
					requireCommandMenu(t, request.Commands, "menu_b")
				}
				// This later chat proves all failure handling and cache updates
				// finished, without polling publisher internals or sleeping.
				for range 2 {
					request := receiveCommandMenu(t, f.completed)
					if request.Scope.ChatID != -20 {
						t.Fatalf("failure barrier chat = %d, want -20", request.Scope.ChatID)
					}
					requireCommandMenu(t, request.Commands, "failure_barrier")
				}
				if tc.uncertain {
					requirePublishedMenus(t, f, -10, "menu_b")
				} else {
					f.mu.Lock()
					commands := slices.Clone(f.menus[-10][language])
					f.mu.Unlock()
					requireCommandMenu(t, commands, "menu_a")
				}

				// Returning to A must restore the actual remote menu, even
				// though A was the last publication acknowledged successfully.
				b.updateCommandMenu(w, []string{"menu_a"})
				b.updateCommandMenu(barrier, []string{"restored_barrier"})
				restoreRequests := 1
				if tc.uncertain {
					restoreRequests = 2
				}
				for range restoreRequests {
					request := receiveCommandMenu(t, f.completed)
					if request.Scope.ChatID != -10 {
						t.Fatalf("restoration chat = %d, want -10", request.Scope.ChatID)
					}
					if !tc.uncertain && request.Language == language {
						t.Fatalf("definite rejection discarded the successful cache for language %q", language)
					}
					requireCommandMenu(t, request.Commands, "menu_a")
				}
				for range 2 {
					request := receiveCommandMenu(t, f.completed)
					if request.Scope.ChatID != -20 {
						t.Fatalf("restoration barrier chat = %d, want -20", request.Scope.ChatID)
					}
					requireCommandMenu(t, request.Commands, "restored_barrier")
				}
				requirePublishedMenus(t, f, -10, "menu_a")
			})
		}
	}
}
