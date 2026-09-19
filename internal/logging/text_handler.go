package logging

import (
	"context"
	"encoding"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"
)

// Derived handlers share only immutable formatting state and the synchronized
// writer. Values are resolved before Write, so a LogValuer may itself log.
type compactTextHandler struct {
	writer    io.Writer
	level     slog.Level
	component Component
	prefix    string
	bound     string
}

func (h *compactTextHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *compactTextHandler) Handle(_ context.Context, record slog.Record) error {
	buf := make([]byte, 0, 128+len(h.bound)+len(record.Message))
	if !record.Time.IsZero() {
		buf = record.Time.AppendFormat(buf, "2006-01-02 15:04:05")
		buf = append(buf, ' ')
	}
	buf = append(buf, record.Level.String()...)
	buf = append(buf, " ["...)
	buf = append(buf, h.component...)
	buf = append(buf, "] "...)
	buf = appendTextMessage(buf, record.Message)
	buf = append(buf, h.bound...)
	record.Attrs(func(attr slog.Attr) bool {
		buf = appendTextAttr(buf, h.prefix, attr)
		return true
	})
	buf = append(buf, '\n')
	n, err := h.writer.Write(buf)
	if err == nil && n != len(buf) {
		return io.ErrShortWrite
	}
	return err
}

func (h *compactTextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	derived := *h
	var buf []byte
	for _, attr := range attrs {
		buf = appendTextAttr(buf, h.prefix, attr)
	}
	derived.bound += string(buf)
	return &derived
}

func (h *compactTextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	derived := *h
	derived.prefix += name + "."
	return &derived
}

func appendTextAttr(buf []byte, prefix string, attr slog.Attr) []byte {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return buf
	}
	if attr.Value.Kind() == slog.KindGroup {
		if attr.Key != "" {
			prefix += attr.Key + "."
		}
		for _, child := range attr.Value.Group() {
			buf = appendTextAttr(buf, prefix, child)
		}
		return buf
	}
	buf = append(buf, ' ')
	buf = appendTextToken(buf, prefix+attr.Key)
	buf = append(buf, '=')
	value := attr.Value
	switch value.Kind() {
	case slog.KindString:
		return appendTextToken(buf, value.String())
	case slog.KindBool:
		return strconv.AppendBool(buf, value.Bool())
	case slog.KindInt64:
		return strconv.AppendInt(buf, value.Int64(), 10)
	case slog.KindUint64:
		return strconv.AppendUint(buf, value.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.AppendFloat(buf, value.Float64(), 'g', -1, 64)
	case slog.KindDuration:
		return appendTextToken(buf, value.Duration().String())
	case slog.KindTime:
		return value.Time().AppendFormat(buf, time.RFC3339Nano)
	default:
		if marshaler, ok := value.Any().(encoding.TextMarshaler); ok {
			text, err := marshaler.MarshalText()
			if err != nil {
				return appendTextToken(buf, "!ERROR:"+err.Error())
			}
			return appendTextToken(buf, string(text))
		}
		return appendTextToken(buf, fmt.Sprint(value.Any()))
	}
}

func appendTextToken(buf []byte, value string) []byte {
	quote := value == ""
	for offset := 0; offset < len(value); {
		r, size := utf8.DecodeRuneInString(value[offset:])
		offset += size
		if (r == utf8.RuneError && size == 1) || r == '=' || r == '"' || r == '\\' || unicode.IsSpace(r) || !strconv.IsPrint(r) {
			quote = true
			break
		}
	}
	if quote {
		return strconv.AppendQuote(buf, value)
	}
	return append(buf, value...)
}

// Keep ordinary messages verbatim, escaping only characters that could alter
// the terminal or introduce another physical record.
func appendTextMessage(buf []byte, message string) []byte {
	const hex = "0123456789abcdef"
	start := 0
	for offset := 0; offset < len(message); {
		r, size := utf8.DecodeRuneInString(message[offset:])
		if r == utf8.RuneError && size == 1 {
			buf = append(buf, message[start:offset]...)
			buf = append(buf, '\\', 'x', hex[message[offset]>>4], hex[message[offset]&15])
			start = offset + size
		} else if !strconv.IsPrint(r) {
			buf = append(buf, message[start:offset]...)
			quoteStart := len(buf)
			buf = strconv.AppendQuoteRune(buf, r)
			copy(buf[quoteStart:], buf[quoteStart+1:len(buf)-1])
			buf = buf[:len(buf)-2]
			start = offset + size
		}
		offset += size
	}
	return append(buf, message[start:]...)
}
