package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
