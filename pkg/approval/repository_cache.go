package approval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const repositoryLeaseName = ".approval-lease"
const repositoryDirectoryPrefix = "objects-v2-"

func lockRepositoryCache(ctx context.Context, root string) (*os.File, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	lock, err := openRepositoryLock(filepath.Join(root, ".approval-cache-lock"), true)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = lock.Close()
			return nil, err
		}
		locked, err := tryRepositoryLock(lock)
		if err != nil {
			_ = lock.Close()
			return nil, err
		}
		if locked {
			return lock, nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = lock.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func newRepositoryDirectory(ctx context.Context, root string) (string, *os.File, error) {
	guard, err := lockRepositoryCache(ctx, root)
	if err != nil {
		return "", nil, err
	}
	defer guard.Close()
	directory, err := os.MkdirTemp(root, repositoryDirectoryPrefix)
	if err != nil {
		return "", nil, err
	}
	lease, err := openRepositoryLock(filepath.Join(directory, repositoryLeaseName), true)
	if err != nil {
		_ = os.RemoveAll(directory)
		return "", nil, err
	}
	locked, err := tryRepositoryLock(lease)
	if err != nil || !locked {
		_ = lease.Close()
		_ = os.RemoveAll(directory)
		if err == nil {
			err = errors.New("new repository lease is already held")
		}
		return "", nil, err
	}
	return directory, lease, nil
}

func PruneRepositoryCache(ctx context.Context, root string) (int, error) {
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	guard, err := lockRepositoryCache(ctx, root)
	if err != nil {
		return 0, err
	}
	defer guard.Close()
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), repositoryDirectoryPrefix) {
			continue
		}
		directory := filepath.Join(root, entry.Name())
		lease, err := openRepositoryLock(filepath.Join(directory, repositoryLeaseName), false)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.RemoveAll(directory); err != nil {
				return removed, err
			}
			removed++
			continue
		}
		if err != nil {
			return removed, err
		}
		locked, err := tryRepositoryLock(lease)
		if err == nil && locked {
			err = os.RemoveAll(directory)
			if err == nil {
				removed++
			}
		}
		_ = lease.Close()
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}
