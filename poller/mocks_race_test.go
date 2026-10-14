package poller

import (
	"sync"
	"testing"

	"pr-review-server/db"
)

func TestMockDatabase_UsersAreSafeForConcurrentPollAndReviewJobs(t *testing.T) {
	m := &MockDatabase{Users: []db.User{{ID: 1, GitHubUsername: "alice"}}}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = m.CreateUser(&db.User{GitHubUsername: "bot"})
		}
	}()
	for i := 0; i < 50; i++ {
		users, _ := m.GetAllUsers()
		for _, u := range users {
			_ = u.GitHubUsername
		}
	}
	wg.Wait()
}
