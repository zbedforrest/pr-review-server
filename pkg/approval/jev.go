package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProviderJev scores pull requests with TypeSafe's Jev decision model through
// OpenRouter's System One endpoint instead of running a generative
// investigation.
const ProviderJev = "jev"

const jevEndpoint = "https://openrouter.ai/api/v1/systemone"

// Score is a pull request's approval readiness from 0 to 100 with the facts
// and per-concern estimates behind it. Every eligible pull request gets one;
// Candidate says whether it clears the threshold with no hard blocker.
type Score struct {
	Value      float64        `json:"value"`
	Threshold  float64        `json:"threshold"`
	Candidate  bool           `json:"candidate"`
	Readiness  float64        `json:"readiness"`
	Risk       float64        `json:"risk"`
	Blockers   []string       `json:"blockers,omitempty"`
	Deductions []Deduction    `json:"deductions,omitempty"`
	Concerns   []ConcernScore `json:"concerns,omitempty"`
	Model      string         `json:"model"`
	LatencyMS  int64          `json:"latency_ms"`
}

// Deduction is a fixed point loss for a snapshot fact.
type Deduction struct {
	Reason string  `json:"reason"`
	Points float64 `json:"points"`
}

// ConcernScore is Jev's estimate for one review concern: the probability it
// is resolved at head, its expected impact level (0 cosmetic to 3 severe), and
// the risk it contributes after severity weighting.
type ConcernScore struct {
	ID          string  `json:"id"`
	Claim       string  `json:"claim"`
	Severity    string  `json:"severity"`
	PResolved   float64 `json:"p_resolved"`
	Impact      float64 `json:"impact"`
	Weight      float64 `json:"weight"`
	Risk        float64 `json:"risk"`
	Disposition string  `json:"disposition"`
	OnHead      bool    `json:"on_head"`
}

// Scoring knobs. impactWeights weights Jev's impact levels (cosmetic, minor,
// real defect, severe) so one open minor concern costs about 5 points, one open
// real defect lands just under the default cutoff and one severe concern near
// 25. A concern raised on an older revision that the review of the current
// head did not repeat is usually fixed, so it counts at supersededWeight. A
// concern's risk is P(unresolved) times its weight; current-head risks combine
// as independent chances that something blocks, while superseded concerns are
// correlated restatements, so only the worst of them counts.
var (
	impactWeights       = [4]float64{0, 0.05, 0.35, 0.8}
	supersededWeight    = 0.15
	DefaultScoreCutoff  = 80.0
	blockerCap          = 20.0
	draftDeduction      = 40.0
	pendingCIDeduction  = 5.0
	inProgressDeduction = 10.0
	incompleteDeduction = 10.0
)

// jevStateBudget bounds the state of one request, about 25k tokens, which
// leaves room for the longest question inside Jev's 32k-token limit.
const jevStateBudget = 90_000

// JevScorer implements Investigator by asking Jev, per pull request, whether
// each concern is resolved at head and how much it would matter, plus an
// overall readiness score, and combining those with the snapshot's hard facts.
type JevScorer struct {
	Config ModelConfig
	Cutoff float64
}

func (j JevScorer) Investigate(ctx context.Context, s Snapshot, repo Repository, budget Budget) (Assessment, error) {
	started := time.Now()
	base := Assessment{SchemaVersion: "1", PolicyVersion: PolicyVersion, PromptVersion: PromptVersion, RuntimeVersion: RuntimeVersion, SnapshotID: s.ID, SnapshotDigest: s.Digest, Model: j.Config.Model, AssessedAt: time.Now().UTC()}
	facts := Evaluate(s, base).ReasonCodes
	contexts := concernContexts(ctx, s, repo)
	chunks := chunkConcerns(s, contexts)
	type result struct {
		answers map[string]jevAnswer
		err     error
	}
	results := make([]result, len(chunks))
	var wg sync.WaitGroup
	for i, chunk := range chunks {
		wg.Add(1)
		go func(i int, chunk []jevConcern) {
			defer wg.Done()
			answers, err := j.ask(ctx, s, chunk, i == 0)
			results[i] = result{answers, err}
		}(i, chunk)
	}
	wg.Wait()
	answers := map[string]jevAnswer{}
	for _, r := range results {
		if r.err != nil {
			return base, r.err
		}
		for k, v := range r.answers {
			answers[k] = v
		}
	}
	score := j.combine(s, facts, contexts, answers)
	score.Model = j.Config.Model
	score.LatencyMS = time.Since(started).Milliseconds()
	a := base
	a.Score = &score
	a.ReasonCodes = scoreReasons(score)
	a.Decision = "needs_attention"
	if score.Candidate {
		a.Decision = "candidate"
	}
	a.Summary = scoreSummary(score)
	return a, nil
}

type jevConcern struct {
	Alias    string `json:"-"`
	ID       string `json:"-"`
	Severity string `json:"reported_severity"`
	Reporter string `json:"reported_by"`
	Claim    string `json:"claim"`
	File     string `json:"file,omitempty"`
	Lines    string `json:"lines,omitempty"`
	OnHead   bool   `json:"raised_on_current_head"`
	HeadCode string `json:"code_at_head,omitempty"`
	Diff     string `json:"pr_diff_for_file,omitempty"`
}

func concernContexts(ctx context.Context, s Snapshot, repo Repository) []jevConcern {
	reporters := map[string]string{}
	for _, src := range s.Sources {
		reporters[src.ID] = src.Provider
	}
	byEvidence := map[string]string{}
	for _, e := range s.Evidence {
		byEvidence[e.ID] = reporters[e.SourceID]
	}
	diffs := map[string]string{}
	out := make([]jevConcern, 0, len(s.Concerns))
	for i, c := range s.Concerns {
		jc := jevConcern{Alias: "C" + strconv.Itoa(i+1), ID: c.ID, Severity: orUnknown(c.OriginalSeverity), Claim: clip(c.Claim, 2000), File: c.Path, OnHead: c.OriginalRevision == s.Revision.Head}
		for _, id := range c.EvidenceIDs {
			if r := byEvidence[id]; r != "" {
				jc.Reporter = r
				break
			}
		}
		if c.Path != "" && repo != nil {
			if c.StartLine > 0 {
				jc.Lines = fmt.Sprintf("%d-%d", c.StartLine, max(c.StartLine, c.EndLine))
				start := max(1, c.StartLine-25)
				if r, err := repo.Read(ctx, "read_file", ReadRequest{Revision: s.Revision.Head, Path: c.Path, StartLine: start, EndLine: start + 79}); err == nil {
					jc.HeadCode = clip(r.Text, 6000)
				}
			}
			d, ok := diffs[c.Path]
			if !ok && s.Revision.MergeBase != "" {
				if r, err := repo.Read(ctx, "read_diff", ReadRequest{Revision: s.Revision.Head, OtherRevision: s.Revision.MergeBase, Path: c.Path}); err == nil {
					d = clip(r.Text, 8000)
				}
				diffs[c.Path] = d
			}
			jc.Diff = d
		}
		out = append(out, jc)
	}
	return out
}

// chunkConcerns splits concerns so each request's state stays inside Jev's
// context budget; most pull requests fit in one request.
func chunkConcerns(s Snapshot, cs []jevConcern) [][]jevConcern {
	header := len(prSummaryJSON(s))
	var chunks [][]jevConcern
	var cur []jevConcern
	size := header
	for _, c := range cs {
		b, _ := json.Marshal(c)
		n := len(b)
		if len(cur) > 0 && size+n > jevStateBudget {
			chunks = append(chunks, cur)
			cur, size = nil, header
		}
		if header+n > jevStateBudget {
			c.HeadCode, c.Diff = clip(c.HeadCode, 2000), clip(c.Diff, 3000)
		}
		cur = append(cur, c)
		size += n
	}
	if len(cur) > 0 || len(chunks) == 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

func prSummary(s Snapshot) map[string]any {
	checks := map[string][]string{}
	for _, c := range s.Checks {
		if c.SHA == s.Revision.Head {
			checks[c.State] = append(checks[c.State], c.Name)
		}
	}
	var reviewers []string
	for _, src := range s.Sources {
		if src.ReviewedSHA == s.Revision.Head && src.Completion == "completed" {
			reviewers = append(reviewers, src.Provider)
		}
	}
	sort.Strings(reviewers)
	return map[string]any{
		"repository":                 s.Target.Owner + "/" + s.Target.Repo,
		"number":                     s.Target.Number,
		"draft":                      s.Draft,
		"human_changes_requested":    s.HumanChangesRequested,
		"provider_changes_requested": s.ProviderChangesRequested,
		"review_in_progress":         s.ReviewInProgress,
		"checks_on_head":             checks,
		"completed_reviews_on_head":  reviewers,
	}
}

func prSummaryJSON(s Snapshot) []byte {
	b, _ := json.Marshal(prSummary(s))
	return b
}

type jevAnswer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Score         float64            `json:"score"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

func (j JevScorer) ask(ctx context.Context, s Snapshot, chunk []jevConcern, readiness bool) (map[string]jevAnswer, error) {
	concerns := map[string]jevConcern{}
	questions := map[string]any{}
	for _, c := range chunk {
		concerns[c.Alias] = c
		ref := "`concerns." + c.Alias + "`"
		questions[c.Alias+"_resolved"] = map[string]any{"type": "noul", "instructions": "Is the review concern " + ref + " resolved by the pull request's code at head?", "criteria": map[string]any{"true": "The code at head fixes what the concern describes, or the concern does not apply to this code", "false": "The problem the concern describes still exists at head, or the context does not show it was addressed"}}
		questions[c.Alias+"_impact"] = map[string]any{"type": "score", "instructions": "If the review concern " + ref + " were still unresolved, how much would it matter for merging this pull request?", "criteria": []string{"Cosmetic, style or naming only", "Minor; safe to merge and fix later", "Real defect that should be fixed before merge", "Severe: security, data loss, outage or broken core behavior"}}
		questions[c.Alias+"_disposition"] = map[string]any{"type": "choice", "instructions": "What best describes the review concern " + ref + " at head?", "criteria": map[string]string{"fixed": "The code at head fixes it", "not_applicable": "The concern is mistaken or does not apply to this code", "still_present": "The problem still exists at head", "style_only": "Only a style, naming or preference comment", "cannot_tell": "The provided context is not enough to decide"}}
	}
	if readiness {
		questions["readiness"] = map[string]any{"type": "score", "instructions": "Given `pull_request` and the review `concerns`, how ready is this pull request for a human to approve now?", "criteria": []string{"Not ready: blocking problems remain", "Needs significant work before approval", "Needs minor follow-ups but could be approved with notes", "Ready: only trivial or no remaining concerns", "Clearly ready: verified clean"}}
	}
	state := scrubSecrets(map[string]any{"pull_request": prSummary(s), "concerns": concerns})
	if state == nil {
		return nil, fmt.Errorf("could not prepare Jev state")
	}
	body, err := json.Marshal(map[string]any{"model": j.Config.Model, "state": state, "questions": questions})
	if err != nil {
		return nil, err
	}
	endpoint := j.Config.BaseURL
	if endpoint == "" {
		endpoint = jevEndpoint
	}
	raw, err := j.Config.post(ctx, endpoint, body)
	if err != nil {
		return nil, err
	}
	var reply struct {
		Answers map[string]jevAnswer `json:"answers"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("invalid Jev response: %w", err)
	}
	for q := range questions {
		if _, ok := reply.Answers[q]; !ok {
			return nil, fmt.Errorf("Jev response missing answer %s", q)
		}
	}
	return reply.Answers, nil
}

func (j JevScorer) combine(s Snapshot, facts []string, cs []jevConcern, answers map[string]jevAnswer) Score {
	cutoff := j.Cutoff
	if cutoff <= 0 {
		cutoff = DefaultScoreCutoff
	}
	score := Score{Threshold: cutoff}
	survive, superseded := 1.0, 0.0
	for _, c := range cs {
		resolved := answers[c.Alias+"_resolved"]
		impact := answers[c.Alias+"_impact"]
		disposition := answers[c.Alias+"_disposition"]
		weight := 0.0
		for level, p := range impact.Probabilities {
			if i, err := strconv.Atoi(level); err == nil && i >= 0 && i < len(impactWeights) {
				weight += p * impactWeights[i]
			}
		}
		if !c.OnHead {
			weight *= supersededWeight
		}
		risk := clamp01(1-resolved.Noul) * weight
		if c.OnHead {
			survive *= 1 - risk
		} else {
			superseded = math.Max(superseded, risk)
		}
		score.Concerns = append(score.Concerns, ConcernScore{ID: c.ID, Claim: shorten(c.Claim, 400), Severity: c.Severity, PResolved: round2(resolved.Noul), Impact: round2(impact.Score), Weight: round2(weight), Risk: round2(risk), Disposition: disposition.Choice, OnHead: c.OnHead})
	}
	survive *= 1 - superseded
	sort.SliceStable(score.Concerns, func(a, b int) bool { return score.Concerns[a].Risk > score.Concerns[b].Risk })
	score.Risk = round2(1 - survive)
	if r, ok := answers["readiness"]; ok {
		score.Readiness = round2(r.Score / 4 * 100)
	}
	value := 100 * survive
	has := func(code string) bool {
		for _, f := range facts {
			if f == code {
				return true
			}
		}
		return false
	}
	deduct := func(reason string, points float64) {
		score.Deductions = append(score.Deductions, Deduction{reason, points})
		value -= points
	}
	if s.Draft {
		deduct("pr_draft", draftDeduction)
	}
	if has("ci_pending") {
		deduct("ci_pending", pendingCIDeduction)
	}
	if s.ReviewInProgress {
		deduct("review_in_progress", inProgressDeduction)
	}
	if has("source_incomplete") {
		deduct("source_incomplete", incompleteDeduction)
	}
	for _, code := range []string{"ci_failed", "human_changes_requested", "provider_changes_requested"} {
		if has(code) {
			score.Blockers = append(score.Blockers, code)
		}
	}
	if len(score.Blockers) > 0 {
		value = math.Min(value, blockerCap)
	}
	score.Value = round2(math.Max(0, math.Min(100, value)))
	// Pending CI costs only a few points but blocks the candidate flag until checks finish.
	score.Candidate = len(score.Blockers) == 0 && !s.Draft && !has("ci_pending") && score.Value >= cutoff
	return score
}

func scoreReasons(s Score) []string {
	reasons := append([]string(nil), s.Blockers...)
	for _, d := range s.Deductions {
		reasons = append(reasons, d.Reason)
	}
	if !s.Candidate && len(s.Blockers) == 0 {
		reasons = append(reasons, "score_below_threshold")
	}
	return reasons
}

func scoreSummary(s Score) string {
	parts := []string{fmt.Sprintf("Approval score %.0f/100 (threshold %.0f).", s.Value, s.Threshold)}
	if len(s.Blockers) > 0 {
		parts = append(parts, "Blocked by "+strings.Join(s.Blockers, ", ")+".")
	}
	var top []string
	for _, c := range s.Concerns {
		if c.Risk < 0.05 || len(top) == 3 {
			break
		}
		top = append(top, fmt.Sprintf("%s (%.0f%% likely unresolved, impact %.1f/3)", shorten(c.Claim, 120), 100*(1-c.PResolved), c.Impact))
	}
	if len(top) > 0 {
		parts = append(parts, "Top risks: "+strings.Join(top, "; ")+".")
	}
	return strings.Join(parts, " ")
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`(?i)\b(password|passwd|secret|api[_-]?key|token)\b(\s*[:=]\s*)["']?[^\s"']{8,}`),
}

// scrubSecrets redacts credential-shaped strings in every string value
// before any text leaves the server; Jev's approval forbids submitting
// credentials.
func scrubSecrets(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var generic any
	if json.Unmarshal(b, &generic) != nil {
		return nil
	}
	return scrubValue(generic)
}

func scrubValue(v any) any {
	switch t := v.(type) {
	case string:
		for _, p := range secretPatterns {
			t = p.ReplaceAllStringFunc(t, func(m string) string {
				if sub := p.FindStringSubmatch(m); len(sub) == 4 {
					return sub[1] + sub[2] + "[redacted]"
				}
				return "[redacted]"
			})
		}
		return t
	case map[string]any:
		for k, x := range t {
			t[k] = scrubValue(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = scrubValue(x)
		}
		return t
	}
	return v
}

func clip(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[:n] + "\n[truncated]"
}

// shorten cuts display text at a word boundary with an ellipsis.
func shorten(text string, n int) string {
	if len(text) <= n {
		return text
	}
	cut := text[:n]
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:") + "…"
}

func clamp01(x float64) float64 { return math.Max(0, math.Min(1, x)) }

func round2(x float64) float64 { return math.Round(x*100) / 100 }
