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

			// Start the bot in a separate goroutine
			go bot.Start(ctx)

			// Keep the bot running until the context is cancelled
			<-ctx.Done()

			InfoLogger.Printf("Bot %s stopped", cfg.ID)
		}(config)
	}

	// Wait for all bots to finish
	wg.Wait()

	InfoLogger.Println("All bots have stopped. Exiting application.")
}
