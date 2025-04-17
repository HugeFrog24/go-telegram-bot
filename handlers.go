package main

import (
	"context"
	"fmt"
	"strconv"
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
	firstName := message.From.FirstName
	text := message.Text

	// Check if it's a new chat
	isNewChatFlag := b.isNewChat(chatID)

	// Screen incoming message
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

	// Get the chat memory which now contains the user's message
	chatMemory := b.getOrCreateChatMemory(chatID)
	contextMessages := b.prepareContextMessages(chatMemory)

	if isNewChatFlag {

		// Get response from Anthropic using the context messages
		response, err := b.getAnthropicResponse(ctx, contextMessages, true, isOwner, false, username, firstName)
		if err != nil {
			ErrorLogger.Printf("Error getting Anthropic response: %v", err)
			// Use the same error message as in the non-new chat case
			response = "I'm sorry, I'm having trouble processing your request right now."
		}

		// Send the AI-generated response or error message
		if err := b.sendResponse(ctx, chatID, response, businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
			return
		}
	} else {
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
						// Parse command parameters
						parts := strings.Fields(message.Text)

						// Default: show global stats
						if len(parts) == 1 {
							b.sendStats(ctx, chatID, userID, 0, businessConnectionID)
							return
						}

						// Check for "user" parameter
						if len(parts) >= 2 && parts[1] == "user" {
							var targetUserID int64 = userID // Default to current user

							// If a user ID is provided, parse it
							if len(parts) >= 3 {
								var parseErr error
								targetUserID, parseErr = strconv.ParseInt(parts[2], 10, 64)
								if parseErr != nil {
									InfoLogger.Printf("User %d provided invalid user ID format: %s", userID, parts[2])
									b.sendResponse(ctx, chatID, "Invalid user ID format. Usage: /stats user [user_id]", businessConnectionID)
									return
								}
							}

							b.sendStats(ctx, chatID, userID, targetUserID, businessConnectionID)
							return
						}

						// Invalid parameter
						b.sendResponse(ctx, chatID, "Invalid command format. Usage: /stats or /stats user [user_id]", businessConnectionID)
						return
					case "/whoami":
						b.sendWhoAmI(ctx, chatID, userID, username, businessConnectionID)
						return
					case "/clear":
						// Extract optional user ID parameter
						parts := strings.Fields(message.Text)
						var targetUserID int64 = 0
						if len(parts) > 1 {
							// Parse the user ID
							var parseErr error
							targetUserID, parseErr = strconv.ParseInt(parts[1], 10, 64)
							if parseErr != nil {
								// Invalid user ID format
								InfoLogger.Printf("User %d provided invalid user ID format: %s", userID, parts[1])
								b.sendResponse(ctx, chatID, "Invalid user ID format. Usage: /clear [user_id]", businessConnectionID)
								return
							}
						}
						b.clearChatHistory(ctx, chatID, userID, targetUserID, businessConnectionID, false)
						return
					case "/clear_hard":
						// Extract optional user ID parameter
						parts := strings.Fields(message.Text)
						var targetUserID int64 = 0
						if len(parts) > 1 {
							// Parse the user ID
							var parseErr error
							targetUserID, parseErr = strconv.ParseInt(parts[1], 10, 64)
							if parseErr != nil {
								// Invalid user ID format
								InfoLogger.Printf("User %d provided invalid user ID format: %s", userID, parts[1])
								b.sendResponse(ctx, chatID, "Invalid user ID format. Usage: /clear_hard [user_id]", businessConnectionID)
								return
							}
						}
						b.clearChatHistory(ctx, chatID, userID, targetUserID, businessConnectionID, true)
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
		response, err := b.getAnthropicResponse(ctx, contextMessages, false, isOwner, isEmojiOnly, username, firstName) // isNewChat is false here
		if err != nil {
			ErrorLogger.Printf("Error getting Anthropic response: %v", err)
			response = "I'm sorry, I'm having trouble processing your request right now."
		}

		// Send the response
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

	// Send the response
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
		response, err := b.getAnthropicResponse(ctx, contextMessages, false, false, false, message.Username, "")
		if err != nil {
			return "", err
		}

		return response, nil
	}

	return "Hmm, that's interesting!", nil
}

func (b *Bot) clearChatHistory(ctx context.Context, chatID int64, currentUserID int64, targetUserID int64, businessConnectionID string, hardDelete bool) {
	// If targetUserID is provided and different from currentUserID, check permissions
	if targetUserID != 0 && targetUserID != currentUserID {
		// Check if the current user is an admin or owner
		if !b.isAdminOrOwner(currentUserID) {
			InfoLogger.Printf("User %d attempted to clear history for user %d without permission", currentUserID, targetUserID)
			if err := b.sendResponse(ctx, chatID, "Permission denied. Only admins and owners can clear other users' histories.", businessConnectionID); err != nil {
				ErrorLogger.Printf("Error sending response: %v", err)
			}
			return
		}

		// Check if the target user exists
		var targetUser User
		err := b.db.Where("telegram_id = ? AND bot_id = ?", targetUserID, b.botID).First(&targetUser).Error
		if err != nil {
			ErrorLogger.Printf("Error finding target user %d: %v", targetUserID, err)
			if err := b.sendResponse(ctx, chatID, fmt.Sprintf("User with ID %d not found.", targetUserID), businessConnectionID); err != nil {
				ErrorLogger.Printf("Error sending response: %v", err)
			}
			return
		}
	} else {
		// If no targetUserID is provided, set it to currentUserID
		targetUserID = currentUserID
	}

	// Delete messages from the database
	var err error
	if hardDelete {
		// Permanently delete messages
		if targetUserID == currentUserID {
			// Deleting own messages
			err = b.db.Unscoped().Where("chat_id = ? AND bot_id = ? AND user_id = ?", chatID, b.botID, targetUserID).Delete(&Message{}).Error
			InfoLogger.Printf("User %d permanently deleted their own chat history in chat %d", currentUserID, chatID)
		} else {
			// Deleting another user's messages
			err = b.db.Unscoped().Where("chat_id = ? AND bot_id = ? AND user_id = ?", chatID, b.botID, targetUserID).Delete(&Message{}).Error
			InfoLogger.Printf("Admin/owner %d permanently deleted chat history for user %d in chat %d", currentUserID, targetUserID, chatID)
		}
	} else {
		// Soft delete messages
		if targetUserID == currentUserID {
			// Deleting own messages
			err = b.db.Where("chat_id = ? AND bot_id = ? AND user_id = ?", chatID, b.botID, targetUserID).Delete(&Message{}).Error
			InfoLogger.Printf("User %d soft deleted their own chat history in chat %d", currentUserID, chatID)
		} else {
			// Deleting another user's messages
			err = b.db.Where("chat_id = ? AND bot_id = ? AND user_id = ?", chatID, b.botID, targetUserID).Delete(&Message{}).Error
			InfoLogger.Printf("Admin/owner %d soft deleted chat history for user %d in chat %d", currentUserID, targetUserID, chatID)
		}
	}

	if err != nil {
		ErrorLogger.Printf("Error clearing chat history: %v", err)
		if err := b.sendResponse(ctx, chatID, "Sorry, I couldn't clear the chat history.", businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
		}
		return
	}

	// Reset the chat memory if clearing own history
	if targetUserID == currentUserID {
		chatMemory := b.getOrCreateChatMemory(chatID)
		chatMemory.Messages = []Message{} // Clear the messages
		b.chatMemoriesMu.Lock()
		b.chatMemories[chatID] = chatMemory
		b.chatMemoriesMu.Unlock()
	}

	// Send a confirmation message
	var confirmationMessage string
	if targetUserID == currentUserID {
		confirmationMessage = "Your chat history has been cleared."
	} else {
		// Get the username of the target user if available
		var targetUser User
		err := b.db.Where("telegram_id = ? AND bot_id = ?", targetUserID, b.botID).First(&targetUser).Error
		if err == nil && targetUser.Username != "" {
			confirmationMessage = fmt.Sprintf("Chat history for user @%s (ID: %d) has been cleared.", targetUser.Username, targetUserID)
		} else {
			confirmationMessage = fmt.Sprintf("Chat history for user with ID %d has been cleared.", targetUserID)
		}
	}

	if err := b.sendResponse(ctx, chatID, confirmationMessage, businessConnectionID); err != nil {
		ErrorLogger.Printf("Error sending response: %v", err)
	}
}
