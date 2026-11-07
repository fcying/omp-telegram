package config

import (
	"fmt"
	"strings"
	"time"
)

// DefaultGitTimeout bounds built-in and custom Git status sampling.
const DefaultGitTimeout = 450 * time.Millisecond

// StatusBar selects ordered fields for each Telegram header. Nil field arrays
// use the defaults; explicitly empty field arrays disable their headers.
type StatusBar struct {
	Progress     []string
	Intermediate []string
	Final        []string
	// GitCommand overrides the final Git label. Empty uses the built-in safe probe.
	GitCommand []string
	// GitTimeout bounds Git sampling. Zero uses DefaultGitTimeout.
	GitTimeout time.Duration
}

var defaultStatusBarFields = [...]string{"status", "thinking", "model", "fast", "context", "speed"}

// Fields returns the selected fields in display order. The returned slice is
// read-only. An unknown header kind has no fields.
func (s StatusBar) Fields(kind string) []string {
	var fields []string
	switch kind {
	case "progress":
		fields = s.Progress
	case "intermediate":
		fields = s.Intermediate
	case "final":
		fields = s.Final
	default:
		return nil
	}
	if fields == nil {
		return defaultStatusBarFields[:]
	}
	return fields
}

func (s StatusBar) Has(kind, field string) bool {
	for _, selected := range s.Fields(kind) {
		if selected == field {
			return true
		}
	}
	return false
}

func (s StatusBar) Uses(field string) bool {
	return s.Has("progress", field) || s.Has("intermediate", field) || s.Has("final", field)
}

func (s StatusBar) validate() error {
	if len(s.GitCommand) != 0 && strings.TrimSpace(s.GitCommand[0]) == "" {
		return fmt.Errorf("telegram.status_bar.git_command requires a nonempty executable")
	}
	for _, argument := range s.GitCommand {
		if strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("telegram.status_bar.git_command cannot contain NUL bytes")
		}
	}
	for _, kind := range []string{"progress", "intermediate", "final"} {
		fields := s.Fields(kind)
		for i, field := range fields {
			switch field {
			case "status", "model", "thinking", "fast", "context", "speed", "quota":
			case "git":
				if kind != "final" {
					return fmt.Errorf("telegram.status_bar.%s: git is only allowed in final headers", kind)
				}
			default:
				return fmt.Errorf("telegram.status_bar.%s contains an unknown field", kind)
			}
			for _, previous := range fields[:i] {
				if previous == field {
					return fmt.Errorf("telegram.status_bar.%s contains a duplicate field", kind)
				}
			}
		}
	}
	return nil
}

type statusBarFileConfig struct {
	Progress     *[]string `toml:"progress"`
	Intermediate *[]string `toml:"intermediate"`
	Final        *[]string `toml:"final"`
	GitCommand   []string  `toml:"git_command"`
	GitTimeout   *string   `toml:"git_timeout"`
}

func (s statusBarFileConfig) statusBar() (StatusBar, error) {
	timeout := DefaultGitTimeout
	if s.GitTimeout != nil {
		var err error
		timeout, err = time.ParseDuration(*s.GitTimeout)
		if err != nil || timeout <= 0 {
			return StatusBar{}, fmt.Errorf("telegram.status_bar.git_timeout must be a positive Go duration string")
		}
	}
	return StatusBar{
		Progress:     statusBarFileFields(s.Progress),
		Intermediate: statusBarFileFields(s.Intermediate),
		Final:        statusBarFileFields(s.Final),
		GitCommand:   s.GitCommand,
		GitTimeout:   timeout,
	}, nil
}

func statusBarFileFields(fields *[]string) []string {
	if fields == nil {
		return nil
	}
	if *fields == nil {
		return []string{}
	}
	return *fields
}
