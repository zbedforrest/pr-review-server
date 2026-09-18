//go:build !windows

package service

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func writeChildScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "child.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write child script: %v", err)
	}
	return path
}

type agentRunOutcome struct {
	parsed   *agentParseResult
	parseErr error
	waitErr  error
	stdout   bytes.Buffer
	stderr   bytes.Buffer
}

// runStreamLikeAgent mirrors RunAgentReview's stream handling: drain stderr in
// the background, parse stdout to EOF, then Wait.
func runStreamLikeAgent(proc SpawnedProcess, maxTurns int) *agentRunOutcome {
	out := &agentRunOutcome{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&out.stderr, proc.Stderr())
	}()
	out.parsed, out.parseErr = parseAgentStream(proc, &out.stdout, maxTurns)
	out.waitErr = proc.Wait()
	wg.Wait()
	return out
}

func waitForGoroutineBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines leaked: %d now, %d before spawn", runtime.NumGoroutine(), baseline)
}

func TestDefaultSpawnerDrainsChildOutputUntilExitAfterResult(t *testing.T) {
	script := writeChildScript(t, `
echo '{"type":"assistant","message":{"model":"m"}}'
echo '{"type":"result","subtype":"success","result":"done"}'
i=0
while [ $i -lt 300 ]; do
  echo "{\"type\":\"trailing\",\"n\":$i}"
  echo "stderr $i" >&2
  i=$((i+1))
done
exit 0
`)
	baseline := runtime.NumGoroutine()
	proc, err := (DefaultSpawner{}).SpawnWithEnv(context.Background(), script, nil, "", []string{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	out := runStreamLikeAgent(proc, 10)

	if out.parseErr != nil {
		t.Fatalf("parse: %v", out.parseErr)
	}
	if out.waitErr != nil {
		t.Fatalf("child did not exit cleanly (a closed pipe would surface as broken pipe): %v", out.waitErr)
	}
	if out.parsed.finalOutput != "done" {
		t.Errorf("finalOutput = %q, want done", out.parsed.finalOutput)
	}
	if n := strings.Count(out.stdout.String(), "\n"); n != 302 {
		t.Errorf("stdout lines captured after the result event = %d, want 302", n)
	}
	if n := strings.Count(out.stderr.String(), "\n"); n != 300 {
		t.Errorf("stderr lines captured = %d, want 300", n)
	}
	waitForGoroutineBaseline(t, baseline)
}

func TestDefaultSpawnerMaxTurnsKillTearsDownProcessGroup(t *testing.T) {
	script := writeChildScript(t, `
sleep 60 &
echo "$!" >&2
while :; do echo '{"type":"assistant"}'; done
`)
	baseline := runtime.NumGoroutine()
	proc, err := (DefaultSpawner{}).SpawnWithEnv(context.Background(), script, nil, "", []string{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	started := time.Now()
	out := runStreamLikeAgent(proc, 3)

	if out.parseErr == nil || !strings.Contains(out.parseErr.Error(), "max-turns") {
		t.Fatalf("parse error = %v, want max-turns", out.parseErr)
	}
	if out.waitErr == nil {
		t.Fatal("Wait returned nil for a killed child")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Wait took %s after the kill", elapsed)
	}

	grandchild, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(out.stderr.String(), "\n", 2)[0]))
	if err != nil {
		t.Fatalf("grandchild pid from stderr %q: %v", out.stderr.String(), err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(grandchild, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(grandchild, 0); err == nil {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild %d survived the process-group kill", grandchild)
	}
	waitForGoroutineBaseline(t, baseline)
}
