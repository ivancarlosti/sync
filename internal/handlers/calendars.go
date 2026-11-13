package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/services"
)

// Widths of the calendar display columns on models.CalendarJob, mirroring their
// gorm tags. The names come from the provider, so they are clamped here before
// they reach the database instead of turning a save into a 500.
const (
	calendarNameWidth = 191
	calendarIDWidth   = 512
	calendarURLWidth  = 1024
	calendarUserWidth = 191
	// calendarMarkerICS is the synthetic source calendar id of a subscription:
	// the model keeps one column for both kinds of source.
	calendarMarkerICS = "ics"
)

// Bounds of the listing window. Two years of history covers an audit of last
// year's meetings and five years ahead covers a long-lived subscription; anything
// outside is almost always a typo in a number field.
const (
	calendarPastDaysMin   = 0
	calendarPastDaysMax   = 730
	calendarFutureDaysMin = 1
	calendarFutureDaysMax = 1825
	calendarPastDefault   = 30
	calendarFutureDefault = 365
)

// calendarInput is the create/update payload of a calendar job. The .ics
// password is write-only: an update that leaves it empty keeps the stored secret,
// which is what lets the editor round-trip a job without decrypting anything.
type calendarInput struct {
	Name string `json:"name"`

	SourceKind         string `json:"source_kind"`
	SourceAccountID    uint   `json:"source_account_id"`
	SourceCalendarID   string `json:"source_calendar_id"`
	SourceCalendarName string `json:"source_calendar_name"`
	SourceURL          string `json:"source_url"`
	SourceUsername     string `json:"source_username"`
	SourcePassword     string `json:"source_password"`
	ClearSourceSecret  bool   `json:"clear_source_password"`

	DestinationAccountID    uint   `json:"destination_account_id"`
	DestinationCalendarID   string `json:"destination_calendar_id"`
	DestinationCalendarName string `json:"destination_calendar_name"`

	Direction      string `json:"direction"`
	ConflictPolicy string `json:"conflict_policy"`
	DeleteMissing  bool   `json:"delete_missing"`
	PastDays       int    `json:"past_days"`
	FutureDays     int    `json:"future_days"`

	IntervalMinutes int   `json:"interval_minutes"`
	Enabled         *bool `json:"enabled"`
}

// calendarView is a job plus the piece of state the UI needs but that does not
// live in the table: whether a run is in flight.
type calendarView struct {
	models.CalendarJob
	Running bool `json:"running"`
}

// newCalendarView decorates a job for the API.
func (s *Server) newCalendarView(job models.CalendarJob) calendarView {
	return calendarView{CalendarJob: job, Running: s.deps.Calendars.Running(job.ID)}
}

// calendarOption is one entry of a calendar picker.
type calendarOption struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Primary  bool   `json:"primary"`
	TimeZone string `json:"time_zone,omitempty"`
	ReadOnly bool   `json:"read_only"`
}

// applyCalendarInput validates a payload and writes it on job. It owns the whole
// contract of a calendar job: the required fields, the enum values, the accounts,
// the subscription rules and the defaults.
func (s *Server) applyCalendarInput(c *gin.Context, input calendarInput, job *models.CalendarJob) error {
	ctx := c.Request.Context()

	name := strings.TrimSpace(input.Name)
	switch {
	case name == "":
		return fmt.Errorf("%w: the calendar job name is required", services.ErrValidation)
	case len([]rune(name)) > calendarNameWidth:
		return fmt.Errorf("%w: the calendar job name must be at most %d characters", services.ErrValidation, calendarNameWidth)
	}

	kind := models.CalendarSourceKind(strings.ToLower(strings.TrimSpace(input.SourceKind)))
	if kind == "" {
		kind = models.CalendarSourceGoogle
	}
	if !kind.Valid() {
		return fmt.Errorf("%w: unknown calendar source %q", services.ErrValidation, input.SourceKind)
	}

	destination, err := s.deps.Store.GetAccount(ctx, input.DestinationAccountID)
	if err != nil {
		return err
	}
	if err := s.requireCalendarAccount(destination); err != nil {
		return err
	}

	// The direction of an .ics job is one way by nature: a feed has no writable
	// end. Forcing it instead of refusing the payload keeps the editor from
	// having to special-case the field.
	direction := models.CalendarDirection(strings.ToLower(strings.TrimSpace(input.Direction)))
	if direction == "" {
		direction = models.CalendarDirectionBidirectional
	}
	if kind == models.CalendarSourceICS {
		direction = models.CalendarDirectionOneWay
	}
	if !direction.Valid() {
		return fmt.Errorf("%w: unknown direction %q", services.ErrValidation, input.Direction)
	}

	policy := models.ConflictPolicy(strings.ToLower(strings.TrimSpace(input.ConflictPolicy)))
	if policy == "" {
		policy = models.ConflictNewestWins
	}
	if !policy.Valid() {
		return fmt.Errorf("%w: unknown conflict policy %q", services.ErrValidation, input.ConflictPolicy)
	}

	job.Name = name
	job.SourceKind = string(kind)
	job.Direction = string(direction)
	job.ConflictPolicy = string(policy)
	job.DeleteMissing = input.DeleteMissing
	job.PastDays = clampInt(input.PastDays, calendarPastDefault, calendarPastDaysMin, calendarPastDaysMax)
	job.FutureDays = clampInt(input.FutureDays, calendarFutureDefault, calendarFutureDaysMin, calendarFutureDaysMax)
	job.IntervalMinutes = clampInt(input.IntervalMinutes, 0, 0, maxIntervalMinutes)
	if input.Enabled != nil {
		job.Enabled = *input.Enabled
	}

	// The two ends are compared by account and calendar, not by account alone:
	// syncing a calendar with itself would rewrite every event on every run.
	destinationCalendar := strings.TrimSpace(input.DestinationCalendarID)
	job.DestinationAccountID = destination.ID
	job.DestinationCalendarID = calendarLabel(destinationCalendar, calendarIDWidth)
	job.DestinationCalendarName = calendarLabel(input.DestinationCalendarName, calendarNameWidth)

	if kind == models.CalendarSourceICS {
		return s.applyFeedInput(c, input, job)
	}
	return s.applyProviderSourceInput(c, input, job, destination, destinationCalendar)
}

// applyFeedInput writes the source half of a subscription job: the URL, the
// optional basic credentials and the validators.
func (s *Server) applyFeedInput(c *gin.Context, input calendarInput, job *models.CalendarJob) error {
	target := strings.TrimSpace(input.SourceURL)
	switch {
	case target == "":
		return fmt.Errorf("%w: the subscription URL is required", services.ErrValidation)
	case len(target) > calendarURLWidth:
		return fmt.Errorf("%w: the subscription URL is too long", services.ErrValidation)
	}
	if err := s.deps.Calendars.ValidateFeedURL(target); err != nil {
		return fmt.Errorf("%w: %v", services.ErrValidation, err)
	}

	// Changing the target invalidates the validators of the previous fetch:
	// keeping them would ask the new server for a 304 it never issued.
	if target != job.SourceURL {
		job.SourceETag = ""
		job.SourceLastModified = ""
		job.SourceFetchedAt = nil
	}

	job.SourceAccountID = 0
	job.SourceCalendarID = calendarMarkerICS
	job.SourceCalendarName = "iCalendar subscription"
	job.SourceURL = target
	job.SourceUsername = calendarLabel(input.SourceUsername, calendarUserWidth)

	switch {
	case input.ClearSourceSecret:
		job.SourceSecret = ""
	case strings.TrimSpace(input.SourcePassword) != "":
		sealed, err := s.deps.Calendars.SealFeedSecret(input.SourcePassword)
		if err != nil {
			return err
		}
		job.SourceSecret = sealed
	}
	return nil
}

// applyProviderSourceInput writes the source half of an account-to-account job.
func (s *Server) applyProviderSourceInput(c *gin.Context, input calendarInput, job *models.CalendarJob, destination *models.ConnectedAccount, destinationCalendar string) error {
	source, err := s.deps.Store.GetAccount(c.Request.Context(), input.SourceAccountID)
	if err != nil {
		return err
	}
	if err := s.requireCalendarAccount(source); err != nil {
		return err
	}
	sourceCalendar := strings.TrimSpace(input.SourceCalendarID)
	if source.ID == destination.ID && sourceCalendar != "" && sourceCalendar == destinationCalendar {
		return fmt.Errorf("%w: the source and the destination cannot be the same calendar", services.ErrValidation)
	}

	job.SourceAccountID = source.ID
	job.SourceCalendarID = calendarLabel(sourceCalendar, calendarIDWidth)
	job.SourceCalendarName = calendarLabel(input.SourceCalendarName, calendarNameWidth)
	// The subscription columns stay empty so a job that is switched back to a
	// provider source does not keep a stale credential around.
	job.SourceURL = ""
	job.SourceUsername = ""
	job.SourceSecret = ""
	job.SourceETag = ""
	job.SourceLastModified = ""
	job.SourceFetchedAt = nil
	return nil
}

// requireCalendarAccount refuses an account whose provider has no calendar half in
// this build, with the message the editor shows next to the picker.
func (s *Server) requireCalendarAccount(account *models.ConnectedAccount) error {
	provider, err := s.deps.Registry.Get(models.ProviderName(account.Provider))
	if err != nil {
		return fmt.Errorf("%w: %v", services.ErrValidation, err)
	}
	if !providers.CalendarsReadable(provider) {
		return fmt.Errorf("%w: %s does not support calendars in this build", services.ErrValidation, account.MaskEmail())
	}
	return nil
}

// calendarLabel clamps a display value to the width of its column, by rune so a
// truncated value stays valid UTF-8.
func calendarLabel(value string, max int) string {
	trimmed := strings.TrimSpace(value)
	runes := []rune(trimmed)
	if len(runes) <= max {
		return trimmed
	}
	return string(runes[:max])
}

// clampInt bounds a number that comes from a form. A zero means the field was
// left out and yields the default, except where zero is meaningful (interval).
func clampInt(value, fallback, min, max int) int {
	if value == 0 && min > 0 {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// lookupCalendar reads the `:id` parameter and loads the job, answering 404 when
// it does not exist.
func (s *Server) lookupCalendar(c *gin.Context) (*models.CalendarJob, bool) {
	id, ok := parseID(c, "id")
	if !ok {
		return nil, false
	}
	job, err := s.deps.Store.GetCalendarJob(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return nil, false
	}
	return job, true
}

// handleListCalendars answers GET /api/calendars.
func (s *Server) handleListCalendars(c *gin.Context) {
	jobs, err := s.deps.Store.ListCalendarJobs(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	views := make([]calendarView, 0, len(jobs))
	for _, job := range jobs {
		views = append(views, s.newCalendarView(job))
	}
	c.JSON(http.StatusOK, gin.H{"calendars": views})
}

// handleGetCalendar answers GET /api/calendars/:id.
func (s *Server) handleGetCalendar(c *gin.Context) {
	job, ok := s.lookupCalendar(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, s.newCalendarView(*job))
}

// handleCreateCalendar answers POST /api/calendars.
func (s *Server) handleCreateCalendar(c *gin.Context) {
	var input calendarInput
	if !decode(c, &input) {
		return
	}
	job := models.CalendarJob{Enabled: true}
	if err := s.applyCalendarInput(c, input, &job); err != nil {
		fail(c, err)
		return
	}
	if err := s.deps.Store.SaveCalendarJob(c.Request.Context(), &job); err != nil {
		fail(c, err)
		return
	}
	if err := s.rescheduleCalendar(c, &job); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, s.newCalendarView(job))
}

// handleUpdateCalendar answers PUT /api/calendars/:id. The payload replaces the
// job as a whole and the schedule is recomputed, so a changed interval or a job
// that was just disabled takes effect at once instead of on the next bootstrap.
func (s *Server) handleUpdateCalendar(c *gin.Context) {
	job, ok := s.lookupCalendar(c)
	if !ok {
		return
	}
	var input calendarInput
	if !decode(c, &input) {
		return
	}
	if err := s.applyCalendarInput(c, input, job); err != nil {
		fail(c, err)
		return
	}
	if err := s.deps.Store.SaveCalendarJob(c.Request.Context(), job); err != nil {
		fail(c, err)
		return
	}
	if err := s.rescheduleCalendar(c, job); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, s.newCalendarView(*job))
}

// handleDeleteCalendar answers DELETE /api/calendars/:id. A run in flight is
// cancelled first: it would otherwise keep writing to a calendar nobody is
// watching anymore.
func (s *Server) handleDeleteCalendar(c *gin.Context) {
	job, ok := s.lookupCalendar(c)
	if !ok {
		return
	}
	cancelled := s.deps.Calendars.Cancel(job.ID)
	if err := s.deps.Store.DeleteCalendarJob(c.Request.Context(), job.ID); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": job.ID, "cancelled": cancelled})
}

// handleRunCalendar answers POST /api/calendars/:id/run, the "Sync now" button.
// The run is started in the background and the answer carries its row, so the SPA
// can follow it.
func (s *Server) handleRunCalendar(c *gin.Context) {
	job, ok := s.lookupCalendar(c)
	if !ok {
		return
	}
	run, err := s.deps.Calendars.Start(c.Request.Context(), job.ID, models.TriggerManual)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job_id": job.ID, "run": run})
}

// handleCancelCalendar answers POST /api/calendars/:id/cancel. Cancelling a job
// that is not running is not an error, so the endpoint is idempotent.
func (s *Server) handleCancelCalendar(c *gin.Context) {
	job, ok := s.lookupCalendar(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"job_id": job.ID, "cancelled": s.deps.Calendars.Cancel(job.ID)})
}

// handleCalendarRuns answers GET /api/calendars/:id/runs: the history of one job.
func (s *Server) handleCalendarRuns(c *gin.Context) {
	job, ok := s.lookupCalendar(c)
	if !ok {
		return
	}
	runs, err := s.deps.Store.ListCalendarRuns(c.Request.Context(), job.ID, limit(c, 20, 200))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs})
}

// handleCalendarRun answers GET /api/calendars/:id/runs/:runId: one run with its
// per-event evidence, which is what the history drawer shows.
func (s *Server) handleCalendarRun(c *gin.Context) {
	job, ok := s.lookupCalendar(c)
	if !ok {
		return
	}
	runID, ok := parseID(c, "runId")
	if !ok {
		return
	}
	run, err := s.deps.Store.GetCalendarRun(c.Request.Context(), runID)
	if err != nil {
		fail(c, err)
		return
	}
	if run.JobID != job.ID {
		abort(c, http.StatusNotFound, "the run belongs to another calendar job")
		return
	}
	items, err := s.deps.Store.ListCalendarRunItems(c.Request.Context(), run.ID, limit(c, 200, 1000))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"run": run, "items": items})
}

// handleCalendarPreview answers GET /api/calendars/:id/preview: what a run would
// do right now, without writing anything on either side.
func (s *Server) handleCalendarPreview(c *gin.Context) {
	job, ok := s.lookupCalendar(c)
	if !ok {
		return
	}
	preview, err := s.deps.Calendars.Preview(c.Request.Context(), job)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, preview)
}

// feedPreviewInput is the body of the "check this subscription" call.
type feedPreviewInput struct {
	URL        string `json:"url"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	PastDays   int    `json:"past_days"`
	FutureDays int    `json:"future_days"`
}

// handleCalendarFeedPreview answers POST /api/calendars/feed-preview: it fetches an
// .ics feed before a job exists, so the editor can show the operator what the URL
// actually holds. The password travels in the body, never in the query string,
// because a URL ends up in a proxy log.
func (s *Server) handleCalendarFeedPreview(c *gin.Context) {
	var input feedPreviewInput
	if !decode(c, &input) {
		return
	}
	past := clampInt(input.PastDays, calendarPastDefault, calendarPastDaysMin, calendarPastDaysMax)
	future := clampInt(input.FutureDays, calendarFutureDefault, calendarFutureDaysMin, calendarFutureDaysMax)
	preview, err := s.deps.Calendars.PreviewFeed(c.Request.Context(), input.URL, input.Username, input.Password, past, future)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, preview)
}

// handleAccountCalendars answers GET /api/accounts/:id/calendars: the list the
// source and destination pickers show.
func (s *Server) handleAccountCalendars(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	calendars, err := s.deps.Calendars.ListCalendars(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	options := make([]calendarOption, 0, len(calendars))
	for _, calendar := range calendars {
		options = append(options, calendarOption{
			ID:       calendar.ID,
			Name:     calendar.Name,
			Primary:  calendar.Primary,
			TimeZone: calendar.TimeZone,
			ReadOnly: calendar.ReadOnly,
		})
	}
	c.JSON(http.StatusOK, gin.H{"calendars": options})
}

// rescheduleCalendar recomputes next_run_at from the current schedule and persists
// it, so a new interval or a disabled job takes effect immediately.
func (s *Server) rescheduleCalendar(c *gin.Context, job *models.CalendarJob) error {
	next := services.NextCalendarRunAt(s.now(), job)
	if err := s.deps.Store.ScheduleCalendarJob(c.Request.Context(), job.ID, next); err != nil {
		return err
	}
	job.NextRunAt = next
	return nil
}
