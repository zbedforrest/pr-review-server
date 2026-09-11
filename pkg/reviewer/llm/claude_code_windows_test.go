//go:build windows

package llm

import "os/exec"

func detachFromProcessGroup(*exec.Cmd) {}
