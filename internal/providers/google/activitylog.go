package google

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// The activity-log streams of the Google provider. Each one is one application
// of the Admin SDK Reports API (`applications/{name}`), which is why the stream
// name is the application name: the collector stores one cursor per stream, so a
// busy sign-in feed never delays a file-deletion feed.
const (
	// streamDrive is file activity: creations, uploads, edits, deletions, moves
	// and sharing changes.
	streamDrive = "drive"
	// streamUserAccounts is the user lifecycle (create, delete, suspend,
	// undelete, rename, password and admin-privilege changes).
	streamUserAccounts = "user_accounts"
	// streamGroups is the group lifecycle and its membership.
	streamGroups = "groups"
	// streamAdmin is the Admin console settings feed (every settings change an
	// administrator makes).
	streamAdmin = "admin"
	// streamLogin is the sign-in feed: successes, failures, challenges and the
	// account warnings Google raises on suspicious sign-ins.
	streamLogin = "login"
)

// activityStreams lists the feeds ActivityLogs reads, in the order the screen
// lists them.
var activityStreams = []string{streamDrive, streamUserAccounts, streamGroups, streamAdmin, streamLogin}

// reportsScope is the single scope every Reports API application needs. It is
// declared here (and repeated in the permission table, permissions.go) because
// the permission table sends it in the authorization request while the collector
// only reports it in ActivityLogScopes.
const reportsScope = "https://www.googleapis.com/auth/admin.reports.audit.readonly"

const (
	// activityBase is the Reports API activity endpoint. It is served by the
	// Admin SDK API the Workspace connection already enables.
	activityBase = "https://admin.googleapis.com/admin/reports/v1/activity"
	// activityPageLimit is the largest page the API accepts.
	activityPageLimit = 1000
	// activityMaxPages bounds how many pages one pass of one stream reads, so a
	// first run over a large tenant cannot walk the whole history in one tick.
	// When the cap is reached the cursor is the newest event read, and the next
	// pass resumes from it (the overlap is absorbed by the event identity).
	activityMaxPages = 20
	// activityLookback is how far back a stream with no cursor starts. It is the
	// window a fresh install shows, not the API limit (Google keeps 180 days):
	// a whole history would be one long backlog no operator waits for.
	activityLookback = 7 * 24 * time.Hour
	// activityOverlap re-reads a few minutes of the previous pass, because the
	// Reports API puts an event in the feed when it processes it, not exactly
	// when it happened, so an event can land slightly behind the watermark.
	activityOverlap = 5 * time.Minute
)

// driveEventNames narrows the drive stream to the changes an operator audits.
// The API would otherwise return every view, preview and download as well, and
// that volume would bury the deletions the screen exists for. The list is the
// filter of the request, so the payload stays small too.
var driveEventNames = []string{
	"create",
	"upload",
	"edit",
	"move",
	"rename",
	"delete",
	"change_user_access",
	"change_document_access_scope",
	"change_acl_editors",
	"change_acl_scope",
}

// Compile time guard: the audit-log collector hands these calls to the registry
// as an interface, so a renamed method must fail the build.
var _ providers.ActivityLogSource = (*Provider)(nil)

// ActivityLogStreams implements providers.ActivityLogSource.
func (p *Provider) ActivityLogStreams() []string {
	streams := make([]string, len(activityStreams))
	copy(streams, activityStreams)
	return streams
}

// ActivityLogScopes implements providers.ActivityLogSource: the one scope the
// Reports API needs, which is also the one a Workspace account connected before
// the feed existed does not have yet.
func (p *Provider) ActivityLogScopes() []string {
	return []string{reportsScope}
}

// ActivityLogs implements providers.ActivityLogSource: it reads one application
// of the Reports API for the domain behind tokens, page by page, and answers the
// normalised events plus the watermark of the next pass.
//
// The cursor is an RFC3339 UTC timestamp: the feed is ordered by time, so a
// watermark is all resuming needs, and it survives the page token expiring.
func (p *Provider) ActivityLogs(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, stream, cursor string) ([]providers.ActivityEvent, string, error) {
	if tokens == nil || strings.TrimSpace(tokens.AccessToken) == "" {
		return nil, "", fmt.Errorf("google: the activity feed needs an access token")
	}
	application, err := activityApplication(stream)
	if err != nil {
		return nil, "", err
	}

	end := time.Now().UTC()
	start := end.Add(-activityLookback)
	if previous, ok := parseActivityWatermark(cursor); ok {
		start = previous.Add(-activityOverlap)
	}
	if start.After(end) {
		start = end
	}

	client := p.client(tokens)
	events := []providers.ActivityEvent{}
	newest := start
	pageToken := ""
	for page := 0; page < activityMaxPages; page++ {
		var answer struct {
			Items         []googleActivity `json:"items"`
			NextPageToken string           `json:"nextPageToken"`
		}
		if err := p.doJSON(ctx, client, "GET", activityURL(application, start, end, pageToken), nil, &answer); err != nil {
			return events, "", err
		}
		for _, activity := range answer.Items {
			events = append(events, normaliseActivity(application, activity)...)
			if activity.ID.Time.After(newest) {
				newest = activity.ID.Time
			}
		}
		if answer.NextPageToken == "" {
			return events, formatActivityWatermark(end), nil
		}
		pageToken = answer.NextPageToken
	}
	// The page cap was reached: resume from the newest event read instead of
	// claiming the whole window was consumed.
	return events, formatActivityWatermark(newest), nil
}

// activityApplication validates a stream name against the ones this provider
// reads, so a cursor stored for another provider can never be sent here.
func activityApplication(stream string) (string, error) {
	for _, name := range activityStreams {
		if name == stream {
			return name, nil
		}
	}
	return "", fmt.Errorf("google: unknown activity stream %q", stream)
}

// activityURL builds one page of one application's feed. The window is the
// incremental one described by the cursor, so the API does the filtering and the
// page stays small.
func activityURL(application string, start, end time.Time, pageToken string) string {
	target := activityBase + "/users/all/applications/" + application
	target = withParam(target, "startTime", formatActivityWatermark(start))
	target = withParam(target, "endTime", formatActivityWatermark(end))
	target = withParam(target, "maxResults", fmt.Sprintf("%d", activityPageLimit))
	target = withParam(target, "pageToken", pageToken)
	if application == streamDrive {
		for _, name := range driveEventNames {
			target += "&eventName=" + name
		}
	}
	return target
}

// formatActivityWatermark renders a cursor. Milliseconds are dropped so a cursor
// is stable between passes and readable in SQL.
func formatActivityWatermark(moment time.Time) string {
	return moment.UTC().Format(time.RFC3339)
}

// parseActivityWatermark reads a cursor, answering false for an empty or
// unreadable value (which makes the stream start from the default window).
func parseActivityWatermark(cursor string) (time.Time, bool) {
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

// googleActivity is one item of the Reports API answer, trimmed to the fields
// the normaliser maps.
type googleActivity struct {
	ID struct {
		Time            time.Time `json:"time"`
		UniqueQualifier string    `json:"uniqueQualifier"`
		ApplicationName string    `json:"applicationName"`
		CustomerID      string    `json:"customerId"`
	} `json:"id"`
	Actor struct {
		CallerType string `json:"callerType"`
		Email      string `json:"email"`
		ProfileID  string `json:"profileId"`
	} `json:"actor"`
	IPAddress string        `json:"ipAddress"`
	Events    []googleEvent `json:"events"`
}

// googleEvent is one event inside an activity: an activity describes one
// request, and a request may report several effects.
type googleEvent struct {
	Type       string        `json:"type"`
	Name       string        `json:"name"`
	Parameters []googleParam `json:"parameters"`
}

// googleParam is one parameter of an event. The Reports API types the value, so
// every shape is carried and the normaliser reads the one it needs.
type googleParam struct {
	Name          string   `json:"name"`
	Value         string   `json:"value"`
	IntValue      *int64   `json:"intValue"`
	BoolValue     *bool    `json:"boolValue"`
	MultiValue    []string `json:"multiValue"`
	MultiIntValue []int64  `json:"multiIntValue"`
}

// param returns the string value of a named parameter.
func (e googleEvent) param(name string) string {
	for _, parameter := range e.Parameters {
		if strings.EqualFold(parameter.Name, name) {
			return parameter.Value
		}
	}
	return ""
}

// normaliseActivity turns one Reports API item into the events the collector
// stores: one ActivityEvent per event of the activity, so a request that both
// moved and shared a file is two rows.
func normaliseActivity(application string, activity googleActivity) []providers.ActivityEvent {
	if len(activity.Events) == 0 {
		return nil
	}
	occurred := activity.ID.Time.UTC()
	events := make([]providers.ActivityEvent, 0, len(activity.Events))
	for index, event := range activity.Events {
		raw, err := json.Marshal(struct {
			IPAddress string      `json:"ip_address,omitempty"`
			Event     googleEvent `json:"event"`
		}{IPAddress: activity.IPAddress, Event: event})
		if err != nil {
			raw = nil
		}
		action, target := classifyActivity(application, event)
		events = append(events, providers.ActivityEvent{
			Source:     application,
			RemoteID:   activityRemoteID(activity, index),
			Action:     action,
			Target:     target,
			ActorEmail: strings.ToLower(strings.TrimSpace(activity.Actor.Email)),
			ActorID:    activity.Actor.ProfileID,
			TargetName: activityTargetName(application, event),
			TargetID:   activityTargetID(application, event),
			OccurredAt: occurred,
			Raw:        raw,
		})
	}
	return events
}

// activityRemoteID is the identity of one event: the customer, the application,
// the request Google qualified as unique and the index of the event inside it.
// The index is what keeps a request that reports several effects from collapsing
// into one row.
func activityRemoteID(activity googleActivity, index int) string {
	return fmt.Sprintf("%s/%s/%s/%d",
		activity.ID.CustomerID, activity.ID.ApplicationName, activity.ID.UniqueQualifier, index)
}

// classifyActivity maps the vendor vocabulary of one application onto the shared
// action/target vocabulary. An unrecognised event is kept as an update of
// whatever the parameters say, never dropped: the Raw record still explains it.
func classifyActivity(application string, event googleEvent) (models.AuditLogAction, models.AuditLogTarget) {
	name := strings.ToLower(strings.TrimSpace(event.Name))
	switch application {
	case streamDrive:
		target := driveTarget(event, name)
		switch name {
		case "delete", "delete_team_drive":
			return models.AuditLogDelete, target
		case "create", "upload", "create_team_drive":
			return models.AuditLogCreate, target
		case "move":
			return models.AuditLogMove, target
		case "rename", "edit":
			return models.AuditLogUpdate, target
		case "change_user_access", "change_document_access_scope", "change_acl_editors", "change_acl_scope":
			return models.AuditLogShare, target
		}
		return models.AuditLogOther, target
	case streamUserAccounts:
		switch name {
		case "create_user":
			return models.AuditLogCreate, models.AuditLogTargetUser
		case "delete_user":
			return models.AuditLogDelete, models.AuditLogTargetUser
		case "undelete_user", "restore_user":
			return models.AuditLogCreate, models.AuditLogTargetUser
		}
		return models.AuditLogUpdate, models.AuditLogTargetUser
	case streamGroups:
		switch name {
		case "create_group":
			return models.AuditLogCreate, models.AuditLogTargetGroup
		case "delete_group":
			return models.AuditLogDelete, models.AuditLogTargetGroup
		case "add_group_member":
			return models.AuditLogCreate, models.AuditLogTargetGroup
		case "remove_group_member":
			return models.AuditLogDelete, models.AuditLogTargetGroup
		}
		return models.AuditLogUpdate, models.AuditLogTargetGroup
	case streamLogin:
		return models.AuditLogLogin, models.AuditLogTargetUser
	case streamAdmin:
		if target, ok := adminTarget(strings.ToLower(strings.TrimSpace(event.Type))); ok {
			return models.AuditLogUpdate, target
		}
		return models.AuditLogUpdate, models.AuditLogTargetOther
	}
	return models.AuditLogOther, models.AuditLogTargetOther
}

// driveTarget names the object a drive event touched. A team-drive event is
// about the drive itself, a folder event about a folder, everything else about a
// file.
func driveTarget(event googleEvent, name string) models.AuditLogTarget {
	if strings.Contains(name, "team_drive") {
		return models.AuditLogTargetDrive
	}
	switch strings.ToLower(event.param("doc_type")) {
	case "folder":
		return models.AuditLogTargetFolder
	case "shared_drive", "team_drive":
		return models.AuditLogTargetDrive
	}
	return models.AuditLogTargetFile
}

// adminTarget maps the Admin console settings type of an event onto the object
// it belongs to, so the screen can narrow the feed to users, groups or sites.
func adminTarget(settingsType string) (models.AuditLogTarget, bool) {
	switch settingsType {
	case "user_settings", "user_confirmation":
		return models.AuditLogTargetUser, true
	case "group_settings":
		return models.AuditLogTargetGroup, true
	case "drive_settings", "shared_drive_settings":
		return models.AuditLogTargetDrive, true
	case "sites_settings":
		return models.AuditLogTargetSite, true
	}
	return models.AuditLogTargetOther, false
}

// activityTargetName is the display label of the object: the document title, the
// group or user address, or the setting that changed.
func activityTargetName(application string, event googleEvent) string {
	switch application {
	case streamDrive:
		if title := event.param("doc_title"); title != "" {
			return title
		}
		return event.param("doc_id")
	case streamUserAccounts:
		if email := event.param("USER_EMAIL"); email != "" {
			return email
		}
		return event.param("USER_NAME")
	case streamGroups:
		if email := event.param("GROUP_EMAIL"); email != "" {
			return email
		}
		return event.param("GROUP_NAME")
	case streamAdmin:
		for _, name := range []string{"SETTING_NAME", "USER_EMAIL", "GROUP_EMAIL"} {
			if value := event.param(name); value != "" {
				return value
			}
		}
		return strings.TrimSpace(event.Name)
	case streamLogin:
		return strings.TrimSpace(event.Name)
	}
	return ""
}

// activityTargetID is the stable id of the object, when the feed carries one.
func activityTargetID(application string, event googleEvent) string {
	switch application {
	case streamDrive:
		return event.param("doc_id")
	case streamUserAccounts, streamAdmin:
		return event.param("USER_EMAIL")
	case streamGroups:
		return event.param("GROUP_EMAIL")
	}
	return ""
}
