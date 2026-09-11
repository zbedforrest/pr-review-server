package reconcile

import (
	"regexp"
	"strings"
)

var (
	greptileBadgeRe      = regexp.MustCompile(`<img[^>]*\balt="(P[0-9])"[^>]*>`)
	boldTitleRe          = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	suggestionFenceRe    = regexp.MustCompile("(?s)```suggestion[ \\t]*\\n(.*?)```")
	greptileBodyTerminal = []string{"<details", "<a "}
)

// IsGreptileAuthor reports whether a comment author login belongs to Greptile.
func IsGreptileAuthor(login string) bool {
	return login == "greptile-apps[bot]" || login == "greptile-apps"
}

// ParseGreptileComment parses one Greptile inline review comment. It returns
// ok=false for thread replies (which carry no severity badge) and for bodies
// that lack the badge or a bold title.
func ParseGreptileComment(c ExternalComment) (ExternalFinding, bool) {
	if c.InReplyToID != 0 {
		return ExternalFinding{}, false
	}
	badge := greptileBadgeRe.FindStringSubmatchIndex(c.Body)
	if badge == nil {
		return ExternalFinding{}, false
	}
	severity := greptileSeverity(c.Body[badge[2]:badge[3]])
	rest := c.Body[badge[1]:]

	title := boldTitleRe.FindStringSubmatchIndex(rest)
	if title == nil {
		return ExternalFinding{}, false
	}
	body := rest[title[1]:]
	for _, marker := range greptileBodyTerminal {
		if i := strings.Index(body, marker); i >= 0 {
			body = body[:i]
		}
	}

	suggestion := ""
	if m := suggestionFenceRe.FindStringSubmatchIndex(body); m != nil {
		suggestion = strings.TrimRight(body[m[2]:m[3]], "\n")
		body = body[:m[0]] + body[m[1]:]
	}

	start, end := c.StartLine, c.Line
	if start == 0 {
		start = end
	}
	return ExternalFinding{
		Source:     SourceGreptile,
		CommentID:  c.ID,
		Severity:   severity,
		Title:      strings.TrimSpace(rest[title[2]:title[3]]),
		Body:       strings.TrimSpace(body),
		File:       c.Path,
		StartLine:  start,
		EndLine:    end,
		Suggestion: suggestion,
	}, true
}

// ParseGreptileComments parses every top-level Greptile comment in cs,
// skipping other authors, replies, and unparseable bodies.
func ParseGreptileComments(cs []ExternalComment) []ExternalFinding {
	var out []ExternalFinding
	for _, c := range cs {
		if !IsGreptileAuthor(c.Author) {
			continue
		}
		if f, ok := ParseGreptileComment(c); ok {
			out = append(out, f)
		}
	}
	return out
}

func greptileSeverity(badge string) string {
	switch badge {
	case "P0":
		return "critical"
	case "P1":
		return "medium"
	case "P2":
		return "low"
	default:
		return "unknown"
	}
}
