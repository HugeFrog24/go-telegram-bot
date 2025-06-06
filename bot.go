package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/liushuangls/go-anthropic/v2"
	"gorm.io/gorm"
)

type Bot struct {
	tgBot           TelegramClient
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

// Helper function to determine message type
func messageType(msg *models.Message) string {
	if msg.Sticker != nil {
		return "sticker"
	}
	return "text"
}

// NewBot initializes and returns a new Bot instance.
func NewBot(db *gorm.DB, config BotConfig, clock Clock, tgClient TelegramClient) (*Bot, error) {
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

	// Ensure the owner exists in the Users table
	var owner User
	err = db.Where("telegram_id = ? AND bot_id = ?", config.OwnerTelegramID, botEntry.ID).First(&owner).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// Assign the "owner" role
		var ownerRole Role
		err := db.Where("name = ?", "owner").First(&ownerRole).Error
		if err != nil {
			return nil, fmt.Errorf("owner role not found: %w", err)
		}

		owner = User{
			BotID:      botEntry.ID,
			TelegramID: config.OwnerTelegramID,
			Username:   "", // Initialize as empty; will be updated upon interaction
			RoleID:     ownerRole.ID,
			IsOwner:    true,
		}

		if err := db.Create(&owner).Error; err != nil {
			// If unique constraint is violated, another owner already exists
			if strings.Contains(err.Error(), "unique index") {
				return nil, fmt.Errorf("an owner already exists for this bot")
			}
			return nil, fmt.Errorf("failed to create owner user: %w", err)
		}
	} else if err != nil {
		return nil, err
	}

	// Use the per-bot Anthropic API key
	anthropicClient := anthropic.NewClient(config.AnthropicAPIKey)

	b := &Bot{
		db:              db,
		anthropicClient: anthropicClient,
		chatMemories:    make(map[int64]*ChatMemory),
		memorySize:      config.MemorySize,
		config:          config,
		userLimiters:    make(map[int64]*userLimiter),
		clock:           clock,
		botID:           botEntry.ID, // Ensure BotModel has ID field
		tgBot:           tgClient,
	}

	if tgClient == nil {
		var err error
		tgClient, err = initTelegramBot(config.TelegramToken, b)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Telegram bot: %w", err)
		}
		b.tgBot = tgClient
	}

	return b, nil
}

// Start begins the bot's operation.
func (b *Bot) Start(ctx context.Context) {
	b.tgBot.Start(ctx)
}

func (b *Bot) getOrCreateUser(userID int64, username string, isOwner bool) (User, error) {
	var user User
	err := b.db.Preload("Role").Where("telegram_id = ? AND bot_id = ?", userID, b.botID).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Check if an owner already exists for this bot
			if isOwner {
				var existingOwner User
				err := b.db.Where("bot_id = ? AND is_owner = ?", b.botID, true).First(&existingOwner).Error
				if err == nil {
					return User{}, fmt.Errorf("an owner already exists for this bot")
				} else if !errors.Is(err, gorm.ErrRecordNotFound) {
					return User{}, fmt.Errorf("failed to check existing owner: %w", err)
				}
			}

			var role Role
			var roleName string
			if isOwner {
				roleName = "owner"
			} else {
				roleName = "user" // Assign "user" role to non-owner users
			}

			err := b.db.Where("name = ?", roleName).First(&role).Error
			if err != nil {
				return User{}, fmt.Errorf("failed to get role: %w", err)
			}

			user = User{
				BotID:      b.botID,
				TelegramID: userID,
				Username:   username,
				RoleID:     role.ID,
				Role:       role,
				IsOwner:    isOwner,
			}

			if err := b.db.Create(&user).Error; err != nil {
				// If unique constraint is violated, another owner already exists
				if strings.Contains(err.Error(), "unique index") {
					return User{}, fmt.Errorf("an owner already exists for this bot")
				}
				return User{}, fmt.Errorf("failed to create user: %w", err)
			}
		} else {
			return User{}, err
		}
	} else {
		if isOwner && !user.IsOwner {
			return User{}, fmt.Errorf("cannot change existing user to owner")
		}
	}

	return user, nil
}

func (b *Bot) getRoleByName(roleName string) (Role, error) {
	var role Role
	err := b.db.Where("name = ?", roleName).First(&role).Error
	return role, err
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

// storeMessage stores a message in the database and updates its ID
func (b *Bot) storeMessage(message *Message) error {
	message.BotID = b.botID           // Associate the message with the correct bot
	return b.db.Create(message).Error // This will update the message with its new ID
}

func (b *Bot) getOrCreateChatMemory(chatID int64) *ChatMemory {
	b.chatMemoriesMu.RLock()
	chatMemory, exists := b.chatMemories[chatID]
	b.chatMemoriesMu.RUnlock()

	if !exists {
		b.chatMemoriesMu.Lock()
		defer b.chatMemoriesMu.Unlock()

		chatMemory, exists = b.chatMemories[chatID]
		if !exists {
			// Check if this is a new chat by querying the database
			var count int64
			b.db.Model(&Message{}).Where("chat_id = ? AND bot_id = ?", chatID, b.botID).Count(&count)
			isNewChat := count == 0 // Truly new chat if no messages exist

			var messages []Message
			if !isNewChat {
				// Fetch existing messages only if it's not a new chat
				err := b.db.Where("chat_id = ? AND bot_id = ?", chatID, b.botID).
					Order("timestamp asc").
					Limit(b.memorySize * 2).
					Find(&messages).Error

				if err != nil {
					ErrorLogger.Printf("Error fetching messages from database: %v", err)
					messages = []Message{} // Initialize an empty slice on error
				}
			} else {
				messages = []Message{} // Ensure messages is initialized for new chats
			}

			chatMemory = &ChatMemory{
				Messages: messages,
				Size:     b.memorySize * 2,
			}

			b.chatMemories[chatID] = chatMemory
		}
	}

	return chatMemory
}

// addMessageToChatMemory adds a new message to the chat memory, ensuring the memory size is maintained.
func (b *Bot) addMessageToChatMemory(chatMemory *ChatMemory, message Message) {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()

	// Add the new message
	chatMemory.Messages = append(chatMemory.Messages, message)

	// Maintain the memory size
	if len(chatMemory.Messages) > chatMemory.Size {
		chatMemory.Messages = chatMemory.Messages[len(chatMemory.Messages)-chatMemory.Size:]
	}
}

func (b *Bot) prepareContextMessages(chatMemory *ChatMemory) []anthropic.Message {
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()

	// Debug logging
	InfoLogger.Printf("Chat memory contains %d messages", len(chatMemory.Messages))
	for i, msg := range chatMemory.Messages {
		InfoLogger.Printf("Message %d: IsUser=%v, Text=%q", i, msg.IsUser, msg.Text)
	}

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
	return count == 0 // Only consider a chat new if it has 0 messages
}

func (b *Bot) isAdminOrOwner(userID int64) bool {
	var user User
	err := b.db.Preload("Role").Where("telegram_id = ?", userID).First(&user).Error
	if err != nil {
		return false
	}
	return user.Role.Name == "admin" || user.Role.Name == "owner"
}

func initTelegramBot(token string, b *Bot) (TelegramClient, error) {
	opts := []bot.Option{
		bot.WithDefaultHandler(b.handleUpdate),
	}

	tgBot, err := bot.New(token, opts...)
	if err != nil {
		return nil, err
	}

	// Define bot commands
	commands := []models.BotCommand{
		{
			Command:     "stats",
			Description: "Get bot statistics. Usage: /stats or /stats user [user_id]",
		},
		{
			Command:     "whoami",
			Description: "Get your user information",
		},
		{
			Command:     "clear",
			Description: "Clear chat history (soft delete). Admins: /clear [user_id]",
		},
		{
			Command:     "clear_hard",
			Description: "Clear chat history (permanently delete). Admins: /clear_hard [user_id]",
		},
	}

	// Set bot commands
	_, err = tgBot.SetMyCommands(context.Background(), &bot.SetMyCommandsParams{
		Commands: commands,
	})
	if err != nil {
		ErrorLogger.Printf("Error setting bot commands: %v", err)
		return nil, err
	}

	return tgBot, nil
}

func (b *Bot) sendResponse(ctx context.Context, chatID int64, text string, businessConnectionID string) error {
	// Pass the outgoing message through the centralized screen for storage and chat memory update
	_, err := b.screenOutgoingMessage(chatID, text)
	if err != nil {
		ErrorLogger.Printf("Error storing assistant message: %v", err)
		return err
	}

	// Prepare message parameters
	params := &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	}

	if businessConnectionID != "" {
		params.BusinessConnectionID = businessConnectionID
	}

	// Send the message via Telegram client
	_, err = b.tgBot.SendMessage(ctx, params)
	if err != nil {
		ErrorLogger.Printf("[%s] Error sending message to chat %d with BusinessConnectionID %s: %v",
			b.config.ID, chatID, businessConnectionID, err)
		return err
	}
	return nil
}

// sendStats sends the bot statistics to the specified chat.
func (b *Bot) sendStats(ctx context.Context, chatID int64, userID int64, targetUserID int64, businessConnectionID string) {
	// If targetUserID is 0, show global stats
	if targetUserID == 0 {
		totalUsers, totalMessages, err := b.getStats()
		if err != nil {
			ErrorLogger.Printf("Error fetching stats: %v\n", err)
			if err := b.sendResponse(ctx, chatID, "Sorry, I couldn't retrieve the stats at this time.", businessConnectionID); err != nil {
				ErrorLogger.Printf("Error sending response: %v", err)
			}
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

		// Send the response through the centralized screen
		if err := b.sendResponse(ctx, chatID, statsMessage, businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending stats message: %v", err)
		}
		return
	}

	// If targetUserID is not 0, show user-specific stats
	// Check permissions if the user is trying to view someone else's stats
	if targetUserID != userID {
		if !b.isAdminOrOwner(userID) {
			InfoLogger.Printf("User %d attempted to view stats for user %d without permission", userID, targetUserID)
			if err := b.sendResponse(ctx, chatID, "Permission denied. Only admins and owners can view other users' statistics.", businessConnectionID); err != nil {
				ErrorLogger.Printf("Error sending response: %v", err)
			}
			return
		}
	}

	// Get user stats
	username, messagesIn, messagesOut, totalMessages, err := b.getUserStats(targetUserID)
	if err != nil {
		ErrorLogger.Printf("Error fetching user stats: %v\n", err)
		if err := b.sendResponse(ctx, chatID, fmt.Sprintf("Sorry, I couldn't retrieve statistics for user ID %d.", targetUserID), businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
		}
		return
	}

	// Build the user stats message
	userInfo := fmt.Sprintf("@%s (ID: %d)", username, targetUserID)
	if username == "" {
		userInfo = fmt.Sprintf("User ID: %d", targetUserID)
	}

	statsMessage := fmt.Sprintf(
		"👤 User Statistics for %s:\n\n"+
			"- Messages Sent: %d\n"+
			"- Messages Received: %d\n"+
			"- Total Messages: %d",
		userInfo,
		messagesIn,
		messagesOut,
		totalMessages,
	)

	if err := b.sendResponse(ctx, chatID, statsMessage, businessConnectionID); err != nil {
		ErrorLogger.Printf("Error sending user stats message: %v", err)
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

// getUserStats retrieves statistics for a specific user
func (b *Bot) getUserStats(userID int64) (string, int64, int64, int64, error) {
	// Get user information from database
	var user User
	err := b.db.Where("telegram_id = ? AND bot_id = ?", userID, b.botID).First(&user).Error
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("user not found: %w", err)
	}

	// Count messages sent by the user (IN)
	var messagesIn int64
	if err := b.db.Model(&Message{}).Where("user_id = ? AND bot_id = ? AND is_user = ?",
		userID, b.botID, true).Count(&messagesIn).Error; err != nil {
		return "", 0, 0, 0, err
	}

	// Count responses to the user (OUT)
	var messagesOut int64
	if err := b.db.Model(&Message{}).Where("chat_id IN (SELECT DISTINCT chat_id FROM messages WHERE user_id = ? AND bot_id = ?) AND bot_id = ? AND is_user = ?",
		userID, b.botID, b.botID, false).Count(&messagesOut).Error; err != nil {
		return "", 0, 0, 0, err
	}

	// Total messages is the sum
	totalMessages := messagesIn + messagesOut

	return user.Username, messagesIn, messagesOut, totalMessages, nil
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

func (b *Bot) sendWhoAmI(ctx context.Context, chatID int64, userID int64, username string, businessConnectionID string) {
	user, err := b.getOrCreateUser(userID, username, false)
	if err != nil {
		ErrorLogger.Printf("Error getting or creating user: %v", err)
		if err := b.sendResponse(ctx, chatID, "Sorry, I couldn't retrieve your information.", businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
		}
		return
	}

	role, err := b.getRoleByName(user.Role.Name)
	if err != nil {
		ErrorLogger.Printf("Error getting role by name: %v", err)
		if err := b.sendResponse(ctx, chatID, "Sorry, I couldn't retrieve your role information.", businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
		}
		return
	}

	whoAmIMessage := fmt.Sprintf(
		"👤 Your Information:\n\n"+
			"- Username: %s\n"+
			"- Role: %s",
		user.Username,
		role.Name,
	)

	// Send the response through the centralized screen
	if err := b.sendResponse(ctx, chatID, whoAmIMessage, businessConnectionID); err != nil {
		ErrorLogger.Printf("Error sending /whoami message: %v", err)
	}
}

// screenIncomingMessage centralizes all incoming message processing: storing messages and updating chat memory.
func (b *Bot) screenIncomingMessage(message *models.Message) (Message, error) {
	if b.config.DebugScreening {
		start := time.Now()
		defer func() {
			InfoLogger.Printf(
				"[Screen] Incoming: chat=%d user=%d type=%s memory_size=%d duration=%v",
				message.Chat.ID,
				message.From.ID,
				messageType(message),
				len(b.getOrCreateChatMemory(message.Chat.ID).Messages),
				time.Since(start),
			)
		}()
	}

	userRole := string(anthropic.RoleUser)

	// Determine message text based on message type
	messageText := message.Text
	if message.Sticker != nil {
		if message.Sticker.Emoji != "" {
			messageText = fmt.Sprintf("Sent a sticker: %s", message.Sticker.Emoji)
		} else {
			messageText = "Sent a sticker."
		}
	}

	userMessage := b.createMessage(message.Chat.ID, message.From.ID, message.From.Username, userRole, messageText, true)

	// Handle sticker-specific details if present
	if message.Sticker != nil {
		userMessage.StickerFileID = message.Sticker.FileID
		userMessage.StickerEmoji = message.Sticker.Emoji // Store the sticker emoji
		if message.Sticker.Thumbnail != nil {
			userMessage.StickerPNGFile = message.Sticker.Thumbnail.FileID
		}
	}

	// Get the chat memory before storing the message
	chatMemory := b.getOrCreateChatMemory(message.Chat.ID)

	// Store the message and get its ID
	if err := b.storeMessage(&userMessage); err != nil {
		return Message{}, err
	}

	// Add the message to the chat memory
	b.addMessageToChatMemory(chatMemory, userMessage)

	return userMessage, nil
}

// screenOutgoingMessage handles storing of outgoing messages and updating chat memory.
// It also marks the most recent unanswered user message as answered.
func (b *Bot) screenOutgoingMessage(chatID int64, response string) (Message, error) {
	if b.config.DebugScreening {
		start := time.Now()
		defer func() {
			InfoLogger.Printf(
				"[Screen] Outgoing: chat=%d len=%d memory_size=%d duration=%v",
				chatID,
				len(response),
				len(b.getOrCreateChatMemory(chatID).Messages),
				time.Since(start),
			)
		}()
	}

	// Create and store the assistant message
	assistantMessage := b.createMessage(chatID, 0, "", string(anthropic.RoleAssistant), response, false)
	if err := b.storeMessage(&assistantMessage); err != nil {
		return Message{}, err
	}

	// Find and mark the most recent unanswered user message as answered
	now := time.Now()
	err := b.db.Model(&Message{}).
		Where("chat_id = ? AND bot_id = ? AND is_user = ? AND answered_on IS NULL",
			chatID, b.botID, true).
		Order("timestamp DESC").
		Limit(1).
		Update("answered_on", now).Error

	if err != nil {
		ErrorLogger.Printf("Error marking user message as answered: %v", err)
		// Continue even if there's an error updating the user message
	}

	// Update chat memory with the message that now has an ID
	chatMemory := b.getOrCreateChatMemory(chatID)
	b.addMessageToChatMemory(chatMemory, assistantMessage)

	return assistantMessage, nil
}

func (b *Bot) promoteUserToAdmin(promoterID, userToPromoteID int64) error {
	// Check if the promoter is an owner or admin
	if !b.isAdminOrOwner(promoterID) {
		return errors.New("only admins or owners can promote users to admin")
	}

	// Get the user to promote
	userToPromote, err := b.getOrCreateUser(userToPromoteID, "", false)
	if err != nil {
		return err
	}

	// Get the admin role
	var adminRole Role
	if err := b.db.Where("name = ?", "admin").First(&adminRole).Error; err != nil {
		return err
	}

	// Update the user's role
	userToPromote.RoleID = adminRole.ID
	userToPromote.Role = adminRole
	return b.db.Save(&userToPromote).Error
}
