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

func TestClearChatHistory(t *testing.T) {
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

	// Create test users
	ownerID := int64(123)
	adminID := int64(456)
	regularUserID := int64(789)
	nonExistentUserID := int64(999)
	chatID := int64(1000)

	// Create admin role
	adminRole, err := b.getRoleByName("admin")
	assert.NoError(t, err)

	// Create admin user
	adminUser := User{
		BotID:      b.botID,
		TelegramID: adminID,
		Username:   "admin",
		RoleID:     adminRole.ID,
		Role:       adminRole,
		IsOwner:    false,
	}
	err = db.Create(&adminUser).Error
	assert.NoError(t, err)

	// Create regular user
	regularRole, err := b.getRoleByName("user")
	assert.NoError(t, err)
	regularUser := User{
		BotID:      b.botID,
		TelegramID: regularUserID,
		Username:   "regular",
		RoleID:     regularRole.ID,
		Role:       regularRole,
		IsOwner:    false,
	}
	err = db.Create(&regularUser).Error
	assert.NoError(t, err)

	// Create test messages for each user
	for _, userID := range []int64{ownerID, adminID, regularUserID} {
		for i := 0; i < 5; i++ {
			message := Message{
				BotID:     b.botID,
				ChatID:    chatID,
				UserID:    userID,
				Username:  "test",
				UserRole:  "user",
				Text:      "Test message",
				Timestamp: time.Now(),
				IsUser:    true,
			}
			err = db.Create(&message).Error
			assert.NoError(t, err)
		}
	}

	// Test cases
	testCases := []struct {
		name           string
		currentUserID  int64
		targetUserID   int64
		hardDelete     bool
		expectedError  bool
		expectedCount  int64
		expectedMsg    string
		businessConnID string
	}{
		{
			name:          "Owner clears own history",
			currentUserID: ownerID,
			targetUserID:  ownerID,
			hardDelete:    false,
			expectedError: false,
			expectedCount: 0,
			expectedMsg:   "Your chat history has been cleared.",
		},
		{
			name:          "Admin clears own history",
			currentUserID: adminID,
			targetUserID:  adminID,
			hardDelete:    false,
			expectedError: false,
			expectedCount: 0,
			expectedMsg:   "Your chat history has been cleared.",
		},
		{
			name:          "Regular user clears own history",
			currentUserID: regularUserID,
			targetUserID:  regularUserID,
			hardDelete:    false,
			expectedError: false,
			expectedCount: 0,
			expectedMsg:   "Your chat history has been cleared.",
		},
		{
			name:          "Owner clears admin's history",
			currentUserID: ownerID,
			targetUserID:  adminID,
			hardDelete:    false,
			expectedError: false,
			expectedCount: 0,
			expectedMsg:   "Chat history for user @admin (ID: 456) has been cleared.",
		},
		{
			name:          "Admin clears regular user's history",
			currentUserID: adminID,
			targetUserID:  regularUserID,
			hardDelete:    false,
			expectedError: false,
			expectedCount: 0,
			expectedMsg:   "Chat history for user @regular (ID: 789) has been cleared.",
		},
		{
			name:          "Regular user attempts to clear admin's history",
			currentUserID: regularUserID,
			targetUserID:  adminID,
			hardDelete:    false,
			expectedError: true,
			expectedCount: 5, // Messages should remain
			expectedMsg:   "Permission denied. Only admins and owners can clear other users' histories.",
		},
		{
			name:          "Admin attempts to clear non-existent user's history",
			currentUserID: adminID,
			targetUserID:  nonExistentUserID,
			hardDelete:    false,
			expectedError: true,
			expectedCount: 5, // Messages should remain for admin
			expectedMsg:   "User with ID 999 not found.",
		},
		{
			name:          "Owner hard deletes regular user's history",
			currentUserID: ownerID,
			targetUserID:  regularUserID,
			hardDelete:    true,
			expectedError: false,
			expectedCount: 0,
			expectedMsg:   "Chat history for user @regular (ID: 789) has been cleared.",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset messages for the test case
			if tc.name != "Owner hard deletes regular user's history" {
				// Delete all messages for the target user
				err = db.Where("user_id = ?", tc.targetUserID).Delete(&Message{}).Error
				assert.NoError(t, err)

				// Recreate messages for the target user
				for i := 0; i < 5; i++ {
					message := Message{
						BotID:     b.botID,
						ChatID:    chatID,
						UserID:    tc.targetUserID,
						Username:  "test",
						UserRole:  "user",
						Text:      "Test message",
						Timestamp: time.Now(),
						IsUser:    true,
					}
					err = db.Create(&message).Error
					assert.NoError(t, err)
				}
			}

			// Setup mock response expectations
			var sentMessage string
			mockTgClient.SendMessageFunc = func(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error) {
				sentMessage = params.Text
				return &models.Message{}, nil
			}

			// Call the clearChatHistory method
			b.clearChatHistory(context.Background(), chatID, tc.currentUserID, tc.targetUserID, tc.businessConnID, tc.hardDelete)

			// Verify the response message
			assert.Equal(t, tc.expectedMsg, sentMessage)

			// Count remaining messages for the target user
			var count int64
			if tc.hardDelete {
				db.Unscoped().Model(&Message{}).Where("user_id = ? AND chat_id = ?", tc.targetUserID, chatID).Count(&count)
			} else {
				db.Model(&Message{}).Where("user_id = ? AND chat_id = ?", tc.targetUserID, chatID).Count(&count)
			}
			assert.Equal(t, tc.expectedCount, count)
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
