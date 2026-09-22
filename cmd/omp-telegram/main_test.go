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

func TestCheckRejectsLegacyAndMixedConfiguration(t *testing.T) {
	for _, layout := range []string{"flat", "mixed"} {
		t.Run(layout, func(t *testing.T) {
			path := cliConfig(t, "")
			var source string
			if layout == "flat" {
				source = "token = 'SECRET_LEGACY_TOKEN'\nallowed_users = [7]\nallowed_chats = [7]\nomp = '/bin/true'\nomp_args = ''\n"
			} else {
				grouped, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				source = "max_workers = 99\n" + string(grouped)
			}
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			out, errout, err := cliCommand(t, false, "--check", "--config", path)
			if err == nil || out != "" || strings.Contains(errout, "SECRET") {
				t.Fatalf("legacy layout was accepted or exposed values: stdout=%q stderr=%q err=%v", out, errout, err)
			}
		})
	}
}

func TestDaemonStructuredLogFormats(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			out, errout, err := cliCommand(t, true, "--config", cliConfig(t, fmt.Sprintf("[logging]\nformat = %q\n", format)))
			if err != nil || out != "" {
				t.Fatalf("daemon stdout=%q stderr=%q err=%v", out, errout, err)
			}
			if strings.Contains(errout, "SECRET") || strings.Contains(errout, "api.telegram.org") {
				t.Fatal("daemon logs exposed credentials")
			}
			events := map[string]bool{}
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
				} else {
					fields := strings.Fields(line)
					if len(fields) < 5 || !strings.HasPrefix(fields[3], "[") || !strings.HasSuffix(fields[3], "]") || !strings.Contains(line, " event=") {
						t.Fatalf("missing compact header or event: %q", line)
					}
					for _, label := range []string{"time=", "level=", "msg=", "component="} {
						if strings.Contains(line, label) {
							t.Fatalf("unexpected text header label %q: %q", label, line)
						}
					}
					for event, component := range map[string]string{"daemon_start": "daemon", "daemon_stop": "daemon", "command_menu_registered": "telegram"} {
						if strings.Contains(line, "event="+event) {
							if fields[2] != "INFO" || fields[3] != "["+component+"]" {
								t.Fatalf("incorrect lifecycle header for %s: %q", event, line)
							}
							events[event] = true
						}
					}
				}
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
