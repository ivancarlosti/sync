// This file exposes the Audit Logs feature over the API: the paginated list with
// its filters, the CSV export, the cursor status banner and the maintenance pass
// that polls the feeds on demand. The storage and the collector live in
// internal/services (auditlog.go); here it is only the HTTP shape.
package handlers

import (
	"encoding/csv"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/services"
)

// auditLogCSVHeader is the column order of the export: the same order the screen
// shows, so a spreadsheet opens as the operator sees it.
var auditLogCSVHeader = []string{
	"occurred_at", "provider", "org_id", "stream", "action", "target",
	"actor_kind", "actor_email", "actor_name", "actor_on_behalf_of", "actor_on_behalf_of_name",
	"target_name", "target_id", "site_id", "remote_id",
}

// auditLogView is one row of the Audit Logs screen. The stored row carries the
// vendor record in Raw, which is never sent: the screen renders the normalised
// columns, and the raw document stays available in the database.
type auditLogView struct {
	ID       uint   `json:"id"`
	Provider string `json:"provider"`
	Source   string `json:"source"`
	RemoteID string `json:"remote_id"`
	OrgID    string `json:"org_id"`
	Action   string `json:"action"`
	Target   string `json:"target"`
	// ActorKind is `user`, `application` or `system` (models.AuditLogActorKind):
	// the screen renders the actor cell from it, because an application name in
	// the address column reads as a person (see plan
	// 2026-11-07-audit-log-actor-attribution.md).
	ActorKind           string    `json:"actor_kind"`
	ActorEmail          string    `json:"actor_email"`
	ActorName           string    `json:"actor_name,omitempty"`
	ActorID             string    `json:"actor_id,omitempty"`
	ActorOnBehalfOf     string    `json:"actor_on_behalf_of,omitempty"`
	ActorOnBehalfOfName string    `json:"actor_on_behalf_of_name,omitempty"`
	TargetName          string    `json:"target_name,omitempty"`
	TargetID            string    `json:"target_id,omitempty"`
	DriveID             string    `json:"drive_id,omitempty"`
	SiteID              string    `json:"site_id,omitempty"`
	OccurredAt          time.Time `json:"occurred_at"`
}

// newAuditLogView decorates one stored event.
func newAuditLogView(event models.AuditLogEvent) auditLogView {
	return auditLogView{
		ID:                  event.ID,
		Provider:            event.Provider,
		Source:              event.Source,
		RemoteID:            event.RemoteID,
		OrgID:               event.OrgID,
		Action:              event.Action,
		Target:              event.Target,
		ActorKind:           event.ActorKind,
		ActorEmail:          event.ActorEmail,
		ActorName:           event.ActorName,
		ActorID:             event.ActorID,
		ActorOnBehalfOf:     event.ActorOnBehalfOf,
		ActorOnBehalfOfName: event.ActorOnBehalfOfName,
		TargetName:          event.TargetName,
		TargetID:            event.TargetID,
		DriveID:             event.DriveID,
		SiteID:              event.SiteID,
		OccurredAt:          event.OccurredAt,
	}
}

// auditLogFilter reads the filters shared by the list and the export. Every value
// is validated against the stored vocabulary: an unknown action or target could
// only answer an empty page, so it is refused with an explanation instead.
func auditLogFilter(c *gin.Context) (services.AuditLogFilter, bool) {
	filter := services.AuditLogFilter{
		Provider: strings.ToLower(strings.TrimSpace(c.Query("provider"))),
		OrgID:    strings.ToLower(strings.TrimSpace(c.Query("org_id"))),
		Action:   strings.ToLower(strings.TrimSpace(c.Query("action"))),
		Target:   strings.ToLower(strings.TrimSpace(c.Query("target"))),
		Actor:    strings.TrimSpace(c.Query("actor")),
		Search:   strings.TrimSpace(c.Query("search")),
	}
	if filter.Provider != "" &&
		models.ProviderName(filter.Provider) != models.ProviderGoogle &&
		models.ProviderName(filter.Provider) != models.ProviderMicrosoft {
		abort(c, http.StatusBadRequest, "provider must be google or microsoft")
		return filter, false
	}
	if filter.Action != "" && !models.AuditLogAction(filter.Action).Valid() {
		abort(c, http.StatusBadRequest, "action must be one of "+strings.Join(auditLogActions(), ", "))
		return filter, false
	}
	if filter.Target != "" && !models.AuditLogTarget(filter.Target).Valid() {
		abort(c, http.StatusBadRequest, "target must be one of "+strings.Join(auditLogTargets(), ", "))
		return filter, false
	}
	from, ok := auditLogQueryTime(c, "from", false)
	if !ok {
		return filter, false
	}
	to, ok := auditLogQueryTime(c, "to", true)
	if !ok {
		return filter, false
	}
	if from != nil && to != nil && to.Before(*from) {
		abort(c, http.StatusBadRequest, "to must not be before from")
		return filter, false
	}
	filter.From = from
	filter.To = to
	return filter, true
}

// auditLogQueryTime reads one optional RFC3339 or date-only bound. A date-only
// value means the start of that day in UTC for `from` and the end of it for `to`,
// which is what a date picker means by "until the 5th".
func auditLogQueryTime(c *gin.Context, name string, endOfDay bool) (*time.Time, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		parsed, err := time.Parse(layout, raw)
		if err != nil {
			continue
		}
		utc := parsed.UTC()
		if endOfDay && layout == "2006-01-02" {
			utc = utc.Add(24 * time.Hour).Add(-time.Second)
		}
		return &utc, true
	}
	abort(c, http.StatusBadRequest, name+" must be an RFC3339 timestamp or a YYYY-MM-DD date")
	return nil, false
}

// auditLogActions lists the normalised verbs, in the order the filter offers them.
func auditLogActions() []string {
	return []string{
		string(models.AuditLogCreate), string(models.AuditLogDelete), string(models.AuditLogUpdate),
		string(models.AuditLogMove), string(models.AuditLogShare), string(models.AuditLogLogin),
		string(models.AuditLogOther),
	}
}

// auditLogTargets lists the normalised objects, in the order the filter offers
// them.
func auditLogTargets() []string {
	return []string{
		string(models.AuditLogTargetFile), string(models.AuditLogTargetFolder),
		string(models.AuditLogTargetUser), string(models.AuditLogTargetGroup),
		string(models.AuditLogTargetDevice), string(models.AuditLogTargetApplication),
		string(models.AuditLogTargetSite), string(models.AuditLogTargetDrive),
		string(models.AuditLogTargetOther),
	}
}

// auditLogPage parses the pagination of the list.
func auditLogPage(c *gin.Context, filter *services.AuditLogFilter) bool {
	filter.Limit = limit(c, 50, services.AuditLogMaxPage)
	raw := strings.TrimSpace(c.Query("offset"))
	if raw == "" {
		return true
	}
	offset, err := strconv.Atoi(raw)
	if err != nil || offset < 0 {
		abort(c, http.StatusBadRequest, "offset must be a non-negative integer")
		return false
	}
	filter.Offset = offset
	return true
}

// handleListAuditLogs answers GET /api/audit-logs: one page of events, newest
// first, with the total of the filtered set and the cursor of every feed, so the
// screen can render the history and its health from one request.
func (s *Server) handleListAuditLogs(c *gin.Context) {
	filter, ok := auditLogFilter(c)
	if !ok {
		return
	}
	if !auditLogPage(c, &filter) {
		return
	}
	events, total, err := s.deps.Store.ListAuditLogEvents(c.Request.Context(), filter)
	if err != nil {
		fail(c, err)
		return
	}
	cursors, err := s.deps.Store.ListAuditLogCursors(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	views := make([]auditLogView, 0, len(events))
	for _, event := range events {
		views = append(views, newAuditLogView(event))
	}
	c.JSON(http.StatusOK, gin.H{
		"events":  views,
		"total":   total,
		"limit":   filter.Limit,
		"offset":  filter.Offset,
		"cursors": services.NewAuditLogCursorViews(cursors),
	})
}

// handleAuditLogCursors answers GET /api/audit-logs/cursors: the position and
// health of every feed, which is what the screen shows when a list looks stale
// (a tenant waiting for a consent, a provider refusing the call).
func (s *Server) handleAuditLogCursors(c *gin.Context) {
	cursors, err := s.deps.Store.ListAuditLogCursors(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"cursors": services.NewAuditLogCursorViews(cursors)})
}

// handleExportAuditLogs answers GET /api/audit-logs/export: it streams the
// filtered events as a CSV download, capped at the export limit so one request
// cannot pull an unbounded history into memory.
func (s *Server) handleExportAuditLogs(c *gin.Context) {
	filter, ok := auditLogFilter(c)
	if !ok {
		return
	}
	filter.Limit = services.AuditLogExportLimit
	filter.Offset = 0
	events, total, err := s.deps.Store.ListAuditLogEvents(c.Request.Context(), filter)
	if err != nil {
		fail(c, err)
		return
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "audit-logs.csv"))
	c.Header("Cache-Control", "no-store")
	// The number of rows an operator is about to miss, so a truncated export is
	// never silent.
	c.Header("X-Total-Count", strconv.FormatInt(total, 10))
	c.Status(http.StatusOK)

	// A UTF-8 BOM is what makes a spreadsheet open an accented name correctly.
	if _, err := c.Writer.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		slog.Warn("writing the activity log CSV BOM failed", "error", err)
		return
	}
	out := csv.NewWriter(c.Writer)
	if err := out.Write(auditLogCSVHeader); err != nil {
		slog.Warn("writing the activity log CSV header failed", "error", err)
		return
	}
	for _, event := range events {
		record := []string{
			event.OccurredAt.UTC().Format(time.RFC3339),
			event.Provider,
			event.OrgID,
			event.Source,
			event.Action,
			event.Target,
			event.ActorKind,
			event.ActorEmail,
			event.ActorName,
			event.ActorOnBehalfOf,
			event.ActorOnBehalfOfName,
			event.TargetName,
			event.TargetID,
			event.SiteID,
			event.RemoteID,
		}
		if err := out.Write(record); err != nil {
			slog.Warn("writing an activity log CSV row failed", "error", err)
			return
		}
	}
	out.Flush()
	if err := out.Error(); err != nil {
		slog.Warn("flushing the activity log CSV failed", "error", err)
	}
}

// handleMaintenanceAuditLogs answers POST /api/maintenance/auditlogs/collect: it
// polls every activity feed right away instead of waiting for the ten minute
// loop, and answers the counters of the pass (including the feeds that failed,
// which is how the screen explains a missing consent).
func (s *Server) handleMaintenanceAuditLogs(c *gin.Context) {
	outcome := s.deps.Scheduler.RunAuditLogs(c.Request.Context())
	c.JSON(http.StatusOK, outcome)
}

// handleMaintenanceAuditLogRepair answers POST /api/maintenance/auditlogs/renormalise:
// it re-reads the stored vendor document of the events a release before the actor
// kind attributed with a single slot, and gives them the actor the current
// normaliser produces (a person, a workload, or the person behind a workload). The
// same pass runs at the end of every collect, so this endpoint is how an operator
// repairs the history now instead of waiting for the next tick.
func (s *Server) handleMaintenanceAuditLogRepair(c *gin.Context) {
	outcome := s.deps.Scheduler.RunAuditLogRepair(c.Request.Context())
	c.JSON(http.StatusOK, outcome)
}
