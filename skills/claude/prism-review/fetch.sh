#!/usr/bin/env bash
# Fetch a PRism AI review for a given PR and emit it as a self-describing
# markdown block on stdout. Designed to be invoked by the prism-review skill.
#
# Usage:
#   fetch.sh <pr-ref>
#
# pr-ref forms accepted:
#   123                                 (resolves owner/repo from `git remote get-url origin`)
#   owner/repo#123
#   owner/repo/123
#   https://github.com/owner/repo/pull/123
#
# Optional env:
#   PRISM_BASE_URL  base URL of the prism server (REQUIRED, e.g. http://localhost:8080)
#   PRISM_TOKEN     bearer token (default: `gh auth token`)
#   PRISM_SHA       pin to a specific commit's review (optional)
#
# Exit codes:
#   0  success
#   2  bad arguments / could not resolve PR
#   3  auth missing
#   4  HTTP error from server (401/404/5xx)

set -euo pipefail

die() { echo "prism-review: $*" >&2; exit "${2:-2}"; }

BASE_URL="${PRISM_BASE_URL:-}"
if [ -z "$BASE_URL" ]; then
  die "PRISM_BASE_URL is not set — point it at your prism server (e.g. http://localhost:8080)"
fi

if [ "$#" -lt 1 ]; then
  die "usage: fetch.sh <pr-number | owner/repo#N | github-url>"
fi

ref="$1"
owner=""
repo=""
pr=""

# Form 1: full GitHub URL
if [[ "$ref" =~ ^https?://github\.com/([^/]+)/([^/]+)/pull/([0-9]+) ]]; then
  owner="${BASH_REMATCH[1]}"
  repo="${BASH_REMATCH[2]}"
  pr="${BASH_REMATCH[3]}"

# Form 2: owner/repo#N or owner/repo/N
elif [[ "$ref" =~ ^([^/]+)/([^/#]+)[#/]([0-9]+)$ ]]; then
  owner="${BASH_REMATCH[1]}"
  repo="${BASH_REMATCH[2]}"
  pr="${BASH_REMATCH[3]}"

# Form 3: bare PR number — resolve from current git repo
elif [[ "$ref" =~ ^[0-9]+$ ]]; then
  pr="$ref"
  if ! remote_url=$(git remote get-url origin 2>/dev/null); then
    die "bare PR number given but not in a git repo with an 'origin' remote"
  fi
  # Strip .git suffix; handle both ssh and https
  remote_url="${remote_url%.git}"
  if [[ "$remote_url" =~ github\.com[:/]([^/]+)/([^/]+)$ ]]; then
    owner="${BASH_REMATCH[1]}"
    repo="${BASH_REMATCH[2]}"
  else
    die "origin remote is not a github.com URL: $remote_url"
  fi

else
  die "could not parse PR ref: $ref"
fi

# Resolve token
token="${PRISM_TOKEN:-}"
if [ -z "$token" ]; then
  if command -v gh >/dev/null 2>&1; then
    token=$(gh auth token 2>/dev/null || true)
  fi
fi
if [ -z "$token" ]; then
  die "no PRISM_TOKEN env var and 'gh auth token' returned empty — run 'gh auth login' or set PRISM_TOKEN" 3
fi

url="${BASE_URL}/api/review/${owner}/${repo}/${pr}"
if [ -n "${PRISM_SHA:-}" ]; then
  url="${url}?sha=${PRISM_SHA}"
fi

# -f returns non-zero on HTTP >= 400; -S shows the error; -s otherwise silent.
# Capture body and HTTP status separately so we can report nicely on failure.
tmp_body=$(mktemp)
trap 'rm -f "$tmp_body"' EXIT

http_code=$(curl -sS -w '%{http_code}' -o "$tmp_body" \
  -H "Authorization: Bearer $token" \
  -H "Accept: application/json" \
  "$url" || true)

# 202: review is generating — print status payload and exit 0 so the caller
# (Claude) can decide whether to wait + retry.
if [ "$http_code" = "202" ]; then
  if command -v jq >/dev/null 2>&1; then
    jq -r '
      "Status: review is in flight (\(.pr_status))",
      "PR: \(.owner)/\(.repo)#\(.pr_number)",
      "Current HEAD: \(.head_sha)",
      "",
      "No review file yet. Retry this fetch in ~30s."
    ' "$tmp_body"
  else
    cat "$tmp_body"
  fi
  exit 0
fi

# 424: prism tried to generate a review and the upstream model failed.
if [ "$http_code" = "424" ]; then
  if command -v jq >/dev/null 2>&1; then
    jq -r '
      "Status: review generation FAILED (\(.pr_status))",
      "PR: \(.owner)/\(.repo)#\(.pr_number)",
      "Error: \(.error_message)",
      "",
      "Tell the user to re-trigger the review from the prism dashboard."
    ' "$tmp_body"
  else
    cat "$tmp_body"
  fi
  exit 0
fi

if [ "$http_code" != "200" ]; then
  echo "prism-review: HTTP $http_code from $url" >&2
  cat "$tmp_body" >&2
  echo >&2
  exit 4
fi

# Pretty-print metadata header + raw HTML body for the model to consume.
# Use jq if available for clean extraction; otherwise pass the JSON through.
if command -v jq >/dev/null 2>&1; then
  jq -r '
    "PR: \(.owner)/\(.repo)#\(.pr_number)",
    "Reviewed commit: \(.commit_sha)",
    "Current HEAD:    \(.head_sha)",
    "Stale: \(.is_stale)",
    "In flight (regenerating): \(.is_in_flight)",
    "Generated at: \(.generated_at)",
    "Counts: critical=\(.counts.critical) medium=\(.counts.medium) low=\(.counts.low)",
    "Review URL: \(.review_url)",
    "",
    "=== REVIEW HTML ===",
    .html
  ' "$tmp_body"
else
  cat "$tmp_body"
fi
