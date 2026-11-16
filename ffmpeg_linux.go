//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// setToolProcAttr has the kernel SIGKILL an ffmpeg/ffprobe child when the bot
// dies, so no orphaned decoder keeps running (security decisions, Task 2
// item 10). The signal follows the OS thread that started the child; the Go
// runtime only retires a thread after a goroutine exits while locked to it,
// which this code never does.
func setToolProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
