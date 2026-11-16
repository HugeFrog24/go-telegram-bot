package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for behaviours a mutation once left unnoticed (review r2, finding 5).

// A transcript of nothing but white space is a failed transcription: the
// user is told, the note never joins, and nothing asks the model.
func TestHandleVoiceMessage_BlankTranscriptIsAFailedTranscription(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, ownerChat)
	useAnthropicStub(b, stub)
	sent := recordSends(tg)
	(&voiceStubs{text: "  \n "}).install(t, b, tg)
	voice := voiceMessage(ownerChat, 1001)
	userMsg, err := b.screenIncomingMessage(voice)
	require.NoError(t, err)

	b.handleVoiceMessage(context.Background(), voice, userMsg, replyTarget{chatID: ownerChat},
		123, "owner", "", "", false, "en", 0)

	assert.Equal(t, []string{"Sorry, I couldn't understand your voice message."}, sentTexts(sent))
	assert.Empty(t, stub.requestBetas())
	assert.Empty(t, memoryTexts(b, ownerChat), "the note never joined")
	assert.True(t, storedRow(t, b, ownerChat, "[Voice message]").Aside)
}

// A reply that covered less than an earlier one (here a turn that read no
// memory) never lowers answeredThrough: the earlier answer still counts, and
// the answered message's late flush still registers no turn.
func TestFileReply_NeverLowersAnsweredThrough(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := historyTestBot(t, nil)
	const chatID = int64(805)
	storeUserText(t, b, chatID, "where is the candle farm?")
	answerInTurn(t, b, chatID, "Open the Farm tab.")
	mem := b.getOrCreateChatMemory(chatID)
	answered := mem.answeredThrough.Load()
	require.NotZero(t, answered)

	turn, _ := b.beginTurn(context.Background(), chatID)
	_, err := b.recordTurnReply(turn, chatID, "a reply that covered nothing")
	require.NoError(t, err)
	b.finishTurn(turn)

	assert.Equal(t, answered, mem.answeredThrough.Load())
	turns := turnsRegistered(b)
	b.respondToChat(textReq(chatID))
	assert.Equal(t, turns, turnsRegistered(b), "the answered message's flush registers no turn")
}
