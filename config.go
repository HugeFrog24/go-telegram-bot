package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type BotConfig struct {
	ID              string            `json:"id"` // Unique identifier for the bot
	MemorySize      int               `json:"memory_size"`
	MessagePerHour  int               `json:"messages_per_hour"`
	MessagePerDay   int               `json:"messages_per_day"`
	TempBanDuration string            `json:"temp_ban_duration"`
	SystemPrompts   map[string]string `json:"system_prompts"`
	TelegramToken   string            `json:"telegram_token"` // Telegram Bot Token
}

func loadAllConfigs(dir string) ([]BotConfig, error) {
	var configs []BotConfig
	ids := make(map[string]bool)
	tokens := make(map[string]bool)

	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read config directory: %w", err)
	}

	for _, file := range files {
		if filepath.Ext(file.Name()) == ".json" {
			configPath := filepath.Join(dir, file.Name())
			config, err := loadConfig(configPath)
			if err != nil {
				return nil, fmt.Errorf("failed to load config %s: %w", configPath, err)
			}

			// Validate that ID is present
			if config.ID == "" {
				return nil, fmt.Errorf("config %s is missing 'id' field", configPath)
			}

			// Check for unique ID
			if _, exists := ids[config.ID]; exists {
				return nil, fmt.Errorf("duplicate bot id '%s' found in %s", config.ID, configPath)
			}
			ids[config.ID] = true

			// Validate Telegram Token
			if config.TelegramToken == "" {
				return nil, fmt.Errorf("config %s is missing 'telegram_token' field", configPath)
			}

			// Check for unique Telegram Token
			if _, exists := tokens[config.TelegramToken]; exists {
				return nil, fmt.Errorf("duplicate telegram_token '%s' found in %s", config.TelegramToken, configPath)
			}
			tokens[config.TelegramToken] = true

			configs = append(configs, config)
		}
	}

	return configs, nil
}

func loadConfig(filename string) (BotConfig, error) {
	var config BotConfig
	file, err := os.Open(filename)
	if err != nil {
		return config, fmt.Errorf("failed to open config file %s: %w", filename, err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("failed to decode JSON from %s: %w", filename, err)
	}

	// Optionally override telegram_token with environment variable if set
	// Uncomment the following lines if you choose to use environment variables for tokens
	/*
	   if envToken := os.Getenv(fmt.Sprintf("TELEGRAM_TOKEN_%s", config.ID)); envToken != "" {
	       config.TelegramToken = envToken
	   }
	*/

	return config, nil
}

func (c *BotConfig) Reload(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to open config file %s: %w", filename, err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(c); err != nil {
		return fmt.Errorf("failed to decode JSON from %s: %w", filename, err)
	}

	return nil
}
