package main

import (
	"context"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/mock"
)

type MockTelegramClient struct {
	mock.Mock
	SendMessageFunc      func(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error)
	SendMessageDraftFunc func(ctx context.Context, params *bot.SendMessageDraftParams) (bool, error)
	SendAudioFunc        func(ctx context.Context, params *bot.SendAudioParams) (*models.Message, error)
	SendChatActionFunc   func(ctx context.Context, params *bot.SendChatActionParams) (bool, error)
	SetMyCommandsFunc    func(ctx context.Context, params *bot.SetMyCommandsParams) (bool, error)
	GetFileFunc          func(ctx context.Context, params *bot.GetFileParams) (*models.File, error)
	FileDownloadLinkFunc func(f *models.File) string
	StartFunc            func(ctx context.Context)
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
