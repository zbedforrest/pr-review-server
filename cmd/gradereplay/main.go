// Command gradereplay runs the daily grader's agent over a fixed set of
// comments, so a prompt or model change can be scored against a graded
// answer key before it ships.
//
// Input is a JSON array of batches, one per PR head:
//
//	[{"owner":"o","repo":"r","pr":1,"head":"<sha>","base":"<sha>","title":"...","body":"...","items":[grader.Item...]}]
//
// Each batch's result is written to <out>/<owner>__<repo>__<pr>__<head7>.json.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"pr-review-server/pkg/grader"
	"pr-review-server/pkg/reviewer/service"
)

type batch struct {
	Owner string        `json:"owner"`
	Repo  string        `json:"repo"`
	PR    int           `json:"pr"`
	Head  string        `json:"head"`
	Base  string        `json:"base"`
	Title string        `json:"title"`
	Body  string        `json:"body"`
	Items []grader.Item `json:"items"`
}

type result struct {
	Owner   string         `json:"owner"`
	Repo    string         `json:"repo"`
	PR      int            `json:"pr"`
	Grades  []grader.Grade `json:"grades"`
	CostUSD float64        `json:"cost_usd"`
	Error   string         `json:"error,omitempty"`
}

func main() {
	in := flag.String("in", "", "batches JSON file")
	out := flag.String("out", "", "output directory")
	clones := flag.String("clones", "", "agent clone root holding .cache/owner__repo")
	model := flag.String("model", "claude-fable-5-1", "grader model")
	turns := flag.Int("turns", 120, "agent turn budget")
	wall := flag.Duration("wall", 15*time.Minute, "agent wall clock")
	par := flag.Int("par", 4, "concurrent batches")
	maxUSD := flag.Float64("max-usd", 100, "stop starting new batches once this much is spent")
	flag.Parse()
	if *in == "" || *out == "" || *clones == "" {
		log.Fatal("usage: gradereplay -in batches.json -out dir -clones root")
	}
	raw, err := os.ReadFile(*in)
	if err != nil {
		log.Fatal(err)
	}
	var batches []batch
	if err := json.Unmarshal(raw, &batches); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	cfg := service.AgentConfig{
		CloneRootDir: *clones, LogsDir: filepath.Join(*out, "agent-logs"), WallClock: *wall, MaxTurns: *turns,
		Backend: service.AgentBackendClaude, Model: *model,
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"), GitHubToken: os.Getenv("GITHUB_TOKEN"),
	}

	var mu sync.Mutex
	spent := 0.0
	sem := make(chan struct{}, max(1, *par))
	var wg sync.WaitGroup
	for _, b := range batches {
		path := filepath.Join(*out, fmt.Sprintf("%s__%s__%d__%.7s.json", b.Owner, b.Repo, b.PR, b.Head))
		if _, err := os.Stat(path); err == nil {
			continue
		}
		mu.Lock()
		over := spent >= *maxUSD
		mu.Unlock()
		if over {
			log.Printf("budget $%.2f reached; stopping", *maxUSD)
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(b batch, path string) {
			defer func() { <-sem; wg.Done() }()
			res, err := service.RunAgentGrade(context.Background(), cfg, service.DefaultSpawner{}, service.GradeAgentInput{
				Owner: b.Owner, Repo: b.Repo, PRNumber: b.PR, HeadSHA: b.Head, BaseSHA: b.Base, Title: b.Title, Body: b.Body, Items: b.Items,
			})
			r := result{Owner: b.Owner, Repo: b.Repo, PR: b.PR}
			if res != nil {
				r.Grades, r.CostUSD = res.Grades, res.CostUSD
			}
			if err != nil {
				r.Error = err.Error()
			}
			mu.Lock()
			spent += r.CostUSD
			mu.Unlock()
			log.Printf("%s/%s#%d grades=%d cost=$%.2f err=%s", b.Owner, b.Repo, b.PR, len(r.Grades), r.CostUSD, r.Error)
			if err != nil {
				return
			}
			body, _ := json.MarshalIndent(r, "", " ")
			if werr := os.WriteFile(path, body, 0o644); werr != nil {
				log.Printf("write %s: %v", path, werr)
			}
		}(b, path)
	}
	wg.Wait()
	log.Printf("spent $%.2f", spent)
}
