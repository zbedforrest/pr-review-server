package approval

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func answerFixture() (Snapshot, Preload) {
	s := preloadFixture()
	s.Evidence = append(s.Evidence,
		Evidence{ID: "repeat", SourceID: "person", Kind: "comment", Body: "Same leak as the bot said."},
		Evidence{ID: "thanks", SourceID: "person", Kind: "comment", Body: "Thanks!"},
		Evidence{ID: "note", SourceID: "person", Kind: "comment", Body: "The   retry delay\n  doubles without an upper bound."},
		Evidence{ID: "echo", SourceID: "bot", Kind: "comment", Body: "Retry delay doubles without an upper bound."},
		Evidence{ID: "silent", SourceID: "person", Kind: "comment", Body: "Some unrelated remark."},
	)
	s.Digest = SnapshotDigest(s)
	p, _ := BuildPreload(context.Background(), s, nil)
	return s, p
}

func TestAssembleTakesProvenanceFromTheSnapshot(t *testing.T) {
	s, p := answerFixture()
	got := assembleAssessment(s, p, compactAnswer{Verdicts: []compactVerdict{{Concern: "C1", Disposition: "Not Applicable", Rationale: " The handler closes the connection. ", Related: stringList{"E5", "E99", "E1"}}}})
	c := got.Concerns[0]
	want := s.Concerns[0]
	if c.ID != want.ID || c.Claim != want.Claim || c.OriginalSeverity != want.OriginalSeverity || c.Impact != want.Impact || c.OriginalRevision != want.OriginalRevision || c.Path != want.Path || c.StartLine != want.StartLine || c.EndLine != want.EndLine {
		t.Fatalf("provenance not copied: %+v", c)
	}
	if !reflect.DeepEqual(c.EvidenceIDs, []string{"finding", "repeat"}) || c.Disposition != "not_applicable" || c.Rationale != "The handler closes the connection." {
		t.Fatalf("verdict not applied: %+v", c)
	}
	if got.SnapshotID != s.ID || got.SnapshotDigest != s.Digest || got.PromptVersion != PromptVersion || got.Summary != noSummary {
		t.Fatalf("assessment header: %+v", got)
	}
}

func TestAssembleDerivesEveryArtifact(t *testing.T) {
	s, p := answerFixture()
	got := assembleAssessment(s, p, compactAnswer{
		Summary:    "One concern remains open.",
		Verdicts:   []compactVerdict{{Concern: "C1", Disposition: "unresolved", Rationale: "Still leaks.", Related: stringList{"E5"}}},
		Discovered: []compactDiscovery{{Evidence: "E7", Claim: "retry delay doubles without an upper bound", Disposition: "not_applicable", Rationale: "The delay is capped.", Related: stringList{"E8"}}},
		NoConcerns: stringList{"E6", "E2"},
	})
	rationale := map[string]string{}
	for _, art := range got.Artifacts {
		rationale[art.EvidenceID] = art.Classification + ": " + art.Rationale
	}
	for id, want := range map[string]string{
		"finding": "concerns: Carries the concerns assessed above.",
		"repeat":  "concerns: Carries the concerns assessed above.",
		"note":    "concerns: Carries the concerns assessed above.",
		"request": "non_actionable: Review request metadata.",
		"state":   "non_actionable: Review state only; no review text.",
		"thanks":  "non_actionable: Investigator found no actionable concern.",
		"inline":  "non_actionable: Investigator found no actionable concern.",
		"echo":    "non_actionable: Repeats discovered concern found:note:1.",
		"silent":  "non_actionable: Not classified by the investigator.",
	} {
		if rationale[id] != want {
			t.Errorf("%s: got %q, want %q", id, rationale[id], want)
		}
	}
	if len(got.Artifacts) != len(s.Evidence) || !reflect.DeepEqual(got.CoverageGaps, []string{"Artifact silent was not classified by the investigator"}) {
		t.Fatalf("artifacts=%d gaps=%v", len(got.Artifacts), got.CoverageGaps)
	}
	if concerns, artifacts := p.missing(compactAnswer{Verdicts: []compactVerdict{{Concern: "leak"}}, NoConcerns: stringList{"E6", "E2"}, Discovered: []compactDiscovery{{Evidence: "E7", Related: stringList{"E8"}}}}); len(concerns) != 0 || !reflect.DeepEqual(artifacts, []string{"E5", "E9"}) {
		t.Fatalf("missing concerns=%v artifacts=%v", concerns, artifacts)
	}
}

func TestAssembleQuotesDiscoveredClaimsFromTheirSource(t *testing.T) {
	s, p := answerFixture()
	got := assembleAssessment(s, p, compactAnswer{Discovered: []compactDiscovery{
		{Evidence: "E7", Claim: "retry delay doubles without an upper bound", Disposition: "fixed", Related: stringList{"E8"}},
		{Evidence: "note", Claim: "An invented paraphrase of the note", Disposition: "unresolved", Related: stringList{"E8"}},
		{Evidence: "E7", Claim: "  ", Disposition: "unresolved"},
		{Evidence: "E42", Claim: "Something from nowhere at all", Disposition: "fixed"},
	}})
	if len(got.Concerns) != 2 {
		t.Fatalf("concerns: %+v", got.Concerns)
	}
	first, second := got.Concerns[0], got.Concerns[1]
	if first.ID != "found:note:1" || first.Claim != "retry delay\n  doubles without an upper bound" || !strings.Contains(s.Evidence[6].Body, first.Claim) || first.Impact != "unknown" || !reflect.DeepEqual(first.EvidenceIDs, []string{"note"}) {
		t.Fatalf("favorable discovery: %+v", first)
	}
	if second.ID != "found:note:2" || second.Claim != "An invented paraphrase of the note" || !reflect.DeepEqual(second.EvidenceIDs, []string{"note", "echo"}) {
		t.Fatalf("unfavorable discovery: %+v", second)
	}
	if gaps := strings.Join(got.CoverageGaps, "\n"); strings.Count(gaps, "Ignored a discovered concern") != 2 {
		t.Fatalf("gaps: %v", got.CoverageGaps)
	}
}

func TestAssembleRepairsCitationsWithoutTrustingThem(t *testing.T) {
	s, p := answerFixture()
	got := assembleAssessment(s, p, compactAnswer{Verdicts: []compactVerdict{
		{Concern: "C1", Disposition: "fixed", Citations: []compactCite{
			{Revision: "R1", Path: "app.go", Lines: lineRange{100, 101}, Excerpt: "  100| conn := open()\n  101| defer conn.Close()"},
			{Revision: "H", Path: "app.go", Lines: lineRange{110}, Excerpt: "10 items: one"},
			{Evidence: "E1", Excerpt: "leaks the   connection"},
			{Evidence: "E77", Excerpt: "anything"},
		}},
		{Concern: "C9", Disposition: "fixed"},
		{Concern: "leak", Disposition: "unresolved"},
		{Concern: "C1", Disposition: "maybe"},
	}})
	cites := got.Concerns[0].Citations
	if cites[0].Revision != strings.Repeat("c", 40) || cites[0].StartLine != 100 || cites[0].EndLine != 101 || cites[0].Excerpt != "conn := open()\ndefer conn.Close()" {
		t.Fatalf("code citation: %+v", cites[0])
	}
	if cites[1].Revision != s.Revision.Head || cites[1].EndLine != 110 || cites[1].Excerpt != "10 items: one" {
		t.Fatalf("an unnumbered excerpt must stay as given: %+v", cites[1])
	}
	if cites[2].EvidenceID != "finding" || cites[2].Excerpt != "leaks the connection" || cites[3].EvidenceID != "E77" {
		t.Fatalf("evidence citations: %+v", cites[2:])
	}
	gaps := strings.Join(got.CoverageGaps, "\n")
	if len(got.Concerns) != 1 || got.Concerns[0].Disposition != "fixed" || strings.Count(gaps, "unknown concern") != 1 || strings.Count(gaps, "duplicate verdict") != 1 {
		t.Fatalf("unknown and duplicate verdicts: %+v %v", got.Concerns, got.CoverageGaps)
	}
}

func TestStripLinePrefixesOnlyWhenEveryLineIsNumbered(t *testing.T) {
	for in, want := range map[string]string{
		"   12| a := 1\n   13| b := 2": "a := 1\nb := 2",
		"12: a\n\n13: b":               "a\n\nb",
		"12| a\nb := 2":                "12| a\nb := 2",
		"plain text":                   "plain text",
	} {
		if got := stripLinePrefixes(in); got != want {
			t.Errorf("stripLinePrefixes(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeCompactAnswer(t *testing.T) {
	got, err := decodeCompactAnswer("```json\n{\"summary\":\"ok\",\"no_concerns\":\"E1\",\"verdicts\":[{\"concern\":\"C1\",\"disposition\":\"fixed\",\"extra\":true,\"rationale\":7,\"citations\":[{\"revision\":\"H\",\"path\":\"a.go\",\"lines\":\"3-5\",\"excerpt\":\"x\"}]}],\"gaps\":\"one gap\"}\n```")
	if err != nil || got.Summary != "ok" || !reflect.DeepEqual([]string(got.NoConcerns), []string{"E1"}) || got.Verdicts[0].Disposition != "fixed" || got.Verdicts[0].Rationale != "" || !reflect.DeepEqual([]int(got.Verdicts[0].Citations[0].Lines), []int{3, 5}) || !reflect.DeepEqual([]string(got.Gaps), []string{"one gap"}) {
		t.Fatalf("lenient decode: %+v %v", got, err)
	}
	if got, err := decodeCompactAnswer(`Here it is: {"summary": "healed", "no_concerns": ["E1"],}`); err != nil || got.Summary != "healed" {
		t.Fatalf("healed decode: %+v %v", got, err)
	}
	for _, text := range []string{"", "null", "[]", "no JSON here", `"just a string"`} {
		if _, err := decodeCompactAnswer(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestDecodeCompactAnswerRejectsMisshapenNegativeFields(t *testing.T) {
	for _, text := range []string{
		`{"summary":"ok","discovered":{"evidence":"E1","claim":"The retry loop never stops."}}`,
		`{"summary":"ok","discoveries":[{"evidence":"E1","claim":"The retry loop never stops."}]}`,
		`{"summary":"ok","discovered":["E1: the retry loop never stops"]}`,
		`{"summary":"ok","gaps":[{"item":"E1","reason":"Could not verify the migration path."}]}`,
		`{"summary":"ok","coverage_gaps":["Could not verify the migration path."]}`,
		`{"summary":"ok","gaps":7}`,
		`{"summary":"ok","verdicts":{"concern":"C1","disposition":"fixed"}}`,
		`{"summary":"ok","no_concerns":[{"evidence":"E1"}]}`,
	} {
		if got, err := decodeCompactAnswer(text); err == nil {
			t.Errorf("accepted %s as %+v", text, got)
		}
	}
}

func TestNativeFailsClosedOnMisshapenDiscoveriesAndGaps(t *testing.T) {
	for _, text := range []string{
		`{"summary":"ok","no_concerns":["E1"],"discovered":{"evidence":"E1","claim":"No concerns at all here."}}`,
		`{"summary":"ok","no_concerns":["E1"],"coverage_gaps":["Could not verify the migration path."]}`,
	} {
		s, _ := validFixture()
		server, calls := fakeModel(t, "anthropic", func(int, map[string]any) modelTurn { return answerTurn(text) })
		got, err := fakeInvestigator("anthropic", server).Investigate(context.Background(), s, nil, &testBudget{})
		if *calls != 2 || err == nil || !strings.Contains(err.Error(), "invalid_assessment") {
			t.Fatalf("%s: calls=%d decision=%s err=%v", text, *calls, got.Decision, err)
		}
	}
}

func TestMergeKeepsFirstVerdictsAndUnitesClassifications(t *testing.T) {
	_, p := answerFixture()
	got := p.merge(compactAnswer{Summary: "first", Verdicts: []compactVerdict{{Concern: "C1", Disposition: "unresolved"}}, NoConcerns: stringList{"E2"}, Gaps: stringList{"a"}},
		compactAnswer{Summary: "second", Verdicts: []compactVerdict{{Concern: "leak", Disposition: "fixed"}}, NoConcerns: stringList{"inline", "E6"}, Discovered: []compactDiscovery{{Evidence: "E7"}}, Gaps: stringList{"b"}})
	if got.Summary != "first" || len(got.Verdicts) != 1 || got.Verdicts[0].Disposition != "unresolved" || !reflect.DeepEqual([]string(got.NoConcerns), []string{"E2", "E6"}) || len(got.Discovered) != 1 || len(got.Gaps) != 2 {
		t.Fatalf("merge: %+v", got)
	}
}

func TestNativeNeverApprovesAnUnverifiedFixOrTestClaims(t *testing.T) {
	s, _ := validFixture()
	s.Evidence[0].Body = "The cache key ignores the tenant, so tenants can read each other's entries."
	s.Evidence[0].ReviewedSHA = s.Revision.Head
	s.Evidence[0].Path = "cache.go"
	s.Evidence[0].StartLine = 12
	s.Evidence[0].ConcernIDs = []string{"tenant"}
	s.Concerns = []Concern{{ID: "tenant", EvidenceIDs: []string{"review"}, Claim: s.Evidence[0].Body, OriginalSeverity: "critical", Impact: "security", OriginalRevision: s.Revision.Head, Path: "cache.go", StartLine: 12, EndLine: 12}}
	s.Digest = SnapshotDigest(s)
	server, _ := fakeModel(t, "anthropic", func(int, map[string]any) modelTurn {
		return answerTurn(`{"summary":"All tests passed.","verdicts":[{"concern":"C1","disposition":"fixed","rationale":"The tests pass now.","citations":[{"revision":"H","path":"cache.go","lines":[12,12],"excerpt":"key := tenant + id"}]}]}`)
	})
	got, err := fakeInvestigator("anthropic", server).Investigate(context.Background(), s, nil, &testBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision == "candidate" || got.Concerns[0].Disposition != "uncertain" || got.Summary != noSummary || claimsTestExecution(got.Concerns[0].Rationale) {
		t.Fatalf("unverified fix or test claim survived: %+v", got)
	}
}

func TestInvalidDispositionsDoNotDiscardValidOnes(t *testing.T) {
	s, a := anchoredConcernFixture()
	ids := []string{"concern"}
	for _, id := range []string{"naming", "spacing"} {
		nit := Concern{ID: id, EvidenceIDs: []string{"review"}, Claim: "A naming or spacing nit.", OriginalSeverity: "low", Impact: "unknown", OriginalRevision: s.Revision.Head}
		s.Concerns = append(s.Concerns, nit)
		nit.Disposition, nit.Rationale = "non_blocking", "Only a nit."
		nit.Citations = []Citation{{EvidenceID: "review", Excerpt: s.Evidence[0].Body, Validated: true}}
		a.Concerns = append(a.Concerns, nit)
		ids = append(ids, id)
	}
	s.Digest = SnapshotDigest(s)
	a.SnapshotDigest = s.Digest
	a.Artifacts[0].ConcernIDs = ids
	downgradeInvalidDispositions(s, &a)
	if err := repairByDowngrade(s, &a); err != nil {
		t.Fatal(err)
	}
	if a.Concerns[0].Disposition != "not_applicable" || a.Concerns[1].Disposition != "uncertain" || a.Concerns[2].Disposition != "uncertain" {
		t.Fatalf("dispositions: %s %s %s", a.Concerns[0].Disposition, a.Concerns[1].Disposition, a.Concerns[2].Disposition)
	}
}
