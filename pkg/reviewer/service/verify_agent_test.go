package service

import (
	"bytes"
	"context"
	"os"
	"strings"
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
