package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pr-review-server/pkg/reviewer/types"
)

// firstPassClaim is one first-pass comment as handed to the agent, with the
// server-assigned id the agent uses to account for it.
type firstPassClaim struct {
	SourceID    string `json:"source_id"`
	FilePath    string `json:"file_path"`
	LineNumber  int    `json:"line_number"`
	CommentBody string `json:"comment_body"`
	Importance  string `json:"importance,omitempty"`
}

func firstPassClaims(comments []types.LineComment) []firstPassClaim {
	claims := make([]firstPassClaim, 0, len(comments))
	for _, c := range comments {
		// The first pass's own SUMMARY is narrative, not a claim to account for.
		if c.FilePath == "SUMMARY" || strings.TrimSpace(c.FilePath) == "" {
			continue
		}
		claims = append(claims, firstPassClaim{
			SourceID: fmt.Sprintf("FP-%d", len(claims)+1), FilePath: c.FilePath, LineNumber: c.LineNumber,
			CommentBody: c.CommentBody, Importance: c.Importance,
		})
	}
	return claims
}

// NormalizeAgentLifecycleFields clears the lifecycle fields the agent has no
// business setting on its own findings. Only ApplyDispositions may create an
// inactive, rejected, merged or disputed record; the agent expresses itself
// through findings, sources and disposition entries.
func NormalizeAgentLifecycleFields(out []types.LineComment) {
	isFinding := func(c types.LineComment) bool {
		return c.Disposition == nil && c.FilePath != "SUMMARY" && c.FilePath != checkFilePath
	}
	seen := map[string]int{}
	for _, c := range out {
		if c.ID != "" && isFinding(c) {
			seen[c.ID]++
		}
	}
	for i := range out {
		c := &out[i]
		c.State, c.Inactive, c.MergedInto, c.MergeBasis = "", false, "", ""
		c.Assessment, c.Original = nil, nil
		// Only ordinary findings carry ids; an id used twice among them
		// identifies nothing. Unlabelled entries still merge by location, so
		// nothing is lost, only the ambiguous reference.
		if !isFinding(*c) || seen[c.ID] > 1 {
			c.ID = ""
		}
	}
}

var summaryVerdictText = map[string]string{
	"approve":             "approve",
	"approve_suggestions": "approve with suggestions",
	"request_changes":     "request changes",
}

// RenderStructuredSummaries writes the SUMMARY prose from the agent's
// structured summary so every review reads the same way: the verdict line
// first, the upshot, the prioritized findings, then the notes. A SUMMARY
// that arrived as prose is left alone.
func RenderStructuredSummaries(comments []types.LineComment) {
	byID := map[string]types.LineComment{}
	for _, c := range comments {
		if c.ID != "" && c.FilePath != "SUMMARY" {
			byID[c.ID] = c
		}
	}
	for i := range comments {
		c := &comments[i]
		if c.FilePath != "SUMMARY" || c.Summary == nil {
			continue
		}
		var b strings.Builder
		verdict, ok := summaryVerdictText[strings.ToLower(strings.TrimSpace(c.Summary.Verdict))]
		if !ok {
			verdict = "unavailable"
		}
		fmt.Fprintf(&b, "Verdict: %s.", verdict)
		if u := strings.TrimSpace(c.Summary.Upshot); u != "" {
			b.WriteString("\n\n" + u)
		}
		n := 0
		for _, id := range c.Summary.PriorityIDs {
			f, ok := byID[id]
			if !ok {
				continue
			}
			if n == 0 {
				b.WriteString("\n\nNext actions:")
			}
			n++
			fmt.Fprintf(&b, "\n%d. %s (%s)", n, findingLabel(f), findingLocation(f))
		}
		if notes := strings.TrimSpace(c.Summary.Notes); notes != "" {
			b.WriteString("\n\n" + notes)
		}
		c.CommentBody = b.String()
	}
}

func findingLabel(f types.LineComment) string {
	if f.FindingContract != nil && strings.TrimSpace(f.FindingContract.Headline) != "" {
		return strings.TrimSpace(f.FindingContract.Headline)
	}
	body := strings.TrimSpace(f.CommentBody)
	if i := strings.IndexAny(body, ".\n"); i > 0 {
		body = body[:i]
	}
	return body
}

func findingLocation(f types.LineComment) string {
	if f.LineNumber > 0 {
		return fmt.Sprintf("%s:%d", f.FilePath, f.LineNumber)
	}
	return f.FilePath
}

const (
	StateConfirmed  = "confirmed"
	StateUnverified = "unverified"
	StateRejected   = "rejected"
	StateMerged     = "merged"
)

// ApplyDispositions reconciles the agent's output against the first-pass
// claims it was handed. It returns the agent's own findings (disposition
// entries removed), the first-pass claims that stay active under today's
// retention policy (criticals the agent did not confirm, marked unverified
// and, when the agent rejected them, disputed), and the inactive records that
// preserve everything else: claims merged into an agent finding, rejected
// non-critical claims with the agent's reason, and claims never examined.
// A claim the agent does not account for is unverified, never dropped.
func ApplyDispositions(agentOut []types.LineComment, claims []firstPassClaim) (findings, active, records []types.LineComment) {
	return ApplyDispositionsWithEvidence(agentOut, claims, func(string) bool { return true })
}

// ApplyDispositionsWithEvidence is ApplyDispositions with a check that a
// rejection's cited evidence paths exist (in the diff or the worktree).
func ApplyDispositionsWithEvidence(agentOut []types.LineComment, claims []firstPassClaim, pathExists func(string) bool) (findings, active, records []types.LineComment) {
	return applyDispositions(agentOut, claims, func(e types.EvidenceRef) bool {
		return e.Line > 0 && pathExists(strings.TrimSpace(e.File))
	})
}

// ApplyDispositionsWithEvidenceRefs is the form the agent stage uses: the
// resolver sees the whole reference so it can check the line against the file.
func ApplyDispositionsWithEvidenceRefs(agentOut []types.LineComment, claims []firstPassClaim, resolves func(types.EvidenceRef) bool) (findings, active, records []types.LineComment) {
	return applyDispositions(agentOut, claims, resolves)
}

func applyDispositions(agentOut []types.LineComment, claims []firstPassClaim, resolves func(types.EvidenceRef) bool) (findings, active, records []types.LineComment) {
	confirmedBy := map[string]string{}
	rejected := map[string]*types.Disposition{}
	for _, c := range agentOut {
		if c.Disposition != nil {
			// A rejection stands only with a reason and at least one code
			// reference that resolves; unsupported prose falls through to
			// unverified.
			if c.Disposition.State == StateRejected && supportedRejection(c.Disposition, resolves) {
				rejected[c.Disposition.SourceID] = resolvedEvidenceOnly(c.Disposition, resolves)
			}
			continue
		}
		// Only an ordinary finding can cover a claim; a SUMMARY or CHECK entry
		// tagging sources would otherwise retire the claim with nothing behind it.
		if c.FilePath != "SUMMARY" && c.FilePath != checkFilePath {
			for _, src := range c.Sources {
				confirmedBy[src] = mergeTarget(c)
			}
		}
		findings = append(findings, c)
	}

	for _, claim := range claims {
		record := types.LineComment{
			FilePath: claim.FilePath, LineNumber: claim.LineNumber, CommentBody: claim.CommentBody,
			Importance: claim.Importance, Provenance: "first-pass",
			Original: &types.OriginalClaim{SourceID: claim.SourceID, FilePath: claim.FilePath, LineNumber: claim.LineNumber, Importance: claim.Importance, Comment: claim.CommentBody},
		}
		critical := strings.EqualFold(strings.TrimSpace(claim.Importance), "CRITICAL")
		switch {
		case hasKey(confirmedBy, claim.SourceID):
			record.State, record.Inactive, record.MergedInto, record.MergeBasis = StateMerged, true, confirmedBy[claim.SourceID], "sources"
			records = append(records, record)
		case rejected[claim.SourceID] != nil && critical:
			record.State, record.Assessment = StateUnverified, rejected[claim.SourceID]
			active = append(active, record)
		case rejected[claim.SourceID] != nil:
			record.State, record.Inactive, record.Assessment = StateRejected, true, rejected[claim.SourceID]
			records = append(records, record)
		case critical:
			record.State = StateUnverified
			active = append(active, record)
		default:
			record.State, record.Inactive = StateUnverified, true
			records = append(records, record)
		}
	}
	return findings, active, records
}

// evidenceFileExists reports whether a cited path names a regular file: an
// exact diff path, or a file under the worktree. Directories and bare
// basenames do not count; rejection is the one disposition that retires a
// claim, so its evidence has to point at real code.
func evidenceFileExists(diffPaths []string, worktreeDir string) func(string) bool {
	inDiff := make(map[string]bool, len(diffPaths))
	for _, p := range diffPaths {
		inDiff[p] = true
	}
	return func(path string) bool {
		path = strings.TrimSpace(strings.TrimPrefix(path, "./"))
		if path == "" || filepath.IsAbs(path) || strings.Contains(path, "..") {
			return false
		}
		if inDiff[path] {
			return true
		}
		if worktreeDir == "" {
			return false
		}
		info, err := os.Stat(filepath.Join(worktreeDir, path))
		return err == nil && info.Mode().IsRegular()
	}
}

// resolvedEvidenceOnly copies a disposition keeping only the evidence that
// resolves, so unresolved references are never rendered as grounding.
func resolvedEvidenceOnly(d *types.Disposition, resolves func(types.EvidenceRef) bool) *types.Disposition {
	out := *d
	out.Evidence = nil
	for _, e := range d.Evidence {
		if strings.TrimSpace(e.File) != "" && resolves(e) {
			out.Evidence = append(out.Evidence, e)
		}
	}
	return &out
}

func supportedRejection(d *types.Disposition, resolves func(types.EvidenceRef) bool) bool {
	if strings.TrimSpace(d.Reason) == "" {
		return false
	}
	for _, e := range d.Evidence {
		if strings.TrimSpace(e.File) != "" && resolves(e) {
			return true
		}
	}
	return false
}

// evidenceRefResolves reports whether a cited file:line names a regular file
// (an exact diff path or a worktree file) and a line the file actually has.
// Rejection is the one disposition that retires a claim, so its evidence has
// to point at real code.
func evidenceRefResolves(diffPaths []string, worktreeDir string) func(types.EvidenceRef) bool {
	fileExists := evidenceFileExists(diffPaths, worktreeDir)
	return func(e types.EvidenceRef) bool {
		path := strings.TrimSpace(strings.TrimPrefix(e.File, "./"))
		if e.Line <= 0 || !fileExists(path) {
			return false
		}
		if worktreeDir == "" {
			return true
		}
		data, err := os.ReadFile(filepath.Join(worktreeDir, path))
		if err != nil {
			return false // in the diff but gone from disk (deleted): nothing to cite
		}
		lines := strings.Count(string(data), "\n")
		if len(data) > 0 && data[len(data)-1] != '\n' {
			lines++
		}
		return e.Line <= lines
	}
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}
