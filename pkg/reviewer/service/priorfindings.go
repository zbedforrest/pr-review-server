package service

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"pr-review-server/pkg/reviewer/types"
)

// PriorFinding is one finding PRism already raised on the PR under review,
// read from the publication ledger: what the agent is shown so a restatement
// names its earlier identity instead of minting a new one. ID is the ledger
// fingerprint the publisher aliases on; the prompt shows a short label.
type PriorFinding struct {
	ID       string
	State    string // open | fixed | dismissed | contested | external
	Severity string
	File     string
	// LineFrom and LineTo bound the lines the earlier comment cited; both
	// zero for a whole-file note.
	LineFrom int
	LineTo   int
	Headline string
	// Verdicts are what the author and PRism said in the thread since, in
	// order ("author: intentional", "PRism conceded").
	Verdicts []string
	// SettledOnPR names the pull request of this repo where the author
	// settled the point (an earlier one, or this one for another reviewer's
	// thread settled here); Reason is what they said. Zero for a ledger row.
	SettledOnPR int
	Reason      string
}

const (
	priorFindingsMaxRows       = 40
	priorFindingsHeadlineMax   = 200
	priorFindingsVerdictMax    = 80
	priorFindingsReasonMax     = 160
	priorFindingsSectionBudget = 8000
)

var priorLabelRe = regexp.MustCompile(`^P-([1-9][0-9]*)$`)

func priorLabel(index int) string {
	return "P-" + strconv.Itoa(index+1)
}

// priorFindingsSection renders the "already raised" block: one line per
// earlier finding under a short label, then the rules for prior_id and
// relates_to. Empty input renders nothing, so a run without priors keeps
// the prompt byte-identical.
func priorFindingsSection(priors []PriorFinding) string {
	if len(priors) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(priorFindingsHeader)
	visible := visiblePriors(priors)
	for i, p := range visible {
		b.WriteString("- " + priorLabel(i) + " " + priorFindingLine(p) + "\n")
	}
	if rest := len(priors) - len(visible); rest > 0 {
		fmt.Fprintf(&b, "- ...and %d more earlier findings not listed\n", rest)
	}
	b.WriteString(priorFindingsRules)
	return b.String()
}

const priorFindingsRules = `Rules for these:
- If a finding you would report is the same point as one above (same defect, whatever the wording, line or file), set "prior_id" in its finding_contract to that label, exactly as written, and report it once with your current evidence; the earlier comment is not evidence by itself.
- If it is a distinct point that builds on one above, list those labels in "relates_to" (array of labels) and leave "prior_id" unset.
- A point in state dismissed, contested or external was settled by the author or belongs to another reviewer's thread: do not raise it again under any wording; a finding whose "prior_id" names it is dropped before posting. The one exception is a CRITICAL security_risk whose cited code this push changed: report it with "prior_id" and say what changed.
- These two fields go inside finding_contract alongside its other fields.
- A point in state fixed was judged addressed: raise it again, with "prior_id", only if the defect is still present at HEAD.
- A point marked "settled on #N" was settled by this author on that pull request (an earlier one, or this one when N is its number), with the reason shown: do not raise it again in that file unless the code here differs in a way that answers the reason, and then say how; the same subject in another file is a distinct point. A CRITICAL finding is the exception: report it with "prior_id" and your current evidence, the publisher posts it regardless.
- Never use a label that is not listed here. Both fields are optional and omitted on SUMMARY entries.
`

const priorFindingsHeader = "\n--- FINDINGS ALREADY RAISED ON THIS PR (PRism's own earlier comments on this pull request, and points this author settled recently in this repo) ---\n" +
	"One line per earlier finding: label, state and severity, file and lines, headline, then what the author or PRism said since.\n"

// visiblePriors is the prefix of priors the section lists: at most
// priorFindingsMaxRows lines, with header, rules and the overflow line
// within priorFindingsSectionBudget characters. Labels resolve against this
// same prefix.
func visiblePriors(priors []PriorFinding) []PriorFinding {
	size := len(priorFindingsHeader) + len(priorFindingsRules) + len(fmt.Sprintf("- ...and %d more earlier findings not listed\n", len(priors)))
	for i, p := range priors {
		size += len("- "+priorLabel(i)+" "+priorFindingLine(p)) + 1
		if i == priorFindingsMaxRows || size > priorFindingsSectionBudget {
			return priors[:i]
		}
	}
	return priors
}

// VisiblePriorFindings is the prefix of priors the prompt listed, the set a
// label can resolve to.
func VisiblePriorFindings(priors []PriorFinding) []PriorFinding {
	return visiblePriors(priors)
}

func priorFindingLine(p PriorFinding) string {
	state := p.State
	if state == "" {
		state = "open"
	}
	if p.SettledOnPR > 0 {
		state = fmt.Sprintf("settled on #%d", p.SettledOnPR)
	}
	var b strings.Builder
	b.WriteString("[" + state)
	if p.Severity != "" {
		b.WriteString(", " + p.Severity)
	}
	b.WriteString("] " + p.File)
	switch {
	case p.LineFrom > 0 && p.LineTo > p.LineFrom:
		fmt.Fprintf(&b, ":%d-%d", p.LineFrom, p.LineTo)
	case p.LineFrom > 0:
		fmt.Fprintf(&b, ":%d", p.LineFrom)
	}
	if headline := sanitizeText(p.Headline, priorFindingsHeadlineMax); headline != "" {
		b.WriteString(": " + headline)
	}
	var verdicts []string
	for _, v := range p.Verdicts {
		if v = sanitizeText(v, priorFindingsVerdictMax); v != "" {
			verdicts = append(verdicts, v)
		}
	}
	if reason := sanitizeText(p.Reason, priorFindingsReasonMax); reason != "" {
		verdicts = append(verdicts, "author: "+reason)
	}
	if len(verdicts) > 0 {
		b.WriteString(" (" + strings.Join(verdicts, "; ") + ")")
	}
	return b.String()
}

// PriorRefResolution counts what ResolvePriorRefs did, for the run log.
type PriorRefResolution struct {
	PriorIDs  int // findings whose prior_id named a listed finding
	Related   int // relates_to entries that named a listed finding
	Unlisted  int // references to nothing shown, dropped
	Duplicate int // a second finding claiming a prior another already claimed
}

// ResolvePriorRefs rewrites the labels the agent used in prior_id and
// relates_to to the ledger fingerprints they stand for. A reference to
// nothing that was shown (an unknown label, a fingerprint not listed, any
// reference at all when no priors were given) is dropped, so a prior_id that
// reaches the publisher always names a row the agent was shown; a prior
// claimed by two findings stays with the first active one. Runs after
// dispositions so an entry merged or rejected away cannot hold a claim.
func ResolvePriorRefs(comments []types.LineComment, priors []PriorFinding) PriorRefResolution {
	var res PriorRefResolution
	claimed := map[string]bool{}
	for i := range comments {
		c := comments[i].FindingContract
		if c == nil {
			continue
		}
		if comments[i].Inactive {
			c.PriorID, c.RelatesTo = "", nil
			continue
		}
		if c.PriorID != "" {
			id, ok := resolvePriorRef(c.PriorID, priors)
			switch {
			case !ok:
				res.Unlisted++
				c.PriorID = ""
			case claimed[id]:
				res.Duplicate++
				c.PriorID = ""
			default:
				claimed[id] = true
				res.PriorIDs++
				c.PriorID = id
			}
		}
		var related []string
		seen := map[string]bool{c.PriorID: true}
		for _, ref := range c.RelatesTo {
			id, ok := resolvePriorRef(ref, priors)
			if !ok {
				res.Unlisted++
				continue
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			res.Related++
			related = append(related, id)
		}
		c.RelatesTo = related
	}
	return res
}

func resolvePriorRef(ref string, priors []PriorFinding) (string, bool) {
	ref = strings.TrimSpace(ref)
	visible := visiblePriors(priors)
	if m := priorLabelRe.FindStringSubmatch(ref); m != nil {
		n, _ := strconv.Atoi(m[1])
		if n >= 1 && n <= len(visible) {
			return visible[n-1].ID, true
		}
		return "", false
	}
	for _, p := range visible {
		if p.ID == ref {
			return p.ID, true
		}
	}
	return "", false
}
