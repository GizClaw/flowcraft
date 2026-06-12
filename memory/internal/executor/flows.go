package executor

import (
	"context"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	"github.com/GizClaw/flowcraft/memory/internal/projectors"
	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	viewentity "github.com/GizClaw/flowcraft/memory/views/entity"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	viewobservation "github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// IndexDocument chunks a canonical document and writes any configured
// projection to namespaceOverride instead of the compiler-bound namespace.
func (r *Executor) IndexDocument(ctx context.Context, scope views.Scope, documentID, namespaceOverride string) ([]viewdocument.Chunk, error) {
	if err := r.requireDocumentChunks(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, errdefs.Validationf("%s: invalid document scope: %w", errPrefix, err)
	}
	if scope.DatasetID == "" {
		return nil, errdefs.Validationf("%s: dataset_id is required", errPrefix)
	}
	datasetID := scope.DatasetID
	doc, ok, err := r.documentStore.Get(ctx, datasetID, documentID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errdefs.NotFoundf("%s: document %q/%q not found", errPrefix, datasetID, documentID)
	}

	chunks, err := r.documentChunker.ChunkDocument(ctx, DocumentChunkInput{
		View:     r.documentChunks.Descriptor(),
		Scope:    scope,
		Document: doc,
	})
	if err != nil {
		return nil, err
	}

	stored := make([]viewdocument.Chunk, 0, len(chunks))
	for _, chunk := range chunks {
		written, err := r.documentChunks.PutChunk(ctx, chunk)
		if err != nil {
			return nil, err
		}
		stored = append(stored, written)
	}
	writer, err := r.writerFor(compiler.CapabilityDocumentChunks, namespaceOverride)
	if err != nil {
		return nil, err
	}
	if writer != nil {
		records := make([]indexed.Record, 0, len(stored))
		for _, chunk := range stored {
			record, err := projectors.DocumentChunk(chunk)
			if err != nil {
				return nil, err
			}
			records = append(records, record)
		}
		if err := writer.Upsert(ctx, records); err != nil {
			return nil, err
		}
	}
	return stored, nil
}

// SearchDocumentChunks searches the document chunk projection namespace and
// hydrates every hit from the semantic chunk store.
func (r *Executor) SearchDocumentChunks(ctx context.Context, req retrieval.SearchRequest, namespaceOverride string) (*DocumentChunkSearchResponse, error) {
	if err := r.requireDocumentChunkSearch(); err != nil {
		return nil, err
	}
	namespace, err := r.namespaceForSearch(compiler.CapabilityDocumentChunks, namespaceOverride)
	if err != nil {
		return nil, err
	}
	resp, err := r.index.Search(ctx, namespace, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errdefs.NotAvailablef("%s: search projection for capability %q returned nil response", errPrefix, compiler.CapabilityDocumentChunks)
	}
	out := &DocumentChunkSearchResponse{Took: resp.Took, Hits: make([]DocumentChunkSearchHit, 0, len(resp.Hits))}
	for _, hit := range resp.Hits {
		datasetID, err := metadataString(hit, projectors.MetadataDatasetIDKey)
		if err != nil {
			return nil, err
		}
		scope, err := metadataScope(hit)
		if err != nil {
			return nil, err
		}
		scope.DatasetID = datasetID
		documentID, err := metadataString(hit, projectors.MetadataDocumentIDKey)
		if err != nil {
			return nil, err
		}
		chunkID, err := metadataString(hit, projectors.MetadataChunkIDKey)
		if err != nil {
			return nil, err
		}
		chunk, ok, err := r.documentChunks.GetChunk(ctx, scope, documentID, viewdocument.ChunkID(chunkID))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errdefs.NotAvailablef("%s: hydrate document chunk hit %q: chunk %q/%q/%q not found", errPrefix, hit.Doc.ID, datasetID, documentID, chunkID)
		}
		out.Hits = append(out.Hits, DocumentChunkSearchHit{
			Retrieval: retrieval.CloneHit(hit),
			Chunk:     chunk,
		})
	}
	return out, nil
}

// BuildSummaryDAG derives summary nodes from a recent message window.
func (r *Executor) BuildSummaryDAG(ctx context.Context, req recent.WindowRequest, namespaceOverride string) ([]recent.SummaryNode, error) {
	if err := r.requireSummaryDAG(); err != nil {
		return nil, err
	}
	window, err := r.recentWindow.Load(ctx, req)
	if err != nil {
		return nil, err
	}
	nodes, err := r.summarizer.Summarize(ctx, SummaryInput{
		View:   r.summaryDAG.Descriptor(),
		Scope:  req.Scope,
		Window: window,
	})
	if err != nil {
		return nil, err
	}

	stored := make([]recent.SummaryNode, 0, len(nodes))
	for _, node := range nodes {
		written, err := r.summaryDAG.PutNode(ctx, node)
		if err != nil {
			return nil, err
		}
		stored = append(stored, written)
	}
	writer, err := r.writerFor(compiler.CapabilitySummaryDAG, namespaceOverride)
	if err != nil {
		return nil, err
	}
	if writer != nil {
		records := make([]indexed.Record, 0, len(stored))
		for _, node := range stored {
			record, err := projectors.SummaryNode(node)
			if err != nil {
				return nil, err
			}
			records = append(records, record)
		}
		if err := writer.Upsert(ctx, records); err != nil {
			return nil, err
		}
	}
	return stored, nil
}

// SearchSummaryNodes searches the SummaryDAG projection namespace and hydrates
// every hit from the SummaryDAG store.
func (r *Executor) SearchSummaryNodes(ctx context.Context, req retrieval.SearchRequest, namespaceOverride string) (*SummaryNodeSearchResponse, error) {
	if err := r.requireSummaryNodeSearch(); err != nil {
		return nil, err
	}
	namespace, err := r.namespaceForSearch(compiler.CapabilitySummaryDAG, namespaceOverride)
	if err != nil {
		return nil, err
	}
	resp, err := r.index.Search(ctx, namespace, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errdefs.NotAvailablef("%s: search projection for capability %q returned nil response", errPrefix, compiler.CapabilitySummaryDAG)
	}
	out := &SummaryNodeSearchResponse{Took: resp.Took, Hits: make([]SummaryNodeSearchHit, 0, len(resp.Hits))}
	for _, hit := range resp.Hits {
		scope, err := metadataScope(hit)
		if err != nil {
			return nil, err
		}
		nodeID, err := metadataString(hit, projectors.MetadataNodeIDKey)
		if err != nil {
			return nil, err
		}
		node, ok, err := r.summaryDAG.GetNode(ctx, scope, recent.NodeID(nodeID))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errdefs.NotAvailablef("%s: hydrate summary hit %q: node %q/%q/%q/%q not found", errPrefix, hit.Doc.ID, scope.RuntimeID, scope.UserID, scope.ConversationID, nodeID)
		}
		out.Hits = append(out.Hits, SummaryNodeSearchHit{
			Retrieval: retrieval.CloneHit(hit),
			Node:      node,
		})
	}
	return out, nil
}

// ExtractObservations derives observations and writes any configured
// projection to namespaceOverride instead of the compiler-bound namespace.
func (r *Executor) ExtractObservations(ctx context.Context, req recent.WindowRequest, scope viewobservation.Scope, namespaceOverride string) ([]viewobservation.Observation, error) {
	if err := r.requireObservationLedger(); err != nil {
		return nil, err
	}
	window, err := r.recentWindow.Load(ctx, req)
	if err != nil {
		return nil, err
	}
	observations, err := r.observationExtractor.ExtractObservations(ctx, ObservationInput{
		View:   r.observationLedger.Descriptor(),
		Window: window,
		Scope:  scope,
	})
	if err != nil {
		return nil, err
	}

	stored := make([]viewobservation.Observation, 0, len(observations))
	for _, observation := range observations {
		written, err := r.observationLedger.Put(ctx, observation)
		if err != nil {
			return nil, err
		}
		stored = append(stored, written)
	}
	writer, err := r.writerFor(compiler.CapabilityObservationLedger, namespaceOverride)
	if err != nil {
		return nil, err
	}
	if writer != nil {
		records := make([]indexed.Record, 0, len(stored))
		for _, observation := range stored {
			record, err := projectors.Observation(observation)
			if err != nil {
				return nil, err
			}
			records = append(records, record)
		}
		if err := writer.Upsert(ctx, records); err != nil {
			return nil, err
		}
	}
	return stored, nil
}

// SearchObservations searches the observation projection namespace and hydrates
// every hit from the observation ledger.
func (r *Executor) SearchObservations(ctx context.Context, req retrieval.SearchRequest) (*ObservationSearchResponse, error) {
	return r.searchObservations(ctx, req, "")
}

func (r *Executor) searchObservations(ctx context.Context, req retrieval.SearchRequest, namespaceOverride string) (*ObservationSearchResponse, error) {
	if err := r.requireObservationSearch(); err != nil {
		return nil, err
	}
	namespace, err := r.namespaceForSearch(compiler.CapabilityObservationLedger, namespaceOverride)
	if err != nil {
		return nil, err
	}
	resp, err := r.index.Search(ctx, namespace, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errdefs.NotAvailablef("%s: search projection for capability %q returned nil response", errPrefix, compiler.CapabilityObservationLedger)
	}
	out := &ObservationSearchResponse{Took: resp.Took, Hits: make([]ObservationSearchHit, 0, len(resp.Hits))}
	for _, hit := range resp.Hits {
		observationID, err := metadataString(hit, projectors.MetadataObservationIDKey)
		if err != nil {
			return nil, err
		}
		observation, ok, err := r.observationLedger.Get(ctx, observationID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errdefs.NotAvailablef("%s: hydrate observation hit %q: observation %q not found", errPrefix, hit.Doc.ID, observationID)
		}
		out.Hits = append(out.Hits, ObservationSearchHit{
			Retrieval:   retrieval.CloneHit(hit),
			Observation: observation,
		})
	}
	return out, nil
}

// ReconcileFacts derives fact records from observations, stores them, and
// projects them when a fact ledger projection is configured.
func (r *Executor) ReconcileFacts(ctx context.Context, observations []viewobservation.Observation) ([]fact.Fact, error) {
	return r.ReconcileFactsScoped(ctx, observations, "")
}

// ReconcileFactsScoped stores reconciled facts and writes any configured
// projection to namespaceOverride instead of the compiler-bound namespace.
func (r *Executor) ReconcileFactsScoped(ctx context.Context, observations []viewobservation.Observation, namespaceOverride string) ([]fact.Fact, error) {
	if err := r.requireFactLedger(); err != nil {
		return nil, err
	}
	scope, err := factReconcileScope(observations)
	if err != nil {
		return nil, err
	}
	var current []fact.Fact
	if !scope.IsZero() {
		current, err = r.factLedger.List(ctx, fact.ListOptions{Scope: scope, ActiveOnly: true})
		if err != nil {
			return nil, err
		}
	}
	facts, err := r.factReconciler.ReconcileFacts(ctx, FactReconcileInput{
		View:         r.factLedger.Descriptor(),
		Scope:        scope,
		Observations: observations,
		Current:      current,
	})
	if err != nil {
		return nil, err
	}

	stored := make([]fact.Fact, 0, len(facts))
	for _, record := range facts {
		written, err := r.factLedger.Put(ctx, record)
		if err != nil {
			return nil, err
		}
		stored = append(stored, written)
	}
	writer, err := r.writerFor(compiler.CapabilityFactLedger, namespaceOverride)
	if err != nil {
		return nil, err
	}
	if writer != nil {
		records := make([]indexed.Record, 0, len(stored))
		for _, record := range stored {
			if !isActiveFact(record) {
				continue
			}
			projected, err := projectors.FactRecord(record)
			if err != nil {
				return nil, err
			}
			records = append(records, projected)
		}
		if err := writer.Upsert(ctx, records); err != nil {
			return nil, err
		}
	}
	return stored, nil
}

// SearchFacts searches the fact ledger projection namespace and hydrates every
// hit from the fact ledger.
func (r *Executor) SearchFacts(ctx context.Context, req retrieval.SearchRequest) (*FactSearchResponse, error) {
	return r.searchFacts(ctx, req, "")
}

func (r *Executor) searchFacts(ctx context.Context, req retrieval.SearchRequest, namespaceOverride string) (*FactSearchResponse, error) {
	if err := r.requireFactSearch(); err != nil {
		return nil, err
	}
	namespace, err := r.namespaceForSearch(compiler.CapabilityFactLedger, namespaceOverride)
	if err != nil {
		return nil, err
	}
	resp, err := r.index.Search(ctx, namespace, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errdefs.NotAvailablef("%s: search projection for capability %q returned nil response", errPrefix, compiler.CapabilityFactLedger)
	}
	out := &FactSearchResponse{Took: resp.Took, Hits: make([]FactSearchHit, 0, len(resp.Hits))}
	for _, hit := range resp.Hits {
		factID, err := metadataString(hit, projectors.MetadataFactIDKey)
		if err != nil {
			return nil, err
		}
		record, ok, err := r.factLedger.Get(ctx, fact.FactID(factID))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errdefs.NotAvailablef("%s: hydrate fact hit %q: fact %q not found", errPrefix, hit.Doc.ID, factID)
		}
		if !isActiveFact(record) {
			continue
		}
		out.Hits = append(out.Hits, FactSearchHit{
			Retrieval: retrieval.CloneHit(hit),
			Fact:      record,
		})
	}
	return out, nil
}

// BuildFactGraph derives graph records from facts, stores them, and projects
// nodes and edges when a fact graph projection is configured.
func (r *Executor) BuildFactGraph(ctx context.Context, facts []fact.Fact) (*FactGraphBuildResult, error) {
	return r.BuildFactGraphScoped(ctx, facts, "")
}

// BuildFactGraphScoped stores graph records and writes any configured
// projection to namespaceOverride instead of the compiler-bound namespace.
func (r *Executor) BuildFactGraphScoped(ctx context.Context, facts []fact.Fact, namespaceOverride string) (*FactGraphBuildResult, error) {
	if err := r.requireFactGraph(); err != nil {
		return nil, err
	}
	facts = activeFacts(facts)
	output, err := r.factGraphBuilder.BuildFactGraph(ctx, FactGraphInput{
		View:  r.factGraph.Descriptor(),
		Facts: facts,
	})
	if err != nil {
		return nil, err
	}

	result := &FactGraphBuildResult{
		Nodes: make([]fact.Node, 0, len(output.Nodes)),
		Edges: make([]fact.Edge, 0, len(output.Edges)),
	}
	for _, node := range output.Nodes {
		written, err := r.factGraph.PutNode(ctx, node)
		if err != nil {
			return nil, err
		}
		result.Nodes = append(result.Nodes, written)
	}
	for _, edge := range output.Edges {
		written, err := r.factGraph.PutEdge(ctx, edge)
		if err != nil {
			return nil, err
		}
		result.Edges = append(result.Edges, written)
	}
	writer, err := r.writerFor(compiler.CapabilityFactGraph, namespaceOverride)
	if err != nil {
		return nil, err
	}
	if writer != nil {
		records := make([]indexed.Record, 0, len(result.Nodes)+len(result.Edges))
		for _, node := range result.Nodes {
			projected, err := projectors.FactNode(node)
			if err != nil {
				return nil, err
			}
			records = append(records, projected)
		}
		for _, edge := range result.Edges {
			projected, err := projectors.FactEdge(edge)
			if err != nil {
				return nil, err
			}
			records = append(records, projected)
		}
		if err := writer.Upsert(ctx, records); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// SearchFactGraph searches the fact graph projection namespace and hydrates node
// and edge hits from the graph store.
func (r *Executor) SearchFactGraph(ctx context.Context, req retrieval.SearchRequest) (*FactGraphSearchResponse, error) {
	return r.searchFactGraph(ctx, req, "")
}

func (r *Executor) searchFactGraph(ctx context.Context, req retrieval.SearchRequest, namespaceOverride string) (*FactGraphSearchResponse, error) {
	if err := r.requireFactGraphSearch(); err != nil {
		return nil, err
	}
	namespace, err := r.namespaceForSearch(compiler.CapabilityFactGraph, namespaceOverride)
	if err != nil {
		return nil, err
	}
	resp, err := r.index.Search(ctx, namespace, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errdefs.NotAvailablef("%s: search projection for capability %q returned nil response", errPrefix, compiler.CapabilityFactGraph)
	}
	out := &FactGraphSearchResponse{Took: resp.Took, Hits: make([]FactGraphSearchHit, 0, len(resp.Hits))}
	for _, hit := range resp.Hits {
		recordType, err := metadataString(hit, projectors.MetadataRecordTypeKey)
		if err != nil {
			return nil, err
		}
		switch recordType {
		case projectors.RecordTypeFactNode:
			nodeID, err := metadataString(hit, projectors.MetadataNodeIDKey)
			if err != nil {
				return nil, err
			}
			node, ok, err := r.factGraph.GetNode(ctx, fact.NodeID(nodeID))
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errdefs.NotAvailablef("%s: hydrate fact graph hit %q: node %q not found", errPrefix, hit.Doc.ID, nodeID)
			}
			out.Hits = append(out.Hits, FactGraphSearchHit{
				Retrieval: retrieval.CloneHit(hit),
				Node:      &node,
			})
		case projectors.RecordTypeFactEdge:
			edgeID, err := metadataString(hit, projectors.MetadataEdgeIDKey)
			if err != nil {
				return nil, err
			}
			edge, ok, err := r.factGraph.GetEdge(ctx, fact.EdgeID(edgeID))
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errdefs.NotAvailablef("%s: hydrate fact graph hit %q: edge %q not found", errPrefix, hit.Doc.ID, edgeID)
			}
			out.Hits = append(out.Hits, FactGraphSearchHit{
				Retrieval: retrieval.CloneHit(hit),
				Edge:      &edge,
			})
		default:
			return nil, errdefs.NotAvailablef("%s: hydrate fact graph hit %q: unknown record type %q", errPrefix, hit.Doc.ID, recordType)
		}
	}
	return out, nil
}

// BuildEntityProfiles derives profile records from fact/graph evidence, stores
// them, and projects them when an entity profile projection is configured.
func (r *Executor) BuildEntityProfiles(ctx context.Context, input EntityBuildInput) ([]viewentity.ProfileRecord, error) {
	return r.BuildEntityProfilesScoped(ctx, input, "")
}

// BuildEntityProfilesScoped stores profile records and writes any configured
// projection to namespaceOverride instead of the compiler-bound namespace.
func (r *Executor) BuildEntityProfilesScoped(ctx context.Context, input EntityBuildInput, namespaceOverride string) ([]viewentity.ProfileRecord, error) {
	if err := r.requireEntityProfile(); err != nil {
		return nil, err
	}
	if err := input.Scope.Validate(); err != nil {
		return nil, errdefs.Validationf("%s: invalid entity profile scope: %w", errPrefix, err)
	}
	input.Facts = activeFacts(input.Facts)
	records, err := r.entityProfileBuilder.BuildEntityProfiles(ctx, EntityProfileInput{
		View:  r.entityProfile.Descriptor(),
		Scope: input.Scope,
		Facts: input.Facts,
		Graph: entityGraphOutput(input.Graph),
	})
	if err != nil {
		return nil, err
	}

	stored := make([]viewentity.ProfileRecord, 0, len(records))
	for _, record := range records {
		written, err := r.entityProfile.Put(ctx, record)
		if err != nil {
			return nil, err
		}
		stored = append(stored, written)
	}
	writer, err := r.writerFor(compiler.CapabilityEntityProfile, namespaceOverride)
	if err != nil {
		return nil, err
	}
	if writer != nil {
		records := make([]indexed.Record, 0, len(stored))
		for _, record := range stored {
			projected, err := projectors.EntityProfile(record)
			if err != nil {
				return nil, err
			}
			records = append(records, projected)
		}
		if err := writer.Upsert(ctx, records); err != nil {
			return nil, err
		}
	}
	return stored, nil
}

// SearchEntityProfiles searches the entity profile projection namespace and
// hydrates every hit from the semantic profile store.
func (r *Executor) SearchEntityProfiles(ctx context.Context, req retrieval.SearchRequest) (*EntityProfileSearchResponse, error) {
	return r.searchEntityProfiles(ctx, req, "")
}

func (r *Executor) searchEntityProfiles(ctx context.Context, req retrieval.SearchRequest, namespaceOverride string) (*EntityProfileSearchResponse, error) {
	if err := r.requireEntityProfileSearch(); err != nil {
		return nil, err
	}
	namespace, err := r.namespaceForSearch(compiler.CapabilityEntityProfile, namespaceOverride)
	if err != nil {
		return nil, err
	}
	resp, err := r.index.Search(ctx, namespace, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errdefs.NotAvailablef("%s: search projection for capability %q returned nil response", errPrefix, compiler.CapabilityEntityProfile)
	}
	out := &EntityProfileSearchResponse{Took: resp.Took, Hits: make([]EntityProfileSearchHit, 0, len(resp.Hits))}
	for _, hit := range resp.Hits {
		scope, err := metadataScope(hit)
		if err != nil {
			return nil, err
		}
		profileID, err := metadataString(hit, projectors.MetadataProfileIDKey)
		if err != nil {
			return nil, err
		}
		record, ok, err := r.entityProfile.Get(ctx, scope, viewentity.ProfileID(profileID))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errdefs.NotAvailablef("%s: hydrate entity profile hit %q: profile %q/%q/%q/%q not found", errPrefix, hit.Doc.ID, scope.RuntimeID, scope.UserID, scope.EntityID, profileID)
		}
		out.Hits = append(out.Hits, EntityProfileSearchHit{
			Retrieval: retrieval.CloneHit(hit),
			Profile:   record,
		})
	}
	return out, nil
}

// BuildEntityTimeline derives timeline events from fact/graph evidence, stores
// them, and projects them when an entity timeline projection is configured.
func (r *Executor) BuildEntityTimeline(ctx context.Context, input EntityBuildInput) ([]viewentity.Event, error) {
	return r.BuildEntityTimelineScoped(ctx, input, "")
}

// BuildEntityTimelineScoped stores timeline events and writes any configured
// projection to namespaceOverride instead of the compiler-bound namespace.
func (r *Executor) BuildEntityTimelineScoped(ctx context.Context, input EntityBuildInput, namespaceOverride string) ([]viewentity.Event, error) {
	if err := r.requireEntityTimeline(); err != nil {
		return nil, err
	}
	if err := input.Scope.Validate(); err != nil {
		return nil, errdefs.Validationf("%s: invalid entity timeline scope: %w", errPrefix, err)
	}
	input.Facts = activeFacts(input.Facts)
	events, err := r.entityTimelineBuilder.BuildEntityTimeline(ctx, EntityTimelineInput{
		View:  r.entityTimeline.Descriptor(),
		Scope: input.Scope,
		Facts: input.Facts,
		Graph: entityGraphOutput(input.Graph),
	})
	if err != nil {
		return nil, err
	}

	stored := make([]viewentity.Event, 0, len(events))
	for _, event := range events {
		written, err := r.entityTimeline.Put(ctx, event)
		if err != nil {
			return nil, err
		}
		stored = append(stored, written)
	}
	writer, err := r.writerFor(compiler.CapabilityEntityTimeline, namespaceOverride)
	if err != nil {
		return nil, err
	}
	if writer != nil {
		records := make([]indexed.Record, 0, len(stored))
		for _, event := range stored {
			projected, err := projectors.EntityEvent(event)
			if err != nil {
				return nil, err
			}
			records = append(records, projected)
		}
		if err := writer.Upsert(ctx, records); err != nil {
			return nil, err
		}
	}
	return stored, nil
}

// SearchEntityTimeline searches the entity timeline projection namespace and
// hydrates every hit from the semantic timeline store.
func (r *Executor) SearchEntityTimeline(ctx context.Context, req retrieval.SearchRequest) (*EntityTimelineSearchResponse, error) {
	return r.searchEntityTimeline(ctx, req, "")
}

func (r *Executor) searchEntityTimeline(ctx context.Context, req retrieval.SearchRequest, namespaceOverride string) (*EntityTimelineSearchResponse, error) {
	if err := r.requireEntityTimelineSearch(); err != nil {
		return nil, err
	}
	namespace, err := r.namespaceForSearch(compiler.CapabilityEntityTimeline, namespaceOverride)
	if err != nil {
		return nil, err
	}
	resp, err := r.index.Search(ctx, namespace, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errdefs.NotAvailablef("%s: search projection for capability %q returned nil response", errPrefix, compiler.CapabilityEntityTimeline)
	}
	out := &EntityTimelineSearchResponse{Took: resp.Took, Hits: make([]EntityTimelineSearchHit, 0, len(resp.Hits))}
	for _, hit := range resp.Hits {
		scope, err := metadataScope(hit)
		if err != nil {
			return nil, err
		}
		eventID, err := metadataString(hit, projectors.MetadataEventIDKey)
		if err != nil {
			return nil, err
		}
		event, ok, err := r.entityTimeline.Get(ctx, scope, viewentity.EventID(eventID))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errdefs.NotAvailablef("%s: hydrate entity timeline hit %q: event %q/%q/%q/%q not found", errPrefix, hit.Doc.ID, scope.RuntimeID, scope.UserID, scope.EntityID, eventID)
		}
		out.Hits = append(out.Hits, EntityTimelineSearchHit{
			Retrieval: retrieval.CloneHit(hit),
			Event:     event,
		})
	}
	return out, nil
}

func entityGraphOutput(graph *FactGraphBuildResult) FactGraphOutput {
	if graph == nil {
		return FactGraphOutput{}
	}
	return FactGraphOutput{
		Nodes: graph.Nodes,
		Edges: graph.Edges,
	}
}

func factReconcileScope(observations []viewobservation.Observation) (views.Scope, error) {
	if len(observations) == 0 {
		return views.Scope{}, nil
	}
	scope := observations[0].Scope
	if err := scope.Validate(); err != nil {
		return views.Scope{}, errdefs.Validationf("%s: invalid fact reconcile scope: %w", errPrefix, err)
	}
	for _, observation := range observations[1:] {
		if observation.Scope != scope {
			return views.Scope{}, errdefs.Validationf("%s: observations must share one scope for fact reconciliation", errPrefix)
		}
	}
	return scope, nil
}

func activeFacts(in []fact.Fact) []fact.Fact {
	if len(in) == 0 {
		return nil
	}
	out := make([]fact.Fact, 0, len(in))
	for _, record := range in {
		if isActiveFact(record) {
			out = append(out, record)
		}
	}
	return out
}

func isActiveFact(record fact.Fact) bool {
	return record.Status == "" || record.Status == fact.FactActive
}

// WholeDocumentChunker is a deterministic chunker useful for tests and simple
// callers that explicitly opt in. New never installs it automatically.
type WholeDocumentChunker struct {
	Layer              viewdocument.Layer
	TransformSignature string
}

func (c WholeDocumentChunker) ChunkDocument(_ context.Context, input DocumentChunkInput) ([]viewdocument.Chunk, error) {
	if strings.TrimSpace(input.Document.Content) == "" {
		return nil, nil
	}
	layer := c.Layer
	if layer.Name == "" {
		layer.Name = "whole_document"
	}
	if layer.Version == "" {
		layer.Version = "v1"
	}
	transformSignature := c.TransformSignature
	if transformSignature == "" {
		transformSignature = layer.TransformSignature
	}
	if transformSignature == "" {
		transformSignature = "whole_document:v1"
	}
	if layer.TransformSignature == "" {
		layer.TransformSignature = transformSignature
	}

	doc := input.Document
	span := views.Span{Start: 0, End: len(doc.Content)}
	ref := views.SourceRef{
		Kind: views.SourceDocument,
		Document: &views.DocumentSourceRef{
			DatasetID:   doc.DatasetID,
			DocumentID:  doc.ID,
			Version:     strconv.FormatUint(doc.Version, 10),
			ContentHash: doc.ContentHash,
			Span:        &span,
		},
	}
	return []viewdocument.Chunk{{
		ID:         "whole",
		Scope:      input.Scope,
		DocumentID: doc.ID,
		Layer:      layer,
		Ordinal:    0,
		Span:       span,
		Text:       doc.Content,
		SourceRef:  ref,
		Signature: views.ViewSignature{
			ViewID: input.View.ID,
			SourceRevisions: []views.SourceRevision{{
				Kind:        views.SourceDocument,
				SourceKey:   ref.StableKey(),
				Revision:    strconv.FormatUint(doc.Version, 10),
				ContentHash: doc.ContentHash,
				ObservedAt:  doc.UpdatedAt,
			}},
			TransformSignature: transformSignature,
		},
	}}, nil
}

func (r *Executor) requireDocumentChunks() error {
	if _, ok := r.enabled[compiler.CapabilityDocumentChunks]; !ok {
		return errdefs.NotAvailablef("%s: capability %q is not configured", errPrefix, compiler.CapabilityDocumentChunks)
	}
	if r.documentStore == nil {
		return errdefs.NotAvailablef("%s: document store is not configured", errPrefix)
	}
	if r.documentChunks == nil {
		return errdefs.NotAvailablef("%s: document chunks view is not configured", errPrefix)
	}
	if r.documentChunker == nil {
		return errdefs.NotAvailablef("%s: DocumentChunker is not configured", errPrefix)
	}
	return nil
}

func (r *Executor) requireDocumentChunkSearch() error {
	if r.documentChunks == nil {
		return errdefs.NotAvailablef("%s: document chunks view is not configured", errPrefix)
	}
	return r.requireProjection(compiler.CapabilityDocumentChunks)
}

func (r *Executor) requireSummaryDAG() error {
	if _, ok := r.enabled[compiler.CapabilitySummaryDAG]; !ok {
		return errdefs.NotAvailablef("%s: capability %q is not configured", errPrefix, compiler.CapabilitySummaryDAG)
	}
	if r.recentWindow == nil {
		return errdefs.NotAvailablef("%s: recent window is not configured", errPrefix)
	}
	if r.summaryDAG == nil {
		return errdefs.NotAvailablef("%s: summary dag view is not configured", errPrefix)
	}
	if r.summarizer == nil {
		return errdefs.NotAvailablef("%s: Summarizer is not configured", errPrefix)
	}
	return nil
}

func (r *Executor) requireSummaryNodeSearch() error {
	if r.summaryDAG == nil {
		return errdefs.NotAvailablef("%s: summary dag view is not configured", errPrefix)
	}
	return r.requireProjection(compiler.CapabilitySummaryDAG)
}

func (r *Executor) requireObservationLedger() error {
	if _, ok := r.enabled[compiler.CapabilityObservationLedger]; !ok {
		return errdefs.NotAvailablef("%s: capability %q is not configured", errPrefix, compiler.CapabilityObservationLedger)
	}
	if r.recentWindow == nil {
		return errdefs.NotAvailablef("%s: recent window is not configured", errPrefix)
	}
	if r.observationLedger == nil {
		return errdefs.NotAvailablef("%s: observation ledger view is not configured", errPrefix)
	}
	if r.observationExtractor == nil {
		return errdefs.NotAvailablef("%s: ObservationExtractor is not configured", errPrefix)
	}
	return nil
}

func (r *Executor) requireObservationSearch() error {
	if r.observationLedger == nil {
		return errdefs.NotAvailablef("%s: observation ledger view is not configured", errPrefix)
	}
	return r.requireProjection(compiler.CapabilityObservationLedger)
}

func (r *Executor) requireFactLedger() error {
	if _, ok := r.enabled[compiler.CapabilityFactLedger]; !ok {
		return errdefs.NotAvailablef("%s: capability %q is not configured", errPrefix, compiler.CapabilityFactLedger)
	}
	if r.factLedger == nil {
		return errdefs.NotAvailablef("%s: fact ledger view is not configured", errPrefix)
	}
	if r.factReconciler == nil {
		return errdefs.NotAvailablef("%s: FactReconciler is not configured", errPrefix)
	}
	return nil
}

func (r *Executor) requireFactSearch() error {
	if r.factLedger == nil {
		return errdefs.NotAvailablef("%s: fact ledger view is not configured", errPrefix)
	}
	return r.requireProjection(compiler.CapabilityFactLedger)
}

func (r *Executor) requireFactGraph() error {
	if _, ok := r.enabled[compiler.CapabilityFactGraph]; !ok {
		return errdefs.NotAvailablef("%s: capability %q is not configured", errPrefix, compiler.CapabilityFactGraph)
	}
	if r.factGraph == nil {
		return errdefs.NotAvailablef("%s: fact graph view is not configured", errPrefix)
	}
	if r.factGraphBuilder == nil {
		return errdefs.NotAvailablef("%s: FactGraphBuilder is not configured", errPrefix)
	}
	return nil
}

func (r *Executor) requireFactGraphSearch() error {
	if r.factGraph == nil {
		return errdefs.NotAvailablef("%s: fact graph view is not configured", errPrefix)
	}
	return r.requireProjection(compiler.CapabilityFactGraph)
}

func (r *Executor) requireEntityProfile() error {
	if _, ok := r.enabled[compiler.CapabilityEntityProfile]; !ok {
		return errdefs.NotAvailablef("%s: capability %q is not configured", errPrefix, compiler.CapabilityEntityProfile)
	}
	if r.entityProfile == nil {
		return errdefs.NotAvailablef("%s: entity profile view is not configured", errPrefix)
	}
	if r.entityProfileBuilder == nil {
		return errdefs.NotAvailablef("%s: EntityProfileBuilder is not configured", errPrefix)
	}
	return nil
}

func (r *Executor) requireEntityProfileSearch() error {
	if r.entityProfile == nil {
		return errdefs.NotAvailablef("%s: entity profile view is not configured", errPrefix)
	}
	return r.requireProjection(compiler.CapabilityEntityProfile)
}

func (r *Executor) requireEntityTimeline() error {
	if _, ok := r.enabled[compiler.CapabilityEntityTimeline]; !ok {
		return errdefs.NotAvailablef("%s: capability %q is not configured", errPrefix, compiler.CapabilityEntityTimeline)
	}
	if r.entityTimeline == nil {
		return errdefs.NotAvailablef("%s: entity timeline view is not configured", errPrefix)
	}
	if r.entityTimelineBuilder == nil {
		return errdefs.NotAvailablef("%s: EntityTimelineBuilder is not configured", errPrefix)
	}
	return nil
}

func (r *Executor) requireEntityTimelineSearch() error {
	if r.entityTimeline == nil {
		return errdefs.NotAvailablef("%s: entity timeline view is not configured", errPrefix)
	}
	return r.requireProjection(compiler.CapabilityEntityTimeline)
}

func (r *Executor) requireProjection(capability compiler.Capability) error {
	if _, ok := r.projections[capability]; !ok {
		return errdefs.NotAvailablef("%s: projection for capability %q is not configured", errPrefix, capability)
	}
	if r.writers[capability] == nil || r.index == nil {
		return errdefs.NotAvailablef("%s: projection writer for capability %q is not configured", errPrefix, capability)
	}
	return nil
}

func (r *Executor) writerFor(capability compiler.Capability, namespaceOverride string) (*indexed.Writer, error) {
	writer := r.writers[capability]
	if strings.TrimSpace(namespaceOverride) == "" || writer == nil {
		return writer, nil
	}
	projection := r.projections[capability]
	binding := projection.Binding
	binding.Namespace = namespaceOverride
	return r.newProjectionWriter(binding)
}

func (r *Executor) namespaceForSearch(capability compiler.Capability, namespaceOverride string) (string, error) {
	projection := r.projections[capability]
	binding := projection.Binding
	if strings.TrimSpace(namespaceOverride) != "" {
		binding.Namespace = namespaceOverride
	}
	if err := binding.Validate(); err != nil {
		return "", err
	}
	return binding.Namespace, nil
}

func metadataString(hit retrieval.Hit, key string) (string, error) {
	value, ok := hit.Doc.Metadata[key]
	if !ok {
		return "", errdefs.NotAvailablef("%s: hydrate hit %q: metadata %q is missing", errPrefix, hit.Doc.ID, key)
	}
	out, ok := value.(string)
	if !ok || out == "" {
		return "", errdefs.NotAvailablef("%s: hydrate hit %q: metadata %q has invalid value %v", errPrefix, hit.Doc.ID, key, value)
	}
	return out, nil
}

func metadataScope(hit retrieval.Hit) (views.Scope, error) {
	runtimeID, err := metadataString(hit, projectors.MetadataRuntimeIDKey)
	if err != nil {
		return views.Scope{}, err
	}
	return views.Scope{
		RuntimeID:      runtimeID,
		UserID:         metadataOptionalString(hit, projectors.MetadataUserIDKey),
		AgentID:        metadataOptionalString(hit, projectors.MetadataAgentIDKey),
		ConversationID: metadataOptionalString(hit, projectors.MetadataConversationIDKey),
		DatasetID:      metadataOptionalString(hit, projectors.MetadataDatasetIDKey),
		EntityID:       metadataOptionalString(hit, projectors.MetadataEntityIDKey),
	}, nil
}

func metadataOptionalString(hit retrieval.Hit, key string) string {
	value, ok := hit.Doc.Metadata[key]
	if !ok {
		return ""
	}
	out, _ := value.(string)
	return out
}
