package bridge

import (
	"context"
	"log/slog"
	"strings"

	"omp-telegram/internal/telegram"
)

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
