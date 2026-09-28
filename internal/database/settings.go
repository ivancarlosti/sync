package database

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ivancarlosti/sync/internal/models"
)

// Settings is the repository for the key/value settings table. Every read
// returns a usable value: a missing key falls back to the value seeded at boot
// or to the caller's default, so callers never have to handle "not found".
type Settings struct {
	db *gorm.DB
}

// NewSettings wraps the database handle.
func NewSettings(db *gorm.DB) *Settings { return &Settings{db: db} }

// DB exposes the underlying handle for the rare caller that needs a query
// alongside a settings write.
func (s *Settings) DB() *gorm.DB { return s.db }

// All returns every setting as a map, ordered by key for stable output.
func (s *Settings) All(ctx context.Context) (map[string]string, error) {
	var rows []models.Setting
	if err := s.db.WithContext(ctx).Order("`key` ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make(map[string]string, len(rows))
	for _, row := range rows {
		values[row.Key] = row.Value
	}
	return values, nil
}

// Exists reports whether key is stored.
func (s *Settings) Exists(ctx context.Context, key string) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Model(&models.Setting{}).
		Where("`key` = ?", key).Count(&count).Error
	return count > 0, err
}

// Get returns the stored value and whether the key was present.
func (s *Settings) Get(ctx context.Context, key string) (string, bool, error) {
	var row models.Setting
	err := s.db.WithContext(ctx).Where("`key` = ?", key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return row.Value, true, nil
}

// Value returns the stored value or fallback when the key is unset.
func (s *Settings) Value(ctx context.Context, key, fallback string) string {
	value, found, err := s.Get(ctx, key)
	if err != nil || !found || strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// Int returns an integer setting, falling back when unset or malformed.
func (s *Settings) Int(ctx context.Context, key string, fallback int) int {
	raw := s.Value(ctx, key, "")
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

// Bool returns a boolean setting, falling back when unset or malformed.
func (s *Settings) Bool(ctx context.Context, key string, fallback bool) bool {
	raw := s.Value(ctx, key, "")
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

// Set inserts or updates a single setting.
func (s *Settings) Set(ctx context.Context, key, value string) error {
	return s.SetMany(ctx, map[string]string{key: value})
}

// SetMany upserts several settings in one statement.
func (s *Settings) SetMany(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	rows := make([]models.Setting, 0, len(values))
	for key, value := range values {
		rows = append(rows, models.Setting{Key: key, Value: value})
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
	}).Create(&rows).Error
}

// Delete removes a setting (used when an admin clears a provider credential
// override so the environment value applies again).
func (s *Settings) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Where("`key` IN ?", keys).Delete(&models.Setting{}).Error
}

// Prefix returns every key starting with prefix, without the prefix. It is how
// the provider credential overrides are loaded in one query.
//
// `key` is quoted everywhere in this file: it is a reserved word in
// MySQL/MariaDB, and GORM only quotes the identifiers it builds itself.
func (s *Settings) Prefix(ctx context.Context, prefix string) (map[string]string, error) {
	var rows []models.Setting
	if err := s.db.WithContext(ctx).
		Where("`key` LIKE ?", prefix+"%").
		Order("`key` ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make(map[string]string, len(rows))
	for _, row := range rows {
		values[strings.TrimPrefix(row.Key, prefix)] = row.Value
	}
	return values, nil
}
