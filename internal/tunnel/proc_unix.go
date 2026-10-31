//go:build !windows

package tunnel

import (
	"os/exec"
	"syscall"
)

var setGroup = func(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

var terminateTree = func(c *exec.Cmd) error {
	return syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
}

var killTree = func(c *exec.Cmd) error {
	return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
}
