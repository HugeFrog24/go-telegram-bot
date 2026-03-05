package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/liushuangls/go-anthropic/v2"
)

// anthropicErrorResponse returns the message to send back to the user when getAnthropicResponse
// fails. Admins and owners receive an actionable hint when the model is deprecated; regular users
// always get the generic fallback to avoid leaking internal details.
func (b *Bot) anthropicErrorResponse(err error, userID int64) string {
	if errors.Is(err, ErrModelNotFound) && b.hasScope(userID, ScopeModelSet) {
		return fmt.Sprintf(
			"⚠️ Model `%s` is no longer available (deprecated or removed by Anthropic).\n"+
				"Use /set_model <model-id> to switch. Current models: https://platform.claude.com/docs/en/about-claude/models/overview",
			b.config.Model,
		)
	}
	return "I'm sorry, I'm having trouble processing your request right now."
}

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

	if message.From == nil {
		// Channel posts and some automated messages have no sender — ignore them.
		// see: https://core.telegram.org/bots/api#message
		return
	}

	chatID := message.Chat.ID
	userID := message.From.ID
	username := message.From.Username
	firstName := message.From.FirstName
	lastName := message.From.LastName
	languageCode := message.From.LanguageCode
	isPremium := message.From.IsPremium
	messageTime := message.Date
	text := message.Text

	// Check if it's a new chat (before storing the message so the flag is accurate).
	isNewChatFlag := b.isNewChat(chatID)

	// Screen incoming message (store to DB + add to chat memory)
	userMsg, err := b.screenIncomingMessage(message)
	if err != nil {
		ErrorLogger.Printf("Error storing user message: %v", err)
		return
	}

	// Determine if the user is the owner
	var isOwner bool
	err = b.db.Where("telegram_id = ? AND bot_id = ? AND is_owner = ?", userID, b.botID, true).First(&User{}).Error
	if err == nil {
		isOwner = true
	}

	// Always create/get the user record — on the very first message and on all subsequent ones.
	user, err := b.getOrCreateUser(userID, username, isOwner)
	if err != nil {
		ErrorLogger.Printf("Error getting or creating user: %v", err)
		return
	}

	// Update the username if it has changed
	if user.Username != username {
		user.Username = username
		if err := b.db.Save(&user).Error; err != nil {
			ErrorLogger.Printf("Error updating user username: %v", err)
		}
	}

	// Check if the message is a command — applies on every message, including the very first.
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
						targetUserID := userID // Default to current user

						// If a user ID is provided, parse it
						if len(parts) >= 3 {
							var parseErr error
							targetUserID, parseErr = strconv.ParseInt(parts[2], 10, 64)
							if parseErr != nil {
								InfoLogger.Printf("User %d provided invalid user ID format: %s", userID, parts[2])
								if err := b.sendResponse(ctx, chatID, "Invalid user ID format. Usage: /stats user [user_id]", businessConnectionID); err != nil {
									ErrorLogger.Printf("Error sending response: %v", err)
								}
								return
							}
						}

						b.sendStats(ctx, chatID, userID, targetUserID, businessConnectionID)
						return
					}

					// Invalid parameter
					if err := b.sendResponse(ctx, chatID, "Invalid command format. Usage: /stats or /stats user [user_id]", businessConnectionID); err != nil {
						ErrorLogger.Printf("Error sending response: %v", err)
					}
					return
				case "/whoami":
					b.sendWhoAmI(ctx, chatID, userID, username, businessConnectionID)
					return
				case "/clear":
					parts := strings.Fields(message.Text)
					var targetUserID, targetChatID int64
					if len(parts) > 1 {
						var parseErr error
						targetUserID, parseErr = strconv.ParseInt(parts[1], 10, 64)
						if parseErr != nil {
							InfoLogger.Printf("User %d provided invalid user ID format: %s", userID, parts[1])
							if err := b.sendResponse(ctx, chatID, "Invalid user ID format. Usage: /clear [user_id] [chat_id]", businessConnectionID); err != nil {
								ErrorLogger.Printf("Error sending response: %v", err)
							}
							return
						}
					}
					if len(parts) > 2 {
						var parseErr error
						targetChatID, parseErr = strconv.ParseInt(parts[2], 10, 64)
						if parseErr != nil {
							InfoLogger.Printf("User %d provided invalid chat ID format: %s", userID, parts[2])
							if err := b.sendResponse(ctx, chatID, "Invalid chat ID format. Usage: /clear [user_id] [chat_id]", businessConnectionID); err != nil {
								ErrorLogger.Printf("Error sending response: %v", err)
							}
							return
						}
					}
					b.clearChatHistory(ctx, chatID, userID, targetUserID, targetChatID, businessConnectionID, false)
					return
				case "/set_model":
					if !b.hasScope(userID, ScopeModelSet) {
						if err := b.sendResponse(ctx, chatID, "Permission denied. Only admins and owners can change the model.", businessConnectionID); err != nil {
							ErrorLogger.Printf("Error sending response: %v", err)
						}
						return
					}
					parts := strings.Fields(message.Text)
					if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
						if err := b.sendResponse(ctx, chatID, "Usage: /set_model <model-id>", businessConnectionID); err != nil {
							ErrorLogger.Printf("Error sending response: %v", err)
						}
						return
					}
					newModel := strings.TrimSpace(parts[1])
					// No upfront model validation:
					// - The go-anthropic library constants are not enumerable at runtime (Go has no const reflection).
					// - A live /v1/models probe would add a network round-trip and show in the API audit log.
					// - An invalid model ID will produce a 404 on the next real message, which routes through
					//   anthropicErrorResponse and already delivers an actionable admin-facing hint.
					if err := b.config.PersistModel(newModel); err != nil {
						ErrorLogger.Printf("Failed to persist model change: %v", err)
						if err := b.sendResponse(ctx, chatID, fmt.Sprintf("Model updated in memory to `%s`, but failed to save to config file: %v", newModel, err), businessConnectionID); err != nil {
							ErrorLogger.Printf("Error sending response: %v", err)
						}
						return
					}
					InfoLogger.Printf("Model changed to %s by user %d", newModel, userID)
					if err := b.sendResponse(ctx, chatID, fmt.Sprintf("✅ Model updated to `%s` and saved to config.", newModel), businessConnectionID); err != nil {
						ErrorLogger.Printf("Error sending response: %v", err)
					}
					return
				case "/clear_hard":
					parts := strings.Fields(message.Text)
					var targetUserID, targetChatID int64
					if len(parts) > 1 {
						var parseErr error
						targetUserID, parseErr = strconv.ParseInt(parts[1], 10, 64)
						if parseErr != nil {
							InfoLogger.Printf("User %d provided invalid user ID format: %s", userID, parts[1])
							if err := b.sendResponse(ctx, chatID, "Invalid user ID format. Usage: /clear_hard [user_id] [chat_id]", businessConnectionID); err != nil {
								ErrorLogger.Printf("Error sending response: %v", err)
							}
							return
						}
					}
					if len(parts) > 2 {
						var parseErr error
						targetChatID, parseErr = strconv.ParseInt(parts[2], 10, 64)
						if parseErr != nil {
							InfoLogger.Printf("User %d provided invalid chat ID format: %s", userID, parts[2])
							if err := b.sendResponse(ctx, chatID, "Invalid chat ID format. Usage: /clear_hard [user_id] [chat_id]", businessConnectionID); err != nil {
								ErrorLogger.Printf("Error sending response: %v", err)
							}
							return
						}
					}
					b.clearChatHistory(ctx, chatID, userID, targetUserID, targetChatID, businessConnectionID, true)
					return
				}
			}
		}
	}

	// Rate limit check applies to all message types including stickers.
	if !b.checkRateLimits(userID) {
		b.sendRateLimitExceededMessage(ctx, chatID, businessConnectionID)
		return
	}

	// Build context once — shared by the sticker and text response paths.
	chatMemory := b.getOrCreateChatMemory(chatID)
	contextMessages := b.prepareContextMessages(chatMemory)

	// Check if the message contains a sticker
	if message.Sticker != nil {
		b.handleStickerMessage(ctx, chatID, userMsg, message, contextMessages, businessConnectionID)
		return
	}

	// Proceed only if the message contains text
	if text == "" {
		InfoLogger.Printf("Received a non-text message from user %d in chat %d", userID, chatID)
		return
	}

	// Determine if the text contains only emojis
	isEmojiOnly := isOnlyEmojis(text)

	// Get response from Anthropic
	response, err := b.getAnthropicResponse(ctx, contextMessages, isNewChatFlag, isOwner, isEmojiOnly, username, firstName, lastName, isPremium, languageCode, messageTime)
	if err != nil {
		ErrorLogger.Printf("Error getting Anthropic response: %v", err)
		response = b.anthropicErrorResponse(err, userID)
	}

	// Send the response
	if err := b.sendResponse(ctx, chatID, response, businessConnectionID); err != nil {
		ErrorLogger.Printf("Error sending response: %v", err)
		return
	}
}

func (b *Bot) sendRateLimitExceededMessage(ctx context.Context, chatID int64, businessConnectionID string) {
	if err := b.sendResponse(ctx, chatID, "Rate limit exceeded. Please try again later.", businessConnectionID); err != nil {
		ErrorLogger.Printf("Error sending rate limit exceeded message: %v", err)
	}
}

func (b *Bot) handleStickerMessage(ctx context.Context, chatID int64, userMessage Message, message *models.Message, contextMessages []anthropic.Message, businessConnectionID string) {
	// userMessage was already screened (stored + added to memory) by handleUpdate — do not call screenIncomingMessage again.

	// Generate AI response about the sticker
	response, err := b.generateStickerResponse(ctx, userMessage, contextMessages)
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

func (b *Bot) generateStickerResponse(ctx context.Context, message Message, contextMessages []anthropic.Message) (string, error) {
	// contextMessages already contains the sticker turn (added by screenIncomingMessage as
	// "Sent a sticker: <emoji>"), so the full conversation history is preserved.
	if message.StickerFileID != "" {
		messageTime := int(message.Timestamp.Unix())
		response, err := b.getAnthropicResponse(ctx, contextMessages, false, false, true, message.Username, "", "", false, "", messageTime)
		if err != nil {
			return "", err
		}
		return response, nil
	}

	return "Hmm, that's interesting!", nil
}

func (b *Bot) clearChatHistory(ctx context.Context, chatID int64, currentUserID int64, targetUserID int64, targetChatID int64, businessConnectionID string, hardDelete bool) {
	// If targetUserID is provided and different from currentUserID, check permissions
	if targetUserID != 0 && targetUserID != currentUserID {
		requiredScope := ScopeHistoryClearAny
		if hardDelete {
			requiredScope = ScopeHistoryClearHardAny
		}
		if !b.hasScope(currentUserID, requiredScope) {
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
	//
	// Assumption: this bot is primarily used in private DMs, where each user's messages
	// are stored with chat_id == their own user_id — not the caller's chat_id. Scoping
	// a cross-user delete by the caller's chatID would therefore match 0 rows.
	//
	// When clearing another user's history the default (targetChatID == 0) deletes all
	// of that user's messages across every chat for this bot — the natural meaning of
	// "/clear <userID>" (wipe their entire history with the bot).
	//
	// When targetChatID != 0 the deletion is scoped to that specific chat, which is
	// useful for group moderation ("/clear <userID> <chatID>").
	var err error
	if hardDelete {
		// Permanently delete messages
		if targetUserID == currentUserID {
			// Own history — delete ALL messages (user + assistant) in the current chat.
			err = b.db.Unscoped().Where("chat_id = ? AND bot_id = ?", chatID, b.botID).Delete(&Message{}).Error
			InfoLogger.Printf("User %d permanently deleted their own chat history in chat %d", currentUserID, chatID)
		} else {
			if targetChatID != 0 {
				// Chat-scoped: delete ALL messages (user + assistant) in the specified chat.
				err = b.db.Unscoped().Where("chat_id = ? AND bot_id = ?", targetChatID, b.botID).Delete(&Message{}).Error
				InfoLogger.Printf("Admin/owner %d permanently deleted chat history for user %d in chat %d", currentUserID, targetUserID, targetChatID)
			} else {
				// Bot-wide: delete all of the user's own messages across every chat, then delete
				// assistant messages from their DM chat (where chat_id == user_id by Telegram convention).
				err = b.db.Unscoped().Where("bot_id = ? AND user_id = ?", b.botID, targetUserID).Delete(&Message{}).Error
				if err == nil {
					err = b.db.Unscoped().Where("chat_id = ? AND bot_id = ? AND is_user = ?", targetUserID, b.botID, false).Delete(&Message{}).Error
				}
				InfoLogger.Printf("Admin/owner %d permanently deleted all chat history for user %d", currentUserID, targetUserID)
			}
		}
	} else {
		// Soft delete messages
		if targetUserID == currentUserID {
			// Own history — delete ALL messages (user + assistant) in the current chat.
			err = b.db.Where("chat_id = ? AND bot_id = ?", chatID, b.botID).Delete(&Message{}).Error
			InfoLogger.Printf("User %d soft deleted their own chat history in chat %d", currentUserID, chatID)
		} else {
			if targetChatID != 0 {
				// Chat-scoped: delete ALL messages (user + assistant) in the specified chat.
				err = b.db.Where("chat_id = ? AND bot_id = ?", targetChatID, b.botID).Delete(&Message{}).Error
				InfoLogger.Printf("Admin/owner %d soft deleted chat history for user %d in chat %d", currentUserID, targetUserID, targetChatID)
			} else {
				// Bot-wide: delete all of the user's own messages across every chat, then delete
				// assistant messages from their DM chat (where chat_id == user_id by Telegram convention).
				err = b.db.Where("bot_id = ? AND user_id = ?", b.botID, targetUserID).Delete(&Message{}).Error
				if err == nil {
					err = b.db.Where("chat_id = ? AND bot_id = ? AND is_user = ?", targetUserID, b.botID, false).Delete(&Message{}).Error
				}
				InfoLogger.Printf("Admin/owner %d soft deleted all chat history for user %d", currentUserID, targetUserID)
			}
		}
	}

	if err != nil {
		ErrorLogger.Printf("Error clearing chat history: %v", err)
		if err := b.sendResponse(ctx, chatID, "Sorry, I couldn't clear the chat history.", businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
		}
		return
	}

	// Evict the relevant in-memory cache entry so the next access rebuilds from
	// the now-clean DB. Applies to all cases: own history, cross-user
	// scoped to a specific chat, and bot-wide cross-user clear.
	b.chatMemoriesMu.Lock()
	if targetUserID == currentUserID {
		// Own history is always scoped to the current chat.
		delete(b.chatMemories, chatID)
	} else if targetChatID != 0 {
		// Admin cleared a specific chat — evict that chat's cache.
		delete(b.chatMemories, targetChatID)
	} else {
		// Bot-wide clear: primary use-case is DMs where chatID == userID.
		delete(b.chatMemories, targetUserID)
	}
	b.chatMemoriesMu.Unlock()

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
