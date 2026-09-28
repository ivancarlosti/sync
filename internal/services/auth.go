package services

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/crypto"
	"github.com/ivancarlosti/sync/internal/models"
)

// Session constants. The cookie is stateless: the identity travels inside a
// signed payload, so Sync needs no session table and keeps working behind a
// proxy or with several replicas.
const (
	// SessionCookieName is the cookie carrying the signed session payload.
	SessionCookieName = "sync_session"
	// SessionTTL is how long a login stays valid.
	SessionTTL = 7 * 24 * time.Hour
	// loginFlowProvider is the provider column used by Keycloak login states.
	loginFlowProvider = "keycloak"
	// discoveryTTL caches the OIDC discovery document.
	discoveryTTL = time.Hour
	// LoginAttempts is how many failures one remote address may produce.
	LoginAttempts = 10
	// LoginBlockWindow is how long an address stays blocked after that.
	LoginBlockWindow = 15 * time.Minute
)

// Identity is the authenticated operator. Every field is safe to send to the
// SPA: it never holds a token, only the claims of the session.
type Identity struct {
	Authenticated bool            `json:"authenticated"`
	Subject       string          `json:"subject,omitempty"`
	Email         string          `json:"email,omitempty"`
	Name          string          `json:"name,omitempty"`
	Picture       string          `json:"picture,omitempty"`
	Method        models.AuthMode `json:"method"`
	Admin         bool            `json:"admin"`
	ExpiresAt     time.Time       `json:"expires_at,omitempty"`
}

// Session is a signed cookie value plus the identity it carries.
type Session struct {
	Value     string    `json:"-"`
	Identity  Identity  `json:"identity"`
	ExpiresAt time.Time `json:"expires_at"`
}

// sessionClaims is the payload signed into the cookie.
type sessionClaims struct {
	Subject   string `json:"sub,omitempty"`
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
	Picture   string `json:"picture,omitempty"`
	Method    string `json:"method"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// loginAttempts tracks failed logins per remote address.
type loginAttempts struct {
	Count     int
	BlockedAt time.Time
	SeenAt    time.Time
}

// AuthService implements the three authentication modes described in
// docs/authentication.md: `none` (the instance is open), `account` (one
// credential pair from the environment, optionally protected by reCAPTCHA) and
// `keycloak` (OIDC authorization code + PKCE with an allow-list).
type AuthService struct {
	cfg   *config.Config
	store *Store

	http   *http.Client
	now    func() time.Time
	mu     sync.Mutex
	failed map[string]loginAttempts
	oidc   oidcEndpoints
	oidcAt time.Time
}

// NewAuthService builds the service.
func NewAuthService(cfg *config.Config, store *Store) *AuthService {
	return &AuthService{
		cfg:    cfg,
		store:  store,
		http:   &http.Client{Timeout: 10 * time.Second},
		now:    func() time.Time { return time.Now().UTC() },
		failed: map[string]loginAttempts{},
	}
}

// Mode returns the configured authentication mode.
func (a *AuthService) Mode() models.AuthMode { return a.cfg.AuthMethod }

// ModeName is the same value as a string, handy for JSON responses.
func (a *AuthService) ModeName() string { return string(a.cfg.AuthMethod) }

// Open reports whether the instance requires no authentication at all.
func (a *AuthService) Open() bool { return a.cfg.AuthMethod == models.AuthModeNone }

// CaptchaEnabled reports whether the login form must carry a reCAPTCHA token.
func (a *AuthService) CaptchaEnabled() bool {
	return a.cfg.AuthMethod == models.AuthModeAccount &&
		a.cfg.RecaptchaClientID != "" && a.cfg.RecaptchaClientSecret != ""
}

// CaptchaSiteKey is the public reCAPTCHA key the SPA embeds.
func (a *AuthService) CaptchaSiteKey() string {
	if !a.CaptchaEnabled() {
		return ""
	}
	return a.cfg.RecaptchaClientID
}

// KeycloakConfigured reports whether an OIDC login can be started.
func (a *AuthService) KeycloakConfigured() bool {
	return a.cfg.AuthMethod == models.AuthModeKeycloak && a.cfg.KeycloakConfigured()
}

// OpenIdentity is the administrator identity used in `none` mode, where every
// request is trusted.
func (a *AuthService) OpenIdentity() Identity {
	return Identity{
		Authenticated: true,
		Subject:       "anonymous",
		Name:          "Administrator",
		Method:        models.AuthModeNone,
		Admin:         true,
	}
}

// Session builds a signed session for an authenticated operator.
func (a *AuthService) Session(identity Identity) (Session, error) {
	if !identity.Method.Valid() {
		identity.Method = a.cfg.AuthMethod
	}
	identity.Authenticated = true
	identity.Admin = true

	now := a.now()
	expires := now.Add(SessionTTL)
	identity.ExpiresAt = expires
	claims := sessionClaims{
		Subject:   identity.Subject,
		Email:     identity.Email,
		Name:      identity.Name,
		Picture:   identity.Picture,
		Method:    string(identity.Method),
		IssuedAt:  now.Unix(),
		ExpiresAt: expires.Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return Session{}, fmt.Errorf("services: encoding the session: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return Session{
		Value:     encoded + "." + crypto.Sign(a.cfg.SessionKey(), encoded),
		Identity:  identity,
		ExpiresAt: expires,
	}, nil
}

// FromCookie verifies a session cookie and returns the identity it carries. A
// tampered, malformed or expired cookie is reported as unauthenticated, never as
// an error: the caller only has to answer 401.
func (a *AuthService) FromCookie(value string) (Identity, bool) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return Identity{}, false
	}
	encoded, signature, found := strings.Cut(raw, ".")
	if !found || encoded == "" || signature == "" {
		return Identity{}, false
	}
	if !crypto.Verify(a.cfg.SessionKey(), encoded, signature) {
		return Identity{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Identity{}, false
	}
	claims := sessionClaims{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Identity{}, false
	}
	// A session issued for another authentication mode is not accepted: switching
	// AUTH_METHOD must invalidate the logins made under the previous one.
	if claims.Method != string(a.cfg.AuthMethod) {
		return Identity{}, false
	}
	expires := time.Unix(claims.ExpiresAt, 0).UTC()
	if !expires.After(a.now()) {
		return Identity{}, false
	}
	return Identity{
		Authenticated: true,
		Subject:       claims.Subject,
		Email:         claims.Email,
		Name:          claims.Name,
		Picture:       claims.Picture,
		Method:        models.AuthMode(claims.Method),
		Admin:         true,
		ExpiresAt:     expires,
	}, true
}

// Login validates the credentials entered on the login page. It is only usable
// in `account` mode; the caller supplies the reCAPTCHA token (empty when the
// captcha is disabled) and the client address used for throttling.
func (a *AuthService) Login(ctx context.Context, login, password, captchaToken, clientAddress string) (Session, error) {
	if a.cfg.AuthMethod != models.AuthModeAccount {
		return Session{}, fmt.Errorf("%w: this instance does not use the account login", ErrValidation)
	}
	client := strings.TrimSpace(clientAddress)
	if a.throttled(client) {
		return Session{}, fmt.Errorf("%w: too many failed attempts, try again in a few minutes", ErrForbidden)
	}
	if err := a.verifyCaptcha(ctx, captchaToken, client); err != nil {
		a.noteFailure(client)
		return Session{}, err
	}
	loginOK := constantTimeEqual(strings.TrimSpace(login), a.cfg.AccountLogin)
	passwordOK := constantTimeEqual(password, a.cfg.AccountPassword)
	if !loginOK || !passwordOK {
		a.noteFailure(client)
		slog.Warn("login rejected", "remote", client, "login", maskLogin(login))
		return Session{}, fmt.Errorf("%w: invalid credentials", ErrUnauthorized)
	}
	a.reset(client)
	identity := Identity{
		Subject: "account:" + a.cfg.AccountLogin,
		Name:    a.cfg.AccountLogin,
		Method:  models.AuthModeAccount,
	}
	if strings.Contains(a.cfg.AccountLogin, "@") {
		identity.Email = a.cfg.AccountLogin
	}
	session, err := a.Session(identity)
	if err != nil {
		return Session{}, err
	}
	slog.Info("operator logged in", "remote", client, "login", maskLogin(login), "method", a.ModeName())
	return session, nil
}

// captchaResponse is the subset of Google's siteverify answer Sync reads.
type captchaResponse struct {
	Success bool     `json:"success"`
	Score   float64  `json:"score"`
	Action  string   `json:"action"`
	Errors  []string `json:"error-codes"`
}

// verifyCaptcha posts the widget token to Google. It is a no-op when the captcha
// is disabled, so the caller never has to branch on the configuration.
func (a *AuthService) verifyCaptcha(ctx context.Context, token, clientAddress string) error {
	if !a.CaptchaEnabled() {
		return nil
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("%w: the captcha challenge is missing, reload the page", ErrValidation)
	}
	form := url.Values{
		"secret":   {a.cfg.RecaptchaClientSecret},
		"response": {token},
	}
	if clientAddress != "" {
		form.Set("remoteip", clientAddress)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://www.google.com/recaptcha/api/siteverify",
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("services: building the captcha request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := a.http.Do(request)
	if err != nil {
		return fmt.Errorf("%w: the captcha could not be verified, try again", ErrForbidden)
	}
	defer func() { _ = response.Body.Close() }()

	answer := captchaResponse{}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return fmt.Errorf("%w: the captcha answer could not be read", ErrForbidden)
	}
	if !answer.Success {
		slog.Warn("captcha rejected", "remote", clientAddress, "errors", answer.Errors)
		return fmt.Errorf("%w: the captcha was not accepted, try again", ErrForbidden)
	}
	return nil
}

// throttled reports whether an address exhausted its login attempts.
func (a *AuthService) throttled(client string) bool {
	if client == "" {
		return false
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.failed[client]
	if !ok {
		return false
	}
	if entry.BlockedAt.IsZero() {
		return false
	}
	if now.Sub(entry.BlockedAt) > LoginBlockWindow {
		delete(a.failed, client)
		return false
	}
	return true
}

// noteFailure counts a failed attempt and blocks the address once the budget is
// spent. The map is pruned opportunistically so it cannot grow without bound.
func (a *AuthService) noteFailure(client string) {
	if client == "" {
		return
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, entry := range a.failed {
		if now.Sub(entry.SeenAt) > 2*LoginBlockWindow {
			delete(a.failed, key)
		}
	}
	entry := a.failed[client]
	entry.Count++
	entry.SeenAt = now
	if entry.Count >= LoginAttempts {
		entry.BlockedAt = now
		slog.Warn("login blocked after too many failures", "remote", client, "attempts", entry.Count)
	}
	a.failed[client] = entry
}

// reset clears the failure counter after a successful login.
func (a *AuthService) reset(client string) {
	if client == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.failed, client)
}

// constantTimeEqual compares two strings without leaking their content through
// timing (the length is still observable, which is acceptable here).
func constantTimeEqual(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

// maskLogin hides the local part of an e-mail address in log lines.
func maskLogin(value string) string {
	login := strings.TrimSpace(value)
	at := strings.IndexByte(login, '@')
	if at <= 1 {
		return login
	}
	return login[:1] + "***" + login[at:]
}

// ---------------------------------------------------------------------------
// Keycloak (OIDC authorization code + PKCE)
// ---------------------------------------------------------------------------

// oidcEndpoints is the subset of the discovery document Sync needs.
type oidcEndpoints struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
}

// discovery loads (and caches) the realm discovery document.
func (a *AuthService) discovery(ctx context.Context) (oidcEndpoints, error) {
	if !a.KeycloakConfigured() {
		return oidcEndpoints{}, fmt.Errorf("%w: Keycloak is not configured", ErrNotConfigured)
	}
	a.mu.Lock()
	cached, fetchedAt := a.oidc, a.oidcAt
	a.mu.Unlock()
	if cached.AuthorizationEndpoint != "" && a.now().Sub(fetchedAt) < discoveryTTL {
		return cached, nil
	}

	address := a.cfg.Keycloak.Issuer() + "/.well-known/openid-configuration"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return oidcEndpoints{}, fmt.Errorf("services: building the discovery request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := a.http.Do(request)
	if err != nil {
		return oidcEndpoints{}, fmt.Errorf("services: reaching Keycloak at %s failed: %w", address, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return oidcEndpoints{}, fmt.Errorf("services: the Keycloak discovery returned %s", response.Status)
	}
	endpoints := oidcEndpoints{}
	if err := json.NewDecoder(response.Body).Decode(&endpoints); err != nil {
		return oidcEndpoints{}, fmt.Errorf("services: reading the Keycloak discovery document: %w", err)
	}
	if endpoints.AuthorizationEndpoint == "" || endpoints.TokenEndpoint == "" {
		return oidcEndpoints{}, fmt.Errorf("services: the Keycloak realm did not publish its endpoints")
	}

	a.mu.Lock()
	a.oidc, a.oidcAt = endpoints, a.now()
	a.mu.Unlock()
	return endpoints, nil
}

// oauthConfig builds the x/oauth2 client of the realm.
func (a *AuthService) oauthConfig(ctx context.Context) (*oauth2.Config, oidcEndpoints, error) {
	endpoints, err := a.discovery(ctx)
	if err != nil {
		return nil, oidcEndpoints{}, err
	}
	return &oauth2.Config{
		ClientID:     a.cfg.Keycloak.ClientID,
		ClientSecret: a.cfg.Keycloak.ClientSecret,
		RedirectURL:  a.cfg.Keycloak.RedirectURI,
		Scopes:       []string{"openid", "profile", "email"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  endpoints.AuthorizationEndpoint,
			TokenURL: endpoints.TokenEndpoint,
			// Keycloak accepts the credentials in the body; posting them keeps the
			// flow working for confidential clients without Basic auth surprises.
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}, endpoints, nil
}

// KeycloakBegin stores the server side of a login and returns the URL the
// browser must follow, alongside the state the callback carries back.
func (a *AuthService) KeycloakBegin(ctx context.Context, redirectTo string) (Authorization, error) {
	conf, _, err := a.oauthConfig(ctx)
	if err != nil {
		return Authorization{}, err
	}
	state, err := crypto.RandomToken(32)
	if err != nil {
		return Authorization{}, fmt.Errorf("services: generating the login state: %w", err)
	}
	verifier, challenge, err := crypto.PKCEPair()
	if err != nil {
		return Authorization{}, fmt.Errorf("services: generating the PKCE pair: %w", err)
	}
	row := &models.OAuthState{
		State:        state,
		Flow:         string(models.FlowKeycloak),
		Provider:     loginFlowProvider,
		CodeVerifier: verifier,
		RedirectTo:   SafeRedirect(redirectTo),
		ExpiresAt:    a.now().Add(stateTTL),
	}
	if a.store != nil {
		if err := a.store.SaveOAuthState(ctx, row); err != nil {
			return Authorization{}, err
		}
	}
	address := conf.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
	return Authorization{URL: address, State: state}, nil
}

// keycloakClaims is the userinfo answer Sync reads after the code exchange.
type keycloakClaims struct {
	Subject           string `json:"sub"`
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Picture           string `json:"picture"`
}

// userinfo reads the operator profile from the realm's userinfo endpoint. The
// endpoint is used instead of parsing the ID token so no JWKS handling is
// needed: the access token is validated by Keycloak itself.
func (a *AuthService) userinfo(ctx context.Context, address string, token *oauth2.Token) (keycloakClaims, error) {
	claims := keycloakClaims{}
	if address == "" {
		return claims, fmt.Errorf("services: the Keycloak realm published no userinfo endpoint")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return claims, fmt.Errorf("services: building the userinfo request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	response, err := a.http.Do(request)
	if err != nil {
		return claims, fmt.Errorf("services: reading the Keycloak profile failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return claims, fmt.Errorf("%w: Keycloak refused to describe the logged in account (%s)", ErrUnauthorized, response.Status)
	}
	if err := json.NewDecoder(response.Body).Decode(&claims); err != nil {
		return claims, fmt.Errorf("services: reading the Keycloak profile: %w", err)
	}
	return claims, nil
}

// KeycloakComplete exchanges the authorization code, reads the operator profile
// from the realm and — when the allow-list permits it — issues a session.
// A refused account answers ErrForbidden so the SPA can explain the refusal.
func (a *AuthService) KeycloakComplete(ctx context.Context, code, stateValue string) (Session, error) {
	if !a.KeycloakConfigured() {
		return Session{}, fmt.Errorf("%w: Keycloak is not configured", ErrNotConfigured)
	}
	if strings.TrimSpace(code) == "" {
		return Session{}, fmt.Errorf("%w: Keycloak did not return an authorization code", ErrValidation)
	}
	if a.store == nil {
		return Session{}, fmt.Errorf("%w: this build has no state store", ErrValidation)
	}
	row, err := a.store.ConsumeOAuthState(ctx, strings.TrimSpace(stateValue))
	if err != nil {
		if IsNotFound(err) {
			return Session{}, fmt.Errorf("%w: this login link was already used or has expired", ErrUnauthorized)
		}
		return Session{}, err
	}
	if row.Flow != string(models.FlowKeycloak) || row.Provider != loginFlowProvider {
		return Session{}, fmt.Errorf("%w: this login request does not belong to Keycloak", ErrValidation)
	}
	if row.Expired(a.now()) {
		return Session{}, fmt.Errorf("%w: the login request expired, start again", ErrValidation)
	}

	conf, endpoints, err := a.oauthConfig(ctx)
	if err != nil {
		return Session{}, err
	}
	token, err := conf.Exchange(ctx, code, oauth2.SetAuthURLParam("code_verifier", row.CodeVerifier))
	if err != nil {
		return Session{}, fmt.Errorf("%w: exchanging the Keycloak code failed: %v", ErrUnauthorized, err)
	}
	claims, err := a.userinfo(ctx, endpoints.UserinfoEndpoint, token)
	if err != nil {
		return Session{}, err
	}
	email := strings.TrimSpace(claims.Email)
	if email == "" {
		email = strings.TrimSpace(claims.PreferredUsername)
	}
	if !a.Allowed(email) {
		slog.Warn("keycloak login refused by the allow-list", "account", maskLogin(email))
		return Session{}, fmt.Errorf("%w: %s is not allowed to use this instance", ErrForbidden, maskLogin(email))
	}
	if claims.Subject == "" {
		claims.Subject = email
	}
	identity := Identity{
		Subject: claims.Subject,
		Email:   email,
		Name:    strings.TrimSpace(claims.Name),
		Picture: strings.TrimSpace(claims.Picture),
		Method:  models.AuthModeKeycloak,
	}
	session, err := a.Session(identity)
	if err != nil {
		return Session{}, err
	}
	slog.Info("operator logged in", "account", maskLogin(email), "method", a.ModeName())
	return session, nil
}

// Allowed reports whether an account may enter the instance. The allow-list
// holds full addresses, bare domains and the wildcard `*`.
func (a *AuthService) Allowed(email string) bool {
	return allowedAccount(a.cfg.Keycloak.Accounts, email)
}

// allowedAccount matches an e-mail against the allow-list entries:
// `*` accepts everyone, `user@example.com` one account, `example.com` (or
// `@example.com`) every address of that domain.
func allowedAccount(list []string, email string) bool {
	value := strings.ToLower(strings.TrimSpace(email))
	if value == "" {
		return false
	}
	_, domain, _ := strings.Cut(value, "@")
	for _, entry := range list {
		pattern := strings.ToLower(strings.TrimSpace(entry))
		pattern = strings.TrimPrefix(pattern, "@")
		switch {
		case pattern == "":
			continue
		case pattern == "*":
			return true
		case strings.Contains(pattern, "@"):
			if pattern == value {
				return true
			}
		case domain != "" && pattern == domain:
			return true
		}
	}
	return false
}

// AllowedList returns the configured allow-list (read-only copy).
func (a *AuthService) AllowedList() []string {
	return append([]string{}, a.cfg.Keycloak.Accounts...)
}
