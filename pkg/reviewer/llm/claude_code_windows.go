//go:build windows

package llm

import "os/exec"

// Windows has no POSIX process groups; the default cmd.Cancel kills only the
// direct child and cmd.WaitDelay bounds the wait for anything it spawned.
func configureProcessGroup(*exec.Cmd) {}
