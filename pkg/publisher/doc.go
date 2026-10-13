// Package publisher posts PRism reviews to GitHub and keeps the publication
// ledger (db.PublishedFinding) that remembers what was posted.
//
// # Ledger decision table
//
// Publish applies this table unless Policy.LegacyLedger is set
// (PUBLISH_POLICY_V2=false). It is the specification the replay harness
// (cmd/publishreplay) asserts against.
//
// Identity. Every current finding is matched to at most one ledger row, in
// this order: its fingerprint equals a row's; its contract's prior_id names
// a row (Policy.PriorIDAlias, PUBLISH_PRIOR_ID_ALIAS, on unless set to
// false: the agent was shown the PR's ledger rows under AGENT_PRIOR_FINDINGS
// and said this is the same point, so the match is exact, one finding per
// row, and a prior_id the ledger does not hold is ignored); same file, line within ten of
// the row's comment line (the bucket centre when GitHub shows no comment),
// raw-text Jaccard >= 0.20 against the row's CommentText (the stripped
// posted body when the row predates the column); same file and the same
// sorted contract subjects, any line; the same kind and subjects in another
// file when the row's kind is known and the finding names two subjects or
// shares two words with the row. A matched finding takes the row's
// fingerprint. A row whose fingerprint a current finding still carries
// verbatim is never given to another finding.
//
// Settled points. A finding the tiers above left unmatched is dropped when
// it restates a settled row, dismissed, contested, external or fixed, from
// another file or another part of its own file (Policy.SettledAlias,
// PUBLISH_SETTLED_ALIAS, on unless set to false): the kinds agree or one is
// unknown, a specific subject of the finding (a symbol, selector or key,
// never a bare file or workflow) is one of the row's subjects or is named
// in the row's thread (the root's prose plus PRism's own replies there,
// which Round.PriorComments carries), and the finding's text sits inside
// that thread text (containment at or above 0.35 with eight shared words,
// or its headline at or above 0.5 with four). The row is untouched: a
// conceded or dismissed point never returns from elsewhere, and nothing is
// posted or counted. An exact or near return on the row's own lines is
// still the table's business below.
//
// Inputs per prior row: its state; whether a current finding matched it
// (present); whether HeadSHA equals the row's LastSeenSHA; the change set
// between LastSeenSHA and HeadSHA (Round.Changes, unknown when nil or when
// the lookup fails); whether the lines the finding cites are inside that
// change set (anchor changed: a changed line within three of the cited line
// when the set carries lines, a hunk within three lines on the head side
// when it carries hunks, else the file having changed); and whether the
// row's cited lines sit in a changed hunk (hunk changed: the row's stored
// Line, in the base's coordinates, within three lines of a hunk's base-side
// range; for a row written before the column, the line GitHub shows for
// its comment against the head side, else the fingerprint's ten-line
// bucket; unknown when the set carries no hunks for the file or no hunks at
// all, and taken as the file having changed when Policy.FixedByFile restores
// the file rule).
//
//	state                        present  same head  hunk changed  result
//	open                         yes      any        any           still open; LastSeenSHA = head; Line = cited line; severity clamp (below)
//	open                         no       yes        n/a           still open, untouched
//	open                         no       no         yes           fixed: thread resolved with no reply (Policy.ResolveNotes adds "Not seen at <sha7>")
//	open                         no       no         unknown       still open, untouched, nothing posted; counted in Report.HunksUnknown and logged once when the compare had no hunks at all
//	open                         no       no         no            still open, untouched, nothing posted
//	fixed / resolved (legacy)    yes      any        any           reopened: open, LastSeenSHA = head, one reply "Back at <sha7>" in its thread, thread unresolved, never a new root
//	fixed (conceded, see below)  no       no         any           counted as fixed once; Rounds = this round
//	fixed / resolved             no       any        any           untouched
//	dismissed/contested/external yes      any        any           finding dropped: not posted, not counted, row untouched (exception below); an external row keeps its summary link
//	dismissed/contested/external no       any        any           untouched
//	no row, another reviewer's   new      n/a        n/a           external row: no root, summary links to that root, one delta reply there (see Other reviewers)
//	root on the point
//	no row                       new      n/a        n/a           Select decides inline or summary; a shown finding gets an open row
//
// Returns. A fixed row reopens only for a finding re-detected on the root's
// subject: when both the row and the finding name subjects and share none,
// the text match is another point near the fixed one, so the finding keeps
// its own identity and Select treats it as new. A row or finding without
// subjects is matched as before. Policy.LegacyStatusNotes
// (PUBLISH_STATUS_NOTES_V2=false) reopens on any match.
//
// Scope. With a review scope on the round (Round.Scope, from the payload's
// scope) and Policy.NoScopeGuard off (PUBLISH_SCOPE_GUARD), a finding on a
// path the scope marks inherited from the base branch makes no transition:
// an absent row on it is neither fixed nor counted still open, a present
// finding on it does not reopen a fixed row (the finding is dropped for the
// round) and its severity stays the row's. A path outside the PR diff
// GitHub lists (Round.DiffFiles) makes no return and no severity change but
// may still be fixed by the hunk rule. The summary row remembers the
// merge-base the last published review diffed against (empty after a round
// without a scope); a review against another base is base drift, and that
// round makes none of those transitions for any row. Nothing is posted in
// any of these cases.
//
// Already published. Select never offers a root for a finding whose row
// has a comment id, whatever the row's state; a row without a comment (a
// summary-only annotation) may still be promoted inline when its line
// becomes commentable. The round cap (Policy.InlineCap) shrinks to what the
// PR cap (Policy.InlinePRCap, publish_inline_pr_cap) leaves after the inline
// roots the ledger already holds in any state; findings over either cap
// stay summary-only annotations.
//
// Severity clamp. A finding matched to a non-terminal row keeps the row's
// severity unless its anchor changed; an accepted change is written to the
// row and, when the row has a thread, announced there once ("Severity
// medium to critical at <sha7>: the cited lines changed.").
//
// Status notes. A thread gets one status note per round at most: a return
// and a severity change on one thread are one reply ("Back at <sha7>;
// severity low to medium: the cited lines changed."). Policy.LegacyStatusNotes
// posts them as two.
//
// Terminal exception. A CRITICAL security_risk matched to a terminal row
// may post once more, as a fresh root whose text names the change, when
// its anchor changed since the row was settled and no other row exists for
// the point (same file, same subjects, any state). Once the fresh root
// exists, a later wording that aliases to the terminal row is handed to the
// newest live row on the point (refreshed, or reopened in its thread) and
// dropped when every row on the point is terminal; a third root is never
// posted. The fresh root carries the settled row's subjects and, when the
// wording and line did not move, a marker minted from the text that names
// the change, so it never repeats the settled row's marker.
//
// Other reviewers. With Policy.ExternalReconcile (PUBLISH_EXTERNAL_RECONCILE,
// on unless set to false) every root thread PRism did not post (Greptile,
// Copilot, CodeRabbit, Gemini, code scanning, other bots, humans) is a match
// candidate for a shown finding the ledger does not know, and with
// Policy.NoteReconcile (PUBLISH_NOTE_RECONCILE, on unless set to false) for
// a folded lower-severity note too: same file, the finding's line within
// ten of the root's range (an outdated root keeps its original anchor; a
// root with no line never matches), and containment at or
// above 0.35 (shared tokens over the shorter of the two texts, or of one
// side's headline against the other's body, with a floor of three shared
// words, four for a headline). A contract subject named in the root only
// breaks a tie between roots of equal score: distinct defects share a
// symbol all the time, so a name alone never matches. The roots are re-listed right before the round posts when the GitHub
// implements ReviewCommentLister, so a reviewer who commented seconds
// earlier is seen. A match becomes an external row (Kind annotation,
// ExternalCommentID the root, SourceTag "external:<source>"): never a root
// of PRism's own, dropped from the new and still-open counts but still in
// the merge-confidence score (the score judges the PR, not who found the
// defect first), linked from the summary under "already raised in another
// reviewer's thread" for as long as the point is raised and the thread is
// open, and, for a finding above the inline bar, answered once inside that
// thread with only what the root lacks (the impact sentence, the suggested
// code as a plain fence since a suggestion block would apply to the root's
// lines, how to verify); a folded note is linked, never replied for. A root
// some row already claims gets no second reply. A summary refresh after a
// concession keeps the external links. The delta reply runs inside the
// publish gate like every other write of the round.
//
// Settled threads. Under Policy.NoteReconcile a root whose thread the
// listing shows resolved is a settled point: a finding that restates it is
// recorded as an external row and dropped, with no link and no reply, and
// an external row whose root thread has since been resolved stops being
// linked. Without it a resolved thread is no candidate at all and the
// finding posts as before.
//
// Pull request body. Under Policy.NoteReconcile a folded note the body
// already states is held: one paragraph or list item of Round.PRBody names
// a specific subject of the note and carries its headline or text at the
// settled-point bar. Findings above the inline bar are never held this way;
// no row is written, so an edited body is judged afresh next round.
// The Greptile reconcile runs beside it: a finding that restates a Greptile
// root but misses the external bar keeps its "both" tag, and a Greptile-only
// point whose root a row claims is dropped so nothing is listed twice. Off,
// only Greptile roots are parsed, a match tags the finding "both" and the
// root is still posted.
//
// Conceded fix claims. The reply path marks a conceded fix claim fixed, not
// dismissed, and resolves its thread; a conceded pushback or a withdrawal
// still dismisses. The reply path stamps such a row with the last published
// head, so the next round counts it as fixed once and writes its round
// number into Rounds, which no finding row otherwise carries; a settled
// verdict (intentional, won't fix) dismisses and resolves too.
//
// Threads. The thread listing is read whenever the GitHub implements
// ThreadResolver: it supplies node ids and tells a settled root from an open
// one, and without it the round declines external matching entirely. Thread
// changes and their notes also need Policy.ResolveThreads
// (PUBLISH_THREAD_RESOLUTION, on unless set to false); without it the ledger
// moves and the threads stay as they are. A root's thread
// node id is stored when the root is posted, from one listing taken after
// the review; when that listing lags the write, or the row predates the
// column, the id is found through the listing on the row's first transition.
// A resolve posts nothing unless Policy.ResolveNotes (PUBLISH_RESOLVE_NOTES)
// is on; then the "Not seen" note goes only into a thread the listing shows
// open, and a thread a human already resolved is resolved again silently. A
// posted concession (replies.go) dismisses its row and resolves the thread
// the same way. Thread calls follow the ledger writes and never fail a round; a lost
// call is counted on the Report and the next round, seeing the same state,
// does not retry it. Every thread change runs inside the publish gate, since
// both callers are reached only for authors publish_enabled_authors admits.
//
// Counts. "Since last review" is the number of records that transitioned:
// new (shown findings with no row), still open (rows that end the round
// open, including reopened ones and absent rows no change could have
// fixed), fixed (rows flipped to fixed this round). Hash-set differences
// are not used.
//
// Memory columns. New rows store the raw agent prose (CommentText), the
// contract's finding kind (FindingKind) and the sorted subject names
// (Subjects). A row written before those columns existed is filled the
// first round it is matched or GitHub shows its body.
//
// Output hygiene. After aliasing and before selection, HoldReason keeps a
// finding off GitHub for the round: praise with nothing behind it and a
// design opinion that concedes no impact and calls itself a preference
// (PUBLISH_RENDER_HYGIENE), a claim that admits it rests on third-party
// behaviour the agent did not read ("from memory", "unverified against the
// pinned version", "defaults are certain"; Policy.LibraryClaimHold,
// PUBLISH_LIBRARY_CLAIM_HOLD; the rule is the admission, not a cited-path
// check, and AGENT_CLAIM_EVIDENCE makes the agent write the admission) and
// a regression claim whose cited hunk still shows the code it says was
// dropped (Policy.RegressionEvidence, PUBLISH_REGRESSION_EVIDENCE). A held
// finding is neither posted nor listed; it is reported on Report.Held and
// as a hygiene note, and a row it aliases to stays open with its LastSeenSHA
// advanced, never fixed, never reopened. A finding whose thread is already
// open on GitHub is not held: the summary lists and counts it as still
// open. The renderer, under the same
// PUBLISH_RENDER_HYGIENE switch read at call time, strips a confidence label
// and any internal finding label from the contract sentences, titles a
// comment that opens with a compliment by its first reporting sentence,
// drops the source footer, names the folded notes on the status line and
// refreshes nothing when the summary body did not change.
package publisher
