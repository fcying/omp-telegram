package bridge

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"omp-telegram/internal/telegram"
)

const commandMenuTimeout = 20 * time.Second
const nativeCommandDescription = "OMP native command"

type commandMenuOwner struct {
	worker *worker
	active bool
	names  []string
}

type commandMenuChat struct {
	dirty     bool
	published [2][]telegram.BotCommand
}

// commandMenuState is initialized under mu, since catalog updates can arrive
// before the publisher starts. Only the publisher performs Telegram requests.
type commandMenuState struct {
	mu      sync.Mutex
	wake    chan struct{}
	owners  map[target]commandMenuOwner
	chats   map[int64]*commandMenuChat
	pending []int64
	head    int
}

func (s *commandMenuState) initLocked() {
	if s.wake != nil {
		return
	}
	s.wake = make(chan struct{}, 1)
	s.owners = make(map[target]commandMenuOwner)
	s.chats = make(map[int64]*commandMenuChat)
}

func (s *commandMenuState) dirtyLocked(chatID int64) {
	chat := s.chats[chatID]
	if chat == nil {
		chat = &commandMenuChat{}
		s.chats[chatID] = chat
	}
	if !chat.dirty {
		chat.dirty = true
		s.pending = append(s.pending, chatID)
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (b *Bridge) registerCommandMenuWorker(w *worker) {
	s := &b.commandMenus
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	s.owners[w.key] = commandMenuOwner{worker: w, active: true}
	s.dirtyLocked(w.key.chat)
}

func telegramNativeCommandName(name string) bool {
	if !telegramMenuCommandName(name) || name == "start" {
		return false
	}
	for _, command := range botCommands {
		if name == command.Command {
			return false
		}
	}
	return unsupportedNativeCommand("/"+name, "") == ""
}

func (b *Bridge) updateCommandMenu(w *worker, names []string) {
	// Own a normalized copy; the actor may reuse its catalog's storage.
	var valid []string
	for _, name := range names {
		if telegramNativeCommandName(name) {
			valid = append(valid, name)
		}
	}
	sort.Strings(valid)
	valid = slices.Compact(valid)

	s := &b.commandMenus
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	owner, ok := s.owners[w.key]
	if !ok || owner.worker != w || !owner.active {
		return
	}
	owner.names = valid
	s.owners[w.key] = owner
	// Successful lists are cached by the publisher. Repeated catalogs also
	// allow an earlier failed publication to be attempted again.
	s.dirtyLocked(w.key.chat)
}

func (b *Bridge) removeCommandMenu(w *worker) {
	s := &b.commandMenus
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	owner, ok := s.owners[w.key]
	if !ok || owner.worker != w || !owner.active {
		return
	}
	// Keep an inactive tombstone instead of retaining the retired actor. Late
	// results cannot revive it without an explicit registration.
	s.owners[w.key] = commandMenuOwner{}
	s.dirtyLocked(w.key.chat)
}

func (s *commandMenuState) nextLocked() (int64, []telegram.BotCommand, bool) {
	if s.head == len(s.pending) {
		return 0, nil, false
	}
	chatID := s.pending[s.head]
	s.head++
	if s.head == len(s.pending) {
		s.pending = s.pending[:0]
		s.head = 0
	} else if s.head >= 64 && s.head >= len(s.pending)/2 {
		// Reclaim consumed slots even if continuous updates keep the queue busy.
		s.pending = s.pending[:copy(s.pending, s.pending[s.head:])]
		s.head = 0
	}
	s.chats[chatID].dirty = false

	union := make(map[string]struct{})
	for key, owner := range s.owners {
		if key.chat == chatID && owner.active {
			for _, name := range owner.names {
				union[name] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(union))
	for name := range union {
		names = append(names, name)
	}
	sort.Strings(names)
	commands := make([]telegram.BotCommand, 0, min(100, len(botCommands)+len(names)))
	commands = append(commands, botCommands[:min(100, len(botCommands))]...)
	for _, name := range names {
		if len(commands) == 100 {
			break
		}
		commands = append(commands, telegram.BotCommand{Command: name, Description: nativeCommandDescription})
	}
	return chatID, commands, true
}

func (b *Bridge) runCommandMenus(ctx context.Context) {
	s := &b.commandMenus
	s.mu.Lock()
	s.initLocked()
	// Reset persisted chat scopes on startup without discarding catalogs that
	// already arrived. No worker or OMP process is needed for this reset.
	for _, chatID := range b.cfg.AllowedChats {
		s.dirtyLocked(chatID)
	}
	wake := s.wake
	s.mu.Unlock()

	for {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		chatID, commands, ok := s.nextLocked()
		s.mu.Unlock()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			continue
		}
		for i, language := range [...]string{"", "zh"} {
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			unchanged := slices.Equal(s.chats[chatID].published[i], commands)
			s.mu.Unlock()
			if unchanged {
				continue
			}
			requestCtx, cancel := context.WithTimeout(ctx, commandMenuTimeout)
			err := b.tg.SetChatCommands(requestCtx, chatID, commands, language)
			cancel()
			if err != nil {
				if telegram.DeliveryUncertain(err) {
					// The remote menu may have changed despite the failed response.
					s.mu.Lock()
					s.chats[chatID].published[i] = nil
					s.mu.Unlock()
				}
				if ctx.Err() == nil && b.telegramLog != nil {
					logTelegramFailure(b.telegramLog, slog.LevelWarn, "telegram_chat_commands_failed", "telegram chat command registration failed", err, slog.Int64("chat_id", chatID), slog.String("language_code", language))
				}
				continue
			}
			s.mu.Lock()
			s.chats[chatID].published[i] = commands
			s.mu.Unlock()
		}
	}
}
