package main

import (
	"testing"
	"time"
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

// To ensure thread safety and avoid race conditions during testing,
// you can run the tests with the `-race` flag:
// go test -race -v
