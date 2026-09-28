package services

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/database"
	"github.com/ivancarlosti/sync/internal/models"
)

// Default scheduler values used when nothing was configured yet.
const (
	// DefaultSyncInterval is the schedule proposed for a new job, in minutes.
	DefaultSyncInterval = 60
	// DefaultRunTimeout is how long a run may take before it is aborted.
	DefaultRunTimeout = 30
)

// AppSettings is the runtime configuration an administrator can change in
// Admin > Settings. The environment provides the initial value, the database
// holds the operator's choice.
type AppSettings struct {
	DefaultLocale string `json:"default_locale"`
	DefaultTheme  string `json:"default_theme"`
	// SyncIntervalMinutes is the schedule proposed by default (0 = manual only).
	SyncIntervalMinutes int `json:"sync_default_interval_minutes"`
	// RunTimeoutMinutes bounds a single sync run.
	RunTimeoutMinutes int `json:"sync_run_timeout_minutes"`
}

// SettingsService reads and writes the editable settings.
type SettingsService struct {
	cfg      *config.Config
	settings *database.Settings
}

// NewSettingsService builds the service.
func NewSettingsService(cfg *config.Config, settings *database.Settings) *SettingsService {
	return &SettingsService{cfg: cfg, settings: settings}
}

// Defaults returns the values seeded at boot: the environment configuration.
func Defaults(cfg *config.Config) map[string]string {
	return map[string]string{
		models.SettingDefaultLocale: cfg.DefaultLocale,
		models.SettingDefaultTheme:  cfg.DefaultTheme,
		models.SettingSyncInterval:  strconv.Itoa(DefaultSyncInterval),
		models.SettingSyncTimeout:   strconv.Itoa(DefaultRunTimeout),
	}
}

// Load returns the effective settings, falling back to the environment defaults
// and clamping malformed values instead of failing the request.
func (s *SettingsService) Load(ctx context.Context) (AppSettings, error) {
	out := AppSettings{
		DefaultLocale:       s.cfg.DefaultLocale,
		DefaultTheme:        s.cfg.DefaultTheme,
		SyncIntervalMinutes: DefaultSyncInterval,
		RunTimeoutMinutes:   DefaultRunTimeout,
	}
	if locale := strings.TrimSpace(s.settings.Value(ctx, models.SettingDefaultLocale, out.DefaultLocale)); locale != "" {
		out.DefaultLocale = locale
	}
	if theme := strings.ToLower(strings.TrimSpace(s.settings.Value(ctx, models.SettingDefaultTheme, out.DefaultTheme))); theme != "" {
		out.DefaultTheme = theme
	}
	out.SyncIntervalMinutes = s.settings.Int(ctx, models.SettingSyncInterval, out.SyncIntervalMinutes)
	out.RunTimeoutMinutes = s.settings.Int(ctx, models.SettingSyncTimeout, out.RunTimeoutMinutes)
	return out, nil
}

// Update validates and stores every editable setting at once.
func (s *SettingsService) Update(ctx context.Context, in AppSettings) (AppSettings, error) {
	if !containsString(config.SupportedLocales, in.DefaultLocale) {
		return AppSettings{}, fmt.Errorf("%w: default locale must be one of %s",
			ErrValidation, strings.Join(config.SupportedLocales, ", "))
	}
	theme := strings.ToLower(strings.TrimSpace(in.DefaultTheme))
	if !containsString(config.SupportedThemes, theme) {
		return AppSettings{}, fmt.Errorf("%w: default theme must be one of %s",
			ErrValidation, strings.Join(config.SupportedThemes, ", "))
	}
	if in.SyncIntervalMinutes < 0 || in.SyncIntervalMinutes > 10080 {
		return AppSettings{}, fmt.Errorf("%w: the default interval must be between 0 and 10080 minutes", ErrValidation)
	}
	if in.RunTimeoutMinutes < 1 || in.RunTimeoutMinutes > 1440 {
		return AppSettings{}, fmt.Errorf("%w: the run timeout must be between 1 and 1440 minutes", ErrValidation)
	}
	values := map[string]string{
		models.SettingDefaultLocale: in.DefaultLocale,
		models.SettingDefaultTheme:  theme,
		models.SettingSyncInterval:  strconv.Itoa(in.SyncIntervalMinutes),
		models.SettingSyncTimeout:   strconv.Itoa(in.RunTimeoutMinutes),
	}
	if err := s.settings.SetMany(ctx, values); err != nil {
		return AppSettings{}, fmt.Errorf("services: saving settings: %w", err)
	}
	return s.Load(ctx)
}

// Raw returns every stored setting (used by the admin screen and to show the
// provider credential overrides, which are masked before leaving the API).
func (s *SettingsService) Raw(ctx context.Context) (map[string]string, error) {
	values, err := s.settings.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("services: reading settings: %w", err)
	}
	return values, nil
}

// RunTimeout returns the configured run timeout as minutes.
func (s *SettingsService) RunTimeout(ctx context.Context) int {
	return s.settings.Int(ctx, models.SettingSyncTimeout, DefaultRunTimeout)
}

// containsString reports whether list holds value.
func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
