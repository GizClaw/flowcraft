package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

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
	Scope        Scope
	Capabilities []Capability
	Documents    []DocumentTarget
	DryRun       bool
	Reason       string
}

// RebuildReport describes the control-plane work planned or accepted for a rebuild.
type RebuildReport struct {
	LifecycleReport
}

// RebuildResult reports whether rebuild was handled by this facade.
type RebuildResult struct {
	RebuildReport
}

// ReconcileRequest describes a requested reconciliation scope.
type ReconcileRequest struct {
	Scope        Scope
	Capabilities []Capability
	DryRun       bool
	Reason       string
}

// ReconcileReport describes the control-plane work planned or accepted for reconciliation.
type ReconcileReport struct {
	LifecycleReport
}

// ReconcileResult reports whether reconciliation was handled by this facade.
type ReconcileResult struct {
	ReconcileReport
}

// ReloadRequest describes a requested lifecycle reload scope.
type ReloadRequest struct {
	Scope        Scope
	Capabilities []Capability
	Documents    []DocumentTarget
	DryRun       bool
	Reason       string
}

// ReloadReport describes the control-plane work planned or accepted for reload.
type ReloadReport struct {
	LifecycleReport
}

// ReloadResult reports whether reload was handled by this facade.
type ReloadResult struct {
	ReloadReport
}

// FreshnessRequest describes a requested lifecycle freshness check scope.
type FreshnessRequest struct {
	Scope        Scope
	Capabilities []Capability
	Documents    []DocumentTarget
	DryRun       bool
	Reason       string
}

// FreshnessReport describes the control-plane work planned or accepted for freshness.
type FreshnessReport struct {
	LifecycleReport
	Ready       bool
	OK          bool
	Checks      []DiagnosticCheck
	Warnings    []string
	Diagnostics DiagnosticReport
}

// FreshnessResult reports whether freshness checking was handled by this facade.
type FreshnessResult struct {
	FreshnessReport
}

// LifecycleReport is the shared substrate report shape for lifecycle actions.
type LifecycleReport struct {
	Action       string
	Scope        Scope
	Capabilities []Capability
	Documents    []DocumentTarget
	DryRun       bool
	Accepted     bool
	Supported    bool
	Job          JobHandle
	Reason       string
	Message      string
	Steps        []LifecycleStep
}

// LifecycleStep reports one planned or completed lifecycle substrate step.
type LifecycleStep struct {
	Name      string
	Planned   bool
	Completed bool
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
		add("scheduler", r.scheduler != nil, dependencyMessage("Scheduler", r.scheduler != nil))
	}
	return report, nil
}

// QueueStats returns scheduler counters. Sync-only systems without a scheduler
// report an empty queue.
func (r *System) QueueStats(ctx context.Context) (QueueStats, error) {
	if r == nil || r.inner == nil {
		return QueueStats{}, errdefs.NotAvailablef("memory: system is not configured")
	}
	if r.scheduler == nil {
		return QueueStats{}, nil
	}
	return r.scheduler.Stats(ctx)
}

// RunOnce executes one pending async memory job.
func (r *System) RunOnce(ctx context.Context) (JobResult, error) {
	if r == nil || r.inner == nil {
		err := errdefs.NotAvailablef("memory: system is not configured")
		return JobResult{Error: err.Error()}, err
	}
	if r.scheduler == nil {
		err := errdefs.NotAvailablef("memory: scheduler is not configured")
		return JobResult{Error: err.Error()}, err
	}
	return r.scheduler.RunOnce(ctx)
}

// Drain executes pending async memory jobs until the queue is empty.
func (r *System) Drain(ctx context.Context) error {
	if r == nil || r.inner == nil {
		return errdefs.NotAvailablef("memory: system is not configured")
	}
	if r.scheduler == nil {
		return nil
	}
	return r.scheduler.Drain(ctx)
}

// Shutdown stops the scheduler and releases resources owned by the system.
func (r *System) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	var err error
	if r.scheduler != nil {
		err = errors.Join(err, r.scheduler.Shutdown(ctx))
	}
	if r.inner != nil {
		err = errors.Join(err, r.inner.Close())
	}
	return err
}

// Rebuild plans or enqueues derived-view rebuild substrate work.
func (r *System) Rebuild(ctx context.Context, req RebuildRequest) (RebuildResult, error) {
	report, err := r.dispatchLifecycle(ctx, lifecycleStageRebuild, lifecycleDispatchRequest{
		Scope:        req.Scope,
		Capabilities: req.Capabilities,
		Documents:    req.Documents,
		DryRun:       req.DryRun,
		Reason:       req.Reason,
	})
	return RebuildResult{RebuildReport: RebuildReport{LifecycleReport: report}}, err
}

// Reconcile plans or enqueues cross-view reconciliation substrate work.
func (r *System) Reconcile(ctx context.Context, req ReconcileRequest) (ReconcileResult, error) {
	report, err := r.dispatchLifecycle(ctx, lifecycleStageReconcile, lifecycleDispatchRequest{
		Scope:        req.Scope,
		Capabilities: req.Capabilities,
		DryRun:       req.DryRun,
		Reason:       req.Reason,
	})
	return ReconcileResult{ReconcileReport: ReconcileReport{LifecycleReport: report}}, err
}

// Reload plans or enqueues lifecycle reload substrate work.
func (r *System) Reload(ctx context.Context, req ReloadRequest) (ReloadResult, error) {
	report, err := r.dispatchLifecycle(ctx, lifecycleStageReload, lifecycleDispatchRequest{
		Scope:        req.Scope,
		Capabilities: req.Capabilities,
		Documents:    req.Documents,
		DryRun:       req.DryRun,
		Reason:       req.Reason,
	})
	return ReloadResult{ReloadReport: ReloadReport{LifecycleReport: report}}, err
}

// Freshness plans or enqueues lifecycle freshness-check substrate work.
func (r *System) Freshness(ctx context.Context, req FreshnessRequest) (FreshnessResult, error) {
	report, err := r.dispatchLifecycle(ctx, lifecycleStageFreshnessCheck, lifecycleDispatchRequest{
		Scope:        req.Scope,
		Capabilities: req.Capabilities,
		Documents:    req.Documents,
		DryRun:       req.DryRun,
		Reason:       req.Reason,
	})
	freshness := FreshnessReport{LifecycleReport: report}
	if err != nil {
		return FreshnessResult{FreshnessReport: freshness}, err
	}
	diagnostics, err := r.buildDiagnosticReport(ctx, DiagnosticRequest{
		Scope:        report.Scope,
		Capabilities: report.Capabilities,
		Documents:    report.Documents,
		Stage:        diagnosticStageFreshness,
	}, false)
	if err != nil {
		return FreshnessResult{FreshnessReport: freshness}, err
	}
	freshness.Ready = diagnostics.Ready
	freshness.OK = diagnostics.OK
	freshness.Checks = cloneDiagnosticChecks(diagnostics.Checks)
	freshness.Warnings = append([]string(nil), diagnostics.Warnings...)
	freshness.Diagnostics = diagnostics
	return FreshnessResult{FreshnessReport: freshness}, nil
}

type lifecycleDispatchRequest struct {
	Scope        Scope
	Capabilities []Capability
	Documents    []DocumentTarget
	DryRun       bool
	Reason       string
}

func (r *System) dispatchLifecycle(ctx context.Context, action string, req lifecycleDispatchRequest) (LifecycleReport, error) {
	report := newLifecycleReport(action, req)
	if r == nil || r.inner == nil {
		report.Message = "system is not configured"
		return report, errdefs.NotAvailablef("memory: system is not configured")
	}

	scope := normalizeScope(req.Scope)
	report.Scope = scope
	if err := scope.Validate(); err != nil {
		report.Message = "invalid scope"
		return report, errdefs.Validationf("memory: invalid scope: %w", err)
	}
	documents, err := normalizeDocumentTargets(scope, req.Documents)
	if err != nil {
		report.Message = "invalid document targets"
		return report, err
	}
	report.Documents = documents
	if !r.lifecycleActionDeclared(action) {
		report.Message = fmt.Sprintf("lifecycle action %q is not declared by the plan", action)
		return report, errdefs.NotAvailablef("memory: lifecycle action %q is not declared by the plan", action)
	}

	report.Supported = true
	if handled, err := r.dispatchDocumentChunkLifecycle(ctx, action, &report); handled || err != nil {
		return report, err
	}
	if req.DryRun {
		report.Accepted = true
		report.Message = fmt.Sprintf("%s lifecycle substrate is planned; business execution is not implemented in Stage 1", action)
		return report, nil
	}
	if r.scheduler == nil {
		report.Message = "scheduler is not configured"
		return report, errdefs.NotAvailablef("memory: scheduler is not configured for lifecycle action %q", action)
	}

	jobScope := scope
	jobCapabilities := cloneCapabilities(report.Capabilities)
	jobReason := report.Reason
	job := Job{
		Kind:         action,
		Scope:        jobScope,
		Capabilities: jobCapabilities,
		Reason:       jobReason,
		Stages:       []PlannedStage{{Name: action}},
	}
	job.run = func(ctx context.Context) error {
		return runLifecycleSubstrateJob(ctx)
	}
	handle, err := r.scheduler.Enqueue(ctx, job)
	if err != nil {
		report.Message = err.Error()
		return report, err
	}
	report.Accepted = true
	report.Job = handle
	report.Message = fmt.Sprintf("%s lifecycle substrate job enqueued; business execution is not implemented in Stage 1", action)
	return report, nil
}

func newLifecycleReport(action string, req lifecycleDispatchRequest) LifecycleReport {
	return LifecycleReport{
		Action:       action,
		Capabilities: cloneCapabilities(req.Capabilities),
		Documents:    cloneDocumentTargets(req.Documents),
		DryRun:       req.DryRun,
		Reason:       strings.TrimSpace(req.Reason),
		Steps: []LifecycleStep{{
			Name:    "lifecycle_substrate",
			Planned: true,
			Message: "Stage 1 only validates and schedules lifecycle substrate work; business execution is not implemented",
		}},
	}
}

func (r *System) dispatchDocumentChunkLifecycle(ctx context.Context, action string, report *LifecycleReport) (bool, error) {
	if action != lifecycleStageReload && action != lifecycleStageRebuild {
		return false, nil
	}
	if !capabilitySelected(report.Capabilities, CapabilityDocumentChunks) {
		return false, nil
	}

	report.Steps = nil
	if len(report.Documents) == 0 {
		report.Accepted = true
		report.Message = "document_chunks reload/rebuild requires explicit document targets; no full scan was planned"
		report.Steps = append(report.Steps, LifecycleStep{
			Name:    "document_chunks.targets",
			Planned: true,
			Message: "no document targets supplied; full document scans are intentionally unsupported",
			Details: map[string]any{
				"capability": string(CapabilityDocumentChunks),
			},
		})
		return true, nil
	}

	if !r.writeAvailable[CapabilityDocumentChunks] {
		report.Message = "document_chunks capability is not configured for writes"
		report.Steps = append(report.Steps, LifecycleStep{
			Name:    "document_chunks.configure",
			Planned: true,
			Message: "DocumentStore, ChunkStore, and DocumentChunker are required for targeted reload/rebuild",
			Details: map[string]any{
				"capability": string(CapabilityDocumentChunks),
			},
		})
		return true, errdefs.NotAvailablef("memory: document_chunks capability is not configured for targeted %s", action)
	}

	if report.DryRun {
		report.Accepted = true
		report.Message = fmt.Sprintf("%s planned for %d document target(s)", action, len(report.Documents))
		for _, target := range report.Documents {
			report.Steps = append(report.Steps, LifecycleStep{
				Name:    "document_chunks.target",
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

	for _, target := range report.Documents {
		targetScope := report.Scope
		targetScope.DatasetID = target.DatasetID
		namespace, err := r.scopedWriteNamespace(CapabilityDocumentChunks, targetScope)
		if err != nil {
			report.Message = err.Error()
			return true, err
		}
		chunks, err := r.inner.IndexDocument(ctx, targetScope, target.DocumentID, namespace)
		if err != nil {
			report.Message = err.Error()
			return true, err
		}
		report.Steps = append(report.Steps, LifecycleStep{
			Name:      "document_chunks.target",
			Planned:   true,
			Completed: true,
			Message:   fmt.Sprintf("re-indexed document %s into %d chunk(s)", documentTargetLabel(target), len(chunks)),
			Details: map[string]any{
				"capability":       string(CapabilityDocumentChunks),
				"dataset_id":       target.DatasetID,
				"document_id":      target.DocumentID,
				"chunk_count":      len(chunks),
				"scoped_namespace": namespace,
				"runtime_id":       targetScope.RuntimeID,
				"user_id":          targetScope.UserID,
				"conversation_id":  targetScope.ConversationID,
			},
		})
	}
	report.Accepted = true
	report.Message = fmt.Sprintf("%s completed for %d document target(s)", action, len(report.Documents))
	return true, nil
}

func (r *System) lifecycleActionDeclared(action string) bool {
	for _, stage := range r.plan.Lifecycle {
		if stage.Name == action {
			return true
		}
	}
	return false
}

func runLifecycleSubstrateJob(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
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
