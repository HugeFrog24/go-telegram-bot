package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const topicChatID int64 = -100500

const topicThreadID = 77

// recordSends captures every SendMessage the bot makes and returns a snapshot
// accessor, so assertions never race with the sending goroutines.
func recordSends(mockTg *MockTelegramClient) func() []bot.SendMessageParams {
	var mu sync.Mutex
	var sent []bot.SendMessageParams
	mockTg.SendMessageFunc = func(_ context.Context, p *bot.SendMessageParams) (*models.Message, error) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, *p)
		return &models.Message{}, nil
	}
	return func() []bot.SendMessageParams {
		mu.Lock()
		defer mu.Unlock()
		return append([]bot.SendMessageParams(nil), sent...)
	}
}

// recordChatActions is recordSends for SendChatAction.
func recordChatActions(mockTg *MockTelegramClient) func() []bot.SendChatActionParams {
	var mu sync.Mutex
	var actions []bot.SendChatActionParams
	mockTg.SendChatActionFunc = func(_ context.Context, p *bot.SendChatActionParams) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		actions = append(actions, *p)
		return true, nil
	}
	return func() []bot.SendChatActionParams {
		mu.Lock()
		defer mu.Unlock()
		return append([]bot.SendChatActionParams(nil), actions...)
	}
}

func whoamiInChat(chatID int64, inTopic bool, threadID int) *models.Update {
	return &models.Update{Message: &models.Message{
		ID:              5,
		Chat:            models.Chat{ID: chatID, Type: models.ChatTypeSupergroup},
		From:            &models.User{ID: 123, Username: "owner"},
		Text:            "/whoami",
		Entities:        []models.MessageEntity{{Type: "bot_command", Offset: 0, Length: 7}},
		IsTopicMessage:  inTopic,
		MessageThreadID: threadID,
	}}
}

func TestTargetFor(t *testing.T) {
	cases := []struct {
		name  string
		msg   *models.Message
		bizID string
		want  replyTarget
	}{
		{
			name: "topic message carries its thread",
			msg: &models.Message{
				Chat: models.Chat{ID: topicChatID}, IsTopicMessage: true, MessageThreadID: topicThreadID,
			},
			want: replyTarget{chatID: topicChatID, threadID: topicThreadID},
		},
		{
			name: "reply thread in a non-forum supergroup has no topic",
			msg: &models.Message{
				Chat: models.Chat{ID: topicChatID}, IsTopicMessage: false, MessageThreadID: 12,
			},
			want: replyTarget{chatID: topicChatID},
		},
		{
			name:  "private message keeps the business connection",
			msg:   &models.Message{Chat: models.Chat{ID: 42}},
			bizID: "biz-1",
			want:  replyTarget{chatID: 42, businessConnectionID: "biz-1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, targetFor(tc.msg, tc.bizID))
		})
	}
}

func TestTurnRequestTarget(t *testing.T) {
	req := turnRequest{chatID: topicChatID, businessConnectionID: "biz-2", threadID: topicThreadID}
	assert.Equal(t, replyTarget{chatID: topicChatID, businessConnectionID: "biz-2", threadID: topicThreadID}, req.target())
}

func TestSendResponse_TargetsTopic(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	sent := recordSends(mockTg)

	require.NoError(t, b.sendResponse(context.Background(), replyTarget{chatID: topicChatID, threadID: topicThreadID}, "hi"))

	got := sent()
	require.Len(t, got, 1)
	assert.Equal(t, topicChatID, got[0].ChatID)
	assert.Equal(t, topicThreadID, got[0].MessageThreadID)
	assert.Empty(t, got[0].BusinessConnectionID)
}

func TestStartChatAction_TargetsTopic(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	actions := recordChatActions(mockTg)

	stop := b.startChatAction(context.Background(), replyTarget{chatID: topicChatID, threadID: topicThreadID}, models.ChatActionTyping)
	stop()

	got := actions()
	require.Len(t, got, 1)
	assert.Equal(t, topicThreadID, got[0].MessageThreadID)
}

func TestDraftStream_TargetsTopic(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	rec := &draftRecorder{}
	mockTg.SendMessageDraftFunc = rec.send

	d := b.newDraftStream(context.Background(), replyTarget{chatID: 123, threadID: 9}, "7", nil)
	d.interval = testDraftInterval
	d.keepalive = time.Hour
	d.start()
	t.Cleanup(d.close)
	d.update("Hi")
	eventuallyCalls(t, rec, 1)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, 9, rec.calls[0].MessageThreadID)
}

func TestHandleUpdate_CommandInTopicRepliesInTopic(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	sent := recordSends(mockTg)

	b.handleUpdate(context.Background(), nil, whoamiInChat(topicChatID, true, topicThreadID))

	got := sent()
	require.NotEmpty(t, got)
	for _, p := range got {
		assert.Equal(t, topicThreadID, p.MessageThreadID)
		assert.Equal(t, topicChatID, p.ChatID)
	}
}

func TestHandleUpdate_MessageOutsideTopicSendsNoThreadID(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	sent := recordSends(mockTg)

	b.handleUpdate(context.Background(), nil, whoamiInChat(topicChatID, false, 12))

	got := sent()
	require.NotEmpty(t, got)
	for _, p := range got {
		assert.Zero(t, p.MessageThreadID, "a reply thread is not a forum topic")
	}
}

// Review Focus 4: an edit that restarts a turn must carry the edited message's
// topic, or the rerun answers in the wrong place.
func TestHandleEditedMessage_TopicKeepsThreadID(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	msg := storeUserMessage(t, b, topicChatID, 77, "how do I use libtik")
	turn, _, ok := b.beginTextTurn(textReq(topicChatID), msg.Seq)
	require.True(t, ok)

	b.handleUpdate(context.Background(), nil, &models.Update{EditedMessage: &models.Message{
		ID: 77, Chat: models.Chat{ID: topicChatID}, From: &models.User{ID: topicChatID, Username: "owner"},
		IsTopicMessage: true, MessageThreadID: 41, Text: "fixed",
	}})

	b.turnsMu.Lock()
	rerun := b.turns[topicChatID].rerun
	b.turnsMu.Unlock()
	require.NotNil(t, rerun, "the edit must restart the uncommitted turn")
	assert.Equal(t, 41, rerun.threadID)
	b.finishTurn(turn)
}

func TestRunTurn_SegmentsAndTypingTargetTopic(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	sent := recordSends(mockTg)
	actions := recordChatActions(mockTg)
	req := turnRequest{ctx: context.Background(), chatID: topicChatID, userID: 123, threadID: topicThreadID}
	turn, turnCtx, ok := b.beginTextTurn(req, 1)
	require.True(t, ok)

	b.runTurn(turn, turnCtx, req, func(_ context.Context, out replyOutput) (string, error) {
		return "hello", out.onSegment("hello")
	})

	require.Len(t, sent(), 1)
	assert.Equal(t, topicThreadID, sent()[0].MessageThreadID)
	require.NotEmpty(t, actions())
	assert.Equal(t, topicThreadID, actions()[0].MessageThreadID)
}

func TestRunTurn_ErrorReplyTargetsTopic(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	sent := recordSends(mockTg)
	req := turnRequest{ctx: context.Background(), chatID: topicChatID, userID: 555, threadID: topicThreadID}
	turn, turnCtx, ok := b.beginTextTurn(req, 1)
	require.True(t, ok)

	b.runTurn(turn, turnCtx, req, func(context.Context, replyOutput) (string, error) {
		return "", errors.New("boom")
	})

	require.Len(t, sent(), 1)
	assert.Equal(t, topicThreadID, sent()[0].MessageThreadID)
}

func TestRunTurn_DraftAndFinalMessageTargetTopic(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTg := setupBotForTest(t, 123)
	b.config.StreamDrafts = true
	sent := recordSends(mockTg)
	rec := &draftRecorder{}
	mockTg.SendMessageDraftFunc = rec.send
	// A private chat with forum topic mode: eligible for drafts, still threaded.
	req := turnRequest{ctx: context.Background(), chatID: 555, userID: 555, threadID: 9}
	turn, turnCtx, ok := b.beginTextTurn(req, 1)
	require.True(t, ok)

	b.runTurn(turn, turnCtx, req, func(_ context.Context, out replyOutput) (string, error) {
		out.onProgress("partial")
		eventuallyCalls(t, rec, 1)
		return "final", nil
	})

	rec.mu.Lock()
	assert.Equal(t, 9, rec.calls[0].MessageThreadID)
	rec.mu.Unlock()
	require.Len(t, sent(), 1)
	assert.Equal(t, 9, sent()[0].MessageThreadID)
}
