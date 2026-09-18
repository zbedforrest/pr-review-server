//go:build !windows

package service

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// runStreamLikeAgent mirrors RunAgentReview's stream handling: drain stderr
// in the background, parse stdout to EOF, Wait, then collect stderr.
func runStreamLikeAgent(proc SpawnedProcess, maxTurns int) *agentRunOutcome {
	out := &agentRunOutcome{}
	stderrOutput := collectStderr(proc)
	out.parsed, out.parseErr = parseAgentStream(proc, &out.stdout, maxTurns)
	out.waitErr = proc.Wait()
	out.stderr.WriteString(stderrOutput())
	return out
}

func waitForProcessGroupGone(t *testing.T, proc SpawnedProcess) {
	t.Helper()
	pgid := proc.(*execProcess).cmd.Process.Pid
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pgid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process group %d still has live members a second after Wait returned", pgid)
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

func TestDefaultSpawnerWaitReturnsWhenGrandchildHoldsStderr(t *testing.T) {
	script := writeChildScript(t, `
echo '{"type":"result","subtype":"success","result":"done"}'
sleep 30 >/dev/null &
i=0
while [ $i -lt 50 ]; do echo "stderr $i" >&2; i=$((i+1)); done
exit 0
`)
	baseline := runtime.NumGoroutine()
	proc, err := (DefaultSpawner{}).SpawnWithEnv(context.Background(), script, nil, "", []string{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	started := time.Now()
	out := runStreamLikeAgent(proc, 10)

	if out.parseErr != nil || out.waitErr != nil {
		t.Fatalf("parseErr=%v waitErr=%v, want a clean exit despite the lingering grandchild", out.parseErr, out.waitErr)
	}
	if elapsed := time.Since(started); elapsed > stderrWaitDelay+2*time.Second {
		t.Fatalf("Wait took %s; WaitDelay is %s", elapsed, stderrWaitDelay)
	}
	if n := strings.Count(out.stderr.String(), "\n"); n != 50 {
		t.Errorf("stderr lines captured = %d, want everything the child itself wrote (50)", n)
	}
	waitForGoroutineBaseline(t, baseline)
}

func TestDefaultSpawnerMaxTurnsKillTearsDownProcessGroup(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "grandchild-started")
	marker := filepath.Join(dir, "grandchild-survived")
	script := writeChildScript(t, `
( touch "`+started+`"; sleep 1; touch "`+marker+`" ) &
while [ ! -f "`+started+`" ]; do sleep 0.05; done
while :; do echo '{"type":"assistant"}'; done
`)
	baseline := runtime.NumGoroutine()
	proc, err := (DefaultSpawner{}).SpawnWithEnv(context.Background(), script, nil, "", []string{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	killedAt := time.Now()
	out := runStreamLikeAgent(proc, 3)

	if out.parseErr == nil || !strings.Contains(out.parseErr.Error(), "max-turns") {
		t.Fatalf("parse error = %v, want max-turns", out.parseErr)
	}
	if out.waitErr == nil {
		t.Fatal("Wait returned nil for a killed child")
	}
	if elapsed := time.Since(killedAt); elapsed > 5*time.Second {
		t.Fatalf("Wait took %s after the kill", elapsed)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("grandchild never started, so the kill was not exercised: %v", err)
	}

	time.Sleep(2 * time.Second)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("grandchild survived the process-group kill (marker stat err=%v)", err)
	}
	waitForGoroutineBaseline(t, baseline)
}

func TestDefaultSpawnerWaitKillsDescendantThatClosedEveryPipe(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "grandchild-started")
	marker := filepath.Join(dir, "grandchild-survived")
	script := writeChildScript(t, `
( touch "`+started+`"; sleep 1; touch "`+marker+`" ) >/dev/null 2>&1 &
while [ ! -f "`+started+`" ]; do sleep 0.05; done
echo '{"type":"result","subtype":"success","result":"done"}'
exit 0
`)
	proc, err := (DefaultSpawner{}).SpawnWithEnv(context.Background(), script, nil, "", []string{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	out := runStreamLikeAgent(proc, 10)
	if out.parseErr != nil || out.waitErr != nil {
		t.Fatalf("parseErr=%v waitErr=%v, want a clean exit", out.parseErr, out.waitErr)
	}
	waitForProcessGroupGone(t, proc)

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("grandchild survived the parent's exit (marker stat err=%v)", err)
	}
}

func TestDefaultSpawnerNonZeroExitStillKillsDescendantHoldingStderr(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "grandchild-started")
	script := writeChildScript(t, `
( touch "`+started+`"; sleep 30 ) >/dev/null &
while [ ! -f "`+started+`" ]; do sleep 0.05; done
echo '{"type":"result","subtype":"error","result":"failed"}'
exit 3
`)
	proc, err := (DefaultSpawner{}).SpawnWithEnv(context.Background(), script, nil, "", []string{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	out := runStreamLikeAgent(proc, 10)
	if out.waitErr == nil || !strings.Contains(out.waitErr.Error(), "exit status 3") {
		t.Fatalf("waitErr = %v, want the child's exit status 3", out.waitErr)
	}
	waitForProcessGroupGone(t, proc)
}
