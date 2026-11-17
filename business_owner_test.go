package main

import (
	"context"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Business chat's messages from the business account itself (the user who
// made the connection) are what the owner told the customer by hand: kept as
// the business side of the conversation, never answered (user decision B9).

// shopChat is the Business chat of the test shop with the customer.
var shopChat = convKey{businessConnectionID: "bc1", chatID: customer.ID}

// ownerText is a message the shop's owner sends the customer by hand.
func ownerText(msgID int, text string) *models.Update {
	return &models.Update{BusinessMessage: &models.Message{
		ID: msgID, Chat: models.Chat{ID: customer.ID, Type: models.ChatTypePrivate}, From: shop,
		BusinessConnectionID: "bc1", Text: text,
	}}
}

// B9: an owner's message alone starts no turn and sends nothing; it is
// stored as the business side of the conversation, and joins memory.
func TestOwner_AMessageAloneIsKeptButNotAnswered(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)

	b.handleUpdate(context.Background(), nil, ownerText(70, "OWNER: we open at 9"))
	waitIdle(t, b, customer.ID)

	assert.Empty(t, stub.requestBetas(), "no model turn")
	assert.Empty(t, tg.sent(), "nothing sent")
	row := storedRow(t, b, customer.ID, "OWNER: we open at 9")
	assert.False(t, row.IsUser, "the business side of the conversation")
	assert.False(t, row.Aside)
	assert.Equal(t, shop.ID, row.UserID, "written by the owner")
	assert.Nil(t, row.AnsweredOn)
	assert.Equal(t, []string{"OWNER: we open at 9"}, memoryIn(b, shopChat))
}

// B9: the owner's message, then the customer's, are one turn whose context
// holds both, in order: the owner's as the business's (assistant) side, the
// request ending with the customer's message.
func TestOwner_ThenTheCustomerIsOneTurnReadingBoth(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)

	b.handleUpdate(context.Background(), nil, ownerText(70, "OWNER: we open at 9"))
	b.handleUpdate(context.Background(), nil, businessText("bc1", 71, customer, "CUSTOMER: and on Sunday?"))
	waitIdle(t, b, customer.ID)

	require.Len(t, stub.requestBetas(), 1, "one turn")
	msgs := requestMessages(t, stub.body(t, 0))
	require.Len(t, msgs, 2)
	assert.Equal(t, "assistant", msgs[0].Role)
	assert.Equal(t, "OWNER: we open at 9", msgs[0].Text)
	assert.Equal(t, "user", msgs[1].Role)
	assert.Equal(t, "CUSTOMER: and on Sunday?", msgs[1].Text)
	require.Len(t, tg.sent(), 1)
	assert.Equal(t, "bc1", tg.sent()[0].BusinessConnectionID)
}

// B9 with I1: an owner's message landing while the customer's question waits
// in its quiet window does not end the question's request: the request ends
// with the question, and the owner's message, which came after it, stays
// after the reply.
func TestOwner_AMessageAfterAWaitingQuestionDoesNotEndItsRequest(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	errs := captureErrors(t)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)

	b.handleUpdate(context.Background(), nil, businessText("bc1", 80, customer, "CUSTOMER: price?"))
	b.handleUpdate(context.Background(), nil, ownerText(81, "OWNER: it's $5"))
	flushPending(t, b, customer.ID)
	waitIdle(t, b, customer.ID)

	require.Len(t, stub.requestBetas(), 1)
	role, text := stub.lastMessage(t, 0)
	assert.Equal(t, "user", role, "the request ends with a user message")
	assert.Equal(t, "CUSTOMER: price?", text)
	assert.NotContains(t, errs.String(), "BUG")
	assert.Equal(t, []string{"CUSTOMER: price?", "Open the Farm tab.", "OWNER: it's $5"}, memoryIn(b, shopChat))

	dropMemory(b, shopChat) // after a restart the owner's message is still not a reply
	b.handleUpdate(context.Background(), nil, businessText("bc1", 82, customer, "CUSTOMER: ok, two please"))
	flushPending(t, b, customer.ID)
	waitIdle(t, b, customer.ID)
	require.Len(t, stub.requestBetas(), 2)
	msgs := requestMessages(t, stub.body(t, 1))
	assert.Equal(t, []string{"user", "assistant", "assistant", "user"},
		[]string{msgs[0].Role, msgs[1].Role, msgs[2].Role, msgs[3].Role})
}

// B9: an owner's edit corrects the stored record, and starts nothing.
func TestOwner_AnEditUpdatesTheRecord(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	b.handleUpdate(context.Background(), nil, ownerText(70, "OWNER: we open at 9"))

	b.handleUpdate(context.Background(), nil, &models.Update{EditedBusinessMessage: &models.Message{
		ID: 70, Chat: models.Chat{ID: customer.ID, Type: models.ChatTypePrivate}, From: shop,
		BusinessConnectionID: "bc1", Text: "OWNER: we open at 10"}})
	waitIdle(t, b, customer.ID)

	assert.Equal(t, []string{"OWNER: we open at 10"}, memoryIn(b, shopChat))
	assert.Equal(t, []string{"OWNER: we open at 10"}, liveTexts(t, b, customer.ID, shop.ID))
	assert.Empty(t, stub.requestBetas())
	assert.Empty(t, tg.sent())
}

// B9: a message this bot sent on behalf of the business account
// (sender_business_bot is the bot) is already recorded as its reply: it is
// not stored again, and starts nothing.
func TestOwner_TheBotsOwnMessagesAreNotRecordedTwice(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	b.selfID = 777000111
	b.handleUpdate(context.Background(), nil, businessText("bc1", 90, customer, "CUSTOMER: hi"))
	waitIdle(t, b, customer.ID)
	require.Len(t, tg.sent(), 1)

	own := ownerText(91, tg.sent()[0].Text)
	own.BusinessMessage.SenderBusinessBot = &models.User{ID: b.selfID, IsBot: true, Username: "thisbot"}
	b.handleUpdate(context.Background(), nil, own)
	waitIdle(t, b, customer.ID)

	assert.Equal(t, []string{"CUSTOMER: hi", "Open the Farm tab."}, memoryIn(b, shopChat), "the reply is in memory once")
	assert.Equal(t, []string{"CUSTOMER: hi", "Open the Farm tab."}, liveIn(t, b, shopChat), "and stored once")
	assert.Len(t, stub.requestBetas(), 1)
}

// B9: after a reload, the owner's message is no reply of the bot's: a
// customer message before it is still unanswered, and its flush still runs.
func TestOwner_AReloadDoesNotTakeTheOwnersMessageForAReply(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.handleUpdate(context.Background(), nil, businessText("bc1", 80, customer, "CUSTOMER: price?"))
	b.handleUpdate(context.Background(), nil, ownerText(81, "OWNER: let me check"))

	dropMemory(b, shopChat) // reloaded before the question's window ends
	flushPending(t, b, customer.ID)
	waitIdle(t, b, customer.ID)

	require.Len(t, stub.requestBetas(), 1, "the question is answered")
	role, text := stub.lastMessage(t, 0)
	assert.Equal(t, "user", role)
	assert.Equal(t, "CUSTOMER: price?", text)
}

// B9: an owner's edit, while the customer's question waits in its quiet
// window, only corrects the record: it does not re-arm the window.
func TestOwner_AnEditDoesNotReArmTheCustomersWindow(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	logged := captureInfo(t)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.handleUpdate(context.Background(), nil, businessText("bc1", 80, customer, "CUSTOMER: price?"))
	b.handleUpdate(context.Background(), nil, ownerText(81, "OWNER: let me check"))

	b.handleUpdate(context.Background(), nil, &models.Update{EditedBusinessMessage: &models.Message{
		ID: 81, Chat: models.Chat{ID: customer.ID, Type: models.ChatTypePrivate}, From: shop,
		BusinessConnectionID: "bc1", Text: "OWNER: one moment"}})

	assert.NotContains(t, logged.String(), "re-armed the quiet window")
	assert.Contains(t, memoryIn(b, shopChat), "OWNER: one moment")
}

// B2/B9: /stats counts what it counted before owner messages were kept as the
// business side: the owner's messages are messages the owner sent, not
// messages the customer received from the bot.
func TestOwner_StatsCountTheOwnersMessagesAsBefore(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	b.handleUpdate(context.Background(), nil, ownerText(70, "OWNER: we open at 9"))
	b.handleUpdate(context.Background(), nil, businessText("bc1", 71, customer, "CUSTOMER: thanks"))
	waitIdle(t, b, customer.ID)

	_, ownerSent, ownerReceived, _, err := b.getUserStats(shop.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), ownerSent, "the owner's message counts as sent by the owner")
	assert.Equal(t, int64(1), ownerReceived, "the bot's reply in the chat")
	_, customerSent, customerReceived, _, err := b.getUserStats(customer.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), customerSent)
	assert.Equal(t, int64(1), customerReceived, "the bot's reply only, as before")

	b.sendStats(context.Background(), replyTarget{chatID: 123}, 123, 0) // the bot's owner sees the most active users
	sent := tg.sent()
	assert.Contains(t, sent[len(sent)-1].Text, "@shop — 1 messages", "the owner is among the most active users, as before")
}
