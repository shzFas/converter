//go:build unix

package source

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes the child a group leader and kills the whole group
// on cancellation, so grandchildren (yt-dlp's ffmpeg) do not outlive the job.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
