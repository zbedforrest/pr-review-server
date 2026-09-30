package ensemble

import (
	"reflect"
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

func lc(id, file string, line int, imp, body string) types.LineComment {
	return types.LineComment{ID: id, FilePath: file, LineNumber: line, Importance: imp, CommentBody: body}
}

func TestMembersNamespacesIDsAndSkipsNonClaims(t *testing.T) {
	runs := [][]types.LineComment{
		{lc("A-1", "a.go", 10, "LOW", "x"), {FilePath: "SUMMARY"}, lc("A-1", "b.go", 3, "LOW", "dup label")},
		{lc("A-1", "a.go", 12, "MEDIUM", "y"), {FilePath: "c.go", Inactive: true}, lc("", "d.go", 1, "LOW", "no id")},
	}
	var got []string
	for _, m := range Members(runs) {
		got = append(got, m.ID)
	}
	want := []string{"r1:A-1", "r1:A-1#2", "r2:A-1", "r2:F-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestClusterChainsTransitivelyAndKeepsMechanicalApart(t *testing.T) {
	mech := lc("M", "a.go", 11, "LOW", "gate")
	mech.Provenance = "mechanical"
	ms := Members([][]types.LineComment{
		{lc("A", "a.go", 10, "LOW", "")},
		{lc("A", "a.go", 19, "LOW", ""), mech},
		{lc("A", "a.go", 28, "LOW", "")},
	})
	cs := ClusterMembers(ms, DefaultLineWindow)
	if len(cs) != 2 {
		t.Fatalf("got %d clusters, want the 10-19-28 chain plus the mechanical finding alone", len(cs))
	}
	if n := len(cs[0].Members); n != 3 || cs[0].Support() != 3 {
		t.Fatalf("chain cluster has %d members and support %d, want 3 and 3", n, cs[0].Support())
	}
}

func TestClusterWholeFileFindingsJoinOnlyEachOther(t *testing.T) {
	ms := Members([][]types.LineComment{
		{lc("A", "a.go", 0, "LOW", ""), lc("B", "a.go", 5, "LOW", "")},
		{lc("A", "a.go", 0, "LOW", ""), lc("B", "b.go", 5, "LOW", "")},
	})
	cs := ClusterMembers(ms, DefaultLineWindow)
	if len(cs) != 3 {
		t.Fatalf("got %d clusters, want whole-file pair, a.go:5, b.go:5", len(cs))
	}
}

func TestGuardReinsertsEveryUncitedMember(t *testing.T) {
	ms := Members([][]types.LineComment{
		{lc("A", "a.go", 10, "CRITICAL", "nil deref when the user is anonymous")},
		{lc("A", "a.go", 12, "MEDIUM", "anonymous user path crashes"), lc("B", "a.go", 14, "LOW", "unrelated typo")},
	})
	d := Draft{Findings: []types.LineComment{
		{ID: "M1", FilePath: "a.go", LineNumber: 10, Importance: "CRITICAL", CommentBody: "merged", Sources: []string{"r1:A", "r2:A"}},
	}}
	res := Guard(d, ms, Options{})
	if len(res.Findings) != 2 {
		t.Fatalf("got %d findings, want the merged one plus the reinserted typo", len(res.Findings))
	}
	if !reflect.DeepEqual(res.Report.Reinserted, []string{"r2:B"}) {
		t.Fatalf("reinserted = %v, want [r2:B]", res.Report.Reinserted)
	}
	if !reflect.DeepEqual(res.Support, []int{2, 1}) {
		t.Fatalf("support = %v, want [2 1]", res.Support)
	}
}

func TestGuardDropsFindingsThatCiteNoMember(t *testing.T) {
	ms := Members([][]types.LineComment{{lc("A", "a.go", 10, "LOW", "real")}})
	d := Draft{Findings: []types.LineComment{
		{FilePath: "a.go", LineNumber: 10, Importance: "LOW", CommentBody: "real", Sources: []string{"r1:A"}},
		{FilePath: "z.go", LineNumber: 1, Importance: "CRITICAL", CommentBody: "made up", Sources: []string{"r9:X"}},
		{FilePath: "z.go", LineNumber: 2, Importance: "CRITICAL", CommentBody: "no sources"},
	}}
	res := Guard(d, ms, Options{})
	if len(res.Findings) != 1 || res.Report.Invented != 2 {
		t.Fatalf("findings=%d invented=%d, want 1 and 2", len(res.Findings), res.Report.Invented)
	}
	if !reflect.DeepEqual(res.Report.UnknownSources, []string{"r9:X"}) {
		t.Fatalf("unknown sources = %v", res.Report.UnknownSources)
	}
}

func TestGuardCapsSeverityRelocatesAndInheritsContract(t *testing.T) {
	contract := &types.FindingContract{SchemaVersion: 1}
	src := lc("A", "a.go", 10, "MEDIUM", "real")
	src.FindingContract = contract
	ms := Members([][]types.LineComment{{src}})
	d := Draft{Findings: []types.LineComment{
		{FilePath: "elsewhere.go", LineNumber: 99, Importance: "CRITICAL", CommentBody: "escalated", Sources: []string{"r1:A"}},
	}}
	res := Guard(d, ms, Options{})
	f := res.Findings[0]
	if f.Importance != "MEDIUM" || f.FilePath != "a.go" || f.LineNumber != 10 || f.FindingContract != contract {
		t.Fatalf("got %+v, want MEDIUM at a.go:10 with the source contract", f)
	}
	if res.Report.SeverityCapped != 1 || res.Report.Relocated != 1 {
		t.Fatalf("report = %+v", res.Report)
	}
}

func TestGuardTranslatesPriorityIDsAndForcesRequestChangesOnCritical(t *testing.T) {
	ms := Members([][]types.LineComment{{lc("A", "a.go", 10, "CRITICAL", "bad"), lc("B", "b.go", 3, "LOW", "nit")}})
	d := Draft{
		Findings: []types.LineComment{
			{ID: "M2", FilePath: "b.go", LineNumber: 3, Importance: "LOW", CommentBody: "nit", Sources: []string{"r1:B"}},
			{ID: "M1", FilePath: "a.go", LineNumber: 10, Importance: "CRITICAL", CommentBody: "bad", Sources: []string{"r1:A"}},
		},
		Summary: &types.SummaryBlock{Verdict: "approve", PriorityIDs: []string{"M1", "ghost"}},
	}
	res := Guard(d, ms, Options{})
	if res.Summary.Verdict != "request_changes" {
		t.Fatalf("verdict = %q, want request_changes with an active critical", res.Summary.Verdict)
	}
	if !reflect.DeepEqual(res.Summary.PriorityIDs, []string{"E-2"}) {
		t.Fatalf("priority ids = %v, want [E-2]", res.Summary.PriorityIDs)
	}
}

func TestGuardMinSupportCriticalDowngradesLoneCriticals(t *testing.T) {
	ms := Members([][]types.LineComment{
		{lc("A", "a.go", 10, "CRITICAL", "shared"), lc("B", "b.go", 1, "CRITICAL", "lone")},
		{lc("A", "a.go", 11, "CRITICAL", "shared")},
	})
	d := Deterministic(ClusterMembers(ms, DefaultLineWindow))
	res := Guard(d, ms, Options{MinSupportCritical: 2})
	got := map[string]string{}
	for _, f := range res.Findings {
		got[f.FilePath] = f.Importance
	}
	if got["a.go"] != "CRITICAL" || got["b.go"] != "MEDIUM" || res.Report.SupportDowngraded != 1 {
		t.Fatalf("severities = %v, report = %+v", got, res.Report)
	}
}

func TestDeterministicKeepsOneFindingPerClusterCitingAllMembers(t *testing.T) {
	ms := Members([][]types.LineComment{
		{lc("A", "a.go", 10, "LOW", "short")},
		{lc("A", "a.go", 12, "MEDIUM", "the more specific statement")},
		{lc("A", "b.go", 5, "LOW", "other file")},
	})
	res := Guard(Deterministic(ClusterMembers(ms, DefaultLineWindow)), ms, Options{})
	if len(res.Findings) != 2 || len(res.Report.Reinserted) != 0 {
		t.Fatalf("findings=%d reinserted=%v", len(res.Findings), res.Report.Reinserted)
	}
	f := res.Findings[0]
	if f.CommentBody != "the more specific statement" || f.Importance != "MEDIUM" || !reflect.DeepEqual(f.Sources, []string{"r1:A", "r2:A"}) {
		t.Fatalf("got %+v", f)
	}
}

func TestGuardDoesNotLetTheAuthorAskForChangesWithoutACritical(t *testing.T) {
	ms := Members([][]types.LineComment{{lc("A", "a.go", 10, "MEDIUM", "worth fixing")}})
	d := Draft{
		Findings: []types.LineComment{{FilePath: "a.go", LineNumber: 10, Importance: "MEDIUM", CommentBody: "worth fixing", Sources: []string{"r1:A"}}},
		Summary:  &types.SummaryBlock{Verdict: "request_changes"},
	}
	if v := Guard(d, ms, Options{}).Summary.Verdict; v != "approve_suggestions" {
		t.Fatalf("verdict = %q, want approve_suggestions without an active critical", v)
	}
	if v := Guard(Draft{}, nil, Options{}).Summary.Verdict; v != "approve" {
		t.Fatalf("empty review verdict = %q, want approve", v)
	}
}
