package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/database"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// -----------------------------------------------------------------------------
// Test fixtures
// -----------------------------------------------------------------------------

// newTestStore opens a private in-memory SQLite database, migrates the real
// schema and wraps it in a Store, so the sync engine is exercised against the
// same tables it uses in production.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := "file:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating the test database: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return NewStore(db, database.NewSettings(db))
}

// fakeItem is one entry of the in-memory tree of a fakeProvider.
type fakeItem struct {
	item     providers.Item
	parentID string
	body     []byte
}

// fakeProvider is the in-memory Provider used by the engine tests. It models one
// drive whose root is providers.DriveRoot, which is enough to drive a full run:
// listing, downloading, uploading, creating folders and deleting.
type fakeProvider struct {
	name    models.ProviderName
	items   map[string]*fakeItem
	counter int
	calls   []string
	failing error
	// denied, when set, makes the write operations (upload, folder creation and
	// deletion) answer a 401 until the next Refresh. It models a token that died
	// or was revoked while a run was already in flight.
	denied bool
	// refreshes counts the Refresh calls, so a test can assert the engine renewed
	// a token mid-run without depending on how many files it copied.
	refreshes int
	// block, when set, holds every listing until it is closed or the context of
	// the run is cancelled: it is how a test keeps a run in flight on purpose.
	block chan struct{}
}

// newFakeProvider builds a provider with an empty root folder.
func newFakeProvider(name models.ProviderName) *fakeProvider {
	root := providers.Item{ID: providers.DriveRoot, Name: providers.DriveRoot, IsDir: true}
	return &fakeProvider{
		name:  name,
		items: map[string]*fakeItem{providers.DriveRoot: {item: root}},
	}
}

// add puts a file or folder under path ("docs/notes.md"); missing folders are
// created on the way so a test can seed a tree in one call.
func (p *fakeProvider) add(path string, body string, modified time.Time) *fakeItem {
	parent := providers.DriveRoot
	segments := strings.Split(path, "/")
	for _, name := range append([]string{}, segments[:len(segments)-1]...) {
		parent = p.folder(parent, name).item.ID
	}
	return p.file(parent, segments[len(segments)-1], body, modified)
}

// folder returns the folder called name inside parentID, creating it if needed.
func (p *fakeProvider) folder(parentID, name string) *fakeItem {
	for _, item := range p.items {
		if item.item.IsDir && item.parentID == parentID && item.item.Name == name {
			return item
		}
	}
	p.counter++
	item := &fakeItem{
		item:     providers.Item{ID: id(p.name, p.counter), Name: name, IsDir: true, ParentID: parentID},
		parentID: parentID,
	}
	p.items[item.item.ID] = item
	return item
}

// file stores a new file inside parentID.
func (p *fakeProvider) file(parentID, name, body string, modified time.Time) *fakeItem {
	p.counter++
	content := []byte(body)
	item := &fakeItem{
		item: providers.Item{
			ID:         id(p.name, p.counter),
			Name:       name,
			ParentID:   parentID,
			Size:       int64(len(content)),
			ModifiedAt: modified,
			MimeType:   "text/plain",
			Hash:       fakeHash(content),
		},
		parentID: parentID,
		body:     content,
	}
	p.items[item.item.ID] = item
	return item
}

// fakeHash derives a content hash the way a real provider exposes one: two files
// with the same body share it, two different bodies do not.
func fakeHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:8])
}

// paths lists every file of the tree as "relative/path" -> body, which is what
// the assertions of the end-to-end tests compare against.
func (p *fakeProvider) paths() map[string]string {
	out := map[string]string{}
	var walk func(parentID, prefix string)
	walk = func(parentID, prefix string) {
		for _, item := range p.items {
			if item.parentID != parentID || item.item.ID == providers.DriveRoot {
				continue
			}
			path := joinPath(prefix, item.item.Name)
			if item.item.IsDir {
				walk(item.item.ID, path)
				continue
			}
			out[path] = string(item.body)
		}
	}
	walk(providers.DriveRoot, "")
	return out
}

// resolve returns the item behind a relative path ("" is the root), or nil.
func (p *fakeProvider) resolve(path string) *fakeItem {
	current := p.items[providers.DriveRoot]
	if path == "" {
		return current
	}
	for _, segment := range strings.Split(path, "/") {
		found := (*fakeItem)(nil)
		for _, item := range p.items {
			if item.parentID == current.item.ID && item.item.Name == segment {
				found = item
				break
			}
		}
		if found == nil {
			return nil
		}
		current = found
	}
	return current
}

// remove deletes a path from the tree, which is how a test simulates an edit
// made directly in the cloud.
func (p *fakeProvider) remove(path string) {
	if item := p.resolve(path); item != nil {
		delete(p.items, item.item.ID)
	}
}

// update rewrites the body, the hash and the modification time of a file.
func (p *fakeProvider) update(path, body string, modified time.Time) {
	item := p.resolve(path)
	if item == nil {
		return
	}
	item.body = []byte(body)
	item.item.Size = int64(len(item.body))
	item.item.Hash = fakeHash(item.body)
	item.item.ModifiedAt = modified
}

// id builds a stable item id per provider.
func id(name models.ProviderName, counter int) string {
	return fmt.Sprintf("%s-%d", name, counter)
}

// fakeAuthError is the 401 a fakeProvider answers with while `denied` is set. It
// carries the provider name so the engine's "which side rejected this?" check
// resolves to the side that actually refused the call.
type fakeAuthError struct {
	provider models.ProviderName
}

// Error renders the shape of a real rejected-token answer.
func (e fakeAuthError) Error() string {
	return string(e.provider) + ": http 401 (authError): Invalid Credentials"
}

// auth returns the rejected-token error while the provider is denying writes.
func (p *fakeProvider) auth() error {
	if p.denied {
		return fakeAuthError{provider: p.name}
	}
	return nil
}

// wait is the optional blocking hook of the fake provider: with block set, a
// listing waits until the channel is closed or the context of the run ends.
func (p *fakeProvider) wait(ctx context.Context) error {
	if p.block == nil {
		return nil
	}
	select {
	case <-p.block:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// -----------------------------------------------------------------------------
// fakeProvider: providers.Provider
// -----------------------------------------------------------------------------

func (p *fakeProvider) Name() models.ProviderName { return p.name }

func (p *fakeProvider) Scopes() []string { return []string{"files.readwrite"} }

func (p *fakeProvider) AuthCodeURL(creds providers.Credentials, state, codeChallenge string) string {
	return "https://example.test/authorize?state=" + state + "&challenge=" + codeChallenge
}

func (p *fakeProvider) Exchange(ctx context.Context, creds providers.Credentials, code, codeVerifier string) (*providers.Tokens, error) {
	return p.tokens(), nil
}

func (p *fakeProvider) Refresh(ctx context.Context, creds providers.Credentials, refreshToken string) (*providers.Tokens, error) {
	p.refreshes++
	if p.failing != nil {
		return nil, p.failing
	}
	// A successful renewal clears the rejection, exactly like a real provider
	// starts accepting a fresh access token.
	p.denied = false
	return p.tokens(), nil
}

// tokens returns a token set that is valid for an hour.
func (p *fakeProvider) tokens() *providers.Tokens {
	return &providers.Tokens{
		AccessToken:  "access-" + string(p.name),
		RefreshToken: "refresh-" + string(p.name),
		TokenType:    "Bearer",
		Expiry:       time.Now().UTC().Add(time.Hour),
		Scopes:       p.Scopes(),
	}
}

func (p *fakeProvider) Account(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens) (*providers.Account, error) {
	return &providers.Account{ID: "account-" + string(p.name), Email: string(p.name) + "@example.test"}, nil
}

func (p *fakeProvider) Drives(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens) ([]providers.Drive, error) {
	return []providers.Drive{{ID: providers.DriveRoot, Name: string(p.name), Kind: "personal"}}, nil
}

func (p *fakeProvider) Children(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, driveID, folderID string) ([]providers.Item, error) {
	p.calls = append(p.calls, "children:"+folderID)
	if err := p.wait(ctx); err != nil {
		return nil, err
	}
	parent := p.items[folderID]
	if parent == nil {
		return nil, fmt.Errorf("folder %s: %w", folderID, providers.ErrNotFound)
	}
	children := make([]providers.Item, 0, len(p.items))
	for _, item := range p.items {
		if item.parentID == folderID && item.item.ID != providers.DriveRoot {
			children = append(children, item.item)
		}
	}
	return children, nil
}

func (p *fakeProvider) Item(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, driveID, itemID string) (*providers.Item, error) {
	item, ok := p.items[itemID]
	if !ok {
		return nil, fmt.Errorf("item %s: %w", itemID, providers.ErrNotFound)
	}
	return &item.item, nil
}

func (p *fakeProvider) Download(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, driveID, itemID string) (*providers.Transfer, error) {
	p.calls = append(p.calls, "download:"+itemID)
	item, ok := p.items[itemID]
	if !ok {
		return nil, fmt.Errorf("item %s: %w", itemID, providers.ErrNotFound)
	}
	return &providers.Transfer{
		Body:       io.NopCloser(bytes.NewReader(item.body)),
		Size:       item.item.Size,
		MimeType:   item.item.MimeType,
		ModifiedAt: item.item.ModifiedAt,
		Hash:       item.item.Hash,
	}, nil
}

func (p *fakeProvider) Upload(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, req providers.UploadRequest) (*providers.Item, error) {
	if err := p.auth(); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	p.calls = append(p.calls, "upload:"+req.Name)
	modified := req.ModifiedAt
	if modified.IsZero() {
		modified = time.Now().UTC()
	}
	if req.ExistingID != "" {
		item, ok := p.items[req.ExistingID]
		if !ok {
			return nil, fmt.Errorf("item %s: %w", req.ExistingID, providers.ErrNotFound)
		}
		item.body = body
		item.item.Name = req.Name
		item.item.Size = int64(len(body))
		item.item.Hash = fakeHash(body)
		item.item.ModifiedAt = modified
		item.item.MimeType = req.MimeType
		return &item.item, nil
	}
	stored := p.file(req.ParentID, req.Name, string(body), modified)
	stored.item.Size = int64(len(body))
	stored.item.MimeType = req.MimeType
	return &stored.item, nil
}

func (p *fakeProvider) CreateFolder(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, driveID, parentID, name string) (*providers.Item, error) {
	if err := p.auth(); err != nil {
		return nil, err
	}
	p.calls = append(p.calls, "create_folder:"+name)
	if p.items[parentID] == nil {
		return nil, fmt.Errorf("folder %s: %w", parentID, providers.ErrNotFound)
	}
	folder := p.folder(parentID, name)
	return &folder.item, nil
}

func (p *fakeProvider) Delete(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, driveID, itemID string) error {
	if err := p.auth(); err != nil {
		return err
	}
	p.calls = append(p.calls, "delete:"+itemID)
	item, ok := p.items[itemID]
	if !ok {
		return fmt.Errorf("item %s: %w", itemID, providers.ErrNotFound)
	}
	delete(p.items, item.item.ID)
	return nil
}

func (p *fakeProvider) IsNotFound(err error) bool { return errors.Is(err, providers.ErrNotFound) }

// IsUnauthorized reports whether err is this provider's rejected-token error.
func (p *fakeProvider) IsUnauthorized(err error) bool {
	var authErr fakeAuthError
	return errors.As(err, &authErr) && authErr.provider == p.name
}

// -----------------------------------------------------------------------------
// Engine fixture
// -----------------------------------------------------------------------------

// testEnv bundles everything an end-to-end engine test needs: a database, two
// fake providers registered as Google and Microsoft, and a SyncService.
type testEnv struct {
	cfg    *config.Config
	store  *Store
	tokens *TokenManager
	box    *SecretBox
	sync   *SyncService
	google *fakeProvider
	msft   *fakeProvider
	job    *models.SyncJob
}

// newTestEnv builds the fixture with both accounts connected and one job that
// copies Google → Microsoft (bidirectional when the direction says so).
func newTestEnv(t *testing.T, direction models.SyncDirection, policy models.ConflictPolicy) *testEnv {
	t.Helper()
	ctx := context.Background()

	cfg := testConfig(t)
	cfg.Google = config.ProviderCredentials{ClientID: "google-id", ClientSecret: "google-secret",
		RedirectURI: cfg.AppURL + "/api/oauth/google/callback"}
	cfg.Microsoft = config.ProviderCredentials{ClientID: "microsoft-id", ClientSecret: "microsoft-secret",
		RedirectURI: cfg.AppURL + "/api/oauth/microsoft/callback"}

	store := newTestStore(t)
	box := NewSecretBox(cfg.EncryptionKeyBytes())

	google := newFakeProvider(models.ProviderGoogle)
	msft := newFakeProvider(models.ProviderMicrosoft)
	registry := providers.NewRegistry(google, msft)

	env := &testEnv{
		cfg:    cfg,
		store:  store,
		box:    box,
		google: google,
		msft:   msft,
	}
	env.tokens = NewTokenManager(store, registry, NewProviderSettings(cfg, store.Settings(), box), box)

	googleAccount := env.connect(t, models.ProviderGoogle)
	msftAccount := env.connect(t, models.ProviderMicrosoft)

	job := &models.SyncJob{
		Name:                 "google to microsoft",
		SourceAccountID:      googleAccount.ID,
		DestinationAccountID: msftAccount.ID,
		SourceDriveID:        providers.DriveRoot,
		SourceFolderID:       providers.DriveRoot,
		DestinationDriveID:   providers.DriveRoot,
		DestinationFolderID:  providers.DriveRoot,
		Direction:            string(direction),
		ConflictPolicy:       string(policy),
		Enabled:              true,
		IntervalMinutes:      0,
	}
	if strings.TrimSpace(job.Direction) == "" {
		job.Direction = string(models.DirectionGoogleToMicrosoft)
	}
	if err := store.SaveJob(ctx, job); err != nil {
		t.Fatalf("saving the job: %v", err)
	}
	env.job = job
	env.sync = NewSyncService(store, env.tokens, nil, nil)
	return env
}

// connect stores a connected account with encrypted tokens for a fake provider.
func (e *testEnv) connect(t *testing.T, name models.ProviderName) *models.ConnectedAccount {
	t.Helper()
	access, err := e.box.Encrypt("access-" + string(name))
	if err != nil {
		t.Fatalf("encrypting the access token: %v", err)
	}
	refresh, err := e.box.Encrypt("refresh-" + string(name))
	if err != nil {
		t.Fatalf("encrypting the refresh token: %v", err)
	}
	account := &models.ConnectedAccount{
		Provider:          string(name),
		ProviderAccountID: "account-" + string(name),
		Email:             string(name) + "@example.test",
		DisplayName:       string(name),
		AccessToken:       access,
		RefreshToken:      refresh,
		TokenType:         "Bearer",
		ExpiresAt:         time.Now().UTC().Add(time.Hour),
		Status:            string(models.AccountConnected),
	}
	if err := e.store.SaveAccount(context.Background(), account); err != nil {
		t.Fatalf("saving the %s account: %v", name, err)
	}
	return account
}

// run executes the job synchronously and returns the finished run row.
func (e *testEnv) run(t *testing.T) *models.SyncRun {
	t.Helper()
	run, err := e.sync.Run(context.Background(), e.job, models.TriggerManual)
	if err != nil {
		t.Fatalf("running the job: %v", err)
	}
	if run.Status != string(models.RunSuccess) {
		t.Fatalf("run status = %q (%s), counters %+v", run.Status, run.Message, run)
	}
	return run
}

// items returns the run items of a run, ordered by action then path.
func (e *testEnv) items(t *testing.T, runID uint) []models.SyncItem {
	t.Helper()
	items, err := e.store.ListRunItems(context.Background(), runID, 100)
	if err != nil {
		t.Fatalf("listing the run items: %v", err)
	}
	return items
}

// actionOf returns the action recorded for a path, or "" when the run did not
// touch it.
func actionOf(items []models.SyncItem, path string) string {
	for _, item := range items {
		if item.Path == path {
			return item.Action
		}
	}
	return ""
}

// statePaths returns the paths of the state table of the job.
func (e *testEnv) statePaths(t *testing.T) map[string]models.SyncFile {
	t.Helper()
	state, err := e.store.FileState(context.Background(), e.job.ID)
	if err != nil {
		t.Fatalf("loading the file state: %v", err)
	}
	return state
}

// saveJob persists the job row after a test changed one of its fields.
func (e *testEnv) saveJob(t *testing.T) {
	t.Helper()
	if err := e.store.SaveJob(context.Background(), e.job); err != nil {
		t.Fatalf("saving the job: %v", err)
	}
}

// latestRun returns the newest run of the job.
func (e *testEnv) latestRun(t *testing.T) *models.SyncRun {
	t.Helper()
	runs, err := e.store.ListRuns(context.Background(), e.job.ID, 1)
	if err != nil {
		t.Fatalf("listing the runs: %v", err)
	}
	if len(runs) == 0 {
		t.Fatal("the job has no run")
	}
	return &runs[0]
}

// tree is the whole destination tree as "relative/path" -> body.
func (e *testEnv) tree() map[string]string {
	return e.msft.paths()
}

// assertTree fails the test unless the destination tree matches the expectation.
func assertTree(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()
	for path, body := range want {
		if got[path] != body {
			t.Errorf("destination %q = %q, want %q", path, got[path], body)
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			t.Errorf("destination %q must not exist (%q)", path, got[path])
		}
	}
}

// countCalls counts the recorded provider calls with a given prefix, which is how
// the tests prove that a run did not transfer anything.
func countCalls(calls []string, prefix string) int {
	count := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

// -----------------------------------------------------------------------------
// Pure engine logic
// -----------------------------------------------------------------------------

// newUnitEngine builds a bare engine for the decision tables: no database and no
// provider is touched by decide(), matchPattern() or status().
func newUnitEngine(direction models.SyncDirection, policy models.ConflictPolicy, deleteMissing bool) *engine {
	e := &engine{
		service: NewSyncService(nil, nil, nil, nil),
		job: &models.SyncJob{ID: 1, Enabled: true, Direction: string(direction),
			ConflictPolicy: string(policy), DeleteMissing: deleteMissing},
		run:    &models.SyncRun{ID: 1},
		state:  map[string]models.SyncFile{},
		source: &endpoint{provider: newFakeProvider(models.ProviderGoogle), files: map[string]providers.Item{}},
		target: &endpoint{provider: newFakeProvider(models.ProviderMicrosoft), files: map[string]providers.Item{}},
	}
	e.applyDirections()
	return e
}

// file builds a remote item for the decision tables.
func file(id, hash string, size int64, modified time.Time) *providers.Item {
	return &providers.Item{ID: id, Name: "a.txt", Size: size, Hash: hash, ModifiedAt: modified}
}

func TestMatchPattern(t *testing.T) {
	cases := []struct {
		pattern  string
		relative string
		want     bool
	}{
		{"*.tmp", "cache.tmp", true},
		{"*.tmp", "docs/notes.tmp", true},
		{"*.tmp", "notes.txt", false},
		{"cache/**", "cache", true},
		{"cache/**", "cache/2026/a.bin", true},
		{"cache/**", "other/cache/a.bin", false},
		// `**/x` matches the name at any depth, including the root itself.
		{"**/node_modules", "web/node_modules", true},
		{"**/node_modules", "node_modules", true},
		{"**/node_modules", "web/dist", false},
		{"docs/*.bak", "docs/old.bak", true},
		{"docs/*.bak", "docs/2026/old.bak", false},
		{"secret.txt", "deep/secret.txt", true},
	}
	for _, tc := range cases {
		if got := matchPattern(tc.pattern, tc.relative); got != tc.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", tc.pattern, tc.relative, got, tc.want)
		}
	}
}

func TestExcluded(t *testing.T) {
	e := newUnitEngine(models.DirectionBidirectional, models.ConflictNewestWins, false)
	e.excludes = []string{"*.tmp", "cache/**"}
	cases := map[string]bool{
		"a.tmp":          true,
		"docs/a.tmp":     true,
		"cache":          true,
		"cache/x/y.bin":  true,
		"docs/notes.txt": false,
	}
	for relative, want := range cases {
		if got := e.excluded(relative); got != want {
			t.Errorf("excluded(%q) = %v, want %v", relative, got, want)
		}
	}
}

func TestPathHelpers(t *testing.T) {
	if got := joinPath("", "a.txt"); got != "a.txt" {
		t.Errorf("joinPath root = %q", got)
	}
	if got := joinPath("docs", "a.txt"); got != "docs/a.txt" {
		t.Errorf("joinPath nested = %q", got)
	}
	if got := dirOf("docs/2026/a.txt"); got != "docs/2026" {
		t.Errorf("dirOf nested = %q", got)
	}
	if got := dirOf("a.txt"); got != "" {
		t.Errorf("dirOf root = %q", got)
	}
	if got := displayPath(""); got != "(root)" {
		t.Errorf("displayPath root = %q", got)
	}
}

func TestItemChanged(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	item := providers.Item{ID: "id-1", Size: 10, ModifiedAt: base}
	if itemChanged(item, "id-1", base, 10) {
		t.Error("an identical item must not count as changed")
	}
	if !itemChanged(item, "id-2", base, 10) {
		t.Error("a renewed id must count as changed")
	}
	if !itemChanged(item, "id-1", base, 11) {
		t.Error("a different size must count as changed")
	}
	if itemChanged(item, "id-1", base.Add(time.Second), 10) {
		t.Error("a timestamp inside the tolerance must not count as changed")
	}
	if !itemChanged(item, "id-1", base.Add(10*time.Second), 10) {
		t.Error("a timestamp outside the tolerance must count as changed")
	}
	if itemChanged(item, "", time.Time{}, 0) {
		t.Error("an unknown state must not count as changed")
	}
}

// TestDecide walks the decision table of the engine: every combination of
// presence, tracked state and change that a run can meet.
func TestDecide(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	later := base.Add(time.Hour)
	tracked := models.SyncFile{ID: 7, JobID: 1, Path: "a.txt", Size: 5,
		SourceItemID: "s1", DestinationItemID: "d1",
		SourceModifiedAt: base, DestinationModifiedAt: base}

	type want struct {
		action            models.ItemAction
		upload            bool
		download          bool
		deleteSource      bool
		deleteDestination bool
		conflict          bool
	}
	cases := []struct {
		name          string
		direction     models.SyncDirection
		policy        models.ConflictPolicy
		deleteMissing bool
		src           *providers.Item
		dst           *providers.Item
		state         models.SyncFile
		want          want
	}{
		{
			name: "new file on the source is copied forward",
			src:  file("s2", "h2", 8, later),
			want: want{action: models.ActionCreated, upload: true},
		},
		{
			name: "new file on the destination is left alone by a forward-only job",
			dst:  file("d2", "h2", 8, later),
			want: want{action: models.ActionInSync},
		},
		{
			name:      "new file on the destination is copied back in bidirectional jobs",
			direction: models.DirectionBidirectional,
			dst:       file("d2", "h2", 8, later),
			want:      want{action: models.ActionCreated, download: true},
		},
		{
			name:  "both copies unchanged rest",
			src:   file("s1", "h1", 5, base),
			dst:   file("d1", "h1", 5, base),
			state: tracked,
			want:  want{action: models.ActionInSync},
		},
		{
			name: "an untracked pair of the same size rests",
			src:  file("s2", "", 5, base),
			dst:  file("d2", "", 5, base),
			want: want{action: models.ActionInSync},
		},
		{
			name:  "the changed source propagates forward",
			src:   file("s1", "h2", 9, later),
			dst:   file("d1", "h1", 5, base),
			state: tracked,
			want:  want{action: models.ActionUpdated, upload: true},
		},
		{
			name:      "a changed source is ignored by a backward-only job",
			direction: models.DirectionMicrosoftToGoogle,
			src:       file("s1", "h2", 9, later),
			dst:       file("d1", "h1", 5, base),
			state:     tracked,
			want:      want{action: models.ActionInSync},
		},
		{
			name:  "the changed destination is ignored by a forward-only job",
			src:   file("s1", "h1", 5, base),
			dst:   file("d1", "h2", 9, later),
			state: tracked,
			want:  want{action: models.ActionInSync},
		},
		{
			name:      "the changed destination propagates back in bidirectional jobs",
			direction: models.DirectionBidirectional,
			src:       file("s1", "h1", 5, base),
			dst:       file("d1", "h2", 9, later),
			state:     tracked,
			want:      want{action: models.ActionUpdated, download: true},
		},
		{
			name:      "two identical edits are not a conflict",
			direction: models.DirectionBidirectional,
			src:       file("s1", "same", 5, later),
			dst:       file("d1", "same", 5, later),
			state:     tracked,
			want:      want{action: models.ActionInSync},
		},
		{
			name:      "two different edits follow newest_wins",
			direction: models.DirectionBidirectional,
			src:       file("s1", "h2", 9, later),
			dst:       file("d1", "h3", 7, base),
			state:     tracked,
			want:      want{action: models.ActionUpdated, upload: true, conflict: true},
		},
		{
			name:      "two different edits keep both copies with the skip policy",
			direction: models.DirectionBidirectional,
			policy:    models.ConflictSkip,
			src:       file("s1", "h2", 9, later),
			dst:       file("d1", "h3", 7, base),
			state:     tracked,
			want:      want{action: models.ActionConflict, conflict: true},
		},
		{
			name:          "the deleted destination is propagated in bidirectional jobs",
			direction:     models.DirectionBidirectional,
			deleteMissing: true,
			src:           file("s1", "h1", 5, base),
			state:         tracked,
			want:          want{action: models.ActionDeleted, deleteSource: true},
		},
		{
			name:      "the deleted destination is restored forward when delete missing is off",
			direction: models.DirectionBidirectional,
			src:       file("s1", "h1", 5, base),
			state:     tracked,
			want:      want{action: models.ActionCreated, upload: true},
		},
		{
			name:          "the deleted source is propagated forward",
			deleteMissing: true,
			dst:           file("d1", "h1", 5, base),
			state:         tracked,
			want:          want{action: models.ActionDeleted, deleteDestination: true},
		},
		{
			name:      "the deleted source is restored backward when delete missing is off",
			direction: models.DirectionBidirectional,
			dst:       file("d1", "h1", 5, base),
			state:     tracked,
			want:      want{action: models.ActionCreated, download: true},
		},
		{
			name:          "a source removed after a destination edit is a conflict",
			deleteMissing: true,
			dst:           file("d1", "h9", 9, later),
			state:         tracked,
			want:          want{action: models.ActionConflict, conflict: true},
		},
		{
			name:      "a file only on the source of a backward-only job is skipped",
			direction: models.DirectionMicrosoftToGoogle,
			src:       file("s2", "h2", 8, later),
			want:      want{action: models.ActionSkipped},
		},
		{
			name:  "a tracked file only on the destination is skipped without delete missing",
			dst:   file("d1", "h1", 5, base),
			state: tracked,
			want:  want{action: models.ActionSkipped},
		},
	}

	for _, tc := range cases {
		direction := tc.direction
		if direction == "" {
			direction = models.DirectionGoogleToMicrosoft
		}
		policy := tc.policy
		if policy == "" {
			policy = models.ConflictNewestWins
		}
		e := newUnitEngine(direction, policy, tc.deleteMissing)
		got := e.decide("a.txt", tc.src, tc.dst, tc.state)
		if got.action != tc.want.action || got.upload != tc.want.upload || got.download != tc.want.download ||
			got.deleteSource != tc.want.deleteSource || got.deleteDestination != tc.want.deleteDestination ||
			got.conflict != tc.want.conflict {
			t.Errorf("%s: decide() = %+v, want %+v", tc.name, got, tc.want)
		}
		if got.evidence == "" {
			t.Errorf("%s: every decision must carry an explanation", tc.name)
		}
	}
}

// TestResolveConflictPolicies pins the four conflict policies and the way an
// illegal policy for the direction of a job degrades to "keep both copies".
func TestResolveConflictPolicies(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	newer := base.Add(30 * time.Minute)

	cases := []struct {
		name      string
		direction models.SyncDirection
		policy    models.ConflictPolicy
		src, dst  *providers.Item
		want      models.ItemAction
		conflict  bool
		upload    bool
		download  bool
	}{
		{name: "newest wins forward", direction: models.DirectionGoogleToMicrosoft,
			policy: models.ConflictNewestWins,
			src:    file("s", "a", 3, newer), dst: file("d", "b", 4, base),
			want: models.ActionUpdated, conflict: true, upload: true},
		{name: "newest wins backward", direction: models.DirectionBidirectional,
			policy: models.ConflictNewestWins,
			src:    file("s", "a", 3, base), dst: file("d", "b", 4, newer),
			want: models.ActionUpdated, conflict: true, download: true},
		{name: "identical timestamps keep both", direction: models.DirectionBidirectional,
			policy: models.ConflictNewestWins,
			src:    file("s", "a", 3, base), dst: file("d", "b", 4, base),
			want: models.ActionConflict, conflict: true},
		{name: "unknown timestamps keep both", direction: models.DirectionBidirectional,
			policy: models.ConflictNewestWins,
			src:    file("s", "a", 3, time.Time{}), dst: file("d", "b", 4, newer),
			want: models.ActionConflict, conflict: true},
		{name: "source wins", direction: models.DirectionBidirectional,
			policy: models.ConflictSourceWins,
			src:    file("s", "a", 3, base), dst: file("d", "b", 4, newer),
			want: models.ActionUpdated, conflict: true, upload: true},
		{name: "source wins is illegal backward", direction: models.DirectionMicrosoftToGoogle,
			policy: models.ConflictSourceWins,
			src:    file("s", "a", 3, base), dst: file("d", "b", 4, newer),
			want: models.ActionConflict, conflict: true},
		{name: "destination wins", direction: models.DirectionMicrosoftToGoogle,
			policy: models.ConflictDestinationWins,
			src:    file("s", "a", 3, base), dst: file("d", "b", 4, newer),
			want: models.ActionUpdated, conflict: true, download: true},
		{name: "destination wins is illegal forward", direction: models.DirectionGoogleToMicrosoft,
			policy: models.ConflictDestinationWins,
			src:    file("s", "a", 3, newer), dst: file("d", "b", 4, base),
			want: models.ActionConflict, conflict: true},
		{name: "skip keeps both", direction: models.DirectionBidirectional,
			policy: models.ConflictSkip,
			src:    file("s", "a", 3, newer), dst: file("d", "b", 4, base),
			want: models.ActionConflict, conflict: true},
	}

	for _, tc := range cases {
		e := newUnitEngine(tc.direction, tc.policy, false)
		got := e.resolveConflict(tc.src, tc.dst, "test")
		if got.action != tc.want || got.conflict != tc.conflict || got.upload != tc.upload || got.download != tc.download {
			t.Errorf("%s: resolveConflict() = %+v, want action=%s conflict=%v upload=%v download=%v",
				tc.name, got, tc.want, tc.conflict, tc.upload, tc.download)
		}
		if got.evidence == "" {
			t.Errorf("%s: the decision must carry an explanation", tc.name)
		}
	}
}

// TestEngineStatus pins how the counters of a run become its final status.
func TestEngineStatus(t *testing.T) {
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	cases := []struct {
		name   string
		ctx    context.Context
		fatal  error
		errors int
		want   models.RunStatus
	}{
		{name: "clean run", ctx: ctx, want: models.RunSuccess},
		{name: "per file failures", ctx: ctx, errors: 2, want: models.RunPartial},
		{name: "fatal failure", ctx: ctx, fatal: errors.New("boom"), want: models.RunFailed},
		{name: "cancelled", ctx: cancelled, want: models.RunCancelled},
		{name: "deadline", ctx: ctx, fatal: context.DeadlineExceeded, want: models.RunTimeout},
	}
	for _, tc := range cases {
		e := &engine{run: &models.SyncRun{Errors: tc.errors}, fatal: tc.fatal}
		if got := e.status(tc.ctx); got != tc.want {
			t.Errorf("%s: status() = %q, want %q", tc.name, got, tc.want)
		}
	}

	e := &engine{run: &models.SyncRun{}, fatal: errors.New("boom")}
	if e.status(cancelled) != models.RunCancelled || e.run.Message == "" {
		t.Errorf("a cancelled run must be reported as cancelled with a message (%q)", e.run.Message)
	}

	// A run that ran out of time is a timeout, not a cancellation, and carries a
	// friendly message instead of the raw "context deadline exceeded" text.
	timedOut, cancelTimeout := context.WithTimeout(ctx, 0)
	defer cancelTimeout()
	<-timedOut.Done()
	timeout := &engine{run: &models.SyncRun{Message: "the run was cancelled: context deadline exceeded"}}
	if got := timeout.status(timedOut); got != models.RunTimeout {
		t.Errorf("status() = %q, want %q", got, models.RunTimeout)
	}
	if timeout.run.Message == "" || timeout.run.Message == "the run was cancelled: context deadline exceeded" {
		t.Errorf("a timed out run must carry a friendly message, got %q", timeout.run.Message)
	}
}

// -----------------------------------------------------------------------------
// End to end runs
// -----------------------------------------------------------------------------

// TestEngineFirstRunCopiesTree drives one full run against two in-memory
// providers: the folders of the source are created on the destination as they
// are needed, the excluded and the unsupported files are reported and skipped.
func TestEngineFirstRunCopiesTree(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	env := newTestEnv(t, models.DirectionGoogleToMicrosoft, models.ConflictNewestWins)

	env.google.add("report.txt", "hello world", base)
	env.google.add("docs/notes.md", "# notes", base)
	env.google.add("docs/2026/budget.csv", "a,b\n1,2", base)
	env.google.add("cache.tmp", "junk", base)
	native := env.google.add("plan.gdoc", "", base)
	native.item.NativeDoc = true

	env.job.ExcludePatterns = `["*.tmp"]`
	env.saveJob(t)

	run := env.run(t)
	if run.FilesScanned != 3 {
		t.Errorf("FilesScanned = %d, want 3", run.FilesScanned)
	}
	if run.FilesCreated != 3 {
		t.Errorf("FilesCreated = %d, want 3", run.FilesCreated)
	}
	if run.FoldersCreated != 2 {
		t.Errorf("FoldersCreated = %d, want 2 (docs and docs/2026)", run.FoldersCreated)
	}
	if run.FilesSkipped != 2 {
		t.Errorf("FilesSkipped = %d, want 2 (the *.tmp file and the native document)", run.FilesSkipped)
	}
	wantBytes := int64(len("hello world") + len("# notes") + len("a,b\n1,2"))
	if run.BytesTransferred != wantBytes {
		t.Errorf("BytesTransferred = %d, want %d", run.BytesTransferred, wantBytes)
	}

	assertTree(t, env.tree(), map[string]string{
		"report.txt":           "hello world",
		"docs/notes.md":        "# notes",
		"docs/2026/budget.csv": "a,b\n1,2",
	})

	items := env.items(t, run.ID)
	if action := actionOf(items, "report.txt"); action != string(models.ActionCreated) {
		t.Errorf("the action of report.txt = %q, want created", action)
	}
	if action := actionOf(items, "docs"); action != string(models.ActionFolderCreated) {
		t.Errorf("the action of the docs folder = %q, want folder_created", action)
	}
	if action := actionOf(items, "plan.gdoc"); action != string(models.ActionUnsupported) {
		t.Errorf("the action of the native document = %q, want unsupported", action)
	}
	if action := actionOf(items, "cache.tmp"); action != "" {
		t.Errorf("an excluded file must not appear in the run items (got %q)", action)
	}

	state := env.statePaths(t)
	if len(state) != 3 {
		t.Fatalf("the state table holds %d rows, want 3", len(state))
	}
	row := state["docs/2026/budget.csv"]
	if row.SourceItemID == "" || row.DestinationItemID == "" || row.SourceItemID == row.DestinationItemID {
		t.Errorf("the state row of a copied file must keep both remote ids: %+v", row)
	}
	if row.LastDirection != "source_to_destination" || row.Status != string(models.RunSuccess) {
		t.Errorf("unexpected state row: %+v", row)
	}
}

// TestEngineRecoversFromMidRun401 covers the fix for a long run outliving the
// access token it loaded at its start: when a side answers 401, the engine renews
// the token once and retries, so the files after the expiry still transfer
// instead of failing with an opaque error.
func TestEngineRecoversFromMidRun401(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	env := newTestEnv(t, models.DirectionGoogleToMicrosoft, models.ConflictNewestWins)
	env.google.add("report.txt", "hello world", base)
	env.google.add("docs/notes.md", "# notes", base)

	// The destination token dies at the start of the run: every write is
	// rejected with a 401 until the engine renews the token.
	env.msft.denied = true

	run := env.run(t)
	if run.Errors != 0 {
		t.Fatalf("the run recorded %d errors, want 0: the 401 must be recovered (%s)", run.Errors, run.Message)
	}
	if env.msft.refreshes == 0 {
		t.Fatal("the engine never renewed the rejected token")
	}
	if run.FilesCreated != 2 {
		t.Errorf("FilesCreated = %d, want 2", run.FilesCreated)
	}
	assertTree(t, env.tree(), map[string]string{
		"report.txt":    "hello world",
		"docs/notes.md": "# notes",
	})
}

// TestEngineStopsWhenReconnectIsNeeded covers the fail-fast side of the fix: a 401
// that a renewal cannot clear (the account really has to be reconnected) ends the
// run after the first file, instead of recording the same error for every
// remaining file the way the unpatched engine did.
func TestEngineStopsWhenReconnectIsNeeded(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	env := newTestEnv(t, models.DirectionGoogleToMicrosoft, models.ConflictNewestWins)
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"} {
		env.google.add(name, "body "+name, base)
	}

	// The destination rejects every write and refuses to renew the token: the
	// account needs to be connected again.
	env.msft.denied = true
	env.msft.failing = errors.New("invalid_grant: the token has been revoked")

	run, err := env.sync.Run(context.Background(), env.job, models.TriggerManual)
	if err != nil {
		t.Fatalf("running the job: %v", err)
	}
	if run.Status != string(models.RunFailed) {
		t.Fatalf("run status = %q (%s), want failed", run.Status, run.Message)
	}
	if run.Errors != 1 {
		t.Errorf("run recorded %d errors, want 1: the run must stop once the account needs reconnecting", run.Errors)
	}
	if run.FilesScanned > 1 {
		t.Errorf("FilesScanned = %d, want at most 1", run.FilesScanned)
	}
	if !strings.Contains(run.Message, "reconnect") {
		t.Errorf("run message %q does not mention the needed reconnection", run.Message)
	}
	if env.msft.resolve("a.txt") != nil {
		t.Error("no file must be copied once the token could not be renewed")
	}
}

// TestEngineSecondRunIsIdle verifies that a run over an unchanged tree does not
// touch anything: it is the point of the state table.
func TestEngineSecondRunIsIdle(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	env := newTestEnv(t, models.DirectionGoogleToMicrosoft, models.ConflictNewestWins)
	env.google.add("report.txt", "hello world", base)
	env.google.add("docs/notes.md", "# notes", base)

	if run := env.run(t); run.FilesCreated != 2 {
		t.Fatalf("the first run created %d files, want 2", run.FilesCreated)
	}
	before := env.tree()
	downloads := countCalls(env.google.calls, "download:")
	uploads := countCalls(env.msft.calls, "upload:")

	run := env.run(t)
	if run.FilesCreated != 0 || run.FilesUpdated != 0 || run.FilesDeleted != 0 || run.FilesSkipped != 2 {
		t.Errorf("the second run must be a no-op: %+v", run)
	}
	if run.FilesScanned != 2 {
		t.Errorf("FilesScanned = %d, want 2", run.FilesScanned)
	}
	assertTree(t, env.tree(), before)
	if got := countCalls(env.google.calls, "download:"); got != downloads {
		t.Errorf("the second run downloaded %d files, want none", got-downloads)
	}
	if got := countCalls(env.msft.calls, "upload:"); got != uploads {
		t.Errorf("the second run uploaded %d files, want none", got-uploads)
	}
}

// TestEnginePropagatesSourceUpdate checks that a file edited on the source is
// overwritten in place on the destination.
func TestEnginePropagatesSourceUpdate(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	env := newTestEnv(t, models.DirectionGoogleToMicrosoft, models.ConflictNewestWins)
	env.google.add("report.txt", "first version", base)
	env.run(t)

	if env.msft.resolve("report.txt") == nil {
		t.Fatal("the first run did not copy the file")
	}
	env.google.update("report.txt", "second version, longer", base.Add(time.Hour))

	run := env.run(t)
	if run.FilesUpdated != 1 || run.FilesCreated != 0 {
		t.Errorf("the update run = %+v, want one update", run)
	}
	assertTree(t, env.tree(), map[string]string{"report.txt": "second version, longer"})
	row := env.statePaths(t)["report.txt"]
	if row.Size != int64(len("second version, longer")) {
		t.Errorf("the state row was not refreshed: %+v", row)
	}
}

// TestEnginePropagatesDeletion covers the delete-missing option: a file removed
// on the source is removed on the destination and forgotten.
func TestEnginePropagatesDeletion(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	env := newTestEnv(t, models.DirectionBidirectional, models.ConflictNewestWins)
	env.job.DeleteMissing = true
	env.saveJob(t)
	env.google.add("report.txt", "kept", base)
	env.google.add("docs/notes.md", "# notes", base)
	env.run(t)

	env.google.remove("docs/notes.md")
	run := env.run(t)
	if run.FilesDeleted != 1 {
		t.Errorf("FilesDeleted = %d, want 1", run.FilesDeleted)
	}
	assertTree(t, env.tree(), map[string]string{"report.txt": "kept"})
	if _, ok := env.statePaths(t)["docs/notes.md"]; ok {
		t.Error("the state row of a deleted file must be forgotten")
	}
	if action := actionOf(env.items(t, run.ID), "docs/notes.md"); action != string(models.ActionDeleted) {
		t.Errorf("the action of the deleted file = %q, want deleted", action)
	}
}

// TestEngineRestoresDeletionWithoutFlag verifies that the default is to restore
// a file deleted on the source instead of deleting the surviving copy.
func TestEngineRestoresDeletionWithoutFlag(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	env := newTestEnv(t, models.DirectionBidirectional, models.ConflictNewestWins)
	env.google.add("report.txt", "kept", base)
	env.run(t)

	env.google.remove("report.txt")
	run := env.run(t)
	if run.FilesDeleted != 0 {
		t.Errorf("a file deleted on the source must not be deleted on the destination: %+v", run)
	}
	// The state row still exists, so the copy back over the surviving item is
	// counted as an update even though the plan was to recreate the source file.
	if run.FilesCreated+run.FilesUpdated != 1 {
		t.Errorf("the file must be restored from the destination: %+v", run)
	}
	if action := actionOf(env.items(t, run.ID), "report.txt"); action != string(models.ActionCreated) {
		t.Errorf("the action of the restored file = %q, want created", action)
	}
	if _, ok := env.google.paths()["report.txt"]; !ok {
		t.Error("the file was not restored on the source")
	}
	if got := env.msft.paths()["report.txt"]; got != "kept" {
		t.Errorf("the destination copy = %q, want the surviving version", got)
	}
}

// TestEngineCopiesBackFromDestination covers the reverse flow of a bidirectional
// job, including the folder that only exists on the destination.
func TestEngineCopiesBackFromDestination(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	env := newTestEnv(t, models.DirectionBidirectional, models.ConflictNewestWins)
	env.msft.add("shared/logo.png", "png", base)

	run := env.run(t)
	if run.FilesCreated != 1 || run.FoldersCreated != 1 {
		t.Errorf("the reverse copy = %+v, want one file and one folder", run)
	}
	if source := env.google.paths(); source["shared/logo.png"] != "png" {
		t.Errorf("the source tree = %v", source)
	}
	row := env.statePaths(t)["shared/logo.png"]
	if row.LastDirection != "destination_to_source" {
		t.Errorf("the state row must record the reverse direction: %+v", row)
	}
}

// TestEngineConflictKeepsBothCopies pins the skip policy on conflicting edits.
func TestEngineConflictKeepsBothCopies(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	env := newTestEnv(t, models.DirectionBidirectional, models.ConflictSkip)
	env.google.add("report.txt", "original", base)
	env.run(t)

	env.google.update("report.txt", "the source version", base.Add(time.Hour))
	env.msft.update("report.txt", "the destination version", base.Add(2*time.Hour))

	run := env.run(t)
	if run.Conflicts != 1 {
		t.Errorf("Conflicts = %d, want 1", run.Conflicts)
	}
	if run.FilesCreated != 0 || run.FilesUpdated != 0 || run.FilesDeleted != 0 {
		t.Errorf("a conflicting run must not transfer anything: %+v", run)
	}
	if action := actionOf(env.items(t, run.ID), "report.txt"); action != string(models.ActionConflict) {
		t.Errorf("the action of the conflicting file = %q, want conflict", action)
	}
	if env.google.paths()["report.txt"] != "the source version" {
		t.Error("the source copy was overwritten")
	}
	if env.msft.paths()["report.txt"] != "the destination version" {
		t.Error("the destination copy was overwritten")
	}
}

// TestEngineConflictNewestWins checks the default policy on the same setup.
func TestEngineConflictNewestWins(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	env := newTestEnv(t, models.DirectionBidirectional, models.ConflictNewestWins)
	env.google.add("report.txt", "original", base)
	env.run(t)

	env.google.update("report.txt", "older edit", base.Add(30*time.Minute))
	env.msft.update("report.txt", "newer edit", base.Add(time.Hour))

	run := env.run(t)
	if run.Conflicts != 1 || run.FilesUpdated != 1 {
		t.Errorf("the newest copy must win: %+v", run)
	}
	if got := env.google.paths()["report.txt"]; got != "newer edit" {
		t.Errorf("the source copy = %q, want the newest version", got)
	}
}

// -----------------------------------------------------------------------------
// Run lifecycle
// -----------------------------------------------------------------------------

// TestSyncServiceRunLifecycle covers the asynchronous path: a run in flight
// refuses a second start, reports itself as running and can be cancelled.
func TestSyncServiceRunLifecycle(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t, models.DirectionGoogleToMicrosoft, models.ConflictNewestWins)
	env.google.add("report.txt", "hello", time.Now().UTC())

	env.google.block = make(chan struct{})
	run, err := env.sync.Start(ctx, env.job.ID, models.TriggerManual)
	if err != nil {
		t.Fatalf("starting the run: %v", err)
	}
	if run.Status != string(models.RunRunning) {
		t.Errorf("a started run must be reported as running, got %q", run.Status)
	}
	if !env.sync.Running(env.job.ID) {
		t.Error("the job must be reported as running")
	}
	if _, err := env.sync.Start(ctx, env.job.ID, models.TriggerManual); !errors.Is(err, ErrBusy) {
		t.Errorf("a second start = %v, want ErrBusy", err)
	}
	if _, err := env.store.RunningRun(ctx, env.job.ID); err != nil {
		t.Errorf("the run row must be visible as running: %v", err)
	}

	if !env.sync.Cancel(env.job.ID) {
		t.Error("cancelling a running job must report true")
	}
	env.sync.Wait()

	finished := env.latestRun(t)
	if finished.Status != string(models.RunCancelled) {
		t.Errorf("the cancelled run = %s (%s)", finished.Status, finished.Message)
	}
	if finished.FinishedAt == nil || finished.Message == "" {
		t.Errorf("a cancelled run must be closed with a message: %+v", finished)
	}
	if env.sync.Running(env.job.ID) || len(env.sync.RunningJobs()) != 0 {
		t.Error("the job must not be reported as running once it finished")
	}
	if env.sync.Cancel(env.job.ID) {
		t.Error("cancelling an idle job must report false")
	}

	// A new run must be possible once the previous one released the job.
	env.google.block = nil
	if _, err := env.sync.Run(ctx, env.job, models.TriggerManual); err != nil {
		t.Fatalf("a run after a cancellation failed: %v", err)
	}
	if got := env.latestRun(t).Status; got != string(models.RunSuccess) {
		t.Errorf("the run after a cancellation = %q, want success", got)
	}
}

// TestSyncServiceRefusesDisabledJob pins the validation of reserve().
func TestSyncServiceRefusesDisabledJob(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t, models.DirectionGoogleToMicrosoft, models.ConflictNewestWins)
	env.job.Enabled = false

	if _, err := env.sync.Run(ctx, env.job, models.TriggerManual); !errors.Is(err, ErrValidation) {
		t.Errorf("running a disabled job = %v, want ErrValidation", err)
	}
	if _, err := env.sync.Run(ctx, nil, models.TriggerManual); !errors.Is(err, ErrValidation) {
		t.Errorf("running a nil job = %v, want ErrValidation", err)
	}
}

// -----------------------------------------------------------------------------
// Scheduler
// -----------------------------------------------------------------------------

// TestNextRunAt pins the pure scheduling helper.
func TestNextRunAt(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	if next := NextRunAt(now, nil); next != nil {
		t.Errorf("NextRunAt(nil) = %v, want nil", next)
	}
	scheduled := &models.SyncJob{Enabled: true, IntervalMinutes: 30}
	next := NextRunAt(now, scheduled)
	if next == nil || !next.Equal(now.Add(30*time.Minute)) {
		t.Errorf("NextRunAt = %v, want %v", next, now.Add(30*time.Minute))
	}
	if next := NextRunAt(now, &models.SyncJob{Enabled: false, IntervalMinutes: 30}); next != nil {
		t.Errorf("a disabled job must not be scheduled: %v", next)
	}
	if next := NextRunAt(now, &models.SyncJob{Enabled: true}); next != nil {
		t.Errorf("a manual-only job must not be scheduled: %v", next)
	}
}

// TestSchedulerBootstrapAndRunDue drives the two jobs of Bootstrap — closing the
// runs a restart interrupted and seeding the next slot — then executes a
// scheduled run through RunDue.
func TestSchedulerBootstrapAndRunDue(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t, models.DirectionGoogleToMicrosoft, models.ConflictNewestWins)
	env.google.add("report.txt", "hello", time.Now().UTC())
	env.job.IntervalMinutes = 30
	env.job.NextRunAt = nil
	env.saveJob(t)

	now := time.Now().UTC()
	stale := &models.SyncRun{
		JobID:     env.job.ID,
		JobName:   env.job.Name,
		Status:    string(models.RunRunning),
		Trigger:   string(models.TriggerScheduled),
		StartedAt: now.Add(-time.Hour),
	}
	if err := env.store.CreateRun(ctx, stale); err != nil {
		t.Fatalf("creating the interrupted run: %v", err)
	}

	scheduler := NewScheduler(env.store, env.sync, env.tokens, nil)
	if err := scheduler.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrapping the scheduler: %v", err)
	}

	closed, err := env.store.GetRun(ctx, stale.ID)
	if err != nil {
		t.Fatalf("loading the interrupted run: %v", err)
	}
	if closed.Status != string(models.RunFailed) || closed.FinishedAt == nil {
		t.Errorf("an interrupted run must be closed: %+v", closed)
	}
	if closed.Message == "" {
		t.Error("an interrupted run must explain why it was closed")
	}
	job, err := env.store.GetJob(ctx, env.job.ID)
	if err != nil {
		t.Fatalf("loading the job: %v", err)
	}
	if job.NextRunAt == nil || !job.NextRunAt.After(now) {
		t.Errorf("Bootstrap must give the job a future slot, got %v", job.NextRunAt)
	}

	// Both loops start once and Stop() ends them.
	if err := scheduler.Start(ctx); err != nil {
		t.Fatalf("starting the scheduler: %v", err)
	}
	if err := scheduler.Start(ctx); !errors.Is(err, ErrBusy) {
		t.Errorf("a second Start = %v, want ErrBusy", err)
	}
	scheduler.Stop()

	// A job that is not due is left alone.
	if started := scheduler.RunDue(ctx); started != 0 {
		t.Errorf("RunDue started %d runs for a job scheduled in the future", started)
	}

	past := now.Add(-time.Minute)
	if err := env.store.ScheduleJob(ctx, env.job.ID, &past); err != nil {
		t.Fatalf("making the job due: %v", err)
	}
	if started := scheduler.RunDue(ctx); started != 1 {
		t.Fatalf("RunDue started %d runs, want 1", started)
	}
	env.sync.Wait()

	run := env.latestRun(t)
	if run.Trigger != string(models.TriggerScheduled) {
		t.Errorf("the run trigger = %q, want scheduled", run.Trigger)
	}
	if run.Status != string(models.RunSuccess) {
		t.Errorf("the scheduled run = %q (%s)", run.Status, run.Message)
	}
	job, err = env.store.GetJob(ctx, env.job.ID)
	if err != nil {
		t.Fatalf("loading the job: %v", err)
	}
	if job.NextRunAt == nil || !job.NextRunAt.After(now) {
		t.Errorf("a finished run must push the next slot forward, got %v", job.NextRunAt)
	}
	if job.LastStatus != string(models.RunSuccess) {
		t.Errorf("the job last status = %q, want success", job.LastStatus)
	}

	// A job that is already running skips its slot instead of piling up.
	env.google.block = make(chan struct{})
	if _, err := env.sync.Start(ctx, env.job.ID, models.TriggerManual); err != nil {
		t.Fatalf("starting a manual run: %v", err)
	}
	if err := env.store.ScheduleJob(ctx, env.job.ID, &past); err != nil {
		t.Fatalf("making the job due again: %v", err)
	}
	if started := scheduler.RunDue(ctx); started != 0 {
		t.Errorf("a busy job must not be started twice, RunDue started %d", started)
	}
	busy, err := env.store.GetJob(ctx, env.job.ID)
	if err != nil {
		t.Fatalf("loading the job: %v", err)
	}
	if busy.NextRunAt == nil || !busy.NextRunAt.After(now.Add(20*time.Minute)) {
		t.Errorf("a skipped slot must be pushed by one interval, got %v", busy.NextRunAt)
	}
	env.sync.Cancel(env.job.ID)
	env.sync.Wait()
	env.google.block = nil
}

// TestSchedulerRefreshTokens checks the token loop wrapper reports what the
// token manager refreshed.
func TestSchedulerRefreshTokens(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t, models.DirectionGoogleToMicrosoft, models.ConflictNewestWins)

	scheduler := NewScheduler(env.store, env.sync, env.tokens, nil)
	// The accounts stored by the fixture are valid for an hour, far outside the
	// refresh window, so nothing is refreshed.
	if refreshed := scheduler.RefreshTokens(ctx); refreshed != 0 {
		t.Errorf("RefreshTokens = %d, want 0", refreshed)
	}
	scheduler.Stop()
}
