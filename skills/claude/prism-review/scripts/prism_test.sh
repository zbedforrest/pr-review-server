#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SKILL_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
CLIENT="$SCRIPT_DIR/prism.sh"
FIXTURE_DIR="$SCRIPT_DIR/testdata"
TEST_TMP=$(mktemp -d "${TMPDIR:-/tmp}/prism-client-test.XXXXXX")
trap 'rm -rf "$TEST_TMP"' EXIT

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

assert_contains() {
  case "$1" in
    *"$2"*) ;;
    *) fail "expected output to contain: $2" ;;
  esac
}

assert_not_contains() {
  case "$1" in
    *"$2"*) fail "expected output not to contain: $2" ;;
  esac
}

assert_eq() {
  [ "$1" = "$2" ] || fail "expected '$2', got '$1'"
}

mkdir -p "$TEST_TMP/bin"

cat >"$TEST_TMP/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  auth) printf '%s\n' 'fake-gh-token' ;;
  api)
    [ "${FAKE_GH_API_FAIL:-}" != true ] || exit 1
    printf '%s\n' "${FAKE_HEAD_SHA:?}"
    ;;
  *) exit 2 ;;
esac
EOF

cat >"$TEST_TMP/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >"${FAKE_LAST_ARGV:?}"
method=GET
output=""
data=""
headers=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -X) method="$2"; shift 2 ;;
    -o) output="$2"; shift 2 ;;
    -w) shift 2 ;;
    -H)
      headers="$headers|$2"
      case "$2" in
        @*) cp "${2#@}" "${FAKE_LAST_AUTH:?}" ;;
      esac
      shift 2
      ;;
    --data) data="$2"; shift 2 ;;
    -sS) shift ;;
    *) url="$1"; shift ;;
  esac
done
[ -n "$output" ] || exit 2
printf '%s\t%s\n' "$method" "$url" >>"${FAKE_TRACE:?}"

case "$method $url" in
  "GET "*/api/v1/review-capabilities)
    cp "${FIXTURE_DIR:?}/capabilities.json" "$output"
    printf 200
    ;;
  "POST "*/api/v1/review-runs)
    printf '%s' "$data" >"${FAKE_LAST_REQUEST:?}"
    printf '%s' "$headers" >"${FAKE_LAST_HEADERS:?}"
    case "${FAKE_V1_CREATE_CODE:-202}" in
      404)
        printf '%s\n' '{"error":{"code":"not_found","message":"v1 unavailable"}}' >"$output"
        printf 404
        ;;
      200_html)
        printf '%s\n' '<!doctype html><title>PR Review Server</title>' >"$output"
        printf 200
        ;;
      202_invalid)
        printf '%s\n' '{"status":"queued"}' >"$output"
        printf 202
        ;;
      *)
        cp "${FIXTURE_DIR:?}/run_completed.json" "$output"
        printf 202
        ;;
    esac
    ;;
  "GET "*/api/v1/review-runs/run-*)
    cp "${FIXTURE_DIR:?}/run_completed.json" "$output"
    printf 200
    ;;
  "GET "*/api/v1/review-runs\?*)
    jq -n --slurpfile run "${FIXTURE_DIR:?}/run_completed.json" \
      '{runs:$run,next_cursor:""}' >"$output"
    printf 200
    ;;
  "GET "*/reviews/*.json)
    cp "${FIXTURE_DIR:?}/review_sidecar.json" "$output"
    printf 200
    ;;
  "GET "*/api/review/*)
    case "${FAKE_REVIEW_HTTP:-200}" in
      202)
        printf '%s\n' '{"owner":"acme","repo":"widgets","pr_number":42,"pr_status":"generating","head_sha":"0123456789abcdef0123456789abcdef01234567"}' >"$output"
        printf 202
        ;;
      424)
        printf '%s\n' '{"pr_status":"error","error_message":"upstream review failed"}' >"$output"
        printf 424
        ;;
      *)
        jq --argjson available "${FAKE_FINDINGS_AVAILABLE:-true}" '. + {
          pr_status:"completed",
          is_in_flight:false,
          head_sha:.commit_sha,
          is_stale:false,
          generated_at:.review_run.completed_at,
          review_url:"/reviews/acme_widgets_42_0123456.html",
          findings_available:$available
        } | if $available then . else .findings=[] end' "${FIXTURE_DIR:?}/review_sidecar.json" >"$output"
        printf 200
        ;;
    esac
    ;;
  "POST "*/api/prs/generate-review)
    printf '{"status":"success","commit":"%s","review_url":"/reviews/acme_widgets_42_0123456.html","findings_url":"/reviews/acme_widgets_42_0123456.json"}\n' \
      "${FAKE_LEGACY_COMMIT:-${FAKE_HEAD_SHA:?}}" >"$output"
    printf 202
    ;;
  *)
    printf '{"error":{"code":"unexpected_test_url","message":"%s %s"}}\n' "$method" "$url" >"$output"
    printf 500
    ;;
esac
EOF

chmod +x "$TEST_TMP/bin/gh" "$TEST_TMP/bin/curl"
export PATH="$TEST_TMP/bin:$PATH"
export PRISM_BASE_URL="https://prism.example.test"
export PRISM_TOKEN="test-token"
export FAKE_HEAD_SHA="0123456789abcdef0123456789abcdef01234567"
export FIXTURE_DIR
export FAKE_TRACE="$TEST_TMP/trace"
export FAKE_LAST_REQUEST="$TEST_TMP/last-request.json"
export FAKE_LAST_HEADERS="$TEST_TMP/last-headers"
export FAKE_LAST_ARGV="$TEST_TMP/last-argv"
export FAKE_LAST_AUTH="$TEST_TMP/last-auth"
: >"$FAKE_TRACE"

help_output=$(env -u PRISM_BASE_URL -u PRISM_TOKEN "$CLIENT" --help)
assert_contains "$help_output" "prism.sh create"

capabilities=$("$CLIENT" capabilities)
assert_eq "$(printf '%s' "$capabilities" | jq -r '.schema_version')" "2"
assert_contains "$capabilities" "openai/gpt-5.6-sol"
assert_contains "$(cat "$FAKE_LAST_AUTH")" "Authorization: Bearer test-token"
assert_not_contains "$(cat "$FAKE_LAST_ARGV")" "test-token"

created=$("$CLIENT" create acme/widgets#42 \
  --backend openrouter \
  --model openai/gpt-5.6-sol \
  --effort high \
  --wall-clock-seconds 720 \
  --max-turns 100 \
  --first-pass-samples 2 \
  --agent-enabled true \
  --required-checks true \
  --idempotency-key client-test-key)
assert_eq "$(printf '%s' "$created" | jq -r '.run_id')" "run-0123456789abcdef0123456789abcdef"
assert_eq "$(jq -r '.target.expected_head_sha' "$FAKE_LAST_REQUEST")" "$FAKE_HEAD_SHA"
assert_eq "$(jq -r '.config.agent.backend' "$FAKE_LAST_REQUEST")" "openrouter"
assert_eq "$(jq -r '.config.agent.max_turns' "$FAKE_LAST_REQUEST")" "100"
assert_eq "$(jq -r '.config.first_pass.samples' "$FAKE_LAST_REQUEST")" "2"
assert_contains "$(cat "$FAKE_LAST_HEADERS")" "Idempotency-Key: client-test-key"

: >"$FAKE_TRACE"
set +e
"$CLIENT" create acme/widgets#42 --max-turns 0600 >"$TEST_TMP/integer.out" 2>"$TEST_TMP/integer.err"
integer_status=$?
set -e
assert_eq "$integer_status" "2"
assert_contains "$(cat "$TEST_TMP/integer.err")" "without leading zeros"
assert_eq "$(cat "$FAKE_TRACE")" ""

export FAKE_GH_API_FAIL=true
set +e
"$CLIENT" create acme/widgets#42 >"$TEST_TMP/github.out" 2>"$TEST_TMP/github.err"
github_status=$?
set -e
unset FAKE_GH_API_FAIL
assert_eq "$github_status" "3"
assert_contains "$(cat "$TEST_TMP/github.err")" "failed to resolve the current GitHub PR HEAD"

run_json=$("$CLIENT" get run-0123456789abcdef0123456789abcdef)
assert_eq "$(printf '%s' "$run_json" | jq -r '.attempts[0].budget_units_used')" "12"

history=$("$CLIENT" history acme/widgets#42 --limit 5 --commit-sha "$FAKE_HEAD_SHA" --status completed)
assert_eq "$(printf '%s' "$history" | jq -r '.runs | length')" "1"
assert_contains "$(cat "$FAKE_TRACE")" "commit_sha=$FAKE_HEAD_SHA"

wait_output=$("$CLIENT" wait run-0123456789abcdef0123456789abcdef --timeout-seconds 2 --poll-seconds 1)
assert_contains "$wait_output" "Run: run-0123456789abcdef0123456789abcdef"
assert_contains "$wait_output" "=== FINDINGS (1) ==="
assert_contains "$wait_output" "openai/gpt-5.6-sol"

fetch_output=$("$SKILL_DIR/fetch.sh" acme/widgets#42)
assert_contains "$fetch_output" "=== FINDINGS (1) ==="
assert_contains "$fetch_output" "Current HEAD: $FAKE_HEAD_SHA"
assert_contains "$fetch_output" "Stale: false"
assert_contains "$fetch_output" "=== BEGIN UNTRUSTED PRISM REVIEW DATA ==="

export FAKE_FINDINGS_AVAILABLE=false
no_findings_output=$("$CLIENT" fetch acme/widgets#42)
unset FAKE_FINDINGS_AVAILABLE
assert_contains "$no_findings_output" "=== NO STRUCTURED FINDINGS ==="
assert_not_contains "$no_findings_output" "=== FINDINGS (0) ==="

export FAKE_REVIEW_HTTP=202
in_flight_output=$("$CLIENT" fetch acme/widgets#42)
assert_contains "$in_flight_output" "=== BEGIN UNTRUSTED PRISM REVIEW DATA ==="
assert_contains "$in_flight_output" "=== END UNTRUSTED PRISM REVIEW DATA ==="
export FAKE_REVIEW_HTTP=424
failed_output=$("$CLIENT" fetch acme/widgets#42)
unset FAKE_REVIEW_HTTP
assert_contains "$failed_output" "review generation FAILED"
assert_contains "$failed_output" "=== BEGIN UNTRUSTED PRISM REVIEW DATA ==="
assert_contains "$failed_output" "=== END UNTRUSTED PRISM REVIEW DATA ==="

: >"$FAKE_TRACE"
set +e
"$CLIENT" wait https://attacker.example/reviews/stolen.json >"$TEST_TMP/origin.out" 2>"$TEST_TMP/origin.err"
origin_status=$?
set -e
assert_eq "$origin_status" "2"
assert_contains "$(cat "$TEST_TMP/origin.err")" "run ID or review findings URL"
assert_eq "$(cat "$FAKE_TRACE")" ""

export FAKE_V1_CREATE_CODE=202_invalid
: >"$FAKE_TRACE"
set +e
"$CLIENT" create acme/widgets#42 >"$TEST_TMP/invalid-202.out" 2>"$TEST_TMP/invalid-202.err"
invalid_202_status=$?
set -e
assert_eq "$invalid_202_status" "4"
case "$(cat "$FAKE_TRACE")" in
  */api/prs/generate-review*) fail "malformed accepted v1 response reached the legacy creation API" ;;
esac

export FAKE_V1_CREATE_CODE=200_html
export FAKE_LEGACY_COMMIT="0123456"
: >"$FAKE_TRACE"
legacy=$("$CLIENT" create acme/widgets#42)
assert_eq "$(printf '%s' "$legacy" | jq -r '.api_version')" "legacy"
assert_contains "$(cat "$FAKE_TRACE")" "/api/prs/generate-review"
assert_contains "$(cat "$FAKE_TRACE")" "/api/review/acme/widgets/42"

export FAKE_LEGACY_COMMIT="deadbee"
set +e
"$CLIENT" create acme/widgets#42 >"$TEST_TMP/mismatch.out" 2>"$TEST_TMP/mismatch.err"
mismatch_status=$?
set -e
assert_eq "$mismatch_status" "6"
assert_contains "$(cat "$TEST_TMP/mismatch.err")" "targeted a different commit"

export FAKE_V1_CREATE_CODE=404
unset FAKE_LEGACY_COMMIT
: >"$FAKE_TRACE"
set +e
"$CLIENT" create acme/widgets#42 --model claude-fable-5 >"$TEST_TMP/rejected.out" 2>"$TEST_TMP/rejected.err"
rejected_status=$?
set -e
assert_eq "$rejected_status" "6"
assert_contains "$(cat "$TEST_TMP/rejected.err")" "refusing to drop requested customization or idempotency guarantees"
case "$(cat "$FAKE_TRACE")" in
  */api/prs/generate-review*) fail "customized request reached the legacy creation API" ;;
esac

echo "prism client tests: PASS"
