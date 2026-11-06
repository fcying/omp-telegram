package bridge

import (
	"context"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"omp-telegram/internal/config"
)

const (
	replyGitMaxOutput  = 64 * 1024
	replyGitBranchSize = 64
)

// readReplyGit returns a raw display label, never repository paths or diagnostics.
// An unknown state is deliberately different from a known clean repository.
func readReplyGit(parent context.Context, workspace string, environment []string, timeout time.Duration) string {
	if !filepath.IsAbs(workspace) || parent.Err() != nil {
		return ""
	}
	if timeout <= 0 {
		timeout = config.DefaultGitTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	probe := replyGitProbe{ctx: ctx, environment: replyGitEnvironment(environment), remaining: replyGitMaxOutput}
	root, ok := probe.root(workspace)
	if !ok || !probe.preflight(root) {
		return ""
	}
	label := ""
	dirty := false
	for _, repository := range probe.repositories {
		output, ok := probe.run(repository, nil, "status", "--porcelain=v2", "--branch", "--no-ahead-behind",
			"--untracked-files=normal", "--ignore-submodules=dirty", "-z")
		if !ok {
			return ""
		}
		status := parseReplyGitStatus(string(output))
		if status == "" {
			return ""
		}
		if repository == root {
			label = status
		}
		dirty = dirty || strings.HasSuffix(status, "*")
	}
	// A known driver's attributes may change after preflight. The command-level
	// guards prevent execution, but its unfiltered result is not a valid label.
	for _, repository := range probe.repositories {
		if _, ok := probe.attributes(repository); !ok {
			return ""
		}
	}
	if dirty && !strings.HasSuffix(label, "*") {
		label += "*"
	}
	return label
}

func replyGitEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		// Repository selection and injected configuration must not redirect the
		// snapshot away from workspace. Normal environment (HOME, PATH, etc.) stays.
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
			"GIT_NAMESPACE", "GIT_PREFIX", "GIT_GRAFT_FILE", "GIT_SHALLOW_FILE", "GIT_REPLACE_REF_BASE",
			"GIT_CONFIG", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS":
			continue
		}
		if strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			continue
		}
		// Trace destinations can be files, so inheriting them would make this
		// otherwise read-only command write user state and disclose paths.
		if strings.HasPrefix(name, "GIT_TRACE") {
			continue
		}
		filtered = append(filtered, entry)
	}
	// Explicit zeros also override trace2.*Target settings from every config
	// scope. Merely removing inherited variables re-enables configured targets.
	return append(filtered, "GIT_TRACE2=0", "GIT_TRACE2_EVENT=0", "GIT_TRACE2_PERF=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
}

type replyGitOutput struct {
	data     []byte
	limit    int
	overflow bool
}

func (output *replyGitOutput) Write(data []byte) (int, error) {
	limit := output.limit
	if limit == 0 {
		limit = replyGitMaxOutput
	}
	if len(data) > limit-len(output.data) {
		output.overflow = true
		return 0, io.ErrShortBuffer
	}
	output.data = append(output.data, data...)
	return len(data), nil
}

func parseReplyGitStatus(output string) string {
	var branch, oid string
	dirty := false
	for output != "" {
		record, rest, terminated := strings.Cut(output, "\x00")
		if !terminated || record == "" {
			return ""
		}
		output = rest
		switch {
		case strings.HasPrefix(record, "# "):
			key, value, ok := strings.Cut(record[2:], " ")
			if !ok || value == "" {
				return ""
			}
			switch key {
			case "branch.oid":
				if oid != "" || value != "(initial)" && !replyGitObjectID(value, 0) {
					return ""
				}
				oid = value
			case "branch.head":
				if branch != "" || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 || strings.Contains(value, " ") {
					return ""
				}
				branch = value
			case "branch.ab":
				ahead, behind, ok := strings.Cut(value, " ")
				if !ok || !replyGitCount(ahead, '+') || !replyGitCount(behind, '-') {
					return ""
				}
			}
		case strings.HasPrefix(record, "1 "), strings.HasPrefix(record, "2 "), strings.HasPrefix(record, "u "):
			oidSize := len(oid)
			if oid == "(initial)" {
				oidSize = 0
			}
			if !replyGitChangedRecord(record, oidSize) {
				return ""
			}
			if record[0] == '2' {
				original, remaining, ok := strings.Cut(output, "\x00")
				if !ok || original == "" {
					return ""
				}
				output = remaining
			}
			dirty = true
		case strings.HasPrefix(record, "? "):
			if len(record) == 2 {
				return ""
			}
			dirty = true
		case strings.HasPrefix(record, "! "):
			if len(record) == 2 {
				return ""
			}
		default:
			return ""
		}
	}
	if branch == "" || oid == "" || branch == "(detached)" && oid == "(initial)" {
		return ""
	}
	if branch == "(detached)" {
		branch = "HEAD"
	}
	branch = truncateUTF8(branch, replyGitBranchSize)
	if dirty {
		branch += "*"
	}
	return branch
}

func replyGitChangedRecord(record string, oidSize int) bool {
	// All metadata must be present before the opaque path. A rename has a
	// second, separately terminated path, consumed by the caller.
	fields := 8
	if record[0] == '2' {
		fields = 9
	} else if record[0] == 'u' {
		fields = 10
	}
	kind := record[0]
	for i := range fields {
		field, rest, ok := strings.Cut(record, " ")
		if !ok || field == "" {
			return false
		}
		record = rest
		switch {
		case i == 0:
		case i == 1:
			if len(field) != 2 || strings.IndexByte(".MADRCUT", field[0]) < 0 || strings.IndexByte(".MADRCUT", field[1]) < 0 {
				return false
			}
		case i == 2:
			if field != "N..." && (len(field) != 4 || field[0] != 'S' || !strings.ContainsRune(".C", rune(field[1])) || !strings.ContainsRune(".M", rune(field[2])) || !strings.ContainsRune(".U", rune(field[3]))) {
				return false
			}
		case i >= 3 && (i <= 5 || kind == 'u' && i == 6):
			if len(field) != 6 || strings.IndexFunc(field, func(r rune) bool { return r < '0' || r > '7' }) >= 0 {
				return false
			}
		case kind == '2' && i == 8:
			if len(field) < 2 || field[0] != 'R' && field[0] != 'C' || !replyGitDigits(field[1:]) {
				return false
			}
			score, err := strconv.Atoi(field[1:])
			if err != nil || score > 100 {
				return false
			}
		default:
			if !replyGitObjectID(field, oidSize) {
				return false
			}
		}
	}
	return record != ""
}

func replyGitObjectID(value string, size int) bool {
	if len(value) != 40 && len(value) != 64 || size != 0 && len(value) != size {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' && r < 'a' || r > 'f' }) < 0
}

func replyGitCount(value string, sign byte) bool {
	// --no-ahead-behind uses '?' when an upstream count is not calculated.
	return len(value) > 1 && value[0] == sign && (value[1:] == "?" || replyGitDigits(value[1:]))
}

func replyGitDigits(value string) bool {
	return value != "" && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0
}
