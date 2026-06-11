package memory

import (
	"context"
	"errors"
	"fmt"

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
	Scope Scope
}

// RebuildResult reports whether rebuild was handled by this facade.
type RebuildResult struct {
	Supported bool
	Message   string
}

// ReconcileRequest describes a requested reconciliation scope.
type ReconcileRequest struct {
	Scope Scope
}

// ReconcileResult reports whether reconciliation was handled by this facade.
type ReconcileResult struct {
	Supported bool
	Message   string
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

// Rebuild is reserved for future derived-view rebuild orchestration.
func (r *System) Rebuild(_ context.Context, _ RebuildRequest) (RebuildResult, error) {
	result := RebuildResult{
		Supported: false,
		Message:   "rebuild is not available in this minimal lifecycle control plane",
	}
	return result, errdefs.NotAvailablef("memory: rebuild is not available")
}

// Reconcile is reserved for future cross-view reconciliation orchestration.
func (r *System) Reconcile(_ context.Context, _ ReconcileRequest) (ReconcileResult, error) {
	result := ReconcileResult{
		Supported: false,
		Message:   "reconcile is not available in this minimal lifecycle control plane",
	}
	return result, errdefs.NotAvailablef("memory: reconcile is not available")
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
