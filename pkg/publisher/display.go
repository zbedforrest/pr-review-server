package publisher

import (
	"fmt"
	"strings"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

// displayOf is the finding's rewrite for posting, or nil when it has none
// worth showing.
func displayOf(f payload.Finding) *types.Display {
	if f.Display == nil || strings.TrimSpace(f.Display.Title) == "" || strings.TrimSpace(f.Display.Body) == "" {
		return nil
	}
	return f.Display
}

// renderDisplayInline shows the rewrite's title and body, the way authors
// read a review comment: what triggers the problem, what breaks, the fix.
// Everything the default form shows (effect statement, caveat, reasoning)
// moves into the details block, so nothing the finding says is lost.
func renderDisplayInline(f payload.Finding, d types.Display, sourceTag, agentLinkBase, badgeBase, settleFooter string) string {
	comment := EscapeProseHTML(commentText(f))
	var b strings.Builder
	b.WriteString(FindingMarker(f.ID) + "\n")
	b.WriteString(inlineHeader(f.Severity, EscapeProseHTML(strings.TrimSpace(d.Title)), badgeBase))
	b.WriteString("\n" + EscapeProseHTML(strings.TrimSpace(d.Body)) + "\n")
	if fence := suggestionFenceRe.FindString(comment); fence != "" {
		b.WriteString("\n" + fence + "\n")
	}

	var details strings.Builder
	c := f.FindingContract
	hasContract := c != nil && f.FindingContractStatus == "valid"
	if hasContract {
		if impact := ContractSentence(c.CurrentImpact); impact != "" {
			details.WriteString(impact + "\n\n")
		}
	}
	if reasoning := StripFindingIDAsides(strings.TrimSpace(suggestionFenceRe.ReplaceAllString(comment, "*(suggestion above)*"))); reasoning != "" {
		details.WriteString(reasoning + "\n")
	}
	if hasContract {
		if uncertainty := ContractSentence(c.Uncertainty); uncertainty != "" {
			details.WriteString("\n" + uncertainty + "\n")
		}
		details.WriteString(howToVerify(c))
	}
	if agentLinkBase != "" {
		fmt.Fprintf(&details, "\nAgent prompt:\n```text\n%s\n```\n", agentPrompt(agentLinkBase, f, false))
	}
	b.WriteString("\n<details><summary>Reasoning and how to verify</summary>\n\n" + details.String() + "</details>\n")
	b.WriteString(inlineFooter(f, sourceTag, agentLinkBase, settleFooter))
	return b.String()
}
