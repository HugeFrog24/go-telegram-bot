package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// telegramRecord reads back what a test bot sent to Telegram.
type telegramRecord struct {
	sent    func() []bot.SendMessageParams
	actions func() []bot.SendChatActionParams
}

// historyTestBot is a bot whose Anthropic client talks to stub (when given)
// and whose Telegram sends and chat actions all succeed and are recorded. Its
// database takes one connection at a time, as in production, so handlers on
// several goroutines share the one in-memory database.
func historyTestBot(t *testing.T, stub *anthropicStub) (*Bot, telegramRecord) {
	t.Helper()
	b, tg := setupBotForTest(t, 123)
	if stub != nil {
		useAnthropicStub(b, stub)
	}
	sqlDB, err := b.db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return b, telegramRecord{sent: recordSends(tg), actions: recordChatActions(tg)}
}

// storeUserText puts a user message into the database and chat memory, as a
// text message that joins the conversation does at intake, and returns the
// memory copy, which carries its sequence. Memory is loaded before the store,
// as at intake, so a first load cannot read the new row and add it twice.
func storeUserText(t *testing.T, b *Bot, chatID int64, text string) Message {
	t.Helper()
	b.getOrCreateChatMemory(chatID)
	m := b.createMessage(chatID, 555, "tester", "user", text, true)
	require.NoError(t, b.storeMessage(&m))
	return b.joinConversation(m)
}

// answerInTurn records reply as a turn's answer to everything in the chat's
// memory, as a text turn would, without a model.
func answerInTurn(t *testing.T, b *Bot, chatID int64, reply string) {
	t.Helper()
	turn, _, ok := b.beginTextTurn(textReq(chatID), b.newestAnswerableSeqInChat(chatID))
	require.True(t, ok)
	defer b.finishTurn(turn)
	b.snapshotForTurn(turn, b.getOrCreateChatMemory(chatID))
	_, err := b.recordTurnReply(turn, chatID, reply)
	require.NoError(t, err)
}

func chatIdle(b *Bot, chatID int64) bool {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	_, busy := b.turns[chatID]
	return !busy
}

// turnsRegistered is how many turns the bot has registered so far.
func turnsRegistered(b *Bot) uint64 {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	return b.turnSeq
}

// memoryTexts is the text of every message in the chat's memory, in order.
func memoryTexts(b *Bot, chatID int64) []string {
	mem := b.getOrCreateChatMemory(chatID)
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	texts := make([]string, 0, len(mem.Messages))
	for _, m := range mem.Messages {
		texts = append(texts, m.Text)
	}
	return texts
}

// dropChatMemory forgets the chat's memory, so the next use reloads it from
// the database, as after a restart.
func dropChatMemory(b *Bot, chatID int64) {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	delete(b.chatMemories, chatID)
}

func answeredOn(t *testing.T, b *Bot, id uint) *time.Time {
	t.Helper()
	var stored Message
	require.NoError(t, b.db.First(&stored, id).Error)
	return stored.AnsweredOn
}

// The production 400 "This model does not support assistant message
// prefill": a second message lands just after the first one's quiet window
// closed, so the turn that answers the first reads both; the second message's
// own window then closes after that turn has ended. Its flush must not ask the
// model again, register a turn, or show "typing".
func TestRespondToChat_LateFlushForAnsweredMessageCallsNoModel(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	const chatID = int64(777)
	storeUserText(t, b, chatID, "hi")
	storeUserText(t, b, chatID, "where is the candle farm?")

	b.respondToChat(textReq(chatID)) // answers both, stores the reply, ends
	require.Len(t, stub.requestBetas(), 1)
	turns, typing := turnsRegistered(b), len(tg.actions())

	b.respondToChat(textReq(chatID)) // the second message's flush, after the turn ended

	assert.Len(t, stub.requestBetas(), 1, "an already answered message gets no second model call")
	assert.Equal(t, turns, turnsRegistered(b), "no turn is registered for it")
	assert.Len(t, tg.actions(), typing, "and no typing is shown")
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
	b, _ := historyTestBot(t, stub)
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
	b, _ := historyTestBot(t, stub)
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

// A turn's reply is recorded right after the newest message its context
// included. A message that arrived while it ran stays after the reply, and
// stays unanswered until a turn of its own answers it.
func TestRecordTurnReply_GoesWhereTheTurnsContextEnded(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	const chatID = int64(780)
	mem := b.getOrCreateChatMemory(chatID)
	first := storeUserText(t, b, chatID, "first")
	turn, _, ok := b.beginTextTurn(textReq(chatID), first.Seq)
	require.True(t, ok)
	b.snapshotForTurn(turn, mem)
	late := storeUserText(t, b, chatID, "arrived during the turn")

	_, err := b.recordTurnReply(turn, chatID, "answer to first")
	require.NoError(t, err)
	b.finishTurn(turn)

	assert.Equal(t, []string{"first", "answer to first", "arrived during the turn"}, memoryTexts(b, chatID))
	assert.NotNil(t, answeredOn(t, b, first.ID), "the message the turn read is answered")
	assert.Nil(t, answeredOn(t, b, late.ID), "the message that arrived during the turn is not")
}

// Handlers run concurrently, so a message can be stored before another yet
// reach memory after it. A turn that did not read it must not count it as
// answered, and its own flush must still answer it (review finding 4).
func TestRespondToChat_MessageThatReachedMemoryLateIsStillAnswered(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, candleFarmStream(), 1)
	b, _ := historyTestBot(t, stub)
	const chatID = int64(785)
	b.getOrCreateChatMemory(chatID) // loaded before the stores, as at intake
	first := b.createMessage(chatID, 555, "tester", "user", "first, stored first", true)
	require.NoError(t, b.storeMessage(&first))
	second := b.createMessage(chatID, 556, "other", "user", "second, stored second", true)
	require.NoError(t, b.storeMessage(&second))
	b.joinConversation(second) // second's handler reaches memory first

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.respondToChat(textReq(chatID))
	}()
	// The request is out, so the turn has read memory: second only.
	require.Eventually(t, func() bool { return len(stub.requestBetas()) == 1 }, 5*time.Second, 5*time.Millisecond)
	b.joinConversation(first) // first's handler reaches memory now
	open()
	<-done

	assert.Nil(t, answeredOn(t, b, first.ID), "the model never saw first")
	b.respondToChat(textReq(chatID)) // first's flush
	require.Len(t, stub.requestBetas(), 2, "first gets an answer of its own")
	role, text := stub.lastMessage(t, 1)
	assert.Equal(t, "user", role)
	assert.Equal(t, "first, stored first", text)
}

// After a restart, memory reloads from the database. A message the bot had
// already answered stays answered: its late flush calls no model, registers
// no turn and shows no "typing".
func TestReload_AnsweredMessageStaysAnswered(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	const chatID = int64(786)
	storeUserText(t, b, chatID, "where is the candle farm?")
	b.respondToChat(textReq(chatID))
	require.Len(t, stub.requestBetas(), 1)
	dropChatMemory(b, chatID)
	turns, typing := turnsRegistered(b), len(tg.actions())

	b.respondToChat(textReq(chatID))

	assert.Len(t, stub.requestBetas(), 1, "no second model call")
	assert.Equal(t, turns, turnsRegistered(b), "no turn is registered")
	assert.Len(t, tg.actions(), typing, "and no typing is shown")
}

// The frontier message was evicted during a long turn: the reply goes to the
// front and is trimmed straight away, without a panic.
func TestRecordTurnReply_FrontierEvicted(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	const chatID = int64(787)
	mem := b.getOrCreateChatMemory(chatID)
	first := storeUserText(t, b, chatID, "first")
	turn, _, ok := b.beginTextTurn(textReq(chatID), first.Seq)
	require.True(t, ok)
	b.snapshotForTurn(turn, mem)
	for i := 0; i < mem.Size; i++ {
		storeUserText(t, b, chatID, "flood")
	}
	_, err := b.recordTurnReply(turn, chatID, "answer to first")
	require.NoError(t, err)
	b.finishTurn(turn)

	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	for _, m := range mem.Messages {
		assert.True(t, m.IsUser, "the reply was trimmed at once")
	}
	assert.Len(t, mem.Messages, mem.Size)
}

// An error reply goes where the turn's context ended, like an answer: the
// message that arrived during the turn stays after it and is answered next.
func TestRunTurn_ErrorReplyIsFiledWhereTheContextEnded(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	const chatID = int64(-1003) // a group: no draft
	first := storeUserText(t, b, chatID, "first")
	req := textReq(chatID)
	turn, turnCtx, ok := b.beginTextTurn(req, first.Seq)
	require.True(t, ok)
	boom := errors.New("boom")
	var late Message

	b.runTurn(turn, turnCtx, req, func(context.Context, replyOutput) (string, error) {
		b.snapshotForTurn(turn, b.getOrCreateChatMemory(chatID))
		late = storeUserText(t, b, chatID, "arrived during the turn")
		return "", boom
	})

	assert.Equal(t, []string{"first", b.anthropicErrorResponse(boom, req.userID), "arrived during the turn"},
		memoryTexts(b, chatID))
	assert.Nil(t, answeredOn(t, b, late.ID))
	b.respondToChat(req)
	require.Len(t, stub.requestBetas(), 1)
	role, text := stub.lastMessage(t, 0)
	assert.Equal(t, "user", role)
	assert.Equal(t, "arrived during the turn", text)
}

// A stopped partial goes where the turn's context ended, like an answer.
func TestRunTurn_StoppedPartialIsFiledWhereTheContextEnded(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := historyTestBot(t, nil)
	b.config.StreamDrafts = true
	const chatID = int64(789) // a private chat: drafts
	first := storeUserText(t, b, chatID, "first")
	req := textReq(chatID)
	turn, turnCtx, ok := b.beginTextTurn(req, first.Seq)
	require.True(t, ok)
	var late Message

	b.runTurn(turn, turnCtx, req, func(ctx context.Context, out replyOutput) (string, error) {
		b.snapshotForTurn(turn, b.getOrCreateChatMemory(chatID))
		out.onProgress("Partial answer")
		waitDraftShown(t, b, turn)
		late = storeUserText(t, b, chatID, "arrived during the turn")
		require.True(t, b.stopTurn(chatID, int(turn.seq)))
		<-ctx.Done()
		return "", ctx.Err()
	})

	assert.Equal(t, []string{"first", "Partial answer", "arrived during the turn"}, memoryTexts(b, chatID))
	assert.NotNil(t, answeredOn(t, b, first.ID))
	assert.Nil(t, answeredOn(t, b, late.ID))
}

// Messages from many senders reach memory at once. Every one gets its own
// sequence, increasing along memory order, and each sender's messages keep
// the order they were sent in.
func TestChatMemory_SequencesStayConsistentUnderConcurrentIntake(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := historyTestBot(t, nil)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.memorySize = 50
	const chatID = int64(-1002)
	const senders, each = 8, 4 // within each sender's hourly limit

	var wg sync.WaitGroup
	stop := make(chan struct{})
	readers := make(chan struct{})
	go func() { // a reader racing the writers
		defer close(readers)
		for {
			select {
			case <-stop:
				return
			default:
				b.newestAnswerableSeqInChat(chatID)
			}
		}
	}()
	for s := 0; s < senders; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
					ID:   s*100 + i,
					Chat: models.Chat{ID: chatID, Type: models.ChatTypeSupergroup},
					From: &models.User{ID: int64(1000 + s), Username: fmt.Sprintf("sender%d", s)},
					Text: fmt.Sprintf("%d:%d", s, i),
				}})
			}
		}(s)
	}
	wg.Wait()
	close(stop)
	<-readers

	mem := b.getOrCreateChatMemory(chatID)
	b.chatMemoriesMu.RLock()
	msgs := slices.Clone(mem.Messages)
	b.chatMemoriesMu.RUnlock()
	require.Len(t, msgs, senders*each)
	last := map[int64]int{}
	for i, m := range msgs {
		if i > 0 {
			assert.Greater(t, m.Seq, msgs[i-1].Seq, "sequences increase along memory order")
		}
		if prev, seen := last[m.UserID]; seen {
			assert.Greater(t, m.TelegramMessageID, prev, "a sender's messages keep their order")
		}
		last[m.UserID] = m.TelegramMessageID
	}
	assert.Equal(t, msgs[len(msgs)-1].Seq, b.newestAnswerableSeqInChat(chatID))
}
