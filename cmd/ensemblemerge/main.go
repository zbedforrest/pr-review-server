// Command ensemblemerge merges N independent review runs of each pull request
// into one review sidecar, offline, so a merge strategy can be scored before
// it ships.
//
// Input is a run directory of review sidecars named <case>.r<N>.json (one per
// run), optionally with meta/metrics.jsonl rows {"case","run","total_s"} for
// the quorum policy. Output is <out>/<case>.r1.json per case, plus
// <out>/meta/merge.jsonl with the merge telemetry.
//
//	OPENROUTER_API_KEY=... go run ./cmd/ensemblemerge -in runs/x -out runs/x-merged -model vendor/model
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"pr-review-server/pkg/reviewer/ensemble"
	"pr-review-server/pkg/reviewer/heal"
	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/types"
)

var sidecarName = regexp.MustCompile(`^(.+)\.r(\d+)\.json$`)

type sidecarFinding struct {
	ID                    string                 `json:"id,omitempty"`
	Severity              string                 `json:"severity"`
	Provenance            string                 `json:"provenance,omitempty"`
	File                  string                 `json:"file"`
	Line                  int                    `json:"line"`
	Comment               string                 `json:"comment"`
	FindingContract       *types.FindingContract `json:"finding_contract,omitempty"`
	FindingContractStatus string                 `json:"finding_contract_status,omitempty"`
	State                 string                 `json:"state"`
	Active                *bool                  `json:"active,omitempty"`
	Summary               *types.SummaryBlock    `json:"summary,omitempty"`
	Sources               []string               `json:"sources,omitempty"`
}

type sidecar struct {
	SchemaVersion json.RawMessage  `json:"schema_version"`
	Owner         string           `json:"owner,omitempty"`
	Repo          string           `json:"repo,omitempty"`
	PRNumber      int              `json:"pr_number,omitempty"`
	CommitSHA     string           `json:"commit_sha,omitempty"`
	Findings      []sidecarFinding `json:"findings"`
	ReviewRun     json.RawMessage  `json:"review_run,omitempty"`
}

type mergeLog struct {
	Case     string                `json:"case"`
	Runs     []int                 `json:"runs"`
	Healed   map[string]string     `json:"healed,omitempty"`
	Merge    ensemble.MergeResult  `json:"merge"`
	Findings []ensembleFindingNote `json:"findings"`
}

type ensembleFindingNote struct {
	ID      string   `json:"id"`
	Support int      `json:"support"`
	Sources []string `json:"sources"`
}

func main() {
	in := flag.String("in", "", "run directory with <case>.r<N>.json sidecars")
	out := flag.String("out", "", "output directory for merged sidecars")
	model := flag.String("model", "", "merge author model (OpenRouter slug); empty = deterministic merge")
	providers := flag.String("providers", "", "comma-separated OpenRouter provider order for the author")
	quorum := flag.Int("quorum", 0, "merge only the fastest N runs per case (needs meta/metrics.jsonl); 0 = all runs")
	minSupport := flag.Int("min-support-critical", 0, "downgrade CRITICALs raised by fewer runs than this")
	par := flag.Int("par", 4, "cases merged in parallel")
	flag.Parse()
	if *in == "" || *out == "" {
		log.Fatal("-in and -out are required")
	}
	cases, err := groupRuns(*in)
	if err != nil {
		log.Fatal(err)
	}
	durations := loadDurations(filepath.Join(*in, "meta", "metrics.jsonl"))
	var author *ensemble.LLMAuthor
	if *model != "" {
		client := llm.NewOpenRouterClient(os.Getenv("OPENROUTER_API_KEY"), os.Getenv("OPENROUTER_BASE_URL"), *model, false)
		author = &ensemble.LLMAuthor{Client: client, Model: *model, ProviderOrder: splitList(*providers)}
	}
	if err := os.MkdirAll(filepath.Join(*out, "meta"), 0o755); err != nil {
		log.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(*out, "meta", "merge.jsonl"))
	if err != nil {
		log.Fatal(err)
	}
	defer logFile.Close()
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, *par)
	ids := make([]string, 0, len(cases))
	for id := range cases {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			entry, err := mergeCase(id, cases[id], durations, *quorum, author, ensemble.Options{MinSupportCritical: *minSupport}, *out)
			if err != nil {
				log.Printf("%s: %v", id, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			line, _ := json.Marshal(entry)
			fmt.Fprintln(logFile, string(line))
			log.Printf("%s: %d runs -> %d findings (%s, %d clusters, %d reinserted, $%.4f)", id, len(entry.Runs),
				len(entry.Merge.Findings), entry.Merge.Method, entry.Merge.Clusters, len(entry.Merge.Report.Reinserted), entry.Merge.Call.CostUSD)
		}(id)
	}
	wg.Wait()
}

func mergeCase(id string, runs map[int]string, durations map[string]float64, quorum int, author *ensemble.LLMAuthor, opts ensemble.Options, outDir string) (mergeLog, error) {
	order := make([]int, 0, len(runs))
	for n := range runs {
		order = append(order, n)
	}
	sort.Ints(order)
	if quorum > 0 && quorum < len(order) {
		sort.SliceStable(order, func(a, b int) bool {
			return durations[key(id, order[a])] < durations[key(id, order[b])]
		})
		order = order[:quorum]
		sort.Ints(order)
	}
	entry := mergeLog{Case: id, Runs: order, Healed: map[string]string{}}
	var findings [][]types.LineComment
	var first sidecar
	for i, n := range order {
		sc, err := readSidecar(runs[n])
		if err != nil {
			return entry, err
		}
		if i == 0 {
			first = sc
		}
		lcs, method := toLineComments(sc)
		if method != "" {
			entry.Healed["r"+strconv.Itoa(n)] = method
		}
		findings = append(findings, lcs)
	}
	entry.Merge = ensemble.Merge(context.Background(), findings, author, opts)
	for i, f := range entry.Merge.Findings {
		entry.Findings = append(entry.Findings, ensembleFindingNote{ID: f.ID, Support: entry.Merge.Support[i], Sources: f.Sources})
	}
	merged := first
	merged.SchemaVersion = json.RawMessage(`"2"`)
	merged.ReviewRun = nil
	merged.Findings = toSidecarFindings(entry.Merge)
	body, err := json.MarshalIndent(merged, "", " ")
	if err != nil {
		return entry, err
	}
	return entry, os.WriteFile(filepath.Join(outDir, id+".r1.json"), body, 0o644)
}

// toLineComments converts a sidecar's active findings back to LineComments.
// A summary-only sidecar whose text is broken findings JSON (a parse
// fallback) is healed first; method names the heal step, or "" when none ran.
func toLineComments(sc sidecar) ([]types.LineComment, string) {
	if len(sc.Findings) == 1 && sc.Findings[0].File == "SUMMARY" && strings.Contains(sc.Findings[0].Comment, `"file_path"`) {
		if res, err := heal.Heal(sc.Findings[0].Comment); err == nil {
			var doc struct {
				Findings []types.LineComment `json:"findings"`
			}
			if json.Unmarshal(res.JSON, &doc) == nil {
				return doc.Findings, res.Method
			}
		}
	}
	var out []types.LineComment
	for _, f := range sc.Findings {
		if f.Active != nil && !*f.Active {
			continue
		}
		out = append(out, types.LineComment{
			ID: f.ID, FilePath: f.File, LineNumber: f.Line, CommentBody: f.Comment,
			Importance: strings.ToUpper(f.Severity), FindingContract: f.FindingContract, Summary: f.Summary,
		})
	}
	return out, ""
}

func toSidecarFindings(m ensemble.MergeResult) []sidecarFinding {
	active := true
	var out []sidecarFinding
	for _, f := range m.Findings {
		sev := strings.ToLower(f.Importance)
		if sev == "" {
			sev = "unknown"
		}
		out = append(out, sidecarFinding{
			ID: f.ID, Severity: sev, Provenance: "agent", File: f.FilePath, Line: f.LineNumber,
			Comment: f.CommentBody, FindingContract: f.FindingContract, FindingContractStatus: types.ContractStatus(f.FindingContract),
			State: "confirmed", Active: &active, Sources: f.Sources,
		})
	}
	s := m.Summary
	if s == nil {
		s = &types.SummaryBlock{Verdict: "approve"}
	}
	out = append(out, sidecarFinding{
		ID: "SUMMARY", Severity: "unknown", Provenance: "agent", File: "SUMMARY",
		Comment:               "Verdict: " + strings.ReplaceAll(s.Verdict, "_", " ") + ".\n\n" + s.Upshot,
		FindingContractStatus: "not_applicable", State: "confirmed", Active: &active, Summary: s,
	})
	return out
}

func groupRuns(dir string) (map[string]map[int]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	cases := map[string]map[int]string{}
	for _, e := range entries {
		m := sidecarName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		if cases[m[1]] == nil {
			cases[m[1]] = map[int]string{}
		}
		cases[m[1]][n] = filepath.Join(dir, e.Name())
	}
	return cases, nil
}

func readSidecar(path string) (sidecar, error) {
	var sc sidecar
	b, err := os.ReadFile(path)
	if err != nil {
		return sc, err
	}
	return sc, json.Unmarshal(b, &sc)
}

func loadDurations(path string) map[string]float64 {
	out := map[string]float64{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		var row struct {
			Case   string  `json:"case"`
			Run    int     `json:"run"`
			TotalS float64 `json:"total_s"`
		}
		if json.Unmarshal([]byte(line), &row) == nil && row.Case != "" {
			out[key(row.Case, row.Run)] = row.TotalS
		}
	}
	return out
}

func key(id string, run int) string { return id + "#" + strconv.Itoa(run) }

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
