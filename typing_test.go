package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartChatAction_SendsImmediately(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)

	var mu sync.Mutex
	var got []*bot.SendChatActionParams
	mockTg.SendChatActionFunc = func(_ context.Context, p *bot.SendChatActionParams) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, p)
		return true, nil
	}

	stop := b.startChatAction(context.Background(), 42, "biz-7", "typing")
	stop()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 1, "the indicator must show before the slow work starts, not after")
	assert.Equal(t, int64(42), got[0].ChatID)
	assert.EqualValues(t, "typing", got[0].Action)
	assert.Equal(t, "biz-7", got[0].BusinessConnectionID,
		"business chats need the connection id or the indicator never renders")
}

func TestStartChatAction_OmitsEmptyBusinessConnectionID(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)

	var mu sync.Mutex
	var captured *bot.SendChatActionParams
	mockTg.SendChatActionFunc = func(_ context.Context, p *bot.SendChatActionParams) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		captured = p
		return true, nil
	}

	stop := b.startChatAction(context.Background(), 42, "", "typing")
	stop()

	mu.Lock()
	defer mu.Unlock()
	require.NotNil(t, captured)
	assert.Empty(t, captured.BusinessConnectionID)
}

// The openclaw#27177 guard: once the turn ends, the keepalive must stop. A loop
// that can re-arm after completion left the indicator stuck on until restart.
func TestStartChatAction_StopHaltsKeepalive(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)

	var calls atomic.Int32
	mockTg.SendChatActionFunc = func(_ context.Context, _ *bot.SendChatActionParams) (bool, error) {
		calls.Add(1)
		return true, nil
	}

	stop := b.startChatAction(context.Background(), 42, "", "typing")
	stop()
	after := calls.Load()

	// Well past a refresh tick had the loop survived.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, after, calls.Load(), "no chat action may be sent after stop")
}

// Cancelling the parent context must also tear the loop down, so a turn aborted
// upstream cannot leak a goroutine that keeps calling Telegram.
func TestStartChatAction_ParentCancelHaltsKeepalive(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)

	var calls atomic.Int32
	mockTg.SendChatActionFunc = func(_ context.Context, _ *bot.SendChatActionParams) (bool, error) {
		calls.Add(1)
		return true, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	stop := b.startChatAction(ctx, 42, "", "typing")
	defer stop()

	cancel()
	after := calls.Load()
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, after, calls.Load(), "parent cancellation must stop the keepalive")
}

func TestStartChatAction_StopIsIdempotent(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	mockTg.SendChatActionFunc = func(_ context.Context, _ *bot.SendChatActionParams) (bool, error) {
		return true, nil
	}

	stop := b.startChatAction(context.Background(), 42, "", "typing")
	// The voice path calls stop early and again via defer.
	assert.NotPanics(t, func() {
		stop()
		stop()
	})
}

// A failed indicator is cosmetic and must never surface as a turn failure.
func TestStartChatAction_SendErrorIsNonFatal(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	mockTg.SendChatActionFunc = func(_ context.Context, _ *bot.SendChatActionParams) (bool, error) {
		return false, assert.AnError
	}

	assert.NotPanics(t, func() {
		stop := b.startChatAction(context.Background(), 42, "", "typing")
		stop()
	})
}

// Telegram clears the status after "5 seconds or less", so the refresh must be
// strictly under that or the indicator visibly drops out mid-turn.
func TestTypingRefreshInterval_UnderTelegramExpiry(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	assert.Less(t, typingRefreshInterval, 5*time.Second,
		"Telegram expires a chat action after at most 5s")
}
