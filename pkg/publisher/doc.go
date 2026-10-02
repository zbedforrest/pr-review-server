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
// this order: its fingerprint equals a row's; same file, line within ten of
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
//	open                         no       no         unknown       fixed as above when GitHub lists the thread outdated and the row was last seen at the head it was posted for; else still open, untouched
//	open                         no       no         no            still open, untouched
//	fixed / resolved (legacy)    yes      any        any           reopened: open, LastSeenSHA = head, one reply "Back at <sha7>" in its thread, thread unresolved, never a new root
//	fixed / resolved             no       any        any           untouched
//	dismissed/contested/external yes      any        any           finding dropped: not posted, not counted, row untouched (exception below)
//	dismissed/contested/external no       any        any           untouched
//	no row                       new      n/a        n/a           Select decides inline or summary; a shown finding gets an open row
//
// Already published. Select never offers a root for a finding whose row
// has a comment id, whatever the row's state; a row without a comment (a
// summary-only annotation) may still be promoted inline when its line
// becomes commentable.
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
// Threads. Thread changes need Policy.ResolveThreads (PUBLISH_THREAD_RESOLUTION,
// on unless set to false) and a GitHub that implements ThreadResolver; without
// either the ledger moves and the threads stay as they are. A root's thread
// node id is stored when the root is posted; a row from before that column
// finds its thread through the listing when it is first resolved. A posted
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
