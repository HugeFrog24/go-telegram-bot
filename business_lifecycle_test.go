package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B10 (external review EXT-12): the bot follows a Business connection's
// lifecycle: a connection disabled, or whose right to reply is revoked,
// stops its work; messages deleted in a Business chat leave the
// conversation.

// B10: a connection disabled while its turn runs stops the turn, which sends
// and files nothing, and drops the connection's waiting batch; another
// connection's conversation goes on.
func TestBusiness_ADisabledConnectionStopsItsTurnAndBatch(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, candleFarmStream(), 1)
	b, tg := historyTestBot(t, stub)
	shopper := &models.User{ID: 8101, Username: "shopper"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleUpdate(context.Background(), nil, businessText("bc1", 1, customer, "ASK: is it in stock?"))
	}()
	require.Eventually(t, func() bool { return len(stub.requestBetas()) == 1 }, 5*time.Second, 5*time.Millisecond)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.handleUpdate(context.Background(), nil, businessText("bc1", 2, shopper, "WAITING: and the price?"))
	b.handleUpdate(context.Background(), nil, businessText("bc2", 3, customer, "OTHER SHOP: hello"))
	require.True(t, b.hasPendingIntake(convKey{businessConnectionID: "bc1", chatID: shopper.ID}))

	b.handleUpdate(context.Background(), nil, connectionUpdate("bc1", testShopID, false))
	open()
	<-done
	waitAllIdle(t, b)

	for _, s := range tg.sent() {
		assert.NotEqual(t, "bc1", s.BusinessConnectionID, "nothing is sent through the disabled connection")
	}
	assert.Empty(t, liveTexts(t, b, customer.ID, 0), "and no reply is filed for it")
	assert.False(t, b.hasPendingIntake(convKey{businessConnectionID: "bc1", chatID: shopper.ID}), "its waiting batch is dropped")
	assert.True(t, b.hasPendingIntake(convKey{businessConnectionID: "bc2", chatID: customer.ID}), "another connection's is not")
}

// B10: a connection whose right to reply is revoked is as good as disabled:
// the bot sends nothing through it, even from a turn that started before.
func TestBusiness_NoSendThroughAConnectionThatMayNotReply(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	sent := recordSends(tg)
	revoked := connectionUpdate("bc1", testShopID, true)
	revoked.BusinessConnection.Rights = &models.BusinessBotRights{CanReply: false}
	b.handleUpdate(context.Background(), nil, revoked)

	err := b.sendOneSegment(context.Background(), replyTarget{chatID: customer.ID, businessConnectionID: "bc1", shared: true}, "late reply")

	assert.Error(t, err)
	assert.Empty(t, sent(), "nothing reaches Telegram")
	require.NoError(t, b.sendOneSegment(context.Background(), replyTarget{chatID: customer.ID}, "direct chat"))
	assert.Len(t, sent(), 1, "the direct chat is unaffected")
}

// B10: messages deleted in a Business chat (deleted_business_messages)
// leave the conversation's memory and the next request, their rows are
// soft-deleted in that conversation only, and their uploaded files go.
func TestBusiness_DeletedMessagesLeaveMemoryAndTheNextRequest(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	serveTelegramFiles(t, b.tgBot.(*MockTelegramClient), nil)
	b.handleUpdate(context.Background(), nil, businessText("bc1", 10, customer, "SECRET: my card is 4111"))
	b.handleUpdate(context.Background(), nil, &models.Update{BusinessMessage: &models.Message{
		ID: 11, Chat: models.Chat{ID: customer.ID, Type: models.ChatTypePrivate}, From: customer, BusinessConnectionID: "bc1",
		Photo: []models.PhotoSize{{FileID: "p", Width: 1, Height: 1}}, Caption: "PHOTO of the card"}})
	b.handleUpdate(context.Background(), nil, directText(10, customer, "DIRECT message with the same id"))
	waitAllIdle(t, b)
	require.Len(t, stub.requestBetas(), 3)

	b.handleUpdate(context.Background(), nil, &models.Update{DeletedBusinessMessages: &models.BusinessMessagesDeleted{
		BusinessConnectionID: "bc1", Chat: models.Chat{ID: customer.ID, Type: models.ChatTypePrivate}, MessageIDs: []int{10, 11}}})

	for _, text := range memoryIn(b, shopChat) {
		assert.NotContains(t, text, "SECRET")
		assert.NotContains(t, text, "PHOTO")
	}
	live := liveIn(t, b, shopChat)
	assert.NotContains(t, live, "SECRET: my card is 4111", "its row is deleted")
	assert.NotContains(t, live, "PHOTO of the card")
	assert.Contains(t, liveIn(t, b, direct), "DIRECT message with the same id", "the direct chat's message with that id stays")
	require.Eventually(t, func() bool { return len(stub.deletedFiles()) == 1 }, 5*time.Second, 5*time.Millisecond,
		"the photo's upload is deleted")

	b.handleUpdate(context.Background(), nil, businessText("bc1", 12, customer, "NEXT: anything else?"))
	waitAllIdle(t, b)
	require.Len(t, stub.requestBetas(), 4)
	next := string(stub.body(t, 3))
	assert.NotContains(t, next, "SECRET", "the next request holds none of it")
	assert.NotContains(t, next, "file_stub")
	assert.True(t, strings.Contains(next, "NEXT: anything else?"))
}

// B10: a deletion is applied even after the connection was disabled: it only
// takes rows of that connection's own conversation, and the customer's
// deletion should take their messages whatever the business did.
func TestBusiness_ADeletionAfterTheDisableStillApplies(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	b.handleUpdate(context.Background(), nil, businessText("bc1", 10, customer, "SECRET: my card is 4111"))
	waitAllIdle(t, b)
	b.handleUpdate(context.Background(), nil, connectionUpdate("bc1", testShopID, false))

	b.handleUpdate(context.Background(), nil, &models.Update{DeletedBusinessMessages: &models.BusinessMessagesDeleted{
		BusinessConnectionID: "bc1", Chat: models.Chat{ID: customer.ID, Type: models.ChatTypePrivate}, MessageIDs: []int{10}}})

	assert.NotContains(t, liveIn(t, b, shopChat), "SECRET: my card is 4111")
}
