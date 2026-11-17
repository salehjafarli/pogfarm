//go:build !unix

package runner

import "os/exec"

// Process groups are a Unix feature. On other systems only the command
// itself is killed; pogfarm and clipper are deployed on Linux.

func setProcessGroup(cmd *exec.Cmd) {}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
