package github

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const forbiddenContextsBody = `{
	"data":{"pr0":{"pullRequest":{
		"mergeStateStatus":"UNSTABLE","reviewDecision":null,
		"commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"FAILURE","contexts":{"nodes":[
			{"__typename":"CheckRun","name":"build","conclusion":"SUCCESS","status":"COMPLETED"},
			null,
			null
		]}}}}]}}}},
	"errors":[
		{"type":"FORBIDDEN","message":"Resource not accessible by integration",
		 "path":["pr0","pullRequest","commits","nodes",0,"commit","statusCheckRollup","contexts","nodes",1]},
		{"type":"FORBIDDEN","message":"Resource not accessible by integration",
		 "path":["pr0","pullRequest","commits","nodes",0,"commit","statusCheckRollup","contexts","nodes",2]}
	]
}`

func TestParseCIStatusFromRollup_HiddenContextsKeepRollupState(t *testing.T) {
	rollup := &StatusCheckRollup{
		State: "FAILURE",
		Contexts: ContextsData{Nodes: []CheckNode{
			{TypeName: "CheckRun", Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{},
			{},
		}},
	}
	state, failed, nullNodes := parseCIStatusFromRollup(rollup)
	if state != "failure" {
		t.Errorf("state = %q, want failure (GitHub's rollup over the hidden contexts)", state)
	}
	if len(failed) != 0 {
		t.Errorf("failed = %v, want none: the failing contexts are not readable", failed)
	}
	if nullNodes != 2 {
		t.Errorf("nullNodes = %d, want 2", nullNodes)
	}
}

func batchGetCIStatusWithLogs(t *testing.T, body string) (*CIStatus, string) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()
	client := NewClient("test-token", "")
	client.httpClient = &http.Client{Transport: &redirectTransport{targetURL: ts.URL}}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	results, err := client.BatchGetCIStatus(context.Background(), []PRInfo{{Owner: "acme", Repo: "example", Number: 7}})
	if err != nil {
		t.Fatalf("BatchGetCIStatus: %v", err)
	}
	status := results["acme/example/7"]
	if status == nil {
		t.Fatalf("no result: %v", results)
	}
	return status, buf.String()
}

func TestBatchGetCIStatus_NonForbiddenNullNodesCountAsUnreadable(t *testing.T) {
	body := strings.Replace(forbiddenContextsBody, `"type":"FORBIDDEN","message":"Resource not accessible by integration",
		 "path":["pr0","pullRequest","commits","nodes",0,"commit","statusCheckRollup","contexts","nodes",1]`,
		`"type":"INTERNAL","message":"Something went wrong",
		 "path":["pr0","pullRequest","commits","nodes",0,"commit","statusCheckRollup","contexts","nodes",1]`, 1)
	status, logs := batchGetCIStatusWithLogs(t, body)
	if status.State != "failure" || status.HiddenContexts != 1 || status.UnreadableContexts != 1 {
		t.Errorf("got State=%q Hidden=%d Unreadable=%d, want failure/1/1",
			status.State, status.HiddenContexts, status.UnreadableContexts)
	}
	for _, want := range []string{"CI status: 2 partial errors on 1/1 PRs", "INTERNAL at " + ciContextsPath + " x1", "FORBIDDEN at " + ciContextsPath + " x1"} {
		if !strings.Contains(logs, want) {
			t.Errorf("summary missing %q:\n%s", want, logs)
		}
	}
}

func TestBatchGetCIStatus_ForbiddenContextsLogOneSummaryLine(t *testing.T) {
	status, logs := batchGetCIStatusWithLogs(t, forbiddenContextsBody)
	if status.State != "failure" || status.HiddenContexts != 2 || status.UnreadableContexts != 0 || len(status.FailedChecks) != 0 {
		t.Errorf("got State=%q Hidden=%d Unreadable=%d FailedChecks=%v, want failure/2/0/none",
			status.State, status.HiddenContexts, status.UnreadableContexts, status.FailedChecks)
	}
	if status.MergeStateStatus != "UNSTABLE" {
		t.Errorf("MergeStateStatus = %q, want UNSTABLE", status.MergeStateStatus)
	}

	if strings.Contains(logs, "client partial error") {
		t.Errorf("per-node partial error lines still logged:\n%s", logs)
	}
	if n := strings.Count(logs, "[GRAPHQL] CI status: 2 partial errors on 1/1 PRs"); n != 1 {
		t.Fatalf("summary lines = %d, want 1:\n%s", n, logs)
	}
	for _, want := range []string{
		"FORBIDDEN at pullRequest.commits.nodes.commit.statusCheckRollup.contexts.nodes x2 on 1 PRs (e.g. acme/example#7)",
		"Resource not accessible by integration",
		"Commit statuses: Read",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("summary line missing %q:\n%s", want, logs)
		}
	}
}

func TestPartialErrorSummary_GroupsAcrossBatchesAndTypes(t *testing.T) {
	s := newPartialErrorSummary()
	s.add([]GraphQLPartialError{
		{Type: "FORBIDDEN", Message: "nope", Path: []interface{}{"pr1", "pullRequest", "commits", "nodes", float64(0), "commit", "statusCheckRollup", "contexts", "nodes", float64(3)}},
		{Type: "FORBIDDEN", Message: "nope", Path: []interface{}{"pr1", "pullRequest", "commits", "nodes", float64(0), "commit", "statusCheckRollup", "contexts", "nodes", float64(4)}},
		{Type: "NOT_FOUND", Message: "gone", Path: []interface{}{"pr0", "pullRequest"}},
		{Type: "RATE_LIMITED", Message: "slow down", Path: []interface{}{"rateLimit", "remaining"}},
	}, []PRInfo{{Owner: "acme", Repo: "example", Number: 1}, {Owner: "acme", Repo: "example", Number: 2}})
	s.add([]GraphQLPartialError{
		{Type: "FORBIDDEN", Message: "nope", Path: []interface{}{"pr0", "pullRequest", "commits", "nodes", float64(0), "commit", "statusCheckRollup", "contexts", "nodes", float64(0)}},
	}, []PRInfo{{Owner: "acme", Repo: "other", Number: 9}})

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	s.log("CI status", 60)

	line := buf.String()
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("want exactly one line:\n%s", line)
	}
	for _, want := range []string{
		"CI status: 5 partial errors on 3/60 PRs",
		"FORBIDDEN at pullRequest.commits.nodes.commit.statusCheckRollup.contexts.nodes x3 on 2 PRs (e.g. acme/example#2): nope",
		"NOT_FOUND at pullRequest x1 on 1 PRs (e.g. acme/example#1): gone",
		"RATE_LIMITED at rateLimit.remaining x1 on 0 PRs: slow down",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in:\n%s", want, line)
		}
	}
	if strings.Index(line, "FORBIDDEN") > strings.Index(line, "NOT_FOUND") {
		t.Errorf("groups should be ordered by count desc:\n%s", line)
	}
}

func TestPartialErrorSummary_SilentWithoutErrors(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	s := newPartialErrorSummary()
	s.add(nil, []PRInfo{{Owner: "acme", Repo: "example", Number: 1}})
	s.log("CI status", 1)
	if buf.Len() != 0 {
		t.Errorf("expected no output, got %q", buf.String())
	}
}
