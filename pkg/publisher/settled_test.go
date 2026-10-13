package publisher

import (
	"strings"
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

// The audited case, anonymised: PRism raised the desktop deny state on the
// preview component, the author fixed it, PRism conceded in the thread and
// called the remaining fallback "a fair trade"; the next round raised that
// fallback as a new MEDIUM anchored in the hook file.
const (
	settledPreviewFile = "web/src/components/preview/PreviewPlayer.tsx"
	hookFile           = "web/src/hooks/useDevicePermissions.ts"
	previewRootText    = "On desktop, when `isDevicePermissionDenied` is true, the request button is hidden and only a support link remains. However, `useDevicePermissions` sets `isDenied=true` for any dismissal of the browser prompt, not just hard blocks. Since the denied `DeviceAccessModal` routes its \"Got It\" button to `dismissDeviceAccessModal`, a user who dismisses the prompt lands here unable to retry without a reload, unlike mobile which keeps the button. Keep the retry affordance visible so dismissed prompts are recoverable."
	previewReply       = "The NotAllowedError branch on useDevicePermissions.ts:117 now routes through isDeviceBlocked (useDevicePermissions.ts:20), which only returns true when permissions.query reports \"denied\", so a dismissed prompt leaves isDenied false and the desktop request button stays rendered. The test at useDevicePermissions.test.ts:133 covers the dismiss case. One remaining edge: browsers where the device permission is not queryable fall back to denied on line 31, so a dismissal there still hides the button, which seems like a fair trade.\n\n<!-- prism:reply:v1:4263436227 -->"
	fallbackText       = "The `isDeviceBlocked()` fallback returns `true` (blocked) when `navigator.permissions.query({name: \"camera\"})` is unavailable or throws (Safari, Firefox < 132). In those browsers a dismissed prompt is still classified as denied, so the new desktop denied state hides the Request Device Access button and leaves the user without a retry path. This defeats the PR's stated goal of improving retry behavior on desktop. Consider treating an undeterminable permission state as not-blocked so the request button stays available."
	fallbackImpact     = "On Safari and older Firefox, dismissing the device prompt is still treated as denied, so the desktop denied state hides the request button and the user cannot re-request access without leaving the flow."
)

var previewRowID = fid(settledPreviewFile, 126, previewRootText)

func previewRow(state string) db.PublishedFinding {
	return db.PublishedFinding{
		RepoOwner: "acme", RepoName: "example", PRNumber: 7,
		Kind: db.PublishedKindFinding, Fingerprint: previewRowID, SourceTag: SourceTagPrismOnly,
		Severity: "medium", ReviewedSHA: "sha-1", LastSeenSHA: "sha-2", CommentID: 1001, ThreadNodeID: "T1001",
		State: state, PublishedAt: time.Date(2026, 10, 13, 20, 3, 0, 0, time.UTC),
		CommentText: previewRootText, FindingKind: "production_behavior", Subjects: "previewplayer,requestpermission",
	}
}

func fallbackFinding() payload.Finding {
	x := f(fid(hookFile, 31, fallbackText), "medium", hookFile, 31, fallbackText)
	x.FindingContract.CurrentImpact = fallbackImpact
	x.FindingContract.Subjects = []types.FindingSubject{
		{Kind: "file", Path: hookFile},
		{Kind: "symbol", Path: hookFile, Name: "isDeviceBlocked"},
		{Kind: "workflow", Path: "web/src/components/DesktopController.tsx", Name: "desktop device permission"},
	}
	return x
}

func settledRound(head string, row db.PublishedFinding) Round {
	return Round{
		Owner: "acme", Repo: "example", Number: 7, HeadSHA: head,
		Findings:      []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative."), fallbackFinding()},
		Commentable:   map[string]map[int]bool{hookFile: {31: true}},
		Previous:      []db.PublishedFinding{row, {Kind: db.PublishedKindSummary, Fingerprint: "summary", CommentID: 501, LastSeenSHA: "sha-2", Rounds: 2, State: db.PublishedStateOpen}},
		PriorComments: map[string]PriorComment{previewRowID: {Line: 126, Text: previewRootText, OwnReplies: []string{previewReply}}},
		Changes:       changed(hookFile),
	}
}

func TestSettled_ConcededPointRaisedFromAnotherFileIsDropped(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	rep := publishThreads(t, gh, ledger, settledRound("sha-3", previewRow(db.PublishedStateFixed)))
	if rep.InlinePosted != 0 || rep.SettledDropped != 1 || rep.Reopened != 0 || len(gh.reviews) != 0 {
		t.Fatalf("the fallback restates the conceded thread and posts nothing: %+v reviews=%d", rep, len(gh.reviews))
	}
	if replies := gh.repliesTo(1001); len(replies) != 0 {
		t.Fatalf("nothing returns in the settled thread: %v", replies)
	}
	if row := ledger.get(db.PublishedKindFinding, previewRowID); row == nil || row.State != db.PublishedStateFixed {
		t.Fatalf("the settled row is untouched: %+v", row)
	}
	if ledger.get(db.PublishedKindFinding, fallbackFinding().ID) != nil || ledger.get(db.PublishedKindAnnotation, fallbackFinding().ID) != nil {
		t.Fatal("no row is written for the dropped restatement")
	}
	// The conceded fix is counted once, as the table says; the restatement
	// is neither new nor still open.
	if summary := gh.issueEdits[501]; strings.Contains(summary, "Safari") || !strings.Contains(summary, "0 new · 0 still open · 1 fixed") {
		t.Fatalf("the summary neither lists nor counts the dropped finding:\n%s", summary)
	}
}

func TestSettled_DismissedRowSettlesThePointTheSameWay(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	rep := publishThreads(t, gh, ledger, settledRound("sha-3", previewRow(db.PublishedStateDismissed)))
	if rep.InlinePosted != 0 || rep.SettledDropped != 1 {
		t.Fatalf("a dismissed row settles the point: %+v", rep)
	}
}

func TestSettled_OpenRowIsNotSettled(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	rep := publishThreads(t, gh, ledger, settledRound("sha-3", previewRow(db.PublishedStateOpen)))
	if rep.InlinePosted != 1 || rep.SettledDropped != 0 {
		t.Fatalf("an open row is the alias tiers' business, not the settled pass's: %+v", rep)
	}
}

func TestSettled_RootProseWithoutTheReplyDoesNotCarryThePoint(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	r := settledRound("sha-3", previewRow(db.PublishedStateFixed))
	r.PriorComments[previewRowID] = PriorComment{Line: 126, Text: previewRootText}
	rep := publishThreads(t, gh, ledger, r)
	if rep.InlinePosted != 1 || rep.SettledDropped != 0 {
		t.Fatalf("only PRism's own reply named the fallback, so without it the finding is new: %+v", rep)
	}
}

func TestSettled_KillSwitchPostsTheRestatement(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	pol := testPolicy()
	pol.SettledAlias = false
	rep := publishThreadsWith(t, gh, ledger, settledRound("sha-3", previewRow(db.PublishedStateFixed)), pol)
	if rep.InlinePosted != 1 || rep.SettledDropped != 0 {
		t.Fatalf("PUBLISH_SETTLED_ALIAS=false restores the old behaviour: %+v", rep)
	}
}

func TestSettled_ExactAliasStillReopensAFixedRow(t *testing.T) {
	// The same wording on the same lines is a return, which the decision
	// table reopens; the settled pass only sees what the alias tiers left.
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	r := settledRound("sha-3", previewRow(db.PublishedStateFixed))
	back := f(previewRowID, "medium", settledPreviewFile, 126, previewRootText)
	back.FindingContract.Subjects = []types.FindingSubject{{Kind: "symbol", Path: settledPreviewFile, Name: "PreviewPlayer"}, {Kind: "symbol", Path: hookFile, Name: "requestPermission"}}
	r.Findings = []payload.Finding{back}
	r.Commentable[settledPreviewFile] = map[int]bool{126: true}
	rep := publishThreads(t, gh, ledger, r)
	if rep.Reopened != 1 || rep.SettledDropped != 0 || len(gh.repliesTo(1001)) != 1 {
		t.Fatalf("an exact return reopens: %+v replies=%v", rep, gh.repliesTo(1001))
	}
}

func TestSettledAliasFromEnv(t *testing.T) {
	for _, c := range []struct {
		v    string
		want bool
	}{{"", true}, {"true", true}, {"false", false}, {"0", false}, {"OFF", false}} {
		t.Setenv(SettledAliasEnv, c.v)
		if got := SettledAliasFromEnv(); got != c.want {
			t.Errorf("%s=%q: %v, want %v", SettledAliasEnv, c.v, got, c.want)
		}
		t.Setenv(NoteReconcileEnv, c.v)
		if got := NoteReconcileFromEnv(); got != c.want {
			t.Errorf("%s=%q: %v, want %v", NoteReconcileEnv, c.v, got, c.want)
		}
	}
	for _, c := range []struct {
		v    string
		want bool
	}{{"", false}, {"true", true}, {"1", true}, {"false", false}, {"off", false}} {
		t.Setenv(BodyCheckEnv, c.v)
		if got := BodyCheckFromEnv(); got != c.want {
			t.Errorf("%s=%q: %v, want %v", BodyCheckEnv, c.v, got, c.want)
		}
	}
}
