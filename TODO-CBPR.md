# CBPR Review Items - PR #2

## CRITICAL Issues

### 1. API Batching Performance in Prioritization
**File**: `prioritization/prioritizer.go` (lines 176, 180, 191)
**Issue**: `batchFetchPRDetails` makes N+2 sequential REST API calls per PR (one `GetPR` + one `ListReviews`), which will quickly consume GitHub API rate limits.
**Fix**: Refactor to use a single GraphQL query to batch-fetch all PR details (additions, deletions, changedFiles, createdAt, reviews, requestedReviewers) similar to `BatchGetPRReviewData` in the `github` package.

### 2. N+1 Database Queries in Poll Loop
**File**: `poller/poller.go` (line 636)
**Issue**: Loop makes a `GetPR` call to the database for every PR, even though `dbPRsForReviewUpdate` (all PRs) was just fetched.
**Fix**: Create a map of DB PRs keyed by `owner/repo/number` before the loop, then look up from the map instead of hitting the database repeatedly.

## MEDIUM Issues

### 3. Dockerfile Build Optimization
**File**: `Dockerfile` (line 29)
**Issue**: Using `-a -installsuffix cgo` flags that are largely unnecessary with modern Go. Missing `-ldflags="-w -s"` to strip debug info and reduce binary size.
**Fix**: Change to: `RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-w -s" -o pr-review-server .`

### 4. SQLite Error Handling Brittleness
**File**: `db/db.go` (line 97)
**Issue**: Using `strings.Contains` to check for database errors is brittle and could break if error messages change across SQLite/driver versions.
**Fix**: Type-assert error to `sqlite3.Error` and check specific error codes (e.g., `sqlite3.SQLITE_CONSTRAINT`).

### 5. Duplicate Database Scanning Logic
**File**: `db/db.go` (line 118)
**Issue**: Logic for scanning a database row into a `PR` struct is duplicated across `GetPR`, `GetAllPRs`, and `GetPRsWithMissingMetadata`.
**Fix**: Extract a private helper function to centralize scanning logic, reducing code duplication and maintenance burden.

## Status
- [x] CRITICAL: API batching performance (commit 3ad82e0)
- [x] CRITICAL: N+1 database queries (commit 08dac72)
- [x] CRITICAL: Reduce polling intervals (commit e4ba62d)
- [x] CRITICAL: Add unit tests for prioritization (commit c1d189c)
- [x] MEDIUM: Dockerfile build optimization (commit 61ad914)
- [x] MEDIUM: SQLite error handling (commit 915b162)
- [x] MEDIUM: Duplicate scanning logic (commit bf7b503)

## Remaining Issues (Low Priority)
- Batch update loop atomicity: Wrap review data updates in transaction (would require db.Begin/Commit/Rollback methods)
- Various LOW priority suggestions (code style, minor DRY improvements)
