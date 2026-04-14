package poller

import (
	"context"
	"fmt"
	"sync"
	"time"

	gh "github.com/google/go-github/v57/github"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
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

func (m *MockGitHubClient) BatchGetCIStatus(ctx context.Context, prs []struct {
	Owner, Repo string
	Number      int
	CommitSHA   string
}) (map[string]*github.CIStatus, error) {
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
	mu sync.RWMutex

	// PRs stored in the mock database (keyed by "owner/repo/number")
	PRs map[string]*db.PR

	// Settings
	AutoReviewEnabled bool
	ReviewNRequests   int

	// Track calls for verification
	DeletePRCalls []struct {
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

	EnsureUserPRViewCalls []struct {
		UserID   int
		PRID     int
		IsAuthor bool
	}

	// Users in the mock database
	Users []db.User

	// User PR assignments (keyed by "userID/prID")
	UserPRAssignments map[string]*db.UserPRAssignment

	// Error injection
	DeletePRError          error
	UpdatePRStatusError    error
	ResetPRToOutdatedError error
	GetAllPRsError         error
}

func NewMockDatabase() *MockDatabase {
	return &MockDatabase{
		PRs:               make(map[string]*db.PR),
		AutoReviewEnabled: true,
		ReviewNRequests:   3,
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

func (m *MockDatabase) ResetErrorPRs(maxAgeMinutes int) (int, error) {
	return 0, nil
}

func (m *MockDatabase) GetPRsWithMissingMetadata() ([]db.PR, error) {
	return nil, nil
}

func (m *MockDatabase) UpdatePRMetadata(owner, repo string, prNumber int, title, author string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := prDBKey(owner, repo, prNumber)
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
	return nil, nil
}

func (m *MockDatabase) GetAllUsers() ([]db.User, error) {
	if m.Users == nil {
		return []db.User{}, nil
	}
	return m.Users, nil
}

func (m *MockDatabase) CreateUser(user *db.User) error {
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

func (m *MockDatabase) HidePRForUser(userID, prID int) error {
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
	}
	return nil
}

func (m *MockDatabase) MigrateLegacyNotes(userID int) (int, error) {
	return 0, nil
}

func (m *MockDatabase) CreateTelemetryEvents(events []db.TelemetryEvent) error {
	return nil
}

func (m *MockDatabase) GetTelemetryStats(days int) (*db.TelemetryStats, error) {
	return &db.TelemetryStats{}, nil
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
	SaveReviewError   error

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
	defer m.mu.Unlock()

	key := fmt.Sprintf("%s/%s/%d/%s", owner, repo, prNumber, commitSHA)
	m.ReviewExistsCalls = append(m.ReviewExistsCalls, struct {
		Owner     string
		Repo      string
		PRNumber  int
		CommitSHA string
	}{owner, repo, prNumber, commitSHA})

	if m.ReviewExistsError != nil {
		return false, m.ReviewExistsError
	}

	exists := m.ExistingReviews[key]
	return exists, nil
}

func (m *MockReviewStorage) SaveReview(ctx context.Context, owner, repo string, prNumber int, commitSHA string, htmlContent []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := fmt.Sprintf("%s/%s/%d/%s", owner, repo, prNumber, commitSHA)
	m.SaveReviewCalls = append(m.SaveReviewCalls, struct {
		Owner     string
		Repo      string
		PRNumber  int
		CommitSHA string
		Content   []byte
	}{owner, repo, prNumber, commitSHA, htmlContent})

	if m.SaveReviewError != nil {
		return "", m.SaveReviewError
	}

	m.SavedReviews[key] = htmlContent
	// Return a filename similar to what the real implementation returns
	return fmt.Sprintf("review-%s-%s-%d-%s.html", owner, repo, prNumber, commitSHA[:7]), nil
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
		return result.Result, result.Err
	}

	return m.DefaultResult, nil
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
