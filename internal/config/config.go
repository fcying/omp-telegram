package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/shlex"
	"github.com/pelletier/go-toml/v2"
	defaults "omp-telegram"
	"omp-telegram/internal/omp"
)

type Config struct {
	Token                 string
	AllowedUsers          []int64
	AllowedChats          []int64
	WorkspaceRoot         string
	OMP                   string
	OMPArgs               []string
	DataDir               string
	MaxWorkers            int
	QueueCapacity         int
	DatabaseRetentionDays int
}

// fileConfig accepts quoted environment references in otherwise numeric fields.
type fileConfig struct {
	Token                 string `toml:"token"`
	AllowedUsers          []any  `toml:"allowed_users"`
	AllowedChats          []any  `toml:"allowed_chats"`
	WorkspaceRoot         string `toml:"workspace_root"`
	OMP                   string `toml:"omp"`
	OMPArgs               string `toml:"omp_args"`
	DataDir               string `toml:"data_dir"`
	MaxWorkers            any    `toml:"max_workers"`
	QueueCapacity         any    `toml:"queue_capacity"`
	DatabaseRetentionDays any    `toml:"database_retention_days"`
}

func Load(path string) (Config, error) {
	executable, err := os.Executable()
	if err != nil {
		return Config{}, fmt.Errorf("locate bridge executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return Config{}, fmt.Errorf("resolve bridge executable: %w", err)
	}
	return load(path, filepath.Dir(executable))
}

func load(path, baseDir string) (Config, error) {
	implicit := path == ""
	if path == "" {
		path = filepath.Join(baseDir, "config.toml")
	}
	var c Config
	raw := fileConfig{Token: "${OMP_TELEGRAM_BOT_TOKEN}", WorkspaceRoot: "${OMP_TELEGRAM_WORKSPACE_ROOT}", OMP: "omp", OMPArgs: "${OMP_TELEGRAM_ARGS}", DataDir: ".", MaxWorkers: int64(4), QueueCapacity: int64(16), DatabaseRetentionDays: int64(90)}
	f, err := os.Open(path)
	var reader io.Reader
	if err == nil {
		defer f.Close()
		reader = f
	} else if implicit && errors.Is(err, os.ErrNotExist) {
		reader = strings.NewReader(defaults.DefaultConfig)
	} else {
		return c, err
	}
	d := toml.NewDecoder(reader).DisallowUnknownFields()
	if err = d.Decode(&raw); err != nil {
		// Decoder errors can include source lines containing credentials.
		return c, errors.New("invalid TOML configuration: check syntax, field names and value types")
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"token", &raw.Token}, {"omp", &raw.OMP}, {"data_dir", &raw.DataDir}} {
		value, e := expand(*field.value)
		if e != nil {
			return c, fmt.Errorf("%s: %w", field.name, e)
		}
		*field.value = value
	}
	argText := raw.OMPArgs
	if argText == "${OMP_TELEGRAM_ARGS}" || argText == "$OMP_TELEGRAM_ARGS" {
		argText = os.Getenv("OMP_TELEGRAM_ARGS")
	} else {
		argText, err = expand(argText)
		if err != nil {
			return c, fmt.Errorf("omp_args: %w", err)
		}
	}
	c.OMPArgs, err = shlex.Split(argText)
	if err != nil {
		return c, errors.New("omp_args contains invalid quoting or escaping")
	}
	if err = omp.ValidateArgs(c.OMPArgs); err != nil {
		return c, err
	}
	c.Token, c.OMP, c.DataDir = raw.Token, raw.OMP, raw.DataDir
	if c.Token == "" || c.OMP == "" || c.DataDir == "" {
		return c, errors.New("token, omp and data_dir must be nonempty")
	}
	ompPath, err := exec.LookPath(c.OMP)
	if err != nil {
		return c, errors.New("omp executable not found")
	}
	c.OMP, err = filepath.Abs(ompPath)
	if err != nil {
		return c, errors.New("cannot resolve omp executable path")
	}
	if c.AllowedUsers, err = integers(raw.AllowedUsers, "allowed_users"); err != nil {
		return c, err
	}
	if c.AllowedChats, err = integers(raw.AllowedChats, "allowed_chats"); err != nil {
		return c, err
	}
	workers, err := integer(raw.MaxWorkers, "max_workers")
	if err != nil {
		return c, err
	}
	capacity, err := integer(raw.QueueCapacity, "queue_capacity")
	if err != nil {
		return c, err
	}
	retentionDays, err := integer(raw.DatabaseRetentionDays, "database_retention_days")
	if err != nil {
		return c, err
	}
	if int64(int(workers)) != workers || int64(int(capacity)) != capacity || int64(int(retentionDays)) != retentionDays {
		return c, errors.New("worker, queue, or retention limit exceeds platform integer range")
	}
	c.MaxWorkers, c.QueueCapacity, c.DatabaseRetentionDays = int(workers), int(capacity), int(retentionDays)
	if len(c.AllowedUsers) == 0 || len(c.AllowedChats) == 0 {
		return c, errors.New("allowed_users and allowed_chats must be nonempty")
	}
	for _, id := range c.AllowedUsers {
		if id <= 0 {
			return c, errors.New("allowed_users must contain positive user IDs")
		}
	}
	for _, id := range c.AllowedChats {
		if id == 0 {
			return c, errors.New("allowed_chats cannot contain zero")
		}
	}
	if c.MaxWorkers < 1 || c.QueueCapacity < 1 {
		return c, errors.New("worker and queue limits must be positive")
	}
	if c.DatabaseRetentionDays < 0 {
		return c, errors.New("database_retention_days must be zero or positive")
	}
	workspace := raw.WorkspaceRoot
	if workspace == "${OMP_TELEGRAM_WORKSPACE_ROOT}" || workspace == "$OMP_TELEGRAM_WORKSPACE_ROOT" {
		// This optional default is resolved once; environment contents remain literal.
		workspace = os.Getenv("OMP_TELEGRAM_WORKSPACE_ROOT")
	} else {
		workspace, err = expand(workspace)
		if err != nil {
			return c, fmt.Errorf("workspace_root: %w", err)
		}
	}
	if workspace == "" {
		workspace = "workspace"
	}
	if !filepath.IsAbs(workspace) {
		workspace = filepath.Join(baseDir, workspace)
	}
	c.WorkspaceRoot = workspace
	if err = os.MkdirAll(c.WorkspaceRoot, 0700); err != nil {
		return c, errors.New("workspace_root must be a writable directory")
	}
	if !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(baseDir, c.DataDir)
	}
	if err = os.MkdirAll(c.DataDir, 0700); err != nil {
		return c, err
	}
	return c, nil
}
func (c Config) Authorized(user, chat int64) bool {
	return contains(c.AllowedUsers, user) && contains(c.AllowedChats, chat)
}
func contains(xs []int64, x int64) bool {
	for _, v := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func integers(values []any, field string) ([]int64, error) {
	result := make([]int64, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			expanded, err := expand(text)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", field, err)
			}
			for item := range strings.SplitSeq(expanded, ",") {
				n, err := strconv.ParseInt(strings.TrimSpace(item), 10, 64)
				if err != nil {
					return nil, fmt.Errorf("%s must contain decimal IDs separated by commas, without empty entries", field)
				}
				result = append(result, n)
			}
			continue
		}
		n, err := integer(value, field)
		if err != nil {
			return nil, err
		}
		result = append(result, n)
	}
	return result, nil
}

func integer(value any, field string) (int64, error) {
	switch value := value.(type) {
	case int64:
		return value, nil
	case string:
		expanded, err := expand(value)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", field, err)
		}
		n, err := strconv.ParseInt(expanded, 10, 64)
		if err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("%s must contain an integer or a decimal integer string", field)
}

// Expand only parsed values, once, so environment contents cannot inject TOML.
func expand(value string) (string, error) {
	if !strings.Contains(value, "$") {
		return value, nil
	}
	var result strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '$' {
			result.WriteByte(value[i])
			i++
			continue
		}
		i++
		if i < len(value) && value[i] == '$' {
			result.WriteByte('$')
			i++
			continue
		}
		braced := i < len(value) && value[i] == '{'
		if braced {
			i++
		}
		start := i
		if i >= len(value) || !envNameStart(value[i]) {
			return "", errors.New("invalid environment reference; use $VAR, ${VAR} or $$")
		}
		for i < len(value) && (envNameStart(value[i]) || value[i] >= '0' && value[i] <= '9') {
			i++
		}
		name := value[start:i]
		if braced {
			if i >= len(value) || value[i] != '}' {
				return "", errors.New("invalid environment reference; expected closing brace")
			}
			i++
		}
		replacement, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		result.WriteString(replacement)
	}
	return result.String(), nil
}

func envNameStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
