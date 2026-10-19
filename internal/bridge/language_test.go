package bridge

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func latestLanguageOutput(t *testing.T, w *worker) string {
	t.Helper()
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox WHERE chat=? AND thread=? ORDER BY id DESC LIMIT 1", w.key.chat, w.key.thread).Scan(&text); err != nil {
		t.Fatal(err)
	}
	return text
}

func TestLanguageCommandDefaultsAndPersistenceFailure(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/help")
	if got := latestLanguageOutput(t, w); !strings.Contains(got, "/lang - Set global language") {
		t.Fatalf("unconfigured help is not English: %q", got)
	}
	command("/lang zh")
	command("/help")
	if got := latestLanguageOutput(t, w); !strings.Contains(got, "/lang - 设置全局语言") {
		t.Fatalf("help did not switch language: %q", got)
	}
	command("/lang invalid")
	language, err := w.b.db.Language()
	if err != nil || language != i18n.Chinese || w.b.language() != i18n.Chinese {
		t.Fatalf("invalid command changed language: %q, %v", language, err)
	}
	_, err = w.b.db.DB.Exec(`CREATE TRIGGER reject_language BEFORE UPDATE ON interface_language BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	if err != nil {
		t.Fatal(err)
	}
	command("/lang en")
	language, err = w.b.db.Language()
	if err != nil || language != i18n.Chinese || w.b.language() != i18n.Chinese {
		t.Fatalf("failed persistence changed language: %q, %v", language, err)
	}
	if got := latestLanguageOutput(t, w); !strings.Contains(got, "语言未更改") {
		t.Fatalf("failed persistence was not reported in the old language: %q", got)
	}
}

func TestGlobalLanguageReachesExistingWorkersAndSurvivesRestart(t *testing.T) {
	d := newRecoveryDaemon(t, 1)
	d.command(22, "/help")
	waitFor(t, func() bool { return d.fake.has(22, "/lang - Set global language") })
	d.command(11, "/lang zh")
	d.command(22, "/help")
	waitFor(t, func() bool { return d.fake.has(22, "/lang - 设置全局语言") })
	d.stop()
	d.start()
	d.command(33, "/help")
	waitFor(t, func() bool { return d.fake.has(33, "/lang - 设置全局语言") })
	d.command(33, "/lang en")
	d.command(11, "/help")
	waitFor(t, func() bool { return d.fake.has(11, "/lang - Set global language") })
	language, err := d.db.Language()
	if err != nil || language != i18n.English {
		t.Fatalf("global language = %q, %v", language, err)
	}
}

func TestLanguageCommandDuringTaskAndMigrationDoesNotSubmitWork(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.busy = true
	w.queue = []queued{{id: 900, text: "unchanged"}}
	command("/lang zh")
	if w.b.language() != i18n.Chinese || len(w.queue) != 1 || w.queue[0].text != "unchanged" || w.client != nil {
		t.Fatal("language command waited for or modified model work")
	}
	w.busy = false
	w.migrationRun = &migrationRun{id: "locked"}
	command("/lang en")
	if w.b.language() != i18n.English || len(w.queue) != 1 || w.migrationRun == nil || w.migrationRun.id != "locked" || w.client != nil {
		t.Fatal("language switch altered migration or submitted model work")
	}
	w.migrationRun = nil
	w.queue = nil
}

func TestConcurrentGlobalLanguageChangesKeepStorageAndRuntimeConsistent(t *testing.T) {
	w, _, _ := setupWorkspaceWorker(t)
	var writers sync.WaitGroup
	start := make(chan struct{})
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		writers.Add(1)
		go func() {
			defer writers.Done()
			<-start
			for range 20 {
				if err := w.b.changeLanguage(language); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	close(start)
	writers.Wait()
	saved, err := w.b.db.Language()
	if err != nil || saved != w.b.language() {
		t.Fatalf("concurrent language changes diverged: saved=%q runtime=%q err=%v", saved, w.b.language(), err)
	}
}

func TestGlobalLanguageRepublishesMenusWithoutChangingNativeDescriptions(t *testing.T) {
	b, transport := newCommandMenuFixture(t, -10)
	w := &worker{key: target{chat: -10, thread: 11}}
	b.registerCommandMenuWorker(w)
	b.updateCommandMenu(w, []telegram.BotCommand{{Command: "native_probe", Description: "Cancel 中文 on"}})
	startCommandMenuPublisher(t, b)
	for range 2 {
		request := receiveCommandMenu(t, transport.completed)
		for _, command := range request.Commands {
			if command.Command == "help" && command.Description != "Show usage help" {
				t.Fatalf("default menu is not English: %q", command.Description)
			}
		}
	}
	b.setLanguage(i18n.Chinese)
	b.refreshLanguageMenus()
	for range 4 {
		var request commandMenuRequest
		select {
		case request = <-transport.completed:
		case <-time.After(3 * time.Second):
			t.Fatal("global language did not refresh both default and chat menus")
		}
		for _, command := range request.Commands {
			switch command.Command {
			case "help":
				if command.Description != "显示使用帮助" {
					t.Fatalf("menu %s/%q did not switch globally: %q", request.Scope.Type, request.Language, command.Description)
				}
			case "native_probe":
				if command.Description != "Cancel 中文 on" {
					t.Fatalf("native description changed: %q", command.Description)
				}
			}
		}
	}
}

func TestGlobalLanguageMenusConvergeAfterBlockedPublicationAndRapidSwitches(t *testing.T) {
	b, transport := newCommandMenuFixture(t, -10, -20)
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	b.db = db
	w := &worker{b: b, key: target{chat: -10, thread: 11}}
	b.registerCommandMenuWorker(w)
	gate := make(chan struct{})
	transport.gate = gate
	startCommandMenuPublisher(t, b)
	first := receiveCommandMenu(t, transport.started)
	if first.Language != "" {
		t.Fatalf("first publication language scope = %q", first.Language)
	}
	changed := make(chan struct{})
	go func() {
		w.handleLanguageCommand("zh")
		close(changed)
	}()
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("language change blocked behind Telegram publication")
	}
	// A new chat queued after the refresh is a barrier for every older menu.
	waitForBarrier := func(chat int64) {
		t.Helper()
		b.registerCommandMenuWorker(&worker{key: target{chat: chat, thread: 1}})
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		seen := map[string]bool{}
		for len(seen) != 2 {
			select {
			case request := <-transport.completed:
				if request.Scope.Type == "chat" && request.Scope.ChatID == chat {
					seen[request.Language] = true
				}
			case <-timer.C:
				t.Fatal("publisher did not finish the queued language refresh")
			}
		}
	}
	requireLanguage := func(chats []int64, description string) {
		t.Helper()
		transport.mu.Lock()
		defer transport.mu.Unlock()
		for _, chat := range chats {
			for _, scope := range []string{"", "zh"} {
				found := false
				for _, command := range transport.menus[chat][scope] {
					if command.Command == "help" {
						found = true
						if command.Description != description {
							t.Fatalf("chat %d scope %q retained a stale language: %q", chat, scope, command.Description)
						}
					}
				}
				if !found {
					t.Fatalf("chat %d scope %q has no published help command", chat, scope)
				}
			}
		}
	}
	close(gate)
	waitForBarrier(-30)
	requireLanguage([]int64{0, -10, -20, -30}, "显示使用帮助")
	w.handleLanguageCommand("en")
	w.handleLanguageCommand("zh")
	w.handleLanguageCommand("en")
	waitForBarrier(-40)
	requireLanguage([]int64{0, -10, -20, -30, -40}, "Show usage help")
}

func TestErrorTextLocalizesOnlyTypedBridgeErrors(t *testing.T) {
	w := &worker{b: &Bridge{}}
	w.b.setLanguage(i18n.Chinese)
	if got := w.errorText(newMessageError(i18n.Msg.Queue.Full)); got != "队列已满. 此消息未提交." {
		t.Fatalf("typed Bridge error was not localized: %q", got)
	}
	// An opaque error can equal a Bridge template without becoming Bridge UI.
	raw := "The queue is full. This message was not submitted."
	if got := w.errorText(errors.New(raw)); got != raw {
		t.Fatalf("opaque error was translated by its English text: %q", got)
	}
}
