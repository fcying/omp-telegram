package omp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// ErrSessionFileGone marks a failed deletion whose verified session file is absent after hard stop.
var ErrSessionFileGone = errors.New("omp: session file missing after failed deletion")

// DeleteSession asks a separate native RPC process to delete its resumed session.
// The caller must ensure no active worker is using the session and supply a deadline.
func DeleteSession(ctx context.Context, cfg Config, sessionID string, rpcLogger *slog.Logger) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.Resume != "" || sessionID == "" || strings.ContainsAny(sessionID, "\r\n\x00") || !filepath.IsAbs(cfg.CWD) {
		return errors.New("omp: invalid session deletion request")
	}
	cfg.Resume = sessionID
	client, err := Start(ctx, cfg, rpcLogger)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("omp: cannot start session deletion")
	}
	destructive := false
	sessionFile := ""
	defer func() {
		if destructive {
			stopErr := client.TerminateNow()
			if resultErr != nil && stopErr == nil && sessionFile != "" {
				if _, statErr := os.Lstat(sessionFile); errors.Is(statErr, os.ErrNotExist) {
					resultErr = errors.Join(resultErr, ErrSessionFileGone)
				}
			}
		} else {
			_ = client.Close()
		}
	}()

	info, err := client.SessionInfo(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("omp: cannot verify session identity")
	}
	if info.ID != sessionID || !filepath.IsAbs(info.File) || filepath.Clean(info.File) != info.File || strings.ContainsAny(info.File, "\r\n\x00") {
		return errors.New("omp: session identity mismatch")
	}
	requestedCWD, requestErr := filepath.EvalSymlinks(cfg.CWD)
	resumedCWD, sessionErr := filepath.EvalSymlinks(info.CWD)
	if requestErr != nil || sessionErr != nil || requestedCWD != resumedCWD {
		return errors.New("omp: session working directory mismatch")
	}
	before, err := os.Lstat(info.File)
	if err != nil || !before.Mode().IsRegular() {
		return errors.New("omp: session file unavailable")
	}
	sessionFile = info.File
	if err := ctx.Err(); err != nil {
		return err
	}

	// From this point a prompt write may execute the deletion even if its reply
	// is lost; the supervisor must never offer OMP a graceful stdin EOF.
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
