package poller

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/service"
)

// priorFindingsLedger is what the agent's "already raised" block reads: the
// publication ledger and the author replies recorded under its comments.
type priorFindingsLedger interface {
	publisher.Ledger
	ListPublishedRepliesForPR(owner, repo string, number int) ([]db.PublishedReply, error)
}

// priorFindingsFor loads what PRism already posted on the PR for the agent
// prompt, followed by the points the author settled recently in the repo
// (PUBLISH_CROSS_PR_SETTLED). Any failure yields nothing: the block is
// advisory and a review must not wait on it.
func (p *Poller) priorFindingsFor(owner, repo string, number int, author string) []service.PriorFinding {
	ledger, ok := p.db.(priorFindingsLedger)
	if !ok {
		return nil
	}
	rows, err := ledger.GetPublishedFindingsForPR(owner, repo, number)
	if err != nil {
		log.Printf("[REVIEWER] %s/%s#%d: prior findings: load ledger: %v", owner, repo, number, err)
		return nil
	}
	replies, err := ledger.ListPublishedRepliesForPR(owner, repo, number)
	if err != nil {
		log.Printf("[REVIEWER] %s/%s#%d: prior findings: load replies: %v", owner, repo, number, err)
		replies = nil
	}
	priors := PriorFindingsFromLedger(rows, replies)
	store, ok := p.db.(publisher.SettledPointStore)
	if !ok || p.crossPRSettledOff || strings.TrimSpace(author) == "" {
		return priors
	}
	now := time.Now().UTC()
	points, err := store.ListSettledPoints(owner, repo, author, now.Add(-publisher.SettledWindow), now)
	if err != nil {
		log.Printf("[REVIEWER] %s/%s#%d: prior findings: load settled points: %v", owner, repo, number, err)
		return priors
	}
	// First, so the prompt's row cap cuts ledger rows before the points
	// the agent must not raise again.
	return append(SettledElsewherePriors(points, number), priors...)
}

// settledElsewhereMax bounds the settled points the prompt lists, newest
// first. The list is advisory: the publisher enforces every point in the
// window whether or not the agent saw it, so an older point the agent
// raises again is still dropped there.
const settledElsewhereMax = 10

// SettledElsewherePriors turns the author's settled points from other
// pull requests into prompt entries: the file, the headline and the
// author's reason, newest first, at most settledElsewhereMax. A point the
// author settled on one of PRism's own threads of this pull request is
// its dismissed ledger row, listed with the rows; a thread another
// reviewer opened here that the author settled is listed like one from
// elsewhere. (A settled ledger row is not a point: it marks a finding of
// this PR that one of these points covered, and that point is listed.) The headline and reason are what a reviewer and the author
// wrote, as the ledger rows' author replies already are; the agent reads
// the whole PR conversation anyway, and the rules name these lines as
// context, never as evidence or instruction.
func SettledElsewherePriors(points []db.SettledPoint, number int) []service.PriorFinding {
	sorted := append([]db.SettledPoint(nil), points...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].SettledAt.After(sorted[j].SettledAt) })
	var out []service.PriorFinding
	for _, pt := range sorted {
		if (pt.PRNumber == number && pt.Fingerprint != "") || pt.Source == db.SettledSourceDeferred {
			continue
		}
		headline := pt.Headline
		if headline == "" {
			headline = priorHeadline(pt.Text)
		}
		out = append(out, service.PriorFinding{ID: fmt.Sprintf("settled:%d:%d", pt.PRNumber, pt.CommentID), State: db.PublishedStateSettled, File: pt.File,
			Headline: headline, SettledOnPR: pt.PRNumber, Reason: pt.Reason})
		if len(out) == settledElsewhereMax {
			break
		}
	}
	return out
}

// priorStateOrder lists the rows an agent most needs to know about first:
// what is still open, then what the author settled, then what another
// reviewer owns, then what was judged fixed.
var priorStateOrder = map[string]int{
	db.PublishedStateOpen:      0,
	db.PublishedStateContested: 1,
	db.PublishedStateDismissed: 2,
	db.PublishedStateSettled:   2,
	db.PublishedStateExternal:  3,
	db.PublishedStateFixed:     4,
	db.PublishedStateResolved:  4,
}

// PriorFindingsFromLedger turns the PR's ledger rows and recorded author
// replies into the list the agent is shown: finding and annotation rows
// only (a settled row stays out: SettledElsewherePriors lists the point it
// names), the legacy resolved state read as fixed, the cited lines recovered
// from the fingerprint's bucket, the headline taken from the stored prose,
// and the thread's verdicts in the order they were handled.
func PriorFindingsFromLedger(rows []db.PublishedFinding, replies []db.PublishedReply) []service.PriorFinding {
	verdicts := priorVerdicts(replies)
	var out []service.PriorFinding
	for _, row := range rows {
		if (row.Kind != db.PublishedKindFinding && row.Kind != db.PublishedKindAnnotation) || row.State == db.PublishedStateSettled {
			continue
		}
		file, bucket := payload.FingerprintParts(row.Fingerprint)
		prior := service.PriorFinding{
			ID:       row.Fingerprint,
			State:    priorState(row.State),
			Severity: strings.ToLower(strings.TrimSpace(row.Severity)),
			File:     file,
			Headline: priorHeadline(row.CommentText),
			Verdicts: verdicts[row.Fingerprint],
		}
		switch {
		case bucket > 0:
			prior.LineFrom, prior.LineTo = bucket*payload.LineBucket, bucket*payload.LineBucket+payload.LineBucket-1
		case row.CommentID != 0:
			// An inline comment in bucket 0 sat on a line, not the whole file.
			prior.LineFrom, prior.LineTo = 1, payload.LineBucket-1
		}
		out = append(out, prior)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := priorStateOrder[out[i].State], priorStateOrder[out[j].State]; a != b {
			return a < b
		}
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].LineFrom < out[j].LineFrom
	})
	return out
}

func priorState(state string) string {
	switch state {
	case db.PublishedStateResolved:
		return db.PublishedStateFixed
	case "":
		return db.PublishedStateOpen
	}
	return state
}

// priorHeadline is the first sentence of the stored prose, the same title a
// comment without a contract headline is rendered under.
func priorHeadline(commentText string) string {
	return strings.TrimSpace(publisher.FirstSentence(publisher.StripRendered(commentText)))
}

// priorVerdicts summarises each thread's recorded replies by fingerprint:
// what the author's reply amounted to and what PRism decided, in processing
// order, each kind once.
func priorVerdicts(replies []db.PublishedReply) map[string][]string {
	sorted := append([]db.PublishedReply(nil), replies...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ProcessedAt.Before(sorted[j].ProcessedAt) })
	out := map[string][]string{}
	seen := map[string]map[string]bool{}
	add := func(fp, verdict string) {
		if verdict == "" {
			return
		}
		if seen[fp] == nil {
			seen[fp] = map[string]bool{}
		}
		if seen[fp][verdict] {
			return
		}
		seen[fp][verdict] = true
		out[fp] = append(out[fp], verdict)
	}
	for _, r := range sorted {
		if r.Fingerprint == "" {
			continue
		}
		add(r.Fingerprint, authorVerdict(r))
		add(r.Fingerprint, prismVerdict(r))
		if r.DeferredTo != "" {
			add(r.Fingerprint, "deferred to "+r.DeferredTo)
		}
	}
	return out
}

func authorVerdict(r db.PublishedReply) string {
	switch publisher.ReplyClass(r.Class) {
	case publisher.ReplyVerdict:
		return "author: intentional or won't fix"
	case publisher.ReplyResolution:
		return "author: claims fixed"
	case publisher.ReplyPushback:
		return "author pushed back"
	case publisher.ReplyQuestion:
		return "author asked a question"
	}
	return ""
}

func prismVerdict(r db.PublishedReply) string {
	switch r.Decision {
	case publisher.DecisionConcede:
		return "PRism conceded"
	case publisher.DecisionWithdraw:
		return "PRism withdrew"
	case publisher.DecisionHold:
		return "PRism held"
	case publisher.DecisionAnswer:
		return "PRism answered"
	}
	return ""
}
