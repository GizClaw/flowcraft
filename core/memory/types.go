package memory

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"math"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
)

// Metadata is opaque, serializable annotation data. Capability implementations
// must not use it to weaken Scope's tenant boundary.
type Metadata map[string]string

func (src Metadata) Clone() Metadata {
	if src == nil {
		return nil
	}
	dst := make(Metadata, len(src))
	maps.Copy(dst, src)
	return dst
}

// Budget bounds context packing. Zero fields select implementation defaults.
type Budget struct {
	MaxTokens int
	MaxItems  int
	MaxChars  int
}

func (b Budget) Validate() error {
	if b.MaxTokens < 0 {
		return NewError(KindInvalidRequest, "context", errors.New("memory: max_tokens must not be negative"))
	}
	if b.MaxItems < 0 {
		return NewError(KindInvalidRequest, "context", errors.New("memory: max_items must not be negative"))
	}
	if b.MaxChars < 0 {
		return NewError(KindInvalidRequest, "context", errors.New("memory: max_chars must not be negative"))
	}
	return nil
}

// SourceKind names a canonical source type.
type SourceKind string

const (
	SourceMessage  SourceKind = "message"
	SourceDocument SourceKind = "document"
	SourceExternal SourceKind = "external"
)

func (kind SourceKind) Validate() error {
	switch kind {
	case SourceMessage, SourceDocument, SourceExternal:
		return nil
	default:
		return fmt.Errorf("memory: unknown source kind %q", kind)
	}
}

// SourceRef preserves provenance without exposing a storage backend.
type SourceRef struct {
	Kind     SourceKind
	ID       string
	Revision string
	Locator  string
}

func (r SourceRef) Validate() error {
	if err := r.Kind.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.ID) == "" {
		return errors.New("memory: source id is required")
	}
	return nil
}

// ContextItemKind identifies a hydrated memory view.
type ContextItemKind string

const (
	ContextRawMessage       ContextItemKind = "raw_message"
	ContextFact             ContextItemKind = "fact"
	ContextDocumentResource ContextItemKind = "document_resource"
	ContextDocumentSection  ContextItemKind = "document_section"
	ContextDocumentChunk    ContextItemKind = "document_chunk"
	ContextDocumentSummary  ContextItemKind = "document_summary"
	ContextSummary          ContextItemKind = "summary"
)

func (kind ContextItemKind) Validate() error {
	switch kind {
	case ContextRawMessage,
		ContextFact,
		ContextDocumentResource,
		ContextDocumentSection,
		ContextDocumentChunk,
		ContextDocumentSummary,
		ContextSummary:
		return nil
	default:
		return fmt.Errorf("memory: unknown context item kind %q", kind)
	}
}

// ContextSourceClass identifies the fixed read-path source without relying on
// metadata markers.
type ContextSourceClass string

const (
	ContextSourceRecent   ContextSourceClass = "recent"
	ContextSourceLongTerm ContextSourceClass = "long_term"
	ContextSourceSummary  ContextSourceClass = "summary"
)

func (class ContextSourceClass) Validate() error {
	switch class {
	case ContextSourceRecent, ContextSourceLongTerm, ContextSourceSummary:
		return nil
	default:
		return fmt.Errorf("memory: unknown context source class %q", class)
	}
}

// ContextAddress qualifies a view-local item ID within one hard scope.
// Conversation and document coordinates are explicit so equal local IDs in
// different conversations or datasets remain distinct.
type ContextAddress struct {
	Kind           ContextItemKind
	ConversationID string
	DatasetID      string
	DocumentID     string
	ItemID         string
}

func (address ContextAddress) IsZero() bool {
	return address.Kind == "" && address.ConversationID == "" && address.DatasetID == "" &&
		address.DocumentID == "" && address.ItemID == ""
}

func (address ContextAddress) Validate() error {
	if address.IsZero() {
		return nil
	}
	if err := address.Kind.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(address.ItemID) == "" {
		return errors.New("memory: context address item_id is required")
	}
	switch address.Kind {
	case ContextRawMessage, ContextFact, ContextSummary:
		if strings.TrimSpace(address.ConversationID) == "" {
			return errors.New("memory: context address conversation_id is required")
		}
		if address.DatasetID != "" || address.DocumentID != "" {
			return errors.New("memory: conversation context address contains document coordinates")
		}
	case ContextDocumentResource, ContextDocumentSection, ContextDocumentChunk, ContextDocumentSummary:
		if strings.TrimSpace(address.DatasetID) == "" || strings.TrimSpace(address.DocumentID) == "" {
			return errors.New("memory: document context address dataset_id and document_id are required")
		}
		if address.ConversationID != "" {
			return errors.New("memory: document context address contains conversation_id")
		}
	}
	for _, value := range []string{address.ConversationID, address.DatasetID, address.DocumentID, address.ItemID} {
		if strings.ContainsRune(value, '\x00') {
			return errors.New("memory: context address fields must not contain NUL")
		}
	}
	return nil
}

// Key is a collision-free identity within a single hard scope.
func (address ContextAddress) Key() string {
	return strings.Join([]string{
		string(address.Kind), address.ConversationID, address.DatasetID, address.DocumentID, address.ItemID,
	}, "\x00")
}

// Identity returns an opaque identity qualified by the complete hard scope.
func (address ContextAddress) Identity(scope Scope) string {
	sum := sha256.Sum256([]byte("flowcraft.memory.context-address\x00v1\x00" +
		scope.HardPartitionKey() + "\x00" + address.Key()))
	return fmt.Sprintf("context-item-%x", sum[:])
}

// ContextItem is a hydrated, scored memory item. Score has one portable
// meaning across implementations: larger is more relevant and values are in
// [0,1]. Scores from individual retrieval lanes are implementation details.
type ContextItem struct {
	ID          string
	Address     ContextAddress
	Kind        ContextItemKind
	Content     message.Content
	Score       float64
	Sources     []SourceRef
	Metadata    Metadata
	TokenCount  int
	SourceClass ContextSourceClass
	MessageRole message.Role
	Sequence    uint64
	Timestamp   time.Time
	ParentID    string
	Level       int
	Ordinal     uint64
	Title       string
	Hint        *ExpandHint
}

// Clone returns an independent copy of the item's mutable data.
func (i ContextItem) Clone() ContextItem {
	i.Content = i.Content.Clone()
	i.Sources = append([]SourceRef(nil), i.Sources...)
	i.Metadata = i.Metadata.Clone()
	if i.Hint != nil {
		hint := i.Hint.Clone()
		i.Hint = &hint
	}
	return i
}

// Identity returns the hard-scope-qualified identity used by lifecycle
// reinforcement and visibility overlays. ID remains the view-local ID.
func (i ContextItem) Identity(scope Scope) string {
	address := i.Address
	if address.IsZero() {
		address = ContextAddress{Kind: i.Kind, ItemID: i.ID}
	}
	return address.Identity(scope)
}

// ContextRange identifies the canonical sequence/time interval represented by
// a compressed item.
type ContextRange struct {
	StartSequence uint64
	EndSequence   uint64
	StartTime     time.Time
	EndTime       time.Time
}

func (value ContextRange) Validate() error {
	if value.EndSequence != 0 && value.StartSequence > value.EndSequence {
		return errors.New("memory: context range start_sequence exceeds end_sequence")
	}
	if !value.StartTime.IsZero() && !value.EndTime.IsZero() && value.StartTime.After(value.EndTime) {
		return errors.New("memory: context range start_time exceeds end_time")
	}
	return nil
}

// ExpandHint is structured navigation metadata. Consumers must resolve its
// provenance through hydration APIs; it is never prompt text.
type ExpandHint struct {
	Topics         []string
	SourceRefs     []SourceRef
	Range          ContextRange
	PreferredLevel int
}

func (hint ExpandHint) Validate() error {
	if hint.PreferredLevel < 0 || hint.PreferredLevel > 3 {
		return errors.New("memory: expand hint preferred_level must be in [0,3]")
	}
	if err := hint.Range.Validate(); err != nil {
		return err
	}
	for index, source := range hint.SourceRefs {
		if err := source.Validate(); err != nil {
			return fmt.Errorf("memory: expand hint source %d: %w", index, err)
		}
	}
	for index, topic := range hint.Topics {
		if strings.TrimSpace(topic) == "" {
			return fmt.Errorf("memory: expand hint topic %d is empty", index)
		}
	}
	if len(hint.Topics) == 0 && len(hint.SourceRefs) == 0 &&
		hint.Range == (ContextRange{}) {
		return errors.New("memory: expand hint is empty")
	}
	return nil
}

func (hint ExpandHint) Clone() ExpandHint {
	hint.Topics = append([]string(nil), hint.Topics...)
	hint.SourceRefs = append([]SourceRef(nil), hint.SourceRefs...)
	return hint
}

func (i ContextItem) Validate() error {
	if strings.TrimSpace(i.ID) == "" {
		return errors.New("memory: context item id is required")
	}
	if err := i.Kind.Validate(); err != nil {
		return err
	}
	if err := i.Address.Validate(); err != nil {
		return err
	}
	if !i.Address.IsZero() && (i.Address.Kind != i.Kind || i.Address.ItemID != i.ID) {
		return errors.New("memory: context item address does not match kind/id")
	}
	if err := i.SourceClass.Validate(); err != nil {
		return err
	}
	if math.IsNaN(i.Score) || math.IsInf(i.Score, 0) || i.Score < 0 || i.Score > 1 {
		return fmt.Errorf("memory: context item score must be in [0,1], got %v", i.Score)
	}
	if i.TokenCount < 0 {
		return errors.New("memory: context item token_count must not be negative")
	}
	if i.Level < 0 {
		return errors.New("memory: context item level must not be negative")
	}
	if err := i.Content.Validate(); err != nil {
		return fmt.Errorf("memory: context item content: %w", err)
	}
	if len(i.Sources) == 0 {
		return errors.New("memory: context item provenance is required")
	}
	for index, source := range i.Sources {
		if err := source.Validate(); err != nil {
			return fmt.Errorf("memory: context item source %d: %w", index, err)
		}
	}
	if i.Hint != nil {
		if err := i.Hint.Validate(); err != nil {
			return fmt.Errorf("memory: context item expand hint: %w", err)
		}
	}
	return nil
}
