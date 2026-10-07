//go:build !windows

package streamcontrol

import "os/exec"

func configureProcess(_ *exec.Cmd) {}

func terminateProcess(cmd *exec.Cmd) error { return cmd.Process.Kill() }
