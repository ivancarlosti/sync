package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// stubReportSource is a ReportSource with a fixed catalogue, which is enough to
// test the section negotiation of the service.
type stubReportSource struct {
	sections []providers.ReportSection
}

func (s stubReportSource) ReportSections() []providers.ReportSection { return s.sections }

func (s stubReportSource) CollectReport(context.Context, providers.Credentials, *providers.Tokens, []providers.ReportSection, func(providers.ReportProgress)) ([]providers.ReportTable, error) {
	return nil, nil
}

// TestReportServiceScheduleOptIn pins the per-account opt-in: the default
// interval, the bounds the UI cannot exceed, one row per account however often
// it is saved, and the next slot following the enabled flag.
func TestReportServiceScheduleOptIn(t *testing.T) {
	store := newTestStore(t)
	service := NewReportService(store, nil)
	now := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	ctx := context.Background()

	schedule, err := service.SaveSchedule(ctx, ReportScheduleInput{AccountID: 7, Enabled: true})
	if err != nil {
		t.Fatalf("SaveSchedule = %v", err)
	}
	if schedule.IntervalMinutes != DefaultReportInterval {
		t.Errorf("interval = %d, want the default %d", schedule.IntervalMinutes, DefaultReportInterval)
	}
	if schedule.NextRunAt == nil || !schedule.NextRunAt.Equal(now.Add(DefaultReportInterval*time.Minute)) {
		t.Errorf("next run = %v, want now plus the default interval", schedule.NextRunAt)
	}
	if len(schedule.SectionList()) != len(providers.ReportSections) {
		t.Errorf("sections = %v, want every section for an empty request", schedule.SectionList())
	}

	// A second save edits the same row instead of creating one.
	updated, err := service.SaveSchedule(ctx, ReportScheduleInput{
		AccountID:       7,
		Enabled:         false,
		IntervalMinutes: 60,
		Sections:        []string{"users"},
	})
	if err != nil {
		t.Fatalf("SaveSchedule = %v", err)
	}
	if updated.ID != schedule.ID {
		t.Errorf("schedule id = %d, want the existing row %d", updated.ID, schedule.ID)
	}
	if updated.NextRunAt != nil {
		t.Errorf("next run = %v, want none while the schedule is disabled", updated.NextRunAt)
	}
	if list := updated.SectionList(); len(list) != 1 || list[0] != "users" {
		t.Errorf("sections = %v, want the one requested section", list)
	}

	schedules, err := service.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules = %v", err)
	}
	if len(schedules) != 1 {
		t.Fatalf("schedules = %d, want one row per account", len(schedules))
	}

	// An interval outside the bounds is a validation error, not a silent clamp.
	if _, err := service.SaveSchedule(ctx, ReportScheduleInput{AccountID: 7, Enabled: true, IntervalMinutes: 1}); !errors.Is(err, ErrValidation) {
		t.Errorf("SaveSchedule(1 minute) = %v, want a validation error", err)
	}
	if _, err := service.SaveSchedule(ctx, ReportScheduleInput{AccountID: 0}); !errors.Is(err, ErrValidation) {
		t.Errorf("SaveSchedule(no account) = %v, want a validation error", err)
	}

	if err := service.DeleteSchedule(ctx, 7); err != nil {
		t.Fatalf("DeleteSchedule = %v", err)
	}
	if schedules, err = service.ListSchedules(ctx); err != nil {
		t.Fatalf("ListSchedules = %v", err)
	}
	if len(schedules) != 0 {
		t.Fatalf("schedules = %d, want the opt-in removed", len(schedules))
	}
	// Deleting twice stays a no-op.
	if err := service.DeleteSchedule(ctx, 7); err != nil {
		t.Fatalf("DeleteSchedule(again) = %v", err)
	}
}

// TestReportStoreLifecycle exercises the tables of a report the way the service
// and the handlers use them: insert, read back, prune, and recover the rows a
// restart left running.
func TestReportStoreLifecycle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

	run := &models.DirectoryReport{
		AccountID:    3,
		AccountEmail: "admin@contoso.com",
		Provider:     string(models.ProviderMicrosoft),
		OrgID:        "tenant-1",
		Status:       string(models.RunRunning),
		Trigger:      string(models.TriggerManual),
		StartedAt:    started,
	}
	run.SetSections([]string{"users", "domains"})
	if err := store.CreateReport(ctx, run); err != nil {
		t.Fatalf("CreateReport = %v", err)
	}
	if run.Sections != "users,domains" {
		t.Fatalf("sections = %q, want the comma separated list", run.Sections)
	}

	running, err := store.RunningReport(ctx, 3)
	if err != nil {
		t.Fatalf("RunningReport = %v", err)
	}
	if running.ID != run.ID {
		t.Fatalf("running report = %d, want %d", running.ID, run.ID)
	}
	if _, err := store.RunningReport(ctx, 4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RunningReport(other account) = %v, want ErrNotFound", err)
	}

	columns, err := json.Marshal([]providers.ReportColumn{{Key: "display_name", Title: "Display name"}})
	if err != nil {
		t.Fatalf("marshalling the columns: %v", err)
	}
	rows, err := json.Marshal([]providers.ReportRow{{"Ada"}, {"Grace"}})
	if err != nil {
		t.Fatalf("marshalling the rows: %v", err)
	}
	if err := store.AddReportSections(ctx, []models.ReportSectionData{{
		ReportID: run.ID,
		Section:  "users",
		Columns:  string(columns),
		Rows:     string(rows),
		RowCount: 2,
	}}); err != nil {
		t.Fatalf("AddReportSections = %v", err)
	}

	sections, err := store.ListReportSections(ctx, run.ID)
	if err != nil {
		t.Fatalf("ListReportSections = %v", err)
	}
	if len(sections) != 1 || sections[0].RowCount != 2 {
		t.Fatalf("sections = %+v, want the stored users table", sections)
	}
	section, err := store.GetReportSection(ctx, run.ID, "users")
	if err != nil {
		t.Fatalf("GetReportSection = %v", err)
	}
	var decoded []providers.ReportRow
	if err := json.Unmarshal([]byte(section.Rows), &decoded); err != nil {
		t.Fatalf("decoding the rows: %v", err)
	}
	if len(decoded) != 2 || decoded[1][0] != "Grace" {
		t.Fatalf("rows = %v, want the two stored rows", decoded)
	}
	if _, err := store.GetReportSection(ctx, run.ID, "domains"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetReportSection(missing) = %v, want ErrNotFound", err)
	}

	finished := started.Add(2 * time.Minute)
	run.Status = string(models.RunSuccess)
	run.FinishedAt = &finished
	run.DurationMS = finished.Sub(started).Milliseconds()
	run.SectionCount = 1
	run.RowCount = 2
	if err := store.FinishReport(ctx, run); err != nil {
		t.Fatalf("FinishReport = %v", err)
	}
	if _, err := store.RunningReport(ctx, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RunningReport after finish = %v, want ErrNotFound", err)
	}

	// The pruner keeps the newest report of every account and takes the tables
	// of the ones it drops with it. "Newest" is the highest id, which is the
	// order the history screen shows.
	newer := &models.DirectoryReport{
		AccountID: 3,
		Status:    string(models.RunSuccess),
		StartedAt: started.Add(time.Hour),
	}
	if err := store.CreateReport(ctx, newer); err != nil {
		t.Fatalf("CreateReport = %v", err)
	}
	if err := store.PruneReports(ctx, 1); err != nil {
		t.Fatalf("PruneReports = %v", err)
	}
	if _, err := store.GetReport(ctx, newer.ID); err != nil {
		t.Fatalf("the newest report was pruned: %v", err)
	}
	if _, err := store.GetReport(ctx, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetReport(pruned) = %v, want ErrNotFound", err)
	}
	if sections, err := store.ListReportSections(ctx, run.ID); err != nil {
		t.Fatalf("ListReportSections = %v", err)
	} else if len(sections) != 0 {
		t.Fatalf("sections = %d, want the tables of a pruned report gone with it", len(sections))
	}

	// A restart closes whatever was left running.
	interrupted := &models.DirectoryReport{
		AccountID: 9,
		Status:    string(models.RunRunning),
		StartedAt: started,
	}
	if err := store.CreateReport(ctx, interrupted); err != nil {
		t.Fatalf("CreateReport = %v", err)
	}
	closed, err := store.FailInterruptedReports(ctx, started.Add(time.Hour))
	if err != nil {
		t.Fatalf("FailInterruptedReports = %v", err)
	}
	if closed < 1 {
		t.Fatalf("closed = %d, want the interrupted report", closed)
	}
	stored, err := store.GetReport(ctx, interrupted.ID)
	if err != nil {
		t.Fatalf("GetReport = %v", err)
	}
	if stored.Status != string(models.RunFailed) || stored.FinishedAt == nil {
		t.Fatalf("interrupted report = %+v, want a finished failure", stored)
	}
}

// TestSupportedSectionsNegotiatesTheCatalogue pins the intersection between what
// was requested and what the provider can collect, in the provider's order.
func TestSupportedSectionsNegotiatesTheCatalogue(t *testing.T) {
	source := stubReportSource{sections: []providers.ReportSection{
		providers.ReportSectionUsers,
		providers.ReportSectionDomains,
	}}
	requested := []providers.ReportSection{
		providers.ReportSectionDomains,
		providers.ReportSectionSites,
		providers.ReportSectionUsers,
	}
	kept := supportedSections(source, requested)
	if len(kept) != 2 || kept[0] != providers.ReportSectionUsers || kept[1] != providers.ReportSectionDomains {
		t.Fatalf("supportedSections = %v, want the two supported sections in catalogue order", kept)
	}
	if kept := supportedSections(stubReportSource{}, []providers.ReportSection{providers.ReportSectionUsers}); len(kept) != 0 {
		t.Fatalf("supportedSections = %v, want none for a provider without sections", kept)
	}
}

// TestReportStatusMapping pins the lifecycle of a pass: a cancelled context is a
// cancellation, an expired budget a timeout, a failed section a partial success.
func TestReportStatusMapping(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelTimeout := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelTimeout()

	cases := []struct {
		name   string
		ctx    context.Context
		err    error
		failed int
		want   models.RunStatus
	}{
		{name: "success", ctx: context.Background(), want: models.RunSuccess},
		{name: "partial", ctx: context.Background(), failed: 1, want: models.RunPartial},
		{name: "failed", ctx: context.Background(), err: errors.New("boom"), want: models.RunFailed},
		{name: "cancelled", ctx: cancelled, err: context.Canceled, want: models.RunCancelled},
		{name: "timeout", ctx: expired, err: context.DeadlineExceeded, want: models.RunTimeout},
	}
	for _, test := range cases {
		if got := reportStatus(test.ctx, test.err, test.failed); got != test.want {
			t.Errorf("%s: reportStatus = %q, want %q", test.name, got, test.want)
		}
	}
	if message := reportMessage(string(models.RunPartial), 2, nil); message == "" {
		t.Error("reportMessage returned an empty summary")
	}
	// The summary names the sections that failed, so a `partial` row in the
	// history says what is missing and not only how many tables are.
	message := reportMessage(string(models.RunPartial), 2, []string{"licenses", "shared_drive_members"})
	if !strings.Contains(message, "licenses") || !strings.Contains(message, "shared_drive_members") {
		t.Errorf("reportMessage = %q, want the names of the failed sections", message)
	}
	if named := reportMessage(string(models.RunSuccess), 2, []string{"licenses"}); named != "completed" {
		t.Errorf("reportMessage(success) = %q, want the plain success summary", named)
	}
}
