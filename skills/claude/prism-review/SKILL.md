---
name: prism-review
description: Fetch a PRism AI review for a GitHub PR, investigate the cited code, and report your own assessment with suggested next steps. Default behavior is read-only review and recommendation, NOT applying fixes. Use when the user says things like "read the PRism review for PR X", "fetch the prism review for owner/repo#N", or any variation referring to a "PRism" or "prism" AI review. The skill calls the prism server's `/api/review/{owner}/{repo}/{pr}` endpoint, prints the review HTML, and then you read the cited files and write up what you found. Only apply changes if the user explicitly asks (e.g. "apply the fixes", "handle the suggestions").
---

# prism-review

PRism is a privately-hosted AI code review service. When the user asks you
to handle the PRism review for a PR, this skill fetches it from the prism
server and hands the body back for you to act on.

## How to invoke

Run the fetch helper:

```bash
~/.claude/skills/prism-review/fetch.sh <pr-ref>
```

Where `<pr-ref>` can be any of:

- A bare PR number (e.g. `42`) — resolved against `git remote get-url origin`
  in the current directory.
- `owner/repo#N` shorthand (e.g. `acme/example#6`).
- A full GitHub PR URL (e.g. `https://github.com/acme/example/pull/6`).

The script prints a metadata header followed by `=== REVIEW HTML ===` and
the raw HTML body of the review. Read the HTML directly — it is structured
markup (headings per file, code blocks with line numbers, severity tags).

## What to do with the review

**Default mode is read-only assessment, not application.** Investigate the
code, form your own opinion, and report back. Do NOT edit files unless the
user explicitly asked you to apply / fix / handle the suggestions in their
original message. If unsure, ask before editing.

0. **Check the status header first.** Three states can come back:
   - `Status: review is in flight` — no HTML yet. Tell the user the review
     is still generating and that they can retry in ~30 seconds (or you can
     poll for them by re-running the skill).
   - `Status: review generation FAILED` — surface the error message to the
     user and suggest re-triggering from the prism dashboard. Do NOT try to
     act on a missing review.
   - Normal output (`PR: …` / `=== REVIEW HTML ===`) — continue below.
1. **Check `Stale: true` in the header.** If true, the review was generated
   against an older commit than the PR's current HEAD. Surface this to the
   user as a one-line warning before going further; offer to regenerate.
   Also note `In flight (regenerating): true` — that means a fresh review
   is being computed right now, so the HTML you're reading will be
   superseded shortly. Mention this to the user before acting.
2. **Walk the findings in severity order**: critical → medium → low. The
   review groups them by file and cites line numbers.
3. **For each finding, investigate before judging.** Read the cited file at
   the cited lines AND enough surrounding context to understand intent —
   callers, tests, related modules. PRism can be confidently wrong about
   intent; don't take its word.
4. **Form an independent assessment.** For each finding, decide:
   - **Agree** — the issue is real and the suggested direction is sound.
   - **Agree on problem, different fix** — issue is real but PRism's proposed
     change is wrong/incomplete; describe what you'd do instead.
   - **Disagree** — explain why (intentional behavior, missing context,
     covered by tests, etc.).
   Also flag anything *you* noticed while reading the code that PRism missed.
5. **Report back, don't edit.** Output a structured assessment:
   - One line per finding: severity · file:line · your verdict · recommended
     next step (one short clause).
   - A "Things PRism missed" section if applicable.
   - A "Suggested next steps" section ordered by priority.
   End with: "Want me to apply any of these?" so the user can opt in to
   edits. Only edit files if they say yes (or if their original message
   already said "fix" / "apply" / "handle").

## Auth

The script uses `Authorization: Bearer $(gh auth token)` by default. If the
user has the `gh` CLI authenticated, no setup is needed. Override with
`PRISM_TOKEN` env var if necessary.

## Server URL

`PRISM_BASE_URL` is **required** — set it to your prism deployment's base
URL (e.g. `https://prism.example.com` in prod, `http://localhost:8080`
when running the server locally). The skill exits with an error if it's
unset.

## Pinning to a specific commit

Set `PRISM_SHA=<short_or_full_sha>` to fetch a review for a specific commit
rather than the latest review on the PR. Useful when comparing what changed
between review generations.

## Failure modes

- Exit 2: bad PR ref or could not resolve owner/repo from git remote.
- Exit 3: no auth token available (run `gh auth login` or set `PRISM_TOKEN`).
- Exit 4: server returned non-200. Body is printed to stderr; common cases:
  - 401 — token rejected by prism server (your GitHub login isn't registered).
  - 404 — PR not in prism's database, or no review has been generated yet.
    Tell the user to trigger a review from the prism dashboard first.
