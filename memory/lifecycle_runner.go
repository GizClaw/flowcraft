package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// LifecycleRunner executes one claimed lifecycle job payload.
type LifecycleRunner interface {
	Run(context.Context, LifecycleRunRequest) (LifecycleExecutionReport, error)
}

// LifecycleRunRequest is the in-process context for one claimed runner attempt.
type LifecycleRunRequest struct {
	Job        LifecycleJob
	WorkerID   string
	Attempt    int
	Checkpoint map[string]any
	System     *System
}

// LifecycleRunnerRegistry resolves serializable job kinds to in-process runners.
type LifecycleRunnerRegistry struct {
	runners map[LifecycleJobKind]LifecycleRunner
}

// NewLifecycleRunnerRegistry creates an empty lifecycle runner registry.
func NewLifecycleRunnerRegistry() *LifecycleRunnerRegistry {
	return &LifecycleRunnerRegistry{runners: make(map[LifecycleJobKind]LifecycleRunner)}
}

// Register binds a job kind to a runner. Passing a nil runner removes the binding.
func (r *LifecycleRunnerRegistry) Register(kind LifecycleJobKind, runner LifecycleRunner) {
	if r == nil || kind == "" {
		return
	}
	if r.runners == nil {
		r.runners = make(map[LifecycleJobKind]LifecycleRunner)
	}
	if runner == nil {
		delete(r.runners, kind)
		return
	}
	r.runners[kind] = runner
}

// Lookup returns the runner registered for kind.
func (r *LifecycleRunnerRegistry) Lookup(kind LifecycleJobKind) (LifecycleRunner, bool) {
	if r == nil || r.runners == nil {
		return nil, false
	}
	runner, ok := r.runners[kind]
	return runner, ok && runner != nil
}

func (r *System) defaultLifecycleRunnerRegistry() *LifecycleRunnerRegistry {
	registry := NewLifecycleRunnerRegistry()
	registry.Register(LifecycleJobKindWriteChain, writeChainLifecycleRunner{})
	registry.Register(LifecycleJobKindFreshnessCheck, freshnessCheckLifecycleRunner{})
	registry.Register(LifecycleJobKindReconcile, reconcileLifecycleRunner{})
	if r != nil && r.writeAvailable[CapabilityDocumentChunks] {
		runner := documentChunksLifecycleRunner{}
		registry.Register(LifecycleJobKindReload, runner)
		registry.Register(LifecycleJobKindRebuild, runner)
	}
	return registry
}

func (r *System) lifecycleRunner(kind LifecycleJobKind) (LifecycleRunner, bool) {
	if r == nil || r.runnerRegistry == nil {
		return nil, false
	}
	return r.runnerRegistry.Lookup(kind)
}

type writeChainLifecycleRunner struct{}

func (writeChainLifecycleRunner) Run(ctx context.Context, req LifecycleRunRequest) (LifecycleExecutionReport, error) {
	report := newLifecycleReportForJob(req.System, req.Job)
	if req.Job.Kind != LifecycleJobKindWriteChain {
		err := errdefs.NotAvailablef("memory: write_chain runner cannot execute job kind %q", req.Job.Kind)
		failLifecycleReport(&report, err)
		return report, err
	}
	if req.System == nil || req.System.inner == nil {
		err := errdefs.NotAvailablef("memory: system is not configured")
		failLifecycleReport(&report, err)
		return report, err
	}

	result, err := req.System.executeWriteStages(ctx, req.Job.Stages, req.Job.Window, req.Job.Scope)
	if err != nil {
		failLifecycleReport(&report, err)
		return report, err
	}

	report.Accepted = true
	report.Supported = true
	report.Status = LifecycleStatusCompleted
	report.Message = fmt.Sprintf("write_chain completed %d stage(s)", len(req.Job.Stages))
	report.Checkpoint = map[string]any{
		"stages":          len(req.Job.Stages),
		"observations":    len(result.Observations),
		"facts":           len(result.Facts),
		"entity_profiles": len(result.EntityProfiles),
		"entity_events":   len(result.EntityEvents),
	}
	if result.FactGraph != nil {
		report.Checkpoint["fact_graph"] = true
	}
	for _, stage := range req.Job.Stages {
		if stage.Name == writeStageAppendMessage || stage.Name == writeStageChunkDocument {
			continue
		}
		report.Steps = append(report.Steps, LifecycleStep{
			Name:      stage.Name,
			Status:    LifecycleStatusCompleted,
			Planned:   true,
			Completed: true,
		})
	}
	finalizeLifecycleExecutionReport(&report)
	return report, nil
}

type documentChunksLifecycleRunner struct{}

func (documentChunksLifecycleRunner) Run(ctx context.Context, req LifecycleRunRequest) (LifecycleExecutionReport, error) {
	report := newLifecycleReportForJob(req.System, req.Job)
	if req.Job.Kind != LifecycleJobKindReload && req.Job.Kind != LifecycleJobKindRebuild {
		err := errdefs.NotAvailablef("memory: document_chunks runner cannot execute job kind %q", req.Job.Kind)
		failLifecycleReport(&report, err)
		return report, err
	}
	if !capabilitySelected(req.Job.Capabilities, CapabilityDocumentChunks) {
		err := errdefs.NotAvailablef("memory: no lifecycle runner registered for job kind %q with requested capabilities", req.Job.Kind)
		failLifecycleReport(&report, err)
		return report, err
	}
	if req.System == nil || req.System.inner == nil {
		err := errdefs.NotAvailablef("memory: system is not configured")
		failLifecycleReport(&report, err)
		return report, err
	}
	if !req.System.writeAvailable[CapabilityDocumentChunks] {
		err := errdefs.NotAvailablef("memory: document_chunks capability is not configured for targeted %s", req.Job.Kind)
		failLifecycleReport(&report, err)
		return report, err
	}
	if len(req.Job.Documents) == 0 {
		report.Accepted = true
		report.Supported = true
		report.Status = LifecycleStatusSkipped
		report.Message = "document_chunks lifecycle job has no document targets"
		report.Steps = append(report.Steps, LifecycleStep{
			Name:    "document_chunks.targets",
			Status:  LifecycleStatusSkipped,
			Planned: true,
			Skipped: true,
			Message: "no document targets supplied",
		})
		finalizeLifecycleExecutionReport(&report)
		return report, nil
	}

	var runErr error
	for _, target := range req.Job.Documents {
		chunkCount := 0
		step := LifecycleStep{
			Name:    "document_chunks.target",
			Status:  LifecycleStatusCompleted,
			Planned: true,
			Details: map[string]any{
				"capability":  string(CapabilityDocumentChunks),
				"dataset_id":  target.DatasetID,
				"document_id": target.DocumentID,
			},
		}
		targetScope := req.Job.Scope
		targetScope.DatasetID = target.DatasetID
		namespace, err := req.System.scopedWriteNamespace(CapabilityDocumentChunks, targetScope)
		if err == nil {
			step.Details["scoped_namespace"] = namespace
			step.Details["runtime_id"] = targetScope.RuntimeID
			step.Details["user_id"] = targetScope.UserID
			step.Details["conversation_id"] = targetScope.ConversationID
			chunks, indexErr := req.System.inner.IndexDocument(ctx, targetScope, target.DocumentID, namespace)
			if indexErr == nil {
				chunkCount = len(chunks)
				step.Details["chunk_count"] = chunkCount
			}
			err = indexErr
		}
		if err != nil {
			step.Status = LifecycleStatusFailed
			step.Message = "document re-index failed"
			step.Details["error"] = err.Error()
			report.TargetErrors = append(report.TargetErrors, LifecycleTargetError{
				Target:  lifecycleTargetForDocument(target),
				Message: "document re-index failed",
				Error:   err.Error(),
			})
			runErr = errors.Join(runErr, err)
		} else {
			step.Completed = true
			if step.Details["chunk_count"] == nil {
				step.Details["chunk_count"] = 0
			}
			step.Message = fmt.Sprintf("re-indexed document %s into %d chunk(s)", documentTargetLabel(target), chunkCount)
		}
		report.Steps = append(report.Steps, step)
	}

	report.Accepted = true
	report.Supported = true
	if runErr != nil {
		report.Status = LifecycleStatusFailed
		report.Message = fmt.Sprintf("%s failed for %d of %d document target(s)", req.Job.Kind, len(report.TargetErrors), len(req.Job.Documents))
	} else {
		report.Status = LifecycleStatusCompleted
		report.Message = fmt.Sprintf("%s completed for %d document target(s)", req.Job.Kind, len(req.Job.Documents))
	}
	finalizeLifecycleExecutionReport(&report)
	return report, runErr
}

type freshnessCheckLifecycleRunner struct{}

func (freshnessCheckLifecycleRunner) Run(ctx context.Context, req LifecycleRunRequest) (LifecycleExecutionReport, error) {
	report := newLifecycleReportForJob(req.System, req.Job)
	if req.Job.Kind != LifecycleJobKindFreshnessCheck {
		err := errdefs.NotAvailablef("memory: freshness_check runner cannot execute job kind %q", req.Job.Kind)
		failLifecycleReport(&report, err)
		return report, err
	}
	if req.System == nil || req.System.inner == nil {
		err := errdefs.NotAvailablef("memory: system is not configured")
		failLifecycleReport(&report, err)
		return report, err
	}

	diagnostics, err := req.System.Diagnostics(ctx, DiagnosticRequest{
		TraceID:      req.Job.TraceID,
		Scope:        req.Job.Scope,
		Capabilities: req.Job.Capabilities,
		Documents:    req.Job.Documents,
		Stage:        diagnosticStageFreshness,
	})
	applyDiagnosticsToLifecycleReport(&report, diagnostics)
	if err != nil {
		report.Status = LifecycleStatusFailed
		if report.Message == "" {
			report.Message = err.Error()
		}
		finalizeLifecycleExecutionReport(&report)
		return report, err
	}
	if !diagnostics.OK && diagnosticsHasErrorSeverity(diagnostics) {
		err := fmt.Errorf("memory: freshness diagnostics failed: %s", diagnostics.Message)
		report.Status = LifecycleStatusFailed
		if report.Message == "" || report.Message == "diagnostics checks found missing dependencies" {
			report.Message = err.Error()
		}
		finalizeLifecycleExecutionReport(&report)
		return report, err
	}

	report.Status = LifecycleStatusCompleted
	if report.Message == "" {
		report.Message = "freshness diagnostics completed"
	}
	finalizeLifecycleExecutionReport(&report)
	return report, nil
}

type reconcileLifecycleRunner struct{}

func (reconcileLifecycleRunner) Run(ctx context.Context, req LifecycleRunRequest) (LifecycleExecutionReport, error) {
	report := newLifecycleReportForJob(req.System, req.Job)
	if req.Job.Kind != LifecycleJobKindReconcile {
		err := errdefs.NotAvailablef("memory: reconcile runner cannot execute job kind %q", req.Job.Kind)
		failLifecycleReport(&report, err)
		return report, err
	}
	if req.System == nil || req.System.inner == nil {
		err := errdefs.NotAvailablef("memory: system is not configured")
		failLifecycleReport(&report, err)
		return report, err
	}

	diagnostics, err := req.System.Diagnostics(ctx, DiagnosticRequest{
		TraceID:      req.Job.TraceID,
		Scope:        req.Job.Scope,
		Capabilities: req.Job.Capabilities,
		Documents:    req.Job.Documents,
		Stage:        diagnosticStageConsistency,
		Consistency: []ConsistencyCheckKind{
			ConsistencyCheckProjection,
			ConsistencyCheckSourceView,
		},
	})
	applyDiagnosticsToLifecycleReport(&report, diagnostics)
	if err != nil {
		report.Status = LifecycleStatusFailed
		if report.Message == "" {
			report.Message = err.Error()
		}
		finalizeLifecycleExecutionReport(&report)
		return report, err
	}
	if !diagnostics.OK && diagnosticsHasErrorSeverity(diagnostics) {
		err := fmt.Errorf("memory: reconcile diagnostics failed: %s", diagnostics.Message)
		report.Status = LifecycleStatusFailed
		if report.Message == "" || report.Message == "diagnostics checks found missing dependencies" {
			report.Message = err.Error()
		}
		finalizeLifecycleExecutionReport(&report)
		return report, err
	}

	report.Status = LifecycleStatusCompleted
	if report.Message == "" {
		report.Message = "reconcile diagnostics completed"
	}
	finalizeLifecycleExecutionReport(&report)
	return report, nil
}

func applyDiagnosticsToLifecycleReport(report *LifecycleExecutionReport, diagnostics DiagnosticReport) {
	report.Accepted = true
	report.Supported = true
	report.Message = diagnostics.Message
	report.Checkpoint = map[string]any{
		"ready":         diagnostics.Ready,
		"ok":            diagnostics.OK,
		"check_count":   len(diagnostics.Checks),
		"warning_count": len(diagnostics.Warnings),
	}
	errorCount := 0
	repairHintCount := 0
	for _, check := range diagnostics.Checks {
		step := LifecycleStep{
			Name:      check.Name,
			Status:    LifecycleStatusCompleted,
			Planned:   true,
			Completed: true,
			Message:   check.Message,
			Details: map[string]any{
				"diagnostic_status":   string(check.Status),
				"diagnostic_severity": string(check.Severity),
				"ok":                  check.OK,
			},
		}
		if check.Capability != "" {
			step.Details["capability"] = string(check.Capability)
		}
		if check.Target != (LifecycleTarget{}) {
			step.Details["target"] = check.Target
		}
		if check.RepairHint != "" {
			step.Details["repair_hint"] = check.RepairHint
			repairHintCount++
		}
		for key, value := range check.Details {
			step.Details[key] = value
		}
		if check.Severity == DiagnosticSeverityError && !check.OK {
			errorCount++
			step.Status = LifecycleStatusFailed
			step.Completed = false
		}
		report.Steps = append(report.Steps, step)
	}
	report.Checkpoint["error_count"] = errorCount
	report.Checkpoint["repair_hint_count"] = repairHintCount
}

func diagnosticsHasErrorSeverity(report DiagnosticReport) bool {
	for _, check := range report.Checks {
		if check.Severity == DiagnosticSeverityError && !check.OK {
			return true
		}
	}
	return false
}

func newLifecycleReportForJob(system *System, job LifecycleJob) LifecycleExecutionReport {
	now := time.Now()
	traceID := ensureTraceID(job.TraceID)
	action := LifecycleAction("")
	switch job.Kind {
	case LifecycleJobKindRebuild, LifecycleJobKindReconcile, LifecycleJobKindReload, LifecycleJobKindFreshnessCheck:
		action = LifecycleAction(job.Kind)
	}
	requestedAt := job.CreatedAt
	if requestedAt.IsZero() {
		requestedAt = now
	}
	operation := LifecycleOperation{
		ID:           job.OperationID,
		TraceID:      traceID,
		Action:       action,
		Scope:        job.Scope,
		Capabilities: cloneCapabilities(job.Capabilities),
		Documents:    cloneDocumentTargets(job.Documents),
		Reason:       strings.TrimSpace(job.Reason),
		RequestedAt:  requestedAt,
	}
	if system != nil {
		operation.PlanDigest = system.lifecyclePlanDigest()
	}
	operation.Targets = lifecycleTargetsForDocuments(operation.Documents)
	operation.IdempotencyKey = lifecycleIdempotencyKey(operation, "")
	return LifecycleExecutionReport{
		TraceID:    traceID,
		Operation:  operation,
		Status:     LifecycleStatusPlanned,
		JobID:      job.ID,
		RunID:      lifecycleRunIDForOperation(job.OperationID),
		StartedAt:  now,
		Checkpoint: cloneCheckpoint(job.Checkpoint),
	}
}

func normalizeLifecycleReportForJob(system *System, job LifecycleJob, report LifecycleExecutionReport) LifecycleExecutionReport {
	traceID := report.TraceID
	if traceID == "" {
		traceID = report.Operation.TraceID
	}
	if traceID == "" {
		traceID = job.TraceID
	}
	traceID = ensureTraceID(traceID)
	report.TraceID = traceID
	if report.Operation.ID == "" {
		report.Operation.ID = job.OperationID
	}
	report.Operation.TraceID = traceID
	if report.Operation.Action == "" {
		switch job.Kind {
		case LifecycleJobKindRebuild, LifecycleJobKindReconcile, LifecycleJobKindReload, LifecycleJobKindFreshnessCheck:
			report.Operation.Action = LifecycleAction(job.Kind)
		}
	}
	if report.Operation.Scope.IsZero() {
		report.Operation.Scope = job.Scope
	}
	if report.Operation.Capabilities == nil {
		report.Operation.Capabilities = cloneCapabilities(job.Capabilities)
	}
	if report.Operation.Documents == nil {
		report.Operation.Documents = cloneDocumentTargets(job.Documents)
	}
	if report.Operation.Targets == nil {
		report.Operation.Targets = lifecycleTargetsForDocuments(report.Operation.Documents)
	}
	if report.Operation.Reason == "" {
		report.Operation.Reason = strings.TrimSpace(job.Reason)
	}
	if report.Operation.RequestedAt.IsZero() {
		report.Operation.RequestedAt = job.CreatedAt
		if report.Operation.RequestedAt.IsZero() {
			report.Operation.RequestedAt = time.Now()
		}
	}
	if system != nil && report.Operation.PlanDigest == "" {
		report.Operation.PlanDigest = system.lifecyclePlanDigest()
	}
	if report.Operation.IdempotencyKey == "" {
		report.Operation.IdempotencyKey = lifecycleIdempotencyKey(report.Operation, "")
	}
	if report.JobID == "" {
		report.JobID = job.ID
	}
	if report.RunID == "" {
		report.RunID = lifecycleRunIDForOperation(job.OperationID)
	}
	if report.Checkpoint == nil {
		report.Checkpoint = cloneCheckpoint(job.Checkpoint)
	}
	return report
}

func failLifecycleReport(report *LifecycleExecutionReport, err error) {
	report.Accepted = true
	report.Status = LifecycleStatusFailed
	if errdefs.IsNotAvailable(err) {
		report.Status = LifecycleStatusUnsupported
		report.Supported = false
	}
	if err != nil {
		report.Message = err.Error()
	}
	finalizeLifecycleExecutionReport(report)
}
