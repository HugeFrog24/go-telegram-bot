package main

import (
	"context"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeUserMessage puts a user message with a Telegram id into DB and memory,
// the way screenIncomingMessage does, without going through handleUpdate.
func storeUserMessage(t *testing.T, b *Bot, chatID int64, telegramID int, text string) Message {
	t.Helper()
	msg := b.createMessage(chatID, chatID, "owner", "user", text, true)
	msg.TelegramMessageID = telegramID
	require.NoError(t, b.storeMessage(&msg))
	b.addMessageToChatMemory(b.getOrCreateChatMemory(chatID), msg)
	return msg
}

func editUpdate(chatID int64, telegramID int, text string) *models.Update {
	return &models.Update{EditedMessage: &models.Message{
		ID: telegramID, Chat: models.Chat{ID: chatID}, From: &models.User{ID: chatID, Username: "owner"}, Text: text,
	}}
}

func TestHandleUpdate_EditedMessageUpdatesTextAndRearmsWindow(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	const chatID int64 = 123
	from := &models.User{ID: chatID, Username: "owner"}

	b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
		ID: 77, Chat: models.Chat{ID: chatID}, From: from, Text: "How to use libtik",
	}})
	require.True(t, b.hasPendingIntake(chatID))
	b.intakeBuffersMu.Lock()
	seqBefore := b.intakeBuffers[chatID].seq
	timerBefore := b.intakeBuffers[chatID].timer
	b.intakeBuffersMu.Unlock()

	b.handleUpdate(context.Background(), nil, editUpdate(chatID, 77, "How to use libtibik"))

	var stored Message
	require.NoError(t, b.db.Where("chat_id = ? AND telegram_message_id = ?", chatID, 77).First(&stored).Error)
	assert.Equal(t, "How to use libtibik", stored.Text, "database copy follows the edit")

	mem := b.getOrCreateChatMemory(chatID)
	assert.Equal(t, "How to use libtibik", mem.Messages[len(mem.Messages)-1].Text, "memory copy follows the edit")

	require.True(t, b.hasPendingIntake(chatID), "an edit must not flush or drop the batch")
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	assert.Equal(t, seqBefore, b.intakeBuffers[chatID].seq, "same batch")
	assert.NotSame(t, timerBefore, b.intakeBuffers[chatID].timer, "the window restarted")
	assert.Equal(t, 1, b.intakeBuffers[chatID].count, "an edit is not a new message")
}

func TestHandleUpdate_EditFromEmojiToTextClearsEmojiFlag(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	const chatID int64 = 123

	b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
		ID: 78, Chat: models.Chat{ID: chatID}, From: &models.User{ID: chatID}, Text: "👍",
	}})
	b.intakeBuffersMu.Lock()
	require.True(t, b.intakeBuffers[chatID].allEmojiOnly)
	b.intakeBuffersMu.Unlock()

	b.handleUpdate(context.Background(), nil, editUpdate(chatID, 78, "can you explain that again?"))

	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	assert.False(t, b.intakeBuffers[chatID].allEmojiOnly, "a real question must not get an emoji-only reply")
}

func TestHandleUpdate_CaptionEditApplies(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	const chatID int64 = 123
	storeUserMessage(t, b, chatID, 79, "old caption")

	b.handleUpdate(context.Background(), nil, &models.Update{EditedMessage: &models.Message{
		ID: 79, Chat: models.Chat{ID: chatID}, From: &models.User{ID: chatID}, Caption: "new caption",
	}})

	var stored Message
	require.NoError(t, b.db.Where("chat_id = ? AND telegram_message_id = ?", chatID, 79).First(&stored).Error)
	assert.Equal(t, "new caption", stored.Text)
}

func TestHandleUpdate_EditOfAnsweredMessageOnlyCorrectsRecord(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	const chatID int64 = 123
	storeUserMessage(t, b, chatID, 80, "first question")
	_, err := b.screenOutgoingMessage(chatID, "first answer")
	require.NoError(t, err)

	// A newer, unrelated message is waiting in its window.
	b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
		ID: 81, Chat: models.Chat{ID: chatID}, From: &models.User{ID: chatID}, Text: "second question",
	}})
	b.intakeBuffersMu.Lock()
	timerBefore := b.intakeBuffers[chatID].timer
	b.intakeBuffersMu.Unlock()

	b.handleUpdate(context.Background(), nil, editUpdate(chatID, 80, "first question, corrected"))

	var stored Message
	require.NoError(t, b.db.Where("telegram_message_id = ?", 80).First(&stored).Error)
	assert.Equal(t, "first question, corrected", stored.Text, "the record is corrected")
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	assert.Same(t, timerBefore, b.intakeBuffers[chatID].timer, "an answered message's edit must not touch another batch's window")
}

func TestHandleUpdate_EditRestartsUncommittedTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	const chatID int64 = 123
	msg := storeUserMessage(t, b, chatID, 82, "how do I use libtik")
	turn, ctx, ok := b.beginTextTurn(textReq(chatID), msg.ID)
	require.True(t, ok)

	b.handleUpdate(context.Background(), nil, editUpdate(chatID, 82, "how do I use libtibik"))

	assert.ErrorIs(t, ctx.Err(), context.Canceled, "the model was reading the typo; rerun with the fix")
	assert.Equal(t, cancelRestart, b.turnReason(turn))
	next := b.finishTurn(turn)
	require.NotNil(t, next)
	assert.Equal(t, chatID, next.chatID)
}

func TestHandleUpdate_EditOfUnknownMessageIsIgnored(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)

	b.handleUpdate(context.Background(), nil, editUpdate(123, 999, "never stored"))

	assert.False(t, b.hasPendingIntake(123))
	var count int64
	b.db.Model(&Message{}).Where("chat_id = ?", 123).Count(&count)
	assert.Zero(t, count, "an edit never creates a message")
}

func TestHandleUpdate_StopGenerationCancelsMatchingTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	turn, ctx, ok := b.beginTextTurn(textReq(123), 1)
	require.True(t, ok)
	b.setTurnDraft(turn, turn.draftIDString())

	b.handleUpdate(context.Background(), nil, &models.Update{
		StoppedMessageGeneration: &models.MessageGenerationStopped{
			Chat: models.Chat{ID: 123}, DraftID: int(turn.seq),
		},
	})

	assert.ErrorIs(t, ctx.Err(), context.Canceled)
	assert.Equal(t, cancelStop, b.turnReason(turn))
	b.finishTurn(turn)
}

func TestClearChatHistory_DiscardsCollectedFlush(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	mockTg.SendMessageFunc = func(_ context.Context, _ *bot.SendMessageParams) (*models.Message, error) {
		return &models.Message{}, nil
	}
	const chatID int64 = 123
	turn, _, ok := b.beginTextTurn(textReq(chatID), 5)
	require.True(t, ok)
	b.markCommitted(turn)
	_, _, ok = b.beginTextTurn(textReq(chatID), 7)
	require.False(t, ok, "collected behind the running turn")

	b.clearChatHistory(context.Background(), chatID, chatID, 0, 0, "", false)

	assert.Nil(t, b.finishTurn(turn), "the collected flush must not rerun against the emptied chat")
}
