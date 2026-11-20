package server

import (
	"bytes"
	"encoding/json"
	"html"
	"log"
	"strings"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

const closedPRBanner = "Reviewed at %s. This pull request is closed, so this is the last review PRism ran."

func movedPRBanner(inFlight bool) string {
	if inFlight {
		return "Reviewed at %s. The pull request has newer commits and a new review is running."
	}
	return "Reviewed at %s. The pull request has newer commits that were not reviewed, usually because the diff did not change."
}

// closedPRFromRuns stands in for a PR row the poller removed when the PR
// closed: the review runs and their stored reports outlive the row. With a
// run id it serves that run, otherwise the newest completed run with a report.
func (s *Server) closedPRFromRuns(owner, repo string, prNumber int, runID string) *db.PR {
	var runs []db.ReviewRun
	if runID != "" {
		if !isSafeRunID(runID) {
			return nil
		}
		run, err := s.db.GetReviewRun(runID)
		if err != nil || run == nil {
			return nil
		}
		runs = []db.ReviewRun{*run}
	} else {
		found, err := s.db.ListReviewRuns(db.ReviewRunFilter{
			RepoOwner: owner, RepoName: repo, PRNumber: prNumber,
			Status: db.ReviewRunStatusCompleted, Limit: 20,
		})
		if err != nil {
			log.Printf("[API/review] review runs for closed %s/%s#%d: %v", owner, repo, prNumber, err)
			return nil
		}
		runs = found
	}
	for _, run := range runs {
		if !strings.EqualFold(run.RepoOwner, owner) || !strings.EqualFold(run.RepoName, repo) ||
			run.PRNumber != prNumber || run.Status != db.ReviewRunStatusCompleted || run.HTMLPath == "" {
			continue
		}
		info := payload.ReviewRunInfo{RunID: run.RunID, HTMLPath: run.HTMLPath, JSONPath: run.JSONPath}
		if run.CompletedAt != nil {
			info.CompletedAt = *run.CompletedAt
		}
		infoJSON, err := json.Marshal(info)
		if err != nil {
			return nil
		}
		return &db.PR{
			RepoOwner: run.RepoOwner, RepoName: run.RepoName, PRNumber: run.PRNumber,
			Status: "closed", LastCommitSHA: run.CommitSHA, LastReviewedAt: run.CompletedAt,
			ReviewHTMLPath: run.HTMLPath, ReviewRunID: run.RunID, ReviewRunJSON: string(infoJSON),
			CriticalCount: run.CriticalCount, MediumCount: run.MediumCount, LowCount: run.LowCount,
		}
	}
	return nil
}

// withBanner puts a one-line notice at the top of a stored report without
// re-rendering it; stored run objects are immutable.
func withBanner(page []byte, text string) []byte {
	notice := []byte(`<div role="status" style="background:#3d2e00;color:#f0d78c;border-bottom:1px solid #6b5300;padding:10px 16px;font:14px/1.4 -apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif">` + html.EscapeString(text) + `</div>`)
	at := bytes.Index(bytes.ToLower(page), []byte("<body"))
	if at < 0 {
		return append(notice, page...)
	}
	end := bytes.IndexByte(page[at:], '>')
	if end < 0 {
		return append(notice, page...)
	}
	at += end + 1
	out := make([]byte, 0, len(page)+len(notice))
	out = append(out, page[:at]...)
	out = append(out, notice...)
	return append(out, page[at:]...)
}

func isSafeRepoSegment(s string) bool {
	if s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
