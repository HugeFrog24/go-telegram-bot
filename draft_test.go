package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type draftRecorder struct {
	mu    sync.Mutex
	calls []bot.SendMessageDraftParams
	err   error
	delay time.Duration
}

func (r *draftRecorder) send(_ context.Context, p *bot.SendMessageDraftParams) (bool, error) {
	time.Sleep(r.delay)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, *p)
	return r.err == nil, r.err
}

func (r *draftRecorder) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = c.Text
	}
	return out
}

func (r *draftRecorder) count() int { return len(r.texts()) }

const testDraftInterval = 40 * time.Millisecond

// buildTestDraft builds a draft stream against rec without starting its
// sender, so a test can adjust it first.
func buildTestDraft(t *testing.T, rec *draftRecorder, onFirst func()) *draftStream {
	t.Helper()
	b, mockTg := setupBotForTest(t, 123)
	mockTg.SendMessageDraftFunc = rec.send
	d := b.newDraftStream(context.Background(), replyTarget{chatID: 123}, "7", onFirst)
	d.interval = testDraftInterval
	d.keepalive = time.Hour
	t.Cleanup(d.close)
	return d
}

func newTestDraft(t *testing.T, rec *draftRecorder, onFirst func()) *draftStream {
	t.Helper()
	d := buildTestDraft(t, rec, onFirst)
	d.start()
	return d
}

// countingAfter is time.After that counts the timers it arms. A sender loop
// that sleeps arms a few; one that spins arms them as fast as it can.
func countingAfter(n *atomic.Int64) func(time.Duration) <-chan time.Time {
	return func(wait time.Duration) <-chan time.Time {
		n.Add(1)
		return time.After(wait)
	}
}

func eventuallyCalls(t *testing.T, rec *draftRecorder, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return rec.count() == n }, time.Second, 2*time.Millisecond)
}

func TestDraftStream_FirstUpdateSendsPromptlyThenThrottles(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := &draftRecorder{}
	d := newTestDraft(t, rec, nil)

	d.update("Hel")
	eventuallyCalls(t, rec, 1)
	assert.Equal(t, "Hel", rec.texts()[0], "the first text goes out without waiting for the window")

	d.update("Hello")
	d.update("Hello, wor")
	time.Sleep(testDraftInterval / 4)
	assert.Equal(t, 1, rec.count(), "inside the window nothing is sent")

	eventuallyCalls(t, rec, 2)
	assert.Equal(t, "Hello, wor", rec.texts()[1], "only the newest text is sent when the window elapses")
}

func TestDraftStream_SendsAreSequential(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := &draftRecorder{delay: 3 * testDraftInterval}
	d := newTestDraft(t, rec, nil)

	d.update("a")
	time.Sleep(testDraftInterval)
	d.update("ab")
	time.Sleep(testDraftInterval)
	d.update("abc")

	eventuallyCalls(t, rec, 2)
	time.Sleep(2 * testDraftInterval)
	assert.Equal(t, []string{"a", "abc"}, rec.texts(), "a slow send never overlaps the next; the newest text wins")
}

func TestDraftStream_ParamsCarryStopButton(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := &draftRecorder{}
	d := newTestDraft(t, rec, nil)

	d.update("x")
	eventuallyCalls(t, rec, 1)

	p := rec.calls[0]
	assert.Equal(t, "7", p.DraftID)
	assert.Equal(t, int64(123), p.ChatID)
	assert.True(t, p.CanStop)
	assert.True(t, p.KeepOnStop)
}

func TestDraftStream_OnFirstFiresOnce(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := &draftRecorder{}
	var mu sync.Mutex
	fired := 0
	d := newTestDraft(t, rec, func() { mu.Lock(); fired++; mu.Unlock() })

	d.update("a")
	eventuallyCalls(t, rec, 1)
	d.update("ab")
	eventuallyCalls(t, rec, 2)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, fired)
}

func TestDraftStream_FailureStopsFurtherSends(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := &draftRecorder{err: errors.New("TEXTDRAFT_PEER_INVALID")}
	fired := false
	d := newTestDraft(t, rec, func() { fired = true })

	d.update("a")
	eventuallyCalls(t, rec, 1)
	d.update("ab")
	time.Sleep(3 * testDraftInterval)

	assert.Equal(t, 1, rec.count(), "after a failure the draft goes quiet")
	assert.False(t, fired, "a rejected draft never commits the turn")
	assert.Equal(t, "", d.current(), "nothing was shown to the user")
}

func TestDraftStream_ClampsToTelegramLimit(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := &draftRecorder{}
	d := newTestDraft(t, rec, nil)

	d.update(strings.Repeat("é", telegramMessageLimit+500))
	eventuallyCalls(t, rec, 1)

	assert.Equal(t, telegramMessageLimit, len([]rune(rec.texts()[0])), "a draft over the cap is cut, not rejected")
	assert.Eventually(t, func() bool { return d.current() != "" }, time.Second, 2*time.Millisecond)
}

func TestDraftStream_KeepaliveResendsDuringSilence(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := &draftRecorder{}
	d := newTestDraft(t, rec, nil)
	d.keepalive = 2 * testDraftInterval

	d.update("thinking")
	eventuallyCalls(t, rec, 1)

	require.Eventually(t, func() bool { return rec.count() >= 2 }, time.Second, 2*time.Millisecond)
	assert.Equal(t, "thinking", rec.texts()[1], "silence re-sends the same text to keep the draft alive")
}

func TestDraftStream_EmptyUpdateMeansNothingShown(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := &draftRecorder{}
	d := buildTestDraft(t, rec, nil)
	d.keepalive = 2 * testDraftInterval
	var timers atomic.Int64
	d.after = countingAfter(&timers)
	d.start()

	d.update("Let me check")
	require.Eventually(t, func() bool { return d.current() == "Let me check" }, 5*time.Second, 2*time.Millisecond)

	// The text turned out to be a progress note, so the assembler redraws
	// the draft without it: nothing of the answer is on screen any more.
	d.update("")
	armed := timers.Load()
	assert.Equal(t, "", d.current(), "a Stop now has no partial answer to deliver")

	// After the empty update the sender has at most a throttle wait and one
	// look at the text left before it sleeps until new text arrives, so this
	// bound holds however the goroutines are scheduled. A keepalive still
	// armed fires at once from here on and the loop spins.
	time.Sleep(5 * d.keepalive)
	assert.LessOrEqual(t, timers.Load()-armed, int64(4), "the sender sleeps instead of spinning on an empty draft")
	assert.NotContains(t, rec.texts(), "", "an empty draft is never sent")
}

func TestDraftStream_CloseWaitsAndSilences(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := &draftRecorder{delay: 2 * testDraftInterval}
	d := newTestDraft(t, rec, nil)

	d.update("a")
	time.Sleep(testDraftInterval / 4) // the send is now in flight
	d.close()

	n := rec.count()
	assert.LessOrEqual(t, n, 1)
	d.update("after close")
	time.Sleep(3 * testDraftInterval)
	assert.Equal(t, n, rec.count(), "nothing is sent after close returns")
}

func TestDraftEligible(t *testing.T) {
	b, _ := setupBotForTest(t, 123)

	assert.False(t, b.draftEligible(replyTarget{chatID: 123}), "off by default")
	b.config.StreamDrafts = true
	assert.True(t, b.draftEligible(replyTarget{chatID: 123}))
	assert.True(t, b.draftEligible(replyTarget{chatID: 123, threadID: 9}), "a topic does not change eligibility")
	assert.False(t, b.draftEligible(replyTarget{chatID: 123, businessConnectionID: "biz-1"}), "business connections keep segments")
	assert.False(t, b.draftEligible(replyTarget{chatID: -100123}), "groups keep segments")
}

func TestSplitTelegramMessage(t *testing.T) {
	assert.Nil(t, splitTelegramMessage("   ", 10))
	assert.Equal(t, []string{"short"}, splitTelegramMessage("short", 10))

	parts := splitTelegramMessage("para one\n\npara two\n\npara three", 20)
	assert.Equal(t, []string{"para one\n\npara two", "para three"}, parts, "prefers paragraph breaks")

	words := splitTelegramMessage("one two three four", 9)
	assert.Equal(t, []string{"one two", "three", "four"}, words, "falls back to spaces")

	for _, p := range splitTelegramMessage(strings.Repeat("a", 25), 10) {
		assert.LessOrEqual(t, len([]rune(p)), 10, "hard cut when there is no separator")
	}

	multibyte := splitTelegramMessage(strings.Repeat("é", 12), 5)
	assert.Len(t, multibyte, 3, "limits count runes, not bytes")
}

func TestClampRunes(t *testing.T) {
	assert.Equal(t, "abc", clampRunes("abc", 5))
	assert.Equal(t, "abc", clampRunes("abcde", 3))
	assert.Equal(t, "éé", clampRunes("ééé", 2))
	assert.Equal(t, "", clampRunes("x", 0))
}

func TestJoinNonEmpty(t *testing.T) {
	assert.Equal(t, "", joinNonEmpty("", ""))
	assert.Equal(t, "a", joinNonEmpty("a", ""))
	assert.Equal(t, "b", joinNonEmpty("", "b"))
	assert.Equal(t, "a\n\nb", joinNonEmpty("a", "b"))
}
