package github

import (
	"context"
	"fmt"
)

// CompareFilesCap is the most files the compare endpoint lists; a response
// at the cap may have dropped some.
const CompareFilesCap = 300

// CompareFiles lists the files changed between two commits, a rename
// contributing both names. Truncated is true when the list hit the
// endpoint's cap, so a file absent from it may still have changed.
func (c *Client) CompareFiles(ctx context.Context, owner, repo, base, head string) (files []string, truncated bool, err error) {
	gh, err := c.clientFor(ctx, owner, repo)
	if err != nil {
		return nil, false, err
	}
	cmp, _, err := gh.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
	if err != nil {
		return nil, false, fmt.Errorf("compare %s...%s: %w", base, head, err)
	}
	for _, f := range cmp.Files {
		files = append(files, f.GetFilename())
		if prev := f.GetPreviousFilename(); prev != "" {
			files = append(files, prev)
		}
	}
	return files, len(cmp.Files) >= CompareFilesCap, nil
}
