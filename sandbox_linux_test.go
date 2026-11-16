//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func init() {
	// Test-only rules (Ruling 14 note 6): the shell stand-ins in the tests are
	// scripts run by /bin/sh, and some start sleep. The sandbox lets a tool
	// execute only itself and its ELF interpreter (Ruling 16 item 6), so the
	// shell, sleep and their interpreters need rules of their own. Production
	// code never sets sandboxTestRules; real ffmpeg needs no shell.
	for _, name := range []string{"/bin/sh", "sleep"} {
		bin, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if bin, err = filepath.EvalSymlinks(bin); err != nil {
			continue
		}
		sandboxTestRules = append(sandboxTestRules, sandboxRule{path: bin, access: fsReadExec})
		if interp, err := elfInterpreter(bin); err == nil && interp != "" {
			sandboxTestRules = append(sandboxTestRules, sandboxRule{path: interp, access: fsReadExec})
		}
	}
	// The test binary as a stand-in tool is a Go program: on glibc its
	// runtime alone takes about 1.6 GiB of address space, more than
	// toolMemoryLimit, so it gets room. Real tools keep the cap.
	// "noseccomp-" in front of a stand-in's name runs it without the seccomp
	// filter, for tests whose stand-ins must fork; "abi4-" holds Landlock to
	// ABI 4, before it scoped signals; "bigmem-" gives 32 GiB, so that a
	// thread bomb meets RLIMIT_NPROC before glibc's per-thread stacks and
	// malloc arenas meet RLIMIT_AS.
	sandboxTestVariant = func(tool string) sandboxVariant {
		var v sandboxVariant
		name := filepath.Base(tool)
		if testStandIns[name] != nil {
			v.memory = 4 << 30
		}
		for {
			switch {
			case strings.HasPrefix(name, "noseccomp-"):
				v.noSeccomp, name = true, strings.TrimPrefix(name, "noseccomp-")
			case strings.HasPrefix(name, "abi4-"):
				v.maxABI, name = 4, strings.TrimPrefix(name, "abi4-")
			case strings.HasPrefix(name, "bigmem-"):
				v.memory, name = 32<<30, strings.TrimPrefix(name, "bigmem-")
			default:
				return v
			}
		}
	}
	// Every helper the tests start works on a thread that did not inherit
	// Pdeathsig, so the parent-death test proves the re-arm (review Minor 3).
	sandboxFreshThread = true
	testStandIns["tcp-dial"] = tcpDialStandIn
	testStandIns["noseccomp-tcp-dial"] = tcpDialStandIn
	testStandIns["sandbox-parent"] = sandboxParentStandIn
	testStandIns["truncate-path"] = truncatePathStandIn
	testStandIns["noseccomp-truncate-path"] = truncatePathStandIn
	testStandIns["bigmem-sandbox-probe"] = sandboxProbeStandIn
	// The probe also answers to its variants' names: "noseccomp-" runs it
	// without the seccomp filter, "abi4-" with Landlock held to ABI 4.
	for _, variant := range []string{"", "noseccomp-", "abi4-", "noseccomp-abi4-"} {
		testStandIns[variant+"sandbox-probe"] = sandboxProbeStandIn
	}
}

// requireLandlock skips the test where the kernel (or a seccomp profile)
// offers no Landlock that counts as a sandbox (ABI minSandboxABI or later),
// and returns the ABI otherwise.
//
// Where the task cap binds no user (root) the sandbox is not whole and a
// real run needs the opt-out (Ruling 19 item 4), so the test gets it, as a
// bot with video.allow_unsandboxed would: every other layer still applies.
func requireLandlock(t *testing.T) int {
	t.Helper()
	abi, err := sandboxAvailable()
	if err := sandboxUsable(abi, err); err != nil {
		t.Skipf("no sandbox here: %v", err)
	}
	t.Logf("Landlock ABI %d", abi)
	allowUnsandboxedWhereNeeded(t)
	return abi
}

// standInTool puts the test binary on PATH as a tool called name. Run as that
// tool (through the sandbox or not), TestMain hands it to testStandIns[name].
func standInTool(t *testing.T, name string) string {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	link := filepath.Join(dir, name)
	require.NoError(t, os.Symlink(exe, link))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return link
}

// tcpDialStandIn connects to the address in args[0]: exit 0 when it can, 3 when it cannot.
func tcpDialStandIn(args []string) int {
	if len(args) != 1 {
		return 2
	}
	c, err := net.DialTimeout("tcp", args[0], 2*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	_ = c.Close()
	return 0
}

// probeActions are what sandboxProbeStandIn can try, each as a compromised
// tool might. A nil error means it worked.
var probeActions = map[string]func(args []string) error{
	// exec <path>: replace this process with another program.
	"exec": func(args []string) error {
		return syscall.Exec(args[0], []string{args[0], "-version"}, []string{"PATH=/usr/bin:/bin"}) // #nosec G204 -- the test's own path
	},
	// udp <addr>: send a datagram.
	"udp": func(args []string) error {
		c, err := net.Dial("udp", args[0])
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		_, err = c.Write([]byte("leak"))
		return err
	},
	// unix <path>: connect to a pathname unix socket.
	"unix": func(args []string) error {
		c, err := net.Dial("unix", args[0])
		if err != nil {
			return err
		}
		return c.Close()
	},
	// netlink: open a netlink socket.
	"netlink": func([]string) error {
		fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
		if err != nil {
			return err
		}
		return syscall.Close(fd)
	},
	// kill <pid>: SIGTERM another process of the same user.
	"kill": func(args []string) error {
		pid, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		return syscall.Kill(pid, syscall.SIGTERM)
	},
	// signal-parent: SIGUSR1 to the bot that ran this tool.
	"signal-parent": func([]string) error { return syscall.Kill(os.Getppid(), syscall.SIGUSR1) },
	// fork: a child process, which leaves at once.
	"fork": func([]string) error { return cloneAndReap(0) },
	// userns: a child process in a new user namespace.
	"userns": func([]string) error { return cloneAndReap(syscall.CLONE_NEWUSER) },
	// setsid and setpgid: a session or group of its own, out of the group kill.
	"setsid": func([]string) error {
		_, err := syscall.Setsid()
		return err
	},
	"setpgid": func([]string) error { return syscall.Setpgid(0, 0) },
	// io_uring: set up an io_uring.
	"io_uring": func([]string) error {
		var params [120]byte
		fd, _, errno := syscall.Syscall(unix.SYS_IO_URING_SETUP, 1, uintptr(unsafe.Pointer(&params[0])), 0) // #nosec G103 -- io_uring_setup(2)
		if errno != 0 {
			return errno
		}
		return syscall.Close(int(fd))
	},
	// ptrace <pid>: attach to another process.
	"ptrace": func(args []string) error {
		pid, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		if err := syscall.PtraceAttach(pid); err != nil {
			return err
		}
		return syscall.PtraceDetach(pid)
	},
	// threads <n>: start n OS threads at once, a thread bomb.
	"threads": func(args []string) error {
		n, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		// A failed thread start then prints its error and dies, without a
		// dump of every goroutine that would push the error out of the
		// stderr tail: debug.SetTraceback quiets the runtime's own
		// newosproc throw, and catching SIGABRT stops the dump on a cgo
		// build, whose pthread_create failure calls abort, which then
		// re-raises SIGABRT at its default action.
		debug.SetTraceback("none")
		signal.Notify(make(chan os.Signal, 1), syscall.SIGABRT)
		ready, block := make(chan struct{}), make(chan struct{})
		for range n {
			go func() {
				runtime.LockOSThread()
				ready <- struct{}{}
				<-block
			}()
		}
		for range n {
			<-ready
		}
		return nil
	},
	// The round-3 findings' attacks (blind-task-2b-r2-findings.md), each a
	// harmless stand-in that would change nothing if it got through.
	//
	// prlimit <pid>: read another process's open-files limit; the findings
	// set its CPU limit to 1, which kills it.
	"prlimit": func(args []string) error {
		pid, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		var old unix.Rlimit
		return unix.Prlimit(pid, unix.RLIMIT_NOFILE, nil, &old)
	},
	// setown, setsig: make the bot the owner of this tool's stderr pipe, or
	// pick SIGUSR1 for it; without O_ASYNC no signal follows. With both and
	// O_ASYNC, the findings signalled the bot below ABI 6.
	"setown": func([]string) error {
		_, err := unix.FcntlInt(2, unix.F_SETOWN, os.Getppid())
		return err
	},
	"setsig": func([]string) error {
		_, err := unix.FcntlInt(2, unix.F_SETSIG, int(syscall.SIGUSR1))
		return err
	},
	// shmget, msgget: a SysV segment or queue, which outlives the tool;
	// removed again at once.
	"shmget": func([]string) error {
		id, err := unix.SysvShmGet(unix.IPC_PRIVATE, 4096, unix.IPC_CREAT|0o600)
		if err != nil {
			return err
		}
		_, err = unix.SysvShmCtl(id, unix.IPC_RMID, nil)
		return err
	},
	"msgget": func([]string) error {
		id, _, errno := syscall.RawSyscall(unix.SYS_MSGGET, unix.IPC_PRIVATE, unix.IPC_CREAT|0o600, 0)
		if errno != 0 {
			return errno
		}
		if _, _, errno := syscall.RawSyscall(unix.SYS_MSGCTL, id, unix.IPC_RMID, 0); errno != 0 {
			return errno
		}
		return nil
	},
	// chmod, chown, utimensat, setxattr <path>: metadata changes, which
	// Landlock does not cover, on a file it cannot read: its own mode, its
	// own owner, its times left as they are (UTIME_OMIT), and an attribute
	// replaced that is not there (ENODATA if the call got through).
	"chmod": func(args []string) error {
		var st unix.Stat_t
		if err := unix.Stat(args[0], &st); err != nil {
			return err
		}
		return unix.Chmod(args[0], st.Mode&0o7777)
	},
	"chown": func(args []string) error {
		var st unix.Stat_t
		if err := unix.Stat(args[0], &st); err != nil {
			return err
		}
		return unix.Chown(args[0], int(st.Uid), int(st.Gid))
	},
	"utimensat": func(args []string) error {
		omit := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, {Nsec: unix.UTIME_OMIT}}
		return unix.UtimesNanoAt(unix.AT_FDCWD, args[0], omit, 0)
	},
	"setxattr": func(args []string) error {
		return unix.Setxattr(args[0], "user.media-sandbox-probe", []byte("x"), unix.XATTR_REPLACE)
	},
	// setpriority <pid> <nice>: set a process's nice value to the one it has.
	"setpriority": func(args []string) error {
		pid, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		nice, err := strconv.Atoi(args[1])
		if err != nil {
			return err
		}
		return unix.Setpriority(unix.PRIO_PROCESS, pid, nice)
	},
	// sched_setaffinity <pid>: an empty CPU set, which the kernel itself
	// refuses with EINVAL if the call gets through.
	"sched_setaffinity": func(args []string) error {
		pid, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		var none unix.CPUSet
		return unix.SchedSetaffinity(pid, &none)
	},
	// sched_setscheduler <pid>: an unknown policy, EINVAL if it gets through.
	"sched_setscheduler": func(args []string) error {
		pid, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		var param [1]int32
		if _, _, errno := syscall.RawSyscall(unix.SYS_SCHED_SETSCHEDULER, uintptr(pid), 99,
			uintptr(unsafe.Pointer(&param[0]))); errno != 0 { // #nosec G103 -- sched_setscheduler(2)
			return errno
		}
		return nil
	},
	// ioprio_set <pid>: an unknown I/O class, EINVAL if it gets through.
	"ioprio_set": func(args []string) error {
		pid, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		const whoProcess, badClass = 1, 7 << 13
		if _, _, errno := syscall.RawSyscall(unix.SYS_IOPRIO_SET, whoProcess, uintptr(pid), badClass); errno != 0 {
			return errno
		}
		return nil
	},
	// pdeathsig: clear the parent-death signal, to outlive the bot.
	"pdeathsig": func([]string) error { return unix.Prctl(unix.PR_SET_PDEATHSIG, 0, 0, 0, 0) },
	// mlock <bytes>: lock that much of its own memory, as libgcrypt locks
	// 16 KiB of secure memory on Ubuntu.
	"mlock": func(args []string) error {
		n, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		return unix.Mlock(make([]byte, n))
	},
	// futex-lock-pi: take an uncontended priority-inheriting futex of its
	// own, a kernel path with a history of bugs (CVE-2014-3153).
	"futex-lock-pi": func([]string) error {
		const lockPIPrivate = 6 | 128 // FUTEX_LOCK_PI | FUTEX_PRIVATE_FLAG
		word := new(uint32)
		if _, _, errno := syscall.Syscall6(unix.SYS_FUTEX, uintptr(unsafe.Pointer(word)), lockPIPrivate, 0, 0, 0, 0); errno != 0 { // #nosec G103 -- futex(2)
			return errno
		}
		return nil
	},
	// inotify, fanotify: watch the bot's directories for new names.
	"inotify": func([]string) error {
		fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
		if err != nil {
			return err
		}
		return unix.Close(fd)
	},
	"fanotify": func([]string) error {
		fd, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_REPORT_FID, unix.O_RDONLY)
		if err != nil {
			return err
		}
		return unix.Close(fd)
	},
	// modify_ldt: read the LDT, on x86-64 only (its number there, 154).
	"modify_ldt": func([]string) error {
		if runtime.GOARCH != "amd64" {
			return errors.New("modify_ldt is x86's")
		}
		var ldt [16]byte
		if _, _, errno := syscall.RawSyscall(154, 0, uintptr(unsafe.Pointer(&ldt[0])), uintptr(len(ldt))); errno != 0 { // #nosec G103 -- modify_ldt(2)
			return errno
		}
		return nil
	},
	// syscall <nr>: a call by number, with no arguments.
	"syscall": func(args []string) error {
		nr, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		if _, _, errno := syscall.RawSyscall(uintptr(nr), 0, 0, 0); errno != 0 {
			return errno
		}
		return nil
	},
	// raise: abort the way C code does, raise(SIGABRT) with tgkill on this
	// thread and the default action. It must die of it.
	"raise": func([]string) error {
		act := struct{ handler, flags, restorer, mask uintptr }{} // SIG_DFL
		if _, _, errno := syscall.RawSyscall6(syscall.SYS_RT_SIGACTION, uintptr(syscall.SIGABRT),
			uintptr(unsafe.Pointer(&act)), 0, 8, 0, 0); errno != 0 { // #nosec G103 -- rt_sigaction(2)
			return errno
		}
		runtime.LockOSThread()
		if _, _, errno := syscall.RawSyscall(syscall.SYS_TGKILL, uintptr(os.Getpid()), uintptr(syscall.Gettid()), uintptr(syscall.SIGABRT)); errno != 0 {
			return errno
		}
		time.Sleep(time.Second)
		return errors.New("raise(SIGABRT) returned")
	},
}

// cloneAndReap makes a child with clone(2) and flags; the child exits at once.
func cloneAndReap(flags uintptr) error {
	pid, _, errno := syscall.RawSyscall6(syscall.SYS_CLONE, flags|uintptr(syscall.SIGCHLD), 0, 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	if pid == 0 {
		_, _, _ = syscall.RawSyscall(syscall.SYS_EXIT_GROUP, 0, 0, 0)
	}
	var ws syscall.WaitStatus
	_, err := syscall.Wait4(int(pid), &ws, 0, nil)
	return err
}

// sandboxProbeStandIn runs probeActions[args[0]] with the rest: exit 0 when
// it works, 3 (the error on stderr) when it does not, 2 for a bad call.
func sandboxProbeStandIn(args []string) int {
	if len(args) == 0 || probeActions[args[0]] == nil {
		return 2
	}
	if err := probeActions[args[0]](args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	return 0
}

// truncatePathStandIn empties the file args[0] with truncate(2): by path,
// opening nothing, the call Landlock denies only from ABI 3 (Ruling 15).
// Exit 0 when it can, 3 when it cannot.
func truncatePathStandIn(args []string) int {
	if len(args) != 1 {
		return 2
	}
	if err := syscall.Truncate(args[0], 0); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	return 0
}

// sandboxParentStandIn plays the bot: it starts args[0] (a tool) in the
// directory args[1] through newToolCmd, prints the child's PID and then waits
// to be killed.
func sandboxParentStandIn(args []string) int {
	if len(args) < 2 {
		return 2
	}
	unsandboxedGrants.Add(1) // only matters where there is no Landlock: the helper uses it whenever it can
	cmd, err := newToolCmd(context.Background(), args[0], args[1], "", args[2:])
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(cmd.Process.Pid)
	time.Sleep(time.Hour)
	return 0
}

// writeFake writes content to root/rel, creating its directories.
func writeFake(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

// requireDenied checks that a sandboxed run was refused: a non-zero exit
// whose stderr says "Permission denied" (EACCES, Landlock), "Operation not
// permitted" (EPERM, an argument check of the seccomp filter) or "Function
// not implemented" (ENOSYS, a call off its allowlist), never "No such file".
func requireDenied(t *testing.T, out []byte, err error, what string) {
	t.Helper()
	var te *toolError
	require.ErrorAs(t, err, &te, "%s: the run must fail", what)
	assert.Empty(t, out, what)
	stderr := strings.ToLower(te.Stderr())
	assert.True(t, strings.Contains(stderr, "permission denied") || strings.Contains(stderr, "operation not permitted") ||
		strings.Contains(stderr, "function not implemented"), "%s: refused by the sandbox, got %q", what, te.Stderr())
}

// The sandbox lets a tool read inside its input directory and nothing else
// of the bot's: not another video, not another chat's, not the database, not
// config/. It can create, change or delete nothing. Everything here is a fake
// repository in a temp dir; the real config/ is never touched. The input is
// a download in a directory of its own: the input directory guard refuses
// anything beneath the bot's own directories (Ruling 16 item 4).
func TestMediaSandbox_FileAccess(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireLandlock(t)
	root := t.TempDir() // stands in for the bot's working directory
	token := writeFake(t, root, "config/bot.json", `{"telegram_token":"fake-token-for-the-sandbox-test"}`)
	db := writeFake(t, root, "data/bot.db", "fake chat history")
	sibling := writeFake(t, root, "data/videos/1/42/v2/video", "another video of this chat")
	otherChat := writeFake(t, root, "data/videos/1/43/v1/video", "another chat's video")
	otherBot := writeFake(t, root, "data/videos/2/42/v1/video", "another bot's video")
	video := writeFake(t, t.TempDir(), "video", "this chat's video")
	in := filepath.Dir(video)
	t.Chdir(root)
	ctx := context.Background()
	relative := func(p string) string {
		r, err := filepath.Rel(in, p)
		require.NoError(t, err)
		return r
	}

	out, err := runTool(ctx, "cat", in, "", []string{video})
	require.NoError(t, err, "%q", toolStderr(err))
	assert.Equal(t, "this chat's video", string(out))
	out, err = runTool(ctx, "cat", in, "", []string{"video"})
	require.NoError(t, err, "relative to the tool's working directory, the input directory")
	assert.Equal(t, "this chat's video", string(out))

	for _, p := range []string{sibling, otherChat, otherBot, db, token, relative(sibling), relative(token)} {
		control := exec.Command("cat", p) // #nosec G204 -- a test fixture path
		control.Dir = in
		got, err := control.Output()
		require.NoError(t, err, "outside the sandbox the same cat reads %s", p)
		require.NotEmpty(t, got)
		out, err := runTool(ctx, "cat", in, "", []string{p})
		requireDenied(t, out, err, "read "+p)
	}

	for _, p := range []string{
		filepath.Join("/tmp", "media-sandbox-made-"+strconv.Itoa(os.Getpid())),
		filepath.Join(os.TempDir(), "media-sandbox-made-2-"+strconv.Itoa(os.Getpid())),
		filepath.Join(in, "new"),
		filepath.Join(root, "config", "new.json"),
	} {
		out, err := runTool(ctx, "touch", in, "", []string{p})
		requireDenied(t, out, err, "create "+p)
		assert.NoFileExists(t, p)
	}
	out, err = runTool(ctx, "tee", in, "", []string{"-a", video})
	requireDenied(t, out, err, "append to the input")
	out, err = runTool(ctx, "rm", in, "", []string{"-f", video})
	requireDenied(t, out, err, "delete the input")
	out, err = runTool(ctx, "mkdir", in, "", []string{filepath.Join(in, "d")})
	requireDenied(t, out, err, "make a directory")

	// Truncation, which Landlock can deny only from ABI 3 (Ruling 15): the
	// truncate tool opens the file for writing first, so the write rule
	// stops it too; truncate(2) by path opens nothing, so only the truncate
	// right does. An ABI 1 or 2 kernel would let it empty the database. The
	// seccomp filter refuses truncate(2) as well, so it is tried without the
	// filter too: Landlock alone must refuse it.
	standInTool(t, "truncate-path")
	standInTool(t, "noseccomp-truncate-path")
	for _, p := range []string{db, video} {
		out, err := runTool(ctx, "truncate", in, "", []string{"-s", "0", p})
		requireDenied(t, out, err, "truncate -s 0 "+p)
		out, err = runTool(ctx, "truncate-path", in, "", []string{p})
		requireDenied(t, out, err, "truncate(2) "+p)
		out, err = runTool(ctx, "noseccomp-truncate-path", in, "", []string{p})
		requireDenied(t, out, err, "truncate(2) without the filter "+p)
		assert.Contains(t, toolStderr(err), "permission denied", "Landlock's own refusal")
	}
	data, err := os.ReadFile(db) // #nosec G304 -- the test's own fixture
	require.NoError(t, err)
	assert.Equal(t, "fake chat history", string(data), "the database is unchanged")
	data, err = os.ReadFile(video) // #nosec G304 -- the test's own fixture
	require.NoError(t, err)
	assert.Equal(t, "this chat's video", string(data), "the input is unchanged")
	assert.FileExists(t, token, "the fake config is still there")
}

// The seccomp filter (Rulings 16 and 18) stops what Landlock leaves open.
// Each thing is tried by the probe stand-in through the real helper and
// runTool, and must fail with EPERM (an argument check) or ENOSYS (a call
// off the allowlist). Where the container allows it, the same try without
// the filter ("noseccomp-") works, so the failure is the filter's. Signals
// are tried at ABI 4 too, where Landlock does not scope them, so the filter
// alone keeps the bot and other processes safe.
func TestMediaSandbox_Seccomp(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireLandlock(t)
	for _, name := range []string{"sandbox-probe", "noseccomp-sandbox-probe", "abi4-sandbox-probe", "noseccomp-abi4-sandbox-probe"} {
		standInTool(t, name)
	}
	ctx := context.Background()
	denied := func(what, tool string, args ...string) {
		t.Helper()
		out, err := runTool(ctx, tool, inputDir(t), "", args)
		var te *toolError
		if assert.ErrorAs(t, err, &te, "%s: the run must fail", what) {
			assert.Regexp(t, "operation not permitted|function not implemented", te.Stderr(), what)
		}
		assert.Empty(t, out, what)
	}
	works := func(what, tool string, args ...string) {
		t.Helper()
		_, err := runTool(ctx, tool, inputDir(t), "", args)
		assert.NoError(t, err, "%s without the filter: %q", what, toolStderr(err))
	}

	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = udp.Close() })
	var datagrams atomic.Int32
	go func() {
		buf := make([]byte, 64)
		for {
			if _, _, err := udp.ReadFrom(buf); err != nil {
				return
			}
			datagrams.Add(1)
		}
	}()
	denied("UDP", "sandbox-probe", "udp", udp.LocalAddr().String())
	works("UDP", "noseccomp-sandbox-probe", "udp", udp.LocalAddr().String())
	assert.Eventually(t, func() bool { return datagrams.Load() == 1 }, 2*time.Second, 10*time.Millisecond, "only the unfiltered datagram arrived")

	sock := filepath.Join(t.TempDir(), "bus")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	var connections atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			_ = c.Close()
		}
	}()
	denied("pathname unix connect", "sandbox-probe", "unix", sock)
	works("pathname unix connect", "noseccomp-sandbox-probe", "unix", sock)
	assert.Eventually(t, func() bool { return connections.Load() == 1 }, 2*time.Second, 10*time.Millisecond, "only the unfiltered connection arrived")

	for _, action := range []string{"netlink", "fork", "setpgid"} {
		denied(action, "sandbox-probe", action)
		works(action, "noseccomp-sandbox-probe", action)
	}
	// No control for these: the tool leads its group, so setsid fails anyway,
	// and Docker's own profile already refuses the next two; ptrace of
	// another domain is Landlock's too.
	denied("setsid", "sandbox-probe", "setsid")
	denied("clone(CLONE_NEWUSER)", "sandbox-probe", "userns")
	denied("io_uring_setup", "sandbox-probe", "io_uring")

	other := exec.Command("sleep", "30") // another process of the same user
	require.NoError(t, other.Start())
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	pid := strconv.Itoa(other.Process.Pid)
	denied("ptrace", "sandbox-probe", "ptrace", pid)
	denied("kill another process", "sandbox-probe", "kill", pid)
	denied("kill another process at ABI 4", "abi4-sandbox-probe", "kill", pid)
	assert.False(t, processGone(pid), "the other process lives")

	usr1 := make(chan os.Signal, 4)
	signal.Notify(usr1, syscall.SIGUSR1)
	t.Cleanup(func() { signal.Stop(usr1) })
	denied("signal the bot", "sandbox-probe", "signal-parent")
	denied("signal the bot at ABI 4", "abi4-sandbox-probe", "signal-parent")
	select {
	case <-usr1:
		t.Error("a filtered tool signalled the bot")
	case <-time.After(300 * time.Millisecond):
	}
	works("signal the bot at ABI 4", "noseccomp-abi4-sandbox-probe", "signal-parent")
	select {
	case <-usr1:
	case <-time.After(2 * time.Second):
		t.Error("without the filter, at ABI 4, the signal should have arrived")
	}

	// A tool's own raise(SIGABRT) still works: it dies of the signal, not of EPERM.
	_, err = runTool(ctx, "sandbox-probe", inputDir(t), "", []string{"raise"})
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "%q", toolStderr(err))
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	assert.True(t, ws.Signaled() && ws.Signal() == syscall.SIGABRT, "died of SIGABRT: %v, %q", exitErr, toolStderr(err))
}

// The round-3 findings' attacks (Ruling 18 item 1), each through the real
// helper by the probe stand-in, all refused: EPERM where an allowed call's
// argument check refuses it, ENOSYS (the filter's own mark: the kernel has
// these calls) where the call is off the allowlist. Where harmless, the
// same try without the filter works, so the refusal is the filter's. The
// thread bomb is TestMediaSandbox_ThreadCap's, raise(SIGABRT) the end of
// TestMediaSandbox_Seccomp.
func TestMediaSandbox_AllowlistRefusals(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireLandlock(t)
	for _, name := range []string{"sandbox-probe", "noseccomp-sandbox-probe", "abi4-sandbox-probe", "noseccomp-abi4-sandbox-probe"} {
		standInTool(t, name)
	}
	ctx := context.Background()
	refused := func(errno, what, tool string, args ...string) {
		t.Helper()
		out, err := runTool(ctx, tool, inputDir(t), "", args)
		var te *toolError
		if assert.ErrorAs(t, err, &te, "%s: the run must fail", what) {
			assert.Contains(t, strings.ToLower(te.Stderr()), errno, what)
		}
		assert.Empty(t, out, what)
	}
	const eperm, enosys = "operation not permitted", "function not implemented"
	works := func(what, tool string, args ...string) {
		t.Helper()
		_, err := runTool(ctx, tool, inputDir(t), "", args)
		assert.NoError(t, err, "%s without the filter: %q", what, toolStderr(err))
	}

	other := exec.Command("sleep", "30") // another process of the same user
	require.NoError(t, other.Start())
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	bot, sleeper := strconv.Itoa(os.Getpid()), strconv.Itoa(other.Process.Pid)

	refused(eperm, "prlimit64 on the bot", "sandbox-probe", "prlimit", bot)
	refused(eperm, "prlimit64 on another process", "sandbox-probe", "prlimit", sleeper)
	works("prlimit64 on the bot", "noseccomp-sandbox-probe", "prlimit", bot)
	for _, action := range []string{"setown", "setsig"} {
		refused(eperm, action+" at ABI 4", "abi4-sandbox-probe", action)
		works(action+" at ABI 4", "noseccomp-abi4-sandbox-probe", action)
	}
	refused(eperm, "PR_SET_PDEATHSIG 0", "sandbox-probe", "pdeathsig")
	refused(eperm, "FUTEX_LOCK_PI", "sandbox-probe", "futex-lock-pi")
	works("FUTEX_LOCK_PI", "noseccomp-sandbox-probe", "futex-lock-pi")
	works("PR_SET_PDEATHSIG 0", "noseccomp-sandbox-probe", "pdeathsig")

	for _, action := range []string{"shmget", "msgget", "inotify"} {
		refused(enosys, action, "sandbox-probe", action)
		works(action, "noseccomp-sandbox-probe", action)
	}
	refused(enosys, "fanotify_init", "sandbox-probe", "fanotify")

	// A file outside the input directory, which Landlock lets no tool read.
	outside := writeFake(t, t.TempDir(), "bot.db", "fake chat history")
	before, err := os.Stat(outside)
	require.NoError(t, err)
	for _, action := range []string{"chmod", "chown", "utimensat", "setxattr"} {
		refused(enosys, action+" outside the input directory", "sandbox-probe", action, outside)
	}
	for _, action := range []string{"chmod", "chown", "utimensat"} {
		works(action+" outside the input directory", "noseccomp-sandbox-probe", action, outside)
	}
	after, err := os.Stat(outside)
	require.NoError(t, err)
	assert.Equal(t, before.Mode(), after.Mode(), "the file's mode is as it was")
	assert.Equal(t, before.ModTime(), after.ModTime(), "the file's time is as it was")

	prio, err := unix.Getpriority(unix.PRIO_PROCESS, 0) // the kernel's 20 - nice
	require.NoError(t, err)
	refused(enosys, "setpriority on the bot", "sandbox-probe", "setpriority", bot, strconv.Itoa(20-prio))
	for _, action := range []string{"sched_setaffinity", "sched_setscheduler", "ioprio_set"} {
		refused(enosys, action+" on the bot", "sandbox-probe", action, bot)
	}
	if runtime.GOARCH == "amd64" {
		refused(enosys, "modify_ldt", "sandbox-probe", "modify_ldt")
	}
	refused(enosys, "an unknown, high call", "sandbox-probe", "syscall", "1000")
	assert.False(t, processGone(sleeper), "the other process lives")
}

func init() { taskCapUnboundHere = taskCapUnbound }

// taskCapUnbound reports whether RLIMIT_NPROC binds no user here: root in
// the initial user namespace is exempt (in Docker, uid 0 even without
// CAP_SYS_RESOURCE), so a probe forks even under a limit of 1. The answer is
// the process's, so one probe serves every test.
func taskCapUnbound(t *testing.T) bool {
	t.Helper()
	forks, err := taskCapProbe()
	require.NoError(t, err)
	return forks
}

var taskCapProbe = sync.OnceValues(func() (bool, error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(unix.RLIMIT_NPROC, &lim); err != nil {
		return false, err
	}
	return forksUnder(1, lim.Max)
})

// The helper drops every capability before anything else (Ruling 19 item
// 4), so with no_new_privs the tool's exec gains none back, root's included:
// no prlimit64(0) raising a hard limit, no madvise(MADV_HWPOISON). Read from
// outside, in /proc/<pid>/status, all four sets are empty. The test means
// most as root (uid 0, in Docker): a non-root user has none to drop.
func TestMediaSandbox_NoCapabilities(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	allowUnsandboxedWhereNeeded(t)
	t.Logf("uid %d", os.Getuid())
	sleep, err := lookupTool("sleep")
	require.NoError(t, err)
	cmd := startSandboxed(t, sleep, "30")
	status := procFile(t, cmd.Process.Pid, "status")
	for _, set := range []string{"CapInh", "CapPrm", "CapEff", "CapAmb"} {
		assert.Contains(t, status, set+":\t0000000000000000\n", "%s is empty", set)
	}
}

// Where the task cap binds no user (the bot as root), the sandbox is not
// whole (Ruling 19 item 4): video is off unless video.allow_unsandboxed,
// and the log says to run the bot as a non-root user. With the opt-out it
// runs. Run as uid 0 to test; a user the cap binds skips.
func TestNewBot_VideoGate_TaskCapUnbound(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	requireLandlock(t)
	if !taskCapUnbound(t) {
		t.Skip("the task cap binds this user: run as root (uid 0) to test")
	}
	var logged bytes.Buffer
	prevInfo, prevError := InfoLogger, ErrorLogger
	InfoLogger, ErrorLogger = log.New(&logged, "", 0), log.New(&logged, "", 0)
	t.Cleanup(func() { InfoLogger, ErrorLogger = prevInfo, prevError })

	b := videoGateBot(t, setupTestDB(t), &VideoConfig{})
	assert.False(t, b.videoOn(), "no whole sandbox, no video")
	assert.Contains(t, logged.String(), "run the bot as a non-root user")
	assert.Contains(t, logged.String(), "video watching is off")

	logged.Reset()
	before := unsandboxedGrants.Load()
	b = videoGateBot(t, setupTestDB(t), &VideoConfig{AllowUnsandboxed: true})
	t.Cleanup(func() { unsandboxedGrants.Store(before) })
	assert.True(t, b.videoOn(), "the opt-out restores video: %s", logged.String())
	assert.Contains(t, logged.String(), "Warning: video: ffmpeg runs WITHOUT a full sandbox")
	assert.Contains(t, logged.String(), "run the bot as a non-root user")
}

// The task count is probed in a child of its own: the caller's RLIMIT_NPROC
// never drops to the user's count, where its Go runtime could not start a
// thread. A limit of 1 never lets a probe fork (it is one of the user's
// tasks itself) unless the user is exempt from the limit.
func TestKernelUIDTasks(t *testing.T) {
	var before, after syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(unix.RLIMIT_NPROC, &before))
	visible, err := uidTasks(os.Getuid())
	require.NoError(t, err)
	n, enforced, err := kernelUIDTasks(visible, before.Max, func(limit uint64) (bool, error) { return forksUnder(limit, before.Max) })
	require.NoError(t, err)
	assert.Positive(t, n)
	assert.Equal(t, !taskCapUnbound(t), enforced, "the limit binds this user unless it is root")
	require.NoError(t, syscall.Getrlimit(unix.RLIMIT_NPROC, &after))
	assert.Equal(t, before, after, "the probes leave the caller's limit alone")

	ok, err := forksUnder(1, before.Max)
	require.NoError(t, err)
	if os.Getuid() != 0 {
		assert.False(t, ok, "a limit of 1 refuses the probe's fork")
	}
	ok, err = forksUnder(before.Max, before.Max)
	require.NoError(t, err)
	assert.True(t, ok, "the user's own hard limit lets it fork")
	require.NoError(t, syscall.Getrlimit(unix.RLIMIT_NPROC, &after))
	assert.Equal(t, before, after, "the probes leave the caller's limit alone")
}

// The task count's search ends, whatever the forks answer (Ruling 19 item
// 5). A fake fork stands in for the kernel: with a full pids cgroup every
// fork is refused, whatever the limit, so the bracket never finds one that
// lets a fork through; under a hard limit of RLIM_INFINITY its doubling
// must stop all the same, with an error. A fork refused at the hard limit
// is the user out of tasks; one allowed at every limit is a user the limit
// does not bind; a well-behaved fake gives the count.
func TestKernelUIDTasks_Terminates(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	const infinity = ^uint64(0) // RLIM_INFINITY
	fake := func(t *testing.T, ok func(limit uint64) bool) (func(uint64) (bool, error), *int) {
		calls := 0
		return func(limit uint64) (bool, error) {
			calls++
			if calls > 1000 {
				t.Fatalf("still probing after %d forks (limit %d)", calls, limit)
			}
			return ok(limit), nil
		}, &calls
	}
	const count, k = 40, 30 // the user's tasks; no fork above k
	forks, calls := fake(t, func(limit uint64) bool { return limit >= count+2 && limit <= k })
	_, _, err := kernelUIDTasks(10, infinity, forks)
	require.Error(t, err, "a fork refused under every limit is no count")
	assert.LessOrEqual(t, *calls, 128)

	forks, _ = fake(t, func(limit uint64) bool { return limit >= count+2 })
	_, _, err = kernelUIDTasks(10, 35, forks)
	require.ErrorContains(t, err, "task limit (35)", "refused at the hard limit: out of tasks")

	forks, calls = fake(t, func(limit uint64) bool { return limit >= count+2 })
	n, enforced, err := kernelUIDTasks(10, infinity, forks)
	require.NoError(t, err)
	assert.Equal(t, count, n)
	assert.True(t, enforced)
	assert.LessOrEqual(t, *calls, 128)

	forks, _ = fake(t, func(uint64) bool { return true })
	_, enforced, err = kernelUIDTasks(10, infinity, forks)
	require.NoError(t, err)
	assert.False(t, enforced, "a probe that forks even under a limit of 1: the limit binds no one (root)")

	// Concurrent runs' threads end between the count in /proc and the
	// probes, so a fork goes through at the visible count: that alone is no
	// sign of an unbound cap, as a limit of 1 still refuses it. Seen as
	// refused runs under four-way concurrency on Ubuntu.
	const fell = 5 // the user's tasks by the time the probes run
	forks, calls = fake(t, func(limit uint64) bool { return limit >= fell+2 })
	n, enforced, err = kernelUIDTasks(10, infinity, forks)
	require.NoError(t, err)
	assert.True(t, enforced, "the limit binds: 1 refuses the fork")
	assert.Equal(t, fell, n, "the count as it is now, below the visible one")
	assert.LessOrEqual(t, *calls, maxTaskProbes)
}

// A tool may lock only 64 KiB of memory (RLIMIT_MEMLOCK, Ruling 19 item 3),
// not all of its 1.5 GiB: libgcrypt's 16 KiB still fits (the Ubuntu
// renders take that path on every run), 128 KiB does not.
func TestMediaSandbox_LockedMemoryCap(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	allowUnsandboxedWhereNeeded(t)
	standInTool(t, "sandbox-probe")
	_, err := runTool(context.Background(), "sandbox-probe", inputDir(t), "", []string{"mlock", "16384"})
	require.NoError(t, err, "16 KiB fits: %q", toolStderr(err))
	_, err = runTool(context.Background(), "sandbox-probe", inputDir(t), "", []string{"mlock", "131072"})
	var te *toolError
	require.ErrorAs(t, err, &te, "128 KiB is over the cap")
	assert.Contains(t, te.Stderr(), "cannot allocate memory")
}

// A tool cannot exhaust the uid's tasks: RLIMIT_NPROC holds it to the
// uid's task count at its start plus toolThreadHeadroom (Ruling 18 item 2).
// A stand-in that starts toolThreadHeadroom + 16 threads, well within its 32
// GiB of address space, dies of a failed thread creation, and the bot can
// still start threads.
func TestMediaSandbox_ThreadCap(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	if taskCapUnbound(t) {
		t.Skip("RLIMIT_NPROC binds no root user: no cap to test (TestNewBot_VideoGate_TaskCapUnbound)")
	}
	allowUnsandboxedWhereNeeded(t)
	standInTool(t, "bigmem-sandbox-probe")
	_, err := runTool(context.Background(), "bigmem-sandbox-probe", inputDir(t), "", []string{"threads", strconv.Itoa(toolThreadHeadroom + 16)})
	var te *toolError
	require.ErrorAs(t, err, &te, "%d threads are over the cap", toolThreadHeadroom+16)
	assert.Regexp(t, `pthread_create failed|failed to create new OS thread`, te.Stderr())

	_, err = runTool(context.Background(), "bigmem-sandbox-probe", inputDir(t), "", []string{"threads", "8"})
	assert.NoError(t, err, "a tool's own handful of threads fits: %q", toolStderr(err))
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		close(done)
	}()
	<-done // the bot's own thread creation is untouched
}

// A tool may execute only itself and its ELF interpreter, and read only the
// library, font and fontconfig directories and the loader files: no longer
// the rest of /usr, such as another program (Ruling 16 item 6).
func TestMediaSandbox_NarrowGrants(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireLandlock(t)
	ffprobe, err := lookupTool("ffprobe")
	if err != nil {
		t.Skip("ffprobe not on PATH")
	}
	standInTool(t, "sandbox-probe")
	in := inputDir(t)
	ctx := context.Background()

	out, err := runTool(ctx, "cat", in, "", []string{ffprobe})
	requireDenied(t, out, err, "read another program")
	out, err = runTool(ctx, "sandbox-probe", in, "", []string{"exec", ffprobe})
	requireDenied(t, out, err, "exec another program")
	require.NoError(t, exec.Command(ffprobe, "-version").Run(), "outside the sandbox it runs") // #nosec G204 -- the system's ffprobe
}

// A sandboxed render works, font included, and gives exactly the JPEG of the
// same ffmpeg run without the sandbox. The font is outside every system
// directory, so it is readable only through its own rule.
func TestMediaSandbox_RenderSameAsUnsandboxed(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	requireLandlock(t)
	builtIn := resolveFontFile("")
	other := otherFont(builtIn)
	if builtIn == "" || other == "" {
		t.Skip("needs two built-in fonts")
	}
	font := copyFile(t, other, filepath.Join(t.TempDir(), "font.ttf"))
	clip := ffmpegFixture(t, mp4Args...)
	ctx := context.Background()
	probe, err := probeMedia(ctx, clip, "video")
	require.NoError(t, err, "%q", toolStderr(err))
	spec := sheetSpec{Start: 0, End: probe.Duration, Frames: 4, MaxEdge: 512, FontFile: font}
	sandboxed, _, err := renderSheet(ctx, clip, probe, spec)
	require.NoError(t, err, "%q", toolStderr(err))
	decodeSheet(t, sandboxed, 512)

	plan, err := planSheet(probe, spec)
	require.NoError(t, err)
	args := renderArgs(clip, plan, font, 2*int(sheetTimeout/time.Second))
	bin, err := lookupTool("ffmpeg")
	require.NoError(t, err)
	direct := exec.Command(bin, args...) // #nosec G204 -- the test's own render arguments
	direct.Dir = filepath.Dir(clip)
	plain, err := direct.Output()
	require.NoError(t, err)
	assert.True(t, bytes.Equal(plain, sandboxed), "the sandboxed render is byte for byte the unsandboxed one")

	// Without the font in the spec the same run cannot open it: it fails, or
	// drawtext falls back to fontconfig's default, which draws differently.
	out, err := runTool(ctx, "ffmpeg", filepath.Dir(clip), "", args)
	assert.True(t, err != nil || !bytes.Equal(out, sandboxed), "the font rule is what lets ffmpeg read the font")
}

// On Linux the helper's RLIMIT_CPU caps a run's CPU time, and ffmpeg gets
// no -timelimit (Ruling 19 item 7): its own setrlimit(RLIMIT_CPU, {lim,
// lim+1}) asked for a hard limit above the helper's and failed on every run,
// printing "setrlimit: Operation not permitted". A real render through the
// helper, stderr and all, has no setrlimit in it.
func TestMediaSandbox_RenderNoTimelimit(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	allowUnsandboxedWhereNeeded(t)
	clip := ffmpegFixture(t, gifArgs...)
	ctx := context.Background()
	probe, err := probeMedia(ctx, clip, "animation")
	require.NoError(t, err, "%q", toolStderr(err))
	plan, err := planSheet(probe, sheetSpec{Start: 0, End: probe.Duration, Frames: 4, MaxEdge: 512})
	require.NoError(t, err)
	args := renderArgs(clip, plan, resolveFontFile(""), 120)
	assert.NotContains(t, args, "-timelimit")
	bin, err := lookupTool("ffmpeg")
	require.NoError(t, err)
	cmd, err := newToolCmd(ctx, bin, filepath.Dir(clip), resolveFontFile(""), args)
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	require.NoError(t, cmd.Run(), "%q", stderr.String())
	assert.NotEmpty(t, stdout.Bytes())
	assert.NotContains(t, stderr.String(), "setrlimit")
}

// With no font file at all, drawtext asks fontconfig for its default, which
// reads its configuration and the font directories and tries to write and
// chmod its cache. That fallback still renders under the sandbox, the seccomp
// allowlist included (Ruling 18 item 1).
func TestMediaSandbox_NoFontFallback(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	requireLandlock(t)
	clip := ffmpegFixture(t, mp4Args...)
	ctx := context.Background()
	probe, err := probeMedia(ctx, clip, "video")
	require.NoError(t, err, "%q", toolStderr(err))
	plan, err := planSheet(probe, sheetSpec{Start: 0, End: probe.Duration, Frames: 4, MaxEdge: 512})
	require.NoError(t, err)
	out, err := runTool(ctx, "ffmpeg", filepath.Dir(clip), "", renderArgs(clip, plan, "", 120))
	require.NoError(t, err, "%q", toolStderr(err))
	decodeSheet(t, out, 512)
}

// On ABI 4 and later a sandboxed tool cannot open a TCP connection, not even
// to the loopback; the same stand-in connects fine outside the sandbox. The
// seccomp filter refuses the socket itself; without it ("noseccomp-")
// Landlock still refuses the connect.
func TestMediaSandbox_NoTCP(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	abi := requireLandlock(t)
	if abi < 4 {
		t.Skipf("Landlock ABI %d has no network rules; TCP rules came with ABI 4", abi)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	addr := ln.Addr().String()
	standIn := standInTool(t, "tcp-dial")
	standInTool(t, "noseccomp-tcp-dial")

	require.NoError(t, exec.Command(standIn, addr).Run(), "outside the sandbox the stand-in connects") // #nosec G204 -- the test binary
	require.Eventually(t, func() bool { return accepted.Load() == 1 }, 2*time.Second, 10*time.Millisecond)

	out, err := runTool(context.Background(), "tcp-dial", inputDir(t), "", []string{addr})
	requireDenied(t, out, err, "TCP connect")
	out, err = runTool(context.Background(), "noseccomp-tcp-dial", inputDir(t), "", []string{addr})
	requireDenied(t, out, err, "TCP connect, Landlock alone")
	assert.Contains(t, toolStderr(err), "permission denied", "Landlock's EACCES")
	time.Sleep(100 * time.Millisecond)
	assert.EqualValues(t, 1, accepted.Load(), "no connection from inside the sandbox reached the listener")
}

// procFile reads /proc/<pid>/<name> from the test process, outside the sandbox.
func procFile(t *testing.T, pid int, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), name)) // #nosec G304 -- a procfs path
	require.NoError(t, err)
	return string(data)
}

// startSandboxed starts tool with args through newToolCmd, as runTool would,
// and waits until the helper has exec'd it.
func startSandboxed(t *testing.T, tool string, args ...string) *exec.Cmd {
	t.Helper()
	cmd, err := newToolCmd(context.Background(), tool, inputDir(t), "", args)
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	require.Eventually(t, func() bool {
		cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "cmdline"))
		return err == nil && len(cmdline) > 0 && !bytes.Contains(cmdline, []byte(mediaSandboxArg))
	}, 5*time.Second, 5*time.Millisecond, "the helper execs the tool")
	return cmd
}

// limitFields returns the soft limit, hard limit and units of one line of /proc/<pid>/limits.
func limitFields(t *testing.T, limits, name string) []string {
	t.Helper()
	for line := range strings.SplitSeq(limits, "\n") {
		if rest, ok := strings.CutPrefix(line, name); ok {
			return strings.Fields(rest)
		}
	}
	t.Fatalf("no %q in %s", name, limits)
	return nil
}

// The limits reach the process that runs the tool. The test reads them from
// outside, in /proc/<pid>, since the sandbox gives the tool no /proc. The
// tool's PID is the one runTool started and also its process group: the
// helper execs in place, with no fork, setsid or setpgid (Ruling 14 note 1).
func TestMediaSandbox_Limits(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	allowUnsandboxedWhereNeeded(t) // the limits apply with or without Landlock
	sleep, err := lookupTool("sleep")
	require.NoError(t, err)
	cmd := startSandboxed(t, sleep, "30")
	pid := cmd.Process.Pid

	limits := procFile(t, pid, "limits")
	t.Log(limits)
	memory := strconv.Itoa(toolMemoryLimit)
	assert.Equal(t, []string{memory, memory, "bytes"}, limitFields(t, limits, "Max address space"), "soft and hard = toolMemoryLimit")
	assert.Equal(t, []string{"0", "0", "bytes"}, limitFields(t, limits, "Max core file size"))
	assert.Equal(t, []string{"0", "0", "bytes"}, limitFields(t, limits, "Max file size"))
	assert.Equal(t, []string{"64", "64", "files"}, limitFields(t, limits, "Max open files"), "Go's exec must not restore its saved limit")
	assert.Equal(t, []string{"65536", "65536", "bytes"}, limitFields(t, limits, "Max locked memory"), "RLIMIT_MEMLOCK: 64 KiB (Ruling 19 item 3)")
	cpu := strconv.Itoa(cpuTimeLimit(context.Background())) // twice the call's timeout (Ruling 16 item 2)
	assert.Equal(t, []string{cpu, cpu, "seconds"}, limitFields(t, limits, "Max cpu time"), "ffprobe has no -timelimit: RLIMIT_CPU covers both tools")
	tasks := limitFields(t, limits, "Max processes") // RLIMIT_NPROC: the user's tasks at the start plus toolThreadHeadroom (Ruling 18 item 2)
	require.Len(t, tasks, 3)
	assert.Equal(t, tasks[0], tasks[1], "soft and hard")
	n, err := strconv.Atoi(tasks[0])
	require.NoError(t, err, "a number, not unlimited")
	assert.Greater(t, n, toolThreadHeadroom)

	assert.Equal(t, "1000", strings.TrimSpace(procFile(t, pid, "oom_score_adj")), "the OOM killer's first choice")
	assert.Contains(t, procFile(t, pid, "status"), "NoNewPrivs:\t1")
	_, rest, ok := strings.Cut(procFile(t, pid, "stat"), ") ") // pid (comm) state ppid pgrp ...
	require.True(t, ok)
	fields := strings.Fields(rest)
	require.GreaterOrEqual(t, len(fields), 3)
	assert.Equal(t, strconv.Itoa(pid), fields[2], "the tool leads its own process group")
	assert.Equal(t, strconv.Itoa(os.Getpid()), fields[1], "and is still the bot's child")
}

// The helper is Go, and on glibc its runtime alone holds more address space
// than toolMemoryLimit (about 1.6 GiB on Ubuntu 24.04). The kernel refuses a
// process over its limit any new mapping, so with the limit set first a heap
// growth killed the helper now and then ("fatal error: runtime: out of
// memory", exit status 2). The limit now lands just before execve. Many runs,
// check and tool alike, must all get through.
func TestMediaSandbox_HelperFitsUnderTheLimit(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	allowUnsandboxedWhereNeeded(t)
	in := t.TempDir()
	inside := writeFake(t, in, "media", "inside")
	ctx := context.Background()
	for i := range 150 {
		_, err := sandboxSelfCheck(ctx, in, inside)
		require.NoError(t, err, "check %d: %q", i, toolStderr(err))
		out, err := runTool(ctx, "cat", in, "", []string{inside})
		require.NoError(t, err, "run %d: %q", i, toolStderr(err))
		require.Equal(t, "inside", string(out))
	}
}

// A tool that cannot be exec'd fails the run cleanly. execve's error comes
// after RLIMIT_AS, where the helper must allocate nothing, so it writes a
// message made beforehand and the errno and exits sandboxExitCode, never the
// Go runtime's "out of memory" (exit status 2) (review Minor 7).
func TestMediaSandbox_ExecFailsCleanly(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	notATool := writeFake(t, t.TempDir(), "not-a-tool", "plain text, mode 0600") // execve: EACCES
	spec := sandboxSpec{InputDir: dir, Tool: notATool, MemoryLimit: toolMemoryLimit, CPUSeconds: 60, AllowUnsandboxed: true}
	want := fmt.Sprintf("media sandbox: execve %s failed: errno %d\n", notATool, int(unix.EACCES))
	for i := range 50 {
		cmd := exec.Command(exe, mediaSandboxArg, spec.String(), "--", notATool) // #nosec G204 -- the test binary
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		cmd.Dir = dir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, "run %d", i)
		require.Equal(t, sandboxExitCode, exitErr.ExitCode(), "run %d: %q", i, stderr.String())
		require.Equal(t, want, stderr.String(), "run %d", i)
	}
}

// When the bot dies, the sandboxed tool dies with it: the helper re-arms
// Pdeathsig on the thread that execs (Ruling 14 note 2).
func TestMediaSandbox_ParentDeath(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	sleep, err := lookupTool("sleep")
	require.NoError(t, err)
	exe, err := os.Executable()
	require.NoError(t, err)
	parent := exec.Command(exe, sleep, inputDir(t), "300") // #nosec G204 -- the test binary
	parent.Args[0] = "sandbox-parent"
	parent.Env = []string{"PATH=" + os.Getenv("PATH")}
	stdout, err := parent.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	parent.Stderr = &stderr
	require.NoError(t, parent.Start())
	t.Cleanup(func() {
		_ = parent.Process.Kill()
		_ = parent.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err, "stand-in parent: %s", stderr.String())
	child, err := strconv.Atoi(strings.TrimSpace(line))
	require.NoError(t, err)
	t.Cleanup(func() {
		if !processGone(strconv.Itoa(child)) {
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
	})
	require.Eventually(t, func() bool {
		cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(child), "cmdline"))
		return err == nil && bytes.HasPrefix(cmdline, []byte(sleep+"\x00"))
	}, 5*time.Second, 5*time.Millisecond, "the helper execs sleep")

	require.NoError(t, parent.Process.Kill())
	_ = parent.Wait()
	assert.Eventually(t, func() bool { return processGone(strconv.Itoa(child)) }, 3*time.Second, 10*time.Millisecond,
		"the sandboxed tool %d died with its parent", child)
}

// A hard address-space limit below the cap is not clamped: the helper
// refuses to run (Ruling 14 note 3), and so does the gate. The helper here is
// asked for a 3 GiB cap under a 2.5 GiB hard limit: just below the real 1.5
// GiB cap, the Go runtime itself may fail to start on glibc (it takes about
// 1.6 GiB of address space), which proves nothing about the helper's check.
func TestMediaSandbox_HardLimitBelowTheCap(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	echo, err := lookupTool("echo")
	require.NoError(t, err)
	exe, err := os.Executable()
	require.NoError(t, err)
	const capBytes, hardBytes = 3 << 30, 5 << 29
	spec := sandboxSpec{InputDir: t.TempDir(), Tool: echo, MemoryLimit: capBytes, CPUSeconds: 60, AllowUnsandboxed: true}
	cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf(`ulimit -v %d && exec "$0" "$@"`, hardBytes>>10), // #nosec G204 -- the test binary
		exe, mediaSandboxArg, spec.String(), "--", echo, "ran")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "the helper must fail")
	assert.Equal(t, sandboxExitCode, exitErr.ExitCode())
	assert.Empty(t, out, "the tool never ran")
	assert.Contains(t, stderr.String(), "below")

	t.Run("gate", func(t *testing.T) {
		requireFFmpeg(t)
		var logged bytes.Buffer
		prev := InfoLogger
		InfoLogger = log.New(&logged, "", 0)
		t.Cleanup(func() { InfoLogger = prev })
		prevLimit := addressSpaceHardLimit
		addressSpaceHardLimit = func() (uint64, error) { return toolMemoryLimit - 1, nil }
		t.Cleanup(func() { addressSpaceHardLimit = prevLimit })
		b := videoGateBot(t, setupTestDB(t), &VideoConfig{AllowUnsandboxed: true})
		assert.False(t, b.videoOn())
		assert.Contains(t, logged.String(), "[video_gate_bot] video: ")
		assert.Contains(t, logged.String(), "address-space")
		assert.Contains(t, logged.String(), "video watching is off")
	})
}

// No sandbox rule may reach the bot's own files: a working directory,
// config/ or data/ beneath a directory every tool may read (the library,
// font and fontconfig directories) turns video off (review Minor 5). Since
// the grants were narrowed (Ruling 16 item 6), the rest of /usr is fine.
func TestSandboxSparesBot(t *testing.T) {
	prev := botDirs
	t.Cleanup(func() { botDirs = prev })
	use := func(dirs ...string) { botDirs = func() ([]string, error) { return dirs, nil } }

	wd := t.TempDir()
	use(wd, filepath.Join(wd, "config"), filepath.Join(wd, "data"))
	assert.NoError(t, sandboxSparesBot(), "a bot in a temp dir is outside every grant")
	own, err := prev()
	require.NoError(t, err)
	use(own...)
	assert.NoError(t, sandboxSparesBot(), "nor is this test's own working directory")

	for _, dirs := range [][]string{
		{"/usr/lib/bot", "/usr/lib/bot/config", "/usr/lib/bot/data"},
		{"/home/bot", "/home/bot/config", "/usr/share/fonts/bot-data"},
		{"/usr/local/lib/bot"},
		{"/etc/fonts/bot"},
	} {
		use(dirs...)
		assert.ErrorContains(t, sandboxSparesBot(), "which the ffmpeg sandbox lets every tool read", "%v", dirs)
	}
	use("/lib") // a link to /usr/lib on merged-/usr systems, a directory of its own elsewhere
	assert.Error(t, sandboxSparesBot())
	use("/usr/src/app", "/usr/src/app/config", "/usr/src/app/data")
	assert.NoError(t, sandboxSparesBot(), "the rest of /usr is no longer granted")

	// A data directory that links into a granted directory counts where it points.
	link := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.Symlink("/usr/share/fonts", link))
	use(t.TempDir(), link)
	assert.ErrorContains(t, sandboxSparesBot(), "beneath /usr/share/fonts")

	t.Run("gate", func(t *testing.T) {
		requireFFmpeg(t)
		var logged bytes.Buffer
		prevLog := InfoLogger
		InfoLogger = log.New(&logged, "", 0)
		t.Cleanup(func() { InfoLogger = prevLog })
		use("/usr/share/fonts/bot", "/usr/share/fonts/bot/config", "/usr/share/fonts/bot/data")
		b := videoGateBot(t, setupTestDB(t), &VideoConfig{AllowUnsandboxed: true})
		assert.False(t, b.videoOn())
		assert.Contains(t, logged.String(), "[video_gate_bot] video: the bot's directory /usr/share/fonts/bot is beneath /usr/share/fonts, "+
			"which the ffmpeg sandbox lets every tool read; video watching is off")
	})
}

// The input directory a run grants is never the bot's own: not its working
// directory, a link to it, config/ or data/, nor anything above or below
// them; and it holds exactly one entry, the input (Ruling 16 item 4). A fake
// repository stands in for the bot; the real config/ is never touched.
func TestNewSandboxSpec_InputDirGuard(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	allowUnsandboxedWhereNeeded(t)
	root := t.TempDir()
	writeFake(t, root, "config/bot.json", `{"telegram_token":"fake"}`)
	writeFake(t, root, "data/bot.db", "fake")
	video := writeFake(t, root, "data/videos/1/42/v1/video", "a stored video")
	prev := botDirs
	botDirs = func() ([]string, error) {
		return []string{root, filepath.Join(root, "config"), filepath.Join(root, "data")}, nil
	}
	t.Cleanup(func() { botDirs = prev })
	link := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.Symlink(root, link))
	spec := func(dir string) error {
		_, err := newSandboxSpec(dir, "", "/usr/bin/ffprobe", 20)
		return err
	}

	for name, dir := range map[string]string{
		"the working directory":   root,
		"a link to it":            link,
		"config/":                 filepath.Join(root, "config"),
		"data/":                   filepath.Join(root, "data"),
		"beneath data/":           filepath.Dir(video),
		"beneath the link":        filepath.Join(link, "data", "videos"),
		"above the bot":           filepath.Dir(root),
		"two levels above it":     filepath.Dir(filepath.Dir(root)),
		"beneath the working dir": filepath.Join(root, "config"),
	} {
		assert.ErrorContains(t, spec(dir), "the bot's directory", name)
	}

	empty := t.TempDir()
	assert.ErrorContains(t, spec(empty), "0 entries", "an empty directory")
	two := t.TempDir()
	writeFake(t, two, "media", "the input")
	writeFake(t, two, "other", "something else")
	assert.ErrorContains(t, spec(two), "2 entries")
	one := t.TempDir()
	writeFake(t, one, "media", "the input")
	assert.NoError(t, spec(one), "one input, nowhere near the bot")
}

// The spec is strict: anything malformed is refused before any exec.
func TestParseSandboxArgs(t *testing.T) {
	dir := t.TempDir()
	good := sandboxSpec{InputDir: dir, FontFile: "/usr/share/fonts/x.ttf", Tool: "/usr/bin/ffmpeg", MemoryLimit: toolMemoryLimit, CPUSeconds: 120}
	inv, err := parseSandboxArgs([]string{good.String(), "--", "/usr/bin/ffmpeg", "-version"})
	require.NoError(t, err)
	assert.Equal(t, good, inv.spec)
	assert.Equal(t, []string{"/usr/bin/ffmpeg", "-version"}, inv.argv)
	assert.False(t, inv.check)

	checkSpec := sandboxSpec{InputDir: dir, MemoryLimit: toolMemoryLimit, CPUSeconds: 60}
	inv, err = parseSandboxArgs([]string{checkSpec.String(), "--check", filepath.Join(dir, "media"), "/tmp/outside/file"})
	require.NoError(t, err)
	assert.True(t, inv.check)
	assert.Equal(t, filepath.Join(dir, "media"), inv.inside)
	assert.Equal(t, "/tmp/outside/file", inv.outside)

	with := func(edit func(*sandboxSpec)) string {
		s := good
		edit(&s)
		return s.String()
	}
	for name, args := range map[string][]string{
		"nothing":               {},
		"no tool":               {good.String()},
		"no separator":          {good.String(), "/usr/bin/ffmpeg"},
		"nothing after --":      {good.String(), "--"},
		"empty spec":            {"", "--", "/usr/bin/ffmpeg"},
		"not JSON":              {"input_dir=/tmp", "--", "/usr/bin/ffmpeg"},
		"null":                  {"null", "--", "/usr/bin/ffmpeg"},
		"unknown field":         {strings.Replace(good.String(), "{", `{"extra_dir":"/",`, 1), "--", "/usr/bin/ffmpeg"},
		"trailing data":         {good.String() + " {}", "--", "/usr/bin/ffmpeg"},
		"trailing junk":         {good.String() + "x", "--", "/usr/bin/ffmpeg"},
		"wrong type":            {strings.Replace(good.String(), strconv.Itoa(toolMemoryLimit), `"1"`, 1), "--", "/usr/bin/ffmpeg"},
		"other tool after --":   {good.String(), "--", "/bin/sh", "-c", "id"},
		"relative input":        {with(func(s *sandboxSpec) { s.InputDir = "data/videos" }), "--", "/usr/bin/ffmpeg"},
		"root input":            {with(func(s *sandboxSpec) { s.InputDir = "/" }), "--", "/usr/bin/ffmpeg"},
		"unclean input":         {with(func(s *sandboxSpec) { s.InputDir = dir + "/../.." }), "--", "/usr/bin/ffmpeg"},
		"empty input":           {with(func(s *sandboxSpec) { s.InputDir = "" }), "--", "/usr/bin/ffmpeg"},
		"NUL in input":          {with(func(s *sandboxSpec) { s.InputDir = dir + "\x00x" }), "--", "/usr/bin/ffmpeg"},
		"relative font":         {with(func(s *sandboxSpec) { s.FontFile = "fonts/x.ttf" }), "--", "/usr/bin/ffmpeg"},
		"relative tool":         {with(func(s *sandboxSpec) { s.Tool = "ffmpeg" }), "--", "ffmpeg"},
		"no tool in spec":       {with(func(s *sandboxSpec) { s.Tool = "" }), "--", "/usr/bin/ffmpeg"},
		"no memory limit":       {with(func(s *sandboxSpec) { s.MemoryLimit = 0 }), "--", "/usr/bin/ffmpeg"},
		"tiny memory limit":     {with(func(s *sandboxSpec) { s.MemoryLimit = 1 << 20 }), "--", "/usr/bin/ffmpeg"},
		"huge memory limit":     {with(func(s *sandboxSpec) { s.MemoryLimit = 1 << 50 }), "--", "/usr/bin/ffmpeg"},
		"no CPU limit":          {with(func(s *sandboxSpec) { s.CPUSeconds = 0 }), "--", "/usr/bin/ffmpeg"},
		"huge CPU limit":        {with(func(s *sandboxSpec) { s.CPUSeconds = 1 << 40 }), "--", "/usr/bin/ffmpeg"},
		"check with a tool":     {good.String(), "--check", filepath.Join(dir, "media"), "/tmp/outside/file"},
		"check, one path":       {checkSpec.String(), "--check", filepath.Join(dir, "media")},
		"check, inside outside": {checkSpec.String(), "--check", "/tmp/elsewhere/media", "/tmp/outside/file"},
		"check, outside inside": {checkSpec.String(), "--check", filepath.Join(dir, "media"), filepath.Join(dir, "other")},
		"unknown mode":          {good.String(), "--run", "/usr/bin/ffmpeg"},
	} {
		_, err := parseSandboxArgs(args)
		assert.Error(t, err, name)
	}
}

// A helper with a bad spec exits non-zero and never execs the tool.
func TestMediaSandbox_BadSpecNoExec(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	echo, err := lookupTool("echo")
	require.NoError(t, err)
	exe, err := os.Executable()
	require.NoError(t, err)
	good := sandboxSpec{InputDir: t.TempDir(), Tool: echo, MemoryLimit: toolMemoryLimit, CPUSeconds: 60, AllowUnsandboxed: true}
	for name, args := range map[string][]string{
		"not JSON":       {"{", "--", echo, "ran"},
		"tool mismatch":  {good.String(), "--", "/bin/sh", "-c", "echo ran"},
		"relative input": {strings.Replace(good.String(), good.InputDir, "rel", 1), "--", echo, "ran"},
	} {
		cmd := exec.Command(exe, append([]string{mediaSandboxArg}, args...)...) // #nosec G204 -- the test binary
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		out, err := cmd.Output()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, name)
		assert.Equal(t, sandboxExitCode, exitErr.ExitCode(), name)
		assert.NotContains(t, string(out), "ran", "%s: nothing was exec'd", name)
	}

	// The same helper with the good spec does exec the tool.
	cmd := exec.Command(exe, mediaSandboxArg, good.String(), "--", echo, "ran") // #nosec G204 -- the test binary
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.Dir = good.InputDir
	out, err := cmd.Output()
	require.NoError(t, err)
	assert.Equal(t, "ran\n", string(out))
}

// The access rights handled at each ABI: every right this code knows, up to
// ABI 10, so whatever no rule grants is denied. ABI 11 and 12 add nothing
// here yet (later work), so they get the ABI 10 set.
func TestLandlockAccess(t *testing.T) {
	base := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	tcp := uint64(unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP)
	scopes := uint64(unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL)
	abi2 := base | unix.LANDLOCK_ACCESS_FS_REFER
	abi3 := abi2 | unix.LANDLOCK_ACCESS_FS_TRUNCATE
	abi5 := abi3 | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	abi9 := abi5 | 1<<16     // LANDLOCK_ACCESS_FS_RESOLVE_UNIX
	udp := tcp | 1<<2 | 1<<3 // LANDLOCK_ACCESS_NET_BIND_UDP, _CONNECT_SEND_UDP
	for _, tc := range []struct {
		abi                 int
		fs, network, scoped uint64
	}{
		{1, base, 0, 0},
		{2, abi2, 0, 0},
		{3, abi3, 0, 0},
		{4, abi3, tcp, 0},
		{5, abi5, tcp, 0},
		{6, abi5, tcp, scopes},
		{7, abi5, tcp, scopes},
		{8, abi5, tcp, scopes},
		{9, abi9, tcp, scopes},
		{10, abi9, udp, scopes},
		{11, abi9, udp, scopes},
		{12, abi9, udp, scopes},
	} {
		got := landlockAccess(tc.abi)
		assert.Equal(t, unix.LandlockRulesetAttr{Access_fs: tc.fs, Access_net: tc.network, Scoped: tc.scoped}, got, "ABI %d", tc.abi)
	}
}

// The policy itself (Ruling 16 item 6): read only beneath the library, font
// and fontconfig directories, on the loaders' two /etc files, on /dev/null
// and /dev/urandom, beneath the input directory and on the font; read and
// execute only on the tool and its ELF interpreter. Nothing grants writing,
// and nothing grants the rest of /usr.
func TestSandboxRules(t *testing.T) {
	prev := sandboxTestRules
	sandboxTestRules = nil
	t.Cleanup(func() { sandboxTestRules = prev })
	spec := sandboxSpec{InputDir: "/tmp/video-job-1", FontFile: "/opt/fonts/f.ttf", Tool: "/usr/bin/ffmpeg", MemoryLimit: toolMemoryLimit}
	const interp = "/lib/ld-musl-x86_64.so.1"
	assert.Equal(t, []sandboxRule{
		{path: "/lib", access: fsRead, optional: true},
		{path: "/lib64", access: fsRead, optional: true},
		{path: "/usr/lib", access: fsRead, optional: true},
		{path: "/usr/lib64", access: fsRead, optional: true},
		{path: "/usr/local/lib", access: fsRead, optional: true},
		{path: "/usr/share/fonts", access: fsRead, optional: true},
		{path: "/usr/share/fontconfig", access: fsRead, optional: true},
		{path: "/etc/fonts", access: fsRead, optional: true},
		{path: "/etc/ld.so.cache", access: fsReadFile, optional: true},
		{path: "/etc/ld-musl-*.path", access: fsReadFile, optional: true, glob: true},
		{path: "/dev/null", access: fsReadFile, optional: true},
		{path: "/dev/urandom", access: fsReadFile, optional: true},
		{path: "/tmp/video-job-1", access: fsRead, kind: mustBeDir},
		{path: "/opt/fonts/f.ttf", access: fsReadFile, kind: mustBeFile},
		{path: "/usr/bin/ffmpeg", access: fsReadExec, kind: mustBeFile},
		{path: interp, access: fsReadExec, kind: mustBeFile},
	}, sandboxRules(spec, interp))
	for _, r := range sandboxRules(spec, interp) {
		assert.Zero(t, r.access&^(fsReadExec), "%s: read and execute rights only", r.path)
		if r.access&unix.LANDLOCK_ACCESS_FS_EXECUTE != 0 {
			assert.Contains(t, []string{spec.Tool, interp}, r.path, "only the tool and its interpreter execute")
		}
	}
	noFont := spec
	noFont.FontFile = ""
	assert.Len(t, sandboxRules(noFont, interp), len(sandboxRules(spec, interp))-1, "no font, no font rule")
	assert.Len(t, sandboxRules(spec, ""), len(sandboxRules(spec, interp))-1, "a static tool, no interpreter rule")
}

// The interpreter rule comes from the tool's own PT_INTERP header.
func TestElfInterpreter(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	interp, err := elfInterpreter(exe)
	require.NoError(t, err)
	t.Logf("the test binary's interpreter: %q", interp)
	if interp != "" { // cgo builds are dynamically linked
		assert.True(t, filepath.IsAbs(interp))
		_, err := os.Stat(interp)
		assert.NoError(t, err)
	}
	script := writeFake(t, t.TempDir(), "script", "#!/bin/sh\n")
	_, err = elfInterpreter(script)
	assert.Error(t, err, "a script is no ELF file")
}

// The checks the self-test runs inside the sandbox report a missing one: run
// here, in the unsandboxed test process, every one of them fails.
func TestCheckSandboxed_ReportsNoSandbox(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	in := t.TempDir()
	inside := writeFake(t, in, "media", "inside")
	outside := writeFake(t, t.TempDir(), "file", "outside")
	err := checkSandboxed(in, inside, outside, sandboxReport{ABI: 4, Seccomp: true})
	require.Error(t, err)
	for _, want := range []string{"read " + outside, "created a file in " + in, "created a file in " + filepath.Dir(outside), "TCP",
		"making a unix socket worked", "signalling the bot worked", "reading the bot's limits worked", "F_SETOWN on stderr worked",
		"shmget worked", "chmod outside the input directory worked", "inotify_init1 worked"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.NoFileExists(t, filepath.Join(in, sandboxProbeName()), "the check cleans up after itself")

	missing := checkSandboxed(in, filepath.Join(in, "missing"), outside, sandboxReport{ABI: 1})
	assert.ErrorContains(t, missing, "cannot read "+filepath.Join(in, "missing"))
}

// The check tests only the layers the report says are there (Ruling 18
// item 3): a host without Landlock, under video.allow_unsandboxed, still
// passes with the seccomp filter alone, and with neither layer there is
// nothing to check. Run here, unsandboxed, a probe of a missing layer would
// fail.
func TestCheckSandboxed_OnlyPresentLayers(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	in := t.TempDir()
	inside := writeFake(t, in, "media", "inside")
	outside := writeFake(t, t.TempDir(), "file", "outside")
	assert.NoError(t, checkSandboxed(in, inside, outside, sandboxReport{}), "no layer, no check")

	err := checkSandboxed(in, inside, outside, sandboxReport{Seccomp: true})
	require.Error(t, err, "the seccomp probes run, and fail out here")
	for _, landlock := range []string{"read " + outside, "created a file", "TCP"} {
		assert.NotContains(t, err.Error(), landlock, "no Landlock, no file or TCP check")
	}

	err = checkSandboxed(in, inside, outside, sandboxReport{ABI: 7})
	require.Error(t, err, "the Landlock checks run, and fail out here")
	assert.NotContains(t, err.Error(), "signalling the bot", "no seccomp filter, no seccomp probe")
}

// The self-test reports Landlock as on, with the kernel's ABI, when it is.
func TestToolSelfTest_Sandboxed(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	abi := requireLandlock(t)
	report, err := toolSelfTest(context.Background(), "", true)
	if taskCapUnbound(t) {
		require.ErrorIs(t, err, errTaskCapUnbound, "as root the sandbox is not whole")
		assert.False(t, report.TaskCap)
		return
	}
	require.NoError(t, err, "%q", toolStderr(err))
	assert.Equal(t, sandboxReport{ABI: abi, OOMScoreAdj: true, Seccomp: true, TaskCap: true, NoCapabilities: true}, report)
}

// The helper's check mode fails, and with it the self-test, if Landlock does
// not end up applied although the gate found it.
func TestSandboxSelfCheck_Report(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	allowUnsandboxedWhereNeeded(t)
	in := t.TempDir()
	inside := writeFake(t, in, "media", "inside")
	report, err := sandboxSelfCheck(context.Background(), in, inside)
	require.NoError(t, err, "%q", toolStderr(err))
	abi, lerr := landlockProbe()
	if lerr != nil {
		abi = 0
	}
	assert.Equal(t, abi, report.ABI)
	assert.True(t, report.OOMScoreAdj)
	assert.Equal(t, seccompProbe() == nil, report.Seccomp, "the filter is installed wherever the kernel can run it")
	assert.True(t, report.NoCapabilities, "every capability set is empty after the drop (Ruling 19 item 4)")
	assert.Equal(t, !taskCapUnbound(t), report.TaskCap, "the task cap binds every user but root")
}

// On Linux, newToolCmd runs the tool through the helper: the bot's own binary
// with the spec, then the tool. The child keeps Pdeathsig, leads its own
// process group, and a timeout kills that group.
func TestNewToolCmd_Linux(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	allowUnsandboxedWhereNeeded(t)
	dir := inputDir(t)
	cmd, err := newToolCmd(context.Background(), "/abs/ffmpeg", dir, "/fonts/f.ttf", []string{"-version"})
	require.NoError(t, err)
	assert.True(t, toolMemoryCapped)
	assert.Equal(t, "/proc/self/exe", cmd.Path)
	require.Len(t, cmd.Args, 6)
	assert.Equal(t, []string{"/proc/self/exe", mediaSandboxArg}, cmd.Args[:2])
	assert.Equal(t, []string{"--", "/abs/ffmpeg", "-version"}, cmd.Args[3:])
	inv, err := parseSandboxArgs(cmd.Args[2:])
	require.NoError(t, err)
	noSandbox := sandboxUsable(sandboxAvailable())
	assert.Equal(t, sandboxSpec{InputDir: dir, FontFile: "/fonts/f.ttf", Tool: "/abs/ffmpeg", MemoryLimit: toolMemoryLimit, CPUSeconds: 120,
		AllowUnsandboxed: noSandbox != nil || taskCapUnbound(t)}, inv.spec)
	require.NotNil(t, cmd.SysProcAttr)
	assert.Equal(t, syscall.SIGKILL, cmd.SysProcAttr.Pdeathsig, "SIGKILL when the bot dies")
	assert.True(t, cmd.SysProcAttr.Setpgid, "its own process group (Ruling 12 item 5)")
	assert.NotNil(t, cmd.Cancel, "a timeout kills the whole group")

	// RLIMIT_CPU is twice the call's timeout, rounded up (Ruling 16 item 2).
	ctx, cancel := context.WithTimeout(context.Background(), 9500*time.Millisecond)
	defer cancel()
	cmd, err = newToolCmd(ctx, "/abs/ffprobe", dir, "", nil)
	require.NoError(t, err)
	inv, err = parseSandboxArgs(cmd.Args[2:])
	require.NoError(t, err)
	assert.EqualValues(t, 20, inv.spec.CPUSeconds)

	_, err = newToolCmd(context.Background(), "/abs/ffmpeg", "", "", nil)
	assert.Error(t, err, "no input directory, no run: the bot's own working directory is never granted")
}

// The gate, with real tools: Landlock here means video on and the ABI logged.
func TestNewBot_VideoGateSandboxOn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	abi := requireLandlock(t)
	if taskCapUnbound(t) {
		t.Skip("as root the sandbox is not whole: TestNewBot_VideoGate_TaskCapUnbound")
	}
	var logged bytes.Buffer
	prev := InfoLogger
	InfoLogger = log.New(&logged, "", 0)
	t.Cleanup(func() { InfoLogger = prev })
	b := videoGateBot(t, setupTestDB(t), &VideoConfig{})
	assert.True(t, b.videoOn())
	assert.Contains(t, logged.String(), fmt.Sprintf("[video_gate_bot] video: ffmpeg sandbox on (Landlock ABI %d, seccomp filter)", abi))
	assert.Contains(t, logged.String(), "[video_gate_bot] video: the sandbox does not cover "+sandboxGaps(abi, true)+"\n",
		"with the filter only what neither layer covers is named, once")
	assert.Contains(t, logged.String(), "file metadata (stat: whether a path exists, sizes, times); the parent-death signal, which a thread the tool starts drops by exec",
		"the thread-exec residual (Ruling 19 item 1)")
	assert.Contains(t, logged.String(), "ends only with Docker's PID namespace or a systemd unit's control-group kill\n", "and its backstop")
	assert.Equal(t, 1, strings.Count(logged.String(), "does not cover"))
	assert.NotContains(t, logged.String(), "oom_score_adj")
}

// The Landlock ABI the helper applies (Ruling 15): the kernel's own, but an
// ABI below minSandboxABI is no sandbox and needs the spec's allowance, and
// so does no Landlock at all. With the allowance a weak ABI still applies
// what it can deny; a Landlock that fails otherwise is never skipped.
func TestSandboxABI(t *testing.T) {
	noLandlock := fmt.Errorf("%w: landlock_create_ruleset: %w", errNoLandlock, unix.ENOSYS)
	broken := errors.New("landlock_create_ruleset: invalid argument")
	for _, tc := range []struct {
		abi     int
		err     error
		allow   bool
		want    int
		wantErr string
	}{
		{7, nil, false, 7, ""},
		{3, nil, false, 3, ""},
		{7, nil, true, 7, ""},
		{2, nil, false, 0, "ABI 2 cannot deny file truncation"},
		{1, nil, false, 0, "ABI 1 cannot deny file truncation"},
		{2, nil, true, 2, ""},
		{1, nil, true, 1, ""},
		{0, noLandlock, true, 0, ""},
		{0, noLandlock, false, 0, "no Landlock"},
		{0, broken, true, 0, "invalid argument"},
	} {
		got, err := sandboxABI(tc.abi, tc.err, tc.allow)
		if tc.wantErr != "" {
			assert.ErrorContains(t, err, tc.wantErr, "%+v", tc)
			continue
		}
		assert.NoError(t, err, "%+v", tc)
		assert.Equal(t, tc.want, got, "%+v", tc)
	}
}
