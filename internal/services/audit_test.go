package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// auditEnv bundles what an audit test needs: a store, the token manager, the
// audit service and a fake Microsoft provider standing in for a SharePoint /
// shared drive.
type auditEnv struct {
	store   *Store
	tokens  *TokenManager
	audit   *AuditService
	msft    *fakeProvider
	account *models.ConnectedAccount
}

// newAuditEnv builds the fixture with one connected Microsoft account.
func newAuditEnv(t *testing.T) *auditEnv {
	t.Helper()
	cfg := testConfig(t)
	cfg.Microsoft = config.ProviderCredentials{
		ClientID:     "microsoft-id",
		ClientSecret: "microsoft-secret",
		RedirectURI:  cfg.AppURL + "/api/oauth/microsoft/callback",
	}
	store := newTestStore(t)
	box := NewSecretBox(cfg.EncryptionKeyBytes())
	msft := newFakeProvider(models.ProviderMicrosoft)
	registry := providers.NewRegistry(msft)
	env := &auditEnv{store: store, msft: msft}

	access, err := box.Encrypt("access-microsoft")
	if err != nil {
		t.Fatalf("encrypting the access token: %v", err)
	}
	refresh, err := box.Encrypt("refresh-microsoft")
	if err != nil {
		t.Fatalf("encrypting the refresh token: %v", err)
	}
	account := &models.ConnectedAccount{
		Provider:          string(models.ProviderMicrosoft),
		ProviderAccountID: "account-microsoft",
		Email:             "microsoft@example.test",
		DisplayName:       "microsoft",
		AccessToken:       access,
		RefreshToken:      refresh,
		TokenType:         "Bearer",
		ExpiresAt:         time.Now().UTC().Add(time.Hour),
		Status:            string(models.AccountConnected),
	}
	if err := store.SaveAccount(context.Background(), account); err != nil {
		t.Fatalf("saving the account: %v", err)
	}
	env.account = account
	env.tokens = NewTokenManager(store, registry, NewProviderSettings(cfg, store.Settings(), box), box)
	env.audit = NewAuditService(store, env.tokens)
	t.Cleanup(func() {
		env.audit.Cancel(account.ID)
		env.audit.Wait()
	})
	return env
}

// waitAudit polls until the audit leaves the running state.
func (e *auditEnv) waitAudit(t *testing.T, id uint) *models.AuditRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := e.store.GetAudit(context.Background(), id)
		if err != nil {
			t.Fatalf("loading the audit: %v", err)
		}
		if run.Status != string(models.RunRunning) {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("the audit did not finish: %+v", run)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// seedTree builds the tree every walk test uses:
//
//	README.md            (2 bytes)
//	docs/notes.md        (11 bytes)
//	docs/img/a.png       (3 bytes)
//	empty/               (empty folder)
func seedTree(t *testing.T, env *auditEnv) {
	t.Helper()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	env.msft.add("README.md", "hi", base)
	env.msft.add("docs/notes.md", "hello world", base)
	env.msft.add("docs/img/a.png", "xyz", base)
	env.msft.folder(providers.DriveRoot, "empty")
}

// entriesByPath indexes a report by relative path.
func entriesByPath(t *testing.T, store *Store, id uint) map[string]models.AuditEntry {
	t.Helper()
	rows, err := store.ListAuditEntries(context.Background(), id, AuditNodeLimit)
	if err != nil {
		t.Fatalf("listing the entries: %v", err)
	}
	byPath := make(map[string]models.AuditEntry, len(rows))
	for _, row := range rows {
		byPath[row.Path] = row
	}
	return byPath
}

// TestAuditWalksTree checks the summary, the DFS preorder and the subtree
// aggregates of a full (unlimited depth) report.
func TestAuditWalksTree(t *testing.T) {
	env := newAuditEnv(t)
	seedTree(t, env)

	run, err := env.audit.Start(context.Background(), AuditInput{AccountID: env.account.ID})
	if err != nil {
		t.Fatalf("starting the audit: %v", err)
	}
	finished := env.waitAudit(t, run.ID)
	if finished.Status != string(models.RunSuccess) {
		t.Fatalf("status = %q (%s)", finished.Status, finished.Message)
	}
	if finished.Files != 3 || finished.Folders != 4 || finished.TotalSize != 16 {
		t.Fatalf("summary = files %d, folders %d, size %d; want 3, 4, 16",
			finished.Files, finished.Folders, finished.TotalSize)
	}
	if finished.MaxDepthReached != 3 {
		t.Fatalf("max depth = %d, want 3", finished.MaxDepthReached)
	}
	if finished.Truncated {
		t.Fatal("a small tree must not be truncated")
	}

	rows, err := env.store.ListAuditEntries(context.Background(), run.ID, AuditNodeLimit)
	if err != nil {
		t.Fatalf("listing the entries: %v", err)
	}
	wantOrder := []string{"", "docs", "docs/img", "docs/img/a.png", "docs/notes.md", "empty", "README.md"}
	if len(rows) != len(wantOrder) {
		t.Fatalf("entries = %d, want %d (%+v)", len(rows), len(wantOrder), rows)
	}
	for i, want := range wantOrder {
		if rows[i].Path != want {
			t.Fatalf("entry %d = %q, want %q", i, rows[i].Path, want)
		}
	}

	byPath := entriesByPath(t, env.store, run.ID)
	if got := byPath[""]; got.Kind != string(models.AuditKindFolder) || got.TotalSize != 16 || got.Files != 3 || got.Folders != 3 {
		t.Fatalf("root entry = %+v, want a folder with 16 bytes, 3 files, 3 folders", got)
	}
	if got := byPath["docs"]; got.TotalSize != 14 || got.Files != 2 || got.Folders != 1 || !got.Expanded {
		t.Fatalf("docs entry = %+v, want 14 bytes, 2 files, 1 folder, expanded", got)
	}
	if got := byPath["docs/img"]; got.TotalSize != 3 || got.Files != 1 || got.Folders != 0 {
		t.Fatalf("docs/img entry = %+v, want 3 bytes, 1 file, 0 folders", got)
	}
	if got := byPath["README.md"]; got.Kind != string(models.AuditKindFile) || got.Size != 2 {
		t.Fatalf("README.md entry = %+v, want a 2 byte file", got)
	}
	if got := byPath["empty"]; !got.Expanded || got.Files != 0 {
		t.Fatalf("empty entry = %+v, want an expanded empty folder", got)
	}
}

// TestAuditDepthBoundary checks that a folder at the requested depth is reported
// but not descended into.
func TestAuditDepthBoundary(t *testing.T) {
	env := newAuditEnv(t)
	seedTree(t, env)

	run, err := env.audit.Start(context.Background(), AuditInput{AccountID: env.account.ID, Depth: 1})
	if err != nil {
		t.Fatalf("starting the audit: %v", err)
	}
	finished := env.waitAudit(t, run.ID)
	if finished.Files != 1 || finished.Folders != 3 || finished.TotalSize != 2 {
		t.Fatalf("summary = files %d, folders %d, size %d; want 1, 3, 2",
			finished.Files, finished.Folders, finished.TotalSize)
	}
	if finished.MaxDepthReached != 1 {
		t.Fatalf("max depth = %d, want 1", finished.MaxDepthReached)
	}
	byPath := entriesByPath(t, env.store, run.ID)
	for _, boundary := range []string{"docs", "empty"} {
		got, ok := byPath[boundary]
		if !ok {
			t.Fatalf("%q was not reported at the boundary", boundary)
		}
		if got.Expanded || got.TotalSize != 0 || got.Files != 0 {
			t.Fatalf("%q at the boundary = %+v, want not expanded with a zero subtree", boundary, got)
		}
	}
	if _, ok := byPath["docs/notes.md"]; ok {
		t.Fatal("a file below the boundary must not be reported")
	}
}

// TestAuditorEmitHonoursNodeLimit checks the node budget without building a
// 10001 node tree: the auditor emits exactly AuditNodeLimit entries and then
// flags the report truncated.
func TestAuditorEmitHonoursNodeLimit(t *testing.T) {
	a := &auditor{service: NewAuditService(nil, nil), run: &models.AuditRun{ID: 1}}
	for i := 0; i < AuditNodeLimit; i++ {
		if !a.emit(models.AuditEntry{Kind: string(models.AuditKindFile), Path: "f"}) {
			t.Fatalf("emit #%d reported the budget spent too early", i)
		}
	}
	if a.emit(models.AuditEntry{}) {
		t.Fatal("emit accepted a node past the limit")
	}
	if !a.truncated {
		t.Fatal("the report must be flagged truncated once the budget is spent")
	}
	if len(a.entries) != AuditNodeLimit {
		t.Fatalf("entries = %d, want %d", len(a.entries), AuditNodeLimit)
	}
}

// TestAuditValidation checks the start request refuses a negative depth and an
// unknown account before any row is written.
func TestAuditValidation(t *testing.T) {
	env := newAuditEnv(t)
	if _, err := env.audit.Start(context.Background(), AuditInput{AccountID: env.account.ID, Depth: -1}); !errors.Is(err, ErrValidation) {
		t.Fatalf("negative depth: err = %v, want ErrValidation", err)
	}
	if _, err := env.audit.Start(context.Background(), AuditInput{AccountID: 4242}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account: err = %v, want ErrNotFound", err)
	}
}

// TestAuditBusyAndCancel keeps a walk in flight with a blocking provider,
// checks a second audit of the account is refused and then cancels it.
func TestAuditBusyAndCancel(t *testing.T) {
	env := newAuditEnv(t)
	seedTree(t, env)
	env.msft.block = make(chan struct{}) // never closed: the walk stalls

	run, err := env.audit.Start(context.Background(), AuditInput{AccountID: env.account.ID})
	if err != nil {
		t.Fatalf("starting the audit: %v", err)
	}
	if !env.audit.Running(env.account.ID) {
		t.Fatal("the account must be marked as being audited")
	}
	if _, err := env.audit.Start(context.Background(), AuditInput{AccountID: env.account.ID}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second audit: err = %v, want ErrBusy", err)
	}
	if !env.audit.Cancel(env.account.ID) {
		t.Fatal("Cancel reported nothing to cancel")
	}
	finished := env.waitAudit(t, run.ID)
	if finished.Status != string(models.RunCancelled) {
		t.Fatalf("status = %q (%s), want cancelled", finished.Status, finished.Message)
	}
	if env.audit.Running(env.account.ID) {
		t.Fatal("the reservation must be released once the audit finished")
	}
}
