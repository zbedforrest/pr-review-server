//go:build !windows

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// stderrWaitDelay bounds how long Wait waits for stderr to close after the
// child itself has exited: a descendant the agent left running (a server
// started with `>/dev/null &`) still inherits the stderr write end and would
// otherwise stall a successful run until the wall clock.
const stderrWaitDelay = 2 * time.Second

// stderrCaptureLimit caps captured stderr; failure messages quote at most
// the first kilobyte anyway.
const stderrCaptureLimit = 256 * 1024

// DefaultSpawner runs commands as the leader of a fresh process group so any
// subprocesses they fork (e.g. bash via an agent shell tool) can be torn
// down as a group rather than orphaned when the parent exits.
type DefaultSpawner struct{}

var _ Spawner = DefaultSpawner{}

func (DefaultSpawner) SpawnWithEnv(ctx context.Context, name string, args []string, dir string, environment []string) (SpawnedProcess, error) {
	return spawnCommand(ctx, name, args, dir, environment)
}

func spawnCommand(ctx context.Context, name string, args []string, dir string, environment []string) (SpawnedProcess, error) {
	// Use Command (not CommandContext) so we control kill behavior ourselves.
	// CommandContext's auto-kill only signals the direct child, leaving any
	// process-group children orphaned. We watch ctx.Done in a goroutine and
	// group-kill instead.
	cmd := exec.Command(name, args...)
	// make preserves the distinction between an explicit empty environment
	// and nil, which os/exec interprets as inheriting the parent environment.
	cmd.Env = append(make([]string, 0, len(environment)), environment...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// exec copies stderr into the buffer with its own goroutine and Wait waits
	// for that copy, so nothing here can race Wait closing the pipe; WaitDelay
	// force-closes it once only descendants still hold the write end.
	stderr := &boundedBuffer{limit: stderrCaptureLimit}
	cmd.Stderr = stderr
	cmd.WaitDelay = stderrWaitDelay
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	p := &execProcess{cmd: cmd, stdout: stdout, stderr: stderr, done: make(chan struct{})}

	// When ctx expires, group-kill so child shells go down with the agent.
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
	stderr *boundedBuffer
	done   chan struct{}
	once   sync.Once
}

func (e *execProcess) Stdout() io.Reader { return e.stdout }

// Stderr returns what the child wrote so far; complete once Wait has returned.
func (e *execProcess) Stderr() io.Reader { return strings.NewReader(e.stderr.String()) }

func (e *execProcess) Wait() error {
	err := e.cmd.Wait()
	e.once.Do(func() { close(e.done) })
	if errors.Is(err, exec.ErrWaitDelay) {
		// The child exited successfully but a descendant kept stderr open past
		// WaitDelay; take the stragglers down with the group.
		log.Printf("[AGENT] %s exited but a descendant kept stderr open for %s; killing its process group",
			e.cmd.Path, stderrWaitDelay)
		_ = e.killGroup()
		return nil
	}
	return err
}

// Kill sends SIGKILL to the entire process group so any subprocesses
// (e.g. bash spawned by an agent shell tool) go down with the parent.
// Also closes stdout explicitly so a reader still blocked on the pipe
// unblocks immediately — defense against a future caller that returns from
// the parser without first draining stdout, which would otherwise deadlock
// proc.Wait() waiting for the pipe to close.
func (e *execProcess) Kill() error {
	if e.cmd.Process == nil {
		return nil
	}
	killErr := e.killGroup()
	if e.stdout != nil {
		_ = e.stdout.Close()
	}
	return killErr
}

func (e *execProcess) killGroup() error {
	// Negative PID targets the process group. Setpgid was set in Spawn, so
	// the group ID equals the leader's PID.
	err := syscall.Kill(-e.cmd.Process.Pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

// boundedBuffer keeps the first limit bytes written and counts the rest.
type boundedBuffer struct {
	mu      sync.Mutex
	limit   int
	buf     strings.Builder
	dropped int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	room := b.limit - b.buf.Len()
	if room < n {
		b.dropped += n - max(room, 0)
		p = p[:max(room, 0)]
	}
	b.buf.Write(p)
	return n, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dropped == 0 {
		return b.buf.String()
	}
	return b.buf.String() + fmt.Sprintf("…(%d more bytes dropped)", b.dropped)
}
