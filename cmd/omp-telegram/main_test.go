package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The subprocess exercises the real CLI without contacting Telegram or OMP.
func TestCLIProcess(t *testing.T) {
	if os.Getenv("OMP_TELEGRAM_CLI_TEST") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("omp-telegram", flag.ExitOnError)
	if os.Getenv("OMP_TELEGRAM_CLI_DAEMON") == "1" {
		http.DefaultTransport = &cliTransport{}
	}
	os.Exit(run())
}

type cliTransport struct{ stop sync.Once }

func (f *cliTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var result any = true
	switch filepath.Base(r.URL.Path) {
	case "getMe":
		result = map[string]any{"id": 99, "username": "fixture_bot", "is_bot": true}
	case "getUpdates":
		result = []any{}
		f.stop.Do(func() {
			go func() {
				time.Sleep(50 * time.Millisecond)
				p, err := os.FindProcess(os.Getpid())
				if err == nil {
					_ = p.Signal(os.Interrupt)
				}
			}()
		})
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	raw, err := json.Marshal(map[string]any{"ok": true, "result": result})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header), Request: r}, nil
}

func cliCommand(t *testing.T, daemon bool, args ...string) (string, string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestCLIProcess$", "--"}, args...)...)
	mode := "0"
	if daemon {
		mode = "1"
	}
	cmd.Env = append(os.Environ(), "OMP_TELEGRAM_CLI_TEST=1", "OMP_TELEGRAM_CLI_DAEMON="+mode)
	var out, errout bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errout
	err = cmd.Run()
	return out.String(), errout.String(), err
}

func cliConfig(t *testing.T, extra string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	text := fmt.Sprintf("[telegram]\ntoken = 'SECRET_TOKEN'\nallowed_users = [7]\nallowed_chats = [7]\n[omp]\nargs = ''\nbinary = %q\n[storage]\ndata_dir = %q\nworkspace_root = %q\n", exe, filepath.Join(root, "data"), filepath.Join(root, "workspace")) + extra
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoggingBootstrapBehavior(t *testing.T) {
	out, errout, err := cliCommand(t, false, "--version")
	if err != nil || !strings.HasPrefix(out, "omp-telegram ") || strings.Count(out, "\n") != 1 || errout != "" {
		t.Fatalf("version stdout=%q stderr=%q err=%v", out, errout, err)
	}
	out, errout, err = cliCommand(t, false, "--check", "--config", cliConfig(t, "[logging]\nformat = 'json'\n"))
	if err != nil || out != "configuration valid\n" || errout != "" {
		t.Fatalf("check stdout=%q stderr=%q err=%v", out, errout, err)
	}
	out, errout, err = cliCommand(t, false, "--check", "--config", cliConfig(t, "[logging]\nlevel = 'SECRET_BAD_LEVEL'\n"))
	if err == nil || out != "" || strings.Contains(errout, "SECRET") || !strings.Contains(errout, "logging.level") {
		t.Fatalf("invalid logging stdout=%q stderr=%q err=%v", out, errout, err)
	}
}

func TestUnknownSettingsLogErrorsWithoutBlockingStartup(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		for _, daemon := range []bool{false, true} {
			for _, unknown := range []struct {
				name  string
				input string
			}{
				{"legacy-enabled", "[telegram]\nprovider_usage = true\n"},
				{"legacy-disabled", "[telegram]\nprovider_usage = false\n"},
				{"legacy-unexpanded", "[telegram]\nprovider_usage = '${UNKNOWN_CONFIG_SECRET_MISSING}'\n"},
				{"root", "future_setting = 'SECRET_UNKNOWN_VALUE'\n"},
				{"nested", "[omp.environment]\nfuture_setting = false\n"},
				{"table", "[future]\nsecret = 'SECRET_UNKNOWN_VALUE'\n"},
				{"component", "[logging.component_levels]\nfuture = ['SECRET_UNKNOWN_VALUE']\n"},
				{"unsafe-name", "[logging.component_levels]\n\"SECRET_KEY\\nhttps://api.telegram.org/botSECRET_TOKEN\" = '${UNKNOWN_CONFIG_SECRET_MISSING}'\n"},
			} {
				t.Run(fmt.Sprintf("%s/daemon=%t/%s", format, daemon, unknown.name), func(t *testing.T) {
					path := cliConfig(t, "[logging]\nformat = '"+format+"'\nlevel = 'error'\n")
					source, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					updated := unknown.input + string(source)
					switch unknown.name {
					case "legacy-enabled", "legacy-disabled", "legacy-unexpanded":
						updated = strings.Replace(string(source), "[telegram]\n", unknown.input, 1)
					case "nested", "table", "component", "unsafe-name":
						updated = string(source) + unknown.input
					}
					if err := os.WriteFile(path, []byte(updated), 0600); err != nil {
						t.Fatal(err)
					}
					args := []string{"--config", path}
					if !daemon {
						args = append(args, "--check")
					}
					out, errout, err := cliCommand(t, daemon, args...)
					if err != nil {
						t.Fatalf("unknown setting prevented startup/check: %v, stderr=%q", err, errout)
					}
					if daemon && out != "" || !daemon && out != "configuration valid\n" {
						t.Fatalf("unexpected CLI output: %q", out)
					}
					if strings.Contains(errout, "SECRET") || strings.Contains(errout, path) || strings.Contains(errout, "api.telegram.org") {
						t.Fatalf("unknown configuration diagnostic exposed input: %q", errout)
					}
					for _, line := range strings.Split(strings.TrimSpace(errout), "\n") {
						if format == "json" {
							var record map[string]any
							if err := json.Unmarshal([]byte(line), &record); err != nil {
								t.Fatal(err)
							}
							row, _ := record["line"].(float64)
							column, _ := record["column"].(float64)
							if record["level"] != "ERROR" || record["component"] != "daemon" || record["event"] != "unknown_config" || row < 1 || column < 1 {
								t.Fatalf("missing error-level source location: %+v", record)
							}
						} else if !strings.Contains(line, " ERROR [daemon] ") || !strings.Contains(line, "event=unknown_config") || !strings.Contains(line, " line=") || !strings.Contains(line, " column=") {
							t.Fatalf("missing error-level source location: %q", line)
						}
					}
				})
			}
		}
	}
}

func TestKnownConfigErrorsRemainFatalWithUnknownSettings(t *testing.T) {
	for _, invalid := range []string{
		"[worker]\nmax_workers = 'SECRET_INVALID_WORKERS'\n",
		"[logging]\nlevel = 'SECRET_INVALID_LEVEL'\n",
		"[logging.component_levels]\ndaemon = false\n",
		"[telegram.status_bar]\nfinal = ['SECRET_INVALID_FIELD']\n",
		"[telegram.status_bar]\nfinal = true\n",
	} {
		path := cliConfig(t, invalid)
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range []string{
			"future = 'SECRET_UNKNOWN_VALUE'\n" + string(source),
			string(source) + "[future]\nsecret = 'SECRET_UNKNOWN_VALUE'\n",
		} {
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			out, errout, err := cliCommand(t, false, "--check", "--config", path)
			if err == nil || out != "" || strings.TrimSpace(errout) == "" || strings.Contains(errout, "SECRET") {
				t.Fatalf("known invalid setting was ignored or exposed input: stdout=%q stderr=%q err=%v", out, errout, err)
			}
		}
	}
}

func TestDaemonStructuredLogFormats(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			path := cliConfig(t, fmt.Sprintf("[worker]\nmax_workers = 3\nqueue_capacity = 5\nidle_timeout = '45s'\n[logging]\nformat = %q\n", format))
			out, errout, err := cliCommand(t, true, "--config", path)
			if err != nil || out != "" {
				t.Fatalf("daemon stdout=%q stderr=%q err=%v", out, errout, err)
			}
			if strings.Contains(errout, "SECRET") || strings.Contains(errout, "api.telegram.org") || strings.Contains(errout, path) {
				t.Fatal("daemon logs exposed credentials")
			}
			events := map[string]bool{}
			startupFields := false
			for _, line := range strings.Split(strings.TrimSpace(errout), "\n") {
				if format == "json" {
					var record map[string]any
					if err := json.Unmarshal([]byte(line), &record); err != nil {
						t.Fatalf("invalid JSON log: %v", err)
					}
					component, _ := record["component"].(string)
					event, _ := record["event"].(string)
					if component == "" || event == "" {
						t.Fatalf("missing structured identity: %+v", record)
					}
					events[event] = true
					if event == "daemon_start" {
						if record["progress_mode"] != "summary" || record["max_workers"] != float64(3) || record["queue_capacity"] != float64(5) || record["idle_timeout"] != "45s" || record["database_retention_days"] != float64(90) || record["log_level"] != "info" || record["log_format"] != format {
							t.Fatalf("incorrect startup settings: %+v", record)
						}
						startupFields = true
					}
				} else {
					fields := strings.Fields(line)
					if len(fields) < 5 || !strings.HasPrefix(fields[3], "[") || !strings.HasSuffix(fields[3], "]") || !strings.Contains(line, " event=") {
						t.Fatalf("missing compact header or event: %q", line)
					}
					for event, component := range map[string]string{"daemon_start": "daemon", "daemon_stop": "daemon", "command_menu_registered": "telegram"} {
						if strings.Contains(line, "event="+event) {
							if fields[2] != "INFO" || fields[3] != "["+component+"]" {
								t.Fatalf("incorrect lifecycle header for %s: %q", event, line)
							}
							events[event] = true
						}
					}
					if strings.Contains(line, "event=daemon_start") {
						for _, field := range []string{"progress_mode=summary", "max_workers=3", "queue_capacity=5", "idle_timeout=45s", "database_retention_days=90", "log_level=info", "log_format=text"} {
							if !strings.Contains(line, field) {
								t.Fatalf("startup log missing %q: %q", field, line)
							}
						}
						startupFields = true
					}
				}
			}
			if !startupFields {
				t.Fatal("startup settings were not logged")
			}
			for _, event := range []string{"daemon_start", "daemon_stop", "command_menu_registered"} {
				if !events[event] {
					t.Errorf("missing lifecycle event %s", event)
				}
			}
		})
	}
}
func TestDaemonLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.lock")
	link := filepath.Join(dir, "daemon.lock")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	lock, err := openDaemonLock(link)
	if err == nil {
		lock.Close()
		t.Fatal("daemon lock followed a symlink")
	}
}
