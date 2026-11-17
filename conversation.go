package main

import (
	"fmt"

	"github.com/go-telegram/bot/models"
)

// convKey names one conversation: the chat, the Business connection it came
// through ("" outside Business), and the forum topic (0 outside the topics of
// a forum supergroup). Telegram gives a Business chat the customer's own ID,
// so without the connection a customer's chats with two businesses and their
// private chat with the bot would be one conversation, with one memory, one
// batch, one turn and one set of rows. Every chat-scoped map, query and lookup
// goes by it.
//
// The topic follows the user decision that each forum topic is its own
// conversation. Telegram also sends message_thread_id on reply threads in a
// plain supergroup, and on topics in a private chat with the bot; neither
// splits a conversation (targetFor sets replyTarget.forum only for a topic
// message in a supergroup), so in Business and private chats it is 0.
type convKey struct {
	businessConnectionID string
	chatID               int64
	threadID             int
}

// String is the key as logs show it: the chat ID, with the topic and the
// connection when there are any.
func (k convKey) String() string {
	s := fmt.Sprint(k.chatID)
	if k.threadID != 0 {
		s += fmt.Sprintf(" topic %d", k.threadID)
	}
	if k.businessConnectionID != "" {
		s += fmt.Sprintf(" via business connection %s", k.businessConnectionID)
	}
	return s
}

// conv is the conversation the target's messages belong to.
func (t replyTarget) conv() convKey {
	k := convKey{businessConnectionID: t.businessConnectionID, chatID: t.chatID}
	if t.forum {
		k.threadID = t.threadID
	}
	return k
}

// conv is the conversation the request's message belongs to.
func (r turnRequest) conv() convKey { return r.target().conv() }

// conv is the conversation a stored message belongs to.
func (m Message) conv() convKey {
	return convKey{businessConnectionID: m.BusinessConnectionID, chatID: m.ChatID, threadID: m.ThreadID}
}

// inConv puts m in conversation k: its chat, connection and topic.
func (m *Message) inConv(k convKey) {
	m.ChatID, m.BusinessConnectionID, m.ThreadID = k.chatID, k.businessConnectionID, k.threadID
}

// convScope is the database scope of one conversation's rows. A row stored
// before rows carried their connection has a NULL business_connection_id and
// thread_id, which no "= ?" matches: those rows stay in the database (and in
// /stats) but no conversation loads, answers, edits or clears them, so every
// conversation started afresh when the connection was first stored (user
// decision B2).
func (b *Bot) convScope(k convKey) (string, []any) {
	return "bot_id = ? AND business_connection_id = ? AND chat_id = ? AND thread_id = ?",
		[]any{b.botID, k.businessConnectionID, k.chatID, k.threadID}
}

// albumKey is one album being buffered: its conversation and Telegram's
// media_group_id, which is unique only inside a chat (external review EXT-1).
type albumKey struct {
	conv         convKey
	mediaGroupID string
}

// profile is who sent a message, as the bot tells senders apart, with what
// the model is told of them.
type profile struct {
	id                                          int64
	username, firstName, lastName, languageCode string
	isPremium                                   bool
	// anonymous: an anonymous admin of the group, whom Telegram sends as the
	// group itself, as it does every other anonymous admin.
	anonymous bool
}

// senderOf is the sender of msg (B11). A message sent on behalf of a chat
// (sender_chat: a channel posting in a group, or the group itself for an
// anonymous admin) is that chat's: Telegram puts one stand-in user in from
// for all of them (Channel_Bot, GroupAnonymousBot), so going by from would
// make every channel poster one sender, sharing own-scope clears, folds,
// rows and limits. Anonymous admins still share the group's identity: the
// Bot API does not tell them apart.
func senderOf(msg *models.Message) profile {
	if c := msg.SenderChat; c != nil {
		return profile{id: c.ID, username: c.Username, firstName: c.Title, anonymous: c.ID == msg.Chat.ID}
	}
	return profile{id: msg.From.ID, username: msg.From.Username, firstName: msg.From.FirstName, lastName: msg.From.LastName,
		languageCode: msg.From.LanguageCode, isPremium: msg.From.IsPremium}
}
