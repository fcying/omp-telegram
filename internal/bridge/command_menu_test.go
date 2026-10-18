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
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/logging"
	"omp-telegram/internal/omp"
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

func commandMenuCommands(names ...string) []telegram.BotCommand {
	commands := make([]telegram.BotCommand, 0, len(names))
	for _, name := range names {
		commands = append(commands, telegram.BotCommand{Command: name})
	}
	return commands
}

func requireCommandMenu(t *testing.T, commands []telegram.BotCommand, native ...string) {
	t.Helper()
	commandsWithDescriptions := commandMenuCommands(native...)
	for i := range commandsWithDescriptions {
		commandsWithDescriptions[i].Description = nativeCommandDescription
	}
	requireDescribedCommandMenu(t, commands, commandsWithDescriptions...)
}

func requireDescribedCommandMenu(t *testing.T, commands []telegram.BotCommand, native ...telegram.BotCommand) {
	t.Helper()
	want := append(slices.Clone(botCommands), native...)
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
