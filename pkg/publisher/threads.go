package publisher

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
)

// ReviewThread is one GitHub review thread as the resolver lists it.
type ReviewThread struct {
	NodeID        string
	RootCommentID int64
	Resolved      bool
	Outdated      bool
}

// ThreadResolver is the optional slice of GitHub that lists, resolves and
// unresolves review threads. A GitHub without it moves ledger states and
// leaves the threads as they are.
type ThreadResolver interface {
	ListReviewThreads(ctx context.Context, owner, repo string, number int) ([]ReviewThread, error)
	ResolveThread(ctx context.Context, owner, repo, threadNodeID string) error
	UnresolveThread(ctx context.Context, owner, repo, threadNodeID string) error
}

// ThreadResolutionEnv is the kill switch for thread resolution: "false", "0",
// "off" and "no" leave every thread open; anything else keeps it on.
const ThreadResolutionEnv = "PUBLISH_THREAD_RESOLUTION"

// ThreadResolutionFromEnv reports whether the environment left thread
// resolution on. The poller resolves it once at start-up.
func ThreadResolutionFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ThreadResolutionEnv))) {
	case "false", "0", "off", "no":
		return false
	}
	return true
}

// threadIndex is one round's view of the PR's review threads keyed by root
// comment, listed on first use and again after the round posts new roots.
type threadIndex struct {
	gh     ThreadResolver
	owner  string
	repo   string
	number int
	byRoot map[int64]ReviewThread
	loaded bool
	err    error
}

func newThreadIndex(gh GitHub, pol Policy, owner, repo string, number int) *threadIndex {
	if !pol.ResolveThreads {
		return nil
	}
	resolver, ok := gh.(ThreadResolver)
	if !ok {
		return nil
	}
	return &threadIndex{gh: resolver, owner: owner, repo: repo, number: number}
}

func (ti *threadIndex) reload(ctx context.Context) {
	ti.loaded = true
	ti.byRoot = map[int64]ReviewThread{}
	threads, err := ti.gh.ListReviewThreads(ctx, ti.owner, ti.repo, ti.number)
	if err != nil {
		ti.err = err
		log.Printf("[PUBLISH] %s/%s#%d: list review threads: %v", ti.owner, ti.repo, ti.number, err)
		return
	}
	ti.err = nil
	for _, t := range threads {
		if t.RootCommentID != 0 {
			ti.byRoot[t.RootCommentID] = t
		}
	}
}

// lookup finds the thread a root comment opened; false when the listing
// failed or the comment opened no thread GitHub lists.
func (ti *threadIndex) lookup(ctx context.Context, rootCommentID int64) (ReviewThread, bool) {
	if ti == nil || rootCommentID == 0 {
		return ReviewThread{}, false
	}
	if !ti.loaded {
		ti.reload(ctx)
	}
	t, ok := ti.byRoot[rootCommentID]
	return t, ok
}

// thread is the thread a root comment opened, looked up in the listing so
// its resolved flag is current; a stored node id stands in when the listing
// cannot find it. Looking it up before the row is written keeps the id on a
// row that predates the ThreadNodeID column.
func (ti *threadIndex) thread(ctx context.Context, stored string, rootCommentID int64) (ReviewThread, bool) {
	if t, ok := ti.lookup(ctx, rootCommentID); ok {
		return t, true
	}
	if stored == "" {
		return ReviewThread{}, false
	}
	return ReviewThread{NodeID: stored, RootCommentID: rootCommentID}, true
}

// nodeIDOf is the node id of the thread a root comment opened, or empty when
// the listing does not show it yet.
func (ti *threadIndex) nodeIDOf(ctx context.Context, rootCommentID int64) string {
	t, _ := ti.lookup(ctx, rootCommentID)
	return t.NodeID
}

// threadAction is a resolve or unresolve the round owes once its ledger
// writes are done; nodeID empty means the thread could not be found.
type threadAction struct {
	nodeID  string
	resolve bool
}

func (ti *threadIndex) apply(ctx context.Context, actions []threadAction, rep *Report) {
	if ti == nil {
		return
	}
	for _, a := range actions {
		if a.nodeID == "" {
			rep.ThreadResolveFailures++
			continue
		}
		var err error
		if a.resolve {
			err = ti.gh.ResolveThread(ctx, ti.owner, ti.repo, a.nodeID)
		} else {
			err = ti.gh.UnresolveThread(ctx, ti.owner, ti.repo, a.nodeID)
		}
		if err != nil {
			// The ledger already holds the state; the next round sees the same
			// row and does not resolve again, so a lost call is logged only.
			rep.ThreadResolveFailures++
			log.Printf("[PUBLISH] %s/%s#%d: thread %s %s lost: %v", ti.owner, ti.repo, ti.number, a.nodeID, verb(a.resolve), err)
			continue
		}
		if a.resolve {
			rep.ThreadsResolved++
		} else {
			rep.ThreadsUnresolved++
		}
	}
}

func verb(resolve bool) string {
	if resolve {
		return "resolve"
	}
	return "unresolve"
}

// notSeenNote is the one line a resolved thread gets when its finding is
// judged fixed.
func notSeenNote(headSHA string) string {
	return fmt.Sprintf("Not seen at %s.", shortSHA(headSHA))
}
