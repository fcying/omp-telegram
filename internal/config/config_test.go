package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func configFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGE_TEST_ROOT", root)
	t.Setenv("BRIDGE_TEST_OMP", exe)
	t.Setenv("BRIDGE_TEST_TOKEN", "test-secret")
	t.Setenv("OMP_TELEGRAM_PROGRESS_MODE", "")
	t.Setenv("OMP_TELEGRAM_ARGS", "")
	source := `[telegram]
token = "${BRIDGE_TEST_TOKEN}"
allowed_users = [7]
allowed_chats = [-10]

[omp]
binary = "$BRIDGE_TEST_OMP"

[storage]
data_dir = "${BRIDGE_TEST_ROOT}/data"
workspace_root = "$BRIDGE_TEST_ROOT"

[worker]
max_workers = 4
queue_capacity = 16
`
	return filepath.Join(root, "config.toml"), source
}

func setTOMLField(source, table, key, value string) string {
	header := "[" + table + "]"
	assignment := key + " = " + value
	headerStart := strings.Index(source, header+"\n")
	if headerStart < 0 {
		return strings.TrimRight(source, "\n") + "\n\n" + header + "\n" + assignment + "\n"
	}
	sectionStart := headerStart + len(header) + 1
	sectionEnd := len(source)
	if next := strings.Index(source[sectionStart:], "\n["); next >= 0 {
		sectionEnd = sectionStart + next + 1
	}
	section := source[sectionStart:sectionEnd]
	for lineStart := 0; lineStart < len(section); {
		lineEnd := strings.IndexByte(section[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(section)
		} else {
			lineEnd += lineStart + 1
		}
		line := strings.TrimSpace(section[lineStart:lineEnd])
		if name, _, ok := strings.Cut(line, "="); ok && strings.TrimSpace(name) == key {
			return source[:sectionStart+lineStart] + assignment + "\n" + source[sectionStart+lineEnd:]
		}
		lineStart = lineEnd
	}
	prefix := source[:sectionEnd]
	if !strings.HasSuffix(prefix, "\n") {
		prefix += "\n"
	}
	return prefix + assignment + "\n" + source[sectionEnd:]
}
func loadSource(t *testing.T, path, source string) (Config, error) {
	t.Helper()
	return loadSourceAt(t, path, source, filepath.Dir(path))
}

func loadSourceAt(t *testing.T, path, source, baseDir string) (Config, error) {
	t.Helper()
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	return load(path, baseDir)
}

func TestTOMLAuthorization(t *testing.T) {
	path, source := configFixture(t)
	c, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Authorized(7, -10) || c.Authorized(8, -10) || c.Authorized(7, -20) {
		t.Fatal("authorization must require both allowlists")
	}
}

func TestProgressMode(t *testing.T) {
	path, source := configFixture(t)
	c, err := loadSource(t, path, source)
	if err != nil || c.ProgressMode != "summary" {
		t.Fatalf("default progress mode = %q, err = %v", c.ProgressMode, err)
	}
	for _, mode := range []string{"off", "summary", "verbose"} {
		c, err = loadSource(t, path, setTOMLField(source, "telegram", "progress_mode", strconv.Quote(mode)))
		if err != nil || c.ProgressMode != mode {
			t.Fatalf("progress mode %q = %q, err = %v", mode, c.ProgressMode, err)
		}
	}
	c, err = loadSource(t, path, setTOMLField(source, "telegram", "progress_mode", `"detailed"`))
	if err != nil || c.ProgressMode != "summary" {
		t.Fatalf("invalid progress mode = %q, err = %v", c.ProgressMode, err)
	}
	empty := setTOMLField(source, "telegram", "progress_mode", `""`)
	c, err = loadSource(t, path, empty)
	if err != nil || c.ProgressMode != "summary" {
		t.Fatalf("empty progress mode = %q, err = %v", c.ProgressMode, err)
	}
}

func TestProgressModeEnvironmentReference(t *testing.T) {
	path, source := configFixture(t)
	source = setTOMLField(source, "telegram", "progress_mode", `"$OMP_TELEGRAM_PROGRESS_MODE"`)
	for _, tc := range []struct {
		name        string
		environment string
		want        string
	}{
		{name: "verbose", environment: "verbose", want: "verbose"},
		{name: "off", environment: "off", want: "off"},
		{name: "empty", environment: "", want: "summary"},
		{name: "invalid", environment: "invalid", want: "summary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_PROGRESS_MODE", tc.environment)
			c, err := loadSource(t, path, source)
			if err != nil || c.ProgressMode != tc.want {
				t.Fatalf("progress mode = %q, want %q, err = %v", c.ProgressMode, tc.want, err)
			}
		})
	}
}

func TestProgressModeEnvironmentExpansion(t *testing.T) {
	path, source := configFixture(t)
	configured := setTOMLField(source, "telegram", "progress_mode", `"${BRIDGE_TEST_PROGRESS}"`)
	t.Setenv("BRIDGE_TEST_PROGRESS", "verbose")
	c, err := loadSource(t, path, configured)
	if err != nil || c.ProgressMode != "verbose" {
		t.Fatalf("expanded progress mode=%q err=%v", c.ProgressMode, err)
	}
	t.Setenv("BRIDGE_TEST_PROGRESS", "${BRIDGE_TEST_SECOND_PROGRESS}")
	t.Setenv("BRIDGE_TEST_SECOND_PROGRESS", "off")
	c, err = loadSource(t, path, configured)
	if err != nil || c.ProgressMode != "summary" {
		t.Fatalf("nested progress mode = %q, err = %v", c.ProgressMode, err)
	}
	t.Setenv("BRIDGE_TEST_PROGRESS", "summary")
	escaped := setTOMLField(source, "telegram", "progress_mode", `"$${BRIDGE_TEST_PROGRESS}"`)
	c, err = loadSource(t, path, escaped)
	if err != nil || c.ProgressMode != "summary" {
		t.Fatalf("escaped progress mode = %q, err = %v", c.ProgressMode, err)
	}
	t.Setenv("BRIDGE_TEST_PROGRESS", "SECRET_INVALID_PROGRESS")
	c, err = loadSource(t, path, configured)
	if err != nil || c.ProgressMode != "summary" {
		t.Fatalf("invalid expanded progress mode = %q, err = %v", c.ProgressMode, err)
	}
	if err := os.Unsetenv("BRIDGE_TEST_PROGRESS"); err != nil {
		t.Fatal(err)
	}
	c, err = loadSource(t, path, configured)
	if err != nil || c.ProgressMode != "summary" {
		t.Fatalf("missing progress mode = %q, err = %v", c.ProgressMode, err)
	}
}

func TestIdleTimeout(t *testing.T) {
	path, source := configFixture(t)
	c, err := loadSource(t, path, source)
	if err != nil || c.IdleTimeout != 30*time.Minute {
		t.Fatalf("default idle timeout = %v, error %v", c.IdleTimeout, err)
	}
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"\"0\"", 0},
		{"\"disabled\"", 0},
		{"\"30m\"", 30 * time.Minute},
		{"\"${BRIDGE_TEST_IDLE_TIMEOUT}\"", 45 * time.Second},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("BRIDGE_TEST_IDLE_TIMEOUT", "45s")
			c, err := loadSource(t, path, setTOMLField(source, "worker", "idle_timeout", tc.value))
			if err != nil || c.IdleTimeout != tc.want {
				t.Fatalf("idle timeout = %v, want %v, error %v", c.IdleTimeout, tc.want, err)
			}
		})
	}
	for _, value := range []string{"\"-1s\"", "\"bad\"", "\"1\""} {
		if _, err := loadSource(t, path, setTOMLField(source, "worker", "idle_timeout", value)); err == nil {
			t.Fatalf("invalid idle timeout accepted: %s", value)
		}
	}
}

func TestRelativeOMPPathIsMadeAbsoluteAfterValidation(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "omp"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	path, source := configFixture(t)
	source = setTOMLField(source, "omp", "binary", `"./bin/omp"`)
	c, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "bin", "omp")
	if c.OMP != want {
		t.Fatalf("omp path = %q, want validated executable %q", c.OMP, want)
	}
}

func TestOMPArgsRejectInvalidLaunchOverrides(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("OMP_TELEGRAM_TEST_MISSING_ARG", "")
	if err := os.Unsetenv("OMP_TELEGRAM_TEST_MISSING_ARG"); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{
		`42`,
		`["--config", "file.yml"]`,
		`'--config ${OMP_TELEGRAM_TEST_MISSING_ARG}'`,
		`'--config "secret-unclosed'`,
		`'--mode=secret-invalid-mode'`,
		`'--cwd /tmp'`,
		`'--resume=secret-session'`,
		`'-rsecret-session'`,
		`'--continue'`,
		`'-c'`,
		`'--print'`,
		`'-p'`,
		`'--no-session'`,
		`'--'`,
	} {
		if _, err := loadSource(t, path, setTOMLField(source, "omp", "args", args)); err == nil {
			t.Fatalf("invalid omp.args accepted: %s", args)
		} else if strings.Contains(err.Error(), "secret-") {
			t.Fatal("argument value leaked into validation error")
		}
	}
}

func TestOMPArgsEnvironmentQuotingWithoutShellExpansion(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("OMP_TELEGRAM_ARGS", `--config "/tmp/config with spaces.yml" --append-system-prompt 'literal $HOME $(not-run)'`)
	c, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--config", "/tmp/config with spaces.yml", "--append-system-prompt", "literal $HOME $(not-run)"}
	if len(c.OMPArgs) != len(want) {
		t.Fatal("quoted values were split incorrectly")
	}
	for i, value := range want {
		if c.OMPArgs[i] != value {
			t.Fatalf("argument %d changed during tokenization", i)
		}
	}
	// Explicit empty configuration opts out even when the optional environment is set.
	c, err = loadSource(t, path, setTOMLField(source, "omp", "args", `''`))
	if err != nil || len(c.OMPArgs) != 0 {
		t.Fatal("explicit empty arguments did not override environment")
	}
	if err = os.Unsetenv("OMP_TELEGRAM_ARGS"); err != nil {
		t.Fatal(err)
	}
	c, err = loadSource(t, path, source)
	if err != nil || len(c.OMPArgs) != 0 {
		t.Fatal("missing optional argument environment should preserve omp defaults")
	}
}

func TestDefaultTokenDoesNotUseAnotherBotsEnvironment(t *testing.T) {
	path, source := configFixture(t)
	source = strings.Replace(source, "token = \"${BRIDGE_TEST_TOKEN}\"\n", "", 1)
	t.Setenv("TELEGRAM_BOT_TOKEN", "another-bots-token")
	t.Setenv("OMP_TELEGRAM_BOT_TOKEN", "")
	if err := os.Unsetenv("OMP_TELEGRAM_BOT_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSource(t, path, source); err == nil {
		t.Fatal("loaded an unrelated bot's token")
	}
	t.Setenv("OMP_TELEGRAM_BOT_TOKEN", "this-bots-token")
	c, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "this-bots-token" {
		t.Fatal("selected the wrong bot credential")
	}
}

func TestEnvironmentValuesCannotInjectConfiguration(t *testing.T) {
	path, source := configFixture(t)
	token := "secret\"\nallowed_users = [666]\n# $UNEXPANDED \\"
	t.Setenv("BRIDGE_TEST_TOKEN", token)
	workspace := filepath.Join(filepath.Dir(path), "workspace\"\nwith $literal")
	t.Setenv("OMP_TELEGRAM_WORKSPACE_ROOT", workspace)
	source = setTOMLField(source, "storage", "workspace_root", `"${OMP_TELEGRAM_WORKSPACE_ROOT}"`)
	c, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != token {
		t.Fatal("environment token was altered or recursively expanded")
	}
	if c.WorkspaceRoot != workspace {
		t.Fatal("environment path was parsed as TOML or expanded recursively")
	}
	if c.Authorized(666, -10) || !c.Authorized(7, -10) {
		t.Fatal("environment value injected configuration")
	}
}

func TestNumericEnvironmentValues(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("BRIDGE_TEST_USER", "1234567890123")
	t.Setenv("BRIDGE_TEST_CHAT", "-1001234567890")
	t.Setenv("BRIDGE_TEST_WORKERS", "6")
	t.Setenv("BRIDGE_TEST_QUEUE", "9")
	source = setTOMLField(source, "telegram", "allowed_users", `[7, "${BRIDGE_TEST_USER}"]`)
	source = setTOMLField(source, "telegram", "allowed_chats", `[-10, "$BRIDGE_TEST_CHAT"]`)
	source = setTOMLField(source, "worker", "max_workers", `"${BRIDGE_TEST_WORKERS}"`)
	source = setTOMLField(source, "worker", "queue_capacity", `"$BRIDGE_TEST_QUEUE"`)
	c, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Authorized(1234567890123, -1001234567890) || c.MaxWorkers != 6 || c.QueueCapacity != 9 {
		t.Fatal("numeric environment configuration not applied")
	}
	for _, bad := range []string{"", "4.5", "9223372036854775808", "secret-invalid-number"} {
		t.Setenv("BRIDGE_TEST_WORKERS", bad)
		_, err = loadSource(t, path, source)
		if err == nil {
			t.Fatalf("invalid integer accepted: %q", bad)
		}
		if bad != "" && strings.Contains(err.Error(), bad) {
			t.Fatal("expanded value leaked into error")
		}
	}
}

func TestWorkerLimits(t *testing.T) {
	path, source := configFixture(t)
	for _, tc := range []struct {
		table, key, value string
		limit             int
	}{
		{"worker", "max_workers", strconv.Itoa(MaxWorkersLimit), MaxWorkersLimit},
		{"worker", "queue_capacity", strconv.Itoa(MaxQueueCapacityLimit), MaxQueueCapacityLimit},
	} {
		c, err := loadSource(t, path, setTOMLField(source, tc.table, tc.key, tc.value))
		if err != nil {
			t.Fatalf("rejected safe %s: %v", tc.key, err)
		}
		if got := map[string]int{"max_workers": c.MaxWorkers, "queue_capacity": c.QueueCapacity}[tc.key]; got != tc.limit {
			t.Fatalf("%s = %d, want %d", tc.key, got, tc.limit)
		}
	}
	for _, tc := range []struct {
		table, key, value string
	}{
		{"worker", "max_workers", strconv.Itoa(MaxWorkersLimit + 1)},
		{"worker", "queue_capacity", strconv.Itoa(MaxQueueCapacityLimit + 1)},
	} {
		if _, err := loadSource(t, path, setTOMLField(source, tc.table, tc.key, tc.value)); err == nil {
			t.Fatalf("accepted unsafe %s", tc.key)
		}
	}
}

func TestDatabaseRetentionDays(t *testing.T) {
	path, source := configFixture(t)
	c, err := loadSource(t, path, source)
	if err != nil || c.DatabaseRetentionDays != 90 {
		t.Fatalf("default database retention = %d, error %v", c.DatabaseRetentionDays, err)
	}
	for _, tc := range []struct {
		value string
		want  int
	}{{"0", 0}, {"90", 90}, {`"${BRIDGE_TEST_RETENTION}"`, 180}} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("BRIDGE_TEST_RETENTION", "180")
			c, err := loadSource(t, path, setTOMLField(source, "storage", "database_retention_days", tc.value))
			if err != nil || c.DatabaseRetentionDays != tc.want {
				t.Fatalf("database retention = %d, error %v", c.DatabaseRetentionDays, err)
			}
		})
	}
	for _, value := range []string{"-1", "1.5", "true"} {
		if _, err := loadSource(t, path, setTOMLField(source, "storage", "database_retention_days", value)); err == nil {
			t.Fatalf("invalid retention accepted: %s", value)
		}
	}
}

func TestCommaSeparatedAllowlists(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("OMP_TELEGRAM_ALLOWED_USERS", " 8, 9 ")
	t.Setenv("OMP_TELEGRAM_ALLOWED_CHATS", "7, -1001234567890")
	source = setTOMLField(source, "telegram", "allowed_users", `[7, "${OMP_TELEGRAM_ALLOWED_USERS}"]`)
	source = setTOMLField(source, "telegram", "allowed_chats", `[-10, "${OMP_TELEGRAM_ALLOWED_CHATS}"]`)
	c, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []int64{7, 8, 9} {
		for _, chat := range []int64{-10, 7, -1001234567890} {
			if !c.Authorized(user, chat) {
				t.Fatalf("authorized pair denied: %d/%d", user, chat)
			}
		}
	}
	if c.Authorized(666, 7) || c.Authorized(8, -99) {
		t.Fatal("unauthorized user or chat accepted")
	}
	for _, field := range []string{"OMP_TELEGRAM_ALLOWED_USERS", "OMP_TELEGRAM_ALLOWED_CHATS"} {
		for _, bad := range []string{"", "7,", ",7", "7,,8", "7,secret-invalid", "7,9223372036854775808", "7,0"} {
			t.Run(field+"/"+bad, func(t *testing.T) {
				t.Setenv(field, bad)
				_, err := loadSource(t, path, source)
				if err == nil {
					t.Fatal("invalid allowlist accepted")
				}
				if strings.Contains(err.Error(), "secret-invalid") {
					t.Fatal("expanded value leaked")
				}
			})
		}
	}
}

func TestMissingEnvironmentAndLiteralDollar(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("BRIDGE_TEST_MISSING", "present")
	if err := os.Unsetenv("BRIDGE_TEST_MISSING"); err != nil {
		t.Fatal(err)
	}
	_, err := loadSource(t, path, strings.Replace(source, "${BRIDGE_TEST_TOKEN}", "${BRIDGE_TEST_MISSING}", 1))
	if err == nil || !strings.Contains(err.Error(), "BRIDGE_TEST_MISSING") {
		t.Fatal("missing environment variable must produce a useful error")
	}
	literal := strings.Replace(source, `token = "${BRIDGE_TEST_TOKEN}"`, `token = 'literal$$TOKEN/$${BRIDGE_TEST_MISSING}'`, 1)
	c, err := loadSource(t, path, literal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "literal$TOKEN/${BRIDGE_TEST_MISSING}" {
		t.Fatal("literal dollar escape failed")
	}
	for _, bad := range []string{"${}", "${BRIDGE_TEST_TOKEN", "$", "${BRIDGE_TEST_TOKEN:-fallback}", "$9"} {
		if _, err = loadSource(t, path, strings.Replace(source, "${BRIDGE_TEST_TOKEN}", bad, 1)); err == nil {
			t.Fatalf("invalid reference accepted: %s", bad)
		}
	}
}

func TestInvalidTOMLAndTypesDoNotLeakSecrets(t *testing.T) {
	path, source := configFixture(t)
	cases := []string{
		`token = "secret-value"` + "\ninvalid = [\n",
		setTOMLField(source, "telegram", "allowed_users", `[0]`),
		setTOMLField(source, "worker", "max_workers", `true`),
		setTOMLField(source, "worker", "max_workers", `1.5`),
		setTOMLField(source, "telegram", "token", `""`),
	}
	for _, input := range cases {
		_, err := loadSource(t, path, input)
		if err == nil {
			t.Fatal("invalid configuration accepted")
		}
		if strings.Contains(err.Error(), "secret-value") {
			t.Fatal("parser source leaked into error")
		}
	}
}

func TestWorkspaceRootSelection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		env   string
		unset bool
		want  string
	}{
		{name: "omitted absent", unset: true, want: "workspace"},
		{name: "omitted empty", want: "workspace"},
		{name: "omitted configured", env: "environment", want: "environment"},
		{name: "reference absent", value: `"${OMP_TELEGRAM_WORKSPACE_ROOT}"`, unset: true, want: "workspace"},
		{name: "reference empty", value: `"$OMP_TELEGRAM_WORKSPACE_ROOT"`, want: "workspace"},
		{name: "explicit empty", value: `""`, env: "environment", want: "workspace"},
		{name: "explicit directory", value: `"configured/nested"`, env: "environment", want: "configured/nested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, source := configFixture(t)
			baseDir := t.TempDir()
			cwd := t.TempDir()
			t.Chdir(cwd)
			t.Setenv("OMP_TELEGRAM_WORKSPACE_ROOT", tc.env)
			if tc.unset {
				if err := os.Unsetenv("OMP_TELEGRAM_WORKSPACE_ROOT"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.value == "" {
				source = strings.Replace(source, "workspace_root = \"$BRIDGE_TEST_ROOT\"\n", "", 1)
			} else {
				source = setTOMLField(source, "storage", "workspace_root", tc.value)
			}
			c, err := loadSourceAt(t, path, source, baseDir)
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(baseDir, tc.want)
			if c.WorkspaceRoot != want {
				t.Fatalf("workspace root = %q, want %q", c.WorkspaceRoot, want)
			}
			info, err := os.Stat(want)
			if err != nil {
				t.Fatal(err)
			}
			if !info.IsDir() || info.Mode().Perm() != 0700 {
				t.Fatalf("workspace root mode = %v", info.Mode())
			}
		})
	}
}

func TestBridgePathsIgnoreCallerAndConfigDirectories(t *testing.T) {
	for _, configLocation := range []string{"default", "absolute", "relative"} {
		for _, directories := range []string{"omitted", "relative", "absolute"} {
			t.Run(configLocation+"/"+directories, func(t *testing.T) {
				path, source := configFixture(t)
				baseDir := t.TempDir()
				cwd := t.TempDir()
				t.Chdir(cwd)
				t.Setenv("OMP_TELEGRAM_WORKSPACE_ROOT", "")
				dataValue, workspaceValue := "", ""
				wantData, wantWorkspace := baseDir, filepath.Join(baseDir, "workspace")
				switch directories {
				case "relative":
					dataValue = `"state/nested"`
					workspaceValue = `"jobs/nested"`
					wantData = filepath.Join(baseDir, "state", "nested")
					wantWorkspace = filepath.Join(baseDir, "jobs", "nested")
				case "absolute":
					dataValue = `"$BRIDGE_TEST_ROOT/data"`
					workspaceValue = `"$BRIDGE_TEST_ROOT/jobs"`
					wantData = filepath.Join(filepath.Dir(path), "data")
					wantWorkspace = filepath.Join(filepath.Dir(path), "jobs")
				}
				if dataValue != "" {
					source = setTOMLField(source, "storage", "data_dir", dataValue)
				} else {
					source = strings.Replace(source, "data_dir = \"${BRIDGE_TEST_ROOT}/data\"\n", "", 1)
				}
				if workspaceValue != "" {
					source = setTOMLField(source, "storage", "workspace_root", workspaceValue)
				} else {
					source = strings.Replace(source, "workspace_root = \"$BRIDGE_TEST_ROOT\"\n", "", 1)
				}
				loadPath := path
				switch configLocation {
				case "default":
					path = filepath.Join(baseDir, "config.toml")
					loadPath = ""
				case "relative":
					var err error
					loadPath, err = filepath.Rel(cwd, path)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(path, []byte(source), 0600); err != nil {
					t.Fatal(err)
				}
				c, err := load(loadPath, baseDir)
				if err != nil {
					t.Fatal(err)
				}
				if c.DataDir != wantData || c.WorkspaceRoot != wantWorkspace {
					t.Fatalf("directories = (%q, %q), want (%q, %q)", c.DataDir, c.WorkspaceRoot, wantData, wantWorkspace)
				}
				for _, directory := range []string{wantData, wantWorkspace} {
					info, err := os.Stat(directory)
					if err != nil {
						t.Fatal(err)
					}
					if !info.IsDir() {
						t.Fatalf("%q is not a directory", directory)
					}
				}
			})
		}
	}
}

func TestBundledConfigFallback(t *testing.T) {
	base := t.TempDir()
	t.Setenv("OMP_TELEGRAM_BOT_TOKEN", "test-token")
	t.Setenv("OMP_TELEGRAM_ALLOWED_USERS", "7")
	t.Setenv("OMP_TELEGRAM_ALLOWED_CHATS", "-10")
	t.Setenv("OMP_TELEGRAM_ARGS", "")
	t.Setenv("OMP_TELEGRAM_WORKSPACE_ROOT", "")
	t.Setenv("OMP_TELEGRAM_PROGRESS_MODE", "verbose")
	// Config loading only needs an executable lookup, not a running omp.
	if err := os.WriteFile(filepath.Join(base, "omp"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", base)
	c, err := load("", base)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Authorized(7, -10) || c.Authorized(8, -10) || c.DataDir != base || c.WorkspaceRoot != filepath.Join(base, "workspace") {
		t.Fatal("bundled config lost authorization or executable-relative paths")
	}
	if c.ProgressMode != "verbose" {
		t.Fatalf("bundled config ignored progress mode environment: %q", c.ProgressMode)
	}
	path := filepath.Join(base, "config.toml")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fallback wrote a config file: %v", err)
	}
	if _, err := load(path, base); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit missing config did not fail: %v", err)
	}
	if err := os.WriteFile(path, []byte("invalid TOML"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := load("", base); err == nil {
		t.Fatal("invalid existing config fell back to defaults")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := load("", base); err == nil {
		t.Fatal("unreadable config fell back to defaults")
	}
}

func TestWorkspaceRootRejectsFile(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("BRIDGE_TEST_ROOT", path)
	if _, err := loadSource(t, path, source); err == nil {
		t.Fatal("regular file accepted as workspace root")
	}
}

func TestWorkspaceRootMissingOtherEnvironmentIsStrict(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("BRIDGE_TEST_MISSING", "")
	if err := os.Unsetenv("BRIDGE_TEST_MISSING"); err != nil {
		t.Fatal(err)
	}
	source = setTOMLField(source, "storage", "workspace_root", `"${BRIDGE_TEST_MISSING}"`)
	if _, err := loadSource(t, path, source); err == nil || !strings.Contains(err.Error(), "BRIDGE_TEST_MISSING") {
		t.Fatal("missing unrelated workspace environment variable must fail")
	}
}

func TestStructuredLoggingConfigDefaultsAndOverrides(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("BRIDGE_TEST_LOG_LEVEL", "debug")
	t.Setenv("BRIDGE_TEST_LOG_FORMAT", "json")
	t.Setenv("BRIDGE_TEST_BRIDGE_LEVEL", "warn")
	source = setTOMLField(source, "logging", "level", `"${BRIDGE_TEST_LOG_LEVEL}"`)
	source = setTOMLField(source, "logging", "format", `"${BRIDGE_TEST_LOG_FORMAT}"`)
	source = setTOMLField(source, "logging.component_levels", "bridge", `"${BRIDGE_TEST_BRIDGE_LEVEL}"`)
	source = setTOMLField(source, "logging.component_levels", "store", `"info"`)
	c, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "debug" || c.LogFormat != "json" || c.LogComponentLevels["bridge"] != "warn" || c.LogComponentLevels["store"] != "info" {
		t.Fatalf("logging settings = %#v/%#v/%#v", c.LogLevel, c.LogFormat, c.LogComponentLevels)
	}

	// No logging-specific environment variable is consulted when fields are omitted.
	t.Setenv("OMP_TELEGRAM_LOG_LEVEL", "debug")
	_, defaultsSource := configFixture(t)
	c, err = loadSource(t, path, defaultsSource)
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "info" || c.LogFormat != "text" || len(c.LogComponentLevels) != 0 {
		t.Fatalf("logging defaults = %#v/%#v/%#v", c.LogLevel, c.LogFormat, c.LogComponentLevels)
	}
}

func TestStructuredLoggingConfigExpansionIsSinglePass(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("BRIDGE_TEST_LOG_LEVEL", "${BRIDGE_TEST_NESTED_LEVEL}")
	t.Setenv("BRIDGE_TEST_NESTED_LEVEL", "debug")
	source = setTOMLField(source, "logging", "level", `"${BRIDGE_TEST_LOG_LEVEL}"`)
	_, err := loadSource(t, path, source)
	if err == nil || strings.Contains(err.Error(), "BRIDGE_TEST_NESTED_LEVEL") || strings.Contains(err.Error(), "${BRIDGE_TEST_LOG_LEVEL}") {
		t.Fatalf("single-pass expansion leaked a value or was accepted: %v", err)
	}
	if expanded, err := expand("literal$$dollar"); err != nil || expanded != "literal$dollar" {
		t.Fatalf("literal dollar expansion = %q, error %v", expanded, err)
	}
}

func TestStructuredLoggingConfigInvalidMapDoesNotExposeKeys(t *testing.T) {
	path, source := configFixture(t)
	source = setTOMLField(source, "logging.component_levels", "zeta", `"debug"`)
	source = setTOMLField(source, "logging.component_levels", "alpha", `"info"`)
	_, err := loadSource(t, path, source)
	if err == nil || !strings.Contains(err.Error(), "unknown log component") || strings.Contains(err.Error(), "alpha") || strings.Contains(err.Error(), "zeta") {
		t.Fatalf("invalid map diagnostic exposed a key or lacked semantic validation: %v", err)
	}
}

func TestStructuredLoggingConfigRejectsUnsafeKeysBeforeExpansion(t *testing.T) {
	path, source := configFixture(t)
	const unsafeKey = "/private/omp-telegram/config.toml https://api.telegram.org/bot123456:FAKE_TOKEN\nUNKNOWN_COMPONENT_CANARY"
	const missingEnv = "BRIDGE_TEST_MISSING_COMPONENT_LEVEL"
	t.Setenv(missingEnv, "present")
	if err := os.Unsetenv(missingEnv); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "literal level", value: `"debug"`},
		{name: "missing environment level", value: `"${BRIDGE_TEST_MISSING_COMPONENT_LEVEL}"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := setTOMLField(source, "logging.component_levels", strconv.Quote(unsafeKey), tc.value)
			_, err := loadSource(t, path, config)
			if err == nil || !strings.Contains(err.Error(), "unknown log component") {
				t.Fatalf("unsafe component key was not rejected semantically: %v", err)
			}
			for _, canary := range []string{"/private/omp-telegram/config.toml", "api.telegram.org", "FAKE_TOKEN", "UNKNOWN_COMPONENT_CANARY", unsafeKey, missingEnv} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("configuration error exposed component or value canary %q: %v", canary, err)
				}
			}
		})
	}
}

func TestConfigurationErrorsDoNotExposePathOrExpandedValue(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "private-config-value.toml")
	if _, err := load(missing, t.TempDir()); err == nil || strings.Contains(err.Error(), missing) {
		t.Fatalf("missing configuration error exposed path: %v", err)
	}
	path, source := configFixture(t)
	t.Setenv("BRIDGE_TEST_WORKERS", "private-expanded-value")
	source = setTOMLField(source, "worker", "max_workers", `"${BRIDGE_TEST_WORKERS}"`)
	_, err := loadSource(t, path, source)
	if err == nil || strings.Contains(err.Error(), "private-expanded-value") || strings.Contains(err.Error(), path) {
		t.Fatalf("invalid value error leaked sensitive data: %v", err)
	}
}

func TestGroupedConfigLoadsAllSections(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("BRIDGE_TEST_WORKERS", "6")
	t.Setenv("BRIDGE_TEST_QUEUE", "9")
	t.Setenv("BRIDGE_TEST_RETENTION", "180")
	t.Setenv("BRIDGE_TEST_IDLE_TIMEOUT", "45s")
	t.Setenv("BRIDGE_TEST_LEVEL", "debug")
	t.Setenv("BRIDGE_TEST_FORMAT", "json")
	t.Setenv("BRIDGE_TEST_COMPONENT_LEVEL", "warn")
	source = setTOMLField(source, "telegram", "progress_mode", `"verbose"`)
	source = setTOMLField(source, "omp", "args", `''`)
	source = setTOMLField(source, "storage", "database_retention_days", `"${BRIDGE_TEST_RETENTION}"`)
	source = setTOMLField(source, "worker", "max_workers", `"${BRIDGE_TEST_WORKERS}"`)
	source = setTOMLField(source, "worker", "queue_capacity", `"${BRIDGE_TEST_QUEUE}"`)
	source = setTOMLField(source, "worker", "idle_timeout", `"${BRIDGE_TEST_IDLE_TIMEOUT}"`)
	source = setTOMLField(source, "logging", "level", `"${BRIDGE_TEST_LEVEL}"`)
	source = setTOMLField(source, "logging", "format", `"${BRIDGE_TEST_FORMAT}"`)
	source = setTOMLField(source, "logging.component_levels", "bridge", `"${BRIDGE_TEST_COMPONENT_LEVEL}"`)
	c, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}
	if c.ProgressMode != "verbose" || len(c.OMPArgs) != 0 || c.DatabaseRetentionDays != 180 || c.MaxWorkers != 6 || c.QueueCapacity != 9 || c.IdleTimeout != 45*time.Second {
		t.Fatalf("grouped sections did not populate runtime config: %#v", c)
	}
	if c.LogLevel != "debug" || c.LogFormat != "json" || c.LogComponentLevels["bridge"] != "warn" {
		t.Fatalf("grouped logging sections did not populate runtime config: %#v", c)
	}
}

func TestOmittedOptionalTablesUseDefaults(t *testing.T) {
	base := t.TempDir()
	omp := filepath.Join(base, "omp")
	if err := os.WriteFile(omp, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", base)
	t.Setenv("OMP_TELEGRAM_ARGS", "")
	t.Setenv("OMP_TELEGRAM_WORKSPACE_ROOT", "")
	source := `[telegram]
token = "literal-token"
allowed_users = [7]
allowed_chats = [-10]
`
	path := filepath.Join(base, "config.toml")
	c, err := loadSourceAt(t, path, source, base)
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "literal-token" || c.OMP != omp || len(c.OMPArgs) != 0 || c.DataDir != base || c.WorkspaceRoot != filepath.Join(base, "workspace") {
		t.Fatalf("omitted optional tables did not use defaults: %#v", c)
	}
	if c.MaxWorkers != 4 || c.QueueCapacity != 16 || c.DatabaseRetentionDays != 90 || c.IdleTimeout != 30*time.Minute || c.LogLevel != "info" || c.LogFormat != "text" || len(c.LogComponentLevels) != 0 {
		t.Fatalf("omitted optional table defaults changed: %#v", c)
	}
}

func TestGroupedSchemaRejectsLegacyAndMixedLayouts(t *testing.T) {
	path, source := configFixture(t)
	flat := `token = "flat-secret"
allowed_users = [7]
allowed_chats = [-10]
omp = "omp"
data_dir = "."
`
	cases := map[string]string{
		"legacy flat":                flat,
		"mixed root field":           "max_workers = 8\n" + source,
		"mixed legacy logging field": "log_level = \"debug\"\n" + source,
		"old logging table":          source + "\n[log_component_levels]\nbridge = \"debug\"\n",
		"unknown nested worker key":  setTOMLField(source, "worker", "unexpected_worker_key", `"secret-value"`),
		"unknown nested logging key": setTOMLField(source, "logging", "unexpected_logging_key", `"secret-value"`),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadSource(t, path, input)
			if err == nil {
				t.Fatal("legacy, mixed, or unknown grouped layout was accepted")
			}
			if strings.Contains(err.Error(), "flat-secret") || strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "unexpected_") {
				t.Fatalf("schema error exposed untrusted configuration data: %v", err)
			}
		})
	}
}
