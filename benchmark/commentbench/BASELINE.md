# Comment machinery benchmark baseline

`cmd/commentbench` replays historical PRs through the real publisher (`publisher.Publish` via
`poller.BuildPublishRound`) and the real reply reactor (`publisher.ReplyReactor`) in-process, with an
in-memory SQLite ledger and a recording fake GitHub, and scores what they do against what the
comment-system audit says PRism should have done. It shares its dump, sidecar and replay plumbing with
`cmd/publishreplay` through `internal/replaykit`.

Measured with this branch, whose `pkg/publisher` is identical to master's, replies in stub mode,
shipped publisher defaults (inline cap 3, minimum severity medium, unverified claims folded).
Date: 2026-10-02.

## Dataset

Built from the 203 audited PRs. Cases name real repositories and people, so they live outside git.

| tier | cases | rounds (with sidecar) | findings post / suppress / unmapped | summaries | resolutions | replies |
|---|---:|---:|---:|---:|---:|---:|
| gold | 50 | 121 (119) | 59 / 52 / 6 | 65 | 69 | 68 |
| accepted | 106 | 273 (269) | 174 / 140 / 6 | 136 | 121 | 91 |
| excluded | 47 | | | | | |

Excluded: 42 summary-only PRs (no inline comment, so no review submission to replay), 3 refuted by a
verifier as a whole, 2 left with nothing scorable (one after its refuted comments were dropped).

## Metrics (master)

| metric | gold | accepted |
|---|---:|---:|
| cases | 50 | 106 |
| rounds replayed | 119 | 269 |
| findings that should post / be suppressed | 59 / 52 | 174 / 140 |
| post precision | 0.569 | 0.601 |
| post recall | 0.983 | 0.977 |
| suppression recall | 0.154 | 0.193 |
| reposts | 4 | 41 |
| fixed flips / wrong / unconfirmed | 42 / 11 / 8 | 151 / 42 / 21 |
| summary-count correctness | 0.446 of 65 | 0.272 of 136 |
| thread-resolution recall | n/a of 69 | n/a of 121 |
| reply decision accuracy | 0.476 of 63 | 0.557 of 88 |
| replies whose thread the replay did not post | 5 | 3 |
| unlabelled posts / unmapped expectations | 5 / 6 | 44 / 6 |

| reply class | gold correct / expected | accepted correct / expected |
|---|---:|---:|
| ack | 3 / 4 | 1 / 2 |
| fix_claim | 5 / 10 | 28 / 42 |
| frustration | 1 / 3 | 2 / 6 |
| intentional_behavior | 14 / 25 | 11 / 22 |
| pushback | 7 / 21 | 5 / 13 |
| question | 0 / 0 | 2 / 3 |

Stub mode made 33 (gold) and 47 (accepted) reply decisions without a recorded one; see Limits.

## What the numbers mean

- Post precision: of the replayed roots matched to an audited comment, the share the audit says should
  have been posted. Suppression recall: of the audited comments that should not have been posted, the
  share the replay did not post. A comment should post only when it adds context, is not a repost, was
  not addressed before the post, was first raised by PRism and is not incorrect; otherwise the reason is
  recorded (`repost`, `addressed_before_post`, `external`, `incorrect`, `no_context`).
- Reposts: replayed roots whose marker or alias (same file, line within 10, raw Jaccard >= 0.20 or a
  shared subject) matches a root from an earlier round.
- Wrong fixed: a ledger row moved out of open when the case has no fix for that defect by then and the
  defect is still raised under new wording, comes back later, or its cited file did not change.
  Unconfirmed: a flip with none of that evidence either way.
- Summary-count correctness: rounds whose "Since last review" new, still open and fixed counts,
  restricted to labelled findings, equal the expectation. Expected fixed = a fix claim whose commit landed since the
  previous round, or the defect absent from the review while its cited file changed (policy R5); a
  defect settled by an author verdict is no longer counted.
- Thread-resolution recall: expected resolutions (fix, absence with file change, author verdict) whose
  thread the publisher resolved. Not applicable until `publisher.GitHub` has `ResolveThread`
  (W2-3); the recorder already implements it.
- Reply decision accuracy: replies whose replayed action (none, react_only, concede, hold, answer,
  withdraw) is one the audit accepts, and, where the verdict settles the thread, whose ledger row ends
  dismissed. Replies whose thread the replay never posted are counted separately, not scored.

Tiers: gold when every expectation on the PR is backed by a verifier-confirmed pattern example or by a
settling human reply (fix claim, intentional behaviour, pushback), and no verifier refuted any of its
comments, and every finding was found by its marker in the round's sidecar (a looser file, line and
text match is accepted only); accepted for analyst labels only; excluded when refuted as a whole, `read_ok` is false or
nothing can be replayed. Refuted comments are dropped from their case.

## Rebuild the dataset

```
F=<audit folder>   # holds workflow_result.json, prs/, sidecars/, reply_ledger.json
PRISM_BASE_URL=<review server> go run ./cmd/commentbench build \
  --audit $F/workflow_result.json --dumps $F/prs --sidecars $F/sidecars \
  --reply-ledger $F/reply_ledger.json --out $F/benchmark
```

The sidecar cache is the one `cmd/publishreplay` fills; missing sidecars and compares are fetched the
same way (`PRISM_TOKEN` or the gh token; compares through `gh api`). `--offline` builds from the cache
only. Output: `$F/benchmark/manifest.json` (case id, tier, why, expectation counts) and one
self-contained case per PR under `$F/benchmark/cases/`.

## Run it

```
go run ./cmd/commentbench run --cases $F/benchmark --json $F/benchmark/result.json --md $F/benchmark/result.md
```

`COMMENTBENCH_DIR` can replace `--cases`. The Markdown report lists every per-case difference
(`posted_should_not`, `suppressed_should_post`, `repost`, `wrong_fixed`, `summary_mismatch`,
`wrong_reply`, `not_resolved`); it names real PRs, so keep it outside git. `--anonymise` prints case
ids as `case-N`. To gate a publisher PR on gold, pass thresholds; the command exits 1 when one fails:

```
go run ./cmd/commentbench run --cases $F/benchmark \
  --min-gold-post-precision 0.56 --min-gold-suppression-recall 0.15 --max-gold-reposts 4 \
  --max-gold-wrong-fixed 11 --min-gold-summary 0.44 --min-gold-reply-accuracy 0.47
```

`--replies=live` runs the real reply agent (`service.RunAgentReply`) for every eligible reply instead of
the stub: one billed agent run per reply, up to `REPLY_MAX_TURNS` (20) and `REPLY_WALL_CLOCK_SEC`
(180 s), with a fresh clone of the PR head, so it needs read access to every case's repository and the
usual agent credentials (`AGENT_BACKEND`, `REPLY_MODEL` or `AGENT_MODEL`, API keys). The default stub
replays the decision the reply ledger recorded for that author comment; without one it applies the
class rule those decisions follow (fix claim conceded, question answered, intent claim conceded, other
pushback held with a cite).

## Regression tests

`go test ./cmd/commentbench/...` replays three anonymised fixtures under `cmd/commentbench/testdata/cases`
(author pushback then a reworded repost; a fix commit next to a finding that is only absent and comes
back; a Greptile duplicate, a same-commit rerun and a fix). Each test pins the posts, replies, ledger
states and summary lines, and lists master's remaining differences as known gaps with the item expected
to remove them. A publisher PR that removes a gap deletes that entry; one that adds a difference fails.
`TestBuildReproducesThePushbackFixture` checks that the builder derives the same expectations from an
audit entry.

## Add a case from a new PR

1. Dump the PR in the audit's format (GraphQL `reviews`, `reviewThreads` with every comment and
   `replyTo`, `commits` with `committedDate`) as `<owner>__<repo>__<number>.json` in the dumps folder.
2. Write a label file `{"perPr": [...], "verified": [...]}` in the audit's shape: the PR `key`
   (`owner/repo#number`), `read_ok`, one `comments` entry per PRism root (`comment_id`, `path`, `line`,
   `correctness`, `first_raised_by`, `redundant_repost`, `repost_of_comment_id`,
   `addressed_before_post`, `adds_context`) and one `human_responses` entry per human reply
   (`comment_id`, `login`, `created`, `class`, `quote`, `prism_replied`, `prism_reply_quality`).
   For gold, back each comment with a settling reply or a `verified` example naming the PR (and its
   comment ids) under `examples_confirmed`.
3. Rebuild with `--labels <file>` added (repeatable) and check the case's line in `manifest.json`.
4. To make it a regression test, copy the case file under `cmd/commentbench/testdata/cases`,
   anonymise it (`acme/example`, invented logins, rewritten prose and paths, fresh ids), and add a test
   in `e2e_test.go` that pins its outcome and known gaps.

## Limits

- A round is a PRism review submission; rounds that posted no inline comment are invisible, so
  summary-only PRs cannot be replayed.
- Same-commit reruns replay one sidecar twice (the live system overwrote it); see the replay baseline.
- Findings whose round sidecar no longer holds them are reported as unmapped and not scored (6 gold).
- Expected summary counts and fix rounds are derived from the labels, reply text, commit dates and the
  three-dot compares; a rebase can widen a compare and make a fix look corroborated.
- In stub mode a reply the audit judged good is replayed with the decision PRism actually made, so
  stub accuracy measures the reactor (eligibility, caps, rendering, ledger state) rather than the
  model, and stub decisions without a ledger record are a rule; use `--replies=live` to measure the
  model itself.
- Replies by someone other than the PR author are scored: PRism ignores them by design, and the audit
  judged some of those silences wrong.
