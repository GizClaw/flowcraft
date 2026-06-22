package memory_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/memory/derive"
	derivedocument "github.com/GizClaw/flowcraft/memory/derive/document"
	summaryderive "github.com/GizClaw/flowcraft/memory/derive/summary"
	"github.com/GizClaw/flowcraft/memory/internal/projectors"
	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	retrievalworkspace "github.com/GizClaw/flowcraft/memory/retrieval/workspace"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestRemovedCapabilitiesAreRejected(t *testing.T) {
	for _, capability := range []memory.Capability{
		"observation_ledger",
		"fact_ledger",
		"fact_graph",
		"entity_profile",
		"entity_timeline",
	} {
		t.Run(string(capability), func(t *testing.T) {
			err := memory.Compile(memory.Spec{
				Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog}},
				Capabilities: []memory.CapabilitySpec{{Capability: capability}},
			})
			if err == nil {
				t.Fatalf("Compile() error = nil, want removed capability rejected")
			}
		})
	}
}

func TestRemovedStagesAreUnsupportedByFacadePlan(t *testing.T) {
	for _, stage := range []string{
		"extract_observations",
		"reconcile_facts",
		"build_fact_graph",
		"build_entity_profiles",
		"build_entity_timeline",
		"retrieve_observations",
		"retrieve_facts",
		"retrieve_fact_graph",
		"retrieve_entity_profiles",
		"retrieve_entity_timeline",
		"expand_fact_graph",
	} {
		t.Run(stage, func(t *testing.T) {
			spec := memory.Spec{
				Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
				Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
			}
			if strings.HasPrefix(stage, "retrieve_") || stage == "expand_fact_graph" {
				spec.ReadStages = []memory.StageSpec{{Name: stage}}
			} else {
				spec.WriteStages = []memory.StageSpec{{Name: stage}}
			}
			_, err := memory.New(spec, memory.Deps{MessageStore: newMessageStore()})
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("New() error = %v, want validation error", err)
			}
		})
	}
}

func TestDefaultPlanContainsOnlySupportedStages(t *testing.T) {
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
	}, memory.Deps{MessageStore: newMessageStore()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	plan := mem.Plan()
	if len(plan.Write) != 0 {
		t.Fatalf("default Write = %+v, want no derived write stages", plan.Write)
	}
	stageNames := map[string]bool{}
	for _, stage := range plan.Read {
		stageNames[stage.Name] = true
	}
	if !stageNames["load_recent_messages"] || !stageNames["pack_context"] {
		t.Fatalf("default Read = %+v, want recent load and pack", plan.Read)
	}
	for _, removed := range []string{"retrieve_facts", "retrieve_fact_graph", "retrieve_entity_profiles", "retrieve_entity_timeline"} {
		if stageNames[removed] {
			t.Fatalf("default Read includes removed stage %q: %+v", removed, plan.Read)
		}
	}
	lifecycleStageNames := plannedStageNames(plan.Lifecycle)
	for _, stage := range []string{"readiness", "queue_stats", "rebuild", "reload", "reconcile", "freshness_check", "drain", "shutdown"} {
		if !lifecycleStageNames[stage] {
			t.Fatalf("default Lifecycle = %+v, missing %q", plan.Lifecycle, stage)
		}
	}
	diagnosticStageNames := plannedStageNames(plan.Diagnostics)
	for _, stage := range []string{"freshness", "consistency", "trace", "readiness", "queue_stats"} {
		if !diagnosticStageNames[stage] {
			t.Fatalf("default Diagnostics = %+v, missing %q", plan.Diagnostics, stage)
		}
	}
}

func TestFreshnessDefaultPlanRunsSynchronouslyWithoutJobStore(t *testing.T) {
	ctx := context.Background()
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
	}, memory.Deps{MessageStore: newMessageStore()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := mem.Freshness(ctx, memory.FreshnessRequest{
		Scope: memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"},
	})
	if err != nil {
		t.Fatalf("Freshness() error = %v", err)
	}
	if result.Status != memory.LifecycleStatusCompleted || !result.Accepted || !result.Supported {
		t.Fatalf("Freshness lifecycle report = %+v, want accepted supported completed", result.LifecycleExecutionReport)
	}
	if result.JobID != "" {
		t.Fatalf("Freshness JobID = %q, want sync execution without queued job", result.JobID)
	}
	if result.Diagnostics.Stage != "freshness" || len(result.Checks) == 0 {
		t.Fatalf("Freshness diagnostics = %+v, want freshness checks", result.Diagnostics)
	}
	if !hasDiagnosticCheck(result.Checks, "diagnostics.stage.freshness") {
		t.Fatalf("Freshness checks = %+v, want freshness stage check", result.Checks)
	}
}

func TestFreshnessCustomLifecycleDoesNotRequireDiagnosticsStage(t *testing.T) {
	ctx := context.Background()
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
		Lifecycle:    []memory.StageSpec{{Name: "freshness_check"}},
		Diagnostics:  []memory.StageSpec{{Name: "trace"}},
	}, memory.Deps{MessageStore: newMessageStore()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := mem.Freshness(ctx, memory.FreshnessRequest{
		Scope: memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"},
	})
	if err != nil {
		t.Fatalf("Freshness() error = %v", err)
	}
	if result.Status != memory.LifecycleStatusCompleted || !result.Accepted || !result.Supported {
		t.Fatalf("Freshness lifecycle report = %+v, want accepted supported completed", result.LifecycleExecutionReport)
	}
	if result.Diagnostics.Stage != "freshness" || !hasDiagnosticCheck(result.Checks, "diagnostics.stage.freshness") {
		t.Fatalf("Freshness diagnostics = %+v, want freshness checks despite custom diagnostics plan", result.Diagnostics)
	}

	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope: memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"},
		Stage: "freshness",
	})
	if err != nil {
		t.Fatalf("Diagnostics() error = %v", err)
	}
	if report.OK || report.Ready || !hasDiagnosticCheck(report.Checks, "diagnostics.stage.freshness") {
		t.Fatalf("Diagnostics() report = %+v, want undeclared freshness diagnostics rejected", report)
	}
}

func TestFreshnessDiagnosticStoreErrorFailsLifecycleReportMessage(t *testing.T) {
	ctx := context.Background()
	storeErr := errors.New("diagnostic report store failed")
	reportStore := &diagnosticFailingReportStore{
		delegate: memory.NewMemoryReportStore(),
		err:      storeErr,
	}
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
	}, memory.Deps{
		MessageStore: newMessageStore(),
		ReportStore:  reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := mem.Freshness(ctx, memory.FreshnessRequest{
		Scope: memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"},
	})
	if err == nil || !strings.Contains(err.Error(), storeErr.Error()) {
		t.Fatalf("Freshness() error = %v, want %q", err, storeErr)
	}
	if result.Status != memory.LifecycleStatusFailed {
		t.Fatalf("Freshness status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Message, storeErr.Error()) || !strings.Contains(result.Summary, storeErr.Error()) {
		t.Fatalf("Freshness message/summary = %q / %q, want diagnostic store error", result.Message, result.Summary)
	}

	stored, ok, err := reportStore.GetLifecycleReport(ctx, result.TraceID)
	if err != nil {
		t.Fatalf("GetLifecycleReport() error = %v", err)
	}
	if !ok {
		t.Fatalf("stored lifecycle report not found for trace %q", result.TraceID)
	}
	if stored.Status != memory.LifecycleStatusFailed || !strings.Contains(stored.Message, storeErr.Error()) || !strings.Contains(stored.Summary, storeErr.Error()) {
		t.Fatalf("stored lifecycle report = %+v, want failed report with diagnostic store error", stored)
	}
}

func TestDefaultDiagnosticsPlanRunsRegisteredProbes(t *testing.T) {
	ctx := context.Background()
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
	}, memory.Deps{MessageStore: newMessageStore()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for _, stage := range []string{"freshness", "consistency", "trace"} {
		t.Run(stage, func(t *testing.T) {
			report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
				Scope: memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"},
				Stage: stage,
			})
			if err != nil {
				t.Fatalf("Diagnostics(%q) error = %v", stage, err)
			}
			if !report.Ready || !report.OK {
				t.Fatalf("Diagnostics(%q) = %+v, want ready OK", stage, report)
			}
			if hasDiagnosticCheck(report.Checks, "diagnostics.stage."+stage) {
				return
			}
			t.Fatalf("Diagnostics(%q) checks = %+v, want stage check", stage, report.Checks)
		})
	}
}

func TestDocumentLifecycleEmptyCapabilitiesSelectConfiguredDocumentChunks(t *testing.T) {
	ctx := context.Background()
	mem, err := newDocumentChunkSystem()
	if err != nil {
		t.Fatalf("newDocumentChunkSystem() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv", DatasetID: "dataset"}
	documents := []memory.DocumentTarget{{DocumentID: "doc-1"}}

	tests := map[string]func(context.Context) (memory.LifecycleExecutionReport, error){
		"rebuild": func(ctx context.Context) (memory.LifecycleExecutionReport, error) {
			return mem.Rebuild(ctx, memory.RebuildRequest{Scope: scope, Documents: documents, DryRun: true})
		},
		"reload": func(ctx context.Context) (memory.LifecycleExecutionReport, error) {
			return mem.Reload(ctx, memory.ReloadRequest{Scope: scope, Documents: documents, DryRun: true})
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			report, err := run(ctx)
			if err != nil {
				t.Fatalf("%s error = %v", name, err)
			}
			if report.Status != memory.LifecycleStatusPlanned || !report.Accepted || !report.Supported {
				t.Fatalf("%s report = %+v, want planned document_chunks dry-run", name, report)
			}
			if !capabilityPresent(report.Operation.Capabilities, memory.CapabilityDocumentChunks) {
				t.Fatalf("%s capabilities = %+v, want document_chunks selected", name, report.Operation.Capabilities)
			}
			if len(report.Steps) != 1 || report.Steps[0].Name != "document_chunks.target" {
				t.Fatalf("%s steps = %+v, want document_chunks target path", name, report.Steps)
			}
		})
	}
}

func TestDocumentLifecycleRebuildScanDocumentsUsesBoundedPages(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	importProjectedDocument(t, ctx, fixture, "doc-2", "Babbage filed a second page of notes.")
	if err := fixture.chunkStore.DeleteDocument(ctx, fixture.scope, "doc-1"); err != nil {
		t.Fatalf("DeleteDocument(doc-1) error = %v", err)
	}
	if err := fixture.chunkStore.DeleteDocument(ctx, fixture.scope, "doc-2"); err != nil {
		t.Fatalf("DeleteDocument(doc-2) error = %v", err)
	}

	first, err := fixture.mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:         fixture.scope,
		ScanDocuments: true,
		PageSize:      1,
	})
	if err != nil {
		t.Fatalf("Rebuild(ScanDocuments first page) error = %v", err)
	}
	if first.Status != memory.LifecycleStatusEnqueued || first.JobID == "" {
		t.Fatalf("Rebuild(ScanDocuments first page) = %+v, want enqueued page rebuild", first)
	}
	if len(first.Operation.Documents) != 1 || first.Operation.Documents[0].DocumentID != "doc-1" {
		t.Fatalf("first page documents = %+v, want doc-1 only", first.Operation.Documents)
	}
	assertCheckpointBool(t, first.Checkpoint, "document_scan", true)
	assertCheckpointInt(t, first.Checkpoint, "document_scan_scanned", 1)
	assertCheckpointString(t, first.Checkpoint, "document_scan_next_page_token", "doc-1")
	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce(first scan rebuild) result = %+v error = %v, want completed", result, err)
	}
	if chunks, err := fixture.chunkStore.ListChunks(ctx, "doc-1", viewdocument.ListOptions{Scope: &fixture.scope}); err != nil || len(chunks) == 0 {
		t.Fatalf("doc-1 chunks = %+v err = %v, want rebuilt chunks", chunks, err)
	}
	if chunks, err := fixture.chunkStore.ListChunks(ctx, "doc-2", viewdocument.ListOptions{Scope: &fixture.scope}); err != nil || len(chunks) != 0 {
		t.Fatalf("doc-2 chunks = %+v err = %v, want second page untouched", chunks, err)
	}

	nextPageToken := first.Checkpoint["document_scan_next_page_token"].(string)
	second, err := fixture.mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:         fixture.scope,
		ScanDocuments: true,
		PageSize:      1,
		PageToken:     nextPageToken,
	})
	if err != nil {
		t.Fatalf("Rebuild(ScanDocuments second page) error = %v", err)
	}
	if second.Status != memory.LifecycleStatusEnqueued || second.JobID == "" {
		t.Fatalf("Rebuild(ScanDocuments second page) = %+v, want enqueued page rebuild", second)
	}
	if len(second.Operation.Documents) != 1 || second.Operation.Documents[0].DocumentID != "doc-2" {
		t.Fatalf("second page documents = %+v, want doc-2 only", second.Operation.Documents)
	}
	assertCheckpointString(t, second.Checkpoint, "document_scan_page_token", nextPageToken)
	assertCheckpointString(t, second.Checkpoint, "document_scan_next_page_token", "")
	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce(second scan rebuild) result = %+v error = %v, want completed", result, err)
	}
	if chunks, err := fixture.chunkStore.ListChunks(ctx, "doc-2", viewdocument.ListOptions{Scope: &fixture.scope}); err != nil || len(chunks) == 0 {
		t.Fatalf("doc-2 chunks = %+v err = %v, want rebuilt chunks", chunks, err)
	}
}

func TestDocumentLifecycleReloadScanDocumentsUsesBoundedPages(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	importProjectedDocument(t, ctx, fixture, "doc-2", "Babbage filed a second page of notes.")
	if err := fixture.chunkStore.DeleteDocument(ctx, fixture.scope, "doc-1"); err != nil {
		t.Fatalf("DeleteDocument(doc-1) error = %v", err)
	}
	if err := fixture.chunkStore.DeleteDocument(ctx, fixture.scope, "doc-2"); err != nil {
		t.Fatalf("DeleteDocument(doc-2) error = %v", err)
	}

	first, err := fixture.mem.Reload(ctx, memory.ReloadRequest{
		Scope:         fixture.scope,
		ScanDocuments: true,
		PageSize:      1,
	})
	if err != nil {
		t.Fatalf("Reload(ScanDocuments first page) error = %v", err)
	}
	if first.Status != memory.LifecycleStatusEnqueued || first.JobID == "" {
		t.Fatalf("Reload(ScanDocuments first page) = %+v, want enqueued page reload", first)
	}
	if len(first.Operation.Documents) != 1 || first.Operation.Documents[0].DocumentID != "doc-1" {
		t.Fatalf("first page documents = %+v, want doc-1 only", first.Operation.Documents)
	}
	assertCheckpointBool(t, first.Checkpoint, "document_scan", true)
	assertCheckpointInt(t, first.Checkpoint, "document_scan_scanned", 1)
	assertCheckpointString(t, first.Checkpoint, "document_scan_next_page_token", "doc-1")
	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce(first scan reload) result = %+v error = %v, want completed", result, err)
	}
	if chunks, err := fixture.chunkStore.ListChunks(ctx, "doc-1", viewdocument.ListOptions{Scope: &fixture.scope}); err != nil || len(chunks) == 0 {
		t.Fatalf("doc-1 chunks = %+v err = %v, want reloaded chunks", chunks, err)
	}
	if chunks, err := fixture.chunkStore.ListChunks(ctx, "doc-2", viewdocument.ListOptions{Scope: &fixture.scope}); err != nil || len(chunks) != 0 {
		t.Fatalf("doc-2 chunks = %+v err = %v, want second page untouched", chunks, err)
	}

	nextPageToken := first.Checkpoint["document_scan_next_page_token"].(string)
	second, err := fixture.mem.Reload(ctx, memory.ReloadRequest{
		Scope:         fixture.scope,
		ScanDocuments: true,
		PageSize:      1,
		PageToken:     nextPageToken,
	})
	if err != nil {
		t.Fatalf("Reload(ScanDocuments second page) error = %v", err)
	}
	if second.Status != memory.LifecycleStatusEnqueued || second.JobID == "" {
		t.Fatalf("Reload(ScanDocuments second page) = %+v, want enqueued page reload", second)
	}
	if len(second.Operation.Documents) != 1 || second.Operation.Documents[0].DocumentID != "doc-2" {
		t.Fatalf("second page documents = %+v, want doc-2 only", second.Operation.Documents)
	}
	assertCheckpointString(t, second.Checkpoint, "document_scan_page_token", nextPageToken)
	assertCheckpointString(t, second.Checkpoint, "document_scan_next_page_token", "")
	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce(second scan reload) result = %+v error = %v, want completed", result, err)
	}
	if chunks, err := fixture.chunkStore.ListChunks(ctx, "doc-2", viewdocument.ListOptions{Scope: &fixture.scope}); err != nil || len(chunks) == 0 {
		t.Fatalf("doc-2 chunks = %+v err = %v, want reloaded chunks", chunks, err)
	}
}

func TestReconcileScanDocumentsDryRunPlansRepairWithoutChangingProjection(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	makeProjectedDocumentStale(t, ctx, fixture)
	before := requireDocumentChunkStale(t, ctx, fixture)

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:         fixture.scope,
		ScanDocuments: true,
		DryRun:        true,
		PageSize:      1,
	})
	if err != nil {
		t.Fatalf("Reconcile(ScanDocuments DryRun) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusPlanned || report.JobID != "" {
		t.Fatalf("Reconcile(ScanDocuments DryRun) = %+v, want planned repair", report)
	}
	if len(report.Operation.Documents) != 1 || report.Operation.Documents[0].DocumentID != "doc-1" {
		t.Fatalf("dry-run scan documents = %+v, want doc-1", report.Operation.Documents)
	}
	assertCheckpointBool(t, report.Checkpoint, "document_scan", true)
	assertCheckpointString(t, report.Checkpoint, "repair_targets_source", "document_scan")
	assertCheckpointInt(t, report.Checkpoint, "repair_target_count", 1)
	if !lifecycleStepPresent(report.Steps, "document_chunks.target") {
		t.Fatalf("dry-run steps = %+v, want planned document_chunks repair", report.Steps)
	}
	after := requireDocumentChunkStale(t, ctx, fixture)
	if before.Details["stale_records"] != after.Details["stale_records"] {
		t.Fatalf("stale records changed after scan dry-run: before %+v after %+v", before.Details, after.Details)
	}
}

func TestReconcileScanDocumentsRepairsOnlyScannedPage(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	importProjectedDocument(t, ctx, fixture, "doc-2", "Babbage filed a second page of notes.")
	makeProjectedDocumentStale(t, ctx, fixture)
	makeProjectedDocumentStaleByID(t, ctx, fixture, "doc-2", "Babbage rewrote the second page of notes.")
	requireDocumentChunkStaleCount(t, ctx, fixture, 2)

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:         fixture.scope,
		ScanDocuments: true,
		PageSize:      1,
	})
	if err != nil {
		t.Fatalf("Reconcile(ScanDocuments) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued || report.JobID == "" {
		t.Fatalf("Reconcile(ScanDocuments) = %+v, want enqueued repair", report)
	}
	if len(report.Operation.Documents) != 1 || report.Operation.Documents[0].DocumentID != "doc-1" {
		t.Fatalf("scan documents = %+v, want first page doc-1", report.Operation.Documents)
	}
	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce(scan reconcile) result = %+v error = %v, want completed", result, err)
	}
	stored, ok, err := fixture.reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport(scan reconcile) ok = %v err = %v, want stored report", ok, err)
	}
	assertCheckpointBool(t, stored.Checkpoint, "document_scan", true)
	assertCheckpointString(t, stored.Checkpoint, "repair_targets_source", "document_scan")
	assertCheckpointInt(t, stored.Checkpoint, "document_scan_scanned", 1)
	assertCheckpointInt(t, stored.Checkpoint, "document_scan_repaired", 1)
	requireDocumentChunkStaleCount(t, ctx, fixture, 1)
}

func TestReconcileScanDocumentsPrefersExplicitDocuments(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	importProjectedDocument(t, ctx, fixture, "doc-2", "Babbage filed a second page of notes.")
	makeProjectedDocumentStale(t, ctx, fixture)
	makeProjectedDocumentStaleByID(t, ctx, fixture, "doc-2", "Babbage rewrote the second page of notes.")
	requireDocumentChunkStaleCount(t, ctx, fixture, 2)

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:         fixture.scope,
		Documents:     []memory.DocumentTarget{{DocumentID: "doc-2"}},
		ScanDocuments: true,
		PageSize:      1,
	})
	if err != nil {
		t.Fatalf("Reconcile(explicit Documents + ScanDocuments) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued || report.JobID == "" {
		t.Fatalf("Reconcile(explicit Documents + ScanDocuments) = %+v, want enqueued explicit repair", report)
	}
	if len(report.Operation.Documents) != 1 || report.Operation.Documents[0].DocumentID != "doc-2" {
		t.Fatalf("explicit documents = %+v, want doc-2 only", report.Operation.Documents)
	}
	if _, ok := report.Checkpoint["document_scan"]; ok {
		t.Fatalf("checkpoint = %+v, want explicit documents to bypass document scan", report.Checkpoint)
	}
	assertCheckpointString(t, report.Checkpoint, "repair_targets_source", "explicit")
	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce(explicit over scan) result = %+v error = %v, want completed", result, err)
	}
	requireDocumentChunkStaleCount(t, ctx, fixture, 1)
}

func TestDocumentLifecycleScanDocumentsRequiresDatasetScope(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	scope := fixture.scope
	scope.DatasetID = ""

	report, err := fixture.mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:         scope,
		Capabilities:  []memory.Capability{memory.CapabilityDocumentChunks},
		ScanDocuments: true,
		PageSize:      1,
	})
	if err == nil {
		t.Fatalf("Rebuild(ScanDocuments without dataset) error = nil, report = %+v, want validation error", report)
	}
	if report.Status != memory.LifecycleStatusRejected || !strings.Contains(report.Message, "document scan requires scope.dataset_id") {
		t.Fatalf("Rebuild(ScanDocuments without dataset) report = %+v, want rejected dataset validation", report)
	}
	if _, ok := report.Checkpoint["document_scan"]; ok {
		t.Fatalf("checkpoint = %+v, want no document scan without dataset", report.Checkpoint)
	}
}

func TestDocumentLifecycleScanDocumentsRejectsDiagnosticsPageToken(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	token := "diagproj:v1:not-a-document-id"

	tests := map[string]func(context.Context) (memory.LifecycleExecutionReport, error){
		"rebuild": func(ctx context.Context) (memory.LifecycleExecutionReport, error) {
			return fixture.mem.Rebuild(ctx, memory.RebuildRequest{Scope: fixture.scope, ScanDocuments: true, PageToken: token})
		},
		"reload": func(ctx context.Context) (memory.LifecycleExecutionReport, error) {
			return fixture.mem.Reload(ctx, memory.ReloadRequest{Scope: fixture.scope, ScanDocuments: true, PageToken: token})
		},
		"reconcile": func(ctx context.Context) (memory.LifecycleExecutionReport, error) {
			return fixture.mem.Reconcile(ctx, memory.ReconcileRequest{Scope: fixture.scope, ScanDocuments: true, PageToken: token})
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			report, err := run(ctx)
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("%s(ScanDocuments diagnostics token) error = %v, report = %+v, want validation error", name, err, report)
			}
			if report.Status != memory.LifecycleStatusRejected || !strings.Contains(report.Message, "invalid document scan page token") {
				t.Fatalf("%s report = %+v, want rejected invalid document scan token", name, report)
			}
			if report.JobID != "" {
				t.Fatalf("%s report = %+v, want no job enqueued", name, report)
			}
			if lifecycleStepPresent(report.Steps, "message_index.conversation") || lifecycleStepPresent(report.Steps, "summary_dag.conversation") {
				t.Fatalf("%s steps = %+v, want no message/summary routing", name, report.Steps)
			}
		})
	}
}

func TestDocumentLifecycleScanDocumentsFalseKeepsNoDocumentBehavior(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)

	report, err := fixture.mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        fixture.scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
	})
	if err != nil {
		t.Fatalf("Rebuild(ScanDocuments false no documents) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusSkipped || report.JobID != "" {
		t.Fatalf("Rebuild(ScanDocuments false no documents) = %+v, want existing skipped behavior", report)
	}
	if len(report.Operation.Documents) != 0 {
		t.Fatalf("documents = %+v, want no implicit scan targets", report.Operation.Documents)
	}
	if _, ok := report.Checkpoint["document_scan"]; ok {
		t.Fatalf("checkpoint = %+v, want no document scan marker", report.Checkpoint)
	}
	if !lifecycleSkippedStepPresent(report.Steps, "document_chunks.targets") {
		t.Fatalf("steps = %+v, want no-document skipped step", report.Steps)
	}
}

func TestDocumentLifecycleEmptyCapabilitiesDoNotSelectConversationViewsWithoutDocumentChunks(t *testing.T) {
	ctx := context.Background()
	mem, err := newLifecycleSelectionSystem(t, true)
	if err != nil {
		t.Fatalf("newLifecycleSelectionSystem() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv", DatasetID: "dataset"}
	report, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:     scope,
		Documents: []memory.DocumentTarget{{DocumentID: "doc-1"}},
		DryRun:    true,
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("Rebuild() error = %v, want document target unsupported without document_chunks", err)
	}
	if len(report.Operation.Capabilities) != 0 {
		t.Fatalf("capabilities = %+v, want none when document_chunks is unavailable", report.Operation.Capabilities)
	}
	if lifecycleStepPresent(report.Steps, "message_index.conversation") || lifecycleStepPresent(report.Steps, "summary_dag.conversation") {
		t.Fatalf("steps = %+v, want no message/summary routing for document target", report.Steps)
	}
}

func TestDocumentLifecycleScanDocumentsDoesNotFallbackToConversationViewsWithoutDocumentChunks(t *testing.T) {
	ctx := context.Background()
	mem, err := newLifecycleSelectionSystem(t, true)
	if err != nil {
		t.Fatalf("newLifecycleSelectionSystem() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv", DatasetID: "dataset"}

	tests := map[string]func(context.Context) (memory.LifecycleExecutionReport, error){
		"rebuild": func(ctx context.Context) (memory.LifecycleExecutionReport, error) {
			return mem.Rebuild(ctx, memory.RebuildRequest{Scope: scope, ScanDocuments: true, DryRun: true})
		},
		"reload": func(ctx context.Context) (memory.LifecycleExecutionReport, error) {
			return mem.Reload(ctx, memory.ReloadRequest{Scope: scope, ScanDocuments: true, DryRun: true})
		},
		"reconcile": func(ctx context.Context) (memory.LifecycleExecutionReport, error) {
			return mem.Reconcile(ctx, memory.ReconcileRequest{Scope: scope, ScanDocuments: true, DryRun: true})
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			report, err := run(ctx)
			if err == nil || !errdefs.IsNotAvailable(err) {
				t.Fatalf("%s(ScanDocuments without document_chunks) error = %v, report = %+v, want NotAvailable", name, err, report)
			}
			if report.Status != memory.LifecycleStatusUnsupported || report.JobID != "" {
				t.Fatalf("%s report = %+v, want unsupported without enqueued job", name, report)
			}
			if !report.Operation.ScanDocuments {
				t.Fatalf("%s operation = %+v, want ScanDocuments preserved", name, report.Operation)
			}
			if len(report.Operation.Documents) != 0 {
				t.Fatalf("%s documents = %+v, want no document scan targets", name, report.Operation.Documents)
			}
			if lifecycleStepPresent(report.Steps, "message_index.conversation") || lifecycleStepPresent(report.Steps, "summary_dag.conversation") {
				t.Fatalf("%s steps = %+v, want no message/summary routing", name, report.Steps)
			}
			if !lifecycleStepPresent(report.Steps, "document_chunks.scan") {
				t.Fatalf("%s steps = %+v, want clear document_chunks scan rejection", name, report.Steps)
			}
		})
	}
}

func TestDocumentLifecycleScanDocumentsEmptyPageDoesNotFallbackToConversationViews(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{
			{Kind: memory.SourceDocumentStore, Required: true},
			{Kind: memory.SourceMessageLog, Required: true},
		},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityDocumentChunks, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{
			{Capability: memory.CapabilityDocumentChunks, Namespace: "document_chunks", Required: true},
			{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true},
			{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true},
		},
	}, memory.Deps{
		DocumentStore:   sourcedocument.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/document")),
		ChunkStore:      viewdocument.NewChunkWorkspaceStore(sdkworkspace.Sub(root, "views/document_chunks")),
		DocumentChunker: derivedocument.WholeDocumentChunker{},
		MessageStore:    sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message")),
		SummaryStore:    recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag")),
		Summarizer:      &recordingSummaryLifecycleSummarizer{},
		Index:           index,
		JobStore:        memory.NewMemoryJobStore(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv", DatasetID: "empty-dataset"}

	tests := map[string]func(context.Context) (memory.LifecycleExecutionReport, error){
		"rebuild": func(ctx context.Context) (memory.LifecycleExecutionReport, error) {
			return mem.Rebuild(ctx, memory.RebuildRequest{Scope: scope, ScanDocuments: true, PageSize: 1})
		},
		"reload": func(ctx context.Context) (memory.LifecycleExecutionReport, error) {
			return mem.Reload(ctx, memory.ReloadRequest{Scope: scope, ScanDocuments: true, PageSize: 1})
		},
		"reconcile": func(ctx context.Context) (memory.LifecycleExecutionReport, error) {
			return mem.Reconcile(ctx, memory.ReconcileRequest{Scope: scope, ScanDocuments: true, PageSize: 1})
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			report, err := run(ctx)
			if err != nil {
				t.Fatalf("%s(ScanDocuments empty page) error = %v", name, err)
			}
			if report.Status != memory.LifecycleStatusSkipped || report.JobID != "" {
				t.Fatalf("%s report = %+v, want skipped empty document scan without job", name, report)
			}
			if !report.Operation.ScanDocuments {
				t.Fatalf("%s operation = %+v, want ScanDocuments preserved", name, report.Operation)
			}
			if len(report.Operation.Documents) != 0 {
				t.Fatalf("%s documents = %+v, want empty scan page", name, report.Operation.Documents)
			}
			if len(report.Operation.Capabilities) != 1 || report.Operation.Capabilities[0] != memory.CapabilityDocumentChunks {
				t.Fatalf("%s capabilities = %+v, want only document_chunks", name, report.Operation.Capabilities)
			}
			assertCheckpointBool(t, report.Checkpoint, "document_scan", true)
			assertCheckpointInt(t, report.Checkpoint, "document_scan_scanned", 0)
			if !lifecycleSkippedStepPresent(report.Steps, "document_chunks.scan") {
				t.Fatalf("%s steps = %+v, want skipped document_chunks scan", name, report.Steps)
			}
			if lifecycleStepPresent(report.Steps, "message_index.conversation") || lifecycleStepPresent(report.Steps, "summary_dag.conversation") {
				t.Fatalf("%s steps = %+v, want no message/summary routing", name, report.Steps)
			}
		})
	}
}

func TestLifecycleRebuildReloadMessageIndexConversation(t *testing.T) {
	ctx := context.Background()
	for _, action := range []string{"rebuild", "reload"} {
		t.Run(action, func(t *testing.T) {
			root := sdkworkspace.NewMemWorkspace()
			index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
			if err != nil {
				t.Fatalf("retrieval workspace: %v", err)
			}
			t.Cleanup(func() { _ = index.Close() })
			msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
			mem, err := memory.New(memory.Spec{
				Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
				Capabilities: []memory.CapabilitySpec{
					{Capability: memory.CapabilityRecentWindow, Required: true},
					{Capability: memory.CapabilityMessageIndex, Required: true},
				},
				Projections: []memory.ProjectionSpec{{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true}},
				ReadStages: []memory.StageSpec{
					{Name: "load_recent_messages"},
					{Name: "retrieve_messages"},
					{Name: "pack_context"},
				},
			}, memory.Deps{
				MessageStore: msgStore,
				Index:        index,
				JobStore:     memory.NewMemoryJobStore(),
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv", DatasetID: "dataset"}
			if _, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
				ConversationID: scope.ConversationID,
				Messages: []sourcemessage.Message{{
					ID:       "dia-1",
					Message:  model.NewTextMessage(model.RoleUser, "Ada hid the blue notebook beside the lamp."),
					Metadata: map[string]any{"speaker": "Ada"},
				}},
			}); err != nil {
				t.Fatalf("Append() error = %v", err)
			}

			report, err := runLifecycleRequest(ctx, mem, action, scope, []memory.Capability{memory.CapabilityMessageIndex})
			if err != nil {
				t.Fatalf("%s request error = %v", action, err)
			}
			if report.Status != memory.LifecycleStatusEnqueued || report.JobID == "" {
				t.Fatalf("%s report = %+v, want enqueued job", action, report)
			}
			result, err := mem.RunOnce(ctx)
			if err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			if !result.Completed {
				t.Fatalf("RunOnce() result = %+v, want completed", result)
			}

			pack, err := mem.PackContext(ctx, memory.ContextRequest{
				Scope: scope,
				Query: "blue notebook lamp",
				TopK:  3,
			})
			if err != nil {
				t.Fatalf("PackContext() error = %v", err)
			}
			if len(pack.MessageHits) == 0 || pack.MessageHits[0].Message.ID != "dia-1" {
				t.Fatalf("MessageHits = %+v, want rebuilt searchable source message", pack.MessageHits)
			}
		})
	}
}

func TestMessageIndexLifecycleCleanupKeepsOtherAgentProjection(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true}},
	}, memory.Deps{
		MessageStore: msgStore,
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent-a", ConversationID: "conv"}
	if _, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-1",
			Message: model.NewTextMessage(model.RoleUser, "Ada hid the blue notebook beside the lamp."),
		}},
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	namespace, err := projectors.ScopedNamespace("message_index", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace() error = %v", err)
	}
	scopeB := scope
	scopeB.AgentID = "agent-b"
	agentBRecord := sourceMessageRecord(t, scopeB, sourcemessage.Message{
		ID:             "dia-1",
		ConversationID: scope.ConversationID,
		Message:        model.NewTextMessage(model.RoleUser, "Agent B projection must survive agent A rebuild."),
	})
	writer, err := indexed.NewWriter(index, indexed.Binding{Namespace: namespace})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Upsert(ctx, []indexed.Record{agentBRecord}); err != nil {
		t.Fatalf("seed agent-b projection error = %v", err)
	}

	if _, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityMessageIndex},
	}); err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if result, err := mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want completed", result, err)
	}
	agentBDoc, ok, err := index.Get(ctx, namespace, agentBRecord.ID)
	if err != nil || !ok {
		t.Fatalf("other agent projection after cleanup ok = %v err = %v, want retained", ok, err)
	}
	if got, want := agentBDoc.Metadata[projectors.MetadataAgentIDKey], "agent-b"; got != want {
		t.Fatalf("other agent projection metadata[%q] = %v, want %q", projectors.MetadataAgentIDKey, got, want)
	}
	if !strings.Contains(agentBDoc.Content, "Agent B projection") {
		t.Fatalf("other agent projection content = %q, want agent-b content retained", agentBDoc.Content)
	}
}

func TestMessageIndexLifecycleCleanupEmptyAgentDatasetKeepsScopedProjectionPartitions(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true}},
	}, memory.Deps{
		MessageStore: msgStore,
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	messages, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-current",
			Message: model.NewTextMessage(model.RoleUser, "Ada hid the blue notebook beside the lamp."),
		}},
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	namespace, err := projectors.ScopedNamespace("message_index", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace() error = %v", err)
	}
	currentRecord := sourceMessageRecord(t, scope, messages[0])
	staleEmptyRecord := sourceMessageRecord(t, scope, sourcemessage.Message{
		ID:             "dia-stale",
		ConversationID: scope.ConversationID,
		Message:        model.NewTextMessage(model.RoleUser, "stale empty-agent projection"),
	})
	agentAScope := scope
	agentAScope.AgentID = "agent-a"
	agentARecord := sourceMessageRecord(t, agentAScope, sourcemessage.Message{
		ID:             "dia-stale",
		ConversationID: scope.ConversationID,
		Message:        model.NewTextMessage(model.RoleUser, "agent-a projection must survive empty-agent cleanup"),
	})
	agentBScope := scope
	agentBScope.AgentID = "agent-b"
	agentBRecord := sourceMessageRecord(t, agentBScope, sourcemessage.Message{
		ID:             "dia-stale",
		ConversationID: scope.ConversationID,
		Message:        model.NewTextMessage(model.RoleUser, "agent-b projection must survive empty-agent cleanup"),
	})
	datasetScope := scope
	datasetScope.DatasetID = "dataset-a"
	datasetRecord := sourceMessageRecord(t, datasetScope, sourcemessage.Message{
		ID:             "dia-stale",
		ConversationID: scope.ConversationID,
		Message:        model.NewTextMessage(model.RoleUser, "dataset projection must survive empty-dataset cleanup"),
	})
	missingPartitionDoc := retrieval.Doc{
		ID:      "legacy-missing-agent-dataset",
		Content: "legacy projection with missing agent and dataset metadata",
		Metadata: map[string]any{
			projectors.MetadataViewKindKey:       "message_index",
			projectors.MetadataRecordTypeKey:     projectors.RecordTypeSourceMessage,
			projectors.MetadataConversationIDKey: scope.ConversationID,
		},
	}
	writer, err := indexed.NewWriter(index, indexed.Binding{Namespace: namespace})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Upsert(ctx, []indexed.Record{staleEmptyRecord, agentARecord, agentBRecord, datasetRecord}); err != nil {
		t.Fatalf("seed projections error = %v", err)
	}
	if err := index.Upsert(ctx, namespace, []retrieval.Doc{missingPartitionDoc}); err != nil {
		t.Fatalf("seed missing partition projection error = %v", err)
	}

	if _, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityMessageIndex},
	}); err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if result, err := mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want completed", result, err)
	}
	for label, id := range map[string]string{
		"stale empty partition":   staleEmptyRecord.ID,
		"missing partition stale": missingPartitionDoc.ID,
	} {
		if ok, err := projectionDocExists(ctx, index, namespace, id); err != nil || ok {
			t.Fatalf("%s projection exists = %v err = %v, want deleted", label, ok, err)
		}
	}
	for label, id := range map[string]string{
		"current": currentRecord.ID,
		"agent-a": agentARecord.ID,
		"agent-b": agentBRecord.ID,
		"dataset": datasetRecord.ID,
	} {
		if ok, err := projectionDocExists(ctx, index, namespace, id); err != nil || !ok {
			t.Fatalf("%s projection exists = %v err = %v, want retained", label, ok, err)
		}
	}
}

func TestMessageIndexLifecycleCleanupFallsBackWhenDeleteByFilterUnavailable(t *testing.T) {
	ctx := context.Background()
	for _, action := range []string{"rebuild", "reload"} {
		t.Run(action, func(t *testing.T) {
			root := sdkworkspace.NewMemWorkspace()
			baseIndex, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
			if err != nil {
				t.Fatalf("retrieval workspace: %v", err)
			}
			t.Cleanup(func() { _ = baseIndex.Close() })
			index := &deleteByFilterUnavailableLifecycleIndex{Index: baseIndex}
			msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
			reportStore := memory.NewMemoryReportStore()
			mem, err := memory.New(memory.Spec{
				Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
				Capabilities: []memory.CapabilitySpec{
					{Capability: memory.CapabilityRecentWindow, Required: true},
					{Capability: memory.CapabilityMessageIndex, Required: true},
				},
				Projections: []memory.ProjectionSpec{{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true}},
			}, memory.Deps{
				MessageStore: msgStore,
				Index:        index,
				JobStore:     memory.NewMemoryJobStore(),
				ReportStore:  reportStore,
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			scope := memory.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent-a", ConversationID: "conv"}
			messages, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
				ConversationID: scope.ConversationID,
				Messages: []sourcemessage.Message{{
					ID:      "dia-current",
					Message: model.NewTextMessage(model.RoleUser, "Ada hid the blue notebook beside the lamp."),
				}},
			})
			if err != nil {
				t.Fatalf("Append() error = %v", err)
			}
			namespace, err := projectors.ScopedNamespace("message_index", scope)
			if err != nil {
				t.Fatalf("ScopedNamespace() error = %v", err)
			}
			currentRecord := sourceMessageRecord(t, scope, messages[0])
			staleRecord := sourceMessageRecord(t, scope, sourcemessage.Message{
				ID:             "dia-stale",
				ConversationID: scope.ConversationID,
				Message:        model.NewTextMessage(model.RoleUser, "stale message projection"),
			})
			otherAgentScope := scope
			otherAgentScope.AgentID = "agent-b"
			otherAgentRecord := sourceMessageRecord(t, otherAgentScope, sourcemessage.Message{
				ID:             "dia-stale",
				ConversationID: scope.ConversationID,
				Message:        model.NewTextMessage(model.RoleUser, "other agent projection must survive"),
			})
			otherConversationScope := scope
			otherConversationScope.ConversationID = "other-conv"
			otherConversationRecord := sourceMessageRecord(t, otherConversationScope, sourcemessage.Message{
				ID:             "dia-stale",
				ConversationID: otherConversationScope.ConversationID,
				Message:        model.NewTextMessage(model.RoleUser, "other conversation projection must survive"),
			})
			writer, err := indexed.NewWriter(index, indexed.Binding{Namespace: namespace})
			if err != nil {
				t.Fatalf("NewWriter() error = %v", err)
			}
			if err := writer.Upsert(ctx, []indexed.Record{staleRecord, otherAgentRecord, otherConversationRecord}); err != nil {
				t.Fatalf("seed projections error = %v", err)
			}

			report, err := runLifecycleRequest(ctx, mem, action, scope, []memory.Capability{memory.CapabilityMessageIndex})
			if err != nil {
				t.Fatalf("%s request error = %v", action, err)
			}
			if result, err := mem.RunOnce(ctx); err != nil || !result.Completed {
				t.Fatalf("RunOnce() result = %+v error = %v, want completed", result, err)
			}
			stored, ok, err := reportStore.GetLifecycleReport(ctx, report.TraceID)
			if err != nil || !ok {
				t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
			}
			step := requireLifecycleStep(t, stored.Steps, "message_index.conversation")
			if got, want := step.Details["cleanup_mode"], "list_delete"; got != want {
				t.Fatalf("cleanup_mode = %v, want %q; step=%+v", got, want, step)
			}
			if got, want := stored.Checkpoint["cleanup_mode"], "list_delete"; got != want {
				t.Fatalf("checkpoint cleanup_mode = %v, want %q; checkpoint=%+v", got, want, stored.Checkpoint)
			}
			if ok, err := projectionDocExists(ctx, baseIndex, namespace, staleRecord.ID); err != nil || ok {
				t.Fatalf("stale projection exists = %v err = %v, want deleted", ok, err)
			}
			for label, id := range map[string]string{
				"current":            currentRecord.ID,
				"other agent":        otherAgentRecord.ID,
				"other conversation": otherConversationRecord.ID,
			} {
				if ok, err := projectionDocExists(ctx, baseIndex, namespace, id); err != nil || !ok {
					t.Fatalf("%s projection exists = %v err = %v, want retained", label, ok, err)
				}
			}
		})
	}
}

func TestMessageIndexLifecycleCleanupFallbackEmptyAgentDatasetKeepsScopedProjectionPartitions(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	baseIndex, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = baseIndex.Close() })
	index := &deleteByFilterUnavailableLifecycleIndex{Index: baseIndex}
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	reportStore := memory.NewMemoryReportStore()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true}},
	}, memory.Deps{
		MessageStore: msgStore,
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
		ReportStore:  reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	messages, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-current",
			Message: model.NewTextMessage(model.RoleUser, "Ada hid the blue notebook beside the lamp."),
		}},
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	namespace, err := projectors.ScopedNamespace("message_index", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace() error = %v", err)
	}
	currentRecord := sourceMessageRecord(t, scope, messages[0])
	staleEmptyRecord := sourceMessageRecord(t, scope, sourcemessage.Message{
		ID:             "dia-stale",
		ConversationID: scope.ConversationID,
		Message:        model.NewTextMessage(model.RoleUser, "stale empty-agent projection"),
	})
	agentAScope := scope
	agentAScope.AgentID = "agent-a"
	agentARecord := sourceMessageRecord(t, agentAScope, sourcemessage.Message{
		ID:             "dia-stale",
		ConversationID: scope.ConversationID,
		Message:        model.NewTextMessage(model.RoleUser, "agent-a projection must survive empty-agent cleanup"),
	})
	agentBScope := scope
	agentBScope.AgentID = "agent-b"
	agentBRecord := sourceMessageRecord(t, agentBScope, sourcemessage.Message{
		ID:             "dia-stale",
		ConversationID: scope.ConversationID,
		Message:        model.NewTextMessage(model.RoleUser, "agent-b projection must survive empty-agent cleanup"),
	})
	datasetScope := scope
	datasetScope.DatasetID = "dataset-a"
	datasetRecord := sourceMessageRecord(t, datasetScope, sourcemessage.Message{
		ID:             "dia-stale",
		ConversationID: scope.ConversationID,
		Message:        model.NewTextMessage(model.RoleUser, "dataset projection must survive empty-dataset cleanup"),
	})
	missingPartitionDoc := retrieval.Doc{
		ID:      "legacy-missing-agent-dataset",
		Content: "legacy projection with missing agent and dataset metadata",
		Metadata: map[string]any{
			projectors.MetadataViewKindKey:       "message_index",
			projectors.MetadataRecordTypeKey:     projectors.RecordTypeSourceMessage,
			projectors.MetadataConversationIDKey: scope.ConversationID,
		},
	}
	writer, err := indexed.NewWriter(index, indexed.Binding{Namespace: namespace})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Upsert(ctx, []indexed.Record{staleEmptyRecord, agentARecord, agentBRecord, datasetRecord}); err != nil {
		t.Fatalf("seed projections error = %v", err)
	}
	if err := baseIndex.Upsert(ctx, namespace, []retrieval.Doc{missingPartitionDoc}); err != nil {
		t.Fatalf("seed missing partition projection error = %v", err)
	}

	report, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityMessageIndex},
	})
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if result, err := mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want completed", result, err)
	}
	stored, ok, err := reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
	}
	step := requireLifecycleStep(t, stored.Steps, "message_index.conversation")
	if got, want := step.Details["cleanup_mode"], "list_delete"; got != want {
		t.Fatalf("cleanup_mode = %v, want %q; step=%+v", got, want, step)
	}
	for label, id := range map[string]string{
		"stale empty partition":   staleEmptyRecord.ID,
		"missing partition stale": missingPartitionDoc.ID,
	} {
		if ok, err := projectionDocExists(ctx, baseIndex, namespace, id); err != nil || ok {
			t.Fatalf("%s projection exists = %v err = %v, want deleted", label, ok, err)
		}
	}
	for label, id := range map[string]string{
		"current": currentRecord.ID,
		"agent-a": agentARecord.ID,
		"agent-b": agentBRecord.ID,
		"dataset": datasetRecord.ID,
	} {
		if ok, err := projectionDocExists(ctx, baseIndex, namespace, id); err != nil || !ok {
			t.Fatalf("%s projection exists = %v err = %v, want retained", label, ok, err)
		}
	}
}

func TestLifecycleRebuildReloadSummaryDAGConversation(t *testing.T) {
	ctx := context.Background()
	for _, action := range []string{"rebuild", "reload"} {
		t.Run(action, func(t *testing.T) {
			root := sdkworkspace.NewMemWorkspace()
			index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
			if err != nil {
				t.Fatalf("retrieval workspace: %v", err)
			}
			t.Cleanup(func() { _ = index.Close() })
			msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
			summaryStore := recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag"))
			summarizer := &recordingSummaryLifecycleSummarizer{}
			mem, err := memory.New(memory.Spec{
				Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
				Capabilities: []memory.CapabilitySpec{
					{Capability: memory.CapabilityRecentWindow, Required: true},
					{Capability: memory.CapabilitySummaryDAG, Required: true},
				},
				Projections: []memory.ProjectionSpec{{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true}},
				ReadStages: []memory.StageSpec{
					{Name: "load_recent_messages"},
					{Name: "retrieve_summaries"},
					{Name: "pack_context"},
				},
			}, memory.Deps{
				MessageStore: msgStore,
				SummaryStore: summaryStore,
				Summarizer:   summarizer,
				Index:        index,
				JobStore:     memory.NewMemoryJobStore(),
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
			if _, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
				ConversationID: scope.ConversationID,
				Messages: []sourcemessage.Message{{
					ID:      "dia-1",
					Message: model.NewTextMessage(model.RoleUser, "Ada found a brass lantern."),
				}},
			}); err != nil {
				t.Fatalf("Append() error = %v", err)
			}

			report, err := runLifecycleRequest(ctx, mem, action, scope, []memory.Capability{memory.CapabilitySummaryDAG})
			if err != nil {
				t.Fatalf("%s request error = %v", action, err)
			}
			if report.Status != memory.LifecycleStatusEnqueued || report.JobID == "" {
				t.Fatalf("%s report = %+v, want enqueued job", action, report)
			}
			result, err := mem.RunOnce(ctx)
			if err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			if !result.Completed {
				t.Fatalf("RunOnce() result = %+v, want completed", result)
			}
			if summarizer.calls != 1 {
				t.Fatalf("summarizer calls = %d, want 1", summarizer.calls)
			}
			nodes, err := summaryStore.ListNodes(ctx, scope, recent.ListOptions{})
			if err != nil {
				t.Fatalf("ListNodes() error = %v", err)
			}
			if len(nodes) != 1 || !strings.Contains(nodes[0].Summary, "lantern") {
				t.Fatalf("Summary nodes = %+v, want persisted lantern summary", nodes)
			}
			pack, err := mem.PackContext(ctx, memory.ContextRequest{
				Scope: scope,
				Query: "lantern",
				TopK:  3,
			})
			if err != nil {
				t.Fatalf("PackContext() error = %v", err)
			}
			if len(pack.SummaryHits) == 0 || pack.SummaryHits[0].Node.ID != nodes[0].ID {
				t.Fatalf("SummaryHits = %+v, want rebuilt searchable summary", pack.SummaryHits)
			}
		})
	}
}

func TestSummaryDAGLifecycleRebuildFailureKeepsExistingSummaryAndProjection(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	summaryStore := recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag"))
	summarizer := &failingSummaryLifecycleSummarizer{err: errors.New("summarizer boom")}
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true}},
	}, memory.Deps{
		MessageStore: msgStore,
		SummaryStore: summaryStore,
		Summarizer:   summarizer,
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	if _, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-1",
			Message: model.NewTextMessage(model.RoleUser, "Ada found a brass lantern."),
		}},
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	oldNode := summaryLifecycleNode(scope, "summary-old", "old summary that must survive failure")
	if _, err := summaryStore.PutNode(ctx, oldNode); err != nil {
		t.Fatalf("PutNode(old) error = %v", err)
	}
	namespace, err := projectors.ScopedNamespace("summary_dag", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace() error = %v", err)
	}
	record, err := projectors.SummaryNode(oldNode)
	if err != nil {
		t.Fatalf("SummaryNode(old) error = %v", err)
	}
	writer, err := indexed.NewWriter(index, indexed.Binding{Namespace: namespace})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Upsert(ctx, []indexed.Record{record}); err != nil {
		t.Fatalf("seed old summary projection error = %v", err)
	}

	if _, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
	}); err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if result, err := mem.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), summarizer.err.Error()) || result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want summarizer failure", result, err)
	}
	nodes, err := summaryStore.ListNodes(ctx, scope, recent.ListOptions{})
	if err != nil {
		t.Fatalf("ListNodes() error = %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != oldNode.ID {
		t.Fatalf("Summary nodes after failed rebuild = %+v, want old node retained", nodes)
	}
	if _, ok, err := index.Get(ctx, namespace, record.ID); err != nil || !ok {
		t.Fatalf("old summary projection after failed rebuild ok = %v err = %v, want retained", ok, err)
	}
}

func TestSummaryDAGLifecycleCleanupKeepsOtherAgentProjection(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	summaryStore := recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag"))
	reportStore := memory.NewMemoryReportStore()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true}},
		ReadStages: []memory.StageSpec{
			{Name: "retrieve_summaries"},
			{Name: "pack_context"},
		},
	}, memory.Deps{
		MessageStore: msgStore,
		SummaryStore: summaryStore,
		Summarizer:   &recordingSummaryLifecycleSummarizer{},
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
		ReportStore:  reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent-a", ConversationID: "conv"}
	if _, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-1",
			Message: model.NewTextMessage(model.RoleUser, "Ada found a brass lantern."),
		}},
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	namespace, err := projectors.ScopedNamespace("summary_dag", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace() error = %v", err)
	}
	scopeB := scope
	scopeB.AgentID = "agent-b"
	staleNode := summaryLifecycleNode(scope, "summary-stale", "stale Agent A summary must be deleted.")
	if _, err := summaryStore.PutNode(ctx, staleNode); err != nil {
		t.Fatalf("PutNode(stale) error = %v", err)
	}
	agentBNode := summaryLifecycleNode(scopeB, "summary-conv", "Agent B summary must survive agent A rebuild.")
	if _, err := summaryStore.PutNode(ctx, agentBNode); err != nil {
		t.Fatalf("PutNode(agent-b) error = %v", err)
	}
	otherConversationScope := scope
	otherConversationScope.ConversationID = "conv-other"
	otherConversationNode := summaryLifecycleNode(otherConversationScope, "summary-stale", "other conversation summary must survive.")
	if _, err := summaryStore.PutNode(ctx, otherConversationNode); err != nil {
		t.Fatalf("PutNode(other conversation) error = %v", err)
	}
	staleRecord, err := projectors.SummaryNode(staleNode)
	if err != nil {
		t.Fatalf("SummaryNode(stale) error = %v", err)
	}
	agentBRecord, err := projectors.SummaryNode(agentBNode)
	if err != nil {
		t.Fatalf("SummaryNode(agent-b) error = %v", err)
	}
	writer, err := indexed.NewWriter(index, indexed.Binding{Namespace: namespace})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Upsert(ctx, []indexed.Record{staleRecord, agentBRecord}); err != nil {
		t.Fatalf("seed summary projections error = %v", err)
	}

	report, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
	})
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if result, err := mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want completed", result, err)
	}
	stored, ok, err := reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
	}
	step := requireLifecycleStep(t, stored.Steps, "summary_dag.conversation")
	if got, want := step.Details["stale_store_cleanup"], "delete_node"; got != want {
		t.Fatalf("stale_store_cleanup = %v, want %q; step=%+v", got, want, step)
	}
	assertStepDetailInt(t, step, "canonical_stale_candidates", 1)
	assertStepDetailInt(t, step, "canonical_stale_deleted", 1)
	assertCheckpointString(t, stored.Checkpoint, "canonical_cleanup_mode", "delete_node")
	assertCheckpointInt(t, stored.Checkpoint, "canonical_stale_deleted", 1)
	if _, ok, err := summaryStore.GetNode(ctx, scope, staleNode.ID); err != nil || ok {
		t.Fatalf("stale canonical summary ok = %v err = %v, want deleted", ok, err)
	}
	currentNode, ok, err := summaryStore.GetNode(ctx, scope, recent.NodeID("summary-"+scope.ConversationID))
	if err != nil || !ok {
		t.Fatalf("current canonical summary ok = %v err = %v, want retained", ok, err)
	}
	currentRecord, err := projectors.SummaryNode(currentNode)
	if err != nil {
		t.Fatalf("SummaryNode(current) error = %v", err)
	}
	if ok, err := projectionDocExists(ctx, index, namespace, staleRecord.ID); err != nil || ok {
		t.Fatalf("stale summary projection after cleanup ok = %v err = %v, want deleted", ok, err)
	}
	if ok, err := projectionDocExists(ctx, index, namespace, currentRecord.ID); err != nil || !ok {
		t.Fatalf("current summary projection after cleanup ok = %v err = %v, want retained", ok, err)
	}
	agentBDoc, ok, err := index.Get(ctx, namespace, agentBRecord.ID)
	if err != nil || !ok {
		t.Fatalf("other agent summary projection after cleanup ok = %v err = %v, want retained", ok, err)
	}
	if got, want := agentBDoc.Metadata[projectors.MetadataAgentIDKey], "agent-b"; got != want {
		t.Fatalf("other agent summary metadata[%q] = %v, want %q", projectors.MetadataAgentIDKey, got, want)
	}
	if !strings.Contains(agentBDoc.Content, "Agent B summary") {
		t.Fatalf("other agent summary content = %q, want agent-b content retained", agentBDoc.Content)
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: scopeB,
		Query: "Agent B summary",
		TopK:  3,
	})
	if err != nil {
		t.Fatalf("PackContext(agent-b) error = %v", err)
	}
	if len(pack.SummaryHits) == 0 {
		t.Fatalf("PackContext(agent-b) SummaryHits = %+v, want agent-b summary", pack.SummaryHits)
	}
	gotNode := pack.SummaryHits[0].Node
	if gotNode.Scope.AgentID != "agent-b" || gotNode.Summary != agentBNode.Summary {
		t.Fatalf("PackContext(agent-b) hydrated node = %+v, want agent-b summary %q", gotNode, agentBNode.Summary)
	}
	if got, ok, err := summaryStore.GetNode(ctx, scopeB, agentBNode.ID); err != nil || !ok || got.Summary != agentBNode.Summary {
		t.Fatalf("agent-b canonical summary after cleanup = %+v ok %v err %v, want retained", got, ok, err)
	}
	if got, ok, err := summaryStore.GetNode(ctx, otherConversationScope, otherConversationNode.ID); err != nil || !ok || got.Summary != otherConversationNode.Summary {
		t.Fatalf("other conversation canonical summary after cleanup = %+v ok %v err %v, want retained", got, ok, err)
	}
}

func TestSummaryDAGLifecycleProjectionCleanupFallsBackWhenDeleteByFilterUnavailable(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	baseIndex, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = baseIndex.Close() })
	index := &deleteByFilterUnavailableLifecycleIndex{Index: baseIndex}
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	summaryStore := recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag"))
	reportStore := memory.NewMemoryReportStore()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true}},
		ReadStages: []memory.StageSpec{
			{Name: "retrieve_summaries"},
			{Name: "pack_context"},
		},
	}, memory.Deps{
		MessageStore: msgStore,
		SummaryStore: summaryStore,
		Summarizer:   &recordingSummaryLifecycleSummarizer{},
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
		ReportStore:  reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent", ConversationID: "conv"}
	if _, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-1",
			Message: model.NewTextMessage(model.RoleUser, "Ada found a brass lantern."),
		}},
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	namespace, err := projectors.ScopedNamespace("summary_dag", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace() error = %v", err)
	}
	staleNode := summaryLifecycleNode(scope, "summary-stale", "stale summary projection")
	if _, err := summaryStore.PutNode(ctx, staleNode); err != nil {
		t.Fatalf("PutNode(stale) error = %v", err)
	}
	staleRecord, err := projectors.SummaryNode(staleNode)
	if err != nil {
		t.Fatalf("SummaryNode(stale) error = %v", err)
	}
	writer, err := indexed.NewWriter(index, indexed.Binding{Namespace: namespace})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Upsert(ctx, []indexed.Record{staleRecord}); err != nil {
		t.Fatalf("seed stale summary projection error = %v", err)
	}

	report, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
	})
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if result, err := mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want completed", result, err)
	}
	stored, ok, err := reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
	}
	step := requireLifecycleStep(t, stored.Steps, "summary_dag.conversation")
	if got, want := step.Details["cleanup_mode"], "list_delete"; got != want {
		t.Fatalf("cleanup_mode = %v, want %q; step=%+v", got, want, step)
	}
	if got, want := step.Details["stale_store_cleanup"], "delete_node"; got != want {
		t.Fatalf("stale_store_cleanup = %v, want %q; step=%+v", got, want, step)
	}
	assertStepDetailInt(t, step, "canonical_stale_candidates", 1)
	assertStepDetailInt(t, step, "canonical_stale_deleted", 1)
	assertCheckpointString(t, stored.Checkpoint, "canonical_cleanup_mode", "delete_node")
	if ok, err := projectionDocExists(ctx, baseIndex, namespace, staleRecord.ID); err != nil || ok {
		t.Fatalf("stale summary projection exists = %v err = %v, want deleted", ok, err)
	}
	if _, ok, err := summaryStore.GetNode(ctx, scope, staleNode.ID); err != nil || ok {
		t.Fatalf("stale canonical summary exists = %v err = %v, want deleted", ok, err)
	}
	nodes, err := summaryStore.ListNodes(ctx, scope, recent.ListOptions{})
	if err != nil {
		t.Fatalf("ListNodes() error = %v", err)
	}
	var currentProjectionID string
	for _, node := range nodes {
		if node.ID == recent.NodeID("summary-"+scope.ConversationID) {
			record, err := projectors.SummaryNode(node)
			if err != nil {
				t.Fatalf("SummaryNode(current) error = %v", err)
			}
			currentProjectionID = record.ID
		}
	}
	if currentProjectionID == "" {
		t.Fatalf("Summary nodes = %+v, want rebuilt current node", nodes)
	}
	if ok, err := projectionDocExists(ctx, baseIndex, namespace, currentProjectionID); err != nil || !ok {
		t.Fatalf("current summary projection exists = %v err = %v, want retained", ok, err)
	}
}

func TestSummaryDAGLifecycleDeleteNodeFailureReportsFailedAndKeepsProjections(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	baseSummaryStore := recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag"))
	summaryStore := &failingSummaryDeleteStore{
		SummaryStore: baseSummaryStore,
		err:          errors.New("delete node boom"),
	}
	reportStore := memory.NewMemoryReportStore()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true}},
	}, memory.Deps{
		MessageStore: msgStore,
		SummaryStore: summaryStore,
		Summarizer:   &recordingSummaryLifecycleSummarizer{},
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
		ReportStore:  reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent", ConversationID: "conv"}
	if _, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-1",
			Message: model.NewTextMessage(model.RoleUser, "Ada found a brass lantern."),
		}},
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	staleNode := summaryLifecycleNode(scope, "summary-stale", "stale summary should remain after delete failure")
	if _, err := summaryStore.PutNode(ctx, staleNode); err != nil {
		t.Fatalf("PutNode(stale) error = %v", err)
	}
	namespace, err := projectors.ScopedNamespace("summary_dag", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace() error = %v", err)
	}
	staleRecord, err := projectors.SummaryNode(staleNode)
	if err != nil {
		t.Fatalf("SummaryNode(stale) error = %v", err)
	}
	writer, err := indexed.NewWriter(index, indexed.Binding{Namespace: namespace})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Upsert(ctx, []indexed.Record{staleRecord}); err != nil {
		t.Fatalf("seed stale summary projection error = %v", err)
	}

	report, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
	})
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if result, err := mem.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), summaryStore.err.Error()) || result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want delete failure", result, err)
	}
	stored, ok, err := reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
	}
	if stored.Status != memory.LifecycleStatusFailed {
		t.Fatalf("stored status = %q, want failed; report=%+v", stored.Status, stored)
	}
	step := requireLifecycleStep(t, stored.Steps, "summary_dag.conversation")
	if step.Status != memory.LifecycleStatusFailed {
		t.Fatalf("step status = %q, want failed; step=%+v", step.Status, step)
	}
	if got, want := step.Details["stale_store_cleanup"], "failed"; got != want {
		t.Fatalf("stale_store_cleanup = %v, want %q; step=%+v", got, want, step)
	}
	assertStepDetailInt(t, step, "canonical_stale_candidates", 1)
	assertStepDetailInt(t, step, "canonical_stale_deleted", 0)
	assertCheckpointString(t, stored.Checkpoint, "canonical_cleanup_mode", "failed")
	if _, ok, err := summaryStore.GetNode(ctx, scope, staleNode.ID); err != nil || !ok {
		t.Fatalf("stale canonical summary ok = %v err = %v, want retained", ok, err)
	}
	currentNode, ok, err := summaryStore.GetNode(ctx, scope, recent.NodeID("summary-"+scope.ConversationID))
	if err != nil || !ok {
		t.Fatalf("current canonical summary ok = %v err = %v, want retained", ok, err)
	}
	currentRecord, err := projectors.SummaryNode(currentNode)
	if err != nil {
		t.Fatalf("SummaryNode(current) error = %v", err)
	}
	if ok, err := projectionDocExists(ctx, index, namespace, staleRecord.ID); err != nil || !ok {
		t.Fatalf("stale summary projection after delete failure ok = %v err = %v, want retained", ok, err)
	}
	if ok, err := projectionDocExists(ctx, index, namespace, currentRecord.ID); err != nil || !ok {
		t.Fatalf("current summary projection after delete failure ok = %v err = %v, want retained", ok, err)
	}
}

func TestSummaryDAGLifecycleCanonicalCleanupDegradesWhenDeleteNodeUnsupported(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	baseSummaryStore := recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag"))
	summaryStore := summaryStoreWithoutTargetDelete{SummaryStore: baseSummaryStore}
	reportStore := memory.NewMemoryReportStore()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true}},
	}, memory.Deps{
		MessageStore: msgStore,
		SummaryStore: summaryStore,
		Summarizer:   &recordingSummaryLifecycleSummarizer{},
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
		ReportStore:  reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent", ConversationID: "conv"}
	if _, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-1",
			Message: model.NewTextMessage(model.RoleUser, "Ada found a brass lantern."),
		}},
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	staleNode := summaryLifecycleNode(scope, "summary-stale", "stale summary remains when targeted delete is unsupported")
	if _, err := summaryStore.PutNode(ctx, staleNode); err != nil {
		t.Fatalf("PutNode(stale) error = %v", err)
	}

	report, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
	})
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if result, err := mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want completed degraded cleanup", result, err)
	}
	stored, ok, err := reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
	}
	step := requireLifecycleStep(t, stored.Steps, "summary_dag.conversation")
	if got, want := step.Details["stale_store_cleanup"], "skipped_degraded"; got != want {
		t.Fatalf("stale_store_cleanup = %v, want %q; step=%+v", got, want, step)
	}
	if got, want := step.Details["canonical_cleanup_mode"], "skipped_degraded"; got != want {
		t.Fatalf("canonical_cleanup_mode = %v, want %q; step=%+v", got, want, step)
	}
	assertStepDetailInt(t, step, "canonical_stale_candidates", 1)
	assertStepDetailInt(t, step, "canonical_stale_deleted", 0)
	assertCheckpointString(t, stored.Checkpoint, "canonical_cleanup_mode", "skipped_degraded")
	if _, ok, err := summaryStore.GetNode(ctx, scope, staleNode.ID); err != nil || !ok {
		t.Fatalf("stale canonical summary ok = %v err = %v, want retained", ok, err)
	}
	if _, ok, err := summaryStore.GetNode(ctx, scope, recent.NodeID("summary-"+scope.ConversationID)); err != nil || !ok {
		t.Fatalf("current canonical summary ok = %v err = %v, want retained", ok, err)
	}
}

func TestMessageIndexLifecycleCleanupDegradesWhenListDeleteUnsupported(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	baseIndex, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = baseIndex.Close() })
	index := &deleteByFilterUnavailableLifecycleIndex{
		Index:     baseIndex,
		listErr:   errdefs.NotAvailablef("test: list unsupported"),
		deleteErr: errdefs.NotAvailablef("test: delete unsupported"),
	}
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	reportStore := memory.NewMemoryReportStore()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true}},
	}, memory.Deps{
		MessageStore: msgStore,
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
		ReportStore:  reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent", ConversationID: "conv"}
	messages, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-current",
			Message: model.NewTextMessage(model.RoleUser, "Ada hid the blue notebook beside the lamp."),
		}},
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	namespace, err := projectors.ScopedNamespace("message_index", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace() error = %v", err)
	}
	currentRecord := sourceMessageRecord(t, scope, messages[0])
	staleRecord := sourceMessageRecord(t, scope, sourcemessage.Message{
		ID:             "dia-stale",
		ConversationID: scope.ConversationID,
		Message:        model.NewTextMessage(model.RoleUser, "stale message projection"),
	})
	writer, err := indexed.NewWriter(baseIndex, indexed.Binding{Namespace: namespace})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Upsert(ctx, []indexed.Record{staleRecord}); err != nil {
		t.Fatalf("seed stale projection error = %v", err)
	}

	report, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityMessageIndex},
	})
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if result, err := mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want degraded completion", result, err)
	}
	stored, ok, err := reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
	}
	step := requireLifecycleStep(t, stored.Steps, "message_index.conversation")
	if got, want := step.Details["cleanup_mode"], "skipped_degraded"; got != want {
		t.Fatalf("cleanup_mode = %v, want %q; step=%+v", got, want, step)
	}
	if got, want := stored.Checkpoint["cleanup_mode"], "skipped_degraded"; got != want {
		t.Fatalf("checkpoint cleanup_mode = %v, want %q; checkpoint=%+v", got, want, stored.Checkpoint)
	}
	for label, id := range map[string]string{
		"current": currentRecord.ID,
		"stale":   staleRecord.ID,
	} {
		if ok, err := projectionDocExists(ctx, baseIndex, namespace, id); err != nil || !ok {
			t.Fatalf("%s projection exists = %v err = %v, want retained", label, ok, err)
		}
	}
}

func TestMessageIndexLifecycleCleanupPlainUnsupportedTextListErrorFails(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	baseIndex, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = baseIndex.Close() })
	listErr := errors.New("test: backend not supported while listing projections")
	index := &deleteByFilterUnavailableLifecycleIndex{
		Index:   baseIndex,
		listErr: listErr,
	}
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	reportStore := memory.NewMemoryReportStore()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true}},
	}, memory.Deps{
		MessageStore: msgStore,
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
		ReportStore:  reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent", ConversationID: "conv"}
	if _, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-current",
			Message: model.NewTextMessage(model.RoleUser, "Ada hid the blue notebook beside the lamp."),
		}},
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	report, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityMessageIndex},
	})
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	result, err := mem.RunOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), listErr.Error()) {
		t.Fatalf("RunOnce() result = %+v error = %v, want list backend failure", result, err)
	}
	if result.Completed {
		t.Fatalf("RunOnce() result = %+v, want failed job", result)
	}
	stored, ok, err := reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
	}
	if stored.Status != memory.LifecycleStatusFailed {
		t.Fatalf("stored status = %q, want failed; report=%+v", stored.Status, stored)
	}
	step := requireLifecycleStep(t, stored.Steps, "message_index.conversation")
	if step.Status != memory.LifecycleStatusFailed {
		t.Fatalf("step status = %q, want failed; step=%+v", step.Status, step)
	}
	if got := step.Details["stale_projection_scan"]; got != "list_failed" {
		t.Fatalf("stale_projection_scan = %v, want list_failed; step=%+v", got, step)
	}
	if got := step.Details["cleanup_mode"]; got == "skipped_degraded" {
		t.Fatalf("cleanup_mode = %v, want backend failure not degraded; step=%+v", got, step)
	}
}

func TestLifecycleEmptyCapabilitiesSelectConversationDerivedViews(t *testing.T) {
	ctx := context.Background()
	messageOnly, err := newLifecycleSelectionSystem(t, false)
	if err != nil {
		t.Fatalf("newLifecycleSelectionSystem(messageOnly) error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	report, err := messageOnly.Rebuild(ctx, memory.RebuildRequest{Scope: scope, DryRun: true})
	if err != nil {
		t.Fatalf("Rebuild(messageOnly) error = %v", err)
	}
	if !capabilityPresent(report.Operation.Capabilities, memory.CapabilityMessageIndex) || capabilityPresent(report.Operation.Capabilities, memory.CapabilitySummaryDAG) {
		t.Fatalf("messageOnly capabilities = %+v, want only message_index", report.Operation.Capabilities)
	}

	messageAndSummary, err := newLifecycleSelectionSystem(t, true)
	if err != nil {
		t.Fatalf("newLifecycleSelectionSystem(messageAndSummary) error = %v", err)
	}
	report, err = messageAndSummary.Reload(ctx, memory.ReloadRequest{Scope: scope, DryRun: true})
	if err != nil {
		t.Fatalf("Reload(messageAndSummary) error = %v", err)
	}
	if !capabilityPresent(report.Operation.Capabilities, memory.CapabilityMessageIndex) || !capabilityPresent(report.Operation.Capabilities, memory.CapabilitySummaryDAG) {
		t.Fatalf("messageAndSummary capabilities = %+v, want message_index and summary_dag", report.Operation.Capabilities)
	}
	if !lifecycleStepPresent(report.Steps, "message_index.conversation") || !lifecycleStepPresent(report.Steps, "summary_dag.conversation") {
		t.Fatalf("steps = %+v, want message and summary dry-run steps", report.Steps)
	}
}

func TestLifecycleConversationDerivedViewsRequireConversationScope(t *testing.T) {
	ctx := context.Background()
	mem, err := newLifecycleSelectionSystem(t, true)
	if err != nil {
		t.Fatalf("newLifecycleSelectionSystem() error = %v", err)
	}
	report, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope: memory.Scope{RuntimeID: "rt", UserID: "user"},
		Capabilities: []memory.Capability{
			memory.CapabilityMessageIndex,
			memory.CapabilitySummaryDAG,
		},
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if report.Status == memory.LifecycleStatusCompleted || report.Status == memory.LifecycleStatusEnqueued {
		t.Fatalf("Rebuild report = %+v, want skipped/not enqueued without conversation_id", report)
	}
	if !lifecycleSkippedStepPresent(report.Steps, "message_index.scope") || !lifecycleSkippedStepPresent(report.Steps, "summary_dag.scope") {
		t.Fatalf("steps = %+v, want skipped scope steps", report.Steps)
	}
}

func TestAppendMessageAndPackRecentContext(t *testing.T) {
	ctx := context.Background()
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "pack_context"},
		},
	}, memory.Deps{MessageStore: newMessageStore()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	result, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
		Scope: scope,
		Messages: []sourcemessage.Message{{
			Message: model.NewTextMessage(model.RoleUser, "hello"),
		}},
	})
	if err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}
	if len(result.Jobs) != 0 {
		t.Fatalf("AppendMessage Jobs = %+v, want none", result.Jobs)
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{Scope: scope})
	if err != nil {
		t.Fatalf("PackContext() error = %v", err)
	}
	if len(pack.Items) != 1 || pack.Items[0].Text != "user: hello" {
		t.Fatalf("PackContext Items = %+v, want recent message", pack.Items)
	}
}

func TestAppendMessageIndexesAndRetrievesSourceMessages(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	defer func() { _ = index.Close() }()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true}},
		WriteStages: []memory.StageSpec{
			{Name: "append_message"},
			{Name: "index_messages"},
		},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_messages"},
			{Name: "pack_context"},
		},
	}, memory.Deps{
		MessageStore: sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message")),
		Index:        index,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv", DatasetID: "dataset"}
	_, err = mem.AppendMessage(ctx, memory.AppendMessageRequest{
		Scope: scope,
		Messages: []sourcemessage.Message{
			{
				ID:       "dia-1",
				Message:  model.NewTextMessage(model.RoleUser, "Ada hid the blue notebook beside the lamp."),
				Metadata: map[string]any{"dia_id": "dia-1", "speaker": "Ada"},
			},
			{
				ID:       "dia-2",
				Message:  model.NewTextMessage(model.RoleUser, "Ben talked about lunch."),
				Metadata: map[string]any{"dia_id": "dia-2", "speaker": "Ben"},
			},
		},
	})
	if err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: scope,
		Query: "blue notebook lamp",
		TopK:  3,
		Window: recent.WindowRequest{Budget: &recent.WindowBudget{
			MaxMessages: 1,
		}},
	})
	if err != nil {
		t.Fatalf("PackContext() error = %v", err)
	}
	if len(pack.MessageHits) == 0 {
		t.Fatalf("MessageHits = 0, want source message retrieval hit")
	}
	if pack.MessageHits[0].Message.ID != "dia-1" {
		t.Fatalf("first MessageHit = %q, want dia-1", pack.MessageHits[0].Message.ID)
	}
	if len(pack.Items) == 0 || pack.Items[0].Message == nil || pack.Items[0].Message.ID != "dia-1" || pack.Items[0].Retrieval == nil {
		t.Fatalf("first context item = %+v, want retrieval-backed dia-1 source message", pack.Items)
	}
}

func TestImportDocumentWithoutChunkerReturnsNotAvailableWhenDefaultPlanIncludesChunks(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	documentStore := sourcedocument.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/document"))
	chunkStore := viewdocument.NewChunkWorkspaceStore(sdkworkspace.Sub(root, "views/document_chunks"))
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceDocumentStore, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityDocumentChunks, Required: true}},
		Projections:  []memory.ProjectionSpec{{Capability: memory.CapabilityDocumentChunks, Namespace: "document_chunks", Required: true}},
	}, memory.Deps{
		DocumentStore: documentStore,
		ChunkStore:    chunkStore,
		Index:         index,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !plannedStageNames(mem.Plan().Write)["chunk_document"] {
		t.Fatalf("default Write = %+v, want chunk_document despite missing chunker", mem.Plan().Write)
	}

	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv", DatasetID: "dataset"}
	_, err = mem.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope: scope,
		Document: sourcedocument.Document{
			ID:      "doc-1",
			Content: "Ada stored the field notes in the cedar box.",
		},
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("ImportDocument() error = %v, want NotAvailable without DocumentChunker", err)
	}
	chunks, err := chunkStore.ListChunks(ctx, "doc-1", viewdocument.ListOptions{Scope: &scope})
	if err != nil {
		t.Fatalf("ListChunks() error = %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("chunks = %+v, want no derived chunks after NotAvailable", chunks)
	}
}

func TestAppendMessageWithoutSummarizerReturnsNotAvailableAfterMessageIndex(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	summaryStore := recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag"))
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{
			{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true},
			{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true},
		},
	}, memory.Deps{
		MessageStore: msgStore,
		SummaryStore: summaryStore,
		Index:        index,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stages := plannedStageNames(mem.Plan().Write)
	if !stages["index_messages"] || !stages["build_summary_dag"] {
		t.Fatalf("default Write = %+v, want message_index and summary_dag stages despite missing summarizer", mem.Plan().Write)
	}

	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	_, err = mem.AppendMessage(ctx, memory.AppendMessageRequest{
		Scope: scope,
		Messages: []sourcemessage.Message{{
			ID:      "dia-1",
			Message: model.NewTextMessage(model.RoleUser, "Ada found a brass lantern."),
		}},
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("AppendMessage() error = %v, want NotAvailable without Summarizer", err)
	}
	messages, err := msgStore.List(ctx, scope.ConversationID, sourcemessage.ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %+v, want appended source message retained", messages)
	}
	messageNamespace, err := projectors.ScopedNamespace("message_index", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace(message_index) error = %v", err)
	}
	record := sourceMessageRecord(t, scope, messages[0])
	if _, ok, err := index.Get(ctx, messageNamespace, record.ID); err != nil || !ok {
		t.Fatalf("message projection exists = %v err = %v, want message_index stage to complete before summary failure", ok, err)
	}
	nodes, err := summaryStore.ListNodes(ctx, scope, recent.ListOptions{})
	if err != nil {
		t.Fatalf("ListNodes() error = %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("summary nodes = %+v, want no summary nodes after NotAvailable", nodes)
	}
}

func TestFreshnessScansProjectionMetadata(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.document_chunks.freshness_metadata")
	if !report.Ready || !report.OK || !check.OK {
		t.Fatalf("freshness report/check = %+v / %+v, want OK", report, check)
	}
	assertDiagnosticDetailInt(t, check, "records_scanned", 1)
	assertDiagnosticDetailInt(t, check, "missing_source_refs", 0)
	assertDiagnosticDetailInt(t, check, "invalid_source_refs", 0)
	assertDiagnosticDetailInt(t, check, "missing_signature", 0)
	assertDiagnosticDetailInt(t, check, "invalid_signature", 0)
	staleness := requireDiagnosticCheck(t, report.Checks, "projection.document_chunks.staleness")
	if !staleness.OK {
		t.Fatalf("staleness check = %+v, want OK", staleness)
	}
	assertDiagnosticDetailInt(t, staleness, "records_scanned", 1)
	assertDiagnosticDetailInt(t, staleness, "records_compared", 1)
	assertDiagnosticDetailInt(t, staleness, "stale_records", 0)
}

func TestDocumentChunkReadAvailabilityDoesNotRequireChunker(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)

	readOnly, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceDocumentStore, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityDocumentChunks, Required: true}},
		Projections:  []memory.ProjectionSpec{{Capability: memory.CapabilityDocumentChunks, Namespace: "document_chunks", Required: true}},
	}, memory.Deps{
		DocumentStore: fixture.documentStore,
		ChunkStore:    fixture.chunkStore,
		Index:         fixture.index,
	})
	if err != nil {
		t.Fatalf("New(read-only without chunker) error = %v", err)
	}

	report, err := readOnly.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness without chunker) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.document_chunks.freshness_metadata")
	if !report.Ready || !check.OK {
		t.Fatalf("freshness without chunker = %+v / %+v, want read checks ready and OK", report, check)
	}
	writeWarning := requireDiagnosticCheck(t, report.Checks, "write_readiness.document_chunks")
	if writeWarning.Status != memory.DiagnosticStatusWarning || writeWarning.Severity != memory.DiagnosticSeverityWarning || writeWarning.OK {
		t.Fatalf("write readiness warning = %+v, want warning without failing Ready", writeWarning)
	}
	if hasDiagnosticCheck(report.Checks, "capability.document_chunks.service") {
		t.Fatalf("freshness checks = %+v, want no write-side chunker dependency check", report.Checks)
	}
	readiness, err := readOnly.Readiness(ctx)
	if err != nil {
		t.Fatalf("Readiness(without chunker) error = %v", err)
	}
	readinessWarning := requireReadinessCheck(t, readiness.Checks, "write_readiness.document_chunks")
	if !readiness.Ready || readinessWarning.Ready || readinessWarning.Severity != memory.DiagnosticSeverityWarning {
		t.Fatalf("Readiness(without chunker) = %+v, warning = %+v, want Ready with write warning", readiness, readinessWarning)
	}

	freshness, err := readOnly.Freshness(ctx, memory.FreshnessRequest{
		Scope:        fixture.scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
	})
	if err != nil {
		t.Fatalf("Freshness(without chunker) error = %v", err)
	}
	if freshness.Status != memory.LifecycleStatusCompleted || !freshness.Ready || freshness.OK {
		t.Fatalf("Freshness(without chunker) = %+v, want completed with write warning and Ready=true", freshness)
	}
	if warning := requireDiagnosticCheck(t, freshness.Checks, "write_readiness.document_chunks"); warning.Severity != memory.DiagnosticSeverityWarning {
		t.Fatalf("Freshness write warning = %+v, want warning severity", warning)
	}

	rebuild, err := readOnly.Rebuild(ctx, memory.RebuildRequest{
		Scope:     fixture.scope,
		Documents: []memory.DocumentTarget{{DocumentID: "doc-1"}},
		DryRun:    true,
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("Rebuild(without chunker) error = %v, report = %+v, want NotAvailable", err, rebuild)
	}
}

func TestSummaryDAGReadAvailabilityDoesNotRequireSummarizer(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedSummaryFixture(t, ctx)

	readOnly, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true}},
	}, memory.Deps{
		MessageStore: fixture.mem.MessageStore(),
		SummaryStore: fixture.summaryStore,
		Index:        fixture.index,
	})
	if err != nil {
		t.Fatalf("New(read-only without summarizer) error = %v", err)
	}

	report, err := readOnly.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness without summarizer) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.summary_dag.freshness_metadata")
	if !report.Ready || !check.OK {
		t.Fatalf("freshness without summarizer = %+v / %+v, want read checks ready and OK", report, check)
	}
	writeWarning := requireDiagnosticCheck(t, report.Checks, "write_readiness.summary_dag")
	if writeWarning.Status != memory.DiagnosticStatusWarning || writeWarning.Severity != memory.DiagnosticSeverityWarning || writeWarning.OK {
		t.Fatalf("write readiness warning = %+v, want warning without failing Ready", writeWarning)
	}
	if hasDiagnosticCheck(report.Checks, "capability.summary_dag.service") {
		t.Fatalf("freshness checks = %+v, want no write-side summarizer dependency check", report.Checks)
	}
	readiness, err := readOnly.Readiness(ctx)
	if err != nil {
		t.Fatalf("Readiness(without summarizer) error = %v", err)
	}
	readinessWarning := requireReadinessCheck(t, readiness.Checks, "write_readiness.summary_dag")
	if !readiness.Ready || readinessWarning.Ready || readinessWarning.Severity != memory.DiagnosticSeverityWarning {
		t.Fatalf("Readiness(without summarizer) = %+v, warning = %+v, want Ready with write warning", readiness, readinessWarning)
	}

	freshness, err := readOnly.Freshness(ctx, memory.FreshnessRequest{
		Scope:        fixture.scope,
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
	})
	if err != nil {
		t.Fatalf("Freshness(without summarizer) error = %v", err)
	}
	if freshness.Status != memory.LifecycleStatusCompleted || !freshness.Ready || freshness.OK {
		t.Fatalf("Freshness(without summarizer) = %+v, want completed with write warning and Ready=true", freshness)
	}
	if warning := requireDiagnosticCheck(t, freshness.Checks, "write_readiness.summary_dag"); warning.Severity != memory.DiagnosticSeverityWarning {
		t.Fatalf("Freshness write warning = %+v, want warning severity", warning)
	}

	pack, err := readOnly.PackContext(ctx, memory.ContextRequest{
		Scope: fixture.scope,
		Query: "summary",
		TopK:  3,
	})
	if err != nil {
		t.Fatalf("PackContext(without summarizer) error = %v", err)
	}
	if len(pack.SummaryHits) == 0 || pack.SummaryHits[0].Node.ID != fixture.node.ID {
		t.Fatalf("SummaryHits = %+v, want existing summary node", pack.SummaryHits)
	}

	rebuild, err := readOnly.Rebuild(ctx, memory.RebuildRequest{
		Scope:        fixture.scope,
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
		DryRun:       true,
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("Rebuild(without summarizer) error = %v, report = %+v, want NotAvailable", err, rebuild)
	}
}

func TestFreshnessReportsStaleDocumentChunkViewRecordWarning(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	chunk, ok, err := fixture.chunkStore.GetChunk(ctx, fixture.scope, "doc-1", viewdocument.ChunkID("whole"))
	if err != nil || !ok {
		t.Fatalf("GetChunk() ok = %v err = %v, want existing chunk", ok, err)
	}
	chunk.Signature.TransformSignature = "test:canonical-chunk-updated"
	if _, err := fixture.chunkStore.PutChunk(ctx, chunk); err != nil {
		t.Fatalf("PutChunk(updated canonical chunk) error = %v", err)
	}

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.document_chunks.view_freshness")
	if check.Status != memory.DiagnosticStatusStale || check.Severity != memory.DiagnosticSeverityWarning || check.OK {
		t.Fatalf("view_freshness check = %+v, want stale warning", check)
	}
	if !report.Ready || report.OK {
		t.Fatalf("freshness report = %+v, want stale warning to keep Ready but fail OK", report)
	}
	assertDiagnosticDetailInt(t, check, "records_compared", 1)
	assertDiagnosticDetailInt(t, check, "records_skipped", 0)
	assertDiagnosticDetailInt(t, check, "stale_records", 1)
	assertDiagnosticDetailString(t, check, "first_stale_doc_id", projectors.DocumentChunkRecordID(fixture.scope.DatasetID, "doc-1", viewdocument.ChunkID("whole")))
	assertDiagnosticDetailString(t, check, "first_stale_reason", "signature")
}

func TestFreshnessReportsStaleSummaryViewRecordWarning(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedSummaryFixture(t, ctx)
	node := fixture.node
	node.Signature.TransformSignature = "test-summary-lifecycle:new"
	if _, err := fixture.summaryStore.PutNode(ctx, node); err != nil {
		t.Fatalf("PutNode(updated canonical summary) error = %v", err)
	}

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.summary_dag.view_freshness")
	if check.Status != memory.DiagnosticStatusStale || check.Severity != memory.DiagnosticSeverityWarning || check.OK {
		t.Fatalf("view_freshness check = %+v, want stale warning", check)
	}
	if !report.Ready || report.OK {
		t.Fatalf("freshness report = %+v, want stale warning to keep Ready but fail OK", report)
	}
	assertDiagnosticDetailInt(t, check, "records_compared", 1)
	assertDiagnosticDetailInt(t, check, "records_skipped", 0)
	assertDiagnosticDetailInt(t, check, "stale_records", 1)
	assertDiagnosticDetailString(t, check, "first_stale_reason", "signature")
}

func TestFreshnessMessageViewFreshnessComparesSourceRefs(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedMessageFixture(t, ctx)

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilityMessageIndex},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	metadata := requireDiagnosticCheck(t, report.Checks, "projection.message_index.freshness_metadata")
	if metadata.Status != memory.DiagnosticStatusWarning || metadata.Severity != memory.DiagnosticSeverityWarning || metadata.OK {
		t.Fatalf("freshness metadata check = %+v, want missing signature warning", metadata)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.message_index.view_freshness")
	if !check.OK {
		t.Fatalf("view_freshness check = %+v, want OK despite missing message signature", check)
	}
	sourceStaleness := requireDiagnosticCheck(t, report.Checks, "projection.message_index.source_staleness")
	if !sourceStaleness.OK {
		t.Fatalf("source_staleness check = %+v, want OK for valid canonical source", sourceStaleness)
	}
	if !report.Ready || report.OK {
		t.Fatalf("freshness report = %+v, want missing signature warning only", report)
	}
	assertDiagnosticDetailInt(t, check, "records_compared", 1)
	assertDiagnosticDetailInt(t, check, "records_skipped", 0)
	assertDiagnosticDetailInt(t, check, "stale_records", 0)
	assertDiagnosticDetailInt(t, sourceStaleness, "records_compared", 1)
	assertDiagnosticDetailInt(t, sourceStaleness, "stale_records", 0)
}

func TestFreshnessReportsStaleMessageViewRecordWarning(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedMessageFixture(t, ctx)
	if err := fixture.index.Delete(ctx, fixture.namespace, []string{fixture.recordID}); err != nil {
		t.Fatalf("Delete valid projection doc error = %v", err)
	}
	writer, err := indexed.NewWriter(fixture.index, indexed.Binding{Namespace: fixture.namespace})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	wrongRef := views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: fixture.scope.ConversationID,
			MessageID:      "dia-wrong",
		},
	}
	if err := writer.Upsert(ctx, []indexed.Record{{
		ID:         fixture.recordID,
		Text:       "projection points at the wrong source ref",
		Metadata:   diagnosticProjectionMetadata(fixture.scope, memory.CapabilityMessageIndex, fixture.message.ID, 0),
		SourceRefs: []views.SourceRef{wrongRef},
	}}); err != nil {
		t.Fatalf("Upsert stale message projection doc error = %v", err)
	}

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilityMessageIndex},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.message_index.view_freshness")
	if check.Status != memory.DiagnosticStatusStale || check.Severity != memory.DiagnosticSeverityWarning || check.OK {
		t.Fatalf("view_freshness check = %+v, want stale warning", check)
	}
	if !report.Ready || report.OK {
		t.Fatalf("freshness report = %+v, want warning to keep Ready but fail OK", report)
	}
	assertDiagnosticDetailInt(t, check, "records_compared", 1)
	assertDiagnosticDetailInt(t, check, "records_skipped", 0)
	assertDiagnosticDetailInt(t, check, "stale_records", 1)
	assertDiagnosticDetailString(t, check, "first_stale_doc_id", fixture.recordID)
	assertDiagnosticDetailString(t, check, "first_stale_reason", "source_refs")
}

func TestFreshnessReportsStaleMessageSourceWarningWhenCanonicalMissing(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedMessageFixture(t, ctx)
	if err := fixture.mem.MessageStore().DeleteConversation(ctx, fixture.scope.ConversationID); err != nil {
		t.Fatalf("DeleteConversation() error = %v", err)
	}

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilityMessageIndex},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.message_index.source_staleness")
	if check.Status != memory.DiagnosticStatusStale || check.Severity != memory.DiagnosticSeverityWarning || check.OK {
		t.Fatalf("source_staleness check = %+v, want stale warning", check)
	}
	if !report.Ready || report.OK {
		t.Fatalf("freshness report = %+v, want stale warning to keep Ready but fail OK", report)
	}
	assertDiagnosticDetailInt(t, check, "records_compared", 1)
	assertDiagnosticDetailInt(t, check, "records_skipped", 0)
	assertDiagnosticDetailInt(t, check, "canonical_misses", 1)
	assertDiagnosticDetailInt(t, check, "stale_records", 1)
}

func TestFreshnessSummarySourceStalenessValidProjectionOK(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedSummaryFixture(t, ctx)

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.summary_dag.source_staleness")
	if !report.Ready || !report.OK || !check.OK {
		t.Fatalf("freshness report/source_staleness check = %+v / %+v, want OK", report, check)
	}
	assertDiagnosticDetailInt(t, check, "records_compared", 1)
	assertDiagnosticDetailInt(t, check, "stale_records", 0)
}

func TestFreshnessReportsStaleSummarySourceWarningWhenCanonicalMissing(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedSummaryFixture(t, ctx)
	if err := fixture.mem.MessageStore().DeleteConversation(ctx, fixture.scope.ConversationID); err != nil {
		t.Fatalf("DeleteConversation() error = %v", err)
	}

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.summary_dag.source_staleness")
	if check.Status != memory.DiagnosticStatusStale || check.Severity != memory.DiagnosticSeverityWarning || check.OK {
		t.Fatalf("source_staleness check = %+v, want stale warning", check)
	}
	if !report.Ready || report.OK {
		t.Fatalf("freshness report = %+v, want stale warning to keep Ready but fail OK", report)
	}
	assertDiagnosticDetailInt(t, check, "records_compared", 1)
	assertDiagnosticDetailInt(t, check, "canonical_misses", 1)
	assertDiagnosticDetailInt(t, check, "stale_records", 1)
}

func TestFreshnessReportsStaleSummarySourceWarningWhenCanonicalChanged(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedSummaryFixture(t, ctx)
	addSummarySourceContentHash(t, ctx, fixture)
	if err := fixture.mem.MessageStore().DeleteConversation(ctx, fixture.scope.ConversationID); err != nil {
		t.Fatalf("DeleteConversation() error = %v", err)
	}
	if _, err := fixture.mem.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: fixture.scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-1",
			Message: model.NewTextMessage(model.RoleUser, "Ada found a changed brass lantern."),
		}},
	}); err != nil {
		t.Fatalf("Append(changed canonical message) error = %v", err)
	}

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.summary_dag.source_staleness")
	if check.Status != memory.DiagnosticStatusStale || check.Severity != memory.DiagnosticSeverityWarning || check.OK {
		t.Fatalf("source_staleness check = %+v, want stale warning", check)
	}
	if !report.Ready || report.OK {
		t.Fatalf("freshness report = %+v, want stale warning to keep Ready but fail OK", report)
	}
	assertDiagnosticDetailInt(t, check, "records_compared", 1)
	assertDiagnosticDetailInt(t, check, "canonical_misses", 0)
	assertDiagnosticDetailInt(t, check, "stale_records", 1)
}

func TestFreshnessReportsStaleSummarySourceWarningWhenSourceRevisionUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sourceKey string
	}{
		{name: "malformed_source_key", sourceKey: "{not-json"},
		{name: "unresolvable_source_key", sourceKey: `{"schema":"opaque.source_ref.v1","kind":"message","message":{"conversation_id":"conv","message_id":"dia-1"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newProjectedSummaryFixture(t, ctx)
			node := fixture.node
			node.Signature.SourceRevisions[0].SourceKey = tc.sourceKey
			putProjectedSummaryNode(t, ctx, fixture, node)

			report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
				Scope:        fixture.scope,
				Stage:        "freshness",
				Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
				PageSize:     10,
			})
			if err != nil {
				t.Fatalf("Diagnostics(freshness) error = %v", err)
			}
			check := requireDiagnosticCheck(t, report.Checks, "projection.summary_dag.source_staleness")
			if check.Status != memory.DiagnosticStatusStale || check.Severity != memory.DiagnosticSeverityWarning || check.OK {
				t.Fatalf("source_staleness check = %+v, want stale warning", check)
			}
			if !report.Ready || report.OK {
				t.Fatalf("freshness report = %+v, want invalid lineage warning to keep Ready but fail OK", report)
			}
			assertDiagnosticDetailInt(t, check, "records_compared", 0)
			assertDiagnosticDetailInt(t, check, "records_skipped", 1)
			assertDiagnosticDetailInt(t, check, "missing_source_revisions", 1)
			assertDiagnosticDetailInt(t, check, "invalid_source_revisions", 1)
			assertDiagnosticDetailInt(t, check, "compare_errors", 0)
		})
	}
}

func TestFreshnessReportsStaleSummarySourceWarningWhenSourceRevisionNotInSourceRefs(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedSummaryFixture(t, ctx)
	messages, err := fixture.mem.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: fixture.scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-2",
			Message: model.NewTextMessage(model.RoleUser, "Ada found a silver compass."),
		}},
	})
	if err != nil {
		t.Fatalf("Append(mismatched source message) error = %v", err)
	}
	mismatchedRef := views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: fixture.scope.ConversationID,
			MessageID:      messages[0].ID,
		},
	}
	node := fixture.node
	node.Signature.SourceRevisions[0].SourceKey = mismatchedRef.StableKey()
	node.Signature.SourceRevisions[0].Revision = strconv.FormatUint(messages[0].Seq, 10)
	putProjectedSummaryNode(t, ctx, fixture, node)

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.summary_dag.source_staleness")
	if check.Status != memory.DiagnosticStatusStale || check.Severity != memory.DiagnosticSeverityWarning || check.OK {
		t.Fatalf("source_staleness check = %+v, want stale warning", check)
	}
	if !report.Ready || report.OK {
		t.Fatalf("freshness report = %+v, want mismatched lineage warning to keep Ready but fail OK", report)
	}
	assertDiagnosticDetailInt(t, check, "records_compared", 1)
	assertDiagnosticDetailInt(t, check, "records_skipped", 0)
	assertDiagnosticDetailInt(t, check, "invalid_source_revisions", 1)
	assertDiagnosticDetailInt(t, check, "canonical_misses", 0)
	assertDiagnosticDetailInt(t, check, "stale_records", 0)
	assertDiagnosticDetailInt(t, check, "compare_errors", 0)
}

func TestFreshnessReportsStaleSummarySourceWarningWhenSourceRefMissingRevision(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedSummaryFixture(t, ctx)
	messages, err := fixture.mem.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: fixture.scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-2",
			Message: model.NewTextMessage(model.RoleUser, "Ada also found a silver compass."),
		}},
	})
	if err != nil {
		t.Fatalf("Append(uncovered source message) error = %v", err)
	}
	uncoveredRef := views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: fixture.scope.ConversationID,
			MessageID:      messages[0].ID,
		},
	}
	node := fixture.node
	node.SourceRefs = append(append([]views.SourceRef(nil), node.SourceRefs...), uncoveredRef)
	putProjectedSummaryNode(t, ctx, fixture, node)

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.summary_dag.source_staleness")
	if check.Status != memory.DiagnosticStatusStale || check.Severity != memory.DiagnosticSeverityWarning || check.OK {
		t.Fatalf("source_staleness check = %+v, want stale warning", check)
	}
	if !report.Ready || report.OK {
		t.Fatalf("freshness report = %+v, want missing source revision warning to keep Ready but fail OK", report)
	}
	assertDiagnosticDetailInt(t, check, "records_compared", 1)
	assertDiagnosticDetailInt(t, check, "records_skipped", 0)
	assertDiagnosticDetailInt(t, check, "missing_source_revisions", 1)
	assertDiagnosticDetailInt(t, check, "invalid_source_revisions", 0)
	assertDiagnosticDetailInt(t, check, "canonical_misses", 0)
	assertDiagnosticDetailInt(t, check, "stale_records", 0)
	assertDiagnosticDetailInt(t, check, "compare_errors", 0)
}

func TestFreshnessReportsStaleDocumentChunkProjectionWarning(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	if _, err := fixture.documentStore.Put(ctx, sourcedocument.PutRequest{
		Document: sourcedocument.Document{
			DatasetID: fixture.scope.DatasetID,
			ID:        "doc-1",
			Content:   "Ada moved the updated field notes into the cedar box.",
		},
	}); err != nil {
		t.Fatalf("Put updated canonical document error = %v", err)
	}

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.document_chunks.staleness")
	if check.Status != memory.DiagnosticStatusStale || check.Severity != memory.DiagnosticSeverityWarning || check.OK {
		t.Fatalf("staleness check = %+v, want stale warning", check)
	}
	if !report.Ready || report.OK {
		t.Fatalf("freshness report = %+v, want stale warning to keep Ready but fail OK", report)
	}
	if check.RepairHint != "rebuild document_chunks for affected documents" {
		t.Fatalf("RepairHint = %q, want rebuild hint", check.RepairHint)
	}
	assertDiagnosticDetailInt(t, check, "records_scanned", 1)
	assertDiagnosticDetailInt(t, check, "records_compared", 1)
	assertDiagnosticDetailInt(t, check, "stale_records", 1)
}

func TestFreshnessMissingSignatureSkipsDocumentChunkStalenessCompare(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	if err := fixture.index.Delete(ctx, fixture.namespace, []string{
		projectors.DocumentChunkRecordID(fixture.scope.DatasetID, "doc-1", viewdocument.ChunkID("whole")),
	}); err != nil {
		t.Fatalf("Delete valid projection doc error = %v", err)
	}
	ref := views.SourceRef{
		Kind: views.SourceDocument,
		Document: &views.DocumentSourceRef{
			DatasetID:  fixture.scope.DatasetID,
			DocumentID: "doc-1",
		},
	}
	writer, err := indexed.NewWriter(fixture.index, indexed.Binding{Namespace: fixture.namespace})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Upsert(ctx, []indexed.Record{{
		ID:         "missing-signature",
		Text:       "projection missing indexed signature",
		Metadata:   diagnosticProjectionMetadata(fixture.scope, memory.CapabilityDocumentChunks, "missing-signature", 0),
		SourceRefs: []views.SourceRef{ref},
	}}); err != nil {
		t.Fatalf("Upsert missing signature projection doc error = %v", err)
	}

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	metadata := requireDiagnosticCheck(t, report.Checks, "projection.document_chunks.freshness_metadata")
	if metadata.Status != memory.DiagnosticStatusWarning || metadata.Severity != memory.DiagnosticSeverityWarning || metadata.OK {
		t.Fatalf("freshness metadata check = %+v, want missing signature warning", metadata)
	}
	staleness := requireDiagnosticCheck(t, report.Checks, "projection.document_chunks.staleness")
	if !staleness.OK {
		t.Fatalf("staleness check = %+v, want OK when missing signature is skipped", staleness)
	}
	assertDiagnosticDetailInt(t, staleness, "records_scanned", 1)
	assertDiagnosticDetailInt(t, staleness, "records_compared", 0)
	assertDiagnosticDetailInt(t, staleness, "records_skipped", 1)
	assertDiagnosticDetailInt(t, staleness, "stale_records", 0)
}

func TestFreshnessMissingProjectionMetadataWarningKeepsReady(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	if err := fixture.index.Upsert(ctx, fixture.namespace, []retrieval.Doc{{
		ID:      "missing-metadata",
		Content: "projection missing indexed freshness metadata",
		Metadata: map[string]any{
			projectors.MetadataViewKindKey:   "document_chunks",
			projectors.MetadataRecordTypeKey: projectors.RecordTypeDocumentChunk,
			projectors.MetadataRuntimeIDKey:  fixture.scope.RuntimeID,
			projectors.MetadataUserIDKey:     fixture.scope.UserID,
			projectors.MetadataAgentIDKey:    fixture.scope.AgentID,
			projectors.MetadataDatasetIDKey:  fixture.scope.DatasetID,
			projectors.MetadataDocumentIDKey: "doc-1",
			projectors.MetadataChunkIDKey:    "missing-metadata",
		},
	}}); err != nil {
		t.Fatalf("Upsert missing metadata projection doc error = %v", err)
	}

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.document_chunks.freshness_metadata")
	if check.Status != memory.DiagnosticStatusWarning || check.Severity != memory.DiagnosticSeverityWarning || check.OK {
		t.Fatalf("freshness metadata check = %+v, want warning", check)
	}
	if !report.Ready || report.OK {
		t.Fatalf("freshness report = %+v, want warning to keep Ready but fail OK", report)
	}
	assertDiagnosticDetailInt(t, check, "records_scanned", 2)
	assertDiagnosticDetailInt(t, check, "missing_source_refs", 1)
	assertDiagnosticDetailInt(t, check, "missing_signature", 1)
}

func TestFreshnessReportsProjectionMetadataIssues(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	if err := fixture.index.Upsert(ctx, fixture.namespace, []retrieval.Doc{{
		ID:      "bad-metadata",
		Content: "projection with bad indexed metadata",
		Metadata: map[string]any{
			projectors.MetadataViewKindKey:   "document_chunks",
			projectors.MetadataRecordTypeKey: projectors.RecordTypeDocumentChunk,
			projectors.MetadataRuntimeIDKey:  fixture.scope.RuntimeID,
			projectors.MetadataUserIDKey:     fixture.scope.UserID,
			projectors.MetadataAgentIDKey:    fixture.scope.AgentID,
			projectors.MetadataDatasetIDKey:  fixture.scope.DatasetID,
			projectors.MetadataDocumentIDKey: "doc-1",
			projectors.MetadataChunkIDKey:    "bad-metadata",
			indexed.MetadataSourceRefsKey: []any{
				map[string]any{"kind": "document"},
			},
		},
	}}); err != nil {
		t.Fatalf("Upsert bad metadata projection doc error = %v", err)
	}

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.document_chunks.freshness_metadata")
	if check.Status != memory.DiagnosticStatusError || check.Severity != memory.DiagnosticSeverityError || check.OK {
		t.Fatalf("freshness metadata check = %+v, want error", check)
	}
	if report.Ready || report.OK {
		t.Fatalf("freshness report = %+v, want invalid metadata to fail readiness", report)
	}
	assertDiagnosticDetailInt(t, check, "records_scanned", 2)
	assertDiagnosticDetailInt(t, check, "invalid_source_refs", 1)
	assertDiagnosticDetailInt(t, check, "missing_signature", 1)
	viewFreshness := requireDiagnosticCheck(t, report.Checks, "projection.document_chunks.view_freshness")
	if !viewFreshness.OK {
		t.Fatalf("view_freshness check = %+v, want OK because invalid metadata is handled by metadata check", viewFreshness)
	}
	assertDiagnosticDetailInt(t, viewFreshness, "records_scanned", 2)
	assertDiagnosticDetailInt(t, viewFreshness, "records_compared", 1)
	assertDiagnosticDetailInt(t, viewFreshness, "records_skipped", 1)
	assertDiagnosticDetailInt(t, viewFreshness, "stale_records", 0)
}

func TestConsistencyReportsProjectionHydrationMiss(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	if err := fixture.index.Upsert(ctx, fixture.namespace, []retrieval.Doc{danglingDocumentChunkProjectionDoc(fixture.scope)}); err != nil {
		t.Fatalf("Upsert dangling projection doc error = %v", err)
	}

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "consistency",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Consistency:  []memory.ConsistencyCheckKind{memory.ConsistencyCheckProjection},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(consistency) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.document_chunks.hydration")
	if check.Status != memory.DiagnosticStatusError || check.Severity != memory.DiagnosticSeverityError || check.OK {
		t.Fatalf("hydration check = %+v, want error", check)
	}
	if report.Ready || report.OK {
		t.Fatalf("consistency report = %+v, want hydration miss to fail readiness", report)
	}
	assertDiagnosticDetailInt(t, check, "records_scanned", 2)
	assertDiagnosticDetailInt(t, check, "records_hydrated", 1)
	assertDiagnosticDetailInt(t, check, "hydrate_misses", 1)
	if check.RepairHint != "rebuild document_chunks for affected documents" {
		t.Fatalf("RepairHint = %q, want document_chunks rebuild hint", check.RepairHint)
	}
	if check.Target.DatasetID != fixture.scope.DatasetID || check.Target.DocumentID != "missing-doc" {
		t.Fatalf("Target = %+v, want missing-doc document target", check.Target)
	}
}

func TestConsistencyHydratesValidProjection(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)

	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "consistency",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Consistency:  []memory.ConsistencyCheckKind{memory.ConsistencyCheckProjection},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(consistency) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.document_chunks.hydration")
	if !report.Ready || !report.OK || !check.OK {
		t.Fatalf("consistency report/check = %+v / %+v, want OK", report, check)
	}
	assertDiagnosticDetailInt(t, check, "records_scanned", 1)
	assertDiagnosticDetailInt(t, check, "records_hydrated", 1)
	assertDiagnosticDetailInt(t, check, "hydrate_misses", 0)
	assertDiagnosticDetailInt(t, check, "hydrate_errors", 0)
}

func TestReconcileDocumentTargetRepairsStaleDocumentChunks(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	makeProjectedDocumentStale(t, ctx, fixture)
	requireDocumentChunkStale(t, ctx, fixture)

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:     fixture.scope,
		Documents: []memory.DocumentTarget{{DocumentID: "doc-1"}},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued || report.JobID == "" {
		t.Fatalf("Reconcile() report = %+v, want enqueued explicit repair", report)
	}
	if !capabilityPresent(report.Operation.Capabilities, memory.CapabilityDocumentChunks) {
		t.Fatalf("Capabilities = %+v, want document_chunks selected", report.Operation.Capabilities)
	}
	if len(report.Operation.Documents) != 1 || report.Operation.Documents[0].DatasetID != fixture.scope.DatasetID || report.Operation.Documents[0].DocumentID != "doc-1" {
		t.Fatalf("Documents = %+v, want normalized doc-1 target", report.Operation.Documents)
	}
	if len(report.Operation.Targets) != 1 || report.Operation.Targets[0].DocumentID != "doc-1" {
		t.Fatalf("Targets = %+v, want document lifecycle target", report.Operation.Targets)
	}

	result, err := fixture.mem.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if !result.Completed {
		t.Fatalf("RunOnce() result = %+v, want completed", result)
	}
	stored, ok, err := fixture.reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want stored final report", ok, err)
	}
	if stored.Status != memory.LifecycleStatusCompleted {
		t.Fatalf("final Reconcile status = %q, want completed: %+v", stored.Status, stored)
	}
	if !lifecycleStepPresent(stored.Steps, "projection.document_chunks.hydration") || !lifecycleStepPresent(stored.Steps, "document_chunks.target") {
		t.Fatalf("final Reconcile steps = %+v, want diagnostics and repaired target", stored.Steps)
	}
	if !lifecycleStepPresentWithDiagnosticPhase(stored.Steps, "projection.document_chunks.hydration", "pre-repair") ||
		!lifecycleStepPresentWithDiagnosticPhase(stored.Steps, "projection.document_chunks.hydration", "post-repair") {
		t.Fatalf("final Reconcile steps = %+v, want pre/post repair diagnostics", stored.Steps)
	}
	assertCheckpointBool(t, stored.Checkpoint, "post_repair_ok", true)
	requireDocumentChunkFresh(t, ctx, fixture)
}

func TestReconcileDocumentTargetDryRunPlansRepairWithoutChangingStaleProjection(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	makeProjectedDocumentStale(t, ctx, fixture)
	before := requireDocumentChunkStale(t, ctx, fixture)

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:     fixture.scope,
		Documents: []memory.DocumentTarget{{DocumentID: "doc-1"}},
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("Reconcile(DryRun) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusPlanned || report.JobID != "" {
		t.Fatalf("Reconcile(DryRun) report = %+v, want planned without queued job", report)
	}
	if !lifecycleStepPresent(report.Steps, "projection.document_chunks.hydration") || !lifecycleStepPresent(report.Steps, "document_chunks.target") {
		t.Fatalf("DryRun steps = %+v, want diagnostics and planned target repair", report.Steps)
	}
	if got := report.Operation.IdempotencyKey; got == "" {
		t.Fatalf("IdempotencyKey is empty, want normalized operation key")
	}
	other, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:     fixture.scope,
		Documents: []memory.DocumentTarget{{DocumentID: "doc-2"}},
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("Reconcile(DryRun doc-2) error = %v", err)
	}
	if report.Operation.IdempotencyKey == other.Operation.IdempotencyKey {
		t.Fatalf("IdempotencyKey = %q for different document targets, want different keys", report.Operation.IdempotencyKey)
	}

	after := requireDocumentChunkStale(t, ctx, fixture)
	if before.Details["stale_records"] != after.Details["stale_records"] {
		t.Fatalf("stale records changed after dry-run: before %+v after %+v", before.Details, after.Details)
	}
}

func TestReconcileAutoRepairDryRunPlansAffectedDocumentChunksFromDiagnosticsPage(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	makeProjectedDocumentStale(t, ctx, fixture)
	before := requireDocumentChunkStale(t, ctx, fixture)

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:      fixture.scope,
		AutoRepair: true,
		DryRun:     true,
		PageSize:   10,
	})
	if err != nil {
		t.Fatalf("Reconcile(AutoRepair DryRun) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusPlanned || report.JobID != "" {
		t.Fatalf("Reconcile(AutoRepair DryRun) report = %+v, want planned without queued job", report)
	}
	if len(report.Operation.Documents) != 1 || report.Operation.Documents[0].DocumentID != "doc-1" {
		t.Fatalf("AutoRepair DryRun documents = %+v, want affected doc-1", report.Operation.Documents)
	}
	if !capabilityPresent(report.Operation.Capabilities, memory.CapabilityDocumentChunks) {
		t.Fatalf("AutoRepair DryRun capabilities = %+v, want document_chunks", report.Operation.Capabilities)
	}
	if !lifecycleStepPresent(report.Steps, "projection.document_chunks.staleness") || !lifecycleStepPresent(report.Steps, "document_chunks.target") {
		t.Fatalf("AutoRepair DryRun steps = %+v, want diagnostics and planned document repair", report.Steps)
	}
	assertCheckpointString(t, report.Checkpoint, "repair_targets_source", "diagnostics_page")
	assertCheckpointInt(t, report.Checkpoint, "repair_target_count", 1)

	after := requireDocumentChunkStale(t, ctx, fixture)
	if before.Details["stale_records"] != after.Details["stale_records"] {
		t.Fatalf("stale records changed after auto-repair dry-run: before %+v after %+v", before.Details, after.Details)
	}
}

func TestReconcileAutoRepairRepairsAffectedDocumentChunksFromDiagnosticsPage(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	makeProjectedDocumentStale(t, ctx, fixture)
	requireDocumentChunkStale(t, ctx, fixture)

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:      fixture.scope,
		AutoRepair: true,
		PageSize:   10,
	})
	if err != nil {
		t.Fatalf("Reconcile(AutoRepair) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued || report.JobID == "" {
		t.Fatalf("Reconcile(AutoRepair) report = %+v, want enqueued auto repair", report)
	}
	assertCheckpointString(t, report.Checkpoint, "repair_targets_source", "diagnostics_page")

	result, err := fixture.mem.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce(AutoRepair) error = %v", err)
	}
	if !result.Completed {
		t.Fatalf("RunOnce(AutoRepair) result = %+v, want completed", result)
	}
	stored, ok, err := fixture.reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want stored final report", ok, err)
	}
	if stored.Status != memory.LifecycleStatusCompleted {
		t.Fatalf("final AutoRepair status = %q, want completed: %+v", stored.Status, stored)
	}
	if len(stored.Operation.Documents) != 1 || stored.Operation.Documents[0].DocumentID != "doc-1" {
		t.Fatalf("final AutoRepair documents = %+v, want bounded affected doc-1", stored.Operation.Documents)
	}
	if !lifecycleStepPresentWithDiagnosticPhase(stored.Steps, "projection.document_chunks.staleness", "pre-repair") ||
		!lifecycleStepPresentWithDiagnosticPhase(stored.Steps, "projection.document_chunks.staleness", "post-repair") ||
		!lifecycleStepPresent(stored.Steps, "document_chunks.target") {
		t.Fatalf("final AutoRepair steps = %+v, want pre/repair/post document_chunks flow", stored.Steps)
	}
	assertCheckpointString(t, stored.Checkpoint, "repair_targets_source", "diagnostics_page")
	assertCheckpointBool(t, stored.Checkpoint, "post_repair_ok", true)
	requireDocumentChunkFresh(t, ctx, fixture)
}

func TestReconcileAutoRepairUsesOnlyCurrentDiagnosticsPage(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	importProjectedDocument(t, ctx, fixture, "doc-2", "Babbage filed a second page of notes.")
	makeProjectedDocumentStale(t, ctx, fixture)
	makeProjectedDocumentStaleByID(t, ctx, fixture, "doc-2", "Babbage rewrote the second page of notes.")
	requireDocumentChunkStaleCount(t, ctx, fixture, 2)

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:      fixture.scope,
		AutoRepair: true,
		PageSize:   1,
	})
	if err != nil {
		t.Fatalf("Reconcile(AutoRepair PageSize=1) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued || report.JobID == "" {
		t.Fatalf("Reconcile(AutoRepair PageSize=1) report = %+v, want enqueued first-page repair", report)
	}
	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce(AutoRepair first page) result = %+v error = %v, want completed first-page repair", result, err)
	}
	firstStored, ok, err := fixture.reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport(first page) ok = %v err = %v, want stored final report", ok, err)
	}
	if firstStored.Status != memory.LifecycleStatusCompleted {
		t.Fatalf("final first-page AutoRepair status = %q, want completed: %+v", firstStored.Status, firstStored)
	}
	if len(firstStored.Operation.Documents) != 1 {
		t.Fatalf("final first-page AutoRepair documents = %+v, want exactly one current-page target", firstStored.Operation.Documents)
	}
	firstDocID := firstStored.Operation.Documents[0].DocumentID
	assertCheckpointString(t, firstStored.Checkpoint, "repair_targets_source", "diagnostics_page")
	assertCheckpointInt(t, firstStored.Checkpoint, "repair_target_count", 1)
	nextPageToken, ok := firstStored.Checkpoint["pre_repair_next_page_token"].(string)
	if !ok || nextPageToken == "" {
		t.Fatalf("final first-page AutoRepair checkpoint = %+v, want pre_repair_next_page_token from bounded diagnostics page", firstStored.Checkpoint)
	}
	requireDocumentChunkStaleCount(t, ctx, fixture, 1)

	second, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:      fixture.scope,
		AutoRepair: true,
		PageSize:   1,
		PageToken:  nextPageToken,
	})
	if err != nil {
		t.Fatalf("Reconcile(AutoRepair PageSize=1 second page) error = %v", err)
	}
	if second.Status != memory.LifecycleStatusEnqueued || second.JobID == "" {
		t.Fatalf("Reconcile(AutoRepair PageSize=1 second page) report = %+v, want enqueued second-page repair", second)
	}
	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce(AutoRepair second page) result = %+v error = %v, want completed second-page repair", result, err)
	}
	secondStored, ok, err := fixture.reportStore.GetLifecycleReport(ctx, second.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport(second page) ok = %v err = %v, want stored final report", ok, err)
	}
	if secondStored.Status != memory.LifecycleStatusCompleted {
		t.Fatalf("final second-page AutoRepair status = %q, want completed: %+v", secondStored.Status, secondStored)
	}
	if secondStored.Operation.PageToken != nextPageToken {
		t.Fatalf("final second-page AutoRepair PageToken = %q, want first page token %q", secondStored.Operation.PageToken, nextPageToken)
	}
	if len(secondStored.Operation.Documents) != 1 {
		t.Fatalf("final second-page AutoRepair documents = %+v, want exactly one second-page target", secondStored.Operation.Documents)
	}
	secondDocID := secondStored.Operation.Documents[0].DocumentID
	if secondDocID == firstDocID {
		t.Fatalf("second AutoRepair document = %q, want a different second-page target than first page", secondDocID)
	}
	assertCheckpointString(t, secondStored.Checkpoint, "repair_targets_source", "diagnostics_page")
	assertCheckpointInt(t, secondStored.Checkpoint, "repair_target_count", 1)
	requireDocumentChunkStaleCount(t, ctx, fixture, 0)
}

func TestReconcileAutoRepairPrefersExplicitDocuments(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	importProjectedDocument(t, ctx, fixture, "doc-2", "Babbage filed a second page of notes.")
	makeProjectedDocumentStale(t, ctx, fixture)
	makeProjectedDocumentStaleByID(t, ctx, fixture, "doc-2", "Babbage rewrote the second page of notes.")
	requireDocumentChunkStaleCount(t, ctx, fixture, 2)

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:      fixture.scope,
		Documents:  []memory.DocumentTarget{{DocumentID: "doc-1"}},
		AutoRepair: true,
		PageSize:   10,
	})
	if err != nil {
		t.Fatalf("Reconcile(AutoRepair explicit Documents) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued || report.JobID == "" {
		t.Fatalf("Reconcile(AutoRepair explicit Documents) report = %+v, want enqueued explicit repair", report)
	}
	if len(report.Operation.Documents) != 1 || report.Operation.Documents[0].DocumentID != "doc-1" {
		t.Fatalf("explicit AutoRepair documents = %+v, want only explicit doc-1", report.Operation.Documents)
	}
	assertCheckpointString(t, report.Checkpoint, "repair_targets_source", "explicit")

	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce(AutoRepair explicit Documents) result = %+v error = %v, want completed explicit repair", result, err)
	}
	stored, ok, err := fixture.reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want stored final report", ok, err)
	}
	if len(stored.Operation.Documents) != 1 || stored.Operation.Documents[0].DocumentID != "doc-1" {
		t.Fatalf("final explicit AutoRepair documents = %+v, want only explicit doc-1", stored.Operation.Documents)
	}
	assertCheckpointString(t, stored.Checkpoint, "repair_targets_source", "explicit")
	requireDocumentChunkStaleCount(t, ctx, fixture, 1)
}

func TestReconcileDryRunWithoutDocumentsRunsDiagnosticsOnly(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	makeProjectedDocumentStale(t, ctx, fixture)

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:  fixture.scope,
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("Reconcile(DryRun no documents) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusPlanned || report.JobID != "" {
		t.Fatalf("Reconcile(DryRun no documents) report = %+v, want planned without queued job", report)
	}
	if !lifecycleStepPresent(report.Steps, "diagnostics.stage.consistency") || !lifecycleStepPresent(report.Steps, "projection.document_chunks.hydration") {
		t.Fatalf("DryRun no-doc steps = %+v, want consistency diagnostics checks", report.Steps)
	}
	if lifecycleStepPresent(report.Steps, "lifecycle_substrate") || strings.Contains(report.Message, "no runner registered yet") {
		t.Fatalf("DryRun no-doc report = %+v, want diagnostics-only report without substrate placeholder", report)
	}
	if lifecycleStepPresent(report.Steps, "document_chunks.target") {
		t.Fatalf("DryRun no-doc steps = %+v, want no repair target without Documents", report.Steps)
	}
}

func TestReconcileWithoutDocumentsRunsDiagnosticsOnly(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	makeProjectedDocumentStale(t, ctx, fixture)

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:        fixture.scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued {
		t.Fatalf("Reconcile() status = %q, want enqueued diagnostics job", report.Status)
	}
	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want diagnostics-only completion", result, err)
	}
	stored, ok, err := fixture.reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want stored final report", ok, err)
	}
	if lifecycleStepPresent(stored.Steps, "document_chunks.target") {
		t.Fatalf("Reconcile steps = %+v, want no repair target without Documents", stored.Steps)
	}
	requireDocumentChunkStale(t, ctx, fixture)
}

func TestReconcileDocumentTargetRepairFailsWhenPostDiagnosticsStillError(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedDocumentChunkFixture(t, ctx)
	makeProjectedDocumentStale(t, ctx, fixture)
	if err := fixture.index.Upsert(ctx, fixture.namespace, []retrieval.Doc{danglingDocumentChunkProjectionDoc(fixture.scope)}); err != nil {
		t.Fatalf("Upsert dangling projection doc error = %v", err)
	}

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:     fixture.scope,
		Documents: []memory.DocumentTarget{{DocumentID: "doc-1"}},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued {
		t.Fatalf("Reconcile() report = %+v, want enqueued repair", report)
	}
	result, err := fixture.mem.RunOnce(ctx)
	if err == nil || result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want post diagnostics failure", result, err)
	}
	stored, ok, storeErr := fixture.reportStore.GetLifecycleReport(ctx, report.TraceID)
	if storeErr != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want stored final report", ok, storeErr)
	}
	if stored.Status != memory.LifecycleStatusFailed {
		t.Fatalf("final Reconcile status = %q, want failed: %+v", stored.Status, stored)
	}
	if !lifecycleStepPresentWithDiagnosticPhase(stored.Steps, "projection.document_chunks.hydration", "pre-repair") ||
		!lifecycleFailedStepPresentWithDiagnosticPhase(stored.Steps, "projection.document_chunks.hydration", "post-repair") {
		t.Fatalf("final Reconcile steps = %+v, want pre diagnostics and failed post diagnostics", stored.Steps)
	}
	if !lifecycleStepPresent(stored.Steps, "document_chunks.target") {
		t.Fatalf("final Reconcile steps = %+v, want repaired explicit target before post failure", stored.Steps)
	}
	assertCheckpointBool(t, stored.Checkpoint, "post_repair_ok", false)
}

func TestReconcileMessageIndexCapabilityRepairsMissingProjection(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedMessageFixture(t, ctx)
	if err := fixture.index.Delete(ctx, fixture.namespace, []string{fixture.recordID}); err != nil {
		t.Fatalf("Delete message projection error = %v", err)
	}

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:        fixture.scope,
		Capabilities: []memory.Capability{memory.CapabilityMessageIndex},
	})
	if err != nil {
		t.Fatalf("Reconcile(message_index) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued || report.JobID == "" {
		t.Fatalf("Reconcile(message_index) report = %+v, want enqueued repair", report)
	}
	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want completed message_index repair", result, err)
	}

	stored, ok, err := fixture.reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
	}
	if stored.Status != memory.LifecycleStatusCompleted {
		t.Fatalf("final Reconcile status = %q, want completed: %+v", stored.Status, stored)
	}
	if !lifecycleStepPresent(stored.Steps, "message_index.conversation") ||
		!lifecycleStepPresentWithDiagnosticPhase(stored.Steps, "projection.message_index.hydration", "pre-repair") ||
		!lifecycleStepPresentWithDiagnosticPhase(stored.Steps, "projection.message_index.hydration", "post-repair") {
		t.Fatalf("final Reconcile steps = %+v, want pre/repair/post message_index steps", stored.Steps)
	}
	if !checkpointStringSliceContains(stored.Checkpoint, "repaired_capabilities", string(memory.CapabilityMessageIndex)) {
		t.Fatalf("checkpoint = %+v, want repaired message_index capability", stored.Checkpoint)
	}
	requireMessageIndexSearchableAndConsistent(t, ctx, fixture)
}

func TestReconcileSummaryDAGCapabilityRepairsMissingProjection(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedSummaryFixture(t, ctx)
	record, err := projectors.SummaryNode(fixture.node)
	if err != nil {
		t.Fatalf("SummaryNode() error = %v", err)
	}
	if err := fixture.index.Delete(ctx, fixture.namespace, []string{record.ID}); err != nil {
		t.Fatalf("Delete summary projection error = %v", err)
	}

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:        fixture.scope,
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
	})
	if err != nil {
		t.Fatalf("Reconcile(summary_dag) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued || report.JobID == "" {
		t.Fatalf("Reconcile(summary_dag) report = %+v, want enqueued repair", report)
	}
	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want completed summary_dag repair", result, err)
	}

	stored, ok, err := fixture.reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
	}
	if stored.Status != memory.LifecycleStatusCompleted {
		t.Fatalf("final Reconcile status = %q, want completed: %+v", stored.Status, stored)
	}
	if !lifecycleStepPresent(stored.Steps, "summary_dag.conversation") ||
		!lifecycleStepPresentWithDiagnosticPhase(stored.Steps, "projection.summary_dag.hydration", "pre-repair") ||
		!lifecycleStepPresentWithDiagnosticPhase(stored.Steps, "projection.summary_dag.hydration", "post-repair") {
		t.Fatalf("final Reconcile steps = %+v, want pre/repair/post summary_dag steps", stored.Steps)
	}
	if !checkpointStringSliceContains(stored.Checkpoint, "repaired_capabilities", string(memory.CapabilitySummaryDAG)) {
		t.Fatalf("checkpoint = %+v, want repaired summary_dag capability", stored.Checkpoint)
	}
	step := requireLifecycleStep(t, stored.Steps, "summary_dag.conversation")
	if got, want := step.Details["stale_store_cleanup"], "delete_node"; got != want {
		t.Fatalf("stale_store_cleanup = %v, want %q; step=%+v", got, want, step)
	}
	assertStepDetailInt(t, step, "canonical_stale_candidates", 1)
	assertStepDetailInt(t, step, "canonical_stale_deleted", 1)
	assertCheckpointString(t, stored.Checkpoint, "canonical_cleanup_mode", "delete_node")
	if _, ok, err := fixture.summaryStore.GetNode(ctx, fixture.scope, fixture.node.ID); err != nil || ok {
		t.Fatalf("stale canonical summary after reconcile ok = %v err = %v, want deleted", ok, err)
	}
	if _, ok, err := fixture.summaryStore.GetNode(ctx, fixture.scope, recent.NodeID("summary-"+fixture.scope.ConversationID)); err != nil || !ok {
		t.Fatalf("current canonical summary after reconcile ok = %v err = %v, want retained", ok, err)
	}
	requireSummaryDAGSearchableAndConsistent(t, ctx, fixture)
}

func TestReconcileEmptyCapabilitiesRepairsConversationDerivedViews(t *testing.T) {
	ctx := context.Background()
	fixture := newProjectedConversationFixture(t, ctx)
	if err := fixture.index.Delete(ctx, fixture.messageNamespace, []string{fixture.messageRecordID}); err != nil {
		t.Fatalf("Delete message projection error = %v", err)
	}
	if err := fixture.index.Delete(ctx, fixture.summaryNamespace, []string{fixture.summaryRecordID}); err != nil {
		t.Fatalf("Delete summary projection error = %v", err)
	}

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{Scope: fixture.scope})
	if err != nil {
		t.Fatalf("Reconcile(empty capabilities) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued || report.JobID == "" {
		t.Fatalf("Reconcile(empty capabilities) report = %+v, want enqueued repair", report)
	}
	if !capabilityPresent(report.Operation.Capabilities, memory.CapabilityMessageIndex) ||
		!capabilityPresent(report.Operation.Capabilities, memory.CapabilitySummaryDAG) {
		t.Fatalf("Capabilities = %+v, want message_index and summary_dag auto-selected", report.Operation.Capabilities)
	}
	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want completed conversation repair", result, err)
	}

	stored, ok, err := fixture.reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
	}
	if stored.Status != memory.LifecycleStatusCompleted {
		t.Fatalf("final Reconcile status = %q, want completed: %+v", stored.Status, stored)
	}
	if !lifecycleStepPresent(stored.Steps, "message_index.conversation") ||
		!lifecycleStepPresent(stored.Steps, "summary_dag.conversation") ||
		!lifecycleStepPresentWithDiagnosticPhase(stored.Steps, "projection.message_index.hydration", "pre-repair") ||
		!lifecycleStepPresentWithDiagnosticPhase(stored.Steps, "projection.summary_dag.hydration", "pre-repair") ||
		!lifecycleStepPresentWithDiagnosticPhase(stored.Steps, "projection.message_index.hydration", "post-repair") ||
		!lifecycleStepPresentWithDiagnosticPhase(stored.Steps, "projection.summary_dag.hydration", "post-repair") {
		t.Fatalf("final Reconcile steps = %+v, want pre/repair/post conversation repair steps", stored.Steps)
	}
	assertCheckpointBool(t, stored.Checkpoint, "post_repair_ok", true)
	assertCheckpointInt(t, stored.Checkpoint, "repair_capability_count", 2)
	if !checkpointStringSliceContains(stored.Checkpoint, "repaired_capabilities", string(memory.CapabilityMessageIndex)) ||
		!checkpointStringSliceContains(stored.Checkpoint, "repaired_capabilities", string(memory.CapabilitySummaryDAG)) {
		t.Fatalf("checkpoint = %+v, want both repaired conversation capabilities", stored.Checkpoint)
	}

	requireMessageIndexSearchableAndConsistent(t, ctx, projectedMessageFixture{
		mem:         fixture.mem,
		index:       fixture.index,
		reportStore: fixture.reportStore,
		scope:       fixture.scope,
		namespace:   fixture.messageNamespace,
		message:     fixture.message,
		recordID:    fixture.messageRecordID,
	})
	requireSummaryDAGSearchableAndConsistent(t, ctx, projectedSummaryFixture{
		mem:          fixture.mem,
		index:        fixture.index,
		summaryStore: fixture.summaryStore,
		reportStore:  fixture.reportStore,
		scope:        fixture.scope,
		namespace:    fixture.summaryNamespace,
		node:         fixture.summaryNode,
	})
}

func TestReconcileMessageIndexRepairRebuildsAcrossBatchBoundary(t *testing.T) {
	ctx := context.Background()
	const messageCount = 513 // lifecycleMessageIndexBatchSize + 1.
	fixture := newMessageIndexBatchFixture(t, ctx, messageCount)

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:        fixture.scope,
		Capabilities: []memory.Capability{memory.CapabilityMessageIndex},
	})
	if err != nil {
		t.Fatalf("Reconcile(message_index batch) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued || report.JobID == "" {
		t.Fatalf("Reconcile(message_index batch) report = %+v, want enqueued repair", report)
	}
	if result, err := fixture.mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want completed batch repair", result, err)
	}

	stored, ok, err := fixture.reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
	}
	if stored.Status != memory.LifecycleStatusCompleted {
		t.Fatalf("final Reconcile status = %q, want completed: %+v", stored.Status, stored)
	}
	step := requireLifecycleStep(t, stored.Steps, "message_index.conversation")
	assertStepDetailInt(t, step, "message_count", messageCount)
	assertCheckpointBool(t, stored.Checkpoint, "post_repair_ok", true)

	docs := listProjectionDocs(t, ctx, fixture.index, fixture.namespace)
	if len(docs) != messageCount {
		t.Fatalf("message projection doc count = %d, want %d", len(docs), messageCount)
	}
	seen := make(map[string]bool, len(docs))
	for _, doc := range docs {
		id, _ := doc.Metadata[projectors.MetadataMessageIDKey].(string)
		if id == "" {
			t.Fatalf("message projection doc %q metadata = %+v, missing message id", doc.ID, doc.Metadata)
		}
		if seen[id] {
			t.Fatalf("message projection contains duplicate message id %q", id)
		}
		seen[id] = true
	}
	for _, id := range fixture.messageIDs {
		if !seen[id] {
			t.Fatalf("message projection missing id %q across batch boundary", id)
		}
	}
}

func TestReconcileSummaryDAGRepairFailureKeepsExistingSummaryAndProjection(t *testing.T) {
	ctx := context.Background()
	summarizer := &failingSummaryLifecycleSummarizer{err: errors.New("summarizer boom")}
	fixture := newProjectedSummaryFixtureWithSummarizer(t, ctx, summarizer)
	record, err := projectors.SummaryNode(fixture.node)
	if err != nil {
		t.Fatalf("SummaryNode() error = %v", err)
	}

	report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:        fixture.scope,
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
	})
	if err != nil {
		t.Fatalf("Reconcile(summary_dag failure) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued || report.JobID == "" {
		t.Fatalf("Reconcile(summary_dag failure) report = %+v, want enqueued repair", report)
	}
	if result, err := fixture.mem.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), summarizer.err.Error()) || result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want summarizer failure", result, err)
	}

	stored, ok, err := fixture.reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
	}
	if stored.Status != memory.LifecycleStatusFailed || !strings.Contains(stored.Message, "reconcile repair failed") {
		t.Fatalf("final Reconcile report = %+v, want failed repair message", stored)
	}
	step := requireLifecycleStep(t, stored.Steps, "summary_dag.conversation")
	if step.Status != memory.LifecycleStatusFailed || !strings.Contains(step.Details["error"].(string), summarizer.err.Error()) {
		t.Fatalf("summary_dag repair step = %+v, want failed step with summarizer error", step)
	}
	assertCheckpointInt(t, stored.Checkpoint, "repair_failed", 1)
	if !checkpointStringSliceContains(stored.Checkpoint, "failed_capabilities", string(memory.CapabilitySummaryDAG)) {
		t.Fatalf("checkpoint = %+v, want failed summary_dag capability", stored.Checkpoint)
	}
	nodes, err := fixture.summaryStore.ListNodes(ctx, fixture.scope, recent.ListOptions{})
	if err != nil {
		t.Fatalf("ListNodes() error = %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != fixture.node.ID || nodes[0].Summary != fixture.node.Summary {
		t.Fatalf("Summary nodes after failed reconcile = %+v, want old node retained", nodes)
	}
	if ok, err := projectionDocExists(ctx, fixture.index, fixture.namespace, record.ID); err != nil || !ok {
		t.Fatalf("old summary projection after failed reconcile ok = %v err = %v, want retained", ok, err)
	}
}

func TestReconcileDryRunPlansConversationRepairsWithoutChangingProjection(t *testing.T) {
	ctx := context.Background()

	t.Run("message_index", func(t *testing.T) {
		fixture := newProjectedMessageFixture(t, ctx)
		if err := fixture.index.Delete(ctx, fixture.namespace, []string{fixture.recordID}); err != nil {
			t.Fatalf("Delete message projection error = %v", err)
		}
		report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
			Scope:  fixture.scope,
			DryRun: true,
		})
		if err != nil {
			t.Fatalf("Reconcile(message_index dry-run) error = %v", err)
		}
		if report.Status != memory.LifecycleStatusPlanned || report.JobID != "" {
			t.Fatalf("Reconcile(message_index dry-run) report = %+v, want planned without job", report)
		}
		if !capabilityPresent(report.Operation.Capabilities, memory.CapabilityMessageIndex) ||
			!lifecycleStepPresent(report.Steps, "message_index.conversation") {
			t.Fatalf("DryRun message_index report = %+v, want derived message repair plan", report)
		}
		if ok, err := projectionDocExists(ctx, fixture.index, fixture.namespace, fixture.recordID); err != nil || ok {
			t.Fatalf("message projection after dry-run ok = %v err = %v, want still missing", ok, err)
		}
	})

	t.Run("summary_dag", func(t *testing.T) {
		fixture := newProjectedSummaryFixture(t, ctx)
		record, err := projectors.SummaryNode(fixture.node)
		if err != nil {
			t.Fatalf("SummaryNode() error = %v", err)
		}
		if err := fixture.index.Delete(ctx, fixture.namespace, []string{record.ID}); err != nil {
			t.Fatalf("Delete summary projection error = %v", err)
		}
		report, err := fixture.mem.Reconcile(ctx, memory.ReconcileRequest{
			Scope:  fixture.scope,
			DryRun: true,
		})
		if err != nil {
			t.Fatalf("Reconcile(summary_dag dry-run) error = %v", err)
		}
		if report.Status != memory.LifecycleStatusPlanned || report.JobID != "" {
			t.Fatalf("Reconcile(summary_dag dry-run) report = %+v, want planned without job", report)
		}
		if !capabilityPresent(report.Operation.Capabilities, memory.CapabilitySummaryDAG) ||
			!lifecycleStepPresent(report.Steps, "summary_dag.conversation") {
			t.Fatalf("DryRun summary_dag report = %+v, want derived summary repair plan", report)
		}
		if ok, err := projectionDocExists(ctx, fixture.index, fixture.namespace, record.ID); err != nil || ok {
			t.Fatalf("summary projection after dry-run ok = %v err = %v, want still missing", ok, err)
		}
	})
}

func TestReconcileConversationRepairsRequireConversationScope(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	reportStore := memory.NewMemoryReportStore()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{
			{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true},
			{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true},
		},
	}, memory.Deps{
		MessageStore: sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message")),
		SummaryStore: recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag")),
		Summarizer:   &recordingSummaryLifecycleSummarizer{},
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
		ReportStore:  reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	report, err := mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope: memory.Scope{RuntimeID: "rt", UserID: "user"},
		Capabilities: []memory.Capability{
			memory.CapabilityMessageIndex,
			memory.CapabilitySummaryDAG,
		},
	})
	if err != nil {
		t.Fatalf("Reconcile(no conversation) error = %v", err)
	}
	if report.Status != memory.LifecycleStatusEnqueued {
		t.Fatalf("Reconcile(no conversation) report = %+v, want enqueued skipped repair job", report)
	}
	if result, err := mem.RunOnce(ctx); err != nil || !result.Completed {
		t.Fatalf("RunOnce() result = %+v error = %v, want skipped repair completion", result, err)
	}
	stored, ok, err := reportStore.GetLifecycleReport(ctx, report.TraceID)
	if err != nil || !ok {
		t.Fatalf("GetLifecycleReport() ok = %v err = %v, want final report", ok, err)
	}
	if stored.Status != memory.LifecycleStatusSkipped {
		t.Fatalf("final Reconcile status = %q, want skipped: %+v", stored.Status, stored)
	}
	if !lifecycleSkippedStepPresent(stored.Steps, "message_index.conversation") ||
		!lifecycleSkippedStepPresent(stored.Steps, "summary_dag.conversation") {
		t.Fatalf("final Reconcile steps = %+v, want skipped conversation repair steps", stored.Steps)
	}
	if checkpointStringSliceContains(stored.Checkpoint, "repaired_capabilities", string(memory.CapabilityMessageIndex)) ||
		checkpointStringSliceContains(stored.Checkpoint, "repaired_capabilities", string(memory.CapabilitySummaryDAG)) {
		t.Fatalf("checkpoint = %+v, want no repaired conversation capabilities", stored.Checkpoint)
	}
	if !checkpointStringSliceContains(stored.Checkpoint, "skipped_capabilities", string(memory.CapabilityMessageIndex)) ||
		!checkpointStringSliceContains(stored.Checkpoint, "skipped_capabilities", string(memory.CapabilitySummaryDAG)) {
		t.Fatalf("checkpoint = %+v, want skipped conversation capabilities", stored.Checkpoint)
	}
}

func TestDiagnosticProjectionPaginationUsesCompositeTokenPerCapability(t *testing.T) {
	ctx := context.Background()
	fixture := newDualProjectionFixture(t, ctx)

	first, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope: fixture.scope,
		Stage: "freshness",
		Capabilities: []memory.Capability{
			memory.CapabilityMessageIndex,
			memory.CapabilityDocumentChunks,
		},
		PageSize: 1,
	})
	if err != nil {
		t.Fatalf("Diagnostics(first) error = %v", err)
	}
	if !strings.HasPrefix(first.NextPageToken, "diagproj:v1:") {
		t.Fatalf("first NextPageToken = %q, want diagnostics composite token", first.NextPageToken)
	}
	firstMessage := requireDiagnosticCheck(t, first.Checks, "projection.message_index.freshness_metadata")
	firstDocument := requireDiagnosticCheck(t, first.Checks, "projection.document_chunks.freshness_metadata")
	if !first.Ready || !firstMessage.OK || !firstDocument.OK {
		t.Fatalf("first diagnostics = %+v, checks = %+v / %+v, want ready pagination checks", first, firstMessage, firstDocument)
	}
	assertDiagnosticDetailInt(t, firstMessage, "records_scanned", 1)
	assertDiagnosticDetailInt(t, firstDocument, "records_scanned", 1)
	assertDiagnosticDetailString(t, firstMessage, "namespace", fixture.messageNamespace)
	assertDiagnosticDetailString(t, firstDocument, "namespace", fixture.documentNamespace)

	second, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope: fixture.scope,
		Stage: "freshness",
		Capabilities: []memory.Capability{
			memory.CapabilityMessageIndex,
			memory.CapabilityDocumentChunks,
		},
		PageSize:  1,
		PageToken: first.NextPageToken,
	})
	if err != nil {
		t.Fatalf("Diagnostics(second) error = %v", err)
	}
	secondMessage := requireDiagnosticCheck(t, second.Checks, "projection.message_index.freshness_metadata")
	secondDocument := requireDiagnosticCheck(t, second.Checks, "projection.document_chunks.freshness_metadata")
	if !second.Ready || !secondMessage.OK || !secondDocument.OK {
		t.Fatalf("second diagnostics = %+v, checks = %+v / %+v, want ready pagination checks", second, secondMessage, secondDocument)
	}
	if second.NextPageToken != "" {
		t.Fatalf("second NextPageToken = %q, want pagination complete", second.NextPageToken)
	}
	assertDiagnosticDetailInt(t, secondMessage, "records_scanned", 1)
	assertDiagnosticDetailInt(t, secondDocument, "records_scanned", 1)
	assertDiagnosticDetailString(t, secondMessage, "namespace", fixture.messageNamespace)
	assertDiagnosticDetailString(t, secondDocument, "namespace", fixture.documentNamespace)
}

func newMessageStore() sourcemessage.Store {
	return sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(sdkworkspace.NewMemWorkspace(), "sources/message"))
}

func newDocumentChunkSystem() (*memory.System, error) {
	root := sdkworkspace.NewMemWorkspace()
	return memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceDocumentStore, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityDocumentChunks, Required: true}},
	}, memory.Deps{
		DocumentStore:   sourcedocument.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/document")),
		ChunkStore:      viewdocument.NewChunkWorkspaceStore(sdkworkspace.Sub(root, "views/document_chunks")),
		DocumentChunker: derivedocument.WholeDocumentChunker{},
	})
}

type projectedDocumentChunkFixture struct {
	mem           *memory.System
	index         retrieval.Index
	documentStore sourcedocument.Store
	chunkStore    viewdocument.ChunkStore
	reportStore   *memory.MemoryReportStore
	scope         memory.Scope
	namespace     string
}

type projectedSummaryFixture struct {
	mem          *memory.System
	index        retrieval.Index
	summaryStore recent.SummaryStore
	reportStore  *memory.MemoryReportStore
	scope        memory.Scope
	namespace    string
	node         recent.SummaryNode
}

type projectedMessageFixture struct {
	mem         *memory.System
	index       retrieval.Index
	reportStore *memory.MemoryReportStore
	scope       memory.Scope
	namespace   string
	message     sourcemessage.Message
	recordID    string
}

type projectedConversationFixture struct {
	mem              *memory.System
	index            retrieval.Index
	summaryStore     recent.SummaryStore
	reportStore      *memory.MemoryReportStore
	scope            memory.Scope
	messageNamespace string
	summaryNamespace string
	message          sourcemessage.Message
	messageRecordID  string
	summaryNode      recent.SummaryNode
	summaryRecordID  string
}

type messageIndexBatchFixture struct {
	mem         *memory.System
	index       retrieval.Index
	reportStore *memory.MemoryReportStore
	scope       memory.Scope
	namespace   string
	messageIDs  []string
}

type dualProjectionFixture struct {
	mem               *memory.System
	scope             memory.Scope
	messageNamespace  string
	documentNamespace string
}

func newProjectedDocumentChunkFixture(t *testing.T, ctx context.Context) projectedDocumentChunkFixture {
	t.Helper()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	documentStore := sourcedocument.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/document"))
	chunkStore := viewdocument.NewChunkWorkspaceStore(sdkworkspace.Sub(root, "views/document_chunks"))
	reportStore := memory.NewMemoryReportStore()
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceDocumentStore, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityDocumentChunks, Required: true}},
		Projections:  []memory.ProjectionSpec{{Capability: memory.CapabilityDocumentChunks, Namespace: "document_chunks", Required: true}},
		WriteStages:  []memory.StageSpec{{Name: "chunk_document"}},
	}, memory.Deps{
		DocumentStore:   documentStore,
		ChunkStore:      chunkStore,
		Index:           index,
		DocumentChunker: derivedocument.WholeDocumentChunker{},
		JobStore:        memory.NewMemoryJobStore(),
		ReportStore:     reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{
		RuntimeID:      "rt",
		UserID:         "user",
		AgentID:        "agent",
		ConversationID: "conv",
		DatasetID:      "dataset",
	}
	if _, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope: scope,
		Document: sourcedocument.Document{
			ID:      "doc-1",
			Content: "Ada stored the field notes in the cedar box.",
		},
	}); err != nil {
		t.Fatalf("ImportDocument() error = %v", err)
	}
	namespace, err := projectors.ScopedNamespace("document_chunks", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace() error = %v", err)
	}
	return projectedDocumentChunkFixture{mem: mem, index: index, documentStore: documentStore, chunkStore: chunkStore, reportStore: reportStore, scope: scope, namespace: namespace}
}

func newProjectedSummaryFixture(t *testing.T, ctx context.Context) projectedSummaryFixture {
	t.Helper()
	return newProjectedSummaryFixtureWithSummarizer(t, ctx, &recordingSummaryLifecycleSummarizer{})
}

func newProjectedSummaryFixtureWithSummarizer(t *testing.T, ctx context.Context, summarizer derive.Summarizer) projectedSummaryFixture {
	t.Helper()
	if summarizer == nil {
		summarizer = &recordingSummaryLifecycleSummarizer{}
	}
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	summaryStore := recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag"))
	reportStore := memory.NewMemoryReportStore()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true}},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_summaries"},
			{Name: "pack_context"},
		},
	}, memory.Deps{
		MessageStore: msgStore,
		SummaryStore: summaryStore,
		Summarizer:   summarizer,
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
		ReportStore:  reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent", ConversationID: "conv"}
	if _, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-1",
			Message: model.NewTextMessage(model.RoleUser, "Ada found a brass lantern."),
		}},
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	node := summaryLifecycleNode(scope, "summary-1", "summary before canonical mutation")
	if _, err := summaryStore.PutNode(ctx, node); err != nil {
		t.Fatalf("PutNode() error = %v", err)
	}
	namespace, err := projectors.ScopedNamespace("summary_dag", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace(summary_dag) error = %v", err)
	}
	record, err := projectors.SummaryNode(node)
	if err != nil {
		t.Fatalf("SummaryNode() error = %v", err)
	}
	writer, err := indexed.NewWriter(index, indexed.Binding{Namespace: namespace})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Upsert(ctx, []indexed.Record{record}); err != nil {
		t.Fatalf("Upsert summary projection error = %v", err)
	}
	return projectedSummaryFixture{mem: mem, index: index, summaryStore: summaryStore, reportStore: reportStore, scope: scope, namespace: namespace, node: node}
}

func newProjectedConversationFixture(t *testing.T, ctx context.Context) projectedConversationFixture {
	t.Helper()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	summaryStore := recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag"))
	reportStore := memory.NewMemoryReportStore()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{
			{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true},
			{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true},
		},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_messages"},
			{Name: "retrieve_summaries"},
			{Name: "pack_context"},
		},
	}, memory.Deps{
		MessageStore: msgStore,
		SummaryStore: summaryStore,
		Summarizer:   &recordingSummaryLifecycleSummarizer{},
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
		ReportStore:  reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent", ConversationID: "conv"}
	messages, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-1",
			Message: model.NewTextMessage(model.RoleUser, "Ada hid the blue notebook beside the lamp and found a brass lantern."),
		}},
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	messageNamespace, err := projectors.ScopedNamespace("message_index", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace(message_index) error = %v", err)
	}
	messageRecord := sourceMessageRecord(t, scope, messages[0])
	summaryNamespace, err := projectors.ScopedNamespace("summary_dag", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace(summary_dag) error = %v", err)
	}
	summaryNode := summaryLifecycleNode(scope, "summary-1", "Conversation summary: Ada found a brass lantern.")
	if _, err := summaryStore.PutNode(ctx, summaryNode); err != nil {
		t.Fatalf("PutNode() error = %v", err)
	}
	summaryRecord, err := projectors.SummaryNode(summaryNode)
	if err != nil {
		t.Fatalf("SummaryNode() error = %v", err)
	}
	messageWriter, err := indexed.NewWriter(index, indexed.Binding{Namespace: messageNamespace})
	if err != nil {
		t.Fatalf("NewWriter(message) error = %v", err)
	}
	if err := messageWriter.Upsert(ctx, []indexed.Record{messageRecord}); err != nil {
		t.Fatalf("Upsert message projection error = %v", err)
	}
	summaryWriter, err := indexed.NewWriter(index, indexed.Binding{Namespace: summaryNamespace})
	if err != nil {
		t.Fatalf("NewWriter(summary) error = %v", err)
	}
	if err := summaryWriter.Upsert(ctx, []indexed.Record{summaryRecord}); err != nil {
		t.Fatalf("Upsert summary projection error = %v", err)
	}
	return projectedConversationFixture{
		mem:              mem,
		index:            index,
		summaryStore:     summaryStore,
		reportStore:      reportStore,
		scope:            scope,
		messageNamespace: messageNamespace,
		summaryNamespace: summaryNamespace,
		message:          messages[0],
		messageRecordID:  messageRecord.ID,
		summaryNode:      summaryNode,
		summaryRecordID:  summaryRecord.ID,
	}
}

func newMessageIndexBatchFixture(t *testing.T, ctx context.Context, count int) messageIndexBatchFixture {
	t.Helper()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	reportStore := memory.NewMemoryReportStore()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true}},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_messages"},
			{Name: "pack_context"},
		},
	}, memory.Deps{
		MessageStore: msgStore,
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
		ReportStore:  reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent", ConversationID: "conv"}
	inputs := make([]sourcemessage.Message, 0, count)
	messageIDs := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		id := "batch-" + strconv.Itoa(i)
		messageIDs = append(messageIDs, id)
		inputs = append(inputs, sourcemessage.Message{
			ID:      id,
			Message: model.NewTextMessage(model.RoleUser, "batch boundary message "+strconv.Itoa(i)),
		})
	}
	if _, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages:       inputs,
	}); err != nil {
		t.Fatalf("Append(batch messages) error = %v", err)
	}
	namespace, err := projectors.ScopedNamespace("message_index", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace(message_index) error = %v", err)
	}
	return messageIndexBatchFixture{
		mem:         mem,
		index:       index,
		reportStore: reportStore,
		scope:       scope,
		namespace:   namespace,
		messageIDs:  messageIDs,
	}
}

func newProjectedMessageFixture(t *testing.T, ctx context.Context) projectedMessageFixture {
	t.Helper()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	msgStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	reportStore := memory.NewMemoryReportStore()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true}},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_messages"},
			{Name: "pack_context"},
		},
	}, memory.Deps{
		MessageStore: msgStore,
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
		ReportStore:  reportStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent", ConversationID: "conv"}
	messages, err := msgStore.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-1",
			Message: model.NewTextMessage(model.RoleUser, "Ada hid the blue notebook beside the lamp."),
		}},
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	namespace, err := projectors.ScopedNamespace("message_index", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace(message_index) error = %v", err)
	}
	record := sourceMessageRecord(t, scope, messages[0])
	writer, err := indexed.NewWriter(index, indexed.Binding{Namespace: namespace})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Upsert(ctx, []indexed.Record{record}); err != nil {
		t.Fatalf("Upsert message projection error = %v", err)
	}
	return projectedMessageFixture{mem: mem, index: index, reportStore: reportStore, scope: scope, namespace: namespace, message: messages[0], recordID: record.ID}
}

func addSummarySourceContentHash(t *testing.T, ctx context.Context, fixture projectedSummaryFixture) {
	t.Helper()
	msg, ok, err := fixture.mem.MessageStore().Get(ctx, fixture.scope.ConversationID, "dia-1")
	if err != nil || !ok {
		t.Fatalf("Get(summary source message) ok = %v err = %v, want existing message", ok, err)
	}
	node := fixture.node
	if len(node.Signature.SourceRevisions) == 0 {
		t.Fatalf("summary node has no source revisions")
	}
	node.Signature.SourceRevisions[0].ContentHash = summaryderive.MessageContentHash(msg)
	if _, err := fixture.summaryStore.PutNode(ctx, node); err != nil {
		t.Fatalf("PutNode(summary with source content hash) error = %v", err)
	}
	record, err := projectors.SummaryNode(node)
	if err != nil {
		t.Fatalf("SummaryNode(summary with source content hash) error = %v", err)
	}
	writer, err := indexed.NewWriter(fixture.index, indexed.Binding{Namespace: fixture.namespace})
	if err != nil {
		t.Fatalf("NewWriter(summary source content hash) error = %v", err)
	}
	if err := writer.Upsert(ctx, []indexed.Record{record}); err != nil {
		t.Fatalf("Upsert summary projection with source content hash error = %v", err)
	}
}

func putProjectedSummaryNode(t *testing.T, ctx context.Context, fixture projectedSummaryFixture, node recent.SummaryNode) {
	t.Helper()
	put, err := fixture.summaryStore.PutNode(ctx, node)
	if err != nil {
		t.Fatalf("PutNode(projected summary) error = %v", err)
	}
	record, err := projectors.SummaryNode(put)
	if err != nil {
		t.Fatalf("SummaryNode(projected summary) error = %v", err)
	}
	writer, err := indexed.NewWriter(fixture.index, indexed.Binding{Namespace: fixture.namespace})
	if err != nil {
		t.Fatalf("NewWriter(projected summary) error = %v", err)
	}
	if err := writer.Upsert(ctx, []indexed.Record{record}); err != nil {
		t.Fatalf("Upsert projected summary error = %v", err)
	}
}

func makeProjectedDocumentStale(t *testing.T, ctx context.Context, fixture projectedDocumentChunkFixture) {
	t.Helper()
	makeProjectedDocumentStaleByID(t, ctx, fixture, "doc-1", "Ada moved the updated field notes into the cedar box.")
}

func importProjectedDocument(t *testing.T, ctx context.Context, fixture projectedDocumentChunkFixture, documentID, content string) {
	t.Helper()
	if _, err := fixture.mem.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope: fixture.scope,
		Document: sourcedocument.Document{
			ID:      documentID,
			Content: content,
		},
	}); err != nil {
		t.Fatalf("ImportDocument(%s) error = %v", documentID, err)
	}
}

func makeProjectedDocumentStaleByID(t *testing.T, ctx context.Context, fixture projectedDocumentChunkFixture, documentID, content string) {
	t.Helper()
	if _, err := fixture.documentStore.Put(ctx, sourcedocument.PutRequest{
		Document: sourcedocument.Document{
			DatasetID: fixture.scope.DatasetID,
			ID:        documentID,
			Content:   content,
		},
	}); err != nil {
		t.Fatalf("Put updated canonical document %s error = %v", documentID, err)
	}
}

func requireDocumentChunkStale(t *testing.T, ctx context.Context, fixture projectedDocumentChunkFixture) memory.DiagnosticCheck {
	t.Helper()
	check := requireDocumentChunkFreshnessCheck(t, ctx, fixture)
	if check.Status != memory.DiagnosticStatusStale || check.Severity != memory.DiagnosticSeverityWarning || check.OK {
		t.Fatalf("staleness check = %+v, want stale warning", check)
	}
	return check
}

func requireDocumentChunkFresh(t *testing.T, ctx context.Context, fixture projectedDocumentChunkFixture) memory.DiagnosticCheck {
	t.Helper()
	check := requireDocumentChunkFreshnessCheck(t, ctx, fixture)
	if !check.OK || check.Status != memory.DiagnosticStatusOK {
		t.Fatalf("staleness check = %+v, want OK", check)
	}
	return check
}

func requireDocumentChunkStaleCount(t *testing.T, ctx context.Context, fixture projectedDocumentChunkFixture, want int) memory.DiagnosticCheck {
	t.Helper()
	check := requireDocumentChunkFreshnessCheck(t, ctx, fixture)
	if want == 0 {
		if !check.OK || check.Status != memory.DiagnosticStatusOK {
			t.Fatalf("staleness check = %+v, want OK", check)
		}
		return check
	}
	if check.Status != memory.DiagnosticStatusStale || check.Severity != memory.DiagnosticSeverityWarning || check.OK {
		t.Fatalf("staleness check = %+v, want stale warning", check)
	}
	assertDiagnosticDetailInt(t, check, "stale_records", want)
	return check
}

func requireDocumentChunkFreshnessCheck(t *testing.T, ctx context.Context, fixture projectedDocumentChunkFixture) memory.DiagnosticCheck {
	t.Helper()
	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "freshness",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(freshness) error = %v", err)
	}
	return requireDiagnosticCheck(t, report.Checks, "projection.document_chunks.staleness")
}

func requireMessageIndexSearchableAndConsistent(t *testing.T, ctx context.Context, fixture projectedMessageFixture) {
	t.Helper()
	pack, err := fixture.mem.PackContext(ctx, memory.ContextRequest{
		Scope: fixture.scope,
		Query: "blue notebook lamp",
		TopK:  3,
	})
	if err != nil {
		t.Fatalf("PackContext(message_index) error = %v", err)
	}
	if len(pack.MessageHits) == 0 || pack.MessageHits[0].Message.ID != fixture.message.ID {
		t.Fatalf("MessageHits = %+v, want repaired searchable message %q", pack.MessageHits, fixture.message.ID)
	}
	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "consistency",
		Capabilities: []memory.Capability{memory.CapabilityMessageIndex},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(message_index consistency) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.message_index.hydration")
	if !report.Ready || !report.OK || !check.OK {
		t.Fatalf("message_index consistency report/check = %+v / %+v, want OK", report, check)
	}
}

func requireSummaryDAGSearchableAndConsistent(t *testing.T, ctx context.Context, fixture projectedSummaryFixture) {
	t.Helper()
	pack, err := fixture.mem.PackContext(ctx, memory.ContextRequest{
		Scope: fixture.scope,
		Query: "lantern",
		TopK:  3,
	})
	if err != nil {
		t.Fatalf("PackContext(summary_dag) error = %v", err)
	}
	if len(pack.SummaryHits) == 0 || !strings.Contains(pack.SummaryHits[0].Node.Summary, "lantern") {
		t.Fatalf("SummaryHits = %+v, want repaired searchable lantern summary", pack.SummaryHits)
	}
	report, err := fixture.mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        fixture.scope,
		Stage:        "consistency",
		Capabilities: []memory.Capability{memory.CapabilitySummaryDAG},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Diagnostics(summary_dag consistency) error = %v", err)
	}
	check := requireDiagnosticCheck(t, report.Checks, "projection.summary_dag.hydration")
	if !report.Ready || !report.OK || !check.OK {
		t.Fatalf("summary_dag consistency report/check = %+v / %+v, want OK", report, check)
	}
}

func projectionDocExists(ctx context.Context, index retrieval.Index, namespace, id string) (bool, error) {
	resp, err := index.List(ctx, namespace, retrieval.ListRequest{
		PageSize: 100,
		OrderBy:  retrieval.OrderByIDAsc,
	})
	if err != nil || resp == nil {
		return false, err
	}
	for _, doc := range resp.Items {
		if doc.ID == id {
			return true, nil
		}
	}
	return false, nil
}

func newDualProjectionFixture(t *testing.T, ctx context.Context) dualProjectionFixture {
	t.Helper()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	documentStore := sourcedocument.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/document"))
	messageStore := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{
			{Kind: memory.SourceMessageLog, Required: true},
			{Kind: memory.SourceDocumentStore, Required: true},
		},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityMessageIndex, Required: true},
			{Capability: memory.CapabilityDocumentChunks, Required: true},
		},
		Projections: []memory.ProjectionSpec{
			{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true},
			{Capability: memory.CapabilityDocumentChunks, Namespace: "document_chunks", Required: true},
		},
	}, memory.Deps{
		MessageStore:  messageStore,
		DocumentStore: documentStore,
		ChunkStore:    viewdocument.NewChunkWorkspaceStore(sdkworkspace.Sub(root, "views/document_chunks")),
		Index:         index,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := memory.Scope{
		RuntimeID:      "rt",
		UserID:         "user",
		AgentID:        "agent",
		ConversationID: "conv",
		DatasetID:      "dataset",
	}
	messageNamespace, err := projectors.ScopedNamespace("message_index", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace(message_index) error = %v", err)
	}
	documentNamespace, err := projectors.ScopedNamespace("document_chunks", scope)
	if err != nil {
		t.Fatalf("ScopedNamespace(document_chunks) error = %v", err)
	}
	for _, id := range []string{"chunk-a", "chunk-b"} {
		if _, err := documentStore.Put(ctx, sourcedocument.PutRequest{
			Document: sourcedocument.Document{
				DatasetID: scope.DatasetID,
				ID:        id,
				Content:   "fresh projection " + id,
			},
		}); err != nil {
			t.Fatalf("Put canonical document %q error = %v", id, err)
		}
	}
	writeFreshMessageProjectionRecords(t, ctx, index, messageNamespace, messageStore, scope, "message", 2)
	writeFreshProjectionRecords(t, ctx, index, documentNamespace, memory.CapabilityDocumentChunks, scope, "chunk", 2)
	return dualProjectionFixture{
		mem:               mem,
		scope:             scope,
		messageNamespace:  messageNamespace,
		documentNamespace: documentNamespace,
	}
}

func writeFreshMessageProjectionRecords(t *testing.T, ctx context.Context, index retrieval.Index, namespace string, store sourcemessage.Store, scope memory.Scope, prefix string, count int) {
	t.Helper()
	inputs := make([]sourcemessage.Message, 0, count)
	for i := range count {
		id := prefix + "-" + string(rune('a'+i))
		inputs = append(inputs, sourcemessage.Message{
			ID:      id,
			Message: model.NewTextMessage(model.RoleUser, "fresh projection "+id),
		})
	}
	messages, err := store.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages:       inputs,
	})
	if err != nil {
		t.Fatalf("Append(%s messages) error = %v", prefix, err)
	}
	records := make([]indexed.Record, 0, len(messages))
	for _, msg := range messages {
		messageRecords, err := projectors.SourceMessageRecords(scope, msg)
		if err != nil {
			t.Fatalf("SourceMessageRecords(%s) error = %v", msg.ID, err)
		}
		for _, record := range messageRecords {
			record.Signature = views.ViewSignature{
				ViewID: views.ID(memory.CapabilityMessageIndex),
				SourceRevisions: []views.SourceRevision{{
					Kind:      views.SourceMessage,
					SourceKey: record.SourceRefs[0].StableKey(),
					Revision:  strconv.FormatUint(msg.Seq, 10),
				}},
				TransformSignature: "test:v1",
			}
			records = append(records, record)
		}
	}
	writer, err := indexed.NewWriter(index, indexed.Binding{Namespace: namespace})
	if err != nil {
		t.Fatalf("NewWriter(%s) error = %v", namespace, err)
	}
	if err := writer.Upsert(ctx, records); err != nil {
		t.Fatalf("Upsert(%s) error = %v", namespace, err)
	}
}

func writeFreshProjectionRecords(t *testing.T, ctx context.Context, index retrieval.Index, namespace string, capability memory.Capability, scope memory.Scope, prefix string, count int) {
	t.Helper()
	writer, err := indexed.NewWriter(index, indexed.Binding{Namespace: namespace})
	if err != nil {
		t.Fatalf("NewWriter(%s) error = %v", namespace, err)
	}
	records := make([]indexed.Record, 0, count)
	for i := range count {
		id := prefix + "-" + string(rune('a'+i))
		ref := diagnosticSourceRef(scope, capability, id)
		records = append(records, indexed.Record{
			ID:         id,
			Text:       "fresh projection " + id,
			Metadata:   diagnosticProjectionMetadata(scope, capability, id, i),
			SourceRefs: []views.SourceRef{ref},
			Signature: views.ViewSignature{
				ViewID: views.ID(capability),
				SourceRevisions: []views.SourceRevision{{
					Kind:      ref.Kind,
					SourceKey: ref.StableKey(),
					Revision:  "1",
				}},
				TransformSignature: "test:v1",
			},
		})
	}
	if err := writer.Upsert(ctx, records); err != nil {
		t.Fatalf("Upsert(%s) error = %v", namespace, err)
	}
}

func diagnosticSourceRef(scope memory.Scope, capability memory.Capability, id string) views.SourceRef {
	switch capability {
	case memory.CapabilityMessageIndex:
		return views.SourceRef{
			Kind: views.SourceMessage,
			Message: &views.MessageSourceRef{
				ConversationID: scope.ConversationID,
				MessageID:      id,
			},
		}
	default:
		return views.SourceRef{
			Kind: views.SourceDocument,
			Document: &views.DocumentSourceRef{
				DatasetID:  scope.DatasetID,
				DocumentID: id,
			},
		}
	}
}

func diagnosticProjectionMetadata(scope memory.Scope, capability memory.Capability, id string, seq int) map[string]any {
	metadata := map[string]any{
		projectors.MetadataRuntimeIDKey: scope.RuntimeID,
		projectors.MetadataUserIDKey:    scope.UserID,
		projectors.MetadataAgentIDKey:   scope.AgentID,
		projectors.MetadataDatasetIDKey: scope.DatasetID,
	}
	switch capability {
	case memory.CapabilityMessageIndex:
		metadata[projectors.MetadataViewKindKey] = "message_index"
		metadata[projectors.MetadataRecordTypeKey] = projectors.RecordTypeSourceMessage
		metadata[projectors.MetadataConversationIDKey] = scope.ConversationID
		metadata[projectors.MetadataMessageIDKey] = id
		metadata[projectors.MetadataMessageSeqKey] = seq
	default:
		metadata[projectors.MetadataViewKindKey] = "document_chunks"
		metadata[projectors.MetadataRecordTypeKey] = projectors.RecordTypeDocumentChunk
		metadata[projectors.MetadataDocumentIDKey] = "doc-" + id
		metadata[projectors.MetadataChunkIDKey] = id
	}
	return metadata
}

func danglingDocumentChunkProjectionDoc(scope memory.Scope) retrieval.Doc {
	return retrieval.Doc{
		ID:      "dangling-chunk",
		Content: "projection whose canonical chunk is missing",
		Metadata: map[string]any{
			projectors.MetadataViewKindKey:       "document_chunks",
			projectors.MetadataRecordTypeKey:     projectors.RecordTypeDocumentChunk,
			projectors.MetadataRuntimeIDKey:      scope.RuntimeID,
			projectors.MetadataUserIDKey:         scope.UserID,
			projectors.MetadataAgentIDKey:        scope.AgentID,
			projectors.MetadataConversationIDKey: scope.ConversationID,
			projectors.MetadataDatasetIDKey:      scope.DatasetID,
			projectors.MetadataDocumentIDKey:     "missing-doc",
			projectors.MetadataChunkIDKey:        "missing-chunk",
		},
	}
}

func plannedStageNames(stages []memory.PlannedStage) map[string]bool {
	names := make(map[string]bool, len(stages))
	for _, stage := range stages {
		names[stage.Name] = true
	}
	return names
}

func hasDiagnosticCheck(checks []memory.DiagnosticCheck, name string) bool {
	for _, check := range checks {
		if check.Name == name {
			return true
		}
	}
	return false
}

func requireReadinessCheck(t *testing.T, checks []memory.ReadinessCheck, name string) memory.ReadinessCheck {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("readiness checks = %+v, want %q", checks, name)
	return memory.ReadinessCheck{}
}

func lifecycleStepPresent(steps []memory.LifecycleStep, name string) bool {
	for _, step := range steps {
		if step.Name == name {
			return true
		}
	}
	return false
}

func requireLifecycleStep(t *testing.T, steps []memory.LifecycleStep, name string) memory.LifecycleStep {
	t.Helper()
	for _, step := range steps {
		if step.Name == name {
			return step
		}
	}
	t.Fatalf("lifecycle steps = %+v, want %q", steps, name)
	return memory.LifecycleStep{}
}

func lifecycleStepPresentWithDiagnosticPhase(steps []memory.LifecycleStep, name, phase string) bool {
	for _, step := range steps {
		if step.Name == name && step.Details["diagnostic_phase"] == phase {
			return true
		}
	}
	return false
}

func lifecycleFailedStepPresentWithDiagnosticPhase(steps []memory.LifecycleStep, name, phase string) bool {
	for _, step := range steps {
		if step.Name == name && step.Status == memory.LifecycleStatusFailed && step.Details["diagnostic_phase"] == phase {
			return true
		}
	}
	return false
}

func lifecycleSkippedStepPresent(steps []memory.LifecycleStep, name string) bool {
	for _, step := range steps {
		if step.Name == name && (step.Skipped || step.Status == memory.LifecycleStatusSkipped) {
			return true
		}
	}
	return false
}

func runLifecycleRequest(ctx context.Context, mem *memory.System, action string, scope memory.Scope, capabilities []memory.Capability) (memory.LifecycleExecutionReport, error) {
	switch action {
	case "rebuild":
		return mem.Rebuild(ctx, memory.RebuildRequest{Scope: scope, Capabilities: capabilities})
	case "reload":
		return mem.Reload(ctx, memory.ReloadRequest{Scope: scope, Capabilities: capabilities})
	default:
		return memory.LifecycleExecutionReport{}, errdefs.Validationf("unknown lifecycle action %q", action)
	}
}

func newLifecycleSelectionSystem(t *testing.T, includeSummary bool) (*memory.System, error) {
	t.Helper()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = index.Close() })
	spec := memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true}},
	}
	deps := memory.Deps{
		MessageStore: sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message")),
		Index:        index,
		JobStore:     memory.NewMemoryJobStore(),
	}
	if includeSummary {
		spec.Capabilities = append(spec.Capabilities, memory.CapabilitySpec{Capability: memory.CapabilitySummaryDAG, Required: true})
		spec.Projections = append(spec.Projections, memory.ProjectionSpec{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true})
		deps.SummaryStore = recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag"))
		deps.Summarizer = &recordingSummaryLifecycleSummarizer{}
	}
	mem, err := memory.New(spec, deps)
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	return mem, nil
}

type recordingSummaryLifecycleSummarizer struct {
	calls int
}

func (s *recordingSummaryLifecycleSummarizer) Summarize(_ context.Context, input derive.SummaryInput) ([]recent.SummaryNode, error) {
	s.calls++
	if len(input.Window.Messages) == 0 || len(input.Window.SourceRefs) == 0 {
		return nil, nil
	}
	sourceRefs := append([]views.SourceRef(nil), input.Window.SourceRefs...)
	revisions := make([]views.SourceRevision, 0, len(sourceRefs))
	for _, ref := range sourceRefs {
		key, err := ref.StableKeyE()
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, views.SourceRevision{
			Kind:      views.SourceMessage,
			SourceKey: key,
			Revision:  "1",
		})
	}
	return []recent.SummaryNode{{
		ID:         recent.NodeID("summary-" + input.Scope.ConversationID),
		Scope:      input.Scope,
		SourceRefs: sourceRefs,
		Summary:    "Conversation summary: Ada found a brass lantern.",
		Level:      0,
		Signature: views.ViewSignature{
			ViewID:             input.View.ID,
			SourceRevisions:    revisions,
			TransformSignature: "test-summary-lifecycle:v1",
		},
	}}, nil
}

type failingSummaryLifecycleSummarizer struct {
	calls int
	err   error
}

func (s *failingSummaryLifecycleSummarizer) Summarize(_ context.Context, _ derive.SummaryInput) ([]recent.SummaryNode, error) {
	s.calls++
	return nil, s.err
}

type summaryStoreWithoutTargetDelete struct {
	recent.SummaryStore
}

type failingSummaryDeleteStore struct {
	recent.SummaryStore
	err error
}

func (s *failingSummaryDeleteStore) DeleteNode(context.Context, views.Scope, recent.NodeID) error {
	return s.err
}

func summaryLifecycleNode(scope memory.Scope, id recent.NodeID, summary string) recent.SummaryNode {
	ref := views.SourceRef{
		Kind:    views.SourceMessage,
		Message: &views.MessageSourceRef{ConversationID: scope.ConversationID, MessageID: "dia-1"},
	}
	return recent.SummaryNode{
		ID:         id,
		Scope:      scope,
		SourceRefs: []views.SourceRef{ref},
		Summary:    summary,
		Level:      0,
		Signature: views.ViewSignature{
			ViewID: "summary_dag",
			SourceRevisions: []views.SourceRevision{{
				Kind:      views.SourceMessage,
				SourceKey: ref.StableKey(),
				Revision:  "1",
			}},
			TransformSignature: "test-summary-lifecycle:old",
		},
	}
}

func requireDiagnosticCheck(t *testing.T, checks []memory.DiagnosticCheck, name string) memory.DiagnosticCheck {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("diagnostic checks = %+v, want %q", checks, name)
	return memory.DiagnosticCheck{}
}

func assertCheckpointBool(t *testing.T, checkpoint map[string]any, key string, want bool) {
	t.Helper()
	got, ok := checkpoint[key]
	if !ok {
		t.Fatalf("checkpoint = %+v, missing %q", checkpoint, key)
	}
	value, ok := got.(bool)
	if !ok || value != want {
		t.Fatalf("checkpoint[%q] = %T(%v), want %v", key, got, got, want)
	}
}

func assertCheckpointInt(t *testing.T, checkpoint map[string]any, key string, want int) {
	t.Helper()
	got, ok := checkpoint[key]
	if !ok {
		t.Fatalf("checkpoint = %+v, missing %q", checkpoint, key)
	}
	assertAnyInt(t, "checkpoint["+key+"]", got, want)
}

func assertCheckpointString(t *testing.T, checkpoint map[string]any, key, want string) {
	t.Helper()
	got, ok := checkpoint[key]
	if !ok {
		t.Fatalf("checkpoint = %+v, missing %q", checkpoint, key)
	}
	value, ok := got.(string)
	if !ok || value != want {
		t.Fatalf("checkpoint[%q] = %T(%v), want %q", key, got, got, want)
	}
}

func assertStepDetailInt(t *testing.T, step memory.LifecycleStep, key string, want int) {
	t.Helper()
	got, ok := step.Details[key]
	if !ok {
		t.Fatalf("%s details = %+v, missing %q", step.Name, step.Details, key)
	}
	assertAnyInt(t, step.Name+" details["+key+"]", got, want)
}

func assertAnyInt(t *testing.T, label string, got any, want int) {
	t.Helper()
	switch value := got.(type) {
	case int:
		if value == want {
			return
		}
	case int64:
		if value == int64(want) {
			return
		}
	case uint64:
		if value == uint64(want) {
			return
		}
	case float64:
		if value == float64(want) {
			return
		}
	}
	t.Fatalf("%s = %T(%v), want %d", label, got, got, want)
}

func checkpointStringSliceContains(checkpoint map[string]any, key, want string) bool {
	if checkpoint == nil {
		return false
	}
	got, ok := checkpoint[key]
	if !ok {
		return false
	}
	values, ok := got.([]string)
	if !ok {
		return false
	}
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func listProjectionDocs(t *testing.T, ctx context.Context, index retrieval.Index, namespace string) []retrieval.Doc {
	t.Helper()
	resp, err := index.List(ctx, namespace, retrieval.ListRequest{
		PageSize: 10000,
		OrderBy:  retrieval.OrderByIDAsc,
	})
	if err != nil {
		t.Fatalf("List(%s) error = %v", namespace, err)
	}
	if resp == nil {
		t.Fatalf("List(%s) returned nil response", namespace)
	}
	if resp.NextPageToken != "" {
		t.Fatalf("List(%s) NextPageToken = %q, want complete page", namespace, resp.NextPageToken)
	}
	return resp.Items
}

func sourceMessageRecord(t *testing.T, scope memory.Scope, msg sourcemessage.Message) indexed.Record {
	t.Helper()
	records, err := projectors.SourceMessageRecords(scope, msg)
	if err != nil {
		t.Fatalf("SourceMessageRecords(%s) error = %v", msg.ID, err)
	}
	if len(records) != 1 {
		t.Fatalf("SourceMessageRecords(%s) len = %d, want 1", msg.ID, len(records))
	}
	return records[0]
}

func assertDiagnosticDetailInt(t *testing.T, check memory.DiagnosticCheck, key string, want int) {
	t.Helper()
	got, ok := check.Details[key]
	if !ok {
		t.Fatalf("%s details = %+v, missing %q", check.Name, check.Details, key)
	}
	switch value := got.(type) {
	case int:
		if value != want {
			t.Fatalf("%s details[%q] = %d, want %d", check.Name, key, value, want)
		}
	case int64:
		if value != int64(want) {
			t.Fatalf("%s details[%q] = %d, want %d", check.Name, key, value, want)
		}
	default:
		t.Fatalf("%s details[%q] = %T(%v), want int %d", check.Name, key, got, got, want)
	}
}

func assertDiagnosticDetailString(t *testing.T, check memory.DiagnosticCheck, key, want string) {
	t.Helper()
	got, ok := check.Details[key]
	if !ok {
		t.Fatalf("%s details = %+v, missing %q", check.Name, check.Details, key)
	}
	value, ok := got.(string)
	if !ok || value != want {
		t.Fatalf("%s details[%q] = %T(%v), want %q", check.Name, key, got, got, want)
	}
}

func capabilityPresent(capabilities []memory.Capability, capability memory.Capability) bool {
	for _, candidate := range capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

type diagnosticFailingReportStore struct {
	delegate *memory.MemoryReportStore
	err      error
}

func (s *diagnosticFailingReportStore) PutLifecycleReport(ctx context.Context, report memory.LifecycleExecutionReport) error {
	return s.delegate.PutLifecycleReport(ctx, report)
}

func (s *diagnosticFailingReportStore) GetLifecycleReport(ctx context.Context, traceID memory.TraceID) (memory.LifecycleExecutionReport, bool, error) {
	return s.delegate.GetLifecycleReport(ctx, traceID)
}

func (s *diagnosticFailingReportStore) ListLifecycleReports(ctx context.Context) ([]memory.LifecycleExecutionReport, error) {
	return s.delegate.ListLifecycleReports(ctx)
}

func (s *diagnosticFailingReportStore) PutDiagnosticReport(context.Context, memory.DiagnosticReport) error {
	return s.err
}

func (s *diagnosticFailingReportStore) GetDiagnosticReport(ctx context.Context, traceID memory.TraceID) (memory.DiagnosticReport, bool, error) {
	return s.delegate.GetDiagnosticReport(ctx, traceID)
}

func (s *diagnosticFailingReportStore) ListDiagnosticReports(ctx context.Context) ([]memory.DiagnosticReport, error) {
	return s.delegate.ListDiagnosticReports(ctx)
}

type deleteByFilterUnavailableLifecycleIndex struct {
	retrieval.Index
	listErr   error
	deleteErr error
}

func (i *deleteByFilterUnavailableLifecycleIndex) DeleteByFilter(context.Context, string, retrieval.Filter) (int64, error) {
	return 0, errdefs.NotAvailablef("test: delete by filter unsupported")
}

func (i *deleteByFilterUnavailableLifecycleIndex) List(ctx context.Context, namespace string, req retrieval.ListRequest) (*retrieval.ListResponse, error) {
	if i.listErr != nil {
		return nil, i.listErr
	}
	return i.Index.List(ctx, namespace, req)
}

func (i *deleteByFilterUnavailableLifecycleIndex) Delete(ctx context.Context, namespace string, ids []string) error {
	if i.deleteErr != nil {
		return i.deleteErr
	}
	return i.Index.Delete(ctx, namespace, ids)
}
