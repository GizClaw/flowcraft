package memory

import (
	"strings"

	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Plan is the root memory facade's executable control plan. It is compiled
// from the public Spec/Assembly and intentionally lives outside internal
// compiler output so stage semantics stay owned by this facade.
type Plan struct {
	Write       []PlannedStage
	Read        []PlannedStage
	Lifecycle   []PlannedStage
	Diagnostics []PlannedStage
}

// PlannedStage is a validated stage with root-facade execution metadata.
type PlannedStage struct {
	Name       string
	Async      bool
	Optional   bool
	Capability Capability
}

const (
	lifecycleStageReadiness  = "readiness"
	lifecycleStageQueueStats = "queue_stats"
	lifecycleStageDrain      = "drain"
	lifecycleStageShutdown   = "shutdown"
	lifecycleStageRebuild    = "rebuild"
	lifecycleStageReconcile  = "reconcile"

	diagnosticStageTrace     = "trace"
	diagnosticStageFreshness = "freshness"

	lifecycleStageCompact        = "compact"
	lifecycleStageArchive        = "archive"
	lifecycleStageReload         = "reload"
	lifecycleStageFreshnessCheck = "freshness_check"
)

func compilePlan(assembly compiler.Assembly, writeAvailable, readAvailable map[Capability]bool) (Plan, error) {
	write, err := compileWritePlan(assembly, writeAvailable)
	if err != nil {
		return Plan{}, err
	}
	read, err := compileReadPlan(assembly, readAvailable)
	if err != nil {
		return Plan{}, err
	}
	lifecycle, err := compileLifecyclePlan(assembly)
	if err != nil {
		return Plan{}, err
	}
	diagnostics, err := compileDiagnosticsPlan(assembly)
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		Write:       write,
		Read:        read,
		Lifecycle:   lifecycle,
		Diagnostics: diagnostics,
	}, nil
}

func compileWritePlan(assembly compiler.Assembly, available map[Capability]bool) ([]PlannedStage, error) {
	stages := assembly.WriteStages
	if len(stages) == 0 {
		stages = defaultWritePlan(available)
	}
	plan := make([]PlannedStage, 0, len(stages))
	for _, stage := range stages {
		planned, ok, err := planWriteStage(stage, available)
		if err != nil {
			return nil, err
		}
		if ok {
			plan = append(plan, planned)
		}
	}
	return plan, nil
}

func compileReadPlan(assembly compiler.Assembly, available map[Capability]bool) ([]PlannedStage, error) {
	stages := assembly.ReadStages
	if len(stages) == 0 {
		stages = defaultReadPlan(available)
	}
	plan := make([]PlannedStage, 0, len(stages))
	for _, stage := range stages {
		planned, ok, err := planReadStage(stage, available)
		if err != nil {
			return nil, err
		}
		if ok {
			plan = append(plan, planned)
		}
	}
	return plan, nil
}

func compileLifecyclePlan(assembly compiler.Assembly) ([]PlannedStage, error) {
	stages := assembly.Lifecycle
	if len(stages) == 0 {
		stages = []StageSpec{
			{Name: lifecycleStageReadiness},
			{Name: lifecycleStageQueueStats},
			{Name: lifecycleStageDrain},
			{Name: lifecycleStageShutdown},
		}
	}
	plan := make([]PlannedStage, 0, len(stages))
	for _, stage := range stages {
		planned, ok, err := planLifecycleStage(stage)
		if err != nil {
			return nil, err
		}
		if ok {
			plan = append(plan, planned)
		}
	}
	return plan, nil
}

func compileDiagnosticsPlan(assembly compiler.Assembly) ([]PlannedStage, error) {
	stages := assembly.Diagnostics
	plan := make([]PlannedStage, 0, len(stages))
	for _, stage := range stages {
		planned, ok, err := planDiagnosticsStage(stage)
		if err != nil {
			return nil, err
		}
		if ok {
			plan = append(plan, planned)
		}
	}
	return plan, nil
}

func planWriteStage(stage StageSpec, available map[Capability]bool) (PlannedStage, bool, error) {
	name := strings.TrimSpace(stage.Name)
	if name == "" {
		return PlannedStage{}, false, errdefs.Validationf("memory: write stage name is required")
	}
	if !isSupportedWriteStage(name) {
		if stage.Optional {
			return PlannedStage{}, false, nil
		}
		return PlannedStage{}, false, errdefs.Validationf("memory: unsupported write stage %q", stage.Name)
	}
	capability, hasCapability := writeStageCapability(name)
	if stage.Optional && hasCapability && !available[capability] {
		return PlannedStage{}, false, nil
	}
	return PlannedStage{
		Name:       name,
		Async:      stage.Async,
		Optional:   stage.Optional,
		Capability: capability,
	}, true, nil
}

func planReadStage(stage StageSpec, available map[Capability]bool) (PlannedStage, bool, error) {
	name := strings.TrimSpace(stage.Name)
	if name == "" {
		return PlannedStage{}, false, errdefs.Validationf("memory: read stage name is required")
	}
	if stage.Async {
		return PlannedStage{}, false, errdefs.Validationf("memory: read stage %q cannot be async", stage.Name)
	}
	if !isSupportedReadStage(name) {
		if stage.Optional {
			return PlannedStage{}, false, nil
		}
		return PlannedStage{}, false, errdefs.Validationf("memory: unsupported read stage %q", stage.Name)
	}
	capability, hasCapability := readStageCapability(name)
	if stage.Optional && hasCapability && !available[capability] {
		return PlannedStage{}, false, nil
	}
	return PlannedStage{
		Name:       name,
		Optional:   stage.Optional,
		Capability: capability,
	}, true, nil
}

func planLifecycleStage(stage StageSpec) (PlannedStage, bool, error) {
	name := strings.TrimSpace(stage.Name)
	if name == "" {
		return PlannedStage{}, false, errdefs.Validationf("memory: lifecycle stage name is required")
	}
	if stage.Async {
		if stage.Optional {
			return PlannedStage{}, false, nil
		}
		return PlannedStage{}, false, errdefs.Validationf("memory: lifecycle stage %q cannot be async", stage.Name)
	}
	if isSupportedLifecycleStage(name) {
		return PlannedStage{Name: name, Optional: stage.Optional}, true, nil
	}
	if isToleratedOptionalLifecycleName(name) && stage.Optional {
		return PlannedStage{}, false, nil
	}
	if stage.Optional {
		return PlannedStage{}, false, nil
	}
	return PlannedStage{}, false, errdefs.Validationf("memory: unsupported lifecycle stage %q", stage.Name)
}

func planDiagnosticsStage(stage StageSpec) (PlannedStage, bool, error) {
	name := strings.TrimSpace(stage.Name)
	if name == "" {
		return PlannedStage{}, false, errdefs.Validationf("memory: diagnostics stage name is required")
	}
	if stage.Async {
		if stage.Optional {
			return PlannedStage{}, false, nil
		}
		return PlannedStage{}, false, errdefs.Validationf("memory: diagnostics stage %q cannot be async", stage.Name)
	}
	switch name {
	case lifecycleStageReadiness, lifecycleStageQueueStats, diagnosticStageTrace, diagnosticStageFreshness:
		return PlannedStage{Name: name, Optional: stage.Optional}, true, nil
	default:
		if stage.Optional {
			return PlannedStage{}, false, nil
		}
		return PlannedStage{}, false, errdefs.Validationf("memory: unsupported diagnostics stage %q", stage.Name)
	}
}

func defaultWritePlan(available map[Capability]bool) []StageSpec {
	var stages []StageSpec
	if available[CapabilityDocumentChunks] {
		stages = append(stages, StageSpec{Name: writeStageChunkDocument})
	}
	if available[CapabilitySummaryDAG] {
		stages = append(stages, StageSpec{Name: writeStageBuildSummaryDAG})
	}
	if available[CapabilityObservationLedger] {
		stages = append(stages, StageSpec{Name: writeStageExtractObservations})
	}
	if available[CapabilityObservationLedger] && available[CapabilityFactLedger] {
		stages = append(stages, StageSpec{Name: writeStageReconcileFacts})
	}
	if available[CapabilityObservationLedger] && available[CapabilityFactLedger] && available[CapabilityFactGraph] {
		stages = append(stages, StageSpec{Name: writeStageBuildFactGraph})
	}
	if available[CapabilityObservationLedger] && available[CapabilityFactLedger] && available[CapabilityFactGraph] && available[CapabilityEntityProfile] {
		stages = append(stages, StageSpec{Name: writeStageBuildEntityProfiles})
	}
	if available[CapabilityObservationLedger] && available[CapabilityFactLedger] && available[CapabilityFactGraph] && available[CapabilityEntityTimeline] {
		stages = append(stages, StageSpec{Name: writeStageBuildEntityTimeline})
	}
	return stages
}

func defaultReadPlan(available map[Capability]bool) []StageSpec {
	stages := []StageSpec{{Name: readStageLoadRecentMessages}}
	if available[CapabilitySummaryDAG] {
		stages = append(stages, StageSpec{Name: readStageRetrieveSummaries})
	}
	if available[CapabilityDocumentChunks] {
		stages = append(stages, StageSpec{Name: readStageRetrieveDocuments})
	}
	if available[CapabilityObservationLedger] {
		stages = append(stages, StageSpec{Name: readStageRetrieveObs})
	}
	if available[CapabilityFactLedger] {
		stages = append(stages, StageSpec{Name: readStageRetrieveFacts})
	}
	if available[CapabilityFactGraph] {
		stages = append(stages, StageSpec{Name: readStageRetrieveFactGraph})
	}
	if available[CapabilityEntityProfile] {
		stages = append(stages, StageSpec{Name: readStageRetrieveEntityProfiles})
	}
	if available[CapabilityEntityTimeline] {
		stages = append(stages, StageSpec{Name: readStageRetrieveEntityTimeline})
	}
	return append(stages, StageSpec{Name: readStagePackContext})
}

func isSupportedWriteStage(name string) bool {
	switch name {
	case writeStageAppendMessage,
		writeStageChunkDocument,
		writeStageExtractObservations,
		writeStageReconcileFacts,
		writeStageBuildFactGraph,
		writeStageBuildEntityProfiles,
		writeStageBuildEntityTimeline,
		writeStageBuildSummaryDAG:
		return true
	default:
		return false
	}
}

func isSupportedReadStage(name string) bool {
	switch name {
	case readStageLoadRecentMessages,
		readStageRetrieveSummaries,
		readStageRetrieveDocuments,
		readStageRetrieveObs,
		readStageRetrieveFacts,
		readStageRetrieveFactGraph,
		readStageRetrieveEntityProfiles,
		readStageRetrieveEntityTimeline,
		readStageExpandFactGraph,
		readStagePackContext:
		return true
	default:
		return false
	}
}

func isSupportedLifecycleStage(name string) bool {
	switch name {
	case lifecycleStageReadiness,
		lifecycleStageQueueStats,
		lifecycleStageDrain,
		lifecycleStageShutdown,
		lifecycleStageRebuild,
		lifecycleStageReconcile,
		lifecycleStageReload,
		lifecycleStageFreshnessCheck:
		return true
	default:
		return false
	}
}

func isToleratedOptionalLifecycleName(name string) bool {
	switch name {
	case lifecycleStageCompact, lifecycleStageArchive:
		return true
	default:
		return false
	}
}

func writeStageCapability(name string) (Capability, bool) {
	switch name {
	case writeStageChunkDocument:
		return CapabilityDocumentChunks, true
	case writeStageBuildSummaryDAG:
		return CapabilitySummaryDAG, true
	case writeStageExtractObservations:
		return CapabilityObservationLedger, true
	case writeStageReconcileFacts:
		return CapabilityFactLedger, true
	case writeStageBuildFactGraph:
		return CapabilityFactGraph, true
	case writeStageBuildEntityProfiles:
		return CapabilityEntityProfile, true
	case writeStageBuildEntityTimeline:
		return CapabilityEntityTimeline, true
	default:
		return "", false
	}
}

func readStageCapability(name string) (Capability, bool) {
	switch name {
	case readStageRetrieveSummaries:
		return CapabilitySummaryDAG, true
	case readStageRetrieveDocuments:
		return CapabilityDocumentChunks, true
	case readStageRetrieveObs:
		return CapabilityObservationLedger, true
	case readStageRetrieveFacts:
		return CapabilityFactLedger, true
	case readStageRetrieveFactGraph, readStageExpandFactGraph:
		return CapabilityFactGraph, true
	case readStageRetrieveEntityProfiles:
		return CapabilityEntityProfile, true
	case readStageRetrieveEntityTimeline:
		return CapabilityEntityTimeline, true
	default:
		return "", false
	}
}

func hasAsyncWriteStages(stages []PlannedStage) bool {
	for _, stage := range stages {
		if stage.Async && stage.Name != writeStageAppendMessage {
			return true
		}
	}
	return false
}

func clonePlannedStages(in []PlannedStage) []PlannedStage {
	if in == nil {
		return nil
	}
	out := make([]PlannedStage, len(in))
	copy(out, in)
	return out
}

func clonePlan(in Plan) Plan {
	return Plan{
		Write:       clonePlannedStages(in.Write),
		Read:        clonePlannedStages(in.Read),
		Lifecycle:   clonePlannedStages(in.Lifecycle),
		Diagnostics: clonePlannedStages(in.Diagnostics),
	}
}
