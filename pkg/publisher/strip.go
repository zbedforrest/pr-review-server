package publisher

import (
	"regexp"
	"strings"
)

var (
	htmlTagRe        = regexp.MustCompile(`<[^>]+>`)
	markdownLinkRe   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	agentPromptRe    = regexp.MustCompile("(?s)Agent prompt:\\s*```text\n.*?\n```")
	howToVerifyRe    = regexp.MustCompile(`\*\*How to verify:\*\*[^\n]*`)
	renderedChromeRe = regexp.MustCompile(`Reasoning and how to verify|Source: PRism(?: · Both)?|Source: Greptile|Fix with agent|\*\(suggestion above\)\*`)
)

// StripRendered reduces a rendered inline comment to the prose it was built
// from: markers, badges, links, the agent prompt, the contract's verification
// line and the fixed chrome go, so the result compares with raw agent text
// instead of with boilerplate.
func StripRendered(body string) string {
	body = findingMarkerRe.ReplaceAllString(body, " ")
	body = agentPromptRe.ReplaceAllString(body, " ")
	body = howToVerifyRe.ReplaceAllString(body, " ")
	body = htmlTagRe.ReplaceAllString(body, " ")
	body = markdownLinkRe.ReplaceAllString(body, "$1")
	body = renderedChromeRe.ReplaceAllString(body, " ")
	return strings.Join(strings.Fields(body), " ")
}
