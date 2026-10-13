package reconcile

import (
	"testing"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

// The audited case: the pull request body spelled out why the team
// identifier resolves the same way in every payload shape, and the review
// still raised the parent-team mapping's identifier as a folded note.
const teamBody = `## Summary / Background

A catalog team sync needs ` + "`catalogUser.email`" + ` and ` + "`catalogTeam.members`" + `. Today the email is mapped outside ` + "`properties`" + ` (dropped) and teams sync without members.

## Code Changes

- ` + "`catalogTeam.members`" + ` (many, to ` + "`catalogUser`" + `).
- ` + "`team`" + ` kind: ` + "`members: true`" + `. Identifier is ` + "`(.databaseId // .id) | tostring`" + `, which gives ` + "`18780620`" + ` for ` + "`platform-experts`" + ` in all three shapes: the REST payload the catalog holds today, the integration's REST+GraphQL merged payload, and a pure GraphQL response (where ` + "`.id`" + ` is the node ID ` + "`T_kwDOA_yb4s4BHpHM`" + `). ` + "`catalogTeams`" + ` and ` + "`parent_team`" + ` keep resolving.
- ` + "`user`" + ` kind: ` + "`includeSAMLEmail: true`" + `, ` + "`email: .__SAMLEmail // .email`" + `. Users with no SAML link fall back to their email.
<!-- a hidden note -->
`

func teamFinding() payload.Finding {
	return payload.Finding{
		ID: "catalog-prod/mappings/github.yml:17:0c1d2e3f4a5b", Severity: "medium", File: "catalog-prod/mappings/github.yml", Line: 175,
		Comment: "The `team` resource for parent links uses `identifier: .id | tostring` and `parent_team: .parent.id | tostring`, but the main `team` resource (with `members: true`) was updated to use `(.databaseId // .id) | tostring`. When the payload is GraphQL-shaped, `.id` becomes the node ID (e.g., `T_kwDO...`) and only `.databaseId` contains the numeric ID. This causes the two team mappings to key the same teams by different identifiers (node ID vs. databaseId), creating duplicate `catalogTeam` entities and leaving `parent_team` relations pointing at non-canonical entities. Align both the identifier and parent_team relation expressions to use the shape-stable form.",
		FindingContract: &types.FindingContract{FindingKind: "production_behavior", Materiality: "unknown",
			CurrentImpact: "If the parent-team resource receives the merged/GraphQL team payload, teams with parents produce a duplicate `catalogTeam` entity and an unresolved `parent_team` relation; if it receives a REST payload there is no impact.",
			Subjects: []types.FindingSubject{
				{Kind: "file", Path: "catalog-prod/mappings/github.yml"},
				{Kind: "symbol", Path: "catalog-prod/mappings/github.yml", Name: "parent_team"},
				{Kind: "config_key", Path: "catalog-prod/mappings/github.yml", Name: "identifier"},
			}},
		FindingContractStatus: "valid",
	}
}

func TestBodyCovers_TheBodyAlreadyStatesThePoint(t *testing.T) {
	if !BodyCovers(teamFinding(), teamBody) {
		t.Fatal("the team bullet of the body names parent_team and the identifier shapes the note repeats")
	}
}

func TestBodyCovers_ASubjectMentionAloneIsNotCoverage(t *testing.T) {
	body := "## Code Changes\n\n- Drop the unused `parent_team` column from the users table; nothing maps it.\n- Bump the chart version.\n"
	if BodyCovers(teamFinding(), body) {
		t.Fatal("naming the subject in another sentence does not state the finding's point")
	}
}

func TestBodyCovers_TheWholeBodyIsNotOneParagraph(t *testing.T) {
	// The words of the point are all in the body, but spread over several
	// paragraphs; no single paragraph states it.
	body := "- `team` kind: `members: true`.\n- Identifier is `(.databaseId // .id) | tostring`.\n- In a pure GraphQL response `.id` is the node ID.\n- `parent_team` keeps resolving.\n- Only `.databaseId` contains the numeric ID in the merged payload shape.\n"
	if BodyCovers(teamFinding(), body) {
		t.Fatal("coverage is judged one paragraph at a time")
	}
}

func TestBodyCovers_NeedsASpecificSubjectAndAFile(t *testing.T) {
	f := teamFinding()
	f.FindingContract.Subjects = []types.FindingSubject{{Kind: "file", Path: f.File}}
	if BodyCovers(f, teamBody) {
		t.Fatal("a finding with only a file subject cannot be anchored to a paragraph")
	}
	f = teamFinding()
	f.File = "SUMMARY"
	if BodyCovers(f, teamBody) || BodyCovers(teamFinding(), "   ") {
		t.Fatal("narrative and empty bodies never match")
	}
}

func TestBodyParagraphs(t *testing.T) {
	got := BodyParagraphs("## Title\n\nFirst line\ncontinues here.\n\n- item one\n  wraps\n* item two\n1. third\n\n```yaml\nkey: value\n```\n<!-- hidden -->\nLast [link](http://x) ![img](y).\n")
	want := []string{"Title", "First line continues here.", "item one wraps", "item two", "third", "Last link ."}
	if len(got) != len(want) {
		t.Fatalf("BodyParagraphs = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paragraph %d = %q, want %q", i, got[i], want[i])
		}
	}
}
