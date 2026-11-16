package main

import (
	"bytes"
	"context"
	"errors"
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
		if sandboxUsable(sandboxAvailable()) != nil {
			assert.False(t, b.videoOn(), "no sandbox and no opt-out: off")
			assert.Nil(t, b.video)
			assert.Contains(t, logged.String(), "[video_gate_bot] video: no sandbox for ffmpeg (")
			assert.Contains(t, logged.String(), "video watching is off")
			assert.False(t, unsandboxedAllowed(), "nothing may run without the sandbox")

			reset()
			b = videoGateBot(t, setupTestDB(t), &VideoConfig{SheetFrames: 8, AllowUnsandboxed: true})
			want.AllowUnsandboxed = true
			assert.Contains(t, warned.String(), "[video_gate_bot] Warning: video: ffmpeg runs WITHOUT a sandbox")
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
		assert.Contains(t, warned.String(), "[video_gate_bot] Warning: video: ffmpeg runs WITHOUT a sandbox (simulated: no Landlock)")
		// Only the gate's probe is simulated: on a host with Landlock the
		// helper still applies it, but the gate says "sandbox on" only when it
		// found a sandbox.
		assert.NotContains(t, logged.String(), "ffmpeg sandbox on")
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
		assert.Contains(t, warned.String(), "[video_gate_bot] Warning: video: ffmpeg runs WITHOUT a sandbox (the kernel's Landlock ABI 2 cannot deny file truncation")
		assert.NotContains(t, logged.String(), "ffmpeg sandbox on")
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

// What neither layer covers (Ruling 18 item 4): each ABI's gaps without the
// seccomp filter, with what only the filter could deny; with it, from ABI
// 3, only what no layer hides, file metadata; and without Landlock, files.
func TestSandboxGaps(t *testing.T) {
	const meta = "file metadata (stat: whether a path exists, sizes, times)"
	const filterOnly = "other processes' resource limits, priority and CPU affinity, " +
		"file metadata changes (mode, owner, times, extended attributes), SysV IPC, inotify and fanotify, " +
		"clearing the parent-death signal, every other system call (new processes, namespaces, io_uring, bpf), "
	assert.Equal(t, "TCP (ABI 4), abstract unix sockets and signals (ABI 6), pathname unix sockets (ABI 9), UDP (ABI 10), "+
		filterOnly+meta, sandboxGaps(3, false))
	assert.Equal(t, "abstract unix sockets and signals (ABI 6), pathname unix sockets (ABI 9), UDP (ABI 10), "+filterOnly+meta,
		sandboxGaps(4, false))
	assert.Equal(t, "pathname unix sockets (ABI 9), UDP (ABI 10), "+filterOnly+meta, sandboxGaps(7, false))
	assert.Equal(t, "UDP (ABI 10), "+filterOnly+meta, sandboxGaps(9, false))
	assert.Equal(t, filterOnly+meta, sandboxGaps(10, false))
	for _, abi := range []int{3, 4, 7, 10} {
		assert.Equal(t, meta, sandboxGaps(abi, true), "ABI %d with the filter", abi)
	}
	assert.Equal(t, "file truncation (ABI 3), "+meta, sandboxGaps(2, true))
	assert.Equal(t, "files (reading, writing and running any the user may) (ABI 1), file truncation (ABI 3), "+meta,
		sandboxGaps(0, true), "the filter alone, under video.allow_unsandboxed")
	for _, gaps := range []string{sandboxGaps(3, true), sandboxGaps(0, true)} {
		assert.NotContains(t, gaps, "signals")
	}
}
