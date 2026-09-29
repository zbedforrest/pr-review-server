package github

import (
	"errors"
	"fmt"
	"testing"
	"time"

	gh "github.com/google/go-github/v57/github"
)

func TestRateLimitedRecognizesRESTAndGraphQLLimits(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	reset := now.Add(10 * time.Minute)
	retry := 42 * time.Second
	cases := []struct {
		name  string
		err   error
		reset time.Time
		ok    bool
	}{
		{"primary", fmt.Errorf("get pr: %w", &gh.RateLimitError{Rate: gh.Rate{Reset: gh.Timestamp{Time: reset}}}), reset, true},
		{"secondary with retry-after", &gh.AbuseRateLimitError{RetryAfter: &retry}, now.Add(retry), true},
		{"secondary without retry-after", &gh.AbuseRateLimitError{}, time.Time{}, true},
		{"graphql", &GraphQLRateLimitError{ResetAt: reset}, reset, true},
		{"other error", errors.New("boom"), time.Time{}, false},
		{"nil", nil, time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := RateLimited(c.err, now)
		if ok != c.ok || !got.Equal(c.reset) {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", c.name, got, ok, c.reset, c.ok)
		}
	}
}
