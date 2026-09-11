package server

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

// agentLinkPath turns a published finding into a Claude Code deep link. GitHub
// only renders http(s) links, so the inline comment points here and this
// endpoint 302s to the claude-cli:// scheme. It is stateless on purpose: the
// prompt tells the agent to read the comment off the PR, which is always
// current, rather than embedding review text that could go stale.
const agentLinkPath = "/go/agent"

// claude-cli deep links accept at most 5000 characters of prompt.
const agentLinkPromptMax = 5000

var (
	agentLinkSafeName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	// A finding id is <path>:<line bucket>:<12 hex>; the path may not contain
	// whitespace. Only that grammar and plain repository paths reach the prompt.
	agentLinkFindingID = regexp.MustCompile(`^[^\s:]{1,300}:\d{1,7}:[0-9a-f]{12}$`)
	agentLinkRepoPath  = regexp.MustCompile(`^[A-Za-z0-9._/@+-]{1,300}$`)
)

func (s *Server) handleAgentLink(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	owner, repo, fid, path := q.Get("o"), q.Get("r"), q.Get("f"), q.Get("p")
	number, errN := strconv.Atoi(q.Get("n"))
	line, errL := strconv.Atoi(q.Get("l"))
	if q.Get("l") == "" {
		line, errL = 0, nil
	}
	switch {
	case !agentLinkSafeName.MatchString(owner), !agentLinkSafeName.MatchString(repo),
		errN != nil, number <= 0, errL != nil, line < 0,
		!agentLinkFindingID.MatchString(fid), !agentLinkRepoPath.MatchString(path):
		http.Error(w, "invalid agent link", http.StatusBadRequest)
		return
	}

	where := path
	if line > 0 {
		where = fmt.Sprintf("%s:%d", path, line)
	}
	prompt := fmt.Sprintf(
		"PRism left a code review comment on pull request %s/%s#%d at %s (finding id %s).\n"+
			"Read that comment from the PR itself: the review comment whose body contains the marker "+
			"\"<!-- prism:finding:%s -->\" (use `gh api repos/%s/%s/pulls/%d/comments --paginate`). "+
			"Decide whether the finding is valid. If it is, fix it directly in the working tree and say what you changed; "+
			"if it is not, explain why rather than changing code to satisfy it.",
		owner, repo, number, where, fid, fid, owner, repo, number)
	if len(prompt) > agentLinkPromptMax {
		prompt = prompt[:agentLinkPromptMax]
	}

	target := "claude-cli://open?" + url.Values{
		"q":    {prompt},
		"repo": {owner + "/" + repo},
	}.Encode()
	http.Redirect(w, r, target, http.StatusFound)
}
