package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var fullSHA = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
var testExecutionClaim = regexp.MustCompile(`(?i)\b(?:tests?|test\s+suite)\s+(?:(?:all|have|has|were|was|are|is|successfully)\s+)*(?:pass(?:ed|es|ing)?|succeed(?:ed|s)?|executed|run|green)\b|\b(?:ran|executed|running)\s+(?:the\s+)?(?:tests?|pytest|unittest|go\s+test|npm\s+test)\b`)

func SnapshotDigest(s Snapshot) string {
	s.ID = ""
	s.Digest = ""
	s.CapturedAt = time.Time{}
	s.Evidence = append([]Evidence(nil), s.Evidence...)
	for i := range s.Evidence {
		s.Evidence[i].CreatedAt = time.Time{}
		s.Evidence[i].UpdatedAt = time.Time{}
	}
	sort.Slice(s.Evidence, func(i, j int) bool { return s.Evidence[i].ID < s.Evidence[j].ID })
	s.Sources = append([]Source(nil), s.Sources...)
	sort.Slice(s.Sources, func(i, j int) bool { return s.Sources[i].ID < s.Sources[j].ID })
	s.Checks = append([]Check(nil), s.Checks...)
	sort.Slice(s.Checks, func(i, j int) bool { return s.Checks[i].Name < s.Checks[j].Name })
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func Evaluate(s Snapshot, a Assessment) Assessment {
	a.Sources = s.Sources
	a.PolicyVersion = PolicyVersion
	a.ReasonCodes = nil
	add := func(reason string) {
		for _, r := range a.ReasonCodes {
			if r == reason {
				return
			}
		}
		a.ReasonCodes = append(a.ReasonCodes, reason)
	}
	if !s.Eligible {
		a.Decision = "excluded"
		a.ReasonCodes = append(a.ReasonCodes, s.ExclusionReasons...)
		return a
	}
	if s.Draft {
		add("pr_draft")
	}
	blocked := false
	if s.HumanChangesRequested {
		blocked = true
		add("human_changes_requested")
	}
	if s.ProviderChangesRequested {
		blocked = true
		add("provider_changes_requested")
	}
	success := false
	if len(s.Checks) == 0 {
		add("ci_unknown")
	}
	for _, check := range s.Checks {
		if check.SHA != s.Revision.Head {
			add("ci_unknown")
			continue
		}
		switch check.State {
		case "success":
			success = true
		case "neutral", "skipped":
		case "pending", "queued", "in_progress":
			add("ci_pending")
		default:
			blocked = true
			add("ci_failed")
		}
	}
	if !success {
		add("ci_unknown")
	}
	complete := s.Manifest.Complete && len(s.Manifest.Endpoints) > 0 && len(s.Manifest.Errors) == 0
	for _, e := range s.Manifest.Endpoints {
		complete = complete && e.Complete && !e.Truncated && len(e.Errors) == 0
	}
	if !complete {
		add("source_incomplete")
	}
	coverage := false
	for _, source := range s.Sources {
		if source.Incomplete {
			add("source_incomplete")
		}
		if source.ReviewedSHA == s.Revision.Head && (source.Completion == "running" || source.Completion == "queued" || source.Completion == "pending") {
			add("review_in_progress")
		}
		if source.Verified && source.ReviewedSHA == s.Revision.Head && source.Completion == "completed" && !source.Incomplete && source.FileCoverage != "reported_partial" && (source.Provider == "prism" || source.Provider == "greptile" || source.Provider == "copilot") {
			coverage = true
		}
	}
	if !coverage {
		add("review_missing")
	}
	if s.ReviewInProgress {
		add("review_in_progress")
	}
	if !fullSHA.MatchString(s.Revision.Head) || !fullSHA.MatchString(s.Revision.Base) || !fullSHA.MatchString(s.Revision.MergeBase) {
		add("source_incomplete")
	}
	if s.Target.ExpectedHeadSHA != s.Revision.Head {
		add("head_changed")
	}
	for _, c := range a.Concerns {
		switch c.Disposition {
		case "unresolved":
			blocked = true
			add("open_concern")
		case "uncertain":
			add("uncertain_concern")
		}
	}
	if len(a.CoverageGaps) > 0 {
		add("source_incomplete")
	}
	if err := ValidateAssessment(s, a); err != nil {
		add("invalid_assessment")
	}
	switch {
	case blocked:
		a.Decision = "needs_attention"
	case len(a.ReasonCodes) > 0:
		a.Decision = "insufficient_evidence"
	default:
		a.Decision = "candidate"
	}
	return a
}

func ValidateAssessment(s Snapshot, a Assessment) error {
	if a.SnapshotID != s.ID || a.SnapshotDigest != s.Digest || s.Digest == "" || s.Digest != SnapshotDigest(s) {
		return fmt.Errorf("snapshot mismatch")
	}
	claims := []string{a.Summary}
	for _, concern := range a.Concerns {
		claims = append(claims, concern.Rationale)
	}
	claims = append(claims, a.CoverageGaps...)
	for _, artifact := range a.Artifacts {
		claims = append(claims, artifact.Rationale)
	}
	for _, claim := range claims {
		if claimsTestExecution(claim) {
			return fmt.Errorf("unsupported test execution claim")
		}
	}
	if strings.TrimSpace(a.Summary) == "" {
		return fmt.Errorf("missing summary")
	}
	evidence := map[string]Evidence{}
	for _, e := range s.Evidence {
		if e.ID == "" {
			return fmt.Errorf("empty evidence ID")
		}
		if _, ok := evidence[e.ID]; ok {
			return fmt.Errorf("duplicate evidence")
		}
		evidence[e.ID] = e
	}
	concerns := map[string]Concern{}
	canonical := map[string]Concern{}
	for _, c := range s.Concerns {
		canonical[c.ID] = c
	}
	for _, c := range a.Concerns {
		if c.ID == "" || c.Claim == "" || c.Rationale == "" || len(c.EvidenceIDs) == 0 {
			return fmt.Errorf("incomplete concern")
		}
		if _, ok := concerns[c.ID]; ok {
			return fmt.Errorf("duplicate concern")
		}
		concerns[c.ID] = c
		for _, id := range c.EvidenceIDs {
			if _, ok := evidence[id]; !ok {
				return fmt.Errorf("unknown evidence")
			}
		}
		original, known := canonical[c.ID]
		if !known && (c.Disposition == "fixed" || c.Disposition == "not_applicable" || c.Disposition == "non_blocking") {
			if len(c.EvidenceIDs) != 1 {
				return fmt.Errorf("discovered concern needs one authoritative source")
			}
			source := evidence[c.EvidenceIDs[0]]
			if len(strings.TrimSpace(c.Claim)) < 16 || !strings.Contains(source.Body, c.Claim) {
				return fmt.Errorf("discovered concern lacks source claim")
			}
			original = Concern{Path: source.Path, StartLine: source.StartLine, EndLine: source.EndLine, OriginalRevision: source.ReviewedSHA, Impact: "unknown"}
			if c.OriginalRevision != original.OriginalRevision {
				return fmt.Errorf("discovered concern revision changed")
			}
		}
		if c.Disposition == "fixed" || c.Disposition == "not_applicable" {
			if original.Path == "" || !safePath(original.Path) || original.StartLine < 1 || c.Path != original.Path || c.StartLine != original.StartLine || c.EndLine != original.EndLine {
				return fmt.Errorf("concern lacks immutable source anchor")
			}
		}
		if c.Disposition == "fixed" {
			attributed := false
			for _, id := range c.EvidenceIDs {
				attributed = attributed || evidence[id].ReviewedSHA == c.OriginalRevision && fullSHA.MatchString(c.OriginalRevision)
			}
			if !attributed {
				return fmt.Errorf("fix original revision lacks source attribution")
			}
		}
		switch c.Disposition {
		case "fixed", "not_applicable", "non_blocking", "unresolved", "uncertain":
		default:
			return fmt.Errorf("unknown disposition")
		}
		if c.Disposition == "non_blocking" && ((original.Impact != "style" && original.Impact != "documentation") || c.Impact != original.Impact) {
			return fmt.Errorf("unsupported non-blocking source impact")
		}
		if c.Disposition == "non_blocking" && (strings.EqualFold(original.OriginalSeverity, "critical") || strings.EqualFold(original.OriginalSeverity, "high")) {
			return fmt.Errorf("blocking source severity cannot be downgraded")
		}
		current, old := false, false
		currentExcerpts, oldExcerpts := map[string]bool{}, map[string]bool{}
		for _, cite := range c.Citations {
			if err := validateCitation(evidence, cite); err != nil {
				return err
			}
			anchored := cite.Path == original.Path && sourceExcerpt(cite.Excerpt)
			if anchored && cite.Revision == s.Revision.Head {
				currentExcerpts[cite.Excerpt] = true
			}
			if anchored && cite.Revision == c.OriginalRevision && anchorOverlap(original, cite) {
				oldExcerpts[cite.Excerpt] = true
			}
			current = current || anchored && cite.Revision == s.Revision.Head && (c.OriginalRevision != s.Revision.Head || anchorOverlap(original, cite))
			old = old || anchored && cite.Revision == c.OriginalRevision && c.OriginalRevision != s.Revision.Head && anchorOverlap(original, cite)
		}
		different := false
		for excerpt := range currentExcerpts {
			if !oldExcerpts[excerpt] {
				different = true
			}
		}
		if c.Disposition == "fixed" && (!current || !old || !different || !fullSHA.MatchString(c.OriginalRevision)) {
			return fmt.Errorf("fix lacks original and current code")
		}
		if c.Disposition == "not_applicable" && !current {
			return fmt.Errorf("inapplicable lacks current code")
		}
		if c.Disposition == "non_blocking" {
			supported := false
			for _, cite := range c.Citations {
				for _, id := range original.EvidenceIDs {
					supported = supported || cite.EvidenceID == id && sourceExcerpt(cite.Excerpt)
				}
			}
			if !supported {
				return fmt.Errorf("non-blocking lacks source support")
			}
		}
	}
	for _, c := range s.Concerns {
		ac, ok := concerns[c.ID]
		if !ok {
			return fmt.Errorf("omitted concern")
		}
		if ac.Claim != c.Claim || ac.OriginalSeverity != c.OriginalSeverity || ac.OriginalRevision != c.OriginalRevision || ac.Path != c.Path || ac.StartLine != c.StartLine || ac.EndLine != c.EndLine {
			return fmt.Errorf("altered concern provenance")
		}
		for _, id := range c.EvidenceIDs {
			found := false
			for _, aid := range ac.EvidenceIDs {
				found = found || id == aid
			}
			if !found {
				return fmt.Errorf("omitted concern provenance")
			}
		}
	}
	artifacts := map[string]bool{}
	for _, art := range a.Artifacts {
		if _, ok := evidence[art.EvidenceID]; !ok || artifacts[art.EvidenceID] || art.Rationale == "" {
			return fmt.Errorf("invalid artifact disposition")
		}
		artifacts[art.EvidenceID] = true
		if art.Classification != "concerns" && art.Classification != "non_actionable" {
			return fmt.Errorf("unknown artifact classification")
		}
		if art.Classification == "concerns" && len(art.ConcernIDs) == 0 {
			return fmt.Errorf("missing artifact concerns")
		}
		for _, id := range append(append([]string(nil), art.ConcernIDs...), evidence[art.EvidenceID].ConcernIDs...) {
			c, ok := concerns[id]
			if !ok {
				return fmt.Errorf("unknown artifact concern")
			}
			linked := false
			for _, e := range c.EvidenceIDs {
				linked = linked || e == art.EvidenceID
			}
			if !linked {
				return fmt.Errorf("unlinked concern")
			}
		}
	}
	if len(artifacts) != len(evidence) {
		return fmt.Errorf("unclassified artifacts")
	}
	for _, c := range a.Citations {
		if err := validateCitation(evidence, c); err != nil {
			return err
		}
	}
	return nil
}

func sourceExcerpt(text string) bool { return len(strings.TrimSpace(text)) >= 16 }

func anchorOverlap(concern Concern, cite Citation) bool {
	end := concern.EndLine
	if end < concern.StartLine {
		end = concern.StartLine
	}
	return concern.StartLine > 0 && cite.StartLine <= end && cite.EndLine >= concern.StartLine
}

func validateCitation(evidence map[string]Evidence, c Citation) error {
	if !c.Validated || c.Excerpt == "" {
		return fmt.Errorf("unvalidated citation")
	}
	if c.EvidenceID != "" {
		e, ok := evidence[c.EvidenceID]
		if !ok || !strings.Contains(e.Body, c.Excerpt) || c.Path != "" {
			return fmt.Errorf("invalid evidence citation")
		}
		return nil
	}
	if !fullSHA.MatchString(c.Revision) || !safePath(c.Path) || c.Path == "" || c.StartLine < 1 || c.EndLine < c.StartLine || c.EndLine-c.StartLine >= 400 {
		return fmt.Errorf("invalid code citation")
	}
	return nil
}

// downgradeUnsupportedDiscoveries applies the authoritative-source rule for
// concerns the model discovered itself before validation: a favorable
// disposition that does not rest on exactly one artifact containing its claim
// becomes uncertain. Its evidence links stay, since artifacts reference them.
func downgradeUnsupportedDiscoveries(s Snapshot, a *Assessment) {
	known := map[string]bool{}
	for _, c := range s.Concerns {
		known[c.ID] = true
	}
	bodies := map[string]string{}
	for _, e := range s.Evidence {
		bodies[e.ID] = e.Body
	}
	for i := range a.Concerns {
		c := &a.Concerns[i]
		if known[c.ID] || (c.Disposition != "fixed" && c.Disposition != "not_applicable" && c.Disposition != "non_blocking") {
			continue
		}
		if len(c.EvidenceIDs) == 1 && len(strings.TrimSpace(c.Claim)) >= 16 && strings.Contains(bodies[c.EvidenceIDs[0]], c.Claim) {
			continue
		}
		c.Rationale += " [This discovered concern does not rest on one artifact containing its claim, so the " + c.Disposition + " disposition is marked uncertain.]"
		c.Disposition = "uncertain"
		a.CoverageGaps = append(a.CoverageGaps, "Concern "+c.ID+": no single authoritative source for a discovered concern")
	}
}

// repairByDowngrade validates a and, when that fails, retries with favorable
// concern dispositions downgraded to uncertain: first one concern at a time,
// then all of them. Downgrading only makes the decision less favorable, so the
// server can settle a policy failure without another model round.
func repairByDowngrade(s Snapshot, a *Assessment) error {
	err := ValidateAssessment(s, *a)
	if err == nil {
		return nil
	}
	favorable := func(d string) bool { return d == "fixed" || d == "not_applicable" || d == "non_blocking" }
	downgraded := func(indexes ...int) Assessment {
		copy := *a
		copy.Concerns = append([]Concern(nil), a.Concerns...)
		copy.CoverageGaps = append([]string(nil), a.CoverageGaps...)
		for _, i := range indexes {
			c := &copy.Concerns[i]
			c.Rationale += " [This " + c.Disposition + " disposition did not pass policy validation (" + err.Error() + "), so it is marked uncertain.]"
			copy.CoverageGaps = append(copy.CoverageGaps, "Concern "+c.ID+": "+err.Error())
			c.Disposition = "uncertain"
		}
		return copy
	}
	var all []int
	for i, c := range a.Concerns {
		if !favorable(c.Disposition) {
			continue
		}
		all = append(all, i)
		if candidate := downgraded(i); ValidateAssessment(s, candidate) == nil {
			*a = candidate
			return nil
		}
	}
	if len(all) > 0 {
		if candidate := downgraded(all...); ValidateAssessment(s, candidate) == nil {
			*a = candidate
			return nil
		}
	}
	return err
}

// restoreCanonicalConcerns rewrites the provenance of concerns the collector
// extracted (claim, severity, revision, anchors, evidence) from the snapshot,
// keeping only the model's disposition and rationale, and adds any canonical
// concern the model omitted as unresolved. The model judges concerns; it does
// not get to restate where they came from.
func restoreCanonicalConcerns(s Snapshot, a *Assessment) {
	index := map[string]int{}
	for i, c := range a.Concerns {
		index[c.ID] = i
	}
	for _, canonical := range s.Concerns {
		i, ok := index[canonical.ID]
		if !ok {
			missing := canonical
			missing.Disposition = "unresolved"
			missing.Rationale = "Not assessed by the investigator."
			missing.Citations = nil
			a.Concerns = append(a.Concerns, missing)
			a.CoverageGaps = append(a.CoverageGaps, "Concern "+canonical.ID+" was not assessed and is treated as unresolved")
			continue
		}
		c := &a.Concerns[i]
		c.Claim, c.OriginalSeverity, c.OriginalRevision = canonical.Claim, canonical.OriginalSeverity, canonical.OriginalRevision
		c.Path, c.StartLine, c.EndLine = canonical.Path, canonical.StartLine, canonical.EndLine
		seen := map[string]bool{}
		for _, id := range c.EvidenceIDs {
			seen[id] = true
		}
		for _, id := range canonical.EvidenceIDs {
			if !seen[id] {
				c.EvidenceIDs = append(c.EvidenceIDs, id)
			}
		}
	}
}

// normalizeArtifacts maps obvious classification spellings to the two the
// policy accepts and links each concern an artifact names (or the collector
// attached to it) back to that artifact; neither changes what the model judged.
func normalizeArtifacts(s Snapshot, a *Assessment) {
	concerns := map[string]*Concern{}
	for i := range a.Concerns {
		concerns[a.Concerns[i].ID] = &a.Concerns[i]
	}
	collected := map[string][]string{}
	for _, e := range s.Evidence {
		collected[e.ID] = e.ConcernIDs
	}
	for i := range a.Artifacts {
		art := &a.Artifacts[i]
		switch strings.ToLower(strings.TrimSpace(strings.ReplaceAll(art.Classification, "-", "_"))) {
		case "concern", "concerns", "actionable":
			art.Classification = "concerns"
		case "non_actionable", "nonactionable", "not_actionable", "none":
			art.Classification = "non_actionable"
		}
		for _, id := range append(append([]string(nil), art.ConcernIDs...), collected[art.EvidenceID]...) {
			c, ok := concerns[id]
			if !ok {
				continue
			}
			linked := false
			for _, e := range c.EvidenceIDs {
				linked = linked || e == art.EvidenceID
			}
			if !linked {
				c.EvidenceIDs = append(c.EvidenceIDs, art.EvidenceID)
			}
		}
	}
}

var testExecutionPhrases = []string{"i ran ", "we ran ", "i executed ", "we executed ", "executed tests", "ran the tests", "tests pass", "tests passed", "test passed", "test passes", "tests succeed", "tests succeeded", "test suite pass", "tests were run", "tests were executed", "tests have passed", "tested successfully"}

// claimsTestExecution reports whether text asserts that tests were run or
// passed; the investigator cannot run tests and CI states speak for themselves.
func claimsTestExecution(text string) bool {
	if testExecutionClaim.MatchString(text) {
		return true
	}
	lower := strings.ToLower(text)
	for _, phrase := range testExecutionPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// stripTestExecutionClaims removes the sentences of the assessment's prose
// that assert test execution, which the policy rejects; dropping a claim never
// makes the decision more favorable, and CI results remain in the snapshot.
func stripTestExecutionClaims(a *Assessment) {
	strip := func(text string) string {
		if !claimsTestExecution(text) {
			return text
		}
		var kept []string
		for _, sentence := range splitSentences(text) {
			if !claimsTestExecution(sentence) {
				kept = append(kept, sentence)
			}
		}
		return strings.TrimSpace(strings.Join(kept, " "))
	}
	a.Summary = strip(a.Summary)
	for i := range a.Concerns {
		a.Concerns[i].Rationale = strip(a.Concerns[i].Rationale)
	}
	gaps := a.CoverageGaps[:0]
	for _, gap := range a.CoverageGaps {
		if gap = strip(gap); gap != "" {
			gaps = append(gaps, gap)
		}
	}
	a.CoverageGaps = gaps
	for i := range a.Artifacts {
		if a.Artifacts[i].Rationale = strip(a.Artifacts[i].Rationale); a.Artifacts[i].Rationale == "" {
			a.Artifacts[i].Rationale = "Classified from the artifact text."
		}
	}
}

func splitSentences(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text); i++ {
		if c := text[i]; (c == '.' || c == '!' || c == '?' || c == '\n') && (i+1 == len(text) || text[i+1] == ' ' || text[i+1] == '\n') {
			if sentence := strings.TrimSpace(text[start : i+1]); sentence != "" {
				out = append(out, sentence)
			}
			start = i + 1
		}
	}
	if rest := strings.TrimSpace(text[start:]); rest != "" {
		out = append(out, rest)
	}
	return out
}
