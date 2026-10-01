package approval

import (
	"strings"
	"testing"
	"time"
)

func fixedFixture() (Snapshot, Assessment) {
	s, a := anchoredConcernFixture()
	old := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	s.Concerns[0].OriginalRevision = old
	s.Evidence[0].ReviewedSHA = old
	a.Concerns[0].OriginalRevision = old
	a.Concerns[0].Disposition = "fixed"
	a.Concerns[0].Citations = append(a.Concerns[0].Citations, Citation{Revision: old, Path: "auth.go", StartLine: 10, EndLine: 10, Excerpt: "return allowWithoutAuthorization()", Validated: true})
	a.SchemaVersion, a.PromptVersion, a.RuntimeVersion, a.PolicyVersion = "1", PromptVersion, RuntimeVersion, PolicyVersion
	s.Digest = SnapshotDigest(s)
	a.SnapshotDigest = s.Digest
	return s, a
}

func redigest(s *Snapshot, a *Assessment) {
	s.Digest = SnapshotDigest(*s)
	a.SnapshotDigest = s.Digest
}

func hasReason(a Assessment, reason string) bool {
	for _, r := range a.ReasonCodes {
		if r == reason {
			return true
		}
	}
	return false
}

func TestGateDecidesSnapshotBlockersWithoutInvestigation(t *testing.T) {
	for _, test := range []struct {
		reason, summary string
		change          func(*Snapshot)
	}{
		{"ci_failed", "CI is failing", func(s *Snapshot) {
			s.Checks = append(s.Checks, Check{Name: "lint", State: "failure", SHA: s.Revision.Head})
		}},
		{"human_changes_requested", "change request is still standing", func(s *Snapshot) { s.HumanChangesRequested = true }},
		{"provider_changes_requested", "review provider requested changes", func(s *Snapshot) { s.ProviderChangesRequested = true }},
	} {
		t.Run(test.reason, func(t *testing.T) {
			s, a := fixedFixture()
			if got := Evaluate(s, a).Decision; got != "candidate" {
				t.Fatalf("fixture is not a valid candidate: %s", got)
			}
			test.change(&s)
			redigest(&s, &a)
			if err := ValidateAssessment(s, a); err != nil {
				t.Fatal(err)
			}
			if got := Evaluate(s, a).Decision; got != "needs_attention" {
				t.Fatalf("a fully valid all-fixed assessment cleared the blocker: %s", got)
			}
			gated, ok := Gate(s)
			if !ok || gated.Decision != "needs_attention" || gated.Origin != OriginGate {
				t.Fatalf("gate did not fire: %+v", gated)
			}
			if !hasReason(gated, test.reason) || hasReason(gated, "invalid_assessment") {
				t.Fatalf("reasons %v", gated.ReasonCodes)
			}
			if !strings.Contains(gated.Summary, test.summary) || !strings.HasSuffix(gated.Summary, "so no investigation ran.") {
				t.Fatalf("summary %q", gated.Summary)
			}
			if gated.SnapshotID != s.ID || gated.SnapshotDigest != s.Digest || gated.SchemaVersion != "1" || gated.PromptVersion != PromptVersion || gated.RuntimeVersion != RuntimeVersion || gated.AssessedAt.IsZero() {
				t.Fatalf("gate provenance %+v", gated)
			}
			if len(gated.Concerns) != 0 || len(gated.Artifacts) != 0 || len(gated.CoverageGaps) != 0 {
				t.Fatalf("gate invented assessment content: %+v", gated)
			}
		})
	}
}

func TestGateJoinsEverySnapshotBlocker(t *testing.T) {
	s, _ := validFixture()
	s.HumanChangesRequested = true
	s.Checks[0].State = "failure"
	s.Digest = SnapshotDigest(s)
	gated, ok := Gate(s)
	if !ok || gated.Summary != "CI is failing on the current head, so no investigation ran. A reviewer's change request is still standing, so no investigation ran." {
		t.Fatalf("%v %q", ok, gated.Summary)
	}
}

func TestGateLeavesNeutralFactsToInvestigation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Snapshot)
	}{
		{"clean", func(*Snapshot) {}},
		{"review_missing", func(s *Snapshot) { s.Sources = nil }},
		{"ci_pending", func(s *Snapshot) { s.Checks[0].State = "pending" }},
		{"ci_unknown", func(s *Snapshot) { s.Checks = nil }},
		{"review_in_progress", func(s *Snapshot) { s.ReviewInProgress = true }},
		{"source_incomplete", func(s *Snapshot) { s.Manifest.Complete = false }},
		{"draft", func(s *Snapshot) { s.Draft = true }},
		{"excluded", func(s *Snapshot) {
			s.Eligible = false
			s.ExclusionReasons = []string{"pr_closed"}
			s.Checks[0].State = "failure"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := validFixture()
			test.change(&s)
			s.Digest = SnapshotDigest(s)
			if a, ok := Gate(s); ok {
				t.Fatalf("gate fired: %+v", a)
			}
		})
	}
}

func TestMissingReviewIsNeverNeedsAttentionByItself(t *testing.T) {
	s, a := validFixture()
	s.Sources = nil
	redigest(&s, &a)
	got := Evaluate(s, a)
	if got.Decision != "insufficient_evidence" || !hasReason(got, "review_missing") {
		t.Fatalf("%s %v", got.Decision, got.ReasonCodes)
	}
}

func TestDraftIsInvestigatedButNeverCandidate(t *testing.T) {
	s, a := fixedFixture()
	s.Draft = true
	redigest(&s, &a)
	clean := Evaluate(s, a)
	if clean.Decision != "insufficient_evidence" || !hasReason(clean, "pr_draft") {
		t.Fatalf("clean draft: %s %v", clean.Decision, clean.ReasonCodes)
	}
	a.Concerns[0].Disposition = "unresolved"
	open := Evaluate(s, a)
	if open.Decision != "needs_attention" || !hasReason(open, "pr_draft") || !hasReason(open, "open_concern") {
		t.Fatalf("open draft: %s %v", open.Decision, open.ReasonCodes)
	}
}

func TestReuseKeyCoversEveryInput(t *testing.T) {
	base := []string{"digest", "openrouter", "model", "policy", "prompt", "runtime"}
	key := reuseKey(base...)
	for i := range base {
		changed := append([]string(nil), base...)
		changed[i] += "x"
		if reuseKey(changed...) == key {
			t.Fatalf("input %d did not change the key", i)
		}
	}
	if ReuseKey("digest", "openrouter", "model") != reuseKey("digest", "openrouter", "model", PolicyVersion, PromptVersion, RuntimeVersion) {
		t.Fatal("exported key does not use the current versions")
	}
	if ReuseKey("ab", "c", "d") == ReuseKey("a", "bc", "d") {
		t.Fatal("key fields are not separated")
	}
}

func priorFixture() (Snapshot, Assessment) {
	s, a := fixedFixture()
	a.Origin = OriginInvestigator
	a.Model = "model"
	a.Usage = Usage{InputTokens: 1000, OutputTokens: 100, Rounds: 1}
	a.AssessedAt = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	return s, Evaluate(s, a)
}

func TestReuseAdoptsIdenticalSnapshotAssessment(t *testing.T) {
	s, prior := priorFixture()
	prior.Decision = "insufficient_evidence"
	reused, ok := Reuse(s, prior, "first")
	if !ok {
		t.Fatal("identical snapshot was not reused")
	}
	if reused.Decision != "candidate" || reused.Origin != OriginReused || reused.ReusedFrom != "first" || reused.Usage != (Usage{}) || !reused.AssessedAt.Equal(prior.AssessedAt) {
		t.Fatalf("%+v", reused)
	}
	chained, ok := Reuse(s, reused, "second")
	if !ok || chained.ReusedFrom != "first" || chained.Origin != OriginReused {
		t.Fatalf("chained reuse %v %+v", ok, chained)
	}
}

func TestReuseRejectsAnythingButAnIdenticalCurrentInvestigation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Snapshot, *Assessment)
	}{
		{"digest", func(s *Snapshot, a *Assessment) { a.SnapshotDigest = strings.Repeat("0", 64) }},
		{"snapshot id", func(s *Snapshot, a *Assessment) { a.SnapshotID = "other" }},
		{"incomplete manifest", func(s *Snapshot, a *Assessment) { s.Manifest.Complete = false; redigest(s, a) }},
		{"ineligible", func(s *Snapshot, a *Assessment) { s.Eligible = false; redigest(s, a) }},
		{"gate origin", func(s *Snapshot, a *Assessment) { a.Origin = OriginGate }},
		{"limit origin", func(s *Snapshot, a *Assessment) { a.Origin = OriginLimit }},
		{"unknown origin", func(s *Snapshot, a *Assessment) { a.Origin = "" }},
		{"schema", func(s *Snapshot, a *Assessment) { a.SchemaVersion = "0" }},
		{"policy", func(s *Snapshot, a *Assessment) { a.PolicyVersion = "approval-v0" }},
		{"prompt", func(s *Snapshot, a *Assessment) { a.PromptVersion = "approval-v0" }},
		{"runtime", func(s *Snapshot, a *Assessment) { a.RuntimeVersion = "old-runtime" }},
		{"invalid assessment", func(s *Snapshot, a *Assessment) { a.Artifacts = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, prior := priorFixture()
			test.change(&s, &prior)
			if a, ok := Reuse(s, prior, "first"); ok {
				t.Fatalf("reused: %+v", a)
			}
		})
	}
}
