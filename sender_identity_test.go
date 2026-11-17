package main

import (
	"context"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Telegram's stand-in senders: every "send as channel" message in a group
// carries From = Channel_Bot, every anonymous admin's From =
// GroupAnonymousBot, with the real sender in sender_chat.
var (
	channelBot     = &models.User{ID: 136817688, IsBot: true, Username: "Channel_Bot", FirstName: "Channel"}
	anonymousAdmin = &models.User{ID: 1087968824, IsBot: true, Username: "GroupAnonymousBot", FirstName: "Group"}
	channelA       = &models.Chat{ID: -1001111, Type: models.ChatTypeChannel, Title: "Channel A", Username: "chan_a"}
	channelB       = &models.Chat{ID: -1002222, Type: models.ChatTypeChannel, Title: "Channel B", Username: "chan_b"}
)

// asChannel sends u's message on behalf of channel ch.
func asChannel(u *models.Update, ch *models.Chat) *models.Update {
	u.Message.From, u.Message.SenderChat = channelBot, ch
	return u
}

// asAnonymousAdmin sends u's message as an anonymous admin of its group.
func asAnonymousAdmin(u *models.Update) *models.Update {
	u.Message.From, u.Message.SenderChat = anonymousAdmin, &models.Chat{ID: u.Message.Chat.ID, Type: u.Message.Chat.Type}
	return u
}

// Blind prefill r3 #3 inverted (B11): a member posting as channel B cannot
// clear, with a bare /clear, what another member posted as channel A: each
// channel is a sender of its own, though Telegram gives both the same From.
func TestSender_AChannelPosterCannotClearAnotherChannelsPosts(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	b.config.MessagePerHour, b.config.MessagePerDay = 100, 100
	b.handleUpdate(context.Background(), nil, asChannel(textUpdate(groupChatID, 8999, channelBot, "A-POST: posted as channel A"), channelA))
	b.handleUpdate(context.Background(), nil, asChannel(textUpdate(groupChatID, 9000, channelBot, "B-POST: posted as channel B"), channelB))
	waitIdle(t, b, groupChatID)

	b.handleUpdate(context.Background(), nil, asChannel(commandUpdate(groupChatID, 9001, channelBot, "/clear"), channelB))

	assert.Contains(t, sentTexts(tg.sent), ownClearedNotice)
	assert.Equal(t, []string{"A-POST: posted as channel A"}, liveTexts(t, b, groupChatID, channelA.ID), "channel A's post stays")
	assert.Empty(t, liveTexts(t, b, groupChatID, channelB.ID), "channel B's own post went")
	assert.Contains(t, memoryTexts(b, groupChatID), "A-POST: posted as channel A")
	assert.NotContains(t, memoryTexts(b, groupChatID), "B-POST: posted as channel B")
}

// Blind prefill r3 #3 (B11 decision): anonymous admins all post as the group
// itself, so the bot cannot tell one's messages from another's: an anonymous
// admin's bare /clear of "their own" messages is refused, and nothing goes.
func TestSender_AnAnonymousAdminsOwnClearIsRefused(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	b.handleUpdate(context.Background(), nil, asAnonymousAdmin(textUpdate(groupChatID, 9010, anonymousAdmin, "ANON-1: written by admin one")))
	waitIdle(t, b, groupChatID)

	b.handleUpdate(context.Background(), nil, asAnonymousAdmin(commandUpdate(groupChatID, 9011, anonymousAdmin, "/clear")))

	assert.Contains(t, sentTexts(tg.sent), anonymousClearRefused)
	assert.NotContains(t, sentTexts(tg.sent), ownClearedNotice)
	assert.Contains(t, liveTexts(t, b, groupChatID, groupChatID), "ANON-1: written by admin one", "nothing was deleted")
	assert.Contains(t, memoryTexts(b, groupChatID), "ANON-1: written by admin one")
}

// Blind prefill r3 #3 inverted (B11): channel posters do not share one
// command limit.
func TestSender_ChannelPostersDoNotShareACommandLimit(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	sent := recordSends(tg)
	for i := 0; i < commandsPerMinute; i++ {
		b.handleUpdate(context.Background(), nil, asChannel(commandUpdate(groupChatID, 9100+i, channelBot, "/whoami"), channelB))
	}
	before := len(sent())
	b.handleUpdate(context.Background(), nil, asChannel(commandUpdate(groupChatID, 9200, channelBot, "/whoami"), channelA))
	require.Len(t, sent(), before+1)
	assert.NotEqual(t, commandsLimited, sent()[before].Text, "channel A's first command runs")
}

// Blind prefill r3 #3 inverted (B11): two channel posters' text and sticker
// do not fold into one turn, as two members' would not: channel A's waiting
// text runs first, on its own, and channel B's sticker turn reads only
// itself.
func TestSender_TwoChannelPostersDoNotFold(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.handleUpdate(context.Background(), nil, asChannel(textUpdate(groupChatID, 9300, channelBot, "A-TEXT: what's new?"), channelA))
	require.True(t, b.hasPendingIntake(chatKey(groupChatID)))

	b.handleUpdate(context.Background(), nil, asChannel(rv4Sticker(groupChatID, 9301, channelBot), channelB))
	waitIdle(t, b, groupChatID)

	require.Len(t, stub.requestBetas(), 2, "a turn each")
	role, text := stub.lastMessage(t, 0)
	assert.Equal(t, "user", role)
	assert.Equal(t, "A-TEXT: what's new?", text, "channel A's text is answered on its own")
	requireMediaAlone(t, stub, 1, "Sent a sticker: 😀")
}

// B11: the stored rows and the per-user rate limit go by the sender chat
// too: one channel over its limit does not silence another.
func TestSender_ChannelPostersHaveTheirOwnRateLimit(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	b.config.MessagePerHour, b.config.MessagePerDay = 1, 1
	b.config.SystemPrompts = map[string]string{"custom_instructions": "be helpful"}
	b.handleUpdate(context.Background(), nil, asChannel(textUpdate(groupChatID, 9400, channelBot, "B first"), channelB))
	b.handleUpdate(context.Background(), nil, asChannel(textUpdate(groupChatID, 9401, channelBot, "B over its limit"), channelB))
	b.handleUpdate(context.Background(), nil, asChannel(textUpdate(groupChatID, 9402, channelBot, "A first"), channelA))
	waitIdle(t, b, groupChatID)

	require.Len(t, stub.requestBetas(), 2, "B's first and A's first are answered")
	assert.True(t, requestHas(t, stub, 1, "A first"))
	assert.Equal(t, channelA.ID, storedRow(t, b, groupChatID, "A first").UserID, "the row names the channel")
	assert.Contains(t, string(stub.body(t, 1)), "chan_a", "and so does the model's user context")
}

// B11 with B4: a channel poster's edit applies to the channel's own message,
// and another channel's edit with the same message id does not, though both
// come from the one stand-in user.
func TestSender_AChannelsEditAppliesOnlyToItsOwnPost(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.handleUpdate(context.Background(), nil, asChannel(textUpdate(groupChatID, 9500, channelBot, "A original"), channelA))
	edit := func(ch *models.Chat, text string) *models.Update {
		return &models.Update{EditedMessage: &models.Message{ID: 9500, Chat: models.Chat{ID: groupChatID, Type: models.ChatTypeSupergroup},
			From: channelBot, SenderChat: ch, Text: text}}
	}

	b.handleUpdate(context.Background(), nil, edit(channelB, "B rewrote A's post"))
	assert.Equal(t, []string{"A original"}, memoryTexts(b, groupChatID), "another channel's edit is not applied")
	b.handleUpdate(context.Background(), nil, edit(channelA, "A corrected"))
	assert.Equal(t, []string{"A corrected"}, memoryTexts(b, groupChatID), "the channel's own edit is")
	assert.Equal(t, []string{"A corrected"}, liveTexts(t, b, groupChatID, channelA.ID))
}
