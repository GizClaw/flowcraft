// Package localmem builds the LoCoMo local workspace memory baseline.
package localmem

import (
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/memory/derive"
	derivecontextpack "github.com/GizClaw/flowcraft/memory/derive/context"
	deriveentityfact "github.com/GizClaw/flowcraft/memory/derive/entityfact"
	derivesummary "github.com/GizClaw/flowcraft/memory/derive/summary"
	retrievalworkspace "github.com/GizClaw/flowcraft/memory/retrieval/workspace"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

const locomoSummaryMaxRawMessages = 5

// MemoryOptions configures the LocalWorkspace-backed raw-source message baseline.
type MemoryOptions struct {
	WorkspaceRoot  string
	Embedder       embedding.Embedder
	MemoryLLM      llm.LLM
	PerCallTimeout time.Duration
}

// Build creates the required localworkspace + sync memory assembly.
func Build(opts MemoryOptions) (*memory.System, func() error, error) {
	if opts.WorkspaceRoot == "" {
		return nil, nil, fmt.Errorf("locomo localmem: workspace root is required")
	}
	root, err := sdkworkspace.NewLocalWorkspace(opts.WorkspaceRoot)
	if err != nil {
		return nil, nil, err
	}
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval/index"))
	if err != nil {
		return nil, nil, err
	}

	summarizer := derive.Summarizer(derivesummary.BufferSummarizer{Policy: summaryPolicy()})
	entityExtractor := derive.EntityFactExtractor(deriveentityfact.NoopExtractor{})
	if opts.MemoryLLM != nil {
		summarizer = derivesummary.LLMSummarizer{
			LLM:                  opts.MemoryLLM,
			Policy:               summaryPolicy(),
			MaxSourceRefsPerNode: 3,
			MaxMessagesPerCall:   48,
			Timeout:              opts.PerCallTimeout,
		}
		entityExtractor = deriveentityfact.AgentExtractor{
			LLM:                opts.MemoryLLM,
			MaxMessagesPerCall: 16,
			Timeout:            opts.PerCallTimeout,
		}
	}

	mem, err := memory.New(SyncSpec(), memory.Deps{
		MessageStore:        sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message")),
		SummaryStore:        recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag")),
		EntityFactStore:     viewentityfact.NewWorkspaceStore(sdkworkspace.Sub(root, "views/entity_facts")),
		Index:               index,
		Embedder:            opts.Embedder,
		Embedding:           memory.EmbeddingOptions{Timeout: opts.PerCallTimeout},
		Summarizer:          summarizer,
		EntityFactExtractor: entityExtractor,
		ContextPacker: derivecontextpack.SourceEvidencePacker{
			SourceOnly:              true,
			MaxDirectMessages:       12,
			MaxSummaryMessages:      18,
			MaxEntityFactMessages:   4,
			MaxGraphMessages:        0,
			MaxNeighborhoodMessages: 4,
			MaxSourceRefsPerHit:     3,
			MinEntityConfidence:     0.5,
			MinRelativeScore:        0.5,
			UseDirectMessages:       true,
			UseSummaryRefs:          true,
			UseEntityFactRefs:       true,
			UseGraphSources:         true,
			UseNeighborhood:         true,
			NeighborhoodBefore:      1,
			NeighborhoodAfter:       1,
			NeighborhoodAnchors:     []derivecontextpack.SourceEvidenceOrigin{derivecontextpack.SourceEvidenceOriginSummary},
			GraphMaxSeedFacts:       8,
			GraphOptions:            viewentityfact.GraphExpansionOptions{MaxFactsPerSeed: 8, MaxBridgeFacts: 8, MaxSourceRefsPerGraphPath: 2},
			OriginMetadataKey:       "retrieval_origin",
			OriginMetadataValues: derivecontextpack.SourceEvidenceOriginValues{
				Direct:       "source_direct",
				Summary:      "summary_expanded",
				EntityFact:   "entity_fact_expanded",
				Graph:        "graph_fact_expanded",
				Neighborhood: "source_neighborhood_expanded",
			},
		},
	})
	if err != nil {
		_ = index.Close()
		return nil, nil, err
	}
	closeFn := func() error {
		memErr := mem.Close()
		indexErr := index.Close()
		if memErr != nil {
			return memErr
		}
		return indexErr
	}
	return mem, closeFn, nil
}

func summaryPolicy() derive.SummaryPolicy {
	return derive.SummaryPolicy{
		MaxRawMessages:         locomoSummaryMaxRawMessages,
		PreserveRecentMessages: locomoSummaryMaxRawMessages,
	}
}

// SyncSpec pins the LoCoMo suite to synchronous write stages.
func SyncSpec() memory.Spec {
	return memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
			{Capability: memory.CapabilityEntityFactIndex, Required: true},
		},
		Projections: []memory.ProjectionSpec{
			{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true},
			{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true},
			{Capability: memory.CapabilityEntityFactIndex, Namespace: "entity_facts", Required: true},
		},
		WriteStages: []memory.StageSpec{
			{Name: "append_message"},
			{Name: "index_messages"},
			{Name: "build_summary_dag"},
			{Name: "build_entity_facts"},
		},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_messages"},
			{Name: "retrieve_summaries", Config: map[string]any{
				"drilldown_max_depth":   0,
				"drilldown_child_top_k": 2,
			}},
			{Name: "retrieve_entity_facts"},
			{Name: "pack_context"},
		},
	}
}
