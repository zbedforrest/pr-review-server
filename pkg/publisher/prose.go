package publisher

import (
	"regexp"
	"strings"
)

// tagLikeRe matches HTML that GitHub renders as markup when written in
// prose: start, end and self-closing tags, comments and declarations. So
// `renders an <img> with an empty src` loses its subject or shows a broken
// image, and a <!-- comment --> hides its words.
var tagLikeRe = regexp.MustCompile(`</?[A-Za-z][A-Za-z0-9-]*(?:\s[^<>\n]*)?/?>|<!--[^\n]*?-->|<![A-Za-z][^<>\n]*>|<\?[^<>\n]*?\?>`)

var (
	backtickFenceRe = regexp.MustCompile("^(`{3,})[^`]*$")
	tildeFenceRe    = regexp.MustCompile(`^(~{3,})`)
)

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
		switch {
		case fence == "":
			if open := fenceOpening(trimmed); open != "" {
				fence = open
				b.WriteString(line)
				continue
			}
			b.WriteString(escapeOutsideCodeSpans(line))
		case closesFence(trimmed, fence):
			fence = ""
			b.WriteString(line)
		default:
			b.WriteString(line)
		}
	}
	return b.String()
}

// fenceOpening returns the delimiter run when a line opens a fenced block.
func fenceOpening(trimmed string) string {
	if m := backtickFenceRe.FindStringSubmatch(trimmed); m != nil {
		return m[1]
	}
	if m := tildeFenceRe.FindStringSubmatch(trimmed); m != nil {
		return m[1]
	}
	return ""
}

// closesFence reports whether a line is a closing run of the fence's
// character at least as long as the opening one.
func closesFence(trimmed, fence string) bool {
	run := strings.TrimRight(trimmed, " \t")
	return len(run) >= len(fence) && strings.Trim(run, fence[:1]) == ""
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
		b.WriteString(escapeProse(line[prose:i]))
		b.WriteString(line[i : end+run])
		i = end + run
		prose = i
	}
	b.WriteString(escapeProse(line[prose:]))
	return b.String()
}

// escapeProse wraps tag-like tokens in code spans. Backticks left in prose
// are unmatched, and one could pair with a wrapper and push the tag back out
// of its span, so they become entities when a tag is wrapped.
func escapeProse(prose string) string {
	if !tagLikeRe.MatchString(prose) {
		return prose
	}
	return tagLikeRe.ReplaceAllString(strings.ReplaceAll(prose, "`", "&#96;"), "`$0`")
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
