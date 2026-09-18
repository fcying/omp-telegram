package omp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// ModelRole is an ordered native cycle role and its configured selector.
// Default may have no selector when native model resolution supplies it.
type ModelRole struct {
	Role, Selector string
}

// Model contains only the native model identity, not provider configuration.
type Model struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

func validRole(role string) bool {
	if role == "" || len(role) > 128 {
		return false
	}
	for _, ch := range role {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

func validModelText(text string) bool {
	if text == "" || len(text) > 1024 || !utf8.ValidString(text) {
		return false
	}
	for _, ch := range text {
		if unicode.IsSpace(ch) || unicode.IsControl(ch) || unicode.Is(unicode.Cf, ch) {
			return false
		}
	}
	return true
}

// CycleRoles reads native settings in the runtime working directory, preserving
// the runtime's ordered --config overlays through native PI_CONFIG_FILES support.
func CycleRoles(ctx context.Context, cfg Config) ([]ModelRole, error) {
	files, err := cycleConfigFiles(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	orderRaw, err := readModelConfig(ctx, cfg, "cycleOrder", files)
	if err != nil {
		return nil, err
	}
	rolesRaw, err := readModelConfig(ctx, cfg, "modelRoles", files)
	if err != nil {
		return nil, err
	}
	var order []string
	var selectors map[string]string
	if json.Unmarshal(orderRaw, &order) != nil || json.Unmarshal(rolesRaw, &selectors) != nil || order == nil || selectors == nil {
		return nil, errors.New("omp: invalid native model role settings")
	}
	roles := make([]ModelRole, 0, len(order))
	for _, role := range order {
		selector := selectors[role]
		if !validRole(role) || (selector != "" && !validModelText(selector)) {
			return nil, errors.New("omp: invalid native model role settings")
		}
		if selector == "" && role != "default" {
			continue
		}
		roles = append(roles, ModelRole{Role: role, Selector: selector})
	}
	return roles, nil
}

func cycleConfigFiles(cfg Config) ([]string, error) {
	var files []string
	for i := 0; i < len(cfg.Args); i++ {
		name, value, equal := strings.Cut(cfg.Args[i], "=")
		switch name {
		case "--profile", "--smol", "--slow", "--plan":
			return nil, errors.New("omp: model role menu unavailable with runtime settings or role overrides")
		case "--config":
			if !equal {
				i++
				if i >= len(cfg.Args) || strings.HasPrefix(cfg.Args[i], "--") {
					return nil, errors.New("omp: config overlay path is missing")
				}
				value = cfg.Args[i]
			}
			if value == "" {
				return nil, errors.New("omp: config overlay path is missing")
			}
			if value == "~" || strings.HasPrefix(value, "~/") {
				home, err := os.UserHomeDir()
				if err != nil {
					return nil, errors.New("omp: cannot resolve config overlay home directory")
				}
				value = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(value, "~"), "/"))
			}
			if !filepath.IsAbs(value) {
				value = filepath.Join(cfg.CWD, value)
			}
			path, err := filepath.Abs(value)
			if err != nil {
				return nil, errors.New("omp: cannot resolve config overlay path")
			}
			files = append(files, path)
		}
	}
	return files, nil
}

func readModelConfig(ctx context.Context, cfg Config, key string, configFiles []string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Binary == "" {
		cfg.Binary = "omp"
	}
	cmd := exec.Command(cfg.Binary, "config", "get", key, "--json")
	cmd.Dir = cfg.CWD
	defer func() {
		for _, file := range cmd.ExtraFiles {
			file.Close()
		}
	}()
	if len(configFiles) != 0 {
		files := filepath.SplitList(os.Getenv("PI_CONFIG_FILES"))
		for _, path := range configFiles {
			if strings.ContainsRune(path, os.PathListSeparator) {
				// The native environment list has no escaping for ':' in Linux paths.
				file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
				if err != nil {
					return nil, errors.New("omp: cannot open config overlay")
				}
				info, err := file.Stat()
				if err != nil || !info.Mode().IsRegular() {
					file.Close()
					return nil, errors.New("omp: config overlay must be a regular file")
				}
				path = "/proc/self/fd/" + strconv.Itoa(3+len(cmd.ExtraFiles))
				cmd.ExtraFiles = append(cmd.ExtraFiles, file)
			}
			files = append(files, path)
		}
		cmd.Env = append(cmd.Environ(), "PI_CONFIG_FILES="+strings.Join(files, string(os.PathListSeparator)))
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.New("omp: cannot create model settings output pipe")
	}
	defer outRead.Close()
	stderr, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		outWrite.Close()
		return nil, errors.New("omp: cannot discard model settings diagnostics")
	}
	cmd.Stdout, cmd.Stderr = outWrite, stderr
	waited, err := startProcess(cmd)
	outWrite.Close()
	stderr.Close()
	if err != nil {
		return nil, errors.New("omp: cannot read native model settings; check binary and working directory")
	}
	type configResult struct {
		data []byte
		err  error
	}
	results := make(chan configResult, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(outRead, maxFrame+1))
		results <- configResult{data, err}
	}()
	var result configResult
	var waitErr error
	readFinished, exited := false, false
	select {
	case result = <-results:
		readFinished = true
	case waitErr = <-waited:
		exited = true
	case <-ctx.Done():
	}
	if readFinished && result.err == nil && len(result.data) <= maxFrame {
		select {
		case waitErr = <-waited:
			exited = true
		case <-ctx.Done():
		}
	}
	// Reap descendants even when the leader exits with an inherited stdout open.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if !exited {
		waitErr = <-waited
	}
	if !readFinished {
		timer := time.NewTimer(time.Second)
		select {
		case result = <-results:
		case <-timer.C:
			outRead.Close()
			result = <-results
		case <-ctx.Done():
			outRead.Close()
			result = <-results
		}
		timer.Stop()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if waitErr != nil || result.err != nil || len(result.data) > maxFrame {
		return nil, errors.New("omp: native model settings query failed")
	}
	var envelope struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if json.Unmarshal(result.data, &envelope) != nil || envelope.Key != key || len(envelope.Value) == 0 {
		return nil, errors.New("omp: invalid native model settings response")
	}
	return envelope.Value, nil
}

func parseModelOutput(text string) (Model, bool) {
	var model Model
	if !strings.HasPrefix(text, "Model set to ") || !strings.HasSuffix(text, ".") {
		return model, false
	}
	identity := strings.TrimSuffix(strings.TrimPrefix(text, "Model set to "), ".")
	provider, id, ok := strings.Cut(identity, "/")
	if !ok || !validModelText(provider) || !validModelText(id) {
		return model, false
	}
	return Model{Provider: provider, ID: id}, true
}

// SetModelRole delegates model and thinking-level resolution to the native local
// command. Callers must exclude prompts and other model mutations until it ends.
func (c *Client) SetModelRole(ctx context.Context, role string) (Model, error) {
	var model Model
	if !validRole(role) {
		return model, errors.New("omp: invalid model role")
	}
	if err := ctx.Err(); err != nil {
		return model, err
	}
	c.mu.Lock()
	if c.metadataBusy || c.metadataUnavailable {
		c.mu.Unlock()
		return model, errors.New("omp: native model role command unavailable")
	}
	query := &metadataQuery{modelRole: true, output: make(chan string, 1)}
	c.metadataBusy = true
	c.metadata = query
	c.mu.Unlock()
	completed := false
	defer func() {
		c.mu.Lock()
		c.metadata = nil
		c.metadataBusy = false
		if !completed {
			c.metadataUnavailable = true
		}
		c.mu.Unlock()
		if !completed {
			c.fail(errors.New("omp: native model role command failed; execution state uncertain"))
		}
	}()
	raw, err := c.Call(ctx, "prompt", map[string]any{"message": "/model @" + role})
	if err != nil {
		return model, err
	}
	var ack struct {
		AgentInvoked *bool `json:"agentInvoked"`
	}
	if json.Unmarshal(raw, &ack) != nil || ack.AgentInvoked == nil || *ack.AgentInvoked {
		return model, errors.New("omp: model role command was not local")
	}
	var text string
	select {
	case text = <-query.output:
	default:
		return model, errors.New("omp: native model role success output missing")
	}
	selected, ok := parseModelOutput(text)
	if !ok {
		return model, errors.New("omp: invalid native model identity")
	}
	raw, err = c.Call(ctx, "get_state", nil)
	if err != nil {
		return model, err
	}
	var state struct {
		Model Model `json:"model"`
	}
	if json.Unmarshal(raw, &state) != nil || state.Model != selected {
		return model, errors.New("omp: native model role state mismatch")
	}
	if err := ctx.Err(); err != nil {
		return model, err
	}
	completed = true
	return state.Model, nil
}
