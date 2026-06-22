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

	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
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
	Name     string
	Ready    bool
	Severity DiagnosticSeverity
	Message  string
}

// RebuildRequest describes a requested derived-view rebuild scope.
type RebuildRequest struct {
	TraceID        TraceID
	Scope          Scope
	Capabilities   []Capability
	Documents      []DocumentTarget
	ScanDocuments  bool
	DryRun         bool
	PageSize       int
	PageToken      string
	Reason         string
	IdempotencyKey string
}

// ReconcileRequest describes a requested reconciliation scope.
type ReconcileRequest struct {
	TraceID        TraceID
	Scope          Scope
	Capabilities   []Capability
	Documents      []DocumentTarget
	AutoRepair     bool
	ScanDocuments  bool
	DryRun         bool
	PageSize       int
	PageToken      string
	Reason         string
	IdempotencyKey string
}

// ReloadRequest describes a requested lifecycle reload scope.
type ReloadRequest struct {
	TraceID        TraceID
	Scope          Scope
	Capabilities   []Capability
	Documents      []DocumentTarget
	ScanDocuments  bool
	DryRun         bool
	PageSize       int
	PageToken      string
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
	AutoRepair     bool
	ScanDocuments  bool
	DryRun         bool
	PageSize       int
	PageToken      string
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
		severity := DiagnosticSeverityInfo
		if !ready {
			severity = DiagnosticSeverityError
		}
		report.Checks = append(report.Checks, ReadinessCheck{
			Name:     name,
			Ready:    ready,
			Severity: severity,
			Message:  message,
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
	r.addWriteDependencyReadiness(&report)
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
		ScanDocuments:  req.ScanDocuments,
		DryRun:         req.DryRun,
		PageSize:       req.PageSize,
		PageToken:      req.PageToken,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
	})
}

// Reconcile plans or enqueues cross-view reconciliation substrate work.
func (r *System) Reconcile(ctx context.Context, req ReconcileRequest) (LifecycleExecutionReport, error) {
	return r.dispatchLifecycle(ctx, LifecycleActionReconcile, lifecycleDispatchRequest(req))
}

// Reload plans or enqueues lifecycle reload substrate work.
func (r *System) Reload(ctx context.Context, req ReloadRequest) (LifecycleExecutionReport, error) {
	return r.dispatchLifecycle(ctx, LifecycleActionReload, lifecycleDispatchRequest{
		TraceID:        req.TraceID,
		Scope:          req.Scope,
		Capabilities:   req.Capabilities,
		Documents:      req.Documents,
		ScanDocuments:  req.ScanDocuments,
		DryRun:         req.DryRun,
		PageSize:       req.PageSize,
		PageToken:      req.PageToken,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
	})
}

// Freshness runs bounded freshness diagnostics synchronously and returns a
// lifecycle-shaped control-plane report for callers that do not run workers.
func (r *System) Freshness(ctx context.Context, req FreshnessRequest) (FreshnessResult, error) {
	report, diagnostics, err := r.dispatchFreshnessLifecycle(ctx, lifecycleDispatchRequest{
		TraceID:        req.TraceID,
		Scope:          req.Scope,
		Capabilities:   req.Capabilities,
		Documents:      req.Documents,
		DryRun:         req.DryRun,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
	})
	freshness := FreshnessResult{LifecycleExecutionReport: report}
	applyDiagnosticsToFreshnessResult(&freshness, diagnostics)
	return freshness, err
}

type lifecycleDispatchRequest struct {
	TraceID        TraceID
	Scope          Scope
	Capabilities   []Capability
	Documents      []DocumentTarget
	AutoRepair     bool
	ScanDocuments  bool
	DryRun         bool
	PageSize       int
	PageToken      string
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
	if action == LifecycleActionReconcile || req.ScanDocuments {
		report.Operation.AutoRepair = req.AutoRepair
		report.Operation.PageSize = normalizeDiagnosticPageSize(req.PageSize)
		report.Operation.PageToken = strings.TrimSpace(req.PageToken)
	}
	report.Operation.ScanDocuments = req.ScanDocuments
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
	documentScanRequested := req.ScanDocuments && len(documents) == 0
	if req.ScanDocuments && documentScanPageTokenLooksDiagnostic(req.PageToken) {
		report.Status = LifecycleStatusRejected
		report.Message = "invalid document scan page token"
		return report, errdefs.Validationf("memory: document scan page token must be a source document id, not a diagnostics projection page token")
	}
	if documentScanRequested && scope.DatasetID == "" {
		report.Status = LifecycleStatusRejected
		report.Message = "document scan requires scope.dataset_id"
		return report, errdefs.Validationf("memory: document scan requires scope.dataset_id; refusing to scan all datasets")
	}
	if documentScanRequested && !r.documentScanCapabilitySelected(req.Capabilities) {
		report.Status = LifecycleStatusUnsupported
		report.Message = "document scan requires document_chunks capability"
		report.Steps = []LifecycleStep{lifecycleCapabilityStep("document_chunks.scan", CapabilityDocumentChunks, LifecycleStatusUnsupported, "ScanDocuments requires the document_chunks capability; message_index and summary_dag are not valid document scan targets")}
		return report, errdefs.NotAvailablef("memory: document scan requires document_chunks capability")
	}
	if documentScanRequested && !r.lifecycleCapabilityAvailable(CapabilityDocumentChunks) {
		report.Status = LifecycleStatusUnsupported
		report.Message = "document_chunks capability is not configured for document scan"
		report.Steps = []LifecycleStep{lifecycleCapabilityStep("document_chunks.configure", CapabilityDocumentChunks, LifecycleStatusUnsupported, "DocumentStore, ChunkStore, DocumentChunker, and document_chunks projection are required for document scan")}
		return report, errdefs.NotAvailablef("memory: document_chunks capability is not configured for document scan")
	}
	if documentScanRequested {
		page, err := r.scanDocumentTargetsPage(ctx, scope, req.PageSize, req.PageToken)
		if err != nil {
			report.Status = LifecycleStatusFailed
			report.Message = err.Error()
			return report, err
		}
		documents = page.Documents
		report.Operation.ScanDocuments = true
		report.Operation.PageSize = page.PageSize
		report.Operation.PageToken = page.PageToken
		applyDocumentScanCheckpoint(&report, page)
	} else if len(documents) > 0 {
		report.Operation.ScanDocuments = false
	}
	report.Operation.Documents = documents
	report.Operation.Targets = lifecycleTargetsForDocuments(documents)
	if documentScanRequested {
		report.Operation.Capabilities = []Capability{CapabilityDocumentChunks}
	} else {
		report.Operation.Capabilities = r.normalizeLifecycleOperationCapabilities(action, req.Capabilities, scope, documents)
	}
	report.Operation.IdempotencyKey = lifecycleIdempotencyKey(report.Operation, req.IdempotencyKey)
	if !r.lifecycleActionDeclared(action) {
		report.Status = LifecycleStatusUnsupported
		report.Message = fmt.Sprintf("lifecycle action %q is not declared by the plan", action)
		return report, errdefs.NotAvailablef("memory: lifecycle action %q is not declared by the plan", action)
	}

	report.Supported = true
	if report.Operation.ScanDocuments && len(report.Operation.Documents) == 0 {
		report.Accepted = true
		report.Status = LifecycleStatusSkipped
		report.Steps = []LifecycleStep{emptyDocumentScanLifecycleStep(report.Operation)}
		report.Message = fmt.Sprintf("%s skipped; document scan returned no documents for requested page", action)
		return report, nil
	}
	if action == LifecycleActionReconcile && report.Operation.DryRun {
		return r.dispatchReconcileDryRun(ctx, &report)
	}
	if handled, err := r.dispatchDerivedViewLifecycle(ctx, action, &report); handled || err != nil {
		return report, err
	}
	if req.DryRun {
		report.Accepted = true
		report.Status = LifecycleStatusPlanned
		report.Message = fmt.Sprintf("%s lifecycle operation planned; no runner registered yet", action)
		return report, nil
	}

	jobID := lifecycleJobIDForOperation(report.Operation.ID)
	checkpoint := lifecycleJobCheckpoint(report.Operation)
	checkpoint = mergeLifecycleCheckpoints(checkpoint, report.Checkpoint)
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
		Checkpoint:   checkpoint,
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
	if len(checkpoint) > 0 {
		report.Checkpoint = cloneCheckpoint(checkpoint)
	}
	report.Message = fmt.Sprintf("%s lifecycle job enqueued", action)
	return report, nil
}

func (r *System) dispatchFreshnessLifecycle(ctx context.Context, req lifecycleDispatchRequest) (report LifecycleExecutionReport, diagnostics DiagnosticReport, err error) {
	action := LifecycleActionFreshnessCheck
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
		return report, diagnostics, errdefs.NotAvailablef("memory: system is not configured")
	}

	scope := normalizeScope(req.Scope)
	report.Operation.Scope = scope
	report.Operation.PlanDigest = r.lifecyclePlanDigest()
	if err := scope.Validate(); err != nil {
		report.Status = LifecycleStatusRejected
		report.Message = "invalid scope"
		return report, diagnostics, errdefs.Validationf("memory: invalid scope: %w", err)
	}
	documents, err := normalizeDocumentTargets(scope, req.Documents)
	if err != nil {
		report.Status = LifecycleStatusRejected
		report.Message = "invalid document targets"
		return report, diagnostics, err
	}
	report.Operation.Documents = documents
	report.Operation.Targets = lifecycleTargetsForDocuments(documents)
	report.Operation.Capabilities = r.normalizeLifecycleOperationCapabilities(action, req.Capabilities, scope, documents)
	report.Operation.IdempotencyKey = lifecycleIdempotencyKey(report.Operation, req.IdempotencyKey)
	if !r.lifecycleActionDeclared(action) {
		report.Status = LifecycleStatusUnsupported
		report.Message = fmt.Sprintf("lifecycle action %q is not declared by the plan", action)
		return report, diagnostics, errdefs.NotAvailablef("memory: lifecycle action %q is not declared by the plan", action)
	}

	report.Supported = true
	report.Steps = nil
	diagnostics, err = r.runDiagnosticProbes(ctx, DiagnosticRequest{
		TraceID:      report.TraceID,
		Scope:        report.Operation.Scope,
		Capabilities: report.Operation.Capabilities,
		Documents:    report.Operation.Documents,
		Stage:        diagnosticStageFreshness,
	}, false)
	applyDiagnosticsToLifecycleReport(&report, diagnostics)
	if err != nil {
		report.Status = LifecycleStatusFailed
		report.Message = err.Error()
		report.Summary = err.Error()
		return report, diagnostics, err
	}
	if !diagnostics.OK && diagnosticsHasErrorSeverity(diagnostics) {
		err = fmt.Errorf("memory: freshness diagnostics failed: %s", diagnostics.Message)
		report.Status = LifecycleStatusFailed
		if report.Message == "" || report.Message == "diagnostics reported issues" {
			report.Message = err.Error()
		}
		return report, diagnostics, err
	}

	report.Status = LifecycleStatusCompleted
	if report.Message == "" {
		report.Message = "freshness diagnostics completed"
	}
	return report, diagnostics, nil
}

func newLifecycleExecutionReport(action LifecycleAction, req lifecycleDispatchRequest) LifecycleExecutionReport {
	now := time.Now()
	traceID := ensureTraceID(req.TraceID)
	operation := LifecycleOperation{
		ID:            newOperationID(),
		TraceID:       traceID,
		Action:        action,
		Scope:         normalizeScope(req.Scope),
		Capabilities:  cloneCapabilities(req.Capabilities),
		Documents:     cloneDocumentTargets(req.Documents),
		Reason:        strings.TrimSpace(req.Reason),
		AutoRepair:    req.AutoRepair,
		ScanDocuments: req.ScanDocuments,
		DryRun:        req.DryRun,
		PageSize:      req.PageSize,
		PageToken:     strings.TrimSpace(req.PageToken),
		RequestedAt:   now,
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

func (r *System) dispatchDerivedViewLifecycle(ctx context.Context, action LifecycleAction, report *LifecycleExecutionReport) (bool, error) {
	if action != LifecycleActionReload && action != LifecycleActionRebuild {
		return false, nil
	}
	capabilities := selectedDerivedViewLifecycleCapabilities(report.Operation.Capabilities)
	if len(capabilities) == 0 {
		report.Steps = nil
		report.Accepted = true
		report.Status = LifecycleStatusUnsupported
		report.Message = fmt.Sprintf("no derived-view capabilities selected for %s", action)
		report.Steps = append(report.Steps, LifecycleStep{
			Name:    "derived_views.capabilities",
			Status:  LifecycleStatusUnsupported,
			Planned: true,
			Message: "rebuild/reload requires document_chunks, message_index, or summary_dag",
		})
		return true, errdefs.NotAvailablef("memory: no derived-view capabilities selected for %s", action)
	}

	report.Steps = nil
	var runnable bool
	var skipped int
	var unsupportedErr error
	for _, capability := range capabilities {
		switch capability {
		case CapabilityDocumentChunks:
			if !r.lifecycleCapabilityAvailable(CapabilityDocumentChunks) {
				unsupportedErr = errors.Join(unsupportedErr, errdefs.NotAvailablef("memory: document_chunks capability is not configured for targeted %s", action))
				report.Steps = append(report.Steps, lifecycleCapabilityStep("document_chunks.configure", CapabilityDocumentChunks, LifecycleStatusUnsupported, "DocumentStore, ChunkStore, and DocumentChunker are required for targeted reload/rebuild"))
				continue
			}
			if len(report.Operation.Documents) == 0 {
				skipped++
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
				continue
			}
			runnable = true
			if report.Operation.DryRun {
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
			} else {
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
			}
		case CapabilityMessageIndex:
			if !r.lifecycleCapabilityAvailable(CapabilityMessageIndex) {
				unsupportedErr = errors.Join(unsupportedErr, errdefs.NotAvailablef("memory: message_index capability is not configured for scoped %s", action))
				report.Steps = append(report.Steps, lifecycleCapabilityStep("message_index.configure", CapabilityMessageIndex, LifecycleStatusUnsupported, "MessageStore and message_index projection are required for scoped reload/rebuild"))
				continue
			}
			if report.Operation.Scope.ConversationID == "" {
				skipped++
				report.Steps = append(report.Steps, lifecycleCapabilityStep("message_index.scope", CapabilityMessageIndex, LifecycleStatusSkipped, "conversation_id is required; full message scans are intentionally unsupported"))
				continue
			}
			runnable = true
			report.Steps = append(report.Steps, lifecycleCapabilityStep("message_index.conversation", CapabilityMessageIndex, lifecycleDryRunStatus(report.Operation.DryRun), lifecyclePlannedMessage(report.Operation.DryRun, "message_index", report.Operation.Scope.ConversationID)))
		case CapabilitySummaryDAG:
			if !r.lifecycleCapabilityAvailable(CapabilitySummaryDAG) {
				unsupportedErr = errors.Join(unsupportedErr, errdefs.NotAvailablef("memory: summary_dag capability is not configured for scoped %s", action))
				report.Steps = append(report.Steps, lifecycleCapabilityStep("summary_dag.configure", CapabilitySummaryDAG, LifecycleStatusUnsupported, "MessageStore, SummaryStore, Summarizer, and summary_dag projection are required for scoped reload/rebuild"))
				continue
			}
			if report.Operation.Scope.ConversationID == "" {
				skipped++
				report.Steps = append(report.Steps, lifecycleCapabilityStep("summary_dag.scope", CapabilitySummaryDAG, LifecycleStatusSkipped, "conversation_id is required; full summary scans are intentionally unsupported"))
				continue
			}
			runnable = true
			report.Steps = append(report.Steps, lifecycleCapabilityStep("summary_dag.conversation", CapabilitySummaryDAG, lifecycleDryRunStatus(report.Operation.DryRun), lifecyclePlannedMessage(report.Operation.DryRun, "summary_dag", report.Operation.Scope.ConversationID)))
		}
	}
	if unsupportedErr != nil {
		report.Accepted = true
		report.Status = LifecycleStatusUnsupported
		report.Message = unsupportedErr.Error()
		return true, unsupportedErr
	}
	if !runnable {
		report.Accepted = true
		report.Supported = true
		report.Status = LifecycleStatusSkipped
		report.Message = fmt.Sprintf("%s skipped; scoped targets were not sufficient", action)
		if skipped == 0 {
			report.Message = fmt.Sprintf("%s skipped; no full scan was planned", action)
		}
		return true, nil
	}
	if report.Operation.DryRun {
		report.Accepted = true
		report.Status = LifecycleStatusPlanned
		report.Message = fmt.Sprintf("%s planned for %d derived-view capability(s)", action, len(capabilities))
		return true, nil
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
		Stages:       lifecycleDerivedViewPlannedStages(capabilities),
		MaxAttempts:  1,
		Checkpoint:   cloneCheckpoint(report.Checkpoint),
	}
	if _, ok := r.lifecycleRunner(job.Kind); !ok {
		report.Status = LifecycleStatusUnsupported
		report.Message = fmt.Sprintf("no lifecycle runner registered for derived-view %s", action)
		return true, errdefs.NotAvailablef("memory: no lifecycle runner registered for derived-view %s", action)
	}
	if r.jobStore == nil {
		report.Status = LifecycleStatusFailed
		report.Message = "job store is not configured"
		return true, errdefs.NotAvailablef("memory: job store is not configured for derived-view %s", action)
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
	if len(report.Checkpoint) > 0 {
		report.Checkpoint = cloneCheckpoint(report.Checkpoint)
	}
	report.Message = fmt.Sprintf("%s enqueued for %d derived-view capability(s)", action, len(capabilities))
	return true, nil
}

func lifecycleCapabilityStep(name string, capability Capability, status LifecycleExecutionStatus, message string) LifecycleStep {
	step := LifecycleStep{
		Name:    name,
		Status:  status,
		Planned: true,
		Message: message,
		Details: map[string]any{
			"capability": string(capability),
		},
	}
	if status == LifecycleStatusSkipped {
		step.Skipped = true
	}
	return step
}

func emptyDocumentScanLifecycleStep(operation LifecycleOperation) LifecycleStep {
	step := lifecycleCapabilityStep("document_chunks.scan", CapabilityDocumentChunks, LifecycleStatusSkipped, "document scan returned no documents for requested page; no document_chunks work was planned")
	step.Details["dataset_id"] = operation.Scope.DatasetID
	step.Details["page_size"] = operation.PageSize
	if operation.PageToken != "" {
		step.Details["page_token"] = operation.PageToken
	}
	return step
}

func lifecycleDryRunStatus(dryRun bool) LifecycleExecutionStatus {
	if dryRun {
		return LifecycleStatusPlanned
	}
	return LifecycleStatusEnqueued
}

func lifecyclePlannedMessage(dryRun bool, capability, conversationID string) string {
	if dryRun {
		return fmt.Sprintf("would rebuild %s for conversation %s", capability, conversationID)
	}
	return fmt.Sprintf("will rebuild %s for conversation %s asynchronously", capability, conversationID)
}

type documentScanPage struct {
	Documents     []DocumentTarget
	PageSize      int
	PageToken     string
	NextPageToken string
	Scanned       int
}

func (r *System) documentScanCapabilitySelected(requested []Capability) bool {
	capabilities := dedupeCapabilities(requested)
	if len(capabilities) == 0 {
		return r != nil && r.assembly.HasCapability(CapabilityDocumentChunks)
	}
	for _, capability := range capabilities {
		if capability == CapabilityDocumentChunks {
			return true
		}
	}
	return false
}

func documentScanPageTokenLooksDiagnostic(pageToken string) bool {
	return strings.HasPrefix(strings.TrimSpace(pageToken), diagnosticProjectionPageTokenPrefix)
}

func (r *System) scanDocumentTargetsPage(ctx context.Context, scope Scope, pageSize int, pageToken string) (documentScanPage, error) {
	if r == nil || r.deps.DocumentStore == nil {
		return documentScanPage{}, errdefs.NotAvailablef("memory: document store is not configured for document scan")
	}
	datasetID := strings.TrimSpace(scope.DatasetID)
	if datasetID == "" {
		return documentScanPage{}, errdefs.Validationf("memory: document scan requires scope.dataset_id; refusing to scan all datasets")
	}
	normalizedPageSize := normalizeDiagnosticPageSize(pageSize)
	normalizedPageToken := strings.TrimSpace(pageToken)
	docs, err := r.deps.DocumentStore.List(ctx, datasetID, sourcedocument.ListOptions{
		AfterID: normalizedPageToken,
		Limit:   normalizedPageSize + 1,
	})
	if err != nil {
		return documentScanPage{}, err
	}
	page := documentScanPage{
		PageSize:  normalizedPageSize,
		PageToken: normalizedPageToken,
	}
	if len(docs) > normalizedPageSize {
		page.NextPageToken = docs[normalizedPageSize-1].ID
		docs = docs[:normalizedPageSize]
	}
	page.Documents = make([]DocumentTarget, 0, len(docs))
	for _, doc := range docs {
		if strings.TrimSpace(doc.ID) == "" {
			continue
		}
		targetDatasetID := strings.TrimSpace(doc.DatasetID)
		if targetDatasetID == "" {
			targetDatasetID = datasetID
		}
		page.Documents = append(page.Documents, DocumentTarget{
			DatasetID:  targetDatasetID,
			DocumentID: strings.TrimSpace(doc.ID),
		})
	}
	page.Scanned = len(page.Documents)
	return page, nil
}

func applyDocumentScanCheckpoint(report *LifecycleExecutionReport, page documentScanPage) {
	if report == nil {
		return
	}
	if report.Checkpoint == nil {
		report.Checkpoint = map[string]any{}
	}
	report.Checkpoint["document_scan"] = true
	report.Checkpoint["document_scan_page_size"] = page.PageSize
	report.Checkpoint["document_scan_scanned"] = page.Scanned
	report.Checkpoint["document_scan_next_page_token"] = page.NextPageToken
	if page.PageToken != "" {
		report.Checkpoint["document_scan_page_token"] = page.PageToken
	}
}

func (r *System) normalizeLifecycleOperationCapabilities(action LifecycleAction, requested []Capability, scope Scope, documents []DocumentTarget) []Capability {
	capabilities := dedupeCapabilities(requested)
	if len(capabilities) > 0 {
		return capabilities
	}
	switch action {
	case LifecycleActionRebuild, LifecycleActionReload, LifecycleActionReconcile:
		if len(documents) > 0 {
			if r != nil && r.assembly.HasCapability(CapabilityDocumentChunks) {
				return []Capability{CapabilityDocumentChunks}
			}
			return nil
		}
		if normalizeScope(scope).ConversationID != "" && r != nil {
			var out []Capability
			if r.lifecycleCapabilityAvailable(CapabilityMessageIndex) {
				out = append(out, CapabilityMessageIndex)
			}
			if r.lifecycleCapabilityAvailable(CapabilitySummaryDAG) {
				out = append(out, CapabilitySummaryDAG)
			}
			return out
		}
	}
	return nil
}

func (r *System) lifecycleActionDeclared(action LifecycleAction) bool {
	for _, stage := range r.plan.Lifecycle {
		if stage.Name == string(action) {
			return true
		}
	}
	return false
}

func applyDiagnosticsToFreshnessResult(freshness *FreshnessResult, diagnostics DiagnosticReport) {
	if freshness == nil || diagnostics.Stage == "" {
		return
	}
	freshness.Ready = diagnostics.Ready
	freshness.OK = diagnostics.OK
	freshness.Checks = cloneDiagnosticChecks(diagnostics.Checks)
	freshness.Warnings = append([]string(nil), diagnostics.Warnings...)
	freshness.Diagnostics = diagnostics
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
	builder.WriteString(strconv.FormatBool(operation.AutoRepair))
	builder.WriteByte('|')
	builder.WriteString(strconv.FormatBool(operation.ScanDocuments))
	builder.WriteByte('|')
	builder.WriteString(strconv.Itoa(operation.PageSize))
	builder.WriteByte('|')
	builder.WriteString(operation.PageToken)
	builder.WriteByte('|')
	builder.WriteString(operation.Reason)
	builder.WriteByte('|')
	builder.WriteString(operation.PlanDigest)
	sum := sha256.Sum256([]byte(builder.String()))
	return "sha256:" + hex.EncodeToString(sum[:16])
}

func lifecycleJobCheckpoint(operation LifecycleOperation) map[string]any {
	if operation.Action != LifecycleActionReconcile {
		return nil
	}
	checkpoint := map[string]any{
		"auto_repair":           operation.AutoRepair,
		"repair_targets_source": reconcileRepairTargetsSource(operation.AutoRepair, operation.ScanDocuments, operation.Documents),
		"diagnostics_page_size": operation.PageSize,
	}
	if operation.PageToken != "" && !operation.ScanDocuments {
		checkpoint["diagnostics_page_token"] = operation.PageToken
	}
	return checkpoint
}

func reconcileRepairTargetsSource(autoRepair, documentScan bool, documents []DocumentTarget) string {
	if len(documents) > 0 {
		if documentScan {
			return "document_scan"
		}
		return "explicit"
	}
	if documentScan {
		return "document_scan"
	}
	if autoRepair {
		return "diagnostics_page"
	}
	return "none"
}

func mergeLifecycleCheckpoints(left, right map[string]any) map[string]any {
	if len(left) == 0 {
		return cloneCheckpoint(right)
	}
	out := cloneCheckpoint(left)
	for key, value := range right {
		out[key] = value
	}
	return out
}

func scopeDigestInput(scope Scope) string {
	return strings.Join([]string{
		scope.RuntimeID,
		scope.UserID,
		scope.AgentID,
		scope.ConversationID,
		scope.DatasetID,
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
		severity := DiagnosticSeverityInfo
		if !ready {
			severity = DiagnosticSeverityError
		}
		report.Checks = append(report.Checks, ReadinessCheck{
			Name:     name,
			Ready:    ready,
			Severity: severity,
			Message:  message,
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
	case CapabilityDocumentChunks:
		add("capability.document_chunks.store", r.deps.ChunkStore != nil, dependencyMessage("ChunkStore", r.deps.ChunkStore != nil))
	default:
		add(fmt.Sprintf("capability.%s", capability), false, "capability is not implemented by the root facade")
	}
}

func (r *System) addWriteDependencyReadiness(report *ReadinessReport) {
	if r == nil || report == nil {
		return
	}
	addWarning := func(name, dependency string) {
		report.Checks = append(report.Checks, ReadinessCheck{
			Name:     name,
			Ready:    false,
			Severity: DiagnosticSeverityWarning,
			Message:  dependency + " missing; writes for this capability will return NotAvailable",
		})
	}
	for _, stage := range r.plan.Write {
		switch stage.Name {
		case writeStageChunkDocument:
			if !stage.Optional && !r.writeAvailable[CapabilityDocumentChunks] && r.deps.DocumentChunker == nil {
				addWarning("write_readiness.document_chunks", "DocumentChunker")
			}
		case writeStageBuildSummaryDAG:
			if !stage.Optional && !r.writeAvailable[CapabilitySummaryDAG] && r.deps.Summarizer == nil {
				addWarning("write_readiness.summary_dag", "Summarizer")
			}
		}
	}
}

func dependencyMessage(name string, ready bool) string {
	if ready {
		return name + " configured"
	}
	return name + " missing"
}
