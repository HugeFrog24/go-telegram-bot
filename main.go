package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
)

func main() {
	// Initialize custom loggers
	initLoggers()

	// Log the start of the application
	InfoLogger.Println("Starting Telegram Bot Application")

	// Initialize database
	db, err := initDB()
	if err != nil {
		ErrorLogger.Fatalf("Error initializing database: %v", err)
	}

	// Load all bot configurations
	configs, err := loadAllConfigs("config")
	if err != nil {
		ErrorLogger.Fatalf("Error loading configurations: %v", err)
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
				ErrorLogger.Printf("Error creating bot %s: %v", cfg.ID, err)
				return
			}

			// Initialize TelegramClient with the bot's handleUpdate method
			tgClient, err := initTelegramBot(cfg.TelegramToken, bot.handleUpdate)
			if err != nil {
				ErrorLogger.Printf("Error initializing Telegram client for bot %s: %v", cfg.ID, err)
				return
			}

			// Assign the TelegramClient to the bot
			bot.tgBot = tgClient

			// Start the bot
			InfoLogger.Printf("Starting bot %s...", cfg.ID)
			bot.Start(ctx)
		}(config)
	}

	// Wait for all bots to finish
	wg.Wait()

	InfoLogger.Println("All bots have stopped. Exiting application.")
}
