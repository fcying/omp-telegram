package config

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// FinalAction is a literal shortcut to an existing slash command.
// Neither labels nor commands undergo environment or shell expansion.
type FinalAction struct {
	Label   string `toml:"label"`
	Command string `toml:"command"`
}

func validateFinalActions(actions []FinalAction) error {
	if len(actions) > 6 {
		return fmt.Errorf("telegram.final_actions supports at most 6 buttons")
	}
	for i, action := range actions {
		invalid := func(field, reason string) error {
			// Do not expose custom labels, command arguments or secrets in errors.
			return fmt.Errorf("telegram.final_actions[%d].%s %s", i, field, reason)
		}
		if strings.TrimSpace(action.Label) == "" || !utf8.ValidString(action.Label) {
			return invalid("label", "must be nonempty text")
		}
		units := 0
		for _, r := range action.Label {
			if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
				return invalid("label", "must be single-line text without controls")
			}
			units++
			if r > 0xffff {
				units++
			}
		}
		if units > 64 {
			return invalid("label", "must not exceed 64 UTF-16 units")
		}
		command := action.Command
		if command == "" || !utf8.ValidString(command) || !strings.HasPrefix(command, "/") {
			return invalid("command", "must be one slash invocation")
		}
		for _, r := range command {
			if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
				return invalid("command", "must not contain controls or line breaks")
			}
		}
		end := strings.IndexFunc(command[1:], func(r rune) bool { return r == ':' || unicode.IsSpace(r) || r == '\uFEFF' })
		name := command[1:]
		if end >= 0 {
			name = name[:end]
		}
		if name == "" {
			return invalid("command", "must have a command name")
		}
		for _, r := range name {
			if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' || r == '@') {
				return invalid("command", "has an invalid command name")
			}
		}
	}
	return nil
}
