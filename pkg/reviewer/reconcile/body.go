package reconcile

import (
	"regexp"
	"strings"

	"pr-review-server/pkg/reviewer/payload"
)

var (
	bodyHTMLCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	bodyListItemRe    = regexp.MustCompile(`(?m)^\s*(?:[-*+]|\d+[.)])\s+`)
	bodyHeadingRe     = regexp.MustCompile(`(?m)^#+\s*`)
)

// BodyCovers reports whether the pull request body already states the
// finding's point: one paragraph or list item of the body names a specific
// subject of the finding and carries its headline (containment at or above
// half with four shared words) or its text (the external bar with eight).
// A body that merely mentions the same file is not coverage; the paragraph
// has to say what the finding says. A description_drift finding is about
// the body and quotes it, so it is never covered by it.
func BodyCovers(f payload.Finding, body string) bool {
	if isPseudoFile(f.File) || strings.TrimSpace(body) == "" {
		return false
	}
	if c := f.FindingContract; c != nil && c.FindingKind == "description_drift" {
		return false
	}
	subjects := SpecificSubjects(f.FindingContract)
	if len(subjects) == 0 {
		return false
	}
	text, headline := prismText(f), prismHeadline(f)
	for _, para := range BodyParagraphs(body) {
		named := false
		for _, name := range subjects {
			if NamesIdentifier(para, name) {
				named = true
				break
			}
		}
		if !named {
			continue
		}
		if _, ok := settledTextScore(text, headline, para); ok {
			return true
		}
	}
	return false
}

// BodyParagraphs splits a pull request body into the units a point is
// stated in: paragraphs separated by blank lines, and list items one by one.
// Markup that carries no prose (HTML comments, images, headings marks,
// fences) is removed; link text and code spans stay.
func BodyParagraphs(body string) []string {
	body = bodyHTMLCommentRe.ReplaceAllString(body, "\n")
	body = codeFenceRe.ReplaceAllString(body, "\n")
	body = markdownImageRe.ReplaceAllString(body, " ")
	body = htmlTagRe.ReplaceAllString(body, " ")
	body = markdownLinkRe.ReplaceAllString(body, "$1")
	body = bodyHeadingRe.ReplaceAllString(body, "")
	body = strings.ReplaceAll(body, "\r\n", "\n")
	var out []string
	var current []string
	flush := func() {
		if joined := strings.Join(strings.Fields(strings.Join(current, " ")), " "); joined != "" {
			out = append(out, joined)
		}
		current = current[:0]
	}
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case bodyListItemRe.MatchString(line):
			flush()
			current = append(current, bodyListItemRe.ReplaceAllString(line, ""))
		default:
			current = append(current, line)
		}
	}
	flush()
	return out
}
