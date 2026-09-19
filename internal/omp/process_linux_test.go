package omp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// All fixture roles run in separate executables. Only the isolated controller
// becomes a subreaper; the test runner's other children are never adopted/killed.
func init() {
	role := os.Getenv("OMP_DEATH_FIXTURE")
	if role == "daemon" && len(os.Args) > 1 && (os.Args[1] == "--mode" || os.Args[1] == "acp") {
		role = "direct"
	}
	mark := func(name string, pid int) {
		if err := os.WriteFile(filepath.Join(os.Getenv("OMP_DEATH_DIR"), name), []byte(strconv.Itoa(pid)), 0600); err != nil {
			os.Exit(91)
		}
	}
	switch role {
	case "daemon":
		cfg := Config{Binary: os.Args[0], CWD: os.Getenv("OMP_DEATH_DIR")}
		if os.Getenv("OMP_DEATH_TRANSPORT") == "acp" {
			_, _ = ListSessions(context.Background(), cfg)
		} else {
			_, _ = Start(context.Background(), cfg, testRPCLogger())
		}
		os.Exit(92)
	case "direct":
		mark("direct", os.Getpid())
		// A shell with a waiting tool child cannot optimize itself away via exec.
		cmd := exec.Command("/bin/sh", "-c", `"$OMP_DEATH_BINARY" & wait`)
		cmd.Env = append(os.Environ(), "OMP_DEATH_FIXTURE=tool")
		if cmd.Start() != nil {
			os.Exit(93)
		}
		mark("shell", cmd.Process.Pid)
		for {
			time.Sleep(time.Hour)
		}
	case "tool":
		mark("tool", os.Getpid())
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "OMP_DEATH_FIXTURE=grandchild")
		// Deliberately escape the original group to expose the containment limit.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if cmd.Start() != nil {
			os.Exit(94)
		}
		_ = cmd.Wait()
		os.Exit(0)
	case "grandchild":
		mark("grandchild", os.Getpid())
		for {
			time.Sleep(time.Hour)
		}
	}
}

func TestParentDeathSignalsDirectChild(t *testing.T) {
	if os.Getenv("OMP_DEATH_FIXTURE") != "controller" {
		for _, transport := range []string{"rpc", "acp"} {
			t.Run(transport, func(t *testing.T) {
				cmd := exec.Command(os.Args[0], "-test.run=^TestParentDeathSignalsDirectChild$", "-test.v")
				cmd.Env = append(os.Environ(), "OMP_DEATH_FIXTURE=controller", "OMP_DEATH_TRANSPORT="+transport, "OMP_DEATH_DIR="+t.TempDir(), "OMP_DEATH_BINARY="+os.Args[0])
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("parent-death fixture: %v\n%s", err, out)
				} else {
					t.Log(string(out))
				}
			})
		}
		return
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Skipf("pidfd cleanup is unavailable: %v", err)
	}
	if err = unix.Close(fd); err != nil {
		t.Fatal(err)
	}
	daemon := exec.Command(os.Args[0])
	daemon.Env = append(os.Environ(), "OMP_DEATH_FIXTURE=daemon")
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = daemon.Process.Kill()
			_ = daemon.Wait()
		}
		reapDeathFixture(t)
	}()
	pids := make(map[string]int)
	for _, name := range []string{"direct", "shell", "tool", "grandchild"} {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(filepath.Join(os.Getenv("OMP_DEATH_DIR"), name))
			if err == nil {
				pid, err := strconv.Atoi(string(data))
				if err == nil && pid > 0 {
					pids[name] = pid
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		if pids[name] == 0 {
			t.Fatalf("%s did not start", name)
		}
	}
	// Let launch goroutines return and the runtime recycle ordinary threads.
	// A launcher that locks only around Start can spuriously signal its child.
	time.Sleep(100 * time.Millisecond)
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pids["direct"]))
	if err != nil {
		t.Fatal(err)
	}
	state := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
	if len(state) == 0 || state[0] == "Z" || state[0] == "X" {
		t.Fatal("direct child died while daemon was still alive")
	}
	if err := daemon.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = daemon.Wait()
	waited = true
	var status unix.WaitStatus
	deadline := time.Now().Add(5 * time.Second)
	reaped := false
	for time.Now().Before(deadline) {
		pid, err := unix.Wait4(pids["direct"], &status, unix.WNOHANG, nil)
		if err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			reaped = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !reaped || !status.Signaled() || status.Signal() != unix.SIGTERM {
		t.Fatalf("direct child: reaped=%v status=%v; expected parent-death SIGTERM", reaped, status)
	}
	t.Log("direct child independently reaped with SIGTERM")
	for _, name := range []string{"shell", "tool", "grandchild"} {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pids[name]))
		if os.IsNotExist(err) {
			t.Logf("%s independently observed exited after daemon death", name)
			continue
		}
		if err != nil {
			t.Fatalf("cannot inspect %s after daemon death: %v", name, err)
		}
		fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
		if len(fields) == 0 {
			t.Fatalf("cannot parse %s process state", name)
		}
		if fields[0] == "Z" || fields[0] == "X" {
			t.Logf("%s independently observed exited after daemon death", name)
			continue
		}
		t.Logf("%s independently alive after daemon death (not covered by Pdeathsig)", name)
	}
}

// Kill only children of this isolated subreaper, using pidfds to prevent PID
// reuse from targeting unrelated processes. Repeated adoption handles setsid
// descendants as their fixture parents die. Wait4 reaps every adopted child.
func reapDeathFixture(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		paths, err := filepath.Glob("/proc/self/task/*/children")
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			data, _ := os.ReadFile(path)
			for _, text := range strings.Fields(string(data)) {
				pid, _ := strconv.Atoi(text)
				fd, err := unix.PidfdOpen(pid, 0)
				if err != nil {
					t.Errorf("open fixture child pidfd: %v", err)
					continue
				}
				_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
				_ = unix.Close(fd)
			}
		}
		for {
			pid, err := unix.Wait4(-1, nil, unix.WNOHANG, nil)
			if err == unix.ECHILD {
				return
			}
			if err != nil || pid == 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("fixture descendants not fully reaped before cleanup deadline")
}

func TestProcessStartFailure(t *testing.T) {
	cmd := exec.Command(filepath.Join(t.TempDir(), "missing-omp"))
	if waited, err := startProcess(cmd); err == nil || waited != nil {
		t.Fatalf("failed start returned wait channel=%v, error=%v", waited, err)
	}
}
