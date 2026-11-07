// This file covers the collector of the Audit Logs feature (auditlog.go): the
// grouping of the connected accounts by organisation, the per-stream cursor that
// makes a pass incremental, the failure of one feed never hiding the others, and
// the ordering rules that keep a throttled organisation out of the rest of the
// pass.

package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// activityCall is one ActivityLogs call the stub provider received.
type activityCall struct {
	Stream string
	Cursor string
}

// stubActivityProvider is a provider with an activity feed: the fake drive of the
// engine tests plus the two feeds of a Microsoft organisation, scripted per
// stream so a test can answer events, an error or a rate limit.
type stubActivityProvider struct {
	*fakeProvider

	mu      sync.Mutex
	streams []string
	calls   []activityCall
	events  map[string][]providers.ActivityEvent
	next    map[string]string
	errors  map[string]error
	// normalised answers the repair pass: the event a stored vendor document is
	// re-read into, keyed by the document itself.
	normalised map[string]providers.ActivityEvent
	// enrich is the enrichment of the repair pass (see
	// providers.ActivityActorEnricher); nil means the stub cannot improve an
	// actor, which is what a feed without owners looks like.
	enrich func(events []providers.ActivityEvent) int
}

// newStubActivityProvider builds the stub around an empty fake drive.
func newStubActivityProvider(name models.ProviderName) *stubActivityProvider {
	return &stubActivityProvider{
		fakeProvider: newFakeProvider(name),
		streams:      []string{"directory", "unified"},
		events:       map[string][]providers.ActivityEvent{},
		next:         map[string]string{},
		errors:       map[string]error{},
		normalised:   map[string]providers.ActivityEvent{},
	}
}

// ActivityLogStreams implements providers.ActivityLogSource.
func (p *stubActivityProvider) ActivityLogStreams() []string { return p.streams }

// ActivityLogScopes implements providers.ActivityLogSource.
func (p *stubActivityProvider) ActivityLogScopes() []string {
	return []string{"https://graph.microsoft.com/AuditLog.Read.All"}
}

// ActivityLogs implements providers.ActivityLogSource: it records the call and
// answers what the test scripted for that stream.
func (p *stubActivityProvider) ActivityLogs(_ context.Context, _ providers.Credentials, _ *providers.Tokens, stream, cursor string) ([]providers.ActivityEvent, string, error) {
	p.mu.Lock()
	p.calls = append(p.calls, activityCall{Stream: stream, Cursor: cursor})
	events := append([]providers.ActivityEvent{}, p.events[stream]...)
	next := p.next[stream]
	failure := p.errors[stream]
	p.mu.Unlock()
	if failure != nil {
		return nil, "", failure
	}
	if next == "" {
		next = "2026-11-07T12:00:00Z"
	}
	return events, next, nil
}

// NormaliseActivityEvent implements providers.ActivityLogNormaliser: the repair
// pass re-reads a stored vendor document through it.
func (p *stubActivityProvider) NormaliseActivityEvent(source string, raw json.RawMessage) (providers.ActivityEvent, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	event, ok := p.normalised[string(raw)]
	if event.Source == "" {
		event.Source = source
	}
	return event, ok
}

// EnrichActivityActors implements providers.ActivityActorEnricher: it hands the
// events of the batch to whatever the test scripted.
func (p *stubActivityProvider) EnrichActivityActors(_ context.Context, _ *providers.Tokens, events []providers.ActivityEvent) int {
	if p.enrich == nil {
		return 0
	}
	return p.enrich(events)
}

// callsFor lists the cursors a stream was called with, in order.
func (p *stubActivityProvider) callsFor(stream string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []string{}
	for _, call := range p.calls {
		if call.Stream == stream {
			out = append(out, call.Cursor)
		}
	}
	return out
}

// activityEvent builds one normalised event of a stream.
func activityEventOf(source, remoteID string, action models.AuditLogAction, actor, targetName string, at time.Time) providers.ActivityEvent {
	return providers.ActivityEvent{
		Source:     source,
		RemoteID:   remoteID,
		Action:     action,
		Target:     models.AuditLogTargetFile,
		ActorEmail: actor,
		TargetName: targetName,
		OccurredAt: at,
		Raw:        []byte(`{"vendor":"record"}`),
	}
}

// activityEnv is the fixture of the collector tests: a store, the token manager,
// the collector and the stub provider behind it.
type activityEnv struct {
	store    *Store
	tokens   *TokenManager
	service  *AuditLogService
	provider *stubActivityProvider
}

// newActivityEnv builds the fixture with no account connected yet.
func newActivityEnv(t *testing.T) *activityEnv {
	t.Helper()
	cfg := testConfig(t)
	cfg.Microsoft = config.ProviderCredentials{
		ClientID:     "microsoft-id",
		ClientSecret: "microsoft-secret",
		RedirectURI:  cfg.AppURL + "/api/oauth/microsoft/callback",
	}
	store := newTestStore(t)
	box := NewSecretBox(cfg.EncryptionKeyBytes())
	provider := newStubActivityProvider(models.ProviderMicrosoft)
	registry := providers.NewRegistry(provider)
	tokens := NewTokenManager(store, registry, NewProviderSettings(cfg, store.Settings(), box), box)
	return &activityEnv{store: store, tokens: tokens, service: NewAuditLogService(store, tokens), provider: provider}
}

// connect stores one connected account of the fixture's provider.
func (e *activityEnv) connect(t *testing.T, email, orgID string, status models.AccountStatus) *models.ConnectedAccount {
	t.Helper()
	box := e.tokens.box
	access, err := box.Encrypt("access-" + email)
	if err != nil {
		t.Fatalf("encrypting the access token: %v", err)
	}
	refresh, err := box.Encrypt("refresh-" + email)
	if err != nil {
		t.Fatalf("encrypting the refresh token: %v", err)
	}
	account := &models.ConnectedAccount{
		Provider:          string(models.ProviderMicrosoft),
		ProviderAccountID: "account-" + email,
		Email:             email,
		DisplayName:       email,
		OrgID:             orgID,
		AccessToken:       access,
		RefreshToken:      refresh,
		TokenType:         "Bearer",
		ExpiresAt:         time.Now().UTC().Add(time.Hour),
		Status:            string(status),
	}
	if err := e.store.SaveAccount(context.Background(), account); err != nil {
		t.Fatalf("saving the account %s: %v", email, err)
	}
	return account
}

// cursorOf loads one stored cursor.
func (e *activityEnv) cursorOf(t *testing.T, provider models.ProviderName, orgID, stream string) models.AuditLogCursor {
	t.Helper()
	cursor, err := e.store.AuditLogCursorFor(context.Background(), string(provider), orgID, stream)
	if err != nil {
		t.Fatalf("loading the cursor of %s/%s: %v", orgID, stream, err)
	}
	return cursor
}

// TestCollectReadsOneOrganisationOnce pins the grouping: two administrator
// accounts of the same tenant produce one read per stream, and the events are
// stored under that organisation.
func TestCollectReadsOneOrganisationOnce(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "admin@contoso.com", "tenant-a", models.AccountConnected)
	env.connect(t, "second@contoso.com", "tenant-a", models.AccountConnected)
	now := time.Date(2026, 11, 7, 11, 0, 0, 0, time.UTC)
	env.provider.events["directory"] = []providers.ActivityEvent{
		activityEventOf("directory", "dir-1", models.AuditLogDelete, "admin@contoso.com", "Bob New", now),
	}

	result := env.service.Collect(context.Background())
	if result.Organisations != 1 {
		t.Fatalf("organisations = %d, want the two accounts grouped into one", result.Organisations)
	}
	if result.Streams != 2 || result.Failed != 0 {
		t.Fatalf("streams/failed = %d/%d, want 2/0", result.Streams, result.Failed)
	}
	if result.Events != 1 {
		t.Fatalf("events = %d, want 1", result.Events)
	}
	if calls := len(env.provider.callsFor("directory")); calls != 1 {
		t.Fatalf("the directory feed was read %d times, want once per organisation", calls)
	}

	rows, total, err := env.store.ListAuditLogEvents(context.Background(), AuditLogFilter{})
	if err != nil {
		t.Fatalf("listing the events: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("stored %d/%d events, want 1", len(rows), total)
	}
	if rows[0].OrgID != "tenant-a" || rows[0].Provider != string(models.ProviderMicrosoft) {
		t.Errorf("stored under %s/%s, want microsoft/tenant-a", rows[0].Provider, rows[0].OrgID)
	}

	cursor := env.cursorOf(t, models.ProviderMicrosoft, "tenant-a", "directory")
	if cursor.Cursor != "2026-11-07T12:00:00Z" {
		t.Errorf("cursor = %q, want the watermark the provider answered", cursor.Cursor)
	}
	if cursor.LastEventAt == nil || !cursor.LastEventAt.Equal(now) {
		t.Errorf("last_event_at = %v, want %s", cursor.LastEventAt, now)
	}
	if cursor.LastError != "" || cursor.LastRunAt == nil {
		t.Errorf("cursor = %+v, want a successful run recorded", cursor)
	}
}

// TestCollectResumesFromTheStoredCursor pins the incrementality: the second pass
// hands the stored watermark back to the provider, and a re-read of the same
// window stores nothing twice.
func TestCollectResumesFromTheStoredCursor(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "admin@contoso.com", "tenant-a", models.AccountConnected)
	env.provider.events["directory"] = []providers.ActivityEvent{
		activityEventOf("directory", "dir-1", models.AuditLogCreate, "admin@contoso.com", "Bob New",
			time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)),
	}

	env.service.Collect(context.Background())
	env.provider.next["directory"] = "2026-11-07T12:30:00Z"
	env.service.Collect(context.Background())

	cursors := env.provider.callsFor("directory")
	if len(cursors) != 2 {
		t.Fatalf("the feed was read %d times, want 2", len(cursors))
	}
	if cursors[0] != "" {
		t.Errorf("first pass cursor = %q, want an empty one (the default window)", cursors[0])
	}
	if cursors[1] != "2026-11-07T12:00:00Z" {
		t.Errorf("second pass cursor = %q, want the stored watermark", cursors[1])
	}
	_, total, err := env.store.ListAuditLogEvents(context.Background(), AuditLogFilter{})
	if err != nil {
		t.Fatalf("listing the events: %v", err)
	}
	if total != 1 {
		t.Fatalf("stored %d events after two overlapping passes, want 1", total)
	}
}

// TestCollectKeepsTheOtherStreamsWhenOneFails pins the graceful degradation: a
// tenant that did not consent to the workload feed still gets its directory
// changes, and the failure is explained on the cursor of the stream that failed.
func TestCollectKeepsTheOtherStreamsWhenOneFails(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "admin@contoso.com", "tenant-a", models.AccountConnected)
	env.provider.errors["unified"] = errors.New("microsoft: the office 365 activity feed needs an administrator to consent to ActivityFeed.Read")
	env.provider.events["directory"] = []providers.ActivityEvent{
		activityEventOf("directory", "dir-1", models.AuditLogCreate, "admin@contoso.com", "Bob New",
			time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)),
	}

	result := env.service.Collect(context.Background())
	if result.Streams != 2 || result.Failed != 1 || result.Events != 1 {
		t.Fatalf("streams/failed/events = %d/%d/%d, want 2/1/1", result.Streams, result.Failed, result.Events)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "ActivityFeed.Read") {
		t.Fatalf("errors = %v, want the consent line", result.Errors)
	}

	failed := env.cursorOf(t, models.ProviderMicrosoft, "tenant-a", "unified")
	if !strings.Contains(failed.LastError, "ActivityFeed.Read") {
		t.Errorf("unified last_error = %q, want the reason", failed.LastError)
	}
	if failed.Cursor != "" {
		t.Errorf("unified cursor = %q, want it untouched after a failure", failed.Cursor)
	}
	healthy := env.cursorOf(t, models.ProviderMicrosoft, "tenant-a", "directory")
	if healthy.LastError != "" || healthy.Cursor == "" {
		t.Errorf("directory cursor = %+v, want it advanced by the healthy pass", healthy)
	}
}

// TestCollectStopsTheOrganisationOnRateLimit pins the backoff rule: once a feed
// of one organisation is throttled, the remaining feeds of that organisation are
// left for the next pass instead of burning through the window.
func TestCollectStopsTheOrganisationOnRateLimit(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "admin@contoso.com", "tenant-a", models.AccountConnected)
	env.provider.errors["directory"] = fmt.Errorf("microsoft graph: %w", providers.ErrRateLimited)

	result := env.service.Collect(context.Background())
	if result.Failed != 1 {
		t.Fatalf("failed = %d, want the throttled feed alone", result.Failed)
	}
	if calls := env.provider.callsFor("unified"); len(calls) != 0 {
		t.Fatalf("the second feed was read %d times after a throttle, want none", len(calls))
	}
}

// TestCollectSkipsAccountsWithoutAnOrganisation pins the one case the collector
// refuses to guess: an account that cannot name its organisation has no feed to
// read, and no request is made for it.
func TestCollectSkipsAccountsWithoutAnOrganisation(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "someone@example.test", "", models.AccountConnected)

	result := env.service.Collect(context.Background())
	if result.Organisations != 0 || result.Streams != 0 || len(env.provider.callsFor("directory")) != 0 {
		t.Fatalf("result = %+v with %d calls, want no read at all", result, len(env.provider.callsFor("directory")))
	}
}

// TestCollectSkipsAnOrganisationWhoseAccountsNeedAttention pins the choice of the
// reading account: a group whose only account is in error is skipped, and the
// healthy account of the group is used when there is one.
func TestCollectSkipsAnOrganisationWhoseAccountsNeedAttention(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "broken@contoso.com", "tenant-a", models.AccountError)
	result := env.service.Collect(context.Background())
	if result.Organisations != 0 || len(env.provider.callsFor("directory")) != 0 {
		t.Fatalf("an organisation without a healthy account was read: %+v", result)
	}

	env.connect(t, "healthy@contoso.com", "tenant-a", models.AccountConnected)
	result = env.service.Collect(context.Background())
	if result.Organisations != 1 {
		t.Fatalf("organisations = %d, want the group read once the healthy account joined", result.Organisations)
	}
}

// TestCollectGroupsByOrganisationCaseInsensitively pins the key normalisation:
// the tenant of a row and the tenant of a token claim differ in case, and one
// organisation must stay one group.
func TestCollectGroupsByOrganisationCaseInsensitively(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "first@contoso.com", "TENANT-A", models.AccountConnected)
	env.connect(t, "second@contoso.com", "tenant-a", models.AccountConnected)

	result := env.service.Collect(context.Background())
	if result.Organisations != 1 {
		t.Fatalf("organisations = %d, want one group for the two spellings", result.Organisations)
	}
	cursor := env.cursorOf(t, models.ProviderMicrosoft, "tenant-a", "directory")
	if cursor.Provider != string(models.ProviderMicrosoft) {
		t.Errorf("cursor provider = %q, want the stored organisation key", cursor.Provider)
	}
}

// TestAuditLogRowDropsRecordsWithoutAnIdentity pins the one record the collector
// refuses to store: an event with no id could be stored again by every pass.
func TestAuditLogRowDropsRecordsWithoutAnIdentity(t *testing.T) {
	group := auditLogOrganisation{provider: models.ProviderMicrosoft, orgID: "tenant-a"}
	if _, ok := auditLogRow(group, "directory", providers.ActivityEvent{ActorEmail: "a@b.test"}); ok {
		t.Fatal("a record without an identity was accepted")
	}
	row, ok := auditLogRow(group, "directory", providers.ActivityEvent{
		RemoteID:   "dir-1",
		Action:     models.AuditLogDelete,
		Target:     models.AuditLogTargetFile,
		ActorEmail: "Admin@Contoso.com",
		OccurredAt: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC),
		Raw:        []byte(`{"vendor":"record"}`),
	})
	if !ok {
		t.Fatal("a complete record was refused")
	}
	if row.ActorEmail != "admin@contoso.com" {
		t.Errorf("ActorEmail = %q, want it lower-cased", row.ActorEmail)
	}
	if row.Source != "directory" || row.OrgID != "tenant-a" {
		t.Errorf("row = %+v, want the stream and the organisation", row)
	}
	if row.Raw != `{"vendor":"record"}` {
		t.Errorf("Raw = %q, want the vendor record", row.Raw)
	}
}

// TestSchedulerRunsTheActivityFeedPass pins the wiring of the fourth loop: the
// scheduler's pass is the collector, it reports what it read, and the interval it
// runs on is the documented one.
func TestSchedulerRunsTheActivityFeedPass(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "admin@contoso.com", "tenant-a", models.AccountConnected)
	env.provider.events["directory"] = []providers.ActivityEvent{
		activityEventOf("directory", "dir-1", models.AuditLogCreate, "admin@contoso.com", "Bob New",
			time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)),
	}
	scheduler := &Scheduler{store: env.store, logs: env.service}

	result := scheduler.RunAuditLogs(context.Background())
	if result.Organisations != 1 || result.Events != 1 || result.Failed != 0 {
		t.Fatalf("pass = %+v, want the organisation read once with one event", result)
	}
	if _, total, err := env.store.ListAuditLogEvents(context.Background(), AuditLogFilter{}); err != nil || total != 1 {
		t.Fatalf("stored events = %d (%v), want 1", total, err)
	}

	// A scheduler built with no account connected must answer an empty pass
	// rather than fail: the loop runs whether or not anything is connected.
	empty := &Scheduler{store: newTestStore(t), logs: NewAuditLogService(newTestStore(t), nil)}
	if result := empty.RunAuditLogs(context.Background()); result.Organisations != 0 || result.Events != 0 {
		t.Fatalf("empty pass = %+v, want nothing read", result)
	}

	if DefaultAuditLogInterval != 10*time.Minute {
		t.Fatalf("DefaultAuditLogInterval = %s, want 10m", DefaultAuditLogInterval)
	}
}

// TestNeedsConsentRecognisesTheManagementApiCode pins the marker that turns a
// refused subscription into the "waiting for a permission" banner: the AF10001
// document spells the code but the permission name may be elided ("permission set
// ()"), so the code itself has to be recognised. A plain server fault must not be
// mistaken for a grant the operator could add.
func TestNeedsConsentRecognisesTheManagementApiCode(t *testing.T) {
	refused := "microsoft: cannot enable the Audit.SharePoint activity subscription: http 401: AF10001:" +
		" The permission set () sent in the request did not include the expected permission."
	if !needsConsent(refused) {
		t.Error("needsConsent() = false, want the AF10001 refusal to ask for a grant")
	}
	if needsConsent("microsoft graph: the directory feed answered http 500") {
		t.Error("needsConsent() = true, want a server fault to stay a plain failure")
	}
}

// legacyDeviceEvent is the event a release before the actor kind stored: the
// service name reached the address column and the object was filed as "other".
func legacyDeviceEvent(at time.Time) models.AuditLogEvent {
	return models.AuditLogEvent{
		Provider:   string(models.ProviderMicrosoft),
		Source:     "directory",
		RemoteID:   "Directory_dev-1",
		OrgID:      "tenant-a",
		Action:     string(models.AuditLogUpdate),
		Target:     string(models.AuditLogTargetOther),
		ActorEmail: "deviceregistrationservice",
		TargetName: "DESKTOP-J979VG0",
		OccurredAt: at,
		Raw:        `{"id":"Directory_dev-1"}`,
	}
}

// TestRepairActorsReattributesTheStoredHistory pins the repair pass: it re-reads
// the vendor document the collector kept, names the person behind a workload and
// writes the result back without touching the rest of the row.
func TestRepairActorsReattributesTheStoredHistory(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "admin@contoso.com", "tenant-a", models.AccountConnected)
	ctx := context.Background()
	at := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

	legacy := legacyDeviceEvent(at)
	if err := env.store.InsertAuditLogEvents(ctx, []models.AuditLogEvent{legacy}); err != nil {
		t.Fatalf("seeding the legacy row: %v", err)
	}
	env.provider.normalised[legacy.Raw] = providers.ActivityEvent{
		Source:     "directory",
		RemoteID:   legacy.RemoteID,
		Action:     models.AuditLogUpdate,
		Target:     models.AuditLogTargetDevice,
		ActorKind:  models.AuditLogActorApplication,
		ActorName:  "Device Registration Service",
		TargetName: "DESKTOP-J979VG0",
		TargetID:   "device-object-id",
		OccurredAt: at,
		Raw:        []byte(legacy.Raw),
	}
	env.provider.enrich = func(events []providers.ActivityEvent) int {
		for i := range events {
			events[i].ActorOnBehalfOf = "ivan@contoso.com"
			events[i].ActorOnBehalfOfName = "Ivan Almeida"
		}
		return len(events)
	}

	result := env.service.RepairActors(ctx)
	if result.Scanned != 1 || result.Updated != 1 || result.Attributed != 1 || result.Remaining != 0 {
		t.Fatalf("repair = %+v, want one row scanned, updated and attributed with none left", result)
	}

	rows, _, err := env.store.ListAuditLogEvents(ctx, AuditLogFilter{})
	if err != nil {
		t.Fatalf("listing the repaired row: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("stored %d rows after the repair, want 1", len(rows))
	}
	row := rows[0]
	if row.ActorKind != string(models.AuditLogActorApplication) || row.ActorEmail != "" {
		t.Errorf("actor = %q/%q, want a workload with no address", row.ActorKind, row.ActorEmail)
	}
	if row.ActorName != "Device Registration Service" {
		t.Errorf("ActorName = %q, want the workload of the record", row.ActorName)
	}
	if row.ActorOnBehalfOf != "ivan@contoso.com" || row.ActorOnBehalfOfName != "Ivan Almeida" {
		t.Errorf("on behalf of = %q/%q, want the enrolling person",
			row.ActorOnBehalfOf, row.ActorOnBehalfOfName)
	}
	if row.Target != string(models.AuditLogTargetDevice) || row.TargetName != "DESKTOP-J979VG0" {
		t.Errorf("target = %q/%q, want the device of the record", row.Target, row.TargetName)
	}
}

// TestRepairActorsIsIdempotent pins the convergence of the pass: a second run has
// nothing left to do, so a scheduler that repairs on every tick costs one count.
func TestRepairActorsIsIdempotent(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "admin@contoso.com", "tenant-a", models.AccountConnected)
	ctx := context.Background()
	legacy := legacyDeviceEvent(time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC))
	if err := env.store.InsertAuditLogEvents(ctx, []models.AuditLogEvent{legacy}); err != nil {
		t.Fatalf("seeding the legacy row: %v", err)
	}

	first := env.service.RepairActors(ctx)
	if first.Updated != 1 || first.Remaining != 0 {
		t.Fatalf("first repair = %+v, want the legacy row repaired", first)
	}
	second := env.service.RepairActors(ctx)
	if second.Scanned != 0 || second.Updated != 0 || second.Remaining != 0 {
		t.Fatalf("second repair = %+v, want nothing left to do", second)
	}
}

// TestRepairActorsRepairsALegacyRowWithoutAVendorDocument pins the fallback: a row
// whose Raw is missing or unreadable is repaired from the columns it already
// carries, so the service name leaves the address column even then.
func TestRepairActorsRepairsALegacyRowWithoutAVendorDocument(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "admin@contoso.com", "tenant-a", models.AccountConnected)
	ctx := context.Background()
	legacy := legacyDeviceEvent(time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC))
	legacy.Raw = ""
	if err := env.store.InsertAuditLogEvents(ctx, []models.AuditLogEvent{legacy}); err != nil {
		t.Fatalf("seeding the legacy row: %v", err)
	}

	if result := env.service.RepairActors(ctx); result.Updated != 1 || result.Remaining != 0 {
		t.Fatalf("repair = %+v, want the row repaired from its own columns", result)
	}
	rows, _, err := env.store.ListAuditLogEvents(ctx, AuditLogFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("listing the repaired row: %v (%d rows)", err, len(rows))
	}
	if rows[0].ActorEmail != "" {
		t.Errorf("ActorEmail = %q, want the service name out of the address column", rows[0].ActorEmail)
	}
	if rows[0].ActorKind != string(models.AuditLogActorApplication) || rows[0].ActorName != "deviceregistrationservice" {
		t.Errorf("actor = %q/%q, want the workload name kept in its own column",
			rows[0].ActorKind, rows[0].ActorName)
	}
}

// TestCollectRepairsTheHistory pins the wiring: a collect pass repairs the history
// left by the previous release without anybody asking, and reports it.
func TestCollectRepairsTheHistory(t *testing.T) {
	env := newActivityEnv(t)
	env.connect(t, "admin@contoso.com", "tenant-a", models.AccountConnected)
	ctx := context.Background()
	legacy := legacyDeviceEvent(time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC))
	if err := env.store.InsertAuditLogEvents(ctx, []models.AuditLogEvent{legacy}); err != nil {
		t.Fatalf("seeding the legacy row: %v", err)
	}

	result := env.service.Collect(ctx)
	if result.Repaired != 1 || result.PendingRepairs != 0 {
		t.Fatalf("collect = %+v, want the legacy row repaired by the pass", result)
	}
}
