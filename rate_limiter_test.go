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

// TestCheckRateLimits tests the checkRateLimits method of the Bot.
// It verifies that users are allowed or denied based on their message rates.
func TestCheckRateLimits(t *testing.T) {
	// Create a mock clock starting at a fixed time
	mockClock := &MockClock{
		currentTime: time.Date(2023, 10, 1, 0, 0, 0, 0, time.UTC),
	}

	// Create a mock configuration with reduced timeframes for testing
	config := BotConfig{
		ID:              "bot1",
		MemorySize:      10,
		MessagePerHour:  5,    // Allow 5 messages per hour
		MessagePerDay:   10,   // Allow 10 messages per day
		TempBanDuration: "1m", // Temporary ban duration of 1 minute for testing
		SystemPrompts:   make(map[string]string),
		TelegramToken:   "YOUR_TELEGRAM_BOT_TOKEN",
		OwnerTelegramID: 123456789,
	}

	// Initialize the Bot with mock data and MockClock
	bot := &Bot{
		config:       config,
		userLimiters: make(map[int64]*userLimiter),
		clock:        mockClock,
	}

	userID := int64(12345)

	// Helper function to simulate message sending
	sendMessage := func() bool {
		return bot.checkRateLimits(userID)
	}

	// Send 5 messages within the hourly limit
	for i := 0; i < config.MessagePerHour; i++ {
		if !sendMessage() {
			t.Errorf("Expected message %d to be allowed", i+1)
		}
	}

	// 6th message should exceed the hourly limit and trigger a ban
	if sendMessage() {
		t.Errorf("Expected message to be denied due to hourly limit exceeded")
	}

	// Attempt to send another message immediately, should still be banned
	if sendMessage() {
		t.Errorf("Expected message to be denied while user is banned")
	}

	// Fast-forward time by TempBanDuration to lift the ban
	mockClock.Advance(time.Minute) // Banned for 1 minute

	// Advance time to allow hourly limiter to replenish
	mockClock.Advance(time.Hour) // Advance by 1 hour

	// Send another message, should be allowed now
	if !sendMessage() {
		t.Errorf("Expected message to be allowed after ban duration")
	}

	// Send additional messages to reach the daily limit
	for i := 0; i < config.MessagePerDay-config.MessagePerHour-1; i++ {
		if !sendMessage() {
			t.Errorf("Expected message %d to be allowed towards daily limit", i+1)
		}
	}

	// Attempt to exceed the daily limit
	if sendMessage() {
		t.Errorf("Expected message to be denied due to daily limit exceeded")
	}
}

func TestOwnerAssignment(t *testing.T) {
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
	adminUser, err := bot.getOrCreateUser(333333333, "AdminUser", false)
	if err != nil {
		t.Fatalf("Failed to create admin user: %v", err)
	}

	if adminUser.Role.Name != "admin" {
		t.Fatalf("Expected role 'admin', got '%s'", adminUser.Role.Name)
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
}

// To ensure thread safety and avoid race conditions during testing,
// you can run the tests with the `-race` flag:
// go test -race -v
