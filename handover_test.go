package main

import (
	"context"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// handoverTurn registers a committed turn on chatID, with a flush that wants
// want collected behind it.
func handoverTurn(t *testing.T, b *Bot, chatID int64, want uint64) *chatTurn {
	t.Helper()
	turn, _, ok := b.beginTextTurn(textReq(chatID), 1)
	require.True(t, ok)
	b.markCommitted(turn)
	_, _, ok = b.beginTextTurn(textReq(chatID), want)
	require.False(t, ok, "collected behind the committed turn")
	return turn
}

func heldTurn(b *Bot, chatID int64) *chatTurn {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	return b.turns[chatID]
}

// closeTurn with handOver registers the collected flush's turn in the same
// step that unregisters the ending one: the chat is never free in between.
func TestCloseTurn_HandsTheChatToItsCollectedFlush(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	const chatID = int64(820)
	turn := handoverTurn(t, b, chatID, 7)

	rerun, next, nextCtx := b.closeTurn(turn, true)

	require.NotNil(t, next)
	assert.Same(t, next, heldTurn(b, chatID), "the chat passes to the flush's turn")
	assert.NotNil(t, rerun)
	assert.NoError(t, nextCtx.Err())
	b.finishTurn(next)
}

// A collected flush whose message a recorded reply already covered gets no
// turn: the chat is left free.
func TestCloseTurn_NoHandoverForAnAnsweredFlush(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	const chatID = int64(821)
	turn := handoverTurn(t, b, chatID, 7)
	b.getOrCreateChatMemory(chatID).answeredThrough.Store(7) // a reply covered it meanwhile

	_, next, _ := b.closeTurn(turn, true)

	assert.Nil(t, next)
	assert.Nil(t, heldTurn(b, chatID), "the chat is free")
}

// endTurn has handed the chat over by the time it returns: the next turn is
// registered on the ending turn's goroutine, not later on its own. Memory
// holds nothing to answer here, so only the handover can register a turn.
func TestEndTurn_RegistersTheCollectedFlushBeforeReturning(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	recordSends(tg)
	const chatID = int64(822)
	release := make(chan struct{})
	tg.SendChatActionFunc = func(context.Context, *bot.SendChatActionParams) (bool, error) {
		<-release // holds the handed-over turn in its first typing action
		return true, nil
	}
	turn := handoverTurn(t, b, chatID, 7)

	b.endTurn(turn)

	next := heldTurn(b, chatID)
	require.NotNil(t, next, "the collected flush holds the chat")
	assert.NotSame(t, turn, next)
	b.cancelForClear(chatID) // let it end silently
	close(release)
	waitIdle(t, b, chatID)
}
