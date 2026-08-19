package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	gh "github.com/google/go-github/v57/github"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/payload"
)

// MockGitHubClient implements GitHubClient for testing
type MockGitHubClient struct {
	// PRsRequestingReview to return from GetPRsRequestingReview
	PRsRequestingReview []github.PullRequest
	// MyOpenPRs to return from GetMyOpenPRs
	MyOpenPRs []github.PullRequest
	// IsPROpenResults maps "owner/repo/number" to (isOpen, error)
	IsPROpenResults map[string]struct {
		IsOpen bool
		Err    error
	}
	// GetPRDetailsResults maps "owner/repo/number" to (title, author, error)
	GetPRDetailsResults map[string]struct {
		Title  string
		Author string
		Err    error
	}
	// GetPRHeadSHAResults maps "owner/repo/number" to (sha, error)
	GetPRHeadSHAResults map[string]struct {
		SHA string
		Err error
	}
	// GetPRResults maps "owner/repo/number" to (*github.PullRequest, error)
	GetPRResults map[string]struct {
		PR  *gh.PullRequest
		Err error
	}
	// BatchGetPRReviewDataResults to return from BatchGetPRReviewData
	BatchGetPRReviewDataResults map[string]*github.PRReviewData
	// BatchGetCIStatusResults to return from BatchGetCIStatus
	BatchGetCIStatusResults map[string]*github.CIStatus

	// BatchGetReviewerGroupsResults to return from BatchGetReviewerGroups
	BatchGetReviewerGroupsResults map[string]*github.ReviewerGroupData

	// AllOpenPRs to return from GetAllOpenPRs
	AllOpenPRs []github.PullRequest

	// SearchOpenPRsResults to return from SearchOpenPRs
	SearchOpenPRsResults []github.PRInfo
	// BatchGetPRDetailsResults to return from BatchGetPRDetails
	BatchGetPRDetailsResults map[string]github.PullRequest

	// BatchGetPRStateResults to return from BatchGetPRState
	BatchGetPRStateResults map[string]*github.PRState

	// GetOrgTeamMembersFunc optional function to customize GetOrgTeamMembers behavior
	GetOrgTeamMembersFunc func(ctx context.Context, orgName, teamSlug string) ([]string, error)

	// Track calls for verification
	GetAllOpenPRsCalls []string // org names passed
	IsPROpenCalls      []struct {
		Owner    string
		Repo     string
		PRNumber int
	}
	GetPRHeadSHACalls []struct {
		Owner    string
		Repo     string
		PRNumber int
	}
	BatchGetPRReviewDataCalls [][]github.PullRequest

	// CallLog records the order of method calls for sequencing verification
	CallLog   []string
	callLogMu sync.Mutex
}

func NewMockGitHubClient() *MockGitHubClient {
	return &MockGitHubClient{
		IsPROpenResults: make(map[string]struct {
			IsOpen bool
			Err    error
		}),
		GetPRDetailsResults: make(map[string]struct {
			Title  string
			Author string
			Err    error
		}),
		GetPRHeadSHAResults: make(map[string]struct {
			SHA string
			Err error
		}),
		GetPRResults: make(map[string]struct {
			PR  *gh.PullRequest
			Err error
		}),
		BatchGetPRReviewDataResults: make(map[string]*github.PRReviewData),
		BatchGetCIStatusResults:     make(map[string]*github.CIStatus),
	}
}

func (m *MockGitHubClient) GetPRsRequestingReview(ctx context.Context) ([]github.PullRequest, error) {
	m.callLogMu.Lock()
	m.CallLog = append(m.CallLog, "GetPRsRequestingReview")
	m.callLogMu.Unlock()
	return m.PRsRequestingReview, nil
}

func (m *MockGitHubClient) GetMyOpenPRs(ctx context.Context) ([]github.PullRequest, error) {
	m.callLogMu.Lock()
	m.CallLog = append(m.CallLog, "GetMyOpenPRs")
	m.callLogMu.Unlock()
	return m.MyOpenPRs, nil
}

func (m *MockGitHubClient) GetAllOpenPRs(ctx context.Context, orgName string) ([]github.PullRequest, error) {
	m.callLogMu.Lock()
	m.CallLog = append(m.CallLog, "GetAllOpenPRs")
	m.GetAllOpenPRsCalls = append(m.GetAllOpenPRsCalls, orgName)
	m.callLogMu.Unlock()
	return m.AllOpenPRs, nil
}

func (m *MockGitHubClient) SearchOpenPRs(ctx context.Context, orgName string) ([]github.PRInfo, error) {
	m.callLogMu.Lock()
	m.CallLog = append(m.CallLog, "SearchOpenPRs")
	m.callLogMu.Unlock()
	return m.SearchOpenPRsResults, nil
}

func (m *MockGitHubClient) BatchGetPRDetails(ctx context.Context, prs []github.PRInfo) (map[string]github.PullRequest, error) {
	m.callLogMu.Lock()
	m.CallLog = append(m.CallLog, "BatchGetPRDetails")
	m.callLogMu.Unlock()
	if m.BatchGetPRDetailsResults != nil {
		return m.BatchGetPRDetailsResults, nil
	}
	return make(map[string]github.PullRequest), nil
}

func (m *MockGitHubClient) IsPROpen(ctx context.Context, owner, repo string, prNumber int) (bool, error) {
	key := fmt.Sprintf("%s/%s/%d", owner, repo, prNumber)
	m.IsPROpenCalls = append(m.IsPROpenCalls, struct {
		Owner    string
		Repo     string
		PRNumber int
	}{owner, repo, prNumber})
	m.callLogMu.Lock()
	m.CallLog = append(m.CallLog, "IsPROpen")
	m.callLogMu.Unlock()

	if result, ok := m.IsPROpenResults[key]; ok {
		return result.IsOpen, result.Err
	}
	return true, nil // Default: PR is open
}

func (m *MockGitHubClient) GetPRDetails(ctx context.Context, owner, repo string, prNumber int) (string, string, error) {
	key := fmt.Sprintf("%s/%s/%d", owner, repo, prNumber)
	if result, ok := m.GetPRDetailsResults[key]; ok {
		return result.Title, result.Author, result.Err
	}
	return "", "", fmt.Errorf("no mock result for %s", key)
}

func (m *MockGitHubClient) GetPRHeadSHA(ctx context.Context, owner, repo string, prNumber int) (string, error) {
	key := fmt.Sprintf("%s/%s/%d", owner, repo, prNumber)
	m.GetPRHeadSHACalls = append(m.GetPRHeadSHACalls, struct {
		Owner    string
		Repo     string
		PRNumber int
	}{owner, repo, prNumber})
	m.callLogMu.Lock()
	m.CallLog = append(m.CallLog, "GetPRHeadSHA")
	m.callLogMu.Unlock()

	if result, ok := m.GetPRHeadSHAResults[key]; ok {
		return result.SHA, result.Err
	}
	return "", fmt.Errorf("no mock result for %s", key)
}

func (m *MockGitHubClient) GetPR(ctx context.Context, owner, repo string, prNumber int) (*gh.PullRequest, *gh.Response, error) {
	key := fmt.Sprintf("%s/%s/%d", owner, repo, prNumber)
	if result, ok := m.GetPRResults[key]; ok {
		return result.PR, nil, result.Err
	}
	return nil, nil, fmt.Errorf("no mock result for %s", key)
}

func (m *MockGitHubClient) BatchGetPRReviewData(ctx context.Context, prs []github.PullRequest) (map[string]*github.PRReviewData, error) {
	m.BatchGetPRReviewDataCalls = append(m.BatchGetPRReviewDataCalls, prs)
	m.callLogMu.Lock()
	m.CallLog = append(m.CallLog, "BatchGetPRReviewData")
	m.callLogMu.Unlock()
	return m.BatchGetPRReviewDataResults, nil
}

func (m *MockGitHubClient) BatchGetCIStatus(ctx context.Context, prs []github.PRInfo) (map[string]*github.CIStatus, error) {
	return m.BatchGetCIStatusResults, nil
}

func (m *MockGitHubClient) BatchGetReviewerGroups(ctx context.Context, prs []github.PullRequest) (map[string]*github.ReviewerGroupData, error) {
	if m.BatchGetReviewerGroupsResults != nil {
		return m.BatchGetReviewerGroupsResults, nil
	}
	return nil, nil
}

func (m *MockGitHubClient) BatchGetPRState(ctx context.Context, prs []github.PRInfo) (map[string]*github.PRState, error) {
	m.callLogMu.Lock()
	m.CallLog = append(m.CallLog, "BatchGetPRState")
	m.callLogMu.Unlock()
	if m.BatchGetPRStateResults != nil {
		return m.BatchGetPRStateResults, nil
	}
	// Default: return empty map (no state data — phases will skip all PRs)
	return make(map[string]*github.PRState), nil
}

func (m *MockGitHubClient) GetOrgTeamMembers(ctx context.Context, orgName, teamSlug string) ([]string, error) {
	if m.GetOrgTeamMembersFunc != nil {
		return m.GetOrgTeamMembersFunc(ctx, orgName, teamSlug)
	}
	return nil, nil
}

// MockDatabase implements db.Database for testing
type MockDatabase struct {
	mu                  sync.RWMutex
	ReviewRuns          map[string]*db.ReviewRun
	ReviewStageAttempts map[string][]db.ReviewStageAttempt
	GetReviewRunFunc    func(string) (*db.ReviewRun, error)

	// PRs stored in the mock database (keyed by "owner/repo/number")
	PRs              map[string]*db.PR
	ProjectionRunIDs map[string]string

	// Settings
	AutoReviewEnabled bool
	ReviewNRequests   int

	// Track calls for verification
	UpdatePRMetadataCalls []string // "owner/repo/number" keys, in call order
	DeletePRCalls         []struct {
		Owner    string
		Repo     string
		PRNumber int
	}
	UpdatePRStatusCalls []struct {
		Owner    string
		Repo     string
		PRNumber int
		Status   string
	}
	ResetPRToOutdatedCalls []struct {
		Owner        string
		Repo         string
		PRNumber     int
		NewCommitSHA string
	}
	UpdateUserReviewStatusCalls []struct {
		UserID int
		PRID   int
		Status string
	}
	UpdateUserViaTeamsCalls []struct {
		UserID   int
		PRID     int
		ViaTeams []string
	}

	ManualClaimPRIDs        []int
	TelemetryEvents         []db.TelemetryEvent
	CreateUserErr           error
	HideNonManualViewsCalls []int
	EnsureUserPRViewCalls   []struct {
		UserID   int
		PRID     int
		IsAuthor bool
	}

	// Users in the mock database
	Users []db.User

	// User PR assignments (keyed by "userID/prID")
	UserPRAssignments map[string]*db.UserPRAssignment

	// User PR views (keyed by "userID/prID"), maintained by
	// BatchUpsertUserPRViews and BatchPruneViaTeams so multi-cycle poll tests
	// observe the same state evolution as the real database.
	UserPRViews map[string]*db.UserPRView

	// Track prune calls for verification
	BatchPruneViaTeamsCalls [][]db.ViaTeamsPrune

	// Leader election: nil func = always leader.
	TryAcquireOrRenewLeadershipFunc func(holderID string, ttl time.Duration) (bool, error)

	// Error injection
	DeletePRError          error
	UpdatePRStatusError    error
	ResetPRToOutdatedError error
	GetAllPRsError         error
}

func NewMockDatabase() *MockDatabase {
	return &MockDatabase{
		PRs:                 make(map[string]*db.PR),
		ProjectionRunIDs:    make(map[string]string),
		ReviewRuns:          make(map[string]*db.ReviewRun),
		ReviewStageAttempts: make(map[string][]db.ReviewStageAttempt),
		UserPRViews:         make(map[string]*db.UserPRView),
		AutoReviewEnabled:   true,
		ReviewNRequests:     3,
	}
}

func prDBKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s/%d", owner, repo, number)
}

func (m *MockDatabase) GetPR(owner, repo string, prNumber int) (*db.PR, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := prDBKey(owner, repo, prNumber)
	pr, exists := m.PRs[key]
	if !exists {
		return nil, nil
	}
	return pr, nil
}

func (m *MockDatabase) UpsertPR(pr *db.PR) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(pr.RepoOwner, pr.RepoName, pr.PRNumber)
	m.PRs[key] = pr
	return nil
}

func (m *MockDatabase) UpdatePRStatus(owner, repo string, prNumber int, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.UpdatePRStatusCalls = append(m.UpdatePRStatusCalls, struct {
		Owner    string
		Repo     string
		PRNumber int
		Status   string
	}{owner, repo, prNumber, status})

	if m.UpdatePRStatusError != nil {
		return m.UpdatePRStatusError
	}

	key := prDBKey(owner, repo, prNumber)
	if pr, exists := m.PRs[key]; exists {
		pr.Status = status
	}
	return nil
}

func (m *MockDatabase) ResetPRToOutdated(owner, repo string, prNumber int, newCommitSHA string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ResetPRToOutdatedCalls = append(m.ResetPRToOutdatedCalls, struct {
		Owner        string
		Repo         string
		PRNumber     int
		NewCommitSHA string
	}{owner, repo, prNumber, newCommitSHA})

	if m.ResetPRToOutdatedError != nil {
		return m.ResetPRToOutdatedError
	}

	key := prDBKey(owner, repo, prNumber)
	if pr, exists := m.PRs[key]; exists {
		pr.LastCommitSHA = newCommitSHA
		pr.Status = "pending"
		pr.ReviewHTMLPath = ""
	}
	return nil
}

func (m *MockDatabase) SetPRGenerating(owner, repo string, prNumber int, commitSHA, title, author string, createdAt *time.Time, draft bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(owner, repo, prNumber)
	now := time.Now()
	if pr, exists := m.PRs[key]; exists {
		pr.Status = "generating"
		pr.GeneratingSince = &now
		pr.LastCommitSHA = commitSHA
		pr.Title = title
		pr.Author = author
		pr.CreatedAt = createdAt
		pr.Draft = draft
	} else {
		m.PRs[key] = &db.PR{
			RepoOwner:       owner,
			RepoName:        repo,
			PRNumber:        prNumber,
			LastCommitSHA:   commitSHA,
			Status:          "generating",
			GeneratingSince: &now,
			Title:           title,
			Author:          author,
			CreatedAt:       createdAt,
			Draft:           draft,
		}
	}
	return nil
}

func (m *MockDatabase) SetPRAgentReviewing(owner, repo string, prNumber int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(owner, repo, prNumber)
	if pr, exists := m.PRs[key]; exists {
		pr.Status = "agent_reviewing"
		pr.ErrorMessage = ""
	}
	return nil
}

func (m *MockDatabase) SetPRError(owner, repo string, prNumber int, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(owner, repo, prNumber)
	if pr, exists := m.PRs[key]; exists {
		pr.Status = "error"
		pr.ErrorMessage = message
	}
	return nil
}

func (m *MockDatabase) MarkPRCompleted(owner, repo string, prNumber int, commitSHA, reviewPath string, critical, medium, low int, verdict string, modelFallback bool, reviewRun ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(owner, repo, prNumber)
	if pr, exists := m.PRs[key]; exists {
		now := time.Now()
		pr.Status = "completed"
		pr.LastCommitSHA = commitSHA
		pr.ReviewHTMLPath = reviewPath
		pr.LastReviewedAt = &now
		pr.CriticalCount = critical
		pr.MediumCount = medium
		pr.LowCount = low
		pr.ReviewVerdict = verdict
		pr.ModelFallback = modelFallback
		pr.ErrorMessage = ""
		if len(reviewRun) > 0 {
			pr.ReviewRunID = reviewRun[0]
		}
		if len(reviewRun) > 1 {
			pr.ReviewRunJSON = reviewRun[1]
		}
	}
	return nil
}

func (m *MockDatabase) SetPRGeneratingForReviewRun(owner, repo string, prNumber int, commitSHA, title, author string, createdAt *time.Time, draft bool, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(owner, repo, prNumber)
	now := time.Now()
	pr, exists := m.PRs[key]
	if !exists {
		pr = &db.PR{RepoOwner: owner, RepoName: repo, PRNumber: prNumber}
		m.PRs[key] = pr
	}
	pr.Status = "generating"
	pr.GeneratingSince = &now
	pr.LastCommitSHA = commitSHA
	pr.Title = title
	pr.Author = author
	pr.CreatedAt = createdAt
	pr.Draft = draft
	pr.ErrorMessage = ""
	m.ProjectionRunIDs[key] = runID
	return nil
}

func (m *MockDatabase) SetPRAgentReviewingForReviewRun(owner, repo string, prNumber int, runID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(owner, repo, prNumber)
	if m.ProjectionRunIDs[key] != runID {
		return false, nil
	}
	if pr := m.PRs[key]; pr != nil {
		pr.Status = "agent_reviewing"
		pr.ErrorMessage = ""
	}
	return true, nil
}

func (m *MockDatabase) SetPRErrorForReviewRun(owner, repo string, prNumber int, runID, message string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(owner, repo, prNumber)
	if m.ProjectionRunIDs[key] != runID {
		return false, nil
	}
	if pr := m.PRs[key]; pr != nil {
		pr.Status = "error"
		pr.ErrorMessage = message
		pr.GeneratingSince = nil
	}
	return true, nil
}

func (m *MockDatabase) SetPRErrorIfNoLiveReview(owner, repo string, prNumber int, message string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(owner, repo, prNumber)
	if ownerRunID := m.ProjectionRunIDs[key]; ownerRunID != "" {
		if ownerRun := m.ReviewRuns[ownerRunID]; ownerRun != nil &&
			(ownerRun.Status == db.ReviewRunStatusQueued ||
				(ownerRun.Status == db.ReviewRunStatusRunning && (ownerRun.LeaseExpiresAt == nil || ownerRun.LeaseExpiresAt.After(time.Now())))) {
			return false, nil
		}
	}
	if pr := m.PRs[key]; pr != nil {
		pr.Status = "error"
		pr.ErrorMessage = message
		pr.GeneratingSince = nil
		return true, nil
	}
	return false, nil
}

func (m *MockDatabase) MarkPRCompletedForReviewRun(owner, repo string, prNumber int, projectionRunID, reviewRunID, commitSHA, reviewPath string, critical, medium, low int, verdict string, modelFallback bool, reviewRunJSON string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(owner, repo, prNumber)
	if m.ProjectionRunIDs[key] != projectionRunID {
		return false, nil
	}
	if pr := m.PRs[key]; pr != nil {
		now := time.Now()
		pr.Status = "completed"
		pr.LastCommitSHA = commitSHA
		pr.ReviewHTMLPath = reviewPath
		pr.LastReviewedAt = &now
		pr.GeneratingSince = nil
		pr.CriticalCount = critical
		pr.MediumCount = medium
		pr.LowCount = low
		pr.ReviewVerdict = verdict
		pr.ModelFallback = modelFallback
		pr.ErrorMessage = ""
		pr.ReviewRunID = reviewRunID
		pr.ReviewRunJSON = reviewRunJSON
	}
	return true, nil
}

func (m *MockDatabase) RestorePRCompletedFromCacheForReviewRun(owner, repo string, prNumber int, projectionRunID, reviewRunID, commitSHA, reviewPath string, critical, medium, low int, verdict string, modelFallback bool, reviewRunJSON string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(owner, repo, prNumber)
	pr := m.PRs[key]
	if pr == nil || pr.Status == "generating" || pr.Status == "agent_reviewing" || pr.Status == "completed" {
		return false, nil
	}
	if ownerRunID := m.ProjectionRunIDs[key]; ownerRunID != "" {
		if ownerRun := m.ReviewRuns[ownerRunID]; ownerRun != nil &&
			(ownerRun.Status == db.ReviewRunStatusQueued ||
				(ownerRun.Status == db.ReviewRunStatusRunning && (ownerRun.LeaseExpiresAt == nil || ownerRun.LeaseExpiresAt.After(time.Now())))) {
			return false, nil
		}
	}
	now := time.Now()
	pr.Status = "completed"
	pr.LastCommitSHA = commitSHA
	pr.ReviewHTMLPath = reviewPath
	pr.LastReviewedAt = &now
	pr.GeneratingSince = nil
	pr.CriticalCount = critical
	pr.MediumCount = medium
	pr.LowCount = low
	pr.ReviewVerdict = verdict
	pr.ModelFallback = modelFallback
	pr.ErrorMessage = ""
	pr.ReviewRunID = reviewRunID
	pr.ReviewRunJSON = reviewRunJSON
	m.ProjectionRunIDs[key] = projectionRunID
	return true, nil
}

func (m *MockDatabase) GetAllPRs() ([]db.PR, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.GetAllPRsError != nil {
		return nil, m.GetAllPRsError
	}

	var prs []db.PR
	for _, pr := range m.PRs {
		prs = append(prs, *pr)
	}
	return prs, nil
}

func (m *MockDatabase) DeletePR(owner, repo string, prNumber int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DeletePRCalls = append(m.DeletePRCalls, struct {
		Owner    string
		Repo     string
		PRNumber int
	}{owner, repo, prNumber})

	if m.DeletePRError != nil {
		return m.DeletePRError
	}

	key := prDBKey(owner, repo, prNumber)
	delete(m.PRs, key)
	return nil
}

func (m *MockDatabase) ResetStaleGeneratingPRs(timeoutMinutes int) (int, error) {
	return 0, nil
}

func (m *MockDatabase) ResetErrorPRs(maxAgeMinutes int, maxRetries int) (int, error) {
	return 0, nil
}

func (m *MockDatabase) GetPRsWithMissingMetadata() ([]db.PR, error) {
	return nil, nil
}

func (m *MockDatabase) UpdatePRMetadata(owner, repo string, prNumber int, title, author string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(owner, repo, prNumber)
	m.UpdatePRMetadataCalls = append(m.UpdatePRMetadataCalls, key)
	if pr, exists := m.PRs[key]; exists {
		pr.Title = title
		pr.Author = author
	}
	return nil
}

func (m *MockDatabase) UpdatePRNotes(owner, repo string, prNumber int, notes string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(owner, repo, prNumber)
	if pr, exists := m.PRs[key]; exists {
		pr.Notes = notes
	}
	return nil
}

func (m *MockDatabase) GetPRsWithMissingCreatedAt() ([]db.PR, error) {
	return nil, nil
}

func (m *MockDatabase) UpdatePRCreatedAt(owner, repo string, prNumber int, createdAt time.Time) error {
	return nil
}

func (m *MockDatabase) UpdatePRGitHubUpdatedAt(owner, repo string, prNumber int, updatedAt time.Time) error {
	return nil
}

func (m *MockDatabase) UpdatePRDraft(owner, repo string, prNumber int, draft bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pr, exists := m.PRs[prDBKey(owner, repo, prNumber)]; exists {
		pr.Draft = draft
	}
	return nil
}

func (m *MockDatabase) GetSetting(key string) (string, error) {
	return "", nil
}

func (m *MockDatabase) SetSetting(key, value string) error {
	return nil
}

func (m *MockDatabase) GetAutoReviewRequestedPRs() (bool, error) {
	return m.AutoReviewEnabled, nil
}

func (m *MockDatabase) SetAutoReviewRequestedPRs(enabled bool) error {
	m.AutoReviewEnabled = enabled
	return nil
}

func (m *MockDatabase) GetReviewNRequests() (int, error) {
	return m.ReviewNRequests, nil
}

func (m *MockDatabase) SetReviewNRequests(n int) error {
	m.ReviewNRequests = n
	return nil
}

func (m *MockDatabase) GetGenerateHTML() (bool, error) {
	return true, nil
}

func (m *MockDatabase) SetGenerateHTML(enabled bool) error {
	return nil
}

// User operations (not needed for poller tests, stub implementations)
func (m *MockDatabase) GetUserByGitHubID(githubID int64) (*db.User, error) {
	return nil, nil
}

func (m *MockDatabase) GetUserByID(id int) (*db.User, error) {
	return nil, nil
}

func (m *MockDatabase) GetUserByUsername(username string) (*db.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range m.Users {
		if m.Users[i].GitHubUsername == username {
			return &m.Users[i], nil
		}
	}
	return nil, nil
}

func (m *MockDatabase) GetAllUsers() ([]db.User, error) {
	if m.Users == nil {
		return []db.User{}, nil
	}
	return m.Users, nil
}

func (m *MockDatabase) CreateUser(user *db.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.CreateUserErr != nil {
		// Simulate losing a create race: the winner's row exists, we error.
		m.Users = append(m.Users, db.User{ID: len(m.Users) + 1, GitHubID: user.GitHubID, GitHubUsername: user.GitHubUsername})
		return m.CreateUserErr
	}
	user.ID = len(m.Users) + 1
	m.Users = append(m.Users, *user)
	return nil
}

func (m *MockDatabase) UpdateUserLastLogin(userID int) error {
	return nil
}

// Session operations (not needed for poller tests, stub implementations)
func (m *MockDatabase) CreateSession(session *db.Session) error {
	return nil
}

func (m *MockDatabase) GetSession(id string) (*db.Session, error) {
	return nil, nil
}

func (m *MockDatabase) DeleteSession(id string) error {
	return nil
}

// User-PR view operations
func (m *MockDatabase) GetUserPRAssignment(userID, prID int) (*db.UserPRAssignment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := fmt.Sprintf("%d/%d", userID, prID)
	if a, ok := m.UserPRAssignments[key]; ok {
		return a, nil
	}
	return nil, nil
}

func (m *MockDatabase) UpsertUserPRAssignment(assignment *db.UserPRAssignment) error {
	return nil
}

func (m *MockDatabase) GetPRsForUser(userID int) ([]db.PR, error) {
	return nil, nil
}

func (m *MockDatabase) GetPRsForUserWithNotes(userID int) ([]db.PRWithUserView, error) {
	return nil, nil
}

func (m *MockDatabase) UpdateUserPRNotes(userID, prID int, notes string) error {
	return nil
}

func (m *MockDatabase) UpdateUserReviewStatus(userID, prID int, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.UpdateUserReviewStatusCalls = append(m.UpdateUserReviewStatusCalls, struct {
		UserID int
		PRID   int
		Status string
	}{userID, prID, status})
	return nil
}

func (m *MockDatabase) UpdateUserViaTeams(userID, prID int, viaTeams []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.UpdateUserViaTeamsCalls = append(m.UpdateUserViaTeamsCalls, struct {
		UserID   int
		PRID     int
		ViaTeams []string
	}{userID, prID, viaTeams})
	return nil
}

func (m *MockDatabase) DeleteAllUserPRViews(userID int) (int64, error) {
	return 0, nil
}

func (m *MockDatabase) HidePRForUser(userID, prID int) error {
	return nil
}

func (m *MockDatabase) SetUserHiddenForPR(userID, prID int, hidden bool) error {
	return nil
}

func (m *MockDatabase) EnsureUserPRView(userID, prID int, isAuthor bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.EnsureUserPRViewCalls = append(m.EnsureUserPRViewCalls, struct {
		UserID   int
		PRID     int
		IsAuthor bool
	}{userID, prID, isAuthor})
	return nil
}

func (m *MockDatabase) EnsureManualPRView(userID, prID int, isAuthor bool) error {
	return nil
}

func (m *MockDatabase) HideNonManualViewsForPR(prID int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.HideNonManualViewsCalls = append(m.HideNonManualViewsCalls, prID)
	return nil
}

func (m *MockDatabase) SetPRState(owner, repo string, prNumber int, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pr, ok := m.PRs[prDBKey(owner, repo, prNumber)]; ok {
		pr.PRState = state
	}
	return nil
}

func (m *MockDatabase) GetPRIDsWithManualClaims() (map[int]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	claims := make(map[int]bool, len(m.ManualClaimPRIDs))
	for _, id := range m.ManualClaimPRIDs {
		claims[id] = true
	}
	return claims, nil
}

func (m *MockDatabase) BatchUpsertPRs(prs []*db.PR) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pr := range prs {
		key := prDBKey(pr.RepoOwner, pr.RepoName, pr.PRNumber)
		m.PRs[key] = pr
	}
	return nil
}

func (m *MockDatabase) BatchUpsertUserPRViews(items []db.UserPRViewBatchItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Decompose into individual call trackers for backward compatibility
	for _, item := range items {
		m.EnsureUserPRViewCalls = append(m.EnsureUserPRViewCalls, struct {
			UserID   int
			PRID     int
			IsAuthor bool
		}{item.UserID, item.PRID, item.IsAuthor})
		if item.ReviewStatus != nil {
			m.UpdateUserReviewStatusCalls = append(m.UpdateUserReviewStatusCalls, struct {
				UserID int
				PRID   int
				Status string
			}{item.UserID, item.PRID, *item.ReviewStatus})
		}
		if item.ViaTeams != nil {
			m.UpdateUserViaTeamsCalls = append(m.UpdateUserViaTeamsCalls, struct {
				UserID   int
				PRID     int
				ViaTeams []string
			}{item.UserID, item.PRID, *item.ViaTeams})
		}

		// Mirror the real upsert semantics into the view store: every upsert
		// un-hides the row; optional fields only overwrite when set.
		key := viewMockKey(item.UserID, item.PRID)
		view, exists := m.UserPRViews[key]
		if !exists {
			view = &db.UserPRView{UserID: item.UserID, PRID: item.PRID, ViaTeams: "[]"}
			m.UserPRViews[key] = view
		}
		view.IsAuthor = item.IsAuthor
		view.Hidden = false
		if item.ReviewStatus != nil {
			view.ReviewStatus = *item.ReviewStatus
		}
		if item.ViaTeams != nil {
			if bytes, err := json.Marshal(*item.ViaTeams); err == nil {
				view.ViaTeams = string(bytes)
			}
		}
	}
	return nil
}

func viewMockKey(userID, prID int) string {
	return fmt.Sprintf("%d/%d", userID, prID)
}

func (m *MockDatabase) GetUserPRViewsWithViaTeams(prIDs []int) ([]db.UserPRView, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	idSet := make(map[int]bool, len(prIDs))
	for _, id := range prIDs {
		idSet[id] = true
	}
	var views []db.UserPRView
	for _, view := range m.UserPRViews {
		if !idSet[view.PRID] {
			continue
		}
		if view.ViaTeams == "" || view.ViaTeams == "[]" || view.ViaTeams == "null" {
			continue
		}
		views = append(views, *view)
	}
	return views, nil
}

// TryAcquireOrRenewLeadership: nil func = always leader (the default for tests).
func (m *MockDatabase) TryAcquireOrRenewLeadership(holderID string, ttl time.Duration) (bool, error) {
	if m.TryAcquireOrRenewLeadershipFunc != nil {
		return m.TryAcquireOrRenewLeadershipFunc(holderID, ttl)
	}
	return true, nil
}

func (m *MockDatabase) BatchPruneViaTeams(prunes []db.ViaTeamsPrune) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BatchPruneViaTeamsCalls = append(m.BatchPruneViaTeamsCalls, prunes)
	for _, p := range prunes {
		if view, ok := m.UserPRViews[viewMockKey(p.UserID, p.PRID)]; ok {
			view.ViaTeams = "[]"
			if p.Hide {
				view.Hidden = true
			}
		}
	}
	return nil
}

func (m *MockDatabase) MigrateLegacyNotes(userID int) (int, error) {
	return 0, nil
}

func (m *MockDatabase) CreateTelemetryEvents(events []db.TelemetryEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.TelemetryEvents = append(m.TelemetryEvents, events...)
	return nil
}

func (m *MockDatabase) GetTelemetryStats(days int) (*db.TelemetryStats, error) {
	return &db.TelemetryStats{}, nil
}

func (m *MockDatabase) CreateReviewRun(run *db.ReviewRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if run == nil {
		return fmt.Errorf("create review run: run is nil")
	}
	if run.RunID == "" || run.RepoOwner == "" || run.RepoName == "" || run.PRNumber <= 0 || run.CommitSHA == "" ||
		run.TriggerSource == "" || run.Status == "" || run.RequestedConfigJSON == "" ||
		run.EffectiveConfigJSON == "" || run.ConfigSourcesJSON == "" || run.AcceptedAt.IsZero() || run.QueuedAt.IsZero() {
		return fmt.Errorf("create review run %s: required ledger fields are missing", run.RunID)
	}
	if (run.PRID != nil && *run.PRID <= 0) || (run.RequestedByUserID != nil && *run.RequestedByUserID <= 0) {
		return fmt.Errorf("create review run %s: optional database IDs must be positive", run.RunID)
	}
	if (run.IdempotencyScope == "") != (run.IdempotencyKeyHash == "") {
		return fmt.Errorf("create review run %s: idempotency scope and key hash must be set together", run.RunID)
	}
	if _, exists := m.ReviewRuns[run.RunID]; exists {
		return fmt.Errorf("%w: run_id=%s", db.ErrReviewRunConflict, run.RunID)
	}
	if run.IdempotencyKeyHash != "" {
		for _, existing := range m.ReviewRuns {
			if existing.IdempotencyScope == run.IdempotencyScope && existing.IdempotencyKeyHash == run.IdempotencyKeyHash {
				return fmt.Errorf("%w: run_id=%s", db.ErrReviewRunConflict, run.RunID)
			}
		}
	}
	copy := *run
	m.ReviewRuns[run.RunID] = &copy
	return nil
}

func (m *MockDatabase) GetReviewRun(runID string) (*db.ReviewRun, error) {
	if m.GetReviewRunFunc != nil {
		return m.GetReviewRunFunc(runID)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	run := m.ReviewRuns[runID]
	if run == nil {
		return nil, nil
	}
	copy := *run
	return &copy, nil
}

func (m *MockDatabase) GetReviewRunByIdempotency(scope, keyHash string) (*db.ReviewRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if scope == "" || keyHash == "" {
		return nil, nil
	}
	for _, run := range m.ReviewRuns {
		if run.IdempotencyScope == scope && run.IdempotencyKeyHash == keyHash {
			copy := *run
			return &copy, nil
		}
	}
	return nil, nil
}

func (m *MockDatabase) ListReviewRuns(filter db.ReviewRunFilter) ([]db.ReviewRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var runs []db.ReviewRun
	for _, run := range m.ReviewRuns {
		if filter.RepoOwner != "" && run.RepoOwner != filter.RepoOwner {
			continue
		}
		if filter.RepoName != "" && run.RepoName != filter.RepoName {
			continue
		}
		if filter.PRNumber > 0 && run.PRNumber != filter.PRNumber {
			continue
		}
		if filter.CommitSHA != "" && run.CommitSHA != filter.CommitSHA {
			continue
		}
		if filter.Status != "" && run.Status != filter.Status {
			continue
		}
		runs = append(runs, *run)
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].AcceptedAt.Equal(runs[j].AcceptedAt) {
			return runs[i].RunID > runs[j].RunID
		}
		return runs[i].AcceptedAt.After(runs[j].AcceptedAt)
	})
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

func (m *MockDatabase) PatchReviewRun(runID string, patch db.ReviewRunPatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.ReviewRuns[runID]
	if run == nil {
		return fmt.Errorf("review run %s not found", runID)
	}
	return m.patchReviewRunLocked(run, patch)
}

func (m *MockDatabase) PatchQueuedReviewRun(runID string, patch db.ReviewRunPatch) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.ReviewRuns[runID]
	if run == nil || run.Status != db.ReviewRunStatusQueued {
		return false, nil
	}
	if err := m.patchReviewRunLocked(run, patch); err != nil {
		return false, err
	}
	return true, nil
}

func (m *MockDatabase) patchReviewRunLocked(run *db.ReviewRun, patch db.ReviewRunPatch) error {
	if patch == (db.ReviewRunPatch{}) {
		return fmt.Errorf("patch review run %s: patch is empty", run.RunID)
	}
	if patch.Status != nil {
		run.Status = *patch.Status
	}
	if patch.StartedAt != nil {
		run.StartedAt = patch.StartedAt
	}
	if patch.CompletedAt != nil {
		run.CompletedAt = patch.CompletedAt
	}
	if patch.DurationMS != nil {
		run.DurationMS = *patch.DurationMS
	}
	if patch.HTMLPath != nil {
		run.HTMLPath = *patch.HTMLPath
	}
	if patch.JSONPath != nil {
		run.JSONPath = *patch.JSONPath
	}
	if patch.CriticalCount != nil {
		run.CriticalCount = *patch.CriticalCount
	}
	if patch.MediumCount != nil {
		run.MediumCount = *patch.MediumCount
	}
	if patch.LowCount != nil {
		run.LowCount = *patch.LowCount
	}
	if patch.Verdict != nil {
		run.Verdict = *patch.Verdict
	}
	if patch.ModelFallback != nil {
		run.ModelFallback = *patch.ModelFallback
	}
	if patch.ServingModelVerification != nil {
		run.ServingModelVerification = *patch.ServingModelVerification
	}
	if patch.ActualModelsJSON != nil {
		run.ActualModelsJSON = *patch.ActualModelsJSON
	}
	if patch.PublicationStatus != nil {
		run.PublicationStatus = *patch.PublicationStatus
	}
	if patch.TerminalCode != nil {
		run.TerminalCode = *patch.TerminalCode
	}
	if patch.FailureStage != nil {
		run.FailureStage = *patch.FailureStage
	}
	if patch.ErrorSummary != nil {
		run.ErrorSummary = *patch.ErrorSummary
	}
	if patch.LeaseHolder != nil {
		run.LeaseHolder = *patch.LeaseHolder
	}
	if patch.LeaseExpiresAt != nil {
		if patch.LeaseExpiresAt.IsZero() {
			run.LeaseExpiresAt = nil
		} else {
			run.LeaseExpiresAt = patch.LeaseExpiresAt
		}
	}
	if patch.ExecutionAttempt != nil {
		run.ExecutionAttempt = *patch.ExecutionAttempt
	}
	return nil
}

func (m *MockDatabase) PatchReviewRunAsHolder(runID, holder string, now time.Time, patch db.ReviewRunPatch) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.ReviewRuns[runID]
	if run == nil || run.Status != db.ReviewRunStatusRunning || run.LeaseHolder != holder ||
		run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now) {
		return false, nil
	}
	if err := m.patchReviewRunLocked(run, patch); err != nil {
		return false, err
	}
	return true, nil
}

func (m *MockDatabase) ClaimReviewRun(runID, holder string, now, leaseExpiresAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.ReviewRuns[runID]
	if run == nil {
		return false, nil
	}
	claimable := run.Status == db.ReviewRunStatusQueued ||
		(run.Status == db.ReviewRunStatusRunning && (run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now)))
	if !claimable {
		return false, nil
	}
	run.Status = db.ReviewRunStatusRunning
	if run.StartedAt == nil {
		started := now
		run.StartedAt = &started
	}
	run.LeaseHolder = holder
	expires := leaseExpiresAt
	run.LeaseExpiresAt = &expires
	run.ExecutionAttempt++
	run.CompletedAt = nil
	run.DurationMS = 0
	run.HTMLPath = ""
	run.JSONPath = ""
	run.CriticalCount = 0
	run.MediumCount = 0
	run.LowCount = 0
	run.Verdict = ""
	run.ModelFallback = false
	run.ServingModelVerification = ""
	run.ActualModelsJSON = ""
	run.PublicationStatus = ""
	run.TerminalCode = ""
	run.FailureStage = ""
	run.ErrorSummary = ""
	return true, nil
}

func (m *MockDatabase) ClaimOrRenewQueuedReviewRunLease(runID, holder string, now, leaseExpiresAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runID == "" || holder == "" || now.IsZero() || !leaseExpiresAt.After(now) {
		return false, fmt.Errorf("claim queued review run lease: invalid arguments")
	}
	run := m.ReviewRuns[runID]
	if run == nil || run.Status != db.ReviewRunStatusQueued ||
		(run.LeaseHolder != "" && run.LeaseHolder != holder && run.LeaseExpiresAt != nil && run.LeaseExpiresAt.After(now)) {
		return false, nil
	}
	run.LeaseHolder = holder
	expires := leaseExpiresAt
	run.LeaseExpiresAt = &expires
	return true, nil
}

func (m *MockDatabase) RenewReviewRunLease(runID, holder string, now, leaseExpiresAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.ReviewRuns[runID]
	if run == nil || run.Status != db.ReviewRunStatusRunning || run.LeaseHolder != holder ||
		run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now) {
		return false, nil
	}
	expires := leaseExpiresAt
	run.LeaseExpiresAt = &expires
	return true, nil
}

func (m *MockDatabase) AbandonExpiredReviewRuns(now time.Time, runningGrace, queuedMaxAge time.Duration) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if now.IsZero() || runningGrace < 0 || queuedMaxAge <= 0 {
		return 0, fmt.Errorf("abandon expired review runs: invalid arguments")
	}
	runningCutoff := now.Add(-runningGrace)
	queuedCutoff := now.Add(-queuedMaxAge)
	abandoned := 0
	for _, run := range m.ReviewRuns {
		terminalCode := ""
		failureStage := ""
		errorSummary := ""
		switch {
		case run.Status == db.ReviewRunStatusRunning && run.LeaseExpiresAt != nil && !run.LeaseExpiresAt.After(runningCutoff):
			terminalCode = "lease_abandoned"
			failureStage = "execution"
			errorSummary = "review worker lease expired before terminal completion"
		case run.Status == db.ReviewRunStatusQueued &&
			((run.LeaseExpiresAt != nil && !run.LeaseExpiresAt.After(now)) ||
				(run.LeaseExpiresAt == nil && !run.QueuedAt.After(queuedCutoff))):
			terminalCode = "queue_abandoned"
			failureStage = "dispatch"
			errorSummary = "review run remained queued beyond the dispatch recovery window"
		default:
			continue
		}
		completedAt := now
		run.Status = db.ReviewRunStatusTimedOut
		run.CompletedAt = &completedAt
		run.TerminalCode = terminalCode
		run.FailureStage = failureStage
		run.ErrorSummary = errorSummary
		run.LeaseHolder = ""
		run.LeaseExpiresAt = nil
		abandoned++
	}
	return abandoned, nil
}

func (m *MockDatabase) UpsertReviewStageAttempt(attempt *db.ReviewStageAttempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if attempt == nil || attempt.RunID == "" || attempt.ExecutionAttempt <= 0 || attempt.Stage == "" || attempt.InvocationNumber <= 0 || attempt.AttemptNumber <= 0 {
		return fmt.Errorf("upsert review stage attempt: required key fields are missing")
	}
	attempts := m.ReviewStageAttempts[attempt.RunID]
	for i := range attempts {
		if attempts[i].ExecutionAttempt == attempt.ExecutionAttempt && attempts[i].Stage == attempt.Stage && attempts[i].InvocationNumber == attempt.InvocationNumber && attempts[i].AttemptNumber == attempt.AttemptNumber {
			attempts[i] = *attempt
			m.ReviewStageAttempts[attempt.RunID] = attempts
			return nil
		}
	}
	m.ReviewStageAttempts[attempt.RunID] = append(attempts, *attempt)
	return nil
}

func (m *MockDatabase) ListReviewStageAttempts(runID string) ([]db.ReviewStageAttempt, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	attempts := append([]db.ReviewStageAttempt(nil), m.ReviewStageAttempts[runID]...)
	sort.Slice(attempts, func(i, j int) bool {
		if attempts[i].ExecutionAttempt != attempts[j].ExecutionAttempt {
			return attempts[i].ExecutionAttempt < attempts[j].ExecutionAttempt
		}
		if attempts[i].Stage != attempts[j].Stage {
			return attempts[i].Stage < attempts[j].Stage
		}
		if attempts[i].InvocationNumber != attempts[j].InvocationNumber {
			return attempts[i].InvocationNumber < attempts[j].InvocationNumber
		}
		return attempts[i].AttemptNumber < attempts[j].AttemptNumber
	})
	return attempts, nil
}

func (m *MockDatabase) Close() error {
	return nil
}

// MockReviewStorage implements ReviewStorage for testing
type MockReviewStorage struct {
	// ExistingReviews maps "owner/repo/number/sha" to existence
	ExistingReviews map[string]bool

	// SavedReviews tracks saved reviews for verification
	SavedReviews map[string][]byte

	// Error injection
	ReviewExistsError error
	ReviewExistsFunc  func(context.Context, string, string, int, string) (bool, error)
	SaveReviewError   error
	SaveReviewFunc    func(context.Context, string, string, int, string, []byte) (string, error)

	// Track calls
	ReviewExistsCalls []struct {
		Owner     string
		Repo      string
		PRNumber  int
		CommitSHA string
	}
	SaveReviewCalls []struct {
		Owner     string
		Repo      string
		PRNumber  int
		CommitSHA string
		Content   []byte
	}

	// Mutex for thread safety
	mu sync.Mutex
}

func NewMockReviewStorage() *MockReviewStorage {
	return &MockReviewStorage{
		ExistingReviews: make(map[string]bool),
		SavedReviews:    make(map[string][]byte),
	}
}

func (m *MockReviewStorage) ReviewExists(ctx context.Context, owner, repo string, prNumber int, commitSHA string) (bool, error) {
	m.mu.Lock()
	key := fmt.Sprintf("%s/%s/%d/%s", owner, repo, prNumber, commitSHA)
	m.ReviewExistsCalls = append(m.ReviewExistsCalls, struct {
		Owner     string
		Repo      string
		PRNumber  int
		CommitSHA string
	}{owner, repo, prNumber, commitSHA})

	fn := m.ReviewExistsFunc
	err := m.ReviewExistsError
	exists := m.ExistingReviews[key]
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, owner, repo, prNumber, commitSHA)
	}
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (m *MockReviewStorage) SaveReview(ctx context.Context, owner, repo string, prNumber int, commitSHA string, content []byte) (string, error) {
	m.mu.Lock()
	key := fmt.Sprintf("%s/%s/%d/%s", owner, repo, prNumber, commitSHA)
	m.SaveReviewCalls = append(m.SaveReviewCalls, struct {
		Owner     string
		Repo      string
		PRNumber  int
		CommitSHA string
		Content   []byte
	}{owner, repo, prNumber, commitSHA, content})
	saveErr := m.SaveReviewError
	saveFunc := m.SaveReviewFunc
	m.mu.Unlock()

	if saveFunc != nil {
		return saveFunc(ctx, owner, repo, prNumber, commitSHA, content)
	}
	if saveErr != nil {
		return "", saveErr
	}

	m.mu.Lock()
	m.SavedReviews[key] = content
	m.mu.Unlock()
	return fmt.Sprintf("review-%s-%s-%d-%s.html", owner, repo, prNumber, commitSHA[:7]), nil
}

// SaveReviewSidecar is a no-op for the mock — tests that need to inspect the
// sidecar can extend this struct, but the default behavior is to swallow it
// since the poller treats sidecar writes as best-effort.
func (m *MockReviewStorage) SaveReviewSidecar(ctx context.Context, filename, contentType string, content []byte) error {
	return nil
}

// MockReviewGenerator implements ReviewGenerator for testing
type MockReviewGenerator struct {
	// Results maps "owner/repo/number" to (result, error)
	Results map[string]struct {
		Result *ReviewResult
		Err    error
	}

	// DefaultResult is returned if no specific result is set
	DefaultResult *ReviewResult

	// Track calls
	GenerateReviewCalls []ReviewGeneratorConfig
	mu                  sync.Mutex

	// Simulate delay
	SimulateDelay time.Duration
}

func NewMockReviewGenerator() *MockReviewGenerator {
	return &MockReviewGenerator{
		Results: make(map[string]struct {
			Result *ReviewResult
			Err    error
		}),
		DefaultResult: &ReviewResult{
			HTMLContent:   []byte("<html><body>Mock review</body></html>"),
			CriticalCount: 0,
			MediumCount:   1,
			LowCount:      2,
		},
	}
}

func (m *MockReviewGenerator) GenerateReview(ctx context.Context, cfg ReviewGeneratorConfig) (*ReviewResult, error) {
	m.mu.Lock()
	m.GenerateReviewCalls = append(m.GenerateReviewCalls, cfg)
	m.mu.Unlock()

	if m.SimulateDelay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(m.SimulateDelay):
		}
	}

	key := fmt.Sprintf("%s/%s/%d", cfg.Owner, cfg.RepoName, cfg.PRNumber)
	if result, ok := m.Results[key]; ok {
		return cloneMockReviewResult(result.Result), result.Err
	}

	return cloneMockReviewResult(m.DefaultResult), nil
}

// cloneMockReviewResult models the production generator contract: each call
// returns an independently owned result. The poller attaches run metadata to
// that result, so sharing the default pointer across concurrent calls creates
// an artificial race that cannot occur with real review generation.
func cloneMockReviewResult(result *ReviewResult) *ReviewResult {
	if result == nil {
		return nil
	}
	cloned := *result
	if result.ReviewRun != nil {
		reviewRun := *result.ReviewRun
		reviewRun.Models = append([]payload.ModelUse(nil), result.ReviewRun.Models...)
		cloned.ReviewRun = &reviewRun
	}
	return &cloned
}

// testConfig returns a minimal config for testing
func testConfig() *config.Config {
	return &config.Config{
		GitHubUsername:  "testuser",
		GitHubToken:     "test-token",
		PollingInterval: time.Minute,
		ReviewsDir:      "/tmp/test-reviews",
	}
}

// newTestPoller creates a Poller with mock dependencies for testing
func newTestPoller(mockGH *MockGitHubClient, mockDB *MockDatabase) *Poller {
	return &Poller{
		cfg:           testConfig(),
		db:            mockDB,
		ghClient:      mockGH,
		reviewDir:     "/tmp/test-reviews",
		activeReviews: make(map[string]ProcessInfo),
	}
}

// newTestPollerWithStorage creates a Poller with mock dependencies including storage
func newTestPollerWithStorage(mockGH *MockGitHubClient, mockDB *MockDatabase, mockStorage *MockReviewStorage) *Poller {
	p := &Poller{
		cfg:           testConfig(),
		db:            mockDB,
		ghClient:      mockGH,
		reviewDir:     "/tmp/test-reviews",
		activeReviews: make(map[string]ProcessInfo),
	}
	p.storage = mockStorage
	return p
}

// newTestPollerFull creates a Poller with all mock dependencies
func newTestPollerFull(mockGH *MockGitHubClient, mockDB *MockDatabase, mockStorage *MockReviewStorage, mockGenerator *MockReviewGenerator) *Poller {
	p := &Poller{
		cfg:           testConfig(),
		db:            mockDB,
		ghClient:      mockGH,
		reviewDir:     "/tmp/test-reviews",
		activeReviews: make(map[string]ProcessInfo),
	}
	p.storage = mockStorage
	p.reviewGenerator = mockGenerator
	return p
}
