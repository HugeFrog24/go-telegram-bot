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
// instead (Ruling 18 items 1 and 4, Ruling 19 item 8). Without Landlock the
// tool reaches every file the bot's user may, the bot's own
// /proc/<pid>/environ and, where Yama's ptrace_scope is 0, its
// /proc/<pid>/mem. Neither layer hides a file's metadata from a read by
// path (stat, access, readlink, statfs), which the tools use, nor whether a
// process exists (a stat of /proc/<pid>), nor keeps the parent-death signal
// (Ruling 19 item 1): the filter refuses
// prctl(PR_SET_PDEATHSIG), but the signal is per task and a new thread has
// none, so a thread the tool starts can exec it again without one, and
// de_thread keeps its PID. That orphan keeps Landlock, the filter, the
// rlimits, its PID and its group; it outlives only a bot that dies, and only
// outside a supervisor: Docker's PID namespace and systemd's default
// KillMode=control-group kill it.
var sandboxGapList = []struct {
	abi     int  // the Landlock ABI that denies it; 0 for none
	seccomp bool // whether the seccomp filter denies it
	what    string
}{
	{1, false, "files: reading, writing and running any the bot's user may, /proc/<bot>/environ (any token in the bot's environment) " +
		"among them, and /proc/<bot>/mem where Yama's ptrace_scope is 0"},
	{3, false, "file truncation"},
	{4, true, "TCP"},
	{6, true, "abstract unix sockets and signals"},
	{9, true, "pathname unix sockets"},
	{10, true, "UDP"},
	{0, true, "other processes' resource limits, priority and CPU affinity"},
	{0, true, "file metadata changes (mode, owner, times, extended attributes)"},
	{0, true, "SysV IPC, inotify and fanotify"},
	{0, true, "ioctl requests (TIOCSTI among them), priority-inheriting futexes and other prctl options"},
	{0, true, "every other system call (new processes, ptrace, namespaces, io_uring, bpf)"},
	{0, false, "file and process metadata (stat: whether a path exists, its size and times, and through /proc/<pid> whether a process does)"},
	{0, false, "readlink targets and statfs"},
	{0, false, "the parent-death signal, which a thread the tool starts drops by exec: while the bot lives a timeout still kills the tool's group, " +
		"but a tool that outlives a dead bot, still sandboxed, ends only with Docker's PID namespace or a systemd unit's control-group kill"},
}

// The task cap's gap (Ruling 19 items 2 and 4): RLIMIT_NPROC is per user, so
// where it binds, the headroom is a pool every concurrent run shares, and
// where it binds no one (root), there is no cap at all.
const (
	sandboxGapTaskPool = "the user's task pool: RLIMIT_NPROC counts every task of the bot's user, so a compromised tool can take what is left of it " +
		"and make other chats' concurrent runs fail while it runs"
	sandboxGapNoTaskCap = "threads: RLIMIT_NPROC does not bind the bot's user (root), so a tool can start them until the host, " +
		"or a container's pids limit, runs out"
)

// sandboxGaps lists what the layers in r leave open: what neither Landlock
// at r.ABI nor, when r.Seccomp, the seccomp filter covers, each with the ABI
// that would, and what the task cap leaves, separated by semicolons.
func sandboxGaps(r sandboxReport) string {
	var gaps []string
	for _, g := range sandboxGapList {
		switch {
		case r.Seccomp && g.seccomp, g.abi > 0 && r.ABI >= g.abi:
		case g.abi > 0:
			gaps = append(gaps, fmt.Sprintf("%s (ABI %d)", g.what, g.abi))
		default:
			gaps = append(gaps, g.what)
		}
	}
	if r.TaskCap {
		gaps = append(gaps, sandboxGapTaskPool)
	} else {
		gaps = append(gaps, sandboxGapNoTaskCap)
	}
	return strings.Join(gaps, "; ")
}

// sandboxReport is what the helper's check run reports to the gate.
type sandboxReport struct {
	ABI            int  `json:"landlock_abi"`    // the Landlock ABI applied; 0 for none
	OOMScoreAdj    bool `json:"oom_score_adj"`   // whether oom_score_adj was set to 1000
	Seccomp        bool `json:"seccomp"`         // whether the seccomp filter was installed
	TaskCap        bool `json:"task_cap"`        // whether RLIMIT_NPROC binds the user (root is exempt)
	NoCapabilities bool `json:"no_capabilities"` // whether every capability set was empty after the drop
}

// sandboxSummary is what the gate logs, line by line, about the sandbox a
// passing self-test reported, in every mode that runs video (Ruling 19 item
// 8): the layers in place, then what they leave open. helper is whether the
// sandbox helper runs the tools at all (Linux; elsewhere there is no
// sandbox), full whether the gate found the whole sandbox; a partial one
// runs only under video.allow_unsandboxed.
func sandboxSummary(helper, full bool, r sandboxReport) []string {
	if !helper {
		return []string{"there is no sandbox on this OS: ffmpeg can do anything the bot's user can, reading, writing and running any file and using the network"}
	}
	layers := fmt.Sprintf("ffmpeg sandbox on (Landlock ABI %d, seccomp filter)", r.ABI)
	if !full {
		layers = "ffmpeg partial sandbox (" + sandboxLayers(r) + ")"
	}
	return []string{layers, "the sandbox does not cover " + sandboxGaps(r)}
}

// sandboxLayers names the layers r reports, and the missing ones.
func sandboxLayers(r sandboxReport) string {
	landlock, filter, taskCap := "no Landlock", "no seccomp filter", "no task cap: run the bot as a non-root user"
	if r.ABI > 0 {
		landlock = fmt.Sprintf("Landlock ABI %d", r.ABI)
	}
	if r.Seccomp {
		filter = "seccomp filter"
	}
	if r.TaskCap {
		taskCap = "task cap"
	}
	return landlock + ", " + filter + ", " + taskCap
}

// errNoSeccomp means the kernel, or a container's own seccomp profile, offers
// no seccomp filter the helper can install. That is no sandbox either.
var errNoSeccomp = errors.New("no seccomp filter")

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
