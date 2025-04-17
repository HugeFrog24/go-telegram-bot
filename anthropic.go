package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/liushuangls/go-anthropic/v2"
)

func (b *Bot) getAnthropicResponse(ctx context.Context, messages []anthropic.Message, isNewChat, isAdminOrOwner, isEmojiOnly bool, username string, firstName string) (string, error) {
	// Use prompts from config
	var systemMessage string
	if isNewChat {
		systemMessage = b.config.SystemPrompts["new_chat"]
	} else {
		systemMessage = b.config.SystemPrompts["continue_conversation"]
	}

	// Combine default prompt with custom instructions
	systemMessage = b.config.SystemPrompts["default"] + " " + b.config.SystemPrompts["custom_instructions"] + " " + systemMessage

	// Handle username placeholder
	usernameValue := username
	if username == "" {
		usernameValue = "unknown" // Use "unknown" when username is not available
	}
	systemMessage = strings.ReplaceAll(systemMessage, "{username}", usernameValue)

	// Handle firstname placeholder
	firstnameValue := firstName
	if firstName == "" {
		firstnameValue = "unknown" // Use "unknown" when first name is not available
	}
	systemMessage = strings.ReplaceAll(systemMessage, "{firstname}", firstnameValue)

	if !isAdminOrOwner {
		systemMessage += " " + b.config.SystemPrompts["avoid_sensitive"]
	}

	if isEmojiOnly {
		systemMessage += " " + b.config.SystemPrompts["respond_with_emojis"]
	}

	// Debug logging
	InfoLogger.Printf("Sending %d messages to Anthropic", len(messages))
	for i, msg := range messages {
		for _, content := range msg.Content {
			if content.Type == anthropic.MessagesContentTypeText {
				InfoLogger.Printf("Message %d: Role=%v, Text=%v", i, msg.Role, content.Text)
			}
		}
	}

	// Ensure the roles are correct
	for i := range messages {
		switch messages[i].Role {
		case anthropic.RoleUser:
			messages[i].Role = anthropic.RoleUser
		case anthropic.RoleAssistant:
			messages[i].Role = anthropic.RoleAssistant
		default:
			// Default to 'user' if role is unrecognized
			messages[i].Role = anthropic.RoleUser
		}
	}

	model := anthropic.Model(b.config.Model)

	resp, err := b.anthropicClient.CreateMessages(ctx, anthropic.MessagesRequest{
		Model:     model, // Now `model` is of type anthropic.Model
		Messages:  messages,
		System:    systemMessage,
		MaxTokens: 1000,
	})
	if err != nil {
		return "", fmt.Errorf("error creating Anthropic message: %w", err)
	}

	if len(resp.Content) == 0 || resp.Content[0].Type != anthropic.MessagesContentTypeText {
		return "", fmt.Errorf("unexpected response format from Anthropic")
	}

	return resp.Content[0].GetText(), nil
}
