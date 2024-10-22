package main

import (
	"context"
	"fmt"

	"github.com/liushuangls/go-anthropic/v2"
)

func (b *Bot) getAnthropicResponse(ctx context.Context, messages []anthropic.Message, isNewChat, isAdminOrOwner, isEmojiOnly bool) (string, error) {
	// Use prompts from config
	var systemMessage string
	if isNewChat {
		systemMessage = b.config.SystemPrompts["new_chat"]
	} else {
		systemMessage = b.config.SystemPrompts["continue_conversation"]
	}

	// Combine default prompt with custom instructions
	systemMessage = b.config.SystemPrompts["default"] + " " + b.config.SystemPrompts["custom_instructions"] + " " + systemMessage

	if !isAdminOrOwner {
		systemMessage += " " + b.config.SystemPrompts["avoid_sensitive"]
	}

	if isEmojiOnly {
		systemMessage += " " + b.config.SystemPrompts["respond_with_emojis"]
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
