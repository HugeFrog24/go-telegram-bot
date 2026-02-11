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

	db, err := gorm.Open(sqlite.Open("data/bot.db"), &gorm.Config{
		Logger: newLogger,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// AutoMigrate the models
	err = db.AutoMigrate(&BotModel{}, &ConfigModel{}, &Message{}, &User{}, &Role{})
	if err != nil {
		return nil, fmt.Errorf("failed to migrate database schema: %w", err)
	}

	// Enforce unique owner per bot using raw SQL
	// Note: SQLite doesn't support partial indexes, but we can simulate it by making a unique index on (BotID, IsOwner)
	// and ensuring that IsOwner can only be true for one user per BotID.
	// This approach allows multiple users with IsOwner=false for the same BotID,
	// but only one user can have IsOwner=true per BotID.
	err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_bot_owner ON users (bot_id, is_owner) WHERE is_owner = 1;`).Error
	if err != nil {
		return nil, fmt.Errorf("failed to create unique index for bot owners: %w", err)
	}

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
			ErrorLogger.Printf("Failed to create default role %s: %v", roleName, err)
			return fmt.Errorf("failed to create default role %s: %w", roleName, err)
		}
		InfoLogger.Printf("Created or confirmed default role: %s", roleName)
	}
	return nil
}
