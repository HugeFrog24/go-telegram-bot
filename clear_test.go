package main

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const clearedNotice = "Your chat history has been cleared."

// captureErrors records ErrorLogger for the rest of the test.
func captureErrors(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	prev := ErrorLogger
	ErrorLogger = log.New(&logged, "", 0)
	t.Cleanup(func() { ErrorLogger = prev })
	return &logged
}

func clearUpdate(chatID int64) *models.Update {
	return commandUpdate(chatID, 970, &models.User{ID: 123, Username: "owner"}, "/clear")
}

func sentTexts(sent func() []bot.SendMessageParams) []string {
	var texts []string
	for _, s := range sent() {
		texts = append(texts, s.Text)
	}
	return texts
}

// Scoped P3 inverted: /clear lands after a text turn registered but before it
// read memory. The turn is cancelled silently: no model call, no "BUG" log,
// no error reply after the confirmation, nothing filed into the new memory.
func TestClear_BetweenRegisterAndSnapshotCancelsTheTurnSilently(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	sent := recordSends(tg)
	errs := captureErrors(t)
	const chatID = int64(123)
	storeUserText(t, b, chatID, "hi")
	gate := make(chan struct{})
	entered := make(chan struct{}, 4)
	tg.SendChatActionFunc = func(context.Context, *bot.SendChatActionParams) (bool, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
		return true, nil
	}
	done := make(chan struct{})
	go func() { defer close(done); b.respondToChat(textReq(chatID)) }()
	<-entered // the turn is registered; its first typing action is in flight

	b.handleUpdate(context.Background(), nil, clearUpdate(chatID))
	close(gate)
	<-done
	waitIdle(t, b, chatID)

	assert.Empty(t, stub.requestBetas(), "no model call")
	assert.Equal(t, []string{clearedNotice}, sentTexts(sent), "nothing after the confirmation")
	assert.NotContains(t, errs.String(), "BUG")
	assert.Empty(t, memoryTexts(b, chatID), "nothing filed into the new memory")
}

// A turn that read memory before /clear and is still streaming when it lands
// ends silently: nothing sent after the confirmation, nothing filed, and the
// flush collected behind it does not rerun.
func TestClear_DuringTheModelCallDropsTheTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	events := evStream(evTextBlock(0, "First part."), evTextBlock(1, "Second part."), evMessageEnd("end_turn"))
	stub, open := newGatedAnthropicStub(t, events, 4) // message_start and the first text block
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	sent := recordSends(tg)
	errs := captureErrors(t)
	const chatID = int64(123)
	storeUserText(t, b, chatID, "hi")
	done := make(chan struct{})
	go func() { defer close(done); b.respondToChat(textReq(chatID)) }()
	// The first segment is out: the turn is committed, so the next flush is
	// collected behind it rather than restarting it.
	require.Eventually(t, func() bool {
		b.turnsMu.Lock()
		defer b.turnsMu.Unlock()
		running, ok := b.turns[chatID]
		return ok && running.committed
	}, 5*time.Second, 5*time.Millisecond)
	storeUserText(t, b, chatID, "and another thing")
	b.respondToChat(textReq(chatID)) // collected behind the turn

	b.handleUpdate(context.Background(), nil, clearUpdate(chatID))
	open()
	<-done
	waitIdle(t, b, chatID)

	assert.Len(t, stub.requestBetas(), 1, "no rerun")
	assert.Equal(t, []string{"First part.", clearedNotice}, sentTexts(sent), "nothing after the confirmation")
	assert.Empty(t, errs.String())
	assert.Empty(t, memoryTexts(b, chatID), "nothing filed")
}

// A turn's reply recorded after /clear dropped the memory the turn read is
// stored aside, and stays out of the new conversation.
func TestRecordTurnReply_AfterClearIsAside(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	recordSends(tg)
	const chatID = int64(123)
	first := storeUserText(t, b, chatID, "hi")
	turn, _, ok := b.beginTextTurn(textReq(chatID), first.Seq)
	require.True(t, ok)
	b.snapshotForTurn(turn, b.getOrCreateChatMemory(chatID))
	b.clearChatHistory(context.Background(), replyTarget{chatID: chatID}, 123, 0, 0, false)
	storeUserText(t, b, chatID, "a new start")

	reply, err := b.recordTurnReply(turn, chatID, "an answer to the cleared history")
	require.NoError(t, err)
	b.finishTurn(turn)

	assert.True(t, reply.Aside)
	assert.True(t, storedRow(t, b, chatID, "an answer to the cleared history").Aside)
	assert.Equal(t, []string{"a new start"}, memoryTexts(b, chatID))
	dropChatMemory(b, chatID)
	assert.Equal(t, []string{"a new start"}, memoryTexts(b, chatID))
}

// Blind P7 inverted: /clear lands between a photo's store and its join. The
// photo joins the new memory, and its turn reads that memory: the request
// holds the photo and none of the cleared history, and the reply is filed
// after the photo.
func TestHandlePhotoMessage_ClearBetweenStoreAndJoinReadsTheNewMemory(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	sqlDB, err := b.db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	recordSends(tg)
	serveTelegramFiles(t, tg, nil)
	const chatID = int64(123)
	storeUserText(t, b, chatID, "OLD-SECRET before clear")
	var fired atomic.Bool
	require.NoError(t, b.db.Callback().Create().After("gorm:create").Register("test:clear_mid_photo", func(db *gorm.DB) {
		if m, ok := db.Statement.Dest.(*Message); ok && len(m.ImageFileIDs) > 0 && fired.CompareAndSwap(false, true) {
			// what clearChatHistory does, landing between the store and the join
			db.Session(&gorm.Session{NewDB: true}).Where("chat_id = ? AND id <> ?", chatID, m.ID).Delete(&Message{})
			dropChatMemory(b, chatID)
		}
	}))

	b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
		ID: 971, Chat: models.Chat{ID: chatID}, From: &models.User{ID: 123, Username: "owner"},
		Photo: []models.PhotoSize{{FileID: "p", Width: 1, Height: 1}},
	}})
	waitIdle(t, b, chatID)

	require.True(t, fired.Load())
	require.Len(t, stub.requestBetas(), 1)
	body := string(stub.body(t, 0))
	assert.Contains(t, body, "file_stub", "the request holds the photo")
	assert.False(t, strings.Contains(body, "OLD-SECRET"), "and none of the cleared history")
	assert.Equal(t, []string{"", "Open the Farm tab."}, memoryTexts(b, chatID), "the reply follows the photo")
}
