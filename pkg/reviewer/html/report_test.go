package html

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pr-review-server/pkg/reviewer/types"
)

var update = flag.Bool("update", false, "update golden files")

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

func TestReportHTMLSnapshot(t *testing.T) {
	mockDataDir := "testdata/mock"
	commentsMockPath := filepath.Join(mockDataDir, "comments.json")
	diffMockPath := filepath.Join(mockDataDir, "diff.txt")
	prBodyMockPath := filepath.Join(mockDataDir, "pr_body.txt")
	prNumberMockPath := filepath.Join(mockDataDir, "pr_number.txt")

	// Check if mock data exists
	if _, err := os.Stat(commentsMockPath); os.IsNotExist(err) {
		t.Skipf("Mock data not found. Generate it by running: go run ./tools/mockgen -pr=20986")
	}

	// Load mock data
	commentsJSON, err := os.ReadFile(commentsMockPath)
	require.NoError(t, err)
	var comments []types.LineComment
	err = json.Unmarshal(commentsJSON, &comments)
	require.NoError(t, err)

	diffBytes, err := os.ReadFile(diffMockPath)
	require.NoError(t, err)
	diff := string(diffBytes)

	prBodyBytes, err := os.ReadFile(prBodyMockPath)
	require.NoError(t, err)
	prBody := string(prBodyBytes)

	prNumberBytes, err := os.ReadFile(prNumberMockPath)
	require.NoError(t, err)
	var prNumber int
	_, err = fmt.Sscanf(string(prNumberBytes), "%d", &prNumber)
	require.NoError(t, err)

	testModelName := "TEST_MODEL_NAME_PLACEHOLDER"
	testPrompt := "TEST_PROMPT_PLACEHOLDER"
	testCommitSHA := "abc1234567890def"
	testPRURL := fmt.Sprintf("https://github.com/test-owner/test-repo/pull/%d", prNumber)
	testTime := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	htmlContent, err := GenerateReport(comments, diff, prNumber, testPRURL, prBody, testPrompt, testCommitSHA, testModelName, int32(100), int32(200), int32(300), testTime)
	require.NoError(t, err)

	goldenFile := filepath.Join("testdata", "report.html.golden")

	if *update {
		t.Log("Updating golden file")
		err := os.MkdirAll(filepath.Dir(goldenFile), 0755)
		require.NoError(t, err)
		err = os.WriteFile(goldenFile, []byte(htmlContent), 0644)
		require.NoError(t, err)
	}

	golden, err := os.ReadFile(goldenFile)
	require.NoError(t, err, "Golden file not found. Run with -update to create it.")

	assert.Equal(t, string(golden), htmlContent, "HTML content does not match golden file. Run with -update to update it.")
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

	// Should NOT contain adjacent comments section since fileContents is nil
	assert.NotContains(t, report, "<h2>Adjacent File-Specific Comments</h2>")
	assert.NotContains(t, report, "This line is not in the diff")
	assert.NotContains(t, report, "app.go:999")

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
