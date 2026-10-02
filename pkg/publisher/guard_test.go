package publisher

import (
	"context"
	"errors"
	"testing"

	"pr-review-server/db"
)

func TestHeadPublished_ReadsTheSummaryRowOnly(t *testing.T) {
	rows := []db.PublishedFinding{
		{Kind: db.PublishedKindFinding, Fingerprint: "c1", LastSeenSHA: "sha-2", State: db.PublishedStateOpen},
		{Kind: db.PublishedKindSummary, Fingerprint: "summary", LastSeenSHA: "SHA-1"},
	}
	if !HeadPublished(rows, "sha-1") {
		t.Fatal("the summary row's head is published, case-insensitively")
	}
	if HeadPublished(rows, "sha-2") {
		t.Fatal("a finding row alone is not a completed round")
	}
	if HeadPublished(rows, "") || HeadPublished(nil, "sha-1") {
		t.Fatal("no head or no rows is never published")
	}
}

func TestPublish_RefusesASecondRoundForAPublishedHead(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())
	p := &Publisher{GH: gh, Ledger: ledger, Policy: DefaultPolicy()}

	again := roundOne()
	again.Findings = again.Findings[:1]
	_, err := p.Publish(context.Background(), again)
	if !errors.Is(err, ErrHeadAlreadyPublished) {
		t.Fatalf("err = %v, want ErrHeadAlreadyPublished", err)
	}
	if len(gh.reviews) != 1 || len(gh.issueEdits) != 0 {
		t.Fatalf("a refused round writes nothing: reviews=%d edits=%d", len(gh.reviews), len(gh.issueEdits))
	}
	if c1 := ledger.get(db.PublishedKindFinding, "c1"); c1 == nil || c1.State != db.PublishedStateOpen {
		t.Fatalf("a refused round resolves nothing: %+v", c1)
	}

	next := roundOne()
	next.HeadSHA, next.RoundNumber = "sha-2", 0
	if _, err := p.Publish(context.Background(), next); err != nil {
		t.Fatalf("a new head publishes: %v", err)
	}
}
