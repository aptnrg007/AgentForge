//go:build unix

package tools

import (
	"os/exec"
	"syscall"
	"time"
)

// configureProcessGroup puts cmd in its own process group and arranges
// for context cancellation (tool_policy timeout or run cancellation) to
// kill that whole group, not just the direct child — so a command that
// spawns its own children doesn't orphan them when the deadline fires.
// WaitDelay bounds how long Cmd.Wait gives the group after Cancel before
// forcibly closing its I/O pipes.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
}
