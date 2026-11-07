package bridge

import (
	"strings"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/store"
)

// dispatchShortcut reuses the accepted callback inbox and ordinary command flows.
func (w *worker) dispatchShortcut(in incoming, command string) {
	q := in.callback
	if q == nil || q.Message == nil || !w.b.cfg.Authorized(q.From.ID, q.Message.Chat.ID) ||
		q.Message.Chat.ID != w.key.chat || q.Message.MessageThreadID != w.key.thread {
		w.mark(in.id, store.InboxCancelled)
		return
	}
	message := *q.Message
	message.From = &q.From
	message.Text = strings.TrimSpace(command)
	message.ReplyToMessage = nil
	message.Quote = nil
	in.msg = &message
	in.callback = nil
	if w.handleMigrationIncoming(in) {
		return
	}
	name, _, foreign := parseSlashInvocation(message.Text, w.b.bot.Username)
	if foreign || name == "" {
		w.mark(in.id, store.InboxCancelled)
		w.say(w.tr(i18n.Msg.ActionUnavailable))
		return
	}
	if knownBridgeCommand(name) {
		w.handleSlash(in, message.Text)
		return
	}
	w.routeSlashInput(in, message.Text, true)
}
