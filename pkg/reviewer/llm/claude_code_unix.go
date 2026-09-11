//go:build !windows

package llm

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup runs the CLI as the leader of a fresh process group
// and makes context cancellation SIGKILL the whole group, so helpers the CLI
// forks cannot keep the stdout pipe (and therefore cmd.Wait) alive.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
}
