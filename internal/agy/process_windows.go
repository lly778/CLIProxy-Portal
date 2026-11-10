package agy

import "os/exec"

func configureProcess(c *exec.Cmd) {}
func killProcess(c *exec.Cmd) {
	if c.Process != nil {
		_ = c.Process.Kill()
	}
}
