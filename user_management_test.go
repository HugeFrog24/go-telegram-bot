package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestOwnerAssignment(t *testing.T) {
	// Initialize loggers
	initLoggers()

	// Initialize in-memory database for testing
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("Failed to open in-memory database: %v", err)
	}

	// Migrate the schema
	err = db.AutoMigrate(&BotModel{}, &ConfigModel{}, &Message{}, &User{}, &Role{})
	if err != nil {
		t.Fatalf("Failed to migrate database schema: %v", err)
	}

	// Create default roles
	err = createDefaultRoles(db)
	if err != nil {
		t.Fatalf("Failed to create default roles: %v", err)
	}

	// Create a bot configuration
	config := BotConfig{
		ID:              "test_bot",
		TelegramToken:   "TEST_TELEGRAM_TOKEN",
		MemorySize:      10,
		MessagePerHour:  5,
		MessagePerDay:   10,
		TempBanDuration: "1m",
		SystemPrompts:   make(map[string]string),
		Active:          true,
		OwnerTelegramID: 111111111,
	}

	// Initialize MockClock
	mockClock := &MockClock{
		currentTime: time.Now(),
	}

	// Initialize MockTelegramClient
	mockTGClient := &MockTelegramClient{
		SendMessageFunc: func(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error) {
			chatID, ok := params.ChatID.(int64)
			if !ok {
				return nil, fmt.Errorf("ChatID is not of type int64")
			}
			// Simulate successful message sending
			return &models.Message{ID: 1, Chat: models.Chat{ID: chatID}}, nil
		},
	}

	// Create the bot with the mock Telegram client
	bot, err := NewBot(db, config, mockClock, mockTGClient)
	if err != nil {
		t.Fatalf("Failed to create bot: %v", err)
	}

	// Verify that the owner exists
	var owner User
	err = db.Where("telegram_id = ? AND bot_id = ? AND is_owner = ?", config.OwnerTelegramID, bot.botID, true).First(&owner).Error
	if err != nil {
		t.Fatalf("Owner was not created: %v", err)
	}

	// Attempt to create another owner for the same bot
	_, err = bot.getOrCreateUser(222222222, "AnotherOwner", true)
	if err == nil {
		t.Fatalf("Expected error when creating a second owner, but got none")
	}

	// Verify that the error message is appropriate
	expectedErrorMsg := "an owner already exists for this bot"
	if err.Error() != expectedErrorMsg {
		t.Fatalf("Unexpected error message: %v", err)
	}

	// Assign admin role to a new user
	regularUser, err := bot.getOrCreateUser(333333333, "RegularUser", false)
	if err != nil {
		t.Fatalf("Failed to create regular user: %v", err)
	}

	if regularUser.Role.Name != "user" {
		t.Fatalf("Expected role 'user', got '%s'", regularUser.Role.Name)
	}

	// Attempt to change an existing user to owner
	_, err = bot.getOrCreateUser(333333333, "AdminUser", true)
	if err == nil {
		t.Fatalf("Expected error when changing existing user to owner, but got none")
	}

	expectedErrorMsg = "cannot change existing user to owner"
	if err.Error() != expectedErrorMsg {
		t.Fatalf("Unexpected error message: %v", err)
	}

	// If you need to test admin creation, you should do it through a separate admin creation function
	// or by updating an existing user's role with proper authorization checks
}

func TestPromoteUserToAdmin(t *testing.T) {
	// Initialize loggers
	initLoggers()

	// Initialize in-memory database for testing
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("Failed to open in-memory database: %v", err)
	}

	// Migrate the schema
	err = db.AutoMigrate(&BotModel{}, &ConfigModel{}, &Message{}, &User{}, &Role{})
	if err != nil {
		t.Fatalf("Failed to migrate database schema: %v", err)
	}

	// Create default roles
	err = createDefaultRoles(db)
	if err != nil {
		t.Fatalf("Failed to create default roles: %v", err)
	}

	config := BotConfig{
		ID:              "test_bot",
		TelegramToken:   "TEST_TELEGRAM_TOKEN",
		MemorySize:      10,
		MessagePerHour:  5,
		MessagePerDay:   10,
		TempBanDuration: "1m",
		SystemPrompts:   make(map[string]string),
		Active:          true,
		OwnerTelegramID: 111111111,
	}

	mockClock := &MockClock{currentTime: time.Now()}
	mockTGClient := &MockTelegramClient{}

	bot, err := NewBot(db, config, mockClock, mockTGClient)
	if err != nil {
		t.Fatalf("Failed to create bot: %v", err)
	}

	// Create an owner
	owner, err := bot.getOrCreateUser(config.OwnerTelegramID, "OwnerUser", true)
	if err != nil {
		t.Fatalf("Failed to create owner: %v", err)
	}

	// Test promoting a user to admin
	regularUser, err := bot.getOrCreateUser(444444444, "RegularUser", false)
	if err != nil {
		t.Fatalf("Failed to create regular user: %v", err)
	}

	err = bot.promoteUserToAdmin(owner.TelegramID, regularUser.TelegramID)
	if err != nil {
		t.Fatalf("Failed to promote user to admin: %v", err)
	}

	// Refresh user data
	promotedUser, err := bot.getOrCreateUser(444444444, "RegularUser", false)
	if err != nil {
		t.Fatalf("Failed to get promoted user: %v", err)
	}

	if promotedUser.Role.Name != "admin" {
		t.Fatalf("Expected role 'admin', got '%s'", promotedUser.Role.Name)
	}
}

// TestGetOrCreateUser tests the getOrCreateUser method of the Bot.
// It verifies that a new user is created when one does not exist,
// and an existing user is returned when one does exist.
func TestGetOrCreateUser(t *testing.T) {
	// Initialize loggers
	initLoggers()

	// Initialize in-memory database for testing
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("Failed to open in-memory database: %v", err)
	}

	// Migrate the schema
	err = db.AutoMigrate(&BotModel{}, &ConfigModel{}, &Message{}, &User{}, &Role{})
	if err != nil {
		t.Fatalf("Failed to migrate database schema: %v", err)
	}

	// Create default roles
	err = createDefaultRoles(db)
	if err != nil {
		t.Fatalf("Failed to create default roles: %v", err)
	}

	// Create a mock clock starting at a fixed time
	mockClock := &MockClock{
		currentTime: time.Date(2023, 10, 1, 0, 0, 0, 0, time.UTC),
	}

	// Create a mock configuration
	config := BotConfig{
		ID:              "bot1",
		MemorySize:      10,
		MessagePerHour:  5,
		MessagePerDay:   10,
		TempBanDuration: "1m",
		SystemPrompts:   make(map[string]string),
		TelegramToken:   "YOUR_TELEGRAM_BOT_TOKEN",
		OwnerTelegramID: 123456789,
	}

	// Initialize MockTelegramClient
	mockTGClient := &MockTelegramClient{
		SendMessageFunc: func(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error) {
			chatID, ok := params.ChatID.(int64)
			if !ok {
				return nil, fmt.Errorf("ChatID is not of type int64")
			}
			// Simulate successful message sending
			return &models.Message{ID: 1, Chat: models.Chat{ID: chatID}}, nil
		},
	}

	// Create the bot with the mock Telegram client
	bot, err := NewBot(db, config, mockClock, mockTGClient)
	if err != nil {
		t.Fatalf("Failed to create bot: %v", err)
	}

	// Verify that the owner exists
	var owner User
	err = db.Where("telegram_id = ? AND bot_id = ? AND is_owner = ?", config.OwnerTelegramID, bot.botID, true).First(&owner).Error
	if err != nil {
		t.Fatalf("Owner was not created: %v", err)
	}

	// Attempt to create another owner for the same bot
	_, err = bot.getOrCreateUser(222222222, "AnotherOwner", true)
	if err == nil {
		t.Fatalf("Expected error when creating a second owner, but got none")
	}

	// Create a new user
	newUser, err := bot.getOrCreateUser(987654321, "TestUser", false)
	if err != nil {
		t.Fatalf("Failed to create a new user: %v", err)
	}

	// Verify that the new user was created
	var userInDB User
	err = db.Where("telegram_id = ?", newUser.TelegramID).First(&userInDB).Error
	if err != nil {
		t.Fatalf("New user was not created in the database: %v", err)
	}

	// Get the existing user
	existingUser, err := bot.getOrCreateUser(987654321, "TestUser", false)
	if err != nil {
		t.Fatalf("Failed to get existing user: %v", err)
	}

	// Verify that the existing user is the same as the new user
	if existingUser.ID != userInDB.ID {
		t.Fatalf("Expected to get the existing user, but got a different user")
	}
}

// To ensure thread safety and avoid race conditions during testing,
// you can run the tests with the `-race` flag:
// go test -race -v
