package handlers

import (
	"net/http"
	"testing"

	"github.com/ivancarlosti/sync/internal/models"
)

// TestManagementOperationsRequireAnAccount pins the one required query parameter
// of the catalogue endpoint: without an account there is nothing to decorate.
func TestManagementOperationsRequireAnAccount(t *testing.T) {
	deps := testDepsWith(t, models.AuthModeNone, testOptions{})
	server, err := New(deps)
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}

	recorder := call(t, server, http.MethodGet, "/api/management/operations", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
}

// TestManagementStartRejectsAnUnknownKind pins that the start endpoint answers a
// clean 400 for a kind the catalogue does not offer, before touching an account.
func TestManagementStartRejectsAnUnknownKind(t *testing.T) {
	deps := testDepsWith(t, models.AuthModeNone, testOptions{})
	server, err := New(deps)
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}

	recorder := call(t, server, http.MethodPost, "/api/management/tasks", map[string]any{
		"account_id": 1,
		"kind":       "not-an-operation",
		"params":     map[string]string{},
	})
	requireStatus(t, recorder, http.StatusBadRequest)
}

// TestManagementTasksListIsEmptyAtBoot pins the history endpoint: a fresh
// instance answers an empty list, not an error.
func TestManagementTasksListIsEmptyAtBoot(t *testing.T) {
	deps := testDepsWith(t, models.AuthModeNone, testOptions{})
	server, err := New(deps)
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}

	recorder := call(t, server, http.MethodGet, "/api/management/tasks", nil)
	requireStatus(t, recorder, http.StatusOK)
	list := decodeJSON[struct {
		Tasks []managementTaskView `json:"tasks"`
	}](t, recorder)
	if len(list.Tasks) != 0 {
		t.Fatalf("tasks = %+v, want none", list.Tasks)
	}
}
