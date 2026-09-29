package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v57/github"
	githubclient "pr-review-server/github"
)

type EvidenceClient interface {
	CollectApprovalEvidence(context.Context, string, string, int, githubclient.ApprovalReadLimits) (*githubclient.ApprovalEvidence, error)
}
type ProviderIdentity struct {
	Provider string `json:"provider"`
	ActorID  int64  `json:"actor_id"`
	AppID    int64  `json:"app_id,omitempty"`
}
type PRismEvidence func(context.Context, Target) ([]Source, []Evidence, []Concern, []Endpoint, error)
type Collector struct {
	GitHub    EvidenceClient
	Providers []ProviderIdentity
	PRism     PRismEvidence
	Limits    githubclient.ApprovalReadLimits
}

func evidenceDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func (c *Collector) identity(user *gh.User, id string) Source {
	s := Source{ID: id, Provider: "generic", ActorID: user.GetID(), Login: user.GetLogin(), ActorType: user.GetType(), Presence: "observed", Completion: "unknown", FileCoverage: "not_reported", AdapterVersion: "github-v1", RevisionRelation: "unknown"}
	for _, allowed := range c.Providers {
		if allowed.ActorID > 0 && allowed.ActorID == user.GetID() && user.GetType() == "Bot" && (allowed.Provider == "greptile" || allowed.Provider == "copilot") && allowed.AppID == 0 {
			s.Provider = allowed.Provider
			s.Verified = true
			s.Verification = "operator_actor_id_and_bot_type"
			break
		}
	}
	return s
}

func sourceRevision(s *Source, sha, head string) {
	s.ReviewedSHA = sha
	if sha == "" {
		s.RevisionRelation = "unknown"
	} else if sha == head {
		s.RevisionRelation = "current"
	} else {
		s.RevisionRelation = "older"
	}
}

var reportedFileCoverage = regexp.MustCompile(`(?i)reviewed\s+(\d+)\s*(?:out\s+of|of|/)\s*(\d+)\s+(?:changed\s+)?files`)
var greptileReviewSummary = regexp.MustCompile(`(?im)(?:#{1,6}\s*Greptile Summary|<h[1-6]>\s*Greptile Summary)`)

func reviewCompletesProvider(source Source, review *gh.PullRequestReview) bool {
	if !source.Verified {
		return false
	}
	switch strings.ToUpper(review.GetState()) {
	case "APPROVED", "CHANGES_REQUESTED":
		return true
	case "COMMENTED":
		body := review.GetBody()
		lower := strings.ToLower(body)
		if source.Provider == "copilot" {
			return strings.Contains(lower, "copilot reviewed") && reportedFileCoverage.MatchString(body)
		}
		return source.Provider == "greptile" && greptileReviewSummary.MatchString(body)
	default:
		return false
	}
}

func isViewer(user *gh.User, viewer Viewer) bool {
	if viewer.GitHubID > 0 && user.GetID() > 0 {
		return viewer.GitHubID == user.GetID()
	}
	return viewer.Login != "" && strings.EqualFold(user.GetLogin(), viewer.Login)
}

func sourceCoverage(source *Source, body string) {
	if !source.Verified {
		return
	}
	lower := strings.ToLower(body)
	for _, marker := range []string{"unable to review", "could not review", "review skipped", "timed out", "review incomplete", "files were skipped", "partial review", "review was interrupted", "wasn't able to review", "was not able to review"} {
		if strings.Contains(lower, marker) {
			source.Incomplete = true
			source.FileCoverage = "reported_partial"
		}
	}
	for _, count := range reportedFileCoverage.FindAllStringSubmatch(body, -1) {
		reviewed, a := strconv.Atoi(count[1])
		total, b := strconv.Atoi(count[2])
		if a != nil || b != nil || reviewed < total {
			source.Incomplete = true
			source.FileCoverage = "reported_partial"
		} else if total > 0 && reviewed == total && !source.Incomplete {
			source.FileCoverage = "reported_complete"
		}
	}
}

func (c *Collector) Collect(ctx context.Context, target Target, viewer Viewer) (Snapshot, error) {
	if c.GitHub == nil {
		return Snapshot{}, fmt.Errorf("evidence client unavailable")
	}
	remote, err := c.GitHub.CollectApprovalEvidence(ctx, target.Owner, target.Repo, target.Number, c.Limits)
	if err != nil {
		return Snapshot{}, err
	}
	if remote == nil || remote.PR == nil {
		return Snapshot{}, fmt.Errorf("missing pull request")
	}
	pr := remote.PR
	s := Snapshot{Target: target, Viewer: viewer, CapturedAt: time.Now().UTC(), Eligible: true, RepositoryID: pr.GetBase().GetRepo().GetID(), AccessPartition: remote.AccessPartition, Revision: Revision{Head: pr.GetHead().GetSHA(), Base: pr.GetBase().GetSHA(), MergeBase: remote.MergeBase}, Manifest: Manifest{Complete: true, TotalBytes: int64(remote.Bytes), AdapterVersions: map[string]string{"github": "github-v1", "greptile": "github-review-v1", "copilot": "github-review-v1"}}}
	for _, endpoint := range remote.Endpoints {
		ep := Endpoint{Name: endpoint.Name, Pages: endpoint.Pages, Complete: endpoint.Complete}
		if endpoint.Error != "" {
			ep.Errors = []string{endpoint.Error}
		}
		s.Manifest.Endpoints = append(s.Manifest.Endpoints, ep)
		if !ep.Complete {
			s.Manifest.Complete = false
		}
	}
	exclude := func(reason string) { s.Eligible = false; s.ExclusionReasons = append(s.ExclusionReasons, reason) }
	if pr.GetState() != "open" || pr.GetMerged() {
		exclude("pr_closed")
	}
	if pr.GetDraft() {
		exclude("pr_draft")
	}
	if isViewer(pr.GetUser(), viewer) {
		exclude("own_pr")
	}
	if pr.GetChangedFiles() > 500 {
		s.Manifest.Complete = false
		s.Manifest.Errors = append(s.Manifest.Errors, "changed file limit exceeded")
	}
	baseURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", target.Owner, target.Repo, target.Number)
	add := func(source Source, e Evidence) {
		e.SourceID = source.ID
		e.BodyDigest = evidenceDigest(e.Body)
		s.Sources = append(s.Sources, source)
		s.Evidence = append(s.Evidence, e)
	}
	add(c.identity(pr.GetUser(), "pr_description"), Evidence{ID: "pr_description", Kind: "description", RemoteID: strconv.Itoa(target.Number), Body: pr.GetBody(), URL: baseURL, CreatedAt: pr.GetCreatedAt().Time, UpdatedAt: pr.GetUpdatedAt().Time})
	reviews := append([]*gh.PullRequestReview(nil), remote.Reviews...)
	sort.SliceStable(reviews, func(i, j int) bool {
		a, b := reviews[i].GetSubmittedAt().Time, reviews[j].GetSubmittedAt().Time
		if a.Equal(b) {
			return reviews[i].GetID() < reviews[j].GetID()
		}
		return a.Before(b)
	})
	standing := map[int64]*gh.PullRequestReview{}
	botStanding := map[int64]*gh.PullRequestReview{}
	for _, review := range reviews {
		id := "review:" + strconv.FormatInt(review.GetID(), 10)
		source := c.identity(review.GetUser(), id)
		sourceRevision(&source, review.GetCommitID(), s.Revision.Head)
		state := strings.ToUpper(review.GetState())
		if review.GetUser().GetID() <= 0 || (review.GetUser().GetType() != "User" && review.GetUser().GetType() != "Bot") || (state != "PENDING" && review.GetSubmittedAt().Time.IsZero()) {
			s.Manifest.Complete = false
			s.Manifest.Errors = append(s.Manifest.Errors, "review identity or submission metadata unavailable")
		}
		source.Verdict = strings.ToLower(state)
		if reviewCompletesProvider(source, review) {
			source.Completion = "completed"
		}
		if state == "PENDING" {
			source.Completion = "running"
			if source.Verified {
				s.ReviewInProgress = true
			}
		}
		sourceCoverage(&source, review.GetBody())
		if review.GetUser().GetType() == "User" && (state == "APPROVED" || state == "CHANGES_REQUESTED" || state == "DISMISSED") {
			standing[review.GetUser().GetID()] = review
		}
		if review.GetUser().GetType() == "Bot" && (state == "APPROVED" || state == "CHANGES_REQUESTED" || state == "DISMISSED") {
			botStanding[review.GetUser().GetID()] = review
		}
		add(source, Evidence{ID: id, Kind: "review", RemoteID: strconv.FormatInt(review.GetID(), 10), Body: "Review state: " + state + "\n" + review.GetBody(), URL: baseURL + "#pullrequestreview-" + strconv.FormatInt(review.GetID(), 10), ReviewedSHA: review.GetCommitID(), CreatedAt: review.GetSubmittedAt().Time, UpdatedAt: review.GetSubmittedAt().Time})
	}
	for _, review := range standing {
		if review.GetState() == "CHANGES_REQUESTED" {
			s.HumanChangesRequested = true
		}
		if isViewer(review.GetUser(), viewer) && review.GetState() == "APPROVED" && review.GetCommitID() == s.Revision.Head {
			exclude("already_approved")
		}
	}
	for _, review := range botStanding {
		if review.GetState() == "CHANGES_REQUESTED" && review.GetCommitID() == s.Revision.Head {
			s.ProviderChangesRequested = true
		}
	}
	for _, comment := range remote.Comments {
		id := "comment:" + strconv.FormatInt(comment.GetID(), 10)
		source := c.identity(comment.GetUser(), id)
		sourceCoverage(&source, comment.GetBody())
		add(source, Evidence{ID: id, Kind: "comment", RemoteID: strconv.FormatInt(comment.GetID(), 10), Body: comment.GetBody(), URL: baseURL + "#issuecomment-" + strconv.FormatInt(comment.GetID(), 10), CreatedAt: comment.GetCreatedAt().Time, UpdatedAt: comment.GetUpdatedAt().Time})
	}
	threadFor := map[int64]githubclient.ApprovalThread{}
	for _, thread := range remote.Threads {
		for _, id := range thread.Comments {
			threadFor[id] = thread
		}
	}
	seenInline := map[int64]bool{}
	for _, comment := range remote.InlineComments {
		seenInline[comment.GetID()] = true
		id := "inline:" + strconv.FormatInt(comment.GetID(), 10)
		thread, known := threadFor[comment.GetID()]
		if !known {
			s.Manifest.Complete = false
			s.Manifest.Errors = append(s.Manifest.Errors, "inline comment has no collected thread")
		}
		source := c.identity(comment.GetUser(), id)
		sourceRevision(&source, comment.GetOriginalCommitID(), s.Revision.Head)
		body := fmt.Sprintf("File: %s\nOriginal line: %d\nCurrent line: %d\nOriginal revision: %s\nCurrent revision: %s\nReview: %d\nReply to: %d\nOutdated thread: %t\nDiff:\n%s\n\n%s", comment.GetPath(), comment.GetOriginalLine(), comment.GetLine(), comment.GetOriginalCommitID(), comment.GetCommitID(), comment.GetPullRequestReviewID(), comment.GetInReplyTo(), thread.Outdated, comment.GetDiffHunk(), comment.GetBody())
		start, end := comment.GetOriginalStartLine(), comment.GetOriginalLine()
		if start <= 0 {
			start = end
		}
		add(source, Evidence{ID: id, Kind: "inline_comment", RemoteID: strconv.FormatInt(comment.GetID(), 10), ParentID: thread.ID, Body: body, URL: baseURL + "#discussion_r" + strconv.FormatInt(comment.GetID(), 10), ReviewedSHA: comment.GetOriginalCommitID(), Resolved: thread.Resolved, CreatedAt: comment.GetCreatedAt().Time, UpdatedAt: comment.GetUpdatedAt().Time, Path: comment.GetPath(), StartLine: start, EndLine: end})
	}
	for id := range threadFor {
		if !seenInline[id] {
			s.Manifest.Complete = false
			s.Manifest.Errors = append(s.Manifest.Errors, "thread comment missing from collected comments")
		}
	}
	for _, user := range remote.RequestedUsers {
		source := c.identity(user, "request:"+strconv.FormatInt(user.GetID(), 10))
		source.Completion = "requested"
		if source.Verified {
			s.ReviewInProgress = true
		}
		s.Sources = append(s.Sources, source)
	}
	for _, team := range remote.RequestedTeams {
		s.Evidence = append(s.Evidence, Evidence{ID: "team_request:" + strconv.FormatInt(team.GetID(), 10), Kind: "review_request", RemoteID: strconv.FormatInt(team.GetID(), 10), Body: team.GetSlug(), BodyDigest: evidenceDigest(team.GetSlug()), URL: baseURL})
	}
	checks := map[string]*gh.CheckRun{}
	for _, check := range remote.Checks {
		key := fmt.Sprintf("%d:%d:%s", check.GetApp().GetID(), check.GetCheckSuite().GetID(), check.GetName())
		if check.GetCheckSuite().GetID() == 0 {
			key = fmt.Sprintf("unattributed:%d", check.GetID())
		}
		if prev := checks[key]; prev == nil || check.GetID() > prev.GetID() {
			checks[key] = check
		}
	}
	for _, check := range checks {
		state := strings.ToLower(check.GetConclusion())
		if check.GetStatus() != "completed" {
			state = "pending"
		}
		if state == "" {
			state = "unknown"
		}
		s.Checks = append(s.Checks, Check{Name: check.GetName(), State: state, SHA: check.GetHeadSHA()})
	}
	statuses := map[string]*gh.RepoStatus{}
	for _, status := range remote.Statuses {
		if prev := statuses[status.GetContext()]; prev == nil || status.GetID() > prev.GetID() {
			statuses[status.GetContext()] = status
		}
	}
	for _, status := range statuses {
		s.Checks = append(s.Checks, Check{Name: status.GetContext(), State: status.GetState(), SHA: s.Revision.Head})
	}
	suiteRuns := map[int64]int{}
	for _, check := range remote.Checks {
		suiteRuns[check.GetCheckSuite().GetID()]++
	}
	for _, suite := range remote.Suites {
		state := strings.ToLower(suite.GetConclusion())
		if suite.GetStatus() != "completed" {
			// Apps register a suite on every push whether or not they run
			// anything; an unfinished suite with no check runs is not CI.
			// Suites that do run checks are represented by those runs.
			if suiteRuns[suite.GetID()] == 0 {
				continue
			}
			state = "pending"
		}
		if state == "" {
			state = "unknown"
		}
		if state == "success" {
			continue
		}
		s.Checks = append(s.Checks, Check{Name: fmt.Sprintf("Check suite %d", suite.GetID()), State: state, SHA: suite.GetHeadSHA()})
	}
	if c.PRism == nil {
		s.Manifest.Complete = false
		s.Manifest.Errors = append(s.Manifest.Errors, "PRism history unavailable")
	} else {
		sources, evidence, concerns, endpoints, e := c.PRism(ctx, target)
		s.Sources = append(s.Sources, sources...)
		s.Evidence = append(s.Evidence, evidence...)
		s.Concerns = append(s.Concerns, concerns...)
		s.Manifest.Endpoints = append(s.Manifest.Endpoints, endpoints...)
		if e != nil {
			s.Manifest.Complete = false
			s.Manifest.Errors = append(s.Manifest.Errors, "PRism history incomplete")
		}
		for _, ep := range endpoints {
			if !ep.Complete {
				s.Manifest.Complete = false
			}
		}
		runTimes := map[string]time.Time{}
		for _, artifact := range evidence {
			if artifact.Kind == "prism_run" {
				runTimes[artifact.SourceID] = artifact.CreatedAt
			}
		}
		var latestRun *Source
		for index, source := range sources {
			if source.Provider == "prism" && source.ReviewedSHA == s.Revision.Head && source.Completion == "completed" && (latestRun == nil || runTimes[source.ID].After(runTimes[latestRun.ID]) || (runTimes[source.ID].Equal(runTimes[latestRun.ID]) && source.ID > latestRun.ID)) {
				latestRun = &sources[index]
			}
			if (source.Completion == "queued" || source.Completion == "running") && (source.ReviewedSHA == s.Revision.Head || source.ReviewedSHA == "") {
				s.ReviewInProgress = true
			}
		}
		if latestRun != nil && latestRun.Verdict == "request_changes" {
			s.ProviderChangesRequested = true
		}
	}
	allowed := map[string]bool{}
	for _, sha := range []string{s.Revision.Head, s.Revision.Base, s.Revision.MergeBase} {
		if fullRevision.MatchString(sha) {
			allowed[sha] = true
		} else {
			s.Manifest.Complete = false
			s.Manifest.Errors = append(s.Manifest.Errors, "required revision unavailable")
		}
	}
	for _, e := range s.Evidence {
		if fullRevision.MatchString(e.ReviewedSHA) {
			allowed[e.ReviewedSHA] = true
		}
	}
	for sha := range allowed {
		s.AllowedRevisions = append(s.AllowedRevisions, sha)
	}
	sort.Strings(s.AllowedRevisions)
	if s.RepositoryID == 0 {
		s.Manifest.Complete = false
		s.Manifest.Errors = append(s.Manifest.Errors, "repository identity unavailable")
	}
	s.Manifest.TotalBytes = 0
	for _, artifact := range s.Evidence {
		s.Manifest.TotalBytes += int64(len(artifact.Body))
	}
	if len(s.Evidence) > 2000 || s.Manifest.TotalBytes > 8<<20 {
		s.Manifest.Complete = false
		s.Manifest.Errors = append(s.Manifest.Errors, "combined evidence limit exceeded")
		s.Manifest.Endpoints = append(s.Manifest.Endpoints, Endpoint{Name: "combined_evidence", Truncated: true, Errors: []string{"combined evidence limit exceeded"}})
	}
	CanonicalizeSnapshot(&s)
	return s, nil
}

func CanonicalizeSnapshot(s *Snapshot) {
	sort.Slice(s.Sources, func(i, j int) bool { return s.Sources[i].ID < s.Sources[j].ID })
	sort.Slice(s.Evidence, func(i, j int) bool { return s.Evidence[i].ID < s.Evidence[j].ID })
	sort.Slice(s.Concerns, func(i, j int) bool { return s.Concerns[i].ID < s.Concerns[j].ID })
	sort.Slice(s.Checks, func(i, j int) bool {
		a, b := s.Checks[i], s.Checks[j]
		return a.Name+":"+a.State+":"+a.SHA < b.Name+":"+b.State+":"+b.SHA
	})
	s.Digest = SnapshotDigest(*s)
	if s.ID == "" {
		s.ID = s.Digest
	}
}
