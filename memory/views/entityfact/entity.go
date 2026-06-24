// Package entityfact stores entity-linked fact views for source-message recall.
package entityfact

import (
	"maps"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
)

const (
	DefaultEntityFactsID      views.ID = "entity_facts"
	DefaultEntityFactsVersion string   = "v1"

	FactObjectSpansMetadataKey = "object_spans"
	FactGraphableMetadataKey   = "graphable"
)

type EntityID string
type FactID string

type EntityType string

const (
	EntityPerson       EntityType = "person"
	EntityPlace        EntityType = "place"
	EntityOrganization EntityType = "organization"
	EntityObject       EntityType = "object"
	EntityEvent        EntityType = "event"
	EntityConcept      EntityType = "concept"
	EntityDate         EntityType = "date"
	EntityUnknown      EntityType = "unknown"
)

type RelationType string

const (
	RelationAttribute    RelationType = "attribute"
	RelationPreference   RelationType = "preference"
	RelationActivity     RelationType = "activity"
	RelationRelationship RelationType = "relationship"
	RelationLocation     RelationType = "location"
	RelationPossession   RelationType = "possession"
	RelationPlan         RelationType = "plan"
	RelationEvent        RelationType = "event"
	RelationTime         RelationType = "time"
	RelationOther        RelationType = "other"
)

// Entity is a conversation-local canonical entity used for recall indexing.
type Entity struct {
	ID          EntityID
	Scope       views.Scope
	Type        EntityType
	Name        string
	Aliases     []string
	Summary     string
	MentionRefs []views.SourceRef
	Confidence  float64
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Metadata    map[string]any
}

// Fact is a source-backed, entity-linked recall unit.
type Fact struct {
	ID              FactID
	Scope           views.Scope
	SubjectEntityID EntityID
	ObjectEntityIDs []EntityID
	RelationType    RelationType
	PredicateText   string
	FactText        string
	TimeText        string
	SourceRefs      []views.SourceRef
	Confidence      float64
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Metadata        map[string]any
}

type ObjectSpan struct {
	Text     string `json:"text"`
	SourceID string `json:"source_id"`
	Type     string `json:"type,omitempty"`
}

func (e Entity) AliasKeys() []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range append([]string{e.Name}, e.Aliases...) {
		key := NormalizeAlias(raw)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

func NormalizeAlias(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Join(strings.Fields(value), " ")
	return value
}

func NormalizeTimeKey(value string) string {
	return NormalizeAlias(value)
}

func IsGraphableFact(fact Fact) bool {
	if graphable, ok := fact.Metadata[FactGraphableMetadataKey].(bool); ok && !graphable {
		return false
	}
	if fact.SubjectEntityID == "" || fact.RelationType == "" || fact.RelationType == RelationOther {
		return false
	}
	if len(fact.ObjectEntityIDs) > 0 || len(ObjectSpansFromMetadata(fact.Metadata)) > 0 {
		return true
	}
	if strings.TrimSpace(fact.TimeText) == "" {
		return false
	}
	switch fact.RelationType {
	case RelationTime, RelationPlan, RelationEvent:
		return true
	default:
		return false
	}
}

func ObjectSpansFromMetadata(metadata map[string]any) []ObjectSpan {
	if len(metadata) == 0 {
		return nil
	}
	switch spans := metadata[FactObjectSpansMetadataKey].(type) {
	case []ObjectSpan:
		return normalizeObjectSpans(spans)
	case []map[string]string:
		out := make([]ObjectSpan, 0, len(spans))
		for _, span := range spans {
			out = append(out, ObjectSpan{
				Text:     span["text"],
				SourceID: span["source_id"],
				Type:     span["type"],
			})
		}
		return normalizeObjectSpans(out)
	case []map[string]any:
		out := make([]ObjectSpan, 0, len(spans))
		for _, span := range spans {
			out = append(out, objectSpanFromMap(span))
		}
		return normalizeObjectSpans(out)
	case []any:
		out := make([]ObjectSpan, 0, len(spans))
		for _, raw := range spans {
			switch span := raw.(type) {
			case ObjectSpan:
				out = append(out, span)
			case map[string]string:
				out = append(out, ObjectSpan{
					Text:     span["text"],
					SourceID: span["source_id"],
					Type:     span["type"],
				})
			case map[string]any:
				out = append(out, objectSpanFromMap(span))
			}
		}
		return normalizeObjectSpans(out)
	default:
		return nil
	}
}

func objectSpanFromMap(span map[string]any) ObjectSpan {
	return ObjectSpan{
		Text:     stringMetadataValue(span["text"]),
		SourceID: stringMetadataValue(span["source_id"]),
		Type:     stringMetadataValue(span["type"]),
	}
}

func stringMetadataValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func normalizeObjectSpans(in []ObjectSpan) []ObjectSpan {
	out := make([]ObjectSpan, 0, len(in))
	for _, span := range in {
		span.Text = strings.TrimSpace(span.Text)
		span.SourceID = strings.TrimSpace(span.SourceID)
		span.Type = strings.TrimSpace(span.Type)
		if span.Text == "" || span.SourceID == "" {
			continue
		}
		out = append(out, span)
	}
	return out
}

func CloneEntity(in Entity) Entity {
	out := in
	out.Aliases = append([]string(nil), in.Aliases...)
	out.MentionRefs = cloneSourceRefs(in.MentionRefs)
	out.Metadata = maps.Clone(in.Metadata)
	return out
}

func CloneFact(in Fact) Fact {
	out := in
	out.ObjectEntityIDs = append([]EntityID(nil), in.ObjectEntityIDs...)
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Metadata = maps.Clone(in.Metadata)
	return out
}

func cloneSourceRefs(in []views.SourceRef) []views.SourceRef {
	if in == nil {
		return nil
	}
	out := make([]views.SourceRef, len(in))
	for i, ref := range in {
		if ref.Message != nil {
			msg := *ref.Message
			if msg.Span != nil {
				span := *msg.Span
				msg.Span = &span
			}
			ref.Message = &msg
		}
		if ref.Document != nil {
			doc := *ref.Document
			if doc.Span != nil {
				span := *doc.Span
				doc.Span = &span
			}
			ref.Document = &doc
		}
		out[i] = ref
	}
	return out
}
