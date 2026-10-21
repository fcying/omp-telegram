package omp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrSessionFileGone marks a failed or partial deletion whose verified session
// file is absent after the native child has stopped.
var ErrSessionFileGone = errors.New("omp: session file missing after failed deletion")

// ErrBoundSessionMissing means the bound file was absent before native deletion started.
var ErrBoundSessionMissing = errors.New("omp: bound session file does not exist")

// DeleteSession asks a separate native RPC process to delete its resumed session.
// The caller must ensure no active worker is using the session and supply a deadline.
func DeleteSession(ctx context.Context, cfg Config, sessionID string, rpcLogger *slog.Logger) error {
	return deleteSession(ctx, cfg, sessionID, "", rpcLogger)
}

// DeleteBoundSession verifies the persisted native file before deleting through
// OMP. It temporarily recreates a missing original workspace, then removes only
// unchanged empty directories it created, after the native child has stopped.
// A cleanup failure is an error even after native success; ErrSessionFileGone
// distinguishes that partial outcome from a session that is still present.
func DeleteBoundSession(ctx context.Context, cfg Config, sessionID, sessionFile string, rpcLogger *slog.Logger) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(sessionFile) || filepath.Clean(sessionFile) != sessionFile || strings.ContainsAny(sessionFile, "\r\n\x00") || filepath.Clean(cfg.CWD) != cfg.CWD || strings.ContainsAny(cfg.CWD, "\r\n\x00") {
		return &classifiedError{kind: "invalid_request", err: errors.New("omp: invalid bound session deletion request")}
	}
	before, err := os.Lstat(sessionFile)
	if errors.Is(err, os.ErrNotExist) {
		return ErrBoundSessionMissing
	}
	if err != nil || !before.Mode().IsRegular() {
		return &classifiedError{kind: "file_unavailable", err: errors.New("omp: bound session file unavailable")}
	}
	return deleteSession(ctx, cfg, sessionID, sessionFile, rpcLogger)
}

func deleteSession(ctx context.Context, cfg Config, sessionID, expectedFile string, rpcLogger *slog.Logger) (resultErr error) {
	reason := "invalid_request"
	defer func() {
		if resultErr != nil {
			resultErr = &classifiedError{kind: reason, err: resultErr}
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.Resume != "" || sessionID == "" || strings.ContainsAny(sessionID, "\r\n\x00") || !filepath.IsAbs(cfg.CWD) {
		return errors.New("omp: invalid session deletion request")
	}
	var workspace *deletionWorkspace
	var client *Client
	destructive := false
	sessionFile := ""
	defer func() {
		var stopErr error
		if client != nil {
			if destructive {
				stopErr = client.TerminateNow()
			} else {
				_ = client.Close()
			}
		}
		if workspace != nil {
			cleanupErr := workspace.close()
			if cleanupErr != nil && resultErr == nil {
				reason = "cleanup_failed"
			}
			resultErr = errors.Join(resultErr, cleanupErr)
		}
		if resultErr != nil && destructive && stopErr == nil && sessionFile != "" {
			if _, statErr := os.Lstat(sessionFile); errors.Is(statErr, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, ErrSessionFileGone)
			}
		}
	}()
	var err error
	if expectedFile != "" {
		reason = "workspace_unavailable"
		workspace, err = prepareDeletionWorkspace(ctx, cfg.CWD)
		if err != nil {
			return err
		}
	}
	cfg.Resume = sessionID
	reason = "startup_failed"
	client, err = Start(ctx, cfg, rpcLogger)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("omp: cannot start session deletion")
	}

	reason = "identity_unconfirmed"
	info, err := client.SessionInfo(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("omp: cannot verify session identity")
	}
	reason = "identity_mismatch"
	if info.ID != sessionID || !filepath.IsAbs(info.File) || filepath.Clean(info.File) != info.File || strings.ContainsAny(info.File, "\r\n\x00") {
		return errors.New("omp: session identity mismatch")
	}
	if expectedFile != "" && info.File != expectedFile {
		return errors.New("omp: bound session file mismatch")
	}
	requestedCWD, requestErr := filepath.EvalSymlinks(cfg.CWD)
	resumedCWD, sessionErr := filepath.EvalSymlinks(info.CWD)
	if requestErr != nil || sessionErr != nil || requestedCWD != resumedCWD {
		return errors.New("omp: session working directory mismatch")
	}
	reason = "file_unavailable"
	before, err := os.Lstat(info.File)
	if err != nil || !before.Mode().IsRegular() {
		return errors.New("omp: session file unavailable")
	}
	sessionFile = info.File
	if err := ctx.Err(); err != nil {
		return err
	}
	reason = "workspace_unavailable"
	if workspace != nil && !workspace.unchanged() {
		return errors.New("omp: session working directory changed")
	}

	// From this point a prompt write may execute the deletion even if its reply
	// is lost; the supervisor must never offer OMP a graceful stdin EOF.
	reason = "native_unconfirmed"
	if err := client.armDestructiveShutdown(); err != nil {
		return errors.New("omp: cannot start native session deletion")
	}
	destructive = true

	// A fresh process has no competing prompts. Native command_output precedes the
	// prompt acknowledgment, but event forwarding is independent of RPC responses.
	raw, err := client.Call(ctx, "prompt", map[string]any{"message": "/session delete"})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("omp: native session deletion failed")
	}
	var ack struct {
		AgentInvoked *bool `json:"agentInvoked"`
	}
	if json.Unmarshal(raw, &ack) != nil || ack.AgentInvoked == nil || *ack.AgentInvoked {
		return errors.New("omp: native deletion was not a local command")
	}

	// Consume a bounded number of events rather than waiting for a prompt_result:
	// local commands do not emit one. Only native deletion output for this exact
	// file is evidence; unrelated or forged-looking command output is not.
	const maxDeleteEvents = 128
	confirmed := false
events:
	for range maxDeleteEvents {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-client.Done():
			return errors.New("omp: deletion process exited without confirmation")
		case frame, ok := <-client.Events():
			if !ok {
				return errors.New("omp: deletion output unavailable")
			}
			var event struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(frame, &event) != nil || event.Type != "command_output" {
				continue
			}
			if strings.HasPrefix(event.Text, "Failed to delete session: ") {
				reason = "native_failed"
				return errors.New("omp: native session deletion failed")
			}
			if strings.HasPrefix(event.Text, "Session deleted: ") {
				if !strings.HasPrefix(event.Text, "Session deleted: "+info.File+". Use ACP ") || strings.ContainsAny(event.Text, "\r\x00") {
					return errors.New("omp: native deletion not confirmed")
				}
				confirmed = true
			}
			if confirmed {
				break events
			}
		}
	}
	if !confirmed {
		return errors.New("omp: native deletion not confirmed")
	}
	if err := client.TerminateNow(); err != nil {
		reason = "cleanup_failed"
		return errors.New("omp: cannot stop deletion process")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(info.File); !errors.Is(err, os.ErrNotExist) {
		return errors.New("omp: native session file remains")
	}
	return nil
}

type deletionDirectory struct {
	parent, fd int
	name       string
	identity   unix.Stat_t
	owned      bool
}

type deletionWorkspace struct {
	root int
	dirs []deletionDirectory
}

func prepareDeletionWorkspace(ctx context.Context, path string) (*deletionWorkspace, error) {
	root, err := unix.Open(string(filepath.Separator), deletionDirectoryOpenFlags, 0)
	if err != nil {
		return nil, errors.New("omp: session working directory unavailable")
	}
	w := &deletionWorkspace{root: root}
	parent := root
	for _, name := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		if name == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return w, err
		}
		owned := false
		fd, err := unix.Openat(parent, name, deletionDirectoryOpenFlags, 0)
		if errors.Is(err, unix.ENOENT) {
			if err = unix.Mkdirat(parent, name, 0700); err == nil {
				owned = true
			} else if !errors.Is(err, unix.EEXIST) {
				return w, errors.New("omp: cannot prepare session working directory")
			}
			fd, err = unix.Openat(parent, name, deletionDirectoryOpenFlags, 0)
		}
		if err != nil {
			return w, errors.New("omp: session working directory unavailable")
		}
		dir := deletionDirectory{parent: parent, fd: fd, name: name, owned: owned}
		if unix.Fstat(fd, &dir.identity) != nil {
			_ = unix.Close(fd)
			return w, errors.New("omp: cannot verify session working directory")
		}
		w.dirs = append(w.dirs, dir)
		parent = fd
	}
	if !w.unchanged() {
		return w, errors.New("omp: session working directory changed")
	}
	return w, nil
}

func (w *deletionWorkspace) unchanged() bool {
	for _, dir := range w.dirs {
		var current unix.Stat_t
		if unix.Fstatat(dir.parent, dir.name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || current.Mode&unix.S_IFMT != unix.S_IFDIR || current.Dev != dir.identity.Dev || current.Ino != dir.identity.Ino {
			return false
		}
	}
	return true
}

func (w *deletionWorkspace) close() error {
	// Pinned parent descriptors prevent a replaced ancestor or symlink from
	// redirecting cleanup. Never remove a replacement or recursively remove data.
	unchanged := w.unchanged()
	var cleanupErr error
	for i := len(w.dirs) - 1; i >= 0; i-- {
		dir := w.dirs[i]
		if dir.owned {
			var current unix.Stat_t
			err := unix.Fstatat(dir.parent, dir.name, &current, unix.AT_SYMLINK_NOFOLLOW)
			switch {
			case errors.Is(err, unix.ENOENT):
				// Already absent; nothing belongs to us at this location.
			case !unchanged || err != nil || current.Mode&unix.S_IFMT != unix.S_IFDIR || current.Dev != dir.identity.Dev || current.Ino != dir.identity.Ino:
				cleanupErr = errors.New("omp: cannot safely clean temporary session working directory")
			default:
				if err := unix.Unlinkat(dir.parent, dir.name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
					cleanupErr = errors.New("omp: cannot clean temporary session working directory")
				}
			}
		}
		_ = unix.Close(dir.fd)
	}
	_ = unix.Close(w.root)
	return cleanupErr
}
