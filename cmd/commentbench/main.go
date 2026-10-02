// commentbench scores PRism's commenting machinery against what it should
// have done. `build` turns the comment-system audit and the PR dumps into
// self-contained cases; `run` replays every case through the real publisher
// and reply reactor (in-memory ledger, recording fake GitHub) and scores
// posts, suppressions, reposts, "fixed" claims, summary counts, thread
// resolutions and reply decisions, per tier.
//
// Usage:
//
//	commentbench build --audit <workflow_result.json> [--labels <extra.json>] --dumps <dir> --sidecars <dir> [--reply-ledger <json>] --out <dir>
//	commentbench run --cases <dir> [--replies stub|live] [--json out.json] [--md out.md] [--min-gold-... thresholds]
//
// The case directory may also come from COMMENTBENCH_DIR. Cases name real
// repositories; keep them outside the repository.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pr-review-server/internal/replaykit"
	"pr-review-server/pkg/publisher"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: commentbench build|run [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "build":
		os.Exit(cmdBuild(os.Args[2:]))
	case "run":
		os.Exit(cmdRun(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q; want build or run\n", os.Args[1])
		os.Exit(2)
	}
}

func cmdBuild(args []string) int {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	auditPath := fs.String("audit", "", "audit workflow_result.json")
	var extra stringList
	fs.Var(&extra, "labels", "extra label file with perPr and verified entries in the audit's shape (repeatable)")
	dumps := fs.String("dumps", "", "directory of PR dumps")
	sidecars := fs.String("sidecars", "", "sidecar cache directory (the replay harness cache)")
	ledgerPath := fs.String("reply-ledger", "", "reply ledger export with recorded reply decisions (optional)")
	out := fs.String("out", os.Getenv("COMMENTBENCH_DIR"), "output directory for cases and manifest.json")
	offline := fs.Bool("offline", false, "never fetch missing sidecars or compares")
	_ = fs.Parse(args)
	if *auditPath == "" || *dumps == "" || *sidecars == "" || *out == "" {
		fs.Usage()
		return 2
	}
	a, err := loadAudit(*auditPath)
	if err != nil {
		log.Print(err)
		return 1
	}
	for _, p := range extra {
		more, err := loadAudit(p)
		if err != nil {
			log.Print(err)
			return 1
		}
		a.PerPR = append(a.PerPR, more.PerPR...)
		a.Verified = append(a.Verified, more.Verified...)
	}
	var f replaykit.Fetcher
	if !*offline {
		if lf, err := replaykit.NewLiveFetcher(os.Getenv("PRISM_BASE_URL")); err == nil {
			f = lf
		} else {
			log.Printf("fetcher: %v; building from the cache only", err)
		}
	}
	st, err := replaykit.NewStore(*sidecars, f)
	if err != nil {
		log.Print(err)
		return 1
	}
	o := BuildOptions{Audit: a, DumpsDir: *dumps, Store: st, Logf: log.Printf}
	if *ledgerPath != "" {
		if o.ReplyLedger, err = loadReplyLedger(*ledgerPath); err != nil {
			log.Printf("reply ledger: %v", err)
			return 1
		}
	}
	m, err := buildAll(o, *out)
	if err != nil {
		log.Print(err)
		return 1
	}
	fmt.Printf("cases: %d gold, %d accepted, %d excluded; manifest %s\n", m.Counts[TierGold], m.Counts[TierAccepted], m.Counts[TierExcluded], filepath.Join(*out, "manifest.json"))
	return 0
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// Thresholds gate a run on the gold tier; a negative value disables one.
type Thresholds struct {
	MinPostPrecision     float64
	MinSuppressionRecall float64
	MaxReposts           int
	MaxWrongFixed        int
	MinSummaryCorrect    float64
	MinReplyAccuracy     float64
	MinResolutionRecall  float64
}

// Result is the JSON the run writes.
type Result struct {
	Generated time.Time              `json:"generated"`
	Replies   string                 `json:"replies_mode"`
	Tiers     map[string]TierMetrics `json:"tiers"`
	Failures  []string               `json:"threshold_failures,omitempty"`
	Cases     []CaseScore            `json:"cases"`
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dir := fs.String("cases", os.Getenv("COMMENTBENCH_DIR"), "dataset directory (cases/ and manifest.json), or COMMENTBENCH_DIR")
	replies := fs.String("replies", "stub", "reply model: stub replays recorded decisions, live runs the reply agent")
	only := fs.String("case", "", "run only the case with this id")
	jsonOut := fs.String("json", "", "write the result JSON here")
	mdOut := fs.String("md", "", "write the Markdown report here")
	anon := fs.Bool("anonymise", false, "print case ids as case-N in reports")
	inlineCap := fs.Int("inline-cap", publisher.DefaultInlineCap, "publisher inline cap per round")
	minSeverity := fs.String("inline-min-severity", publisher.DefaultInlineMinSeverity, "publisher inline minimum severity")
	var th Thresholds
	fs.Float64Var(&th.MinPostPrecision, "min-gold-post-precision", -1, "fail below this gold post precision")
	fs.Float64Var(&th.MinSuppressionRecall, "min-gold-suppression-recall", -1, "fail below this gold suppression recall")
	fs.IntVar(&th.MaxReposts, "max-gold-reposts", -1, "fail above this many gold reposts")
	fs.IntVar(&th.MaxWrongFixed, "max-gold-wrong-fixed", -1, "fail above this many gold wrong fixed")
	fs.Float64Var(&th.MinSummaryCorrect, "min-gold-summary", -1, "fail below this gold summary-count correctness")
	fs.Float64Var(&th.MinReplyAccuracy, "min-gold-reply-accuracy", -1, "fail below this gold reply decision accuracy")
	fs.Float64Var(&th.MinResolutionRecall, "min-gold-resolution-recall", -1, "fail below this gold thread-resolution recall (ignored while not applicable)")
	_ = fs.Parse(args)
	if *dir == "" {
		fs.Usage()
		return 2
	}
	cases, err := loadCases(*dir)
	if err != nil {
		log.Print(err)
		return 1
	}
	var responder func(*Case, *Run) publisher.Responder
	switch *replies {
	case "stub":
		responder = stubResponder
	case "live":
		scratch, err := os.MkdirTemp("", "commentbench-live-")
		if err != nil {
			log.Print(err)
			return 1
		}
		cfg, err := liveAgentConfig(scratch)
		if err != nil {
			log.Print(err)
			return 1
		}
		responder = liveResponder(cfg)
	default:
		log.Printf("--replies must be stub or live")
		return 2
	}
	policy := publisher.Policy{InlineCap: *inlineCap, InlineMinSeverity: *minSeverity, ShowUnverified: true}
	res, err := runCases(context.Background(), cases, *only, policy, responder)
	if err != nil {
		log.Print(err)
		return 1
	}
	res.Replies = *replies
	res.Failures = th.check(res.Tiers[TierGold])
	if *anon {
		for i := range res.Cases {
			res.Cases[i].ID = fmt.Sprintf("case-%d", i+1)
		}
	}
	raw, _ := json.MarshalIndent(res, "", "  ")
	if *jsonOut != "" {
		if err := os.WriteFile(*jsonOut, append(raw, '\n'), 0o644); err != nil {
			log.Print(err)
			return 1
		}
	}
	md := renderMarkdown(res)
	if *mdOut != "" {
		if err := os.WriteFile(*mdOut, []byte(md), 0o644); err != nil {
			log.Print(err)
			return 1
		}
	}
	fmt.Print(metricsTable(res))
	if len(res.Failures) > 0 {
		fmt.Fprintln(os.Stderr, "gold thresholds failed: "+strings.Join(res.Failures, "; "))
		return 1
	}
	return 0
}

// runCases replays and scores every non-excluded case (or the one named).
func runCases(ctx context.Context, cases []*Case, only string, policy publisher.Policy, responder func(*Case, *Run) publisher.Responder) (Result, error) {
	res := Result{Generated: time.Now().UTC(), Tiers: map[string]TierMetrics{}}
	byTier := map[string][]CaseScore{}
	for _, c := range cases {
		if only != "" && c.ID != only {
			continue
		}
		if c.Tier == TierExcluded {
			continue
		}
		run, err := replayCase(ctx, c, policy, responder)
		if err != nil {
			return res, fmt.Errorf("%s: %w", c.ID, err)
		}
		s := scoreCase(run)
		res.Cases = append(res.Cases, s)
		byTier[c.Tier] = append(byTier[c.Tier], s)
	}
	for _, tier := range []string{TierGold, TierAccepted} {
		res.Tiers[tier] = aggregate(byTier[tier])
	}
	return res, nil
}

func (th Thresholds) check(g TierMetrics) []string {
	var out []string
	below := func(name string, v *float64, min float64) {
		if min >= 0 && v != nil && *v < min {
			out = append(out, fmt.Sprintf("%s %.3f < %.3f", name, *v, min))
		}
	}
	below("post_precision", g.PostPrecision, th.MinPostPrecision)
	below("suppression_recall", g.SuppressionRecall, th.MinSuppressionRecall)
	below("summary_count_correct", g.SummaryCorrect, th.MinSummaryCorrect)
	below("reply_accuracy", g.ReplyAccuracy, th.MinReplyAccuracy)
	below("thread_resolution_recall", g.ResolutionRecall, th.MinResolutionRecall)
	if th.MaxReposts >= 0 && g.Reposts > th.MaxReposts {
		out = append(out, fmt.Sprintf("reposts %d > %d", g.Reposts, th.MaxReposts))
	}
	if th.MaxWrongFixed >= 0 && g.WrongFixed > th.MaxWrongFixed {
		out = append(out, fmt.Sprintf("wrong_fixed %d > %d", g.WrongFixed, th.MaxWrongFixed))
	}
	return out
}
