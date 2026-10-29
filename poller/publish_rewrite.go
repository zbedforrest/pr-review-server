package poller

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
	"pr-review-server/pkg/rewrite"
)

// RewriteReport is what the publish rewrite did on one review.
type RewriteReport struct {
	Mode       string  `json:"mode"`
	Status     string  `json:"status"`
	Skip       string  `json:"skip,omitempty"`
	Items      int     `json:"items"`
	Rewritten  int     `json:"rewritten"`
	CostUSD    float64 `json:"cost_usd"`
	DurationMS int64   `json:"duration_ms"`
	Error      string  `json:"error,omitempty"`
}

// rewriteItem is one rewrite as logged, with the title it replaces.
type rewriteItem struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Before   string `json:"before"`
	Title    string `json:"title"`
	Body     string `json:"body"`
}

var (
	rewriteRunFn      = service.RunRewrite
	rewriteBackground = make(chan struct{}, 4)
	rewriteSpendMu    sync.Mutex
	rewriteSpendDay   string
	rewriteSpend      float64
)

func rewriteSpent(day string, add float64) float64 {
	rewriteSpendMu.Lock()
	defer rewriteSpendMu.Unlock()
	if rewriteSpendDay != day {
		rewriteSpendDay, rewriteSpend = day, 0
	}
	rewriteSpend += add
	return rewriteSpend
}

// rewriteTimeoutAllowance is the time on mode adds to an ensemble review.
func (p *Poller) rewriteTimeoutAllowance() time.Duration {
	if p.cfg == nil || p.cfg.PublishRewrite != gateModeOn {
		return 0
	}
	return time.Duration(p.cfg.PublishRewriteWallClockSec) * time.Second
}

// rewriteReview rewrites every finding the Lite ensemble (or its fallback)
// is about to post inline (MEDIUM and CRITICAL, not held by the verifier or
// the gate); findings merged in after this stage keep the default form
// into a short title and a direct body. On mode sets the finding's Display;
// shadow mode only logs. Any failure leaves the review as it was, and every
// outcome logs a [REWRITE] line.
func (p *Poller) rewriteReview(ctx context.Context, execution *reviewExecution, base service.AgentConfig, review *service.AgentReview) *RewriteReport {
	if p.cfg == nil || (p.cfg.PublishRewrite != gateModeShadow && p.cfg.PublishRewrite != gateModeOn) {
		return nil
	}
	job := execution.Job
	in := service.RewriteInput{Owner: job.PR.Owner, Repo: job.PR.Repo, PRNumber: job.PR.Number}
	rep := &RewriteReport{Mode: p.cfg.PublishRewrite}
	skip := func(why string) *RewriteReport {
		rep.Status, rep.Skip = "skipped", why
		logRewrite(in, *rep, nil)
		return rep
	}
	if !p.reviewPublishBound(job) {
		return skip("not_publish_bound")
	}
	if p.cfg.PublishLegacyTitles {
		return skip("legacy_titles")
	}
	var index []int
	for i, c := range review.Comments {
		if c.FilePath == "SUMMARY" || c.FilePath == "CHECK" || c.Inactive || c.Verification.Suppresses() ||
			strings.EqualFold(strings.TrimSpace(c.Importance), "LOW") {
			continue
		}
		in.Items = append(in.Items, rewriteInputItem("F"+strconv.Itoa(len(in.Items)+1), c))
		index = append(index, i)
	}
	if len(in.Items) == 0 {
		return skip("no_inline_findings")
	}
	rep.Items = len(in.Items)
	day := time.Now().UTC().Format("2006-01-02")
	if p.cfg.PublishRewriteBudgetUSD > 0 && rewriteSpent(day, 0) >= p.cfg.PublishRewriteBudgetUSD {
		return skip("over_budget")
	}
	cfg := p.verifyAgentConfig(ctx, base.CloneRootDir)
	cfg.Backend, cfg.Model = service.AgentBackendClaude, p.cfg.PublishRewriteModel
	cfg.WallClock = time.Duration(p.cfg.PublishRewriteWallClockSec) * time.Second
	cfg.MaxTurns = 3

	if rep.Mode == gateModeShadow {
		r := *rep
		comments := append([]types.LineComment(nil), review.Comments...)
		run := rewriteRunFn
		select {
		case rewriteBackground <- struct{}{}:
			go func() {
				defer func() { <-rewriteBackground }()
				rctx, cancel := context.WithTimeout(context.Background(), cfg.WallClock)
				defer cancel()
				p.runRewrite(rctx, run, cfg, in, comments, index, &r, day, false)
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
	p.runRewrite(ctx, rewriteRunFn, cfg, in, review.Comments, index, rep, day, true)
	return rep
}

func rewriteInputItem(id string, c types.LineComment) rewrite.Item {
	it := rewrite.Item{ID: id, Severity: strings.ToLower(c.Importance), Explanation: c.CommentBody}
	if fc := c.FindingContract; fc != nil {
		it.Headline, it.Impact, it.Caveat = fc.Headline, fc.CurrentImpact, fc.Uncertainty
	}
	return it
}

// runRewrite runs the model and, when apply is set, records each rewrite on
// its finding. Shadow runs pass copies and never apply, so they only log.
func (p *Poller) runRewrite(ctx context.Context, run func(context.Context, service.AgentConfig, service.Spawner, service.RewriteInput) (*service.RewriteResult, error), cfg service.AgentConfig, in service.RewriteInput, comments []types.LineComment, index []int, rep *RewriteReport, day string, apply bool) {
	started := time.Now()
	res, err := run(ctx, cfg, p.agentSpawner, in)
	if res != nil {
		rep.CostUSD = res.CostUSD
		rewriteSpent(day, res.CostUSD)
	}
	rep.DurationMS = time.Since(started).Milliseconds()
	if err != nil {
		rep.Status, rep.Error = "error", err.Error()
		logRewrite(in, *rep, nil)
		return
	}
	byID := make(map[string]rewrite.Result, len(res.Rewrites))
	for _, r := range res.Rewrites {
		byID[r.ID] = r
	}
	var logged []rewriteItem
	for k, it := range in.Items {
		r, ok := byID[it.ID]
		if !ok {
			continue
		}
		rep.Rewritten++
		c := comments[index[k]]
		logged = append(logged, rewriteItem{File: c.FilePath, Line: c.LineNumber, Severity: c.Importance, Before: gateHeadline(it.Headline + "\n" + c.CommentBody), Title: r.Title, Body: r.Body})
		if apply {
			comments[index[k]].Display = &types.Display{Title: r.Title, Body: r.Body}
		}
	}
	rep.Status = "applied"
	if !apply {
		rep.Status = "recorded"
	}
	logRewrite(in, *rep, logged)
}

func logRewrite(in service.RewriteInput, rep RewriteReport, items []rewriteItem) {
	body, _ := json.Marshal(struct {
		RewriteReport
		Items []rewriteItem `json:"items,omitempty"`
	}{rep, items})
	log.Printf("[REWRITE %s/%s#%d] %s", in.Owner, in.Repo, in.PRNumber, body)
}
