package ensemble

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/types"
)

type fakeCompleter struct {
	reply  string
	err    error
	prompt string
}

func (f *fakeCompleter) CompleteStructured(_ context.Context, req llm.StructuredRequest) (string, llm.Call, error) {
	f.prompt = req.User
	return f.reply, llm.Call{ServedModel: "fake", CostUSD: 0.001}, f.err
}

func twoRuns() [][]types.LineComment {
	return [][]types.LineComment{
		{lc("A-1", "app/views.py", 40, "CRITICAL", "Anonymous users crash on the stats page."), lc("A-2", "app/forms.py", 7, "LOW", "Blank option was dropped.")},
		{lc("A-1", "app/views.py", 41, "MEDIUM", "request.user.id is None for anonymous users.")},
	}
}

func TestMergeMapsAuthorAliasesBackToMembersAndReinsertsOmissions(t *testing.T) {
	fc := &fakeCompleter{reply: `{"findings":[{"key":"M1","sources":["S2","S3"],"file_path":"app/views.py","line_number":40,
		"importance":"CRITICAL","comment_body":"Anonymous users crash on the stats page because request.user.id is None."}],
		"summary":{"verdict":"request_changes","upshot":"Fix the anonymous crash.","priority":["M1"]}}`}
	res := Merge(context.Background(), twoRuns(), &LLMAuthor{Client: fc}, Options{})
	if res.Method != "author" || res.Clusters != 2 {
		t.Fatalf("method=%s clusters=%d", res.Method, res.Clusters)
	}
	if len(res.Findings) != 2 || !reflect.DeepEqual(res.Findings[0].Sources, []string{"r1:A-1", "r2:A-1"}) {
		t.Fatalf("findings = %+v", res.Findings)
	}
	if !reflect.DeepEqual(res.Report.Reinserted, []string{"r1:A-2"}) || res.Support[0] != 2 {
		t.Fatalf("report=%+v support=%v", res.Report, res.Support)
	}
	if !reflect.DeepEqual(res.Summary.PriorityIDs, []string{"E-1"}) || res.Call.ServedModel != "fake" {
		t.Fatalf("summary=%+v call=%+v", res.Summary, res.Call)
	}
	if strings.Contains(fc.prompt, "r1:A-1") || !strings.Contains(fc.prompt, "S2 [CRITICAL] app/views.py:40") {
		t.Fatalf("prompt must use opaque aliases, not member ids:\n%s", fc.prompt)
	}
}

func TestMergeFallsBackToDeterministicWhenTheAuthorFails(t *testing.T) {
	for name, fc := range map[string]*fakeCompleter{
		"call error":   {err: errors.New("provider down")},
		"schema error": {reply: `not json`},
	} {
		res := Merge(context.Background(), twoRuns(), &LLMAuthor{Client: fc}, Options{})
		if res.Method != "deterministic" || res.AuthorErr == "" || len(res.Findings) != 2 || len(res.Report.Reinserted) != 0 {
			t.Errorf("%s: method=%s err=%q findings=%d reinserted=%v", name, res.Method, res.AuthorErr, len(res.Findings), res.Report.Reinserted)
		}
	}
}

func TestMergeWithoutAnAuthorIsDeterministic(t *testing.T) {
	res := Merge(context.Background(), twoRuns(), nil, Options{})
	if res.Method != "deterministic" || len(res.Findings) != 2 || res.Findings[1].Importance != "CRITICAL" || res.Support[1] != 2 {
		t.Fatalf("got %+v", res)
	}
}
