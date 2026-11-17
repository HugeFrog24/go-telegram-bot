package main

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func commandMessage(text string, entities ...models.MessageEntity) *models.Message {
	for i := range entities {
		entities[i].Type = "bot_command"
	}
	return &models.Message{Text: text, Entities: entities}
}

// Telegram counts entity offsets in UTF-16 code units. Out-of-range values,
// however large, never panic (a panic in a handler would take down every bot
// in the process) and never match.
func TestHandledCommand_OutOfRangeEntities(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, entity := range map[string]models.MessageEntity{
		"huge offset and length":         {Offset: 1 << 62, Length: 1 << 62},
		"huge length":                    {Offset: 1, Length: math.MaxInt},
		"largest offset":                 {Offset: math.MaxInt, Length: 1},
		"negative offset":                {Offset: -1, Length: 6},
		"negative length":                {Offset: 0, Length: -6},
		"zero length":                    {Offset: 0, Length: 0},
		"runs past the end":              {Offset: 1, Length: 6},
		"starts at the end":              {Offset: 6, Length: 1},
		"starts inside a surrogate pair": {Offset: 1, Length: 7},
		"ends inside a surrogate pair":   {Offset: 0, Length: 7},
	} {
		t.Run(name, func(t *testing.T) {
			text := map[string]string{
				"starts inside a surrogate pair": "😀/clear",
				"ends inside a surrogate pair":   "/clear😀",
			}[name]
			if text == "" {
				text = "/clear"
			}
			assert.NotPanics(t, func() {
				command, _ := handledCommand(commandMessage(text, entity), "")
				assert.Empty(t, command)
			})
		})
	}
}

// An overlapping, out-of-range entity before a good one is skipped.
func TestHandledCommand_OverlappingEntities(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	assert.NotPanics(t, func() {
		command, _ := handledCommand(commandMessage("/stats /clear",
			models.MessageEntity{Offset: 3, Length: 1 << 40},
			models.MessageEntity{Offset: 2, Length: 6},
			models.MessageEntity{Offset: 0, Length: 6},
		), "")
		assert.Equal(t, "/stats", command)
	})
}

// A command counts only at the start of the message. Elsewhere it is part of
// what the user is saying, in any script, and goes to the model as text.
func TestHandledCommand_OnlyAtTheStart(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, tc := range map[string]struct {
		text           string
		offset, length int
		want           string
	}{
		"Cyrillic before it":     {"привет /clear", 7, 6, ""},
		"emoji before it":        {"😀 /clear", 3, 6, ""},
		"a question about it":    {"what does /stats do?", 10, 6, ""},
		"emoji and text":         {"hi 👋🏽 /stats user", 8, 6, ""},
		"alone":                  {"/stats", 0, 6, "/stats"},
		"with an argument":       {"/clear 5", 0, 6, "/clear"},
		"with an emoji after it": {"/clear 😀", 0, 6, "/clear"},
	} {
		t.Run(name, func(t *testing.T) {
			command, _ := handledCommand(commandMessage(tc.text,
				models.MessageEntity{Offset: tc.offset, Length: tc.length}), "")
			assert.Equal(t, tc.want, command)
		})
	}
}

// Through the handler: a command mid-text reaches the model as text; one at
// the start is answered by the bot and never reaches it.
func TestHandleUpdate_CommandMidTextIsText(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	sent := recordSends(tg)
	user := &models.User{ID: 123, Username: "owner"}
	update := func(id int, text string, offset int) *models.Update {
		u := textUpdate(123, id, user, text)
		u.Message.Entities = []models.MessageEntity{{Type: "bot_command", Offset: offset, Length: 6}}
		return u
	}

	b.handleUpdate(context.Background(), nil, update(1300, "what does /stats do?", 10))
	b.handleUpdate(context.Background(), nil, update(1301, "привет /clear", 7))
	require.Len(t, stub.requestBetas(), 2, "both reach the model")
	_, text := stub.lastMessage(t, 0)
	assert.Equal(t, "what does /stats do?", text)
	_, text = stub.lastMessage(t, 1)
	assert.Equal(t, "привет /clear", text)

	b.handleUpdate(context.Background(), nil, update(1302, "/stats", 0))
	b.handleUpdate(context.Background(), nil, update(1303, "/clear 5", 0))
	assert.Len(t, stub.requestBetas(), 2, "commands at the start never reach the model")
	texts := sentTexts(sent)
	require.Len(t, texts, 4)
	assert.Contains(t, texts[2], "Bot Statistics")
	assert.Equal(t, "User with ID 5 not found.", texts[3], "/clear 5 acts on user 5")
}

// setBotUsername makes b's Telegram username name, as GetMe reports it at
// startup.
func setBotUsername(b *Bot, name string) { b.username = name }

// A command addressed to a bot by name counts only when the name is this
// bot's, in any case of the name: /stats@thisbot is /stats. One addressed to another bot
// is ignored: no reply, and nothing reaches the model.
func TestHandleUpdate_CommandAddressedToABot(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	sent := recordSends(tg)
	setBotUsername(b, "ThisBot")
	user := &models.User{ID: 123, Username: "owner"}
	msgID := 1400
	send := func(text string) []string {
		before := len(sent())
		msgID++
		command, _, _ := strings.Cut(text, " ")
		u := textUpdate(groupChatID, msgID, user, text)
		u.Message.Entities = []models.MessageEntity{{Type: "bot_command", Offset: 0, Length: len(command)}}
		b.handleUpdate(context.Background(), nil, u)
		return sentTexts(sent)[before:]
	}

	for _, text := range []string{"/stats@ThisBot", "/stats@thisbot", "/stats"} {
		replies := send(text)
		require.Len(t, replies, 1, text)
		assert.Contains(t, replies[0], "Bot Statistics", text)
	}
	assert.Empty(t, send("/stats@OtherBot"), "a command for another bot gets no reply")
	assert.Empty(t, send("/start@OtherBot hello"), "nor does any other")
	assert.Empty(t, stub.requestBetas(), "and none reaches the model")
}
