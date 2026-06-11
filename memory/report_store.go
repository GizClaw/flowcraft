package memory

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ReportStore persists lifecycle and diagnostics reports for later inspection.
type ReportStore interface {
	PutLifecycleReport(context.Context, LifecycleExecutionReport) error
	GetLifecycleReport(context.Context, TraceID) (LifecycleExecutionReport, bool, error)
	ListLifecycleReports(context.Context) ([]LifecycleExecutionReport, error)
	PutDiagnosticReport(context.Context, DiagnosticReport) error
	GetDiagnosticReport(context.Context, TraceID) (DiagnosticReport, bool, error)
	ListDiagnosticReports(context.Context) ([]DiagnosticReport, error)
}

// MemoryReportStore is an in-memory reference implementation of ReportStore.
type MemoryReportStore struct {
	mu                sync.Mutex
	lifecycleOrder    []TraceID
	lifecycleReports  map[TraceID]LifecycleExecutionReport
	diagnosticOrder   []TraceID
	diagnosticReports map[TraceID]DiagnosticReport
}

// NewMemoryReportStore returns a local report reference store.
func NewMemoryReportStore() *MemoryReportStore {
	return &MemoryReportStore{
		lifecycleReports:  make(map[TraceID]LifecycleExecutionReport),
		diagnosticReports: make(map[TraceID]DiagnosticReport),
	}
}

// PutLifecycleReport stores the latest lifecycle report for its trace.
func (s *MemoryReportStore) PutLifecycleReport(_ context.Context, report LifecycleExecutionReport) error {
	if s == nil {
		return errdefs.NotAvailablef("memory: report store is not configured")
	}
	traceID := ensureTraceID(report.TraceID)
	report.TraceID = traceID
	if report.Operation.TraceID == "" {
		report.Operation.TraceID = traceID
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifecycleReports == nil {
		s.lifecycleReports = make(map[TraceID]LifecycleExecutionReport)
	}
	if _, exists := s.lifecycleReports[traceID]; !exists {
		s.lifecycleOrder = append(s.lifecycleOrder, traceID)
	}
	s.lifecycleReports[traceID] = cloneLifecycleExecutionReport(report)
	return nil
}

// GetLifecycleReport returns the latest lifecycle report for a trace.
func (s *MemoryReportStore) GetLifecycleReport(_ context.Context, traceID TraceID) (LifecycleExecutionReport, bool, error) {
	if s == nil {
		return LifecycleExecutionReport{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	report, ok := s.lifecycleReports[TraceID(string(traceID))]
	if !ok {
		return LifecycleExecutionReport{}, false, nil
	}
	return cloneLifecycleExecutionReport(report), true, nil
}

// ListLifecycleReports returns lifecycle reports in first-seen trace order.
func (s *MemoryReportStore) ListLifecycleReports(_ context.Context) ([]LifecycleExecutionReport, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]LifecycleExecutionReport, 0, len(s.lifecycleOrder))
	for _, traceID := range s.lifecycleOrder {
		out = append(out, cloneLifecycleExecutionReport(s.lifecycleReports[traceID]))
	}
	return out, nil
}

// PutDiagnosticReport stores the latest diagnostic report for its trace.
func (s *MemoryReportStore) PutDiagnosticReport(_ context.Context, report DiagnosticReport) error {
	if s == nil {
		return errdefs.NotAvailablef("memory: report store is not configured")
	}
	traceID := ensureTraceID(report.TraceID)
	report.TraceID = traceID

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.diagnosticReports == nil {
		s.diagnosticReports = make(map[TraceID]DiagnosticReport)
	}
	if _, exists := s.diagnosticReports[traceID]; !exists {
		s.diagnosticOrder = append(s.diagnosticOrder, traceID)
	}
	s.diagnosticReports[traceID] = cloneDiagnosticReport(report)
	return nil
}

// GetDiagnosticReport returns the latest diagnostic report for a trace.
func (s *MemoryReportStore) GetDiagnosticReport(_ context.Context, traceID TraceID) (DiagnosticReport, bool, error) {
	if s == nil {
		return DiagnosticReport{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	report, ok := s.diagnosticReports[TraceID(string(traceID))]
	if !ok {
		return DiagnosticReport{}, false, nil
	}
	return cloneDiagnosticReport(report), true, nil
}

// ListDiagnosticReports returns diagnostic reports in first-seen trace order.
func (s *MemoryReportStore) ListDiagnosticReports(_ context.Context) ([]DiagnosticReport, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DiagnosticReport, 0, len(s.diagnosticOrder))
	for _, traceID := range s.diagnosticOrder {
		out = append(out, cloneDiagnosticReport(s.diagnosticReports[traceID]))
	}
	return out, nil
}

func (r *System) putLifecycleReport(ctx context.Context, report LifecycleExecutionReport) error {
	if r == nil || r.reportStore == nil {
		return nil
	}
	return r.reportStore.PutLifecycleReport(ctx, report)
}

func (r *System) putDiagnosticReport(ctx context.Context, report DiagnosticReport) error {
	if r == nil || r.reportStore == nil {
		return nil
	}
	return r.reportStore.PutDiagnosticReport(ctx, report)
}

func cloneLifecycleExecutionReport(report LifecycleExecutionReport) LifecycleExecutionReport {
	report.Operation = cloneLifecycleOperation(report.Operation)
	report.Steps = cloneLifecycleSteps(report.Steps)
	report.TargetErrors = cloneLifecycleTargetErrors(report.TargetErrors)
	report.Checkpoint = cloneCheckpoint(report.Checkpoint)
	return report
}

func cloneLifecycleOperation(operation LifecycleOperation) LifecycleOperation {
	operation.Capabilities = cloneCapabilities(operation.Capabilities)
	operation.Documents = cloneDocumentTargets(operation.Documents)
	operation.Targets = cloneLifecycleTargets(operation.Targets)
	return operation
}

func cloneLifecycleTargets(in []LifecycleTarget) []LifecycleTarget {
	if in == nil {
		return nil
	}
	out := make([]LifecycleTarget, len(in))
	copy(out, in)
	return out
}

func cloneLifecycleSteps(in []LifecycleStep) []LifecycleStep {
	if in == nil {
		return nil
	}
	out := make([]LifecycleStep, len(in))
	for i, step := range in {
		out[i] = step
		out[i].Details = cloneDiagnosticDetails(step.Details)
	}
	return out
}

func cloneLifecycleTargetErrors(in []LifecycleTargetError) []LifecycleTargetError {
	if in == nil {
		return nil
	}
	out := make([]LifecycleTargetError, len(in))
	copy(out, in)
	return out
}

func cloneDiagnosticReport(report DiagnosticReport) DiagnosticReport {
	report.Capabilities = cloneCapabilities(report.Capabilities)
	report.Documents = cloneDocumentTargets(report.Documents)
	report.Checks = cloneDiagnosticChecks(report.Checks)
	report.Warnings = append([]string(nil), report.Warnings...)
	return report
}
