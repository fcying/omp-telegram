package omp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// SessionSummary contains only native session-list metadata, never transcript data.
type SessionSummary struct {
	ID, CWD, Title, UpdatedAt string
}

const maxListFrame = 4 << 20
const maxListEntries = 10000

var errListProtocol = errors.New("omp: invalid session-list response")

// ListSessions queries a short-lived native ACP process without loading a session.
// The caller should supply a deadline; cancellation also reaps the process group.
func ListSessions(ctx context.Context, cfg Config) ([]SessionSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Resume != "" || !filepath.IsAbs(cfg.CWD) {
		return nil, errors.New("omp: session listing requires an absolute working directory and no resume ID")
	}
	cwd, err := filepath.EvalSymlinks(cfg.CWD)
	if err != nil {
		return nil, errors.New("omp: cannot resolve session-list working directory")
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return nil, errors.New("omp: invalid session-list working directory")
	}
	if err := ValidateArgs(cfg.Args); err != nil {
		return nil, err
	}
	if cfg.Binary == "" {
		cfg.Binary = "omp"
	}
	args := append([]string{"acp", "--cwd", cfg.CWD}, cfg.Args...)
	cmd := exec.Command(cfg.Binary, args...)
	cmd.Env = ChildEnv(cfg.Environment)
	cmd.Dir = cfg.CWD
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.New("omp: cannot create session-list input pipe")
	}
	defer inWrite.Close()
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		inRead.Close()
		return nil, errors.New("omp: cannot create session-list output pipe")
	}
	defer outRead.Close()
	// A direct /dev/null descriptor cannot block on verbose or inherited stderr.
	stderr, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		inRead.Close()
		outWrite.Close()
		return nil, errors.New("omp: cannot discard session-list diagnostics")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inRead, outWrite, stderr
	waited, err := startProcess(cmd)
	inRead.Close()
	outWrite.Close()
	stderr.Close()
	if err != nil {
		return nil, errors.New("omp: cannot start session listing; check binary and working directory")
	}
	type listingResult struct {
		sessions []SessionSummary
		err      error
	}
	results := make(chan listingResult, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		reader := bufio.NewReader(outRead)
		sessions, err := collectSessions(reader, inWrite, cfg.CWD, cwd)
		results <- listingResult{sessions, err}
		_, _ = io.Copy(io.Discard, reader)
	}()
	var result listingResult
	select {
	case result = <-results:
	case <-ctx.Done():
		result.err = ctx.Err()
	}
	inWrite.Close()
	// Always drain stdout and reap the group, including descendants of an exited leader.
	timer := time.NewTimer(2 * time.Second)
	exited := false
	select {
	case <-waited:
		exited = true
	case <-timer.C:
	}
	timer.Stop()
	if !exited {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		timer.Reset(time.Second)
		select {
		case <-waited:
			exited = true
		case <-timer.C:
		}
		timer.Stop()
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if !exited {
		<-waited
	}
	outRead.Close()
	<-readDone
	return result.sessions, result.err
}

func collectSessions(reader *bufio.Reader, writer io.Writer, requestedCWD, canonicalCWD string) ([]SessionSummary, error) {
	var nextID int
	call := func(method string, params any) (json.RawMessage, error) {
		nextID++
		id := strconv.Itoa(nextID)
		if json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}) != nil {
			return nil, errors.New("omp: cannot write session-list request")
		}
		for {
			line, err := readLine(reader, maxListFrame)
			if err != nil {
				return nil, errListProtocol
			}
			var frame struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Method  string          `json:"method"`
				Result  json.RawMessage `json:"result"`
				Error   json.RawMessage `json:"error"`
			}
			if json.Unmarshal(line, &frame) != nil || frame.JSONRPC != "2.0" {
				return nil, errListProtocol
			}
			if frame.Method != "" {
				if len(frame.ID) != 0 {
					if json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "error": map[string]any{"code": -32601, "message": "Method not found"}}) != nil {
						return nil, errors.New("omp: cannot reject session-list server request")
					}
				}
				continue
			}
			var responseID string
			if json.Unmarshal(frame.ID, &responseID) != nil || responseID != id {
				return nil, errListProtocol
			}
			if len(frame.Error) != 0 {
				return nil, errors.New("omp: native session-list request failed")
			}
			if len(frame.Result) == 0 || bytes.Equal(frame.Result, []byte("null")) {
				return nil, errListProtocol
			}
			return frame.Result, nil
		}
	}
	data, err := call("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	if err != nil {
		return nil, err
	}
	var initialized struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			SessionCapabilities struct {
				List json.RawMessage `json:"list"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	if json.Unmarshal(data, &initialized) != nil || initialized.ProtocolVersion != 1 {
		return nil, errListProtocol
	}
	list := bytes.TrimSpace(initialized.AgentCapabilities.SessionCapabilities.List)
	if len(list) == 0 || list[0] != '{' {
		return nil, errors.New("omp: native ACP session listing is unavailable")
	}
	var sessions []SessionSummary
	ids := make(map[string]bool)
	cursors := make(map[string]bool)
	cursor := ""
	entries := 0
	for {
		params := map[string]any{"cwd": requestedCWD}
		if cursor != "" {
			params["cursor"] = cursor
		}
		data, err := call("session/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Sessions *[]struct {
				ID        string `json:"sessionId"`
				CWD       string `json:"cwd"`
				Title     string `json:"title"`
				UpdatedAt string `json:"updatedAt"`
			} `json:"sessions"`
			NextCursor string `json:"nextCursor"`
		}
		if json.Unmarshal(data, &page) != nil || page.Sessions == nil {
			return nil, errListProtocol
		}
		entries += len(*page.Sessions)
		if entries > maxListEntries || len(cursors) >= maxListEntries {
			return nil, errors.New("omp: native session listing exceeds safety limit")
		}
		for _, session := range *page.Sessions {
			if session.ID == "" || len(session.ID) > 4096 || !filepath.IsAbs(session.CWD) {
				return nil, errListProtocol
			}
			resolved, err := filepath.EvalSymlinks(session.CWD)
			if err != nil || resolved != canonicalCWD || ids[session.ID] {
				continue
			}
			ids[session.ID] = true
			sessions = append(sessions, SessionSummary{session.ID, session.CWD, session.Title, session.UpdatedAt})
		}
		cursor = page.NextCursor
		if cursor == "" {
			return sessions, nil
		}
		if cursors[cursor] {
			return nil, errors.New("omp: native session listing repeated a cursor")
		}
		cursors[cursor] = true
	}
}
