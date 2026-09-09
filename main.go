package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
)

func main() {
	initLoggers()

	InfoLogger.Println("Starting Telegram Bot Application")

	db, err := initDB()
	if err != nil {
		ErrorLogger.Fatalf("Error initializing database: %v", err)
	}

	configs, err := loadAllConfigs("config")
	if err != nil {
		ErrorLogger.Fatalf("Error loading configurations: %v", err)
	}

	var wg sync.WaitGroup

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	for _, config := range configs {
		wg.Add(1)
		go func(cfg BotConfig) {
			defer wg.Done()

			realClock := RealClock{}
			bot, err := NewBot(db, cfg, realClock, nil)
			if err != nil {
				ErrorLogger.Printf("Error creating bot %s: %v", cfg.ID, err)
				return
			}

			go bot.Start(ctx)

			<-ctx.Done()

			InfoLogger.Printf("Bot %s stopped", cfg.ID)
		}(config)
	}

	wg.Wait()

	InfoLogger.Println("All bots have stopped. Exiting application.")
}
