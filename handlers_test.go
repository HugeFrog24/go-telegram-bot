package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

	// Create test messages for each user.
	// Each user's messages are stored with chat_id == their own user_id, mirroring
	// how Telegram private DMs work (chat_id == user_id in 1-on-1 bot conversations).
	// Using a shared artificial chatID here would mask the cross-user delete bug.
	for _, userID := range []int64{ownerID, adminID, regularUserID} {
		for i := 0; i < 5; i++ {
			message := Message{
				BotID:     b.botID,
				ChatID:    userID, // per-user chat, not a shared chatID
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
		targetChatID   int64
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
		{
			// targetChatID scopes the delete to a specific chat; messages in other chats survive.
			// We seed messages with ChatID == userID (per-user DM), so targeting a different chatID
			// should leave the user's messages untouched (expectedCount == 5).
			name:          "Admin clears regular user's history scoped to non-matching chat",
			currentUserID: adminID,
			targetUserID:  regularUserID,
			targetChatID:  int64(9999), // a chat the user has no messages in
			hardDelete:    false,
			expectedError: false,
			expectedCount: 5, // messages in chat 789 are unaffected
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
			b.clearChatHistory(context.Background(), chatID, tc.currentUserID, tc.targetUserID, tc.targetChatID, tc.businessConnID, tc.hardDelete)

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

func TestStatsCommand(t *testing.T) {
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
			// User message
			userMessage := Message{
				BotID:     b.botID,
				ChatID:    chatID,
				UserID:    userID,
				Username:  "test",
				UserRole:  "user",
				Text:      "Test message",
				Timestamp: time.Now(),
				IsUser:    true,
			}
			err = db.Create(&userMessage).Error
			assert.NoError(t, err)

			// Bot response
			botMessage := Message{
				BotID:     b.botID,
				ChatID:    chatID,
				UserID:    0,
				Username:  "AI Assistant",
				UserRole:  "assistant",
				Text:      "Test response",
				Timestamp: time.Now(),
				IsUser:    false,
			}
			err = db.Create(&botMessage).Error
			assert.NoError(t, err)
		}
	}

	// Test cases
	testCases := []struct {
		name           string
		command        string
		currentUserID  int64
		expectedError  bool
		expectedMsg    string
		businessConnID string
	}{
		{
			name:          "Global stats",
			command:       "/stats",
			currentUserID: regularUserID,
			expectedError: false,
			expectedMsg:   "📊 Bot Statistics:",
		},
		{
			name:          "User requests own stats",
			command:       "/stats user",
			currentUserID: regularUserID,
			expectedError: false,
			expectedMsg:   "👤 User Statistics for @regular (ID: 789):",
		},
		{
			name:          "Admin requests another user's stats",
			command:       "/stats user 789",
			currentUserID: adminID,
			expectedError: false,
			expectedMsg:   "👤 User Statistics for @regular (ID: 789):",
		},
		{
			name:          "Owner requests another user's stats",
			command:       "/stats user 456",
			currentUserID: ownerID,
			expectedError: false,
			expectedMsg:   "👤 User Statistics for @admin (ID: 456):",
		},
		{
			name:          "Regular user attempts to request another user's stats",
			command:       "/stats user 456",
			currentUserID: regularUserID,
			expectedError: true,
			expectedMsg:   "Permission denied. Only admins and owners can view other users' statistics.",
		},
		{
			name:          "User provides invalid user ID format",
			command:       "/stats user abc",
			currentUserID: adminID,
			expectedError: true,
			expectedMsg:   "Invalid user ID format. Usage: /stats user [user_id]",
		},
		{
			name:          "User provides invalid command format",
			command:       "/stats invalid",
			currentUserID: adminID,
			expectedError: true,
			expectedMsg:   "Invalid command format. Usage: /stats or /stats user [user_id]",
		},
		{
			name:          "User requests non-existent user's stats",
			command:       "/stats user 999",
			currentUserID: adminID,
			expectedError: true,
			expectedMsg:   "Sorry, I couldn't retrieve statistics for user ID 999.",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Setup mock response expectations
			var sentMessage string
			mockTgClient.SendMessageFunc = func(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error) {
				sentMessage = params.Text
				return &models.Message{}, nil
			}

			// Create update with command
			update := &models.Update{
				Message: &models.Message{
					Chat: models.Chat{ID: chatID},
					From: &models.User{
						ID:       tc.currentUserID,
						Username: getUsernameByID(tc.currentUserID),
					},
					Text: tc.command,
					Entities: []models.MessageEntity{
						{
							Type:   "bot_command",
							Offset: 0,
							Length: 6, // Length of "/stats"
						},
					},
				},
			}

			// Handle the update
			b.handleUpdate(context.Background(), nil, update)

			// Verify the response message contains the expected text
			assert.Contains(t, sentMessage, tc.expectedMsg)
		})
	}
}

// Helper function to get username by ID for test
func getUsernameByID(id int64) string {
	switch id {
	case 123:
		return "owner"
	case 456:
		return "admin"
	case 789:
		return "regular"
	default:
		return "unknown"
	}
}

func setupTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("Failed to open test database: %v", err)
	}

	// AutoMigrate the models
	err = db.AutoMigrate(&BotModel{}, &ConfigModel{}, &Message{}, &User{}, &Role{}, &Scope{})
	if err != nil {
		t.Fatalf("Failed to migrate database schema: %v", err)
	}

	// Create default roles and scopes
	err = createDefaultRoles(db)
	if err != nil {
		t.Fatalf("Failed to create default roles: %v", err)
	}
	if err := createDefaultScopes(db); err != nil {
		t.Fatalf("Failed to create default scopes: %v", err)
	}

	return db
}

// setupBotForTest creates a minimal Bot instance backed by an in-memory DB.
// It follows the same pattern as the existing handler tests to avoid duplication.
func setupBotForTest(t *testing.T, ownerID int64) (*Bot, *MockTelegramClient) {
	t.Helper()
	db := setupTestDB(t)
	mockClock := &MockClock{currentTime: time.Now()}
	config := BotConfig{
		ID:              "test_bot",
		OwnerTelegramID: ownerID,
		TelegramToken:   "test_token",
		MemorySize:      10,
		MessagePerHour:  5,
		MessagePerDay:   10,
		TempBanDuration: "1h",
		Model:           "claude-3-5-haiku-latest",
		SystemPrompts:   make(map[string]string),
		Active:          true,
	}
	mockTgClient := &MockTelegramClient{}
	botModel := &BotModel{Identifier: config.ID, Name: config.ID}
	assert.NoError(t, db.Create(botModel).Error)
	assert.NoError(t, db.Create(&ConfigModel{
		BotID:           botModel.ID,
		MemorySize:      config.MemorySize,
		MessagePerHour:  config.MessagePerHour,
		MessagePerDay:   config.MessagePerDay,
		TempBanDuration: config.TempBanDuration,
		SystemPrompts:   "{}",
		TelegramToken:   config.TelegramToken,
		Active:          config.Active,
	}).Error)
	b, err := NewBot(db, config, mockClock, mockTgClient)
	assert.NoError(t, err)
	return b, mockTgClient
}

// TestAnthropicErrorResponse verifies that model-deprecation errors surface actionable
// details only to admin/owner, and that regular users and non-model errors always get
// the generic fallback.
func TestAnthropicErrorResponse(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)

	// Create admin user
	adminRole, err := b.getRoleByName("admin")
	assert.NoError(t, err)
	assert.NoError(t, b.db.Create(&User{
		BotID: b.botID, TelegramID: 456, Username: "admin",
		RoleID: adminRole.ID, Role: adminRole,
	}).Error)

	// Create regular user
	userRole, err := b.getRoleByName("user")
	assert.NoError(t, err)
	assert.NoError(t, b.db.Create(&User{
		BotID: b.botID, TelegramID: 789, Username: "regular",
		RoleID: userRole.ID, Role: userRole,
	}).Error)

	modelErr := fmt.Errorf("%w: claude-3-5-haiku-latest", ErrModelNotFound)
	otherErr := errors.New("network error")

	tests := []struct {
		name        string
		err         error
		userID      int64
		wantSubstr  string
		wantMissing string
	}{
		{
			name:       "owner receives actionable model-not-found message",
			err:        modelErr,
			userID:     123,
			wantSubstr: "/set_model",
		},
		{
			name:       "admin receives actionable model-not-found message",
			err:        modelErr,
			userID:     456,
			wantSubstr: "/set_model",
		},
		{
			name:        "regular user receives generic message for model-not-found",
			err:         modelErr,
			userID:      789,
			wantSubstr:  "I'm sorry",
			wantMissing: "/set_model",
		},
		{
			name:        "owner receives generic message for non-model error",
			err:         otherErr,
			userID:      123,
			wantSubstr:  "I'm sorry",
			wantMissing: "/set_model",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := b.anthropicErrorResponse(tc.err, tc.userID)
			assert.Contains(t, resp, tc.wantSubstr)
			if tc.wantMissing != "" {
				assert.NotContains(t, resp, tc.wantMissing)
			}
		})
	}
}

// TestSetModelCommand verifies that /set_model enforces permissions, validates input,
// updates the model in memory, and persists the change to the config file on disk.
func TestSetModelCommand(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, mockTgClient := setupBotForTest(t, 123)

	// Point the config at a temporary file so PersistModel can write to disk.
	tempDir, err := os.MkdirTemp("", "set_model_cmd_test")
	assert.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	configPath := filepath.Join(tempDir, "config.json")
	initialJSON := `{"id":"test_bot","telegram_token":"test_token","model":"claude-3-5-haiku-latest","messages_per_hour":5,"messages_per_day":10}`
	assert.NoError(t, os.WriteFile(configPath, []byte(initialJSON), 0600))
	b.config.ConfigFilePath = configPath

	// Create admin and regular users
	adminRole, err := b.getRoleByName("admin")
	assert.NoError(t, err)
	assert.NoError(t, b.db.Create(&User{
		BotID: b.botID, TelegramID: 456, Username: "admin",
		RoleID: adminRole.ID, Role: adminRole,
	}).Error)
	userRole, err := b.getRoleByName("user")
	assert.NoError(t, err)
	assert.NoError(t, b.db.Create(&User{
		BotID: b.botID, TelegramID: 789, Username: "regular",
		RoleID: userRole.ID, Role: userRole,
	}).Error)

	chatID := int64(1000)

	// Seed chat 1000 with a prior message so isNewChatFlag is false for all subtests.
	// Commands are only processed in the non-new-chat branch of handleUpdate.
	assert.NoError(t, b.db.Create(&Message{
		BotID: b.botID, ChatID: chatID, UserID: 789, Username: "regular",
		UserRole: "user", Text: "hello", IsUser: true,
	}).Error)

	makeUpdate := func(userID int64, text string, cmdLen int) *models.Update {
		return &models.Update{
			Message: &models.Message{
				Chat: models.Chat{ID: chatID},
				From: &models.User{ID: userID, Username: getUsernameByID(userID)},
				Text: text,
				Entities: []models.MessageEntity{
					{Type: "bot_command", Offset: 0, Length: cmdLen},
				},
			},
		}
	}

	tests := []struct {
		name       string
		userID     int64
		text       string
		wantSubstr string
	}{
		{
			name:       "regular user is denied",
			userID:     789,
			text:       "/set_model claude-sonnet-4-6",
			wantSubstr: "Permission denied",
		},
		{
			name:       "admin missing argument shows usage",
			userID:     456,
			text:       "/set_model",
			wantSubstr: "Usage:",
		},
		{
			name:       "owner missing argument shows usage",
			userID:     123,
			text:       "/set_model",
			wantSubstr: "Usage:",
		},
		{
			name:       "admin sets model successfully",
			userID:     456,
			text:       "/set_model claude-sonnet-4-6",
			wantSubstr: "✅",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var sentMessage string
			mockTgClient.SendMessageFunc = func(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error) {
				sentMessage = params.Text
				return &models.Message{}, nil
			}
			b.handleUpdate(context.Background(), nil, makeUpdate(tc.userID, tc.text, 10))
			assert.Contains(t, sentMessage, tc.wantSubstr)
		})
	}

	// Verify the successful update took effect in memory and on disk.
	t.Run("model change persisted in memory and on disk", func(t *testing.T) {
		assert.Equal(t, "claude-sonnet-4-6", string(b.config.Model))
		data, err := os.ReadFile(configPath)
		assert.NoError(t, err)
		assert.Contains(t, string(data), `"claude-sonnet-4-6"`)
	})
}

// TestHasScope verifies that scope checks honour role assignments and the owner bypass.
func TestHasScope(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	const ownerID int64 = 100
	b, _ := setupBotForTest(t, ownerID)

	// Admin user
	adminRole, err := b.getRoleByName("admin")
	assert.NoError(t, err)
	assert.NoError(t, b.db.Create(&User{
		BotID: b.botID, TelegramID: 200, Username: "admin_user",
		RoleID: adminRole.ID, Role: adminRole,
	}).Error)

	// Regular user
	userRole, err := b.getRoleByName("user")
	assert.NoError(t, err)
	assert.NoError(t, b.db.Create(&User{
		BotID: b.botID, TelegramID: 300, Username: "regular_user",
		RoleID: userRole.ID, Role: userRole,
	}).Error)

	tests := []struct {
		name   string
		userID int64
		scope  string
		want   bool
	}{
		{"owner bypass: model:set", ownerID, ScopeModelSet, true},
		{"owner bypass: stats:view:any", ownerID, ScopeStatsViewAny, true},
		{"admin: model:set", 200, ScopeModelSet, true},
		{"admin: stats:view:any", 200, ScopeStatsViewAny, true},
		{"admin: history:clear:any", 200, ScopeHistoryClearAny, true},
		{"user: model:set denied", 300, ScopeModelSet, false},
		{"user: stats:view:any denied", 300, ScopeStatsViewAny, false},
		{"user: history:clear:any denied", 300, ScopeHistoryClearAny, false},
		{"user: stats:view:own allowed", 300, ScopeStatsViewOwn, true},
		{"user: history:clear:own allowed", 300, ScopeHistoryClearOwn, true},
		{"unknown telegram_id", 999, ScopeModelSet, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, b.hasScope(tc.userID, tc.scope))
		})
	}
}
