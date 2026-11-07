// This file resolves the person behind a workload-made directory change.
//
// Microsoft reports a great many changes as the work of an application: adding a
// device names "Device Registration Service", a policy refresh names the workload
// that ran it, a token issue names "Microsoft Authentication Library". The human
// the workload acted for is a property of the *object*, not of the record: the
// device carries the accounts registered as its owners, and one of them is the
// person who enrolled it.
//
// The read is a plain directory read of `/devices/{id}` with
// `$expand=registeredOwners`, which Graph authorises with `Device.Read.All`,
// `Device.ReadWrite.All`, `Directory.Read.All` or `Directory.ReadWrite.All`. This
// provider already holds `Directory.ReadWrite.All` (see permissions.go), so
// attribution needs no new consent and no reconnect — see plan
// 2026-11-07-audit-log-actor-attribution.md §1.1 for why adding a dedicated scope
// would break every existing account.
package microsoft

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

const (
	// devicesURL is the Graph device endpoint the owner read expands.
	devicesURL = baseURL + "/devices"
	// deviceOwnerLookups bounds the lookups one enrichment call may make. One
	// page of directory audits can hold hundreds of device registrations, and a
	// tenant-wide import would otherwise turn one collect pass into thousands of
	// extra Graph calls; what is not resolved this pass is resolved by the next
	// one (the repair pass keeps the rows that still need it).
	deviceOwnerLookups = 20
	// deviceOwnerTimeout bounds one lookup. It is short on purpose: an
	// unreachable Graph must not hold the collect pass, and the row stays
	// correctly attributed to the workload until the next pass.
	deviceOwnerTimeout = 20 * time.Second
)

// deviceOwnerScopes are the delegated scopes Graph accepts for reading
// `registeredOwners` of a device. Any one of them is enough, which is why the gate
// below asks for the scopes and not for a capability: the provider's own table only
// declares two of them, and a capability demands every permission it declares.
var deviceOwnerScopes = []string{
	graphScope("Directory.ReadWrite.All"),
	graphScope("Directory.Read.All"),
	graphScope("Device.Read.All"),
	graphScope("Device.ReadWrite.All"),
}

// canReadDeviceOwners reports whether the grant in hand authorises the owner read.
func canReadDeviceOwners(granted []string) bool {
	for _, scope := range deviceOwnerScopes {
		if providers.GrantsScope(granted, scope) {
			return true
		}
	}
	return false
}

// Compile time guard: the collector hands the enrichment to the registry as an
// interface, so a renamed method must fail the build.
var _ providers.ActivityActorEnricher = (*Provider)(nil)

// deviceOwner is the attribution recovered for one device.
type deviceOwner struct {
	// email is the address of the owner, empty when the device has none.
	email string
	// name is the display label of that owner.
	name string
}

// deviceObject is the trimmed answer of `GET /devices/{id}?$expand=registeredOwners`.
type deviceObject struct {
	ID               string `json:"id"`
	DeviceID         string `json:"deviceId"`
	DisplayName      string `json:"displayName"`
	RegisteredOwners []struct {
		ID                string `json:"id"`
		DisplayName       string `json:"displayName"`
		UserPrincipalName string `json:"userPrincipalName"`
	} `json:"registeredOwners"`
}

// EnrichActivityActors implements providers.ActivityActorEnricher: it names the
// person behind the workload-made device changes of one batch.
//
// Only events that still need it are looked up (a workload, a device target, no
// owner recovered from the record), the lookups of one device are made once, and
// nothing is guessed: a device with no registered owner, a directory that refuses
// the read or a device deleted since the event all leave the row attributed to the
// workload instead of to a stranger.
func (p *Provider) EnrichActivityActors(ctx context.Context, tokens *providers.Tokens, events []providers.ActivityEvent) int {
	if len(events) == 0 || tokens == nil || strings.TrimSpace(tokens.AccessToken) == "" {
		return 0
	}
	if !canReadDeviceOwners(grantedScopes(tokens)) {
		// The owner read is a directory read. Without that grant the columns
		// stay empty and the screen says so; nothing is guessed.
		return 0
	}
	memo := map[string]deviceOwner{}
	lookups, enriched := 0, 0
	for i := range events {
		event := &events[i]
		if !needsDeviceOwner(*event) {
			continue
		}
		key := strings.TrimSpace(event.TargetID)
		owner, known := memo[key]
		if !known {
			if lookups >= deviceOwnerLookups {
				continue
			}
			lookups++
			owner = p.deviceOwner(ctx, tokens.AccessToken, key)
			memo[key] = owner
		}
		if owner.email == "" {
			continue
		}
		event.ActorOnBehalfOf = owner.email
		event.ActorOnBehalfOfName = owner.name
		enriched++
	}
	return enriched
}

// needsDeviceOwner reports whether an event still needs the lookup: a workload
// changed a device and the record itself did not name the person behind it.
func needsDeviceOwner(event providers.ActivityEvent) bool {
	return event.ActorKind == models.AuditLogActorApplication &&
		event.Target == models.AuditLogTargetDevice &&
		strings.TrimSpace(event.ActorOnBehalfOf) == "" &&
		strings.TrimSpace(event.TargetID) != ""
}

// deviceOwner answers the first registered owner of a device. key is the object
// id of the device (Graph accepts the Entra device id in that position too).
func (p *Provider) deviceOwner(ctx context.Context, token, key string) deviceOwner {
	ctx, cancel := context.WithTimeout(ctx, deviceOwnerTimeout)
	defer cancel()
	query := url.Values{}
	query.Set("$select", "id,deviceId,displayName")
	query.Set("$expand", "registeredOwners($select=id,displayName,userPrincipalName)")
	target := devicesURL + "/" + url.PathEscape(key) + "?" + query.Encode()
	var device deviceObject
	if err := p.getJSON(ctx, target, token, &device); err != nil {
		// A device deleted since the event, a directory that refused the read or
		// a throttle: the row keeps its workload actor and a later pass tries
		// again. Logged at debug because it is an expected outcome, not a fault.
		slog.Debug("reading the registered owners of a device failed",
			"device_id", key, "error", err)
		return deviceOwner{}
	}
	return firstOwner(device)
}

// firstOwner picks the owner to attribute the change to. The record does not say
// which of several owners enrolled the device, so the first one with an address
// wins: attributing the change to an owner who did not make it would be a worse
// lie than naming the workload.
func firstOwner(device deviceObject) deviceOwner {
	for _, owner := range device.RegisteredOwners {
		email := strings.ToLower(strings.TrimSpace(owner.UserPrincipalName))
		if addressOf(email) == "" {
			continue
		}
		return deviceOwner{email: email, name: strings.TrimSpace(owner.DisplayName)}
	}
	return deviceOwner{}
}

// grantedScopes reads the scopes the access token in hand actually carries. The
// `scp` claim of the token is authoritative and survives a refresh that returned
// no `scope` value; the stored grant is the fallback for a token that cannot be
// read (see TokenManager.tokens).
func grantedScopes(tokens *providers.Tokens) []string {
	if scopes := accessTokenScopes(tokens.AccessToken); len(scopes) > 0 {
		return scopes
	}
	return tokens.Scopes
}

// accessTokenScopes decodes the `scp` claim of a Graph access token. A token that
// is not a readable JWT answers nil, which makes the caller fall back to the
// stored grant.
func accessTokenScopes(token string) []string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Scope string `json:"scp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return strings.Fields(claims.Scope)
}
