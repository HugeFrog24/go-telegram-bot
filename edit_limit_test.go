package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// groupEdit is an edit of message id in the test group, from user.
func groupEdit(id int, user *models.User, text string) *models.Update {
	return &models.Update{EditedMessage: &models.Message{
		ID: id, Chat: models.Chat{ID: groupChatID, Type: models.ChatTypeSupergroup}, From: user, Text: text,
	}}
}

// startHeldTurn sends text from user in the test group and waits until its
// turn's model request has started, held by the gated stub.
func startHeldTurn(t *testing.T, b *Bot, stub *anthropicStub, id int, user *models.User, text string) chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, id, user, text))
	}()
	require.Eventually(t, func() bool { return len(stub.requestBetas()) == 1 }, 5*time.Second, 5*time.Millisecond)
	return done
}

// Blind P2 inverted (B6): a rate-limited sender's edits only correct the
// stored text: none restarts the turn reading it, so no edit costs a model
// request.
func TestEdit_ARateLimitedSendersEditsRestartNothing(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, candleFarmStream(), 1) // message_start only, then waits
	b, _ := historyTestBot(t, stub)
	b.config.MessagePerHour, b.config.MessagePerDay = 1, 1
	user := &models.User{ID: 5151, Username: "editor"}
	done := startHeldTurn(t, b, stub, 20, user, "hello")
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 21, user, "blocked")) // rate-limited from here on

	for i := 0; i < 15; i++ {
		b.handleUpdate(context.Background(), nil, groupEdit(20, user, "hello"+strings.Repeat("!", i+1)))
	}
	time.Sleep(100 * time.Millisecond)
	assert.Len(t, stub.requestBetas(), 1, "no edit restarted the turn")
	assert.Equal(t, "hello"+strings.Repeat("!", 15), storedRow(t, b, groupChatID, "hello"+strings.Repeat("!", 15)).Text,
		"the stored text is corrected all the same")
	open()
	<-done
	waitIdle(t, b, groupChatID)
	assert.Len(t, stub.requestBetas(), 1)
}

// Blind P2 inverted (B6): edits restart a given turn at most once. Fifteen
// edits from a sender within their limits cost one restart; the later edits
// only correct the record.
func TestEdit_RestartsATurnAtMostOnce(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, candleFarmStream(), 1)
	b, _ := historyTestBot(t, stub)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 1000
	user := &models.User{ID: 5152, Username: "editor"}
	done := startHeldTurn(t, b, stub, 30, user, "hello")

	b.handleUpdate(context.Background(), nil, groupEdit(30, user, "hello!"))
	require.Eventually(t, func() bool { return len(stub.requestBetas()) == 2 }, 5*time.Second, 5*time.Millisecond,
		"the first edit restarts the turn")
	for i := 0; i < 14; i++ {
		b.handleUpdate(context.Background(), nil, groupEdit(30, user, "hello!"+strings.Repeat("?", i+1)))
	}
	time.Sleep(100 * time.Millisecond)
	assert.Len(t, stub.requestBetas(), 2, "the later edits restart nothing")
	open()
	<-done
	waitIdle(t, b, groupChatID)
	assert.Len(t, stub.requestBetas(), 2, "nor run after it: the turn read the message")
}

// Blind P3 inverted (B6): edits re-arm a batch's quiet window, but never past
// four windows from the batch's first message: a steady stream of edits
// cannot keep a waiting question from being answered.
func TestEdit_CannotHoldABatchPastFourWindows(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 1000
	b.config.DebounceMs = 300
	victim := &models.User{ID: 6161, Username: "victim"}
	griefer := &models.User{ID: 6262, Username: "griefer"}

	start := time.Now()
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 30, griefer, "hi"))
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 31, victim, "please answer me"))
	var answeredAfter time.Duration
	for i := 0; time.Since(start) < 2500*time.Millisecond; i++ {
		b.handleUpdate(context.Background(), nil, groupEdit(30, griefer, "hi"+strings.Repeat(".", i%5+1)))
		if answeredAfter == 0 && len(stub.requestBetas()) > 0 {
			answeredAfter = time.Since(start)
		}
		time.Sleep(200 * time.Millisecond)
	}
	waitIdle(t, b, groupChatID)
	require.NotEmpty(t, stub.requestBetas(), "the batch flushed while the edits went on")
	assert.Less(t, answeredAfter, 2*time.Second, "within four windows (1.2 s) of the first message, give or take an edit")
	assert.True(t, requestHas(t, stub, 0, "please answer me"))
}

// B6: a rate-limited sender's edit does not re-arm the quiet window either.
func TestEdit_ARateLimitedSendersEditDoesNotReArmTheWindow(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	b.config.MessagePerHour, b.config.MessagePerDay = 1, 1
	b.config.DebounceMs = 300
	griefer := &models.User{ID: 6263, Username: "griefer"}
	victim := &models.User{ID: 6162, Username: "victim"}
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 40, griefer, "hi"))
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 41, griefer, "over the limit"))
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 42, victim, "please answer me"))

	start := time.Now()
	for time.Since(start) < 900*time.Millisecond {
		b.handleUpdate(context.Background(), nil, groupEdit(40, griefer, "hi?"))
		time.Sleep(100 * time.Millisecond)
	}
	assert.NotEmpty(t, stub.requestBetas(), "the window ran out: the limited edits did not re-arm it")
	waitIdle(t, b, groupChatID)
}

// B6: an edit that re-arms nothing and restarts nothing (here of a message
// left unanswered, with no batch and no turn on it) only corrects the record,
// and costs its sender nothing of their rate limit.
func TestEdit_ThatActsOnNothingCostsNoRateLimit(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	b.config.MessagePerHour, b.config.MessagePerDay = 1, 1
	user := &models.User{ID: 5153, Username: "editor"}
	m := storeTextFrom(t, b, groupChatID, user, "left unanswered")
	require.NoError(t, b.db.Model(&Message{}).Where("id = ?", m.ID).Update("telegram_message_id", 50).Error)
	b.chatMemoriesMu.Lock()
	for i, held := range b.chatMemories[chatKey(groupChatID)].Messages {
		if held.ID == m.ID {
			b.chatMemories[chatKey(groupChatID)].Messages[i].TelegramMessageID = 50
		}
	}
	b.chatMemoriesMu.Unlock()

	for i := 0; i < 3; i++ {
		b.handleUpdate(context.Background(), nil, groupEdit(50, user, "left unanswered"+strings.Repeat("!", i+1)))
	}
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 51, user, "a real question"))
	waitIdle(t, b, groupChatID)

	require.Len(t, stub.requestBetas(), 1, "the message after the edits is within the limit")
	assert.True(t, requestHas(t, stub, 0, "left unanswered!!!"), "and the edits corrected the record")
}
