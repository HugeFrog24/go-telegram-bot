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
	Users      []User        `gorm:"foreignKey:BotID;constraint:OnDelete:CASCADE"` // Added foreign key
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
}

type Message struct {
	gorm.Model
	BotID     uint
	ChatID    int64
	UserID    int64
	Username  string
	UserRole  string
	Text      string
	Timestamp time.Time
	IsUser    bool
}

type ChatMemory struct {
	Messages []Message
	Size     int
}

type Role struct {
	gorm.Model
	Name string `gorm:"uniqueIndex"`
}

type User struct {
	gorm.Model
	BotID      uint  `gorm:"index"`       // Added foreign key to BotModel
	TelegramID int64 `gorm:"uniqueIndex"` // Consider composite unique index if TelegramID is unique per Bot
	Username   string
	RoleID     uint
	Role       Role `gorm:"foreignKey:RoleID"`
}
