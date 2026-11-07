// This file covers the owner lookup that gives a workload-made device change the
// person behind it (deviceowners.go): the grant it rides on, the deduplication of
// the lookups, the bound of one pass, and the rule that a refusal degrades the row
// instead of failing it.

package microsoft

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// accessTokenWithScopes builds a token whose `scp` claim carries the granted
// delegated scopes, which is what the owner lookup gates on.
func accessTokenWithScopes(scopes string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"scp":"` + scopes + `"}`))
	return header + "." + payload + ".signature"
}

// deviceEvent builds a normalised event of a workload-changed device.
func deviceEvent(deviceID string) providers.ActivityEvent {
	return providers.ActivityEvent{
		Source:     streamDirectory,
		RemoteID:   "Directory_" + deviceID,
		Action:     models.AuditLogUpdate,
		Target:     models.AuditLogTargetDevice,
		ActorKind:  models.AuditLogActorApplication,
		ActorName:  "Device Registration Service",
		TargetName: "DESKTOP-J979VG0",
		TargetID:   deviceID,
	}
}

// deviceOwnersBody is the Graph answer for one device: the owners a registration
// can be attributed to.
const deviceOwnersBody = `{"id":"device-object-id","deviceId":"device-entra-id",` +
	`"displayName":"DESKTOP-J979VG0","registeredOwners":[` +
	`{"id":"user-1","displayName":"Ivan Almeida","userPrincipalName":"Ivan@Contoso.com"}]}`

// TestEnrichActivityActorsNamesTheOwner pins the whole path of the lookup: a
// workload-changed device gets the registered owner, read from `/devices/{id}` with
// the grant the provider already holds.
func TestEnrichActivityActorsNamesTheOwner(t *testing.T) {
	transport := &recordingTransport{bodies: map[string]string{"/v1.0/devices/device-object-id": deviceOwnersBody}}
	provider := New()
	provider.httpClient.Transport = transport
	tokens := &providers.Tokens{AccessToken: accessTokenWithScopes(
		"Files.ReadWrite.All Directory.ReadWrite.All User.Read")}

	events := []providers.ActivityEvent{deviceEvent("device-object-id")}
	if enriched := provider.EnrichActivityActors(context.Background(), tokens, events); enriched != 1 {
		t.Fatalf("EnrichActivityActors() = %d, want the one device attributed", enriched)
	}
	if events[0].ActorOnBehalfOf != "ivan@contoso.com" || events[0].ActorOnBehalfOfName != "Ivan Almeida" {
		t.Errorf("on behalf of = %q/%q, want the registered owner",
			events[0].ActorOnBehalfOf, events[0].ActorOnBehalfOfName)
	}
	if events[0].ActorName != "Device Registration Service" || events[0].ActorKind != models.AuditLogActorApplication {
		t.Errorf("the workload actor was overwritten: %+v", events[0])
	}
	if len(transport.urls) != 1 {
		t.Fatalf("the lookup issued %d requests, want 1", len(transport.urls))
	}
}

// TestEnrichActivityActorsNeedsTheDirectoryGrant pins the gate: no directory read
// in the grant means no call at all, and the row keeps the workload as its actor.
func TestEnrichActivityActorsNeedsTheDirectoryGrant(t *testing.T) {
	transport := &recordingTransport{bodies: map[string]string{"/v1.0/devices/device-object-id": deviceOwnersBody}}
	provider := New()
	provider.httpClient.Transport = transport
	tokens := &providers.Tokens{AccessToken: accessTokenWithScopes("Files.ReadWrite.All")}

	events := []providers.ActivityEvent{deviceEvent("device-object-id")}
	if enriched := provider.EnrichActivityActors(context.Background(), tokens, events); enriched != 0 {
		t.Fatalf("EnrichActivityActors() = %d, want none without the directory grant", enriched)
	}
	if len(transport.urls) != 0 {
		t.Errorf("the lookup issued %d requests without the grant, want none", len(transport.urls))
	}
	if events[0].ActorOnBehalfOf != "" {
		t.Errorf("ActorOnBehalfOf = %q, want it left empty", events[0].ActorOnBehalfOf)
	}
}

// TestEnrichActivityActorsOnlyLooksUpWhatItNeeds pins the three skips of a pass: a
// person does not need an owner, an event whose record already named one does not
// either, and two events of the same device cost one lookup.
func TestEnrichActivityActorsOnlyLooksUpWhatItNeeds(t *testing.T) {
	transport := &recordingTransport{bodies: map[string]string{"/v1.0/devices/device-object-id": deviceOwnersBody}}
	provider := New()
	provider.httpClient.Transport = transport
	tokens := &providers.Tokens{AccessToken: accessTokenWithScopes("Directory.ReadWrite.All")}

	person := deviceEvent("device-object-id")
	person.ActorKind, person.ActorName = models.AuditLogActorUser, "Ada Admin"
	person.ActorEmail = "ada@contoso.com"
	known := deviceEvent("device-object-id")
	known.ActorOnBehalfOf = "maria@contoso.com"
	first := deviceEvent("device-object-id")
	second := deviceEvent("device-object-id")

	events := []providers.ActivityEvent{person, known, first, second}
	if enriched := provider.EnrichActivityActors(context.Background(), tokens, events); enriched != 2 {
		t.Fatalf("EnrichActivityActors() = %d, want the two device events attributed", enriched)
	}
	if len(transport.urls) != 1 {
		t.Errorf("the pass issued %d requests, want one per distinct device", len(transport.urls))
	}
	if events[0].ActorOnBehalfOf != "" {
		t.Errorf("a person was given an owner: %q", events[0].ActorOnBehalfOf)
	}
	if events[1].ActorOnBehalfOf != "maria@contoso.com" {
		t.Errorf("the owner of the record was overwritten: %q", events[1].ActorOnBehalfOf)
	}
}

// TestEnrichActivityActorsBoundsTheLookups pins the bound of one pass: a page of
// device registrations cannot turn into an unbounded number of Graph calls.
func TestEnrichActivityActorsBoundsTheLookups(t *testing.T) {
	transport := &recordingTransport{bodies: map[string]string{}}
	provider := New()
	provider.httpClient.Transport = transport
	tokens := &providers.Tokens{AccessToken: accessTokenWithScopes("Directory.ReadWrite.All")}

	events := make([]providers.ActivityEvent, 0, deviceOwnerLookups+5)
	for i := 0; i < deviceOwnerLookups+5; i++ {
		events = append(events, deviceEvent("device-"+string(rune('a'+i))))
	}
	provider.EnrichActivityActors(context.Background(), tokens, events)
	if len(transport.urls) != deviceOwnerLookups {
		t.Errorf("the pass issued %d requests, want the documented bound of %d",
			len(transport.urls), deviceOwnerLookups)
	}
}

// TestEnrichActivityActorsSurvivesARefusedRead pins the rule that attribution is
// an improvement and never a failure: a device deleted since the event answers 404
// and the row simply keeps its workload actor.
func TestEnrichActivityActorsSurvivesARefusedRead(t *testing.T) {
	transport := &recordingTransport{
		statuses: map[string]int{"/v1.0/devices/gone-device": http.StatusNotFound},
	}
	provider := New()
	provider.httpClient.Transport = transport
	tokens := &providers.Tokens{AccessToken: accessTokenWithScopes("Directory.ReadWrite.All")}

	events := []providers.ActivityEvent{deviceEvent("gone-device")}
	if enriched := provider.EnrichActivityActors(context.Background(), tokens, events); enriched != 0 {
		t.Errorf("EnrichActivityActors() = %d, want none for a device that no longer exists", enriched)
	}
	if events[0].ActorOnBehalfOf != "" || events[0].ActorName != "Device Registration Service" {
		t.Errorf("event = %+v, want the workload actor untouched", events[0])
	}
}

// TestGrantedScopesReadsTheClaim pins the grant reader: the `scp` claim wins over
// the stored grant, and an unreadable token falls back to it.
func TestGrantedScopesReadsTheClaim(t *testing.T) {
	tokens := &providers.Tokens{
		AccessToken: accessTokenWithScopes("Directory.Read.All Files.ReadWrite.All"),
		Scopes:      []string{"stored.scope"},
	}
	scopes := grantedScopes(tokens)
	if len(scopes) != 2 || scopes[0] != "Directory.Read.All" {
		t.Errorf("grantedScopes() = %v, want the scopes of the token", scopes)
	}
	opaque := &providers.Tokens{AccessToken: "opaque-token", Scopes: []string{"stored.scope"}}
	if scopes := grantedScopes(opaque); len(scopes) != 1 || scopes[0] != "stored.scope" {
		t.Errorf("grantedScopes() = %v, want the stored grant as the fallback", scopes)
	}
}
