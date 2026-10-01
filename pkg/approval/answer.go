package approval

import (
	"encoding/json"
	"errors"
	"fmt"
	"pr-review-server/pkg/reviewer/heal"
	"regexp"
	"strconv"
	"strings"
)

// compactAnswer is what the model returns: verdicts and classifications only.
// The server fills provenance, artifact dispositions and completeness.
type compactAnswer struct {
	Summary    string             `json:"summary"`
	Verdicts   []compactVerdict   `json:"verdicts"`
	Discovered []compactDiscovery `json:"discovered"`
	NoConcerns stringList         `json:"no_concerns"`
	Gaps       stringList         `json:"gaps"`
}

type compactVerdict struct {
	Concern     string        `json:"concern"`
	Disposition string        `json:"disposition"`
	Rationale   string        `json:"rationale"`
	Related     stringList    `json:"related"`
	Citations   []compactCite `json:"citations"`
}

type compactDiscovery struct {
	Evidence    string        `json:"evidence"`
	Claim       string        `json:"claim"`
	Disposition string        `json:"disposition"`
	Rationale   string        `json:"rationale"`
	Related     stringList    `json:"related"`
	Citations   []compactCite `json:"citations"`
}

type compactCite struct {
	Evidence string    `json:"evidence"`
	Revision string    `json:"revision"`
	Path     string    `json:"path"`
	Lines    lineRange `json:"lines"`
	Excerpt  string    `json:"excerpt"`
}

// stringList accepts a JSON array of strings or a single string.
type stringList []string

func (l *stringList) UnmarshalJSON(data []byte) error {
	var one string
	if json.Unmarshal(data, &one) == nil {
		*l = stringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*l = many
	return nil
}

// lineRange accepts [start, end], [line], a number or "start-end".
type lineRange []int

func (r *lineRange) UnmarshalJSON(data []byte) error {
	var many []int
	if json.Unmarshal(data, &many) == nil {
		*r = many
		return nil
	}
	var one int
	if json.Unmarshal(data, &one) == nil {
		*r = lineRange{one}
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	*r = nil
	for _, part := range strings.SplitN(text, "-", 2) {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return fmt.Errorf("invalid line range %q", text)
		}
		*r = append(*r, n)
	}
	return nil
}

// decodeCompactAnswer reads the first JSON object of a reply. Unknown fields
// are ignored and a mistyped field is left empty, which only removes what the
// model claimed; it fails only when no JSON object can be decoded.
func decodeCompactAnswer(text string) (compactAnswer, error) {
	var answer compactAnswer
	body := strings.TrimSpace(stripCodeFence(text))
	err := decodeAnswerObject([]byte(body), &answer)
	if err == nil {
		return answer, nil
	}
	if healed, _, healErr := heal.HealObject(text); healErr == nil {
		answer = compactAnswer{}
		if decodeAnswerObject(healed, &answer) == nil {
			return answer, nil
		}
	}
	return compactAnswer{}, err
}

func decodeAnswerObject(data []byte, answer *compactAnswer) error {
	if !strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		return fmt.Errorf("no JSON object")
	}
	err := json.Unmarshal(data, answer)
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return nil
	}
	return err
}

// merge folds a follow-up answer into the accumulated one: missing verdicts
// are filled, discoveries and gaps appended, classifications united.
func (p Preload) merge(a, b compactAnswer) compactAnswer {
	if strings.TrimSpace(a.Summary) == "" {
		a.Summary = b.Summary
	}
	have := map[string]bool{}
	for _, v := range a.Verdicts {
		have[p.concernID(v.Concern)] = true
	}
	for _, v := range b.Verdicts {
		if id := p.concernID(v.Concern); !have[id] {
			have[id] = true
			a.Verdicts = append(a.Verdicts, v)
		}
	}
	a.Discovered = append(a.Discovered, b.Discovered...)
	listed := map[string]bool{}
	for _, ref := range a.NoConcerns {
		listed[p.evidenceID(ref)] = true
	}
	for _, ref := range b.NoConcerns {
		if id := p.evidenceID(ref); !listed[id] {
			listed[id] = true
			a.NoConcerns = append(a.NoConcerns, ref)
		}
	}
	a.Gaps = append(a.Gaps, b.Gaps...)
	return a
}

// missing returns the aliases of canonical concerns without a verdict and of
// artifacts the answer leaves unclassified.
func (p Preload) missing(a compactAnswer) (concerns, artifacts []string) {
	verdicts := map[string]bool{}
	for _, v := range a.Verdicts {
		verdicts[p.concernID(v.Concern)] = true
	}
	for i := 1; ; i++ {
		alias := "C" + strconv.Itoa(i)
		id, ok := p.Concerns[alias]
		if !ok {
			break
		}
		if !verdicts[id] {
			concerns = append(concerns, alias)
		}
	}
	covered := map[string]bool{}
	for _, ref := range a.NoConcerns {
		covered[p.evidenceID(ref)] = true
	}
	for _, v := range a.Verdicts {
		for _, ref := range v.Related {
			covered[p.evidenceID(ref)] = true
		}
	}
	for _, d := range a.Discovered {
		covered[p.evidenceID(d.Evidence)] = true
		for _, ref := range d.Related {
			covered[p.evidenceID(ref)] = true
		}
	}
	for i := 1; ; i++ {
		alias := "E" + strconv.Itoa(i)
		id, ok := p.Evidence[alias]
		if !ok {
			break
		}
		if !covered[id] && p.Auto[id] == "" && !p.linked[id] {
			artifacts = append(artifacts, alias)
		}
	}
	return concerns, artifacts
}

func normalizeDisposition(d string) (string, bool) {
	d = strings.NewReplacer(" ", "_", "-", "_").Replace(strings.ToLower(strings.TrimSpace(d)))
	switch d {
	case "fixed", "not_applicable", "non_blocking", "unresolved", "uncertain":
		return d, true
	}
	return "uncertain", false
}

func favorableDisposition(d string) bool {
	return d == "fixed" || d == "not_applicable" || d == "non_blocking"
}

// exactQuote returns the exact span of body that quote names, matching with
// whitespace runs collapsed when the quote is not a verbatim substring.
func exactQuote(body, quote string) (string, bool) {
	trimmed := strings.TrimSpace(quote)
	if trimmed == "" {
		return "", false
	}
	if strings.Contains(body, quote) {
		return quote, true
	}
	if strings.Contains(body, trimmed) {
		return trimmed, true
	}
	want, _ := collapseSpace(trimmed)
	have, offsets := collapseSpace(body)
	at := strings.Index(have, want)
	if want == "" || at < 0 {
		return "", false
	}
	return body[offsets[at] : offsets[at+len(want)-1]+1], true
}

// collapseSpace replaces each whitespace run with one space and returns, for
// every byte of the result, its offset in text.
func collapseSpace(text string) (string, []int) {
	var out strings.Builder
	offsets := make([]int, 0, len(text))
	space := false
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			if !space && out.Len() > 0 {
				out.WriteByte(' ')
				offsets = append(offsets, i)
			}
			space = true
		default:
			out.WriteByte(text[i])
			offsets = append(offsets, i)
			space = false
		}
	}
	return out.String(), offsets
}

var renderedLinePrefix = regexp.MustCompile(`^\s*\d+\s*[|:]\s?`)

// stripLinePrefixes removes the rendered line numbers a model copied from the
// preloaded code, only when every non-empty line carries one.
func stripLinePrefixes(excerpt string) string {
	lines := strings.Split(excerpt, "\n")
	numbered := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !renderedLinePrefix.MatchString(line) {
			return excerpt
		}
		numbered = true
	}
	if !numbered {
		return excerpt
	}
	for i, line := range lines {
		lines[i] = renderedLinePrefix.ReplaceAllString(line, "")
	}
	return strings.Join(lines, "\n")
}

func (p Preload) citations(s Snapshot, cites []compactCite) []Citation {
	bodies := map[string]string{}
	for _, e := range s.Evidence {
		bodies[e.ID] = e.Body
	}
	out := make([]Citation, 0, len(cites))
	for _, c := range cites {
		if strings.TrimSpace(c.Evidence) != "" {
			id := p.evidenceID(c.Evidence)
			excerpt := c.Excerpt
			if exact, ok := exactQuote(bodies[id], excerpt); ok {
				excerpt = exact
			}
			out = append(out, Citation{EvidenceID: id, Excerpt: excerpt})
			continue
		}
		citation := Citation{Revision: p.revision(c.Revision), Path: strings.TrimSpace(c.Path), Excerpt: stripLinePrefixes(c.Excerpt)}
		if len(c.Lines) > 0 {
			citation.StartLine, citation.EndLine = c.Lines[0], c.Lines[0]
		}
		if len(c.Lines) > 1 {
			citation.EndLine = c.Lines[1]
		}
		out = append(out, citation)
	}
	return out
}

// assembleAssessment builds a full assessment from a compact answer. Canonical
// provenance comes from the snapshot, discovered provenance from its source
// artifact, and every artifact is classified from collector links, server
// rules or the answer; an artifact none of them covers becomes a coverage gap.
func assembleAssessment(s Snapshot, p Preload, a compactAnswer) Assessment {
	out := Assessment{SchemaVersion: "1", PolicyVersion: PolicyVersion, PromptVersion: PromptVersion, RuntimeVersion: RuntimeVersion, SnapshotID: s.ID, SnapshotDigest: s.Digest}
	var gaps []string
	evidence := map[string]Evidence{}
	for _, e := range s.Evidence {
		evidence[e.ID] = e
	}
	resolveRelated := func(base []string, refs []string) []string {
		ids := append([]string(nil), base...)
		seen := map[string]bool{}
		for _, id := range ids {
			seen[id] = true
		}
		for _, ref := range refs {
			id := p.evidenceID(ref)
			if _, ok := evidence[id]; ok && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		return ids
	}
	disposition := func(subject, d string) string {
		normalized, ok := normalizeDisposition(d)
		if !ok {
			gaps = append(gaps, fmt.Sprintf("Concern %s: unknown disposition %q treated as uncertain", subject, d))
		}
		return normalized
	}
	rationale := func(text string) string {
		if text = strings.TrimSpace(text); text == "" {
			return "No rationale given."
		}
		return text
	}

	canonical := map[string]bool{}
	for _, c := range s.Concerns {
		canonical[c.ID] = true
	}
	verdicts := map[string]compactVerdict{}
	for _, v := range a.Verdicts {
		id := p.concernID(v.Concern)
		switch {
		case !canonical[id]:
			gaps = append(gaps, fmt.Sprintf("Ignored a verdict for unknown concern %q", v.Concern))
		case verdicts[id].Concern != "":
			gaps = append(gaps, fmt.Sprintf("Ignored a duplicate verdict for concern %s", id))
		default:
			if strings.TrimSpace(v.Concern) == "" {
				v.Concern = id
			}
			verdicts[id] = v
		}
	}
	for _, c := range s.Concerns {
		v, ok := verdicts[c.ID]
		if !ok {
			continue
		}
		concern := Concern{ID: c.ID, EvidenceIDs: resolveRelated(c.EvidenceIDs, v.Related), OriginalSeverity: c.OriginalSeverity, Impact: c.Impact, Claim: c.Claim, OriginalRevision: c.OriginalRevision, Path: c.Path, StartLine: c.StartLine, EndLine: c.EndLine}
		concern.Disposition = disposition(c.ID, v.Disposition)
		concern.Rationale = rationale(v.Rationale)
		concern.Citations = p.citations(s, v.Citations)
		out.Concerns = append(out.Concerns, concern)
	}

	repeats := map[string]string{}
	counts := map[string]int{}
	for _, d := range a.Discovered {
		sourceID := p.evidenceID(d.Evidence)
		source, ok := evidence[sourceID]
		if !ok {
			gaps = append(gaps, fmt.Sprintf("Ignored a discovered concern with unknown source %q", d.Evidence))
			continue
		}
		claim := strings.TrimSpace(d.Claim)
		if claim == "" {
			gaps = append(gaps, "Ignored a discovered concern without a claim from "+sourceID)
			continue
		}
		if exact, ok := exactQuote(source.Body, claim); ok {
			claim = exact
		}
		counts[sourceID]++
		id := fmt.Sprintf("found:%s:%d", sourceID, counts[sourceID])
		concern := Concern{ID: id, Claim: claim, Impact: "unknown", OriginalRevision: source.ReviewedSHA, Path: source.Path, StartLine: source.StartLine, EndLine: source.EndLine}
		concern.Disposition = disposition(id, d.Disposition)
		concern.Rationale = rationale(d.Rationale)
		concern.Citations = p.citations(s, d.Citations)
		concern.EvidenceIDs = []string{sourceID}
		if favorableDisposition(concern.Disposition) {
			for _, rid := range resolveRelated(nil, d.Related) {
				if _, taken := repeats[rid]; !taken && rid != sourceID {
					repeats[rid] = id
				}
			}
		} else {
			concern.EvidenceIDs = resolveRelated(concern.EvidenceIDs, d.Related)
		}
		out.Concerns = append(out.Concerns, concern)
	}

	listed := map[string]bool{}
	for _, ref := range a.NoConcerns {
		listed[p.evidenceID(ref)] = true
	}
	for _, e := range s.Evidence {
		ids := append([]string(nil), e.ConcernIDs...)
		seen := map[string]bool{}
		for _, id := range ids {
			seen[id] = true
		}
		for _, c := range append(append([]Concern(nil), s.Concerns...), out.Concerns...) {
			for _, eid := range c.EvidenceIDs {
				if eid == e.ID && !seen[c.ID] {
					seen[c.ID] = true
					ids = append(ids, c.ID)
				}
			}
		}
		art := ArtifactDisposition{EvidenceID: e.ID, Classification: "non_actionable"}
		switch {
		case len(ids) > 0:
			art.Classification, art.Rationale, art.ConcernIDs = "concerns", "Carries the concerns assessed above.", ids
		case p.Auto[e.ID] != "":
			art.Rationale = p.Auto[e.ID]
		case listed[e.ID]:
			art.Rationale = "Investigator found no actionable concern."
		case repeats[e.ID] != "":
			art.Rationale = "Repeats discovered concern " + repeats[e.ID] + "."
		default:
			art.Rationale = "Not classified by the investigator."
			gaps = append(gaps, "Artifact "+e.ID+" was not classified by the investigator")
		}
		out.Artifacts = append(out.Artifacts, art)
	}

	seenGap := map[string]bool{}
	for _, gap := range append(append([]string(nil), a.Gaps...), gaps...) {
		if gap = strings.TrimSpace(gap); gap != "" && !seenGap[gap] {
			seenGap[gap] = true
			out.CoverageGaps = append(out.CoverageGaps, gap)
		}
	}
	out.Summary = strings.TrimSpace(a.Summary)
	if out.Summary == "" {
		out.Summary = noSummary
	}
	return out
}

const noSummary = "The investigator returned no summary."

// downgradeInvalidDispositions checks each favorable disposition on its own,
// with every other favorable one treated as uncertain, and marks the ones that
// fail validation uncertain. One invalid verdict then no longer forces the
// all-or-nothing fallback that would also discard the valid ones.
func downgradeInvalidDispositions(s Snapshot, a *Assessment) {
	if ValidateAssessment(s, *a) == nil {
		return
	}
	var failed []int
	var reasons []error
	for i, c := range a.Concerns {
		if !favorableDisposition(c.Disposition) {
			continue
		}
		alone := *a
		alone.Concerns = append([]Concern(nil), a.Concerns...)
		for j := range alone.Concerns {
			if j != i && favorableDisposition(alone.Concerns[j].Disposition) {
				alone.Concerns[j].Disposition = "uncertain"
			}
		}
		if err := ValidateAssessment(s, alone); err != nil {
			failed = append(failed, i)
			reasons = append(reasons, err)
		}
	}
	for k, i := range failed {
		c := &a.Concerns[i]
		c.Rationale += " [This " + c.Disposition + " disposition did not pass policy validation (" + reasons[k].Error() + "), so it is marked uncertain.]"
		a.CoverageGaps = append(a.CoverageGaps, "Concern "+c.ID+": "+reasons[k].Error())
		c.Disposition = "uncertain"
	}
}
