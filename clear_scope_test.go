package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const ownClearedNotice = "Your messages in this chat were cleared."

const groupChatID = int64(-100500)

var (
	alice   = &models.User{ID: 201, Username: "alice"}
	mallory = &models.User{ID: 202, Username: "mallory"}
)

// storeTextFrom puts a message from from into the database and chat memory,
// as text that joins the conversation at intake does.
func storeTextFrom(t *testing.T, b *Bot, chatID int64, from *models.User, text string) Message {
	t.Helper()
	b.getOrCreateChatMemory(chatKey(chatID))
	m := b.createMessage(chatKey(chatID), from.ID, from.Username, "user", text, true)
	require.NoError(t, b.storeMessage(&m))
	joined, ok := b.joinConversation(m)
	require.True(t, ok)
	return joined
}

// liveTexts is the text of userID's rows in the chat that are not deleted,
// oldest first; userID 0 is the bot.
func liveTexts(t *testing.T, b *Bot, chatID, userID int64) []string {
	t.Helper()
	var rows []Message
	require.NoError(t, b.db.Where("chat_id = ? AND user_id = ?", chatID, userID).Order("id").Find(&rows).Error)
	texts := make([]string, 0, len(rows))
	for _, m := range rows {
		texts = append(texts, m.Text)
	}
	return texts
}

// createUserWithRole stores a user of this bot with the named role.
func createUserWithRole(t *testing.T, b *Bot, telegramID int64, username, roleName string) *models.User {
	t.Helper()
	role, err := b.getRoleByName(roleName)
	require.NoError(t, err)
	require.NoError(t, b.db.Create(&User{
		BotID: b.botID, TelegramID: telegramID, Username: username, RoleID: role.ID, Role: role,
	}).Error)
	return &models.User{ID: telegramID, Username: username}
}

// privateCommand is a command in a private chat with the bot.
func privateCommand(from *models.User, msgID int, text string) *models.Update {
	u := commandUpdate(from.ID, msgID, from, text)
	u.Message.Chat.Type = models.ChatTypePrivate
	return u
}

// businessCommand is a command a Business chat delivers.
func businessCommand(chatID int64, msgID int, from *models.User, text string) *models.Update {
	u := commandUpdate(chatID, msgID, from, text)
	u.Message.Chat.Type = models.ChatTypePrivate
	u.Message.BusinessConnectionID = "bc1"
	return &models.Update{BusinessMessage: u.Message}
}

// pendingBatch reports whether the chat has a batch in its quiet window.
func pendingBatch(b *Bot, chatID int64) bool {
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	for k := range b.intakeBuffers { // in any of its topics or connections
		if k.chatID == chatID {
			return true
		}
	}
	return false
}

// startCommittedTurn runs a text turn for the chat on its own goroutine and
// waits until its first segment is out. It returns the turn and a channel
// closed when the turn's call returns.
func startCommittedTurn(t *testing.T, b *Bot, chatID int64) (*chatTurn, chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); b.respondToChat(textReq(chatID)) }()
	var running *chatTurn
	require.Eventually(t, func() bool {
		b.turnsMu.Lock()
		defer b.turnsMu.Unlock()
		running = b.turns[chatKey(chatID)]
		return running != nil && running.committed
	}, 5*time.Second, 5*time.Millisecond)
	return running, done
}

// splitAnswer is a reply in two segments, the second held by a gated stub.
func splitAnswer() []string {
	return evStream(evTextBlock(0, "First part."), evTextBlock(1, "Second part."), evMessageEnd("end_turn"))
}

// Blind finding 2 inverted: Mallory's bare /clear in a group takes only her own
// messages. Alice's running answer goes on and is delivered, Alice's waiting
// batch still runs, and Alice's rows and the bot's replies stay.
func TestClear_GroupMemberClearsOnlyTheirOwnMessages(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, splitAnswer(), 4)
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	errs := captureErrors(t)
	ctx := context.Background()
	storeTextFrom(t, b, groupChatID, mallory, "my number is 555-0199")
	answerInTurn(t, b, groupChatID, "Noted.")
	storeTextFrom(t, b, groupChatID, alice, "where is the farm tab?")
	running, done := startCommittedTurn(t, b, groupChatID)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.handleUpdate(ctx, nil, textUpdate(groupChatID, 12, alice, "and the candle farm?"))
	require.True(t, pendingBatch(b, groupChatID))

	b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 13, mallory, "/clear"))

	assert.False(t, b.clearedTurn(running), "Alice's answer is not cancelled")
	assert.True(t, pendingBatch(b, groupChatID), "Alice's waiting batch stays")
	open()
	<-done
	flushPending(t, b, groupChatID)
	waitIdle(t, b, groupChatID)

	assert.Equal(t, []string{"First part.", ownClearedNotice, "Second part.", "First part.", "Second part."},
		sentTexts(sent), "both of Alice's answers are delivered")
	assert.Equal(t, []string{"where is the farm tab?", "and the candle farm?"}, liveTexts(t, b, groupChatID, alice.ID),
		"Alice's rows stay")
	assert.Empty(t, liveTexts(t, b, groupChatID, mallory.ID), "Mallory's own rows go, her /clear included")
	assert.Contains(t, liveTexts(t, b, groupChatID, 0), "Noted.", "the bot's replies stay")
	assert.NotContains(t, memoryTexts(b, groupChatID), "my number is 555-0199")
	assert.Contains(t, memoryTexts(b, groupChatID), "where is the farm tab?")
	require.Len(t, stub.requestBetas(), 2)
	assert.False(t, requestHas(t, stub, 1, "555-0199"), "the next request holds nothing of Mallory's")
	assert.Empty(t, errs.String())
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	assert.NotZero(t, b.chatMemories[chatKey(groupChatID)].userCutoffs[mallory.ID],
		"the clear's cutoff stays in force, kept with the chat's memory")
}

// storeAside stores a message from from that has not joined the
// conversation yet, as a voice note being transcribed is.
func storeAside(t *testing.T, b *Bot, chatID int64, from *models.User, text string) Message {
	t.Helper()
	b.getOrCreateChatMemory(chatKey(chatID))
	m := b.createMessage(chatKey(chatID), from.ID, from.Username, "user", text, true)
	m.Aside = true
	require.NoError(t, b.storeMessage(&m))
	return m
}

// A message of Mallory's stored before her /clear that tries to join while
// the clear's delete is still to run (its row still there) stays out: the
// clear's cutoff refuses it.
func TestClear_OwnMessageJoiningBeforeTheDeleteStaysOut(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	oneConnection(t, b)
	recordSends(tg)
	storeTextFrom(t, b, groupChatID, alice, "where is the farm tab?")
	late := storeAside(t, b, groupChatID, mallory, "my number is 555-0199")
	var once sync.Once
	joined := true
	// Before the delete's transaction takes the one connection, which the
	// join's own write needs.
	require.NoError(t, b.db.Callback().Delete().Before("gorm:begin_transaction").Register("test:late_join", func(*gorm.DB) {
		once.Do(func() { _, joined = b.joinConversation(late) })
	}))

	b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 13, mallory, "/clear"))

	assert.False(t, joined, "the late message does not join")
	assert.Equal(t, []string{"where is the farm tab?"}, memoryTexts(b, groupChatID))
	assert.Empty(t, liveTexts(t, b, groupChatID, mallory.ID))
}

// A sender's /clear landing between their message's insert into memory and
// its row's write takes it out again, and the join reports it did not join.
func TestJoinConversation_SendersClearDuringTheJoinEndsIt(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	oneConnection(t, b)
	storeTextFrom(t, b, groupChatID, alice, "where is the farm tab?")
	msg := storeAside(t, b, groupChatID, mallory, "my number is 555-0199")
	var once sync.Once
	// Before the write's transaction takes the one connection, which the
	// clear's cutoff read needs.
	require.NoError(t, b.db.Callback().Update().Before("gorm:begin_transaction").Register("test:own_clear", func(*gorm.DB) {
		once.Do(func() {
			k := chatKey(groupChatID)
			_, err := b.beginUserClear(userClear{userID: mallory.ID, conv: &k})
			assert.NoError(t, err)
		})
	}))

	_, ok := b.joinConversation(msg)

	assert.False(t, ok)
	assert.Equal(t, []string{"where is the farm tab?"}, memoryTexts(b, groupChatID))
}

// An admin's bare /clear in a group, with the :any scope, is the full clear:
// every row goes, the running turn ends silently, the waiting batch is dropped.
func TestClear_AnyScopeInAGroupClearsEveryone(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, splitAnswer(), 4)
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	ctx := context.Background()
	admin := createUserWithRole(t, b, 203, "moderator", "admin")
	storeTextFrom(t, b, groupChatID, mallory, "my number is 555-0199")
	answerInTurn(t, b, groupChatID, "Noted.")
	storeTextFrom(t, b, groupChatID, alice, "where is the farm tab?")
	running, done := startCommittedTurn(t, b, groupChatID)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.handleUpdate(ctx, nil, textUpdate(groupChatID, 12, alice, "and the candle farm?"))

	b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 14, admin, "/clear"))

	assert.True(t, b.clearedTurn(running), "the running answer is cancelled")
	assert.False(t, pendingBatch(b, groupChatID), "the waiting batch is dropped")
	open()
	<-done
	waitIdle(t, b, groupChatID)
	assert.Equal(t, []string{"First part.", clearedNotice}, sentTexts(sent))
	assert.Empty(t, liveTexts(t, b, groupChatID, alice.ID))
	assert.Empty(t, liveTexts(t, b, groupChatID, mallory.ID))
	assert.Equal(t, []string{clearedNotice}, liveTexts(t, b, groupChatID, 0), "only the confirmation is left")
	assert.Empty(t, memoryTexts(b, groupChatID))
}

// In a private chat with the bot, a user's bare /clear is the full clear: the
// bot's replies go with the user's messages.
func TestClear_PrivateChatIsAFullClear(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	sent := recordSends(tg)
	user := &models.User{ID: 301, Username: "carol"}
	storeTextFrom(t, b, user.ID, user, "my secret")
	answerInTurn(t, b, user.ID, "Noted.")

	b.handleUpdate(context.Background(), nil, privateCommand(user, 31, "/clear"))

	assert.Equal(t, []string{clearedNotice}, sentTexts(sent))
	assert.Empty(t, liveTexts(t, b, user.ID, user.ID))
	assert.Equal(t, []string{clearedNotice}, liveTexts(t, b, user.ID, 0), "the bot's replies go too")
	assert.Empty(t, memoryTexts(b, user.ID))
}

// A Business customer's /clear takes only the customer's own messages: the
// business owner's messages and the bot's replies stay.
func TestClear_BusinessCustomerClearsOnlyTheirOwnMessages(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	sent := recordSends(tg)
	customer := &models.User{ID: 401, Username: "customer"}
	shopOwner := &models.User{ID: 402, Username: "shop"}
	chat := convKey{businessConnectionID: "bc1", chatID: customer.ID}
	storeTextIn(t, b, chat, customer, "my address is 12 Elm St")
	storeTextIn(t, b, chat, shopOwner, "we ship tomorrow")
	answerIn(t, b, chat, "Noted.")

	b.handleUpdate(context.Background(), nil, businessCommand(customer.ID, 41, customer, "/clear"))

	require.Len(t, sent(), 1)
	assert.Equal(t, ownClearedNotice, sent()[0].Text)
	assert.Equal(t, "bc1", sent()[0].BusinessConnectionID)
	assert.Empty(t, liveTexts(t, b, customer.ID, customer.ID))
	assert.Equal(t, []string{"we ship tomorrow"}, liveTexts(t, b, customer.ID, shopOwner.ID))
	assert.Contains(t, liveTexts(t, b, customer.ID, 0), "Noted.")
	assert.Equal(t, []string{"we ship tomorrow", "Noted."}, memoryIn(b, chat))
}

// A sender's own /clear landing after their turn registered but before it read
// memory leaves the turn nothing to answer: it ends silently, with no model
// call, no "BUG" and no error reply, though it is not cancelled.
func TestClear_OwnMessagesGoneBeforeTheTurnReadsThemEndsItSilently(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	errs := captureErrors(t)
	storeTextFrom(t, b, groupChatID, alice, "where is the farm tab?")
	answerInTurn(t, b, groupChatID, "Open the Farm tab.")
	storeTextFrom(t, b, groupChatID, mallory, "my number is 555-0199")
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
	go func() { defer close(done); b.respondToChat(textReq(groupChatID)) }()
	<-entered // the turn is registered; its first typing action is in flight

	b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 13, mallory, "/clear"))
	close(gate)
	<-done
	waitIdle(t, b, groupChatID)

	assert.Empty(t, stub.requestBetas(), "no model call")
	assert.Equal(t, []string{ownClearedNotice}, sentTexts(sent), "nothing after the confirmation")
	assert.NotContains(t, errs.String(), "BUG")
	assert.Equal(t, []string{"where is the farm tab?", "Open the Farm tab."}, memoryTexts(b, groupChatID))
}

// Handled commands have their own limit per user: 5 at once, refilled at 5
// per minute (one per 12 seconds); 30 at once, refilled at 30 per hour. A
// refused command gets one notice a minute at most. Owners and admins are
// exempt.
func TestCommands_RateLimitedPerUser(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	const limited = "Too many commands, try again later."
	b, tg := setupBotForTest(t, 123)
	sent := recordSends(tg)
	clock, ok := b.clock.(*MockClock)
	require.True(t, ok)
	ctx := context.Background()
	user := &models.User{ID: 601, Username: "spammer"}
	msgID := 100
	send := func(from *models.User, n int) {
		for range n {
			msgID++
			b.handleUpdate(ctx, nil, privateCommand(from, msgID, "/whoami"))
		}
	}
	count := func() (ran, notices int) {
		for _, text := range sentTexts(sent) {
			if text == limited {
				notices++
			} else {
				ran++
			}
		}
		return ran, notices
	}

	send(user, 20) // 20 commands within the same second
	ran, notices := count()
	assert.Equal(t, 5, ran, "five run")
	assert.Equal(t, 1, notices, "one notice")
	var notice Message
	require.NoError(t, b.db.Where("chat_id = ? AND text = ?", user.ID, limited).First(&notice).Error)
	assert.True(t, notice.Aside, "the notice is aside")

	clock.Advance(12 * time.Second)
	send(user, 2)
	ran, notices = count()
	assert.Equal(t, 6, ran, "one more after 12 seconds")
	assert.Equal(t, 1, notices, "no second notice within the minute")

	clock.Advance(48 * time.Second)
	send(user, 5)
	ran, notices = count()
	assert.Equal(t, 10, ran, "four more a minute after the first")
	assert.Equal(t, 2, notices, "a notice again after a minute")

	// One every 12 seconds keeps within the minute's rate; the hour's 30
	// still run out.
	for range 30 {
		clock.Advance(12 * time.Second)
		send(user, 1)
	}
	ran, _ = count()
	assert.Less(t, ran, 10+30, "the hourly limit holds where the minute's would not")

	owner := &models.User{ID: 123, Username: "owner"}
	admin := createUserWithRole(t, b, 603, "moderator", "admin")
	before, _ := count()
	send(owner, 20)
	send(admin, 20)
	after, _ := count()
	assert.Equal(t, 40, after-before, "owners and admins are exempt")
}

// Blind r3 #5 inverted (P5a): a text stored before its sender's own /clear,
// whose handler joins it only after the clear finished, never enters memory:
// the clear's cutoff stays in force, so no context built meanwhile holds it.
func TestClear_OwnTextJoiningAfterTheClearNeverEntersMemory(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	oneConnection(t, b)
	recordSends(tg)
	late := storeAside(t, b, groupChatID, alice, "ALICE-SECRET") // intake stored it; its handler has not joined it yet
	storeTextFrom(t, b, groupChatID, mallory, "hello")
	b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 9700, alice, "/clear"))
	require.Empty(t, liveTexts(t, b, groupChatID, alice.ID))

	var seen []string
	require.NoError(t, b.db.Callback().Update().Before("gorm:begin_transaction").Register("test:peek", func(*gorm.DB) {
		seen = append(seen, memoryTexts(b, groupChatID)...)
	}))
	_, joined := b.joinConversation(late)

	assert.False(t, joined)
	assert.NotContains(t, seen, "ALICE-SECRET", "no context built during the join holds the deleted text")
	assert.Equal(t, []string{"hello"}, memoryTexts(b, groupChatID))
}

// Blind r3 #5 inverted (P5b): the same text, its join's write failing, stays
// out too.
func TestClear_OwnTextJoiningAfterTheClearStaysOutWhenItsWriteFails(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	recordSends(tg)
	late := storeAside(t, b, groupChatID, alice, "ALICE-SECRET")
	storeTextFrom(t, b, groupChatID, mallory, "hello")
	b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 9800, alice, "/clear"))
	require.NoError(t, b.db.Callback().Update().Before("gorm:begin_transaction").Register("test:busy", func(db *gorm.DB) {
		_ = db.AddError(errors.New("database is locked"))
	}))

	_, joined := b.joinConversation(late)

	assert.False(t, joined)
	assert.Equal(t, []string{"hello"}, memoryTexts(b, groupChatID))
}

// A join whose row write fails is not a join: the message leaves memory
// again, and its row stays aside.
func TestJoinConversation_AFailedWriteIsNotAJoin(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	msg := storeAside(t, b, groupChatID, alice, "a message")
	require.NoError(t, b.db.Callback().Update().Before("gorm:begin_transaction").Register("test:busy", func(db *gorm.DB) {
		_ = db.AddError(errors.New("database is locked"))
	}))

	_, joined := b.joinConversation(msg)

	assert.False(t, joined)
	assert.Empty(t, memoryTexts(b, groupChatID))
	assert.True(t, storedRow(t, b, groupChatID, "a message").Aside, "its row stays aside")
}

// pinStream is a reply in two segments, the second quoting what the user
// asked the bot to keep.
func pinStream() []string {
	return evStream(evTextBlock(0, "Noted,"), evTextBlock(1, "your PIN is 9876."), evMessageEnd("end_turn"))
}

// Scoped (b) inverted: Mallory's bare /clear lands while her own turn, which
// answers only her message, is streaming: the turn is cancelled silently, its
// reply is neither finished, nor stored, nor in the next request.
func TestClear_OwnClearStopsTheTurnAnsweringOnlyThem(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, pinStream(), 4)
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	storeTextFrom(t, b, groupChatID, alice, "hi all")
	answerInTurn(t, b, groupChatID, "Hello!")
	storeTextFrom(t, b, groupChatID, mallory, "my PIN is 9876, remember it")
	running, done := startCommittedTurn(t, b, groupChatID)

	b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 20, mallory, "/clear"))
	assert.True(t, b.clearedTurn(running), "her turn is cancelled")
	open()
	<-done
	waitIdle(t, b, groupChatID)

	assert.Equal(t, []string{"Noted,", ownClearedNotice}, sentTexts(sent), "nothing after the confirmation")
	assert.NotContains(t, liveTexts(t, b, groupChatID, 0), "Noted,\n\nyour PIN is 9876.", "nothing stored")
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 21, alice, "hello"))
	waitIdle(t, b, groupChatID)
	require.Len(t, stub.requestBetas(), 2)
	assert.False(t, requestHas(t, stub, 1, "9876"), "the next request holds nothing of it")
}

// A committed turn that answers Mallory and Alice goes on when Mallory clears
// her messages: Alice gets her answer, but the reply, which read Mallory's
// message, is stored aside: never in memory or a later request.
func TestClear_OwnClearKeepsAMixedCommittedReplyAside(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, pinStream(), 4)
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	storeTextFrom(t, b, groupChatID, mallory, "my PIN is 9876, remember it")
	storeTextFrom(t, b, groupChatID, alice, "and what is mine?")
	running, done := startCommittedTurn(t, b, groupChatID)

	b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 20, mallory, "/clear"))
	assert.False(t, b.clearedTurn(running), "the turn also answers Alice: it goes on")
	open()
	<-done
	waitIdle(t, b, groupChatID)

	assert.Equal(t, []string{"Noted,", ownClearedNotice, "your PIN is 9876."}, sentTexts(sent), "Alice gets her answer")
	reply := storedRow(t, b, groupChatID, "Noted,\n\nyour PIN is 9876.")
	assert.True(t, reply.Aside, "the reply is stored aside")
	assert.Equal(t, []string{"and what is mine?"}, memoryTexts(b, groupChatID), "and is not in memory")
	assert.NotNil(t, storedRow(t, b, groupChatID, "and what is mine?").AnsweredOn, "Alice's message is answered")
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 21, alice, "thanks"))
	waitIdle(t, b, groupChatID)
	require.Len(t, stub.requestBetas(), 2)
	assert.False(t, requestHas(t, stub, 1, "9876"), "the next request holds nothing of it")
}

// A mixed turn that read Mallory's message and has shown nothing yet restarts
// without it.
func TestClear_OwnClearRestartsAMixedTurnThatShowedNothing(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, pinStream(), 1) // message_start only: nothing shown
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	storeTextFrom(t, b, groupChatID, mallory, "my PIN is 9876, remember it")
	storeTextFrom(t, b, groupChatID, alice, "and what is mine?")
	done := make(chan struct{})
	go func() { defer close(done); b.respondToChat(textReq(groupChatID)) }()
	require.Eventually(t, func() bool { return len(stub.requestBetas()) == 1 }, 5*time.Second, time.Millisecond,
		"the turn read both messages and asked the model")

	b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 20, mallory, "/clear"))
	open()
	<-done
	waitIdle(t, b, groupChatID)

	require.Len(t, stub.requestBetas(), 2, "the turn restarted")
	assert.True(t, requestHas(t, stub, 0, "9876"), "the first request had read it")
	assert.False(t, requestHas(t, stub, 1, "9876"), "the rerun does not")
	_, last := stub.lastMessage(t, 1)
	assert.Equal(t, "and what is mine?", last)
	assert.Equal(t, []string{ownClearedNotice, "Noted,", "your PIN is 9876."}, sentTexts(sent))
}

// A waiting batch of only Mallory's messages goes with her clear; a mixed one
// stays, and its turn reads no message of hers.
func TestClear_OwnClearDropsABatchOfOnlyTheirMessages(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, tc := range map[string]struct {
		senders []*models.User
		kept    bool
	}{
		"only hers": {senders: []*models.User{mallory, mallory}, kept: false},
		"mixed":     {senders: []*models.User{mallory, alice}, kept: true},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newAnthropicStub(t, candleFarmStream())
			b, tg := setupBotForTest(t, 123)
			useAnthropicStub(b, stub)
			oneConnection(t, b)
			recordSends(tg)
			b.config.DebounceMs = int(bufferOnly / time.Millisecond)
			for i, from := range tc.senders {
				b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 30+i, from, fmt.Sprintf("%s says %d", from.Username, i)))
			}
			require.True(t, pendingBatch(b, groupChatID))

			b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 40, mallory, "/clear"))

			assert.Equal(t, tc.kept, pendingBatch(b, groupChatID))
			if tc.kept {
				flushPending(t, b, groupChatID)
				waitIdle(t, b, groupChatID)
				require.Len(t, stub.requestBetas(), 1)
				assert.False(t, requestHas(t, stub, 0, "mallory says"))
			}
		})
	}
}

// Mallory's flush collected behind her own turn goes with her clear: the
// cancelled turn hands the chat to nothing.
func TestClear_OwnClearDropsTheirCollectedFlush(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, pinStream(), 4)
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	recordSends(tg)
	storeTextFrom(t, b, groupChatID, mallory, "my PIN is 9876, remember it")
	running, done := startCommittedTurn(t, b, groupChatID)
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 19, mallory, "and my locker is 42")) // collected
	b.turnsMu.Lock()
	require.NotNil(t, running.rerun)
	b.turnsMu.Unlock()

	b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 20, mallory, "/clear"))
	open()
	<-done
	waitIdle(t, b, groupChatID)

	assert.Len(t, stub.requestBetas(), 1, "no rerun")
	assert.Empty(t, memoryTexts(b, groupChatID))
}
