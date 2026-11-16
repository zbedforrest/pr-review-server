package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"pr-review-server/pkg/grader"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
)

const (
	gateModeShadow = "shadow"
	gateModeOn     = "on"
)

// GateReport is what the quality gate did on one review.
type GateReport struct {
	Mode       string         `json:"mode"`
	Status     string         `json:"status"`
	Skip       string         `json:"skip,omitempty"`
	Graded     int            `json:"graded"`
	Grades     map[string]int `json:"grades"`
	Dropped    int            `json:"dropped"`
	Demoted    int            `json:"demoted"`
	CostUSD    float64        `json:"cost_usd"`
	DurationMS int64          `json:"duration_ms"`
	Error      string         `json:"error,omitempty"`
}

// gateItem is one decision as logged for review in shadow mode.
type gateItem struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Grade    string `json:"grade"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
	Headline string `json:"headline"`
}

var (
	gateRunFn      = service.RunAgentGrade
	gateBackground = make(chan struct{}, 4)
	gateSpendMu    sync.Mutex
	gateSpendDay   string
	gateSpend      float64
)

func gateSpent(day string, add float64) float64 {
	gateSpendMu.Lock()
	defer gateSpendMu.Unlock()
	if gateSpendDay != day {
		gateSpendDay, gateSpend = day, 0
	}
	gateSpend += add
	return gateSpend
}

// gateTimeoutAllowance is the time on mode adds to an ensemble review.
func (p *Poller) gateTimeoutAllowance() time.Duration {
	if p.cfg == nil || p.cfg.QualityGate != gateModeOn {
		return 0
	}
	return time.Duration(p.cfg.GraderWallClockSec) * time.Second
}

// qualityGateReview grades every finding a publish-bound Lite (ensemble)
// review is about to post with the grader rubric, including the fallback
// run's review. In on mode a BAD grade keeps the finding off GitHub (a
// CRITICAL is posted as a LOW note instead); shadow mode only records. Any
// failure leaves the review as it was, and every outcome logs a [GATE] line.
func (p *Poller) qualityGateReview(ctx context.Context, execution *reviewExecution, base service.AgentConfig, result *service.ReviewResult, review *service.AgentReview) *GateReport {
	if p.cfg == nil || (p.cfg.QualityGate != gateModeShadow && p.cfg.QualityGate != gateModeOn) {
		return nil
	}
	rep := &GateReport{Mode: p.cfg.QualityGate, Grades: map[string]int{}}
	job := execution.Job
	skip := func(why string) *GateReport {
		rep.Status, rep.Skip = "skipped", why
		log.Printf("[GATE %s/%s#%d] {\"mode\":%q,\"status\":\"skipped\",\"skip\":%q}", job.PR.Owner, job.PR.Repo, job.PR.Number, rep.Mode, why)
		return rep
	}
	if !p.reviewPublishBound(job) {
		return skip("not_publish_bound")
	}
	prior, posted := gatePriors(p.priorFindingsFor(job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.Author))
	var items []grader.Item
	var index []int
	for i, c := range review.Comments {
		if c.FilePath == "SUMMARY" || c.FilePath == "CHECK" || c.Inactive || c.Verification.Suppresses() {
			continue
		}
		kind := grader.KindSummaryNote
		if !strings.EqualFold(strings.TrimSpace(c.Importance), "LOW") {
			kind = grader.KindInline
		}
		items = append(items, grader.Item{ID: "F" + strconv.Itoa(len(items)+1), Kind: kind, File: c.FilePath, Line: c.LineNumber, Severity: strings.ToLower(c.Importance), Comment: c.CommentBody,
			AlreadyPosted: posted[payload.Fingerprint(c.FilePath, c.LineNumber, c.CommentBody)]})
		index = append(index, i)
	}
	if len(items) == 0 {
		return skip("no_findings")
	}
	day := time.Now().UTC().Format("2006-01-02")
	if p.cfg.QualityGateBudgetUSD > 0 && gateSpent(day, 0) >= p.cfg.QualityGateBudgetUSD {
		return skip("over_budget")
	}
	in := service.GradeAgentInput{Owner: job.PR.Owner, Repo: job.PR.Repo, PRNumber: job.PR.Number, DefaultBranch: result.BaseRef,
		HeadSHA: job.PR.CommitSHA, Title: result.PRTitle, Body: result.PRBody, Items: items, Prior: prior}
	if review.Scope != nil {
		in.BaseSHA = review.Scope.MergeBase
	}
	cfg := p.verifyAgentConfig(ctx, base.CloneRootDir)
	cfg.Backend, cfg.Model = service.AgentBackendClaude, p.cfg.GraderModel
	cfg.WallClock = time.Duration(p.cfg.GraderWallClockSec) * time.Second
	cfg.MaxTurns = p.cfg.GraderMaxTurns

	if rep.Mode == gateModeShadow {
		// The background run gets its own report and findings, so nothing it
		// writes is shared with the review the caller keeps publishing.
		r := GateReport{Mode: rep.Mode, Grades: map[string]int{}}
		comments := append([]types.LineComment(nil), review.Comments...)
		run := gateRunFn
		select {
		case gateBackground <- struct{}{}:
			go func() {
				defer func() { <-gateBackground }()
				gctx, cancel := context.WithTimeout(context.Background(), cfg.WallClock)
				defer cancel()
				p.runGate(gctx, run, cfg, in, comments, index, &r, day, false)
			}()
			rep.Status = "pending"
		default:
			return skip("busy")
		}
		return rep
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < cfg.WallClock {
		return skip("deadline")
	}
	gctx, cancel := context.WithTimeout(ctx, cfg.WallClock)
	defer cancel()
	p.runGate(gctx, gateRunFn, cfg, in, review.Comments, index, rep, day, true)
	return rep
}

// runGate runs the grader and records each grade on its finding; apply
// changes what posts. Shadow runs pass a copy of the report and never apply,
// so they only log.
func (p *Poller) runGate(ctx context.Context, run func(context.Context, service.AgentConfig, service.Spawner, service.GradeAgentInput) (*service.GradeAgentResult, error), cfg service.AgentConfig, in service.GradeAgentInput, comments []types.LineComment, index []int, rep *GateReport, day string, apply bool) {
	started := time.Now()
	res, err := run(ctx, cfg, p.agentSpawner, in)
	if res != nil {
		rep.CostUSD = res.CostUSD
		gateSpent(day, res.CostUSD)
	}
	rep.DurationMS = time.Since(started).Milliseconds()
	if err != nil {
		rep.Status, rep.Error = "error", err.Error()
		logGate(in, *rep, nil)
		return
	}
	byID := make(map[string]grader.Grade, len(res.Grades))
	for _, g := range grader.ResolveDuplicates(in.Items, res.Grades) {
		byID[g.ID] = g
	}
	var logged []gateItem
	for k, it := range in.Items {
		g, ok := byID[it.ID]
		if !ok {
			continue
		}
		rep.Graded++
		rep.Grades[g.Grade]++
		c := comments[index[k]]
		logged = append(logged, gateItem{File: c.FilePath, Line: c.LineNumber, Severity: c.Importance, Grade: g.Grade, Category: g.Category, Reason: g.Reason, Headline: gateHeadline(c.CommentBody)})
		if !apply {
			continue
		}
		v := c.Verification
		if v == nil {
			v = &types.Verification{Status: types.VerifySkipped}
		}
		v.Gate, v.GateCategory, v.GateReason, v.GateApplied = g.Grade, g.Category, g.Reason, true
		if g.Grade == grader.GradeBad {
			if strings.EqualFold(strings.TrimSpace(c.Importance), "CRITICAL") {
				v.GateDemoted = true
				c.Importance = "LOW"
				rep.Demoted++
			} else {
				rep.Dropped++
			}
		}
		c.Verification = v
		comments[index[k]] = c
	}
	rep.Status = "applied"
	if !apply {
		rep.Status = "recorded"
	}
	logGate(in, *rep, logged)
}

// gatePriors turns the "already raised" block into the grader's earlier
// points, and returns the fingerprints already on the ledger so an item that
// is one of them is not graded a duplicate of itself.
func gatePriors(priors []service.PriorFinding) ([]grader.Prior, map[string]bool) {
	out := make([]grader.Prior, 0, len(priors))
	posted := make(map[string]bool, len(priors))
	for _, pf := range priors {
		if pf.SettledOnPR == 0 {
			posted[pf.ID] = true
		}
		g := grader.Prior{State: pf.State, File: pf.File, Headline: pf.Headline, Verdicts: pf.Verdicts}
		if pf.LineFrom > 0 {
			g.Lines = fmt.Sprintf("%d-%d", pf.LineFrom, pf.LineTo)
		}
		if pf.SettledOnPR != 0 {
			g.State = fmt.Sprintf("settled on #%d", pf.SettledOnPR)
			if pf.Reason != "" {
				g.Verdicts = append(g.Verdicts, "author: "+pf.Reason)
			}
		}
		out = append(out, g)
	}
	return out, posted
}

func gateHeadline(body string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(body), "\n", 2)[0])
	if r := []rune(line); len(r) > 140 {
		return string(r[:140])
	}
	return line
}

func logGate(in service.GradeAgentInput, rep GateReport, items []gateItem) {
	body, _ := json.Marshal(struct {
		GateReport
		Items []gateItem `json:"items,omitempty"`
	}{rep, items})
	log.Printf("[GATE %s/%s#%d] %s", in.Owner, in.Repo, in.PRNumber, body)
}
