package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// One conversation per (Business connection, chat, forum topic): Telegram
// gives a Business chat the customer's own ID, so the customer's chats with
// two businesses and their private chat with the bot share a chat ID.

// businessText is a text message customer from sends in a Business chat of
// connection conn; the chat's ID is the customer's own.
func businessText(conn string, msgID int, from *models.User, text string) *models.Update {
	return &models.Update{BusinessMessage: &models.Message{
		ID: msgID, Chat: models.Chat{ID: from.ID, Type: models.ChatTypePrivate}, From: from,
		BusinessConnectionID: conn, Text: text,
	}}
}

// directText is a text message from sends in their private chat with the bot.
func directText(msgID int, from *models.User, text string) *models.Update {
	return &models.Update{Message: &models.Message{
		ID: msgID, Chat: models.Chat{ID: from.ID, Type: models.ChatTypePrivate}, From: from, Text: text,
	}}
}

var customer = &models.User{ID: 8001, Username: "customer"}

// chatKey is the conversation of a chat with no Business connection and no
// forum topic: a private chat with the bot, or a group.
func chatKey(chatID int64) convKey { return convKey{chatID: chatID} }

// Blind P4 inverted: business A1, business A2 and the direct chat each get
// their own memory and turn, and each request holds only its own
// conversation; each reply goes back through its own connection.
func TestConversation_BusinessChatsAndTheDirectChatAreApart(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)

	b.handleUpdate(context.Background(), nil, businessText("conn-A1", 40, customer, "BUSINESS-A1 order #4471 for alice"))
	b.handleUpdate(context.Background(), nil, businessText("conn-A2", 40, customer, "BUSINESS-A2 asking a second shop"))
	b.handleUpdate(context.Background(), nil, directText(41, customer, "DIRECT chat with the bot"))

	require.Len(t, stub.requestBetas(), 3, "one turn each")
	for i, own := range []string{"BUSINESS-A1", "BUSINESS-A2", "DIRECT"} {
		body := string(stub.body(t, i))
		assert.Contains(t, body, own)
		for _, other := range []string{"BUSINESS-A1", "BUSINESS-A2", "DIRECT"} {
			if other != own {
				assert.NotContains(t, body, other, "request %d reads only its own conversation", i)
			}
		}
	}
	sent := tg.sent()
	require.Len(t, sent, 3)
	for i, conn := range []string{"conn-A1", "conn-A2", ""} {
		assert.Equal(t, conn, sent[i].BusinessConnectionID, "reply %d goes back where its message came from", i)
		assert.Equal(t, customer.ID, sent[i].ChatID)
	}
}

// Blind P4b inverted: a Business message and the customer's direct message
// in one quiet window are two batches, two turns, each answered in its own
// chat, and each message is marked answered by its own reply.
func TestConversation_ABusinessAndADirectMessageInOneWindowAreTwoTurns(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	shopper := &models.User{ID: 8002, Username: "shopper"}

	b.handleUpdate(context.Background(), nil, businessText("conn-A1", 70, shopper, "BUSINESS question for the shop"))
	b.handleUpdate(context.Background(), nil, directText(71, shopper, "private hello"))
	flushAllPending(t, b)
	waitAllIdle(t, b)

	require.Len(t, stub.requestBetas(), 2, "a turn for each chat")
	conns := map[string]string{}
	for i := range stub.requestBetas() {
		body := string(stub.body(t, i))
		assert.False(t, strings.Contains(body, "BUSINESS question") && strings.Contains(body, "private hello"),
			"no request reads both chats")
	}
	for _, s := range tg.sent() {
		conns[s.BusinessConnectionID] = s.Text
	}
	assert.Len(t, conns, 2, "one reply through the connection, one in the direct chat")
	for _, text := range []string{"BUSINESS question for the shop", "private hello"} {
		var row Message
		require.NoError(t, b.db.Where("text = ?", text).First(&row).Error)
		assert.NotNil(t, row.AnsweredOn, "%q is answered", text)
	}
}

// Blind P4c inverted (B4): an edit applies only to the message with the same
// conversation, the same Telegram message id and the same sender. An edit in
// the direct chat never rewrites the Business chat's message with that id,
// nor does an edit from someone else.
func TestEdit_StaysInItsConversationAndWithItsSender(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	shopper := &models.User{ID: 8003, Username: "shopper"}
	b.handleUpdate(context.Background(), nil, businessText("conn-A1", 90, shopper, "ORIGINAL in business chat A1"))
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 91, alice, "ALICE original"))

	b.handleUpdate(context.Background(), nil, &models.Update{EditedMessage: &models.Message{
		ID: 90, Chat: models.Chat{ID: shopper.ID, Type: models.ChatTypePrivate}, From: shopper, Text: "EDIT from the private chat",
	}})
	b.handleUpdate(context.Background(), nil, &models.Update{EditedBusinessMessage: &models.Message{
		ID: 90, Chat: models.Chat{ID: shopper.ID, Type: models.ChatTypePrivate}, From: shopper,
		BusinessConnectionID: "conn-A2", Text: "EDIT through another connection",
	}})
	b.handleUpdate(context.Background(), nil, &models.Update{EditedMessage: &models.Message{
		ID: 91, Chat: models.Chat{ID: groupChatID, Type: models.ChatTypeSupergroup}, From: mallory, Text: "MALLORY rewrote it",
	}})

	row := func(tgID int, chatID int64) Message {
		var m Message
		require.NoError(t, b.db.Where("telegram_message_id = ? AND chat_id = ?", tgID, chatID).First(&m).Error)
		return m
	}
	assert.Equal(t, "ORIGINAL in business chat A1", row(90, shopper.ID).Text, "the Business chat's message is untouched")
	assert.Equal(t, "ALICE original", row(91, groupChatID).Text, "another sender's edit with the same id is not applied")
	assert.Contains(t, memoryTexts(b, groupChatID), "ALICE original")

	// The Business chat's own edit still applies.
	b.handleUpdate(context.Background(), nil, &models.Update{EditedBusinessMessage: &models.Message{
		ID: 90, Chat: models.Chat{ID: shopper.ID, Type: models.ChatTypePrivate}, From: shopper,
		BusinessConnectionID: "conn-A1", Text: "EDITED in business chat A1",
	}})
	assert.Equal(t, "EDITED in business chat A1", row(90, shopper.ID).Text)
}

// Each forum topic is a conversation of its own (user decision, B1): a turn
// in one topic reads nothing of another's, and answers in its own topic. A
// reply thread in a plain supergroup (message_thread_id without
// is_topic_message) stays in the one conversation.
func TestConversation_EachForumTopicIsItsOwn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	bob := &models.User{ID: 205, Username: "bob"}

	b.handleUpdate(context.Background(), nil, inTopic(textUpdate(forumChat, 10, alice, "TOPIC-ONE question"), 1))
	b.handleUpdate(context.Background(), nil, inTopic(textUpdate(forumChat, 11, bob, "TOPIC-TWO question"), 2))
	b.handleUpdate(context.Background(), nil, textUpdate(forumChat, 12, bob, "GENERAL question"))

	require.Len(t, stub.requestBetas(), 3)
	for i, own := range []string{"TOPIC-ONE", "TOPIC-TWO", "GENERAL"} {
		body := string(stub.body(t, i))
		assert.Contains(t, body, own)
		for _, other := range []string{"TOPIC-ONE", "TOPIC-TWO", "GENERAL"} {
			if other != own {
				assert.NotContains(t, body, other, "request %d reads only its own topic", i)
			}
		}
	}
	assert.Equal(t, []int{1, 2, 0}, threadsOf(tg.sent))

	// After a restart each topic loads only its own rows.
	for _, k := range []convKey{forumTopic(1), forumTopic(2), chatKey(forumChat)} {
		dropMemory(b, k)
	}
	b.handleUpdate(context.Background(), nil, inTopic(textUpdate(forumChat, 13, alice, "TOPIC-ONE again"), 1))
	require.Len(t, stub.requestBetas(), 4)
	assert.Contains(t, string(stub.body(t, 3)), "TOPIC-ONE question", "the topic's history reloads")
	assert.NotContains(t, string(stub.body(t, 3)), "TOPIC-TWO", "and only its own")
	assert.NotContains(t, string(stub.body(t, 3)), "GENERAL")

	// A reply thread in a plain supergroup is not a topic.
	reply := func(id int, text string) *models.Update {
		u := textUpdate(groupChatID, id, alice, text)
		u.Message.MessageThreadID = 500
		return u
	}
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 20, alice, "PLAIN first"))
	b.handleUpdate(context.Background(), nil, reply(21, "PLAIN reply in a thread"))
	require.Len(t, stub.requestBetas(), 6)
	assert.Contains(t, string(stub.body(t, 5)), "PLAIN first", "the reply thread reads the group's conversation")
}

// Fresh start (B2): rows stored before messages carried their connection
// (business_connection_id NULL) stay in the database, and in /stats, but
// never load into a conversation again; a new row stores "" for a chat with
// no connection, never NULL.
func TestConversation_RowsFromBeforeTheConnectionColumnNeverLoad(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	old := storeTextFrom(t, b, customer.ID, customer, "OLD message from before the change")
	require.NoError(t, b.db.Exec("UPDATE messages SET business_connection_id = NULL WHERE id = ?", old.ID).Error)
	_, before, err := b.getStats()
	require.NoError(t, err)
	dropChatMemory(b, customer.ID)

	b.handleUpdate(context.Background(), nil, directText(60, customer, "a new question"))

	require.Len(t, stub.requestBetas(), 1)
	assert.NotContains(t, string(stub.body(t, 0)), "OLD message", "the old row is not loaded into context")
	var kept int64
	require.NoError(t, b.db.Model(&Message{}).Where("id = ?", old.ID).Count(&kept).Error)
	assert.Equal(t, int64(1), kept, "the old row stays in the database")
	_, after, err := b.getStats()
	require.NoError(t, err)
	assert.Equal(t, before+2, after, "the stats still count the old row, with the new message and its reply")
	var nulls int64
	require.NoError(t, b.db.Model(&Message{}).Where("id <> ? AND business_connection_id IS NULL", old.ID).Count(&nulls).Error)
	assert.Zero(t, nulls, "new rows store an empty connection, not NULL")
}

// flushAllPending fires every pending quiet window now.
func flushAllPending(t *testing.T, b *Bot) {
	t.Helper()
	b.intakeBuffersMu.Lock()
	type batch struct {
		key convKey
		seq uint64
	}
	var batches []batch
	for key, p := range b.intakeBuffers {
		batches = append(batches, batch{key, p.seq})
	}
	b.intakeBuffersMu.Unlock()
	require.NotEmpty(t, batches, "no pending batch")
	for _, p := range batches {
		b.flushIntake(context.Background(), p.key, p.seq)
	}
}

// waitAllIdle waits until no turn runs in any conversation.
func waitAllIdle(t *testing.T, b *Bot) {
	t.Helper()
	idle := func() bool {
		b.turnsMu.Lock()
		defer b.turnsMu.Unlock()
		return len(b.turns) == 0
	}
	require.Eventually(t, idle, 5*time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond) // a rerun is started on its own goroutine
	require.Eventually(t, idle, 5*time.Second, 5*time.Millisecond)
}

// storeTextIn puts a message from from into conversation k, as text that
// joins the conversation at intake does.
func storeTextIn(t *testing.T, b *Bot, k convKey, from *models.User, text string) Message {
	t.Helper()
	b.getOrCreateChatMemory(k)
	m := b.createMessage(k, from.ID, from.Username, "user", text, true)
	require.NoError(t, b.storeMessage(&m))
	joined, ok := b.joinConversation(m)
	require.True(t, ok)
	return joined
}

// answerIn records reply as a turn's answer to everything in conversation
// k's memory, as a text turn would, without a model.
func answerIn(t *testing.T, b *Bot, k convKey, reply string) {
	t.Helper()
	req := turnRequest{ctx: context.Background(), chatID: k.chatID, businessConnectionID: k.businessConnectionID,
		threadID: k.threadID, forum: k.threadID != 0, userID: 555, username: "tester"}
	turn, _, ok := b.beginTextTurn(req, b.newestAnswerableSeqInChat(k))
	require.True(t, ok)
	defer b.finishTurn(turn)
	b.snapshotForTurn(turn, b.getOrCreateChatMemory(k))
	_, err := b.recordTurnReply(turn, k, reply)
	require.NoError(t, err)
}

// liveIn is the text of the rows of conversation k that are not deleted,
// oldest first.
func liveIn(t *testing.T, b *Bot, k convKey) []string {
	t.Helper()
	var rows []Message
	scope, args := b.convScope(k)
	require.NoError(t, b.db.Where(scope, args...).Order("id").Find(&rows).Error)
	texts := make([]string, 0, len(rows))
	for _, m := range rows {
		texts = append(texts, m.Text)
	}
	return texts
}

// memoryIn is the text of every message in conversation k's memory.
func memoryIn(b *Bot, k convKey) []string {
	mem := b.getOrCreateChatMemory(k)
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	texts := make([]string, 0, len(mem.Messages))
	for _, m := range mem.Messages {
		texts = append(texts, m.Text)
	}
	return texts
}

var (
	shopA1 = convKey{businessConnectionID: "conn-A1", chatID: customer.ID}
	shopA2 = convKey{businessConnectionID: "conn-A2", chatID: customer.ID}
	direct = chatKey(customer.ID)
	shop   = &models.User{ID: testShopID, Username: "shop"}
)

// threeConversations fills the customer's direct chat and their Business
// chats with A1 and A2, each with a message of theirs, A1 with the shop's
// own message too, and a reply in each.
func threeConversations(t *testing.T, b *Bot) {
	t.Helper()
	storeTextIn(t, b, direct, customer, "DIRECT my card is 4111")
	answerIn(t, b, direct, "DIRECT reply")
	storeTextIn(t, b, shopA1, customer, "A1 my address is 12 Elm St")
	storeTextIn(t, b, shopA1, shop, "A1 we ship tomorrow")
	answerIn(t, b, shopA1, "A1 reply")
	storeTextIn(t, b, shopA2, customer, "A2 do you have size 9?")
	answerIn(t, b, shopA2, "A2 reply")
}

// A /clear in the customer's direct chat with the bot clears that chat
// whole, and only it: their Business chats share its chat ID, not its rows.
func TestClear_ADirectChatClearLeavesTheBusinessChatsWithItsID(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	recordSends(tg)
	threeConversations(t, b)

	b.handleUpdate(context.Background(), nil, privateCommand(customer, 50, "/clear_hard"))

	assert.Equal(t, []string{clearedNotice}, liveIn(t, b, direct), "the direct chat is cleared whole")
	assert.Empty(t, memoryIn(b, direct))
	assert.Equal(t, []string{"A1 my address is 12 Elm St", "A1 we ship tomorrow", "A1 reply"}, liveIn(t, b, shopA1))
	assert.Equal(t, []string{"A2 do you have size 9?", "A2 reply"}, liveIn(t, b, shopA2))
	assert.Equal(t, []string{"A1 my address is 12 Elm St", "A1 we ship tomorrow", "A1 reply"}, memoryIn(b, shopA1))
}

// A Business customer's bare /clear takes only their own messages, and only
// in that business's chat: the shop's messages and the bot's replies stay,
// and so do their chats with the other business and with the bot.
func TestClear_ABusinessCustomersClearStaysInItsConnection(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	sent := recordSends(tg)
	threeConversations(t, b)

	u := businessCommand(customer.ID, 51, customer, "/clear")
	u.BusinessMessage.BusinessConnectionID = "conn-A1"
	b.handleUpdate(context.Background(), nil, u)

	require.Equal(t, []string{ownClearedNotice}, sentTexts(sent))
	assert.Equal(t, "conn-A1", sent()[0].BusinessConnectionID)
	assert.Equal(t, []string{"A1 we ship tomorrow", "A1 reply"}, memoryIn(b, shopA1))
	assert.NotContains(t, liveIn(t, b, shopA1), "A1 my address is 12 Elm St")
	assert.Equal(t, []string{"A2 do you have size 9?", "A2 reply"}, liveIn(t, b, shopA2))
	assert.Equal(t, []string{"A2 do you have size 9?", "A2 reply"}, memoryIn(b, shopA2))
	assert.Equal(t, []string{"DIRECT my card is 4111", "DIRECT reply"}, liveIn(t, b, direct))
}

// B3 addendum (blind prefill r4 #5): an admin's /clear <customer> and
// /clear <customer> <customer> clear the customer's direct chat with the bot
// whole, but in their Business chats only their own rows: the business
// owner's messages and the bot's Business replies stay.
func TestClear_AdminFormLeavesTheBusinessOwnersRows(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, form := range map[string]string{"all chats": "/clear_hard %d", "that chat": "/clear_hard %d %d"} {
		t.Run(name, func(t *testing.T) {
			b, tg := setupBotForTest(t, 123)
			recordSends(tg)
			admin := createUserWithRole(t, b, 203, "moderator", "admin")
			createUserWithRole(t, b, customer.ID, customer.Username, "user")
			threeConversations(t, b)
			command := fmt.Sprintf(form, customer.ID)
			if name == "that chat" {
				command = fmt.Sprintf(form, customer.ID, customer.ID)
			}

			b.handleUpdate(context.Background(), nil, privateCommand(admin, 9300, command))

			assert.Empty(t, liveIn(t, b, direct), "the direct chat goes whole, the bot's replies too")
			assert.Empty(t, memoryIn(b, direct))
			assert.Equal(t, []string{"A1 we ship tomorrow", "A1 reply"}, liveIn(t, b, shopA1))
			assert.Equal(t, []string{"A1 we ship tomorrow", "A1 reply"}, memoryIn(b, shopA1))
			assert.Equal(t, []string{"A2 reply"}, liveIn(t, b, shopA2))
			assert.Equal(t, []string{"A2 reply"}, memoryIn(b, shopA2))
		})
	}
}

// External review EXT-1: an album is buffered by its conversation and its
// media_group_id, which is unique only inside a chat: two albums with the
// same id in two conversations with one chat ID stay two albums.
func TestAlbum_TheSameGroupIDInTwoConversationsStaysApart(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	item := func(id int, conn string) *models.Message {
		return &models.Message{ID: id, Chat: models.Chat{ID: customer.ID, Type: models.ChatTypePrivate}, From: customer,
			BusinessConnectionID: conn, MediaGroupID: "g1", Photo: []models.PhotoSize{{FileID: "p", Width: 1, Height: 1}}}
	}
	b.bufferAlbumItem(context.Background(), item(1, "conn-A1"), targetFor(item(1, "conn-A1"), "conn-A1"), customer.ID, "", "", "", false, "", 0)
	b.bufferAlbumItem(context.Background(), item(2, ""), targetFor(item(2, ""), ""), customer.ID, "", "", "", false, "", 0)

	b.albumBuffersMu.Lock()
	defer b.albumBuffersMu.Unlock()
	require.Len(t, b.albumBuffers, 2, "two albums")
	for key, album := range b.albumBuffers {
		require.Len(t, album.items, 1, "album %v", key)
		assert.Equal(t, key.conv, album.to.conv())
	}
}

// preConnectionMessage is a messages row as it was stored before rows
// carried their connection and topic.
type preConnectionMessage struct {
	gorm.Model
	BotID     uint
	ChatID    int64
	UserID    int64
	Text      string
	IsUser    bool
	Timestamp time.Time
	Aside     bool
}

func (preConnectionMessage) TableName() string { return "messages" }

// B2: the migration adds the connection and topic columns, indexed with the
// bot and chat, and leaves NULL in the rows it finds; a conversation never
// loads them.
func TestMigration_RowsFromBeforeGetNULLAndStayUnloaded(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&preConnectionMessage{}))
	require.NoError(t, db.Create(&preConnectionMessage{BotID: 1, ChatID: 77, UserID: 77, Text: "from before", IsUser: true,
		Timestamp: time.Now()}).Error)

	require.NoError(t, db.AutoMigrate(&Message{}))

	var nulls int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM messages WHERE business_connection_id IS NULL AND thread_id IS NULL").Scan(&nulls).Error)
	assert.Equal(t, int64(1), nulls, "the old row reads NULL")
	var columns []string
	require.NoError(t, db.Raw("SELECT name FROM pragma_index_info('idx_messages_conversation')").Scan(&columns).Error)
	assert.Equal(t, []string{"bot_id", "business_connection_id", "chat_id"}, columns)
	b := &Bot{db: db, botID: 1, memorySize: 10}
	assert.Empty(t, b.loadChatHistory(chatKey(77)), "no conversation loads it")
}

// dropMemory forgets conversation k's memory, so the next use reloads it from
// the database, as after a restart.
func dropMemory(b *Bot, k convKey) {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	delete(b.chatMemories, k)
}
