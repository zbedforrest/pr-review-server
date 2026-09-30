package approval

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxRepositoryRead = 64 << 10

var fullRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)
var repositoryComponent = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var errUnsearchableBlob = errors.New("binary or oversized repository file")

type GitRepository struct {
	directory string
	revisions map[string]bool
	mu        sync.Mutex
	lease     *os.File
	managed   bool
}
type RepositoryToken func(context.Context, string, string) (string, error)

type boundedGitOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedGitOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("repository output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func (r *GitRepository) command(ctx context.Context, args ...string) *exec.Cmd {
	base := []string{"-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "-c", "core.attributesFile=/dev/null", "-c", "diff.external=", "-c", "protocol.file.allow=never", "-c", "protocol.ext.allow=never", "-c", "protocol.version=2", "-c", "submodule.recurse=false", "-c", "fetch.recurseSubmodules=false", "--git-dir=" + r.directory}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin", "HOME=/nonexistent", "XDG_CONFIG_HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1", "LC_ALL=C"}
	if r.lease != nil {
		cmd.ExtraFiles = []*os.File{r.lease}
	}
	return cmd
}

func (r *GitRepository) run(ctx context.Context, limit int, args ...string) (string, error) {
	cmd := r.command(ctx, args...)
	out := &boundedGitOutput{limit: limit}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("repository read failed")
	}
	return out.String(), nil
}

// NewGitRepository fetches revisions into a fresh bare repository. When
// reference names a local objects directory for the same repository, it is
// borrowed as an alternate so revisions already there skip the network and the
// rest fetch only missing objects.
func NewGitRepository(ctx context.Context, root string, target Target, revisions []string, token RepositoryToken, reference string) (*GitRepository, error) {
	if !repositoryComponent.MatchString(target.Owner) || !repositoryComponent.MatchString(target.Repo) || target.Owner == ".." || target.Repo == ".." || target.Owner == "." || target.Repo == "." {
		return nil, fmt.Errorf("invalid repository")
	}
	if len(revisions) == 0 || len(revisions) > 128 {
		return nil, fmt.Errorf("invalid revision inventory")
	}
	for _, sha := range revisions {
		if !fullRevision.MatchString(sha) {
			return nil, fmt.Errorf("invalid revision")
		}
	}
	directory, lease, err := newRepositoryDirectory(ctx, root)
	if err != nil {
		return nil, err
	}
	r := &GitRepository{directory: directory, revisions: map[string]bool{}, lease: lease, managed: true}
	ok := false
	defer func() {
		if !ok {
			r.Close()
		}
	}()
	if _, err = r.run(ctx, 4096, "init", "--bare", "--template=", directory); err != nil {
		return nil, err
	}
	if reference != "" {
		if info, e := os.Stat(reference); e == nil && info.IsDir() {
			alternates := filepath.Join(directory, "objects", "info")
			if err = os.MkdirAll(alternates, 0o755); err != nil {
				return nil, err
			}
			if err = os.WriteFile(filepath.Join(alternates, "alternates"), []byte(reference+"\n"), 0o644); err != nil {
				return nil, err
			}
		}
	}
	for _, sha := range revisions {
		if r.revisions[sha] {
			continue
		}
		if kind, e := r.run(ctx, 128, "cat-file", "-t", sha); e != nil || strings.TrimSpace(kind) != "commit" {
			if err = r.fetch(ctx, target, sha, token); err != nil {
				return nil, err
			}
		}
		kind, e := r.run(ctx, 128, "cat-file", "-t", sha)
		if e != nil || strings.TrimSpace(kind) != "commit" {
			return nil, fmt.Errorf("revision is not a commit")
		}
		if _, err = r.run(ctx, 4096, "update-ref", "refs/approval/"+sha, sha); err != nil {
			return nil, err
		}
		r.revisions[sha] = true
	}
	ok = true
	return r, nil
}

func (r *GitRepository) fetch(ctx context.Context, target Target, sha string, token RepositoryToken) error {
	args := []string{}
	var reader, writer *os.File
	if token != nil {
		secret, err := token(ctx, target.Owner, target.Repo)
		if err != nil {
			return fmt.Errorf("repository credentials unavailable")
		}
		if strings.ContainsAny(secret, "\r\n") || len(secret) > 16384 {
			return fmt.Errorf("invalid repository credential")
		}
		reader, writer, err = os.Pipe()
		if err != nil {
			return err
		}
		defer reader.Close()
		defer writer.Close()
		credentialFD := 3
		if r.lease != nil {
			credentialFD++
		}
		args = append(args, "-c", fmt.Sprintf(`credential.helper=!f() { if test "$1" = get; then cat <&%d; fi; }; f`, credentialFD))
		go func() {
			defer writer.Close()
			_, _ = io.WriteString(writer, "username=x-access-token\npassword="+secret+"\n\n")
		}()
	}
	args = append(args, "fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "https://github.com/"+target.Owner+"/"+target.Repo+".git", sha)
	cmd := r.command(ctx, args...)
	if reader != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, reader)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("repository fetch failed")
	}
	return nil
}

func (r *GitRepository) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.managed {
		return os.RemoveAll(r.directory)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	guard, err := lockRepositoryCache(ctx, filepath.Dir(r.directory))
	if r.lease != nil {
		_ = r.lease.Close()
		r.lease = nil
	}
	if err != nil {
		return err
	}
	defer guard.Close()
	lease, err := openRepositoryLock(filepath.Join(r.directory, repositoryLeaseName), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lease.Close()
	locked, err := tryRepositoryLock(lease)
	if err != nil || !locked {
		return err
	}
	return os.RemoveAll(r.directory)
}

func (r *GitRepository) MergeBase(ctx context.Context, base, head string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.revisions[base] || !r.revisions[head] {
		return "", fmt.Errorf("unregistered revision")
	}
	out, err := r.run(ctx, 128, "merge-base", base, head)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if !fullRevision.MatchString(sha) {
		return "", fmt.Errorf("invalid merge base")
	}
	r.revisions[sha] = true
	if _, err = r.run(ctx, 4096, "update-ref", "refs/approval/"+sha, sha); err != nil {
		return "", err
	}
	return sha, nil
}

func validRepositoryPath(p string, empty bool) bool {
	return (p == "" && empty) || (p != "" && !strings.ContainsAny(p, "\x00\\\n\r") && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != "." && p != ".." && !strings.HasPrefix(p, "../"))
}

func (r *GitRepository) blob(ctx context.Context, revision, p string) (string, error) {
	if !validRepositoryPath(p, false) {
		return "", fmt.Errorf("invalid path")
	}
	entry, err := r.run(ctx, 4096, "ls-tree", "-z", revision, "--", p)
	if err != nil {
		return "", err
	}
	parts := strings.SplitN(strings.TrimSuffix(entry, "\x00"), "\t", 2)
	if len(parts) != 2 || parts[1] != p {
		return "", fmt.Errorf("file unavailable")
	}
	fields := strings.Fields(parts[0])
	if len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" || !fullRevision.MatchString(fields[2]) {
		return "", fmt.Errorf("non-regular file denied")
	}
	sizeText, err := r.run(ctx, 64, "cat-file", "-s", fields[2])
	if err != nil {
		return "", err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 10, 64)
	if err != nil || size < 0 {
		return "", fmt.Errorf("invalid blob size")
	}
	if size > 2<<20 {
		return "", errUnsearchableBlob
	}
	out, err := r.run(ctx, 2<<20, "cat-file", "blob", fields[2])
	if err != nil {
		return "", err
	}
	if strings.ContainsRune(out, 0) {
		return "", errUnsearchableBlob
	}
	return out, nil
}

func (r *GitRepository) Read(ctx context.Context, name string, req ReadRequest) (ReadResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !fullRevision.MatchString(req.Revision) || !r.revisions[req.Revision] {
		return ReadResult{}, fmt.Errorf("unregistered revision")
	}
	if !validRepositoryPath(req.Path, true) {
		return ReadResult{}, fmt.Errorf("invalid path")
	}
	switch name {
	case "read_file":
		body, err := r.blob(ctx, req.Revision, req.Path)
		if err != nil {
			return ReadResult{}, err
		}
		lines := strings.Split(body, "\n")
		if req.StartLine < 1 || req.EndLine < req.StartLine || req.EndLine-req.StartLine >= 400 || req.EndLine > len(lines) {
			return ReadResult{}, fmt.Errorf("invalid line range")
		}
		text := strings.Join(lines[req.StartLine-1:req.EndLine], "\n")
		if len(text) > maxRepositoryRead {
			return ReadResult{}, fmt.Errorf("file excerpt limit exceeded")
		}
		return ReadResult{Text: text}, nil
	case "read_diff":
		if !fullRevision.MatchString(req.OtherRevision) || !r.revisions[req.OtherRevision] {
			return ReadResult{}, fmt.Errorf("unregistered revision")
		}
		args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", req.OtherRevision, req.Revision, "--"}
		if req.Path != "" {
			args = append(args, req.Path)
		}
		text, err := r.run(ctx, 2<<20, args...)
		if err != nil {
			return ReadResult{}, err
		}
		return repositoryTextPage(text, req.Cursor)
	case "search_code":
		return r.search(ctx, req)
	case "list_files":
		args := []string{"ls-tree", "-rz", req.Revision}
		if req.Path != "" {
			args = append(args, "--", req.Path)
		}
		text, err := r.run(ctx, 2<<20, args...)
		if err != nil {
			return ReadResult{}, err
		}
		entries := strings.Split(strings.TrimSuffix(text, "\x00"), "\x00")
		offset := 0
		if req.Cursor != "" {
			offset, err = strconv.Atoi(req.Cursor)
			if err != nil || offset < 0 || offset > len(entries) {
				return ReadResult{}, fmt.Errorf("invalid cursor")
			}
		}
		var out strings.Builder
		next := offset
		for ; next < len(entries) && next < offset+1000 && out.Len() < maxRepositoryRead-512; next++ {
			parts := strings.SplitN(entries[next], "\t", 2)
			if len(parts) != 2 {
				continue
			}
			fields := strings.Fields(parts[0])
			if len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || !validRepositoryPath(parts[1], false) {
				continue
			}
			out.WriteString(parts[1] + "\n")
		}
		result := ReadResult{Text: out.String()}
		if next < len(entries) {
			result.NextCursor = strconv.Itoa(next)
		}
		return result, nil
	default:
		return ReadResult{}, fmt.Errorf("unsupported repository read")
	}
}

func repositoryTextPage(text, cursor string) (ReadResult, error) {
	lines := strings.SplitAfter(text, "\n")
	start := 0
	var err error
	if cursor != "" {
		start, err = strconv.Atoi(cursor)
		if err != nil || start < 0 || start > len(lines) {
			return ReadResult{}, fmt.Errorf("invalid cursor")
		}
	}
	var out strings.Builder
	next := start
	for ; next < len(lines) && next < start+400; next++ {
		if len(lines[next]) > maxRepositoryRead {
			return ReadResult{}, fmt.Errorf("diff line limit exceeded")
		}
		if out.Len()+len(lines[next]) > maxRepositoryRead {
			break
		}
		out.WriteString(lines[next])
	}
	result := ReadResult{Text: out.String()}
	if next < len(lines) {
		result.NextCursor = strconv.Itoa(next)
	}
	return result, nil
}

func (r *GitRepository) ValidateDiff(ctx context.Context, base, head string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.revisions[base] || !r.revisions[head] {
		return fmt.Errorf("unregistered revision")
	}
	_, err := r.run(ctx, 2<<20, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", base, head, "--")
	if err != nil {
		return fmt.Errorf("required diff unavailable or exceeds limit")
	}
	files, err := r.run(ctx, 1<<20, "diff", "--name-only", "-z", "--no-ext-diff", "--no-textconv", "--no-renames", base, head, "--")
	if err != nil {
		return err
	}
	if strings.Count(files, "\x00") > 500 {
		return fmt.Errorf("changed file limit exceeded")
	}
	return nil
}

func ApprovalCacheRoot(root string) string { return filepath.Join(root, "approval-objects") }

// search runs one fixed-string git grep over the revision (optionally under
// req.Path) and returns up to 200 matches as path:line:text. Only regular
// files of at most 2 MiB are searched and binary files are skipped. The
// cursor is the number of matches already returned.
func (r *GitRepository) search(ctx context.Context, req ReadRequest) (ReadResult, error) {
	if req.Query == "" || len(req.Query) > 256 {
		return ReadResult{}, fmt.Errorf("invalid search")
	}
	skip := 0
	if req.Cursor != "" {
		n, err := strconv.Atoi(req.Cursor)
		if err != nil || n < 0 {
			return ReadResult{}, fmt.Errorf("invalid cursor")
		}
		skip = n
	}
	listArgs := []string{"ls-tree", "-r", "-l", "-z", req.Revision}
	if req.Path != "" {
		listArgs = append(listArgs, "--", req.Path)
	}
	listing, err := r.run(ctx, 8<<20, listArgs...)
	if err != nil {
		return ReadResult{}, err
	}
	searchable := map[string]bool{}
	oversized := 0
	for _, entry := range strings.Split(strings.TrimSuffix(listing, "\x00"), "\x00") {
		parts := strings.SplitN(entry, "\t", 2)
		fields := strings.Fields(parts[0])
		if len(parts) != 2 || len(fields) != 4 || (fields[0] != "100644" && fields[0] != "100755") || !validRepositoryPath(parts[1], false) {
			continue
		}
		if size, err := strconv.ParseInt(fields[3], 10, 64); err != nil || size > 2<<20 {
			oversized++
			continue
		}
		searchable[parts[1]] = true
	}

	grepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"grep", "-n", "-z", "-I", "-F", "--no-color", "-e", req.Query, req.Revision}
	if req.Path != "" {
		args = append(args, "--", req.Path)
	}
	cmd := r.command(grepCtx, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ReadResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return ReadResult{}, fmt.Errorf("repository search failed")
	}
	var out strings.Builder
	seen, matches, more := 0, 0, false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	prefix := req.Revision + ":"
	for scanner.Scan() {
		fields := strings.SplitN(strings.TrimPrefix(scanner.Text(), prefix), "\x00", 3)
		if len(fields) != 3 || !searchable[fields[0]] {
			continue
		}
		if seen++; seen <= skip {
			continue
		}
		match := fields[0] + ":" + fields[1] + ":" + fields[2] + "\n"
		if len(match) > maxRepositoryRead {
			match = match[:maxRepositoryRead/4] + "...\n"
		}
		if matches == 200 || out.Len()+len(match) > maxRepositoryRead-512 {
			more = true
			break
		}
		out.WriteString(match)
		matches++
	}
	cancel()
	waitErr := cmd.Wait()
	var exit *exec.ExitError
	if !more && waitErr != nil && !(errors.As(waitErr, &exit) && exit.ExitCode() == 1) {
		return ReadResult{}, fmt.Errorf("repository search failed")
	}
	result := ReadResult{Text: out.String()}
	if oversized > 0 {
		result.Text += fmt.Sprintf("Skipped %d oversized files; binary files are not searched.\n", oversized)
	}
	if more {
		result.NextCursor = strconv.Itoa(skip + matches)
	}
	return result, nil
}
