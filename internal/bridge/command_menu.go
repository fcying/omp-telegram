package bridge

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/telegram"
)

const (
	commandMenuTimeout      = 20 * time.Second
	commandMenuRetryInitial = time.Second
	commandMenuRetryMax     = time.Minute
)

type commandMenuOwner struct {
	worker   *worker
	active   bool
	known    bool
	commands []telegram.BotCommand
}

type commandMenuChat struct {
	dirty      bool
	published  [2][]telegram.BotCommand
	retryAt    time.Time
	retryDelay time.Duration
}

// commandMenuState is initialized under mu, since catalog updates can arrive
// before the publisher starts. Only the publisher performs Telegram requests.
type commandMenuState struct {
	mu        sync.Mutex
	wake      chan struct{}
	owners    map[target]commandMenuOwner
	chats     map[int64]*commandMenuChat
	bootstrap []telegram.BotCommand
	pending   []int64
	head      int
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

// Discover once before starting workers, without creating a native session or
// occupying a worker slot. Only builtins are safe to advertise across workspaces.
func (b *Bridge) bootstrapCommandMenus(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, commandMenuTimeout)
	defer cancel()
	catalog, err := omp.DiscoverCommands(ctx, omp.Config{
		Binary: b.cfg.OMP, CWD: b.cfg.WorkspaceRoot,
		Args: b.selectedArgsSnapshot(), Environment: b.cfg.OMPEnvironment,
	}, b.rpcLog)
	if err != nil {
		if b.log != nil && ctx.Err() != context.Canceled {
			b.log.Warn("startup command discovery unavailable", "event", "command_menu_bootstrap_failed", "error_kind", omp.ClassifyError(err))
		}
		return
	}
	var commands []telegram.BotCommand
	for name := range catalog.ExecutableCommands {
		if catalog.HasBuiltinCommand(name) && telegramNativeCommandName(name) {
			commands = append(commands, telegram.BotCommand{Command: name, Description: catalog.CommandDescription(name)})
		}
	}
	s := &b.commandMenus
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	s.bootstrap = commands
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

func (b *Bridge) updateCommandMenu(w *worker, commands []telegram.BotCommand) {
	// Own a filtered copy; the actor may reuse its catalog's storage.
	valid := make([]telegram.BotCommand, 0, len(commands))
	for _, command := range commands {
		if telegramNativeCommandName(command.Command) {
			valid = append(valid, command)
		}
	}

	s := &b.commandMenus
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	owner, ok := s.owners[w.key]
	if !ok || owner.worker != w || !owner.active {
		return
	}
	owner.commands = valid
	owner.known = true
	s.owners[w.key] = owner
	// Successful lists are cached by the publisher. Repeated catalogs also
	// allow an earlier failed publication to be attempted again.
	s.dirtyLocked(w.key.chat)
}

func (b *Bridge) invalidateCommandMenu(w *worker) {
	s := &b.commandMenus
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	owner, ok := s.owners[w.key]
	if !ok || owner.worker != w || !owner.active || !owner.known {
		return
	}
	owner.known = false
	owner.commands = nil
	s.owners[w.key] = owner
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

func (b *Bridge) refreshLanguageMenus() {
	s := &b.commandMenus
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	// Chat ID zero is reserved for the global default command menu.
	s.dirtyLocked(0)
	for chatID := range s.chats {
		s.dirtyLocked(chatID)
	}
	for _, chatID := range b.cfg.AllowedChats {
		s.dirtyLocked(chatID)
	}
}

func (s *commandMenuState) nextLocked(now time.Time) (int64, []telegram.BotCommand, bool, time.Time) {
	var chatID int64
	var retryAt time.Time
	ready := false
	// Scan each queued chat once; deferred failures must not block healthy chats.
	for range len(s.pending) - s.head {
		chatID = s.pending[s.head]
		s.head++
		if s.head == len(s.pending) {
			s.pending = s.pending[:0]
			s.head = 0
		} else if s.head >= 64 && s.head >= len(s.pending)/2 {
			// Reclaim consumed slots even if continuous updates keep the queue busy.
			s.pending = s.pending[:copy(s.pending, s.pending[s.head:])]
			s.head = 0
		}
		chat := s.chats[chatID]
		if now.Before(chat.retryAt) {
			s.pending = append(s.pending, chatID)
			if retryAt.IsZero() || chat.retryAt.Before(retryAt) {
				retryAt = chat.retryAt
			}
			continue
		}
		chat.dirty = false
		ready = true
		break
	}
	if !ready {
		return 0, nil, false, retryAt
	}
	if chatID == 0 {
		return 0, nil, true, time.Time{}
	}

	union := make(map[string]string)
	known := false
	for key, owner := range s.owners {
		if key.chat == chatID && owner.active && owner.known {
			known = true
			for _, command := range owner.commands {
				previous := union[command.Command]
				if previous == "" || command.Description != "" && command.Description < previous {
					union[command.Command] = command.Description
				}
			}
		}
	}
	if !known {
		for _, command := range s.bootstrap {
			union[command.Command] = command.Description
		}
	}
	names := make([]string, 0, len(union))
	for name := range union {
		names = append(names, name)
	}
	sort.Strings(names)
	commands := make([]telegram.BotCommand, 0, min(max(0, 100-len(botCommands)), len(names)))
	for _, name := range names {
		if len(commands)+len(botCommands) >= 100 {
			break
		}
		commands = append(commands, telegram.BotCommand{Command: name, Description: union[name]})
	}
	return chatID, commands, true, time.Time{}
}

func (b *Bridge) runCommandMenus(ctx context.Context) {
	s := &b.commandMenus
	s.mu.Lock()
	s.initLocked()
	// Replace persisted chat scopes with the bootstrap or authoritative catalogs.
	// Bootstrap discovery has finished and released its process before we start.
	for _, chatID := range b.cfg.AllowedChats {
		s.dirtyLocked(chatID)
	}
	wake := s.wake
	s.mu.Unlock()
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		chatID, commands, ok, retryAt := s.nextLocked(time.Now())
		reportFailure := ok && s.chats[chatID].retryDelay == 0
		s.mu.Unlock()
		if !ok {
			var retry <-chan time.Time
			if !retryAt.IsZero() {
				if timer == nil {
					timer = time.NewTimer(time.Until(retryAt))
				} else {
					timer.Reset(time.Until(retryAt))
				}
				retry = timer.C
			}
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-retry:
			}
			if timer != nil {
				timer.Stop()
			}
			continue
		}
		localized := localizedCommands(commands, b.language())
		failed, retry := false, false
		var retryAfter time.Duration
		for i, language := range [...]string{"", "zh"} {
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			unchanged := slices.Equal(s.chats[chatID].published[i], localized)
			s.mu.Unlock()
			if unchanged {
				continue
			}
			requestCtx, cancel := context.WithTimeout(ctx, commandMenuTimeout)
			var err error
			if chatID == 0 {
				err = b.tg.SetCommands(requestCtx, localized, language)
			} else {
				err = b.tg.SetChatCommands(requestCtx, chatID, localized, language)
			}
			cancel()
			if err != nil {
				failed = true
				info := telegram.ClassifyError(err)
				if info.Uncertain || info.Reason == "timeout" {
					retry = true
				}
				if info.Code == 429 || info.Code >= 500 && info.Code <= 599 {
					retry = true
				}
				if info.RetryAfter > 0 {
					seconds := min(int64(info.RetryAfter), int64((1<<63-1)/time.Second))
					retryAfter = max(retryAfter, time.Duration(seconds)*time.Second)
				}
				if telegram.DeliveryUncertain(err) {
					// The remote menu may have changed despite the failed response.
					s.mu.Lock()
					s.chats[chatID].published[i] = nil
					s.mu.Unlock()
				}
				if reportFailure && ctx.Err() == nil && b.telegramLog != nil {
					logTelegramFailure(b.telegramLog, slog.LevelWarn, "telegram_chat_commands_failed", "telegram chat command registration failed", err, slog.Int64("chat_id", chatID), slog.String("language_code", language))
				}
				continue
			}
			s.mu.Lock()
			s.chats[chatID].published[i] = localized
			s.mu.Unlock()
		}
		s.mu.Lock()
		chat := s.chats[chatID]
		if failed {
			if chat.retryDelay == 0 {
				chat.retryDelay = commandMenuRetryInitial
			} else {
				chat.retryDelay = min(chat.retryDelay*2, commandMenuRetryMax)
			}
			chat.retryAt = time.Now().Add(max(chat.retryDelay, retryAfter))
			if retry {
				s.dirtyLocked(chatID)
			}
		} else {
			chat.retryDelay = 0
			chat.retryAt = time.Time{}
		}
		s.mu.Unlock()
	}
}
