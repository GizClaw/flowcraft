package entityfact

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const errPrefix = "memory/views/entityfact"

type ListOptions struct {
	AfterID string
	Limit   int
}

type Store interface {
	PutEntity(context.Context, Entity) (Entity, error)
	PutFact(context.Context, Fact) (Fact, error)
	GetEntity(context.Context, views.Scope, EntityID) (Entity, bool, error)
	GetFact(context.Context, views.Scope, FactID) (Fact, bool, error)
	ListEntities(context.Context, views.Scope, ListOptions) ([]Entity, error)
	ListFacts(context.Context, views.Scope, ListOptions) ([]Fact, error)
	LookupAlias(context.Context, views.Scope, string) ([]EntityID, error)
	DeleteScope(context.Context, views.Scope) error
}

type Graph struct {
	store Store
	view  views.Descriptor
}

func NewGraph(store Store, opts ...Option) *Graph {
	g := &Graph{
		store: store,
		view: views.Descriptor{
			ID:      DefaultEntityFactsID,
			Kind:    views.KindEntityFacts,
			Version: DefaultEntityFactsVersion,
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(g)
		}
	}
	return g
}

type Option func(*Graph)

func WithID(id views.ID) Option {
	return func(g *Graph) {
		if id != "" {
			g.view.ID = id
		}
	}
}

func WithVersion(version string) Option {
	return func(g *Graph) {
		if version != "" {
			g.view.Version = version
		}
	}
}

func (g *Graph) Descriptor() views.Descriptor {
	if g == nil {
		return views.Descriptor{}
	}
	return g.view
}

func (g *Graph) PutEntity(ctx context.Context, entity Entity) (Entity, error) {
	if g == nil || g.store == nil {
		return Entity{}, errdefs.NotAvailablef("%s: store is not configured", errPrefix)
	}
	return g.store.PutEntity(ctx, entity)
}

func (g *Graph) PutFact(ctx context.Context, fact Fact) (Fact, error) {
	if g == nil || g.store == nil {
		return Fact{}, errdefs.NotAvailablef("%s: store is not configured", errPrefix)
	}
	return g.store.PutFact(ctx, fact)
}

func (g *Graph) GetEntity(ctx context.Context, scope views.Scope, id EntityID) (Entity, bool, error) {
	if g == nil || g.store == nil {
		return Entity{}, false, errdefs.NotAvailablef("%s: store is not configured", errPrefix)
	}
	return g.store.GetEntity(ctx, scope, id)
}

func (g *Graph) GetFact(ctx context.Context, scope views.Scope, id FactID) (Fact, bool, error) {
	if g == nil || g.store == nil {
		return Fact{}, false, errdefs.NotAvailablef("%s: store is not configured", errPrefix)
	}
	return g.store.GetFact(ctx, scope, id)
}

func (g *Graph) ListEntities(ctx context.Context, scope views.Scope, opts ListOptions) ([]Entity, error) {
	if g == nil || g.store == nil {
		return nil, errdefs.NotAvailablef("%s: store is not configured", errPrefix)
	}
	return g.store.ListEntities(ctx, scope, opts)
}

func (g *Graph) ListFacts(ctx context.Context, scope views.Scope, opts ListOptions) ([]Fact, error) {
	if g == nil || g.store == nil {
		return nil, errdefs.NotAvailablef("%s: store is not configured", errPrefix)
	}
	return g.store.ListFacts(ctx, scope, opts)
}

func (g *Graph) LookupAlias(ctx context.Context, scope views.Scope, alias string) ([]EntityID, error) {
	if g == nil || g.store == nil {
		return nil, errdefs.NotAvailablef("%s: store is not configured", errPrefix)
	}
	return g.store.LookupAlias(ctx, scope, alias)
}

func (g *Graph) DeleteScope(ctx context.Context, scope views.Scope) error {
	if g == nil || g.store == nil {
		return errdefs.NotAvailablef("%s: store is not configured", errPrefix)
	}
	return g.store.DeleteScope(ctx, scope)
}

func ValidateEntity(entity Entity) error {
	if entity.ID == "" {
		return errdefs.Validationf("%s: entity id is required", errPrefix)
	}
	if err := entity.Scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid entity scope: %w", errPrefix, err)
	}
	if entity.Scope.ConversationID == "" {
		return errdefs.Validationf("%s: entity conversation_id is required", errPrefix)
	}
	if entity.Name == "" {
		return errdefs.Validationf("%s: entity name is required", errPrefix)
	}
	if !validEntityType(entity.Type) {
		return errdefs.Validationf("%s: unsupported entity type %q", errPrefix, entity.Type)
	}
	for _, ref := range entity.MentionRefs {
		if err := ref.Validate(); err != nil {
			return errdefs.Validationf("%s: invalid entity mention ref: %w", errPrefix, err)
		}
	}
	return nil
}

func ValidateFact(fact Fact) error {
	if fact.ID == "" {
		return errdefs.Validationf("%s: fact id is required", errPrefix)
	}
	if err := fact.Scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid fact scope: %w", errPrefix, err)
	}
	if fact.Scope.ConversationID == "" {
		return errdefs.Validationf("%s: fact conversation_id is required", errPrefix)
	}
	if fact.SubjectEntityID == "" {
		return errdefs.Validationf("%s: fact subject entity id is required", errPrefix)
	}
	if !validRelationType(fact.RelationType) {
		return errdefs.Validationf("%s: unsupported relation type %q", errPrefix, fact.RelationType)
	}
	if fact.FactText == "" {
		return errdefs.Validationf("%s: fact text is required", errPrefix)
	}
	if len(fact.SourceRefs) == 0 {
		return errdefs.Validationf("%s: fact source refs are required", errPrefix)
	}
	for _, ref := range fact.SourceRefs {
		if err := ref.Validate(); err != nil {
			return errdefs.Validationf("%s: invalid fact source ref: %w", errPrefix, err)
		}
	}
	return nil
}

func validEntityType(entityType EntityType) bool {
	switch entityType {
	case EntityPerson, EntityPlace, EntityOrganization, EntityObject, EntityEvent, EntityConcept, EntityDate, EntityUnknown:
		return true
	default:
		return false
	}
}

func validRelationType(relationType RelationType) bool {
	switch relationType {
	case RelationAttribute, RelationPreference, RelationActivity, RelationRelationship, RelationLocation, RelationPossession, RelationPlan, RelationEvent, RelationTime, RelationOther:
		return true
	default:
		return false
	}
}
