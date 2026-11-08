// This file covers the polling policy of the activity feed collector: the three
// settings of Admin > Settings > Activity log capture, the gate the scheduler
// applies to them, and the pause that keeps a pass from spending a tenant's
// throttling limit on its own streams.

package services

import (
	"context"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// TestAuditLogPolicyDefaultsAndClamps checks what the policy reader makes of a
// fresh database and of one somebody edited by hand: the capture is armed at the
// documented interval, a stored value is honoured, and an extreme one is clamped
// instead of turning the loop into a request storm or a sleep.
func TestAuditLogPolicyDefaultsAndClamps(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := NewAuditLogService(store, nil)

	// Nothing stored yet: the scheduled pass runs every ten minutes, unpaced.
	policy := service.Policy(ctx)
	if !policy.Enabled {
		t.Error("the scheduled activity feed pass must be armed by default")
	}
	if policy.Interval != DefaultAuditLogInterval {
		t.Fatalf("interval = %s, want the %s default", policy.Interval, DefaultAuditLogInterval)
	}
	if DefaultAuditLogInterval != 10*time.Minute || DefaultAuditLogIntervalMinutes != 10 {
		t.Fatalf("the default interval = %s, want the documented ten minutes", DefaultAuditLogInterval)
	}
	if policy.RequestDelay != 0 {
		t.Fatalf("request delay = %s, want an unpaced pass by default", policy.RequestDelay)
	}

	// What an operator saved is what the next pass reads.
	setSettings(t, store, map[string]string{
		models.SettingAuditLogsEnabled:             "false",
		models.SettingAuditLogsIntervalMinutes:     "30",
		models.SettingAuditLogsRequestDelaySeconds: "45",
	})
	policy = service.Policy(ctx)
	if policy.Enabled || policy.Interval != 30*time.Minute || policy.RequestDelay != 45*time.Second {
		t.Fatalf("policy = %+v, want the stored 30m/45s with the capture off", policy)
	}

	// A hand-edited database cannot turn the collector into a request storm
	// (0 minutes) or a pass that never resumes, and a negative pause reads as
	// "unpaced" rather than as an error.
	setSettings(t, store, map[string]string{
		models.SettingAuditLogsIntervalMinutes:     "0",
		models.SettingAuditLogsRequestDelaySeconds: "-10",
	})
	policy = service.Policy(ctx)
	if policy.Interval != MinAuditLogIntervalMinutes*time.Minute {
		t.Errorf("interval = %s, want the %d minute floor", policy.Interval, MinAuditLogIntervalMinutes)
	}
	if policy.RequestDelay != 0 {
		t.Errorf("request delay = %s, want a negative pause to read as unpaced", policy.RequestDelay)
	}

	setSettings(t, store, map[string]string{
		models.SettingAuditLogsIntervalMinutes:     "5000",
		models.SettingAuditLogsRequestDelaySeconds: "9000",
	})
	policy = service.Policy(ctx)
	if policy.Interval != MaxAuditLogIntervalMinutes*time.Minute {
		t.Errorf("interval = %s, want the %d minute ceiling", policy.Interval, MaxAuditLogIntervalMinutes)
	}
	if policy.RequestDelay != MaxAuditLogRequestDelaySeconds*time.Second {
		t.Errorf("request delay = %s, want the %d second ceiling", policy.RequestDelay, MaxAuditLogRequestDelaySeconds)
	}
}

// TestScheduledAuditLogsHonourTheSwitchAndTheInterval pins the gate of the logs
// loop: with the capture off nothing is read and nothing is scheduled, with it on
// the first tick polls immediately and the following ones wait for the interval,
// and a forced pass counts as the previous pass.
func TestScheduledAuditLogsHonourTheSwitchAndTheInterval(t *testing.T) {
	ctx := context.Background()
	env := newActivityEnv(t)
	env.connect(t, "admin@contoso.com", "tenant-a", models.AccountConnected)
	env.provider.events["directory"] = []providers.ActivityEvent{
		activityEventOf("directory", "dir-1", models.AuditLogCreate, "admin@contoso.com", "Bob New",
			time.Date(2026, 11, 8, 8, 0, 0, 0, time.UTC)),
	}
	clock := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	scheduler := &Scheduler{store: env.store, logs: env.service, now: func() time.Time { return clock }}

	// The capture is off: the tick reads nothing, and the answer says why the
	// list stopped growing by leaving the next pass out.
	setSettings(t, env.store, map[string]string{models.SettingAuditLogsEnabled: "false"})
	if _, ran := scheduler.RunScheduledAuditLogs(ctx); ran {
		t.Fatal("a pass ran with the capture switched off")
	}
	if calls := env.provider.callsFor("directory"); len(calls) != 0 {
		t.Fatalf("the feed was read %d times with the capture off, want 0", len(calls))
	}
	schedule := scheduler.AuditLogSchedule(ctx)
	if schedule.Enabled || schedule.NextRunAt != nil || schedule.LastRunAt != nil {
		t.Fatalf("schedule = %+v, want an off switch with nothing scheduled", schedule)
	}

	// Switching it on arms the loop: the next tick polls immediately, because a
	// fresh process has not run a pass yet.
	setSettings(t, env.store, map[string]string{
		models.SettingAuditLogsEnabled:         "true",
		models.SettingAuditLogsIntervalMinutes: "10",
	})
	result, ran := scheduler.RunScheduledAuditLogs(ctx)
	if !ran {
		t.Fatal("the first tick after switching the capture on read nothing")
	}
	if result.Organisations != 1 || result.Events != 1 {
		t.Fatalf("pass = %+v, want the organisation read once with one event", result)
	}
	schedule = scheduler.AuditLogSchedule(ctx)
	if !schedule.Enabled || schedule.IntervalMinutes != 10 {
		t.Fatalf("schedule = %+v, want the stored ten minute interval", schedule)
	}
	if schedule.LastRunAt == nil || !schedule.LastRunAt.Equal(clock) {
		t.Fatalf("last_run_at = %v, want the pass that just finished", schedule.LastRunAt)
	}
	if schedule.NextRunAt == nil || !schedule.NextRunAt.Equal(clock.Add(10*time.Minute)) {
		t.Fatalf("next_run_at = %v, want the pass one interval later", schedule.NextRunAt)
	}

	// The tick a minute later is too early: the interval is measured from the
	// end of the previous pass, not from the wake-up.
	clock = clock.Add(time.Minute)
	if _, ran := scheduler.RunScheduledAuditLogs(ctx); ran {
		t.Fatal("a second pass ran before the interval elapsed")
	}
	if calls := env.provider.callsFor("directory"); len(calls) != 1 {
		t.Fatalf("the feed was read %d times, want the single pass", len(calls))
	}

	// A forced pass ignores the interval — that is what Collect now promises —
	// and moves the schedule, so the loop does not poll again right after it.
	scheduler.RunAuditLogs(ctx)
	if calls := env.provider.callsFor("directory"); len(calls) != 2 {
		t.Fatalf("the feed was read %d times, want the forced pass included", len(calls))
	}
	if _, total, err := env.store.ListAuditLogEvents(ctx, AuditLogFilter{}); err != nil || total != 1 {
		t.Fatalf("stored events = %d (%v), want the re-read window to stay one event", total, err)
	}
	schedule = scheduler.AuditLogSchedule(ctx)
	if schedule.NextRunAt == nil || !schedule.NextRunAt.Equal(clock.Add(10*time.Minute)) {
		t.Fatalf("next_run_at = %v, want the interval restarted by the forced pass", schedule.NextRunAt)
	}

	// One interval after the forced pass the loop polls again on its own.
	clock = clock.Add(10 * time.Minute)
	if _, ran := scheduler.RunScheduledAuditLogs(ctx); !ran {
		t.Fatal("the tick after the interval elapsed read nothing")
	}
	if calls := env.provider.callsFor("directory"); len(calls) != 3 {
		t.Fatalf("the feed was read %d times, want 3", len(calls))
	}

	// The pause an operator configured is reported beside the interval, so the
	// screen can explain a pass that looks slow.
	setSettings(t, env.store, map[string]string{models.SettingAuditLogsRequestDelaySeconds: "3"})
	if schedule = scheduler.AuditLogSchedule(ctx); schedule.RequestDelaySeconds != 3 {
		t.Fatalf("request_delay_seconds = %d, want the stored 3", schedule.RequestDelaySeconds)
	}
}

// TestAuditLogPaceWaitsBetweenCalls pins the pacing primitive on its own: the first
// call of a pass never waits, every later one does, and a cancelled pass answers
// false instead of sleeping for the whole pause.
func TestAuditLogPaceWaitsBetweenCalls(t *testing.T) {
	ctx := context.Background()

	// No pause configured: the calls follow one another.
	unpaced := &auditLogPace{}
	for i := 0; i < 3; i++ {
		if !unpaced.wait(ctx) {
			t.Fatalf("call %d was refused on an unpaced pass", i+1)
		}
	}

	// The first call runs immediately even with a pause of an hour; the second
	// one waits it out (cancelled here, so the test stays fast).
	long := &auditLogPace{delay: time.Hour}
	if !long.wait(ctx) {
		t.Fatal("the first call of a pass must not wait")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if long.wait(cancelled) {
		t.Fatal("a cancelled pass waited out the pause instead of stopping")
	}

	// A short pause is really spent before the next call goes out.
	short := &auditLogPace{delay: 30 * time.Millisecond}
	if !short.wait(ctx) {
		t.Fatal("the first call of a paced pass was refused")
	}
	started := time.Now()
	if !short.wait(ctx) {
		t.Fatal("the second call of a paced pass was refused")
	}
	if elapsed := time.Since(started); elapsed < 30*time.Millisecond {
		t.Fatalf("the pause between two calls lasted %s, want at least 30ms", elapsed)
	}
}

// TestCollectPacesTheCallsOfOnePass pins the wiring: the pause reaches the streams
// of a pass through the policy, so the two feeds of one organisation are no longer
// read back to back.
func TestCollectPacesTheCallsOfOnePass(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "admin@contoso.com", "tenant-a", models.AccountConnected)
	setSettings(t, env.store, map[string]string{models.SettingAuditLogsRequestDelaySeconds: "1"})

	started := time.Now()
	result := env.service.Collect(context.Background())
	elapsed := time.Since(started)
	if result.Streams != 2 || result.Failed != 0 {
		t.Fatalf("streams/failed = %d/%d, want the two feeds read", result.Streams, result.Failed)
	}
	if elapsed < time.Second {
		t.Fatalf("the pass lasted %s, want the one configured pause between its two calls", elapsed)
	}
}

// TestCollectStopsWhilePacing pins the other half of the pause: a cancelled pass
// leaves the remaining streams unread instead of waiting out the pause and then
// calling the provider with a dead context.
func TestCollectStopsWhilePacing(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "admin@contoso.com", "tenant-a", models.AccountConnected)
	setSettings(t, env.store, map[string]string{
		models.SettingAuditLogsRequestDelaySeconds: "300",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// The first call is made without a pause, so cancelling after a short
		// wait lands inside the pause before the second one.
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	result := env.service.Collect(ctx)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the cancelled pass lasted %s, want it to abandon the pause", elapsed)
	}
	if result.Streams != 1 {
		t.Fatalf("streams = %d, want the single stream read before the pause", result.Streams)
	}
	if calls := env.provider.callsFor("unified"); len(calls) != 0 {
		t.Fatalf("the second feed was read %d times after the cancellation, want 0", len(calls))
	}
}
