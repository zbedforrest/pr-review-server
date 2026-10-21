package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ErrNotFound means the path does not exist at the requested revision.
var ErrNotFound = errors.New("path not found at revision")

// Hit is one git grep match.
type Hit struct {
	Path string
	Line int
}

// Git reads repository objects at fixed revisions. Nothing reads the working
// tree, so a packet can be built after the review's checkout is gone.
type Git interface {
	Show(ctx context.Context, rev, path string) (string, error)
	Diff(ctx context.Context, base, head, path string) (string, error)
	Grep(ctx context.Context, rev, word string, max int) ([]Hit, error)
}

// RepoGit runs git against the clone at Dir.
type RepoGit struct{ Dir string }

func (g RepoGit) run(ctx context.Context, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.quotePath=false"}, args...)...)
	cmd.Dir = g.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	}
	if err != nil {
		return "", code, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), 0, nil
}

func (g RepoGit) Show(ctx context.Context, rev, path string) (string, error) {
	if _, code, err := g.run(ctx, "cat-file", "-e", rev+":"+path); err != nil {
		if ctx.Err() != nil || code != 128 {
			return "", err
		}
		return "", ErrNotFound
	}
	out, _, err := g.run(ctx, "show", rev+":"+path)
	return out, err
}

func (g RepoGit) Diff(ctx context.Context, base, head, path string) (string, error) {
	out, _, err := g.run(ctx, "diff", "--no-renames", "--no-color", "--no-ext-diff", base, head, "--", path)
	return out, err
}

// Grep finds whole-word fixed-string matches; no match is not an error.
func (g RepoGit) Grep(ctx context.Context, rev, word string, max int) ([]Hit, error) {
	out, code, err := g.run(ctx, "grep", "-n", "-w", "-F", "-I", "-e", word, rev, "--")
	if code == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var hits []Hit
	prefix := rev + ":"
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		path, rest, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		num, _, ok := strings.Cut(rest, ":")
		n, err := strconv.Atoi(num)
		if !ok || err != nil {
			continue
		}
		hits = append(hits, Hit{Path: path, Line: n})
		if len(hits) == max {
			break
		}
	}
	return hits, nil
}
