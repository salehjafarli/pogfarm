//go:build unix

package runner

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes the command the leader of a new process group, so
// that it and everything it starts can be killed together.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup sends SIGKILL to every process in the command's group. It is
// harmless when the group is already empty.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// A negative pid addresses the process group with that ID. The group ID
	// equals the leader's pid because of Setpgid.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
