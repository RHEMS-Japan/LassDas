//go:build darwin || linux

package worker

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 5 * time.Second
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

func terminateProcessGroup(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return
	}
}
