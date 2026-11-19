package poller

import (
	"os"
	"strings"

	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/html"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
)

// ReportHideWithheldEnv turns off leaving publisher-held findings out of the
// HTML report ("false", "0", "off", "no"); on by default.
const ReportHideWithheldEnv = "REPORT_HIDE_WITHHELD"

func reportHideWithheld() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ReportHideWithheldEnv))) {
	case "false", "0", "off", "no":
		return false
	}
	return true
}

// reportPublishView works out what the publisher will make of the review so
// the report shows what GitHub shows: the findings it holds off the PR
// (verifier hold or drop, low value, gate BAD, hygiene and claim holds) and
// the merge confidence over the rest. Ledger dedupe and caps are not known
// before publishing and stay visible.
func (p *Poller) reportPublishView(result *service.ReviewResult) html.PublishView {
	if !reportHideWithheld() || result == nil {
		return html.PublishView{}
	}
	pol := p.publishPolicy()
	pl := payload.Build("", "", 0, "", result.Comments, result.Diff, result.FileContents)
	held := map[string]bool{}
	var posted []payload.Finding
	for _, f := range pl.Findings {
		if f.Active && publisher.HoldReason(f, pol) != "" {
			held[f.ID] = true
			continue
		}
		posted = append(posted, f)
	}
	return html.PublishView{
		Withhold: func(c types.LineComment) bool {
			return held[payload.Fingerprint(c.FilePath, c.LineNumber, c.CommentBody)]
		},
		Confidence: publisher.Confidence(posted, result.Checks.ChecksViolated > 0),
	}
}
