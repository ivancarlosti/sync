package handlers

import (
	"archive/zip"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/services"
)

// reportInput is the payload of POST /api/reports, the start request.
type reportInput struct {
	AccountID uint `json:"account_id"`
	// Sections is the list of sections to collect; empty means every section
	// the provider supports.
	Sections []string `json:"sections"`
}

// reportView is a report plus the two things the history screen needs without a
// second request: whether it is still running (to offer Cancel) and the section
// list as an array instead of the stored comma separated column.
type reportView struct {
	models.DirectoryReport
	Running     bool     `json:"running"`
	SectionList []string `json:"section_list"`
}

// newReportView decorates a report.
func newReportView(run models.DirectoryReport) reportView {
	return reportView{
		DirectoryReport: run,
		Running:         run.Status == string(models.RunRunning),
		SectionList:     run.SectionList(),
	}
}

// reportSectionView is the table of contents of one section: its columns, its
// size and its health, without the rows themselves (which may be tens of
// thousands and are read one page at a time).
type reportSectionView struct {
	Section   string                   `json:"section"`
	Columns   []providers.ReportColumn `json:"columns"`
	RowCount  int                      `json:"row_count"`
	Truncated bool                     `json:"truncated"`
	Error     string                   `json:"error,omitempty"`
}

// reportRowsView is one page of a section.
type reportRowsView struct {
	Section   string                   `json:"section"`
	Columns   []providers.ReportColumn `json:"columns"`
	Rows      []providers.ReportRow    `json:"rows"`
	Total     int                      `json:"total"`
	Offset    int                      `json:"offset"`
	Limit     int                      `json:"limit"`
	Truncated bool                     `json:"truncated"`
	Error     string                   `json:"error,omitempty"`
}

// reportScheduleView is the opt-in of one account with its sections as an array.
type reportScheduleView struct {
	models.ReportSchedule
	SectionList []string `json:"section_list"`
}

// newReportScheduleView decorates a schedule.
func newReportScheduleView(schedule models.ReportSchedule) reportScheduleView {
	return reportScheduleView{ReportSchedule: schedule, SectionList: schedule.SectionList()}
}

// reportScheduleInput is the payload of PUT /api/reports/schedules/:account_id.
type reportScheduleInput struct {
	Enabled         bool     `json:"enabled"`
	IntervalMinutes int      `json:"interval_minutes"`
	Sections        []string `json:"sections"`
}

// decodeReportTable reads the two JSON documents of a stored section. An empty
// document is an empty table rather than an error: a section that failed was
// stored with its columns and no rows.
func decodeReportTable(section models.ReportSectionData) ([]providers.ReportColumn, []providers.ReportRow, error) {
	columns := []providers.ReportColumn{}
	if raw := section.Columns; raw != "" {
		if err := json.Unmarshal([]byte(raw), &columns); err != nil {
			return nil, nil, fmt.Errorf("handlers: reading the columns of %s: %w", section.Section, err)
		}
	}
	rows := []providers.ReportRow{}
	if raw := section.Rows; raw != "" {
		if err := json.Unmarshal([]byte(raw), &rows); err != nil {
			return nil, nil, fmt.Errorf("handlers: reading the rows of %s: %w", section.Section, err)
		}
	}
	return columns, rows, nil
}

// handleReportSections answers GET /api/reports/sections?account_id=: the sections
// the provider of that account can collect, which is what the start form offers.
// Without an account it answers the whole vocabulary.
func (s *Server) handleReportSections(c *gin.Context) {
	accountID, ok := queryID(c, "account_id")
	if !ok {
		return
	}
	sections := providers.ReportSections
	supported := true
	if accountID > 0 {
		account, err := s.deps.Store.GetAccount(c.Request.Context(), accountID)
		if err != nil {
			fail(c, err)
			return
		}
		source, available := s.deps.Registry.ReportSourceFor(models.ProviderName(account.Provider))
		if !available {
			c.JSON(http.StatusOK, gin.H{"sections": []string{}, "supported": false})
			return
		}
		sections = source.ReportSections()
		// The shared mailboxes section needs MailboxSettings.Read.All. A grant
		// made before that scope existed cannot fill it, so the start form
		// leaves it out until the account is reconnected (Accounts shows the
		// missing capability) instead of offering a section every report would
		// mark unusable.
		if catalog, ok := source.(providers.ScopeCatalog); ok {
			permissions := catalog.Permissions()
			granted := providers.SplitScopes(account.Scopes)
			if !providers.HasCapability(permissions, granted, providers.CapabilityMailboxSettings) {
				filtered := sections[:0]
				for _, section := range sections {
					if section != providers.ReportSectionSharedMailboxes {
						filtered = append(filtered, section)
					}
				}
				sections = filtered
			}
		}
	}
	names := make([]string, 0, len(sections))
	for _, section := range sections {
		names = append(names, string(section))
	}
	c.JSON(http.StatusOK, gin.H{"sections": names, "supported": supported})
}

// handleStartReport answers POST /api/reports. The report is collected in the
// background; the answer carries the created row so the SPA can follow it (409
// while another report of the same account is in flight).
func (s *Server) handleStartReport(c *gin.Context) {
	var input reportInput
	if !decode(c, &input) {
		return
	}
	run, err := s.deps.Reports.Start(c.Request.Context(), services.ReportInput{
		AccountID: input.AccountID,
		Sections:  input.Sections,
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"report": newReportView(*run)})
}

// handleListReports answers GET /api/reports?limit=, the history screen's log.
func (s *Server) handleListReports(c *gin.Context) {
	reports, err := s.deps.Store.ListReports(c.Request.Context(), limit(c, 50, 200))
	if err != nil {
		fail(c, err)
		return
	}
	views := make([]reportView, 0, len(reports))
	for _, run := range reports {
		views = append(views, newReportView(run))
	}
	c.JSON(http.StatusOK, gin.H{"reports": views})
}

// handleGetReport answers GET /api/reports/:id with the report and the table of
// contents of its sections.
func (s *Server) handleGetReport(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	run, err := s.deps.Store.GetReport(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}
	sections, err := s.deps.Store.ListReportSections(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}
	views := make([]reportSectionView, 0, len(sections))
	for _, section := range sections {
		columns, rows, decodeErr := decodeReportTable(section)
		if decodeErr != nil {
			slog.Warn("reading a stored report section failed", "report", id, "section", section.Section, "error", decodeErr)
		}
		view := reportSectionView{
			Section:   section.Section,
			Columns:   columns,
			RowCount:  section.RowCount,
			Truncated: section.Truncated,
			Error:     section.Error,
		}
		if decodeErr != nil && view.Error == "" {
			// A corrupt document is reported like a failed section instead of
			// failing the whole report view.
			view.Error = decodeErr.Error()
			view.RowCount = len(rows)
		}
		views = append(views, view)
	}
	c.JSON(http.StatusOK, gin.H{"report": newReportView(*run), "sections": views})
}

// handleGetReportSection answers GET /api/reports/:id/sections/:section with one
// page of a stored table.
func (s *Server) handleGetReportSection(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	name := c.Param("section")
	if !providers.ReportSection(name).Valid() {
		abort(c, http.StatusBadRequest, "unknown report section: "+name)
		return
	}
	section, err := s.deps.Store.GetReportSection(c.Request.Context(), id, name)
	if err != nil {
		fail(c, err)
		return
	}
	columns, rows, err := decodeReportTable(*section)
	if err != nil {
		fail(c, err)
		return
	}
	offset := queryInt(c, "offset", 0, 0, len(rows))
	size := queryInt(c, "limit", 200, 1, 1000)
	end := offset + size
	if end > len(rows) {
		end = len(rows)
	}
	page := []providers.ReportRow{}
	if offset < len(rows) {
		page = rows[offset:end]
	}
	c.JSON(http.StatusOK, reportRowsView{
		Section:   section.Section,
		Columns:   columns,
		Rows:      page,
		Total:     len(rows),
		Offset:    offset,
		Limit:     size,
		Truncated: section.Truncated,
		Error:     section.Error,
	})
}

// handleCancelReport answers POST /api/reports/:id/cancel. Cancelling a report
// that is not running is not an error, so the endpoint is idempotent.
func (s *Server) handleCancelReport(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	run, err := s.deps.Store.GetReport(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "cancelled": s.deps.Reports.Cancel(run.AccountID)})
}

// handleExportReport answers GET /api/reports/:id/export: it streams the stored
// report as a ZIP holding one CSV per section.
//
// A ZIP of CSVs (rather than a single workbook) keeps the export dependency free:
// every spreadsheet opens a CSV, and one file per section is what makes the
// download readable without a column-splicing tool. The server never re-reads the
// provider — the tables were written when the report ran.
func (s *Server) handleExportReport(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	run, err := s.deps.Store.GetReport(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}
	sections, err := s.deps.Store.ListReportSections(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}

	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("report-%d.zip", run.ID)))
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)

	archive := zip.NewWriter(c.Writer)
	for _, section := range sections {
		if err := writeReportCSV(archive, section); err != nil {
			// The body is already streaming, so the header cannot become an
			// error answer any more: log it and stop with what was written.
			slog.Warn("writing a report CSV failed", "report", run.ID, "section", section.Section, "error", err)
			break
		}
	}
	if err := archive.Close(); err != nil {
		slog.Warn("closing the report archive failed", "report", run.ID, "error", err)
	}
}

// writeReportCSV adds one section to the archive. A UTF-8 BOM is what makes a
// spreadsheet open an accented name correctly, exactly like the audit export.
func writeReportCSV(archive *zip.Writer, section models.ReportSectionData) error {
	columns, rows, err := decodeReportTable(section)
	if err != nil {
		return err
	}
	file, err := archive.Create(section.Section + ".csv")
	if err != nil {
		return fmt.Errorf("handlers: creating the CSV of %s: %w", section.Section, err)
	}
	if _, err := file.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return fmt.Errorf("handlers: writing the CSV BOM of %s: %w", section.Section, err)
	}
	out := csv.NewWriter(file)
	header := make([]string, 0, len(columns))
	for _, column := range columns {
		header = append(header, column.Title)
	}
	if err := out.Write(header); err != nil {
		return fmt.Errorf("handlers: writing the CSV header of %s: %w", section.Section, err)
	}
	for _, row := range rows {
		if err := out.Write(row); err != nil {
			return fmt.Errorf("handlers: writing a CSV row of %s: %w", section.Section, err)
		}
	}
	out.Flush()
	if err := out.Error(); err != nil {
		return fmt.Errorf("handlers: flushing the CSV of %s: %w", section.Section, err)
	}
	return nil
}

// handleListReportSchedules answers GET /api/reports/schedules: the opt-ins of
// every account.
func (s *Server) handleListReportSchedules(c *gin.Context) {
	schedules, err := s.deps.Reports.ListSchedules(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	views := make([]reportScheduleView, 0, len(schedules))
	for _, schedule := range schedules {
		views = append(views, newReportScheduleView(schedule))
	}
	c.JSON(http.StatusOK, gin.H{"schedules": views})
}

// handleSaveReportSchedule answers PUT /api/reports/schedules/:account_id: it
// enables, edits or disables the automatic report of one account.
func (s *Server) handleSaveReportSchedule(c *gin.Context) {
	accountID, ok := parseID(c, "account_id")
	if !ok {
		return
	}
	var input reportScheduleInput
	if !decode(c, &input) {
		return
	}
	if _, err := s.deps.Store.GetAccount(c.Request.Context(), accountID); err != nil {
		fail(c, err)
		return
	}
	schedule, err := s.deps.Reports.SaveSchedule(c.Request.Context(), services.ReportScheduleInput{
		AccountID:       accountID,
		Enabled:         input.Enabled,
		IntervalMinutes: input.IntervalMinutes,
		Sections:        input.Sections,
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"schedule": newReportScheduleView(*schedule)})
}

// handleDeleteReportSchedule answers DELETE /api/reports/schedules/:account_id.
// It is idempotent: deleting a schedule that does not exist is not an error.
func (s *Server) handleDeleteReportSchedule(c *gin.Context) {
	accountID, ok := parseID(c, "account_id")
	if !ok {
		return
	}
	if err := s.deps.Reports.DeleteSchedule(c.Request.Context(), accountID); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"account_id": accountID, "deleted": true})
}
