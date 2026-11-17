package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// A role without history:clear:own (or history:clear_hard:own) cannot clear
// at all: not in a private chat, not in a group.
func TestClear_RoleWithoutTheOwnScopeIsRefused(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	const refused = "Permission denied. Your role can't clear chat history."
	b, tg := setupBotForTest(t, 123)
	sent := recordSends(tg)
	require.NoError(t, b.db.Create(&Role{Name: "guest"}).Error)
	guest := createUserWithRole(t, b, 701, "guest", "guest")
	storeTextFrom(t, b, guest.ID, guest, "my secret")
	storeTextFrom(t, b, groupChatID, guest, "my group secret")

	for i, u := range []*models.Update{
		privateCommand(guest, 71, "/clear"),
		privateCommand(guest, 72, "/clear_hard"),
		commandUpdate(groupChatID, 73, guest, "/clear"),
		commandUpdate(groupChatID, 74, guest, "/clear_hard"),
	} {
		b.handleUpdate(context.Background(), nil, u)
		require.Len(t, sent(), i+1)
		assert.Equal(t, refused, sent()[i].Text, u.Message.Text)
	}
	assert.Contains(t, liveTexts(t, b, guest.ID, guest.ID), "my secret", "nothing is cleared")
	assert.Contains(t, liveTexts(t, b, groupChatID, guest.ID), "my group secret")
	assert.Contains(t, memoryTexts(b, guest.ID), "my secret")
	assert.Contains(t, memoryTexts(b, groupChatID), "my group secret")
}

// The admin form /clear <user_id> <chat_id> deletes only that user's rows in
// that chat: the group's other members and the bot's replies stay, in the
// database and in the group's memory.
func TestClear_AdminFormInAGroupDeletesOnlyTheTargetsRows(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	sent := recordSends(tg)
	admin := createUserWithRole(t, b, 203, "moderator", "admin")
	storeTextFrom(t, b, groupChatID, mallory, "my number is 555-0199")
	answerInTurn(t, b, groupChatID, "Noted.")
	storeTextFrom(t, b, groupChatID, alice, "where is the farm tab?")
	createUserWithRole(t, b, mallory.ID, mallory.Username, "user")

	b.handleUpdate(context.Background(), nil,
		privateCommand(admin, 75, fmt.Sprintf("/clear %d %d", mallory.ID, groupChatID)))

	require.NotEmpty(t, sent())
	assert.Equal(t, "Chat history for user @mallory (ID: 202) has been cleared.", sent()[len(sent())-1].Text)
	assert.Empty(t, liveTexts(t, b, groupChatID, mallory.ID))
	assert.Equal(t, []string{"where is the farm tab?"}, liveTexts(t, b, groupChatID, alice.ID), "the others' rows stay")
	assert.Contains(t, liveTexts(t, b, groupChatID, 0), "Noted.", "the bot's replies stay")
	assert.Equal(t, []string{"Noted.", "where is the farm tab?"}, memoryTexts(b, groupChatID))
}

// /clear <user_id> deletes that user's rows in every chat, and every cached
// conversation that held them lets them go, not only the user's private chat.
func TestClear_AllChatsFormDropsTheTargetFromEveryCachedMemory(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	const otherGroup = int64(-100600)
	b, tg := setupBotForTest(t, 123)
	sent := recordSends(tg)
	admin := createUserWithRole(t, b, 203, "moderator", "admin")
	createUserWithRole(t, b, mallory.ID, mallory.Username, "user")
	for _, chatID := range []int64{groupChatID, otherGroup} {
		storeTextFrom(t, b, chatID, mallory, "my number is 555-0199")
		storeTextFrom(t, b, chatID, alice, "where is the farm tab?")
	}
	storeTextFrom(t, b, mallory.ID, mallory, "my number is 555-0199") // her private chat
	answerInTurn(t, b, mallory.ID, "Noted.")

	b.handleUpdate(context.Background(), nil, privateCommand(admin, 76, fmt.Sprintf("/clear %d", mallory.ID)))

	require.NotEmpty(t, sent())
	assert.Equal(t, "Chat history for user @mallory (ID: 202) has been cleared.", sent()[len(sent())-1].Text)
	for _, chatID := range []int64{groupChatID, otherGroup} {
		assert.Empty(t, liveTexts(t, b, chatID, mallory.ID), "chat %d", chatID)
		assert.Equal(t, []string{"where is the farm tab?"}, liveTexts(t, b, chatID, alice.ID), "chat %d", chatID)
		assert.Equal(t, []string{"where is the farm tab?"}, memoryTexts(b, chatID), "chat %d's cached memory", chatID)
	}
	assert.Empty(t, liveTexts(t, b, mallory.ID, mallory.ID))
	assert.Empty(t, liveTexts(t, b, mallory.ID, 0), "her private chat is cleared whole, the bot's replies too")
	assert.Empty(t, memoryTexts(b, mallory.ID))
}

// A conversation first loaded while an admin's clear of one user is still
// deleting that user's rows there (they are still in the database) does not
// hold them: the all-chats cutoff skips them in the load (scoped r5 P11), and
// the one-chat form loads the chat's memory before its cutoff, so the load
// finds it loaded already (N31).
func TestClear_AdminFormMemoryLoadedDuringTheDeleteSkipsTheTarget(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	const coldGroup = int64(-100700)
	for name, tc := range map[string]struct {
		form    string
		deletes int32 // the user's rows' delete is the last: the all-chats form clears their private chat first
	}{
		"all chats": {fmt.Sprintf("/clear %d", mallory.ID), 2},
		"that chat": {fmt.Sprintf("/clear %d %d", mallory.ID, coldGroup), 1},
	} {
		t.Run(name, func(t *testing.T) {
			b, tg := setupBotForTest(t, 123)
			oneConnection(t, b)
			recordSends(tg)
			admin := createUserWithRole(t, b, 203, "moderator", "admin")
			createUserWithRole(t, b, mallory.ID, mallory.Username, "user")
			for _, m := range []Message{
				b.createMessage(chatKey(coldGroup), mallory.ID, mallory.Username, "user", "my number is 555-0199", true),
				b.createMessage(chatKey(coldGroup), alice.ID, alice.Username, "user", "where is the farm tab?", true),
			} {
				require.NoError(t, b.storeMessage(&m)) // stored, but the chat's memory is not loaded
			}
			var deletes atomic.Int32
			// Before the delete's transaction takes the one connection the load needs.
			require.NoError(t, b.db.Callback().Delete().Before("gorm:begin_transaction").Register("test:cold_load", func(*gorm.DB) {
				if deletes.Add(1) == tc.deletes {
					b.getOrCreateChatMemory(chatKey(coldGroup))
				}
			}))

			b.handleUpdate(context.Background(), nil, privateCommand(admin, 77, tc.form))

			require.Equal(t, tc.deletes, deletes.Load(), "the load ran during the user's rows' delete")
			assert.Empty(t, liveTexts(t, b, coldGroup, mallory.ID))
			assert.Equal(t, []string{"where is the farm tab?"}, memoryTexts(b, coldGroup))
		})
	}
}

// Blind r3 #2 inverted (P2): an admin's /clear_hard <user> clears the
// target's private chat whole, the bot's replies included: nothing of the
// deleted card number comes back into the model's context.
func TestClear_AdminFormClearsTheTargetsPrivateChatWhole(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, form := range map[string]string{"all chats": "/clear_hard %d", "that chat": "/clear_hard %d %d"} {
		t.Run(name, func(t *testing.T) {
			stub := newAnthropicStub(t, candleFarmStream())
			b, tg := setupBotForTest(t, 123)
			useAnthropicStub(b, stub)
			recordSends(tg)
			admin := createUserWithRole(t, b, 203, "moderator", "admin")
			createUserWithRole(t, b, mallory.ID, mallory.Username, "user")
			storeTextFrom(t, b, mallory.ID, mallory, "my card number is 4111-1111-1111-1111")
			answerInTurn(t, b, mallory.ID, "Saved your card 4111-1111-1111-1111.")
			command := fmt.Sprintf(form, mallory.ID)
			if name == "that chat" {
				command = fmt.Sprintf(form, mallory.ID, mallory.ID)
			}

			b.handleUpdate(context.Background(), nil, privateCommand(admin, 9300, command))

			assert.Empty(t, liveTexts(t, b, mallory.ID, mallory.ID), "her rows are gone")
			assert.Empty(t, liveTexts(t, b, mallory.ID, 0), "and so are the bot's")
			assert.Empty(t, memoryTexts(b, mallory.ID))
			u := textUpdate(mallory.ID, 9301, mallory, "hi again")
			u.Message.Chat.Type = models.ChatTypePrivate
			b.handleUpdate(context.Background(), nil, u)
			waitIdle(t, b, mallory.ID)
			require.Len(t, stub.requestBetas(), 1)
			assert.NotContains(t, string(stub.body(t, 0)), "4111", "the deleted number is not back in the model's context")
		})
	}
}
