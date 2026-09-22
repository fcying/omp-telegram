package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/shlex"
	"github.com/pelletier/go-toml/v2"
	defaults "omp-telegram"
	"omp-telegram/internal/logging"
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
	ProgressMode          string
	IdleTimeout           time.Duration
	LogLevel              string
	LogFormat             string
	LogComponentLevels    map[string]string
}

const (
	MaxWorkersLimit       = 64
	MaxQueueCapacityLimit = 1024
)

// fileConfig accepts quoted environment references in otherwise numeric fields.
type fileConfig struct {
	Telegram telegramFileConfig `toml:"telegram"`
	OMP      ompFileConfig      `toml:"omp"`
	Storage  storageFileConfig  `toml:"storage"`
	Worker   workerFileConfig   `toml:"worker"`
	Logging  loggingFileConfig  `toml:"logging"`
}

type telegramFileConfig struct {
	Token        string `toml:"token"`
	AllowedUsers []any  `toml:"allowed_users"`
	AllowedChats []any  `toml:"allowed_chats"`
	ProgressMode string `toml:"progress_mode"`
}

type ompFileConfig struct {
	Binary string `toml:"binary"`
	Args   string `toml:"args"`
}

type storageFileConfig struct {
	DataDir               string `toml:"data_dir"`
	WorkspaceRoot         string `toml:"workspace_root"`
	DatabaseRetentionDays any    `toml:"database_retention_days"`
}

type workerFileConfig struct {
	MaxWorkers    any    `toml:"max_workers"`
	QueueCapacity any    `toml:"queue_capacity"`
	IdleTimeout   string `toml:"idle_timeout"`
}

type loggingFileConfig struct {
	Level           string            `toml:"level"`
	Format          string            `toml:"format"`
	ComponentLevels map[string]string `toml:"component_levels"`
}

func Load(path string) (Config, error) {
	executable, err := os.Executable()
	if err != nil {
		return Config{}, errors.New("cannot locate bridge executable")
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return Config{}, errors.New("cannot resolve bridge executable")
	}
	return load(path, filepath.Dir(executable))
}

func load(path, baseDir string) (Config, error) {
	implicit := path == ""
	if path == "" {
		path = filepath.Join(baseDir, "config.toml")
	}
	var c Config
	raw := fileConfig{
		Telegram: telegramFileConfig{
			Token: "${OMP_TELEGRAM_BOT_TOKEN}",
		},
		OMP: ompFileConfig{
			Binary: "omp",
			Args:   "${OMP_TELEGRAM_ARGS}",
		},
		Storage: storageFileConfig{
			WorkspaceRoot:         "${OMP_TELEGRAM_WORKSPACE_ROOT}",
			DataDir:               ".",
			DatabaseRetentionDays: int64(90),
		},
		Worker: workerFileConfig{
			MaxWorkers:    int64(4),
			QueueCapacity: int64(16),
			IdleTimeout:   "30m",
		},
		Logging: loggingFileConfig{
			Level:  "info",
			Format: "text",
		},
	}

	f, err := os.Open(path)
	var reader io.Reader
	if err == nil {
		defer f.Close()
		reader = f
	} else if implicit && errors.Is(err, os.ErrNotExist) {
		reader = strings.NewReader(defaults.DefaultConfig)
	} else {
		if errors.Is(err, os.ErrNotExist) {
			return c, fmt.Errorf("cannot read configuration: %w", os.ErrNotExist)
		}
		return c, errors.New("cannot read configuration")
	}
	d := toml.NewDecoder(reader).DisallowUnknownFields()
	if err = d.Decode(&raw); err != nil {
		// Decoder errors can include source lines containing credentials.
		return c, errors.New("invalid TOML configuration: check syntax, field names and value types")
	}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"telegram.token", &raw.Telegram.Token},
		{"omp.binary", &raw.OMP.Binary},
		{"storage.data_dir", &raw.Storage.DataDir},
		{"logging.level", &raw.Logging.Level},
		{"logging.format", &raw.Logging.Format},
	} {
		value, e := expand(*field.value)
		if e != nil {
			return c, fmt.Errorf("%s: %w", field.name, e)
		}
		*field.value = value
	}

	progressMode, err := expand(raw.Telegram.ProgressMode)
	if err != nil {
		progressMode = ""
	}

	keys := make([]string, 0, len(raw.Logging.ComponentLevels))
	for component := range raw.Logging.ComponentLevels {
		keys = append(keys, component)
	}
	sort.Strings(keys)
	for _, component := range keys {
		if err := logging.ValidateComponent(component); err != nil {
			return c, err
		}
	}
	for _, component := range keys {
		level := raw.Logging.ComponentLevels[component]
		expanded, e := expand(level)
		if e != nil {
			return c, fmt.Errorf("logging.component_levels.%s: %w", component, e)
		}
		raw.Logging.ComponentLevels[component] = expanded
	}
	if err = logging.Validate(logging.Options{Level: raw.Logging.Level, Format: raw.Logging.Format, ComponentLevels: raw.Logging.ComponentLevels}); err != nil {
		return c, err
	}

	argText := raw.OMP.Args
	if argText == "${OMP_TELEGRAM_ARGS}" || argText == "$OMP_TELEGRAM_ARGS" {
		argText = os.Getenv("OMP_TELEGRAM_ARGS")
	} else {
		argText, err = expand(argText)
		if err != nil {
			return c, fmt.Errorf("omp.args: %w", err)
		}
	}
	c.OMPArgs, err = shlex.Split(argText)
	if err != nil {
		return c, errors.New("omp.args contains invalid quoting or escaping")
	}
	if err = omp.ValidateArgs(c.OMPArgs); err != nil {
		return c, err
	}
	c.Token, c.OMP, c.DataDir = raw.Telegram.Token, raw.OMP.Binary, raw.Storage.DataDir
	c.LogLevel, c.LogFormat, c.LogComponentLevels = raw.Logging.Level, raw.Logging.Format, raw.Logging.ComponentLevels
	if c.Token == "" || c.OMP == "" || c.DataDir == "" {
		return c, errors.New("telegram.token, omp.binary and storage.data_dir must be nonempty")
	}
	opmPath, err := exec.LookPath(c.OMP)
	if err != nil {
		return c, errors.New("omp.binary executable not found")
	}
	c.OMP, err = filepath.Abs(opmPath)
	if err != nil {
		return c, errors.New("cannot resolve omp.binary executable path")
	}
	if c.AllowedUsers, err = integers(raw.Telegram.AllowedUsers, "telegram.allowed_users"); err != nil {
		return c, err
	}
	if c.AllowedChats, err = integers(raw.Telegram.AllowedChats, "telegram.allowed_chats"); err != nil {
		return c, err
	}
	workers, err := integer(raw.Worker.MaxWorkers, "worker.max_workers")
	if err != nil {
		return c, err
	}
	capacity, err := integer(raw.Worker.QueueCapacity, "worker.queue_capacity")
	if err != nil {
		return c, err
	}
	retentionDays, err := integer(raw.Storage.DatabaseRetentionDays, "storage.database_retention_days")
	if err != nil {
		return c, err
	}
	if int64(int(workers)) != workers || int64(int(capacity)) != capacity || int64(int(retentionDays)) != retentionDays {
		return c, errors.New("worker.max_workers, worker.queue_capacity, or storage.database_retention_days exceeds platform integer range")
	}
	if workers > MaxWorkersLimit || capacity > MaxQueueCapacityLimit {
		return c, errors.New("worker.max_workers or worker.queue_capacity exceeds the configured safety limit")
	}
	c.MaxWorkers, c.QueueCapacity, c.DatabaseRetentionDays = int(workers), int(capacity), int(retentionDays)
	switch progressMode {
	case "off", "summary", "verbose":
		c.ProgressMode = progressMode
	default:
		c.ProgressMode = "summary"
	}
	idleTimeout, err := parseIdleTimeout(raw.Worker.IdleTimeout, "worker.idle_timeout")
	if err != nil {
		return c, err
	}
	c.IdleTimeout = idleTimeout
	if len(c.AllowedUsers) == 0 || len(c.AllowedChats) == 0 {
		return c, errors.New("telegram.allowed_users and telegram.allowed_chats must be nonempty")
	}
	for _, id := range c.AllowedUsers {
		if id <= 0 {
			return c, errors.New("telegram.allowed_users must contain positive user IDs")
		}
	}
	for _, id := range c.AllowedChats {
		if id == 0 {
			return c, errors.New("telegram.allowed_chats cannot contain zero")
		}
	}
	if c.MaxWorkers < 1 || c.QueueCapacity < 1 {
		return c, errors.New("worker.max_workers and worker.queue_capacity must be positive")
	}
	if c.DatabaseRetentionDays < 0 {
		return c, errors.New("storage.database_retention_days must be zero or positive")
	}
	workspace := raw.Storage.WorkspaceRoot
	if workspace == "${OMP_TELEGRAM_WORKSPACE_ROOT}" || workspace == "$OMP_TELEGRAM_WORKSPACE_ROOT" {
		// This optional default is resolved once; environment contents remain literal.
		workspace = os.Getenv("OMP_TELEGRAM_WORKSPACE_ROOT")
	} else {
		workspace, err = expand(workspace)
		if err != nil {
			return c, fmt.Errorf("storage.workspace_root: %w", err)
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
		return c, errors.New("storage.workspace_root must be a writable directory")
	}
	if !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(baseDir, c.DataDir)
	}
	if err = os.MkdirAll(c.DataDir, 0700); err != nil {
		return c, errors.New("storage.data_dir must be a writable directory")
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

func parseIdleTimeout(value, field string) (time.Duration, error) {
	value, err := expand(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	value = strings.TrimSpace(value)
	if value == "" || value == "0" || strings.EqualFold(value, "disabled") {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be 0, disabled, or a positive Go duration", field)
	}
	return duration, nil
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
