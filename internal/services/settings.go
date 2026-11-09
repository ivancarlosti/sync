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

// Retention bounds used when nothing was configured yet.
const (
	// DefaultRetentionDays is the age, in days, past which the automatic purge
	// erases a run, an audit report or a traffic day.
	DefaultRetentionDays = 180
	// MinRetentionDays and MaxRetentionDays bound an operator's choice: one day
	// keeps the report usable during an incident, ten years covers any record
	// keeping rule this side of a legal hold.
	MinRetentionDays = 1
	MaxRetentionDays = 3650
)

// Rates bound what an operator may price one GiB at: the cost columns of the
// billing report stay readable numbers. The bound itself lives next to the
// billing schema vocabulary, where ValidateBillingSettings applies it.

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
	// BillingSchema selects the line-item vocabulary of the billing report
	// (`none`, `oci`, `aws`, `gcp` or `custom`).
	BillingSchema string `json:"billing_schema"`
	// BillingSchemaCustom is the JSON document of the `custom` schema.
	BillingSchemaCustom string `json:"billing_schema_custom"`
	// BillingRateInPerGiB prices one GiB read from a source account; 0 is the
	// common case (ingress is free) and leaves the inbound cost at zero.
	BillingRateInPerGiB float64 `json:"billing_rate_in_per_gib"`
	// BillingRateOutPerGiB prices one GiB written to a destination account. With
	// both rates at 0 the report stays in bytes only.
	BillingRateOutPerGiB float64 `json:"billing_rate_out_per_gib"`
	// BillingCurrency labels the cost column (a three letter code).
	BillingCurrency string `json:"billing_currency"`
	// PurgeEnabled arms the automatic retention pass. It never disables the
	// manual "purge now" actions of Admin > Settings.
	PurgeEnabled bool `json:"purge_enabled"`
	// PurgeJobsDays, PurgeAuditsDays and PurgeBillingDays are the ages, in days,
	// past which each kind of record is erased.
	PurgeJobsDays    int `json:"purge_jobs_days"`
	PurgeAuditsDays  int `json:"purge_audits_days"`
	PurgeBillingDays int `json:"purge_billing_days"`
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
//
// The legacy `billing_rate_per_gib` is deliberately not seeded any more: it is
// only read as the fallback of the outbound rate, so an instance configured
// before the split keeps its price and a fresh one never grows a dead key.
func Defaults(cfg *config.Config) map[string]string {
	return map[string]string{
		models.SettingDefaultLocale:        cfg.DefaultLocale,
		models.SettingDefaultTheme:         cfg.DefaultTheme,
		models.SettingSyncInterval:         strconv.Itoa(DefaultSyncInterval),
		models.SettingSyncTimeout:          strconv.Itoa(DefaultRunTimeout),
		models.SettingBillingSchema:        BillingSchemaNone,
		models.SettingBillingSchemaCustom:  DefaultBillingSchemaDocument(),
		models.SettingBillingRateInPerGiB:  "0",
		models.SettingBillingRateOutPerGiB: "0",
		models.SettingBillingCurrency:      "USD",
		models.SettingPurgeEnabled:         "true",
		models.SettingPurgeJobsDays:        strconv.Itoa(DefaultRetentionDays),
		models.SettingPurgeAuditsDays:      strconv.Itoa(DefaultRetentionDays),
		models.SettingPurgeBillingDays:     strconv.Itoa(DefaultRetentionDays),
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
		BillingSchema:       BillingSchemaNone,
		BillingCurrency:     "USD",
		PurgeEnabled:        true,
		PurgeJobsDays:       DefaultRetentionDays,
		PurgeAuditsDays:     DefaultRetentionDays,
		PurgeBillingDays:    DefaultRetentionDays,
	}
	if locale := strings.TrimSpace(s.settings.Value(ctx, models.SettingDefaultLocale, out.DefaultLocale)); locale != "" {
		out.DefaultLocale = locale
	}
	if theme := strings.ToLower(strings.TrimSpace(s.settings.Value(ctx, models.SettingDefaultTheme, out.DefaultTheme))); theme != "" {
		out.DefaultTheme = theme
	}
	out.SyncIntervalMinutes = s.settings.Int(ctx, models.SettingSyncInterval, out.SyncIntervalMinutes)
	out.RunTimeoutMinutes = s.settings.Int(ctx, models.SettingSyncTimeout, out.RunTimeoutMinutes)

	if schema := strings.ToLower(strings.TrimSpace(s.settings.Value(ctx, models.SettingBillingSchema, out.BillingSchema))); schema != "" {
		out.BillingSchema = schema
	}
	out.BillingSchemaCustom = s.settings.Value(ctx, models.SettingBillingSchemaCustom, "")
	out.BillingRateInPerGiB = s.float(ctx, models.SettingBillingRateInPerGiB, 0)
	// The single rate of the first releases is the fallback of the outbound one,
	// so an upgraded instance keeps the price it had configured.
	legacy := s.float(ctx, models.SettingBillingRatePerGiB, 0)
	out.BillingRateOutPerGiB = s.float(ctx, models.SettingBillingRateOutPerGiB, legacy)
	if currency := strings.ToUpper(strings.TrimSpace(s.settings.Value(ctx, models.SettingBillingCurrency, out.BillingCurrency))); currency != "" {
		out.BillingCurrency = currency
	}
	out.PurgeEnabled = s.settings.Bool(ctx, models.SettingPurgeEnabled, out.PurgeEnabled)
	out.PurgeJobsDays = clampDays(s.settings.Int(ctx, models.SettingPurgeJobsDays, out.PurgeJobsDays))
	out.PurgeAuditsDays = clampDays(s.settings.Int(ctx, models.SettingPurgeAuditsDays, out.PurgeAuditsDays))
	out.PurgeBillingDays = clampDays(s.settings.Int(ctx, models.SettingPurgeBillingDays, out.PurgeBillingDays))
	return out, nil
}

// clampDays keeps a stored retention inside its bounds, so a hand-edited
// database cannot make the purge erase everything or nothing.
func clampDays(days int) int {
	if days < MinRetentionDays {
		return MinRetentionDays
	}
	if days > MaxRetentionDays {
		return MaxRetentionDays
	}
	return days
}

// float reads a numeric setting, falling back to the default when the stored
// value is missing or malformed (a report must never fail on a bad setting).
func (s *SettingsService) float(ctx context.Context, key string, fallback float64) float64 {
	raw := strings.TrimSpace(s.settings.Value(ctx, key, ""))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 {
		return fallback
	}
	return value
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
	schema, document, currency, err := ValidateBillingSettings(
		in.BillingSchema, in.BillingSchemaCustom, in.BillingCurrency, in.BillingRateInPerGiB, in.BillingRateOutPerGiB)
	if err != nil {
		return AppSettings{}, err
	}
	for _, retention := range []struct {
		name string
		days int
	}{
		{"jobs", in.PurgeJobsDays},
		{"audits", in.PurgeAuditsDays},
		{"billing", in.PurgeBillingDays},
	} {
		if retention.days < MinRetentionDays || retention.days > MaxRetentionDays {
			return AppSettings{}, fmt.Errorf("%w: the %s retention must be between %d and %d days",
				ErrValidation, retention.name, MinRetentionDays, MaxRetentionDays)
		}
	}
	values := map[string]string{
		models.SettingDefaultLocale:        in.DefaultLocale,
		models.SettingDefaultTheme:         theme,
		models.SettingSyncInterval:         strconv.Itoa(in.SyncIntervalMinutes),
		models.SettingSyncTimeout:          strconv.Itoa(in.RunTimeoutMinutes),
		models.SettingBillingSchema:        schema,
		models.SettingBillingSchemaCustom:  document,
		models.SettingBillingRateInPerGiB:  strconv.FormatFloat(in.BillingRateInPerGiB, 'f', -1, 64),
		models.SettingBillingRateOutPerGiB: strconv.FormatFloat(in.BillingRateOutPerGiB, 'f', -1, 64),
		models.SettingBillingCurrency:      currency,
		models.SettingPurgeEnabled:         strconv.FormatBool(in.PurgeEnabled),
		models.SettingPurgeJobsDays:        strconv.Itoa(in.PurgeJobsDays),
		models.SettingPurgeAuditsDays:      strconv.Itoa(in.PurgeAuditsDays),
		models.SettingPurgeBillingDays:     strconv.Itoa(in.PurgeBillingDays),
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
