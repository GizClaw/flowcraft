package executor

import (
	"context"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	viewentity "github.com/GizClaw/flowcraft/memory/views/entity"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	viewobservation "github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/memory/views/recent"
)

func (r *Executor) applyContextPacker(ctx context.Context, req PackContextRequest, pack *ContextPack) error {
	output, err := r.contextPacker.PackContext(ctx, cloneContextPackInput(ContextPackInput{
		Scope:              contextPackScope(req),
		Query:              contextPackQuery(req),
		Window:             pack.Window,
		Items:              pack.Items,
		SummaryHits:        pack.SummaryHits,
		DocumentHits:       pack.DocumentHits,
		ObservationHits:    pack.ObservationHits,
		FactHits:           pack.FactHits,
		FactGraphHits:      pack.FactGraphHits,
		EntityProfileHits:  pack.EntityProfileHits,
		EntityTimelineHits: pack.EntityTimelineHits,
	}))
	if err != nil {
		return err
	}
	pack.Items = cloneContextItems(output.Items)
	return nil
}

func contextPackScope(req PackContextRequest) views.Scope {
	if !req.Scope.IsZero() {
		return req.Scope
	}
	return req.Window.Scope
}

func contextPackQuery(req PackContextRequest) string {
	if strings.TrimSpace(req.Query) != "" {
		return req.Query
	}
	for _, search := range []*retrieval.SearchRequest{
		req.SummarySearch,
		req.DocumentSearch,
		req.ObservationSearch,
		req.FactSearch,
		req.FactGraphSearch,
		req.EntityProfileSearch,
		req.EntityTimelineSearch,
	} {
		if search != nil && strings.TrimSpace(search.QueryText) != "" {
			return search.QueryText
		}
	}
	return ""
}

func cloneContextPackInput(in ContextPackInput) ContextPackInput {
	out := in
	out.Window = cloneWindowResult(in.Window)
	out.Items = cloneContextItems(in.Items)
	out.SummaryHits = cloneSummaryNodeSearchHits(in.SummaryHits)
	out.DocumentHits = cloneDocumentChunkSearchHits(in.DocumentHits)
	out.ObservationHits = cloneObservationSearchHits(in.ObservationHits)
	out.FactHits = cloneFactSearchHits(in.FactHits)
	out.FactGraphHits = cloneFactGraphSearchHits(in.FactGraphHits)
	out.EntityProfileHits = cloneEntityProfileSearchHits(in.EntityProfileHits)
	out.EntityTimelineHits = cloneEntityTimelineSearchHits(in.EntityTimelineHits)
	return out
}

func cloneWindowResult(in recent.WindowResult) recent.WindowResult {
	out := in
	if in.Messages != nil {
		out.Messages = make([]sourcemessage.Message, len(in.Messages))
		for i, msg := range in.Messages {
			out.Messages[i] = cloneMessage(msg)
		}
	}
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	return out
}

func cloneContextItems(in []ContextItem) []ContextItem {
	if in == nil {
		return nil
	}
	out := make([]ContextItem, len(in))
	for i, item := range in {
		out[i] = cloneContextItem(item)
	}
	return out
}

func cloneContextItem(in ContextItem) ContextItem {
	out := in
	if in.Message != nil {
		msg := cloneMessage(*in.Message)
		out.Message = &msg
	}
	if in.SummaryNode != nil {
		node := cloneSummaryNode(*in.SummaryNode)
		out.SummaryNode = &node
	}
	if in.DocumentChunk != nil {
		chunk := cloneDocumentChunk(*in.DocumentChunk)
		out.DocumentChunk = &chunk
	}
	if in.Observation != nil {
		observation := cloneObservation(*in.Observation)
		out.Observation = &observation
	}
	if in.Fact != nil {
		record := cloneFact(*in.Fact)
		out.Fact = &record
	}
	if in.FactGraphNode != nil {
		node := cloneFactGraphNode(*in.FactGraphNode)
		out.FactGraphNode = &node
	}
	if in.FactGraphEdge != nil {
		edge := cloneFactGraphEdge(*in.FactGraphEdge)
		out.FactGraphEdge = &edge
	}
	if in.EntityProfile != nil {
		profile := cloneEntityProfile(*in.EntityProfile)
		out.EntityProfile = &profile
	}
	if in.EntityEvent != nil {
		event := cloneEntityEvent(*in.EntityEvent)
		out.EntityEvent = &event
	}
	if in.Retrieval != nil {
		hit := cloneRetrievalHit(*in.Retrieval)
		out.Retrieval = &hit
	}
	return out
}

func cloneSummaryNodeSearchHits(in []SummaryNodeSearchHit) []SummaryNodeSearchHit {
	if in == nil {
		return nil
	}
	out := make([]SummaryNodeSearchHit, len(in))
	for i, hit := range in {
		out[i] = SummaryNodeSearchHit{
			Retrieval: cloneRetrievalHit(hit.Retrieval),
			Node:      cloneSummaryNode(hit.Node),
		}
	}
	return out
}

func cloneDocumentChunkSearchHits(in []DocumentChunkSearchHit) []DocumentChunkSearchHit {
	if in == nil {
		return nil
	}
	out := make([]DocumentChunkSearchHit, len(in))
	for i, hit := range in {
		out[i] = DocumentChunkSearchHit{
			Retrieval: cloneRetrievalHit(hit.Retrieval),
			Chunk:     cloneDocumentChunk(hit.Chunk),
		}
	}
	return out
}

func cloneObservationSearchHits(in []ObservationSearchHit) []ObservationSearchHit {
	if in == nil {
		return nil
	}
	out := make([]ObservationSearchHit, len(in))
	for i, hit := range in {
		out[i] = ObservationSearchHit{
			Retrieval:   cloneRetrievalHit(hit.Retrieval),
			Observation: cloneObservation(hit.Observation),
		}
	}
	return out
}

func cloneFactSearchHits(in []FactSearchHit) []FactSearchHit {
	if in == nil {
		return nil
	}
	out := make([]FactSearchHit, len(in))
	for i, hit := range in {
		out[i] = FactSearchHit{
			Retrieval: cloneRetrievalHit(hit.Retrieval),
			Fact:      cloneFact(hit.Fact),
		}
	}
	return out
}

func cloneFactGraphSearchHits(in []FactGraphSearchHit) []FactGraphSearchHit {
	if in == nil {
		return nil
	}
	out := make([]FactGraphSearchHit, len(in))
	for i, hit := range in {
		out[i].Retrieval = cloneRetrievalHit(hit.Retrieval)
		if hit.Node != nil {
			node := cloneFactGraphNode(*hit.Node)
			out[i].Node = &node
		}
		if hit.Edge != nil {
			edge := cloneFactGraphEdge(*hit.Edge)
			out[i].Edge = &edge
		}
	}
	return out
}

func cloneEntityProfileSearchHits(in []EntityProfileSearchHit) []EntityProfileSearchHit {
	if in == nil {
		return nil
	}
	out := make([]EntityProfileSearchHit, len(in))
	for i, hit := range in {
		out[i] = EntityProfileSearchHit{
			Retrieval: cloneRetrievalHit(hit.Retrieval),
			Profile:   cloneEntityProfile(hit.Profile),
		}
	}
	return out
}

func cloneEntityTimelineSearchHits(in []EntityTimelineSearchHit) []EntityTimelineSearchHit {
	if in == nil {
		return nil
	}
	out := make([]EntityTimelineSearchHit, len(in))
	for i, hit := range in {
		out[i] = EntityTimelineSearchHit{
			Retrieval: cloneRetrievalHit(hit.Retrieval),
			Event:     cloneEntityEvent(hit.Event),
		}
	}
	return out
}

func cloneMessage(in sourcemessage.Message) sourcemessage.Message {
	out := in
	out.Message = in.Message.Clone()
	out.Metadata = cloneAnyMap(in.Metadata)
	if in.SpanRefs != nil {
		out.SpanRefs = append([]sourcemessage.SpanRef(nil), in.SpanRefs...)
	}
	return out
}

func cloneSummaryNode(in recent.SummaryNode) recent.SummaryNode {
	out := in
	if in.ParentIDs != nil {
		out.ParentIDs = append([]recent.NodeID(nil), in.ParentIDs...)
	}
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Signature = cloneViewSignature(in.Signature)
	out.Metadata = cloneAnyMap(in.Metadata)
	return out
}

func cloneDocumentChunk(in viewdocument.Chunk) viewdocument.Chunk {
	out := in
	out.SourceRef = cloneSourceRef(in.SourceRef)
	out.Signature = cloneViewSignature(in.Signature)
	out.Metadata = cloneAnyMap(in.Metadata)
	return out
}

func cloneObservation(in viewobservation.Observation) viewobservation.Observation {
	out := in
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Signature = cloneViewSignature(in.Signature)
	out.Metadata = cloneAnyMap(in.Metadata)
	return out
}

func cloneFact(in fact.Fact) fact.Fact {
	out := in
	out.Supersedes = append([]fact.FactID(nil), in.Supersedes...)
	out.SupersededBy = append([]fact.FactID(nil), in.SupersededBy...)
	out.ConflictWith = append([]fact.FactID(nil), in.ConflictWith...)
	out.ValidFrom = cloneTimePtr(in.ValidFrom)
	out.ValidUntil = cloneTimePtr(in.ValidUntil)
	out.RetractedAt = cloneTimePtr(in.RetractedAt)
	out.ResolvedAt = cloneTimePtr(in.ResolvedAt)
	if in.ObservationRefs != nil {
		out.ObservationRefs = append([]fact.ObservationRef(nil), in.ObservationRefs...)
	}
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Signature = cloneViewSignature(in.Signature)
	out.Metadata = cloneAnyMap(in.Metadata)
	return out
}

func cloneFactGraphNode(in fact.Node) fact.Node {
	out := in
	if in.Aliases != nil {
		out.Aliases = append([]string(nil), in.Aliases...)
	}
	out.FactRefs = cloneFactRefs(in.FactRefs)
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Signature = cloneViewSignature(in.Signature)
	out.Metadata = cloneAnyMap(in.Metadata)
	return out
}

func cloneFactGraphEdge(in fact.Edge) fact.Edge {
	out := in
	out.ValidFrom = cloneTimePtr(in.ValidFrom)
	out.ValidUntil = cloneTimePtr(in.ValidUntil)
	out.FactRefs = cloneFactRefs(in.FactRefs)
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Signature = cloneViewSignature(in.Signature)
	out.Metadata = cloneAnyMap(in.Metadata)
	return out
}

func cloneEntityProfile(in viewentity.ProfileRecord) viewentity.ProfileRecord {
	out := in
	if in.Slots != nil {
		out.Slots = make([]viewentity.Slot, len(in.Slots))
		for i, slot := range in.Slots {
			out.Slots[i] = slot
			out.Slots[i].FactRefs = cloneFactRefs(slot.FactRefs)
			out.Slots[i].Metadata = cloneAnyMap(slot.Metadata)
		}
	}
	out.FactRefs = cloneFactRefs(in.FactRefs)
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Signature = cloneViewSignature(in.Signature)
	out.Metadata = cloneAnyMap(in.Metadata)
	return out
}

func cloneEntityEvent(in viewentity.Event) viewentity.Event {
	out := in
	out.OccurredAt = cloneTimePtr(in.OccurredAt)
	out.ValidFrom = cloneTimePtr(in.ValidFrom)
	out.ValidUntil = cloneTimePtr(in.ValidUntil)
	out.FactRefs = cloneFactRefs(in.FactRefs)
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Signature = cloneViewSignature(in.Signature)
	out.Metadata = cloneAnyMap(in.Metadata)
	return out
}

func cloneRetrievalHit(in retrieval.Hit) retrieval.Hit {
	out := in
	out.Doc = cloneRetrievalDoc(in.Doc)
	if in.Scores != nil {
		out.Scores = make(map[string]float64, len(in.Scores))
		for k, v := range in.Scores {
			out.Scores[k] = v
		}
	}
	return out
}

func cloneRetrievalDoc(in retrieval.Doc) retrieval.Doc {
	out := in
	if in.Vector != nil {
		out.Vector = append([]float32(nil), in.Vector...)
	}
	out.Metadata = cloneAnyMap(in.Metadata)
	if in.SparseVector != nil {
		out.SparseVector = make(map[string]float32, len(in.SparseVector))
		for k, v := range in.SparseVector {
			out.SparseVector[k] = v
		}
	}
	return out
}

func cloneFactRefs(in []fact.FactRef) []fact.FactRef {
	if in == nil {
		return nil
	}
	return append([]fact.FactRef(nil), in...)
}

func cloneSourceRefs(in []views.SourceRef) []views.SourceRef {
	if in == nil {
		return nil
	}
	out := make([]views.SourceRef, len(in))
	for i, ref := range in {
		out[i] = cloneSourceRef(ref)
	}
	return out
}

func cloneSourceRef(in views.SourceRef) views.SourceRef {
	out := in
	if in.Message != nil {
		msg := *in.Message
		if in.Message.Span != nil {
			span := *in.Message.Span
			msg.Span = &span
		}
		out.Message = &msg
	}
	if in.Document != nil {
		doc := *in.Document
		if in.Document.Span != nil {
			span := *in.Document.Span
			doc.Span = &span
		}
		out.Document = &doc
	}
	return out
}

func cloneViewSignature(in views.ViewSignature) views.ViewSignature {
	out := in
	if in.SourceRevisions != nil {
		out.SourceRevisions = append([]views.SourceRevision(nil), in.SourceRevisions...)
	}
	if in.UpstreamViewRefs != nil {
		out.UpstreamViewRefs = append([]views.UpstreamViewRef(nil), in.UpstreamViewRefs...)
	}
	if in.DiagnosticSignatures != nil {
		out.DiagnosticSignatures = make(map[string]string, len(in.DiagnosticSignatures))
		for k, v := range in.DiagnosticSignatures {
			out.DiagnosticSignatures[k] = v
		}
	}
	return out
}

func cloneTimePtr(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(in any) any {
	switch v := in.(type) {
	case map[string]any:
		return cloneAnyMap(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneAny(item)
		}
		return out
	default:
		return v
	}
}
