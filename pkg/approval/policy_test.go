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

func TestPolicyPreservesExtractedConcernProvenance(t *testing.T) {
	for _, field := range []string{"claim", "severity", "revision"} {
		t.Run(field, func(t *testing.T) {
			s, a := validFixture()
			s.Concerns = []Concern{{ID: "concern", EvidenceIDs: []string{"review"}, Claim: "Missing authorization check", OriginalSeverity: "critical", OriginalRevision: s.Revision.Head}}
			a.Concerns = append([]Concern(nil), s.Concerns...)
			a.Concerns[0].Disposition = "unresolved"
			a.Concerns[0].Rationale = "The condition persists."
			a.Artifacts[0].Classification = "concerns"
			a.Artifacts[0].ConcernIDs = []string{"concern"}
			s.Digest = SnapshotDigest(s)
			a.SnapshotDigest = s.Digest
			if err := ValidateAssessment(s, a); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "claim":
				a.Concerns[0].Claim = "Style suggestion"
			case "severity":
				a.Concerns[0].OriginalSeverity = "low"
			case "revision":
				a.Concerns[0].OriginalRevision = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			}
			if err := ValidateAssessment(s, a); err == nil {
				t.Fatal("altered concern accepted")
			}
		})
	}
}

func TestPolicyRejectsInventedOriginalRevisionForDiscoveredConcern(t *testing.T) {
	s, a := validFixture()
	s.Evidence[0].ReviewedSHA = ""
	a.Concerns = []Concern{{ID: "new", EvidenceIDs: []string{"review"}, Claim: "A reported bug", OriginalRevision: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Disposition: "fixed", Rationale: "Changed code", Citations: []Citation{{Revision: s.Revision.Head, Path: "main.go", StartLine: 1, EndLine: 1, Excerpt: "new", Validated: true}, {Revision: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Path: "main.go", StartLine: 1, EndLine: 1, Excerpt: "old", Validated: true}}}}
	a.Artifacts[0].Classification = "concerns"
	a.Artifacts[0].ConcernIDs = []string{"new"}
	s.Digest = SnapshotDigest(s)
	a.SnapshotDigest = s.Digest
	if err := ValidateAssessment(s, a); err == nil {
		t.Fatal("unattributed original revision accepted")
	}
}

func anchoredConcernFixture() (Snapshot, Assessment) {
	s, a := validFixture()
	s.Evidence[0].Body = "The authorization guard may be missing."
	s.Evidence[0].ReviewedSHA = s.Revision.Head
	s.Evidence[0].Path = "auth.go"
	s.Evidence[0].StartLine = 10
	s.Evidence[0].EndLine = 10
	s.Concerns = []Concern{{ID: "concern", EvidenceIDs: []string{"review"}, Claim: s.Evidence[0].Body, OriginalSeverity: "critical", OriginalRevision: s.Revision.Head, Impact: "unknown", Path: "auth.go", StartLine: 10, EndLine: 10}}
	a.Concerns = append([]Concern(nil), s.Concerns...)
	a.Concerns[0].Disposition = "not_applicable"
	a.Concerns[0].Rationale = "The existing authorization guard rejects unauthenticated requests."
	a.Concerns[0].Citations = []Citation{{Revision: s.Revision.Head, Path: "auth.go", StartLine: 10, EndLine: 10, Excerpt: "if !authorized(request) { return denied }", Validated: true}}
	a.Artifacts[0].Classification = "concerns"
	a.Artifacts[0].ConcernIDs = []string{"concern"}
	s.Digest = SnapshotDigest(s)
	a.SnapshotDigest = s.Digest
	return s, a
}

func TestPolicySafeDispositionsRequireCanonicalAnchors(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Snapshot, *Assessment)
		want   string
	}{
		{"anchored guard", func(s *Snapshot, a *Assessment) {}, "candidate"},
		{"unrelated README", func(s *Snapshot, a *Assessment) { a.Concerns[0].Citations[0].Path = "README.md" }, "insufficient_evidence"},
		{"unrelated line", func(s *Snapshot, a *Assessment) {
			a.Concerns[0].Citations[0].StartLine = 50
			a.Concerns[0].Citations[0].EndLine = 50
		}, "insufficient_evidence"},
		{"single character", func(s *Snapshot, a *Assessment) { a.Concerns[0].Citations[0].Excerpt = "#" }, "insufficient_evidence"},
		{"invented anchor", func(s *Snapshot, a *Assessment) {
			a.Concerns[0].Path = "README.md"
			a.Concerns[0].Citations[0].Path = "README.md"
		}, "insufficient_evidence"},
		{"missing anchor", func(s *Snapshot, a *Assessment) { s.Concerns[0].Path = ""; a.Concerns[0].Path = "" }, "insufficient_evidence"},
		{"unknown to style", func(s *Snapshot, a *Assessment) {
			a.Concerns[0].Impact = "style"
			a.Concerns[0].Disposition = "non_blocking"
			a.Concerns[0].Citations = []Citation{{EvidenceID: "review", Excerpt: "a", Validated: true}}
		}, "insufficient_evidence"},
		{"provider objection", func(s *Snapshot, a *Assessment) { s.ProviderChangesRequested = true }, "needs_attention"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, a := anchoredConcernFixture()
			test.change(&s, &a)
			s.Digest = SnapshotDigest(s)
			a.SnapshotDigest = s.Digest
			if got := Evaluate(s, a).Decision; got != test.want {
				t.Fatalf("got %s want %s", got, test.want)
			}
		})
	}
}

func TestPolicyNonBlockingRequiresCanonicalNonBlockingImpact(t *testing.T) {
	s, a := anchoredConcernFixture()
	s.Concerns[0].Impact = "style"
	s.Concerns[0].OriginalSeverity = "low"
	a.Concerns[0].Impact = "style"
	a.Concerns[0].OriginalSeverity = "low"
	a.Concerns[0].Disposition = "non_blocking"
	a.Concerns[0].Citations = []Citation{{EvidenceID: "review", Excerpt: s.Evidence[0].Body, Validated: true}}
	s.Digest = SnapshotDigest(s)
	a.SnapshotDigest = s.Digest
	if got := Evaluate(s, a).Decision; got != "candidate" {
		t.Fatal(got)
	}
	a.Concerns[0].Citations[0].Excerpt = "a"
	if got := Evaluate(s, a).Decision; got != "insufficient_evidence" {
		t.Fatal(got)
	}
}

func TestPolicyDiscoveredConcernCannotBorrowOlderArtifactRevision(t *testing.T) {
	s, a := anchoredConcernFixture()
	s.Concerns = nil
	old := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	s.Evidence = append(s.Evidence, Evidence{ID: "older", Body: "Earlier unrelated review", ReviewedSHA: old})
	a.Concerns[0].EvidenceIDs = append(a.Concerns[0].EvidenceIDs, "older")
	a.Concerns[0].OriginalRevision = old
	a.Concerns[0].Disposition = "fixed"
	a.Concerns[0].Citations = append(a.Concerns[0].Citations, Citation{Revision: old, Path: "auth.go", StartLine: 10, EndLine: 10, Excerpt: "return allowWithoutAuthorization()", Validated: true})
	a.Artifacts = append(a.Artifacts, ArtifactDisposition{EvidenceID: "older", Classification: "concerns", ConcernIDs: []string{"concern"}, Rationale: "Claims previous revision"})
	s.Digest = SnapshotDigest(s)
	a.SnapshotDigest = s.Digest
	if got := Evaluate(s, a).Decision; got != "insufficient_evidence" {
		t.Fatal(got)
	}
}

func TestPolicyRejectsUnsupportedTestClaimsInGeneratedProse(t *testing.T) {
	for _, field := range []string{"summary", "gap", "artifact", "concern"} {
		t.Run(field, func(t *testing.T) {
			s, a := anchoredConcernFixture()
			switch field {
			case "summary":
				a.Summary = "Tests pass."
			case "gap":
				a.CoverageGaps = []string{"Tests were executed successfully."}
			case "artifact":
				a.Artifacts[0].Rationale = "Tests have passed."
			case "concern":
				a.Concerns[0].Rationale = "The test suite passes."
			}
			if err := ValidateAssessment(s, a); err == nil {
				t.Fatal("unsupported execution claim accepted")
			}
		})
	}
}

func TestPolicyFixedRequiresOriginalCodeAtSourceAnchor(t *testing.T) {
	s, a := anchoredConcernFixture()
	old := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	s.Concerns[0].OriginalRevision = old
	s.Evidence[0].ReviewedSHA = old
	a.Concerns[0].OriginalRevision = old
	a.Concerns[0].Disposition = "fixed"
	a.Concerns[0].Citations = append(a.Concerns[0].Citations, Citation{Revision: old, Path: "auth.go", StartLine: 10, EndLine: 10, Excerpt: "return allowWithoutAuthorization()", Validated: true})
	s.Digest = SnapshotDigest(s)
	a.SnapshotDigest = s.Digest
	if got := Evaluate(s, a).Decision; got != "candidate" {
		t.Fatalf("source anchored fix rejected: %s", got)
	}
	a.Concerns[0].Citations[1].StartLine = 50
	a.Concerns[0].Citations[1].EndLine = 50
	if got := Evaluate(s, a).Decision; got != "insufficient_evidence" {
		t.Fatalf("unrelated original code accepted: %s", got)
	}
}

func TestDowngradeUnsupportedDiscoveriesDowngradesWithoutDroppingLinks(t *testing.T) {
	s := Snapshot{Evidence: []Evidence{{ID: "a", Body: "The handler skips the authorization check for admins."}, {ID: "b", Body: "Unrelated note."}}}
	a := Assessment{Concerns: []Concern{
		{ID: "named", Disposition: "fixed", Claim: "skips the authorization check", EvidenceIDs: []string{"b", "a"}},
		{ID: "unsourced", Disposition: "not_applicable", Claim: "a claim no artifact makes", EvidenceIDs: []string{"a", "b"}},
		{ID: "open", Disposition: "unresolved", Claim: "a claim no artifact makes", EvidenceIDs: []string{"a", "b"}},
	}}
	downgradeUnsupportedDiscoveries(s, &a)
	if got := a.Concerns[0]; got.Disposition != "uncertain" || len(got.EvidenceIDs) != 2 {
		t.Fatalf("a discovered fix citing several artifacts should become uncertain and keep its links: %+v", got)
	}
	if a.Concerns[1].Disposition != "uncertain" {
		t.Fatalf("a favorable disposition without a source should become uncertain: %+v", a.Concerns[1])
	}
	if a.Concerns[2].Disposition != "unresolved" || len(a.Concerns[2].EvidenceIDs) != 2 {
		t.Fatalf("an unresolved concern is left as the model wrote it: %+v", a.Concerns[2])
	}
}

func TestRepairByDowngradeSettlesAFavorableDispositionThePolicyRejects(t *testing.T) {
	s, a := validFixture()
	s.Evidence[0].Body = "Rename the helper to describe what it returns."
	s.Digest = SnapshotDigest(s)
	a.SnapshotDigest = s.Digest
	a.Artifacts[0] = ArtifactDisposition{EvidenceID: "review", Classification: "concerns", Rationale: "Naming suggestion.", ConcernIDs: []string{"naming"}}
	a.Concerns = []Concern{{ID: "naming", EvidenceIDs: []string{"review"}, OriginalSeverity: "low", Impact: "unknown", Claim: "Rename the helper to describe what it returns.", OriginalRevision: s.Revision.Head, Disposition: "non_blocking", Rationale: "Only a naming nit."}}
	if ValidateAssessment(s, a) == nil {
		t.Fatal("fixture should fail policy: non_blocking needs a style or documentation source")
	}
	if err := repairByDowngrade(s, &a); err != nil {
		t.Fatalf("repair should settle the assessment: %v", err)
	}
	if a.Concerns[0].Disposition != "uncertain" || Evaluate(s, a).Decision == "candidate" {
		t.Fatalf("the rejected disposition must become uncertain and must not yield a candidate: %+v %s", a.Concerns[0], Evaluate(s, a).Decision)
	}
}

func TestRestoreCanonicalConcernsKeepsJudgmentAndRestoresProvenance(t *testing.T) {
	canonical := Concern{ID: "c1", EvidenceIDs: []string{"review"}, Claim: "Exact reviewer claim.", OriginalSeverity: "high", OriginalRevision: "r1", Path: "a.go", StartLine: 3, EndLine: 4}
	s := Snapshot{Concerns: []Concern{canonical, {ID: "c2", EvidenceIDs: []string{"review"}, Claim: "Second claim."}}}
	a := Assessment{Concerns: []Concern{{ID: "c1", Claim: "Paraphrased claim.", OriginalSeverity: "low", Path: "b.go", Disposition: "unresolved", Rationale: "Still open."}}}
	restoreCanonicalConcerns(s, &a)
	got := a.Concerns[0]
	if got.Claim != canonical.Claim || got.OriginalSeverity != "high" || got.Path != "a.go" || got.StartLine != 3 || got.Disposition != "unresolved" || got.Rationale != "Still open." || len(got.EvidenceIDs) != 1 {
		t.Fatalf("provenance not restored or judgment lost: %+v", got)
	}
	if len(a.Concerns) != 2 || a.Concerns[1].ID != "c2" || a.Concerns[1].Disposition != "unresolved" {
		t.Fatalf("an omitted canonical concern must be added as unresolved: %+v", a.Concerns)
	}
}

func TestNormalizeArtifactsMapsSpellingsAndLinksConcerns(t *testing.T) {
	a := Assessment{Concerns: []Concern{{ID: "c1"}}, Artifacts: []ArtifactDisposition{{EvidenceID: "e1", Classification: "Concern", ConcernIDs: []string{"c1"}}, {EvidenceID: "e2", Classification: "non-actionable"}}}
	normalizeArtifacts(Snapshot{}, &a)
	if a.Artifacts[0].Classification != "concerns" || a.Artifacts[1].Classification != "non_actionable" || len(a.Concerns[0].EvidenceIDs) != 1 || a.Concerns[0].EvidenceIDs[0] != "e1" {
		t.Fatalf("not normalized: %+v", a)
	}
}

func TestStripTestExecutionClaimsKeepsTheRest(t *testing.T) {
	a := Assessment{Summary: "The change moves the toast. All tests passed in CI. The fix looks complete.", CoverageGaps: []string{"We ran the tests locally."}}
	stripTestExecutionClaims(&a)
	if a.Summary != "The change moves the toast. The fix looks complete." || len(a.CoverageGaps) != 0 {
		t.Fatalf("test execution sentences not removed cleanly: %q %q", a.Summary, a.CoverageGaps)
	}
	if claimsTestExecution(a.Summary) {
		t.Fatal("summary still claims test execution")
	}
}
