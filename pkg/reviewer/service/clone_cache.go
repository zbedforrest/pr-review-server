package service

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Repo caches live under <cloneRoot>/.cache/<owner>__<repo> and are never
// removed by a review. With a byte cap set, the least recently used caches
// that no review is using are evicted until the total fits. A cache is in use
// from before its fetch until its worktree is removed; eviction checks that
// under the per-repo mutex, so a live worktree never loses its object store.
// A review that races an eviction simply re-clones (ensureAgentCache).
var cloneCache = struct {
	sync.Mutex
	maxBytes  int64
	inUse     map[string]int
	lastUsed  map[string]time.Time
	lastSweep time.Time
}{inUse: map[string]int{}, lastUsed: map[string]time.Time{}}

// cloneCacheSweepEvery rate-limits sweeps: sizing a monorepo cache walks it.
const cloneCacheSweepEvery = time.Minute

// SetCloneCacheMaxBytes sets the total cache cap; zero or less is unlimited.
func SetCloneCacheMaxBytes(n int64) {
	cloneCache.Lock()
	defer cloneCache.Unlock()
	cloneCache.maxBytes = n
}

// acquireCacheUse marks owner/repo's cache in use and returns the release.
func acquireCacheUse(key string) func() {
	cloneCache.Lock()
	cloneCache.inUse[key]++
	cloneCache.lastUsed[key] = time.Now()
	cloneCache.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			cloneCache.Lock()
			if cloneCache.inUse[key]--; cloneCache.inUse[key] <= 0 {
				delete(cloneCache.inUse, key)
			}
			cloneCache.Unlock()
		})
	}
}

// maybeEvictCloneCaches sweeps in the background when a cap is set and the
// last sweep is older than cloneCacheSweepEvery.
func maybeEvictCloneCaches(cloneRoot string) {
	cloneCache.Lock()
	due := cloneCache.maxBytes > 0 && time.Since(cloneCache.lastSweep) >= cloneCacheSweepEvery
	if due {
		cloneCache.lastSweep = time.Now()
	}
	max := cloneCache.maxBytes
	cloneCache.Unlock()
	if due {
		go evictCloneCaches(cloneRoot, max)
	}
}

type cacheEntry struct {
	key, dir string
	bytes    int64
	used     time.Time
}

// evictCloneCaches removes least recently used idle caches until the caches
// under cloneRoot total at most maxBytes. It returns the keys it removed.
func evictCloneCaches(cloneRoot string, maxBytes int64) []string {
	root := filepath.Join(cloneRoot, ".cache")
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var entries []cacheEntry
	var total int64
	for _, d := range dirs {
		owner, repo, ok := strings.Cut(d.Name(), "__")
		if !d.IsDir() || !ok {
			continue
		}
		dir := filepath.Join(root, d.Name())
		e := cacheEntry{key: owner + "/" + repo, dir: dir, bytes: dirBytes(dir)}
		cloneCache.Lock()
		e.used = cloneCache.lastUsed[e.key]
		cloneCache.Unlock()
		if e.used.IsZero() {
			if info, err := d.Info(); err == nil {
				e.used = info.ModTime()
			}
		}
		entries = append(entries, e)
		total += e.bytes
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].used.Before(entries[j].used) })
	var evicted []string
	for _, e := range entries {
		if total <= maxBytes {
			break
		}
		mu := cacheLock(e.key)
		mu.Lock()
		cloneCache.Lock()
		busy := cloneCache.inUse[e.key] > 0
		cloneCache.Unlock()
		if !busy {
			if err := os.RemoveAll(e.dir); err != nil {
				log.Printf("[AGENT] clone cache: evicting %s failed: %v", e.key, err)
			} else {
				total -= e.bytes
				evicted = append(evicted, e.key)
				log.Printf("[AGENT] clone cache: evicted %s (%d MB), total now %d MB of %d MB", e.key, e.bytes>>20, total>>20, maxBytes>>20)
			}
		}
		mu.Unlock()
	}
	return evicted
}

func dirBytes(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
