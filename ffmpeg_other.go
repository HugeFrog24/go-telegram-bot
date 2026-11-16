//go:build !linux

package main

import (
	"fmt"
	"os/exec"
)

// toolMemoryCapped is false: there is no per-child memory cap here. The
// rlimits, oom_score_adj, Pdeathsig and Landlock are Linux's, and the bot is
// deployed on Linux; other OSes are for development (Ruling 12 item 2,
// Ruling 13).
const toolMemoryCapped = false

// toolCommand runs the tool directly, and only once a bot's gate allowed
// tools without a sandbox (video.allow_unsandboxed): there is none here.
func toolCommand(bin, _, _ string, args []string) (string, []string, error) {
	if !unsandboxedTools.Load() {
		_, err := sandboxAvailable()
		return "", nil, fmt.Errorf("%w: %w", errNoSandbox, err)
	}
	return bin, args, nil
}

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
