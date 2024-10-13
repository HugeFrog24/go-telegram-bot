package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/liushuangls/go-anthropic/v2"
	"gorm.io/gorm"
)

type Bot struct {
	tgBot           *bot.Bot
	db              *gorm.DB
	anthropicClient *anthropic.Client
	chatMemories    map[int64]*ChatMemory
	memorySize      int
	chatMemoriesMu  sync.RWMutex
	config          Config
	userLimiters    map[int64]*userLimiter
	userLimitersMu  sync.RWMutex
}

func NewBot(db *gorm.DB, config Config) (*Bot, error) {
	anthropicClient := anthropic.NewClient(os.Getenv("ANTHROPIC_API_KEY"))

	b := &Bot{
		db:              db,
		anthropicClient: anthropicClient,
		chatMemories:    make(map[int64]*ChatMemory),
		memorySize:      config.MemorySize,
		config:          config,
		userLimiters:    make(map[int64]*userLimiter),
	}

	tgBot, err := initTelegramBot(b.handleUpdate)
	if err != nil {
		return nil, err
	}
	b.tgBot = tgBot

	return b, nil
}

func (b *Bot) Start(ctx context.Context) {
	b.tgBot.Start(ctx)
}

func (b *Bot) getOrCreateUser(userID int64, username string) (User, error) {
	var user User
	err := b.db.Preload("Role").Where("telegram_id = ?", userID).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			var defaultRole Role
			if err := b.db.Where("name = ?", "user").First(&defaultRole).Error; err != nil {
				return User{}, err
			}
			user = User{TelegramID: userID, Username: username, RoleID: defaultRole.ID}
			if err := b.db.Create(&user).Error; err != nil {
				return User{}, err
			}
		} else {
			return User{}, err
		}
	}
	return user, nil
}

func (b *Bot) createMessage(chatID, userID int64, username, userRole, text string, isUser bool) Message {
	return Message{
		ChatID:    chatID,
		UserID:    userID,
		Username:  username,
		UserRole:  userRole,
		Text:      text,
		Timestamp: time.Now(),
		IsUser:    isUser,
	}
}

func (b *Bot) storeMessage(message Message) error {
	return b.db.Create(&message).Error
}

func (b *Bot) getOrCreateChatMemory(chatID int64) *ChatMemory {
	b.chatMemoriesMu.RLock()
	chatMemory, exists := b.chatMemories[chatID]
	b.chatMemoriesMu.RUnlock()

	if !exists {
		var messages []Message
		b.db.Where("chat_id = ?", chatID).Order("timestamp asc").Limit(b.memorySize * 2).Find(&messages)

		chatMemory = &ChatMemory{
			Messages: messages,
			Size:     b.memorySize * 2,
		}

		b.chatMemoriesMu.Lock()
		b.chatMemories[chatID] = chatMemory
		b.chatMemoriesMu.Unlock()
	}

	return chatMemory
}

func (b *Bot) addMessageToChatMemory(chatMemory *ChatMemory, message Message) {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()

	chatMemory.Messages = append(chatMemory.Messages, message)
	if len(chatMemory.Messages) > chatMemory.Size {
		chatMemory.Messages = chatMemory.Messages[2:]
	}
}

func (b *Bot) prepareContextMessages(chatMemory *ChatMemory) []anthropic.Message {
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()

	var contextMessages []anthropic.Message
	for _, msg := range chatMemory.Messages {
		role := anthropic.RoleUser
		if !msg.IsUser {
			role = anthropic.RoleAssistant
		}
		contextMessages = append(contextMessages, anthropic.Message{
			Role: role,
			Content: []anthropic.MessageContent{
				anthropic.NewTextMessageContent(msg.Text),
			},
		})
	}
	return contextMessages
}

func (b *Bot) isNewChat(chatID int64) bool {
	var count int64
	b.db.Model(&Message{}).Where("chat_id = ?", chatID).Count(&count)
	return count == 1
}

func (b *Bot) isAdminOrOwner(userID int64) bool {
	var user User
	err := b.db.Preload("Role").Where("telegram_id = ?", userID).First(&user).Error
	if err != nil {
		return false
	}
	return user.Role.Name == "admin" || user.Role.Name == "owner"
}

func initTelegramBot(handleUpdate func(ctx context.Context, b *bot.Bot, update *models.Update)) (*bot.Bot, error) {
	opts := []bot.Option{
		bot.WithDefaultHandler(handleUpdate),
	}

	return bot.New(os.Getenv("TELEGRAM_BOT_TOKEN"), opts...)
}
