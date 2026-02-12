package html

import (
	"bytes"
	"embed"
	"fmt"
	"html"
	"html/template"
	"strings"
	"time"

	"github.com/gomarkdown/markdown"
	"pr-review-server/pkg/reviewer/types"
)

//go:embed templates
var templateFS embed.FS

// Line represents a single line in a diff.
type Line struct {
	Type          string
	Content       template.HTML
	OldLineNumber int
	NewLineNumber int
}

// DiffFile represents the diff for a single file.
type DiffFile struct {
	Path  string
	Lines []Line
}

// CommentView is a wrapper for a comment with additional view-related data.
type CommentView struct {
	types.LineComment
	Counter      int
	Total        int
	ContextLines []ContextLine // For adjacent comments, lines around the commented line
}

// ContextLine represents a line of code context for adjacent comments
type ContextLine struct {
	LineNumber int
	Content    template.HTML
	IsTarget   bool // True if this is the line being commented on
}

// ParseDiff parses a raw diff string into a structured format.
func ParseDiff(diff string) []*DiffFile {
	var files []*DiffFile
	lines := strings.Split(diff, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var currentFile *DiffFile
	var oldLine, newLine int

	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git") {
			if currentFile != nil {
				files = append(files, currentFile)
			}
			parts := strings.Split(line, " ")
			path := strings.TrimPrefix(parts[2], "a/")
			currentFile = &DiffFile{Path: path}
			continue
		}
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "index") {
			continue
		}
		if currentFile == nil {
			continue
		}

		if strings.HasPrefix(line, "@@") {
			// Parse hunk header like "@@ -1,5 +1,6 @@"
			parts := strings.Split(line, " ")
			oldRange := strings.Split(strings.TrimPrefix(parts[1], "-"), ",")
			newRange := strings.Split(strings.TrimPrefix(parts[2], "+"), ",")
			_, _ = fmt.Sscanf(oldRange[0], "%d", &oldLine)
			_, _ = fmt.Sscanf(newRange[0], "%d", &newLine)
			continue
		}

		switch {
		case strings.HasPrefix(line, "+"):
			currentFile.Lines = append(currentFile.Lines, Line{
				Type:          "added",
				Content:       template.HTML(html.EscapeString(strings.TrimPrefix(line, "+"))),
				NewLineNumber: newLine,
			})
			newLine++
		case strings.HasPrefix(line, "-"):
			currentFile.Lines = append(currentFile.Lines, Line{
				Type:          "deleted",
				Content:       template.HTML(html.EscapeString(strings.TrimPrefix(line, "-"))),
				OldLineNumber: oldLine,
			})
			oldLine++
		default:
			currentFile.Lines = append(currentFile.Lines, Line{
				Type:          "context",
				Content:       template.HTML(html.EscapeString(strings.TrimPrefix(line, " "))),
				OldLineNumber: oldLine,
				NewLineNumber: newLine,
			})
			oldLine++
			newLine++
		}
	}
	if currentFile != nil && len(currentFile.Lines) > 0 {
		files = append(files, currentFile)
	}

	return files
}

// GenerateReport creates an HTML report from a slice of line comments and a diff string.
func GenerateReport(comments []types.LineComment, diff string, prNumber int, prURL string, prBody string, prompt string, commitSHA string, modelName string, promptTokenCount int32, candidatesTokenCount int32, totalTokenCount int32, generatedAt time.Time) (string, error) {
	return GenerateReportWithContext(comments, diff, prNumber, prURL, prBody, prompt, commitSHA, modelName, promptTokenCount, candidatesTokenCount, totalTokenCount, generatedAt, nil)
}

// GenerateReportWithContext creates an HTML report with additional file context for adjacent comments and token counting.
func GenerateReportWithContext(comments []types.LineComment, diff string, prNumber int, prURL string, prBody string, prompt string, commitSHA string, modelName string, promptTokenCount int32, candidatesTokenCount int32, totalTokenCount int32, generatedAt time.Time, fileContents map[string]string) (string, error) {
	diffFiles := ParseDiff(diff)
	commentsByFile := make(map[string]map[int][]CommentView)
	var summaryComments []CommentView
	var generalComments []CommentView
	var adjacentComments []CommentView
	totalComments := len(comments)

	for i, comment := range comments {
		// Separate summary comments, general comments, and file-specific comments
		if comment.FilePath == "SUMMARY" {
			commentView := CommentView{
				LineComment: comment,
				Counter:     i + 1,
				Total:       totalComments,
			}
			summaryComments = append(summaryComments, commentView)
		} else if comment.FilePath == "GENERAL" {
			commentView := CommentView{
				LineComment: comment,
				Counter:     i + 1,
				Total:       totalComments,
			}
			generalComments = append(generalComments, commentView)
		} else {
			// Check if this file-specific comment can be displayed inline with the diff
			canDisplayInline := false
			for _, diffFile := range diffFiles {
				if diffFile.Path == comment.FilePath {
					// Check if the line number exists in the diff
					for _, line := range diffFile.Lines {
						if line.NewLineNumber == comment.LineNumber || line.OldLineNumber == comment.LineNumber {
							canDisplayInline = true
							break
						}
					}
					break
				}
			}

			commentView := CommentView{
				LineComment: comment,
				Counter:     i + 1,
				Total:       totalComments,
			}

			if canDisplayInline {
				// Display inline with diff
				if _, ok := commentsByFile[comment.FilePath]; !ok {
					commentsByFile[comment.FilePath] = make(map[int][]CommentView)
				}
				commentsByFile[comment.FilePath][comment.LineNumber] = append(commentsByFile[comment.FilePath][comment.LineNumber], commentView)
			} else {
				// Can't display inline - add to adjacent comments section with context lines (only if adjacent comments enabled)
				if fileContents != nil {
					commentView.ContextLines = generateContextLines(comment.FilePath, comment.LineNumber, fileContents)
					adjacentComments = append(adjacentComments, commentView)
				}
				// If fileContents is nil (adjacent comments disabled), skip this comment
			}
		}
	}

	// Format commit SHA for display (first 7 characters)
	shortCommitSHA := commitSHA
	if len(commitSHA) > 7 {
		shortCommitSHA = commitSHA[:7]
	}

	reportData := struct {
		PRNumber             int
		PRURL                string
		PRBody               template.HTML
		Prompt               template.HTML
		Files                []*DiffFile
		Comments             map[string]map[int][]CommentView
		SummaryComments      []CommentView
		GeneralComments      []CommentView
		AdjacentComments     []CommentView
		RawDiff              string
		ModelName            string
		PromptTokenCount     int32
		CandidatesTokenCount int32
		TotalTokenCount      int32
		GeneratedAt          string
		CommitSHA            string
		ShortCommitSHA       string
	}{
		PRNumber:             prNumber,
		PRURL:                prURL,
		PRBody:               template.HTML(markdown.ToHTML([]byte(prBody), nil, nil)),
		Prompt:               template.HTML(markdown.ToHTML([]byte(prompt), nil, nil)),
		Files:                diffFiles,
		Comments:             commentsByFile,
		SummaryComments:      summaryComments,
		GeneralComments:      generalComments,
		AdjacentComments:     adjacentComments,
		RawDiff:              diff,
		ModelName:            modelName,
		PromptTokenCount:     promptTokenCount,
		CandidatesTokenCount: candidatesTokenCount,
		TotalTokenCount:      totalTokenCount,
		GeneratedAt:          generatedAt.Format("Monday, January 2, 2006 at 3:04 PM MST"),
		CommitSHA:            commitSHA,
		ShortCommitSHA:       shortCommitSHA,
	}

	funcMap := template.FuncMap{
		"markdownify": func(s string) template.HTML {
			// Unescape HTML entities that were auto-escaped by the template engine
			unescaped := html.UnescapeString(s)
			htmlBytes := markdown.ToHTML([]byte(unescaped), nil, nil)
			return template.HTML(htmlBytes)
		},
		"dict": func(values ...interface{}) (map[string]interface{}, error) {
			if len(values)%2 != 0 {
				return nil, fmt.Errorf("dict expects an even number of arguments: got %d", len(values))
			}
			m := make(map[string]interface{}, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				key, ok := values[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict keys must be strings: got %T", values[i])
				}
				m[key] = values[i+1]
			}
			return m, nil
		},
	}

	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.tmpl")
	if err != nil {
		return "", fmt.Errorf("error parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", reportData); err != nil {
		return "", fmt.Errorf("error executing template: %w", err)
	}

	return buf.String(), nil
}

// generateContextLines creates context lines around a specific line number for adjacent comments
func generateContextLines(filePath string, lineNumber int, fileContents map[string]string) []ContextLine {
	return GenerateContextLinesForTest(filePath, lineNumber, fileContents)
}

// GenerateContextLinesForTest exports the context line generation for testing
func GenerateContextLinesForTest(filePath string, lineNumber int, fileContents map[string]string) []ContextLine {
	content, exists := fileContents[filePath]
	if !exists {
		return nil
	}

	lines := strings.Split(content, "\n")
	if lineNumber <= 0 || lineNumber > len(lines) {
		return nil
	}

	// Show 3 lines before and after the target line
	contextRange := 3
	start := lineNumber - contextRange - 1 // Convert to 0-based indexing
	end := lineNumber + contextRange - 1

	if start < 0 {
		start = 0
	}
	if end >= len(lines) {
		end = len(lines) - 1
	}

	var contextLines []ContextLine
	for i := start; i <= end; i++ {
		contextLine := ContextLine{
			LineNumber: i + 1, // Convert back to 1-based
			Content:    template.HTML(lines[i]),
			IsTarget:   i+1 == lineNumber,
		}
		contextLines = append(contextLines, contextLine)
	}

	return contextLines
}
