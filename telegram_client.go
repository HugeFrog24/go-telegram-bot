// telegram_client.go
package main

import (
	"context"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// TelegramClient defines the methods required from the Telegram bot.
type TelegramClient interface {
	SendMessage(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error)
	Start(ctx context.Context)
	// Add other methods if needed.
}
