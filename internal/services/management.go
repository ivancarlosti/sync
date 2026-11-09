package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// Bounds of an administrative operation.
const (
	// ManagementTimeout bounds one task. Moving a large Drive or copying a
	// mailbox is slow, so the budget is generous; it exists so a throttled
	// provider ends as `timeout` instead of holding the account slot forever.
	ManagementTimeout = 30 * time.Minute
	// managementKeepCount is how many tasks per account survive the pruner.
	managementKeepCount = 50
)

// ManagementParam is one input an operation accepts, described for the UI. The
// label of a parameter is translated by the frontend (`management.params.<key>`),
// so the API never carries English text.
type ManagementParam struct {
	Key      string `json:"key"`
	Required bool   `json:"required"`
}

// managementOperation is the catalogue entry of one kind: which provider offers
// it, the capability it needs, whether it is destructive and the inputs it takes.
type managementOperation struct {
	Kind       string
	Provider   models.ProviderName
	Capability providers.Capability
	Danger     bool
	Params     []ManagementParam
	// provide reports whether a provider implementation can run the kind. It is
	// what keeps a Google-only operation off a Microsoft account (and vice
	// versa) without a second, parallel table of provider/kind pairs.
	provide func(provider providers.Provider) bool
}

// managementOperations is the catalogue the API exposes (GET
// /api/management/operations). It is the single source of truth of the screen: a
// kind that is not here cannot be started, so an unknown kind in a request is a
// clean validation error.
var managementOperations = []managementOperation{
	{
		Kind:       models.ManagementKindDriveToNewSharedDrive,
		Provider:   models.ProviderGoogle,
		Capability: providers.CapabilityFiles,
		Danger:     true,
		Params:     []ManagementParam{{Key: "name", Required: true}, {Key: "source_drive_id"}, {Key: "confirm", Required: true}},
		provide:    isSharedDriveManager,
	},
	{
		Kind:       models.ManagementKindDriveToSharedDrive,
		Provider:   models.ProviderGoogle,
		Capability: providers.CapabilityFiles,
		Danger:     true,
		Params: []ManagementParam{
			{Key: "source_drive_id"}, {Key: "target_drive_id", Required: true},
			{Key: "target_folder_id"}, {Key: "target_folder_name"}, {Key: "confirm", Required: true},
		},
		provide: isSharedDriveManager,
	},
	{
		Kind:       models.ManagementKindArchiveUserGroup,
		Provider:   models.ProviderGoogle,
		Capability: providers.CapabilityGroups,
		Danger:     true,
		Params: []ManagementParam{
			{Key: "user_email", Required: true}, {Key: "group_email", Required: true},
			{Key: "group_name"}, {Key: "member_email"}, {Key: "archive_email"}, {Key: "confirm", Required: true},
		},
		provide: isDirectoryArchiver,
	},
	{
		Kind:       models.ManagementKindMicrosoftCopyMailbox,
		Provider:   models.ProviderMicrosoft,
		Capability: providers.CapabilityMail,
		Params:     []ManagementParam{{Key: "source"}, {Key: "target", Required: true}, {Key: "max"}},
		provide:    isMailboxCopier,
	},
	{
		Kind:       models.ManagementKindOneDriveToSite,
		Provider:   models.ProviderMicrosoft,
		Capability: providers.CapabilitySitesAdmin,
		Danger:     true,
		Params: []ManagementParam{
			{Key: "site_name"}, {Key: "site_display_name"}, {Key: "target_drive_id"},
			{Key: "target_folder_id"}, {Key: "confirm", Required: true},
		},
		provide: isSiteContentCopier,
	},
	{
		Kind:       models.ManagementKindMicrosoftCalendars,
		Provider:   models.ProviderMicrosoft,
		Capability: providers.CapabilityCalendars,
		Danger:     true,
		Params:     []ManagementParam{{Key: "source"}, {Key: "target", Required: true}, {Key: "confirm", Required: true}},
		provide:    isCalendarTransferrer,
	},
	{
		Kind:       models.ManagementKindFolderSizes,
		Provider:   models.ProviderMicrosoft,
		Capability: providers.CapabilityFiles,
		Params:     []ManagementParam{{Key: "drive_id"}},
		provide:    isFolderSizeReporter,
	},
}

// Provider capability probes. An interface assertion is all that decides whether
// an operation is offered, which is what makes a provider that gains an operation
// later light up without a second table to edit.
func isSharedDriveManager(provider providers.Provider) bool {
	_, ok := provider.(providers.SharedDriveManager)
	return ok
}

func isDirectoryArchiver(provider providers.Provider) bool {
	_, ok := provider.(providers.DirectoryArchiver)
	return ok
}

func isMailboxCopier(provider providers.Provider) bool {
	_, ok := provider.(providers.MailboxCopier)
	return ok
}

func isCalendarTransferrer(provider providers.Provider) bool {
	_, ok := provider.(providers.CalendarTransferrer)
	return ok
}

func isFolderSizeReporter(provider providers.Provider) bool {
	_, ok := provider.(providers.FolderSizeReporter)
	return ok
}

func isSiteContentCopier(provider providers.Provider) bool {
	_, ok := provider.(providers.SiteContentCopier)
	return ok
}

// ManagementOperationView is one catalogue entry as the API answers it, decorated
// with whether the account can run it right now.
type ManagementOperationView struct {
	Kind       string            `json:"kind"`
	Provider   string            `json:"provider"`
	Capability string            `json:"capability"`
	Danger     bool              `json:"danger"`
	Params     []ManagementParam `json:"params"`
	// Supported is true when the provider implements the operation and the
	// grant covers its capability: the start button is only offered then.
	Supported bool `json:"supported"`
	// MissingCapability marks an operation the provider supports but the
	// account's grant does not cover: the UI offers a reconnect instead.
	MissingCapability bool `json:"missing_capability"`
}

// ManagementInput is the request that starts a task.
type ManagementInput struct {
	AccountID uint
	Kind      string
	Params    map[string]string
	Trigger   string
}

// ManagementService executes administrative operations and keeps their journal.
//
// Like ReportService it is the async brother of SyncService: Start reserves the
// account, writes a `running` row and runs the operation in the background, so the
// HTTP handler answers 202 with an id the UI polls. One task per account runs at a
// time.
type ManagementService struct {
	store    *Store
	tokens   *TokenManager
	registry *providers.Registry
	now      func() time.Time

	mu      sync.Mutex
	running map[uint]*managementState
	wg      sync.WaitGroup
}

// managementState is the in-memory handle of a task in flight.
type managementState struct {
	cancel context.CancelFunc
}

// NewManagementService builds the service.
func NewManagementService(store *Store, tokens *TokenManager, registry *providers.Registry) *ManagementService {
	return &ManagementService{
		store:    store,
		tokens:   tokens,
		registry: registry,
		now:      func() time.Time { return time.Now().UTC() },
		running:  map[uint]*managementState{},
	}
}

// Operations answers the catalogue of an account, decorated with whether that
// account can run each operation right now.
func (s *ManagementService) Operations(ctx context.Context, accountID uint) ([]ManagementOperationView, error) {
	account, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	provider, err := s.registry.Get(models.ProviderName(account.Provider))
	if err != nil {
		return nil, err
	}
	permissions := permissionsOf(provider)
	granted := providers.SplitScopes(account.Scopes)
	views := make([]ManagementOperationView, 0, len(managementOperations))
	for _, op := range managementOperations {
		if op.Provider != models.ProviderName(account.Provider) || !op.provide(provider) {
			continue
		}
		view := ManagementOperationView{
			Kind:       op.Kind,
			Provider:   string(op.Provider),
			Capability: string(op.Capability),
			Danger:     op.Danger,
			Params:     op.Params,
		}
		if permissions != nil && providers.DeclaresCapability(permissions, op.Capability) {
			view.Supported = providers.HasCapability(permissions, granted, op.Capability)
			view.MissingCapability = !view.Supported
		} else {
			view.Supported = true
		}
		views = append(views, view)
	}
	return views, nil
}

// operationFor returns the catalogue entry of a kind.
func operationFor(kind string) (managementOperation, bool) {
	for _, op := range managementOperations {
		if op.Kind == kind {
			return op, true
		}
	}
	return managementOperation{}, false
}

// permissionsOf reads a provider's permission table, when it publishes one.
func permissionsOf(provider providers.Provider) []providers.Permission {
	if catalog, ok := provider.(providers.ScopeCatalog); ok {
		return catalog.Permissions()
	}
	return nil
}

// confirmTrue reads the confirmation flag of a destructive operation.
func confirmTrue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// Start launches an operation in the background and returns the row immediately.
// The execution is not bound to the request context: finishing the request must
// not abort it.
func (s *ManagementService) Start(ctx context.Context, input ManagementInput) (*models.ManagementTask, error) {
	if input.AccountID == 0 {
		return nil, fmt.Errorf("%w: an account is required", ErrValidation)
	}
	op, ok := operationFor(input.Kind)
	if !ok {
		return nil, fmt.Errorf("%w: unknown operation %q", ErrValidation, input.Kind)
	}
	params := make(map[string]string, len(input.Params))
	for key, value := range input.Params {
		params[key] = strings.TrimSpace(value)
	}
	for _, param := range op.Params {
		if param.Required && param.Key != "confirm" && params[param.Key] == "" {
			return nil, fmt.Errorf("%w: %s is required", ErrValidation, param.Key)
		}
	}
	if op.Danger && !confirmTrue(params["confirm"]) {
		return nil, fmt.Errorf("%w: this operation needs an explicit confirmation", ErrValidation)
	}

	if err := s.reserve(ctx, input.AccountID); err != nil {
		return nil, err
	}
	session, err := s.tokens.Resolve(ctx, input.AccountID)
	if err != nil {
		s.release(input.AccountID)
		return nil, err
	}
	if !op.provide(session.Provider) {
		s.release(input.AccountID)
		return nil, fmt.Errorf("%w: %s does not support this operation", ErrValidation, session.Account.Provider)
	}
	if permissions := permissionsOf(session.Provider); permissions != nil &&
		providers.DeclaresCapability(permissions, op.Capability) &&
		!providers.HasCapability(permissions, providers.SplitScopes(session.Account.Scopes), op.Capability) {
		s.release(input.AccountID)
		return nil, fmt.Errorf("%w: reconnect the account to grant the %q permission", ErrConsent, op.Capability)
	}

	trigger := strings.TrimSpace(input.Trigger)
	if trigger == "" {
		trigger = string(models.TriggerManual)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.attach(input.AccountID, cancel)
	task, err := s.createTask(ctx, session.Account, op, params, trigger)
	if err != nil {
		s.release(input.AccountID)
		return nil, err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.release(input.AccountID)
		s.execute(runCtx, session, op, params, task)
	}()
	return task, nil
}

// Running reports whether an account has a task in flight.
func (s *ManagementService) Running(accountID uint) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.running[accountID]
	return ok
}

// Cancel aborts the task of an account. It stops at the next checkpoint and the
// row is marked as cancelled.
func (s *ManagementService) Cancel(accountID uint) bool {
	s.mu.Lock()
	state, ok := s.running[accountID]
	var cancel context.CancelFunc
	if ok && state != nil {
		cancel = state.cancel
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return ok
}

// Wait blocks until every background task finished; the server calls it during
// shutdown so a task in flight is not left half written.
func (s *ManagementService) Wait() { s.wg.Wait() }

// reserve marks an account as busy, refusing a second concurrent task and one
// left behind by a previous process.
func (s *ManagementService) reserve(ctx context.Context, accountID uint) error {
	s.mu.Lock()
	if _, busy := s.running[accountID]; busy {
		s.mu.Unlock()
		return fmt.Errorf("%w: an operation of this account is already running", ErrBusy)
	}
	s.running[accountID] = &managementState{}
	s.mu.Unlock()

	if _, err := s.store.RunningManagementTask(ctx, accountID); err == nil {
		s.release(accountID)
		return fmt.Errorf("%w: an operation of this account is still marked as running", ErrBusy)
	} else if !IsNotFound(err) {
		s.release(accountID)
		return err
	}
	return nil
}

// attach hands the cancel function of the pass context to the reservation.
func (s *ManagementService) attach(accountID uint, cancel context.CancelFunc) {
	s.mu.Lock()
	if state, ok := s.running[accountID]; ok && state != nil {
		state.cancel = cancel
	}
	s.mu.Unlock()
}

// release forgets an account reservation and cancels a context still attached.
func (s *ManagementService) release(accountID uint) {
	s.mu.Lock()
	state, ok := s.running[accountID]
	delete(s.running, accountID)
	s.mu.Unlock()
	if ok && state != nil && state.cancel != nil {
		state.cancel()
	}
}

// createTask inserts the task row in the running state, storing the request.
func (s *ManagementService) createTask(ctx context.Context, account *models.ConnectedAccount, op managementOperation, params map[string]string, trigger string) (*models.ManagementTask, error) {
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("services: encoding the management request: %w", err)
	}
	task := &models.ManagementTask{
		AccountID:    account.ID,
		AccountEmail: account.Email,
		Provider:     account.Provider,
		Kind:         op.Kind,
		Title:        op.Kind,
		Status:       string(models.RunRunning),
		Trigger:      trigger,
		Params:       string(encoded),
		StartedAt:    s.now(),
	}
	if err := s.store.CreateManagementTask(ctx, task); err != nil {
		return nil, err
	}
	return task, nil
}

// execute runs the operation, journals every step and closes the row. passCtx is
// the cancellable context created by Start plus the task budget; the persistence
// below uses a fresh context so a cancelled task still stores what it managed to
// do.
func (s *ManagementService) execute(passCtx context.Context, session ProviderSession, op managementOperation, params map[string]string, task *models.ManagementTask) {
	ctx := context.Background()
	passCtx, cancel := context.WithTimeout(passCtx, ManagementTimeout)
	defer cancel()

	slog.Info("management task started",
		"task", task.ID, "account", task.AccountID, "kind", task.Kind, "trigger", task.Trigger)

	lastWrite := s.now().Add(-2 * time.Second)
	progress := func(step providers.ManagementStep) {
		task.StepCount++
		row := &models.ManagementTaskStep{
			TaskID: task.ID,
			Seq:    task.StepCount,
			Name:   step.Name,
			Status: step.Status,
			Detail: step.Detail,
			Items:  step.Items,
			Bytes:  step.Bytes,
		}
		if err := s.store.AppendManagementStep(ctx, row); err != nil {
			slog.Warn("appending a management step failed", "task", task.ID, "error", err)
		}
		if step.Status == providers.StepFailed {
			task.Failed++
		}
		task.ItemsProcessed += step.Items
		task.BytesMoved += step.Bytes
		if now := s.now(); now.Sub(lastWrite) >= 2*time.Second {
			lastWrite = now
			if err := s.store.UpdateManagementProgress(ctx, task.ID, task.StepCount, task.Failed, task.ItemsProcessed, task.BytesMoved); err != nil {
				slog.Warn("updating the management progress failed", "task", task.ID, "error", err)
			}
		}
	}

	result, runErr := s.run(passCtx, session, op, params, progress)

	finished := s.now()
	task.FinishedAt = &finished
	task.DurationMS = finished.Sub(task.StartedAt).Milliseconds()
	task.Status = string(managementStatus(passCtx, runErr, task.Failed))
	if encoded, err := json.Marshal(resultData(result)); err == nil {
		task.Result = string(encoded)
	}
	switch {
	case runErr != nil && task.Message == "":
		task.Message = runErr.Error()
	case result.Message != "":
		task.Message = result.Message
	}
	if err := s.store.FinishManagementTask(ctx, task); err != nil {
		slog.Error("closing the management task failed", "task", task.ID, "error", err)
	}
	if err := s.store.PruneManagementTasks(ctx, managementKeepCount); err != nil {
		slog.Warn("pruning the management history failed", "error", err)
	}
	if runErr != nil {
		slog.Error("management task failed", "task", task.ID, "account", task.AccountID, "kind", task.Kind, "error", runErr)
		return
	}
	slog.Info("management task finished",
		"task", task.ID, "status", task.Status, "steps", task.StepCount,
		"failed", task.Failed, "items", task.ItemsProcessed, "bytes", task.BytesMoved)
}

// resultData keeps the summary counters and the provider details in one JSON
// document, which is what the history screen renders next to the message.
func resultData(result providers.ManagementResult) map[string]any {
	data := map[string]any{"items": result.Items, "bytes": result.Bytes}
	if len(result.Data) > 0 {
		data["details"] = result.Data
	}
	return data
}

// run dispatches one task to the provider interface behind its kind. The catalogue
// already proved the provider implements it, so a missing interface is a
// programming error reported as a failed task rather than a panic.
func (s *ManagementService) run(ctx context.Context, session ProviderSession, op managementOperation, params map[string]string, progress providers.ProgressFunc) (providers.ManagementResult, error) {
	creds, tokens := session.Credentials, session.Tokens
	switch op.Kind {
	case models.ManagementKindDriveToNewSharedDrive:
		manager, ok := session.Provider.(providers.SharedDriveManager)
		if !ok {
			return providers.ManagementResult{}, providers.ErrUnsupported
		}
		drive, err := manager.CreateSharedDrive(ctx, creds, tokens, params["name"])
		if err != nil {
			return providers.ManagementResult{}, err
		}
		progress(providers.ManagementStep{Name: "create shared drive", Status: providers.StepSuccess, Detail: drive.Name, Items: 1})
		result, err := manager.MoveIntoSharedDrive(ctx, creds, tokens, params["source_drive_id"], drive.ID, "", progress)
		if result.Data == nil {
			result.Data = map[string]string{}
		}
		result.Data["drive_id"] = drive.ID
		result.Data["drive_name"] = drive.Name
		return result, err
	case models.ManagementKindDriveToSharedDrive:
		manager, ok := session.Provider.(providers.SharedDriveManager)
		if !ok {
			return providers.ManagementResult{}, providers.ErrUnsupported
		}
		targetFolderID := strings.TrimSpace(params["target_folder_id"])
		if name := strings.TrimSpace(params["target_folder_name"]); name != "" {
			parent := targetFolderID
			if parent == "" {
				parent = providers.DriveRoot
			}
			folder, err := session.Provider.CreateFolder(ctx, creds, tokens, params["target_drive_id"], parent, name)
			if err != nil {
				return providers.ManagementResult{}, fmt.Errorf("creating the target folder %q failed: %w", name, err)
			}
			targetFolderID = folder.ID
			progress(providers.ManagementStep{
				Name: "create target folder", Status: providers.StepSuccess, Detail: folder.Name, Items: 1,
			})
		}
		result, err := manager.MoveIntoSharedDrive(ctx, creds, tokens, params["source_drive_id"], params["target_drive_id"], targetFolderID, progress)
		if result.Data == nil {
			result.Data = map[string]string{}
		}
		result.Data["target_folder_id"] = targetFolderID
		return result, err
	case models.ManagementKindArchiveUserGroup:
		archiver, ok := session.Provider.(providers.DirectoryArchiver)
		if !ok {
			return providers.ManagementResult{}, providers.ErrUnsupported
		}
		return archiver.ArchiveUserToGroup(ctx, creds, tokens, providers.ArchiveUserInput{
			UserEmail:    params["user_email"],
			GroupEmail:   params["group_email"],
			GroupName:    params["group_name"],
			MemberEmail:  params["member_email"],
			ArchiveEmail: params["archive_email"],
		}, progress)
	case models.ManagementKindMicrosoftCopyMailbox:
		copier, ok := session.Provider.(providers.MailboxCopier)
		if !ok {
			return providers.ManagementResult{}, providers.ErrUnsupported
		}
		return copier.CopyMailbox(ctx, creds, tokens, providers.CopyMailboxInput{
			Source: params["source"],
			Target: params["target"],
			Max:    atoi(params["max"]),
		}, progress)
	case models.ManagementKindMicrosoftCalendars:
		transferrer, ok := session.Provider.(providers.CalendarTransferrer)
		if !ok {
			return providers.ManagementResult{}, providers.ErrUnsupported
		}
		return transferrer.TransferCalendars(ctx, creds, tokens, providers.CalendarTransferInput{
			Source: params["source"],
			Target: params["target"],
		}, progress)
	case models.ManagementKindOneDriveToSite:
		copier, ok := session.Provider.(providers.SiteContentCopier)
		if !ok {
			return providers.ManagementResult{}, providers.ErrUnsupported
		}
		return copier.CopyDriveToSite(ctx, creds, tokens, providers.SiteCopyInput{
			SiteName:        params["site_name"],
			SiteDisplayName: params["site_display_name"],
			TargetDriveID:   params["target_drive_id"],
			TargetFolderID:  params["target_folder_id"],
		}, progress)
	case models.ManagementKindFolderSizes:
		reporter, ok := session.Provider.(providers.FolderSizeReporter)
		if !ok {
			return providers.ManagementResult{}, providers.ErrUnsupported
		}
		return reporter.ReportFolderSizes(ctx, creds, tokens, providers.FolderSizeInput{DriveID: params["drive_id"]}, progress)
	default:
		return providers.ManagementResult{}, fmt.Errorf("%w: unknown operation %q", providers.ErrUnsupported, op.Kind)
	}
}

// managementStatus maps the outcome of a task onto the lifecycle status of the row.
func managementStatus(passCtx context.Context, err error, failed int) models.RunStatus {
	switch {
	case err != nil && errors.Is(passCtx.Err(), context.DeadlineExceeded):
		return models.RunTimeout
	case err != nil && passCtx.Err() != nil:
		return models.RunCancelled
	case err != nil:
		return models.RunFailed
	case failed > 0:
		return models.RunPartial
	default:
		return models.RunSuccess
	}
}

// atoi parses an optional integer parameter, answering 0 for a missing or
// malformed value (the provider then applies its default).
func atoi(raw string) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return value
}
