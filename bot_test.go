package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestContentBlocksForMessage(t *testing.T) {
	t.Run("empty message yields no blocks", func(t *testing.T) {
		blocks := contentBlocksForMessage(Message{IsUser: true})
		assert.Empty(t, blocks)
	})

	t.Run("user text only yields one text block", func(t *testing.T) {
		blocks := contentBlocksForMessage(Message{IsUser: true, Text: "hello"})
		assert.Len(t, blocks, 1)
		assert.NotNil(t, blocks[0].OfText)
		assert.Equal(t, "hello", blocks[0].OfText.Text)
	})

	t.Run("user single image without caption — no label, no text", func(t *testing.T) {
		blocks := contentBlocksForMessage(Message{
			IsUser:       true,
			ImageFileIDs: []string{"file_solo"},
		})
		assert.Len(t, blocks, 1)
		assert.NotNil(t, blocks[0].OfImage)
		assert.NotNil(t, blocks[0].OfImage.Source.OfFile)
		assert.Equal(t, "file_solo", blocks[0].OfImage.Source.OfFile.FileID)
	})

	t.Run("user single image with caption — image before text", func(t *testing.T) {
		blocks := contentBlocksForMessage(Message{
			IsUser:       true,
			Text:         "is this right?",
			ImageFileIDs: []string{"file_solo"},
		})
		assert.Len(t, blocks, 2)
		assert.NotNil(t, blocks[0].OfImage, "image block must come before text per Anthropic guidance")
		assert.Equal(t, "file_solo", blocks[0].OfImage.Source.OfFile.FileID)
		assert.NotNil(t, blocks[1].OfText)
		assert.Equal(t, "is this right?", blocks[1].OfText.Text)
	})

	t.Run("user album (multi-image) labels each with Image N:", func(t *testing.T) {
		blocks := contentBlocksForMessage(Message{
			IsUser:       true,
			Text:         "compare these",
			ImageFileIDs: []string{"file_a", "file_b", "file_c"},
		})
		assert.Len(t, blocks, 7)
		assert.Equal(t, "Image 1:", blocks[0].OfText.Text)
		assert.Equal(t, "file_a", blocks[1].OfImage.Source.OfFile.FileID)
		assert.Equal(t, "Image 2:", blocks[2].OfText.Text)
		assert.Equal(t, "file_b", blocks[3].OfImage.Source.OfFile.FileID)
		assert.Equal(t, "Image 3:", blocks[4].OfText.Text)
		assert.Equal(t, "file_c", blocks[5].OfImage.Source.OfFile.FileID)
		assert.Equal(t, "compare these", blocks[6].OfText.Text)
	})

	t.Run("assistant message with images-set is text-only (defensive)", func(t *testing.T) {
		blocks := contentBlocksForMessage(Message{
			IsUser:       false,
			Text:         "I see your screenshot",
			ImageFileIDs: []string{"file_should_be_ignored"},
		})
		assert.Len(t, blocks, 1)
		assert.NotNil(t, blocks[0].OfText)
		assert.Equal(t, "I see your screenshot", blocks[0].OfText.Text)
	})

	t.Run("whitespace-only text is skipped but images survive", func(t *testing.T) {
		blocks := contentBlocksForMessage(Message{
			IsUser:       true,
			Text:         "   \n  ",
			ImageFileIDs: []string{"file_x"},
		})
		assert.Len(t, blocks, 1)
		assert.NotNil(t, blocks[0].OfImage)
	})
}

func TestStripDeadFileIDFromMemory(t *testing.T) {
	b, _ := setupBotForTest(t, 100)
	chatID := int64(42)

	cm := b.getOrCreateChatMemory(chatID)
	cm.Messages = []Message{
		{IsUser: true, Text: "first", ImageFileIDs: []string{"file_a", "file_b"}},
		{IsUser: false, Text: "reply"},
		{IsUser: true, Text: "third", ImageFileIDs: []string{"file_b", "file_c"}},
	}

	b.stripDeadFileIDFromMemory(chatID, "file_b")

	assert.Equal(t, []string{"file_a"}, cm.Messages[0].ImageFileIDs, "file_b should be removed from message 1")
	assert.Empty(t, cm.Messages[1].ImageFileIDs, "assistant message untouched")
	assert.Equal(t, []string{"file_c"}, cm.Messages[2].ImageFileIDs, "file_b should be removed from message 3")
}

func TestStripDeadFileIDFromMemory_UnknownChatIsNoop(t *testing.T) {
	b, _ := setupBotForTest(t, 100)
	b.stripDeadFileIDFromMemory(99999, "file_anything")
}

// videoGateBot builds a Bot around the given video block. The database is
// created by the caller so a test can change PATH after it and before NewBot.
// The gate may let tools run unsandboxed for the process; that is undone
// when the test ends.
func videoGateBot(t *testing.T, db *gorm.DB, video *VideoConfig) *Bot {
	t.Helper()
	prev := unsandboxedGrants.Load()
	t.Cleanup(func() { unsandboxedGrants.Store(prev) })
	b, err := NewBot(db, BotConfig{
		ID:              "video_gate_bot",
		OwnerTelegramID: 1,
		TelegramToken:   "test_token",
		MemorySize:      10,
		MessagePerHour:  5,
		MessagePerDay:   10,
		Model:           "claude-haiku-4-5",
		Active:          true,
		Video:           video,
	}, &MockClock{currentTime: time.Now()}, &MockTelegramClient{})
	assert.NoError(t, err)
	return b
}

// stubGate replaces the gate's Landlock probe and, when selfTest is not nil,
// its self-test for one test.
func stubGate(t *testing.T, probe func() (int, error), selfTest func(context.Context, string, bool) (sandboxReport, error)) {
	t.Helper()
	prevProbe, prevSelfTest := gateSandbox, gateSelfTest
	t.Cleanup(func() { gateSandbox, gateSelfTest = prevProbe, prevSelfTest })
	gateSandbox = probe
	if selfTest != nil {
		gateSelfTest = selfTest
	}
}

// assertSandboxLines checks that a gate which turned video on logged the
// sandbox's layers (layers, a prefix of that line) and its gaps; off Linux,
// that there is no sandbox (Ruling 19 item 8).
func assertSandboxLines(t *testing.T, logged, layers string) {
	t.Helper()
	if !sandboxHelper {
		assert.Contains(t, logged, "[video_gate_bot] video: there is no sandbox on this OS: ffmpeg can do anything the bot's user can")
		return
	}
	assert.Contains(t, logged, layers)
	assert.Contains(t, logged, "[video_gate_bot] video: the sandbox does not cover ")
}

// onPath skips the test unless ffmpeg and ffprobe are on PATH. Unlike
// requireFFmpeg it leaves the unsandboxed switch alone: the gate sets it.
func onPath(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
}

func TestNewBot_VideoGate(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	var logged, warned bytes.Buffer
	prev, prevErr := InfoLogger, ErrorLogger
	InfoLogger, ErrorLogger = log.New(&logged, "", 0), log.New(&warned, "", 0)
	t.Cleanup(func() { InfoLogger, ErrorLogger = prev, prevErr })
	reset := func() {
		logged.Reset()
		warned.Reset()
	}

	t.Run("no video block: off", func(t *testing.T) {
		b := videoGateBot(t, setupTestDB(t), nil)
		assert.False(t, b.videoOn())
		assert.Nil(t, b.video)
		assert.NotContains(t, logged.String(), "video:")
	})

	// Real tools and the real sandbox probe. Where Landlock runs, video is on
	// (TestNewBot_VideoGateSandboxOn checks the ABI line). Elsewhere, as on
	// Windows, it is off unless allow_unsandboxed, and then on with a warning.
	t.Run("video block with ffmpeg and ffprobe", func(t *testing.T) {
		onPath(t)
		reset()
		b := videoGateBot(t, setupTestDB(t), &VideoConfig{SheetFrames: 8})
		want := VideoConfig{MaxDurationSeconds: 600, SheetFrames: 8, MaxLookups: 6, MaxFramesPerLookup: 12, RetentionHours: 24}
		if sandboxUsable(sandboxAvailable()) != nil || taskCapUnboundHere(t) {
			assert.False(t, b.videoOn(), "no sandbox and no opt-out: off")
			assert.Nil(t, b.video)
			assert.Contains(t, logged.String(), "[video_gate_bot] video: no sandbox for ffmpeg (")
			assert.Contains(t, logged.String(), "video watching is off")
			assert.False(t, unsandboxedAllowed(), "nothing may run without the sandbox")

			reset()
			b = videoGateBot(t, setupTestDB(t), &VideoConfig{SheetFrames: 8, AllowUnsandboxed: true})
			want.AllowUnsandboxed = true
			assert.Contains(t, warned.String(), "[video_gate_bot] Warning: video: ffmpeg runs WITHOUT a full sandbox")
			assert.NotContains(t, logged.String(), "ffmpeg sandbox on")
		}
		assert.True(t, b.videoOn())
		require.NotNil(t, b.video)
		assert.Equal(t, want, *b.video)
		assert.Contains(t, logged.String(), `[video_gate_bot] video: "ffmpeg version `, "the version line is logged once video is on")
	})

	// The brief's test hook: Landlock unavailable, whatever the host has.
	t.Run("no Landlock: off without allow_unsandboxed, on with it", func(t *testing.T) {
		onPath(t)
		stubGate(t, func() (int, error) { return 0, errors.New("simulated: no Landlock") }, nil)
		reset()
		b := videoGateBot(t, setupTestDB(t), &VideoConfig{})
		assert.False(t, b.videoOn())
		assert.Contains(t, logged.String(), "[video_gate_bot] video: no sandbox for ffmpeg (simulated: no Landlock); video watching is off")
		assert.Empty(t, warned.String())

		reset()
		b = videoGateBot(t, setupTestDB(t), &VideoConfig{AllowUnsandboxed: true})
		assert.True(t, b.videoOn(), "the opt-out runs ffmpeg without the sandbox")
		assert.True(t, unsandboxedAllowed())
		assert.Contains(t, warned.String(), "[video_gate_bot] Warning: video: ffmpeg runs WITHOUT a full sandbox (simulated: no Landlock)")
		assert.Contains(t, warned.String(), "read, write and run any file", "not only read (Ruling 19 item 8)")
		// Only the gate's probe is simulated: on a host with Landlock the
		// helper still applies it, but the gate says "sandbox on" only when it
		// found a sandbox. Its layers and gaps are logged all the same.
		assert.NotContains(t, logged.String(), "ffmpeg sandbox on")
		assertSandboxLines(t, logged.String(), "[video_gate_bot] video: ffmpeg partial sandbox (")
	})

	// Ruling 15: Landlock below ABI 3 cannot deny truncation, so it counts as
	// no sandbox. The opt-out still runs the tools, through the helper,
	// which applies what the kernel's Landlock can deny.
	t.Run("Landlock ABI 2: off without allow_unsandboxed, on with it", func(t *testing.T) {
		onPath(t)
		stubGate(t, func() (int, error) { return 2, nil }, nil)
		reset()
		b := videoGateBot(t, setupTestDB(t), &VideoConfig{})
		assert.False(t, b.videoOn())
		assert.Contains(t, logged.String(),
			"[video_gate_bot] video: no sandbox for ffmpeg (the kernel's Landlock ABI 2 cannot deny file truncation, which takes ABI 3); video watching is off")
		assert.Empty(t, warned.String())

		reset()
		b = videoGateBot(t, setupTestDB(t), &VideoConfig{AllowUnsandboxed: true})
		assert.True(t, b.videoOn())
		assert.Contains(t, warned.String(), "[video_gate_bot] Warning: video: ffmpeg runs WITHOUT a full sandbox (the kernel's Landlock ABI 2 cannot deny file truncation")
		if sandboxHelper {
			assert.Contains(t, warned.String(), "could empty any file the bot's user can write")
		}
		assert.NotContains(t, logged.String(), "ffmpeg sandbox on")
		assertSandboxLines(t, logged.String(), "[video_gate_bot] video: ffmpeg partial sandbox (")
	})

	// Landlock without the seccomp filter (a kernel or container profile
	// without seccomp) is no sandbox either; the opt-out logs what only the
	// filter would close.
	t.Run("no seccomp filter: off without allow_unsandboxed, on with it", func(t *testing.T) {
		onPath(t)
		stubGate(t, func() (int, error) { return 7, fmt.Errorf("%w: seccomp: invalid argument", errNoSeccomp) },
			func(context.Context, string, bool) (sandboxReport, error) {
				return sandboxReport{ABI: 7, OOMScoreAdj: true, TaskCap: true, NoCapabilities: true}, nil
			})
		reset()
		b := videoGateBot(t, setupTestDB(t), &VideoConfig{})
		assert.False(t, b.videoOn())
		assert.Contains(t, logged.String(), "[video_gate_bot] video: no sandbox for ffmpeg (no seccomp filter: seccomp: invalid argument); video watching is off")

		reset()
		b = videoGateBot(t, setupTestDB(t), &VideoConfig{AllowUnsandboxed: true})
		assert.True(t, b.videoOn())
		assert.Contains(t, warned.String(), "[video_gate_bot] Warning: video: ffmpeg runs WITHOUT a full sandbox (no seccomp filter")
		if sandboxHelper {
			assert.Contains(t, warned.String(), "could use what only the seccomp filter closes")
			assert.Contains(t, logged.String(), "[video_gate_bot] video: ffmpeg partial sandbox (Landlock ABI 7, no seccomp filter, task cap)\n")
			assert.Contains(t, logged.String(), "[video_gate_bot] video: the sandbox does not cover pathname unix sockets (ABI 9); UDP (ABI 10); "+gapFilterOnly)
		}
		assertSandboxLines(t, logged.String(), "[video_gate_bot] video: ffmpeg partial sandbox (")
		unsandboxedGrants.Add(-1)
	})

	// Any self-test failure turns video off, and the log carries the tool's
	// stderr tail, quoted.
	t.Run("self-test fails: off, stderr logged", func(t *testing.T) {
		db := setupTestDB(t)
		fakeTool(t, "ffprobe", `echo 'probe "exploded"' >&2; exit 1`)
		fakeTool(t, "ffmpeg", "exit 0")
		reset()
		b := videoGateBot(t, db, &VideoConfig{AllowUnsandboxed: true})
		assert.False(t, b.videoOn())
		assert.Nil(t, b.video)
		assert.Contains(t, logged.String(), "[video_gate_bot] video: the ffmpeg self-test failed: probe: the tool failed: ffprobe: exit status 1",
			"a tool's exit is errToolFailed, not unsupported media (Ruling 19 item 2)")
		assert.Contains(t, logged.String(), `stderr "probe \"exploded\"\n"; video watching is off`)
	})

	// A gate that let tools run unsandboxed and then failed its self-test has
	// video off, so it takes that grant back; one that passed keeps it
	// (review Minor 6).
	t.Run("no Landlock, self-test fails: the unsandboxed grant is taken back", func(t *testing.T) {
		onPath(t)
		failing := func(context.Context, string, bool) (sandboxReport, error) {
			return sandboxReport{}, errors.New("simulated")
		}
		stubGate(t, func() (int, error) { return 0, errors.New("simulated: no Landlock") }, failing)
		before := unsandboxedGrants.Load()
		b := videoGateBot(t, setupTestDB(t), &VideoConfig{AllowUnsandboxed: true})
		assert.False(t, b.videoOn())
		assert.Equal(t, before, unsandboxedGrants.Load(), "the failed gate leaves no grant behind")

		passing := func(context.Context, string, bool) (sandboxReport, error) { return sandboxReport{}, nil }
		stubGate(t, func() (int, error) { return 0, errors.New("simulated: no Landlock") }, passing)
		b = videoGateBot(t, setupTestDB(t), &VideoConfig{AllowUnsandboxed: true})
		assert.True(t, b.videoOn())
		assert.Equal(t, before+1, unsandboxedGrants.Load(), "a passing gate keeps its grant")
	})

	// Only the check run, after the helper drops its capabilities, can tell
	// that the task cap binds no user (the bot as root): the sandbox is then
	// not whole, and video is off unless video.allow_unsandboxed, which runs
	// the self-test again without demanding the full sandbox (Ruling 19
	// item 4).
	t.Run("task cap unbound: off, or on under the opt-out", func(t *testing.T) {
		onPath(t)
		var demanded []bool
		selfTest := func(_ context.Context, _ string, landlock bool) (sandboxReport, error) {
			demanded = append(demanded, landlock)
			report := sandboxReport{ABI: 7, OOMScoreAdj: true, Seccomp: true, NoCapabilities: true}
			if landlock {
				return report, fmt.Errorf("sandbox check: %w", errTaskCapUnbound)
			}
			return report, nil
		}
		stubGate(t, func() (int, error) { return 7, nil }, selfTest)
		reset()
		before := unsandboxedGrants.Load()
		b := videoGateBot(t, setupTestDB(t), &VideoConfig{})
		assert.False(t, b.videoOn())
		assert.Contains(t, logged.String(), "[video_gate_bot] video: no sandbox for ffmpeg (sandbox check: the task cap does not bind")
		assert.Contains(t, logged.String(), "run the bot as a non-root user")
		assert.Contains(t, logged.String(), "video watching is off")
		assert.Equal(t, before, unsandboxedGrants.Load())

		reset()
		demanded = nil
		b = videoGateBot(t, setupTestDB(t), &VideoConfig{AllowUnsandboxed: true})
		assert.True(t, b.videoOn())
		assert.Equal(t, []bool{true, false}, demanded, "the full self-test, then the one without the demand")
		assert.Contains(t, warned.String(), "[video_gate_bot] Warning: video: ffmpeg runs WITHOUT a full sandbox (sandbox check: the task cap")
		assert.Contains(t, warned.String(), "start threads until the host, or a container's pids limit, runs out of them")
		assert.NotContains(t, logged.String(), "ffmpeg sandbox on")
		if sandboxHelper {
			assert.Contains(t, logged.String(),
				"[video_gate_bot] video: ffmpeg partial sandbox (Landlock ABI 7, seccomp filter, no task cap: run the bot as a non-root user)\n")
			assert.Contains(t, logged.String(), "[video_gate_bot] video: the sandbox does not cover "+gapAlways+"; "+gapThreads+"\n")
		}
		assertSandboxLines(t, logged.String(), "[video_gate_bot] video: ffmpeg partial sandbox (")
		assert.Equal(t, before+1, unsandboxedGrants.Load(), "the opt-out's grant")
		unsandboxedGrants.Add(-1)
	})

	// The version check never turns video off; it only warns (Ruling 12 item 6).
	for _, tc := range []struct{ name, ffmpeg, logs, warns string }{
		{"ffmpeg older than 8", "echo 'ffmpeg version 6.1.1-3ubuntu5 Copyright (c) 2000-2023 the FFmpeg developers'",
			`[video_gate_bot] video: "ffmpeg version 6.1.1-3ubuntu5`, "[video_gate_bot] Warning: video: ffmpeg below version 8"},
		{"a git build", "echo 'ffmpeg version N-118000-gabcdef0123 Copyright'",
			`[video_gate_bot] video: "ffmpeg version N-118000`, "[video_gate_bot] Warning: video: ffmpeg below version 8 or of unknown version"},
		{"-version fails", "exit 1", "", "[video_gate_bot] Warning: video: could not read the ffmpeg version"},
		{"ffmpeg 8", "echo 'ffmpeg version 8.1.3 Copyright (c) 2000-2026 the FFmpeg developers'",
			`[video_gate_bot] video: "ffmpeg version 8.1.3`, ""},
	} {
		t.Run(tc.name+": on, version logged", func(t *testing.T) {
			db := setupTestDB(t)
			fakeTool(t, "ffprobe", "exit 0")
			fakeTool(t, "ffmpeg", tc.ffmpeg)
			// The stand-ins cannot pass a real self-test; this is about the version line.
			stubGate(t, func() (int, error) { return 7, nil },
				func(context.Context, string, bool) (sandboxReport, error) {
					return sandboxReport{ABI: 7, OOMScoreAdj: true, Seccomp: true}, nil
				})
			reset()
			b := videoGateBot(t, db, &VideoConfig{})
			assert.True(t, b.videoOn(), "never fatal")
			assert.Contains(t, logged.String(), "[video_gate_bot] video: ffmpeg sandbox on (Landlock ABI 7, seccomp filter)")
			assert.Contains(t, logged.String(), tc.logs)
			if tc.warns == "" {
				assert.Empty(t, warned.String())
				return
			}
			assert.Contains(t, warned.String(), tc.warns)
		})
	}

	// oom_score_adj is a preference, not a control: when it cannot be set the
	// gate says so and video stays on (Ruling 14 note 4).
	t.Run("oom_score_adj not applied: on, logged", func(t *testing.T) {
		if !toolMemoryCapped {
			t.Skip("oom_score_adj is set only on Linux")
		}
		onPath(t)
		stubGate(t, func() (int, error) { return 7, nil },
			func(context.Context, string, bool) (sandboxReport, error) { return sandboxReport{ABI: 7}, nil })
		reset()
		b := videoGateBot(t, setupTestDB(t), &VideoConfig{AllowUnsandboxed: true})
		assert.True(t, b.videoOn())
		assert.Contains(t, logged.String(), "[video_gate_bot] video: oom_score_adj could not be set")
	})

	t.Run("video block without the tools on PATH: off, logged", func(t *testing.T) {
		db := setupTestDB(t)
		reset()
		t.Setenv("PATH", "")
		b := videoGateBot(t, db, &VideoConfig{})
		assert.False(t, b.videoOn())
		assert.Nil(t, b.video)
		assert.Contains(t, logged.String(), "[video_gate_bot] video: ffmpeg/ffprobe not found on PATH; video watching is off")
	})
}

// Ruling 15: Landlock counts as a sandbox from ABI 3, the first that can deny
// truncation.
func TestSandboxUsable(t *testing.T) {
	assert.NoError(t, sandboxUsable(3, nil))
	assert.NoError(t, sandboxUsable(7, nil))
	for _, abi := range []int{1, 2} {
		assert.ErrorContains(t, sandboxUsable(abi, nil), "cannot deny file truncation", "ABI %d", abi)
	}
	probe := errors.New("no Landlock: it is Linux-only")
	assert.Equal(t, probe, sandboxUsable(0, probe), "no Landlock stays the probe's own reason")
}

// The gaps, by text, so that a change to the list shows here. Each part is
// what one layer, or none, leaves open (Rulings 18 item 4, 19 items 1, 2
// and 8).
const (
	gapFiles = "files: reading, writing and running any the bot's user may, /proc/<bot>/environ (any token in the bot's environment) " +
		"among them, and /proc/<bot>/mem where Yama's ptrace_scope is 0 (ABI 1)"
	gapFilterOnly = "other processes' resource limits, priority and CPU affinity; " +
		"file metadata changes (mode, owner, times, extended attributes); SysV IPC, inotify and fanotify; " +
		"ioctl requests (TIOCSTI among them), priority-inheriting futexes and other prctl options; " +
		"every other system call (new processes, ptrace, namespaces, io_uring, bpf); "
	gapAlways = "file and process metadata (stat: whether a path exists, its size and times, and through /proc/<pid> whether a process does); " +
		"readlink targets and statfs; " +
		"the parent-death signal, which a thread the tool starts drops by exec: while the bot lives a timeout still kills the tool's group, " +
		"but a tool that outlives a dead bot, still sandboxed, ends only with Docker's PID namespace or a systemd unit's control-group kill"
	gapPool = "the user's task pool: RLIMIT_NPROC counts every task of the bot's user, so a compromised tool can take what is left of it " +
		"and make other chats' concurrent runs fail while it runs"
	gapThreads = "threads: RLIMIT_NPROC does not bind the bot's user (root), so a tool can start them until the host, " +
		"or a container's pids limit, runs out"
)

// What the layers leave open, in every mode that runs video: the full
// sandbox names only what no layer covers, the shared task pool among them;
// each partial one adds what its missing layer would close.
func TestSandboxGaps(t *testing.T) {
	full := gapAlways + "; " + gapPool
	for _, abi := range []int{3, 4, 7, 10} {
		assert.Equal(t, full, sandboxGaps(sandboxReport{ABI: abi, Seccomp: true, TaskCap: true}), "ABI %d with the filter", abi)
	}
	// Without the filter (Landlock from ABI 3, under the opt-out).
	assert.Equal(t, "TCP (ABI 4); abstract unix sockets and signals (ABI 6); pathname unix sockets (ABI 9); UDP (ABI 10); "+
		gapFilterOnly+full, sandboxGaps(sandboxReport{ABI: 3, TaskCap: true}))
	assert.Equal(t, "abstract unix sockets and signals (ABI 6); pathname unix sockets (ABI 9); UDP (ABI 10); "+gapFilterOnly+full,
		sandboxGaps(sandboxReport{ABI: 4, TaskCap: true}))
	assert.Equal(t, "pathname unix sockets (ABI 9); UDP (ABI 10); "+gapFilterOnly+full, sandboxGaps(sandboxReport{ABI: 7, TaskCap: true}))
	assert.Equal(t, "UDP (ABI 10); "+gapFilterOnly+full, sandboxGaps(sandboxReport{ABI: 9, TaskCap: true}))
	assert.Equal(t, gapFilterOnly+full, sandboxGaps(sandboxReport{ABI: 10, TaskCap: true}))
	// Landlock below ABI 3, with the filter.
	assert.Equal(t, "file truncation (ABI 3); "+full, sandboxGaps(sandboxReport{ABI: 2, Seccomp: true, TaskCap: true}))
	// Without Landlock: the filter alone, then nothing but the limits.
	assert.Equal(t, gapFiles+"; file truncation (ABI 3); "+full, sandboxGaps(sandboxReport{Seccomp: true, TaskCap: true}),
		"the filter alone, under video.allow_unsandboxed")
	assert.Equal(t, gapFiles+"; file truncation (ABI 3); TCP (ABI 4); abstract unix sockets and signals (ABI 6); "+
		"pathname unix sockets (ABI 9); UDP (ABI 10); "+gapFilterOnly+full, sandboxGaps(sandboxReport{TaskCap: true}))
	// A task cap that binds no user (root, under the opt-out).
	assert.Equal(t, gapAlways+"; "+gapThreads, sandboxGaps(sandboxReport{ABI: 7, Seccomp: true}))
	for _, gaps := range []string{sandboxGaps(sandboxReport{ABI: 3, Seccomp: true}), sandboxGaps(sandboxReport{Seccomp: true})} {
		assert.NotContains(t, gaps, "signals;")
	}
}

// The gate's log lines about the sandbox, in every mode that runs video
// (Ruling 19 item 8): the layers in place, then what they leave open; off
// Linux, that there is none.
func TestSandboxSummary(t *testing.T) {
	for _, tc := range []struct {
		name         string
		helper, full bool
		report       sandboxReport
		first        string
	}{
		{"full", true, true, sandboxReport{ABI: 7, Seccomp: true, TaskCap: true}, "ffmpeg sandbox on (Landlock ABI 7, seccomp filter)"},
		{"no Landlock", true, false, sandboxReport{Seccomp: true, TaskCap: true}, "ffmpeg partial sandbox (no Landlock, seccomp filter, task cap)"},
		{"Landlock ABI 2", true, false, sandboxReport{ABI: 2, Seccomp: true, TaskCap: true},
			"ffmpeg partial sandbox (Landlock ABI 2, seccomp filter, task cap)"},
		{"no filter", true, false, sandboxReport{ABI: 7, TaskCap: true}, "ffmpeg partial sandbox (Landlock ABI 7, no seccomp filter, task cap)"},
		{"neither", true, false, sandboxReport{TaskCap: true}, "ffmpeg partial sandbox (no Landlock, no seccomp filter, task cap)"},
		{"root", true, false, sandboxReport{ABI: 7, Seccomp: true},
			"ffmpeg partial sandbox (Landlock ABI 7, seccomp filter, no task cap: run the bot as a non-root user)"},
	} {
		assert.Equal(t, []string{tc.first, "the sandbox does not cover " + sandboxGaps(tc.report)},
			sandboxSummary(tc.helper, tc.full, tc.report), tc.name)
	}
	assert.Equal(t, []string{"there is no sandbox on this OS: ffmpeg can do anything the bot's user can, reading, writing and running any file and using the network"},
		sandboxSummary(false, false, sandboxReport{}), "off Linux, under the opt-out")
}

// The opt-out's warning says what a video that exploits ffmpeg could do for
// want of the missing layer: without Landlock, read, write and run files,
// not only read them (Ruling 19 item 8).
func TestUnsandboxedHarm(t *testing.T) {
	const files = "read, write and run any file the bot's user can (every bot's token, the chat database, other chats' media)"
	const truncate = "empty any file the bot's user can write (the chat database, stored videos, a writable config)"
	const filter = "use what only the seccomp filter closes, as the gaps line lists (other processes, SysV IPC, ptrace, " +
		"the sockets its Landlock ABI allows)"
	noFilter := fmt.Errorf("%w: seccomp: invalid argument", errNoSeccomp)
	for _, tc := range []struct {
		name   string
		helper bool
		abi    int
		why    error
		want   string
	}{
		{"off Linux", false, 0, errors.New("no Landlock: it is Linux-only"),
			"do anything the bot's user can: read, write and run any file (every bot's token, the chat database, other chats' media) and use the network"},
		{"no Landlock", true, 0, errors.New("no Landlock: landlock_create_ruleset: function not implemented"), files},
		{"Landlock ABI 2", true, 2, sandboxUsable(2, nil), truncate},
		{"no filter", true, 7, noFilter, filter},
		{"Landlock ABI 2, no filter", true, 2, noFilter, truncate + "; and " + filter},
		{"root", true, 7, fmt.Errorf("sandbox check: %w", errTaskCapUnbound), "start threads until the host, or a container's pids limit, runs out of them"},
	} {
		assert.Equal(t, tc.want, unsandboxedHarm(tc.helper, tc.abi, tc.why), tc.name)
	}
}
