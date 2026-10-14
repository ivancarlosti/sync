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

// Bounds of the billing endpoints.
const (
	// billingDefaultDays is the window of an unfiltered report: the last 90 days,
	// which is what an operator comparing months looks at.
	billingDefaultDays = 90
	// billingRowLimit caps a report so one request can never load an unbounded
	// history into the process.
	billingRowLimit = 1000
	// billingGroupDefault is how many buckets a report returns when the caller
	// does not say (90 days of daily rows).
	billingGroupDefault = 90
	// billingRunLimit and billingRunDefault bound the per-run breakdown, which is
	// a much longer list (two rows per run).
	billingRunLimit   = 2000
	billingRunDefault = 500
)

// handleBilling answers GET /api/billing: the traffic of a window grouped by day
// (the default), month, account or run, labelled with the configured billing
// schema.
func (s *Server) handleBilling(c *gin.Context) {
	filter, ok := s.billingFilter(c, billingDefaultDays, billingGroupDefault, billingRowLimit)
	if !ok {
		return
	}
	settings, err := s.deps.Settings.Load(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	rows, err := s.deps.Store.BillingSummary(c.Request.Context(), filter)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"group_by": filter.GroupBy,
		"from":     billingDayString(filter.From),
		"to":       billingDayString(filter.To),
		"schema":   services.ResolveBillingSchema(settings),
		"rows":     rows,
		"total":    sumBillingRows(rows),
	})
}

// handleBillingRuns answers GET /api/billing/runs: the individual traffic rows of
// a window, newest run first (both directions of a run come back as two rows).
func (s *Server) handleBillingRuns(c *gin.Context) {
	filter, ok := s.billingFilter(c, billingDefaultDays, billingRunDefault, billingRunLimit)
	if !ok {
		return
	}
	rows, err := s.deps.Store.ListTrafficRuns(c.Request.Context(), filter)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"from":    billingDayString(filter.From),
		"to":      billingDayString(filter.To),
		"traffic": rows,
	})
}

// billingCSVHeader is the column order of the export. It is fixed and English (a
// report is a data export, not a UI string): the billing schema decides the unit
// of the two quantity columns and whether a cost column carries a value, never
// the shape of the file.
var billingCSVHeader = []string{
	"bucket", "day", "month", "account_id", "account_email", "run_id", "job_id",
	"inbound", "outbound", "operations", "total", "cost", "unit", "currency",
}

// handleExportBilling answers GET /api/billing/export: it streams the same rows
// the JSON report returns as a CSV download, in the unit of the active schema.
func (s *Server) handleExportBilling(c *gin.Context) {
	filter, ok := s.billingFilter(c, billingDefaultDays, billingRowLimit, billingRowLimit)
	if !ok {
		return
	}
	settings, err := s.deps.Settings.Load(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	schema := services.ResolveBillingSchema(settings)
	rows, err := s.deps.Store.BillingSummary(c.Request.Context(), filter)
	if err != nil {
		fail(c, err)
		return
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", billingFileName(filter)))
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)

	// A UTF-8 BOM is what makes a spreadsheet open the file correctly.
	if _, err := c.Writer.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		slog.Warn("writing the billing CSV BOM failed", "error", err)
		return
	}
	out := csv.NewWriter(c.Writer)
	if err := out.Write(billingCSVHeader); err != nil {
		slog.Warn("writing the billing CSV header failed", "error", err)
		return
	}
	for _, row := range rows {
		cost := ""
		if value, ok := schema.Cost(row.Total.Bytes); ok {
			cost = strconv.FormatFloat(value, 'f', 6, 64)
		}
		record := []string{
			billingBucket(row),
			row.Day,
			row.Month,
			formatUint(row.AccountID),
			row.AccountEmail,
			formatUint(row.RunID),
			formatUint(row.JobID),
			schema.Quantity(row.Inbound.Bytes),
			schema.Quantity(row.Outbound.Bytes),
			strconv.FormatInt(row.Total.Operations, 10),
			schema.Quantity(row.Total.Bytes),
			cost,
			schema.Unit,
			schema.Currency,
		}
		if err := out.Write(record); err != nil {
			slog.Warn("writing a billing CSV row failed", "error", err)
			return
		}
	}
	out.Flush()
	if err := out.Error(); err != nil {
		slog.Warn("flushing the billing CSV failed", "error", err)
	}
}

// billingFilter reads the shared query parameters of the billing endpoints. It
// answers 400 itself and returns false when the caller must stop.
//
// The window defaults to the last `defaultDays` days ending today: an unfiltered
// report over the whole history is a year long after a while, which no screen
// wants to load by accident.
func (s *Server) billingFilter(c *gin.Context, defaultDays, defaultLimit, maxLimit int) (services.BillingFilter, bool) {
	filter := services.BillingFilter{
		GroupBy: strings.ToLower(strings.TrimSpace(c.DefaultQuery("group_by", services.BillingByDay))),
		Limit:   queryInt(c, "limit", defaultLimit, 1, maxLimit),
	}
	from, to := strings.TrimSpace(c.Query("from")), strings.TrimSpace(c.Query("to"))
	if from != "" {
		day, err := parseDay(from)
		if err != nil {
			abort(c, http.StatusBadRequest, err.Error())
			return filter, false
		}
		filter.From = day
	}
	if to != "" {
		day, err := parseDay(to)
		if err != nil {
			abort(c, http.StatusBadRequest, err.Error())
			return filter, false
		}
		filter.To = day
	}
	today := endOfToday(s.now())
	if filter.To.IsZero() {
		filter.To = today
	}
	if filter.From.IsZero() {
		filter.From = filter.To.AddDate(0, 0, -defaultDays)
	}
	accountID, ok := queryID(c, "account_id")
	if !ok {
		return filter, false
	}
	filter.AccountID = accountID
	jobID, ok := queryID(c, "job_id")
	if !ok {
		return filter, false
	}
	filter.JobID = jobID
	return filter, true
}

// parseDay reads a `YYYY-MM-DD` query parameter (the UTC day bucket a traffic row
// stores).
func parseDay(raw string) (time.Time, error) {
	day, err := time.Parse(models.TrafficDayFormat, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: the dates must be written as YYYY-MM-DD", services.ErrValidation)
	}
	return day.UTC(), nil
}

// endOfToday is the inclusive upper bound of an unbounded window: the last second
// of the day the clock is in, so a report always covers a partial day.
func endOfToday(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 23, 59, 59, 0, time.UTC)
}

// billingDayString renders an optional bound for the JSON answer.
func billingDayString(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(models.TrafficDayFormat)
}

// billingBucket labels a report line: the value of the grouping the request asked
// for, which is what a spreadsheet wants in its first column.
func billingBucket(row services.BillingRow) string {
	switch {
	case row.Day != "":
		return row.Day
	case row.Month != "":
		return row.Month
	case row.RunID > 0:
		return "run " + formatUint(row.RunID)
	case row.AccountID > 0:
		if row.AccountEmail != "" {
			return row.AccountEmail
		}
		return "account " + formatUint(row.AccountID)
	default:
		return "all"
	}
}

// billingFileName is the download name of an export. Every character of it comes
// from the validated filter, so a header injection is impossible.
func billingFileName(filter services.BillingFilter) string {
	from, to := "all", "all"
	if !filter.From.IsZero() {
		from = filter.From.UTC().Format(models.TrafficDayFormat)
	}
	if !filter.To.IsZero() {
		to = filter.To.UTC().Format(models.TrafficDayFormat)
	}
	return fmt.Sprintf("billing-%s-%s-to-%s.csv", filter.GroupBy, from, to)
}

// sumBillingRows adds the lines of a report into the window total.
func sumBillingRows(rows []services.BillingRow) services.TrafficTotals {
	var total services.TrafficTotals
	for _, row := range rows {
		total.Bytes += row.Total.Bytes
		total.Files += row.Total.Files
		total.Operations += row.Total.Operations
		total.Runs += row.Total.Runs
	}
	return total
}

// formatUint renders an optional identifier, leaving 0 empty.
func formatUint(value uint) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatUint(uint64(value), 10)
}
