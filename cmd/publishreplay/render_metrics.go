package main

import (
	"regexp"
	"strings"
)

// Renderer defects the program tracks: a summary bullet whose text says the
// finding has no impact (the impact sentence rendered as the title) and an
// inline comment titled by its bare kind label.

var summaryBulletRe = regexp.MustCompile(`(?m)^- (?:\*\*\[[A-Z]+\]\*\*|<img[^>]*>) (?:\*\*[^*]+\*\* )?(.*?) — \[`)

var nilImpactRe = regexp.MustCompile(`(?i)^(?:none\b|not? (?:user|runtime|production|demonstrated|current|direct|immediate|observable|functional|visible|impact|effect|behaviou?r|change))`)

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
