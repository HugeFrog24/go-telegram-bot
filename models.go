package main

import (
	"time"

	"gorm.io/gorm"
)

type BotModel struct {
	gorm.Model
	Identifier string `gorm:"uniqueIndex"` // Renamed from ID to Identifier
	Name       string
	Configs    []ConfigModel `gorm:"foreignKey:BotID;constraint:OnDelete:CASCADE"`
	Users      []User        `gorm:"foreignKey:BotID;constraint:OnDelete:CASCADE"` // Associated users
	Messages   []Message     `gorm:"foreignKey:BotID;constraint:OnDelete:CASCADE"`
}

type ConfigModel struct {
	gorm.Model
	BotID           uint   `gorm:"index"`
	MemorySize      int    `json:"memory_size"`
	MessagePerHour  int    `json:"messages_per_hour"`
	MessagePerDay   int    `json:"messages_per_day"`
	TempBanDuration string `json:"temp_ban_duration"`
	SystemPrompts   string `json:"system_prompts"` // Consider JSON string or separate table
	TelegramToken   string `json:"telegram_token"`
	Active          bool   `json:"active"`
}

type Message struct {
	gorm.Model
	BotID          uint      `gorm:"index"`
	ChatID         int64     `gorm:"index"`
	UserID         int64     `gorm:"index"`
	Username       string    `gorm:"index"`
	UserRole       string    // Store the role as a string
	Text           string    `gorm:"type:text"`
	Timestamp      time.Time `gorm:"index"`
	IsUser         bool
	StickerFileID  string
	StickerPNGFile string
	StickerEmoji   string         // Store the emoji associated with the sticker
	DeletedAt      gorm.DeletedAt `gorm:"index"` // Add soft delete field
	AnsweredOn     *time.Time     `gorm:"index"` // Tracks when a user message was answered (NULL for assistant messages and unanswered user messages)
}

type ChatMemory struct {
	Messages             []Message
	Size                 int
	BusinessConnectionID string // New field to store the business connection ID
}

// Scope name constants — used in DB seeding, hasScope checks, and tests.
const (
	ScopeStatsViewOwn        = "stats:view:own"
	ScopeStatsViewAny        = "stats:view:any"
	ScopeHistoryClearOwn     = "history:clear:own"
	ScopeHistoryClearAny     = "history:clear:any"
	ScopeHistoryClearHardOwn = "history:clear_hard:own"
	ScopeHistoryClearHardAny = "history:clear_hard:any"
	ScopeModelSet            = "model:set"
	ScopeUserPromote         = "user:promote"
	ScopeTTSUse              = "tts:use"
)

type Scope struct {
	gorm.Model
	Name string `gorm:"uniqueIndex"`
}

type Role struct {
	gorm.Model
	Name   string  `gorm:"uniqueIndex"`
	Scopes []Scope `gorm:"many2many:role_scopes;"`
}

type User struct {
	gorm.Model
	BotID      uint  `gorm:"uniqueIndex:idx_user_bot;index"`    // Foreign key to BotModel
	TelegramID int64 `gorm:"uniqueIndex:idx_user_bot;not null"` // Unique per (telegram_id, bot_id) pair
	Username   string
	RoleID     uint
	Role       Role `gorm:"foreignKey:RoleID"`
	IsOwner    bool `gorm:"default:false"` // Indicates if the user is the owner
}

// idx_user_bot is a composite unique index on (bot_id, telegram_id),
// allowing the same Telegram user to be registered independently on each bot.
func (User) TableName() string {
	return "users"
}
