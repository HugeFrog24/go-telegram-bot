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

const (
	errOpenDB        = "Failed to open in-memory database: %v"
	errMigrateSchema = "Failed to migrate database schema: %v"
	errCreateRoles   = "Failed to create default roles: %v"
	errCreateScopes  = "Failed to create default scopes: %v"
	errCreateBot     = "Failed to create bot: %v"
	memoryDSN        = ":memory:"
)

func TestOwnerAssignment(t *testing.T) {
	initLoggers()

	db, err := gorm.Open(sqlite.Open(memoryDSN), &gorm.Config{})
	if err != nil {
		t.Fatalf(errOpenDB, err)
	}

	err = db.AutoMigrate(&BotModel{}, &ConfigModel{}, &Message{}, &User{}, &Role{}, &Scope{})
	if err != nil {
		t.Fatalf(errMigrateSchema, err)
	}

	err = createDefaultRoles(db)
	if err != nil {
		t.Fatalf(errCreateRoles, err)
	}
	if err := createDefaultScopes(db); err != nil {
		t.Fatalf(errCreateScopes, err)
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

	mockClock := &MockClock{
		currentTime: time.Now(),
	}

	mockTGClient := &MockTelegramClient{
		SendMessageFunc: func(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error) {
			chatID, ok := params.ChatID.(int64)
			if !ok {
				return nil, fmt.Errorf("ChatID is not of type int64")
			}
			return &models.Message{ID: 1, Chat: models.Chat{ID: chatID}}, nil
		},
	}

	bot, err := NewBot(db, config, mockClock, mockTGClient)
	if err != nil {
		t.Fatalf(errCreateBot, err)
	}

	var owner User
	err = db.Where("telegram_id = ? AND bot_id = ? AND is_owner = ?", config.OwnerTelegramID, bot.botID, true).First(&owner).Error
	if err != nil {
		t.Fatalf("Owner was not created: %v", err)
	}

	_, err = bot.getOrCreateUser(222222222, "AnotherOwner", true)
	if err == nil {
		t.Fatalf("Expected error when creating a second owner, but got none")
	}

	expectedErrorMsg := "an owner already exists for this bot"
	if err.Error() != expectedErrorMsg {
		t.Fatalf("Unexpected error message: %v", err)
	}

	regularUser, err := bot.getOrCreateUser(333333333, "RegularUser", false)
	if err != nil {
		t.Fatalf("Failed to create regular user: %v", err)
	}

	if regularUser.Role.Name != "user" {
		t.Fatalf("Expected role 'user', got '%s'", regularUser.Role.Name)
	}

	_, err = bot.getOrCreateUser(333333333, "AdminUser", true)
	if err == nil {
		t.Fatalf("Expected error when changing existing user to owner, but got none")
	}

	expectedErrorMsg = "cannot change existing user to owner"
	if err.Error() != expectedErrorMsg {
		t.Fatalf("Unexpected error message: %v", err)
	}

}

func TestPromoteUserToAdmin(t *testing.T) {
	initLoggers()

	db, err := gorm.Open(sqlite.Open(memoryDSN), &gorm.Config{})
	if err != nil {
		t.Fatalf(errOpenDB, err)
	}

	err = db.AutoMigrate(&BotModel{}, &ConfigModel{}, &Message{}, &User{}, &Role{}, &Scope{})
	if err != nil {
		t.Fatalf(errMigrateSchema, err)
	}

	err = createDefaultRoles(db)
	if err != nil {
		t.Fatalf(errCreateRoles, err)
	}
	if err := createDefaultScopes(db); err != nil {
		t.Fatalf(errCreateScopes, err)
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
		t.Fatalf(errCreateBot, err)
	}

	owner, err := bot.getOrCreateUser(config.OwnerTelegramID, "OwnerUser", true)
	if err != nil {
		t.Fatalf("Failed to create owner: %v", err)
	}

	regularUser, err := bot.getOrCreateUser(444444444, "RegularUser", false)
	if err != nil {
		t.Fatalf("Failed to create regular user: %v", err)
	}

	err = bot.promoteUserToAdmin(owner.TelegramID, regularUser.TelegramID)
	if err != nil {
		t.Fatalf("Failed to promote user to admin: %v", err)
	}

	promotedUser, err := bot.getOrCreateUser(444444444, "RegularUser", false)
	if err != nil {
		t.Fatalf("Failed to get promoted user: %v", err)
	}

	if promotedUser.Role.Name != "admin" {
		t.Fatalf("Expected role 'admin', got '%s'", promotedUser.Role.Name)
	}
}

func TestGetOrCreateUser(t *testing.T) {
	initLoggers()

	db, err := gorm.Open(sqlite.Open(memoryDSN), &gorm.Config{})
	if err != nil {
		t.Fatalf(errOpenDB, err)
	}

	err = db.AutoMigrate(&BotModel{}, &ConfigModel{}, &Message{}, &User{}, &Role{}, &Scope{})
	if err != nil {
		t.Fatalf(errMigrateSchema, err)
	}

	err = createDefaultRoles(db)
	if err != nil {
		t.Fatalf(errCreateRoles, err)
	}
	if err := createDefaultScopes(db); err != nil {
		t.Fatalf(errCreateScopes, err)
	}

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

	mockTGClient := &MockTelegramClient{
		SendMessageFunc: func(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error) {
			chatID, ok := params.ChatID.(int64)
			if !ok {
				return nil, fmt.Errorf("ChatID is not of type int64")
			}
			return &models.Message{ID: 1, Chat: models.Chat{ID: chatID}}, nil
		},
	}

	bot, err := NewBot(db, config, mockClock, mockTGClient)
	if err != nil {
		t.Fatalf(errCreateBot, err)
	}

	var owner User
	err = db.Where("telegram_id = ? AND bot_id = ? AND is_owner = ?", config.OwnerTelegramID, bot.botID, true).First(&owner).Error
	if err != nil {
		t.Fatalf("Owner was not created: %v", err)
	}

	_, err = bot.getOrCreateUser(222222222, "AnotherOwner", true)
	if err == nil {
		t.Fatalf("Expected error when creating a second owner, but got none")
	}

	newUser, err := bot.getOrCreateUser(987654321, "TestUser", false)
	if err != nil {
		t.Fatalf("Failed to create a new user: %v", err)
	}

	var userInDB User
	err = db.Where("telegram_id = ?", newUser.TelegramID).First(&userInDB).Error
	if err != nil {
		t.Fatalf("New user was not created in the database: %v", err)
	}

	existingUser, err := bot.getOrCreateUser(987654321, "TestUser", false)
	if err != nil {
		t.Fatalf("Failed to get existing user: %v", err)
	}

	if existingUser.ID != userInDB.ID {
		t.Fatalf("Expected to get the existing user, but got a different user")
	}
}
