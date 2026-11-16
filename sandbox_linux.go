//go:build linux

package main

import (
	"bytes"
	"context"
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

// A spec's memory limit must be within these bounds.
const (
	minSandboxMemory = 64 << 20
	maxSandboxMemory = 64 << 30
)

// sandboxSpec is all the helper is told. The bot builds it from its own paths
// and constants; nothing in it comes from a user, a file or the model.
type sandboxSpec struct {
	InputDir         string `json:"input_dir"`                   // read only: the directory that holds only the input
	FontFile         string `json:"font_file,omitempty"`         // read only: drawtext's font, if any
	Tool             string `json:"tool,omitempty"`              // read and execute: the tool ("" for a check)
	MemoryLimit      uint64 `json:"memory_limit"`                // RLIMIT_AS, soft and hard
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
// set, a check otherwise. It fails with errNoSandbox when there is no
// Landlock and no bot allowed running without it.
func newSandboxSpec(dir, font, tool string) (sandboxSpec, error) {
	if font != "" {
		font = filepath.Clean(font) // the same file; resolveFontFile already took it
	}
	spec := sandboxSpec{InputDir: dir, FontFile: font, Tool: tool, MemoryLimit: toolMemoryLimit, AllowUnsandboxed: unsandboxedTools.Load()}
	if _, err := sandboxAvailable(); err != nil && !spec.AllowUnsandboxed {
		return spec, fmt.Errorf("%w: %w", errNoSandbox, err)
	}
	return spec, spec.validate()
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
// returns 0. Any failure is a line on stderr (the bot logs the tail) and
// sandboxExitCode, and the tool never runs.
func runMediaSandbox(args []string) int {
	ppid := unix.Getppid()
	// Every step below and the exec happen on this one thread: Landlock,
	// no_new_privs and the re-armed Pdeathsig are all per thread.
	runtime.LockOSThread()
	// The helper allocates a few kilobytes and lives for milliseconds: no
	// collection is worth the mappings one might need under the limit.
	debug.SetGCPercent(-1)
	if err := mediaSandbox(args, ppid); err != nil {
		fmt.Fprintf(os.Stderr, "media sandbox: %v\n", err)
		return sandboxExitCode
	}
	return 0
}

func mediaSandbox(args []string, ppid int) error {
	inv, err := parseSandboxArgs(args)
	if err != nil {
		return err
	}
	report, err := applySandbox(inv.spec, ppid)
	if err != nil {
		return err
	}
	if inv.check {
		if report.ABI > 0 {
			if err := checkSandboxed(inv.spec.InputDir, inv.inside, inv.outside, report.ABI); err != nil {
				return err
			}
		}
		out, err := json.Marshal(report)
		if err != nil {
			return err
		}
		return underMemoryLimit(inv.spec.MemoryLimit, func() unix.Errno {
			_, _, errno := unix.RawSyscall(unix.SYS_WRITE, 1, uintptr(unsafe.Pointer(&out[0])), uintptr(len(out))) // #nosec G103 -- write(2) of the report
			return errno
		})
	}
	return execTool(inv.spec, inv.argv, os.Environ())
}

// applySandbox applies, in order: the rlimits (the address-space limit
// itself comes last, see underMemoryLimit), oom_score_adj (best effort),
// Pdeathsig re-armed on this thread, no_new_privs, then Landlock at the best
// ABI the kernel has. Without Landlock it fails unless the spec allows that.
func applySandbox(spec sandboxSpec, ppid int) (sandboxReport, error) {
	var report sandboxReport
	if err := setToolLimits(spec.MemoryLimit); err != nil {
		return report, err
	}
	report.OOMScoreAdj = setOOMScoreAdj() == nil
	// Pdeathsig belongs to the thread that inherited it, and a new thread
	// starts without it. Exec from another thread would drop it, so this one
	// takes it again, and a parent already gone means no run (Ruling 14 note 2).
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0); err != nil {
		return report, fmt.Errorf("PR_SET_PDEATHSIG: %w", err)
	}
	if unix.Getppid() != ppid {
		return report, errors.New("the bot that started this run is gone")
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return report, fmt.Errorf("PR_SET_NO_NEW_PRIVS: %w", err)
	}
	abi, err := landlockABI()
	if err != nil {
		if spec.AllowUnsandboxed && errors.Is(err, errNoLandlock) {
			return report, nil
		}
		return report, err
	}
	report.ABI = abi
	return report, restrictSelf(abi, sandboxRules(spec))
}

// setToolLimits sets core and file size to 0 and open files to
// toolOpenFiles, and checks that the address-space limit can be set to
// memory: a hard limit already below it is an error, never clamped (Ruling 14
// note 3). underMemoryLimit sets that one.
func setToolLimits(memory uint64) error {
	var as, files syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &as); err != nil {
		return fmt.Errorf("getrlimit RLIMIT_AS: %w", err)
	}
	if as.Max < memory {
		return fmt.Errorf("the hard address-space limit %d is below the %d ffmpeg runs under", as.Max, memory)
	}
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &files); err != nil {
		return fmt.Errorf("getrlimit RLIMIT_NOFILE: %w", err)
	}
	for _, l := range []struct {
		name     string
		resource int
		value    uint64
	}{
		{"RLIMIT_CORE", syscall.RLIMIT_CORE, 0},
		{"RLIMIT_FSIZE", syscall.RLIMIT_FSIZE, 0},
		{"RLIMIT_NOFILE", syscall.RLIMIT_NOFILE, min(toolOpenFiles, files.Max)},
	} {
		// syscall's Setrlimit, not unix's: for RLIMIT_NOFILE it also stops
		// Go restoring the limit it saved at startup in a later exec.
		if err := syscall.Setrlimit(l.resource, &syscall.Rlimit{Cur: l.value, Max: l.value}); err != nil {
			return fmt.Errorf("setrlimit %s: %w", l.name, err)
		}
	}
	return nil
}

// underMemoryLimit sets RLIMIT_AS, soft and hard, to memory and then makes
// the one system call last, allocating nothing in between. The limit has to
// come last: the helper's own Go runtime already holds more address space
// than toolMemoryLimit on glibc (about 1.6 GiB measured on Ubuntu 24.04, 1.2
// GiB on Alpine), and the kernel refuses every new mapping to a process over
// its limit, so the helper would die at its next heap growth. The tool gets
// the limit all the same: execve keeps it.
func underMemoryLimit(memory uint64, last func() unix.Errno) error {
	limit := unix.Rlimit{Cur: memory, Max: memory}
	if _, _, errno := unix.RawSyscall6(unix.SYS_PRLIMIT64, 0, unix.RLIMIT_AS,
		uintptr(unsafe.Pointer(&limit)), 0, 0, 0); errno != 0 { // #nosec G103 -- prlimit(2) of this process
		return fmt.Errorf("setrlimit RLIMIT_AS: %w", errno)
	}
	if errno := last(); errno != 0 {
		return errno
	}
	return nil
}

// execTool execs the tool in place, under the address-space limit. Its argv
// and environment are converted first, so execve needs no allocation. A raw
// execve, unlike syscall.Exec, does not take the runtime's exec lock against
// thread creation: the helper runs no goroutine of its own that could make
// the runtime start a thread then, and a failed exec only ends the run.
func execTool(spec sandboxSpec, argv, env []string) error {
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
	err = underMemoryLimit(spec.MemoryLimit, func() unix.Errno {
		// execve(2) of the bot's own tool path, checked against the spec.
		_, _, errno := unix.RawSyscall(unix.SYS_EXECVE, uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(&argvp[0])), uintptr(unsafe.Pointer(&envp[0]))) // #nosec G103 G204
		return errno
	})
	return fmt.Errorf("exec %s: %w", spec.Tool, err)
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

// errNoLandlock means the kernel offers no Landlock: not built in, disabled
// at boot, or blocked (a seccomp profile that answers EPERM).
var errNoLandlock = errors.New("no Landlock")

// landlockABI asks the kernel for its Landlock ABI version.
func landlockABI() (int, error) {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	switch {
	case errno == unix.ENOSYS || errno == unix.EOPNOTSUPP || errno == unix.EPERM:
		return 0, fmt.Errorf("%w: landlock_create_ruleset: %w", errNoLandlock, errno)
	case errno != 0:
		return 0, fmt.Errorf("landlock_create_ruleset: %w", errno)
	case int(abi) < 1:
		return 0, fmt.Errorf("%w: ABI %d", errNoLandlock, int(abi))
	}
	return int(abi), nil
}

// landlockProbe asks once per process.
var landlockProbe = sync.OnceValues(landlockABI)

// sandboxAvailable reports the kernel's Landlock ABI, or why there is none.
func sandboxAvailable() (int, error) { return landlockProbe() }

// Landlock access rights x/sys v0.35.0 does not name yet (linux/landlock.h).
const (
	landlockAccessFSResolveUnix     = 1 << 16 // ABI 9: connecting to a pathname unix socket
	landlockAccessNetBindUDP        = 1 << 2  // ABI 10
	landlockAccessNetConnectSendUDP = 1 << 3  // ABI 10
)

// landlockAccess is every right the kernel's ABI knows, so that whatever no
// rule grants is denied: each ABI only adds rights (kernel docs, "Backward
// and forward compatibility").
func landlockAccess(abi int) unix.LandlockRulesetAttr {
	attr := unix.LandlockRulesetAttr{Access_fs: unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE | unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK | unix.LANDLOCK_ACCESS_FS_MAKE_SYM}
	for _, added := range []struct {
		abi                 int
		fs, network, scoped uint64
	}{
		{2, unix.LANDLOCK_ACCESS_FS_REFER, 0, 0},
		{3, unix.LANDLOCK_ACCESS_FS_TRUNCATE, 0, 0},
		{4, 0, unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP, 0},
		{5, unix.LANDLOCK_ACCESS_FS_IOCTL_DEV, 0, 0},
		{6, 0, 0, unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL},
		{9, landlockAccessFSResolveUnix, 0, 0},
		{10, 0, landlockAccessNetBindUDP | landlockAccessNetConnectSendUDP, 0},
	} {
		if abi >= added.abi {
			attr.Access_fs |= added.fs
			attr.Access_net |= added.network
			attr.Scoped |= added.scoped
		}
	}
	return attr
}

// The rights the rules grant, and the ones a rule on a file (not a
// directory) may carry at all.
const (
	fsReadFile = unix.LANDLOCK_ACCESS_FS_READ_FILE
	fsRead     = fsReadFile | unix.LANDLOCK_ACCESS_FS_READ_DIR
	fsReadExec = fsRead | unix.LANDLOCK_ACCESS_FS_EXECUTE
	fileAccess = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// pathKind is what a rule's path must be.
type pathKind int

const (
	anyKind pathKind = iota
	mustBeDir
	mustBeFile
)

// sandboxRule grants access beneath (or on) one path.
type sandboxRule struct {
	path     string
	access   uint64
	optional bool // a path that does not exist is skipped
	glob     bool // path is a pattern: each match gets the rule
	kind     pathKind
}

// sandboxSystemRules are what ffprobe and ffmpeg open of the system on every
// run, as traced on Alpine 3.24 (ffmpeg 8.1.3) and Ubuntu 24.04 (6.1.1). The
// rest of what they try at start-up (/proc/self/status, /proc/mounts,
// /proc/filesystems, /sys/devices/system/*, /etc/gnutls/config: library
// probes) is denied, and they cope: renders are byte for byte the same.
// fontconfig, which drawtext uses when no font file is found, falls back to
// its built-in configuration without /etc/fonts and still finds the fonts
// beneath /usr. ffmpeg itself never opened /dev/null in the traces (the bot
// opens the tool's stdin before the sandbox), but reading it is harmless and
// ordinary code needs it: a shell gives a background job /dev/null as stdin.
var sandboxSystemRules = []sandboxRule{
	{path: "/usr", access: fsReadExec, optional: true},                            // the tools, their libraries, fonts and fontconfig's own files
	{path: "/lib", access: fsReadExec, optional: true},                            // musl's loader and libc; a link into /usr on merged-/usr systems
	{path: "/lib64", access: fsReadExec, optional: true},                          // glibc's loader path on x86-64
	{path: "/etc/ld.so.cache", access: fsReadFile, optional: true},                // glibc's loader reads it on every start (/usr/local/lib is found only through it)
	{path: "/etc/ld-musl-*.path", access: fsReadFile, optional: true, glob: true}, // musl's loader reads its library path on every start, where one is set
	{path: "/dev/null", access: fsReadFile, optional: true},
	{path: "/dev/urandom", access: fsReadFile, optional: true}, // drawtext seeds its PRNG through av_get_random_seed on every render
}

// sandboxTestRules are extra rules for the tests' stand-in tools. Production
// code never sets them (sandbox_linux_test.go does).
var sandboxTestRules []sandboxRule

// sandboxRules is the whole policy for a spec: the system rules, the input
// directory and font read only, and the tool read and execute. No rule grants
// writing, and nothing grants /proc, /sys or the rest of /etc.
func sandboxRules(spec sandboxSpec) []sandboxRule {
	rules := append(slices.Clone(sandboxSystemRules), sandboxRule{path: spec.InputDir, access: fsRead, kind: mustBeDir})
	if spec.FontFile != "" {
		rules = append(rules, sandboxRule{path: spec.FontFile, access: fsReadFile, kind: mustBeFile})
	}
	if spec.Tool != "" {
		rules = append(rules, sandboxRule{path: spec.Tool, access: fsReadExec, kind: mustBeFile})
	}
	return append(rules, sandboxTestRules...)
}

// restrictSelf builds a ruleset handling every right of abi, adds the rules
// and restricts this thread. The tool exec'd from it inherits the domain.
func restrictSelf(abi int, rules []sandboxRule) error {
	attr := landlockAccess(abi)
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0) // #nosec G103 -- the kernel's ruleset struct
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	ruleset := int(fd)
	defer func() { _ = unix.Close(ruleset) }()
	for _, r := range rules {
		paths := []string{r.path}
		if r.glob {
			paths, _ = filepath.Glob(r.path) // the patterns are constants, so never malformed
		}
		for _, p := range paths {
			if err := addPathRule(ruleset, p, r, attr.Access_fs); err != nil {
				return err
			}
		}
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}

// addPathRule adds one path-beneath rule. A file gets only the file rights
// of r.access; the kernel refuses directory rights on a file.
func addPathRule(ruleset int, path string, r sandboxRule, handled uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		if r.optional && errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	dir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	switch {
	case r.kind == mustBeDir && !dir:
		return fmt.Errorf("%s is not a directory", path)
	case r.kind == mustBeFile && st.Mode&unix.S_IFMT != unix.S_IFREG:
		return fmt.Errorf("%s is not a regular file", path)
	}
	access := r.access & handled
	if !dir {
		access &= fileAccess
	}
	rule := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)} // #nosec G115 -- a file descriptor fits
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 { // #nosec G103 -- the kernel's rule struct
		return fmt.Errorf("landlock_add_rule %s: %w", path, errno)
	}
	return nil
}

// sandboxProbeName is the file a check tries to create.
func sandboxProbeName() string {
	return ".media-sandbox-check-" + strconv.Itoa(os.Getpid())
}

// checkSandboxed is the check run's test of the sandbox it is in: inside
// (in the input directory) reads, outside does not, no file can be created in
// either directory, and from ABI 4 no TCP socket can bind. It returns every
// failure at once.
func checkSandboxed(inputDir, inside, outside string, abi int) error {
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
	if len(failed) > 0 {
		return errors.New("sandbox check failed: " + strings.Join(failed, "; "))
	}
	return nil
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
	spec, err := newSandboxSpec(dir, "", "")
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
