package main

import (
	"context"
	"log"
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

	chatID := message.Chat.ID
	userID := message.From.ID

	// Extract businessConnectionID if available
	var businessConnectionID string
	if update.BusinessConnection != nil {
		businessConnectionID = update.BusinessConnection.ID
	} else if message.BusinessConnectionID != "" {
		businessConnectionID = message.BusinessConnectionID
	}

	// Check if the message is a command
	if message.Entities != nil {
		for _, entity := range message.Entities {
			if entity.Type == "bot_command" {
				command := strings.TrimSpace(message.Text[entity.Offset : entity.Offset+entity.Length])
				switch command {
				case "/stats":
					b.sendStats(ctx, chatID, userID, message.From.Username, businessConnectionID)
					return
				}
			}
		}
	}

	// Check if the message contains a sticker
	if message.Sticker != nil {
		b.handleStickerMessage(ctx, chatID, userID, message, businessConnectionID)
		return
	}

	// Existing rate limit and message handling
	if !b.checkRateLimits(userID) {
		if err := b.sendRateLimitExceededMessage(ctx, chatID, businessConnectionID); err != nil {
			log.Printf("Error sending rate limit exceeded message: %v", err)
			// Optionally, handle the error
		}
		return
	}

	username := message.From.Username
	text := message.Text

	// Proceed only if the message contains text
	if text == "" {
		log.Printf("Received a non-text message from user %d in chat %d", userID, chatID)
		return
	}

	user, err := b.getOrCreateUser(userID, username)
	if err != nil {
		log.Printf("Error getting or creating user: %v", err)
		return
	}

	// 1. Create the user message
	userMessage := b.createMessage(chatID, userID, username, user.Role.Name, text, true)
	userMessage.UserRole = string(anthropic.RoleUser)

	// 2. Store the message
	if err := b.storeMessage(userMessage); err != nil {
		log.Printf("Error storing user message: %v", err)
		return
	}

	// 3. Retrieve or create chat memory (now includes the current message)
	chatMemory := b.getOrCreateChatMemory(chatID)

	// 4. Prepare context messages
	contextMessages := b.prepareContextMessages(chatMemory)

	isEmojiOnly := isOnlyEmojis(text)
	response, err := b.getAnthropicResponse(ctx, contextMessages, b.isNewChat(chatID), b.isAdminOrOwner(userID), isEmojiOnly)
	if err != nil {
		log.Printf("Error getting Anthropic response: %v", err)
		response = "I'm sorry, I'm having trouble processing your request right now."
	}

	if err := b.sendResponse(ctx, chatID, response, businessConnectionID); err != nil {
		log.Printf("Error sending AI response: %v", err)
		return
	}

	assistantMessage := b.createMessage(chatID, 0, "", string(anthropic.RoleAssistant), response, false)
	if err := b.storeMessage(assistantMessage); err != nil {
		log.Printf("Error storing assistant message: %v", err)
	}
	// Since the assistant's message is also stored, it will be included in the chat memory during the next update

	// Optionally, you can avoid adding messages to chatMemory manually
}

func (b *Bot) sendRateLimitExceededMessage(ctx context.Context, chatID int64, businessConnectionID string) error {
	err := b.sendResponse(ctx, chatID, "Rate limit exceeded. Please try again later.", businessConnectionID)
	if err != nil {
		log.Printf("Error sending rate limit exceeded message: %v", err)
	}
	return err
}

func (b *Bot) handleStickerMessage(ctx context.Context, chatID, userID int64, message *models.Message, businessConnectionID string) {
	username := message.From.Username

	// Create and store the sticker message
	userMessage := b.createMessage(chatID, userID, username, "user", "Sent a sticker.", true)
	userMessage.StickerFileID = message.Sticker.FileID

	// Safely store the Thumbnail's FileID if available
	if message.Sticker.Thumbnail != nil {
		userMessage.StickerPNGFile = message.Sticker.Thumbnail.FileID
	}

	// Store the sticker message and check for errors
	if err := b.storeMessage(userMessage); err != nil {
		log.Printf("Error storing sticker message: %v", err)
		return
	}

	// Update chat memory
	chatMemory := b.getOrCreateChatMemory(chatID)
	b.addMessageToChatMemory(chatMemory, userMessage)

	// Generate AI response about the sticker
	response, err := b.generateStickerResponse(ctx, userMessage)
	if err != nil {
		log.Printf("Error generating sticker response: %v", err)
		// Provide a fallback dynamic response based on sticker type
		if message.Sticker.IsAnimated {
			response = "Wow, that's a cool animated sticker!"
		} else if message.Sticker.IsVideo {
			response = "Interesting video sticker!"
		} else {
			response = "That's a cool sticker!"
		}
	}

	// Send the response and check for errors
	if err := b.sendResponse(ctx, chatID, response, businessConnectionID); err != nil {
		log.Printf("Error sending sticker response: %v", err)
		return
	}

	assistantMessage := b.createMessage(chatID, 0, "", string(anthropic.RoleAssistant), response, false)
	if err := b.storeMessage(assistantMessage); err != nil {
		log.Printf("Error storing assistant message: %v", err)
		return
	}
	b.addMessageToChatMemory(chatMemory, assistantMessage)
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
