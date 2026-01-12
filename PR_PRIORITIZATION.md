# PR Review Prioritization Approach

This document describes the data-driven approach used by `review-next.sh` to intelligently prioritize which PRs you should review next.

## Overview

The prioritization system combines data from two sources:
1. **PR Review Server API** - Internal tracking data (review status, completion, your previous reviews)
2. **GitHub CLI** - Real-time GitHub data (PR age, size, requested reviewers, review counts)

## Data Collection

### From PR Review Server (`/api/prs`)

```bash
curl -s http://localhost:7769/api/prs | jq .
```

Provides:
- `status`: Current review generation status (completed, generating, pending, error)
- `is_mine`: Whether this is your own PR (always excluded from prioritization)
- `draft`: Whether the PR is a draft (always excluded from prioritization)
- `approval_count`: Number of approvals the PR has received
- `my_review_status`: Your previous review state (APPROVED, COMMENTED, or empty)
- `last_reviewed_at`: When the cbpr review was last generated
- `review_url`: Link to the HTML review on the local server

### From GitHub API (via `gh` CLI)

```bash
gh pr view <NUMBER> --repo <OWNER/REPO> --json createdAt,additions,reviewRequests,reviews
```

Provides:
- `createdAt`: When the PR was opened (used to calculate age)
- `updatedAt`: Last update timestamp
- `additions`/`deletions`: Lines of code changed
- `changedFiles`: Number of files modified
- `reviewRequests`: List of requested reviewers (to detect if you're explicitly requested)
- `reviews`: Full review history (to count total reviews)

## Scoring Algorithm

Each PR receives a score based on weighted factors. Higher scores = higher priority.

### Critical Factors (High Weight)

#### 1. Age of PR (+10 to +50 points)
```
Age >= 4 days: +50 points  (Very old - author waiting long time)
Age >= 3 days: +30 points  (Old - should review soon)
Age >= 2 days: +20 points  (Aging - getting stale)
Age >= 1 day:  +10 points  (Recent - not urgent)
Age < 1 day:   +0 points   (Fresh - can wait)
```

**Reasoning**: Respects the author's time. PRs sitting for 4+ days likely have a blocked engineer waiting for feedback.

#### 2. Approval Gap (+40 points)
```
If review_count >= 3 AND approval_count == 0: +40 points
```

**Reasoning**: Many reviews but no approvals suggests:
- Controversial changes needing more eyes
- Blocking issues that need resolution
- Complex PR where reviewers are uncertain

This is often the **most important signal** - it means something is stuck.

#### 3. Size vs Attention Gap (+30 points)
```
If additions >= 500 AND review_count < 2: +30 points
```

**Reasoning**: Large PRs (500+ lines) with few reviews are high-risk. They need thorough review but aren't getting attention.

#### 4. Explicit Request (+25 points)
```
If your username in requested_reviewers: +25 points
```

**Reasoning**: When someone specifically requests you as a reviewer, they're asking for your particular expertise or perspective.

### Moderate Factors (Medium Weight)

#### 5. Size (+10 to +20 points)
```
additions >= 1000: +20 points  (Very large - needs thorough review)
additions >= 500:  +10 points  (Large - significant change)
```

**Reasoning**: Larger PRs need more careful attention and are often harder to review, so prioritizing them helps prevent them from becoming stale.

### Penalty Factors (Negative Weight)

#### 6. Well-Covered PRs (-30 points)
```
If approval_count >= 1 AND review_count >= 5: -30 points
```

**Reasoning**: PRs with 5+ reviews and at least one approval are well-covered by others. Your review adds marginal value.

#### 7. Already Reviewed (-40 points)
```
If my_review_status in [APPROVED, COMMENTED]: -40 points
```

**Reasoning**: You've already provided feedback. Unless there are significant new changes, your time is better spent on PRs you haven't seen.

## Priority Levels

Based on final scores:

| Score | Priority | Meaning |
|-------|----------|---------|
| 60+   | 🔴 HIGH   | Review today - author likely blocked or PR is critical |
| 30-59 | 🟡 MEDIUM | Review this week - important but not urgent |
| 0-29  | 🟢 LOW    | Review when you have time - low impact |
| < 0   | ⚪ SKIP   | Skip for now - well-covered or already reviewed |

## Example Scenarios

### Scenario 1: Blocking PR
```
PR #24792: "feature: ts lingo chat cache"
- Age: 3 days (+30)
- 7 reviews but 0 approvals (+40)
- 1280 lines changed (+20)
= Score: 90 (🔴 HIGH)
```
**Why high priority**: Multiple people reviewed but couldn't approve. Something is blocking this, and the author has been waiting 3 days.

### Scenario 2: Large PR Needing Attention
```
PR #84: "Support requiring owners from both base and head refs"
- Age: fresh (+0)
- 795 lines, only 1 review (+30)
- You're explicitly requested (+25)
- Large size (+10)
= Score: 65 (🔴 HIGH)
```
**Why high priority**: Substantial change, you're requested specifically, and it hasn't gotten enough review attention yet.

### Scenario 3: Well-Covered PR
```
PR #24847: "Mobile search overlay history"
- Age: 1 day (+10)
- 1 approval, 7 reviews (-30)
= Score: -20 (⚪ SKIP)
```
**Why skip**: Already well-reviewed by others with an approval. Your review adds little value.

### Scenario 4: Already Reviewed by You
```
PR #24689: "Fix gender tabs overlap"
- Age: 21 days (+50)
- You already approved (-40)
- 1 approval, 17 reviews (-30)
= Score: -20 (⚪ SKIP)
```
**Why skip**: Despite being old, you already reviewed it and it has extensive coverage. No action needed from you.

## Script Usage

### Basic Usage
```bash
# Show top 3 PRs to review (default)
./review-next.sh

# Show top 5 PRs
./review-next.sh --top 5

# Show all PRs with scores
./review-next.sh --show-all

# Filter to specific repository
./review-next.sh --repo multimediallc/chaturbate
```

### Output Format
```
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
1. 🔴 HIGH (Score: 90)
   multimediallc/chaturbate #24792
   "feature: ts lingo chat cache" by @mvpowers

   📏 Size: 15 files, +1280/-57 lines
   ⏰ Age: 3 days
   ✅ Reviews: 7 reviews, 0 approvals

   📋 Reasons: Old (3d);7 reviews but no approvals;Very large (1280+ lines)

   🔗 Review: http://localhost:7769/reviews/multimediallc_chaturbate_24792.html
   🔗 GitHub: https://github.com/multimediallc/chaturbate/pull/24792
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
```

## Configuration

The script can be configured via environment variables:

```bash
# Change the PR review server URL (default: http://localhost:7769)
export PR_REVIEW_SERVER_URL=http://localhost:8080
./review-next.sh
```

## Requirements

- `jq` - JSON parsing
- `gh` - GitHub CLI (authenticated)
- `curl` - HTTP requests
- PR review server running locally

## Integration Ideas

### Daily Review Routine
```bash
# Start your day by checking what to review
./review-next.sh --top 5

# Focus on one repo
./review-next.sh --repo multimediallc/chaturbate

# See everything including low-priority items
./review-next.sh --show-all
```

### Alias Setup
Add to your `.zshrc` or `.bashrc`:
```bash
alias review-next='~/pr-review-server/review-next.sh'
alias review-all='~/pr-review-server/review-next.sh --show-all'
```

Then simply run:
```bash
review-next
```

## Future Enhancements

Potential improvements to the algorithm:
1. **Team signals**: PRs from your direct team members get +15 points
2. **Breaking change detection**: Parse titles for "breaking", "BREAKING" → +20 points
3. **Staleness decay**: PRs older than 7 days that keep getting updated get increasing priority
4. **Historical bias**: Track which PRs you typically review well and boost similar ones
5. **Dependency detection**: PRs that block other PRs (via PR description parsing) get +30 points
6. **Author relationship**: PRs from authors you frequently collaborate with get +10 points

## Philosophy

The prioritization approach balances three objectives:

1. **Respect author time**: Old PRs with no approvals likely have blocked engineers
2. **Leverage your expertise**: Explicit review requests and relevant repos get priority
3. **Avoid redundancy**: Well-covered PRs don't need another review

The goal is to maximize your impact as a reviewer by focusing on PRs where your review will unblock work or provide unique value.
