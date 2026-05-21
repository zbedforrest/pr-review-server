package payload

import (
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

func TestSourceWindow_BasicSlice(t *testing.T) {
	src := joinLines("L1", "L2", "L3", "L4", "L5", "L6", "L7", "L8", "L9", "L10")
	before, after := SourceWindow(src, 5, 2)

	wantBefore := []string{"L3", "L4", "L5"}
	wantAfter := []string{"L6", "L7"}
	assertLines(t, "before", before, wantBefore)
	assertLines(t, "after", after, wantAfter)
}

func TestSourceWindow_StartOfFile(t *testing.T) {
	src := joinLines("A", "B", "C", "D")
	before, after := SourceWindow(src, 1, 5)
	assertLines(t, "before", before, []string{"A"})
	assertLines(t, "after", after, []string{"B", "C", "D"})
}

func TestSourceWindow_EndOfFile(t *testing.T) {
	src := joinLines("A", "B", "C", "D")
	before, after := SourceWindow(src, 4, 5)
	assertLines(t, "before", before, []string{"A", "B", "C", "D"})
	assertLines(t, "after", after, nil)
}

func TestSourceWindow_TrailingNewlineIgnored(t *testing.T) {
	// File ending in \n shouldn't add a phantom empty line.
	src := "A\nB\nC\n"
	before, after := SourceWindow(src, 3, 2)
	assertLines(t, "before", before, []string{"A", "B", "C"})
	assertLines(t, "after", after, nil)
}

func TestSourceWindow_OutOfRange(t *testing.T) {
	src := joinLines("only one line")
	before, after := SourceWindow(src, 50, 5)
	if before != nil || after != nil {
		t.Errorf("expected nils for out-of-range line, got %v / %v", before, after)
	}
}

func TestHunkForLine_FindsContainingHunk(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/foo.go b/foo.go",
		"--- a/foo.go",
		"+++ b/foo.go",
		"@@ -10,5 +10,7 @@",
		" ctx1",
		" ctx2",
		"-old line",
		"+new line A",
		"+new line B",
		" ctx3",
		" ctx4",
		"@@ -100,3 +102,3 @@",
		" x",
		"-y",
		"+y2",
		"",
	}, "\n")

	// Line 13 (post-image) falls inside the first hunk [10, 10+7).
	got := HunkForLine(diff, "foo.go", 13)
	if !strings.Contains(got, "+new line A") {
		t.Errorf("expected first hunk body, got:\n%s", got)
	}
	if !strings.HasPrefix(got, "@@ -10,5 +10,7 @@") {
		t.Errorf("expected hunk header at start, got:\n%s", got)
	}
	if strings.Contains(got, "y2") {
		t.Errorf("hunk leaked into the next hunk's body:\n%s", got)
	}

	// Line 103 lands in the second hunk.
	got2 := HunkForLine(diff, "foo.go", 103)
	if !strings.HasPrefix(got2, "@@ -100,3 +102,3 @@") || !strings.Contains(got2, "+y2") {
		t.Errorf("expected second hunk, got:\n%s", got2)
	}
}

func TestHunkForLine_NoMatchReturnsEmpty(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/foo.go b/foo.go",
		"+++ b/foo.go",
		"@@ -1,3 +1,3 @@",
		" a",
		"-b",
		"+B",
		"",
	}, "\n")

	if got := HunkForLine(diff, "foo.go", 999); got != "" {
		t.Errorf("expected empty for line outside all hunks, got %q", got)
	}
	if got := HunkForLine(diff, "other.go", 2); got != "" {
		t.Errorf("expected empty for unknown file, got %q", got)
	}
}

func TestHunkForLine_DefaultCount(t *testing.T) {
	// Unified diff spec: omitted count means 1.
	diff := "diff --git a/x.go b/x.go\n+++ b/x.go\n@@ -5 +5 @@\n-old\n+new\n"
	if got := HunkForLine(diff, "x.go", 5); !strings.Contains(got, "+new") {
		t.Errorf("expected hunk for line 5 with default count=1, got %q", got)
	}
	if got := HunkForLine(diff, "x.go", 6); got != "" {
		t.Errorf("expected empty for line 6 outside default count=1 hunk, got %q", got)
	}
}

func TestBuild_SortsBySeverityThenFileLine(t *testing.T) {
	diff := "" // no diff = no hunk extraction; we're testing sort + counts here.
	comments := []types.LineComment{
		{FilePath: "z.go", LineNumber: 1, CommentBody: "low z", Importance: "LOW"},
		{FilePath: "a.go", LineNumber: 50, CommentBody: "crit a hi", Importance: "CRITICAL"},
		{FilePath: "a.go", LineNumber: 5, CommentBody: "crit a lo", Importance: "CRITICAL"},
		{FilePath: "b.go", LineNumber: 10, CommentBody: "med b", Importance: "MEDIUM"},
		{FilePath: "x.go", LineNumber: 1, CommentBody: "unknown", Importance: ""},
	}

	p := Build("o", "r", 1, "sha", comments, diff, nil)

	if p.Counts.Critical != 2 || p.Counts.Medium != 1 || p.Counts.Low != 1 {
		t.Errorf("counts wrong: %+v", p.Counts)
	}
	if got := []string{
		p.Findings[0].Comment,
		p.Findings[1].Comment,
		p.Findings[2].Comment,
		p.Findings[3].Comment,
		p.Findings[4].Comment,
	}; got[0] != "crit a lo" || got[1] != "crit a hi" || got[2] != "med b" || got[3] != "low z" || got[4] != "unknown" {
		t.Errorf("wrong sort order: %v", got)
	}
}

func TestBuild_AttachesHunkAndSourceWindow(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/foo.go b/foo.go",
		"+++ b/foo.go",
		"@@ -1,3 +1,4 @@",
		" line1",
		" line2",
		"+inserted",
		" line3",
		"",
	}, "\n")

	contents := joinLines("line1", "line2", "inserted", "line3", "tail1", "tail2")
	comments := []types.LineComment{
		{FilePath: "foo.go", LineNumber: 3, CommentBody: "look at insert", Importance: "MEDIUM"},
	}

	p := Build("o", "r", 1, "sha", comments, diff, map[string]string{"foo.go": contents})

	if len(p.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(p.Findings))
	}
	f := p.Findings[0]
	if !strings.Contains(f.DiffHunk, "+inserted") {
		t.Errorf("expected hunk to include +inserted, got: %s", f.DiffHunk)
	}
	if len(f.SourceBefore) == 0 || f.SourceBefore[len(f.SourceBefore)-1] != "inserted" {
		t.Errorf("expected SourceBefore to end with cited line 'inserted', got: %v", f.SourceBefore)
	}
	if len(f.SourceAfter) == 0 || f.SourceAfter[0] != "line3" {
		t.Errorf("expected SourceAfter to start with 'line3', got: %v", f.SourceAfter)
	}
}

func TestBuild_EmptyComments(t *testing.T) {
	p := Build("o", "r", 1, "sha", nil, "", nil)
	if len(p.Findings) != 0 {
		t.Errorf("expected zero findings, got %d", len(p.Findings))
	}
	if p.Counts.Critical+p.Counts.Medium+p.Counts.Low != 0 {
		t.Errorf("expected zero counts, got %+v", p.Counts)
	}
	if p.SchemaVersion != "1" {
		t.Errorf("expected schema_version=1, got %q", p.SchemaVersion)
	}
}

// --- helpers ---

func joinLines(ls ...string) string {
	return strings.Join(ls, "\n")
}

func assertLines(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: len=%d want=%d (got=%v want=%v)", label, len(got), len(want), got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d]=%q want %q", label, i, got[i], want[i])
		}
	}
}
