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
