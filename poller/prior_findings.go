package poller

import (
	"log"
	"sort"
	"strings"

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
// prompt. Any failure yields nothing: the block is advisory and a review
// must not wait on it.
func (p *Poller) priorFindingsFor(owner, repo string, number int) []service.PriorFinding {
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
	return PriorFindingsFromLedger(rows, replies)
}

// priorStateOrder lists the rows an agent most needs to know about first:
// what is still open, then what the author settled, then what another
// reviewer owns, then what was judged fixed.
var priorStateOrder = map[string]int{
	db.PublishedStateOpen:      0,
	db.PublishedStateContested: 1,
	db.PublishedStateDismissed: 2,
	db.PublishedStateExternal:  3,
	db.PublishedStateFixed:     4,
	db.PublishedStateResolved:  4,
}

// PriorFindingsFromLedger turns the PR's ledger rows and recorded author
// replies into the list the agent is shown: finding and annotation rows
// only, the legacy resolved state read as fixed, the cited lines recovered
// from the fingerprint's bucket, the headline taken from the stored prose,
// and the thread's verdicts in the order they were handled.
func PriorFindingsFromLedger(rows []db.PublishedFinding, replies []db.PublishedReply) []service.PriorFinding {
	verdicts := priorVerdicts(replies)
	var out []service.PriorFinding
	for _, row := range rows {
		if row.Kind != db.PublishedKindFinding && row.Kind != db.PublishedKindAnnotation {
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
