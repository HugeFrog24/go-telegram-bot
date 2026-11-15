package main

import (
	"context"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type TelegramClient interface {
	SendMessage(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error)
	SendMessageDraft(ctx context.Context, params *bot.SendMessageDraftParams) (bool, error)
	EditMessageText(ctx context.Context, params *bot.EditMessageTextParams) (*models.Message, error)
	DeleteMessage(ctx context.Context, params *bot.DeleteMessageParams) (bool, error)
	DeleteBusinessMessages(ctx context.Context, params *bot.DeleteBusinessMessagesParams) (bool, error)
	GetBusinessConnection(ctx context.Context, params *bot.GetBusinessConnectionParams) (*models.BusinessConnection, error)
	SendAudio(ctx context.Context, params *bot.SendAudioParams) (*models.Message, error)
	SendChatAction(ctx context.Context, params *bot.SendChatActionParams) (bool, error)
	SetMyCommands(ctx context.Context, params *bot.SetMyCommandsParams) (bool, error)
	GetFile(ctx context.Context, params *bot.GetFileParams) (*models.File, error)
	FileDownloadLink(f *models.File) string
	Start(ctx context.Context)
}
