// Package handlers exposes the HTTP API of Sync.
//
// The package is deliberately thin: it owns routing, sessions, request
// validation and the translation of service errors into HTTP status codes, but
// never talks to a provider, a database or a token directly. Everything it
// needs is injected through Deps, which is what keeps the API testable with an
// in-memory database (`handlers_test.go`) and the endpoints documented in
// docs/api.md in sync with the code.
package handlers

import (
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/services"
	"github.com/ivancarlosti/sync/internal/version"
)

// Deps is the complete set of collaborators of the API. New() refuses to build a
// server when one of the mandatory fields is missing, so a wiring mistake fails
// at start-up instead of on the first request.
type Deps struct {
	// Config is the validated environment configuration.
	Config *config.Config
	// Store is the persistence layer used by the CRUD endpoints.
	Store *services.Store
	// Auth implements the three authentication modes (`none`, `account`, `keycloak`).
	Auth *services.AuthService
	// OAuth drives the provider authorization-code flows.
	OAuth *services.OAuthService
	// Tokens resolves an account into a usable provider session.
	Tokens *services.TokenManager
	// Credentials reads and writes the provider OAuth clients.
	Credentials *services.ProviderSettings
	// Registry lists the providers compiled into this binary.
	Registry *providers.Registry
	// Settings reads and writes the editable settings.
	Settings *services.SettingsService
	// Notifier owns the notification channels.
	Notifier *services.Notifier
	// Sync starts, cancels and reports sync runs.
	Sync *services.SyncService
	// Scheduler is used to force a scheduling pass or a token refresh.
	Scheduler *services.Scheduler
	// Assets is the embedded SPA (`web.Dist()`).
	Assets fs.FS
	// Info is the build metadata reported by /api/version.
	Info version.Info
	// Now is the clock used for schedules and session cookies. It is injectable
	// so the tests never have to sleep; New() defaults it to time.Now.
	Now func() time.Time
}

// Server holds the dependencies and the Gin engine.
type Server struct {
	deps   Deps
	router *gin.Engine
}

// New builds the API server: it configures Gin, registers every route and
// returns the ready-to-serve HTTP handler.
func New(deps Deps) (*Server, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Config.Dev {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	server := &Server{deps: deps}
	router := gin.New()
	// The recovery middleware turns a panic into a 500 instead of killing the
	// process; the logger stays silent for the health probe, which would
	// otherwise flood the logs of a container running under an orchestrator.
	router.Use(gin.LoggerWithConfig(gin.LoggerConfig{SkipPaths: []string{"/api/health"}}), gin.Recovery())
	router.Use(server.securityHeaders())
	server.trustProxies(router)
	server.registerRoutes(router)
	server.router = router
	return server, nil
}

// validate reports the first missing mandatory dependency.
func (d Deps) validate() error {
	switch {
	case d.Config == nil:
		return errors.New("handlers: Config is required")
	case d.Store == nil:
		return errors.New("handlers: Store is required")
	case d.Auth == nil:
		return errors.New("handlers: Auth is required")
	case d.OAuth == nil:
		return errors.New("handlers: OAuth is required")
	case d.Tokens == nil:
		return errors.New("handlers: Tokens is required")
	case d.Credentials == nil:
		return errors.New("handlers: Credentials is required")
	case d.Registry == nil:
		return errors.New("handlers: Registry is required")
	case d.Settings == nil:
		return errors.New("handlers: Settings is required")
	case d.Notifier == nil:
		return errors.New("handlers: Notifier is required")
	case d.Sync == nil:
		return errors.New("handlers: Sync is required")
	case d.Scheduler == nil:
		return errors.New("handlers: Scheduler is required")
	case d.Assets == nil:
		return errors.New("handlers: Assets is required (use web.Dist())")
	}
	return nil
}

// Router returns the Gin engine, which the tests and cmd/server use to mount the
// API under an arbitrary HTTP server.
func (s *Server) Router() *gin.Engine { return s.router }

// now is the clock of the handlers: schedules and session cookies are computed
// from it, so a test can pin time by injecting Deps.Now.
func (s *Server) now() time.Time {
	if s.deps.Now == nil {
		return time.Now()
	}
	return s.deps.Now()
}

// trustProxies honours APP_TRUST_PROXY: when the instance runs behind a reverse
// proxy the client address must come from X-Forwarded-For (login throttling and
// logs depend on it), otherwise Gin is told to trust nobody.
func (s *Server) trustProxies(router *gin.Engine) {
	if s.deps.Config.TrustProxy {
		if err := router.SetTrustedProxies([]string{"0.0.0.0/0", "::/0"}); err != nil {
			panic(fmt.Sprintf("handlers: trusting every proxy failed: %v", err))
		}
		return
	}
	if err := router.SetTrustedProxies(nil); err != nil {
		panic(fmt.Sprintf("handlers: disabling trusted proxies failed: %v", err))
	}
}
