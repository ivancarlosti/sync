package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/services"
)

// stubCalendarProvider is the file stub of handlers_test.go plus an in-memory
// calendar store, so the calendar endpoints and the engine behind them run
// without reaching Google or Graph. The embedded pointer supplies the Provider
// half (identity, tokens, drives); this type adds providers.CalendarProvider.
type stubCalendarProvider struct {
	*stubProvider

	mu        sync.Mutex
	calendars []providers.Calendar
	events    map[string][]providers.CalendarEvent
	nextID    int
}

// newStubCalendarProvider builds a stub of one provider with the given calendars,
// each holding no events yet.
func newStubCalendarProvider(provider models.ProviderName, calendars ...providers.Calendar) *stubCalendarProvider {
	return &stubCalendarProvider{
		stubProvider: &stubProvider{name: provider},
		calendars:    calendars,
		events:       map[string][]providers.CalendarEvent{},
	}
}

// Calendars implements providers.CalendarProvider.
func (p *stubCalendarProvider) Calendars(context.Context, providers.Credentials, *providers.Tokens) ([]providers.Calendar, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providers.Calendar(nil), p.calendars...), nil
}

// CalendarEvents implements providers.CalendarProvider: the window is honoured
// because the engine filters nothing itself.
func (p *stubCalendarProvider) CalendarEvents(_ context.Context, _ providers.Credentials, _ *providers.Tokens, calendarID string, window providers.CalendarWindow) ([]providers.CalendarEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	listed := make([]providers.CalendarEvent, 0, len(p.events[calendarID]))
	for _, event := range p.events[calendarID] {
		if event.Start.Before(window.End) && !event.Start.Before(window.Start) {
			listed = append(listed, event)
		}
	}
	return listed, nil
}

// CreateCalendarEvent implements providers.CalendarProvider: the event is stored
// the way a provider would store it, with an id and a modification stamp.
func (p *stubCalendarProvider) CreateCalendarEvent(_ context.Context, _ providers.Credentials, _ *providers.Tokens, calendarID string, input providers.CalendarEventInput) (*providers.CalendarEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	event := providers.CalendarEvent{
		ID:          "evt-" + strconv.Itoa(p.nextID),
		UID:         input.UID,
		Summary:     input.Summary,
		Description: input.Description,
		Location:    input.Location,
		Start:       input.Start,
		End:         input.End,
		AllDay:      input.AllDay,
		TimeZone:    input.TimeZone,
		UpdatedAt:   fixedNow,
		Attendees:   append([]string(nil), input.Attendees...),
	}
	if event.UID == "" {
		event.UID = providers.CalendarEventUID(event.ID)
	}
	p.events[calendarID] = append(p.events[calendarID], event)
	created := event
	return &created, nil
}

// UpdateCalendarEvent implements providers.CalendarProvider.
func (p *stubCalendarProvider) UpdateCalendarEvent(_ context.Context, _ providers.Credentials, _ *providers.Tokens, calendarID, eventID string, input providers.CalendarEventInput) (*providers.CalendarEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	events := p.events[calendarID]
	for index, event := range events {
		if event.ID != eventID {
			continue
		}
		event.Summary = input.Summary
		event.Description = input.Description
		event.Location = input.Location
		event.Start = input.Start
		event.End = input.End
		event.AllDay = input.AllDay
		event.TimeZone = input.TimeZone
		event.Attendees = append([]string(nil), input.Attendees...)
		event.UpdatedAt = fixedNow.Add(time.Minute)
		events[index] = event
		updated := event
		return &updated, nil
	}
	return nil, fmt.Errorf("%w: event %s", providers.ErrNotFound, eventID)
}

// DeleteCalendarEvent implements providers.CalendarProvider.
func (p *stubCalendarProvider) DeleteCalendarEvent(_ context.Context, _ providers.Credentials, _ *providers.Tokens, calendarID, eventID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	events := p.events[calendarID]
	for index, event := range events {
		if event.ID != eventID {
			continue
		}
		p.events[calendarID] = append(events[:index], events[index+1:]...)
		return nil
	}
	return fmt.Errorf("%w: event %s", providers.ErrNotFound, eventID)
}

// seedEvent stores an event as if the provider already held it.
func (p *stubCalendarProvider) seedEvent(calendarID string, event providers.CalendarEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events[calendarID] = append(p.events[calendarID], event)
}

// eventsOf returns a copy of the events of a calendar.
func (p *stubCalendarProvider) eventsOf(calendarID string) []providers.CalendarEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providers.CalendarEvent(nil), p.events[calendarID]...)
}

// -----------------------------------------------------------------------------
// Job endpoints
// -----------------------------------------------------------------------------

// calendarServer wires an API whose Microsoft provider is a calendar stub and
// returns the accounts of a source and a destination calendar, both connected.
func calendarServer(t *testing.T) (*Server, *stubCalendarProvider, *models.ConnectedAccount, *models.ConnectedAccount) {
	t.Helper()
	stub := newStubCalendarProvider(models.ProviderMicrosoft,
		providers.Calendar{ID: "cal-source", Name: "Work"},
		providers.Calendar{ID: "cal-target", Name: "Family", Primary: true},
	)
	server, store := newTestServerWith(t, models.AuthModeNone, testOptions{
		// The token manager resolves an account through its OAuth client, so the
		// stub needs one installed the way an operator installs it.
		configure: func(cfg *config.Config) {
			cfg.Microsoft = config.ProviderCredentials{
				ClientID:     "stub-client",
				ClientSecret: "stub-secret",
				RedirectURI:  "https://sync.example.com/api/oauth/microsoft/callback",
			}
		},
		providers: []providers.Provider{stub},
	})
	source := saveAccount(t, store, models.ProviderMicrosoft, "ms-source", "source@outlook.com")
	target := saveAccount(t, store, models.ProviderMicrosoft, "ms-target", "target@outlook.com")
	for _, account := range []*models.ConnectedAccount{source, target} {
		if err := server.deps.Tokens.Persist(context.Background(), account, &providers.Tokens{
			AccessToken:  "stub-access",
			RefreshToken: "stub-refresh",
			TokenType:    "Bearer",
			Expiry:       time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("storing the tokens: %v", err)
		}
	}
	return server, stub, source, target
}

// calendarBody is the create payload of a provider-to-provider job.
func calendarBody(name string, source, target *models.ConnectedAccount) map[string]any {
	return map[string]any{
		"name":                      name,
		"source_kind":               "microsoft",
		"source_account_id":         source.ID,
		"source_calendar_id":        "cal-source",
		"source_calendar_name":      "Work",
		"destination_account_id":    target.ID,
		"destination_calendar_id":   "cal-target",
		"destination_calendar_name": "Family",
	}
}

// TestCalendarJobLifecycle walks the CRUD endpoints of a calendar job and pins
// the defaults the model promises.
func TestCalendarJobLifecycle(t *testing.T) {
	server, _, source, target := calendarServer(t)

	recorder := call(t, server, http.MethodPost, "/api/calendars", calendarBody("Work in the family calendar", source, target))
	requireStatus(t, recorder, http.StatusCreated)
	created := decodeJSON[calendarView](t, recorder)
	if created.ID == 0 {
		t.Fatalf("the job was not stored: %+v", created)
	}
	if created.SourceKind != "microsoft" || created.Direction != "bidirectional" || created.ConflictPolicy != "newest_wins" {
		t.Fatalf("defaults = %+v", created)
	}
	if created.PastDays != calendarPastDefault || created.FutureDays != calendarFutureDefault {
		t.Fatalf("window = %d/%d, want %d/%d", created.PastDays, created.FutureDays, calendarPastDefault, calendarFutureDefault)
	}
	if !created.Enabled || created.Running {
		t.Fatalf("enabled = %v, running = %v", created.Enabled, created.Running)
	}
	if created.NextRunAt != nil {
		t.Fatalf("a manual-only job has a schedule: %v", created.NextRunAt)
	}

	recorder = call(t, server, http.MethodGet, "/api/calendars", nil)
	requireStatus(t, recorder, http.StatusOK)
	list := decodeJSON[struct {
		Calendars []calendarView `json:"calendars"`
	}](t, recorder)
	if len(list.Calendars) != 1 || list.Calendars[0].ID != created.ID {
		t.Fatalf("calendars = %+v", list.Calendars)
	}

	path := "/api/calendars/" + strconv.FormatUint(uint64(created.ID), 10)
	recorder = call(t, server, http.MethodGet, path, nil)
	requireStatus(t, recorder, http.StatusOK)

	updated := calendarBody("Everything", source, target)
	updated["interval_minutes"] = 15
	updated["conflict_policy"] = "source_wins"
	updated["past_days"] = 7
	recorder = call(t, server, http.MethodPut, path, updated)
	requireStatus(t, recorder, http.StatusOK)
	after := decodeJSON[calendarView](t, recorder)
	if after.Name != "Everything" || after.IntervalMinutes != 15 || after.ConflictPolicy != "source_wins" || after.PastDays != 7 {
		t.Fatalf("update = %+v", after)
	}
	if after.NextRunAt == nil {
		t.Fatalf("a scheduled job has no next run")
	}

	recorder = call(t, server, http.MethodDelete, path, nil)
	requireStatus(t, recorder, http.StatusOK)
	recorder = call(t, server, http.MethodGet, path, nil)
	requireStatus(t, recorder, http.StatusNotFound)
}

// TestCalendarJobValidation pins the refusals of the create payload: every case
// is a mistake an operator can make in the editor.
func TestCalendarJobValidation(t *testing.T) {
	server, _, source, target := calendarServer(t)
	cases := []struct {
		name   string
		body   map[string]any
		status int
	}{
		{
			name:   "no name",
			body:   mutate(calendarBody("", source, target), "name", ""),
			status: http.StatusBadRequest,
		},
		{
			name:   "an unknown source kind",
			body:   mutate(calendarBody("x", source, target), "source_kind", "dropbox"),
			status: http.StatusBadRequest,
		},
		{
			name:   "an unknown direction",
			body:   mutate(calendarBody("x", source, target), "direction", "sideways"),
			status: http.StatusBadRequest,
		},
		{
			name:   "an unknown conflict policy",
			body:   mutate(calendarBody("x", source, target), "conflict_policy", "coin_flip"),
			status: http.StatusBadRequest,
		},
		{
			name: "the same calendar on both sides",
			body: mutate(mutate(calendarBody("x", source, target), "source_account_id", target.ID),
				"source_calendar_id", "cal-target"),
			status: http.StatusBadRequest,
		},
		{
			name:   "an account that does not exist",
			body:   mutate(calendarBody("x", source, target), "destination_account_id", uint(4242)),
			status: http.StatusNotFound,
		},
		{
			name: "a subscription without a URL",
			body: mutate(mutate(calendarBody("x", source, target), "source_kind", "ics"),
				"source_url", ""),
			status: http.StatusBadRequest,
		},
		{
			name: "a subscription with a non-http URL",
			body: mutate(mutate(calendarBody("x", source, target), "source_kind", "ics"),
				"source_url", "ftp://example.com/feed.ics"),
			status: http.StatusBadRequest,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := call(t, server, http.MethodPost, "/api/calendars", testCase.body)
			requireStatus(t, recorder, testCase.status)
		})
	}
}

// mutate returns a copy of a payload with one key replaced, so a case only
// spells out what it changes.
func mutate(body map[string]any, key string, value any) map[string]any {
	copyOf := make(map[string]any, len(body))
	for k, v := range body {
		copyOf[k] = v
	}
	copyOf[key] = value
	return copyOf
}

// calendarActionCreate is the plan action of a copy that has to be written to the
// destination; the plan vocabulary of internal/services uses these strings.
const calendarActionCreate = "create_destination"

// TestCalendarRunAndPreview drives the engine through the API: the preview
// answers a plan without writing, the run writes it, the history shows it.
func TestCalendarRunAndPreview(t *testing.T) {
	server, stub, source, target := calendarServer(t)
	near := time.Now().UTC().Truncate(time.Minute).Add(24 * time.Hour)
	stub.seedEvent("cal-source", providers.CalendarEvent{
		ID:        "src-1",
		UID:       "uid-1",
		Summary:   "Dentist",
		Start:     near,
		End:       near.Add(time.Hour),
		UpdatedAt: fixedNow,
	})

	recorder := call(t, server, http.MethodPost, "/api/calendars", calendarBody("Mirror", source, target))
	requireStatus(t, recorder, http.StatusCreated)
	job := decodeJSON[calendarView](t, recorder)
	path := "/api/calendars/" + strconv.FormatUint(uint64(job.ID), 10)

	recorder = call(t, server, http.MethodGet, path+"/preview", nil)
	requireStatus(t, recorder, http.StatusOK)
	preview := decodeJSON[services.CalendarPreview](t, recorder)
	if preview.JobID != job.ID || preview.SourceListed != 1 {
		t.Fatalf("preview = %+v", preview)
	}
	if len(preview.Plan) != 1 || preview.Plan[0].Action != calendarActionCreate {
		t.Fatalf("plan = %+v", preview.Plan)
	}
	if len(stub.eventsOf("cal-target")) != 0 {
		t.Fatalf("the preview wrote to the destination")
	}

	recorder = call(t, server, http.MethodPost, path+"/run", nil)
	requireStatus(t, recorder, http.StatusAccepted)
	started := decodeJSON[struct {
		Run models.CalendarRun `json:"run"`
	}](t, recorder)
	if started.Run.ID == 0 || started.Run.JobID != job.ID {
		t.Fatalf("run = %+v", started.Run)
	}
	server.deps.Calendars.Wait()

	recorder = call(t, server, http.MethodGet, path+"/runs", nil)
	requireStatus(t, recorder, http.StatusOK)
	history := decodeJSON[struct {
		Runs []models.CalendarRun `json:"runs"`
	}](t, recorder)
	if len(history.Runs) != 1 {
		t.Fatalf("runs = %+v", history.Runs)
	}
	if history.Runs[0].Status != string(models.RunSuccess) {
		t.Fatalf("status = %q, message = %q", history.Runs[0].Status, history.Runs[0].Message)
	}

	runPath := path + "/runs/" + strconv.FormatUint(uint64(started.Run.ID), 10)
	recorder = call(t, server, http.MethodGet, runPath, nil)
	requireStatus(t, recorder, http.StatusOK)
	detail := decodeJSON[struct {
		Run   models.CalendarRun       `json:"run"`
		Items []models.CalendarRunItem `json:"items"`
	}](t, recorder)
	if len(detail.Items) != 1 || detail.Items[0].Action != calendarActionCreate {
		t.Fatalf("items = %+v", detail.Items)
	}
	written := stub.eventsOf("cal-target")
	if len(written) != 1 || written[0].Summary != "Dentist" {
		t.Fatalf("destination = %+v", written)
	}

	// A run of another job is not readable through this one.
	recorder = call(t, server, http.MethodGet, path+"/runs/999999", nil)
	requireStatus(t, recorder, http.StatusNotFound)

	// Cancelling a job that is not running is not an error.
	recorder = call(t, server, http.MethodPost, path+"/cancel", nil)
	requireStatus(t, recorder, http.StatusOK)
}

// TestAccountCalendarPicker pins the answer of the picker endpoint.
func TestAccountCalendarPicker(t *testing.T) {
	server, _, source, _ := calendarServer(t)

	recorder := call(t, server, http.MethodGet, "/api/accounts/"+strconv.FormatUint(uint64(source.ID), 10)+"/calendars", nil)
	requireStatus(t, recorder, http.StatusOK)
	list := decodeJSON[struct {
		Calendars []calendarOption `json:"calendars"`
	}](t, recorder)
	if len(list.Calendars) != 2 {
		t.Fatalf("calendars = %+v", list.Calendars)
	}
	if list.Calendars[0].ID != "cal-source" || list.Calendars[0].Name != "Work" || list.Calendars[0].Primary {
		t.Fatalf("first = %+v", list.Calendars[0])
	}
	if !list.Calendars[1].Primary {
		t.Fatalf("the primary calendar is not marked: %+v", list.Calendars[1])
	}

	recorder = call(t, server, http.MethodGet, "/api/accounts/999999/calendars", nil)
	requireStatus(t, recorder, http.StatusNotFound)
}

// -----------------------------------------------------------------------------
// iCalendar subscriptions
// -----------------------------------------------------------------------------

// feedServer serves a small .ics document, with the ETag a real feed would send.
func feedServer(t *testing.T, events int) *httptest.Server {
	t.Helper()
	body := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//sync//test//EN",
	}
	first := time.Now().UTC().Truncate(time.Hour).Add(48 * time.Hour)
	for index := 0; index < events; index++ {
		body = append(body,
			"BEGIN:VEVENT",
			"UID:feed-"+strconv.Itoa(index)+"@sync.local",
			"SUMMARY:Feed event "+strconv.Itoa(index),
			"DTSTART:"+first.Add(time.Duration(index)*time.Hour).Format("20060102T150405Z"),
			"DTEND:"+first.Add(time.Duration(index+1)*time.Hour).Format("20060102T150405Z"),
			"END:VEVENT",
		)
	}
	body = append(body, "END:VCALENDAR", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/calendar")
		w.Header().Set("ETag", `"feed-1"`)
		_, _ = w.Write([]byte(strings.Join(body, "\r\n")))
	}))
	t.Cleanup(server.Close)
	return server
}

// feedServerWithPrivateTargets is the test server with the operator policy that
// allows a subscription on the loopback interface: httptest lives there.
func feedServerWithPrivateTargets(t *testing.T) *Server {
	t.Helper()
	server, _ := newTestServerWith(t, models.AuthModeNone, testOptions{
		configure: func(cfg *config.Config) { cfg.CalendarAllowPrivateTargets = true },
	})
	return server
}

// TestCalendarFeedPreview pins the "check this subscription" call: the editor
// learns how many events the URL holds before a job exists.
func TestCalendarFeedPreview(t *testing.T) {
	server := feedServerWithPrivateTargets(t)
	feed := feedServer(t, 2)

	recorder := call(t, server, http.MethodPost, "/api/calendars/feed-preview", map[string]any{
		"url":         feed.URL,
		"past_days":   7,
		"future_days": 30,
	})
	requireStatus(t, recorder, http.StatusOK)
	preview := decodeJSON[services.FeedPreview](t, recorder)
	if preview.Events != 2 || len(preview.Sample) != 2 {
		t.Fatalf("preview = %+v", preview)
	}
	if preview.ETag != `"feed-1"` {
		t.Fatalf("etag = %q", preview.ETag)
	}
	if preview.Sample[0].Summary != "Feed event 0" || preview.Sample[0].UID == "" {
		t.Fatalf("sample = %+v", preview.Sample)
	}

	recorder = call(t, server, http.MethodPost, "/api/calendars/feed-preview", map[string]any{"url": "not a url"})
	requireStatus(t, recorder, http.StatusBadRequest)
	recorder = call(t, server, http.MethodPost, "/api/calendars/feed-preview", map[string]any{"url": ""})
	requireStatus(t, recorder, http.StatusBadRequest)
}

// TestCalendarFeedRefusesPrivateTargetByDefault pins the SSRF policy: a
// subscription to a loopback address is refused unless the operator allowed it.
func TestCalendarFeedRefusesPrivateTargetByDefault(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)
	feed := feedServer(t, 1)

	recorder := call(t, server, http.MethodPost, "/api/calendars/feed-preview", map[string]any{"url": feed.URL})
	requireStatus(t, recorder, http.StatusBadRequest)
	if strings.Contains(recorder.Body.String(), "Feed event") {
		t.Fatalf("the feed was read: %s", recorder.Body.String())
	}
}

// TestCalendarSubscriptionJob pins how a job is stored when its source is an .ics
// feed: the direction is forced one way, the password never leaves the process
// and switching back to a provider source clears the subscription columns.
func TestCalendarSubscriptionJob(t *testing.T) {
	server := feedServerWithPrivateTargets(t)
	target := saveAccount(t, server.deps.Store, models.ProviderMicrosoft, "ms-target", "target@outlook.com")
	feed := feedServer(t, 1)

	body := map[string]any{
		"name":                      "Holidays",
		"source_kind":               "ics",
		"source_url":                feed.URL,
		"source_username":           "operator",
		"source_password":           "s3cret-feed",
		"direction":                 "bidirectional",
		"destination_account_id":    target.ID,
		"destination_calendar_id":   "primary",
		"destination_calendar_name": "Calendar",
	}
	recorder := call(t, server, http.MethodPost, "/api/calendars", body)
	requireStatus(t, recorder, http.StatusCreated)
	if strings.Contains(recorder.Body.String(), "s3cret-feed") {
		t.Fatalf("the password is in the answer: %s", recorder.Body.String())
	}
	created := decodeJSON[calendarView](t, recorder)
	if created.SourceKind != "ics" || created.Direction != "one_way" {
		t.Fatalf("job = %+v", created)
	}
	if created.SourceCalendarID != calendarMarkerICS || created.SourceURL != feed.URL {
		t.Fatalf("subscription columns = %+v", created)
	}
	if created.SourceAccountID != 0 {
		t.Fatalf("an .ics job carries an account: %d", created.SourceAccountID)
	}

	stored, err := server.deps.Store.GetCalendarJob(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("loading the job: %v", err)
	}
	if stored.SourceSecret == "" || strings.Contains(stored.SourceSecret, "s3cret-feed") {
		t.Fatalf("the secret was not sealed: %q", stored.SourceSecret)
	}

	path := "/api/calendars/" + strconv.FormatUint(uint64(created.ID), 10)
	// An update that leaves the password out keeps the stored secret.
	body["source_password"] = ""
	body["name"] = "Holidays and birthdays"
	recorder = call(t, server, http.MethodPut, path, body)
	requireStatus(t, recorder, http.StatusOK)
	kept, err := server.deps.Store.GetCalendarJob(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("loading the job: %v", err)
	}
	if kept.SourceSecret != stored.SourceSecret {
		t.Fatalf("the secret changed: %q -> %q", stored.SourceSecret, kept.SourceSecret)
	}

	// An explicit clear wipes it.
	body["clear_source_password"] = true
	recorder = call(t, server, http.MethodPut, path, body)
	requireStatus(t, recorder, http.StatusOK)
	cleared, err := server.deps.Store.GetCalendarJob(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("loading the job: %v", err)
	}
	if cleared.SourceSecret != "" {
		t.Fatalf("the secret survived: %q", cleared.SourceSecret)
	}

	// Switching the source to a provider account drops the subscription.
	body["clear_source_password"] = false
	body["source_kind"] = "microsoft"
	body["source_account_id"] = target.ID
	body["source_calendar_id"] = "cal-other"
	recorder = call(t, server, http.MethodPut, path, body)
	requireStatus(t, recorder, http.StatusOK)
	switched := decodeJSON[calendarView](t, recorder)
	if switched.SourceURL != "" || switched.SourceUsername != "" || switched.SourceCalendarID != "cal-other" {
		t.Fatalf("switched = %+v", switched)
	}
	if switched.Direction != "bidirectional" {
		t.Fatalf("the direction of the payload was not applied: %q", switched.Direction)
	}
}
