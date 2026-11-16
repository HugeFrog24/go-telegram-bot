package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// historyTestBot is a bot whose Anthropic client talks to stub and whose
// Telegram sends and chat actions all succeed.
func historyTestBot(t *testing.T, stub *anthropicStub) *Bot {
	t.Helper()
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	tg.On("SendMessage", mock.Anything, mock.Anything).Return(&models.Message{ID: 1}, nil).Maybe()
	tg.On("SendChatAction", mock.Anything, mock.Anything).Return(true, nil).Maybe()
	return b
}

// storeUserText puts a user message into the database and chat memory, as
// screenIncomingMessage does at intake.
func storeUserText(t *testing.T, b *Bot, chatID int64, text string) Message {
	t.Helper()
	m := b.createMessage(chatID, 555, "tester", "user", text, true)
	require.NoError(t, b.storeMessage(&m))
	b.addMessageToChatMemory(b.getOrCreateChatMemory(chatID), m)
	return m
}

func chatIdle(b *Bot, chatID int64) bool {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	_, busy := b.turns[chatID]
	return !busy
}

// The production 400 "This model does not support assistant message
// prefill": a second message lands just after the first one's quiet window
// closed, so the turn that answers the first reads both; the second message's
// own window then closes after that turn has ended. Its flush must not ask the
// model again, because the history now ends with the bot's reply.
func TestRespondToChat_LateFlushForAnsweredMessageCallsNoModel(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b := historyTestBot(t, stub)
	const chatID = int64(777)
	storeUserText(t, b, chatID, "hi")
	storeUserText(t, b, chatID, "where is the candle farm?")

	b.respondToChat(textReq(chatID)) // answers both, stores the reply, ends
	require.Len(t, stub.requestBetas(), 1)

	b.respondToChat(textReq(chatID)) // the second message's flush, after the turn ended

	assert.Len(t, stub.requestBetas(), 1, "an already answered message gets no second model call")
}

// A message that arrives while a turn's answer is already on screen runs as
// its own turn afterwards. That turn must ask the model to answer the new
// message, so the request ends with it, not with the reply the first turn
// stored after it arrived.
func TestRespondToChat_CollectedMessageIsAnsweredAfterTheReply(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	events := evStream(
		evTextBlock(0, "First part."),
		evTextBlock(1, "Second part."),
		evMessageEnd("end_turn"),
	)
	stub, open := newGatedAnthropicStub(t, events, 4) // message_start and the first text block
	b := historyTestBot(t, stub)
	const chatID = int64(778)
	storeUserText(t, b, chatID, "tell me about the farm")

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.respondToChat(textReq(chatID))
	}()
	// The first segment is on screen: the turn is committed and can only be
	// collected behind, not restarted.
	require.Eventually(t, func() bool {
		b.turnsMu.Lock()
		defer b.turnsMu.Unlock()
		running, ok := b.turns[chatID]
		return ok && running.committed
	}, 5*time.Second, 5*time.Millisecond)

	storeUserText(t, b, chatID, "and the candle farm?")
	b.respondToChat(textReq(chatID)) // collected behind the running turn
	open()
	<-done

	require.Eventually(t, func() bool { return len(stub.requestBetas()) == 2 && chatIdle(b, chatID) },
		5*time.Second, 5*time.Millisecond, "the collected message runs as its own turn")
	role, text := stub.lastMessage(t, 1)
	assert.Equal(t, "user", role, "the request must end with the message it answers")
	assert.Equal(t, "and the candle farm?", text)
}

func TestGetAnthropicResponse_RefusesHistoryEndingWithTheBot(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b := historyTestBot(t, stub)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	for name, messages := range map[string][]anthropic.BetaMessageParam{
		"ends with the bot": {
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hi")),
			{OfAssistant: &anthropic.BetaAssistantMessageParam{Content: []anthropic.BetaContentBlockParamUnion{
				anthropic.NewBetaTextBlock("hello"),
			}}},
		},
		"empty": nil,
	} {
		_, err := b.getAnthropicResponse(ctx, 42, messages, false, "alice", "", "", false, "en", 0, replySinks{})
		assert.ErrorIs(t, err, errNothingToAnswer, name)
	}
	assert.Empty(t, stub.requestBetas(), "nothing is sent")
}

// A sticker that lands just as a text turn starts is read by that turn, which
// answers both. The sticker's own turn waited for it and then has nothing
// left to answer: no second model call, and no canned sticker reply either.
func TestRunStickerTurn_AnsweredByTheTurnItWaitedForSaysNothing(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	var mu sync.Mutex
	var sent []string
	tg.On("SendMessage", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, args.Get(1).(*bot.SendMessageParams).Text)
	}).Return(&models.Message{ID: 1}, nil).Maybe()
	tg.On("SendChatAction", mock.Anything, mock.Anything).Return(true, nil).Maybe()
	const chatID = int64(779)
	storeUserText(t, b, chatID, "look at this")
	sticker := b.createMessage(chatID, 555, "tester", "user", "Sent a sticker: 😀", true)
	sticker.StickerFileID = "sticker-file"
	require.NoError(t, b.storeMessage(&sticker))
	b.addMessageToChatMemory(b.getOrCreateChatMemory(chatID), sticker)

	b.respondToChat(textReq(chatID)) // the text turn reads both and answers
	require.Len(t, stub.requestBetas(), 1)
	mu.Lock()
	answered := len(sent)
	mu.Unlock()

	b.runStickerTurn(context.Background(), replyTarget{chatID: chatID}, sticker,
		&models.Message{Sticker: &models.Sticker{FileID: "sticker-file", Emoji: "😀"}})

	assert.Len(t, stub.requestBetas(), 1, "no second model call")
	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, sent, answered, "no canned sticker reply either")
}

// A turn's reply is recorded right after the newest message its context
// included. A message that arrived while it ran stays after the reply, and
// stays unanswered until a turn of its own answers it.
func TestRecordTurnReply_GoesWhereTheTurnsContextEnded(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	const chatID = int64(780)
	mem := b.getOrCreateChatMemory(chatID)
	first := storeUserText(t, b, chatID, "first")
	turn, _, ok := b.beginTextTurn(textReq(chatID), first.ID)
	require.True(t, ok)
	b.snapshotForTurn(turn, mem)
	late := storeUserText(t, b, chatID, "arrived during the turn")

	_, err := b.recordTurnReply(turn, chatID, "answer to first")
	require.NoError(t, err)
	b.finishTurn(turn)

	var texts []string
	for _, m := range mem.Messages {
		texts = append(texts, m.Text)
	}
	assert.Equal(t, []string{"first", "answer to first", "arrived during the turn"}, texts)
	var readByTurn, arrivedLate Message
	require.NoError(t, b.db.First(&readByTurn, first.ID).Error)
	assert.NotNil(t, readByTurn.AnsweredOn, "the message the turn read is answered")
	require.NoError(t, b.db.First(&arrivedLate, late.ID).Error)
	assert.Nil(t, arrivedLate.AnsweredOn, "the message that arrived during the turn is not")
}
