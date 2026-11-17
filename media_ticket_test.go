package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
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

const emojiRule = "EMOJI_ONLY_RULE"

var asker = &models.User{ID: 4141, Username: "asker"}

// mediaTestBot is a bot with a stubbed model, a 10 s quiet window and the
// emoji rule as a marker in the system prompt.
func mediaTestBot(t *testing.T, stub *anthropicStub) (*Bot, *MockTelegramClient, func() []bot.SendMessageParams) {
	t.Helper()
	b, tg := setupBotForTest(t, ownerChat)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.config.SystemPrompts = map[string]string{"custom_instructions": "be helpful", "respond_with_emojis": emojiRule}
	return b, tg, sent
}

// holdDownloads makes Telegram file downloads wait until the returned release
// is called, and returns a channel told of each download that starts.
func holdDownloads(t *testing.T, tg *MockTelegramClient) (started chan struct{}, release func()) {
	t.Helper()
	gate := make(chan struct{})
	serveTelegramFiles(t, tg, gate)
	started = make(chan struct{}, 8)
	inner := tg.GetFileFunc
	tg.GetFileFunc = func(ctx context.Context, p *bot.GetFileParams) (*models.File, error) {
		started <- struct{}{}
		return inner(ctx, p)
	}
	released := false
	release = func() {
		if !released {
			released = true
			close(gate)
		}
	}
	t.Cleanup(release)
	return started, release
}

// requestWith is the index of the one request whose body holds s.
func requestWith(t *testing.T, stub *anthropicStub, s string) int {
	t.Helper()
	found := -1
	for i := range stub.requestBetas() {
		if requestHas(t, stub, i, s) {
			require.Equal(t, -1, found, "only one request holds %q", s)
			found = i
		}
	}
	require.NotEqual(t, -1, found, "a request holds %q", s)
	return found
}

// requireOwnTurn checks request i is the asker's question's own: with the
// asker's context and no emoji rule, ending with the question.
func requireOwnTurn(t *testing.T, stub *anthropicStub, i int, question string) {
	t.Helper()
	role, text := stub.lastMessage(t, i)
	assert.Equal(t, "user", role)
	assert.Equal(t, question, text, "the question's request ends with it")
	assert.Contains(t, string(stub.body(t, i)), "@asker", "with the asker's user context")
	assert.NotContains(t, string(stub.body(t, i)), emojiRule, "and no emoji rule")
}

// requireMediaAlone checks request i ends with the media message and holds
// no unanswered text it did not bring.
func requireMediaAlone(t *testing.T, stub *anthropicStub, i int, last string) {
	t.Helper()
	msgs := requestMessages(t, stub.body(t, i))
	require.GreaterOrEqual(t, len(msgs), 2)
	assert.Equal(t, "user", msgs[len(msgs)-1].Role)
	assert.Equal(t, last, msgs[len(msgs)-1].Text, "the media request ends with the media")
	assert.Equal(t, "assistant", msgs[len(msgs)-2].Role, "and holds no unanswered text it did not bring")
}

// threadsOf is the forum thread of every message sent.
func threadsOf(sent func() []bot.SendMessageParams) []int {
	var threads []int
	for _, s := range sent() {
		threads = append(threads, s.MessageThreadID)
	}
	return threads
}

// Blind B2 inverted: the asker's text arrives while the owner's voice note in
// the same topic is being transcribed. The note is transcribed before it asks
// for the conversation, so the text runs first, as its own turn; the voice
// turn then answers only the note. (A text in another topic is another
// conversation: TestConversation_EachForumTopicIsItsOwn.)
func TestVoice_TextArrivingDuringTranscriptionGetsItsOwnTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg, sent := mediaTestBot(t, stub)
	(&voiceStubs{text: transcript}).install(t, b, tg)
	started, release := holdDownloads(t, tg)
	const question = "what time does the store open?"

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleUpdate(context.Background(), nil, inTopic(&models.Update{Message: &models.Message{
			ID: 10, From: owner, Voice: &models.Voice{FileID: "v"}}}, 1))
	}()
	<-started // the note is being fetched for transcription
	b.handleUpdate(context.Background(), nil, inTopic(textUpdate(forumChat, 11, asker, question), 1))
	release()
	<-done
	waitIdle(t, b, forumChat)

	require.Len(t, stub.requestBetas(), 2)
	requireOwnTurn(t, stub, 0, question) // the text goes first
	requireMediaAlone(t, stub, 1, transcript)
	assert.Equal(t, []int{1, 1}, threadsOf(sent), "each answer goes to its own topic")
	assert.False(t, pendingBatch(b, forumChat))
	assert.NotNil(t, storedRow(t, b, forumChat, question).AnsweredOn)
}

// The same for a photo: text arriving during its upload gets its own turn.
func TestPhoto_TextArrivingDuringUploadGetsItsOwnTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg, sent := mediaTestBot(t, stub)
	started, release := holdDownloads(t, tg)
	const question = "what time does the store open?"

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleUpdate(context.Background(), nil, inTopic(&models.Update{Message: &models.Message{
			ID: 20, From: owner, Photo: []models.PhotoSize{{FileID: "p", Width: 1, Height: 1}}}}, 1))
	}()
	<-started // the photo is being fetched for upload
	b.handleUpdate(context.Background(), nil, inTopic(textUpdate(forumChat, 21, asker, question), 1))
	release()
	<-done
	waitIdle(t, b, forumChat)

	require.Len(t, stub.requestBetas(), 2)
	requireOwnTurn(t, stub, 0, question) // the text goes first
	requireMediaAlone(t, stub, 1, "")
	assert.Contains(t, string(stub.body(t, 1)), "file_stub")
	assert.Equal(t, []int{1, 1}, threadsOf(sent))
	assert.False(t, pendingBatch(b, forumChat))
}

// Scoped N1 inverted: the asker's question arrives while another member's
// sticker in the same topic waits behind a running turn. The sticker turn
// does not read it: the question gets its own turn and context.
func TestSticker_TextArrivingWhileItWaitsGetsItsOwnTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _, sent := mediaTestBot(t, stub)
	stickerer := &models.User{ID: 4242, Username: "stickerer"}
	const question = "what time does the store open?"
	running, _ := b.beginTurnIn(context.Background(), forumTopic(1)) // someone else's turn

	stickerDone := make(chan struct{})
	go func() {
		defer close(stickerDone)
		b.handleUpdate(context.Background(), nil, inTopic(&models.Update{Message: &models.Message{
			ID: 1001, From: stickerer, Sticker: &models.Sticker{FileID: "s", Emoji: "😀"}}}, 1))
	}()
	waitForMediaWaiter(t, b, forumChat)
	b.handleUpdate(context.Background(), nil, inTopic(textUpdate(forumChat, 1002, asker, question), 1))

	b.endTurn(running)
	<-stickerDone
	if pendingBatch(b, forumChat) {
		flushPending(t, b, forumChat) // the question's quiet window ends
	}
	waitIdle(t, b, forumChat)

	require.Len(t, stub.requestBetas(), 2)
	requireOwnTurn(t, stub, requestWith(t, stub, question), question)
	sticker := requestWith(t, stub, emojiRule)
	assert.False(t, requestHas(t, stub, sticker, question), "the sticker turn does not read the question")
	assert.Equal(t, []int{1, 1}, threadsOf(sent))
	assert.NotNil(t, storedRow(t, b, forumChat, question).AnsweredOn)
}

// Controller decision (F2): in a private chat, "look at this" and then a
// photo from the same person within the quiet window are one turn: one
// request that has read both, one reply.
func TestPhoto_SameSendersWaitingTextFoldsIntoItsTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, wrap := range map[string]func(*models.Update) *models.Update{
		"private":  func(u *models.Update) *models.Update { return u },
		"business": asBusiness,
	} {
		t.Run(name, func(t *testing.T) {
			stub := newAnthropicStub(t, candleFarmStream())
			b, tg, sent := mediaTestBot(t, stub)
			serveTelegramFiles(t, tg, nil)

			b.handleUpdate(context.Background(), nil, wrap(textUpdate(ownerChat, 30, owner, "look at this")))
			b.handleUpdate(context.Background(), nil, wrap(photoUpdate(31)))
			waitIdle(t, b, ownerChat)

			require.Len(t, stub.requestBetas(), 1, "one turn")
			assert.True(t, requestHas(t, stub, 0, "look at this"))
			assert.True(t, requestHas(t, stub, 0, "file_stub"))
			assert.False(t, b.hasPendingIntake(chatKey(ownerChat)), "the batch was taken over")
			assert.Len(t, sent(), 1, "one reply")
			assert.NotNil(t, storedRow(t, b, ownerChat, "look at this").AnsweredOn)
		})
	}
}

// asBusiness delivers u's message as a Business message.
func asBusiness(u *models.Update) *models.Update {
	u.Message.Chat.Type = models.ChatTypePrivate
	u.Message.BusinessConnectionID = "bc1"
	return &models.Update{BusinessMessage: u.Message}
}

// A waiting batch that holds another sender's text is not folded: it runs as
// its own turn first. (The same sender's text in another topic is another
// conversation: TestPhoto_InAnotherTopicLeavesThatTopicsBatchWaiting.)
func TestPhoto_OtherSendersOrTopicsSplit(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	alicePhoto := func(thread int) *models.Update {
		return inTopic(&models.Update{Message: &models.Message{
			ID: 42, From: alice, Photo: []models.PhotoSize{{FileID: "p", Width: 1, Height: 1}}}}, thread)
	}
	for name, tc := range map[string]struct {
		texts       []*models.Update
		photoThread int
	}{
		"two senders": {
			texts: []*models.Update{
				inTopic(textUpdate(forumChat, 40, alice, "look at this"), 1),
				inTopic(textUpdate(forumChat, 41, mallory, "nice one"), 1),
			},
			photoThread: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newAnthropicStub(t, candleFarmStream())
			b, tg, sent := mediaTestBot(t, stub)
			serveTelegramFiles(t, tg, nil)

			for _, u := range tc.texts {
				b.handleUpdate(context.Background(), nil, u)
			}
			b.handleUpdate(context.Background(), nil, alicePhoto(tc.photoThread))
			waitIdle(t, b, forumChat)

			require.Len(t, stub.requestBetas(), 2, "split")
			role, text := stub.lastMessage(t, 0)
			assert.Equal(t, "user", role)
			assert.Equal(t, sentFrom(tc.texts[len(tc.texts)-1]), text, "the batch goes first, alone")
			assert.False(t, requestHas(t, stub, 0, "file_stub"))
			requireMediaAlone(t, stub, 1, "")
			assert.Equal(t, []int{1, tc.photoThread}, threadsOf(sent))
		})
	}
}

// Scoped r5 #10: a photo's message carries its topic, as a text's does: it is
// in its topic's conversation (B1).
func TestPhoto_ItsMessageCarriesItsTopic(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg, _ := mediaTestBot(t, stub)
	serveTelegramFiles(t, tg, nil)
	b.handleUpdate(context.Background(), nil, inTopic(&models.Update{Message: &models.Message{
		ID: 43, From: alice, Photo: []models.PhotoSize{{FileID: "p", Width: 1, Height: 1}}}}, 2))
	waitIdle(t, b, forumChat)

	chatMemory := b.getOrCreateChatMemory(forumTopic(2))
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	at := slices.IndexFunc(chatMemory.Messages, func(m Message) bool { return len(m.ImageFileIDs) > 0 })
	require.GreaterOrEqual(t, at, 0, "the photo joined")
	assert.Equal(t, 2, chatMemory.Messages[at].ThreadID)
}

// A photo in one topic and its sender's waiting text in another are two
// conversations (B1): the photo is answered alone, in its topic, and the text
// still waits in its own quiet window, to be answered in its topic.
func TestPhoto_InAnotherTopicLeavesThatTopicsBatchWaiting(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg, sent := mediaTestBot(t, stub)
	serveTelegramFiles(t, tg, nil)

	b.handleUpdate(context.Background(), nil, inTopic(textUpdate(forumChat, 40, alice, "look at this"), 1))
	b.handleUpdate(context.Background(), nil, inTopic(&models.Update{Message: &models.Message{
		ID: 42, From: alice, Photo: []models.PhotoSize{{FileID: "p", Width: 1, Height: 1}}}}, 2))
	waitIdle(t, b, forumChat)

	require.Len(t, stub.requestBetas(), 1, "the photo is answered at once")
	msgs := requestMessages(t, stub.body(t, 0))
	require.Len(t, msgs, 1, "alone in its conversation")
	assert.Equal(t, "user", msgs[0].Role)
	assert.True(t, requestHas(t, stub, 0, "file_stub"))
	assert.False(t, requestHas(t, stub, 0, "look at this"), "it reads nothing of the other topic")
	assert.True(t, b.hasPendingIntake(forumTopic(1)), "the text waits in its own quiet window")
	flushPending(t, b, forumChat)
	waitIdle(t, b, forumChat)
	require.Len(t, stub.requestBetas(), 2)
	role, text := stub.lastMessage(t, 1)
	assert.Equal(t, "user", role)
	assert.Equal(t, "look at this", text)
	assert.False(t, requestHas(t, stub, 1, "file_stub"))
	assert.Equal(t, []int{2, 1}, threadsOf(sent))
}

// sentFrom is the text of u's message.
func sentFrom(u *models.Update) string { return u.Message.Text }

// A sticker turn that took over text keeps the emoji rule only if every
// message it took over was emoji only.
func TestSticker_FoldedTextSetsTheEmojiRule(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for text, wantRule := range map[string]bool{"what is this?": false, "😂": true} {
		t.Run(text, func(t *testing.T) {
			stub := newAnthropicStub(t, candleFarmStream())
			b, _, _ := mediaTestBot(t, stub)

			b.handleUpdate(context.Background(), nil, textUpdate(ownerChat, 50, owner, text))
			b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
				ID: 51, Chat: models.Chat{ID: ownerChat}, From: owner, Sticker: &models.Sticker{FileID: "s", Emoji: "😀"}}})
			waitIdle(t, b, ownerChat)

			require.Len(t, stub.requestBetas(), 1, "one turn")
			assert.True(t, requestHas(t, stub, 0, jsonEscaped(t, text)))
			assert.Equal(t, wantRule, requestHas(t, stub, 0, emojiRule))
		})
	}
}

// jsonEscaped is s as it appears inside a JSON string.
func jsonEscaped(t *testing.T, s string) string {
	t.Helper()
	q := jsonString(s)
	return q[1 : len(q)-1]
}

// F6: steady text from other members while a voice note waits behind a
// running turn does not starve it: when that turn ends, the voice turn goes
// next, and the text collected meanwhile runs after it.
func TestVoice_SteadyTextDoesNotStarveIt(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, splitAnswer(), 4)
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	recordSends(tg)
	(&voiceStubs{text: transcript}).install(t, b, tg)
	storeTextFrom(t, b, groupChatID, alice, "tell me about the farm")
	_, done := startCommittedTurn(t, b, groupChatID)

	voiceDone := make(chan struct{})
	go func() {
		defer close(voiceDone)
		b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
			ID: 60, Chat: models.Chat{ID: groupChatID, Type: models.ChatTypeSupergroup}, From: owner,
			Voice: &models.Voice{FileID: "v"}}})
	}()
	waitForMediaWaiter(t, b, groupChatID)
	for i, text := range []string{"and the bakery?", "and the mill?", "and the dock?"} {
		b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 61+i, mallory, text)) // no quiet window: each flushes at once
	}
	open()
	<-done
	<-voiceDone
	waitIdle(t, b, groupChatID)

	require.Len(t, stub.requestBetas(), 3)
	_, last := stub.lastMessage(t, 1)
	assert.Equal(t, transcript, last, "the voice turn runs right after the running turn")
	_, last = stub.lastMessage(t, 2)
	assert.Equal(t, "and the dock?", last, "the text collected meanwhile runs after it")
	assert.False(t, requestHas(t, stub, 1, "and the bakery?"), "the voice turn does not read it")
}

// waitForMediaWaiter waits until a media turn waits for the chat.
func waitForMediaWaiter(t *testing.T, b *Bot, chatID int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		b.turnsMu.Lock()
		defer b.turnsMu.Unlock()
		for k, waiters := range b.mediaWaiters {
			if k.chatID == chatID && len(waiters) > 0 {
				return true
			}
		}
		return false
	}, 5*time.Second, time.Millisecond)
}

// The residual case: a text that joined before a waiting sticker's ticket
// reaches its quiet window only while the sticker waits. When the running
// turn ends, the chat is not handed to the sticker: the text runs first, as
// its own turn, and the sticker turn does not read it.
func TestCloseTurn_LateBatchFromBeforeTheTicketGoesFirst(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _, _ := mediaTestBot(t, stub)
	const question = "what time does the store open?"
	early := storeTextFrom(t, b, groupChatID, asker, question) // joined, not yet buffered
	running, _ := b.beginTurn(context.Background(), groupChatID)

	stickerDone := make(chan struct{})
	go func() {
		defer close(stickerDone)
		b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
			ID: 70, Chat: models.Chat{ID: groupChatID, Type: models.ChatTypeSupergroup}, From: owner,
			Sticker: &models.Sticker{FileID: "s", Emoji: "😀"}}})
	}()
	waitForMediaWaiter(t, b, groupChatID)
	b.bufferIntake(turnRequest{ctx: context.Background(), chatID: groupChatID, userID: asker.ID, username: asker.Username,
		joinedSeq: early.Seq})

	b.endTurn(running)
	<-stickerDone
	waitIdle(t, b, groupChatID)

	require.Len(t, stub.requestBetas(), 2)
	requireOwnTurn(t, stub, 0, question)
	requireMediaAlone(t, stub, 1, "Sent a sticker: 😀")
}

// Text the sticker's own sender sends while the sticker waits is taken over
// too: one turn whose request reads both, the text before the sticker.
func TestSticker_SendersTextWhileItWaitsFoldsIntoItsTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _, sent := mediaTestBot(t, stub)
	running, _ := b.beginTurn(context.Background(), ownerChat)

	stickerDone := make(chan struct{})
	go func() {
		defer close(stickerDone)
		b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
			ID: 80, Chat: models.Chat{ID: ownerChat}, From: owner, Sticker: &models.Sticker{FileID: "s", Emoji: "😀"}}})
	}()
	waitForMediaWaiter(t, b, ownerChat)
	b.handleUpdate(context.Background(), nil, textUpdate(ownerChat, 81, owner, "lol"))

	b.endTurn(running)
	<-stickerDone
	waitIdle(t, b, ownerChat)

	require.Len(t, stub.requestBetas(), 1, "one turn")
	msgs := requestMessages(t, stub.body(t, 0))
	require.GreaterOrEqual(t, len(msgs), 2)
	assert.Equal(t, "lol", msgs[len(msgs)-2].Text)
	assert.Equal(t, "Sent a sticker: 😀", msgs[len(msgs)-1].Text)
	assert.False(t, b.hasPendingIntake(chatKey(ownerChat)))
	assert.Len(t, sent(), 1)
}

// Text from before a waiting sticker's ticket goes first, and its turn also
// answers text sent after the ticket. The sticker then joins after all of it,
// with its ticket below what was answered meanwhile: its turn reads that
// reply, and its own row is marked answered.
func TestSticker_AnsweredAfterTextAnsweredWhileItWaited(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, splitAnswer(), 4)
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	recordSends(tg)
	storeTextFrom(t, b, groupChatID, alice, "tell me about the farm")
	_, done := startCommittedTurn(t, b, groupChatID)
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 90, mallory, "and the bakery?")) // collected: before the ticket

	stickerDone := make(chan struct{})
	go func() {
		defer close(stickerDone)
		b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
			ID: 91, Chat: models.Chat{ID: groupChatID, Type: models.ChatTypeSupergroup}, From: owner,
			Sticker: &models.Sticker{FileID: "s", Emoji: "😀"}}})
	}()
	waitForMediaWaiter(t, b, groupChatID)
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 92, mallory, "and the mill?")) // after it
	open()
	<-done
	<-stickerDone
	waitIdle(t, b, groupChatID)

	require.Len(t, stub.requestBetas(), 3)
	_, last := stub.lastMessage(t, 1)
	assert.Equal(t, "and the mill?", last, "the text from before the ticket goes first, with what came after")
	requireMediaAlone(t, stub, 2, "Sent a sticker: 😀")
	var sticker Message
	require.NoError(t, b.db.Where("chat_id = ? AND sticker_file_id = ?", groupChatID, "s").First(&sticker).Error)
	assert.NotNil(t, sticker.AnsweredOn, "the sticker is marked answered")
}

// Blind r3 #1 inverted (P4): a sticker waiting for the chat while a full
// window of later text joins memory is placed before that text, and the
// window's trim does not drop it: its turn reads it and answers it.
func TestSticker_AFullWindowOfLaterTextDoesNotDropIt(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	recordSends(tg)
	b.config.MessagePerHour, b.config.MessagePerDay = 20, 100 // the shipped default burst
	b.config.DebounceMs = 60000                               // the flood waits in its quiet window
	victim := &models.User{ID: 4343, Username: "victim"}
	running, _ := b.beginTurn(context.Background(), groupChatID) // another turn holds the chat

	stickerDone := make(chan struct{})
	go func() {
		defer close(stickerDone)
		b.handleUpdate(context.Background(), nil, rv4Sticker(groupChatID, 9500, victim))
	}()
	waitForMediaWaiter(t, b, groupChatID)
	for i := 0; i < 2*b.memorySize; i++ { // one member's hourly burst
		b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 9600+i, mallory, fmt.Sprintf("flood %d", i)))
	}
	b.endTurn(running)
	<-stickerDone
	waitIdle(t, b, groupChatID)

	require.Len(t, stub.requestBetas(), 1, "the sticker turn asked the model")
	_, last := stub.lastMessage(t, 0)
	assert.Equal(t, "Sent a sticker: 😀", last)
	assert.False(t, requestHas(t, stub, 0, "flood"), "and read none of the later text")
	row := storedRow(t, b, groupChatID, "Sent a sticker: 😀")
	assert.False(t, row.Aside)
	assert.NotNil(t, row.AnsweredOn, "it is answered")
}

// A full window of text joining while the sticker's own join writes its row,
// before its turn reads memory, does not trim the sticker out either.
func TestSticker_AFloodDuringItsJoinDoesNotDropIt(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	recordSends(tg)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 1000
	b.config.DebounceMs = 60000
	victim := &models.User{ID: 4343, Username: "victim"}
	var fired atomic.Bool
	require.NoError(t, b.db.Callback().Update().Before("gorm:begin_transaction").Register("test:flood", func(db *gorm.DB) {
		m, ok := db.Statement.Dest.(map[string]any)
		if !ok || m["aside"] != false || !fired.CompareAndSwap(false, true) {
			return
		}
		// The sticker's join: a window's worth of text joins meanwhile (their
		// own joins come back here, and pass).
		for i := 0; i < 2*b.memorySize; i++ {
			storeTextFrom(t, b, groupChatID, mallory, fmt.Sprintf("flood %d", i))
		}
	}))

	b.handleUpdate(context.Background(), nil, rv4Sticker(groupChatID, 9700, victim))
	waitIdle(t, b, groupChatID)

	require.Len(t, stub.requestBetas(), 1, "the sticker turn asked the model")
	_, last := stub.lastMessage(t, 0)
	assert.Equal(t, "Sent a sticker: 😀", last)
}

// The pin lasts until the sticker's turn reads it: after that, a window's
// worth of later text trims the sticker out as it would any message.
func TestSticker_LeavesTheWindowOnceItsTurnReadIt(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	recordSends(tg)
	victim := &models.User{ID: 4343, Username: "victim"}
	b.handleUpdate(context.Background(), nil, rv4Sticker(groupChatID, 9800, victim))
	waitIdle(t, b, groupChatID)
	require.Len(t, stub.requestBetas(), 1, "the sticker turn asked the model")
	require.Contains(t, memoryTexts(b, groupChatID), "Sent a sticker: 😀")

	for i := 0; i < 2*b.memorySize; i++ {
		storeTextFrom(t, b, groupChatID, mallory, fmt.Sprintf("later %d", i))
	}
	assert.NotContains(t, memoryTexts(b, groupChatID), "Sent a sticker: 😀", "its turn read it: the window trims it again")
}

// rv4Sticker is a sticker from from in a supergroup.
func rv4Sticker(chatID int64, id int, from *models.User) *models.Update {
	return &models.Update{Message: &models.Message{
		ID: id, Chat: models.Chat{ID: chatID, Type: models.ChatTypeSupergroup}, From: from,
		Sticker: &models.Sticker{FileID: "s", Emoji: "😀"}}}
}

// Scoped R1 inverted: Alice's sticker waits with its ticket; Mallory's
// question is collected behind the running turn after it, and Alice's "lol"
// waits in its quiet window. When the turn ends the sticker is admitted, but
// does not fold "lol" over Mallory's question: it reads only itself, Mallory
// gets a request of her own, and "lol" runs as its own text turn after it.
func TestSticker_DoesNotFoldOverAnotherSendersText(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, tc := range map[string]struct {
		thread int
		asker  *models.User
	}{
		"group": {0, mallory},
		"forum": {2, mallory},
	} {
		thread, asker := tc.thread, tc.asker
		t.Run(name, func(t *testing.T) {
			stub, open := newGatedAnthropicStub(t, splitAnswer(), 4)
			b, tg := setupBotForTest(t, 123)
			b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
			useAnthropicStub(b, stub)
			oneConnection(t, b)
			sent := recordSends(tg)
			b.config.SystemPrompts = map[string]string{"custom_instructions": "be helpful", "respond_with_emojis": emojiRule}
			ctx := context.Background()
			const question = "what is the bakery's phone number?"
			chatID := groupChatID
			in := func(u *models.Update, _ int) *models.Update { return u }
			if thread != 0 {
				chatID = forumChat
				in = inTopic
			}
			first := in(textUpdate(chatID, 99, alice, "tell me about the farm"), 2)
			firstDone := make(chan struct{})
			go func() { defer close(firstDone); b.handleUpdate(ctx, nil, first) }()
			require.Eventually(t, func() bool {
				b.turnsMu.Lock()
				defer b.turnsMu.Unlock()
				running := b.turns[convKey{chatID: chatID, threadID: thread}]
				return running != nil && running.committed
			}, 5*time.Second, time.Millisecond)
			stickerDone := make(chan struct{})
			go func() {
				defer close(stickerDone)
				b.handleUpdate(ctx, nil, in(rv4Sticker(chatID, 100, alice), 2))
			}()
			waitForMediaWaiter(t, b, chatID)
			b.config.DebounceMs = int(bufferOnly / time.Millisecond)
			b.handleUpdate(ctx, nil, in(textUpdate(chatID, 101, asker, question), thread))
			flushPending(t, b, chatID) // collected behind the running turn
			b.handleUpdate(ctx, nil, in(textUpdate(chatID, 102, alice, "lol"), 2))
			require.True(t, pendingBatch(b, chatID))
			open()
			<-firstDone
			<-stickerDone
			waitIdle(t, b, chatID)
			if pendingBatch(b, chatID) {
				flushPending(t, b, chatID)
				waitIdle(t, b, chatID)
			}

			sticker := requestWith(t, stub, emojiRule) // not folded: the sticker keeps the emoji rule
			assert.False(t, requestHas(t, stub, sticker, "bakery"), "the sticker turn does not read Mallory's question")
			assert.False(t, requestHas(t, stub, sticker, "lol"), "nor fold Alice's text over it")
			own := -1
			for i := sticker + 1; i < len(stub.requestBetas()); i++ {
				if requestHas(t, stub, i, "bakery") && requestHas(t, stub, i, "@"+asker.Username) {
					own = i
				}
			}
			assert.NotEqual(t, -1, own, "Mallory's question is answered after the sticker, in F6 order, by a turn of hers")
			if thread != 0 {
				assert.Contains(t, threadsOf(sent), thread, "and an answer in her topic")
			}
		})
	}
}

// A sticker that took over its sender's waiting text, and whose own join then
// fails, puts that text back: it is answered by a turn of its own.
func TestSticker_ThatDoesNotJoinPutsItsSendersTextBack(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _, _ := mediaTestBot(t, stub)
	var joins atomic.Int32
	require.NoError(t, b.db.Callback().Update().Before("gorm:begin_transaction").Register("test:fail_sticker_join", func(db *gorm.DB) {
		if m, ok := db.Statement.Dest.(map[string]any); ok && m["aside"] == false && joins.Add(1) == 2 {
			_ = db.AddError(errors.New("database is locked")) // the sticker's join, after "lol"'s
		}
	}))

	b.handleUpdate(context.Background(), nil, textUpdate(ownerChat, 50, owner, "lol"))
	b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
		ID: 51, Chat: models.Chat{ID: ownerChat}, From: owner, Sticker: &models.Sticker{FileID: "s", Emoji: "😀"}}})
	waitIdle(t, b, ownerChat)
	require.True(t, b.hasPendingIntake(chatKey(ownerChat)), "the text is back in its quiet window")
	flushPending(t, b, ownerChat)
	waitIdle(t, b, ownerChat)

	require.Len(t, stub.requestBetas(), 1)
	_, last := stub.lastMessage(t, 0)
	assert.Equal(t, "lol", last, "and answered by a turn of its own")
}

// waitForTextOnWay waits until a media turn waits for a text message of the
// chat that is on its way to a batch or a turn.
func waitForTextOnWay(t *testing.T, b *Bot, chatID int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		b.intakeBuffersMu.Lock()
		defer b.intakeBuffersMu.Unlock()
		for k, onWay := range b.textsOnWay {
			if k.chatID == chatID && onWay.changed != nil {
				return true
			}
		}
		return false
	}, 5*time.Second, time.Millisecond)
}

// Scoped R4 inverted: the asker's text has joined memory, and its join's row
// write is slow; another member's sticker in another topic arrives then. The
// sticker waits for the text on its way instead of covering it: the text gets
// its own turn, context and topic, first.
func TestSticker_WaitsForTextOnItsWayToItsBatch(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _, sent := mediaTestBot(t, stub)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	stickerer := &models.User{ID: 4242, Username: "stickerer"}
	const question = "what time does the store open?"
	ctx := context.Background()
	var held atomic.Bool
	inUpdate, proceed := make(chan struct{}), make(chan struct{})
	require.NoError(t, b.db.Callback().Update().Before("gorm:begin_transaction").Register("test:slow_join", func(db *gorm.DB) {
		if m, ok := db.Statement.Dest.(map[string]any); ok && m["aside"] == false && held.CompareAndSwap(false, true) {
			close(inUpdate)
			<-proceed
		}
	}))
	textDone := make(chan struct{})
	go func() {
		defer close(textDone)
		b.handleUpdate(ctx, nil, inTopic(textUpdate(forumChat, 801, asker, question), 1))
	}()
	<-inUpdate // the question is in memory; its row's write waits
	stickerDone := make(chan struct{})
	go func() {
		defer close(stickerDone)
		b.handleUpdate(ctx, nil, inTopic(&models.Update{Message: &models.Message{
			ID: 802, From: stickerer, Sticker: &models.Sticker{FileID: "s", Emoji: "😀"}}}, 1))
	}()
	waitForTextOnWay(t, b, forumChat) // the sticker waits for it
	close(proceed)
	<-textDone
	<-stickerDone
	waitIdle(t, b, forumChat)

	require.Len(t, stub.requestBetas(), 2)
	requireOwnTurn(t, stub, 0, question)
	requireMediaAlone(t, stub, 1, "Sent a sticker: 😀")
	assert.Equal(t, []int{1, 1}, threadsOf(sent))
}

// The same on the grant path: the text's row write is slow while the sticker
// waits behind a running turn. When that turn ends, the chat is not handed to
// the sticker (closeTurn's own check, batchForMedia): the text goes first.
func TestCloseTurn_DoesNotHandTheChatToMediaPastTextOnItsWay(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _, sent := mediaTestBot(t, stub)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	stickerer := &models.User{ID: 4242, Username: "stickerer"}
	const question = "what time does the store open?"
	ctx := context.Background()
	running, _ := b.beginTurnIn(ctx, forumTopic(1))
	var held atomic.Bool
	inUpdate, proceed := make(chan struct{}), make(chan struct{})
	require.NoError(t, b.db.Callback().Update().Before("gorm:begin_transaction").Register("test:slow_join", func(db *gorm.DB) {
		if m, ok := db.Statement.Dest.(map[string]any); ok && m["aside"] == false && held.CompareAndSwap(false, true) {
			close(inUpdate)
			<-proceed
		}
	}))
	textDone := make(chan struct{})
	go func() {
		defer close(textDone)
		b.handleUpdate(ctx, nil, inTopic(textUpdate(forumChat, 811, asker, question), 1))
	}()
	<-inUpdate // the question is in memory; its row's write waits
	stickerDone := make(chan struct{})
	go func() {
		defer close(stickerDone)
		b.handleUpdate(ctx, nil, inTopic(&models.Update{Message: &models.Message{
			ID: 812, From: stickerer, Sticker: &models.Sticker{FileID: "s", Emoji: "😀"}}}, 1))
	}()
	waitForMediaWaiter(t, b, forumChat)

	b.endTurn(running) // hands the chat on in this step, or not
	b.turnsMu.Lock()
	granted := b.turns[forumTopic(1)]
	b.turnsMu.Unlock()
	assert.Nil(t, granted, "the sticker is not handed the chat while the text is on its way")
	close(proceed)
	<-textDone
	<-stickerDone
	waitIdle(t, b, forumChat)

	require.Len(t, stub.requestBetas(), 2)
	requireOwnTurn(t, stub, 0, question)
	requireMediaAlone(t, stub, 1, "Sent a sticker: 😀")
	assert.Equal(t, []int{1, 1}, threadsOf(sent))
}

// blockingWriter blocks the first write that contains match until release.
type blockingWriter struct {
	match   string
	hit     chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.match) {
		w.once.Do(func() { close(w.hit); <-w.release })
	}
	return len(p), nil
}

// Scoped R4b inverted: the quiet window's flush is between taking its batch
// and registering its turn (held there by its log line) when another member's
// sticker arrives: the sticker cannot slip in between, and the question gets
// its own turn first.
func TestFlushIntake_TakesItsBatchAndRegistersInOneStep(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _, sent := mediaTestBot(t, stub)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	stickerer := &models.User{ID: 4242, Username: "stickerer"}
	ctx := context.Background()
	b.handleUpdate(ctx, nil, inTopic(textUpdate(forumChat, 901, asker, "hi,"), 1))
	b.handleUpdate(ctx, nil, inTopic(textUpdate(forumChat, 902, asker, "what time does the store open?"), 1))
	w := &blockingWriter{match: "intake flush: coalesced", hit: make(chan struct{}), release: make(chan struct{})}
	prev := InfoLogger
	InfoLogger = log.New(w, "", 0)
	t.Cleanup(func() { InfoLogger = prev })
	b.intakeBuffersMu.Lock()
	seq := b.intakeBuffers[forumTopic(1)].seq
	b.intakeBuffersMu.Unlock()
	flushDone := make(chan struct{})
	go func() { defer close(flushDone); b.flushIntake(ctx, forumTopic(1), seq) }()
	<-w.hit // the batch is out of the buffer
	registered := make(chan uint64, 1)
	go func() { registered <- turnsRegistered(b) }() // blocks while the flush holds the step
	stickerDone := make(chan struct{})
	go func() {
		defer close(stickerDone)
		b.handleUpdate(ctx, nil, inTopic(&models.Update{Message: &models.Message{
			ID: 903, From: stickerer, Sticker: &models.Sticker{FileID: "s", Emoji: "😀"}}}, 1))
	}()
	select {
	case <-registered:
		t.Error("the chat's turns could be read while the flush was between its take and its registration")
	case <-time.After(300 * time.Millisecond): // whatever the wait, the fixed step holds turnsMu throughout
	}
	close(w.release)
	<-flushDone
	<-stickerDone
	waitIdle(t, b, forumChat)

	require.Len(t, stub.requestBetas(), 2)
	requireOwnTurn(t, stub, 0, "what time does the store open?")
	requireMediaAlone(t, stub, 1, "Sent a sticker: 😀")
	assert.Equal(t, []int{1, 1}, threadsOf(sent))
}
