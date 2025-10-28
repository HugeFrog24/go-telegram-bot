// telegram_client_mock.go
package main

import (
	"context"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/mock"
)

// MockTelegramClient is a mock implementation of TelegramClient for testing.
type MockTelegramClient struct {
	mock.Mock
	SendMessageFunc func(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error)
	StartFunc       func(ctx context.Context)
}

// SendMessage mocks sending a message.
func (m *MockTelegramClient) SendMessage(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error) {
	if m.SendMessageFunc != nil {
		return m.SendMessageFunc(ctx, params)
	}
	args := m.Called(ctx, params)
	if msg, ok := args.Get(0).(*models.Message); ok {
		return msg, args.Error(1)
	}
	return nil, args.Error(1)
}

// Start mocks starting the Telegram client.
func (m *MockTelegramClient) Start(ctx context.Context) {
	if m.StartFunc != nil {
		m.StartFunc(ctx)
		return
	}
	m.Called(ctx)
}
