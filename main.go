package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

func main() {
	// The bot re-runs its own binary as the media sandbox helper for every
	// ffprobe/ffmpeg run (sandbox_linux.go). That run must never reach the
	// bot's startup: no config, no logs, no database.
	if len(os.Args) > 1 && os.Args[1] == mediaSandboxArg {
		os.Exit(runMediaSandbox(os.Args[2:]))
	}

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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
			// A turn cut short still removes its status message. Every bot
			// waits from the same moment, so the exit waits this long at most.
			bot.waitForStatusLines(statusShutdownGrace)

			InfoLogger.Printf("Bot %s stopped", cfg.ID)
		}(config)
	}

	wg.Wait()

	InfoLogger.Println("All bots have stopped. Exiting application.")
}
