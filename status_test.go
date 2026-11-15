package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testStatusTiming = 20 * time.Millisecond

var testStatusTarget = replyTarget{chatID: -100500, threadID: 77}

// statusCall is one Telegram call the status line made, in the order made.
type statusCall struct {
	kind   string // send, edit, delete, deleteBusiness, rights
	params any
	at     time.Time // when the call began
	ctxErr error     // the call's context error when the call returned
}

// statusRecorder stands in for Telegram. It records every call and gives
// sent messages IDs 501, 502 and so on.
type statusRecorder struct {
	mu          sync.Mutex
	calls       []statusCall
	nextID      int
	sendErr     error
	hold        chan struct{} // when set, sends wait here until release
	releaseOnce sync.Once
	editErr     error
	deleteErr   error // deleteBusinessMessages fails with it
	conn        *models.BusinessConnection
	connErr     error
}

func newStatusRecorder() *statusRecorder { return &statusRecorder{nextID: 501} }

// holdSends makes every send, once recorded, stay in flight until release.
// Tests use it instead of sleeps to make "while a send is in flight" certain.
func (r *statusRecorder) holdSends() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hold = make(chan struct{})
}

// release lets held sends return. It is safe to call more than once.
func (r *statusRecorder) release() {
	r.mu.Lock()
	hold := r.hold
	r.mu.Unlock()
	if hold != nil {
		r.releaseOnce.Do(func() { close(hold) })
	}
}

func (r *statusRecorder) begin(kind string, params any) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, statusCall{kind: kind, params: params, at: time.Now()})
	return len(r.calls) - 1
}

// end stamps call i with its context error as the call returns. The caller
// holds r.mu.
func (r *statusRecorder) end(ctx context.Context, i int) {
	r.calls[i].ctxErr = ctx.Err()
}

func (r *statusRecorder) install(m *MockTelegramClient) {
	m.SendMessageFunc = func(ctx context.Context, p *bot.SendMessageParams) (*models.Message, error) {
		i := r.begin("send", *p)
		r.mu.Lock()
		hold := r.hold
		r.mu.Unlock()
		if hold != nil {
			<-hold
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.end(ctx, i)
		if r.sendErr != nil {
			return nil, r.sendErr
		}
		id := r.nextID
		r.nextID++
		return &models.Message{ID: id}, nil
	}
	m.EditMessageTextFunc = func(ctx context.Context, p *bot.EditMessageTextParams) (*models.Message, error) {
		i := r.begin("edit", *p)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.end(ctx, i)
		if r.editErr != nil {
			return nil, r.editErr
		}
		return &models.Message{ID: p.MessageID}, nil
	}
	m.DeleteMessageFunc = func(ctx context.Context, p *bot.DeleteMessageParams) (bool, error) {
		i := r.begin("delete", *p)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.end(ctx, i)
		return true, nil
	}
	m.DeleteBusinessMessagesFunc = func(ctx context.Context, p *bot.DeleteBusinessMessagesParams) (bool, error) {
		i := r.begin("deleteBusiness", *p)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.end(ctx, i)
		if r.deleteErr != nil {
			return false, r.deleteErr
		}
		return true, nil
	}
	m.GetBusinessConnectionFunc = func(ctx context.Context, p *bot.GetBusinessConnectionParams) (*models.BusinessConnection, error) {
		i := r.begin("rights", *p)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.end(ctx, i)
		if r.connErr != nil {
			return nil, r.connErr
		}
		if r.conn != nil {
			return r.conn, nil
		}
		return &models.BusinessConnection{IsEnabled: true, Rights: fullBusinessRights()}, nil
	}
}

func fullBusinessRights() *models.BusinessBotRights {
	return &models.BusinessBotRights{CanReply: true, CanDeleteSentMessages: true}
}

func (r *statusRecorder) set(f func(r *statusRecorder)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(r)
}

func (r *statusRecorder) snapshot() []statusCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]statusCall(nil), r.calls...)
}

func (r *statusRecorder) count() int { return len(r.snapshot()) }

// waitCalls waits until at least n calls have been made. The timeout only
// matters when a test fails, so it is generous for loaded runners.
func (r *statusRecorder) waitCalls(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return r.count() >= n }, 5*time.Second, 2*time.Millisecond,
		"expected %d Telegram calls", n)
}

// buildTestStatusLine builds a status line with 20 ms timings against rec
// without starting its sender, so a test can queue notes before the sender
// can act on any of them, however slowly the test goroutine runs.
func buildTestStatusLine(t *testing.T, rec *statusRecorder, turnCtx context.Context, to replyTarget) *statusLine {
	t.Helper()
	return buildTestStatusLineFor(t, rec, turnCtx, context.Background(), to)
}

// buildTestStatusLineFor is buildTestStatusLine with the delivery context
// given, for tests where the process shuts down mid-turn.
func buildTestStatusLineFor(t *testing.T, rec *statusRecorder, turnCtx, deliverCtx context.Context, to replyTarget) *statusLine {
	t.Helper()
	b, mockTg := setupBotForTest(t, 123)
	rec.install(mockTg)
	b.statusFirstDelay = testStatusTiming
	b.statusEditInterval = testStatusTiming
	s := b.newStatusLine(turnCtx, deliverCtx, to)
	t.Cleanup(func() {
		rec.release() // a test that stopped while holding a send must not hang close
		s.close()
	})
	return s
}

// newTestStatusLine is buildTestStatusLine with the sender already running.
func newTestStatusLine(t *testing.T, rec *statusRecorder, turnCtx context.Context, to replyTarget) *statusLine {
	t.Helper()
	s := buildTestStatusLine(t, rec, turnCtx, to)
	s.start()
	return s
}

func sentParams(t *testing.T, c statusCall) bot.SendMessageParams {
	t.Helper()
	require.Equal(t, "send", c.kind)
	return c.params.(bot.SendMessageParams)
}

func editParams(t *testing.T, c statusCall) bot.EditMessageTextParams {
	t.Helper()
	require.Equal(t, "edit", c.kind)
	return c.params.(bot.EditMessageTextParams)
}

func plainDelete(id int) bot.DeleteMessageParams {
	return bot.DeleteMessageParams{ChatID: testStatusTarget.chatID, MessageID: id}
}

func TestStatusLine_NoNotesMakesNoCalls(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := newTestStatusLine(t, rec, context.Background(), testStatusTarget)

	time.Sleep(2 * testStatusTiming)
	s.close()

	assert.Empty(t, rec.snapshot(), "a turn without notes makes no Telegram calls")
}

func TestStatusLine_FirstNoteWaitsThenSendsSilently(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := newTestStatusLine(t, rec, context.Background(), testStatusTarget)

	noted := time.Now()
	s.note("Checking the docs")
	rec.waitCalls(t, 1)
	c := rec.snapshot()[0]
	p := sentParams(t, c)
	assert.Equal(t, "Checking the docs", p.Text)
	assert.True(t, p.DisableNotification, "the status message never makes the phone ring")
	assert.Equal(t, int64(-100500), p.ChatID)
	assert.Equal(t, 77, p.MessageThreadID)
	assert.Empty(t, p.BusinessConnectionID)
	// Timestamps, not a sleep: no call can begin before the first delay,
	// however the goroutines are scheduled.
	assert.GreaterOrEqual(t, c.at.Sub(noted), testStatusTiming, "the send waits for the first delay")
}

func TestStatusLine_FirstSendShowsNewestNote(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := buildTestStatusLine(t, rec, context.Background(), testStatusTarget)

	// Both notes are waiting before the sender starts.
	s.note("A")
	s.note("B")
	s.start()
	rec.waitCalls(t, 1)
	assert.Equal(t, "B", sentParams(t, rec.snapshot()[0]).Text)

	time.Sleep(3 * testStatusTiming)
	assert.Equal(t, 1, rec.count(), "the newest note is already shown, so nothing is edited")
}

func TestStatusLine_LaterNotesEditInPlaceThrottled(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	rec.holdSends()
	s := newTestStatusLine(t, rec, context.Background(), testStatusTarget)

	s.note("A")
	rec.waitCalls(t, 1)
	// The send is held, so C and D are both waiting before any edit can begin.
	s.note("C")
	s.note("D")
	released := time.Now()
	rec.release()
	rec.waitCalls(t, 2)
	time.Sleep(3 * testStatusTiming)

	calls := rec.snapshot()
	require.Len(t, calls, 2, "two quick notes make exactly one edit")
	p := editParams(t, calls[1])
	assert.Equal(t, 501, p.MessageID)
	assert.Equal(t, int64(-100500), p.ChatID)
	assert.Equal(t, "D", p.Text, "the edit shows the newest note")
	assert.Empty(t, p.BusinessConnectionID)
	assert.GreaterOrEqual(t, calls[1].at.Sub(released), testStatusTiming, "the edit waits the edit interval after the send")
}

func TestStatusLine_UnchangedNoteNotEdited(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := newTestStatusLine(t, rec, context.Background(), testStatusTarget)

	s.note("A")
	rec.waitCalls(t, 1)
	s.note("A")
	time.Sleep(3 * testStatusTiming)

	assert.Equal(t, 1, rec.count(), "unchanged text is never sent as an edit")
}

func TestStatusLine_NotModifiedErrorIsHarmless(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := newTestStatusLine(t, rec, context.Background(), testStatusTarget)

	s.note("A")
	rec.waitCalls(t, 1)
	rec.set(func(r *statusRecorder) { r.editErr = errors.New("Bad Request: message is not modified") })
	s.note("B")
	rec.waitCalls(t, 2)
	s.note("E")
	rec.waitCalls(t, 3)

	calls := rec.snapshot()
	assert.Equal(t, "B", editParams(t, calls[1]).Text)
	assert.Equal(t, "E", editParams(t, calls[2]).Text, "a not-modified error does not end the notes")
}

func TestStatusLine_AnswerSentDeletesThenNewNoteStartsNew(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := newTestStatusLine(t, rec, context.Background(), testStatusTarget)

	s.note("A")
	rec.waitCalls(t, 1)
	s.answerSent()
	rec.waitCalls(t, 2)
	calls := rec.snapshot()
	require.Equal(t, "delete", calls[1].kind)
	assert.Equal(t, plainDelete(501), calls[1].params)

	noted := time.Now()
	s.note("F")
	rec.waitCalls(t, 3)
	calls = rec.snapshot()
	assert.Equal(t, "F", sentParams(t, calls[2]).Text, "the next note starts a new status message")
	assert.GreaterOrEqual(t, calls[2].at.Sub(noted), testStatusTiming, "a new status message waits the first delay again")

	s.close()
	calls = rec.snapshot()
	require.Len(t, calls, 4)
	assert.Equal(t, plainDelete(502), calls[3].params, "close removes the new status message")
}

func TestStatusLine_AnswerSentDropsWaitingNote(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := buildTestStatusLine(t, rec, context.Background(), testStatusTarget)

	// answerSent lands inside the first delay: the sender has not started yet.
	s.note("A")
	s.answerSent()
	s.start()
	time.Sleep(5 * testStatusTiming)

	assert.Zero(t, rec.count(), "a note overtaken by the answer is never shown")
}

func TestStatusLine_DropWaitingKeepsNotesAndShownMessage(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	rec.holdSends()
	s := buildTestStatusLine(t, rec, context.Background(), testStatusTarget)

	// Dropped before the sender starts: the note can never be shown.
	s.note("A")
	s.dropWaiting()
	s.start()
	assert.Never(t, func() bool { return rec.count() != 0 }, 5*testStatusTiming, 2*time.Millisecond,
		"a dropped note is never sent")

	s.note("B")
	rec.waitCalls(t, 1) // the send is in flight until released
	assert.Equal(t, "B", sentParams(t, rec.snapshot()[0]).Text, "dropping a note does not end the notes")

	// The sender is busy with the send, so C is certainly still waiting.
	s.note("C")
	s.dropWaiting()
	rec.release()
	assert.Never(t, func() bool { return rec.count() != 1 }, 5*testStatusTiming, 2*time.Millisecond,
		"the dropped note is not edited in, and the shown message stays")

	s.note("D")
	rec.waitCalls(t, 2)
	e := editParams(t, rec.snapshot()[1])
	assert.Equal(t, 501, e.MessageID)
	assert.Equal(t, "D", e.Text, "a later note still edits the shown message")

	s.close()
	calls := rec.snapshot()
	require.Len(t, calls, 3)
	assert.Equal(t, plainDelete(501), calls[2].params)
}

func TestStatusLine_FreezeKeepsMessageAndIgnoresNotes(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := newTestStatusLine(t, rec, context.Background(), testStatusTarget)

	s.note("A")
	rec.waitCalls(t, 1)
	s.freeze()
	s.note("G")
	time.Sleep(3 * testStatusTiming)
	assert.Equal(t, 1, rec.count(), "a frozen status line is not edited")

	s.close()
	calls := rec.snapshot()
	require.Len(t, calls, 2)
	require.Equal(t, "delete", calls[1].kind)
	assert.Equal(t, plainDelete(501), calls[1].params)
}

func TestStatusLine_CloseDeletesWithDeliverCtx(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	turnCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newTestStatusLine(t, rec, turnCtx, testStatusTarget)

	s.note("A")
	rec.waitCalls(t, 1)
	cancel()
	s.close()

	calls := rec.snapshot()
	require.Len(t, calls, 2, "the delete is done by the time close returns")
	require.Equal(t, "delete", calls[1].kind)
	assert.Equal(t, plainDelete(501), calls[1].params)
	assert.NoError(t, calls[1].ctxErr, "the delete runs on the delivery context, which a restart does not cancel")
}

// showThenShutDown shows one status message, then cancels the delivery
// context the way a process shutdown does.
func showThenShutDown(t *testing.T, rec *statusRecorder) *statusLine {
	t.Helper()
	deliverCtx, shutdown := context.WithCancel(context.Background())
	s := buildTestStatusLineFor(t, rec, context.Background(), deliverCtx, testStatusTarget)
	s.start()
	s.note("A")
	rec.waitCalls(t, 1)
	shutdown()
	return s
}

func TestStatusLine_CloseDeletesAfterShutdown(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := showThenShutDown(t, rec)

	var ctxErr error
	hasDeadline := false
	s.b.tgBot.(*MockTelegramClient).DeleteMessageFunc = func(ctx context.Context, p *bot.DeleteMessageParams) (bool, error) {
		ctxErr = ctx.Err()
		_, hasDeadline = ctx.Deadline()
		rec.begin("delete", *p)
		return true, nil
	}
	s.close()

	calls := rec.snapshot()
	require.Len(t, calls, 2)
	assert.Equal(t, plainDelete(501), calls[1].params, "the status message is removed although the process is stopping")
	assert.NoError(t, ctxErr, "the delete runs on a context the shutdown did not cancel")
	assert.True(t, hasDeadline, "that context has a deadline, so a stuck delete cannot hold up the exit")
}

func TestStatusLine_ShutdownDeleteGivesUp(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := showThenShutDown(t, rec)
	s.shutdownGrace = testStatusTiming

	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) }) // runs before the status line's own cleanup
	s.b.tgBot.(*MockTelegramClient).DeleteMessageFunc = func(ctx context.Context, _ *bot.DeleteMessageParams) (bool, error) {
		select { // Telegram never answers
		case <-ctx.Done():
			return false, ctx.Err()
		case <-unblock:
			return false, errors.New("test over")
		}
	}
	closed := make(chan struct{})
	go func() {
		s.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close hung on a delete Telegram never answered")
	}
}

func TestStatusLine_ShutdownWaitsForOpenLines(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	newStatusRecorder().install(mockTg)
	assert.True(t, b.waitForStatusLines(time.Hour), "with no status line open there is nothing to wait for")

	s := b.newStatusLine(context.Background(), context.Background(), testStatusTarget)
	s.start()
	t.Cleanup(s.close)
	waited := make(chan bool, 1)
	go func() { waited <- b.waitForStatusLines(time.Hour) }()
	assert.Never(t, func() bool { return len(waited) != 0 }, 5*testStatusTiming, 2*time.Millisecond,
		"an open status line holds up the shutdown")

	s.close()
	select {
	case ok := <-waited:
		assert.True(t, ok, "every line closed before the deadline")
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end once the last line closed")
	}
}

func TestStatusLine_ShutdownWaitGivesUpAtDeadline(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	newStatusRecorder().install(mockTg)
	s := b.newStatusLine(context.Background(), context.Background(), testStatusTarget)
	s.start()
	t.Cleanup(s.close)

	began := time.Now()
	assert.False(t, b.waitForStatusLines(testStatusTiming), "a line still open at the deadline")
	assert.GreaterOrEqual(t, time.Since(began), testStatusTiming, "the wait lasts until the deadline")
}

func TestStatusLine_SendFailureStopsNotes(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	rec.sendErr = errors.New("Forbidden: bot was blocked by the user")
	s := newTestStatusLine(t, rec, context.Background(), testStatusTarget)

	s.note("A")
	rec.waitCalls(t, 1)
	s.note("H")
	time.Sleep(5 * testStatusTiming)
	assert.Equal(t, 1, rec.count(), "after a failed send the turn shows no more notes")

	s.close()
	assert.Equal(t, 1, rec.count(), "nothing was shown, so nothing is deleted")
}

func TestStatusLine_EditFailureStopsNotesButCloseDeletes(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := newTestStatusLine(t, rec, context.Background(), testStatusTarget)

	s.note("A")
	rec.waitCalls(t, 1)
	rec.set(func(r *statusRecorder) { r.editErr = errors.New("Too Many Requests: retry after 5") })
	s.note("B")
	rec.waitCalls(t, 2)
	s.note("C")
	time.Sleep(5 * testStatusTiming)
	assert.Equal(t, 2, rec.count(), "after a failed edit the turn shows no more notes")

	s.close()
	calls := rec.snapshot()
	require.Len(t, calls, 3)
	require.Equal(t, "delete", calls[2].kind)
	assert.Equal(t, plainDelete(501), calls[2].params, "the message already shown is still removed")
}

func TestStatusLine_ClampsLongNote(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := newTestStatusLine(t, rec, context.Background(), testStatusTarget)

	s.note(strings.Repeat("é", 5000))
	rec.waitCalls(t, 1)

	assert.Equal(t, telegramMessageLimit, utf8.RuneCountInString(sentParams(t, rec.snapshot()[0]).Text))
}

func TestStatusLine_BusinessUsesBusinessDelete(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := newTestStatusLine(t, rec, context.Background(), replyTarget{chatID: 555, businessConnectionID: "biz-1"})

	s.note("A")
	rec.waitCalls(t, 2)
	calls := rec.snapshot()
	require.Equal(t, "rights", calls[0].kind, "rights are looked up before the first send")
	assert.Equal(t, bot.GetBusinessConnectionParams{BusinessConnectionID: "biz-1"}, calls[0].params)
	p := sentParams(t, calls[1])
	assert.Equal(t, "biz-1", p.BusinessConnectionID)
	assert.Equal(t, int64(555), p.ChatID)
	assert.True(t, p.DisableNotification)

	s.note("B")
	rec.waitCalls(t, 3)
	e := editParams(t, rec.snapshot()[2])
	assert.Equal(t, "biz-1", e.BusinessConnectionID)
	assert.Equal(t, 501, e.MessageID)

	s.close()
	calls = rec.snapshot()
	require.Len(t, calls, 4)
	require.Equal(t, "deleteBusiness", calls[3].kind)
	assert.Equal(t, bot.DeleteBusinessMessagesParams{BusinessConnectionID: "biz-1", MessageIDs: []int{501}}, calls[3].params)
	for _, c := range calls {
		assert.NotEqual(t, "delete", c.kind, "business messages are never deleted with deleteMessage")
	}
}

func TestStatusLine_BusinessDeleteFailureForgetsRights(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := newTestStatusLine(t, rec, context.Background(), replyTarget{chatID: 555, businessConnectionID: "biz-1"})

	s.note("A")
	rec.waitCalls(t, 2) // the rights lookup, then the send
	rec.set(func(r *statusRecorder) { r.deleteErr = errors.New("Bad Request: message can't be deleted") })
	s.close()

	// The rights may have changed mid-turn, so the next turn looks them up again.
	assert.True(t, s.b.businessNotesAllowed(context.Background(), "biz-1"))
	var kinds []string
	for _, c := range rec.snapshot() {
		kinds = append(kinds, c.kind)
	}
	assert.Equal(t, []string{"rights", "send", "deleteBusiness", "rights"}, kinds,
		"a failed delete drops the cached rights of the connection")
}

// assertBusinessShowsNothing runs a Business turn with two notes and checks
// that the rights lookup was the only Telegram call.
func assertBusinessShowsNothing(t *testing.T, rec *statusRecorder) {
	t.Helper()
	s := newTestStatusLine(t, rec, context.Background(), replyTarget{chatID: 555, businessConnectionID: "biz-1"})

	s.note("A")
	rec.waitCalls(t, 1)
	s.note("B")
	time.Sleep(5 * testStatusTiming)
	s.close()

	calls := rec.snapshot()
	require.Len(t, calls, 1, "only the rights lookup is made")
	assert.Equal(t, "rights", calls[0].kind)
}

func TestStatusLine_BusinessWithoutDeleteRightShowsNothing(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	rec.conn = &models.BusinessConnection{IsEnabled: true, Rights: &models.BusinessBotRights{CanReply: true}}
	assertBusinessShowsNothing(t, rec)
}

func TestStatusLine_BusinessDisabledConnectionShowsNothing(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	rec.conn = &models.BusinessConnection{IsEnabled: false, Rights: fullBusinessRights()}
	assertBusinessShowsNothing(t, rec)
}

func TestStatusLine_BusinessLookupFailureShowsNothing(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	rec.connErr = errors.New("Bad Request: business connection not found")
	assertBusinessShowsNothing(t, rec)
}

func TestBusinessNotesAllowed_CachesSuccessForTTL(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	rec := newStatusRecorder()
	rec.install(mockTg)
	ctx := context.Background()

	assert.True(t, b.businessNotesAllowed(ctx, "biz-1"))
	assert.True(t, b.businessNotesAllowed(ctx, "biz-1"))
	assert.Equal(t, 1, rec.count(), "a successful lookup is reused")

	b.clock.(*MockClock).Advance(11 * time.Minute)
	assert.True(t, b.businessNotesAllowed(ctx, "biz-1"))
	assert.Equal(t, 2, rec.count(), "the cached rights expire after businessRightsTTL")

	rec.set(func(r *statusRecorder) {
		r.conn = &models.BusinessConnection{IsEnabled: true, Rights: &models.BusinessBotRights{CanReply: true}}
	})
	assert.False(t, b.businessNotesAllowed(ctx, "biz-2"))
	assert.False(t, b.businessNotesAllowed(ctx, "biz-2"))
	assert.Equal(t, 3, rec.count(), "a successful lookup that denies notes is cached too")

	rec.set(func(r *statusRecorder) { r.connErr = errors.New("Bad Request: business connection not found") })
	assert.False(t, b.businessNotesAllowed(ctx, "biz-3"))
	assert.False(t, b.businessNotesAllowed(ctx, "biz-3"))
	assert.Equal(t, 5, rec.count(), "a failed lookup is not cached")
}

func TestStatusLine_NoteAfterCloseIsIgnored(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	s := newTestStatusLine(t, rec, context.Background(), testStatusTarget)

	s.close()
	s.note("late")
	s.answerSent()
	s.freeze()
	time.Sleep(5 * testStatusTiming)

	assert.Zero(t, rec.count(), "nothing reaches Telegram after close")
}

func TestStatusLine_CancelledTurnSendsNothing(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	turnCtx, cancel := context.WithCancel(context.Background())
	s := buildTestStatusLine(t, rec, turnCtx, testStatusTarget)

	// The turn is cancelled inside the first delay: the sender has not started yet.
	s.note("A")
	cancel()
	s.start()
	time.Sleep(5 * testStatusTiming)
	s.close()

	assert.Zero(t, rec.count(), "a note still waiting when the turn is cancelled is never shown")
}

func TestStatusLine_InFlightSendDeletedOnClose(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	rec.nextID = 900
	rec.holdSends()
	s := newTestStatusLine(t, rec, context.Background(), testStatusTarget)

	s.note("A")
	rec.waitCalls(t, 1) // the send is in flight until released
	closed := make(chan struct{})
	go func() {
		s.close()
		close(closed)
	}()
	require.Eventually(t, func() bool { return s.ctx.Err() != nil }, 5*time.Second, time.Millisecond,
		"close has begun")
	select {
	case <-closed:
		t.Fatal("close returned while the send was still in flight")
	default:
	}
	rec.release()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not return after the send finished")
	}

	calls := rec.snapshot()
	require.Len(t, calls, 2, "close waits for the send, then deletes what it created")
	assert.NoError(t, calls[0].ctxErr, "close never cancels a send in flight")
	require.Equal(t, "delete", calls[1].kind)
	assert.Equal(t, plainDelete(900), calls[1].params)
}

func TestStatusLine_AnswerSentDuringSendDeletesIt(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	rec := newStatusRecorder()
	rec.holdSends()
	s := newTestStatusLine(t, rec, context.Background(), testStatusTarget)

	s.note("A")
	rec.waitCalls(t, 1) // the send is in flight until released
	s.answerSent()
	rec.release()
	rec.waitCalls(t, 2)

	calls := rec.snapshot()
	require.Equal(t, "delete", calls[1].kind)
	assert.Equal(t, plainDelete(501), calls[1].params, "a status message that lands after the answer is removed at once")

	s.close()
	assert.Equal(t, 2, rec.count(), "nothing is left to delete on close")
}
