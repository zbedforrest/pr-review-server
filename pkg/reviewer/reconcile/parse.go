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

	prismMarkerRe    = regexp.MustCompile(`<!-- prism:finding:[^\s>]+ -->`)
	htmlCommentRe    = regexp.MustCompile(`(?s)<!--.*?-->`)
	detailsBlockRe   = regexp.MustCompile(`(?s)<details>.*?</details>`)
	htmlTagRe        = regexp.MustCompile(`<[^>]+>`)
	markdownImageRe  = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	markdownLinkRe   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	codeFenceRe      = regexp.MustCompile("(?s)```.*?```")
	italicHeaderRe   = regexp.MustCompile(`(?m)^_[^_\n]*_(?:\s*\|\s*_[^_\n]*_)*\s*$`)
	codeScanHeaderRe = regexp.MustCompile(`(?m)^##\s*[^/\n]+/\s*(.+)$`)
	geminiBadgeRe    = regexp.MustCompile(`!\[(critical|high|medium|low)\]\([^)]*codereviewagent[^)]*\)`)
	coderabbitSevRe  = regexp.MustCompile(`_\S*\s*(Critical|Major|Minor|Trivial)_`)
	sentenceEndRe    = regexp.MustCompile(`[.!?](?:\s|$)`)
)

// IsGreptileAuthor reports whether a comment author login belongs to Greptile.
func IsGreptileAuthor(login string) bool {
	return login == "greptile-apps[bot]" || login == "greptile-apps"
}

// SourceOf names the producer behind a comment author login. Logins with a
// [bot] suffix that no known reviewer owns are other bots; everything else
// is a human.
func SourceOf(login string) Source {
	bare := strings.TrimSuffix(login, "[bot]")
	switch {
	case IsGreptileAuthor(login):
		return SourceGreptile
	case strings.EqualFold(bare, "copilot") || strings.EqualFold(bare, "copilot-pull-request-reviewer"):
		return SourceCopilot
	case strings.EqualFold(bare, "coderabbitai"):
		return SourceCodeRabbit
	case strings.EqualFold(bare, "gemini-code-assist"):
		return SourceGemini
	case strings.EqualFold(bare, "github-advanced-security"):
		return SourceCodeScan
	case strings.HasSuffix(login, "[bot]"):
		return SourceOtherBot
	}
	return SourceHuman
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

	start, end := commentRange(c)
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

// ParseExternalComments parses every root comment in cs that PRism did not
// post: Greptile, Copilot, CodeRabbit, Gemini, code scanning, other bots and
// humans alike. Replies are skipped, as are the comments whose ids the
// ledger owns and any body carrying the PRism finding marker. A root with no
// prose left after the markup is dropped.
func ParseExternalComments(cs []ExternalComment, own map[int64]bool) []ExternalFinding {
	var out []ExternalFinding
	for _, c := range cs {
		if c.InReplyToID != 0 || own[c.ID] || prismMarkerRe.MatchString(c.Body) || c.Path == "" {
			continue
		}
		if IsGreptileAuthor(c.Author) {
			if f, ok := ParseGreptileComment(c); ok {
				out = append(out, f)
				continue
			}
		}
		if f, ok := ParseGenericComment(c); ok {
			out = append(out, f)
		}
	}
	return out
}

// ParseGenericComment reads a root comment of any producer into a finding:
// the severity from a badge the producer is known to use, the title from a
// leading bold line or heading, else the first sentence, and the body from
// the prose with HTML, hidden blocks, images and prompts removed.
func ParseGenericComment(c ExternalComment) (ExternalFinding, bool) {
	if c.InReplyToID != 0 {
		return ExternalFinding{}, false
	}
	source := SourceOf(c.Author)
	body := c.Body
	severity := genericSeverity(source, body)

	suggestion := ""
	if m := suggestionFenceRe.FindStringSubmatchIndex(body); m != nil {
		suggestion = strings.TrimRight(body[m[2]:m[3]], "\n")
		body = body[:m[0]] + body[m[1]:]
	}
	body = htmlCommentRe.ReplaceAllString(body, " ")
	body = detailsBlockRe.ReplaceAllString(body, " ")
	body = codeFenceRe.ReplaceAllString(body, " ")
	body = markdownImageRe.ReplaceAllString(body, " ")
	body = htmlTagRe.ReplaceAllString(body, " ")
	body = markdownLinkRe.ReplaceAllString(body, "$1")
	body = italicHeaderRe.ReplaceAllString(body, " ")

	title := ""
	if m := codeScanHeaderRe.FindStringSubmatchIndex(body); m != nil {
		title = strings.TrimSpace(body[m[2]:m[3]])
		body = body[:m[0]] + body[m[1]:]
	} else if m := boldTitleRe.FindStringSubmatchIndex(body); m != nil && strings.TrimSpace(body[:m[0]]) == "" {
		title = strings.TrimSpace(body[m[2]:m[3]])
		body = body[m[1]:]
	}
	body = strings.Join(strings.Fields(body), " ")
	if title == "" {
		title, body = splitFirstSentence(body)
	}
	if title == "" && body == "" {
		return ExternalFinding{}, false
	}
	start, end := commentRange(c)
	return ExternalFinding{
		Source:     source,
		CommentID:  c.ID,
		Severity:   severity,
		Title:      strings.TrimSuffix(title, "."),
		Body:       body,
		File:       c.Path,
		StartLine:  start,
		EndLine:    end,
		Suggestion: suggestion,
	}, true
}

// splitFirstSentence returns the first sentence of s and the rest; a text
// with no sentence boundary is all title.
func splitFirstSentence(s string) (string, string) {
	if m := sentenceEndRe.FindStringIndex(s); m != nil {
		return strings.TrimSpace(s[:m[0]+1]), strings.TrimSpace(s[m[1]:])
	}
	return s, ""
}

func commentRange(c ExternalComment) (int, int) {
	start, end := c.StartLine, c.Line
	if start == 0 {
		start = end
	}
	return start, end
}

func genericSeverity(source Source, body string) string {
	switch source {
	case SourceCodeRabbit:
		if m := coderabbitSevRe.FindStringSubmatch(body); m != nil {
			switch m[1] {
			case "Critical":
				return "critical"
			case "Major":
				return "medium"
			default:
				return "low"
			}
		}
	case SourceGemini:
		if m := geminiBadgeRe.FindStringSubmatch(body); m != nil {
			switch m[1] {
			case "critical":
				return "critical"
			case "high", "medium":
				return "medium"
			default:
				return "low"
			}
		}
	}
	return "unknown"
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
