package poller

import (
	"fmt"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
)

// ciMergeStateEveryCycles is the longest a watched PR goes without a
// merge-state fetch when neither its head nor its CI state changes.
const ciMergeStateEveryCycles = 5

// ciWatcherActiveWindow bounds how long ago a user last logged in for their
// dashboard views to keep merge state fresh.
const ciWatcherActiveWindow = 30 * 24 * time.Hour

// ciMergeMark records the last merge-state fetch of one PR: the cycle it ran
// in and the head and CI state observed, so the next selection can tell
// whether anything that moves the merge box has changed since.
type ciMergeMark struct {
	cycle   int
	head    string
	ciState string
}

// mergeStateDue reports whether a watched PR gets merge state this cycle: on
// first sight, on a head or CI state change since the last fetch, or when the
// cadence cap is reached.
func mergeStateDue(mark ciMergeMark, marked bool, cycle int, head, ciState string) bool {
	if !marked {
		return true
	}
	return mark.head != head || mark.ciState != ciState || cycle-mark.cycle >= ciMergeStateEveryCycles
}

// isExcludedCIAuthor reports whether a PR author never gets merge-state
// requests: GitHub App logins ("[bot]" suffix) and the configured list.
func isExcludedCIAuthor(login string, excluded map[string]bool) bool {
	l := strings.ToLower(strings.TrimSpace(login))
	if l == "" {
		return false
	}
	return strings.HasSuffix(l, "[bot]") || excluded[l] || excluded[strings.TrimSuffix(l, "[bot]")]
}

// ciSelectOptions carries the per-cycle inputs of selectCIStatusPRs.
type ciSelectOptions struct {
	// watched is the set of PR IDs some active user's dashboard renders; nil
	// means the lookup failed and every known open PR counts as visible.
	watched map[int]bool
	// excludedAuthors are lowercase logins whose PRs skip merge state.
	excludedAuthors map[string]bool
	marks           map[string]ciMergeMark
	cycle           int
	fullRefresh     bool
}

// ciSelection is the outcome of selectCIStatusPRs: the PRs to query plus the
// per-reason counts logged with every cycle.
type ciSelection struct {
	prs []github.PRInfo
	// open counts open PRs; visible those on an active dashboard; botExcluded
	// the visible ones whose author is excluded; watched = visible -
	// botExcluded; due the watched ones fetched with merge state this cycle.
	open, visible, botExcluded, watched, due, unwatched, closedSkipped, checksOnly int
}

func (s ciSelection) String() string {
	return fmt.Sprintf("open=%d watched=%d (visible=%d bot-excluded=%d due=%d) unwatched-skipped=%d closed-skipped=%d checks-only=%d",
		s.open, s.watched, s.visible, s.botExcluded, s.due, s.unwatched, s.closedSkipped, s.checksOnly)
}

// selectCIStatusPRs picks the PRs whose CI status is fetched this cycle.
//
// A row is open only if GitHub returned it this cycle (ghKeys), it is not yet
// in the database, or its stored pr_state says so; an empty pr_state is not a
// vote for open here, unlike isOpenPRState, because merge state is the
// expensive part of the query. A PR not yet in the database has no views, so
// it is queried checks-only on first sight. Open PRs on an active dashboard
// are watched unless their author is excluded; watched PRs get merge state
// when due (see mergeStateDue) and checks only otherwise. Open PRs nobody
// watches are skipped: author views are synced before this selection and
// reviewer views after it, so a PR whose only watcher is a reviewer joins on
// the next cycle. Excluded-author, closed, merged and unknown-state rows keep
// their stored merge fields and are queried for checks only when their CI
// state is empty or on a full-refresh cycle.
func selectCIStatusPRs(allPRs []github.PullRequest, dbPRMap map[string]*db.PR, ghKeys map[string]bool, opts ciSelectOptions) ciSelection {
	sel := ciSelection{prs: make([]github.PRInfo, 0, len(allPRs))}
	checksOnlyIfStale := func(info github.PRInfo, ciState string) {
		if ciState != "" && !opts.fullRefresh {
			sel.closedSkipped++
			return
		}
		sel.checksOnly++
		sel.prs = append(sel.prs, info)
	}
	for _, pr := range allPRs {
		key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
		info := github.PRInfo{Owner: pr.Owner, Repo: pr.Repo, Number: pr.Number}
		dbPR, known := dbPRMap[key]
		if !known {
			sel.open++
			sel.checksOnly++
			sel.prs = append(sel.prs, info)
			continue
		}
		if !ghKeys[key] && !strings.EqualFold(dbPR.PRState, "open") {
			checksOnlyIfStale(info, dbPR.CIState)
			continue
		}
		sel.open++
		if opts.watched != nil && !opts.watched[dbPR.ID] {
			sel.unwatched++
			continue
		}
		sel.visible++
		author := pr.Author
		if author == "" {
			author = dbPR.Author
		}
		if isExcludedCIAuthor(author, opts.excludedAuthors) {
			sel.botExcluded++
			if dbPR.CIState == "" || opts.fullRefresh {
				sel.checksOnly++
				sel.prs = append(sel.prs, info)
			}
			continue
		}
		sel.watched++
		mark, marked := opts.marks[key]
		if mergeStateDue(mark, marked, opts.cycle, pr.CommitSHA, dbPR.CIState) {
			sel.due++
			info.IncludeMergeState = true
		} else {
			sel.checksOnly++
		}
		sel.prs = append(sel.prs, info)
	}
	return sel
}

// recordMergeStateFetches marks every PR whose merge state was requested this
// cycle and answered, and forgets PRs no longer tracked so the map cannot grow
// past the tracked set.
func recordMergeStateFetches(marks map[string]ciMergeMark, allPRs []github.PullRequest, requested []github.PRInfo, results map[string]*github.CIStatus, cycle int) {
	withMerge := make(map[string]bool, len(requested))
	for _, info := range requested {
		if info.IncludeMergeState {
			withMerge[fmt.Sprintf("%s/%s/%d", info.Owner, info.Repo, info.Number)] = true
		}
	}
	tracked := make(map[string]bool, len(allPRs))
	for _, pr := range allPRs {
		key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
		tracked[key] = true
		if status, ok := results[key]; ok && withMerge[key] {
			marks[key] = ciMergeMark{cycle: cycle, head: pr.CommitSHA, ciState: status.State}
		}
	}
	for key := range marks {
		if !tracked[key] {
			delete(marks, key)
		}
	}
}
