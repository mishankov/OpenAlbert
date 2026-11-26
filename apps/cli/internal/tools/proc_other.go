//go:build !unix

package tools

import (
	"os"
	"os/exec"
)

func newShellCommand(command string) *exec.Cmd {
	if pwsh, err := exec.LookPath("pwsh"); err == nil {
		return exec.Command(pwsh, "-NoProfile", "-Command", command)
	}
	if ps, err := exec.LookPath("powershell"); err == nil {
		return exec.Command(ps, "-NoProfile", "-Command", command)
	}
	comspec := os.Getenv("COMSPEC")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	return exec.Command(comspec, "/C", command)
}

func configureProcessGroup(_ *exec.Cmd) {}

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
