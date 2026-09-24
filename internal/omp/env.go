package omp

import (
	"os"
	"strings"
)

// ChildEnv returns the current process environment without the bridge bot token.
func ChildEnv() []string {
	env := os.Environ()
	filtered := env[:0]
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if name != "OMP_TELEGRAM_BOT_TOKEN" {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
