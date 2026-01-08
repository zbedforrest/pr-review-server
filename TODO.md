# PR Review Server - TODO List

## Project Overview
A local server that monitors GitHub PRs where you're a requested reviewer, automatically generates reviews using cbpr, and provides a web frontend to view them.

## Current Status

### What's Working
The core infrastructure of the PR review server is complete and functional:

**Backend Services:**
- GitHub API integration successfully fetches PRs where you're a requested reviewer (fixed initial bug where `issue.Repository` was nil by parsing from `issue.GetRepositoryURL()`)
- Polling service runs every minute checking for new PRs and commit changes
- SQLite database tracks PR states with status field (pending/generating/completed/error)
- HTTP server provides JSON API endpoints and serves embedded frontend
- Status tracking prevents duplicate review generation and shows progress in real-time
- Delete functionality removes both database entries and HTML files

**Frontend Dashboard:**
- Auto-refreshes every 30 seconds
- Shows ALL PRs requesting your review (not just ones with completed reviews)
- Displays status badges with pulsing animation for "generating" state
- Shows "Not yet reviewed" in yellow text
- Delete buttons with confirmation dialogs
- Links to GitHub PRs and review HTML files

**cbpr Integration:**
- Modified cbpr to support `--output` flag for direct file path specification
- Modified cbpr to support `--no-open` flag to prevent browser tabs from opening
- Server uses `--fast` flag for development speed
- Command execution with proper context cancellation

### Current Blocker

**HTML Files Not Being Created:**
The server logs show "Successfully generated review" messages, but the review HTML files are not appearing in the `~/pr-review-server/reviews/` directory. The symptoms are:

1. cbpr command executes without error (cmd.Run() returns nil)
2. Server logs show: "cbpr succeeded but file not created at /Users/zbedmm/pr-review-server/reviews/multimediallc_chaturbate_XXXX.html"
3. The reviews directory remains empty

**Diagnosis attempts:**
- Added absolute path conversion: `filepath.Abs(p.reviewDir)` to ensure correct directory
- Added debug logging to see exact command and paths being used
- Added file existence verification after cbpr runs

**Next debugging step:** Need to manually test cbpr with the `--output` flag to verify it actually creates files when called directly. This will determine if the bug is in cbpr's implementation or in how the server is calling it (environment, working directory, permissions, etc.).

### What's Left

**Critical (Blocking):**
1. **Fix HTML file generation** - Resolve why cbpr's `--output` flag isn't creating files
   - Test cbpr manually with `--output` flag
   - Check cbpr's code to ensure it properly handles the output path parameter
   - Verify file permissions and directory creation
   - Check if cbpr's working directory affects output path resolution

**High Priority:**
2. **PR title wrapping** - Update frontend CSS so PR titles wrap instead of getting cut off (user wants to read full titles)
3. **Automatic cleanup** - Implement deletion of HTML files for PRs that no longer request your review (when PR is closed/merged or review request removed)

**Medium Priority:**
4. **Error handling improvements** - Better error messages when cbpr fails
5. **Performance optimization** - Consider running multiple cbpr reviews in parallel instead of sequentially
6. **Testing** - End-to-end testing once HTML generation is fixed

**Nice to Have:**
7. **Persistence** - Add systemd/launchd service file for automatic startup
8. **Notifications** - Desktop notifications when new PRs request review
9. **Metrics** - Track review generation times, success rates

## Architecture

**Tech Stack:**
- Backend: Go with google/go-github/v57
- Database: SQLite
- Frontend: HTML/JS (embedded in binary)
- GitHub API integration with OAuth2

**Database Schema:**
```sql
CREATE TABLE prs (
  id INTEGER PRIMARY KEY,
  repo_owner TEXT,
  repo_name TEXT,
  pr_number INTEGER,
  last_commit_sha TEXT,
  last_reviewed_at TIMESTAMP,
  review_html_path TEXT,
  status TEXT DEFAULT 'pending',
  UNIQUE(repo_owner, repo_name, pr_number)
);
```

**Key Components:**
1. `config/` - Load configuration from environment variables
2. `db/` - SQLite database layer with migration support
3. `github/` - GitHub API client for fetching PRs
4. `poller/` - Polling service that triggers cbpr reviews
5. `server/` - HTTP server with API endpoints and embedded frontend
6. `main.go` - Application entry point

**cbpr Modifications:**
Located at `~/cbpr` on branch `feature/clickable-pr-link`:
- Added `--output` flag to specify output file path
- Added `--no-open` flag to prevent browser opening
- Modified `review/report.go` to write directly to specified path without temp files

## Configuration

Required environment variables (set in .zshrc):
- `GITHUB_TOKEN` - GitHub personal access token
- `GITHUB_USERNAME` - Your GitHub username (zbedforrest)
- `POLLING_INTERVAL` - How often to check for PRs (default: 1m)
- `SERVER_PORT` - Port for web server (7769)
- `CBPR_PATH` - Path to cbpr binary (~/cbpr/cbpr)

## Running the Server

```bash
cd ~/pr-review-server
go run main.go
```

Then visit: http://localhost:7769

## Next Immediate Actions

1. Test cbpr manually: `~/cbpr/cbpr review --repo-name=multimediallc/chaturbate -p 24843 --fast --output=/tmp/test-review.html`
2. Verify file was created: `ls -lh /tmp/test-review.html`
3. If file exists: Bug is in server's cbpr invocation (environment/permissions)
4. If file doesn't exist: Bug is in cbpr's `--output` implementation
5. Fix the identified issue
6. Test end-to-end with actual PR reviews
7. Update frontend CSS for title wrapping
8. Implement automatic cleanup of old reviews
