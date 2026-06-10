package entity

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	// DefaultTimelineID is the descriptor ID used by NewTimeline unless overridden.
	DefaultTimelineID views.ID = "entity-timeline"

	// DefaultTimelineVersion is the descriptor version used by NewTimeline unless overridden.
	DefaultTimelineVersion = "v1"
)

// TimelineOption configures a Timeline.
type TimelineOption interface {
	applyTimeline(*Timeline)
}

type timelineDescriptorOption struct {
	id      views.ID
	version string
}

// WithTimelineID overrides the descriptor ID for Timeline.
func WithTimelineID(id views.ID) TimelineOption {
	return timelineDescriptorOption{id: id}
}

// WithTimelineVersion overrides the descriptor version for Timeline.
func WithTimelineVersion(version string) TimelineOption {
	return timelineDescriptorOption{version: version}
}

func (o timelineDescriptorOption) applyTimeline(t *Timeline) {
	if o.id != "" {
		t.id = o.id
	}
	if o.version != "" {
		t.version = o.version
	}
}

// Timeline is a lightweight facade for the entity timeline view contract.
//
// It persists entity events derived from fact graph/ledger outputs. Temporal
// fields are stored as semantic data for later recipes; List stays ID-ordered.
type Timeline struct {
	store   TimelineStore
	id      views.ID
	version string
}

var _ views.View = (*Timeline)(nil)

// NewTimeline creates an entity timeline view backed by store.
func NewTimeline(store TimelineStore, opts ...TimelineOption) *Timeline {
	timeline := &Timeline{
		store:   store,
		id:      DefaultTimelineID,
		version: DefaultTimelineVersion,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyTimeline(timeline)
		}
	}
	return timeline
}

// Descriptor declares the Timeline view identity.
func (t *Timeline) Descriptor() views.Descriptor {
	return views.Descriptor{
		ID:      t.id,
		Kind:    views.KindEntityTimeline,
		Version: t.version,
	}
}

// Put stores or replaces an entity timeline event.
func (t *Timeline) Put(ctx context.Context, event Event) (Event, error) {
	if t.store == nil {
		return Event{}, errdefs.Validationf("%s: store is required", timelineErrPrefix)
	}
	event = cloneEvent(event)
	if err := validateEvent(event); err != nil {
		return Event{}, err
	}
	stored, err := t.store.Put(ctx, event)
	if err != nil {
		return Event{}, err
	}
	return cloneEvent(stored), nil
}

// Get returns one entity timeline event by id.
func (t *Timeline) Get(ctx context.Context, id EventID) (Event, bool, error) {
	if t.store == nil {
		return Event{}, false, errdefs.Validationf("%s: store is required", timelineErrPrefix)
	}
	if err := validateEventID(id); err != nil {
		return Event{}, false, err
	}
	event, ok, err := t.store.Get(ctx, id)
	if err != nil {
		return Event{}, false, err
	}
	if !ok {
		return Event{}, false, nil
	}
	return cloneEvent(event), true, nil
}

// List returns entity timeline events matching opts.
func (t *Timeline) List(ctx context.Context, opts TimelineListOptions) ([]Event, error) {
	if t.store == nil {
		return nil, errdefs.Validationf("%s: store is required", timelineErrPrefix)
	}
	events, err := t.store.List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return cloneEvents(events), nil
}

// Delete removes one entity timeline event by id. It is idempotent at the Store boundary.
func (t *Timeline) Delete(ctx context.Context, id EventID) error {
	if t.store == nil {
		return errdefs.Validationf("%s: store is required", timelineErrPrefix)
	}
	if err := validateEventID(id); err != nil {
		return err
	}
	return t.store.Delete(ctx, id)
}

// DeleteEntity removes all timeline events for one entity. It is idempotent at the Store boundary.
func (t *Timeline) DeleteEntity(ctx context.Context, entityID fact.NodeID) error {
	if t.store == nil {
		return errdefs.Validationf("%s: store is required", timelineErrPrefix)
	}
	if err := validateEntityID(timelineErrPrefix, entityID); err != nil {
		return err
	}
	return t.store.DeleteEntity(ctx, entityID)
}
