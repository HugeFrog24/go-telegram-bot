package main

import (
	"bytes"
	"context"
	"log"
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

const panicSecret = "my secret phrase"

// lockedBuffer is a log destination safe to read while goroutines write it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureErrorsLocked records ErrorLogger for the rest of the test, readable
// while other goroutines log.
func captureErrorsLocked(t *testing.T) *lockedBuffer {
	t.Helper()
	logged := &lockedBuffer{}
	prev := ErrorLogger
	ErrorLogger = log.New(logged, "", 0)
	t.Cleanup(func() { ErrorLogger = prev })
	return logged
}

// panicOnFirstSend makes the first message sent panic, with the secret in the
// panic's value, and records the sends after it.
func panicOnFirstSend(tg *MockTelegramClient) func() []bot.SendMessageParams {
	return panicOnSend(tg, 1)
}

// panicOnSend makes the nth message sent panic, with the secret in the
// panic's value, and records the others.
func panicOnSend(tg *MockTelegramClient, nth int64) func() []bot.SendMessageParams {
	sent := recordSends(tg)
	inner := tg.SendMessageFunc
	var sends atomic.Int64
	tg.SendMessageFunc = func(ctx context.Context, p *bot.SendMessageParams) (*models.Message, error) {
		if sends.Add(1) == nth {
			panic("boom while sending: " + panicSecret)
		}
		return inner(ctx, p)
	}
	return sent
}

// requireRedactedPanicLog checks the recovered panic was logged with the bot,
// where it happened and the panic's type, but no message text and no token.
func requireRedactedPanicLog(t *testing.T, logged, where string) {
	t.Helper()
	assert.Contains(t, logged, "[test_bot] PANIC recovered in "+where+": string")
	assert.Contains(t, logged, "panic_test.go", "with a short stack")
	assert.NotContains(t, logged, panicSecret, "no message text")
	assert.NotContains(t, logged, "test_token", "no token")
}

// A panic while an update is handled is recovered: the process goes on, the
// log names the bot, the update type, the panic's type and a short stack but
// not the message's text, and the next update is handled.
func TestHandleUpdate_PanicIsRecovered(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	errs := captureErrorsLocked(t)
	sent := panicOnFirstSend(tg)

	require.NotPanics(t, func() {
		b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 1500, alice, "tell me "+panicSecret))
	})
	waitIdle(t, b, groupChatID)
	requireRedactedPanicLog(t, errs.String(), "message update")

	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 1501, alice, "and the bakery?"))
	assert.Equal(t, []string{"Open the Farm tab."}, sentTexts(sent), "the next update is handled")
}

// The same for work an update started on a goroutine of its own: here the
// quiet window's flush, from its timer.
func TestFlushIntake_PanicInTheTimerIsRecovered(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	errs := captureErrorsLocked(t)
	sent := panicOnFirstSend(tg)
	b.config.DebounceMs = 1

	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 1600, alice, "tell me "+panicSecret))
	require.Eventually(t, func() bool { return strings.Contains(errs.String(), "PANIC recovered") },
		5*time.Second, 5*time.Millisecond)
	waitIdle(t, b, groupChatID)
	requireRedactedPanicLog(t, errs.String(), "intake flush")

	b.config.DebounceMs = 0
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 1601, alice, "and the bakery?"))
	assert.Equal(t, []string{"Open the Farm tab."}, sentTexts(sent), "the next update is handled")
}

// The same for a collected flush that the ending turn hands the chat to, on
// a goroutine of its own.
func TestEndTurn_PanicInTheHandedOverTurnIsRecovered(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, splitAnswer(), 4)
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	errs := captureErrorsLocked(t)
	sent := panicOnSend(tg, 3) // the handed-over turn's first segment
	storeTextFrom(t, b, groupChatID, alice, "tell me about the farm")
	_, done := startCommittedTurn(t, b, groupChatID)
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 1700, mallory, "tell me "+panicSecret)) // collected

	open()
	<-done
	require.Eventually(t, func() bool { return strings.Contains(errs.String(), "PANIC recovered") },
		5*time.Second, 5*time.Millisecond)
	waitIdle(t, b, groupChatID)
	requireRedactedPanicLog(t, errs.String(), "text turn")

	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 1701, alice, "and the bakery?"))
	assert.Len(t, sent(), 4, "the next update is handled")
}

// panicWriter panics once, on the first write that contains match.
type panicWriter struct {
	match string
	once  sync.Once
}

func (w *panicWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.match) {
		fired := false
		w.once.Do(func() { fired = true })
		if fired {
			panic("injected")
		}
	}
	return len(p), nil
}

// Scoped R6 inverted: a panic while a turn builds its context (here from the
// log line it writes there) is recovered without leaving the memory lock
// held: a message in another chat is still answered, and the first chat is
// idle again.
func TestPanic_InContextBuildingLeavesNoLockHeld(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	errs := captureErrorsLocked(t)
	prev := InfoLogger
	InfoLogger = log.New(&panicWriter{match: "Chat memory contains"}, "", 0)
	t.Cleanup(func() { InfoLogger = prev })

	b.handleUpdate(context.Background(), nil, textUpdate(ownerChat, 1, owner, "first"))
	require.Contains(t, errs.String(), "PANIC recovered")

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 2, alice, "another chat"))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a message in another chat is stuck behind a lock the recovered panic left held")
	}
	assert.Equal(t, []string{"Open the Farm tab."}, sentTexts(sent), "it is answered")
	assert.True(t, chatIdle(b, ownerChat))
}

// Scoped R7 inverted: a panic while a voice note is fetched for transcription,
// before its media turn, is recovered and stops the typing indicator.
func TestPanic_BeforeTheMediaTurnStopsTheTypingIndicator(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	oneConnection(t, b)
	recordSends(tg)
	(&voiceStubs{text: transcript}).install(t, b, tg)
	captureErrorsLocked(t)
	actions := make(chan context.Context, 8)
	tg.SendChatActionFunc = func(ctx context.Context, _ *bot.SendChatActionParams) (bool, error) {
		actions <- ctx
		return true, nil
	}
	tg.GetFileFunc = func(context.Context, *bot.GetFileParams) (*models.File, error) {
		panic("boom while fetching the note")
	}

	b.handleUpdate(context.Background(), nil, &models.Update{Message: voiceMessage(123, 900)})

	require.NotEmpty(t, actions, "the indicator was started")
	assert.Error(t, (<-actions).Err(), "and stopped: its context is done")
}

// Scoped r5 P2 and blind r4 P2 inverted (K3): a panic in a text's join write,
// after the text was marked on its way, does not leave it on its way: a later
// sticker in the chat is not stuck waiting for it.
func TestPanic_InTheJoinWriteLeavesNoTextOnItsWay(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	recordSends(tg)
	captureErrorsLocked(t)
	var fired atomic.Bool
	require.NoError(t, b.db.Callback().Update().Before("gorm:begin_transaction").Register("test:panic_join", func(db *gorm.DB) {
		if m, ok := db.Statement.Dest.(map[string]any); ok && m["aside"] == false && fired.CompareAndSwap(false, true) {
			panic("injected in the join's write")
		}
	}))
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 20, alice, "hello"))

	b.intakeBuffersMu.Lock()
	left := b.textsOnWay[chatKey(groupChatID)]
	b.intakeBuffersMu.Unlock()
	assert.Nil(t, left, "no text is left on its way")
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleUpdate(context.Background(), nil, rv4Sticker(groupChatID, 21, mallory))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a later sticker waits for the text the panic left on its way")
	}
	waitIdle(t, b, groupChatID)
}

// Scoped r5 P8 inverted (K3): a panic between a turn's registration and its
// hand-off (here in the log line a media turn writes as the text waiting from
// before it runs first) leaves no turn registered that no one runs.
func TestPanic_AroundARegistrationLeavesNoOrphanTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	captureErrorsLocked(t)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 50, alice, "hello"))
	prev := InfoLogger
	InfoLogger = log.New(&panicWriter{match: "runs first"}, "", 0)
	b.handleUpdate(context.Background(), nil, rv4Sticker(groupChatID, 51, mallory))
	InfoLogger = prev

	require.Eventually(t, func() bool { return chatIdle(b, groupChatID) }, 5*time.Second, time.Millisecond, "the chat is not wedged")
	b.config.DebounceMs = 0
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 52, alice, "anyone?"))
	waitIdle(t, b, groupChatID)
	assert.NotEmpty(t, sentTexts(sent), "the next message is answered")
}

// K3: a media turn whose goroutine dies while it waits for the chat leaves no
// waiter behind: the chat is not handed to a turn no one runs.
func TestPanic_InAWaitingMediaTurnLeavesNoWaiter(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	captureErrorsLocked(t)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	early := storeTextFrom(t, b, groupChatID, asker, "what time does the store open?") // joined, not yet buffered
	running, _ := b.beginTurn(context.Background(), groupChatID)
	stickerDone := make(chan struct{})
	go func() {
		defer close(stickerDone)
		b.handleUpdate(context.Background(), nil, rv4Sticker(groupChatID, 70, owner))
	}()
	waitForMediaWaiter(t, b, groupChatID)
	b.bufferIntake(turnRequest{ctx: context.Background(), chatID: groupChatID, userID: asker.ID, username: asker.Username,
		joinedSeq: early.Seq})
	prev := InfoLogger
	InfoLogger = log.New(&panicWriter{match: "runs first"}, "", 0) // the waiting sticker's next round panics
	b.endTurn(running)
	<-stickerDone
	InfoLogger = prev

	b.turnsMu.Lock()
	waiters := len(b.mediaWaiters[chatKey(groupChatID)])
	b.turnsMu.Unlock()
	assert.Zero(t, waiters, "no waiter is left")
	require.Eventually(t, func() bool { return chatIdle(b, groupChatID) }, 5*time.Second, time.Millisecond)
	flushPending(t, b, groupChatID)
	waitIdle(t, b, groupChatID)
	assert.NotEmpty(t, sentTexts(sent), "the question is answered")
}

// K3: a media turn waiting for a text on its way gives up when its update's
// context ends, instead of waiting forever.
func TestMediaTurn_WaitForTextOnItsWayEndsWithItsContext(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	recordSends(tg)
	stranded := b.reserveSeq() // a text on its way that never lands
	b.intakeBuffersMu.Lock()
	b.markTextOnWayLocked(chatKey(groupChatID), stranded)
	b.intakeBuffersMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.handleUpdate(ctx, nil, rv4Sticker(groupChatID, 80, mallory)) }()
	waitForTextOnWay(t, b, groupChatID)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the media turn still waits after its context ended")
	}
	assert.Empty(t, stub.requestBetas())
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	assert.Empty(t, b.mediaWaiters[chatKey(groupChatID)])
}
