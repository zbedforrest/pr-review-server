package publisher

import (
	"context"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/reconcile"
	"pr-review-server/pkg/reviewer/types"
)

// The audited case, anonymised: Greptile raised the buyer lookup, the author
// answered, Greptile withdrew and the thread was resolved; four days later a
// lite review raised the same point as a folded MEDIUM note.
const (
	modelsFile = "store/models.py"
	buyerText  = "The `record_club_bulk` function reads the mirrored `buyer_id` field unconditionally to detect which users have already bought a set, but `buyer_id` is a `MirrorBigIntegerField` that is only populated on `save()`/`bulk_create()` and may be NULL for rows written before backfill completion or via `update()`/`bulk_update()`. This bypasses the `READ_SWAP` switch that guards all other purchase reads (see `Purchases._active` in `store/api/feed.py`). When the switch is off and mirror rows remain NULL, buyers are not skipped, causing the function to insert a `CLUB` unread row for a set they already own, lighting the Club dot/NEW badge for bought sets. Gate this lookup on the same switch, using `buyer_id` when the switch is active and `user_id` when it is off."
)

func buyerNote() payload.Finding {
	x := f(fid(modelsFile, 241, buyerText), "medium", modelsFile, 241, buyerText)
	x.FindingContract.Materiality = "unknown"
	x.FindingContract.CurrentImpact = "When the buyer mirror is unpopulated and the read switch is off, buyers of a club set still receive a CLUB unread row, so the Club tab lights a dot and NEW badge for a set they bought."
	x.FindingContract.Subjects = []types.FindingSubject{
		{Kind: "symbol", Path: modelsFile, Name: "record_club_bulk"},
		{Kind: "symbol", Path: "store/api/feed.py", Name: "Purchases"},
		{Kind: "config_key", Path: "store/api/feed.py", Name: "READ_SWAP"},
	}
	return x
}

func greptileBuyerRoot(id int64) reconcile.ExternalFinding {
	return reconcile.ExternalFinding{Source: reconcile.SourceGreptile, CommentID: id, Severity: "medium", File: modelsFile, StartLine: 238, EndLine: 241,
		Title: "Older buyers get wrong badges",
		Body:  "`record_club_bulk` reads buyers only from `buyer_id`, even when the buyer read switch is off. If older purchases still have a null `buyer_id`, this query misses those buyers. The existing purchase reader uses `user_id` until those rows are backfilled. A member who bought before joining the toggle cohort has no purchase state row. Publishing that set for club members then creates a `CLUB` unread row, so the bought set shows NEW on Club instead of Purchased. Use `user_id` while the read switch is off, or fall back to it for null `buyer_id` values."}
}

func noteRound(head string, roots ...reconcile.ExternalFinding) Round {
	return Round{
		Owner: "acme", Repo: "example", Number: 7, HeadSHA: head,
		Findings:    []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative."), buyerNote()},
		Commentable: map[string]map[int]bool{modelsFile: {241: true}},
		External:    roots,
	}
}

func TestNotes_FoldedNoteOnAResolvedThreadIsSettled(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	gh.extra = []ReviewThread{{NodeID: "T900", RootCommentID: 900, Resolved: true}}
	rep := publishThreads(t, gh, ledger, noteRound("sha-1", greptileBuyerRoot(900)))
	if rep.ExternalSettled != 1 || rep.ExternalMatched != 0 || rep.ExternalReplies != 0 || rep.InlinePosted != 0 || len(gh.repliesTo(900)) != 0 {
		t.Fatalf("the note restates a withdrawn thread: %+v replies=%v", rep, gh.repliesTo(900))
	}
	summary := gh.issueCreates[0]
	if strings.Contains(summary, "folded note") || strings.Contains(summary, "record_club_bulk") || strings.Contains(summary, "already raised") {
		t.Fatalf("a settled point is neither folded nor linked:\n%s", summary)
	}
	row := ledger.get(db.PublishedKindAnnotation, buyerNote().ID)
	if row == nil || row.State != db.PublishedStateExternal || row.ExternalCommentID != 900 || row.SourceTag != "external:greptile" {
		t.Fatalf("the point is remembered as external: %+v", row)
	}
}

func TestNotes_FoldedNoteOnAnOpenThreadStaysFolded(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	gh.extra = []ReviewThread{{NodeID: "T900", RootCommentID: 900}}
	rep := publishThreads(t, gh, ledger, noteRound("sha-1", greptileBuyerRoot(900)))
	if rep.ExternalMatched != 0 || rep.ExternalSettled != 0 || len(gh.repliesTo(900)) != 0 {
		t.Fatalf("an open thread is no candidate for a note: %+v replies=%v", rep, gh.repliesTo(900))
	}
	if summary := gh.issueCreates[0]; !strings.Contains(summary, "1 folded note") || strings.Contains(summary, "already raised") {
		t.Fatalf("the note folds as before and claims no root:\n%s", summary)
	}
	if ledger.get(db.PublishedKindAnnotation, buyerNote().ID) != nil {
		t.Fatal("no external row is written for a note on an open thread")
	}
}

func TestNotes_ANoteNeverTakesTheDeltaReplyFromALaterShownFinding(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	gh.extra = []ReviewThread{{NodeID: "T900", RootCommentID: 900}}
	publishThreads(t, gh, ledger, noteRound("sha-1", greptileBuyerRoot(900)))
	r := noteRound("sha-2", greptileBuyerRoot(900))
	shown := buyerNote()
	shown.FindingContract.Materiality = "current_impact"
	r.Findings = []payload.Finding{r.Findings[0], shown}
	rep := publishThreads(t, gh, ledger, r)
	if rep.ExternalMatched != 1 || rep.ExternalReplies != 1 || len(gh.repliesTo(900)) != 1 {
		t.Fatalf("the shown finding still earns the thread's one delta reply: %+v replies=%v", rep, gh.repliesTo(900))
	}
}

func TestNotes_AShownFindingMatchesBeforeANoteTakesTheRoot(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	gh.extra = []ReviewThread{{NodeID: "T900", RootCommentID: 900}}
	r := noteRound("sha-1", greptileBuyerRoot(900))
	shown := buyerNote()
	shown.Comment = "`record_club_bulk` reads buyers only from `buyer_id` even when the read switch is off, so older purchases with a null `buyer_id` are missed and the member gets a `CLUB` unread row."
	shown.ID = fid(modelsFile, 240, shown.Comment)
	shown.Line = 240
	shown.FindingContract.Materiality = "current_impact"
	r.Findings = append(r.Findings, shown)
	r.Commentable[modelsFile][240] = true
	rep := publishThreads(t, gh, ledger, r)
	if rep.ExternalMatched != 1 || rep.ExternalReplies != 1 || rep.InlinePosted != 0 {
		t.Fatalf("the shown finding takes the root and its delta reply; the note folds: %+v", rep)
	}
	if row := ledger.get(db.PublishedKindAnnotation, shown.ID); row == nil || row.ExternalCommentID != 900 {
		t.Fatalf("the shown finding owns the external row: %+v", row)
	}
	if !strings.Contains(gh.issueCreates[0], "1 folded note") {
		t.Fatalf("the note stays folded:\n%s", gh.issueCreates[0])
	}
}

func TestNotes_ShownFindingOnAResolvedThreadStillPosts(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	gh.extra = []ReviewThread{{NodeID: "T900", RootCommentID: 900, Resolved: true}}
	rep := publishThreads(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)))
	if rep.ExternalSettled != 0 || rep.ExternalMatched != 0 || rep.InlinePosted != 2 || len(gh.repliesTo(900)) != 0 {
		t.Fatalf("a resolved thread settles notes only; a finding above the bar gets its own root: %+v", rep)
	}
}

func TestNotes_SettledExternalRowIsNotLinkedOnLaterRounds(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	gh.extra = []ReviewThread{{NodeID: "T900", RootCommentID: 900, Resolved: true}}
	publishThreads(t, gh, ledger, noteRound("sha-1", greptileBuyerRoot(900)))
	rep := publishThreads(t, gh, ledger, noteRound("sha-2", greptileBuyerRoot(900)))
	if rep.ExternalSettled != 0 || rep.ExternalMatched != 0 || rep.InlinePosted != 0 {
		t.Fatalf("the second round aliases to the external row: %+v", rep)
	}
	if summary := gh.issueEdits[501]; strings.Contains(summary, "already raised") || strings.Contains(summary, "record_club_bulk") {
		t.Fatalf("a settled point stays off the summary:\n%s", summary)
	}
	if len(ledger.rows) != 2 {
		t.Fatalf("one external row and the summary: %d rows", len(ledger.rows))
	}
}

func TestNotes_OpenExternalRowStopsBeingLinkedOnceTheThreadResolves(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	gh.extra = []ReviewThread{{NodeID: "T900", RootCommentID: 900}}
	publishThreads(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)))
	if !strings.Contains(gh.issueCreates[0], "already raised") {
		t.Fatalf("the open thread is linked first:\n%s", gh.issueCreates[0])
	}
	gh.extra[0].Resolved = true
	publishThreads(t, gh, ledger, externalRound("sha-2", greptileRetryRoot(900)))
	if summary := gh.issueEdits[501]; strings.Contains(summary, "already raised") {
		t.Fatalf("a point the other reviewer settled since is no longer listed:\n%s", summary)
	}
}

func TestNotes_SummaryRefreshDropsTheLinkOfASettledRow(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	gh.extra = []ReviewThread{{NodeID: "T900", RootCommentID: 900}}
	publishThreads(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)))
	gh.existingIssueComments = []IssueComment{{ID: 501, Body: gh.issueCreates[0]}}
	gh.extra[0].Resolved = true
	p := &Publisher{GH: gh, Ledger: ledger, Policy: testPolicy()}
	if err := p.RefreshSummary(context.Background(), externalRound("", greptileRetryRoot(900))); err != nil {
		t.Fatal(err)
	}
	if out := gh.issueEdits[501]; strings.Contains(out, "already raised") {
		t.Fatalf("the refresh reads the thread state too:\n%s", out)
	}
}

func TestNotes_KillSwitchListsNoThreadsForExternalRows(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	gh.extra = []ReviewThread{{NodeID: "T900", RootCommentID: 900}}
	publishThreads(t, gh, ledger, externalRound("sha-1", greptileRetryRoot(900)))
	gh.existingIssueComments = []IssueComment{{ID: 501, Body: "stale"}}
	pol := testPolicy()
	pol.NoteReconcile = false
	gh.listings = 0
	p := &Publisher{GH: gh, Ledger: ledger, Policy: pol}
	if err := p.RefreshSummary(context.Background(), externalRound("", greptileRetryRoot(900))); err != nil {
		t.Fatal(err)
	}
	if gh.listings != 0 {
		t.Fatalf("PUBLISH_NOTE_RECONCILE=false: the refresh listed threads %d times", gh.listings)
	}
	if out := gh.issueEdits[501]; !strings.Contains(out, "already raised") {
		t.Fatalf("the external link is kept as before:\n%s", out)
	}
}

func TestNotes_KillSwitchKeepsTheFoldedNote(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	gh.extra = []ReviewThread{{NodeID: "T900", RootCommentID: 900, Resolved: true}}
	pol := testPolicy()
	pol.NoteReconcile = false
	rep := publishThreadsWith(t, gh, ledger, noteRound("sha-1", greptileBuyerRoot(900)), pol)
	if rep.ExternalSettled != 0 || rep.ExternalMatched != 0 || !strings.Contains(gh.issueCreates[0], "1 folded note") {
		t.Fatalf("PUBLISH_NOTE_RECONCILE=false folds the note as before: %+v\n%s", rep, gh.issueCreates[0])
	}
}

// The audited case, anonymised: the pull request body explained why the team
// identifier resolves the same way in every payload shape, and a lite
// review still raised the parent-team mapping as a folded note.
const (
	mappingFile = "catalog-prod/mappings/github.yml"
	teamText    = "The `team` resource for parent links uses `identifier: .id | tostring` and `parent_team: .parent.id | tostring`, but the main `team` resource (with `members: true`) was updated to use `(.databaseId // .id) | tostring`. When the payload is GraphQL-shaped, `.id` becomes the node ID (e.g., `T_kwDO...`) and only `.databaseId` contains the numeric ID. This causes the two team mappings to key the same teams by different identifiers (node ID vs. databaseId), creating duplicate `catalogTeam` entities and leaving `parent_team` relations pointing at non-canonical entities. Align both the identifier and parent_team relation expressions to use the shape-stable form."
	teamBody    = "## Code Changes\n\n- `catalogTeam.members` (many, to `catalogUser`).\n- `team` kind: `members: true`. Identifier is `(.databaseId // .id) | tostring`, which gives `18780620` for `platform-experts` in all three shapes: the REST payload the catalog holds today, the integration's REST+GraphQL merged payload, and a pure GraphQL response (where `.id` is the node ID `T_kwDOA_yb4s4BHpHM`). `catalogTeams` and `parent_team` keep resolving.\n- `user` kind: `includeSAMLEmail: true`.\n"
)

func teamNote() payload.Finding {
	x := f(fid(mappingFile, 175, teamText), "medium", mappingFile, 175, teamText)
	x.FindingContract.Materiality = "unknown"
	x.FindingContract.CurrentImpact = "If the parent-team resource receives the merged/GraphQL team payload, teams with parents produce a duplicate `catalogTeam` entity and an unresolved `parent_team` relation; if it receives a REST payload there is no impact."
	x.FindingContract.Subjects = []types.FindingSubject{
		{Kind: "file", Path: mappingFile},
		{Kind: "symbol", Path: mappingFile, Name: "parent_team"},
		{Kind: "config_key", Path: mappingFile, Name: "identifier"},
	}
	return x
}

func bodyRound(head string, note payload.Finding) Round {
	return Round{
		Owner: "acme", Repo: "example", Number: 7, HeadSHA: head,
		Findings:    []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative."), note},
		Commentable: map[string]map[int]bool{mappingFile: {175: true}},
		PRBody:      teamBody,
	}
}

func bodyPolicy() Policy {
	pol := testPolicy()
	pol.BodyCheck = true
	return pol
}

func TestNotes_ANoteThePullRequestBodyStatesIsHeld(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	rep := publishThreadsWith(t, gh, ledger, bodyRound("sha-1", teamNote()), bodyPolicy())
	if rep.BodyCovered != 1 || rep.InlinePosted != 0 {
		t.Fatalf("the body already says it: %+v", rep)
	}
	if summary := gh.issueCreates[0]; strings.Contains(summary, "folded note") || strings.Contains(summary, "parent_team") {
		t.Fatalf("a held note is not folded:\n%s", summary)
	}
	if len(ledger.rows) != 1 {
		t.Fatalf("only the summary row is written: %d", len(ledger.rows))
	}
}

func TestNotes_AFindingAboveTheInlineBarIsNotHeldByTheBody(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	shown := teamNote()
	shown.FindingContract.Materiality = "current_impact"
	rep := publishThreadsWith(t, gh, ledger, bodyRound("sha-1", shown), bodyPolicy())
	if rep.BodyCovered != 0 || rep.InlinePosted != 1 {
		t.Fatalf("a stated behaviour can still be a defect worth a thread: %+v", rep)
	}
}

func TestNotes_ABodyThatOnlyNamesTheSubjectHoldsNothing(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	r := bodyRound("sha-1", teamNote())
	r.PRBody = "- Drop the unused `parent_team` column; nothing maps it.\n"
	rep := publishThreadsWith(t, gh, ledger, r, bodyPolicy())
	if rep.BodyCovered != 0 || !strings.Contains(gh.issueCreates[0], "1 folded note") {
		t.Fatalf("naming the subject is not stating the point: %+v\n%s", rep, gh.issueCreates[0])
	}
}

func TestNotes_BodyCheckIsOffByDefault(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	rep := publishThreads(t, gh, ledger, bodyRound("sha-1", teamNote()))
	if rep.BodyCovered != 0 || !strings.Contains(gh.issueCreates[0], "1 folded note") {
		t.Fatalf("without PUBLISH_BODY_CHECK the note folds as before: %+v", rep)
	}
}
