package main

import (
	"context"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/liushuangls/go-anthropic/v2"
)

func (b *Bot) handleUpdate(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	var message *models.Message

	if update.Message != nil {
		message = update.Message
	} else if update.BusinessMessage != nil {
		message = update.BusinessMessage
	} else {
		// No message to process
		return
	}

	// Extract businessConnectionID if available
	var businessConnectionID string
	if update.BusinessConnection != nil {
		businessConnectionID = update.BusinessConnection.ID
	} else if message.BusinessConnectionID != "" {
		businessConnectionID = message.BusinessConnectionID
	}

	chatID := message.Chat.ID
	userID := message.From.ID
	username := message.From.Username
	text := message.Text

	// Check if it's a new chat
	if b.isNewChat(chatID) {
		// Get initial response for a new chat from Anthropic
		response, err := b.getAnthropicResponse(ctx, []anthropic.Message{}, true, false, false) // Empty context for new chat
		if err != nil {
			ErrorLogger.Printf("Error getting initial Anthropic response: %v", err)
			response = "Hello! I'm your new assistant."
		}

		// Send the initial response and handle outgoing message
		if err := b.sendResponse(ctx, chatID, response, businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending initial response: %v", err)
			return
		}
	} else {
		// Process incoming message through centralized screening
		if _, err := b.screenIncomingMessage(message); err != nil {
			ErrorLogger.Printf("Error storing user message: %v", err)
			return
		}

		// Determine if the user is the owner
		var isOwner bool
		err := b.db.Where("telegram_id = ? AND bot_id = ? AND is_owner = ?", userID, b.botID, true).First(&User{}).Error
		if err == nil {
			isOwner = true
		}

		user, err := b.getOrCreateUser(userID, username, isOwner)
		if err != nil {
			ErrorLogger.Printf("Error getting or creating user: %v", err)
			return
		}

		// Update the username if it's empty or has changed
		if user.Username != username {
			user.Username = username
			if err := b.db.Save(&user).Error; err != nil {
				ErrorLogger.Printf("Error updating user username: %v", err)
			}
		}

		// Check if the message is a command
		if message.Entities != nil {
			for _, entity := range message.Entities {
				if entity.Type == "bot_command" {
					command := strings.TrimSpace(message.Text[entity.Offset : entity.Offset+entity.Length])
					switch command {
					case "/stats":
						b.sendStats(ctx, chatID, businessConnectionID)
						return
					case "/whoami":
						b.sendWhoAmI(ctx, chatID, userID, username, businessConnectionID)
						return
					case "/clear":
						b.clearChatHistory(ctx, chatID, businessConnectionID, false)
						return
					case "/clear_hard":
						b.clearChatHistory(ctx, chatID, businessConnectionID, true)
						return
					}
				}
			}
		}

		// Check if the message contains a sticker
		if message.Sticker != nil {
			b.handleStickerMessage(ctx, chatID, message, businessConnectionID)
			return
		}

		// Rate limit check
		if !b.checkRateLimits(userID) {
			b.sendRateLimitExceededMessage(ctx, chatID, businessConnectionID)
			return
		}

		// Proceed only if the message contains text
		if text == "" {
			InfoLogger.Printf("Received a non-text message from user %d in chat %d", userID, chatID)
			return
		}

		// Determine if the text contains only emojis
		isEmojiOnly := isOnlyEmojis(text)

		// Prepare context messages for Anthropic
		chatMemory := b.getOrCreateChatMemory(chatID)
		contextMessages := b.prepareContextMessages(chatMemory)

		// Get response from Anthropic
		response, err := b.getAnthropicResponse(ctx, contextMessages, false, isOwner, isEmojiOnly) // isNewChat is false here
		if err != nil {
			ErrorLogger.Printf("Error getting Anthropic response: %v", err)
			response = "I'm sorry, I'm having trouble processing your request right now."
		}

		// Send the response and handle outgoing message
		if err := b.sendResponse(ctx, chatID, response, businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
			return
		}
	}
}

func (b *Bot) sendRateLimitExceededMessage(ctx context.Context, chatID int64, businessConnectionID string) {
	if err := b.sendResponse(ctx, chatID, "Rate limit exceeded. Please try again later.", businessConnectionID); err != nil {
		ErrorLogger.Printf("Error sending rate limit exceeded message: %v", err)
	}
}

func (b *Bot) handleStickerMessage(ctx context.Context, chatID int64, message *models.Message, businessConnectionID string) {
	// Process sticker through centralized screening
	userMessage, err := b.screenIncomingMessage(message)
	if err != nil {
		ErrorLogger.Printf("Error processing sticker message: %v", err)
		return
	}

	// Generate AI response about the sticker
	response, err := b.generateStickerResponse(ctx, userMessage)
	if err != nil {
		ErrorLogger.Printf("Error generating sticker response: %v", err)
		// Provide a fallback dynamic response based on sticker type
		if message.Sticker.IsAnimated {
			response = "Wow, that's a cool animated sticker!"
		} else if message.Sticker.IsVideo {
			response = "Interesting video sticker!"
		} else {
			response = "That's a cool sticker!"
		}
	}

	// Send the response through the centralized screen
	if err := b.sendResponse(ctx, chatID, response, businessConnectionID); err != nil {
		ErrorLogger.Printf("Error sending response: %v", err)
		return
	}
}

func (b *Bot) generateStickerResponse(ctx context.Context, message Message) (string, error) {
	// Example: Use the sticker type to generate a response
	if message.StickerFileID != "" {
		// Prepare context with information about the sticker
		contextMessages := []anthropic.Message{
			{
				Role: anthropic.RoleUser,
				Content: []anthropic.MessageContent{
					anthropic.NewTextMessageContent("User sent a sticker."),
				},
			},
		}

		// Since this is a sticker message, isEmojiOnly is false
		response, err := b.getAnthropicResponse(ctx, contextMessages, false, false, false)
		if err != nil {
			return "", err
		}

		return response, nil
	}

	return "Hmm, that's interesting!", nil
}

func (b *Bot) clearChatHistory(ctx context.Context, chatID int64, businessConnectionID string, hardDelete bool) {
	// Delete messages from the database
	var err error
	if hardDelete {
		// Permanently delete messages
		err = b.db.Unscoped().Where("chat_id = ? AND bot_id = ?", chatID, b.botID).Delete(&Message{}).Error
	} else {
		// Soft delete messages
		err = b.db.Where("chat_id = ? AND bot_id = ?", chatID, b.botID).Delete(&Message{}).Error
	}

	if err != nil {
		ErrorLogger.Printf("Error clearing chat history: %v", err)
		if err := b.sendResponse(ctx, chatID, "Sorry, I couldn't clear the chat history.", businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
		}
		return
	}

	// Reset the chat memory through the centralized memory management
	chatMemory := b.getOrCreateChatMemory(chatID)
	chatMemory.Messages = []Message{} // Clear the messages
	b.chatMemoriesMu.Lock()
	b.chatMemories[chatID] = chatMemory
	b.chatMemoriesMu.Unlock()

	// Send a confirmation message through the centralized screen
	if err := b.sendResponse(ctx, chatID, "Chat history cleared.", businessConnectionID); err != nil {
		ErrorLogger.Printf("Error sending response: %v", err)
	}
}
