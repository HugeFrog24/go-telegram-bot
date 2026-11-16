//go:build linux && (amd64 || arm64)

package main

import (
	"fmt"
	"math"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The media sandbox's seccomp filter (Ruling 18 item 1, which replaced Ruling
// 16's deny list). Landlock decides which files a tool may open; this filter
// decides which system calls it may make at all. It is an allowlist, made by
// tracing ffprobe and ffmpeg (strace -f) on Alpine (musl) and Ubuntu (glibc)
// across Ruling 18's fixture matrix: H.264, HEVC 10-bit, VP8, VP9, VP9 alpha
// through libvpx, AV1 through dav1d and GIF, in MP4, MOV, MKV and WebM;
// probe and render, with a font and with the fontconfig fallback; -version;
// a refused format, a timeout and the output bound. To that come the libc
// and Go runtime baseline and the helper's own calls after the filter.
//
// Any other call fails with ENOSYS, as a call the kernel lacks does, so libc
// falls back where it can; an allowed call whose arguments fail its check
// fails with EPERM. AArch64's list is x86-64's by name, with the *at calls
// in place of the old path calls. Nothing here can run it, so only the
// tests' BPF evaluator checks it; a wrong list fails closed in the gate's
// self-test.
//
// The helper installs the filter after no_new_privs and Landlock, on the
// thread that then execs the tool, which keeps it. Classic BPF, built here
// with x/sys/unix: no libseccomp.

// seccompAllowed are the calls allowed whatever their arguments, by group.
var seccompAllowed = [][]string{
	// Files, through descriptors and paths. Landlock decides which files
	// open: a write, a create or an exec outside its rules fails there.
	// Reading metadata by path (stat, access, readlink, statfs) is no
	// Landlock right: the sandbox's one gap (sandboxGaps). The tool writes
	// only to the pipes it inherits. dup3 (and dup2 on x86-64) is the libc
	// baseline, which gives no more than dup and F_DUPFD do: a shell needs
	// it, and the tests' stand-in tools are shell scripts too.
	{"openat", "read", "pread64", "readv", "write", "writev", "lseek", "close", "dup", "dup3",
		"fstat", "newfstatat", "statx", "faccessat", "faccessat2", "readlinkat", "getdents64", "getcwd",
		"statfs", "fstatfs"},
	// Memory: the tool's own address space, which RLIMIT_AS caps; mlock is
	// capped by RLIMIT_MEMLOCK.
	{"brk", "mmap", "munmap", "mprotect", "mremap", "madvise", "mlock", "membarrier"},
	// Threads, which clone (checked below) starts and RLIMIT_NPROC counts:
	// futexes, libc's thread setup, yielding and ending a thread.
	{"futex", "set_tid_address", "set_robust_list", "rseq", "gettid", "sched_yield", "exit"},
	// Signals that name no target: handlers, masks, the alternate stack,
	// the return from a handler and the restart of the call it interrupted.
	{"rt_sigaction", "rt_sigprocmask", "rt_sigreturn", "sigaltstack", "restart_syscall"},
	// Time and sleep, which the vDSO usually answers without a call.
	{"clock_gettime", "clock_getres", "gettimeofday", "nanosleep", "clock_nanosleep"},
	// The process's own identity, randomness and resource usage; the group
	// IDs, like the user IDs, are the libc baseline a shell needs.
	{"getpid", "getppid", "getuid", "geteuid", "getgid", "getegid", "getrandom", "getrusage"},
	// The Go runtime's poller: the helper's check run is Go code under this
	// filter, and so are the tests' stand-in tools.
	{"epoll_create1", "epoll_ctl", "epoll_pwait", "eventfd2"},
	// The helper's last calls: the exec of the tool, which Landlock lets run
	// only the tool and its ELF interpreter, and the exit after the check
	// run's report. Its prlimit64 calls are checked below.
	{"execve", "exit_group"},
}

// seccompSelfTargets may only name the tool itself: their first argument,
// the process or thread group a signal goes to, must be its PID, which the
// helper knows because it execs in place. raise(3) and abort(3) (tgkill or
// tkill of the tool, kill of itself) and signals between its own threads
// (tgkill) keep working. 0, which for kill(2) is the tool's process group,
// is refused: nothing needs it.
var seccompSelfTargets = []string{"kill", "tkill", "tgkill", "rt_sigqueueinfo", "rt_tgsigqueueinfo"}

// seccompSelfOrZero may name the tool by its PID or by 0, "this process": no
// other process's limits or CPU affinity can be read or changed.
var seccompSelfOrZero = []string{"prlimit64", "sched_getaffinity"}

// seccompFcntlCommands are the fcntl(2) commands allowed: duplicating a
// descriptor, its flags and status flags, and reading a lock. Taking
// ownership and choosing a signal (F_SETOWN, F_SETOWN_EX, F_SETSIG), which
// would signal another process below Landlock ABI 6, leases, change
// notification and setting locks are refused.
var seccompFcntlCommands = []uint32{unix.F_DUPFD, unix.F_DUPFD_CLOEXEC, unix.F_GETFD, unix.F_SETFD,
	unix.F_GETFL, unix.F_SETFL, unix.F_GETLK}

// seccompPrctlOptions are the prctl(2) options the tools are seen to use:
// naming a thread (ffmpeg's workers), reading the capability bounding set
// (libcap, on Ubuntu) and naming a mapping (the Go runtime). Every other
// option, PR_SET_PDEATHSIG among them, is refused.
var seccompPrctlOptions = []uint32{unix.PR_SET_NAME, unix.PR_GET_NAME, unix.PR_CAPBSET_READ, unix.PR_SET_VMA}

// seccompIoctlRequests are the only ioctl(2) requests allowed: isatty(3) on
// the tool's stderr (TCGETS for glibc, TIOCGWINSZ for musl). TIOCSTI and
// TIOCLINUX are refused with every other request, a device's among them.
var seccompIoctlRequests = []uint32{unix.TCGETS, unix.TIOCGWINSZ}

// cloneNamespaces are the namespace flags clone(2) refuses here.
const cloneNamespaces = unix.CLONE_NEWNS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC |
	unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET

// seccompArch is an architecture the filter is written for.
type seccompArch struct {
	audit  uint32                    // its AUDIT_ARCH_* value; any other architecture's call is killed
	x32Bit uint32                    // the bit of a second ABI with the same audit value (x32), killed; 0 for none
	number func(seccompNumber) int32 // its column of seccompNumbers
	extra  []string                  // calls only this architecture has, allowed like seccompAllowed
}

// seccompNumber is a call's number on each architecture, -1 where it has none.
type seccompNumber = struct{ amd64, arm64 int32 }

var (
	seccompAMD64 = seccompArch{
		audit:  unix.AUDIT_ARCH_X86_64,
		x32Bit: 0x40000000, // __X32_SYSCALL_BIT
		number: func(n seccompNumber) int32 { return n.amd64 },
		// The old path calls musl and glibc still make here (open, stat,
		// lstat, access, readlink, and dup2 as above), the thread pointer's
		// setup (arch_prctl) and time(2)'s fallback from the vDSO.
		extra: []string{"open", "stat", "lstat", "access", "readlink", "dup2", "arch_prctl", "time"},
	}
	// AArch64 has only the *at calls, which seccompAllowed names.
	seccompARM64 = seccompArch{
		audit:  unix.AUDIT_ARCH_AARCH64,
		number: func(n seccompNumber) int32 { return n.arm64 },
	}
)

// nr is the number of the call named name on a.
func (a seccompArch) nr(name string) (uint32, error) {
	n, ok := seccompNumbers[name]
	if !ok || a.number(n) < 0 {
		return 0, fmt.Errorf("seccomp: no system call %q here", name)
	}
	return uint32(a.number(n)), nil // #nosec G115 -- not negative
}

// The parts of struct seccomp_data the filter reads (linux/seccomp.h). Both
// supported architectures are little-endian, so an argument's low 32 bits
// come first. The kernel itself reads only those of every argument checked
// here: PIDs, clone(2)'s legacy flags, and fcntl(2), prctl(2) and ioctl(2)
// commands are 32 bits wide.
const (
	seccompNr       = 0
	seccompArchWord = 4
	seccompArg0Low  = 16
	seccompArg1Low  = 24
)

// Classic BPF instructions the filter is made of.
const (
	bpfLoad    = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
	bpfJumpEq  = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
	bpfJumpGE  = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
	bpfJumpSet = unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K
	bpfReturn  = unix.BPF_RET | unix.BPF_K
)

// The filter's answers.
const (
	seccompAllow  = unix.SECCOMP_RET_ALLOW
	seccompKill   = unix.SECCOMP_RET_KILL_PROCESS
	seccompEPERM  = unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
	seccompENOSYS = unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)
)

// bpfAsm assembles a classic BPF program whose conditional jumps name labels
// ("" for the next instruction), and checks that each fits classic BPF's
// 8-bit jump offsets, which only jump forward.
type bpfAsm struct {
	prog   []unix.SockFilter
	labels map[string]int
	jumps  []bpfJump
}

type bpfJump struct {
	at     int
	jt, jf string
}

func (a *bpfAsm) stmt(code uint16, k uint32) {
	a.prog = append(a.prog, unix.SockFilter{Code: code, K: k})
}

func (a *bpfAsm) jump(code uint16, k uint32, jt, jf string) {
	a.jumps = append(a.jumps, bpfJump{len(a.prog), jt, jf})
	a.stmt(code, k)
}

func (a *bpfAsm) label(name string) {
	if a.labels == nil {
		a.labels = map[string]int{}
	}
	a.labels[name] = len(a.prog)
}

func (a *bpfAsm) assemble() ([]unix.SockFilter, error) {
	for _, j := range a.jumps {
		for _, to := range []struct {
			label  string
			offset *uint8
		}{{j.jt, &a.prog[j.at].Jt}, {j.jf, &a.prog[j.at].Jf}} {
			if to.label == "" {
				continue
			}
			target, ok := a.labels[to.label]
			if !ok {
				return nil, fmt.Errorf("seccomp: no label %q", to.label)
			}
			d := target - j.at - 1
			if d < 0 || d > math.MaxUint8 {
				return nil, fmt.Errorf("seccomp: the jump at %d to %q is %d long", j.at, to.label, d)
			}
			*to.offset = uint8(d) // #nosec G115 -- checked above
		}
	}
	return a.prog, nil
}

// seccompProgram is the filter for a tool that runs as pid on arch: the
// architecture check, every allowed call jumping to ALLOW, the checked calls
// jumping to their checks, ENOSYS for the rest, then the checks.
func seccompProgram(arch seccompArch, pid uint32) ([]unix.SockFilter, error) {
	var a bpfAsm
	// Another architecture's calls (a 32-bit entry, say) have other numbers: kill.
	a.stmt(bpfLoad, seccompArchWord)
	a.jump(bpfJumpEq, arch.audit, "", "kill")
	a.stmt(bpfLoad, seccompNr)
	if arch.x32Bit != 0 {
		// x32 calls share x86-64's architecture value and carry this bit: kill.
		a.jump(bpfJumpGE, arch.x32Bit, "kill", "")
	}
	to := func(label string, names ...string) error {
		for _, name := range names {
			nr, err := arch.nr(name)
			if err != nil {
				return err
			}
			a.jump(bpfJumpEq, nr, label, "")
		}
		return nil
	}
	for _, group := range append(seccompAllowed, arch.extra) {
		if err := to("allow", group...); err != nil {
			return nil, err
		}
	}
	for _, c := range []struct {
		label string
		names []string
	}{
		{"self", seccompSelfTargets}, {"self-or-zero", seccompSelfOrZero},
		{"clone", []string{"clone"}}, {"fcntl", []string{"fcntl"}}, {"prctl", []string{"prctl"}}, {"ioctl", []string{"ioctl"}},
	} {
		if err := to(c.label, c.names...); err != nil {
			return nil, err
		}
	}
	a.stmt(bpfReturn, seccompENOSYS)

	a.label("self-or-zero")
	a.stmt(bpfLoad, seccompArg0Low)
	a.jump(bpfJumpEq, 0, "allow", "self-pid")
	a.label("self")
	a.stmt(bpfLoad, seccompArg0Low)
	a.label("self-pid")
	a.jump(bpfJumpEq, pid, "allow", "eperm")

	// clone makes threads only: CLONE_THREAD set, no namespace flag. A fork,
	// vfork or posix_spawn has no CLONE_THREAD. clone3 passes its flags in
	// memory the filter cannot read, so it is not allowed: its ENOSYS makes
	// libc fall back to clone.
	a.label("clone")
	a.stmt(bpfLoad, seccompArg0Low)
	a.jump(bpfJumpSet, unix.CLONE_THREAD, "", "eperm")
	a.jump(bpfJumpSet, cloneNamespaces, "eperm", "allow")

	for _, c := range []struct {
		label  string
		arg    uint32
		values []uint32
	}{
		{"fcntl", seccompArg1Low, seccompFcntlCommands},
		{"prctl", seccompArg0Low, seccompPrctlOptions},
		{"ioctl", seccompArg1Low, seccompIoctlRequests},
	} {
		a.label(c.label)
		a.stmt(bpfLoad, c.arg)
		for _, v := range c.values {
			a.jump(bpfJumpEq, v, "allow", "")
		}
		a.stmt(bpfReturn, seccompEPERM)
	}

	a.label("allow")
	a.stmt(bpfReturn, seccompAllow)
	a.label("eperm")
	a.stmt(bpfReturn, seccompEPERM)
	a.label("kill")
	a.stmt(bpfReturn, seccompKill)
	return a.assemble()
}

// seccompAvailable says why the kernel cannot run the filter, or returns
// nil: no seccomp filters, or none that can kill the whole process (before
// Linux 4.14), or a seccomp(2) blocked by a container's own profile.
func seccompAvailable() error {
	action := uint32(seccompKill)
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_GET_ACTION_AVAIL, 0,
		uintptr(unsafe.Pointer(&action))); errno != 0 { // #nosec G103 -- seccomp(2) of a u32
		return fmt.Errorf("%w: seccomp: %w", errNoSeccomp, errno)
	}
	return nil
}

// installSeccomp installs the filter for pid on the calling thread. With
// no_new_privs set, execve keeps it.
func installSeccomp(pid int) error {
	filter, err := seccompProgram(seccompHost, uint32(pid)) // #nosec G115 -- a PID fits
	if err != nil {
		return err
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]} // #nosec G115 -- under 200 instructions
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0,
		uintptr(unsafe.Pointer(&prog))); errno != 0 { // #nosec G103 -- seccomp(2) of the filter
		return fmt.Errorf("seccomp: %w", errno)
	}
	return nil
}
