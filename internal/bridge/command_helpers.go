package bridge

import (
	"context"
	"log/slog"
	"strings"

	"omp-telegram/internal/telegram"
)

func parseSlashCommandToken(token, botUsername string) (string, bool) {
	name, target, addressed := strings.Cut(token, "@")
	if !addressed || len(name) < 2 || name[0] != '/' || !telegramCommandIdentifier(name[1:]) || len(target) < 5 || !telegramCommandIdentifier(target) {
		return token, false
	}
	return name, !strings.EqualFold(target, botUsername)
}

func telegramCommandIdentifier(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func knownBridgeCommand(command string) bool {
	if command == "/start" {
		return true
	}
	if !strings.HasPrefix(command, "/") {
		return false
	}
	for _, known := range botCommands {
		if command[1:] == known.Command {
			return true
		}
	}
	return false
}

func commandHelp() string {
	var help strings.Builder
	for i, command := range botCommands {
		if i > 0 {
			help.WriteByte('\n')
		}
		help.WriteByte('/')
		help.WriteString(command.Command)
		help.WriteString(" - ")
		help.WriteString(command.Description)
	}
	return help.String()
}

func logTelegramFailure(logger *slog.Logger, level slog.Level, event, message string, err error, extra ...slog.Attr) {
	info := telegram.ClassifyError(err)
	attrs := []slog.Attr{
		slog.String("event", event),
		slog.String("reason", info.Reason),
		slog.Bool("uncertain", info.Uncertain),
	}
	if info.Code != 0 {
		attrs = append(attrs, slog.Int("api_code", info.Code))
	}
	if info.RetryAfter > 0 {
		attrs = append(attrs, slog.Int("retry_after_s", info.RetryAfter))
	}
	attrs = append(attrs, extra...)
	logger.LogAttrs(context.Background(), level, message, attrs...)
}
