//go:build !windows

package llm

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func detachFromProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func TestClaudeCodeClientTimeoutKillsGrandchildrenHoldingStdout(t *testing.T) {
	client := newClaudeCodeHelperClient(t, "grandchild_holds_stdout", "")
	client.timeout = time.Second
	started := time.Now()
	_, _, _, _, err := client.GetReview("review me")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "timed out")
	assert.Less(t, time.Since(started), 4*time.Second, "Wait must not block on a grandchild holding the stdout pipe")
}

func TestClaudeCodeClientTimeoutStopsWaitingForDetachedGrandchild(t *testing.T) {
	client := newClaudeCodeHelperClient(t, "detached_grandchild_holds_stdout", "")
	client.timeout = time.Second
	client.waitDelay = 500 * time.Millisecond
	started := time.Now()
	_, _, _, _, err := client.GetReview("review me")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "timed out")
	assert.Less(t, time.Since(started), 4*time.Second, "WaitDelay must bound the wait on a grandchild outside the process group")
}
