package executor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	"github.com/GizClaw/flowcraft/memory/internal/projectors"
	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// IndexMessages writes canonical source messages into the message projection.
func (r *Executor) IndexMessages(ctx context.Context, req recent.WindowRequest, namespaceOverride string) ([]sourcemessage.Message, error) {
	if err := r.requireMessageIndex(); err != nil {
		return nil, err
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, errdefs.Validationf("%s: invalid message scope: %w", errPrefix, err)
	}
	if req.Scope.ConversationID == "" {
		return nil, errdefs.Validationf("%s: conversation_id is required", errPrefix)
	}
	messages, err := r.messageStore.List(ctx, req.Scope.ConversationID, sourcemessage.ListOptions{
		AfterSeq: req.AfterSeq,
		Limit:    req.Budget.MaxMessages,
	})
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, nil
	}
	writer, err := r.writerFor(compiler.CapabilityMessageIndex, namespaceOverride)
	if err != nil {
		return nil, err
	}
	if writer != nil {
		records := make([]indexed.Record, 0, len(messages))
		for _, msg := range messages {
			messageRecords, err := projectors.SourceMessageRecords(req.Scope, msg)
			if err != nil {
				return nil, err
			}
			records = append(records, messageRecords...)
		}
		if err := writer.Upsert(ctx, records); err != nil {
			return nil, err
		}
	}
	return messages, nil
}

// SearchSourceMessages searches the source message projection namespace and
// hydrates every hit from the canonical message store.
func (r *Executor) SearchSourceMessages(ctx context.Context, req retrieval.SearchRequest, namespaceOverride string) (*SourceMessageSearchResponse, error) {
	if err := r.requireMessageSearch(); err != nil {
		return nil, err
	}
	namespace, err := r.namespaceForSearch(compiler.CapabilityMessageIndex, namespaceOverride)
	if err != nil {
		return nil, err
	}
	messageTopK := req.TopK
	searchReq := req
	searchReq.TopK = sourceMessageChunkSearchTopK(req.TopK)
	resp, err := r.index.Search(ctx, namespace, searchReq)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errdefs.NotAvailablef("%s: search projection for capability %q returned nil response", errPrefix, compiler.CapabilityMessageIndex)
	}
	out := &SourceMessageSearchResponse{Took: resp.Took, Hits: make([]derive.SourceMessageSearchHit, 0, len(resp.Hits))}
	seen := map[string]struct{}{}
	for _, hit := range resp.Hits {
		conversationID, err := metadataString(hit, projectors.MetadataConversationIDKey)
		if err != nil {
			return nil, err
		}
		messageID, err := metadataString(hit, projectors.MetadataMessageIDKey)
		if err != nil {
			return nil, err
		}
		key := conversationID + "\x00" + messageID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		msg, ok, err := r.messageStore.Get(ctx, conversationID, messageID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errdefs.NotAvailablef("%s: hydrate source message hit %q: message %q/%q not found", errPrefix, hit.Doc.ID, conversationID, messageID)
		}
		out.Hits = append(out.Hits, derive.SourceMessageSearchHit{
			Retrieval: retrieval.CloneHit(hit),
			Message:   msg,
		})
		if messageTopK > 0 && len(out.Hits) >= messageTopK {
			break
		}
	}
	return out, nil
}

func sourceMessageChunkSearchTopK(topK int) int {
	if topK <= 0 {
		return topK
	}
	return topK * 4
}

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

	oldChunks, err := r.documentChunks.ListChunks(ctx, documentID, viewdocument.ListOptions{Scope: &scope})
	if err != nil {
		return nil, err
	}
	writer, err := r.writerFor(compiler.CapabilityDocumentChunks, namespaceOverride)
	if err != nil {
		return nil, err
	}
	oldRecordIDs := make([]string, 0, len(oldChunks))
	oldRecordIDSet := make(map[string]struct{}, len(oldChunks))
	oldChunkIDs := make([]viewdocument.ChunkID, 0, len(oldChunks))
	oldChunkIDSet := make(map[viewdocument.ChunkID]struct{}, len(oldChunks))
	for _, chunk := range oldChunks {
		if _, ok := oldChunkIDSet[chunk.ID]; !ok {
			oldChunkIDs = append(oldChunkIDs, chunk.ID)
			oldChunkIDSet[chunk.ID] = struct{}{}
		}
		recordID := projectors.DocumentChunkRecordID(chunk.Scope.DatasetID, chunk.DocumentID, chunk.ID)
		if _, ok := oldRecordIDSet[recordID]; !ok {
			oldRecordIDs = append(oldRecordIDs, recordID)
			oldRecordIDSet[recordID] = struct{}{}
		}
	}

	chunks, err := r.documentChunker.ChunkDocument(ctx, derive.DocumentChunkInput{
		View:     r.documentChunks.Descriptor(),
		Scope:    scope,
		Document: doc,
	})
	if err != nil {
		return nil, err
	}

	stored := make([]viewdocument.Chunk, 0, len(chunks))
	newChunkIDs := make(map[viewdocument.ChunkID]struct{}, len(chunks))
	for _, chunk := range chunks {
		written, err := r.documentChunks.PutChunk(ctx, chunk)
		if err != nil {
			return nil, err
		}
		stored = append(stored, written)
		newChunkIDs[written.ID] = struct{}{}
	}
	newRecordIDSet := make(map[string]struct{}, len(stored))
	if writer != nil {
		records := make([]indexed.Record, 0, len(stored))
		for _, chunk := range stored {
			record, err := projectors.DocumentChunk(chunk)
			if err != nil {
				return nil, err
			}
			records = append(records, record)
			newRecordIDSet[record.ID] = struct{}{}
		}
		if len(records) > 0 {
			if err := writer.Upsert(ctx, records); err != nil {
				return nil, err
			}
		}
	}
	if writer != nil {
		staleRecordIDs := make([]string, 0, len(oldRecordIDs))
		for _, id := range oldRecordIDs {
			if _, ok := newRecordIDSet[id]; ok {
				continue
			}
			staleRecordIDs = append(staleRecordIDs, id)
		}
		if len(staleRecordIDs) > 0 {
			if err := writer.Delete(ctx, staleRecordIDs); err != nil {
				return stored, fmt.Errorf("%s: document chunks rebuilt but delete stale projection records for document %q/%q failed: %w", errPrefix, datasetID, documentID, err)
			}
		}
	}
	for _, id := range oldChunkIDs {
		if _, ok := newChunkIDs[id]; ok {
			continue
		}
		if err := r.documentChunks.DeleteChunk(ctx, scope, documentID, id); err != nil {
			return stored, fmt.Errorf("%s: document chunks rebuilt but delete stale chunks for document %q/%q failed: %w", errPrefix, datasetID, documentID, err)
		}
	}
	return stored, nil
}

// SearchDocumentChunks searches the document chunk projection namespace and
// hydrates every hit from the chunk store.
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
	out := &DocumentChunkSearchResponse{Took: resp.Took, Hits: make([]derive.DocumentChunkSearchHit, 0, len(resp.Hits))}
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
		out.Hits = append(out.Hits, derive.DocumentChunkSearchHit{
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
	current, err := r.summaryDAG.ListNodes(ctx, req.Scope, recent.ListOptions{})
	if err != nil {
		return nil, err
	}
	nodes, err := r.summarizer.Summarize(ctx, derive.SummaryInput{
		View:    r.summaryDAG.Descriptor(),
		Scope:   req.Scope,
		Window:  window,
		Current: current,
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

// BuildEntityFacts derives entity-linked facts from a recent message window.
func (r *Executor) BuildEntityFacts(ctx context.Context, req recent.WindowRequest, namespaceOverride string) ([]viewentityfact.Fact, error) {
	if err := r.requireEntityFacts(); err != nil {
		return nil, err
	}
	window, err := r.recentWindow.Load(ctx, req)
	if err != nil {
		return nil, err
	}
	currentEntities, err := r.entityFacts.ListEntities(ctx, req.Scope, viewentityfact.ListOptions{})
	if err != nil {
		return nil, err
	}
	currentFacts, err := r.entityFacts.ListFacts(ctx, req.Scope, viewentityfact.ListOptions{})
	if err != nil {
		return nil, err
	}
	derived, err := r.entityFactExtractor.ExtractEntityFacts(ctx, derive.EntityFactInput{
		View:            r.entityFacts.Descriptor(),
		Scope:           req.Scope,
		Window:          window,
		CurrentEntities: currentEntities,
		CurrentFacts:    currentFacts,
	})
	if err != nil {
		return nil, err
	}

	for _, entity := range derived.Entities {
		if _, err := r.entityFacts.PutEntity(ctx, entity); err != nil {
			return nil, err
		}
	}
	stored := make([]viewentityfact.Fact, 0, len(derived.Facts))
	for _, fact := range derived.Facts {
		written, err := r.entityFacts.PutFact(ctx, fact)
		if err != nil {
			return nil, err
		}
		stored = append(stored, written)
	}
	writer, err := r.writerFor(compiler.CapabilityEntityFactIndex, namespaceOverride)
	if err != nil {
		return nil, err
	}
	if writer != nil {
		records := make([]indexed.Record, 0, len(stored))
		for _, fact := range stored {
			record, err := projectors.EntityFact(fact)
			if err != nil {
				return nil, err
			}
			records = append(records, record)
		}
		if len(records) > 0 {
			if err := writer.Upsert(ctx, records); err != nil {
				return nil, err
			}
		}
	}
	return stored, nil
}

// SearchEntityFacts searches the entity fact projection namespace and hydrates
// every hit from the entity fact store.
func (r *Executor) SearchEntityFacts(ctx context.Context, req retrieval.SearchRequest, namespaceOverride string) (*EntityFactSearchResponse, error) {
	if err := r.requireEntityFactSearch(); err != nil {
		return nil, err
	}
	namespace, err := r.namespaceForSearch(compiler.CapabilityEntityFactIndex, namespaceOverride)
	if err != nil {
		return nil, err
	}
	resp, err := r.index.Search(ctx, namespace, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errdefs.NotAvailablef("%s: search projection for capability %q returned nil response", errPrefix, compiler.CapabilityEntityFactIndex)
	}
	out := &EntityFactSearchResponse{Took: resp.Took, Hits: make([]derive.EntityFactSearchHit, 0, len(resp.Hits))}
	for _, hit := range resp.Hits {
		scope, err := metadataScope(hit)
		if err != nil {
			return nil, err
		}
		factID, err := metadataString(hit, projectors.MetadataFactIDKey)
		if err != nil {
			return nil, err
		}
		fact, ok, err := r.entityFacts.GetFact(ctx, scope, viewentityfact.FactID(factID))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errdefs.NotAvailablef("%s: hydrate entity fact hit %q: fact %q/%q/%q/%q not found", errPrefix, hit.Doc.ID, scope.RuntimeID, scope.UserID, scope.ConversationID, factID)
		}
		out.Hits = append(out.Hits, derive.EntityFactSearchHit{
			Retrieval: retrieval.CloneHit(hit),
			Fact:      fact,
		})
	}
	return out, nil
}

// SearchSummaryNodes searches the SummaryDAG projection namespace, optionally
// drilling high-level compaction hits down through ParentIDs before returning
// hydrated summary hits.
func (r *Executor) SearchSummaryNodes(ctx context.Context, req retrieval.SearchRequest, namespaceOverride string, cfg SummaryRetrievalConfig) (*SummaryNodeSearchResponse, error) {
	if err := r.requireSummaryNodeSearch(); err != nil {
		return nil, err
	}
	cfg = normalizeSummaryRetrievalConfig(cfg)
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
	out := &SummaryNodeSearchResponse{Took: resp.Took, Hits: make([]derive.SummaryNodeSearchHit, 0, len(resp.Hits))}
	for _, hit := range resp.Hits {
		hydrated, err := r.hydrateSummarySearchHit(ctx, hit)
		if err != nil {
			return nil, err
		}
		drilled, err := r.drillDownSummarySearchHit(ctx, namespace, req, cfg, hydrated)
		if err != nil {
			return nil, err
		}
		out.Hits = append(out.Hits, drilled...)
	}
	out.Hits = dedupeSummaryNodeSearchHits(out.Hits, req.TopK)
	return out, nil
}

const (
	defaultSummaryDrillDownMaxDepth  = -1
	defaultSummaryDrillDownChildTopK = 2
)

func normalizeSummaryRetrievalConfig(cfg SummaryRetrievalConfig) SummaryRetrievalConfig {
	if cfg.DrillDownChildTopK <= 0 {
		cfg.DrillDownChildTopK = defaultSummaryDrillDownChildTopK
	}
	return cfg
}

func (r *Executor) hydrateSummarySearchHit(ctx context.Context, hit retrieval.Hit) (derive.SummaryNodeSearchHit, error) {
	scope, err := metadataScope(hit)
	if err != nil {
		return derive.SummaryNodeSearchHit{}, err
	}
	nodeID, err := metadataString(hit, projectors.MetadataNodeIDKey)
	if err != nil {
		return derive.SummaryNodeSearchHit{}, err
	}
	node, ok, err := r.summaryDAG.GetNode(ctx, scope, recent.NodeID(nodeID))
	if err != nil {
		return derive.SummaryNodeSearchHit{}, err
	}
	if !ok {
		return derive.SummaryNodeSearchHit{}, errdefs.NotAvailablef("%s: hydrate summary hit %q: node %q/%q/%q/%q not found", errPrefix, hit.Doc.ID, scope.RuntimeID, scope.UserID, scope.ConversationID, nodeID)
	}
	return derive.SummaryNodeSearchHit{
		Retrieval: retrieval.CloneHit(hit),
		Node:      node,
	}, nil
}

func (r *Executor) drillDownSummarySearchHit(ctx context.Context, namespace string, req retrieval.SearchRequest, cfg SummaryRetrievalConfig, hit derive.SummaryNodeSearchHit) ([]derive.SummaryNodeSearchHit, error) {
	if cfg.DrillDownMaxDepth == 0 || strings.TrimSpace(req.QueryText) == "" || len(hit.Node.ParentIDs) == 0 {
		return []derive.SummaryNodeSearchHit{hit}, nil
	}
	children, err := r.searchSummaryChildren(ctx, namespace, req, cfg, hit)
	if err != nil {
		return nil, err
	}
	if len(children) == 0 {
		return []derive.SummaryNodeSearchHit{hit}, nil
	}
	out := make([]derive.SummaryNodeSearchHit, 0, len(children))
	nextCfg := cfg
	if nextCfg.DrillDownMaxDepth > 0 {
		nextCfg.DrillDownMaxDepth--
	}
	for _, child := range children {
		drilled, err := r.drillDownSummarySearchHit(ctx, namespace, req, nextCfg, child)
		if err != nil {
			return nil, err
		}
		out = append(out, drilled...)
	}
	return out, nil
}

func (r *Executor) searchSummaryChildren(ctx context.Context, namespace string, req retrieval.SearchRequest, cfg SummaryRetrievalConfig, parent derive.SummaryNodeSearchHit) ([]derive.SummaryNodeSearchHit, error) {
	childIDs := make([]any, 0, len(parent.Node.ParentIDs))
	for _, id := range parent.Node.ParentIDs {
		childIDs = append(childIDs, string(id))
	}
	childReq := req
	childReq.TopK = min(cfg.DrillDownChildTopK, len(childIDs))
	childReq.Filter = combineSearchFilters(req.Filter, retrieval.Filter{
		In: map[string][]any{projectors.MetadataNodeIDKey: childIDs},
	})

	resp, err := r.index.Search(ctx, namespace, childReq)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errdefs.NotAvailablef("%s: child search projection for capability %q returned nil response", errPrefix, compiler.CapabilitySummaryDAG)
	}

	children := make([]derive.SummaryNodeSearchHit, 0, len(resp.Hits))
	for _, hit := range resp.Hits {
		child, err := r.hydrateSummarySearchHit(ctx, hit)
		if err != nil {
			return nil, err
		}
		child.Retrieval = summaryChildPathHit(parent.Retrieval, child.Retrieval)
		children = append(children, child)
	}
	if len(children) > 0 {
		return children, nil
	}
	return r.fallbackSummaryChildren(ctx, req.QueryText, cfg, parent)
}

func (r *Executor) fallbackSummaryChildren(ctx context.Context, query string, cfg SummaryRetrievalConfig, parent derive.SummaryNodeSearchHit) ([]derive.SummaryNodeSearchHit, error) {
	children := make([]derive.SummaryNodeSearchHit, 0, len(parent.Node.ParentIDs))
	for _, id := range parent.Node.ParentIDs {
		node, ok, err := r.summaryDAG.GetNode(ctx, parent.Node.Scope, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errdefs.NotAvailablef("%s: hydrate summary child %q of %q not found", errPrefix, id, parent.Node.ID)
		}
		score := lexicalSummaryScore(query, node.Summary)
		children = append(children, derive.SummaryNodeSearchHit{
			Retrieval: fallbackSummaryChildHit(parent.Retrieval, node, score),
			Node:      node,
		})
	}
	sort.SliceStable(children, func(i, j int) bool {
		if children[i].Retrieval.Score != children[j].Retrieval.Score {
			return children[i].Retrieval.Score > children[j].Retrieval.Score
		}
		return children[i].Node.ID < children[j].Node.ID
	})
	if len(children) > cfg.DrillDownChildTopK {
		children = children[:cfg.DrillDownChildTopK]
	}
	return children, nil
}

func combineSearchFilters(left, right retrieval.Filter) retrieval.Filter {
	if filterEmpty(left) {
		return right
	}
	if filterEmpty(right) {
		return left
	}
	return retrieval.Filter{And: []retrieval.Filter{left, right}}
}

func filterEmpty(f retrieval.Filter) bool {
	return len(f.And) == 0 &&
		len(f.Or) == 0 &&
		f.Not == nil &&
		len(f.Eq) == 0 &&
		len(f.Neq) == 0 &&
		len(f.In) == 0 &&
		len(f.NotIn) == 0 &&
		len(f.Range) == 0 &&
		len(f.Exists) == 0 &&
		len(f.Missing) == 0 &&
		len(f.Match) == 0 &&
		len(f.Contains) == 0 &&
		len(f.IContains) == 0 &&
		len(f.ContainsAny) == 0 &&
		len(f.ContainsAll) == 0
}

func summaryChildPathHit(parent, child retrieval.Hit) retrieval.Hit {
	out := retrieval.CloneHit(child)
	out.Score = 0.7*child.Score + 0.3*parent.Score
	if out.Doc.Metadata == nil {
		out.Doc.Metadata = map[string]any{}
	}
	if parentID, ok := parent.Doc.Metadata[projectors.MetadataNodeIDKey]; ok {
		out.Doc.Metadata["summary_drilldown_parent_node_id"] = parentID
	}
	out.Doc.Metadata["summary_drilldown_parent_score"] = parent.Score
	return out
}

func fallbackSummaryChildHit(parent retrieval.Hit, node recent.SummaryNode, score float64) retrieval.Hit {
	out := retrieval.CloneHit(parent)
	out.Score = 0.7*score + 0.3*parent.Score
	out.Distance = 0
	out.Doc.ID = string(node.ID)
	out.Doc.Content = node.Summary
	out.Doc.Metadata = cloneAnyMap(parent.Doc.Metadata)
	if out.Doc.Metadata == nil {
		out.Doc.Metadata = map[string]any{}
	}
	out.Doc.Metadata[projectors.MetadataNodeIDKey] = string(node.ID)
	if parentID, ok := parent.Doc.Metadata[projectors.MetadataNodeIDKey]; ok {
		out.Doc.Metadata["summary_drilldown_parent_node_id"] = parentID
	}
	out.Doc.Metadata["summary_drilldown_parent_score"] = parent.Score
	out.Scores = cloneFloat64MapWithCapacity(parent.Scores, 1)
	out.Scores["summary_drilldown_lexical"] = score
	return out
}

func cloneFloat64MapWithCapacity(in map[string]float64, extra int) map[string]float64 {
	out := make(map[string]float64, len(in)+extra)
	for key, value := range in {
		out[key] = value
	}
	return out
}

func lexicalSummaryScore(query, text string) float64 {
	queryTokens := summaryScoreTokens(query)
	if len(queryTokens) == 0 {
		return 0
	}
	textTokens := summaryScoreTokenSet(text)
	matches := 0
	for token := range queryTokens {
		if textTokens[token] {
			matches++
		}
	}
	return float64(matches) / float64(len(queryTokens))
}

func summaryScoreTokens(text string) map[string]bool {
	tokens := summaryScoreTokenSet(text)
	for token := range tokens {
		if len(token) < 3 {
			delete(tokens, token)
		}
	}
	return tokens
}

func summaryScoreTokenSet(text string) map[string]bool {
	tokens := map[string]bool{}
	for _, field := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		field = strings.TrimSpace(field)
		if field != "" {
			tokens[field] = true
		}
	}
	return tokens
}

func dedupeSummaryNodeSearchHits(hits []derive.SummaryNodeSearchHit, limit int) []derive.SummaryNodeSearchHit {
	out := make([]derive.SummaryNodeSearchHit, 0, len(hits))
	seen := map[recent.NodeID]bool{}
	for _, hit := range hits {
		if seen[hit.Node.ID] {
			continue
		}
		seen[hit.Node.ID] = true
		out = append(out, hit)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
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

func (r *Executor) requireMessageIndex() error {
	if _, ok := r.enabled[compiler.CapabilityMessageIndex]; !ok {
		return errdefs.NotAvailablef("%s: capability %q is not configured", errPrefix, compiler.CapabilityMessageIndex)
	}
	if r.messageStore == nil {
		return errdefs.NotAvailablef("%s: message store is not configured", errPrefix)
	}
	return r.requireProjection(compiler.CapabilityMessageIndex)
}

func (r *Executor) requireMessageSearch() error {
	if r.messageStore == nil {
		return errdefs.NotAvailablef("%s: message store is not configured", errPrefix)
	}
	return r.requireProjection(compiler.CapabilityMessageIndex)
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

func (r *Executor) requireEntityFacts() error {
	if _, ok := r.enabled[compiler.CapabilityEntityFactIndex]; !ok {
		return errdefs.NotAvailablef("%s: capability %q is not configured", errPrefix, compiler.CapabilityEntityFactIndex)
	}
	if r.recentWindow == nil {
		return errdefs.NotAvailablef("%s: recent window is not configured", errPrefix)
	}
	if r.entityFacts == nil {
		return errdefs.NotAvailablef("%s: entity facts view is not configured", errPrefix)
	}
	if r.entityFactExtractor == nil {
		return errdefs.NotAvailablef("%s: EntityFactExtractor is not configured", errPrefix)
	}
	return nil
}

func (r *Executor) requireEntityFactSearch() error {
	if r.entityFacts == nil {
		return errdefs.NotAvailablef("%s: entity facts view is not configured", errPrefix)
	}
	return r.requireProjection(compiler.CapabilityEntityFactIndex)
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
