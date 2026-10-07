package handlers

import (
	"encoding/csv"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/services"
)

// auditInput is the payload of POST /api/audits, the start request.
type auditInput struct {
	AccountID  uint   `json:"account_id"`
	DriveID    string `json:"drive_id"`
	FolderID   string `json:"folder_id"`
	FolderPath string `json:"folder_path"`
	// Depth is how many folder levels below the selected folder are analysed; 0
	// is unlimited.
	Depth int `json:"depth"`
}

// auditView is an audit plus the flag the history screen uses to offer the
// cancel button without a second request.
type auditView struct {
	models.AuditRun
	Running bool `json:"running"`
}

// auditEntryView is one node of a report.
type auditEntryView struct {
	ID         uint       `json:"id"`
	Kind       string     `json:"kind"`
	Path       string     `json:"path"`
	Name       string     `json:"name"`
	Depth      int        `json:"depth"`
	Size       int64      `json:"size"`
	TotalSize  int64      `json:"total_size"`
	Files      int        `json:"files"`
	Folders    int        `json:"folders"`
	Expanded   bool       `json:"expanded"`
	ModifiedAt *time.Time `json:"modified_at,omitempty"`
	MimeType   string     `json:"mime_type,omitempty"`
}

// newAuditView decorates an audit.
func newAuditView(run models.AuditRun) auditView {
	return auditView{AuditRun: run, Running: run.Status == string(models.RunRunning)}
}

// newAuditEntryViews converts the report rows.
func newAuditEntryViews(entries []models.AuditEntry) []auditEntryView {
	views := make([]auditEntryView, 0, len(entries))
	for _, entry := range entries {
		views = append(views, auditEntryView{
			ID:         entry.ID,
			Kind:       entry.Kind,
			Path:       entry.Path,
			Name:       entry.Name,
			Depth:      entry.Depth,
			Size:       entry.Size,
			TotalSize:  entry.TotalSize,
			Files:      entry.Files,
			Folders:    entry.Folders,
			Expanded:   entry.Expanded,
			ModifiedAt: entry.ModifiedAt,
			MimeType:   entry.MimeType,
		})
	}
	return views
}

// handleStartAudit answers POST /api/audits. The audit is started in the
// background; the answer carries the created row so the SPA can follow it (409
// while another audit of the same account is in flight).
func (s *Server) handleStartAudit(c *gin.Context) {
	var input auditInput
	if !decode(c, &input) {
		return
	}
	run, err := s.deps.Audit.Start(c.Request.Context(), services.AuditInput{
		AccountID:  input.AccountID,
		DriveID:    input.DriveID,
		FolderID:   input.FolderID,
		FolderPath: input.FolderPath,
		Depth:      input.Depth,
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"audit": newAuditView(*run)})
}

// handleListAudits answers GET /api/audits?limit=, the history screen's log.
func (s *Server) handleListAudits(c *gin.Context) {
	audits, err := s.deps.Store.ListAudits(c.Request.Context(), limit(c, 50, 200))
	if err != nil {
		fail(c, err)
		return
	}
	views := make([]auditView, 0, len(audits))
	for _, run := range audits {
		views = append(views, newAuditView(run))
	}
	c.JSON(http.StatusOK, gin.H{"audits": views})
}

// handleGetAudit answers GET /api/audits/:id with the audit and its report.
func (s *Server) handleGetAudit(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	run, err := s.deps.Store.GetAudit(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	entries, err := s.deps.Store.ListAuditEntries(c.Request.Context(), id, services.AuditNodeLimit)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"audit": newAuditView(*run), "entries": newAuditEntryViews(entries)})
}

// handleCancelAudit answers POST /api/audits/:id/cancel. Cancelling an audit
// that is not running is not an error, so the endpoint is idempotent.
func (s *Server) handleCancelAudit(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	run, err := s.deps.Store.GetAudit(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "cancelled": s.deps.Audit.Cancel(run.AccountID)})
}

// auditCSVHeader is the CSV column order. It is fixed and English (a report is a
// data export, not a UI string), matching the documentation in docs/api.md.
var auditCSVHeader = []string{
	"kind", "path", "name", "depth", "size", "total_size",
	"files", "folders", "expanded", "modified_at", "mime_type",
}

// handleExportAudit answers GET /api/audits/:id/export: it streams the persisted
// report of an audit as a CSV download. The server never re-scans the provider -
// the entries were written when the audit ran.
func (s *Server) handleExportAudit(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	run, err := s.deps.Store.GetAudit(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	entries, err := s.deps.Store.ListAuditEntries(c.Request.Context(), id, services.AuditNodeLimit)
	if err != nil {
		fail(c, err)
		return
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("audit-%d.csv", run.ID)))
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)

	// A UTF-8 BOM is what makes a spreadsheet open an accented path correctly.
	if _, err := c.Writer.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		slog.Warn("writing the audit CSV BOM failed", "audit", run.ID, "error", err)
		return
	}
	out := csv.NewWriter(c.Writer)
	if err := out.Write(auditCSVHeader); err != nil {
		slog.Warn("writing the audit CSV header failed", "audit", run.ID, "error", err)
		return
	}
	for _, entry := range entries {
		modified := ""
		if entry.ModifiedAt != nil {
			modified = entry.ModifiedAt.UTC().Format(time.RFC3339)
		}
		record := []string{
			entry.Kind,
			entry.Path,
			entry.Name,
			strconv.Itoa(entry.Depth),
			strconv.FormatInt(entry.Size, 10),
			strconv.FormatInt(entry.TotalSize, 10),
			strconv.Itoa(entry.Files),
			strconv.Itoa(entry.Folders),
			strconv.FormatBool(entry.Expanded),
			modified,
			entry.MimeType,
		}
		if err := out.Write(record); err != nil {
			slog.Warn("writing an audit CSV row failed", "audit", run.ID, "error", err)
			return
		}
	}
	out.Flush()
	if err := out.Error(); err != nil {
		slog.Warn("flushing the audit CSV failed", "audit", run.ID, "error", err)
	}
}
