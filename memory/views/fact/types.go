package fact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"golang.org/x/text/unicode/norm"
)

const (
	// CanonicalAlgorithmVersion identifies the normalization and hash contract.
	CanonicalAlgorithmVersion = "fact-canonical-v1"
	canonicalHashDomain       = "flowcraft.memory.fact\x00v1\x00"
)

// Fact is one immutable, derived fact in a conversation.
type Fact struct {
	ID                 string                `json:"id"`
	CanonicalHash      string                `json:"canonical_hash"`
	Scope              sdkmemory.Scope       `json:"scope"`
	ConversationID     string                `json:"conversation_id"`
	Text               string                `json:"text"`
	Content            sdkmessage.Content    `json:"content"`
	Entities           []string              `json:"entities,omitempty"`
	Predicate          string                `json:"predicate,omitempty"`
	TemporalDetail     string                `json:"temporal_detail,omitempty"`
	EventTime          time.Time             `json:"event_time"`
	LinkedMemoryIDs    []string              `json:"linked_memory_ids,omitempty"`
	Provenance         []sdkmemory.SourceRef `json:"provenance"`
	SourceDigest       string                `json:"source_digest"`
	TransformSignature string                `json:"transform_signature"`
	Metadata           sdkmemory.Metadata    `json:"metadata,omitempty"`
	CreatedAt          time.Time             `json:"created_at"`
}

// AddRequest appends one immutable fact.
type AddRequest struct {
	ID                 string
	CanonicalHash      string
	Scope              sdkmemory.Scope
	ConversationID     string
	Content            sdkmessage.Content
	Entities           []string
	Predicate          string
	TemporalDetail     string
	EventTime          time.Time
	LinkedMemoryIDs    []string
	Provenance         []sdkmemory.SourceRef
	SourceDigest       string
	TransformSignature string
	Metadata           sdkmemory.Metadata
}

// ListOptions paginates by the stable (CreatedAt, ID) ordering. AfterCreatedAt
// and AfterID form an exclusive cursor. A non-positive Limit means no limit.
type ListOptions struct {
	AfterCreatedAt time.Time
	AfterID        string
	Limit          int
}

func cloneFact(value Fact) Fact {
	value.Content = value.Content.Clone()
	value.Entities = append([]string(nil), value.Entities...)
	value.LinkedMemoryIDs = append([]string(nil), value.LinkedMemoryIDs...)
	value.Provenance = append([]sdkmemory.SourceRef(nil), value.Provenance...)
	value.Metadata = value.Metadata.Clone()
	return value
}

// NormalizeText applies NFKC and deterministic Unicode whitespace folding
// while preserving case for display.
func NormalizeText(value string) string {
	return strings.Join(strings.Fields(norm.NFKC.String(value)), " ")
}

// CanonicalContent is the versioned identity input: NFKC, Unicode whitespace
// folding, and Unicode-aware lowercase.
func CanonicalContent(value string) string {
	return strings.ToLower(NormalizeText(value))
}

// CanonicalHash returns a domain-separated SHA-256 identity.
func CanonicalHash(value string) string {
	sum := sha256.Sum256([]byte(canonicalHashDomain + CanonicalContent(value)))
	return CanonicalAlgorithmVersion + ":" + hex.EncodeToString(sum[:])
}

// NormalizeEntities canonicalizes, de-duplicates, and sorts entity names.
func NormalizeEntities(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = CanonicalContent(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func validateFact(value Fact) error {
	if strings.TrimSpace(value.ID) == "" {
		return errors.New("fact view: fact_id is required")
	}
	if err := value.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(value.ConversationID) == "" {
		return errors.New("fact view: conversation_id is required")
	}
	if err := value.Content.Validate(); err != nil {
		return fmt.Errorf("fact view: content: %w", err)
	}
	if value.Text == "" || value.Text != NormalizeText(value.Text) {
		return errors.New("fact view: text must be normalized and non-empty")
	}
	if value.Content.Text() != value.Text {
		return errors.New("fact view: content and text differ")
	}
	if value.CanonicalHash != CanonicalHash(value.Text) {
		return errors.New("fact view: canonical_hash does not match text")
	}
	if !reflectStringsEqual(value.Entities, NormalizeEntities(value.Entities)) {
		return errors.New("fact view: entities are not canonical")
	}
	if value.EventTime.IsZero() || value.CreatedAt.IsZero() {
		return errors.New("fact view: event_time and created_at are required")
	}
	if strings.TrimSpace(value.SourceDigest) == "" || strings.TrimSpace(value.TransformSignature) == "" {
		return errors.New("fact view: source_digest and transform_signature are required")
	}
	for index, id := range value.LinkedMemoryIDs {
		if strings.TrimSpace(id) == "" || id == value.ID {
			return fmt.Errorf("fact view: invalid linked_memory_ids[%d]", index)
		}
		if index > 0 && value.LinkedMemoryIDs[index-1] >= id {
			return errors.New("fact view: linked_memory_ids must be sorted and unique")
		}
	}
	if len(value.Provenance) == 0 {
		return errors.New("fact view: provenance is required")
	}
	for index, source := range value.Provenance {
		if err := source.Validate(); err != nil {
			return fmt.Errorf("fact view: provenance %d: %w", index, err)
		}
	}
	return nil
}

func reflectStringsEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
