// telegram_client_mock.go
package main

import (
	"context"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// MockTelegramClient is a mock implementation of TelegramClient for testing.
type MockTelegramClient struct {
	// You can add fields to keep track of calls if needed.
	SendMessageFunc func(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error)
	StartFunc       func(ctx context.Context) // Optional: track Start calls
}

// SendMessage mocks sending a message.
func (m *MockTelegramClient) SendMessage(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error) {
	if m.SendMessageFunc != nil {
		return m.SendMessageFunc(ctx, params)
	}
	// Default behavior: return an empty message without error.
	return &models.Message{}, nil
}

// Start mocks starting the Telegram client.
func (m *MockTelegramClient) Start(ctx context.Context) {
	if m.StartFunc != nil {
		m.StartFunc(ctx)
	}
	// Default behavior: do nothing.
}

// Add other mocked methods if your Bot uses more TelegramClient methods.
