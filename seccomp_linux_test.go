//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/binary"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// evalSeccomp runs a classic BPF seccomp program on one system call, as the
// kernel does: the arch, the call number and its first two arguments.
func evalSeccomp(t *testing.T, prog []unix.SockFilter, arch, nr uint32, args ...uint64) uint32 {
	t.Helper()
	var data [64]byte // struct seccomp_data
	binary.LittleEndian.PutUint32(data[seccompNr:], nr)
	binary.LittleEndian.PutUint32(data[seccompArchWord:], arch)
	for i, arg := range args {
		binary.LittleEndian.PutUint64(data[seccompArg0Low+8*i:], arg)
	}
	jump := func(taken bool, in unix.SockFilter) int {
		if taken {
			return int(in.Jt)
		}
		return int(in.Jf)
	}
	var a uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case bpfLoad:
			a = binary.LittleEndian.Uint32(data[in.K:])
		case bpfJumpEq:
			pc += jump(a == in.K, in)
		case bpfJumpGE:
			pc += jump(a >= in.K, in)
		case bpfJumpSet:
			pc += jump(a&in.K != 0, in)
		case bpfAnd:
			a &= in.K
		case bpfReturn:
			return in.K
		default:
			t.Fatalf("unknown instruction %#x at %d", in.Code, pc)
		}
	}
	t.Fatal("the filter ran off its end")
	return 0
}

// The allowlist, pinned: a call added or dropped must be added or dropped
// here too, on purpose (Ruling 18 item 1).
var (
	wantSeccompAllowed = []string{
		"brk", "clock_getres", "clock_gettime", "clock_nanosleep", "close", "dup", "dup3", "epoll_create1", "epoll_ctl",
		"epoll_pwait", "eventfd2", "execve", "exit", "exit_group", "faccessat", "faccessat2", "fstat", "fstatfs",
		"getcwd", "getdents64", "getegid", "geteuid", "getgid", "getpid", "getppid", "getrandom", "getrusage", "gettid",
		"gettimeofday", "getuid", "lseek", "madvise", "membarrier", "mlock", "mmap", "mprotect", "mremap", "munmap",
		"nanosleep", "newfstatat", "openat", "pread64", "read", "readlinkat", "readv", "restart_syscall", "rseq",
		"rt_sigaction", "rt_sigprocmask", "rt_sigreturn", "sched_yield", "set_robust_list", "set_tid_address",
		"sigaltstack", "statfs", "statx", "write", "writev",
	}
	wantSeccompChecked = []string{"clone", "fcntl", "futex", "ioctl", "kill", "prctl", "prlimit64",
		"sched_getaffinity", "tgkill", "tkill"}
	wantSeccompAMD64Extra = []string{"access", "arch_prctl", "dup2", "lstat", "open", "readlink", "stat", "time"}
)

func TestSeccompAllowlist(t *testing.T) {
	var allowed []string
	for _, group := range seccompAllowed {
		allowed = append(allowed, group...)
	}
	slices.Sort(allowed)
	assert.Equal(t, wantSeccompAllowed, allowed)
	checked := append(append(slices.Clone(seccompSelfTargets), seccompSelfOrZero...), "clone", "fcntl", "futex", "prctl", "ioctl")
	slices.Sort(checked)
	assert.Equal(t, wantSeccompChecked, checked)
	extra := slices.Clone(seccompAMD64.extra)
	slices.Sort(extra)
	assert.Equal(t, wantSeccompAMD64Extra, extra)
	assert.Empty(t, seccompARM64.extra, "AArch64 has only the *at calls")
	for name := range seccompNumbers {
		assert.True(t, slices.Contains(allowed, name) || slices.Contains(checked, name) || slices.Contains(extra, name),
			"%s is numbered but not in the filter", name)
	}
}

// The filter's answer to each kind of call, on both architectures: x86-64's
// program and AArch64's, built here from the same names (Ruling 18 item 1).
func TestSeccompFilter(t *testing.T) {
	for _, tc := range []struct {
		name string
		arch seccompArch
	}{{"amd64", seccompAMD64}, {"arm64", seccompARM64}} {
		t.Run(tc.name, func(t *testing.T) { testSeccompFilter(t, tc.arch) })
	}
}

func testSeccompFilter(t *testing.T, arch seccompArch) {
	const self = 4242
	prog, err := seccompProgram(arch, self)
	require.NoError(t, err)
	require.Less(t, len(prog), 4096, "within the kernel's BPF_MAXINSNS")

	// rt_sigqueueinfo and rt_tgsigqueueinfo, which queue a signal with
	// siginfo of the caller's choosing, were never traced and are not in the
	// filter at all (Ruling 21 item 1): ENOSYS, whatever the target. The
	// sweep below covers them too; here they are by name.
	queueinfo := map[uint32][]uint32{unix.AUDIT_ARCH_X86_64: {129, 297}, unix.AUDIT_ARCH_AARCH64: {138, 240}}[arch.audit]
	require.Len(t, queueinfo, 2)
	for _, n := range queueinfo {
		for _, target := range []uint64{self, 1} {
			assert.Equal(t, seccompENOSYS, evalSeccomp(t, prog, arch.audit, n, target, 0, 0), "call %d to %d", n, target)
		}
	}
	nr := func(name string) uint32 {
		n, err := arch.nr(name)
		require.NoError(t, err)
		return n
	}
	eval := func(name string, args ...uint64) uint32 {
		return evalSeccomp(t, prog, arch.audit, nr(name), args...)
	}

	// Every number up to 1023, and far beyond: the listed calls are allowed,
	// the checked ones are tried below, and every other gets ENOSYS.
	plain, checked := map[uint32]bool{}, map[uint32]bool{}
	for _, group := range append(slices.Clone(seccompAllowed), arch.extra) {
		for _, name := range group {
			plain[nr(name)] = true
		}
	}
	for _, name := range wantSeccompChecked {
		checked[nr(name)] = true
	}
	for n := uint32(0); n < 1024; n++ {
		switch {
		case checked[n]:
		case plain[n]:
			assert.Equal(t, uint32(seccompAllow), evalSeccomp(t, prog, arch.audit, n), "call %d is allowed", n)
		default:
			assert.Equal(t, uint32(seccompENOSYS), evalSeccomp(t, prog, arch.audit, n), "call %d gets ENOSYS", n)
		}
	}
	assert.Equal(t, uint32(seccompENOSYS), evalSeccomp(t, prog, arch.audit, 0x3ffffff0), "an unknown, high call gets ENOSYS")

	thread := uint64(unix.CLONE_VM | unix.CLONE_FS | unix.CLONE_FILES | unix.CLONE_SIGHAND | unix.CLONE_THREAD |
		unix.CLONE_SYSVSEM | unix.CLONE_SETTLS | unix.CLONE_PARENT_SETTID | unix.CLONE_CHILD_CLEARTID) // glibc's and Go's
	for _, c := range []struct {
		what string
		call string
		args []uint64
		want uint32
	}{
		{"kill itself", "kill", []uint64{self}, seccompAllow},
		{"kill itself, high bits set", "kill", []uint64{self | 1<<40}, seccompAllow},
		{"kill another process", "kill", []uint64{self + 1}, seccompEPERM},
		{"kill its process group", "kill", []uint64{0}, seccompEPERM},
		{"kill everything", "kill", []uint64{^uint64(0)}, seccompEPERM},
		{"kill a group by -pgid", "kill", []uint64{uint64(^uint32(self - 1))}, seccompEPERM},
		{"tgkill one of its threads", "tgkill", []uint64{self}, seccompAllow},
		{"tgkill another process", "tgkill", []uint64{1}, seccompEPERM},
		{"tkill its main thread", "tkill", []uint64{self}, seccompAllow},
		{"tkill another thread", "tkill", []uint64{self + 3}, seccompEPERM},
		{"prlimit64 of itself as 0", "prlimit64", []uint64{0}, seccompAllow},
		{"prlimit64 of itself by PID", "prlimit64", []uint64{self}, seccompAllow},
		{"prlimit64 of the bot", "prlimit64", []uint64{self - 1}, seccompEPERM},
		{"prlimit64 of init", "prlimit64", []uint64{1}, seccompEPERM},
		{"sched_getaffinity of itself", "sched_getaffinity", []uint64{0}, seccompAllow},
		{"sched_getaffinity of another", "sched_getaffinity", []uint64{1}, seccompEPERM},
		{"a thread", "clone", []uint64{thread}, seccompAllow},
		{"fork through clone", "clone", []uint64{uint64(unix.SIGCHLD)}, seccompEPERM},
		{"vfork through clone", "clone", []uint64{uint64(unix.CLONE_VM | unix.CLONE_VFORK | unix.SIGCHLD)}, seccompEPERM},
		{"a thread in a new user namespace", "clone", []uint64{thread | unix.CLONE_NEWUSER}, seccompEPERM},
		{"a thread in a new network namespace", "clone", []uint64{thread | unix.CLONE_NEWNET}, seccompEPERM},
		{"a child in a new user namespace", "clone", []uint64{uint64(unix.CLONE_NEWUSER | unix.SIGCHLD)}, seccompEPERM},
		{"F_GETFD", "fcntl", []uint64{1, unix.F_GETFD}, seccompAllow},
		{"F_SETFD", "fcntl", []uint64{1, unix.F_SETFD}, seccompAllow},
		{"F_GETFL", "fcntl", []uint64{1, unix.F_GETFL}, seccompAllow},
		{"F_SETFL", "fcntl", []uint64{1, unix.F_SETFL}, seccompAllow},
		{"F_DUPFD", "fcntl", []uint64{1, unix.F_DUPFD}, seccompAllow},
		{"F_DUPFD_CLOEXEC", "fcntl", []uint64{1, unix.F_DUPFD_CLOEXEC}, seccompAllow},
		{"F_GETLK", "fcntl", []uint64{1, unix.F_GETLK}, seccompAllow},
		{"F_GETFD, high bits set", "fcntl", []uint64{1, 1<<40 | unix.F_GETFD}, seccompAllow},
		{"F_SETOWN", "fcntl", []uint64{1, unix.F_SETOWN}, seccompEPERM},
		{"F_SETOWN_EX", "fcntl", []uint64{1, unix.F_SETOWN_EX}, seccompEPERM},
		{"F_SETSIG", "fcntl", []uint64{1, unix.F_SETSIG}, seccompEPERM},
		{"F_SETLEASE", "fcntl", []uint64{1, unix.F_SETLEASE}, seccompEPERM},
		{"F_NOTIFY", "fcntl", []uint64{1, unix.F_NOTIFY}, seccompEPERM},
		{"F_SETLK", "fcntl", []uint64{1, unix.F_SETLK}, seccompEPERM},
		{"F_OFD_SETLK", "fcntl", []uint64{1, unix.F_OFD_SETLK}, seccompEPERM},
		// futex(2): only the non-PI operations, whatever FUTEX_PRIVATE_FLAG
		// (128) and FUTEX_CLOCK_REALTIME (256) say (Ruling 19 item 6).
		{"FUTEX_WAIT", "futex", []uint64{0, 0}, seccompAllow},
		{"FUTEX_WAKE", "futex", []uint64{0, 1}, seccompAllow},
		{"FUTEX_REQUEUE", "futex", []uint64{0, 3}, seccompAllow},
		{"FUTEX_CMP_REQUEUE", "futex", []uint64{0, 4}, seccompAllow},
		{"FUTEX_WAKE_OP", "futex", []uint64{0, 5}, seccompAllow},
		{"FUTEX_WAIT_BITSET", "futex", []uint64{0, 9}, seccompAllow},
		{"FUTEX_WAKE_BITSET", "futex", []uint64{0, 10}, seccompAllow},
		{"FUTEX_WAIT_PRIVATE (Go)", "futex", []uint64{0, 128}, seccompAllow},
		{"FUTEX_WAKE_PRIVATE (Go)", "futex", []uint64{0, 129}, seccompAllow},
		{"FUTEX_WAIT_BITSET_PRIVATE|FUTEX_CLOCK_REALTIME (glibc)", "futex", []uint64{0, 9 | 128 | 256}, seccompAllow},
		{"FUTEX_WAKE_PRIVATE, high bits set", "futex", []uint64{0, 1<<40 | 129}, seccompAllow},
		{"FUTEX_FD", "futex", []uint64{0, 2}, seccompEPERM},
		{"FUTEX_LOCK_PI", "futex", []uint64{0, 6}, seccompEPERM},
		{"FUTEX_LOCK_PI_PRIVATE", "futex", []uint64{0, 6 | 128}, seccompEPERM},
		{"FUTEX_UNLOCK_PI", "futex", []uint64{0, 7}, seccompEPERM},
		{"FUTEX_TRYLOCK_PI", "futex", []uint64{0, 8}, seccompEPERM},
		{"FUTEX_WAIT_REQUEUE_PI", "futex", []uint64{0, 11}, seccompEPERM},
		{"FUTEX_CMP_REQUEUE_PI", "futex", []uint64{0, 12 | 128}, seccompEPERM},
		{"FUTEX_LOCK_PI2", "futex", []uint64{0, 13 | 256}, seccompEPERM},
		{"an unknown futex op", "futex", []uint64{0, 14}, seccompEPERM},
		{"PR_SET_NAME", "prctl", []uint64{unix.PR_SET_NAME}, seccompAllow},
		{"PR_GET_NAME", "prctl", []uint64{unix.PR_GET_NAME}, seccompAllow},
		{"PR_CAPBSET_READ", "prctl", []uint64{unix.PR_CAPBSET_READ}, seccompAllow},
		{"PR_SET_VMA", "prctl", []uint64{unix.PR_SET_VMA}, seccompAllow},
		{"PR_SET_PDEATHSIG", "prctl", []uint64{unix.PR_SET_PDEATHSIG}, seccompEPERM},
		{"PR_SET_DUMPABLE", "prctl", []uint64{unix.PR_SET_DUMPABLE}, seccompEPERM},
		{"PR_SET_PTRACER", "prctl", []uint64{unix.PR_SET_PTRACER}, seccompEPERM},
		{"PR_SET_MM", "prctl", []uint64{unix.PR_SET_MM}, seccompEPERM},
		{"TCGETS", "ioctl", []uint64{2, unix.TCGETS}, seccompAllow},
		{"TIOCGWINSZ", "ioctl", []uint64{2, unix.TIOCGWINSZ}, seccompAllow},
		{"TIOCSTI", "ioctl", []uint64{0, unix.TIOCSTI}, seccompEPERM},
		{"TIOCLINUX", "ioctl", []uint64{0, unix.TIOCLINUX}, seccompEPERM},
		{"FIONREAD", "ioctl", []uint64{0, unix.TIOCINQ}, seccompEPERM},
	} {
		assert.Equal(t, c.want, eval(c.call, c.args...), c.what)
	}

	assert.Equal(t, uint32(seccompKill), evalSeccomp(t, prog, 0x40000003 /* AUDIT_ARCH_I386 */, 0),
		"another architecture's call is killed")
	assert.Equal(t, uint32(seccompKill), evalSeccomp(t, prog, 0x40000028 /* AUDIT_ARCH_ARM */, 0),
		"another architecture's call is killed")
	if arch.x32Bit != 0 {
		assert.Equal(t, uint32(seccompKill), evalSeccomp(t, prog, arch.audit, nr("read")|arch.x32Bit), "an x32 call is killed")
	}
	other, err := seccompProgram(arch, 99)
	require.NoError(t, err)
	assert.Equal(t, uint32(seccompEPERM), evalSeccomp(t, other, arch.audit, nr("kill"), self),
		"the PID is the filter's own: another tool's is another PID")
}
