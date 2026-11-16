//go:build linux

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The calls the round-3 findings used, by their x86-64 numbers from x/sys,
// all refused (Ruling 18; blind-task-2b-r2-findings.md).
func TestSeccompFilter_Findings(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	const self = 4242
	prog, err := seccompProgram(seccompHost, self)
	require.NoError(t, err)
	for _, nr := range []uint32{
		unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_SHMGET, unix.SYS_SHMAT, unix.SYS_MSGGET, unix.SYS_SEMGET,
		unix.SYS_CHMOD, unix.SYS_FCHMOD, unix.SYS_FCHMODAT, unix.SYS_CHOWN, unix.SYS_FCHOWN, unix.SYS_FCHOWNAT,
		unix.SYS_LCHOWN, unix.SYS_UTIMENSAT, unix.SYS_UTIMES, unix.SYS_UTIME, unix.SYS_FUTIMESAT,
		unix.SYS_SETXATTR, unix.SYS_LSETXATTR, unix.SYS_FSETXATTR, unix.SYS_REMOVEXATTR,
		unix.SYS_SETPRIORITY, unix.SYS_GETPRIORITY, unix.SYS_SCHED_SETAFFINITY, unix.SYS_SCHED_SETSCHEDULER,
		unix.SYS_SCHED_SETPARAM, unix.SYS_SCHED_SETATTR, unix.SYS_IOPRIO_SET,
		unix.SYS_INOTIFY_INIT, unix.SYS_INOTIFY_INIT1, unix.SYS_INOTIFY_ADD_WATCH, unix.SYS_FANOTIFY_INIT,
		unix.SYS_MODIFY_LDT, unix.SYS_FORK, unix.SYS_VFORK, unix.SYS_CLONE3, unix.SYS_EXECVEAT,
		unix.SYS_UNSHARE, unix.SYS_SETNS, unix.SYS_PTRACE, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PIDFD_OPEN,
		unix.SYS_PIDFD_SEND_SIGNAL, unix.SYS_SETSID, unix.SYS_SETPGID, unix.SYS_IO_URING_SETUP, unix.SYS_BPF,
		unix.SYS_PERF_EVENT_OPEN, unix.SYS_KEYCTL, unix.SYS_USERFAULTFD, unix.SYS_MOUNT, unix.SYS_OPEN_TREE,
		unix.SYS_TRUNCATE, unix.SYS_FTRUNCATE, unix.SYS_UNLINK, unix.SYS_UNLINKAT, unix.SYS_RENAMEAT2,
		unix.SYS_MKDIRAT, unix.SYS_LINKAT, unix.SYS_SYMLINKAT, unix.SYS_MEMFD_CREATE, unix.SYS_SETRLIMIT,
	} {
		assert.Equal(t, uint32(seccompENOSYS), evalSeccomp(t, prog, seccompHost.audit, nr), "call %d gets ENOSYS", nr)
	}
}
