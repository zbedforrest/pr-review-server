package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/types"
)

// Completer runs one schema-enforced completion (llm.OpenRouterClient).
type Completer interface {
	CompleteStructured(ctx context.Context, req llm.StructuredRequest) (string, llm.Call, error)
}

// Verifier asks one model to judge every merged finding against its packet.
type Verifier struct {
	Client        Completer
	Model         string
	ProviderOrder []string
	MaxTokens     int
}

// Result is the findings with a Verification on each, unapplied: the caller
// sets Applied for the mode and decides whether a gap fails the round.
type Result struct {
	Comments []types.LineComment
	Call     llm.Call
	// Missing lists finding keys the reply gave no decision for; Invalid
	// lists keys whose decision broke a rule and was turned into a hold.
	Missing []string
	Invalid []string
}

// Complete reports whether every finding got a decision.
func (r Result) Complete() bool { return len(r.Missing) == 0 }

const (
	reviseLineWindow = 15
	maxCommentRunes  = 4000
	maxReasonRunes   = 500
	maxQuoteLines    = 12
)

// EstimateTokens approximates the prompt size for a budget check.
func EstimateTokens(pkt *Packet, findings []Finding) int {
	return (len(verifySystem) + len(verifyPrompt(pkt, findings))) / 4
}

// Verify judges findings against pkt. On an error no finding was judged and
// the caller publishes the merge unchanged.
func (v Verifier) Verify(ctx context.Context, pkt *Packet, findings []Finding) (Result, error) {
	maxTokens := v.MaxTokens
	if maxTokens == 0 {
		maxTokens = 16000
	}
	out, call, err := v.Client.CompleteStructured(ctx, llm.StructuredRequest{
		Model: v.Model, System: verifySystem, User: verifyPrompt(pkt, findings),
		SchemaName: "verification", Schema: json.RawMessage(verifySchema),
		MaxTokens: maxTokens, ProviderOrder: v.ProviderOrder,
	})
	res := Result{Call: call}
	if err != nil {
		return res, err
	}
	var reply struct {
		Decisions []decision `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		return res, fmt.Errorf("verification reply: %w", err)
	}
	model := call.ServedModel
	if model == "" {
		model = v.Model
	}
	byKey := map[string]decision{}
	dup := map[string]bool{}
	for _, d := range reply.Decisions {
		if _, ok := byKey[d.Key]; ok {
			dup[d.Key] = true
		}
		byKey[d.Key] = d
	}
	for _, f := range findings {
		c := f.Comment
		d, ok := byKey[f.Key]
		if !ok || dup[f.Key] {
			res.Missing = append(res.Missing, f.Key)
			c.Verification = &types.Verification{Status: types.VerifySkipped, Model: model, Reason: "no single decision for this finding"}
			res.Comments = append(res.Comments, c)
			continue
		}
		c, valid := apply(c, d, pkt, pkt.ByFinding[f.Key])
		c.Verification.Model = model
		if !valid {
			res.Invalid = append(res.Invalid, f.Key)
		}
		res.Comments = append(res.Comments, c)
	}
	return res, nil
}

type decision struct {
	Key      string    `json:"key"`
	Decision string    `json:"decision"`
	Cites    []string  `json:"cites"`
	Reason   string    `json:"reason"`
	Quote    *quote    `json:"refuting_quote"`
	Revised  *Revision `json:"revised"`
}

type quote struct {
	Item  string `json:"item"`
	Start int    `json:"start_line"`
	End   int    `json:"end_line"`
}

type Revision struct {
	LineNumber            int     `json:"line_number"`
	Importance            string  `json:"importance"`
	Headline              string  `json:"headline"`
	CommentBody           string  `json:"comment_body"`
	Materiality           string  `json:"materiality"`
	CurrentImpact         string  `json:"current_impact"`
	CounterfactualTrigger *string `json:"counterfactual_trigger"`
	Falsifiability        string  `json:"falsifiability"`
	FalsifiableCondition  *string `json:"falsifiable_condition"`
	ExpectedObservable    *string `json:"expected_observable"`
	Uncertainty           string  `json:"uncertainty"`
	SeverityRationale     string  `json:"severity_rationale"`
}

// ruling is one decision in the form both phases share: cites already
// checked (packet ids for Phase 1, resolving file:line for the agent), and
// the withdrawal quote built from them.
type ruling struct {
	decision string
	reason   string
	cites    []string
	evidence func() string
	revised  *Revision
}

// apply turns one Phase 1 decision into the finding's Verification.
func apply(c types.LineComment, d decision, pkt *Packet, own []string) (types.LineComment, bool) {
	relevant := relevantCites(d.Cites, own)
	return applyRuling(c, ruling{
		decision: d.Decision, reason: d.Reason, cites: relevant, revised: d.Revised,
		evidence: func() string { return evidence(pkt, relevant, d.Quote) },
	})
}

// applyRuling records a ruling on the finding. Only a cited hold or drop on
// a non-CRITICAL finding suppresses it; any decision that breaks a rule, and
// any hold or drop on a CRITICAL, keeps the finding as merged with the
// model's decision recorded in Proposed. It reports whether the decision was
// valid as given.
func applyRuling(c types.LineComment, r ruling) (types.LineComment, bool) {
	v := &types.Verification{Cites: r.cites, Reason: cut(r.reason, maxReasonRunes)}
	c.Verification = v
	keepAs := func(proposed, why string, valid bool) (types.LineComment, bool) {
		v.Status, v.Proposed = types.VerifyKeep, proposed
		v.Reason = cut(strings.TrimSpace(why+". "+v.Reason), maxReasonRunes)
		return c, valid
	}
	critical := strings.EqualFold(strings.TrimSpace(c.Importance), "CRITICAL")

	switch r.decision {
	case "keep":
		v.Status = types.VerifyKeep
		return c, true
	case "evidence_unavailable":
		v.Status = types.VerifyUnavailable
		return c, true
	case "hold", "drop":
		status := types.VerifyHold
		if r.decision == "drop" {
			status = types.VerifyDrop
		}
		if len(r.cites) == 0 {
			return keepAs(status, r.decision+" without a cite to this finding's evidence", false)
		}
		if status == types.VerifyDrop {
			v.Evidence = r.evidence()
		}
		if critical {
			return keepAs(status, "CRITICAL never suppressed", true)
		}
		v.Status = status
		return c, true
	case "revise":
		if len(r.cites) == 0 {
			return keepAs(types.VerifyRevise, "revise without a cite to this finding's evidence", false)
		}
		if r.revised == nil {
			return keepAs(types.VerifyRevise, "revise without a replacement", false)
		}
		revised, why := revise(c, *r.revised)
		if why != "" {
			return keepAs(types.VerifyRevise, why, false)
		}
		revised.Verification = v
		v.Status = types.VerifyRevise
		v.PriorComment = c.CommentBody
		v.PriorContract = c.FindingContract
		return revised, true
	}
	return keepAs(r.decision, "unknown decision "+strconv.Quote(r.decision), false)
}

func relevantCites(cites, own []string) []string {
	ownSet := make(map[string]bool, len(own))
	for _, id := range own {
		ownSet[id] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, id := range cites {
		id = strings.TrimSpace(id)
		if ownSet[id] && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

var suggestionBlock = regexp.MustCompile("(?s)```suggestion.*?```")

// revise builds the replacement finding, or says why it must be held: it
// may not move beyond the original line's neighborhood, change the file or
// kind, carry an invalid contract, run long, or contain a credential.
func revise(c types.LineComment, r Revision) (types.LineComment, string) {
	if c.FindingContract == nil {
		return c, "revise of a finding without a contract"
	}
	if r.LineNumber < c.LineNumber-reviseLineWindow || r.LineNumber > c.LineNumber+reviseLineWindow {
		return c, "revise moved the finding too far"
	}
	importance := strings.ToUpper(strings.TrimSpace(r.Importance))
	if importance != "CRITICAL" && importance != "MEDIUM" && importance != "LOW" {
		return c, "revise with an unknown importance"
	}
	body := strings.TrimSpace(suggestionBlock.ReplaceAllString(r.CommentBody, ""))
	if body == "" || len([]rune(body)) > maxCommentRunes {
		return c, "revise with an empty or over-long body"
	}
	contract := *c.FindingContract
	contract.Subjects = append([]types.FindingSubject(nil), c.FindingContract.Subjects...)
	contract.RelatesTo = append([]string(nil), c.FindingContract.RelatesTo...)
	contract.Headline = r.Headline
	contract.Materiality = r.Materiality
	contract.CurrentImpact = r.CurrentImpact
	contract.CounterfactualTrigger = r.CounterfactualTrigger
	contract.Falsifiability = r.Falsifiability
	contract.FalsifiableCondition = r.FalsifiableCondition
	contract.ExpectedObservable = r.ExpectedObservable
	contract.Uncertainty = r.Uncertainty
	contract.SeverityRationale = r.SeverityRationale
	reconcileContract(&contract)
	types.NormalizeFindingContract(&contract)
	if err := types.ValidateFindingContract(&contract); err != nil {
		return c, "revised contract is invalid: " + err.Error()
	}
	for _, text := range []string{body, contract.Headline, contract.CurrentImpact, contract.Uncertainty, contract.SeverityRationale} {
		if hasSecret(text) {
			return c, "revised text contains a credential-shaped string"
		}
	}
	out := c
	out.LineNumber = r.LineNumber
	out.Importance = importance
	out.CommentBody = body
	out.FindingContract = &contract
	return out, ""
}

// reconcileContract clears the fields a revised contract's own choices rule
// out, so a model that filled every schema field still validates: a trigger
// only belongs to future-only materiality, an experiment only to falsifiable
// findings, and opinions and description drift are never falsifiable.
func reconcileContract(c *types.FindingContract) {
	if c.FindingKind == "design_opinion" || c.FindingKind == "description_drift" {
		c.Falsifiability = "not_falsifiable"
	}
	if c.Materiality != "future_condition_only" {
		c.CounterfactualTrigger = nil
	}
	if c.Falsifiability != "falsifiable" {
		c.FalsifiableCondition, c.ExpectedObservable = nil, nil
	}
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
}

func hasSecret(s string) bool {
	for _, p := range secretPatterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}

var numberedLine = regexp.MustCompile(`^\s*(\d+)\| ?(.*)$`)

// evidence quotes the refuting lines for a withdrawal: the model's quoted
// range when it lies inside a cited code item, else the start of the first
// cited code item. Notes are quoted as text.
func evidence(pkt *Packet, cites []string, q *quote) string {
	pick := func(id string) (Item, bool) {
		it, ok := pkt.Item(id)
		return it, ok && it.Kind != KindNote && it.Kind != KindPR
	}
	if q != nil {
		for _, id := range cites {
			if id != q.Item {
				continue
			}
			if it, ok := pick(id); ok && q.End >= q.Start && q.End-q.Start < maxQuoteLines {
				if s := quoteLines(it, q.Start, q.End); s != "" {
					return s
				}
			}
		}
	}
	for _, id := range cites {
		if it, ok := pick(id); ok {
			return quoteLines(it, it.Start, min(it.End, it.Start+maxQuoteLines-1))
		}
	}
	it, _ := pkt.Item(cites[0])
	return cut(it.Text, maxReasonRunes)
}

func quoteLines(it Item, start, end int) string {
	var lines []string
	first, last := 0, 0
	for _, l := range strings.Split(it.Text, "\n") {
		m := numberedLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n < start || n > end {
			continue
		}
		if first == 0 {
			first = n
		}
		last = n
		lines = append(lines, m[2])
	}
	if len(lines) == 0 {
		return ""
	}
	loc := it.Path + ":" + strconv.Itoa(first)
	if last != first {
		loc += "-" + strconv.Itoa(last)
	}
	if it.Kind == KindBase {
		loc += " (before this PR)"
	}
	return loc + "\n" + strings.Join(lines, "\n")
}

func cut(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}
