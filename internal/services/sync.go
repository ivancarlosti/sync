package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// runKeepCount is how many runs per job survive the retention pruner.
const runKeepCount = 50

// SyncService executes sync jobs.
//
// One run is a full reconciliation: it lists both trees, compares them with the
// sync_files state table of the job and applies the resulting plan. Nothing is
// hashed or downloaded unless the plan requires it, which keeps a run over an
// unchanged tree cheap. Every run is bounded by the configured timeout and can
// be cancelled from the UI.
type SyncService struct {
	store    *Store
	tokens   *TokenManager
	settings *SettingsService
	notifier *Notifier
	now      func() time.Time
	mu       sync.Mutex
	running  map[uint]*runState
	wg       sync.WaitGroup
}

// runState is the in-memory handle of a run in flight. The cancel function is
// attached once the run context exists, which is what Cancel() triggers.
type runState struct {
	cancel context.CancelFunc
}

// NewSyncService builds the service. notifier may be nil (no notifications are
// then published) and so may settings (the default timeout is used).
func NewSyncService(store *Store, tokens *TokenManager, settings *SettingsService, notifier *Notifier) *SyncService {
	return &SyncService{
		store:    store,
		tokens:   tokens,
		settings: settings,
		notifier: notifier,
		now:      func() time.Time { return time.Now().UTC() },
		running:  map[uint]*runState{},
	}
}

// Start launches a run in the background and returns the run row immediately, so
// the HTTP handler can answer 202 with an id the UI can poll. The run is not
// bound to the request context: finishing the request must not abort the sync.
func (s *SyncService) Start(ctx context.Context, jobID uint, trigger models.TriggerSource) (*models.SyncRun, error) {
	job, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if err := s.reserve(ctx, job); err != nil {
		return nil, err
	}
	run, err := s.createRun(ctx, job, trigger)
	if err != nil {
		s.release(job.ID)
		return nil, err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.release(job.ID)
		s.execute(context.Background(), job, run, trigger)
	}()
	return run, nil
}

// Run executes a job synchronously and returns the finished run. Tests and the
// scheduler use it; Start() is the asynchronous flavour used by the API.
func (s *SyncService) Run(ctx context.Context, job *models.SyncJob, trigger models.TriggerSource) (*models.SyncRun, error) {
	if err := s.reserve(ctx, job); err != nil {
		return nil, err
	}
	defer s.release(job.ID)
	run, err := s.createRun(ctx, job, trigger)
	if err != nil {
		return nil, err
	}
	s.execute(ctx, job, run, trigger)
	return run, nil
}

// StartIfIdle starts a job when nothing is running for it and returns the run,
// or nil when the job was already busy. The scheduler uses it so a slow run
// simply skips its next slot instead of piling up.
func (s *SyncService) StartIfIdle(ctx context.Context, jobID uint, trigger models.TriggerSource) (*models.SyncRun, error) {
	run, err := s.Start(ctx, jobID, trigger)
	if errors.Is(err, ErrBusy) {
		slog.Debug("sync job is already running, skipping this slot", "job", jobID)
		return nil, nil
	}
	return run, err
}

// Running reports whether a job has a run in flight.
func (s *SyncService) Running(jobID uint) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.running[jobID]
	return ok
}

// RunningJobs returns the ids of the jobs currently running.
func (s *SyncService) RunningJobs() []uint {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]uint, 0, len(s.running))
	for id := range s.running {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Cancel aborts the run of a job. The engine stops at the next checkpoint and
// marks the run as cancelled.
func (s *SyncService) Cancel(jobID uint) bool {
	s.mu.Lock()
	state, ok := s.running[jobID]
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

// Wait blocks until every background run finished; the server calls it during
// shutdown so a run in flight is not left half written.
func (s *SyncService) Wait() {
	s.wg.Wait()
	if s.notifier != nil {
		s.notifier.Wait()
	}
}

// reserve marks a job as running, refusing a second concurrent run and one
// left behind by a previous process.
func (s *SyncService) reserve(ctx context.Context, job *models.SyncJob) error {
	if job == nil {
		return fmt.Errorf("%w: no sync job given", ErrValidation)
	}
	if !job.Enabled {
		return fmt.Errorf("%w: the sync job is disabled", ErrValidation)
	}
	s.mu.Lock()
	if _, busy := s.running[job.ID]; busy {
		s.mu.Unlock()
		return fmt.Errorf("%w: the sync job is already running", ErrBusy)
	}
	s.running[job.ID] = &runState{}
	s.mu.Unlock()

	if _, err := s.store.RunningRun(ctx, job.ID); err == nil {
		s.release(job.ID)
		return fmt.Errorf("%w: a run of this job is still marked as running", ErrBusy)
	} else if !IsNotFound(err) {
		s.release(job.ID)
		return err
	}
	return nil
}

// attach hands the cancel function of the run context to the reservation, so
// Cancel() can reach a run that already started.
func (s *SyncService) attach(jobID uint, cancel context.CancelFunc) {
	s.mu.Lock()
	if state, ok := s.running[jobID]; ok && state != nil {
		state.cancel = cancel
	}
	s.mu.Unlock()
}

// release forgets a job reservation and cancels a context still attached to it.
func (s *SyncService) release(jobID uint) {
	s.mu.Lock()
	state, ok := s.running[jobID]
	delete(s.running, jobID)
	s.mu.Unlock()
	if ok && state != nil && state.cancel != nil {
		state.cancel()
	}
}

// createRun inserts the run row in the running state.
func (s *SyncService) createRun(ctx context.Context, job *models.SyncJob, trigger models.TriggerSource) (*models.SyncRun, error) {
	run := &models.SyncRun{
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    string(models.RunRunning),
		Trigger:   string(trigger),
		StartedAt: s.now(),
	}
	if err := s.store.CreateRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

// execute runs the engine and closes the run row, publishing the notification
// that matches the outcome.
func (s *SyncService) execute(ctx context.Context, job *models.SyncJob, run *models.SyncRun, trigger models.TriggerSource) {
	timeout := s.runTimeout(ctx)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s.attach(job.ID, cancel)

	slog.Info("sync run started",
		"job", job.ID, "name", job.Name, "run", run.ID, "trigger", trigger, "timeout", timeout)

	engine := &engine{service: s, job: job, run: run, state: map[string]models.SyncFile{}}
	engine.loadExcludes()
	if err := engine.reconcile(runCtx); err != nil {
		if run.Message == "" {
			run.Message = err.Error()
		}
	}

	if len(engine.items) > 0 {
		if err := s.store.AddRunItems(ctx, engine.items); err != nil {
			slog.Error("storing the run items failed", "job", job.ID, "run", run.ID, "error", err)
		}
	}

	finished := s.now()
	run.FinishedAt = &finished
	run.DurationMS = finished.Sub(run.StartedAt).Milliseconds()
	run.Status = string(engine.status(runCtx))
	if run.Message == "" && run.Status == string(models.RunSuccess) {
		run.Message = "completed"
	}
	if err := s.store.FinishRun(ctx, run, job); err != nil {
		slog.Error("closing the sync run failed", "job", job.ID, "run", run.ID, "error", err)
	}
	if err := s.store.PruneRuns(ctx, runKeepCount); err != nil {
		slog.Warn("pruning the run history failed", "job", job.ID, "error", err)
	}
	if engine.fatal != nil {
		slog.Error("sync run failed",
			"job", job.ID, "name", job.Name, "run", run.ID, "error", engine.fatal)
	} else {
		slog.Info("sync run finished",
			"job", job.ID, "name", job.Name, "run", run.ID, "status", run.Status,
			"scanned", run.FilesScanned, "created", run.FilesCreated, "updated", run.FilesUpdated,
			"deleted", run.FilesDeleted, "skipped", run.FilesSkipped, "conflicts", run.Conflicts,
			"errors", run.Errors, "bytes", run.BytesTransferred,
			"duration_ms", run.DurationMS)
	}
	s.publish(ctx, job, run)
}

// runTimeout returns the configured run timeout, falling back to the default.
func (s *SyncService) runTimeout(ctx context.Context) time.Duration {
	minutes := DefaultRunTimeout
	if s.settings != nil {
		minutes = s.settings.RunTimeout(ctx)
	}
	if minutes <= 0 {
		minutes = DefaultRunTimeout
	}
	return time.Duration(minutes) * time.Minute
}

// publish sends the notification matching the run outcome and clears the job
// "last error" field after a successful run. The payload keys match the ones
// renderMessage understands.
func (s *SyncService) publish(ctx context.Context, job *models.SyncJob, run *models.SyncRun) {
	if s.notifier == nil {
		return
	}
	data := map[string]any{
		"job":       job.Name,
		"job_id":    job.ID,
		"run":       run.ID,
		"status":    run.Status,
		"scanned":   run.FilesScanned,
		"added":     run.FilesCreated,
		"updated":   run.FilesUpdated,
		"deleted":   run.FilesDeleted,
		"skipped":   run.FilesSkipped,
		"conflicts": run.Conflicts,
		"failed":    run.Errors,
		"bytes":     run.BytesTransferred,
		"duration":  formatDuration(run.DurationMS),
		"error":     run.Message,
	}
	switch models.RunStatus(run.Status) {
	case models.RunSuccess:
		s.notifier.Publish(ctx, models.EventSyncSuccess, data)
	case models.RunFailed:
		s.notifier.Publish(ctx, models.EventSyncRunFailed, data)
	default:
		s.notifier.Publish(ctx, models.EventSyncError, data)
	}
}

// formatDuration renders a run duration for a notification body.
func formatDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	duration := time.Duration(ms) * time.Millisecond
	if duration < time.Minute {
		return fmt.Sprintf("%.1fs", duration.Seconds())
	}
	return duration.Round(time.Second).String()
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// engine is one execution of a job. It is single-use: created by execute() and
// discarded when the run finishes.
type engine struct {
	service  *SyncService
	job      *models.SyncJob
	run      *models.SyncRun
	state    map[string]models.SyncFile
	items    []models.SyncItem
	excludes []string
	source   *endpoint
	target   *endpoint
	forward  bool
	backward bool
	fatal    error
}

// endpoint is one side of a job: the account, its provider client, the selected
// drive/folder and the tree listed during the run.
type endpoint struct {
	account     *models.ConnectedAccount
	provider    providers.Provider
	creds       providers.Credentials
	tokens      *providers.Tokens
	drive       string
	root        string
	files       map[string]providers.Item
	folders     map[string]string
	unsupported []providers.Item
}

// label names the side in logs and evidence strings.
func (e *endpoint) label() string {
	return string(e.provider.Name())
}

// reconcile is the body of a run: resolve both sides, list both trees, apply the
// plan and record the outcome.
func (e *engine) reconcile(ctx context.Context) error {
	e.applyDirections()

	source, err := e.resolveEndpoint(ctx, e.job.SourceAccountID, e.job.SourceDriveID, e.job.SourceFolderID)
	if err != nil {
		return e.fail(err)
	}
	e.source = source
	target, err := e.resolveEndpoint(ctx, e.job.DestinationAccountID, e.job.DestinationDriveID, e.job.DestinationFolderID)
	if err != nil {
		return e.fail(err)
	}
	e.target = target

	state, err := e.service.store.FileState(ctx, e.job.ID)
	if err != nil {
		return e.fail(err)
	}
	e.state = state

	for _, side := range []*endpoint{e.source, e.target} {
		if err := e.walk(ctx, side); err != nil {
			return e.fail(err)
		}
		e.recordUnsupported(side)
	}
	if err := e.reconcilePaths(ctx); err != nil {
		return e.fail(err)
	}
	if e.fatal != nil {
		return e.fatal
	}

	now := e.service.now()
	for _, side := range []*endpoint{e.source, e.target} {
		if err := e.service.store.TouchAccount(ctx, side.account.ID, now); err != nil {
			slog.Warn("recording the account usage failed", "account", side.account.ID, "error", err)
		}
	}
	return nil
}

// fail records the first fatal problem of a run and returns it.
func (e *engine) fail(err error) error {
	if err == nil {
		return nil
	}
	if e.fatal == nil {
		e.fatal = err
		e.run.Message = err.Error()
	}
	return err
}

// applyDirections translates the requested direction into the two allowed data
// flows, which is what makes intra-provider and bidirectional jobs share the
// same code path. An unknown direction is treated as a one-way copy.
func (e *engine) applyDirections() {
	switch models.SyncDirection(e.job.Direction) {
	case models.DirectionMicrosoftToGoogle:
		e.forward, e.backward = false, true
	case models.DirectionBidirectional:
		e.forward, e.backward = true, true
	default:
		e.forward, e.backward = true, false
	}
}

// loadExcludes decodes the pattern list of the job. A malformed list is ignored
// (with a warning) instead of failing the run.
func (e *engine) loadExcludes() {
	raw := strings.TrimSpace(e.job.ExcludePatterns)
	if raw == "" {
		return
	}
	var patterns []string
	if err := json.Unmarshal([]byte(raw), &patterns); err != nil {
		slog.Warn("ignoring the malformed exclude patterns of a job",
			"job", e.job.ID, "error", err, "value", raw)
		return
	}
	for _, pattern := range patterns {
		if trimmed := strings.TrimSpace(pattern); trimmed != "" {
			e.excludes = append(e.excludes, trimmed)
		}
	}
}

// resolveEndpoint loads one side of the job with a valid access token.
func (e *engine) resolveEndpoint(ctx context.Context, accountID uint, driveID, folderID string) (*endpoint, error) {
	session, err := e.service.tokens.Resolve(ctx, accountID)
	if err != nil {
		return nil, err
	}
	root := strings.TrimSpace(folderID)
	if root == "" {
		root = providers.DriveRoot
	}
	drive := strings.TrimSpace(driveID)
	if drive == "" {
		drive = providers.DriveRoot
	}
	return &endpoint{
		account:  session.Account,
		provider: session.Provider,
		creds:    session.Credentials,
		tokens:   session.Tokens,
		drive:    drive,
		root:     root,
		files:    map[string]providers.Item{},
		folders:  map[string]string{"": root},
	}, nil
}

// ---------------------------------------------------------------------------
// Tree listing
// ---------------------------------------------------------------------------

// folderRef is one folder waiting to be listed.
type folderRef struct {
	id   string
	path string
}

// walk lists every file below the root of a side and records the folders it
// visited, so the engine can create a missing destination folder without an
// extra listing round trip.
//
// Files matching an exclude pattern are counted as skipped and never listed
// again; Google native documents and shortcuts are collected separately.
func (e *engine) walk(ctx context.Context, side *endpoint) error {
	queue := []folderRef{{id: side.root, path: ""}}
	seen := map[string]bool{}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("the run was cancelled while listing %s: %w", side.label(), err)
		}
		current := queue[0]
		queue = queue[1:]
		if seen[current.id] {
			continue
		}
		seen[current.id] = true

		children, err := side.provider.Children(ctx, side.creds, side.tokens, side.drive, current.id)
		if err != nil {
			if side.provider.IsNotFound(err) {
				// The folder was removed between two listings: nothing to do.
				slog.Debug("folder vanished during the walk", "provider", side.label(), "path", current.path)
				continue
			}
			return fmt.Errorf("listing %s %q failed: %w", side.label(), displayPath(current.path), err)
		}
		for _, child := range children {
			relative := joinPath(current.path, child.Name)
			if e.excluded(relative) {
				e.run.FilesSkipped++
				continue
			}
			if child.Dir() {
				side.folders[relative] = child.ID
				queue = append(queue, folderRef{id: child.ID, path: relative})
				continue
			}
			if child.NativeDoc || child.Shortcut {
				child.Path = relative
				side.unsupported = append(side.unsupported, child)
				continue
			}
			child.Path = relative
			side.files[relative] = child
		}
	}
	slog.Debug("listed a tree",
		"provider", side.label(), "files", len(side.files), "folders", len(side.folders))
	return nil
}

// recordUnsupported writes one run item per item the engine cannot transfer, so
// the UI can explain why a document was left alone.
func (e *engine) recordUnsupported(side *endpoint) {
	for _, item := range side.unsupported {
		evidence := "skipped: " + side.label() + " does not expose a file body for this item"
		if item.Shortcut {
			evidence = "skipped: shortcuts are not followed in this version"
		}
		e.run.FilesSkipped++
		e.record(models.ActionUnsupported, item.Path, item.Size, evidence)
	}
}

// excluded reports whether a relative path matches one of the job patterns.
// Patterns follow the shell syntax: `*.tmp` matches at any depth, `cache/**`
// matches a whole subtree and `docs/*.bak` a single level.
func (e *engine) excluded(relative string) bool {
	for _, pattern := range e.excludes {
		if matchPattern(pattern, relative) {
			return true
		}
	}
	return false
}

// matchPattern applies one exclude pattern to a relative path.
func matchPattern(pattern, relative string) bool {
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		if relative == prefix || strings.HasPrefix(relative, prefix+"/") {
			return true
		}
	}
	if strings.HasPrefix(pattern, "**/") {
		rest := strings.TrimPrefix(pattern, "**/")
		if ok, _ := path.Match(rest, path.Base(relative)); ok {
			return true
		}
	}
	if ok, _ := path.Match(pattern, relative); ok {
		return true
	}
	if strings.Contains(pattern, "/") {
		return false
	}
	ok, _ := path.Match(pattern, path.Base(relative))
	return ok
}

// joinPath appends a child name to the relative path of its parent folder.
func joinPath(parent, name string) string {
	clean := strings.Trim(strings.TrimSpace(name), "/")
	if parent == "" {
		return clean
	}
	return parent + "/" + clean
}

// dirOf returns the relative folder of a path ("" for a file in the root).
func dirOf(relative string) string {
	index := strings.LastIndex(relative, "/")
	if index < 0 {
		return ""
	}
	return relative[:index]
}

// displayPath renders the root selection of a folder in log lines.
func displayPath(relative string) string {
	if relative == "" {
		return "(root)"
	}
	return relative
}

// ---------------------------------------------------------------------------
// Reconciliation
// ---------------------------------------------------------------------------

// reconcilePaths walks every path the plan must consider — the files of the
// source, the files the state table knows about (deletions) and, for jobs that
// copy back, the files of the destination — in a deterministic order.
func (e *engine) reconcilePaths(ctx context.Context) error {
	paths := map[string]struct{}{}
	for relative := range e.source.files {
		paths[relative] = struct{}{}
	}
	for relative := range e.state {
		paths[relative] = struct{}{}
	}
	if e.backward {
		for relative := range e.target.files {
			paths[relative] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(paths))
	for relative := range paths {
		ordered = append(ordered, relative)
	}
	sort.Strings(ordered)

	for _, relative := range ordered {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("the run was cancelled: %w", err)
		}
		e.run.FilesScanned++
		e.one(ctx, relative)
	}
	return nil
}

// one decides what to do with a single relative path and applies the decision.
func (e *engine) one(ctx context.Context, relative string) {
	src, hasSource := e.source.files[relative]
	dst, hasTarget := e.target.files[relative]
	if !hasSource && !hasTarget {
		// Both copies are gone: the state row has nothing left to describe.
		if err := e.service.store.DeleteFileState(ctx, e.job.ID, relative); err != nil {
			slog.Warn("forgetting a synchronised file failed", "job", e.job.ID, "path", relative, "error", err)
		}
		delete(e.state, relative)
		return
	}
	state := e.state[relative]
	var srcPtr, dstPtr *providers.Item
	if hasSource {
		item := src
		srcPtr = &item
	}
	if hasTarget {
		item := dst
		dstPtr = &item
	}

	plan := e.decide(relative, srcPtr, dstPtr, state)
	if plan.conflict {
		e.run.Conflicts++
	}
	switch plan.action {
	case models.ActionInSync:
		e.run.FilesSkipped++
		return
	case models.ActionSkipped, models.ActionConflict:
		e.run.FilesSkipped++
		e.record(plan.action, relative, plan.size, plan.evidence)
		return
	}

	e.record(plan.action, relative, plan.size, plan.evidence)
	if err := e.apply(ctx, relative, plan, srcPtr, dstPtr); err != nil {
		e.run.Errors++
		e.record(models.ActionFailed, relative, plan.size, err.Error())
		slog.Warn("synchronising a file failed",
			"job", e.job.ID, "path", relative, "action", plan.action, "error", err)
	}
}

// decision is what the engine must do with one relative path.
type decision struct {
	action            models.ItemAction
	upload            bool
	download          bool
	deleteSource      bool
	deleteDestination bool
	conflict          bool
	size              int64
	evidence          string
}

// decide compares both sides of a path with the state of the last successful
// synchronisation and returns the action to apply.
func (e *engine) decide(relative string, src, dst *providers.Item, state models.SyncFile) decision {
	tracked := state.ID != 0
	srcChanged := src != nil && itemChanged(*src, state.SourceItemID, state.SourceModifiedAt, state.Size)
	dstChanged := dst != nil && itemChanged(*dst, state.DestinationItemID, state.DestinationModifiedAt, state.Size)

	switch {
	case src != nil && dst == nil:
		return e.decideMissingTarget(*src, state, tracked, srcChanged)
	case src == nil && dst != nil:
		return e.decideMissingSource(*dst, state, tracked, dstChanged)
	}
	return e.decideBoth(src, dst, state, tracked, srcChanged, dstChanged)
}

// decideMissingTarget handles a path that exists on the source only.
func (e *engine) decideMissingTarget(src providers.Item, state models.SyncFile, tracked, srcChanged bool) decision {
	if !tracked {
		if !e.forward {
			return decision{action: models.ActionSkipped, size: src.Size,
				evidence: "present only on the source, not part of this one-way job"}
		}
		return decision{action: models.ActionCreated, upload: true, size: src.Size,
			evidence: "new file on the source"}
	}
	// The destination copy of a tracked file disappeared.
	if e.job.DeleteMissing && !srcChanged && e.backward {
		return decision{action: models.ActionDeleted, deleteSource: true, size: state.Size,
			evidence: "the destination copy was removed"}
	}
	if e.forward {
		return decision{action: models.ActionCreated, upload: true, size: src.Size,
			evidence: "the destination copy is missing and was recreated"}
	}
	return decision{action: models.ActionSkipped, size: src.Size,
		evidence: "the destination copy was removed; enable delete missing to propagate it"}
}

// decideMissingSource handles a path that exists on the destination only.
func (e *engine) decideMissingSource(dst providers.Item, state models.SyncFile, tracked, dstChanged bool) decision {
	if !tracked {
		if !e.backward {
			return decision{action: models.ActionInSync,
				evidence: "present only on the destination, not part of this one-way job"}
		}
		return decision{action: models.ActionCreated, download: true, size: dst.Size,
			evidence: "new file on the destination"}
	}
	// The source copy of a tracked file disappeared.
	if !dstChanged {
		if e.job.DeleteMissing && e.forward {
			return decision{action: models.ActionDeleted, deleteDestination: true, size: state.Size,
				evidence: "the source copy was removed"}
		}
		if e.backward {
			return decision{action: models.ActionCreated, download: true, size: dst.Size,
				evidence: "the source copy was removed and was restored from the destination"}
		}
		return decision{action: models.ActionSkipped, size: state.Size,
			evidence: "the source copy was removed; enable delete missing to propagate it"}
	}
	return decision{action: models.ActionConflict, conflict: true, size: dst.Size,
		evidence: "the source copy was removed after the destination was edited"}
}

// decideBoth handles a path present on both sides: unchanged copies rest, a
// single changed copy propagates and two changed copies follow the conflict
// policy.
func (e *engine) decideBoth(src, dst *providers.Item, state models.SyncFile, tracked, srcChanged, dstChanged bool) decision {
	switch {
	case !srcChanged && !dstChanged:
		if !tracked && src.Size != dst.Size {
			// The files were copied outside Sync and disagree: the policy
			// resolves it, and the evidence says why.
			return e.resolveConflict(src, dst, "both copies exist and only one can be kept")
		}
		return decision{action: models.ActionInSync, evidence: "both copies are identical"}
	case srcChanged && !dstChanged:
		if e.forward {
			return decision{action: models.ActionUpdated, upload: true, size: src.Size,
				evidence: "the source changed since the last successful synchronisation"}
		}
		return decision{action: models.ActionInSync, evidence: "the source changed but this job is one-way"}
	case !srcChanged && dstChanged:
		if e.backward {
			return decision{action: models.ActionUpdated, download: true, size: dst.Size,
				evidence: "the destination changed since the last successful synchronisation"}
		}
		return decision{action: models.ActionInSync, evidence: "the destination changed but this job is one-way"}
	}
	// Both copies changed: they are identical only when hashes prove it.
	if src.Size == dst.Size && src.Hash != "" && dst.Hash != "" && src.Hash == dst.Hash {
		return decision{action: models.ActionInSync, evidence: "both copies changed to the same content"}
	}
	return e.resolveConflict(src, dst, "both copies changed since the last successful synchronisation")
}

// resolveConflict applies the configured conflict policy to a path that cannot
// be reconciled automatically. A policy that cannot be expressed for the
// direction of the job degrades to keeping both copies.
func (e *engine) resolveConflict(src, dst *providers.Item, reason string) decision {
	size := src.Size
	if dst.Size > size {
		size = dst.Size
	}
	sourceWins := decision{action: models.ActionUpdated, upload: true, size: src.Size, conflict: true,
		evidence: reason + "; the source wins"}
	destinationWins := decision{action: models.ActionUpdated, download: true, size: dst.Size, conflict: true,
		evidence: reason + "; the destination wins"}
	skip := decision{action: models.ActionConflict, conflict: true, size: size,
		evidence: reason + "; the policy keeps both copies"}

	switch models.ConflictPolicy(e.job.ConflictPolicy) {
	case models.ConflictSourceWins:
		if e.forward {
			return sourceWins
		}
		return skip
	case models.ConflictDestinationWins:
		if e.backward {
			return destinationWins
		}
		return skip
	case models.ConflictSkip:
		return skip
	}
	// newest_wins is the default: the more recent copy is copied over the other.
	tie := decision{action: models.ActionConflict, conflict: true, size: size,
		evidence: reason + "; identical timestamps keep both copies"}
	if src.ModifiedAt.IsZero() || dst.ModifiedAt.IsZero() || src.ModifiedAt.Equal(dst.ModifiedAt) {
		return tie
	}
	if src.ModifiedAt.After(dst.ModifiedAt) {
		if e.forward {
			return decision{action: models.ActionUpdated, upload: true, size: src.Size, conflict: true,
				evidence: reason + "; the source is newer by " + src.ModifiedAt.Sub(dst.ModifiedAt).Round(time.Second).String()}
		}
		return tie
	}
	if e.backward {
		return decision{action: models.ActionUpdated, download: true, size: dst.Size, conflict: true,
			evidence: reason + "; the destination is newer by " + dst.ModifiedAt.Sub(src.ModifiedAt).Round(time.Second).String()}
	}
	return tie
}

// itemChanged reports whether a remote item differs from the state row of the
// last successful synchronisation (renewed id, different size or a modification
// time outside the tolerance).
func itemChanged(item providers.Item, id string, modified time.Time, size int64) bool {
	if id != "" && item.ID != id {
		return true
	}
	if size > 0 && item.Size != size {
		return true
	}
	if !modified.IsZero() && !item.ModifiedAt.IsZero() {
		delta := item.ModifiedAt.Sub(modified)
		if delta < 0 {
			delta = -delta
		}
		if delta > syncTimestampTolerance {
			return true
		}
	}
	return false
}

// syncTimestampTolerance absorbs the rounding both providers apply to the
// modification time they report.
const syncTimestampTolerance = 2 * time.Second

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

// apply executes a decision. A copy always streams the body of the losing side
// into the winner, so nothing is buffered in memory: a 4 GB video costs a
// bounded amount of RAM.
func (e *engine) apply(ctx context.Context, relative string, plan decision, src, dst *providers.Item) error {
	switch {
	case plan.upload:
		return e.copy(ctx, relative, e.source, e.target, src, dst)
	case plan.download:
		return e.copy(ctx, relative, e.target, e.source, dst, src)
	case plan.deleteDestination:
		return e.remove(ctx, relative, e.target, dst)
	case plan.deleteSource:
		return e.remove(ctx, relative, e.source, src)
	}
	return nil
}

// copy transfers one file from one side to the other. `item` is the copy being
// read, `existing` the copy being overwritten (nil when the file is new).
func (e *engine) copy(ctx context.Context, relative string, from, to *endpoint, item, existing *providers.Item) error {
	if item == nil || item.ID == "" {
		return fmt.Errorf("the file %q has no remote id on %s", relative, from.label())
	}
	parentID, err := e.ensureFolder(ctx, to, dirOf(relative))
	if err != nil {
		return err
	}
	transfer, err := from.provider.Download(ctx, from.creds, from.tokens, from.drive, item.ID)
	if err != nil {
		if from.provider.IsNotFound(err) {
			// The file vanished between the listing and the transfer; the next
			// run reconciles it.
			return fmt.Errorf("the file %q disappeared from %s", relative, from.label())
		}
		return fmt.Errorf("downloading %q from %s: %w", relative, from.label(), err)
	}
	defer func() { _ = transfer.Body.Close() }()

	modified := item.ModifiedAt
	if modified.IsZero() {
		modified = transfer.ModifiedAt
	}
	mime := item.MimeType
	if mime == "" {
		mime = transfer.MimeType
	}
	existingID := ""
	if existing != nil {
		existingID = existing.ID
	}
	saved, err := to.provider.Upload(ctx, to.creds, to.tokens, providers.UploadRequest{
		DriveID:    to.drive,
		ParentID:   parentID,
		ExistingID: existingID,
		Name:       path.Base(relative),
		Size:       item.Size,
		MimeType:   mime,
		ModifiedAt: modified,
		Body:       transfer.Body,
	})
	if err != nil {
		return fmt.Errorf("uploading %q to %s: %w", relative, to.label(), err)
	}
	e.run.BytesTransferred += item.Size
	if _, tracked := e.state[relative]; tracked {
		e.run.FilesUpdated++
	} else {
		e.run.FilesCreated++
	}
	direction := "source_to_destination"
	if from != e.source {
		direction = "destination_to_source"
	}
	return e.remember(ctx, relative, from, item, saved, direction)
}

// remove deletes one remote file and forgets its state row.
func (e *engine) remove(ctx context.Context, relative string, side *endpoint, item *providers.Item) error {
	if item == nil || item.ID == "" {
		return e.forget(ctx, relative)
	}
	if err := side.provider.Delete(ctx, side.creds, side.tokens, side.drive, item.ID); err != nil {
		if !side.provider.IsNotFound(err) {
			return fmt.Errorf("deleting %q on %s: %w", relative, side.label(), err)
		}
	}
	e.run.FilesDeleted++
	delete(side.files, relative)
	return e.forget(ctx, relative)
}

// forget drops the state row of a path from the database and the in-memory map.
func (e *engine) forget(ctx context.Context, relative string) error {
	if err := e.service.store.DeleteFileState(ctx, e.job.ID, relative); err != nil {
		return err
	}
	delete(e.state, relative)
	return nil
}

// remember writes the state row of a file that is now in sync on both sides and
// keeps the in-memory tree of the destination up to date, so a later path can
// find the folder or file this run just created.
func (e *engine) remember(ctx context.Context, relative string, from *endpoint, item, saved *providers.Item, direction string) error {
	hash := item.Hash
	if hash == "" && saved != nil {
		hash = saved.Hash
	}
	row := models.SyncFile{
		JobID:         e.job.ID,
		Path:          relative,
		Size:          item.Size,
		Hash:          hash,
		LastSyncedAt:  e.service.now(),
		LastDirection: direction,
		Status:        string(models.RunSuccess),
	}
	target := e.target
	if from != e.source {
		target = e.source
	}
	if saved == nil {
		saved = item
	}
	if from == e.source {
		row.SourceItemID, row.SourceModifiedAt = item.ID, item.ModifiedAt
		row.DestinationItemID, row.DestinationModifiedAt = saved.ID, saved.ModifiedAt
	} else {
		row.SourceItemID, row.SourceModifiedAt = saved.ID, saved.ModifiedAt
		row.DestinationItemID, row.DestinationModifiedAt = item.ID, item.ModifiedAt
	}
	if err := e.service.store.SaveFileState(ctx, &row); err != nil {
		return err
	}
	e.state[relative] = row
	copied := *saved
	copied.Path = relative
	target.files[relative] = copied
	return nil
}

// ensureFolder returns the id of the folder holding `relative`, creating the
// missing ancestors one level at a time (providers only create one folder per
// call) and remembering them so a second file in the same folder is free.
func (e *engine) ensureFolder(ctx context.Context, side *endpoint, dir string) (string, error) {
	if dir == "" {
		return side.root, nil
	}
	if id, ok := side.folders[dir]; ok && id != "" {
		return id, nil
	}
	parentID, err := e.ensureFolder(ctx, side, dirOf(dir))
	if err != nil {
		return "", err
	}
	folder, err := side.provider.CreateFolder(ctx, side.creds, side.tokens, side.drive, parentID, path.Base(dir))
	if err != nil {
		return "", fmt.Errorf("creating the folder %q on %s: %w", dir, side.label(), err)
	}
	side.folders[dir] = folder.ID
	e.run.FoldersCreated++
	e.record(models.ActionFolderCreated, dir, 0, "created the folder on "+side.label())
	return folder.ID, nil
}

// record appends one run item to the batch written when the run ends.
func (e *engine) record(action models.ItemAction, relative string, size int64, evidence string) {
	e.items = append(e.items, models.SyncItem{
		RunID:     e.run.ID,
		JobID:     e.job.ID,
		Action:    string(action),
		Path:      relative,
		Size:      size,
		Evidence:  evidence,
		CreatedAt: e.service.now(),
	})
}

// status derives the final status of the run from its counters.
func (e *engine) status(ctx context.Context) models.RunStatus {
	switch {
	case ctx.Err() != nil, errors.Is(e.fatal, context.Canceled), errors.Is(e.fatal, context.DeadlineExceeded):
		if e.run.Message == "" {
			e.run.Message = "the run was cancelled"
		}
		return models.RunCancelled
	case e.fatal != nil:
		return models.RunFailed
	case e.run.Errors > 0:
		return models.RunPartial
	default:
		return models.RunSuccess
	}
}
