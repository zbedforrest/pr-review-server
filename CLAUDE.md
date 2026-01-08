# PR Review Server - Important Notes for Claude

## Critical Issues Discovered and Fixed

### 1. CBPR File Output Behavior
**Issue**: cbpr does NOT respect the working directory when using `--html --no-open` flags. It writes files to a temp directory (`/var/folders/.../T/review-*.html`) instead of the current working directory.

**Solution**: Always use the `--output` flag to explicitly specify the file path:
```bash
cbpr review --repo-name=owner/repo -n 3 -p 123 --output=/path/to/file.html
```

**DO NOT** rely on `cmd.Dir` or working directory changes with cbpr.

### 2. Batch Processing Issues
**Issue**: Processing too many PRs at once (20+) causes cbpr to timeout (>5 minutes), triggering the monitor to kill the process.

**Solution**: Process PRs in batches of 5 or fewer. The `processInBatches()` function handles this.

### 3. PID Tracking and "Generating" Status
**Issue**: PRs can be marked as "generating" even when the associated cbpr process has died, been killed, or never existed.

**Requirements**:
- When marking a PR as "generating", you MUST have a valid PID being tracked
- Before considering a PR as "generating", verify the PID is still a running process
- If a process dies or gets killed, the PR should be marked as "error" or reset to "pending"

**Current State**: The monitor tracks one cbpr PID at a time in `p.cbprPID`. Each PR processed sequentially updates this PID.

### 4. Self-Healing Mechanisms Implemented
The server now has automatic recovery:

1. **Stale PR Reset** (`ResetStaleGeneratingPRs`): PRs stuck in "generating" for >2 minutes are reset to "pending"
2. **Error PR Retry** (`ResetErrorPRs`): PRs in "error" state for >5 minutes are reset to "pending" for automatic retry
3. **Partial Success Recovery**: If cbpr completes but some files weren't created, only missing files are marked as errors (not the entire batch)
4. **Closed PR Cleanup** (`cleanupClosedPRs`): Every poll cycle checks all tracked PRs on GitHub and removes any that are closed/merged, deleting both database entry and HTML file. If a closed PR is reopened, it will be picked up again on the next poll.
5. **Outdated Review Detection** (`checkForOutdatedReviews`): Every poll cycle checks all completed PRs to see if new commits have been pushed. Compares the stored commit SHA against GitHub's current HEAD SHA. When a mismatch is detected, the PR is reset to "pending" status and will be re-reviewed automatically with the latest changes.

### 5. Database Schema
The `prs` table tracks:
- `status`: "pending", "generating", "completed", or "error"
- `generating_since`: Timestamp when generation started (used to detect stale processes)
- `is_mine`: Distinguishes between PRs to review vs your own PRs
- `last_reviewed_at`: When the review was last updated

### 6. Immediate Database Updates (CRITICAL)
**Issue**: Originally, PRs were marked as "generating" at the start of a batch and only updated to "completed" after ALL PRs in the batch finished. This caused PRs to appear stuck in "generating" for 5-10+ minutes even though they had completed successfully.

**Solution**: Update database status **immediately** after each individual PR completes:
- Mark as "completed" right after file verification succeeds
- Mark as "error" right after cbpr fails or file is missing
- Don't wait for the entire batch to finish

**Why This Matters**: Provides real-time visibility into progress and prevents confusion about which PRs are actually being processed vs already done.

### 7. Processing Flow
1. Poll runs every 1 minute
2. Reset stale/error PRs first (self-healing)
3. Clean up closed PRs (remove from database/filesystem)
4. Backfill missing PR metadata (title/author)
5. **Check for outdated reviews** (detect PRs with new commits and reset to pending)
6. Fetch PRs from GitHub (both review requests and your own PRs)
7. Check database for pending PRs (ensures processing even when GitHub API fails)
8. Group PRs by repository
9. Split into batches of 5
10. Process each PR individually with `--output` flag
11. **Immediately** mark as completed/error after each PR (not after batch)
12. Continue to next PR in batch

## Environment Variables
Required:
- `GITHUB_TOKEN`: GitHub personal access token
- `GITHUB_USERNAME`: Your GitHub username

Optional:
- `POLLING_INTERVAL`: How often to check for PRs (default: 1m)
- `SERVER_PORT`: Port for web interface (default: 8080)
- `CBPR_PATH`: Path to cbpr binary (default: "cbpr" from PATH)

## Common Gotchas
1. cbpr uses Gemini by default (unless `--llm` is specified)
2. cbpr `-n 3` means 3 review requests to the LLM (for consensus/variation)
3. The monitor checks every 30 seconds and kills processes running >5 minutes
4. File verification happens AFTER cbpr completes, checking if expected HTML files exist
5. The `--no-open` flag prevents cbpr from opening files in browser, but it STILL writes to temp dir without `--output`

## Testing Tips
To test cbpr behavior manually:
```bash
cd /path/to/reviews
cbpr review --repo-name=owner/repo -n 1 -p 123 --html --no-open
# Check where it writes: it will log "HTML report generated at: /tmp/..."

# Correct way:
cbpr review --repo-name=owner/repo -n 1 -p 123 --output=/path/to/output.html
```

## Architecture Notes
- **Poller** (`poller/poller.go`): Fetches PRs and triggers reviews
- **Database** (`db/db.go`): SQLite storage for PR state
- **Server** (`server/server.go`): Web UI for viewing reviews
- **GitHub Client** (`github/client.go`): GitHub API interactions

The poller and server run concurrently. The poller updates the cache whenever it finds new PRs, and the server serves the cached data for fast dashboard loading.

## Data Integrity & Regression Prevention

### Problem: Missing Author/Title Fields (2026-01-08)
**What happened**: Database schema was extended with `title` and `author` columns, but existing PRs had empty values. When GitHub API failed, no new data came in, leaving the dashboard with empty author columns.

**Root causes**:
1. Database schema changes without data migration/backfill
2. No validation that required fields have data
3. Silent failures when GitHub API doesn't work

### Prevention Mechanisms Implemented

1. **Automatic Metadata Backfill** (`backfillPRMetadata`):
   - Runs on every poll cycle (self-healing)
   - Identifies PRs with missing title/author
   - Fetches data directly from GitHub PR API
   - Updates database immediately
   - Logs all backfill operations

2. **Fallback Values**:
   - API returns "Unknown" for empty author fields
   - API returns "PR #N" for empty titles
   - Ensures dashboard never shows completely blank fields

3. **Health Monitoring**:
   - `/api/status` endpoint includes `missing_metadata_count`
   - Alerts when PRs need metadata backfill
   - Helps diagnose GitHub API issues early

4. **Database Functions for Prevention**:
   - `GetPRsWithMissingMetadata()`: Identifies PRs needing backfill
   - `UpdatePRMetadata()`: Updates only title/author without affecting other fields
   - Prevents accidental overwrites during backfill

### Best Practices Going Forward
- **Schema changes**: Always add backfill logic for existing rows
- **Required fields**: Add NOT NULL constraints and defaults where appropriate
- **API dependencies**: Always have fallback values when external APIs fail
- **Monitoring**: Track data quality metrics in status endpoints
- **Self-healing**: Automatically fix data issues rather than requiring manual intervention
