package memory

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/memory/internal/projectors"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// DiagnosticRequest asks the root facade to run bounded diagnostics for a
// declared diagnostics stage. Empty Stage currently means "freshness".
type DiagnosticRequest struct {
	Scope        Scope
	Capabilities []Capability
	Documents    []DocumentTarget
	Stage        string
}

// DiagnosticReport is the structured diagnostics contract shared by freshness
// and consistency checks.
type DiagnosticReport struct {
	Stage        string
	Scope        Scope
	Capabilities []Capability
	Documents    []DocumentTarget
	Ready        bool
	OK           bool
	Checks       []DiagnosticCheck
	Message      string
	Warnings     []string
}

// DiagnosticCheck reports one bounded diagnostics fact.
type DiagnosticCheck struct {
	Name       string
	Capability Capability
	Status     DiagnosticStatus
	OK         bool
	Severity   DiagnosticSeverity
	Message    string
	Details    map[string]any
}

// DiagnosticStatus is the machine-readable check outcome.
type DiagnosticStatus string

const (
	DiagnosticStatusOK             DiagnosticStatus = "ok"
	DiagnosticStatusWarning        DiagnosticStatus = "warning"
	DiagnosticStatusError          DiagnosticStatus = "error"
	DiagnosticStatusMissing        DiagnosticStatus = "missing"
	DiagnosticStatusStale          DiagnosticStatus = "stale"
	DiagnosticStatusNotImplemented DiagnosticStatus = "not_implemented"
)

// DiagnosticSeverity describes whether a failed check should make the report
// not ready.
type DiagnosticSeverity string

const (
	DiagnosticSeverityInfo    DiagnosticSeverity = "info"
	DiagnosticSeverityWarning DiagnosticSeverity = "warning"
	DiagnosticSeverityError   DiagnosticSeverity = "error"
)

// ConsistencyReport is reserved for source/view and cross-projection checks.
type ConsistencyReport struct {
	DiagnosticReport
}

// ProjectionReport is reserved for projection namespace and index checks.
type ProjectionReport struct {
	DiagnosticReport
}

// Diagnostics runs bounded structured diagnostics for the compiled plan.
func (r *System) Diagnostics(ctx context.Context, req DiagnosticRequest) (DiagnosticReport, error) {
	return r.buildDiagnosticReport(ctx, req, true)
}

func (r *System) buildDiagnosticReport(ctx context.Context, req DiagnosticRequest, requireDeclaredStage bool) (DiagnosticReport, error) {
	stage := strings.TrimSpace(req.Stage)
	if stage == "" {
		stage = diagnosticStageFreshness
	}
	report := DiagnosticReport{
		Stage:        stage,
		Scope:        normalizeScope(req.Scope),
		Capabilities: normalizeDiagnosticCapabilities(req.Capabilities),
		Documents:    cloneDocumentTargets(req.Documents),
		Ready:        true,
		OK:           true,
	}
	if r == nil || r.inner == nil {
		report.addCheck("system.configured", "", DiagnosticStatusError, DiagnosticSeverityError, false, "system is not configured", nil)
		report.Message = "system is not configured"
		return report, errdefs.NotAvailablef("memory: system is not configured")
	}
	if err := report.Scope.Validate(); err != nil {
		report.addCheck("scope.valid", "", DiagnosticStatusError, DiagnosticSeverityError, false, "scope is invalid", map[string]any{
			"error": err.Error(),
		})
		report.Message = "invalid scope"
		return report, errdefs.Validationf("memory: invalid scope: %w", err)
	}
	documents, err := normalizeDocumentTargets(report.Scope, req.Documents)
	if err != nil {
		report.addCheck("diagnostics.document_targets", CapabilityDocumentChunks, DiagnosticStatusError, DiagnosticSeverityError, false, "document targets are invalid", map[string]any{
			"error": err.Error(),
		})
		report.Message = "invalid document targets"
		return report, err
	}
	report.Documents = documents
	if requireDeclaredStage && !r.diagnosticStageDeclared(stage) {
		report.addCheck("diagnostics.stage."+stage, "", DiagnosticStatusError, DiagnosticSeverityError, false, "diagnostics stage is not declared by the plan", nil)
		report.Message = fmt.Sprintf("diagnostics stage %q is not declared by the plan", stage)
		return report, errdefs.NotAvailablef("memory: diagnostics stage %q is not declared by the plan", stage)
	}

	report.addCheck("system.configured", "", DiagnosticStatusOK, DiagnosticSeverityInfo, true, "system is configured", nil)
	switch stage {
	case diagnosticStageFreshness:
		r.addFreshnessDiagnostics(ctx, &report)
	case lifecycleStageReadiness:
		r.addReadinessDiagnostics(&report)
	default:
		report.addCheck("diagnostics.stage."+stage, "", DiagnosticStatusNotImplemented, DiagnosticSeverityWarning, true, "diagnostics stage is declared but has no Stage 2 executor", map[string]any{
			"stage": stage,
		})
	}
	if report.Message == "" {
		if report.OK {
			report.Message = "diagnostics checks completed"
		} else {
			report.Message = "diagnostics checks found missing dependencies"
		}
	}
	return report, nil
}

func (r *System) addFreshnessDiagnostics(ctx context.Context, report *DiagnosticReport) {
	report.addCheck("diagnostics.stage.freshness", "", DiagnosticStatusOK, DiagnosticSeverityInfo, true, "freshness diagnostics stage is available", nil)
	r.addSourceDiagnostics(report)

	capabilities := report.Capabilities
	if len(capabilities) == 0 {
		capabilities = r.assembly.Capabilities()
		report.Capabilities = cloneCapabilities(capabilities)
	}
	for _, capability := range capabilities {
		r.addCapabilityDiagnostics(report, capability)
		r.addProjectionDiagnostics(report, capability)
		r.addFreshnessCapabilityChecks(ctx, report, capability)
	}
}

func (r *System) addReadinessDiagnostics(report *DiagnosticReport) {
	readiness, err := r.Readiness(context.Background())
	if err != nil {
		report.addCheck("readiness", "", DiagnosticStatusError, DiagnosticSeverityError, false, err.Error(), nil)
		return
	}
	for _, check := range readiness.Checks {
		status := DiagnosticStatusOK
		severity := DiagnosticSeverityInfo
		if !check.Ready {
			status = DiagnosticStatusError
			severity = DiagnosticSeverityError
		}
		report.addCheck(check.Name, "", status, severity, check.Ready, check.Message, nil)
	}
}

func (r *System) addSourceDiagnostics(report *DiagnosticReport) {
	if r.assembly.HasSource(SourceMessageLog) {
		report.addDependencyCheck("source.message_store", "", "MessageStore", r.deps.MessageStore != nil)
	}
	if r.assembly.HasSource(SourceDocumentStore) {
		report.addDependencyCheck("source.document_store", "", "DocumentStore", r.deps.DocumentStore != nil)
	}
}

func (r *System) addCapabilityDiagnostics(report *DiagnosticReport, capability Capability) {
	if !r.assembly.HasCapability(capability) {
		report.addCheck(fmt.Sprintf("capability.%s.declared", capability), capability, DiagnosticStatusError, DiagnosticSeverityError, false, "capability is not declared by the assembly", nil)
		return
	}

	switch capability {
	case CapabilityRecentWindow:
		report.addDependencyCheck("capability.recent_window.message_store", capability, "MessageStore", r.deps.MessageStore != nil)
	case CapabilitySummaryDAG:
		report.addDependencyCheck("capability.summary_dag.store", capability, "SummaryStore", r.deps.SummaryStore != nil)
		report.addDependencyCheck("capability.summary_dag.service", capability, "Summarizer", r.deps.Summarizer != nil)
	case CapabilityDocumentChunks:
		report.addDependencyCheck("capability.document_chunks.store", capability, "ChunkStore", r.deps.ChunkStore != nil)
		report.addDependencyCheck("capability.document_chunks.service", capability, "DocumentChunker", r.deps.DocumentChunker != nil)
	case CapabilityObservationLedger:
		report.addDependencyCheck("capability.observation_ledger.store", capability, "ObservationStore", r.deps.ObservationStore != nil)
		report.addDependencyCheck("capability.observation_ledger.service", capability, "ObservationExtractor", r.deps.ObservationExtractor != nil)
	case CapabilityFactLedger:
		report.addDependencyCheck("capability.fact_ledger.store", capability, "FactStore", r.deps.FactStore != nil)
		report.addDependencyCheck("capability.fact_ledger.service", capability, "FactReconciler", r.deps.FactReconciler != nil)
		report.addCheck("capability.fact_ledger.lifecycle_semantics", capability, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "fact ledger lifecycle semantics support active, superseded, retracted, conflict, and revision lineage", map[string]any{
			"statuses": []string{"active", "superseded", "retracted", "conflict"},
		})
	case CapabilityFactGraph:
		report.addDependencyCheck("capability.fact_graph.store", capability, "FactGraphStore", r.deps.FactGraphStore != nil)
		report.addDependencyCheck("capability.fact_graph.service", capability, "FactGraphBuilder", r.deps.FactGraphBuilder != nil)
	case CapabilityEntityProfile:
		report.addDependencyCheck("capability.entity_profile.store", capability, "EntityProfileStore", r.deps.EntityProfileStore != nil)
		report.addDependencyCheck("capability.entity_profile.service", capability, "EntityProfileBuilder", r.deps.EntityProfileBuilder != nil)
	case CapabilityEntityTimeline:
		report.addDependencyCheck("capability.entity_timeline.store", capability, "EntityTimelineStore", r.deps.EntityTimelineStore != nil)
		report.addDependencyCheck("capability.entity_timeline.service", capability, "EntityTimelineBuilder", r.deps.EntityTimelineBuilder != nil)
	default:
		report.addCheck(fmt.Sprintf("capability.%s.implemented", capability), capability, DiagnosticStatusError, DiagnosticSeverityError, false, "capability is not implemented by the root facade", nil)
	}
}

func (r *System) addProjectionDiagnostics(report *DiagnosticReport, capability Capability) {
	baseNamespace, ok := r.assembly.ProjectionNamespace(capability)
	if !ok {
		if capabilitySupportsProjectionDiagnostics(capability) {
			report.addCheck(fmt.Sprintf("projection.%s.binding", capability), capability, DiagnosticStatusWarning, DiagnosticSeverityWarning, true, "projection namespace is not declared; indexed consistency checks are skipped", nil)
		}
		return
	}

	report.addCheck(fmt.Sprintf("projection.%s.binding", capability), capability, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "projection namespace is declared", map[string]any{
		"base_namespace": baseNamespace,
	})
	report.addDependencyCheck(fmt.Sprintf("projection.%s.index", capability), capability, "Index", r.deps.Index != nil)

	scopedNamespace, err := projectors.ScopedNamespace(baseNamespace, report.Scope)
	if err != nil {
		report.addCheck(fmt.Sprintf("projection.%s.scoped_namespace", capability), capability, DiagnosticStatusError, DiagnosticSeverityError, false, "scoped projection namespace cannot be computed", map[string]any{
			"base_namespace": baseNamespace,
			"error":          err.Error(),
		})
		return
	}
	report.addCheck(fmt.Sprintf("projection.%s.scoped_namespace", capability), capability, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "scoped projection namespace is computable", map[string]any{
		"base_namespace":   baseNamespace,
		"scoped_namespace": scopedNamespace,
		"runtime_id":       report.Scope.RuntimeID,
		"user_id":          report.Scope.UserID,
	})
}

func (r *System) addFreshnessCapabilityChecks(ctx context.Context, report *DiagnosticReport, capability Capability) {
	switch capability {
	case CapabilityDocumentChunks:
		if len(report.Documents) > 0 {
			r.addDocumentTargetFreshnessDiagnostics(ctx, report)
			return
		}
		report.addCheck("freshness.document_chunks.targets", capability, DiagnosticStatusNotImplemented, DiagnosticSeverityWarning, true, "document chunk freshness requires explicit document targets; full scans are not implemented", map[string]any{
			"requires_targets": true,
		})
	case CapabilityFactLedger:
		report.addCheck("freshness.fact.reconcile_semantics", capability, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "fact lifecycle ledger semantics are implemented; document freshness remains deferred", map[string]any{
			"implemented_stage": "Stage 4",
		})
	default:
		if _, ok := r.assembly.ProjectionNamespace(capability); ok {
			report.addCheck(fmt.Sprintf("consistency.projection_records.%s", capability), capability, DiagnosticStatusNotImplemented, DiagnosticSeverityInfo, true, "record-level projection scan is not implemented in Stage 2", map[string]any{
				"deferred_stage": "Stage 5",
			})
		}
	}
}

func (r *System) addDocumentTargetFreshnessDiagnostics(ctx context.Context, report *DiagnosticReport) {
	for _, target := range report.Documents {
		scope := report.Scope
		scope.DatasetID = target.DatasetID
		details := documentTargetDetails(target, scope)
		if r.deps.DocumentStore == nil || r.deps.ChunkStore == nil {
			report.addCheck("freshness.document_chunks.target", CapabilityDocumentChunks, DiagnosticStatusError, DiagnosticSeverityError, false, "document chunk freshness requires DocumentStore and ChunkStore", details)
			continue
		}

		doc, ok, err := r.deps.DocumentStore.Get(ctx, target.DatasetID, target.DocumentID)
		if err != nil {
			details["error"] = err.Error()
			report.addCheck("freshness.document_chunks.target", CapabilityDocumentChunks, DiagnosticStatusError, DiagnosticSeverityError, false, "canonical document lookup failed", details)
			continue
		}
		if !ok {
			details["state"] = "missing_document"
			details["chunk_count"] = 0
			report.addCheck("freshness.document_chunks.target", CapabilityDocumentChunks, DiagnosticStatusMissing, DiagnosticSeverityError, false, fmt.Sprintf("canonical document %s is missing", documentTargetLabel(target)), details)
			continue
		}

		chunks, err := r.deps.ChunkStore.ListChunks(ctx, target.DocumentID, viewdocument.ListOptions{Scope: &scope})
		if err != nil {
			details["error"] = err.Error()
			report.addCheck("freshness.document_chunks.target", CapabilityDocumentChunks, DiagnosticStatusError, DiagnosticSeverityError, false, "document chunk lookup failed", details)
			continue
		}
		outcome := compareDocumentTargetFreshness(target, scope, doc, chunks)
		report.addCheck("freshness.document_chunks.target", CapabilityDocumentChunks, outcome.status, outcome.severity, outcome.ok, outcome.message, outcome.details)
	}
}

type documentFreshnessOutcome struct {
	status   DiagnosticStatus
	severity DiagnosticSeverity
	ok       bool
	message  string
	details  map[string]any
}

func compareDocumentTargetFreshness(target DocumentTarget, scope Scope, doc sourcedocument.Document, chunks []viewdocument.Chunk) documentFreshnessOutcome {
	details := documentTargetDetails(target, scope)
	details["document_version"] = strconv.FormatUint(doc.Version, 10)
	details["document_content_hash"] = doc.ContentHash
	details["chunk_count"] = len(chunks)
	if len(chunks) == 0 {
		details["state"] = "missing_chunks"
		return documentFreshnessOutcome{
			status:   DiagnosticStatusMissing,
			severity: DiagnosticSeverityError,
			ok:       false,
			message:  fmt.Sprintf("document %s has no chunks", documentTargetLabel(target)),
			details:  details,
		}
	}

	var staleChunks []string
	var unknownChunks []string
	for _, chunk := range chunks {
		comparable, staleReasons := compareDocumentChunkFreshness(scope, doc, chunk)
		if len(staleReasons) > 0 {
			staleChunks = append(staleChunks, string(chunk.ID))
			continue
		}
		if !comparable {
			unknownChunks = append(unknownChunks, string(chunk.ID))
		}
	}
	if len(staleChunks) > 0 {
		details["state"] = "stale"
		details["stale_chunks"] = staleChunks
		return documentFreshnessOutcome{
			status:   DiagnosticStatusStale,
			severity: DiagnosticSeverityError,
			ok:       false,
			message:  fmt.Sprintf("document %s has stale chunks", documentTargetLabel(target)),
			details:  details,
		}
	}
	if len(unknownChunks) > 0 {
		details["state"] = "unknown"
		details["unknown_chunks"] = unknownChunks
		return documentFreshnessOutcome{
			status:   DiagnosticStatusNotImplemented,
			severity: DiagnosticSeverityWarning,
			ok:       true,
			message:  fmt.Sprintf("document %s chunk freshness cannot be proven from available signatures", documentTargetLabel(target)),
			details:  details,
		}
	}
	details["state"] = "fresh"
	return documentFreshnessOutcome{
		status:   DiagnosticStatusOK,
		severity: DiagnosticSeverityInfo,
		ok:       true,
		message:  fmt.Sprintf("document %s chunks are fresh", documentTargetLabel(target)),
		details:  details,
	}
}

func compareDocumentChunkFreshness(scope Scope, doc sourcedocument.Document, chunk viewdocument.Chunk) (bool, []string) {
	var comparable bool
	var stale []string
	wantRevision := strconv.FormatUint(doc.Version, 10)

	if chunk.Scope.RuntimeID != scope.RuntimeID || chunk.Scope.UserID != scope.UserID {
		stale = append(stale, "chunk hard partition does not match target scope")
	}
	if chunk.Scope.DatasetID != doc.DatasetID || chunk.DocumentID != doc.ID {
		stale = append(stale, "chunk document identity does not match canonical document")
	}

	ref := chunk.SourceRef
	if ref.Kind != views.SourceDocument || ref.Document == nil {
		stale = append(stale, "chunk source_ref does not reference a document")
	} else {
		docRef := ref.Document
		if docRef.DatasetID != doc.DatasetID || docRef.DocumentID != doc.ID {
			stale = append(stale, "source_ref document identity does not match canonical document")
		}
		if docRef.Version != "" {
			comparable = true
			if docRef.Version != wantRevision {
				stale = append(stale, "source_ref version does not match canonical document version")
			}
		}
		if docRef.ContentHash != "" {
			comparable = true
			if docRef.ContentHash != doc.ContentHash {
				stale = append(stale, "source_ref content hash does not match canonical document content hash")
			}
		}
		if sourceKey, err := ref.StableKeyE(); err == nil {
			matchedRevision, foundRevision := documentSourceRevision(chunk.Signature.SourceRevisions, sourceKey)
			if !foundRevision {
				comparable = true
				stale = append(stale, "signature source revision is missing for chunk source_ref")
			} else {
				if matchedRevision.Revision != "" {
					comparable = true
					if matchedRevision.Revision != wantRevision {
						stale = append(stale, "signature source revision does not match canonical document version")
					}
				}
				if matchedRevision.ContentHash != "" {
					comparable = true
					if matchedRevision.ContentHash != doc.ContentHash {
						stale = append(stale, "signature source content hash does not match canonical document content hash")
					}
				}
			}
		}
	}

	if chunk.Layer.TransformSignature != "" {
		comparable = true
		if chunk.Signature.TransformSignature != chunk.Layer.TransformSignature {
			stale = append(stale, "signature transform does not match chunk layer transform")
		}
	}
	return comparable, stale
}

func documentSourceRevision(revisions []views.SourceRevision, sourceKey string) (views.SourceRevision, bool) {
	for _, revision := range revisions {
		if revision.Kind == views.SourceDocument && revision.SourceKey == sourceKey {
			return revision, true
		}
	}
	return views.SourceRevision{}, false
}

func documentTargetDetails(target DocumentTarget, scope Scope) map[string]any {
	return map[string]any{
		"dataset_id":      target.DatasetID,
		"document_id":     target.DocumentID,
		"runtime_id":      scope.RuntimeID,
		"user_id":         scope.UserID,
		"conversation_id": scope.ConversationID,
	}
}

func (report *DiagnosticReport) addDependencyCheck(name string, capability Capability, dependency string, ready bool) {
	status := DiagnosticStatusOK
	severity := DiagnosticSeverityInfo
	if !ready {
		status = DiagnosticStatusError
		severity = DiagnosticSeverityError
	}
	report.addCheck(name, capability, status, severity, ready, dependencyMessage(dependency, ready), nil)
}

func (report *DiagnosticReport) addCheck(name string, capability Capability, status DiagnosticStatus, severity DiagnosticSeverity, ok bool, message string, details map[string]any) {
	report.Checks = append(report.Checks, DiagnosticCheck{
		Name:       name,
		Capability: capability,
		Status:     status,
		OK:         ok,
		Severity:   severity,
		Message:    message,
		Details:    cloneDiagnosticDetails(details),
	})
	if severity == DiagnosticSeverityError && !ok {
		report.Ready = false
		report.OK = false
	}
	if severity == DiagnosticSeverityWarning {
		report.Warnings = append(report.Warnings, message)
	}
}

func (r *System) diagnosticStageDeclared(stage string) bool {
	for _, planned := range r.plan.Diagnostics {
		if planned.Name == stage {
			return true
		}
	}
	return false
}

func normalizeDiagnosticCapabilities(in []Capability) []Capability {
	if in == nil {
		return nil
	}
	out := make([]Capability, 0, len(in))
	for _, capability := range in {
		trimmed := Capability(strings.TrimSpace(string(capability)))
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func capabilitySupportsProjectionDiagnostics(capability Capability) bool {
	switch capability {
	case CapabilitySummaryDAG,
		CapabilityDocumentChunks,
		CapabilityObservationLedger,
		CapabilityFactLedger,
		CapabilityFactGraph,
		CapabilityEntityProfile,
		CapabilityEntityTimeline:
		return true
	default:
		return false
	}
}

func cloneDiagnosticChecks(in []DiagnosticCheck) []DiagnosticCheck {
	if in == nil {
		return nil
	}
	out := make([]DiagnosticCheck, len(in))
	for i, check := range in {
		out[i] = check
		out[i].Details = cloneDiagnosticDetails(check.Details)
	}
	return out
}

func cloneDiagnosticDetails(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
