// Package hydrate resolves fused candidates into canonical context items.
package hydrate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/memory/internal/projection"
	messagesource "github.com/GizClaw/flowcraft/memory/sources/message"
	documentview "github.com/GizClaw/flowcraft/memory/views/document"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/memory/views/summary"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// Hydrator is the narrow read-side hydration capability.
type Hydrator interface {
	Hydrate(context.Context, sdkmemory.Scope, component.Candidate) (sdkmemory.ContextItem, error)
}

// Progressive resolves one hierarchy parent from an already hydrated item.
type Progressive interface {
	Parent(context.Context, sdkmemory.Scope, sdkmemory.ContextItem) (sdkmemory.ContextItem, bool, error)
}

// ProvenanceExpander resolves structured hints against current canonical
// sources. Stale or invalid references fail closed.
type ProvenanceExpander interface {
	Expand(context.Context, sdkmemory.Scope, sdkmemory.ExpandHint) ([]sdkmemory.ContextItem, error)
}

// Composite resolves message source, fact view, and document chunk addresses.
type Composite struct {
	Messages  messagesource.Store
	Facts     factview.Store
	Chunks    documentview.Store
	Summaries summaryview.Store
}

var _ Hydrator = (*Composite)(nil)

func (hydrator *Composite) Hydrate(ctx context.Context, scope sdkmemory.Scope, candidate component.Candidate) (sdkmemory.ContextItem, error) {
	if hydrator == nil {
		return sdkmemory.ContextItem{}, errors.New("hydrate: composite is required")
	}
	if ctx == nil {
		return sdkmemory.ContextItem{}, errors.New("hydrate: context is required")
	}
	if err := ctx.Err(); err != nil {
		return sdkmemory.ContextItem{}, err
	}
	if err := scope.Validate(); err != nil {
		return sdkmemory.ContextItem{}, err
	}
	if err := candidate.Validate(); err != nil {
		return sdkmemory.ContextItem{}, fmt.Errorf("hydrate: candidate: %w", err)
	}
	address, err := resolveAddress(candidate)
	if err != nil {
		return sdkmemory.ContextItem{}, err
	}
	switch address.Kind {
	case sdkmemory.ContextRawMessage:
		if hydrator.Messages == nil {
			return sdkmemory.ContextItem{}, errors.New("hydrate: message store is not configured")
		}
		record, ok, err := hydrator.Messages.Get(ctx, scope, address.ConversationID, address.ItemID)
		if err != nil {
			return sdkmemory.ContextItem{}, fmt.Errorf("hydrate: get message: %w", err)
		}
		if !ok {
			return sdkmemory.ContextItem{}, fmt.Errorf("hydrate: message %q not found", address.ItemID)
		}
		source := sdkmemory.SourceRef{
			Kind: sdkmemory.SourceMessage, ID: record.ConversationID + "/" + record.ID,
			Revision: strconv.FormatUint(record.Seq, 10),
		}
		if candidate.Source != source {
			return sdkmemory.ContextItem{}, errors.New("hydrate: message candidate provenance is stale or invalid")
		}
		return sdkmemory.ContextItem{
			ID: record.ID, Kind: sdkmemory.ContextRawMessage, Content: record.Message.Content.Clone(),
			Address: contextAddress(address), Score: candidate.Score, Sources: []sdkmemory.SourceRef{source},
			Metadata:    record.Metadata.Clone(),
			MessageRole: record.Message.Role, Sequence: record.Seq, Timestamp: record.CreatedAt,
		}, nil
	case sdkmemory.ContextFact:
		if hydrator.Facts == nil {
			return sdkmemory.ContextItem{}, errors.New("hydrate: fact store is not configured")
		}
		fact, ok, err := hydrator.Facts.Get(ctx, scope, address.ConversationID, address.ItemID)
		if err != nil {
			return sdkmemory.ContextItem{}, fmt.Errorf("hydrate: get fact: %w", err)
		}
		if !ok {
			return sdkmemory.ContextItem{}, fmt.Errorf("hydrate: fact %q not found", address.ItemID)
		}
		if !containsSource(fact.Provenance, candidate.Source) {
			return sdkmemory.ContextItem{}, errors.New("hydrate: fact candidate provenance is stale or invalid")
		}
		return sdkmemory.ContextItem{
			ID: fact.ID, Kind: sdkmemory.ContextFact, Content: fact.Content.Clone(),
			Address: contextAddress(address), Score: candidate.Score, Sources: append([]sdkmemory.SourceRef(nil), fact.Provenance...),
			Metadata: fact.Metadata.Clone(),
		}, nil
	case sdkmemory.ContextSummary:
		if hydrator.Summaries == nil {
			return sdkmemory.ContextItem{}, errors.New("hydrate: summary store is not configured")
		}
		record, ok, err := hydrator.Summaries.Get(ctx, scope, address.ConversationID, address.ItemID)
		if err != nil {
			return sdkmemory.ContextItem{}, fmt.Errorf("hydrate: get summary: %w", err)
		}
		if !ok {
			return sdkmemory.ContextItem{}, fmt.Errorf("hydrate: summary %q not found", address.ItemID)
		}
		if !containsSource(record.SourceRefs, candidate.Source) {
			return sdkmemory.ContextItem{}, errors.New("hydrate: summary candidate provenance is stale or invalid")
		}
		return sdkmemory.ContextItem{
			ID: record.ID, Kind: sdkmemory.ContextSummary, Content: record.Content.Clone(),
			Address: contextAddress(address), Score: candidate.Score, Sources: append([]sdkmemory.SourceRef(nil), record.SourceRefs...),
			Level: int(record.Level),
			Hint: &sdkmemory.ExpandHint{
				Topics:     append([]string(nil), record.Topics...),
				SourceRefs: append([]sdkmemory.SourceRef(nil), record.SourceRefs...),
				Range: sdkmemory.ContextRange{
					StartSequence: record.CoverageRange.StartSeq, EndSequence: record.CoverageRange.EndSeq,
					StartTime: record.CoverageRange.StartTime, EndTime: record.CoverageRange.EndTime,
				},
				PreferredLevel: max(0, int(record.Level)-1),
			},
		}, nil
	case sdkmemory.ContextDocumentResource, sdkmemory.ContextDocumentSection, sdkmemory.ContextDocumentChunk, sdkmemory.ContextDocumentSummary:
		if hydrator.Chunks == nil {
			return sdkmemory.ContextItem{}, errors.New("hydrate: document chunk store is not configured")
		}
		chunk, ok, err := hydrator.Chunks.Get(ctx, scope, address.DatasetID, address.DocumentID, address.ItemID)
		if err != nil {
			return sdkmemory.ContextItem{}, fmt.Errorf("hydrate: get document chunk: %w", err)
		}
		if !ok {
			return sdkmemory.ContextItem{}, fmt.Errorf("hydrate: document chunk %q not found", address.ItemID)
		}
		if documentContextKind(chunk.Kind) != address.Kind || !containsSource(chunk.Provenance, candidate.Source) {
			return sdkmemory.ContextItem{}, errors.New("hydrate: document candidate kind or provenance is stale or invalid")
		}
		return sdkmemory.ContextItem{
			ID: chunk.ID, Kind: documentContextKind(chunk.Kind), Content: chunk.Content.Clone(),
			Address: contextAddress(address), Score: candidate.Score, Sources: append([]sdkmemory.SourceRef(nil), chunk.Provenance...),
			Metadata: chunk.Metadata.Clone(),
			ParentID: chunk.ParentID, Level: chunk.Level, Ordinal: chunk.Ordinal, Title: chunk.Title,
		}, nil
	default:
		return sdkmemory.ContextItem{}, fmt.Errorf("hydrate: unsupported address kind %q", address.Kind)
	}
}

func (hydrator *Composite) Expand(ctx context.Context, scope sdkmemory.Scope, hint sdkmemory.ExpandHint) ([]sdkmemory.ContextItem, error) {
	if hydrator == nil || hydrator.Messages == nil {
		return nil, errors.New("hydrate: canonical message store is not configured")
	}
	if err := hint.Validate(); err != nil {
		return nil, fmt.Errorf("hydrate: expand hint: %w", err)
	}
	result := make([]sdkmemory.ContextItem, 0, len(hint.SourceRefs))
	for _, source := range hint.SourceRefs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if source.Kind != sdkmemory.SourceMessage {
			return nil, fmt.Errorf("hydrate: unsupported expansion source kind %q", source.Kind)
		}
		conversationID, itemID, ok := strings.Cut(source.ID, "/")
		if !ok || conversationID == "" || itemID == "" {
			return nil, errors.New("hydrate: invalid message source address")
		}
		record, found, err := hydrator.Messages.Get(ctx, scope, conversationID, itemID)
		if err != nil {
			return nil, err
		}
		if !found || source.Revision != strconv.FormatUint(record.Seq, 10) {
			return nil, errors.New("hydrate: expansion source is stale or missing")
		}
		result = append(result, sdkmemory.ContextItem{
			ID: record.ID, Kind: sdkmemory.ContextRawMessage, Content: record.Message.Content.Clone(),
			Address: sdkmemory.ContextAddress{Kind: sdkmemory.ContextRawMessage, ConversationID: conversationID, ItemID: record.ID},
			Score:   1, Sources: []sdkmemory.SourceRef{source}, Metadata: record.Metadata.Clone(),
			MessageRole: record.Message.Role, Sequence: record.Seq, Timestamp: record.CreatedAt,
		})
	}
	return result, nil
}

func containsSource(values []sdkmemory.SourceRef, target sdkmemory.SourceRef) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (hydrator *Composite) Parent(ctx context.Context, scope sdkmemory.Scope, item sdkmemory.ContextItem) (sdkmemory.ContextItem, bool, error) {
	if item.ParentID == "" || hydrator == nil || hydrator.Chunks == nil {
		return sdkmemory.ContextItem{}, false, nil
	}
	datasetID, documentID := item.Metadata["dataset_id"], item.Metadata["document_id"]
	if datasetID == "" || documentID == "" {
		return sdkmemory.ContextItem{}, false, errors.New("hydrate: hierarchy item has no document address")
	}
	parent, ok, err := hydrator.Chunks.Get(ctx, scope, datasetID, documentID, item.ParentID)
	if err != nil || !ok {
		return sdkmemory.ContextItem{}, ok, err
	}
	return sdkmemory.ContextItem{
		ID: parent.ID, Kind: documentContextKind(parent.Kind), Content: parent.Content.Clone(),
		Address: sdkmemory.ContextAddress{
			Kind: documentContextKind(parent.Kind), DatasetID: datasetID, DocumentID: documentID, ItemID: parent.ID,
		},
		Score: item.Score, Sources: append([]sdkmemory.SourceRef(nil), parent.Provenance...),
		Metadata: parent.Metadata.Clone(), ParentID: parent.ParentID, Level: parent.Level,
		Ordinal: parent.Ordinal, Title: parent.Title, SourceClass: item.SourceClass,
	}, true, nil
}

func contextAddress(address component.CandidateAddress) sdkmemory.ContextAddress {
	return sdkmemory.ContextAddress{
		Kind: address.Kind, ConversationID: address.ConversationID, DatasetID: address.DatasetID,
		DocumentID: address.DocumentID, ItemID: address.ItemID,
	}
}

func documentContextKind(kind documentview.RecordKind) sdkmemory.ContextItemKind {
	switch kind {
	case documentview.KindResource:
		return sdkmemory.ContextDocumentResource
	case documentview.KindSection:
		return sdkmemory.ContextDocumentSection
	case documentview.KindSummary:
		return sdkmemory.ContextDocumentSummary
	default:
		return sdkmemory.ContextDocumentChunk
	}
}

func resolveAddress(candidate component.Candidate) (component.CandidateAddress, error) {
	if !candidate.Address.IsZero() {
		if err := candidate.Address.Validate(); err != nil {
			return component.CandidateAddress{}, fmt.Errorf("hydrate: address: %w", err)
		}
		return candidate.Address, nil
	}
	if candidate.Source.Locator == "" {
		return component.CandidateAddress{}, errors.New("hydrate: candidate has no explicit address")
	}
	var locator struct {
		SchemaVersion int                        `json:"schema_version"`
		Address       component.CandidateAddress `json:"address"`
	}
	if err := projectionstore.Decode([]byte(candidate.Source.Locator), &locator); err != nil {
		return component.CandidateAddress{}, fmt.Errorf("hydrate: decode source locator address: %w", err)
	}
	if locator.SchemaVersion != projectionstore.SchemaVersion {
		return component.CandidateAddress{}, fmt.Errorf("hydrate: unsupported locator schema_version %d", locator.SchemaVersion)
	}
	if err := locator.Address.Validate(); err != nil {
		return component.CandidateAddress{}, fmt.Errorf("hydrate: source locator address: %w", err)
	}
	return locator.Address, nil
}
