//go:build !linux

package omp

import (
	"os"
	"os/exec"
	"syscall"
)

func startProcess(cmd *exec.Cmd) (<-chan error, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	return waited, nil
}
func killProcessGroup(process *os.Process) {
	if process != nil {
		_ = syscall.Kill(-process.Pid, syscall.SIGKILL)
	}
}
