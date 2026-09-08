package server

import (
	"fmt"
	"log"
	"strings"

	"pr-review-server/db"
)

// publishedSummaryLister is the narrow ledger capability the dashboard needs;
// *db.GormDB implements it, mocks need not.
type publishedSummaryLister interface {
	ListPublishedSummaries() ([]db.PublishedFinding, error)
}

func publishedKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", strings.ToLower(owner), strings.ToLower(repo), number)
}

// publishedSummaries maps every PR that has a GitHub summary comment to its
// ledger row. Errors degrade to "nothing published" so the PR list never fails
// because of the badge.
func (s *Server) publishedSummaries() map[string]db.PublishedFinding {
	out := map[string]db.PublishedFinding{}
	lister, ok := s.db.(publishedSummaryLister)
	if !ok {
		return out
	}
	rows, err := lister.ListPublishedSummaries()
	if err != nil {
		log.Printf("[API] published summaries unavailable: %v", err)
		return out
	}
	for _, row := range rows {
		out[publishedKey(row.RepoOwner, row.RepoName, row.PRNumber)] = row
	}
	return out
}
