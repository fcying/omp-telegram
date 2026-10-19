package omp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	MigrationCommand        = "omp_telegram_migrate_v1"
	MigrationHandoffCommand = "omp_telegram_migrate_handoff_v1"
	MigrationMarkerType     = "omp-telegram.migration.complete"
	maxMigrationSummary     = 128 << 10
	maxMigrationJournal     = 2 << 20
	maxMigrationEntries     = 4096
)

var (
	ErrMigrationInvalid        = errors.New("omp: invalid migration evidence")
	ErrMigrationUnavailable    = errors.New("omp: migration evidence unavailable")
	ErrMigrationNotSettled     = errors.New("omp: migration session is not settled")
	ErrMigrationMarkerNotReady = errors.New("omp: migration completion marker not ready")
	ErrHandoffNotReady         = errors.New("omp: migration handoff not ready")
)

type MigrationSettings struct {
	Provider, Model, Thinking string
	Fast                      bool
}

type MigrationRequest struct {
	Version          int    `json:"version"`
	MigrationID      string `json:"migrationId"`
	SourceSessionID  string `json:"sourceSessionId"`
	SourceGeneration int64  `json:"sourceGeneration"`
	Workspace        string `json:"workspace"`
	Summary          string `json:"summary"`
	SummarySHA256    string `json:"summarySha256"`
}

func migrationText(s string, limit int) bool {
	return s != "" && len(s) <= limit && utf8.ValidString(s) && !strings.ContainsAny(s, "\r\n\x00")
}

func migrationHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func validMigrationRequest(r MigrationRequest, requireSummary bool) bool {
	decoded, err := hex.DecodeString(r.SummarySHA256)
	return r.Version == 1 && migrationText(r.MigrationID, 128) && migrationText(r.SourceSessionID, 128) &&
		r.SourceGeneration >= 0 && filepath.IsAbs(r.Workspace) && migrationText(r.Workspace, 4096) &&
		err == nil && len(decoded) == sha256.Size && strings.ToLower(r.SummarySHA256) == r.SummarySHA256 &&
		len(r.Summary) <= maxMigrationSummary && utf8.ValidString(r.Summary) &&
		(!requireSummary || r.Summary != "") && (r.Summary == "" || migrationHash(r.Summary) == r.SummarySHA256)
}

// MigrationPrompt encodes an explicitly loaded local command, never a model prompt.
func MigrationPrompt(r MigrationRequest) (string, error) {
	if !validMigrationRequest(r, true) {
		return "", ErrMigrationInvalid
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return "", ErrMigrationInvalid
	}
	prompt := "/" + MigrationCommand + " " + string(raw)
	frame, err := json.Marshal(map[string]any{"type": "prompt", "id": strings.Repeat("0", 20), "message": prompt})
	if err != nil || len(frame)+1 >= maxFrame {
		return "", ErrRequestTooLarge
	}
	return prompt, nil
}

// MigrationHandoff lowers only this runtime's retention budget and restores it before returning.
// If an RPC outcome is uncertain, the caller must close the runtime rather than reuse it.
func (c *Client) MigrationHandoff(ctx context.Context, migrationID string, source SessionInfo) (raw json.RawMessage, err error) {
	if !migrationText(migrationID, 128) || !migrationText(source.ID, 128) ||
		!migrationText(source.CWD, 4096) || !filepath.IsAbs(source.CWD) {
		return nil, ErrMigrationInvalid
	}
	control := func(ctx context.Context, action string) error {
		request, marshalErr := json.Marshal(map[string]string{
			"migrationId": migrationID, "sourceSessionId": source.ID, "workspace": source.CWD, "action": action,
		})
		if marshalErr != nil {
			return ErrMigrationInvalid
		}
		_, callErr := c.Call(ctx, "prompt", map[string]any{"message": "/" + MigrationHandoffCommand + " " + string(request)})
		return callErr
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if restoreErr := control(cleanup, "restore"); restoreErr != nil {
			err = errors.Join(err, restoreErr)
		}
	}()
	if err = control(ctx, "begin"); err != nil {
		return nil, err
	}
	return c.Call(ctx, "handoff", nil)
}

type migrationState struct {
	ID         string `json:"sessionId"`
	File       string `json:"sessionFile"`
	Model      Model  `json:"model"`
	Fast       *bool  `json:"fastModeEnabled"`
	Settled    *bool  `json:"isSettled"`
	Pending    *bool  `json:"hasPendingAsyncWork"`
	Streaming  *bool  `json:"isStreaming"`
	Compacting *bool  `json:"isCompacting"`
	Queued     *int   `json:"queuedMessageCount"`
}

func migrationCallError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrMigrationUnavailable
}

func (c *Client) migrationState(ctx context.Context) (migrationState, error) {
	var state migrationState
	raw, err := c.Call(ctx, "get_state", nil)
	if err != nil {
		return state, migrationCallError(ctx)
	}
	if len(raw) > maxMigrationJournal || json.Unmarshal(raw, &state) != nil || state.Fast == nil ||
		state.Settled == nil || state.Pending == nil || state.Streaming == nil || state.Compacting == nil || state.Queued == nil ||
		!validModelText(state.Model.Provider) || !validModelText(state.Model.ID) || !migrationText(state.ID, 128) || !filepath.IsAbs(state.File) {
		return state, ErrMigrationInvalid
	}
	if !*state.Settled || *state.Pending || *state.Streaming || *state.Compacting || *state.Queued != 0 {
		return state, ErrMigrationNotSettled
	}
	return state, nil
}

// MigrationSettings preserves the configured selector (including auto), not its effective level.
func (c *Client) MigrationSettings(ctx context.Context) (MigrationSettings, error) {
	state, err := c.migrationState(ctx)
	if err != nil {
		return MigrationSettings{}, err
	}
	thinking, err := c.ConfiguredThinking(ctx)
	if err != nil {
		return MigrationSettings{}, migrationCallError(ctx)
	}
	after, err := c.migrationState(ctx)
	if err != nil {
		return MigrationSettings{}, err
	}
	if state.ID != after.ID || state.File != after.File || state.Model != after.Model || *state.Fast != *after.Fast {
		return MigrationSettings{}, ErrMigrationInvalid
	}
	return MigrationSettings{Provider: state.Model.Provider, Model: state.Model.ID, Thinking: thinking, Fast: *state.Fast}, nil
}

type migrationHeader struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	CWD  string `json:"cwd"`
}

// HandoffCursor retains only file identity and a bounded pre-append fence.
// The fence detects in-place rewrites without reading historical conversation.
type HandoffCursor struct {
	path, sessionID, workspace string
	info                       os.FileInfo
	offset, fenceOffset        int64
	fence                      []byte
}

func validMigrationTitle(line []byte) bool {
	if len(line) != 255 || !utf8.Valid(line) {
		return false
	}
	var slot struct {
		Type      string  `json:"type"`
		Version   int     `json:"v"`
		Title     *string `json:"title"`
		Source    string  `json:"source"`
		UpdatedAt *string `json:"updatedAt"`
		Pad       *string `json:"pad"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(line, &slot) != nil || json.Unmarshal(line, &fields) != nil || slot.Type != "title" || slot.Version != 1 ||
		slot.Title == nil || slot.UpdatedAt == nil || slot.Pad == nil || strings.Trim(*slot.Pad, " ") != "" ||
		(slot.Source != "" && slot.Source != "auto" && slot.Source != "user") {
		return false
	}
	if fields["source"] != nil && slot.Source == "" {
		return false
	}
	for key := range fields {
		switch key {
		case "type", "v", "title", "source", "updatedAt", "pad":
		default:
			return false
		}
	}
	return true
}

func readMigrationHeader(file *os.File, sessionID, workspace string) error {
	reader := bufio.NewReader(io.NewSectionReader(file, 0, 64<<10))
	for i := range maxSessionHeaderLines {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return ErrMigrationInvalid
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		if i == 0 && validMigrationTitle(line) {
			continue
		}
		var header migrationHeader
		if !utf8.Valid(line) || json.Unmarshal(line, &header) != nil || header.Type != "session" || header.ID != sessionID || header.CWD != workspace {
			return ErrMigrationInvalid
		}
		return nil
	}
	return ErrMigrationInvalid
}

func CaptureHandoff(path, sessionID, workspace string) (HandoffCursor, error) {
	var cursor HandoffCursor
	if !filepath.IsAbs(path) || !migrationText(sessionID, 128) || !filepath.IsAbs(workspace) {
		return cursor, ErrMigrationInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return cursor, ErrMigrationUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return cursor, ErrMigrationUnavailable
	}
	if err := readMigrationHeader(file, sessionID, workspace); err != nil {
		return cursor, err
	}
	start := info.Size() - 4096
	if start < 0 {
		start = 0
	}
	fence := make([]byte, info.Size()-start)
	if _, err := file.ReadAt(fence, start); err != nil || len(fence) == 0 || fence[len(fence)-1] != '\n' {
		return cursor, ErrMigrationInvalid
	}
	return HandoffCursor{path: path, sessionID: sessionID, workspace: workspace, info: info, offset: info.Size(), fenceOffset: start, fence: fence}, nil
}

func ReadHandoff(cursor HandoffCursor) (summary, entryID, hash string, err error) {
	if cursor.info == nil {
		return "", "", "", ErrMigrationInvalid
	}
	file, openErr := os.Open(cursor.path)
	if openErr != nil {
		return "", "", "", ErrMigrationUnavailable
	}
	defer file.Close()
	info, statErr := file.Stat()
	if statErr != nil || !os.SameFile(cursor.info, info) || info.Size() < cursor.offset || info.Size()-cursor.offset > maxMigrationJournal {
		return "", "", "", ErrMigrationInvalid
	}
	if err := readMigrationHeader(file, cursor.sessionID, cursor.workspace); err != nil {
		return "", "", "", err
	}
	fence := make([]byte, len(cursor.fence))
	if _, err := file.ReadAt(fence, cursor.fenceOffset); err != nil || !bytes.Equal(fence, cursor.fence) {
		return "", "", "", ErrMigrationInvalid
	}
	tail := make([]byte, info.Size()-cursor.offset)
	if len(tail) != 0 {
		if _, err := file.ReadAt(tail, cursor.offset); err != nil {
			return "", "", "", ErrMigrationUnavailable
		}
	}
	complete := bytes.LastIndexByte(tail, '\n')
	if complete < 0 {
		return "", "", "", ErrHandoffNotReady
	}
	count := 0
	for _, line := range bytes.Split(tail[:complete], []byte{'\n'}) {
		var entry struct{ Type, ID, Method, Summary string }
		if len(line) == 0 || !utf8.Valid(line) || json.Unmarshal(line, &entry) != nil {
			return "", "", "", ErrMigrationInvalid
		}
		if entry.Type == "session" || entry.Type == "title" {
			return "", "", "", ErrMigrationInvalid
		}
		if entry.Type != "compaction" || entry.Method != "handoff" {
			continue
		}
		count++
		if count > 1 || !migrationText(entry.ID, 128) || entry.Summary == "" || len(entry.Summary) > maxMigrationSummary || !utf8.ValidString(entry.Summary) {
			return "", "", "", ErrMigrationInvalid
		}
		summary, entryID = entry.Summary, entry.ID
	}
	if count == 0 {
		return "", "", "", ErrHandoffNotReady
	}
	after, statErr := file.Stat()
	current, pathErr := os.Stat(cursor.path)
	if statErr != nil || pathErr != nil || !os.SameFile(info, current) || after.Size() != info.Size() || current.Size() != info.Size() ||
		!after.ModTime().Equal(info.ModTime()) || !current.ModTime().Equal(info.ModTime()) {
		return "", "", "", ErrMigrationInvalid
	}
	return summary, entryID, migrationHash(summary), nil
}

type migrationEntry struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	Parent     *string         `json:"parentId"`
	CustomType string          `json:"customType"`
	Data       json.RawMessage `json:"data"`
	Message    json.RawMessage `json:"message"`
	Model      string          `json:"model"`
}

type migrationMarker struct {
	Version          int    `json:"version"`
	MigrationID      string `json:"migrationId"`
	SourceSessionID  string `json:"sourceSessionId"`
	SourceGeneration int64  `json:"sourceGeneration"`
	TargetSessionID  string `json:"targetSessionId"`
	Workspace        string `json:"workspace"`
	SummaryEntryID   string `json:"summaryEntryId"`
	SummarySHA256    string `json:"summarySha256"`
	ReloadCompleted  bool   `json:"reloadCompleted"`
}

type migrationJournal struct {
	entries []json.RawMessage
	parsed  []migrationEntry
	info    os.FileInfo
	header  migrationHeader
}

func readMigrationJournal(target SessionInfo) (migrationJournal, error) {
	var journal migrationJournal
	file, err := os.Open(target.File)
	if os.IsNotExist(err) {
		return journal, ErrMigrationMarkerNotReady
	}
	if err != nil {
		return journal, ErrMigrationUnavailable
	}
	defer file.Close()
	return readMigrationJournalFile(file, target)
}

func readMigrationJournalFile(file *os.File, target SessionInfo) (migrationJournal, error) {
	var journal migrationJournal
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxMigrationJournal {
		return journal, ErrMigrationInvalid
	}
	data, err := io.ReadAll(io.LimitReader(file, maxMigrationJournal+1))
	if err != nil || len(data) > maxMigrationJournal {
		return journal, ErrMigrationUnavailable
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return journal, ErrMigrationMarkerNotReady
	}
	if !utf8.Valid(data) {
		return journal, ErrMigrationInvalid
	}
	lines := bytes.Split(data[:len(data)-1], []byte{'\n'})
	headerIndex := 0
	if validMigrationTitle(lines[0]) {
		headerIndex++
	}
	if headerIndex >= len(lines) {
		return journal, ErrMigrationMarkerNotReady
	}
	var header migrationHeader
	if json.Unmarshal(lines[headerIndex], &header) != nil || header.Type != "session" || !migrationText(header.ID, 128) ||
		(target.ID != "" && header.ID != target.ID) || header.CWD != target.CWD {
		return journal, ErrMigrationInvalid
	}
	for _, line := range lines[headerIndex+1:] {
		var entry migrationEntry
		if json.Unmarshal(line, &entry) != nil {
			return journal, ErrMigrationInvalid
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(line, &fields) != nil || fields["parentId"] == nil || entry.Type == "" ||
			!migrationText(entry.ID, 128) || entry.Type == "session" || entry.Type == "title" || len(journal.entries) >= maxMigrationEntries {
			return journal, ErrMigrationInvalid
		}
		journal.entries = append(journal.entries, json.RawMessage(line))
		journal.parsed = append(journal.parsed, entry)
	}
	journal.info = info
	journal.header = header
	return journal, nil
}

func migrationSeed(raw json.RawMessage, request MigrationRequest) (string, bool) {
	var message struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &message) != nil || message.Role != "user" || len(message.Content) != 1 || message.Content[0].Type != "text" {
		return "", false
	}
	text := message.Content[0].Text
	return text, text != "" && len(text) <= maxMigrationSummary && migrationHash(text) == request.SummarySHA256 && (request.Summary == "" || request.Summary == text)
}

func validMigrationMetadata(entry migrationEntry) bool {
	switch entry.Type {
	case "model_change":
		provider, model, qualified := strings.Cut(entry.Model, "/")
		return qualified && validModelText(provider) && validModelText(model)
	case "thinking_level_change", "service_tier_change":
		return true
	default:
		return false
	}
}

func verifyMigrationJournal(journal migrationJournal, entries []json.RawMessage, leaf *string, request MigrationRequest, target SessionInfo, settings MigrationSettings) (string, error) {
	if len(entries) != len(journal.entries) {
		return "", ErrMigrationInvalid
	}
	for i := range entries {
		var disk, rpc any
		if json.Unmarshal(journal.entries[i], &disk) != nil || json.Unmarshal(entries[i], &rpc) != nil || !reflect.DeepEqual(disk, rpc) {
			return "", ErrMigrationInvalid
		}
	}
	return validateMigrationJournal(journal, leaf, request, target, &settings)
}

func validateMigrationJournal(journal migrationJournal, leaf *string, request MigrationRequest, target SessionInfo, settings *MigrationSettings) (string, error) {
	index := make(map[string]int, len(journal.parsed))
	markerIndex := -1
	for i, entry := range journal.parsed {
		if _, exists := index[entry.ID]; exists {
			return "", ErrMigrationInvalid
		}
		index[entry.ID] = i
		if entry.CustomType == MigrationMarkerType {
			if entry.Type != "custom" || markerIndex != -1 {
				return "", ErrMigrationInvalid
			}
			markerIndex = i
		}
	}
	// Every parent must exist earlier in append order. This also rejects cycles
	// and orphan branches, even when they are outside the selected chain.
	// Offline inspection has no selected leaf, but marker descendants must not
	// hide conversation turns or unknown initialization entries.
	markerDescendants := make([]bool, len(journal.parsed))
	for i, entry := range journal.parsed {
		if entry.Parent != nil {
			parent, exists := index[*entry.Parent]
			if !exists || parent >= i {
				return "", ErrMigrationInvalid
			}
			markerDescendants[i] = parent == markerIndex || markerDescendants[parent]
			if markerDescendants[i] && !validMigrationMetadata(entry) {
				return "", ErrMigrationInvalid
			}
		}
	}
	if markerIndex == -1 {
		return "", ErrMigrationMarkerNotReady
	}
	var marker migrationMarker
	if json.Unmarshal(journal.parsed[markerIndex].Data, &marker) != nil || marker.Version != request.Version || marker.MigrationID != request.MigrationID ||
		marker.SourceSessionID != request.SourceSessionID || marker.SourceGeneration != request.SourceGeneration || marker.TargetSessionID != target.ID ||
		marker.Workspace != request.Workspace || marker.SummarySHA256 != request.SummarySHA256 || !marker.ReloadCompleted {
		return "", ErrMigrationInvalid
	}
	seedIndex, exists := index[marker.SummaryEntryID]
	if !exists || journal.parsed[seedIndex].Type != "message" || seedIndex >= markerIndex {
		return "", ErrMigrationInvalid
	}
	text, valid := migrationSeed(journal.parsed[seedIndex].Message, request)
	if !valid {
		return "", ErrMigrationInvalid
	}
	markerSeen, seedSeen := false, false
	var persistedModel string
	// Walk through the seed to the root: a matching seed must not conceal an
	// earlier conversation. Only native settings metadata may surround it.
	for current := leaf; current != nil; {
		i, exists := index[*current]
		if !exists {
			return "", ErrMigrationInvalid
		}
		entry := journal.parsed[i]
		switch entry.Type {
		case "custom":
			if i != markerIndex {
				return "", ErrMigrationInvalid
			}
			markerSeen = true
		case "message":
			if i != seedIndex || !markerSeen {
				return "", ErrMigrationInvalid
			}
			seedSeen = true
		default:
			if !validMigrationMetadata(entry) {
				return "", ErrMigrationInvalid
			}
			if entry.Type == "model_change" && persistedModel == "" {
				persistedModel = entry.Model
			}
		}
		current = entry.Parent
	}
	if !markerSeen || !seedSeen || persistedModel == "" ||
		(settings != nil && persistedModel != settings.Provider+"/"+settings.Model) {
		return "", ErrMigrationInvalid
	}
	return text, nil
}

// InspectMigrationCandidate validates durable migration evidence before the
// caller reserves or opens a user-specified recovery candidate. It requires a
// persisted model selector, but does not attest to the native selected leaf,
// live Agent context or runtime settings.
func InspectMigrationCandidate(request MigrationRequest, path string) (SessionInfo, error) {
	var target SessionInfo
	if !validMigrationRequest(request, false) || !filepath.IsAbs(path) || !migrationText(path, 4096) {
		return target, ErrMigrationInvalid
	}
	before, err := os.Lstat(path)
	if err != nil {
		return target, ErrMigrationUnavailable
	}
	if !before.Mode().IsRegular() || before.Size() > maxMigrationJournal {
		return target, ErrMigrationInvalid
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return target, ErrMigrationUnavailable
	}
	defer file.Close()
	journal, err := readMigrationJournalFile(file, SessionInfo{File: path, CWD: request.Workspace})
	if err != nil {
		if errors.Is(err, ErrMigrationMarkerNotReady) {
			return target, ErrMigrationInvalid
		}
		return target, err
	}
	if !os.SameFile(before, journal.info) || before.Size() != journal.info.Size() || !before.ModTime().Equal(journal.info.ModTime()) || journal.header.ID == request.SourceSessionID {
		return target, ErrMigrationInvalid
	}
	target = SessionInfo{ID: journal.header.ID, File: path, CWD: journal.header.CWD}
	var markerID string
	for _, entry := range journal.parsed {
		if entry.CustomType == MigrationMarkerType {
			markerID = entry.ID
		}
	}
	if markerID == "" {
		return SessionInfo{}, ErrMigrationInvalid
	}
	if _, err := validateMigrationJournal(journal, &markerID, request, target, nil); err != nil {
		return SessionInfo{}, ErrMigrationInvalid
	}
	after, err := os.Lstat(path)
	opened, statErr := file.Stat()
	if err != nil || statErr != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) ||
		after.Size() != before.Size() || opened.Size() != before.Size() ||
		!after.ModTime().Equal(before.ModTime()) || !opened.ModTime().Equal(before.ModTime()) {
		return SessionInfo{}, ErrMigrationInvalid
	}
	return target, nil
}

// VerifyMigration independently checks durable evidence and the current Agent
// transcript, including the final persisted model on the selected chain. A
// reload marker alone is never evidence of live model context.
func (c *Client) VerifyMigration(ctx context.Context, request MigrationRequest, target SessionInfo, settings MigrationSettings) error {
	if !validMigrationRequest(request, false) || !migrationText(target.ID, 128) || target.ID == request.SourceSessionID || !filepath.IsAbs(target.File) || target.CWD != request.Workspace {
		return ErrMigrationInvalid
	}
	state, err := c.migrationState(ctx)
	if err != nil {
		return err
	}
	if state.ID != target.ID || state.File != target.File {
		return ErrMigrationInvalid
	}
	journal, err := readMigrationJournal(target)
	if err != nil {
		return err
	}
	raw, err := c.Call(ctx, "get_entries", nil)
	if err != nil {
		return migrationCallError(ctx)
	}
	var snapshot struct {
		Entries []json.RawMessage `json:"entries"`
		Leaf    *string           `json:"leafId"`
	}
	if len(raw) > maxMigrationJournal || json.Unmarshal(raw, &snapshot) != nil {
		return ErrMigrationInvalid
	}
	text, err := verifyMigrationJournal(journal, snapshot.Entries, snapshot.Leaf, request, target, settings)
	if err != nil {
		return err
	}
	raw, err = c.Call(ctx, "get_messages", nil)
	if err != nil {
		return migrationCallError(ctx)
	}
	var live struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if len(raw) > maxMigrationJournal || json.Unmarshal(raw, &live) != nil || live.Messages == nil {
		return ErrMigrationInvalid
	}
	if len(live.Messages) != 1 {
		return ErrMigrationInvalid
	}
	if seed, valid := migrationSeed(live.Messages[0], request); !valid || seed != text {
		return ErrMigrationInvalid
	}
	actual, err := c.MigrationSettings(ctx)
	if err != nil {
		return err
	}
	if actual != settings {
		return ErrMigrationInvalid
	}
	after, err := c.migrationState(ctx)
	if err != nil {
		return err
	}
	current, err := os.Stat(target.File)
	if err != nil || !os.SameFile(journal.info, current) || journal.info.Size() != current.Size() || !journal.info.ModTime().Equal(current.ModTime()) || after.ID != target.ID || after.File != target.File {
		return ErrMigrationInvalid
	}
	return nil
}
