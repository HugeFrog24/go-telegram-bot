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
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func init() {
	// Test-only rule (Ruling 14 note 6): the shell stand-ins in the tests are
	// scripts run by /bin/sh. On Alpine that is /bin/busybox, outside /usr and
	// /lib, so the sandbox would refuse to execute it. Production code never
	// sets sandboxTestRules; real ffmpeg needs no shell.
	if sh, err := filepath.EvalSymlinks("/bin/sh"); err == nil {
		sandboxTestRules = []sandboxRule{{path: sh, access: fsReadExec}}
	}
	// Every helper the tests start works on a thread that did not inherit
	// Pdeathsig, so the parent-death test proves the re-arm (review Minor 3).
	sandboxFreshThread = true
	testStandIns["tcp-dial"] = tcpDialStandIn
	testStandIns["sandbox-parent"] = sandboxParentStandIn
	testStandIns["truncate-path"] = truncatePathStandIn
}

// requireLandlock skips the test where the kernel (or a seccomp profile)
// offers no Landlock that counts as a sandbox (ABI minSandboxABI or later),
// and returns the ABI otherwise.
func requireLandlock(t *testing.T) int {
	t.Helper()
	abi, err := sandboxAvailable()
	if err := sandboxUsable(abi, err); err != nil {
		t.Skipf("no sandbox here: %v", err)
	}
	t.Logf("Landlock ABI %d", abi)
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

// requireDenied checks that a sandboxed run failed with EACCES: a non-zero exit
// whose stderr says "Permission denied", never "No such file".
func requireDenied(t *testing.T, out []byte, err error, what string) {
	t.Helper()
	var te *toolError
	require.ErrorAs(t, err, &te, "%s: the run must fail", what)
	assert.Empty(t, out, what)
	assert.Contains(t, strings.ToLower(te.Stderr()), "permission denied", what)
}

// The sandbox lets a tool read inside its input directory and nothing else
// of the bot's: not another video, not another chat's, not the database, not
// config/. It can create, change or delete nothing. Everything here is a fake
// repository in a temp dir; the real config/ is never touched.
func TestMediaSandbox_FileAccess(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireLandlock(t)
	root := t.TempDir() // stands in for the bot's working directory
	token := writeFake(t, root, "config/bot.json", `{"telegram_token":"fake-token-for-the-sandbox-test"}`)
	db := writeFake(t, root, "data/bot.db", "fake chat history")
	video := writeFake(t, root, "data/videos/1/42/v1/video", "this chat's video")
	sibling := writeFake(t, root, "data/videos/1/42/v2/video", "another video of this chat")
	otherChat := writeFake(t, root, "data/videos/1/43/v1/video", "another chat's video")
	otherBot := writeFake(t, root, "data/videos/2/42/v1/video", "another bot's video")
	in := filepath.Dir(video)
	t.Chdir(root)
	ctx := context.Background()

	out, err := runTool(ctx, "cat", in, "", []string{video})
	require.NoError(t, err, "%q", toolStderr(err))
	assert.Equal(t, "this chat's video", string(out))
	out, err = runTool(ctx, "cat", in, "", []string{"video"})
	require.NoError(t, err, "relative to the tool's working directory, the input directory")
	assert.Equal(t, "this chat's video", string(out))

	for _, p := range []string{sibling, otherChat, otherBot, db, token, "../v2/video", "../../../../../config/bot.json"} {
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
	// right does. An ABI 1 or 2 kernel would let it empty the database.
	standInTool(t, "truncate-path")
	for _, p := range []string{db, video} {
		out, err := runTool(ctx, "truncate", in, "", []string{"-s", "0", p})
		requireDenied(t, out, err, "truncate -s 0 "+p)
		out, err = runTool(ctx, "truncate-path", in, "", []string{p})
		requireDenied(t, out, err, "truncate(2) "+p)
	}
	data, err := os.ReadFile(db) // #nosec G304 -- the test's own fixture
	require.NoError(t, err)
	assert.Equal(t, "fake chat history", string(data), "the database is unchanged")
	data, err = os.ReadFile(video) // #nosec G304 -- the test's own fixture
	require.NoError(t, err)
	assert.Equal(t, "this chat's video", string(data), "the input is unchanged")
	assert.FileExists(t, token, "the fake config is still there")
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

// On ABI 4 and later a sandboxed tool cannot open a TCP connection, not even
// to the loopback; the same stand-in connects fine outside the sandbox.
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

	require.NoError(t, exec.Command(standIn, addr).Run(), "outside the sandbox the stand-in connects") // #nosec G204 -- the test binary
	require.Eventually(t, func() bool { return accepted.Load() == 1 }, 2*time.Second, 10*time.Millisecond)

	out, err := runTool(context.Background(), "tcp-dial", t.TempDir(), "", []string{addr})
	requireDenied(t, out, err, "TCP connect")
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
	cmd, err := newToolCmd(context.Background(), tool, t.TempDir(), "", args)
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
	cpu := strconv.Itoa(cpuTimeLimit(context.Background())) // twice the call's timeout (Ruling 16 item 2)
	assert.Equal(t, []string{cpu, cpu, "seconds"}, limitFields(t, limits, "Max cpu time"), "ffprobe has no -timelimit: RLIMIT_CPU covers both tools")

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
	parent := exec.Command(exe, sleep, t.TempDir(), "300") // #nosec G204 -- the test binary
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
// config/ or data/ beneath a directory every tool may read (/usr, /lib,
// /lib64) turns video off (review Minor 5).
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
		{"/usr/src/app", "/usr/src/app/config", "/usr/src/app/data"},
		{"/home/bot", "/home/bot/config", "/usr/local/share/bot-data"},
		{"/usr"},
	} {
		use(dirs...)
		assert.ErrorContains(t, sandboxSparesBot(), "beneath /usr", "%v", dirs)
	}
	use("/lib") // a link into /usr/lib on merged-/usr systems, a directory of its own elsewhere
	assert.Error(t, sandboxSparesBot())

	// A data directory that links into /usr counts where it points.
	link := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.Symlink("/usr/share", link))
	use(t.TempDir(), link)
	assert.ErrorContains(t, sandboxSparesBot(), "beneath /usr")

	t.Run("gate", func(t *testing.T) {
		requireFFmpeg(t)
		var logged bytes.Buffer
		prevLog := InfoLogger
		InfoLogger = log.New(&logged, "", 0)
		t.Cleanup(func() { InfoLogger = prevLog })
		use("/usr/src/app", "/usr/src/app/config", "/usr/src/app/data")
		b := videoGateBot(t, setupTestDB(t), &VideoConfig{AllowUnsandboxed: true})
		assert.False(t, b.videoOn())
		assert.Contains(t, logged.String(), "[video_gate_bot] video: the bot's directory /usr/src/app is beneath /usr, "+
			"which the ffmpeg sandbox lets every tool read; video watching is off")
	})
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

// The policy itself: read and execute beneath the system library and binary
// directories; read only on the loaders' two /etc files, on /dev/null and
// /dev/urandom, beneath the input directory and on the font; read and execute
// on the tool. Nothing grants writing.
func TestSandboxRules(t *testing.T) {
	prev := sandboxTestRules
	sandboxTestRules = nil
	t.Cleanup(func() { sandboxTestRules = prev })
	spec := sandboxSpec{InputDir: "/data/videos/1/2/v3", FontFile: "/opt/fonts/f.ttf", Tool: "/usr/bin/ffmpeg", MemoryLimit: toolMemoryLimit}
	assert.Equal(t, []sandboxRule{
		{path: "/usr", access: fsReadExec, optional: true},
		{path: "/lib", access: fsReadExec, optional: true},
		{path: "/lib64", access: fsReadExec, optional: true},
		{path: "/etc/ld.so.cache", access: fsReadFile, optional: true},
		{path: "/etc/ld-musl-*.path", access: fsReadFile, optional: true, glob: true},
		{path: "/dev/null", access: fsReadFile, optional: true},
		{path: "/dev/urandom", access: fsReadFile, optional: true},
		{path: "/data/videos/1/2/v3", access: fsRead, kind: mustBeDir},
		{path: "/opt/fonts/f.ttf", access: fsReadFile, kind: mustBeFile},
		{path: "/usr/bin/ffmpeg", access: fsReadExec, kind: mustBeFile},
	}, sandboxRules(spec))
	for _, r := range sandboxRules(spec) {
		assert.Zero(t, r.access&^(fsReadExec), "%s: read and execute rights only", r.path)
	}
	noFont := spec
	noFont.FontFile = ""
	assert.Len(t, sandboxRules(noFont), len(sandboxRules(spec))-1, "no font, no font rule")
}

// The checks the self-test runs inside the sandbox report a missing one: run
// here, in the unsandboxed test process, every one of them fails.
func TestCheckSandboxed_ReportsNoSandbox(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	in := t.TempDir()
	inside := writeFake(t, in, "media", "inside")
	outside := writeFake(t, t.TempDir(), "file", "outside")
	err := checkSandboxed(in, inside, outside, 4)
	require.Error(t, err)
	for _, want := range []string{"read " + outside, "created a file in " + in, "created a file in " + filepath.Dir(outside), "TCP"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.NoFileExists(t, filepath.Join(in, sandboxProbeName()), "the check cleans up after itself")

	missing := checkSandboxed(in, filepath.Join(in, "missing"), outside, 1)
	assert.ErrorContains(t, missing, "cannot read "+filepath.Join(in, "missing"))
}

// The self-test reports Landlock as on, with the kernel's ABI, when it is.
func TestToolSelfTest_Sandboxed(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	abi := requireLandlock(t)
	report, err := toolSelfTest(context.Background(), "", true)
	require.NoError(t, err, "%q", toolStderr(err))
	assert.Equal(t, sandboxReport{ABI: abi, OOMScoreAdj: true}, report)
}

// The helper's check mode fails, and with it the self-test, if Landlock does
// not end up applied although the gate found it.
func TestSandboxSelfCheck_Report(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	allowUnsandboxedWhereNeeded(t)
	in := t.TempDir()
	inside := writeFake(t, in, "media", "inside")
	report, err := sandboxSelfCheck(context.Background(), in, inside)
	require.NoError(t, err, "%q", toolStderr(err))
	abi, lerr := sandboxAvailable()
	if lerr != nil {
		abi = 0
	}
	assert.Equal(t, abi, report.ABI)
	assert.True(t, report.OOMScoreAdj)
}

// On Linux, newToolCmd runs the tool through the helper: the bot's own binary
// with the spec, then the tool. The child keeps Pdeathsig, leads its own
// process group, and a timeout kills that group.
func TestNewToolCmd_Linux(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	allowUnsandboxedWhereNeeded(t)
	dir := t.TempDir()
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
		AllowUnsandboxed: noSandbox != nil}, inv.spec)
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
	var logged bytes.Buffer
	prev := InfoLogger
	InfoLogger = log.New(&logged, "", 0)
	t.Cleanup(func() { InfoLogger = prev })
	b := videoGateBot(t, setupTestDB(t), &VideoConfig{})
	assert.True(t, b.videoOn())
	assert.Contains(t, logged.String(), fmt.Sprintf("[video_gate_bot] video: ffmpeg sandbox on (Landlock ABI %d)", abi))
	if gaps := sandboxGaps(abi); gaps != "" {
		assert.Contains(t, logged.String(), fmt.Sprintf("[video_gate_bot] video: Landlock ABI %d cannot deny %s\n", abi, gaps),
			"what this ABI cannot deny is logged once")
		assert.Equal(t, 1, strings.Count(logged.String(), "cannot deny"))
	}
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
