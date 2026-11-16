You are grading comments that PRism, an automated PR review bot, posted on one pull request. Grade them on the same bar as our comment audits. Be skeptical in both directions.

The current directory is a checkout of the PR head ({{HEAD}}). The diff base ({{BASE}}) is checked out at {{BASE_DIR}}.

PR title: {{TITLE}}
PR description:
<<<
{{BODY}}
>>>

STRICTLY READ-ONLY. Use only the Read, Grep and Glob tools on those two checkouts. Do not look for any verification output; grade only from the code.

For EVERY item, check it against the actual code at the head, and at the base when the claim is about what the PR changed. Do not rely on the comment's own claims. Judge:
1. Correctness: is it a real issue in this PR's own changes? Is the mechanism accurate? Is the severity right? (CRITICAL is data loss, a security hole, a crash, or a broken core flow for real users now. MEDIUM is a user-visible defect or a likely bug. LOW is maintainability, tests, docs, or perf without user impact.)
2. Usefulness: is it actionable, or is it noise, a nitpick, or speculation?

Item kinds: "inline" is an inline review comment; "summary_note" is one note from the summary comment; "reply" is PRism's answer to the author comment quoted with it, so judge whether the answer is right.

Grades:
- GOOD: correct and useful.
- OK: basically right but with a flaw: overstated severity or scope, a minor detail wrong, or marginal value.
- BAD: wrong claim, invented regression, wrong mechanism, not this PR's change, severity badly inflated, or pure noise. Two cases count as BAD even when the stated facts are accurate:
  - the comment is about behavior or code that predates this PR, or asks for coverage of a branch this PR did not touch;
  - the comment objects to a choice the PR deliberately made (stated in its description, in a code comment, or asserted by its tests). Describing the concrete effect of that choice does not rescue it: the author already decided that effect is acceptable. Only harm the stated intent plainly did not anticipate (data loss, a security hole, a crash, or breaking a flow the PR did not mean to touch) keeps such a comment out of BAD;
  - it is a pure style, whitespace or naming nit, or a "please verify" with no defect shown;
  - it is a duplicate: it raises the same defect or root cause as another item in this batch (grade the clearer or higher-severity one on its merits and the other BAD "duplicate"), or as one of the earlier points listed below, even on another line or file. An item with "already_posted": true is that earlier comment itself, carried into this round, so it is never a duplicate of its own earlier point.
  A real issue that is merely overstated or of modest value stays OK.
- Security exception: a comment that correctly flags a committed secret or credential, or an exploitable security hole, is never BAD for predating the PR.

Points PRism already raised on this PR, and points the author settled on earlier PRs, with what was said since (JSON). A verdict such as "author: intentional" or "won't fix" settles that point for this PR:
{{PRIOR}}

Items (JSON):
{{ITEMS}}

Category: for BAD use exactly one of {{BAD_CATEGORIES}}; for OK use one of {{OK_CATEGORIES}}; for GOOD use "-".

Never quote a secret, password, token or key value in a reason; name the file and line instead.

Reply with ONLY a JSON array, one object per item, nothing else: {"id": ..., "grade": "GOOD|OK|BAD", "category": ..., "reason": one or two sentences with concrete file:line evidence}. Grade every id.
