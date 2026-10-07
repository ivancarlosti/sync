package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// Bounds of an audit run. They mirror the ones of the sync engine (runKeepCount)
// and add the ones specific to a report: how deep a tree may be walked, how many
// nodes a report may hold and how often the live counters reach the row.
const (
	// auditKeepCount is how many audits per account survive the pruner.
	auditKeepCount = 50
	// AuditNodeLimit caps the number of entries one report may hold, so a huge
	// tree cannot exhaust the database or the CSV download.
	AuditNodeLimit = 10000
	// AuditMaxDepth caps the requested depth: deeper than this is a typo, and a
	// report is a summary, not a backup manifest.
	AuditMaxDepth = 25
	// auditProgressEvery throttles the writes of the live counters.
	auditProgressEvery = 2 * time.Second
)

// AuditInput is the request that starts an audit.
type AuditInput struct {
	AccountID uint
	// DriveID and FolderID default to the drive root when empty.
	DriveID    string
	FolderID   string
	FolderPath string
	// Depth is how many folder levels below the selected folder are walked; 0 is
	// unlimited.
	Depth int
}

// AuditService executes content audits.
//
// An audit lists a provider folder tree to a chosen depth and stores the
// resulting report. It is the async brother of SyncService: Start reserves the
// account, writes a `running` row and launches the walk in the background, so
// the HTTP handler can answer 202 with an id the UI polls. One audit per account
// runs at a time, and the walk is single-use and cancellable.
type AuditService struct {
	store  *Store
	tokens *TokenManager
	now    func() time.Time

	mu      sync.Mutex
	running map[uint]*auditState
	wg      sync.WaitGroup
}

// auditState is the in-memory handle of an audit in flight. The cancel function
// is attached once the walk context exists, which is what Cancel triggers.
type auditState struct {
	cancel context.CancelFunc
}

// NewAuditService builds the service.
func NewAuditService(store *Store, tokens *TokenManager) *AuditService {
	return &AuditService{
		store:   store,
		tokens:  tokens,
		now:     func() time.Time { return time.Now().UTC() },
		running: map[uint]*auditState{},
	}
}

// Start launches an audit in the background and returns the run row immediately.
// The walk is not bound to the request context: finishing the request must not
// abort it.
func (s *AuditService) Start(ctx context.Context, input AuditInput) (*models.AuditRun, error) {
	if input.AccountID == 0 {
		return nil, fmt.Errorf("%w: an account is required", ErrValidation)
	}
	if input.Depth < 0 {
		return nil, fmt.Errorf("%w: the depth cannot be negative", ErrValidation)
	}
	if input.Depth > AuditMaxDepth {
		input.Depth = AuditMaxDepth
	}

	if err := s.reserve(ctx, input.AccountID); err != nil {
		return nil, err
	}
	// Resolving the account before a row is written keeps a missing or
	// unreachable account a clean 404/412 instead of a failed audit.
	session, err := s.tokens.Resolve(ctx, input.AccountID)
	if err != nil {
		s.release(input.AccountID)
		return nil, err
	}
	// The walk context is created here, not inside the goroutine, so a Cancel
	// that races the very start of the audit is never lost.
	runCtx, cancel := context.WithCancel(context.Background())
	s.attach(input.AccountID, cancel)
	run, err := s.createRun(ctx, session.Account, input)
	if err != nil {
		s.release(input.AccountID)
		return nil, err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.release(input.AccountID)
		s.execute(runCtx, session, run)
	}()
	return run, nil
}

// Running reports whether an account has an audit in flight.
func (s *AuditService) Running(accountID uint) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.running[accountID]
	return ok
}

// Cancel aborts the audit of an account. The walk stops at the next checkpoint
// and the run is marked as cancelled.
func (s *AuditService) Cancel(accountID uint) bool {
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

// Wait blocks until every background audit finished; the server calls it during
// shutdown so a walk in flight is not left half written.
func (s *AuditService) Wait() {
	s.wg.Wait()
}

// reserve marks an account as being audited, refusing a second concurrent audit
// and one left behind by a previous process.
func (s *AuditService) reserve(ctx context.Context, accountID uint) error {
	s.mu.Lock()
	if _, busy := s.running[accountID]; busy {
		s.mu.Unlock()
		return fmt.Errorf("%w: an audit of this account is already running", ErrBusy)
	}
	s.running[accountID] = &auditState{}
	s.mu.Unlock()

	if _, err := s.store.RunningAudit(ctx, accountID); err == nil {
		s.release(accountID)
		return fmt.Errorf("%w: an audit of this account is still marked as running", ErrBusy)
	} else if !IsNotFound(err) {
		s.release(accountID)
		return err
	}
	return nil
}

// attach hands the cancel function of the walk context to the reservation.
func (s *AuditService) attach(accountID uint, cancel context.CancelFunc) {
	s.mu.Lock()
	if state, ok := s.running[accountID]; ok && state != nil {
		state.cancel = cancel
	}
	s.mu.Unlock()
}

// release forgets an account reservation and cancels a context still attached.
func (s *AuditService) release(accountID uint) {
	s.mu.Lock()
	state, ok := s.running[accountID]
	delete(s.running, accountID)
	s.mu.Unlock()
	if ok && state != nil && state.cancel != nil {
		state.cancel()
	}
}

// createRun inserts the audit row in the running state.
func (s *AuditService) createRun(ctx context.Context, account *models.ConnectedAccount, input AuditInput) (*models.AuditRun, error) {
	run := &models.AuditRun{
		AccountID:    account.ID,
		AccountEmail: account.Email,
		Provider:     account.Provider,
		DriveID:      driveOrDefault(input.DriveID),
		RootFolderID: folderOrDefault(input.FolderID),
		RootPath:     displayRoot(input.FolderPath),
		MaxDepth:     input.Depth,
		Status:       string(models.RunRunning),
		Trigger:      string(models.TriggerManual),
		StartedAt:    s.now(),
	}
	if err := s.store.CreateAudit(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

// execute walks the tree, stores the report and closes the run row. walkCtx is
// the cancellable context created by Start (Cancel reaches it through the
// reservation); the persistence below deliberately uses a fresh context so a
// cancelled walk still stores the report it managed to build.
func (s *AuditService) execute(walkCtx context.Context, session ProviderSession, run *models.AuditRun) {
	ctx := context.Background()
	auditor := &auditor{
		service:      s,
		run:          run,
		provider:     session.Provider,
		creds:        session.Credentials,
		tokens:       session.Tokens,
		drive:        run.DriveID,
		lastProgress: s.now().Add(-auditProgressEvery),
	}
	slog.Info("audit started", "audit", run.ID, "account", run.AccountID, "depth", run.MaxDepth)

	size, files, folders := auditor.expand(walkCtx, run.RootFolderID, "", rootName(run.RootPath), 0)
	run.TotalSize = size
	run.Files = files
	// The root itself is a folder and is not counted by expand.
	run.Folders = folders + 1
	run.MaxDepthReached = auditor.maxDepthReached
	run.Truncated = auditor.truncated

	if len(auditor.entries) > 0 {
		if err := s.store.AddAuditEntries(ctx, auditor.entries); err != nil {
			slog.Error("storing the audit entries failed", "audit", run.ID, "error", err)
			if auditor.fatal == nil {
				auditor.fatal = err
			}
		}
	}

	finished := s.now()
	run.FinishedAt = &finished
	run.DurationMS = finished.Sub(run.StartedAt).Milliseconds()
	run.Status = string(auditor.status(walkCtx))
	if run.Message == "" {
		switch models.RunStatus(run.Status) {
		case models.RunSuccess:
			run.Message = "completed"
		case models.RunCancelled:
			run.Message = "the audit was cancelled"
		case models.RunTimeout:
			run.Message = "the audit ran out of time"
		}
	}
	if err := s.store.FinishAudit(ctx, run); err != nil {
		slog.Error("closing the audit failed", "audit", run.ID, "error", err)
	}
	if err := s.store.PruneAudits(ctx, auditKeepCount); err != nil {
		slog.Warn("pruning the audit history failed", "error", err)
	}
	if auditor.fatal != nil {
		slog.Error("audit failed", "audit", run.ID, "account", run.AccountID, "error", auditor.fatal)
	} else {
		slog.Info("audit finished",
			"audit", run.ID, "status", run.Status, "files", run.Files, "folders", run.Folders,
			"bytes", run.TotalSize, "depth", run.MaxDepthReached, "truncated", run.Truncated)
	}
}

// auditor is one walk in flight. It is single-use: created by execute and
// discarded once the report is stored.
type auditor struct {
	service  *AuditService
	run      *models.AuditRun
	provider providers.Provider
	creds    providers.Credentials
	tokens   *providers.Tokens
	drive    string

	entries []models.AuditEntry
	fatal   error

	files           int
	folders         int
	total           int64
	maxDepthReached int
	truncated       bool
	lastProgress    time.Time
}

// expand walks the folder `parentID` at `depth`, records its entry and every
// descendant in DFS preorder and returns the subtree (size, files, folders not
// counting the folder itself). A folder sitting at the depth boundary is
// recorded but not descended into, so its subtree reads as zero.
func (a *auditor) expand(ctx context.Context, parentID, path, name string, depth int) (int64, int, int) {
	if ctx.Err() != nil || a.truncated {
		return 0, 0, 0
	}
	if !a.emit(models.AuditEntry{
		Kind:     string(models.AuditKindFolder),
		Path:     path,
		Name:     name,
		Depth:    depth,
		Expanded: true,
	}) {
		return 0, 0, 0
	}
	index := len(a.entries) - 1
	a.folders++
	a.track(depth)

	if a.run.MaxDepth > 0 && depth >= a.run.MaxDepth {
		a.entries[index].Expanded = false
		a.progress(ctx)
		return 0, 0, 0
	}

	children, err := a.children(ctx, parentID)
	if err != nil {
		if a.provider.IsNotFound(err) {
			// The folder was removed between two listings: nothing to report.
			slog.Debug("folder vanished during the audit", "audit", a.run.ID, "path", displayPath(path))
			a.progress(ctx)
			return 0, 0, 0
		}
		if a.fatal == nil {
			a.fatal = fmt.Errorf("listing %q failed: %w", displayPath(path), err)
			a.run.Message = a.fatal.Error()
		}
		a.progress(ctx)
		return 0, 0, 0
	}
	sortChildren(children)

	var total int64
	var files, folders int
	for _, child := range children {
		if ctx.Err() != nil || a.truncated {
			break
		}
		childPath := joinPath(path, child.Name)
		if child.Dir() {
			size, subFiles, subFolders := a.expand(ctx, child.ID, childPath, child.Name, depth+1)
			total += size
			files += subFiles
			// The child folder itself, even at a boundary where it was not
			// expanded, counts as one folder.
			folders += subFolders + 1
			continue
		}
		if !a.emit(models.AuditEntry{
			Kind:       string(models.AuditKindFile),
			Path:       childPath,
			Name:       child.Name,
			Depth:      depth + 1,
			Size:       child.Size,
			TotalSize:  child.Size,
			Files:      1,
			ModifiedAt: timeOrNil(child.ModifiedAt),
			MimeType:   child.MimeType,
		}) {
			break
		}
		a.files++
		a.total += child.Size
		total += child.Size
		files++
		a.track(depth + 1)
	}

	a.entries[index].TotalSize = total
	a.entries[index].Files = files
	a.entries[index].Folders = folders
	a.progress(ctx)
	return total, files, folders
}

// children lists one folder, renewing the access token when it has expired and
// retrying once when the provider rejects it mid-walk (same recovery as the sync
// engine, which is what lets a long audit ride out a token rotation).
func (a *auditor) children(ctx context.Context, folderID string) ([]providers.Item, error) {
	if a.tokens != nil && a.tokens.Expired(a.service.now()) {
		if err := a.reissue(ctx); err != nil {
			return nil, err
		}
	}
	children, err := a.provider.Children(ctx, a.creds, a.tokens, a.drive, folderID)
	if err != nil && a.provider.IsUnauthorized(err) {
		if renewErr := a.reissue(ctx); renewErr == nil {
			children, err = a.provider.Children(ctx, a.creds, a.tokens, a.drive, folderID)
		} else {
			err = renewErr
		}
	}
	return children, err
}

// reissue forces a token renewal and swaps the snapshot in place.
func (a *auditor) reissue(ctx context.Context) error {
	renewed, err := a.service.tokens.Reissue(ctx, a.run.AccountID)
	if err != nil {
		return err
	}
	a.tokens = renewed
	return nil
}

// emit appends one entry unless the node budget is spent, in which case it flags
// the report truncated and reports false.
func (a *auditor) emit(entry models.AuditEntry) bool {
	if len(a.entries) >= AuditNodeLimit {
		a.truncated = true
		return false
	}
	entry.AuditID = a.run.ID
	entry.CreatedAt = a.service.now()
	a.entries = append(a.entries, entry)
	return true
}

// track records the deepest level reached by the walk.
func (a *auditor) track(depth int) {
	if depth > a.maxDepthReached {
		a.maxDepthReached = depth
	}
}

// progress mirrors the live counters onto the row, throttled so a big tree does
// not turn every node into a write.
func (a *auditor) progress(ctx context.Context) {
	now := a.service.now()
	if now.Sub(a.lastProgress) < auditProgressEvery {
		return
	}
	a.lastProgress = now
	if err := a.service.store.UpdateAuditProgress(ctx, a.run.ID, a.files, a.folders, a.total, a.maxDepthReached, a.truncated); err != nil {
		slog.Warn("updating the audit progress failed", "audit", a.run.ID, "error", err)
	}
}

// status derives the final status of the audit.
func (a *auditor) status(ctx context.Context) models.RunStatus {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(a.fatal, context.Canceled) {
		return models.RunCancelled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(a.fatal, context.DeadlineExceeded) {
		return models.RunTimeout
	}
	if a.fatal != nil {
		return models.RunFailed
	}
	return models.RunSuccess
}

// sortChildren orders a folder listing folders-first then files, both
// case-insensitively, so the report reads the way the folder browser shows it.
func sortChildren(items []providers.Item) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
}

// driveOrDefault and folderOrDefault map an empty selection onto the drive root.
func driveOrDefault(value string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return providers.DriveRoot
}

func folderOrDefault(value string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return providers.DriveRoot
}

// displayRoot renders the stored root path: the operator selection, or "/" for
// the drive root.
func displayRoot(value string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return "/"
}

// rootName is the display name of the root entry: the last segment of the
// selected path, or the provider root label.
func rootName(rootPath string) string {
	trimmed := strings.Trim(strings.TrimSpace(rootPath), "/")
	if trimmed == "" {
		return providers.DriveRoot
	}
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}

// timeOrNil keeps a zero modification time out of the database.
func timeOrNil(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
