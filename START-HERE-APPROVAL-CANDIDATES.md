# START HERE: approval candidates follow-up

**Status: investigation complete; the fixes below have not been implemented.**

## Resume checklist

1. Resume on `feature/approval-06-validation`, the top of draft PR stack [#140](https://github.com/zbedforrest/pr-review-server/pull/140) through [#145](https://github.com/zbedforrest/pr-review-server/pull/145). The implementation at the time of diagnosis was `482b5cb`.
2. Start with polling amplification, rate-limit handling and retained diagnostic errors. Then fix evidence-read completion, conversation-size handling, empty check suites and representative local review data.
3. Preserve evidence validation, user isolation, pinned revisions and resource limits. An investigation must not trigger new provider reviews, post comments or approve PRs.
4. Validate one known reviewed PR before another broad scan. A full scan previously exhausted GitHub's API quota.
5. Keep every PR in the stack in draft mode.

Already implemented: scans of up to 50 PRs, clickable disclosure bar, animated prism progress, a lightweight progress endpoint and optional short Flash-Lite activity summaries. These changes are pushed. The findings below remain open.

Development processes stop on reboot. Build the frontend before building the Go server; run the Vite development frontend on port 3000 against a backend on port 7769. Keep the existing isolated development database, and provide authentic review history before interpreting candidate yield. Do not point diagnostic writes at production.

Related documents: [specification](docs/approval-candidates-spec.md), [operations](docs/approval-candidates-operations.md).

---

## Investigation of the 36-target local scan

Inspected the live local database and server logs without modifying scan history. Ran one controlled reproduction using a private database copy, the original frozen snapshot and pinned code. Source code is unchanged at commit `482b5cb`.

## 30 access failures: GitHub quota exhaustion

All 30 targets failed before any model call, between 08:59:00 and 08:59:18 Pacific on September 29. Server logs shortly afterward explicitly report GitHub API rate-limit exhaustion and repeated HTTP 403 responses. The access helper converts every GitHub error into a boolean false, and the worker converts that into `access_unavailable`, immediately consuming the rest of the queue.

The dashboard's candidate feed and selected-scan feed perform a live GitHub PR GET per target on each refresh. An earlier isolated reproduction showed 70 GitHub GETs per paired poll for 35 targets. The previous two-second refresh made this a major source of request pressure; the new ten-second evidence refresh reduces pressure but does not remove the per-target fanout. Other application calls share this quota, so the logs cannot attribute every exhausted request exclusively to this scan.

Locations: `server/approval_api.go:422`, `server/approval_api.go:435`, `server/approval_worker.go` access checks. Fix by sharing bounded freshness reads, preserving authorization and generation fencing, and pausing/requeuing on GitHub rate limits until their reset instead of reporting permission failures.

## Four investigation failures: underlying errors were discarded

All four made multiple successful model calls, settled all usage reservations, and accumulated 4 to 7 rounds. The worker discarded their original errors and retained only `investigation_failed`, so their exact historical error strings cannot be recovered.

A controlled rerun of the second target reproduced `invalid_assessment: unread evidence artifact`. The agent listed evidence, inspected code and tried to return a final assessment without reading a required review comment. The validator correctly rejected this. The original second target had a similar 4-round execution, but the replay cannot prove the exact original error or establish that all four failed for this same reason.

Locations: `pkg/approval/investigator.go:63`, `server/approval_worker.go:357`. Keep structured failure details and use a bounded correction round for missing required evidence or invalid output rather than silently weakening validation.

## Two resource-limited runs were labeled completed

Both retained `budget_exhausted` and insufficient-evidence decisions, not valid assessments. They used 5 and 8 model rounds, 22 and 11 tools, and approximately 79 KB and 80 KB of tool output. They were well below the 600,000 input-token, 12,000 output-token, 16-round and 40-tool ceilings. Their final reservations were successfully settled tool calls, with no pending reservations.

The execution path and counters point to the separate 90,000-byte serialized conversation cap at `pkg/approval/investigator.go:154`. Escaping tool JSON and retaining earlier messages increases the serialized size beyond raw tool-output bytes. The precise original error text was not retained, so this conclusion is inferred from the code and counters rather than a saved trace. Report the specific limit and compact or page retained context within enforced resource limits.

The fallback assessment also generates `invalid_assessment` from an empty assessment, making displayed reason lists noisier than the underlying resource-limit failure.

## Local evidence setup prevents candidates independently

The isolated development database contains zero PRism review runs. All six collected snapshots contain no PRism sources, and none contains a verified completed provider review tied to the current head. Greptile artifacts are recognized, but remain unknown-completion or older/unknown-revision evidence. Connecting to production review history read-only, or importing authentic review fixtures, is necessary for representative local testing. This should not trigger new provider reviews or weaken provenance checks.

All six snapshots also contain eight pending check suites. A live check of one pinned revision found eight queued suites with zero check runs, alongside completed successful suites. `pkg/approval/collector.go:287` turns every non-completed suite into pending CI without distinguishing empty suites from actual scheduled checks. The [GitHub checks documentation](https://docs.github.com/en/rest/guides/using-the-rest-api-to-interact-with-checks) distinguishes suites from their child runs. Correct handling needs to retain actual pending runs and review requests while avoiding unconditional blocking on empty suites.

These known eligibility gates should be evaluated before spending investigation tokens where the model cannot change their outcome.

## Recommended order

1. Remove polling request amplification and retain rate-limit retry/reset metadata.
2. Preserve structured diagnostic errors and specific resource-limit reasons.
3. Add bounded evidence-read/output correction and address the serialized-context cap.
4. Correct empty-suite handling and provide authentic local review history.
5. Recheck a single known reviewed PR before another broad scan.

No GitHub reviews or comments were created during this investigation. No production data or live scan results were changed. The diagnostic replay and its metadata remain local.
