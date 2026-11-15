package main

import (
	"time"

	"gorm.io/gorm"
)

type BotModel struct {
	gorm.Model
	Identifier string `gorm:"uniqueIndex"`
	Name       string
	Configs    []ConfigModel `gorm:"foreignKey:BotID;constraint:OnDelete:CASCADE"`
	Users      []User        `gorm:"foreignKey:BotID;constraint:OnDelete:CASCADE"`
	Messages   []Message     `gorm:"foreignKey:BotID;constraint:OnDelete:CASCADE"`
}

type ConfigModel struct {
	gorm.Model
	BotID           uint   `gorm:"index"`
	MemorySize      int    `json:"memory_size"`
	MessagePerHour  int    `json:"messages_per_hour"`
	MessagePerDay   int    `json:"messages_per_day"`
	TempBanDuration string `json:"temp_ban_duration"`
	SystemPrompts   string `json:"system_prompts"`
	TelegramToken   string `json:"telegram_token"`
	Active          bool   `json:"active"`
}

type Message struct {
	gorm.Model
	BotID          uint   `gorm:"index"`
	ChatID         int64  `gorm:"index"`
	UserID         int64  `gorm:"index"`
	Username       string `gorm:"index"`
	UserRole       string
	Text           string    `gorm:"type:text"`
	Timestamp      time.Time `gorm:"index"`
	IsUser         bool
	StickerFileID  string
	StickerPNGFile string
	StickerEmoji   string
	DeletedAt      gorm.DeletedAt `gorm:"index"`
	AnsweredOn     *time.Time     `gorm:"index"`
	ImageFileIDs   []string       `gorm:"type:text;serializer:json"`
	FilesCleanedAt *time.Time     `gorm:"index"`
	// TelegramMessageID is Telegram's message_id, kept so an edited_message
	// update can find and correct the stored copy. Zero on rows stored before
	// edits were tracked.
	TelegramMessageID int `gorm:"index"`
}

type ChatMemory struct {
	Messages             []Message
	Size                 int
	BusinessConnectionID string
}

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
	BotID      uint  `gorm:"uniqueIndex:idx_user_bot;index"`
	TelegramID int64 `gorm:"uniqueIndex:idx_user_bot;not null"`
	Username   string
	RoleID     uint
	Role       Role `gorm:"foreignKey:RoleID"`
	IsOwner    bool `gorm:"default:false"`
}

func (User) TableName() string {
	return "users"
}
