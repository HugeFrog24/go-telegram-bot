package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"

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

	// Load all bot configurations
	configs, err := loadAllConfigs("config")
	if err != nil {
		log.Fatalf("Error loading configurations: %v", err)
	}

	// Create a WaitGroup to manage goroutines
	var wg sync.WaitGroup

	// Set up context with cancellation
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Initialize and start each bot
	for _, config := range configs {
		wg.Add(1)
		go func(cfg BotConfig) {
			defer wg.Done()

			// Create Bot instance without TelegramClient initially
			realClock := RealClock{}
			bot, err := NewBot(db, cfg, realClock, nil)
			if err != nil {
				log.Printf("Error creating bot %s: %v", cfg.ID, err)
				return
			}

			// Initialize TelegramClient with the bot's handleUpdate method
			tgClient, err := initTelegramBot(cfg.TelegramToken, bot.handleUpdate)
			if err != nil {
				log.Printf("Error initializing Telegram client for bot %s: %v", cfg.ID, err)
				return
			}

			// Assign the TelegramClient to the bot
			bot.tgBot = tgClient

			// Start the bot
			log.Printf("Starting bot %s...", cfg.ID)
			bot.Start(ctx)
		}(config)
	}

	// Wait for all bots to finish
	wg.Wait()
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
	requiredEnvVars := []string{"ANTHROPIC_API_KEY"}
	for _, envVar := range requiredEnvVars {
		if os.Getenv(envVar) == "" {
			log.Fatalf("%s environment variable is not set", envVar)
		}
	}
}
