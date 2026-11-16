package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// The media sandbox's cross-platform part: the helper's marker and exit
// status, the switch that lets tools run without the sandbox, the helper's
// report and the gate's self-test. The sandbox itself is Linux's
// (sandbox_linux.go, landlock_linux.go); other OSes have none
// (sandbox_other.go).

// mediaSandboxArg, as the first argument, makes the bot's binary the media
// sandbox helper instead of the bot (main, sandbox_linux.go).
const mediaSandboxArg = "__media_sandbox"

// sandboxExitCode is the helper's exit status when it refuses or fails to run
// the tool.
const sandboxExitCode = 125

// unsandboxedTools is set once a bot's gate lets tools run without the
// Landlock sandbox: video.allow_unsandboxed where the kernel or OS has none.
// Until then no tool runs without it (errNoSandbox).
var unsandboxedTools atomic.Bool

// errNoSandbox means a tool was not run: there is no sandbox, and no bot
// allowed running without one.
var errNoSandbox = errors.New("no sandbox for ffmpeg")

// sandboxReport is what the helper's check run reports to the gate.
type sandboxReport struct {
	ABI         int  `json:"landlock_abi"`  // the Landlock ABI applied; 0 for none
	OOMScoreAdj bool `json:"oom_score_adj"` // whether oom_score_adj was set to 1000
}

// selfTestGIF is the gate's self-test input: a 16×16, four-frame GIF made
// once with `ffmpeg -f lavfi -i testsrc2=size=16x16:rate=4:duration=1 -f gif`.
//
//go:embed selftest.gif
var selfTestGIF []byte

const selfTestTimeout = 30 * time.Second

// toolSelfTest is the gate's self-test (Ruling 14 note 5). It writes the
// embedded GIF to a fresh input directory and runs, through the production
// runner: the helper's check run (where landlock is true it must apply
// Landlock and pass the negative checks, a read outside the input directory
// and creating a file both failing), then probeMedia and a one-frame
// renderSheet with font. That catches a blocked Landlock, a bad rlimit, a
// broken ffmpeg and a missing font at once.
func toolSelfTest(ctx context.Context, font string, landlock bool) (sandboxReport, error) {
	ctx, cancel := context.WithTimeout(ctx, selfTestTimeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "video-selftest-")
	if err != nil {
		return sandboxReport{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "media")
	if err := os.WriteFile(path, selfTestGIF, 0o600); err != nil {
		return sandboxReport{}, err
	}
	report, err := sandboxSelfCheck(ctx, dir, path)
	switch {
	case err != nil:
		return report, fmt.Errorf("sandbox check: %w", err)
	case landlock && report.ABI == 0:
		return report, errors.New("sandbox check: Landlock was not applied")
	}
	probe, err := probeMedia(ctx, path, "animation")
	if err != nil {
		return report, fmt.Errorf("probe: %w", err)
	}
	img, _, err := renderSheet(ctx, path, probe, sheetSpec{Frames: 1, MaxEdge: minSheetEdge, FontFile: font})
	if err != nil {
		return report, fmt.Errorf("render: %w", err)
	}
	if !bytes.HasPrefix(img, []byte{0xFF, 0xD8, 0xFF}) {
		return report, errors.New("render: not a JPEG")
	}
	return report, nil
}
