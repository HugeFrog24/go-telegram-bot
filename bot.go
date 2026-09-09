package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"gorm.io/gorm"
)

type Bot struct {
	tgBot           TelegramClient
	db              *gorm.DB
	anthropicClient anthropic.Client
	chatMemories    map[int64]*ChatMemory
	memorySize      int
	chatMemoriesMu  sync.RWMutex
	config          BotConfig
	userLimiters    map[int64]*userLimiter
	userLimitersMu  sync.RWMutex
	clock           Clock
	botID           uint
	albumBuffers    map[string]*pendingAlbum
	albumBuffersMu  sync.Mutex
	intakeBuffers   map[int64]*pendingIntake
	intakeBuffersMu sync.Mutex
	intakeSeq       uint64
	turns           map[int64]*chatTurn
	turnsMu         sync.Mutex
	turnSeq         uint64
}

func messageType(msg *models.Message) string {
	if msg.Sticker != nil {
		return "sticker"
	}
	return "text"
}

func NewBot(db *gorm.DB, config BotConfig, clock Clock, tgClient TelegramClient) (*Bot, error) {
	var botEntry BotModel
	err := db.Where("identifier = ?", config.ID).First(&botEntry).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		botEntry = BotModel{Identifier: config.ID, Name: config.ID}
		if err := db.Create(&botEntry).Error; err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	var owner User
	err = db.Where("telegram_id = ? AND bot_id = ?", config.OwnerTelegramID, botEntry.ID).First(&owner).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		var ownerRole Role
		err := db.Where("name = ?", "owner").First(&ownerRole).Error
		if err != nil {
			return nil, fmt.Errorf("owner role not found: %w", err)
		}

		owner = User{
			BotID:      botEntry.ID,
			TelegramID: config.OwnerTelegramID,
			Username:   "",
			RoleID:     ownerRole.ID,
			IsOwner:    true,
		}

		if err := db.Create(&owner).Error; err != nil {
			if strings.Contains(err.Error(), "unique index") {
				return nil, fmt.Errorf("an owner already exists for this bot")
			}
			return nil, fmt.Errorf("failed to create owner user: %w", err)
		}
	} else if err != nil {
		return nil, err
	}

	anthropicClient := anthropic.NewClient(option.WithAPIKey(config.AnthropicAPIKey))

	b := &Bot{
		db:              db,
		anthropicClient: anthropicClient,
		chatMemories:    make(map[int64]*ChatMemory),
		memorySize:      config.MemorySize,
		config:          config,
		userLimiters:    make(map[int64]*userLimiter),
		clock:           clock,
		botID:           botEntry.ID,
		tgBot:           tgClient,
		albumBuffers:    make(map[string]*pendingAlbum),
		intakeBuffers:   make(map[int64]*pendingIntake),
		turns:           make(map[int64]*chatTurn),
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

func (b *Bot) Start(ctx context.Context) {
	b.tgBot.Start(ctx)
}

func (b *Bot) getOrCreateUser(userID int64, username string, isOwner bool) (User, error) {
	var user User
	err := b.db.Preload("Role").Where("telegram_id = ? AND bot_id = ?", userID, b.botID).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
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
				roleName = "user"
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

func (b *Bot) storeMessage(message *Message) error {
	message.BotID = b.botID
	return b.db.Create(message).Error
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
			var count int64
			b.db.Model(&Message{}).Where("chat_id = ? AND bot_id = ?", chatID, b.botID).Count(&count)
			isNewChat := count == 0

			var messages []Message
			if !isNewChat {
				err := b.db.Where("chat_id = ? AND bot_id = ?", chatID, b.botID).
					Order("timestamp desc").
					Limit(b.memorySize * 2).
					Find(&messages).Error

				if err != nil {
					ErrorLogger.Printf("Error fetching messages from database: %v", err)
					messages = []Message{}
				} else {
					for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
						messages[i], messages[j] = messages[j], messages[i]
					}
				}
			} else {
				messages = []Message{}
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

func (b *Bot) stripDeadFileIDFromMemory(chatID int64, deadFileID string) {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	cm, exists := b.chatMemories[chatID]
	if !exists {
		return
	}
	for i := range cm.Messages {
		if len(cm.Messages[i].ImageFileIDs) == 0 {
			continue
		}
		survivors := make([]string, 0, len(cm.Messages[i].ImageFileIDs))
		for _, fid := range cm.Messages[i].ImageFileIDs {
			if fid != deadFileID {
				survivors = append(survivors, fid)
			}
		}
		cm.Messages[i].ImageFileIDs = survivors
	}
}

func (b *Bot) addMessageToChatMemory(chatMemory *ChatMemory, message Message) {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()

	chatMemory.Messages = append(chatMemory.Messages, message)

	if len(chatMemory.Messages) > chatMemory.Size {
		chatMemory.Messages = chatMemory.Messages[len(chatMemory.Messages)-chatMemory.Size:]
	}
}

// prepareContextMessages converts chat memory into API messages.
func (b *Bot) prepareContextMessages(chatMemory *ChatMemory) []anthropic.BetaMessageParam {
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	return b.buildContextMessages(chatMemory)
}

// snapshotForTurn builds the model context for turn and records on it the
// newest user message that context includes, both read under one lock so they
// cannot disagree. The recorded id is what lets a later flush tell whether the
// running turn already answers it.
func (b *Bot) snapshotForTurn(turn *chatTurn, chatMemory *ChatMemory) []anthropic.BetaMessageParam {
	b.chatMemoriesMu.RLock()
	contextMessages := b.buildContextMessages(chatMemory)
	covered := newestUserMessageID(chatMemory)
	b.chatMemoriesMu.RUnlock()

	b.markCovered(turn, covered)
	return contextMessages
}

// newestUserMessageID returns the highest database id among the user messages
// in memory. Ids are monotonic, so "newer" and "higher" agree.
func newestUserMessageID(chatMemory *ChatMemory) uint {
	var newest uint
	for _, msg := range chatMemory.Messages {
		if msg.IsUser && msg.ID > newest {
			newest = msg.ID
		}
	}
	return newest
}

func (b *Bot) newestUserMessageIDInChat(chatID int64) uint {
	chatMemory := b.getOrCreateChatMemory(chatID)
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	return newestUserMessageID(chatMemory)
}

// buildContextMessages is prepareContextMessages without the lock.
func (b *Bot) buildContextMessages(chatMemory *ChatMemory) []anthropic.BetaMessageParam {
	InfoLogger.Printf("Chat memory contains %d messages", len(chatMemory.Messages))
	if b.config.DebugScreening {
		for i, msg := range chatMemory.Messages {
			InfoLogger.Printf("Message %d: IsUser=%v, Text=%q Images=%d", i, msg.IsUser, msg.Text, len(msg.ImageFileIDs))
		}
	}

	var contextMessages []anthropic.BetaMessageParam
	for _, msg := range chatMemory.Messages {
		blocks := contentBlocksForMessage(msg)
		if len(blocks) == 0 {
			continue
		}
		var param anthropic.BetaMessageParam
		if msg.IsUser {
			param = anthropic.NewBetaUserMessage(blocks...)
		} else {
			param = anthropic.BetaMessageParam{
				Role:    anthropic.BetaMessageParamRoleAssistant,
				Content: blocks,
			}
		}
		contextMessages = append(contextMessages, param)
	}

	if b.config.CacheHistoryEnabled() {
		markTrailingCacheBreakpoint(contextMessages)
	}
	return contextMessages
}

// markTrailingCacheBreakpoint puts a cache_control breakpoint on the final
// content block of the conversation, so the next turn reads the whole prefix
// from cache instead of reprocessing it. The system prompt keeps its own
// breakpoint; tools and system render ahead of messages, so the two compose.
//
// Caveat worth knowing when reading [usage] lines: chat memory is a sliding
// window. Once it is full, each new turn evicts the oldest message, which
// changes the prefix and forces a miss. Until then, and for any chat shorter
// than the window, this converts a full-price reread into a cache read.
func markTrailingCacheBreakpoint(messages []anthropic.BetaMessageParam) {
	if len(messages) == 0 {
		return
	}
	blocks := messages[len(messages)-1].Content
	if len(blocks) == 0 {
		return
	}

	switch last := &blocks[len(blocks)-1]; {
	case last.OfText != nil:
		last.OfText.CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
	case last.OfImage != nil:
		last.OfImage.CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
	}
}

func contentBlocksForMessage(msg Message) []anthropic.BetaContentBlockParamUnion {
	var blocks []anthropic.BetaContentBlockParamUnion
	if msg.IsUser && len(msg.ImageFileIDs) > 0 {
		multi := len(msg.ImageFileIDs) > 1
		for i, fileID := range msg.ImageFileIDs {
			if multi {
				blocks = append(blocks, anthropic.NewBetaTextBlock(fmt.Sprintf("Image %d:", i+1)))
			}
			blocks = append(blocks, anthropic.NewBetaImageBlock(anthropic.BetaFileImageSourceParam{FileID: fileID}))
		}
	}
	if textContent := strings.TrimSpace(msg.Text); textContent != "" {
		blocks = append(blocks, anthropic.NewBetaTextBlock(textContent))
	}
	return blocks
}

func roleHasScope(role Role, scope string) bool {
	for _, s := range role.Scopes {
		if s.Name == scope {
			return true
		}
	}
	return false
}

func (b *Bot) hasScope(userID int64, scope string) bool {
	var user User
	if err := b.db.Preload("Role.Scopes").
		Where("telegram_id = ? AND bot_id = ?", userID, b.botID).
		First(&user).Error; err != nil {
		return false
	}
	if user.IsOwner {
		return true
	}
	return roleHasScope(user.Role, scope)
}

var publicBotCommands = []models.BotCommand{
	{Command: "stats", Description: "Get bot statistics. Usage: /stats or /stats user [user_id]"},
	{Command: "whoami", Description: "Get your user information"},
	{Command: "clear", Description: "Clear chat history (soft delete). Admins: /clear [user_id]"},
}

var adminBotCommands = []models.BotCommand{
	{Command: "clear_hard", Description: "Clear chat history (permanently delete). Admins: /clear_hard [user_id]"},
	{Command: "set_model", Description: "Switch the AI model (admin/owner only). Usage: /set_model <model-id>"},
}

func (b *Bot) registerAdminCommandsForUser(ctx context.Context, telegramID int64) {
	allCommands := make([]models.BotCommand, 0, len(publicBotCommands)+len(adminBotCommands))
	allCommands = append(allCommands, publicBotCommands...)
	allCommands = append(allCommands, adminBotCommands...)
	_, err := b.tgBot.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: allCommands,
		Scope:    &models.BotCommandScopeChat{ChatID: telegramID},
	})
	if err != nil {
		ErrorLogger.Printf("Failed to register admin commands for user %d: %v", telegramID, err)
	}
}

func setElevatedCommands(tgBot TelegramClient, users []User) {
	allCommands := make([]models.BotCommand, 0, len(publicBotCommands)+len(adminBotCommands))
	allCommands = append(allCommands, publicBotCommands...)
	allCommands = append(allCommands, adminBotCommands...)
	for _, u := range users {
		if u.TelegramID == 0 {
			continue
		}
		if !u.IsOwner && !roleHasScope(u.Role, ScopeModelSet) {
			continue
		}
		_, err := tgBot.SetMyCommands(context.Background(), &bot.SetMyCommandsParams{
			Commands: allCommands,
			Scope:    &models.BotCommandScopeChat{ChatID: u.TelegramID},
		})
		if err != nil {
			ErrorLogger.Printf("Warning: could not set admin commands for user %d: %v", u.TelegramID, err)
		}
	}
}

func initTelegramBot(token string, b *Bot) (TelegramClient, error) {
	opts := []bot.Option{
		bot.WithDefaultHandler(b.handleUpdate),
	}

	tgBot, err := bot.New(token, opts...)
	if err != nil {
		return nil, err
	}

	_, err = tgBot.SetMyCommands(context.Background(), &bot.SetMyCommandsParams{
		Commands: publicBotCommands,
		Scope:    &models.BotCommandScopeDefault{},
	})
	if err != nil {
		ErrorLogger.Printf("Error setting default bot commands: %v", err)
		return nil, err
	}

	var allUsers []User
	if err := b.db.Preload("Role.Scopes").Where("bot_id = ?", b.botID).Find(&allUsers).Error; err != nil {
		ErrorLogger.Printf("Warning: could not query users for command scoping: %v", err)
	} else {
		setElevatedCommands(tgBot, allUsers)
	}

	return tgBot, nil
}

func (b *Bot) sendResponse(ctx context.Context, chatID int64, text string, businessConnectionID string) error {
	_, err := b.screenOutgoingMessage(chatID, text)
	if err != nil {
		ErrorLogger.Printf("Error storing assistant message: %v", err)
		return err
	}

	params := &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	}

	if businessConnectionID != "" {
		params.BusinessConnectionID = businessConnectionID
	}

	_, err = b.tgBot.SendMessage(ctx, params)
	if err != nil {
		ErrorLogger.Printf("[%s] Error sending message to chat %d with BusinessConnectionID %s: %v",
			b.config.ID, chatID, businessConnectionID, err)
		return err
	}
	return nil
}

func (b *Bot) sendOneSegment(ctx context.Context, chatID int64, text, businessConnectionID string) error {
	params := &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	}
	if businessConnectionID != "" {
		params.BusinessConnectionID = businessConnectionID
	}
	if _, err := b.tgBot.SendMessage(ctx, params); err != nil {
		ErrorLogger.Printf("[%s] Error sending segment to chat %d with BusinessConnectionID %s: %v",
			b.config.ID, chatID, businessConnectionID, err)
		return err
	}
	return nil
}

func (b *Bot) sendStats(ctx context.Context, chatID int64, userID int64, targetUserID int64, businessConnectionID string) {
	if targetUserID == 0 {
		totalUsers, totalMessages, err := b.getStats()
		if err != nil {
			ErrorLogger.Printf("Error fetching stats: %v\n", err)
			if err := b.sendResponse(ctx, chatID, "Sorry, I couldn't retrieve the stats at this time.", businessConnectionID); err != nil {
				ErrorLogger.Printf("Error sending response: %v", err)
			}
			return
		}

		statsMessage := fmt.Sprintf(
			"📊 Bot Statistics:\n\n"+
				"- Total Users: %d\n"+
				"- Total Messages: %d",
			totalUsers,
			totalMessages,
		)

		if b.hasScope(userID, ScopeStatsViewAny) {
			type topEntry struct {
				UserID   int64
				MsgCount int64
			}
			var top []topEntry
			if err := b.db.Model(&Message{}).
				Select("user_id, COUNT(*) as msg_count").
				Where("bot_id = ? AND is_user = ? AND deleted_at IS NULL", b.botID, true).
				Group("user_id").
				Order("msg_count DESC").
				Limit(3).
				Scan(&top).Error; err != nil {
				ErrorLogger.Printf("Error fetching top users: %v", err)
			} else if len(top) > 0 {
				statsMessage += "\n\n🏆 Most Active Users:"
				for i, entry := range top {
					var u User
					if err := b.db.Select("username").Where("telegram_id = ? AND bot_id = ?", entry.UserID, b.botID).First(&u).Error; err != nil {
						u.Username = fmt.Sprintf("ID:%d", entry.UserID)
					}
					name := u.Username
					if name == "" {
						name = fmt.Sprintf("ID:%d", entry.UserID)
					}
					statsMessage += fmt.Sprintf("\n%d. @%s — %d messages", i+1, name, entry.MsgCount)
				}
			}
		}

		if err := b.sendResponse(ctx, chatID, statsMessage, businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending stats message: %v", err)
		}
		return
	}

	if targetUserID != userID {
		if !b.hasScope(userID, ScopeStatsViewAny) {
			InfoLogger.Printf("User %d attempted to view stats for user %d without permission", userID, targetUserID)
			if err := b.sendResponse(ctx, chatID, "Permission denied. Only admins and owners can view other users' statistics.", businessConnectionID); err != nil {
				ErrorLogger.Printf("Error sending response: %v", err)
			}
			return
		}
	}

	username, messagesIn, messagesOut, totalMessages, err := b.getUserStats(targetUserID)
	if err != nil {
		ErrorLogger.Printf("Error fetching user stats: %v\n", err)
		if err := b.sendResponse(ctx, chatID, fmt.Sprintf("Sorry, I couldn't retrieve statistics for user ID %d.", targetUserID), businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
		}
		return
	}

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

func (b *Bot) getStats() (int64, int64, error) {
	var totalUsers int64
	if err := b.db.Model(&User{}).Where("bot_id = ?", b.botID).Count(&totalUsers).Error; err != nil {
		return 0, 0, err
	}

	var totalMessages int64
	if err := b.db.Model(&Message{}).Where("bot_id = ?", b.botID).Count(&totalMessages).Error; err != nil {
		return 0, 0, err
	}

	return totalUsers, totalMessages, nil
}

func (b *Bot) getUserStats(userID int64) (string, int64, int64, int64, error) {
	var user User
	err := b.db.Where("telegram_id = ? AND bot_id = ?", userID, b.botID).First(&user).Error
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("user not found: %w", err)
	}

	var messagesIn int64
	if err := b.db.Model(&Message{}).Where("user_id = ? AND bot_id = ? AND is_user = ?",
		userID, b.botID, true).Count(&messagesIn).Error; err != nil {
		return "", 0, 0, 0, err
	}

	var messagesOut int64
	if err := b.db.Model(&Message{}).Where("chat_id IN (SELECT DISTINCT chat_id FROM messages WHERE user_id = ? AND bot_id = ? AND deleted_at IS NULL) AND bot_id = ? AND is_user = ?",
		userID, b.botID, b.botID, false).Count(&messagesOut).Error; err != nil {
		return "", 0, 0, 0, err
	}

	totalMessages := messagesIn + messagesOut

	return user.Username, messagesIn, messagesOut, totalMessages, nil
}

func isOnlyEmojis(s string) bool {
	for _, r := range s {
		if !isEmoji(r) {
			return false
		}
	}
	return true
}

func isEmoji(r rune) bool {
	return (r >= 0x1F600 && r <= 0x1F64F) ||
		(r >= 0x1F300 && r <= 0x1F5FF) ||
		(r >= 0x1F680 && r <= 0x1F6FF) ||
		(r >= 0x2600 && r <= 0x26FF) ||
		(r >= 0x2700 && r <= 0x27BF)
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

	if err := b.sendResponse(ctx, chatID, whoAmIMessage, businessConnectionID); err != nil {
		ErrorLogger.Printf("Error sending /whoami message: %v", err)
	}
}

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

	userRole := "user"

	messageText := message.Text
	if message.Sticker != nil {
		if message.Sticker.Emoji != "" {
			messageText = fmt.Sprintf("Sent a sticker: %s", message.Sticker.Emoji)
		} else {
			messageText = "Sent a sticker."
		}
	}
	if message.Voice != nil {
		messageText = "[Voice message]"
	}

	userMessage := b.createMessage(message.Chat.ID, message.From.ID, message.From.Username, userRole, messageText, true)
	userMessage.TelegramMessageID = message.ID

	if message.Sticker != nil {
		userMessage.StickerFileID = message.Sticker.FileID
		userMessage.StickerEmoji = message.Sticker.Emoji
		if message.Sticker.Thumbnail != nil {
			userMessage.StickerPNGFile = message.Sticker.Thumbnail.FileID
		}
	}

	chatMemory := b.getOrCreateChatMemory(message.Chat.ID)

	if err := b.storeMessage(&userMessage); err != nil {
		return Message{}, err
	}

	b.addMessageToChatMemory(chatMemory, userMessage)

	return userMessage, nil
}

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

	assistantMessage := b.createMessage(chatID, 0, "", "assistant", response, false)
	if err := b.storeMessage(&assistantMessage); err != nil {
		return Message{}, err
	}

	// Mark every outstanding user message in the chat, not just the newest one.
	// A coalesced turn answers the whole batch, so a single-row update would
	// leave the earlier messages permanently unanswered. This also drops an
	// UPDATE ... ORDER BY ... LIMIT, which stock SQLite builds do not support.
	now := time.Now()
	err := b.db.Model(&Message{}).
		Where("chat_id = ? AND bot_id = ? AND is_user = ? AND answered_on IS NULL",
			chatID, b.botID, true).
		Update("answered_on", now).Error

	if err != nil {
		ErrorLogger.Printf("Error marking user messages as answered: %v", err)
	}

	chatMemory := b.getOrCreateChatMemory(chatID)
	b.addMessageToChatMemory(chatMemory, assistantMessage)

	return assistantMessage, nil
}

func (b *Bot) promoteUserToAdmin(promoterID, userToPromoteID int64) error {
	if !b.hasScope(promoterID, ScopeUserPromote) {
		return errors.New("only admins or owners can promote users to admin")
	}

	userToPromote, err := b.getOrCreateUser(userToPromoteID, "", false)
	if err != nil {
		return err
	}

	var adminRole Role
	if err := b.db.Where("name = ?", "admin").First(&adminRole).Error; err != nil {
		return err
	}

	userToPromote.RoleID = adminRole.ID
	userToPromote.Role = adminRole
	if err := b.db.Save(&userToPromote).Error; err != nil {
		return err
	}

	b.registerAdminCommandsForUser(context.Background(), userToPromoteID)
	return nil
}
