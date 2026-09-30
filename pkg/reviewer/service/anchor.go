package service

import (
	"path"
	"regexp"
	"strconv"
	"strings"

	"pr-review-server/pkg/reviewer/types"
)

var backtickSpan = regexp.MustCompile("`([^`\n]{3,120})`")

// AnchorWholeFileFindings gives active whole-file findings (line 0) a line in
// the PR diff when their text quotes code that a changed line of that file
// contains, so they post inline instead of at file level. A finding is
// anchored to the diff line matching the most quoted spans, preferring added
// lines and then the earliest; one with no match keeps line 0. It returns how
// many findings it anchored.
func AnchorWholeFileFindings(comments []types.LineComment, diff string) int {
	files := diffNewLines(diff)
	if len(files) == 0 {
		return 0
	}
	anchored := 0
	for i := range comments {
		c := &comments[i]
		if c.LineNumber != 0 || c.Inactive || c.FilePath == "SUMMARY" || c.Provenance == "mechanical" {
			continue
		}
		lines := linesForFile(files, c.FilePath)
		spans := quotedSpans(c.CommentBody, c.FilePath)
		if len(lines) == 0 || len(spans) == 0 {
			continue
		}
		best, bestScore := 0, 0
		for _, l := range lines {
			score := 0
			for _, s := range spans {
				if strings.Contains(l.text, s) {
					score += 2
				}
			}
			if score == 0 {
				continue
			}
			if l.added {
				score++
			}
			if score > bestScore {
				best, bestScore = l.number, score
			}
		}
		if best > 0 {
			c.LineNumber = best
			anchored++
		}
	}
	return anchored
}

type diffLine struct {
	number int
	text   string
	added  bool
}

// diffNewLines maps each file in a unified diff to its added and context
// lines, numbered in the new version.
func diffNewLines(diff string) map[string][]diffLine {
	files := map[string][]diffLine{}
	var file string
	line := 0
	for _, raw := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(raw, "+++ "):
			file = strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(raw, "+++ ")), "b/")
			if file == "/dev/null" {
				file = ""
			}
			line = 0
		case strings.HasPrefix(raw, "@@"):
			if start, ok := parseHunkNewStart(raw); ok {
				line = start
			}
		case file == "" || line == 0:
		case strings.HasPrefix(raw, "+"):
			files[file] = append(files[file], diffLine{line, raw[1:], true})
			line++
		case strings.HasPrefix(raw, " "):
			files[file] = append(files[file], diffLine{line, raw[1:], false})
			line++
		}
	}
	return files
}

func parseHunkNewStart(header string) (int, bool) {
	plus := strings.Index(header, "+")
	if plus < 0 {
		return 0, false
	}
	rest := header[plus+1:]
	if end := strings.IndexAny(rest, ", @"); end >= 0 {
		rest = rest[:end]
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n > 0
}

func linesForFile(files map[string][]diffLine, file string) []diffLine {
	if lines, ok := files[file]; ok {
		return lines
	}
	for name, lines := range files {
		if sameFileStrict(name, file) {
			return lines
		}
	}
	return nil
}

// quotedSpans returns the backticked code spans of a comment, minus ones that
// only name the file itself.
func quotedSpans(body, file string) []string {
	seen := map[string]bool{}
	var spans []string
	base := path.Base(file)
	for _, m := range backtickSpan.FindAllStringSubmatch(body, -1) {
		s := strings.TrimSpace(m[1])
		if len(s) < 3 || seen[s] || s == file || s == base || strings.HasSuffix(file, s) {
			continue
		}
		seen[s] = true
		spans = append(spans, s)
	}
	return spans
}
