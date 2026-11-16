//go:build !linux

package main

import "os/exec"

// toolMemoryCapped is false: there is no per-child memory cap here. ulimit
// through /bin/sh, oom_score_adj and Pdeathsig are Linux's, and the bot is
// deployed on Linux; other OSes are for development (Ruling 12 item 2).
const toolMemoryCapped = false

// toolCommand runs the tool directly.
func toolCommand(bin string, args []string) (string, []string) { return bin, args }

// setToolProcAttr does nothing here: Pdeathsig and process groups are set up
// only on Linux (see ffmpeg_linux.go). A timeout kills just the direct child.
// On Windows that can be a launcher shim (Chocolatey's ffmpeg.exe is one), and
// the real ffmpeg it started keeps running, orphaned, until it finishes on
// its own; runTool still returns within toolWaitDelay. A Job Object would
// close this, but it is declined, as Windows is dev-only (Ruling 12 items 5
// and 10).
func setToolProcAttr(*exec.Cmd) {
	// Nothing to set off Linux, as explained above.
}
