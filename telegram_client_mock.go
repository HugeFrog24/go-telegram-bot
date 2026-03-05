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
	SendMessageFunc      func(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error)
	SendAudioFunc        func(ctx context.Context, params *bot.SendAudioParams) (*models.Message, error)
	SetMyCommandsFunc    func(ctx context.Context, params *bot.SetMyCommandsParams) (bool, error)
	GetFileFunc          func(ctx context.Context, params *bot.GetFileParams) (*models.File, error)
	FileDownloadLinkFunc func(f *models.File) string
	StartFunc            func(ctx context.Context)
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

// SetMyCommands mocks registering bot commands.
func (m *MockTelegramClient) SetMyCommands(ctx context.Context, params *bot.SetMyCommandsParams) (bool, error) {
	if m.SetMyCommandsFunc != nil {
		return m.SetMyCommandsFunc(ctx, params)
	}
	return true, nil
}

// SendAudio mocks sending an audio message.
func (m *MockTelegramClient) SendAudio(ctx context.Context, params *bot.SendAudioParams) (*models.Message, error) {
	if m.SendAudioFunc != nil {
		return m.SendAudioFunc(ctx, params)
	}
	return nil, nil
}

// GetFile mocks retrieving file info from Telegram.
func (m *MockTelegramClient) GetFile(ctx context.Context, params *bot.GetFileParams) (*models.File, error) {
	if m.GetFileFunc != nil {
		return m.GetFileFunc(ctx, params)
	}
	return &models.File{}, nil
}

// FileDownloadLink mocks building the file download URL.
func (m *MockTelegramClient) FileDownloadLink(f *models.File) string {
	if m.FileDownloadLinkFunc != nil {
		return m.FileDownloadLinkFunc(f)
	}
	return ""
}

// Start mocks starting the Telegram client.
func (m *MockTelegramClient) Start(ctx context.Context) {
	if m.StartFunc != nil {
		m.StartFunc(ctx)
		return
	}
	m.Called(ctx)
}
