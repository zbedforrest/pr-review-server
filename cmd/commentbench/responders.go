package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/publisher/replytext"
	"pr-review-server/pkg/reviewer/service"
)

// stubResponder replays the reply model deterministically: the decision the
// reply ledger recorded for that author comment when there is one, else the
// class rule the recorded decisions follow (a fix claim is conceded, a
// question answered, an intent claim conceded, other pushback held with a
// cite). It never calls a model.
func stubResponder(c *Case, run *Run) publisher.Responder {
	return func(_ context.Context, req publisher.ReplyRequest) (publisher.ReplyDecision, error) {
		if rec, ok := c.Recorded[req.Reply.CommentID]; ok {
			if rec.BudgetExhausted {
				return publisher.ReplyDecision{Model: "stub"}, fmt.Errorf("%w: recorded", publisher.ErrBudgetExhausted)
			}
			body := rec.Reply
			if body == "" && rec.Decision != publisher.DecisionAbstain {
				body = cannedReply(rec.Decision, req)
			}
			return publisher.ReplyDecision{Decision: rec.Decision, Reply: body, Cited: rec.Cited, React: rec.React, Model: "stub"}, nil
		}
		run.Synthetic++
		decision := publisher.DecisionHold
		switch {
		case req.Reply.Class == publisher.ReplyQuestion:
			decision = publisher.DecisionAnswer
		case req.Reply.Class == publisher.ReplyResolution, replytext.AssertsIntent(req.Reply.Body):
			decision = publisher.DecisionConcede
		}
		file, line := citeOf(req.Root.Body)
		return publisher.ReplyDecision{Decision: decision, Reply: cannedReply(decision, req), Cited: []publisher.EvidenceRef{{File: file, Line: line}},
			React: decision != publisher.DecisionHold, Model: "stub"}, nil
	}
}

func citeOf(rootBody string) (string, int) {
	id, ok := publisher.FindingIDFromBody(rootBody)
	if !ok {
		return "", 0
	}
	parts := strings.Split(id, ":")
	if len(parts) < 3 {
		return id, 0
	}
	bucket, _ := strconv.Atoi(parts[len(parts)-2])
	return strings.Join(parts[:len(parts)-2], ":"), bucket * 10
}

func cannedReply(decision string, req publisher.ReplyRequest) string {
	file, line := citeOf(req.Root.Body)
	switch decision {
	case publisher.DecisionConcede:
		return fmt.Sprintf("Checked %s:%d at this head; the finding no longer applies.", file, line)
	case publisher.DecisionAnswer:
		return fmt.Sprintf("The behaviour comes from %s:%d.", file, line)
	}
	return fmt.Sprintf("%s:%d still shows the behaviour the finding describes.", file, line)
}

// liveResponder runs the real reply agent (service.RunAgentReply) against a
// fresh clone of each PR head: one billed agent run per eligible reply, up to
// the reply budget (REPLY_MAX_TURNS, REPLY_WALL_CLOCK_SEC), with read access
// to the case's repository.
func liveResponder(cfg service.AgentConfig) func(*Case, *Run) publisher.Responder {
	return func(c *Case, run *Run) publisher.Responder {
		run.Live = true
		return func(ctx context.Context, req publisher.ReplyRequest) (publisher.ReplyDecision, error) {
			ourID := req.Root.AuthorID
			thread := make([]service.ReplyMessage, 0, len(req.Thread))
			for _, m := range req.Thread {
				thread = append(thread, service.ReplyMessage{Author: m.Author, Ours: m.AuthorID == ourID, Body: m.Body, At: m.CreatedAt})
			}
			in := service.ReplyInput{Owner: req.Owner, Repo: req.Repo, DefaultBranch: req.BaseRef, PRNumber: req.Number, HeadSHA: req.HeadSHA,
				Fingerprint: req.Fingerprint, FindingBody: req.Root.Body, Thread: thread, AuthorReply: req.Reply.Body, Class: string(req.Reply.Class)}
			started := time.Now()
			out, err := service.RunAgentReply(ctx, cfg, service.DefaultSpawner{}, in)
			if errors.Is(err, service.ErrReplyBudgetExhausted) {
				return publisher.ReplyDecision{Model: cfg.Model, DurationMS: time.Since(started).Milliseconds()}, fmt.Errorf("%w: %v", publisher.ErrBudgetExhausted, err)
			}
			if err != nil {
				return publisher.ReplyDecision{}, err
			}
			cited := make([]publisher.EvidenceRef, 0, len(out.Cited))
			for _, e := range out.Cited {
				cited = append(cited, publisher.EvidenceRef{File: e.File, Line: e.Line})
			}
			return publisher.ReplyDecision{Decision: out.Decision, Reply: out.Reply, Cited: cited, React: out.React, Model: out.ServedModel, DurationMS: out.DurationMS}, nil
		}
	}
}

// liveAgentConfig reads the reply agent settings the server uses from the
// environment, with the server's defaults.
func liveAgentConfig(scratch string) (service.AgentConfig, error) {
	token, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return service.AgentConfig{}, fmt.Errorf("gh auth token: %w", err)
	}
	envInt := func(k string, def int) int {
		if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
			return v
		}
		return def
	}
	model := os.Getenv("REPLY_MODEL")
	if model == "" {
		model = os.Getenv("AGENT_MODEL")
	}
	return service.AgentConfig{
		CloneRootDir: scratch + "/clones", LogsDir: scratch + "/logs",
		WallClock: time.Duration(envInt("REPLY_WALL_CLOCK_SEC", 180)) * time.Second, MaxTurns: envInt("REPLY_MAX_TURNS", 20),
		GitHubToken: strings.TrimSpace(string(token)), Backend: os.Getenv("AGENT_BACKEND"), Model: model, Effort: os.Getenv("AGENT_EFFORT"),
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"), OpenRouterAPIKey: os.Getenv("OPENROUTER_API_KEY"), OpenRouterBaseURL: os.Getenv("OPENROUTER_BASE_URL"),
	}, nil
}
