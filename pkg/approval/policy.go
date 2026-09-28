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
	blocked := false
	if s.HumanChangesRequested {
		blocked = true
		add("human_changes_requested")
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
	for _, claim := range claims {
		lower := strings.ToLower(claim)
		for _, phrase := range []string{"i ran ", "we ran ", "i executed ", "we executed ", "executed tests", "ran the tests"} {
			if strings.Contains(lower, phrase) {
				return fmt.Errorf("unsupported test execution claim")
			}
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
		if c.Disposition == "non_blocking" && c.Impact != "style" && c.Impact != "documentation" {
			return fmt.Errorf("unsupported non-blocking impact")
		}
		current, old := false, false
		currentExcerpts, oldExcerpts := map[string]bool{}, map[string]bool{}
		for _, cite := range c.Citations {
			if err := validateCitation(evidence, cite); err != nil {
				return err
			}
			if cite.Path != "" && cite.Revision == s.Revision.Head {
				currentExcerpts[cite.Excerpt] = true
			}
			if cite.Path != "" && cite.Revision == c.OriginalRevision {
				oldExcerpts[cite.Excerpt] = true
			}
			current = current || cite.Path != "" && cite.Revision == s.Revision.Head
			old = old || cite.Path != "" && cite.Revision == c.OriginalRevision && c.OriginalRevision != s.Revision.Head
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
		if c.Disposition == "non_blocking" && len(c.Citations) == 0 {
			return fmt.Errorf("non-blocking lacks support")
		}
	}
	for _, c := range s.Concerns {
		ac, ok := concerns[c.ID]
		if !ok {
			return fmt.Errorf("omitted concern")
		}
		if ac.Claim != c.Claim || ac.OriginalSeverity != c.OriginalSeverity || ac.OriginalRevision != c.OriginalRevision {
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
