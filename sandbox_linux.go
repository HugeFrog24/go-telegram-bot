//go:build linux

package main

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The media sandbox (Task 2b; Rulings 13 and 14 of
// docs/superpowers/research/2026-11-16-untrusted-video-parsing-decisions.md).
// On Linux every ffprobe/ffmpeg run is
//
//	/proc/self/exe __media_sandbox <spec> -- <tool> <args…>
//
// and main hands that to runMediaSandbox before anything else. The helper
// limits itself, restricts itself with Landlock and then execs the tool in
// place: the same PID and process group, so Pdeathsig and the group kill reach
// the tool as they reached the helper.

// sandboxExe is how the bot runs itself again: the binary it is running from,
// even if the file on disk has been replaced since.
const sandboxExe = "/proc/self/exe"

// toolOpenFiles is RLIMIT_NOFILE inside the sandbox.
const toolOpenFiles = 64

// toolLockedMemory is RLIMIT_MEMLOCK inside the sandbox, in bytes (Ruling 19
// item 3): the only mlock the traces saw is libgcrypt's 16 KiB of secure
// memory on Ubuntu, and the rest of the 1.5 GiB a tool may map stays
// swappable.
const toolLockedMemory = 64 << 10

// toolThreadHeadroom is how many tasks a tool may add to its user's count
// (RLIMIT_NPROC, Ruling 18 item 2): ffmpeg's decoder, filter and encoder
// threads with room to spare, but no thread bomb. With compose's pids_limit
// of 256, four runs and the bot fit.
const toolThreadHeadroom = 32

// A spec's memory limit and CPU seconds must be within these bounds.
const (
	minSandboxMemory = 64 << 20
	maxSandboxMemory = 64 << 30
	maxSandboxCPU    = 3600
)

// sandboxSpec is all the helper is told. The bot builds it from its own paths
// and constants; nothing in it comes from a user, a file or the model.
type sandboxSpec struct {
	InputDir         string `json:"input_dir"`                   // read only: the directory that holds only the input
	FontFile         string `json:"font_file,omitempty"`         // read only: drawtext's font, if any
	Tool             string `json:"tool,omitempty"`              // read and execute: the tool ("" for a check)
	MemoryLimit      uint64 `json:"memory_limit"`                // RLIMIT_AS, soft and hard
	CPUSeconds       uint64 `json:"cpu_seconds"`                 // RLIMIT_CPU, soft and hard: twice the call's timeout
	AllowUnsandboxed bool   `json:"allow_unsandboxed,omitempty"` // a kernel without Landlock is not an error
}

// String is the spec as the helper's argument.
func (s sandboxSpec) String() string {
	b, _ := json.Marshal(s) // strings, a number and a bool always marshal
	return string(b)
}

// validate checks a spec on both sides of the re-exec.
func (s sandboxSpec) validate() error {
	if err := sandboxPath("input directory", s.InputDir); err != nil {
		return err
	}
	if s.InputDir == "/" {
		return errors.New("the input directory cannot be /")
	}
	for what, p := range map[string]string{"font": s.FontFile, "tool": s.Tool} {
		if p == "" {
			continue
		}
		if err := sandboxPath(what, p); err != nil {
			return err
		}
	}
	if s.MemoryLimit < minSandboxMemory || s.MemoryLimit > maxSandboxMemory {
		return fmt.Errorf("memory limit %d out of range", s.MemoryLimit)
	}
	if s.CPUSeconds < 1 || s.CPUSeconds > maxSandboxCPU {
		return fmt.Errorf("CPU limit %d s out of range", s.CPUSeconds)
	}
	return nil
}

// sandboxPath refuses anything but a clean, absolute, UTF-8 path.
func sandboxPath(what, p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.IndexByte(p, 0) >= 0 || !utf8.ValidString(p) {
		return fmt.Errorf("the %s %q is not a clean absolute path", what, p)
	}
	return nil
}

// newSandboxSpec builds the spec for a run in dir: a tool run when tool is
// set, a check otherwise, with cpu seconds of CPU time (cpuTimeLimit). It
// fails with errNoSandbox when there is no Landlock and no bot allowed
// running without it.
func newSandboxSpec(dir, font, tool string, cpu int) (sandboxSpec, error) {
	if font != "" {
		font = filepath.Clean(font) // the same file; resolveFontFile already took it
	}
	spec := sandboxSpec{InputDir: dir, FontFile: font, Tool: tool, MemoryLimit: toolMemoryLimit,
		CPUSeconds: uint64(max(cpu, 1)), AllowUnsandboxed: unsandboxedAllowed()} // #nosec G115 -- positive, from cpuTimeLimit
	if err := sandboxUsable(sandboxAvailable()); err != nil && !spec.AllowUnsandboxed {
		return spec, fmt.Errorf("%w: %w", errNoSandbox, err)
	}
	if err := spec.validate(); err != nil {
		return spec, err
	}
	return spec, inputDirSafe(dir)
}

// inputDirSafe says why dir cannot be a run's input directory, which the
// sandbox lets the tool read, or returns nil (Ruling 16 item 4). It must not
// be one of the bot's own directories, hold one or lie beneath one (links
// followed), and it must hold exactly one entry: the input. A bot whose
// working directory is / still has its config/ and data/ checked; everything
// lies beneath /, so / itself cannot count.
func inputDirSafe(dir string) error {
	dirs, err := botDirs()
	if err != nil {
		return fmt.Errorf("cannot find the bot's directories: %w", err)
	}
	in := realPath(dir)
	for _, d := range dirs {
		bot := realPath(d)
		if bot == "/" {
			continue
		}
		if within(bot, in) || within(in, bot) {
			return fmt.Errorf("the input directory %s is, holds or lies beneath the bot's directory %s", dir, d)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("the input directory: %w", err)
	}
	if len(entries) != 1 {
		return fmt.Errorf("the input directory %s holds %d entries, not only the input", dir, len(entries))
	}
	return nil
}

// sandboxInvocation is a parsed helper command line.
type sandboxInvocation struct {
	spec            sandboxSpec
	argv            []string // a tool run: the tool and its arguments
	check           bool     // a check run instead (the gate's self-test)
	inside, outside string   // a check run's files: one in the input directory, one elsewhere
}

// parseSandboxArgs parses what follows mediaSandboxArg: either
// <spec> -- <tool> [args…], where the tool is the spec's, or
// <spec> --check <inside> <outside> with no tool in the spec.
func parseSandboxArgs(args []string) (sandboxInvocation, error) {
	if len(args) < 2 {
		return sandboxInvocation{}, errors.New("usage: " + mediaSandboxArg + " <spec> -- <tool> [args...] | --check <inside> <outside>")
	}
	spec, err := decodeSandboxSpec(args[0])
	if err != nil {
		return sandboxInvocation{}, err
	}
	if err := spec.validate(); err != nil {
		return sandboxInvocation{}, err
	}
	inv := sandboxInvocation{spec: spec}
	switch args[1] {
	case "--":
		inv.argv = args[2:]
		if spec.Tool == "" || len(inv.argv) == 0 || inv.argv[0] != spec.Tool {
			return sandboxInvocation{}, errors.New("the tool after -- is not the spec's")
		}
	case "--check":
		if spec.Tool != "" || len(args) != 4 {
			return sandboxInvocation{}, errors.New("a check takes two files and no tool")
		}
		inv.check, inv.inside, inv.outside = true, args[2], args[3]
		if sandboxPath("file", inv.inside) != nil || sandboxPath("file", inv.outside) != nil ||
			filepath.Dir(inv.inside) != spec.InputDir || within(spec.InputDir, inv.outside) {
			return sandboxInvocation{}, errors.New("a check needs a file in the input directory and one outside it")
		}
	default:
		return sandboxInvocation{}, fmt.Errorf("unknown mode %q", args[1])
	}
	return inv, nil
}

// decodeSandboxSpec decodes exactly one JSON spec with known fields only.
func decodeSandboxSpec(s string) (sandboxSpec, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var spec sandboxSpec
	if err := dec.Decode(&spec); err != nil {
		return sandboxSpec{}, fmt.Errorf("malformed spec: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return sandboxSpec{}, errors.New("malformed spec: more after it")
	}
	return spec, nil
}

// within reports whether p is dir or beneath it (both clean and absolute).
func within(dir, p string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// runMediaSandbox is the helper. It applies the sandbox to itself and execs
// the tool, so on success it never returns; a check run prints its report and
// exits 0. Any failure is a line on stderr (the bot logs the tail) and
// sandboxExitCode, and the tool never runs.
func runMediaSandbox(args []string) int {
	ppid := unix.Getppid()
	// Every step below and the exec happen on this one thread: Landlock,
	// no_new_privs and the re-armed Pdeathsig are all per thread.
	runtime.LockOSThread()
	// The helper allocates a few kilobytes and lives for milliseconds: no
	// collection is worth the mappings one might need under the limit. And
	// one P keeps its runtime to a few threads, well within the task cap
	// that its last moments run under (underLastLimits).
	debug.SetGCPercent(-1)
	runtime.GOMAXPROCS(1)
	if err := onSandboxThread(func() error { return mediaSandbox(args, ppid) }); err != nil {
		fmt.Fprintf(os.Stderr, "media sandbox: %v\n", err)
		return sandboxExitCode
	}
	return 0
}

// sandboxFreshThread, set only by the tests (sandbox_linux_test.go), runs the
// helper's steps and the exec on a new thread instead of this one. The main
// thread keeps the Pdeathsig it inherited, so a helper that stays on it would
// pass the parent-death test without its re-arm; on a new thread only the
// re-arm keeps the signal (review Minor 3).
var sandboxFreshThread bool

// onSandboxThread runs f on the calling locked thread, or with
// sandboxFreshThread on a new locked one.
func onSandboxThread(f func() error) error {
	if !sandboxFreshThread {
		return f()
	}
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // a thread of its own: the caller holds the main one
		done <- f()
	}()
	return <-done
}

// sandboxVariant is how the tests may change the sandbox for one of their
// stand-in tools (sandboxTestVariant).
type sandboxVariant struct {
	memory    uint64 // RLIMIT_AS instead of the spec's; 0 keeps it
	maxABI    int    // Landlock at this ABI at most; 0 keeps the kernel's
	noSeccomp bool   // no seccomp filter
}

// sandboxTestVariant, set only by sandbox_linux_test.go, picks a variant by
// the tool's name: the test binary run as a stand-in tool needs more address
// space than toolMemoryLimit, since on glibc the Go runtime alone takes about
// 1.6 GiB; some stand-ins must fork, which the filter refuses; and the
// signal tests hold Landlock to ABI 4, before it scoped signals. Production
// code never sets it.
var sandboxTestVariant func(tool string) sandboxVariant

func mediaSandbox(args []string, ppid int) error {
	inv, err := parseSandboxArgs(args)
	if err != nil {
		return err
	}
	var v sandboxVariant
	if sandboxTestVariant != nil {
		if v = sandboxTestVariant(inv.spec.Tool); v.memory > 0 {
			inv.spec.MemoryLimit = v.memory
		}
	}
	report, limits, err := applySandbox(inv.spec, ppid, v)
	if err != nil {
		return err
	}
	if inv.check {
		if report.ABI > 0 || report.Seccomp {
			if err := checkSandboxed(inv.spec.InputDir, inv.inside, inv.outside, report); err != nil {
				return err
			}
		}
		out, err := json.Marshal(report)
		if err != nil {
			return err
		}
		// The run ends right after the report, under the limit, on this
		// thread: handing back to the main thread could need a new one, and
		// no thread can be made under the limit.
		return underLastLimits(limits, []byte("media sandbox: writing the report failed: errno "), func() unix.Errno {
			if errno := rawWrite(1, out); errno != 0 {
				return errno
			}
			exitNow(0)
			return 0
		})
	}
	return execTool(inv.spec, limits, inv.argv, os.Environ())
}

// applySandbox applies, in order: the rlimits (the address-space and task
// limits themselves come last, see underLastLimits), oom_score_adj (best
// effort), Pdeathsig re-armed on this thread, no_new_privs, Landlock at the
// best ABI the kernel has (see sandboxABI), and the seccomp filter (see
// sandboxSeccomp), all on this thread, which then execs the tool.
func applySandbox(spec sandboxSpec, ppid int, v sandboxVariant) (sandboxReport, lastLimits, error) {
	var report sandboxReport
	limits, err := setToolLimits(spec.MemoryLimit, spec.CPUSeconds)
	if err != nil {
		return report, limits, err
	}
	report.OOMScoreAdj = setOOMScoreAdj() == nil
	// Pdeathsig belongs to the thread that inherited it, and a new thread
	// starts without it. Exec from another thread would drop it, so this one
	// takes it again, and a parent already gone means no run (Ruling 14 note 2).
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0); err != nil {
		return report, limits, fmt.Errorf("PR_SET_PDEATHSIG: %w", err)
	}
	if unix.Getppid() != ppid {
		return report, limits, errors.New("the bot that started this run is gone")
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return report, limits, fmt.Errorf("PR_SET_NO_NEW_PRIVS: %w", err)
	}
	kernel, err := landlockABI()
	abi, err := sandboxABI(kernel, err, spec.AllowUnsandboxed)
	if err != nil {
		return report, limits, err
	}
	if v.maxABI > 0 {
		abi = min(abi, v.maxABI)
	}
	if abi > 0 {
		report.ABI = abi
		// A tool that is no ELF file (a test's shell script) gets no
		// interpreter rule; a dynamically linked tool without one cannot start.
		interp := ""
		if spec.Tool != "" {
			interp, _ = elfInterpreter(spec.Tool)
		}
		if err := restrictSelf(abi, sandboxRules(spec, interp)); err != nil {
			return report, limits, err
		}
	}
	if !v.noSeccomp {
		report.Seccomp, err = sandboxSeccomp(spec.AllowUnsandboxed)
	}
	return report, limits, err
}

// errNoSeccomp means the kernel, or a container's own seccomp profile, offers
// no seccomp filter the helper can install. That is no sandbox either.
var errNoSeccomp = errors.New("no seccomp filter")

// sandboxSeccomp installs the seccomp filter for this process, which becomes
// the tool (Ruling 16 item 1), and reports whether it did. Without one it
// fails unless the spec allows running without a sandbox.
func sandboxSeccomp(allowUnsandboxed bool) (bool, error) {
	if err := seccompAvailable(); err != nil {
		if allowUnsandboxed && errors.Is(err, errNoSeccomp) {
			return false, nil
		}
		return false, err
	}
	return true, installSeccomp(os.Getpid())
}

// sandboxABI is the Landlock ABI the helper applies, from the kernel's
// answer: its own ABI, or 0 for none. No Landlock at all, or an ABI before
// minSandboxABI, is no sandbox and fails unless the spec allows running
// without one; with that allowance a weak ABI still applies what it can deny
// (Ruling 15). A Landlock that fails in any other way is never skipped.
func sandboxABI(kernel int, err error, allowUnsandboxed bool) (int, error) {
	switch {
	case err != nil && allowUnsandboxed && errors.Is(err, errNoLandlock):
		return 0, nil
	case err != nil:
		return 0, err
	case !allowUnsandboxed:
		if err := sandboxUsable(kernel, nil); err != nil {
			return 0, err
		}
	}
	return kernel, nil
}

// lastLimits are the limits underLastLimits sets just before the helper's
// last system call, since the helper's own Go runtime could not live under
// them: RLIMIT_AS and RLIMIT_NPROC, soft and hard.
type lastLimits struct {
	memory uint64 // bytes of address space
	tasks  uint64 // the user's tasks
}

// setToolLimits sets core and file size to 0, open files to toolOpenFiles,
// locked memory to toolLockedMemory and CPU time to cpu seconds (Ruling 16
// item 2: ffprobe has no -timelimit), and works out the last limits: the
// address space, memory, where a hard limit already below it is an error,
// never clamped (Ruling 14 note 3), and the user's tasks, its count now plus
// toolThreadHeadroom (Ruling 18 item 2), counted here, before Landlock takes
// /proc away. A lower inherited open-files, locked-memory, CPU or task hard
// limit is kept: it is only stricter.
func setToolLimits(memory, cpu uint64) (lastLimits, error) {
	var as, files, locked, cpuTime, tasks syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &as); err != nil {
		return lastLimits{}, fmt.Errorf("getrlimit RLIMIT_AS: %w", err)
	}
	if as.Max < memory {
		return lastLimits{}, fmt.Errorf("the hard address-space limit %d is below the %d ffmpeg runs under", as.Max, memory)
	}
	for _, l := range []struct {
		resource int
		limit    *syscall.Rlimit
	}{{syscall.RLIMIT_NOFILE, &files}, {unix.RLIMIT_MEMLOCK, &locked}, {syscall.RLIMIT_CPU, &cpuTime}, {unix.RLIMIT_NPROC, &tasks}} {
		if err := syscall.Getrlimit(l.resource, l.limit); err != nil {
			return lastLimits{}, fmt.Errorf("getrlimit %d: %w", l.resource, err)
		}
	}
	visible, err := uidTasks(os.Getuid())
	if err != nil {
		return lastLimits{}, fmt.Errorf("counting the user's tasks: %w", err)
	}
	running, err := kernelUIDTasks(visible, tasks.Max, func(limit uint64) (bool, error) { return forksUnder(limit, tasks.Max) })
	if err != nil {
		return lastLimits{}, err
	}
	for _, l := range []struct {
		name     string
		resource int
		value    uint64
	}{
		{"RLIMIT_CORE", syscall.RLIMIT_CORE, 0},
		{"RLIMIT_FSIZE", syscall.RLIMIT_FSIZE, 0},
		{"RLIMIT_NOFILE", syscall.RLIMIT_NOFILE, min(toolOpenFiles, files.Max)},
		{"RLIMIT_MEMLOCK", unix.RLIMIT_MEMLOCK, min(toolLockedMemory, locked.Max)},
		{"RLIMIT_CPU", syscall.RLIMIT_CPU, min(cpu, cpuTime.Max)},
	} {
		// syscall's Setrlimit, not unix's: for RLIMIT_NOFILE it also stops
		// Go restoring the limit it saved at startup in a later exec.
		if err := syscall.Setrlimit(l.resource, &syscall.Rlimit{Cur: l.value, Max: l.value}); err != nil {
			return lastLimits{}, fmt.Errorf("setrlimit %s: %w", l.name, err)
		}
	}
	return lastLimits{memory: memory, tasks: min(uint64(running)+toolThreadHeadroom, tasks.Max)}, nil // #nosec G115 -- a count
}

// uidTasks counts the tasks (threads) whose real uid is uid, which is what
// RLIMIT_NPROC limits, from /proc. It runs before Landlock takes /proc away.
// A process that ends while it is read is skipped.
func uidTasks(uid int) (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	want := strconv.Itoa(uid)
	count := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a process
		}
		status, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status")) // #nosec G304 -- a procfs path
		if err != nil {
			continue
		}
		var realUID string
		threads := 0
		for line := range strings.SplitSeq(string(status), "\n") {
			if v, ok := strings.CutPrefix(line, "Uid:"); ok {
				if f := strings.Fields(v); len(f) > 0 {
					realUID = f[0]
				}
			} else if v, ok := strings.CutPrefix(line, "Threads:"); ok {
				threads, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
		if realUID == want {
			count += threads
		}
	}
	return count, nil
}

// maxUserTasks bounds the task-count search: no user has more tasks than
// the kernel has PIDs (PID_MAX_LIMIT, 4,194,304 on 64-bit systems).
const maxUserTasks = 1 << 22

// maxTaskProbes caps the forks one count may take: the doubling reaches
// maxUserTasks in at most 16, and the binary search takes no more.
const maxTaskProbes = 64

// kernelUIDTasks is the kernel's own count of the user's tasks, which is
// what RLIMIT_NPROC compares with. /proc shows only this PID namespace's
// tasks (visible, a lower bound), while a container shares its uid with the
// host and other containers: in Docker, uid 1000 with 1 visible task could
// not fork under a limit of 40. So the count is found by trying: forks
// reports whether a probe can fork under a soft limit (forksUnder), by
// doubling and then a binary search. The search ends whatever the answers
// (Ruling 19 item 5): a limit that never lets a fork through, up to the
// hard limit or to maxUserTasks, and a search over maxTaskProbes forks are
// errors, and so fail closed.
func kernelUIDTasks(visible int, hard uint64, forks func(limit uint64) (bool, error)) (int, error) {
	probes := 0
	try := func(limit uint64) (bool, error) {
		if probes++; probes > maxTaskProbes {
			return false, fmt.Errorf("the task count did not settle in %d forks", maxTaskProbes)
		}
		return forks(limit)
	}
	lo, hi, err := taskBracket(uint64(visible), hard, try) // #nosec G115 -- a count
	if err != nil {
		return 0, err
	}
	if ok, err := try(lo); err != nil || ok {
		return visible, err // the limit is not enforced (a privileged user): the count is moot
	}
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		ok, err := try(mid)
		if err != nil {
			return 0, err
		}
		if ok {
			hi = mid
		} else {
			lo = mid
		}
	}
	// hi, the smallest limit that lets the probe fork, is the count plus
	// two: the probe itself is one of the user's tasks.
	return int(lo) - 1, nil // #nosec G115 -- a count
}

// taskBracket finds limits lo, which does not let a fork through (the count
// is at least visible), and hi, which does, by doubling up to the hard limit
// or maxUserTasks, whichever is lower, so it never overflows. A fork refused
// there is the user out of tasks, or, above maxUserTasks, something other
// than the limit refusing it, such as a full pids cgroup.
func taskBracket(visible, hard uint64, forks func(limit uint64) (bool, error)) (lo, hi uint64, err error) {
	ceiling := min(hard, maxUserTasks)
	lo, hi = visible, min(visible+2*toolThreadHeadroom, ceiling)
	for {
		ok, err := forks(hi)
		switch {
		case err != nil:
			return 0, 0, err
		case ok:
			return lo, hi, nil
		case hi == ceiling && ceiling == hard:
			return 0, 0, fmt.Errorf("the user has reached its task limit (%d)", hard)
		case hi == ceiling:
			return 0, 0, fmt.Errorf("a fork is refused even under a task limit of %d, above any user's count: "+
				"something else refuses it, such as a full pids cgroup", ceiling)
		}
		lo, hi = hi, min(2*hi, ceiling)
	}
}

// Exit codes of a task-count probe (taskProbe).
const (
	probeForked    = 0
	probeRefused   = 1
	probeBrokeDown = 2
)

// forksUnder reports whether a process whose RLIMIT_NPROC soft limit is
// limit can fork: whether the user then has fewer tasks than limit. The
// limit is tried in a probe of its own, a raw fork of this thread that
// makes system calls only and exits (taskProbe), never in the helper: at
// the user's count it would also stop the helper's Go runtime starting a
// thread, which the runtime may do at any moment and does not survive.
// Signals stay blocked from the fork on, so that no Go signal handler runs
// in the probe.
func forksUnder(limit, hard uint64) (bool, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	lim := unix.Rlimit{Cur: limit, Max: hard}
	var all, old unix.Sigset_t
	for i := range all.Val {
		all.Val[i] = ^all.Val[i]
	}
	if err := unix.PthreadSigmask(unix.SIG_SETMASK, &all, &old); err != nil {
		return false, fmt.Errorf("blocking signals: %w", err)
	}
	pid, errno := rawFork()
	if errno == 0 && pid == 0 {
		taskProbe(&lim) // never returns
	}
	if err := unix.PthreadSigmask(unix.SIG_SETMASK, &old, nil); err != nil {
		return false, fmt.Errorf("restoring the signal mask: %w", err)
	}
	if errno != 0 {
		return false, fmt.Errorf("a task-count probe: fork: %w", errno)
	}
	var ws unix.WaitStatus
	for {
		_, err := unix.Wait4(int(pid), &ws, 0, nil) // #nosec G115 -- a PID
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EINTR) {
			return false, fmt.Errorf("a task-count probe: wait: %w", err)
		}
	}
	switch {
	case ws.Exited() && ws.ExitStatus() == probeForked:
		return true, nil
	case ws.Exited() && ws.ExitStatus() == probeRefused:
		return false, nil
	}
	return false, fmt.Errorf("a task-count probe failed (wait status %#x)", uint32(ws))
}

// taskProbe is the probe forksUnder forks: one thread, a copy of the
// helper's memory, and none of the Go runtime's help. So it only makes raw
// system calls and never allocates or grows its stack: it sets its own soft
// RLIMIT_NPROC to lim, forks a child that exits at once, reaps it and exits
// with probeForked; probeRefused if the fork was refused for want of tasks.
//
//go:nosplit
//go:norace
func taskProbe(lim *unix.Rlimit) {
	code := uintptr(probeBrokeDown)
	_, _, errno := syscall.RawSyscall6(unix.SYS_PRLIMIT64, 0, unix.RLIMIT_NPROC, uintptr(unsafe.Pointer(lim)), 0, 0, 0) // #nosec G103 -- prlimit(2) of a struct
	if errno == 0 {
		pid, errno := rawFork()
		switch {
		case errno == unix.EAGAIN:
			code = probeRefused
		case errno != 0:
		case pid == 0:
			code = probeForked // the child: exits at once
		default:
			_, _, errno = syscall.RawSyscall6(unix.SYS_WAIT4, pid, 0, 0, 0, 0, 0)
			if errno == 0 {
				code = probeForked
			}
		}
	}
	for {
		_, _, _ = syscall.RawSyscall(unix.SYS_EXIT_GROUP, code, 0, 0)
	}
}

// rawFork is fork(2) as clone(2) with SIGCHLD, which every architecture
// has (AArch64 has no fork): the child's PID, or 0 in the child.
//
//go:nosplit
//go:norace
func rawFork() (uintptr, syscall.Errno) {
	if runtime.GOARCH == "s390x" { // clone(stack, flags, ...) there
		pid, _, errno := syscall.RawSyscall6(unix.SYS_CLONE, 0, uintptr(syscall.SIGCHLD), 0, 0, 0, 0)
		return pid, errno
	}
	pid, _, errno := syscall.RawSyscall6(unix.SYS_CLONE, uintptr(syscall.SIGCHLD), 0, 0, 0, 0, 0)
	return pid, errno
}

// underLastLimits sets RLIMIT_NPROC and then RLIMIT_AS, soft and hard, to
// l and then makes the one system call last, allocating nothing in between.
// These limits have to come last, as the helper's own Go runtime could not
// live under them. It already holds more address space than toolMemoryLimit
// on glibc (about 1.6 GiB measured on Ubuntu 24.04, 1.2 GiB on Alpine), and
// the kernel refuses every new mapping to a process over its limit, so the
// helper would die at its next heap growth. And on a many-CPU host its
// runtime may start dozens of threads (a collection's workers, say), and it
// dies if one cannot start. The tool gets the limits all the same: execve
// keeps them. If a limit fails the helper may still allocate, but if last
// fails nothing may any more: failUnderLimit reports failMsg, made before
// the limits, and the errno, and ends the helper (review Minor 7).
func underLastLimits(l lastLimits, failMsg []byte, last func() unix.Errno) error {
	tasks := unix.Rlimit{Cur: l.tasks, Max: l.tasks}
	if _, _, errno := unix.RawSyscall6(unix.SYS_PRLIMIT64, 0, unix.RLIMIT_NPROC,
		uintptr(unsafe.Pointer(&tasks)), 0, 0, 0); errno != 0 { // #nosec G103 -- prlimit(2) of this process
		return fmt.Errorf("setrlimit RLIMIT_NPROC: %w", errno)
	}
	memory := unix.Rlimit{Cur: l.memory, Max: l.memory}
	if _, _, errno := unix.RawSyscall6(unix.SYS_PRLIMIT64, 0, unix.RLIMIT_AS,
		uintptr(unsafe.Pointer(&memory)), 0, 0, 0); errno != 0 { // #nosec G103 -- prlimit(2) of this process
		return fmt.Errorf("setrlimit RLIMIT_AS: %w", errno)
	}
	if errno := last(); errno != 0 {
		failUnderLimit(failMsg, errno)
	}
	return nil
}

// failUnderLimit writes msg and errno, in decimal, to stderr and exits with
// sandboxExitCode, all through raw system calls: the digits go into an array
// on the stack, and with the collector off no stack has shrunk since the
// helper's deeper calls, so nothing here allocates. It never returns.
func failUnderLimit(msg []byte, errno unix.Errno) {
	var num [24]byte
	i := len(num) - 1
	num[i] = '\n'
	for n := uint64(errno); ; n /= 10 {
		i--
		num[i] = byte('0' + n%10)
		if n < 10 {
			break
		}
	}
	rawWrite(2, msg)
	rawWrite(2, num[i:])
	exitNow(sandboxExitCode)
}

// exitNow ends the helper with the exit_group system call: no deferred call,
// no runtime exit path, nothing that might allocate or need another thread.
func exitNow(code int) {
	_, _, _ = unix.RawSyscall(unix.SYS_EXIT_GROUP, uintptr(code), 0, 0)
}

// rawWrite is write(2) of b to fd, with no allocation.
func rawWrite(fd int, b []byte) unix.Errno {
	if len(b) == 0 {
		return 0
	}
	_, _, errno := unix.RawSyscall(unix.SYS_WRITE, uintptr(fd), uintptr(unsafe.Pointer(&b[0])), uintptr(len(b))) // #nosec G103 -- write(2)
	return errno
}

// execTool execs the tool in place, under the address-space limit. Its argv
// and environment are converted first, so execve needs no allocation. A raw
// execve, unlike syscall.Exec, does not take the runtime's exec lock against
// thread creation: the helper runs no goroutine of its own that could make
// the runtime start a thread then, and a failed exec only ends the run.
func execTool(spec sandboxSpec, limits lastLimits, argv, env []string) error {
	path, err := unix.BytePtrFromString(spec.Tool)
	if err != nil {
		return err
	}
	argvp, err := syscall.SlicePtrFromStrings(argv)
	if err != nil {
		return err
	}
	envp, err := syscall.SlicePtrFromStrings(env)
	if err != nil {
		return err
	}
	failMsg := []byte("media sandbox: execve " + spec.Tool + " failed: errno ")
	return underLastLimits(limits, failMsg, func() unix.Errno {
		// execve(2) of the bot's own tool path, checked against the spec.
		_, _, errno := unix.RawSyscall(unix.SYS_EXECVE, uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(&argvp[0])), uintptr(unsafe.Pointer(&envp[0]))) // #nosec G103 G204
		return errno
	})
}

// setOOMScoreAdj makes this process, and the tool after the exec, the OOM
// killer's first choice. It is a preference, not a control (Ruling 14 note 4).
func setOOMScoreAdj() error {
	f, err := os.OpenFile("/proc/self/oom_score_adj", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString("1000")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// landlockProbe asks once per process.
var landlockProbe = sync.OnceValues(landlockABI)

// seccompProbe asks once per process.
var seccompProbe = sync.OnceValue(seccompAvailable)

// sandboxAvailable reports the kernel's Landlock ABI, or why there is no
// sandbox: no Landlock, or no seccomp filter (Ruling 16 item 1).
func sandboxAvailable() (int, error) {
	abi, err := landlockProbe()
	if err != nil {
		return 0, err
	}
	if err := seccompProbe(); err != nil {
		return abi, err
	}
	return abi, nil
}

// sandboxSystemRules are what ffprobe and ffmpeg read of the system, as
// traced on Alpine 3.24 (ffmpeg 8.1.3) and Ubuntu 24.04 (6.1.1): the library
// directories, the font and fontconfig directories and the loaders' two
// files, all read only (Ruling 16 item 6). Nothing here may be executed:
// only the tool and its ELF interpreter can be (sandboxRules). The rest of
// what the tools try at start-up (/proc/self/status, /proc/mounts,
// /proc/filesystems, /sys/devices/system/*, /etc/gnutls/config: library
// probes) is denied, and they cope: renders are byte for byte the same.
// /usr/local/lib is where a locally built ffmpeg keeps its libraries; musl's
// loader looks there first. ffmpeg never opened /dev/null in the traces (the
// bot opens the tool's stdin before the sandbox), but reading it is harmless
// and ordinary code needs it: a shell gives a background job /dev/null.
//
// Landlock cannot hide metadata: a tool can still stat any path it names,
// learning whether it exists and its size and times (the database's,
// config/*.json's), though never its contents.
var sandboxSystemRules = []sandboxRule{
	{path: "/lib", access: fsRead, optional: true},                                // musl's loader and libc; a link to /usr/lib on merged-/usr systems
	{path: "/lib64", access: fsRead, optional: true},                              // glibc's loader path on x86-64
	{path: "/usr/lib", access: fsRead, optional: true},                            // the tools' libraries (and glibc's gconv and locales)
	{path: "/usr/lib64", access: fsRead, optional: true},                          // the same on distributions that split them
	{path: "/usr/local/lib", access: fsRead, optional: true},                      // a locally built ffmpeg's libraries
	{path: "/usr/share/fonts", access: fsRead, optional: true},                    // the fonts drawtext and fontconfig find
	{path: "/usr/share/fontconfig", access: fsRead, optional: true},               // fontconfig's own configuration files
	{path: "/etc/fonts", access: fsRead, optional: true},                          // fontconfig's configuration
	{path: "/etc/ld.so.cache", access: fsReadFile, optional: true},                // glibc's loader reads it on every start (/usr/local/lib is found only through it)
	{path: "/etc/ld-musl-*.path", access: fsReadFile, optional: true, glob: true}, // musl's loader reads its library path on every start, where one is set
	{path: "/dev/null", access: fsReadFile, optional: true},
	{path: "/dev/urandom", access: fsReadFile, optional: true}, // drawtext seeds its PRNG through av_get_random_seed on every render
}

// sandboxTestRules are extra rules for the tests' stand-in tools. Production
// code never sets them (sandbox_linux_test.go does).
var sandboxTestRules []sandboxRule

// sandboxRules is the whole policy for a spec: the system rules, the input
// directory and font read only, and the tool and its ELF interpreter (interp,
// "" for none) read and execute. No rule grants writing, and nothing grants
// /proc, /sys or the rest of /etc and /usr.
func sandboxRules(spec sandboxSpec, interp string) []sandboxRule {
	rules := append(slices.Clone(sandboxSystemRules), sandboxRule{path: spec.InputDir, access: fsRead, kind: mustBeDir})
	if spec.FontFile != "" {
		rules = append(rules, sandboxRule{path: spec.FontFile, access: fsReadFile, kind: mustBeFile})
	}
	if spec.Tool != "" {
		rules = append(rules, sandboxRule{path: spec.Tool, access: fsReadExec, kind: mustBeFile})
	}
	if interp != "" {
		rules = append(rules, sandboxRule{path: interp, access: fsReadExec, kind: mustBeFile})
	}
	return append(rules, sandboxTestRules...)
}

// elfInterpreter is the ELF interpreter bin names in its PT_INTERP header,
// such as /lib/ld-musl-x86_64.so.1, or "" when it names none (a static
// binary). The kernel opens it to run bin, so it needs execute too.
func elfInterpreter(bin string) (string, error) {
	f, err := elf.Open(bin)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(p.Open(), 4096))
		if err != nil {
			return "", err
		}
		interp := strings.TrimRight(string(b), "\x00")
		if sandboxPath("ELF interpreter", interp) != nil {
			return "", fmt.Errorf("%s names the ELF interpreter %q", bin, interp)
		}
		return interp, nil
	}
	return "", nil
}

// sandboxProbeName is the file a check tries to create.
func sandboxProbeName() string {
	return ".media-sandbox-check-" + strconv.Itoa(os.Getpid())
}

// checkSandboxed is the check run's test of the sandbox it is in, layer by
// layer as the report says it is (Ruling 18 item 3). With Landlock: inside
// (in the input directory) reads, outside does not, no file can be created
// in either directory, and from ABI 4 no TCP socket can bind. With the
// seccomp filter: the seccompDenies probes. It returns every failure at once.
func checkSandboxed(inputDir, inside, outside string, report sandboxReport) error {
	var failed []string
	if report.ABI > 0 {
		failed = append(failed, landlockDenies(inputDir, inside, outside, report.ABI)...)
	}
	if report.Seccomp {
		failed = append(failed, seccompDenies(outside)...)
	}
	if len(failed) > 0 {
		return errors.New("sandbox check failed: " + strings.Join(failed, "; "))
	}
	return nil
}

// landlockDenies checks what Landlock at abi must refuse; checkSandboxed
// runs it only when Landlock is there (Ruling 18 item 3), so a host without
// Landlock under video.allow_unsandboxed passes on the filter alone.
func landlockDenies(inputDir, inside, outside string, abi int) []string {
	var failed []string
	if _, err := os.ReadFile(inside); err != nil { // #nosec G304 -- the bot's own self-test file
		failed = append(failed, fmt.Sprintf("cannot read %s: %v", inside, err))
	}
	if _, err := os.ReadFile(outside); !errors.Is(err, os.ErrPermission) { // #nosec G304 -- the bot's own self-test file
		failed = append(failed, fmt.Sprintf("read %s outside the input directory (%v)", outside, err))
	}
	for _, dir := range []string{inputDir, filepath.Dir(outside)} {
		p := filepath.Join(dir, sandboxProbeName())
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a fixed name in the bot's own directories
		switch {
		case err == nil:
			_ = f.Close()
			_ = os.Remove(p)
			failed = append(failed, "created a file in "+dir)
		case !errors.Is(err, os.ErrPermission):
			failed = append(failed, fmt.Sprintf("creating a file in %s failed oddly: %v", dir, err))
		}
	}
	if abi >= 4 {
		if err := tcpBindDenied(); err != nil {
			failed = append(failed, err.Error())
		}
	}
	return failed
}

// seccompDenies checks what only the seccomp filter refuses at any Landlock
// ABI (Ruling 18 item 3): making a socket, signalling the bot (the parent),
// reading the bot's resource limits, taking ownership of a pipe (F_SETOWN,
// a way to signal below ABI 6), SysV shared memory, a mode change outside
// the input directory and an inotify instance. Each must fail with EPERM
// (an argument check) or ENOSYS (a call off the allowlist). None would
// change anything if it worked: the owner would be the helper itself, the
// mode the outside file's own, and a segment is removed again.
func seccompDenies(outside string) []string {
	var failed []string
	refused := func(what string, err error) {
		switch {
		case err == nil:
			failed = append(failed, what+" worked")
		case !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.ENOSYS):
			failed = append(failed, fmt.Sprintf("%s failed oddly: %v", what, err))
		}
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err == nil {
		_ = unix.Close(fd)
	}
	refused("making a unix socket", err)
	refused("signalling the bot", unix.Kill(unix.Getppid(), 0))
	var limit unix.Rlimit
	refused("reading the bot's limits", unix.Prlimit(unix.Getppid(), unix.RLIMIT_NOFILE, nil, &limit))
	_, err = unix.FcntlInt(2, unix.F_SETOWN, unix.Getpid())
	refused("F_SETOWN on stderr", err)
	id, err := unix.SysvShmGet(unix.IPC_PRIVATE, 4096, unix.IPC_CREAT|0o600)
	if err == nil {
		_, _ = unix.SysvShmCtl(id, unix.IPC_RMID, nil)
	}
	refused("shmget", err)
	refused("chmod outside the input directory", unix.Chmod(outside, 0o600))
	fd, err = unix.InotifyInit1(unix.IN_CLOEXEC)
	if err == nil {
		_ = unix.Close(fd)
	}
	refused("inotify_init1", err)
	return failed
}

// tcpBindDenied checks that a TCP socket cannot bind, even on the loopback.
func tcpBindDenied() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil // no TCP socket at all
	}
	defer func() { _ = unix.Close(fd) }()
	err = unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}})
	if errors.Is(err, unix.EACCES) {
		return nil
	}
	return fmt.Errorf("a TCP bind on the loopback was not denied (%v)", err)
}

// sandboxSelfCheck runs the helper in check mode on dir, the self-test's
// input directory, with inside its file, through the production runner. It
// returns the helper's report: the Landlock ABI it applied (0 for none) and
// whether oom_score_adj took.
func sandboxSelfCheck(ctx context.Context, dir, inside string) (sandboxReport, error) {
	outsideDir, err := os.MkdirTemp("", "video-selftest-outside-")
	if err != nil {
		return sandboxReport{}, err
	}
	defer func() { _ = os.RemoveAll(outsideDir) }()
	outside := filepath.Join(outsideDir, "file")
	if err := os.WriteFile(outside, []byte("outside the input directory"), 0o600); err != nil {
		return sandboxReport{}, err
	}
	spec, err := newSandboxSpec(dir, "", "", cpuTimeLimit(ctx))
	if err != nil {
		return sandboxReport{}, err
	}
	out, err := runToolCmd(ctx, "media sandbox", func(ctx context.Context) (*exec.Cmd, error) {
		return toolProcess(ctx, dir, sandboxExe, mediaSandboxArg, spec.String(), "--check", inside, outside), nil
	})
	if err != nil {
		return sandboxReport{}, err
	}
	var report sandboxReport
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		return sandboxReport{}, fmt.Errorf("the sandbox check's report %q: %w", out, err)
	}
	return report, nil
}

// addressSpaceHardLimit is the bot's hard RLIMIT_AS, which every child
// inherits. A variable so a test can lower it.
var addressSpaceHardLimit = func() (uint64, error) {
	var rl syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_AS, &rl)
	return rl.Max, err
}

// botDirs are the bot's own directories, which no sandbox rule may reach: the
// working directory and its config/ and data/ (the database and, by default,
// the stored videos). A variable so a test can place them under /usr.
var botDirs = func() ([]string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return []string{wd, filepath.Join(wd, "config"), filepath.Join(wd, "data")}, nil
}

// sandboxSparesBot says why the sandbox would let a tool read the bot's own
// files, or returns nil: a bot directory beneath a directory the system rules
// grant to every tool (the library, font and fontconfig directories:
// /lib, /lib64, /usr/lib, /usr/lib64, /usr/local/lib, /usr/share/fonts,
// /usr/share/fontconfig, /etc/fonts) would be open to all of them, the
// tokens and the chat database included (review Minor 5). Links are followed
// on both sides, so /lib on a merged-/usr system counts as /usr/lib.
func sandboxSparesBot() error {
	dirs, err := botDirs()
	if err != nil {
		return fmt.Errorf("cannot find the bot's directories: %w", err)
	}
	for _, r := range sandboxSystemRules {
		if r.access&unix.LANDLOCK_ACCESS_FS_READ_DIR == 0 {
			continue // a rule on one file
		}
		granted := realPath(r.path)
		for _, d := range dirs {
			if within(granted, realPath(d)) {
				return fmt.Errorf("the bot's directory %s is beneath %s, which the ffmpeg sandbox lets every tool read", d, r.path)
			}
		}
	}
	return nil
}

// realPath is p with its links resolved where it exists, or p cleaned.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// toolLimitsUsable says why the tools' limits cannot be set, or returns nil:
// a hard address-space limit below toolMemoryLimit turns video off rather
// than being clamped (Ruling 14 note 3).
func toolLimitsUsable() error {
	hard, err := addressSpaceHardLimit()
	if err != nil {
		return fmt.Errorf("cannot read the address-space limit: %w", err)
	}
	if hard < toolMemoryLimit {
		return fmt.Errorf("the hard address-space limit (%d bytes) is below the %d bytes ffmpeg runs under", hard, toolMemoryLimit)
	}
	return nil
}
