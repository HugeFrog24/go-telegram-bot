package main

import (
	"fmt"
	"log"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func initDB() (*gorm.DB, error) {
	newLogger := logger.New(
		log.New(log.Writer(), "\r\n", log.LstdFlags),
		logger.Config{
			SlowThreshold: time.Second,
			LogLevel:      logger.Info,
			Colorful:      false,
		},
	)

	db, err := gorm.Open(sqlite.Open("bot.db"), &gorm.Config{
		Logger: newLogger,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// AutoMigrate with unique constraint for owners
	err = db.AutoMigrate(&BotModel{}, &ConfigModel{}, &Message{}, &User{}, &Role{})
	if err != nil {
		return nil, fmt.Errorf("failed to migrate database schema: %w", err)
	}

	// Add unique index for owners per bot
	db.SetupJoinTable(&BotModel{}, "Users", &User{})

	err = createDefaultRoles(db)
	if err != nil {
		return nil, err
	}

	return db, nil
}

func createDefaultRoles(db *gorm.DB) error {
	roles := []string{"user", "admin", "owner"}
	for _, roleName := range roles {
		var role Role
		if err := db.FirstOrCreate(&role, Role{Name: roleName}).Error; err != nil {
			return fmt.Errorf("failed to create default role %s: %w", roleName, err)
		}
	}
	return nil
}
