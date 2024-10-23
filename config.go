package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/liushuangls/go-anthropic/v2"
)

type BotConfig struct {
	ID              string            `json:"id"`
	TelegramToken   string            `json:"telegram_token"`
	MemorySize      int               `json:"memory_size"`
	MessagePerHour  int               `json:"messages_per_hour"`
	MessagePerDay   int               `json:"messages_per_day"`
	TempBanDuration string            `json:"temp_ban_duration"`
	Model           anthropic.Model   `json:"model"`
	SystemPrompts   map[string]string `json:"system_prompts"`
	Active          bool              `json:"active"`
	OwnerTelegramID int64             `json:"owner_telegram_id"`
	AnthropicAPIKey string            `json:"anthropic_api_key"`
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

// validateConfigPath ensures the file path is within the allowed directory
func validateConfigPath(configDir, filename string) (string, error) {
	// Clean the paths to remove any . or .. components
	configDir = filepath.Clean(configDir)
	filename = filepath.Clean(filename)

	// Get absolute paths
	absConfigDir, err := filepath.Abs(configDir)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path for config directory: %w", err)
	}

	fullPath := filepath.Join(absConfigDir, filename)
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path for config file: %w", err)
	}

	// Check if the file path is within the config directory
	if !isSubPath(absConfigDir, absPath) {
		return "", fmt.Errorf("invalid config path: file must be within the config directory")
	}

	// Verify file extension
	if filepath.Ext(absPath) != ".json" {
		return "", fmt.Errorf("invalid file extension: must be .json")
	}

	return absPath, nil
}

// isSubPath checks if childPath is a subdirectory of parentPath
func isSubPath(parentPath, childPath string) bool {
	rel, err := filepath.Rel(parentPath, childPath)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
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
			validPath, err := validateConfigPath(dir, file.Name())
			if err != nil {
				return nil, fmt.Errorf("invalid config path: %w", err)
			}

			config, err := loadConfig(validPath)
			if err != nil {
				return nil, fmt.Errorf("failed to load config %s: %w", validPath, err)
			}

			// Validation checks...
			if !config.Active {
				InfoLogger.Printf("Skipping inactive bot: %s", config.ID)
				continue
			}

			if err := validateConfig(&config, ids, tokens); err != nil {
				return nil, fmt.Errorf("config validation failed for %s: %w", validPath, err)
			}

			configs = append(configs, config)
		}
	}
	return configs, nil
}

func validateConfig(config *BotConfig, ids, tokens map[string]bool) error {
	if config.ID == "" {
		return fmt.Errorf("missing 'id' field")
	}
	if _, exists := ids[config.ID]; exists {
		return fmt.Errorf("duplicate bot id '%s'", config.ID)
	}
	ids[config.ID] = true

	if config.TelegramToken == "" {
		return fmt.Errorf("missing 'telegram_token' field")
	}
	if _, exists := tokens[config.TelegramToken]; exists {
		return fmt.Errorf("duplicate telegram_token")
	}
	tokens[config.TelegramToken] = true

	if config.Model == "" {
		return fmt.Errorf("missing 'model' field")
	}

	return nil
}

func loadConfig(filename string) (BotConfig, error) {
	var config BotConfig
	file, err := os.OpenFile(filename, os.O_RDONLY, 0)
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
	// Get the directory of the current executable
	execDir, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable directory: %w", err)
	}
	configDir := filepath.Dir(execDir)

	// Validate the config path
	validPath, err := validateConfigPath(configDir, filename)
	if err != nil {
		return fmt.Errorf("invalid config path: %w", err)
	}

	file, err := os.OpenFile(validPath, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open config file %s: %w", validPath, err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(c); err != nil {
		return fmt.Errorf("failed to decode JSON from %s: %w", validPath, err)
	}

	c.Model = anthropic.Model(c.Model)
	return nil
}
