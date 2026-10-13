package reconcile

import (
	"testing"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

// The audited case: PRism conceded a fix in a thread on the preview
// component and called the remaining fallback "a fair trade"; five minutes
// later the agent raised that fallback as a new MEDIUM anchored in the hook
// file. The root's prose alone does not carry the point; PRism's own reply
// in the thread does.
const (
	settledRootText = "On desktop, when `isDevicePermissionDenied` is true, the request button is hidden and only a support link remains. However, `useDevicePermissions` sets `isDenied=true` for any dismissal of the browser prompt, not just hard blocks. Since the denied `DeviceAccessModal` routes its \"Got It\" button to `dismissDeviceAccessModal`, a user who dismisses the prompt lands here unable to retry without a reload, unlike mobile which keeps the button. Keep the retry affordance visible so dismissed prompts are recoverable."
	settledReply    = "The NotAllowedError branch on useDevicePermissions.ts:117 now routes through isDeviceBlocked (useDevicePermissions.ts:20), which only returns true when permissions.query reports \"denied\", so a dismissed prompt leaves isDenied false and the desktop request button stays rendered. The test at useDevicePermissions.test.ts:133 covers the dismiss case. One remaining edge: browsers where the device permission is not queryable fall back to denied on line 31, so a dismissal there still hides the button, which seems like a fair trade."
	fallbackText    = "The `isDeviceBlocked()` fallback returns `true` (blocked) when `navigator.permissions.query({name: \"camera\"})` is unavailable or throws (Safari, Firefox < 132). In those browsers a dismissed prompt is still classified as denied, so the new desktop denied state hides the Request Device Access button and leaves the user without a retry path. This defeats the PR's stated goal of improving retry behavior on desktop. Consider treating an undeterminable permission state as not-blocked so the request button stays available."
	fallbackImpact  = "On Safari and older Firefox, dismissing the device prompt is still treated as denied, so the desktop denied state hides the request button and the user cannot re-request access without leaving the flow."
)

func fallbackFinding() payload.Finding {
	return payload.Finding{
		ID: "web/src/hooks/useDevicePermissions.ts:3:593067713e57", Severity: "medium", File: "web/src/hooks/useDevicePermissions.ts", Line: 31,
		Comment: fallbackText,
		FindingContract: &types.FindingContract{FindingKind: "production_behavior", Materiality: "current_impact", CurrentImpact: fallbackImpact,
			Subjects: []types.FindingSubject{
				{Kind: "file", Path: "web/src/hooks/useDevicePermissions.ts"},
				{Kind: "symbol", Path: "web/src/hooks/useDevicePermissions.ts", Name: "isDeviceBlocked"},
				{Kind: "workflow", Path: "web/src/components/DesktopController.tsx", Name: "desktop device permission"},
			}},
		FindingContractStatus: "valid",
	}
}

func concededPoint(withReply bool) SettledPoint {
	text := settledRootText
	if withReply {
		text += " " + settledReply
	}
	return SettledPoint{FindingID: "web/src/components/preview/PreviewPlayer.tsx:12:571d811f96f7", File: "web/src/components/preview/PreviewPlayer.tsx",
		Kind: "production_behavior", Subjects: []string{"previewplayer", "requestpermission"}, Text: text}
}

func TestMatchSettled_ConcededPointRestatedFromAnotherFile(t *testing.T) {
	got := MatchSettled([]payload.Finding{fallbackFinding()}, []SettledPoint{concededPoint(true)})
	if got[fallbackFinding().ID] != concededPoint(true).FindingID {
		t.Fatalf("the fallback finding restates the conceded thread: %v", got)
	}
}

func TestMatchSettled_RootProseAloneDoesNotCarryThePoint(t *testing.T) {
	if got := MatchSettled([]payload.Finding{fallbackFinding()}, []SettledPoint{concededPoint(false)}); len(got) != 0 {
		t.Fatalf("without PRism's reply the thread never named the fallback: %v", got)
	}
}

func TestMatchSettled_SharedSymbolWithoutTheTextIsNotAMatch(t *testing.T) {
	other := SettledPoint{FindingID: "web/src/hooks/useDevicePermissions.ts:1:aaaaaaaaaaaa", File: "web/src/hooks/useDevicePermissions.ts", Kind: "production_behavior",
		Subjects: []string{"isdeviceblocked"},
		Text:     "isDeviceBlocked is called on every render because the memo dependency list omits the stream id, so the permission query runs again each time the preview re-renders and the console fills with warnings."}
	if got := MatchSettled([]payload.Finding{fallbackFinding()}, []SettledPoint{other}); len(got) != 0 {
		t.Fatalf("a distinct defect on the same symbol is not the settled point: %v", got)
	}
}

func TestMatchSettled_KindMismatchNeverMatches(t *testing.T) {
	p := concededPoint(true)
	p.Kind = "test_quality"
	if got := MatchSettled([]payload.Finding{fallbackFinding()}, []SettledPoint{p}); len(got) != 0 {
		t.Fatalf("a test-quality row is not a production-behavior point: %v", got)
	}
	p.Kind = ""
	if got := MatchSettled([]payload.Finding{fallbackFinding()}, []SettledPoint{p}); len(got) != 1 {
		t.Fatalf("an unknown kind on the row does not block the match: %v", got)
	}
}

func TestMatchSettled_FileAndWorkflowSubjectsCarryNoEvidence(t *testing.T) {
	f := fallbackFinding()
	f.FindingContract.Subjects = []types.FindingSubject{{Kind: "file", Path: f.File}, {Kind: "workflow", Name: "desktop device permission"}}
	p := concededPoint(true)
	p.Text += " desktop device permission"
	if got := MatchSettled([]payload.Finding{f}, []SettledPoint{p}); len(got) != 0 {
		t.Fatalf("a bare file or workflow subject never joins two findings: %v", got)
	}
}

func TestMatchSettled_PseudoFilesNeverMatch(t *testing.T) {
	f := fallbackFinding()
	f.File = "SUMMARY"
	if got := MatchSettled([]payload.Finding{f}, []SettledPoint{concededPoint(true)}); len(got) != 0 {
		t.Fatalf("summary narrative is never aliased: %v", got)
	}
}

func TestNamesIdentifier(t *testing.T) {
	cases := []struct {
		text, name string
		want       bool
	}{
		{"routes through isdeviceblocked (usedevicepermissions.ts:20)", "isdeviceblocked", true},
		{"the isdeviceblockedfallback helper", "isdeviceblocked", false},
		{"calls .devicemodal--desktop here", ".devicemodal--desktop", true},
		{"nothing here", "isdeviceblocked", false},
		{"parent_team keeps resolving", "parent_team", true},
		{"parent_team_id keeps resolving", "parent_team", false},
	}
	for _, c := range cases {
		if got := NamesIdentifier(c.text, c.name); got != c.want {
			t.Errorf("NamesIdentifier(%q, %q) = %v, want %v", c.text, c.name, got, c.want)
		}
	}
}

func TestSpecificSubjects(t *testing.T) {
	c := &types.FindingContract{Subjects: []types.FindingSubject{
		{Kind: "file", Path: "a.go"}, {Kind: "symbol", Name: "Zed"}, {Kind: "symbol", Name: " FetchUser "}, {Kind: "selector", Name: ".Modal--desktop"},
		{Kind: "workflow", Name: "checkout flow"}, {Kind: "config_key", Name: "fetchuser"},
	}}
	got := SpecificSubjects(c)
	if len(got) != 2 || got[0] != ".modal--desktop" || got[1] != "fetchuser" {
		t.Fatalf("SpecificSubjects = %v", got)
	}
	if SpecificSubjects(nil) != nil {
		t.Fatal("nil contract has no subjects")
	}
}

func TestIsOwnReply(t *testing.T) {
	if !IsOwnReply("Fixed as described.\n\n<!-- prism:reply:v1:4263436227 -->") || IsOwnReply("Not seen at abc1234.") || IsOwnReply("<!-- prism:external:a.go:1:abc -->\nPRism found the same point.") {
		t.Fatal("only the reply policy's marker identifies PRism's own reply")
	}
}
