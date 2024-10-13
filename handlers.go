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
	if update.Message == nil {
		return
	}

	chatID := update.Message.Chat.ID
	userID := update.Message.From.ID

	// Check if the message is a command
	if update.Message.Entities != nil {
		for _, entity := range update.Message.Entities {
			if entity.Type == "bot_command" {
				command := strings.TrimSpace(update.Message.Text[entity.Offset : entity.Offset+entity.Length])
				switch command {
				case "/stats":
					b.sendStats(ctx, chatID)
					return
				}
			}
		}
	}

	// Existing rate limit and message handling
	if !b.checkRateLimits(userID) {
		b.sendRateLimitExceededMessage(ctx, chatID)
		return
	}

	username := update.Message.From.Username
	text := update.Message.Text

	user, err := b.getOrCreateUser(userID, username)
	if err != nil {
		log.Printf("Error getting or creating user: %v", err)
		return
	}

	userMessage := b.createMessage(chatID, userID, username, user.Role.Name, text, true)
	userMessage.UserRole = string(anthropic.RoleUser) // Convert to string
	b.storeMessage(userMessage)

	chatMemory := b.getOrCreateChatMemory(chatID)
	b.addMessageToChatMemory(chatMemory, userMessage)

	contextMessages := b.prepareContextMessages(chatMemory)

	response, err := b.getAnthropicResponse(ctx, contextMessages, b.isNewChat(chatID), b.isAdminOrOwner(userID))
	if err != nil {
		log.Printf("Error getting Anthropic response: %v", err)
		response = "I'm sorry, I'm having trouble processing your request right now."
	}

	b.sendResponse(ctx, chatID, response)

	assistantMessage := b.createMessage(chatID, 0, "", string(anthropic.RoleAssistant), response, false)
	b.storeMessage(assistantMessage)
	b.addMessageToChatMemory(chatMemory, assistantMessage)
}

func (b *Bot) sendRateLimitExceededMessage(ctx context.Context, chatID int64) {
	b.sendResponse(ctx, chatID, "Rate limit exceeded. Please try again later.")
}
