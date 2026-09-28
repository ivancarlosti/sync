package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/models"
)

// runView is a run plus the flag the history screen uses to offer the cancel
// button without a second request.
type runView struct {
	models.SyncRun
	Running bool `json:"running"`
}

// runItemView is one file operation of a run.
type runItemView struct {
	ID        uint      `json:"id"`
	Action    string    `json:"action"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	Evidence  string    `json:"evidence,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// newRunView decorates a run.
func newRunView(run models.SyncRun) runView {
	return runView{SyncRun: run, Running: run.Status == string(models.RunRunning)}
}

// newRunItemViews converts the per-file rows of a run.
func newRunItemViews(items []models.SyncItem) []runItemView {
	views := make([]runItemView, 0, len(items))
	for _, item := range items {
		views = append(views, runItemView{
			ID:        item.ID,
			Action:    item.Action,
			Path:      item.Path,
			Size:      item.Size,
			Evidence:  item.Evidence,
			CreatedAt: item.CreatedAt,
		})
	}
	return views
}

// handleListRuns answers GET /api/runs?job_id=&limit=&status=. `job_id` is
// optional: the history screen shows every run, the job detail screen passes its
// own id. `status` filters the returned page (the database query is limited
// first, which is what keeps one page cheap).
func (s *Server) handleListRuns(c *gin.Context) {
	jobID := uint(0)
	if raw := strings.TrimSpace(c.Query("job_id")); raw != "" {
		parsed, ok := queryID(c, "job_id")
		if !ok {
			return
		}
		jobID = parsed
	}
	status := strings.TrimSpace(c.Query("status"))
	runs, err := s.deps.Store.ListRuns(c.Request.Context(), jobID, limit(c, 50, 200))
	if err != nil {
		fail(c, err)
		return
	}
	views := make([]runView, 0, len(runs))
	for _, run := range runs {
		if status != "" && run.Status != status {
			continue
		}
		views = append(views, newRunView(run))
	}
	c.JSON(http.StatusOK, gin.H{"runs": views})
}

// handleGetRun answers GET /api/runs/:id with the run and its file operations.
func (s *Server) handleGetRun(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	run, err := s.deps.Store.GetRun(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	items, err := s.deps.Store.ListRunItems(c.Request.Context(), id, limit(c, 500, 1000))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"run": newRunView(*run), "items": newRunItemViews(items)})
}

// handleListRunItems answers GET /api/runs/:id/items, the paginated detail the
// history screen loads when a run is expanded.
func (s *Server) handleListRunItems(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, err := s.deps.Store.GetRun(c.Request.Context(), id); err != nil {
		fail(c, err)
		return
	}
	size := limit(c, 100, 1000)
	items, err := s.deps.Store.ListRunItems(c.Request.Context(), id, size)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": newRunItemViews(items), "limit": size})
}

// handleStats answers GET /api/stats?days=30: the aggregate numbers of the
// dashboard. `days=0` means "since the beginning", which is the figure the
// Admin > About page shows.
func (s *Server) handleStats(c *gin.Context) {
	days := queryInt(c, "days", 30, 0, 3650)
	since := time.Time{}
	if days > 0 {
		since = s.now().Add(-time.Duration(days) * 24 * time.Hour)
	}
	stats, err := s.deps.Store.Stats(c.Request.Context(), since)
	if err != nil {
		fail(c, err)
		return
	}
	running := s.deps.Sync.RunningJobs()
	c.JSON(http.StatusOK, gin.H{
		"days":         days,
		"since":        since,
		"stats":        stats,
		"running_jobs": len(running),
		"running_ids":  running,
	})
}
