package handlers

import "github.com/gin-gonic/gin"

// registerRoutes wires every endpoint of the API.
//
// The layout is: public endpoints first (health, version, session, login and the
// two OAuth callback URLs), then the whole `/secured` tree behind requireSession.
// The callbacks must stay public: the browser arrives on a top-level navigation
// whose session cookie may have expired, and the protection comes from the
// signed state the flow stored server side, not from the cookie. The SPA is
// mounted as the NoRoute handler, which is what turns a deep link into a page.
func (s *Server) registerRoutes(router *gin.Engine) {
	api := router.Group("/api")
	api.Use(s.session())

	api.GET("/health", s.handleHealth)
	api.GET("/version", s.handleVersion)
	api.GET("/stats", s.requireSession(), s.handleStats)

	auth := api.Group("/auth")
	auth.GET("/session", s.handleSession)
	auth.POST("/login", s.handleLogin)
	auth.POST("/logout", s.handleLogout)
	auth.GET("/keycloak", s.handleKeycloakStart)
	auth.GET("/callback", s.handleKeycloakCallback)

	// The start request is authenticated, the two callbacks are not.
	oauth := api.Group("/oauth")
	oauth.GET("", s.requireSession(), s.handleOAuthProviders)
	oauth.POST("/:provider/start", s.requireSession(), s.handleOAuthStart)
	oauth.GET("/:provider/callback", s.handleOAuthCallback)

	secured := api.Group("")
	secured.Use(s.requireSession())
	{
		accounts := secured.Group("/accounts")
		accounts.GET("", s.handleListAccounts)
		accounts.DELETE("/:id", s.handleDeleteAccount)
		accounts.POST("/:id/verify", s.handleVerifyAccount)
		accounts.GET("/:id/drives", s.handleAccountDrives)
		accounts.GET("/:id/drives/:drive/items", s.handleAccountItems)
		accounts.GET("/:id/sites", s.handleAccountSites)

		jobs := secured.Group("/jobs")
		jobs.GET("", s.handleListJobs)
		jobs.POST("", s.handleCreateJob)
		jobs.GET("/:id", s.handleGetJob)
		jobs.PUT("/:id", s.handleUpdateJob)
		jobs.DELETE("/:id", s.handleDeleteJob)
		jobs.POST("/:id/run", s.handleRunJob)
		jobs.POST("/:id/cancel", s.handleCancelJob)
		jobs.GET("/:id/schedule", s.handleJobSchedule)

		runs := secured.Group("/runs")
		runs.GET("", s.handleListRuns)
		runs.GET("/:id", s.handleGetRun)
		runs.GET("/:id/items", s.handleListRunItems)

		settings := secured.Group("/settings")
		settings.GET("", s.handleGetSettings)
		settings.PUT("", s.handleUpdateSettings)
		settings.GET("/raw", s.handleRawSettings)

		providers := secured.Group("/providers")
		providers.GET("", s.handleListProviders)
		providers.GET("/:provider", s.handleGetProvider)
		providers.PUT("/:provider", s.handleUpdateProvider)
		providers.DELETE("/:provider", s.handleClearProvider)

		notifications := secured.Group("/notifications")
		notifications.GET("", s.handleListNotifications)
		notifications.POST("", s.handleCreateNotification)
		notifications.GET("/:id", s.handleGetNotification)
		notifications.PUT("/:id", s.handleUpdateNotification)
		notifications.DELETE("/:id", s.handleDeleteNotification)
		notifications.PUT("/:id/enabled", s.handleNotificationEnabled)
		notifications.POST("/:id/test", s.handleTestNotification)

		maintenance := secured.Group("/maintenance")
		maintenance.POST("/schedule/run", s.handleMaintenanceSchedule)
		maintenance.POST("/tokens/refresh", s.handleMaintenanceTokens)
		maintenance.POST("/oauth/states/prune", s.handleMaintenanceOAuthStates)
		maintenance.POST("/runs/prune", s.handleMaintenanceRuns)
	}

	router.NoRoute(s.spa())
}
