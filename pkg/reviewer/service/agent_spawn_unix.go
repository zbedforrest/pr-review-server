//go:build !windows

package service

import (
	"context"
	"io"
	"os/exec"
	"sync"
	"syscall"
)

// DefaultSpawner runs commands as the leader of a fresh process group so any
// subprocesses they fork (e.g. bash via `claude --tools Bash`) can be torn
// down as a group rather than orphaned when the parent exits.
type DefaultSpawner struct{}

func (DefaultSpawner) Spawn(ctx context.Context, name string, args []string, dir string) (SpawnedProcess, error) {
	// Use Command (not CommandContext) so we control kill behavior ourselves.
	// CommandContext's auto-kill only signals the direct child, leaving any
	// process-group children orphaned. We watch ctx.Done in a goroutine and
	// group-kill instead.
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	p := &execProcess{cmd: cmd, stdout: stdout, stderr: stderr, done: make(chan struct{})}

	// When ctx expires, group-kill so child shells go down with claude.
	// Idempotent with explicit Kill().
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Kill()
		case <-p.done:
		}
	}()
	return p, nil
}

type execProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
	done   chan struct{}
	once   sync.Once
}

func (e *execProcess) Stdout() io.Reader { return e.stdout }
func (e *execProcess) Stderr() io.Reader { return e.stderr }
func (e *execProcess) Wait() error {
	err := e.cmd.Wait()
	e.once.Do(func() { close(e.done) })
	return err
}

// Kill sends SIGKILL to the entire process group so any subprocesses
// (e.g. bash spawned by claude --tools Bash) go down with the parent.
// Also closes stdout/stderr explicitly so any reader still blocked on
// the pipes unblocks immediately — defense against a future caller that
// returns from the parser without first draining stdout, which would
// otherwise deadlock proc.Wait() waiting for the pipe to close.
func (e *execProcess) Kill() error {
	if e.cmd.Process == nil {
		return nil
	}
	// Negative PID targets the process group. Setpgid was set in Spawn, so
	// the group ID equals the leader's PID.
	killErr := syscall.Kill(-e.cmd.Process.Pid, syscall.SIGKILL)
	if killErr == syscall.ESRCH {
		killErr = nil
	}
	if e.stdout != nil {
		_ = e.stdout.Close()
	}
	if e.stderr != nil {
		_ = e.stderr.Close()
	}
	return killErr
}
