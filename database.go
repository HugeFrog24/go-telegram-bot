package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// dbLogLevel reads DB_LOG_LEVEL. The default is warn (GORM's own default):
// errors and slow queries only. "info" logs every statement, which is useful
// alone at a terminal and, in a deployed bot, writes each user message and
// reply into the container log twice over.
func dbLogLevel() logger.LogLevel {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("DB_LOG_LEVEL")))
	switch raw {
	case "", "warn", "warning":
		return logger.Warn
	case "silent":
		return logger.Silent
	case "error":
		return logger.Error
	case "info":
		return logger.Info
	default:
		InfoLogger.Printf("unknown DB_LOG_LEVEL %q; falling back to warn", raw)
		return logger.Warn
	}
}

func initDB() (*gorm.DB, error) {
	if err := os.MkdirAll("data", 0750); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	newLogger := logger.New(
		log.New(log.Writer(), "\r\n", log.LstdFlags),
		logger.Config{
			SlowThreshold: time.Second,
			LogLevel:      dbLogLevel(),
			Colorful:      false,
			// A missing row is routine here (the owner probe, a first-time
			// user), so it must not surface as an error once the level drops
			// below info.
			IgnoreRecordNotFoundError: true,
			// Log placeholders rather than inline values: a slow-query line
			// would otherwise carry the message text it was writing.
			ParameterizedQueries: true,
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

	err = db.AutoMigrate(&BotModel{}, &ConfigModel{}, &Message{}, &User{}, &Role{}, &Scope{})
	if err != nil {
		return nil, fmt.Errorf("failed to migrate database schema: %w", err)
	}

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
