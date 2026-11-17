package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Guards for gaps the round-3 review's mutants found (G11).

// A message stored aside before /clear's cutoff that joins after the swap but
// before the delete has removed its row stays out of the new memory, and
// nothing of it comes back (the reviewer's guard probe; kills A16, no
// clearedThrough check).
func TestClear_JoinBetweenSwapAndDeleteStaysOut(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	oneConnection(t, b)
	recordSends(tg)
	early := storeAside(t, b, ownerChat, owner, "sent before the clear")
	var once sync.Once
	var joined bool
	require.NoError(t, b.db.Callback().Delete().Before("gorm:begin_transaction").Register("test:join_before_delete", func(db *gorm.DB) {
		if _, isMsg := db.Statement.Model.(*Message); !isMsg {
			return
		}
		once.Do(func() {
			done := make(chan struct{})
			go func() { defer close(done); _, joined = b.joinConversation(early) }()
			<-done
		})
	}))

	clearOwnChat(b, ownerChat, false)

	assert.False(t, joined, "the pre-clear message does not join")
	assert.Empty(t, memoryTexts(b, ownerChat))
	dropChatMemory(b, ownerChat)
	assert.Empty(t, memoryTexts(b, ownerChat))
}

// A message that arrives after /clear's swap, before its soft delete runs,
// keeps its row: it is above the cutoff (the reviewer's guard probe; kills
// A35, a soft delete without the cutoff).
func TestClear_SoftClearKeepsARowAboveTheCutoff(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	oneConnection(t, b)
	recordSends(tg)
	storeUserText(t, b, ownerChat, "old")
	var once sync.Once
	require.NoError(t, b.db.Callback().Delete().Before("gorm:begin_transaction").Register("test:new_before_delete", func(db *gorm.DB) {
		if _, isMsg := db.Statement.Model.(*Message); !isMsg {
			return
		}
		once.Do(func() {
			done := make(chan struct{})
			go func() { defer close(done); storeUserText(t, b, ownerChat, "new after the clear") }()
			<-done
		})
	}))

	clearOwnChat(b, ownerChat, false)

	assert.Equal(t, []string{"new after the clear"}, memoryTexts(b, ownerChat))
	dropChatMemory(b, ownerChat)
	assert.Equal(t, []string{"new after the clear"}, memoryTexts(b, ownerChat), "its row survives the clear")
}

// While this bot's username is unknown, a command addressed to any bot is
// ignored, and a bare command still works (kills A30).
func TestHandleUpdate_UnknownUsernameIgnoresAddressedCommands(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	sent := recordSends(tg)
	setBotUsername(b, "")
	user := &models.User{ID: 123, Username: "owner"}

	b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 1, user, "/stats@x"))
	assert.Empty(t, sent(), "ignored")
	b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 2, user, "/stats"))
	require.Len(t, sent(), 1)
	assert.Contains(t, sent()[0].Text, "Bot Statistics", "a bare command works")
	assert.Empty(t, stub.requestBetas())
}

// A text turn registered for a flush whose want and generation were read
// from the memory a /clear has since replaced (the clear landed between the
// read and the registration) ends silently, though the new memory holds a
// message to answer: that one gets its own turn, with its own sender (kills
// A38, no generation drop in runTextTurn).
func TestRunTextTurn_ATurnForTheClearedMemoryEndsSilently(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	recordSends(tg)
	b.config.SystemPrompts = map[string]string{"custom_instructions": "be helpful"} // the request carries the user context
	before := storeUserText(t, b, groupChatID, "before the clear")
	old := b.getOrCreateChatMemory(chatKey(groupChatID))
	req := textReq(groupChatID)
	req.memoryGen = old.generation
	clearOwnChat(b, groupChatID, false)
	storeTextFrom(t, b, groupChatID, alice, "after the clear")

	b.turnsMu.Lock()
	turn, turnCtx, ok := b.beginTextTurnLocked(req, before.Seq, old) // the memory read before the clear
	b.turnsMu.Unlock()
	require.True(t, ok)
	b.runTextTurn(turn, turnCtx, req)
	assert.Empty(t, stub.requestBetas(), "the old flush's turn ends silently")

	newReq := textReq(groupChatID)
	newReq.userID, newReq.username = alice.ID, alice.Username
	b.respondToChat(newReq)
	require.Len(t, stub.requestBetas(), 1)
	assert.True(t, requestHas(t, stub, 0, "@alice"), "the new message's own turn answers it")
}

// A panic in an album's flush, from its timer, is recovered, and the next
// update is handled (kills A50, no recover in the album flush).
func TestFlushAlbum_PanicInTheTimerIsRecovered(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	errs := captureErrorsLocked(t)
	sent := recordSends(tg)
	serveTelegramFiles(t, tg, nil)
	var panicked atomic.Bool
	tg.SendChatActionFunc = func(context.Context, *bot.SendChatActionParams) (bool, error) {
		if panicked.CompareAndSwap(false, true) {
			panic("boom in the album's typing: " + panicSecret)
		}
		return true, nil
	}
	album := photoUpdate(1800)
	album.Message.MediaGroupID = "album-panic"

	b.handleUpdate(context.Background(), nil, album)
	require.Eventually(t, func() bool { return strings.Contains(errs.String(), "PANIC recovered in album flush") },
		10*time.Second, 10*time.Millisecond)
	waitIdle(t, b, ownerChat)
	logged := errs.String()
	assert.Contains(t, logged, "[test_bot] PANIC recovered in album flush: string")
	assert.Contains(t, logged, "guard_test.go", "with a short stack")
	assert.NotContains(t, logged, panicSecret, "no message text")

	b.handleUpdate(context.Background(), nil, textUpdate(ownerChat, 1801, owner, "and the bakery?"))
	waitIdle(t, b, ownerChat)
	assert.Equal(t, []string{"Open the Farm tab."}, sentTexts(sent), "the next update is handled")
}
