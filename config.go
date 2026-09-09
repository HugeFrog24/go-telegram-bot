package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type MCPServer struct {
	Name               string   `json:"name"`
	URL                string   `json:"url"`
	AuthorizationToken string   `json:"authorization_token,omitempty"`
	AllowedTools       []string `json:"allowed_tools,omitempty"`
}

type WebSearchConfig struct {
	AllowedDomains      []string `json:"allowed_domains,omitempty"`
	BlockedDomains      []string `json:"blocked_domains,omitempty"`
	FetchAllowedDomains []string `json:"fetch_allowed_domains,omitempty"`
	DynamicFiltering    *bool    `json:"dynamic_filtering,omitempty"`
	MaxUses             int      `json:"max_uses,omitempty"`
	Fetch               bool     `json:"fetch,omitempty"`
	MaxContentTokens    int      `json:"max_content_tokens,omitempty"`
}

// validEfforts are the output_config.effort levels the API accepts. Models
// older than Claude 4.6 (Haiku 4.5, Sonnet 4.5) reject the parameter entirely.
var validEfforts = []string{"low", "medium", "high", "xhigh", "max"}

const (
	ThinkingModeAdaptive      = "adaptive"
	ThinkingModeDisabled      = "disabled"
	ThinkingDisplaySummarized = "summarized"
	ThinkingDisplayOmitted    = "omitted"
)

// maxDebounceMs bounds debounce_ms. Beyond this the bot reads as unresponsive
// rather than deliberate, and the coalesced turn drifts far enough from the
// user's last message that the reply feels stale.
const maxDebounceMs = 30000

// DebounceWindow is the quiet period an intake buffer waits before dispatching a
// coalesced turn. Zero disables debouncing entirely (the default), matching the
// opt-in behavior of comparable gateways.
func (c *BotConfig) DebounceWindow() time.Duration {
	if c.DebounceMs <= 0 {
		return 0
	}
	return time.Duration(c.DebounceMs) * time.Millisecond
}

// DynamicFilteringEnabled reports whether the web tools may run inside code
// execution to filter results before they reach the context window. Defaults to
// true; set "dynamic_filtering": false for models older than Claude 4.6, which
// reject the tool unless it is called directly.
func (c *WebSearchConfig) DynamicFilteringEnabled() bool {
	return c == nil || c.DynamicFiltering == nil || *c.DynamicFiltering
}

// CacheHistoryEnabled reports whether a cache_control breakpoint should be placed
// on the trailing conversation block in addition to the system prompt. Defaults
// to true; set "cache_history": false to opt out.
func (c *BotConfig) CacheHistoryEnabled() bool {
	return c.CacheHistory == nil || *c.CacheHistory
}

type BotConfig struct {
	ID                string            `json:"id"`
	TelegramToken     string            `json:"telegram_token"`
	MemorySize        int               `json:"memory_size"`
	MessagePerHour    int               `json:"messages_per_hour"`
	MessagePerDay     int               `json:"messages_per_day"`
	TempBanDuration   string            `json:"temp_ban_duration"`
	Model             string            `json:"model"`
	Temperature       *float32          `json:"temperature,omitempty"`
	MaxTokens         int               `json:"max_tokens,omitempty"`
	Thinking          string            `json:"thinking,omitempty"`
	ThinkingDisplay   string            `json:"thinking_display,omitempty"`
	Effort            string            `json:"effort,omitempty"`
	DebounceMs        int               `json:"debounce_ms,omitempty"`
	StreamDrafts      bool              `json:"stream_drafts,omitempty"`
	CacheHistory      *bool             `json:"cache_history,omitempty"`
	SystemPrompts     map[string]string `json:"system_prompts"`
	Active            bool              `json:"active"`
	OwnerTelegramID   int64             `json:"owner_telegram_id"`
	AnthropicAPIKey   string            `json:"anthropic_api_key"`
	ElevenLabsAPIKey  string            `json:"elevenlabs_api_key"`
	ElevenLabsVoiceID string            `json:"elevenlabs_voice_id"`
	ElevenLabsModel   string            `json:"elevenlabs_model"`
	DebugScreening    bool              `json:"debug_screening"`
	MCPServers        []MCPServer       `json:"mcp_servers,omitempty"`
	WebSearch         *WebSearchConfig  `json:"web_search,omitempty"`
	ConfigFilePath    string            `json:"-"`
}

func validateConfigPath(configDir, filename string) (string, error) {
	configDir = filepath.Clean(configDir)
	filename = filepath.Clean(filename)

	absConfigDir, err := filepath.Abs(configDir)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path for config directory: %w", err)
	}

	fullPath := filepath.Join(absConfigDir, filename)
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path for config file: %w", err)
	}

	rel, err := filepath.Rel(absConfigDir, absPath)
	if err != nil || strings.HasPrefix(rel, "..") || strings.Contains(rel, "..") {
		return "", fmt.Errorf("invalid config path: file must be within the config directory")
	}

	if filepath.Ext(absPath) != ".json" {
		return "", fmt.Errorf("invalid file extension: must be .json")
	}

	return absPath, nil
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
				InfoLogger.Printf("Invalid config path for %s: %v", file.Name(), err)
				continue
			}

			config, err := loadConfig(validPath)
			if err != nil {
				InfoLogger.Printf("Failed to load config %s: %v", validPath, err)
				continue
			}

			if !config.Active {
				InfoLogger.Printf("Skipping inactive bot: %s", config.ID)
				continue
			}

			if err := validateConfig(&config, ids, tokens); err != nil {
				InfoLogger.Printf("Config validation failed for %s: %v", validPath, err)
				continue
			}

			logConfigAdvisories(&config)

			config.ConfigFilePath = validPath
			configs = append(configs, config)
		}
	}

	if len(configs) == 0 {
		return nil, fmt.Errorf("no valid configs found")
	}

	return configs, nil
}

// logConfigAdvisories emits non-fatal boot-time notes about settings that are
// valid but likely to surprise: silently ineffective, or costlier than intended.
func logConfigAdvisories(config *BotConfig) {
	if config.Thinking == ThinkingModeAdaptive && config.MaxTokens > 0 && config.MaxTokens < 4000 {
		InfoLogger.Printf("[%s] thinking=adaptive with max_tokens=%d: thinking tokens count toward max_tokens; consider >= 4000",
			config.ID, config.MaxTokens)
	}

	if config.DebounceMs > 0 {
		InfoLogger.Printf("[%s] intake debounce enabled: coalescing rapid text messages over a %dms quiet window",
			config.ID, config.DebounceMs)
	} else {
		InfoLogger.Printf("[%s] intake debounce disabled: every message dispatches its own turn (set debounce_ms to coalesce rapid follow-ups)",
			config.ID)
	}

	if config.Effort != "" {
		InfoLogger.Printf("[%s] effort=%s: caps reasoning depth and token spend; the API rejects it on models older than Claude 4.6",
			config.ID, config.Effort)
	}

	if config.StreamDrafts {
		InfoLogger.Printf("[%s] draft streaming enabled: private chats get one growing draft with a Stop button; business and group chats keep per-block messages",
			config.ID)
	}

	if ws := config.WebSearch; ws != nil && len(ws.AllowedDomains) == 0 && len(ws.BlockedDomains) == 0 {
		InfoLogger.Printf("[%s] web_search enabled with no allowed_domains/blocked_domains: the model may search the open web",
			config.ID)
	}

	if ws := config.WebSearch; ws != nil && !ws.DynamicFilteringEnabled() {
		InfoLogger.Printf("[%s] web_search dynamic_filtering disabled: results enter context unfiltered (set it true on Claude 4.6+ models to cut tokens)",
			config.ID)
	}

	if ws := config.WebSearch; ws != nil && len(ws.FetchAllowedDomains) > 0 {
		if !ws.Fetch {
			InfoLogger.Printf("[%s] web_search.fetch_allowed_domains is set but fetch is disabled: it has no effect",
				config.ID)
		}
		for _, d := range ws.FetchAllowedDomains {
			if strings.Contains(d, "/") {
				InfoLogger.Printf("[%s] web_search.fetch_allowed_domains entry %q includes a path: web_fetch matches host-only, so the whole host is fetchable",
					config.ID, d)
			}
		}
	}
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

	switch config.Thinking {
	case "", ThinkingModeAdaptive, ThinkingModeDisabled:
	default:
		return fmt.Errorf("invalid 'thinking' value %q: must be %q or %q (or omitted)",
			config.Thinking, ThinkingModeAdaptive, ThinkingModeDisabled)
	}

	switch config.ThinkingDisplay {
	case "":
	case ThinkingDisplaySummarized, ThinkingDisplayOmitted:
		if config.Thinking != ThinkingModeAdaptive {
			return fmt.Errorf("'thinking_display' requires 'thinking': %q (the API rejects display with thinking disabled)",
				ThinkingModeAdaptive)
		}
	default:
		return fmt.Errorf("invalid 'thinking_display' value %q: must be %q or %q (or omitted)",
			config.ThinkingDisplay, ThinkingDisplaySummarized, ThinkingDisplayOmitted)
	}

	if config.Effort != "" && !slices.Contains(validEfforts, config.Effort) {
		return fmt.Errorf("invalid 'effort' value %q: must be one of %s (or omitted)",
			config.Effort, strings.Join(validEfforts, ", "))
	}

	if config.MaxTokens < 0 {
		return fmt.Errorf("'max_tokens' must be greater than 0 when set")
	}

	if config.DebounceMs < 0 {
		return fmt.Errorf("'debounce_ms' must be greater than 0 when set")
	}
	if config.DebounceMs > maxDebounceMs {
		return fmt.Errorf("'debounce_ms' of %d exceeds the maximum of %d (Telegram drops long-idle updates and users read silence as failure)",
			config.DebounceMs, maxDebounceMs)
	}

	if ws := config.WebSearch; ws != nil {
		if len(ws.AllowedDomains) > 0 && len(ws.BlockedDomains) > 0 {
			return fmt.Errorf("'web_search' cannot set both allowed_domains and blocked_domains (the API rejects that)")
		}
		if ws.MaxUses < 0 {
			return fmt.Errorf("'web_search.max_uses' must be greater than 0 when set")
		}
		if ws.MaxContentTokens < 0 {
			return fmt.Errorf("'web_search.max_content_tokens' must be greater than 0 when set")
		}
	}

	if config.MessagePerHour <= 0 {
		return fmt.Errorf("'messages_per_hour' must be greater than 0")
	}

	if config.MessagePerDay <= 0 {
		return fmt.Errorf("'messages_per_day' must be greater than 0")
	}

	return nil
}

func loadConfig(filename string) (BotConfig, error) {
	var config BotConfig
	file, err := os.OpenFile(filepath.Clean(filename), os.O_RDONLY, 0)
	if err != nil {
		return config, fmt.Errorf("failed to open config file %s: %w", filename, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			InfoLogger.Printf("Failed to close config file: %v", err)
		}
	}()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("failed to decode JSON from %s: %w", filename, err)
	}

	return config, nil
}

func (c *BotConfig) Reload(configDir, filename string) error {
	validPath, err := validateConfigPath(configDir, filename)
	if err != nil {
		return fmt.Errorf("invalid config path: %w", err)
	}

	cleanPath := filepath.Clean(validPath)
	file, err := os.OpenFile(cleanPath, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open config file %s: %w", cleanPath, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			InfoLogger.Printf("Failed to close config file: %v", err)
		}
	}()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(c); err != nil {
		return fmt.Errorf("failed to decode JSON from %s: %w", validPath, err)
	}

	return nil
}

func (c *BotConfig) PersistModel(newModel string) error {
	if c.ConfigFilePath == "" {
		return fmt.Errorf("config file path not set; cannot persist model")
	}

	data, err := os.ReadFile(c.ConfigFilePath)
	if err != nil {
		return fmt.Errorf("failed to read config for update: %w", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("failed to parse config for update: %w", err)
	}

	raw["model"] = newModel

	updated, err := json.MarshalIndent(raw, "", "\t")
	if err != nil {
		return fmt.Errorf("failed to re-encode config: %w", err)
	}

	if err := os.WriteFile(c.ConfigFilePath, updated, 0600); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	c.Model = newModel
	return nil
}
