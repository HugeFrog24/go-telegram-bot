//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// toolMemoryCapped is true: on Linux every tool runs under the memory cap.
const toolMemoryCapped = true

// memoryCapScript caps the child's address space at toolMemoryLimit and makes
// it the OOM killer's first choice, then execs the tool in the same process,
// so its PID, its Pdeathsig and its process group stay as runTool set them
// (Ruling 12 item 2). The tool path comes in as "$0" and its arguments as
// "$@", so the shell never parses anything generated.
var memoryCapScript = fmt.Sprintf(`ulimit -v %d && echo 1000 > /proc/self/oom_score_adj && exec "$0" "$@"`, toolMemoryLimit>>10)

// toolCommand runs bin with args through toolShell and memoryCapScript.
func toolCommand(bin string, args []string) (string, []string) {
	return toolShell, append([]string{"-c", memoryCapScript, bin}, args...)
}

// setToolProcAttr has the kernel SIGKILL an ffmpeg/ffprobe child when the bot
// dies, so no orphaned decoder keeps running (security decisions, Task 2
// item 10). The signal follows the OS thread that started the child; the Go
// runtime only retires a thread after a goroutine exits while locked to it,
// which this code never does.
//
// The child also leads its own process group, and a timeout or the output
// bound kills that whole group: a wrapper script on PATH cannot leave a
// child of its own running (Ruling 12 item 5).
func setToolProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process.Pid) }
}

// killProcessGroup SIGKILLs the process group that pid leads. A group that is
// already gone is os.ErrProcessDone, so Wait reports the tool's own status.
func killProcessGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
