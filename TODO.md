# PR Review Server - TODO List

## Project Overview
A local server that monitors GitHub PRs where you're a requested reviewer, automatically generates reviews using cbpr, and provides a web frontend to view them.

## Tasks

- [ ] Create new project directory structure and initialize Go module
- [ ] Design SQLite schema for PRs (repo, number, last_commit_sha, last_reviewed_at)
- [ ] Implement GitHub API client to fetch PRs where user is requested reviewer
- [ ] Implement database layer with SQLite for tracking PR states
- [ ] Implement polling service that checks PRs every minute
- [ ] Implement commit tracking logic to detect new commits on PR branches
- [ ] Implement cbpr command executor with proper error handling
- [ ] Set up directory structure for storing generated HTML review files
- [ ] Implement HTTP server with API endpoints (list PRs, serve HTML reviews)
- [ ] Create frontend HTML/JS for displaying PR list with links
- [ ] Add configuration support (GitHub token, username, polling interval)
- [ ] Add logging and error handling throughout the application
- [ ] Test the complete workflow end-to-end

## Architecture

**Tech Stack:**
- Backend: Go
- Database: SQLite
- Frontend: HTML/JS
- GitHub API integration

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
  UNIQUE(repo_owner, repo_name, pr_number)
);
```

**Key Components:**
1. GitHub API client - fetch PRs and commit info
2. Polling service - runs every minute
3. Review generator - executes cbpr command
4. HTTP server - serves API and frontend
5. SQLite database - tracks PR states
