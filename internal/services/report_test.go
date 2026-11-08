package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

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
	if err := store.AddReportSection(ctx, &models.ReportSectionData{
		ReportID: run.ID,
		Section:  "users",
		Columns:  string(columns),
		Rows:     string(rows),
		RowCount: 2,
	}); err != nil {
		t.Fatalf("AddReportSection = %v", err)
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

// TestReportSectionRowsColumnIsLongText pins the column that closed a report with
// nothing in it: "Error 1406 (22001): Data too long for column 'rows'".
//
// A section is a whole table of the directory — team_members and planner_plans are
// the ones that blow up first, because they hold one row per membership — and its
// JSON document is measured in megabytes on a large tenant, while a MySQL TEXT
// column stops at 65 535 bytes. The assertion reads the DDL GORM emits for MySQL,
// which is the dialect the failure came from: SQLite, the database of these tests,
// has no length limit and would never notice.
func TestReportSectionRowsColumnIsLongText(t *testing.T) {
	db, err := gorm.Open(mysql.New(mysql.Config{
		// The dialector only has to translate the model into its DDL, so no server
		// is contacted: the version probe (which would need one) is skipped.
		DSN:                       "sync:sync@tcp(127.0.0.1:3306)/sync?charset=utf8mb4",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("opening the MySQL dialect: %v", err)
	}
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(&models.ReportSectionData{}); err != nil {
		t.Fatalf("parsing the report section model: %v", err)
	}
	rows := stmt.Schema.LookUpField("Rows")
	if rows == nil {
		t.Fatal("the report section model has no Rows field")
	}
	if dataType := strings.ToLower(db.Migrator().FullDataTypeOf(rows).SQL); dataType != "longtext" {
		t.Fatalf("rows column type = %q, want longtext (a TEXT column caps at 65535 bytes)", dataType)
	}
}

// mustMarshal is the JSON document of value, for the comparisons of a test.
func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshalling %T: %v", value, err)
	}
	return encoded
}

// TestStoreTablesHoldsADocumentPastTheTextLimit stores a section bigger than a
// MySQL TEXT column accepts and reads it back whole.
func TestStoreTablesHoldsADocumentPastTheTextLimit(t *testing.T) {
	store := newTestStore(t)
	service := NewReportService(store, nil)
	ctx := context.Background()
	run := &models.DirectoryReport{
		AccountID:    9,
		AccountEmail: "admin@contoso.com",
		Provider:     string(models.ProviderMicrosoft),
		OrgID:        "tenant-9",
		Status:       string(models.RunRunning),
		Trigger:      string(models.TriggerManual),
		StartedAt:    time.Date(2026, 11, 8, 8, 0, 0, 0, time.UTC),
	}
	if err := store.CreateReport(ctx, run); err != nil {
		t.Fatalf("CreateReport = %v", err)
	}

	// Roughly 4 000 memberships of a big tenant: well past the 65 535 bytes of a
	// TEXT column, which is what made the INSERT fail.
	rows := make([]providers.ReportRow, 4000)
	for i := range rows {
		rows[i] = providers.ReportRow{
			fmt.Sprintf("team-%02d", i%40),
			fmt.Sprintf("member-%04d@contoso.com", i),
			"Member",
		}
	}
	tables := []providers.ReportTable{{
		Section: providers.ReportSectionTeamMembers,
		Columns: []providers.ReportColumn{
			{Key: "team", Title: "Team"},
			{Key: "member", Title: "Member"},
			{Key: "role", Title: "Role"},
		},
		Rows: rows,
	}}
	stored := service.storeTables(ctx, run.ID, tables)
	if len(stored.Missing) != 0 || stored.Sections != 1 || stored.Rows != len(rows) {
		t.Fatalf("storeTables = %+v, want the section stored with its %d rows", stored, len(rows))
	}
	if stored.Truncated {
		t.Error("storeTables reported a truncation, want the section stored whole")
	}

	section, err := store.GetReportSection(ctx, run.ID, string(providers.ReportSectionTeamMembers))
	if err != nil {
		t.Fatalf("GetReportSection = %v", err)
	}
	if len(section.Rows) <= 64*1024 {
		t.Fatalf("stored rows document = %d bytes, want more than the 65535 bytes of a TEXT column", len(section.Rows))
	}
	if section.RowCount != len(rows) || section.Truncated {
		t.Errorf("section row count = %d truncated = %v, want %d and false", section.RowCount, section.Truncated, len(rows))
	}
	var decoded []providers.ReportRow
	if err := json.Unmarshal([]byte(section.Rows), &decoded); err != nil {
		t.Fatalf("decoding the stored rows: %v", err)
	}
	if len(decoded) != len(rows) || decoded[len(rows)-1][1] != rows[len(rows)-1][1] {
		t.Fatalf("decoded %d rows, want the %d stored rows with the last one intact", len(decoded), len(rows))
	}
}

// TestStoreTablesKeepsTheSectionsAroundAFailingOne pins the blast radius of a
// section the database refuses: the tables beside it are still stored, and the
// refused one is named so the pass reports it like a section the provider could not
// collect.
//
// The refusal is produced with a duplicate section name, which the unique index
// idx_report_section rejects deterministically. Before, one INSERT carried every
// table of the pass, so a single document the database refused (an oversized one
// hitting the 65 535 bytes of a TEXT column) discarded all of them and closed the
// report empty.
func TestStoreTablesKeepsTheSectionsAroundAFailingOne(t *testing.T) {
	store := newTestStore(t)
	service := NewReportService(store, nil)
	ctx := context.Background()
	run := &models.DirectoryReport{
		AccountID: 11,
		Status:    string(models.RunRunning),
		Provider:  string(models.ProviderGoogle),
		Trigger:   string(models.TriggerManual),
		StartedAt: time.Date(2026, 11, 8, 8, 30, 0, 0, time.UTC),
	}
	if err := store.CreateReport(ctx, run); err != nil {
		t.Fatalf("CreateReport = %v", err)
	}
	columns := []providers.ReportColumn{{Key: "name", Title: "Name"}}
	tables := []providers.ReportTable{
		{Section: providers.ReportSectionDomains, Columns: columns, Rows: []providers.ReportRow{{"contoso.com"}}},
		{Section: providers.ReportSectionUsers, Columns: columns, Rows: []providers.ReportRow{{"Ada"}}},
		// The duplicate: refused by the unique index, as an oversized value was.
		{Section: providers.ReportSectionDomains, Columns: columns, Rows: []providers.ReportRow{{"contoso.com"}}},
		{Section: providers.ReportSectionGroups, Columns: columns, Rows: []providers.ReportRow{{"Engineering"}}},
	}
	stored := service.storeTables(ctx, run.ID, tables)
	if stored.Sections != 3 || stored.Rows != 3 {
		t.Errorf("storeTables = %+v, want the three sections it could write", stored)
	}
	if len(stored.Missing) != 1 || stored.Missing[0] != string(providers.ReportSectionDomains) {
		t.Fatalf("missing = %v, want the refused section named", stored.Missing)
	}
	for _, name := range []providers.ReportSection{providers.ReportSectionUsers, providers.ReportSectionGroups} {
		if _, err := store.GetReportSection(ctx, run.ID, string(name)); err != nil {
			t.Errorf("GetReportSection(%s) = %v, want the table stored", name, err)
		}
	}
}

// TestBuildReportSectionTrimsAtTheDocumentBudget pins the size budget: a section
// over it keeps the first rows that fit and is flagged truncated, and one that fits
// keeps everything along with the flags it came with.
func TestBuildReportSectionTrimsAtTheDocumentBudget(t *testing.T) {
	rows := make([]providers.ReportRow, 300)
	for i := range rows {
		rows[i] = providers.ReportRow{
			fmt.Sprintf("user-%03d", i),
			fmt.Sprintf("user-%03d@contoso.com", i),
			"Member",
		}
	}
	columns := []providers.ReportColumn{
		{Key: "display_name", Title: "Display name"},
		{Key: "email", Title: "Email"},
		{Key: "role", Title: "Role"},
	}
	budget := len(mustMarshal(t, rows)) / 3

	record, err := buildReportSection(7, providers.ReportTable{
		Section: providers.ReportSectionUsers,
		Columns: columns,
		Rows:    rows,
	}, budget)
	if err != nil {
		t.Fatalf("buildReportSection = %v", err)
	}
	if !record.Truncated {
		t.Error("truncated = false, want true for a section cut at the budget")
	}
	if len(record.Rows) > budget {
		t.Errorf("stored document = %d bytes, want at most the %d byte budget", len(record.Rows), budget)
	}
	if record.RowCount <= 0 || record.RowCount >= len(rows) {
		t.Fatalf("row count = %d, want a prefix of the %d rows", record.RowCount, len(rows))
	}
	var kept []providers.ReportRow
	if err := json.Unmarshal([]byte(record.Rows), &kept); err != nil {
		t.Fatalf("decoding the stored rows: %v", err)
	}
	if len(kept) != record.RowCount {
		t.Errorf("decoded %d rows, want the %d counted", len(kept), record.RowCount)
	}
	// The cut is the longest prefix that fits: one more row would be over budget,
	// which is what keeps the stored table as complete as the budget allows.
	if more := mustMarshal(t, rows[:record.RowCount+1]); len(more) <= budget {
		t.Errorf("rows up to %d = %d bytes, want more than the %d byte budget", record.RowCount+1, len(more), budget)
	}

	// A table under the budget is stored whole, and the truncation of the collector
	// survives: the row cap and the byte budget are reported the same way.
	fits, err := buildReportSection(7, providers.ReportTable{
		Section:   providers.ReportSectionUsers,
		Columns:   columns,
		Rows:      rows[:2],
		Truncated: true,
	}, budget)
	if err != nil {
		t.Fatalf("buildReportSection(fits) = %v", err)
	}
	if fits.RowCount != 2 || !fits.Truncated || fits.Rows != string(mustMarshal(t, rows[:2])) {
		t.Errorf("fits = %+v, want the two rows, the collector flag and the exact document", fits)
	}

	// A failed section keeps carrying its reason instead of rows.
	failed, err := buildReportSection(7, providers.ReportTable{
		Section: providers.ReportSectionLicenses,
		Error:   "403 Forbidden",
	}, budget)
	if err != nil {
		t.Fatalf("buildReportSection(failed) = %v", err)
	}
	if failed.Error != "403 Forbidden" || failed.RowCount != 0 {
		t.Errorf("failed = %+v, want the reason and no rows", failed)
	}
}
