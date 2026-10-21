# PR Review Server

**Approval candidates follow-up: [START HERE: findings and resume checklist](START-HERE-APPROVAL-CANDIDATES.md).**

A self-hostable code review dashboard for GitHub pull requests, with an optional multi-stage AI review pipeline.

**Dashboard** — polls GitHub for PRs assigned to you (or your org), shows CI status, draft/ready state, and merged/closed indicators, with filtering and search mirrored into the URL for shareable views. Rows can be hidden into a collapsed section, and reviews you request by pasting any PR URL land in a "Requested by Me" section.

**AI reviews** — a sampled first pass generates review comments from the diff and surrounding context in a single prompt (no tools), a classification stage scores those comments, and optionally a Claude Code or Codex/OpenRouter agent stage clones the PR, verifies and reconciles the comments against the real code, and produces the final report. The first pass runs on Gemini, Claude, or an OpenRouter model; the agent stage runs on Claude Code or Codex. Reports are rendered as HTML with per-comment deep links (`#comment-N`), J/K keyboard navigation, and a Markdown export optimized for coding agents. A deterministic layer (mechanical gates + a "bug memory" of past bug patterns) can inject forced checks into the agent's review.

**Review API** — authenticated callers can inspect deployment capabilities, create an exact-commit review with per-run model and budget choices, poll it by a unique run ID, and query history. Requested/effective configuration, model usage, fallback detection, and provider-attempt telemetry are durable metadata. The legacy structured findings endpoint remains available, and a bundled Claude Code skill (`skills/claude/prism-review/`) supports both contracts.

## Prerequisites

- **Go 1.25+** with CGO enabled (the SQLite driver requires it)
- **Node 20+** and npm (the dashboard is built with Vite and embedded into the Go binary)
- **git** on `PATH`
- Optional: **Docker** (recommended run path), or the **`claude` / `codex` CLI** required by your selected agent backend

## Quick Start (Docker)

```bash
cp .env.example .env   # set GITHUB_TOKEN and GITHUB_USERNAME at minimum
make start             # docker-compose up -d
```

Dashboard: <http://localhost:7769>. Use `make logs`, `make status`, `make restart`, `make rebuild`, and `make stop` to manage it.

## Building from source

The frontend must be built first — `server/dist` is embedded into the binary via `go:embed` and is not checked in, so a bare `go build` on a fresh clone fails without this step:

```bash
cd frontend && npm ci && npm run build   # outputs to ../server/dist
cd .. && go build -o pr-review-server .

export GITHUB_TOKEN=your_token       # PAT with repo + read:org scopes
export GITHUB_USERNAME=your_username
export GEMINI_API_KEY=your_key       # optional, enables AI reviews

./pr-review-server                   # dashboard at http://localhost:8080
```

## Deployment options

| Resource | Options | Notes |
|----------|---------|-------|
| **Hosting** | Any server, VM, or container platform | Dockerfile and docker-compose included |
| **Database** | SQLite (default) or PostgreSQL | Set `DATABASE_URL` for PostgreSQL; SQLite works for small teams |
| **GitHub auth** | PAT (single-user dev mode) or GitHub App | GitHub App provides OAuth login and org-wide PR access for multi-user deployments |
| **Gemini API** | Required for AI reviews | The classification stage always runs on Gemini flash, whatever the first-pass provider |
| **Agent runtime** | Optional | Claude Code + Anthropic auth, or Codex + an OpenRouter key, for `AGENTIC_REVIEWS=true` |
| **GCS bucket** | Optional | Stores review artifacts (HTML/Markdown + JSON findings) in cloud deployments; defaults to local disk |

Minimal deployment: a single VM with Docker and a GitHub App.

### Auth modes

- **Dev mode** (default when no GitHub App is configured): authenticates to GitHub with `GITHUB_TOKEN` and auto-logs-in `GITHUB_USERNAME`. Good for single-user/local use.
- **Multi-user mode**: set the `GITHUB_APP_*` variables, `OAUTH_CALLBACK_URL`, and `SESSION_SECRET`. Users log in via GitHub OAuth; org membership (`GITHUB_ORG_NAME`) gates access. The OAuth callback path is `/auth/github/callback`. Each login also stores the user's OAuth token on the session row, encrypted (AES-256-GCM) under `SESSION_SECRET`, so quick actions can post reviews as that person; the App's "Expire user authorization tokens" setting is supported (tokens refresh automatically). Rotating `SESSION_SECRET` invalidates the stored tokens and users see "Sign in again" until they log in once more.

GitHub App permissions: Pull requests (read and write), Issues (read and write), Contents (read), Members (read), Checks (read) and Commit statuses (read). The last two cover the two kinds of entries in a commit's `statusCheckRollup.contexts`: `CheckRun` needs Checks, `StatusContext` (legacy commit statuses) needs Commit statuses. Missing either one makes GitHub return those entries as `FORBIDDEN`; the rollup state still arrives, but the failed-check names for hidden entries do not, and the poller logs one `[GRAPHQL] CI status: ... partial errors` summary per cycle.

## Environment variables

The most common ones:

| Variable | Required | Description |
|----------|----------|-------------|
| `GITHUB_TOKEN` | Dev mode | GitHub PAT with `repo` and `read:org` scopes; quick actions post reviews with it in dev mode |
| `GITHUB_USERNAME` | Dev mode | Auto-login user and poller identity |
| `GITHUB_APP_*`, `OAUTH_CALLBACK_URL`, `SESSION_SECRET` | Multi-user mode | GitHub App auth (see `.env.example`) |
| `GEMINI_API_KEY` | For AI reviews | Enables the review pipeline; required even when the first pass runs on another provider |
| `AGENTIC_REVIEWS` | No | Pipe reviews through the selected agent stage after the first pass |
| `QUICK_ACTIONS_ENABLED` | No | Enable `POST /api/prs/quick-action` and the row menu's Approve / Request changes / Comment, posted as the signed-in human |
| `QUICK_ACTIONS_ADMIN_ONLY` | No | Limit quick actions to admins while rolling out |
| `AGENT_BACKEND` | No | `claude` (default) or `openrouter` |
| `REPLY_POLICY_V2` | No | Reply policy for author pushback (default `true`): a reply the model could not verify within its budget abstains with a reaction instead of a canned hold, a hold must cite a file:line, a hold or author thumbs-down marks the finding `contested` so it is not re-posted, and a reply decided against an older head is retaken on the new one. `false` restores the previous posting and ledger behaviour (the reply prompt itself is not switched) |
| `REPLY_VERDICT_FAST_PATH` | No | Author verdict fast path (default `true`): a reply under a PRism finding that rules on it ("intentional", "by design", "won't fix", "accepted risk", "not a bug") dismisses the finding, gets a thumbs-up and no model reply, so it is not raised again on the PR. `false`, or `REPLY_POLICY_V2=false`, sends such replies to the reply model as pushback |
| `AGENT_MODEL` | No | Backend model; OpenRouter defaults to `openai/gpt-5.6-sol` |
| `AGENT_CLONE_CACHE_MAX_GB` | No | Cap on the repo clone caches under `AGENT_CLONE_ROOT_DIR`; least recently used idle caches are evicted past it. `0` (default) is unlimited |
| `OPENROUTER_API_KEY` | OpenRouter backend | Authenticates Codex requests routed through OpenRouter |
| `ENSEMBLE_MAX_AGENTS` | No | Concurrent agent processes the `lite` ensemble may run per server (default 25) |
| `AGENT_PREWARM_REPOS` | No | Comma-separated `owner/repo` clone caches built at startup; unset warms the five most-reviewed repos |
| `NEW_RELIC_ACCOUNT_ID`, `NEW_RELIC_LICENSE_KEY` or `NEW_RELIC_INSERT_KEY`, `NEW_RELIC_REGION` | No | Also send the telemetry events to New Relic (region `US` or `EU`). The events (`PrismReviewRun`, `PrismEnsembleRun`, `PrismLlmRequest`) are always written to stdout as one JSON object per line, which Cloud Logging indexes as structured fields |
| `REVIEW_AGENT_MODELS_*`, `REVIEW_AGENT_EFFORTS_*` | No | Per-backend allowlists for authenticated review API callers |
| `REVIEW_MAX_WALL_CLOCK_SEC`, `REVIEW_MAX_TURNS`, `REVIEW_MAX_FIRST_PASS_SAMPLES` | No | Operator-owned ceilings for per-review overrides |
| `DATABASE_URL` | No | PostgreSQL connection string; unset = SQLite at `DB_PATH` |
| `SERVER_PORT` | No | Default `8080` (docker-compose publishes it on `7769`) |
| `POLLING_INTERVAL` | No | GitHub poll cadence, default `1m` |
| `DISABLE_POLLING` | No | Run purely as an on-demand review API |
| `MENTION_HANDLE` | No | App login mentioned to request a review (`@<handle> review`) by the PR author or a repository owner, member or collaborator; default `prism-pr-review-server`, empty disables |
| `PUBLISH_SETTLE_FOOTER` | No | `false` drops the settle and opt-out footer from posted comments and summaries; default on |
| `PUBLISH_RESOLVE_NOTES` | No | Default `false`: resolving a PRism thread (a fixed finding, or `threadcleanup`) posts no reply; the ledger records the transition. `true` restores the old in-thread notes ("Not seen at <sha7>.", "Resolving: ...") |
| `HEALTH_JOB_TOKEN` | No | Enables `POST /api/health/daily` for a scheduler (header `X-Prism-Job-Token`); the report is stored and readable at `GET /api/health/daily` (`?format=md`) |
| `FEEDBACK_DIGEST` | No | `true` lets the daily health run scan GitHub for author feedback on PRism comments and classify it with a model call; default off. See [Daily health report and author feedback](#daily-health-report-and-author-feedback) |
| `GITHUB_WEBHOOK_SECRET` | No | Secret for the GitHub App webhook at `POST /webhooks/github` (`X-Hub-Signature-256`); empty leaves the endpoint disabled. See [Automatic reviews](#automatic-reviews-for-allowlisted-authors) |
| `PUBLISH_RENDER_V2` | No | Default `true`: summary bullets and inline titles use the finding's headline, else its comment's first sentence, and the impact sentence only when the contract asserts current impact. `false` restores the impact-sentence titles; the word-boundary shortening of headlines over 90 runes (previously blanked) is not switchable |
| `PUBLISH_SAME_COMMIT_GUARD` | No | Default `true`: a review of a commit the summary comment already covers is not published again (API and legacy re-runs, including a full review re-run of a head a lite review already published; its findings stay dashboard-only until the next push). `false` restores re-publication |
| `PUBLISH_REFRESH_SUMMARY` | No | Default `true`: the sticky summary is re-rendered as soon as a reply concedes a finding. `false` waits for the next push |
| `PUBLISH_INLINE_PR_BUDGET` | No | Default `true`: `publish_inline_pr_cap` (default 6) bounds the inline comments PRism posts over the life of one PR, counting resolved and dismissed threads; later findings stay in the summary. `false` leaves only the per-round `publish_inline_cap` |
| `PUBLISH_PR_MUTE` | No | Default `true`: a PR author mutes PRism on one PR with `@<handle> stop` in the conversation or a thumbs-down on the sticky summary; the mute is a flag on the summary ledger row, acknowledged with a reaction only, and `@<handle> review` from the author lifts it. `false` ignores both |
| `AUTO_REVIEW_SYNC_DEBOUNCE_SEC` | No | Default `600` (ten minutes): a pushed head waits at least this long before its automatic review is admitted, so a burst of pushes reviews the last head once; it is admitted on the first poll after the window, so up to one `POLLING_INTERVAL` later. `0` admits every push at once |
| `AUTO_REVIEW_SKIP_UNCHANGED_DIFF` | No | Default `true`: a push whose PR diff has the patch-id of the last automatically reviewed head (a merge from the base branch, a rebase, an amended message) is settled without a review and counted as `reviews_skipped_unchanged_diff` on the daily health report. `false` reviews every push |
| `AUTO_REVIEW_DAILY_ROUND_CAP` | No | Default `true`: a PR gets at most `auto_review_daily_round_cap` automatic rounds per UTC day (setting, default 4; 0 removes the bound); a later push that day is settled as `skipped: daily round cap`, counted as `reviews_skipped_daily_round_cap` on the daily health report, and the sticky summary's footer says "Further pushes today are reviewed on request". Manual reviews (`@<handle> review`, generate-review, the dashboard) never count and are never capped. `false` admits every push |
| `AGENT_PRIOR_FINDINGS` | No | Default off: `true` adds a "findings already raised on this PR" block to the review agent prompt, built from the PR's publication ledger (state, severity, file and lines, headline, author verdicts, under labels `P-1`, `P-2`, ...), and lets the finding contract carry `prior_id` (this finding restates that earlier one) and `relates_to` (distinct, builds on those). References to anything not shown are dropped before the finding is stored |
| `PUBLISH_RENDER_HYGIENE` | No | Default `true`: the renderer strips a confidence label ("Low;", "Medium:") and any internal finding label (`A-1`, `FP-2`) from the sentences under a comment's badge, titles a comment that opens with a compliment by its first sentence that reports something, holds a praise-only finding and a design opinion that admits no impact and calls itself a preference, drops the "Source: PRism · Both" footer, names the folded notes on the "Since last review" line and skips a summary refresh that would change nothing. `false` restores the previous rendering |
| `PUBLISH_LIBRARY_CLAIM_HOLD` | No | Default `true`: a finding that admits it rests on third-party behaviour the agent did not read ("from memory", "cannot inspect the pinned release", "unverified against the pinned version", or a library's defaults asserted as certain) is held: not posted, not listed, recorded as `findings_held` telemetry with `reason=unverified_library_claim`, still on the dashboard. The rule is the admission, not a cited-path check; `AGENT_CLAIM_EVIDENCE` makes the agent write it. A held finding whose thread is already open stays listed as still open. `false` posts such findings |
| `PUBLISH_REGRESSION_EVIDENCE` | No | Default `true`: a finding that says the PR dropped, lost or no longer applies some quoted code is held when the hunk it cites still shows that code on a context or added line and on no removed line (`reason=uncited_regression_claim`). A quoted span on a removed line is the citation the claim needs; one the hunk does not show is left alone. `false` posts such findings |
| `AGENT_CLAIM_EVIDENCE` | No | Default off: `true` appends two evidence rules to every review agent prompt: a claim about a library, framework, action or package must cite the pinned source or a repo doc (else "unverified against the pinned version" at LOW), and a regression claim must quote the removed base line. A prompt change, so it waits for a battery run |
| `PUBLISH_PRIOR_ID_ALIAS` | No | Default `true`: the publisher gives a finding whose contract names a `prior_id` that ledger row's identity before text or subject matching, so a restatement refreshes the earlier thread and a settled point stays settled. `false` matches such findings like any other |
| `PUBLISH_FIXED_NEEDS_HUNK` | No | Default `true`: an absent finding counts as fixed (one "Not seen at" note, thread resolved) only when the push since the row's last-seen head changed its file in a hunk within three lines of the cited line; a change elsewhere in the file leaves the row open with no note. A compare that carries no patch data at all leaves every absent finding open (logged once per round), and a file GitHub lists without a patch proves nothing. `false` restores the file rule |
| `PUBLISH_STATUS_NOTES_V2` | No | Default `true`: one status note per thread per round at most (a return and a severity change are one sentence), and a fixed finding reopens only for a finding that names one of the root's subjects; a text match near a fixed row about other symbols is posted as its own point. `false` restores two notes and reopening on any text match |
| `PUBLISH_RENDER_CLAMP_V2` | No | Default `true`: summary bullets and inline titles that exceed their limit are cut at the end of a sentence, else a clause, else a word, never inside a code span, and end with ` […]`; a contract headline up to 140 runes renders whole in the inline comment. `false` restores the word cut with `...` and the renderer's 110-rune headline and 100-rune title limits (the stored contract headline is normalised at 140 runes either way) |
| `PUBLISH_SCOPE_GUARD` | No | Default `true`: a finding on a path the review scope marks inherited from the base branch makes no fixed, return or severity transition, a path outside the PR diff makes no return or severity transition, and a merge-base change between rounds blocks all three for that round. Active once the stored review carries a scope (`payload.scope`); a review without one is not guarded. `false` turns the guard off |
| `PUBLISH_NOTE_PROBLEM_CLAUSE` | No | Default `true`: a summary bullet, inline title or headline that exceeds its limit keeps the clause that states the problem (after "but", "yet" or "however") with the leading context replaced by `[…]`, unless the leading clause already names the defect or the trailing clause opens with a qualifier or anaphor ("but only when", "but this flag"); and a comment that opens by quoting the PR description is titled by the sentence after it. `false` restores the cut from the right and the first-sentence title; a word cut still avoids ending on a connective or subordinator ("however", "only", "because") and a sentence still ends after any closing quote under either setting |
| `PUBLISH_CROSS_PR_SETTLED` | No | Default `true`: a point the PR author settled (an "intentional" or "won't fix" verdict on a PRism thread, or another reviewer's thread the author answered, standing by the code, that the reviewer then withdrew, or that was resolved after the author's explicit verdict; a thread answered with a fix claim, a bare acknowledgement or a deferral to a follow-up, a later PR, a ticket or "for now" is not a settlement, however it ended, and a deferral after a stand replaces the stand) is remembered per repo and author for thirty days in `settled_points`, recorded by the reply scan as soon as it sees the thread and by every publish round, and a new finding on a later PR of that author in the same file (or in another file when the shared subject is a config key, the one kind of subject that spans files) that shares one of its specific subjects and whose headline or impact sentence sits inside the settled thread at the reconcile bar with four shared words, or whose whole text does with eight (file, subject and text, never fewer) becomes a `settled` ledger row with no root and one folded summary line "Settled earlier on #N" linking the thread ("Settled on #N" when the thread is on this PR); another reviewer's thread on the same PR that the author settled (withdrawn, or resolved after the author's verdict) settles a shown finding the same way, folded notes staying with the note reconcile. A CRITICAL finding is never settled this way. Dismissed rows that predate the table are read back through the reply ledger. `false` matches and records nothing; a row settled while it was on stays settled (terminal, decision 4) and keeps its "settled earlier" line |
| `PUBLISH_PROBLEM_CLAUSE_START` | No | Default `true`: when a summary bullet, inline title or headline drops its leading context clause, the text starts at the problem clause itself, capitalised, with no `[…]` and no connective ("The cache is never invalidated ...", not "[…] but the cache ..."); a contrast inside an unclosed subordinate clause ("Where A but B, C") yields the main clause C; and no rendered bullet, title or headline opens with the marker or a lowercase connective. `false` restores the `[…] but ...` form |
| `PUBLISH_NOTE_SENTENCE_MATCH` | No | Default `true`: a finding is scored against another reviewer's root by the sentence its summary bullet shows as well as by its headline, impact and whole text, so a folded note whose impact sentence concedes nothing today still settles against the resolved thread that raised its point; a summary refresh leaves such a note out too. `false` scores by headline, impact and whole text only |

### First-pass provider

The first pass is provider-selectable, independent of the agent stage:

| Variable | Purpose |
|----------|---------|
| `FIRST_PASS_PROVIDER` | `gemini` (default), `claude`, `claude-code`, or `openrouter` |
| `FIRST_PASS_MODEL` | Model for that provider; defaults per provider |
| `FIRST_PASS_THINKING` | `low`/`medium`/`high`; maps to Gemini thinking and Claude Code `--effort` (unset uses the provider default) |
| `FIRST_PASS_CLAUDE_CODE_TIMEOUT_SEC` | Claude Code CLI timeout in seconds (default 900) |
| `FIRST_PASS_CACHE_STAGGER_SEC` | Seconds between sampled requests so later samples read the first sample's prompt cache (Claude only, default 8) |
| `REVIEW_FIRST_PASS_MODELS_{GEMINI,CLAUDE,CLAUDE_CODE,OPENROUTER}` | Per-provider allowlists for callers selecting a model per run |

The `claude-code` provider runs the headless Claude Code CLI on the operator's
Claude subscription, requires no API key, and defaults to `claude-fable-5-1`.

Benchmarking across model combinations put the strongest pairing at a
`gpt-5.6-sol` first pass with a `claude-fable-5-1` agent stage: different model
families on either side beat using one family for both, because the first pass
casts a wide net and the agent stage is the precision filter over it. Whatever
the first-pass provider, `GEMINI_API_KEY` is still required for the
classification stage.

See `.env.example` for the full reference, including agent tuning (`AGENT_*`), deterministic gates (`GATE_*`), bug memory, and feature flags.

To try GPT-5.6 Sol as the agent stage while retaining Gemini as the first pass:

```bash
AGENTIC_REVIEWS=true
AGENT_BACKEND=openrouter
OPENROUTER_API_KEY=sk-or-...
# AGENT_MODEL=openai/gpt-5.6-sol  # this is already the backend default
```

The OpenRouter path runs `codex exec` in a read-only sandbox with ephemeral state. Agent child environments are constructed from a backend-specific, default-deny allowlist: OpenRouter credentials never enter Claude children, and Anthropic credentials never enter OpenRouter children. OpenRouter's CLI JSONL currently does not report the serving model, so the review records the exact pinned request model as unverified; Claude fallback detection remains stream-verified.

Changing a single review does not change these deployment defaults. If both runtimes are installed and their credentials and policy allowlists are configured, a caller can select OpenRouter for one run while automatic reviews continue using Claude. Query `GET /api/v1/review-capabilities` first; it reports whether each backend is actually ready without exposing credential values.

Configuration snapshots use schema version 3. `max_turns` is explicitly backend-specific: Claude counts assistant stream events, while OpenRouter/Codex counts completed non-reasoning work items and excludes the terminal answer. Capabilities and every durable run expose `turn_budget_unit` and `turn_budget_version`, so clients must not compare raw turn counts across backends as though they represented the same event.

`config.agent.pin_base` (`AGENT_PIN_BASE=true` makes it the deployment default; a request sets it per run either way) reviews a historical PR against a fix-free checkout: a per-PR clone holding only the PR head and the PR's own base, with `origin/<base>` pinned at the merge commit's first parent (the fork point for a squash or rebase merge, the reported base tip for an open PR). The shared cache tracks the live base branch, which for a merged PR already contains its fix, so benchmarks over past PRs must set this; live reviews of open PRs do not need it. With `AGENT_CLONE_CACHE_MAX_GB` set, each pinned PR keeps its own clone under `AGENT_CLONE_ROOT_DIR` for its next run until evicted; without the cap the clone is removed with the review's worktree, so a benchmark deployment that repeats cases sets the cap.

### Recommended configuration

Gates and feature flags all default to off, and the defaults are deliberately conservative. For the strongest reviews, enable the deterministic layer and force the agent to answer it:

```bash
GATE_WIRING=true
GATE_EFFECT_CLEANUP=true
GATE_MIGRATION_RESIDUE=true
GATE_TOPIC_BE_DIR=backend/           # adjust both to your repo layout
GATE_TOPIC_FE_DIR=frontend/
GATE_TASK_PRODUCER_GLOB=**/tasks.py
REQUIRED_CHECKS=true
BUG_MEMORY_OBJECT=bug-memory/bug-memory.json
```

The gates contribute mechanical findings from the diff with no LLM involved, and `REQUIRED_CHECKS=true` is what gives them teeth: each fired gate and bug-memory hit becomes a check the agent must explicitly answer with a VIOLATED / SAFE / NOT-APPLICABLE verdict instead of silently ignoring. The task gate needs only the producer glob; `GATE_TASK_CONSUMER_GLOB` is an optional narrowing. Bug memory pays off once you have a distilled library of past bugs to point it at; start one early.

Two correctness rules ship on by default and have kill switches. `PUBLISH_DISCOUNT_GATE=false` turns off the discount-aware gate: with it on, a finding reaches GitHub only when its contract asserts current impact, names an experiment (`falsifiability: falsifiable`) and does not discount the claim in the same breath (an uncertainty or impact line that opens with "none today", "no user impact", "may be deliberate", "by design", "on purpose" and the like); a CRITICAL is held to the same bar as a MEDIUM, and a self-discounted finding is capped at LOW on the dashboard. `PUBLISH_CONDITION_HEDGE_GATE=true` (default off) extends the gate to an uncertainty line that opens with an unchecked condition ("whether ...", "depends on ...", "I could not verify ..."); on the audit's labelled comments that rule hid 101 correct findings to remove 22 incorrect ones, so turn it on only after accepting that recall cost. `LITE_HYGIENE=false` turns off the lite rules: a CRITICAL that only one of the merged lite runs saw is downgraded to MEDIUM, and a lite run that finished without a tool call that ran (a non-error `tool_result` for Claude; for Codex a completed item other than a missing executable or a bubblewrap refusal, so an empty search that exits 1 still counts) fails instead of publishing what it reasoned out of the inlined diff alone.

The remaining feature flags (`SURFACE_ALERTS`, `CARRY_FORWARD_FINDINGS`, `FINDING_OUTCOMES_ENABLED`, `REVIEW_HISTORY_ARCHIVE`, `QUICK_ACTIONS_ENABLED`) are dashboard and workflow conveniences. They are independent of review quality; enable them as needed. Quick actions post reviews on GitHub as the signed-in user with their own OAuth token, never as the bot; sessions created before the flag shipped have no stored token and show "Sign in again" until the user logs in once more. Approving a draft is allowed only when PRism's verdict is approve with no critical findings and Greptile has reviewed the head with no P0/P1.

### Automatic reviews for allowlisted authors

With the `auto_review_ready_prs` setting on (admin settings page, Publishing
section; seeded off), PRism reviews and comments automatically when a PR by an
author in `publish_enabled_authors` becomes ready for review, opens ready, or
gets a new push. Converting to draft or closing retires queued work. Each PR
head is reviewed once per ready state: the review is forced past the
per-commit cache so a review generated while the PR was a draft does not
stand in for it, a head PRism already commented on is not reviewed again when
the switch is turned on, and a review that finished after the PR went back to
draft is redone when it is ready again.

Pushes are held for `AUTO_REVIEW_SYNC_DEBOUNCE_SEC` (default 600 s, ten
minutes) and admitted on the first poll after the window, so the effective
hold is the window plus up to one `POLLING_INTERVAL`, and a burst of pushes
reviews only the last head. The window restarts on every push, so a PR pushed
to more often than the window is reviewed once the pushes pause. A push that
leaves the PR diff's patch-id unchanged (merging the base branch, rebasing,
amending a message, or changing only trailing whitespace; indentation changes
count as real changes) is settled without a review
(`AUTO_REVIEW_SKIP_UNCHANGED_DIFF`); the intent records the reason as
`skipped: diff unchanged since <sha>`.

A PR gets at most `auto_review_daily_round_cap` automatic rounds per UTC day
from pushes (admin settings page, Publishing section; default 4, 0 removes
the bound), counting the automatic reviews admitted for it since midnight
UTC. A PR becoming ready counts toward the day but is never held back, so a
PR-day can end above the cap. The health report's PR-days cover its 24-hour
window, so the UTC day it opens in is counted from the window's start. A push past
the cap is settled as `skipped: daily round cap` with nothing posted, and the
sticky summary's footer says "Further pushes today are reviewed on request"
from the day's last admitted round until the next day's first. A skipped head
is reviewed on request, when the PR is made ready again, or by the poll on
the next UTC day; new pushes count against the new day. Only completed and
running automatic runs count, so an outage's failed runs and runs that never
started do not use up a PR's day. Only pushes are
capped (`synchronize` deliveries and the poll fallback, which also stands in
for a missed ready-for-review delivery): a PR becoming ready for review
through the webhook is reviewed whatever the day held, and manual reviews
(`@<handle> review`, generate-review, the dashboard) neither count nor are
capped. The daily health report shows `rounds_per_pr_per_day`
(p50, max, PR-days at and over the cap) and the pushes the cap skipped;
`AUTO_REVIEW_DAILY_ROUND_CAP=false` turns the cap off.

Deliveries arrive through the GitHub App webhook; the scheduled poll is the
fallback for a missed delivery. To enable it:

1. Set `GITHUB_WEBHOOK_SECRET` on the server (mount it from your secret store).
2. In the GitHub App settings (`https://github.com/organizations/<your-org>/settings/apps/<your-app>`),
   activate the webhook with URL `https://<your-host>/webhooks/github` and the same
   secret, then subscribe to **Pull request** events. The endpoint acknowledges
   any other event type without storing it, so extra subscriptions are harmless.
3. Send the ping from the app's Advanced tab: a `200` in Recent Deliveries
   confirms the signature setup.
4. Turn on `auto_review_ready_prs` once `publish_enabled_authors` lists the
   authors you want reviewed.

`auto_review_authors` decides who gets automatic reviews when it is set; left
empty it follows `publish_enabled_authors`. It never widens publishing: a
GitHub comment, reply or mention response requires the author to be on
`publish_enabled_authors`, so `auto_review_authors` can be `*` to review every
ready PR on the dashboard while comments stay with the pilot. Bots never get automatic
reviews, `*` included: GitHub App logins (`[bot]`), authors GitHub reports as
bots, and the `ci_status_exclude_authors` list. When the deployment policy
rejects the profile an automatic review resolves to, the review is skipped and
the head stays queued with the reason, rather than running a costlier profile.

### Leaving the comment pilot

`publish_opt_out_authors` (admin settings page, Publishing section) lists the
logins who asked PRism to stop posting. Deny wins: an opted-out author gets no
review comments, inline findings, reply reactions, reply text or mention
responses, whatever `publish_enabled_authors` says, while their reviews keep
appearing on the dashboard. Authors manage it themselves: every PRism root
review comment and summary ends with a footer that links to the dashboard
(and, while `publish_reply_mode` is respond, says how to settle a thread: reply
`intentional` or `won't fix`), where an enrolled
author sees a banner with Leave, and an opted-out author sees Rejoin. The
banner uses `POST` and `DELETE /api/me/publish-opt-out`, and `GET /api/user`
reports `publish_enrolled`, `publish_opted_out` and `enrolled_via` (`login`,
a team slug, or `*`). Gate denials due to an opt-out count as
`opted_out_total` on the daily health report's publication hygiene line.
A mention on an opted-out author's PR still queues a dashboard review but
gets no reaction. `PUBLISH_SETTLE_FOOTER=false` removes the footer.

### Muting one PR

An author who wants silence on a single PR comments `@<handle> stop` (or
`mute`), or puts a thumbs-down on the sticky summary. The stop is answered
with a thumbs-up within a poll cycle; the thumbs-down is read by the one
mute check every posting path makes (a round, the reply scan of a PR with new
activity, a summary refresh) and answered with an eyes reaction, nothing
more. From then on no round, inline comment, reply or reaction reaches that PR,
while reviews keep appearing on the dashboard and a pushed head whose review
was withheld counts as served. Only the PR author can mute, and only the
author lifts it, with `@<handle> review`. The mute is a flag on the PR's
summary ledger row (`published_findings.muted`), counted as `prs_muted` on
the daily health report; `PUBLISH_PR_MUTE=false` turns it off.

### Inline budget

Two caps bound inline comments (admin settings page, Publishing section):
`publish_inline_cap` per round (default 3) and `publish_inline_pr_cap` over
the life of the PR (default 6, 0 removes the bound), counting every inline
root in the ledger whatever its state. A round posts at most what both
leave; the rest of its findings appear in the summary only.
`PUBLISH_INLINE_PR_BUDGET=false` turns the lifetime cap off.

`GET /api/status` reports `webhook.deliveries_24h`, `webhook.last_delivery_at`,
`webhook.intents_queued` and `webhook.oldest_queued_age_sec`, and the daily
health report flags a backlog older than 30 minutes. Clearing
`publish_enabled_authors` stops all publishing; with `auto_review_authors`
also empty, automatic reviews stop too.

`publish_enabled_authors`, `auto_review_authors` and `auto_review_lite_authors` take GitHub logins,
`*`, and team entries: `team:<slug>` (or `@<your-org>/<slug>`, stored as the
`team:` form) enrols whoever is on that team of the `GITHUB_ORG_NAME`
organization, so the list follows team membership instead of being pasted by
hand. Membership is fetched through the installation token the first time a
team entry is matched and cached for 10 minutes; a team that does not exist
or cannot be read matches nobody, and saving a list names the team that
failed. Listing members needs the App's Organization **Members (read)**
permission; if it is missing, saving a list with a team entry fails with the
reason, the server logs one `[TEAMS]` line per team, and a team saved before
the permission was revoked (or the team deleted) shows the error on its chip
and matches nobody. `GET /api/settings` returns the resolved members per team
under `author_list_teams` to every signed-in dashboard user, secret teams
included, so the row menu can apply the publish gate; list only teams whose
membership may be shown that widely.

### Daily health report and author feedback

With `HEALTH_JOB_TOKEN` set, a scheduler posts `POST /api/health/daily` once a
day (header `X-Prism-Job-Token`) and the stored report is readable at
`GET /api/health/daily` (`?format=md` for the newest one as markdown). Each
check is one line with a status; the overall status is the worst line.
The `auto-review backlog` line names the push hold
(`AUTO_REVIEW_SYNC_DEBOUNCE_SEC`), since a head inside it is waiting by
design, and `auto-review rounds per PR per day` gives the p50 and maximum of
automatic rounds per PR per UTC day in the window, the PR-days at and over
`auto_review_daily_round_cap`, and the pushes the cap turned away.

The run also scans GitHub, read-only, for what authors said about PRism's
comments in the last 7 days on PRs the publication ledger knows: replies under
PRism inline comments, conversation comments naming the bot (`@MENTION_HANDLE`
or "prism") and reactions on PRism comments. Each new item is labelled
`happy`, `neutral`, `frustrated` or `very_frustrated` with one Gemini flash
call (the review pipeline's classification model) and a deterministic lexicon
when the model is unavailable, then stored once per comment or reaction id, so
a rerun classifies nothing twice. The report's `author feedback` line carries
the day's counts, every frustrated item (login, a 140-character quote, PR
link) and one happy quote; it turns yellow on any `very_frustrated` item and
red on three or more in a day. Each new frustrated item records a
`feedback_frustrated` telemetry event. `GET /api/health/feedback?format=md|json&days=1|7`
lists the stored items for the last day or week. The scan is off by default
because it classifies with a model; `FEEDBACK_DIGEST=true` turns it on, and the
line says so while it is off.

### Linked ticket context (optional)

The agent otherwise reviews a PR without knowing the intent recorded in its Jira ticket, and will flag deliberate decisions the author documented there. Set `JIRA_BASE_URL`, `JIRA_EMAIL`, and `JIRA_API_TOKEN` (a Jira API token for a service or personal account with read access) and the review fetches the tickets referenced directly in the PR title, body, or branch name (up to 3): summary, status, type, description, and the newest 10 comments. The prompt tells the agent to treat decisions recorded there as intentional, cite the ticket key when a finding touches one, and flag only when the change contradicts the ticket or its rationale no longer holds. `JIRA_PROJECT_KEYS` (comma-separated, optional) restricts which project keys count as references. The PR title and body always reach the agent, with or without Jira; the sidecar's `review_run.linked_tickets` lists the keys that informed a review.

## API

- `GET /api/v1/review-capabilities` — defaults, backend readiness, model/effort allowlists, turn-budget semantics, and override ceilings
- `POST /api/v1/review-runs` — create an exact-target run with optional model, effort, wall-clock, turn, sample, agent, first-pass provider/model, and required-check overrides
- `GET /api/v1/review-runs/{run_id}` — exact status, immutable configuration snapshot, result, models, per-stage timings, and provider-stage attempts
- `GET /api/v1/review-runs?owner=...&repo=...&pull_request=...` — cursor-paginated history; optional `commit_sha` and `status` filters
- `GET /api/review/{owner}/{repo}/{pr}` — legacy/latest structured review JSON (`?format=html` / `?format=md`, `?sha=`, or `?sha=<sha>&run_id=<id>`)
- `POST /api/prs/generate-review` — backward-compatible review creation for callers that do not need customization
- `GET /api/status` — health check, including webhook ingress counters
- `GET /api/health/daily`: stored daily health reports (`?format=md` for the newest as markdown); `POST` with `X-Prism-Job-Token` runs one
- `GET /api/health/feedback`: author feedback on PRism comments (`?format=md|json`, `?days=1|7`)
- `POST /webhooks/github` — GitHub App webhook (HMAC-authenticated; `pull_request` events feed automatic reviews, other events are acknowledged)

Create a customized exact-head run with the bundled client:

```bash
export PRISM_BASE_URL=https://prism.example.com

skills/claude/prism-review/scripts/prism.sh capabilities
created=$(skills/claude/prism-review/scripts/prism.sh create acme/widgets#42 \
  --backend openrouter \
  --model openai/gpt-5.6-sol \
  --effort high \
  --wall-clock-seconds 900 \
  --max-turns 120)
skills/claude/prism-review/scripts/prism.sh wait "$(printf '%s' "$created" | jq -r .run_id)"
```

The client resolves the full current GitHub HEAD and sends it as
`expected_head_sha`; the API rejects a moved target with `409`. Creation
accepts an `Idempotency-Key`, returns `202` with `Location` and `Retry-After`,
and persists both requested and effective configuration before execution.
Separate executions of the same PR commit are distinguished by opaque unique
`run_id` values. Successful artifacts are immutable run-scoped objects, while
the PR row remains only the latest published projection.

## Published pages

The server can host standalone HTML pages (a slide deck, a report) at `/pages/<slug>/`. Pages live in the review bucket (or `REVIEWS_DIR` locally), never in the repo:

```
pages/<slug>/index.html            the page, shown to signed-in org members
pages/<slug>/meta.json             {"title": "...", "description": "..."}
pages/<slug>/preview.png           1200x630 link-preview image
pages/<slug>/favicon.svg           optional icons
pages/<slug>/favicon.png
pages/<slug>/apple-touch-icon.png
```

Signed-out visitors, including Slack and iMessage link unfurlers, get a card with the title, description and preview image as Open Graph tags, plus a sign-in link that returns to the page. The preview image and icons are public; the page itself is not. Slugs are lowercase letters, digits and hyphens.

```bash
gsutil -m cp index.html meta.json preview.png favicon.svg favicon.png apple-touch-icon.png gs://<bucket>/pages/<slug>/
```

## Themes

The dashboard ships twenty themes — One Dark/Light, GitHub, Gruvbox, Solarized, Monokai, Dracula, Nord, Night Owl, Tokyo Night, the four Catppuccin flavors, Everforest, Rose Pine and SynthWave '84. Pick one from the **Theme** control in the header; the choice is a per-browser preference stored in `localStorage` under `prism.theme.v1` and re-applied before the first paint, so it survives reloads without flashing the default. With nothing stored, the OS light/dark preference decides.

Each theme is a block of CSS custom properties in `frontend/src/styles/themes/_palettes.scss`, selected by `data-theme` on `<html>`; `_derived.scss` computes the handful of tokens the palettes don't carry, and `styles/abstracts/_variables.scss` maps the Sass variables components use onto those tokens. To add a theme, add its palette block and list it in `VALID_THEMES`/`THEME_OPTIONS` in `frontend/src/utils/theme.ts` — the tests fail if the two ever disagree, or if a palette omits a token the others define. Components should reference the `$color-*` variables rather than literal colors, and reach for the `*-dim` tokens instead of `rgba()` (the Sass color functions can't operate on a `var()` reference).

## Auxiliary tools

- `go run ./cmd/gatecheck <worktree-dir> <base-branch>` — offline report of what the deterministic layer (gates + bug memory) would contribute for a diff; no LLM calls or API keys needed
- `go run ./cmd/publishreplay --dumps <dir> --sidecars <dir>`: replays historical publication rounds through the publisher against an in-memory ledger and prints repost, resolution and output hygiene metrics; needs `PRISM_BASE_URL` to fill the cache, or `--offline`; `--render-hygiene=false --library-claim-hold=false --regression-evidence=false` replays without the output hygiene gate for a before figure; baseline in `benchmark/replay/BASELINE.md`
- `go run ./cmd/threadcleanup --dumps <dir> [--cache <dir>]`: lists the PRism review threads still open on merged or closed PRs, or whose cited file changed after the comment, with a count summary; `--pr owner/repo#N --dry-run=false` resolves them through the publisher's thread resolver without a reply (`PUBLISH_RESOLVE_NOTES=true` adds the one-line note), behind the publish gate and `PUBLISH_THREAD_RESOLUTION`
- `skills/claude/prism-review/` — Claude Code skill for fetching and acting on reviews; install by copying it to `~/.claude/skills/prism-review/`

## Development

```bash
go test -race ./...                                        # backend tests
golangci-lint run --timeout=5m                             # lint
cd frontend && npm run lint && npm run type-check && npm test
```

For frontend work, `npm run dev` in `frontend/` starts a Vite dev server on port 3000 that proxies API calls to the Go server (`BACKEND_URL`, default `http://localhost:7769`).

### Approval candidates

An optional dashboard investigation finds PRs whose existing reviews support a quick human approval decision. It combines PRism, Greptile, Copilot, human-review and CI evidence with bounded code inspection. Recommendations never submit an approval. The feature is disabled by default; see [configuration, limits and rollout](docs/approval-candidates-operations.md) and the [implementation specification](docs/approval-candidates-spec.md).
