package service

import (
	"path"
	"strconv"
	"strings"

	"pr-review-server/pkg/reviewer/types"
)

// FindingSet pairs a provenance label with one source's review comments.
// Provenance examples: "agent", "gemini", "premortem", "mechanical".
type FindingSet struct {
	Provenance string
	Comments   []types.LineComment
}

// mergeLineTolerance: two findings on the same file within this many lines are
// treated as the same underlying issue.
const mergeLineTolerance = 10

// importanceRank orders severities for the max-upgrade rule. Unknown/empty
// ranks lowest so a labeled duplicate always wins.
func importanceRank(imp string) int {
	switch strings.ToUpper(strings.TrimSpace(imp)) {
	case "CRITICAL":
		return 3
	case "MEDIUM":
		return 2
	case "LOW":
		return 1
	default:
		return 0
	}
}

// MergeFindings unions review findings from multiple sources into one list.
//
// Rationale: the agent stage's output used to fully REPLACE the first-pass
// (Gemini) findings, and our release-blocker eval showed the agent deleting
// correct first-pass catches it had argued itself out of. Merging is
// deterministic — a dropped finding survives no matter how persuasive the
// agent's dismissal prose was.
//
// Rules:
//   - Sets are in priority order: the first set is the canonical voice (its
//     phrasing wins on duplicates, and only its SUMMARY entries are kept).
//   - Two non-SUMMARY findings are duplicates when they target the same file
//     (matched on full path or basename, since sources differ in how much of
//     the path they emit) within mergeLineTolerance lines. Line 0 (whole-file)
//     only matches line 0, and only on a strict path match — with no line to
//     corroborate, basename equality alone would collapse findings on
//     conventionally named files (models.py, index.ts) across directories.
//   - Duplicates keep the higher-priority phrasing but upgrade Importance to
//     the max across the pair — a later source never *lowers* severity.
//   - Findings unique to a lower-priority set are appended with their set's
//     provenance in the structured field, so renderers can mark them as not
//     independently confirmed without any prose preface.
//
// The caller decides what belongs in each set (e.g. filtering Gemini style
// nits before the merge); MergeFindings only unions what it is given.
func MergeFindings(sets ...FindingSet) []types.LineComment {
	merged, _ := MergeFindingsWithRecords(sets...)
	return merged
}

// MergeFindingsWithRecords is MergeFindings that also returns each dropped
// duplicate as an inactive merged record pointing at the finding that kept
// the line, so a first-pass claim folded into an agent finding is preserved.
func MergeFindingsWithRecords(sets ...FindingSet) (merged, records []types.LineComment) {
	for si, set := range sets {
		for _, c := range set.Comments {
			// The caller builds the sets, so the set label is the authoritative
			// attribution for this review (a carried finding's own stamp is
			// the bare word; the set label names the source review).
			switch {
			case set.Provenance != "":
				c.Provenance = set.Provenance
			case si > 0:
				c.Provenance = "first-pass"
			}
			if c.FilePath == "SUMMARY" {
				if si == 0 {
					merged = append(merged, c)
				}
				continue
			}
			if di, ok := findDuplicate(merged, c); ok {
				// Duplicates upgrade severity to the max — but an upgrade
				// sourced from a lower-priority set is capped at MEDIUM for
				// the same reason re-admissions are (see below): unconfirmed
				// first-pass severity must not create blockers.
				incoming := c.Importance
				if si > 0 && importanceRank(incoming) > importanceRank("MEDIUM") {
					incoming = "MEDIUM"
				}
				if importanceRank(incoming) > importanceRank(merged[di].Importance) {
					merged[di].Importance = incoming
				}
				if si > 0 {
					c.State, c.Inactive, c.Assessment = StateMerged, true, nil
					c.MergedInto = mergeTarget(merged[di])
					records = append(records, c)
				}
				continue
			}
			if si > 0 {
				// Cap re-admitted findings at MEDIUM: measured on a set of
				// known-good merged PRs, the first pass emits CRITICALs on
				// half of them — re-admitting those at full severity would
				// turn clean reviews into false blockers. MEDIUM keeps the
				// finding visible on the exact line (recall is unaffected)
				// without letting an unconfirmed concern gate a merge.
				if importanceRank(c.Importance) > importanceRank("MEDIUM") {
					c.Importance = "MEDIUM"
				}
			}
			merged = append(merged, c)
		}
	}
	return merged, records
}

// mergeTarget names the finding a duplicate folded into: the agent's own id
// when it gave one, else the location.
func mergeTarget(f types.LineComment) string {
	if f.ID != "" {
		return f.ID
	}
	if f.LineNumber > 0 {
		return f.FilePath + ":" + strconv.Itoa(f.LineNumber)
	}
	return f.FilePath
}

// findDuplicate returns the index in merged of a finding duplicating c.
func findDuplicate(merged []types.LineComment, c types.LineComment) (int, bool) {
	for i, m := range merged {
		if m.FilePath == "SUMMARY" || !sameFile(m.FilePath, c.FilePath) {
			continue
		}
		if m.LineNumber == 0 || c.LineNumber == 0 {
			// Whole-file findings carry no line signal to corroborate a
			// basename match, so they dedup only on a strict path match.
			if m.LineNumber == c.LineNumber && sameFileStrict(m.FilePath, c.FilePath) {
				return i, true
			}
			continue
		}
		d := m.LineNumber - c.LineNumber
		if d < 0 {
			d = -d
		}
		if d <= mergeLineTolerance {
			return i, true
		}
	}
	return -1, false
}

// sameFile matches paths exactly, or by suffix/basename — different sources
// emit different amounts of leading path for the same file.
func sameFile(a, b string) bool {
	return sameFileStrict(a, b) || path.Base(a) == path.Base(b)
}

// sameFileStrict matches paths exactly or by whole-path suffix, never by bare
// basename. Use it wherever no line number can corroborate the match: two
// directories' models.py are different files, not duplicates.
func sameFileStrict(a, b string) bool {
	return a == b || strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
}

// provenanceNoteMarker is the shared tail of every reconciliation marker.
// stripLeadingProvenanceNote and CarriedFromSHA parse rendered notes by this
// exact string — change it and re-carried findings from older sidecars keep
// their stale markers (cosmetic, not incorrect).
const provenanceNoteMarker = " finding — retained by reconciliation, not independently confirmed by the review agent]_\n\n"

// provenanceNote renders the marker prepended to re-admitted findings.
func provenanceNote(provenance string) string {
	if provenance == "" {
		provenance = "earlier pass"
	}
	return "_[" + provenance + provenanceNoteMarker
}

// stripLeadingProvenanceNote removes one leading reconciliation marker from a
// finding body, if present. Findings loaded from a prior review's sidecar may
// already carry a marker (they were themselves re-admitted, or carried, last
// time); carrying them forward again must not stack markers.
func stripLeadingProvenanceNote(body string) string {
	if !strings.HasPrefix(body, "_[") {
		return body
	}
	if idx := strings.Index(body, provenanceNoteMarker); idx >= 0 {
		return body[idx+len(provenanceNoteMarker):]
	}
	return body
}

// carriedProvenancePrefix identifies FindingSet provenance labels produced by
// CarriedProvenance. MergeFindings uses it to count carried re-admissions for
// the SUMMARY reconciliation note; CarriedFromSHA uses it to recognize carried
// findings in rendered bodies.
const carriedProvenancePrefix = "carried from review of "

// CarriedProvenance returns the FindingSet provenance label for findings
// carried forward from an earlier review of the same PR. The label embeds the
// (short) SHA the finding came from, so the rendered provenance note
// deterministically names its source review.
func CarriedProvenance(fromSHA string) string {
	return carriedProvenancePrefix + shortSHA(fromSHA)
}

// CarriedFromSHA reports whether a provenance label names a carried-forward
// finding and returns the short SHA of the review it was carried from.
func CarriedFromSHA(provenance string) (string, bool) {
	if !strings.HasPrefix(provenance, carriedProvenancePrefix) {
		return "", false
	}
	sha := provenance[len(carriedProvenancePrefix):]
	if sha == "" || strings.ContainsAny(sha, " \t\n") {
		return "", false
	}
	return sha, true
}

// CarryForwardFindings applies the deterministic staleness filter for
// cross-review carry-forward: a prior review's finding is re-admitted iff the
// file it cites was NOT touched between the previously reviewed SHA and the
// current head. If the new push modified the cited file, the finding is
// assumed addressed and dropped (zero-noise rule: recall deliberately left on
// the table rather than carrying line-drifted findings). SUMMARY entries never
// carry — each review writes its own summary.
//
// touchedFiles is the inter-push changed-file list; matching uses the same
// strict path semantics as whole-file dedup (exact or whole-path-suffix,
// never bare basename).
//
// Returns the surviving findings (with any stale reconciliation marker
// stripped, so the merge's carried note doesn't stack on an old one) and the
// count of candidates dropped by the filter.
func CarryForwardFindings(prior []types.LineComment, touchedFiles []string) (carried []types.LineComment, dropped int) {
	for _, c := range prior {
		// Inactive records (rejected, unexamined, merged) were never claims and
		// must not become one by surviving a push.
		if c.FilePath == "SUMMARY" || strings.TrimSpace(c.FilePath) == "" || c.Inactive {
			continue
		}
		touched := false
		for _, f := range touchedFiles {
			if sameFileStrict(c.FilePath, f) {
				touched = true
				break
			}
		}
		if touched {
			dropped++
			continue
		}
		c.CommentBody = stripLeadingProvenanceNote(c.CommentBody)
		c.FindingContract = nil
		// Re-attribute: whatever pass produced the finding originally, in THIS
		// review it is a carry-forward — the structured field must agree with
		// the rendered note or payload.DeriveProvenance would report the stale
		// prior-run label.
		c.Provenance = "carried"
		c.State = StateUnverified
		c.Assessment, c.MergedInto = nil, ""
		carried = append(carried, c)
	}
	return carried, dropped
}

// shortSHA truncates a commit SHA to 7 characters (mirrors the GCS review
// object naming) for compact provenance labels.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
