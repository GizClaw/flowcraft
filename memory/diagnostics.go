package memory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/memory/internal/projectors"
	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	viewentity "github.com/GizClaw/flowcraft/memory/views/entity"
	viewfact "github.com/GizClaw/flowcraft/memory/views/fact"
	viewrecent "github.com/GizClaw/flowcraft/memory/views/recent"
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

const consistencySemanticProjectionPageTokenPrefix = "semantic:"

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
	}
	defer func() {
		if storeErr := r.putDiagnosticReport(ctx, report); storeErr != nil {
			err = errors.Join(err, storeErr)
		}
	}()
	pageSize := normalizeDiagnosticPageSize(req.PageSize)
	consistency := normalizeConsistencyCheckKinds(req.Consistency)
	if r == nil || r.inner == nil {
		report.addCheck("system.configured", "", DiagnosticStatusError, DiagnosticSeverityError, false, "system is not configured", nil)
		finalizeDiagnosticReport(&report, "system is not configured")
		return report, errdefs.NotAvailablef("memory: system is not configured")
	}
	if err := report.Scope.Validate(); err != nil {
		report.addCheck("scope.valid", "", DiagnosticStatusError, DiagnosticSeverityError, false, "scope is invalid", map[string]any{
			"error": err.Error(),
		})
		finalizeDiagnosticReport(&report, "invalid scope")
		return report, errdefs.Validationf("memory: invalid scope: %w", err)
	}
	documents, err := normalizeDocumentTargets(report.Scope, req.Documents)
	if err != nil {
		report.addCheck("diagnostics.document_targets", CapabilityDocumentChunks, DiagnosticStatusError, DiagnosticSeverityError, false, "document targets are invalid", map[string]any{
			"error": err.Error(),
		})
		finalizeDiagnosticReport(&report, "invalid document targets")
		return report, err
	}
	report.Documents = documents
	if requireDeclaredStage && !r.diagnosticStageDeclared(stage) {
		report.addCheck("diagnostics.stage."+stage, "", DiagnosticStatusError, DiagnosticSeverityError, false, "diagnostics stage is not declared by the plan", nil)
		finalizeDiagnosticReport(&report, fmt.Sprintf("diagnostics stage %q is not declared by the plan", stage))
		return report, errdefs.NotAvailablef("memory: diagnostics stage %q is not declared by the plan", stage)
	}

	report.addCheck("system.configured", "", DiagnosticStatusOK, DiagnosticSeverityInfo, true, "system is configured", nil)
	probes := r.diagnosticProbes(stage)
	if len(probes) == 0 {
		report.addCheck("diagnostics.stage."+stage, "", DiagnosticStatusNotImplemented, DiagnosticSeverityError, false, "diagnostics stage is declared but has no registered probe", map[string]any{
			"stage": stage,
		})
		finalizeDiagnosticReport(&report, fmt.Sprintf("diagnostics stage %q has no registered probe", stage))
		return report, errdefs.NotAvailablef("memory: diagnostics stage %q has no registered probe", stage)
	}

	probeReq := r.newDiagnosticProbeRequest(report.TraceID, stage, report.Scope, report.Capabilities, report.Documents, pageSize, strings.TrimSpace(req.PageToken), consistency)
	for _, registered := range probes {
		result, err := registered.probe.Run(ctx, probeReq)
		if len(result.Capabilities) > 0 {
			report.Capabilities = cloneCapabilities(result.Capabilities)
			probeReq.Capabilities = cloneCapabilities(result.Capabilities)
		}
		if result.Documents != nil {
			report.Documents = cloneDocumentTargets(result.Documents)
			probeReq.Documents = cloneDocumentTargets(result.Documents)
		}
		for _, check := range result.Checks {
			report.addProbeCheck(check)
		}
		if result.Message != "" {
			report.Message = result.Message
		}
		if result.NextPageToken != "" {
			report.NextPageToken = result.NextPageToken
			probeReq.PageToken = result.NextPageToken
		}
		if err != nil {
			report.addCheck("diagnostics.probe."+registered.name, "", DiagnosticStatusError, DiagnosticSeverityError, false, err.Error(), map[string]any{
				"stage": stage,
				"probe": registered.name,
			})
			finalizeDiagnosticReport(&report, "diagnostics probe failed")
			return report, err
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
	registry.Register(diagnosticStageConsistency, "consistency", consistencyProbe{})
	registry.Register(lifecycleStageQueueStats, "queue_stats", queueStatsProbe{})
	return registry
}

func (r *System) diagnosticProbes(stage string) []registeredDiagnosticProbe {
	if r == nil || r.diagnosticRegistry == nil {
		return nil
	}
	return r.diagnosticRegistry.stageProbes(stage)
}

func (r *System) newDiagnosticProbeRequest(traceID TraceID, stage string, scope Scope, capabilities []Capability, documents []DocumentTarget, pageSize int, pageToken string, consistency []ConsistencyCheckKind) DiagnosticProbeRequest {
	declaredCapabilities := []Capability(nil)
	if r != nil {
		declaredCapabilities = r.assembly.Capabilities()
	}
	return DiagnosticProbeRequest{
		TraceID:              ensureTraceID(traceID),
		System:               r,
		Deps:                 r.deps,
		Plan:                 r.Plan(),
		Stage:                stage,
		Scope:                scope,
		Capabilities:         cloneCapabilities(capabilities),
		DeclaredCapabilities: cloneCapabilities(declaredCapabilities),
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

	capabilities := result.Capabilities
	if len(capabilities) == 0 {
		capabilities = cloneCapabilities(req.DeclaredCapabilities)
		result.Capabilities = cloneCapabilities(capabilities)
	}
	for _, capability := range capabilities {
		addCapabilityDiagnostics(&result, req, capability)
		addProjectionDiagnostics(&result, req, capability)
		addFreshnessCapabilityChecks(ctx, &result, req, capability)
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
		result.Checks = append(result.Checks, newDiagnosticCheck(check.Name, "", req.Scope, LifecycleTarget{}, status, severity, check.Ready, check.Message, nil))
	}
	return result, nil
}

type traceProbe struct{}

func (traceProbe) Run(ctx context.Context, req DiagnosticProbeRequest) (DiagnosticProbeResult, error) {
	result := DiagnosticProbeResult{Capabilities: cloneCapabilities(req.Capabilities), Documents: cloneDocumentTargets(req.Documents)}
	if len(result.Capabilities) == 0 {
		result.Capabilities = cloneCapabilities(req.DeclaredCapabilities)
	}
	queueDetails, queueErr := traceQueueStatsDetails(ctx, req)
	queueCheck := newDiagnosticCheck("trace.queue_stats", "", req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "queue stats snapshot captured for trace diagnostics", queueDetails)
	if queueErr != nil {
		queueCheck = newDiagnosticCheck("trace.queue_stats", "", req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "queue stats lookup failed", queueDetails)
	}
	result.Checks = append(result.Checks,
		newDiagnosticCheck("diagnostics.stage.trace", "", req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "trace diagnostics stage is available", nil),
		newDiagnosticCheck("trace.plan", "", req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "compiled plan stages selected for this request", map[string]any{
			"write_stages":          plannedStageDetails(req.Plan.Write),
			"read_stages":           plannedStageDetails(req.Plan.Read),
			"lifecycle_stages":      plannedStageDetails(req.Plan.Lifecycle),
			"diagnostic_stages":     plannedStageDetails(req.Plan.Diagnostics),
			"request_stage":         req.Stage,
			"document_targets":      documentTargetTraceDetails(req.Documents),
			"selected_capabilities": capabilityStrings(result.Capabilities),
			"declared_capabilities": capabilityStrings(req.DeclaredCapabilities),
			"runtime_tracing":       "deferred",
			"persistence_status":    "deferred_to_phase6",
		}),
		newDiagnosticCheck("trace.report_store", "", req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "trace report store lookup completed", traceReportStoreDetails(ctx, req)),
		queueCheck,
		newDiagnosticCheck("trace.scope", "", req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "diagnostics scope and hard partition inputs are normalized", scopeTraceDetails(req.Scope)),
		newDiagnosticCheck("trace.projections", "", req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "projection namespaces and retrieval filters were derived without executing runtime tracing", projectionTraceDetails(req, result.Capabilities)),
	)
	if queueErr != nil {
		return result, queueErr
	}
	return result, nil
}

func traceReportStoreDetails(ctx context.Context, req DiagnosticProbeRequest) map[string]any {
	details := map[string]any{
		"trace_id":                string(req.TraceID),
		"report_store_configured": req.System != nil && req.System.reportStore != nil,
		"lifecycle_report_found":  false,
		"diagnostic_report_found": false,
	}
	if req.System == nil || req.System.reportStore == nil {
		return details
	}
	lifecycle, found, err := req.System.reportStore.GetLifecycleReport(ctx, req.TraceID)
	if err != nil {
		details["lifecycle_report_error"] = err.Error()
	} else if found {
		details["lifecycle_report_found"] = true
		details["lifecycle_report"] = lifecycleReportTraceSummary(lifecycle)
	}
	diagnostic, found, err := req.System.reportStore.GetDiagnosticReport(ctx, req.TraceID)
	if err != nil {
		details["diagnostic_report_error"] = err.Error()
	} else if found {
		details["diagnostic_report_found"] = true
		details["diagnostic_report"] = diagnosticReportTraceSummary(diagnostic)
	}
	return details
}

func lifecycleReportTraceSummary(report LifecycleExecutionReport) map[string]any {
	return map[string]any{
		"status":      string(report.Status),
		"message":     report.Message,
		"summary":     report.Summary,
		"job_id":      string(report.JobID),
		"run_id":      string(report.RunID),
		"action":      string(report.Operation.Action),
		"accepted":    report.Accepted,
		"supported":   report.Supported,
		"check_count": lifecycleReportCheckCount(report),
		"step_count":  len(report.Steps),
	}
}

func lifecycleReportCheckCount(report LifecycleExecutionReport) int {
	if report.Checkpoint == nil {
		return len(report.Steps)
	}
	switch v := report.Checkpoint["check_count"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return len(report.Steps)
	}
}

func diagnosticReportTraceSummary(report DiagnosticReport) map[string]any {
	status := string(DiagnosticStatusError)
	if report.OK {
		status = string(DiagnosticStatusOK)
	}
	return map[string]any{
		"status":          status,
		"stage":           report.Stage,
		"message":         report.Message,
		"ready":           report.Ready,
		"ok":              report.OK,
		"check_count":     len(report.Checks),
		"warning_count":   len(report.Warnings),
		"next_page_token": report.NextPageToken,
	}
}

func traceQueueStatsDetails(ctx context.Context, req DiagnosticProbeRequest) (map[string]any, error) {
	details := map[string]any{
		"job_store_configured":   req.System != nil && req.System.jobStore != nil,
		"queue_stats_configured": req.System != nil && req.System.jobStore != nil,
	}
	if req.System == nil {
		err := errdefs.NotAvailablef("memory: system is not configured")
		details["error"] = err.Error()
		return details, err
	}
	stats, err := req.System.QueueStats(ctx)
	for key, value := range queueStatsDetails(stats) {
		details[key] = value
	}
	if err != nil {
		details["error"] = err.Error()
		return details, err
	}
	return details, nil
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
		result.Checks = append(result.Checks, newDiagnosticCheck("diagnostics.stage.queue_stats", "", req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "queue stats lookup failed", map[string]any{
			"error": err.Error(),
		}))
		return result, nil
	}
	result.Checks = append(result.Checks, newDiagnosticCheck("diagnostics.stage.queue_stats", "", req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "queue stats diagnostics stage is available", queueStatsDetails(stats)))
	result.Message = "queue stats diagnostics completed"
	return result, nil
}

func queueStatsDetails(stats QueueStats) map[string]any {
	return map[string]any{
		"pending":           stats.Pending,
		"running":           stats.Running,
		"completed":         stats.Completed,
		"failed":            stats.Failed,
		"cancelled":         stats.Cancelled,
		"attempts":          stats.Attempts,
		"queued_by_kind":    lifecycleJobKindCounts(stats.QueuedByKind),
		"attempts_by_kind":  lifecycleJobKindCounts(stats.AttemptsByKind),
		"completed_by_kind": lifecycleJobKindCounts(stats.CompletedByKind),
		"failed_by_kind":    lifecycleJobKindCounts(stats.FailedByKind),
		"cancelled_by_kind": lifecycleJobKindCounts(stats.CancelledByKind),
	}
}

func lifecycleJobKindCounts(in map[LifecycleJobKind]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for kind, count := range in {
		out[string(kind)] = count
	}
	return out
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
			map[string]any{"page_size": req.PageSize},
		)},
	}
	if len(result.Capabilities) == 0 {
		addCheckWithRepair(&result, "consistency.capabilities", "", req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "consistency scans require explicit capabilities", map[string]any{
			"declared_capabilities": capabilityStrings(req.DeclaredCapabilities),
			"requires_capabilities": true,
		}, "rerun diagnostics with explicit capabilities")
		return result, nil
	}

	kinds := req.Consistency
	if len(kinds) == 0 {
		kinds = []ConsistencyCheckKind{ConsistencyCheckProjection, ConsistencyCheckSourceView}
	}
	for _, kind := range kinds {
		if !supportedConsistencyCheckKind(kind) {
			addCheckWithRepair(&result, "consistency.kind", "", req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "unsupported consistency check kind", map[string]any{
				"kind": string(kind),
			}, "rerun diagnostics with consistency=projection or consistency=source_view")
			return result, nil
		}
	}

	items := consistencyWorkItems(kinds, result.Capabilities)
	pageState, err := decodeConsistencyPageState(req.PageToken)
	if err != nil {
		addCheckWithRepair(&result, "consistency.page_token", "", req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "consistency page token is invalid", map[string]any{
			"error": err.Error(),
		}, "rerun diagnostics without page_token")
		return result, nil
	}
	pageState.ensure()

	for _, item := range items {
		key := item.key()
		if pageState.done[key] {
			continue
		}
		next, scanned := scanConsistencyWorkItem(ctx, &result, req, item, pageState.Positions[key])
		if next == "" {
			pageState.done[key] = true
			delete(pageState.Positions, key)
		} else {
			pageState.Positions[key] = next
		}
		if scanned == 0 && next == "" {
			addCheck(&result, fmt.Sprintf("consistency.%s.%s.page", item.kind, item.capability), item.capability, req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "consistency scan page had no records", map[string]any{
				"kind":       string(item.kind),
				"capability": string(item.capability),
				"page_size":  req.PageSize,
			})
		}
	}
	if !pageState.complete(items) {
		token, err := encodeConsistencyPageState(pageState)
		if err != nil {
			addCheckWithRepair(&result, "consistency.page_token.encode", "", req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "consistency next page token could not be encoded", map[string]any{
				"error": err.Error(),
			}, "rerun diagnostics from the first page")
			return result, nil
		}
		result.NextPageToken = token
	}
	result.Message = "consistency diagnostics page completed"
	return result, nil
}

type consistencyWorkItem struct {
	kind       ConsistencyCheckKind
	capability Capability
}

func (i consistencyWorkItem) key() string {
	return string(i.kind) + "\x00" + string(i.capability)
}

type consistencyPageState struct {
	Positions map[string]string `json:"positions,omitempty"`
	Done      []string          `json:"done,omitempty"`

	done map[string]bool
}

func (s *consistencyPageState) ensure() {
	if s.Positions == nil {
		s.Positions = map[string]string{}
	}
	if s.done == nil {
		s.done = map[string]bool{}
		for _, key := range s.Done {
			if key != "" {
				s.done[key] = true
			}
		}
	}
}

func (s *consistencyPageState) complete(items []consistencyWorkItem) bool {
	s.ensure()
	for _, item := range items {
		if !s.done[item.key()] {
			return false
		}
	}
	return true
}

func consistencyWorkItems(kinds []ConsistencyCheckKind, capabilities []Capability) []consistencyWorkItem {
	items := make([]consistencyWorkItem, 0, len(kinds)*len(capabilities))
	for _, kind := range kinds {
		for _, capability := range capabilities {
			items = append(items, consistencyWorkItem{kind: kind, capability: capability})
		}
	}
	return items
}

func scanConsistencyWorkItem(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, item consistencyWorkItem, pageToken string) (string, int) {
	switch item.kind {
	case ConsistencyCheckProjection:
		return scanProjectionConsistency(ctx, result, req, item.capability, pageToken)
	case ConsistencyCheckSourceView:
		return scanSourceViewConsistency(ctx, result, req, item.capability, pageToken)
	default:
		addCheckWithRepair(result, "consistency.kind", item.capability, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "unsupported consistency check kind", map[string]any{
			"kind": string(item.kind),
		}, "rerun diagnostics with a supported consistency kind")
		return "", 0
	}
}

func scanProjectionConsistency(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability, pageToken string) (string, int) {
	if strings.HasPrefix(pageToken, consistencySemanticProjectionPageTokenPrefix) {
		namespace, ok := projectionConsistencyNamespace(result, req, capability, "projection")
		if !ok {
			return "", 0
		}
		next, scanned := scanSemanticProjectionConsistency(ctx, result, req, capability, namespace, strings.TrimPrefix(pageToken, consistencySemanticProjectionPageTokenPrefix))
		return semanticProjectionPageToken(next), scanned
	}
	resp, namespace, ok := listProjectionConsistencyPage(ctx, result, req, capability, pageToken, "projection")
	if !ok || resp == nil {
		return "", 0
	}
	for _, doc := range resp.Items {
		addProjectionRecordConsistencyCheck(ctx, result, req, capability, namespace, doc)
	}
	if resp.NextPageToken != "" {
		return resp.NextPageToken, len(resp.Items)
	}
	next, semanticScanned := scanSemanticProjectionConsistency(ctx, result, req, capability, namespace, "")
	return semanticProjectionPageToken(next), len(resp.Items) + semanticScanned
}

func scanSourceViewConsistency(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability, pageToken string) (string, int) {
	if capability != CapabilityDocumentChunks {
		addCheckWithRepair(result, fmt.Sprintf("consistency.source_view.%s", capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusNotImplemented, DiagnosticSeverityWarning, true, "source-view consistency scan is not implemented for this capability", map[string]any{
			"capability": string(capability),
		}, fmt.Sprintf("rebuild capability=%s target=scope", capability))
		return "", 0
	}
	resp, namespace, ok := listProjectionConsistencyPage(ctx, result, req, capability, pageToken, "source_view")
	if !ok || resp == nil {
		return "", 0
	}
	for _, doc := range resp.Items {
		addDocumentChunkSourceViewConsistencyCheck(ctx, result, req, namespace, doc)
	}
	return resp.NextPageToken, len(resp.Items)
}

func listProjectionConsistencyPage(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability, pageToken, kind string) (*retrieval.ListResponse, string, bool) {
	scopedNamespace, ok := projectionConsistencyNamespace(result, req, capability, kind)
	if !ok {
		return nil, "", false
	}
	resp, err := req.Deps.Index.List(ctx, scopedNamespace, retrieval.ListRequest{
		Filter:     consistencyScopeFilter(capability, req.Scope),
		PageSize:   req.PageSize,
		PageToken:  pageToken,
		OrderBy:    retrieval.OrderByIDAsc,
		Project:    consistencyProjectionMetadataKeys(capability),
		WithVector: false,
	})
	if err != nil {
		addCheckWithRepair(result, fmt.Sprintf("consistency.%s.%s.scan", kind, capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "projection page scan failed", map[string]any{
			"namespace": scopedNamespace,
			"error":     err.Error(),
		}, fmt.Sprintf("retry consistency scan capability=%s", capability))
		return nil, scopedNamespace, false
	}
	return resp, scopedNamespace, true
}

func projectionConsistencyNamespace(result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability, kind string) (string, bool) {
	if req.System == nil || !req.System.assembly.HasCapability(capability) {
		addCheckWithRepair(result, fmt.Sprintf("consistency.%s.%s.declared", kind, capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "capability is not declared by the assembly", nil, fmt.Sprintf("declare capability=%s before scanning consistency", capability))
		return "", false
	}
	baseNamespace, ok := req.System.assembly.ProjectionNamespace(capability)
	if !ok {
		addCheckWithRepair(result, fmt.Sprintf("consistency.%s.%s.binding", kind, capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusMissing, DiagnosticSeverityError, false, "projection namespace is not declared", nil, fmt.Sprintf("declare projection namespace for capability=%s", capability))
		return "", false
	}
	if req.Deps.Index == nil {
		addCheckWithRepair(result, fmt.Sprintf("consistency.%s.%s.index", kind, capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "projection consistency scan requires Index", map[string]any{
			"base_namespace": baseNamespace,
		}, "configure retrieval index before scanning consistency")
		return "", false
	}
	scopedNamespace, err := projectors.ScopedNamespace(baseNamespace, req.Scope)
	if err != nil {
		addCheckWithRepair(result, fmt.Sprintf("consistency.%s.%s.scoped_namespace", kind, capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "scoped projection namespace cannot be computed", map[string]any{
			"base_namespace": baseNamespace,
			"error":          err.Error(),
		}, fmt.Sprintf("fix scope or projection namespace for capability=%s", capability))
		return "", false
	}
	return scopedNamespace, true
}

func scanSemanticProjectionConsistency(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability, namespace, pageToken string) (string, int) {
	switch capability {
	case CapabilitySummaryDAG:
		return scanSummarySemanticProjectionConsistency(ctx, result, req, namespace, pageToken)
	case CapabilityFactLedger:
		return scanFactSemanticProjectionConsistency(ctx, result, req, namespace, pageToken)
	default:
		return "", 0
	}
}

func scanSummarySemanticProjectionConsistency(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, namespace, pageToken string) (string, int) {
	if req.Deps.SummaryStore == nil {
		addCheckWithRepair(result, "consistency.projection.summary_dag.semantic_scan", CapabilitySummaryDAG, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "summary semantic-projection scan requires SummaryStore", nil, "configure SummaryStore before scanning summary_dag")
		return "", 0
	}
	nodes, err := req.Deps.SummaryStore.ListNodes(ctx, req.Scope, viewrecent.ListOptions{
		AfterID: viewrecent.NodeID(pageToken),
		Limit:   req.PageSize,
	})
	if err != nil {
		addCheckWithRepair(result, "consistency.projection.summary_dag.semantic_scan", CapabilitySummaryDAG, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "summary semantic-projection scan failed", map[string]any{
			"error": err.Error(),
		}, "retry consistency scan capability=summary_dag")
		return "", 0
	}
	for _, node := range nodes {
		record, err := projectors.SummaryNode(node)
		if err != nil {
			addSemanticProjectionProjectorError(result, req, CapabilitySummaryDAG, string(node.ID), err)
			continue
		}
		addExpectedProjectionRecordConsistencyCheck(ctx, result, req, CapabilitySummaryDAG, namespace, record)
	}
	if len(nodes) >= req.PageSize && len(nodes) > 0 {
		return string(nodes[len(nodes)-1].ID), len(nodes)
	}
	return "", len(nodes)
}

func scanFactSemanticProjectionConsistency(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, namespace, pageToken string) (string, int) {
	if req.Deps.FactStore == nil {
		addCheckWithRepair(result, "consistency.projection.fact_ledger.semantic_scan", CapabilityFactLedger, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "fact semantic-projection scan requires FactStore", nil, "configure FactStore before scanning fact_ledger")
		return "", 0
	}
	facts, err := req.Deps.FactStore.List(ctx, viewfact.ListOptions{
		AfterID:    viewfact.FactID(pageToken),
		Limit:      req.PageSize,
		Scope:      req.Scope,
		ActiveOnly: true,
	})
	if err != nil {
		addCheckWithRepair(result, "consistency.projection.fact_ledger.semantic_scan", CapabilityFactLedger, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "fact semantic-projection scan failed", map[string]any{
			"error": err.Error(),
		}, "retry consistency scan capability=fact_ledger")
		return "", 0
	}
	for _, fact := range facts {
		record, err := projectors.FactRecord(fact)
		if err != nil {
			addSemanticProjectionProjectorError(result, req, CapabilityFactLedger, string(fact.ID), err)
			continue
		}
		addExpectedProjectionRecordConsistencyCheck(ctx, result, req, CapabilityFactLedger, namespace, record)
	}
	if len(facts) >= req.PageSize && len(facts) > 0 {
		return string(facts[len(facts)-1].ID), len(facts)
	}
	return "", len(facts)
}

func semanticProjectionPageToken(token string) string {
	if token == "" {
		return ""
	}
	return consistencySemanticProjectionPageTokenPrefix + token
}

func addExpectedProjectionRecordConsistencyCheck(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability, namespace string, record indexed.Record) {
	details := expectedProjectionRecordDetails(req, capability, namespace, record)
	resp, err := req.Deps.Index.List(ctx, namespace, retrieval.ListRequest{
		Filter:     expectedProjectionRecordFilter(capability, record),
		PageSize:   req.PageSize,
		OrderBy:    retrieval.OrderByIDAsc,
		Project:    consistencyProjectionMetadataKeys(capability),
		WithVector: false,
	})
	if err != nil {
		details["error"] = err.Error()
		addCheckWithRepair(result, fmt.Sprintf("consistency.projection.%s.semantic_lookup", capability), capability, req.Scope, LifecycleTarget{Capability: capability}, DiagnosticStatusError, DiagnosticSeverityError, false, "projection lookup for semantic record failed", details, fmt.Sprintf("retry consistency scan capability=%s", capability))
		return
	}
	for _, doc := range resp.Items {
		if doc.ID != record.ID {
			continue
		}
		addProjectionRecordConsistencyCheck(ctx, result, req, capability, namespace, doc)
		return
	}
	details["semantic_state"] = "missing_projection_record"
	addCheckWithRepair(result, fmt.Sprintf("consistency.projection.%s.record", capability), capability, req.Scope, LifecycleTarget{Capability: capability}, DiagnosticStatusMissing, DiagnosticSeverityError, false, "semantic record is missing its projection record", details, projectionRepairHint(capability, LifecycleTarget{Capability: capability}))
}

func addSemanticProjectionProjectorError(result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability, semanticID string, err error) {
	addCheckWithRepair(result, fmt.Sprintf("consistency.projection.%s.record", capability), capability, req.Scope, LifecycleTarget{Capability: capability}, DiagnosticStatusError, DiagnosticSeverityError, false, "semantic record could not be projected for consistency diagnostics", map[string]any{
		"semantic_id": semanticID,
		"error":       err.Error(),
	}, projectionRepairHint(capability, LifecycleTarget{Capability: capability}))
}

func expectedProjectionRecordFilter(capability Capability, record indexed.Record) retrieval.Filter {
	eq := map[string]any{
		projectors.MetadataViewKindKey:   metadataString(record.Metadata, projectors.MetadataViewKindKey),
		projectors.MetadataRecordTypeKey: metadataString(record.Metadata, projectors.MetadataRecordTypeKey),
	}
	switch capability {
	case CapabilitySummaryDAG:
		eq[projectors.MetadataNodeIDKey] = metadataString(record.Metadata, projectors.MetadataNodeIDKey)
	case CapabilityFactLedger:
		eq[projectors.MetadataFactIDKey] = metadataString(record.Metadata, projectors.MetadataFactIDKey)
	}
	return retrieval.Filter{Eq: eq}
}

func expectedProjectionRecordDetails(req DiagnosticProbeRequest, capability Capability, namespace string, record indexed.Record) map[string]any {
	return map[string]any{
		"record_id":       record.ID,
		"namespace":       namespace,
		"capability":      string(capability),
		"page_size":       req.PageSize,
		"record_type":     metadataString(record.Metadata, projectors.MetadataRecordTypeKey),
		"view_kind":       metadataString(record.Metadata, projectors.MetadataViewKindKey),
		"node_id":         metadataString(record.Metadata, projectors.MetadataNodeIDKey),
		"fact_id":         metadataString(record.Metadata, projectors.MetadataFactIDKey),
		"requested_scope": scopeTraceDetails(req.Scope),
	}
}

func addProjectionRecordConsistencyCheck(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability, namespace string, doc retrieval.Doc) {
	details := projectionRecordDetails(req, capability, namespace, doc)
	target := projectionRecordLifecycleTarget(capability, doc.Metadata)
	status := DiagnosticStatusOK
	severity := DiagnosticSeverityInfo
	ok := true
	message := "projection record is consistent with semantic store"
	var repairHint string

	var missing []string
	for _, key := range requiredProjectionMetadataKeys(capability, doc.Metadata) {
		if metadataString(doc.Metadata, key) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		details["missing_metadata"] = missing
		status = DiagnosticStatusMissing
		severity = DiagnosticSeverityError
		ok = false
		message = "projection record is missing required identity metadata"
		repairHint = projectionRepairHint(capability, target)
	}

	if scopeReasons := projectionHardPartitionMismatches(req.Scope, doc.Metadata); len(scopeReasons) > 0 {
		details["scope_mismatches"] = scopeReasons
		status = DiagnosticStatusStale
		severity = DiagnosticSeverityError
		ok = false
		message = "projection record hard partition metadata does not match requested scope"
		repairHint = projectionRepairHint(capability, target)
	}

	if _, found, err := indexed.DecodeSourceRefs(doc.Metadata); err != nil {
		details["source_refs_error"] = err.Error()
		status = DiagnosticStatusError
		severity = DiagnosticSeverityError
		ok = false
		message = "projection record source refs could not be decoded"
		repairHint = projectionRepairHint(capability, target)
	} else if capability == CapabilityDocumentChunks && !found {
		details["missing_metadata"] = appendStringDetail(details["missing_metadata"], indexed.MetadataSourceRefsKey)
		status = DiagnosticStatusMissing
		severity = DiagnosticSeverityError
		ok = false
		message = "projection record is missing indexed source refs"
		repairHint = projectionRepairHint(capability, target)
	}
	if _, found, err := indexed.DecodeSignature(doc.Metadata); err != nil {
		details["signature_error"] = err.Error()
		status = DiagnosticStatusError
		severity = DiagnosticSeverityError
		ok = false
		message = "projection record signature could not be decoded"
		repairHint = projectionRepairHint(capability, target)
	} else if capability == CapabilityDocumentChunks && !found {
		details["missing_metadata"] = appendStringDetail(details["missing_metadata"], indexed.MetadataSignatureKey)
		status = DiagnosticStatusMissing
		severity = DiagnosticSeverityError
		ok = false
		message = "projection record is missing indexed signature"
		repairHint = projectionRepairHint(capability, target)
	}

	hydrateStatus, hydrateMessage, hydrateDetails, hydrateRepair := hydrateProjectionRecord(ctx, req, capability, doc.Metadata, target)
	for key, value := range hydrateDetails {
		details[key] = value
	}
	if hydrateStatus != DiagnosticStatusOK && ok {
		status = hydrateStatus
		severity = DiagnosticSeverityError
		ok = false
		message = hydrateMessage
		repairHint = hydrateRepair
	}
	if status != DiagnosticStatusOK && repairHint == "" {
		repairHint = projectionRepairHint(capability, target)
	}
	addCheckWithRepair(result, fmt.Sprintf("consistency.projection.%s.record", capability), capability, req.Scope, target, status, severity, ok, message, details, repairHint)
}

func hydrateProjectionRecord(ctx context.Context, req DiagnosticProbeRequest, capability Capability, metadata map[string]any, target LifecycleTarget) (DiagnosticStatus, string, map[string]any, string) {
	details := map[string]any{}
	switch capability {
	case CapabilityDocumentChunks:
		if req.Deps.ChunkStore == nil {
			return DiagnosticStatusError, "document chunk projection hydrate requires ChunkStore", details, "configure ChunkStore before scanning document_chunks"
		}
		datasetID := metadataString(metadata, projectors.MetadataDatasetIDKey)
		documentID := metadataString(metadata, projectors.MetadataDocumentIDKey)
		chunkID := metadataString(metadata, projectors.MetadataChunkIDKey)
		if datasetID == "" || documentID == "" || chunkID == "" {
			return DiagnosticStatusMissing, "document chunk projection cannot hydrate without dataset/document/chunk identity", details, projectionRepairHint(capability, target)
		}
		scope := projectionScopeFromMetadata(req.Scope, metadata)
		scope.DatasetID = datasetID
		chunk, found, err := req.Deps.ChunkStore.GetChunk(ctx, scope, documentID, viewdocument.ChunkID(chunkID))
		if err != nil {
			details["hydrate_error"] = err.Error()
			return DiagnosticStatusError, "semantic document chunk hydrate failed", details, fmt.Sprintf("reload document_chunks target=%s/%s", datasetID, documentID)
		}
		if !found {
			details["hydrate_state"] = "missing_semantic_record"
			return DiagnosticStatusMissing, "semantic document chunk record is missing", details, fmt.Sprintf("reload document_chunks target=%s/%s", datasetID, documentID)
		}
		details["hydrate_state"] = "found"
		details["chunk_id"] = string(chunk.ID)
		return DiagnosticStatusOK, "", details, ""
	case CapabilitySummaryDAG:
		if req.Deps.SummaryStore == nil {
			return DiagnosticStatusError, "summary DAG projection hydrate requires SummaryStore", details, "configure SummaryStore before scanning summary_dag"
		}
		nodeID := metadataString(metadata, projectors.MetadataNodeIDKey)
		if nodeID == "" {
			return DiagnosticStatusMissing, "summary DAG projection cannot hydrate without node identity", details, projectionRepairHint(capability, target)
		}
		scope := projectionScopeFromMetadata(req.Scope, metadata)
		node, found, err := req.Deps.SummaryStore.GetNode(ctx, scope, viewrecent.NodeID(nodeID))
		if err != nil {
			details["hydrate_error"] = err.Error()
			return DiagnosticStatusError, "semantic summary node hydrate failed", details, projectionRepairHint(capability, target)
		}
		if !found {
			details["hydrate_state"] = "missing_semantic_record"
			return DiagnosticStatusMissing, "semantic summary node record is missing", details, projectionRepairHint(capability, target)
		}
		details["hydrate_state"] = "found"
		details["node_id"] = string(node.ID)
		return hydratedScopeStatus(req.Scope, scope, node.Scope, capability, target, details)
	case CapabilityObservationLedger:
		if req.Deps.ObservationStore == nil {
			return DiagnosticStatusError, "observation ledger projection hydrate requires ObservationStore", details, "configure ObservationStore before scanning observation_ledger"
		}
		observationID := metadataString(metadata, projectors.MetadataObservationIDKey)
		if observationID == "" {
			return DiagnosticStatusMissing, "observation ledger projection cannot hydrate without observation identity", details, projectionRepairHint(capability, target)
		}
		observation, found, err := req.Deps.ObservationStore.Get(ctx, observationID)
		if err != nil {
			details["hydrate_error"] = err.Error()
			return DiagnosticStatusError, "semantic observation hydrate failed", details, projectionRepairHint(capability, target)
		}
		if !found {
			details["hydrate_state"] = "missing_semantic_record"
			return DiagnosticStatusMissing, "semantic observation record is missing", details, projectionRepairHint(capability, target)
		}
		scope := projectionScopeFromMetadata(req.Scope, metadata)
		details["hydrate_state"] = "found"
		details["observation_id"] = observation.ID
		return hydratedScopeStatus(req.Scope, scope, observation.Scope, capability, target, details)
	case CapabilityFactLedger:
		if req.Deps.FactStore == nil {
			return DiagnosticStatusError, "fact ledger projection hydrate requires FactStore", details, "configure FactStore before scanning fact_ledger"
		}
		factID := metadataString(metadata, projectors.MetadataFactIDKey)
		if factID == "" {
			return DiagnosticStatusMissing, "fact ledger projection cannot hydrate without fact identity", details, projectionRepairHint(capability, target)
		}
		record, found, err := req.Deps.FactStore.Get(ctx, viewfact.FactID(factID))
		if err != nil {
			details["hydrate_error"] = err.Error()
			return DiagnosticStatusError, "semantic fact hydrate failed", details, projectionRepairHint(capability, target)
		}
		if !found {
			details["hydrate_state"] = "missing_semantic_record"
			return DiagnosticStatusMissing, "semantic fact record is missing", details, projectionRepairHint(capability, target)
		}
		scope := projectionScopeFromMetadata(req.Scope, metadata)
		details["hydrate_state"] = "found"
		details["fact_id"] = string(record.ID)
		return hydratedScopeStatus(req.Scope, scope, record.Scope, capability, target, details)
	case CapabilityFactGraph:
		if req.Deps.FactGraphStore == nil {
			return DiagnosticStatusError, "fact graph projection hydrate requires FactGraphStore", details, "configure FactGraphStore before scanning fact_graph"
		}
		scope := projectionScopeFromMetadata(req.Scope, metadata)
		switch recordType := metadataString(metadata, projectors.MetadataRecordTypeKey); recordType {
		case projectors.RecordTypeFactEdge:
			edgeID := metadataString(metadata, projectors.MetadataEdgeIDKey)
			if edgeID == "" {
				return DiagnosticStatusMissing, "fact graph edge projection cannot hydrate without edge identity", details, projectionRepairHint(capability, target)
			}
			details["edge_id"] = edgeID
			edge, found, err := req.Deps.FactGraphStore.GetEdge(ctx, viewfact.EdgeID(edgeID))
			if err != nil {
				details["hydrate_error"] = err.Error()
				return DiagnosticStatusError, "semantic fact graph edge hydrate failed", details, projectionRepairHint(capability, target)
			}
			if !found {
				details["hydrate_state"] = "missing_semantic_record"
				return DiagnosticStatusMissing, "semantic fact graph edge record is missing", details, projectionRepairHint(capability, target)
			}
			details["hydrate_state"] = "found"
			details["edge_id"] = string(edge.ID)
			return hydratedScopeStatus(req.Scope, scope, edge.Scope, capability, target, details)
		case projectors.RecordTypeFactNode:
			nodeID := metadataString(metadata, projectors.MetadataNodeIDKey)
			if nodeID == "" {
				return DiagnosticStatusMissing, "fact graph node projection cannot hydrate without node identity", details, projectionRepairHint(capability, target)
			}
			details["node_id"] = nodeID
			node, found, err := req.Deps.FactGraphStore.GetNode(ctx, viewfact.NodeID(nodeID))
			if err != nil {
				details["hydrate_error"] = err.Error()
				return DiagnosticStatusError, "semantic fact graph node hydrate failed", details, projectionRepairHint(capability, target)
			}
			if !found {
				details["hydrate_state"] = "missing_semantic_record"
				return DiagnosticStatusMissing, "semantic fact graph node record is missing", details, projectionRepairHint(capability, target)
			}
			details["hydrate_state"] = "found"
			details["node_id"] = string(node.ID)
			return hydratedScopeStatus(req.Scope, scope, node.Scope, capability, target, details)
		default:
			details["record_type"] = recordType
			return DiagnosticStatusError, "fact graph projection record type is unsupported", details, projectionRepairHint(capability, target)
		}
	case CapabilityEntityProfile:
		if req.Deps.EntityProfileStore == nil {
			return DiagnosticStatusError, "entity profile projection hydrate requires EntityProfileStore", details, "configure EntityProfileStore before scanning entity_profile"
		}
		profileID := metadataString(metadata, projectors.MetadataProfileIDKey)
		if profileID == "" {
			return DiagnosticStatusMissing, "entity profile projection cannot hydrate without profile identity", details, projectionRepairHint(capability, target)
		}
		scope := projectionScopeFromMetadata(req.Scope, metadata)
		profile, found, err := req.Deps.EntityProfileStore.Get(ctx, scope, viewentity.ProfileID(profileID))
		if err != nil {
			details["hydrate_error"] = err.Error()
			return DiagnosticStatusError, "semantic entity profile hydrate failed", details, projectionRepairHint(capability, target)
		}
		if !found {
			details["hydrate_state"] = "missing_semantic_record"
			return DiagnosticStatusMissing, "semantic entity profile record is missing", details, projectionRepairHint(capability, target)
		}
		details["hydrate_state"] = "found"
		details["profile_id"] = string(profile.ID)
		return hydratedScopeStatus(req.Scope, scope, profile.Scope, capability, target, details)
	case CapabilityEntityTimeline:
		if req.Deps.EntityTimelineStore == nil {
			return DiagnosticStatusError, "entity timeline projection hydrate requires EntityTimelineStore", details, "configure EntityTimelineStore before scanning entity_timeline"
		}
		eventID := metadataString(metadata, projectors.MetadataEventIDKey)
		if eventID == "" {
			return DiagnosticStatusMissing, "entity timeline projection cannot hydrate without event identity", details, projectionRepairHint(capability, target)
		}
		scope := projectionScopeFromMetadata(req.Scope, metadata)
		event, found, err := req.Deps.EntityTimelineStore.Get(ctx, scope, viewentity.EventID(eventID))
		if err != nil {
			details["hydrate_error"] = err.Error()
			return DiagnosticStatusError, "semantic entity timeline hydrate failed", details, projectionRepairHint(capability, target)
		}
		if !found {
			details["hydrate_state"] = "missing_semantic_record"
			return DiagnosticStatusMissing, "semantic entity timeline record is missing", details, projectionRepairHint(capability, target)
		}
		details["hydrate_state"] = "found"
		details["event_id"] = string(event.ID)
		return hydratedScopeStatus(req.Scope, scope, event.Scope, capability, target, details)
	default:
		details["hydrate_state"] = "not_implemented"
		return DiagnosticStatusNotImplemented, "semantic hydrate is not implemented for this capability", details, fmt.Sprintf("rebuild capability=%s target=scope", capability)
	}
}

func hydratedScopeStatus(requested, hydrated, record Scope, capability Capability, target LifecycleTarget, details map[string]any) (DiagnosticStatus, string, map[string]any, string) {
	details["hydrate_scope"] = scopeTraceDetails(hydrated)
	details["semantic_scope"] = scopeTraceDetails(record)
	if mismatches := hydratedHardPartitionMismatches(requested, hydrated, record); len(mismatches) > 0 {
		details["hydrate_scope_mismatches"] = mismatches
		details["hydrate_state"] = "stale_scope"
		return DiagnosticStatusStale, "semantic record hard partition does not match projection scope", details, projectionRepairHint(capability, target)
	}
	if mismatches := hydratedSoftScopeMismatches(hydrated, record); len(mismatches) > 0 {
		details["hydrate_soft_scope_mismatches"] = mismatches
	}
	return DiagnosticStatusOK, "", details, ""
}

func hydratedHardPartitionMismatches(requested, hydrated, record Scope) []string {
	var reasons []string
	if record.RuntimeID != requested.RuntimeID {
		reasons = append(reasons, fmt.Sprintf("semantic runtime_id %q does not match requested scope %q", record.RuntimeID, requested.RuntimeID))
	}
	if record.UserID != requested.UserID {
		reasons = append(reasons, fmt.Sprintf("semantic user_id %q does not match requested scope %q", record.UserID, requested.UserID))
	}
	if record.RuntimeID != hydrated.RuntimeID {
		reasons = append(reasons, fmt.Sprintf("semantic runtime_id %q does not match projection scope %q", record.RuntimeID, hydrated.RuntimeID))
	}
	if record.UserID != hydrated.UserID {
		reasons = append(reasons, fmt.Sprintf("semantic user_id %q does not match projection scope %q", record.UserID, hydrated.UserID))
	}
	return reasons
}

func hydratedSoftScopeMismatches(hydrated, record Scope) []string {
	var reasons []string
	if record.AgentID != hydrated.AgentID {
		reasons = append(reasons, fmt.Sprintf("semantic agent_id %q does not match projection scope %q", record.AgentID, hydrated.AgentID))
	}
	if record.ConversationID != hydrated.ConversationID {
		reasons = append(reasons, fmt.Sprintf("semantic conversation_id %q does not match projection scope %q", record.ConversationID, hydrated.ConversationID))
	}
	if record.DatasetID != hydrated.DatasetID {
		reasons = append(reasons, fmt.Sprintf("semantic dataset_id %q does not match projection scope %q", record.DatasetID, hydrated.DatasetID))
	}
	if record.EntityID != hydrated.EntityID {
		reasons = append(reasons, fmt.Sprintf("semantic entity_id %q does not match projection scope %q", record.EntityID, hydrated.EntityID))
	}
	return reasons
}

func addDocumentChunkSourceViewConsistencyCheck(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, namespace string, doc retrieval.Doc) {
	details := projectionRecordDetails(req, CapabilityDocumentChunks, namespace, doc)
	target := projectionRecordLifecycleTarget(CapabilityDocumentChunks, doc.Metadata)
	datasetID := metadataString(doc.Metadata, projectors.MetadataDatasetIDKey)
	documentID := metadataString(doc.Metadata, projectors.MetadataDocumentIDKey)
	chunkID := metadataString(doc.Metadata, projectors.MetadataChunkIDKey)
	repairHint := fmt.Sprintf("reload document_chunks target=%s/%s", datasetID, documentID)
	if datasetID == "" || documentID == "" || chunkID == "" {
		addCheckWithRepair(result, "consistency.source_view.document_chunks.record", CapabilityDocumentChunks, req.Scope, target, DiagnosticStatusMissing, DiagnosticSeverityError, false, "document chunk source-view scan requires projection identity metadata", details, projectionRepairHint(CapabilityDocumentChunks, target))
		return
	}
	if req.Deps.ChunkStore == nil || req.Deps.DocumentStore == nil {
		addCheckWithRepair(result, "consistency.source_view.document_chunks.record", CapabilityDocumentChunks, req.Scope, target, DiagnosticStatusError, DiagnosticSeverityError, false, "document chunk source-view scan requires ChunkStore and DocumentStore", details, "configure ChunkStore and DocumentStore before scanning source_view")
		return
	}
	scope := projectionScopeFromMetadata(req.Scope, doc.Metadata)
	scope.DatasetID = datasetID
	chunk, found, err := req.Deps.ChunkStore.GetChunk(ctx, scope, documentID, viewdocument.ChunkID(chunkID))
	if err != nil {
		details["chunk_error"] = err.Error()
		addCheckWithRepair(result, "consistency.source_view.document_chunks.record", CapabilityDocumentChunks, req.Scope, target, DiagnosticStatusError, DiagnosticSeverityError, false, "document chunk view record lookup failed", details, repairHint)
		return
	}
	if !found {
		details["state"] = "missing_chunk"
		addCheckWithRepair(result, "consistency.source_view.document_chunks.record", CapabilityDocumentChunks, req.Scope, target, DiagnosticStatusMissing, DiagnosticSeverityError, false, "document chunk view record is missing", details, repairHint)
		return
	}
	canonical, found, err := req.Deps.DocumentStore.Get(ctx, datasetID, documentID)
	if err != nil {
		details["document_error"] = err.Error()
		addCheckWithRepair(result, "consistency.source_view.document_chunks.record", CapabilityDocumentChunks, req.Scope, target, DiagnosticStatusError, DiagnosticSeverityError, false, "canonical document lookup failed", details, repairHint)
		return
	}
	if !found {
		details["state"] = "missing_document"
		addCheckWithRepair(result, "consistency.source_view.document_chunks.record", CapabilityDocumentChunks, req.Scope, target, DiagnosticStatusMissing, DiagnosticSeverityError, false, "canonical document is missing for document chunk source view", details, repairHint)
		return
	}
	outcome := compareDocumentChunkSourceView(req.Scope, canonical, chunk)
	for key, value := range outcome.details {
		details[key] = value
	}
	hint := ""
	if !outcome.ok {
		hint = repairHint
	}
	addCheckWithRepair(result, "consistency.source_view.document_chunks.record", CapabilityDocumentChunks, req.Scope, target, outcome.status, outcome.severity, outcome.ok, outcome.message, details, hint)
}

func compareDocumentChunkSourceView(scope Scope, doc sourcedocument.Document, chunk viewdocument.Chunk) documentFreshnessOutcome {
	comparable, staleReasons := compareDocumentChunkFreshness(scope, doc, chunk)
	details := documentTargetDetails(DocumentTarget{DatasetID: doc.DatasetID, DocumentID: doc.ID}, scope)
	details["document_version"] = strconv.FormatUint(doc.Version, 10)
	details["document_content_hash"] = doc.ContentHash
	details["chunk_id"] = string(chunk.ID)
	if len(staleReasons) > 0 {
		details["state"] = "stale"
		details["stale_reasons"] = staleReasons
		return documentFreshnessOutcome{
			status:   DiagnosticStatusStale,
			severity: DiagnosticSeverityError,
			ok:       false,
			message:  fmt.Sprintf("document chunk %s/%s/%s source view is stale", doc.DatasetID, doc.ID, chunk.ID),
			details:  details,
		}
	}
	if !comparable {
		details["state"] = "unknown"
		return documentFreshnessOutcome{
			status:   DiagnosticStatusNotImplemented,
			severity: DiagnosticSeverityWarning,
			ok:       true,
			message:  fmt.Sprintf("document chunk %s/%s/%s source view cannot be proven from available signatures", doc.DatasetID, doc.ID, chunk.ID),
			details:  details,
		}
	}
	details["state"] = "fresh"
	return documentFreshnessOutcome{
		status:   DiagnosticStatusOK,
		severity: DiagnosticSeverityInfo,
		ok:       true,
		message:  fmt.Sprintf("document chunk %s/%s/%s source view is consistent", doc.DatasetID, doc.ID, chunk.ID),
		details:  details,
	}
}

func addSourceDiagnostics(result *DiagnosticProbeResult, req DiagnosticProbeRequest) {
	if req.System.assembly.HasSource(SourceMessageLog) {
		addDependencyCheck(result, "source.message_store", "", req.Scope, "MessageStore", req.Deps.MessageStore != nil)
	}
	if req.System.assembly.HasSource(SourceDocumentStore) {
		addDependencyCheck(result, "source.document_store", "", req.Scope, "DocumentStore", req.Deps.DocumentStore != nil)
	}
}

func addCapabilityDiagnostics(result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability) {
	if !req.System.assembly.HasCapability(capability) {
		addCheck(result, fmt.Sprintf("capability.%s.declared", capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "capability is not declared by the assembly", nil)
		return
	}

	switch capability {
	case CapabilityRecentWindow:
		addDependencyCheck(result, "capability.recent_window.message_store", capability, req.Scope, "MessageStore", req.Deps.MessageStore != nil)
	case CapabilitySummaryDAG:
		addDependencyCheck(result, "capability.summary_dag.store", capability, req.Scope, "SummaryStore", req.Deps.SummaryStore != nil)
		addDependencyCheck(result, "capability.summary_dag.service", capability, req.Scope, "Summarizer", req.Deps.Summarizer != nil)
	case CapabilityDocumentChunks:
		addDependencyCheck(result, "capability.document_chunks.store", capability, req.Scope, "ChunkStore", req.Deps.ChunkStore != nil)
		addDependencyCheck(result, "capability.document_chunks.service", capability, req.Scope, "DocumentChunker", req.Deps.DocumentChunker != nil)
	case CapabilityObservationLedger:
		addDependencyCheck(result, "capability.observation_ledger.store", capability, req.Scope, "ObservationStore", req.Deps.ObservationStore != nil)
		addDependencyCheck(result, "capability.observation_ledger.service", capability, req.Scope, "ObservationExtractor", req.Deps.ObservationExtractor != nil)
	case CapabilityFactLedger:
		addDependencyCheck(result, "capability.fact_ledger.store", capability, req.Scope, "FactStore", req.Deps.FactStore != nil)
		addDependencyCheck(result, "capability.fact_ledger.service", capability, req.Scope, "FactReconciler", req.Deps.FactReconciler != nil)
		addCheck(result, "capability.fact_ledger.lifecycle_semantics", capability, req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "fact ledger lifecycle semantics support active, superseded, retracted, conflict, and revision lineage", map[string]any{
			"statuses": []string{"active", "superseded", "retracted", "conflict"},
		})
	case CapabilityFactGraph:
		addDependencyCheck(result, "capability.fact_graph.store", capability, req.Scope, "FactGraphStore", req.Deps.FactGraphStore != nil)
		addDependencyCheck(result, "capability.fact_graph.service", capability, req.Scope, "FactGraphBuilder", req.Deps.FactGraphBuilder != nil)
	case CapabilityEntityProfile:
		addDependencyCheck(result, "capability.entity_profile.store", capability, req.Scope, "EntityProfileStore", req.Deps.EntityProfileStore != nil)
		addDependencyCheck(result, "capability.entity_profile.service", capability, req.Scope, "EntityProfileBuilder", req.Deps.EntityProfileBuilder != nil)
	case CapabilityEntityTimeline:
		addDependencyCheck(result, "capability.entity_timeline.store", capability, req.Scope, "EntityTimelineStore", req.Deps.EntityTimelineStore != nil)
		addDependencyCheck(result, "capability.entity_timeline.service", capability, req.Scope, "EntityTimelineBuilder", req.Deps.EntityTimelineBuilder != nil)
	default:
		addCheck(result, fmt.Sprintf("capability.%s.implemented", capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "capability is not implemented by the root facade", nil)
	}
}

func addProjectionDiagnostics(result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability) {
	baseNamespace, ok := req.System.assembly.ProjectionNamespace(capability)
	if !ok {
		if capabilitySupportsProjectionDiagnostics(capability) {
			addCheck(result, fmt.Sprintf("projection.%s.binding", capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusWarning, DiagnosticSeverityWarning, true, "projection namespace is not declared; indexed consistency checks are skipped", nil)
		}
		return
	}

	addCheck(result, fmt.Sprintf("projection.%s.binding", capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "projection namespace is declared", map[string]any{
		"base_namespace": baseNamespace,
	})
	addDependencyCheck(result, fmt.Sprintf("projection.%s.index", capability), capability, req.Scope, "Index", req.Deps.Index != nil)

	scopedNamespace, err := projectors.ScopedNamespace(baseNamespace, req.Scope)
	if err != nil {
		addCheck(result, fmt.Sprintf("projection.%s.scoped_namespace", capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusError, DiagnosticSeverityError, false, "scoped projection namespace cannot be computed", map[string]any{
			"base_namespace": baseNamespace,
			"error":          err.Error(),
		})
		return
	}
	addCheck(result, fmt.Sprintf("projection.%s.scoped_namespace", capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "scoped projection namespace is computable", map[string]any{
		"base_namespace":   baseNamespace,
		"scoped_namespace": scopedNamespace,
		"runtime_id":       req.Scope.RuntimeID,
		"user_id":          req.Scope.UserID,
	})
}

func addFreshnessCapabilityChecks(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest, capability Capability) {
	switch capability {
	case CapabilityDocumentChunks:
		if len(result.Documents) > 0 {
			addDocumentTargetFreshnessDiagnostics(ctx, result, req)
			return
		}
		addCheck(result, "freshness.document_chunks.targets", capability, req.Scope, LifecycleTarget{}, DiagnosticStatusNotImplemented, DiagnosticSeverityWarning, true, "document chunk freshness requires explicit document targets; full scans are not implemented", map[string]any{
			"requires_targets": true,
		})
	case CapabilityFactLedger:
		addCheck(result, "freshness.fact.reconcile_semantics", capability, req.Scope, LifecycleTarget{}, DiagnosticStatusOK, DiagnosticSeverityInfo, true, "fact lifecycle ledger semantics are implemented; document freshness remains deferred", map[string]any{
			"implemented_stage": "Stage 4",
		})
	default:
		if _, ok := req.System.assembly.ProjectionNamespace(capability); ok {
			addCheck(result, fmt.Sprintf("consistency.projection_records.%s", capability), capability, req.Scope, LifecycleTarget{}, DiagnosticStatusNotImplemented, DiagnosticSeverityInfo, true, "record-level projection scan is not implemented in Stage 2", map[string]any{
				"deferred_stage": "Stage 5",
			})
		}
	}
}

func addDocumentTargetFreshnessDiagnostics(ctx context.Context, result *DiagnosticProbeResult, req DiagnosticProbeRequest) {
	for _, target := range result.Documents {
		scope := req.Scope
		scope.DatasetID = target.DatasetID
		details := documentTargetDetails(target, scope)
		checkTarget := lifecycleTargetForDocument(target)
		if req.Deps.DocumentStore == nil || req.Deps.ChunkStore == nil {
			addCheck(result, "freshness.document_chunks.target", CapabilityDocumentChunks, scope, checkTarget, DiagnosticStatusError, DiagnosticSeverityError, false, "document chunk freshness requires DocumentStore and ChunkStore", details)
			continue
		}

		doc, ok, err := req.Deps.DocumentStore.Get(ctx, target.DatasetID, target.DocumentID)
		if err != nil {
			details["error"] = err.Error()
			addCheck(result, "freshness.document_chunks.target", CapabilityDocumentChunks, scope, checkTarget, DiagnosticStatusError, DiagnosticSeverityError, false, "canonical document lookup failed", details)
			continue
		}
		if !ok {
			details["state"] = "missing_document"
			details["chunk_count"] = 0
			addCheck(result, "freshness.document_chunks.target", CapabilityDocumentChunks, scope, checkTarget, DiagnosticStatusMissing, DiagnosticSeverityError, false, fmt.Sprintf("canonical document %s is missing", documentTargetLabel(target)), details)
			continue
		}

		chunks, err := req.Deps.ChunkStore.ListChunks(ctx, target.DocumentID, viewdocument.ListOptions{Scope: &scope})
		if err != nil {
			details["error"] = err.Error()
			addCheck(result, "freshness.document_chunks.target", CapabilityDocumentChunks, scope, checkTarget, DiagnosticStatusError, DiagnosticSeverityError, false, "document chunk lookup failed", details)
			continue
		}
		outcome := compareDocumentTargetFreshness(target, scope, doc, chunks)
		addCheck(result, "freshness.document_chunks.target", CapabilityDocumentChunks, scope, checkTarget, outcome.status, outcome.severity, outcome.ok, outcome.message, outcome.details)
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

func addDependencyCheck(result *DiagnosticProbeResult, name string, capability Capability, scope Scope, dependency string, ready bool) {
	status := DiagnosticStatusOK
	severity := DiagnosticSeverityInfo
	if !ready {
		status = DiagnosticStatusError
		severity = DiagnosticSeverityError
	}
	addCheck(result, name, capability, scope, LifecycleTarget{}, status, severity, ready, dependencyMessage(dependency, ready), nil)
}

func addCheck(result *DiagnosticProbeResult, name string, capability Capability, scope Scope, target LifecycleTarget, status DiagnosticStatus, severity DiagnosticSeverity, ok bool, message string, details map[string]any) {
	result.Checks = append(result.Checks, newDiagnosticCheck(name, capability, scope, target, status, severity, ok, message, details))
}

func addCheckWithRepair(result *DiagnosticProbeResult, name string, capability Capability, scope Scope, target LifecycleTarget, status DiagnosticStatus, severity DiagnosticSeverity, ok bool, message string, details map[string]any, repairHint string) {
	check := newDiagnosticCheck(name, capability, scope, target, status, severity, ok, message, details)
	check.RepairHint = strings.TrimSpace(repairHint)
	result.Checks = append(result.Checks, check)
}

func newDiagnosticCheck(name string, capability Capability, scope Scope, target LifecycleTarget, status DiagnosticStatus, severity DiagnosticSeverity, ok bool, message string, details map[string]any) DiagnosticCheck {
	return DiagnosticCheck{
		Name:       name,
		Capability: capability,
		Scope:      scope,
		Target:     target,
		Status:     status,
		OK:         ok,
		Severity:   severity,
		Message:    message,
		Details:    cloneDiagnosticDetails(details),
	}
}

func normalizeDiagnosticPageSize(pageSize int) int {
	if pageSize <= 0 {
		return defaultDiagnosticPageSize
	}
	if pageSize > maxDiagnosticPageSize {
		return maxDiagnosticPageSize
	}
	return pageSize
}

func (report *DiagnosticReport) addCheck(name string, capability Capability, status DiagnosticStatus, severity DiagnosticSeverity, ok bool, message string, details map[string]any) {
	report.addProbeCheck(newDiagnosticCheck(name, capability, report.Scope, LifecycleTarget{}, status, severity, ok, message, details))
}

func (report *DiagnosticReport) addProbeCheck(check DiagnosticCheck) {
	if check.Scope.IsZero() {
		check.Scope = report.Scope
	}
	check.Details = cloneDiagnosticDetails(check.Details)
	report.Checks = append(report.Checks, check)
}

func finalizeDiagnosticReport(report *DiagnosticReport, defaultMessage string) {
	report.Ready = true
	report.OK = true
	report.Warnings = nil
	for _, check := range report.Checks {
		if check.Severity == DiagnosticSeverityError && !check.OK {
			report.Ready = false
			report.OK = false
		}
		if check.Severity == DiagnosticSeverityWarning {
			report.Warnings = append(report.Warnings, check.Message)
		}
	}
	if defaultMessage != "" {
		report.Message = defaultMessage
		return
	}
	if report.Message != "" {
		return
	}
	if report.OK {
		report.Message = "diagnostics checks completed"
		return
	}
	report.Message = "diagnostics checks found missing dependencies"
}

func plannedStageDetails(stages []PlannedStage) []map[string]any {
	if len(stages) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(stages))
	for _, stage := range stages {
		out = append(out, map[string]any{
			"name":       stage.Name,
			"async":      stage.Async,
			"optional":   stage.Optional,
			"capability": string(stage.Capability),
		})
	}
	return out
}

func capabilityStrings(capabilities []Capability) []string {
	if len(capabilities) == 0 {
		return nil
	}
	out := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		if capability != "" {
			out = append(out, string(capability))
		}
	}
	return out
}

func documentTargetTraceDetails(targets []DocumentTarget) []map[string]string {
	if len(targets) == 0 {
		return nil
	}
	out := make([]map[string]string, 0, len(targets))
	for _, target := range targets {
		out = append(out, map[string]string{
			"dataset_id":  target.DatasetID,
			"document_id": target.DocumentID,
		})
	}
	return out
}

func scopeTraceDetails(scope Scope) map[string]any {
	return map[string]any{
		"runtime_id":      scope.RuntimeID,
		"user_id":         scope.UserID,
		"agent_id":        scope.AgentID,
		"conversation_id": scope.ConversationID,
		"dataset_id":      scope.DatasetID,
		"entity_id":       scope.EntityID,
	}
}

func projectionTraceDetails(req DiagnosticProbeRequest, capabilities []Capability) map[string]any {
	details := map[string]any{}
	if req.System == nil {
		return details
	}
	for _, capability := range capabilities {
		baseNamespace, ok := req.System.assembly.ProjectionNamespace(capability)
		entry := map[string]any{
			"capability": string(capability),
			"filter":     filterForTraceCapability(capability, req.Scope),
		}
		if ok {
			entry["base_namespace"] = baseNamespace
			scopedNamespace, err := projectors.ScopedNamespace(baseNamespace, req.Scope)
			if err != nil {
				entry["scoped_namespace_error"] = err.Error()
			} else {
				entry["scoped_namespace"] = scopedNamespace
			}
		} else {
			entry["projection_declared"] = false
		}
		details[string(capability)] = entry
	}
	return details
}

func filterForTraceCapability(capability Capability, scope Scope) any {
	switch capability {
	case CapabilitySummaryDAG:
		return summaryScopeFilter(scope)
	case CapabilityDocumentChunks:
		return documentScopeFilter(scope)
	case CapabilityFactLedger:
		return factScopeFilter(scope)
	case CapabilityObservationLedger, CapabilityFactGraph, CapabilityEntityProfile, CapabilityEntityTimeline:
		return semanticScopeFilter(scope)
	default:
		return nil
	}
}

func consistencyScopeFilter(capability Capability, scope Scope) retrieval.Filter {
	switch capability {
	case CapabilitySummaryDAG:
		return summaryScopeFilter(scope)
	case CapabilityDocumentChunks:
		return documentScopeFilter(scope)
	case CapabilityFactLedger:
		return factScopeFilter(scope)
	case CapabilityObservationLedger, CapabilityFactGraph, CapabilityEntityProfile, CapabilityEntityTimeline:
		return semanticScopeFilter(scope)
	default:
		return retrieval.Filter{}
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

func normalizeConsistencyCheckKinds(in []ConsistencyCheckKind) []ConsistencyCheckKind {
	if in == nil {
		return nil
	}
	out := make([]ConsistencyCheckKind, 0, len(in))
	seen := map[ConsistencyCheckKind]bool{}
	for _, kind := range in {
		trimmed := ConsistencyCheckKind(strings.TrimSpace(string(kind)))
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
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

func encodeConsistencyPageState(state consistencyPageState) (string, error) {
	state.ensure()
	done := make([]string, 0, len(state.done))
	for key := range state.done {
		done = append(done, key)
	}
	raw, err := json.Marshal(struct {
		Positions map[string]string `json:"positions,omitempty"`
		Done      []string          `json:"done,omitempty"`
	}{
		Positions: state.Positions,
		Done:      done,
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeConsistencyPageState(token string) (consistencyPageState, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		state := consistencyPageState{}
		state.ensure()
		return state, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return consistencyPageState{}, err
	}
	var state consistencyPageState
	if err := json.Unmarshal(raw, &state); err != nil {
		return consistencyPageState{}, err
	}
	state.ensure()
	return state, nil
}

func consistencyProjectionMetadataKeys(capability Capability) []string {
	keys := []string{
		projectors.MetadataViewKindKey,
		projectors.MetadataRecordTypeKey,
		projectors.MetadataRuntimeIDKey,
		projectors.MetadataUserIDKey,
		projectors.MetadataAgentIDKey,
		projectors.MetadataConversationIDKey,
		projectors.MetadataDatasetIDKey,
		projectors.MetadataEntityIDKey,
		indexed.MetadataSourceRefsKey,
		indexed.MetadataSignatureKey,
	}
	switch capability {
	case CapabilityDocumentChunks:
		keys = append(keys, projectors.MetadataDocumentIDKey, projectors.MetadataChunkIDKey)
	case CapabilitySummaryDAG:
		keys = append(keys, projectors.MetadataNodeIDKey)
	case CapabilityObservationLedger:
		keys = append(keys, projectors.MetadataObservationIDKey, projectors.MetadataPredicateKey)
	case CapabilityFactLedger:
		keys = append(keys, projectors.MetadataFactIDKey, projectors.MetadataPredicateKey, projectors.MetadataStatusKey)
	case CapabilityFactGraph:
		keys = append(keys, projectors.MetadataNodeIDKey, projectors.MetadataEdgeIDKey, projectors.MetadataNodeKindKey, projectors.MetadataFromKey, projectors.MetadataToKey, projectors.MetadataStatusKey)
	case CapabilityEntityProfile:
		keys = append(keys, projectors.MetadataProfileIDKey)
	case CapabilityEntityTimeline:
		keys = append(keys, projectors.MetadataEventIDKey)
	}
	return keys
}

func requiredProjectionMetadataKeys(capability Capability, metadata map[string]any) []string {
	keys := []string{
		projectors.MetadataViewKindKey,
		projectors.MetadataRecordTypeKey,
		projectors.MetadataRuntimeIDKey,
	}
	switch capability {
	case CapabilityDocumentChunks:
		return append(keys, projectors.MetadataDatasetIDKey, projectors.MetadataDocumentIDKey, projectors.MetadataChunkIDKey)
	case CapabilitySummaryDAG:
		return append(keys, projectors.MetadataConversationIDKey, projectors.MetadataNodeIDKey)
	case CapabilityObservationLedger:
		return append(keys, projectors.MetadataObservationIDKey)
	case CapabilityFactLedger:
		return append(keys, projectors.MetadataFactIDKey, projectors.MetadataStatusKey)
	case CapabilityFactGraph:
		recordType := metadataString(metadata, projectors.MetadataRecordTypeKey)
		if recordType == projectors.RecordTypeFactEdge {
			return append(keys, projectors.MetadataEdgeIDKey, projectors.MetadataFromKey, projectors.MetadataToKey)
		}
		return append(keys, projectors.MetadataNodeIDKey)
	case CapabilityEntityProfile:
		return append(keys, projectors.MetadataProfileIDKey)
	case CapabilityEntityTimeline:
		return append(keys, projectors.MetadataEventIDKey)
	default:
		return keys
	}
}

func projectionRecordDetails(req DiagnosticProbeRequest, capability Capability, namespace string, doc retrieval.Doc) map[string]any {
	return map[string]any{
		"record_id":       doc.ID,
		"namespace":       namespace,
		"capability":      string(capability),
		"page_size":       req.PageSize,
		"record_type":     metadataString(doc.Metadata, projectors.MetadataRecordTypeKey),
		"view_kind":       metadataString(doc.Metadata, projectors.MetadataViewKindKey),
		"runtime_id":      metadataString(doc.Metadata, projectors.MetadataRuntimeIDKey),
		"user_id":         metadataString(doc.Metadata, projectors.MetadataUserIDKey),
		"dataset_id":      metadataString(doc.Metadata, projectors.MetadataDatasetIDKey),
		"document_id":     metadataString(doc.Metadata, projectors.MetadataDocumentIDKey),
		"chunk_id":        metadataString(doc.Metadata, projectors.MetadataChunkIDKey),
		"requested_scope": scopeTraceDetails(req.Scope),
	}
}

func projectionRecordLifecycleTarget(capability Capability, metadata map[string]any) LifecycleTarget {
	target := LifecycleTarget{Capability: capability}
	if capability == CapabilityDocumentChunks {
		target.Kind = "document"
		target.DatasetID = metadataString(metadata, projectors.MetadataDatasetIDKey)
		target.DocumentID = metadataString(metadata, projectors.MetadataDocumentIDKey)
	}
	return target
}

func projectionRepairHint(capability Capability, target LifecycleTarget) string {
	if capability == CapabilityDocumentChunks && target.DatasetID != "" && target.DocumentID != "" {
		return fmt.Sprintf("rebuild capability=%s target=%s/%s", capability, target.DatasetID, target.DocumentID)
	}
	return fmt.Sprintf("rebuild capability=%s target=scope", capability)
}

func projectionHardPartitionMismatches(scope Scope, metadata map[string]any) []string {
	var reasons []string
	if got := metadataString(metadata, projectors.MetadataRuntimeIDKey); got != scope.RuntimeID {
		reasons = append(reasons, fmt.Sprintf("runtime_id metadata %q does not match requested scope %q", got, scope.RuntimeID))
	}
	if got := metadataString(metadata, projectors.MetadataUserIDKey); got != scope.UserID {
		reasons = append(reasons, fmt.Sprintf("user_id metadata %q does not match requested scope %q", got, scope.UserID))
	}
	return reasons
}

func projectionScopeFromMetadata(fallback Scope, metadata map[string]any) Scope {
	scope := fallback
	if value := metadataString(metadata, projectors.MetadataRuntimeIDKey); value != "" {
		scope.RuntimeID = value
	}
	if value, ok := metadata[projectors.MetadataUserIDKey]; ok {
		scope.UserID = stringFromAny(value)
	}
	if value, ok := metadata[projectors.MetadataAgentIDKey]; ok {
		scope.AgentID = stringFromAny(value)
	}
	if value, ok := metadata[projectors.MetadataConversationIDKey]; ok {
		scope.ConversationID = stringFromAny(value)
	}
	if value, ok := metadata[projectors.MetadataDatasetIDKey]; ok {
		scope.DatasetID = stringFromAny(value)
	}
	if value, ok := metadata[projectors.MetadataEntityIDKey]; ok {
		scope.EntityID = stringFromAny(value)
	}
	return scope
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok {
		return ""
	}
	return stringFromAny(value)
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func appendStringDetail(value any, item string) []string {
	var out []string
	switch existing := value.(type) {
	case []string:
		out = append(out, existing...)
	case []any:
		for _, entry := range existing {
			if s := stringFromAny(entry); s != "" {
				out = append(out, s)
			}
		}
	}
	if item != "" {
		out = append(out, item)
	}
	return out
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
