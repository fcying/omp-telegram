package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	t.Setenv("OMP_TELEGRAM_ARGS", "")
	source := `token = "${BRIDGE_TEST_TOKEN}"
allowed_users = [7]
allowed_chats = [-10]
omp = "$BRIDGE_TEST_OMP"
data_dir = "${BRIDGE_TEST_ROOT}/data"
max_workers = 4
queue_capacity = 16
workspace_root = "$BRIDGE_TEST_ROOT"
`
	return filepath.Join(root, "config.toml"), source
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
	source = strings.Replace(source, `omp = "$BRIDGE_TEST_OMP"`, `omp = "./bin/omp"`, 1)
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
		if _, err := loadSource(t, path, source+"\nomp_args = "+args+"\n"); err == nil {
			t.Fatalf("invalid omp_args accepted: %s", args)
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
	c, err = loadSource(t, path, source+"\nomp_args = ''\n")
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
	source = strings.TrimPrefix(source, "token = \"${BRIDGE_TEST_TOKEN}\"\n")
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
	source = strings.Replace(source, `workspace_root = "$BRIDGE_TEST_ROOT"`, `workspace_root = "${OMP_TELEGRAM_WORKSPACE_ROOT}"`, 1)
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
	source = strings.NewReplacer("allowed_users = [7]", `allowed_users = [7, "${BRIDGE_TEST_USER}"]`, "allowed_chats = [-10]", `allowed_chats = ["$BRIDGE_TEST_CHAT"]`, "max_workers = 4", `max_workers = "${BRIDGE_TEST_WORKERS}"`, "queue_capacity = 16", `queue_capacity = "$BRIDGE_TEST_QUEUE"`).Replace(source)
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
			c, err := loadSource(t, path, source+"\ndatabase_retention_days = "+tc.value)
			if err != nil || c.DatabaseRetentionDays != tc.want {
				t.Fatalf("database retention = %d, error %v", c.DatabaseRetentionDays, err)
			}
		})
	}
	for _, value := range []string{"-1", "1.5", "true"} {
		if _, err := loadSource(t, path, source+"\ndatabase_retention_days = "+value); err == nil {
			t.Fatalf("invalid retention accepted: %s", value)
		}
	}
}

func TestCommaSeparatedAllowlists(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("OMP_TELEGRAM_ALLOWED_USERS", " 8, 9 ")
	t.Setenv("OMP_TELEGRAM_ALLOWED_CHATS", "7, -1001234567890")
	source = strings.NewReplacer("allowed_users = [7]", `allowed_users = [7, "${OMP_TELEGRAM_ALLOWED_USERS}"]`, "allowed_chats = [-10]", `allowed_chats = [-10, "${OMP_TELEGRAM_ALLOWED_CHATS}"]`).Replace(source)
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
		`unknown = "secret-value"` + "\n" + source,
		"token_env = \"BRIDGE_TEST_TOKEN\"\n" + source,
		source + "\n[projects]\ndemo = \"secret-value\"\n",
		`{"token":"secret-value"}`,
		strings.Replace(source, "allowed_users = [7]", "allowed_users = [0]", 1),
		strings.Replace(source, "max_workers = 4", "max_workers = true", 1),
		strings.Replace(source, "max_workers = 4", "max_workers = 1.5", 1),
		strings.Replace(source, `token = "${BRIDGE_TEST_TOKEN}"`, `token = ""`, 1),
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
		field string
		env   string
		unset bool
		want  string
	}{
		{name: "omitted absent", unset: true, want: "workspace"},
		{name: "omitted empty", want: "workspace"},
		{name: "omitted configured", env: "environment", want: "environment"},
		{name: "reference absent", field: `workspace_root = "${OMP_TELEGRAM_WORKSPACE_ROOT}"`, unset: true, want: "workspace"},
		{name: "reference empty", field: `workspace_root = "$OMP_TELEGRAM_WORKSPACE_ROOT"`, want: "workspace"},
		{name: "explicit empty", field: `workspace_root = ""`, env: "environment", want: "workspace"},
		{name: "explicit directory", field: `workspace_root = "configured/nested"`, env: "environment", want: "configured/nested"},
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
			source = strings.Replace(source, `workspace_root = "$BRIDGE_TEST_ROOT"`, tc.field, 1)
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
				dataField, workspaceField := "", ""
				wantData, wantWorkspace := baseDir, filepath.Join(baseDir, "workspace")
				switch directories {
				case "relative":
					dataField = `data_dir = "state/nested"`
					workspaceField = `workspace_root = "jobs/nested"`
					wantData = filepath.Join(baseDir, "state", "nested")
					wantWorkspace = filepath.Join(baseDir, "jobs", "nested")
				case "absolute":
					dataField = `data_dir = "$BRIDGE_TEST_ROOT/data"`
					workspaceField = `workspace_root = "$BRIDGE_TEST_ROOT/jobs"`
					wantData = filepath.Join(filepath.Dir(path), "data")
					wantWorkspace = filepath.Join(filepath.Dir(path), "jobs")
				}
				source = strings.Replace(source, `data_dir = "${BRIDGE_TEST_ROOT}/data"`, dataField, 1)
				source = strings.Replace(source, `workspace_root = "$BRIDGE_TEST_ROOT"`, workspaceField, 1)
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
	source = strings.Replace(source, `workspace_root = "$BRIDGE_TEST_ROOT"`, `workspace_root = "${BRIDGE_TEST_MISSING}"`, 1)
	if _, err := loadSource(t, path, source); err == nil || !strings.Contains(err.Error(), "BRIDGE_TEST_MISSING") {
		t.Fatal("missing unrelated workspace environment variable must fail")
	}
}
