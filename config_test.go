package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liushuangls/go-anthropic/v2"
)

// Add this at the beginning of the file, after the imports
func TestMain(m *testing.M) {
	initLoggers()
	os.Exit(m.Run())
}

// TestBotConfig_UnmarshalJSON tests the custom unmarshalling of BotConfig
func TestBotConfig_UnmarshalJSON(t *testing.T) {
	jsonData := `{
		"id": "bot123",
		"telegram_token": "token123",
		"memory_size": 1024,
		"messages_per_hour": 10,
		"messages_per_day": 100,
		"temp_ban_duration": "1h",
		"model": "claude-v1",
		"system_prompts": {"welcome": "Hello!"},
		"active": true,
		"owner_telegram_id": 123456789,
		"anthropic_api_key": "api_key_123"
	}`

	var config BotConfig
	if err := json.Unmarshal([]byte(jsonData), &config); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	expectedModel := anthropic.Model("claude-v1")
	if config.Model != expectedModel {
		t.Errorf("Expected model %s, got %s", expectedModel, config.Model)
	}

	expectedID := "bot123"
	if config.ID != expectedID {
		t.Errorf("Expected ID %s, got %s", expectedID, config.ID)
	}

	// Add more field checks as necessary
}

// TestValidateConfigPath tests the validateConfigPath function
func TestValidateConfigPath(t *testing.T) {
	execDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get current directory: %v", err)
	}

	tests := []struct {
		name      string
		configDir string
		filename  string
		wantErr   bool
	}{
		{
			name:      "Valid Path",
			configDir: execDir,
			filename:  "config.json",
			wantErr:   false,
		},
		{
			name:      "Invalid Extension",
			configDir: execDir,
			filename:  "config.yaml",
			wantErr:   true,
		},
		{
			name:      "Path Traversal",
			configDir: execDir,
			filename:  "../config.json",
			wantErr:   true,
		},
		{
			name:      "Absolute Path Outside",
			configDir: execDir,
			filename:  "/etc/passwd",
			wantErr:   true,
		},
		{
			name:      "Nested Valid Path",
			configDir: execDir,
			filename:  "subdir/config.json",
			wantErr:   false,
		},
	}

	// Create a subdirectory for testing
	subDir := filepath.Join(execDir, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("Failed to create subdir: %v", err)
	}
	defer os.RemoveAll(subDir)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configDir := tt.configDir
			filename := tt.filename
			if tt.name == "Nested Valid Path" {
				configDir = subDir
			}
			_, err := validateConfigPath(configDir, filename)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateConfigPath() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestLoadConfig tests the loadConfig function
func TestLoadConfig(t *testing.T) {
	// Create a temporary directory
	tempDir, err := os.MkdirTemp("", "config_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Valid config JSON
	validConfig := `{
		"id": "bot123",
		"telegram_token": "token123",
		"memory_size": 1024,
		"messages_per_hour": 10,
		"messages_per_day": 100,
		"temp_ban_duration": "1h",
		"model": "claude-v1",
		"system_prompts": {"welcome": "Hello!"},
		"active": true,
		"owner_telegram_id": 123456789,
		"anthropic_api_key": "api_key_123"
	}`

	// Invalid config JSON
	invalidConfig := `{
		"id": "bot123",
		"telegram_token": "token123",
		"memory_size": "should be int",
		"model": "claude-v1"
	}`

	// Write valid config file
	validPath := filepath.Join(tempDir, "valid_config.json")
	if err := os.WriteFile(validPath, []byte(validConfig), 0644); err != nil {
		t.Fatalf("Failed to write valid config: %v", err)
	}

	// Write invalid config file
	invalidPath := filepath.Join(tempDir, "invalid_config.json")
	if err := os.WriteFile(invalidPath, []byte(invalidConfig), 0644); err != nil {
		t.Fatalf("Failed to write invalid config: %v", err)
	}

	tests := []struct {
		name      string
		filename  string
		wantErr   bool
		expectID  string
		expectErr string
	}{
		{
			name:     "Load Valid Config",
			filename: validPath,
			wantErr:  false,
			expectID: "bot123",
		},
		{
			name:      "Load Invalid Config",
			filename:  invalidPath,
			wantErr:   true,
			expectErr: "failed to decode JSON",
		},
		{
			name:      "Non-existent File",
			filename:  filepath.Join(tempDir, "nonexistent.json"),
			wantErr:   true,
			expectErr: "failed to open config file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := loadConfig(tt.filename)
			if (err != nil) != tt.wantErr {
				t.Errorf("loadConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && err != nil && tt.expectErr != "" {
				if !contains(err.Error(), tt.expectErr) {
					t.Errorf("loadConfig() error = %v, expected to contain %v", err, tt.expectErr)
				}
				return
			}
			if config.ID != tt.expectID {
				t.Errorf("Expected ID %s, got %s", tt.expectID, config.ID)
			}
		})
	}
}

// TestValidateConfig tests the validateConfig function
func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name          string
		config        BotConfig
		ids           map[string]bool
		tokens        map[string]bool
		wantErr       bool
		expectedError string
	}{
		{
			name: "Valid Config",
			config: BotConfig{
				ID:              "bot123",
				TelegramToken:   "token123",
				Model:           "claude-v1",
				Active:          true,
				OwnerTelegramID: 123456789,
			},
			ids:     make(map[string]bool),
			tokens:  make(map[string]bool),
			wantErr: false,
		},
		{
			name: "Missing ID",
			config: BotConfig{
				TelegramToken: "token123",
				Model:         "claude-v1",
				Active:        true,
			},
			ids:           make(map[string]bool),
			tokens:        make(map[string]bool),
			wantErr:       true,
			expectedError: "missing 'id' field",
		},
		{
			name: "Duplicate ID",
			config: BotConfig{
				ID:            "bot123",
				TelegramToken: "token123",
				Model:         "claude-v1",
				Active:        true,
			},
			ids:           map[string]bool{"bot123": true},
			tokens:        make(map[string]bool),
			wantErr:       true,
			expectedError: "duplicate bot id",
		},
		{
			name: "Missing Telegram Token",
			config: BotConfig{
				ID:     "bot123",
				Model:  "claude-v1",
				Active: true,
			},
			ids:           make(map[string]bool),
			tokens:        make(map[string]bool),
			wantErr:       true,
			expectedError: "missing 'telegram_token' field",
		},
		{
			name: "Duplicate Telegram Token",
			config: BotConfig{
				ID:            "bot123",
				TelegramToken: "token123",
				Model:         "claude-v1",
				Active:        true,
			},
			ids:           make(map[string]bool),
			tokens:        map[string]bool{"token123": true},
			wantErr:       true,
			expectedError: "duplicate telegram_token",
		},
		{
			name: "Missing Model",
			config: BotConfig{
				ID:            "bot123",
				TelegramToken: "token123",
				Active:        true,
			},
			ids:           make(map[string]bool),
			tokens:        make(map[string]bool),
			wantErr:       true,
			expectedError: "missing 'model' field",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(&tt.config, tt.ids, tt.tokens)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && err != nil && tt.expectedError != "" {
				if !contains(err.Error(), tt.expectedError) {
					t.Errorf("validateConfig() error = %v, expected to contain %v", err, tt.expectedError)
				}
			}
		})
	}
}

// TestLoadAllConfigs tests the loadAllConfigs function
func TestLoadAllConfigs(t *testing.T) {
	// Create a temporary directory
	tempDir, err := os.MkdirTemp("", "load_all_configs_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	tests := []struct {
		name           string
		setupFiles     map[string]string // filename -> content
		expectConfigs  int
		expectError    bool
		expectErrorMsg string
	}{
		{
			name: "Load All Valid Configs",
			setupFiles: map[string]string{
				"valid_config.json": `{
					"id": "bot123",
					"telegram_token": "token123",
					"memory_size": 1024,
					"messages_per_hour": 10,
					"messages_per_day": 100,
					"temp_ban_duration": "1h",
					"model": "claude-v1",
					"system_prompts": {"welcome": "Hello!"},
					"active": true,
					"owner_telegram_id": 123456789,
					"anthropic_api_key": "api_key_123"
				}`,
			},
			expectConfigs: 1,
			expectError:   false,
		},
		{
			name: "Skip Inactive Config",
			setupFiles: map[string]string{
				"valid_config.json": `{
					"id": "bot123",
					"telegram_token": "token123",
					"memory_size": 1024,
					"messages_per_hour": 10,
					"messages_per_day": 100,
					"temp_ban_duration": "1h",
					"model": "claude-v1",
					"system_prompts": {"welcome": "Hello!"},
					"active": true,
					"owner_telegram_id": 123456789,
					"anthropic_api_key": "api_key_123"
				}`,
				"inactive_config.json": `{
					"id": "bot124",
					"telegram_token": "token124",
					"memory_size": 512,
					"messages_per_hour": 5,
					"messages_per_day": 50,
					"temp_ban_duration": "30m",
					"model": "claude-v2",
					"system_prompts": {"welcome": "Hi!"},
					"active": false,
					"owner_telegram_id": 987654321,
					"anthropic_api_key": "api_key_124"
				}`,
			},
			expectConfigs: 1,
			expectError:   false,
		},
		{
			name: "Duplicate Bot ID",
			setupFiles: map[string]string{
				"valid_config.json": `{
					"id": "bot123",
					"telegram_token": "token123",
					"memory_size": 1024,
					"messages_per_hour": 10,
					"messages_per_day": 100,
					"temp_ban_duration": "1h",
					"model": "claude-v1",
					"system_prompts": {"welcome": "Hello!"},
					"active": true,
					"owner_telegram_id": 123456789,
					"anthropic_api_key": "api_key_123"
				}`,
				"duplicate_id_config.json": `{
					"id": "bot123",
					"telegram_token": "token125",
					"memory_size": 256,
					"messages_per_hour": 2,
					"messages_per_day": 20,
					"temp_ban_duration": "15m",
					"model": "claude-v3",
					"system_prompts": {"welcome": "Hey!"},
					"active": true,
					"owner_telegram_id": 1122334455,
					"anthropic_api_key": "api_key_125"
				}`,
			},
			expectConfigs: 1,
			expectError:   false,
		},
		{
			name: "Duplicate Telegram Token",
			setupFiles: map[string]string{
				"valid_config.json": `{
					"id": "bot123",
					"telegram_token": "token123",
					"memory_size": 1024,
					"messages_per_hour": 10,
					"messages_per_day": 100,
					"temp_ban_duration": "1h",
					"model": "claude-v1",
					"system_prompts": {"welcome": "Hello!"},
					"active": true,
					"owner_telegram_id": 123456789,
					"anthropic_api_key": "api_key_123"
				}`,
				"duplicate_token_config.json": `{
					"id": "bot126",
					"telegram_token": "token123",
					"memory_size": 128,
					"messages_per_hour": 1,
					"messages_per_day": 10,
					"temp_ban_duration": "5m",
					"model": "claude-v4",
					"system_prompts": {"welcome": "Greetings!"},
					"active": true,
					"owner_telegram_id": 5566778899,
					"anthropic_api_key": "api_key_126"
				}`,
			},
			expectConfigs: 1,
			expectError:   false,
		},
		{
			name: "Invalid Config",
			setupFiles: map[string]string{
				"valid_config.json": `{
					"id": "bot123",
					"telegram_token": "token123",
					"memory_size": 1024,
					"messages_per_hour": 10,
					"messages_per_day": 100,
					"temp_ban_duration": "1h",
					"model": "claude-v1",
					"system_prompts": {"welcome": "Hello!"},
					"active": true,
					"owner_telegram_id": 123456789,
					"anthropic_api_key": "api_key_123"
				}`,
				"invalid_config.json": `{
					"id": "bot127",
					"telegram_token": "token127",
					"model": "",
					"active": true
				}`,
			},
			expectConfigs: 1,
			expectError:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear the tempDir before each test
			os.RemoveAll(tempDir)
			os.MkdirAll(tempDir, 0755)

			// Write the test files directly
			for filename, content := range tt.setupFiles {
				err := os.WriteFile(filepath.Join(tempDir, filename), []byte(content), 0644)
				if err != nil {
					t.Fatalf("Failed to write file %s: %v", filename, err)
				}
			}

			configs, err := loadAllConfigs(tempDir)
			if (err != nil) != tt.expectError {
				t.Errorf("loadAllConfigs() error = %v, wantErr %v", err, tt.expectError)
				return
			}
			if len(configs) != tt.expectConfigs {
				t.Errorf("Expected %d configs, got %d", tt.expectConfigs, len(configs))
			}
		})
	}
}

// TestBotConfig_Reload tests the Reload method of BotConfig
func TestBotConfig_Reload(t *testing.T) {
	// Create a temporary directory
	tempDir, err := os.MkdirTemp("", "reload_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create initial config file
	config1 := `{
		"id": "bot123",
		"telegram_token": "token123",
		"memory_size": 1024,
		"messages_per_hour": 10,
		"messages_per_day": 100,
		"temp_ban_duration": "1h",
		"model": "claude-v1",
		"system_prompts": {"welcome": "Hello!"},
		"active": true,
		"owner_telegram_id": 123456789,
		"anthropic_api_key": "api_key_123"
	}`
	configPath := filepath.Join(tempDir, "config.json")
	if err := os.WriteFile(configPath, []byte(config1), 0644); err != nil {
		t.Fatalf("Failed to write initial config: %v", err)
	}

	// Initialize BotConfig
	var config BotConfig
	if err := config.Reload(tempDir, "config.json"); err != nil {
		t.Fatalf("Failed to reload config: %v", err)
	}

	// Verify initial load
	if config.ID != "bot123" {
		t.Errorf("Expected ID 'bot123', got '%s'", config.ID)
	}
	if config.Model != "claude-v1" {
		t.Errorf("Expected Model 'claude-v1', got '%s'", config.Model)
	}

	// Update config file
	config2 := `{
		"id": "bot123",
		"telegram_token": "token123_updated",
		"memory_size": 2048,
		"messages_per_hour": 20,
		"messages_per_day": 200,
		"temp_ban_duration": "2h",
		"model": "claude-v2",
		"system_prompts": {"welcome": "Hi there!"},
		"active": true,
		"owner_telegram_id": 987654321,
		"anthropic_api_key": "api_key_456"
	}`
	if err := os.WriteFile(configPath, []byte(config2), 0644); err != nil {
		t.Fatalf("Failed to write updated config: %v", err)
	}

	// Reload config
	if err := config.Reload(tempDir, "config.json"); err != nil {
		t.Fatalf("Failed to reload updated config: %v", err)
	}

	// Verify updated config
	if config.TelegramToken != "token123_updated" {
		t.Errorf("Expected TelegramToken 'token123_updated', got '%s'", config.TelegramToken)
	}
	if config.MemorySize != 2048 {
		t.Errorf("Expected MemorySize 2048, got %d", config.MemorySize)
	}
	if config.Model != "claude-v2" {
		t.Errorf("Expected Model 'claude-v2', got '%s'", config.Model)
	}
	if config.OwnerTelegramID != 987654321 {
		t.Errorf("Expected OwnerTelegramID 987654321, got %d", config.OwnerTelegramID)
	}
}

// TestBotConfig_UnmarshalJSON_Invalid tests unmarshalling with invalid model
func TestBotConfig_UnmarshalJSON_Invalid(t *testing.T) {
	jsonData := `{
		"id": "bot123",
		"telegram_token": "token123",
		"memory_size": 1024,
		"messages_per_hour": 10,
		"messages_per_day": 100,
		"temp_ban_duration": "1h",
		"model": "",
		"system_prompts": {"welcome": "Hello!"},
		"active": true,
		"owner_telegram_id": 123456789,
		"anthropic_api_key": "api_key_123"
	}`

	var config BotConfig
	err := json.Unmarshal([]byte(jsonData), &config)
	if err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	if config.Model != "" {
		t.Errorf("Expected empty model, got %s", config.Model)
	}
}

// Helper function to check substring
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// Additional tests can be added here to cover more scenarios
