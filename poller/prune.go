package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"pr-review-server/db"
	"pr-review-server/github"
)

// pruneStaleViaTeams is the reconciliation pass at the end of the
// reviewer-groups phase (issue #7). The main loop only ever writes via_teams
// for users currently entitled to see a PR, so when a user leaves a team or a
// team is removed from a PR, the stale row would otherwise live forever. This
// pass verifies every non-empty via_teams row for the polled PRs and clears
// the ones whose entitlement is gone, hiding the row when nothing else
// (authorship, review activity, user notes) keeps the PR on the user's
// dashboard.
//
// Two verification paths:
//   - PRs with fresh reviewer-group data this cycle are checked against the
//     entitled set the main loop just computed (covers team removal from the
//     PR, personal-request removal, and membership changes).
//   - Unchanged PRs get no reviewer-group fetch (poll economy), but their
//     team/personal-request lists can't have changed either — any reviewer
//     change bumps the PR's updated_at and makes it a changed PR. Only team
//     membership can drift, so the row's own via_teams is the team list and
//     membership is re-verified via the (cached) team-members lookup.
//
// A row whose entitlement cannot be verified (failed member fetch, unknown
// org) is kept: a false keep self-corrects on a later cycle, a false prune
// would wrongly hide a PR.
func (p *Poller) pruneStaleViaTeams(
	ctx context.Context,
	orgName string,
	allPRs []github.PullRequest,
	dbPRMap map[string]*db.PR,
	reviewerGroupsMap map[string]*github.ReviewerGroupData,
	entitled map[userPRViewKey]bool,
	failedSlugs map[string]bool,
	allUsers []db.User,
	changedPRs map[string]bool,
) {
	if orgName == "" || len(allUsers) == 0 {
		return
	}

	usersByID := make(map[int]db.User, len(allUsers))
	for _, user := range allUsers {
		usersByID[user.ID] = user
	}

	prIDToKey := make(map[int]string, len(allPRs))
	prIDs := make([]int, 0, len(allPRs))
	for _, pr := range allPRs {
		key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
		if existingPR, ok := dbPRMap[key]; ok && isOpenPRState(existingPR.PRState) {
			prIDToKey[existingPR.ID] = key
			prIDs = append(prIDs, existingPR.ID)
		}
	}

	rows, err := p.db.GetUserPRViewsWithViaTeams(prIDs)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to load user PR views for via_teams reconciliation: %v", err)
		return
	}

	var prunes []db.ViaTeamsPrune
	for _, row := range rows {
		// Entitlement was only computed for users in allUsers; skip anyone else.
		user, known := usersByID[row.UserID]
		if !known {
			continue
		}
		key, ok := prIDToKey[row.PRID]
		if !ok {
			continue
		}
		var viaTeams []string
		if err := json.Unmarshal([]byte(row.ViaTeams), &viaTeams); err != nil || len(viaTeams) == 0 {
			continue
		}

		stale := false
		if groupData, fresh := reviewerGroupsMap[key]; fresh {
			if prHasFailedSlug(groupData, failedSlugs) {
				continue
			}
			stale = !entitled[userPRViewKey{UserID: row.UserID, PRID: row.PRID}]
		} else {
			// Personal requests can't change without bumping updated_at, so an
			// unchanged PR with a __PERSONAL__ marker is still entitled.
			if containsPersonal(viaTeams) {
				continue
			}
			stale = true
			for _, team := range parseTeamNames(viaTeams) {
				members, err := p.getTeamMembers(ctx, orgName, slugifyTeamName(team))
				if err != nil {
					stale = false // unverifiable — keep
					break
				}
				if containsUser(members, user.GitHubUsername) {
					stale = false
					break
				}
			}
		}
		if !stale {
			continue
		}

		prunes = append(prunes, db.ViaTeamsPrune{
			UserID: row.UserID,
			PRID:   row.PRID,
			// Notes keep the row visible: hiding it would bury user-written
			// notes with no UI path back to them. A manual request is its own
			// claim on the row, independent of team entitlement.
			Hide: !row.IsAuthor && row.ReviewStatus == "" && row.Notes == "" && !row.ViaManual,
		})
	}

	if len(prunes) == 0 {
		return
	}
	if err := p.db.BatchPruneViaTeams(prunes); err != nil {
		log.Printf("[POLL] ERROR: Failed to prune stale via_teams: %v", err)
		return
	}
	for _, prune := range prunes {
		changedPRs[prIDToKey[prune.PRID]] = true
	}
	log.Printf("[POLL] Pruned stale via_teams on %d user PR view(s)", len(prunes))
}

// prHasFailedSlug reports whether any of the PR's current reviewer teams had a
// failed member fetch this cycle, making membership (and therefore the
// entitled set) unreliable for this PR.
func prHasFailedSlug(groupData *github.ReviewerGroupData, failedSlugs map[string]bool) bool {
	for _, team := range groupData.ReviewerGroups {
		slug, ok := groupData.TeamSlugs[team]
		if !ok {
			slug = slugifyTeamName(team)
		}
		if failedSlugs[slug] {
			return true
		}
	}
	return false
}

// containsUser reports whether username is in members (case-insensitive,
// matching GitHub login semantics).
func containsUser(members []string, username string) bool {
	for _, member := range members {
		if strings.EqualFold(member, username) {
			return true
		}
	}
	return false
}
