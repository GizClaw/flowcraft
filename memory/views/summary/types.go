// Package summary stores flat immutable conversation summaries.
package summary

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

type Level uint8

const (
	L0 Level = iota
	L1
	L2
	L3
)

func (level Level) String() string { return fmt.Sprintf("L%d", level) }

func (level Level) Validate() error {
	if level > L3 {
		return fmt.Errorf("summary view: invalid level %d", level)
	}
	return nil
}

type CoverageRange struct {
	StartSeq  uint64    `json:"start_seq,omitempty"`
	EndSeq    uint64    `json:"end_seq,omitempty"`
	StartTime time.Time `json:"start_time,omitempty"`
	EndTime   time.Time `json:"end_time,omitempty"`
}

func (value CoverageRange) Validate() error {
	if value.StartSeq > value.EndSeq && value.EndSeq != 0 {
		return errors.New("summary view: coverage start_seq exceeds end_seq")
	}
	if !value.StartTime.IsZero() && !value.EndTime.IsZero() && value.StartTime.After(value.EndTime) {
		return errors.New("summary view: coverage start_time exceeds end_time")
	}
	if value.StartSeq == 0 && value.EndSeq == 0 && value.StartTime.IsZero() && value.EndTime.IsZero() {
		return errors.New("summary view: coverage range is required")
	}
	return nil
}

type Record struct {
	ID                 string                `json:"id"`
	Scope              sdkmemory.Scope       `json:"scope"`
	ConversationID     string                `json:"conversation_id"`
	Level              Level                 `json:"level"`
	Text               string                `json:"text"`
	Content            sdkmessage.Content    `json:"content"`
	Topics             []string              `json:"topics,omitempty"`
	InputIDs           []string              `json:"input_ids"`
	SourceRefs         []sdkmemory.SourceRef `json:"source_refs"`
	CoverageRange      CoverageRange         `json:"coverage_range"`
	SourceDigest       string                `json:"source_digest"`
	TransformSignature string                `json:"transform_signature"`
	GenerationID       string                `json:"generation_id"`
	CreatedAt          time.Time             `json:"created_at"`
}

// Manifest atomically names the immutable records that form the active
// summary hierarchy for one conversation.
type Manifest struct {
	Scope          sdkmemory.Scope `json:"scope"`
	ConversationID string          `json:"conversation_id"`
	GenerationID   string          `json:"generation_id"`
	RecordIDs      []string        `json:"record_ids"`
	CoverageRange  CoverageRange   `json:"coverage_range"`
	FrontierDigest string          `json:"frontier_digest"`
	PublishedAt    time.Time       `json:"published_at"`
}

type AddRequest struct {
	ID                 string
	Scope              sdkmemory.Scope
	ConversationID     string
	Level              Level
	Text               string
	Content            sdkmessage.Content
	Topics             []string
	InputIDs           []string
	SourceRefs         []sdkmemory.SourceRef
	CoverageRange      CoverageRange
	SourceDigest       string
	TransformSignature string
	GenerationID       string
}

type ListOptions struct {
	GenerationID string
	Level        *Level
}

func (record Record) Clone() Record {
	record.Content = record.Content.Clone()
	record.Topics = append([]string(nil), record.Topics...)
	record.InputIDs = append([]string(nil), record.InputIDs...)
	record.SourceRefs = append([]sdkmemory.SourceRef(nil), record.SourceRefs...)
	return record
}

func (manifest Manifest) Clone() Manifest {
	manifest.RecordIDs = append([]string(nil), manifest.RecordIDs...)
	return manifest
}

func (record Record) Validate() error {
	if strings.TrimSpace(record.ID) == "" {
		return errors.New("summary view: id is required")
	}
	if err := record.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(record.ConversationID) == "" {
		return errors.New("summary view: conversation_id is required")
	}
	if err := record.Level.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(record.Text) == "" || record.Content.Text() != record.Text {
		return errors.New("summary view: text and content must be non-empty and equal")
	}
	if err := record.Content.Validate(); err != nil {
		return fmt.Errorf("summary view: content: %w", err)
	}
	if len(record.InputIDs) == 0 || !orderedUniqueStrings(record.InputIDs) {
		return errors.New("summary view: input_ids must be ordered, unique, and non-empty")
	}
	if !canonicalStrings(record.Topics) {
		return errors.New("summary view: topics must be sorted and unique")
	}
	if len(record.SourceRefs) == 0 {
		return errors.New("summary view: source_refs are required")
	}
	for index, source := range record.SourceRefs {
		if err := source.Validate(); err != nil {
			return fmt.Errorf("summary view: source_refs[%d]: %w", index, err)
		}
	}
	normalizedSources := normalizeSourceRefs(record.SourceRefs)
	if len(normalizedSources) != len(record.SourceRefs) {
		return errors.New("summary view: source_refs must be sorted and unique")
	}
	for index := range normalizedSources {
		if normalizedSources[index] != record.SourceRefs[index] {
			return errors.New("summary view: source_refs must be sorted and unique")
		}
	}
	if err := record.CoverageRange.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(record.SourceDigest) == "" || strings.TrimSpace(record.TransformSignature) == "" ||
		strings.TrimSpace(record.GenerationID) == "" || record.CreatedAt.IsZero() {
		return errors.New("summary view: source_digest, transform_signature, generation_id, and created_at are required")
	}
	return nil
}

func (manifest Manifest) Validate() error {
	if err := manifest.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(manifest.ConversationID) == "" || strings.TrimSpace(manifest.GenerationID) == "" ||
		strings.TrimSpace(manifest.FrontierDigest) == "" || manifest.PublishedAt.IsZero() {
		return errors.New("summary view: manifest conversation_id, generation_id, frontier_digest, and published_at are required")
	}
	if !orderedUniqueStrings(manifest.RecordIDs) {
		return errors.New("summary view: manifest record_ids must be ordered and unique")
	}
	if len(manifest.RecordIDs) > 0 {
		if err := manifest.CoverageRange.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func StableID(scope sdkmemory.Scope, conversationID string, level Level, inputIDs []string, sourceDigest, transformSignature string) string {
	ids := normalizeOrderedStrings(inputIDs)
	payload, _ := json.Marshal([]any{scope, conversationID, level, ids, sourceDigest, transformSignature})
	sum := sha256.Sum256(append([]byte("flowcraft.memory.summary.id\x00v2\x00"), payload...))
	return hex.EncodeToString(sum[:])
}

func normalizeOrderedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func orderedUniqueStrings(values []string) bool {
	normalized := normalizeOrderedStrings(values)
	if len(normalized) != len(values) {
		return false
	}
	for index := range values {
		if normalized[index] != values[index] {
			return false
		}
	}
	return true
}

func normalizeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
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

func canonicalStrings(values []string) bool {
	normalized := normalizeStrings(values)
	if len(normalized) != len(values) {
		return false
	}
	for index := range values {
		if values[index] != normalized[index] {
			return false
		}
	}
	return true
}

func normalizeSourceRefs(values []sdkmemory.SourceRef) []sdkmemory.SourceRef {
	result := append([]sdkmemory.SourceRef(nil), values...)
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		if left.Revision != right.Revision {
			return left.Revision < right.Revision
		}
		return left.Locator < right.Locator
	})
	out := result[:0]
	for _, value := range result {
		if len(out) > 0 && out[len(out)-1] == value {
			continue
		}
		out = append(out, value)
	}
	return append([]sdkmemory.SourceRef(nil), out...)
}
