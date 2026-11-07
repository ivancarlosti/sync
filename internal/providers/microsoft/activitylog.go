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

	response, err := p.httpClient.Do(request)
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
	} `json:"targetResources"`
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
	event := providers.ActivityEvent{
		Source:     streamDirectory,
		RemoteID:   record.ID,
		Action:     action,
		Target:     target,
		ActorEmail: strings.ToLower(strings.TrimSpace(directoryActorEmail(record))),
		ActorName:  directoryActorName(record),
		ActorID:    directoryActorID(record),
		OccurredAt: record.ActivityDateTime.UTC(),
		Raw:        raw,
	}
	if len(record.TargetResources) > 0 {
		resource := record.TargetResources[0]
		event.TargetName = strings.TrimSpace(resource.DisplayName)
		if event.TargetName == "" {
			event.TargetName = strings.TrimSpace(resource.UserPrincipalName)
		}
		event.TargetID = strings.TrimSpace(resource.ID)
	}
	return event, true
}

// directoryActorEmail names the principal that made the change: the user when a
// person did it, the service principal or the application display name when the
// change was made by a workload.
func directoryActorEmail(record directoryAuditRecord) string {
	if email := record.InitiatedBy.User.UserPrincipalName; email != "" {
		return email
	}
	if name := record.InitiatedBy.App.ServicePrincipalName; name != "" {
		return name
	}
	return record.InitiatedBy.App.DisplayName
}

// directoryActorName is the display label of the actor.
func directoryActorName(record directoryAuditRecord) string {
	if name := record.InitiatedBy.User.DisplayName; name != "" {
		return name
	}
	return record.InitiatedBy.App.DisplayName
}

// directoryActorID is the stable id of the actor.
func directoryActorID(record directoryAuditRecord) string {
	if id := record.InitiatedBy.User.ID; id != "" {
		return id
	}
	if id := record.InitiatedBy.App.AppID; id != "" {
		return id
	}
	return record.InitiatedBy.App.ServicePrincipalID
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

// directoryTarget names the kind of directory object the change touched.
func directoryTarget(record directoryAuditRecord) models.AuditLogTarget {
	if len(record.TargetResources) == 0 {
		return models.AuditLogTargetOther
	}
	switch strings.ToLower(strings.TrimSpace(record.TargetResources[0].Type)) {
	case "user":
		return models.AuditLogTargetUser
	case "group":
		return models.AuditLogTargetGroup
	}
	return models.AuditLogTargetOther
}
