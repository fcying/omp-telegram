package omp

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

// startProcess preserves a dedicated process group for normal shutdown. Linux
// delivers Pdeathsig when the creating OS thread dies, not just when the daemon
// exits, so that thread must remain locked until its sole Wait has completed.
// SIGTERM gives native omp a chance to clean up its tools after daemon SIGKILL.
// This is not tree containment: ignored SIGTERM, descendants that outlive omp,
// escaped process groups, and programs that clear Pdeathsig require an external
// containment boundary. Parent death supplies no later SIGKILL escalation.
func startProcess(cmd *exec.Cmd) (<-chan error, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	started := make(chan error, 1)
	waited := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := cmd.Start()
		started <- err
		if err == nil {
			waited <- cmd.Wait()
		}
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return waited, nil
}
func killProcessGroup(process *os.Process) {
	if process != nil {
		_ = syscall.Kill(-process.Pid, syscall.SIGKILL)
	}
}
