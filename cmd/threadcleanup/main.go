// threadcleanup lists the PRism review threads that should no longer be
// open: threads on merged or closed PRs, and threads on open PRs whose cited
// file changed after the comment was posted. Without --dry-run it resolves
// them through the same resolver the publisher uses, with a one-line note,
// gated by the publish gate and recorded in the ledger.
//
// Usage:
//
//	threadcleanup --dumps <dir> [--cache <dir>] [--offline] [--bot login] [--csv out.csv] [--out summary.json]
//	threadcleanup --pr owner/repo#N [--pr ...] [--bot login] [--verdicts list] [--dry-run=false]
//
// --dumps reads GitHub dumps (one PR per file) and is always a dry run: it
// lists and counts, with compares through gh cached under <cache>/compare/.
// --pr reads each PR live and, with --dry-run=false, closes the stale
// threads. Live writes should use the GITHUB_APP_* credentials so the note
// is PRism's: with GITHUB_TOKEN alone the note and the resolve carry the
// token owner's human account. Live mode also needs the ledger (DATABASE_URL
// or DB_PATH) for the publish gate and refuses to run while
// PUBLISH_THREAD_RESOLUTION is off.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/internal/replaykit"
	"pr-review-server/pkg/publisher"
	"pr-review-server/poller"
)

type prRef struct {
	owner, repo string
	number      int
}

func main() {
	dryRun := flag.Bool("dry-run", true, "list and count only; --dry-run=false resolves the stale threads")
	dumpsDir := flag.String("dumps", "", "directory of PR dumps (*.json); dry run only")
	cacheDir := flag.String("cache", "", "compare cache directory for --dumps (shared with publishreplay's --sidecars)")
	offline := flag.Bool("offline", false, "never run gh for a compare; an uncached compare counts as unknown")
	bot := flag.String("bot", "", "bot login that posted the threads (default: detected from finding markers)")
	csvPath := flag.String("csv", "", "write the stale thread listing here as CSV instead of TSV on stderr")
	outPath := flag.String("out", "", "write the summary JSON here as well as to stdout")
	limit := flag.Int("limit", 0, "scan at most this many PRs (0 = all)")
	verdictList := flag.String("verdicts", "pr_merged,pr_closed,file_changed", "stale verdicts to list and apply, comma separated")
	var refs []prRef
	flag.Func("pr", "PR to read live, as owner/repo#number (repeatable)", func(s string) error {
		owner, repo, number, err := parsePRRef(s)
		if err != nil {
			return err
		}
		refs = append(refs, prRef{owner, repo, number})
		return nil
	})
	flag.Parse()
	log.SetFlags(0)
	wanted, err := parseVerdicts(*verdictList)
	if err != nil {
		log.Fatal(err)
	}
	if (*dumpsDir == "") == (len(refs) == 0) {
		fmt.Fprintln(os.Stderr, "usage: threadcleanup (--dumps <dir> | --pr owner/repo#N ...) [--dry-run=false]")
		os.Exit(2)
	}
	if !*dryRun && *dumpsDir != "" {
		log.Fatal("--dumps is a dry run; live mode takes --pr")
	}
	if *dumpsDir != "" && *cacheDir == "" {
		log.Print("warning: no --cache, so every compare is unknown and stale_file_changed counts nothing")
	}
	bots := map[string]bool{}
	if *bot != "" {
		bots[replaykit.BareLogin(*bot)] = true
	}

	ctx := context.Background()
	var prs []*pr
	var decisions []decision
	var gh *github.Client
	if *dumpsDir != "" {
		var err error
		prs, decisions, err = scanDumps(*dumpsDir, *cacheDir, *offline, bots, *limit)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		var err error
		gh, err = newGitHubClient(ctx)
		if err != nil {
			log.Fatalf("github: %v", err)
		}
		changed := liveCompares(ctx, gh)
		if *limit > 0 && len(refs) > *limit {
			refs = refs[:*limit]
		}
		for _, r := range refs {
			p, err := fromGitHub(ctx, gh, r.owner, r.repo, r.number, bots)
			if err != nil {
				log.Fatalf("%s/%s#%d: %v", r.owner, r.repo, r.number, err)
			}
			prs = append(prs, p)
			decisions = append(decisions, classify(p, changed)...)
		}
	}

	stale := staleOnly(decisions, wanted)
	if err := writeListing(*csvPath, stale); err != nil {
		log.Fatalf("listing: %v", err)
	}
	s := summarize(prs, decisions)
	if !*dryRun {
		if !publisher.ThreadResolutionFromEnv() {
			log.Fatalf("%s is off; nothing resolved", publisher.ThreadResolutionEnv)
		}
		ledger, allowed, err := openLedgerGate(gh)
		if err != nil {
			log.Fatalf("ledger: %v", err)
		}
		defer ledger.Close()
		a := apply(ctx, gh, ledger, allowed, stale, log.Printf)
		s.Applied = &a
	}
	out, _ := json.MarshalIndent(s, "", "  ")
	if *outPath != "" {
		if err := os.WriteFile(*outPath, append(out, '\n'), 0o644); err != nil {
			log.Fatalf("out: %v", err)
		}
	}
	fmt.Println(string(out))
}

func scanDumps(dumpsDir, cacheDir string, offline bool, bots map[string]bool, limit int) ([]*pr, []decision, error) {
	dumps, err := replaykit.LoadDumps(dumpsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("load dumps: %w", err)
	}
	if limit > 0 && len(dumps) > limit {
		dumps = dumps[:limit]
	}
	changed, err := dumpCompares(cacheDir, offline)
	if err != nil {
		return nil, nil, err
	}
	var prs []*pr
	var decisions []decision
	for _, d := range dumps {
		p := fromDump(d, bots)
		prs = append(prs, p)
		decisions = append(decisions, classify(p, changed)...)
	}
	return prs, decisions, nil
}

func dumpCompares(cacheDir string, offline bool) (changedFiles, error) {
	if cacheDir == "" {
		return func(string, string, string, string) ([]string, bool) { return nil, false }, nil
	}
	var f replaykit.Fetcher
	if !offline {
		f = ghCompares{}
	}
	store, err := replaykit.NewStore(cacheDir, f)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	return func(owner, repo, base, head string) ([]string, bool) {
		res, ok, err := store.Compare(owner, repo, base, head)
		if err != nil {
			log.Printf("%s/%s compare %s...%s: %v", owner, repo, replaykit.Short(base), replaykit.Short(head), err)
			return nil, false
		}
		return res.Files, ok && res.Complete()
	}, nil
}

func parseVerdicts(list string) (map[verdict]bool, error) {
	out := map[verdict]bool{}
	for _, name := range strings.Split(list, ",") {
		v := verdict(strings.TrimSpace(name))
		if !v.stale() {
			return nil, fmt.Errorf("--verdicts %q: want any of pr_merged, pr_closed, file_changed", name)
		}
		out[v] = true
	}
	return out, nil
}

type compared struct {
	files []string
	known bool
}

func liveCompares(ctx context.Context, gh *github.Client) changedFiles {
	seen := map[string]compared{}
	return func(owner, repo, base, head string) ([]string, bool) {
		key := owner + "/" + repo + " " + base + "..." + head
		if c, ok := seen[key]; ok {
			return c.files, c.known
		}
		files, truncated, err := gh.CompareFiles(ctx, owner, repo, base, head)
		if err != nil {
			log.Printf("%s/%s compare %s...%s: %v", owner, repo, replaykit.Short(base), replaykit.Short(head), err)
			return nil, false
		}
		seen[key] = compared{files, !truncated}
		return files, !truncated
	}
}

type ghCompares struct{}

func (ghCompares) Sidecar(string, string, int, string) ([]byte, error) {
	return nil, replaykit.ErrNotFound
}

func (ghCompares) Compare(owner, repo, base, head string) (replaykit.CompareResult, error) {
	return replaykit.CompareViaGH(owner, repo, base, head)
}

// writeListing names real repositories, so a CSV file belongs outside the
// repo; without one the listing goes to stderr so stdout stays the JSON
// summary.
func writeListing(path string, stale []decision) error {
	var w io.Writer = os.Stderr
	if path != "" {
		fh, err := os.Create(path)
		if err != nil {
			return err
		}
		defer fh.Close()
		w = fh
	}
	cw := csv.NewWriter(w)
	if path == "" {
		cw.Comma = '\t'
	}
	if err := cw.Write([]string{"pr", "verdict", "comment_id", "path", "posted_sha", "head_sha", "outdated", "posted_at", "url"}); err != nil {
		return err
	}
	for _, d := range stale {
		p, t := d.PR, d.Thread
		if err := cw.Write([]string{
			p.ref(), string(d.Verdict), strconv.FormatInt(t.RootCommentID, 10), t.Path,
			replaykit.Short(t.PostedSHA), replaykit.Short(p.HeadSHA), strconv.FormatBool(t.Outdated),
			t.PostedAt.UTC().Format(time.RFC3339),
			fmt.Sprintf("https://github.com/%s/%s/pull/%d#discussion_r%d", p.Owner, p.Repo, p.Number, t.RootCommentID),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func newGitHubClient(ctx context.Context) (*github.Client, error) {
	if appID := os.Getenv("GITHUB_APP_ID"); appID != "" {
		app, err := github.NewAppClient(appID, os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH"), os.Getenv("GITHUB_APP_INSTALLATION_ID"))
		if err != nil {
			return nil, err
		}
		token, err := app.GetToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("installation token: %w", err)
		}
		c := github.NewClient(token, "")
		c.SetAppClient(app)
		return c, nil
	}
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		return nil, fmt.Errorf("set GITHUB_TOKEN or the GITHUB_APP_* variables")
	}
	return github.NewClient(token, os.Getenv("GITHUB_USERNAME")), nil
}

func openLedgerGate(gh *github.Client) (*db.GormDB, gate, error) {
	var ledger *db.GormDB
	var err error
	switch {
	case os.Getenv("DATABASE_URL") != "":
		ledger, err = db.NewGormPostgres(os.Getenv("DATABASE_URL"))
	case os.Getenv("DB_PATH") != "":
		ledger, err = db.NewGormSQLite(os.Getenv("DB_PATH"))
	default:
		return nil, nil, fmt.Errorf("set DATABASE_URL or DB_PATH")
	}
	if err != nil {
		return nil, nil, err
	}
	enabled, err := ledger.GetSetting(poller.SettingPublishEnabledAuthors)
	if err != nil {
		return nil, nil, err
	}
	optOut, err := ledger.GetSetting(poller.SettingPublishOptOutAuthors)
	if err != nil {
		return nil, nil, err
	}
	var teams *github.TeamResolver
	if org := os.Getenv("GITHUB_ORG_NAME"); org != "" {
		teams = github.NewTeamResolver(org, gh)
	} else if strings.Contains(enabled, "team:") {
		return nil, nil, fmt.Errorf("%s has team entries; set GITHUB_ORG_NAME", poller.SettingPublishEnabledAuthors)
	}
	return ledger, poller.NewPublishGate(teams, enabled, optOut).Allowed, nil
}
