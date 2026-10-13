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
// Inputs per prior row: its state; whether a current finding matched it
// (present); whether HeadSHA equals the row's LastSeenSHA; the change set
// between LastSeenSHA and HeadSHA (Round.Changes, unknown when nil or when
// the lookup fails); and whether the lines the finding cites are inside
// that change set (anchor changed: a changed line within three of the cited
// line when the set carries lines, else the file having changed).
//
//	state                        present  same head  file changed  result
//	open                         yes      any        any           still open; LastSeenSHA = head; severity clamp (below)
//	open                         no       yes        n/a           still open, untouched
//	open                         no       no         yes           fixed: one reply "Not seen at <sha7>" in its thread, thread resolved
//	open                         no       no         unknown       still open, untouched
//	open                         no       no         no            still open, untouched
//	fixed / resolved (legacy)    yes      any        any           reopened: open, LastSeenSHA = head, one reply "Back at <sha7>" in its thread, thread unresolved, never a new root
//	fixed (conceded, see below)  no       no         any           counted as fixed once; Rounds = this round
//	fixed / resolved             no       any        any           untouched
//	dismissed/contested/external yes      any        any           finding dropped: not posted, not counted, row untouched (exception below); an external row keeps its summary link
//	dismissed/contested/external no       any        any           untouched
//	no row, another reviewer's   new      n/a        n/a           external row: no root, summary links to that root, one delta reply there (see Other reviewers)
//	root on the point
//	no row                       new      n/a        n/a           Select decides inline or summary; a shown finding gets an open row
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
// candidate for a shown finding the ledger does not know, unless the thread
// listing shows its thread resolved: same file, the finding's line within
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
// reviewer's thread" for as long as the point is raised, and answered once
// inside that thread with only what the root lacks (the impact sentence,
// the suggested code as a plain fence since a suggestion block would apply
// to the root's lines, how to verify). A root some row already claims gets
// no second reply. A summary refresh after a concession keeps the external
// links. The delta reply runs inside the publish gate like every other
// write of the round.
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
// The "Not seen" note is posted only into a thread the listing shows open;
// a thread a human already resolved is resolved again silently. A posted
// concession (replies.go) dismisses its row and resolves the thread the same
// way. Thread calls follow the ledger writes and never fail a round; a lost
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
package publisher
