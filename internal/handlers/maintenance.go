package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/services"
)

// pruneInput is the payload of POST /api/maintenance/runs/prune.
type pruneInput struct {
	// Keep is how many runs per job survive the prune.
	Keep int `json:"keep"`
}

// handleMaintenanceSchedule answers POST /api/maintenance/schedule/run: it forces
// one scheduling pass ("start every due job now") instead of waiting for the
// next tick of the scheduler. It is what the dashboard button calls after a long
// outage, and it is the deterministic entry point the tests drive.
func (s *Server) handleMaintenanceSchedule(c *gin.Context) {
	started := s.deps.Scheduler.RunDue(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"started": started})
}

// handleMaintenanceTokens answers POST /api/maintenance/tokens/refresh: it renews
// every access token that expires inside the refresh window.
func (s *Server) handleMaintenanceTokens(c *gin.Context) {
	refreshed := s.deps.Scheduler.RefreshTokens(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"refreshed": refreshed})
}

// handleMaintenanceOAuthStates answers POST /api/maintenance/oauth/states/prune:
// it drops the authorization states that were never redeemed, which is the only
// table that grows without bound when a browser abandons a connect flow.
func (s *Server) handleMaintenanceOAuthStates(c *gin.Context) {
	removed, err := s.deps.OAuth.PruneStates(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"removed": removed})
}

// handleMaintenanceRuns answers POST /api/maintenance/runs/prune: it keeps the
// newest runs of every job and deletes the older ones with their file rows,
// bounding the size of the audit tables.
func (s *Server) handleMaintenanceRuns(c *gin.Context) {
	input := pruneInput{Keep: defaultKeepRuns}
	if c.Request.ContentLength > 0 {
		if !decode(c, &input) {
			return
		}
	}
	if input.Keep < 1 || input.Keep > maxKeepRuns {
		abort(c, http.StatusBadRequest, "keep must be between 1 and 10000")
		return
	}
	if err := s.deps.Store.PruneRuns(c.Request.Context(), input.Keep); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"keep": input.Keep, "pruned": true})
}

// handleMaintenanceAudits answers POST /api/maintenance/audits/prune: it keeps
// the newest audits of every account and deletes the older ones with their
// report rows, bounding the size of the audit tables.
func (s *Server) handleMaintenanceAudits(c *gin.Context) {
	input := pruneInput{Keep: defaultKeepAudits}
	if c.Request.ContentLength > 0 {
		if !decode(c, &input) {
			return
		}
	}
	if input.Keep < 1 || input.Keep > maxKeepRuns {
		abort(c, http.StatusBadRequest, "keep must be between 1 and 10000")
		return
	}
	if err := s.deps.Store.PruneAudits(c.Request.Context(), input.Keep); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"keep": input.Keep, "pruned": true})
}

// purgeInput is the payload of POST /api/maintenance/purge.
type purgeInput struct {
	// Target is one of `jobs`, `audits`, `billing` or `all` (the default).
	Target string `json:"target"`
	// Days is the age to erase past; 0 applies the configured retention of the
	// target, which is what the "purge now" buttons send when they hold no
	// explicit age.
	Days int `json:"days"`
}

// handleMaintenancePurge answers POST /api/maintenance/purge: it applies the
// retention of Admin > Settings > Purge to one target (or to all three of them)
// right away, instead of waiting for the retention loop of the scheduler.
//
// It erases finished records only: a run or an audit that is still going is
// never touched, whatever the age says.
func (s *Server) handleMaintenancePurge(c *gin.Context) {
	input := purgeInput{Target: services.PurgeTargetAll}
	if c.Request.ContentLength > 0 {
		if !decode(c, &input) {
			return
		}
	}
	outcome, err := s.deps.Purge.Purge(c.Request.Context(), input.Target, input.Days)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, outcome)
}

// keepRuns bounds mirror the retention the scheduler applies on its own.
const (
	defaultKeepRuns   = 200
	defaultKeepAudits = 50
	maxKeepRuns       = 10000
)
