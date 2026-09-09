package main

import (
	"context"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// typingRefreshInterval re-arms the chat action before Telegram expires it.
// The Bot API sets the status "for 5 seconds or less", so anything at or above
// 5s leaves visible gaps. Telegram also clears the status as soon as the bot
// sends a message, so streamed segments naturally interrupt it until the next
// tick; there is no API call to clear it early.
const typingRefreshInterval = 4 * time.Second

// startChatAction shows a chat action (typing, uploading a photo, recording a
// voice note) and keeps it alive until the returned stop function runs.
//
// The returned function is idempotent and MUST be deferred by the caller. A
// keepalive loop that can outlive its turn is the failure mode behind
// openclaw/openclaw#27177, where the indicator stuck on until the process was
// restarted, so the loop here owns a derived context and exits on the first of:
// stop being called, or the parent context ending.
func (b *Bot) startChatAction(
	ctx context.Context,
	chatID int64,
	businessConnectionID string,
	action models.ChatAction,
) (stop func()) {
	actionCtx, cancel := context.WithCancel(ctx)

	send := func() {
		params := &bot.SendChatActionParams{
			ChatID: chatID,
			Action: action,
		}
		if businessConnectionID != "" {
			params.BusinessConnectionID = businessConnectionID
		}
		if _, err := b.tgBot.SendChatAction(actionCtx, params); err != nil {
			// Cosmetic only: a failed indicator must never abort the turn.
			InfoLogger.Printf("[%s] chat action %q failed for chat %d: %v",
				b.config.ID, action, chatID, err)
		}
	}

	send()

	go func() {
		ticker := time.NewTicker(typingRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-actionCtx.Done():
				return
			case <-ticker.C:
				send()
			}
		}
	}()

	return cancel
}
