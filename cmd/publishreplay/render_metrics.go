package main

import (
	"regexp"
	"strings"

	"pr-review-server/pkg/publisher"
)

// Renderer defects the program tracks: a summary bullet whose text says the
// finding has no impact (the impact sentence rendered as the title) and an
// inline comment titled by its bare kind label.

var summaryBulletRe = regexp.MustCompile(`(?m)^- (?:\*\*\[[A-Z]+\]\*\*|<img[^>]*>) (?:\*\*[^*]+\*\* )?(.*?) \x{2014} \[`)

var nilImpactRe = regexp.MustCompile(`(?i)^(?:none(?:\s+(?:today|yet|currently|so far|at present|right now|in practice)|[.;:,]|$)|not? (?:user|runtime|production|demonstrated|current|direct|immediate|observable|functional|visible|impact|effect|behaviou?r|change))`)

var inlineTitleRe = regexp.MustCompile(`(?m)^(?:\*\*\[[A-Z]+\] (.+?)\*\*|<img[^>]*> \*\*(.+?)\*\*)$`)

var kindLabels = map[string]bool{
	"Behavior change":   true,
	"Security":          true,
	"Latent hazard":     true,
	"Operational risk":  true,
	"Test quality":      true,
	"Design":            true,
	"Description drift": true,
}

func nilImpactBullets(summary string) int {
	n := 0
	for _, m := range summaryBulletRe.FindAllStringSubmatch(summary, -1) {
		if nilImpactRe.MatchString(strings.TrimSpace(m[1])) {
			n++
		}
	}
	return n
}

func bareLabelTitle(inline string) bool {
	m := inlineTitleRe.FindStringSubmatch(inline)
	if m == nil {
		return false
	}
	title := m[1]
	if title == "" {
		title = m[2]
	}
	return kindLabels[strings.TrimSpace(title)]
}

// Output hygiene defects of the 2026-10-13 audit, read off rendered bodies.

var (
	statusLineRe      = regexp.MustCompile(`\*\*Since last review:\*\* (\d+) new · (\d+) still open · (\d+) fixed([^\n]*)`)
	foldedNoteRe      = regexp.MustCompile(`<summary>\d+ folded note`)
	sourceFooterRe    = regexp.MustCompile(`Source: PRism`)
	visibleInlineEnds = "<details>"
)

// confidenceLabelLines counts the visible lines of an inline body that open
// with a confidence label under the badge.
func confidenceLabelLines(inline string) int {
	visible := inline
	if i := strings.Index(inline, visibleInlineEnds); i >= 0 {
		visible = inline[:i]
	}
	n := 0
	for _, line := range strings.Split(visible, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "<!--") || strings.HasPrefix(line, "**[") || strings.HasPrefix(line, "<img") {
			continue
		}
		if publisher.HasConfidenceLabel(line) {
			n++
		}
	}
	return n
}

// leaksFindingID reports whether a body shows an internal finding label
// outside its marker and agent prompt.
func leaksFindingID(body string) bool {
	return publisher.MentionsFindingID(publisher.StripRendered(body))
}

// praiseBullets counts summary bullets whose text compliments the code.
func praiseBullets(summary string) int {
	n := 0
	for _, m := range summaryBulletRe.FindAllStringSubmatch(summary, -1) {
		if publisher.IsPraise(strings.TrimSpace(m[1])) {
			n++
		}
	}
	return n
}

// praiseTitle reports whether an inline comment is titled by a compliment.
func praiseTitle(inline string) bool {
	m := inlineTitleRe.FindStringSubmatch(inline)
	if m == nil {
		return false
	}
	title := m[1]
	if title == "" {
		title = m[2]
	}
	return publisher.IsPraise(title)
}

// zeroStatusOverNotes reports a summary whose status line says nothing
// changed while folded notes sit under it and the line does not count them.
func zeroStatusOverNotes(summary string) bool {
	m := statusLineRe.FindStringSubmatch(summary)
	if m == nil || !foldedNoteRe.MatchString(summary) {
		return false
	}
	return m[1] == "0" && m[2] == "0" && m[3] == "0" && !strings.Contains(m[4], "folded note")
}
