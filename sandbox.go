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

// unsandboxedGrants counts the bots whose gate lets tools run without the
// Landlock sandbox: video.allow_unsandboxed where the kernel or OS has none.
// While it is 0 no tool runs without the sandbox (errNoSandbox). A gate whose
// self-test then fails takes its grant back (review Minor 6); bots start
// concurrently, so this is a count rather than a flag one bot could clear
// under another.
var unsandboxedGrants atomic.Int32

// unsandboxedAllowed reports whether some bot's gate lets tools run without
// the sandbox.
func unsandboxedAllowed() bool { return unsandboxedGrants.Load() > 0 }

// errNoSandbox means a tool was not run: there is no sandbox, and no bot
// allowed running without one.
var errNoSandbox = errors.New("no sandbox for ffmpeg")

// minSandboxABI is the oldest Landlock ABI that counts as a sandbox. Before
// ABI 3 Landlock cannot deny truncation (kernel docs: "File truncation could
// not be denied before the third Landlock ABI"), so a tool there could empty
// any file the bot's user can write: the database, stored videos, a writable
// config (Ruling 15).
const minSandboxABI = 3

// sandboxUsable turns the sandbox probe's answer into why there is no
// sandbox, or nil: no Landlock or no seccomp filter (the probe's error), or a
// Landlock ABI before minSandboxABI.
func sandboxUsable(abi int, err error) error {
	if err != nil {
		return err
	}
	if abi < minSandboxABI {
		return fmt.Errorf("the kernel's Landlock ABI %d cannot deny file truncation, which takes ABI %d", abi, minSandboxABI)
	}
	return nil
}

// sandboxGapList is what the sandbox's two layers may leave open: what each
// Landlock ABI cannot deny, and whether the seccomp allowlist denies it
// instead (Ruling 18 items 1 and 4). Neither layer hides a file's metadata
// from a read by path (stat, access, readlink, statfs), which the tools use.
var sandboxGapList = []struct {
	abi     int  // the Landlock ABI that denies it; 0 for none
	seccomp bool // whether the seccomp filter denies it
	what    string
}{
	{1, false, "files (reading, writing and running any the user may)"},
	{3, false, "file truncation"},
	{4, true, "TCP"},
	{6, true, "abstract unix sockets and signals"},
	{9, true, "pathname unix sockets"},
	{10, true, "UDP"},
	{0, true, "other processes' resource limits, priority and CPU affinity"},
	{0, true, "file metadata changes (mode, owner, times, extended attributes)"},
	{0, true, "SysV IPC, inotify and fanotify"},
	{0, true, "clearing the parent-death signal"},
	{0, true, "every other system call (new processes, namespaces, io_uring, bpf)"},
	{0, false, "file metadata (stat: whether a path exists, sizes, times)"},
}

// sandboxGaps lists what neither Landlock at abi nor, when seccomp is true,
// the seccomp filter covers, each with the ABI that would; "" for nothing.
func sandboxGaps(abi int, seccomp bool) string {
	var gaps []string
	for _, g := range sandboxGapList {
		switch {
		case seccomp && g.seccomp, g.abi > 0 && abi >= g.abi:
		case g.abi > 0:
			gaps = append(gaps, fmt.Sprintf("%s (ABI %d)", g.what, g.abi))
		default:
			gaps = append(gaps, g.what)
		}
	}
	return strings.Join(gaps, ", ")
}

// sandboxReport is what the helper's check run reports to the gate.
type sandboxReport struct {
	ABI            int  `json:"landlock_abi"`    // the Landlock ABI applied; 0 for none
	OOMScoreAdj    bool `json:"oom_score_adj"`   // whether oom_score_adj was set to 1000
	Seccomp        bool `json:"seccomp"`         // whether the seccomp filter was installed
	TaskCap        bool `json:"task_cap"`        // whether RLIMIT_NPROC binds the user (root is exempt)
	NoCapabilities bool `json:"no_capabilities"` // whether every capability set was empty after the drop
}

// errTaskCapUnbound means RLIMIT_NPROC binds no user here, as for root in the
// initial user namespace even without capabilities: a tool could start
// threads until the user's or the host's run out. That is no whole sandbox
// (Ruling 19 item 4).
var errTaskCapUnbound = errors.New("the task cap does not bind the bot's user (root, say): run the bot as a non-root user")

// selfTestGIF is the gate's self-test input: a 16×16, four-frame GIF made
// once with `ffmpeg -f lavfi -i testsrc2=size=16x16:rate=4:duration=1 -f gif`.
//
//go:embed selftest.gif
var selfTestGIF []byte

const selfTestTimeout = 30 * time.Second

// toolSelfTest is the gate's self-test (Ruling 14 note 5). It writes the
// embedded GIF to a fresh input directory and runs, through the production
// runner: the helper's check run (where landlock is true it must apply
// Landlock, the seccomp filter and a task cap that binds the user, failing
// with errTaskCapUnbound otherwise, and each layer there must pass its
// negative checks: Landlock's a read outside the input directory and
// creating a file, the filter's a socket, a signal to the bot, the bot's
// limits, F_SETOWN, shmget, chmod and inotify; see checkSandboxed), then
// probeMedia and a one-frame renderSheet with font. That catches a blocked
// Landlock, a bad rlimit, a broken ffmpeg and a missing font at once.
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
	case landlock && (report.ABI < minSandboxABI || !report.Seccomp):
		return report, fmt.Errorf("sandbox check: Landlock ABI %d or later and the seccomp filter were not both applied", minSandboxABI)
	case landlock && !report.TaskCap:
		return report, fmt.Errorf("sandbox check: %w", errTaskCapUnbound)
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
