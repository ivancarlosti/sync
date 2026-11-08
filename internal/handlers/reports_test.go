// This file covers the HTTP surface of the directory reports (reports.go): the
// section catalogue, the start request, the stored tables, the ZIP export and the
// per-account schedule.

package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// reportAnswer is the JSON shape of the report views, declared here so the
// assertions also pin the field names the SPA reads.
type reportAnswer struct {
	ID           uint     `json:"id"`
	AccountID    uint     `json:"account_id"`
	OrgID        string   `json:"org_id"`
	Status       string   `json:"status"`
	SectionCount int      `json:"section_count"`
	RowCount     int      `json:"row_count"`
	Failed       int      `json:"failed"`
	Running      bool     `json:"running"`
	SectionList  []string `json:"section_list"`
}

// reportPath builds the API path of one report.
func reportPath(id uint, suffix string) string {
	return "/api/reports/" + strconv.FormatUint(uint64(id), 10) + suffix
}

// seedReport stores one finished report with a users table of two rows.
func seedReport(t *testing.T, store interface {
	CreateReport(context.Context, *models.DirectoryReport) error
	AddReportSection(context.Context, *models.ReportSectionData) error
}) *models.DirectoryReport {
	t.Helper()
	started := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	report := &models.DirectoryReport{
		AccountID:    4,
		AccountEmail: "admin@contoso.com",
		Provider:     string(models.ProviderMicrosoft),
		OrgID:        "tenant-1",
		Status:       string(models.RunSuccess),
		Trigger:      string(models.TriggerManual),
		StartedAt:    started,
		SectionCount: 1,
		RowCount:     2,
	}
	report.SetSections([]string{"users"})
	if err := store.CreateReport(context.Background(), report); err != nil {
		t.Fatalf("seeding the report: %v", err)
	}
	columns, err := json.Marshal([]providers.ReportColumn{
		{Key: "display_name", Title: "Display name"},
		{Key: "user_type", Title: "User type"},
	})
	if err != nil {
		t.Fatalf("encoding the columns: %v", err)
	}
	rows, err := json.Marshal([]providers.ReportRow{
		{"Ada Lovelace", "Member"},
		{"Guest User", "Guest"},
	})
	if err != nil {
		t.Fatalf("encoding the rows: %v", err)
	}
	if err := store.AddReportSection(context.Background(), &models.ReportSectionData{
		ReportID: report.ID,
		Section:  "users",
		Columns:  string(columns),
		Rows:     string(rows),
		RowCount: 2,
	}); err != nil {
		t.Fatalf("seeding the report section: %v", err)
	}
	return report
}

// TestReportSectionsListsTheVocabulary pins the catalogue the start form offers.
func TestReportSectionsListsTheVocabulary(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	recorder := call(t, server, http.MethodGet, "/api/reports/sections", nil)
	requireStatus(t, recorder, http.StatusOK)
	answer := decodeJSON[struct {
		Sections  []string `json:"sections"`
		Supported bool     `json:"supported"`
	}](t, recorder)
	if !answer.Supported {
		t.Error("supported = false, want the vocabulary without an account")
	}
	if len(answer.Sections) != len(providers.ReportSections) {
		t.Fatalf("sections = %v, want the whole vocabulary", answer.Sections)
	}
	for _, section := range answer.Sections {
		if !providers.ReportSection(section).Valid() {
			t.Errorf("unknown section %q in the catalogue", section)
		}
	}
}

// TestStartReportValidatesTheRequest pins the two 400s: no account, and a section
// outside the vocabulary.
func TestStartReportValidatesTheRequest(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	recorder := call(t, server, http.MethodPost, "/api/reports", map[string]any{})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	recorder = call(t, server, http.MethodPost, "/api/reports", map[string]any{
		"account_id": 1,
		"sections":   []string{"channels"},
	})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)
}

// TestReportHistoryAndStoredTables pins the read side: the history list, the
// table of contents and one page of a table.
func TestReportHistoryAndStoredTables(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	seeded := seedReport(t, store)

	recorder := call(t, server, http.MethodGet, "/api/reports", nil)
	requireStatus(t, recorder, http.StatusOK)
	list := decodeJSON[struct {
		Reports []reportAnswer `json:"reports"`
	}](t, recorder)
	if len(list.Reports) != 1 {
		t.Fatalf("reports = %d, want the seeded one", len(list.Reports))
	}
	if len(list.Reports[0].SectionList) != 1 {
		t.Errorf("section_list = %v, want the stored section as an array", list.Reports[0].SectionList)
	}

	recorder = call(t, server, http.MethodGet, reportPath(seeded.ID, ""), nil)
	requireStatus(t, recorder, http.StatusOK)
	detail := decodeJSON[struct {
		Report   reportAnswer `json:"report"`
		Sections []struct {
			Section  string `json:"section"`
			RowCount int    `json:"row_count"`
			Columns  []struct {
				Key   string `json:"key"`
				Title string `json:"title"`
			} `json:"columns"`
		} `json:"sections"`
	}](t, recorder)
	if len(detail.Sections) != 1 || detail.Sections[0].Section != "users" {
		t.Fatalf("sections = %+v, want the stored users table", detail.Sections)
	}
	if len(detail.Sections[0].Columns) != 2 || detail.Sections[0].Columns[0].Title != "Display name" {
		t.Fatalf("columns = %+v, want the stored header", detail.Sections[0].Columns)
	}

	recorder = call(t, server, http.MethodGet, reportPath(seeded.ID, "/sections/users?offset=1&limit=1"), nil)
	requireStatus(t, recorder, http.StatusOK)
	page := decodeJSON[struct {
		Rows   [][]string `json:"rows"`
		Total  int        `json:"total"`
		Offset int        `json:"offset"`
		Limit  int        `json:"limit"`
	}](t, recorder)
	if page.Total != 2 || page.Offset != 1 || page.Limit != 1 || len(page.Rows) != 1 {
		t.Fatalf("page = %+v, want the second row of two", page)
	}
	if len(page.Rows[0]) != 2 || page.Rows[0][0] != "Guest User" {
		t.Fatalf("row = %v, want the guest of the stored table", page.Rows[0])
	}

	recorder = call(t, server, http.MethodGet, reportPath(seeded.ID, "/sections/domains"), nil)
	requireStatus(t, recorder, http.StatusNotFound)
}

// TestExportReportStreamsAZipOfCSVs pins the export: one CSV per section, each
// with the stored header and rows.
func TestExportReportStreamsAZipOfCSVs(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	seeded := seedReport(t, store)

	recorder := call(t, server, http.MethodGet, reportPath(seeded.ID, "/export"), nil)
	requireStatus(t, recorder, http.StatusOK)
	if got := recorder.Header().Get("Content-Type"); got != "application/zip" {
		t.Fatalf("content type = %q, want application/zip", got)
	}
	archive, err := zip.NewReader(bytes.NewReader(recorder.Body.Bytes()), int64(recorder.Body.Len()))
	if err != nil {
		t.Fatalf("reading the archive: %v", err)
	}
	if len(archive.File) != 1 || archive.File[0].Name != "users.csv" {
		t.Fatalf("files = %d, want one CSV per section", len(archive.File))
	}
	file, err := archive.File[0].Open()
	if err != nil {
		t.Fatalf("opening the CSV: %v", err)
	}
	defer file.Close()
	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(file); err != nil {
		t.Fatalf("reading the CSV: %v", err)
	}
	content := body.String()
	for _, want := range []string{"Display name,User type", "Ada Lovelace,Member", "Guest User,Guest"} {
		if !bytes.Contains([]byte(content), []byte(want)) {
			t.Errorf("CSV does not contain %q:\n%s", want, content)
		}
	}
}

// scheduleAnswer is the JSON shape of one schedule.
type scheduleAnswer struct {
	Schedule struct {
		AccountID       uint     `json:"account_id"`
		Enabled         bool     `json:"enabled"`
		IntervalMinutes int      `json:"interval_minutes"`
		SectionList     []string `json:"section_list"`
		NextRunAt       string   `json:"next_run_at"`
	} `json:"schedule"`
}

// TestReportScheduleEndpoints pins the opt-in: saving is idempotent, an interval
// outside the bounds is refused, deleting is a no-op the second time and an
// unknown account is a 404.
func TestReportScheduleEndpoints(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	account := &models.ConnectedAccount{
		Provider:          string(models.ProviderMicrosoft),
		ProviderAccountID: "report-account",
		Email:             "admin@contoso.com",
		Status:            string(models.AccountConnected),
	}
	if err := store.SaveAccount(context.Background(), account); err != nil {
		t.Fatalf("seeding the account: %v", err)
	}
	path := "/api/reports/schedules/" + strconv.FormatUint(uint64(account.ID), 10)

	recorder := call(t, server, http.MethodPut, path, map[string]any{
		"enabled":          true,
		"interval_minutes": 360,
		"sections":         []string{"users", "domains"},
	})
	requireStatus(t, recorder, http.StatusOK)
	saved := decodeJSON[scheduleAnswer](t, recorder)
	if saved.Schedule.AccountID != account.ID || !saved.Schedule.Enabled || saved.Schedule.IntervalMinutes != 360 {
		t.Fatalf("schedule = %+v, want the saved opt-in", saved.Schedule)
	}
	if saved.Schedule.NextRunAt == "" {
		t.Error("next_run_at is empty, want the slot of an enabled schedule")
	}
	if len(saved.Schedule.SectionList) != 2 {
		t.Errorf("section_list = %v, want the two requested sections", saved.Schedule.SectionList)
	}

	recorder = call(t, server, http.MethodPut, path, map[string]any{
		"enabled":          true,
		"interval_minutes": 1,
	})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	recorder = call(t, server, http.MethodGet, "/api/reports/schedules", nil)
	requireStatus(t, recorder, http.StatusOK)
	list := decodeJSON[struct {
		Schedules []struct {
			AccountID   uint     `json:"account_id"`
			SectionList []string `json:"section_list"`
		} `json:"schedules"`
	}](t, recorder)
	if len(list.Schedules) != 1 || len(list.Schedules[0].SectionList) != 2 {
		t.Fatalf("schedules = %+v, want the one opt-in with its sections", list.Schedules)
	}

	recorder = call(t, server, http.MethodDelete, path, nil)
	requireStatus(t, recorder, http.StatusOK)
	recorder = call(t, server, http.MethodDelete, path, nil)
	requireStatus(t, recorder, http.StatusOK)

	recorder = call(t, server, http.MethodPut, "/api/reports/schedules/9999", map[string]any{"enabled": true})
	requireStatus(t, recorder, http.StatusNotFound)
}
