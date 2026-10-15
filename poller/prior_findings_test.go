package poller

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/runconfig"
)

func TestPriorFindingsFromLedger_RowsBecomeLabelledPriors(t *testing.T) {
	open := payload.Fingerprint("pkg/a.go", 123, "The handler writes to a nil map. Second sentence.")
	settled := payload.Fingerprint("pkg/b.go", 4, "<!-- prism:finding:x -->\n**Behavior change · Config read twice**\n\nConfig key is read twice per request.")
	fixed := payload.Fingerprint("pkg/c.go", 0, "Whole-file note")
	rows := []db.PublishedFinding{
		{Kind: db.PublishedKindSummary, Fingerprint: "summary", State: db.PublishedStateOpen},
		{Kind: db.PublishedKindFinding, Fingerprint: fixed, State: db.PublishedStateResolved, Severity: "LOW", CommentText: "Whole-file note"},
		{Kind: db.PublishedKindAnnotation, Fingerprint: settled, State: db.PublishedStateDismissed, Severity: "medium", CommentText: "<!-- prism:finding:x -->\n**Behavior change · Config read twice**\n\nConfig key is read twice per request."},
		{Kind: db.PublishedKindFinding, Fingerprint: open, State: db.PublishedStateOpen, Severity: "critical", CommentText: "The handler writes to a nil map. Second sentence."},
	}
	now := time.Now()
	replies := []db.PublishedReply{
		{Fingerprint: settled, Class: string(publisher.ReplyVerdict), ProcessedAt: now.Add(2 * time.Minute)},
		{Fingerprint: settled, Class: string(publisher.ReplyPushback), Decision: publisher.DecisionHold, ProcessedAt: now},
		{Fingerprint: settled, Class: string(publisher.ReplyPushback), Decision: publisher.DecisionConcede, DeferredTo: "ACME-12", ProcessedAt: now.Add(time.Minute)},
		{Fingerprint: "unknown", Class: string(publisher.ReplyQuestion), ProcessedAt: now},
	}
	got := PriorFindingsFromLedger(rows, replies)
	require.Len(t, got, 3, "the summary row is not a finding")

	assert.Equal(t, open, got[0].ID)
	assert.Equal(t, "open", got[0].State)
	assert.Equal(t, "critical", got[0].Severity)
	assert.Equal(t, "pkg/a.go", got[0].File)
	assert.Equal(t, 120, got[0].LineFrom)
	assert.Equal(t, 129, got[0].LineTo)
	assert.Equal(t, "The handler writes to a nil map.", got[0].Headline)
	assert.Empty(t, got[0].Verdicts)

	assert.Equal(t, settled, got[1].ID)
	assert.Equal(t, "dismissed", got[1].State)
	assert.Equal(t, "medium", got[1].Severity)
	assert.Equal(t, 0, got[1].LineFrom, "bucket 0 is a whole-file note")
	assert.False(t, strings.Contains(got[1].Headline, "prism:finding"), "markers are stripped: %q", got[1].Headline)
	assert.Equal(t, []string{"author pushed back", "PRism held", "PRism conceded", "deferred to ACME-12", "author: intentional or won't fix"}, got[1].Verdicts)

	assert.Equal(t, fixed, got[2].ID)
	assert.Equal(t, "fixed", got[2].State, "legacy resolved reads as fixed")
	assert.Equal(t, "low", got[2].Severity)
}

func TestPriorFindingsFromLedger_OrdersOpenThenSettledThenFixed(t *testing.T) {
	rows := []db.PublishedFinding{
		{Kind: db.PublishedKindFinding, Fingerprint: "z.go:3:1", State: db.PublishedStateFixed},
		{Kind: db.PublishedKindFinding, Fingerprint: "b.go:2:2", State: db.PublishedStateExternal},
		{Kind: db.PublishedKindFinding, Fingerprint: "b.go:9:3", State: db.PublishedStateOpen},
		{Kind: db.PublishedKindFinding, Fingerprint: "a.go:1:4", State: db.PublishedStateOpen},
		{Kind: db.PublishedKindFinding, Fingerprint: "c.go:0:5", State: db.PublishedStateContested},
		{Kind: db.PublishedKindFinding, Fingerprint: "c.go:0:6", State: db.PublishedStateDismissed},
	}
	got := PriorFindingsFromLedger(rows, nil)
	var order []string
	for _, p := range got {
		order = append(order, p.ID)
	}
	assert.Equal(t, []string{"a.go:1:4", "b.go:9:3", "c.go:0:5", "c.go:0:6", "b.go:2:2", "z.go:3:1"}, order)
}

type priorLedgerDB struct {
	*MockDatabase
	rows    []db.PublishedFinding
	replies []db.PublishedReply
	calls   int
}

func (l *priorLedgerDB) UpsertPublishedFinding(*db.PublishedFinding) error { return nil }

func (l *priorLedgerDB) GetPublishedFindingsForPR(_, _ string, _ int) ([]db.PublishedFinding, error) {
	l.calls++
	return l.rows, nil
}

func (l *priorLedgerDB) ListPublishedRepliesForPR(_, _ string, _ int) ([]db.PublishedReply, error) {
	return l.replies, nil
}

func TestAgentConfigForExecution_PriorFindingsOnlyBehindTheFlag(t *testing.T) {
	ledger := &priorLedgerDB{MockDatabase: NewMockDatabase(), rows: []db.PublishedFinding{
		{Kind: db.PublishedKindFinding, Fingerprint: "a.go:1:abc", State: db.PublishedStateOpen, Severity: "medium", CommentText: "Point one."},
	}}
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.db = ledger
	job := liteReviewJob(t, "run-51000000000000000000000000000031", runconfig.ProfileLiteClassic)

	off := p.agentConfigForExecution(&reviewExecution{Job: job}, "tok")
	assert.Nil(t, off.PriorFindings, "flag off: nothing is loaded")
	assert.Zero(t, ledger.calls, "flag off: the ledger is not read")

	p.cfg.AgentPriorFindings = true
	on := p.agentConfigForExecution(&reviewExecution{Job: job}, "tok")
	require.Len(t, on.PriorFindings, 1)
	assert.Equal(t, "a.go:1:abc", on.PriorFindings[0].ID)
	assert.Equal(t, "Point one.", on.PriorFindings[0].Headline)
	assert.Equal(t, 1, ledger.calls)
}

func TestPriorFindingsFor_NothingWithoutALedger(t *testing.T) {
	p := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	p.cfg.AgentPriorFindings = true
	assert.Nil(t, p.priorFindingsFor("acme", "example", 1, "dev"))
	unavailable := newTestPoller(NewMockGitHubClient(), NewMockDatabase())
	unavailable.db = &ledgerUnavailable{NewMockDatabase()}
	unavailable.cfg.AgentPriorFindings = true
	assert.Nil(t, unavailable.priorFindingsFor("acme", "example", 1, "dev"), "a ledger without replies does not qualify")
}

func TestSettledElsewherePriors_ListsTheAuthorsSettledPointsNewestFirst(t *testing.T) {
	at := time.Date(2026, 10, 14, 15, 3, 0, 0, time.UTC)
	points := []db.SettledPoint{
		{PRNumber: 596, CommentID: 4001, File: "store/models.py", Headline: "Buyer lookup hardcodes purchaser_id", Reason: "intentional, the backfill is done", SettledAt: at},
		{PRNumber: 668, CommentID: 5001, Fingerprint: "store/api/views.py:28:abc", File: "store/api/views.py", Headline: "Own thread on this PR", SettledAt: at.Add(time.Hour)},
		{PRNumber: 668, CommentID: 5002, File: "store/api/views.py", Headline: "Older purchases cannot be deleted", Reason: "The backfill is done in prod.", SettledAt: at.Add(2 * time.Hour)},
		{PRNumber: 500, CommentID: 3001, File: "web/app.tsx", Text: "The modal closes on escape. Second sentence.", Reason: "by design", SettledAt: at.Add(-24 * time.Hour)},
	}
	got := SettledElsewherePriors(points, 668)
	require.Len(t, got, 3, "the point PRism posted on this PR is listed from the ledger instead")
	assert.Equal(t, "settled:668:5002", got[0].ID)
	assert.Equal(t, 668, got[0].SettledOnPR)
	assert.Equal(t, "settled:596:4001", got[1].ID)
	assert.Equal(t, db.PublishedStateSettled, got[1].State)
	assert.Equal(t, "Buyer lookup hardcodes purchaser_id", got[1].Headline)
	assert.Equal(t, "intentional, the backfill is done", got[1].Reason)
	assert.Equal(t, "The modal closes on escape.", got[2].Headline, "a point without a headline is titled by its first sentence")

	var many []db.SettledPoint
	for i := 0; i < settledElsewhereMax+5; i++ {
		many = append(many, db.SettledPoint{PRNumber: 100 + i, CommentID: int64(i + 1), File: "a.go", Headline: "x", SettledAt: at.Add(time.Duration(i) * time.Minute)})
	}
	assert.Len(t, SettledElsewherePriors(many, 1), settledElsewhereMax)
}
