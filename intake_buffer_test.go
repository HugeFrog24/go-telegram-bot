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

// bufferOnly parks a message in the intake buffer without letting the flush run,
// by using a window long enough that no test waits it out.
const bufferOnly = 10 * time.Second

func bufferText(b *Bot, chatID int64, isEmojiOnly bool) {
	b.bufferIntake(context.Background(), chatID, 555,
		"tester", "Test", "User", false, "en", int(time.Now().Unix()), "", isEmojiOnly)
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
	assert.Equal(t, 5, b.intakeBuffers[900].count, "all five messages land in the same batch")
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
	assert.Equal(t, 1, b.intakeBuffers[910].count)
	assert.Equal(t, 2, b.intakeBuffers[911].count)
}

func TestBufferIntake_ResetsWindowAndKeepsLatestMetadata(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)

	b.bufferIntake(context.Background(), 901, 1, "first", "First", "", false, "en", 1000, "", true)
	b.bufferIntake(context.Background(), 901, 2, "second", "Second", "", true, "de", 2000, "biz-42", false)

	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	pending := b.intakeBuffers[901]
	require.NotNil(t, pending)

	assert.Equal(t, 2, pending.count)
	// OpenClaw semantics: reply metadata follows the most recent message.
	assert.Equal(t, "second", pending.username)
	assert.Equal(t, int64(2), pending.userID)
	assert.Equal(t, "de", pending.languageCode)
	assert.Equal(t, 2000, pending.messageTime)
	assert.Equal(t, "biz-42", pending.businessConnectionID)
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
	assert.True(t, b.intakeBuffers[902].allEmojiOnly)
}

func TestCancelIntake_DiscardsPendingBatch(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)

	bufferText(b, 903, false)
	bufferText(b, 903, false)
	require.True(t, b.hasPendingIntake(903))

	assert.Equal(t, 2, b.cancelIntake(903), "cancel reports how many messages it discarded")
	assert.False(t, b.hasPendingIntake(903))
	assert.Equal(t, 0, b.cancelIntake(903), "cancelling an empty chat is a no-op")
}

// A cancelled buffer must never dispatch afterwards. This is the openclaw#51046
// shape: the timer had already been armed when the cancel landed.
func TestCancelIntake_TimerDoesNotFireAfterCancel(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = 20

	bufferText(b, 904, false)
	b.cancelIntake(904)

	time.Sleep(80 * time.Millisecond)
	assert.False(t, b.hasPendingIntake(904), "cancelled buffer must not resurrect")
}

// A stale timer from a cancelled batch must not flush a newer batch early.
func TestFlushIntake_IgnoresSupersededSequence(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)

	bufferText(b, 905, false)
	b.intakeBuffersMu.Lock()
	staleSeq := b.intakeBuffers[905].seq
	b.intakeBuffersMu.Unlock()

	b.cancelIntake(905)
	bufferText(b, 905, false) // new batch, new sequence

	// The old timer firing late must not consume the new batch.
	b.flushIntake(context.Background(), 905, staleSeq)
	assert.True(t, b.hasPendingIntake(905), "superseded flush must leave the newer batch armed")
}

func TestClearChatHistory_CancelsPendingIntake(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	mockTg.SendMessageFunc = func(_ context.Context, _ *bot.SendMessageParams) (*models.Message, error) {
		return &models.Message{}, nil
	}

	const chatID int64 = 906
	bufferText(b, chatID, false)
	require.True(t, b.hasPendingIntake(chatID))

	b.clearChatHistory(context.Background(), chatID, 123, 0, 0, "", false)

	assert.False(t, b.hasPendingIntake(chatID),
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
