package streamcontrol

import (
	"os/exec"
	"strconv"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }

func terminateProcess(cmd *exec.Cmd) error {
	kill := exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	configureProcess(kill)
	if err := kill.Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
