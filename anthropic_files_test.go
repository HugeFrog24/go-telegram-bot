package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFormatUploadFilename(t *testing.T) {
	cases := []struct {
		botID       uint
		chatID      int64
		tgMessageID int
		ext         string
		want        string
	}{
		{1, 12345, 42, "jpg", "tg-1-12345-42.jpg"},
		{7, -1001234567890, 1, "png", "tg-7--1001234567890-1.png"},
		{0, 0, 0, "webp", "tg-0-0-0.webp"},
	}
	for _, tc := range cases {
		got := formatUploadFilename(tc.botID, tc.chatID, tc.tgMessageID, tc.ext)
		assert.Equal(t, tc.want, got)
	}
}

func TestParseMissingFileIDFromBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "canonical Anthropic file-not-found body",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"File not found: file_011CNha8iCJcU1wXNR6q4V8w"}}`,
			want: "file_011CNha8iCJcU1wXNR6q4V8w",
		},
		{
			name: "trailing punctuation after the id is excluded",
			body: `something File not found: file_abc123! more text`,
			want: "file_abc123",
		},
		{
			name: "body without the prefix yields empty",
			body: `{"type":"error","error":{"message":"Model not found: claude-foo"}}`,
			want: "",
		},
		{
			name: "id at the very end of the buffer",
			body: `File not found: file_xyz789`,
			want: "file_xyz789",
		},
		{
			name: "empty body",
			body: "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseMissingFileIDFromBody(tc.body))
		})
	}
}

func TestStripDeadFileIDs(t *testing.T) {
	dead := map[string]struct{}{
		"file_a": {},
		"file_b": {},
	}
	cases := []struct {
		name          string
		input         []string
		wantSurvivors []string
		wantDirty     bool
	}{
		{
			name:          "no overlap returns input verbatim",
			input:         []string{"file_x", "file_y"},
			wantSurvivors: []string{"file_x", "file_y"},
			wantDirty:     false,
		},
		{
			name:          "partial overlap returns survivors and reports dirty",
			input:         []string{"file_a", "file_x", "file_b", "file_y"},
			wantSurvivors: []string{"file_x", "file_y"},
			wantDirty:     true,
		},
		{
			name:          "all dead returns empty survivors and dirty",
			input:         []string{"file_a", "file_b"},
			wantSurvivors: []string{},
			wantDirty:     true,
		},
		{
			name:          "empty input is not dirty",
			input:         []string{},
			wantSurvivors: []string{},
			wantDirty:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			survivors, dirty := stripDeadFileIDs(tc.input, dead)
			assert.Equal(t, tc.wantSurvivors, survivors)
			assert.Equal(t, tc.wantDirty, dirty)
		})
	}
}

func TestMarkFilesPendingCleanup(t *testing.T) {
	b, _ := setupBotForTest(t, 123)
	chatID := int64(555)

	row1 := Message{
		BotID:        b.botID,
		ChatID:       chatID,
		UserID:       777,
		Username:     "u",
		UserRole:     "user",
		Text:         "look at these",
		Timestamp:    time.Now(),
		IsUser:       true,
		ImageFileIDs: []string{"file_a", "file_x"},
	}
	assert.NoError(t, b.db.Create(&row1).Error)

	row2 := Message{
		BotID:        b.botID,
		ChatID:       chatID,
		UserID:       777,
		Username:     "u",
		UserRole:     "user",
		Text:         "screenshot",
		Timestamp:    time.Now(),
		IsUser:       true,
		ImageFileIDs: []string{"file_a", "file_b"},
	}
	assert.NoError(t, b.db.Create(&row2).Error)

	row3 := Message{
		BotID:        b.botID,
		ChatID:       chatID,
		UserID:       777,
		Username:     "u",
		UserRole:     "user",
		Text:         "another",
		Timestamp:    time.Now(),
		IsUser:       true,
		ImageFileIDs: []string{"file_x", "file_y"},
	}
	assert.NoError(t, b.db.Create(&row3).Error)

	row4 := Message{
		BotID:        b.botID,
		ChatID:       999,
		UserID:       777,
		Username:     "u",
		UserRole:     "user",
		Text:         "other chat",
		Timestamp:    time.Now(),
		IsUser:       true,
		ImageFileIDs: []string{"file_a"},
	}
	assert.NoError(t, b.db.Create(&row4).Error)

	updated, err := b.markFilesPendingCleanup(t.Context(), chatID, []string{"file_a", "file_b"})
	assert.NoError(t, err)
	assert.Equal(t, 2, updated, "rows 1 and 2 should have been updated")

	var r1 Message
	assert.NoError(t, b.db.First(&r1, row1.ID).Error)
	assert.Equal(t, []string{"file_x"}, r1.ImageFileIDs)
	assert.Nil(t, r1.FilesCleanedAt)

	var r2 Message
	assert.NoError(t, b.db.First(&r2, row2.ID).Error)
	assert.Empty(t, r2.ImageFileIDs)
	assert.NotNil(t, r2.FilesCleanedAt)

	var r3 Message
	assert.NoError(t, b.db.First(&r3, row3.ID).Error)
	assert.Equal(t, []string{"file_x", "file_y"}, r3.ImageFileIDs)
	assert.Nil(t, r3.FilesCleanedAt)

	var r4 Message
	assert.NoError(t, b.db.First(&r4, row4.ID).Error)
	assert.Equal(t, []string{"file_a"}, r4.ImageFileIDs)
	assert.Nil(t, r4.FilesCleanedAt)
}
