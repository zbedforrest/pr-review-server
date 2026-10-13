package publisher

import (
	"os"
	"sort"
	"strings"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

// Greptile posts about half an inline comment per PR; the caps and the
// current-impact gate below are what keep PRism in that range. The per-round
// cap bounds one push, the per-PR cap the whole life of the PR.
const (
	DefaultInlineCap         = 3
	DefaultInlinePRCap       = 6
	DefaultInlineMinSeverity = "medium"

	summaryFile = "SUMMARY"
	checkFile   = "CHECK"
)

type Policy struct {
	InlineCap         int
	InlineMinSeverity string
	// InlinePRCap bounds the inline roots posted over the life of a PR, in
	// every state; the round cap shrinks to what is left. Zero is no bound,
	// negative the default (PUBLISH_INLINE_PR_BUDGET=false sets zero).
	InlinePRCap int
	// ShowUnverified folds the active first-pass claims the agent did not
	// confirm into the summary comment; they are never inline.
	ShowUnverified bool
	// LegacyTitles restores the pre-2026-10 titles and bullets: the effect
	// sentence verbatim, the bare kind label when it did not fit.
	LegacyTitles bool
	// RepublishSameCommit lets Publish run a second round for a head the
	// summary row already records; off, such a round is refused.
	RepublishSameCommit bool
	// LegacyLedger restores the pre-ledger-memory publisher (PUBLISH_POLICY_V2
	// set to false): open rows only, absence means fixed, reposts on return.
	LegacyLedger bool
	// ResolveThreads lets the ledger policy resolve a fixed finding's GitHub
	// thread and reopen it when the finding returns (PUBLISH_THREAD_RESOLUTION);
	// it needs a GitHub that implements ThreadResolver.
	ResolveThreads bool
	// ResolveNotes posts the one-line "Not seen at <sha7>" reply into a thread
	// the round resolves (PUBLISH_RESOLVE_NOTES, off by default); off, the
	// thread is resolved and the ledger moves with nothing posted.
	ResolveNotes bool
	// ExternalReconcile matches findings against every other reviewer's root
	// thread (PUBLISH_EXTERNAL_RECONCILE): a match becomes an external ledger
	// row with one delta reply in that thread and never a root of its own.
	// Off, only Greptile roots are parsed and a match merely tags the finding.
	ExternalReconcile bool
	// PriorIDAlias lets a finding whose contract names a prior_id take that
	// ledger row's identity before the text and subject matching runs
	// (PUBLISH_PRIOR_ID_ALIAS, on unless set to false). The agent only emits
	// a prior_id it was shown (AGENT_PRIOR_FINDINGS), so off, the finding is
	// matched like any other.
	PriorIDAlias bool
	// FixedByFile restores the pre-W6 fix rule (PUBLISH_FIXED_NEEDS_HUNK set
	// to false): an absent finding is fixed when its file changed at all,
	// whatever lines the change touched.
	FixedByFile bool
	// LegacyStatusNotes restores the pre-W6 thread notes
	// (PUBLISH_STATUS_NOTES_V2 set to false): a return and a severity change
	// on one thread are two replies, and a fixed row reopens for any finding
	// the alias matches, whatever subject it names.
	LegacyStatusNotes bool
	// LegacyClamp restores the pre-W6 cuts (PUBLISH_RENDER_CLAMP_V2 set to
	// false): bullets and headlines cut at a word boundary with "...".
	LegacyClamp bool
}

// PolicyV2Env is the kill switch for the ledger decision table: "false", "0",
// "off" and "no" restore the pre-ledger publisher; anything else keeps it on.
const PolicyV2Env = "PUBLISH_POLICY_V2"

// LegacyLedgerFromEnv reports whether the environment turned the ledger
// decision table off. The poller resolves it once at start-up; the library
// never reads the environment itself.
func LegacyLedgerFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(PolicyV2Env))) {
	case "false", "0", "off", "no":
		return true
	}
	return false
}

// ExternalReconcileEnv is the kill switch for cross-reviewer reconciliation:
// "false", "0", "off" and "no" restore the Greptile-only tagging; anything
// else keeps it on.
const ExternalReconcileEnv = "PUBLISH_EXTERNAL_RECONCILE"

// ExternalReconcileFromEnv reports whether the environment left
// cross-reviewer reconciliation on. The poller resolves it once at start-up.
func ExternalReconcileFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ExternalReconcileEnv))) {
	case "false", "0", "off", "no":
		return false
	}
	return true
}

// DefaultPolicy is the shipped posting policy: three inline comments per
// round and six per PR, medium severity and above, unverified first-pass
// claims folded, the ledger decision table, thread resolution and
// cross-reviewer reconciliation on.
func DefaultPolicy() Policy {
	return Policy{InlineCap: DefaultInlineCap, InlinePRCap: DefaultInlinePRCap, InlineMinSeverity: DefaultInlineMinSeverity, ShowUnverified: true, ResolveThreads: true, ExternalReconcile: true, PriorIDAlias: true}
}

// withDefaults fills unset fields only. A zero round cap is a real setting
// (post nothing inline) and a zero PR cap is no bound; negative means unset.
// ShowUnverified is taken as given.
func (p Policy) withDefaults() Policy {
	if p.InlineCap < 0 {
		p.InlineCap = DefaultInlineCap
	}
	if p.InlinePRCap < 0 {
		p.InlinePRCap = DefaultInlinePRCap
	}
	if p.InlineMinSeverity == "" {
		p.InlineMinSeverity = DefaultInlineMinSeverity
	}
	return p
}

// forRound is the policy one round selects under: the round cap, lowered to
// whatever the PR cap still allows after the inline roots the ledger holds.
func (p Policy) forRound(previous []db.PublishedFinding) Policy {
	p = p.withDefaults()
	if p.InlinePRCap <= 0 {
		return p
	}
	if left := p.InlinePRCap - InlineRootsPosted(previous); left < p.InlineCap {
		p.InlineCap = max(left, 0)
	}
	return p
}

// InlineRootsPosted counts the inline root comments the ledger records for
// the PR, in any state: a resolved or dismissed thread still took the
// reviewer's attention once.
func InlineRootsPosted(previous []db.PublishedFinding) int {
	n := 0
	for _, row := range previous {
		if row.Kind == db.PublishedKindFinding && row.CommentID != 0 {
			n++
		}
	}
	return n
}

type Selection struct {
	Inline      []payload.Finding
	Annotations []payload.Finding
}

// severityRank orders critical > medium > low > unknown; higher is worse.
func severityRank(sev string) int {
	switch sev {
	case "critical":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func isNarrative(f payload.Finding) bool {
	return f.File == summaryFile || f.File == checkFile
}

// Publishable reports whether a finding may be asserted on GitHub as a
// bullet or inline comment. Only active confirmed findings from the agent or
// a VIOLATED check qualify; unverified claims (first-pass re-admissions and
// carried findings) have their own fold, first-pass and mechanical alerts
// never post, and a raw bug-memory alert is not a finding.
func Publishable(f payload.Finding) bool {
	// Only confirmed claims are asserted as bullets; unverified ones have
	// their own fold (UnverifiedNote) and must not appear twice.
	if !f.Active || isNarrative(f) || (f.State != "" && f.State != "confirmed") {
		return false
	}
	switch f.Provenance {
	case "first-pass", "mechanical":
		return false
	case "required-check":
		return !isBugMemoryAlert(f)
	}
	return true
}

// An unanswered memory-derived check is re-admitted as its raw alert text;
// only checks the agent answered VIOLATED are confirmed.
func isBugMemoryAlert(f payload.Finding) bool {
	return strings.HasPrefix(commentText(f), "**Bug-memory alert")
}

// UnverifiedNote reports whether a finding belongs in the summary's folded
// unverified section: an active first-pass claim the agent left unverified or
// disputed. These are shown with a status marker, never asserted or inline.
func UnverifiedNote(f payload.Finding) bool {
	return f.Active && f.State == "unverified" && !isNarrative(f) && !isBugMemoryAlert(f)
}

func sortBySeverity(fs []payload.Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
			return ra > rb
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
}

func Select(findings []payload.Finding, alreadyPublished map[string]bool, commentable map[string]map[int]bool, p Policy) Selection {
	p = p.withDefaults()
	minRank := severityRank(p.InlineMinSeverity)

	var candidates, rest []payload.Finding
	for _, f := range findings {
		if !Shown(f) || alreadyPublished[f.ID] {
			continue
		}
		if severityRank(f.Severity) >= minRank && f.Line > 0 && commentable[f.File][f.Line] && worthInline(f) {
			candidates = append(candidates, f)
		} else {
			rest = append(rest, f)
		}
	}
	sortBySeverity(candidates)

	var sel Selection
	if len(candidates) > p.InlineCap {
		rest = append(rest, candidates[p.InlineCap:]...)
		candidates = candidates[:p.InlineCap]
	}
	sel.Inline = candidates
	sortBySeverity(rest)
	sel.Annotations = rest
	return sel
}

// Shown is the bar for an inline comment or a summary bullet: a medium or
// critical finding whose contract asserts current production impact and
// names an experiment. Lower findings and self-discounted ones appear only
// in the summary's folded notes and on the dashboard. With the discount
// gate off, any critical is shown.
func Shown(f payload.Finding) bool {
	if !Publishable(f) {
		return false
	}
	if !types.DiscountGateEnabled() {
		return f.Severity == "critical" || (f.Severity == "medium" && worthInline(f))
	}
	return (f.Severity == "critical" || f.Severity == "medium") && worthInline(f)
}

// worthInline is the Greptile-style bar for occupying a reviewer's diff view:
// the agent must assert an impact that exists today on a behavior or security
// finding, state how to falsify it, and not hedge it away in the same breath.
// Latent hazards, design and test notes, and anything with unknown
// materiality stay in the folded summary table.
func worthInline(f payload.Finding) bool {
	c := f.FindingContract
	if c == nil || f.FindingContractStatus != "valid" || c.Materiality != "current_impact" {
		return false
	}
	if c.FindingKind != "production_behavior" && c.FindingKind != "security_risk" {
		return false
	}
	return !types.DiscountGateEnabled() || types.SelfDiscount(c) == types.DiscountNone
}
