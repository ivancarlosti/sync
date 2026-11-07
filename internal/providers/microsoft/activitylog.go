package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// The activity-log streams of the Microsoft provider. Microsoft does not expose
// one feed: Graph answers the *directory* changes (users, groups, roles,
// policies) and the Office 365 management activity API answers the *workload*
// activity (files in SharePoint and OneDrive, plus the rest of Microsoft 365).
// They differ in authorization too — the Graph feed is read with the delegated
// token of the account, the management API is app-only — which is why they are
// two streams with two independent cursors and two independent last errors.
const (
	// streamDirectory is Graph `auditLogs/directoryAudits`: who changed which
	// directory object, when.
	streamDirectory = "directory"
	// streamUnified is the Office 365 management activity API: file and
	// workload activity, subscribed per content type.
	streamUnified = "unified"
)

// activityStreams lists the feeds ActivityLogs reads, in the order the screen
// lists them.
var activityStreams = []string{streamDirectory, streamUnified}

// auditLogScope is the delegated permission behind the directory feed. It is a
// Graph scope, so it is declared in its qualified form like every other Graph
// scope of this provider.
var auditLogScope = graphScope("AuditLog.Read.All")

const (
	// directoryAuditURL is the Graph v1.0 directory audit endpoint. It is built
	// by hand rather than through the SDK request builder for the same reason
	// the transfers are (see the package comment): the raw JSON of an audit
	// record is what the screen keeps as Raw, and a serialised SDK model would
	// drop the vendor fields the normaliser does not know about.
	directoryAuditURL = baseURL + "/auditLogs/directoryAudits"
	// directoryAuditPage is the $top of every page. Graph caps directory audits
	// at 1000; 200 keeps a page small enough to decode quickly.
	directoryAuditPage = 200
	// directoryAuditMaxPages bounds one pass so a first run over a busy tenant
	// cannot walk the whole retention window in one tick.
	directoryAuditMaxPages = 20
	// directoryAuditLookback is how far back a stream with no cursor starts.
	// Graph keeps directory audits for 30 days; a week is what a fresh install
	// shows.
	directoryAuditLookback = 7 * 24 * time.Hour
	// activityOverlap re-reads a few minutes of the previous pass, because an
	// event can be indexed slightly after the moment it carries.
	activityOverlap = 5 * time.Minute
)

// Compile time guard: the audit-log collector hands these calls to the registry
// as an interface, so a renamed method must fail the build.
var _ providers.ActivityLogSource = (*Provider)(nil)

// ActivityLogStreams implements providers.ActivityLogSource.
func (p *Provider) ActivityLogStreams() []string {
	streams := make([]string, len(activityStreams))
	copy(streams, activityStreams)
	return streams
}

// ActivityLogScopes implements providers.ActivityLogSource: only the directory
// scope is reported here. The management activity API is app-only, so it is not
// part of the delegated grant the authorization request asks for (see
// docs/app-registration.md): reporting it would mark every existing account as
// "connected before these permissions existed" for a permission a reconnect
// cannot add.
func (p *Provider) ActivityLogScopes() []string {
	return []string{auditLogScope}
}

// ActivityLogs implements providers.ActivityLogSource: it dispatches to the feed
// named by stream and answers the normalised events plus the cursor of the next
// pass.
func (p *Provider) ActivityLogs(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, stream, cursor string) ([]providers.ActivityEvent, string, error) {
	switch stream {
	case streamDirectory:
		return p.directoryAuditLogs(ctx, tokens, cursor)
	case streamUnified:
		return p.unifiedAuditLogs(ctx, creds, tokens, cursor)
	default:
		return nil, "", fmt.Errorf("microsoft graph: unknown activity stream %q", stream)
	}
}

// NormaliseActivityEvent implements providers.ActivityLogNormaliser: it re-reads
// one stored vendor record, so the repair pass can attribute the history a
// previous release mapped with one actor slot (see plan
// 2026-11-07-audit-log-actor-attribution.md). Only the streams whose normaliser
// this provider owns are re-read; a record of any other feed answers false and
// keeps the row it has.
func (p *Provider) NormaliseActivityEvent(source string, raw json.RawMessage) (providers.ActivityEvent, bool) {
	if strings.ToLower(strings.TrimSpace(source)) != streamDirectory {
		return providers.ActivityEvent{}, false
	}
	return normaliseDirectoryAudit(raw)
}

// formatWatermark renders a cursor. Seconds are enough for both feeds, and a
// value without sub-second noise is stable between passes and readable in SQL.

func formatWatermark(moment time.Time) string {
	return moment.UTC().Format(time.RFC3339)
}

// parseWatermark reads a cursor, answering false for an empty or unreadable
// value (which makes the stream start from the default window).
func parseWatermark(cursor string) (time.Time, bool) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

// getJSON issues a bearer-authenticated GET and decodes the answer into out. The
// caller bounds the context; the response body is closed here.
func (p *Provider) getJSON(ctx context.Context, target string, token string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("microsoft graph: cannot build the request for %s: %w", safeURL(target), err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")

	response, err := p.doRequest(ctx, request)
	if err != nil {
		return fmt.Errorf("microsoft graph: %s failed: %w", safeURL(target), err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return decodeGraphError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(out); err != nil {
		if err == io.EOF {
			return nil
		}
		return fmt.Errorf("microsoft graph: cannot decode the answer of %s: %w", safeURL(target), err)
	}
	return nil
}

// activityRetryPolicy is the retry of the two audit feeds. It is wider than the
// default policy on purpose: both endpoints answer a throttle with a Retry-After
// of a minute or more, and the default 30s cap would retry too early and burn an
// attempt. Three attempts keep one throttled tenant from starving the other
// streams of the pass, and the whole exchange stays inside requestTimeout.
var activityRetryPolicy = providers.RetryPolicy{
	MaxAttempts: 3,
	MaxDelay:    90 * time.Second,
}

// doRequest sends one request through providers.DoWithRetry, replaying the body of
// a POST from GetBody between attempts (a consumed reader would send an empty
// form the second time). The caller still closes the body it gets back.
func (p *Provider) doRequest(ctx context.Context, request *http.Request) (*http.Response, error) {
	return providers.DoWithRetry(ctx, activityRetryPolicy, func(ctx context.Context) (*http.Response, error) {
		attempt := request.Clone(ctx)
		if request.GetBody != nil {
			replay, err := request.GetBody()
			if err != nil {
				return nil, err
			}
			attempt.Body = replay
		}
		return p.httpClient.Do(attempt)
	})
}

// safeURL trims a URL to a length that is safe to log.
func safeURL(raw string) string {
	if len(raw) > 160 {
		return raw[:160] + "..."
	}
	return raw
}

// directoryAuditRecord is one auditLogs/directoryAudits record, trimmed to the
// fields the normaliser maps. The whole document is kept as the event's Raw.
type directoryAuditRecord struct {
	ID                  string    `json:"id"`
	Result              string    `json:"result"`
	ActivityDisplayName string    `json:"activityDisplayName"`
	ActivityDateTime    time.Time `json:"activityDateTime"`
	LoggedByService     string    `json:"loggedByService"`
	OperationType       string    `json:"operationType"`
	InitiatedBy         struct {
		User struct {
			ID                string `json:"id"`
			DisplayName       string `json:"displayName"`
			UserPrincipalName string `json:"userPrincipalName"`
		} `json:"user"`
		App struct {
			AppID                string `json:"appId"`
			DisplayName          string `json:"displayName"`
			ServicePrincipalID   string `json:"servicePrincipalId"`
			ServicePrincipalName string `json:"servicePrincipalName"`
		} `json:"app"`
	} `json:"initiatedBy"`
	TargetResources []struct {
		ID                string `json:"id"`
		DisplayName       string `json:"displayName"`
		Type              string `json:"type"`
		UserPrincipalName string `json:"userPrincipalName"`
		// ModifiedProperties is where Graph reports what a change actually
		// wrote. It is the only place a device registration names the person
		// behind it (`RegisteredOwner`), because the actor of that record is
		// the "Device Registration Service" workload.
		ModifiedProperties []struct {
			DisplayName string `json:"displayName"`
			OldValue    string `json:"oldValue"`
			NewValue    string `json:"newValue"`
		} `json:"modifiedProperties"`
	} `json:"targetResources"`
	// AdditionalDetails are the key/value pairs some activities add to the
	// record ("DeviceId", "DeviceOSType", …), kept as the second place a
	// workload-made change can name the object or the person behind it.
	AdditionalDetails []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"additionalDetails"`
}

// directoryAuditLogs reads the Graph directory feed: who changed which directory
// object, when. The cursor is an RFC3339 watermark in the `$filter`, so a pass
// reads exactly what the previous one did not.
func (p *Provider) directoryAuditLogs(ctx context.Context, tokens *providers.Tokens, cursor string) ([]providers.ActivityEvent, string, error) {
	if err := requireToken(tokens); err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	start := now.Add(-directoryAuditLookback)
	if previous, ok := parseWatermark(cursor); ok {
		start = previous.Add(-activityOverlap)
	}
	if start.After(now) {
		start = now
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	events := make([]providers.ActivityEvent, 0, directoryAuditPage)
	newest := start
	target := directoryAuditTarget(start)
	for page := 0; page < directoryAuditMaxPages; page++ {
		var answer struct {
			Value    []json.RawMessage `json:"value"`
			NextLink string            `json:"@odata.nextLink"`
		}
		if err := p.getJSON(ctx, target, tokens.AccessToken, &answer); err != nil {
			return events, "", err
		}
		for _, raw := range answer.Value {
			event, ok := normaliseDirectoryAudit(raw)
			if !ok {
				// One unreadable record must not throw the page away: the feed
				// is append-only, so the next pass would only hit it again.
				continue
			}
			events = append(events, event)
			if event.OccurredAt.After(newest) {
				newest = event.OccurredAt
			}
		}
		if answer.NextLink == "" {
			// The page is complete: give the workload-made device changes the
			// person behind them before the collector stores them (see
			// deviceowners.go). It is bounded and never fails the pass.
			p.EnrichActivityActors(ctx, tokens, events)
			return events, formatWatermark(now), nil
		}
		// A page link carries the access token: never follow one that leaves
		// Graph, or a spoofed answer could steal it.
		if !strings.HasPrefix(answer.NextLink, baseURL+"/") {
			return events, "", fmt.Errorf("microsoft graph: a directory audit page link left Graph (%s)", safeURL(answer.NextLink))
		}
		target = answer.NextLink
	}
	// The page cap was reached: resume from the newest record read instead of
	// claiming the whole window was consumed.
	p.EnrichActivityActors(ctx, tokens, events)
	return events, formatWatermark(newest), nil
}

// directoryAuditTarget builds the first page of a pass: the incremental window
// plus the page size.
func directoryAuditTarget(start time.Time) string {
	query := url.Values{}
	query.Set("$filter", "activityDateTime ge "+formatWatermark(start))
	query.Set("$top", strconv.Itoa(directoryAuditPage))
	return directoryAuditURL + "?" + query.Encode()
}

// normaliseDirectoryAudit maps one Graph record onto the shared vocabulary. The
// identity is the record id, so a re-read of an overlapping window cannot store
// the same change twice.
func normaliseDirectoryAudit(raw json.RawMessage) (providers.ActivityEvent, bool) {
	var record directoryAuditRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return providers.ActivityEvent{}, false
	}
	if strings.TrimSpace(record.ID) == "" || record.ActivityDateTime.IsZero() {
		return providers.ActivityEvent{}, false
	}
	action, target := classifyDirectoryAudit(record)
	principal := directoryPrincipalOf(record)
	event := providers.ActivityEvent{
		Source:     streamDirectory,
		RemoteID:   record.ID,
		Action:     action,
		Target:     target,
		ActorEmail: principal.email,
		ActorName:  principal.name,
		ActorID:    principal.id,
		ActorKind:  principal.kind,
		OccurredAt: record.ActivityDateTime.UTC(),
		Raw:        raw,
	}
	if principal.kind == models.AuditLogActorApplication {
		// A workload made the change; the record sometimes still says which
		// person it did it for (a device registration names the enrolling
		// owner). What it does not say stays empty — the device owner lookup
		// of deviceowners.go is the second chance, never a guess.
		event.ActorOnBehalfOf, event.ActorOnBehalfOfName = recordOwnerOnBehalfOf(record)
	}
	if index, ok := primaryTargetResource(record, target); ok {
		resource := record.TargetResources[index]
		event.TargetName = strings.TrimSpace(resource.DisplayName)
		if event.TargetName == "" {
			event.TargetName = strings.TrimSpace(resource.UserPrincipalName)
		}
		event.TargetID = strings.TrimSpace(resource.ID)
	}
	if target == models.AuditLogTargetDevice && event.TargetID == "" {
		// Some activities name the device only by its Entra device id, which is
		// what the owner lookup of deviceowners.go accepts in the same position.
		event.TargetID = recordDeviceID(record)
	}
	return event, true
}

// primaryTargetResource picks the resource the event is filed under: the one
// whose type matches the classified target, or the first one when none does.
// Graph lists a device registration as [device, user], so reading the first
// resource blindly can label a device event with a person's name.
func primaryTargetResource(record directoryAuditRecord, target models.AuditLogTarget) (int, bool) {
	if len(record.TargetResources) == 0 {
		return 0, false
	}
	for i := range record.TargetResources {
		if directoryTargetOfType(record.TargetResources[i].Type) == target {
			return i, true
		}
	}
	return 0, true
}

// directoryPrincipal is the principal behind one record: a person, a workload or
// no principal at all. It is one value instead of three accessors because the
// kind decides which column each field may travel in — an application name must
// never land in the e-mail slot (see plan
// 2026-11-07-audit-log-actor-attribution.md).
type directoryPrincipal struct {
	// kind is the AuditLogActorKind of the principal.
	kind models.AuditLogActorKind
	// email is the address of a person, empty for a workload or a system
	// event: a service principal name is not an address.
	email string
	// name is the display label: the user's name, the application display name
	// or the service that logged the event.
	name string
	// id is the stable object id of the principal.
	id string
}

// directoryPrincipalOf reads the actor of a record. Graph reports a person under
// `initiatedBy.user` and a workload under `initiatedBy.app`; a change that is
// neither (a scheduled policy, a platform job) is attributed to the service that
// logged it.
func directoryPrincipalOf(record directoryAuditRecord) directoryPrincipal {
	user := record.InitiatedBy.User
	if strings.TrimSpace(user.UserPrincipalName) != "" || strings.TrimSpace(user.ID) != "" {
		return directoryPrincipal{
			kind:  models.AuditLogActorUser,
			email: strings.ToLower(strings.TrimSpace(user.UserPrincipalName)),
			name:  strings.TrimSpace(user.DisplayName),
			id:    strings.TrimSpace(user.ID),
		}
	}
	app := record.InitiatedBy.App
	if strings.TrimSpace(app.DisplayName) != "" || strings.TrimSpace(app.ServicePrincipalName) != "" ||
		strings.TrimSpace(app.AppID) != "" || strings.TrimSpace(app.ServicePrincipalID) != "" {
		name := strings.TrimSpace(app.DisplayName)
		if name == "" {
			name = strings.TrimSpace(app.ServicePrincipalName)
		}
		id := strings.TrimSpace(app.AppID)
		if id == "" {
			id = strings.TrimSpace(app.ServicePrincipalID)
		}
		return directoryPrincipal{kind: models.AuditLogActorApplication, name: name, id: id}
	}
	// No principal at all: the only thing the record says is which service
	// logged the change (loggedByService), which is what the screen shows.
	return directoryPrincipal{kind: models.AuditLogActorSystem, name: strings.TrimSpace(record.LoggedByService)}
}

// recordOwnerOnBehalfOf recovers the person a workload-made change was made for.
// Microsoft logs a device registration as the work of "Device Registration
// Service" and repeats the human in one of three places, all of which are read
// here: a sibling user target resource, a `RegisteredOwner`/`RegisteredUsers`
// modified property, or an owner-like additional detail.
//
// A value that is not an address (a display name alone) is not used as the
// e-mail: attribution that cannot be verified is left empty on purpose.
func recordOwnerOnBehalfOf(record directoryAuditRecord) (email, name string) {
	for i := range record.TargetResources {
		resource := record.TargetResources[i]
		if directoryTargetOfType(resource.Type) != models.AuditLogTargetUser {
			continue
		}
		if upn := strings.ToLower(strings.TrimSpace(resource.UserPrincipalName)); strings.Contains(upn, "@") {
			return upn, strings.TrimSpace(resource.DisplayName)
		}
		if upn := strings.ToLower(strings.TrimSpace(resource.DisplayName)); strings.Contains(upn, "@") {
			return upn, ""
		}
	}
	for i := range record.TargetResources {
		for _, property := range record.TargetResources[i].ModifiedProperties {
			if !isOwnerProperty(property.DisplayName) {
				continue
			}
			if value := addressOf(propertyValue(property.NewValue)); value != "" {
				return value, ""
			}
		}
	}
	for _, detail := range record.AdditionalDetails {
		if !isOwnerProperty(detail.Key) {
			continue
		}
		if value := addressOf(propertyValue(detail.Value)); value != "" {
			return value, ""
		}
	}
	return "", ""
}

// isOwnerProperty reports whether a Graph property or detail names the person
// behind a workload change.
func isOwnerProperty(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "registeredowner", "registeredowners", "registereduser", "registeredusers", "owner", "owners":
		return true
	default:
		return false
	}
}

// addressOf answers the lower-cased value of a string that looks like an address
// and an empty string for anything else.
func addressOf(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if !strings.Contains(value, "@") || strings.ContainsAny(value, " \t\n") {
		return ""
	}
	return value
}

// propertyValue decodes a Graph modifiedProperties / additionalDetails value.
// Graph JSON-encodes it, so an address arrives as `"\"ivan@…\""` and a list as
// `"[\"ivan@…\"]"`; a plain value is returned as it is.
func propertyValue(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var text string
	if err := json.Unmarshal([]byte(trimmed), &text); err == nil {
		return strings.TrimSpace(text)
	}
	var list []string
	if err := json.Unmarshal([]byte(trimmed), &list); err == nil {
		for _, item := range list {
			if value := strings.TrimSpace(item); value != "" {
				return value
			}
		}
		return ""
	}
	return strings.Trim(trimmed, `"`)
}

// recordDeviceID reads the Entra device id out of a record: some activities carry
// the `deviceId` of the device object (which `GET /devices/{id}` also accepts) in
// their modified properties instead of the object id.
func recordDeviceID(record directoryAuditRecord) string {
	for _, detail := range record.AdditionalDetails {
		if strings.EqualFold(strings.TrimSpace(detail.Key), "deviceId") {
			if value := propertyValue(detail.Value); value != "" {
				return value
			}
		}
	}
	for i := range record.TargetResources {
		for _, property := range record.TargetResources[i].ModifiedProperties {
			if !strings.EqualFold(strings.TrimSpace(property.DisplayName), "deviceId") {
				continue
			}
			if value := propertyValue(property.NewValue); value != "" {
				return value
			}
		}
	}
	return ""
}

// classifyDirectoryAudit maps the vendor activity onto the shared vocabulary.
// Graph reports the verb in `operationType` for most categories and only in the
// display name for the older ones, so both are read.
func classifyDirectoryAudit(record directoryAuditRecord) (models.AuditLogAction, models.AuditLogTarget) {
	target := directoryTarget(record)
	switch strings.ToLower(strings.TrimSpace(record.OperationType)) {
	case "add", "create":
		return models.AuditLogCreate, target
	case "delete", "removedeleteditem", "harddelete":
		return models.AuditLogDelete, target
	case "update", "set":
		return models.AuditLogUpdate, target
	}
	name := strings.ToLower(strings.TrimSpace(record.ActivityDisplayName))
	switch {
	case strings.HasPrefix(name, "add "), strings.Contains(name, "add user"), strings.Contains(name, "create"):
		return models.AuditLogCreate, target
	case strings.Contains(name, "delete"), strings.Contains(name, "remove"):
		return models.AuditLogDelete, target
	case strings.Contains(name, "update"), strings.Contains(name, "reset"), strings.Contains(name, "change"):
		return models.AuditLogUpdate, target
	}
	return models.AuditLogOther, target
}

// directoryTarget names the kind of directory object the change touched. The
// event is filed under the first resource that maps to a known kind, so a record
// that lists [device, user] is a device event and not a user one.
func directoryTarget(record directoryAuditRecord) models.AuditLogTarget {
	for i := range record.TargetResources {
		if target := directoryTargetOfType(record.TargetResources[i].Type); target != models.AuditLogTargetOther {
			return target
		}
	}
	return models.AuditLogTargetOther
}

// directoryTargetOfType maps one Graph targetResources.type onto the shared
// vocabulary. Device and Application/ServicePrincipal are what keeps a device
// registration and an application consent out of the catch-all "other".
func directoryTargetOfType(raw string) models.AuditLogTarget {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "user":
		return models.AuditLogTargetUser
	case "group":
		return models.AuditLogTargetGroup
	case "device":
		return models.AuditLogTargetDevice
	case "application", "serviceprincipal":
		return models.AuditLogTargetApplication
	case "site", "sharepoint site":
		return models.AuditLogTargetSite
	}
	return models.AuditLogTargetOther
}
