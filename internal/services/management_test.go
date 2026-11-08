package services

import (
	"context"
	"errors"
	"testing"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// TestManagementCatalogueIsWellFormed guards the invariants the screen and the
// start path rely on: every kind is unique, belongs to a real provider, carries a
// capability that provider speaks, names a probe, and every destructive operation
// asks for the explicit confirmation parameter.
func TestManagementCatalogueIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, op := range managementOperations {
		if op.Kind == "" {
			t.Fatal("an operation has an empty kind")
		}
		if seen[op.Kind] {
			t.Fatalf("kind %q is declared twice", op.Kind)
		}
		seen[op.Kind] = true

		if !op.Provider.Valid() {
			t.Fatalf("%s: provider %q is not a real provider", op.Kind, op.Provider)
		}
		if op.provide == nil {
			t.Fatalf("%s: no provider probe", op.Kind)
		}
		// The capability must be part of the provider's vocabulary, otherwise a
		// reconnect hint would name something its directory cannot grant.
		known := false
		for _, capability := range providers.CapabilitiesFor(op.Provider) {
			if capability == op.Capability {
				known = true
				break
			}
		}
		if !known {
			t.Fatalf("%s: capability %q is outside the vocabulary of %s", op.Kind, op.Capability, op.Provider)
		}

		confirm := false
		for _, param := range op.Params {
			if param.Key == "confirm" {
				confirm = true
			}
		}
		if op.Danger && !confirm {
			t.Fatalf("%s is destructive but does not ask for confirmation", op.Kind)
		}
	}
}

// TestManagementStartRejectsBadInput pins the validation that runs before any
// account is resolved: an unknown kind, a missing required parameter and a
// destructive operation without confirmation are all clean validation errors.
func TestManagementStartRejectsBadInput(t *testing.T) {
	service := NewManagementService(nil, nil, nil)
	ctx := context.Background()

	if _, err := service.Start(ctx, ManagementInput{AccountID: 1, Kind: "not-an-operation"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("Start(unknown kind) = %v, want a validation error", err)
	}

	// google_archive_user_group requires user_email and group_email.
	if _, err := service.Start(ctx, ManagementInput{
		AccountID: 1,
		Kind:      models.ManagementKindArchiveUserGroup,
		Params:    map[string]string{"user_email": "a@example.com", "confirm": "true"},
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("Start(missing group_email) = %v, want a validation error", err)
	}

	// The same operation, complete but unconfirmed.
	if _, err := service.Start(ctx, ManagementInput{
		AccountID: 1,
		Kind:      models.ManagementKindArchiveUserGroup,
		Params:    map[string]string{"user_email": "a@example.com", "group_email": "g@example.com"},
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("Start(unconfirmed destructive op) = %v, want a validation error", err)
	}

	if _, err := service.Start(ctx, ManagementInput{AccountID: 0, Kind: models.ManagementKindFolderSizes}); !errors.Is(err, ErrValidation) {
		t.Fatalf("Start(no account) = %v, want a validation error", err)
	}
}

// TestManagementHelpers pins the two small decisions the start path makes: the
// confirmation spellings accepted and the status mapping of an outcome.
func TestManagementHelpers(t *testing.T) {
	for _, value := range []string{"true", "1", "yes", "on", " TRUE "} {
		if !confirmTrue(value) {
			t.Fatalf("confirmTrue(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "false", "0", "no", "nope"} {
		if confirmTrue(value) {
			t.Fatalf("confirmTrue(%q) = true, want false", value)
		}
	}

	ctx := context.Background()
	if status := managementStatus(ctx, nil, 0); status != models.RunSuccess {
		t.Fatalf("status = %q, want success", status)
	}
	if status := managementStatus(ctx, nil, 2); status != models.RunPartial {
		t.Fatalf("status = %q, want partial", status)
	}
	if status := managementStatus(ctx, errors.New("boom"), 0); status != models.RunFailed {
		t.Fatalf("status = %q, want failed", status)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if status := managementStatus(cancelled, cancelled.Err(), 0); status != models.RunCancelled {
		t.Fatalf("status = %q, want cancelled", status)
	}
}
