package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/liushuangls/go-anthropic/v2"
)

type BotConfig struct {
	ID              string            `json:"id"`             // Unique identifier for the bot
	TelegramToken   string            `json:"telegram_token"` // Telegram Bot Token
	MemorySize      int               `json:"memory_size"`
	MessagePerHour  int               `json:"messages_per_hour"`
	MessagePerDay   int               `json:"messages_per_day"`
	TempBanDuration string            `json:"temp_ban_duration"`
	Model           anthropic.Model   `json:"model"` // Changed from string to anthropic.Model
	SystemPrompts   map[string]string `json:"system_prompts"`
}

// Custom unmarshalling to handle anthropic.Model
func (c *BotConfig) UnmarshalJSON(data []byte) error {
	type Alias BotConfig
	aux := &struct {
		Model string `json:"model"`
		*Alias
	}{
		Alias: (*Alias)(c),
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	c.Model = anthropic.Model(aux.Model)
	return nil
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

			// Validate Model
			if config.Model == "" {
				return nil, fmt.Errorf("config %s is missing 'model' field", configPath)
			}

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

	// Ensure the Model is correctly casted
	c.Model = anthropic.Model(c.Model)

	return nil
}
