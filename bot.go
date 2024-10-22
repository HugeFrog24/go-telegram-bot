package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
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
	config          BotConfig
	userLimiters    map[int64]*userLimiter
	userLimitersMu  sync.RWMutex
	clock           Clock
	botID           uint // Reference to BotModel.ID
}

func NewBot(db *gorm.DB, config BotConfig, clock Clock) (*Bot, error) {
	// Retrieve or create Bot entry in the database
	var botEntry BotModel
	err := db.Where("identifier = ?", config.ID).First(&botEntry).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		botEntry = BotModel{Identifier: config.ID, Name: config.ID} // Customize as needed
		if err := db.Create(&botEntry).Error; err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	anthropicClient := anthropic.NewClient(os.Getenv("ANTHROPIC_API_KEY"))

	b := &Bot{
		db:              db,
		anthropicClient: anthropicClient,
		chatMemories:    make(map[int64]*ChatMemory),
		memorySize:      config.MemorySize,
		config:          config,
		userLimiters:    make(map[int64]*userLimiter),
		clock:           clock,
		botID:           botEntry.ID, // Ensure BotModel has ID field
	}

	tgBot, err := initTelegramBot(config.TelegramToken, b.handleUpdate)
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
	message := Message{
		ChatID:    chatID,
		UserRole:  userRole,
		Text:      text,
		Timestamp: time.Now(),
		IsUser:    isUser,
	}

	if isUser {
		message.UserID = userID
		message.Username = username
	} else {
		message.UserID = 0
		message.Username = "AI Assistant"
	}

	return message
}

func (b *Bot) storeMessage(message Message) error {
	message.BotID = b.botID // Associate the message with the correct bot
	return b.db.Create(&message).Error
}

func (b *Bot) getOrCreateChatMemory(chatID int64) *ChatMemory {
	b.chatMemoriesMu.RLock()
	chatMemory, exists := b.chatMemories[chatID]
	b.chatMemoriesMu.RUnlock()

	if !exists {
		b.chatMemoriesMu.Lock()
		// Double-check to prevent race condition
		chatMemory, exists = b.chatMemories[chatID]
		if !exists {
			var messages []Message
			b.db.Where("chat_id = ? AND bot_id = ?", chatID, b.botID).
				Order("timestamp asc").
				Limit(b.memorySize * 2).
				Find(&messages)

			chatMemory = &ChatMemory{
				Messages: messages,
				Size:     b.memorySize * 2,
			}

			b.chatMemories[chatID] = chatMemory
		}
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

		textContent := strings.TrimSpace(msg.Text)
		if textContent == "" {
			// Skip empty messages
			continue
		}

		contextMessages = append(contextMessages, anthropic.Message{
			Role: role,
			Content: []anthropic.MessageContent{
				anthropic.NewTextMessageContent(textContent),
			},
		})
	}
	return contextMessages
}

func (b *Bot) isNewChat(chatID int64) bool {
	var count int64
	b.db.Model(&Message{}).Where("chat_id = ? AND bot_id = ?", chatID, b.botID).Count(&count)
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

func initTelegramBot(token string, handleUpdate func(ctx context.Context, tgBot *bot.Bot, update *models.Update)) (*bot.Bot, error) {
	opts := []bot.Option{
		bot.WithDefaultHandler(handleUpdate),
	}

	return bot.New(token, opts...)
}

func (b *Bot) sendResponse(ctx context.Context, chatID int64, text string, businessConnectionID string) error {
	params := &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	}

	if businessConnectionID != "" {
		params.BusinessConnectionID = businessConnectionID
	}

	_, err := b.tgBot.SendMessage(ctx, params)
	if err != nil {
		log.Printf("[%s] [ERROR] Error sending message to chat %d with BusinessConnectionID %s: %v",
			b.config.ID, chatID, businessConnectionID, err)
		return err
	}
	return nil
}

// sendStats sends the bot statistics to the specified chat.
func (b *Bot) sendStats(ctx context.Context, chatID int64, userID int64, username string, businessConnectionID string) {
	totalUsers, totalMessages, err := b.getStats()
	if err != nil {
		fmt.Printf("Error fetching stats: %v\n", err)
		b.sendResponse(ctx, chatID, "Sorry, I couldn't retrieve the stats at this time.", businessConnectionID)
		return
	}

	// Do NOT manually escape hyphens here
	statsMessage := fmt.Sprintf(
		"📊 Bot Statistics:\n\n"+
			"- Total Users: %d\n"+
			"- Total Messages: %d",
		totalUsers,
		totalMessages,
	)

	// Store the user's /stats command
	userMessage := b.createMessage(chatID, userID, username, "user", "/stats", true)
	if err := b.storeMessage(userMessage); err != nil {
		log.Printf("Error storing user message: %v", err)
	}

	// Send and store the bot's response
	if err := b.sendResponse(ctx, chatID, statsMessage, businessConnectionID); err != nil {
		log.Printf("Error sending stats message: %v", err)
	}
	assistantMessage := b.createMessage(chatID, 0, "", "assistant", statsMessage, false)
	if err := b.storeMessage(assistantMessage); err != nil {
		log.Printf("Error storing assistant message: %v", err)
	}
}

// getStats retrieves the total number of users and messages from the database.
func (b *Bot) getStats() (int64, int64, error) {
	var totalUsers int64
	if err := b.db.Model(&User{}).Count(&totalUsers).Error; err != nil {
		return 0, 0, err
	}

	var totalMessages int64
	if err := b.db.Model(&Message{}).Count(&totalMessages).Error; err != nil {
		return 0, 0, err
	}

	return totalUsers, totalMessages, nil
}

// isOnlyEmojis checks if the string consists solely of emojis.
func isOnlyEmojis(s string) bool {
	for _, r := range s {
		if !isEmoji(r) {
			return false
		}
	}
	return true
}

// isEmoji determines if a rune is an emoji.
// This is a simplistic check and can be expanded based on requirements.
func isEmoji(r rune) bool {
	return (r >= 0x1F600 && r <= 0x1F64F) || // Emoticons
		(r >= 0x1F300 && r <= 0x1F5FF) || // Misc Symbols and Pictographs
		(r >= 0x1F680 && r <= 0x1F6FF) || // Transport and Map
		(r >= 0x2600 && r <= 0x26FF) || // Misc symbols
		(r >= 0x2700 && r <= 0x27BF) // Dingbats
}
