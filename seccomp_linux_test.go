//go:build linux && (amd64 || arm64)

package main

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// evalSeccomp runs a classic BPF seccomp program on one system call, as the
// kernel does: the arch, the call number and its first argument.
func evalSeccomp(t *testing.T, prog []unix.SockFilter, arch, nr uint32, arg0 uint64) uint32 {
	t.Helper()
	var data [64]byte // struct seccomp_data
	binary.LittleEndian.PutUint32(data[seccompNr:], nr)
	binary.LittleEndian.PutUint32(data[seccompArchWord:], arch)
	binary.LittleEndian.PutUint64(data[seccompArg0Low:], arg0)
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
		case bpfReturn:
			return in.K
		default:
			t.Fatalf("unknown instruction %#x at %d", in.Code, pc)
		}
	}
	t.Fatal("the filter ran off its end")
	return 0
}

// The filter's answer to each kind of call (Ruling 16 item 1).
func TestSeccompFilter(t *testing.T) {
	const self = 4242
	prog := seccompFilter(self)
	require.Less(t, len(prog), 4096, "within the kernel's BPF_MAXINSNS")
	thread := uint64(unix.CLONE_VM | unix.CLONE_FS | unix.CLONE_FILES | unix.CLONE_SIGHAND | unix.CLONE_THREAD |
		unix.CLONE_SYSVSEM | unix.CLONE_SETTLS | unix.CLONE_PARENT_SETTID | unix.CLONE_CHILD_CLEARTID) // glibc's and Go's
	for _, tc := range []struct {
		name string
		nr   uint32
		arg0 uint64
		want uint32
	}{
		{"read", unix.SYS_READ, 0, seccompAllow},
		{"mmap", unix.SYS_MMAP, 0, seccompAllow},
		{"execve", unix.SYS_EXECVE, 0, seccompAllow},
		{"futex", unix.SYS_FUTEX, 0, seccompAllow},
		{"socket", unix.SYS_SOCKET, unix.AF_UNIX, seccompEPERM},
		{"kill itself", unix.SYS_KILL, self, seccompAllow},
		{"kill itself, high bits set", unix.SYS_KILL, self | 1<<40, seccompAllow},
		{"kill another process", unix.SYS_KILL, self + 1, seccompEPERM},
		{"kill its process group", unix.SYS_KILL, 0, seccompEPERM},
		{"kill everything", unix.SYS_KILL, ^uint64(0), seccompEPERM},
		{"kill a group by -pgid", unix.SYS_KILL, uint64(^uint32(self - 1)), seccompEPERM},
		{"tgkill one of its threads", unix.SYS_TGKILL, self, seccompAllow},
		{"tgkill another process", unix.SYS_TGKILL, 1, seccompEPERM},
		{"tkill its main thread", unix.SYS_TKILL, self, seccompAllow},
		{"tkill another thread", unix.SYS_TKILL, self + 3, seccompEPERM},
		{"rt_sigqueueinfo another", unix.SYS_RT_SIGQUEUEINFO, 1, seccompEPERM},
		{"rt_tgsigqueueinfo itself", unix.SYS_RT_TGSIGQUEUEINFO, self, seccompAllow},
		{"pidfd_open itself", unix.SYS_PIDFD_OPEN, self, seccompAllow},
		{"pidfd_open another", unix.SYS_PIDFD_OPEN, 1, seccompEPERM},
		{"a thread", unix.SYS_CLONE, thread, seccompAllow},
		{"fork through clone", unix.SYS_CLONE, uint64(unix.SIGCHLD), seccompEPERM},
		{"vfork through clone", unix.SYS_CLONE, uint64(unix.CLONE_VM | unix.CLONE_VFORK | unix.SIGCHLD), seccompEPERM},
		{"a thread in a new user namespace", unix.SYS_CLONE, thread | unix.CLONE_NEWUSER, seccompEPERM},
		{"a thread in a new network namespace", unix.SYS_CLONE, thread | unix.CLONE_NEWNET, seccompEPERM},
		{"a child in a new user namespace", unix.SYS_CLONE, uint64(unix.CLONE_NEWUSER | unix.SIGCHLD), seccompEPERM},
		{"clone3", unix.SYS_CLONE3, 0, seccompENOSYS},
	} {
		assert.Equal(t, tc.want, evalSeccomp(t, prog, seccompArch, tc.nr, tc.arg0), tc.name)
	}
	for _, nr := range seccompDenied {
		assert.Equal(t, uint32(seccompEPERM), evalSeccomp(t, prog, seccompArch, nr, 0), "call %d is denied", nr)
	}
	for _, nr := range []uint32{unix.SYS_SETSID, unix.SYS_SETPGID, unix.SYS_PTRACE, unix.SYS_UNSHARE, unix.SYS_IO_URING_SETUP,
		unix.SYS_BPF, unix.SYS_MOUNT, unix.SYS_SENDMSG, unix.SYS_PIDFD_SEND_SIGNAL} {
		assert.Contains(t, seccompDenied, nr, "call %d is on the deny list", nr)
	}

	assert.Equal(t, uint32(seccompKill), evalSeccomp(t, prog, 0x40000003 /* AUDIT_ARCH_I386 */, unix.SYS_READ, 0),
		"another architecture's call is killed")
	if seccompX32Bit != 0 {
		assert.Equal(t, uint32(seccompKill), evalSeccomp(t, prog, seccompArch, unix.SYS_READ|seccompX32Bit, 0), "an x32 call is killed")
	}
	assert.Equal(t, uint32(seccompEPERM), evalSeccomp(t, seccompFilter(99), seccompArch, unix.SYS_KILL, self),
		"the PID is the filter's own: another tool's is another PID")
}
