//go:build !unix

package tools

import "os/exec"

// configureProcessGroup is a no-op outside Unix: the process-group kill
// in command_unix.go relies on syscall.Kill(-pid, ...), which has no
// portable equivalent here. A timed-out command's own children may
// outlive it on these platforms; the direct child is still killed via
// Cmd's default Cancel behavior. Keeping this a no-op (rather than
// leaving the build broken) protects CGO_ENABLED=0 cross-compilation of
// the single static binary this project ships.
func configureProcessGroup(cmd *exec.Cmd) {}
