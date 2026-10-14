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

func escapeOutsideCodeSpans(line string) string {
	parts := strings.Split(line, "`")
	if len(parts)%2 == 0 {
		// An unpaired backtick: GitHub shows it literally, so every segment is prose.
		return tagLikeRe.ReplaceAllString(line, "`$0`")
	}
	for i := 0; i < len(parts); i += 2 {
		parts[i] = tagLikeRe.ReplaceAllString(parts[i], "`$0`")
	}
	return strings.Join(parts, "`")
}
