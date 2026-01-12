#!/bin/bash
#
# review-next.sh - Prioritize PRs for review
#
# This script analyzes PRs from the PR review server and GitHub API,
# applies a scoring algorithm, and recommends which PR to review next.
#
# Usage:
#   ./review-next.sh [--top N] [--repo OWNER/REPO] [--show-all]
#
# Options:
#   --top N              Show top N PRs (default: 3)
#   --repo OWNER/REPO    Filter to specific repository
#   --show-all           Show all PRs with scores, not just top N
#   --help               Show this help message

set -euo pipefail

# Configuration
SERVER_URL="${PR_REVIEW_SERVER_URL:-http://localhost:7769}"
TOP_N=3
FILTER_REPO=""
SHOW_ALL=false

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --top)
            TOP_N="$2"
            shift 2
            ;;
        --repo)
            FILTER_REPO="$2"
            shift 2
            ;;
        --show-all)
            SHOW_ALL=true
            shift
            ;;
        --help)
            head -n 15 "$0" | grep "^#" | grep -v "^#!/" | sed 's/^# //' | sed 's/^#$//'
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            echo "Use --help for usage information"
            exit 1
            ;;
    esac
done

# Ensure required tools are available
if ! command -v jq &> /dev/null; then
    echo "Error: jq is required but not installed"
    exit 1
fi

if ! command -v gh &> /dev/null; then
    echo "Error: gh (GitHub CLI) is required but not installed"
    exit 1
fi

# Get GitHub username
GITHUB_USERNAME=$(gh api user --jq '.login')

echo "🔍 Fetching PRs from review server..."
PR_DATA=$(curl -s "$SERVER_URL/api/prs")

if [ -z "$PR_DATA" ] || [ "$PR_DATA" = "null" ]; then
    echo "Error: Failed to fetch PR data from $SERVER_URL"
    exit 1
fi

# Filter PRs: not mine, not draft
FILTERED_PRS=$(echo "$PR_DATA" | jq -c '[.[] | select(.is_mine == false and .draft == false)]')

if [ "$FILTERED_PRS" = "[]" ]; then
    echo "✅ No PRs to review!"
    exit 0
fi

# Apply repo filter if specified
if [ -n "$FILTER_REPO" ]; then
    REPO_OWNER=$(echo "$FILTER_REPO" | cut -d'/' -f1)
    REPO_NAME=$(echo "$FILTER_REPO" | cut -d'/' -f2)
    FILTERED_PRS=$(echo "$FILTERED_PRS" | jq -c "[.[] | select(.owner == \"$REPO_OWNER\" and .repo == \"$REPO_NAME\")]")

    if [ "$FILTERED_PRS" = "[]" ]; then
        echo "✅ No PRs to review in $FILTER_REPO"
        exit 0
    fi
fi

echo "📊 Analyzing $(echo "$FILTERED_PRS" | jq 'length') PRs..."
echo ""

# Create temporary file for scoring results
TEMP_SCORES=$(mktemp)
trap "rm -f $TEMP_SCORES" EXIT

# Score each PR
echo "$FILTERED_PRS" | jq -c '.[]' | while read -r pr; do
    OWNER=$(echo "$pr" | jq -r '.owner')
    REPO=$(echo "$pr" | jq -r '.repo')
    NUMBER=$(echo "$pr" | jq -r '.number')
    TITLE=$(echo "$pr" | jq -r '.title')
    AUTHOR=$(echo "$pr" | jq -r '.author')
    APPROVAL_COUNT=$(echo "$pr" | jq -r '.approval_count')
    MY_REVIEW_STATUS=$(echo "$pr" | jq -r '.my_review_status')
    LAST_REVIEWED_AT=$(echo "$pr" | jq -r '.last_reviewed_at')
    REVIEW_URL=$(echo "$pr" | jq -r '.review_url')
    GITHUB_URL=$(echo "$pr" | jq -r '.github_url')

    # Fetch additional data from GitHub
    GH_DATA=$(gh pr view "$NUMBER" --repo "$OWNER/$REPO" --json createdAt,updatedAt,additions,deletions,changedFiles,reviewRequests,reviews 2>/dev/null || echo '{}')

    if [ "$GH_DATA" = "{}" ]; then
        echo "⚠️  Skipping PR #$NUMBER (GitHub API error)" >&2
        continue
    fi

    CREATED_AT=$(echo "$GH_DATA" | jq -r '.createdAt')
    ADDITIONS=$(echo "$GH_DATA" | jq -r '.additions // 0')
    DELETIONS=$(echo "$GH_DATA" | jq -r '.deletions // 0')
    CHANGED_FILES=$(echo "$GH_DATA" | jq -r '.changedFiles // 0')
    REVIEW_COUNT=$(echo "$GH_DATA" | jq -r '.reviews | length')
    REQUESTED_REVIEWERS=$(echo "$GH_DATA" | jq -r '[.reviewRequests[].login] | join(",")')

    # Calculate age in days
    CREATED_TIMESTAMP=$(date -j -f "%Y-%m-%dT%H:%M:%SZ" "$CREATED_AT" +%s 2>/dev/null || echo "0")
    NOW_TIMESTAMP=$(date +%s)
    AGE_DAYS=$(( (NOW_TIMESTAMP - CREATED_TIMESTAMP) / 86400 ))

    # Calculate score
    SCORE=0
    REASONS=()

    # Age scoring (4+ days = +50, 3 days = +30, 2 days = +20, 1 day = +10)
    if [ "$AGE_DAYS" -ge 4 ]; then
        SCORE=$((SCORE + 50))
        REASONS+=("Very old (${AGE_DAYS}d)")
    elif [ "$AGE_DAYS" -ge 3 ]; then
        SCORE=$((SCORE + 30))
        REASONS+=("Old (${AGE_DAYS}d)")
    elif [ "$AGE_DAYS" -ge 2 ]; then
        SCORE=$((SCORE + 20))
        REASONS+=("Aging (${AGE_DAYS}d)")
    elif [ "$AGE_DAYS" -ge 1 ]; then
        SCORE=$((SCORE + 10))
        REASONS+=("Recent (${AGE_DAYS}d)")
    fi

    # Approval gap (reviews but no approvals)
    if [ "$REVIEW_COUNT" -ge 3 ] && [ "$APPROVAL_COUNT" -eq 0 ]; then
        SCORE=$((SCORE + 40))
        REASONS+=("${REVIEW_COUNT} reviews but no approvals")
    fi

    # Size + attention gap
    if [ "$ADDITIONS" -ge 500 ] && [ "$REVIEW_COUNT" -lt 2 ]; then
        SCORE=$((SCORE + 30))
        REASONS+=("Large PR (${ADDITIONS}+ lines) with few reviews")
    fi

    # Explicit request
    if echo "$REQUESTED_REVIEWERS" | grep -q "$GITHUB_USERNAME"; then
        SCORE=$((SCORE + 25))
        REASONS+=("You are explicitly requested")
    fi

    # Size factor
    if [ "$ADDITIONS" -ge 1000 ]; then
        SCORE=$((SCORE + 20))
        REASONS+=("Very large (${ADDITIONS}+ lines)")
    elif [ "$ADDITIONS" -ge 500 ]; then
        SCORE=$((SCORE + 10))
        REASONS+=("Large (${ADDITIONS}+ lines)")
    fi

    # Already well-covered (penalty)
    if [ "$APPROVAL_COUNT" -ge 1 ] && [ "$REVIEW_COUNT" -ge 5 ]; then
        SCORE=$((SCORE - 30))
        REASONS+=("Well-covered ($APPROVAL_COUNT approvals, $REVIEW_COUNT reviews)")
    fi

    # Already reviewed by me (penalty)
    if [ "$MY_REVIEW_STATUS" = "APPROVED" ] || [ "$MY_REVIEW_STATUS" = "COMMENTED" ]; then
        SCORE=$((SCORE - 40))
        REASONS+=("You already reviewed ($MY_REVIEW_STATUS)")
    fi

    # Build reason string
    REASON_STR=$(IFS="; "; echo "${REASONS[*]}")

    # Output: score|repo|number|title|author|additions|deletions|changed_files|approval_count|review_count|age_days|reason|review_url|github_url
    echo "$SCORE|$OWNER/$REPO|$NUMBER|$TITLE|$AUTHOR|$ADDITIONS|$DELETIONS|$CHANGED_FILES|$APPROVAL_COUNT|$REVIEW_COUNT|$AGE_DAYS|$REASON_STR|$SERVER_URL$REVIEW_URL|$GITHUB_URL" >> "$TEMP_SCORES"
done

# Sort by score (descending) and display
if [ ! -s "$TEMP_SCORES" ]; then
    echo "✅ No PRs to review!"
    exit 0
fi

SORTED=$(sort -t'|' -k1 -rn "$TEMP_SCORES")

# Determine how many to show
if [ "$SHOW_ALL" = true ]; then
    DISPLAY_COUNT=$(echo "$SORTED" | wc -l | tr -d ' ')
else
    DISPLAY_COUNT="$TOP_N"
fi

# Display results
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "🎯 Top $DISPLAY_COUNT PRs to Review"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

RANK=1
echo "$SORTED" | head -n "$DISPLAY_COUNT" | while IFS='|' read -r score repo number title author additions deletions changed_files approval_count review_count age_days reason review_url github_url; do

    # Priority badge
    if [ "$score" -ge 60 ]; then
        PRIORITY="🔴 HIGH"
    elif [ "$score" -ge 30 ]; then
        PRIORITY="🟡 MEDIUM"
    elif [ "$score" -ge 0 ]; then
        PRIORITY="🟢 LOW"
    else
        PRIORITY="⚪ SKIP"
    fi

    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "$RANK. $PRIORITY (Score: $score)"
    echo "   $repo #$number"
    echo "   \"$title\" by @$author"
    echo ""
    echo "   📏 Size: $changed_files files, +$additions/-$deletions lines"
    echo "   ⏰ Age: $age_days days"
    echo "   ✅ Reviews: $review_count reviews, $approval_count approvals"
    echo ""
    echo "   📋 Reasons: $reason"
    echo ""
    echo "   🔗 Review: $review_url"
    echo "   🔗 GitHub: $github_url"
    echo ""

    RANK=$((RANK + 1))
done

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
