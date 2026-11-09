package handlers

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/services"
)

// managementTaskView is a task plus what the screen needs without a second
// request: whether it is still running and the request parameters as an object
// instead of the stored JSON document.
type managementTaskView struct {
	models.ManagementTask
	Running  bool              `json:"running"`
	ParamMap map[string]string `json:"param_map"`
}

// newManagementTaskView decorates a task.
func newManagementTaskView(task models.ManagementTask) managementTaskView {
	params := map[string]string{}
	if task.Params != "" {
		_ = json.Unmarshal([]byte(task.Params), &params)
	}
	return managementTaskView{
		ManagementTask: task,
		Running:        task.Status == string(models.RunRunning),
		ParamMap:       params,
	}
}

// managementTaskDetailView is a task with its journal.
type managementTaskDetailView struct {
	managementTaskView
	Steps []models.ManagementTaskStep `json:"steps"`
}

// managementStartInput is the payload of POST /api/management/tasks.
type managementStartInput struct {
	AccountID uint              `json:"account_id"`
	Kind      string            `json:"kind"`
	Params    map[string]string `json:"params"`
}

// handleListManagementOperations answers GET /api/management/operations?account_id=:
// the operations the account can run, decorated with whether its grant covers them.
func (s *Server) handleListManagementOperations(c *gin.Context) {
	accountID, ok := queryID(c, "account_id")
	if !ok {
		return
	}
	if accountID == 0 {
		abort(c, http.StatusBadRequest, "an account_id is required")
		return
	}
	operations, err := s.deps.Management.Operations(c.Request.Context(), accountID)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"operations": operations})
}

// handleManagementOptions answers GET /api/management/options?account_id=&kind=:
// the users, groups and drives the form of one operation can offer. The kind
// decides which collections are read, so a Drive operation does not pay for the
// directory listing and an archive form does not read the drives.
func (s *Server) handleManagementOptions(c *gin.Context) {
	accountID, ok := queryID(c, "account_id")
	if !ok {
		return
	}
	kind := c.Query("kind")
	needsUsers, needsGroups, needsDrives := managementOptionNeeds(kind)
	users := []providers.DirectoryUser{}
	groups := []providers.DirectoryGroup{}
	drives := []driveView{}

	err := s.deps.Tokens.Use(c.Request.Context(), accountID, func(session services.ProviderSession) error {
		provider := session.Provider
		if needsUsers || needsGroups {
			reader, ok := provider.(providers.DirectoryReader)
			if !ok {
				return nil
			}
			if needsUsers {
				list, err := reader.ListDirectoryUsers(c.Request.Context(), session.Credentials, session.Tokens)
				if err != nil {
					return err
				}
				users = list
			}
			if needsGroups {
				list, err := reader.ListDirectoryGroups(c.Request.Context(), session.Credentials, session.Tokens)
				if err != nil {
					return err
				}
				groups = list
			}
		}
		if needsDrives {
			list, err := provider.Drives(c.Request.Context(), session.Credentials, session.Tokens)
			if err != nil {
				return err
			}
			for _, drive := range list {
				drives = append(drives, driveView{ID: drive.ID, Name: drive.Name, Kind: drive.Kind, Owner: drive.Owner})
			}
		}
		return nil
	})
	if err != nil {
		managementProviderFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"users": users, "groups": groups, "drives": drives})
}

// managementProviderFailure answers a provider error the form has to show. It
// keeps the provider's own explanation (a disabled Gmail API, a missing consent,
// a throttled Graph) instead of the generic 500, because that text is the fix an
// operator has to apply.
func managementProviderFailure(c *gin.Context, err error) {
	c.JSON(http.StatusBadGateway, gin.H{"code": "provider_error", "error": err.Error()})
}

// managementOptionNeeds says which collections the form of an operation reads.
func managementOptionNeeds(kind string) (users, groups, drives bool) {
	switch kind {
	case models.ManagementKindArchiveUserGroup:
		return true, true, false
	case models.ManagementKindMicrosoftCopyMailbox, models.ManagementKindMicrosoftCalendars:
		return true, false, false
	case models.ManagementKindDriveToNewSharedDrive, models.ManagementKindDriveToSharedDrive,
		models.ManagementKindFolderSizes, models.ManagementKindOneDriveToSite:
		return false, false, true
	default:
		return false, false, false
	}
}

// handleStartManagementTask answers POST /api/management/tasks. The task runs in
// the background; the answer carries the created row so the SPA can follow it
// (409 while another task of the same account is in flight).
func (s *Server) handleStartManagementTask(c *gin.Context) {
	var input managementStartInput
	if !decode(c, &input) {
		return
	}
	task, err := s.deps.Management.Start(c.Request.Context(), services.ManagementInput{
		AccountID: input.AccountID,
		Kind:      input.Kind,
		Params:    input.Params,
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"task": newManagementTaskView(*task)})
}

// handleListManagementTasks answers GET /api/management/tasks, the history log.
func (s *Server) handleListManagementTasks(c *gin.Context) {
	accountID, ok := queryID(c, "account_id")
	if !ok {
		return
	}
	tasks, err := s.deps.Store.ListManagementTasks(c.Request.Context(), accountID, limit(c, 50, 200))
	if err != nil {
		fail(c, err)
		return
	}
	views := make([]managementTaskView, 0, len(tasks))
	for _, task := range tasks {
		views = append(views, newManagementTaskView(task))
	}
	c.JSON(http.StatusOK, gin.H{"tasks": views})
}

// handleGetManagementTask answers GET /api/management/tasks/:id with its journal.
func (s *Server) handleGetManagementTask(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	task, err := s.deps.Store.GetManagementTask(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}
	steps, err := s.deps.Store.ListManagementSteps(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": managementTaskDetailView{
		managementTaskView: newManagementTaskView(*task),
		Steps:              steps,
	}})
}

// handleCancelManagementTask answers POST /api/management/tasks/:id/cancel.
func (s *Server) handleCancelManagementTask(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	task, err := s.deps.Store.GetManagementTask(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	cancelled := s.deps.Management.Cancel(task.AccountID)
	c.JSON(http.StatusOK, gin.H{"id": id, "cancelled": cancelled})
}

// handleExportManagementTask answers GET /api/management/tasks/:id/export: the
// journal of the task as a CSV file, which is what an operator attaches to an
// audit trail.
func (s *Server) handleExportManagementTask(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	task, err := s.deps.Store.GetManagementTask(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	steps, err := s.deps.Store.ListManagementSteps(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"management-task-%d.csv\"", id))
	// A UTF-8 BOM is what makes a spreadsheet open an accented name correctly.
	if _, err := c.Writer.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		slog.Warn("writing the management CSV BOM failed", "task", id, "error", err)
		return
	}
	out := csv.NewWriter(c.Writer)
	_ = out.Write([]string{"step", "name", "status", "detail", "items", "bytes"})
	for _, step := range steps {
		_ = out.Write([]string{
			strconv.Itoa(step.Seq), step.Name, step.Status, step.Detail,
			strconv.Itoa(step.Items), strconv.FormatInt(step.Bytes, 10),
		})
	}
	out.Flush()
	if err := out.Error(); err != nil {
		slog.Warn("writing the management CSV failed", "task", id, "error", err)
	}
	_ = task
}
