//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// toolMemoryCapped is true: on Linux every tool runs under the memory cap,
// which the sandbox helper sets whether or not Landlock is there.
const toolMemoryCapped = true

// toolCommand runs bin with args through the media sandbox helper: the bot's
// own binary, told the spec, which limits and Landlocks itself and then execs
// bin in place (sandbox_linux.go). It replaced Ruling 12's /bin/sh wrapper.
// cpu is the run's CPU-time limit in seconds.
func toolCommand(bin, dir, font string, cpu int, args []string) (string, []string, error) {
	if bin == "" {
		return "", nil, errors.New("no tool")
	}
	spec, err := newSandboxSpec(dir, font, bin, cpu)
	if err != nil {
		return "", nil, err
	}
	return sandboxExe, append([]string{mediaSandboxArg, spec.String(), "--", bin}, args...), nil
}

// setToolProcAttr has the kernel SIGKILL an ffmpeg/ffprobe child when the bot
// dies, so no orphaned decoder keeps running (security decisions, Task 2
// item 10). The signal follows the OS thread that started the child; the Go
// runtime only retires a thread after a goroutine exits while locked to it,
// which this code never does. The helper takes the signal again on the thread
// it execs from (Ruling 14 note 2). It reaches only the direct child, the
// tool itself: a process the tool forked would outlive a dead bot. That gap
// is accepted (Ruling 14 note 7): ffmpeg and ffprobe do not fork, and a
// timeout kills the whole group.
//
// The child also leads its own process group, and a timeout or the output
// bound kills that whole group: a wrapper script on PATH cannot leave a
// child of its own running (Ruling 12 item 5). The helper execs in place, so
// the tool keeps that PID and group.
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
