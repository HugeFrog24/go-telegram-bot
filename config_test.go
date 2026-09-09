package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	initLoggers()
	os.Exit(m.Run())
}

func TestBotConfig_UnmarshalJSON(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	jsonData := `{
		"id": "bot123",
		"telegram_token": "token123",
		"memory_size": 1024,
		"messages_per_hour": 10,
		"messages_per_day": 100,
		"temp_ban_duration": "1h",
		"model": "claude-v1",
		"temperature": 0.7,
		"system_prompts": {"welcome": "Hello!"},
		"active": true,
		"owner_telegram_id": 123456789,
		"anthropic_api_key": "api_key_123"
	}`

	var config BotConfig
	if err := json.Unmarshal([]byte(jsonData), &config); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	expectedModel := "claude-v1"
	if config.Model != expectedModel {
		t.Errorf("Expected model %s, got %s", expectedModel, config.Model)
	}

	expectedID := "bot123"
	if config.ID != expectedID {
		t.Errorf("Expected ID %s, got %s", expectedID, config.ID)
	}

}

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

	subDir := filepath.Join(execDir, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("Failed to create subdir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(subDir); err != nil {
			t.Errorf("Failed to remove test subdirectory: %v", err)
		}
	}()

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

func TestLoadConfig(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "config_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Errorf("Failed to remove temp directory: %v", err)
		}
	}()

	validConfig := `{
		"id": "bot123",
		"telegram_token": "token123",
		"memory_size": 1024,
		"messages_per_hour": 10,
		"messages_per_day": 100,
		"temp_ban_duration": "1h",
		"model": "claude-v1",
		"temperature": 0.7,
		"system_prompts": {"welcome": "Hello!"},
		"active": true,
		"owner_telegram_id": 123456789,
		"anthropic_api_key": "api_key_123"
	}`

	invalidConfig := `{
		"id": "bot123",
		"telegram_token": "token123",
		"memory_size": "should be int",
		"model": "claude-v1"
	}`

	validPath := filepath.Join(tempDir, "valid_config.json")
	if err := os.WriteFile(validPath, []byte(validConfig), 0644); err != nil {
		t.Fatalf("Failed to write valid config: %v", err)
	}

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
				MessagePerHour:  10,
				MessagePerDay:   100,
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
		{
			name: "Zero MessagePerHour",
			config: BotConfig{
				ID:             "bot123",
				TelegramToken:  "token123",
				Model:          "claude-v1",
				MessagePerHour: 0,
				MessagePerDay:  100,
			},
			ids:           make(map[string]bool),
			tokens:        make(map[string]bool),
			wantErr:       true,
			expectedError: "'messages_per_hour' must be greater than 0",
		},
		{
			name: "Zero MessagePerDay",
			config: BotConfig{
				ID:             "bot123",
				TelegramToken:  "token123",
				Model:          "claude-v1",
				MessagePerHour: 10,
				MessagePerDay:  0,
			},
			ids:           make(map[string]bool),
			tokens:        make(map[string]bool),
			wantErr:       true,
			expectedError: "'messages_per_day' must be greater than 0",
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

func TestLoadAllConfigs(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "load_all_configs_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Errorf("Failed to remove temp directory: %v", err)
		}
	}()

	tests := []struct {
		name           string
		setupFiles     map[string]string
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
					"temperature": 0.7,
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
					"temperature": 0.5,
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
					"temperature": 0.3,
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
					"temperature": 0.2,
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
			if err := os.RemoveAll(tempDir); err != nil {
				t.Fatalf("Failed to remove temp dir: %v", err)
			}
			if err := os.MkdirAll(tempDir, 0755); err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}

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

func TestBotConfig_Reload(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	tempDir, err := os.MkdirTemp("", "reload_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Errorf("Failed to remove temp directory: %v", err)
		}
	}()

	config1 := `{
		"id": "bot123",
		"telegram_token": "token123",
		"memory_size": 1024,
		"messages_per_hour": 10,
		"messages_per_day": 100,
		"temp_ban_duration": "1h",
		"model": "claude-v1",
		"temperature": 0.7,
		"system_prompts": {"welcome": "Hello!"},
		"active": true,
		"owner_telegram_id": 123456789,
		"anthropic_api_key": "api_key_123"
	}`
	configPath := filepath.Join(tempDir, "config.json")
	if err := os.WriteFile(configPath, []byte(config1), 0644); err != nil {
		t.Fatalf("Failed to write initial config: %v", err)
	}

	var config BotConfig
	if err := config.Reload(tempDir, "config.json"); err != nil {
		t.Fatalf("Failed to reload config: %v", err)
	}

	if config.ID != "bot123" {
		t.Errorf("Expected ID 'bot123', got '%s'", config.ID)
	}
	if config.Model != "claude-v1" {
		t.Errorf("Expected Model 'claude-v1', got '%s'", config.Model)
	}

	config2 := `{
		"id": "bot123",
		"telegram_token": "token123_updated",
		"memory_size": 2048,
		"messages_per_hour": 20,
		"messages_per_day": 200,
		"temp_ban_duration": "2h",
		"model": "claude-v2",
		"temperature": 0.3,
		"system_prompts": {"welcome": "Hi there!"},
		"active": true,
		"owner_telegram_id": 987654321,
		"anthropic_api_key": "api_key_456"
	}`
	if err := os.WriteFile(configPath, []byte(config2), 0644); err != nil {
		t.Fatalf("Failed to write updated config: %v", err)
	}

	if err := config.Reload(tempDir, "config.json"); err != nil {
		t.Fatalf("Failed to reload updated config: %v", err)
	}

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

func TestBotConfig_UnmarshalJSON_Invalid(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	jsonData := `{
		"id": "bot123",
		"telegram_token": "token123",
		"memory_size": 1024,
		"messages_per_hour": 10,
		"messages_per_day": 100,
		"temp_ban_duration": "1h",
		"model": "",
		"temperature": 0.7,
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

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func TestTemperatureConfig(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "temperature_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Errorf("Failed to remove temp directory: %v", err)
		}
	}()

	configWithTemp := `{
		"id": "bot123",
		"telegram_token": "token123",
		"memory_size": 1024,
		"messages_per_hour": 10,
		"messages_per_day": 100,
		"temp_ban_duration": "1h",
		"model": "claude-v1",
		"temperature": 0.42,
		"system_prompts": {"welcome": "Hello!"},
		"active": true,
		"owner_telegram_id": 123456789,
		"anthropic_api_key": "api_key_123"
	}`

	configWithoutTemp := `{
		"id": "bot124",
		"telegram_token": "token124",
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

	withTempPath := filepath.Join(tempDir, "with_temp.json")
	if err := os.WriteFile(withTempPath, []byte(configWithTemp), 0644); err != nil {
		t.Fatalf("Failed to write config with temperature: %v", err)
	}

	withoutTempPath := filepath.Join(tempDir, "without_temp.json")
	if err := os.WriteFile(withoutTempPath, []byte(configWithoutTemp), 0644); err != nil {
		t.Fatalf("Failed to write config without temperature: %v", err)
	}

	configWithTempObj, err := loadConfig(withTempPath)
	if err != nil {
		t.Fatalf("Failed to load config with temperature: %v", err)
	}

	if configWithTempObj.Temperature == nil {
		t.Errorf("Expected Temperature to be set, got nil")
	} else if *configWithTempObj.Temperature != 0.42 {
		t.Errorf("Expected Temperature 0.42, got %f", *configWithTempObj.Temperature)
	}

	configWithoutTempObj, err := loadConfig(withoutTempPath)
	if err != nil {
		t.Fatalf("Failed to load config without temperature: %v", err)
	}

	if configWithoutTempObj.Temperature != nil {
		t.Errorf("Expected Temperature to be nil, got %f", *configWithoutTempObj.Temperature)
	}
}

func TestBotConfig_PersistModel(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	tempDir, err := os.MkdirTemp("", "persist_model_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Errorf("Failed to remove temp directory: %v", err)
		}
	}()

	initialJSON := `{
		"id": "bot1",
		"telegram_token": "token1",
		"model": "claude-v1",
		"messages_per_hour": 10,
		"messages_per_day": 100
	}`
	configPath := filepath.Join(tempDir, "config.json")
	if err := os.WriteFile(configPath, []byte(initialJSON), 0600); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	config := BotConfig{
		ID:             "bot1",
		Model:          "claude-v1",
		ConfigFilePath: configPath,
	}

	if err := config.PersistModel("claude-sonnet-4-6"); err != nil {
		t.Fatalf("PersistModel() unexpected error: %v", err)
	}

	if string(config.Model) != "claude-sonnet-4-6" {
		t.Errorf("in-memory model: got %q, want %q", config.Model, "claude-sonnet-4-6")
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Failed to read updated config file: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Failed to unmarshal updated config: %v", err)
	}
	if raw["model"] != "claude-sonnet-4-6" {
		t.Errorf("on-disk model: got %v, want %q", raw["model"], "claude-sonnet-4-6")
	}
	if raw["id"] != "bot1" {
		t.Errorf("on-disk id should be preserved: got %v, want %q", raw["id"], "bot1")
	}

	noPath := BotConfig{Model: "claude-v1"}
	if err := noPath.PersistModel("claude-sonnet-4-6"); err == nil {
		t.Error("PersistModel with empty ConfigFilePath: expected error, got nil")
	}
}

func thinkingTestConfig(id string) BotConfig {
	return BotConfig{
		ID:              id,
		TelegramToken:   "token-" + id,
		MemorySize:      10,
		MessagePerHour:  10,
		MessagePerDay:   100,
		TempBanDuration: "1h",
		Model:           "claude-test",
	}
}

func TestThinkingConfig(t *testing.T) {
	cases := []struct {
		name     string
		thinking string
		display  string
		wantErr  string
	}{
		{"absent", "", "", ""},
		{"adaptive", ThinkingModeAdaptive, "", ""},
		{"disabled", ThinkingModeDisabled, "", ""},
		{"adaptive summarized", ThinkingModeAdaptive, ThinkingDisplaySummarized, ""},
		{"adaptive omitted", ThinkingModeAdaptive, ThinkingDisplayOmitted, ""},
		{"legacy enabled rejected", "enabled", "", "invalid 'thinking'"},
		{"case sensitive", "Adaptive", "", "invalid 'thinking'"},
		{"unknown display", ThinkingModeAdaptive, "verbose", "invalid 'thinking_display'"},
		{"display without thinking", "", ThinkingDisplaySummarized, "'thinking_display' requires"},
		{"display with disabled", ThinkingModeDisabled, ThinkingDisplayOmitted, "'thinking_display' requires"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := thinkingTestConfig(fmt.Sprintf("bot-think-%d", i))
			cfg.Thinking = tc.thinking
			cfg.ThinkingDisplay = tc.display
			err := validateConfig(&cfg, map[string]bool{}, map[string]bool{})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateConfig(thinking=%q display=%q) = %v, want nil", tc.thinking, tc.display, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateConfig(thinking=%q display=%q) = nil, want error containing %q", tc.thinking, tc.display, tc.wantErr)
			}
			if !contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestMaxTokensConfig(t *testing.T) {
	var withValue BotConfig
	if err := json.Unmarshal([]byte(`{"max_tokens": 4000}`), &withValue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if withValue.MaxTokens != 4000 {
		t.Errorf("MaxTokens = %d, want 4000", withValue.MaxTokens)
	}

	var withoutValue BotConfig
	if err := json.Unmarshal([]byte(`{}`), &withoutValue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if withoutValue.MaxTokens != 0 {
		t.Errorf("MaxTokens = %d, want 0 when absent", withoutValue.MaxTokens)
	}

	neg := thinkingTestConfig("bot-maxtok-neg")
	neg.MaxTokens = -1
	if err := validateConfig(&neg, map[string]bool{}, map[string]bool{}); err == nil {
		t.Error("validateConfig(max_tokens=-1) = nil, want error")
	}
	zero := thinkingTestConfig("bot-maxtok-zero")
	zero.MaxTokens = 0
	if err := validateConfig(&zero, map[string]bool{}, map[string]bool{}); err != nil {
		t.Errorf("validateConfig(max_tokens=0) = %v, want nil", err)
	}
}

func TestThinkingConfigLoad(t *testing.T) {
	jsonData := `{
		"id": "bot-think-load",
		"thinking": "adaptive",
		"thinking_display": "omitted",
		"max_tokens": 4096
	}`
	var cfg BotConfig
	if err := json.Unmarshal([]byte(jsonData), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Thinking != ThinkingModeAdaptive {
		t.Errorf("Thinking = %q, want %q", cfg.Thinking, ThinkingModeAdaptive)
	}
	if cfg.ThinkingDisplay != ThinkingDisplayOmitted {
		t.Errorf("ThinkingDisplay = %q, want %q", cfg.ThinkingDisplay, ThinkingDisplayOmitted)
	}
	if cfg.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %d, want 4096", cfg.MaxTokens)
	}
}

func TestWebSearchConfig(t *testing.T) {
	t.Run("absent leaves nil", func(t *testing.T) {
		var cfg BotConfig
		if err := json.Unmarshal([]byte(`{}`), &cfg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if cfg.WebSearch != nil {
			t.Errorf("WebSearch = %+v, want nil when absent", cfg.WebSearch)
		}
	})

	t.Run("loads allowlist", func(t *testing.T) {
		jsonData := `{
			"web_search": {
				"allowed_domains": ["example.com/hc", "docs.example.com"],
				"max_uses": 3,
				"fetch": true,
				"max_content_tokens": 50000
			}
		}`
		var cfg BotConfig
		if err := json.Unmarshal([]byte(jsonData), &cfg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if cfg.WebSearch == nil {
			t.Fatalf("WebSearch = nil, want populated")
		}
		if len(cfg.WebSearch.AllowedDomains) != 2 {
			t.Errorf("AllowedDomains = %v, want 2 entries", cfg.WebSearch.AllowedDomains)
		}
		if cfg.WebSearch.MaxUses != 3 {
			t.Errorf("MaxUses = %d, want 3", cfg.WebSearch.MaxUses)
		}
		if !cfg.WebSearch.Fetch {
			t.Error("Fetch = false, want true")
		}
		if cfg.WebSearch.MaxContentTokens != 50000 {
			t.Errorf("MaxContentTokens = %d, want 50000", cfg.WebSearch.MaxContentTokens)
		}
	})

	cases := []struct {
		name    string
		ws      *WebSearchConfig
		wantErr string
	}{
		{"nil ok", nil, ""},
		{"allowlist ok", &WebSearchConfig{AllowedDomains: []string{"example.com"}, MaxUses: 3}, ""},
		{"blocklist ok", &WebSearchConfig{BlockedDomains: []string{"evil.com"}}, ""},
		{"empty ok", &WebSearchConfig{}, ""},
		{"both allow and block rejected",
			&WebSearchConfig{AllowedDomains: []string{"a.com"}, BlockedDomains: []string{"b.com"}},
			"cannot set both allowed_domains and blocked_domains"},
		{"negative max_uses rejected",
			&WebSearchConfig{AllowedDomains: []string{"a.com"}, MaxUses: -1},
			"'web_search.max_uses' must be greater than 0"},
		{"negative max_content_tokens rejected",
			&WebSearchConfig{AllowedDomains: []string{"a.com"}, MaxContentTokens: -1},
			"'web_search.max_content_tokens' must be greater than 0"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := thinkingTestConfig(fmt.Sprintf("bot-web-%d", i))
			cfg.WebSearch = tc.ws
			err := validateConfig(&cfg, map[string]bool{}, map[string]bool{})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateConfig(web_search=%+v) = %v, want nil", tc.ws, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateConfig(web_search=%+v) = nil, want error containing %q", tc.ws, tc.wantErr)
			}
			if !contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
