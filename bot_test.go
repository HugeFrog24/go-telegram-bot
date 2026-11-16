package main

import (
	"bytes"
	"log"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
func videoGateBot(t *testing.T, db *gorm.DB, video *VideoConfig) *Bot {
	t.Helper()
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

func TestNewBot_VideoGate(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	var logged bytes.Buffer
	prev := InfoLogger
	InfoLogger = log.New(&logged, "", 0)
	t.Cleanup(func() { InfoLogger = prev })

	t.Run("no video block: off", func(t *testing.T) {
		b := videoGateBot(t, setupTestDB(t), nil)
		assert.False(t, b.videoOn())
		assert.Nil(t, b.video)
		assert.NotContains(t, logged.String(), "video:")
	})

	t.Run("video block with ffmpeg and ffprobe: on, defaults filled", func(t *testing.T) {
		for _, tool := range []string{"ffmpeg", "ffprobe"} {
			if _, err := exec.LookPath(tool); err != nil {
				t.Skipf("%s not on PATH", tool)
			}
		}
		b := videoGateBot(t, setupTestDB(t), &VideoConfig{SheetFrames: 8})
		assert.True(t, b.videoOn())
		assert.Equal(t, VideoConfig{
			MaxDurationSeconds: 600, SheetFrames: 8, MaxLookups: 6, MaxFramesPerLookup: 12, RetentionHours: 24,
		}, *b.video)
	})

	t.Run("video block without the tools on PATH: off, logged", func(t *testing.T) {
		db := setupTestDB(t)
		logged.Reset()
		t.Setenv("PATH", "")
		b := videoGateBot(t, db, &VideoConfig{})
		assert.False(t, b.videoOn())
		assert.Nil(t, b.video)
		assert.Contains(t, logged.String(), "[video_gate_bot] video: ffmpeg/ffprobe not found on PATH; video watching is off")
	})
}
