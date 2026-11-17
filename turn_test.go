package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func textReq(chatID int64) turnRequest {
	return turnRequest{ctx: context.Background(), chatID: chatID, userID: 555, username: "tester"}
}

func TestBeginTextTurn_RunsWhenIdle(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)

	turn, ctx, ok := b.beginTextTurn(textReq(1), 5)
	require.True(t, ok)
	require.NotNil(t, turn)
	assert.NoError(t, ctx.Err())

	assert.Nil(t, b.finishTurn(turn))
	_, _, ok = b.beginTextTurn(textReq(1), 6)
	assert.True(t, ok, "chat is free again after finish")
}

func TestBeginTextTurn_DropsFlushAlreadyCovered(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	turn, ctx, _ := b.beginTextTurn(textReq(2), 5)

	_, _, ok := b.beginTextTurn(textReq(2), 5)

	assert.False(t, ok)
	assert.NoError(t, ctx.Err(), "a covered flush must not cancel the running turn")
	assert.Nil(t, b.finishTurn(turn), "nothing to rerun")
}

func TestBeginTextTurn_RestartsUncommittedTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	turn, ctx, _ := b.beginTextTurn(textReq(3), 5)

	_, _, ok := b.beginTextTurn(textReq(3), 7)

	assert.False(t, ok)
	assert.ErrorIs(t, ctx.Err(), context.Canceled, "a newer message before any output cancels the turn")
	assert.Equal(t, cancelRestart, b.turnReason(turn))
	next := b.finishTurn(turn)
	require.NotNil(t, next, "the cancelled turn must be rerun")
	assert.Equal(t, int64(3), next.chatID)
}

func TestFinishTurn_RestartRerunsEvenIfSnapshotRacedIn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	turn, _, _ := b.beginTextTurn(textReq(30), 5)
	_, _, ok := b.beginTextTurn(textReq(30), 7)
	require.False(t, ok)

	// The cancelled turn's snapshot happened to include message 7, but it
	// delivered nothing, so the rerun must still happen.
	b.markCovered(turn, 7)

	assert.NotNil(t, b.finishTurn(turn), "a restart always reruns")
}

func TestBeginTextTurn_CollectsBehindCommittedTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	turn, ctx, _ := b.beginTextTurn(textReq(4), 5)
	b.markCommitted(turn)

	_, _, ok := b.beginTextTurn(textReq(4), 7)

	assert.False(t, ok)
	assert.NoError(t, ctx.Err(), "output already shown: never cancel")
	assert.Equal(t, cancelNone, b.turnReason(turn))
	require.NotNil(t, b.finishTurn(turn), "the collected flush runs afterwards")
}

func TestFinishTurn_DropsCollectedFlushTheTurnEndedUpCovering(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	// A media turn: committed from the start, coverage known only later.
	turn, _ := b.beginTurn(context.Background(), 40)
	_, _, ok := b.beginTextTurn(textReq(40), 7)
	require.False(t, ok, "collected behind the media turn")

	// The media turn's late snapshot includes message 7 and answers it.
	b.markCovered(turn, 7)

	assert.Nil(t, b.finishTurn(turn), "rerunning would answer message 7 twice")
}

func TestStopTurn_RequiresMatchingDraft(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	turn, ctx, _ := b.beginTextTurn(textReq(5), 5)

	assert.False(t, b.stopTurn(chatKey(5), 1), "no draft shown yet: nothing to stop")
	b.setTurnDraft(turn, "42")
	assert.False(t, b.stopTurn(chatKey(5), 41), "a stale draft id is ignored")
	assert.NoError(t, ctx.Err())

	assert.True(t, b.stopTurn(chatKey(5), 42))
	assert.ErrorIs(t, ctx.Err(), context.Canceled)
	assert.Equal(t, cancelStop, b.turnReason(turn))
	b.finishTurn(turn)
}

func TestFinishTurn_StopKeepsCollectedFlush(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	turn, _, _ := b.beginTextTurn(textReq(6), 5)
	b.markCommitted(turn)
	b.setTurnDraft(turn, turn.draftIDString())
	_, _, ok := b.beginTextTurn(textReq(6), 7)
	require.False(t, ok)

	require.True(t, b.stopTurn(chatKey(6), int(turn.seq)))

	assert.NotNil(t, b.finishTurn(turn), "a message sent before the Stop tap still deserves an answer")
}

// dropRerun drops the flush collected behind the chat's running turn, for a
// test about the turn alone.
func dropRerun(b *Bot, chatID int64) {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	if running, exists := b.turns[chatKey(chatID)]; exists {
		running.rerun, running.rerunWant = nil, 0
	}
}

func TestBeginClear_StopsTheTurnAndDropsItsRerun(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	turn, ctx, _ := b.beginTextTurn(textReq(8), 5)
	b.markCommitted(turn)
	_, _, ok := b.beginTextTurn(textReq(8), 7)
	require.False(t, ok)

	_, err := b.beginClear(chatKey(8))
	require.NoError(t, err)

	assert.ErrorIs(t, ctx.Err(), context.Canceled, "the turn stops, committed or not")
	assert.True(t, b.clearedTurn(turn))
	assert.Nil(t, b.finishTurn(turn), "/clear must not let the collected flush rerun on an empty chat")
}

func TestRestartForEdit(t *testing.T) {
	b, _ := setupBotForTest(t, 123)

	t.Run("uncommitted turn reading the edited message restarts", func(t *testing.T) {
		turn, ctx, _ := b.beginTextTurn(textReq(9), 5)
		assert.True(t, b.restartForEdit(textReq(9), 5))
		assert.ErrorIs(t, ctx.Err(), context.Canceled)
		assert.Equal(t, cancelRestart, b.turnReason(turn))
		assert.NotNil(t, b.finishTurn(turn))
	})

	t.Run("committed turn keeps going", func(t *testing.T) {
		turn, ctx, _ := b.beginTextTurn(textReq(10), 5)
		b.markCommitted(turn)
		assert.False(t, b.restartForEdit(textReq(10), 5))
		assert.NoError(t, ctx.Err())
		b.finishTurn(turn)
	})

	t.Run("turn that never read the message is left alone", func(t *testing.T) {
		turn, ctx, _ := b.beginTextTurn(textReq(11), 3)
		assert.False(t, b.restartForEdit(textReq(11), 5))
		assert.NoError(t, ctx.Err())
		b.finishTurn(turn)
	})

	t.Run("no running turn", func(t *testing.T) {
		assert.False(t, b.restartForEdit(textReq(12), 5))
	})
}

func TestBeginTurn_WaitsForRunningTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	running, _, _ := b.beginTextTurn(textReq(7), 5)

	acquired := make(chan *chatTurn, 1)
	go func() {
		turn, _ := b.beginTurn(context.Background(), 7)
		acquired <- turn
	}()

	select {
	case <-acquired:
		t.Fatal("a media turn must wait for the running turn")
	case <-time.After(50 * time.Millisecond):
	}

	b.finishTurn(running)

	select {
	case turn := <-acquired:
		assert.True(t, turn.committed, "media turns are committed from the start")
		b.finishTurn(turn)
	case <-time.After(time.Second):
		t.Fatal("the media turn never acquired the chat")
	}
}

func TestBeginTurn_CancelledParentStillWaits(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	running, _, _ := b.beginTextTurn(textReq(13), 5)
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	acquired := make(chan struct{})
	go func() {
		turn, _ := b.beginTurn(parent, 13)
		b.finishTurn(turn)
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("a cancelled parent must not register over the running turn")
	case <-time.After(50 * time.Millisecond):
	}
	b.finishTurn(running)
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("never acquired after the running turn finished")
	}
}

// beginTurn takes the chat for a committed turn the way a media turn does
// (beginMediaTurn): someone else's media turn, for tests.
func (b *Bot) beginTurn(parent context.Context, chatID int64) (*chatTurn, context.Context) {
	return b.beginTurnIn(parent, chatKey(chatID))
}

// beginTurnIn is beginTurn in conversation k.
func (b *Bot) beginTurnIn(parent context.Context, k convKey) (*chatTurn, context.Context) {
	to := replyTarget{chatID: k.chatID, businessConnectionID: k.businessConnectionID, threadID: k.threadID, forum: k.threadID != 0}
	m, _ := b.beginMediaTurn(parent, to, 0, 0)
	return m.turn, m.ctx
}

// forumTopic is topic thread's conversation in the test forum.
func forumTopic(thread int) convKey { return convKey{chatID: forumChat, threadID: thread} }

// beginTextTurn is beginTextTurnLocked taking the lock, for tests that pick
// the flush's want themselves.
func (b *Bot) beginTextTurn(req turnRequest, wantSeq uint64) (*chatTurn, context.Context, bool) {
	chatMemory := b.getOrCreateChatMemory(req.conv())
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	return b.beginTextTurnLocked(req, wantSeq, chatMemory)
}

// A turn restarted (here by an edit) whose model call finishes anyway before
// it notices is abandoned all the same: nothing of it is sent or filed, and
// its rerun answers.
func TestRunTurn_ARestartedTurnWhoseCallFinishedFirstIsAbandoned(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	sent := recordSends(tg)
	const chatID = int64(-100800) // a group: segments, no draft
	first := storeUserText(t, b, chatID, "hi")
	turn, turnCtx, ok := b.beginTextTurn(textReq(chatID), first.Seq)
	require.True(t, ok)
	called, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.runTurn(turn, turnCtx, textReq(chatID), func(context.Context, replyOutput) (string, error) {
			close(called)
			<-release // the stream ends just as the restart lands
			return "a reply to the old wording", nil
		})
	}()
	<-called

	require.True(t, b.restartForEdit(textReq(chatID), first.Seq))
	close(release)
	<-done
	waitIdle(t, b, chatID)

	assert.NotContains(t, memoryTexts(b, chatID), "a reply to the old wording", "nothing of it is filed")
	assert.Len(t, stub.requestBetas(), 1, "its rerun asked the model")
	assert.Equal(t, []string{"Open the Farm tab."}, sentTexts(sent), "and only the rerun's answer is sent")
}
