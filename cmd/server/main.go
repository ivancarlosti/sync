// Command server is the Sync binary.
//
// It is the only place where the pieces are wired together: configuration,
// database (connect + migrate + seed), services (tokens, OAuth, settings,
// notifications, sync engine, scheduler), the HTTP API and the embedded SPA.
// Everything else in this repository receives what it needs through a
// constructor, which is what keeps the packages testable and the boot sequence
// (and its failure modes) readable in one file.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/database"
	"github.com/ivancarlosti/sync/internal/handlers"
	"github.com/ivancarlosti/sync/internal/notify"
	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/providers/google"
	"github.com/ivancarlosti/sync/internal/providers/microsoft"
	"github.com/ivancarlosti/sync/internal/services"
	"github.com/ivancarlosti/sync/internal/version"
	"github.com/ivancarlosti/sync/web"
)

const (
	// notifyTimeout bounds a single notification delivery, so a dead SMTP
	// server can never hold a sync run open.
	notifyTimeout = 30 * time.Second
	// readHeaderTimeout protects against a client that opens a connection and
	// never finishes the request line.
	readHeaderTimeout = 15 * time.Second
	// idleTimeout is generous because the SPA keeps a connection alive while an
	// operator watches a run.
	idleTimeout = 60 * time.Second
	// shutdownTimeout is how long the in-flight requests get to finish once a
	// SIGTERM (docker stop) arrives.
	shutdownTimeout = 20 * time.Second
)

func main() {
	if err := run(); err != nil {
		// The logger is already installed; this keeps the exit path in one
		// place and returns a non-zero status for the container restart policy.
		slog.Error("sync stopped with an error", "error", err)
		os.Exit(1)
	}
}

// run is main with error handling: every failure is reported to the caller.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slog.SetDefault(newLogger(cfg.LogLevel))

	// The context is cancelled by SIGINT/SIGTERM, which stops the scheduler and
	// the sync runs in flight before the process exits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Open(cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := database.Close(db); err != nil {
			slog.Warn("closing the database failed", "error", err)
		}
	}()

	if err := database.Migrate(db); err != nil {
		return err
	}
	settings := database.NewSettings(db)
	if err := database.Seed(ctx, settings, services.Defaults(cfg)); err != nil {
		return err
	}
	// Flows that were never redeemed (abandoned logins, a closed browser tab)
	// would otherwise sit in the table until the next maintenance pass.
	if err := database.PruneOAuthStates(ctx, db); err != nil {
		slog.Warn("pruning the abandoned oauth states failed", "error", err)
	}

	deps := build(cfg, settings)
	if err := deps.Scheduler.Bootstrap(ctx); err != nil {
		return fmt.Errorf("cmd/server: preparing the schedules: %w", err)
	}

	server, err := handlers.New(deps)
	if err != nil {
		return err
	}
	if err := deps.Scheduler.Start(ctx); err != nil {
		return fmt.Errorf("cmd/server: starting the scheduler: %w", err)
	}
	defer deps.Scheduler.Stop()

	httpServer := &http.Server{
		Addr:              cfg.Address(),
		Handler:           server.Router(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}

	failed := make(chan error, 1)
	go func() {
		slog.Info("sync is listening",
			"address", cfg.Address(),
			"version", version.Version,
			"commit", version.Commit,
			"auth_method", cfg.AuthMethod,
			"google", cfg.GoogleConfigured(),
			"microsoft", cfg.MicrosoftConfigured(),
			"keycloak", cfg.KeycloakConfigured())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	select {
	case err := <-failed:
		return fmt.Errorf("cmd/server: serving http: %w", err)
	case <-ctx.Done():
		slog.Info("shutdown requested, draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Warn("the http server did not drain in time", "error", err)
	}
	// Stop picking new work, then let the runs already in flight reach their
	// final state instead of leaving `running` rows behind for the next boot.
	deps.Scheduler.Stop()
	deps.Sync.Wait()
	slog.Info("bye")
	return nil
}

// build wires the services and the HTTP layer. It is a single function so the
// order in which the collaborators depend on each other stays visible; the
// connection pool is reached through the settings repository it was built from.
func build(cfg *config.Config, settings *database.Settings) handlers.Deps {
	store := services.NewStore(settings.DB(), settings)
	// The box is the only holder of the encryption key: everything that touches
	// a stored token or secret goes through it.
	box := services.NewSecretBox(cfg.EncryptionKeyBytes())
	registry := providers.NewRegistry(google.New(), microsoft.New())
	credentials := services.NewProviderSettings(cfg, settings, box)
	tokens := services.NewTokenManager(store, registry, credentials, box)
	notifier := services.NewNotifier(store, notify.NewDispatcher(notifyTimeout), box)
	appSettings := services.NewSettingsService(cfg, settings)
	syncService := services.NewSyncService(store, tokens, appSettings, notifier)

	return handlers.Deps{
		Config:      cfg,
		Store:       store,
		Auth:        services.NewAuthService(cfg, store),
		OAuth:       services.NewOAuthService(store, registry, credentials, tokens),
		Tokens:      tokens,
		Credentials: credentials,
		Registry:    registry,
		Settings:    appSettings,
		Notifier:    notifier,
		Sync:        syncService,
		Scheduler:   services.NewScheduler(store, syncService, tokens, notifier),
		Assets:      web.Dist(),
		Info:        version.Get(),
	}
}

// newLogger builds the process logger from LOG_LEVEL (already validated at
// boot). Plain text on stderr is what a container log collector expects.
func newLogger(level string) *slog.Logger {
	var parsed slog.Level
	switch level {
	case "debug":
		parsed = slog.LevelDebug
	case "warn":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		parsed = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsed}))
}
