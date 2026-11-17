package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// steppingClock returns its times in turn, then the last one forever.
type steppingClock struct {
	mu    sync.Mutex
	times []time.Time
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.times[0]
	if len(c.times) > 1 {
		c.times = c.times[1:]
	}
	return now
}

// requireWallUTC checks a conversation stamp: wall-clock UTC, with no
// monotonic reading, so it compares, and the database sorts it, by the time
// the wall clock showed.
func requireWallUTC(t *testing.T, what string, stamp time.Time) {
	t.Helper()
	assert.NotContains(t, stamp.String(), "m=", "%s carries a monotonic reading: %v", what, stamp)
	assert.Equal(t, time.UTC, stamp.Location(), "%s is not UTC: %v", what, stamp)
}

// With the real clock, every join and reply stamp is wall-clock UTC without a
// monotonic reading. Go compares two times by their monotonic readings when
// both carry one; after the wall clock is set back (NTP, a resumed VM) that
// order contradicts the wall times the database sorts by.
func TestStamps_RealClockStampsAreWallClockUTC(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := historyTestBot(t, nil)
	b.clock = RealClock{}
	const chatID = int64(806)
	first := storeUserText(t, b, chatID, "first")
	turn, _, ok := b.beginTextTurn(textReq(chatID), first.Seq)
	require.True(t, ok)
	b.snapshotForTurn(turn, b.getOrCreateChatMemory(chatKey(chatID)))
	late := storeUserText(t, b, chatID, "arrived during the turn")
	reply, err := b.recordTurnReply(turn, chatKey(chatID), "a reply filed between them")
	require.NoError(t, err)
	b.finishTurn(turn)
	answer := storeUserText(t, b, chatID, "and then")
	answerInTurn(t, b, chatID, "a reply at the end")

	requireWallUTC(t, "a join", first.Timestamp)
	requireWallUTC(t, "a later join", late.Timestamp)
	requireWallUTC(t, "a reply between messages", reply.Timestamp)
	requireWallUTC(t, "another join", answer.Timestamp)
	var stored []Message
	require.NoError(t, b.db.Where("chat_id = ?", chatID).Order("id").Find(&stored).Error)
	var raw []string
	require.NoError(t, b.db.Raw("SELECT timestamp FROM messages WHERE chat_id = ? ORDER BY id", chatID).Scan(&raw).Error)
	for _, r := range raw {
		assert.True(t, strings.HasSuffix(r, "+00:00") || strings.HasSuffix(r, "Z"), "stored as UTC: %q", r)
	}
}

// The wall clock steps back an hour between two joins: the later join is
// still stamped after the earlier one, and a reload keeps their order.
func TestStamps_WallClockSetBackKeepsOrder(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := historyTestBot(t, nil)
	now := time.Now().Round(0)
	b.clock = &steppingClock{times: []time.Time{now, now.Add(-time.Hour)}}
	const chatID = int64(807)
	first := storeUserText(t, b, chatID, "before the step")
	second := storeUserText(t, b, chatID, "after the step")

	assert.True(t, second.Timestamp.After(first.Timestamp), "%v after %v", second.Timestamp, first.Timestamp)
	dropChatMemory(b, chatID)
	assert.Equal(t, []string{"before the step", "after the step"}, memoryTexts(b, chatID))
}
