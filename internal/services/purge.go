// This file holds the retention pass behind Admin > Settings > Purge: the age
// past which a run, an audit report and a day of traffic stop being worth
// keeping, both as an automatic pass the scheduler runs and as the manual
// "purge now" actions of the settings screen.
//
// It is age based on purpose. Trimming by count (see PruneRuns and PruneAudits)
// answers "keep the overview short", which is not the same question as "delete
// everything older than the retention period an operator wrote down", and the
// two are allowed to disagree.
package services

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// The purge targets: the three kinds of record an operator trims.
const (
	// PurgeTargetJobs erases finished sync runs with their items and traffic.
	PurgeTargetJobs = "jobs"
	// PurgeTargetAudits erases finished content-audit reports with their entries.
	PurgeTargetAudits = "audits"
	// PurgeTargetBilling erases the traffic rows of the days that aged out.
	PurgeTargetBilling = "billing"
	// PurgeTargetAll runs the three of them in one pass.
	PurgeTargetAll = "all"
)

// PurgeTargets lists the three targets in the order the settings screen shows
// them.
var PurgeTargets = []string{PurgeTargetJobs, PurgeTargetAudits, PurgeTargetBilling}

// PurgePolicy is the retention an operator configured: one switch for the
// automatic pass and one age per kind of record.
//
// The three ages move independently because the records do: a run log is
// troubleshooting material, an audit report backs a compliance statement and a
// traffic day is what an invoice is made of.
type PurgePolicy struct {
	Enabled     bool
	JobsDays    int
	AuditsDays  int
	BillingDays int
}

// Days returns the configured age of a target (0 for an unknown one).
func (p PurgePolicy) Days(target string) int {
	switch target {
	case PurgeTargetJobs:
		return p.JobsDays
	case PurgeTargetAudits:
		return p.AuditsDays
	case PurgeTargetBilling:
		return p.BillingDays
	default:
		return 0
	}
}

// PurgeResult counts what a pass erased.
type PurgeResult struct {
	// Jobs is the number of sync runs (with their items and traffic rows).
	Jobs int64 `json:"jobs"`
	// Audits is the number of content-audit reports (with their entries).
	Audits int64 `json:"audits"`
	// Billing is the number of traffic rows.
	Billing int64 `json:"billing"`
}

// Total is what the screen reports as "records removed".
func (r PurgeResult) Total() int64 {
	return r.Jobs + r.Audits + r.Billing
}

// PurgeOutcome is what a pass did: the age it applied to each target and the
// counters of what it erased. It is the answer of the manual purge endpoint and
// what the scheduler logs.
type PurgeOutcome struct {
	Target  string         `json:"target"`
	Days    map[string]int `json:"days"`
	Removed PurgeResult    `json:"removed"`
}

// PurgeService applies the retention policy. It reads the settings through the
// store it is built from, so nothing has to be wired through a constructor.
type PurgeService struct {
	store *Store
	// now is a seam for the tests: the cutoff is always computed in UTC.
	now func() time.Time
}

// NewPurgeService builds the service.
func NewPurgeService(store *Store) *PurgeService {
	return &PurgeService{store: store, now: func() time.Time { return time.Now().UTC() }}
}

// Policy reads the four retention keys, clamping what a hand-edited database
// could have left behind.
func (p *PurgeService) Policy(ctx context.Context) PurgePolicy {
	settings := p.store.Settings()
	return PurgePolicy{
		Enabled:     settings.Bool(ctx, models.SettingPurgeEnabled, true),
		JobsDays:    clampDays(settings.Int(ctx, models.SettingPurgeJobsDays, DefaultRetentionDays)),
		AuditsDays:  clampDays(settings.Int(ctx, models.SettingPurgeAuditsDays, DefaultRetentionDays)),
		BillingDays: clampDays(settings.Int(ctx, models.SettingPurgeBillingDays, DefaultRetentionDays)),
	}
}

// Run is the automatic pass: it applies the configured policy and does nothing
// when an administrator switched it off. The manual purge stays available
// either way, which is what the switch promises.
func (p *PurgeService) Run(ctx context.Context) (PurgeOutcome, error) {
	policy := p.Policy(ctx)
	if !policy.Enabled {
		return PurgeOutcome{Target: PurgeTargetAll, Days: map[string]int{}}, nil
	}
	outcome, err := p.purge(ctx, policy, PurgeTargetAll, 0)
	if err != nil {
		return PurgeOutcome{}, err
	}
	if outcome.Removed.Total() == 0 {
		return outcome, nil
	}
	slog.Info("retention pass done",
		"runs", outcome.Removed.Jobs,
		"audits", outcome.Removed.Audits,
		"traffic_rows", outcome.Removed.Billing)
	return outcome, nil
}

// Purge runs one pass on demand. A zero day count means "use the configured
// retention of the target", which is what a caller that only wants the policy
// applied sends; an unknown target or an age outside
// MinRetentionDays...MaxRetentionDays is a validation error.
func (p *PurgeService) Purge(ctx context.Context, target string, days int) (PurgeOutcome, error) {
	if days < 0 {
		return PurgeOutcome{}, fmt.Errorf("%w: the purge age cannot be negative", ErrValidation)
	}
	return p.purge(ctx, p.Policy(ctx), target, days)
}

// purge is the shared body: it resolves the targets and applies the age to each.
func (p *PurgeService) purge(ctx context.Context, policy PurgePolicy, target string, days int) (PurgeOutcome, error) {
	targets, err := purgeTargets(target)
	if err != nil {
		return PurgeOutcome{}, err
	}
	outcome := PurgeOutcome{Target: target, Days: make(map[string]int, len(targets))}
	now := p.now()
	for _, name := range targets {
		age := days
		if age <= 0 {
			age = policy.Days(name)
		}
		if age < MinRetentionDays || age > MaxRetentionDays {
			return PurgeOutcome{}, fmt.Errorf("%w: the %s retention must be between %d and %d days",
				ErrValidation, name, MinRetentionDays, MaxRetentionDays)
		}
		cutoff := now.AddDate(0, 0, -age)
		switch name {
		case PurgeTargetJobs:
			outcome.Removed.Jobs, err = p.store.PurgeRunsBefore(ctx, cutoff)
		case PurgeTargetAudits:
			outcome.Removed.Audits, err = p.store.PurgeAuditsBefore(ctx, cutoff)
		case PurgeTargetBilling:
			outcome.Removed.Billing, err = p.store.PurgeTrafficBefore(ctx, cutoff)
		}
		if err != nil {
			return PurgeOutcome{}, err
		}
		outcome.Days[name] = age
	}
	return outcome, nil
}

// purgeTargets resolves what a pass touches: one target, or the three of them
// for `all` (and for the empty string a caller may send).
func purgeTargets(target string) ([]string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(target))
	if trimmed == "" || trimmed == PurgeTargetAll {
		return PurgeTargets, nil
	}
	if containsString(PurgeTargets, trimmed) {
		return []string{trimmed}, nil
	}
	return nil, fmt.Errorf("%w: the purge target must be one of %s or %s",
		ErrValidation, strings.Join(PurgeTargets, ", "), PurgeTargetAll)
}
