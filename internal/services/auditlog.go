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
	"sync"
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

	// tenants caches the display name of every organisation under
	// AuditLogFeedKey, so a pass that fails on four streams of one tenant names
	// it once instead of calling the provider four times (and so the API can
	// name a feed without a provider call of its own). See tenantName.
	tenantMu sync.Mutex
	tenants  map[string]auditLogTenant
}

// NewAuditLogService builds the collector.
func NewAuditLogService(store *Store, tokens *TokenManager) *AuditLogService {
	return &AuditLogService{
		store:   store,
		tokens:  tokens,
		now:     func() time.Time { return time.Now().UTC() },
		tenants: map[string]auditLogTenant{},
	}
}

// AuditLogPolicy is how the operator configured the collector: whether the
// scheduled pass runs at all, how often it runs, and how long it waits between
// the provider calls of one pass.
//
// The pause is the answer to a throttled tenant: `directoryAuditLogs` and the
// management activity API count requests per tenant, so a pass that reads four
// streams back to back can spend the limit before the second one and answer 429
// even though the retry policy would have absorbed a single refusal.
type AuditLogPolicy struct {
	// Enabled arms the scheduled pass. A forced pass (the Collect button) runs
	// either way, which is what the switch promises.
	Enabled bool
	// Interval is the minimum age of the previous pass before the next one runs.
	Interval time.Duration
	// RequestDelay is the pause between two provider calls of the same pass.
	RequestDelay time.Duration
}

// Policy reads the three capture keys, clamping what a hand-edited database could
// have left behind. It is read on every pass instead of being cached, so a save
// in Admin > Settings applies to the next tick (the purge reads its policy the
// same way).
func (s *AuditLogService) Policy(ctx context.Context) AuditLogPolicy {
	settings := s.store.Settings()
	return AuditLogPolicy{
		Enabled: settings.Bool(ctx, models.SettingAuditLogsEnabled, true),
		Interval: time.Duration(clampAuditLogInterval(
			settings.Int(ctx, models.SettingAuditLogsIntervalMinutes, DefaultAuditLogIntervalMinutes))) * time.Minute,
		RequestDelay: time.Duration(clampAuditLogDelay(
			settings.Int(ctx, models.SettingAuditLogsRequestDelaySeconds, DefaultAuditLogRequestDelaySeconds))) * time.Second,
	}
}

// auditLogPace is the pacing state of one pass: the pause to wait before every
// provider call but the first. It lives on the stack of Collect, so the loop and
// a forced collect running at once never share it.
type auditLogPace struct {
	delay  time.Duration
	called bool
}

// wait blocks for the configured pause before the next provider call and answers
// false when the pass was cancelled while waiting. The first call of a pass never
// waits: the pause is about the calls that follow one another.
func (p *auditLogPace) wait(ctx context.Context) bool {
	if !p.called {
		p.called = true
		return true
	}
	if p.delay <= 0 {
		return true
	}
	timer := time.NewTimer(p.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
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
	// Repaired is how many stored events the repair pass re-attributed during
	// this pass, and PendingRepairs how many still wait (a history stored by a
	// release that predates the actor kind is repaired a batch at a time).
	Repaired       int   `json:"repaired"`
	PendingRepairs int64 `json:"pending_repairs"`
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
	policy := s.Policy(ctx)
	pace := &auditLogPace{delay: policy.RequestDelay}
	for _, group := range groups {
		if ctx.Err() != nil {
			break
		}
		result.Organisations++
		s.collectGroup(ctx, group, &result, pace)
	}
	// The vendor documents the collector keeps make an upgrade self-repairing:
	// the history a previous release mapped with a single actor slot is re-read
	// here instead of staying wrong until somebody asks (see RepairActors).
	repair := s.RepairActors(ctx)
	result.Repaired = repair.Updated
	result.PendingRepairs = repair.Remaining
	result.Duration = s.now().Sub(started)
	result.DurationMS = result.Duration.Milliseconds()
	if result.Events > 0 || result.Failed > 0 || result.Repaired > 0 {
		slog.Info("activity feed collected",
			"organisations", result.Organisations,
			"streams", result.Streams,
			"events", result.Events,
			"failed", result.Failed,
			"repaired", result.Repaired,
			"pending_repairs", result.PendingRepairs,
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

// collectGroup reads every stream of one organisation, waiting the configured
// pause between the provider calls it makes.
func (s *AuditLogService) collectGroup(ctx context.Context, group auditLogOrganisation, result *AuditLogCollectResult, pace *auditLogPace) {
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
		// The pause that keeps a busy tenant from answering 429 to the next
		// stream of the same pass; a cancelled pass stops here.
		if !pace.wait(ctx) {
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
		s.recordFailure(ctx, &cursor, s.failureMessage(ctx, group, session, err))
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
		s.recordFailure(ctx, &cursor, s.failureMessage(ctx, group, session, err))
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

// AuditLogRepairBatch is how many stored events one repair batch re-reads,
// AuditLogRepairMaxBatches bounds the batches of one pass and AuditLogRepairBudget
// the wall clock it may spend: the repair reads the vendor document (cheap) but may
// also ask Graph for the owner of a device (slow), and a pass that runs inside the
// ten minute collect tick must never run longer than the tick.
const (
	AuditLogRepairBatch      = 500
	AuditLogRepairMaxBatches = 20
	AuditLogRepairBudget     = 2 * time.Minute
)

// AuditLogRepairResult reports what the actor repair pass did. It is returned
// (never swallowed) so the maintenance endpoint can log it and the API can answer
// it, the same way AuditLogCollectResult works.
type AuditLogRepairResult struct {
	// Scanned is how many stored events were re-read.
	Scanned int `json:"scanned"`
	// Updated is how many rows had their actor rewritten.
	Updated int `json:"updated"`
	// Attributed is how many rows were given the person behind a workload.
	Attributed int `json:"attributed"`
	// Remaining is how many rows still await a repair (0 once the history has
	// been walked).
	Remaining int64 `json:"remaining"`
	// Duration is how long the pass took.
	Duration   time.Duration `json:"-"`
	DurationMS int64         `json:"duration_ms"`
}

// RepairActors gives the stored events of the feeds the actor attribution the
// current normaliser produces: it re-reads the vendor document the collector kept
// in Raw, so a release that learned "who is really behind this change" repairs the
// history instead of leaving it wrong forever. It runs at the end of every collect
// pass and on demand from POST /api/maintenance/auditlogs/renormalise.
//
// The pass is idempotent and convergent: it only looks at rows without an actor
// kind and every visited row leaves with one, so a failure part-way through costs
// nothing but the next pass.
func (s *AuditLogService) RepairActors(ctx context.Context) AuditLogRepairResult {
	started := s.now()
	result := AuditLogRepairResult{}
	defer func() {
		result.Duration = s.now().Sub(started)
		result.DurationMS = result.Duration.Milliseconds()
	}()
	pending, err := s.store.CountPendingAuditLogActors(ctx)
	if err != nil {
		slog.Error("counting the activity events to repair failed", "error", err)
		return result
	}
	result.Remaining = pending
	if pending == 0 {
		return result
	}
	groups, err := s.organisations(ctx)
	if err != nil {
		// Without an account there is no token, so no provider and no
		// normaliser: the rows stay as they are, which is what the previous
		// release stored.
		slog.Error("listing the organisations to repair the activity feed failed", "error", err)
		return result
	}
	index := map[string]auditLogOrganisation{}
	for _, group := range groups {
		index[string(group.provider)+"\x00"+group.orgID] = group
	}
	for batch := 0; batch < AuditLogRepairMaxBatches; batch++ {
		if ctx.Err() != nil || s.now().Sub(started) > AuditLogRepairBudget {
			// The pass is bounded by the collect tick, not by the size of the
			// history: what is left is repaired by the next one.
			break
		}
		rows, err := s.store.PendingAuditLogActors(ctx, AuditLogRepairBatch)
		if err != nil {
			slog.Error("reading the activity events to repair failed", "error", err)
			break
		}
		if len(rows) == 0 {
			break
		}
		result.Scanned += len(rows)
		if updated := s.repairBatch(ctx, rows, index, &result); updated == 0 {
			// No row of the batch could be written: the next batch would read
			// the same ones again.
			break
		}
	}
	if remaining, err := s.store.CountPendingAuditLogActors(ctx); err == nil {
		result.Remaining = remaining
	}
	if result.Updated > 0 {
		slog.Info("activity log actors repaired",
			"scanned", result.Scanned,
			"updated", result.Updated,
			"attributed", result.Attributed,
			"remaining", result.Remaining,
			"duration_ms", result.DurationMS)
	}
	return result
}

// repairBatch repairs the rows of one batch, organisation by organisation so one
// token serves every row of a tenant, and answers how many rows it wrote.
func (s *AuditLogService) repairBatch(ctx context.Context, rows []models.AuditLogEvent, index map[string]auditLogOrganisation, result *AuditLogRepairResult) int {
	// Group the rows under the organisation feed they belong to, keeping the
	// order of the batch so the oldest events are repaired first.
	order := []string{}
	buckets := map[string][]models.AuditLogEvent{}
	for _, row := range rows {
		key := row.Provider + "\x00" + row.OrgID
		if _, known := buckets[key]; !known {
			order = append(order, key)
		}
		buckets[key] = append(buckets[key], row)
	}
	updated := 0
	for _, key := range order {
		if ctx.Err() != nil {
			break
		}
		updated += s.repairRows(ctx, buckets[key], index[key], result)
	}
	return updated
}

// repairRows repairs the rows of one organisation feed: it re-reads their vendor
// documents, gives the workload-made device changes the person behind them, and
// writes back only the actor columns.
func (s *AuditLogService) repairRows(ctx context.Context, rows []models.AuditLogEvent, group auditLogOrganisation, result *AuditLogRepairResult) int {
	var session ProviderSession
	var normaliser providers.ActivityLogNormaliser
	var enricher providers.ActivityActorEnricher
	if group.accountID != 0 {
		resolved, err := s.tokens.Resolve(ctx, group.accountID)
		if err != nil {
			// The organisation cannot be read right now (an account that needs
			// attention): its rows are repaired from their own columns only.
			slog.Debug("repairing the activity feed without a token",
				"provider", group.provider, "org_id", group.orgID, "error", err)
		} else {
			session = resolved
			normaliser, _ = session.Provider.(providers.ActivityLogNormaliser)
			enricher, _ = session.Provider.(providers.ActivityActorEnricher)
		}
	}

	// Re-read every vendor document first: the enrichment works on the events of
	// the batch, so the lookups of one device are made once.
	entries := make([]auditLogEventRepair, len(rows))
	for i, row := range rows {
		entry := auditLogEventRepair{row: row}
		if normaliser != nil && row.Raw != "" {
			if event, ok := normaliser.NormaliseActivityEvent(row.Source, json.RawMessage(row.Raw)); ok {
				entry.event, entry.read = event, true
			}
		}
		entries[i] = entry
	}
	if enricher != nil {
		events := make([]providers.ActivityEvent, 0, len(entries))
		positions := make([]int, 0, len(entries))
		for i := range entries {
			if !entries[i].read {
				continue
			}
			events = append(events, entries[i].event)
			positions = append(positions, i)
		}
		if attributed := enricher.EnrichActivityActors(ctx, session.Tokens, events); attributed > 0 {
			for j := range events {
				entries[positions[j]].event = events[j]
			}
			result.Attributed += attributed
		}
	}

	updated := 0
	for i := range entries {
		entry := entries[i]
		actor := auditLogActor(entry.actorEvent())
		target := entry.target()
		if actor.Kind == entry.row.ActorKind && actor.Email == entry.row.ActorEmail &&
			actor.Name == entry.row.ActorName && actor.ID == entry.row.ActorID &&
			actor.OnBehalfOf == entry.row.ActorOnBehalfOf &&
			actor.OnBehalfOfName == entry.row.ActorOnBehalfOfName &&
			(target == "" || target == entry.row.Target) {
			continue
		}
		repair := AuditLogRepair{Actor: actor, Target: target}
		if err := s.store.RepairAuditLogEvent(ctx, entry.row.ID, repair); err != nil {
			slog.Warn("repairing the actor of an activity event failed",
				"event_id", entry.row.ID, "error", err)
			continue
		}
		updated++
		result.Updated++
	}
	return updated
}

// auditLogEventRepair is one row of a repair batch with the event its stored
// vendor document was read back into (read is false when the document is missing
// or unreadable, which is repaired from the columns the row already carries).
type auditLogEventRepair struct {
	row   models.AuditLogEvent
	event providers.ActivityEvent
	read  bool
}

// actorEvent is the event the actor of the row is repaired from.
func (r auditLogEventRepair) actorEvent() providers.ActivityEvent {
	if r.read {
		return r.event
	}
	return providers.ActivityEvent{
		ActorEmail: r.row.ActorEmail,
		ActorName:  r.row.ActorName,
		ActorID:    r.row.ActorID,
	}
}

// target is the normalised object of the row, empty when the vendor document could
// not be re-read and the stored classification has to stand.
func (r auditLogEventRepair) target() string {
	if !r.read || r.event.Target == "" {
		return ""
	}
	return string(r.event.Target)
}

// recordFailure stores why a feed could not be read, so the screen explains a
// stale list instead of showing a silently short one.
func (s *AuditLogService) recordFailure(ctx context.Context, cursor *models.AuditLogCursor, message string) {
	cursor.LastError = message
	cursor.LastRunAt = timePointer(s.now())
	if err := s.store.SaveAuditLogCursor(ctx, cursor); err != nil {
		slog.Warn("recording the failure of an activity feed failed",
			"provider", cursor.Provider, "org_id", cursor.OrgID, "stream", cursor.Stream, "error", err)
	}
}

// Bounds of the organisation-name cache. A tenant is renamed rarely, so a long
// hit window keeps the label without a provider call per pass; a failed lookup is
// retried sooner, because the usual cause — a scope that was never granted — is
// fixed by an administrator and must show up without a restart. The lookup itself
// is bounded because it runs inside a collect pass.
const (
	auditLogTenantTTL      = 6 * time.Hour
	auditLogTenantRetryTTL = 10 * time.Minute
	auditLogTenantTimeout  = 10 * time.Second
)

// auditLogTenant is one cached answer of the organisation-name lookup. An empty
// name is a cached failure: it is remembered too, so a provider that refuses the
// call is not asked again on every stream of the same pass.
type auditLogTenant struct {
	name    string
	expires time.Time
}

// AuditLogFeedKey is the key a feed of one organisation is cached and indexed
// under: the provider and the organisation id, the same pair the cursors and the
// collector group by. It is exported so the API can join a resolved name onto a
// cursor without rebuilding the key by hand.
func AuditLogFeedKey(provider, orgID string) string {
	return provider + "\x00" + orgID
}

// failureMessage turns the error of a feed into the line the operator reads in the
// banner: which tenant it is, which account read it, and only then what failed.
//
// The order is the fix. A stored failure used to be the provider's message alone,
// which for an ungranted Office 365 management API names no directory at all (see
// microsoft.activityFeedConsentHint) — so an operator with several tenants could
// not tell which one needed the grant. The cause keeps its own wording; only the
// duplicated provider prefix is dropped, because the label below already carries
// it.
func (s *AuditLogService) failureMessage(ctx context.Context, group auditLogOrganisation, session ProviderSession, cause error) string {
	message := strings.TrimSpace(cause.Error())
	if prefix := string(group.provider) + ": "; strings.HasPrefix(message, prefix) {
		message = strings.TrimPrefix(message, prefix)
	}
	return fmt.Sprintf("%s: %s: %s", group.provider, s.feedLabel(ctx, group, session), message)
}

// feedLabel names one organisation feed: `tenant "Contoso Ltd" (059c3987-…), read
// by admin@contoso.com`, or the id alone when the provider cannot name it. The id
// is always present (it is what the operator checks in the provider console); the
// name is what makes it recognisable; the account is what makes "grant it"
// actionable.
func (s *AuditLogService) feedLabel(ctx context.Context, group auditLogOrganisation, session ProviderSession) string {
	tenant := "tenant " + group.orgID
	if name := s.tenantName(ctx, group, session); name != "" && !strings.EqualFold(name, group.orgID) {
		tenant = fmt.Sprintf("tenant %q (%s)", name, group.orgID)
	}
	if email := strings.TrimSpace(group.email); email != "" {
		return fmt.Sprintf("%s, read by %s", tenant, email)
	}
	return tenant
}

// tenantName resolves the display name of the organisation of a feed, through the
// provider's OrganisationNamer, and caches it. An organisation that cannot be
// named answers "", which every caller renders as the bare id: naming a tenant is
// an improvement, never a reason to fail a pass.
func (s *AuditLogService) tenantName(ctx context.Context, group auditLogOrganisation, session ProviderSession) string {
	key := AuditLogFeedKey(string(group.provider), group.orgID)
	if name, ok := s.cachedTenantName(key); ok {
		return name
	}
	name := ""
	if namer, ok := session.Provider.(providers.OrganisationNamer); ok {
		lookup, cancel := context.WithTimeout(ctx, auditLogTenantTimeout)
		resolved, _, err := namer.OrganisationName(lookup, session.Credentials, session.Tokens)
		cancel()
		if err != nil {
			// Debug, not warn: the label is a convenience, and the call that
			// failed is reported on the cursor by the error that caused it.
			slog.Debug("naming the organisation of an activity feed failed",
				"provider", group.provider, "org_id", group.orgID, "error", err)
		} else {
			name = strings.TrimSpace(resolved)
		}
	}
	s.rememberTenantName(key, name)
	return name
}

// cachedTenantName answers the cached label of a feed, or false when it has to be
// resolved. A failed lookup is cached as "" so it is not asked twice in one pass.
func (s *AuditLogService) cachedTenantName(key string) (string, bool) {
	s.tenantMu.Lock()
	defer s.tenantMu.Unlock()
	tenant, ok := s.tenants[key]
	if !ok || !s.now().Before(tenant.expires) {
		return "", false
	}
	return tenant.name, true
}

// rememberTenantName caches one label, with a shorter window for a failed lookup.
func (s *AuditLogService) rememberTenantName(key, name string) {
	ttl := auditLogTenantTTL
	if name == "" {
		ttl = auditLogTenantRetryTTL
	}
	s.tenantMu.Lock()
	defer s.tenantMu.Unlock()
	if s.tenants == nil {
		s.tenants = map[string]auditLogTenant{}
	}
	s.tenants[key] = auditLogTenant{name: name, expires: s.now().Add(ttl)}
}

// OrganisationNames answers the display name of every organisation the collector
// has resolved so far, under AuditLogFeedKey. The API joins it onto the cursors so
// the screen names a feed ("Contoso Ltd") instead of showing a bare tenant id; an
// organisation that was never resolved is simply absent, and the screen falls back
// to the id and to the accounts that read the feed.
func (s *AuditLogService) OrganisationNames() map[string]string {
	s.tenantMu.Lock()
	defer s.tenantMu.Unlock()
	names := make(map[string]string, len(s.tenants))
	for key, tenant := range s.tenants {
		if tenant.name == "" {
			continue
		}
		names[key] = tenant.name
	}
	return names
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
	actor := auditLogActor(event)
	row := models.AuditLogEvent{
		Provider:            string(group.provider),
		Source:              source,
		RemoteID:            truncate(remoteID, 191),
		OrgID:               group.orgID,
		Action:              string(event.Action),
		Target:              string(event.Target),
		ActorEmail:          actor.Email,
		ActorName:           actor.Name,
		ActorID:             actor.ID,
		ActorKind:           actor.Kind,
		ActorOnBehalfOf:     actor.OnBehalfOf,
		ActorOnBehalfOfName: actor.OnBehalfOfName,
		TargetName:          truncate(strings.TrimSpace(event.TargetName), 512),
		TargetID:            truncate(strings.TrimSpace(event.TargetID), 191),
		DriveID:             truncate(strings.TrimSpace(event.DriveID), 191),
		SiteID:              truncate(strings.TrimSpace(event.SiteID), 191),
		OccurredAt:          event.OccurredAt.UTC(),
	}
	if row.OccurredAt.IsZero() {
		return models.AuditLogEvent{}, false
	}
	if len(event.Raw) > 0 && json.Valid(event.Raw) {
		row.Raw = string(event.Raw)
	}
	return row, true
}

// auditLogActorKind is the nature of the principal behind an event. A provider
// that names one keeps it; a feed that only names the actor (Google reports a
// user address and nothing else) is read from what it did store: an address is a
// person, a bare name is a workload acting for somebody, and neither is an event
// the vendor attributed to no principal at all.
//
// Every stored row therefore carries a kind, which is what makes the repair pass
// converge: a row it has visited stops being a candidate.
func auditLogActorKind(event providers.ActivityEvent) models.AuditLogActorKind {
	if event.ActorKind.Valid() {
		return event.ActorKind
	}
	switch {
	case looksLikeAddress(event.ActorEmail):
		return models.AuditLogActorUser
	case strings.TrimSpace(event.ActorEmail) != "" || strings.TrimSpace(event.ActorName) != "":
		// A value that is not an address can only be the name of a workload:
		// the releases before the actor kind put a service principal name in
		// the address column, which is what this reads back.
		return models.AuditLogActorApplication
	default:
		return models.AuditLogActorSystem
	}
}

// looksLikeAddress reports whether a value is an e-mail address. The actor column
// of a row stored before the actor kind existed can hold a service principal name
// instead, and the two must never be confused: attribution that guesses is worse
// than attribution that admits it does not know.
func looksLikeAddress(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " \t\n") {
		return false
	}
	at := strings.IndexByte(value, '@')
	return at > 0 && at < len(value)-1
}

// auditLogActor normalises the actor of a provider event into the columns of a
// stored row, the one way the collector and the repair pass both build it, so a
// repaired row and a freshly collected one cannot disagree.
//
// The address is kept only for a person: a workload has a name, and letting a
// service principal name sit in the e-mail column is exactly the defect this
// change removes (see plan 2026-11-07-audit-log-actor-attribution.md).
func auditLogActor(event providers.ActivityEvent) AuditLogActor {
	kind := auditLogActorKind(event)
	actor := AuditLogActor{
		Kind:           string(kind),
		Name:           truncate(strings.TrimSpace(event.ActorName), 191),
		ID:             truncate(strings.TrimSpace(event.ActorID), 191),
		OnBehalfOf:     truncate(strings.ToLower(strings.TrimSpace(event.ActorOnBehalfOf)), 191),
		OnBehalfOfName: truncate(strings.TrimSpace(event.ActorOnBehalfOfName), 191),
	}
	if kind == models.AuditLogActorUser {
		actor.Email = truncate(strings.ToLower(strings.TrimSpace(event.ActorEmail)), 191)
	} else if actor.Name == "" {
		// The address column of a row stored by an older release can hold the
		// name of the workload: move it where it belongs instead of dropping it.
		actor.Name = truncate(strings.TrimSpace(event.ActorEmail), 191)
	}
	return actor
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
	// OrgName is the display name of the organisation (the Entra tenant, the
	// Workspace customer) when the collector has resolved one. It is what makes
	// a failure identifiable: the same tenant as a name and as an id. Empty
	// when the provider could not name it, which the screen renders as the id.
	OrgName string `json:"org_name,omitempty"`
	Stream  string `json:"stream"`
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

// NewAuditLogCursorViews converts the stored cursors, joining the display name of
// the organisation each feed belongs to (see AuditLogService.OrganisationNames;
// a nil map is fine and simply leaves every OrgName empty).
func NewAuditLogCursorViews(rows []models.AuditLogCursor, orgNames map[string]string) []AuditLogCursorView {
	views := make([]AuditLogCursorView, 0, len(rows))
	for _, row := range rows {
		views = append(views, AuditLogCursorView{
			Provider:     row.Provider,
			OrgID:        row.OrgID,
			OrgName:      orgNames[AuditLogFeedKey(row.Provider, row.OrgID)],
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
		"af10001", "insufficient", "unauthorized", "forbidden", "http 401", "http 403",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
