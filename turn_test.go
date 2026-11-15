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

	assert.False(t, b.stopTurn(5, 1), "no draft shown yet: nothing to stop")
	b.setTurnDraft(turn, "42")
	assert.False(t, b.stopTurn(5, 41), "a stale draft id is ignored")
	assert.NoError(t, ctx.Err())

	assert.True(t, b.stopTurn(5, 42))
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

	require.True(t, b.stopTurn(6, int(turn.seq)))

	assert.NotNil(t, b.finishTurn(turn), "a message sent before the Stop tap still deserves an answer")
}

func TestDiscardRerun_DropsCollectedFlush(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	turn, _, _ := b.beginTextTurn(textReq(8), 5)
	b.markCommitted(turn)
	_, _, ok := b.beginTextTurn(textReq(8), 7)
	require.False(t, ok)

	b.discardRerun(8)

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
