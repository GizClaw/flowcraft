package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

var lifecycleOperationCounter atomic.Uint64
var traceCounter atomic.Uint64

const (
	defaultLifecycleWorkerID = "memory-system-worker"
	defaultLifecycleJobLease = 30 * time.Second
)

type lifecycleJobSnapshotStore interface {
	Get(context.Context, LifecycleJobID) (LifecycleJob, bool, error)
}

// ReadinessReport summarizes whether the configured facade can serve its plan.
type ReadinessReport struct {
	Ready  bool
	Checks []ReadinessCheck
}

// ReadinessCheck is one structured dependency or control-plane check.
type ReadinessCheck struct {
	Name    string
	Ready   bool
	Message string
}

// RebuildRequest describes a requested derived-view rebuild scope.
type RebuildRequest struct {
	TraceID        TraceID
	Scope          Scope
	Capabilities   []Capability
	Documents      []DocumentTarget
	DryRun         bool
	Reason         string
	IdempotencyKey string
}

// ReconcileRequest describes a requested reconciliation scope.
type ReconcileRequest struct {
	TraceID        TraceID
	Scope          Scope
	Capabilities   []Capability
	DryRun         bool
	Reason         string
	IdempotencyKey string
}

// ReloadRequest describes a requested lifecycle reload scope.
type ReloadRequest struct {
	TraceID        TraceID
	Scope          Scope
	Capabilities   []Capability
	Documents      []DocumentTarget
	DryRun         bool
	Reason         string
	IdempotencyKey string
}

// FreshnessRequest describes a requested lifecycle freshness check scope.
type FreshnessRequest struct {
	TraceID        TraceID
	Scope          Scope
	Capabilities   []Capability
	Documents      []DocumentTarget
	DryRun         bool
	Reason         string
	IdempotencyKey string
}

// FreshnessResult reports whether freshness checking was handled by this facade
// and includes the bounded diagnostics checks produced for the operation.
type FreshnessResult struct {
	LifecycleExecutionReport
	Ready       bool
	OK          bool
	Checks      []DiagnosticCheck
	Warnings    []string
	Diagnostics DiagnosticReport
}

// OperationID identifies one requested lifecycle operation.
type OperationID string

// TraceID correlates related memory requests, jobs, diagnostics, and reports.
type TraceID string

// LifecycleJobID identifies job-store-visible memory work.
type LifecycleJobID string

// LifecycleRunID identifies an in-process lifecycle runner attempt.
type LifecycleRunID string

// LifecycleAction is the supported lifecycle action contract.
type LifecycleAction string

const (
	LifecycleActionRebuild        LifecycleAction = lifecycleStageRebuild
	LifecycleActionReconcile      LifecycleAction = lifecycleStageReconcile
	LifecycleActionReload         LifecycleAction = lifecycleStageReload
	LifecycleActionFreshnessCheck LifecycleAction = lifecycleStageFreshnessCheck
)

// LifecycleExecutionStatus is the machine-readable operation outcome.
type LifecycleExecutionStatus string

const (
	LifecycleStatusPlanned     LifecycleExecutionStatus = "planned"
	LifecycleStatusEnqueued    LifecycleExecutionStatus = "enqueued"
	LifecycleStatusCompleted   LifecycleExecutionStatus = "completed"
	LifecycleStatusSkipped     LifecycleExecutionStatus = "skipped"
	LifecycleStatusFailed      LifecycleExecutionStatus = "failed"
	LifecycleStatusCancelled   LifecycleExecutionStatus = "cancelled"
	LifecycleStatusRejected    LifecycleExecutionStatus = "rejected"
	LifecycleStatusUnsupported LifecycleExecutionStatus = "unsupported"
)

// LifecycleTarget names one explicit operation target. Document targets are the
// only target kind implemented in Phase 1.
type LifecycleTarget struct {
	Kind       string
	Capability Capability
	DatasetID  string
	DocumentID string
}

// LifecycleOperation is the normalized production lifecycle request.
type LifecycleOperation struct {
	ID             OperationID
	TraceID        TraceID
	Action         LifecycleAction
	Scope          Scope
	Capabilities   []Capability
	Documents      []DocumentTarget
	Targets        []LifecycleTarget
	Reason         string
	DryRun         bool
	RequestedAt    time.Time
	PlanDigest     string
	IdempotencyKey string
}

// LifecycleExecutionReport is the public report for one lifecycle operation.
type LifecycleExecutionReport struct {
	TraceID      TraceID
	Operation    LifecycleOperation
	Accepted     bool
	Supported    bool
	Status       LifecycleExecutionStatus
	JobID        LifecycleJobID
	RunID        LifecycleRunID
	Summary      string
	Message      string
	Steps        []LifecycleStep
	Counts       LifecycleCounts
	TargetErrors []LifecycleTargetError
	Checkpoint   map[string]any
	StartedAt    time.Time
	CompletedAt  time.Time
	Duration     time.Duration
}

// LifecycleCounts summarizes step and target outcomes in an execution report.
type LifecycleCounts struct {
	Planned   int
	Completed int
	Skipped   int
	Failed    int
	Targets   int
}

// LifecycleTargetError reports one target-level lifecycle failure.
type LifecycleTargetError struct {
	Target  LifecycleTarget
	Message string
	Error   string
}

// LifecycleStep reports one planned or completed lifecycle operation step.
type LifecycleStep struct {
	Name      string
	Status    LifecycleExecutionStatus
	Planned   bool
	Completed bool
	Skipped   bool
	Message   string
	Details   map[string]any
}

// Readiness returns structured dependency checks for the compiled plan.
func (r *System) Readiness(_ context.Context) (ReadinessReport, error) {
	report := ReadinessReport{Ready: true}
	add := func(name string, ready bool, message string) {
		report.Checks = append(report.Checks, ReadinessCheck{
			Name:    name,
			Ready:   ready,
			Message: message,
		})
		if !ready {
			report.Ready = false
		}
	}
	if r == nil || r.inner == nil {
		add("system", false, "system is not configured")
		return report, nil
	}

	add("system", true, "system is configured")
	if r.assembly.HasSource(SourceMessageLog) {
		add("source.message_store", r.deps.MessageStore != nil, dependencyMessage("MessageStore", r.deps.MessageStore != nil))
	}
	if r.assembly.HasSource(SourceDocumentStore) {
		add("source.document_store", r.deps.DocumentStore != nil, dependencyMessage("DocumentStore", r.deps.DocumentStore != nil))
	}

	for _, capability := range r.assembly.Capabilities() {
		r.addCapabilityReadiness(&report, capability)
	}
	if len(r.assembly.Projections) > 0 {
		add("retrieval.index", r.deps.Index != nil, dependencyMessage("Index", r.deps.Index != nil))
	}
	if hasAsyncWriteStages(r.plan.Write) {
		add("job_store", r.jobStore != nil, dependencyMessage("JobStore", r.jobStore != nil))
	}
	return report, nil
}

// QueueStats returns durable job counters. Sync-only systems without a job store
// report an empty queue.
func (r *System) QueueStats(ctx context.Context) (QueueStats, error) {
	if r == nil || r.inner == nil {
		return QueueStats{}, errdefs.NotAvailablef("memory: system is not configured")
	}
	if r.jobStore == nil {
		return QueueStats{}, nil
	}
	return r.jobStore.Stats(ctx)
}

// CancelJob marks a queued lifecycle job as cancelled and persists a cancelled
// lifecycle report when a report store is configured.
func (r *System) CancelJob(ctx context.Context, jobID LifecycleJobID, reason string) error {
	if r == nil || r.inner == nil {
		return errdefs.NotAvailablef("memory: system is not configured")
	}
	if r.jobStore == nil {
		return errdefs.NotAvailablef("memory: job store is not configured")
	}
	var (
		job      LifecycleJob
		haveJob  bool
		terminal bool
	)
	if snapshots, ok := r.jobStore.(lifecycleJobSnapshotStore); ok {
		var err error
		job, haveJob, err = snapshots.Get(ctx, jobID)
		if err != nil {
			return err
		}
		terminal = haveJob && isTerminalLifecycleJobStatus(job.Status)
	}
	if err := r.jobStore.Cancel(ctx, jobID, reason); err != nil {
		return err
	}
	if terminal {
		return nil
	}
	if haveJob {
		report := newLifecycleReportForJob(r, job)
		report.Accepted = true
		report.Supported = true
		report.Status = LifecycleStatusCancelled
		report.Message = strings.TrimSpace(reason)
		report.Steps = append(report.Steps, LifecycleStep{
			Name:    "lifecycle_job.cancel",
			Status:  LifecycleStatusCancelled,
			Skipped: true,
			Message: report.Message,
		})
		if report.Message == "" {
			report.Message = "lifecycle job cancelled"
			report.Steps[len(report.Steps)-1].Message = report.Message
		}
		finalizeLifecycleExecutionReport(&report)
		return r.putLifecycleReport(ctx, report)
	}
	report := LifecycleExecutionReport{
		TraceID:   ensureTraceID(""),
		Accepted:  true,
		Supported: true,
		Status:    LifecycleStatusCancelled,
		JobID:     jobID,
		Message:   strings.TrimSpace(reason),
		Steps: []LifecycleStep{{
			Name:    "lifecycle_job.cancel",
			Status:  LifecycleStatusCancelled,
			Skipped: true,
			Message: strings.TrimSpace(reason),
		}},
		StartedAt: time.Now(),
	}
	if report.Message == "" {
		report.Message = "lifecycle job cancelled"
		report.Steps[0].Message = report.Message
	}
	finalizeLifecycleExecutionReport(&report)
	return r.putLifecycleReport(ctx, report)
}

// RunOnce executes one pending async memory job.
func (r *System) RunOnce(ctx context.Context) (LifecycleJobResult, error) {
	if r == nil || r.inner == nil {
		err := errdefs.NotAvailablef("memory: system is not configured")
		return LifecycleJobResult{Error: err.Error()}, err
	}
	if r.jobStore == nil {
		err := errdefs.NotAvailablef("memory: job store is not configured")
		return LifecycleJobResult{Error: err.Error()}, err
	}
	job, ok, err := r.jobStore.Claim(ctx, defaultLifecycleWorkerID, defaultLifecycleJobLease)
	if err != nil {
		return LifecycleJobResult{Error: err.Error()}, err
	}
	if !ok {
		return LifecycleJobResult{}, nil
	}
	result := LifecycleJobResult{
		JobID:       job.ID,
		OperationID: job.OperationID,
		Kind:        job.Kind,
		Checkpoint:  cloneCheckpoint(job.Checkpoint),
	}
	runner, ok := r.lifecycleRunner(job.Kind)
	if !ok {
		err := errdefs.NotAvailablef("memory: no lifecycle runner registered for job kind %q", job.Kind)
		report := newLifecycleReportForJob(r, job)
		failLifecycleReport(&report, err)
		result.Error = err.Error()
		failErr := r.jobStore.Fail(ctx, job.ID, defaultLifecycleWorkerID, err, job.Checkpoint)
		storeErr := r.putLifecycleReport(ctx, report)
		if failErr != nil {
			return result, errors.Join(err, failErr, storeErr)
		}
		if storeErr != nil {
			return result, errors.Join(err, storeErr)
		}
		return result, err
	}
	report, err := runner.Run(ctx, LifecycleRunRequest{
		Job:        job,
		WorkerID:   defaultLifecycleWorkerID,
		Attempt:    job.Attempt,
		Checkpoint: cloneCheckpoint(job.Checkpoint),
		System:     r,
	})
	report = normalizeLifecycleReportForJob(r, job, report)
	if report.Checkpoint != nil {
		result.Checkpoint = cloneCheckpoint(report.Checkpoint)
	}
	if err != nil {
		result.Error = err.Error()
		if report.Status == "" || report.Status == LifecycleStatusPlanned {
			failLifecycleReport(&report, err)
		} else if report.CompletedAt.IsZero() {
			finalizeLifecycleExecutionReport(&report)
		}
		failErr := r.jobStore.Fail(ctx, job.ID, defaultLifecycleWorkerID, err, result.Checkpoint)
		storeErr := r.putLifecycleReport(ctx, report)
		if failErr != nil {
			return result, errors.Join(err, failErr, storeErr)
		}
		if storeErr != nil {
			return result, errors.Join(err, storeErr)
		}
		return result, err
	}
	result.Completed = true
	if err := r.jobStore.Complete(ctx, job.ID, defaultLifecycleWorkerID, result); err != nil {
		result.Completed = false
		result.Error = err.Error()
		return result, err
	}
	if report.CompletedAt.IsZero() {
		finalizeLifecycleExecutionReport(&report)
	}
	if err := r.putLifecycleReport(ctx, report); err != nil {
		result.Completed = false
		result.Error = err.Error()
		return result, err
	}
	return result, nil
}

// Drain executes pending async memory jobs until the queue is empty.
func (r *System) Drain(ctx context.Context) error {
	if r == nil || r.inner == nil {
		return errdefs.NotAvailablef("memory: system is not configured")
	}
	if r.jobStore == nil {
		return nil
	}
	for {
		result, err := r.RunOnce(ctx)
		if err != nil {
			return err
		}
		if result.JobID == "" {
			return nil
		}
	}
}

// Shutdown stops job enqueueing and releases resources owned by the system.
func (r *System) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	var err error
	if r.jobStore != nil {
		err = errors.Join(err, r.jobStore.Shutdown(ctx))
	}
	if r.inner != nil {
		err = errors.Join(err, r.inner.Close())
	}
	return err
}

// Rebuild plans or enqueues derived-view rebuild substrate work.
func (r *System) Rebuild(ctx context.Context, req RebuildRequest) (LifecycleExecutionReport, error) {
	return r.dispatchLifecycle(ctx, LifecycleActionRebuild, lifecycleDispatchRequest{
		TraceID:        req.TraceID,
		Scope:          req.Scope,
		Capabilities:   req.Capabilities,
		Documents:      req.Documents,
		DryRun:         req.DryRun,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
	})
}

// Reconcile plans or enqueues cross-view reconciliation substrate work.
func (r *System) Reconcile(ctx context.Context, req ReconcileRequest) (LifecycleExecutionReport, error) {
	return r.dispatchLifecycle(ctx, LifecycleActionReconcile, lifecycleDispatchRequest{
		TraceID:        req.TraceID,
		Scope:          req.Scope,
		Capabilities:   req.Capabilities,
		DryRun:         req.DryRun,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
	})
}

// Reload plans or enqueues lifecycle reload substrate work.
func (r *System) Reload(ctx context.Context, req ReloadRequest) (LifecycleExecutionReport, error) {
	return r.dispatchLifecycle(ctx, LifecycleActionReload, lifecycleDispatchRequest{
		TraceID:        req.TraceID,
		Scope:          req.Scope,
		Capabilities:   req.Capabilities,
		Documents:      req.Documents,
		DryRun:         req.DryRun,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
	})
}

// Freshness plans or enqueues lifecycle freshness-check substrate work.
func (r *System) Freshness(ctx context.Context, req FreshnessRequest) (FreshnessResult, error) {
	report, err := r.dispatchLifecycle(ctx, LifecycleActionFreshnessCheck, lifecycleDispatchRequest{
		TraceID:        req.TraceID,
		Scope:          req.Scope,
		Capabilities:   req.Capabilities,
		Documents:      req.Documents,
		DryRun:         req.DryRun,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
	})
	freshness := FreshnessResult{LifecycleExecutionReport: report}
	if err != nil {
		return freshness, err
	}
	diagnostics, err := r.runDiagnosticProbes(ctx, DiagnosticRequest{
		TraceID:      report.TraceID,
		Scope:        report.Operation.Scope,
		Capabilities: report.Operation.Capabilities,
		Documents:    report.Operation.Documents,
		Stage:        diagnosticStageFreshness,
	}, false)
	if err != nil {
		return freshness, err
	}
	freshness.Ready = diagnostics.Ready
	freshness.OK = diagnostics.OK
	freshness.Checks = cloneDiagnosticChecks(diagnostics.Checks)
	freshness.Warnings = append([]string(nil), diagnostics.Warnings...)
	freshness.Diagnostics = diagnostics
	return freshness, nil
}

type lifecycleDispatchRequest struct {
	TraceID        TraceID
	Scope          Scope
	Capabilities   []Capability
	Documents      []DocumentTarget
	DryRun         bool
	Reason         string
	IdempotencyKey string
}

func (r *System) dispatchLifecycle(ctx context.Context, action LifecycleAction, req lifecycleDispatchRequest) (report LifecycleExecutionReport, err error) {
	report = newLifecycleExecutionReport(action, req)
	defer func() {
		finalizeLifecycleExecutionReport(&report)
		if storeErr := r.putLifecycleReport(ctx, report); storeErr != nil {
			err = errors.Join(err, storeErr)
		}
	}()

	if r == nil || r.inner == nil {
		report.Status = LifecycleStatusRejected
		report.Message = "system is not configured"
		return report, errdefs.NotAvailablef("memory: system is not configured")
	}

	scope := normalizeScope(req.Scope)
	report.Operation.Scope = scope
	report.Operation.PlanDigest = r.lifecyclePlanDigest()
	if err := scope.Validate(); err != nil {
		report.Status = LifecycleStatusRejected
		report.Message = "invalid scope"
		return report, errdefs.Validationf("memory: invalid scope: %w", err)
	}
	documents, err := normalizeDocumentTargets(scope, req.Documents)
	if err != nil {
		report.Status = LifecycleStatusRejected
		report.Message = "invalid document targets"
		return report, err
	}
	report.Operation.Documents = documents
	report.Operation.Targets = lifecycleTargetsForDocuments(documents)
	report.Operation.IdempotencyKey = lifecycleIdempotencyKey(report.Operation, req.IdempotencyKey)
	if !r.lifecycleActionDeclared(action) {
		report.Status = LifecycleStatusUnsupported
		report.Message = fmt.Sprintf("lifecycle action %q is not declared by the plan", action)
		return report, errdefs.NotAvailablef("memory: lifecycle action %q is not declared by the plan", action)
	}

	report.Supported = true
	if handled, err := r.dispatchDocumentChunkLifecycle(ctx, action, &report); handled || err != nil {
		return report, err
	}
	if req.DryRun {
		report.Accepted = true
		report.Status = LifecycleStatusPlanned
		report.Message = fmt.Sprintf("%s lifecycle operation planned; no runner registered yet", action)
		return report, nil
	}

	jobID := lifecycleJobIDForOperation(report.Operation.ID)
	job := LifecycleJob{
		ID:           jobID,
		TraceID:      report.TraceID,
		OperationID:  report.Operation.ID,
		Kind:         LifecycleJobKind(action),
		Scope:        scope,
		Capabilities: cloneCapabilities(report.Operation.Capabilities),
		Documents:    cloneDocumentTargets(report.Operation.Documents),
		Reason:       report.Operation.Reason,
		Stages:       []PlannedStage{{Name: string(action)}},
		MaxAttempts:  1,
	}
	if _, ok := r.lifecycleRunner(job.Kind); !ok {
		report.Status = LifecycleStatusUnsupported
		report.Message = fmt.Sprintf("no lifecycle runner registered for action %q and requested capabilities", action)
		return report, errdefs.NotAvailablef("memory: no lifecycle runner registered for action %q", action)
	}
	if r.jobStore == nil {
		report.Status = LifecycleStatusFailed
		report.Message = "job store is not configured"
		return report, errdefs.NotAvailablef("memory: job store is not configured for lifecycle action %q", action)
	}
	enqueuedID, err := r.jobStore.Enqueue(ctx, job)
	if err != nil {
		report.Status = LifecycleStatusFailed
		report.Message = err.Error()
		return report, err
	}
	report.Accepted = true
	report.Status = LifecycleStatusEnqueued
	report.JobID = enqueuedID
	report.Message = fmt.Sprintf("%s lifecycle job enqueued", action)
	return report, nil
}

func newLifecycleExecutionReport(action LifecycleAction, req lifecycleDispatchRequest) LifecycleExecutionReport {
	now := time.Now()
	traceID := ensureTraceID(req.TraceID)
	operation := LifecycleOperation{
		ID:           newOperationID(),
		TraceID:      traceID,
		Action:       action,
		Scope:        normalizeScope(req.Scope),
		Capabilities: cloneCapabilities(req.Capabilities),
		Documents:    cloneDocumentTargets(req.Documents),
		Reason:       strings.TrimSpace(req.Reason),
		DryRun:       req.DryRun,
		RequestedAt:  now,
	}
	operation.Targets = lifecycleTargetsForDocuments(operation.Documents)
	operation.IdempotencyKey = lifecycleIdempotencyKey(operation, req.IdempotencyKey)
	return LifecycleExecutionReport{
		TraceID:   traceID,
		Operation: operation,
		Status:    LifecycleStatusPlanned,
		StartedAt: now,
		Steps: []LifecycleStep{{
			Name:    "lifecycle_substrate",
			Status:  LifecycleStatusPlanned,
			Planned: true,
			Message: "lifecycle operation validated; no runner registered yet",
		}},
	}
}

func (r *System) dispatchDocumentChunkLifecycle(ctx context.Context, action LifecycleAction, report *LifecycleExecutionReport) (bool, error) {
	if action != LifecycleActionReload && action != LifecycleActionRebuild {
		return false, nil
	}
	if !capabilitySelected(report.Operation.Capabilities, CapabilityDocumentChunks) {
		return false, nil
	}

	report.Steps = nil
	if len(report.Operation.Documents) == 0 {
		report.Accepted = true
		report.Status = LifecycleStatusSkipped
		report.Message = "document_chunks reload/rebuild requires explicit document targets; no full scan was planned"
		report.Steps = append(report.Steps, LifecycleStep{
			Name:    "document_chunks.targets",
			Status:  LifecycleStatusSkipped,
			Planned: true,
			Skipped: true,
			Message: "no document targets supplied; full document scans are intentionally unsupported",
			Details: map[string]any{
				"capability": string(CapabilityDocumentChunks),
			},
		})
		return true, nil
	}

	if !r.writeAvailable[CapabilityDocumentChunks] {
		report.Status = LifecycleStatusUnsupported
		report.Message = "document_chunks capability is not configured for writes"
		report.Steps = append(report.Steps, LifecycleStep{
			Name:    "document_chunks.configure",
			Status:  LifecycleStatusUnsupported,
			Planned: true,
			Message: "DocumentStore, ChunkStore, and DocumentChunker are required for targeted reload/rebuild",
			Details: map[string]any{
				"capability": string(CapabilityDocumentChunks),
			},
		})
		return true, errdefs.NotAvailablef("memory: document_chunks capability is not configured for targeted %s", action)
	}

	if report.Operation.DryRun {
		report.Accepted = true
		report.Status = LifecycleStatusPlanned
		report.Message = fmt.Sprintf("%s planned for %d document target(s)", action, len(report.Operation.Documents))
		for _, target := range report.Operation.Documents {
			report.Steps = append(report.Steps, LifecycleStep{
				Name:    "document_chunks.target",
				Status:  LifecycleStatusPlanned,
				Planned: true,
				Message: fmt.Sprintf("would re-index document %s", documentTargetLabel(target)),
				Details: map[string]any{
					"capability":  string(CapabilityDocumentChunks),
					"dataset_id":  target.DatasetID,
					"document_id": target.DocumentID,
					"chunk_count": 0,
				},
			})
		}
		return true, nil
	}

	for _, target := range report.Operation.Documents {
		report.Steps = append(report.Steps, LifecycleStep{
			Name:    "document_chunks.target",
			Status:  LifecycleStatusPlanned,
			Planned: true,
			Message: fmt.Sprintf("will re-index document %s asynchronously", documentTargetLabel(target)),
			Details: map[string]any{
				"capability":  string(CapabilityDocumentChunks),
				"dataset_id":  target.DatasetID,
				"document_id": target.DocumentID,
			},
		})
	}
	jobID := lifecycleJobIDForOperation(report.Operation.ID)
	job := LifecycleJob{
		ID:           jobID,
		TraceID:      report.TraceID,
		OperationID:  report.Operation.ID,
		Kind:         LifecycleJobKind(action),
		Scope:        report.Operation.Scope,
		Capabilities: cloneCapabilities(report.Operation.Capabilities),
		Documents:    cloneDocumentTargets(report.Operation.Documents),
		Reason:       report.Operation.Reason,
		Stages:       []PlannedStage{{Name: string(action), Capability: CapabilityDocumentChunks}},
		MaxAttempts:  1,
	}
	if _, ok := r.lifecycleRunner(job.Kind); !ok {
		report.Status = LifecycleStatusUnsupported
		report.Message = fmt.Sprintf("no lifecycle runner registered for document_chunks %s", action)
		return true, errdefs.NotAvailablef("memory: no lifecycle runner registered for document_chunks %s", action)
	}
	if r.jobStore == nil {
		report.Status = LifecycleStatusFailed
		report.Message = "job store is not configured"
		return true, errdefs.NotAvailablef("memory: job store is not configured for document_chunks %s", action)
	}
	enqueuedID, err := r.jobStore.Enqueue(ctx, job)
	if err != nil {
		report.Status = LifecycleStatusFailed
		report.Message = err.Error()
		return true, err
	}
	report.Accepted = true
	report.Status = LifecycleStatusEnqueued
	report.JobID = enqueuedID
	report.Message = fmt.Sprintf("%s enqueued for %d document target(s)", action, len(report.Operation.Documents))
	return true, nil
}

func (r *System) lifecycleActionDeclared(action LifecycleAction) bool {
	for _, stage := range r.plan.Lifecycle {
		if stage.Name == string(action) {
			return true
		}
	}
	return false
}

func newOperationID() OperationID {
	seq := lifecycleOperationCounter.Add(1)
	return OperationID("lifecycle-op-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(seq, 36))
}

func ensureTraceID(id TraceID) TraceID {
	if trimmed := strings.TrimSpace(string(id)); trimmed != "" {
		return TraceID(trimmed)
	}
	seq := traceCounter.Add(1)
	return TraceID("trace-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(seq, 36))
}

func lifecycleJobIDForOperation(id OperationID) LifecycleJobID {
	return LifecycleJobID(strings.Replace(string(id), "lifecycle-op-", "lifecycle-job-", 1))
}

func lifecycleRunIDForOperation(id OperationID) LifecycleRunID {
	return LifecycleRunID(strings.Replace(string(id), "lifecycle-op-", "lifecycle-run-", 1))
}

func (r *System) lifecyclePlanDigest() string {
	if r == nil {
		return ""
	}
	var builder strings.Builder
	for _, stage := range r.plan.Lifecycle {
		builder.WriteString(stage.Name)
		builder.WriteByte('|')
		builder.WriteString(strconv.FormatBool(stage.Optional))
		builder.WriteByte('|')
		builder.WriteString(string(stage.Capability))
		builder.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

func lifecycleIdempotencyKey(operation LifecycleOperation, requested string) string {
	if key := strings.TrimSpace(requested); key != "" {
		return key
	}
	var builder strings.Builder
	builder.WriteString(string(operation.Action))
	builder.WriteByte('|')
	builder.WriteString(scopeDigestInput(operation.Scope))
	builder.WriteByte('|')
	for _, capability := range operation.Capabilities {
		builder.WriteString(string(capability))
		builder.WriteByte(',')
	}
	builder.WriteByte('|')
	for _, document := range operation.Documents {
		builder.WriteString(document.DatasetID)
		builder.WriteByte('/')
		builder.WriteString(document.DocumentID)
		builder.WriteByte(',')
	}
	builder.WriteByte('|')
	builder.WriteString(strconv.FormatBool(operation.DryRun))
	builder.WriteByte('|')
	builder.WriteString(operation.Reason)
	builder.WriteByte('|')
	builder.WriteString(operation.PlanDigest)
	sum := sha256.Sum256([]byte(builder.String()))
	return "sha256:" + hex.EncodeToString(sum[:16])
}

func scopeDigestInput(scope Scope) string {
	return strings.Join([]string{
		scope.RuntimeID,
		scope.UserID,
		scope.AgentID,
		scope.ConversationID,
		scope.DatasetID,
		scope.EntityID,
	}, "/")
}

func lifecycleTargetsForDocuments(documents []DocumentTarget) []LifecycleTarget {
	if len(documents) == 0 {
		return nil
	}
	targets := make([]LifecycleTarget, 0, len(documents))
	for _, document := range documents {
		targets = append(targets, lifecycleTargetForDocument(document))
	}
	return targets
}

func lifecycleTargetForDocument(document DocumentTarget) LifecycleTarget {
	return LifecycleTarget{
		Kind:       "document",
		Capability: CapabilityDocumentChunks,
		DatasetID:  document.DatasetID,
		DocumentID: document.DocumentID,
	}
}

func finalizeLifecycleExecutionReport(report *LifecycleExecutionReport) {
	report.CompletedAt = time.Now()
	if report.StartedAt.IsZero() {
		report.StartedAt = report.CompletedAt
	}
	report.Duration = report.CompletedAt.Sub(report.StartedAt)
	report.Counts = countLifecycleSteps(report.Steps, len(report.Operation.Targets))
	if report.Summary == "" {
		report.Summary = report.Message
	}
}

func countLifecycleSteps(steps []LifecycleStep, targets int) LifecycleCounts {
	counts := LifecycleCounts{Targets: targets}
	for _, step := range steps {
		switch step.Status {
		case LifecycleStatusCompleted:
			counts.Completed++
		case LifecycleStatusSkipped:
			counts.Skipped++
		case LifecycleStatusFailed, LifecycleStatusCancelled, LifecycleStatusRejected, LifecycleStatusUnsupported:
			counts.Failed++
		default:
			if step.Completed {
				counts.Completed++
			} else if step.Skipped {
				counts.Skipped++
			} else if step.Planned {
				counts.Planned++
			}
		}
	}
	return counts
}

func cloneCapabilities(in []Capability) []Capability {
	if in == nil {
		return nil
	}
	out := make([]Capability, len(in))
	copy(out, in)
	return out
}

func (r *System) addCapabilityReadiness(report *ReadinessReport, capability Capability) {
	add := func(name string, ready bool, message string) {
		report.Checks = append(report.Checks, ReadinessCheck{
			Name:    name,
			Ready:   ready,
			Message: message,
		})
		if !ready {
			report.Ready = false
		}
	}

	switch capability {
	case CapabilityRecentWindow:
		add("capability.recent_window.message_store", r.deps.MessageStore != nil, dependencyMessage("MessageStore", r.deps.MessageStore != nil))
	case CapabilitySummaryDAG:
		add("capability.summary_dag.store", r.deps.SummaryStore != nil, dependencyMessage("SummaryStore", r.deps.SummaryStore != nil))
		add("capability.summary_dag.service", r.deps.Summarizer != nil, dependencyMessage("Summarizer", r.deps.Summarizer != nil))
	case CapabilityDocumentChunks:
		add("capability.document_chunks.store", r.deps.ChunkStore != nil, dependencyMessage("ChunkStore", r.deps.ChunkStore != nil))
		add("capability.document_chunks.service", r.deps.DocumentChunker != nil, dependencyMessage("DocumentChunker", r.deps.DocumentChunker != nil))
	case CapabilityObservationLedger:
		add("capability.observation_ledger.store", r.deps.ObservationStore != nil, dependencyMessage("ObservationStore", r.deps.ObservationStore != nil))
		add("capability.observation_ledger.service", r.deps.ObservationExtractor != nil, dependencyMessage("ObservationExtractor", r.deps.ObservationExtractor != nil))
	case CapabilityFactLedger:
		add("capability.fact_ledger.store", r.deps.FactStore != nil, dependencyMessage("FactStore", r.deps.FactStore != nil))
		add("capability.fact_ledger.service", r.deps.FactReconciler != nil, dependencyMessage("FactReconciler", r.deps.FactReconciler != nil))
	case CapabilityFactGraph:
		add("capability.fact_graph.store", r.deps.FactGraphStore != nil, dependencyMessage("FactGraphStore", r.deps.FactGraphStore != nil))
		add("capability.fact_graph.service", r.deps.FactGraphBuilder != nil, dependencyMessage("FactGraphBuilder", r.deps.FactGraphBuilder != nil))
	case CapabilityEntityProfile:
		add("capability.entity_profile.store", r.deps.EntityProfileStore != nil, dependencyMessage("EntityProfileStore", r.deps.EntityProfileStore != nil))
		add("capability.entity_profile.service", r.deps.EntityProfileBuilder != nil, dependencyMessage("EntityProfileBuilder", r.deps.EntityProfileBuilder != nil))
	case CapabilityEntityTimeline:
		add("capability.entity_timeline.store", r.deps.EntityTimelineStore != nil, dependencyMessage("EntityTimelineStore", r.deps.EntityTimelineStore != nil))
		add("capability.entity_timeline.service", r.deps.EntityTimelineBuilder != nil, dependencyMessage("EntityTimelineBuilder", r.deps.EntityTimelineBuilder != nil))
	default:
		add(fmt.Sprintf("capability.%s", capability), false, "capability is not implemented by the root facade")
	}
}

func dependencyMessage(name string, ready bool) string {
	if ready {
		return name + " configured"
	}
	return name + " missing"
}
