package approval

import (
	"testing"
	"time"
)

func validFixture() (Snapshot, Assessment) {
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s := Snapshot{ID: "snapshot", Target: Target{Owner: "acme", Repo: "example", Number: 1, ExpectedHeadSHA: sha}, Revision: Revision{Head: sha, Base: sha, MergeBase: sha}, Eligible: true, Manifest: Manifest{Complete: true, Endpoints: []Endpoint{{Name: "reviews", Complete: true, Pages: 1}}}, Checks: []Check{{Name: "build", State: "success", SHA: sha}}, Sources: []Source{{ID: "prism", Provider: "prism", Verified: true, Completion: "completed", ReviewedSHA: sha, FileCoverage: "not_reported"}}, Evidence: []Evidence{{ID: "review", SourceID: "prism", Body: "No concerns."}}}
	s.Digest = SnapshotDigest(s)
	a := Assessment{SnapshotID: s.ID, SnapshotDigest: s.Digest, Summary: "Current completed review has no concerns.", Artifacts: []ArtifactDisposition{{EvidenceID: "review", Classification: "non_actionable", Rationale: "The review reports no concerns."}}}
	return s, a
}
func TestPolicyConservativeGates(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Snapshot, *Assessment)
		want   string
	}{
		{"complete", func(s *Snapshot, a *Assessment) {}, "candidate"},
		{"missing page", func(s *Snapshot, a *Assessment) { s.Manifest.Endpoints[0].Complete = false }, "insufficient_evidence"},
		{"human objection", func(s *Snapshot, a *Assessment) { s.HumanChangesRequested = true }, "needs_attention"},
		{"optional failing check", func(s *Snapshot, a *Assessment) {
			s.Checks = append(s.Checks, Check{Name: "optional", State: "failure", SHA: s.Revision.Head})
		}, "needs_attention"},
		{"no checks", func(s *Snapshot, a *Assessment) { s.Checks = nil }, "insufficient_evidence"},
		{"only skipped", func(s *Snapshot, a *Assessment) { s.Checks[0].State = "skipped" }, "insufficient_evidence"},
		{"spoofed provider", func(s *Snapshot, a *Assessment) { s.Sources[0].Verified = false }, "insufficient_evidence"},
		{"pending provider", func(s *Snapshot, a *Assessment) { s.ReviewInProgress = true }, "insufficient_evidence"},
		{"old review", func(s *Snapshot, a *Assessment) {
			s.Sources[0].ReviewedSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}, "insufficient_evidence"},
		{"omitted artifact", func(s *Snapshot, a *Assessment) { a.Artifacts = nil }, "insufficient_evidence"},
		{"low correctness concern", func(s *Snapshot, a *Assessment) {
			a.Concerns = []Concern{{ID: "c", EvidenceIDs: []string{"review"}, Claim: "Race", OriginalSeverity: "low", Impact: "correctness", Disposition: "non_blocking", Rationale: "Low severity"}}
		}, "insufficient_evidence"},
		{"author fixed assertion", func(s *Snapshot, a *Assessment) {
			a.Concerns = []Concern{{ID: "c", EvidenceIDs: []string{"review"}, Claim: "Race", Disposition: "fixed", Rationale: "Author says fixed"}}
		}, "insufficient_evidence"},
		{"excluded", func(s *Snapshot, a *Assessment) { s.Eligible = false }, "excluded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, a := validFixture()
			tt.change(&s, &a)
			s.Digest = SnapshotDigest(s)
			a.SnapshotDigest = s.Digest
			if got := Evaluate(s, a).Decision; got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}
func TestDigestContentAndTime(t *testing.T) {
	s, _ := validFixture()
	digest := SnapshotDigest(s)
	s.CapturedAt = time.Now()
	s.Evidence[0].UpdatedAt = time.Now()
	if SnapshotDigest(s) != digest {
		t.Fatal("presentation times changed digest")
	}
	s.Evidence[0].Body = "New blocker"
	if SnapshotDigest(s) == digest {
		t.Fatal("edited review did not change digest")
	}
}
