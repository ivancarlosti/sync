// This file covers the retention service behind Admin > Settings > Purge
// (purge.go): the policy read from the settings, the target resolution and the
// automatic pass the scheduler runs.

package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// setSettings writes a few settings keys, the way an administrator would.
func setSettings(t *testing.T, store *Store, values map[string]string) {
	t.Helper()
	if err := store.Settings().SetMany(context.Background(), values); err != nil {
		t.Fatalf("writing the settings %v: %v", values, err)
	}
}

// TestPurgePolicyDefaultsAndClamps checks what the policy reader makes of a
// fresh database and of one somebody edited by hand.
func TestPurgePolicyDefaultsAndClamps(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := NewPurgeService(store)

	// Nothing stored yet: the automatic pass is on and every target keeps the
	// default retention.
	policy := service.Policy(ctx)
	if !policy.Enabled {
		t.Error("the automatic purge must be armed by default")
	}
	if policy.JobsDays != DefaultRetentionDays || policy.AuditsDays != DefaultRetentionDays || policy.BillingDays != DefaultRetentionDays {
		t.Fatalf("policy = %+v, want the %d day default", policy, DefaultRetentionDays)
	}

	// The three ages move independently.
	setSettings(t, store, map[string]string{
		models.SettingPurgeEnabled:     "false",
		models.SettingPurgeJobsDays:    "30",
		models.SettingPurgeAuditsDays:  "400",
		models.SettingPurgeBillingDays: "90",
	})
	policy = service.Policy(ctx)
	if policy.Enabled || policy.JobsDays != 30 || policy.AuditsDays != 400 || policy.BillingDays != 90 {
		t.Fatalf("policy = %+v, want the stored 30/400/90 with the pass off", policy)
	}
	if days := policy.Days(PurgeTargetJobs); days != 30 {
		t.Fatalf("days(jobs) = %d, want 30", days)
	}
	if days := policy.Days("nonsense"); days != 0 {
		t.Fatalf("days(nonsense) = %d, want 0", days)
	}

	// A hand-edited database cannot make the purge erase everything or nothing.
	setSettings(t, store, map[string]string{
		models.SettingPurgeJobsDays:    "0",
		models.SettingPurgeBillingDays: "999999",
	})
	policy = service.Policy(ctx)
	if policy.JobsDays != MinRetentionDays || policy.BillingDays != MaxRetentionDays {
		t.Fatalf("policy = %+v, want the retentions clamped to %d...%d",
			policy, MinRetentionDays, MaxRetentionDays)
	}
}

// TestPurgeTargetsValidation covers what a caller may ask for.
func TestPurgeTargetsValidation(t *testing.T) {
	for _, target := range []string{"jobs", " JOBS ", "audits", "billing"} {
		targets, err := purgeTargets(target)
		if err != nil || len(targets) != 1 {
			t.Fatalf("purgeTargets(%q) = %v (%v), want the single target", target, targets, err)
		}
	}
	for _, target := range []string{"", "all", "ALL"} {
		targets, err := purgeTargets(target)
		if err != nil || len(targets) != len(PurgeTargets) {
			t.Fatalf("purgeTargets(%q) = %v (%v), want every target", target, targets, err)
		}
	}
	if _, err := purgeTargets("files"); !errors.Is(err, ErrValidation) {
		t.Fatalf("purgeTargets(files): err = %v, want ErrValidation", err)
	}

	ctx := context.Background()
	store := newTestStore(t)
	service := NewPurgeService(store)
	if _, err := service.Purge(ctx, "files", 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("purging an unknown target: err = %v, want ErrValidation", err)
	}
	for _, days := range []int{-1, MaxRetentionDays + 1} {
		if _, err := service.Purge(ctx, PurgeTargetJobs, days); !errors.Is(err, ErrValidation) {
			t.Fatalf("purging %d days: err = %v, want ErrValidation", days, err)
		}
	}
}

// TestPurgeServiceErasesPerTarget checks that a pass touches the tables of the
// targets it was asked for, with the age it resolved, and leaves the others
// alone.
func TestPurgeServiceErasesPerTarget(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := NewPurgeService(store)
	job := seedJob(t, store, "google to microsoft")
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	// The clock is pinned so the cutoff of each target is predictable.
	service.now = func() time.Time { return now }

	seedRun(t, store, job, now.AddDate(0, 0, -400), models.RunSuccess)
	seedRun(t, store, job, now.AddDate(0, 0, -100), models.RunSuccess)
	seedRun(t, store, job, now.AddDate(0, 0, -2), models.RunSuccess)
	seedAudit(t, store, now.AddDate(0, 0, -400), models.RunSuccess)
	seedAudit(t, store, now.AddDate(0, 0, -2), models.RunSuccess)

	// The configured policy: runs go after 180 days, audits after a year, and
	// the traffic follows the runs.
	setSettings(t, store, map[string]string{
		models.SettingPurgeJobsDays:    "180",
		models.SettingPurgeAuditsDays:  "365",
		models.SettingPurgeBillingDays: "180",
	})

	// The traffic goes by its own clock, on the day bucket: only the rows of
	// the oldest run are old enough, and the run itself stays.
	outcome, err := service.Purge(ctx, PurgeTargetBilling, 180)
	if err != nil {
		t.Fatalf("purging the traffic: %v", err)
	}
	if outcome.Target != PurgeTargetBilling || outcome.Removed.Billing != 1 {
		t.Fatalf("outcome = %+v, want the single aged traffic row", outcome)
	}
	if outcome.Removed.Jobs != 0 || outcome.Removed.Audits != 0 {
		t.Fatalf("outcome = %+v, want the other targets untouched", outcome)
	}
	if outcome.Days[PurgeTargetBilling] != 180 || len(outcome.Days) != 1 {
		t.Fatalf("days = %v, want the billing retention alone", outcome.Days)
	}
	if runs := countRows(t, store, &models.SyncRun{}, ""); runs != 3 {
		t.Fatalf("sync_runs = %d, want the runs untouched by a traffic purge", runs)
	}

	// The runs follow the configured policy: the single run older than 180 days
	// goes with its items, and its traffic row went with the traffic pass.
	outcome, err = service.Purge(ctx, PurgeTargetJobs, 0)
	if err != nil {
		t.Fatalf("purging the runs: %v", err)
	}
	if outcome.Removed.Jobs != 1 || outcome.Removed.Audits != 0 {
		t.Fatalf("outcome = %+v, want the single aged run and no audit", outcome)
	}
	if outcome.Days[PurgeTargetJobs] != 180 || len(outcome.Days) != 1 {
		t.Fatalf("days = %v, want the jobs retention alone", outcome.Days)
	}
	// The traffic of a purged run goes with it, but it is not the billing
	// target: that one counts the rows it drops on its own.
	if outcome.Removed.Billing != 0 {
		t.Fatalf("billing rows = %d, want them counted by the billing target only", outcome.Removed.Billing)
	}
	if traffic := countRows(t, store, &models.TrafficEntry{}, ""); traffic != 2 {
		t.Fatalf("traffic_entries = %d, want the rows of the purged run gone", traffic)
	}
	if audits := countRows(t, store, &models.AuditRun{}, ""); audits != 2 {
		t.Fatalf("audit_runs = %d, want the audits untouched by a jobs purge", audits)
	}

	// An explicit age overrides the policy, without writing it back.
	outcome, err = service.Purge(ctx, PurgeTargetAudits, 30)
	if err != nil {
		t.Fatalf("purging the audits: %v", err)
	}
	if outcome.Removed.Audits != 1 || outcome.Days[PurgeTargetAudits] != 30 {
		t.Fatalf("outcome = %+v, want the aged audit erased at 30 days", outcome)
	}
	if stored := NewPurgeService(store).Policy(ctx).AuditsDays; stored != 365 {
		t.Fatalf("stored audits retention = %d, want the policy left at 365", stored)
	}

	// `all` runs the three targets in one pass.
	outcome, err = service.Purge(ctx, PurgeTargetAll, 0)
	if err != nil {
		t.Fatalf("purging everything: %v", err)
	}
	if len(outcome.Days) != len(PurgeTargets) {
		t.Fatalf("days = %v, want one entry per target", outcome.Days)
	}
	if outcome.Removed.Total() != 0 {
		t.Fatalf("outcome = %+v, want nothing left to erase", outcome)
	}
}

// TestPurgeRunHonoursTheSwitchAndTheScheduler checks the automatic pass: it does
// nothing while an administrator has it off, and it applies the configured ages
// when on — which is what the scheduler calls every few hours.
func TestPurgeRunHonoursTheSwitchAndTheScheduler(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := NewPurgeService(store)
	job := seedJob(t, store, "google to microsoft")
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	seedRun(t, store, job, now.AddDate(0, 0, -40), models.RunSuccess)
	seedAudit(t, store, now.AddDate(0, 0, -40), models.RunSuccess)
	setSettings(t, store, map[string]string{
		models.SettingPurgeEnabled:     "false",
		models.SettingPurgeJobsDays:    "30",
		models.SettingPurgeAuditsDays:  "30",
		models.SettingPurgeBillingDays: "30",
	})

	// Switched off: the pass is a no-op, and the manual purge still works.
	if outcome, err := service.Run(ctx); err != nil || outcome.Removed.Total() != 0 {
		t.Fatalf("the disabled pass = %+v (%v), want nothing erased", outcome, err)
	}
	if runs := countRows(t, store, &models.SyncRun{}, ""); runs != 1 {
		t.Fatalf("sync_runs = %d, want the run kept while the pass is off", runs)
	}
	if _, err := service.Purge(ctx, PurgeTargetJobs, 0); err != nil {
		t.Fatalf("the manual purge must work with the automatic pass off: %v", err)
	}

	// Switched on: the scheduler erases what aged out of every target.
	setSettings(t, store, map[string]string{models.SettingPurgeEnabled: "true"})
	scheduler := NewScheduler(store, nil, nil, nil)
	scheduler.purge.now = func() time.Time { return now }
	removed := scheduler.RunRetention(ctx)
	if removed.Audits != 1 {
		t.Fatalf("removed = %+v, want the aged audit erased by the retention pass", removed)
	}
	if audits := countRows(t, store, &models.AuditRun{}, ""); audits != 0 {
		t.Fatalf("audit_runs = %d, want none left", audits)
	}
}
