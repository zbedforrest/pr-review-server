package poller

import (
	"fmt"
	"strings"

	"pr-review-server/db"
	"pr-review-server/github"
)

// mergePRLists combines PRs from multiple sources, removing duplicates.
// PRs are identified by owner/repo/number.
func mergePRLists(reviewPRs, myPRs []github.PullRequest, dbPRs []db.PR) []github.PullRequest {
	seen := make(map[string]bool)
	var result []github.PullRequest

	// Helper to add PRs with deduplication
	addPRs := func(prs []github.PullRequest) {
		for _, pr := range prs {
			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			if !seen[key] {
				seen[key] = true
				result = append(result, pr)
			}
		}
	}

	// Add PRs in order of priority: review requests, my PRs, then database PRs
	addPRs(reviewPRs)
	addPRs(myPRs)

	// Convert database PRs to github.PullRequest format and add
	for _, dbPR := range dbPRs {
		key := fmt.Sprintf("%s/%s/%d", dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber)
		if !seen[key] {
			seen[key] = true
			result = append(result, buildPRFromDB(dbPR))
		}
	}

	return result
}

// groupPRsByRepo groups PRs by their repository key (owner/repo).
func groupPRsByRepo(prs []github.PullRequest) map[string][]github.PullRequest {
	prsByRepo := make(map[string][]github.PullRequest)
	for _, pr := range prs {
		repoKey := fmt.Sprintf("%s/%s", pr.Owner, pr.Repo)
		prsByRepo[repoKey] = append(prsByRepo[repoKey], pr)
	}
	return prsByRepo
}

// buildPRFromDB converts a database PR record to a github.PullRequest.
func buildPRFromDB(dbPR db.PR) github.PullRequest {
	return github.PullRequest{
		Owner:     dbPR.RepoOwner,
		Repo:      dbPR.RepoName,
		Number:    dbPR.PRNumber,
		CommitSHA: dbPR.LastCommitSHA,
		Title:     dbPR.Title,
		Author:    dbPR.Author,
		URL:       fmt.Sprintf("https://github.com/%s/%s/pull/%d", dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber),
		CreatedAt: dbPR.CreatedAt,
		Draft:     dbPR.Draft,
	}
}

// slugifyTeamName derives a GitHub team slug from its display name.
// GitHub slugs are lowercase with non-alphanumeric characters replaced by hyphens.
// e.g. "Viewer Team" -> "viewer-team", "Platform Engineers" -> "platform-engineers"
func slugifyTeamName(name string) string {
	slug := strings.ToLower(name)
	var result []byte
	lastWasHyphen := false
	for _, c := range slug {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result = append(result, byte(c))
			lastWasHyphen = false
		} else if !lastWasHyphen {
			result = append(result, '-')
			lastWasHyphen = true
		}
	}
	return strings.Trim(string(result), "-")
}

// resolveTeamReviewStatuses determines the review status of each team for a PR.
// It combines the currently-pending teams (from reviewRequests) with the
// previously-known teams (high-water-mark from DB) and checks if any team
// member has submitted a review.
//
// When currentUser is non-empty, teams that the user belongs to and are not yet
// satisfied get prefixed with "my_" (e.g. "my_pending", "my_changes_requested")
// so the frontend can highlight them.
//
// Returns a map of team name -> status ("pending", "approved", "changes_requested",
// "my_pending", "my_changes_requested")
func resolveTeamReviewStatuses(
	currentTeams []string,
	previousTeams []string,
	teamMembers map[string][]string,
	teamSlugs map[string]string,
	userReviews map[string]string,
	currentUser string,
) map[string]string {
	// Union currentTeams and previousTeams (high-water-mark)
	allTeams := make(map[string]bool)
	for _, t := range currentTeams {
		if t != "__PERSONAL__" {
			allTeams[t] = true
		}
	}
	for _, t := range previousTeams {
		if t != "__PERSONAL__" {
			allTeams[t] = true
		}
	}

	result := make(map[string]string)
	for team := range allTeams {
		slug, hasSlug := teamSlugs[team]
		if !hasSlug {
			// Derive slug from team name as fallback. This handles teams that
			// were already satisfied before we saw them in reviewRequests.
			slug = slugifyTeamName(team)
		}

		members, hasMembers := teamMembers[slug]
		if !hasMembers {
			result[team] = "pending"
			continue
		}

		// Check member reviews: changes_requested takes priority over approved
		status := "pending"
		isMine := false
		for _, member := range members {
			if currentUser != "" && strings.EqualFold(member, currentUser) {
				isMine = true
			}
			reviewState, hasReview := userReviews[member]
			if !hasReview {
				continue
			}
			switch reviewState {
			case "CHANGES_REQUESTED":
				status = "changes_requested"
			case "APPROVED":
				if status != "changes_requested" {
					status = "approved"
				}
			}
		}
		// Prefix unsatisfied teams the user belongs to so the frontend can highlight them
		if isMine && status != "approved" {
			status = "my_" + status
		}
		result[team] = status
	}

	return result
}

// parseTeamNames extracts team names from via_teams entries, stripping status suffixes.
// e.g. ["platform:approved", "security:pending", "__PERSONAL__"] -> ["platform", "security"]
func parseTeamNames(viaTeams []string) []string {
	var names []string
	for _, entry := range viaTeams {
		if entry == "__PERSONAL__" {
			continue
		}
		if idx := strings.Index(entry, ":"); idx >= 0 {
			names = append(names, entry[:idx])
		} else {
			names = append(names, entry)
		}
	}
	return names
}

// containsPersonal checks if a via_teams list contains the __PERSONAL__ marker.
func containsPersonal(teams []string) bool {
	for _, t := range teams {
		if t == "__PERSONAL__" {
			return true
		}
	}
	return false
}

// shouldUpdateViaTeams returns true when the via_teams data should be written to
// the database. Empty results are treated as "query missed the data" rather than
// "teams were legitimately removed" — if a PR appears in review requests, there
// must have been at least one REVIEW_REQUESTED_EVENT in the timeline.
func shouldUpdateViaTeams(newViaTeams []string) bool {
	return len(newViaTeams) > 0
}

// collectUniqueSlugs collects all unique team slugs from a reviewer groups map.
func collectUniqueSlugs(reviewerGroupsMap map[string]*github.ReviewerGroupData) map[string]bool {
	slugs := make(map[string]bool)
	for _, data := range reviewerGroupsMap {
		for _, slug := range data.TeamSlugs {
			slugs[slug] = true
		}
	}
	return slugs
}
