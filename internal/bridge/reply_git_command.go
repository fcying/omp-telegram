package bridge

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"omp-telegram/internal/config"
)

// Custom commands bypass the built-in Git safety probe by explicit configuration.
func readReplyGitField(parent context.Context, workspace string, environment, command []string, timeout time.Duration) string {
	if len(command) == 0 {
		return readReplyGit(parent, workspace, environment, timeout)
	}
	if !filepath.IsAbs(workspace) || parent.Err() != nil {
		return ""
	}
	if timeout <= 0 {
		timeout = config.DefaultGitTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = workspace
	cmd.Env = environment
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 25 * time.Millisecond
	output := replyGitOutput{}
	cmd.Stdout = &output
	err := cmd.Run()
	if err != nil || ctx.Err() != nil || output.overflow {
		return ""
	}
	label := strings.TrimRight(string(output.data), "\r\n")
	lineBreak := func(r rune) bool {
		return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
	}
	if !utf8.ValidString(label) || strings.IndexFunc(label, lineBreak) >= 0 {
		return ""
	}
	return truncateUTF8(label, replyGitBranchSize)
}
