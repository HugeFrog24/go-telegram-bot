package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// businessConn is what the bot knows of one Business connection: the user
// whose business account made it, whether it is enabled, and the bot's
// rights in it, as getBusinessConnection or the latest business_connection
// update told it.
type businessConn struct {
	userID  int64
	enabled bool
	rights  models.BusinessBotRights
	fetched time.Time // on the bot's clock
	seq     uint64    // the order it was stored in (businessConnSeq)
}

// connFrom is the entry for a connection Telegram described.
func connFrom(c *models.BusinessConnection, now time.Time) businessConn {
	entry := businessConn{userID: c.User.ID, enabled: c.IsEnabled, fetched: now}
	if c.Rights != nil {
		entry.rights = *c.Rights
	}
	return entry
}

// canReply reports whether the bot may send in the connection's chats.
func (c businessConn) canReply() bool { return c.enabled && c.rights.CanReply }

// serves reports whether the bot serves the connection: it may reply there,
// and its user is one the bot may serve (mayConnect).
func (b *Bot) serves(c businessConn) bool { return c.canReply() && b.mayConnect(c.userID) }

// businessConnection is what the bot knows of connection id: its cached
// entry when that is younger than maxAge (any age, for 0), else what
// getBusinessConnection says, which is then cached. A failed lookup is not
// cached, and is tried again next time. A business_connection update that
// lands during the lookup wins over its answer (storeBusinessConn).
func (b *Bot) businessConnection(ctx context.Context, id string, maxAge time.Duration) (businessConn, error) {
	b.businessConnsMu.Lock()
	entry, ok := b.businessConns[id]
	since := b.businessConnSeq
	b.businessConnsMu.Unlock()
	if ok && (maxAge == 0 || b.clock.Now().Sub(entry.fetched) < maxAge) {
		return entry, nil
	}
	conn, err := b.tgBot.GetBusinessConnection(ctx, &bot.GetBusinessConnectionParams{BusinessConnectionID: id})
	if err == nil && conn == nil {
		err = errors.New("getBusinessConnection returned no connection")
	}
	if err != nil {
		return businessConn{}, err
	}
	return b.storeBusinessConn(id, connFrom(conn, b.clock.Now()), since), nil
}

// storeBusinessConn caches entry for connection id, unless an entry was
// stored after the store numbered since (an update that came in while a
// lookup ran), and returns the cached entry.
func (b *Bot) storeBusinessConn(id string, entry businessConn, since uint64) businessConn {
	b.businessConnsMu.Lock()
	defer b.businessConnsMu.Unlock()
	if cur, ok := b.businessConns[id]; ok && cur.seq > since {
		return cur
	}
	b.businessConnSeq++
	entry.seq = b.businessConnSeq
	b.businessConns[id] = entry
	return entry
}

// forgetBusinessRights drops what the bot knows of a connection, so the next
// lookup asks Telegram again.
func (b *Bot) forgetBusinessRights(connectionID string) {
	b.businessConnsMu.Lock()
	defer b.businessConnsMu.Unlock()
	delete(b.businessConns, connectionID)
}

// businessNotesAllowed reports whether a Business connection lets the bot
// show progress notes: it must be enabled and able to send messages (which
// covers editing them) and delete them afterwards. Every successful lookup,
// allowing or not, is reused for businessRightsTTL; a failed lookup means no
// notes and is retried next time.
func (b *Bot) businessNotesAllowed(ctx context.Context, connectionID string) bool {
	conn, err := b.businessConnection(ctx, connectionID, businessRightsTTL)
	if err != nil {
		ErrorLogger.Printf("[%s] business rights lookup for connection %s failed, no progress notes: %v",
			b.config.ID, connectionID, err)
		return false
	}
	return conn.enabled && conn.rights.CanReply && conn.rights.CanDeleteSentMessages
}

// mayConnect reports whether the bot serves Business connections the user
// makes: its owner, and the users in business.allowed_connection_users
// (user decision B8).
func (b *Bot) mayConnect(userID int64) bool {
	if userID != 0 && userID == b.config.OwnerTelegramID {
		return true
	}
	return b.config.Business != nil && slices.Contains(b.config.Business.AllowedConnectionUsers, userID)
}

// businessServed is what the bot knows of Business connection id, and
// whether it serves updates through it: the connection must have been made
// by a user it may serve (mayConnect), be enabled, and let the bot reply.
// Through any other connection nothing is stored or answered; a refusal of a
// stranger's connection is logged once per connection. The connection's user
// is looked up once (getBusinessConnection) and cached; business_connection
// updates keep the cache current (handleBusinessConnection). A failed lookup
// refuses the update, and is retried with the next one.
func (b *Bot) businessServed(ctx context.Context, id string) (businessConn, bool) {
	conn, err := b.businessConnection(ctx, id, 0)
	switch {
	case err != nil:
		ErrorLogger.Printf("[%s] business connection %s could not be looked up; its update is ignored: %v", b.config.ID, id, err)
		return conn, false
	case !b.mayConnect(conn.userID):
		b.logRefusedOnce(id, conn.userID)
		return conn, false
	case !conn.canReply():
		InfoLogger.Printf("[%s] business connection %s is disabled or may not reply; its update is ignored", b.config.ID, id)
		return conn, false
	}
	return conn, true
}

// logRefusedOnce logs, once per connection, that the bot ignores a
// connection the user made.
func (b *Bot) logRefusedOnce(id string, userID int64) {
	b.businessConnsMu.Lock()
	logged := b.businessRefused[id]
	b.businessRefused[id] = true
	b.businessConnsMu.Unlock()
	if !logged {
		InfoLogger.Printf("[%s] ignoring business connection %s of user %d: only the owner and business.allowed_connection_users may connect this bot",
			b.config.ID, id, userID)
	}
}

// handleBusinessConnection applies a business_connection update: the bot was
// connected to a business account or disconnected from it, or the account
// changed the connection's rights. Telegram's word replaces whatever the bot
// had cached for the connection: a disabled connection's entry is dropped
// for one that says so, so every later update through it is ignored without
// a lookup.
func (b *Bot) handleBusinessConnection(conn *models.BusinessConnection) {
	entry := b.storeBusinessConn(conn.ID, connFrom(conn, b.clock.Now()), ^uint64(0))
	served := b.serves(entry)
	InfoLogger.Printf("[%s] business connection %s of user %d: enabled=%v, can reply=%v, served=%v",
		b.config.ID, conn.ID, entry.userID, entry.enabled, entry.rights.CanReply, served)
	if !served {
		b.stopConnection(conn.ID)
	}
}

// sentByThisBot reports whether a Business message is one of this bot's own,
// sent on behalf of the business account (sender_business_bot). While the
// bot's own ID is unknown, any such message counts as its own: a business
// account connects one chatbot at a time.
func (b *Bot) sentByThisBot(message *models.Message) bool {
	sent := message.SenderBusinessBot
	return sent != nil && (b.selfID == 0 || sent.ID == b.selfID)
}

// ownerSide reports whether m is a business owner's own message.
func ownerSide(m Message) bool { return !m.IsUser && m.UserRole == roleBusinessOwner }

// recordOwnerMessage keeps a message the business account sent its customer
// (from) in a Business chat as the business side of the conversation (user
// decision B9): an assistant-side entry, in order, so later replies know what
// the owner said. It is never answered, starts no turn and is no customer
// message for coverage: it is not a user message (answerable), and a text
// turn's request ends before it if it is last (readForTurn). The model sees
// it as its own side, as the customer sees the bot's replies and the owner's
// messages alike come from the business account; the API combines it with an
// adjacent reply. Text, a caption or a sticker's emoji are kept; other media
// the bot does not read is stored aside.
func (b *Bot) recordOwnerMessage(message *models.Message, to replyTarget, from profile) {
	text := message.Text
	if text == "" {
		text = message.Caption
	}
	if message.Sticker != nil {
		text = "Sent a sticker."
		if message.Sticker.Emoji != "" {
			text = "Sent a sticker: " + message.Sticker.Emoji
		}
	}
	row := b.createMessage(to.conv(), from.id, from.username, roleBusinessOwner, text, false)
	row.UserID, row.Username = from.id, from.username
	row.TelegramMessageID = message.ID
	row.Aside = true // until it joins, as every message
	if err := b.storeMessage(&row); err != nil {
		ErrorLogger.Printf("[%s] storing the business owner's message %d in chat %v: %v", b.config.ID, message.ID, to.conv(), err)
		return
	}
	if strings.TrimSpace(text) == "" {
		return // media the bot does not read: it stays aside
	}
	b.joinConversation(row)
}

// connectionStopped reports whether the bot knows Business connection id to
// be one it no longer serves: disabled, unable to reply, or made by a user it
// may not serve. A connection it knows nothing of is not stopped: its
// messages passed the check on their way in (businessServed).
func (b *Bot) connectionStopped(id string) bool {
	if id == "" {
		return false
	}
	b.businessConnsMu.Lock()
	conn, ok := b.businessConns[id]
	b.businessConnsMu.Unlock()
	return ok && !b.serves(conn)
}

// stopConnection stops the bot's work through Business connection id, which
// it no longer serves (B10): each running turn of its conversations ends
// silently, filing and sending nothing, as one a /clear stopped does, and
// their waiting batches are dropped. Nothing is sent through it afterwards
// (sendOneSegment and the voice send check connectionStopped).
func (b *Bot) stopConnection(id string) {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	for k, running := range b.turns {
		if k.businessConnectionID != id || running.reason == cancelClear {
			continue
		}
		InfoLogger.Printf("[%s] turn %d for chat %v stopped: its business connection was disabled", b.config.ID, running.seq, k)
		running.reason = cancelClear
		running.rerun, running.rerunWant, running.rerunFirst = nil, 0, 0
		running.cancel()
	}
	b.intakeBuffersMu.Lock() // turnsMu, then intakeBuffersMu
	defer b.intakeBuffersMu.Unlock()
	for k := range b.intakeBuffers {
		if k.businessConnectionID == id {
			b.removeIntakeLocked(k)
		}
	}
}

// errConnectionStopped refuses a send through a Business connection the bot
// no longer serves.
var errConnectionStopped = errors.New("the business connection is disabled or may not reply")

// handleDeletedBusinessMessages applies deleted_business_messages: messages
// deleted in a Business chat leave its conversation (B10). Their rows in that
// conversation, the customer's and the business owner's alike, are
// soft-deleted first, so no reload brings them back, then they leave memory,
// so no later request holds them, and their uploaded files are deleted from
// the Files API (each upload belongs to its one message). A turn that read
// them already still answers; a message of them still on its way to memory
// finds its row gone and does not join.
func (b *Bot) handleDeletedBusinessMessages(ctx context.Context, deleted *models.BusinessMessagesDeleted) {
	if len(deleted.MessageIDs) == 0 {
		return
	}
	k := convKey{businessConnectionID: deleted.BusinessConnectionID, chatID: deleted.Chat.ID}
	scope, args := b.convScope(k)
	var rows []Message
	if err := b.db.Where(scope, args...).Where("telegram_message_id IN ?", deleted.MessageIDs).Find(&rows).Error; err != nil {
		ErrorLogger.Printf("[%s] reading the messages deleted in chat %v: %v", b.config.ID, k, err)
		return
	}
	if len(rows) == 0 {
		return
	}
	ids := make([]uint, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if err := b.db.Where(scope, args...).Where("id IN ?", ids).Delete(&Message{}).Error; err != nil {
		ErrorLogger.Printf("[%s] deleting the messages deleted in chat %v: %v", b.config.ID, k, err)
		return
	}
	b.dropFromMemory(k, ids)
	InfoLogger.Printf("[%s] %d message(s) deleted in chat %v left its conversation", b.config.ID, len(ids), k)
	b.deleteFilesOf(ctx, rows)
}

// dropFromMemory takes the messages with these row ids out of conversation
// k's memory, if it is loaded.
func (b *Bot) dropFromMemory(k convKey, ids []uint) {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	if mem := b.chatMemories[k]; mem != nil {
		mem.Messages = slices.DeleteFunc(mem.Messages, func(m Message) bool { return m.ID != 0 && slices.Contains(ids, m.ID) })
	}
}

// deleteFilesOf deletes the Files API uploads of deleted rows, on a context
// of its own, bounded as a compensation is (the update's context may end
// first). A row whose files all went is marked cleaned; one whose delete
// failed keeps its ids, so a /clear_hard of its conversation tries again.
func (b *Bot) deleteFilesOf(ctx context.Context, rows []Message) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensatingDeleteTimeout)
	defer cancel()
	for _, row := range rows {
		if len(row.ImageFileIDs) == 0 || !b.deleteRowFiles(ctx, row) {
			continue
		}
		now := time.Now()
		if err := b.db.Unscoped().Model(&Message{}).Where("id = ?", row.ID).Select("ImageFileIDs", "FilesCleanedAt").
			Updates(&Message{ImageFileIDs: nil, FilesCleanedAt: &now}).Error; err != nil {
			ErrorLogger.Printf("[%s] marking the files of deleted message row %d cleaned: %v", b.config.ID, row.ID, err)
		}
	}
}
