package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// minSandboxABI is the oldest Landlock ABI that counts as a sandbox. Before
// ABI 3 Landlock cannot deny truncation (kernel docs: "File truncation could
// not be denied before the third Landlock ABI"), so a tool there could empty
// any file the bot's user can write: the database, stored videos, a writable
// config (Ruling 15).
const minSandboxABI = 3

// sandboxUsable turns a Landlock probe's answer into why there is no
// sandbox, or nil: no Landlock at all, or an ABI before minSandboxABI.
func sandboxUsable(abi int, err error) error {
	if err != nil {
		return err
	}
	if abi < minSandboxABI {
		return fmt.Errorf("the kernel's Landlock ABI %d cannot deny file truncation, which takes ABI %d", abi, minSandboxABI)
	}
	return nil
}

// landlockGaps are what a sandbox from minSandboxABI on may still be unable
// to deny, with the ABI that adds each.
var landlockGaps = []struct {
	abi  int
	what string
}{
	{4, "TCP"},
	{6, "abstract unix sockets and signals"},
	{9, "pathname unix sockets"},
	{10, "UDP"},
}

// sandboxGaps lists what Landlock at abi cannot deny, or "" for nothing the
// sandbox knows of.
func sandboxGaps(abi int) string {
	var gaps []string
	for _, g := range landlockGaps {
		if abi < g.abi {
			gaps = append(gaps, fmt.Sprintf("%s (ABI %d)", g.what, g.abi))
		}
	}
	return strings.Join(gaps, ", ")
}

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
	case landlock && report.ABI < minSandboxABI:
		return report, fmt.Errorf("sandbox check: Landlock ABI %d or later was not applied", minSandboxABI)
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
