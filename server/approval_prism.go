package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"pr-review-server/db"
	"pr-review-server/gcs"
	"pr-review-server/pkg/approval"
	"pr-review-server/pkg/reviewer/payload"
)

var approvalArtifactID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var approvalArtifactSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)

func approvalBodyDigest(body string) string {
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}

func (s *Server) collectApprovalPRism(ctx context.Context, target approval.Target) (sources []approval.Source, evidence []approval.Evidence, concerns []approval.Concern, endpoints []approval.Endpoint, resultErr error) {
	history := approval.Endpoint{Name: "prism_history"}
	defer func() { endpoints = append([]approval.Endpoint{history}, endpoints...) }()
	if s.db == nil {
		history.Errors = []string{"review history unavailable"}
		return nil, nil, nil, nil, fmt.Errorf("review history unavailable")
	}
	filter := db.ReviewRunFilter{RepoOwner: target.Owner, RepoName: target.Repo, PRNumber: target.Number, Limit: 100}
	seen := map[string]bool{}
	totalBytes := 0
	for page := 0; page < 20; page++ {
		if err := ctx.Err(); err != nil {
			history.Errors = []string{"collection cancelled"}
			return sources, evidence, concerns, endpoints, err
		}
		runs, err := s.db.ListReviewRuns(filter)
		history.Pages++
		if err != nil {
			history.Errors = []string{"review history read failed"}
			return sources, evidence, concerns, endpoints, fmt.Errorf("review history read failed")
		}
		sidecars := s.approvalSidecars(ctx, runs, target)
		for _, run := range runs {
			if seen[run.RunID] || !approvalRunIdentity(run, target) {
				history.Errors = []string{"invalid review run identity or pagination"}
				return sources, evidence, concerns, endpoints, fmt.Errorf("invalid review run identity")
			}
			seen[run.RunID] = true
			source := approval.Source{ID: "prism:" + run.RunID, Provider: "prism", Verified: true, Verification: "durable_run_and_immutable_sidecar", AdapterVersion: "prism-v1", Presence: "observed", Completion: run.Status, ReviewedSHA: run.CommitSHA, RevisionRelation: "older", FileCoverage: "not_reported"}
			source.Verdict = run.Verdict
			if run.CommitSHA == target.ExpectedHeadSHA {
				source.RevisionRelation = "current"
			}
			url := "/api/v1/review-runs/" + run.RunID
			meta := map[string]any{"run_id": run.RunID, "status": run.Status, "commit_sha": run.CommitSHA, "verdict": run.Verdict, "requested_config": run.RequestedConfigJSON, "effective_config": run.EffectiveConfigJSON, "config_hash": run.ConfigHash, "model_fallback": run.ModelFallback, "serving_model_verification": run.ServingModelVerification, "actual_models": run.ActualModelsJSON, "terminal_code": run.TerminalCode, "failure_stage": run.FailureStage}
			var pl payload.Payload
			ep := approval.Endpoint{Name: "prism_run:" + run.RunID, Pages: 1, Complete: true}
			if run.Status == db.ReviewRunStatusCompleted {
				expected := gcs.ReviewRunJSONFileName(run.RepoOwner, run.RepoName, run.PRNumber, run.CommitSHA, run.RunID)
				if run.JSONPath != expected {
					ep.Complete = false
					ep.Errors = []string{"immutable sidecar unavailable"}
				} else {
					read, fetched := sidecars[run.JSONPath]
					body, fetchErr := read.body, read.err
					if !fetched {
						fetchErr = fmt.Errorf("sidecar not fetched")
					}
					totalBytes += len(body)
					if fetchErr != nil {
						ep.Complete = false
						ep.Errors = []string{"immutable sidecar read failed"}
					} else if len(body) > 2<<20 || totalBytes > 8<<20 {
						ep.Complete = false
						ep.Truncated = true
						ep.Errors = []string{"PRism evidence byte limit exceeded"}
					} else {
						pl, err = payload.Decode(body)
						if err != nil || pl.ReviewRun == nil || pl.ReviewRun.RunID != run.RunID || pl.ReviewRun.JSONPath != run.JSONPath || pl.CommitSHA != run.CommitSHA || !strings.EqualFold(pl.Owner, target.Owner) || !strings.EqualFold(pl.Repo, target.Repo) || pl.PRNumber != target.Number || pl.ReviewRun.CompletedAt.IsZero() {
							ep.Complete = false
							ep.Errors = []string{"immutable sidecar identity or schema mismatch"}
						}
					}
				}
				if !ep.Complete {
					source.Incomplete = true
					source.Completion = "unknown"
				} else {
					meta["execution"] = pl.ReviewRun
					meta["required_checks"] = pl.RequiredChecks
					meta["carried_findings"] = pl.CarriedFindings
					if pl.RequiredChecks != nil && pl.RequiredChecks.Answered < pl.RequiredChecks.Issued {
						source.Incomplete = true
						source.FileCoverage = "reported_partial"
					}
					if pl.RequiredChecks != nil && pl.RequiredChecks.Violated > 0 {
						source.Verdict = "request_changes"
					}
					fallback := run.ModelFallback
					for _, model := range pl.ReviewRun.Models {
						fallback = fallback || model.Fallback
					}
					if fallback && !s.approvalFallbackAllowed(pl.ReviewRun.Models) {
						source.Incomplete = true
					}
					for index, finding := range pl.Findings {
						id := source.ID + ":finding:" + strconv.Itoa(index)
						body, _ := json.Marshal(finding)
						e := approval.Evidence{ID: id, SourceID: source.ID, Kind: "prism_finding", RemoteID: run.RunID + ":" + strconv.Itoa(index), ParentID: source.ID, Body: string(body), BodyDigest: approvalBodyDigest(string(body)), URL: url, ReviewedSHA: run.CommitSHA, CreatedAt: pl.ReviewRun.CompletedAt, UpdatedAt: pl.ReviewRun.CompletedAt}
						if strings.EqualFold(finding.File, "SUMMARY") {
							e.Kind = "prism_summary"
							if finding.Summary != nil && finding.Summary.Verdict == "request_changes" {
								source.Verdict = "request_changes"
							}
						} else if strings.EqualFold(finding.File, "CHECK") {
							e.Kind = "prism_check"
						} else {
							e.Path = finding.File
							e.StartLine = finding.Line
							e.EndLine = finding.Line
							concern := approval.Concern{ID: id + ":concern", EvidenceIDs: []string{id}, OriginalSeverity: finding.Severity, Impact: "unknown", Claim: finding.Comment, OriginalRevision: run.CommitSHA, Path: finding.File, StartLine: finding.Line, EndLine: finding.Line}
							concerns = append(concerns, concern)
							e.ConcernIDs = []string{concern.ID}
						}
						evidence = append(evidence, e)
					}
					if pl.RequiredChecks != nil {
						for index, check := range pl.RequiredChecks.Records {
							if check.Unresolved || strings.EqualFold(check.Verdict, "VIOLATED") {
								source.Verdict = "request_changes"
							}
							id := source.ID + ":check:" + strconv.Itoa(index)
							body, _ := json.Marshal(check)
							evidence = append(evidence, approval.Evidence{ID: id, SourceID: source.ID, Kind: "prism_check", RemoteID: run.RunID + ":check:" + strconv.Itoa(index), ParentID: source.ID, Body: string(body), BodyDigest: approvalBodyDigest(string(body)), URL: url, ReviewedSHA: run.CommitSHA, CreatedAt: pl.ReviewRun.CompletedAt, UpdatedAt: pl.ReviewRun.CompletedAt})
						}
					}
				}
			}
			body, _ := json.Marshal(meta)
			evidence = append(evidence, approval.Evidence{ID: source.ID, SourceID: source.ID, Kind: "prism_run", RemoteID: run.RunID, Body: string(body), BodyDigest: approvalBodyDigest(string(body)), URL: url, ReviewedSHA: run.CommitSHA, CreatedAt: run.AcceptedAt, UpdatedAt: run.UpdatedAt})
			sources = append(sources, source)
			endpoints = append(endpoints, ep)
			if totalBytes > 8<<20 || len(evidence) > 2000 {
				history.Errors = []string{"PRism evidence byte limit exceeded"}
				history.Truncated = true
				return sources, evidence, concerns, endpoints, fmt.Errorf("PRism evidence limit exceeded")
			}
		}
		if len(runs) < filter.Limit {
			history.Complete = true
			return sources, evidence, concerns, endpoints, nil
		}
		last := runs[len(runs)-1]
		if last.AcceptedAt.IsZero() {
			history.Errors = []string{"review history cursor missing"}
			return sources, evidence, concerns, endpoints, fmt.Errorf("review history cursor missing")
		}
		filter.BeforeAcceptedAt = last.AcceptedAt
		filter.BeforeRunID = last.RunID
	}
	history.Truncated = true
	history.Errors = []string{"review history page limit exceeded"}
	return sources, evidence, concerns, endpoints, fmt.Errorf("review history limit exceeded")
}

func approvalRunIdentity(run db.ReviewRun, target approval.Target) bool {
	return approvalArtifactID.MatchString(run.RunID) && approvalArtifactSHA.MatchString(run.CommitSHA) && strings.EqualFold(run.RepoOwner, target.Owner) && strings.EqualFold(run.RepoName, target.Repo) && run.PRNumber == target.Number
}

type approvalSidecarRead struct {
	body []byte
	err  error
}

const (
	approvalSidecarFetches    = 4
	approvalSidecarCacheBytes = 64 << 20
)

// approvalSidecars reads the immutable sidecars of a page's completed runs a
// few at a time, from the shared cache when possible, and starts no download
// once the page has read the PRism byte ceiling.
func (s *Server) approvalSidecars(ctx context.Context, runs []db.ReviewRun, target approval.Target) map[string]approvalSidecarRead {
	out := map[string]approvalSidecarRead{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, approvalSidecarFetches)
	total := 0
	record := func(path string, read approvalSidecarRead) {
		mu.Lock()
		out[path] = read
		total += len(read.body)
		mu.Unlock()
	}
	for _, run := range runs {
		if run.Status != db.ReviewRunStatusCompleted || !approvalRunIdentity(run, target) || run.JSONPath != gcs.ReviewRunJSONFileName(run.RepoOwner, run.RepoName, run.PRNumber, run.CommitSHA, run.RunID) {
			continue
		}
		path := run.JSONPath
		if body, ok := s.approvalSidecarCache.get(path); ok {
			record(path, approvalSidecarRead{body: body})
			continue
		}
		sem <- struct{}{}
		mu.Lock()
		_, started := out[path]
		full := total > 8<<20
		if !started && !full {
			out[path] = approvalSidecarRead{err: fmt.Errorf("sidecar read pending")}
		}
		mu.Unlock()
		if started || full {
			<-sem
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			body, err := s.fetchReviewBytes(ctx, path)
			if err == nil && len(body) <= 2<<20 {
				s.approvalSidecarCache.put(path, body)
			}
			record(path, approvalSidecarRead{body: body, err: err})
		}()
	}
	wg.Wait()
	return out
}

// approvalSidecarCache keeps recently read sidecars, which never change once
// written, so a candidate's re-collection does not download them again.
type approvalSidecarCache struct {
	mu      sync.Mutex
	entries map[string][]byte
	order   []string
	bytes   int
}

func (c *approvalSidecarCache) get(path string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, ok := c.entries[path]
	return body, ok
}

func (c *approvalSidecarCache) put(path string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string][]byte{}
	}
	if _, ok := c.entries[path]; ok {
		return
	}
	c.entries[path] = body
	c.order = append(c.order, path)
	c.bytes += len(body)
	for c.bytes > approvalSidecarCacheBytes && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		c.bytes -= len(c.entries[oldest])
		delete(c.entries, oldest)
	}
}

func (s *Server) approvalFallbackAllowed(models []payload.ModelUse) bool {
	if s.poller == nil || len(models) == 0 {
		return false
	}
	_, policy, err := s.poller.ReviewConfigDefaultsAndPolicy()
	if err != nil {
		return false
	}
	found := false
	for _, model := range models {
		if !model.Fallback {
			continue
		}
		found = true
		if !model.ServingModelVerified || model.ServedModel == "" {
			return false
		}
		allowed := false
		backend, ok := policy.Backends[model.Backend]
		if ok && backend.PolicyEnabled {
			for _, name := range backend.Models {
				if name == model.ServedModel {
					allowed = true
				}
			}
		}
		if !allowed {
			return false
		}
	}
	return found
}
