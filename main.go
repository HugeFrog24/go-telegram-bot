package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/joho/godotenv"
	"github.com/liushuangls/go-anthropic/v2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Message represents the structure for storing messages in the database
type Message struct {
	gorm.Model
	ChatID    int64
	UserID    int64
	Username  string
	UserRole  string // New field
	Text      string
	Timestamp time.Time
}

// Bot wraps the Telegram bot, database connection, and Anthropic client
type Bot struct {
	tgBot           *bot.Bot
	db              *gorm.DB
	anthropicClient *anthropic.Client
}

type User struct {
	gorm.Model
	TelegramID int64 `gorm:"uniqueIndex"`
	Username   string
	Role       string
}

func main() {
	// Initialize logger to write to both console and file
	logFile, err := os.OpenFile("bot.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	// Create a multi-writer to write to both stdout and the log file
	mw := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(mw)

	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Printf("Error loading .env file: %v", err)
	}

	// Check for required environment variables
	requiredEnvVars := []string{"TELEGRAM_BOT_TOKEN", "ANTHROPIC_API_KEY"}
	for _, envVar := range requiredEnvVars {
		if os.Getenv(envVar) == "" {
			log.Fatalf("%s environment variable is not set", envVar)
		}
	}

	// Initialize database
	db, err := initDB()
	if err != nil {
		log.Fatalf("Error initializing database: %v", err)
	}

	// Initialize Anthropic client
	anthropicClient := anthropic.NewClient(os.Getenv("ANTHROPIC_API_KEY"))

	// Create Bot instance
	b := &Bot{
		db:              db,
		anthropicClient: anthropicClient,
	}

	// Initialize Telegram bot with the handler
	tgBot, err := initTelegramBot(b.handleUpdate)
	if err != nil {
		log.Fatalf("Error initializing Telegram bot: %v", err)
	}
	b.tgBot = tgBot

	// Set up context with cancellation
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Start the bot
	log.Println("Starting bot...")
	b.tgBot.Start(ctx)
}

func initDB() (*gorm.DB, error) {
	// Use the same logger for GORM
	newLogger := logger.New(
		log.New(log.Writer(), "\r\n", log.LstdFlags), // io writer
		logger.Config{
			SlowThreshold: time.Second,
			LogLevel:      logger.Info,
			Colorful:      false,
		},
	)

	// Initialize GORM with SQLite
	db, err := gorm.Open(sqlite.Open("bot.db"), &gorm.Config{
		Logger: newLogger,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// Auto-migrate the schema
	err = db.AutoMigrate(&Message{}, &User{})
	if err != nil {
		return nil, fmt.Errorf("failed to migrate database schema: %w", err)
	}

	return db, nil
}

func initTelegramBot(handler bot.HandlerFunc) (*bot.Bot, error) {
	// Load .env file
	err := godotenv.Load()
	if err != nil {
		log.Println("Error loading .env file")
	}

	// Get bot token from environment variable
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN environment variable is not set")
	}

	// Create new bot instance with the handler
	b, err := bot.New(token, bot.WithDefaultHandler(handler))
	if err != nil {
		return nil, fmt.Errorf("failed to create bot: %w", err)
	}

	log.Println("Telegram bot initialized successfully")
	return b, nil
}

func (b *Bot) handleUpdate(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return // Ignore non-message updates
	}

	chatID := update.Message.Chat.ID
	userID := update.Message.From.ID
	username := update.Message.From.Username
	text := update.Message.Text

	// Check if user exists, if not create a new user with default role
	var user User
	if err := b.db.Where("telegram_id = ?", userID).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			user = User{
				TelegramID: userID,
				Username:   username,
				Role:       "user", // Default role
			}
			b.db.Create(&user)
		} else {
			log.Printf("Error checking user: %v", err)
			return
		}
	}

	// Prepare response using Anthropic
	var response string
	var err error
	isNewChat := b.isNewChat(chatID)
	if b.isAdminOrOwner(userID) {
		response, err = b.getAnthropicResponse(ctx, text, isNewChat)
	} else {
		response, err = b.getModeratedAnthropicResponse(ctx, text, isNewChat)
	}
	if err != nil {
		log.Printf("Error getting Anthropic response: %v", err)
		response = "I'm sorry, I'm having trouble processing your request right now."
	}

	// Store message in database
	message := Message{
		ChatID:    chatID,
		UserID:    userID,
		Username:  username,
		UserRole:  user.Role,
		Text:      text,
		Timestamp: time.Now(),
	}
	if err := b.db.Create(&message).Error; err != nil {
		log.Printf("Error storing message: %v", err)
	}

	// Send response
	_, err = b.tgBot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   response,
	})
	if err != nil {
		log.Printf("Error sending message: %v", err)
	}
}

// isNewChat checks if this is a new chat for the user
func (b *Bot) isNewChat(chatID int64) bool {
	var count int64
	b.db.Model(&Message{}).Where("chat_id = ?", chatID).Count(&count)
	return count == 0
}

func (b *Bot) isAdminOrOwner(userID int64) bool {
	var user User
	if err := b.db.Where("telegram_id = ?", userID).First(&user).Error; err != nil {
		return false
	}
	return user.Role == "admin" || user.Role == "owner"
}

func (b *Bot) getAnthropicResponse(ctx context.Context, userMessage string, isNewChat bool) (string, error) {
	var systemMessage string
	if isNewChat {
		systemMessage = "You are a helpful AI assistant. Greet the user and respond to their message."
	} else {
		systemMessage = "You are a helpful AI assistant. Respond to the user's message."
	}

	resp, err := b.anthropicClient.CreateMessages(ctx, anthropic.MessagesRequest{
		Model: anthropic.ModelClaudeInstant1Dot2,
		Messages: []anthropic.Message{
			anthropic.NewUserTextMessage(systemMessage),
			anthropic.NewUserTextMessage(userMessage),
		},
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

func (b *Bot) getModeratedAnthropicResponse(ctx context.Context, userMessage string, isNewChat bool) (string, error) {
	var systemMessage string
	if isNewChat {
		systemMessage = "You are a helpful AI assistant. Greet the user and respond to their message. Avoid discussing sensitive topics or providing harmful information."
	} else {
		systemMessage = "You are a helpful AI assistant. Respond to the user's message while avoiding sensitive topics or harmful information."
	}

	resp, err := b.anthropicClient.CreateMessages(ctx, anthropic.MessagesRequest{
		Model: anthropic.ModelClaudeInstant1Dot2,
		Messages: []anthropic.Message{
			anthropic.NewUserTextMessage(systemMessage),
			anthropic.NewUserTextMessage(userMessage),
		},
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
