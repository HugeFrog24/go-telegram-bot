package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The prefill branch's carried Minors (B12).

// Scoped r6 #5 / blind r5 #4 inverted: Mallory's sticker turn asked the
// model; a flood of chatter pushes the sticker out of the window; Mallory
// clears her messages. The clear still stops her media turn: it knows its
// message's sender and row from its admission, not from memory, so its reply
// is neither sent after the confirmation nor filed.
func TestClear_OwnClearStopsTheirMediaTurnAfterTheWindowTrimmedIt(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, size := range map[string]int{"small window": 2, "default window": 10} {
		t.Run(name, func(t *testing.T) {
			stub := newHoldingStub(t, pinStream(), 1, 0)
			b, tg := setupBotForTest(t, 123)
			b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
			b.memorySize = size
			useAnthropicStub(b, stub.anthropicStub)
			oneConnection(t, b)
			sent := recordSends(tg)
			ctx := context.Background()
			bob := &models.User{ID: 204, Username: "bob"}

			stickerDone := make(chan struct{})
			go func() { defer close(stickerDone); b.handleUpdate(ctx, nil, rv4Sticker(groupChatID, 11, mallory)) }()
			stub.waitRequests(t, 1) // the sticker's turn read memory and asked
			b.config.DebounceMs = int(bufferOnly / time.Millisecond)
			for i := 0; i < 2*size+2; i++ {
				b.handleUpdate(ctx, nil, textUpdate(groupChatID, 20+i, bob, fmt.Sprintf("chatter %d", i)))
			}
			require.NotContains(t, memoryTexts(b, groupChatID), "Sent a sticker: 😀", "the sticker left the window")
			b.turnsMu.Lock()
			running := b.turns[chatKey(groupChatID)]
			b.turnsMu.Unlock()
			require.NotNil(t, running)

			b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 90, mallory, "/clear"))
			stopped := b.clearedTurn(running)
			stub.open(0)
			<-stickerDone
			stopPendingTimers(b)
			waitIdle(t, b, groupChatID)

			assert.True(t, stopped, "her clear stops her sticker's turn")
			texts := sentTexts(sent)
			assert.Equal(t, len(texts)-1, lastIndex(texts, ownClearedNotice), "nothing after the confirmation")
			var filed int64
			require.NoError(t, b.db.Model(&Message{}).Where("text = ?", "Noted,\n\nyour PIN is 9876.").Count(&filed).Error)
			assert.Zero(t, filed, "the reply to her cleared sticker is not filed")
		})
	}
}

// The media turn's sender is what a clear is matched against: another
// member's own clear leaves Mallory's sticker turn alone, which still
// answers.
func TestClear_AnothersOwnClearLeavesAMediaTurnAlone(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newHoldingStub(t, pinStream(), 1, 0)
	b, tg := setupBotForTest(t, 123)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	useAnthropicStub(b, stub.anthropicStub)
	oneConnection(t, b)
	sent := recordSends(tg)
	ctx := context.Background()
	stickerDone := make(chan struct{})
	go func() { defer close(stickerDone); b.handleUpdate(ctx, nil, rv4Sticker(groupChatID, 11, mallory)) }()
	stub.waitRequests(t, 1)
	b.turnsMu.Lock()
	running := b.turns[chatKey(groupChatID)]
	b.turnsMu.Unlock()

	b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 12, alice, "/clear"))
	stopped := b.clearedTurn(running)
	stub.open(0)
	<-stickerDone
	waitIdle(t, b, groupChatID)

	assert.False(t, stopped, "Alice's clear does not stop Mallory's sticker turn")
	assert.Contains(t, sentTexts(sent), "Noted,\n\nyour PIN is 9876.", "which answers")
}

// Scoped r6 #7 and Q6 (K1-13), in a forum topic: Alice's unanswered message
// and Mallory's sticker are in topic 2. Mallory's clear stops the sticker
// turn and collects Alice's message as a text rerun, which runs in topic 2's
// conversation, answers in topic 2, and carries Alice's identity and the
// time of her message, not "unknown".
func TestClear_TheCollectedRerunCarriesItsTopicAndItsSender(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newHoldingStubOf(t, [][]string{
		evStream(evTextBlock(0, "Nice sticker, mallory!"), evMessageEnd("end_turn")),
		evStream(evTextBlock(0, "Hello, Alice!"), evMessageEnd("end_turn")),
	}, 1, 0)
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub.anthropicStub)
	oneConnection(t, b)
	sent := recordSends(tg)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	b.config.SystemPrompts = map[string]string{"custom_instructions": "be helpful"}
	storeTextIn(t, b, forumTopic(2), alice, "anyone here?") // unanswered, with no turn of its own
	storeTextIn(t, b, forumTopic(2), &models.User{ID: 206, Username: "bob"}, "me too")
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleUpdate(context.Background(), nil, inTopic(rv4Sticker(forumChat, 900, mallory), 2))
	}()
	stub.waitRequests(t, 1)

	b.handleUpdate(context.Background(), nil, inTopic(commandUpdate(forumChat, 901, mallory, "/clear"), 2))
	stub.open(0)
	<-done
	waitIdle(t, b, forumChat)

	require.Len(t, stub.requestBetas(), 2, "Alice's message is answered by a text turn of its own")
	_, last := stub.lastMessage(t, 1)
	assert.Equal(t, "me too", last)
	assert.True(t, requestHas(t, stub.anthropicStub, 1, "anyone here?"))
	assert.True(t, requestHas(t, stub.anthropicStub, 1, "@alice"), "with the oldest message's sender's identity, Alice's")
	assert.False(t, requestHas(t, stub.anthropicStub, 1, "@bob"))
	assert.False(t, requestHas(t, stub.anthropicStub, 1, "@unknown"))
	assert.Equal(t, []int{2, 2}, threadsOf(sent), "the clear's notice and Alice's answer, both in topic 2")
	assert.Equal(t, "Hello, Alice!", sent()[1].Text)
}

// Blind r5 #5 inverted (P3): Mallory's sticker waits behind Alice's turn;
// Mallory clears her messages, then asks a new question, which the sticker
// takes over at admission. The sticker, cleared, does not join; the question
// it took over, sent after the clear, is still in memory, so it goes back to
// its quiet window and is answered, not dropped with the sticker.
func TestClear_TextSentAfterTheClearFoldedIntoTheClearedStickerIsAnswered(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newHoldingStub(t, candleFarmStream(), 1, 0)
	b, tg := setupBotForTest(t, 123)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	useAnthropicStub(b, stub.anthropicStub)
	oneConnection(t, b)
	recordSends(tg)
	ctx := context.Background()
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)

	b.handleUpdate(ctx, nil, textUpdate(groupChatID, 10, alice, "hi all"))
	stickerDone := make(chan struct{})
	go func() { defer close(stickerDone); b.handleUpdate(ctx, nil, rv4Sticker(groupChatID, 11, mallory)) }()
	stub.waitRequests(t, 1) // Alice's batch ran ahead of the sticker; held
	waitForMediaWaiter(t, b, groupChatID)

	b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 12, mallory, "/clear"))
	const q = "POST-CLEAR question: what is two plus two?"
	b.handleUpdate(ctx, nil, textUpdate(groupChatID, 13, mallory, q))
	require.True(t, pendingBatch(b, groupChatID), "her new question waits in its quiet window")
	stub.open(0)
	<-stickerDone
	waitIdle(t, b, groupChatID)
	require.True(t, pendingBatch(b, groupChatID), "the question is back in its quiet window")
	flushPending(t, b, groupChatID)
	waitIdle(t, b, groupChatID)

	asked := false
	for i := range stub.requestBetas() {
		asked = asked || requestHas(t, stub.anthropicStub, i, "two plus two")
	}
	assert.True(t, asked, "the question sent after the clear is answered")
	assert.NotNil(t, storedRow(t, b, groupChatID, q).AnsweredOn)
	assert.NotContains(t, memoryTexts(b, groupChatID), "Sent a sticker: 😀", "the cleared sticker stays out")
}

// clearAfterSnapshot runs clear, once, as soon as turn's snapshot step lets
// go of memory: the earliest point a clear can reach a turn that read it.
func clearAfterSnapshot(t *testing.T, turn *chatTurn, clear func()) {
	t.Helper()
	atSnapshot(t, &afterSnapshot, func(of *chatTurn) bool { return of == turn }, clear)
}

// atSnapshot runs clear, once, at the snapshot seam point, for the first
// turn match accepts.
func atSnapshot(t *testing.T, point *atomic.Pointer[func(*chatTurn)], match func(*chatTurn) bool, clear func()) {
	t.Helper()
	var once sync.Once
	hook := func(of *chatTurn) {
		if match(of) {
			once.Do(clear)
		}
	}
	point.Store(&hook)
	t.Cleanup(func() { point.Store(nil) })
}

// Blind r5b #2 inverted (TestProbe6_OtherSendersClearInsideTheMediaSnapshot):
// Alice's unanswered message lies before Mallory's sticker; Alice's own
// /clear lands right after the sticker turn read memory. The turn recorded
// what it read in the same step, so the clear sees Alice's message in it: the
// reply, whose request held her text, is stored aside, and after a reload
// nothing of hers, or of the reply, is in the conversation.
func TestClear_AnotherSendersClearAfterTheMediaSnapshotSetsTheReplyAside(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	const echo = "ECHO: alice said ALICE-SECRET-42"
	stub := newAnthropicStub(t, evStream(evTextBlock(0, echo), evMessageEnd("end_turn")))
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	ctx := context.Background()
	b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 898, alice, "/whoami"))
	b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 897, mallory, "/whoami"))
	storeTextFrom(t, b, groupChatID, alice, "ALICE-SECRET-42") // unanswered
	upd := rv4Sticker(groupChatID, 900, mallory)
	to := targetFor(upd.Message, "")
	userMsg, err := b.screenIncomingMessage(upd.Message, to)
	require.NoError(t, err)
	media, ok := b.beginMediaTurn(ctx, to, mallory.ID, userMsg.ID)
	require.True(t, ok)
	turn := media.turn
	joined, outcome := b.joinMedia(userMsg, &media)
	require.Equal(t, joinOK, outcome)

	clearAfterSnapshot(t, turn, func() { b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 901, alice, "/clear")) })
	contextMessages, found := b.snapshotForMedia(turn, b.getOrCreateChatMemory(chatKey(groupChatID)), joined.Seq)
	require.True(t, found)
	b.handleStickerMessage(media.ctx, turn, to, userMsg, upd.Message, contextMessages, true)
	b.endTurn(turn)
	waitIdle(t, b, groupChatID)

	require.Contains(t, sentTexts(sent), ownClearedNotice)
	assert.True(t, storedRow(t, b, groupChatID, echo).Aside, "a reply whose context held Alice's cleared message is stored aside")
	dropChatMemory(b, groupChatID)
	for _, text := range memoryTexts(b, groupChatID) {
		assert.NotContains(t, text, "ALICE-SECRET-42", "nothing of hers comes back after a reload")
	}
}

// Blind r5b #2, the text-turn form: a text turn registered for Mallory's
// message also reads Alice's later one; Alice's own /clear lands right after
// the turn read memory. The turn recorded that it read Alice's message, so,
// having shown nothing, it restarts without it: no request after the clear
// holds her text, and no reply to it is filed.
func TestClear_AnotherSendersClearAfterTheTextSnapshotRestartsTheTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	recordSends(tg)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	ctx := context.Background()
	b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 898, alice, "/whoami"))
	m := storeTextFrom(t, b, groupChatID, mallory, "MALLORY: what is new?")
	req := turnRequest{ctx: ctx, chatID: groupChatID, userID: mallory.ID, username: mallory.Username,
		joinedSeq: m.Seq, firstSeq: m.Seq}
	turn, turnCtx, ok := b.beginTextTurn(req, m.Seq) // registered for Mallory's message alone
	require.True(t, ok)
	storeTextFrom(t, b, groupChatID, alice, "ALICE-SECRET-43") // joins before the turn reads memory

	var cleared int
	clearAfterSnapshot(t, turn, func() {
		cleared = len(stub.requestBetas())
		b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 901, alice, "/clear"))
	})
	b.runTextTurn(turn, turnCtx, req)
	waitIdle(t, b, groupChatID)

	require.NotEmpty(t, stub.requestBetas())
	for i := cleared; i < len(stub.requestBetas()); i++ {
		assert.False(t, requestHas(t, stub, i, "ALICE-SECRET-43"), "request %d, after her clear, does not hold her text", i)
	}
	assert.False(t, requestHas(t, stub, len(stub.requestBetas())-1, "ALICE-SECRET-43"))
	var echoed int64
	require.NoError(t, b.db.Model(&Message{}).Where("is_user = ? AND aside = ? AND text <> ''", false, false).Count(&echoed).Error)
	assert.Equal(t, int64(1), echoed, "one reply is filed, by the turn that read memory without her")
	assert.Equal(t, []string{"MALLORY: what is new?", "Open the Farm tab."}, memoryTexts(b, groupChatID))
}

// B12 (blind r5 / scoped r6 answer (c)): a clear lands while an album is
// downloading, and the album's first error is a failed download rather than
// the clear. The album was cleared: nothing more is said, no "couldn't
// process one of your images" after the clear's confirmation.
func TestClear_AnAlbumWhoseDownloadFailsAfterTheClearSaysNothing(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, ownerChat)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	serveTelegramFiles(t, tg, nil)
	tg.GetFileFunc = func(context.Context, *bot.GetFileParams) (*models.File, error) {
		b.handleUpdate(context.Background(), nil, privateCommand(owner, 9702, "/clear")) // lands during the download
		return nil, errors.New("telegram: the file is gone")
	}
	item := &models.Update{Message: &models.Message{
		ID: 9700, Chat: models.Chat{ID: ownerChat, Type: models.ChatTypePrivate}, From: owner,
		MediaGroupID: "g9", Photo: []models.PhotoSize{{FileID: "p1", Width: 1, Height: 1}}}}
	b.handleUpdate(context.Background(), nil, item)

	b.flushAlbum(context.Background(), albumKey{conv: chatKey(ownerChat), mediaGroupID: "g9"})
	waitIdle(t, b, ownerChat)

	assert.Equal(t, []string{clearedNotice}, sentTexts(sent), "only the clear's confirmation")
	assert.Empty(t, stub.requestBetas())
}

// K1-12: Mallory's photo has joined, and her own /clear takes it out of
// memory just before its turn reads memory: the turn has nothing to answer,
// ends silently, and the photo's upload is deleted.
func TestClear_APhotoClearedBeforeItsTurnReadsMemoryLosesItsUpload(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	serveTelegramFiles(t, tg, nil)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	ctx := context.Background()
	b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 890, mallory, "/whoami")) // she exists
	atSnapshot(t, &beforeSnapshot, func(of *chatTurn) bool { return of.mediaFrom == mallory.ID }, func() {
		b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 892, mallory, "/clear"))
	})

	b.handleUpdate(ctx, nil, &models.Update{Message: &models.Message{
		ID: 891, Chat: models.Chat{ID: groupChatID, Type: models.ChatTypeSupergroup}, From: mallory,
		Photo: []models.PhotoSize{{FileID: "p", Width: 1, Height: 1}}}})
	waitIdle(t, b, groupChatID)

	assert.Empty(t, stub.requestBetas(), "nothing is asked about the cleared photo")
	assert.Equal(t, []string{"👤 Your Information:\n\n- Username: mallory\n- Role: user", ownClearedNotice}, sentTexts(sent))
	require.Eventually(t, func() bool { return len(stub.deletedFiles()) == 1 }, 5*time.Second, 5*time.Millisecond,
		"its upload is deleted")
}

// K3-6: a photo, uploaded, that gives up waiting (its update's context ends
// while a text from before it is on its way) deletes its uploads.
func TestPhoto_ThatGivesUpWaitingDeletesItsUploads(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, ownerChat)
	useAnthropicStub(b, stub)
	recordSends(tg)
	serveTelegramFiles(t, tg, nil)
	stranded := b.reserveSeq() // a text on its way that never lands
	b.intakeBuffersMu.Lock()
	b.markTextOnWayLocked(chatKey(ownerChat), stranded)
	b.intakeBuffersMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.handleUpdate(ctx, nil, photoUpdate(990)) }()
	waitForTextOnWay(t, b, ownerChat) // uploaded, it waits for the text

	cancel()
	<-done
	assert.Empty(t, stub.requestBetas())
	require.Eventually(t, func() bool { return len(stub.deletedFiles()) == 1 }, 5*time.Second, 5*time.Millisecond,
		"the upload of the photo that gave up is deleted")
}

// K3-4b: a media turn waiting for a text on its way is handed the chat just
// before its update's context ends. It gives up, and ends the turn it was
// handed, so the chat does not stay taken by a turn nobody runs.
func TestMediaTurn_AGrantJustBeforeGivingUpIsEnded(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	recordSends(tg)
	k := chatKey(groupChatID)
	running, _ := b.beginTurn(context.Background(), groupChatID)
	stranded := b.reserveSeq() // below the sticker's ticket
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.handleUpdate(ctx, nil, rv4Sticker(groupChatID, 80, mallory)) }()
	waitForMediaWaiter(t, b, groupChatID)
	b.intakeBuffersMu.Lock()
	b.markTextOnWayLocked(k, stranded)
	b.intakeBuffersMu.Unlock()
	b.endTurn(running) // the chat is free, but the text from before the sticker is on its way
	waitForTextOnWay(t, b, groupChatID)

	b.turnsMu.Lock()
	w := b.mediaWaiters[k][0]
	granted, grantedCtx := b.registerTurnLocked(w.parent, k, 0, true)
	w.granted <- mediaTurn{turn: granted, ctx: grantedCtx, ticket: w.ticket, from: w.from}
	b.turnsMu.Unlock()
	cancel() // and then its context ends
	<-done

	require.Eventually(t, func() bool { return chatIdle(b, groupChatID) }, 5*time.Second, time.Millisecond,
		"the turn it was handed is ended")
	assert.Empty(t, stub.requestBetas())
}
