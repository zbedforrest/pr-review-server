package poller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync/atomic"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/grader"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/service"
)

var (
	ErrGraderDisabled = errors.New("grader disabled (GRADER_ENABLED=false)")
	ErrGraderBusy     = errors.New("a grading run is already in progress")
	ErrNotLeader      = errors.New("this instance does not hold the poller lease")
)

// GradeReport is what one daily grading run did.
type GradeReport struct {
	Candidates int `json:"candidates"`
	Selected   int `json:"selected"`
	Graded     int `json:"graded"`
	Skipped    int `json:"skipped"`
	Batches    int `json:"batches"`
	Failed     int `json:"failed"`
	// Missing counts items a judge reply left out; they stay ungraded and
	// are offered again on the next run.
	Missing int     `json:"missing"`
	CostUSD float64 `json:"cost_usd"`
	Stopped string  `json:"stopped,omitempty"`
}

type gradeStore interface {
	GradeCandidates(start, end time.Time) ([]db.GradeCandidate, error)
	SaveCommentGrades([]db.CommentGradeModel) error
}

var gradeAgentFn = service.RunAgentGrade

var gradeRunning atomic.Bool

// gradeLookback is how far back a run looks for ungraded comments, so a
// day over the cap or the budget is caught up later instead of lost.
const gradeLookback = 7 * 24 * time.Hour

// RunDailyGrade grades ungraded comments posted in the week before now,
// oldest first, one agent run per PR head, until the run budget is spent.
// Only the poller lease holder runs it, so a deploy overlap cannot pay to
// grade the same comments twice.
// onStart, when set, is called once the run has passed its checks and begun.
func (p *Poller) RunDailyGrade(ctx context.Context, now time.Time, onStart func()) (GradeReport, error) {
	var rep GradeReport
	if !p.cfg.GraderEnabled {
		return rep, ErrGraderDisabled
	}
	if !gradeRunning.CompareAndSwap(false, true) {
		return rep, ErrGraderBusy
	}
	defer gradeRunning.Store(false)
	if !p.holdsGradeLease() {
		return rep, ErrNotLeader
	}
	store, ok := p.db.(gradeStore)
	if !ok {
		return rep, errors.New("grade store unavailable")
	}
	if onStart != nil {
		onStart()
	}
	rows, err := store.GradeCandidates(now.Add(-gradeLookback), now)
	if err != nil {
		return rep, fmt.Errorf("grade candidates: %w", err)
	}
	rep.Candidates = len(rows)
	byID := make(map[string]db.GradeCandidate, len(rows))
	cands := make([]grader.Candidate, 0, len(rows))
	for i, r := range rows {
		id := "c" + strconv.Itoa(i+1)
		byID[id] = r
		item := grader.Item{ID: id, Kind: r.Kind, Line: r.Line, Severity: r.Severity, Comment: r.Text, AuthorComment: r.AuthorText}
		if r.Kind != grader.KindReply {
			item.File = payload.FingerprintFile(r.Fingerprint)
		}
		cands = append(cands, grader.Candidate{Ref: r.Ref, Owner: r.RepoOwner, Repo: r.RepoName, PRNumber: r.PRNumber,
			HeadSHA: r.HeadSHA, BaseSHA: r.MergeBase, CommentURL: commentURL(r), Item: item})
	}
	kept, dropped := grader.Select(cands, p.cfg.GraderMaxItems)
	rep.Selected, rep.Skipped = len(kept), dropped

	cfg := p.verifyAgentConfig(ctx, p.cfg.AgentCloneRootDir)
	cfg.Backend, cfg.Model = service.AgentBackendClaude, p.cfg.GraderModel
	cfg.WallClock = time.Duration(p.cfg.GraderWallClockSec) * time.Second
	cfg.MaxTurns = p.cfg.GraderMaxTurns

	batches := grader.ByPR(kept)
	for i, batch := range batches {
		if p.cfg.GraderDailyBudgetUSD > 0 && rep.CostUSD >= p.cfg.GraderDailyBudgetUSD {
			rep.Stopped = "budget"
			for _, b := range batches[i:] {
				rep.Skipped += len(b)
			}
			break
		}
		if ctx.Err() != nil {
			rep.Stopped = "cancelled"
			break
		}
		rep.Batches++
		graded, cost, err := p.gradeBatch(ctx, cfg, batch, byID, store, now)
		rep.CostUSD += cost
		rep.Graded += graded
		if err == nil {
			rep.Missing += len(batch) - graded
		}
		if err != nil {
			rep.Failed++
			log.Printf("[GRADE %s/%s#%d] %v", batch[0].Owner, batch[0].Repo, batch[0].PRNumber, err)
		}
	}
	log.Printf("[GRADE] candidates=%d selected=%d graded=%d missing=%d skipped=%d batches=%d failed=%d cost=$%.2f stopped=%s",
		rep.Candidates, rep.Selected, rep.Graded, rep.Missing, rep.Skipped, rep.Batches, rep.Failed, rep.CostUSD, rep.Stopped)
	return rep, nil
}

// holdsGradeLease checks the poller lease without failing open (unlike the
// poll loop, a wrong answer here costs a paid run) and without entering the
// election on a deployment that does not poll.
func (p *Poller) holdsGradeLease() bool {
	if p.holderID == "" || p.cfg.DisablePolling {
		return true
	}
	leader, err := p.db.TryAcquireOrRenewLeadership(p.holderID, p.generation, leaderLeaseTTL)
	if err != nil {
		log.Printf("[GRADE] lease check failed, not grading: %v", err)
		return false
	}
	return leader
}

func (p *Poller) gradeBatch(ctx context.Context, cfg service.AgentConfig, batch []grader.Candidate, byID map[string]db.GradeCandidate, store gradeStore, now time.Time) (int, float64, error) {
	first := batch[0]
	in := service.GradeAgentInput{Owner: first.Owner, Repo: first.Repo, PRNumber: first.PRNumber, HeadSHA: first.HeadSHA, BaseSHA: first.BaseSHA}
	if p.ghClient != nil {
		if pr, _, err := p.ghClient.GetPR(ctx, first.Owner, first.Repo, first.PRNumber); err == nil && pr != nil {
			in.Title, in.Body = pr.GetTitle(), pr.GetBody()
			in.DefaultBranch = pr.GetBase().GetRepo().GetDefaultBranch()
		}
	}
	urls := make(map[string]string, len(batch))
	for _, c := range batch {
		in.Items = append(in.Items, c.Item)
		urls[c.Item.ID] = c.CommentURL
	}
	if p.agentSlots != nil {
		select {
		case p.agentSlots <- struct{}{}:
			defer func() { <-p.agentSlots }()
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		}
	}
	res, err := gradeAgentFn(ctx, cfg, p.agentSpawner, in)
	cost := 0.0
	if res != nil {
		cost = res.CostUSD
	}
	if err != nil {
		return 0, cost, err
	}
	if len(res.Grades) == 0 {
		return 0, cost, nil
	}
	share := cost / float64(len(res.Grades))
	rows := make([]db.CommentGradeModel, 0, len(res.Grades))
	for _, g := range res.Grades {
		c := byID[g.ID]
		rows = append(rows, db.CommentGradeModel{
			Ref: c.Ref, RepoOwner: c.RepoOwner, RepoName: c.RepoName, PRNumber: c.PRNumber, Kind: c.Kind, Severity: c.Severity,
			HeadSHA: c.HeadSHA, CommentURL: urls[g.ID], PostedAt: c.PostedAt, Grade: g.Grade, Category: g.Category, Reason: g.Reason,
			Model: res.Model, CostUSD: share, GradedAt: now,
		})
	}
	if err := store.SaveCommentGrades(rows); err != nil {
		return 0, cost, fmt.Errorf("save grades: %w", err)
	}
	return len(rows), cost, nil
}

func commentURL(c db.GradeCandidate) string {
	base := fmt.Sprintf("https://github.com/%s/%s/pull/%d", c.RepoOwner, c.RepoName, c.PRNumber)
	switch {
	case c.Kind == grader.KindSummaryNote && c.SummaryCommentID != 0:
		return fmt.Sprintf("%s#issuecomment-%d", base, c.SummaryCommentID)
	case c.CommentID != 0:
		return fmt.Sprintf("%s#discussion_r%d", base, c.CommentID)
	}
	return base
}
