package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var errNotFound = errors.New("not found")

// fetcher supplies what the cache lacks. A nil fetcher means offline: every
// cache miss is reported as missing data.
type fetcher interface {
	Sidecar(owner, repo string, number int, sha7 string) ([]byte, error)
	Compare(owner, repo, base, head string) (compareResult, error)
}

type compareResult struct {
	Files []string `json:"files"`
	Error string   `json:"error,omitempty"`
	// Truncated marks a response at the endpoint's file cap, which may have
	// dropped the cited file.
	Truncated bool `json:"truncated,omitempty"`
}

// compareFilesCap is the most files the compare endpoint returns; it does not
// page them.
const compareFilesCap = 300

func (r compareResult) complete() bool {
	return r.Error == "" && !r.Truncated
}

// store caches sidecars as <owner>_<repo>_<number>_<sha7>.json and compares
// under compare/<owner>_<repo>_<base7>_<head7>.json. A sidecar the server
// does not have is remembered with a .missing marker so it is asked for once.
type store struct {
	dir string
	f   fetcher
}

func newStore(dir string, f fetcher) (*store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "compare"), 0o755); err != nil {
		return nil, err
	}
	return &store{dir: dir, f: f}, nil
}

func sidecarName(owner, repo string, number int, sha7 string) string {
	return fmt.Sprintf("%s_%s_%d_%s.json", owner, repo, number, sha7)
}

func (s *store) sidecar(owner, repo string, number int, sha7 string) ([]byte, bool, error) {
	path := filepath.Join(s.dir, sidecarName(owner, repo, number, sha7))
	if raw, err := os.ReadFile(path); err == nil {
		if json.Valid(raw) {
			return raw, true, nil
		}
		_ = os.Remove(path)
	}
	if _, err := os.Stat(path + ".missing"); err == nil {
		return nil, false, nil
	}
	if s.f == nil {
		return nil, false, nil
	}
	raw, err := s.f.Sidecar(owner, repo, number, sha7)
	if errors.Is(err, errNotFound) {
		_ = os.WriteFile(path+".missing", []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := writeFileAtomic(path, raw); err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func compareKey(owner, repo, base, head string) string {
	return fmt.Sprintf("%s_%s_%s_%s", owner, repo, short(base), short(head))
}

func (s *store) compare(owner, repo, base, head string) (compareResult, bool, error) {
	path := filepath.Join(s.dir, "compare", compareKey(owner, repo, base, head)+".json")
	if raw, err := os.ReadFile(path); err == nil {
		var res compareResult
		if err := json.Unmarshal(raw, &res); err == nil {
			return res, res.complete(), nil
		}
	}
	if s.f == nil {
		return compareResult{}, false, nil
	}
	res, err := s.f.Compare(owner, repo, base, head)
	if err != nil {
		return compareResult{}, false, err
	}
	raw, _ := json.Marshal(res)
	if err := writeFileAtomic(path, raw); err != nil {
		return compareResult{}, false, err
	}
	return res, res.complete(), nil
}

// writeFileAtomic publishes the file with a rename so a concurrent reader
// never sees a partial cache entry.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// liveFetcher reads sidecars from the review server and compares through the
// gh CLI, so it needs nothing beyond the operator's existing logins.
type liveFetcher struct {
	baseURL string
	token   string
	client  *http.Client
}

func newLiveFetcher(baseURL string) (*liveFetcher, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil, errors.New("PRISM_BASE_URL is not set")
	}
	token := strings.TrimSpace(os.Getenv("PRISM_TOKEN"))
	if token == "" {
		out, err := exec.Command("gh", "auth", "token").Output()
		if err != nil {
			return nil, fmt.Errorf("PRISM_TOKEN unset and gh auth token failed: %w", err)
		}
		token = strings.TrimSpace(string(out))
	}
	return &liveFetcher{baseURL: baseURL, token: token, client: &http.Client{Timeout: 60 * time.Second}}, nil
}

func (f *liveFetcher) Sidecar(owner, repo string, number int, sha7 string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, f.baseURL+"/reviews/"+sidecarName(owner, repo, number, sha7), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("sidecar %s: HTTP %d", sidecarName(owner, repo, number, sha7), resp.StatusCode)
	case !json.Valid(body):
		return nil, fmt.Errorf("sidecar %s: response is not JSON", sidecarName(owner, repo, number, sha7))
	}
	return body, nil
}

const compareTimeout = 2 * time.Minute

func (f *liveFetcher) Compare(owner, repo, base, head string) (compareResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), compareTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", "api", fmt.Sprintf("repos/%s/%s/compare/%s...%s", owner, repo, base, head))
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if ctx.Err() != nil {
			msg = "timed out after " + compareTimeout.String()
		} else if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(msg, "404") || strings.Contains(msg, "Not Found") {
			return compareResult{Error: msg}, nil
		}
		return compareResult{}, fmt.Errorf("gh api compare %s...%s: %s", short(base), short(head), msg)
	}
	return parseCompare(stdout.Bytes())
}

// parseCompare flattens the endpoint's file list, with a rename contributing
// both names, and flags a list at the cap from the raw entry count.
func parseCompare(raw []byte) (compareResult, error) {
	var payload struct {
		Files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return compareResult{}, err
	}
	res := compareResult{Truncated: len(payload.Files) >= compareFilesCap}
	for _, fl := range payload.Files {
		res.Files = append(res.Files, fl.Filename)
		if fl.PreviousFilename != "" {
			res.Files = append(res.Files, fl.PreviousFilename)
		}
	}
	return res, nil
}

// prefetch warms the store for every round's sidecar and every ordered pair
// of distinct round heads (a skipped round leaves LastSeenSHA two heads back), with a few workers so the full set finishes
// in minutes rather than an hour.
func prefetch(s *store, dumps []*prDump, bots func(*prDump) map[string]bool, workers int, logf func(string, ...any)) error {
	type job func() error
	var jobs []job
	seenCompare := map[string]bool{}
	for _, d := range dumps {
		d := d
		rs := d.rounds(bots(d))
		seen := map[string]bool{}
		for i, r := range rs {
			r := r
			if !seen[r.SHA7] {
				seen[r.SHA7] = true
				jobs = append(jobs, func() error { _, _, err := s.sidecar(d.Owner, d.Repo, d.Number, r.SHA7); return err })
			}
			for _, prev := range rs[:i] {
				prev := prev
				key := compareKey(d.Owner, d.Repo, prev.SHA, r.SHA)
				if prev.SHA == r.SHA || seenCompare[key] {
					continue
				}
				seenCompare[key] = true
				jobs = append(jobs, func() error { _, _, err := s.compare(d.Owner, d.Repo, prev.SHA, r.SHA); return err })
			}
		}
	}
	if len(jobs) == 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	logf("prefetch: %d lookups with %d workers", len(jobs), workers)
	ch := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				if err := j(); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					logf("prefetch: %v", err)
				}
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	return firstErr
}
