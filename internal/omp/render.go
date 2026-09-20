package omp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
)

const (
	maxRenderDiagnostics  = 1 << 20
	maxSessionHeaderBytes = 1 << 20
	maxSessionHeaderLines = 8
)

var errSessionPathResolution = errors.New("omp: native session path resolution failed")

// ErrCustomSessionDir reports that native render cannot resolve sessions from a custom store.
var ErrCustomSessionDir = errors.New("omp: native render does not support custom session directory")

// HasCustomSessionDir reports whether OMP is configured to use a non-default session store.
func HasCustomSessionDir(args []string) bool {
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		if name == "--session-dir" {
			return true
		}
	}
	return os.Getenv("PI_CODING_AGENT_SESSION_DIR") != ""
}

// ResolveSessionPath asks the native renderer to resolve a resumable session.
// It does not start an RPC runtime or implement OMP's session-store rules.
func ResolveSessionPath(ctx context.Context, cfg Config, sessionID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if cfg.Resume != "" || !filepath.IsAbs(cfg.CWD) || !validRenderSessionID(sessionID) {
		return "", errSessionPathResolution
	}
	if HasCustomSessionDir(cfg.Args) {
		return "", ErrCustomSessionDir
	}
	if err := ValidateArgs(cfg.Args); err != nil {
		return "", errSessionPathResolution
	}
	cwd, err := filepath.EvalSymlinks(cfg.CWD)
	if err != nil {
		return "", errSessionPathResolution
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return "", errSessionPathResolution
	}

	binary := cfg.Binary
	if binary == "" {
		binary = "omp"
	}
	args := make([]string, 0, len(cfg.Args)+4)
	args = append(args, cfg.Args...)
	args = append(args, "render", sessionID, "-q", "-t")
	cmd := exec.Command(binary, args...)
	cmd.Dir = cfg.CWD
	cmd.Stdout = io.Discard
	diagnostics := &limitedBuffer{limit: maxRenderDiagnostics}
	cmd.Stderr = diagnostics
	if err := runProcess(ctx, cmd); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errSessionPathResolution
	}

	path, ok := parseRenderedSessionPath(diagnostics.Bytes())
	if !ok || !filepath.IsAbs(path) {
		return "", errSessionPathResolution
	}
	if err := verifyRenderedSession(ctx, path, sessionID, cwd); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errSessionPathResolution
	}
	return path, nil
}

func validRenderSessionID(id string) bool {
	if id == "" || len(id) > 4096 || strings.HasPrefix(id, "-") || strings.ContainsAny(id, "\r\n\x00") {
		return false
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func parseRenderedSessionPath(diagnostics []byte) (string, bool) {
	line := diagnostics
	if index := bytes.IndexByte(line, '\n'); index >= 0 {
		line = line[:index]
	}
	line = bytes.TrimSuffix(line, []byte{'\r'})
	const prefix = "session  "
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return "", false
	}
	path := string(line[len(prefix):])
	if path == "" || strings.TrimSpace(path) != path || strings.ContainsAny(path, "\r\n\x00") {
		return "", false
	}
	return filepath.Clean(path), true
}

func verifyRenderedSession(ctx context.Context, path, requestedID, expectedCWD string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stat, err := os.Lstat(path)
	if err != nil || !stat.Mode().IsRegular() {
		return errSessionPathResolution
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return errSessionPathResolution
	}
	defer file.Close()
	stat, err = file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return errSessionPathResolution
	}

	decoder := json.NewDecoder(io.LimitReader(file, maxSessionHeaderBytes))
	for range maxSessionHeaderLines {
		if err := ctx.Err(); err != nil {
			return err
		}
		var header struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			CWD  string `json:"cwd"`
		}
		if err := decoder.Decode(&header); err != nil {
			return errSessionPathResolution
		}
		if header.Type != "session" {
			continue
		}
		if !validRenderSessionID(header.ID) || !renderSessionIDMatches(header.ID, requestedID) || !filepath.IsAbs(header.CWD) {
			return errSessionPathResolution
		}
		resolvedCWD, err := filepath.EvalSymlinks(header.CWD)
		if err != nil || filepath.Clean(resolvedCWD) != filepath.Clean(expectedCWD) {
			return errSessionPathResolution
		}
		return nil
	}
	return errSessionPathResolution
}

func renderSessionIDMatches(actual, requested string) bool {
	actual, requested = strings.ToLower(actual), strings.ToLower(requested)
	return actual == requested || strings.HasPrefix(actual, requested)
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len() < b.limit {
		remaining := b.limit - b.Len()
		if len(p) > remaining {
			_, _ = b.Buffer.Write(p[:remaining])
			return len(p), nil
		}
		_, _ = b.Buffer.Write(p)
		return len(p), nil
	}
	return len(p), nil
}
