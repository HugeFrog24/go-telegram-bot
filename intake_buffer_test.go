package main

import (
	"context"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A batch a media turn took over and put back (rebuffer) merges into a batch
// another sender started meanwhile as mixed: its first sender's own clear
// then does not drop the merged batch, which holds the other's text too.
func TestRebuffer_MergingAnotherSendersBatchMarksItMixed(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	ctx := context.Background()
	lol := storeTextFrom(t, b, groupChatID, alice, "lol")                  // her batch, taken over by her sticker
	question := storeTextFrom(t, b, groupChatID, mallory, "and the mill?") // the batch that started since
	b.bufferIntake(turnRequest{ctx: ctx, chatID: groupChatID, userID: mallory.ID, username: mallory.Username, joinedSeq: question.Seq})
	back := turnRequest{ctx: ctx, chatID: groupChatID, userID: alice.ID, username: alice.Username, joinedSeq: lol.Seq}
	b.rebuffer(&pendingIntake{turnRequest: back, count: 1, firstSeq: lol.Seq, lastSeq: lol.Seq, from: back.sender()})

	k := chatKey(groupChatID)
	_, err := b.beginUserClear(userClear{userID: alice.ID, conv: &k})
	require.NoError(t, err)
	assert.True(t, b.hasPendingIntake(chatKey(groupChatID)), "her clear leaves the batch: it holds Mallory's question too")
}

// bufferOnly parks a message in the intake buffer without letting the flush run,
// by using a window long enough that no test waits it out.
const bufferOnly = 10 * time.Second

func bufferText(b *Bot, chatID int64, isEmojiOnly bool) {
	b.bufferIntake(turnRequest{
		ctx: context.Background(), chatID: chatID, userID: 555,
		username: "tester", firstName: "Test", lastName: "User", languageCode: "en",
		messageTime: int(time.Now().Unix()), isEmojiOnly: isEmojiOnly,
	})
}

func TestBufferIntake_CoalescesIntoSingleTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)

	// The burst this whole feature exists for: one real question, then filler.
	for i := 0; i < 5; i++ {
		bufferText(b, 900, false)
	}

	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	assert.Len(t, b.intakeBuffers, 1, "rapid messages must share one buffer entry")
	assert.Equal(t, 5, b.intakeBuffers[chatKey(900)].count, "all five messages land in the same batch")
}

// Separate chats must not share a window; one user's burst cannot delay another's.
func TestBufferIntake_IsolatesChats(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)

	bufferText(b, 910, false)
	bufferText(b, 911, false)
	bufferText(b, 911, false)

	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	assert.Equal(t, 1, b.intakeBuffers[chatKey(910)].count)
	assert.Equal(t, 2, b.intakeBuffers[chatKey(911)].count)
}

func TestBufferIntake_ResetsWindowAndKeepsLatestMetadata(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)

	b.bufferIntake(turnRequest{
		ctx: context.Background(), chatID: 901, userID: 1, username: "first", firstName: "First",
		languageCode: "en", messageTime: 1000, businessConnectionID: "biz-42", isEmojiOnly: true,
	})
	b.bufferIntake(turnRequest{
		ctx: context.Background(), chatID: 901, userID: 2, username: "second", firstName: "Second",
		isPremium: true, languageCode: "de", messageTime: 2000, businessConnectionID: "biz-42",
		threadID: 31, isEmojiOnly: false,
	})

	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	pending := b.intakeBuffers[convKey{businessConnectionID: "biz-42", chatID: 901}] // one conversation: its connection cannot change
	require.NotNil(t, pending)

	assert.Equal(t, 2, pending.count)
	// OpenClaw semantics: reply metadata follows the most recent message.
	assert.Equal(t, "second", pending.username)
	assert.Equal(t, int64(2), pending.userID)
	assert.Equal(t, "de", pending.languageCode)
	assert.Equal(t, 2000, pending.messageTime)
	assert.Equal(t, "biz-42", pending.businessConnectionID)
	assert.Equal(t, 31, pending.threadID, "the topic follows the most recent message too")
	assert.True(t, pending.isPremium)
	// One non-emoji message makes the whole coalesced turn non-emoji.
	assert.False(t, pending.allEmojiOnly)
}

func TestBufferIntake_AllEmojiOnlySurvivesWhenEveryMessageIsEmoji(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)

	bufferText(b, 902, true)
	bufferText(b, 902, true)

	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	assert.True(t, b.intakeBuffers[chatKey(902)].allEmojiOnly)
}

func TestCancelIntake_DiscardsPendingBatch(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)

	bufferText(b, 903, false)
	bufferText(b, 903, false)
	require.True(t, b.hasPendingIntake(chatKey(903)))

	assert.Equal(t, 2, b.cancelIntake(chatKey(903)), "cancel reports how many messages it discarded")
	assert.False(t, b.hasPendingIntake(chatKey(903)))
	assert.Equal(t, 0, b.cancelIntake(chatKey(903)), "cancelling an empty chat is a no-op")
}

// A cancelled buffer must never dispatch afterwards. This is the openclaw#51046
// shape: the timer had already been armed when the cancel landed.
func TestCancelIntake_TimerDoesNotFireAfterCancel(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = 20

	bufferText(b, 904, false)
	b.cancelIntake(chatKey(904))

	time.Sleep(80 * time.Millisecond)
	assert.False(t, b.hasPendingIntake(chatKey(904)), "cancelled buffer must not resurrect")
}

// A stale timer from a cancelled batch must not flush a newer batch early.
func TestFlushIntake_IgnoresSupersededSequence(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)

	bufferText(b, 905, false)
	b.intakeBuffersMu.Lock()
	staleSeq := b.intakeBuffers[chatKey(905)].seq
	b.intakeBuffersMu.Unlock()

	b.cancelIntake(chatKey(905))
	bufferText(b, 905, false) // new batch, new sequence

	// The old timer firing late must not consume the new batch.
	b.flushIntake(context.Background(), chatKey(905), staleSeq)
	assert.True(t, b.hasPendingIntake(chatKey(905)), "superseded flush must leave the newer batch armed")
}

func TestClearChatHistory_CancelsPendingIntake(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	mockTg.SendMessageFunc = func(_ context.Context, _ *bot.SendMessageParams) (*models.Message, error) {
		return &models.Message{}, nil
	}

	const chatID int64 = 906
	bufferText(b, chatID, false)
	require.True(t, b.hasPendingIntake(chatKey(chatID)))

	b.clearChatHistory(context.Background(), replyTarget{chatID: chatID}, profile{id: 123}, 0, 0, false)

	assert.False(t, b.hasPendingIntake(chatKey(chatID)),
		"clearing history must disarm the buffer, or deleted messages get replayed into memory")
}

func TestDebounceWindow(t *testing.T) {
	cases := []struct {
		name string
		ms   int
		want time.Duration
	}{
		{"unset disables debouncing", 0, 0},
		{"negative disables debouncing", -1, 0},
		{"positive converts to duration", 2500, 2500 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := BotConfig{DebounceMs: tc.ms}
			assert.Equal(t, tc.want, c.DebounceWindow())
		})
	}
}

func TestCacheHistoryEnabled_DefaultsOn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	assert.True(t, (&BotConfig{}).CacheHistoryEnabled(), "cache_history defaults to enabled")

	off := false
	assert.False(t, (&BotConfig{CacheHistory: &off}).CacheHistoryEnabled())

	on := true
	assert.True(t, (&BotConfig{CacheHistory: &on}).CacheHistoryEnabled())
}

// cancelIntake drops a chat's pending buffer without dispatching, returning how
// many messages were discarded, for tests. /clear and /clear_hard drop the
// batch inside their one step (beginClear).
func (b *Bot) cancelIntake(k convKey) int {
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	if pending := b.removeIntakeLocked(k); pending != nil {
		return pending.count
	}
	return 0
}
