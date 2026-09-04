package publisher

import (
	"context"
	"strings"
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

type fakeGitHub struct {
	reviews        []fakeReview
	issueCreates   []string
	issueEdits     map[int64]string
	nextCommentID  int64
	nextIssueID    int64
	nextReviewID   int64
	createReviewSH string
}

type fakeReview struct {
	sha      string
	body     string
	comments []ReviewCommentInput
	ids      []int64
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{issueEdits: map[int64]string{}, nextCommentID: 1000, nextIssueID: 500, nextReviewID: 9000}
}

func (g *fakeGitHub) CreateReview(_ context.Context, _, _ string, _ int, sha, body string, comments []ReviewCommentInput) (int64, []int64, error) {
	g.nextReviewID++
	var ids []int64
	for range comments {
		g.nextCommentID++
		ids = append(ids, g.nextCommentID)
	}
	g.reviews = append(g.reviews, fakeReview{sha: sha, body: body, comments: comments, ids: ids})
	return g.nextReviewID, ids, nil
}

func (g *fakeGitHub) CreateIssueComment(_ context.Context, _, _ string, _ int, body string) (int64, error) {
	g.nextIssueID++
	g.issueCreates = append(g.issueCreates, body)
	return g.nextIssueID, nil
}

func (g *fakeGitHub) EditIssueComment(_ context.Context, _, _ string, id int64, body string) error {
	g.issueEdits[id] = body
	return nil
}

type fakeLedger struct {
	rows map[string]*db.PublishedFinding
}

func newFakeLedger() *fakeLedger { return &fakeLedger{rows: map[string]*db.PublishedFinding{}} }

func (l *fakeLedger) UpsertPublishedFinding(pf *db.PublishedFinding) error {
	cp := *pf
	l.rows[pf.Kind+":"+pf.Fingerprint] = &cp
	return nil
}

func (l *fakeLedger) GetPublishedFindingsForPR(_, _ string, _ int) ([]db.PublishedFinding, error) {
	var out []db.PublishedFinding
	for _, r := range l.rows {
		out = append(out, *r)
	}
	return out, nil
}

func (l *fakeLedger) get(kind, fp string) *db.PublishedFinding { return l.rows[kind+":"+fp] }

func publishRound(t *testing.T, gh *fakeGitHub, ledger *fakeLedger, r Round) Report {
	t.Helper()
	p := &Publisher{GH: gh, Ledger: ledger, Now: func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) }}
	rep, err := p.Publish(context.Background(), r)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return rep
}

func roundOne() Round {
	return Round{
		Owner: "acme", Repo: "example", Number: 7, HeadSHA: "sha-round-1", RoundNumber: 1,
		Findings: []payload.Finding{
			f("sum", "unknown", "SUMMARY", 0, "Narrative."),
			f("c1", "critical", "a.go", 10, "Critical thing."),
			f("m1", "medium", "b.go", 20, "Medium thing."),
			f("l1", "low", "a.go", 11, "Low thing."),
			f("m2", "medium", "a.go", 99, "Medium outside hunk."),
		},
		SourceTags: map[string]string{"m1": "both"},
		Commentable: map[string]map[int]bool{
			"a.go": {10: true, 11: true},
			"b.go": {20: true},
		},
		AgentLinkBase: "https://prism.example/go/agent?o=acme&r=example&n=7",
	}
}

func TestPublishRoundOne(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	rep := publishRound(t, gh, ledger, roundOne())

	if len(gh.issueCreates) != 1 || len(gh.issueEdits) != 0 {
		t.Fatalf("issue creates=%d edits=%d, want 1/0", len(gh.issueCreates), len(gh.issueEdits))
	}
	if !strings.Contains(gh.issueCreates[0], SummaryMarker) {
		t.Errorf("summary comment missing marker")
	}
	if rep.SummaryCommentID != 501 {
		t.Errorf("SummaryCommentID = %d, want 501", rep.SummaryCommentID)
	}
	if len(gh.reviews) != 1 {
		t.Fatalf("reviews = %d, want 1", len(gh.reviews))
	}
	rv := gh.reviews[0]
	if rv.sha != "sha-round-1" || rv.body != "" {
		t.Errorf("review sha=%q body=%q", rv.sha, rv.body)
	}
	if len(rv.comments) != 2 || rv.comments[0].Path != "a.go" || rv.comments[0].Line != 10 || rv.comments[1].Path != "b.go" || rv.comments[1].Line != 20 {
		t.Fatalf("review comments = %+v", rv.comments)
	}
	if !strings.Contains(rv.comments[0].Body, FindingMarker("c1")) || !strings.Contains(rv.comments[1].Body, "Source: PRism · Both") {
		t.Errorf("inline bodies wrong: %+v", rv.comments)
	}
	if rep.ReviewID != 9001 || rep.InlinePosted != 2 || rep.Annotations != 2 || rep.StillOpen != 0 || rep.Fixed != 0 {
		t.Errorf("report = %+v", rep)
	}

	sum := ledger.get(db.PublishedKindSummary, "summary")
	if sum == nil || sum.CommentID != 501 || sum.State != db.PublishedStateOpen || sum.RepoOwner != "acme" || sum.PRNumber != 7 {
		t.Fatalf("summary ledger row = %+v", sum)
	}
	if sum.Rounds != 1 {
		t.Errorf("summary row Rounds = %d, want the round number 1", sum.Rounds)
	}
	c1 := ledger.get(db.PublishedKindFinding, "c1")
	m1 := ledger.get(db.PublishedKindFinding, "m1")
	if c1 == nil || m1 == nil {
		t.Fatalf("finding rows missing: c1=%v m1=%v", c1, m1)
	}
	if c1.CommentID != 1001 || m1.CommentID != 1002 {
		t.Errorf("comment ids by index wrong: c1=%d m1=%d", c1.CommentID, m1.CommentID)
	}
	if c1.ReviewID != 9001 || c1.Severity != "critical" || c1.SourceTag != "prism-only" || c1.ReviewedSHA != "sha-round-1" || c1.LastSeenSHA != "sha-round-1" || c1.State != db.PublishedStateOpen {
		t.Errorf("c1 row = %+v", c1)
	}
	if m1.SourceTag != "both" || m1.PublishedAt.IsZero() {
		t.Errorf("m1 row = %+v", m1)
	}
	if ledger.get(db.PublishedKindFinding, "l1") != nil || ledger.get(db.PublishedKindFinding, "m2") != nil {
		t.Errorf("annotation-only findings must not get ledger rows")
	}
}

func TestPublishRoundTwo(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())

	r2 := roundOne()
	r2.HeadSHA = "sha-round-2"
	r2.RoundNumber = 0 // derived from the ledger's summary row
	r2.Findings = []payload.Finding{
		f("sum", "unknown", "SUMMARY", 0, "Narrative."),
		f("c1", "critical", "a.go", 10, "Critical thing."),
		f("m3", "medium", "b.go", 21, "New medium."),
	}
	r2.Commentable["b.go"][21] = true
	rep := publishRound(t, gh, ledger, r2)

	if len(gh.issueCreates) != 1 {
		t.Fatalf("round 2 must not create a new summary; creates=%d", len(gh.issueCreates))
	}
	edited, ok := gh.issueEdits[501]
	if !ok || !strings.Contains(edited, "**Since last review:** 1 new · 1 still open · 1 fixed") {
		t.Fatalf("summary edit wrong: ok=%v body=%s", ok, edited)
	}
	if !strings.Contains(edited, "Reviews (2)") {
		t.Errorf("round number must be derived from the ledger when the caller leaves it zero")
	}
	if sum := ledger.get(db.PublishedKindSummary, "summary"); sum == nil || sum.Rounds != 2 || sum.LastSeenSHA != "sha-round-2" {
		t.Errorf("summary row after round 2 = %+v", sum)
	}
	if len(gh.reviews) != 2 {
		t.Fatalf("reviews = %d, want 2", len(gh.reviews))
	}
	rv := gh.reviews[1]
	if len(rv.comments) != 1 || !strings.Contains(rv.comments[0].Body, FindingMarker("m3")) {
		t.Fatalf("round 2 review comments = %+v, want only m3", rv.comments)
	}
	if rep.SummaryCommentID != 501 || rep.ReviewID != 9002 || rep.InlinePosted != 1 || rep.Annotations != 0 || rep.StillOpen != 1 || rep.Fixed != 1 {
		t.Errorf("report = %+v", rep)
	}

	c1 := ledger.get(db.PublishedKindFinding, "c1")
	if c1.LastSeenSHA != "sha-round-2" || c1.ReviewedSHA != "sha-round-1" || c1.CommentID != 1001 || c1.State != db.PublishedStateOpen {
		t.Errorf("still-open c1 row = %+v", c1)
	}
	m1 := ledger.get(db.PublishedKindFinding, "m1")
	if m1.LastSeenSHA != "sha-round-1" || m1.State != db.PublishedStateOpen {
		t.Errorf("fixed m1 row must be left untouched: %+v", m1)
	}
	m3 := ledger.get(db.PublishedKindFinding, "m3")
	if m3 == nil || m3.CommentID != 1003 || m3.ReviewID != 9002 || m3.ReviewedSHA != "sha-round-2" {
		t.Errorf("m3 row = %+v", m3)
	}
}

func TestPublishUsesProvidedPreviousInsteadOfLedgerLoad(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r := roundOne()
	r.RoundNumber = 2
	r.Previous = []db.PublishedFinding{
		{Kind: db.PublishedKindSummary, Fingerprint: "summary", CommentID: 777, State: db.PublishedStateOpen},
	}
	rep := publishRound(t, gh, ledger, r)
	if len(gh.issueCreates) != 0 || gh.issueEdits[777] == "" || rep.SummaryCommentID != 777 {
		t.Fatalf("expected edit of comment 777: creates=%d edits=%v rep=%+v", len(gh.issueCreates), gh.issueEdits, rep)
	}
}

func TestPublishNoInlineSkipsReview(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r := roundOne()
	r.Commentable = nil
	rep := publishRound(t, gh, ledger, r)
	if len(gh.reviews) != 0 {
		t.Fatalf("no inline findings must post no review; got %d", len(gh.reviews))
	}
	if len(gh.issueCreates) != 1 || rep.InlinePosted != 0 || rep.Annotations != 4 || rep.ReviewID != 0 {
		t.Errorf("creates=%d rep=%+v", len(gh.issueCreates), rep)
	}
}
