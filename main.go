package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/signal"

	"github.com/joho/godotenv"
)

func main() {
	// Initialize logger
	logFile, err := initLogger()
	if err != nil {
		log.Fatalf("Error initializing logger: %v", err)
	}
	defer logFile.Close()

	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Printf("Error loading .env file: %v", err)
	}

	// Check for required environment variables
	checkRequiredEnvVars()

	// Initialize database
	db, err := initDB()
	if err != nil {
		log.Fatalf("Error initializing database: %v", err)
	}

	// Load configuration
	config, err := loadConfig("config.json")
	if err != nil {
		log.Fatalf("Error loading configuration: %v", err)
	}

	// Create Bot instance
	b, err := NewBot(db, config)
	if err != nil {
		log.Fatalf("Error creating bot: %v", err)
	}

	// Set up context with cancellation
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Start the bot
	log.Println("Starting bot...")
	b.Start(ctx)
}

func initLogger() (*os.File, error) {
	logFile, err := os.OpenFile("bot.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return nil, err
	}
	mw := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(mw)
	return logFile, nil
}

func checkRequiredEnvVars() {
	requiredEnvVars := []string{"TELEGRAM_BOT_TOKEN", "ANTHROPIC_API_KEY"}
	for _, envVar := range requiredEnvVars {
		if os.Getenv(envVar) == "" {
			log.Fatalf("%s environment variable is not set", envVar)
		}
	}
}
