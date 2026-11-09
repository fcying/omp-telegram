package config

import (
	"fmt"
	"strings"
	"time"
)

// DefaultGitTimeout bounds built-in and custom Git status sampling.
const DefaultGitTimeout = 450 * time.Millisecond

// StatusBar selects ordered field groups for each Telegram header. Nil groups
// use the defaults; explicitly empty groups disable their headers.
type StatusBar struct {
	Progress     [][]string
	Intermediate [][]string
	Final        [][]string
	// GitCommand overrides the final Git label. Empty uses the built-in safe probe.
	GitCommand []string
	// GitTimeout bounds Git sampling. Zero uses DefaultGitTimeout.
	GitTimeout time.Duration
}

var defaultStatusBarFields = [][]string{{"status"}, {"thinking", "model", "fast"}, {"context"}, {"speed"}}

// Fields returns the selected groups in display order. The returned slices are
// read-only. An unknown header kind has no fields.
func (s StatusBar) Fields(kind string) [][]string {
	var fields [][]string
	switch kind {
	case "progress":
		fields = s.Progress
	case "intermediate":
		return s.Intermediate
	case "final":
		fields = s.Final
	default:
		return nil
	}
	if fields == nil {
		return defaultStatusBarFields
	}
	return fields
}

func (s StatusBar) Has(kind, field string) bool {
	for _, group := range s.Fields(kind) {
		for _, selected := range group {
			if selected == field {
				return true
			}
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
		seen := make(map[string]bool)
		for _, group := range s.Fields(kind) {
			for _, field := range group {
				switch field {
				case "status", "model", "thinking", "fast", "context", "speed", "quota":
				case "git":
					if kind != "final" {
						return fmt.Errorf("telegram.status_bar.%s: git is only allowed in final headers", kind)
					}
				default:
					return fmt.Errorf("telegram.status_bar.%s contains an unknown field", kind)
				}
				if seen[field] {
					return fmt.Errorf("telegram.status_bar.%s contains a duplicate field", kind)
				}
				seen[field] = true
			}
		}
	}
	return nil
}

type statusBarFileConfig struct {
	Progress     *[]any   `toml:"progress"`
	Intermediate *[]any   `toml:"intermediate"`
	Final        *[]any   `toml:"final"`
	GitCommand   []string `toml:"git_command"`
	GitTimeout   *string  `toml:"git_timeout"`
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
	result := StatusBar{GitCommand: s.GitCommand, GitTimeout: timeout}
	for _, header := range []struct {
		kind   string
		input  *[]any
		output *[][]string
	}{
		{"progress", s.Progress, &result.Progress},
		{"intermediate", s.Intermediate, &result.Intermediate},
		{"final", s.Final, &result.Final},
	} {
		groups, err := statusBarFileFields(header.input)
		if err != nil {
			return StatusBar{}, fmt.Errorf("telegram.status_bar.%s: %w", header.kind, err)
		}
		*header.output = groups
	}
	return result, nil
}

func statusBarFileFields(fields *[]any) ([][]string, error) {
	if fields == nil {
		return nil, nil
	}
	groups := make([][]string, 0, len(*fields))
	for _, entry := range *fields {
		switch value := entry.(type) {
		case string:
			groups = append(groups, []string{value})
		case []any:
			group := make([]string, len(value))
			for i, item := range value {
				field, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("field groups must contain only field names")
				}
				group[i] = field
			}
			if len(group) != 0 {
				groups = append(groups, group)
			}
		default:
			return nil, fmt.Errorf("expected field names or arrays of field names")
		}
	}
	return groups, nil
}
