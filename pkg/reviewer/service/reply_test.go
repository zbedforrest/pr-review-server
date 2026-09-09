package service

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"pr-review-server/pkg/reviewer/types"
)

func replyInput(sha string) ReplyInput {
	return ReplyInput{
		Owner: "acme", Repo: "example", DefaultBranch: "main", PRNumber: 1, HeadSHA: sha,
		Fingerprint: "change.txt:1:abc", FindingBody: "**[MEDIUM] change.txt is not read anywhere.**",
		Thread: []ReplyMessage{
			{Author: "prism", Ours: true, Body: "**[MEDIUM] change.txt is not read anywhere.**", At: time.Date(2026, 9, 9, 17, 0, 0, 0, time.UTC)},
			{Author: "pilot", Body: "It is read by the deploy script, see hello.txt.", At: time.Date(2026, 9, 9, 17, 5, 0, 0, time.UTC)},
		},
		AuthorReply: "It is read by the deploy script, see hello.txt.", Class: "pushback",
	}
}

func runReply(t *testing.T, final string) (*ReplyResult, error, *fakeSpawner) {
	t.Helper()
	bare, sha := setupLocalBareRepo(t)
	cloneRoot := t.TempDir()
	seedAgentCache(t, cloneRoot, "acme", "example", bare)
	stream := `{"type":"system","subtype":"init","model":"claude-fable-5-1"}
{"type":"assistant","message":{"model":"claude-fable-5-1","content":[{"type":"text","text":"looking"}]}}
{"type":"result","result":` + jsonString(final) + `}
`
	spawner := &fakeSpawner{proc: &fakeProcess{stdout: bytes.NewBufferString(stream), stderr: &bytes.Buffer{}, killCh: make(chan struct{})}}
	cfg := AgentConfig{CloneRootDir: cloneRoot, LogsDir: t.TempDir(), WallClock: time.Minute, MaxTurns: 10, Model: "claude-fable-5-1"}
	out, err := RunAgentReply(context.Background(), cfg, spawner, replyInput(sha))
	return out, err, spawner
}

func TestRunAgentReply_HoldWithResolvingEvidence(t *testing.T) {
	out, err, spawner := runReply(t, `{"decision":"hold","reply":"hello.txt only contains the word hi; nothing there reads change.txt, so the file is still unused at this commit.","cited":[{"file":"hello.txt","line":1}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.Decision != "hold" || len(out.Cited) != 1 || out.Cited[0].File != "hello.txt" || out.ServedModel != "claude-fable-5-1" || out.AssistantTurns != 1 {
		t.Fatalf("result = %+v", out)
	}
	prompt := spawner.args[1]
	for _, want := range []string{"change.txt:1:abc", "It is read by the deploy script", "pilot", `"decision"`, "600"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(prompt, "prism:finding") {
		t.Errorf("prompt should not carry our HTML markers")
	}
}

func TestRunAgentReply_HoldWithoutResolvingEvidenceDegradesToAbstain(t *testing.T) {
	out, err, _ := runReply(t, `{"decision":"hold","reply":"Still unused.","cited":[{"file":"missing.go","line":3},{"file":"hello.txt","line":99}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.Decision != "abstain" || out.Reply != "" || len(out.Cited) != 0 || len(out.Unresolved) != 2 {
		t.Fatalf("result = %+v", out)
	}
}

func TestRunAgentReply_ConcedeNeedsResolvingEvidenceLikeAHold(t *testing.T) {
	out, err, _ := runReply(t, "```json\n{\"decision\":\"concede\",\"reply\":\"You're right, withdrawn.\",\"cited\":[{\"file\":\"nope.go\",\"line\":1}]}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if out.Decision != "abstain" || out.Reply != "" {
		t.Fatalf("an unsupported concession must not dismiss the finding: %+v", out)
	}
	out, err, _ = runReply(t, `{"decision":"concede","reply":"You're right, hello.txt is the only reader. Withdrawn.","cited":[{"file":"hello.txt","line":1}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.Decision != "concede" || len(out.Cited) != 1 {
		t.Fatalf("result = %+v", out)
	}
}

func TestRunAgentReply_RefusesReplyTextThatLooksLikeACredential(t *testing.T) {
	out, err, _ := runReply(t, `{"decision":"answer","reply":"The token in the env is ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123 and it is used on line 3.","cited":[{"file":"hello.txt","line":1}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.Decision != "abstain" || out.Reply != "" {
		t.Fatalf("result = %+v", out)
	}
}

func TestRunAgentReply_RunsWithoutTheBashTool(t *testing.T) {
	_, err, spawner := runReply(t, `{"decision":"abstain","reply":""}`)
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range spawner.args {
		if a == "--tools" {
			if spawner.args[i+1] != "Read,Grep,Glob" {
				t.Fatalf("tools = %q", spawner.args[i+1])
			}
			return
		}
	}
	t.Fatal("no --tools flag")
}

func TestRunAgentReply_UnparseableOrUnknownDecisionIsAbstain(t *testing.T) {
	out, err, _ := runReply(t, `I think the author is right but I am not sure.`)
	if err != nil {
		t.Fatal(err)
	}
	if out.Decision != "abstain" {
		t.Fatalf("result = %+v", out)
	}
	out, err, _ = runReply(t, `{"decision":"escalate","reply":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.Decision != "abstain" {
		t.Fatalf("result = %+v", out)
	}
}

func TestParseReplyJSONNormalizesWhitespace(t *testing.T) {
	d, ok := parseReplyJSON("prefix {\"decision\":\"answer\",\"reply\":\"line one\\n\\n  line   two\"} suffix")
	if !ok || d.Decision != "answer" || d.Reply != "line one line two" {
		t.Fatalf("parsed = %+v ok=%v", d, ok)
	}
	if _, ok := parseReplyJSON("no json here"); ok {
		t.Error("expected failure")
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

var _ = types.EvidenceRef{}
