package executor

import (
	"context"
	"maps"
	"strings"

	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
	"github.com/GizClaw/flowcraft/memory/views/recent"
)

func (r *Executor) applyContextPacker(ctx context.Context, req PackContextRequest, pack *ContextPack) error {
	output, err := r.contextPacker.PackContext(ctx, cloneContextPackInput(derive.ContextPackInput{
		Scope:              contextPackScope(req),
		Query:              contextPackQuery(req),
		Options:            req.PackOptions,
		Window:             pack.Window,
		SourceMessages:     sourceMessageResolver{store: r.messageStore},
		EntityGraphSources: entityGraphSourceResolver{store: r.entityFacts},
		Items:              pack.Items,
		MessageHits:        pack.MessageHits,
		SummaryHits:        pack.SummaryHits,
		DocumentHits:       pack.DocumentHits,
		EntityHits:         pack.EntityHits,
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
		req.MessageSearch,
		req.SummarySearch,
		req.DocumentSearch,
		req.EntityFactSearch,
	} {
		if search != nil && strings.TrimSpace(search.QueryText) != "" {
			return search.QueryText
		}
	}
	return ""
}

type sourceMessageResolver struct {
	store sourcemessage.Store
}

func (r sourceMessageResolver) GetSourceMessage(ctx context.Context, conversationID, messageID string) (sourcemessage.Message, bool, error) {
	if r.store == nil {
		return sourcemessage.Message{}, false, nil
	}
	return r.store.Get(ctx, conversationID, messageID)
}

func (r sourceMessageResolver) GetSourceMessageNeighbors(ctx context.Context, conversationID, messageID string, before, after int) ([]sourcemessage.Message, error) {
	if r.store == nil || conversationID == "" || messageID == "" || (before <= 0 && after <= 0) {
		return nil, nil
	}
	messages, err := r.store.List(ctx, conversationID, sourcemessage.ListOptions{})
	if err != nil {
		return nil, err
	}
	anchor := -1
	for i, msg := range messages {
		if msg.ID == messageID {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return nil, nil
	}
	var out []sourcemessage.Message
	maxDistance := max(after, before)
	for distance := 1; distance <= maxDistance; distance++ {
		if distance <= before {
			if idx := anchor - distance; idx >= 0 {
				out = append(out, cloneMessage(messages[idx]))
			}
		}
		if distance <= after {
			if idx := anchor + distance; idx < len(messages) {
				out = append(out, cloneMessage(messages[idx]))
			}
		}
	}
	return out, nil
}

type entityGraphSourceResolver struct {
	store viewentityfact.Store
}

func (r entityGraphSourceResolver) ExpandGraphSources(ctx context.Context, scope views.Scope, seedFacts []viewentityfact.GraphSeedFact, opts viewentityfact.GraphExpansionOptions) (viewentityfact.GraphExpansionResult, error) {
	if r.store == nil {
		return viewentityfact.GraphExpansionResult{}, nil
	}
	return viewentityfact.ExpandGraphSources(ctx, r.store, scope, seedFacts, opts)
}

func cloneContextPackInput(in derive.ContextPackInput) derive.ContextPackInput {
	out := in
	out.Window = cloneWindowResult(in.Window)
	out.Items = cloneContextItems(in.Items)
	out.MessageHits = cloneSourceMessageSearchHits(in.MessageHits)
	out.SummaryHits = cloneSummaryNodeSearchHits(in.SummaryHits)
	out.DocumentHits = cloneDocumentChunkSearchHits(in.DocumentHits)
	out.EntityHits = cloneEntityFactSearchHits(in.EntityHits)
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

func cloneContextItems(in []derive.ContextItem) []derive.ContextItem {
	if in == nil {
		return nil
	}
	out := make([]derive.ContextItem, len(in))
	for i, item := range in {
		out[i] = cloneContextItem(item)
	}
	return out
}

func cloneContextItem(in derive.ContextItem) derive.ContextItem {
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
	if in.EntityFact != nil {
		fact := cloneEntityFact(*in.EntityFact)
		out.EntityFact = &fact
	}
	if in.Retrieval != nil {
		hit := cloneRetrievalHit(*in.Retrieval)
		out.Retrieval = &hit
	}
	return out
}

func cloneSourceMessageSearchHits(in []derive.SourceMessageSearchHit) []derive.SourceMessageSearchHit {
	if in == nil {
		return nil
	}
	out := make([]derive.SourceMessageSearchHit, len(in))
	for i, hit := range in {
		out[i] = derive.SourceMessageSearchHit{
			Retrieval: cloneRetrievalHit(hit.Retrieval),
			Message:   cloneMessage(hit.Message),
		}
	}
	return out
}

func cloneSummaryNodeSearchHits(in []derive.SummaryNodeSearchHit) []derive.SummaryNodeSearchHit {
	if in == nil {
		return nil
	}
	out := make([]derive.SummaryNodeSearchHit, len(in))
	for i, hit := range in {
		out[i] = derive.SummaryNodeSearchHit{
			Retrieval: cloneRetrievalHit(hit.Retrieval),
			Node:      cloneSummaryNode(hit.Node),
		}
	}
	return out
}

func cloneDocumentChunkSearchHits(in []derive.DocumentChunkSearchHit) []derive.DocumentChunkSearchHit {
	if in == nil {
		return nil
	}
	out := make([]derive.DocumentChunkSearchHit, len(in))
	for i, hit := range in {
		out[i] = derive.DocumentChunkSearchHit{
			Retrieval: cloneRetrievalHit(hit.Retrieval),
			Chunk:     cloneDocumentChunk(hit.Chunk),
		}
	}
	return out
}

func cloneEntityFactSearchHits(in []derive.EntityFactSearchHit) []derive.EntityFactSearchHit {
	if in == nil {
		return nil
	}
	out := make([]derive.EntityFactSearchHit, len(in))
	for i, hit := range in {
		out[i] = derive.EntityFactSearchHit{
			Retrieval: cloneRetrievalHit(hit.Retrieval),
			Fact:      cloneEntityFact(hit.Fact),
		}
	}
	return out
}

func cloneMessage(in sourcemessage.Message) sourcemessage.Message {
	out := in
	out.Message = in.Clone()
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

func cloneEntityFact(in viewentityfact.Fact) viewentityfact.Fact {
	return viewentityfact.CloneFact(in)
}

func cloneRetrievalHit(in retrieval.Hit) retrieval.Hit {
	out := in
	out.Doc = cloneRetrievalDoc(in.Doc)
	if in.Scores != nil {
		out.Scores = make(map[string]float64, len(in.Scores))
		maps.Copy(out.Scores, in.Scores)
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
		maps.Copy(out.SparseVector, in.SparseVector)
	}
	return out
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
		maps.Copy(out.DiagnosticSignatures, in.DiagnosticSignatures)
	}
	return out
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
