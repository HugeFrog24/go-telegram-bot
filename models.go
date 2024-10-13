package main

import (
	"time"

	"gorm.io/gorm"
)

type Message struct {
	gorm.Model
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
	TelegramID int64 `gorm:"uniqueIndex"`
	Username   string
	RoleID     uint
	Role       Role `gorm:"foreignKey:RoleID"`
}
