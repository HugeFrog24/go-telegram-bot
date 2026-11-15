package main

import (
	"context"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/mock"
)

type MockTelegramClient struct {
	mock.Mock
	SendMessageFunc            func(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error)
	SendMessageDraftFunc       func(ctx context.Context, params *bot.SendMessageDraftParams) (bool, error)
	EditMessageTextFunc        func(ctx context.Context, params *bot.EditMessageTextParams) (*models.Message, error)
	DeleteMessageFunc          func(ctx context.Context, params *bot.DeleteMessageParams) (bool, error)
	DeleteBusinessMessagesFunc func(ctx context.Context, params *bot.DeleteBusinessMessagesParams) (bool, error)
	GetBusinessConnectionFunc  func(ctx context.Context, params *bot.GetBusinessConnectionParams) (*models.BusinessConnection, error)
	SendAudioFunc              func(ctx context.Context, params *bot.SendAudioParams) (*models.Message, error)
	SendChatActionFunc         func(ctx context.Context, params *bot.SendChatActionParams) (bool, error)
	SetMyCommandsFunc          func(ctx context.Context, params *bot.SetMyCommandsParams) (bool, error)
	GetFileFunc                func(ctx context.Context, params *bot.GetFileParams) (*models.File, error)
	FileDownloadLinkFunc       func(f *models.File) string
	StartFunc                  func(ctx context.Context)
}

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

func (m *MockTelegramClient) SendMessageDraft(ctx context.Context, params *bot.SendMessageDraftParams) (bool, error) {
	if m.SendMessageDraftFunc != nil {
		return m.SendMessageDraftFunc(ctx, params)
	}
	return true, nil
}

func (m *MockTelegramClient) EditMessageText(ctx context.Context, params *bot.EditMessageTextParams) (*models.Message, error) {
	if m.EditMessageTextFunc != nil {
		return m.EditMessageTextFunc(ctx, params)
	}
	return &models.Message{}, nil
}

func (m *MockTelegramClient) DeleteMessage(ctx context.Context, params *bot.DeleteMessageParams) (bool, error) {
	if m.DeleteMessageFunc != nil {
		return m.DeleteMessageFunc(ctx, params)
	}
	return true, nil
}

func (m *MockTelegramClient) DeleteBusinessMessages(ctx context.Context, params *bot.DeleteBusinessMessagesParams) (bool, error) {
	if m.DeleteBusinessMessagesFunc != nil {
		return m.DeleteBusinessMessagesFunc(ctx, params)
	}
	return true, nil
}

// GetBusinessConnection defaults to an enabled connection with every right
// progress notes need.
func (m *MockTelegramClient) GetBusinessConnection(ctx context.Context, params *bot.GetBusinessConnectionParams) (*models.BusinessConnection, error) {
	if m.GetBusinessConnectionFunc != nil {
		return m.GetBusinessConnectionFunc(ctx, params)
	}
	return &models.BusinessConnection{
		IsEnabled: true,
		Rights:    &models.BusinessBotRights{CanReply: true, CanDeleteSentMessages: true},
	}, nil
}

func (m *MockTelegramClient) SetMyCommands(ctx context.Context, params *bot.SetMyCommandsParams) (bool, error) {
	if m.SetMyCommandsFunc != nil {
		return m.SetMyCommandsFunc(ctx, params)
	}
	return true, nil
}

func (m *MockTelegramClient) SendAudio(ctx context.Context, params *bot.SendAudioParams) (*models.Message, error) {
	if m.SendAudioFunc != nil {
		return m.SendAudioFunc(ctx, params)
	}
	return nil, nil
}

func (m *MockTelegramClient) SendChatAction(ctx context.Context, params *bot.SendChatActionParams) (bool, error) {
	if m.SendChatActionFunc != nil {
		return m.SendChatActionFunc(ctx, params)
	}
	return true, nil
}

func (m *MockTelegramClient) GetFile(ctx context.Context, params *bot.GetFileParams) (*models.File, error) {
	if m.GetFileFunc != nil {
		return m.GetFileFunc(ctx, params)
	}
	return &models.File{}, nil
}

func (m *MockTelegramClient) FileDownloadLink(f *models.File) string {
	if m.FileDownloadLinkFunc != nil {
		return m.FileDownloadLinkFunc(f)
	}
	return ""
}

func (m *MockTelegramClient) Start(ctx context.Context) {
	if m.StartFunc != nil {
		m.StartFunc(ctx)
		return
	}
	m.Called(ctx)
}
