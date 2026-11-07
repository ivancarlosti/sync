// This file holds the collector behind the Audit Logs screen: the pass that reads
// the activity feed of every organisation the connected accounts belong to and
// stores what it read.
//
// It is a poller, not a webhook receiver: neither Microsoft nor Google offers a
// push subscription an operator can register without extra infrastructure, and
// polling is what keeps Sync a single binary with no public callback URL. The
// vendor retention windows are the reason the local copy exists at all — Office
// 365 activity blobs expire after about seven days, the directory feed keeps
// thirty, Google keeps six months — so a history that must outlive them has to be
// stored here.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// AuditLogService reads the activity feeds of the connected organisations.
//
// The feed of one organisation is read once, not once per connected
// administrator account: the tokens of any administrator of a tenant/domain can
// read the whole organisation feed, so several accounts of the same tenant would
// only duplicate the same events. That is what the organisation grouping below
// is for, and it is also why the cursor key is (provider, organisation, stream)
// rather than the account.
type AuditLogService struct {
	store  *Store
	tokens *TokenManager
	now    func() time.Time
}

// NewAuditLogService builds the collector.
func NewAuditLogService(store *Store, tokens *TokenManager) *AuditLogService {
	return &AuditLogService{
		store:  store,
		tokens: tokens,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// AuditLogCollectResult reports what one pass did. It is returned (never
// swallowed) so the maintenance endpoint can log the pass and the API can show
// it.
type AuditLogCollectResult struct {
	// Organisations is how many (provider, organisation) groups were read.
	Organisations int `json:"organisations"`
	// Streams is how many feeds were attempted, Failed how many of them
	// answered an error.
	Streams int `json:"streams"`
	Failed  int `json:"failed"`
	// Events is how many normalised events were handed to the store (ingestion
	// is idempotent, so a re-read of an overlapping window adds nothing).
	Events int `json:"events"`
	// Duration is how long the pass took.
	Duration time.Duration `json:"-"`
	// DurationMS is Duration in milliseconds, for the JSON answer.
	DurationMS int64 `json:"duration_ms"`
	// Errors carries one human readable line per failed feed, which is what the
	// operator sees when a consent is missing.
	Errors []string `json:"errors,omitempty"`
}

// Collect reads every feed of every connected organisation once. A failing feed
// is recorded on its own cursor and never stops the pass: one tenant waiting for
// a consent must not hold back the others.
func (s *AuditLogService) Collect(ctx context.Context) AuditLogCollectResult {
	started := s.now()
	result := AuditLogCollectResult{}
	groups, err := s.organisations(ctx)
	if err != nil {
		slog.Error("listing the organisations of the activity feed failed", "error", err)
		result.Errors = append(result.Errors, err.Error())
		result.Duration = s.now().Sub(started)
		result.DurationMS = result.Duration.Milliseconds()
		return result
	}
	for _, group := range groups {
		if ctx.Err() != nil {
			break
		}
		result.Organisations++
		s.collectGroup(ctx, group, &result)
	}
	result.Duration = s.now().Sub(started)
	result.DurationMS = result.Duration.Milliseconds()
	if result.Events > 0 || result.Failed > 0 {
		slog.Info("activity feed collected",
			"organisations", result.Organisations,
			"streams", result.Streams,
			"events", result.Events,
			"failed", result.Failed,
			"duration_ms", result.DurationMS)
	} else {
		slog.Debug("activity feed collected nothing new",
			"organisations", result.Organisations,
			"streams", result.Streams,
			"duration_ms", result.DurationMS)
	}
	return result
}

// auditLogOrganisation is one organisation to read, with the account whose
// tokens read it. The account is the first healthy one of the group: any
// administrator of the organisation can read its whole feed.
type auditLogOrganisation struct {
	provider   models.ProviderName
	orgID      string
	accountIDs []uint
	accountID  uint
	email      string
}

// organisations groups the connected accounts by (provider, organisation) and
// picks the account that reads each feed.
//
// The organisation comes from the account row, which the connection filled from
// the provider (the Entra tenant, the Workspace hosted domain). A row connected
// before that column existed has it empty, so the provider is asked for the
// organisation of its token instead (Microsoft reads the `tid` claim without a
// network call). An account that still cannot name an organisation is skipped:
// there is no feed to read, and guessing one from an e-mail domain would read a
// stranger's tenant.
func (s *AuditLogService) organisations(ctx context.Context) ([]auditLogOrganisation, error) {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	index := map[string]*auditLogOrganisation{}
	order := []string{}
	for i := range accounts {
		account := accounts[i]
		provider := models.ProviderName(account.Provider)
		if !provider.Valid() {
			continue
		}
		orgID := strings.ToLower(strings.TrimSpace(account.OrgID))
		if orgID == "" {
			orgID = strings.ToLower(strings.TrimSpace(s.tokens.OrgID(&account)))
		}
		if orgID == "" {
			continue
		}
		key := string(provider) + "\x00" + orgID
		group, exists := index[key]
		if !exists {
			group = &auditLogOrganisation{provider: provider, orgID: orgID}
			index[key] = group
			order = append(order, key)
		}
		group.accountIDs = append(group.accountIDs, account.ID)
		if models.AccountStatus(account.Status) == models.AccountConnected && group.accountID == 0 {
			group.accountID = account.ID
			group.email = account.Email
		}
	}
	groups := make([]auditLogOrganisation, 0, len(order))
	for _, key := range order {
		group := index[key]
		if group.accountID == 0 {
			// Every account of the organisation needs attention: the feed is
			// unreadable until one of them is connected again.
			slog.Debug("skipping an organisation whose accounts need attention",
				"provider", group.provider, "org_id", group.orgID, "accounts", len(group.accountIDs))
			continue
		}
		groups = append(groups, *group)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].provider != groups[j].provider {
			return groups[i].provider < groups[j].provider
		}
		return groups[i].orgID < groups[j].orgID
	})
	return groups, nil
}

// collectGroup reads every stream of one organisation.
func (s *AuditLogService) collectGroup(ctx context.Context, group auditLogOrganisation, result *AuditLogCollectResult) {
	session, err := s.tokens.Resolve(ctx, group.accountID)
	if err != nil {
		result.Failed++
		result.Errors = append(result.Errors, fmt.Sprintf("%s/%s: %v", group.provider, group.orgID, err))
		return
	}
	source, ok := session.Provider.(providers.ActivityLogSource)
	if !ok {
		// A provider without a feed is not a failure: it simply has nothing to
		// contribute to the screen.
		return
	}
	for _, stream := range source.ActivityLogStreams() {
		if ctx.Err() != nil {
			return
		}
		result.Streams++
		events, err := s.collectStream(ctx, group, source, session, stream)
		result.Events += events
		if err == nil {
			continue
		}
		result.Failed++
		result.Errors = append(result.Errors, fmt.Sprintf("%s/%s/%s: %v",
			group.provider, group.orgID, stream, err))
		slog.Warn("reading an activity feed failed",
			"provider", group.provider,
			"org_id", group.orgID,
			"stream", stream,
			"account", group.email,
			"error", err)
		if errors.Is(err, providers.ErrRateLimited) {
			// The organisation is being throttled: stop asking for its other
			// feeds this pass instead of burning through the whole window.
			return
		}
	}
}

// collectStream reads one feed, stores what it read and advances its cursor. It
// answers how many events it handed to the store.
func (s *AuditLogService) collectStream(ctx context.Context, group auditLogOrganisation, source providers.ActivityLogSource, session ProviderSession, stream string) (int, error) {
	cursor, err := s.store.AuditLogCursorFor(ctx, string(group.provider), group.orgID, stream)
	if err != nil {
		return 0, err
	}
	events, next, err := source.ActivityLogs(ctx, session.Credentials, session.Tokens, stream, cursor.Cursor)
	if err != nil {
		s.recordFailure(ctx, &cursor, err)
		return 0, err
	}

	rows := make([]models.AuditLogEvent, 0, len(events))
	for _, event := range events {
		row, ok := auditLogRow(group, stream, event)
		if !ok {
			continue
		}
		rows = append(rows, row)
	}
	if err := s.store.InsertAuditLogEvents(ctx, rows); err != nil {
		s.recordFailure(ctx, &cursor, err)
		return 0, err
	}

	// The cursor only moves after the events are stored: a failure above keeps
	// the previous position, so the next pass reads the same window again.
	if next != "" {
		cursor.Cursor = next
	}
	for _, event := range events {
		if event.OccurredAt.IsZero() {
			continue
		}
		occurred := event.OccurredAt.UTC()
		if cursor.LastEventAt == nil || occurred.After(*cursor.LastEventAt) {
			cursor.LastEventAt = &occurred
		}
	}
	cursor.LastError = ""
	cursor.LastRunAt = timePointer(s.now())
	if err := s.store.SaveAuditLogCursor(ctx, &cursor); err != nil {
		return len(rows), err
	}
	return len(rows), nil
}

// recordFailure stores why a feed could not be read, so the screen explains a
// stale list instead of showing a silently short one.
func (s *AuditLogService) recordFailure(ctx context.Context, cursor *models.AuditLogCursor, cause error) {
	cursor.LastError = cause.Error()
	cursor.LastRunAt = timePointer(s.now())
	if err := s.store.SaveAuditLogCursor(ctx, cursor); err != nil {
		slog.Warn("recording the failure of an activity feed failed",
			"provider", cursor.Provider, "org_id", cursor.OrgID, "stream", cursor.Stream, "error", err)
	}
}

// auditLogRow turns one normalised event into the row the store keeps. It answers
// false for a record without an identity, which is the one event that could not
// be stored idempotently.
func auditLogRow(group auditLogOrganisation, stream string, event providers.ActivityEvent) (models.AuditLogEvent, bool) {
	remoteID := strings.TrimSpace(event.RemoteID)
	if remoteID == "" {
		return models.AuditLogEvent{}, false
	}
	source := strings.TrimSpace(event.Source)
	if source == "" {
		source = stream
	}
	row := models.AuditLogEvent{
		Provider:   string(group.provider),
		Source:     source,
		RemoteID:   truncate(remoteID, 191),
		OrgID:      group.orgID,
		Action:     string(event.Action),
		Target:     string(event.Target),
		ActorEmail: truncate(strings.ToLower(strings.TrimSpace(event.ActorEmail)), 191),
		ActorName:  truncate(strings.TrimSpace(event.ActorName), 191),
		ActorID:    truncate(strings.TrimSpace(event.ActorID), 191),
		TargetName: truncate(strings.TrimSpace(event.TargetName), 512),
		TargetID:   truncate(strings.TrimSpace(event.TargetID), 191),
		DriveID:    truncate(strings.TrimSpace(event.DriveID), 191),
		SiteID:     truncate(strings.TrimSpace(event.SiteID), 191),
		OccurredAt: event.OccurredAt.UTC(),
	}
	if row.OccurredAt.IsZero() {
		return models.AuditLogEvent{}, false
	}
	if len(event.Raw) > 0 && json.Valid(event.Raw) {
		row.Raw = string(event.Raw)
	}
	return row, true
}

// truncate shortens a value to the width of its column, so a vendor field that is
// longer than the schema cannot fail the batch it travels in.
func truncate(value string, width int) string {
	if len(value) <= width {
		return value
	}
	return value[:width]
}

// timePointer returns a pointer to a copy of value, which is the shape the
// nullable columns of the cursor use.
func timePointer(value time.Time) *time.Time {
	return &value
}

// AuditLogMaxPage is the largest page the API answers (the store clamps to the
// same value), and AuditLogExportLimit the number of events one CSV export may
// carry, which is what keeps a single request bounded.
const (
	AuditLogMaxPage     = 500
	AuditLogExportLimit = 5000
)

// AuditLogCursorView is one feed position as the screen reads it: where the feed
// stands, when it was last read and whether it is healthy. It is a view and not
// the row so the SPA never has to interpret the opaque cursor string itself.
type AuditLogCursorView struct {
	Provider string `json:"provider"`
	OrgID    string `json:"org_id"`
	Stream   string `json:"stream"`
	// Cursor is the opaque resume token, shown only as evidence of progress.
	Cursor string `json:"cursor,omitempty"`
	// LastEventAt is when the newest stored event of the feed happened.
	LastEventAt *time.Time `json:"last_event_at,omitempty"`
	// LastRunAt is when the collector last advanced this feed.
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
	// Healthy is true when the last pass read the feed without an error.
	Healthy bool `json:"healthy"`
	// NeedsConsent is true when the failure is a missing grant, which is the one
	// error an operator can fix from the provider console (the SPA words it as
	// "grant the permission", see docs/audit-logs.md).
	NeedsConsent bool   `json:"needs_consent"`
	LastError    string `json:"last_error,omitempty"`
}

// NewAuditLogCursorViews converts the stored cursors.
func NewAuditLogCursorViews(rows []models.AuditLogCursor) []AuditLogCursorView {
	views := make([]AuditLogCursorView, 0, len(rows))
	for _, row := range rows {
		views = append(views, AuditLogCursorView{
			Provider:     row.Provider,
			OrgID:        row.OrgID,
			Stream:       row.Stream,
			Cursor:       row.Cursor,
			LastEventAt:  row.LastEventAt,
			LastRunAt:    row.LastRunAt,
			Healthy:      strings.TrimSpace(row.LastError) == "",
			NeedsConsent: needsConsent(row.LastError),
			LastError:    row.LastError,
		})
	}
	return views
}

// needsConsent reports whether a stored failure is a missing grant: the vendors
// spell it as a consent, a permission name or a 401/403 on the feed's endpoint.
func needsConsent(lastError string) bool {
	message := strings.ToLower(lastError)
	for _, marker := range []string{
		"consent", "activityfeed.read", "auditlog.read.all", "admin.reports.audit.readonly",
		"insufficient", "unauthorized", "forbidden", "http 401", "http 403",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
