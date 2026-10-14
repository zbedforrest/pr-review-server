package publisher

import (
	"regexp"
	"strings"
)

// tagLikeRe matches an HTML start, end or self-closing tag. GitHub renders
// one written in prose as markup, so `renders an <img> with an empty src`
// loses its subject or shows a broken image.
var tagLikeRe = regexp.MustCompile(`</?[A-Za-z][A-Za-z0-9-]*(?:\s[^<>\n]*)?/?>`)

// EscapeProseHTML puts tag-like tokens in agent prose into code spans,
// leaving fenced blocks and existing code spans as they are.
func EscapeProseHTML(text string) string {
	if !RenderHygieneEnabled() || !strings.Contains(text, "<") {
		return text
	}
	var b strings.Builder
	fence := ""
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence == "" && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
			fence = trimmed[:3]
			b.WriteString(line)
			continue
		}
		if fence != "" && strings.HasPrefix(trimmed, fence) {
			fence = ""
			b.WriteString(line)
			continue
		}
		if fence != "" {
			b.WriteString(line)
			continue
		}
		b.WriteString(escapeOutsideCodeSpans(line))
	}
	return b.String()
}

// escapeOutsideCodeSpans escapes the prose of one line. A code span opens
// with a run of backticks and closes at the next run of the same length; a
// run with no match is literal text, as in CommonMark.
func escapeOutsideCodeSpans(line string) string {
	var b strings.Builder
	prose := 0
	for i := 0; i < len(line); {
		if line[i] != '`' {
			i++
			continue
		}
		run := backtickRun(line, i)
		end := closingRun(line, i+run, run)
		if end < 0 {
			i += run
			continue
		}
		b.WriteString(tagLikeRe.ReplaceAllString(line[prose:i], "`$0`"))
		b.WriteString(line[i : end+run])
		i = end + run
		prose = i
	}
	b.WriteString(tagLikeRe.ReplaceAllString(line[prose:], "`$0`"))
	return b.String()
}

func backtickRun(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}

// closingRun returns where the next run of exactly n backticks starts at or
// after from, or -1.
func closingRun(s string, from, n int) int {
	for i := from; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		run := backtickRun(s, i)
		if run == n {
			return i
		}
		i += run
	}
	return -1
}
