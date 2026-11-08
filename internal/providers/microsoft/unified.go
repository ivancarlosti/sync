package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// The Office 365 management activity API is the only Microsoft feed that
// reports what happens *inside* the workloads: a file deleted from OneDrive or a
// SharePoint document library is an `Audit.SharePoint` record there, and Graph
// v1.0 has no equivalent (the file-activity endpoints exist only in /beta, as an
// asynchronous query job, which is not a feed a poller can resume).
//
// The API is app-only: it takes an application token for the `manage.office.com`
// resource and requires the *application* permission `ActivityFeed.Read` granted
// with tenant-wide consent. That permission is deliberately not part of the
// delegated permission table (permissions.go): it can neither be requested in an
// authorization URL (it is not a delegated scope) nor proven by the `scope` of a
// token response, so declaring it would mark every connected account as missing
// a permission a reconnect cannot add. It is documented instead — see
// docs/app-registration.md — and a tenant that did not grant it answers 401/403
// here, which the collector stores as the stream's last error ("needs consent")
// while the directory stream keeps working.
const (
	// unifiedBase is the management activity API root.
	unifiedBase = "https://manage.office.com/api/v1.0"
	// unifiedResource is the resource the app-only token is requested for.
	unifiedResource = "https://manage.office.com"
	// unifiedScope is the application permission the API needs.
	unifiedScope = unifiedResource + "/.default"
	// unifiedMaxWindows bounds how many 24-hour windows one pass walks, so a
	// first run (or a long outage) cannot read the whole seven days the API
	// retains in one tick.
	unifiedMaxWindows = 7
	// unifiedWindow is the range of one content listing. Microsoft documents a
	// 24-hour maximum, and the blobs it points at are kept for seven days.
	unifiedWindow = 24 * time.Hour
	// unifiedMaxBlobs bounds how many content blobs one pass follows, so a busy
	// tenant cannot turn one tick into thousands of requests.
	unifiedMaxBlobs = 200
)

// unifiedContentTypes are the subscriptions this collector reads. SharePoint is
// where file activity lives; General carries the rest of the workloads (Teams,
// Power BI, …), which is what makes the feed complete rather than just
// file-shaped.
var unifiedContentTypes = []string{"Audit.SharePoint", "Audit.General"}

// unifiedAuditLogs reads the management activity feed for the tenant behind
// tokens: it subscribes (idempotently) to each content type, lists the content
// blobs of the incremental window and decodes each blob.
//
// tokens is only used to name the tenant (its `tid` claim): the records are read
// with an app-only token built from the OAuth client of the instance, because
// this API does not accept a delegated one.
func (p *Provider) unifiedAuditLogs(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, cursor string) ([]providers.ActivityEvent, string, error) {
	tenant := p.OrgIDOf(tokens)
	if tenant == "" {
		tenant = strings.ToLower(strings.TrimSpace(creds.TenantID))
	}
	if !isTenantID(tenant) {
		return nil, "", fmt.Errorf("microsoft: the office 365 activity feed needs a tenant id (connect a work account or set MICROSOFT_TENANT_ID)")
	}
	if strings.TrimSpace(creds.ClientID) == "" || strings.TrimSpace(creds.ClientSecret) == "" {
		return nil, "", fmt.Errorf("microsoft: the office 365 activity feed needs the application credentials (client id and secret)")
	}

	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-unifiedWindow)
	if previous, ok := parseWatermark(cursor); ok {
		start = previous
	}
	if start.After(now) {
		start = now
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	token, err := p.appOnlyToken(ctx, tenant, creds)
	if err != nil {
		return nil, "", err
	}

	events := []providers.ActivityEvent{}
	newest := start
	blobs := 0
	for window := 0; window < unifiedMaxWindows; window++ {
		from := start.Add(time.Duration(window) * unifiedWindow)
		if !from.Before(now) {
			break
		}
		to := from.Add(unifiedWindow)
		if to.After(now) {
			to = now
		}
		for _, contentType := range unifiedContentTypes {
			if err := p.startSubscription(ctx, tenant, token, contentType); err != nil {
				return events, "", err
			}
			uris, err := p.contentBlobs(ctx, tenant, token, contentType, from, to)
			if err != nil {
				return events, "", err
			}
			for _, uri := range uris {
				if blobs >= unifiedMaxBlobs {
					// The blob cap was reached: resume from the newest record
					// read, so the next pass picks the rest up.
					return events, formatWatermark(newest), nil
				}
				blobs++
				records, err := p.blobRecords(ctx, uri, token)
				if err != nil {
					return events, "", err
				}
				for _, record := range records {
					event, ok := normaliseUnifiedRecord(record)
					if !ok {
						continue
					}
					events = append(events, event)
					if event.OccurredAt.After(newest) {
						newest = event.OccurredAt
					}
				}
			}
		}
	}
	return events, formatWatermark(now), nil
}

// isTenantID reports whether value looks like the tenant segment the API path
// accepts: a GUID or a verified domain name. Anything with a slash or a scheme
// is refused, because the value is interpolated into an URL.
func isTenantID(value string) bool {
	if value == "" || len(value) > 191 {
		return false
	}
	if value == "common" || value == "organizations" || value == "consumers" {
		return false
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		case char == '-', char == '.', char == '_':
		default:
			return false
		}
	}
	return true
}

// appOnlyToken exchanges the client credentials for an application token of the
// management activity API. The tenant comes from the connected account, so one
// instance can read several directories without a per-directory setting.
func (p *Provider) appOnlyToken(ctx context.Context, tenant string, creds providers.Credentials) (string, error) {
	form := url.Values{}
	form.Set("client_id", creds.ClientID)
	form.Set("client_secret", creds.ClientSecret)
	form.Set("scope", unifiedScope)
	form.Set("grant_type", "client_credentials")

	target := loginBaseURL + "/" + tenant + "/oauth2/v2.0/token"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("microsoft: cannot build the activity feed token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := p.doRequest(ctx, request)
	if err != nil {
		return "", fmt.Errorf("microsoft: the activity feed token request failed: %w", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", fmt.Errorf("microsoft: the office 365 activity feed needs an administrator to consent to ActivityFeed.Read (%s)",
			oauthErrorSummary(raw, response.StatusCode))
	}
	var answer struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil || answer.AccessToken == "" {
		return "", errors.New("microsoft: the activity feed token response carried no access token")
	}
	return answer.AccessToken, nil
}

// oauthErrorSummary extracts the diagnostic of an Entra or management-API error
// document, so the cursor's last error explains the consent instead of printing
// a raw JSON blob. Entra answers `{"error":"invalid_grant","error_description":…}`
// and the management API `{"error":{"code":"AF20024","message":…}}`, so both
// shapes are read. The client secret is never part of a response, so the text is
// safe to store.
func oauthErrorSummary(raw []byte, status int) string {
	var envelope struct {
		Error            json.RawMessage `json:"error"`
		ErrorDescription string          `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Error) == 0 {
		return fmt.Sprintf("http %d", status)
	}
	var code string
	if err := json.Unmarshal(envelope.Error, &code); err != nil {
		// The object shape: keep the code and the message.
		var detail struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(envelope.Error, &detail); err != nil {
			return fmt.Sprintf("http %d", status)
		}
		code = detail.Code
		if detail.Message != "" {
			return fmt.Sprintf("http %d: %s: %s", status, code, detail.Message)
		}
	}
	if code == "" {
		return fmt.Sprintf("http %d", status)
	}
	description := envelope.ErrorDescription
	if index := strings.Index(description, "\r\n"); index > 0 {
		description = description[:index]
	}
	if description == "" {
		return fmt.Sprintf("http %d: %s", status, code)
	}
	return fmt.Sprintf("http %d: %s: %s", status, code, description)
}

// startSubscription enables the content type subscription of the tenant. The
// call is idempotent: Microsoft answers 400/AF20024 when the subscription is
// already enabled, which is the state every pass after the first one finds.
func (p *Provider) startSubscription(ctx context.Context, tenant, token, contentType string) error {
	target := unifiedBase + "/" + tenant + "/activity/feed/subscriptions/start?contentType=" + url.QueryEscape(contentType)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return fmt.Errorf("microsoft: cannot build the activity feed subscription request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := p.doRequest(ctx, request)
	if err != nil {
		return fmt.Errorf("microsoft: the activity feed subscription request failed: %w", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	if response.StatusCode >= 200 && response.StatusCode <= 299 {
		return nil
	}
	summary := oauthErrorSummary(raw, response.StatusCode)
	if response.StatusCode == http.StatusBadRequest &&
		(strings.Contains(summary, "AF20024") || strings.Contains(strings.ToLower(summary), "already enabled")) {
		return nil
	}
	return fmt.Errorf("microsoft: cannot enable the %s activity subscription: %s%s", contentType, summary, activityFeedConsentHint(summary))
}

// activityFeedConsentHint turns the AF10001 the management API answers an
// ungranted application into the fix. The code comes with `permission set ()`,
// which is the tell: the app-only token carries no application roles, so
// ActivityFeed.Read was never granted (or an administrator of that directory
// never consented to it) — no retry and no reconnect can change that.
//
// Neither this message nor the AF10001 document names the directory, which is why
// the hint says "this directory": the collector prefixes every stored failure with
// the tenant it belongs to (display name, tenant id and the account that read it,
// see services.AuditLogService.failureMessage), so the line the operator reads in
// the Audit Logs banner names the directory before this text begins.
func activityFeedConsentHint(summary string) string {
	if !strings.Contains(summary, "AF10001") {
		return ""
	}
	return " (grant the office 365 management apis application permission ActivityFeed.Read to the app registration," +
		" have an administrator of this directory consent to it, and turn on unified audit logging in Purview)"
}

// contentBlobs lists the content blobs of one content type for one window. The
// answer is paginated through a `NextPageUri` header, which is followed a
// bounded number of times; the URI is checked to stay on the API host because it
// is fetched with the same bearer token.
func (p *Provider) contentBlobs(ctx context.Context, tenant, token, contentType string, from, to time.Time) ([]string, error) {
	query := url.Values{}
	query.Set("contentType", contentType)
	query.Set("startTime", from.UTC().Format(unifiedTimeFormat))
	query.Set("endTime", to.UTC().Format(unifiedTimeFormat))
	target := unifiedBase + "/" + tenant + "/activity/feed/subscriptions/content?" + query.Encode()

	uris := []string{}
	for page := 0; page < directoryAuditMaxPages; page++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, fmt.Errorf("microsoft: cannot build the activity feed content request: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Accept", "application/json")

		response, err := p.doRequest(ctx, request)
		if err != nil {
			return nil, fmt.Errorf("microsoft: the activity feed content request failed: %w", err)
		}
		var blobs []struct {
			ContentURI string `json:"contentUri"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(&blobs)
		next := response.Header.Get("NextPageUri")
		response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode > 299 {
			return nil, fmt.Errorf("microsoft: the activity feed content request answered http %d", response.StatusCode)
		}
		if decodeErr != nil && decodeErr != io.EOF {
			return nil, fmt.Errorf("microsoft: cannot decode the activity feed content list: %w", decodeErr)
		}
		for _, blob := range blobs {
			if blob.ContentURI != "" {
				uris = append(uris, blob.ContentURI)
			}
		}
		if next == "" {
			return uris, nil
		}
		if !strings.HasPrefix(next, unifiedBase+"/") {
			return nil, fmt.Errorf("microsoft: an activity feed page link left the management API (%s)", safeURL(next))
		}
		target = next
	}
	return uris, nil
}

// blobRecords decodes one content blob: a JSON array of audit records, one per
// line and each kept verbatim as the event's Raw.
func (p *Provider) blobRecords(ctx context.Context, uri, token string) ([]json.RawMessage, error) {
	if !strings.HasPrefix(uri, "https://") {
		return nil, fmt.Errorf("microsoft: the activity feed blob link is not an https URL (%s)", safeURL(uri))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, fmt.Errorf("microsoft: cannot build the activity feed blob request: %w", err)
	}
	// A content URI is pre-authenticated, but the token is harmless there and
	// some tenants require it: send it.
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := p.doRequest(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("microsoft: the activity feed blob request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("microsoft: the activity feed blob request answered http %d", response.StatusCode)
	}
	var records []json.RawMessage
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<20)).Decode(&records); err != nil {
		return nil, fmt.Errorf("microsoft: cannot decode an activity feed blob: %w", err)
	}
	return records, nil
}

// unifiedTimeFormat is the timestamp shape the management activity API takes in
// startTime/endTime: ISO 8601 without a zone, because the window is always UTC.
const unifiedTimeFormat = "2006-01-02T15:04:05"

// unifiedRecord is one record of an Audit.SharePoint / Audit.General blob,
// trimmed to the fields the normaliser maps. A record of another workload
// carries a subset and is normalised with what it has.
type unifiedRecord struct {
	ID             string `json:"Id"`
	Operation      string `json:"Operation"`
	Workload       string `json:"Workload"`
	RecordType     int    `json:"RecordType"`
	ResultStatus   string `json:"ResultStatus"`
	CreationTime   string `json:"CreationTime"`
	UserID         string `json:"UserId"`
	UserKey        string `json:"UserKey"`
	UserType       int    `json:"UserType"`
	ClientIP       string `json:"ClientIP"`
	ObjectID       string `json:"ObjectId"`
	ItemType       string `json:"ItemType"`
	EventSource    string `json:"EventSource"`
	SiteURL        string `json:"SiteUrl"`
	SourceFileName string `json:"SourceFileName"`
	TargetUser     string `json:"TargetUserOrGroupName"`
	CorrelationID  string `json:"CorrelationId"`
}

// normaliseUnifiedRecord maps one workload record onto the shared vocabulary.
// The identity is the record Id, which Microsoft assigns per record and keeps
// stable, so a re-read of an overlapping window cannot duplicate a row.
func normaliseUnifiedRecord(raw json.RawMessage) (providers.ActivityEvent, bool) {
	var record unifiedRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return providers.ActivityEvent{}, false
	}
	occurred, ok := parseUnifiedTime(record.CreationTime)
	if !ok {
		return providers.ActivityEvent{}, false
	}
	remoteID := strings.TrimSpace(record.ID)
	if remoteID == "" {
		// Older records of some workloads carry no Id: the correlation id plus
		// the operation is the next best identity, and it is stable.
		remoteID = strings.TrimSpace(record.CorrelationID + "/" + record.Operation + "/" + record.CreationTime)
		if strings.Count(remoteID, "/") != 2 || strings.HasPrefix(remoteID, "/") {
			return providers.ActivityEvent{}, false
		}
	}
	action, target := classifyUnifiedOperation(record)
	event := providers.ActivityEvent{
		Source:     streamUnified,
		RemoteID:   remoteID,
		Action:     action,
		Target:     target,
		ActorEmail: strings.ToLower(strings.TrimSpace(record.UserID)),
		ActorID:    strings.TrimSpace(record.UserKey),
		TargetName: strings.TrimSpace(record.SourceFileName),
		TargetID:   strings.TrimSpace(record.ObjectID),
		SiteID:     strings.TrimSpace(record.SiteURL),
		OccurredAt: occurred,
		Raw:        raw,
	}
	if event.TargetName == "" {
		event.TargetName = strings.TrimSpace(record.TargetUser)
	}
	return event, true
}

// parseUnifiedTime reads a CreationTime. The management API has shipped both the
// zone-less shape and RFC3339, so both are accepted.
func parseUnifiedTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, unifiedTimeFormat, "2006-01-02T15:04:05.000"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

// classifyUnifiedOperation maps the workload operation onto the shared
// vocabulary. The prefix of the operation is what tells the kind of object, so
// the same table covers a file, a folder and a site.
func classifyUnifiedOperation(record unifiedRecord) (models.AuditLogAction, models.AuditLogTarget) {
	operation := strings.ToLower(strings.TrimSpace(record.Operation))
	target := unifiedTarget(record, operation)
	switch {
	case strings.Contains(operation, "deleted"), strings.Contains(operation, "recycled"),
		strings.Contains(operation, "removed"):
		return models.AuditLogDelete, target
	case strings.Contains(operation, "renamed"), strings.Contains(operation, "modified"),
		strings.Contains(operation, "restored"), strings.Contains(operation, "checkedin"),
		strings.Contains(operation, "updated"):
		return models.AuditLogUpdate, target
	case strings.Contains(operation, "moved"):
		return models.AuditLogMove, target
	case strings.Contains(operation, "sharing"), strings.Contains(operation, "anonymouslink"),
		strings.Contains(operation, "securelink"), strings.Contains(operation, "invitation"),
		strings.Contains(operation, "addedtogroup"), strings.Contains(operation, "companylink"):
		return models.AuditLogShare, target
	case strings.Contains(operation, "uploaded"), strings.Contains(operation, "created"),
		strings.Contains(operation, "added"), strings.Contains(operation, "copied"):
		return models.AuditLogCreate, target
	}
	return models.AuditLogOther, target
}

// unifiedTarget names the kind of object the operation touched.
func unifiedTarget(record unifiedRecord, operation string) models.AuditLogTarget {
	switch {
	case strings.Contains(operation, "folder"):
		return models.AuditLogTargetFolder
	case strings.Contains(operation, "site"), strings.Contains(operation, "web"),
		strings.Contains(operation, "library"):
		return models.AuditLogTargetSite
	case strings.Contains(operation, "file"), strings.Contains(operation, "document"):
		return models.AuditLogTargetFile
	}
	switch strings.ToLower(strings.TrimSpace(record.ItemType)) {
	case "file":
		return models.AuditLogTargetFile
	case "folder":
		return models.AuditLogTargetFolder
	case "site", "web":
		return models.AuditLogTargetSite
	}
	return models.AuditLogTargetOther
}
