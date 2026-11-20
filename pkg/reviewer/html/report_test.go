package html

import (
	"fmt"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"pr-review-server/pkg/reviewer/types"
)

func TestParseDiff(t *testing.T) {
	cases := []struct {
		name string
		diff string
		want []*DiffFile
	}{
		{
			name: "two files",
			diff: `diff --git a/file1.txt b/file1.txt
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

`,
			want: []*DiffFile{
				{Path: "file1.txt", Lines: []Line{
					{Type: "added", Content: "Hello World", NewLineNumber: 1},
				}},
				{Path: "file2.go", Lines: []Line{
					{Type: "context", Content: "package main", OldLineNumber: 1, NewLineNumber: 1},
					{Type: "context", Content: "", OldLineNumber: 2, NewLineNumber: 2},
					{Type: "deleted", Content: "func main() {}", OldLineNumber: 3},
					{Type: "added", Content: "func main() { /* hello */ }", NewLineNumber: 3},
					{Type: "context", Content: "", OldLineNumber: 4, NewLineNumber: 4},
				}},
			},
		},
		{
			name: "header-like lines inside a hunk are content",
			diff: `diff --git a/notes.sql b/notes.sql
index 1111111..2222222 100644
--- a/notes.sql
+++ b/notes.sql
@@ -1,4 +1,4 @@
 select 1;
--- old comment
---- a/looks/like/a/header
+++ b/looks/like/a/header
+++counter;
 index of things
 select 2;
`,
			want: []*DiffFile{
				{Path: "notes.sql", Lines: []Line{
					{Type: "context", Content: "select 1;", OldLineNumber: 1, NewLineNumber: 1},
					{Type: "deleted", Content: "-- old comment", OldLineNumber: 2},
					{Type: "deleted", Content: "--- a/looks/like/a/header", OldLineNumber: 3},
					{Type: "added", Content: "++ b/looks/like/a/header", NewLineNumber: 2},
					{Type: "added", Content: "++counter;", NewLineNumber: 3},
					{Type: "context", Content: "index of things", OldLineNumber: 4, NewLineNumber: 4},
					{Type: "context", Content: "select 2;", OldLineNumber: 5, NewLineNumber: 5},
				}},
			},
		},
		{
			name: "no-newline marker is not a line",
			diff: `diff --git a/a.txt b/a.txt
index 1111111..2222222 100644
--- a/a.txt
+++ b/a.txt
@@ -1,2 +1,3 @@
 one
-two
\ No newline at end of file
+two
+three
\ No newline at end of file
`,
			want: []*DiffFile{
				{Path: "a.txt", Lines: []Line{
					{Type: "context", Content: "one", OldLineNumber: 1, NewLineNumber: 1},
					{Type: "deleted", Content: "two", OldLineNumber: 2},
					{Type: "added", Content: "two", NewLineNumber: 2},
					{Type: "added", Content: "three", NewLineNumber: 3},
				}},
			},
		},
		{
			name: "pre-hunk headers are skipped and counters reset per file",
			diff: `diff --git a/first.txt b/first.txt
index 1111111..2222222 100644
--- a/first.txt
+++ b/first.txt
@@ -40,2 +40,2 @@
 keep
-old
+new
diff --git a/run.sh b/run.sh
old mode 100644
new mode 100755
diff --git a/fresh.txt b/fresh.txt
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/fresh.txt
@@ -0,0 +1 @@
+hello
`,
			want: []*DiffFile{
				{Path: "first.txt", Lines: []Line{
					{Type: "gap", Content: "39 lines not shown"},
					{Type: "context", Content: "keep", OldLineNumber: 40, NewLineNumber: 40},
					{Type: "deleted", Content: "old", OldLineNumber: 41},
					{Type: "added", Content: "new", NewLineNumber: 41},
				}},
				{Path: "run.sh"},
				{Path: "fresh.txt", Lines: []Line{
					{Type: "added", Content: "hello", NewLineNumber: 1},
				}},
			},
		},
		{
			name: "deleted file takes the old path",
			diff: `diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index 1111111..0000000
--- a/gone.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-one
-two
`,
			want: []*DiffFile{
				{Path: "gone.txt", Lines: []Line{
					{Type: "deleted", Content: "one", OldLineNumber: 1},
					{Type: "deleted", Content: "two", OldLineNumber: 2},
				}},
			},
		},
		{
			name: "renamed file takes the new path",
			diff: `diff --git a/old/name.go b/new/name.go
similarity index 90%
rename from old/name.go
rename to new/name.go
index 1111111..2222222 100644
--- a/old/name.go
+++ b/new/name.go
@@ -1,2 +1,2 @@
 package name
-var a = 1
+var a = 2
`,
			want: []*DiffFile{
				{Path: "new/name.go", Lines: []Line{
					{Type: "context", Content: "package name", OldLineNumber: 1, NewLineNumber: 1},
					{Type: "deleted", Content: "var a = 1", OldLineNumber: 2},
					{Type: "added", Content: "var a = 2", NewLineNumber: 2},
				}},
			},
		},
		{
			name: "paths with spaces, tabs after the path and quoting",
			diff: "diff --git a/my docs/read me.md b/my docs/read me.md\n" +
				"index 1111111..2222222 100644\n" +
				"--- a/my docs/read me.md\t\n" +
				"+++ b/my docs/read me.md\t\n" +
				"@@ -1 +1 @@\n" +
				"-a\n" +
				"+b\n" +
				"diff --git \"a/caf\\303\\251.txt\" \"b/caf\\303\\251.txt\"\n" +
				"index 1111111..2222222 100644\n" +
				"--- \"a/caf\\303\\251.txt\"\n" +
				"+++ \"b/caf\\303\\251.txt\"\n" +
				"@@ -1 +1 @@\n" +
				"-a\n" +
				"+b\n",
			want: []*DiffFile{
				{Path: "my docs/read me.md", Lines: []Line{
					{Type: "deleted", Content: "a", OldLineNumber: 1},
					{Type: "added", Content: "b", NewLineNumber: 1},
				}},
				{Path: "café.txt", Lines: []Line{
					{Type: "deleted", Content: "a", OldLineNumber: 1},
					{Type: "added", Content: "b", NewLineNumber: 1},
				}},
			},
		},
		{
			name: "files without lines are kept, first and last",
			diff: `diff --git a/art/logo v2.png b/art/logo v2.png
index 1111111..2222222 100644
Binary files a/art/logo v2.png and b/art/logo v2.png differ
diff --git a/mid.txt b/mid.txt
index 1111111..2222222 100644
--- a/mid.txt
+++ b/mid.txt
@@ -1 +1 @@
-a
+b
diff --git a/old name.txt b/new name.txt
similarity index 100%
rename from old name.txt
rename to new name.txt
`,
			want: []*DiffFile{
				{Path: "art/logo v2.png"},
				{Path: "mid.txt", Lines: []Line{
					{Type: "deleted", Content: "a", OldLineNumber: 1},
					{Type: "added", Content: "b", NewLineNumber: 1},
				}},
				{Path: "new name.txt"},
			},
		},
		{
			name: "two hunks with a one-line gap",
			diff: `diff --git a/g.txt b/g.txt
index 1111111..2222222 100644
--- a/g.txt
+++ b/g.txt
@@ -1,2 +1,3 @@
 one
+one and a half
 two
@@ -4,2 +5,2 @@ section
 four
-five
+FIVE
`,
			want: []*DiffFile{
				{Path: "g.txt", Lines: []Line{
					{Type: "context", Content: "one", OldLineNumber: 1, NewLineNumber: 1},
					{Type: "added", Content: "one and a half", NewLineNumber: 2},
					{Type: "context", Content: "two", OldLineNumber: 2, NewLineNumber: 3},
					{Type: "gap", Content: "1 line not shown"},
					{Type: "context", Content: "four", OldLineNumber: 4, NewLineNumber: 5},
					{Type: "deleted", Content: "five", OldLineNumber: 5},
					{Type: "added", Content: "FIVE", NewLineNumber: 6},
				}},
			},
		},
		{
			name: "adjacent hunks get no gap row",
			diff: `diff --git a/h.txt b/h.txt
--- a/h.txt
+++ b/h.txt
@@ -1 +1 @@
-a
+b
@@ -2 +2 @@
-c
+d
`,
			want: []*DiffFile{
				{Path: "h.txt", Lines: []Line{
					{Type: "deleted", Content: "a", OldLineNumber: 1},
					{Type: "added", Content: "b", NewLineNumber: 1},
					{Type: "deleted", Content: "c", OldLineNumber: 2},
					{Type: "added", Content: "d", NewLineNumber: 2},
				}},
			},
		},
		{
			name: "pure deletion hunk counts the lines before it",
			diff: `diff --git a/d.txt b/d.txt
--- a/d.txt
+++ b/d.txt
@@ -5,2 +4,0 @@
-five
-six
@@ -9 +7 @@
-nine
+NINE
`,
			want: []*DiffFile{
				{Path: "d.txt", Lines: []Line{
					{Type: "gap", Content: "4 lines not shown"},
					{Type: "deleted", Content: "five", OldLineNumber: 5},
					{Type: "deleted", Content: "six", OldLineNumber: 6},
					{Type: "gap", Content: "2 lines not shown"},
					{Type: "deleted", Content: "nine", OldLineNumber: 9},
					{Type: "added", Content: "NINE", NewLineNumber: 7},
				}},
			},
		},
		{
			name: "malformed hunk header drops its lines without panicking",
			diff: `diff --git a/m.txt b/m.txt
--- a/m.txt
+++ b/m.txt
@@
+lost
@@ -x +y @@
+lost too
@@ -3 +3 @@
-c
+C
`,
			want: []*DiffFile{
				{Path: "m.txt", Lines: []Line{
					{Type: "gap", Content: "2 lines not shown"},
					{Type: "deleted", Content: "c", OldLineNumber: 3},
					{Type: "added", Content: "C", NewLineNumber: 3},
				}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ParseDiff(tc.diff))
		})
	}
}

func TestParseDiff_EmptyInput(t *testing.T) {
	assert.Empty(t, ParseDiff(""))
	assert.Empty(t, ParseDiff("not a diff\n@@ -1 +1 @@\n+orphan\n"))
}

func TestGenerateReport_HunkGapRendersAsSeparatorNotALine(t *testing.T) {
	report, err := GenerateReportFrom(ReportInput{
		Comments: []types.LineComment{{FilePath: "g.txt", LineNumber: 41, CommentBody: "Check this value."}},
		Diff: `diff --git a/g.txt b/g.txt
--- a/g.txt
+++ b/g.txt
@@ -40,2 +40,2 @@
 keep
-old
+new
`,
	})
	assert.NoError(t, err)
	assert.Contains(t, report, `<div class="hunk-gap-text">39 lines not shown</div>`)
	assert.NotContains(t, report, `<div class="line gap">`)
	assert.NotContains(t, report, "<h2>Adjacent File-Specific Comments</h2>")
	assert.Equal(t, 1, strings.Count(report, `<div class="comment-row">`))
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
	report, err := GenerateReportFrom(ReportInput{Comments: comments, Diff: diff, PRNumber: 123, PRURL: "https://github.com/test-owner/test-repo/pull/123", PRBody: "Test PR body", CommitSHA: "abc1234", ModelName: "gemini-pro", GeneratedAt: testTime})
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
	report, err := GenerateReportFrom(ReportInput{Comments: comments, Diff: diff, PRNumber: 123, PRURL: "https://github.com/test-owner/test-repo/pull/123", PRBody: "Test PR body", CommitSHA: "abc1234", ModelName: "gemini-pro", GeneratedAt: testTime, FileContents: fileContents})
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
	report, err := GenerateReportFrom(ReportInput{Comments: comments, Diff: diff, PRNumber: 123, PRURL: "https://github.com/test-owner/test-repo/pull/123", PRBody: "Test PR body", CommitSHA: "abc1234", ModelName: "gemini-pro", GeneratedAt: testTime})
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
	report, err := GenerateReportFrom(ReportInput{Comments: comments, Diff: diff, PRNumber: 123, PRURL: "https://github.com/test-owner/test-repo/pull/123", PRBody: "Test PR body", CommitSHA: "abc1234", ModelName: "gemini-pro", GeneratedAt: testTime, FileContents: fileContents})
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
	report, err := GenerateReportFrom(ReportInput{Comments: comments, Diff: diff, PRNumber: 1, PRURL: "https://github.com/acme/example/pull/1", PRBody: prBody, Prompt: prompt, CommitSHA: "abc1234", ModelName: "gemini-pro", GeneratedAt: testTime, FileContents: fileContents})
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

	t.Run("leaves punctuation as written", func(t *testing.T) {
		for _, text := range []string{
			"43/44 checks passed",
			"pass --model to override",
			"wait... then 1/2 of it -- or more",
		} {
			assert.Contains(t, string(renderMarkdown(text)), text)
		}
		out := string(renderMarkdown(`it's the "right" flag`))
		assert.Contains(t, out, "it&#39;s the &#34;right&#34; flag")
	})

	t.Run("keeps dollar signs literal", func(t *testing.T) {
		out := string(renderMarkdown("set $A and $B before $$C$$ runs"))
		assert.Contains(t, out, "set $A and $B before $$C$$ runs")
	})

	t.Run("keeps tables, fenced code, autolinks and strikethrough", func(t *testing.T) {
		out := string(renderMarkdown("| a | b |\n|---|---|\n| 1 | 2 |\n\n```go\nx := 1\n```\n\n~~gone~~ https://example.com/x"))
		assert.Contains(t, out, "<table>")
		assert.Contains(t, out, "<pre><code")
		assert.Contains(t, out, "<del>gone</del>")
		assert.Contains(t, out, `href="https://example.com/x"`)
	})

	t.Run("renders code spans with their angle brackets escaped, not executed", func(t *testing.T) {
		out := string(renderMarkdown("use the `<div>` element"))
		assert.Contains(t, out, "<code>&lt;div&gt;</code>")
		assert.NotContains(t, out, "<div>")
	})
}

func TestRenderMarkdownInline(t *testing.T) {
	assert.Equal(t, "Guard <code>cfg.Name</code> first", string(renderMarkdownInline("Guard `cfg.Name` first")))
	assert.Equal(t, "plain", string(renderMarkdownInline("plain")))
	assert.NotContains(t, string(renderMarkdownInline("a <script>alert(1)</script> b")), "<script>")

	twoParagraphs := string(renderMarkdownInline("one\n\ntwo"))
	assert.Contains(t, twoParagraphs, "<p>one</p>")
	assert.Contains(t, twoParagraphs, "<p>two</p>")
}

func TestGenerateReport_RendersMarkdownInTitlesAndReasons(t *testing.T) {
	report, err := GenerateReportFrom(ReportInput{
		Comments: []types.LineComment{
			{FilePath: "app.go", LineNumber: 13, Importance: "MEDIUM", CommentBody: "Original wording.",
				Display: &types.Display{Title: "`Save` drops 43/44 errors", Body: "A failed Save returns nil."},
				Verification: &types.Verification{Applied: true, Status: types.VerifyKeep,
					Reason: "Confirmed in `store.go`; pass --strict to see it."}},
			{FilePath: "app.go", LineNumber: 12, Importance: "LOW", Provenance: "first-pass", State: "rejected", Inactive: true,
				CommentBody: "Rejected claim text.",
				Assessment:  &types.Disposition{State: "rejected", Reason: "`cfg` is checked by the caller."}},
		},
		Diff: layoutFixtureDiff,
	})
	assert.NoError(t, err)
	assert.Contains(t, report, `<div class="finding-display-title"><code>Save</code> drops 43/44 errors</div>`)
	assert.Contains(t, report, "Confirmed in <code>store.go</code>; pass --strict to see it.</div>")
	assert.Contains(t, report, "Rejected because: <code>cfg</code> is checked by the caller.</div>")
}

func TestGenerateContextLines_EscapesContent(t *testing.T) {
	fileContents := map[string]string{
		"x.js": "line one\n<script>alert(1)</script>\nline three\n",
	}

	lines := generateContextLines("x.js", 2, fileContents)
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
	report, err := GenerateReportFrom(ReportInput{Comments: comments, Diff: diff, PRNumber: 123, PRURL: "https://github.com/test-owner/test-repo/pull/123", PRBody: "Test PR body", CommitSHA: "abc1234", ModelName: "gemini-pro", GeneratedAt: testTime})
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
	report, err := GenerateReportFrom(ReportInput{Comments: comments, Diff: diff, PRNumber: 123, PRURL: "https://github.com/test-owner/test-repo/pull/123", PRBody: "Test PR body", CommitSHA: "abc1234", ModelName: "gemini-pro", GeneratedAt: testTime, FileContents: fileContents})
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
	report, err := GenerateReportFrom(ReportInput{Comments: comments, Diff: diff, PRNumber: 123, PRURL: "https://github.com/test-owner/test-repo/pull/123", PRBody: "Test PR body", CommitSHA: "abc1234", ModelName: "gemini-pro", GeneratedAt: testTime, FileContents: fileContents})
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
	assert.Contains(t, report[detailsIdx:], "Input tokens: 100 · Output tokens: 20")
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
	markup := report[:strings.Index(report, `<div id="chat-sidebar"`)]
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
	report, err := GenerateReportFrom(ReportInput{Comments: comments, Diff: diff, PRNumber: 123, PRURL: "https://github.com/test-owner/test-repo/pull/123", CommitSHA: "abc1234", ModelName: "gemini-pro", GeneratedAt: time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC), FileContents: fileContents})
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

func TestGenerateReport_ProfileLineInHeader(t *testing.T) {
	in := layoutFixtureInput()
	in.Profile = "Lite"
	report := renderLayoutFixture(t, in)
	header := report[:strings.Index(report, "<summary>Review details</summary>")]
	assert.Contains(t, header, `<p class="report-meta report-profile">Profile: Lite</p>`)
	assert.Greater(t, strings.Index(header, "Profile: Lite"), strings.Index(header, "<h1>Add retry to the sync worker</h1>"), "profile line sits under the title")

	in.Profile = "Custom (based on Lite)"
	in.ProfileDeviations = []string{"effort high (default medium)", "wall clock 600 s (default 300 s)"}
	report = renderLayoutFixture(t, in)
	assert.Contains(t, report, `Profile: Custom (based on Lite) <span class="report-profile-deviations">(effort high (default medium), wall clock 600 s (default 300 s))</span></p>`)

	in.Profile = "Full (legacy config)"
	in.ProfileDeviations = nil
	report = renderLayoutFixture(t, in)
	assert.Contains(t, report, `Profile: Full (legacy config)</p>`)
	assert.NotContains(t, report, `<span class="report-profile-deviations">`)

	in.Profile = ""
	report = renderLayoutFixture(t, in)
	assert.NotContains(t, report, "Profile:")
}

func addedFileDiff(path string, lines ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, path, path, len(lines))
	for _, line := range lines {
		b.WriteString("+" + line + "\n")
	}
	return b.String()
}

func fileModes(views []FileView) map[string]string {
	modes := map[string]string{}
	for _, v := range views {
		modes[v.Path] = v.Mode
	}
	return modes
}

func TestGenerateReport_CommentedFileIsOpenAndComplete(t *testing.T) {
	report, err := GenerateReportFrom(ReportInput{
		PRURL:    "https://example.test/acme/widgets/pull/7",
		Comments: []types.LineComment{{FilePath: "widgets/gear.go", LineNumber: 2, CommentBody: "Teeth can be negative."}},
		Diff:     addedFileDiff("widgets/gear.go", "package widgets", "var teeth = -1", "var spokes = 4"),
	})
	assert.NoError(t, err)
	assert.Contains(t, report, `<div class="file-header">widgets/gear.go</div>`)
	assert.NotContains(t, report, "<details class=\"diff-file\">")
	assert.NotContains(t, report, "not rendered")
	for _, line := range []string{"package widgets", "var teeth = -1", "var spokes = 4"} {
		assert.Contains(t, report, `<div class="line-content">`+line+`</div>`)
	}
	assert.Less(t, strings.Index(report, "var teeth = -1"), strings.LastIndex(report, "Teeth can be negative."))
	assert.Equal(t, 1, strings.Count(report, `<div class="comment-row">`))
}

func TestGenerateReport_UncommentedFileIsCollapsedWithItsLineCount(t *testing.T) {
	report, err := GenerateReportFrom(ReportInput{
		Diff: `diff --git a/widgets/axle.go b/widgets/axle.go
--- a/widgets/axle.go
+++ b/widgets/axle.go
@@ -40,2 +40,2 @@
 keep
-old
+new
`,
	})
	assert.NoError(t, err)
	assert.Contains(t, report, `<summary class="file-header">widgets/axle.go (3 lines)</summary>`)
	assert.NotContains(t, report, "<details class=\"diff-file\" open")
	assert.NotContains(t, report, `<div class="file-header">widgets/axle.go</div>`)
	assert.Contains(t, report, `<div class="line-content">new</div>`)
}

func TestGenerateReport_GeneratedFileIsNotRenderedUnlessCommented(t *testing.T) {
	in := ReportInput{
		PRURL: "https://example.test/acme/widgets/pull/7",
		Diff:  addedFileDiff("yarn.lock", "lockedDependency", "anotherDependency") + addedFileDiff("vendor/acme/gear.go", "vendoredGear") + addedFileDiff("web/app.min.js", "minifiedBundle"),
	}
	report, err := GenerateReportFrom(in)
	assert.NoError(t, err)
	assert.Contains(t, report, `yarn.lock: not rendered (2 lines) <a href="https://example.test/acme/widgets/pull/7/files">view on GitHub</a>`)
	assert.Contains(t, report, "vendor/acme/gear.go: not rendered (1 line) <a")
	assert.Contains(t, report, "web/app.min.js: not rendered (1 line) <a")
	for _, hidden := range []string{"lockedDependency", "vendoredGear", "minifiedBundle"} {
		assert.NotContains(t, report, hidden)
	}

	in.PRURL = ""
	report, err = GenerateReportFrom(in)
	assert.NoError(t, err)
	assert.Contains(t, report, "yarn.lock: not rendered (2 lines)</div>")
	assert.NotContains(t, report, "view on GitHub")

	in.Comments = []types.LineComment{{FilePath: "yarn.lock", LineNumber: 1, CommentBody: "This pin is yanked."}}
	report, err = GenerateReportFrom(in)
	assert.NoError(t, err)
	assert.Contains(t, report, `<div class="file-header">yarn.lock</div>`)
	assert.Contains(t, report, `<div class="line-content">lockedDependency</div>`)
	assert.Contains(t, report, "This pin is yanked.")
	assert.NotContains(t, report, "vendoredGear")
}

func TestIsGeneratedPath(t *testing.T) {
	cases := map[string]bool{
		"package-lock.json":            true,
		"services/api/go.sum":          true,
		"third_party/vendor/lib/a.go":  true,
		"node_modules/left/index.js":   true,
		"web/dist/bundle.js":           true,
		"build/out.txt":                true,
		"static/site.min.css":          true,
		"static/site.js.map":           true,
		"notebooks/explore.ipynb":      true,
		"api/widget.pb.go":             true,
		"api/widget_pb2.py":            true,
		"web/__snapshots__/gear.snap":  true,
		"assets/logo.svg":              true,
		"widgets/gear.go":              false,
		"widgets/build":                false,
		"vendors/gear.go":              false,
		"docs/distribution/go.summary": false,
	}
	for path, want := range cases {
		assert.Equal(t, want, isGeneratedPath(path), path)
	}
}

func TestParseDiff_LongLineIsTruncatedOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("a", maxLineBytes-1) + "é" + "tail"
	files := ParseDiff(addedFileDiff("widgets/gear.go", long, "<&>"))
	assert.Len(t, files, 1)
	assert.Equal(t, []Line{
		{Type: "added", Content: template.HTML(strings.Repeat("a", maxLineBytes-1)), NewLineNumber: 1, Truncated: true},
		{Type: "added", Content: "&lt;&amp;&gt;", NewLineNumber: 2},
	}, files[0].Lines)

	entities := ParseDiff(addedFileDiff("widgets/gear.go", strings.Repeat("<", maxLineBytes+1)))
	assert.Equal(t, template.HTML(strings.Repeat("&lt;", maxLineBytes)), entities[0].Lines[0].Content)
}

func TestGenerateReport_TruncatedLineShowsAMarker(t *testing.T) {
	long := strings.Repeat("a", maxLineBytes) + "droppedTail"
	in := ReportInput{
		Comments: []types.LineComment{{FilePath: "widgets/gear.go", LineNumber: 1, CommentBody: "This literal is huge."}},
		Diff:     addedFileDiff("widgets/gear.go", long) + addedFileDiff("widgets/axle.go", long, "shortLine"),
	}
	report, err := GenerateReportFrom(in)
	assert.NoError(t, err)
	assert.Equal(t, 1, strings.Count(report, `<span class="line-truncated">line truncated</span>`))
	assert.NotContains(t, report, "droppedTail")
	assert.Contains(t, report, "This literal is huge.")
	assert.Contains(t, report, "widgets/axle.go: not rendered (2 lines)")
	assert.NotContains(t, report, "shortLine")
}

func TestPlanFiles_BudgetKeepsCommentedFilesAndDropsLaterUncommentedOnes(t *testing.T) {
	file := func(path string, size int) *DiffFile {
		return &DiffFile{Path: path, Lines: []Line{
			{Type: lineTypeGap, Content: template.HTML(strings.Repeat("g", 1000))},
			{Type: "added", Content: template.HTML(strings.Repeat("x", size)), NewLineNumber: 1},
		}}
	}
	files := []*DiffFile{file("a.go", 40), file("b.go", 40), file("c.go", 500), file("d.go", 20), file("e.go", 30)}
	comments := map[string]map[int][]CommentView{"c.go": {1: {{}}}, "e.go": {0: {{}}}}

	views := planFiles(files, comments, 600)

	var order []string
	for _, v := range views {
		order = append(order, v.Path)
		assert.Equal(t, 1, v.LineCount, v.Path)
	}
	assert.Equal(t, []string{"a.go", "b.go", "c.go", "d.go", "e.go"}, order)
	assert.Equal(t, map[string]string{
		"a.go": fileCollapsed, "b.go": fileSkipped, "c.go": fileOpen, "d.go": fileCollapsed, "e.go": fileOpen,
	}, fileModes(views))

	assert.Equal(t, map[string]string{
		"a.go": fileSkipped, "b.go": fileSkipped, "c.go": fileOpen, "d.go": fileSkipped, "e.go": fileOpen,
	}, fileModes(planFiles(files, comments, 100)))
}
