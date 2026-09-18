package omp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// SessionInfo is the native session identity and its actual runtime directory.
type SessionInfo struct {
	ID, File, CWD string
}

type metadataQuery struct {
	id        string
	modelRole bool
	output    chan string
}

// SessionInfo queries the local /session command without starting an agent turn.
// Startup callers must not submit other prompts until this query completes.
// An interrupted prompt makes further metadata queries unsafe: native command
// output has no correlation ID, so late output must remain an ordinary event.
func (c *Client) SessionInfo(ctx context.Context) (SessionInfo, error) {
	var info SessionInfo
	if err := ctx.Err(); err != nil {
		return info, err
	}
	c.mu.Lock()
	if c.metadataBusy || c.metadataUnavailable {
		c.mu.Unlock()
		return info, errors.New("omp: session metadata query unavailable")
	}
	c.metadataBusy = true
	c.mu.Unlock()
	promptStarted, completed := false, false
	defer func() {
		c.mu.Lock()
		c.metadata = nil
		c.metadataBusy = false
		if promptStarted && !completed {
			c.metadataUnavailable = true
		}
		c.mu.Unlock()
	}()
	raw, err := c.Call(ctx, "get_state", nil)
	if err != nil {
		return info, err
	}
	var state struct {
		ID   string `json:"sessionId"`
		File string `json:"sessionFile"`
	}
	if json.Unmarshal(raw, &state) != nil || state.ID == "" || strings.ContainsAny(state.ID, "\r\n\x00") || !filepath.IsAbs(state.File) {
		return info, errors.New("omp: invalid session metadata")
	}
	// New native sessions may not have a history file until their first persisted entry.
	query := &metadataQuery{id: state.ID, output: make(chan string, 1)}
	c.mu.Lock()
	c.metadata = query
	c.mu.Unlock()
	promptStarted = true
	raw, err = c.Call(ctx, "prompt", map[string]any{"message": "/session info"})
	if err != nil {
		return info, err
	}
	var ack struct {
		AgentInvoked *bool `json:"agentInvoked"`
	}
	if json.Unmarshal(raw, &ack) != nil || (ack.AgentInvoked != nil && *ack.AgentInvoked) {
		return info, errors.New("omp: session metadata command was not local")
	}
	// Native command_output precedes its acknowledgment. Do not wait for or
	// drain subsequent events when that required output is missing.
	var text string
	select {
	case text = <-query.output:
	default:
		return info, errors.New("omp: session metadata output missing")
	}
	lines := strings.Split(text, "\n")
	if len(lines) != 3 || lines[0] != "Session: "+state.ID || !strings.HasPrefix(lines[1], "Title: ") || !strings.HasPrefix(lines[2], "CWD: ") || strings.ContainsAny(text, "\r\x00") {
		return info, errors.New("omp: invalid session metadata output")
	}
	cwd := strings.TrimPrefix(lines[2], "CWD: ")
	if !filepath.IsAbs(cwd) {
		return info, errors.New("omp: invalid session working directory")
	}
	dir, err := os.Stat(cwd)
	if err != nil || !dir.IsDir() {
		return info, errors.New("omp: session working directory unavailable")
	}
	if err := ctx.Err(); err != nil {
		return info, err
	}
	completed = true
	return SessionInfo{ID: state.ID, File: state.File, CWD: cwd}, nil
}

func (c *Client) captureMetadata(frame json.RawMessage) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	query := c.metadata
	if query == nil {
		return false
	}
	var event struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(frame, &event) != nil {
		return false
	}
	if query.modelRole {
		if _, ok := parseModelOutput(event.Text); !ok {
			return false
		}
	} else if !strings.HasPrefix(event.Text, "Session: "+query.id+"\n") {
		return false
	}
	select {
	case query.output <- event.Text:
		return true
	default:
		return false
	}
}
