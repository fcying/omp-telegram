// Package logging provides the daemon's component-scoped structured loggers.
package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
)

// Component identifies a daemon subsystem whose records share a level policy.
type Component string

// Component names are intentionally fixed so configuration cannot create an
// unreviewed logging sink or metadata namespace.
const (
	Daemon   Component = "daemon"
	Bridge   Component = "bridge"
	RPC      Component = "rpc"
	Telegram Component = "telegram"
	Store    Component = "store"
	Media    Component = "media"
)

var components = [...]Component{
	Daemon,
	Bridge,
	RPC,
	Telegram,
	Store,
	Media,
}

var componentSet = map[Component]struct{}{
	Daemon:   {},
	Bridge:   {},
	RPC:      {},
	Telegram: {},
	Store:    {},
	Media:    {},
}

// Options controls the default and per-component logging levels.
//
// Level and Format must be explicit valid values. Configuration loading owns
// application defaults; New does not silently turn an empty option into one.
type Options struct {
	Level           string
	Format          string
	ComponentLevels map[string]string
}

// Registry contains one logger per supported component.
type Registry struct {
	loggers map[Component]*slog.Logger
}

// lockedWriter serializes writes from independent slog handlers. Keeping one
// lock around the shared writer prevents interleaved JSON or text records.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// ValidateComponent checks that name identifies one of the supported logging components.
// Unknown names are rejected with a fixed message so untrusted configuration keys are never echoed.
func ValidateComponent(name string) error {
	if _, ok := componentSet[Component(name)]; !ok {
		return errors.New("unknown log component")
	}
	return nil
}

// Validate checks the complete logging policy without exposing option values.
func Validate(opts Options) error {
	if !validLevel(opts.Level) {
		return errors.New("logging.level must be debug, info, warn, or error")
	}
	if opts.Format != "text" && opts.Format != "json" {
		return errors.New("logging.format must be text or json")
	}

	keys := make([]string, 0, len(opts.ComponentLevels))
	for component := range opts.ComponentLevels {
		keys = append(keys, component)
	}
	sort.Strings(keys)
	for _, component := range keys {
		if err := ValidateComponent(component); err != nil {
			return err
		}
	}
	for _, component := range keys {
		if !validLevel(opts.ComponentLevels[component]) {
			return fmt.Errorf("logging.component_levels.%s must be debug, info, warn, or error", component)
		}
	}
	return nil
}

func validLevel(level string) bool {
	switch level {
	case "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}

func slogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// New creates component loggers backed by compact text or standard JSON handlers.
// Every handler shares one synchronized writer, so records cannot interleave.
func New(w io.Writer, opts Options) (*Registry, error) {
	if w == nil {
		return nil, errors.New("logging writer must not be nil")
	}
	if err := Validate(opts); err != nil {
		return nil, err
	}

	shared := &lockedWriter{w: w}
	registry := &Registry{loggers: make(map[Component]*slog.Logger, len(components))}
	for _, component := range components {
		level := opts.Level
		if override, ok := opts.ComponentLevels[string(component)]; ok {
			level = override
		}
		handlerOptions := &slog.HandlerOptions{Level: slogLevel(level)}
		var handler slog.Handler
		if opts.Format == "json" {
			handler = slog.NewJSONHandler(shared, handlerOptions).WithAttrs([]slog.Attr{slog.String("component", string(component))})
		} else {
			handler = &compactTextHandler{writer: shared, level: slogLevel(level), component: component}
		}
		registry.loggers[component] = slog.New(handler)
	}
	return registry, nil
}

// Logger returns the logger for a supported component. It returns nil for an
// unknown component; callers must use one of the constants above rather than
// silently falling back to another component or a global logger.
func (r *Registry) Logger(component Component) *slog.Logger {
	if r == nil {
		return nil
	}
	return r.loggers[component]
}
