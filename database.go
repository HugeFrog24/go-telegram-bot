package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func initDB() (*gorm.DB, error) {
	if err := os.MkdirAll("data", 0750); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	newLogger := logger.New(
		log.New(log.Writer(), "\r\n", log.LstdFlags),
		logger.Config{
			SlowThreshold: time.Second,
			LogLevel:      logger.Info,
			Colorful:      false,
		},
	)

	db, err := gorm.Open(sqlite.Open("data/bot.db?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"), &gorm.Config{
		Logger: newLogger,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)

	// AutoMigrate the models
	err = db.AutoMigrate(&BotModel{}, &ConfigModel{}, &Message{}, &User{}, &Role{}, &Scope{})
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

	if err := createDefaultScopes(db); err != nil {
		return nil, fmt.Errorf("createDefaultScopes: %w", err)
	}

	return db, nil
}

func createDefaultScopes(db *gorm.DB) error {
	all := []string{
		ScopeStatsViewOwn, ScopeStatsViewAny,
		ScopeHistoryClearOwn, ScopeHistoryClearAny,
		ScopeHistoryClearHardOwn, ScopeHistoryClearHardAny,
		ScopeModelSet, ScopeUserPromote, ScopeTTSUse,
	}
	for _, name := range all {
		if err := db.FirstOrCreate(&Scope{}, Scope{Name: name}).Error; err != nil {
			return fmt.Errorf("failed to create scope %s: %w", name, err)
		}
	}

	userScopes := []string{
		ScopeStatsViewOwn,
		ScopeHistoryClearOwn,
		ScopeHistoryClearHardOwn,
	}
	elevatedScopes := []string{
		ScopeStatsViewOwn, ScopeStatsViewAny,
		ScopeHistoryClearOwn, ScopeHistoryClearAny,
		ScopeHistoryClearHardOwn, ScopeHistoryClearHardAny,
		ScopeModelSet, ScopeUserPromote, ScopeTTSUse,
	}
	assignments := map[string][]string{
		"user":  userScopes,
		"admin": elevatedScopes,
		// owner gets the same scopes as admin; owner uniqueness is enforced by the IsOwner flag
		"owner": elevatedScopes,
	}
	for roleName, scopes := range assignments {
		var role Role
		if err := db.Where("name = ?", roleName).First(&role).Error; err != nil {
			return fmt.Errorf("role %s not found: %w", roleName, err)
		}
		var scopeModels []Scope
		if err := db.Where("name IN ?", scopes).Find(&scopeModels).Error; err != nil {
			return fmt.Errorf("failed to find scopes for %s: %w", roleName, err)
		}
		if err := db.Model(&role).Association("Scopes").Replace(scopeModels); err != nil {
			return fmt.Errorf("failed to assign scopes to %s: %w", roleName, err)
		}
	}
	return nil
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
