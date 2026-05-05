//go:build windows

package service

import (
	"context"
	"errors"
)

// DefaultSpawner on Windows is unsupported. The agent reviewer relies on
// process-group semantics (Setpgid + SIGKILL to negative PID) that are
// POSIX-only. Cloud Run is Linux and dev is macOS; nobody ships from a
// Windows host. The stub keeps the package compilable on Windows so a
// developer building unrelated parts of the codebase doesn't hit an
// opaque syscall reference, and surfaces a clear error if anyone actually
// tries to run an agent review there.
type DefaultSpawner struct{}

func (DefaultSpawner) Spawn(_ context.Context, _ string, _ []string, _ string) (SpawnedProcess, error) {
	return nil, errors.New("agent reviewer is not supported on Windows: build for linux/darwin")
}
