package github

import (
	"errors"
	"time"

	gh "github.com/google/go-github/v57/github"
)

// RateLimited reports whether err is a GitHub primary or secondary rate limit
// from the REST or GraphQL API, and when it resets. The reset is zero when
// GitHub did not say.
func RateLimited(err error, now time.Time) (time.Time, bool) {
	var primary *gh.RateLimitError
	if errors.As(err, &primary) {
		return primary.Rate.Reset.Time, true
	}
	var secondary *gh.AbuseRateLimitError
	if errors.As(err, &secondary) {
		if secondary.RetryAfter != nil {
			return now.Add(*secondary.RetryAfter), true
		}
		return time.Time{}, true
	}
	if isCIRateLimitError(err) {
		return RateLimitResetAt(err), true
	}
	return time.Time{}, false
}
