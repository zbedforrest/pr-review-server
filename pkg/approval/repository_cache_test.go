//go:build darwin || linux

package approval

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func managedRepository(t *testing.T, root string) *GitRepository {
	t.Helper()
	dir, lease, err := newRepositoryDirectory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	r := &GitRepository{directory: dir, lease: lease, managed: true}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestRepositoryCachePreservesActiveAndLegacyDirectories(t *testing.T) {
	root := t.TempDir()
	active := managedRepository(t, root)
	abandoned := managedRepository(t, root)
	abandoned.lease.Close()
	abandoned.lease = nil
	legacy := filepath.Join(root, "objects-legacy")
	if err := os.Mkdir(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(root, "objects-link")); err != nil {
		t.Fatal(err)
	}
	count, err := PruneRepositoryCache(context.Background(), root)
	if err != nil || count != 1 {
		t.Fatalf("prune: %d %v", count, err)
	}
	for _, p := range []string{active.directory, legacy, external, filepath.Join(root, "objects-link")} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(abandoned.directory); !os.IsNotExist(err) {
		t.Fatalf("abandoned directory retained: %v", err)
	}
}

func TestRepositoryCacheProcessHelper(t *testing.T) {
	root := os.Getenv("APPROVAL_CACHE_TEST_ROOT")
	if root == "" {
		return
	}
	dir, lease, err := newRepositoryDirectory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := os.WriteFile(filepath.Join(dir, "private-object"), []byte("private fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	fmt.Println(dir)
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestRepositoryCacheReclaimsKilledOwner(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRepositoryCacheProcessHelper$")
	cmd.Env = append(os.Environ(), "APPROVAL_CACHE_TEST_ROOT="+root)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	dir := strings.TrimSpace(line)
	if filepath.Dir(dir) != root {
		t.Fatalf("unexpected helper path %q", dir)
	}
	if n, err := PruneRepositoryCache(ctx, root); err != nil || n != 0 {
		t.Fatalf("live process: %d %v", n, err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if n, err := PruneRepositoryCache(ctx, root); err != nil || n != 1 {
		t.Fatalf("killed process: %d %v", n, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("private clone retained: %v", err)
	}
}

func TestRepositoryCacheGitChildRetainsLeaseAfterClose(t *testing.T) {
	root := t.TempDir()
	r := managedRepository(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.run(ctx, 4096, "init", "--bare", "--template=", r.directory); err != nil {
		t.Fatal(err)
	}
	cmd := r.command(ctx, "-c", `alias.hold=!f() { printf 'ready\n'; read line; }; f`, "hold")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child: %q %v", line, err)
	}
	for i := 0; i < 2; i++ {
		if err = r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := PruneRepositoryCache(ctx, root); err != nil || n != 0 {
		t.Fatalf("inherited lease: %d %v", n, err)
	}
	if _, err := os.Stat(r.directory); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write([]byte("done\n")); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if n, err := PruneRepositoryCache(ctx, root); err != nil || n != 1 {
		t.Fatalf("released child: %d %v", n, err)
	}
}

func TestRepositoryCacheConcurrentCreationAndPruning(t *testing.T) {
	root := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				dir, lease, err := newRepositoryDirectory(context.Background(), root)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err = PruneRepositoryCache(context.Background(), root); err != nil {
					t.Error(err)
				}
				if _, err = os.Stat(dir); err != nil {
					t.Error(err)
				}
				if err = (&GitRepository{directory: dir, lease: lease, managed: true}).Close(); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if n, err := PruneRepositoryCache(context.Background(), root); err != nil || n != 0 {
		t.Fatalf("remaining: %d %v", n, err)
	}
}

func TestRepositoryCacheReclaimsInterruptedCreation(t *testing.T) {
	root := t.TempDir()
	dir, err := os.MkdirTemp(root, repositoryDirectoryPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := PruneRepositoryCache(context.Background(), root); err != nil || n != 1 {
		t.Fatalf("interrupted creation: %d %v", n, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("retained directory: %v", err)
	}
}

func TestRepositoryCacheLeaseAndCredentialPipeRemainSeparate(t *testing.T) {
	r := managedRepository(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.run(ctx, 4096, "init", "--bare", "--template=", r.directory); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if _, err = writer.WriteString("username=fixture\npassword=synthetic-secret\n\n"); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	cmd := r.command(ctx, "-c", `credential.helper=!f() { if test "$1" = get; then cat <&4; fi; }; f`, "credential", "fill")
	cmd.ExtraFiles = append(cmd.ExtraFiles, reader)
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	body, err := cmd.Output()
	if err != nil || !strings.Contains(string(body), "password=synthetic-secret") {
		t.Fatalf("credential pipe with lease: %v", err)
	}
	if n, err := PruneRepositoryCache(ctx, filepath.Dir(r.directory)); err != nil || n != 0 {
		t.Fatalf("credential helper released owner lease: %d %v", n, err)
	}
}

func TestRepositoryCacheRejectsLeaseSymlinks(t *testing.T) {
	root := t.TempDir()
	dir, err := os.MkdirTemp(root, repositoryDirectoryPrefix)
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "lock")
	if err = os.WriteFile(external, []byte("private fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(external, filepath.Join(dir, repositoryLeaseName)); err != nil {
		t.Fatal(err)
	}
	if n, err := PruneRepositoryCache(context.Background(), root); err == nil || n != 0 {
		t.Fatalf("symlink lease: %d %v", n, err)
	}
	if body, err := os.ReadFile(external); err != nil || string(body) != "private fixture" {
		t.Fatalf("external file changed: %v", err)
	}
}
