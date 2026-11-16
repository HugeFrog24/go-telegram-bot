package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// A media message takes over the text batch waiting in its quiet window: its
// own turn will read and answer that text. When the media path ends without
// a model turn, the batch must still be answered, as its own text turn.

const ownerChat = int64(123) // the owner's private chat

var owner = &models.User{ID: ownerChat, Username: "owner"}

// takeoverBot is a bot with a stubbed model, a 10 s quiet window and one
// message already waiting in it.
func takeoverBot(t *testing.T) (*Bot, *MockTelegramClient, *anthropicStub, func() []bot.SendMessageParams) {
	t.Helper()
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, ownerChat)
	useAnthropicStub(b, stub)
	sent := recordSends(tg)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.handleUpdate(context.Background(), nil, textUpdate(ownerChat, 950, owner, "where is the candle farm?"))
	require.True(t, b.hasPendingIntake(ownerChat))
	return b, tg, stub, sent
}

// requireBatchAnswered checks the waiting text got a model turn of its own,
// whose request ends with it, and that nothing is left waiting.
func requireBatchAnswered(t *testing.T, b *Bot, stub *anthropicStub) {
	t.Helper()
	waitIdle(t, b, ownerChat)
	require.Len(t, stub.requestBetas(), 1, "the waiting text is answered")
	role, text := stub.lastMessage(t, 0)
	assert.Equal(t, "user", role)
	assert.Equal(t, "where is the candle farm?", text)
	assert.False(t, b.hasPendingIntake(ownerChat))
}

func photoUpdate(msgID int) *models.Update {
	return &models.Update{Message: &models.Message{
		ID: msgID, Chat: models.Chat{ID: ownerChat}, From: owner,
		Photo: []models.PhotoSize{{FileID: "photo-file", Width: 10, Height: 10}},
	}}
}

func TestHandleUpdate_BufferedTextSurvivesRefusedVoice(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _, stub, sent := takeoverBot(t) // no ElevenLabs key: voice is refused

	b.handleUpdate(context.Background(), nil, &models.Update{Message: voiceMessage(ownerChat, 951)})

	requireBatchAnswered(t, b, stub)
	require.NotEmpty(t, sent())
	assert.Equal(t, "I don't understand voice messages.", sent()[0].Text)
}

func TestHandleUpdate_BufferedTextSurvivesFailedTranscription(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg, stub, sent := takeoverBot(t)
	(&voiceStubs{}).install(t, b, tg) // speech to text fails

	b.handleUpdate(context.Background(), nil, &models.Update{Message: voiceMessage(ownerChat, 952)})

	requireBatchAnswered(t, b, stub)
	require.NotEmpty(t, sent())
	assert.Equal(t, "Sorry, I couldn't understand your voice message.", sent()[0].Text)
}

func TestHandleUpdate_BufferedTextSurvivesFailedPhotoUpload(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg, stub, sent := takeoverBot(t)
	tg.GetFileFunc = func(context.Context, *bot.GetFileParams) (*models.File, error) {
		return nil, errors.New("telegram is down")
	}

	b.handleUpdate(context.Background(), nil, photoUpdate(953))

	requireBatchAnswered(t, b, stub)
	require.NotEmpty(t, sent())
	assert.Equal(t, "Sorry, I couldn't process one of your images.", sent()[0].Text)
}

func TestHandleUpdate_BufferedTextSurvivesFailedPhotoStore(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg, stub, sent := takeoverBot(t)
	serveTelegramFiles(t, tg, nil)
	require.NoError(t, b.db.Callback().Create().Before("gorm:create").Register("test:fail_photo_store", func(db *gorm.DB) {
		if m, ok := db.Statement.Dest.(*Message); ok && len(m.ImageFileIDs) > 0 {
			_ = db.AddError(errors.New("disk full"))
		}
	}))

	b.handleUpdate(context.Background(), nil, photoUpdate(954))

	requireBatchAnswered(t, b, stub)
	require.NotEmpty(t, sent())
	assert.Equal(t, "Sorry, I had trouble saving your message.", sent()[0].Text)
}

func TestHandleUpdate_BufferedTextSurvivesRateLimitedPhoto(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, ownerChat)
	useAnthropicStub(b, stub)
	sent := recordSends(tg)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.config.MessagePerHour = 1
	b.handleUpdate(context.Background(), nil, textUpdate(ownerChat, 955, owner, "where is the candle farm?"))

	b.handleUpdate(context.Background(), nil, photoUpdate(956))

	requireBatchAnswered(t, b, stub)
	require.NotEmpty(t, sent())
	assert.Equal(t, "Rate limit exceeded. Please try again later.", sent()[0].Text)
}

func TestFlushAlbum_RateLimitedAlbumLeavesBufferedTextAnswered(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, ownerChat)
	useAnthropicStub(b, stub)
	recordSends(tg)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.config.MessagePerHour = 1
	b.handleUpdate(context.Background(), nil, textUpdate(ownerChat, 957, owner, "where is the candle farm?"))
	album := photoUpdate(958)
	album.Message.MediaGroupID = "album-1"
	b.handleUpdate(context.Background(), nil, album)

	b.flushAlbum(context.Background(), "album-1")

	requireBatchAnswered(t, b, stub)
}
