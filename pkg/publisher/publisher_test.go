package publisher

import (
	"testing"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

func TestFindingMarkerRoundTrip(t *testing.T) {
	body := "some text\n" + FindingMarker("abc123") + "\nmore"
	id, ok := FindingIDFromBody(body)
	if !ok || id != "abc123" {
		t.Fatalf("FindingIDFromBody = %q, %v; want abc123, true", id, ok)
	}
	if FindingMarker("x") != "<!-- prism:finding:x -->" {
		t.Fatalf("FindingMarker = %q", FindingMarker("x"))
	}
	if _, ok := FindingIDFromBody("no marker here " + SummaryMarker); ok {
		t.Fatal("summary marker must not parse as a finding marker")
	}
}

func TestMergeConfidence(t *testing.T) {
	cases := []struct {
		name          string
		critical      int
		medium        int
		checkViolated bool
		want          int
	}{
		{"clean", 0, 0, false, 5},
		{"one critical", 1, 0, false, 3},
		{"two critical still minus two", 2, 0, false, 3},
		{"two medium no penalty", 0, 2, false, 5},
		{"three medium", 0, 3, false, 4},
		{"check violated", 0, 0, true, 4},
		{"everything", 1, 3, true, 1},
		{"floor at zero", 3, 5, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeConfidence(tc.critical, tc.medium, tc.checkViolated); got != tc.want {
				t.Fatalf("MergeConfidence(%d,%d,%v) = %d, want %d", tc.critical, tc.medium, tc.checkViolated, got, tc.want)
			}
		})
	}
}

const twoHunkPatch = `@@ -1,4 +1,5 @@
 package main
+import "fmt"

-func old() {}
+func newer() {}
 func keep() {}
@@ -20,3 +21,4 @@ func tail() {
 	a := 1
+	b := 2
 	_ = a
 }`

func TestCommentableLines(t *testing.T) {
	got := CommentableLines(twoHunkPatch)
	wantIn := []int{1, 2, 3, 4, 5, 21, 22, 23, 24}
	for _, l := range wantIn {
		if !got[l] {
			t.Errorf("line %d should be commentable", l)
		}
	}
	wantOut := []int{0, 6, 10, 20, 25}
	for _, l := range wantOut {
		if got[l] {
			t.Errorf("line %d should not be commentable", l)
		}
	}
	if len(got) != len(wantIn) {
		t.Errorf("got %d lines, want %d: %v", len(got), len(wantIn), got)
	}
	if len(CommentableLines("")) != 0 {
		t.Error("empty patch should yield no lines")
	}
}

// f builds an inline-worthy finding: a valid contract asserting current
// production impact, which is what the Greptile-style gate requires.
func f(id, sev, file string, line int, comment string) payload.Finding {
	x := payload.Finding{ID: id, Severity: sev, File: file, Line: line, Comment: comment}
	if file != "SUMMARY" && file != "CHECK" {
		x.FindingContract = &types.FindingContract{SchemaVersion: 1, FindingKind: "production_behavior", Materiality: "current_impact", Falsifiability: "unknown"}
		x.FindingContractStatus = "valid"
	}
	return x
}

func ids(fs []payload.Finding) []string {
	out := make([]string, 0, len(fs))
	for _, x := range fs {
		out = append(out, x.ID)
	}
	return out
}

func TestSelectExcludesSummaryCheckAndPublished(t *testing.T) {
	commentable := map[string]map[int]bool{"a.go": {10: true, 20: true}}
	findings := []payload.Finding{
		f("sum", "unknown", "SUMMARY", 0, "narrative"),
		f("chk", "critical", "CHECK", 0, "check answer"),
		f("pub", "critical", "a.go", 10, "already posted"),
		f("new", "medium", "a.go", 20, "fresh"),
	}
	sel := Select(findings, map[string]bool{"pub": true}, commentable, DefaultPolicy())
	if got := ids(sel.Inline); len(got) != 1 || got[0] != "new" {
		t.Fatalf("Inline = %v, want [new]", got)
	}
	if len(sel.Annotations) != 0 {
		t.Fatalf("Annotations = %v, want none", ids(sel.Annotations))
	}
}

func TestSelectSeverityFloorAndHunkGate(t *testing.T) {
	commentable := map[string]map[int]bool{"a.go": {10: true}}
	findings := []payload.Finding{
		f("low", "low", "a.go", 10, "low sev"),
		f("outside", "critical", "a.go", 99, "not in hunk"),
		f("nofile", "critical", "b.go", 1, "file not in diff"),
		f("noline", "critical", "a.go", 0, "no line"),
		f("ok", "medium", "a.go", 10, "fine"),
	}
	sel := Select(findings, nil, commentable, DefaultPolicy())
	if got := ids(sel.Inline); len(got) != 1 || got[0] != "ok" {
		t.Fatalf("Inline = %v, want [ok]", got)
	}
	if got := ids(sel.Annotations); len(got) != 4 {
		t.Fatalf("Annotations = %v, want 4 entries", got)
	}
}

func TestSelectCapOrdering(t *testing.T) {
	commentable := map[string]map[int]bool{
		"a.go": {1: true, 2: true, 3: true},
		"b.go": {1: true, 2: true},
	}
	findings := []payload.Finding{
		f("m-b1", "medium", "b.go", 1, "x"),
		f("c-b2", "critical", "b.go", 2, "x"),
		f("m-a3", "medium", "a.go", 3, "x"),
		f("c-a2", "critical", "a.go", 2, "x"),
		f("m-a1", "medium", "a.go", 1, "x"),
	}
	sel := Select(findings, nil, commentable, Policy{InlineCap: 3})
	want := []string{"c-a2", "c-b2", "m-a1"}
	got := ids(sel.Inline)
	if len(got) != len(want) {
		t.Fatalf("Inline = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Inline = %v, want %v", got, want)
		}
	}
	if got := ids(sel.Annotations); len(got) != 2 || got[0] != "m-a3" || got[1] != "m-b1" {
		t.Fatalf("Annotations = %v, want [m-a3 m-b1]", got)
	}
}

func TestSelectMinSeverityLowAdmitsLow(t *testing.T) {
	commentable := map[string]map[int]bool{"a.go": {10: true}}
	sel := Select([]payload.Finding{f("low", "low", "a.go", 10, "x")}, nil, commentable, Policy{InlineCap: DefaultInlineCap, InlineMinSeverity: "low"})
	if len(sel.Inline) != 1 {
		t.Fatalf("Inline = %v, want [low]", ids(sel.Inline))
	}
}
