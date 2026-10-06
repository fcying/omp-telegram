package omp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

type autoresearchEntry struct {
	ID         string  `json:"id"`
	ParentID   *string `json:"parentId"`
	Type       string  `json:"type"`
	CustomType string  `json:"customType"`
	mode       string
}

func (e *autoresearchEntry) UnmarshalJSON(data []byte) error {
	// Ignore model/tool payloads instead of copying native context into metadata.
	type header autoresearchEntry
	var value header
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*e = autoresearchEntry(value)
	if e.Type != "custom" || e.CustomType != "autoresearch-control" {
		return nil
	}
	var control struct {
		Data struct {
			Mode string `json:"mode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &control); err != nil {
		return err
	}
	e.mode = control.Data.Mode
	return nil
}

type autoresearchSnapshot struct {
	Entries []autoresearchEntry `json:"entries"`
	LeafID  json.RawMessage     `json:"leafId"`
}

// AutoresearchMode reads control metadata from the native session's current branch.
// Native history supplies ancestry only; RPC supplies the current leaf and updates.
func (c *Client) AutoresearchMode(ctx context.Context) (bool, error) {
	state, err := c.autoresearchSession(ctx)
	if err != nil {
		return false, err
	}
	entries, cursor, err := readAutoresearchHistory(ctx, state.ID, state.File)
	if err != nil {
		return false, err
	}
	var args map[string]any
	if cursor != "" {
		args = map[string]any{"since": cursor}
	}
	raw, err := c.Call(ctx, "get_entries", args)
	if err != nil {
		return false, err
	}
	var snapshot autoresearchSnapshot
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.Entries == nil {
		return false, errProtocol
	}
	snapshot.Entries = append(entries, snapshot.Entries...)
	current, err := c.autoresearchSession(ctx)
	if err != nil {
		return false, err
	}
	if current != state {
		return false, errProtocol
	}
	return snapshot.mode()
}

func (c *Client) autoresearchSession(ctx context.Context) (SessionInfo, error) {
	raw, err := c.Call(ctx, "get_state", nil)
	if err != nil {
		return SessionInfo{}, err
	}
	var state struct {
		ID   string `json:"sessionId"`
		File string `json:"sessionFile"`
	}
	if json.Unmarshal(raw, &state) != nil || state.ID == "" || !filepath.IsAbs(state.File) {
		return SessionInfo{}, errProtocol
	}
	return SessionInfo{ID: state.ID, File: state.File}, nil
}

func readAutoresearchHistory(ctx context.Context, sessionID, path string) ([]autoresearchEntry, string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		// New native sessions need not have a persisted file yet.
		return nil, "", nil
	}
	if err != nil {
		return nil, "", errors.New("omp: native research history unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, "", errors.New("omp: native research history is not a regular file")
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxLogical)
	var entries []autoresearchEntry
	header := false
	cursor := ""
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		var entry autoresearchEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			var syntaxError *json.SyntaxError
			if errors.As(err, &syntaxError) {
				// Native load can skip crash debris without rewriting the file.
				// Let RPC supply the canonical suffix after the valid prefix.
				break
			}
			return nil, "", errProtocol
		}
		switch entry.Type {
		case "title":
			// Native fixed-width title slots are not ancestry entries.
			continue
		case "session":
			if header || entry.ID != sessionID {
				return nil, "", errProtocol
			}
			header = true
			var native struct {
				Version int `json:"version"`
			}
			if json.Unmarshal(scanner.Bytes(), &native) != nil {
				return nil, "", errProtocol
			}
			if native.Version < 2 {
				// Native migration assigns IDs in memory before rewriting v1 files.
				// No disk cursor or ancestry is valid until that rewrite completes.
				return nil, "", ctx.Err()
			}
		default:
			if !header || entry.ID == "" {
				return nil, "", errProtocol
			}
			entries = append(entries, entry)
			cursor = entry.ID
		}
	}
	if scanner.Err() != nil || !header {
		return nil, "", errors.New("omp: native research history incomplete")
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return entries, cursor, nil
}

func (snapshot autoresearchSnapshot) mode() (bool, error) {
	var leafID *string
	if json.Unmarshal(snapshot.LeafID, &leafID) != nil {
		return false, errProtocol
	}
	if leafID == nil {
		return false, nil
	}
	if *leafID == "" {
		return false, errProtocol
	}
	indices := make(map[string]int, len(snapshot.Entries))
	for i := range snapshot.Entries {
		entry := &snapshot.Entries[i]
		if entry.ID == "" {
			return false, errProtocol
		}
		if _, duplicate := indices[entry.ID]; duplicate {
			return false, errProtocol
		}
		indices[entry.ID] = i
	}
	id := *leafID
	var enabled, foundControl bool
	for range len(snapshot.Entries) {
		index, exists := indices[id]
		if !exists {
			return false, errProtocol
		}
		entry := &snapshot.Entries[index]
		if !foundControl && entry.Type == "custom" && entry.CustomType == "autoresearch-control" {
			foundControl = true
			switch entry.mode {
			case "on":
				enabled = true
			case "off", "clear":
				enabled = false
			default:
				return false, errProtocol
			}
		}
		if entry.ParentID == nil {
			return enabled, nil
		}
		id = *entry.ParentID
	}
	return false, errProtocol
}
