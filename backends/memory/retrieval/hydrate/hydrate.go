// Package hydrate resolves fused candidates into canonical context items.
package hydrate

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
	messagesource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	documentview "github.com/GizClaw/flowcraft/backends/memory/views/document"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// Hydrator is the narrow read-side hydration capability.
type Hydrator interface {
	Hydrate(context.Context, corememory.Scope, component.Candidate) (corememory.ContextItem, error)
}

// Progressive resolves one hierarchy parent from an already hydrated item.
type Progressive interface {
	Parent(context.Context, corememory.Scope, corememory.ContextItem) (corememory.ContextItem, bool, error)
}

// Composite resolves message source, fact view, and document chunk addresses.
type Composite struct {
	Messages  *messagesource.MessageStore
	Facts     *factview.FactStore
	Chunks    *documentview.DocumentViewStore
	Summaries *summaryview.SummaryStore
}

var _ Hydrator = (*Composite)(nil)

func (hydrator *Composite) Hydrate(ctx context.Context, scope corememory.Scope, candidate component.Candidate) (corememory.ContextItem, error) {
	if hydrator == nil {
		return corememory.ContextItem{}, errors.New("hydrate: composite is required")
	}
	if ctx == nil {
		return corememory.ContextItem{}, errors.New("hydrate: context is required")
	}
	if err := ctx.Err(); err != nil {
		return corememory.ContextItem{}, err
	}
	if err := scope.Validate(); err != nil {
		return corememory.ContextItem{}, err
	}
	if err := candidate.Validate(); err != nil {
		return corememory.ContextItem{}, fmt.Errorf("hydrate: candidate: %w", err)
	}
	address, err := resolveAddress(candidate)
	if err != nil {
		return corememory.ContextItem{}, err
	}
	switch address.Kind {
	case corememory.ContextRawMessage:
		if hydrator.Messages == nil {
			return corememory.ContextItem{}, errors.New("hydrate: message store is not configured")
		}
		record, ok, err := hydrator.Messages.Get(ctx, scope, address.ConversationID, address.ItemID)
		if err != nil {
			return corememory.ContextItem{}, fmt.Errorf("hydrate: get message: %w", err)
		}
		if !ok {
			return corememory.ContextItem{}, fmt.Errorf("hydrate: message %q not found", address.ItemID)
		}
		source := corememory.SourceRef{
			Kind: corememory.SourceMessage, ID: record.ConversationID + "/" + record.ID,
			Revision: strconv.FormatUint(record.Seq, 10),
		}
		if candidate.Source != source {
			return corememory.ContextItem{}, errors.New("hydrate: message candidate provenance is stale or invalid")
		}
		return corememory.ContextItem{
			ID: record.ID, Kind: corememory.ContextRawMessage, Content: record.Message.Content.Clone(),
			Address: contextAddress(address), Score: candidate.Score, Sources: []corememory.SourceRef{source},
			Metadata:    record.Metadata.Clone(),
			MessageRole: record.Message.Role, Sequence: record.Seq, Timestamp: record.CreatedAt,
		}, nil
	case corememory.ContextFact:
		if hydrator.Facts == nil {
			return corememory.ContextItem{}, errors.New("hydrate: fact store is not configured")
		}
		fact, ok, err := hydrator.Facts.Get(ctx, scope, address.ConversationID, address.ItemID)
		if err != nil {
			return corememory.ContextItem{}, fmt.Errorf("hydrate: get fact: %w", err)
		}
		if !ok {
			return corememory.ContextItem{}, fmt.Errorf("hydrate: fact %q not found", address.ItemID)
		}
		if !containsSource(fact.Provenance, candidate.Source) {
			return corememory.ContextItem{}, errors.New("hydrate: fact candidate provenance is stale or invalid")
		}
		return corememory.ContextItem{
			ID: fact.ID, Kind: corememory.ContextFact, Content: fact.Content.Clone(),
			Address: contextAddress(address), Score: candidate.Score, Sources: append([]corememory.SourceRef(nil), fact.Provenance...),
			Metadata: fact.Metadata.Clone(),
		}, nil
	case corememory.ContextSummary:
		if hydrator.Summaries == nil {
			return corememory.ContextItem{}, errors.New("hydrate: summary store is not configured")
		}
		record, ok, err := hydrator.Summaries.Get(ctx, scope, address.ConversationID, address.ItemID)
		if err != nil {
			return corememory.ContextItem{}, fmt.Errorf("hydrate: get summary: %w", err)
		}
		if !ok {
			return corememory.ContextItem{}, fmt.Errorf("hydrate: summary %q not found", address.ItemID)
		}
		if !containsSource(record.SourceRefs, candidate.Source) {
			return corememory.ContextItem{}, errors.New("hydrate: summary candidate provenance is stale or invalid")
		}
		return corememory.ContextItem{
			ID: record.ID, Kind: corememory.ContextSummary, Content: record.Content.Clone(),
			Address: contextAddress(address), Score: candidate.Score, Sources: append([]corememory.SourceRef(nil), record.SourceRefs...),
			Level: int(record.Level),
		}, nil
	case corememory.ContextDocumentResource, corememory.ContextDocumentSection, corememory.ContextDocumentChunk, corememory.ContextDocumentSummary:
		if hydrator.Chunks == nil {
			return corememory.ContextItem{}, errors.New("hydrate: document chunk store is not configured")
		}
		chunk, ok, err := hydrator.Chunks.Get(ctx, scope, address.DatasetID, address.DocumentID, address.ItemID)
		if err != nil {
			return corememory.ContextItem{}, fmt.Errorf("hydrate: get document chunk: %w", err)
		}
		if !ok {
			return corememory.ContextItem{}, fmt.Errorf("hydrate: document chunk %q not found", address.ItemID)
		}
		if documentContextKind(chunk.Kind) != address.Kind || !containsSource(chunk.Provenance, candidate.Source) {
			return corememory.ContextItem{}, errors.New("hydrate: document candidate kind or provenance is stale or invalid")
		}
		return corememory.ContextItem{
			ID: chunk.ID, Kind: documentContextKind(chunk.Kind), Content: chunk.Content.Clone(),
			Address: contextAddress(address), Score: candidate.Score, Sources: append([]corememory.SourceRef(nil), chunk.Provenance...),
			Metadata: chunk.Metadata.Clone(),
			ParentID: chunk.ParentID, Level: chunk.Level, Ordinal: chunk.Ordinal, Title: chunk.Title,
		}, nil
	default:
		return corememory.ContextItem{}, fmt.Errorf("hydrate: unsupported address kind %q", address.Kind)
	}
}

func containsSource(values []corememory.SourceRef, target corememory.SourceRef) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (hydrator *Composite) Parent(ctx context.Context, scope corememory.Scope, item corememory.ContextItem) (corememory.ContextItem, bool, error) {
	if item.ParentID == "" || hydrator == nil || hydrator.Chunks == nil {
		return corememory.ContextItem{}, false, nil
	}
	datasetID, documentID := item.Metadata["dataset_id"], item.Metadata["document_id"]
	if datasetID == "" || documentID == "" {
		return corememory.ContextItem{}, false, errors.New("hydrate: hierarchy item has no document address")
	}
	parent, ok, err := hydrator.Chunks.Get(ctx, scope, datasetID, documentID, item.ParentID)
	if err != nil || !ok {
		return corememory.ContextItem{}, ok, err
	}
	return corememory.ContextItem{
		ID: parent.ID, Kind: documentContextKind(parent.Kind), Content: parent.Content.Clone(),
		Address: corememory.ContextAddress{
			Kind: documentContextKind(parent.Kind), DatasetID: datasetID, DocumentID: documentID, ItemID: parent.ID,
		},
		Score: item.Score, Sources: append([]corememory.SourceRef(nil), parent.Provenance...),
		Metadata: parent.Metadata.Clone(), ParentID: parent.ParentID, Level: parent.Level,
		Ordinal: parent.Ordinal, Title: parent.Title, SourceClass: item.SourceClass,
	}, true, nil
}

func contextAddress(address component.CandidateAddress) corememory.ContextAddress {
	return corememory.ContextAddress{
		Kind: address.Kind, ConversationID: address.ConversationID, DatasetID: address.DatasetID,
		DocumentID: address.DocumentID, ItemID: address.ItemID,
	}
}

func documentContextKind(kind documentview.RecordKind) corememory.ContextItemKind {
	switch kind {
	case documentview.KindResource:
		return corememory.ContextDocumentResource
	case documentview.KindSection:
		return corememory.ContextDocumentSection
	case documentview.KindSummary:
		return corememory.ContextDocumentSummary
	default:
		return corememory.ContextDocumentChunk
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
