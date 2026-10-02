package reconcile

import (
	"strings"
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

// A re-review of unchanged code usually rewords a finding; matching against
// the comments PRism itself posted keeps one identity per defect.
func TestAliasPrior_RewordedFindingKeepsItsPublishedIdentity(t *testing.T) {
	own := ParseOwnComments([]ExternalComment{
		{ID: 501, Author: "prism-pr-review-server[bot]", Path: "a.go", Line: 54,
			Body: "<!-- prism:finding:a.go:5:aaaaaaaaaaaa -->\n**[CRITICAL] Behavior change · On desktop, every successful Peer Video start also fires showPreviewDidNotStart and showPreviewStreamStopped, resetting the button to Ready**\n\nreasoning"},
		{ID: 502, Author: "human", Path: "a.go", Line: 54, Body: "no marker here"},
	}, map[int64]bool{501: true, 502: true})
	if len(own) != 1 || own[0].FindingID != "a.go:5:aaaaaaaaaaaa" || own[0].CommentID != 501 {
		t.Fatalf("own comments = %+v", own)
	}
	current := []payload.Finding{
		{ID: "a.go:5:bbbbbbbbbbbb", File: "a.go", Line: 52, Comment: "Clicking Start in the PV setup modal fires showPreviewDidNotStart/showPreviewStreamStopped immediately after starting, resetting the button to Ready and clearing the pending state."},
		{ID: "a.go:9:cccccccccccc", File: "a.go", Line: 98, Comment: "Pressing Escape cancels the whole peer-video start instead of returning to the setup modal."},
	}
	aliases := AliasPrior(current, own)
	if aliases["a.go:5:bbbbbbbbbbbb"] != "a.go:5:aaaaaaaaaaaa" {
		t.Fatalf("reworded finding must alias its published id, got %v", aliases)
	}
	if _, ok := aliases["a.go:9:cccccccccccc"]; ok {
		t.Fatalf("an unrelated finding must not alias")
	}
}

func TestParseOwnComments_TrustsOnlyLedgerCommentIDs(t *testing.T) {
	own := ParseOwnComments([]ExternalComment{
		{ID: 501, Author: "prism-pr-review-server[bot]", Path: "a.go", Line: 5, Body: "<!-- prism:finding:a.go:0:aaaaaaaaaaaa -->\nours"},
		{ID: 777, Author: "human", Path: "a.go", Line: 5, Body: "> <!-- prism:finding:a.go:0:aaaaaaaaaaaa -->\nquoting PRism in a new thread"},
	}, map[int64]bool{501: true})
	if len(own) != 1 || own[0].CommentID != 501 {
		t.Fatalf("a marker outside a ledger comment must not count as ours: %+v", own)
	}
}

func TestAliasPrior_DoesNotAliasASiblingOntoAnIdStillPresent(t *testing.T) {
	own := ParseOwnComments([]ExternalComment{
		{ID: 501, Path: "a.go", Line: 54, Body: "<!-- prism:finding:a.go:5:aaaaaaaaaaaa -->\nClicking Start fires showPreviewDidNotStart and resets the button to Ready"},
	}, map[int64]bool{501: true})
	current := []payload.Finding{
		{ID: "a.go:5:aaaaaaaaaaaa", File: "a.go", Line: 54, Comment: "Clicking Start fires showPreviewDidNotStart and resets the button to Ready"},
		{ID: "a.go:5:bbbbbbbbbbbb", File: "a.go", Line: 56, Comment: "Clicking Start also fires showPreviewStreamStopped which resets the button to Ready"},
	}
	if aliases := AliasPrior(current, own); len(aliases) != 0 {
		t.Fatalf("the published id is still emitted verbatim, so its sibling must keep its own id; got %v", aliases)
	}
}

func TestAliasPrior_RawTextBeatsRenderedBoilerplate(t *testing.T) {
	raw := "Clicking Start in the PV setup modal fires showPreviewDidNotStart immediately after starting, resetting the button to Ready."
	rendered := "**[CRITICAL] Behavior change · every successful Peer Video start also fires showPreviewDidNotStart, resetting the button to Ready** Reasoning and how to verify How to verify: open the modal and press Start. Expected: the button stays Ready. Agent prompt: PRism finding on a.go:54 in acme/example#7: read the review comment marked and decide whether it is valid, and fix it if so; otherwise explain why not. Source: PRism · Both Fix with agent"
	current := []payload.Finding{{ID: "a.go:5:bbbbbbbbbbbb", File: "a.go", Line: 52, Comment: raw}}
	if Similarity(raw, rendered) >= similarityThreshold {
		t.Fatalf("the rendered body must sit under the bar for this test to mean anything: %.2f", Similarity(raw, rendered))
	}
	byRendered := AliasPrior(current, []OwnComment{{FindingID: "a.go:5:aaaaaaaaaaaa", File: "a.go", Line: 54, Text: rendered}})
	byRaw := AliasPrior(current, []OwnComment{{FindingID: "a.go:5:aaaaaaaaaaaa", File: "a.go", Line: 54, Text: "Every successful Peer Video start also fires showPreviewDidNotStart and showPreviewStreamStopped, resetting the button to Ready."}})
	if len(byRendered) != 0 || byRaw["a.go:5:bbbbbbbbbbbb"] != "a.go:5:aaaaaaaaaaaa" {
		t.Fatalf("rendered=%v raw=%v", byRendered, byRaw)
	}
}

func TestAliasPrior_SubjectKeyIgnoresLineAndFile(t *testing.T) {
	contract := &types.FindingContract{FindingKind: "production_behavior", Subjects: []types.FindingSubject{{Name: "Retry"}, {Name: "fetchUser"}}}
	current := []payload.Finding{{ID: "api/users.go:20:bbbbbbbbbbbb", File: "api/users.go", Line: 205, Comment: "No delay between attempts after a 429 response.", FindingContract: contract}}
	own := []OwnComment{{FindingID: "api/users.go:14:aaaaaaaaaaaa", File: "api/users.go", Line: 140, Text: "fetchUser retries without backoff.", Subjects: []string{"fetchuser", "retry"}}}
	if got := AliasPrior(current, own); got[current[0].ID] != own[0].FindingID {
		t.Fatalf("same file, kind and subjects alias at any line: %v", got)
	}
	moved := []payload.Finding{current[0]}
	moved[0].File = "pkg/client/client.go"
	if got := AliasPrior(moved, own); len(got) != 0 {
		t.Fatalf("another file needs the kind on both sides: %v", got)
	}
	own[0].Kind = "production_behavior"
	if got := AliasPrior(moved, own); got[moved[0].ID] != own[0].FindingID {
		t.Fatalf("the same kind and subjects in another file alias too: %v", got)
	}
	own[0].Kind = "test_quality"
	if got := AliasPrior(current, own); len(got) != 0 {
		t.Fatalf("a different finding kind breaks the key: %v", got)
	}
	own[0].Kind = ""
	own[0].Subjects = []string{"fetchuser"}
	if got := AliasPrior(current, own); len(got) != 0 {
		t.Fatalf("the subject sets must match exactly: %v", got)
	}
}

func TestAliasPrior_OneSharedFunctionNeverJoinsTwoFiles(t *testing.T) {
	contract := &types.FindingContract{FindingKind: "production_behavior", Subjects: []types.FindingSubject{{Name: "init"}}}
	current := []payload.Finding{{ID: "b.go:3:bbbbbbbbbbbb", File: "b.go", Line: 30, Comment: "Config is read before the flag parser ran.", FindingContract: contract}}
	own := []OwnComment{{FindingID: "a.go:1:aaaaaaaaaaaa", File: "a.go", Line: 10, Text: "The logger is nil until setup finishes.", Kind: "production_behavior", Subjects: []string{"init"}}}
	if got := AliasPrior(current, own); len(got) != 0 {
		t.Fatalf("two defects that only share an enclosing function in different files stay apart: %v", got)
	}
	own[0].Text = "Config is parsed before the flag parser ran."
	if got := AliasPrior(current, own); got[current[0].ID] != own[0].FindingID {
		t.Fatalf("shared words make the single subject enough: %v", got)
	}
}

func TestAliasPrior_TextMatchWinsOverKeyAndOneSharedWordIsNotEnough(t *testing.T) {
	contract := &types.FindingContract{Subjects: []types.FindingSubject{{Name: "get"}}}
	current := []payload.Finding{{ID: "a.go:7:cccccccccccc", File: "a.go", Line: 72, Comment: "The retry loop in get never backs off after a 429, so every attempt is rejected.", FindingContract: contract}}
	own := []OwnComment{
		{FindingID: "a.go:30:keyed", File: "a.go", Line: 305, Text: "get ignores the context deadline.", Subjects: []string{"get"}},
		{FindingID: "a.go:7:texty", File: "a.go", Line: 70, Text: "The retry loop in get never backs off after a 429 and hammers the upstream."},
	}
	if got := AliasPrior(current, own); got[current[0].ID] != "a.go:7:texty" {
		t.Fatalf("text overlap near the line outranks the subject key: %v", got)
	}
	short := []payload.Finding{{ID: "a.go:1:dddddddddddd", File: "a.go", Line: 11, Comment: "Low thing."}}
	if got := AliasPrior(short, []OwnComment{{FindingID: "a.go:1:eeeeeeeeeeee", File: "a.go", Line: 10, Text: "Critical thing."}}); len(got) != 0 {
		t.Fatalf("one shared word in two short comments is not a match: %v", got)
	}
}

func TestSubjectNames(t *testing.T) {
	got := SubjectNames(&types.FindingContract{Subjects: []types.FindingSubject{{Name: " FetchUser "}, {Name: "retry"}, {Name: "fetchuser"}, {Name: ""}}})
	if len(got) != 2 || got[0] != "fetchuser" || got[1] != "retry" {
		t.Fatalf("SubjectNames = %v", got)
	}
	if SubjectNames(nil) != nil {
		t.Fatal("nil contract has no subjects")
	}
}

func TestAliasPrior_HigherSeverityRestatementClaimsTheRowFirst(t *testing.T) {
	own := []OwnComment{{FindingID: "forms.py:165:aaaaaaaaaaaa", File: "forms.py", Line: 1655, Text: "Re-enabling a membership plan under the toggle wipes the stored preview discount because save only writes changed_data fields."}}
	current := []payload.Finding{
		{ID: "forms.py:166:low", Severity: "low", File: "forms.py", Line: 1665, Comment: "The membership plan toggle check wipes the stored preview discount when save writes changed_data fields and the discount is absent."},
		{ID: "forms.py:165:med", Severity: "medium", File: "forms.py", Line: 1654, Comment: "membership_updates is keyed off changed_data, so re-enabling the membership plan under the toggle wipes the stored preview discount."},
	}
	got := AliasPrior(current, own)
	if got["forms.py:165:med"] != "forms.py:165:aaaaaaaaaaaa" || got["forms.py:166:low"] != "" {
		t.Fatalf("the medium restatement must take the row even when the low note overlaps more: %v", got)
	}
}

func TestParseOwnComments_DropsTheSettleFooterFromTheText(t *testing.T) {
	footers := []string{
		"<sub>Reply <code>intentional</code> or <code>won't fix</code> to settle a thread · <a href=\"https://prism.example/#prism-comments\">Stop PRism comments on your PRs</a></sub>\n",
		"<sub>Reply <code>intentional</code> or <code>won't fix</code> to settle a thread</sub>\n",
		"<sub><a href=\"https://prism.example/#prism-comments\">Stop PRism comments on your PRs</a></sub>\n",
	}
	for _, footer := range footers {
		body := "<!-- prism:finding:a.go:0:aaaaaaaaaaaa -->\n**[MEDIUM] Nil deref when cfg is missing.**\n\n" + footer
		own := ParseOwnComments([]ExternalComment{{ID: 501, Path: "a.go", Line: 5, Body: body}}, map[int64]bool{501: true})
		if len(own) != 1 {
			t.Fatalf("own = %+v", own)
		}
		if strings.Contains(own[0].Text, "settle a thread") || strings.Contains(own[0].Text, "prism-comments") {
			t.Fatalf("footer must not reach the similarity text: %q", own[0].Text)
		}
		if !strings.Contains(own[0].Text, "Nil deref when cfg is missing.") {
			t.Fatalf("stripping must keep the prose: %q", own[0].Text)
		}
	}
}
