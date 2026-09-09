package html

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"pr-review-server/pkg/reviewer/types"
)

func TestParseDiff(t *testing.T) {
	diff := `diff --git a/file1.txt b/file1.txt
index e69de29..d00491f 100644
--- a/file1.txt
+++ b/file1.txt
@@ -0,0 +1 @@
+Hello World
diff --git a/file2.go b/file2.go
index 6e9b81b..1b61971 100644
--- a/file2.go
+++ b/file2.go
@@ -1,4 +1,4 @@
 package main

-func main() {}
+func main() { /* hello */ }

`
	expected := []*DiffFile{
		{
			Path: "file1.txt",
			Lines: []Line{
				{Type: "added", Content: "Hello World", OldLineNumber: 0, NewLineNumber: 1},
			},
		},
		{
			Path: "file2.go",
			Lines: []Line{
				{Type: "context", Content: "package main", OldLineNumber: 1, NewLineNumber: 1},
				{Type: "context", Content: "", OldLineNumber: 2, NewLineNumber: 2},
				{Type: "deleted", Content: "func main() {}", OldLineNumber: 3, NewLineNumber: 0},
				{Type: "added", Content: "func main() { /* hello */ }", OldLineNumber: 0, NewLineNumber: 3},
				{Type: "context", Content: "", OldLineNumber: 4, NewLineNumber: 4},
			},
		},
	}

	actual := ParseDiff(diff)

	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("ParseDiff() = %v, want %v", actual, expected)
		// For more detailed output on mismatch
		for i := range actual {
			if i >= len(expected) {
				t.Errorf("Extra file in actual: %+v", actual[i])
				continue
			}
			if !reflect.DeepEqual(actual[i], expected[i]) {
				t.Errorf("Mismatch in file %d:", i)
				t.Errorf("  Actual:   %+v", actual[i])
				t.Errorf("  Expected: %+v", expected[i])
				for j := range actual[i].Lines {
					if j >= len(expected[i].Lines) {
						t.Errorf("    Extra line in actual: %+v", actual[i].Lines[j])
						continue
					}
					if !reflect.DeepEqual(actual[i].Lines[j], expected[i].Lines[j]) {
						t.Errorf("    Mismatch in line %d:", j)
						t.Errorf("      Actual:   %+v", actual[i].Lines[j])
						t.Errorf("      Expected: %+v", expected[i].Lines[j])
					}
				}
			}
		}
	}
}

func TestGenerateReport_WithGeneralComments(t *testing.T) {
	comments := []types.LineComment{
		{
			FilePath:    "app.go",
			LineNumber:  13,
			CommentBody: "Add error handling here",
			Importance:  "CRITICAL",
		},
		{
			FilePath:    "GENERAL",
			LineNumber:  0,
			CommentBody: "Overall architecture looks good",
			Importance:  "LOW",
		},
		{
			FilePath:    "SUMMARY",
			LineNumber:  0,
			CommentBody: "## Test Coverage Analysis\nAdequate coverage overall",
			Importance:  "",
		},
	}

	diff := `diff --git a/app.go b/app.go
index 123..456 100644
--- a/app.go
+++ b/app.go
@@ -12,6 +12,7 @@ func main() {
 	fmt.Println("Hello")
+	// New line
 }`

	testTime := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	report, err := GenerateReport(comments, diff, 123, "https://github.com/test-owner/test-repo/pull/123", "Test PR body", "", "abc1234", "gemini-pro", 0, 0, 0, testTime)
	assert.NoError(t, err)

	// Should contain general comments section
	assert.Contains(t, report, "<h2>General Comments</h2>")
	assert.Contains(t, report, "Overall architecture looks good")
	assert.Contains(t, report, "class=\"general-comment\"")

	// Should contain summary section
	assert.Contains(t, report, "<h2>Review Summary</h2>")
	assert.Contains(t, report, "Test Coverage Analysis")

	// Should contain file-specific comments
	assert.Contains(t, report, "Add error handling here")

	// Should have correct comment counts - the order depends on how they're processed
	assert.Contains(t, report, "Comment 2 of 3") // General comment
	assert.Contains(t, report, "Comment 3 of 3") // Summary comment
	// Note: File-specific comment might not appear inline if line not in diff
}

func TestGenerateReport_WithAdjacentComments(t *testing.T) {
	// Comments that reference lines not in the diff should appear in adjacent section
	comments := []types.LineComment{
		{
			FilePath:    "app.go",
			LineNumber:  999, // Line not in diff
			CommentBody: "This line is not in the diff",
			Importance:  "CRITICAL",
		},
		{
			FilePath:    "test.go",
			LineNumber:  13, // Line that IS in diff
			CommentBody: "This line is in the diff",
			Importance:  "MEDIUM",
		},
		{
			FilePath:    "SUMMARY",
			LineNumber:  0,
			CommentBody: "## Testing Summary\nOverall assessment",
			Importance:  "",
		},
	}

	diff := `diff --git a/test.go b/test.go
index 123..456 100644
--- a/test.go
+++ b/test.go
@@ -10,6 +10,7 @@ func main() {
 	fmt.Println("Hello")
+	// New line at 13
 }`

	// Provide file contents to enable adjacent comments functionality
	fileContents := map[string]string{
		"app.go": `package main

import "fmt"

func main() {
    fmt.Println("Hello")
}`,
	}

	testTime := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	report, err := GenerateReportWithContext(comments, diff, 123, "https://github.com/test-owner/test-repo/pull/123", "Test PR body", "", "abc1234", "gemini-pro", 0, 0, 0, testTime, fileContents)
	assert.NoError(t, err)

	// Should contain adjacent comments section for comment that can't be displayed inline
	assert.Contains(t, report, "<h2>Adjacent File-Specific Comments</h2>")
	assert.Contains(t, report, "This line is not in the diff")
	assert.Contains(t, report, "app.go:999")
	assert.Contains(t, report, "class=\"diff-file\"") // Now uses diff-file structure

	// Should contain summary section
	assert.Contains(t, report, "<h2>Review Summary</h2>")
	assert.Contains(t, report, "Testing Summary")

	// The inline comment should appear with the diff (test.go:13)
	assert.Contains(t, report, "This line is in the diff")

	// Should have correct comment counts
	assert.Contains(t, report, "Comment 1 of 3")
	assert.Contains(t, report, "Comment 2 of 3")
	assert.Contains(t, report, "Comment 3 of 3")
}

func TestGenerateReport_WithoutAdjacentComments(t *testing.T) {
	// When fileContents is nil (adjacent comments disabled), adjacent comments should not appear
	comments := []types.LineComment{
		{
			FilePath:    "app.go",
			LineNumber:  999, // Line not in diff
			CommentBody: "This line is not in the diff",
			Importance:  "CRITICAL",
		},
		{
			FilePath:    "test.go",
			LineNumber:  13, // Line in diff
			CommentBody: "This line is in the diff",
			Importance:  "MEDIUM",
		},
		{
			FilePath:    "SUMMARY",
			LineNumber:  0,
			CommentBody: "## Testing Summary\n\nOverall assessment",
		},
	}

	diff := `diff --git a/test.go b/test.go
index 123..456 100644
--- a/test.go
+++ b/test.go
@@ -10,6 +10,7 @@ func main() {
 	fmt.Println("Hello")
+	// New line at 13
 }`

	// Use GenerateReport (no file contents) to simulate adjacent comments disabled
	testTime := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	report, err := GenerateReport(comments, diff, 123, "https://github.com/test-owner/test-repo/pull/123", "Test PR body", "", "abc1234", "gemini-pro", 0, 0, 0, testTime)
	assert.NoError(t, err)

	// Without file contents the off-diff finding renders without code
	// context, but it renders: a finding is never dropped from the report.
	assert.Contains(t, report, "<h2>Adjacent File-Specific Comments</h2>")
	assert.Contains(t, report, "This line is not in the diff")

	// Should still contain summary section
	assert.Contains(t, report, "<h2>Review Summary</h2>")
	assert.Contains(t, report, "Testing Summary")
}

func TestGenerateReportWithContext_ShowsContextLines(t *testing.T) {
	// Comments that reference lines with file context available
	comments := []types.LineComment{
		{
			FilePath:    "app.go",
			LineNumber:  5, // Line not in diff but available in file content
			CommentBody: "This function needs error handling",
			Importance:  "CRITICAL",
		},
		{
			FilePath:    "SUMMARY",
			LineNumber:  0,
			CommentBody: "## Testing Summary\nOverall good",
			Importance:  "",
		},
	}

	// File contents with the referenced line
	fileContents := map[string]string{
		"app.go": `package main

import "fmt"

func hello() {
    fmt.Println("Hello, World!")
    return
}
`,
	}

	diff := `diff --git a/other.go b/other.go
index 123..456 100644
--- a/other.go  
+++ b/other.go
@@ -1,3 +1,4 @@ 
 package main
+// New line
 func main() {
 }`

	testTime := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	report, err := GenerateReportWithContext(comments, diff, 123, "https://github.com/test-owner/test-repo/pull/123", "Test PR body", "", "abc1234", "gemini-pro", 0, 0, 0, testTime, fileContents)
	assert.NoError(t, err)

	// Should contain adjacent section with context lines using diff-file structure
	assert.Contains(t, report, "<h2>Adjacent File-Specific Comments</h2>")
	assert.Contains(t, report, "app.go:5")
	assert.Contains(t, report, "class=\"diff-file\"") // Now uses diff-file structure
	assert.Contains(t, report, "target-line")         // The highlighted line
	assert.Contains(t, report, "func hello()")        // Context line content

	// Should contain summary section
	assert.Contains(t, report, "<h2>Review Summary</h2>")
	assert.Contains(t, report, "Testing Summary")
}

// TestGenerateReport_EscapesUntrustedHTML guards against HTML injection / stored
// XSS: code, diffs, PR descriptions, and AI comments come from untrusted sources
// and must never be rendered as live HTML in the review page.
func TestGenerateReport_EscapesUntrustedHTML(t *testing.T) {
	comments := []types.LineComment{
		{
			FilePath:    "evil.js",
			LineNumber:  2, // not in diff -> rendered via context lines
			CommentBody: "Look here: <img src=x onerror=COMMENT_XSS()>",
			Importance:  "CRITICAL",
		},
	}

	// Source code containing markup that must be escaped, not executed.
	fileContents := map[string]string{
		"evil.js": "function init() {\n  $('<script>CTX_XSS()</script>');\n  {% if user %}\n}\n",
	}

	diff := `diff --git a/other.go b/other.go
index 123..456 100644
--- a/other.go
+++ b/other.go
@@ -1,2 +1,3 @@
 package main
+// added
 func main() {}`

	prBody := "PR description with injected <script>BODY_XSS()</script> markup"
	prompt := "Review this code:\n<script type=\"text/javascript\">PROMPT_XSS()</script>\n$('#token_table')"

	testTime := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	report, err := GenerateReportWithContext(comments, diff, 1, "https://github.com/acme/example/pull/1", prBody, prompt, "abc1234", "gemini-pro", 0, 0, 0, testTime, fileContents)
	assert.NoError(t, err)

	// No injected payload may appear as a live element/handler anywhere.
	assert.NotContains(t, report, "<script>BODY_XSS()", "PR body script must be sanitized")
	assert.NotContains(t, report, "<script>CTX_XSS()", "context-line script must be escaped")
	assert.NotContains(t, report, "<script type=\"text/javascript\">PROMPT_XSS()", "prompt script must be escaped")
	assert.NotContains(t, report, "<img src=x onerror=", "comment-body event handler must be sanitized")

	// The prompt and context code must survive as escaped, readable text.
	assert.Contains(t, report, "&lt;script", "escaped markup should still be visible")

	// And the benign parts of the untrusted input must still render, so we'd
	// catch a regression where sanitizing accidentally strips everything.
	assert.Contains(t, report, "PR description with injected", "PR body text should still render")
	assert.Contains(t, report, "{% if user %}", "context-line code should still render (escaped)")
	assert.Contains(t, report, "Look here:", "comment body text should still render")
}

func TestRenderMarkdown(t *testing.T) {
	t.Run("strips dangerous HTML", func(t *testing.T) {
		out := string(renderMarkdown("hello <script>alert(1)</script> world"))
		assert.NotContains(t, out, "<script>")
		assert.NotContains(t, out, "alert(1)")
	})

	t.Run("strips event-handler attributes", func(t *testing.T) {
		out := string(renderMarkdown("![x](data:image/png;base64,abc) <img src=x onerror=alert(1)>"))
		assert.NotContains(t, out, "onerror")
	})

	t.Run("preserves real markdown formatting", func(t *testing.T) {
		out := string(renderMarkdown("This is **bold** and _italic_."))
		assert.Contains(t, out, "<strong>bold</strong>")
		assert.Contains(t, out, "<em>italic</em>")
	})

	t.Run("renders code spans with their angle brackets escaped, not executed", func(t *testing.T) {
		out := string(renderMarkdown("use the `<div>` element"))
		assert.Contains(t, out, "<code>&lt;div&gt;</code>")
		assert.NotContains(t, out, "<div>")
	})
}

func TestGenerateContextLines_EscapesContent(t *testing.T) {
	fileContents := map[string]string{
		"x.js": "line one\n<script>alert(1)</script>\nline three\n",
	}

	lines := GenerateContextLinesForTest("x.js", 2, fileContents)
	if assert.NotEmpty(t, lines) {
		var joined string
		for _, l := range lines {
			joined += string(l.Content)
		}
		assert.Contains(t, joined, "&lt;script&gt;", "source code must be HTML-escaped")
		assert.NotContains(t, joined, "<script>", "source code must not render as a live tag")
	}
}

func TestGenerateReport_WholeFileCommentRendersOnce(t *testing.T) {
	// A whole-file finding (LineNumber 0, e.g. a mechanical gate alert) must
	// render exactly once under the file header — not once per deleted line
	// (deleted lines carry NewLineNumber 0 and used to collide with the
	// comment's line-0 key).
	comments := []types.LineComment{
		{
			FilePath:    "worker.py",
			LineNumber:  0,
			CommentBody: "Mechanical alert — task payload contract changed.",
			Importance:  "MEDIUM",
		},
	}

	diff := `diff --git a/worker.py b/worker.py
index 123..456 100644
--- a/worker.py
+++ b/worker.py
@@ -10,6 +10,3 @@ def handle():
 	keep = 1
-	removed_one = 1
-	removed_two = 2
-	removed_three = 3
 	also_keep = 2`

	testTime := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	report, err := GenerateReport(comments, diff, 123, "https://github.com/test-owner/test-repo/pull/123", "Test PR body", "", "abc1234", "gemini-pro", 0, 0, 0, testTime)
	assert.NoError(t, err)

	occurrences := strings.Count(report, "<p>Mechanical alert — task payload contract changed.</p>")
	assert.Equal(t, 1, occurrences, "whole-file comment should render exactly once, got %d", occurrences)
}

func TestGenerateReport_WholeFileCommentOnOffDiffFile(t *testing.T) {
	// A whole-file comment (LineNumber 0) whose file is NOT in the diff takes
	// the adjacent-comments path, where generateContextLines rejects line 0
	// and returns no context lines. It must still appear in the output.
	comments := []types.LineComment{
		{
			FilePath:    "other.py",
			LineNumber:  0,
			CommentBody: "Whole-file note on a file outside the diff",
			Importance:  "MEDIUM",
		},
	}

	diff := `diff --git a/app.go b/app.go
index 123..456 100644
--- a/app.go
+++ b/app.go
@@ -12,6 +12,7 @@ func main() {
 	fmt.Println("Hello")
+	// New line
 }`

	fileContents := map[string]string{"other.py": "line one\nline two\n"}
	testTime := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	report, err := GenerateReportWithContext(comments, diff, 123, "https://github.com/test-owner/test-repo/pull/123", "Test PR body", "", "abc1234", "gemini-pro", 0, 0, 0, testTime, fileContents)
	assert.NoError(t, err)

	adjacentIdx := strings.Index(report, "adjacent-section")
	assert.Greater(t, adjacentIdx, -1)
	assert.Equal(t, 1, strings.Count(report[adjacentIdx:], "Whole-file note on a file outside the diff"),
		"off-diff whole-file comment should render exactly once in the adjacent section")
}

func TestGenerateReport_CommentPermalinkButtons(t *testing.T) {
	// Every rendered comment gets a copy-link button, whatever section it lands
	// in: summary, general, inline-with-diff, whole-file, and adjacent.
	comments := []types.LineComment{
		{FilePath: "app.go", LineNumber: 13, CommentBody: "Inline finding", Importance: "HIGH"},
		{FilePath: "app.go", LineNumber: 0, CommentBody: "Whole-file finding", Importance: "MEDIUM"},
		{FilePath: "GENERAL", LineNumber: 0, CommentBody: "General finding", Importance: "LOW"},
		{FilePath: "SUMMARY", LineNumber: 0, CommentBody: "Summary finding", Importance: ""},
		{FilePath: "other.go", LineNumber: 2, CommentBody: "Adjacent finding", Importance: "LOW"},
	}

	diff := `diff --git a/app.go b/app.go
index 123..456 100644
--- a/app.go
+++ b/app.go
@@ -12,6 +12,7 @@ func main() {
 	fmt.Println("Hello")
+	// New line
 }`

	fileContents := map[string]string{"other.go": "package other\n\nfunc f() {}\n"}
	testTime := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	report, err := GenerateReportWithContext(comments, diff, 123, "https://github.com/test-owner/test-repo/pull/123", "Test PR body", "", "abc1234", "gemini-pro", 0, 0, 0, testTime, fileContents)
	assert.NoError(t, err)

	assert.Equal(t, 5, strings.Count(report, `aria-label="Copy link to this comment"`),
		"each of the five rendered comments should carry a copy-link button")

	// The label lives in its own span so the load-time renumbering can rewrite
	// the text without wiping out the button next to it.
	assert.Equal(t, 5, strings.Count(report, `class="comment-counter-text"`))

	// Runtime wiring: ids to link to, and fragment handling to act on them.
	assert.Contains(t, report, "comment.id = `comment-${number}`")
	assert.Contains(t, report, "focusCommentFromHash")
	assert.Contains(t, report, "hashchange")
}

const layoutFixtureDiff = `diff --git a/app.go b/app.go
index 123..456 100644
--- a/app.go
+++ b/app.go
@@ -12,6 +12,7 @@ func main() {
 	fmt.Println("Hello")
+	// New line
 }`

func layoutFixtureInput() ReportInput {
	return ReportInput{
		Comments: []types.LineComment{
			{FilePath: "app.go", LineNumber: 13, Importance: "CRITICAL", Provenance: "agent",
				CommentBody: "Nil dereference when the config is missing. The handler reads cfg.Name before the nil check."},
			{FilePath: "app.go", LineNumber: 0, Importance: "MEDIUM", Provenance: "first-pass",
				CommentBody:     "**Retry loop never backs off.** Each attempt fires immediately.",
				FindingContract: &types.FindingContract{Headline: "Retry loop has no backoff"}},
			{FilePath: "GENERAL", LineNumber: 0, Importance: "LOW", Provenance: "carried",
				CommentBody: "Consider documenting the new env var in the deployment guide; operators will otherwise miss it and the service will start with defaults that silently disable the feature in production environments."},
			{FilePath: "worker.py", LineNumber: 0, Importance: "MEDIUM",
				CommentBody: mergeNote("mechanical") + "**Mechanical alert: task payload contract changed.** The worker signature gained a field."},
			{FilePath: "handlers.py", LineNumber: 0, Importance: "MEDIUM", Provenance: "required-check",
				CommentBody: "**Required check CHK-settings-ref-1 answered VIOLATED without an accompanying finding.** See handlers.py:42."},
			{FilePath: "SUMMARY", LineNumber: 0,
				CommentBody: "**Verdict: request changes.**\n\nThe change is small but the nil dereference is a real crash path.\n\n" +
					"Suggestions: add a regression test for the missing-config case and wire the backoff.\n\n" +
					"Coverage is otherwise adequate.\n\n---\n_Required checks (id | verdict | evidence)_\n" +
					"- CHK-portal-layer-1 | SAFE | evidence-ok\n- CHK-settings-ref-1 | VIOLATED | evidence-ok\n\n" +
					"---\n_Reconciliation: 2 earlier-pass finding(s) below were retained despite not being independently confirmed._"},
		},
		Diff:         layoutFixtureDiff,
		PRNumber:     42,
		PRURL:        "https://github.com/acme/example/pull/42",
		PRTitle:      "Add retry to the sync worker",
		PRBody:       "First paragraph of the description.\n\nSecond paragraph with more detail about the rollout.\n\nThird paragraph.",
		Prompt:       "review prompt",
		CommitSHA:    "abc1234def",
		ModelName:    "model-x",
		PromptTokens: 100, CandidateTokens: 20, TotalTokens: 120,
		GeneratedAt:  time.Date(2026, 1, 15, 14, 30, 0, 0, time.UTC),
		FileContents: map[string]string{"worker.py": "import os\n", "handlers.py": "def h():\n    pass\n"},
		Checks: []CheckRecord{
			{ID: "CHK-portal-layer-1", Source: "gate", Question: "Does the overlay declare a layer?", TargetFile: "web/Overlay.tsx",
				Verdict: "SAFE", Answer: "Layer prop is set.", EvidencePath: "web/Overlay.tsx", EvidenceResolved: true},
			{ID: "CHK-mem-1", Source: "memory", Question: "Was the payload skew regression re-checked?", TargetFile: "worker.py",
				Verdict: "UNANSWERED", Unresolved: true},
		},
	}
}

func renderLayoutFixture(t *testing.T, in ReportInput) string {
	t.Helper()
	report, err := GenerateReportFrom(in)
	assert.NoError(t, err)
	return report
}

func sectionIndices(t *testing.T, report string, markers ...string) []int {
	t.Helper()
	idx := make([]int, len(markers))
	for i, m := range markers {
		idx[i] = strings.Index(report, m)
		assert.Greater(t, idx[i], -1, "marker %q missing", m)
	}
	return idx
}

func TestGenerateReport_SectionOrder(t *testing.T) {
	t.Setenv("SURFACE_ALERTS", "")
	report := renderLayoutFixture(t, layoutFixtureInput())

	idx := sectionIndices(t, report,
		"<h1>Add retry to the sync worker</h1>",
		"<h2>PR Description</h2>",
		`class="verdict-block"`,
		"<h2>Suggestions</h2>",
		"<h2>Review Summary</h2>",
		"<h2>Findings</h2>",
		"<h2>General Comments</h2>",
		"<summary>Review details</summary>",
	)
	for i := 1; i < len(idx); i++ {
		assert.Greater(t, idx[i], idx[i-1], "section %d should follow section %d", i, i-1)
	}
}

func TestGenerateReport_TitleInHeaderAndPageTitle(t *testing.T) {
	report := renderLayoutFixture(t, layoutFixtureInput())
	assert.Contains(t, report, "<title>PRism review: Add retry to the sync worker</title>")
	assert.Contains(t, report, "<h1>Add retry to the sync worker</h1>")
	assert.Contains(t, report, `<a href="https://github.com/acme/example/pull/42" target="_blank">#42</a>`)
	assert.Contains(t, report, ">abc1234</a>")

	detailsIdx := strings.Index(report, "<summary>Review details</summary>")
	assert.Greater(t, detailsIdx, -1)
	assert.NotContains(t, report[:detailsIdx], "Token", "token usage leaves the header")
	assert.NotContains(t, report[:detailsIdx], "model-x", "model name is not visible above review details")
	assert.Contains(t, report[detailsIdx:], "model-x")
	assert.Contains(t, report[detailsIdx:], "120")
}

func TestGenerateReport_TitleFallbackWhenEmpty(t *testing.T) {
	in := layoutFixtureInput()
	in.PRTitle = ""
	report := renderLayoutFixture(t, in)
	assert.Contains(t, report, "<title>PRism review: #42</title>")
	assert.Contains(t, report, "<h1>PR #42</h1>")
}

func TestGenerateReport_DescriptionFoldedWhenLong(t *testing.T) {
	report := renderLayoutFixture(t, layoutFixtureInput())
	assert.Contains(t, report, `<details class="pr-description-fold">`)
	assert.Contains(t, report, "<summary>Read full PR description</summary>")
	previewIdx := strings.Index(report, `class="pr-description-preview"`)
	foldIdx := strings.Index(report, `<details class="pr-description-fold">`)
	assert.Greater(t, previewIdx, -1)
	assert.Greater(t, foldIdx, previewIdx)
	assert.Contains(t, report[previewIdx:foldIdx], "First paragraph of the description.")
	assert.NotContains(t, report[previewIdx:foldIdx], "Second paragraph")
	assert.Contains(t, report[foldIdx:], "Third paragraph.")
}

func TestGenerateReport_DescriptionFoldedWhenOneLongParagraph(t *testing.T) {
	in := layoutFixtureInput()
	in.PRBody = strings.Repeat("word ", 120) + "END"
	report := renderLayoutFixture(t, in)
	assert.Contains(t, report, "<summary>Read full PR description</summary>")
	previewIdx := strings.Index(report, `class="pr-description-preview"`)
	foldIdx := strings.Index(report, `<details class="pr-description-fold">`)
	assert.NotContains(t, report[previewIdx:foldIdx], "END")
	assert.Contains(t, report[foldIdx:], "END")
}

func TestGenerateReport_DescriptionPlainWhenShort(t *testing.T) {
	in := layoutFixtureInput()
	in.PRBody = "Just a short description."
	report := renderLayoutFixture(t, in)
	assert.NotContains(t, report, "Read full PR description")
	assert.NotContains(t, report, `class="pr-description-fold"`)
	assert.Contains(t, report, "Just a short description.")
}

func TestGenerateReport_VerdictExtractedWithCounts(t *testing.T) {
	report := renderLayoutFixture(t, layoutFixtureInput())
	start := strings.Index(report, `class="verdict-block"`)
	end := strings.Index(report, "<h2>Suggestions</h2>")
	assert.Greater(t, start, -1)
	assert.Greater(t, end, start)
	block := report[start:end]
	assert.Contains(t, block, "Verdict: request changes.")
	assert.NotContains(t, block, "**")
	assert.Contains(t, block, "2 confirmed")
	assert.Contains(t, block, "2 unverified")
	assert.Contains(t, block, "1 mechanical")

	summaryStart := strings.Index(report, "<h2>Review Summary</h2>")
	summaryEnd := strings.Index(report, "<h2>Findings</h2>")
	assert.NotContains(t, report[summaryStart:summaryEnd], "Verdict:")
}

func TestGenerateReport_ZeroCountsAreHidden(t *testing.T) {
	in := layoutFixtureInput()
	report := renderLayoutFixture(t, in)
	assert.NotContains(t, report, "0 mechanical")
	assert.Contains(t, report, " confirmed</span>")
}

func TestGenerateReport_VerdictUnavailableFallback(t *testing.T) {
	in := layoutFixtureInput()
	in.Comments[len(in.Comments)-1].CommentBody = "Just prose without a decision line."
	report := renderLayoutFixture(t, in)
	assert.Contains(t, report, "Verdict: unavailable")
}

func TestGenerateReport_SuggestionsHoistedOutOfSummary(t *testing.T) {
	report := renderLayoutFixture(t, layoutFixtureInput())
	sugStart := strings.Index(report, "<h2>Suggestions</h2>")
	summaryStart := strings.Index(report, "<h2>Review Summary</h2>")
	summaryEnd := strings.Index(report, "<h2>Findings</h2>")
	assert.Greater(t, sugStart, -1)
	assert.Contains(t, report[sugStart:summaryStart], "add a regression test for the missing-config case")
	assert.NotContains(t, report[summaryStart:summaryEnd], "add a regression test")
	assert.Contains(t, report[summaryStart:summaryEnd], "nil dereference is a real crash path")
	assert.Contains(t, report[summaryStart:summaryEnd], "Coverage is otherwise adequate.")
	assert.NotContains(t, report[sugStart:summaryStart], "Suggestions:", "the heading already says it")
}

func TestStripSuggestionsLabel(t *testing.T) {
	cases := map[string]string{
		"Suggestions: add a test.":                        "add a test.",
		"**Suggestions:** add a test.":                    "add a test.",
		"**Suggestion: add a test.**":                     "**add a test.**",
		"**Suggestions:** add a test, and **bold** stays": "add a test, and **bold** stays",
		"- suggestions: keep the dash body":               "keep the dash body",
	}
	for in, want := range cases {
		if got := stripSuggestionsLabel(in); got != want {
			t.Errorf("stripSuggestionsLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenerateReport_NoSuggestionsBlockWhenAbsent(t *testing.T) {
	in := layoutFixtureInput()
	in.Comments[len(in.Comments)-1].CommentBody = "Verdict: approve.\n\nAll good."
	report := renderLayoutFixture(t, in)
	assert.NotContains(t, report, "<h2>Suggestions</h2>")
}

func TestGenerateReport_LegacySummaryInternalsStripped(t *testing.T) {
	report := renderLayoutFixture(t, layoutFixtureInput())
	summaryStart := strings.Index(report, "<h2>Review Summary</h2>")
	summaryEnd := strings.Index(report, "<h2>Findings</h2>")
	summary := report[summaryStart:summaryEnd]
	assert.NotContains(t, summary, "Required checks (id")
	assert.NotContains(t, summary, "CHK-portal-layer-1")
	assert.NotContains(t, summary, "Reconciliation:")
	assert.NotContains(t, summary, "<hr")
	assert.Contains(t, summary, "Coverage is otherwise adequate.")
}

func TestGenerateReport_FindingsIndexGroupsByProvenance(t *testing.T) {
	report := renderLayoutFixture(t, layoutFixtureInput())
	start := strings.Index(report, "<h2>Findings</h2>")
	end := strings.Index(report, "<h2>General Comments</h2>")
	assert.Greater(t, start, -1)
	index := report[start:end]

	idx := sectionIndices(t, index, "<h3>Confirmed</h3>", "<h3>Needs verification</h3>", "<h3>Mechanical signals</h3>")
	assert.Less(t, idx[0], idx[1])
	assert.Less(t, idx[1], idx[2])

	confirmed := index[idx[0]:idx[1]]
	assert.Contains(t, confirmed, `href="#finding-1"`)
	assert.Contains(t, confirmed, "app.go:13")
	assert.Contains(t, confirmed, "Nil dereference when the config is missing.")
	assert.NotContains(t, confirmed, "The handler reads")
	assert.Contains(t, confirmed, `href="#finding-5"`)
	assert.NotContains(t, confirmed, "UNVERIFIED")

	needs := index[idx[1]:idx[2]]
	assert.Contains(t, needs, `href="#finding-2"`)
	assert.Contains(t, needs, "Retry loop has no backoff")
	assert.Contains(t, needs, "FIRST PASS · UNVERIFIED")
	assert.Contains(t, needs, `href="#finding-3"`)
	assert.Contains(t, needs, "CARRIED · UNVERIFIED")
	assert.Contains(t, needs, "GENERAL")
	assert.NotContains(t, needs, "production environments", "long title truncated on a word boundary")
	assert.Contains(t, needs, "...")

	mech := index[idx[2]:]
	assert.Contains(t, mech, `href="#finding-4"`)
	assert.Contains(t, mech, ">MECHANICAL<")
	assert.Contains(t, mech, "Mechanical alert: task payload contract changed.")

	assert.Contains(t, index, `class="sev-pill sev-critical"`)
	assert.Contains(t, index, `class="sev-pill sev-medium"`)
	assert.Contains(t, index, `class="sev-pill sev-low"`)

	for _, id := range []string{"finding-1", "finding-2", "finding-3", "finding-4", "finding-5"} {
		assert.Equal(t, 1, strings.Count(report, `id="`+id+`"`), "anchor %s should exist exactly once", id)
	}
}

func TestGenerateReport_FindingsIndexOmitsEmptyGroups(t *testing.T) {
	in := layoutFixtureInput()
	in.Comments = []types.LineComment{in.Comments[0], in.Comments[5]}
	report := renderLayoutFixture(t, in)
	assert.Contains(t, report, "<h3>Confirmed</h3>")
	assert.NotContains(t, report, "<h3>Needs verification</h3>")
	assert.NotContains(t, report, "<h3>Mechanical signals</h3>")
}

func TestGenerateReport_DetailedCommentCarriesStatusPill(t *testing.T) {
	report := renderLayoutFixture(t, layoutFixtureInput())
	markup := report[:strings.Index(report, "const GEMINI_API_KEY")]
	assert.Equal(t, 2, strings.Count(markup, "FIRST PASS · UNVERIFIED"), "index row plus detailed comment")
	assert.Equal(t, 2, strings.Count(markup, "CARRIED · UNVERIFIED"))
	assert.Equal(t, 2, strings.Count(markup, ">MECHANICAL<"))
}

func TestGenerateReport_LegacyProvenancePrefaceStripped(t *testing.T) {
	report := renderLayoutFixture(t, layoutFixtureInput())
	assert.NotContains(t, report, "retained by reconciliation")
	assert.Contains(t, report, "The worker signature gained a field.")
}

func TestGenerateReport_ReviewDetailsRendersChecks(t *testing.T) {
	report := renderLayoutFixture(t, layoutFixtureInput())
	detailsIdx := strings.Index(report, "<summary>Review details</summary>")
	assert.Greater(t, detailsIdx, -1)
	details := report[detailsIdx:]

	assert.Contains(t, details, "<h3>Required checks</h3>")
	safeRow := details[strings.Index(details, "CHK-portal-layer-1"):]
	safeRow = safeRow[:strings.Index(safeRow, "</tr>")]
	assert.Contains(t, safeRow, "gate")
	assert.Contains(t, safeRow, "SAFE")
	assert.Contains(t, safeRow, "reference resolved")
	assert.Contains(t, safeRow, "Does the overlay declare a layer?")
	assert.Contains(t, safeRow, "Layer prop is set.")
	assert.NotContains(t, safeRow, "unresolved")

	memRow := details[strings.Index(details, "CHK-mem-1"):]
	memRow = memRow[:strings.Index(memRow, "</tr>")]
	assert.Contains(t, memRow, "memory")
	assert.Contains(t, memRow, "UNANSWERED")
	assert.Contains(t, memRow, "no reference")
	assert.Contains(t, memRow, "unresolved")

	assert.NotContains(t, details, "CHK-settings-ref-1 | VIOLATED", "legacy SUMMARY ledger is not the source")
}

func TestGenerateReport_ReviewDetailsWithoutChecks(t *testing.T) {
	in := layoutFixtureInput()
	in.Checks = nil
	report := renderLayoutFixture(t, in)
	assert.Contains(t, report, "<summary>Review details</summary>")
	assert.NotContains(t, report, "<h3>Required checks</h3>")
}

func TestGenerateReport_UnplaceableFindingStillRendersAndIsIndexed(t *testing.T) {
	in := layoutFixtureInput()
	in.Diff = ""
	in.FileContents = nil
	in.Comments = append(in.Comments, types.LineComment{FilePath: "docs/untouched.md", LineNumber: 12, Importance: "LOW", Provenance: "agent", CommentBody: "The runbook still names the old flag."})
	report := renderLayoutFixture(t, in)
	idx := strings.Index(report, "<h2>Findings</h2>")
	assert.Greater(t, idx, -1, "findings index must exist even with no diff")
	assert.Contains(t, report[idx:], "docs/untouched.md:12")
	assert.Contains(t, report, "The runbook still names the old flag.", "a finding with no diff or file context must still render its body")
	assert.Equal(t, 1, strings.Count(report, `href="#finding-`+fmt.Sprint(len(in.Comments))+`"`), "the index links to the rendered detail")
}

func TestFindingGroup_UnansweredMemoryCheckNeedsVerification(t *testing.T) {
	answered := types.LineComment{Provenance: "required-check", CommentBody: "**Required check CHK-x answered VIOLATED without an accompanying finding.** details"}
	unanswered := types.LineComment{Provenance: "required-check", CommentBody: "Bug-memory alert.\n\n_Required check CHK-mem-1 was not answered with evidence, treat as unresolved risk._"}
	if g, _, _ := findingGroupFor(answered); g != groupConfirmed {
		t.Errorf("violated synthesis = %q, want confirmed", g)
	}
	if g, pill, _ := findingGroupFor(unanswered); g != groupNeedsCheck || pill != "UNANSWERED CHECK" {
		t.Errorf("unanswered memory check = %q/%q, want needs verification with an UNANSWERED CHECK pill", g, pill)
	}
}

func TestDecomposeSummary_OnlyTheGeneratedReconciliationFooterIsStripped(t *testing.T) {
	parts := decomposeSummary("Verdict: approve.\n\nReconciliation: the DB and API representations now agree.\n\n_Reconciliation: 1 earlier-pass finding(s) below were retained despite not being independently confirmed._")
	if !strings.Contains(parts.Prose, "DB and API representations now agree") {
		t.Errorf("ordinary prose starting with Reconciliation must survive:\n%s", parts.Prose)
	}
	if strings.Contains(parts.Prose, "earlier-pass finding") {
		t.Errorf("the generated footer must be stripped:\n%s", parts.Prose)
	}
}

func TestDecomposeSummary_VerdictMustStartALine(t *testing.T) {
	parts := decomposeSummary("The endpoint returns verdict: pending while the job runs.\n\n**Verdict: request changes.**\n\nMore prose.")
	if parts.Verdict != "Verdict: request changes." {
		t.Errorf("verdict = %q, want the line that starts with it", parts.Verdict)
	}
	if !strings.Contains(parts.Prose, "verdict: pending") {
		t.Errorf("an inline mention must stay in the prose:\n%s", parts.Prose)
	}
}

func TestFindingTitle_KeepsAngleBracketText(t *testing.T) {
	c := types.LineComment{FilePath: "a.ts", CommentBody: "`<Tooltip>` lacks a layer and Map<string, int> is fine."}
	if got := findingTitle(c); !strings.Contains(got, "<Tooltip> lacks a layer") || !strings.Contains(got, "Map<string, int>") {
		t.Errorf("title = %q, angle-bracket text must survive (the template escapes it)", got)
	}
}

func TestGenerateReport_OnlyTheFirstSummaryIsDecomposed(t *testing.T) {
	in := layoutFixtureInput()
	in.Comments = append(in.Comments, types.LineComment{FilePath: "SUMMARY", LineNumber: 0, Importance: "LOW",
		CommentBody: "Verdict: request changes.\n\nSuggestions: rename the flag.\n\nSecond summary prose."})
	report := renderLayoutFixture(t, in)
	summaryStart := strings.Index(report, "<h2>Review Summary</h2>")
	summaryEnd := strings.Index(report, "<h2>Findings</h2>")
	block := report[summaryStart:summaryEnd]
	if !strings.Contains(block, "Verdict: request changes.") || !strings.Contains(block, "rename the flag") {
		t.Errorf("a later SUMMARY keeps its verdict and suggestions in the prose:\n%s", block)
	}
	if strings.Count(report, "<h2>Suggestions</h2>") != 1 {
		t.Errorf("only the first summary feeds the hoisted blocks")
	}
}

func TestVerdictClass_KeysOnTheDecisionWord(t *testing.T) {
	cases := map[string]string{
		"Verdict: approve with suggestions.": "approve",
		"Verdict: Approve.":                  "approve",
		"Verdict: request changes.":          "changes",
		"Verdict: do not approve.":           "neutral",
		"Verdict: not approved, see below.":  "neutral",
		"Verdict: unavailable":               "neutral",
	}
	for in, want := range cases {
		if got := verdictClass(in); got != want {
			t.Errorf("verdictClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenerateReport_CommentHeadersShowSeverity(t *testing.T) {
	comments := []types.LineComment{
		{FilePath: "app.go", LineNumber: 13, CommentBody: "Inline finding", Importance: "CRITICAL"},
		{FilePath: "app.go", LineNumber: 0, CommentBody: "Whole-file finding", Importance: "medium"},
		{FilePath: "GENERAL", LineNumber: 0, CommentBody: "General finding", Importance: "LOW"},
		{FilePath: "SUMMARY", LineNumber: 0, CommentBody: "Summary finding", Importance: "CRITICAL"},
		{FilePath: "other.go", LineNumber: 2, CommentBody: "Adjacent finding", Importance: "LOW"},
		{FilePath: "other.go", LineNumber: 3, CommentBody: "Carried note without a severity", Importance: ""},
	}
	diff := `diff --git a/app.go b/app.go
index 123..456 100644
--- a/app.go
+++ b/app.go
@@ -12,6 +12,7 @@ func main() {
 	fmt.Println("Hello")
+	// New line
 }`
	fileContents := map[string]string{"other.go": "package other\n\nfunc f() {}\n"}
	report, err := GenerateReportWithContext(comments, diff, 123, "https://github.com/test-owner/test-repo/pull/123", "", "", "abc1234", "gemini-pro", 0, 0, 0, time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC), fileContents)
	assert.NoError(t, err)

	pills := map[string]int{}
	for _, counter := range strings.Split(report, `class="comment-counter"`)[1:] {
		header := counter[:strings.Index(counter, "comment-link-btn")]
		if strings.Contains(header, "Comment 4 of 6") {
			assert.NotContains(t, header, "sev-pill", "the summary carries no severity even when one is stored")
			continue
		}
		assert.Contains(t, header, "sev-pill", "every finding header names its severity: %s", header)
		for _, sev := range []string{"critical", "medium", "low"} {
			pills[sev] += strings.Count(header, `<span class="sev-pill sev-`+sev+`">`+strings.ToUpper(sev)+`</span>`)
		}
		pills["note"] += strings.Count(header, `<span class="sev-pill sev-note">NOTE</span>`)
	}
	assert.Equal(t, map[string]int{"critical": 1, "medium": 1, "low": 2, "note": 1}, pills, "severity is upper-cased whatever the sidecar stored; none reads NOTE like the index")
	assert.NotContains(t, report, `>medium</span>`, "the index label matches the header's casing")
}

func TestCommentView_SeverityExclusionsAndClassWhitelist(t *testing.T) {
	for _, fp := range []string{"SUMMARY", "CHECK"} {
		v := CommentView{LineComment: types.LineComment{FilePath: fp, Importance: "CRITICAL"}}
		assert.False(t, v.ShowsSeverity(), fp)
		assert.Empty(t, v.SeverityLabel(), fp)
	}
	v := CommentView{LineComment: types.LineComment{FilePath: "a.go", Importance: "LOW hidden"}}
	assert.Equal(t, "LOW HIDDEN", v.SeverityLabel())
	assert.Equal(t, "note", v.SeverityClass(), "unknown severities never reach the class attribute")
	assert.Equal(t, "critical", severityClass("High"))
}
