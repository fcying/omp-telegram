package logging

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCompactTextRendering(t *testing.T) {
	var output bytes.Buffer
	registry, err := New(&output, Options{Level: "info", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 9, 19, 13, 20, 1, 0, time.UTC)
	record := slog.NewRecord(stamp, slog.LevelWarn, "telegram poll failed", 0)
	record.AddAttrs(
		slog.String("event", "poll_failed"), slog.String("reason", "timeout"),
		slog.Int("attempt", -2), slog.Uint64("bytes", 18446744073709551615),
		slog.Float64("ratio", 1.25), slog.Bool("retry", true),
		slog.Duration("elapsed", 1500*time.Millisecond), slog.Time("next", stamp),
		slog.Any("error", fmt.Errorf("remote failure")), slog.String("empty", ""),
	)
	if err := registry.Logger(Telegram).Handler().Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	want := "2026-09-19 13:20:01 WARN [telegram] telegram poll failed event=poll_failed reason=timeout attempt=-2 bytes=18446744073709551615 ratio=1.25 retry=true elapsed=1.5s next=2026-09-19T13:20:01Z error=\"remote failure\" empty=\"\"\n"
	if got := output.String(); got != want {
		t.Fatalf("compact record = %q, want %q", got, want)
	}
}

func TestCompactTextZeroTimeAndInvalidUTF8(t *testing.T) {
	var output bytes.Buffer
	registry, err := New(&output, Options{Level: "info", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	record := slog.NewRecord(time.Time{}, slog.LevelInfo, "invalid\xff\nmessage", 0)
	record.AddAttrs(slog.String("key\xfe", "value\xff\n"))
	if err := registry.Logger(Bridge).Handler().Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	want := `INFO [bridge] invalid\xff\nmessage "key\xfe"="value\xff\n"` + "\n"
	if got := output.String(); got != want || !utf8.ValidString(got) {
		t.Fatalf("invalid-byte record = %q, want %q", got, want)
	}
}

func TestCompactTextGroupsAndDerivedIsolation(t *testing.T) {
	var output bytes.Buffer
	registry, err := New(&output, Options{Level: "info", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	base := registry.Logger(RPC).With("root", 1).WithGroup("request").With("id", 2)
	left := base.WithGroup("nested").With("side", "left")
	right := base.With("side", "right")
	left.Info("left", slog.Group("", slog.String("inline", "yes")), slog.Group("empty"), slog.Attr{}, slog.Group("deep", slog.Int("n", 3)))
	right.WithGroup("").Info("right", "tail", 4)
	base.Info("base")
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	want := []string{
		" INFO [rpc] left root=1 request.id=2 request.nested.side=left request.nested.inline=yes request.nested.deep.n=3",
		" INFO [rpc] right root=1 request.id=2 request.side=right request.tail=4",
		" INFO [rpc] base root=1 request.id=2",
	}
	if len(lines) != len(want) {
		t.Fatalf("records = %q", output.String())
	}
	for i := range want {
		if !strings.HasSuffix(lines[i], want[i]) {
			t.Errorf("record %d = %q, want suffix %q", i, lines[i], want[i])
		}
	}
}

type textLogValue func() slog.Value

func (value textLogValue) LogValue() slog.Value { return value() }

func TestCompactTextResolvedValuesAndEscaping(t *testing.T) {
	var output bytes.Buffer
	registry, err := New(&output, Options{Level: "info", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	logger := registry.Logger(Bridge)
	calls := 0
	bound := logger.With("bound", textLogValue(func() slog.Value {
		calls++
		return slog.StringValue("saved\nvalue")
	}))
	value := textLogValue(func() slog.Value {
		logger.Info("nested") // Resolution must not hold the shared writer lock.
		return slog.GroupValue(slog.String("key\n\x1b", "value\r\t\x00\u2028"))
	})
	bound.WithGroup("g\n").Info("message\n\r\x1b\u2028", slog.Any("resolved", value))
	bound.Info("again")
	if calls != 1 {
		t.Fatalf("bound LogValuer resolved %d times, want once", calls)
	}
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("unsafe record boundaries: %q", output.String())
	}
	want := ` INFO [bridge] message\n\r\x1b\u2028 bound="saved\nvalue" "g\n.resolved.key\n\x1b"="value\r\t\x00\u2028"`
	if !strings.HasSuffix(lines[0], " INFO [bridge] nested") || !strings.HasSuffix(lines[1], want) || !strings.HasSuffix(lines[2], ` INFO [bridge] again bound="saved\nvalue"`) {
		t.Fatalf("resolved/escaped records = %q", output.String())
	}
}

func TestCompactTextDifferentComponentsConcurrent(t *testing.T) {
	var output bytes.Buffer
	registry, err := New(&output, Options{Level: "info", Format: "text", ComponentLevels: map[string]string{"rpc": "debug", "telegram": "warn"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.Logger(Bridge).Debug("hidden")
	registry.Logger(Telegram).WithGroup("request").Info("hidden")
	const count = 32
	var workers sync.WaitGroup
	for _, component := range components {
		workers.Add(1)
		go func(component Component) {
			defer workers.Done()
			logger := registry.Logger(component).With("owner", string(component))
			level := slog.LevelInfo
			if component == RPC {
				level = slog.LevelDebug
			} else if component == Telegram {
				level = slog.LevelWarn
			}
			for n := range count {
				logger.WithGroup("work").Log(context.Background(), level, "component event", "record", n)
			}
		}(component)
	}
	workers.Wait()
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != len(components)*count {
		t.Fatalf("record count = %d, want %d", len(lines), len(components)*count)
	}
	seen := make(map[string]bool)
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 8 {
			t.Fatalf("interleaved record: %q", line)
		}
		component := strings.Trim(fields[3], "[]")
		if fields[4] != "component" || fields[5] != "event" || fields[6] != "owner="+component {
			t.Fatalf("mixed component fields: %q", line)
		}
		identity := component + "/" + fields[7]
		if seen[identity] {
			t.Fatalf("duplicate record: %q", line)
		}
		seen[identity] = true
	}
	for _, component := range components {
		for n := range count {
			if !seen[fmt.Sprintf("%s/work.record=%d", component, n)] {
				t.Errorf("missing %s record %d", component, n)
			}
		}
	}
}
