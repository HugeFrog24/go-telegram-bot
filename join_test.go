package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// memoryCopy is the chat's memory copy of the message with database id.
func memoryCopy(t *testing.T, b *Bot, chatID int64, id uint) []Message {
	t.Helper()
	mem := b.getOrCreateChatMemory(chatKey(chatID))
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
	crashed := &models.Message{
		ID: 960, Chat: models.Chat{ID: chatID}, From: &models.User{ID: chatID}, Text: "stored, then the bot crashed",
	}
	_, err := b.screenIncomingMessage(crashed, targetFor(crashed, ""))
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
	userMsg, err := b.screenIncomingMessage(voice, targetFor(voice, voice.BusinessConnectionID)) // stored between the delete and the drop
	require.NoError(t, err)
	dropChatMemory(b, chatID) // /clear's drop
	storeUserText(t, b, chatID, "hello again")

	b.respondToChat(textReq(chatID))
	require.Len(t, stub.requestBetas(), 1)
	assert.False(t, requestHas(t, stub, 0, "[Voice message]"), "the placeholder is not read")

	b.handleVoiceMessage(context.Background(), voice, userMsg, replyTarget{chatID: chatID},
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
	early := b.createMessage(chatKey(chatID), 555, "tester", "user", "loaded before it joined", true)
	require.NoError(t, b.storeMessage(&early)) // a row in the conversation, as an older version stored it
	dropChatMemory(b, chatID)
	later := storeUserText(t, b, chatID, "joined in between") // loads memory, with early in it

	joined, ok := b.joinConversation(early)
	require.True(t, ok)

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
	voice := b.createMessage(chatKey(chatID), 123, "owner", "user", "V transcript", true)
	voice.Timestamp = time.Now().Add(-time.Hour) // its intake, long before it joined
	voice.Aside = true
	require.NoError(t, b.storeMessage(&voice))
	b.joinConversation(voice)
	turn, _ := b.beginTurn(context.Background(), chatID) // the voice turn
	b.snapshotForTurn(turn, b.getOrCreateChatMemory(chatKey(chatID)))
	storeUserText(t, b, chatID, "Y during the model call")
	_, err := b.recordTurnReply(turn, chatKey(chatID), "answer to X and V")
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

// The join's database write runs after the memory lock is released: while it
// waits for the one connection, other chats' memory stays readable.
func TestJoinConversation_DatabaseWaitHoldsNoMemoryLock(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := historyTestBot(t, nil) // one database connection, as in production
	const chatX, chatY = int64(9001), int64(9002)
	b.getOrCreateChatMemory(chatKey(chatX))
	b.getOrCreateChatMemory(chatKey(chatY))
	msg := b.createMessage(chatKey(chatX), 5, "x", "user", "hello", true)
	msg.Aside = true
	require.NoError(t, b.storeMessage(&msg))
	tx := b.db.Begin() // a slow statement holding the only connection
	require.NoError(t, tx.Error)

	joined := make(chan bool, 1)
	go func() { _, ok := b.joinConversation(msg); joined <- ok }()
	time.Sleep(100 * time.Millisecond) // the join waits for the connection
	readY := make(chan struct{})
	go func() { defer close(readY); b.newestAnswerableSeqInChat(chatKey(chatY)) }()
	select {
	case <-readY:
	case <-time.After(2 * time.Second):
		t.Error("chat Y's memory read waited behind chat X's database write")
	}
	require.NoError(t, tx.Rollback().Error)
	assert.True(t, <-joined)
	<-readY
}

// A row /clear deleted before the message could join no longer joins: the
// message leaves memory again and its path ends.
func TestJoinConversation_ARowDeletedBeforeItJoinsStaysOut(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := historyTestBot(t, nil)
	const chatID = int64(808)
	storeUserText(t, b, chatID, "joined before")
	msg := b.createMessage(chatKey(chatID), 555, "tester", "user", "stored, then cleared", true)
	msg.Aside = true
	require.NoError(t, b.storeMessage(&msg))
	require.NoError(t, b.db.Delete(&Message{}, msg.ID).Error) // /clear's delete

	_, ok := b.joinConversation(msg)

	assert.False(t, ok)
	assert.Equal(t, []string{"joined before"}, memoryTexts(b, chatID))
}

// A message whose memory /clear replaced between its insert and its write
// does not stay in the conversation.
func TestJoinConversation_MemoryReplacedMeanwhileEndsTheJoin(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := historyTestBot(t, nil)
	const chatID = int64(809)
	b.getOrCreateChatMemory(chatKey(chatID))
	msg := b.createMessage(chatKey(chatID), 555, "tester", "user", "joined as the memory was replaced", true)
	msg.Aside = true
	require.NoError(t, b.storeMessage(&msg))
	var once sync.Once
	require.NoError(t, b.db.Callback().Update().Before("gorm:update").Register("test:replace_memory", func(*gorm.DB) {
		once.Do(func() { // /clear's swap lands between the insert and the write
			b.chatMemoriesMu.Lock()
			defer b.chatMemoriesMu.Unlock()
			b.chatMemories[chatKey(chatID)] = &ChatMemory{Size: b.memorySize * 2}
		})
	}))

	_, ok := b.joinConversation(msg)

	assert.False(t, ok)
	assert.Empty(t, memoryTexts(b, chatID), "the new memory never holds it")
}
