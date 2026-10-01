// publishreplay replays historical PRism publication rounds through the real
// publisher code (poller.BuildPublishRound and publisher.Publish) against an
// in-memory ledger and a recording fake GitHub, and reports the repost and
// resolution metrics the comment-system program tracks.
//
// Usage:
//
//	publishreplay --dumps <dir> --sidecars <dir> [--limit N] [--pr repo#number] [--csv out.csv] [--out metrics.json] [--offline]
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
	flag.Parse()
	if *dumpsDir == "" || *sidecarsDir == "" {
		fmt.Fprintln(os.Stderr, "usage: publishreplay --dumps <dir> --sidecars <dir> [--limit N] [--pr repo#number] [--csv out.csv]")
		os.Exit(2)
	}
	log.SetFlags(0)

	dumps, err := loadDumps(*dumpsDir)
	if err != nil {
		log.Fatalf("load dumps: %v", err)
	}
	dumps = filterDumps(dumps, *prFilter, *limit)

	var f fetcher
	if !*offline {
		lf, err := newLiveFetcher(os.Getenv("PRISM_BASE_URL"))
		if err != nil {
			log.Fatalf("fetcher: %v (use --offline to replay the cache only)", err)
		}
		f = lf
	}
	st, err := newStore(*sidecarsDir, f)
	if err != nil {
		log.Fatalf("sidecar store: %v", err)
	}
	opts := Options{Dumps: dumps, Store: st, BotName: *bot, Logf: log.Printf,
		Policy: publisher.Policy{InlineCap: *inlineCap, InlineMinSeverity: *minSeverity, ShowUnverified: true}}
	if f != nil {
		if err := prefetch(st, dumps, opts.bots, *workers, log.Printf); err != nil {
			log.Printf("prefetch finished with errors; affected rounds count as missing: %v", err)
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

func filterDumps(dumps []*prDump, prFilter string, limit int) []*prDump {
	if prFilter != "" {
		want := prFilter
		if i := strings.Index(want, "/"); i >= 0 {
			want = want[i+1:]
		}
		var kept []*prDump
		for _, d := range dumps {
			if d.key() == want {
				kept = append(kept, d)
			}
		}
		dumps = kept
	}
	if limit > 0 && len(dumps) > limit {
		dumps = dumps[:limit]
	}
	return dumps
}
