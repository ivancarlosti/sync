// Package database owns the GORM connection, the schema migration and the thin
// repository helpers (settings) used by the rest of the application.
//
// The MySQL/MariaDB server is always external: this repository never ships a
// database container (see docs/configuration.md).
package database

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/models"
)

// pool settings, deliberately conservative: a single Sync instance runs a
// handful of concurrent sync jobs.
const (
	maxOpenConns    = 25
	maxIdleConns    = 5
	connMaxLifetime = time.Hour
	connMaxIdleTime = 10 * time.Minute
	pingTimeout     = 10 * time.Second
)

// Open connects to the configured database, verifies the connection with a
// ping and returns a ready to use *gorm.DB.
func Open(cfg *config.Config) (*gorm.DB, error) {
	level := gormlogger.Warn
	if cfg.LogLevel == "debug" {
		level = gormlogger.Info
	}
	// Slow query threshold: anything above one second is worth surfacing even
	// at the default log level.
	logger := gormlogger.New(log.New(os.Stderr, "gorm ", log.LstdFlags), gormlogger.Config{
		SlowThreshold:             time.Second,
		LogLevel:                  level,
		IgnoreRecordNotFoundError: true,
		Colorful:                  false,
	})

	db, err := gorm.Open(mysql.Open(DSN(cfg)), &gorm.Config{
		Logger:                                   logger,
		DisableForeignKeyConstraintWhenMigrating: true,
		NowFunc:                                  func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("database: cannot open %s:%d/%s: %w", cfg.Database.Host, cfg.Database.Port, cfg.Database.Name, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("database: cannot access the connection pool: %w", err)
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxIdleConns)
	sqlDB.SetConnMaxLifetime(connMaxLifetime)
	sqlDB.SetConnMaxIdleTime(connMaxIdleTime)

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("database: ping failed (check DB_HOST/DB_PORT/DB_USERNAME/DB_PASSWORD): %w", err)
	}
	return db, nil
}

// DSN builds the go-sql-driver connection string. parseTime is required because
// several columns are timestamps, and the location is pinned to UTC so that
// everything stored is comparable regardless of the host time zone.
func DSN(cfg *config.Config) string {
	params := []string{
		"charset=utf8mb4",
		"parseTime=True",
		"loc=UTC",
		"timeout=10s",
		"readTimeout=60s",
		"writeTimeout=60s",
		"interpolateParams=true",
	}
	if cfg.Database.SSL {
		params = append(params, "tls=skip-verify")
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?%s",
		cfg.Database.Username,
		cfg.Database.Password,
		cfg.Database.Host,
		cfg.Database.Port,
		cfg.Database.Name,
		strings.Join(params, "&"))
}

// Migrate creates or updates every table Sync needs. It is idempotent and runs
// on every boot, which is the documented upgrade path (see docs/database.md).
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&models.Setting{},
		&models.ConnectedAccount{},
		&models.OAuthState{},
		&models.NotificationChannel{},
		&models.SyncJob{},
		&models.SyncRun{},
		&models.SyncItem{},
		&models.SyncFile{},
	); err != nil {
		return fmt.Errorf("database: migration failed: %w", err)
	}
	return nil
}

// Seed inserts the default settings that do not exist yet. Values already
// present are never overwritten, so an operator edit survives an upgrade.
func Seed(ctx context.Context, store *Settings, defaults map[string]string) error {
	for key, value := range defaults {
		exists, err := store.Exists(ctx, key)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if err := store.Set(ctx, key, value); err != nil {
			return err
		}
	}
	return nil
}

// PruneOAuthStates removes the flows that were never redeemed (abandoned login
// attempts and provider connections).
func PruneOAuthStates(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).
		Where("expires_at < ?", time.Now().UTC()).
		Delete(&models.OAuthState{}).Error
}

// Close releases the underlying SQL pool.
func Close(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
