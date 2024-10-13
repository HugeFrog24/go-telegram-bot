package main

import (
	"context"
	"fmt"

	"github.com/liushuangls/go-anthropic/v2"
)

func (b *Bot) getAnthropicResponse(ctx context.Context, messages []anthropic.Message, isNewChat, isAdminOrOwner bool) (string, error) {
	var systemMessage string
	if isNewChat {
		systemMessage = "You are a helpful AI assistant."
	} else {
		systemMessage = "Continue the conversation."
	}

	if !isAdminOrOwner {
		systemMessage += " Avoid discussing sensitive topics or providing harmful information."
	}

	// Ensure the roles are correct
	for i := range messages {
		if messages[i].Role == "user" {
			messages[i].Role = anthropic.RoleUser
		} else if messages[i].Role == "assistant" {
			messages[i].Role = anthropic.RoleAssistant
		}
	}

	model := anthropic.ModelClaude3Dot5Sonnet20240620
	if !isAdminOrOwner {
		model = anthropic.ModelClaudeInstant1Dot2
	}

	resp, err := b.anthropicClient.CreateMessages(ctx, anthropic.MessagesRequest{
		Model:     model,
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
