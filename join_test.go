package main

import (
	"context"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memoryCopy is the chat's memory copy of the message with database id.
func memoryCopy(t *testing.T, b *Bot, chatID int64, id uint) []Message {
	t.Helper()
	mem := b.getOrCreateChatMemory(chatID)
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	var copies []Message
	for _, m := range mem.Messages {
		if m.ID == id {
			copies = append(copies, m)
		}
	}
	return copies
}

// A message is stored at intake but joins the conversation later, or never.
// A reload in between, as after a crash, must not load it.
func TestReload_SkipsStoredMessageThatNeverJoined(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := historyTestBot(t, nil)
	const chatID = int64(801)
	storeUserText(t, b, chatID, "joined")
	_, err := b.screenIncomingMessage(&models.Message{
		ID: 960, Chat: models.Chat{ID: chatID}, From: &models.User{ID: chatID}, Text: "stored, then the bot crashed",
	})
	require.NoError(t, err)

	dropChatMemory(b, chatID)

	assert.Equal(t, []string{"joined"}, memoryTexts(b, chatID))
	assert.True(t, storedRow(t, b, chatID, "stored, then the bot crashed").Aside)
	assert.False(t, storedRow(t, b, chatID, "joined").Aside)
}

// Scoped P2 inverted: /clear drops memory while a voice note is being
// transcribed. The reload does not load its placeholder, so the text turn
// that runs meanwhile does not read it, and the voice turn then answers the
// transcript.
func TestReload_DuringTranscriptionDoesNotLoadThePlaceholder(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	sent := recordSends(tg)
	(&voiceStubs{text: transcript}).install(t, b, tg)
	const chatID = int64(123)
	storeUserText(t, b, chatID, "earlier question")
	answerInTurn(t, b, chatID, "earlier answer")
	require.NoError(t, b.db.Where("chat_id = ?", chatID).Delete(&Message{}).Error) // /clear's delete
	voice := voiceMessage(chatID, 961)
	userMsg, err := b.screenIncomingMessage(voice) // stored between the delete and the drop
	require.NoError(t, err)
	dropChatMemory(b, chatID) // /clear's drop
	storeUserText(t, b, chatID, "hello again")

	b.respondToChat(textReq(chatID))
	require.Len(t, stub.requestBetas(), 1)
	assert.False(t, requestHas(t, stub, 0, "[Voice message]"), "the placeholder is not read")

	b.handleVoiceMessage(context.Background(), voice, userMsg, nil, replyTarget{chatID: chatID},
		123, "owner", "", "", false, "en", 0)

	require.Len(t, stub.requestBetas(), 2, "the transcript reaches the model")
	role, text := stub.lastMessage(t, 1)
	assert.Equal(t, "user", role)
	assert.Equal(t, transcript, text)
	for _, s := range sent() {
		assert.NotEqual(t, b.anthropicErrorResponse(errHistoryNotUserLast, 123), s.Text, "no error reply")
	}
}

// A copy already in memory (a row that was in the conversation when memory
// loaded) moves to the end with a fresh sequence when it joins: it is not
// held twice, and it sorts after everything already there.
func TestJoinConversation_MovesACopyAlreadyInMemoryToTheEnd(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := historyTestBot(t, nil)
	const chatID = int64(802)
	early := b.createMessage(chatID, 555, "tester", "user", "loaded before it joined", true)
	require.NoError(t, b.storeMessage(&early)) // a row in the conversation, as an older version stored it
	dropChatMemory(b, chatID)
	later := storeUserText(t, b, chatID, "joined in between") // loads memory, with early in it

	joined := b.joinConversation(early)

	assert.Equal(t, []string{"joined in between", "loaded before it joined"}, memoryTexts(b, chatID))
	require.Len(t, memoryCopy(t, b, chatID, early.ID), 1, "held once")
	assert.Greater(t, joined.Seq, later.Seq, "a fresh sequence, after everything already there")
}

// Scoped P1 inverted: a voice note is stored when it arrives but joins once
// transcribed, after a text sent meanwhile. The turn reads both; a message
// lands during its model call. However old the voice note's intake time, a
// reload puts the reply after both messages it answered and before the late
// one, which stays unanswered. The test clock is frozen: every join here
// happens in the same clock tick.
func TestReload_KeepsAReplyAfterEveryMessageItAnswered(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	const chatID = int64(803)
	storeUserText(t, b, chatID, "X sent during transcription")
	voice := b.createMessage(chatID, 123, "owner", "user", "V transcript", true)
	voice.Timestamp = time.Now().Add(-time.Hour) // its intake, long before it joined
	voice.Aside = true
	require.NoError(t, b.storeMessage(&voice))
	b.joinConversation(voice)
	turn, _ := b.beginTurn(context.Background(), chatID) // the voice turn
	b.snapshotForTurn(turn, b.getOrCreateChatMemory(chatID))
	storeUserText(t, b, chatID, "Y during the model call")
	_, err := b.recordTurnReply(turn, chatID, "answer to X and V")
	require.NoError(t, err)
	b.finishTurn(turn)

	dropChatMemory(b, chatID)

	assert.Equal(t, []string{"X sent during transcription", "V transcript", "answer to X and V", "Y during the model call"},
		memoryTexts(b, chatID))
	b.respondToChat(textReq(chatID))
	require.Len(t, stub.requestBetas(), 1, "only Y is left to answer")
	_, text := stub.lastMessage(t, 0)
	assert.Equal(t, "Y during the model call", text)
}

// The clock is set back after the last message joined: a reply at the end is
// still stamped after it.
func TestRecordTurnReply_ClockSetBackStillStampsAfterTheLastMessage(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := historyTestBot(t, nil)
	const chatID = int64(804)
	storeUserText(t, b, chatID, "first")
	second := storeUserText(t, b, chatID, "second")
	b.clock = &MockClock{currentTime: second.Timestamp.Add(-time.Hour)}

	answerInTurn(t, b, chatID, "answer")

	dropChatMemory(b, chatID)
	assert.Equal(t, []string{"first", "second", "answer"}, memoryTexts(b, chatID))
}
