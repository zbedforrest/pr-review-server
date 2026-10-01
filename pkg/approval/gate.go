package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

const (
	OriginInvestigator = "investigator"
	OriginGate         = "gate"
	OriginReused       = "reused"
	OriginLimit        = "limit"
)

var gateSummaries = []struct{ reason, summary string }{
	{"ci_failed", "CI is failing on the current head, so no investigation ran."},
	{"human_changes_requested", "A reviewer's change request is still standing, so no investigation ran."},
	{"provider_changes_requested", "A review provider requested changes on the current head, so no investigation ran."},
}

// Gate decides an eligible snapshot without the model when its own facts
// already block it. Concerns can only add blockers, so no investigation could
// change a decision the policy reaches from the snapshot alone.
func Gate(s Snapshot) (Assessment, bool) {
	if !s.Eligible {
		return Assessment{}, false
	}
	base := Assessment{SchemaVersion: "1", PolicyVersion: PolicyVersion, PromptVersion: PromptVersion, RuntimeVersion: RuntimeVersion, SnapshotID: s.ID, SnapshotDigest: s.Digest, Origin: OriginGate, AssessedAt: time.Now().UTC()}
	a := Evaluate(s, base)
	if a.Decision != "needs_attention" {
		return Assessment{}, false
	}
	// The synthesized assessment has no ledger, so its validation failure says nothing.
	a.ReasonCodes = without(a.ReasonCodes, "invalid_assessment")
	var summaries []string
	for _, g := range gateSummaries {
		for _, r := range a.ReasonCodes {
			if r == g.reason {
				summaries = append(summaries, g.summary)
			}
		}
	}
	a.Summary = strings.Join(summaries, " ")
	return a, true
}

// ReuseKey identifies an assessment by everything it depends on: the snapshot
// digest already covers the viewer, revisions, evidence and checks.
func ReuseKey(snapshotDigest, provider, model string) string {
	return reuseKey(snapshotDigest, provider, model, PolicyVersion, PromptVersion, RuntimeVersion)
}

func reuseKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(append(parts, "1"), "\x00")))
	return hex.EncodeToString(sum[:])
}

// Reuse adopts a prior investigator assessment of the identical snapshot,
// revalidated and re-decided by the current policy.
func Reuse(s Snapshot, prior Assessment, priorTargetID string) (Assessment, bool) {
	if !s.Eligible || !s.Manifest.Complete || prior.SnapshotDigest != s.Digest || prior.SnapshotID != s.ID {
		return Assessment{}, false
	}
	if prior.Origin != OriginInvestigator && prior.Origin != OriginReused {
		return Assessment{}, false
	}
	if prior.SchemaVersion != "1" || prior.PolicyVersion != PolicyVersion || prior.PromptVersion != PromptVersion || prior.RuntimeVersion != RuntimeVersion {
		return Assessment{}, false
	}
	a := prior
	a.Origin = OriginReused
	if a.ReusedFrom == "" {
		a.ReusedFrom = priorTargetID
	}
	a.Usage = Usage{}
	if a.Score != nil {
		// A scored assessment carries its own decision; it has no ledger to revalidate.
		return a, true
	}
	if ValidateAssessment(s, a) != nil {
		return Assessment{}, false
	}
	return Evaluate(s, a), true
}

func without(reasons []string, drop string) []string {
	out := reasons[:0:0]
	for _, r := range reasons {
		if r != drop {
			out = append(out, r)
		}
	}
	return out
}
