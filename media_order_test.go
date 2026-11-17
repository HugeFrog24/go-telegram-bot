package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const forumChat = int64(-100900)

// requestMessages is the role and text of every message in a request body.
func requestMessages(t *testing.T, body []byte) []struct{ Role, Text string } {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(body, &req))
	out := make([]struct{ Role, Text string }, 0, len(req.Messages))
	for _, m := range req.Messages {
		var sb strings.Builder
		for _, c := range m.Content {
			sb.WriteString(c.Text)
		}
		out = append(out, struct{ Role, Text string }{m.Role, sb.String()})
	}
	return out
}

// inTopic puts u's message in forum topic thread.
func inTopic(u *models.Update, thread int) *models.Update {
	u.Message.Chat = models.Chat{ID: forumChat, Type: models.ChatTypeSupergroup, IsForum: true}
	u.Message.IsTopicMessage, u.Message.MessageThreadID = true, thread
	return u
}

// A asks in topic 1; while A's question waits in its quiet window, the owner
// sends media in the same topic. A's question is answered first, by a request
// of its own (A's user context, no emoji rule); the media message gets its
// own request, holding no unanswered text it did not bring.
func TestHandleUpdate_MediaNeverTakesOverAnotherPersonsWaitingText(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, tc := range map[string]struct {
		media func(t *testing.T, b *Bot, tg *MockTelegramClient) *models.Update
		last  string // the text the media turn's request ends with
	}{
		"sticker": {
			media: func(*testing.T, *Bot, *MockTelegramClient) *models.Update {
				return &models.Update{Message: &models.Message{ID: 981, From: owner, Sticker: &models.Sticker{FileID: "s", Emoji: "😀"}}}
			},
			last: "Sent a sticker: 😀",
		},
		"photo": {
			media: func(t *testing.T, _ *Bot, tg *MockTelegramClient) *models.Update {
				serveTelegramFiles(t, tg, nil)
				return &models.Update{Message: &models.Message{ID: 982, From: owner,
					Photo: []models.PhotoSize{{FileID: "p", Width: 1, Height: 1}}}}
			},
			last: "", // the image alone
		},
		"voice": {
			media: func(t *testing.T, b *Bot, tg *MockTelegramClient) *models.Update {
				(&voiceStubs{text: transcript}).install(t, b, tg)
				return &models.Update{Message: &models.Message{ID: 983, From: owner, Voice: &models.Voice{FileID: "v"}}}
			},
			last: transcript,
		},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newAnthropicStub(t, candleFarmStream())
			b, tg := setupBotForTest(t, ownerChat)
			useAnthropicStub(b, stub)
			oneConnection(t, b)
			sent := recordSends(tg)
			b.config.DebounceMs = int(bufferOnly / time.Millisecond)
			b.config.SystemPrompts = map[string]string{"custom_instructions": "be helpful", "respond_with_emojis": "EMOJI_ONLY_RULE"}
			asker := &models.User{ID: 4141, Username: "asker"}

			b.handleUpdate(context.Background(), nil, inTopic(textUpdate(forumChat, 980, asker, "what time does the store open?"), 1))
			require.True(t, b.hasPendingIntake(forumTopic(1)))
			b.handleUpdate(context.Background(), nil, inTopic(tc.media(t, b, tg), 1))
			waitIdle(t, b, forumChat)

			require.Len(t, stub.requestBetas(), 2, "one request each")
			asked := string(stub.body(t, 0))
			role, text := stub.lastMessage(t, 0)
			assert.Equal(t, "user", role)
			assert.Equal(t, "what time does the store open?", text, "the question is answered by its own request")
			assert.Contains(t, asked, "@asker", "with the asker's user context")
			assert.NotContains(t, asked, "EMOJI_ONLY_RULE", "and no emoji rule")
			msgs := requestMessages(t, stub.body(t, 1))
			require.GreaterOrEqual(t, len(msgs), 2)
			assert.Equal(t, "user", msgs[len(msgs)-1].Role)
			assert.Equal(t, tc.last, msgs[len(msgs)-1].Text, "the media request ends with the media")
			assert.Equal(t, "assistant", msgs[len(msgs)-2].Role, "and holds no unanswered text it did not bring")
			if name == "photo" {
				assert.Contains(t, string(stub.body(t, 1)), "file_stub")
			}
			require.Len(t, sent(), 2)
			assert.Equal(t, []int{1, 1}, threadsOf(sent), "both answers go to the topic")
			assert.NotNil(t, storedRow(t, b, forumChat, "what time does the store open?").AnsweredOn)
		})
	}
}

// Another member's text batch waits while a committed turn runs, and a
// sticker arrives. The sticker runs the batch ahead of itself: it is collected
// behind the running turn, and when that turn ends it hands the chat to the
// batch, ahead of the waiting sticker: the batch gets its own request, then
// the sticker gets one holding no text it did not bring.
func TestEndTurn_HandsTheChatToCollectedTextBeforeWaitingMedia(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, splitAnswer(), 4) // message_start and the first text block
	b, tg := setupBotForTest(t, ownerChat)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	recordSends(tg)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	storeTextFrom(t, b, groupChatID, alice, "tell me about the farm")
	_, done := startCommittedTurn(t, b, groupChatID)
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 990, mallory, "and the bakery?"))
	require.True(t, b.hasPendingIntake(chatKey(groupChatID)))

	stickerDone := make(chan struct{})
	go func() {
		defer close(stickerDone)
		b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
			ID: 991, Chat: models.Chat{ID: groupChatID, Type: models.ChatTypeSupergroup}, From: owner,
			Sticker: &models.Sticker{FileID: "s", Emoji: "😀"},
		}})
	}()
	waitForMediaWaiter(t, b, groupChatID)
	b.turnsMu.Lock()
	collected := b.turns[chatKey(groupChatID)].rerun != nil
	b.turnsMu.Unlock()
	require.True(t, collected, "the batch is collected behind the running turn")
	open()
	<-done
	<-stickerDone
	waitIdle(t, b, groupChatID)

	require.Len(t, stub.requestBetas(), 3)
	_, text := stub.lastMessage(t, 1)
	assert.Equal(t, "and the bakery?", text, "the collected batch goes before the sticker")
	msgs := requestMessages(t, stub.body(t, 2))
	require.GreaterOrEqual(t, len(msgs), 2)
	assert.Equal(t, "Sent a sticker: 😀", msgs[len(msgs)-1].Text)
	assert.Equal(t, "assistant", msgs[len(msgs)-2].Role, "the sticker request holds no text it did not bring")
}
