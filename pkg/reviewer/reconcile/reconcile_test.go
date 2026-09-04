package reconcile

import (
	"testing"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

func prismFinding(id, file string, line int, comment string) payload.Finding {
	return payload.Finding{ID: id, Severity: "medium", File: file, Line: line, Comment: comment}
}

func greptileFinding(id int64, file string, start, end int, title, body string) ExternalFinding {
	return ExternalFinding{Source: SourceGreptile, CommentID: id, Severity: "medium", File: file, StartLine: start, EndLine: end, Title: title, Body: body}
}

func tagsByID(r Result) map[string]Tagged {
	out := map[string]Tagged{}
	for _, t := range r.Findings {
		out[t.Finding.ID] = t
	}
	return out
}

func TestReconcileEmptyInputs(t *testing.T) {
	r := Reconcile(nil, nil)
	if len(r.Findings) != 0 || len(r.GreptileOnly) != 0 {
		t.Errorf("unexpected output: %+v", r)
	}
}

func TestReconcileMatchesSameFileNearbyLineSimilarText(t *testing.T) {
	p := prismFinding("p1", "app/chat.go", 125, "The synthetic privateMessage has no originatingRoom so purchase threads created during broadcasting miss the LIVE status.")
	g := greptileFinding(42, "app/chat.go", 118, 120, "Purchase threads miss LIVE", "When a purchase event creates a new conversation during broadcasting, the synthetic privateMessage has no originatingRoom.")

	r := Reconcile([]payload.Finding{p}, []ExternalFinding{g})
	if len(r.Findings) != 1 {
		t.Fatalf("got %d findings", len(r.Findings))
	}
	got := r.Findings[0]
	if got.SourceTag != SourceTagBoth || got.MatchedCommentID != 42 {
		t.Errorf("tag=%q matched=%d, want both/42", got.SourceTag, got.MatchedCommentID)
	}
	if got.Finding.ID != "p1" {
		t.Errorf("finding not preserved: %+v", got.Finding)
	}
	if len(r.GreptileOnly) != 0 {
		t.Errorf("GreptileOnly = %+v, want empty", r.GreptileOnly)
	}
}

func TestReconcileFarLineDoesNotMatch(t *testing.T) {
	text := "The synthetic privateMessage has no originatingRoom so purchase threads miss LIVE."
	p := prismFinding("p1", "app/chat.go", 300, text)
	g := greptileFinding(42, "app/chat.go", 118, 120, "Purchase threads miss LIVE", text)

	r := Reconcile([]payload.Finding{p}, []ExternalFinding{g})
	if r.Findings[0].SourceTag != SourceTagPrismOnly || r.Findings[0].MatchedCommentID != 0 {
		t.Errorf("got %+v, want prism-only", r.Findings[0])
	}
	if len(r.GreptileOnly) != 1 || r.GreptileOnly[0].CommentID != 42 {
		t.Errorf("GreptileOnly = %+v", r.GreptileOnly)
	}
}

func TestReconcileLineProximityBoundary(t *testing.T) {
	text := "The synthetic privateMessage has no originatingRoom so purchase threads miss LIVE."
	g := greptileFinding(42, "app/chat.go", 118, 120, "Purchase threads miss LIVE", text)

	for line, want := range map[int]string{108: SourceTagBoth, 130: SourceTagBoth, 107: SourceTagPrismOnly, 131: SourceTagPrismOnly} {
		r := Reconcile([]payload.Finding{prismFinding("p1", "app/chat.go", line, text)}, []ExternalFinding{g})
		if r.Findings[0].SourceTag != want {
			t.Errorf("line %d: tag=%q, want %q", line, r.Findings[0].SourceTag, want)
		}
	}
}

func TestReconcileDifferentFileDoesNotMatch(t *testing.T) {
	text := "The synthetic privateMessage has no originatingRoom so purchase threads miss LIVE."
	p := prismFinding("p1", "app/other.go", 120, text)
	g := greptileFinding(42, "app/chat.go", 118, 120, "Purchase threads miss LIVE", text)

	r := Reconcile([]payload.Finding{p}, []ExternalFinding{g})
	if r.Findings[0].SourceTag != SourceTagPrismOnly {
		t.Errorf("got %+v, want prism-only", r.Findings[0])
	}
	if len(r.GreptileOnly) != 1 {
		t.Errorf("GreptileOnly = %+v", r.GreptileOnly)
	}
}

func TestReconcilePathSuffixAndBasenameMatch(t *testing.T) {
	text := "The synthetic privateMessage has no originatingRoom so purchase threads miss LIVE."
	for name, files := range map[string][2]string{
		"suffix":   {"services/app/chat.go", "app/chat.go"},
		"reverse":  {"chat.go", "services/app/chat.go"},
		"basename": {"other/dir/chat.go", "services/app/chat.go"},
	} {
		p := prismFinding("p1", files[0], 120, text)
		g := greptileFinding(42, files[1], 118, 120, "Purchase threads miss LIVE", text)
		r := Reconcile([]payload.Finding{p}, []ExternalFinding{g})
		if r.Findings[0].SourceTag != SourceTagBoth {
			t.Errorf("%s: tag=%q, want both", name, r.Findings[0].SourceTag)
		}
	}
}

func TestReconcileWholeFilePrismFindingNeverMatches(t *testing.T) {
	text := "The synthetic privateMessage has no originatingRoom so purchase threads miss LIVE."
	p := prismFinding("p1", "app/chat.go", 0, text)
	g := greptileFinding(42, "app/chat.go", 118, 120, "Purchase threads miss LIVE", text)

	r := Reconcile([]payload.Finding{p}, []ExternalFinding{g})
	if r.Findings[0].SourceTag != SourceTagPrismOnly {
		t.Errorf("got %+v, want prism-only", r.Findings[0])
	}
	if len(r.GreptileOnly) != 1 {
		t.Errorf("GreptileOnly = %+v", r.GreptileOnly)
	}
}

func TestReconcileLowSimilarityDoesNotMatch(t *testing.T) {
	p := prismFinding("p1", "app/chat.go", 120, "Unbounded goroutine spawn per request exhausts the scheduler under load.")
	g := greptileFinding(42, "app/chat.go", 118, 120, "Purchase threads miss LIVE", "The synthetic privateMessage has no originatingRoom.")

	r := Reconcile([]payload.Finding{p}, []ExternalFinding{g})
	if r.Findings[0].SourceTag != SourceTagPrismOnly {
		t.Errorf("got %+v, want prism-only", r.Findings[0])
	}
}

func TestReconcileContractSubjectNameMatchesDespiteLowJaccard(t *testing.T) {
	p := prismFinding("p1", "app/chat.go", 120, "Nil dereference when the room lookup returns nothing.")
	p.FindingContract = &types.FindingContract{Subjects: []types.FindingSubject{
		{Kind: "function", Path: "app/chat.go", Name: ""},
		{Kind: "field", Path: "app/chat.go", Name: "originatingRoom"},
	}}
	g := greptileFinding(42, "app/chat.go", 118, 120, "Purchase threads miss LIVE", "The synthetic privateMessage has no OriginatingRoom.")

	if Similarity(p.Comment, g.Title+" "+g.Body) >= 0.20 {
		t.Fatal("test fixture must have low Jaccard")
	}
	r := Reconcile([]payload.Finding{p}, []ExternalFinding{g})
	if r.Findings[0].SourceTag != SourceTagBoth || r.Findings[0].MatchedCommentID != 42 {
		t.Errorf("got %+v, want both/42", r.Findings[0])
	}
}

func TestReconcileEmptySubjectNameNeverMatches(t *testing.T) {
	p := prismFinding("p1", "app/chat.go", 120, "Unbounded goroutine spawn per request exhausts the scheduler under load.")
	p.FindingContract = &types.FindingContract{Subjects: []types.FindingSubject{{Kind: "file", Path: "app/chat.go"}}}
	g := greptileFinding(42, "app/chat.go", 118, 120, "Purchase threads miss LIVE", "The synthetic privateMessage has no originatingRoom.")

	r := Reconcile([]payload.Finding{p}, []ExternalFinding{g})
	if r.Findings[0].SourceTag != SourceTagPrismOnly {
		t.Errorf("got %+v, want prism-only", r.Findings[0])
	}
}

func TestReconcileGreptileFindingMatchesOnlyBestPrismFinding(t *testing.T) {
	weak := prismFinding("weak", "app/chat.go", 119, "Purchase threads created during broadcasting are wrong.")
	strong := prismFinding("strong", "app/chat.go", 126, "When a purchase event creates a new conversation during broadcasting, the synthetic privateMessage has no originatingRoom, so purchase threads miss LIVE.")
	g := greptileFinding(42, "app/chat.go", 118, 120, "Purchase threads miss LIVE", "When a purchase event creates a new conversation during broadcasting, the synthetic privateMessage has no originatingRoom.")

	r := Reconcile([]payload.Finding{weak, strong}, []ExternalFinding{g})
	tags := tagsByID(r)
	if tags["strong"].SourceTag != SourceTagBoth || tags["strong"].MatchedCommentID != 42 {
		t.Errorf("strong = %+v, want both/42", tags["strong"])
	}
	if tags["weak"].SourceTag != SourceTagPrismOnly || tags["weak"].MatchedCommentID != 0 {
		t.Errorf("weak = %+v, want prism-only", tags["weak"])
	}
	if len(r.GreptileOnly) != 0 {
		t.Errorf("GreptileOnly = %+v", r.GreptileOnly)
	}
}

func TestReconcileTieBreaksByClosestLine(t *testing.T) {
	text := "The synthetic privateMessage has no originatingRoom so purchase threads miss LIVE."
	far := prismFinding("far", "app/chat.go", 128, text)
	near := prismFinding("near", "app/chat.go", 121, text)
	g := greptileFinding(42, "app/chat.go", 118, 120, "Purchase threads miss LIVE", text)

	r := Reconcile([]payload.Finding{far, near}, []ExternalFinding{g})
	tags := tagsByID(r)
	if tags["near"].SourceTag != SourceTagBoth {
		t.Errorf("near = %+v, want both", tags["near"])
	}
	if tags["far"].SourceTag != SourceTagPrismOnly {
		t.Errorf("far = %+v, want prism-only", tags["far"])
	}
}

func TestReconcileOneGreptileFindingPerPrismFinding(t *testing.T) {
	text := "The synthetic privateMessage has no originatingRoom so purchase threads miss LIVE."
	p := prismFinding("p1", "app/chat.go", 120, text)
	g1 := greptileFinding(1, "app/chat.go", 118, 120, "Purchase threads miss LIVE", text)
	g2 := greptileFinding(2, "app/chat.go", 122, 122, "Purchase threads miss LIVE", text)

	r := Reconcile([]payload.Finding{p}, []ExternalFinding{g1, g2})
	if r.Findings[0].SourceTag != SourceTagBoth {
		t.Errorf("got %+v, want both", r.Findings[0])
	}
	if len(r.GreptileOnly) != 1 {
		t.Errorf("GreptileOnly = %+v, want exactly one leftover", r.GreptileOnly)
	}
}

func TestReconcilePseudoFilesStayPrismOnly(t *testing.T) {
	text := "Purchase threads miss LIVE because the synthetic privateMessage has no originatingRoom."
	for _, pseudo := range []string{"SUMMARY", "CHECK"} {
		p := prismFinding("p1", pseudo, 0, text)
		g := greptileFinding(42, pseudo, 0, 0, "Purchase threads miss LIVE", text)
		r := Reconcile([]payload.Finding{p}, []ExternalFinding{g})
		if r.Findings[0].SourceTag != SourceTagPrismOnly || r.Findings[0].MatchedCommentID != 0 {
			t.Errorf("%s: got %+v, want prism-only", pseudo, r.Findings[0])
		}
		if len(r.GreptileOnly) != 1 {
			t.Errorf("%s: GreptileOnly = %+v, want the greptile finding left unconsumed", pseudo, r.GreptileOnly)
		}
	}
}

func TestReconcilePreservesPrismOrder(t *testing.T) {
	ps := []payload.Finding{
		prismFinding("a", "x.go", 1, "alpha issue"),
		prismFinding("b", "SUMMARY", 0, "summary text"),
		prismFinding("c", "y.go", 5, "gamma issue"),
	}
	r := Reconcile(ps, nil)
	if len(r.Findings) != 3 {
		t.Fatalf("got %d", len(r.Findings))
	}
	for i, want := range []string{"a", "b", "c"} {
		if r.Findings[i].Finding.ID != want || r.Findings[i].SourceTag != SourceTagPrismOnly {
			t.Errorf("[%d] = %+v", i, r.Findings[i])
		}
	}
}
