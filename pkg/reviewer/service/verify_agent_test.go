package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"pr-review-server/pkg/reviewer/types"
	"pr-review-server/pkg/reviewer/verify"
)

func runVerifyAgent(t *testing.T, final string) (*VerifyAgentResult, error, *fakeSpawner, string) {
	t.Helper()
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)
	stream := `{"type":"system","subtype":"init","model":"claude-fable-5-1"}
{"type":"assistant","message":{"model":"claude-fable-5-1","content":[{"type":"text","text":"reading"}]}}
{"type":"result","total_cost_usd":0.31,"result":` + jsonString(final) + `}
`
	spawner := &fakeSpawner{proc: &fakeProcess{stdout: bytes.NewBufferString(stream), stderr: &bytes.Buffer{}, killCh: make(chan struct{})}}
	cfg := AgentConfig{CloneRootDir: cloneRoot, LogsDir: t.TempDir(), WallClock: time.Minute, MaxTurns: 20, Model: "claude-fable-5-1"}
	findings := []verify.Finding{{Key: "F1", Comment: types.LineComment{FilePath: "hello.txt", LineNumber: 1, Importance: "MEDIUM", CommentBody: "hello.txt is never read"}}}
	out, err := RunAgentVerify(context.Background(), cfg, spawner, VerifyAgentInput{
		Owner: "acme", Repo: "example", DefaultBranch: "main", PRNumber: 1, HeadSHA: sha, BaseSHA: sha,
		Packet: &verify.Packet{ByFinding: map[string][]string{"F1": nil}}, Findings: findings,
	})
	return out, err, spawner, cloneRoot
}

func TestRunAgentVerify_ResolvesCitesInBothCheckoutsAndCleansThemUp(t *testing.T) {
	out, err, spawner, cloneRoot := runVerifyAgent(t, `{"decisions":[{"key":"F1","decision":"drop","cites":["hello.txt:1","base:hello.txt:1","nope.txt:1"],"reason":"It is read.","refuting_quote":"hello.txt:1","revised":null}]}`)
	if err != nil {
		t.Fatal(err)
	}
	d := out.Decisions[0]
	if len(d.Cites) != 2 || strings.Join(d.Unresolved, ",") != "nope.txt:1" || !strings.HasPrefix(d.Quote, "hello.txt:1\n") {
		t.Fatalf("decision = %+v", d)
	}
	if out.CostUSD != 0.31 || out.ServedModel != "claude-fable-5-1" {
		t.Fatalf("result = %+v", out)
	}
	prompt := spawner.args[1]
	if !strings.Contains(prompt, "__base") || !strings.Contains(prompt, "hello.txt is never read") {
		t.Fatalf("prompt:\n%s", prompt)
	}
	entries, _ := os.ReadDir(cloneRoot)
	for _, e := range entries {
		if strings.Contains(e.Name(), "__verify__") {
			t.Fatalf("worktree left behind: %s", e.Name())
		}
	}
}

func TestRunAgentVerify_RunsWithoutTheBashTool(t *testing.T) {
	_, err, spawner, _ := runVerifyAgent(t, `{"decisions":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range spawner.args {
		if a == "--tools" && spawner.args[i+1] == "Read,Grep,Glob" {
			return
		}
	}
	t.Fatalf("args = %v", spawner.args)
}

func TestRunAgentVerify_FinalMessageWithoutJSONIsAnError(t *testing.T) {
	if _, err, _, _ := runVerifyAgent(t, "I looked around and could not decide."); err == nil {
		t.Fatal("want error")
	}
}

func TestEscalateVerify_SendsOnlyUnavailableFindingsAndAppliesTheirDecisions(t *testing.T) {
	orig := runVerifyAgentFn
	var sent []string
	runVerifyAgentFn = func(_ context.Context, _ AgentConfig, _ Spawner, in VerifyAgentInput) (*VerifyAgentResult, error) {
		for _, f := range in.Findings {
			sent = append(sent, f.Key)
		}
		return &VerifyAgentResult{ServedModel: "m", CostUSD: 0.5, Decisions: []verify.AgentDecision{
			{Key: "F2", Decision: "hold", Reason: "r", Cites: []verify.AgentCite{{File: "a.go", Start: 1, End: 1}}},
			{Key: "F1", Decision: "drop", Reason: "not escalated", Cites: []verify.AgentCite{{File: "a.go", Start: 1, End: 1}}},
		}}, nil
	}
	t.Cleanup(func() { runVerifyAgentFn = orig })
	mk := func(key, status string) (verify.Finding, types.LineComment) {
		c := types.LineComment{FilePath: "a.go", LineNumber: 1, Importance: "MEDIUM", CommentBody: key}
		f := verify.Finding{Key: key, Comment: c}
		c.Verification = &types.Verification{Status: status}
		return f, c
	}
	f1, c1 := mk("F1", types.VerifyKeep)
	f2, c2 := mk("F2", types.VerifyUnavailable)
	f3, c3 := mk("F3", types.VerifyUnavailable)
	comments := []types.LineComment{c1, c2, c3}
	res, err := EscalateVerify(context.Background(), AgentConfig{}, nil, VerifyAgentInput{Findings: []verify.Finding{f1, f2, f3}}, comments)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sent, ",") != "F2,F3" || res.Escalated != 2 || res.CostUSD != 0.5 {
		t.Fatalf("sent=%v res=%+v", sent, res)
	}
	if comments[0].Verification.Status != types.VerifyKeep || comments[1].Verification.Status != types.VerifyHold || comments[1].Verification.Phase != 2 {
		t.Fatalf("comments = %+v %+v", comments[0].Verification, comments[1].Verification)
	}
	if comments[2].Verification.Status != types.VerifyUnavailable || comments[2].Verification.Phase != 2 || res.Decisions["no_decision"] != 1 {
		t.Fatalf("F3 = %+v decisions=%v", comments[2].Verification, res.Decisions)
	}
}

func TestEscalateVerify_RunsBatchesInParallelAndKeepsAFailedBatchUnavailable(t *testing.T) {
	origFn, origSize := runVerifyAgentFn, verify.MaxAgentFindings
	verify.MaxAgentFindings = 2
	var mu sync.Mutex
	var batches [][]string
	runVerifyAgentFn = func(_ context.Context, _ AgentConfig, _ Spawner, in VerifyAgentInput) (*VerifyAgentResult, error) {
		var keys []string
		var ds []verify.AgentDecision
		for _, f := range in.Findings {
			keys = append(keys, f.Key)
			ds = append(ds, verify.AgentDecision{Key: f.Key, Decision: "keep", Reason: "ok"})
		}
		mu.Lock()
		batches = append(batches, keys)
		mu.Unlock()
		if keys[0] == "F3" {
			return nil, errors.New("wall-clock timeout")
		}
		return &VerifyAgentResult{ServedModel: "m", CostUSD: 0.1, Decisions: ds}, nil
	}
	t.Cleanup(func() { runVerifyAgentFn, verify.MaxAgentFindings = origFn, origSize })

	var findings []verify.Finding
	var comments []types.LineComment
	for i := 1; i <= 5; i++ {
		key := fmt.Sprintf("F%d", i)
		c := types.LineComment{FilePath: "a.go", LineNumber: i, Importance: "LOW", CommentBody: key}
		findings = append(findings, verify.Finding{Key: key, Comment: c})
		c.Verification = &types.Verification{Status: types.VerifyUnavailable}
		comments = append(comments, c)
	}
	res, err := EscalateVerify(context.Background(), AgentConfig{}, nil, VerifyAgentInput{Findings: findings}, comments)
	if err != nil {
		t.Fatal(err)
	}
	if res.Batches != 3 || res.FailedBatches != 1 || len(batches) != 3 || res.CostUSD < 0.19 || res.CostUSD > 0.21 {
		t.Fatalf("res=%+v batches=%v", res, batches)
	}
	for i, want := range []string{"keep", "keep", "unavailable", "unavailable", "keep"} {
		if comments[i].Verification.Status != want {
			t.Errorf("F%d = %s, want %s", i+1, comments[i].Verification.Status, want)
		}
	}
}

func TestEscalateVerify_ReturnsAnErrorOnlyWhenEveryBatchFails(t *testing.T) {
	orig := runVerifyAgentFn
	runVerifyAgentFn = func(context.Context, AgentConfig, Spawner, VerifyAgentInput) (*VerifyAgentResult, error) {
		return nil, errors.New("boom")
	}
	t.Cleanup(func() { runVerifyAgentFn = orig })
	c := types.LineComment{FilePath: "a.go", LineNumber: 1, CommentBody: "x", Verification: &types.Verification{Status: types.VerifyUnavailable}}
	comments := []types.LineComment{c}
	if _, err := EscalateVerify(context.Background(), AgentConfig{}, nil, VerifyAgentInput{Findings: []verify.Finding{{Key: "F1", Comment: c}}}, comments); err == nil {
		t.Fatal("want error")
	}
	if comments[0].Verification.Status != types.VerifyUnavailable {
		t.Fatalf("status = %s", comments[0].Verification.Status)
	}
}

func TestEscalateVerify_HoldsAServerSlotPerBatch(t *testing.T) {
	origFn, origSize := runVerifyAgentFn, verify.MaxAgentFindings
	verify.MaxAgentFindings = 1
	var mu sync.Mutex
	running, peak := 0, 0
	runVerifyAgentFn = func(_ context.Context, _ AgentConfig, _ Spawner, in VerifyAgentInput) (*VerifyAgentResult, error) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return &VerifyAgentResult{Decisions: []verify.AgentDecision{{Key: in.Findings[0].Key, Decision: "keep", Reason: "ok"}}}, nil
	}
	t.Cleanup(func() { runVerifyAgentFn, verify.MaxAgentFindings = origFn, origSize })
	var findings []verify.Finding
	var comments []types.LineComment
	for i := 1; i <= 3; i++ {
		c := types.LineComment{FilePath: "a.go", LineNumber: i, CommentBody: fmt.Sprint(i)}
		findings = append(findings, verify.Finding{Key: fmt.Sprintf("F%d", i), Comment: c})
		c.Verification = &types.Verification{Status: types.VerifyUnavailable}
		comments = append(comments, c)
	}
	res, err := EscalateVerify(context.Background(), AgentConfig{}, nil, VerifyAgentInput{Findings: findings, Slots: make(chan struct{}, 1)}, comments)
	if err != nil || res.Batches != 3 || peak != 1 {
		t.Fatalf("err=%v res=%+v peak=%d", err, res, peak)
	}
}
