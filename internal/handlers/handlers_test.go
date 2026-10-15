package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
	"unicode/utf8"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/database"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/notify"
	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/providers/google"
	"github.com/ivancarlosti/sync/internal/providers/microsoft"
	"github.com/ivancarlosti/sync/internal/services"
	"github.com/ivancarlosti/sync/internal/version"
)

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

// fixedNow is the clock every test server runs on, so a schedule preview or a
// session cookie never depends on when the suite is executed.
var fixedNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// testNow is the clock injected into Deps.
func testNow() time.Time { return fixedNow }

// testConfig builds a configuration usable without touching the process
// environment: the master key is installed directly.
func testConfig(t *testing.T, mode models.AuthMode) *config.Config {
	t.Helper()
	cfg := &config.Config{
		AppURL:          "https://sync.example.com",
		AppPort:         3000,
		LogLevel:        "error",
		AuthMethod:      mode,
		AccountLogin:    "admin@example.com",
		AccountPassword: "correct-horse-battery",
		DefaultLocale:   "en-US",
		DefaultTheme:    "system",
	}
	if err := cfg.SetEncryptionKey("0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("installing the encryption key: %v", err)
	}
	return cfg
}

// testAssets is a stand-in for the embedded SPA: one entry document and one
// hashed bundle, which is enough to exercise the history fallback and the cache
// policy of the static handler.
func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":        {Data: []byte("<!doctype html><title>sync</title>")},
		"assets/app-abc.js": {Data: []byte("console.log('sync')")},
	}
}

// testOptions tweaks the wiring of a test server: `configure` adjusts the
// configuration before the collaborators are built (installing a provider OAuth
// client, for instance) and `providers` replaces a built-in provider by a stub,
// which is how the endpoints that talk to a remote service are exercised without
// the network.
type testOptions struct {
	configure func(cfg *config.Config)
	providers []providers.Provider
}

// newTestServer wires a complete API on top of an in-memory database.
func newTestServer(t *testing.T, mode models.AuthMode) (*Server, *services.Store) {
	t.Helper()
	return newTestServerWith(t, mode, testOptions{})
}

// newTestServerWith wires a complete API with a customised dependency graph.
func newTestServerWith(t *testing.T, mode models.AuthMode, options testOptions) (*Server, *services.Store) {
	t.Helper()
	deps := testDepsWith(t, mode, options)
	server, err := New(deps)
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	return server, deps.Store
}

// testDeps wires a complete API on top of an in-memory database. The
// collaborators are the real services, so a test exercises the same validation,
// error mapping and SQL the production binary uses.
func testDeps(t *testing.T, mode models.AuthMode) Deps {
	t.Helper()
	return testDepsWith(t, mode, testOptions{})
}

// testDepsWith is testDeps with the wiring of the collaborators adjusted.
func testDepsWith(t *testing.T, mode models.AuthMode, options testOptions) Deps {
	t.Helper()
	cfg := testConfig(t, mode)
	if options.configure != nil {
		options.configure(cfg)
	}

	// The database is a file in a temporary directory with WAL and a busy
	// timeout, like the fixture of the service tests: a shared cache in-memory
	// database reports table locks the busy timeout cannot wait out.
	dsn := "file:" + filepath.Join(t.TempDir(), "sync.db") + "?_journal_mode=WAL&_busy_timeout=5000"
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

	settings := database.NewSettings(db)
	if err := database.Seed(context.Background(), settings, services.Defaults(cfg)); err != nil {
		t.Fatalf("seeding the settings: %v", err)
	}
	store := services.NewStore(db, settings)
	registry := providers.NewRegistry(withProviders(options.providers)...)
	box := services.NewSecretBox(cfg.EncryptionKeyBytes())

	creds := services.NewProviderSettings(cfg, settings, box)
	tokens := services.NewTokenManager(store, registry, creds, box)
	appSettings := services.NewSettingsService(cfg, settings)
	// The fixtures below deliver to httptest servers, which live on the loopback
	// interface, so they stand in for an operator who allowed a private target.
	notifier := services.NewNotifier(store, notify.NewDispatcher(time.Second, notify.Options{AllowPrivateTargets: true}), box)
	notifier.SetLink(cfg.AppURL)
	engine := services.NewSyncService(store, tokens, appSettings, notifier)
	audit := services.NewAuditService(store, tokens)

	deps := Deps{
		Config:      cfg,
		Store:       store,
		Auth:        services.NewAuthService(cfg, store),
		OAuth:       services.NewOAuthService(store, registry, creds, tokens),
		Tokens:      tokens,
		APITokens:   services.NewAPITokenService(store),
		Credentials: creds,
		Guide:       services.NewGuideService(registry, creds),
		Registry:    registry,
		Settings:    appSettings,
		Notifier:    notifier,
		Sync:        engine,
		Audit:       audit,
		Scheduler:   services.NewScheduler(store, engine, tokens, notifier),
		Purge:       services.NewPurgeService(store),
		Assets:      testAssets(),
		Info:        version.Get(),
		Now:         testNow,
	}
	return deps
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// call performs one request against the router. A string body is sent as-is (it
// is how a test submits malformed JSON), every other value is encoded.
func call(t *testing.T, server *Server, method, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	switch payload := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(payload)
	default:
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encoding the request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	request := httptest.NewRequest(method, path, reader)
	if reader != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	server.Router().ServeHTTP(recorder, request)
	return recorder
}

// decodeJSON reads the JSON body of a response into T.
func decodeJSON[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %q: %v", recorder.Body.String(), err)
	}
	return out
}

// requireStatus fails when the answer is not the expected one.
func requireStatus(t *testing.T, recorder *httptest.ResponseRecorder, want int) {
	t.Helper()
	if recorder.Code != want {
		t.Fatalf("status = %d, want %d (body %s)", recorder.Code, want, strings.TrimSpace(recorder.Body.String()))
	}
}

// requireErrorCode fails when the error answer does not carry the expected code.
func requireErrorCode(t *testing.T, recorder *httptest.ResponseRecorder, want string) errorBody {
	t.Helper()
	body := decodeJSON[errorBody](t, recorder)
	if body.Code != want {
		t.Fatalf("error code = %q, want %q (body %s)", body.Code, want, recorder.Body.String())
	}
	if strings.TrimSpace(body.Error) == "" {
		t.Fatalf("the error answer carries no message: %s", recorder.Body.String())
	}
	return body
}

// sessionCookies returns the cookies set by a response, ready to be replayed.
func sessionCookies(t *testing.T, recorder *httptest.ResponseRecorder) []*http.Cookie {
	t.Helper()
	cookies := recorder.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("no cookie was set: %s", recorder.Body.String())
	}
	return cookies
}

// login performs a successful login and returns the session cookie.
func login(t *testing.T, server *Server) *http.Cookie {
	t.Helper()
	recorder := call(t, server, http.MethodPost, "/api/auth/login", map[string]any{
		"login":    "admin@example.com",
		"password": "correct-horse-battery",
	})
	requireStatus(t, recorder, http.StatusOK)
	for _, cookie := range sessionCookies(t, recorder) {
		if cookie.Name == services.SessionCookieName {
			return cookie
		}
	}
	t.Fatal("the login answer did not carry the session cookie")
	return nil
}

// saveAccount inserts a connected account the way the OAuth callback does.
func saveAccount(t *testing.T, store *services.Store, provider models.ProviderName, id, email string) *models.ConnectedAccount {
	t.Helper()
	account := &models.ConnectedAccount{
		Provider:          string(provider),
		ProviderAccountID: id,
		Email:             email,
		Status:            "connected",
		ExpiresAt:         fixedNow.Add(time.Hour),
	}
	if err := store.SaveAccount(context.Background(), account); err != nil {
		t.Fatalf("saving the account: %v", err)
	}
	return account
}

// withProviders returns the built-in providers with the extra ones taking the
// place of a built-in that shares their name.
func withProviders(extra []providers.Provider) []providers.Provider {
	builtin := []providers.Provider{google.New(), microsoft.New()}
	if len(extra) == 0 {
		return builtin
	}
	replaced := make(map[models.ProviderName]bool, len(extra))
	for _, provider := range extra {
		replaced[provider.Name()] = true
	}
	merged := make([]providers.Provider, 0, len(builtin)+len(extra))
	for _, provider := range builtin {
		if !replaced[provider.Name()] {
			merged = append(merged, provider)
		}
	}
	return append(merged, extra...)
}

// stubProvider stands in for a real cloud storage service. The account screens
// (identity check, drive picker, folder browser, site search) are the only part
// of the API that calls a remote service, so they can only be exercised with a
// provider that answers from memory; it records what it was asked for and can be
// made to fail on demand.
type stubProvider struct {
	name     models.ProviderName
	account  providers.Account
	drives   []providers.Drive
	sites    []providers.Drive
	children []providers.Item
	// childrenByFolder overrides the canned listing of one folder, so a test can
	// describe a real tree instead of a single flat listing.
	childrenByFolder map[string][]providers.Item
	failure          error
	asked            []string
	refreshes        int
}

// Name identifies the stub, which replaces the built-in implementation of the
// same provider.
func (p *stubProvider) Name() models.ProviderName { return p.name }

// Scopes is the OAuth scope set of the stub.
func (p *stubProvider) Scopes() []string { return []string{"files.readwrite"} }

// AuthCodeURL builds a recognizable authorization URL.
func (p *stubProvider) AuthCodeURL(creds providers.Credentials, state, codeChallenge string) string {
	return "https://stub.example.com/authorize?state=" + state + "&code_challenge=" + codeChallenge
}

// Exchange is only reached by the OAuth callback, not by the account screens.
func (p *stubProvider) Exchange(ctx context.Context, creds providers.Credentials, code, codeVerifier string) (*providers.Tokens, error) {
	if p.failure != nil {
		return nil, p.failure
	}
	return p.tokens(), nil
}

// Refresh hands out a fresh access token and counts the call.
func (p *stubProvider) Refresh(ctx context.Context, creds providers.Credentials, refreshToken string) (*providers.Tokens, error) {
	if p.failure != nil {
		return nil, p.failure
	}
	p.refreshes++
	tokens := p.tokens()
	tokens.RefreshToken = refreshToken
	return tokens, nil
}

// tokens is the token set the stub issues.
func (p *stubProvider) tokens() *providers.Tokens {
	return &providers.Tokens{
		AccessToken:  "stub-access-" + strconv.Itoa(p.refreshes),
		RefreshToken: "stub-refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
		Scopes:       []string{"files.readwrite"},
	}
}

// Account returns the remote identity of the stub.
func (p *stubProvider) Account(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens) (*providers.Account, error) {
	if p.failure != nil {
		return nil, p.failure
	}
	account := p.account
	return &account, nil
}

// Drives lists the roots of the stub.
func (p *stubProvider) Drives(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens) ([]providers.Drive, error) {
	if p.failure != nil {
		return nil, p.failure
	}
	return p.drives, nil
}

// Children lists the canned entries and records the folder that was browsed.
func (p *stubProvider) Children(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, driveID, folderID string) ([]providers.Item, error) {
	if p.failure != nil {
		return nil, p.failure
	}
	p.asked = append(p.asked, driveID+"/"+folderID)
	if items, ok := p.childrenByFolder[folderID]; ok {
		return items, nil
	}
	return p.children, nil
}

// Item resolves a single entry.
func (p *stubProvider) Item(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, driveID, itemID string) (*providers.Item, error) {
	if p.failure != nil {
		return nil, p.failure
	}
	item := providers.Item{ID: itemID, Name: itemID}
	return &item, nil
}

// Download is not used by the account screens.
func (p *stubProvider) Download(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, driveID, itemID string) (*providers.Transfer, error) {
	return nil, providers.ErrUnsupported
}

// Upload is not used by the account screens.
func (p *stubProvider) Upload(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, req providers.UploadRequest) (*providers.Item, error) {
	return &providers.Item{ID: "uploaded", Name: req.Name, Size: req.Size}, nil
}

// CreateFolder is not used by the account screens.
func (p *stubProvider) CreateFolder(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, driveID, parentID, name string) (*providers.Item, error) {
	return &providers.Item{ID: "folder-1", Name: name, IsDir: true}, nil
}

// Delete is not used by the account screens.
func (p *stubProvider) Delete(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, driveID, itemID string) error {
	return nil
}

// IsNotFound recognizes the provider's "gone" error.
func (p *stubProvider) IsNotFound(err error) bool { return errors.Is(err, providers.ErrNotFound) }

// IsUnauthorized reports whether err is a rejected-access-token error. The stub
// never issues one, so this is always false.
func (p *stubProvider) IsUnauthorized(error) bool { return false }

// SearchSites is the SiteBrowser capability: it filters the canned library list
// by name, which is what the SharePoint picker asks for.
func (p *stubProvider) SearchSites(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, query string) ([]providers.Drive, error) {
	if p.failure != nil {
		return nil, p.failure
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return p.sites, nil
	}
	matches := make([]providers.Drive, 0, len(p.sites))
	for _, site := range p.sites {
		if strings.Contains(strings.ToLower(site.Name), needle) {
			matches = append(matches, site)
		}
	}
	return matches, nil
}

// ResolveSite is the SiteResolver capability: a location naming one of the canned
// libraries (a URL containing its name, or its id) resolves to it; anything else
// is "not found".
func (p *stubProvider) ResolveSite(ctx context.Context, creds providers.Credentials, tokens *providers.Tokens, location string) (providers.Drive, error) {
	if p.failure != nil {
		return providers.Drive{}, p.failure
	}
	needle := strings.ToLower(strings.TrimSpace(location))
	for _, site := range p.sites {
		if needle != "" && (strings.EqualFold(site.ID, needle) ||
			strings.Contains(needle, strings.ToLower(site.Name))) {
			return site, nil
		}
	}
	return providers.Drive{}, fmt.Errorf("%w: %s", providers.ErrNotFound, location)
}

// -----------------------------------------------------------------------------
// Authentication
// -----------------------------------------------------------------------------

// TestSessionReportsAuthenticationMode checks the public session endpoint: it
// is what the SPA reads before rendering anything, so it must answer with 200
// (never 401) and describe the mode even when the visitor is anonymous.
func TestSessionReportsAuthenticationMode(t *testing.T) {
	cases := []struct {
		name         string
		mode         models.AuthMode
		wantAuth     bool
		wantOpen     bool
		wantKeycloak bool
	}{
		{name: "open instance authenticates everyone", mode: models.AuthModeNone, wantAuth: true, wantOpen: true},
		{name: "account mode waits for a login", mode: models.AuthModeAccount},
		{name: "keycloak without a realm is not offered", mode: models.AuthModeKeycloak},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newTestServer(t, tc.mode)

			recorder := call(t, server, http.MethodGet, "/api/auth/session", nil)
			requireStatus(t, recorder, http.StatusOK)

			view := decodeJSON[sessionView](t, recorder)
			if view.Mode != string(tc.mode) {
				t.Errorf("mode = %q, want %q", view.Mode, tc.mode)
			}
			if view.Authenticated != tc.wantAuth {
				t.Errorf("authenticated = %v, want %v", view.Authenticated, tc.wantAuth)
			}
			if view.Open != tc.wantOpen {
				t.Errorf("open = %v, want %v", view.Open, tc.wantOpen)
			}
			if view.Operator.Authenticated != tc.wantAuth {
				t.Errorf("operator.authenticated = %v, want %v", view.Operator.Authenticated, tc.wantAuth)
			}
			if view.Keycloak.Enabled != tc.wantKeycloak {
				t.Errorf("keycloak.enabled = %v, want %v", view.Keycloak.Enabled, tc.wantKeycloak)
			}
			if view.Captcha.Enabled {
				t.Error("captcha is enabled although no site key is configured")
			}
			// The version travels with the session so the login page and
			// Admin > About can show it without a second request.
			if view.Version.Name != version.Name {
				t.Errorf("version.name = %q, want %q", view.Version.Name, version.Name)
			}
			if view.Defaults.DefaultLocale != "en-US" {
				t.Errorf("defaults.default_locale = %q, want %q", view.Defaults.DefaultLocale, "en-US")
			}
			if view.Defaults.SyncIntervalMinutes == 0 || view.Defaults.RunTimeoutMinutes == 0 {
				t.Errorf("defaults = %+v, want non-zero intervals", view.Defaults)
			}
		})
	}
}

// TestKeycloakDisabledWithoutRealm checks that the login button is reported as
// unavailable and that starting the flow fails with 424 (not configured) instead
// of redirecting to a half-built realm URL.
func TestKeycloakDisabledWithoutRealm(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeKeycloak)

	recorder := call(t, server, http.MethodGet, "/api/auth/session", nil)
	requireStatus(t, recorder, http.StatusOK)
	if view := decodeJSON[sessionView](t, recorder); view.Keycloak.Enabled {
		t.Error("keycloak is offered although no realm is configured")
	}

	recorder = call(t, server, http.MethodGet, "/api/auth/keycloak", nil)
	requireStatus(t, recorder, http.StatusFailedDependency)
	requireErrorCode(t, recorder, codeNotConfigured)
}

// TestLoginFlowAndGuard walks the account mode end to end: the guard rejects an
// anonymous request, a wrong password counts as a failure, a good one issues the
// HttpOnly session cookie, and logout expires it.
func TestLoginFlowAndGuard(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeAccount)

	recorder := call(t, server, http.MethodGet, "/api/jobs", nil)
	requireStatus(t, recorder, http.StatusUnauthorized)
	requireErrorCode(t, recorder, codeUnauthorized)

	recorder = call(t, server, http.MethodPost, "/api/auth/login", map[string]any{
		"login":    "admin@example.com",
		"password": "not-the-password",
	})
	requireStatus(t, recorder, http.StatusUnauthorized)
	requireErrorCode(t, recorder, codeUnauthorized)

	// A malformed payload is rejected before any credential is checked.
	recorder = call(t, server, http.MethodPost, "/api/auth/login", "{not json")
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	recorder = call(t, server, http.MethodPost, "/api/auth/login", map[string]any{
		"login":    "admin@example.com",
		"password": "correct-horse-battery",
	})
	requireStatus(t, recorder, http.StatusOK)
	view := decodeJSON[sessionView](t, recorder)
	if !view.Authenticated || !view.Operator.Admin {
		t.Fatalf("operator = %+v, want an authenticated administrator", view.Operator)
	}
	if view.Operator.Email != "admin@example.com" {
		t.Errorf("operator.email = %q, want the configured login", view.Operator.Email)
	}

	cookies := sessionCookies(t, recorder)
	cookie := cookies[0]
	if cookie.Name != services.SessionCookieName {
		t.Fatalf("cookie = %q, want %q", cookie.Name, services.SessionCookieName)
	}
	if !cookie.HttpOnly {
		t.Error("the session cookie is not HttpOnly")
	}
	if !cookie.Secure {
		t.Error("the session cookie is not Secure although APP_URL uses HTTPS")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie.SameSite = %v, want Lax", cookie.SameSite)
	}

	// The cookie opens the secured tree…
	recorder = call(t, server, http.MethodGet, "/api/jobs", nil, cookie)
	requireStatus(t, recorder, http.StatusOK)

	// …while a forged value is simply unauthenticated.
	recorder = call(t, server, http.MethodGet, "/api/jobs", nil, &http.Cookie{
		Name: services.SessionCookieName, Value: "forged.session.value",
	})
	requireStatus(t, recorder, http.StatusUnauthorized)
	requireErrorCode(t, recorder, codeUnauthorized)

	// Logout is idempotent and clears the cookie.
	recorder = call(t, server, http.MethodPost, "/api/auth/logout", nil, cookie)
	requireStatus(t, recorder, http.StatusNoContent)
	for _, expired := range recorder.Result().Cookies() {
		if expired.Name == services.SessionCookieName && expired.MaxAge >= 0 {
			t.Errorf("cleared cookie MaxAge = %d, want a negative value", expired.MaxAge)
		}
	}
}

// -----------------------------------------------------------------------------
// System endpoints
// -----------------------------------------------------------------------------

// TestHealthAndVersion checks the two probes an orchestrator and the SPA use.
// Both must stay public even in account mode, otherwise a container would be
// declared unhealthy because nobody logged in.
func TestHealthAndVersion(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeAccount)

	recorder := call(t, server, http.MethodGet, "/api/health", nil)
	requireStatus(t, recorder, http.StatusOK)
	health := decodeJSON[map[string]any](t, recorder)
	if health["status"] != "ok" || health["database"] != "ok" {
		t.Fatalf("health = %v, want status and database ok", health)
	}
	if health["auth_mode"] != string(models.AuthModeAccount) {
		t.Errorf("health auth_mode = %v, want %q", health["auth_mode"], models.AuthModeAccount)
	}
	if health["version"] != version.Version {
		t.Errorf("health version = %v, want %q", health["version"], version.Version)
	}
	if health["running_jobs"] != float64(0) {
		t.Errorf("health running_jobs = %v, want 0", health["running_jobs"])
	}

	recorder = call(t, server, http.MethodGet, "/api/version", nil)
	requireStatus(t, recorder, http.StatusOK)
	versionView := decodeJSON[struct {
		Info      version.Info `json:"info"`
		AuthMode  string       `json:"auth_mode"`
		Providers []string     `json:"providers"`
		Locales   []string     `json:"locales"`
		Themes    []string     `json:"themes"`
	}](t, recorder)
	if versionView.Info.Name != version.Name || versionView.Info.Version != version.Version {
		t.Errorf("info = %+v, want the running build", versionView.Info)
	}
	if versionView.Info.GoVersion == "" {
		t.Error("go_version is empty, want the toolchain used for the build")
	}
	if versionView.AuthMode != string(models.AuthModeAccount) {
		t.Errorf("auth_mode = %q, want %q", versionView.AuthMode, models.AuthModeAccount)
	}
	if len(versionView.Providers) != 2 {
		t.Fatalf("providers = %v, want the two compiled providers", versionView.Providers)
	}
	if versionView.Providers[0] != string(models.ProviderGoogle) && versionView.Providers[1] != string(models.ProviderGoogle) {
		t.Errorf("providers = %v, want google to be part of the build", versionView.Providers)
	}
	if len(versionView.Locales) != len(config.SupportedLocales) || len(versionView.Themes) != len(config.SupportedThemes) {
		t.Errorf("locales = %v, themes = %v, want the supported vocabulary", versionView.Locales, versionView.Themes)
	}
}

// -----------------------------------------------------------------------------
// Static assets
// -----------------------------------------------------------------------------

// TestStaticAssetsAndHistoryFallback verifies the single page contract: the
// entry document is always revalidated, a hashed bundle is immutable, and a deep
// link reaches the SPA router instead of a 404.
func TestStaticAssetsAndHistoryFallback(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	recorder := call(t, server, http.MethodGet, "/", nil)
	requireStatus(t, recorder, http.StatusOK)
	if got := recorder.Header().Get("Cache-Control"); got != revalidate {
		t.Errorf("index Cache-Control = %q, want %q", got, revalidate)
	}
	if !strings.Contains(recorder.Body.String(), "<!doctype html>") {
		t.Errorf("index body = %q, want the entry document", recorder.Body.String())
	}

	recorder = call(t, server, http.MethodGet, "/assets/app-abc.js", nil)
	requireStatus(t, recorder, http.StatusOK)
	if got := recorder.Header().Get("Cache-Control"); got != immutableCache {
		t.Errorf("asset Cache-Control = %q, want %q", got, immutableCache)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "javascript") {
		t.Errorf("asset Content-Type = %q, want a JavaScript type", contentType)
	}

	recorder = call(t, server, http.MethodGet, "/jobs/12", nil)
	requireStatus(t, recorder, http.StatusOK)
	if !strings.Contains(recorder.Body.String(), "<!doctype html>") {
		t.Errorf("deep link body = %q, want the entry document", recorder.Body.String())
	}
}

// TestUnknownAPIRouteAnswersJSON keeps /api/ out of the history fallback: an API
// client must receive a JSON 404, never the HTML of the SPA.
func TestUnknownAPIRouteAnswersJSON(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	recorder := call(t, server, http.MethodGet, "/api/does-not-exist", nil)
	requireStatus(t, recorder, http.StatusNotFound)
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", contentType)
	}
	body := requireErrorCode(t, recorder, codeNotFound)
	if !strings.Contains(body.Error, "/api/does-not-exist") {
		t.Errorf("error = %q, want the requested endpoint", body.Error)
	}
}

// TestMissingFrontendReportsNotConfigured covers a binary built without the
// `web/dist` payload: the operator gets an explicit 503, not an empty page.
func TestMissingFrontendReportsNotConfigured(t *testing.T) {
	deps := testDeps(t, models.AuthModeNone)
	deps.Assets = fstest.MapFS{}
	server, err := New(deps)
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}

	recorder := call(t, server, http.MethodGet, "/", nil)
	requireStatus(t, recorder, http.StatusServiceUnavailable)
	requireErrorCode(t, recorder, codeNotConfigured)
}

// TestSecurityHeadersOnEveryAnswer pins the hardening applied to every response,
// including the ones served to an anonymous visitor.
func TestSecurityHeadersOnEveryAnswer(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeAccount)

	recorder := call(t, server, http.MethodGet, "/api/auth/session", nil)
	requireStatus(t, recorder, http.StatusOK)
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
	} {
		if got := recorder.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if csp := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("Content-Security-Policy = %q, want frame-ancestors 'none'", csp)
	}
}

// -----------------------------------------------------------------------------
// Jobs
// -----------------------------------------------------------------------------

// jobPayload is the editor form of a job, as the SPA sends it.
func jobPayload(google, microsoft *models.ConnectedAccount) map[string]any {
	return map[string]any{
		"name":                    "Backup",
		"source_account_id":       google.ID,
		"destination_account_id":  microsoft.ID,
		"source_folder_path":      "/Documents",
		"destination_folder_path": "/Backup/Documents",
		"exclude_patterns":        []string{"*.tmp", "cache/**", "*.tmp", " "},
		"interval_minutes":        30,
		"enabled":                 true,
	}
}

// TestCreateJobValidation walks the checks the handler performs before anything
// is written, so a broken job never reaches the scheduler.
func TestCreateJobValidation(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	google := saveAccount(t, store, models.ProviderGoogle, "google-1", "g@example.com")
	microsoft := saveAccount(t, store, models.ProviderMicrosoft, "ms-1", "m@example.com")

	cases := []struct {
		name       string
		patch      map[string]any
		wantStatus int
		wantCode   string
	}{
		{name: "a name is required", patch: map[string]any{"name": "   "}},
		{name: "an unknown source account is not found",
			patch: map[string]any{"source_account_id": uint(999)}, wantStatus: http.StatusNotFound, wantCode: codeNotFound},
		{name: "an unknown destination account is not found",
			patch: map[string]any{"destination_account_id": uint(999)}, wantStatus: http.StatusNotFound, wantCode: codeNotFound},
		{name: "one account cannot use the same folder on both ends", patch: map[string]any{"destination_account_id": google.ID}},
		{name: "a destination folder inside the source folder is refused",
			patch: map[string]any{
				"destination_account_id":  google.ID,
				"source_drive_id":         "drive-1",
				"source_folder_id":        "folder-1",
				"source_folder_path":      "/Documents",
				"destination_drive_id":    "drive-1",
				"destination_folder_id":   "folder-2",
				"destination_folder_path": "/Documents/Old",
			}},
		{name: "a source folder inside the destination folder is refused when the copy runs backwards",
			patch: map[string]any{
				"destination_account_id":  google.ID,
				"source_drive_id":         "drive-1",
				"source_folder_id":        "folder-1",
				"source_folder_path":      "/Documents/Old",
				"destination_drive_id":    "drive-1",
				"destination_folder_id":   "folder-2",
				"destination_folder_path": "/Documents",
				"direction":               string(models.DirectionMicrosoftToGoogle),
			}},
		{name: "the interval is bounded", patch: map[string]any{"interval_minutes": maxIntervalMinutes + 1}},
		{name: "a negative interval is refused", patch: map[string]any{"interval_minutes": -5}},
		{name: "exclude patterns must be valid globs", patch: map[string]any{"exclude_patterns": []string{"[unclosed"}}},
		{name: "an unknown direction is refused", patch: map[string]any{"direction": "sideways"}},
		{name: "an unknown conflict policy is refused", patch: map[string]any{"conflict_policy": "coin_toss"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := jobPayload(google, microsoft)
			for key, value := range tc.patch {
				payload[key] = value
			}

			wantStatus := tc.wantStatus
			if wantStatus == 0 {
				wantStatus = http.StatusBadRequest
			}
			wantCode := tc.wantCode
			if wantCode == "" {
				wantCode = codeValidation
			}

			recorder := call(t, server, http.MethodPost, "/api/jobs", payload)
			requireStatus(t, recorder, wantStatus)
			requireErrorCode(t, recorder, wantCode)
		})
	}

	// Nothing was written by any of the rejected payloads.
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("listing the jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %d, want none after rejected payloads", len(jobs))
	}
}

// TestCreateJobAllowsOneAccountOnBothEnds pins the relaxation of the old
// "two different accounts" rule: an operator may keep two folders of one account
// aligned (OneDrive → OneDrive), and may copy an OneDrive folder into a
// SharePoint library of the same work account. Only the location of the two ends
// is still checked, and the engine already runs an intra-provider job through the
// same code path as any other (see services.applyDirections).
func TestCreateJobAllowsOneAccountOnBothEnds(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	microsoft := saveAccount(t, store, models.ProviderMicrosoft, "ms-1", "m@example.com")

	cases := []struct {
		name         string
		sourceDrive  string
		sourceFolder string
		sourcePath   string
		destDrive    string
		destFolder   string
		destPath     string
	}{
		{
			name:        "two folders of the same OneDrive",
			sourceDrive: "b!onedrive", sourceFolder: "folder-1", sourcePath: "/Documents",
			destDrive: "b!onedrive", destFolder: "folder-2", destPath: "/Backup",
		},
		{
			name:        "an OneDrive folder into a SharePoint library of the same account",
			sourceDrive: "b!onedrive", sourceFolder: "folder-1", sourcePath: "/Documents",
			destDrive: "b!library", destFolder: "folder-3", destPath: "/Documents",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			payload := map[string]any{
				"name":                    testCase.name,
				"source_account_id":       microsoft.ID,
				"destination_account_id":  microsoft.ID,
				"source_drive_id":         testCase.sourceDrive,
				"source_folder_id":        testCase.sourceFolder,
				"source_folder_path":      testCase.sourcePath,
				"destination_drive_id":    testCase.destDrive,
				"destination_folder_id":   testCase.destFolder,
				"destination_folder_path": testCase.destPath,
				"interval_minutes":        0,
			}

			recorder := call(t, server, http.MethodPost, "/api/jobs", payload)
			requireStatus(t, recorder, http.StatusCreated)
			job := decodeJSON[jobView](t, recorder)
			if job.SourceAccountID != microsoft.ID || job.DestinationAccountID != microsoft.ID {
				t.Errorf("accounts = %d/%d, want the one account on both ends",
					job.SourceAccountID, job.DestinationAccountID)
			}
			if job.SourceFolderID != testCase.sourceFolder || job.DestinationFolderID != testCase.destFolder {
				t.Errorf("folder ids = %q/%q, want %q/%q", job.SourceFolderID, job.DestinationFolderID,
					testCase.sourceFolder, testCase.destFolder)
			}
			if job.SourceFolderPath != testCase.sourcePath || job.DestinationFolderPath != testCase.destPath {
				t.Errorf("folder paths = %q/%q, want %q/%q", job.SourceFolderPath, job.DestinationFolderPath,
					testCase.sourcePath, testCase.destPath)
			}
			// An intra-provider job still carries a direction, and the default of
			// any pair that is not Microsoft → Google is the forward one.
			if job.Direction != string(models.DirectionGoogleToMicrosoft) {
				t.Errorf("direction = %q, want the default of a Microsoft → Microsoft pair", job.Direction)
			}
		})
	}

	// Every accepted payload reached the store.
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("listing the jobs: %v", err)
	}
	if len(jobs) != len(cases) {
		t.Fatalf("jobs = %d, want the %d accepted payloads", len(jobs), len(cases))
	}
}

// TestPathWithin pins the comparison behind the nesting guard: the display paths
// of the picker, normalised so a trailing slash and a missing leading one do not
// matter, and an empty path answering false because it cannot be placed in the
// tree (see applyJobInput).
func TestPathWithin(t *testing.T) {
	cases := []struct {
		inner string
		outer string
		want  bool
	}{
		{inner: "/Documents/Old", outer: "/Documents", want: true},
		{inner: "/Documents", outer: "/Documents", want: true},
		{inner: "Documents/Old/", outer: "Documents", want: true},
		{inner: "/Documents/Old/2026", outer: "/Documents", want: true},
		{inner: "/DocumentsX", outer: "/Documents", want: false},
		{inner: "/Documents", outer: "/Documents/Old", want: false},
		{inner: "/Other", outer: "/Documents", want: false},
		{inner: "/Documents", outer: "", want: false},
		{inner: "/Documents", outer: "/", want: false},
		{inner: "", outer: "/Documents", want: false},
	}
	for _, testCase := range cases {
		if got := pathWithin(testCase.inner, testCase.outer); got != testCase.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v",
				testCase.inner, testCase.outer, got, testCase.want)
		}
	}
}

// TestJobLifecycle covers create → read → update → schedule → delete, i.e. what
// the Jobs screen does between loading and saving. It also pins the derived
// defaults so a fresh job is never left without a direction or a policy.
func TestJobLifecycle(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	google := saveAccount(t, store, models.ProviderGoogle, "google-1", "g@example.com")
	microsoft := saveAccount(t, store, models.ProviderMicrosoft, "ms-1", "m@example.com")

	recorder := call(t, server, http.MethodPost, "/api/jobs", jobPayload(google, microsoft))
	requireStatus(t, recorder, http.StatusCreated)
	job := decodeJSON[jobView](t, recorder)

	if job.ID == 0 {
		t.Fatal("the created job has no identifier")
	}
	if job.Direction != string(models.DirectionGoogleToMicrosoft) {
		t.Errorf("direction = %q, want the default for a Google → Microsoft pair", job.Direction)
	}
	if job.ConflictPolicy != string(models.ConflictNewestWins) {
		t.Errorf("conflict_policy = %q, want %q", job.ConflictPolicy, models.ConflictNewestWins)
	}
	if !job.Enabled {
		t.Error("a newly created job is disabled")
	}
	if job.IntervalMinutes != 30 {
		t.Errorf("interval_minutes = %d, want 30", job.IntervalMinutes)
	}
	// The picker may omit the ids: they fall back to the root of the drive.
	if job.SourceFolderID != providers.DriveRoot || job.DestinationFolderID != providers.DriveRoot {
		t.Errorf("folder ids = %q/%q, want the drive root", job.SourceFolderID, job.DestinationFolderID)
	}
	// The payload omits the folder option: a fresh job mirrors the tree.
	if !job.SyncEmptyFolders {
		t.Error("a job created without sync_empty_folders must mirror the empty folders")
	}
	// Blank entries and duplicates are dropped by the encoder.
	if len(job.ExcludePatterns) != 2 {
		t.Errorf("exclude_patterns = %v, want the two normalised patterns", job.ExcludePatterns)
	}
	// Creating a job schedules it immediately: the dashboard would otherwise
	// show an enabled job that never runs.
	wantNext := fixedNow.Add(30 * time.Minute)
	if job.NextRunAt == nil || !job.NextRunAt.Equal(wantNext) {
		t.Errorf("next_run_at = %v, want %v", job.NextRunAt, wantNext)
	}
	if job.Running {
		t.Error("the new job is reported as running")
	}

	id := strconv.FormatUint(uint64(job.ID), 10)

	// A listing returns the same job.
	recorder = call(t, server, http.MethodGet, "/api/jobs", nil)
	requireStatus(t, recorder, http.StatusOK)
	list := decodeJSON[struct {
		Jobs []jobView `json:"jobs"`
	}](t, recorder)
	if len(list.Jobs) != 1 || list.Jobs[0].ID != job.ID {
		t.Fatalf("jobs = %+v, want the created job", list.Jobs)
	}

	// Reading it by id works, a malformed id is a 400 and a missing one a 404.
	recorder = call(t, server, http.MethodGet, "/api/jobs/"+id, nil)
	requireStatus(t, recorder, http.StatusOK)
	if fetched := decodeJSON[jobView](t, recorder); fetched.Name != "Backup" {
		t.Errorf("name = %q, want Backup", fetched.Name)
	}

	recorder = call(t, server, http.MethodGet, "/api/jobs/not-a-number", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	recorder = call(t, server, http.MethodGet, "/api/jobs/4242", nil)
	requireStatus(t, recorder, http.StatusNotFound)
	requireErrorCode(t, recorder, codeNotFound)
}

// TestJobSyncEmptyFoldersRoundTrip pins the per-job folder option: a payload that
// turns it off must stay off across a re-read — the column carries a `default:true`,
// so the store compensates for the substitution GORM performs on insert — and a
// later update can turn it back on.
func TestJobSyncEmptyFoldersRoundTrip(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	google := saveAccount(t, store, models.ProviderGoogle, "google-1", "g@example.com")
	microsoft := saveAccount(t, store, models.ProviderMicrosoft, "ms-1", "m@example.com")

	payload := jobPayload(google, microsoft)
	payload["sync_empty_folders"] = false
	recorder := call(t, server, http.MethodPost, "/api/jobs", payload)
	requireStatus(t, recorder, http.StatusCreated)
	created := decodeJSON[jobView](t, recorder)
	if created.SyncEmptyFolders {
		t.Error("sync_empty_folders = true, want the false the payload asked for")
	}
	if stored, err := store.GetJob(context.Background(), created.ID); err != nil {
		t.Fatalf("loading the job: %v", err)
	} else if stored.SyncEmptyFolders {
		t.Error("the stored column must keep the option off")
	}

	// An update that omits the field leaves the stored value alone, and one that
	// names it wins. The editor always sends the whole form, the request shape
	// keeps both cases working.
	id := strconv.FormatUint(uint64(created.ID), 10)
	delete(payload, "sync_empty_folders")
	payload["name"] = "Backup (quiet)"
	recorder = call(t, server, http.MethodPut, "/api/jobs/"+id, payload)
	requireStatus(t, recorder, http.StatusOK)
	if updated := decodeJSON[jobView](t, recorder); updated.SyncEmptyFolders {
		t.Error("an update that omits the field must keep the stored value")
	}

	payload["sync_empty_folders"] = true
	recorder = call(t, server, http.MethodPut, "/api/jobs/"+id, payload)
	requireStatus(t, recorder, http.StatusOK)
	if updated := decodeJSON[jobView](t, recorder); !updated.SyncEmptyFolders {
		t.Error("an update that names the field must apply it")
	}
	if stored, err := store.GetJob(context.Background(), created.ID); err != nil {
		t.Fatalf("loading the job: %v", err)
	} else if !stored.SyncEmptyFolders {
		t.Error("the stored column must follow the update")
	}
}

// TestJobStoresDriveLabels pins the drive display columns: the folder picker
// sends the name, kind and owner of each root along with its id, and the job
// keeps them so the list can name an OneDrive, a shared drive or a SharePoint
// library without a provider call. They are optional, so a payload that omits
// them (an older client) still saves and leaves the columns empty.
func TestJobStoresDriveLabels(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	google := saveAccount(t, store, models.ProviderGoogle, "google-1", "g@example.com")
	microsoft := saveAccount(t, store, models.ProviderMicrosoft, "ms-1", "m@example.com")

	payload := jobPayload(google, microsoft)
	payload["source_drive_id"] = "shared-drive-1"
	payload["source_drive_name"] = "  Finance  "
	payload["source_drive_kind"] = "shared"
	payload["destination_drive_id"] = "b!library"
	payload["destination_drive_name"] = "Documents"
	payload["destination_drive_kind"] = "document_library"
	payload["destination_drive_owner"] = "https://contoso.sharepoint.com/sites/marketing/Shared Documents"

	recorder := call(t, server, http.MethodPost, "/api/jobs", payload)
	requireStatus(t, recorder, http.StatusCreated)
	job := decodeJSON[jobView](t, recorder)

	// The values are trimmed, and a Google shared drive has no owner URL.
	if job.SourceDriveName != "Finance" || job.SourceDriveKind != "shared" || job.SourceDriveOwner != "" {
		t.Errorf("source drive = %q/%q/%q, want the shared drive label",
			job.SourceDriveName, job.SourceDriveKind, job.SourceDriveOwner)
	}
	if job.DestinationDriveName != "Documents" || job.DestinationDriveKind != "document_library" {
		t.Errorf("destination drive = %q/%q, want the SharePoint library label",
			job.DestinationDriveName, job.DestinationDriveKind)
	}
	if job.DestinationDriveOwner != payload["destination_drive_owner"] {
		t.Errorf("destination drive owner = %q, want %q",
			job.DestinationDriveOwner, payload["destination_drive_owner"])
	}

	// The columns are stored, not merely echoed: a re-read returns them.
	id := strconv.FormatUint(uint64(job.ID), 10)
	recorder = call(t, server, http.MethodGet, "/api/jobs/"+id, nil)
	requireStatus(t, recorder, http.StatusOK)
	stored := decodeJSON[jobView](t, recorder)
	if stored.SourceDriveKind != "shared" || stored.DestinationDriveKind != "document_library" {
		t.Errorf("stored drive kinds = %q/%q, want shared/document_library",
			stored.SourceDriveKind, stored.DestinationDriveKind)
	}

	// An update replaces the job as a whole, so the editor sends the labels again.
	payload["source_drive_name"] = "Finance 2026"
	recorder = call(t, server, http.MethodPut, "/api/jobs/"+id, payload)
	requireStatus(t, recorder, http.StatusOK)
	if updated := decodeJSON[jobView](t, recorder); updated.SourceDriveName != "Finance 2026" {
		t.Errorf("updated drive name = %q, want Finance 2026", updated.SourceDriveName)
	}

	// A payload without the display parts leaves them empty instead of failing:
	// that is what an older SPA (and a script) sends.
	trimmed := jobPayload(google, microsoft)
	trimmed["source_drive_id"] = "shared-drive-1"
	trimmed["destination_drive_id"] = "b!library"
	recorder = call(t, server, http.MethodPost, "/api/jobs", trimmed)
	requireStatus(t, recorder, http.StatusCreated)
	bare := decodeJSON[jobView](t, recorder)
	if bare.SourceDriveName != "" || bare.DestinationDriveKind != "" {
		t.Errorf("drive columns = %q/%q, want them empty",
			bare.SourceDriveName, bare.DestinationDriveKind)
	}
}

// TestCreateJobClampsDriveLabels pins the width guard: a provider value longer
// than its column is cut, not refused, so an unusually long library name cannot
// turn a job save into a 500.
func TestCreateJobClampsDriveLabels(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	google := saveAccount(t, store, models.ProviderGoogle, "google-1", "g@example.com")
	microsoft := saveAccount(t, store, models.ProviderMicrosoft, "ms-1", "m@example.com")

	payload := jobPayload(google, microsoft)
	payload["source_drive_id"] = "shared-drive-1"
	payload["source_drive_name"] = strings.Repeat("ä", driveNameWidth+5)
	payload["source_drive_kind"] = strings.Repeat("k", driveKindWidth+5)

	recorder := call(t, server, http.MethodPost, "/api/jobs", payload)
	requireStatus(t, recorder, http.StatusCreated)
	job := decodeJSON[jobView](t, recorder)
	if got := len([]rune(job.SourceDriveName)); got != driveNameWidth {
		t.Errorf("drive name length = %d, want %d", got, driveNameWidth)
	}
	if got := len([]rune(job.SourceDriveKind)); got != driveKindWidth {
		t.Errorf("drive kind length = %d, want %d", got, driveKindWidth)
	}
	// The cut is by rune, so the clamped value stays valid UTF-8.
	if !utf8.ValidString(job.SourceDriveName) {
		t.Error("the clamped drive name is not valid UTF-8")
	}
}

// createJob creates a job through the API.
func createJob(t *testing.T, server *Server, google, microsoft *models.ConnectedAccount) jobView {
	t.Helper()
	recorder := call(t, server, http.MethodPost, "/api/jobs", jobPayload(google, microsoft))
	requireStatus(t, recorder, http.StatusCreated)
	return decodeJSON[jobView](t, recorder)
}

// TestJobUpdateScheduleRunAndCancel follows a job through the rest of its life:
// updating the interval replans it, disabling clears the plan and refuses a
// manual run, and the manual run of an enabled job is accepted then cancellable.
func TestJobUpdateScheduleRunAndCancel(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	google := saveAccount(t, store, models.ProviderGoogle, "google-1", "g@example.com")
	microsoft := saveAccount(t, store, models.ProviderMicrosoft, "ms-1", "m@example.com")
	job := createJob(t, server, google, microsoft)
	id := strconv.FormatUint(uint64(job.ID), 10)

	// The schedule preview lists the next five executions, one interval apart.
	recorder := call(t, server, http.MethodGet, "/api/jobs/"+id+"/schedule", nil)
	requireStatus(t, recorder, http.StatusOK)
	preview := decodeJSON[struct {
		NextRunAt *time.Time  `json:"next_run_at"`
		Scheduled bool        `json:"scheduled"`
		Preview   []time.Time `json:"preview"`
	}](t, recorder)
	if !preview.Scheduled || preview.NextRunAt == nil {
		t.Fatalf("preview = %+v, want a scheduled job", preview)
	}
	if len(preview.Preview) != 5 {
		t.Fatalf("preview has %d entries, want 5", len(preview.Preview))
	}
	for i := 1; i < len(preview.Preview); i++ {
		if step := preview.Preview[i].Sub(preview.Preview[i-1]); step != 30*time.Minute {
			t.Errorf("step %d = %v, want 30m", i, step)
		}
	}

	// A new interval is planned as soon as the job is saved.
	payload := jobPayload(google, microsoft)
	payload["name"] = "Backup H2"
	payload["interval_minutes"] = 120
	recorder = call(t, server, http.MethodPut, "/api/jobs/"+id, payload)
	requireStatus(t, recorder, http.StatusOK)
	updated := decodeJSON[jobView](t, recorder)
	if updated.Name != "Backup H2" || updated.IntervalMinutes != 120 {
		t.Errorf("updated job = %s/%d, want Backup H2/120", updated.Name, updated.IntervalMinutes)
	}
	wantNext := fixedNow.Add(120 * time.Minute)
	if updated.NextRunAt == nil || !updated.NextRunAt.Equal(wantNext) {
		t.Errorf("next_run_at = %v, want %v", updated.NextRunAt, wantNext)
	}

	// Disabling the job clears its schedule and refuses a manual run.
	payload["enabled"] = false
	recorder = call(t, server, http.MethodPut, "/api/jobs/"+id, payload)
	requireStatus(t, recorder, http.StatusOK)
	disabled := decodeJSON[jobView](t, recorder)
	if disabled.Enabled {
		t.Error("the job is still enabled after being disabled")
	}
	if disabled.NextRunAt != nil {
		t.Errorf("next_run_at = %v, want none for a disabled job", disabled.NextRunAt)
	}
	recorder = call(t, server, http.MethodPost, "/api/jobs/"+id+"/run", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// "Synchronise now" is accepted once the job is enabled again, and the
	// answer carries the run the UI follows.
	payload["enabled"] = true
	recorder = call(t, server, http.MethodPut, "/api/jobs/"+id, payload)
	requireStatus(t, recorder, http.StatusOK)

	recorder = call(t, server, http.MethodPost, "/api/jobs/"+id+"/run", nil)
	requireStatus(t, recorder, http.StatusAccepted)
	started := decodeJSON[struct {
		JobID uint           `json:"job_id"`
		Run   models.SyncRun `json:"run"`
	}](t, recorder)
	if started.JobID != job.ID {
		t.Errorf("job_id = %d, want %d", started.JobID, job.ID)
	}
	if started.Run.ID == 0 || started.Run.Status != string(models.RunRunning) {
		t.Errorf("run = %+v, want a fresh running run", started.Run)
	}

	// Cancelling is idempotent and reports what it did.
	recorder = call(t, server, http.MethodPost, "/api/jobs/"+id+"/cancel", nil)
	requireStatus(t, recorder, http.StatusOK)
	cancelled := decodeJSON[struct {
		JobID     uint `json:"job_id"`
		Cancelled bool `json:"cancelled"`
	}](t, recorder)
	if cancelled.JobID != job.ID {
		t.Errorf("job_id = %d, want %d", cancelled.JobID, job.ID)
	}

	// The run is asynchronous: drain it before the database is closed.
	server.deps.Sync.Wait()

	// Deleting reports the identifier it removed, and the job is gone.
	recorder = call(t, server, http.MethodDelete, "/api/jobs/"+id, nil)
	requireStatus(t, recorder, http.StatusOK)
	if deleted := decodeJSON[struct {
		ID uint `json:"id"`
	}](t, recorder); deleted.ID != job.ID {
		t.Errorf("deleted id = %d, want %d", deleted.ID, job.ID)
	}

	recorder = call(t, server, http.MethodGet, "/api/jobs/"+id, nil)
	requireStatus(t, recorder, http.StatusNotFound)
}

// -----------------------------------------------------------------------------
// Notification channels
// -----------------------------------------------------------------------------

// TestNotificationChannelsCRUD drives the channel form end to end: the API serves
// the schema of every kind, a stored secret is never returned, the test event
// really reaches the webhook, and the toggle is separate from the full update.
func TestNotificationChannelsCRUD(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	delivered := make(chan string, 4)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		delivered <- string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	// The list is empty but already carries the vocabulary of the build.
	recorder := call(t, server, http.MethodGet, "/api/notifications", nil)
	requireStatus(t, recorder, http.StatusOK)
	list := decodeJSON[struct {
		Channels []services.ChannelView `json:"channels"`
		Kinds    []channelKind          `json:"kinds"`
		Events   []string               `json:"events"`
	}](t, recorder)
	if len(list.Channels) != 0 {
		t.Fatalf("channels = %+v, want none on a fresh instance", list.Channels)
	}
	if len(list.Kinds) != 3 {
		t.Errorf("kinds = %d, want the three shipped senders", len(list.Kinds))
	}
	if len(list.Events) != len(models.AllEvents) {
		t.Errorf("events = %v, want %v", list.Events, models.AllEvents)
	}
	for _, kind := range list.Kinds {
		if kind.Label == "" || len(kind.Fields) == 0 {
			t.Errorf("kind %q has no label or no fields", kind.Kind)
		}
	}

	// An unknown kind and a missing name are both validation errors.
	recorder = call(t, server, http.MethodPost, "/api/notifications", map[string]any{
		"name": "Carrier pigeon", "type": "pigeon", "events": []string{models.EventSyncSuccess},
	})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	recorder = call(t, server, http.MethodPost, "/api/notifications", map[string]any{
		"name": "   ", "type": notify.KindWebhook, "events": []string{models.EventSyncSuccess},
	})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// A webhook without a URL cannot work, so it is refused up front.
	recorder = call(t, server, http.MethodPost, "/api/notifications", map[string]any{
		"name": "Alerts", "type": notify.KindWebhook, "events": []string{models.EventSyncSuccess},
		"config": map[string]any{},
	})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// The real thing: a webhook subscribed to one event.
	payload := map[string]any{
		"name":    "Alerts",
		"type":    notify.KindWebhook,
		"events":  []string{models.EventSyncSuccess},
		"enabled": true,
		"config": map[string]any{
			"url":    target.URL,
			"secret": "s3cret-signing-key",
		},
	}
	recorder = call(t, server, http.MethodPost, "/api/notifications", payload)
	requireStatus(t, recorder, http.StatusCreated)
	channel := decodeJSON[services.ChannelView](t, recorder)
	if channel.ID == 0 || !channel.Enabled {
		t.Fatalf("channel = %+v, want an enabled channel with an id", channel)
	}
	if channel.Config["url"] != target.URL {
		t.Errorf("config url = %v, want %q", channel.Config["url"], target.URL)
	}
	// The signing secret was stored encrypted and is masked on the way out.
	const masked = "********"
	if channel.Config["secret"] != masked {
		t.Errorf("config secret = %v, want %q", channel.Config["secret"], masked)
	}
	id := strconv.FormatUint(uint64(channel.ID), 10)

	// Reading it back is the same view; an unknown id is a 404.
	recorder = call(t, server, http.MethodGet, "/api/notifications/"+id, nil)
	requireStatus(t, recorder, http.StatusOK)
	if fetched := decodeJSON[services.ChannelView](t, recorder); fetched.Name != "Alerts" {
		t.Errorf("name = %q, want Alerts", fetched.Name)
	}

	recorder = call(t, server, http.MethodGet, "/api/notifications/4242", nil)
	requireStatus(t, recorder, http.StatusNotFound)
	requireErrorCode(t, recorder, codeNotFound)

	// The toggle refuses a payload without the boolean it exists for.
	recorder = call(t, server, http.MethodPut, "/api/notifications/"+id+"/enabled", map[string]any{})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	recorder = call(t, server, http.MethodPut, "/api/notifications/"+id+"/enabled", map[string]any{"enabled": false})
	requireStatus(t, recorder, http.StatusOK)
	if toggled := decodeJSON[services.ChannelView](t, recorder); toggled.Enabled {
		t.Error("the channel is still enabled after being disabled")
	}

	// A test event is delivered synchronously and really reaches the webhook.
	recorder = call(t, server, http.MethodPost, "/api/notifications/"+id+"/test", nil)
	requireStatus(t, recorder, http.StatusOK)
	result := decodeJSON[struct {
		ID        uint `json:"id"`
		Delivered bool `json:"delivered"`
	}](t, recorder)
	if result.ID != channel.ID || !result.Delivered {
		t.Fatalf("test answer = %+v, want a delivery", result)
	}
	select {
	case body := <-delivered:
		if !strings.Contains(body, models.EventTest) {
			t.Errorf("webhook body = %q, want the %q event", body, models.EventTest)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the test event never reached the webhook")
	}

	// The delivery outcome is recorded on the channel, which is what the list
	// shows beside the name.
	recorder = call(t, server, http.MethodGet, "/api/notifications/"+id, nil)
	requireStatus(t, recorder, http.StatusOK)
	if touched := decodeJSON[services.ChannelView](t, recorder); touched.LastStatus != "ok" {
		t.Errorf("last_status = %q, want the recorded delivery outcome", touched.LastStatus)
	}

	// Updating keeps the stored secret when the mask is sent back unchanged.
	payload["name"] = "Alerts (prod)"
	payload["enabled"] = true
	recorder = call(t, server, http.MethodPut, "/api/notifications/"+id, payload)
	requireStatus(t, recorder, http.StatusOK)
	updated := decodeJSON[services.ChannelView](t, recorder)
	if updated.Name != "Alerts (prod)" || !updated.Enabled {
		t.Errorf("updated channel = %+v, want the new name and enabled", updated)
	}
	if updated.Config["secret"] != masked || updated.Config["url"] != target.URL {
		t.Errorf("updated config = %v, want the stored values kept", updated.Config)
	}

	// Deleting answers 204 and the channel is gone.
	recorder = call(t, server, http.MethodDelete, "/api/notifications/"+id, nil)
	requireStatus(t, recorder, http.StatusNoContent)

	recorder = call(t, server, http.MethodGet, "/api/notifications/"+id, nil)
	requireStatus(t, recorder, http.StatusNotFound)
}

// TestNotificationDeliveryFailureIsReported checks the failure half of the test
// button: the endpoint answers 502 with the reason the sender reported (so a
// misconfigured host or a dead endpoint is diagnosable from the UI instead of
// hiding behind "check the server logs"), and the same reason is recorded on the
// channel. The destination embeds the signing secret on purpose: the recorded
// error would leak it if the notifier did not mask every configuration secret.
func TestNotificationDeliveryFailureIsReported(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	const secret = "s3cret-signing-key"
	recorder := call(t, server, http.MethodPost, "/api/notifications", map[string]any{
		"name":   "Dead webhook",
		"type":   notify.KindWebhook,
		"events": []string{models.EventSyncRunFailed},
		"config": map[string]any{
			// Port 1 on the loopback refuses immediately, so the test needs no
			// network and no waiting.
			"url":             "http://127.0.0.1:1/" + secret,
			"secret":          secret,
			"timeout_seconds": 2,
		},
	})
	requireStatus(t, recorder, http.StatusCreated)
	channel := decodeJSON[services.ChannelView](t, recorder)
	id := strconv.FormatUint(uint64(channel.ID), 10)

	recorder = call(t, server, http.MethodPost, "/api/notifications/"+id+"/test", nil)
	requireStatus(t, recorder, http.StatusBadGateway)
	requireErrorCode(t, recorder, codeDeliveryFailed)
	failure := decodeJSON[struct {
		ID        uint   `json:"id"`
		Delivered bool   `json:"delivered"`
		Error     string `json:"error"`
	}](t, recorder)
	if failure.ID != channel.ID || failure.Delivered {
		t.Fatalf("test answer = %+v, want a reported failure", failure)
	}
	if strings.TrimSpace(failure.Error) == "" {
		t.Error("the failure carries no reason for the operator")
	}
	if strings.Contains(failure.Error, secret) {
		t.Errorf("the failure leaked the channel secret: %q", failure.Error)
	}

	// The outcome of the attempt is on the channel, which is what the list shows.
	recorder = call(t, server, http.MethodGet, "/api/notifications/"+id, nil)
	requireStatus(t, recorder, http.StatusOK)
	stored := decodeJSON[services.ChannelView](t, recorder)
	if stored.LastStatus != "error" {
		t.Errorf("last_status = %q, want error", stored.LastStatus)
	}
	if strings.TrimSpace(stored.LastError) == "" || strings.Contains(stored.LastError, secret) {
		t.Errorf("last_error = %q, want a masked reason", stored.LastError)
	}

	// Testing a channel that does not exist stays a 404, not a delivery failure.
	recorder = call(t, server, http.MethodPost, "/api/notifications/4242/test", nil)
	requireStatus(t, recorder, http.StatusNotFound)
	requireErrorCode(t, recorder, codeNotFound)
}

// TestNotificationSchemasCoverSenderKeys guards the server driven forms: a form
// that does not ask for a value the sender requires makes the feature unusable
// from the SPA (an SMTP channel needs at least one recipient).
func TestNotificationSchemasCoverSenderKeys(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	recorder := call(t, server, http.MethodGet, "/api/notifications", nil)
	requireStatus(t, recorder, http.StatusOK)
	catalog := decodeJSON[struct {
		Kinds []channelKind `json:"kinds"`
	}](t, recorder)

	required := map[string][]string{
		notify.KindSMTP:     {"host", "from", "to"},
		notify.KindWebhook:  {"url"},
		notify.KindShoutrrr: {"url"},
	}
	seen := make(map[string]bool, len(required))
	for _, kind := range catalog.Kinds {
		wanted, ok := required[kind.Kind]
		if !ok {
			continue
		}
		seen[kind.Kind] = true
		for _, key := range wanted {
			found := false
			for _, field := range kind.Fields {
				if field.Key == key {
					found = field.Required
				}
			}
			if !found {
				t.Errorf("the %s form does not ask for the required key %q", kind.Kind, key)
			}
		}
	}
	for kind := range required {
		if !seen[kind] {
			t.Errorf("the catalog does not describe the %s channel", kind)
		}
	}
}

// -----------------------------------------------------------------------------
// Maintenance
// -----------------------------------------------------------------------------

// TestMaintenanceEndpoints checks the four janitorial buttons. They are safe on
// an empty instance: nothing is due, nothing expires and nothing is pruned.
func TestMaintenanceEndpoints(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	recorder := call(t, server, http.MethodPost, "/api/maintenance/schedule/run", nil)
	requireStatus(t, recorder, http.StatusOK)
	if started := decodeJSON[struct {
		Started int `json:"started"`
	}](t, recorder); started.Started != 0 {
		t.Errorf("started = %d, want no due job on a fresh instance", started.Started)
	}

	recorder = call(t, server, http.MethodPost, "/api/maintenance/tokens/refresh", nil)
	requireStatus(t, recorder, http.StatusOK)
	if refreshed := decodeJSON[struct {
		Refreshed int `json:"refreshed"`
	}](t, recorder); refreshed.Refreshed != 0 {
		t.Errorf("refreshed = %d, want none with no connected account", refreshed.Refreshed)
	}

	recorder = call(t, server, http.MethodPost, "/api/maintenance/oauth/states/prune", nil)
	requireStatus(t, recorder, http.StatusOK)
	if removed := decodeJSON[struct {
		Removed int64 `json:"removed"`
	}](t, recorder); removed.Removed != 0 {
		t.Errorf("removed = %d, want none on a fresh instance", removed.Removed)
	}

	// Without a body the retention of the scheduler applies.
	recorder = call(t, server, http.MethodPost, "/api/maintenance/runs/prune", nil)
	requireStatus(t, recorder, http.StatusOK)
	pruned := decodeJSON[struct {
		Keep   int  `json:"keep"`
		Pruned bool `json:"pruned"`
	}](t, recorder)
	if !pruned.Pruned || pruned.Keep != defaultKeepRuns {
		t.Errorf("prune answer = %+v, want the default retention %d", pruned, defaultKeepRuns)
	}

	// A retention out of bounds is refused instead of silently clamped.
	recorder = call(t, server, http.MethodPost, "/api/maintenance/runs/prune", map[string]any{"keep": 0})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	recorder = call(t, server, http.MethodPost, "/api/maintenance/runs/prune", map[string]any{"keep": maxKeepRuns + 1})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	recorder = call(t, server, http.MethodPost, "/api/maintenance/runs/prune", map[string]any{"keep": 10})
	requireStatus(t, recorder, http.StatusOK)
	if custom := decodeJSON[struct {
		Keep int `json:"keep"`
	}](t, recorder); custom.Keep != 10 {
		t.Errorf("keep = %d, want the requested 10", custom.Keep)
	}

	// The retention purge erases nothing on a fresh instance and answers the age
	// it applied to each of the three targets.
	recorder = call(t, server, http.MethodPost, "/api/maintenance/purge", nil)
	requireStatus(t, recorder, http.StatusOK)
	purged := decodeJSON[services.PurgeOutcome](t, recorder)
	if purged.Target != services.PurgeTargetAll || purged.Removed.Total() != 0 {
		t.Errorf("purge answer = %+v, want all/0 on a fresh instance", purged)
	}
	for _, target := range services.PurgeTargets {
		if purged.Days[target] != services.DefaultRetentionDays {
			t.Errorf("purge days = %v, want the seeded %d days for %s", purged.Days, services.DefaultRetentionDays, target)
		}
	}

	// A target may be purged on its own, with an explicit age.
	recorder = call(t, server, http.MethodPost, "/api/maintenance/purge", map[string]any{"target": "jobs", "days": 30})
	requireStatus(t, recorder, http.StatusOK)
	scoped := decodeJSON[services.PurgeOutcome](t, recorder)
	if len(scoped.Days) != 1 || scoped.Days[services.PurgeTargetJobs] != 30 {
		t.Errorf("purge days = %v, want the jobs target alone at 30 days", scoped.Days)
	}

	// An unknown target and an age out of bounds are validation errors.
	recorder = call(t, server, http.MethodPost, "/api/maintenance/purge", map[string]any{"target": "files"})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	recorder = call(t, server, http.MethodPost, "/api/maintenance/purge", map[string]any{"target": "billing", "days": -1})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)
}

// -----------------------------------------------------------------------------
// Settings
// -----------------------------------------------------------------------------

// TestSettingsReadAndUpdate covers the two panels of Admin > Settings: the local
// preferences and the raw key/value bag, which never leaks provider credentials.
func TestSettingsReadAndUpdate(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)
	cfg := testConfig(t, models.AuthModeNone)

	recorder := call(t, server, http.MethodGet, "/api/settings", nil)
	requireStatus(t, recorder, http.StatusOK)
	view := decodeJSON[struct {
		Settings services.AppSettings `json:"settings"`
		Locales  []string             `json:"locales"`
		Themes   []string             `json:"themes"`
	}](t, recorder)
	if view.Settings.DefaultLocale != cfg.DefaultLocale || view.Settings.DefaultTheme != cfg.DefaultTheme {
		t.Errorf("settings = %+v, want the configured defaults", view.Settings)
	}
	if view.Settings.SyncIntervalMinutes != services.DefaultSyncInterval || view.Settings.RunTimeoutMinutes != services.DefaultRunTimeout {
		t.Errorf("settings = %+v, want the seeded numeric defaults %d/%d",
			view.Settings, services.DefaultSyncInterval, services.DefaultRunTimeout)
	}
	if len(view.Locales) != len(config.SupportedLocales) || len(view.Themes) != len(config.SupportedThemes) {
		t.Errorf("locales/themes = %v/%v, want the supported vocabularies", view.Locales, view.Themes)
	}

	// An unsupported locale, theme or out-of-range number is refused.
	base := func(extra map[string]any) map[string]any {
		payload := map[string]any{
			"default_locale":                "en-US",
			"default_theme":                 "system",
			"sync_default_interval_minutes": 30,
			"sync_run_timeout_minutes":      60,
			"billing_schema":                "none",
			"billing_schema_custom":         "",
			"billing_rate_in_per_gib":       0,
			"billing_rate_out_per_gib":      0,
			"billing_currency":              "USD",
			"purge_enabled":                 true,
			"purge_jobs_days":               180,
			"purge_audits_days":             180,
			"purge_billing_days":            180,
		}
		for key, value := range extra {
			payload[key] = value
		}
		return payload
	}
	for name, payload := range map[string]map[string]any{
		"locale":            base(map[string]any{"default_locale": "xx-XX"}),
		"theme":             base(map[string]any{"default_theme": "neon"}),
		"interval":          base(map[string]any{"sync_default_interval_minutes": -1}),
		"run timeout":       base(map[string]any{"sync_run_timeout_minutes": 0}),
		"inbound rate":      base(map[string]any{"billing_rate_in_per_gib": -1}),
		"jobs retention":    base(map[string]any{"purge_jobs_days": 0}),
		"audits retention":  base(map[string]any{"purge_audits_days": services.MaxRetentionDays + 1}),
		"billing retention": base(map[string]any{"purge_billing_days": -5}),
	} {
		recorder = call(t, server, http.MethodPut, "/api/settings", payload)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("the %s payload: status = %d, want 400 (body %s)", name, recorder.Code, recorder.Body.String())
			continue
		}
		requireErrorCode(t, recorder, codeValidation)
	}

	// A valid update is applied and echoed back.
	recorder = call(t, server, http.MethodPut, "/api/settings", base(map[string]any{
		"default_locale":                "pt-BR",
		"default_theme":                 "dark",
		"sync_default_interval_minutes": 15,
		"sync_run_timeout_minutes":      45,
		"purge_enabled":                 false,
		"purge_jobs_days":               30,
	}))
	requireStatus(t, recorder, http.StatusOK)
	updated := decodeJSON[struct {
		Settings services.AppSettings `json:"settings"`
	}](t, recorder)
	if updated.Settings.DefaultLocale != "pt-BR" || updated.Settings.DefaultTheme != "dark" {
		t.Errorf("updated settings = %+v, want pt-BR/dark", updated.Settings)
	}
	if updated.Settings.SyncIntervalMinutes != 15 || updated.Settings.RunTimeoutMinutes != 45 {
		t.Errorf("updated settings = %+v, want 15/45", updated.Settings)
	}
	if updated.Settings.PurgeEnabled || updated.Settings.PurgeJobsDays != 30 {
		t.Errorf("updated settings = %+v, want the auto purge off and the 30 day retention", updated.Settings)
	}
	if updated.Settings.PurgeAuditsDays != 180 || updated.Settings.PurgeBillingDays != 180 {
		t.Errorf("updated settings = %+v, want the untouched retentions", updated.Settings)
	}

	// The session view reports the new defaults, so a reload keeps them.
	recorder = call(t, server, http.MethodGet, "/api/auth/session", nil)
	requireStatus(t, recorder, http.StatusOK)
	session := decodeJSON[sessionView](t, recorder)
	if session.Defaults.DefaultLocale != "pt-BR" || session.Defaults.SyncIntervalMinutes != 15 {
		t.Errorf("session defaults = %+v, want the stored settings", session.Defaults)
	}

	// The raw panel shows the plain keys and hides the credentials.
	recorder = call(t, server, http.MethodGet, "/api/settings/raw", nil)
	requireStatus(t, recorder, http.StatusOK)
	raw := decodeJSON[struct {
		Values map[string]string `json:"values"`
	}](t, recorder)
	if raw.Values["default_locale"] != "pt-BR" {
		t.Errorf("raw values = %v, want the updated locale", raw.Values)
	}
	for key := range raw.Values {
		if strings.HasPrefix(key, "provider.") || strings.Contains(strings.ToLower(key), "secret") {
			t.Errorf("raw values leak %q", key)
		}
	}
}

// -----------------------------------------------------------------------------
// Provider credentials
// -----------------------------------------------------------------------------

// TestProviderCredentialsEndpoints walks the Admin > Providers screen: the
// compiled-in providers are listed, the runtime override is stored encrypted, no
// secret is ever echoed, and clearing falls back to the environment.
func TestProviderCredentialsEndpoints(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	recorder := call(t, server, http.MethodGet, "/api/providers", nil)
	requireStatus(t, recorder, http.StatusOK)
	list := decodeJSON[struct {
		Providers    []services.ProviderCredentialsInfo `json:"providers"`
		RedirectHint map[string]string                  `json:"redirect_hint"`
	}](t, recorder)
	if len(list.Providers) != 2 {
		t.Fatalf("providers = %+v, want Google and Microsoft", list.Providers)
	}
	for _, info := range list.Providers {
		if info.Configured || info.SecretSet {
			t.Errorf("provider %s is configured without credentials", info.Provider)
		}
	}
	if list.RedirectHint["google"] == "" || list.RedirectHint["microsoft"] == "" {
		t.Errorf("redirect_hint = %v, want both callback paths", list.RedirectHint)
	}

	// One provider at a time, and only the ones this build ships.
	recorder = call(t, server, http.MethodGet, "/api/providers/google", nil)
	requireStatus(t, recorder, http.StatusOK)
	google := decodeJSON[services.ProviderCredentialsInfo](t, recorder)
	if google.Provider != models.ProviderGoogle || google.Source != services.SourceNone {
		t.Errorf("google = %+v, want an unconfigured Google entry", google)
	}

	recorder = call(t, server, http.MethodGet, "/api/providers/dropbox", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// The override is written once and reported as coming from the database.
	const secret = "gcp-client-secret-value"
	recorder = call(t, server, http.MethodPut, "/api/providers/google", map[string]any{
		"client_id":     "client-id.apps.googleusercontent.com",
		"client_secret": secret,
		"redirect_uri":  "https://sync.example.com/api/oauth/google/callback",
	})
	requireStatus(t, recorder, http.StatusOK)
	saved := decodeJSON[services.ProviderCredentialsInfo](t, recorder)
	if !saved.Configured || !saved.SecretSet {
		t.Errorf("saved = %+v, want a configured provider with a stored secret", saved)
	}
	if saved.Source != services.SourceDatabase {
		t.Errorf("source = %q, want %q", saved.Source, services.SourceDatabase)
	}
	if saved.ClientID != "client-id.apps.googleusercontent.com" {
		t.Errorf("client_id = %q, want the stored value", saved.ClientID)
	}
	if strings.Contains(recorder.Body.String(), secret) {
		t.Error("the answer echoed the client secret back")
	}

	// Clearing the override leaves the provider unconfigured, with no secret.
	recorder = call(t, server, http.MethodDelete, "/api/providers/google", nil)
	requireStatus(t, recorder, http.StatusOK)
	cleared := decodeJSON[services.ProviderCredentialsInfo](t, recorder)
	if cleared.Configured || cleared.SecretSet || cleared.ClientID != "" {
		t.Errorf("cleared = %+v, want an empty provider again", cleared)
	}
}

// -----------------------------------------------------------------------------
// Accounts and the OAuth guard rails
// -----------------------------------------------------------------------------

// TestAccountsAndOAuthGuards covers the account surface that never leaves the
// host: the list, the guard rails of the connect flow, the outcome redirect of
// the callback, and the deletion that reports the jobs it dropped.
func TestAccountsAndOAuthGuards(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)

	// Nothing is connected yet, and the connect screen still lists the build.
	recorder := call(t, server, http.MethodGet, "/api/accounts", nil)
	requireStatus(t, recorder, http.StatusOK)
	list := decodeJSON[struct {
		Accounts []accountView `json:"accounts"`
	}](t, recorder)
	if len(list.Accounts) != 0 {
		t.Fatalf("accounts = %+v, want none on a fresh instance", list.Accounts)
	}

	recorder = call(t, server, http.MethodGet, "/api/oauth", nil)
	requireStatus(t, recorder, http.StatusOK)
	offered := decodeJSON[struct {
		Providers []services.ProviderCredentialsInfo `json:"providers"`
	}](t, recorder)
	if len(offered.Providers) != 2 {
		t.Errorf("providers = %+v, want the two compiled-in providers", offered.Providers)
	}
	// The object shape (not bare names) and `configured` are what the connect
	// screen reads: it offers a provider only once its OAuth client is complete,
	// so a fresh instance advertises both and offers none.
	for _, info := range offered.Providers {
		if info.Configured {
			t.Errorf("%s: reported as configured without an OAuth client", info.Provider)
		}
	}

	// An unknown provider is refused before any state is created.
	recorder = call(t, server, http.MethodPost, "/api/oauth/dropbox/start", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// A known provider without an OAuth client reports the missing setup, which
	// the SPA turns into a link to Admin > Providers.
	recorder = call(t, server, http.MethodPost, "/api/oauth/google/start", nil)
	requireStatus(t, recorder, http.StatusFailedDependency)
	requireErrorCode(t, recorder, codeNotConfigured)

	// The callback never trusts a state it did not issue: the browser goes back
	// to the accounts screen carrying the reason instead of an error page.
	recorder = call(t, server, http.MethodGet, "/api/oauth/google/callback?code=stolen&state=forged", nil)
	requireStatus(t, recorder, http.StatusFound)
	if location := recorder.Result().Header.Get("Location"); !strings.Contains(location, "connect_error="+codeUnauthorized) {
		t.Errorf("location = %q, want the %q reason", location, codeUnauthorized)
	}

	// A provider that reports a denial is handled by the same redirect.
	recorder = call(t, server, http.MethodGet, "/api/oauth/google/callback?error=access_denied&error_description=user+refused", nil)
	requireStatus(t, recorder, http.StatusFound)
	location := recorder.Result().Header.Get("Location")
	if !strings.Contains(location, "connect_error=denied") || !strings.Contains(location, "user+refused") {
		t.Errorf("location = %q, want the denial and its detail", location)
	}

	// A connected account is listed without its tokens and shows its identity.
	google := saveAccount(t, store, models.ProviderGoogle, "google-1", "g@example.com")
	microsoft := saveAccount(t, store, models.ProviderMicrosoft, "ms-1", "m@example.com")
	createJob(t, server, google, microsoft)

	recorder = call(t, server, http.MethodGet, "/api/accounts", nil)
	requireStatus(t, recorder, http.StatusOK)
	list = decodeJSON[struct {
		Accounts []accountView `json:"accounts"`
	}](t, recorder)
	if len(list.Accounts) != 2 {
		t.Fatalf("accounts = %+v, want the two saved accounts", list.Accounts)
	}
	for _, account := range list.Accounts {
		if account.Email == "" || account.Status != "connected" || account.ProviderAccountID == "" {
			t.Errorf("account = %+v, want a connected account with an identity", account)
		}
	}
	if body := recorder.Body.String(); strings.Contains(body, "access_token") || strings.Contains(body, "refresh_token") {
		t.Errorf("the account view leaks token columns: %s", body)
	}

	// Deleting an account also drops the jobs that used it, and says how many.
	recorder = call(t, server, http.MethodDelete, "/api/accounts/"+strconv.FormatUint(uint64(google.ID), 10), nil)
	requireStatus(t, recorder, http.StatusOK)
	deleted := decodeJSON[struct {
		ID          uint  `json:"id"`
		DeletedJobs int64 `json:"deleted_jobs"`
	}](t, recorder)
	if deleted.ID != google.ID || deleted.DeletedJobs != 1 {
		t.Errorf("delete answer = %+v, want the account and its single job", deleted)
	}

	recorder = call(t, server, http.MethodGet, "/api/jobs", nil)
	requireStatus(t, recorder, http.StatusOK)
	if jobs := decodeJSON[struct {
		Jobs []jobView `json:"jobs"`
	}](t, recorder); len(jobs.Jobs) != 0 {
		t.Errorf("jobs = %+v, want none left after the account was removed", jobs.Jobs)
	}

	// Removing it a second time reports the missing account.
	recorder = call(t, server, http.MethodDelete, "/api/accounts/"+strconv.FormatUint(uint64(google.ID), 10), nil)
	requireStatus(t, recorder, http.StatusNotFound)
	requireErrorCode(t, recorder, codeNotFound)

	// The account surface validates the identifiers of its paths.
	recorder = call(t, server, http.MethodDelete, "/api/accounts/not-a-number", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)
}

// -----------------------------------------------------------------------------
// History and dashboard
// -----------------------------------------------------------------------------

// seedRun stores a finished run of a job together with its file operations, the
// way the engine records a completed pass.
func seedRun(t *testing.T, store *services.Store, job jobView, startedAt time.Time, run models.SyncRun, items []models.SyncItem) models.SyncRun {
	t.Helper()
	run.JobID = job.ID
	run.JobName = job.Name
	run.StartedAt = startedAt
	finished := startedAt.Add(time.Minute)
	run.FinishedAt = &finished
	run.DurationMS = finished.Sub(startedAt).Milliseconds()

	ctx := context.Background()
	if err := store.CreateRun(ctx, &run); err != nil {
		t.Fatalf("creating the run: %v", err)
	}
	for i := range items {
		items[i].RunID = run.ID
		items[i].JobID = job.ID
	}
	if err := store.AddRunItems(ctx, items); err != nil {
		t.Fatalf("storing the run items: %v", err)
	}
	if err := store.FinishRun(ctx, &run, nil); err != nil {
		t.Fatalf("finishing the run: %v", err)
	}
	return run
}

// TestIdentifierParametersCoverTheWholeRange pins the parsing of the identifier
// parameters: a value that does not fit the identifier type is a 400 (never a
// silent wrap to a smaller number) and a well formed but unknown id is a 404.
func TestIdentifierParametersCoverTheWholeRange(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	type idCase struct {
		name string
		path string
		want int
	}
	cases := []idCase{
		{"not a number", "/api/runs/abc", http.StatusBadRequest},
		{"zero", "/api/runs/0", http.StatusBadRequest},
		{"negative", "/api/runs/-1", http.StatusBadRequest},
		{"hex", "/api/runs/0x10", http.StatusBadRequest},
		{"float", "/api/runs/1.5", http.StatusBadRequest},
		// One past the largest uint64: refused on every architecture.
		{"uint64 overflow", "/api/runs/18446744073709551616", http.StatusBadRequest},
	}
	// A value beyond 32 bits is a valid identifier only where uint is 64 bits
	// wide; elsewhere it must be refused instead of truncated.
	wide := http.StatusNotFound
	if strconv.IntSize < 64 {
		wide = http.StatusBadRequest
	}
	cases = append(cases,
		idCase{"uint64 max", "/api/runs/18446744073709551615", wide},
		idCase{"just past 32 bits", "/api/runs/4294967296", wide},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := call(t, server, http.MethodGet, tc.path, nil)
			requireStatus(t, recorder, tc.want)
			if tc.want == http.StatusBadRequest {
				requireErrorCode(t, recorder, codeValidation)
			}
		})
	}

	// The same rules apply to the optional `job_id` query parameter of the list.
	for _, query := range []string{"abc", "0x10", "-1", "18446744073709551616"} {
		recorder := call(t, server, http.MethodGet, "/api/runs?job_id="+query, nil)
		requireStatus(t, recorder, http.StatusBadRequest)
		requireErrorCode(t, recorder, codeValidation)
	}
	// An id that fits is a filter, not an error: an unknown job simply matches no
	// run, so the answer stays 200.
	accepted := []string{"", "1"}
	if strconv.IntSize == 64 {
		accepted = append(accepted, "4294967296", "18446744073709551615")
	}
	for _, query := range accepted {
		recorder := call(t, server, http.MethodGet, "/api/runs?job_id="+query, nil)
		requireStatus(t, recorder, http.StatusOK)
	}
}

// TestRunHistoryAndDashboard covers the read-only side of the engine: the
// history list with its filters, one run with its file operations, and the
// aggregate the home screen and Admin > About display.
func TestRunHistoryAndDashboard(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	google := saveAccount(t, store, models.ProviderGoogle, "google-1", "g@example.com")
	microsoft := saveAccount(t, store, models.ProviderMicrosoft, "ms-1", "m@example.com")
	job := createJob(t, server, google, microsoft)
	jobID := strconv.FormatUint(uint64(job.ID), 10)

	// A successful run of two hours ago, one older than the thirty day default
	// window of the dashboard, and the run the engine is still working on.
	success := seedRun(t, store, job, fixedNow.Add(-2*time.Hour), models.SyncRun{
		Status:           string(models.RunSuccess),
		Trigger:          string(models.TriggerScheduled),
		FilesCreated:     3,
		FilesUpdated:     1,
		BytesTransferred: 4096,
	}, []models.SyncItem{
		{Action: "created", Path: "/Documents/a.txt", Size: 1024},
		{Action: "created", Path: "/Documents/b.txt", Size: 2048, Evidence: "destination newer by 30s"},
		{Action: "updated", Path: "/Documents/c.txt", Size: 1024},
	})

	old := seedRun(t, store, job, fixedNow.Add(-40*24*time.Hour), models.SyncRun{
		Status:  string(models.RunFailed),
		Trigger: string(models.TriggerScheduled),
		Message: "the destination is unreachable",
	}, nil)

	running := &models.SyncRun{
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    string(models.RunRunning),
		Trigger:   string(models.TriggerManual),
		StartedAt: fixedNow.Add(-5 * time.Minute),
	}
	if err := store.CreateRun(context.Background(), running); err != nil {
		t.Fatalf("creating the running run: %v", err)
	}

	// The history lists every run, newest first, and flags the live one so the
	// screen can offer Cancel without a second request.
	recorder := call(t, server, http.MethodGet, "/api/runs", nil)
	requireStatus(t, recorder, http.StatusOK)
	history := decodeJSON[struct {
		Runs []runView `json:"runs"`
	}](t, recorder)
	if len(history.Runs) != 3 {
		t.Fatalf("runs = %+v, want the three seeded runs", history.Runs)
	}
	if history.Runs[0].ID != running.ID || !history.Runs[0].Running {
		t.Errorf("first run = %+v, want the running run %d", history.Runs[0], running.ID)
	}
	// The page is ordered by insertion (the identifier), newest first.
	if history.Runs[1].ID != old.ID || history.Runs[2].ID != success.ID {
		t.Errorf("run order = %d/%d, want %d then %d", history.Runs[1].ID, history.Runs[2].ID, old.ID, success.ID)
	}

	// job_id and status narrow the page; a malformed id is refused instead of
	// silently listing everything.
	recorder = call(t, server, http.MethodGet, "/api/runs?job_id="+jobID+"&status="+string(models.RunSuccess), nil)
	requireStatus(t, recorder, http.StatusOK)
	filtered := decodeJSON[struct {
		Runs []runView `json:"runs"`
	}](t, recorder)
	if len(filtered.Runs) != 1 || filtered.Runs[0].ID != success.ID {
		t.Errorf("filtered runs = %+v, want only the successful run %d", filtered.Runs, success.ID)
	}

	recorder = call(t, server, http.MethodGet, "/api/runs?job_id=4242", nil)
	requireStatus(t, recorder, http.StatusOK)
	if other := decodeJSON[struct {
		Runs []runView `json:"runs"`
	}](t, recorder); len(other.Runs) != 0 {
		t.Errorf("runs of an unknown job = %+v, want none", other.Runs)
	}

	recorder = call(t, server, http.MethodGet, "/api/runs?job_id=not-a-number", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// One run carries its file operations, evidence included.
	recorder = call(t, server, http.MethodGet, "/api/runs/"+strconv.FormatUint(uint64(success.ID), 10), nil)
	requireStatus(t, recorder, http.StatusOK)
	detail := decodeJSON[struct {
		Run   runView       `json:"run"`
		Items []runItemView `json:"items"`
	}](t, recorder)
	if detail.Run.ID != success.ID || len(detail.Items) != 3 {
		t.Fatalf("detail = %+v, want the run with its three file operations", detail)
	}
	for _, item := range detail.Items {
		if item.Path == "" || item.Action == "" {
			t.Errorf("item = %+v, want an action and a path", item)
		}
	}
	if detail.Items[1].Evidence == "" {
		t.Errorf("item = %+v, want the decision evidence kept", detail.Items[1])
	}

	// The expanded view of the history screen is the same list, paginated.
	recorder = call(t, server, http.MethodGet, "/api/runs/"+strconv.FormatUint(uint64(success.ID), 10)+"/items?limit=2", nil)
	requireStatus(t, recorder, http.StatusOK)
	page := decodeJSON[struct {
		Items []runItemView `json:"items"`
		Limit int           `json:"limit"`
	}](t, recorder)
	if page.Limit != 2 || len(page.Items) != 2 {
		t.Errorf("page = %d items with limit %d, want two of the three", len(page.Items), page.Limit)
	}

	// An unknown run is a 404, a malformed id a 400.
	recorder = call(t, server, http.MethodGet, "/api/runs/4242", nil)
	requireStatus(t, recorder, http.StatusNotFound)
	requireErrorCode(t, recorder, codeNotFound)

	recorder = call(t, server, http.MethodGet, "/api/runs/4242/items", nil)
	requireStatus(t, recorder, http.StatusNotFound)
	requireErrorCode(t, recorder, codeNotFound)

	recorder = call(t, server, http.MethodGet, "/api/runs/not-a-number/items", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// The dashboard defaults to a thirty day window, so the failed run from
	// forty days ago is outside it while the two recent ones are counted.
	recorder = call(t, server, http.MethodGet, "/api/stats", nil)
	requireStatus(t, recorder, http.StatusOK)
	window := decodeJSON[struct {
		Days        int            `json:"days"`
		Stats       services.Stats `json:"stats"`
		RunningJobs int            `json:"running_jobs"`
		RunningIDs  []uint         `json:"running_ids"`
	}](t, recorder)
	if window.Days != 30 {
		t.Errorf("days = %d, want the thirty day default", window.Days)
	}
	if window.Stats.Accounts != 2 || window.Stats.Jobs != 1 || window.Stats.EnabledJobs != 1 {
		t.Errorf("stats = %+v, want the two accounts and the single enabled job", window.Stats)
	}
	if window.Stats.Runs != 2 || window.Stats.Succeeded != 1 || window.Stats.Failed != 0 {
		t.Errorf("stats = %+v, want the two runs of the window (one successful)", window.Stats)
	}
	if window.Stats.FilesCreated != 3 || window.Stats.FilesUpdated != 1 || window.Stats.Bytes != 4096 {
		t.Errorf("stats = %+v, want the counters of the successful run", window.Stats)
	}
	// Nothing is running in this process: a stored `running` row is history, not
	// a live job, which is exactly the distinction the dashboard shows.
	if window.RunningJobs != 0 || len(window.RunningIDs) != 0 {
		t.Errorf("running = %d/%v, want no job in flight", window.RunningJobs, window.RunningIDs)
	}

	// days=0 is the Admin > About figure: everything ever recorded.
	recorder = call(t, server, http.MethodGet, "/api/stats?days=0", nil)
	requireStatus(t, recorder, http.StatusOK)
	all := decodeJSON[struct {
		Stats services.Stats `json:"stats"`
	}](t, recorder)
	if all.Stats.Runs != 3 || all.Stats.Failed != 1 {
		t.Errorf("stats = %+v, want every run including the failed one", all.Stats)
	}
}

// -----------------------------------------------------------------------------
// Account screens backed by a provider
// -----------------------------------------------------------------------------

// TestAccountScreensThroughProvider drives the account endpoints that talk to the
// cloud: "test this account", the drive picker, the folder browser and the site
// search. A stub provider replaces the network, so the assertions are about the
// contract the SPA consumes, the ordering the browser relies on and the error
// mapping when the remote service answers badly.
func TestAccountScreensThroughProvider(t *testing.T) {
	stub := &stubProvider{
		name: models.ProviderGoogle,
		account: providers.Account{
			ID: "remote-1", Email: "operator@example.com", Name: "Operator",
			AvatarURL: "https://img.example.com/operator.png",
		},
		drives: []providers.Drive{{ID: "drive-1", Name: "My Drive", Kind: "personal"}},
		children: []providers.Item{
			{ID: "file-2", Name: "zeta.txt", Size: 10, MimeType: "text/plain"},
			{ID: "folder-2", Name: "Archive", IsDir: true},
			{ID: "folder-1", Name: "albums", IsDir: true},
			{ID: "file-1", Name: "Alpha.txt", Size: 20, Hash: "abc", NativeDoc: true, ModifiedAt: fixedNow},
		},
		sites: []providers.Drive{{ID: "site-1", Name: "Marketing", Kind: "document_library", Owner: "Ada"}},
	}

	server, store := newTestServerWith(t, models.AuthModeNone, testOptions{
		configure: func(cfg *config.Config) {
			// The endpoints resolve the OAuth client of the provider before
			// they call it, so the stub needs a configured pair.
			cfg.Google = config.ProviderCredentials{
				ClientID:     "stub-client",
				ClientSecret: "stub-secret",
				RedirectURI:  "https://sync.example.com/api/oauth/google/callback",
			}
		},
		providers: []providers.Provider{stub},
	})

	account := saveAccount(t, store, models.ProviderGoogle, "remote-1", "operator@example.com")
	if err := server.deps.Tokens.Persist(context.Background(), account, &providers.Tokens{
		AccessToken:  "stub-access",
		RefreshToken: "stub-refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
		Scopes:       []string{"files.readwrite"},
	}); err != nil {
		t.Fatalf("storing the tokens: %v", err)
	}
	id := strconv.FormatUint(uint64(account.ID), 10)

	// "Test this account" reads the remote identity and reports the stored
	// scopes, so the operator knows what access was granted.
	recorder := call(t, server, http.MethodPost, "/api/accounts/"+id+"/verify", nil)
	requireStatus(t, recorder, http.StatusOK)
	verified := decodeJSON[accountView](t, recorder)
	if verified.Email != "operator@example.com" || verified.DisplayName != "Operator" || verified.Status != "connected" {
		t.Errorf("verified account = %+v, want the remote identity", verified)
	}
	if len(verified.Scopes) != 1 || verified.Scopes[0] != "files.readwrite" {
		t.Errorf("scopes = %v, want the scope granted by the provider", verified.Scopes)
	}
	// The identity read from the provider is written back, so a renamed profile
	// shows up on the account card.
	recorder = call(t, server, http.MethodGet, "/api/accounts", nil)
	requireStatus(t, recorder, http.StatusOK)
	listed := decodeJSON[struct {
		Accounts []accountView `json:"accounts"`
	}](t, recorder)
	if len(listed.Accounts) != 1 || listed.Accounts[0].DisplayName != "Operator" ||
		listed.Accounts[0].AvatarURL != "https://img.example.com/operator.png" {
		t.Errorf("accounts = %+v, want the identity refreshed by the test button", listed.Accounts)
	}

	// The drive picker.
	recorder = call(t, server, http.MethodGet, "/api/accounts/"+id+"/drives", nil)
	requireStatus(t, recorder, http.StatusOK)
	picker := decodeJSON[struct {
		Drives []driveView `json:"drives"`
	}](t, recorder)
	if len(picker.Drives) != 1 || picker.Drives[0].ID != "drive-1" || picker.Drives[0].Kind != "personal" {
		t.Errorf("drives = %+v, want the single personal drive", picker.Drives)
	}

	// The folder browser: folders first, then files, both alphabetically and
	// ignoring case, with the types V1 cannot transfer flagged.
	recorder = call(t, server, http.MethodGet, "/api/accounts/"+id+"/drives/drive-1/items", nil)
	requireStatus(t, recorder, http.StatusOK)
	browser := decodeJSON[struct {
		DriveID  string     `json:"drive_id"`
		FolderID string     `json:"folder_id"`
		Items    []itemView `json:"items"`
	}](t, recorder)
	if browser.DriveID != "drive-1" || browser.FolderID != providers.DriveRoot {
		t.Errorf("browse = %q/%q, want drive-1 at the root of the drive", browser.DriveID, browser.FolderID)
	}
	want := []string{"albums", "Archive", "Alpha.txt", "zeta.txt"}
	if len(browser.Items) != len(want) {
		t.Fatalf("items = %+v, want %d entries", browser.Items, len(want))
	}
	for i, name := range want {
		if browser.Items[i].Name != name {
			t.Errorf("item %d = %q, want %q", i, browser.Items[i].Name, name)
		}
	}
	if !browser.Items[0].IsDir || browser.Items[2].Unsupported != true || browser.Items[2].Hash != "abc" {
		t.Errorf("items = %+v, want the folders marked and the native document flagged", browser.Items)
	}

	// Browsing a subfolder forwards the drive and the folder to the provider.
	recorder = call(t, server, http.MethodGet, "/api/accounts/"+id+"/drives/drive-1/items?folder_id=folder-1", nil)
	requireStatus(t, recorder, http.StatusOK)
	if len(stub.asked) != 2 || stub.asked[1] != "drive-1/folder-1" {
		t.Errorf("provider was asked for %v, want the browsed subfolder last", stub.asked)
	}

	// The SharePoint style search: only matching libraries come back.
	recorder = call(t, server, http.MethodGet, "/api/accounts/"+id+"/sites?q=mark", nil)
	requireStatus(t, recorder, http.StatusOK)
	sites := decodeJSON[struct {
		Sites []driveView `json:"sites"`
	}](t, recorder)
	if len(sites.Sites) != 1 || sites.Sites[0].Owner != "Ada" {
		t.Errorf("sites = %+v, want the matching library", sites.Sites)
	}

	recorder = call(t, server, http.MethodGet, "/api/accounts/"+id+"/sites?q=payroll", nil)
	requireStatus(t, recorder, http.StatusOK)
	if none := decodeJSON[struct {
		Sites []driveView `json:"sites"`
	}](t, recorder); len(none.Sites) != 0 {
		t.Errorf("sites = %+v, want none for an unmatched query", none.Sites)
	}

	// The manual fallback: a pasted SharePoint URL resolves to the library of
	// that site, for the libraries the keyword search does not surface.
	recorder = call(t, server, http.MethodPost, "/api/accounts/"+id+"/sites/resolve",
		map[string]string{"url": "https://contoso.sharepoint.com/sites/marketing/Shared%20Documents"})
	requireStatus(t, recorder, http.StatusOK)
	resolved := decodeJSON[struct {
		Sites []driveView `json:"sites"`
	}](t, recorder)
	if len(resolved.Sites) != 1 || resolved.Sites[0].ID != "site-1" || resolved.Sites[0].Owner != "Ada" {
		t.Errorf("resolved = %+v, want the library behind the pasted URL", resolved.Sites)
	}

	// A URL that names no site the account can reach is a 404 the operator can
	// act on, not a 500 from an opaque provider failure.
	recorder = call(t, server, http.MethodPost, "/api/accounts/"+id+"/sites/resolve",
		map[string]string{"url": "https://contoso.sharepoint.com/sites/payroll"})
	requireStatus(t, recorder, http.StatusNotFound)
	requireErrorCode(t, recorder, codeNotFound)
}

// TestAccountScreensRefreshAndFailures covers the paths that decide whether the
// account screens can be trusted: an expired access token is renewed behind the
// operator's back (and only once), a provider that refuses an identifier answers
// a 400 the caller can act on, a provider that fails answers with a mapped
// status whose body never leaks the provider's own message, and a refused
// refresh asks for a reconnection instead of pretending the account works.
func TestAccountScreensRefreshAndFailures(t *testing.T) {
	unreachable := errors.New("stub: 500 from drive.internal.example.com")

	stub := &stubProvider{
		name:    models.ProviderGoogle,
		account: providers.Account{ID: "remote-1", Email: "operator@example.com", Name: "Operator"},
		drives:  []providers.Drive{{ID: "drive-1", Name: "My Drive", Kind: "personal"}},
	}

	server, store := newTestServerWith(t, models.AuthModeNone, testOptions{
		configure: func(cfg *config.Config) {
			cfg.Google = config.ProviderCredentials{
				ClientID:     "stub-client",
				ClientSecret: "stub-secret",
				RedirectURI:  "https://sync.example.com/api/oauth/google/callback",
			}
		},
		providers: []providers.Provider{stub},
	})

	account := saveAccount(t, store, models.ProviderGoogle, "remote-1", "operator@example.com")
	if err := server.deps.Tokens.Persist(context.Background(), account, &providers.Tokens{
		AccessToken:  "stub-fresh",
		RefreshToken: "stub-refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
		Scopes:       []string{"files.readwrite"},
	}); err != nil {
		t.Fatalf("storing the tokens: %v", err)
	}
	id := strconv.FormatUint(uint64(account.ID), 10)

	// An unknown account and a malformed identifier never reach the provider.
	recorder := call(t, server, http.MethodGet, "/api/accounts/4242/drives", nil)
	requireStatus(t, recorder, http.StatusNotFound)
	requireErrorCode(t, recorder, codeNotFound)

	recorder = call(t, server, http.MethodGet, "/api/accounts/not-a-number/drives", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	recorder = call(t, server, http.MethodPost, "/api/accounts/not-a-number/verify", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// A provider that refuses an identifier the caller supplied (a folder id in
	// an explorer link, a drive id in a stored job) is neither a server failure
	// nor a missing resource: the value is wrong, so the answer is 400 with the
	// validation code and the caller is told to fix it.
	stub.failure = providers.ErrInvalidIdentifier
	recorder = call(t, server, http.MethodGet, "/api/accounts/"+id+"/drives/drive-1/items?folder_id=../permissions", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)
	stub.failure = nil

	// A provider that fails while the token is still valid is a server side
	// failure: 500, with the provider's own message left in the logs.
	stub.failure = unreachable
	recorder = call(t, server, http.MethodGet, "/api/accounts/"+id+"/drives", nil)
	requireStatus(t, recorder, http.StatusInternalServerError)
	requireErrorCode(t, recorder, codeInternal)
	if body := recorder.Body.String(); strings.Contains(body, "drive.internal.example.com") {
		t.Errorf("body = %s, want the provider detail left in the logs", body)
	}
	if stub.refreshes != 0 {
		t.Errorf("refreshes = %d, want no refresh while the token is valid", stub.refreshes)
	}

	// Once the stored access token is expired the call renews it before it
	// reaches the provider, and the renewal is stored: that is what keeps a busy
	// dashboard from refreshing on every single request.
	stub.failure = nil
	if err := server.deps.Tokens.Persist(context.Background(), account, &providers.Tokens{
		AccessToken:  "stub-stale",
		RefreshToken: "stub-refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(-time.Minute),
		Scopes:       []string{"files.readwrite"},
	}); err != nil {
		t.Fatalf("expiring the token: %v", err)
	}
	recorder = call(t, server, http.MethodGet, "/api/accounts/"+id+"/drives", nil)
	requireStatus(t, recorder, http.StatusOK)
	if stub.refreshes != 1 {
		t.Fatalf("refreshes = %d, want the expired token renewed once", stub.refreshes)
	}

	stub.failure = nil
	for i := 0; i < 2; i++ {
		recorder = call(t, server, http.MethodGet, "/api/accounts/"+id+"/drives", nil)
		requireStatus(t, recorder, http.StatusOK)
	}
	if stub.refreshes != 1 {
		t.Errorf("refreshes = %d, want the renewed token reused", stub.refreshes)
	}

	stored, err := store.GetAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("reading the account: %v", err)
	}
	if stored.Status != string(models.AccountConnected) || stored.LastError != "" || !stored.ExpiresAt.After(time.Now()) {
		t.Errorf("account = %+v, want a connected account holding a fresh token", stored)
	}

	// A refresh the provider refuses is not fatal to the account, but it is a
	// reconnection: 412 with the `reconnect` code is the banner the accounts
	// screen keys off.
	if err := server.deps.Tokens.Persist(context.Background(), stored, &providers.Tokens{
		AccessToken:  "stub-stale",
		RefreshToken: "stub-refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(-time.Minute),
		Scopes:       []string{"files.readwrite"},
	}); err != nil {
		t.Fatalf("expiring the token again: %v", err)
	}
	stub.failure = unreachable
	recorder = call(t, server, http.MethodGet, "/api/accounts/"+id+"/sites?q=mark", nil)
	requireStatus(t, recorder, http.StatusPreconditionFailed)
	requireErrorCode(t, recorder, codeReconnect)

	stored, err = store.GetAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("reading the account: %v", err)
	}
	if stored.Status != string(models.AccountError) || stored.LastError == "" {
		t.Errorf("account = %+v, want the failed refresh flagged on the account", stored)
	}
}

// -----------------------------------------------------------------------------
// Audits
// -----------------------------------------------------------------------------

// waitAuditRun polls the store until an audit leaves the running state. The
// walk writes from its own goroutine, so a transient SQLite lock between the two
// is retried instead of failing the test.
func waitAuditRun(t *testing.T, store *services.Store, id uint) *models.AuditRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := store.GetAudit(context.Background(), id)
		if err != nil {
			if strings.Contains(err.Error(), "locked") && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
				continue
			}
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

// TestAuditLifecycle follows a content audit end to end: the start request is
// accepted (202) and polled, the report reads in DFS preorder, the history lists
// it, the CSV export carries the documented contract and the maintenance prune
// uses the audit retention.
func TestAuditLifecycle(t *testing.T) {
	stub := &stubProvider{
		name:    models.ProviderGoogle,
		account: providers.Account{ID: "remote-1", Email: "operator@example.com", Name: "Operator"},
		children: []providers.Item{
			{ID: "file-2", Name: "zeta.txt", Size: 10, MimeType: "text/plain"},
			{ID: "folder-2", Name: "Archive", IsDir: true},
			{ID: "folder-1", Name: "albums", IsDir: true},
			{ID: "file-1", Name: "Alpha.txt", Size: 20, ModifiedAt: fixedNow},
		},
		// A real tree below the folders: `albums` holds one file and `Archive` is
		// empty. The nested file stays out of a depth 1 report, but its bytes must
		// still reach the totals of its folder and of the run.
		childrenByFolder: map[string][]providers.Item{
			"folder-1": {{ID: "file-3", Name: "inside.txt", Size: 5}},
			"folder-2": {},
		},
	}
	server, store := newTestServerWith(t, models.AuthModeNone, testOptions{
		configure: func(cfg *config.Config) {
			// The audit resolves the OAuth client of the account's provider
			// before it lists anything, so the stub needs a configured pair.
			cfg.Google = config.ProviderCredentials{
				ClientID:     "stub-client",
				ClientSecret: "stub-secret",
				RedirectURI:  "https://sync.example.com/api/oauth/google/callback",
			}
		},
		providers: []providers.Provider{stub},
	})
	account := saveAccount(t, store, models.ProviderGoogle, "remote-1", "operator@example.com")
	if err := server.deps.Tokens.Persist(context.Background(), account, &providers.Tokens{
		AccessToken:  "stub-access",
		RefreshToken: "stub-refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
		Scopes:       []string{"files.readwrite"},
	}); err != nil {
		t.Fatalf("storing the tokens: %v", err)
	}

	// A negative depth is refused before anything is written.
	recorder := call(t, server, http.MethodPost, "/api/audits", map[string]any{
		"account_id": account.ID,
		"depth":      -1,
	})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// Start the audit: the answer is the row to poll (202). The drive display
	// parts travel with the drive id, so the history entry names the root.
	recorder = call(t, server, http.MethodPost, "/api/audits", map[string]any{
		"account_id":  account.ID,
		"drive_id":    "drive-1",
		"drive_name":  "  My Drive  ",
		"drive_kind":  "personal",
		"folder_path": "/",
		"depth":       1,
	})
	requireStatus(t, recorder, http.StatusAccepted)
	started := decodeJSON[struct {
		Audit auditView `json:"audit"`
	}](t, recorder)
	if started.Audit.ID == 0 || started.Audit.AccountID != account.ID {
		t.Fatalf("started audit = %+v", started.Audit)
	}
	if started.Audit.DriveName != "My Drive" || started.Audit.DriveKind != "personal" {
		t.Errorf("started audit drive = %q/%q, want the My Drive label",
			started.Audit.DriveName, started.Audit.DriveKind)
	}

	finished := waitAuditRun(t, store, started.Audit.ID)
	if finished.Status != string(models.RunSuccess) {
		t.Fatalf("status = %q (%s)", finished.Status, finished.Message)
	}
	if finished.Files != 3 || finished.Folders != 3 || finished.TotalSize != 35 {
		t.Fatalf("summary = files %d, folders %d, size %d; want 3, 3, 35 (the nested file of `albums` counts)",
			finished.Files, finished.Folders, finished.TotalSize)
	}
	if finished.MaxDepthReached != 2 {
		t.Fatalf("max depth = %d, want 2: the depth bounds the report, not the walk", finished.MaxDepthReached)
	}

	// The report reads in DFS preorder, folders first then files, and stops at the
	// requested depth: `inside.txt` is counted but never listed.
	id := strconv.FormatUint(uint64(started.Audit.ID), 10)
	recorder = call(t, server, http.MethodGet, "/api/audits/"+id, nil)
	requireStatus(t, recorder, http.StatusOK)
	report := decodeJSON[struct {
		Audit   auditView        `json:"audit"`
		Entries []auditEntryView `json:"entries"`
	}](t, recorder)
	wantPaths := []string{"", "albums", "Archive", "Alpha.txt", "zeta.txt"}
	if len(report.Entries) != len(wantPaths) {
		t.Fatalf("entries = %+v, want %d", report.Entries, len(wantPaths))
	}
	for i, want := range wantPaths {
		if report.Entries[i].Path != want {
			t.Fatalf("entry %d = %q, want %q", i, report.Entries[i].Path, want)
		}
	}
	// The boundary folder is listed with its exact subtree and says through Listed
	// that its children are not part of the report; the root aggregates everything.
	boundary := report.Entries[1]
	if !boundary.Expanded || boundary.Listed || boundary.TotalSize != 5 || boundary.Files != 1 {
		t.Fatalf("boundary folder = %+v, want a walked, unlisted folder whose 5 bytes are counted", boundary)
	}
	root := report.Entries[0]
	if !root.Listed || root.TotalSize != 35 || root.Files != 3 || root.Folders != 2 {
		t.Fatalf("root entry = %+v, want the exact totals of the whole tree", root)
	}

	// The history lists it.
	recorder = call(t, server, http.MethodGet, "/api/audits", nil)
	requireStatus(t, recorder, http.StatusOK)
	list := decodeJSON[struct {
		Audits []auditView `json:"audits"`
	}](t, recorder)
	if len(list.Audits) != 1 || list.Audits[0].ID != started.Audit.ID {
		t.Fatalf("audits = %+v, want the started audit only", list.Audits)
	}

	// The CSV export carries the documented headers, the BOM and the rows.
	recorder = call(t, server, http.MethodGet, "/api/audits/"+id+"/export", nil)
	requireStatus(t, recorder, http.StatusOK)
	if ct := recorder.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type = %q, want text/csv", ct)
	}
	if cd := recorder.Header().Get("Content-Disposition"); !strings.Contains(cd, "audit-"+id+".csv") {
		t.Fatalf("content-disposition = %q, want the audit file name", cd)
	}
	body := recorder.Body.String()
	if !strings.HasPrefix(body, "\ufeff") {
		t.Fatal("the CSV must start with a UTF-8 BOM")
	}
	if !strings.Contains(body, "kind,path,name,depth,size,total_size,files,folders,expanded,listed,modified_at,mime_type") {
		t.Fatalf("the CSV header is missing: %q", body)
	}
	if !strings.Contains(body, "folder,albums,albums,1,0,5,1,0,true,false,,") {
		t.Fatalf("the CSV must carry the counted subtree of a folder whose children are not listed: %q", body)
	}
	if !strings.Contains(body, "file,Alpha.txt,Alpha.txt,1,20,20,1,0,false,false,"+fixedNow.Format(time.RFC3339)+",") {
		t.Fatalf("the CSV rows are missing: %q", body)
	}

	// Cancelling a finished audit is idempotent.
	recorder = call(t, server, http.MethodPost, "/api/audits/"+id+"/cancel", nil)
	requireStatus(t, recorder, http.StatusOK)

	// A missing audit is a 404 and a malformed id a 400.
	recorder = call(t, server, http.MethodGet, "/api/audits/4242", nil)
	requireStatus(t, recorder, http.StatusNotFound)
	requireErrorCode(t, recorder, codeNotFound)

	recorder = call(t, server, http.MethodGet, "/api/audits/not-a-number", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// The maintenance endpoint prunes with the audit retention.
	recorder = call(t, server, http.MethodPost, "/api/maintenance/audits/prune", nil)
	requireStatus(t, recorder, http.StatusOK)
	pruned := decodeJSON[struct {
		Keep   int  `json:"keep"`
		Pruned bool `json:"pruned"`
	}](t, recorder)
	if !pruned.Pruned || pruned.Keep != defaultKeepAudits {
		t.Fatalf("prune answer = %+v, want the default retention %d", pruned, defaultKeepAudits)
	}
}

// -----------------------------------------------------------------------------
// API tokens
// -----------------------------------------------------------------------------

// callWithHeader performs one request against the router with one extra header,
// which is how the token endpoints are exercised (Authorization: Bearer …).
func callWithHeader(t *testing.T, server *Server, method, path, header, value string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	if reader != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set(header, value)
	recorder := httptest.NewRecorder()
	server.Router().ServeHTTP(recorder, request)
	return recorder
}

// TestAPITokenEndpoints follows the administration of a token end to end: the
// secret is answered once, the token authenticates a request, a read-only token
// is refused on a write, and revoke/delete take it away.
func TestAPITokenEndpoints(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	// An unnamed token is refused before anything is written.
	recorder := call(t, server, http.MethodPost, "/api/tokens", map[string]any{"name": "  "})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// Creating a read-only token answers the secret, exactly once.
	recorder = call(t, server, http.MethodPost, "/api/tokens", map[string]any{
		"name":            "cost exporter",
		"scopes":          []string{"read"},
		"expires_in_days": 30,
	})
	requireStatus(t, recorder, http.StatusCreated)
	created := decodeJSON[struct {
		Token  apiTokenView `json:"token"`
		Secret string       `json:"secret"`
	}](t, recorder)
	if created.Token.ID == 0 || created.Token.Name != "cost exporter" || created.Token.State != "active" {
		t.Fatalf("created token = %+v", created.Token)
	}
	if created.Token.ExpiresAt == nil || !created.Token.ExpiresAt.After(fixedNow) {
		t.Fatalf("expiry = %v, want the requested 30 days", created.Token.ExpiresAt)
	}
	if len(created.Token.Scopes) != 1 || created.Token.Scopes[0] != models.ScopeRead {
		t.Fatalf("scopes = %v, want read only", created.Token.Scopes)
	}
	rest, ok := strings.CutPrefix(created.Secret, "sync_")
	if !ok {
		t.Fatalf("secret = %q, want the sync_ prefix", created.Secret)
	}
	prefix, _, ok := strings.Cut(rest, ".")
	if !ok || prefix != created.Token.Prefix {
		t.Fatalf("secret = %q, want sync_%s.<secret>", created.Secret, created.Token.Prefix)
	}

	// The list never carries a secret, only the public prefix.
	recorder = call(t, server, http.MethodGet, "/api/tokens", nil)
	requireStatus(t, recorder, http.StatusOK)
	listed := decodeJSON[struct {
		Tokens []apiTokenView `json:"tokens"`
		Scopes []string       `json:"scopes"`
	}](t, recorder)
	if len(listed.Tokens) != 1 || len(listed.Scopes) != 2 {
		t.Fatalf("tokens = %+v, scopes = %v", listed.Tokens, listed.Scopes)
	}
	if body := recorder.Body.String(); strings.Contains(body, created.Secret) || strings.Contains(body, "\"hash\"") {
		t.Fatalf("the list leaks the secret: %s", body)
	}

	// The token authenticates a read.
	recorder = callWithHeader(t, server, http.MethodGet, "/api/stats", "Authorization", "Bearer "+created.Secret, nil)
	requireStatus(t, recorder, http.StatusOK)

	// …and the session endpoint reports it as the operator of the request.
	recorder = callWithHeader(t, server, http.MethodGet, "/api/auth/session", "Authorization", "Bearer "+created.Secret, nil)
	requireStatus(t, recorder, http.StatusOK)
	operator := decodeJSON[struct {
		Operator services.Identity `json:"operator"`
	}](t, recorder)
	if operator.Operator.Token != "cost exporter" || operator.Operator.Method != models.AuthModeToken {
		t.Fatalf("operator = %+v, want the API-token identity", operator.Operator)
	}

	// The `X-API-Key` header is accepted too.
	recorder = callWithHeader(t, server, http.MethodGet, "/api/stats", "X-API-Key", created.Secret, nil)
	requireStatus(t, recorder, http.StatusOK)

	// A read-only token cannot write, and a wrong secret authenticates nothing.
	recorder = callWithHeader(t, server, http.MethodPost, "/api/maintenance/oauth/states/prune", "Authorization", "Bearer "+created.Secret, nil)
	requireStatus(t, recorder, http.StatusForbidden)
	requireErrorCode(t, recorder, codeForbidden)

	recorder = callWithHeader(t, server, http.MethodGet, "/api/stats", "Authorization", "Bearer sync_"+prefix+".wrong", nil)
	requireStatus(t, recorder, http.StatusUnauthorized)
}

// TestAPITokenRegenerateRevokeAndDelete covers the rest of the token lifecycle:
// the old secret dies with the regeneration, revoke is idempotent, and delete
// removes the row (a stale identifier is then a 400 from the path parser only
// when it is not a number).
func TestAPITokenRegenerateRevokeAndDelete(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	recorder := call(t, server, http.MethodPost, "/api/tokens", map[string]any{"name": "writer"})
	requireStatus(t, recorder, http.StatusCreated)
	created := decodeJSON[struct {
		Token  apiTokenView `json:"token"`
		Secret string       `json:"secret"`
	}](t, recorder)
	if len(created.Token.Scopes) != 2 {
		t.Fatalf("scopes = %v, want read and write by default", created.Token.Scopes)
	}

	// A write token may write.
	recorder = callWithHeader(t, server, http.MethodPost, "/api/maintenance/oauth/states/prune", "Authorization", "Bearer "+created.Secret, nil)
	requireStatus(t, recorder, http.StatusOK)

	// Regenerating replaces the credential in place.
	id := strconv.FormatUint(uint64(created.Token.ID), 10)
	recorder = call(t, server, http.MethodPost, "/api/tokens/"+id+"/regenerate", nil)
	requireStatus(t, recorder, http.StatusOK)
	regenerated := decodeJSON[struct {
		Token  apiTokenView `json:"token"`
		Secret string       `json:"secret"`
	}](t, recorder)
	if regenerated.Secret == created.Secret || regenerated.Token.Prefix == created.Token.Prefix || regenerated.Token.ID != created.Token.ID {
		t.Fatalf("regenerated = %+v, want the same row with a new credential", regenerated.Token)
	}
	recorder = callWithHeader(t, server, http.MethodGet, "/api/stats", "Authorization", "Bearer "+created.Secret, nil)
	requireStatus(t, recorder, http.StatusUnauthorized)
	recorder = callWithHeader(t, server, http.MethodGet, "/api/stats", "Authorization", "Bearer "+regenerated.Secret, nil)
	requireStatus(t, recorder, http.StatusOK)

	// Revoking is idempotent, keeps the row and the credential stops working.
	recorder = call(t, server, http.MethodPost, "/api/tokens/"+id+"/revoke", nil)
	requireStatus(t, recorder, http.StatusOK)
	revoked := decodeJSON[struct {
		Token apiTokenView `json:"token"`
	}](t, recorder)
	if revoked.Token.State != "revoked" || revoked.Token.RevokedAt == nil {
		t.Fatalf("revoked token = %+v", revoked.Token)
	}
	recorder = call(t, server, http.MethodPost, "/api/tokens/"+id+"/revoke", nil)
	requireStatus(t, recorder, http.StatusOK)
	recorder = callWithHeader(t, server, http.MethodGet, "/api/stats", "Authorization", "Bearer "+regenerated.Secret, nil)
	requireStatus(t, recorder, http.StatusUnauthorized)
	// A revoked token cannot be regenerated.
	recorder = call(t, server, http.MethodPost, "/api/tokens/"+id+"/regenerate", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// Deleting removes the row for good.
	recorder = call(t, server, http.MethodDelete, "/api/tokens/"+id, nil)
	requireStatus(t, recorder, http.StatusNoContent)
	if _, err := server.deps.Store.GetAPIToken(context.Background(), created.Token.ID); !errors.Is(err, services.ErrNotFound) {
		t.Fatalf("the token is still stored: %v", err)
	}

	// A malformed identifier is a 400; an unknown but well formed one is a 404.
	recorder = call(t, server, http.MethodDelete, "/api/tokens/not-a-number", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)
	recorder = call(t, server, http.MethodPost, "/api/tokens/4242/revoke", nil)
	requireStatus(t, recorder, http.StatusNotFound)
	requireErrorCode(t, recorder, codeNotFound)
}

// -----------------------------------------------------------------------------
// Billing
// -----------------------------------------------------------------------------

// seedBilling stores one job, one run and the two traffic rows of that run, which
// is everything the billing endpoints read.
func seedBilling(t *testing.T, store *services.Store) (*models.SyncJob, *models.SyncRun) {
	t.Helper()
	ctx := context.Background()
	job := &models.SyncJob{Name: "nightly", Enabled: true, Direction: string(models.DirectionGoogleToMicrosoft)}
	if err := store.SaveJob(ctx, job); err != nil {
		t.Fatalf("saving the job: %v", err)
	}
	started := time.Date(2026, 9, 27, 1, 0, 0, 0, time.UTC)
	run := &models.SyncRun{
		JobID: job.ID, JobName: job.Name, Status: string(models.RunSuccess),
		Trigger: string(models.TriggerScheduled), StartedAt: started,
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("creating the run: %v", err)
	}
	rows := []models.TrafficEntry{
		{
			RunID: run.ID, JobID: job.ID, AccountID: 1, AccountEmail: "source@example.test", Provider: "google",
			Direction: string(models.TrafficInbound), Bytes: 2048, Files: 2,
			Day: "2026-09-27", Month: "2026-09", StartedAt: started,
		},
		{
			RunID: run.ID, JobID: job.ID, AccountID: 2, AccountEmail: "target@example.test", Provider: "microsoft",
			Direction: string(models.TrafficOutbound), Bytes: 2048, Files: 2, Operations: 3,
			Day: "2026-09-27", Month: "2026-09", StartedAt: started,
		},
	}
	if err := store.SaveTraffic(ctx, rows); err != nil {
		t.Fatalf("saving the traffic: %v", err)
	}
	return job, run
}

// TestBillingEndpoints checks the JSON report, the per-run breakdown, the run
// detail and the CSV export, including the schema that labels them.
func TestBillingEndpoints(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	_, run := seedBilling(t, store)

	// The default report is grouped by day and covers the window it was given.
	recorder := call(t, server, http.MethodGet, "/api/billing?from=2026-09-01&to=2026-09-30", nil)
	requireStatus(t, recorder, http.StatusOK)
	report := decodeJSON[struct {
		GroupBy string                 `json:"group_by"`
		Schema  services.BillingSchema `json:"schema"`
		Rows    []services.BillingRow  `json:"rows"`
		Total   services.TrafficTotals `json:"total"`
	}](t, recorder)
	if report.GroupBy != services.BillingByDay || len(report.Rows) != 1 {
		t.Fatalf("report = %+v, want one day", report)
	}
	if report.Schema.ID != services.BillingSchemaNone || report.Schema.Unit != "B" {
		t.Fatalf("schema = %+v, want the bare-bytes default", report.Schema)
	}
	if row := report.Rows[0]; row.Day != "2026-09-27" || row.Inbound.Bytes != 2048 || row.Outbound.Operations != 3 {
		t.Fatalf("row = %+v, want the day bucket of the run", row)
	}
	if report.Total.Bytes != 4096 || report.Total.Files != 4 || report.Total.Operations != 3 {
		t.Fatalf("total = %+v, want both directions summed", report.Total)
	}

	// Every grouping answers (the account one splits the two accounts).
	for _, query := range []string{"group_by=month", "group_by=run"} {
		recorder = call(t, server, http.MethodGet, "/api/billing?"+query+"&from=2026-09-01&to=2026-09-30", nil)
		requireStatus(t, recorder, http.StatusOK)
		if got := decodeJSON[struct {
			Rows []services.BillingRow `json:"rows"`
		}](t, recorder); len(got.Rows) != 1 {
			t.Fatalf("%s returned %d rows, want 1", query, len(got.Rows))
		}
	}
	recorder = call(t, server, http.MethodGet, "/api/billing?group_by=account&from=2026-09-01&to=2026-09-30", nil)
	requireStatus(t, recorder, http.StatusOK)
	if got := decodeJSON[struct {
		Rows []services.BillingRow `json:"rows"`
	}](t, recorder); len(got.Rows) != 2 {
		t.Fatalf("group_by=account returned %d rows, want one per account", len(got.Rows))
	}

	// The per-run breakdown carries the individual rows of the window.
	recorder = call(t, server, http.MethodGet, "/api/billing/runs?from=2026-09-01&to=2026-09-30", nil)
	requireStatus(t, recorder, http.StatusOK)
	breakdown := decodeJSON[struct {
		Traffic []models.TrafficEntry `json:"traffic"`
	}](t, recorder)
	if len(breakdown.Traffic) != 2 {
		t.Fatalf("traffic = %+v, want the two rows of the run", breakdown.Traffic)
	}

	// The run detail shows the same traffic beside the per-file items.
	id := strconv.FormatUint(uint64(run.ID), 10)
	recorder = call(t, server, http.MethodGet, "/api/runs/"+id, nil)
	requireStatus(t, recorder, http.StatusOK)
	detail := decodeJSON[struct {
		Traffic []models.TrafficEntry `json:"traffic"`
	}](t, recorder)
	if len(detail.Traffic) != 2 {
		t.Fatalf("run traffic = %+v, want the two rows", detail.Traffic)
	}
}

// TestBillingExportAndValidation checks the CSV contract, the input validation and
// that the report follows the billing schema of Admin > Settings.
func TestBillingExportAndValidation(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	seedBilling(t, store)

	// The export is a CSV with a stable English header and the byte quantities.
	recorder := call(t, server, http.MethodGet, "/api/billing/export?from=2026-09-01&to=2026-09-30", nil)
	requireStatus(t, recorder, http.StatusOK)
	if ct := recorder.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type = %q, want text/csv", ct)
	}
	if cd := recorder.Header().Get("Content-Disposition"); !strings.Contains(cd, "billing-day-2026-09-01-to-2026-09-30.csv") {
		t.Fatalf("content-disposition = %q, want the dated file name", cd)
	}
	body := recorder.Body.String()
	if !strings.HasPrefix(body, "\ufeff") {
		t.Fatal("the billing CSV must start with a UTF-8 BOM")
	}
	if !strings.Contains(body, "bucket,day,month,account_id,account_email,run_id,job_id,inbound,outbound,operations,total,cost,cost_in,cost_out,unit,currency") {
		t.Fatalf("the CSV header is missing: %q", body)
	}
	if !strings.Contains(body, "2026-09-27,2026-09-27,,,,,,2048,2048,3,4096,,,,B,USD") {
		t.Fatalf("the CSV row is missing or mislabelled: %q", body)
	}

	// A malformed date and an unknown grouping are validation errors.
	recorder = call(t, server, http.MethodGet, "/api/billing?from=27-09-2026", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)
	recorder = call(t, server, http.MethodGet, "/api/billing?group_by=weekly", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)
	recorder = call(t, server, http.MethodGet, "/api/billing?account_id=abc", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// The schema of the report follows Admin > Settings, and the settings answer
	// carries the presets for the form.
	recorder = call(t, server, http.MethodGet, "/api/settings", nil)
	requireStatus(t, recorder, http.StatusOK)
	settings := decodeJSON[struct {
		Schemas []services.BillingSchema `json:"billing_schemas"`
		Default string                   `json:"billing_schema_default"`
	}](t, recorder)
	if len(settings.Schemas) != 5 || settings.Default == "" {
		t.Fatalf("schemas = %+v, want the five presets and the custom document", settings.Schemas)
	}

	recorder = call(t, server, http.MethodPut, "/api/settings", map[string]any{
		"default_locale":                "en-US",
		"default_theme":                 "system",
		"sync_default_interval_minutes": 60,
		"sync_run_timeout_minutes":      30,
		"billing_schema":                "oci",
		"billing_schema_custom":         "",
		"billing_rate_in_per_gib":       1000,
		"billing_rate_out_per_gib":      2000,
		"billing_currency":              "usd",
		"purge_enabled":                 true,
		"purge_jobs_days":               180,
		"purge_audits_days":             180,
		"purge_billing_days":            180,
	})
	requireStatus(t, recorder, http.StatusOK)

	recorder = call(t, server, http.MethodGet, "/api/billing?from=2026-09-01&to=2026-09-30", nil)
	requireStatus(t, recorder, http.StatusOK)
	priced := decodeJSON[struct {
		Schema services.BillingSchema `json:"schema"`
	}](t, recorder)
	if priced.Schema.ID != services.BillingSchemaOCI || priced.Schema.Unit != "GiB" || priced.Schema.Currency != "USD" {
		t.Fatalf("schema = %+v, want the priced OCI schema", priced.Schema)
	}
	if priced.Schema.RateInPerGiB != 1000 || priced.Schema.RateOutPerGiB != 2000 || !priced.Schema.Priced() {
		t.Fatalf("schema = %+v, want the two configured rates", priced.Schema)
	}
	recorder = call(t, server, http.MethodGet, "/api/billing/export?from=2026-09-01&to=2026-09-30", nil)
	requireStatus(t, recorder, http.StatusOK)
	// Each direction is priced with its own rate (the window holds 2048 bytes
	// inbound and 2048 outbound) and the quantities switch to GiB; `cost` is the
	// sum of the two, which is what the column meant before the split.
	perGiB := float64(2048) / (1024 * 1024 * 1024)
	inCost := perGiB * 1000
	outCost := perGiB * 2000
	wantTail := "," + strconv.FormatFloat(inCost+outCost, 'f', 6, 64) +
		"," + strconv.FormatFloat(inCost, 'f', 6, 64) +
		"," + strconv.FormatFloat(outCost, 'f', 6, 64) + ",GiB,USD"
	if body = recorder.Body.String(); !strings.Contains(body, wantTail) {
		t.Fatalf("the priced CSV is missing the three cost columns (%q): %q", wantTail, body)
	}

	// An unknown schema is refused by the settings validation.
	recorder = call(t, server, http.MethodPut, "/api/settings", map[string]any{
		"default_locale":                "en-US",
		"default_theme":                 "system",
		"sync_default_interval_minutes": 60,
		"sync_run_timeout_minutes":      30,
		"billing_schema":                "weekly",
		"billing_schema_custom":         "",
		"billing_rate_in_per_gib":       0,
		"billing_rate_out_per_gib":      0,
		"billing_currency":              "USD",
		"purge_enabled":                 true,
		"purge_jobs_days":               180,
		"purge_audits_days":             180,
		"purge_billing_days":            180,
	})
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)
}
