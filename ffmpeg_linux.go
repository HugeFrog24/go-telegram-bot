//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// toolMemoryCapped is true: on Linux every tool runs under the memory cap,
// which the sandbox helper sets whether or not Landlock is there.
const toolMemoryCapped = true

// toolTimeLimitFlag is false: on Linux the helper's RLIMIT_CPU caps every
// run's CPU time, soft and hard, so ffmpeg gets no -timelimit, whose own
// setrlimit(RLIMIT_CPU, {lim, lim+1}) asks for more than that hard limit
// and fails on every run (Ruling 19 item 7).
const toolTimeLimitFlag = false

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

// endToolGroup waits for the tool to exit and then kills its whole process
// group, on every run and not only on a timeout, so nothing the tool forked
// outlives it (Ruling 16 item 3). The wait leaves the tool a zombie (WNOWAIT):
// until cmd.Wait reaps it, its PID, which is also the group's ID, cannot be
// reused, so the kill reaches only that group.
func endToolGroup(cmd *exec.Cmd) {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	_ = killProcessGroup(cmd.Process.Pid)
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
