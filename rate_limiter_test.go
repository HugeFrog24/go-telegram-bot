package main

import (
	"testing"
	"time"
)

func TestCheckRateLimits(t *testing.T) {
	mockClock := &MockClock{
		currentTime: time.Date(2023, 10, 1, 0, 0, 0, 0, time.UTC),
	}

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

	bot := &Bot{
		config:       config,
		userLimiters: make(map[int64]*userLimiter),
		clock:        mockClock,
	}

	userID := int64(12345)

	sendMessage := func() bool {
		return bot.checkRateLimits(userID)
	}

	for i := 0; i < config.MessagePerHour; i++ {
		if !sendMessage() {
			t.Errorf("Expected message %d to be allowed", i+1)
		}
	}

	if sendMessage() {
		t.Errorf("Expected message to be denied due to hourly limit exceeded")
	}

	if sendMessage() {
		t.Errorf("Expected message to be denied while user is banned")
	}

	mockClock.Advance(time.Minute)

	mockClock.Advance(time.Hour)

	if !sendMessage() {
		t.Errorf("Expected message to be allowed after ban duration")
	}

	for i := 0; i < config.MessagePerDay-config.MessagePerHour-1; i++ {
		if !sendMessage() {
			t.Errorf("Expected message %d to be allowed towards daily limit", i+1)
		}
	}

	if sendMessage() {
		t.Errorf("Expected message to be denied due to daily limit exceeded")
	}
}
