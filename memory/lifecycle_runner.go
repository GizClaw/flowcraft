package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/internal/projectors"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/views/recent"
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
	if r != nil && r.hasDerivedViewLifecycleRunner() {
		runner := derivedViewsLifecycleRunner{}
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

func (r *System) hasDerivedViewLifecycleRunner() bool {
	return r != nil &&
		(r.lifecycleCapabilityAvailable(CapabilityDocumentChunks) ||
			r.lifecycleCapabilityAvailable(CapabilityMessageIndex) ||
			r.lifecycleCapabilityAvailable(CapabilitySummaryDAG))
}

func (r *System) lifecycleCapabilityAvailable(capability Capability) bool {
	if r == nil {
		return false
	}
	switch capability {
	case CapabilityDocumentChunks:
		return r.writeAvailable[CapabilityDocumentChunks]
	case CapabilityMessageIndex:
		return r.writeAvailable[CapabilityMessageIndex]
	case CapabilitySummaryDAG:
		return r.writeAvailable[CapabilitySummaryDAG] && r.readAvailable[CapabilitySummaryDAG]
	default:
		return false
	}
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

	err := req.System.executeWriteStages(ctx, req.Job.Stages, req.Job.Window, req.Job.Scope)
	if err != nil {
		failLifecycleReport(&report, err)
		return report, err
	}

	report.Accepted = true
	report.Supported = true
	report.Status = LifecycleStatusCompleted
	report.Message = fmt.Sprintf("write_chain completed %d stage(s)", len(req.Job.Stages))
	report.Checkpoint = map[string]any{
		"stages": len(req.Job.Stages),
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

const lifecycleMessageIndexBatchSize = 512

const lifecycleProjectionCleanupListPageSize = lifecycleMessageIndexBatchSize

type derivedViewsLifecycleRunner struct{}

func (derivedViewsLifecycleRunner) Run(ctx context.Context, req LifecycleRunRequest) (LifecycleExecutionReport, error) {
	report := newLifecycleReportForJob(req.System, req.Job)
	if req.Job.Kind != LifecycleJobKindReload && req.Job.Kind != LifecycleJobKindRebuild {
		err := errdefs.NotAvailablef("memory: derived-view runner cannot execute job kind %q", req.Job.Kind)
		failLifecycleReport(&report, err)
		return report, err
	}
	if req.System == nil || req.System.inner == nil {
		err := errdefs.NotAvailablef("memory: system is not configured")
		failLifecycleReport(&report, err)
		return report, err
	}
	capabilities := selectedDerivedViewLifecycleCapabilities(req.Job.Capabilities)
	if len(capabilities) == 0 {
		err := errdefs.NotAvailablef("memory: no derived-view lifecycle capabilities requested for job kind %q", req.Job.Kind)
		failLifecycleReport(&report, err)
		return report, err
	}

	var runErr error
	completed := 0
	skipped := 0
	for _, capability := range capabilities {
		beforeSteps := len(report.Steps)
		var err error
		switch capability {
		case CapabilityDocumentChunks:
			err = runDocumentChunksLifecycle(ctx, req, &report)
		case CapabilityMessageIndex:
			err = runMessageIndexLifecycle(ctx, req, &report)
		case CapabilitySummaryDAG:
			err = runSummaryDAGLifecycle(ctx, req, &report)
		}
		for _, step := range report.Steps[beforeSteps:] {
			if step.Completed || step.Status == LifecycleStatusCompleted {
				completed++
			}
			if step.Skipped || step.Status == LifecycleStatusSkipped {
				skipped++
			}
		}
		if err != nil {
			runErr = errors.Join(runErr, err)
		}
	}

	report.Accepted = true
	report.Supported = true
	runCheckpoint := map[string]any{
		"capabilities": len(capabilities),
		"completed":    completed,
		"skipped":      skipped,
		"failed":       len(report.TargetErrors),
	}
	if checkpointBool(report.Checkpoint, "document_scan") {
		runCheckpoint["document_scan_repaired"] = completed
		runCheckpoint["document_scan_failed"] = len(report.TargetErrors)
	}
	report.Checkpoint = mergeLifecycleCheckpoints(report.Checkpoint, runCheckpoint)
	applyLifecycleCleanupCheckpoint(&report)
	if runErr != nil {
		report.Status = LifecycleStatusFailed
		report.Message = fmt.Sprintf("%s failed for derived-view lifecycle capability(s)", req.Job.Kind)
	} else if completed > 0 {
		report.Status = LifecycleStatusCompleted
		report.Message = fmt.Sprintf("%s completed for %d derived-view capability(s)", req.Job.Kind, len(capabilities))
	} else {
		report.Status = LifecycleStatusSkipped
		report.Message = fmt.Sprintf("%s skipped; scoped targets were not sufficient", req.Job.Kind)
	}
	finalizeLifecycleExecutionReport(&report)
	return report, runErr
}

func runDocumentChunksLifecycle(ctx context.Context, req LifecycleRunRequest, report *LifecycleExecutionReport) error {
	if !req.System.lifecycleCapabilityAvailable(CapabilityDocumentChunks) {
		err := errdefs.NotAvailablef("memory: document_chunks capability is not configured for targeted %s", req.Job.Kind)
		appendLifecycleCapabilityFailure(report, "document_chunks.configure", CapabilityDocumentChunks, "document_chunks capability is not configured", err)
		return err
	}
	if len(req.Job.Documents) == 0 {
		report.Steps = append(report.Steps, LifecycleStep{
			Name:    "document_chunks.targets",
			Status:  LifecycleStatusSkipped,
			Planned: true,
			Skipped: true,
			Message: "no document targets supplied",
			Details: map[string]any{
				"capability": string(CapabilityDocumentChunks),
			},
		})
		return nil
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
	return runErr
}

func runMessageIndexLifecycle(ctx context.Context, req LifecycleRunRequest, report *LifecycleExecutionReport) error {
	scope := req.Job.Scope
	step := lifecycleConversationStep("message_index.conversation", CapabilityMessageIndex, scope)
	if !req.System.lifecycleCapabilityAvailable(CapabilityMessageIndex) {
		err := errdefs.NotAvailablef("memory: message_index capability is not configured for scoped %s", req.Job.Kind)
		failLifecycleStep(&step, "message_index capability is not configured", err)
		report.Steps = append(report.Steps, step)
		return err
	}
	if scope.ConversationID == "" {
		step.Status = LifecycleStatusSkipped
		step.Skipped = true
		step.Message = "conversation_id is required; full message scans are intentionally unsupported"
		report.Steps = append(report.Steps, step)
		return nil
	}
	namespace, err := req.System.scopedWriteNamespace(CapabilityMessageIndex, scope)
	if err != nil {
		failLifecycleStep(&step, "message_index scoped namespace failed", err)
		report.Steps = append(report.Steps, step)
		return err
	}
	step.Details["scoped_namespace"] = namespace
	cleanup := cleanupScopedProjection(ctx, req.System, CapabilityMessageIndex, scope, namespace)
	var existingProjections projectionSnapshotResult
	if cleanup.needsPostRebuildFallback() {
		existingProjections = snapshotScopedProjection(ctx, req.System, CapabilityMessageIndex, scope, namespace)
		step.Details["stale_projection_scan"] = existingProjections.status
		step.Details["stale_projection_candidates"] = len(existingProjections.ids)
		if existingProjections.degraded {
			cleanup = degradedProjectionCleanup(existingProjections.status, existingProjections.err)
		} else if existingProjections.err != nil {
			failLifecycleStep(&step, "message_index stale projection scan failed", existingProjections.err)
			report.Steps = append(report.Steps, step)
			return existingProjections.err
		}
	}
	if cleanup.err != nil && !cleanup.degraded {
		applyProjectionCleanupDetails(&step, cleanup)
		failLifecycleStep(&step, "message_index stale cleanup failed", cleanup.err)
		report.Steps = append(report.Steps, step)
		return cleanup.err
	}
	count, liveIDs, err := indexMessagesInBatches(ctx, req.System, scope, namespace)
	if err != nil {
		applyProjectionCleanupDetails(&step, cleanup)
		failLifecycleStep(&step, "message_index rebuild failed", err)
		report.Steps = append(report.Steps, step)
		return err
	}
	if cleanup.needsPostRebuildFallback() && !existingProjections.degraded {
		cleanup = cleanupStaleScopedProjection(ctx, req.System, namespace, existingProjections.ids, liveIDs)
		if cleanup.err != nil && !cleanup.degraded {
			applyProjectionCleanupDetails(&step, cleanup)
			failLifecycleStep(&step, "message_index stale cleanup failed", cleanup.err)
			report.Steps = append(report.Steps, step)
			return cleanup.err
		}
	}
	applyProjectionCleanupDetails(&step, cleanup)
	step.Status = LifecycleStatusCompleted
	step.Completed = true
	step.Message = fmt.Sprintf("rebuilt message_index for conversation %s from %d message(s)", scope.ConversationID, count)
	step.Details["message_count"] = count
	report.Steps = append(report.Steps, step)
	return nil
}

func runSummaryDAGLifecycle(ctx context.Context, req LifecycleRunRequest, report *LifecycleExecutionReport) error {
	scope := req.Job.Scope
	step := lifecycleConversationStep("summary_dag.conversation", CapabilitySummaryDAG, scope)
	if !req.System.lifecycleCapabilityAvailable(CapabilitySummaryDAG) {
		err := errdefs.NotAvailablef("memory: summary_dag capability is not configured for scoped %s", req.Job.Kind)
		failLifecycleStep(&step, "summary_dag capability is not configured", err)
		report.Steps = append(report.Steps, step)
		return err
	}
	if scope.ConversationID == "" {
		step.Status = LifecycleStatusSkipped
		step.Skipped = true
		step.Message = "conversation_id is required; full summary scans are intentionally unsupported"
		report.Steps = append(report.Steps, step)
		return nil
	}
	namespace, err := req.System.scopedWriteNamespace(CapabilitySummaryDAG, scope)
	if err != nil {
		failLifecycleStep(&step, "summary_dag scoped namespace failed", err)
		report.Steps = append(report.Steps, step)
		return err
	}
	step.Details["scoped_namespace"] = namespace
	existingProjections := snapshotScopedProjection(ctx, req.System, CapabilitySummaryDAG, scope, namespace)
	step.Details["stale_projection_scan"] = existingProjections.status
	step.Details["stale_projection_candidates"] = len(existingProjections.ids)
	if existingProjections.degraded {
		step.Details["stale_projection_scan_degraded"] = true
		if existingProjections.err != nil {
			step.Details["stale_projection_scan_error"] = existingProjections.err.Error()
		}
	} else if existingProjections.err != nil {
		failLifecycleStep(&step, "summary_dag stale projection scan failed", existingProjections.err)
		report.Steps = append(report.Steps, step)
		return existingProjections.err
	}
	if req.System.deps.SummaryStore == nil {
		err := errdefs.NotAvailablef("memory: summary store is not configured")
		failLifecycleStep(&step, "summary_dag store is not configured", err)
		report.Steps = append(report.Steps, step)
		return err
	}
	existingCanonical, err := req.System.deps.SummaryStore.ListNodes(ctx, scope, recent.ListOptions{})
	if err != nil {
		step.Details["stale_store_scan"] = "list_failed"
		failLifecycleStep(&step, "summary_dag canonical node scan failed", err)
		report.Steps = append(report.Steps, step)
		return err
	}
	step.Details["stale_store_scan"] = "list"
	step.Details["stale_store_scan_candidates"] = len(existingCanonical)
	nodes, err := req.System.inner.BuildSummaryDAG(ctx, recent.WindowRequest{
		Scope: scope,
		Budget: &recent.WindowBudget{
			MaxMessages: -1,
		},
	}, namespace)
	if err != nil {
		step.Details["stale_cleanup"] = "skipped_rebuild_failed"
		step.Details["stale_store_cleanup"] = "skipped_rebuild_failed"
		step.Details["canonical_cleanup_mode"] = "skipped_rebuild_failed"
		step.Details["canonical_stale_candidates"] = 0
		step.Details["canonical_stale_deleted"] = 0
		failLifecycleStep(&step, "summary_dag rebuild failed", err)
		report.Steps = append(report.Steps, step)
		return err
	}
	canonicalCleanup := cleanupStaleSummaryCanonicalNodes(ctx, req.System.deps.SummaryStore, scope, existingCanonical, nodes)
	applySummaryCanonicalCleanupDetails(&step, canonicalCleanup)
	if canonicalCleanup.err != nil && !canonicalCleanup.degraded {
		failLifecycleStep(&step, "summary_dag stale canonical cleanup failed", canonicalCleanup.err)
		report.Steps = append(report.Steps, step)
		return canonicalCleanup.err
	}
	projectionCleanup := projectionCleanupResult{status: "skipped_degraded", mode: "skipped_degraded"}
	if !existingProjections.degraded {
		projectionCleanup = cleanupStaleScopedProjection(ctx, req.System, namespace, existingProjections.ids, summaryProjectionLiveIDs(nodes))
	}
	applyProjectionCleanupDetails(&step, projectionCleanup)
	if projectionCleanup.err != nil && !projectionCleanup.degraded {
		failLifecycleStep(&step, "summary_dag stale projection cleanup failed", projectionCleanup.err)
		report.Steps = append(report.Steps, step)
		return projectionCleanup.err
	}
	step.Status = LifecycleStatusCompleted
	step.Completed = true
	step.Message = fmt.Sprintf("rebuilt summary_dag for conversation %s into %d node(s)", scope.ConversationID, len(nodes))
	step.Details["node_count"] = len(nodes)
	report.Steps = append(report.Steps, step)
	return nil
}

func selectedDerivedViewLifecycleCapabilities(capabilities []Capability) []Capability {
	out := make([]Capability, 0, len(capabilities))
	seen := map[Capability]bool{}
	for _, capability := range capabilities {
		switch capability {
		case CapabilityDocumentChunks, CapabilityMessageIndex, CapabilitySummaryDAG, CapabilityEntityFactIndex:
			if !seen[capability] {
				out = append(out, capability)
				seen[capability] = true
			}
		}
	}
	return out
}

func lifecycleDerivedViewPlannedStages(capabilities []Capability) []PlannedStage {
	stages := make([]PlannedStage, 0, len(capabilities))
	for _, capability := range capabilities {
		stage := PlannedStage{Capability: capability}
		switch capability {
		case CapabilityDocumentChunks:
			stage.Name = writeStageChunkDocument
		case CapabilityMessageIndex:
			stage.Name = writeStageIndexMessages
		case CapabilitySummaryDAG:
			stage.Name = writeStageBuildSummaryDAG
		case CapabilityEntityFactIndex:
			stage.Name = writeStageBuildEntityFacts
		}
		if stage.Name != "" {
			stages = append(stages, stage)
		}
	}
	return stages
}

func indexMessagesInBatches(ctx context.Context, system *System, scope Scope, namespace string) (int, map[string]struct{}, error) {
	total := 0
	liveIDs := map[string]struct{}{}
	var afterSeq uint64
	for {
		messages, err := system.inner.IndexMessages(ctx, recent.WindowRequest{
			Scope:    scope,
			AfterSeq: afterSeq,
			Budget: &recent.WindowBudget{
				MaxMessages: lifecycleMessageIndexBatchSize,
			},
		}, namespace)
		if err != nil {
			return total, liveIDs, err
		}
		if len(messages) == 0 {
			return total, liveIDs, nil
		}
		total += len(messages)
		for _, msg := range messages {
			records, err := projectors.SourceMessageRecords(scope, msg)
			if err != nil {
				return total, liveIDs, err
			}
			for _, record := range records {
				liveIDs[record.ID] = struct{}{}
			}
		}
		lastSeq := messages[len(messages)-1].Seq
		if lastSeq <= afterSeq {
			return total, liveIDs, errdefs.Validationf("memory: message_index rebuild did not advance past seq %d", afterSeq)
		}
		afterSeq = lastSeq
		if len(messages) < lifecycleMessageIndexBatchSize {
			return total, liveIDs, nil
		}
	}
}

type projectionCleanupResult struct {
	status   string
	mode     string
	deleted  int64
	degraded bool
	err      error
}

type projectionSnapshotResult struct {
	status   string
	ids      []string
	degraded bool
	err      error
}

type summaryCanonicalCleanupResult struct {
	status     string
	mode       string
	candidates int
	deleted    int
	degraded   bool
	err        error
}

func (r projectionCleanupResult) needsPostRebuildFallback() bool {
	return r.mode == "list_delete" && r.status == "list_delete_pending"
}

func cleanupScopedProjection(ctx context.Context, system *System, capability Capability, scope Scope, namespace string) projectionCleanupResult {
	if strings.TrimSpace(namespace) == "" {
		return projectionCleanupResult{status: "projection_not_configured", mode: "skipped_degraded", degraded: true}
	}
	index := system.RetrievalIndex()
	deletable, ok := index.(retrieval.DeletableByFilter)
	if !ok {
		return projectionCleanupResult{status: "list_delete_pending", mode: "list_delete"}
	}
	deleted, err := deletable.DeleteByFilter(ctx, namespace, lifecycleProjectionCleanupFilter(capability, scope))
	if err != nil {
		if projectionCleanupUnavailable(err) {
			return projectionCleanupResult{status: "list_delete_pending", mode: "list_delete", degraded: true}
		}
		return projectionCleanupResult{status: "delete_by_filter_failed", mode: "delete_by_filter", err: err}
	}
	return projectionCleanupResult{status: "delete_by_filter", mode: "delete_by_filter", deleted: deleted}
}

func snapshotScopedProjection(ctx context.Context, system *System, capability Capability, scope Scope, namespace string) projectionSnapshotResult {
	if strings.TrimSpace(namespace) == "" {
		return projectionSnapshotResult{status: "projection_not_configured", degraded: true}
	}
	index := system.RetrievalIndex()
	if index == nil {
		return projectionSnapshotResult{status: "projection_not_configured", degraded: true}
	}
	filter := lifecycleProjectionCleanupFilter(capability, scope)
	var ids []string
	var pageToken string
	for {
		resp, err := index.List(ctx, namespace, retrieval.ListRequest{
			Filter:    filter,
			PageSize:  lifecycleProjectionCleanupListPageSize,
			PageToken: pageToken,
			OrderBy:   retrieval.OrderByIDAsc,
			Project:   []string{"id"},
		})
		if err != nil {
			if projectionCleanupUnavailable(err) {
				return projectionSnapshotResult{status: "skipped_degraded", ids: ids, degraded: true, err: err}
			}
			return projectionSnapshotResult{status: "list_failed", err: err}
		}
		if resp == nil {
			return projectionSnapshotResult{status: "list_failed", err: errdefs.NotAvailablef("memory: projection list returned nil response")}
		}
		for _, doc := range resp.Items {
			if doc.ID != "" {
				ids = append(ids, doc.ID)
			}
		}
		if resp.NextPageToken == "" {
			return projectionSnapshotResult{status: "list", ids: ids}
		}
		pageToken = resp.NextPageToken
	}
}

func cleanupStaleScopedProjection(ctx context.Context, system *System, namespace string, existingIDs []string, liveIDs map[string]struct{}) projectionCleanupResult {
	if strings.TrimSpace(namespace) == "" {
		return projectionCleanupResult{status: "projection_not_configured", mode: "skipped_degraded", degraded: true}
	}
	var staleIDs []string
	for _, id := range existingIDs {
		if _, live := liveIDs[id]; live {
			continue
		}
		staleIDs = append(staleIDs, id)
	}
	if len(staleIDs) == 0 {
		return projectionCleanupResult{status: "list_delete", mode: "list_delete"}
	}
	index := system.RetrievalIndex()
	if index == nil {
		return projectionCleanupResult{status: "projection_not_configured", mode: "skipped_degraded", degraded: true}
	}
	if err := index.Delete(ctx, namespace, staleIDs); err != nil {
		if projectionCleanupUnavailable(err) {
			return degradedProjectionCleanup("skipped_degraded", err)
		}
		return projectionCleanupResult{status: "delete_by_id_failed", mode: "list_delete", err: err}
	}
	return projectionCleanupResult{status: "list_delete", mode: "list_delete", deleted: int64(len(staleIDs))}
}

func degradedProjectionCleanup(status string, err error) projectionCleanupResult {
	status = strings.TrimSpace(status)
	if status == "" {
		status = "skipped_degraded"
	}
	return projectionCleanupResult{status: status, mode: "skipped_degraded", degraded: true, err: err}
}

func projectionCleanupUnavailable(err error) bool {
	if err == nil {
		return false
	}
	return errdefs.IsNotAvailable(err)
}

func applyProjectionCleanupDetails(step *LifecycleStep, cleanup projectionCleanupResult) {
	if step == nil {
		return
	}
	if step.Details == nil {
		step.Details = map[string]any{}
	}
	status := strings.TrimSpace(cleanup.status)
	if status == "" {
		status = cleanup.mode
	}
	mode := strings.TrimSpace(cleanup.mode)
	if mode == "" {
		mode = status
	}
	step.Details["stale_cleanup"] = status
	step.Details["cleanup_mode"] = mode
	step.Details["stale_deleted"] = cleanup.deleted
	if cleanup.degraded {
		step.Details["cleanup_degraded"] = true
		if cleanup.err != nil {
			step.Details["cleanup_degraded_error"] = cleanup.err.Error()
		}
	}
}

func cleanupStaleSummaryCanonicalNodes(ctx context.Context, store recent.SummaryStore, scope Scope, existingNodes []recent.SummaryNode, liveNodes []recent.SummaryNode) summaryCanonicalCleanupResult {
	liveIDs := make(map[recent.NodeID]struct{}, len(liveNodes))
	for _, node := range liveNodes {
		if node.ID == "" {
			continue
		}
		liveIDs[node.ID] = struct{}{}
	}
	staleIDs := make([]recent.NodeID, 0, len(existingNodes))
	seen := map[recent.NodeID]struct{}{}
	for _, node := range existingNodes {
		if node.ID == "" {
			continue
		}
		if _, duplicate := seen[node.ID]; duplicate {
			continue
		}
		seen[node.ID] = struct{}{}
		if _, live := liveIDs[node.ID]; live {
			continue
		}
		staleIDs = append(staleIDs, node.ID)
	}

	deleter, ok := store.(recent.SummaryNodeDeleter)
	if !ok {
		return summaryCanonicalCleanupResult{
			status:     "skipped_degraded",
			mode:       "skipped_degraded",
			candidates: len(staleIDs),
			degraded:   true,
			err:        errdefs.NotAvailablef("memory: summary store does not support targeted node delete"),
		}
	}

	result := summaryCanonicalCleanupResult{
		status:     "delete_node",
		mode:       "delete_node",
		candidates: len(staleIDs),
	}
	for _, id := range staleIDs {
		if err := deleter.DeleteNode(ctx, scope, id); err != nil {
			if errdefs.IsNotAvailable(err) {
				result.status = "skipped_degraded"
				result.mode = "skipped_degraded"
				result.degraded = true
				result.err = err
				return result
			}
			result.status = "failed"
			result.mode = "failed"
			result.err = err
			return result
		}
		result.deleted++
	}
	return result
}

func applySummaryCanonicalCleanupDetails(step *LifecycleStep, cleanup summaryCanonicalCleanupResult) {
	if step == nil {
		return
	}
	if step.Details == nil {
		step.Details = map[string]any{}
	}
	status := strings.TrimSpace(cleanup.status)
	if status == "" {
		status = cleanup.mode
	}
	mode := strings.TrimSpace(cleanup.mode)
	if mode == "" {
		mode = status
	}
	step.Details["stale_store_cleanup"] = status
	step.Details["canonical_cleanup_mode"] = mode
	step.Details["canonical_stale_candidates"] = cleanup.candidates
	step.Details["canonical_stale_deleted"] = cleanup.deleted
	if cleanup.degraded {
		step.Details["canonical_cleanup_degraded"] = true
		if cleanup.err != nil {
			step.Details["canonical_cleanup_degraded_error"] = cleanup.err.Error()
		}
	}
	if cleanup.err != nil && !cleanup.degraded {
		step.Details["canonical_cleanup_error"] = cleanup.err.Error()
	}
}

func applyLifecycleCleanupCheckpoint(report *LifecycleExecutionReport) {
	if report == nil {
		return
	}
	modes := map[string]string{}
	canonicalModes := map[string]string{}
	canonicalDeleted := map[string]int{}
	canonicalCandidates := map[string]int{}
	for _, step := range report.Steps {
		if step.Details == nil {
			continue
		}
		capability, _ := step.Details["capability"].(string)
		mode, _ := step.Details["cleanup_mode"].(string)
		if capability == "" || mode == "" {
			if capability == "" {
				continue
			}
		} else {
			modes[capability] = mode
		}
		canonicalMode, _ := step.Details["canonical_cleanup_mode"].(string)
		if canonicalMode == "" {
			continue
		}
		canonicalModes[capability] = canonicalMode
		canonicalDeleted[capability] = checkpointDetailInt(step.Details["canonical_stale_deleted"])
		canonicalCandidates[capability] = checkpointDetailInt(step.Details["canonical_stale_candidates"])
	}
	if len(modes) == 0 && len(canonicalModes) == 0 {
		return
	}
	if report.Checkpoint == nil {
		report.Checkpoint = map[string]any{}
	}
	if len(modes) > 0 {
		report.Checkpoint["cleanup_modes"] = modes
	}
	if len(modes) == 1 {
		for _, mode := range modes {
			report.Checkpoint["cleanup_mode"] = mode
		}
	}
	if len(canonicalModes) > 0 {
		report.Checkpoint["canonical_cleanup_modes"] = canonicalModes
		report.Checkpoint["canonical_stale_deleted_by_capability"] = canonicalDeleted
		report.Checkpoint["canonical_stale_candidates_by_capability"] = canonicalCandidates
	}
	if len(canonicalModes) == 1 {
		for capability, mode := range canonicalModes {
			report.Checkpoint["canonical_cleanup_mode"] = mode
			report.Checkpoint["canonical_stale_deleted"] = canonicalDeleted[capability]
			report.Checkpoint["canonical_stale_candidates"] = canonicalCandidates[capability]
		}
	}
}

func checkpointDetailInt(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case uint64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

func summaryProjectionLiveIDs(nodes []recent.SummaryNode) map[string]struct{} {
	out := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		record, err := projectors.SummaryNode(node)
		if err != nil {
			continue
		}
		out[record.ID] = struct{}{}
	}
	return out
}

func lifecycleProjectionCleanupFilter(capability Capability, scope Scope) retrieval.Filter {
	eq := map[string]any{
		projectors.MetadataConversationIDKey: scope.ConversationID,
		projectors.MetadataViewKindKey:       lifecycleProjectionViewKind(capability),
		projectors.MetadataRecordTypeKey:     lifecycleProjectionRecordType(capability),
	}
	if scope.DatasetID != "" {
		eq[projectors.MetadataDatasetIDKey] = scope.DatasetID
	}
	if scope.AgentID != "" {
		eq[projectors.MetadataAgentIDKey] = scope.AgentID
	}
	filter := retrieval.Filter{Eq: eq}
	if scope.DatasetID == "" {
		filter.And = append(filter.And, emptyOrMissingMetadataFilter(projectors.MetadataDatasetIDKey))
	}
	if scope.AgentID == "" {
		filter.And = append(filter.And, emptyOrMissingMetadataFilter(projectors.MetadataAgentIDKey))
	}
	return filter
}

func emptyOrMissingMetadataFilter(key string) retrieval.Filter {
	return retrieval.Filter{Or: []retrieval.Filter{
		{Eq: map[string]any{key: ""}},
		{Missing: []string{key}},
	}}
}

func lifecycleProjectionViewKind(capability Capability) string {
	switch capability {
	case CapabilityMessageIndex:
		return "message_index"
	case CapabilitySummaryDAG:
		return "summary_dag"
	case CapabilityDocumentChunks:
		return "document_chunks"
	default:
		return string(capability)
	}
}

func lifecycleProjectionRecordType(capability Capability) string {
	switch capability {
	case CapabilityMessageIndex:
		return projectors.RecordTypeSourceMessage
	case CapabilitySummaryDAG:
		return projectors.RecordTypeSummaryNode
	case CapabilityDocumentChunks:
		return projectors.RecordTypeDocumentChunk
	default:
		return string(capability)
	}
}

func lifecycleConversationStep(name string, capability Capability, scope Scope) LifecycleStep {
	return LifecycleStep{
		Name:    name,
		Status:  LifecycleStatusPlanned,
		Planned: true,
		Details: map[string]any{
			"capability":      string(capability),
			"runtime_id":      scope.RuntimeID,
			"user_id":         scope.UserID,
			"agent_id":        scope.AgentID,
			"conversation_id": scope.ConversationID,
			"dataset_id":      scope.DatasetID,
		},
	}
}

func failLifecycleStep(step *LifecycleStep, message string, err error) {
	step.Status = LifecycleStatusFailed
	step.Message = message
	if step.Details == nil {
		step.Details = map[string]any{}
	}
	if err != nil {
		step.Details["error"] = err.Error()
	}
}

func appendLifecycleCapabilityFailure(report *LifecycleExecutionReport, name string, capability Capability, message string, err error) {
	step := lifecycleCapabilityStep(name, capability, LifecycleStatusFailed, message)
	if err != nil {
		step.Details["error"] = err.Error()
	}
	report.Steps = append(report.Steps, step)
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

	autoRepair := checkpointBool(req.Job.Checkpoint, "auto_repair")
	documentScan := checkpointBool(req.Job.Checkpoint, "document_scan")
	pageSize := checkpointInt(req.Job.Checkpoint, "diagnostics_page_size")
	pageToken := checkpointString(req.Job.Checkpoint, "diagnostics_page_token")
	repairCapabilities := selectedReconcileRepairCapabilities(req.Job.Capabilities, req.Job.Documents)
	repairTargetsSource := checkpointString(req.Job.Checkpoint, "repair_targets_source")
	if repairTargetsSource == "" {
		repairTargetsSource = reconcileRepairTargetsSource(autoRepair, documentScan, req.Job.Documents)
	}
	initialCheckpoint := cloneCheckpoint(report.Checkpoint)
	diagnostics, err := req.System.runReconcileConsistencyDiagnostics(ctx, req.Job.TraceID, req.Job.Scope, req.Job.Capabilities, req.Job.Documents, pageSize, pageToken, true)
	if err != nil {
		applyDiagnosticsToLifecycleReport(&report, diagnostics)
		report.Checkpoint = mergeLifecycleCheckpoints(report.Checkpoint, initialCheckpoint)
		applyReconcileRepairTargetCheckpoint(&report, autoRepair, repairTargetsSource, req.Job.Documents)
		report.Status = LifecycleStatusFailed
		if report.Message == "" {
			report.Message = err.Error()
		}
		finalizeLifecycleExecutionReport(&report)
		return report, err
	}
	if autoRepair && !documentScan && len(req.Job.Documents) == 0 {
		targets := documentChunkRepairTargetsFromDiagnostics(req.Job.Scope, diagnostics)
		if len(targets) > 0 {
			req.Job.Documents = targets
			repairCapabilities = appendCapabilityIfMissing(repairCapabilities, CapabilityDocumentChunks)
			report.Operation.Documents = cloneDocumentTargets(targets)
			report.Operation.Targets = lifecycleTargetsForDocuments(targets)
			report.Operation.Capabilities = mergeCapabilities(report.Operation.Capabilities, []Capability{CapabilityDocumentChunks})
			repairTargetsSource = "diagnostics_page"
		}
	}
	if len(repairCapabilities) > 0 {
		applyDiagnosticsToLifecycleReportPhase(&report, diagnostics, "pre-repair", false)
	} else {
		applyDiagnosticsToLifecycleReport(&report, diagnostics)
		report.Checkpoint = mergeLifecycleCheckpoints(report.Checkpoint, initialCheckpoint)
	}
	applyReconcileRepairTargetCheckpoint(&report, autoRepair, repairTargetsSource, req.Job.Documents)
	if len(repairCapabilities) == 0 && !diagnostics.OK && diagnosticsHasErrorSeverity(diagnostics) {
		err := fmt.Errorf("memory: reconcile diagnostics failed: %s", diagnostics.Message)
		report.Status = LifecycleStatusFailed
		if report.Message == "" || report.Message == "diagnostics checks found missing dependencies" {
			report.Message = err.Error()
		}
		finalizeLifecycleExecutionReport(&report)
		return report, err
	}
	if len(repairCapabilities) > 0 {
		beforeSteps := len(report.Steps)
		repairErr := runReconcileRepairs(ctx, req, &report, repairCapabilities)
		repairSteps := report.Steps[beforeSteps:]
		repaired := countCompletedLifecycleSteps(repairSteps)
		skipped := countSkippedLifecycleSteps(repairSteps)
		failed := countFailedLifecycleSteps(repairSteps)
		report.Checkpoint["repair_target_count"] = len(req.Job.Documents)
		report.Checkpoint["repair_capability_count"] = len(repairCapabilities)
		report.Checkpoint["repair_capabilities"] = lifecycleCapabilityNames(repairCapabilities)
		report.Checkpoint["repair_targets_source"] = repairTargetsSource
		report.Checkpoint["repaired_capabilities"] = completedLifecycleStepCapabilities(repairSteps)
		report.Checkpoint["skipped_capabilities"] = skippedLifecycleStepCapabilities(repairSteps)
		report.Checkpoint["failed_capabilities"] = failedLifecycleStepCapabilities(repairSteps)
		report.Checkpoint["repair_completed"] = repaired
		report.Checkpoint["repair_skipped"] = skipped
		report.Checkpoint["repair_failed"] = failed
		if checkpointBool(report.Checkpoint, "document_scan") {
			report.Checkpoint["document_scan_repaired"] = repaired
			report.Checkpoint["document_scan_failed"] = failed
		}
		applyLifecycleCleanupCheckpoint(&report)
		if repairErr != nil {
			report.Status = LifecycleStatusFailed
			report.Message = "reconcile repair failed"
			finalizeLifecycleExecutionReport(&report)
			return report, repairErr
		}

		postDiagnostics, postErr := req.System.runReconcileConsistencyDiagnostics(ctx, req.Job.TraceID, req.Job.Scope, req.Job.Capabilities, req.Job.Documents, pageSize, pageToken, true)
		applyDiagnosticsToLifecycleReportPhase(&report, postDiagnostics, "post-repair", true)
		if postErr != nil {
			report.Status = LifecycleStatusFailed
			if report.Message == "" {
				report.Message = postErr.Error()
			}
			finalizeLifecycleExecutionReport(&report)
			return report, postErr
		}
		if !postDiagnostics.OK && diagnosticsHasErrorSeverity(postDiagnostics) {
			err := fmt.Errorf("memory: reconcile post-repair diagnostics failed: %s", postDiagnostics.Message)
			report.Status = LifecycleStatusFailed
			report.Message = err.Error()
			finalizeLifecycleExecutionReport(&report)
			return report, err
		}
		if repaired == 0 {
			report.Status = LifecycleStatusSkipped
			report.Message = "reconcile skipped; scoped repair targets were not sufficient; post-repair diagnostics passed"
			finalizeLifecycleExecutionReport(&report)
			return report, nil
		}
		report.Status = LifecycleStatusCompleted
		report.Message = fmt.Sprintf("reconcile completed; repaired %d capability step(s); post-repair diagnostics passed", repaired)
		finalizeLifecycleExecutionReport(&report)
		return report, nil
	}

	report.Status = LifecycleStatusCompleted
	if report.Message == "" {
		report.Message = "reconcile diagnostics completed"
	}
	finalizeLifecycleExecutionReport(&report)
	return report, nil
}

func (r *System) dispatchReconcileDryRun(ctx context.Context, report *LifecycleExecutionReport) (LifecycleExecutionReport, error) {
	report.Steps = nil
	initialCheckpoint := cloneCheckpoint(report.Checkpoint)
	pageToken := report.Operation.PageToken
	if report.Operation.ScanDocuments {
		pageToken = ""
	}
	diagnostics, err := r.runReconcileConsistencyDiagnostics(ctx, report.TraceID, report.Operation.Scope, report.Operation.Capabilities, report.Operation.Documents, report.Operation.PageSize, pageToken, true)
	applyDiagnosticsToLifecycleReport(report, diagnostics)
	report.Checkpoint = mergeLifecycleCheckpoints(report.Checkpoint, initialCheckpoint)
	if err != nil {
		applyReconcileRepairTargetCheckpoint(report, report.Operation.AutoRepair, reconcileRepairTargetsSource(report.Operation.AutoRepair, report.Operation.ScanDocuments, report.Operation.Documents), report.Operation.Documents)
		report.Status = LifecycleStatusFailed
		report.Message = err.Error()
		return *report, err
	}
	repairCapabilities := selectedReconcileRepairCapabilities(report.Operation.Capabilities, report.Operation.Documents)
	repairTargetsSource := reconcileRepairTargetsSource(report.Operation.AutoRepair, report.Operation.ScanDocuments, report.Operation.Documents)
	if report.Operation.AutoRepair && !report.Operation.ScanDocuments && len(report.Operation.Documents) == 0 {
		targets := documentChunkRepairTargetsFromDiagnostics(report.Operation.Scope, diagnostics)
		if len(targets) > 0 {
			report.Operation.Documents = cloneDocumentTargets(targets)
			report.Operation.Targets = lifecycleTargetsForDocuments(targets)
			report.Operation.Capabilities = mergeCapabilities(report.Operation.Capabilities, []Capability{CapabilityDocumentChunks})
			repairCapabilities = appendCapabilityIfMissing(repairCapabilities, CapabilityDocumentChunks)
			repairTargetsSource = "diagnostics_page"
		}
	}
	if len(repairCapabilities) > 0 {
		beforeSteps := len(report.Steps)
		appendReconcileDryRunRepairSteps(report, repairCapabilities, repairTargetsSource)
		repairSteps := report.Steps[beforeSteps:]
		if report.Checkpoint == nil {
			report.Checkpoint = map[string]any{}
		}
		report.Checkpoint["repair_target_count"] = len(report.Operation.Documents)
		report.Checkpoint["repair_capability_count"] = len(repairCapabilities)
		report.Checkpoint["repair_capabilities"] = lifecycleCapabilityNames(repairCapabilities)
		report.Checkpoint["repair_targets_source"] = repairTargetsSource
		report.Checkpoint["repair_planned"] = countPlannedLifecycleSteps(repairSteps)
		report.Checkpoint["repair_skipped"] = countSkippedLifecycleSteps(repairSteps)
		if checkpointBool(report.Checkpoint, "document_scan") {
			report.Checkpoint["document_scan_repair_planned"] = countPlannedLifecycleSteps(repairSteps)
		}
		report.Status = LifecycleStatusPlanned
		report.Message = "reconcile repair plan generated; no data modified"
		return *report, nil
	}
	applyReconcileRepairTargetCheckpoint(report, report.Operation.AutoRepair, repairTargetsSource, report.Operation.Documents)
	report.Status = LifecycleStatusPlanned
	report.Message = "reconcile diagnostics-only dry-run completed; no repair target selected"
	return *report, nil
}

func runReconcileRepairs(ctx context.Context, req LifecycleRunRequest, report *LifecycleExecutionReport, capabilities []Capability) error {
	var runErr error
	for _, capability := range capabilities {
		var err error
		switch capability {
		case CapabilityDocumentChunks:
			err = runDocumentChunksLifecycle(ctx, req, report)
		case CapabilityMessageIndex:
			err = runMessageIndexLifecycle(ctx, req, report)
		case CapabilitySummaryDAG:
			err = runSummaryDAGLifecycle(ctx, req, report)
		}
		if err != nil {
			runErr = errors.Join(runErr, err)
		}
	}
	return runErr
}

func appendReconcileDryRunRepairSteps(report *LifecycleExecutionReport, capabilities []Capability, targetSource string) {
	if report == nil {
		return
	}
	for _, capability := range capabilities {
		switch capability {
		case CapabilityDocumentChunks:
			for _, target := range report.Operation.Documents {
				report.Steps = append(report.Steps, LifecycleStep{
					Name:    "document_chunks.target",
					Status:  LifecycleStatusPlanned,
					Planned: true,
					Message: fmt.Sprintf("would re-index document %s", documentTargetLabel(target)),
					Details: map[string]any{
						"capability":            string(CapabilityDocumentChunks),
						"dataset_id":            target.DatasetID,
						"document_id":           target.DocumentID,
						"repair_hint":           "rebuild document_chunks for affected document target",
						"repair_targets_source": targetSource,
					},
				})
			}
		case CapabilityMessageIndex:
			report.Steps = append(report.Steps, reconcileConversationRepairPlanStep(report.Operation.Scope, CapabilityMessageIndex))
		case CapabilitySummaryDAG:
			report.Steps = append(report.Steps, reconcileConversationRepairPlanStep(report.Operation.Scope, CapabilitySummaryDAG))
		}
	}
}

func reconcileConversationRepairPlanStep(scope Scope, capability Capability) LifecycleStep {
	name := string(capability) + ".conversation"
	message := fmt.Sprintf("would rebuild %s for conversation %s", capability, scope.ConversationID)
	if scope.ConversationID == "" {
		name = string(capability) + ".scope"
		message = "conversation_id is required; scoped repair would be skipped"
		step := lifecycleCapabilityStep(name, capability, LifecycleStatusSkipped, message)
		step.Details["repair_hint"] = "supply scope.conversation_id for bounded conversation repair"
		return step
	}
	step := lifecycleConversationStep(name, capability, scope)
	step.Status = LifecycleStatusPlanned
	step.Message = message
	step.Details["repair_hint"] = fmt.Sprintf("rebuild %s for explicit conversation scope", capability)
	return step
}

func (r *System) runReconcileConsistencyDiagnostics(ctx context.Context, traceID TraceID, scope Scope, capabilities []Capability, documents []DocumentTarget, pageSize int, pageToken string, requireDeclaredStage bool) (DiagnosticReport, error) {
	return r.runDiagnosticProbes(ctx, DiagnosticRequest{
		TraceID:      traceID,
		Scope:        scope,
		Capabilities: capabilities,
		Documents:    documents,
		Stage:        diagnosticStageConsistency,
		PageSize:     pageSize,
		PageToken:    pageToken,
		Consistency: []ConsistencyCheckKind{
			ConsistencyCheckProjection,
			ConsistencyCheckSourceView,
		},
	}, requireDeclaredStage)
}

func selectedReconcileRepairCapabilities(capabilities []Capability, documents []DocumentTarget) []Capability {
	out := make([]Capability, 0, len(capabilities))
	seen := map[Capability]bool{}
	for _, capability := range capabilities {
		switch capability {
		case CapabilityDocumentChunks:
			if len(documents) == 0 {
				continue
			}
		case CapabilityMessageIndex, CapabilitySummaryDAG:
		default:
			continue
		}
		if seen[capability] {
			continue
		}
		seen[capability] = true
		out = append(out, capability)
	}
	return out
}

func appendCapabilityIfMissing(capabilities []Capability, capability Capability) []Capability {
	if capability == "" {
		return capabilities
	}
	for _, existing := range capabilities {
		if existing == capability {
			return capabilities
		}
	}
	return append(capabilities, capability)
}

func documentChunkRepairTargetsFromDiagnostics(scope Scope, diagnostics DiagnosticReport) []DocumentTarget {
	var targets []DocumentTarget
	seen := map[DocumentTarget]bool{}
	add := func(target DocumentTarget) {
		target.DatasetID = strings.TrimSpace(target.DatasetID)
		target.DocumentID = strings.TrimSpace(target.DocumentID)
		if target.DatasetID == "" {
			target.DatasetID = scope.DatasetID
		}
		if target.DatasetID == "" || target.DocumentID == "" || seen[target] {
			return
		}
		seen[target] = true
		targets = append(targets, target)
	}
	for _, check := range diagnostics.Checks {
		if check.OK || check.Capability != CapabilityDocumentChunks {
			continue
		}
		if check.Target.Kind == "document" && check.Target.Capability == CapabilityDocumentChunks {
			add(DocumentTarget{DatasetID: check.Target.DatasetID, DocumentID: check.Target.DocumentID})
		}
		for _, target := range documentTargetsFromAffectedDocumentsDetail(check.Details["affected_documents"]) {
			add(target)
		}
	}
	return targets
}

func documentTargetsFromAffectedDocumentsDetail(detail any) []DocumentTarget {
	items, ok := detail.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	targets := make([]DocumentTarget, 0, len(items))
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		datasetID, _ := record["dataset_id"].(string)
		documentID, _ := record["document_id"].(string)
		targets = append(targets, DocumentTarget{
			DatasetID:  strings.TrimSpace(datasetID),
			DocumentID: strings.TrimSpace(documentID),
		})
	}
	return targets
}

func applyReconcileRepairTargetCheckpoint(report *LifecycleExecutionReport, autoRepair bool, targetSource string, documents []DocumentTarget) {
	if report == nil {
		return
	}
	if report.Checkpoint == nil {
		report.Checkpoint = map[string]any{}
	}
	report.Checkpoint["auto_repair"] = autoRepair
	report.Checkpoint["repair_targets_source"] = targetSource
	report.Checkpoint["repair_target_count"] = len(documents)
}

func lifecycleCapabilityNames(capabilities []Capability) []string {
	out := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		if capability == "" {
			continue
		}
		out = append(out, string(capability))
	}
	return out
}

func completedLifecycleStepCapabilities(steps []LifecycleStep) []string {
	out := make([]string, 0, len(steps))
	seen := map[string]bool{}
	for _, step := range steps {
		if !step.Completed && step.Status != LifecycleStatusCompleted {
			continue
		}
		capability, _ := step.Details["capability"].(string)
		if capability == "" || seen[capability] {
			continue
		}
		seen[capability] = true
		out = append(out, capability)
	}
	return out
}

func skippedLifecycleStepCapabilities(steps []LifecycleStep) []string {
	out := make([]string, 0, len(steps))
	seen := map[string]bool{}
	for _, step := range steps {
		if !step.Skipped && step.Status != LifecycleStatusSkipped {
			continue
		}
		capability, _ := step.Details["capability"].(string)
		if capability == "" || seen[capability] {
			continue
		}
		seen[capability] = true
		out = append(out, capability)
	}
	return out
}

func failedLifecycleStepCapabilities(steps []LifecycleStep) []string {
	out := make([]string, 0, len(steps))
	seen := map[string]bool{}
	for _, step := range steps {
		if step.Status != LifecycleStatusFailed {
			continue
		}
		capability, _ := step.Details["capability"].(string)
		if capability == "" || seen[capability] {
			continue
		}
		seen[capability] = true
		out = append(out, capability)
	}
	return out
}

func countPlannedLifecycleSteps(steps []LifecycleStep) int {
	count := 0
	for _, step := range steps {
		if step.Planned {
			count++
		}
	}
	return count
}

func countCompletedLifecycleSteps(steps []LifecycleStep) int {
	count := 0
	for _, step := range steps {
		if step.Completed || step.Status == LifecycleStatusCompleted {
			count++
		}
	}
	return count
}

func countSkippedLifecycleSteps(steps []LifecycleStep) int {
	count := 0
	for _, step := range steps {
		if step.Skipped || step.Status == LifecycleStatusSkipped {
			count++
		}
	}
	return count
}

func countFailedLifecycleSteps(steps []LifecycleStep) int {
	count := 0
	for _, step := range steps {
		if step.Status == LifecycleStatusFailed {
			count++
		}
	}
	return count
}

func applyDiagnosticsToLifecycleReport(report *LifecycleExecutionReport, diagnostics DiagnosticReport) {
	applyDiagnosticsToLifecycleReportPhase(report, diagnostics, "", true)
}

func applyDiagnosticsToLifecycleReportPhase(report *LifecycleExecutionReport, diagnostics DiagnosticReport, phase string, failErrorSeverity bool) {
	report.Accepted = true
	report.Supported = true
	report.Message = diagnostics.Message
	errorCount := 0
	repairHintCount := 0
	phase = strings.TrimSpace(phase)
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
		if phase != "" {
			step.Details["diagnostic_phase"] = phase
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
			if failErrorSeverity {
				step.Status = LifecycleStatusFailed
				step.Completed = false
			} else {
				step.Details["diagnostic_would_fail"] = true
			}
		}
		report.Steps = append(report.Steps, step)
	}
	checkpoint := map[string]any{
		"ready":             diagnostics.Ready,
		"ok":                diagnostics.OK,
		"check_count":       len(diagnostics.Checks),
		"warning_count":     len(diagnostics.Warnings),
		"error_count":       errorCount,
		"repair_hint_count": repairHintCount,
	}
	if diagnostics.NextPageToken != "" {
		checkpoint["next_page_token"] = diagnostics.NextPageToken
	}
	if phase == "" {
		report.Checkpoint = checkpoint
		return
	}
	if report.Checkpoint == nil {
		report.Checkpoint = map[string]any{}
	}
	prefix := strings.ReplaceAll(phase, "-", "_")
	report.Checkpoint[prefix+"_diagnostics"] = checkpoint
	report.Checkpoint[prefix+"_ready"] = diagnostics.Ready
	report.Checkpoint[prefix+"_ok"] = diagnostics.OK
	report.Checkpoint[prefix+"_check_count"] = len(diagnostics.Checks)
	report.Checkpoint[prefix+"_warning_count"] = len(diagnostics.Warnings)
	report.Checkpoint[prefix+"_error_count"] = errorCount
	report.Checkpoint[prefix+"_repair_hint_count"] = repairHintCount
	if diagnostics.NextPageToken != "" {
		report.Checkpoint[prefix+"_next_page_token"] = diagnostics.NextPageToken
	}
}

func diagnosticsHasErrorSeverity(report DiagnosticReport) bool {
	for _, check := range report.Checks {
		if check.Severity == DiagnosticSeverityError && !check.OK {
			return true
		}
	}
	return false
}

func checkpointBool(checkpoint map[string]any, key string) bool {
	if checkpoint == nil {
		return false
	}
	value, _ := checkpoint[key].(bool)
	return value
}

func checkpointInt(checkpoint map[string]any, key string) int {
	if checkpoint == nil {
		return 0
	}
	switch value := checkpoint[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func checkpointString(checkpoint map[string]any, key string) string {
	if checkpoint == nil {
		return ""
	}
	value, _ := checkpoint[key].(string)
	return strings.TrimSpace(value)
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
		ID:            job.OperationID,
		TraceID:       traceID,
		Action:        action,
		Scope:         job.Scope,
		Capabilities:  cloneCapabilities(job.Capabilities),
		Documents:     cloneDocumentTargets(job.Documents),
		Reason:        strings.TrimSpace(job.Reason),
		AutoRepair:    checkpointBool(job.Checkpoint, "auto_repair"),
		ScanDocuments: checkpointBool(job.Checkpoint, "document_scan"),
		PageSize:      checkpointInt(job.Checkpoint, "diagnostics_page_size"),
		PageToken:     checkpointString(job.Checkpoint, "diagnostics_page_token"),
		RequestedAt:   requestedAt,
	}
	if operation.PageSize == 0 {
		operation.PageSize = checkpointInt(job.Checkpoint, "document_scan_page_size")
	}
	if operation.PageToken == "" {
		operation.PageToken = checkpointString(job.Checkpoint, "document_scan_page_token")
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
