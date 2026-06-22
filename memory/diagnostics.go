package memory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	summaryderive "github.com/GizClaw/flowcraft/memory/derive/summary"
	"github.com/GizClaw/flowcraft/memory/internal/projectors"
	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// DiagnosticRequest asks the root facade to run bounded diagnostics for a
// declared diagnostics stage. Empty Stage currently means "freshness".
type DiagnosticRequest struct {
	TraceID      TraceID
	Scope        Scope
	Capabilities []Capability
	Documents    []DocumentTarget
	Stage        string
	PageSize     int
	PageToken    string
	Consistency  []ConsistencyCheckKind
}

// ConsistencyCheckKind selects which consistency surfaces a diagnostics request
// should scan.
type ConsistencyCheckKind string

const (
	ConsistencyCheckProjection ConsistencyCheckKind = "projection"
	ConsistencyCheckSourceView ConsistencyCheckKind = "source_view"
)

const (
	defaultDiagnosticPageSize = 100
	maxDiagnosticPageSize     = 500
)

// DiagnosticProbe runs one named diagnostics probe for a declared diagnostics
// stage. Probes should return structured checks and leave report aggregation to
// the System facade.
type DiagnosticProbe interface {
	Run(context.Context, DiagnosticProbeRequest) (DiagnosticProbeResult, error)
}

// DiagnosticProbeRequest is the normalized context passed to a diagnostics
// probe. Deps is a read-only snapshot of configured dependency interfaces.
type DiagnosticProbeRequest struct {
	TraceID              TraceID
	System               *System
	Deps                 Deps
	Plan                 Plan
	Stage                string
	Scope                Scope
	Capabilities         []Capability
	DeclaredCapabilities []Capability
	Documents            []DocumentTarget
	PageSize             int
	PageToken            string
	Consistency          []ConsistencyCheckKind
}

// DiagnosticProbeResult is the partial report produced by one probe.
type DiagnosticProbeResult struct {
	Capabilities  []Capability
	Documents     []DocumentTarget
	Checks        []DiagnosticCheck
	Message       string
	NextPageToken string
}

// DiagnosticProbeRegistry resolves diagnostics stages to one or more named
// probes. Probe names are stage-local and run in registration order.
type DiagnosticProbeRegistry struct {
	stages map[string][]registeredDiagnosticProbe
}

type registeredDiagnosticProbe struct {
	name  string
	probe DiagnosticProbe
}

// NewDiagnosticProbeRegistry creates an empty diagnostics probe registry.
func NewDiagnosticProbeRegistry() *DiagnosticProbeRegistry {
	return &DiagnosticProbeRegistry{stages: make(map[string][]registeredDiagnosticProbe)}
}

// Register binds name to a probe for stage. Passing a nil probe removes the
// binding.
func (r *DiagnosticProbeRegistry) Register(stage, name string, probe DiagnosticProbe) {
	if r == nil {
		return
	}
	stage = strings.TrimSpace(stage)
	name = strings.TrimSpace(name)
	if stage == "" || name == "" {
		return
	}
	if r.stages == nil {
		r.stages = make(map[string][]registeredDiagnosticProbe)
	}
	probes := r.stages[stage]
	for i, registered := range probes {
		if registered.name != name {
			continue
		}
		if probe == nil {
			r.stages[stage] = append(probes[:i], probes[i+1:]...)
			return
		}
		probes[i].probe = probe
		r.stages[stage] = probes
		return
	}
	if probe != nil {
		r.stages[stage] = append(probes, registeredDiagnosticProbe{name: name, probe: probe})
	}
}

// Lookup returns the named probe registered for stage.
func (r *DiagnosticProbeRegistry) Lookup(stage, name string) (DiagnosticProbe, bool) {
	if r == nil || r.stages == nil {
		return nil, false
	}
	stage = strings.TrimSpace(stage)
	name = strings.TrimSpace(name)
	for _, registered := range r.stages[stage] {
		if registered.name == name && registered.probe != nil {
			return registered.probe, true
		}
	}
	return nil, false
}

func (r *DiagnosticProbeRegistry) stageProbes(stage string) []registeredDiagnosticProbe {
	if r == nil || r.stages == nil {
		return nil
	}
	probes := r.stages[strings.TrimSpace(stage)]
	if len(probes) == 0 {
		return nil
	}
	out := make([]registeredDiagnosticProbe, 0, len(probes))
	for _, registered := range probes {
		if registered.probe != nil {
			out = append(out, registered)
		}
	}
	return out
}

func (r *DiagnosticProbeRegistry) mergeFrom(other *DiagnosticProbeRegistry) {
	if r == nil || other == nil || other.stages == nil {
		return
	}
	for stage, probes := range other.stages {
		for _, registered := range probes {
			r.Register(stage, registered.name, registered.probe)
		}
	}
}

// DiagnosticReport is the structured diagnostics contract shared by freshness
// and consistency checks.
type DiagnosticReport struct {
	TraceID       TraceID
	Stage         string
	Scope         Scope
	Capabilities  []Capability
	Documents     []DocumentTarget
	NextPageToken string
	Ready         bool
	OK            bool
	Checks        []DiagnosticCheck
	Message       string
	Warnings      []string
}

// DiagnosticCheck reports one bounded diagnostics fact.
type DiagnosticCheck struct {
	Name       string
	Capability Capability
	Scope      Scope
	Target     LifecycleTarget
	Status     DiagnosticStatus
	OK         bool
	Severity   DiagnosticSeverity
	Message    string
	Details    map[string]any
	RepairHint string
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

// Diagnostics runs bounded structured diagnostics for the compiled plan.
func (r *System) Diagnostics(ctx context.Context, req DiagnosticRequest) (report DiagnosticReport, err error) {
	return r.runDiagnosticProbes(ctx, req, true)
}

func (r *System) runDiagnosticProbes(ctx context.Context, req DiagnosticRequest, requireDeclaredStage bool) (report DiagnosticReport, err error) {
	stage := strings.TrimSpace(req.Stage)
	if stage == "" {
		stage = diagnosticStageFreshness
	}
	report = DiagnosticReport{
		TraceID:      ensureTraceID(req.TraceID),
		Stage:        stage,
		Scope:        normalizeScope(req.Scope),
		Capabilities: normalizeDiagnosticCapabilities(req.Capabilities),
		Documents:    cloneDocumentTargets(req.Documents),
		Ready:        true,
		OK:           true,
	}
	defer func() {
		if storeErr := r.putDiagnosticReport(ctx, report); storeErr != nil && err == nil {
			err = storeErr
		}
	}()

	pageSize := normalizeDiagnosticPageSize(req.PageSize)
	consistency := normalizeConsistencyCheckKinds(req.Consistency)
	if r == nil || r.inner == nil {
		report.addCheck("system.configured", "", DiagnosticStatusError, DiagnosticSeverityError, false, "system is not configured", nil)
		finalizeDiagnosticReport(&report, "system is not configured")
		return report, nil
	}
	if !report.Scope.IsZero() {
		if err := report.Scope.Validate(); err != nil {
			report.addCheck("scope.valid", "", DiagnosticStatusError, DiagnosticSeverityError, false, "scope is invalid", map[string]any{"error": err.Error()})
			finalizeDiagnosticReport(&report, "invalid scope")
			return report, nil
		}
	}
	documents, err := normalizeDocumentTargets(report.Scope, report.Documents)
	if err != nil {
		report.addCheck("diagnostics.document_targets", CapabilityDocumentChunks, DiagnosticStatusError, DiagnosticSeverityError, false, "document targets are invalid", map[string]any{"error": err.Error()})
		finalizeDiagnosticReport(&report, "invalid document targets")
		return report, nil
	}
	report.Documents = documents
	if requireDeclaredStage && !plannedStageNamed(r.plan.Diagnostics, stage) {
		report.addCheck("diagnostics.stage."+stage, "", DiagnosticStatusError, DiagnosticSeverityError, false, "diagnostics stage is not declared by the plan", nil)
		finalizeDiagnosticReport(&report, fmt.Sprintf("diagnostics stage %q is not declared by the plan", stage))
		return report, nil
	}
	report.addCheck("system.configured", "", DiagnosticStatusOK, DiagnosticSeverityInfo, true, "system is configured", nil)
	probes := r.diagnosticProbes(stage)
	if len(probes) == 0 {
		report.addCheck("diagnostics.stage."+stage, "", DiagnosticStatusNotImplemented, DiagnosticSeverityError, false, "diagnostics stage is declared but has no registered probe", map[string]any{"stage": stage})
		finalizeDiagnosticReport(&report, fmt.Sprintf("diagnostics stage %q has no registered probe", stage))
		return report, nil
	}

	probeReq := r.newDiagnosticProbeRequest(report.TraceID, stage, report.Scope, report.Capabilities, report.Documents, pageSize, strings.TrimSpace(req.PageToken), consistency)
	for _, registered := range probes {
		result, err := registered.probe.Run(ctx, probeReq)
		if err != nil {
			report.addCheck("diagnostics.probe."+registered.name, "", DiagnosticStatusError, DiagnosticSeverityError, false, err.Error(), map[string]any{"probe": registered.name})
			finalizeDiagnosticReport(&report, "diagnostics probe failed")
			return report, nil
		}
		report.Capabilities = mergeCapabilities(report.Capabilities, result.Capabilities)
		report.Documents = mergeDocumentTargets(report.Documents, result.Documents)
		report.Checks = append(report.Checks, cloneDiagnosticChecks(result.Checks)...)
		if result.Message != "" {
			report.Message = result.Message
		}
		if result.NextPageToken != "" {
			report.NextPageToken = result.NextPageToken
		}
	}
	finalizeDiagnosticReport(&report, "")
	return report, nil
}

func (r *System) defaultDiagnosticProbeRegistry() *DiagnosticProbeRegistry {
	registry := NewDiagnosticProbeRegistry()
	registry.Register(lifecycleStageReadiness, "readiness", readinessProbe{})
	registry.Register(diagnosticStageFreshness, "freshness", freshnessProbe{})
	registry.Register(diagnosticStageTrace, "trace", traceProbe{})
	registry.Register(lifecycleStageQueueStats, "queue_stats", queueStatsProbe{})
	registry.Register(diagnosticStageConsistency, "consistency", consistencyProbe{})
	return registry
}

func (r *System) diagnosticProbes(stage string) []registeredDiagnosticProbe {
	if r == nil || r.diagnosticRegistry == nil {
		return nil
	}
	return r.diagnosticRegistry.stageProbes(stage)
}

func (r *System) newDiagnosticProbeRequest(traceID TraceID, stage string, scope Scope, capabilities []Capability, documents []DocumentTarget, pageSize int, pageToken string, consistency []ConsistencyCheckKind) DiagnosticProbeRequest {
	var declared []Capability
	if r != nil {
		declared = r.assembly.Capabilities()
	}
	return DiagnosticProbeRequest{
		TraceID:              traceID,
		System:               r,
		Deps:                 r.deps,
		Plan:                 r.Plan(),
		Stage:                stage,
		Scope:                scope,
		Capabilities:         cloneCapabilities(capabilities),
		DeclaredCapabilities: cloneCapabilities(declared),
		Documents:            cloneDocumentTargets(documents),
		PageSize:             pageSize,
		PageToken:            pageToken,
		Consistency:          cloneConsistencyCheckKinds(consistency),
	}
}

type freshnessProbe struct{}

func (freshnessProbe) Run(ctx context.Context, req DiagnosticProbeRequest) (DiagnosticProbeResult, error) {
	result := DiagnosticProbeResult{
		Capabilities: cloneCapabilities(req.Capabilities),
		Documents:    cloneDocumentTargets(req.Documents),
		Checks: []DiagnosticCheck{newDiagnosticCheck(
			"diagnostics.stage.freshness",
			"",
			req.Scope,
			LifecycleTarget{},
			DiagnosticStatusOK,
			DiagnosticSeverityInfo,
			true,
			"freshness diagnostics stage is available",
			nil,
		)},
	}
	addSourceDiagnostics(&result, req)
	addWriteDependencyDiagnostics(&result, req)
	for _, capability := range diagnosticCapabilitiesOrDeclared(req) {
		addCapabilityDiagnostics(&result, req, capability)
		addProjectionDiagnostics(&result, req, capability)
		addProjectionFreshnessDiagnostics(ctx, &result, req, capability)
	}
	return result, nil
}

type readinessProbe struct{}

func (readinessProbe) Run(ctx context.Context, req DiagnosticProbeRequest) (DiagnosticProbeResult, error) {
	result := DiagnosticProbeResult{}
	if req.System == nil {
		result.Checks = append(result.Checks, newDiagnosticCheck("readiness", "", req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "system is not configured", nil))
		return result, nil
	}
	readiness, err := req.System.Readiness(ctx)
	if err != nil {
		result.Checks = append(result.Checks, newDiagnosticCheck("readiness", "", req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, err.Error(), nil))
		return result, nil
	}
	for _, check := range readiness.Checks {
		status := DiagnosticStatusOK
		severity := DiagnosticSeverityInfo
		if !check.Ready {
			status = DiagnosticStatusError
			severity = DiagnosticSeverityError
		}
		if check.Severity != "" {
			severity = check.Severity
			if !check.Ready && severity == DiagnosticSeverityWarning {
				status = DiagnosticStatusWarning
			}
		}
		result.Checks = append(result.Checks, newDiagnosticCheck(check.Name, "", req.Scope, LifecycleTarget{}, status, severity, check.Ready, check.Message, nil))
	}
	return result, nil
}

type traceProbe struct{}

func (traceProbe) Run(ctx context.Context, req DiagnosticProbeRequest) (DiagnosticProbeResult, error) {
	result := DiagnosticProbeResult{Capabilities: cloneCapabilities(req.Capabilities), Documents: cloneDocumentTargets(req.Documents)}
	stats, statsErr := QueueStats{}, error(nil)
	if req.System != nil {
		stats, statsErr = req.System.QueueStats(ctx)
	}
	status, severity, ok := DiagnosticStatusOK, DiagnosticSeverityInfo, true
	details := map[string]any{"stats": queueStatsDetails(stats)}
	message := "queue stats snapshot captured for trace diagnostics"
	if statsErr != nil {
		status, severity, ok = DiagnosticStatusError, DiagnosticSeverityError, false
		message = "queue stats lookup failed"
		details["error"] = statsErr.Error()
	}
	result.Checks = append(result.Checks,
		newDiagnosticCheck("diagnostics.stage.trace", "", req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "trace diagnostics stage is available", nil),
		newDiagnosticCheck("trace.queue_stats", "", req.Scope, LifecycleTarget{}, status, severity, ok, message, details),
		newDiagnosticCheck("trace.plan", "", req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "compiled plan stages selected for this request", map[string]any{
			"write_stages":      plannedStageDetails(req.Plan.Write),
			"read_stages":       plannedStageDetails(req.Plan.Read),
			"lifecycle_stages":  plannedStageDetails(req.Plan.Lifecycle),
			"diagnostic_stages": plannedStageDetails(req.Plan.Diagnostics),
		}),
	)
	return result, nil
}

type queueStatsProbe struct{}

func (queueStatsProbe) Run(ctx context.Context, req DiagnosticProbeRequest) (DiagnosticProbeResult, error) {
	result := DiagnosticProbeResult{Capabilities: cloneCapabilities(req.Capabilities), Documents: cloneDocumentTargets(req.Documents)}
	if req.System == nil {
		result.Checks = append(result.Checks, newDiagnosticCheck("diagnostics.stage.queue_stats", "", req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "queue stats diagnostics requires a configured system", nil))
		return result, nil
	}
	stats, err := req.System.QueueStats(ctx)
	if err != nil {
		result.Checks = append(result.Checks, newDiagnosticCheck("diagnostics.stage.queue_stats", "", req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "queue stats lookup failed", map[string]any{"error": err.Error()}))
		return result, nil
	}
	result.Checks = append(result.Checks, newDiagnosticCheck("diagnostics.stage.queue_stats", "", req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "queue stats diagnostics stage is available", queueStatsDetails(stats)))
	return result, nil
}

type consistencyProbe struct{}

func (consistencyProbe) Run(ctx context.Context, req DiagnosticProbeRequest) (DiagnosticProbeResult, error) {
	result := DiagnosticProbeResult{
		Capabilities: cloneCapabilities(req.Capabilities),
		Documents:    cloneDocumentTargets(req.Documents),
		Checks: []DiagnosticCheck{newDiagnosticCheck(
			"diagnostics.stage.consistency",
			"",
			req.Scope,
			LifecycleTarget{},
			DiagnosticStatusOK,
			DiagnosticSeverityInfo,
			true,
			"consistency diagnostics stage is available",
			nil,
		)},
	}
	for _, kind := range req.Consistency {
		if !supportedConsistencyCheckKind(kind) {
			result.Checks = append(result.Checks, newDiagnosticCheck("consistency.kind", "", req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "unsupported consistency check kind", map[string]any{"kind": string(kind)}))
			return result, nil
		}
	}
	for _, capability := range diagnosticCapabilitiesOrDeclared(req) {
		result.Checks = append(result.Checks, newDiagnosticCheck("consistency.capability."+string(capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "capability consistency checks are scoped to configured stores and projections", nil))
		if consistencyIncludesProjection(req.Consistency) {
			addProjectionConsistencyDiagnostics(ctx, &result, req, capability)
		}
		if consistencyIncludesSourceView(req.Consistency) && capability == CapabilityDocumentChunks {
			addProjectionFreshnessDiagnostics(ctx, &result, req, capability)
		}
	}
	return result, nil
}

func addSourceDiagnostics(result *DiagnosticProbeResult, req DiagnosticProbeRequest) {
	if req.System == nil {
		return
	}
	if req.System.assembly.HasSource(SourceMessageLog) {
		addDependencyCheck(result, "source.message_store", "", req.Scope, "MessageStore", req.Deps.MessageStore != nil)
	}
	if req.System.assembly.HasSource(SourceDocumentStore) {
		addDependencyCheck(result, "source.document_store", "", req.Scope, "DocumentStore", req.Deps.DocumentStore != nil)
	}
}

func addCapabilityDiagnostics(result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability) {
	switch capability {
	case CapabilityRecentWindow:
		addDependencyCheck(result, "capability.recent_window.store", capability, req.Scope, "MessageStore", req.Deps.MessageStore != nil)
	case CapabilitySummaryDAG:
		addDependencyCheck(result, "capability.summary_dag.store", capability, req.Scope, "SummaryStore", req.Deps.SummaryStore != nil)
	case CapabilityDocumentChunks:
		addDependencyCheck(result, "capability.document_chunks.store", capability, req.Scope, "ChunkStore", req.Deps.ChunkStore != nil)
	case CapabilityEntityFactIndex:
		addDependencyCheck(result, "capability.entity_fact_index.store", capability, req.Scope, "EntityFactStore", req.Deps.EntityFactStore != nil)
	}
}

func addWriteDependencyDiagnostics(result *DiagnosticProbeResult, req DiagnosticProbeRequest) {
	if req.System == nil {
		return
	}
	capabilities := map[Capability]bool{}
	for _, capability := range diagnosticCapabilitiesOrDeclared(req) {
		capabilities[capability] = true
	}
	for _, stage := range req.Plan.Write {
		switch stage.Name {
		case writeStageChunkDocument:
			if capabilities[CapabilityDocumentChunks] && !stage.Optional && req.Deps.DocumentChunker == nil {
				addDependencyWarning(result, "write_readiness.document_chunks", CapabilityDocumentChunks, req.Scope, "DocumentChunker")
			}
		case writeStageBuildSummaryDAG:
			if capabilities[CapabilitySummaryDAG] && !stage.Optional && req.Deps.Summarizer == nil {
				addDependencyWarning(result, "write_readiness.summary_dag", CapabilitySummaryDAG, req.Scope, "Summarizer")
			}
		case writeStageBuildEntityFacts:
			if capabilities[CapabilityEntityFactIndex] && !stage.Optional && req.Deps.EntityFactExtractor == nil {
				addDependencyWarning(result, "write_readiness.entity_fact_index", CapabilityEntityFactIndex, req.Scope, "EntityFactExtractor")
			}
		}
	}
}

func addProjectionDiagnostics(result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability) {
	if req.System == nil || !readProjectionConfigured(req.System.assembly, req.Deps, capability) {
		return
	}
	namespace, _ := req.System.assembly.ProjectionNamespace(capability)
	result.Checks = append(result.Checks, newDiagnosticCheck("projection."+string(capability)+".index", capability, req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "projection namespace is configured", map[string]any{"namespace": namespace}))
}

type diagnosticProjectionPage struct {
	namespace     string
	docs          []retrieval.Doc
	nextPageToken string
	total         int64
}

const diagnosticProjectionPageTokenPrefix = "diagproj:v1:"

type diagnosticProjectionPageToken struct {
	Version int                             `json:"v"`
	Scans   []diagnosticProjectionScanToken `json:"scans"`
}

type diagnosticProjectionScanToken struct {
	Capability Capability `json:"capability"`
	Namespace  string     `json:"namespace"`
	Token      string     `json:"token"`
}

func addProjectionFreshnessDiagnostics(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability) {
	page, ok := listDiagnosticProjectionPage(ctx, result, req, capability)
	if !ok {
		return
	}
	stats := newProjectionFreshnessStats(capability, page.namespace)
	viewFreshness := newProjectionViewFreshnessStats(capability, page.namespace)
	var staleness *projectionStalenessStats
	if capability == CapabilityDocumentChunks {
		next := newProjectionStalenessStats(capability, page.namespace)
		staleness = &next
	}
	var sourceStaleness *projectionSourceStalenessStats
	if capability == CapabilityMessageIndex || capability == CapabilitySummaryDAG {
		next := newProjectionSourceStalenessStats(capability, page.namespace)
		sourceStaleness = &next
	}
	for _, doc := range page.docs {
		stats.recordsScanned++
		sourceRefs, sourceRefsFound, sourceRefsErr := indexed.DecodeSourceRefs(doc.Metadata)
		if sourceRefsErr != nil {
			stats.invalidSourceRefs++
			stats.noteError(doc.ID, sourceRefsErr)
		} else if !sourceRefsFound {
			stats.missingSourceRefs++
		}
		signature, signatureFound, signatureErr := indexed.DecodeSignature(doc.Metadata)
		if signatureErr != nil {
			stats.invalidSignature++
			stats.noteError(doc.ID, signatureErr)
		} else if !signatureFound {
			stats.missingSignature++
		}
		viewFreshness.recordsScanned++
		if sourceRefsErr != nil || signatureErr != nil || !sourceRefsFound || (capability != CapabilityMessageIndex && !signatureFound) {
			viewFreshness.recordsSkipped++
		} else {
			compareProjectionViewFreshness(ctx, req, capability, doc, sourceRefs, signature, &viewFreshness)
		}
		if staleness != nil {
			staleness.recordsScanned++
			if sourceRefsErr != nil || signatureErr != nil || !signatureFound {
				staleness.recordsSkipped++
				continue
			}
			compareDocumentChunkProjectionStaleness(ctx, req, doc, sourceRefs, signature, staleness)
		}
		if sourceStaleness != nil {
			sourceStaleness.recordsScanned++
			if sourceRefsErr != nil || !sourceRefsFound || signatureErr != nil || (capability == CapabilitySummaryDAG && !signatureFound) {
				sourceStaleness.recordsSkipped++
				continue
			}
			compareProjectionSourceStaleness(ctx, req, capability, doc, sourceRefs, sourceStaleness)
		}
	}
	stats.nextPageToken = page.nextPageToken
	stats.total = page.total
	result.Checks = append(result.Checks, stats.check(req.Scope))
	viewFreshness.nextPageToken = page.nextPageToken
	viewFreshness.total = page.total
	result.Checks = append(result.Checks, viewFreshness.check(req.Scope))
	if staleness != nil {
		staleness.nextPageToken = page.nextPageToken
		staleness.total = page.total
		result.Checks = append(result.Checks, staleness.check(req.Scope))
	}
	if sourceStaleness != nil {
		sourceStaleness.nextPageToken = page.nextPageToken
		sourceStaleness.total = page.total
		result.Checks = append(result.Checks, sourceStaleness.check(req.Scope))
	}
	setDiagnosticProjectionNextPageToken(result, capability, page.namespace, page.nextPageToken)
}

func addProjectionConsistencyDiagnostics(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability) {
	page, ok := listDiagnosticProjectionPage(ctx, result, req, capability)
	if !ok {
		return
	}
	stats := newProjectionConsistencyStats(capability, page.namespace)
	for _, doc := range page.docs {
		stats.recordsScanned++
		if err := hydrateDiagnosticProjectionDoc(ctx, req, capability, doc); err != nil {
			stats.noteHydrateError(doc, err)
			continue
		}
		stats.recordsHydrated++
	}
	stats.nextPageToken = page.nextPageToken
	stats.total = page.total
	result.Checks = append(result.Checks, stats.check(req.Scope))
	setDiagnosticProjectionNextPageToken(result, capability, page.namespace, page.nextPageToken)
}

func listDiagnosticProjectionPage(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability) (diagnosticProjectionPage, bool) {
	if result == nil || req.System == nil || req.Deps.Index == nil {
		return diagnosticProjectionPage{}, false
	}
	if !readProjectionConfigured(req.System.assembly, req.Deps, capability) {
		return diagnosticProjectionPage{}, false
	}
	if req.Scope.IsZero() {
		return diagnosticProjectionPage{}, false
	}
	namespace, err := diagnosticProjectionNamespace(req, capability)
	if err != nil {
		result.Checks = append(result.Checks, newDiagnosticCheck("projection."+string(capability)+".scan", capability, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "projection namespace lookup failed", map[string]any{"error": err.Error()}))
		return diagnosticProjectionPage{}, false
	}
	pageToken, skip, err := diagnosticProjectionScanPageToken(req, capability, namespace)
	if err != nil {
		result.Checks = append(result.Checks, newDiagnosticCheck("projection."+string(capability)+".scan", capability, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "projection bounded scan failed", map[string]any{"namespace": namespace, "error": err.Error()}))
		return diagnosticProjectionPage{}, false
	}
	if skip {
		return diagnosticProjectionPage{}, false
	}
	resp, err := req.Deps.Index.List(ctx, namespace, retrieval.ListRequest{
		Filter:    diagnosticProjectionFilter(capability, req.Scope),
		PageSize:  req.PageSize,
		PageToken: pageToken,
		OrderBy:   retrieval.OrderByIDAsc,
	})
	if err != nil {
		result.Checks = append(result.Checks, newDiagnosticCheck("projection."+string(capability)+".scan", capability, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "projection bounded scan failed", map[string]any{"namespace": namespace, "error": err.Error()}))
		return diagnosticProjectionPage{}, false
	}
	if resp == nil {
		result.Checks = append(result.Checks, newDiagnosticCheck("projection."+string(capability)+".scan", capability, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "projection bounded scan returned nil response", map[string]any{"namespace": namespace}))
		return diagnosticProjectionPage{}, false
	}
	return diagnosticProjectionPage{
		namespace:     namespace,
		docs:          append([]retrieval.Doc(nil), resp.Items...),
		nextPageToken: resp.NextPageToken,
		total:         resp.Total,
	}, true
}

func diagnosticProjectionNamespace(req DiagnosticProbeRequest, capability Capability) (string, error) {
	if req.System == nil {
		return "", errdefs.NotAvailablef("memory: diagnostics requires a configured system")
	}
	if namespace, err := req.System.scopedReadNamespace(capability, req.Scope); err != nil {
		return "", err
	} else if namespace != "" {
		return namespace, nil
	}
	base, ok := req.System.assembly.ProjectionNamespace(capability)
	if !ok {
		return "", errdefs.NotAvailablef("memory: projection namespace for capability %q is not configured", capability)
	}
	return projectors.ScopedNamespace(base, req.Scope)
}

func diagnosticProjectionFilter(capability Capability, scope Scope) retrieval.Filter {
	switch capability {
	case CapabilityMessageIndex:
		return messageScopeFilter(scope)
	case CapabilitySummaryDAG:
		return summaryScopeFilter(scope)
	case CapabilityDocumentChunks:
		return documentScopeFilter(scope)
	default:
		return retrieval.Filter{}
	}
}

type projectionFreshnessStats struct {
	capability        Capability
	namespace         string
	recordsScanned    int
	missingSourceRefs int
	invalidSourceRefs int
	missingSignature  int
	invalidSignature  int
	nextPageToken     string
	total             int64
	firstErrorDocID   string
	firstError        string
}

func newProjectionFreshnessStats(capability Capability, namespace string) projectionFreshnessStats {
	return projectionFreshnessStats{capability: capability, namespace: namespace}
}

func (s *projectionFreshnessStats) noteError(docID string, err error) {
	if s == nil || err == nil || s.firstError != "" {
		return
	}
	s.firstErrorDocID = docID
	s.firstError = err.Error()
}

func (s projectionFreshnessStats) check(scope Scope) DiagnosticCheck {
	status := DiagnosticStatusOK
	severity := DiagnosticSeverityInfo
	ok := true
	message := "projection metadata scanned"
	if s.invalidSourceRefs > 0 || s.invalidSignature > 0 {
		status = DiagnosticStatusError
		severity = DiagnosticSeverityError
		ok = false
		message = "projection metadata contains invalid indexed freshness metadata"
	} else if s.missingSourceRefs > 0 || s.missingSignature > 0 {
		status = DiagnosticStatusWarning
		severity = DiagnosticSeverityWarning
		ok = false
		message = "projection metadata is missing indexed freshness metadata"
	}
	details := map[string]any{
		"namespace":           s.namespace,
		"records_scanned":     s.recordsScanned,
		"missing_source_refs": s.missingSourceRefs,
		"invalid_source_refs": s.invalidSourceRefs,
		"missing_signature":   s.missingSignature,
		"invalid_signature":   s.invalidSignature,
		"next_page_token":     s.nextPageToken,
		"total":               s.total,
	}
	if s.firstError != "" {
		details["first_error_doc_id"] = s.firstErrorDocID
		details["first_error"] = s.firstError
	}
	return newDiagnosticCheck("projection."+string(s.capability)+".freshness_metadata", s.capability, scope, LifecycleTarget{}, status, severity, ok, message, details)
}

type projectionViewFreshnessStats struct {
	capability        Capability
	namespace         string
	recordsScanned    int
	recordsCompared   int
	recordsSkipped    int
	staleRecords      int
	nextPageToken     string
	total             int64
	firstStaleDocID   string
	firstStaleReason  string
	firstStaleDetails []string
}

func newProjectionViewFreshnessStats(capability Capability, namespace string) projectionViewFreshnessStats {
	return projectionViewFreshnessStats{capability: capability, namespace: namespace}
}

func (s *projectionViewFreshnessStats) noteSkipped() {
	if s == nil {
		return
	}
	s.recordsSkipped++
}

func (s *projectionViewFreshnessStats) noteStale(docID, reason string, details []string) {
	if s == nil {
		return
	}
	s.staleRecords++
	if s.firstStaleDocID == "" {
		s.firstStaleDocID = docID
		s.firstStaleReason = reason
		s.firstStaleDetails = append([]string(nil), details...)
	}
}

func (s projectionViewFreshnessStats) check(scope Scope) DiagnosticCheck {
	status := DiagnosticStatusOK
	severity := DiagnosticSeverityInfo
	ok := true
	message := "projection records match canonical view records"
	if s.staleRecords > 0 {
		status = DiagnosticStatusStale
		severity = DiagnosticSeverityWarning
		ok = false
		message = "projection records are stale relative to canonical view records"
	}
	details := map[string]any{
		"namespace":        s.namespace,
		"records_scanned":  s.recordsScanned,
		"records_compared": s.recordsCompared,
		"records_skipped":  s.recordsSkipped,
		"stale_records":    s.staleRecords,
		"next_page_token":  s.nextPageToken,
		"total":            s.total,
	}
	if s.firstStaleDocID != "" {
		details["first_stale_doc_id"] = s.firstStaleDocID
		details["first_stale_reason"] = s.firstStaleReason
	}
	if len(s.firstStaleDetails) > 0 {
		details["first_stale_details"] = append([]string(nil), s.firstStaleDetails...)
	}
	return newDiagnosticCheck("projection."+string(s.capability)+".view_freshness", s.capability, scope, LifecycleTarget{}, status, severity, ok, message, details)
}

type projectionStalenessStats struct {
	capability               Capability
	namespace                string
	recordsScanned           int
	recordsCompared          int
	recordsSkipped           int
	staleRecords             int
	missingDocumentRevisions int
	canonicalMisses          int
	compareErrors            int
	nextPageToken            string
	total                    int64
	firstErrorDocID          string
	firstError               string
	affectedDocuments        []any
}

func newProjectionStalenessStats(capability Capability, namespace string) projectionStalenessStats {
	return projectionStalenessStats{capability: capability, namespace: namespace}
}

func (s *projectionStalenessStats) noteCompareError(docID string, err error) {
	if s == nil || err == nil {
		return
	}
	s.compareErrors++
	if s.firstError == "" {
		s.firstErrorDocID = docID
		s.firstError = err.Error()
	}
}

func (s *projectionStalenessStats) noteStaleRecord(projectionID string, record map[string]any) {
	if s == nil {
		return
	}
	s.staleRecords++
	if record == nil {
		record = map[string]any{}
	}
	record["projection_id"] = projectionID
	s.affectedDocuments = append(s.affectedDocuments, record)
}

func (s projectionStalenessStats) check(scope Scope) DiagnosticCheck {
	status := DiagnosticStatusOK
	severity := DiagnosticSeverityInfo
	ok := true
	message := "projection records match canonical document revisions"
	if s.compareErrors > 0 {
		status = DiagnosticStatusError
		severity = DiagnosticSeverityError
		ok = false
		message = "projection staleness compare failed"
	} else if s.staleRecords > 0 || s.canonicalMisses > 0 {
		status = DiagnosticStatusStale
		severity = DiagnosticSeverityWarning
		ok = false
		message = "projection records are stale relative to canonical documents"
	}
	details := map[string]any{
		"namespace":                  s.namespace,
		"records_scanned":            s.recordsScanned,
		"records_compared":           s.recordsCompared,
		"records_skipped":            s.recordsSkipped,
		"stale_records":              s.staleRecords,
		"missing_document_revisions": s.missingDocumentRevisions,
		"canonical_misses":           s.canonicalMisses,
		"compare_errors":             s.compareErrors,
		"next_page_token":            s.nextPageToken,
		"total":                      s.total,
	}
	if len(s.affectedDocuments) > 0 {
		details["affected_documents"] = append([]any(nil), s.affectedDocuments...)
	}
	if s.firstError != "" {
		details["first_error_doc_id"] = s.firstErrorDocID
		details["first_error"] = s.firstError
	}
	check := newDiagnosticCheck("projection."+string(s.capability)+".staleness", s.capability, scope, diagnosticTargetFromAffectedDocuments(s.capability, s.affectedDocuments), status, severity, ok, message, details)
	if !ok {
		check.RepairHint = "rebuild document_chunks for affected documents"
	}
	return check
}

type projectionSourceStalenessStats struct {
	capability             Capability
	namespace              string
	recordsScanned         int
	recordsCompared        int
	recordsSkipped         int
	staleRecords           int
	missingMessageRefs     int
	missingSourceRevisions int
	invalidSourceRevisions int
	canonicalMisses        int
	compareErrors          int
	nextPageToken          string
	total                  int64
	firstErrorDocID        string
	firstError             string
	affectedSources        []any
}

func newProjectionSourceStalenessStats(capability Capability, namespace string) projectionSourceStalenessStats {
	return projectionSourceStalenessStats{capability: capability, namespace: namespace}
}

func (s *projectionSourceStalenessStats) noteCompareError(docID string, err error) {
	if s == nil || err == nil {
		return
	}
	s.compareErrors++
	if s.firstError == "" {
		s.firstErrorDocID = docID
		s.firstError = err.Error()
	}
}

func (s *projectionSourceStalenessStats) noteStaleProjection(projectionID string, affected []any) {
	if s == nil {
		return
	}
	s.staleRecords++
	if len(affected) == 0 {
		s.affectedSources = append(s.affectedSources, map[string]any{"projection_id": projectionID})
		return
	}
	for _, item := range affected {
		record, ok := item.(map[string]any)
		if !ok || record == nil {
			record = map[string]any{}
		}
		record["projection_id"] = projectionID
		s.affectedSources = append(s.affectedSources, record)
	}
}

func (s *projectionSourceStalenessStats) noteInvalidSourceRevision(projectionID string, revision diagnosticInvalidSourceRevision) {
	if s == nil {
		return
	}
	s.invalidSourceRevisions++
	s.affectedSources = append(s.affectedSources, map[string]any{
		"projection_id": projectionID,
		"kind":          string(revision.kind),
		"source_key":    revision.sourceKey,
		"reason":        revision.reason,
	})
}

func (s *projectionSourceStalenessStats) noteMissingSourceRevision(projectionID string, revision diagnosticMissingSourceRevision) {
	if s == nil {
		return
	}
	s.missingSourceRevisions++
	s.affectedSources = append(s.affectedSources, map[string]any{
		"projection_id": projectionID,
		"kind":          string(revision.kind),
		"source_key":    revision.sourceKey,
		"reason":        revision.reason,
	})
}

func (s projectionSourceStalenessStats) check(scope Scope) DiagnosticCheck {
	status := DiagnosticStatusOK
	severity := DiagnosticSeverityInfo
	ok := true
	message := "projection records match canonical message sources"
	if s.compareErrors > 0 {
		status = DiagnosticStatusError
		severity = DiagnosticSeverityError
		ok = false
		message = "projection source staleness compare failed"
	} else if s.staleRecords > 0 || s.canonicalMisses > 0 || s.invalidSourceRevisions > 0 || s.missingSourceRevisions > 0 {
		status = DiagnosticStatusStale
		severity = DiagnosticSeverityWarning
		ok = false
		message = "projection records are stale or invalid relative to canonical message sources"
	}
	details := map[string]any{
		"namespace":                s.namespace,
		"records_scanned":          s.recordsScanned,
		"records_compared":         s.recordsCompared,
		"records_skipped":          s.recordsSkipped,
		"stale_records":            s.staleRecords,
		"missing_message_refs":     s.missingMessageRefs,
		"missing_source_revisions": s.missingSourceRevisions,
		"invalid_source_revisions": s.invalidSourceRevisions,
		"canonical_misses":         s.canonicalMisses,
		"compare_errors":           s.compareErrors,
		"next_page_token":          s.nextPageToken,
		"total":                    s.total,
	}
	if len(s.affectedSources) > 0 {
		details["affected_sources"] = append([]any(nil), s.affectedSources...)
	}
	if s.firstError != "" {
		details["first_error_doc_id"] = s.firstErrorDocID
		details["first_error"] = s.firstError
	}
	return newDiagnosticCheck("projection."+string(s.capability)+".source_staleness", s.capability, scope, LifecycleTarget{}, status, severity, ok, message, details)
}

func diagnosticDocumentTargetDetailsFromMetadata(metadata map[string]any) (map[string]any, bool) {
	if metadata == nil {
		return nil, false
	}
	datasetID, _ := metadata[projectors.MetadataDatasetIDKey].(string)
	documentID, _ := metadata[projectors.MetadataDocumentIDKey].(string)
	datasetID = strings.TrimSpace(datasetID)
	documentID = strings.TrimSpace(documentID)
	if datasetID == "" || documentID == "" {
		return nil, false
	}
	record := map[string]any{
		"dataset_id":  datasetID,
		"document_id": documentID,
	}
	if chunkID, _ := metadata[projectors.MetadataChunkIDKey].(string); strings.TrimSpace(chunkID) != "" {
		record["chunk_id"] = strings.TrimSpace(chunkID)
	}
	return record, true
}

func diagnosticTargetFromAffectedDocuments(capability Capability, affected []any) LifecycleTarget {
	if capability != CapabilityDocumentChunks || len(affected) == 0 {
		return LifecycleTarget{}
	}
	var target LifecycleTarget
	for _, item := range affected {
		record, ok := item.(map[string]any)
		if !ok {
			return LifecycleTarget{}
		}
		datasetID, _ := record["dataset_id"].(string)
		documentID, _ := record["document_id"].(string)
		next := LifecycleTarget{
			Kind:       "document",
			Capability: capability,
			DatasetID:  strings.TrimSpace(datasetID),
			DocumentID: strings.TrimSpace(documentID),
		}
		if next.DatasetID == "" || next.DocumentID == "" {
			return LifecycleTarget{}
		}
		if target == (LifecycleTarget{}) {
			target = next
			continue
		}
		if target.DatasetID != next.DatasetID || target.DocumentID != next.DocumentID {
			return LifecycleTarget{}
		}
	}
	return target
}

func diagnosticHydrationFailureReason(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(err.Error(), "metadata") {
		return "missing_metadata"
	}
	if errdefs.IsNotAvailable(err) {
		return "canonical_missing"
	}
	return "hydrate_error"
}

func compareProjectionViewFreshness(ctx context.Context, req DiagnosticProbeRequest, capability Capability, doc retrieval.Doc, refs []views.SourceRef, signature views.ViewSignature, stats *projectionViewFreshnessStats) {
	if stats == nil {
		return
	}
	canonicalRefs, canonicalSignature, compareSignature, ok := hydrateDiagnosticProjectionViewRecord(ctx, req, capability, doc)
	if !ok {
		stats.noteSkipped()
		return
	}
	stats.recordsCompared++
	mismatches := make([]string, 0, 2)
	if !sourceRefsEqual(refs, canonicalRefs) {
		mismatches = append(mismatches, "source_refs")
	}
	if compareSignature && signature.IsStaleAgainst(canonicalSignature) {
		mismatches = append(mismatches, "signature")
	}
	if len(mismatches) == 0 {
		return
	}
	reason := strings.Join(mismatches, "_")
	stats.noteStale(doc.ID, reason, mismatches)
}

func hydrateDiagnosticProjectionViewRecord(ctx context.Context, req DiagnosticProbeRequest, capability Capability, doc retrieval.Doc) ([]views.SourceRef, views.ViewSignature, bool, bool) {
	switch capability {
	case CapabilityMessageIndex:
		if req.Deps.MessageStore == nil {
			return nil, views.ViewSignature{}, false, false
		}
		conversationID, err := diagnosticMetadataString(doc, projectors.MetadataConversationIDKey)
		if err != nil {
			return nil, views.ViewSignature{}, false, false
		}
		messageID, err := diagnosticMetadataString(doc, projectors.MetadataMessageIDKey)
		if err != nil {
			return nil, views.ViewSignature{}, false, false
		}
		msg, ok, err := req.Deps.MessageStore.Get(ctx, conversationID, messageID)
		if err != nil || !ok {
			return nil, views.ViewSignature{}, false, false
		}
		return []views.SourceRef{{
			Kind: views.SourceMessage,
			Message: &views.MessageSourceRef{
				ConversationID: msg.ConversationID,
				MessageID:      msg.ID,
			},
		}}, views.ViewSignature{}, false, true
	case CapabilityDocumentChunks:
		if req.Deps.ChunkStore == nil {
			return nil, views.ViewSignature{}, false, false
		}
		datasetID, err := diagnosticMetadataString(doc, projectors.MetadataDatasetIDKey)
		if err != nil {
			return nil, views.ViewSignature{}, false, false
		}
		scope, err := diagnosticMetadataScope(doc)
		if err != nil {
			return nil, views.ViewSignature{}, false, false
		}
		scope.DatasetID = datasetID
		documentID, err := diagnosticMetadataString(doc, projectors.MetadataDocumentIDKey)
		if err != nil {
			return nil, views.ViewSignature{}, false, false
		}
		chunkID, err := diagnosticMetadataString(doc, projectors.MetadataChunkIDKey)
		if err != nil {
			return nil, views.ViewSignature{}, false, false
		}
		chunk, ok, err := req.Deps.ChunkStore.GetChunk(ctx, scope, documentID, viewdocument.ChunkID(chunkID))
		if err != nil || !ok {
			return nil, views.ViewSignature{}, false, false
		}
		return []views.SourceRef{chunk.SourceRef}, chunk.Signature, true, true
	case CapabilitySummaryDAG:
		if req.Deps.SummaryStore == nil {
			return nil, views.ViewSignature{}, false, false
		}
		scope, err := diagnosticMetadataScope(doc)
		if err != nil {
			return nil, views.ViewSignature{}, false, false
		}
		nodeID, err := diagnosticMetadataString(doc, projectors.MetadataNodeIDKey)
		if err != nil {
			return nil, views.ViewSignature{}, false, false
		}
		node, ok, err := req.Deps.SummaryStore.GetNode(ctx, scope, recent.NodeID(nodeID))
		if err != nil || !ok {
			return nil, views.ViewSignature{}, false, false
		}
		return node.SourceRefs, node.Signature, true, true
	default:
		return nil, views.ViewSignature{}, false, false
	}
}

func sourceRefsEqual(left, right []views.SourceRef) bool {
	if len(left) != len(right) {
		return false
	}
	leftKeys, ok := sourceRefComparisonKeys(left)
	if !ok {
		return false
	}
	rightKeys, ok := sourceRefComparisonKeys(right)
	if !ok {
		return false
	}
	sort.Strings(leftKeys)
	sort.Strings(rightKeys)
	for i := range leftKeys {
		if leftKeys[i] != rightKeys[i] {
			return false
		}
	}
	return true
}

func sourceRefComparisonKeys(refs []views.SourceRef) ([]string, bool) {
	out := make([]string, len(refs))
	for i, ref := range refs {
		key, err := ref.StableKeyE()
		if err != nil {
			return nil, false
		}
		if ref.Document != nil {
			key += "\x00" + ref.Document.Version + "\x00" + ref.Document.ContentHash
		}
		out[i] = key
	}
	return out, true
}

type diagnosticDocumentRevision struct {
	datasetID   string
	documentID  string
	version     string
	contentHash string
	sourceKey   string
}

func compareDocumentChunkProjectionStaleness(ctx context.Context, req DiagnosticProbeRequest, doc retrieval.Doc, refs []views.SourceRef, signature views.ViewSignature, stats *projectionStalenessStats) {
	if stats == nil {
		return
	}
	if req.Deps.DocumentStore == nil {
		stats.noteCompareError(doc.ID, errdefs.NotAvailablef("memory: document store is not configured"))
		return
	}
	revisions := diagnosticDocumentRevisions(refs, signature)
	if len(revisions) == 0 {
		stats.missingDocumentRevisions++
		stats.recordsSkipped++
		return
	}
	stats.recordsCompared++
	for _, revision := range revisions {
		current, ok, err := req.Deps.DocumentStore.Get(ctx, revision.datasetID, revision.documentID)
		if err != nil {
			stats.noteCompareError(doc.ID, err)
			return
		}
		if !ok {
			stats.canonicalMisses++
			stats.noteStaleRecord(doc.ID, map[string]any{
				"dataset_id":           revision.datasetID,
				"document_id":          revision.documentID,
				"indexed_version":      revision.version,
				"indexed_content_hash": revision.contentHash,
				"source_key":           revision.sourceKey,
				"reason":               "canonical_missing",
			})
			continue
		}
		currentVersion := strconv.FormatUint(current.Version, 10)
		mismatches := make([]string, 0, 2)
		if revision.version != "" && revision.version != currentVersion {
			mismatches = append(mismatches, "version")
		}
		if revision.contentHash != "" && revision.contentHash != current.ContentHash {
			mismatches = append(mismatches, "content_hash")
		}
		if len(mismatches) == 0 {
			continue
		}
		stats.noteStaleRecord(doc.ID, map[string]any{
			"dataset_id":            revision.datasetID,
			"document_id":           revision.documentID,
			"indexed_version":       revision.version,
			"current_version":       currentVersion,
			"indexed_content_hash":  revision.contentHash,
			"current_content_hash":  current.ContentHash,
			"source_key":            revision.sourceKey,
			"mismatched_dimensions": append([]string(nil), mismatches...),
		})
	}
}

type diagnosticMessageRevision struct {
	conversationID string
	messageID      string
	revision       string
	contentHash    string
	sourceKey      string
}

type diagnosticInvalidSourceRevision struct {
	kind      views.SourceKind
	sourceKey string
	reason    string
}

type diagnosticMissingSourceRevision struct {
	kind      views.SourceKind
	sourceKey string
	reason    string
}

func compareProjectionSourceStaleness(ctx context.Context, req DiagnosticProbeRequest, capability Capability, doc retrieval.Doc, refs []views.SourceRef, stats *projectionSourceStalenessStats) {
	switch capability {
	case CapabilityMessageIndex:
		compareMessageIndexProjectionSourceStaleness(ctx, req, doc, refs, stats)
	case CapabilitySummaryDAG:
		compareSummaryDAGProjectionSourceStaleness(ctx, req, doc, stats)
	}
}

func compareMessageIndexProjectionSourceStaleness(ctx context.Context, req DiagnosticProbeRequest, doc retrieval.Doc, refs []views.SourceRef, stats *projectionSourceStalenessStats) {
	if stats == nil {
		return
	}
	if req.Deps.MessageStore == nil {
		stats.noteCompareError(doc.ID, errdefs.NotAvailablef("memory: message store is not configured"))
		return
	}
	messageRefs := diagnosticMessageRefs(refs)
	if len(messageRefs) == 0 {
		stats.missingMessageRefs++
		stats.recordsSkipped++
		return
	}

	stats.recordsCompared++
	affected := make([]any, 0, len(messageRefs))
	hasMissingRef := false
	for _, ref := range messageRefs {
		if _, ok, err := req.Deps.MessageStore.Get(ctx, ref.conversationID, ref.messageID); err != nil {
			stats.noteCompareError(doc.ID, err)
			return
		} else if !ok {
			stats.canonicalMisses++
			hasMissingRef = true
			affected = append(affected, map[string]any{
				"conversation_id": ref.conversationID,
				"message_id":      ref.messageID,
				"source_key":      ref.sourceKey,
				"reason":          "canonical_missing",
			})
		}
	}
	if hasMissingRef {
		stats.noteStaleProjection(doc.ID, affected)
		return
	}

	scope, current, ok, err := diagnosticProjectionMessage(ctx, req, doc)
	if err != nil {
		stats.noteCompareError(doc.ID, err)
		return
	}
	if !ok {
		return
	}
	canonicalRecords, err := projectors.SourceMessageRecords(scope, current)
	if err != nil {
		stats.noteCompareError(doc.ID, err)
		return
	}
	var canonical *indexed.Record
	for i := range canonicalRecords {
		if canonicalRecords[i].ID == doc.ID {
			canonical = &canonicalRecords[i]
			break
		}
	}
	if canonical == nil {
		stats.noteStaleProjection(doc.ID, []any{map[string]any{
			"conversation_id": current.ConversationID,
			"message_id":      current.ID,
			"reason":          "projection_record_id_not_current",
		}})
		return
	}
	mismatches := make([]string, 0, 2)
	if doc.Content != canonical.Text {
		mismatches = append(mismatches, "content")
	}
	if !sourceRefsEqual(refs, canonical.SourceRefs) {
		mismatches = append(mismatches, "source_refs")
	}
	if len(mismatches) == 0 {
		return
	}
	sourceKey := ""
	if len(canonical.SourceRefs) > 0 {
		sourceKey = canonical.SourceRefs[0].StableKey()
	}
	stats.noteStaleProjection(doc.ID, []any{map[string]any{
		"conversation_id":       current.ConversationID,
		"message_id":            current.ID,
		"source_key":            sourceKey,
		"mismatched_dimensions": append([]string(nil), mismatches...),
	}})
}

func diagnosticProjectionMessage(ctx context.Context, req DiagnosticProbeRequest, doc retrieval.Doc) (Scope, sourcemessage.Message, bool, error) {
	scope, err := diagnosticMetadataScope(doc)
	if err != nil {
		return Scope{}, sourcemessage.Message{}, false, nil
	}
	conversationID, err := diagnosticMetadataString(doc, projectors.MetadataConversationIDKey)
	if err != nil {
		return Scope{}, sourcemessage.Message{}, false, nil
	}
	messageID, err := diagnosticMetadataString(doc, projectors.MetadataMessageIDKey)
	if err != nil {
		return Scope{}, sourcemessage.Message{}, false, nil
	}
	current, ok, err := req.Deps.MessageStore.Get(ctx, conversationID, messageID)
	if err != nil {
		return Scope{}, sourcemessage.Message{}, false, err
	}
	if !ok {
		return Scope{}, sourcemessage.Message{}, false, nil
	}
	scope.ConversationID = conversationID
	return scope, current, true, nil
}

func compareSummaryDAGProjectionSourceStaleness(ctx context.Context, req DiagnosticProbeRequest, doc retrieval.Doc, stats *projectionSourceStalenessStats) {
	if stats == nil {
		return
	}
	if req.Deps.MessageStore == nil {
		stats.noteCompareError(doc.ID, errdefs.NotAvailablef("memory: message store is not configured"))
		return
	}
	node, ok, err := hydrateDiagnosticSummaryNodeForSourceStaleness(ctx, req, doc)
	if err != nil {
		stats.noteCompareError(doc.ID, err)
		return
	}
	if !ok {
		stats.recordsSkipped++
		return
	}
	revisions, invalidRevisions, missingRevisions := diagnosticMessageRevisions(node.SourceRefs, node.Signature)
	for _, invalid := range invalidRevisions {
		stats.noteInvalidSourceRevision(doc.ID, invalid)
	}
	for _, missing := range missingRevisions {
		stats.noteMissingSourceRevision(doc.ID, missing)
	}
	if len(revisions) == 0 {
		if len(missingRevisions) == 0 {
			stats.missingSourceRevisions++
		}
		stats.recordsSkipped++
		return
	}

	stats.recordsCompared++
	affected := make([]any, 0, len(revisions))
	for _, revision := range revisions {
		current, ok, err := req.Deps.MessageStore.Get(ctx, revision.conversationID, revision.messageID)
		if err != nil {
			stats.noteCompareError(doc.ID, err)
			return
		}
		if !ok {
			stats.canonicalMisses++
			affected = append(affected, map[string]any{
				"conversation_id":      revision.conversationID,
				"message_id":           revision.messageID,
				"indexed_revision":     revision.revision,
				"indexed_content_hash": revision.contentHash,
				"source_key":           revision.sourceKey,
				"reason":               "canonical_missing",
			})
			continue
		}

		currentRevision := strconv.FormatUint(current.Seq, 10)
		currentContentHash := summaryderive.MessageContentHash(current)
		mismatches := make([]string, 0, 2)
		if revision.revision != "" && revision.revision != currentRevision {
			mismatches = append(mismatches, "revision")
		}
		if revision.contentHash != "" && revision.contentHash != currentContentHash {
			mismatches = append(mismatches, "content_hash")
		}
		if len(mismatches) == 0 {
			continue
		}
		affected = append(affected, map[string]any{
			"conversation_id":       revision.conversationID,
			"message_id":            revision.messageID,
			"indexed_revision":      revision.revision,
			"current_revision":      currentRevision,
			"indexed_content_hash":  revision.contentHash,
			"current_content_hash":  currentContentHash,
			"source_key":            revision.sourceKey,
			"mismatched_dimensions": append([]string(nil), mismatches...),
		})
	}
	if len(affected) == 0 {
		return
	}
	stats.noteStaleProjection(doc.ID, affected)
}

func hydrateDiagnosticSummaryNodeForSourceStaleness(ctx context.Context, req DiagnosticProbeRequest, doc retrieval.Doc) (recent.SummaryNode, bool, error) {
	if req.Deps.SummaryStore == nil {
		return recent.SummaryNode{}, false, errdefs.NotAvailablef("memory: summary store is not configured")
	}
	scope, err := diagnosticMetadataScope(doc)
	if err != nil {
		return recent.SummaryNode{}, false, nil
	}
	nodeID, err := diagnosticMetadataString(doc, projectors.MetadataNodeIDKey)
	if err != nil {
		return recent.SummaryNode{}, false, nil
	}
	node, ok, err := req.Deps.SummaryStore.GetNode(ctx, scope, recent.NodeID(nodeID))
	if err != nil {
		return recent.SummaryNode{}, false, err
	}
	return node, ok, nil
}

func diagnosticDocumentRevisions(refs []views.SourceRef, signature views.ViewSignature) []diagnosticDocumentRevision {
	refByKey := make(map[string]views.DocumentSourceRef, len(refs))
	for _, ref := range refs {
		if ref.Kind != views.SourceDocument || ref.Document == nil {
			continue
		}
		key, err := ref.StableKeyE()
		if err != nil {
			continue
		}
		refByKey[key] = *ref.Document
	}

	out := make([]diagnosticDocumentRevision, 0, len(signature.SourceRevisions))
	seen := map[string]struct{}{}
	for _, sourceRevision := range signature.SourceRevisions {
		if sourceRevision.Kind != views.SourceDocument {
			continue
		}
		revision := diagnosticDocumentRevision{
			version:     sourceRevision.Revision,
			contentHash: sourceRevision.ContentHash,
			sourceKey:   sourceRevision.SourceKey,
		}
		if ref, ok := refByKey[sourceRevision.SourceKey]; ok {
			revision.datasetID = ref.DatasetID
			revision.documentID = ref.DocumentID
			if revision.version == "" {
				revision.version = ref.Version
			}
			if revision.contentHash == "" {
				revision.contentHash = ref.ContentHash
			}
		} else if parsed, ok := diagnosticDocumentIdentityFromSourceKey(sourceRevision.SourceKey); ok {
			revision.datasetID = parsed.datasetID
			revision.documentID = parsed.documentID
		}
		if revision.datasetID == "" || revision.documentID == "" {
			continue
		}
		key := revision.datasetID + "\x00" + revision.documentID + "\x00" + revision.sourceKey
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, revision)
	}
	return out
}

func diagnosticMessageRefs(refs []views.SourceRef) []diagnosticMessageRevision {
	out := make([]diagnosticMessageRevision, 0, len(refs))
	seen := map[string]struct{}{}
	for _, ref := range refs {
		if ref.Kind != views.SourceMessage || ref.Message == nil {
			continue
		}
		key, err := ref.StableKeyE()
		if err != nil {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, diagnosticMessageRevision{
			conversationID: ref.Message.ConversationID,
			messageID:      ref.Message.MessageID,
			sourceKey:      key,
		})
	}
	return out
}

func diagnosticMessageRevisions(refs []views.SourceRef, signature views.ViewSignature) ([]diagnosticMessageRevision, []diagnosticInvalidSourceRevision, []diagnosticMissingSourceRevision) {
	refByKey := make(map[string]views.MessageSourceRef, len(refs))
	for _, ref := range refs {
		if ref.Kind != views.SourceMessage || ref.Message == nil {
			continue
		}
		key, err := ref.StableKeyE()
		if err != nil {
			continue
		}
		refByKey[key] = *ref.Message
	}

	out := make([]diagnosticMessageRevision, 0, len(signature.SourceRevisions))
	invalid := make([]diagnosticInvalidSourceRevision, 0)
	missing := make([]diagnosticMissingSourceRevision, 0)
	seen := map[string]struct{}{}
	coveredSourceKeys := make(map[string]struct{}, len(signature.SourceRevisions))
	for _, sourceRevision := range signature.SourceRevisions {
		if sourceRevision.Kind != views.SourceMessage {
			continue
		}
		coveredSourceKeys[sourceRevision.SourceKey] = struct{}{}
		revision := diagnosticMessageRevision{
			revision:    sourceRevision.Revision,
			contentHash: sourceRevision.ContentHash,
			sourceKey:   sourceRevision.SourceKey,
		}
		if ref, ok := refByKey[sourceRevision.SourceKey]; ok {
			revision.conversationID = ref.ConversationID
			revision.messageID = ref.MessageID
		} else if parsed, ok := diagnosticMessageIdentityFromSourceKey(sourceRevision.SourceKey); ok {
			revision.conversationID = parsed.conversationID
			revision.messageID = parsed.messageID
			if len(refByKey) > 0 {
				invalid = append(invalid, diagnosticInvalidSourceRevision{
					kind:      sourceRevision.Kind,
					sourceKey: sourceRevision.SourceKey,
					reason:    "source_ref_mismatch",
				})
			}
		}
		if revision.conversationID == "" || revision.messageID == "" {
			invalid = append(invalid, diagnosticInvalidSourceRevision{
				kind:      sourceRevision.Kind,
				sourceKey: sourceRevision.SourceKey,
				reason:    "unresolved_source_revision",
			})
			continue
		}
		key := revision.conversationID + "\x00" + revision.messageID + "\x00" + revision.sourceKey
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, revision)
	}
	for sourceKey := range refByKey {
		if _, ok := coveredSourceKeys[sourceKey]; ok {
			continue
		}
		missing = append(missing, diagnosticMissingSourceRevision{
			kind:      views.SourceMessage,
			sourceKey: sourceKey,
			reason:    "missing_source_revision",
		})
	}
	return out, invalid, missing
}

func diagnosticMessageIdentityFromSourceKey(sourceKey string) (diagnosticMessageRevision, bool) {
	var key struct {
		Schema  string           `json:"schema"`
		Kind    views.SourceKind `json:"kind"`
		Message *struct {
			ConversationID string `json:"conversation_id"`
			MessageID      string `json:"message_id"`
		} `json:"message,omitempty"`
	}
	if err := json.Unmarshal([]byte(sourceKey), &key); err != nil {
		return diagnosticMessageRevision{}, false
	}
	if key.Schema != "views.source_ref.v1" || key.Kind != views.SourceMessage || key.Message == nil {
		return diagnosticMessageRevision{}, false
	}
	if key.Message.ConversationID == "" || key.Message.MessageID == "" {
		return diagnosticMessageRevision{}, false
	}
	return diagnosticMessageRevision{
		conversationID: key.Message.ConversationID,
		messageID:      key.Message.MessageID,
		sourceKey:      sourceKey,
	}, true
}

func diagnosticDocumentIdentityFromSourceKey(sourceKey string) (diagnosticDocumentRevision, bool) {
	var key struct {
		Schema   string           `json:"schema"`
		Kind     views.SourceKind `json:"kind"`
		Document *struct {
			DatasetID  string `json:"dataset_id"`
			DocumentID string `json:"document_id"`
		} `json:"document,omitempty"`
	}
	if err := json.Unmarshal([]byte(sourceKey), &key); err != nil {
		return diagnosticDocumentRevision{}, false
	}
	if key.Schema != "views.source_ref.v1" || key.Kind != views.SourceDocument || key.Document == nil {
		return diagnosticDocumentRevision{}, false
	}
	if key.Document.DatasetID == "" || key.Document.DocumentID == "" {
		return diagnosticDocumentRevision{}, false
	}
	return diagnosticDocumentRevision{
		datasetID:  key.Document.DatasetID,
		documentID: key.Document.DocumentID,
		sourceKey:  sourceKey,
	}, true
}

type projectionConsistencyStats struct {
	capability        Capability
	namespace         string
	recordsScanned    int
	recordsHydrated   int
	missingMetadata   int
	hydrateMisses     int
	hydrateErrors     int
	nextPageToken     string
	total             int64
	firstErrorDocID   string
	firstError        string
	affectedDocuments []any
}

func newProjectionConsistencyStats(capability Capability, namespace string) projectionConsistencyStats {
	return projectionConsistencyStats{capability: capability, namespace: namespace}
}

func (s *projectionConsistencyStats) noteHydrateError(doc retrieval.Doc, err error) {
	if s == nil || err == nil {
		return
	}
	docID := doc.ID
	if strings.Contains(err.Error(), "metadata") {
		s.missingMetadata++
	} else if errdefs.IsNotAvailable(err) {
		s.hydrateMisses++
	} else {
		s.hydrateErrors++
	}
	if s.firstError == "" {
		s.firstErrorDocID = docID
		s.firstError = err.Error()
	}
	if s.capability == CapabilityDocumentChunks {
		if record, ok := diagnosticDocumentTargetDetailsFromMetadata(doc.Metadata); ok {
			record["projection_id"] = docID
			record["reason"] = diagnosticHydrationFailureReason(err)
			s.affectedDocuments = append(s.affectedDocuments, record)
		}
	}
}

func (s projectionConsistencyStats) check(scope Scope) DiagnosticCheck {
	status := DiagnosticStatusOK
	severity := DiagnosticSeverityInfo
	ok := true
	message := "projection records hydrated from canonical stores"
	if s.missingMetadata > 0 || s.hydrateMisses > 0 || s.hydrateErrors > 0 {
		status = DiagnosticStatusError
		severity = DiagnosticSeverityError
		ok = false
		message = "projection records failed canonical hydration"
	}
	details := map[string]any{
		"namespace":        s.namespace,
		"records_scanned":  s.recordsScanned,
		"records_hydrated": s.recordsHydrated,
		"missing_metadata": s.missingMetadata,
		"hydrate_misses":   s.hydrateMisses,
		"hydrate_errors":   s.hydrateErrors,
		"next_page_token":  s.nextPageToken,
		"total":            s.total,
	}
	if s.firstError != "" {
		details["first_error_doc_id"] = s.firstErrorDocID
		details["first_error"] = s.firstError
	}
	if len(s.affectedDocuments) > 0 {
		details["affected_documents"] = append([]any(nil), s.affectedDocuments...)
	}
	check := newDiagnosticCheck("projection."+string(s.capability)+".hydration", s.capability, scope, diagnosticTargetFromAffectedDocuments(s.capability, s.affectedDocuments), status, severity, ok, message, details)
	if !ok && s.capability == CapabilityDocumentChunks && len(s.affectedDocuments) > 0 {
		check.RepairHint = "rebuild document_chunks for affected documents"
	}
	return check
}

func hydrateDiagnosticProjectionDoc(ctx context.Context, req DiagnosticProbeRequest, capability Capability, doc retrieval.Doc) error {
	switch capability {
	case CapabilityMessageIndex:
		if req.Deps.MessageStore == nil {
			return errdefs.NotAvailablef("memory: message store is not configured")
		}
		conversationID, err := diagnosticMetadataString(doc, projectors.MetadataConversationIDKey)
		if err != nil {
			return err
		}
		messageID, err := diagnosticMetadataString(doc, projectors.MetadataMessageIDKey)
		if err != nil {
			return err
		}
		if _, ok, err := req.Deps.MessageStore.Get(ctx, conversationID, messageID); err != nil {
			return err
		} else if !ok {
			return errdefs.NotAvailablef("memory: hydrate message projection %q: message %q/%q not found", doc.ID, conversationID, messageID)
		}
		return nil
	case CapabilityDocumentChunks:
		if req.Deps.ChunkStore == nil {
			return errdefs.NotAvailablef("memory: chunk store is not configured")
		}
		datasetID, err := diagnosticMetadataString(doc, projectors.MetadataDatasetIDKey)
		if err != nil {
			return err
		}
		scope, err := diagnosticMetadataScope(doc)
		if err != nil {
			return err
		}
		scope.DatasetID = datasetID
		documentID, err := diagnosticMetadataString(doc, projectors.MetadataDocumentIDKey)
		if err != nil {
			return err
		}
		chunkID, err := diagnosticMetadataString(doc, projectors.MetadataChunkIDKey)
		if err != nil {
			return err
		}
		if _, ok, err := req.Deps.ChunkStore.GetChunk(ctx, scope, documentID, viewdocument.ChunkID(chunkID)); err != nil {
			return err
		} else if !ok {
			return errdefs.NotAvailablef("memory: hydrate document chunk projection %q: chunk %q/%q/%q not found", doc.ID, datasetID, documentID, chunkID)
		}
		return nil
	case CapabilitySummaryDAG:
		if req.Deps.SummaryStore == nil {
			return errdefs.NotAvailablef("memory: summary store is not configured")
		}
		scope, err := diagnosticMetadataScope(doc)
		if err != nil {
			return err
		}
		nodeID, err := diagnosticMetadataString(doc, projectors.MetadataNodeIDKey)
		if err != nil {
			return err
		}
		if _, ok, err := req.Deps.SummaryStore.GetNode(ctx, scope, recent.NodeID(nodeID)); err != nil {
			return err
		} else if !ok {
			return errdefs.NotAvailablef("memory: hydrate summary projection %q: node %q/%q/%q/%q not found", doc.ID, scope.RuntimeID, scope.UserID, scope.ConversationID, nodeID)
		}
		return nil
	default:
		return nil
	}
}

func diagnosticMetadataString(doc retrieval.Doc, key string) (string, error) {
	value, ok := doc.Metadata[key]
	if !ok {
		return "", errdefs.NotAvailablef("memory: hydrate projection %q: metadata %q is missing", doc.ID, key)
	}
	out, ok := value.(string)
	if !ok || strings.TrimSpace(out) == "" {
		return "", errdefs.NotAvailablef("memory: hydrate projection %q: metadata %q has invalid value %v", doc.ID, key, value)
	}
	return out, nil
}

func diagnosticMetadataOptionalString(doc retrieval.Doc, key string) string {
	value, ok := doc.Metadata[key]
	if !ok {
		return ""
	}
	out, _ := value.(string)
	return out
}

func diagnosticMetadataScope(doc retrieval.Doc) (Scope, error) {
	runtimeID, err := diagnosticMetadataString(doc, projectors.MetadataRuntimeIDKey)
	if err != nil {
		return Scope{}, err
	}
	return Scope{
		RuntimeID:      runtimeID,
		UserID:         diagnosticMetadataOptionalString(doc, projectors.MetadataUserIDKey),
		AgentID:        diagnosticMetadataOptionalString(doc, projectors.MetadataAgentIDKey),
		ConversationID: diagnosticMetadataOptionalString(doc, projectors.MetadataConversationIDKey),
		DatasetID:      diagnosticMetadataOptionalString(doc, projectors.MetadataDatasetIDKey),
	}, nil
}

func consistencyIncludesProjection(kinds []ConsistencyCheckKind) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, kind := range kinds {
		if kind == ConsistencyCheckProjection {
			return true
		}
	}
	return false
}

func consistencyIncludesSourceView(kinds []ConsistencyCheckKind) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, kind := range kinds {
		if kind == ConsistencyCheckSourceView {
			return true
		}
	}
	return false
}

func diagnosticProjectionScanPageToken(req DiagnosticProbeRequest, capability Capability, namespace string) (string, bool, error) {
	token := strings.TrimSpace(req.PageToken)
	if token == "" {
		return "", false, nil
	}
	composite, ok, err := decodeDiagnosticProjectionPageToken(token)
	if err != nil {
		return "", false, err
	}
	if ok {
		for _, scan := range composite.Scans {
			if scan.Capability == capability && scan.Namespace == namespace {
				return scan.Token, false, nil
			}
		}
		return "", true, nil
	}
	if diagnosticProjectionScanCount(req) == 1 {
		return token, false, nil
	}
	return "", false, errdefs.Validationf("memory: diagnostics projection page token must be composite when scanning multiple projections")
}

func diagnosticProjectionScanCount(req DiagnosticProbeRequest) int {
	if req.System == nil || req.Scope.IsZero() {
		return 0
	}
	seen := map[string]struct{}{}
	for _, capability := range diagnosticCapabilitiesOrDeclared(req) {
		if !readProjectionConfigured(req.System.assembly, req.Deps, capability) {
			continue
		}
		namespace, ok := req.System.assembly.ProjectionNamespace(capability)
		if !ok {
			continue
		}
		key := diagnosticProjectionScanKey(capability, namespace)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
	}
	return len(seen)
}

func setDiagnosticProjectionNextPageToken(result *DiagnosticProbeResult, capability Capability, namespace, token string) {
	if result == nil || strings.TrimSpace(token) == "" {
		return
	}
	composite, ok, err := decodeDiagnosticProjectionPageToken(result.NextPageToken)
	if err != nil || !ok {
		composite = diagnosticProjectionPageToken{Version: 1}
	}
	upsertDiagnosticProjectionScanToken(&composite, diagnosticProjectionScanToken{
		Capability: capability,
		Namespace:  namespace,
		Token:      token,
	})
	encoded, err := encodeDiagnosticProjectionPageToken(composite)
	if err == nil {
		result.NextPageToken = encoded
	}
}

func upsertDiagnosticProjectionScanToken(page *diagnosticProjectionPageToken, scan diagnosticProjectionScanToken) {
	if page == nil {
		return
	}
	if page.Version == 0 {
		page.Version = 1
	}
	for i, existing := range page.Scans {
		if existing.Capability == scan.Capability && existing.Namespace == scan.Namespace {
			page.Scans[i] = scan
			return
		}
	}
	page.Scans = append(page.Scans, scan)
}

func encodeDiagnosticProjectionPageToken(page diagnosticProjectionPageToken) (string, error) {
	scans := make([]diagnosticProjectionScanToken, 0, len(page.Scans))
	for _, scan := range page.Scans {
		scan.Namespace = strings.TrimSpace(scan.Namespace)
		scan.Token = strings.TrimSpace(scan.Token)
		if scan.Capability == "" || scan.Namespace == "" || scan.Token == "" {
			continue
		}
		scans = append(scans, scan)
	}
	if len(scans) == 0 {
		return "", nil
	}
	sort.Slice(scans, func(i, j int) bool {
		return diagnosticProjectionScanKey(scans[i].Capability, scans[i].Namespace) < diagnosticProjectionScanKey(scans[j].Capability, scans[j].Namespace)
	})
	raw, err := json.Marshal(diagnosticProjectionPageToken{Version: 1, Scans: scans})
	if err != nil {
		return "", err
	}
	return diagnosticProjectionPageTokenPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeDiagnosticProjectionPageToken(token string) (diagnosticProjectionPageToken, bool, error) {
	token = strings.TrimSpace(token)
	if token == "" || !strings.HasPrefix(token, diagnosticProjectionPageTokenPrefix) {
		return diagnosticProjectionPageToken{}, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, diagnosticProjectionPageTokenPrefix))
	if err != nil {
		return diagnosticProjectionPageToken{}, true, errdefs.Validationf("memory: invalid diagnostics projection page token: %w", err)
	}
	var page diagnosticProjectionPageToken
	if err := json.Unmarshal(raw, &page); err != nil {
		return diagnosticProjectionPageToken{}, true, errdefs.Validationf("memory: invalid diagnostics projection page token: %w", err)
	}
	if page.Version != 1 {
		return diagnosticProjectionPageToken{}, true, errdefs.Validationf("memory: unsupported diagnostics projection page token version %d", page.Version)
	}
	seen := map[string]struct{}{}
	scans := make([]diagnosticProjectionScanToken, 0, len(page.Scans))
	for _, scan := range page.Scans {
		scan.Namespace = strings.TrimSpace(scan.Namespace)
		scan.Token = strings.TrimSpace(scan.Token)
		if scan.Capability == "" || scan.Namespace == "" || scan.Token == "" {
			return diagnosticProjectionPageToken{}, true, errdefs.Validationf("memory: invalid diagnostics projection page token scan")
		}
		key := diagnosticProjectionScanKey(scan.Capability, scan.Namespace)
		if _, exists := seen[key]; exists {
			return diagnosticProjectionPageToken{}, true, errdefs.Validationf("memory: duplicate diagnostics projection page token scan %q", key)
		}
		seen[key] = struct{}{}
		scans = append(scans, scan)
	}
	page.Scans = scans
	return page, true, nil
}

func diagnosticProjectionScanKey(capability Capability, namespace string) string {
	return string(capability) + "\x00" + namespace
}

func addDependencyCheck(result *DiagnosticProbeResult, name string, capability Capability, scope Scope, dependency string, ready bool) {
	status := DiagnosticStatusOK
	severity := DiagnosticSeverityInfo
	message := dependencyMessage(dependency, ready)
	if !ready {
		status = DiagnosticStatusError
		severity = DiagnosticSeverityError
	}
	result.Checks = append(result.Checks, newDiagnosticCheck(name, capability, scope, LifecycleTarget{}, status, severity, ready, message, nil))
}

func addDependencyWarning(result *DiagnosticProbeResult, name string, capability Capability, scope Scope, dependency string) {
	message := dependency + " missing; writes for this capability will return NotAvailable"
	result.Checks = append(result.Checks, newDiagnosticCheck(name, capability, scope, LifecycleTarget{}, DiagnosticStatusWarning, DiagnosticSeverityWarning, false, message, nil))
}

func diagnosticCapabilitiesOrDeclared(req DiagnosticProbeRequest) []Capability {
	if len(req.Capabilities) > 0 {
		return cloneCapabilities(req.Capabilities)
	}
	return cloneCapabilities(req.DeclaredCapabilities)
}

func normalizeDiagnosticCapabilities(in []Capability) []Capability {
	return dedupeCapabilities(in)
}

func normalizeDiagnosticPageSize(in int) int {
	if in <= 0 {
		return defaultDiagnosticPageSize
	}
	if in > maxDiagnosticPageSize {
		return maxDiagnosticPageSize
	}
	return in
}

func normalizeConsistencyCheckKinds(in []ConsistencyCheckKind) []ConsistencyCheckKind {
	if len(in) == 0 {
		return nil
	}
	out := make([]ConsistencyCheckKind, 0, len(in))
	seen := map[ConsistencyCheckKind]bool{}
	for _, kind := range in {
		if kind == "" || seen[kind] {
			continue
		}
		seen[kind] = true
		out = append(out, kind)
	}
	return out
}

func cloneConsistencyCheckKinds(in []ConsistencyCheckKind) []ConsistencyCheckKind {
	if in == nil {
		return nil
	}
	out := make([]ConsistencyCheckKind, len(in))
	copy(out, in)
	return out
}

func supportedConsistencyCheckKind(kind ConsistencyCheckKind) bool {
	switch kind {
	case ConsistencyCheckProjection, ConsistencyCheckSourceView:
		return true
	default:
		return false
	}
}

func plannedStageNamed(stages []PlannedStage, name string) bool {
	for _, stage := range stages {
		if stage.Name == name {
			return true
		}
	}
	return false
}

func plannedStageDetails(stages []PlannedStage) []map[string]any {
	out := make([]map[string]any, 0, len(stages))
	for _, stage := range stages {
		details := map[string]any{
			"name":       stage.Name,
			"async":      stage.Async,
			"optional":   stage.Optional,
			"capability": string(stage.Capability),
		}
		if len(stage.Config) > 0 {
			details["config"] = cloneDiagnosticDetails(stage.Config)
		}
		out = append(out, details)
	}
	return out
}

func queueStatsDetails(stats QueueStats) map[string]any {
	return map[string]any{
		"pending":   stats.Pending,
		"running":   stats.Running,
		"completed": stats.Completed,
		"failed":    stats.Failed,
		"cancelled": stats.Cancelled,
		"attempts":  stats.Attempts,
	}
}

func newDiagnosticCheck(name string, capability Capability, scope Scope, target LifecycleTarget, status DiagnosticStatus, severity DiagnosticSeverity, ok bool, message string, details map[string]any) DiagnosticCheck {
	return DiagnosticCheck{
		Name:       name,
		Capability: capability,
		Scope:      scope,
		Target:     target,
		Status:     status,
		Severity:   severity,
		OK:         ok,
		Message:    message,
		Details:    cloneDiagnosticDetails(details),
	}
}

func (r *DiagnosticReport) addCheck(name string, capability Capability, status DiagnosticStatus, severity DiagnosticSeverity, ok bool, message string, details map[string]any) {
	if r == nil {
		return
	}
	r.Checks = append(r.Checks, newDiagnosticCheck(name, capability, r.Scope, LifecycleTarget{}, status, severity, ok, message, details))
}

func finalizeDiagnosticReport(report *DiagnosticReport, message string) {
	if report == nil {
		return
	}
	report.Ready = true
	report.OK = true
	for _, check := range report.Checks {
		if !check.OK {
			report.OK = false
			if check.Severity == DiagnosticSeverityError {
				report.Ready = false
			}
		}
	}
	if strings.TrimSpace(message) != "" {
		report.Message = message
		return
	}
	if report.OK {
		report.Message = "diagnostics completed"
	} else {
		report.Message = "diagnostics reported issues"
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
		out[key] = cloneDiagnosticValue(value)
	}
	return out
}

func cloneDiagnosticValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneDiagnosticDetails(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneDiagnosticValue(item)
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	case []Capability:
		return cloneCapabilities(typed)
	case []DocumentTarget:
		return cloneDocumentTargets(typed)
	default:
		return value
	}
}

func mergeCapabilities(left, right []Capability) []Capability {
	return dedupeCapabilities(append(cloneCapabilities(left), right...))
}

func dedupeCapabilities(in []Capability) []Capability {
	if len(in) == 0 {
		return nil
	}
	out := make([]Capability, 0, len(in))
	seen := map[Capability]bool{}
	for _, capability := range in {
		if capability == "" || seen[capability] {
			continue
		}
		seen[capability] = true
		out = append(out, capability)
	}
	return out
}

func mergeDocumentTargets(left, right []DocumentTarget) []DocumentTarget {
	if len(right) == 0 {
		return cloneDocumentTargets(left)
	}
	out := cloneDocumentTargets(left)
	seen := map[DocumentTarget]bool{}
	for _, target := range out {
		seen[target] = true
	}
	for _, target := range right {
		if seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	return out
}
