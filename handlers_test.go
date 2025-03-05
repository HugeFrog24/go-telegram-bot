package main

import (
	"context"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestHandleUpdate_NewChat(t *testing.T) {
	// Setup
	db := setupTestDB(t)
	mockClock := &MockClock{
		currentTime: time.Now(),
	}

	config := BotConfig{
		ID:              "test_bot",
		OwnerTelegramID: 123, // owner's ID
		TelegramToken:   "test_token",
		MemorySize:      10,
		MessagePerHour:  5,
		MessagePerDay:   10,
		TempBanDuration: "1h",
		SystemPrompts:   make(map[string]string),
		Active:          true,
	}

	mockTgClient := &MockTelegramClient{}

	// Create bot model first
	botModel := &BotModel{
		Identifier: config.ID,
		Name:       config.ID,
	}
	err := db.Create(botModel).Error
	assert.NoError(t, err)

	// Create bot config
	configModel := &ConfigModel{
		BotID:           botModel.ID,
		MemorySize:      config.MemorySize,
		MessagePerHour:  config.MessagePerHour,
		MessagePerDay:   config.MessagePerDay,
		TempBanDuration: config.TempBanDuration,
		SystemPrompts:   "{}",
		TelegramToken:   config.TelegramToken,
		Active:          config.Active,
	}
	err = db.Create(configModel).Error
	assert.NoError(t, err)

	// Create bot instance
	b, err := NewBot(db, config, mockClock, mockTgClient)
	assert.NoError(t, err)

	testCases := []struct {
		name     string
		userID   int64
		isOwner  bool
		wantResp string
	}{
		{
			name:     "Owner First Message",
			userID:   123, // owner's ID
			isOwner:  true,
			wantResp: "I'm sorry, I'm having trouble processing your request right now.",
		},
		{
			name:     "Regular User First Message",
			userID:   456,
			isOwner:  false,
			wantResp: "I'm sorry, I'm having trouble processing your request right now.",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Setup mock response expectations for error case to test fallback messages
			mockTgClient.SendMessageFunc = func(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error) {
				assert.Equal(t, tc.userID, params.ChatID)
				assert.Equal(t, tc.wantResp, params.Text)
				return &models.Message{}, nil
			}

			// Create update with new message
			update := &models.Update{
				Message: &models.Message{
					Chat: models.Chat{ID: tc.userID},
					From: &models.User{
						ID:       tc.userID,
						Username: "testuser",
					},
					Text: "Hello",
				},
			}

			// Handle the update
			b.handleUpdate(context.Background(), nil, update)

			// Verify message was stored
			var storedMsg Message
			err := db.Where("chat_id = ? AND user_id = ? AND text = ?", tc.userID, tc.userID, "Hello").First(&storedMsg).Error
			assert.NoError(t, err)

			// Verify response was stored
			var respMsg Message
			err = db.Where("chat_id = ? AND is_user = ? AND text = ?", tc.userID, false, tc.wantResp).First(&respMsg).Error
			assert.NoError(t, err)
		})
	}
}

func setupTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("Failed to open test database: %v", err)
	}

	// AutoMigrate the models
	err = db.AutoMigrate(&BotModel{}, &ConfigModel{}, &Message{}, &User{}, &Role{})
	if err != nil {
		t.Fatalf("Failed to migrate database schema: %v", err)
	}

	// Create default roles
	err = createDefaultRoles(db)
	if err != nil {
		t.Fatalf("Failed to create default roles: %v", err)
	}

	return db
}
