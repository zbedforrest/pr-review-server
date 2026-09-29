package service

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func seedCache(t *testing.T, root, name string, size int, used time.Time) {
	t.Helper()
	dir := filepath.Join(root, ".cache", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack"), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, used, used); err != nil {
		t.Fatal(err)
	}
}

func TestEvictCloneCachesRemovesLeastRecentlyUsedIdleCachesOnly(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	seedCache(t, root, "acme__busy", 1000, now.Add(-3*time.Hour))
	seedCache(t, root, "acme__stale", 1000, now.Add(-2*time.Hour))
	seedCache(t, root, "acme__warm", 1000, now.Add(-time.Hour))
	seedCache(t, root, "acme__fresh", 1000, now)
	release := acquireCacheUse("acme/busy")
	defer release()
	cloneCache.Lock()
	cloneCache.lastUsed["acme/busy"] = now.Add(-3 * time.Hour)
	cloneCache.Unlock()

	evicted := evictCloneCaches(root, 2000)

	if !reflect.DeepEqual(evicted, []string{"acme/stale", "acme/warm"}) {
		t.Fatalf("evicted %v, want the two oldest idle caches and never the busy one", evicted)
	}
	for name, want := range map[string]bool{"acme__busy": true, "acme__stale": false, "acme__warm": false, "acme__fresh": true} {
		if _, err := os.Stat(filepath.Join(root, ".cache", name)); (err == nil) != want {
			t.Errorf("%s present=%v, want %v", name, err == nil, want)
		}
	}
}

func TestEvictCloneCachesKeepsEverythingUnderTheCap(t *testing.T) {
	root := t.TempDir()
	seedCache(t, root, "acme__one", 1000, time.Now())
	if evicted := evictCloneCaches(root, 1<<20); len(evicted) != 0 {
		t.Fatalf("evicted %v under the cap", evicted)
	}
}

func TestMaybeEvictCloneCachesIsOffWithoutACap(t *testing.T) {
	SetCloneCacheMaxBytes(0)
	root := t.TempDir()
	seedCache(t, root, "acme__one", 1000, time.Now().Add(-time.Hour))
	maybeEvictCloneCaches(root)
	time.Sleep(20 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(root, ".cache", "acme__one")); err != nil {
		t.Fatalf("a cache was removed with no cap set: %v", err)
	}
}
