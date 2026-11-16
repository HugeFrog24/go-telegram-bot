//go:build !linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// The media sandbox is Linux's (sandbox_linux.go): Landlock, the rlimits,
// oom_score_adj and Pdeathsig have no counterpart here. Other OSes are for
// development, where video.allow_unsandboxed runs the tools directly.

// runMediaSandbox refuses: nothing here re-execs the bot as a helper.
func runMediaSandbox([]string) int {
	fmt.Fprintln(os.Stderr, "media sandbox: Linux only")
	return sandboxExitCode
}

// sandboxAvailable reports that there is no sandbox on this OS.
func sandboxAvailable() (int, error) {
	return 0, errors.New("no Landlock: it is Linux-only")
}

// toolLimitsUsable has nothing to check: there are no per-tool limits here.
func toolLimitsUsable() error { return nil }

// sandboxSelfCheck has no helper to run: its report is empty.
func sandboxSelfCheck(context.Context, string, string) (sandboxReport, error) {
	return sandboxReport{}, nil
}
