package publisher

import (
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/textcut"
)

// The inline title of the 2026-10-14 annoyance audit, anonymised: a
// behaviour-change finding with no headline whose effect sentence puts its
// contrast inside the opening "Where" clause; the title rendered as
// "Behavior change · […] but differ by target_audience ...".
const (
	auditLeadImpact = "Where two enabled campaigns share a trigger and auth×device unit but differ by target_audience or assign_countries, the higher-priority one now holds the slot and the other's audience gets matched=false, whereas main fell through and served it."
	auditLeadTitle  = "The higher-priority one now holds the slot and the other's audience gets matched=false, whereas main fell through and served it"
)

func auditLeadFinding() payload.Finding {
	comment := strings.TrimSuffix(auditLeadImpact, ".") + " The slot lookup now chooses the campaign from the audience unit alone, ignoring the non-axis gates."
	return withContract(f("lead", "medium", "internal/journey/evaluator.go", 278, comment), "production_behavior", "current_impact", auditLeadImpact, "")
}

func TestHeadline_DroppedLeadStartsAtTheProblemClause(t *testing.T) {
	if n := len([]rune(strings.TrimSuffix(auditLeadImpact, "."))); n <= impactTitleMaxRunes {
		t.Fatalf("fixture sentence is %d runes, within the title limit", n)
	}
	fd := auditLeadFinding()
	r := Round{Owner: "acme", Repo: "example", HeadSHA: "abc1234"}

	inline := RenderInline(fd, "prism-only", "", "", "")
	if want := "**[MEDIUM] Behavior change · " + auditLeadTitle + "**"; !strings.Contains(inline, want) {
		t.Fatalf("inline:\n%s\nwant title line:\n%s", inline, want)
	}
	if strings.Contains(inline, "· […]") || strings.Contains(inline, "· but ") {
		t.Fatalf("title opens with the marker or a connective:\n%s", inline)
	}
	if bullet := r.bullet(fd); !strings.Contains(bullet, auditLeadTitle+bulletWhereSep) {
		t.Fatalf("bullet:\n%s", bullet)
	}

	t.Setenv(ProblemClauseStartEnv, "false")
	legacy := RenderInline(fd, "prism-only", "", "", "")
	if !strings.Contains(legacy, "Behavior change · […] but differ by target_audience") {
		t.Fatalf("kill switch does not restore the marked lead:\n%s", legacy)
	}
}

func TestHeadline_NeverOpensWithTheMarkerOrAConnective(t *testing.T) {
	r := Round{Owner: "acme", Repo: "example", HeadSHA: "abc1234"}
	findings := []payload.Finding{
		auditLeadFinding(),
		lowNote("a", "web/src/store/bridge.ts", 493, "no_user_impact", "No runtime impact; the PR description contradicts the implemented behavior.", smokeQuoteNote),
		lowNote("b", "web/src/store/bridge.ts", 493, "future_condition_only", "None today: the overlay is off by default.", smokeGuardNote),
		lowNote("c", "web/src/Preview.tsx", 40, "future_condition_only", "None on browsers that can query mic permission.", auditFlagNote),
		lowNote("d", "app/views/discovery.py", 16, "future_condition_only", "Normal users see no change.", auditYetNote),
		f("e", "medium", "pkg/cache/store.go", 10, "but the cache is never invalidated when the row is deleted, so a deleted row is served until the process restarts and every reader sees it"),
	}
	for _, fd := range findings {
		title := headline(fd, false)
		if textcut.OpensWithMarkerOrConnective(strings.TrimPrefix(title, kindLabels["production_behavior"]+" · ")) {
			t.Errorf("%s: headline opens with the marker or a connective: %q", fd.ID, title)
		}
		text := strings.TrimSpace(strings.Split(strings.SplitN(r.bullet(fd), "** ", 2)[1], bulletWhereSep)[0])
		if textcut.OpensWithMarkerOrConnective(text) {
			t.Errorf("%s: bullet opens with the marker or a connective: %q", fd.ID, text)
		}
	}
}
