//go:build unix

package ytdlp

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts yt-dlp in its own process group and makes
// cancellation stop the whole group with SIGTERM, so the ffmpeg it starts
// stops too.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

// killProcessGroup kills whatever is left of the group after yt-dlp exits.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
