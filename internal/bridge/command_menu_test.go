package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/i18n"
	"omp-telegram/internal/logging"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
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

type commandMenuScope struct {
	chatID   int64
	language string
}

type commandMenuTransport struct {
	mu            sync.Mutex
	calls         []commandMenuRequest
	menus         map[int64]map[string][]telegram.BotCommand
	failures      map[string]int
	lostResponses map[string]int
	scopeFailures map[commandMenuScope]int
	failureStatus int
	retryAfter    int
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
	scope := commandMenuScope{request.Scope.ChatID, request.Language}
	if !lostResponse && f.scopeFailures[scope] > 0 {
		f.scopeFailures[scope]--
		fail = true
	}
	failureStatus, retryAfter := f.failureStatus, f.retryAfter
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
		status = failureStatus
		if status == 0 {
			status = http.StatusBadRequest
		}
		body = fmt.Sprintf(`{"ok":false,"error_code":%d,"description":"SECRET_NATIVE_NAME SECRET_TOKEN https://private.invalid/workspace","parameters":{"retry_after":%d}}`, status, retryAfter)
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
		scopeFailures: make(map[commandMenuScope]int),
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

func commandMenuCommands(names ...string) []telegram.BotCommand {
	commands := make([]telegram.BotCommand, 0, len(names))
	for _, name := range names {
		commands = append(commands, telegram.BotCommand{Command: name})
	}
	return commands
}

func requireCommandMenu(t *testing.T, commands []telegram.BotCommand, native ...string) {
	t.Helper()
	requireDescribedCommandMenu(t, commands, commandMenuCommands(native...)...)
}

func requireDescribedCommandMenu(t *testing.T, commands []telegram.BotCommand, native ...telegram.BotCommand) {
	t.Helper()
	want := localizedCommands(native, i18n.English)
	if len(commands) != len(want) {
		t.Fatalf("commands = %#v, want identities %#v", commands, want)
	}
	for index, expected := range want {
		if commands[index].Command != expected.Command {
			t.Fatalf("command %d = %q, want %q", index, commands[index].Command, expected.Command)
		}
		if commands[index].Description != expected.Description {
			t.Fatalf("command %q description = %q, want %q", expected.Command, commands[index].Description, expected.Description)
		}
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
	b.updateCommandMenu(first, commandMenuCommands("zebra", "shared"))
	b.updateCommandMenu(second, commandMenuCommands("alpha", "shared"))
	b.updateCommandMenu(other, commandMenuCommands("private_command"))
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
	commands := commandMenuCommands(names...)
	for i := range commands {
		for _, command := range botCommands {
			if commands[i].Command == command.Command {
				commands[i].Description = "Upstream description must not replace the bridge description"
			}
		}
	}
	b.updateCommandMenu(w, commands)
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
	b.updateCommandMenu(old, commandMenuCommands("obsolete"))
	b.registerCommandMenuWorker(current)
	b.updateCommandMenu(old, commandMenuCommands("stale"))
	b.removeCommandMenu(old)
	startCommandMenuPublisher(t, b)
	for range 2 {
		requireCommandMenu(t, receiveCommandMenu(t, f.completed).Commands)
	}

	// A retiring actor's removal races with a replacement's fresh catalog.
	staleDone := make(chan struct{})
	go func() {
		b.removeCommandMenu(old)
		b.updateCommandMenu(old, commandMenuCommands("stale"))
		close(staleDone)
	}()
	b.updateCommandMenu(current, commandMenuCommands("current"))
	<-staleDone
	for range 2 {
		requireCommandMenu(t, receiveCommandMenu(t, f.completed).Commands, "current")
	}
	b.removeCommandMenu(current)
	b.updateCommandMenu(current, commandMenuCommands("revived"))
	for range 2 {
		requireCommandMenu(t, receiveCommandMenu(t, f.completed).Commands)
	}
	requirePublishedMenus(t, f, -10)

	b.registerCommandMenuWorker(current)
	b.updateCommandMenu(current, commandMenuCommands("rebound"))
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
	b.updateCommandMenu(w, commandMenuCommands("first"))
	b.updateCommandMenu(w, commandMenuCommands("before_start_latest"))
	startCommandMenuPublisher(t, b)
	first := receiveCommandMenu(t, f.started)
	requireCommandMenu(t, first.Commands, "before_start_latest")

	updated := make(chan struct{})
	go func() {
		b.updateCommandMenu(w, commandMenuCommands("intermediate"))
		b.updateCommandMenu(w, commandMenuCommands("latest"))
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
		b.updateCommandMenu(w, commandMenuCommands("latest"))
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
	b.updateCommandMenu(w, commandMenuCommands("early_catalog"))
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
	b.updateCommandMenu(w, commandMenuCommands("safe"))
	startCommandMenuPublisher(t, b)
	for range 2 {
		receiveCommandMenu(t, f.completed)
	}
	b.updateCommandMenu(w, commandMenuCommands("safe"))
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
				b.updateCommandMenu(w, commandMenuCommands("menu_a"))
				b.updateCommandMenu(barrier, commandMenuCommands("initial_barrier"))
				startCommandMenuPublisher(t, b)
				for range 4 {
					receiveCommandMenu(t, f.completed)
				}
				requirePublishedMenus(t, f, -10, "menu_a")

				f.mu.Lock()
				f.lostResponses[language] = tc.lostResponses
				f.failures[language] = tc.rejections
				f.mu.Unlock()
				b.updateCommandMenu(w, commandMenuCommands("menu_b"))
				b.updateCommandMenu(barrier, commandMenuCommands("failure_barrier"))
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
				b.updateCommandMenu(w, commandMenuCommands("menu_a"))
				b.updateCommandMenu(barrier, commandMenuCommands("restored_barrier"))
				restoreRequests := 1
				if tc.uncertain {
					restoreRequests = 2
				}
				restored, barriers := 0, 0
				for range restoreRequests + 2 {
					request := receiveCommandMenu(t, f.completed)
					switch request.Scope.ChatID {
					case -10:
						restored++
						if !tc.uncertain && request.Language == language {
							t.Fatalf("definite rejection discarded the successful cache for language %q", language)
						}
						requireCommandMenu(t, request.Commands, "menu_a")
					case -20:
						barriers++
						requireCommandMenu(t, request.Commands, "restored_barrier")
					default:
						t.Fatalf("unexpected restoration chat: %d", request.Scope.ChatID)
					}
				}
				if restored != restoreRequests || barriers != 2 {
					t.Fatalf("restoration requests: restored=%d, barriers=%d", restored, barriers)
				}
				requirePublishedMenus(t, f, -10, "menu_a")
			})
		}
	}
}

func TestCommandMenuDescriptionUpdatesAndCachesIdenticalLists(t *testing.T) {
	b, f := newCommandMenuFixture(t, -10)
	w := &worker{key: target{chat: -10, thread: 1}}
	b.registerCommandMenuWorker(w)
	initial := []telegram.BotCommand{
		{Command: "native", Description: "Initial upstream description"},
		{Command: "native_alias", Description: "Initial upstream description"},
	}
	b.updateCommandMenu(w, initial)
	startCommandMenuPublisher(t, b)
	for range 2 {
		requireDescribedCommandMenu(t, receiveCommandMenu(t, f.completed).Commands, initial...)
	}

	updated := []telegram.BotCommand{
		{Command: "native", Description: "Revised upstream description"},
		{Command: "native_alias", Description: "Revised upstream description"},
	}
	b.updateCommandMenu(w, updated)
	for range 2 {
		requireDescribedCommandMenu(t, receiveCommandMenu(t, f.completed).Commands, updated...)
	}

	// A later chat proves identical descriptions were processed without HTTP.
	for range 10 {
		b.updateCommandMenu(w, updated)
	}
	barrier := &worker{key: target{chat: -20, thread: 1}}
	b.registerCommandMenuWorker(barrier)
	for range 2 {
		request := receiveCommandMenu(t, f.completed)
		if request.Scope.ChatID != -20 {
			t.Fatalf("unchanged descriptions caused another request for %d", request.Scope.ChatID)
		}
	}

	// Losing upstream descriptions is also a menu change, with fallback only then.
	b.updateCommandMenu(w, commandMenuCommands("native", "native_alias"))
	for range 2 {
		requireCommandMenu(t, receiveCommandMenu(t, f.completed).Commands, "native", "native_alias")
	}
}

func TestCommandMenuTopicDescriptionsPreferNonemptyStableMinimum(t *testing.T) {
	for _, descriptions := range [][3]string{
		{"", "Zulu description", "Alpha description"},
		{"Zulu description", "Alpha description", ""},
		{"Alpha description", "", "Zulu description"},
	} {
		t.Run(fmt.Sprintf("descriptions=%q", descriptions), func(t *testing.T) {
			b, f := newCommandMenuFixture(t, -10)
			var minimum, remaining *worker
			for i, description := range descriptions {
				w := &worker{key: target{chat: -10, thread: int64(i + 1)}}
				b.registerCommandMenuWorker(w)
				b.updateCommandMenu(w, []telegram.BotCommand{
					{Command: "shared", Description: description},
					{Command: "shared"},
					{Command: botCommands[0].Command, Description: "Not the bridge description"},
				})
				switch description {
				case "Alpha description":
					minimum = w
				case "Zulu description":
					remaining = w
				}
			}
			startCommandMenuPublisher(t, b)
			for range 2 {
				requireDescribedCommandMenu(t, receiveCommandMenu(t, f.completed).Commands,
					telegram.BotCommand{Command: "shared", Description: "Alpha description"})
			}

			b.removeCommandMenu(minimum)
			for range 2 {
				requireDescribedCommandMenu(t, receiveCommandMenu(t, f.completed).Commands,
					telegram.BotCommand{Command: "shared", Description: "Zulu description"})
			}
			b.removeCommandMenu(remaining)
			for range 2 {
				requireCommandMenu(t, receiveCommandMenu(t, f.completed).Commands, "shared")
			}
		})
	}
}

func TestDaemonBootstrapMenusBeforeFirstMessage(t *testing.T) {
	for _, mode := range []string{"bootstrap", "fail", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			sessions := t.TempDir()
			trace := filepath.Join(t.TempDir(), "rpc.trace")
			starts := filepath.Join(t.TempDir(), "starts.jsonl")
			t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", sessions)
			t.Setenv("OMP_TELEGRAM_FIXTURE_NATIVE_COMMANDS", mode)
			t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
			t.Setenv("OMP_TELEGRAM_FIXTURE_ARGV_TRACE", starts)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			d := &recoveryDaemon{
				t: t, root: t.TempDir(), chat: -10,
				fake: &fakeHTTP{updates: make(chan telegram.Update, 8), commandRequests: make(chan map[string]any, 16)},
				cfg: config.Config{Token: "fake", AllowedUsers: []int64{7}, AllowedChats: []int64{-10, -20},
					WorkspaceRoot: t.TempDir(), OMP: exe, DataDir: t.TempDir(), MaxWorkers: 1, QueueCapacity: 4,
					OMPArgsFingerprint: strings.Repeat("a", 64)},
			}
			previous := http.DefaultTransport
			http.DefaultTransport = d.fake
			t.Cleanup(func() {
				d.stop()
				http.DefaultTransport = previous
			})
			d.start()
			seen := make(map[string]bool)
			for range 6 { // Default menus plus the first default/zh menu for both chats.
				select {
				case raw := <-d.fake.commandRequests:
					data, err := json.Marshal(raw)
					if err != nil {
						t.Fatal(err)
					}
					var request commandMenuRequest
					if err := json.Unmarshal(data, &request); err != nil {
						t.Fatal(err)
					}
					if request.Scope.Type != "chat" {
						continue
					}
					key := fmt.Sprintf("%d/%s", request.Scope.ChatID, request.Language)
					if seen[key] {
						t.Fatalf("scope %s was cleared before its first bootstrap menu", key)
					}
					seen[key] = true
					if mode == "bootstrap" {
						requireDescribedCommandMenu(t, request.Commands,
							telegram.BotCommand{Command: "jiggle", Description: "Fixture builtin description"},
							telegram.BotCommand{Command: "shake", Description: "Fixture builtin description"})
					} else {
						requireCommandMenu(t, request.Commands)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("startup menus did not arrive before any message")
				}
			}
			for _, chat := range []int64{-10, -20} {
				for _, language := range []string{"", "zh"} {
					if !seen[fmt.Sprintf("%d/%s", chat, language)] {
						t.Fatalf("missing first menu for chat %d language %q", chat, language)
					}
				}
			}
			entries, err := os.ReadDir(sessions)
			if err != nil || len(entries) != 0 {
				t.Fatalf("bootstrap persisted native history: %v, %v", entries, err)
			}
			for _, table := range []string{"bindings", "startup_intents", "inbox", "outbox"} {
				var count int
				if err := d.db.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("bootstrap created %s records: count=%d, error=%v", table, count, err)
				}
			}
			if got := len(readStringArgvTrace(t, starts)); got != 1 {
				t.Fatalf("startup launched %d processes, want one ephemeral discovery", got)
			}
			for _, entry := range fixtureRPCTrace(trace) {
				if entry.kind == "prompt" {
					t.Fatal("bootstrap sent a prompt")
				}
			}
			// Discovery failure must not prevent polling; a healthy discovery must
			// also release the sole runtime slot before the first real session.
			d.command(11, "/help")
			d.command(11, "/new "+t.TempDir())
			if !d.binding(11).Running {
				t.Fatal("bootstrap blocked the first real native runtime")
			}
		})
	}
}

func TestCommandMenuBootstrapYieldsToAuthoritativeTopics(t *testing.T) {
	b, f := newCommandMenuFixture(t, -10, -20)
	b.commandMenus.bootstrap = []telegram.BotCommand{{Command: "shake", Description: "Bootstrap description"}}
	unknown := &worker{key: target{chat: -10, thread: 1}}
	actual := &worker{key: target{chat: -10, thread: 2}}
	b.registerCommandMenuWorker(unknown)
	b.registerCommandMenuWorker(actual)
	startCommandMenuPublisher(t, b)
	for range 4 {
		requireDescribedCommandMenu(t, receiveCommandMenu(t, f.completed).Commands, b.commandMenus.bootstrap...)
	}
	b.updateCommandMenu(actual, []telegram.BotCommand{{Command: "topic_command", Description: "Topic description"}})
	for range 2 {
		request := receiveCommandMenu(t, f.completed)
		if request.Scope.ChatID != -10 {
			t.Fatal("topic catalog changed another chat's bootstrap menu")
		}
		requireDescribedCommandMenu(t, request.Commands, telegram.BotCommand{Command: "topic_command", Description: "Topic description"})
	}
	// Empty is authoritative, rather than an invitation to reinsert hints.
	b.updateCommandMenu(actual, nil)
	for range 2 {
		requireCommandMenu(t, receiveCommandMenu(t, f.completed).Commands)
	}
	b.removeCommandMenu(actual)
	for range 2 {
		requireDescribedCommandMenu(t, receiveCommandMenu(t, f.completed).Commands, b.commandMenus.bootstrap...)
	}
}

func TestCommandMenuUnknownActorPreservesBootstrapUntilUnsupported(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_NATIVE_COMMANDS", "hold")
	trace := filepath.Join(t.TempDir(), "rpc.trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	b, f := newCommandMenuFixture(t, -10)
	b.commandMenus.bootstrap = []telegram.BotCommand{{Command: "shake", Description: "Bootstrap description"}}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := omp.Start(ctx, omp.Config{Binary: exe, NoSession: true}, b.rpcLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	w := &worker{b: b, key: target{chat: -10, thread: 1}, client: client}
	w.runtime = runtimeConnected
	b.registerCommandMenuWorker(w)
	w.syncCommandMenu()
	startCommandMenuPublisher(t, b)
	for range 2 {
		requireDescribedCommandMenu(t, receiveCommandMenu(t, f.completed).Commands, b.commandMenus.bootstrap...)
	}
	// Join the real RPC query, then turn its response into an authoritative
	// unsupported catalog. No actor-only state mutation stands in for discovery.
	queryDone := make(chan error, 1)
	go func() {
		_, err := client.RefreshCommandCatalog(ctx)
		queryDone <- err
	}()
	waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
	if _, err := client.Call(ctx, "fixture_catalog_unsupported", nil); err != nil {
		t.Fatal(err)
	}
	if err := <-queryDone; err != nil {
		t.Fatal(err)
	}
	w.syncCommandMenu()
	for range 2 {
		requireCommandMenu(t, receiveCommandMenu(t, f.completed).Commands)
	}
}

func TestCommandMenuCatalogInvalidationRestoresBootstrap(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_NATIVE_COMMANDS", "enabled")
	b, f := newCommandMenuFixture(t, -10)
	b.commandMenus.bootstrap = []telegram.BotCommand{{Command: "shake", Description: "Bootstrap description"}}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := omp.Start(ctx, omp.Config{Binary: exe, NoSession: true}, b.rpcLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	if _, err := client.RefreshCommandCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	w := &worker{b: b, key: target{chat: -10, thread: 1}, client: client}
	w.runtime = runtimeConnected
	b.registerCommandMenuWorker(w)
	w.syncCommandMenu()
	startCommandMenuPublisher(t, b)
	for range 2 {
		request := receiveCommandMenu(t, f.completed)
		hasNative := false
		for _, command := range request.Commands {
			if command.Command == "native_local" {
				hasNative = true
			}
			if command.Command == "shake" {
				t.Fatal("bootstrap overrode an authoritative catalog")
			}
		}
		if !hasNative {
			t.Fatal("authoritative native command was omitted")
		}
	}
	client.InvalidateCommandCatalog()
	w.syncCommandMenu()
	for range 2 {
		requireDescribedCommandMenu(t, receiveCommandMenu(t, f.completed).Commands, b.commandMenus.bootstrap...)
	}
}

func TestLocalizedCommandMenusPreserveOpaqueNativeDescriptions(t *testing.T) {
	native := []telegram.BotCommand{
		{Command: "empty"},
		{Command: "native_fallback_phrase", Description: "OMP native command"},
		{Command: "bridge_phrase", Description: "Cancel"},
		{Command: "bridge_help_phrase", Description: "New session: /new <name or project path>"},
		{Command: "native_text", Description: "原生 command description"},
	}
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		b, transport := newCommandMenuFixture(t, -10)
		b.setLanguage(language)
		w := &worker{key: target{chat: -10, thread: 1}}
		b.registerCommandMenuWorker(w)
		b.updateCommandMenu(w, native)
		startCommandMenuPublisher(t, b)
		for range 2 {
			commands := receiveCommandMenu(t, transport.completed).Commands
			if len(commands) != len(botCommands)+len(native) {
				t.Fatalf("menu length = %d", len(commands))
			}
			for index, command := range botCommands {
				want := telegram.BotCommand{Command: command.Command, Description: i18n.Text(language, command.Description)}
				if commands[index] != want {
					t.Fatalf("Bridge command = %#v, want %#v", commands[index], want)
				}
			}
			byName := make(map[string]string, len(native))
			for _, command := range commands[len(botCommands):] {
				byName[command.Command] = command.Description
			}
			for _, command := range native {
				want := command.Description
				if want == "" {
					want = "OMP native command"
					if language == i18n.Chinese {
						want = "OMP 原生命令"
					}
				}
				if got := byName[command.Command]; got != want {
					t.Fatalf("native command %q = %q, want %q", command.Command, got, want)
				}
			}
		}
		for index, command := range native {
			if index == 0 && command.Description != "" {
				t.Fatal("publication mutated the native catalog")
			}
		}
	}
}

func requireCommandMenuLanguage(t *testing.T, f *commandMenuTransport, chatID int64, language i18n.Language) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, code := range []string{"", "zh"} {
		found := false
		for _, command := range f.menus[chatID][code] {
			if command.Command == "help" {
				found = true
				if command.Description != i18n.Text(language, i18n.Msg.CommandHelp) {
					t.Fatalf("menu chat=%d language=%q did not converge to %s: %q", chatID, code, language, command.Description)
				}
			}
		}
		if !found {
			t.Fatalf("menu chat=%d language=%q is missing bridge commands", chatID, code)
		}
	}
}

func TestCommandMenuRetriesPersistedLanguageWithoutUpdates(t *testing.T) {
	for _, chatID := range []int64{0, -10} {
		for _, code := range []string{"", "zh"} {
			t.Run(fmt.Sprintf("chat=%d/language=%q", chatID, code), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					b, f := newCommandMenuFixture(t, -10)
					db, err := store.Open(t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { db.Close() })
					b.db = db
					b.refreshLanguageMenus()
					startCommandMenuPublisher(t, b)
					synctest.Wait()
					requireCommandMenuLanguage(t, f, 0, i18n.English)
					requireCommandMenuLanguage(t, f, -10, i18n.English)
					failed := commandMenuScope{chatID, code}
					f.mu.Lock()
					f.scopeFailures[failed] = 1
					f.failureStatus = http.StatusTooManyRequests
					f.retryAfter = 90
					f.mu.Unlock()
					if err := b.changeLanguage(i18n.Chinese); err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
					if language, err := db.Language(); err != nil || language != i18n.Chinese {
						t.Fatalf("language was not persisted: %s, %v", language, err)
					}
					f.mu.Lock()
					before := len(f.calls)
					f.mu.Unlock()
					time.Sleep(90*time.Second - time.Nanosecond)
					synctest.Wait()
					f.mu.Lock()
					calls := len(f.calls)
					f.mu.Unlock()
					if calls != before {
						t.Fatal("menu retry ignored Telegram retry_after")
					}
					time.Sleep(time.Nanosecond)
					synctest.Wait()
					requireCommandMenuLanguage(t, f, 0, i18n.Chinese)
					requireCommandMenuLanguage(t, f, -10, i18n.Chinese)
					f.mu.Lock()
					defer f.mu.Unlock()
					counts := make(map[commandMenuScope]int)
					for _, request := range f.calls {
						counts[commandMenuScope{request.Scope.ChatID, request.Language}]++
					}
					for _, id := range []int64{0, -10} {
						for _, language := range []string{"", "zh"} {
							scope := commandMenuScope{id, language}
							want := 2
							if scope == failed {
								want++
							}
							if counts[scope] != want {
								t.Fatalf("scope %+v sent %d requests, want %d", scope, counts[scope], want)
							}
						}
					}
				})
			})
		}
	}
}

func commandMenuCallCount(f *commandMenuTransport, scope commandMenuScope) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, request := range f.calls {
		if request.Scope.ChatID == scope.chatID && request.Language == scope.language {
			count++
		}
	}
	return count
}

func TestCommandMenuRetryBackoffBoundsTrafficAndLogs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, f := newCommandMenuFixture(t, -10)
		capture := &logCapture{}
		logs, err := logging.New(capture, logging.Options{Level: "info", Format: "json"})
		if err != nil {
			t.Fatal(err)
		}
		b.telegramLog = logs.Logger(logging.Telegram)
		failed := commandMenuScope{-10, ""}
		f.scopeFailures[failed] = 100
		f.failureStatus = http.StatusTooManyRequests
		cancel, done := startCommandMenuPublisher(t, b)
		synctest.Wait()
		// A failed chat must not delay unrelated menus.
		b.registerCommandMenuWorker(&worker{key: target{chat: -20, thread: 1}})
		synctest.Wait()
		requireCommandMenuLanguage(t, f, -20, i18n.English)
		for attempt, delay := range []time.Duration{
			time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
			16 * time.Second, 32 * time.Second, time.Minute, time.Minute,
		} {
			for range 20 {
				b.refreshLanguageMenus()
			}
			synctest.Wait()
			time.Sleep(delay - time.Nanosecond)
			synctest.Wait()
			if calls := commandMenuCallCount(f, failed); calls != attempt+1 {
				t.Fatalf("attempt %d bypassed backoff: %d requests", attempt, calls)
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			if calls := commandMenuCallCount(f, failed); calls != attempt+2 {
				t.Fatalf("attempt %d did not retry after %s: %d requests", attempt, delay, calls)
			}
		}
		if warnings := strings.Count(capture.String(), "telegram_chat_commands_failed"); warnings != 1 {
			t.Fatalf("one continuous failure produced %d warnings", warnings)
		}
		cancel()
		<-done
		calls := commandMenuCallCount(f, failed)
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if commandMenuCallCount(f, failed) != calls {
			t.Fatal("cancellation left a menu retry running")
		}
	})
}

func TestCommandMenuRetryUsesLatestCatalogAndLanguage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, f := newCommandMenuFixture(t, -10)
		w := &worker{key: target{chat: -10, thread: 1}}
		b.registerCommandMenuWorker(w)
		b.updateCommandMenu(w, commandMenuCommands("initial"))
		startCommandMenuPublisher(t, b)
		synctest.Wait()
		f.mu.Lock()
		f.scopeFailures[commandMenuScope{-10, "zh"}] = 1
		f.failureStatus = http.StatusTooManyRequests
		f.retryAfter = 5
		f.mu.Unlock()
		b.setLanguage(i18n.Chinese)
		b.refreshLanguageMenus()
		synctest.Wait()
		b.updateCommandMenu(w, commandMenuCommands("latest"))
		b.setLanguage(i18n.English)
		b.refreshLanguageMenus()
		synctest.Wait()
		f.mu.Lock()
		before := len(f.calls)
		f.mu.Unlock()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		requireCommandMenuLanguage(t, f, -10, i18n.English)
		requirePublishedMenus(t, f, -10, "latest")
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.calls)-before != 2 {
			t.Fatalf("retry sent %d requests, want both scopes of only the deferred chat", len(f.calls)-before)
		}
		for _, request := range f.calls[before:] {
			if request.Scope.ChatID != -10 {
				t.Fatalf("retry rewrote an unrelated chat: %d", request.Scope.ChatID)
			}
			requireCommandMenu(t, request.Commands, "latest")
		}
	})
}

func TestCommandMenuPermanentRejectionWaitsForChanges(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b, f := newCommandMenuFixture(t, -10)
				w := &worker{key: target{chat: -10, thread: 1}}
				b.registerCommandMenuWorker(w)
				failed := commandMenuScope{-10, ""}
				f.scopeFailures[failed] = 100
				f.failureStatus = status
				startCommandMenuPublisher(t, b)
				synctest.Wait()
				time.Sleep(10 * time.Minute)
				synctest.Wait()
				if calls := commandMenuCallCount(f, failed); calls != 1 {
					t.Fatalf("permanent HTTP %d rejection retried without an update: %d requests", status, calls)
				}
				b.updateCommandMenu(w, commandMenuCommands("changed"))
				synctest.Wait()
				if calls := commandMenuCallCount(f, failed); calls != 2 {
					t.Fatalf("new catalog did not retry HTTP %d rejection: %d requests", status, calls)
				}
			})
		})
	}
}

func TestCommandMenuRetriesUnconfirmedPublication(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		lostResponses int
		rejections    int
	}{
		{"lost responses", 3, 0},
		{"temporary rejection", 0, 3},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b, f := newCommandMenuFixture(t, -10)
				w := &worker{key: target{chat: -10, thread: 1}}
				b.registerCommandMenuWorker(w)
				b.updateCommandMenu(w, commandMenuCommands("initial"))
				startCommandMenuPublisher(t, b)
				synctest.Wait()
				f.mu.Lock()
				f.lostResponses["zh"] = scenario.lostResponses
				f.scopeFailures[commandMenuScope{-10, "zh"}] = scenario.rejections
				f.failureStatus = http.StatusServiceUnavailable
				f.mu.Unlock()
				b.updateCommandMenu(w, commandMenuCommands("latest"))
				time.Sleep(600 * time.Millisecond)
				synctest.Wait()
				failed := commandMenuScope{-10, "zh"}
				if calls := commandMenuCallCount(f, failed); calls != 4 {
					t.Fatalf("expected initial publication and three failed client attempts, got %d", calls)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				requirePublishedMenus(t, f, -10, "latest")
				if calls := commandMenuCallCount(f, failed); calls != 5 {
					t.Fatalf("publisher did not retry after client retries were exhausted: %d requests", calls)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if calls := commandMenuCallCount(f, failed); calls != 5 {
					t.Fatal("acknowledged menu publication kept retrying")
				}
			})
		})
	}
}
