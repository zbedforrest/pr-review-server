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
	state, failed, hidden := parseCIStatusFromRollup(rollup)
	if state != "failure" {
		t.Errorf("state = %q, want failure (GitHub's rollup over the hidden contexts)", state)
	}
	if len(failed) != 0 {
		t.Errorf("failed = %v, want none: the failing contexts are not readable", failed)
	}
	if hidden != 2 {
		t.Errorf("hidden = %d, want 2", hidden)
	}
}

func TestBatchGetCIStatus_ForbiddenContextsLogOneSummaryLine(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(forbiddenContextsBody))
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
	if status.State != "failure" || status.HiddenContexts != 2 || len(status.FailedChecks) != 0 {
		t.Errorf("got State=%q HiddenContexts=%d FailedChecks=%v, want failure/2/none",
			status.State, status.HiddenContexts, status.FailedChecks)
	}
	if status.MergeStateStatus != "UNSTABLE" {
		t.Errorf("MergeStateStatus = %q, want UNSTABLE", status.MergeStateStatus)
	}

	logs := buf.String()
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
		"CI status: 4 partial errors on 3/60 PRs",
		"FORBIDDEN at pullRequest.commits.nodes.commit.statusCheckRollup.contexts.nodes x3 on 2 PRs (e.g. acme/example#2): nope",
		"NOT_FOUND at pullRequest x1 on 1 PRs (e.g. acme/example#1): gone",
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
