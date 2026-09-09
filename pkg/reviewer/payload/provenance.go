// Provenance derivation and the no-swallow guarantee for deterministic
// alerts.
//
// The merge layer (service.MergeFindings) knows which source produced each
// finding, but historically recorded it only as a prose marker prepended to
// the comment body. The helpers here recover that provenance as a structured
// value: the explicit types.LineComment.Provenance field wins when a producer
// set it, and the prose marker is parsed as the fallback so sidecars built
// from pre-field merge output still carry correct attribution.
package payload

import (
	"fmt"
	"regexp"
	"strings"

	"pr-review-server/pkg/reviewer/types"
)

// Provenance values carried on findings. These mirror the FindingSet
// provenance labels the poller passes to service.MergeFindings so consumers
// can branch on a stable enum instead of scraping prose.
const (
	ProvenanceAgent         = "agent"          // canonical review-agent voice (first merge set)
	ProvenanceFirstPass     = "first-pass"     // re-admitted first-pass critical
	ProvenanceMechanical    = "mechanical"     // deterministic gate alert
	ProvenanceRequiredCheck = "required-check" // required-check escalation/synthesis
	ProvenanceCarried       = "carried"        // carried forward from a prior push's review
)

// provenanceNoteRe matches the marker service.MergeFindings prepends to
// findings re-admitted from lower-priority sets: "_[<provenance> finding —
// retained by reconciliation, ...]_". The phrasing is owned by
// service/merge.go (provenanceNote); this package parses it rather than
// importing the service package to stay dependency-light (and because the
// service package renders through html, which shares these helpers).
var provenanceNoteRe = regexp.MustCompile(`^_\[([^\[\]]+) finding — retained by reconciliation`)

// DeriveProvenance returns the finding's provenance: the explicit
// LineComment.Provenance field when the producer set one, else the label
// parsed from the merge-layer provenance note, else ProvenanceAgent — a
// finding with no marker is the canonical (first-set) voice.
func DeriveProvenance(c types.LineComment) string {
	if p := strings.TrimSpace(c.Provenance); p != "" {
		return normalizeProvenance(p)
	}
	return DeriveProvenanceFromBody(c.CommentBody)
}

// DeriveProvenanceFromBody derives provenance from comment text alone, for
// consumers that no longer hold the structured LineComment (e.g. re-reading
// a rendered finding).
func DeriveProvenanceFromBody(body string) string {
	if m := provenanceNoteRe.FindStringSubmatch(body); m != nil {
		return normalizeProvenance(m[1])
	}
	return ProvenanceAgent
}

// StripProvenanceNote removes one leading legacy provenance preface (and the
// whitespace after it) so the marker never renders as finding text.
func StripProvenanceNote(body string) string {
	if !provenanceNoteRe.MatchString(body) {
		return body
	}
	end := strings.Index(body, "]_")
	if end < 0 {
		return body
	}
	return strings.TrimLeft(body[end+2:], " \t\r\n")
}

// normalizeProvenance folds label variants onto the stable enum. Unknown
// labels pass through verbatim — truthful attribution beats forcing a value.
func normalizeProvenance(label string) string {
	switch {
	case label == "earlier pass":
		// merge.go's fallback wording when a set carried no provenance label.
		return ProvenanceFirstPass
	case label == "prior-review",
		strings.HasPrefix(label, ProvenanceCarried),
		strings.HasPrefix(label, "prior-review"):
		// Carry-forward sets label per-review ("carried from review of
		// <sha>"); collapse every variant onto the enum value.
		return ProvenanceCarried
	}
	return label
}

// IsDeterministicProvenance reports whether p identifies a zero-LLM finding
// source (a mechanical gate or the required-check enforcement layer) — the
// findings the no-swallow guarantee and the pinned report section cover.
func IsDeterministicProvenance(p string) bool {
	return p == ProvenanceMechanical || p == ProvenanceRequiredCheck
}

// gateAlertKinds maps a stable phrase from each mechanical gate's alert body
// to that gate type's slug — the same slugs required-check ids embed
// (CHK-<slug>-N). The phrases are owned by service/gates.go and this table
// mirrors service/checks.go gateCheckKinds; keep the three in sync when
// adding a gate.
var gateAlertKinds = []struct{ marker, slug string }{
	{"portal overlay without an explicit layer", "portal-layer"},
	{"undefined settings reference", "settings-ref"},
	{"shared module edited", "shared-file"},
	{"new model property", "model-property"},
	{"backend topic without frontend counterpart", "registry-split"},
	{"new symbol with no references", "wiring"},
	{"symbol de-wired", "wiring"},
	{"resource acquired without unmount cleanup", "effect-cleanup"},
	{"task payload contract changed", "task-skew"},
	{"migration residue", "migration-residue"},
}

// AlertKind returns the gate-kind slug for a deterministic alert body, or ""
// when the body matches no known gate phrasing (e.g. bug-memory alerts).
func AlertKind(body string) string {
	for _, k := range gateAlertKinds {
		if strings.Contains(body, k.marker) {
			return k.slug
		}
	}
	return ""
}

// AlertID renders a stable, log-greppable identifier for a fired
// deterministic alert: "<kind>:<file>:<line>", with kind "alert" when the
// body matches no known gate phrasing.
func AlertID(c types.LineComment) string {
	kind := AlertKind(c.CommentBody)
	if kind == "" {
		kind = "alert"
	}
	return fmt.Sprintf("%s:%s:%d", kind, c.FilePath, c.LineNumber)
}

// MissingAlerts returns the fired deterministic alerts that are NOT
// represented in findings. An alert is represented when either
//   - its body text survives verbatim inside some finding (unique alerts keep
//     their body through the merge, gaining only a provenance-note prefix), or
//   - a deterministic-provenance finding targets the alert's file (the merge
//     intentionally lets a required-check VIOLATED synthesis on the same file
//     absorb the generic gate advisory — the defect is still surfaced there).
//
// Anything else means the alert was swallowed — typically duplicate-collapse
// into a nearby agent finding whose phrasing won — and the caller must report
// it, never drop it silently.
func MissingAlerts(findings []Finding, alerts []types.LineComment) []types.LineComment {
	var missing []types.LineComment
	for _, a := range alerts {
		if !alertRepresented(findings, a) {
			missing = append(missing, a)
		}
	}
	return missing
}

func alertRepresented(findings []Finding, a types.LineComment) bool {
	for _, f := range findings {
		if a.CommentBody != "" && strings.Contains(f.Comment, a.CommentBody) {
			return true
		}
		if IsDeterministicProvenance(f.Provenance) && sameAlertFile(f.File, a.FilePath) {
			return true
		}
	}
	return false
}

// sameAlertFile matches paths exactly or by whole-path suffix (sources differ
// in how much leading path they emit), mirroring service/merge.go
// sameFileStrict. Never by bare basename: with no line signal to corroborate,
// two directories' models.py are different files.
func sameAlertFile(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	return a == b || strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
}
