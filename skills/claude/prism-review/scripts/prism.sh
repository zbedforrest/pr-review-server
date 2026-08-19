#!/usr/bin/env bash
# Deterministic PRism review API client. Compatible with macOS Bash 3.2.
set -euo pipefail

die() {
  echo "prism-review: $1" >&2
  exit "${2:-2}"
}

usage() {
  cat <<'EOF'
Usage:
  prism.sh capabilities
  prism.sh create <pr-ref> [options]
  prism.sh get <run-id>
  prism.sh wait <run-id|findings-url> [--timeout-seconds N] [--poll-seconds N]
  prism.sh history <pr-ref> [--limit N] [--cursor TOKEN] [--commit-sha SHA] [--status STATUS]
  prism.sh fetch <pr-ref> [--sha SHA]

Create options:
  --backend NAME                 claude | openrouter
  --model MODEL_ID
  --effort LEVEL
  --wall-clock-seconds N
  --max-turns N
  --first-pass-samples N
  --agent-enabled true|false
  --required-checks true|false
  --expected-head-sha FULL_SHA   defaults to the current GitHub PR HEAD
  --idempotency-key KEY          defaults to a unique invocation key

PR refs: 42, owner/repo#42, owner/repo/42, or a GitHub pull URL.
EOF
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required" 3
}

is_positive_integer() {
  [[ "$1" =~ ^[1-9][0-9]*$ ]]
}

is_full_sha() {
  [[ "$1" =~ ^[0-9a-fA-F]{40}$ ]]
}

is_run_id() {
  [[ "$1" =~ ^run-[0-9a-f]{32}$ ]]
}

is_github_name() {
  [ -n "$1" ] && [ "${#1}" -le 100 ] && [ "$1" != . ] && [ "$1" != .. ] &&
    [[ "$1" =~ ^[A-Za-z0-9_.-]+$ ]]
}

command_name="${1:-}"
case "$command_name" in
  help|-h|--help|'')
    usage
    exit 0
    ;;
esac

BASE_URL="${PRISM_BASE_URL:-}"
if [ -z "$BASE_URL" ]; then
  die "PRISM_BASE_URL is not set — point it at your PRism server"
fi
BASE_URL="${BASE_URL%/}"
case "$BASE_URL" in
  http://*|https://*) ;;
  *) die "PRISM_BASE_URL must be an http:// or https:// URL" ;;
esac
case "$BASE_URL" in
  *\?*|*\#*) die "PRISM_BASE_URL must not contain a query string or fragment" ;;
esac

need_command curl
need_command jq

TOKEN="${PRISM_TOKEN:-}"
if [ -z "$TOKEN" ] && command -v gh >/dev/null 2>&1; then
  TOKEN=$(gh auth token 2>/dev/null || true)
fi
if [ -z "$TOKEN" ]; then
  die "no PRISM_TOKEN and 'gh auth token' returned empty" 3
fi
case "$TOKEN" in
  *$'\r'*|*$'\n'*) die "PRism token must not contain a newline" 3 ;;
esac

TMP_BODY=$(mktemp)
TMP_RUN=$(mktemp)
TMP_AUTH=$(mktemp)
chmod 600 "$TMP_AUTH"
printf 'Authorization: Bearer %s\n' "$TOKEN" >"$TMP_AUTH"
trap 'rm -f "$TMP_BODY" "$TMP_RUN" "$TMP_AUTH"' EXIT
HTTP_CODE=""
REQUEST_URL=""

absolute_url() {
  case "$1" in
    "$BASE_URL") printf '%s' "$1" ;;
    "$BASE_URL"/*) printf '%s' "$1" ;;
    http://*|https://*) die "refusing to send the PRism token to a cross-origin URL" ;;
    /*) printf '%s%s' "$BASE_URL" "$1" ;;
    *) printf '%s/%s' "$BASE_URL" "$1" ;;
  esac
}

http_request() {
  local method="$1"
  local request_body="${3:-}"
  local request_idempotency_key="${4:-}"
  local curl_args
  REQUEST_URL=$(absolute_url "$2")
  curl_args=(-sS -X "$method" -w '%{http_code}' -o "$TMP_BODY"
    -H "@$TMP_AUTH" -H 'Accept: application/json')
  if [ -n "$request_body" ]; then
    curl_args+=( -H 'Content-Type: application/json' --data "$request_body" )
  fi
  if [ -n "$request_idempotency_key" ]; then
    curl_args+=( -H "Idempotency-Key: $request_idempotency_key" )
  fi
  HTTP_CODE=$(curl "${curl_args[@]}" "$REQUEST_URL" || true)
}

print_http_error() {
  echo "prism-review: HTTP $HTTP_CODE from $REQUEST_URL" >&2
  cat "$TMP_BODY" >&2
  echo >&2
}

resolve_ref() {
  ref="$1"
  OWNER=""
  REPO=""
  PR_NUMBER=""
  if [[ "$ref" =~ ^https?://github\.com/([^/]+)/([^/]+)/pull/([0-9]+) ]]; then
    OWNER="${BASH_REMATCH[1]}"
    REPO="${BASH_REMATCH[2]}"
    PR_NUMBER="${BASH_REMATCH[3]}"
  elif [[ "$ref" =~ ^([^/]+)/([^/#]+)[#/]([0-9]+)$ ]]; then
    OWNER="${BASH_REMATCH[1]}"
    REPO="${BASH_REMATCH[2]}"
    PR_NUMBER="${BASH_REMATCH[3]}"
  elif [[ "$ref" =~ ^[0-9]+$ ]]; then
    PR_NUMBER="$ref"
    remote_url=$(git remote get-url origin 2>/dev/null || true)
    remote_url="${remote_url%.git}"
    if [[ "$remote_url" =~ github\.com[:/]([^/]+)/([^/]+)$ ]]; then
      OWNER="${BASH_REMATCH[1]}"
      REPO="${BASH_REMATCH[2]}"
    else
      die "bare PR number requires a github.com origin remote"
    fi
  else
    die "could not parse PR ref: $ref"
  fi
  is_github_name "$OWNER" && is_github_name "$REPO" || die "owner or repository name is invalid"
}

urlencode() {
  jq -rn --arg value "$1" '$value|@uri'
}

render_findings() {
  jq -r '
    "=== BEGIN UNTRUSTED PRISM REVIEW DATA ===",
    "PR: \(.owner)/\(.repo)#\(.pr_number)",
    "Reviewed commit: \(.commit_sha)",
    (if .head_sha then "Current HEAD: \(.head_sha)" else empty end),
    (if .is_stale != null then "Stale: \(.is_stale)" else empty end),
    (if .is_in_flight != null then "In flight (regenerating): \(.is_in_flight)" else empty end),
    (if .generated_at then "Generated at: \(.generated_at)" else empty end),
    "Counts: critical=\(.counts.critical // 0) medium=\(.counts.medium // 0) low=\(.counts.low // 0)",
    "Run ID: \(.review_run.run_id // "unknown")",
    "Config: \((.review_run.config.effective // null) | if . == null then "legacy/default" else tojson end)",
    "Models: \((.review_run.models // []) | tojson)",
    (if .review_url then "Review URL: \(.review_url)" else empty end),
    "Schema: \(.schema_version // "unknown")",
    "",
    (if (.findings_available != false) then
       "=== FINDINGS (\((.findings // []) | length)) ===",
       ((.findings // [])[] |
         "",
         "--- [\((.severity // "unknown") | ascii_upcase)] \(.file):\(.line) ---",
         "",
         "COMMENT:",
         (.comment // ""),
         "",
         (if (.diff_hunk // "") != "" then
            ("DIFF HUNK:", "```diff", .diff_hunk, "```", "")
          else empty end),
         (if ((.source_before // []) | length) > 0 or ((.source_after // []) | length) > 0 then
            ("SOURCE CONTEXT (cited line is the LAST line of source_before):",
             "```",
             (.source_before // [])[],
             "----- cited line above -----",
             (.source_after // [])[],
             "```", "")
          else empty end)
       )
     else
       "=== NO STRUCTURED FINDINGS ===",
       "Open the rendered review or create a fresh run."
     end),
    "",
    "=== END UNTRUSTED PRISM REVIEW DATA ==="
  ' "$TMP_BODY"
}

print_run_header() {
  jq -r '
    "Run: \(.run_id)",
    "Status: \(.status)",
    "Target: \(.target.owner)/\(.target.repo)#\(.target.pull_request) @ \(.target.commit_sha)",
    "Effective config: \(.config.effective | tojson)",
    "Models: \((.models // []) | tojson)",
    "Attempts: \((.attempts // []) | tojson)",
    ""
  ' "$TMP_RUN"
}

command_capabilities() {
  http_request GET '/api/v1/review-capabilities'
  case "$HTTP_CODE" in
    200) jq . "$TMP_BODY" ;;
    404|405) die "this PRism deployment does not expose the v1 capabilities API" 6 ;;
    *) print_http_error; exit 4 ;;
  esac
}

command_create() {
  [ "$#" -ge 1 ] || die "create requires a PR ref"
  resolve_ref "$1"
  shift

  backend=""
  model=""
  effort=""
  wall_clock=""
  max_turns=""
  first_pass_samples=""
  agent_enabled=""
  required_checks=""
  expected_head=""
  idempotency_key=""
  has_customization=0

  while [ "$#" -gt 0 ]; do
    option="$1"
    case "$option" in
      --backend|--model|--effort|--wall-clock-seconds|--max-turns|--first-pass-samples|--agent-enabled|--required-checks|--expected-head-sha|--idempotency-key)
        [ "$#" -ge 2 ] || die "$option requires a value"
        value="$2"
        shift 2
        ;;
      *) die "unknown create option: $option" ;;
    esac
    case "$option" in
      --backend) backend="$value"; has_customization=1 ;;
      --model) model="$value"; has_customization=1 ;;
      --effort) effort="$value"; has_customization=1 ;;
      --wall-clock-seconds) is_positive_integer "$value" || die "$option must be a positive integer without leading zeros"; wall_clock="$value"; has_customization=1 ;;
      --max-turns) is_positive_integer "$value" || die "$option must be a positive integer without leading zeros"; max_turns="$value"; has_customization=1 ;;
      --first-pass-samples) is_positive_integer "$value" || die "$option must be a positive integer without leading zeros"; first_pass_samples="$value"; has_customization=1 ;;
      --agent-enabled) [ "$value" = true ] || [ "$value" = false ] || die "$option must be true or false"; agent_enabled="$value"; has_customization=1 ;;
      --required-checks) [ "$value" = true ] || [ "$value" = false ] || die "$option must be true or false"; required_checks="$value"; has_customization=1 ;;
      --expected-head-sha) expected_head="$value" ;;
      --idempotency-key) idempotency_key="$value"; has_customization=1 ;;
    esac
  done

  if [ -z "$expected_head" ]; then
    command -v gh >/dev/null 2>&1 || die "gh is required to resolve the exact PR HEAD; pass --expected-head-sha" 3
    expected_head=$(gh api "repos/$OWNER/$REPO/pulls/$PR_NUMBER" --jq .head.sha 2>/dev/null || true)
    [ -n "$expected_head" ] || die "failed to resolve the current GitHub PR HEAD; pass --expected-head-sha or check gh authentication" 3
  fi
  is_full_sha "$expected_head" || die "expected head SHA must be a full 40-character hexadecimal SHA"
  expected_head=$(printf '%s' "$expected_head" | tr '[:upper:]' '[:lower:]')
  if [ -z "$idempotency_key" ]; then
    idempotency_key="prism-$expected_head-$(date +%s)-$$-${TMP_BODY##*/}"
  fi

  request=$(jq -cn --arg owner "$OWNER" --arg repo "$REPO" --argjson pr "$PR_NUMBER" --arg sha "$expected_head" \
    '{target:{owner:$owner,repo:$repo,pull_request:$pr,expected_head_sha:$sha},config:{}}')
  if [ -n "$backend$model$effort$wall_clock$max_turns$agent_enabled" ]; then
    request=$(printf '%s' "$request" | jq -c '.config.agent = {}')
  fi
  [ -z "$backend" ] || request=$(printf '%s' "$request" | jq -c --arg value "$backend" '.config.agent.backend=$value')
  [ -z "$model" ] || request=$(printf '%s' "$request" | jq -c --arg value "$model" '.config.agent.model=$value')
  [ -z "$effort" ] || request=$(printf '%s' "$request" | jq -c --arg value "$effort" '.config.agent.effort=$value')
  [ -z "$wall_clock" ] || request=$(printf '%s' "$request" | jq -c --argjson value "$wall_clock" '.config.agent.wall_clock_seconds=$value')
  [ -z "$max_turns" ] || request=$(printf '%s' "$request" | jq -c --argjson value "$max_turns" '.config.agent.max_turns=$value')
  [ -z "$agent_enabled" ] || request=$(printf '%s' "$request" | jq -c --argjson value "$agent_enabled" '.config.agent.enabled=$value')
  [ -z "$first_pass_samples" ] || request=$(printf '%s' "$request" | jq -c --argjson value "$first_pass_samples" '.config.first_pass={samples:$value}')
  [ -z "$required_checks" ] || request=$(printf '%s' "$request" | jq -c --argjson value "$required_checks" '.config.required_checks=$value')

  http_request POST '/api/v1/review-runs' "$request" "$idempotency_key"
  case "$HTTP_CODE" in
    200|202)
      if jq -e '.run_id | strings | test("^run-[0-9a-f]{32}$")' "$TMP_BODY" >/dev/null 2>&1; then
        jq . "$TMP_BODY"
        return
      fi
      if [ "$HTTP_CODE" = 202 ]; then
        print_http_error
        exit 4
      fi
      ;;
    404|405) ;;
    *) print_http_error; exit 4 ;;
  esac

  # Older deployments route an unknown /api/v1 path to the HTML app shell
  # with HTTP 200. A response without a valid v1 run ID is therefore the same
  # compatibility signal as 404/405, but fallback remains forbidden whenever
  # it would weaken caller-requested semantics.
  if [ "$has_customization" -ne 0 ]; then
    die "this deployment only supports legacy review creation; refusing to drop requested customization or idempotency guarantees" 6
  fi
  legacy_request=$(jq -cn --arg owner "$OWNER" --arg repo "$REPO" --argjson number "$PR_NUMBER" \
    '{owner:$owner,repo:$repo,number:$number}')
  http_request POST '/api/prs/generate-review' "$legacy_request"
  if [ "$HTTP_CODE" = 200 ] || [ "$HTTP_CODE" = 202 ]; then
    cp "$TMP_BODY" "$TMP_RUN"
    legacy_commit=$(jq -r '.commit // ""' "$TMP_RUN")
    if [ "$legacy_commit" != "$expected_head" ]; then
      if [ -n "$legacy_commit" ]; then
        [ "${#legacy_commit}" -ge 7 ] || die "legacy review creation returned an invalid commit identifier" 6
        case "$expected_head" in
          "$legacy_commit"*) ;;
          *) die "legacy review creation targeted a different commit" 6 ;;
        esac
      fi
      http_request GET "/api/review/$OWNER/$REPO/$PR_NUMBER"
      if [ "$HTTP_CODE" != 200 ] && [ "$HTTP_CODE" != 202 ]; then
        die "legacy review creation did not expose a verifiable target HEAD" 6
      fi
      legacy_head=$(jq -r '.head_sha // ""' "$TMP_BODY")
      if [ "$legacy_head" != "$expected_head" ]; then
        die "legacy review creation did not confirm the exact expected HEAD; refusing an ambiguous result" 6
      fi
    fi
    jq '. + {api_version:"legacy"}' "$TMP_RUN"
  else
    print_http_error
    exit 4
  fi
}

command_get() {
  [ "$#" -eq 1 ] || die "get requires one run ID"
  is_run_id "$1" || die "invalid run ID"
  http_request GET "/api/v1/review-runs/$1"
  if [ "$HTTP_CODE" = 200 ]; then
    jq . "$TMP_BODY"
  else
    print_http_error
    exit 4
  fi
}

command_history() {
  [ "$#" -ge 1 ] || die "history requires a PR ref"
  resolve_ref "$1"
  shift
  limit=20
  cursor=""
  commit_sha=""
  status=""
  while [ "$#" -gt 0 ]; do
    [ "$#" -ge 2 ] || die "$1 requires a value"
    case "$1" in
      --limit) is_positive_integer "$2" || die "--limit must be positive"; limit="$2" ;;
      --cursor) cursor="$2" ;;
      --commit-sha) commit_sha="$2"; is_full_sha "$commit_sha" || die "--commit-sha must be a full SHA" ;;
      --status) status="$2" ;;
      *) die "unknown history option: $1" ;;
    esac
    shift 2
  done
  [ "$limit" -le 100 ] || die "--limit must be at most 100"
  path="/api/v1/review-runs?owner=$(urlencode "$OWNER")&repo=$(urlencode "$REPO")&pull_request=$PR_NUMBER&limit=$limit"
  [ -z "$cursor" ] || path="$path&cursor=$(urlencode "$cursor")"
  [ -z "$commit_sha" ] || path="$path&commit_sha=$(urlencode "$commit_sha")"
  [ -z "$status" ] || path="$path&status=$(urlencode "$status")"
  http_request GET "$path"
  case "$HTTP_CODE" in
    200) jq . "$TMP_BODY" ;;
    404|405) die "this PRism deployment does not expose v1 run history" 6 ;;
    *) print_http_error; exit 4 ;;
  esac
}

command_wait() {
  [ "$#" -ge 1 ] || die "wait requires a run ID or findings URL"
  target="$1"
  shift
  timeout_seconds=1200
  poll_seconds=10
  while [ "$#" -gt 0 ]; do
    [ "$#" -ge 2 ] || die "$1 requires a value"
    case "$1" in
      --timeout-seconds) is_positive_integer "$2" || die "$1 must be positive"; timeout_seconds="$2" ;;
      --poll-seconds) is_positive_integer "$2" || die "$1 must be positive"; poll_seconds="$2" ;;
      *) die "unknown wait option: $1" ;;
    esac
    shift 2
  done
  deadline=$(( $(date +%s) + timeout_seconds ))

  if is_run_id "$target"; then
    while :; do
      http_request GET "/api/v1/review-runs/$target"
      if [ "$HTTP_CODE" != 200 ]; then
        print_http_error
        exit 4
      fi
      status=$(jq -r '.status // ""' "$TMP_BODY")
      case "$status" in
        queued|running)
          [ "$(date +%s)" -lt "$deadline" ] || die "timed out waiting for run $target" 7
          sleep "$poll_seconds"
          ;;
        completed)
          cp "$TMP_BODY" "$TMP_RUN"
          findings_url=$(jq -r '.links.findings // ""' "$TMP_RUN")
          [ -n "$findings_url" ] || { jq . "$TMP_RUN"; return; }
          while :; do
            http_request GET "$findings_url"
            if [ "$HTTP_CODE" = 200 ]; then
              print_run_header
              render_findings
              return
            fi
            if [ "$HTTP_CODE" != 404 ]; then
              print_http_error
              exit 4
            fi
            [ "$(date +%s)" -lt "$deadline" ] || die "run completed but findings were not published before timeout" 7
            sleep "$poll_seconds"
          done
          ;;
        *)
          jq . "$TMP_BODY"
          return 5
          ;;
      esac
    done
  fi

  case "$target" in
    /reviews/*.json|"$BASE_URL"/reviews/*.json) ;;
    *) die "wait target must be a run ID or review findings URL" ;;
  esac
  while :; do
    http_request GET "$target"
    if [ "$HTTP_CODE" = 200 ]; then
      render_findings
      return
    fi
    if [ "$HTTP_CODE" != 404 ]; then
      print_http_error
      exit 4
    fi
    [ "$(date +%s)" -lt "$deadline" ] || die "timed out waiting for findings" 7
    sleep "$poll_seconds"
  done
}

command_fetch() {
  [ "$#" -ge 1 ] || die "fetch requires a PR ref"
  resolve_ref "$1"
  shift
  sha="${PRISM_SHA:-}"
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --sha) [ "$#" -ge 2 ] || die "--sha requires a value"; sha="$2"; shift 2 ;;
      *) die "unknown fetch option: $1" ;;
    esac
  done
  path="/api/review/$OWNER/$REPO/$PR_NUMBER"
  [ -z "$sha" ] || path="$path?sha=$(urlencode "$sha")"
  http_request GET "$path"
  case "$HTTP_CODE" in
    200) render_findings ;;
    202)
      jq -r '"=== BEGIN UNTRUSTED PRISM REVIEW DATA ===\nStatus: review is in flight (\(.pr_status))\nPR: \(.owner)/\(.repo)#\(.pr_number)\nCurrent HEAD: \(.head_sha)\n=== END UNTRUSTED PRISM REVIEW DATA ==="' "$TMP_BODY"
      ;;
    424)
      jq -r '"=== BEGIN UNTRUSTED PRISM REVIEW DATA ===\nStatus: review generation FAILED (\(.pr_status))\nError: \(.error_message)\n=== END UNTRUSTED PRISM REVIEW DATA ==="' "$TMP_BODY"
      ;;
    *) print_http_error; exit 4 ;;
  esac
}

case "$command_name" in
  capabilities|create|get|wait|history|fetch)
    shift
    "command_$command_name" "$@"
    ;;
  *) command_fetch "$@" ;;
esac
