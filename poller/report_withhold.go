package poller

import (
	"os"
	"strings"

	"pr-review-server/pkg/publisher"
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

// reportWithhold names the findings the publisher would hold off the PR this
// round (verifier hold or drop, low value, gate BAD, hygiene and claim holds),
// so the report shows what GitHub shows. Ledger dedupe and caps are not known
// before publishing and stay visible.
func (p *Poller) reportWithhold(result *service.ReviewResult) func(types.LineComment) bool {
	if !reportHideWithheld() || result == nil {
		return nil
	}
	pol := p.publishPolicy()
	pl := payload.Build("", "", 0, "", result.Comments, result.Diff, result.FileContents)
	held := map[string]bool{}
	for _, f := range pl.Findings {
		if f.Active && publisher.HoldReason(f, pol) != "" {
			held[f.ID] = true
		}
	}
	return func(c types.LineComment) bool {
		return held[payload.Fingerprint(c.FilePath, c.LineNumber, c.CommentBody)]
	}
}
