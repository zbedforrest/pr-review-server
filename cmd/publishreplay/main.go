// publishreplay replays historical PRism publication rounds through the real
// publisher code (poller.BuildPublishRound and publisher.Publish) against an
// in-memory ledger and a recording fake GitHub, and reports the repost and
// resolution metrics the comment-system program tracks.
//
// Usage:
//
//	publishreplay --dumps <dir> --sidecars <dir> [--limit N] [--pr repo#number] [--csv out.csv] [--out metrics.json] [--offline] [--legacy]
//
// Dumps are the GitHub GraphQL exports (one PR per file). Sidecars are the
// review findings JSON per round, fetched from PRISM_BASE_URL/reviews/ with
// PRISM_TOKEN or the gh CLI token when the cache lacks them; compares between
// round heads come from gh api and are cached under <sidecars>/compare/.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"pr-review-server/internal/replaykit"
	"pr-review-server/pkg/publisher"
)

func main() {
	dumpsDir := flag.String("dumps", "", "directory of PR dumps (*.json)")
	sidecarsDir := flag.String("sidecars", "", "sidecar cache directory")
	limit := flag.Int("limit", 0, "replay at most this many PRs (0 = all)")
	prFilter := flag.String("pr", "", "replay one PR, as repo#number or owner/repo#number")
	csvPath := flag.String("csv", "", "write the per-PR CSV here")
	outPath := flag.String("out", "", "write the metrics JSON here as well as to stdout")
	offline := flag.Bool("offline", false, "never fetch; missing sidecars or compares count as missing")
	bot := flag.String("bot", "", "bot login that posted the rounds (default: detected from finding markers)")
	workers := flag.Int("workers", 6, "parallel fetches during prefetch")
	inlineCap := flag.Int("inline-cap", publisher.DefaultInlineCap, "publisher inline cap per round")
	minSeverity := flag.String("inline-min-severity", publisher.DefaultInlineMinSeverity, "publisher inline minimum severity")
	legacy := flag.Bool("legacy", false, "replay with the pre-ledger-memory publisher (PUBLISH_POLICY_V2=false) for a baseline")
	textOnly := flag.Bool("alias-text-only", false, "count same-defect reposts by raw-text overlap alone, without the subject key")
	sameCommitGuard := flag.Bool("same-commit-guard", true, "refuse a round whose head was already published (PUBLISH_SAME_COMMIT_GUARD)")
	legacyTitles := flag.Bool("legacy-titles", false, "render titles and bullets the pre-2026-10 way (PUBLISH_RENDER_V2=false)")
	showUnverified := flag.Bool("show-unverified", publisher.DefaultPolicy().ShowUnverified, "fold unverified first-pass claims into the summary")
	flag.Parse()
	if *dumpsDir == "" || *sidecarsDir == "" {
		fmt.Fprintln(os.Stderr, "usage: publishreplay --dumps <dir> --sidecars <dir> [--limit N] [--pr repo#number] [--csv out.csv]")
		os.Exit(2)
	}
	log.SetFlags(0)

	dumps, err := replaykit.LoadDumps(*dumpsDir)
	if err != nil {
		log.Fatalf("load dumps: %v", err)
	}
	dumps, err = filterDumps(dumps, *prFilter, *limit)
	if err != nil {
		log.Fatal(err)
	}

	var f replaykit.Fetcher
	if !*offline {
		lf, err := replaykit.NewLiveFetcher(os.Getenv("PRISM_BASE_URL"))
		if err != nil {
			log.Fatalf("fetcher: %v (pass --offline to replay the cache only)", err)
		}
		f = lf
	}
	st, err := replaykit.NewStore(*sidecarsDir, f)
	if err != nil {
		log.Fatalf("sidecar store: %v", err)
	}
	opts := Options{Dumps: dumps, Store: st, BotName: *bot, Logf: log.Printf, TextOnlyAlias: *textOnly,
		Policy: replayPolicy(*inlineCap, *minSeverity, *showUnverified, *legacy)}
	opts.Policy.RepublishSameCommit = !*sameCommitGuard
	opts.Policy.LegacyTitles = *legacyTitles
	if f != nil {
		if err := replaykit.Prefetch(st, dumps, opts.bots, *workers, log.Printf); err != nil {
			log.Printf("prefetch finished with errors; the replay retries each fetch once more and counts what still fails as missing: %v", err)
		}
	}

	res, err := Run(context.Background(), opts)
	if err != nil {
		log.Fatalf("replay: %v", err)
	}
	if *csvPath != "" {
		fh, err := os.Create(*csvPath)
		if err != nil {
			log.Fatalf("csv: %v", err)
		}
		if err := writeCSV(fh, res.PerPR); err != nil {
			log.Fatalf("csv: %v", err)
		}
		_ = fh.Close()
	}
	out, _ := json.MarshalIndent(res.Metrics, "", "  ")
	if *outPath != "" {
		if err := os.WriteFile(*outPath, append(out, '\n'), 0o644); err != nil {
			log.Fatalf("out: %v", err)
		}
	}
	fmt.Println(string(out))
}

// replayPolicy starts from the shipped defaults so the replay and the tests
// run the publisher the way prod does unless a flag says otherwise.
func replayPolicy(inlineCap int, minSeverity string, showUnverified, legacy bool) publisher.Policy {
	p := publisher.DefaultPolicy()
	p.InlineCap = inlineCap
	p.InlineMinSeverity = minSeverity
	p.ShowUnverified = showUnverified
	p.LegacyLedger = legacy
	return p
}

func filterDumps(dumps []*replaykit.PRDump, prFilter string, limit int) ([]*replaykit.PRDump, error) {
	if prFilter != "" {
		owner, want := "", prFilter
		if i := strings.Index(want, "/"); i >= 0 {
			owner, want = want[:i], want[i+1:]
		}
		var kept []*replaykit.PRDump
		for _, d := range dumps {
			if d.Key() == want && (owner == "" || strings.EqualFold(d.Owner, owner)) {
				kept = append(kept, d)
			}
		}
		if len(kept) == 0 {
			return nil, fmt.Errorf("no dump matches --pr %q", prFilter)
		}
		if owner == "" && len(kept) > 1 {
			return nil, fmt.Errorf("--pr %q matches %d dumps; qualify it with the owner", prFilter, len(kept))
		}
		dumps = kept
	}
	if limit > 0 && len(dumps) > limit {
		dumps = dumps[:limit]
	}
	return dumps, nil
}
