//go:build linux && (amd64 || arm64)

package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The media sandbox's seccomp filter (Ruling 16 item 1). Landlock decides
// which files a tool may open; this filter takes away the system calls it
// leaves open: sockets of every kind, signals to anything but the tool
// itself, new processes, sessions and namespaces, and kernel interfaces no
// decoder needs. The helper installs it after no_new_privs and Landlock, on
// the thread that then execs the tool, which keeps it. Classic BPF, built
// here with x/sys/unix: no libseccomp.

// seccompDenied are refused with EPERM whatever their arguments.
var seccompDenied = append([]uint32{
	// Sockets: the tools read one file and write a pipe.
	unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_CONNECT, unix.SYS_BIND, unix.SYS_LISTEN,
	unix.SYS_ACCEPT, unix.SYS_ACCEPT4, unix.SYS_SENDTO, unix.SYS_SENDMSG, unix.SYS_SENDMMSG,
	// A signal through a pidfd names no PID that could be checked.
	unix.SYS_PIDFD_SEND_SIGNAL,
	// A session or group of its own would escape the group kill.
	unix.SYS_SETSID, unix.SYS_SETPGID,
	// Other processes and their memory.
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PIDFD_GETFD, unix.SYS_PROCESS_MADVISE,
	// Namespaces.
	unix.SYS_UNSHARE, unix.SYS_SETNS,
	// Kernel attack surface.
	unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY, unix.SYS_USERFAULTFD,
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_MOVE_MOUNT, unix.SYS_OPEN_TREE,
	unix.SYS_FSOPEN, unix.SYS_FSCONFIG, unix.SYS_FSMOUNT, unix.SYS_FSPICK, unix.SYS_MOUNT_SETATTR,
	unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE, unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD,
}, seccompArchDenied...)

// seccompSelfSignals may only signal the tool itself: their first argument,
// the target PID or thread group, must be the tool's PID, which the helper
// knows because it execs in place. raise(3) and abort(3) (tgkill with the
// tool's PID, or kill and tkill of it) and the tool's signals between its own
// threads (tgkill) keep working.
var seccompSelfSignals = []uint32{
	unix.SYS_KILL, unix.SYS_TKILL, unix.SYS_TGKILL, unix.SYS_RT_SIGQUEUEINFO, unix.SYS_RT_TGSIGQUEUEINFO, unix.SYS_PIDFD_OPEN,
}

// cloneNamespaces are the namespace flags clone(2) refuses here.
const cloneNamespaces = unix.CLONE_NEWNS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC |
	unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET

// The parts of struct seccomp_data the filter reads (linux/seccomp.h). Both
// supported architectures are little-endian, so an argument's low 32 bits
// come first; the kernel truncates PIDs and clone flags to those.
const (
	seccompNr       = 0
	seccompArchWord = 4
	seccompArg0Low  = 16
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

func bpfStmt(code uint16, k uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k}
}

func bpfJump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

// seccompFilter is the program for a tool that runs as pid. Each check jumps
// only a few instructions, so it stays within classic BPF's 8-bit jumps.
func seccompFilter(pid uint32) []unix.SockFilter {
	f := []unix.SockFilter{
		// Another architecture's calls (a 32-bit entry, say) have other numbers: kill.
		bpfStmt(bpfLoad, seccompArchWord),
		bpfJump(bpfJumpEq, seccompArch, 1, 0),
		bpfStmt(bpfReturn, seccompKill),
		bpfStmt(bpfLoad, seccompNr),
	}
	if seccompX32Bit != 0 {
		// x32 calls share x86-64's architecture value and carry this bit: kill.
		f = append(f, bpfJump(bpfJumpGE, seccompX32Bit, 0, 1), bpfStmt(bpfReturn, seccompKill))
	}
	for _, nr := range seccompDenied {
		f = append(f, bpfJump(bpfJumpEq, nr, 0, 1), bpfStmt(bpfReturn, seccompEPERM))
	}
	// clone3 passes its flags in memory the filter cannot read: ENOSYS, so
	// libc falls back to clone, whose flags it can.
	f = append(f, bpfJump(bpfJumpEq, unix.SYS_CLONE3, 0, 1), bpfStmt(bpfReturn, seccompENOSYS))
	for _, nr := range seccompSelfSignals {
		f = append(f,
			bpfJump(bpfJumpEq, nr, 0, 4),
			bpfStmt(bpfLoad, seccompArg0Low),
			bpfJump(bpfJumpEq, pid, 0, 1),
			bpfStmt(bpfReturn, seccompAllow),
			bpfStmt(bpfReturn, seccompEPERM))
	}
	// clone makes threads only: CLONE_THREAD set, no namespace flag. A fork,
	// vfork or posix_spawn has no CLONE_THREAD.
	f = append(f,
		bpfJump(bpfJumpEq, unix.SYS_CLONE, 0, 5),
		bpfStmt(bpfLoad, seccompArg0Low),
		bpfJump(bpfJumpSet, unix.CLONE_THREAD, 0, 2),
		bpfJump(bpfJumpSet, cloneNamespaces, 1, 0),
		bpfStmt(bpfReturn, seccompAllow),
		bpfStmt(bpfReturn, seccompEPERM))
	return append(f, bpfStmt(bpfReturn, seccompAllow))
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
	filter := seccompFilter(uint32(pid))                                 // #nosec G115 -- a PID fits
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]} // #nosec G115 -- under 200 instructions
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0,
		uintptr(unsafe.Pointer(&prog))); errno != 0 { // #nosec G103 -- seccomp(2) of the filter
		return fmt.Errorf("seccomp: %w", errno)
	}
	return nil
}
