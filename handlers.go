package main

import (
	"context"
	"log"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func (b *Bot) handleUpdate(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}

	chatID := update.Message.Chat.ID
	userID := update.Message.From.ID

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

	assistantMessage := b.createMessage(chatID, 0, "Assistant", "assistant", response, false)
	b.storeMessage(assistantMessage)
	b.addMessageToChatMemory(chatMemory, assistantMessage)
}

func (b *Bot) sendRateLimitExceededMessage(ctx context.Context, chatID int64) {
	_, err := b.tgBot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   "Rate limit exceeded. Please try again later.",
	})
	if err != nil {
		log.Printf("Error sending rate limit message: %v", err)
	}
}

func (b *Bot) sendResponse(ctx context.Context, chatID int64, text string) {
	_, err := b.tgBot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	})
	if err != nil {
		log.Printf("Error sending message: %v", err)
	}
}
