package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/liushuangls/go-anthropic/v2"
)

func (b *Bot) getAnthropicResponse(ctx context.Context, messages []anthropic.Message, isNewChat, isAdminOrOwner, isEmojiOnly bool, username string, firstName string, lastName string, isPremium bool, languageCode string, messageTime int) (string, error) {
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

	// Handle lastname placeholder
	lastnameValue := lastName
	if lastName == "" {
		lastnameValue = "" // Empty string when last name is not available
	}
	systemMessage = strings.ReplaceAll(systemMessage, "{lastname}", lastnameValue)

	// Handle language code placeholder
	langValue := languageCode
	if languageCode == "" {
		langValue = "en" // Default to English when language code is not available
	}
	systemMessage = strings.ReplaceAll(systemMessage, "{language}", langValue)

	// Handle premium status
	premiumStatus := "regular user"
	if isPremium {
		premiumStatus = "premium user"
	}
	systemMessage = strings.ReplaceAll(systemMessage, "{premium_status}", premiumStatus)

	// Handle time awareness
	timeObj := time.Unix(int64(messageTime), 0)
	hour := timeObj.Hour()
	var timeContext string
	if hour >= 5 && hour < 12 {
		timeContext = "morning"
	} else if hour >= 12 && hour < 18 {
		timeContext = "afternoon"
	} else if hour >= 18 && hour < 22 {
		timeContext = "evening"
	} else {
		timeContext = "night"
	}
	systemMessage = strings.ReplaceAll(systemMessage, "{time_context}", timeContext)

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

	// Create the request
	request := anthropic.MessagesRequest{
		Model:     model, // Now `model` is of type anthropic.Model
		Messages:  messages,
		System:    systemMessage,
		MaxTokens: 1000,
	}

	// Apply temperature if set in config
	if b.config.Temperature != nil {
		request.Temperature = b.config.Temperature
	}

	resp, err := b.anthropicClient.CreateMessages(ctx, request)
	if err != nil {
		return "", fmt.Errorf("error creating Anthropic message: %w", err)
	}

	if len(resp.Content) == 0 || resp.Content[0].Type != anthropic.MessagesContentTypeText {
		return "", fmt.Errorf("unexpected response format from Anthropic")
	}

	return resp.Content[0].GetText(), nil
}
