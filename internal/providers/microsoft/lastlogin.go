// Last login for the Microsoft 365 users and guests reports.
//
// Microsoft Graph exposes a user's last sign-in as the `signInActivity` property,
// but the property is gated by a Microsoft Entra ID P1/P2 licence: a tenant on
// Microsoft 365 Business Standard (Entra ID Free) refuses it, which is the plan
// this falls back for.
//
// The Microsoft 365 unified audit log records the same event as `UserLoggedIn`
// under the `Audit.AzureActiveDirectory` content type, and Audit (Standard) is
// part of Business Standard. The Office 365 Management Activity API serves that
// content with the app-only `ActivityFeed.Read` permission the Audit Logs screen
// already uses, so the fallback needs no new Entra licence and no new consent.
// Its window is the seven days the activity API retains, not the whole life of
// the account: a user who has not signed in during that window is left blank
// rather than guessed.
package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/providers"
)

// lastLoginContentType is the unified audit log content type that carries the
// Microsoft Entra sign-in events. It is separate from the `Audit.SharePoint` and
// `Audit.General` subscriptions of the activity feed, which is why the report
// asks for it explicitly instead of consuming the audit-log cursor.
const lastLoginContentType = "Audit.AzureActiveDirectory"

// lastLoginRecord is one user sign-in record of the unified audit log, trimmed to
// the fields the map needs. `UserId` is the user principal name of the account
// that signed in; `ObjectId` is used only when it carries an address (some
// workloads leave UserId empty).
type lastLoginRecord struct {
	Operation    string `json:"Operation"`
	CreationTime string `json:"CreationTime"`
	ResultStatus string `json:"ResultStatus"`
	UserID       string `json:"UserId"`
	UserKey      string `json:"UserKey"`
	ObjectID     string `json:"ObjectId"`
}

// loadLastLogins fills the per-address map the users table reads when Graph did
// not answer `signInActivity`. It is best effort: a tenant without the activity
// feed consent, or without any sign-in record in the window, leaves the cells
// blank and never fails the users section.
func (d *reportData) loadLastLogins() {
	if d.lastLoginsLoaded {
		return
	}
	d.lastLoginsLoaded = true
	needed := false
	for _, user := range d.users {
		if signInActivityOf(user) == "" {
			needed = true
			break
		}
	}
	if !needed {
		// The tenant has Entra ID P1/P2 and every user answered the property: the
		// fallback would only repeat a more precise answer.
		return
	}
	if strings.TrimSpace(d.creds.ClientID) == "" || strings.TrimSpace(d.creds.ClientSecret) == "" {
		return
	}
	logins, err := d.provider.unifiedLastLogins(d.ctx, d.creds, d.tokens)
	if err != nil {
		d.lastLoginsErr = err
		slog.Debug("the last-login fallback did not answer", "error", err)
		return
	}
	d.lastLogins = logins
}

// lastLoginOf answers the last-login cell of one user: the precise
// `signInActivity` value when the tenant has it, the latest `UserLoggedIn` record
// of the audit window otherwise, and an empty cell when neither answered.
func (d *reportData) lastLoginOf(user reportUser) string {
	if value := signInActivityOf(user); value != "" {
		return value
	}
	for _, address := range []string{user.UserPrincipalName, user.Mail} {
		key := strings.ToLower(strings.TrimSpace(address))
		if key == "" {
			continue
		}
		if occurred, ok := d.lastLogins[key]; ok && !occurred.IsZero() {
			return occurred.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// signInActivityOf answers the most useful timestamp of the Graph property, most
// precise first: the last successful sign-in (interactive or non-interactive),
// then the last interactive attempt, then the last non-interactive attempt.
func signInActivityOf(user reportUser) string {
	if user.SignInActivity == nil {
		return ""
	}
	for _, value := range []string{
		user.SignInActivity.LastSuccessfulSignInDateTime,
		user.SignInActivity.LastSignInDateTime,
		user.SignInActivity.LastNonInteractiveSignInDateTime,
	} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// unifiedLastLogins reads the `UserLoggedIn` records of the last seven days — the
// window the Office 365 Management Activity API serves — and answers the newest
// one per address.
//
// The activity API is app-only: the delegated token of the report names the
// tenant, and the records are read with the application credentials of the
// instance. A tenant that did not grant ActivityFeed.Read answers 401/403, which
// is returned as the reason and leaves the cells blank.
func (p *Provider) unifiedLastLogins(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens) (map[string]time.Time, error) {
	tenant := p.OrgIDOf(tokens)
	if tenant == "" {
		tenant = strings.ToLower(strings.TrimSpace(creds.TenantID))
	}
	if !isTenantID(tenant) {
		return nil, fmt.Errorf("microsoft: the last-login lookup needs a tenant id (connect a work account or set MICROSOFT_TENANT_ID)")
	}
	if strings.TrimSpace(creds.ClientID) == "" || strings.TrimSpace(creds.ClientSecret) == "" {
		return nil, fmt.Errorf("microsoft: the last-login lookup needs the application credentials (client id and secret)")
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	token, err := p.appOnlyToken(ctx, tenant, creds)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-unifiedMaxWindows * unifiedWindow)
	logins := make(map[string]time.Time, 256)
	for window := 0; window < unifiedMaxWindows; window++ {
		from := start.Add(time.Duration(window) * unifiedWindow)
		if !from.Before(now) {
			break
		}
		to := from.Add(unifiedWindow)
		if to.After(now) {
			to = now
		}
		if err := p.startSubscription(ctx, tenant, token, lastLoginContentType); err != nil {
			return logins, err
		}
		uris, err := p.contentBlobs(ctx, tenant, token, lastLoginContentType, from, to)
		if err != nil {
			return logins, err
		}
		for _, uri := range uris {
			records, err := p.blobRecords(ctx, uri, token)
			if err != nil {
				return logins, err
			}
			for _, raw := range records {
				address, occurred, ok := lastLoginFromRecord(raw)
				if !ok {
					continue
				}
				if previous, exists := logins[address]; !exists || occurred.After(previous) {
					logins[address] = occurred
				}
			}
		}
	}
	return logins, nil
}

// lastLoginFromRecord answers the address and moment of one audit record when it
// is a successful `UserLoggedIn` event.
func lastLoginFromRecord(raw json.RawMessage) (string, time.Time, bool) {
	var record lastLoginRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return "", time.Time{}, false
	}
	if !strings.EqualFold(strings.TrimSpace(record.Operation), "UserLoggedIn") {
		return "", time.Time{}, false
	}
	status := strings.ToLower(strings.TrimSpace(record.ResultStatus))
	if status != "" && !strings.Contains(status, "success") {
		return "", time.Time{}, false
	}
	occurred, ok := parseUnifiedTime(record.CreationTime)
	if !ok || occurred.IsZero() {
		return "", time.Time{}, false
	}
	address := strings.ToLower(strings.TrimSpace(record.UserID))
	if !strings.Contains(address, "@") {
		address = strings.ToLower(strings.TrimSpace(record.ObjectID))
	}
	if !strings.Contains(address, "@") {
		return "", time.Time{}, false
	}
	return address, occurred, true
}
